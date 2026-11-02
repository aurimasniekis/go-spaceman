package app_test

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/api"
	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/oidc"
	"github.com/aurimasniekis/go-spaceman/internal/oidc/oidctest"
	"github.com/aurimasniekis/go-spaceman/internal/store"
)

// seedSpace inserts a node and a space directly (the space only names a mapping's domain here).
func seedSpace(t *testing.T, h *harness) string {
	t.Helper()
	id := ids.New(ids.Space)
	err := h.c.Store.W().Tx(context.Background(), func(tx *store.Tx) error {
		if _, err := tx.SQL.ExecContext(context.Background(), `INSERT INTO nodes (id, name, state, created_at, created_by, updated_at)
			VALUES ('nod_oidc', 'oidc-node', 'active', 0, 't', 0)`); err != nil {
			return err
		}
		_, err := tx.SQL.ExecContext(context.Background(), `INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at)
			VALUES (?, 'sso-space', 'SSO space', 'nod_oidc', 'active', 'w', 0, 't', 0)`, id)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// SM-AUTH-001/002 (ADR-0029): OIDC sign-in through the real HTTP stack against an in-process fake provider that
// behaves like Porch (porch.groups, RFC 9207 iss, PKCE S256 only). A group mapping grants a space role to SSO sessions
// only; the same user's password session does not get it (Hallpass SR-26, K-11).
func TestOIDCSignInOverHTTP(t *testing.T) {
	h := newHarness(t)
	idp := oidctest.New(t)
	idp.Now = h.clk.Now
	idp.RedirectURI = h.srv.URL + oidc.CallbackPath
	idp.User.Subject = "u_01porchsubject"
	idp.User.Claims["porch"] = map[string]any{"v": 1, "tenant": map[string]any{"id": "t_1", "slug": "acme"}, "roles": []string{},
		"groups": []string{"spaceman-operators"}}
	idp.ResponseIssuer = idp.Issuer()

	rootSecret, _, err := h.c.Auth.IssueRootToken(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	root := h.client()
	root.bearer = rootSecret
	expect(t, root.do("POST", "/users", map[string]any{"email": "ada@example.com", "display_name": "Ada",
		"password": "correct horse battery staple", "grants": []map[string]string{{"role": "admin", "domain": "*"}}}), 201)
	r := root.do("POST", "/users", map[string]any{"email": "vic@example.com", "display_name": "Vic", "password": "correct horse battery staple",
		"grants": []map[string]string{{"role": "viewer", "domain": "*"}}})
	expect(t, r, 201)
	var vic struct {
		ID string `json:"id"`
	}
	r.json(t, &vic)

	r = root.do("POST", "/oidc/providers", map[string]any{"slug": "porch", "display_name": "Porch", "issuer": idp.Issuer(),
		"client_id": idp.ClientID, "client_secret": idp.ClientSecret, "scopes": []string{"openid", "profile", "email", "groups"},
		"groups_claim": "porch.groups", "ca_bundle_pem": idp.CAPEM(), "allow_private_network": true, "enabled": true})
	expect(t, r, 201)
	var prov oidc.Provider
	r.json(t, &prov)
	if !prov.HasClientSecret || strings.Contains(string(r.body), idp.ClientSecret) {
		t.Fatalf("client secret returned: %s", r.body)
	}
	r = root.do("GET", "/oidc/providers", nil)
	expect(t, r, 200)
	var list api.ProviderList
	r.json(t, &list)
	if list.RedirectURI != h.srv.URL+oidc.CallbackPath || len(list.Items) != 1 {
		t.Fatalf("providers %+v", list)
	}
	spaceID := seedSpace(t, h)
	expect(t, root.do("POST", "/oidc/providers/"+prov.ID+"/mappings", map[string]any{"group_value": "spaceman-operators", "role": "operator",
		"domain": spaceID}), 201)
	if p := root.do("POST", "/oidc/providers/"+prov.ID+"/mappings", map[string]any{"group_value": "x", "role": "operator",
		"domain": "spc_00000000000000000000000000"}).problem(t); p.Status != 422 {
		t.Fatalf("mapping to a missing space: %+v", p)
	}
	expect(t, root.do("POST", "/users/"+vic.ID+"/identities", map[string]any{"provider_id": prov.ID, "subject": "u_01porchsubject"}), 204)

	// The sign-in page lists the enabled provider without configuration details.
	anon := h.client()
	r = anon.do("GET", "/auth/oidc/providers", nil)
	expect(t, r, 200)
	if strings.Contains(string(r.body), "issuer") || !strings.Contains(string(r.body), `"slug":"porch"`) {
		t.Fatalf("public providers %s", r.body)
	}

	// Browser sign-in: start → provider → callback.
	browser := h.client()
	browser.hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	r = browser.do("GET", "/auth/oidc/porch/start?return_to="+url.QueryEscape("/spaces/"+spaceID), nil)
	expect(t, r, http.StatusFound)
	loc := r.header.Get("Location")
	if !strings.HasPrefix(loc, idp.Issuer()+"/authorize?") || !strings.Contains(strings.Join(r.header.Values("Set-Cookie"), ";"), "sm_oidc=") {
		t.Fatalf("start redirect %q cookies %v", loc, r.header.Values("Set-Cookie"))
	}
	q, err := idp.Authorize(loc)
	if err != nil {
		t.Fatal(err)
	}
	if aq := idp.LastAuthorizeParams(); aq.Get("code_challenge_method") != "S256" || aq.Get("scope") != "openid profile email groups" {
		t.Fatalf("authorize params %v", aq)
	}
	r = browser.do("GET", "/auth/oidc/callback?"+q.Encode(), nil)
	expect(t, r, http.StatusSeeOther)
	if r.header.Get("Location") != "/spaces/"+spaceID {
		t.Fatalf("callback redirect %q", r.header.Get("Location"))
	}
	r = browser.do("GET", "/auth/me", nil)
	expect(t, r, 200)
	var me api.Me
	r.json(t, &me)
	if me.Method != "oidc" || me.Provider != "porch" || me.User == nil || me.User.ID != vic.ID || me.Limited || !slices.Contains(me.Domains, spaceID) {
		t.Fatalf("SSO me %+v", me)
	}
	browser.csrf = me.CSRFToken
	r = browser.do("GET", "/auth/identities", nil)
	expect(t, r, 200)
	if !strings.Contains(string(r.body), `"provider_slug":"porch"`) {
		t.Fatalf("identities %s", r.body)
	}
	// The mapped operator grant works in this SSO session (queue write in the space).
	if !h.c.Authz.AllowedAll([]string{auth.SSOSubject(vic.ID)}, spaceID, authz.QueueWrite) || h.c.Authz.AllowedAll([]string{auth.UserSubject(vic.ID)}, spaceID, authz.QueueWrite) {
		t.Fatal("SSO grant not effective")
	}

	// A replayed callback is refused and sends the browser back with a stable error code.
	r = browser.do("GET", "/auth/oidc/callback?"+q.Encode(), nil)
	expect(t, r, http.StatusSeeOther)
	if r.header.Get("Location") != "/account?sso_error=validation_failed" {
		t.Fatalf("replay redirect %q", r.header.Get("Location"))
	}

	// The same user's password session has only the manual viewer grant (SR-26).
	pw := h.client()
	r = pw.do("POST", "/auth/login", map[string]string{"email": "vic@example.com", "password": "correct horse battery staple"})
	expect(t, r, 200)
	var pme api.Me
	r.json(t, &pme)
	if pme.Method != "password" || slices.Contains(pme.Domains, spaceID) || len(pme.User.SSOGrants) != 1 {
		t.Fatalf("password me %+v", pme)
	}

	// An unknown provider redirects to the sign-in page with an error code, never a problem body.
	anon.hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	r = anon.do("GET", "/auth/oidc/nope/start", nil)
	expect(t, r, http.StatusSeeOther)
	if r.header.Get("Location") != "/login?sso_error=not_found" {
		t.Fatalf("unknown provider redirect %q", r.header.Get("Location"))
	}

	// Self-unlink needs a recent sign-in; it ends every session of the user.
	expect(t, browser.do("DELETE", "/auth/identities?issuer="+url.QueryEscape(idp.Issuer())+"&subject=u_01porchsubject", nil), 204)
	expect(t, browser.do("GET", "/auth/me", nil), 401)
}
