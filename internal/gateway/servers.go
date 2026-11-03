// Package gateway is SpaceMan's central MCP gateway (SM-MCP-001/002, ADR-0023). Administrators register external MCP
// servers here and grant them to spaces and space roles. A granted run sees the server's tools as "<slug>__<tool>" in
// its own SpaceMan MCP server; it never learns the server's address or credentials. Central opens one upstream session
// per run and server (never shared), checks the grant again on every call, bounds time, concurrency and size, and
// dials only addresses the egress policy allows.
package gateway

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/keyring"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Server is a registered upstream MCP server. Header values are write-only: only their names are ever returned.
type Server struct {
	ID             string    `json:"id"`
	Slug           string    `json:"slug" doc:"Namespace of the server's tools: <slug>__<tool>"`
	Name           string    `json:"name"`
	Description    string    `json:"description,omitempty"`
	URL            string    `json:"url" doc:"Streamable HTTP endpoint"`
	HeaderNames    []string  `json:"header_names" doc:"Names of the stored request headers; values are never returned"`
	HeadersVersion int64     `json:"headers_version"`
	AllowPrivate   bool      `json:"allow_private" doc:"Allow loopback and private addresses (and plain http). Off by default."`
	Tools          []string  `json:"tools" doc:"Upstream tools offered to agents; empty offers every tool"`
	CallTimeoutMS  int64     `json:"call_timeout_ms"`
	MaxConcurrency int64     `json:"max_concurrency" doc:"Calls in flight to this server across all runs"`
	MaxResultBytes int64     `json:"max_result_bytes"`
	Enabled        bool      `json:"enabled"`
	Grants         int64     `json:"grants"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	Version        int64     `json:"version"`
}

// Input creates or edits a server. Headers replace the stored set when SetHeaders is true (an empty map clears them).
type Input struct {
	Slug           string            `json:"slug,omitempty" maxLength:"31" doc:"Fixed after creation"`
	Name           string            `json:"name" maxLength:"120"`
	Description    string            `json:"description,omitempty" maxLength:"2000"`
	URL            string            `json:"url" maxLength:"2048"`
	SetHeaders     bool              `json:"set_headers,omitempty" doc:"Replace the stored headers with headers"`
	Headers        map[string]string `json:"headers,omitempty" writeOnly:"true" doc:"Write-only request headers such as Authorization"`
	AllowPrivate   bool              `json:"allow_private,omitempty"`
	Tools          []string          `json:"tools,omitempty" maxItems:"200"`
	CallTimeoutMS  int64             `json:"call_timeout_ms,omitempty" doc:"1000-20000; default 20000"`
	MaxConcurrency int64             `json:"max_concurrency,omitempty" doc:"1-32; default 4"`
	MaxResultBytes int64             `json:"max_result_bytes,omitempty" doc:"1 KiB-4 MiB; default 256 KiB"`
	Enabled        bool              `json:"enabled"`
}

// Grant lets a space's runs (of one space role, or every role) use a server's tools.
type Grant struct {
	ID        string    `json:"id"`
	ServerID  string    `json:"server_id"`
	SpaceID   string    `json:"space_id"`
	Role      string    `json:"role" doc:"A space role name, or * for every role"`
	Tools     []string  `json:"tools" doc:"Subset of the server's tools; empty grants all of them"`
	CreatedAt time.Time `json:"created_at"`
}

// Limits.
const (
	defaultTimeout     = 20 * time.Second
	maxTimeout         = 20 * time.Second // below the node protocol's tool-call deadline
	defaultConcurrency = 4
	defaultResultBytes = 256 << 10
	maxResultBytes     = 4 << 20
	maxHeaders         = 20
	// MaxToolName is the longest namespaced tool name offered: model APIs accept up to 64 characters.
	MaxToolName = 64
	// Separator joins a server's slug and an upstream tool name.
	Separator = "__"
)

var (
	reSlug     = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)
	reToolName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	// reUpstreamTool is any tool name the MCP specification allows (used in allowlists).
	reUpstreamTool = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)
	reRole         = regexp.MustCompile(`^(\*|[a-z][a-z0-9-]{0,47})$`)
	reHeader       = regexp.MustCompile("^[A-Za-z0-9!#$%&'*+.^_`|~-]{1,64}$")
	// Headers the gateway's transport owns.
	reservedHeaders = []string{"host", "content-length", "content-type", "accept", "connection", "transfer-encoding", "upgrade",
		"te", "trailer", "keep-alive", "proxy-authorization", "proxy-connection", "mcp-session-id", "mcp-protocol-version", "last-event-id"}

	errNotFound = apperr.New(apperr.CodeNotFound, "MCP server not found")
)

