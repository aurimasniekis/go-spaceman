package oidc

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/oidc/oidctest"
)

// Regression tests ported from go-hallpass internal/oidc/review_test.go (its 2026-10-24 internal security review).
// Hallpass SR-06 (SSO-only enforcement) is not ported: SpaceMan has no SSO-only switch yet (docs/LIMITATIONS.md).

func (f *fx) ssoGrants(userID string) []string {
	f.t.Helper()
	u, err := f.auth.Get(f.ctx, userID)
	must(f.t, err)
	out := []string{}
	for _, g := range u.SSOGrants {
		out = append(out, string(g.Role)+"@"+g.Domain)
	}
	return out
}

// SR-10: self-service account linking needs recent authentication.
func TestReviewSR10SelfLinkNeedsRecentAuth(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	u := f.user("a@example.com")
	p := f.localSession(u.Email)
	f.clk.Advance(10 * time.Minute)
	if _, err := f.svc.Start(f.ctx, StartInput{Provider: "corp", Purpose: PurposeLink, Principal: p}); !errors.Is(err, auth.ErrReauthRequired) {
		t.Fatalf("stale session started self-link: %v", err)
	}
	p = f.localSession(u.Email)
	if _, err := f.flow(PurposeLink, p); err != nil {
		t.Fatalf("fresh self-link: %v", err)
	}
	if ids, _ := f.svc.Identities(f.ctx, u.ID); len(ids) != 1 || ids[0].LinkedVia != "self_link" {
		t.Fatalf("%+v", ids)
	}
}

// SR-48: an MFA-pending (limited) administrator session cannot run the SSO test login.
func TestReviewSR48TestLoginNeedsFullSession(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	res, err := f.auth.Login(f.ctx, auth.LoginInput{Email: "root@corp.example", Password: password, Meta: f.meta})
	must(t, err)
	if !res.Principal.Limited {
		t.Fatal("administrator password session is not limited before MFA")
	}
	if _, err := f.svc.Start(f.ctx, StartInput{Provider: "corp", Purpose: PurposeTest, Principal: res.Principal}); !isCode(err, apperr.CodeForbidden) {
		t.Fatalf("limited session started a test login: %v", err)
	}
	if _, err := f.flow(PurposeTest, f.localSession("root@corp.example")); err != nil {
		t.Fatalf("full admin session: %v", err)
	}
}

// SR-03 (b): the TOTP step-up fallback needs an SSO session whose provider allows it.
func TestReviewSR03TOTPFallbackNeedsProviderOptIn(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	u := f.user("alice@example.com", authz.RoleOperator)
	pw := f.localSession(u.Email) // enrolls TOTP (operator grants require it)
	must(t, f.svc.LinkIdentity(f.ctx, admin, u.ID, f.prov.ID, "user-1"))
	p := f.principal(mustToken(t, mustFlow(t, f, PurposeLogin, nil)))
	f.clk.Advance(30 * time.Second)
	if _, err := f.svc.ReauthTOTP(f.ctx, p, f.totpCode(u.ID), f.meta); !errors.Is(err, auth.ErrTOTPStepUpNotAllowed) {
		t.Fatalf("TOTP step-up with stepup_fallback=none: %v", err)
	}
	if _, err := f.svc.ReauthTOTP(f.ctx, pw, f.totpCode(u.ID), f.meta); !errors.Is(err, auth.ErrTOTPStepUpNotAllowed) {
		t.Fatalf("TOTP step-up on a password session: %v", err)
	}
	pr, err := f.svc.UpdateProvider(f.ctx, admin, f.prov.ID, ProviderInput{Version: f.prov.Version, StepUpFallback: ptr(StepUpLocalTOTP)})
	must(t, err)
	f.prov = pr
	if _, err := f.svc.ReauthTOTP(f.ctx, p, f.totpCode(u.ID), f.meta); err != nil {
		t.Fatalf("TOTP step-up with local_totp: %v", err)
	}
	p = f.principal(mustToken(t, mustFlow(t, f, PurposeLogin, nil)))
	must(t, f.svc.DeleteProvider(f.ctx, admin, f.prov.ID))
	if _, err := f.svc.ReauthTOTP(f.ctx, p, f.totpCode(u.ID), f.meta); !errors.Is(err, auth.ErrTOTPStepUpNotAllowed) {
		t.Fatalf("TOTP step-up after the provider was deleted: %v", err)
	}
}

