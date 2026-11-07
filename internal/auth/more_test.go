package auth

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
)

func TestOptionalMFAAndCodes(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	admin := f.bootstrap()
	actor := audit.Actor{Type: audit.ActorUser, ID: admin.ID}
	_, err := f.svc.Create(f.ctx, actor, CreateInput{Email: "aud@example.com", DisplayName: "Aud", Grants: grants(RoleAuditor), Password: strongPW})
	must(t, err)
	res, err := f.svc.Login(f.ctx, LoginInput{Email: "aud@example.com", Password: strongPW})
	must(t, err)
	if res.MFAPending || res.MustEnroll || res.Principal.Limited {
		t.Fatalf("auditor without MFA policy should not be limited: %+v", res)
	}
	if _, err := f.svc.RegenerateRecoveryCodes(f.ctx, res.Principal); err == nil {
		t.Fatal("regenerated codes without MFA")
	}
	enr, err := f.svc.BeginTOTP(f.ctx, res.Principal)
	must(t, err)
	if enr.Secret == "" || enr.URI == "" {
		t.Fatal("enrollment data missing")
	}
	if _, _, err := f.svc.ConfirmTOTP(f.ctx, res.Principal, "000000", SessionMeta{}); err == nil {
		t.Fatal("wrong confirmation code accepted")
	}
	_, rotated, err := f.svc.ConfirmTOTP(f.ctx, res.Principal, f.pendingCode(res.Principal.User.ID), SessionMeta{})
	must(t, err)
	if _, err := f.svc.Authenticate(f.ctx, res.Token); err == nil {
		t.Fatal("full session not rotated on enrollment")
	}
	p, err := f.svc.Authenticate(f.ctx, rotated)
	must(t, err)
	codes, err := f.svc.RegenerateRecoveryCodes(f.ctx, p)
	if err != nil || len(codes) != RecoveryCodeCount {
		t.Fatal(err)
	}
	must(t, f.svc.DisableTOTP(f.ctx, p))
	op, _ := f.svc.Get(f.ctx, p.User.ID)
	if op.MFAEnrolled {
		t.Fatal("TOTP not removed")
	}
}

func TestMFAThrottleAndSessionRevoke(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	f.bootstrap()
	f.loginEnrolled("admin@example.com")
	res, err := f.svc.Login(f.ctx, LoginInput{Email: "admin@example.com", Password: strongPW})
	must(t, err)
	for range 4 {
		_, _ = f.svc.VerifyMFA(f.ctx, res.Principal, "111111", SessionMeta{})
	}
	if _, err := f.svc.VerifyMFA(f.ctx, res.Principal, f.totpCode(res.Principal.User.ID), SessionMeta{}); !errors.Is(err, ErrThrottled) {
		t.Fatalf("MFA guessing not throttled: %v", err)
	}
	f.clk.Advance(20 * time.Minute)
	tok, p := f.loginEnrolled("admin@example.com")
	must(t, f.svc.RevokeSession(f.ctx, p.User.ID, p.SessionID, "user"))
	if _, err := f.svc.Authenticate(f.ctx, tok); err == nil {
		t.Fatal("revoked session valid")
	}
	if err := f.svc.RevokeSession(f.ctx, p.User.ID, "wss_missing", "x"); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := f.svc.Authenticate(f.ctx, ""); err == nil {
		t.Fatal("empty token")
	}
	if _, err := f.svc.Get(f.ctx, "usr_missing"); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := f.svc.Reauth(f.ctx, p, "wrong password!!", "", SessionMeta{}); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatal(err)
	}
	if err := f.svc.ChangePassword(f.ctx, p, "wrong password!!", "another long password"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatal(err)
	}
	actor := audit.Actor{Type: audit.ActorUser, ID: p.User.ID}
	if _, _, err := f.svc.Invite(f.ctx, actor, "admin@example.com", nil); !errors.Is(err, apperr.ErrAlreadyExists) {
		t.Fatal("invited an existing email")
	}
	if _, _, err := f.svc.IssueResetToken(f.ctx, actor, "usr_missing"); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatal(err)
	}
	other, err := f.svc.Create(f.ctx, actor, CreateInput{Email: "o@example.com", DisplayName: "O", Grants: grants(RoleViewer)})
	must(t, err)
	_, err = f.svc.SetEnabled(f.ctx, actor, other.ID, false)
	must(t, err)
	if op, err := f.svc.SetEnabled(f.ctx, actor, other.ID, true); err != nil || !op.Enabled {
		t.Fatal(err)
	}
}

// Security review SR-M01: parallel wrong second-factor codes cannot outrun the throttle. Attempts on one factor are
// serialized, so after the free attempts every further code in the burst is refused before it is checked.
func TestMFAThrottleHoldsUnderConcurrency(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	f.bootstrap()
	f.loginEnrolled("admin@example.com")
	res, err := f.svc.Login(f.ctx, LoginInput{Email: "admin@example.com", Password: strongPW})
	must(t, err)
	const burst = 24
	var wg sync.WaitGroup
	var mu sync.Mutex
	throttled, wrong := 0, 0
	for range burst {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.svc.VerifyMFA(f.ctx, res.Principal, "111111", SessionMeta{})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case errors.Is(err, ErrThrottled):
				throttled++
			case err != nil:
				wrong++
			}
		}()
	}
	wg.Wait()
	// throttleFree failures are free, the next one sets the delay; the fake clock does not move, so the rest wait.
	if wrong > throttleFree+1 || throttled != burst-wrong {
		t.Fatalf("%d codes were checked and %d throttled out of %d parallel attempts; at most %d may be checked",
			wrong, throttled, burst, throttleFree+1)
	}
}

// The same holds for parallel password guesses against one account from many addresses.
func TestLoginThrottleHoldsUnderConcurrency(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	f.bootstrap()
	const burst = 16
	var wg sync.WaitGroup
	var mu sync.Mutex
	checked := 0
	for i := range burst {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.svc.Login(f.ctx, LoginInput{Email: "admin@example.com", Password: "wrong password!!", Meta: SessionMeta{IP: "198.51.100." + itoa(i+1)}})
			if !errors.Is(err, ErrThrottled) {
				mu.Lock()
				checked++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if checked > throttleFree+1 {
		t.Fatalf("%d of %d parallel guesses were checked; at most %d may be", checked, burst, throttleFree+1)
	}
}
