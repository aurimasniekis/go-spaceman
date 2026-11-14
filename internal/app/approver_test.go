package app

import (
	"context"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/eventbus"
	"github.com/aurimasniekis/go-spaceman/internal/keyring/keyringtest"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/storetest"
)

// The approver re-check (SM-APPR-003) never counts a SpaceMan MCP call as an approver (ADR-0063): not by its "via",
// and not when an internal token is named as an API token, even though its owner and grants could decide.
func TestApproverRecheckRefusesSpacemanMCP(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := storetest.Open(t)
	kr := keyringtest.Load(t, s)
	e, err := authz.New(ctx, s.R())
	if err != nil {
		t.Fatal(err)
	}
	svc := auth.New(s, kr, audit.NewWriter(kr), eventbus.New(), e, auth.Options{})
	sys := audit.Actor{Type: audit.ActorSystem, ID: "test"}
	u, err := svc.Create(ctx, sys, auth.CreateInput{Email: "admin@example.com", DisplayName: "Admin",
		Grants: []auth.GrantInput{{Role: authz.RoleAdmin, Domain: authz.Global}}})
	if err != nil {
		t.Fatal(err)
	}
	owner := &auth.Principal{User: u}
	bearer, _, err := svc.CreateAPIToken(ctx, owner, "ci", []auth.GrantInput{{Role: authz.RoleAdmin, Domain: authz.Global}}, 30*24*time.Hour, false)
	if err != nil {
		t.Fatal(err)
	}
	internal, err := svc.CreateInternalToken(ctx, owner, auth.InternalTokenInput{Purpose: auth.TokenPurposeSpacemanMCP, Name: "SpaceMan",
		Grants: []auth.GrantInput{{Role: authz.RoleAdmin, Domain: authz.Global}}, NeverExpires: true})
	if err != nil {
		t.Fatal(err)
	}
	c := &Central{Store: s, Authz: e}
	const space = "spc_0000000000000000000000000x"
	for name, tc := range map[string]struct {
		by   audit.Actor
		want bool
	}{
		"user":                    {audit.Actor{Type: audit.ActorUser, ID: u.ID}, true},
		"bearer token":            {audit.Actor{Type: audit.ActorUser, ID: u.ID, Via: "api_token:" + bearer.ID}, true},
		"spaceman mcp":            {audit.Actor{Type: audit.ActorUser, ID: u.ID, Via: auth.ViaSpacemanMCP + internal.ID}, false},
		"internal token as token": {audit.Actor{Type: audit.ActorUser, ID: u.ID, Via: "api_token:" + internal.ID}, false},
	} {
		if got := c.approverStillAuthorized(ctx, tc.by, space); got != tc.want {
			t.Errorf("%s: authorized %v, want %v", name, got, tc.want)
		}
	}
}

// The approver re-check of a decision made through Telegram (ADR-0083 §3.2 change 5): it holds while the link is
// active, for the same user, who is enabled and not Limited, and the link's capped subject and the user's grants both
// allow approval:decide. A revoked or suspended link, a link capped to viewer, a removed user grant, a disabled user, a
// link of another user, or a missing link each fail it.
func TestApproverRecheckTelegram(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := storetest.Open(t)
	kr := keyringtest.Load(t, s)
	e, err := authz.New(ctx, s.R())
	if err != nil {
		t.Fatal(err)
	}
	svc := auth.New(s, kr, audit.NewWriter(kr), eventbus.New(), e, auth.Options{})
	sys := audit.Actor{Type: audit.ActorSystem, ID: "test"}
	const space = "spc_0000000000000000000000000x"
	mk := func(email string) *auth.User {
		u, err := svc.Create(ctx, sys, auth.CreateInput{Email: email, DisplayName: email,
			Grants: []auth.GrantInput{{Role: authz.RoleOperator, Domain: space}}})
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	if _, err := svc.Create(ctx, sys, auth.CreateInput{Email: "admin@example.com", DisplayName: "Admin",
		Grants: []auth.GrantInput{{Role: authz.RoleAdmin, Domain: authz.Global}}}); err != nil {
		t.Fatal(err)
	}
	u, other := mk("ada@example.com"), mk("bob@example.com")
	exec := func(q string, args ...any) {
		t.Helper()
		if err := s.W().Tx(ctx, func(tx *store.Tx) error {
			_, err := tx.SQL.ExecContext(ctx, q, args...)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if err := e.Reload(ctx); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO telegram_links (id, user_id, bot_id, tg_user_id, chat_id, state, created_at, updated_at) VALUES ('tgl_a', ?, 1, 1, 1, 'active', 0, 0)`, u.ID)
	setCap := func(role string) {
		exec(`DELETE FROM role_grants WHERE subject = 'telegram:tgl_a'`)
		exec(`INSERT INTO role_grants (subject, role, domain, source, created_at, created_by) VALUES ('telegram:tgl_a', ?, ?, 'telegram', 0, 't')`, role, space)
	}
	setCap("operator")
	c := &Central{Store: s, Authz: e, Auth: svc}
	by := audit.Actor{Type: audit.ActorUser, ID: u.ID, Via: "telegram:tgl_a", Channel: audit.ChannelTelegram}
	check := func(what string, by audit.Actor, want bool) {
		t.Helper()
		if got := c.approverStillAuthorized(ctx, by, space); got != want {
			t.Errorf("%s: authorized %v, want %v", what, got, want)
		}
	}
	check("active link", by, true)
	check("another user's actor on the link", audit.Actor{Type: audit.ActorUser, ID: other.ID, Via: "telegram:tgl_a"}, false)
	check("missing link", audit.Actor{Type: audit.ActorUser, ID: u.ID, Via: "telegram:tgl_none"}, false)
	check("empty link id", audit.Actor{Type: audit.ActorUser, ID: u.ID, Via: "telegram:"}, false)

	setCap("viewer")
	check("link capped to viewer", by, false)
	setCap("operator")
	check("cap restored", by, true)

	exec(`DELETE FROM role_grants WHERE subject = ?`, auth.UserSubject(u.ID))
	check("user grant removed (the link's own grant isn't enough)", by, false)
	exec(`INSERT INTO role_grants (subject, role, domain, source, created_at, created_by) VALUES (?, 'operator', ?, 'manual', 0, 't')`, auth.UserSubject(u.ID), space)
	check("user grant restored", by, true)

	exec(`UPDATE users SET mfa_required = 1 WHERE id = ?`, u.ID)
	check("user must still enroll MFA (Limited)", by, false)
	exec(`UPDATE users SET mfa_required = 0 WHERE id = ?`, u.ID)

	exec(`UPDATE telegram_links SET state = 'suspended' WHERE id = 'tgl_a'`)
	check("suspended link", by, false)
	exec(`UPDATE telegram_links SET state = 'active' WHERE id = 'tgl_a'`)
	check("reactivated link", by, true)

	if _, err := svc.SetEnabled(ctx, sys, u.ID, false); err != nil {
		t.Fatal(err)
	}
	check("disabled user", by, false)
	if _, err := svc.SetEnabled(ctx, sys, u.ID, true); err != nil {
		t.Fatal(err)
	}
	check("re-enabled user", by, true)

	exec(`UPDATE telegram_links SET state = 'revoked', revoked_at = 1 WHERE id = 'tgl_a'`)
	check("revoked link", by, false)
}
