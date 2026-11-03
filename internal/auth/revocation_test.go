package auth

import (
	"errors"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
)

func signalled(w *StreamWatch) bool {
	select {
	case <-w.C:
		return true
	default:
		return false
	}
}

func drain(ws ...*StreamWatch) {
	for _, w := range ws {
		signalled(w)
	}
}

// expectSignals checks which watches were signalled by the last committed change, then drains them all.
func expectSignals(t *testing.T, what string, want map[*StreamWatch]bool, all ...*StreamWatch) {
	t.Helper()
	for _, w := range all {
		if got := signalled(w); got != want[w] {
			t.Errorf("%s: watch signalled=%v, want %v", what, got, want[w])
		}
	}
}

// SM-AUTH-007 (ADR-0033): every committed revocation signals exactly the streams it may affect, once, after commit;
// a grant change signals every stream; a rolled-back transaction signals nothing.
func TestStreamWatchSignals(t *testing.T) {
	f := newFx(t)
	admin := f.bootstrap()
	sys := audit.Actor{Type: audit.ActorUser, ID: admin.ID}
	adminTok, adminP := f.loginEnrolled("admin@example.com")
	bob, err := f.svc.Create(f.ctx, sys, CreateInput{Email: "bob@example.com", DisplayName: "Bob", Password: strongPW,
		Grants: []GrantInput{{Role: authz.RoleViewer, Domain: spaceX}}})
	must(t, err)
	login := func() (string, *Principal) {
		res, err := f.svc.Login(f.ctx, LoginInput{Email: "bob@example.com", Password: strongPW})
		must(t, err)
		if res.Principal.Limited {
			t.Fatal("viewer session limited")
		}
		return res.Token, res.Principal
	}
	bobTok, bobP := login()
	_, bob2P := login()
	_, tokRaw, err := f.svc.CreateAPIToken(f.ctx, adminP, "stream", grants(authz.RoleViewer), 30*24*time.Hour, false)
	must(t, err)
	tokP, err := f.svc.AuthenticateAPIToken(f.ctx, tokRaw, "")
	must(t, err)
	rootRaw, _, err := f.svc.RotateRootToken(f.ctx, sys)
	must(t, err)
	rootP, err := f.svc.AuthenticateRootToken(f.ctx, rootRaw, "")
	must(t, err)

	r := f.svc.Revocations()
	wAdmin, wBob, wBob2, wTok, wRoot := r.Watch(adminP), r.Watch(bobP), r.Watch(bob2P), r.Watch(tokP), r.Watch(rootP)
	all := []*StreamWatch{wAdmin, wBob, wBob2, wTok, wRoot}
	// Principals authenticated before later revocations start with a pending re-check (no revocation is missed).
	if !signalled(wAdmin) {
		t.Fatal("watch of a principal authenticated before a revocation was not pre-signalled")
	}
	drain(all...)
	if r.Active() != len(all) {
		t.Fatalf("active watches %d", r.Active())
	}

	must(t, f.svc.Logout(f.ctx, bobTok))
	expectSignals(t, "logout", map[*StreamWatch]bool{wBob: true}, all...)

	must(t, f.svc.RevokeSession(f.ctx, bob.ID, bob2P.SessionID, "user"))
	expectSignals(t, "revoke one session", map[*StreamWatch]bool{wBob2: true}, all...)

	_, err = f.svc.RevokeAPIToken(f.ctx, sys, tokP.TokenID)
	must(t, err)
	// The token's grants were deleted, so the engine reloaded: every stream re-checks.
	expectSignals(t, "revoke API token", map[*StreamWatch]bool{wAdmin: true, wBob: true, wBob2: true, wTok: true, wRoot: true}, all...)

	_, err = f.svc.SetEnabled(f.ctx, sys, bob.ID, false)
	must(t, err)
	expectSignals(t, "disable user", map[*StreamWatch]bool{wBob: true, wBob2: true}, all...)

	must(t, f.svc.RevokeAllSessions(f.ctx, admin.ID, "test"))
	// The user's API-token streams re-check too (a revoked token still fails; a valid one continues).
	expectSignals(t, "revoke all sessions", map[*StreamWatch]bool{wAdmin: true, wTok: true}, all...)

	_, err = f.svc.SetGrants(f.ctx, sys, bob.ID, []GrantInput{{Role: authz.RoleOperator, Domain: spaceX}})
	must(t, err)
	expectSignals(t, "grants changed", map[*StreamWatch]bool{wAdmin: true, wBob: true, wBob2: true, wTok: true, wRoot: true}, all...)

	must(t, f.svc.RevokeRootToken(f.ctx, sys))
	if !signalled(wRoot) {
		t.Error("root token revocation did not signal its stream")
	}
	drain(all...)

	// A transaction that rolls back revokes nothing and signals nothing.
	if _, err := f.svc.SetEnabled(f.ctx, sys, admin.ID, false); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("disabling the last admin: %v", err)
	}
	expectSignals(t, "rolled back", nil, all...)

	// Rotation revokes the old session token: its stream re-checks (and the client reconnects with the new one).
	_, adminP2 := f.loginEnrolled("admin@example.com")
	wAdmin2 := r.Watch(adminP2)
	drain(wAdmin2)
	_, err = f.svc.Rotate(f.ctx, adminP2, SessionMeta{})
	must(t, err)
	if !signalled(wAdmin2) {
		t.Error("session rotation did not signal its stream")
	}
	_ = adminTok

	for _, w := range append(all, wAdmin2) {
		w.Close()
		w.Close() // idempotent
	}
	if r.Active() != 0 {
		t.Fatalf("watches left after close: %d", r.Active())
	}
	must(t, f.svc.Logout(f.ctx, adminTok)) // signalling with no watches is a no-op
}

// A user's own password change and MFA changes signal the user's streams; the stream that made the change
// re-checks and keeps going, because its session is the one kept.
func TestStreamWatchUserChanges(t *testing.T) {
	f := newFx(t)
	f.bootstrap()
	tok, p := f.loginEnrolled("admin@example.com")
	w := f.svc.Revocations().Watch(p)
	defer w.Close()
	drain(w)
	must(t, f.svc.ChangePassword(f.ctx, p, strongPW, strongPW+"!"))
	if !signalled(w) {
		t.Fatal("password change did not signal")
	}
	if _, err := f.svc.Validate(f.ctx, tok); err != nil {
		t.Fatalf("the changing session must stay valid: %v", err)
	}
	reset, _, err := f.svc.IssueResetToken(f.ctx, audit.System, p.User.ID)
	must(t, err)
	must(t, f.svc.ResetPassword(f.ctx, reset, strongPW+"?"))
	if !signalled(w) {
		t.Fatal("password reset did not signal")
	}
	if _, err := f.svc.Validate(f.ctx, tok); err == nil {
		t.Fatal("a password reset must end every session")
	}
	if w2 := f.svc.Revocations().Watch(nil); !signalled(w2) {
		t.Fatal("an anonymous watch must re-check at once")
	} else {
		w2.Close()
	}
}