// Service manages servers and grants, and runs the gateway.
type Service struct {
	s       *store.Store
	kr      *keyring.Keyring
	audit   *audit.Writer
	changes *changefeed.Recorder
	rt      *runtime
	// transport reaches upstream servers (SM-ARCH-003).
	transport Transport
	// Gate enforces approval policies on gateway tools (ADR-0024); nil gates nothing.
	Gate interface {
		Gate(ctx context.Context, p *fleet.RunPrincipal, op, resource string, args json.RawMessage) (func(error), error)
	}
}

// Options tune the gateway runtime.
type Options struct {
	MaxSessions int           // open upstream sessions across all runs (default 256)
	IdleClose   time.Duration // an unused session is closed after this (default 10m)
	Log         *slog.Logger
	// Transport reaches upstream servers; nil is StreamableHTTP, the only transport compiled in.
	Transport Transport
}

// New returns the service.
func New(s *store.Store, kr *keyring.Keyring, aw *audit.Writer, ch *changefeed.Recorder, o Options) *Service {
	if o.Transport == nil {
		o.Transport = StreamableHTTP{}
	}
	svc := &Service{s: s, kr: kr, audit: aw, changes: ch, transport: o.Transport}
	svc.rt = newRuntime(svc, o)
	return svc
}

func aad(id string) []byte { return []byte("mcp_server:" + id) }

func b2i(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func listJSON(l []string) string {
	if l == nil {
		l = []string{}
	}
	b, _ := json.Marshal(l)
	return string(b)
}

func parseList(s string) []string {
	out := []string{}
	_ = json.Unmarshal([]byte(s), &out)
	return out
}

func (in *Input) normalize(create bool) error {
	v := &apperr.Validation{}
	in.Name = strings.TrimSpace(in.Name)
	if create && (!reSlug.MatchString(in.Slug) || in.Slug == "spaceman" || strings.Contains(in.Slug, Separator)) {
		v.Add("slug", "must be 1-31 lowercase letters, digits or dashes, start with a letter, and not be \"spaceman\"")
	}
	if in.Name == "" || len(in.Name) > 120 {
		v.Add("name", "is required (at most 120 characters)")
	}
	if len(in.Description) > 2000 {
		v.Add("description", "at most 2000 characters")
	}
	if err := checkURL(in.URL, in.AllowPrivate); err != nil {
		v.Add("url", "%s", err.Error())
	}
	if in.SetHeaders {
		if len(in.Headers) > maxHeaders {
			v.Add("headers", "at most %d headers", maxHeaders)
		}
		for k, val := range in.Headers {
			if !reHeader.MatchString(k) || slices.Contains(reservedHeaders, strings.ToLower(k)) {
				v.Add("headers", "%q is not a header SpaceMan can send", k)
			}
			if val == "" || len(val) > 8192 || strings.ContainsAny(val, "\r\n\x00") {
				v.Add("headers", "the value of %q must be one line of 1-8192 bytes", k)
			}
		}
	}
	seen := map[string]bool{}
	for _, t := range in.Tools {
		if !reUpstreamTool.MatchString(t) || seen[t] {
			v.Add("tools", "%q is not a tool name (or is listed twice)", t)
		}
		seen[t] = true
	}
	sort.Strings(in.Tools)
	if in.CallTimeoutMS == 0 {
		in.CallTimeoutMS = defaultTimeout.Milliseconds()
	}
	if in.CallTimeoutMS < 1000 || in.CallTimeoutMS > maxTimeout.Milliseconds() {
		v.Add("call_timeout_ms", "must be between 1000 and %d", maxTimeout.Milliseconds())
	}
	if in.MaxConcurrency == 0 {
		in.MaxConcurrency = defaultConcurrency
	}
	if in.MaxConcurrency < 1 || in.MaxConcurrency > 32 {
		v.Add("max_concurrency", "must be between 1 and 32")
	}
	if in.MaxResultBytes == 0 {
		in.MaxResultBytes = defaultResultBytes
	}
	if in.MaxResultBytes < 1024 || in.MaxResultBytes > maxResultBytes {
		v.Add("max_result_bytes", "must be between 1024 and %d", maxResultBytes)
	}
	return v.Err()
}

// checkURL accepts an absolute https URL without credentials; plain http only with private networks allowed.
func checkURL(raw string, allowPrivate bool) error {
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.Host == "" || u.Opaque != "" {
		return errors.New("must be an absolute http(s) URL")
	}
	if len(raw) > 2048 {
		return errors.New("at most 2048 characters")
	}
	if u.User != nil {
		return errors.New("must not contain credentials; use a header")
	}
	if u.Fragment != "" {
		return errors.New("must not have a fragment")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !allowPrivate {
			return errors.New("must use https (plain http needs private networks allowed)")
		}
	default:
		return errors.New("must use https")
	}
	return nil
}

func (svc *Service) seal(id string, headers map[string]string) ([]byte, string, error) {
	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, http.CanonicalHeaderKey(k))
	}
	sort.Strings(names)
	if len(headers) == 0 {
		return nil, listJSON(names), nil
	}
	canon := map[string]string{}
	for k, v := range headers {
		canon[http.CanonicalHeaderKey(k)] = v
	}
	b, _ := json.Marshal(canon)
	sealed, err := svc.kr.Seal(keyring.PurposeCredential, aad(id), b)
	return sealed, listJSON(names), err
}