// SR-26 (SpaceMan K-11): SSO-sourced grants apply only to SSO sessions. The same user's password session and API
// tokens act with the manual grants alone.
func TestReviewSR26SSOGrantsOnlyForSSOSessions(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	u := f.user("v@example.com", authz.RoleViewer)
	must(t, f.svc.LinkIdentity(f.ctx, admin, u.ID, f.prov.ID, "user-1"))
	f.mapGroup("operators", authz.RoleOperator, authz.Global)
	f.idp.Set(func(i *oidctest.IdP) { i.User.Claims["groups"] = []string{"operators"} })
	sso := f.principal(mustToken(t, mustFlow(t, f, PurposeLogin, nil)))
	if !f.can(sso, authz.Global, authz.ApprovalDecide) || !f.can(sso, authz.Global, authz.SpaceRead) {
		t.Fatalf("SSO session lacks its grants: %v", sso.Subjects())
	}
	pw := f.localSession(u.Email)
	if f.can(pw, authz.Global, authz.ApprovalDecide) || !f.can(pw, authz.Global, authz.SpaceRead) {
		t.Fatalf("password session got the SSO grant: %v", pw.Subjects())
	}
	if slices.Contains(pw.Subjects(), auth.SSOSubject(u.ID)) || !slices.Equal(sso.Subjects(), []string{auth.SSOSubject(u.ID)}) {
		t.Fatalf("subjects: password %v, sso %v", pw.Subjects(), sso.Subjects())
	}
	if got := f.az.Domains(sso.Subjects()[0]); !slices.Equal(got, []string{authz.Global}) {
		t.Fatalf("domains %v", got)
	}
	if got := f.az.RolesIn(sso.Subjects()[0], authz.Global); !slices.Equal(got, []authz.Role{authz.RoleOperator, authz.RoleViewer}) {
		t.Fatalf("roles %v", got)
	}
}

// SR-26: deleting a provider removes the SSO-sourced grants of the users linked to it; manual grants stay.
func TestReviewSR26DeleteProviderDropsSSOGrants(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	u := f.user("alice@example.com", authz.RoleViewer)
	must(t, f.svc.LinkIdentity(f.ctx, admin, u.ID, f.prov.ID, "user-1"))
	f.mapGroup("operators", authz.RoleOperator, authz.Global)
	f.idp.Set(func(i *oidctest.IdP) { i.User.Claims["groups"] = []string{"operators"} })
	sso := f.principal(mustToken(t, mustFlow(t, f, PurposeLogin, nil)))
	if got := f.ssoGrants(u.ID); !slices.Equal(got, []string{"operator@*"}) {
		t.Fatalf("mapping not applied: %v", got)
	}
	must(t, f.svc.DeleteProvider(f.ctx, admin, f.prov.ID))
	if got := f.ssoGrants(u.ID); len(got) != 0 {
		t.Fatalf("SSO grants survived provider deletion: %v", got)
	}
	if f.can(sso, authz.Global, authz.ApprovalDecide) || !f.can(sso, authz.Global, authz.SpaceRead) {
		t.Fatal("authorization not reloaded after the provider was deleted")
	}
	if n, _ := f.s.R().CountEnabledGlobalAdmins(f.ctx); n != 1 {
		t.Fatal("the manual administrator was affected")
	}
}

// SR-24: fragments that redirect path-cleaning would turn into /api or /auth are refused.
func TestReviewSR24ReturnToFragment(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"/#/../api/v1/me", "/x#/../../auth/oidc/corp/start?purpose=link", "/x#y/../../api", "/a//b"} {
		if got := SanitizeReturnTo(in); got != "/" {
			t.Errorf("%q accepted", in)
		}
	}
	for _, in := range []string{"/spaces/spc_1#t=3", "/spaces/", "/x?y=/../api"} {
		if got := SanitizeReturnTo(in); got != in {
			t.Errorf("%q rewritten to %q", in, got)
		}
	}
}
