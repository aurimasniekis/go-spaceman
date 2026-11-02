package auth

import (
	"errors"
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
)

// SM-AUTH-005: the root token is issued once, has full scope, never expires, is stored only as a MAC, can be rotated
// and revoked (never leaving no administrator), and every step is audited.
func TestRootTokenLifecycle(t *testing.T) {
	f := newFx(t)
	raw, rt, err := f.svc.IssueRootToken(f.ctx)
	must(t, err)
	if !strings.HasPrefix(raw, RootTokenPrefix) || len(raw) != len(RootTokenPrefix)+43 || rt.Hint != raw[len(raw)-4:] {
		t.Fatalf("root token %q %+v", raw, rt)
	}
	var stored int
	must(t, f.s.ReadDB().QueryRowContext(f.ctx, `SELECT count(*) FROM root_tokens WHERE instr(hint, ?) = 0 AND length(mac) = 32`, raw).Scan(&stored))
	if stored != 1 {
		t.Fatal("root token must be stored only as a MAC")
	}
	if _, _, err := f.svc.IssueRootToken(f.ctx); !errors.Is(err, ErrRootTokenExists) {
		t.Fatalf("second bootstrap: %v", err)
	}
	rp, err := f.svc.AuthenticateRootToken(f.ctx, raw, "192.0.2.1")
	must(t, err)
	if !rp.IsRoot() || rp.User != nil || len(rp.Subjects()) != 1 || rp.ActorID() != rt.ID {
		t.Fatalf("root principal %+v", rp)
	}
	f.clk.Advance(10 * 366 * 24 * 3600e9) // ten years: never expires
	if _, err := f.svc.AuthenticateRootToken(f.ctx, raw, ""); err != nil {
		t.Fatalf("root token expired: %v", err)
	}
	for _, perm := range []authz.Permission{authz.UserWrite, authz.NodeEnroll, authz.TerminalTakeover} {
		if !f.allowed(rp, authz.Global, perm) || !f.allowed(rp, spaceX, authz.TerminalTakeover) {
			t.Fatalf("root lacks %s", perm)
		}
	}
	for _, bad := range []string{"", raw[:len(raw)-1], RootTokenPrefix + strings.Repeat("A", 43), strings.Replace(raw, RootTokenPrefix, APITokenPrefix, 1)} {
		if _, err := f.svc.AuthenticateRootToken(f.ctx, bad, ""); !errors.Is(err, ErrUnauthorized) {
			t.Errorf("%q accepted: %v", bad, err)
		}
	}
	if _, err := f.svc.AuthenticateAPIToken(f.ctx, raw, ""); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("root token accepted as an API token")
	}

	rootActor := audit.Actor{Type: audit.ActorRoot, ID: rt.ID}
	// No administrator user yet: revoking would lock everyone out.
	if err := f.svc.RevokeRootToken(f.ctx, rootActor); !errors.Is(err, apperr.ErrPrecondition) {
		t.Fatalf("revoke without an admin: %v", err)
	}
	// Rotation: the old token stops at once, the new one works with the full grant.
	raw2, rt2, err := f.svc.RotateRootToken(f.ctx, rootActor)
	must(t, err)
	if rt2.ID == rt.ID || raw2 == raw {
		t.Fatal("rotation reused the token")
	}
	if _, err := f.svc.AuthenticateRootToken(f.ctx, raw, ""); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("rotated-out root token accepted")
	}
	if f.allowed(rp, authz.Global, authz.UserWrite) {
		t.Fatal("old root grant still effective after rotation")
	}
	rp2, err := f.svc.AuthenticateRootToken(f.ctx, raw2, "")
	must(t, err)
	if !f.allowed(rp2, authz.Global, authz.UserWrite) {
		t.Fatal("rotated root token lacks full scope")
	}

	admin, err := f.svc.Create(f.ctx, audit.Actor{Type: audit.ActorRoot, ID: rt2.ID}, CreateInput{Email: "a@example.com", DisplayName: "A",
		Password: strongPW, Grants: grants(authz.RoleAdmin)})
	must(t, err)
	if admin.ProvisionedVia != "bootstrap" || !admin.IsGlobalAdmin() || !admin.MFARequired {
		t.Fatalf("first admin %+v", admin)
	}
	must(t, f.svc.RevokeRootToken(f.ctx, audit.Actor{Type: audit.ActorUser, ID: admin.ID}))
	if _, err := f.svc.AuthenticateRootToken(f.ctx, raw2, ""); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("revoked root token accepted")
	}
	if _, err := f.svc.GetRootToken(f.ctx); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("active root token after revoke: %v", err)
	}
	// Recovery path: rotation works offline even with no active token.
	_, _, err = f.svc.RotateRootToken(f.ctx, audit.Actor{Type: audit.ActorCLI, ID: "admin"})
	must(t, err)

	var n int
	must(t, f.s.ReadDB().QueryRowContext(f.ctx,
		`SELECT count(*) FROM audit_events WHERE action IN ('root_token.issued','root_token.rotated','root_token.revoked')`).Scan(&n))
	if n != 4 {
		t.Fatalf("root token audit events: %d", n)
	}
}

// Root cannot issue API tokens (it is not a user); last-admin protection holds for grants.
func TestRootIsNotAUserAndLastAdminGuard(t *testing.T) {
	f := newFx(t)
	op := f.bootstrap()
	raw, _, err := f.svc.RotateRootToken(f.ctx, audit.Actor{Type: audit.ActorCLI})
	must(t, err)
	rp, err := f.svc.AuthenticateRootToken(f.ctx, raw, "")
	must(t, err)
	if _, _, err := f.svc.CreateAPIToken(f.ctx, rp, "x", grants(authz.RoleViewer), 0, true); !errors.Is(err, apperr.ErrForbidden) {
		t.Fatalf("root issued an API token: %v", err)
	}
	actor := audit.Actor{Type: audit.ActorUser, ID: op.ID}
	if _, err := f.svc.SetGrants(f.ctx, actor, op.ID, []GrantInput{{Role: authz.RoleAdmin, Domain: spaceX}}); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("last global admin demoted to a space admin: %v", err)
	}
	if _, err := f.svc.SetEnabled(f.ctx, actor, op.ID, false); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("last global admin disabled: %v", err)
	}
}