func (svc *Service) headers(r db.McpServer) (map[string]string, error) {
	out := map[string]string{}
	if len(r.HeadersEnc) == 0 {
		return out, nil
	}
	b, err := svc.kr.Open(keyring.PurposeCredential, aad(r.ID), r.HeadersEnc)
	if err != nil {
		return nil, err
	}
	err = json.Unmarshal(b, &out)
	return out, err
}

func fromRow(r db.McpServer, grants int64) *Server {
	return &Server{ID: r.ID, Slug: r.Slug, Name: r.Name, Description: r.Description, URL: r.Url, HeaderNames: parseList(r.HeaderNamesJson),
		HeadersVersion: r.HeadersVersion, AllowPrivate: r.AllowPrivate == 1, Tools: parseList(r.ToolsJson), CallTimeoutMS: r.CallTimeoutMs,
		MaxConcurrency: r.MaxConcurrency, MaxResultBytes: r.MaxResultBytes, Enabled: r.Enabled == 1, Grants: grants,
		CreatedAt: time.UnixMicro(r.CreatedAt).UTC(), UpdatedAt: time.UnixMicro(r.UpdatedAt).UTC(), Version: r.Version}
}

// Create registers a server.
func (svc *Service) Create(ctx context.Context, actor audit.Actor, in Input) (*Server, error) {
	in.SetHeaders = true
	if err := in.normalize(true); err != nil {
		return nil, err
	}
	id := ids.New(ids.MCPServer)
	sealed, names, err := svc.seal(id, in.Headers)
	if err != nil {
		return nil, err
	}
	err = svc.s.W().TxNamed(ctx, "mcp.create", func(tx *store.Tx) error {
		if _, err := tx.Q.GetMCPServerBySlug(ctx, in.Slug); err == nil {
			return apperr.New(apperr.CodeAlreadyExists, "an MCP server with slug %q exists", in.Slug)
		}
		if err := tx.Q.InsertMCPServer(ctx, db.InsertMCPServerParams{ID: id, Slug: in.Slug, Name: in.Name, Description: in.Description, Url: in.URL,
			HeadersEnc: sealed, HeaderNamesJson: names, HeadersVersion: 1, AllowPrivate: b2i(in.AllowPrivate), ToolsJson: listJSON(in.Tools),
			CallTimeoutMs: in.CallTimeoutMS, MaxConcurrency: in.MaxConcurrency, MaxResultBytes: in.MaxResultBytes, Enabled: b2i(in.Enabled),
			CreatedAt: tx.NowMicros(), CreatedBy: actor.ID, UpdatedAt: tx.NowMicros()}); err != nil {
			return err
		}
		if err := svc.changes.Record(ctx, tx, "", "mcp_server", id, "created", 1); err != nil {
			return err
		}
		_, err := svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "mcp_server.created", Target: audit.Target{Type: "mcp_server", ID: id},
			Details: map[string]any{"slug": in.Slug, "url": in.URL, "header_names": parseList(names), "allow_private": in.AllowPrivate, "enabled": in.Enabled}})
		return err
	})
	if err != nil {
		return nil, err
	}
	return svc.Get(ctx, id)
}

