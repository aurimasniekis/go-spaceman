package auth

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Internal tokens (ADR-0063). A SpaceMan MCP server acts through an API token row with a purpose: it is owned by the
// admin who created the server, carries its own role@domain grants (covered by the owner's, and by the issuing token's
// when a token issued it), and is revoked, expires and dies with its owner exactly like any other token. It has no
// secret: its MAC is over a random value that is discarded, and AuthenticateAPIToken refuses every row with a purpose,
// so it can never be presented as a bearer. Central builds its principal from the token ID per call (InternalPrincipal).

// TokenPurposeSpacemanMCP is the purpose of a SpaceMan MCP server's internal token.
const TokenPurposeSpacemanMCP = "spaceman_mcp"

// ViaSpacemanMCP prefixes the audit "via" of calls made through a SpaceMan MCP server's internal token.
const ViaSpacemanMCP = "spaceman_mcp:"

// InternalTokenInput describes an internal token to mint.
type InternalTokenInput struct {
	Purpose      string
	Name         string
	Grants       []GrantInput
	TTL          time.Duration
	NeverExpires bool
}

func validPurpose(p string) bool { return p == TokenPurposeSpacemanMCP }

// CreateInternalToken mints an internal token for the calling user in its own transaction and returns its metadata.
func (svc *Service) CreateInternalToken(ctx context.Context, p *Principal, in InternalTokenInput) (*APIToken, error) {
	var id string
	err := svc.s.W().TxNamed(ctx, "auth.create_internal_token", func(tx *store.Tx) error {
		var err error
		id, err = svc.CreateInternalTokenTx(ctx, tx, p, in)
		return err
	})
	if err != nil {
		return nil, err
	}
	return svc.GetAPIToken(ctx, id)
}

// CreateInternalTokenTx mints an internal token inside the caller's transaction (a SpaceMan MCP server is created
// together with its token) and returns its ID. The same rules as CreateAPIToken apply: only a user may mint, every
// grant must be covered by the user's current grants, and a token issued by another token cannot mint. Additionally,
// when a token mints, the grants must be covered by that token's own grants, and an internal principal can never mint.
func (svc *Service) CreateInternalTokenTx(ctx context.Context, tx *store.Tx, p *Principal, in InternalTokenInput) (string, error) {
	if !validPurpose(in.Purpose) {
		return "", apperr.New(apperr.CodeInvalid, "unknown internal token purpose")
	}
	if p == nil || p.User == nil {
		return "", apperr.New(apperr.CodeForbidden, "only users can issue API tokens; create a user first")
	}
	if p.Internal() {
		return "", apperr.New(apperr.CodeForbidden, "an internal token cannot issue tokens")
	}
	if p.TokenParentID != "" {
		return "", apperr.New(apperr.CodeForbidden, "a token issued by another token cannot issue tokens")
	}
	name := strings.TrimSpace(in.Name)
	v := &apperr.Validation{}
	if name == "" || utf8.RuneCountInString(name) > 100 {
		v.Add("name", "must be 1-100 characters")
	}
	grants, err := validGrants(in.Grants)
	if err != nil {
		return "", err
	}
	if len(grants) == 0 {
		v.Add("grants", "at least one grant is required")
	}
	// The owner's grants as they are now, not as they were when the request was authenticated.
	userRows, err := tx.Q.ListRoleGrantsBySubject(ctx, UserSubject(p.User.ID))
	if err != nil {
		return "", err
	}
	owner := grantsFromRows(userRows)
	var issuer []authz.Grant
	if p.TokenID != "" {
		rows, err := tx.Q.ListRoleGrantsBySubject(ctx, TokenSubject(p.TokenID))
		if err != nil {
			return "", err
		}
		issuer = grantsFromRows(rows)
	}
	for i, g := range grants {
		want := authz.Grant{Role: g.Role, Domain: g.Domain}
		if !authz.Covers(owner, want) {
			v.Add("grants", "entry %d (%s in %s) exceeds your own grants", i, g.Role, g.Domain)
		} else if p.TokenID != "" && !authz.Covers(issuer, want) {
			v.Add("grants", "entry %d (%s in %s) exceeds the grants of the token you are using", i, g.Role, g.Domain)
		}
	}
	switch {
	case in.NeverExpires && in.TTL != 0:
		v.Add("expires_in_days", "must be omitted for a token that never expires")
	case !in.NeverExpires && (in.TTL < MinAPITokenTTL || in.TTL > MaxAPITokenTTL):
		v.Add("expires_in_days", "must be between 1 and 366 days")
	}
	if err := v.Err(); err != nil {
		return "", err
	}
	// The MAC is over a random value nobody ever sees: no bearer can match it.
	discarded, err := randomToken()
	if err != nil {
		return "", err
	}
	mac, err := svc.tokenMAC("internal|" + discarded)
	if err != nil {
		return "", err
	}
	id := ids.New(ids.APIToken)
	expires := tx.Now.Add(in.TTL)
	details := map[string]any{"name": name, "grants": grants, "purpose": in.Purpose}
	if in.NeverExpires {
		expires, details["never_expires"] = neverExpiresAt, true
	} else {
		details["expires_in"] = in.TTL.String()
	}
	actor := audit.Actor{Type: audit.ActorUser, ID: p.User.ID}
	if p.TokenID != "" {
		actor.Via, details["parent_token_id"] = "api_token:"+p.TokenID, p.TokenID
	}
	if err := tx.Q.InsertAPIToken(ctx, db.InsertAPITokenParams{ID: id, Name: name, UserID: p.User.ID, Mac: mac, Hint: "",
		CreatedAt: tx.NowMicros(), ExpiresAt: expires.UnixMicro(), NeverExpires: b2i(in.NeverExpires),
		ParentTokenID: sql.NullString{String: p.TokenID, Valid: p.TokenID != ""}, Purpose: in.Purpose}); err != nil {
		return "", err
	}
	if err := svc.insertGrants(ctx, tx, TokenSubject(id), grants, "token", actor); err != nil {
		return "", err
	}
	if _, err := svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "api_token.created",
		Target: audit.Target{Type: "api_token", ID: id}, Details: details}); err != nil {
		return "", err
	}
	return id, nil
}

// InternalPrincipal builds the principal of a call made through an internal token for runID. It fails like a bad
// bearer token (ErrUnauthorized) when the token is unknown, is not internal, is revoked or expired, or its owner is
// disabled. Authorization is then the token's grants and the owner's, as for any token.
func (svc *Service) InternalPrincipal(ctx context.Context, tokenID, runID string) (*Principal, error) {
	gen := svc.rev.gen.Load()
	row, err := svc.s.R().GetAPIToken(ctx, tokenID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrUnauthorized
	}
	if err != nil {
		return nil, err
	}
	now := svc.opts.Clock.Now()
	if !validPurpose(row.Purpose) || row.RevokedAt.Valid || !now.Before(time.UnixMicro(row.ExpiresAt)) {
		return nil, ErrUnauthorized
	}
	op, err := svc.Get(ctx, row.UserID)
	if err != nil || !op.Enabled {
		return nil, ErrUnauthorized
	}
	return &Principal{User: op, SessionID: row.ID, TokenID: row.ID, TokenParentID: row.ParentTokenID.String,
		TokenPurpose: row.Purpose, RunID: runID, Method: MethodInternal, AuthAt: now, ExpiresAt: time.UnixMicro(row.ExpiresAt),
		revGen: gen}, nil
}
