package app_test

import (
	"context"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/api"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/auth"
)

// SM-AUTH-001 (Hallpass HP-AUTH-010/016) over HTTP: an administrator-issued reset token sets a new password once;
// a CLI-issued break-glass token signs in, removes the lost factor, hands out a password reset and raises an alert until
// an administrator acknowledges it; recovery codes can be replaced only after a recent re-authentication with TOTP.
func TestAPIRecoveryAndReset(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	rootSecret, _, err := h.c.Auth.IssueRootToken(ctx)
	if err != nil {
		t.Fatal(err)
	}
	root := h.client()
	root.bearer = rootSecret
	var u auth.User
	r := root.do("POST", "/users", map[string]any{"email": "ada@example.com", "display_name": "Ada", "password": "correct horse battery staple",
		"grants": []map[string]string{{"role": "admin", "domain": "*"}}})
	expect(t, r, 201)
	r.json(t, &u)

	// Password reset with an administrator-issued token: single use.
	tok, _, err := h.c.Auth.IssueResetToken(ctx, audit.Actor{Type: audit.ActorUser, ID: "usr_admin"}, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	anon := h.client()
	expect(t, anon.do("POST", "/auth/reset", map[string]string{"token": tok, "password": "a brand new long passphrase"}), 204)
	if p := anon.do("POST", "/auth/reset", map[string]string{"token": tok, "password": "another long passphrase!"}).problem(t); p.Code != "unauthorized" {
		t.Fatalf("reset token reused: %+v", p)
	}
	expect(t, anon.do("POST", "/auth/login", map[string]string{"email": "ada@example.com", "password": "correct horse battery staple"}), 401)
	r = anon.do("POST", "/auth/login", map[string]string{"email": "ada@example.com", "password": "a brand new long passphrase"})
	expect(t, r, 200)

	// Enroll TOTP, so recovery has a factor to remove.
	var me api.Me
	r.json(t, &me)
	anon.csrf = me.CSRFToken
	r = anon.do("POST", "/auth/mfa/enroll", nil)
	var enr api.TOTPEnrollment
	r.json(t, &enr)
	secret, _ := auth.DecodeTOTPSecret(enr.Secret)
	expect(t, anon.do("POST", "/auth/mfa/enroll/confirm", map[string]string{"code": auth.TOTPCode(secret, auth.TOTPStep(h.clk.Now()))}), 200)
	h.clk.Advance(30 * time.Second)
	r = anon.do("POST", "/auth/mfa/verify", map[string]string{"code": auth.TOTPCode(secret, auth.TOTPStep(h.clk.Now()))})
	expect(t, r, 200)
	r.json(t, &me)
	anon.csrf = me.CSRFToken

	// Recovery codes: replaced with a recent re-authentication including TOTP.
	h.clk.Advance(30 * time.Minute)
	if p := anon.do("POST", "/auth/mfa/recovery-codes", nil).problem(t); p.Code != "reauth_required" && p.Code != "mfa_required" {
		t.Fatalf("stale session replaced recovery codes: %+v", p)
	}
	r = anon.do("POST", "/auth/reauth", map[string]string{"password": "a brand new long passphrase", "code": auth.TOTPCode(secret, auth.TOTPStep(h.clk.Now()))})
	expect(t, r, 200)
	r.json(t, &me)
	anon.csrf = me.CSRFToken
	r = anon.do("POST", "/auth/mfa/recovery-codes", nil)
	expect(t, r, 200)
	var codes struct {
		RecoveryCodes []string `json:"recovery_codes"`
	}
	r.json(t, &codes)
	if len(codes.RecoveryCodes) != 10 {
		t.Fatalf("recovery codes %v", codes.RecoveryCodes)
	}

	// Break-glass recovery login (as `spaceman admin recover` issues it).
	var bg api.BreakGlassStatus
	root.do("GET", "/admin/break-glass", nil).json(t, &bg)
	if bg.Alert {
		t.Fatal("alert before any break-glass use")
	}
	rec, _, err := h.c.Auth.IssueRecoveryLogin(ctx, u.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	fresh := h.client()
	if p := fresh.do("POST", "/auth/recover", map[string]string{"token": "not-a-token"}).problem(t); p.Code != "unauthorized" {
		t.Fatalf("bad recovery token: %+v", p)
	}
	r = fresh.do("POST", "/auth/recover", map[string]string{"token": rec})
	expect(t, r, 200)
	var out api.RecoveryOut
	r.json(t, &out)
	if !out.MFAReset || out.PasswordReset == nil || out.PasswordReset.Token == "" || out.Me.User == nil || out.Me.User.ID != u.ID {
		t.Fatalf("recovery result %+v", out)
	}
	if r.header.Get("Set-Cookie") == "" {
		t.Fatal("recovery login set no session cookie")
	}
	// The user's earlier session ended.
	if p := anon.do("GET", "/auth/sessions", nil).problem(t); p.Code != "unauthorized" {
		t.Fatalf("pre-recovery session still valid: %+v", p)
	}
	root.do("GET", "/admin/break-glass", nil).json(t, &bg)
	if !bg.Alert || bg.At == nil {
		t.Fatalf("no alert after break-glass use: %+v", bg)
	}
	expect(t, root.do("POST", "/admin/break-glass/acknowledge", nil), 204)
	root.do("GET", "/admin/break-glass", nil).json(t, &bg)
	if bg.Alert {
		t.Fatal("alert still raised after acknowledgement")
	}
}
