package app_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/api"
	"github.com/aurimasniekis/go-spaceman/internal/oidc"
	"github.com/aurimasniekis/go-spaceman/internal/oidc/oidctest"
	"github.com/aurimasniekis/go-spaceman/internal/store"
)

// SM-AUTH-001 (ADR-0045, ported from Hallpass bdd8329) over the real HTTP stack: SSO-only enforcement with its
// precondition and guards, the direct provider on the public system info, and RP-initiated logout of an SSO session.
func TestSignInSettingsOverHTTP(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	idp := oidctest.New(t)
	idp.Now = h.clk.Now
	idp.RedirectURI = h.srv.URL + oidc.CallbackPath
	idp.User.Subject = "u_01adasubject"

	rootSecret, _, err := h.c.Auth.IssueRootToken(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	root := h.client()
	root.bearer = rootSecret
	r := root.do("POST", "/users", map[string]any{"email": "ada@example.com", "display_name": "Ada", "password": "correct horse battery staple",
		"grants": []map[string]string{{"role": "admin", "domain": "*"}}})
	expect(t, r, 201)
	var ada struct {
		ID string `json:"id"`
	}
	r.json(t, &ada)
	r = root.do("POST", "/oidc/providers", map[string]any{"slug": "porch", "display_name": "Porch", "issuer": idp.Issuer(),
		"client_id": idp.ClientID, "client_secret": idp.ClientSecret, "ca_bundle_pem": idp.CAPEM(), "allow_private_network": true,
		"end_session_enabled": true, "enabled": true})
	expect(t, r, 201)
	var prov oidc.Provider
	r.json(t, &prov)
	if !prov.EndSessionEnabled {
		t.Fatalf("end_session_enabled not stored: %s", r.body)
	}

	settings := func(r resp) oidc.SignInSettings {
		t.Helper()
		expect(t, r, 200)
		var s oidc.SignInSettings
		r.json(t, &s)
		return s
	}
	if s := settings(root.do("GET", "/oidc/settings", nil)); s.LocalLoginDisabled || s.SSOOnlyReady {
		t.Fatalf("initial %+v", s)
	}
	// Not ready: no tested provider and no SSO administrator.
	if p := root.do("PATCH", "/oidc/settings", map[string]any{"local_login_disabled": true}).problem(t); p.Code != "precondition_failed" {
		t.Fatalf("SSO-only without the precondition: %+v", p)
	}
	// The browser test login is covered in internal/oidc (TestTestLogin); here its evidence is recorded directly.
	if err := h.c.Store.W().Tx(context.Background(), func(tx *store.Tx) error {
		_, err := tx.SQL.ExecContext(context.Background(), `UPDATE oidc_providers SET test_login_ok_at = 1, test_login_by = ? WHERE id = ?`, ada.ID, prov.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	expect(t, root.do("POST", "/users/"+ada.ID+"/identities", map[string]any{"provider_id": prov.ID, "subject": "u_01adasubject"}), 204)
	s := settings(root.do("PATCH", "/oidc/settings", map[string]any{"local_login_disabled": true, "direct_provider": "porch"}))
	if !s.LocalLoginDisabled || s.DirectProvider != "porch" || s.SSOAdmins != 1 {
		t.Fatalf("settings %+v", s)
	}

	// The public system info tells the sign-in page; password sign-in is refused.
	anon := h.client()
	r = anon.do("GET", "/system/info", nil)
	expect(t, r, 200)
	var info api.SystemInfo
	r.json(t, &info)
	if !info.LocalLoginDisabled || info.DirectProvider != "porch" {
		t.Fatalf("system info %+v", info)
	}
	if p := anon.do("POST", "/auth/login", map[string]string{"email": "ada@example.com", "password": "correct horse battery staple"}).problem(t); p.Code != "precondition_failed" {
		t.Fatalf("password sign-in under SSO-only: %+v", p)
	}
	// The guards: the only tested provider cannot be disabled while password sign-in is off.
	etag := root.do("GET", "/oidc/providers/"+prov.ID, nil).header.Get("ETag")
	if p := root.do("PATCH", "/oidc/providers/"+prov.ID, map[string]any{"enabled": false}, "If-Match", etag).problem(t); p.Code != "precondition_failed" {
		t.Fatalf("disabling the only tested provider: %+v", p)
	}

	// SSO sign-in, then sign-out: the response names the provider's RP-initiated logout URL.
	browser := h.client()
	browser.hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	r = browser.do("GET", "/auth/oidc/porch/start", nil)
	expect(t, r, http.StatusFound)
	q, err := idp.Authorize(r.header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	expect(t, browser.do("GET", "/auth/oidc/callback?"+q.Encode(), nil), http.StatusSeeOther)
	r = browser.do("GET", "/auth/me", nil)
	expect(t, r, 200)
	var me api.Me
	r.json(t, &me)
	if me.Method != "oidc" || me.User == nil || me.User.ID != ada.ID {
		t.Fatalf("SSO me %+v", me)
	}
	browser.csrf = me.CSRFToken
	r = browser.do("POST", "/auth/logout", nil)
	expect(t, r, 200)
	var out api.LogoutResult
	r.json(t, &out)
	if out.ProviderLogoutURL == nil {
		t.Fatalf("no provider logout URL: %s", r.body)
	}
	u, err := url.Parse(*out.ProviderLogoutURL)
	if err != nil || !strings.HasPrefix(*out.ProviderLogoutURL, idp.Issuer()+"/logout?") || u.Query().Get("client_id") != idp.ClientID ||
		u.Query().Get("post_logout_redirect_uri") != h.srv.URL+"/login" || u.Query().Has("id_token_hint") {
		t.Fatalf("provider logout URL %q", *out.ProviderLogoutURL)
	}
	expect(t, browser.do("GET", "/auth/me", nil), 401)

	// Turning password sign-in back on needs no precondition; then a password session's sign-out names no provider.
	if s := settings(root.do("PATCH", "/oidc/settings", map[string]any{"local_login_disabled": false})); s.LocalLoginDisabled {
		t.Fatalf("still SSO-only: %+v", s)
	}
	pw := h.client()
	r = pw.do("POST", "/auth/login", map[string]string{"email": "ada@example.com", "password": "correct horse battery staple"})
	expect(t, r, 200)
	var pme api.Me
	r.json(t, &pme)
	pw.csrf = pme.CSRFToken
	r = pw.do("POST", "/auth/logout", nil)
	expect(t, r, 200)
	if !strings.Contains(string(r.body), `"provider_logout_url":null`) {
		t.Fatalf("password sign-out named a provider: %s", r.body)
	}
}