// Update edits a server. Open sessions of runs pick up the change on their next call (they are reopened).
func (svc *Service) Update(ctx context.Context, actor audit.Actor, id string, version int64, in Input) (*Server, error) {
	if err := in.normalize(false); err != nil {
		return nil, err
	}
	var sealed []byte
	var names string
	if in.SetHeaders {
		var err error
		if sealed, names, err = svc.seal(id, in.Headers); err != nil {
			return nil, err
		}
	}
	err := svc.s.W().TxNamed(ctx, "mcp.update", func(tx *store.Tx) error {
		cur, err := tx.Q.GetMCPServer(ctx, id)
		if errors.Is(err, sql.ErrNoRows) {
			return errNotFound
		} else if err != nil {
			return err
		}
		if cur.Version != version {
			return apperr.New(apperr.CodePrecondition, "the MCP server changed since version %d (now %d)", version, cur.Version)
		}
		if _, err := tx.Q.UpdateMCPServer(ctx, db.UpdateMCPServerParams{Name: in.Name, Description: in.Description, Url: in.URL, AllowPrivate: b2i(in.AllowPrivate),
			ToolsJson: listJSON(in.Tools), CallTimeoutMs: in.CallTimeoutMS, MaxConcurrency: in.MaxConcurrency, MaxResultBytes: in.MaxResultBytes,
			Enabled: b2i(in.Enabled), Now: tx.NowMicros(), ID: id, Version: version}); err != nil {
			return err
		}
		next := version + 1
		if in.SetHeaders {
			if _, err := tx.Q.SetMCPServerHeaders(ctx, db.SetMCPServerHeadersParams{HeadersEnc: sealed, HeaderNamesJson: names, Now: tx.NowMicros(), ID: id, Version: next}); err != nil {
				return err
			}
			next++
		}
		if err := svc.changes.Record(ctx, tx, "", "mcp_server", id, "updated", next); err != nil {
			return err
		}
		d := map[string]any{"url": in.URL, "allow_private": in.AllowPrivate, "enabled": in.Enabled, "tools": in.Tools, "headers_replaced": in.SetHeaders}
		if in.SetHeaders {
			d["header_names"] = parseList(names)
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "mcp_server.updated", Target: audit.Target{Type: "mcp_server", ID: id}, Details: d})
		return err
	})
	if err != nil {
		return nil, err
	}
	svc.rt.closeServer(id)
	return svc.Get(ctx, id)
}

// Delete removes a server and its grants, closing its open sessions.
func (svc *Service) Delete(ctx context.Context, actor audit.Actor, id string) error {
	err := svc.s.W().TxNamed(ctx, "mcp.delete", func(tx *store.Tx) error {
		cur, err := tx.Q.GetMCPServer(ctx, id)
		if errors.Is(err, sql.ErrNoRows) {
			return errNotFound
		} else if err != nil {
			return err
		}
		if _, err := tx.Q.DeleteMCPServer(ctx, id); err != nil {
			return err
		}
		if err := svc.changes.Record(ctx, tx, "", "mcp_server", id, "deleted", cur.Version+1); err != nil {
			return err
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "mcp_server.deleted", Target: audit.Target{Type: "mcp_server", ID: id},
			Details: map[string]any{"slug": cur.Slug}})
		return err
	})
	if err == nil {
		svc.rt.closeServer(id)
	}
	return err
}

// Get returns a server.
func (svc *Service) Get(ctx context.Context, id string) (*Server, error) {
	r, err := svc.s.R().GetMCPServer(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound
	} else if err != nil {
		return nil, err
	}
	n, err := svc.s.R().CountMCPGrants(ctx, id)
	if err != nil {
		return nil, err
	}
	return fromRow(r, n), nil
}

