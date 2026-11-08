package gateway

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Server kinds (mcp_servers.kind).
const (
	// KindHTTP is an external Streamable HTTP server (ADR-0023).
	KindHTTP = "http"
	// KindSpaceman is SpaceMan's own API as an MCP server, served in central through a secretless internal token
	// (ADR-0063).
	KindSpaceman = "spaceman"
)

// Approval modes of a SpaceMan MCP server. Dangerous tools always need a person's approval, whatever the mode.
const (
	// ApprovalNone gates only dangerous tools (and whatever approval policies name): the default (operator decision).
	ApprovalNone = "none"
	// ApprovalWrites also gates every write tool.
	ApprovalWrites = "writes"
)

// The tool classes the gateway acts on (internal/spacemanmcp's classes).
const (
	classWrite     = "write"
	classDangerous = "dangerous"
)

// spacemanKinds are the run kinds that see SpaceMan MCP tools (ADR-0063): never KB, dreamer or diagnostic runs.
var spacemanKinds = []string{"control", "worker"}

// SpacemanSettings is a SpaceMan MCP server's tool choice and approval mode.
type SpacemanSettings struct {
	Preset   string   `json:"preset,omitempty" enum:"read,operate,admin,custom" doc:"Tool set: read, operate (read and write), admin (read, write and dangerous; the default) or custom (only the tools named)"`
	Tools    []string `json:"tools,omitempty" maxItems:"400" doc:"Tools added by name, without the slug (dangerous and sensitive-read tools are offered only by name or by the admin preset)"`
	Approval string   `json:"approval,omitempty" enum:"none,writes" doc:"none (the default): only dangerous tools need a person's approval; writes: every write tool does too"`
}

// SpacemanGrant is one role@domain grant of a SpaceMan MCP server's internal token.
type SpacemanGrant = auth.GrantInput

// SpacemanInfo describes a SpaceMan MCP server's settings and internal token. The token has no secret: nothing here
// is sensitive.
type SpacemanInfo struct {
	SpacemanSettings
	TokenID      string          `json:"token_id" doc:"The internal API token the server acts through"`
	OwnerUserID  string          `json:"owner_user_id" doc:"The administrator whose authority the token carries"`
	Grants       []SpacemanGrant `json:"grants"`
	ExpiresAt    *time.Time      `json:"expires_at,omitempty"`
	NeverExpires bool            `json:"never_expires"`
	TokenState   string          `json:"token_state" enum:"active,revoked,expired,owner_disabled" doc:"active, or why the server's tools are refused: re-issue it"`
}

// SpacemanInput creates a SpaceMan MCP server together with its internal token.
type SpacemanInput struct {
	Slug        string `json:"slug" maxLength:"31" doc:"Namespace of the server's tools (<slug>__<tool>); fixed after creation"`
	Name        string `json:"name" maxLength:"120"`
	Description string `json:"description,omitempty" maxLength:"2000"`
	SpacemanSettings
	SpacemanToken
	CallTimeoutMS  int64 `json:"call_timeout_ms,omitempty" doc:"1000-20000; default 20000"`
	MaxConcurrency int64 `json:"max_concurrency,omitempty" doc:"1-32; default 4"`
	MaxResultBytes int64 `json:"max_result_bytes,omitempty" doc:"1 KiB-4 MiB; default 256 KiB"`
	Enabled        *bool `json:"enabled,omitempty" doc:"Default true"`
}

// SpacemanToken is the internal token a SpaceMan MCP server is created or re-issued with.
type SpacemanToken struct {
	Grants        []SpacemanGrant `json:"grants,omitempty" maxItems:"50" doc:"The token's grants, covered by yours; default admin on every space (preset admin), required otherwise"`
	ExpiresInDays int64           `json:"expires_in_days,omitempty" doc:"1-366; omitted: the token never expires (the default)"`
}

// SpacemanUpdate edits a SpaceMan MCP server. Its token (grants, expiry, owner) changes only by re-issuing it.
type SpacemanUpdate struct {
	Name        string `json:"name" maxLength:"120"`
	Description string `json:"description,omitempty" maxLength:"2000"`
	SpacemanSettings
	CallTimeoutMS  int64 `json:"call_timeout_ms,omitempty"`
	MaxConcurrency int64 `json:"max_concurrency,omitempty"`
	MaxResultBytes int64 `json:"max_result_bytes,omitempty"`
	Enabled        bool  `json:"enabled"`
}

