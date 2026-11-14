package auth

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/store"
)

// A Telegram principal's subjects are its link's and its user's (ADR-0083 §3.2): the user's local subject unless
// TelegramSSO is set. A Telegram principal that also carries a token or the root token, or has no user, has no
// subjects at all, so it can never act as the token, as root, or as the user alone.
func TestTelegramPrincipalSubjects(t *testing.T) {
	t.Parallel()
	u := &User{ID: "usr_x"}
	p := &Principal{User: u, Method: MethodTelegram, TelegramLinkID: "tgl_1"}
	if s := p.Subjects(); !slices.Equal(s, []string{"telegram:tgl_1", "user:usr_x"}) {
		t.Fatalf("subjects %v", s)
	}
	if !p.ViaTelegram() || p.ViaMCP() || p.IsRoot() || p.Internal() {
		t.Fatalf("kind: telegram %v mcp %v root %v", p.ViaTelegram(), p.ViaMCP(), p.IsRoot())
	}
	sso := *p
	sso.TelegramSSO = true
	if s := sso.Subjects(); !slices.Equal(s, []string{"telegram:tgl_1", "user:usr_x#sso"}) {
		t.Fatalf("SSO subjects %v", s)
	}
	for name, bad := range map[string]*Principal{
		"with a token":      {User: u, TelegramLinkID: "tgl_1", TokenID: "atk_1"},
		"with a root token": {User: u, TelegramLinkID: "tgl_1", RootTokenID: "rtk_1"},
		"root only":         {TelegramLinkID: "tgl_1", RootTokenID: "rtk_1"},
		"without a user":    {TelegramLinkID: "tgl_1"},
	} {
		if s := bad.Subjects(); s != nil {
			t.Errorf("%s: subjects %v, want none", name, s)
		}
	}
	var nilP *Principal
	if nilP.ViaTelegram() || (&Principal{User: u}).ViaTelegram() {
		t.Fatal("ViaTelegram on a non-Telegram principal")
	}
}