// List returns every server.
func (svc *Service) List(ctx context.Context) ([]*Server, error) {
	rows, err := svc.s.R().ListMCPServers(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*Server, 0, len(rows))
	for _, r := range rows {
		n, err := svc.s.R().CountMCPGrants(ctx, r.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, fromRow(r, n))
	}
	return out, nil
}

func grantFromRow(g db.McpGrant) *Grant {
	return &Grant{ID: g.ID, ServerID: g.ServerID, SpaceID: g.SpaceID, Role: g.Role, Tools: parseList(g.ToolsJson), CreatedAt: time.UnixMicro(g.CreatedAt).UTC()}
}

// AddGrant lets a space's runs of role (or "*") use the server's tools (all of them, or the listed subset).
func (svc *Service) AddGrant(ctx context.Context, actor audit.Actor, serverID, spaceID, role string, tools []string) (*Grant, error) {
	v := &apperr.Validation{}
	if role == "" {
		role = "*"
	}
	if !reRole.MatchString(role) {
		v.Add("role", "must be a space role name or *")
	}
	for _, t := range tools {
		if !reUpstreamTool.MatchString(t) {
			v.Add("tools", "%q is not a tool name", t)
		}
	}
	if err := v.Err(); err != nil {
		return nil, err
	}
	sort.Strings(tools)
	id := ids.New(ids.MCPGrant)
	err := svc.s.W().TxNamed(ctx, "mcp.grant", func(tx *store.Tx) error {
		if _, err := tx.Q.GetMCPServer(ctx, serverID); errors.Is(err, sql.ErrNoRows) {
			return errNotFound
		} else if err != nil {
			return err
		}
		if _, err := tx.Q.GetSpace(ctx, spaceID); errors.Is(err, sql.ErrNoRows) {
			return apperr.New(apperr.CodeNotFound, "space not found")
		} else if err != nil {
			return err
		}
		if _, err := tx.Q.GetMCPGrantByKey(ctx, db.GetMCPGrantByKeyParams{ServerID: serverID, SpaceID: spaceID, Role: role}); err == nil {
			return apperr.New(apperr.CodeAlreadyExists, "the server is already granted to role %q in this space", role)
		}
		if err := tx.Q.InsertMCPGrant(ctx, db.InsertMCPGrantParams{ID: id, ServerID: serverID, SpaceID: spaceID, Role: role, ToolsJson: listJSON(tools),
			CreatedAt: tx.NowMicros(), CreatedBy: actor.ID}); err != nil {
			return err
		}
		if err := svc.changes.Record(ctx, tx, spaceID, "mcp_server", serverID, "updated", 0); err != nil {
			return err
		}
		_, err := svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "mcp_server.granted", Target: audit.Target{Type: "mcp_server", ID: serverID},
			Details: map[string]any{"space_id": spaceID, "role": role, "tools": tools}})
		return err
	})
	if err != nil {
		return nil, err
	}
	g, err := svc.s.R().GetMCPGrant(ctx, id)
	if err != nil {
		return nil, err
	}
	return grantFromRow(g), nil
}

// RevokeGrant removes a grant. Calls already in flight finish; the next call of an affected run is refused.
func (svc *Service) RevokeGrant(ctx context.Context, actor audit.Actor, serverID, grantID string) error {
	var spaceID string
	err := svc.s.W().TxNamed(ctx, "mcp.revoke", func(tx *store.Tx) error {
		g, err := tx.Q.GetMCPGrant(ctx, grantID)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && g.ServerID != serverID) {
			return apperr.New(apperr.CodeNotFound, "grant not found")
		} else if err != nil {
			return err
		}
		spaceID = g.SpaceID
		if _, err := tx.Q.DeleteMCPGrant(ctx, grantID); err != nil {
			return err
		}
		if err := svc.changes.Record(ctx, tx, g.SpaceID, "mcp_server", g.ServerID, "updated", 0); err != nil {
			return err
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "mcp_server.revoked", Target: audit.Target{Type: "mcp_server", ID: g.ServerID},
			Details: map[string]any{"space_id": g.SpaceID, "role": g.Role}})
		return err
	})
	if err == nil {
		svc.rt.closeGrant(serverID, spaceID)
	}
	return err
}

