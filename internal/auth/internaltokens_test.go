package auth

import (
	"errors"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/store"
)

func (f *fx) exec(q string, args ...any) error {
	return f.s.W().Tx(f.ctx, func(tx *store.Tx) error {
		_, err := tx.SQL.ExecContext(f.ctx, q, args...)
		return err
	})
}

func spacemanMCP(name string, scope []GrantInput, ttl time.Duration, never bool) InternalTokenInput {
	return InternalTokenInput{Purpose: TokenPurposeSpacemanMCP, Name: name, Grants: scope, TTL: ttl, NeverExpires: never}
}

// Internal tokens (ADR-0063): minted under the same rules as API tokens (a user, covered grants, no token-issued
// issuer), additionally covered by the issuing token's grants; never usable as a bearer; their principal acts for the
// owner and the run and fails once the token is revoked or expired or the owner is disabled.
func TestInternalTokens(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	op := f.bootstrap()
	admin := &Principal{User: op}
	sys := audit.Actor{Type: audit.ActorUser, ID: op.ID}
	const day = 24 * time.Hour

	for name, tc := range map[string]struct {
		p  *Principal
		in InternalTokenInput
		ok error
	}{
		"unknown purpose": {admin, InternalTokenInput{Purpose: "bearer", Name: "x", Grants: grants(authz.RoleAdmin), NeverExpires: true}, apperr.ErrInvalid},
		"empty purpose":   {admin, InternalTokenInput{Name: "x", Grants: grants(authz.RoleAdmin), NeverExpires: true}, apperr.ErrInvalid},
		"no user":         {&Principal{RootTokenID: "rtk_1"}, spacemanMCP("x", grants(authz.RoleAdmin), 0, true), apperr.ErrForbidden},
		"internal issuer": {&Principal{User: op, TokenID: "atk_1", TokenPurpose: TokenPurposeSpacemanMCP}, spacemanMCP("x", grants(authz.RoleViewer), 0, true), apperr.ErrForbidden},
		"endpoint issuer": {&Principal{User: op, TokenID: "atk_1", MCPEndpoint: true}, spacemanMCP("x", grants(authz.RoleViewer), 0, true), apperr.ErrForbidden},
		"token-issued":    {&Principal{User: op, TokenID: "atk_1", TokenParentID: "atk_0"}, spacemanMCP("x", grants(authz.RoleViewer), 0, true), apperr.ErrForbidden},
		"empty name":      {admin, spacemanMCP(" ", grants(authz.RoleAdmin), 0, true), apperr.ErrInvalid},
		"no grants":       {admin, spacemanMCP("x", nil, 0, true), apperr.ErrInvalid},
		"bad domain":      {admin, spacemanMCP("x", []GrantInput{{Role: authz.RoleViewer, Domain: "spc_*"}}, 0, true), apperr.ErrInvalid},
		"ttl and never":   {admin, spacemanMCP("x", grants(authz.RoleAdmin), day, true), apperr.ErrInvalid},
		"ttl too short":   {admin, spacemanMCP("x", grants(authz.RoleAdmin), time.Hour, false), apperr.ErrInvalid},
		"ttl too long":    {admin, spacemanMCP("x", grants(authz.RoleAdmin), 400*day, false), apperr.ErrInvalid},
	} {
		if _, err := f.svc.CreateInternalToken(f.ctx, tc.p, tc.in); !errors.Is(err, tc.ok) {
			t.Errorf("%s: %v", name, err)
		}
	}

	// The default preset: full admin on every space, never expiring.
	tok, err := f.svc.CreateInternalToken(f.ctx, admin, spacemanMCP("SpaceMan (infra)", grants(authz.RoleAdmin), 0, true))
	must(t, err)
	if tok.Purpose != TokenPurposeSpacemanMCP || tok.Hint != "" || !tok.NeverExpires || tok.ExpiresAt != nil || len(tok.Grants) != 1 || tok.ParentTokenID != "" {
		t.Fatalf("internal token %+v", tok)
	}
	p, err := f.svc.InternalPrincipal(f.ctx, tok.ID, "run_1")
	must(t, err)
	if !p.Internal() || p.Method != MethodInternal || p.RunID != "run_1" || p.User.ID != op.ID || p.TokenID != tok.ID || len(p.Subjects()) != 2 {
		t.Fatalf("principal %+v", p)
	}
	if !f.allowed(p, authz.Global, authz.UserWrite) {
		t.Fatal("admin internal token denied")
	}
	// An internal principal never issues tokens of either kind.
	if _, _, err := f.svc.CreateAPIToken(f.ctx, p, "escalate", grants(authz.RoleViewer), day, false); !errors.Is(err, apperr.ErrForbidden) {
		t.Fatalf("internal principal issued an API token: %v", err)
	}
	if _, err := f.svc.CreateInternalToken(f.ctx, p, spacemanMCP("again", grants(authz.RoleViewer), 0, true)); !errors.Is(err, apperr.ErrForbidden) {
		t.Fatalf("internal principal issued an internal token: %v", err)
	}

	// Never a bearer: a token row with a purpose is refused even when the presented value matches its MAC.
	bearer, raw, err := f.svc.CreateAPIToken(f.ctx, admin, "ci", grants(authz.RoleViewer), 30*day, false)
	must(t, err)
	if _, err := f.svc.AuthenticateAPIToken(f.ctx, raw, ""); err != nil {
		t.Fatal(err)
	}
	must(t, f.exec(`UPDATE api_tokens SET purpose = 'spaceman_mcp' WHERE id = ?`, bearer.ID))
	if _, err := f.svc.AuthenticateAPIToken(f.ctx, raw, ""); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("internal token accepted as a bearer: %v", err)
	}
	// And an ordinary token's ID never yields an internal principal.
	_, raw2, err := f.svc.CreateAPIToken(f.ctx, admin, "ci2", grants(authz.RoleViewer), 30*day, false)
	must(t, err)
	bp, err := f.svc.AuthenticateAPIToken(f.ctx, raw2, "")
	must(t, err)
	for _, id := range []string{bp.TokenID, "atk_unknown"} {
		if _, err := f.svc.InternalPrincipal(f.ctx, id, "run_1"); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("%s gave an internal principal: %v", id, err)
		}
	}
	if err := f.exec(`UPDATE api_tokens SET purpose = 'bearer' WHERE id = ?`, bp.TokenID); err == nil {
		t.Fatal("an unknown purpose was stored")
	}

	// Covered grants: the owner's current grants, and the issuing token's.
	rita, err := f.svc.Create(f.ctx, sys, CreateInput{Email: "rita@example.com", DisplayName: "Rita",
		Grants: []GrantInput{{Role: authz.RoleOperator, Domain: spaceX}, {Role: authz.RoleViewer, Domain: authz.Global}}})
	must(t, err)
	rp := &Principal{User: rita}
	for _, scope := range [][]GrantInput{{{Role: authz.RoleAdmin, Domain: spaceX}}, grants(authz.RoleOperator)} {
		if _, err := f.svc.CreateInternalToken(f.ctx, rp, spacemanMCP("x", scope, 0, true)); !errors.Is(err, apperr.ErrInvalid) {
			t.Fatalf("%v beyond the owner's grants: %v", scope, err)
		}
	}
	_, ritaRaw, err := f.svc.CreateAPIToken(f.ctx, rp, "rita-ci", []GrantInput{{Role: authz.RoleViewer, Domain: spaceX}}, 30*day, false)
	must(t, err)
	rtp, err := f.svc.AuthenticateAPIToken(f.ctx, ritaRaw, "")
	must(t, err)
	if _, err := f.svc.CreateInternalToken(f.ctx, rtp, spacemanMCP("x", []GrantInput{{Role: authz.RoleOperator, Domain: spaceX}}, 0, true)); !errors.Is(err, apperr.ErrInvalid) {
		t.Fatalf("grant beyond the issuing token accepted: %v", err)
	}
	child, err := f.svc.CreateInternalToken(f.ctx, rtp, spacemanMCP("x", []GrantInput{{Role: authz.RoleViewer, Domain: spaceX}}, 0, true))
	must(t, err)
	if child.ParentTokenID != rtp.TokenID {
		t.Fatalf("parent not recorded: %+v", child)
	}
	// A child never outlives its parent: asked never to expire, it expires with the 30-day token that issued it.
	if child.NeverExpires || child.ExpiresAt == nil || !child.ExpiresAt.Equal(rtp.ExpiresAt) {
		t.Fatalf("child of a 30-day token: never %v, expires %v, parent expires %v", child.NeverExpires, child.ExpiresAt, rtp.ExpiresAt)
	}
	short, err := f.svc.CreateInternalToken(f.ctx, rtp, spacemanMCP("short", []GrantInput{{Role: authz.RoleViewer, Domain: spaceX}}, 2*day, false))
	must(t, err)
	if short.ExpiresAt == nil || !short.ExpiresAt.Before(rtp.ExpiresAt) {
		t.Fatalf("a shorter child was extended: %v (parent %v)", short.ExpiresAt, rtp.ExpiresAt)
	}
	ops, err := f.svc.CreateInternalToken(f.ctx, rp, spacemanMCP("ops", []GrantInput{{Role: authz.RoleOperator, Domain: spaceX}}, 2*day, false))
	must(t, err)
	opsP, err := f.svc.InternalPrincipal(f.ctx, ops.ID, "run_2")
	must(t, err)
	if !f.allowed(opsP, spaceX, authz.QueueWrite) || f.allowed(opsP, authz.Global, authz.NodeRead) {
		t.Fatal("internal token scope not applied")
	}
	// A token loses what its owner loses.
	_, err = f.svc.SetGrants(f.ctx, sys, rita.ID, []GrantInput{{Role: authz.RoleViewer, Domain: spaceX}})
	must(t, err)
	if f.allowed(opsP, spaceX, authz.QueueWrite) || !f.allowed(opsP, spaceX, authz.QueueRead) {
		t.Fatal("internal token kept a permission its owner lost")
	}

	// Revoking the issuing token revokes the internal token it parented.
	_, err = f.svc.RevokeAPIToken(f.ctx, sys, rtp.TokenID)
	must(t, err)
	if _, err := f.svc.InternalPrincipal(f.ctx, child.ID, "run_1"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("child of a revoked token accepted: %v", err)
	}
	// Expiry.
	f.clk.Advance(2 * day)
	if _, err := f.svc.InternalPrincipal(f.ctx, ops.ID, "run_2"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expired internal token accepted: %v", err)
	}
	// Revocation.
	if _, err := f.svc.InternalPrincipal(f.ctx, tok.ID, "run_1"); err != nil {
		t.Fatalf("never-expiring token expired: %v", err)
	}
	_, err = f.svc.RevokeAPIToken(f.ctx, sys, tok.ID)
	must(t, err)
	if _, err := f.svc.InternalPrincipal(f.ctx, tok.ID, "run_1"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("revoked internal token accepted: %v", err)
	}
	if f.allowed(p, authz.Global, authz.UserWrite) {
		t.Fatal("revoked internal token's grants still effective for an in-flight principal")
	}
	// A disabled owner.
	never, err := f.svc.CreateInternalToken(f.ctx, rp, spacemanMCP("viewer", []GrantInput{{Role: authz.RoleViewer, Domain: spaceX}}, 0, true))
	must(t, err)
	_, err = f.svc.SetEnabled(f.ctx, sys, rita.ID, false)
	must(t, err)
	if _, err := f.svc.InternalPrincipal(f.ctx, never.ID, "run_3"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("disabled owner's internal token accepted: %v", err)
	}

	// Minting is audited with the purpose, attributed to the owner (and the issuing token).
	var n int
	must(t, f.s.ReadDB().QueryRowContext(f.ctx, `SELECT count(*) FROM audit_events WHERE action = 'api_token.created'
		AND json_extract(details_json, '$.purpose') = 'spaceman_mcp'`).Scan(&n))
	if n != 5 {
		t.Fatalf("%d internal token creations audited, want 5", n)
	}
}
