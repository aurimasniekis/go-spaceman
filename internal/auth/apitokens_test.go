package auth

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
)

const spaceX = "spc_0000000000000000000000000x"

// allowed evaluates a principal against the live authorization engine, as the API layer does.
func (f *fx) allowed(p *Principal, dom string, perm authz.Permission) bool {
	return f.svc.Authz().AllowedAll(p.Subjects(), dom, perm)
}

// Automation tokens (SM-AUTH-006): issued once with a scoped grant set covered by the creator's grants; they act as the
// creator and are authorized as both token and creator, so they lose whatever the creator loses, and stop entirely when
// revoked, expired or when the creator is disabled.
func TestAPITokens(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	op := f.bootstrap()
	p := &Principal{User: op}
	sys := audit.Actor{Type: audit.ActorUser, ID: op.ID}

	for name, tc := range map[string]struct {
		name  string
		scope []GrantInput
		ttl   time.Duration
	}{
		"empty name":   {"", grants(authz.RoleAdmin), 30 * 24 * time.Hour},
		"too short":    {"ci", grants(authz.RoleAdmin), time.Hour},
		"too long":     {"ci", grants(authz.RoleAdmin), 400 * 24 * time.Hour},
		"unknown role": {"ci", []GrantInput{{Role: "root", Domain: authz.Global}}, 30 * 24 * time.Hour},
		"bad domain":   {"ci", []GrantInput{{Role: authz.RoleViewer, Domain: "spc_*"}}, 30 * 24 * time.Hour},
		"empty scope":  {"ci", nil, 30 * 24 * time.Hour},
	} {
		if _, _, err := f.svc.CreateAPIToken(f.ctx, p, tc.name, tc.scope, tc.ttl, false); !errors.Is(err, apperr.ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}

	tok, raw, err := f.svc.CreateAPIToken(f.ctx, p, "terraform", grants(authz.RoleAdmin), 30*24*time.Hour, false)
	must(t, err)
	if !strings.HasPrefix(raw, APITokenPrefix) || len(raw) != len(APITokenPrefix)+43 || tok.Hint != raw[len(raw)-4:] || len(tok.Grants) != 1 {
		t.Fatalf("token %q %+v", raw, tok)
	}
	got, err := f.svc.AuthenticateAPIToken(f.ctx, raw, "198.51.100.9")
	must(t, err)
	if got.TokenID != tok.ID || got.Method != MethodToken || got.User.ID != op.ID || len(got.Subjects()) != 2 {
		t.Fatalf("principal %+v", got)
	}
	if !f.allowed(got, authz.Global, authz.UserWrite) {
		t.Fatal("admin token denied")
	}
	if again, _ := f.svc.GetAPIToken(f.ctx, tok.ID); again.LastUsedAt == nil || again.LastUsedIP != "198.51.100.9" {
		t.Fatalf("last use not recorded: %+v", again)
	}
	for _, bad := range []string{"", raw[:len(raw)-1], "spaceman_at_" + strings.Repeat("A", 43), strings.Replace(raw, APITokenPrefix, "porch_at_", 1)} {
		if _, err := f.svc.AuthenticateAPIToken(f.ctx, bad, ""); !errors.Is(err, ErrUnauthorized) {
			t.Errorf("%q accepted: %v", bad, err)
		}
	}

	// A non-administrator may only scope tokens within their own grants; a token loses what its creator loses.
	rita, err := f.svc.Create(f.ctx, sys, CreateInput{Email: "rita@example.com", DisplayName: "Rita",
		Grants: []GrantInput{{Role: authz.RoleOperator, Domain: spaceX}, {Role: authz.RoleViewer, Domain: authz.Global}}})
	must(t, err)
	if _, _, err := f.svc.CreateAPIToken(f.ctx, &Principal{User: rita}, "admin", []GrantInput{{Role: authz.RoleAdmin, Domain: spaceX}}, 24*time.Hour, false); !errors.Is(err, apperr.ErrInvalid) {
		t.Fatalf("grant not held: %v", err)
	}
	if _, _, err := f.svc.CreateAPIToken(f.ctx, &Principal{User: rita}, "global-op", grants(authz.RoleOperator), 24*time.Hour, false); !errors.Is(err, apperr.ErrInvalid) {
		t.Fatalf("space grant widened to global: %v", err)
	}
	_, opsRaw, err := f.svc.CreateAPIToken(f.ctx, &Principal{User: rita}, "ops", []GrantInput{{Role: authz.RoleOperator, Domain: spaceX}}, 24*time.Hour, false)
	must(t, err)
	tp, err := f.svc.AuthenticateAPIToken(f.ctx, opsRaw, "")
	must(t, err)
	if !f.allowed(tp, spaceX, authz.QueueWrite) || f.allowed(tp, authz.Global, authz.NodeRead) {
		t.Fatal("token scope not applied (allowed within scope, nothing beyond)")
	}
	_, err = f.svc.SetGrants(f.ctx, sys, rita.ID, []GrantInput{{Role: authz.RoleViewer, Domain: spaceX}})
	must(t, err)
	if f.allowed(tp, spaceX, authz.QueueWrite) {
		t.Fatal("token kept a permission its user lost")
	}
	if !f.allowed(tp, spaceX, authz.QueueRead) {
		t.Fatal("token lost a permission both it and its user still hold")
	}
	_, err = f.svc.SetEnabled(f.ctx, sys, rita.ID, false)
	must(t, err)
	if _, err := f.svc.AuthenticateAPIToken(f.ctx, opsRaw, ""); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("disabled user's token accepted: %v", err)
	}

	// Expiry and revocation.
	f.clk.Advance(25 * time.Hour)
	_, err = f.svc.AuthenticateAPIToken(f.ctx, raw, "")
	must(t, err)
	revoked, err := f.svc.RevokeAPIToken(f.ctx, sys, tok.ID)
	must(t, err)
	if revoked.RevokedAt == nil || len(revoked.Grants) != 0 {
		t.Fatalf("not revoked: %+v", revoked)
	}
	if _, err := f.svc.AuthenticateAPIToken(f.ctx, raw, ""); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("revoked token accepted: %v", err)
	}
	if f.allowed(got, authz.Global, authz.UserWrite) {
		t.Fatal("revoked token's grants still effective for an in-flight principal")
	}
	_, short, err := f.svc.CreateAPIToken(f.ctx, &Principal{User: op}, "short", grants(authz.RoleAdmin), 24*time.Hour, false)
	must(t, err)
	f.clk.Advance(24 * time.Hour)
	if _, err := f.svc.AuthenticateAPIToken(f.ctx, short, ""); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expired token accepted: %v", err)
	}
	list, err := f.svc.ListAPITokens(f.ctx)
	must(t, err)
	if len(list) != 3 || list[len(list)-1].ID != tok.ID {
		t.Fatalf("list (revoked last): %+v", list)
	}
	mine, err := f.svc.ListUserAPITokens(f.ctx, rita.ID)
	must(t, err)
	if len(mine) != 1 {
		t.Fatalf("user tokens: %+v", mine)
	}
}

