package oidc

import (
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/oidc/oidctest"
)

// Hallpass HP-AUTH-015 / HP-OIDC-014 / SR-06: SSO-only enforcement needs a tested provider and an administrator who
// can sign in with SSO; while it is on, password sign-in is refused, break-glass still works, and changes that would
// lock the administrators out are refused.
func TestSSOOnlyEnforcement(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	off := func(v bool) SignInSettingsInput { return SignInSettingsInput{LocalLoginDisabled: ptr(v)} }
	st, err := f.svc.SignInSettings(f.ctx)
	must(t, err)
	if st.LocalLoginDisabled || st.SSOOnlyReady || st.TestedProviders != 0 || st.SSOAdmins != 0 {
		t.Fatalf("initial settings %+v", st)
	}
	if _, err := f.svc.SetSignInSettings(f.ctx, admin, off(true)); !errors.Is(err, ErrSSOOnlyPrecondition) {
		t.Fatalf("SSO-only without a tested provider: %v", err)
	}
	rootP := f.localSession("root@corp.example")
	mustFlow(t, f, PurposeTest, rootP)
	if _, err := f.svc.SetSignInSettings(f.ctx, admin, off(true)); !errors.Is(err, ErrSSOOnlyPrecondition) {
		t.Fatalf("SSO-only without an SSO administrator: %v", err)
	}
	// An administrator who is only SSO-mapped does not count (SSO grants are re-derived at every sign-in, ADR-0029).
	f.mapGroup("admins", authz.RoleAdmin, authz.Global)
	other := f.user("ops@corp.example", authz.RoleOperator)
	must(t, f.svc.LinkIdentity(f.ctx, admin, other.ID, f.prov.ID, "user-9"))
	if _, err := f.svc.SetSignInSettings(f.ctx, admin, off(true)); !errors.Is(err, ErrSSOOnlyPrecondition) {
		t.Fatalf("SSO-only with only a non-admin linked user: %v", err)
	}
	must(t, f.svc.LinkIdentity(f.ctx, admin, rootP.User.ID, f.prov.ID, "user-1"))
	st, err = f.svc.SetSignInSettings(f.ctx, admin, off(true))
	must(t, err)
	if !st.LocalLoginDisabled || !st.SSOOnlyReady || st.SSOAdmins != 1 || st.TestedProviders != 1 {
		t.Fatalf("settings %+v", st)
	}
	if on, err := f.auth.LocalLoginDisabled(f.ctx); err != nil || !on {
		t.Fatalf("auth does not see SSO-only: %v %v", on, err)
	}

	// Password sign-in is refused; SSO sign-in works.
	if _, err := f.auth.Login(f.ctx, auth.LoginInput{Email: "root@corp.example", Password: password, Meta: f.meta}); !errors.Is(err, auth.ErrLocalLoginDisabled) {
		t.Fatalf("password sign-in under SSO-only: %v", err)
	}
	if res := mustFlow(t, f, PurposeLogin, nil); res.UserID != rootP.User.ID {
		t.Fatalf("SSO sign-in %+v", res)
	}
	// Break-glass recovery is unaffected.
	tok, _, err := f.auth.IssueRecoveryLogin(f.ctx, rootP.User.ID, "")
	must(t, err)
	if r, err := f.auth.RecoveryLogin(f.ctx, tok, f.meta); err != nil || r.Token == "" {
		t.Fatalf("break-glass under SSO-only: %v", err)
	}

	// Guards: nothing may remove the last SSO administrator path while password sign-in is off.
	second := f.user("second@corp.example", authz.RoleAdmin) // a password-only administrator: the last-admin guard is not the one tripping
	if _, err := f.auth.SetGrants(f.ctx, admin, rootP.User.ID, []auth.GrantInput{{Role: authz.RoleViewer, Domain: authz.Global}}); !errors.Is(err, auth.ErrLastSSOAdmin) {
		t.Fatalf("demoting the last SSO administrator: %v", err)
	}
	if _, err := f.auth.SetEnabled(f.ctx, admin, rootP.User.ID, false); !errors.Is(err, auth.ErrLastSSOAdmin) {
		t.Fatalf("disabling the last SSO administrator: %v", err)
	}
	if err := f.svc.UnlinkIdentity(f.ctx, admin, rootP.User.ID, f.prov.Issuer, "user-1", false); !isCode(err, apperr.CodePrecondition) {
		t.Fatalf("unlinking the last SSO administrator: %v", err)
	}
	p, _ := f.svc.GetProvider(f.ctx, f.prov.ID)
	if _, err := f.svc.UpdateProvider(f.ctx, admin, p.ID, ProviderInput{Version: p.Version, Enabled: ptr(false)}); !isCode(err, apperr.CodePrecondition) {
		t.Fatalf("disabling the only tested provider: %v", err)
	}
	if _, err := f.svc.UpdateProvider(f.ctx, admin, p.ID, ProviderInput{Version: p.Version, ClientID: ptr("other")}); !isCode(err, apperr.CodePrecondition) {
		t.Fatalf("clearing the only test-login evidence: %v", err)
	}
	if err := f.svc.DeleteProvider(f.ctx, admin, p.ID); !isCode(err, apperr.CodePrecondition) {
		t.Fatalf("deleting the only tested provider: %v", err)
	}
	// Unrelated changes still work.
	if _, err := f.svc.UpdateProvider(f.ctx, admin, p.ID, ProviderInput{Version: p.Version, DisplayName: ptr("Corp")}); err != nil {
		t.Fatalf("an unrelated provider change: %v", err)
	}
	if _, err := f.auth.SetEnabled(f.ctx, admin, second.ID, false); err != nil {
		t.Fatalf("disabling a password-only administrator: %v", err)
	}

	// Turning password sign-in back on needs no precondition and lifts the guards.
	st, err = f.svc.SetSignInSettings(f.ctx, admin, off(false))
	must(t, err)
	if st.LocalLoginDisabled {
		t.Fatal("still SSO-only")
	}
	if _, err := f.auth.Login(f.ctx, auth.LoginInput{Email: "ops@corp.example", Password: password, Meta: f.meta}); err != nil {
		t.Fatalf("password sign-in after turning it back on: %v", err)
	}
	must(t, f.svc.UnlinkIdentity(f.ctx, admin, rootP.User.ID, f.prov.Issuer, "user-1", false))
}

