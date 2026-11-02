package auth

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Break-glass use raises an alert that stays until acknowledged; acknowledging is audited.
func TestBreakGlassAlert(t *testing.T) {
	f := newFx(t)
	op := f.bootstrap()
	if on, _, err := f.svc.BreakGlassAlert(f.ctx); err != nil || on {
		t.Fatalf("alert before use: %v %v", on, err)
	}
	must(t, f.svc.AcknowledgeBreakGlass(f.ctx, audit.Actor{Type: audit.ActorUser, ID: op.ID})) // nothing to acknowledge: no-op
	_, _, err := f.svc.IssueRecoveryLogin(f.ctx, op.ID, "")
	must(t, err)
	on, at, err := f.svc.BreakGlassAlert(f.ctx)
	if err != nil || !on || at.IsZero() {
		t.Fatalf("alert after use: %v %v %v", on, at, err)
	}
	must(t, f.svc.AcknowledgeBreakGlass(f.ctx, audit.Actor{Type: audit.ActorUser, ID: op.ID}))
	if on, _, _ := f.svc.BreakGlassAlert(f.ctx); on {
		t.Fatal("alert survived acknowledgement")
	}
}

// External step-up (SSO) rotates the session with a fresh auth time; the TOTP fallback is SSO-only and needs a
// correct code.
func TestExternalReauth(t *testing.T) {
	f := newFx(t)
	op := f.bootstrap()
	f.loginEnrolled("admin@example.com")
	_, sso := f.session(op.ID, MethodOIDC, SessionMeta{Provider: "porch"})
	tok, err := f.svc.ReauthExternal(f.ctx, sso, "porch", SessionMeta{})
	must(t, err)
	p, err := f.svc.Authenticate(f.ctx, tok)
	must(t, err)
	if p.Provider != "porch" || p.Method != MethodOIDC {
		t.Fatalf("rotated SSO session %+v", p)
	}
	if _, err := f.svc.Authenticate(f.ctx, "stale"); !errors.Is(err, ErrUnauthorized) {
		t.Fatal(err)
	}
	if _, err := f.svc.ReauthTOTP(f.ctx, p, "000000", SessionMeta{}); !errors.Is(err, ErrMFARequired) {
		t.Fatalf("wrong step-up code: %v", err)
	}
	f.clk.Advance(30 * time.Second) // the enrollment login used this step's code; replay protection refuses it
	tok2, err := f.svc.ReauthTOTP(f.ctx, p, f.totpCode(op.ID), SessionMeta{})
	must(t, err)
	p2, err := f.svc.Authenticate(f.ctx, tok2)
	must(t, err)
	if p2.MFAAt == nil {
		t.Fatal("TOTP step-up did not record MFA")
	}
	// The rotated-out session cannot rotate again.
	if _, err := f.svc.ReauthExternal(f.ctx, p, "porch", SessionMeta{}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("reauth on a revoked session: %v", err)
	}
}

func TestPrincipalSubjects(t *testing.T) {
	var nilP *Principal
	if nilP.Subjects() != nil || nilP.ActorID() != "" || nilP.IsRoot() {
		t.Fatal("nil principal")
	}
	if (&Principal{}).Subjects() != nil || (&Principal{}).ActorID() != "" {
		t.Fatal("empty principal must have no subjects")
	}
	u := &User{ID: "usr_x"}
	if s := (&Principal{User: u}).Subjects(); len(s) != 1 || s[0] != u.Subject() {
		t.Fatalf("session subjects %v", s)
	}
	if !(&Principal{User: u}).MFASatisfied() || (&Principal{User: u, Limited: true}).MFASatisfied() {
		t.Fatal("MFASatisfied")
	}
	if a := (&Principal{User: u}).ActorID(); a != "usr_x" {
		t.Fatal(a)
	}
	if s := (&Principal{RootTokenID: "rtk_1"}).Subjects(); len(s) != 1 || s[0] != RootSubject("rtk_1") {
		t.Fatal(s)
	}
}

func TestLocalLoginDisabled(t *testing.T) {
	f := newFx(t)
	f.bootstrap()
	must(t, f.s.W().Tx(f.ctx, func(tx *store.Tx) error {
		return tx.Q.UpsertSetting(f.ctx, db.UpsertSettingParams{Key: SettingLocalLoginDisabled, Value: "true", UpdatedAt: tx.NowMicros(),
			UpdatedBy: sql.NullString{}})
	}))
	if off, err := f.svc.LocalLoginDisabled(f.ctx); err != nil || !off {
		t.Fatalf("setting not read: %v %v", off, err)
	}
	if _, err := f.svc.Login(f.ctx, LoginInput{Email: "admin@example.com", Password: strongPW}); !errors.Is(err, ErrLocalLoginDisabled) {
		t.Fatalf("password login while disabled: %v", err)
	}
	raw, rt, err := f.svc.RotateRootToken(f.ctx, audit.Actor{Type: audit.ActorCLI})
	must(t, err)
	got, err := f.svc.GetRootToken(f.ctx)
	must(t, err)
	if got.ID != rt.ID || got.Hint != raw[len(raw)-4:] {
		t.Fatalf("root token metadata %+v", got)
	}
	if _, err := f.svc.Get(f.ctx, "usr_missing"); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatal(err)
	}
}