// A token issued without expiry keeps working past any expiry and still stops when revoked; it cannot be combined
// with an expiry.
func TestAPITokenNeverExpires(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	op := f.bootstrap()
	sys := audit.Actor{Type: audit.ActorUser, ID: op.ID}
	p := &Principal{User: op}
	if _, _, err := f.svc.CreateAPIToken(f.ctx, p, "tf", grants(authz.RoleAdmin), 30*24*time.Hour, true); !errors.Is(err, apperr.ErrInvalid) {
		t.Fatalf("never-expiring token with an expiry: %v", err)
	}
	tok, raw, err := f.svc.CreateAPIToken(f.ctx, p, "terraform", grants(authz.RoleAdmin), 0, true)
	must(t, err)
	if !tok.NeverExpires || tok.ExpiresAt != nil {
		t.Fatalf("token %+v", tok)
	}
	f.clk.Advance(5 * 366 * 24 * time.Hour)
	_, err = f.svc.AuthenticateAPIToken(f.ctx, raw, "")
	must(t, err)
	_, err = f.svc.RevokeAPIToken(f.ctx, sys, tok.ID)
	must(t, err)
	if _, err := f.svc.AuthenticateAPIToken(f.ctx, raw, ""); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("revoked never-expiring token accepted: %v", err)
	}
}