// Hallpass: the sign-in page can go straight to one enabled provider; a disabled or deleted one turns it off.
func TestDirectProvider(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	set := func(slug string) error {
		_, err := f.svc.SetSignInSettings(f.ctx, admin, SignInSettingsInput{DirectProvider: ptr(slug)})
		return err
	}
	if err := set("missing"); !isCode(err, apperr.CodeInvalid) {
		t.Fatalf("unknown provider: %v", err)
	}
	must(t, set("corp"))
	if s, err := DirectProvider(f.ctx, f.s.R()); err != nil || s != "corp" {
		t.Fatalf("direct provider %q %v", s, err)
	}
	p, err := f.svc.UpdateProvider(f.ctx, admin, f.prov.ID, ProviderInput{Version: f.prov.Version, Enabled: ptr(false)})
	must(t, err)
	if st, _ := f.svc.SignInSettings(f.ctx); st.DirectProvider != "" {
		t.Fatalf("disabled provider still direct: %+v", st)
	}
	if err := set("corp"); !isCode(err, apperr.CodeInvalid) {
		t.Fatalf("direct sign-in through a disabled provider: %v", err)
	}
	_, err = f.svc.UpdateProvider(f.ctx, admin, p.ID, ProviderInput{Version: p.Version, Enabled: ptr(true)})
	must(t, err)
	if s, _ := DirectProvider(f.ctx, f.s.R()); s != "corp" {
		t.Fatalf("re-enabled provider: %q", s)
	}
	must(t, set(""))
	if s, _ := DirectProvider(f.ctx, f.s.R()); s != "" {
		t.Fatalf("turned off: %q", s)
	}
}

// Hallpass HP-OIDC-015: RP-initiated logout only when the provider enables it and advertises end_session_endpoint;
// the endpoint is validated like the others, no id_token_hint is sent and the redirect comes back to /login.
func TestLogoutURL(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	if u, err := f.svc.LogoutURL(f.ctx, "corp", "/login"); err != nil || u != "" {
		t.Fatalf("not enabled: %q %v", u, err)
	}
	if u, err := f.svc.LogoutURL(f.ctx, "missing", "/login"); err != nil || u != "" {
		t.Fatalf("unknown provider: %q %v", u, err)
	}
	p, err := f.svc.UpdateProvider(f.ctx, admin, f.prov.ID, ProviderInput{Version: f.prov.Version, EndSessionEnabled: ptr(true)})
	must(t, err)
	if !p.EndSessionEnabled {
		t.Fatal("end_session_enabled not stored")
	}
	raw, err := f.svc.LogoutURL(f.ctx, "corp", "/login")
	must(t, err)
	u, err := url.Parse(raw)
	must(t, err)
	q := u.Query()
	if !strings.HasPrefix(raw, f.idp.Issuer()+"/logout?") || q.Get("client_id") != f.idp.ClientID ||
		q.Get("post_logout_redirect_uri") != "https://spaceman.example/login" || q.Has("id_token_hint") {
		t.Fatalf("logout URL %s", raw)
	}
	if raw, _ := f.svc.LogoutURL(f.ctx, "corp", "https://evil.example/"); !strings.Contains(raw, url.QueryEscape("https://spaceman.example/")) {
		t.Fatalf("post-logout redirect not sanitized: %s", raw)
	}
	// A provider that advertises none: signing out ends only the SpaceMan session.
	f.idp.Set(func(i *oidctest.IdP) { i.EndSession = "-" })
	p, err = f.svc.UpdateProvider(f.ctx, admin, p.ID, ProviderInput{Version: p.Version, DisplayName: ptr("Corp")})
	must(t, err)
	if raw, err := f.svc.LogoutURL(f.ctx, "corp", "/login"); err != nil || raw != "" {
		t.Fatalf("no end_session_endpoint: %q %v", raw, err)
	}
	// An end_session_endpoint off the issuer's domain is rejected like any other endpoint (Hallpass HP-OIDC-006).
	f.idp.Set(func(i *oidctest.IdP) { i.EndSession = "https://evil.example/logout" })
	p, err = f.svc.UpdateProvider(f.ctx, admin, p.ID, ProviderInput{Version: p.Version, DisplayName: ptr("Corp SSO")})
	must(t, err)
	if raw, err := f.svc.LogoutURL(f.ctx, "corp", "/login"); err == nil || raw != "" {
		t.Fatalf("cross-origin end_session_endpoint accepted: %q", raw)
	}
	// Disabled again: no provider logout.
	f.idp.Set(func(i *oidctest.IdP) { i.EndSession = "" })
	_, err = f.svc.UpdateProvider(f.ctx, admin, p.ID, ProviderInput{Version: p.Version, EndSessionEnabled: ptr(false)})
	must(t, err)
	if raw, err := f.svc.LogoutURL(f.ctx, "corp", "/login"); err != nil || raw != "" {
		t.Fatalf("disabled: %q %v", raw, err)
	}
}