// SpacemanTarget is a SpaceMan MCP server as its backend sees it.
type SpacemanTarget struct {
	ID, Slug, Name string
	TokenID        string
	Settings       SpacemanSettings
	MaxResultBytes int64
}

// SpacemanBackend serves SpaceMan MCP servers in process (internal/spacemanmcp). The gateway keeps everything else:
// grants, run kinds, limits, approvals and audit.
type SpacemanBackend interface {
	// Validate checks a server's settings (preset and tool names).
	Validate(s SpacemanSettings) error
	// Catalog lists the tools a server offers runs, named <slug>__<tool>. It is empty when the server's token can no
	// longer be used (revoked, expired, owner disabled).
	Catalog(ctx context.Context, t SpacemanTarget) []proto.AgentTool
	// Class returns the class of a call of tool with args (a diagnostic launch is dangerous); false for a tool the
	// catalog does not offer.
	Class(t SpacemanTarget, tool string, args json.RawMessage) (string, bool)
	// Call runs one tool call with the server's internal token for the run. A refused token is an error with a reason.
	Call(ctx context.Context, t SpacemanTarget, runID, tool string, args json.RawMessage) (json.RawMessage, error)
}

// TokenIssuer mints and revokes internal tokens inside the gateway's transactions (auth.Service).
type TokenIssuer interface {
	CreateInternalTokenTx(ctx context.Context, tx *store.Tx, p *auth.Principal, in auth.InternalTokenInput) (string, error)
	RevokeAPITokenTx(ctx context.Context, tx *store.Tx, actor audit.Actor, id string) error
}

var errSpacemanUnavailable = apperr.New(apperr.CodeUnavailable, "SpaceMan MCP servers are not available in this installation")

// errSelfManagement refuses an agent acting through a SpaceMan MCP server on SpaceMan MCP servers (ADR-0063).
var errSelfManagement = apperr.New(apperr.CodeForbidden, "SpaceMan MCP servers are managed by people, not through the SpaceMan MCP")

func spacemanSettings(r db.McpServer) SpacemanSettings {
	var s SpacemanSettings
	_ = json.Unmarshal([]byte(r.SpacemanJson), &s)
	return s
}

func target(r db.McpServer) SpacemanTarget {
	return SpacemanTarget{ID: r.ID, Slug: r.Slug, Name: r.Name, TokenID: r.ApiTokenID.String, Settings: spacemanSettings(r), MaxResultBytes: r.MaxResultBytes}
}

// normalizeSettings applies the defaults (preset admin, approval none) and checks the settings.
func (svc *Service) normalizeSettings(s *SpacemanSettings, v *apperr.Validation) {
	if s.Preset == "" {
		s.Preset = "admin"
	}
	if s.Approval == "" {
		s.Approval = ApprovalNone
	}
	if s.Approval != ApprovalNone && s.Approval != ApprovalWrites {
		v.Add("approval", "must be none or writes")
	}
	if s.Tools == nil {
		s.Tools = []string{}
	}
	sort.Strings(s.Tools)
	s.Tools = slices.Compact(s.Tools)
	if svc.Spaceman != nil {
		if err := svc.Spaceman.Validate(*s); err != nil {
			v.Add("tools", "%s", err.Error())
		}
	}
}

// tokenInput turns a SpacemanToken into the internal token to mint: the preset's default grants (admin everywhere for
// admin) when none are given, and no expiry unless days are.
func tokenInput(name, preset string, t SpacemanToken, v *apperr.Validation) auth.InternalTokenInput {
	in := auth.InternalTokenInput{Purpose: auth.TokenPurposeSpacemanMCP, Name: name, Grants: t.Grants}
	if len(in.Grants) == 0 {
		if preset != "admin" {
			v.Add("grants", "are required for the %s preset (for example viewer or operator on one space)", preset)
		}
		in.Grants = []auth.GrantInput{{Role: authz.RoleAdmin, Domain: authz.Global}}
	}
	switch {
	case t.ExpiresInDays == 0:
		in.NeverExpires = true
	case t.ExpiresInDays < 1 || t.ExpiresInDays > 366:
		v.Add("expires_in_days", "must be between 1 and 366 (omit it for a token that never expires)")
	default:
		in.TTL = time.Duration(t.ExpiresInDays) * 24 * time.Hour
	}
	return in
}