// TelegramPrincipal builds a principal only for an active link of an enabled user; the SSO subject only while the
// policy allows it and the confirming SSO session's absolute expiry hasn't passed. The principal never has a token,
// never passes step-up, and is Limited while its user must still enroll MFA.
func TestTelegramPrincipal(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	admin := f.bootstrap()
	sys := audit.Actor{Type: audit.ActorUser, ID: admin.ID}
	bob, err := f.svc.Create(f.ctx, sys, CreateInput{Email: "bob@example.com", DisplayName: "Bob", Password: strongPW,
		Grants: []GrantInput{{Role: authz.RoleViewer, Domain: spaceX}}})
	must(t, err)
	now := f.clk.Now().UnixMicro()
	link := func(id, state string, tgUser int64, viaSSO int, ssoUntil any) {
		t.Helper()
		var revokedAt any
		if state == "revoked" {
			revokedAt = now
		}
		must(t, f.exec(`INSERT INTO telegram_links (id, user_id, bot_id, tg_user_id, chat_id, state, via_sso, sso_until, created_at,
			updated_at, revoked_at) VALUES (?, ?, 1, ?, ?, ?, ?, ?, ?, ?, ?)`, id, bob.ID, tgUser, tgUser, state, viaSSO, ssoUntil, now, now, revokedAt))
	}
	link("tgl_active", "active", 100, 0, nil)
	link("tgl_suspended", "suspended", 101, 0, nil)
	link("tgl_blocked", "blocked", 102, 0, nil)
	link("tgl_revoked", "revoked", 103, 0, nil)
	link("tgl_sso", "active", 104, 1, f.clk.Now().Add(time.Hour).UnixMicro())
	link("tgl_local", "active", 105, 0, f.clk.Now().Add(time.Hour).UnixMicro())

	p, err := f.svc.TelegramPrincipal(f.ctx, "tgl_active", TelegramOptions{SSOGrants: true})
	must(t, err)
	if p.User.ID != bob.ID || p.TelegramLinkID != "tgl_active" || p.Method != MethodTelegram || p.TelegramSSO || p.Limited ||
		p.TokenID != "" || p.RootTokenID != "" || p.SessionID != "" || !p.AuthAt.IsZero() || p.MFAAt != nil {
		t.Fatalf("principal %+v", p)
	}
	if s := p.Subjects(); !slices.Equal(s, []string{"telegram:tgl_active", "user:" + bob.ID}) {
		t.Fatalf("subjects %v", s)
	}
	// Step-up never passes: AuthAt is zero, whatever the reauth window.
	if err := f.svc.RequireRecentAuth(p, false); !errors.Is(err, ErrReauthRequired) {
		t.Fatalf("step-up: %v", err)
	}

	for _, id := range []string{"tgl_suspended", "tgl_blocked", "tgl_revoked", "tgl_missing", "", strings.Repeat("x", 65)} {
		if _, err := f.svc.TelegramPrincipal(f.ctx, id, TelegramOptions{}); !errors.Is(err, ErrUnauthorized) {
			t.Errorf("link %q: %v, want unauthorized", id, err)
		}
	}

	// SSO: only with the policy on, a via_sso link and an sso_until in the future.
	if p, err := f.svc.TelegramPrincipal(f.ctx, "tgl_sso", TelegramOptions{}); err != nil || p.TelegramSSO {
		t.Fatalf("policy off: %+v %v", p, err)
	}
	if p, err := f.svc.TelegramPrincipal(f.ctx, "tgl_sso", TelegramOptions{SSOGrants: true}); err != nil || !p.TelegramSSO ||
		!slices.Equal(p.Subjects(), []string{"telegram:tgl_sso", "user:" + bob.ID + "#sso"}) {
		t.Fatalf("policy on: %+v %v", p, err)
	}
	if p, err := f.svc.TelegramPrincipal(f.ctx, "tgl_local", TelegramOptions{SSOGrants: true}); err != nil || p.TelegramSSO {
		t.Fatalf("not confirmed from SSO: %+v %v", p, err)
	}
	f.clk.Advance(time.Hour)
	if p, err := f.svc.TelegramPrincipal(f.ctx, "tgl_sso", TelegramOptions{SSOGrants: true}); err != nil || p.TelegramSSO {
		t.Fatalf("sso_until passed: %+v %v", p, err)
	}
	must(t, f.exec(`UPDATE telegram_links SET sso_until = NULL WHERE id = 'tgl_sso'`))
	if p, err := f.svc.TelegramPrincipal(f.ctx, "tgl_sso", TelegramOptions{SSOGrants: true}); err != nil || p.TelegramSSO {
		t.Fatalf("no sso_until: %+v %v", p, err)
	}

	// MFA required but not enrolled: Limited, like the user's web session.
	must(t, f.exec(`UPDATE users SET mfa_required = 1 WHERE id = ?`, bob.ID))
	if p, err := f.svc.TelegramPrincipal(f.ctx, "tgl_active", TelegramOptions{}); err != nil || !p.Limited {
		t.Fatalf("MFA required: %+v %v", p, err)
	}
	must(t, f.exec(`UPDATE users SET mfa_required = 0 WHERE id = ?`, bob.ID))

	// A disabled user's link stops at once.
	_, err = f.svc.SetEnabled(f.ctx, sys, bob.ID, false)
	must(t, err)
	if _, err := f.svc.TelegramPrincipal(f.ctx, "tgl_active", TelegramOptions{}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("disabled user: %v", err)
	}
}

// A change to a Telegram link signals exactly that link's streams, after commit; the user's revocations reach them too.
func TestTelegramLinkChangeSignalsStreams(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	r := f.svc.Revocations()
	u := &User{ID: "usr_x"}
	w1 := r.Watch(&Principal{User: u, Method: MethodTelegram, TelegramLinkID: "tgl_1"})
	w2 := r.Watch(&Principal{User: &User{ID: "usr_y"}, Method: MethodTelegram, TelegramLinkID: "tgl_2"})
	defer w1.Close()
	defer w2.Close()
	drain(w1, w2)
	must(t, f.s.W().Tx(f.ctx, func(tx *store.Tx) error {
		f.svc.TelegramLinkChangedAfterCommit(tx, "tgl_1")
		return nil
	}))
	expectSignals(t, "link change", map[*StreamWatch]bool{w1: true}, w1, w2)
	r.signal(revocation{userID: "usr_x"})
	expectSignals(t, "user revocation", map[*StreamWatch]bool{w1: true}, w1, w2)
	errRollback := errors.New("rollback")
	if err := f.s.W().Tx(f.ctx, func(tx *store.Tx) error {
		f.svc.TelegramLinkChangedAfterCommit(tx, "tgl_1")
		return errRollback
	}); !errors.Is(err, errRollback) {
		t.Fatal(err)
	}
	expectSignals(t, "rolled back", nil, w1, w2)
}
