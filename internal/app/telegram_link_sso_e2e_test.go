package app_test

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/api"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/oidc"
	"github.com/aurimasniekis/go-spaceman/internal/oidc/oidctest"
	tglink "github.com/aurimasniekis/go-spaceman/internal/telegram/link"
)

// ssoLinkEnv is a link environment with an OIDC provider and one person, Sam, who holds a local viewer grant and, through
// a group mapping, an SSO-only admin grant in the space. Sam is signed in through the IdP in a real browser session.
type ssoLinkEnv struct {
	*linkEnv
	idp  *oidctest.IdP
	sam  *webUser
	slug string
	me   api.Me
}

// newSSOLinkEnv signs Sam in over HTTP (start, IdP, callback). ssoGrants sets the link_sso_grants policy first.
func newSSOLinkEnv(t *testing.T, ssoGrants bool) *ssoLinkEnv {
	t.Helper()
	e := newLinkEnv(t)
	idp := oidctest.New(t)
	idp.Now = e.h.clk.Now
	idp.RedirectURI = e.h.srv.URL + oidc.CallbackPath
	idp.User.Subject = "u_sam"
	idp.User.Claims["porch"] = map[string]any{"v": 1, "tenant": map[string]any{"id": "t_1", "slug": "acme"}, "roles": []string{},
		"groups": []string{"spaceman-admins"}}
	idp.ResponseIssuer = idp.Issuer()

	if ssoGrants {
		expect(t, e.root.do("PUT", "/telegram/settings", map[string]any{"link_sso_grants": true}), 200)
		if err := e.h.c.TelegramSettings.Load(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	r := e.root.do("POST", "/users", map[string]any{"email": "sam@example.com", "display_name": "Sam", "password": linkPassword,
		"grants": []map[string]string{{"role": "viewer", "domain": "*"}}})
	expect(t, r, 201)
	var u struct {
		ID string `json:"id"`
	}
	r.json(t, &u)
	r = e.root.do("POST", "/oidc/providers", map[string]any{"slug": "porch", "display_name": "Porch", "issuer": idp.Issuer(),
		"client_id": idp.ClientID, "client_secret": idp.ClientSecret, "scopes": []string{"openid", "profile", "email", "groups"},
		"groups_claim": "porch.groups", "ca_bundle_pem": idp.CAPEM(), "allow_private_network": true, "enabled": true})
	expect(t, r, 201)
	var prov oidc.Provider
	r.json(t, &prov)
	expect(t, e.root.do("POST", "/oidc/providers/"+prov.ID+"/mappings", map[string]any{"group_value": "spaceman-admins", "role": "admin",
		"domain": e.space}), 201)
	expect(t, e.root.do("POST", "/users/"+u.ID+"/identities", map[string]any{"provider_id": prov.ID, "subject": "u_sam"}), 204)

	s := &ssoLinkEnv{linkEnv: e, idp: idp, slug: "porch"}
	browser := e.h.client()
	browser.hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	s.sam = &webUser{id: u.ID, ui: browser}
	s.idpRoundTrip(t, "login")
	return s
}

// idpRoundTrip runs start → IdP → callback for the purpose (login or stepup) and refreshes the cookie, /auth/me and the
// CSRF token: for a step-up the session rotates.
func (s *ssoLinkEnv) idpRoundTrip(t *testing.T, purpose string) {
	t.Helper()
	b := s.sam.ui
	r := b.do("GET", "/auth/oidc/"+s.slug+"/start?purpose="+purpose+"&return_to="+url.QueryEscape("/account"), nil)
	expect(t, r, http.StatusFound)
	q, err := s.idp.Authorize(r.header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	r = b.do("GET", "/auth/oidc/callback?"+q.Encode(), nil)
	expect(t, r, http.StatusSeeOther)
	if loc := r.header.Get("Location"); loc != "/account" {
		t.Fatalf("%s callback redirected to %q", purpose, loc)
	}
	s.refreshMe(t)
}

func (s *ssoLinkEnv) refreshMe(t *testing.T) {
	t.Helper()
	r := s.sam.ui.do("GET", "/auth/me", nil)
	expect(t, r, 200)
	r.json(t, &s.me)
	if s.me.Method != "oidc" || s.me.Limited || s.me.User == nil || s.me.User.ID != s.sam.id {
		t.Fatalf("SSO session %+v", s.me)
	}
	s.sam.ui.csrf = s.me.CSRFToken
}

// SM-TG-014 (ADR-0083 decision 3, Q8): a link confirmed from a real OIDC session carries the user's SSO-mapped grants only
// while link_sso_grants is on. On: the link is via_sso, its sso_until is the session's absolute expiry, and the SSO admin
// grant is capped to the link's cap (operator) in the space beside the local viewer grant. Off (the default): no SSO
// flag, no sso_until and nothing from the SSO subject.
func TestTelegramLinkFromSSOSessionHonoursLinkSSOGrants(t *testing.T) {
	t.Parallel()
	for _, on := range []bool{true, false} {
		t.Run(map[bool]string{true: "on", false: "off"}[on], func(t *testing.T) {
			t.Parallel()
			s := newSSOLinkEnv(t, on)
			c := s.confirmedInTelegram(t, s.sam, tgPerson(901, "Sam"))
			s.refreshMe(t) // the session expiry the link is bound to
			r := s.sam.confirm(c.ID, nil)
			expect(t, r, 201)
			var l tglink.Link
			r.json(t, &l)
			if l.MaxRole != "operator" {
				t.Fatalf("link cap %q", l.MaxRole)
			}
			got := map[authz.Grant]bool{}
			for _, g := range l.Grants {
				got[authz.Grant{Role: g.Role, Domain: g.Domain}] = true
			}
			viewerAll := authz.Grant{Role: authz.RoleViewer, Domain: "*"}
			if on {
				if !l.ViaSSO || l.SSOUntil == nil || !l.SSOUntil.Equal(s.me.ExpiresAt) {
					t.Fatalf("SSO link %+v, want via_sso until the session's absolute expiry %s", l, s.me.ExpiresAt)
				}
				want := map[authz.Grant]bool{viewerAll: true, {Role: authz.RoleOperator, Domain: s.space}: true}
				if len(got) != 2 || !got[viewerAll] || !got[authz.Grant{Role: authz.RoleOperator, Domain: s.space}] {
					t.Fatalf("link grants %v, want %v (SSO admin capped at operator)", got, want)
				}
				return
			}
			if l.ViaSSO || l.SSOUntil != nil {
				t.Fatalf("link carries SSO grants with the policy off: %+v", l)
			}
			if len(got) != 1 || !got[viewerAll] {
				t.Fatalf("link grants %v, want the local viewer grant only", got)
			}
		})
	}
}

// SM-TG-001 (ADR-0083 decision 2): an upstream SSO step-up between creating a code and confirming it rotates the browser
// session, and the code follows the new session, so the person can still confirm; the pre-step-up session is dead.
func TestTelegramLinkSurvivesAnSSOStepUp(t *testing.T) {
	t.Parallel()
	s := newSSOLinkEnv(t, false)
	old := s.sam.ui.csrf
	c := s.confirmedInTelegram(t, s.sam, tgPerson(902, "Sam"))
	s.h.clk.Advance(1) // the IdP's fresh login must not predate the session
	s.idpRoundTrip(t, "stepup")
	if s.sam.ui.csrf == old {
		t.Fatal("the step-up did not rotate the session")
	}
	expect(t, s.sam.confirm(c.ID, nil), 201)
}