// A token may issue child tokens within its own scope; a child cannot issue tokens; revoking the parent revokes its
// children, each audited.
func TestAPITokensIssuedByTokens(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	op := f.bootstrap()
	sys := audit.Actor{Type: audit.ActorUser, ID: op.ID}
	tf, raw, err := f.svc.CreateAPIToken(f.ctx, &Principal{User: op}, "terraform", grants(authz.RoleAdmin), 0, true)
	must(t, err)
	tfp, err := f.svc.AuthenticateAPIToken(f.ctx, raw, "")
	must(t, err)

	child, childRaw, err := f.svc.CreateAPIToken(f.ctx, tfp, "deed", []GrantInput{{Role: authz.RoleViewer, Domain: spaceX}}, 0, true)
	must(t, err)
	if child.ParentTokenID != tf.ID || child.UserID != op.ID || len(child.Grants) != 1 {
		t.Fatalf("child %+v", child)
	}
	cp, err := f.svc.AuthenticateAPIToken(f.ctx, childRaw, "")
	must(t, err)
	if cp.TokenParentID != tf.ID || !f.allowed(cp, spaceX, authz.SpaceRead) || f.allowed(cp, spaceX, authz.QueueWrite) {
		t.Fatalf("child principal %+v", cp)
	}
	adminChild, _, err := f.svc.CreateAPIToken(f.ctx, tfp, "nested-admin", grants(authz.RoleAdmin), 24*time.Hour, false)
	must(t, err)
	adminChildP := &Principal{User: tfp.User, TokenID: adminChild.ID, TokenParentID: tf.ID}
	if _, _, err := f.svc.CreateAPIToken(f.ctx, adminChildP, "grandchild", grants(authz.RoleViewer), 24*time.Hour, false); !errors.Is(err, apperr.ErrForbidden) {
		t.Fatalf("a child token issued a token: %v", err)
	}

	// The child works while its creator holds the covering grant, not after.
	second, err := f.svc.Create(f.ctx, sys, CreateInput{Email: "second@example.com", DisplayName: "Second", Grants: grants(authz.RoleAdmin)})
	must(t, err)
	_, err = f.svc.SetGrants(f.ctx, audit.Actor{Type: audit.ActorUser, ID: second.ID}, op.ID, []GrantInput{{Role: authz.RoleViewer, Domain: "spc_0000000000000000000000000y"}})
	must(t, err)
	if f.allowed(cp, spaceX, authz.SpaceRead) {
		t.Fatal("child token outlived its creator's grant")
	}
	_, err = f.svc.SetGrants(f.ctx, audit.Actor{Type: audit.ActorUser, ID: second.ID}, op.ID, grants(authz.RoleAdmin))
	must(t, err)
	if !f.allowed(cp, spaceX, authz.SpaceRead) {
		t.Fatal("child token not restored with its creator's grant")
	}

	_, err = f.svc.RevokeAPIToken(f.ctx, sys, tf.ID)
	must(t, err)
	for _, id := range []string{child.ID, adminChild.ID} {
		if got, _ := f.svc.GetAPIToken(f.ctx, id); got.RevokedAt == nil {
			t.Fatalf("child %s not revoked with its parent", id)
		}
	}
	if _, err := f.svc.AuthenticateAPIToken(f.ctx, childRaw, ""); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("child of a revoked token accepted: %v", err)
	}
	var n int
	must(t, f.s.ReadDB().QueryRowContext(f.ctx, `SELECT count(*) FROM audit_events WHERE action = 'api_token.revoked'`).Scan(&n))
	if n != 3 {
		t.Fatalf("revocation audit events: %d", n)
	}
}