// CreateSpaceman creates a SpaceMan MCP server and mints its internal token in the same transaction: the token is
// owned by the caller, with grants covered by the caller's (and by the caller's token's). Grant the server to a space
// role (AddGrant) for its runs to see the tools.
func (svc *Service) CreateSpaceman(ctx context.Context, p *auth.Principal, actor audit.Actor, in SpacemanInput) (*Server, error) {
	if svc.Spaceman == nil || svc.Tokens == nil {
		return nil, errSpacemanUnavailable
	}
	if actor.Internal() || p.Internal() {
		return nil, errSelfManagement
	}
	base := Input{Slug: in.Slug, Name: in.Name, Description: in.Description, CallTimeoutMS: in.CallTimeoutMS, MaxConcurrency: in.MaxConcurrency,
		MaxResultBytes: in.MaxResultBytes, Enabled: in.Enabled == nil || *in.Enabled}
	v := base.validate(true, KindSpaceman)
	svc.normalizeSettings(&in.SpacemanSettings, v)
	tin := tokenInput(base.Name, in.Preset, in.SpacemanToken, v)
	if err := v.Err(); err != nil {
		return nil, err
	}
	settings, _ := json.Marshal(in.SpacemanSettings)
	id := ids.New(ids.MCPServer)
	err := svc.s.W().TxNamed(ctx, "mcp.create_spaceman", func(tx *store.Tx) error {
		if _, err := tx.Q.GetMCPServerBySlug(ctx, base.Slug); err == nil {
			return apperr.New(apperr.CodeAlreadyExists, "an MCP server with slug %q exists", base.Slug)
		}
		tokenID, err := svc.Tokens.CreateInternalTokenTx(ctx, tx, p, tin)
		if err != nil {
			return err
		}
		if err := tx.Q.InsertMCPServer(ctx, db.InsertMCPServerParams{ID: id, Slug: base.Slug, Name: base.Name, Description: base.Description, Url: "",
			HeaderNamesJson: "[]", HeadersVersion: 1, ToolsJson: "[]", CallTimeoutMs: base.CallTimeoutMS, MaxConcurrency: base.MaxConcurrency,
			MaxResultBytes: base.MaxResultBytes, Enabled: b2i(base.Enabled), CreatedAt: tx.NowMicros(), CreatedBy: actor.ID, UpdatedAt: tx.NowMicros(),
			Kind: KindSpaceman, ApiTokenID: sql.NullString{String: tokenID, Valid: true}, SpacemanJson: string(settings)}); err != nil {
			return err
		}
		if err := svc.changes.Record(ctx, tx, "", "mcp_server", id, "created", 1); err != nil {
			return err
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "mcp_server.created", Target: audit.Target{Type: "mcp_server", ID: id},
			Details: map[string]any{"slug": base.Slug, "kind": KindSpaceman, "token_id": tokenID, "preset": in.Preset, "tools": in.Tools,
				"approval": in.Approval, "enabled": base.Enabled}})
		return err
	})
	if err != nil {
		return nil, err
	}
	return svc.Get(ctx, id)
}