// Grants lists a server's grants.
func (svc *Service) Grants(ctx context.Context, serverID string) ([]*Grant, error) {
	rows, err := svc.s.R().ListMCPGrantsByServer(ctx, serverID)
	if err != nil {
		return nil, err
	}
	out := make([]*Grant, 0, len(rows))
	for _, r := range rows {
		out = append(out, grantFromRow(r))
	}
	return out, nil
}

// SpaceServer is a server granted in a space, as a space's members see it (no URL or headers).
type SpaceServer struct {
	ServerID string   `json:"server_id"`
	Slug     string   `json:"slug"`
	Name     string   `json:"name"`
	Enabled  bool     `json:"enabled"`
	GrantID  string   `json:"grant_id"`
	Role     string   `json:"role"`
	Tools    []string `json:"tools"`
}

// SpaceGrants lists the servers granted in a space.
func (svc *Service) SpaceGrants(ctx context.Context, spaceID string) ([]*SpaceServer, error) {
	rows, err := svc.s.R().ListMCPGrantsBySpace(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	out := make([]*SpaceServer, 0, len(rows))
	for _, g := range rows {
		s, err := svc.s.R().GetMCPServer(ctx, g.ServerID)
		if err != nil {
			continue
		}
		out = append(out, &SpaceServer{ServerID: s.ID, Slug: s.Slug, Name: s.Name, Enabled: s.Enabled == 1, GrantID: g.ID, Role: g.Role, Tools: parseList(g.ToolsJson)})
	}
	return out, nil
}

// granted is one usable server for a run, with the tools it may call (nil: every tool the server offers).
type granted struct {
	server  db.McpServer
	allowed []string // nil means all
}

// allows reports whether a tool is allowed by both the server's list and the grant's.
func (g granted) allows(tool string) bool {
	if l := parseList(g.server.ToolsJson); len(l) > 0 && !slices.Contains(l, tool) {
		return false
	}
	return g.allowed == nil || slices.Contains(g.allowed, tool)
}

// grantsFor returns the enabled servers granted to a run's space and role. A role-specific grant and a "*" grant for
// the same server combine: the run may use the union of their tools.
func grantsFor(ctx context.Context, q *db.Queries, spaceID, role string) ([]granted, error) {
	rows, err := q.ListMCPGrantsBySpace(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	by := map[string]*granted{}
	var order []string
	for _, g := range rows {
		if g.Role != "*" && g.Role != role {
			continue
		}
		s, err := q.GetMCPServer(ctx, g.ServerID)
		if err != nil || s.Enabled != 1 {
			continue
		}
		cur, ok := by[s.ID]
		if !ok {
			cur = &granted{server: s, allowed: []string{}}
			by[s.ID] = cur
			order = append(order, s.ID)
		}
		tools := parseList(g.ToolsJson)
		if len(tools) == 0 || cur.allowed == nil {
			cur.allowed = nil
		} else {
			cur.allowed = append(cur.allowed, tools...)
		}
	}
	out := make([]granted, 0, len(order))
	for _, id := range order {
		out = append(out, *by[id])
	}
	return out, nil
}

// ManifestEntry records a granted server in a run's manifest.
type ManifestEntry struct {
	Slug    string   `json:"slug"`
	Version int64    `json:"version"`
	Tools   []string `json:"tools,omitempty" doc:"Empty: every tool the server offers"`
}

// ForManifest lists what a launch of role in the space is granted (recorded in the run manifest; calls are checked again).
func (svc *Service) ForManifest(ctx context.Context, spaceID, role string) ([]ManifestEntry, error) {
	gs, err := grantsFor(ctx, svc.s.R(), spaceID, role)
	if err != nil {
		return nil, err
	}
	out := []ManifestEntry{}
	for _, g := range gs {
		var tools []string
		for _, t := range g.allowed {
			if g.allows(t) {
				tools = append(tools, t)
			}
		}
		if g.allowed == nil {
			tools = parseList(g.server.ToolsJson)
		}
		out = append(out, ManifestEntry{Slug: g.server.Slug, Version: g.server.Version, Tools: tools})
	}
	return out, nil
}