// UpdateSpaceman edits a SpaceMan MCP server's settings and limits. Its token is unchanged: re-issue it to change the
// grants, the expiry or the owner.
func (svc *Service) UpdateSpaceman(ctx context.Context, actor audit.Actor, id string, version int64, in SpacemanUpdate) (*Server, error) {
	if svc.Spaceman == nil {
		return nil, errSpacemanUnavailable
	}
	if actor.Internal() {
		return nil, errSelfManagement
	}
	base := Input{Name: in.Name, Description: in.Description, CallTimeoutMS: in.CallTimeoutMS, MaxConcurrency: in.MaxConcurrency,
		MaxResultBytes: in.MaxResultBytes, Enabled: in.Enabled}
	v := base.validate(false, KindSpaceman)
	svc.normalizeSettings(&in.SpacemanSettings, v)
	if err := v.Err(); err != nil {
		return nil, err
	}
	settings, _ := json.Marshal(in.SpacemanSettings)
	err := svc.s.W().TxNamed(ctx, "mcp.update_spaceman", func(tx *store.Tx) error {
		cur, err := svc.spacemanRow(ctx, tx.Q, id, version)
		if err != nil {
			return err
		}
		if _, err := tx.Q.UpdateMCPServer(ctx, db.UpdateMCPServerParams{Name: base.Name, Description: base.Description, Url: "", ToolsJson: "[]",
			CallTimeoutMs: base.CallTimeoutMS, MaxConcurrency: base.MaxConcurrency, MaxResultBytes: base.MaxResultBytes, Enabled: b2i(base.Enabled),
			Now: tx.NowMicros(), ID: id, Version: version}); err != nil {
			return err
		}
		if _, err := tx.Q.SetMCPServerSpaceman(ctx, db.SetMCPServerSpacemanParams{SpacemanJson: string(settings), ApiTokenID: cur.ApiTokenID,
			Now: tx.NowMicros(), ID: id, Version: version + 1}); err != nil {
			return err
		}
		if err := svc.changes.Record(ctx, tx, "", "mcp_server", id, "updated", version+2); err != nil {
			return err
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "mcp_server.updated", Target: audit.Target{Type: "mcp_server", ID: id},
			Details: map[string]any{"kind": KindSpaceman, "preset": in.Preset, "tools": in.Tools, "approval": in.Approval, "enabled": base.Enabled}})
		return err
	})
	if err != nil {
		return nil, err
	}
	return svc.Get(ctx, id)
}

// ReissueSpaceman revokes a SpaceMan MCP server's internal token and mints a new one, owned by the caller, in one
// transaction: for new grants, a new expiry or a new owner (the old owner left or lost grants).
func (svc *Service) ReissueSpaceman(ctx context.Context, p *auth.Principal, actor audit.Actor, id string, version int64, in SpacemanToken) (*Server, error) {
	if svc.Spaceman == nil || svc.Tokens == nil {
		return nil, errSpacemanUnavailable
	}
	if actor.Internal() || p.Internal() {
		return nil, errSelfManagement
	}
	err := svc.s.W().TxNamed(ctx, "mcp.reissue_spaceman", func(tx *store.Tx) error {
		cur, err := svc.spacemanRow(ctx, tx.Q, id, version)
		if err != nil {
			return err
		}
		v := &apperr.Validation{}
		tin := tokenInput(cur.Name, spacemanSettings(cur).Preset, in, v)
		if err := v.Err(); err != nil {
			return err
		}
		old := cur.ApiTokenID.String
		if err := svc.Tokens.RevokeAPITokenTx(ctx, tx, actor, old); err != nil && !errors.Is(err, apperr.ErrNotFound) {
			return err
		}
		tokenID, err := svc.Tokens.CreateInternalTokenTx(ctx, tx, p, tin)
		if err != nil {
			return err
		}
		if _, err := tx.Q.SetMCPServerSpaceman(ctx, db.SetMCPServerSpacemanParams{SpacemanJson: cur.SpacemanJson,
			ApiTokenID: sql.NullString{String: tokenID, Valid: true}, Now: tx.NowMicros(), ID: id, Version: version}); err != nil {
			return err
		}
		if err := svc.changes.Record(ctx, tx, "", "mcp_server", id, "updated", version+1); err != nil {
			return err
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "mcp_server.reissued", Target: audit.Target{Type: "mcp_server", ID: id},
			Details: map[string]any{"revoked_token_id": old, "token_id": tokenID}})
		return err
	})
	if err != nil {
		return nil, err
	}
	return svc.Get(ctx, id)
}

// spacemanRow loads a SpaceMan MCP server for a change at version.
func (svc *Service) spacemanRow(ctx context.Context, q *db.Queries, id string, version int64) (db.McpServer, error) {
	cur, err := q.GetMCPServer(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return cur, errNotFound
	} else if err != nil {
		return cur, err
	}
	if cur.Kind != KindSpaceman {
		return cur, apperr.New(apperr.CodeInvalid, "%s is not a SpaceMan MCP server; edit it with update-mcp-server", cur.Slug)
	}
	if cur.Version != version {
		return cur, apperr.New(apperr.CodePrecondition, "the MCP server changed since version %d (now %d)", version, cur.Version)
	}
	return cur, nil
}

// spacemanInfo describes a SpaceMan MCP server's settings and token.
func (svc *Service) spacemanInfo(ctx context.Context, r db.McpServer) *SpacemanInfo {
	info := &SpacemanInfo{SpacemanSettings: spacemanSettings(r), TokenID: r.ApiTokenID.String, Grants: []SpacemanGrant{}, TokenState: "revoked"}
	if info.Tools == nil {
		info.Tools = []string{}
	}
	t, err := svc.s.R().GetAPIToken(ctx, r.ApiTokenID.String)
	if err != nil {
		return info
	}
	info.OwnerUserID, info.NeverExpires = t.UserID, t.NeverExpires == 1
	if !info.NeverExpires {
		e := time.UnixMicro(t.ExpiresAt).UTC()
		info.ExpiresAt = &e
	}
	if rows, err := svc.s.R().ListRoleGrantsBySubject(ctx, auth.TokenSubject(t.ID)); err == nil {
		for _, g := range rows {
			info.Grants = append(info.Grants, SpacemanGrant{Role: authz.Role(g.Role), Domain: g.Domain})
		}
	}
	u, uerr := svc.s.R().GetUser(ctx, t.UserID)
	switch {
	case t.RevokedAt.Valid:
	case !svc.s.Now().Before(time.UnixMicro(t.ExpiresAt)):
		info.TokenState = "expired"
	case uerr != nil || u.DisabledAt.Valid:
		info.TokenState = "owner_disabled"
	default:
		info.TokenState = "active"
	}
	return info
}

// refuseInternalOnSpaceman refuses an agent acting through a SpaceMan MCP server on a SpaceMan MCP server: it never
// grants, revokes, checks or deletes one (ADR-0063, self-protection).
func refuseInternalOnSpaceman(actor audit.Actor, r db.McpServer) error {
	if actor.Internal() && r.Kind == KindSpaceman {
		return errSelfManagement
	}
	return nil
}

// spacemanKindAllowed reports whether runs of kind may see SpaceMan MCP servers.
func spacemanKindAllowed(kind string) bool { return slices.Contains(spacemanKinds, kind) }

// spacemanGate returns the approval operation of a SpaceMan MCP call, mcp.<slug>.<class>.<tool>, and whether it always
// needs a person's approval: dangerous tools always, write tools when the server gates writes.
func spacemanGate(t SpacemanTarget, class, tool string) (string, bool) {
	always := class == classDangerous || (t.Settings.Approval == ApprovalWrites && class == classWrite)
	return "mcp." + t.Slug + "." + class + "." + tool, always
}

// spacemanCatalog lists a granted SpaceMan MCP server's tools for a run.
func (svc *Service) spacemanCatalog(ctx context.Context, g granted) []proto.AgentTool {
	if svc.Spaceman == nil {
		return nil
	}
	var out []proto.AgentTool
	for _, t := range svc.Spaceman.Catalog(ctx, target(g.server)) {
		if name, ok := strings.CutPrefix(t.Name, g.server.Slug+Separator); ok && g.allows(name) {
			t.Upstream = true
			out = append(out, t)
		}
	}
	return out
}

type approvedCallKey struct{}

// WithApprovedCall marks ctx as carrying one SpaceMan MCP call of the API operation operationID that a person approved
// as dangerous (approvals.Require succeeded for this exact call). Only the gateway sets it, after the approval and only
// for that call's context. The API reads it where an effect is resolved on the server: a diagnostic launch by an
// internal principal is refused without it (ADR-0063).
func WithApprovedCall(ctx context.Context, operationID string) context.Context {
	return context.WithValue(ctx, approvedCallKey{}, operationID)
}

// ApprovedCall returns the operation ID of the approved dangerous call ctx carries, or "".
func ApprovedCall(ctx context.Context) string {
	op, _ := ctx.Value(approvedCallKey{}).(string)
	return op
}

// errNeedsApprovalGate refuses a call that needs an approval when no approval gate is configured: fail closed.
func errNeedsApprovalGate(op string) error {
	return apperr.New(apperr.CodePrecondition, "%s needs a person's approval, and approvals are not available", op)
}
