package oidc

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base32"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/eventbus"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/keyring/keyringtest"
	"github.com/aurimasniekis/go-spaceman/internal/oidc/oidctest"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/storetest"
)

// Ported from go-hallpass internal/oidc/oidc_test.go (bdd8329): operators are users, Hallpass roles are SpaceMan grants.

func TestMain(m *testing.M) {
	// Light Argon2id parameters keep race-enabled tests fast (as in internal/auth).
	auth.ParamSets[2] = auth.Params{Version: 2, Memory: 8 * 1024, Time: 1, Threads: 1, KeyLen: 32, SaltLen: 16}
	auth.CurrentParams = auth.ParamSets[2]
	os.Exit(m.Run())
}

const password = "correct horse battery staple"

var admin = audit.Actor{Type: audit.ActorUser, ID: "usr_admin"}

func ptr[T any](v T) *T { return &v }

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

type fx struct {
	t    *testing.T
	ctx  context.Context
	clk  *clock.Fake
	s    *store.Store
	az   *authz.Engine
	auth *auth.Service
	svc  *Service
	idp  *oidctest.IdP
	prov *Provider
	meta auth.SessionMeta
	totp map[string][]byte // user ID → TOTP secret enrolled by localSession
}

func newFx(t *testing.T, mods ...func(*ProviderInput)) *fx {
	t.Helper()
	clk := clock.NewFake(time.Now())
	s := storetest.Open(t, storetest.WithClock(clk))
	kr := keyringtest.Load(t, s)
	aw := audit.NewWriter(kr)
	az, err := authz.New(context.Background(), s.R())
	must(t, err)
	au := auth.New(s, kr, aw, eventbus.New(), az, auth.Options{Clock: clk, HashConcurrency: 2})
	svc := New(s, kr, aw, au, Options{PublicURL: "https://spaceman.example", Clock: clk, JWKSMinRefetch: 10 * time.Second})
	idp := oidctest.New(t)
	idp.Now = clk.Now
	idp.RedirectURI = "https://spaceman.example/api/v1/auth/oidc/callback"
	in := ProviderInput{Slug: ptr("corp"), DisplayName: ptr("Corp SSO"), Issuer: ptr(idp.Issuer()), ClientID: ptr(idp.ClientID),
		ClientSecret: ptr(idp.ClientSecret), CABundlePEM: ptr(idp.CAPEM()), AllowPrivateNetwork: ptr(true), Enabled: ptr(true), GroupsClaim: ptr("groups")}
	for _, m := range mods {
		m(&in)
	}
	p, err := svc.CreateProvider(context.Background(), admin, in)
	must(t, err)
	f := &fx{t: t, ctx: context.Background(), clk: clk, s: s, az: az, auth: au, svc: svc, idp: idp, prov: p, meta: auth.SessionMeta{IP: "198.51.100.7"},
		totp: map[string][]byte{}}
	// The first global administrator, so grant changes never trip the last-administrator guard.
	f.user("root@corp.example", authz.RoleAdmin)
	return f
}

// flow runs Start → IdP authorize → Callback.
func (f *fx) flow(purpose string, p *auth.Principal) (*CallbackResult, error) {
	f.t.Helper()
	st, err := f.svc.Start(f.ctx, StartInput{Provider: f.prov.Slug, Purpose: purpose, ReturnTo: "/spaces?x=1", Principal: p})
	if err != nil {
		return nil, err
	}
	q, err := f.idp.Authorize(st.RedirectURL)
	must(f.t, err)
	return f.svc.Callback(f.ctx, CallbackInput{State: q.Get("state"), Code: q.Get("code"), Error: q.Get("error"), Issuer: q.Get("iss"),
		Binding: st.Binding, Principal: p, Meta: f.meta})
}

// user creates a password user with global grants.
func (f *fx) user(email string, roles ...authz.Role) *auth.User {
	f.t.Helper()
	gs := []auth.GrantInput{}
	for _, r := range roles {
		gs = append(gs, auth.GrantInput{Role: r, Domain: authz.Global})
	}
	u, err := f.auth.Create(f.ctx, admin, auth.CreateInput{Email: email, DisplayName: email, Grants: gs, Password: password})
	must(f.t, err)
	return u
}

func (f *fx) principal(token string) *auth.Principal {
	f.t.Helper()
	p, err := f.auth.Authenticate(f.ctx, token)
	must(f.t, err)
	return p
}

func (f *fx) totpCode(userID string) string {
	return auth.TOTPCode(f.totp[userID], auth.TOTPStep(f.clk.Now()))
}

// localSession signs in with the password and completes MFA when the account needs it (privileged accounts must
// enroll TOTP); it returns a full password session.
func (f *fx) localSession(email string) *auth.Principal {
	f.t.Helper()
	res, err := f.auth.Login(f.ctx, auth.LoginInput{Email: email, Password: password, Meta: f.meta})
	must(f.t, err)
	p := res.Principal
	switch {
	case res.MustEnroll:
		e, err := f.auth.BeginTOTP(f.ctx, p)
		must(f.t, err)
		secret, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(e.Secret)
		must(f.t, err)
		f.totp[p.User.ID] = secret
		_, _, err = f.auth.ConfirmTOTP(f.ctx, p, f.totpCode(p.User.ID), f.meta)
		must(f.t, err)
		fallthrough
	case res.MFAPending:
		f.clk.Advance(30 * time.Second)
		p = f.principal(res.Token)
		tok, err := f.auth.VerifyMFA(f.ctx, p, f.totpCode(p.User.ID), f.meta)
		must(f.t, err)
		p = f.principal(tok)
	}
	if p.Limited {
		f.t.Fatalf("session still limited")
	}
	return p
}

// can reports whether p may perform perm in domain according to Casbin.
func (f *fx) can(p *auth.Principal, domain string, perm authz.Permission) bool {
	return f.az.AllowedAll(p.Subjects(), domain, perm)
}

func (f *fx) space() string {
	f.t.Helper()
	id := ids.New(ids.Space)
	must(f.t, f.s.W().Tx(f.ctx, func(tx *store.Tx) error {
		for _, q := range []string{
			`INSERT OR IGNORE INTO nodes (id, name, state, created_at, created_by, updated_at) VALUES ('nod_1', 'n1', 'active', 0, 't', 0)`,
			`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES (?, ?, 'S', 'nod_1', 'active', 'w', 0, 't', 0)`,
		} {
			args := []any{}
			if strings.Contains(q, "spaces") {
				args = []any{id, strings.ToLower(id[len(id)-8:])}
			}
			if _, err := tx.SQL.ExecContext(f.ctx, q, args...); err != nil {
				return err
			}
		}
		return nil
	}))
	return id
}

func (f *fx) mapGroup(group string, role authz.Role, domain string) {
	f.t.Helper()
	_, err := f.svc.AddMapping(f.ctx, admin, f.prov.ID, MappingInput{GroupValue: group, Role: role, Domain: domain})
	must(f.t, err)
}

func isCode(err error, c apperr.Code) bool { return apperr.CodeOf(err) == c }

// Hallpass HP-OIDC-002/003/009: code flow with PKCE, validated ID token, identity by (iss, sub), session created.
func TestLoginPreprovisioned(t *testing.T) {
	f := newFx(t)
	u := f.user("alice@corp.example", authz.RoleOperator)
	if _, err := f.flow(PurposeLogin, nil); !errors.Is(err, ErrNotProvisioned) {
		t.Fatalf("unlinked identity: %v", err)
	}
	must(t, f.svc.LinkIdentity(f.ctx, admin, u.ID, f.prov.ID, "user-1"))
	res, err := f.flow(PurposeLogin, nil)
	must(t, err)
	if res.ReturnTo != "/spaces?x=1" || res.UserID != u.ID {
		t.Fatalf("%+v", res)
	}
	p := f.principal(res.SessionToken)
	if p.Method != auth.MethodOIDC || p.Provider != "corp" || !f.can(p, authz.Global, authz.ApprovalDecide) || p.Limited {
		t.Fatalf("principal %+v", p)
	}
	q := f.idp.LastAuthorizeParams()
	if q.Get("code_challenge_method") != "S256" || q.Get("nonce") == "" || q.Get("redirect_uri") != "https://spaceman.example/api/v1/auth/oidc/callback" ||
		q.Get("prompt") != "" {
		t.Fatalf("authorize params %v", q)
	}
	ids, err := f.svc.Identities(f.ctx, u.ID)
	if err != nil || len(ids) != 1 || ids[0].LastLoginAt == nil || ids[0].ProviderSlug != "corp" {
		t.Fatalf("%+v %v", ids, err)
	}
	pp, err := f.svc.EnabledProviders(f.ctx)
	if err != nil || len(pp) != 1 || pp[0].Slug != "corp" {
		t.Fatalf("%+v %v", pp, err)
	}
}

// RFC 9207 (Porch sends iss): a matching iss is accepted, another issuer is a mix-up and rejected.
func TestResponseIssuer(t *testing.T) {
	f := newFx(t)
	u := f.user("alice@corp.example")
	must(t, f.svc.LinkIdentity(f.ctx, admin, u.ID, f.prov.ID, "user-1"))
	f.idp.Set(func(i *oidctest.IdP) { i.ResponseIssuer = f.idp.Issuer() })
	if _, err := f.flow(PurposeLogin, nil); err != nil {
		t.Fatalf("matching iss: %v", err)
	}
	f.idp.Set(func(i *oidctest.IdP) { i.ResponseIssuer = "https://evil.example" })
	if _, err := f.flow(PurposeLogin, nil); !errors.Is(err, ErrTokenValidation) {
		t.Fatalf("mix-up accepted: %v", err)
	}
}

// Hallpass HP-OIDC-009: an email match never links or merges.
func TestNoEmailMerge(t *testing.T) {
	f := newFx(t, func(in *ProviderInput) {
		in.Provisioning = ptr(JIT)
		in.JIT = &JITRules{EmailDomains: []string{"example.com"}}
	})
	f.user("alice@example.com", authz.RoleAdmin) // same email as the IdP user
	_, err := f.flow(PurposeLogin, nil)
	if !isCode(err, apperr.CodeConflict) {
		t.Fatalf("expected conflict, got %v", err)
	}
	var n int
	must(t, f.s.ReadDB().QueryRowContext(f.ctx, `SELECT count(*) FROM user_identities`).Scan(&n))
	if n != 0 {
		t.Fatal("identity linked by email")
	}
}

// Hallpass HP-OIDC-010/011: JIT restrictions, no grant without a mapping, grant sync on every login.
func TestJITAndGroupSync(t *testing.T) {
	f := newFx(t, func(in *ProviderInput) {
		in.Provisioning = ptr(JIT)
		in.JIT = &JITRules{EmailDomains: []string{"example.com"}, Groups: []string{"spaceman-users"}}
	})
	f.idp.Set(func(i *oidctest.IdP) { i.User.Claims["groups"] = []string{"other"} })
	if _, err := f.flow(PurposeLogin, nil); !errors.Is(err, ErrNotProvisioned) {
		t.Fatalf("group restriction: %v", err)
	}
	f.idp.Set(func(i *oidctest.IdP) {
		i.User.Claims["groups"] = []string{"spaceman-users"}
		i.User.Claims["email_verified"] = false
	})
	if _, err := f.flow(PurposeLogin, nil); !errors.Is(err, ErrNotProvisioned) {
		t.Fatalf("unverified email: %v", err)
	}
	f.idp.Set(func(i *oidctest.IdP) { i.User.Claims["email_verified"] = true })
	res, err := f.flow(PurposeLogin, nil)
	must(t, err)
	p := f.principal(res.SessionToken)
	if len(p.User.Grants) != 0 || len(p.User.SSOGrants) != 0 || p.User.ProvisionedVia != "jit" || f.can(p, authz.Global, authz.SpaceRead) {
		t.Fatalf("JIT user got grants %v/%v via %s", p.User.Grants, p.User.SSOGrants, p.User.ProvisionedVia)
	}
	space := f.space()
	f.mapGroup("spaceman-operators", authz.RoleOperator, space)
	f.idp.Set(func(i *oidctest.IdP) { i.User.Claims["groups"] = []string{"spaceman-users", "spaceman-operators"} })
	res, err = f.flow(PurposeLogin, nil)
	must(t, err)
	if p = f.principal(res.SessionToken); !f.can(p, space, authz.QueueWrite) || f.can(p, authz.Global, authz.SpaceRead) {
		t.Fatalf("mapped space grant wrong: %v", p.User.SSOGrants)
	}
	if len(p.User.SSOGrants) != 1 || p.User.SSOGrants[0].Source != "sso" || p.User.SSOGrants[0].SourceRef != "spaceman-operators" {
		t.Fatalf("sso grants %+v", p.User.SSOGrants)
	}
	_, err = f.auth.SetGrants(f.ctx, admin, p.User.ID, []auth.GrantInput{{Role: authz.RoleViewer, Domain: authz.Global}})
	must(t, err)
	f.idp.Set(func(i *oidctest.IdP) { i.User.Claims["groups"] = []string{"spaceman-users"} })
	res, err = f.flow(PurposeLogin, nil)
	must(t, err)
	p = f.principal(res.SessionToken)
	if f.can(p, space, authz.QueueWrite) || !f.can(p, space, authz.SpaceRead) {
		t.Fatalf("after group removal: %v (manual viewer must stay)", p.User.Grants)
	}
}

// Hallpass HP-OIDC-011, Porch contract: groups_claim reads a nested claim by dotted path (Porch sends porch.groups),
// while a top-level claim whose name contains dots still wins.
func TestGroupsClaimPath(t *testing.T) {
	f := newFx(t, func(in *ProviderInput) {
		in.GroupsClaim = ptr("porch.groups")
		in.Provisioning = ptr(JIT)
		in.JIT = &JITRules{Groups: []string{"spaceman-users"}}
	})
	f.mapGroup("spaceman-admins", authz.RoleAdmin, authz.Global)
	f.idp.Set(func(i *oidctest.IdP) {
		i.User.Subject = "u_01porchuser"
		i.User.Claims["porch"] = map[string]any{"v": 1, "tenant": map[string]any{"id": "t_1", "slug": "acme"}, "roles": []string{},
			"groups": []string{"spaceman-users", "spaceman-admins"}}
	})
	res, err := f.flow(PurposeLogin, nil)
	must(t, err)
	if p := f.principal(res.SessionToken); !f.can(p, authz.Global, authz.UserWrite) {
		t.Fatalf("nested groups not mapped: %v", p.User.SSOGrants)
	}
	f.idp.Set(func(i *oidctest.IdP) {
		i.User.Claims["porch"] = map[string]any{"v": 1, "roles": []string{}, "groups": []string{"spaceman-users"}}
	})
	res, err = f.flow(PurposeLogin, nil)
	must(t, err)
	if p := f.principal(res.SessionToken); f.can(p, authz.Global, authz.UserWrite) {
		t.Fatalf("grant kept after the nested group was removed: %v", p.User.SSOGrants)
	}
}

func TestClaimAt(t *testing.T) {
	all := map[string]json.RawMessage{
		"groups":                     json.RawMessage(`["top"]`),
		"https://example.com/groups": json.RawMessage(`["url"]`),
		"porch.groups":               json.RawMessage(`["dotted-name"]`),
		"porch":                      json.RawMessage(`{"groups":["nested"],"tenant":{"slug":"acme"}}`),
		"flat":                       json.RawMessage(`"x"`),
		"nul":                        json.RawMessage(`null`),
	}
	for path, want := range map[string]string{
		"groups":                     `["top"]`,
		"https://example.com/groups": `["url"]`,
		"porch.groups":               `["dotted-name"]`, // an exact top-level name wins over the path
		"porch.tenant.slug":          `"acme"`,
		"porch.missing":              ``,
		"flat.groups":                ``,
		"nul.groups":                 ``,
		"absent":                     ``,
		"":                           ``,
	} {
		if got := string(claimAt(all, path)); got != want {
			t.Errorf("claimAt(%q) = %q, want %q", path, got, want)
		}
	}
	delete(all, "porch.groups")
	if got := string(claimAt(all, "porch.groups")); got != `["nested"]` {
		t.Errorf("nested path = %q", got)
	}
}

// SSO grants never count toward the last-administrator guard (adapted from Hallpass HP-AUTH-015 TestSyncKeepsLastAdmin):
// an IdP-only administrator can lose the grant, and a manual global administrator always remains.
func TestSSOGrantsNeverCountAsLastAdmin(t *testing.T) {
	f := newFx(t)
	u, err := f.auth.Create(f.ctx, admin, auth.CreateInput{Email: "sso-admin@example.com", DisplayName: "SSO admin"})
	must(t, err)
	must(t, f.svc.LinkIdentity(f.ctx, admin, u.ID, f.prov.ID, "user-1"))
	f.mapGroup("admins", authz.RoleAdmin, authz.Global)
	f.idp.Set(func(i *oidctest.IdP) { i.User.Claims["groups"] = []string{"admins"} })
	res, err := f.flow(PurposeLogin, nil)
	must(t, err)
	if !f.can(f.principal(res.SessionToken), authz.Global, authz.UserWrite) {
		t.Fatal("SSO admin grant not applied")
	}
	if n, err := f.s.R().CountEnabledGlobalAdmins(f.ctx); err != nil || n != 1 {
		t.Fatalf("SSO admin counted toward the guard: %d %v", n, err)
	}
	f.idp.Set(func(i *oidctest.IdP) { i.User.Claims["groups"] = []string{} })
	res, err = f.flow(PurposeLogin, nil)
	must(t, err)
	if f.can(f.principal(res.SessionToken), authz.Global, authz.UserWrite) {
		t.Fatal("SSO admin grant kept after the group was removed")
	}
}

// Hallpass HP-OIDC-002: state is single-use, bound to the browser and expires after 10 minutes.
func TestStateSafety(t *testing.T) {
	f := newFx(t)
	u := f.user("bob@example.com")
	must(t, f.svc.LinkIdentity(f.ctx, admin, u.ID, f.prov.ID, "user-1"))
	st, err := f.svc.Start(f.ctx, StartInput{Provider: "corp", Purpose: PurposeLogin})
	must(t, err)
	q, err := f.idp.Authorize(st.RedirectURL)
	must(t, err)
	if _, err := f.svc.Callback(f.ctx, CallbackInput{State: q.Get("state"), Code: q.Get("code"), Binding: "wrong"}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("binding: %v", err)
	}
	if _, err := f.svc.Callback(f.ctx, CallbackInput{State: q.Get("state"), Code: q.Get("code"), Binding: st.Binding}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("replayed state accepted: %v", err)
	}
	st, err = f.svc.Start(f.ctx, StartInput{Provider: "corp", Purpose: PurposeLogin})
	must(t, err)
	q, err = f.idp.Authorize(st.RedirectURL)
	must(t, err)
	f.clk.Advance(11 * time.Minute)
	if _, err := f.svc.Callback(f.ctx, CallbackInput{State: q.Get("state"), Code: q.Get("code"), Binding: st.Binding}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("expired state: %v", err)
	}
	if n, err := f.svc.PurgeStates(f.ctx); err != nil || n != 0 {
		t.Fatalf("purge %d %v", n, err)
	}
	if _, err := f.svc.Callback(f.ctx, CallbackInput{State: "", Code: "x"}); !errors.Is(err, ErrInvalidState) {
		t.Fatal("empty state")
	}
	// IdP error responses are rejected after consuming the state.
	st, err = f.svc.Start(f.ctx, StartInput{Provider: "corp", Purpose: PurposeLogin})
	must(t, err)
	q, err = f.idp.Authorize(st.RedirectURL)
	must(t, err)
	if _, err := f.svc.Callback(f.ctx, CallbackInput{State: q.Get("state"), Error: "access_denied", Binding: st.Binding}); !errors.Is(err, ErrLoginRejected) {
		t.Fatalf("idp error: %v", err)
	}
	// A pending state that expired is purged.
	_, err = f.svc.Start(f.ctx, StartInput{Provider: "corp", Purpose: PurposeLogin})
	must(t, err)
	f.clk.Advance(11 * time.Minute)
	if n, err := f.svc.PurgeStates(f.ctx); err != nil || n != 1 {
		t.Fatalf("purge %d %v", n, err)
	}
}

// Hallpass HP-OIDC-003: signature algorithms, iss/aud/azp, time claims and nonce are enforced.
func TestIDTokenValidation(t *testing.T) {
	f := newFx(t)
	u := f.user("carol@example.com")
	must(t, f.svc.LinkIdentity(f.ctx, admin, u.ID, f.prov.ID, "user-1"))
	for _, alg := range []string{"RS256", "ES256", "EdDSA"} {
		f.idp.Set(func(i *oidctest.IdP) { i.Alg = alg })
		if _, err := f.flow(PurposeLogin, nil); err != nil {
			t.Fatalf("%s rejected: %v", alg, err)
		}
	}
	cases := map[string]func(i *oidctest.IdP){
		"alg none":  func(i *oidctest.IdP) { i.Alg = "none" },
		"alg HS256": func(i *oidctest.IdP) { i.Alg = "HS256" },
		"wrong iss": func(i *oidctest.IdP) { i.Mutate = func(c map[string]any) { c["iss"] = "https://evil.example" } },
		"wrong aud": func(i *oidctest.IdP) { i.Mutate = func(c map[string]any) { c["aud"] = "other-client" } },
		"multi aud": func(i *oidctest.IdP) { i.Mutate = func(c map[string]any) { c["aud"] = []string{"spaceman", "other"} } },
		"azp mismatch": func(i *oidctest.IdP) {
			i.Mutate = func(c map[string]any) { c["aud"] = []string{"spaceman", "other"}; c["azp"] = "other" }
		},
		"expired": func(i *oidctest.IdP) {
			i.Mutate = func(c map[string]any) { c["exp"] = f.clk.Now().Add(-2 * time.Minute).Unix() }
		},
		"iat in future": func(i *oidctest.IdP) {
			i.Mutate = func(c map[string]any) { c["iat"] = f.clk.Now().Add(5 * time.Minute).Unix() }
		},
		"nbf in future": func(i *oidctest.IdP) {
			i.Mutate = func(c map[string]any) { c["nbf"] = f.clk.Now().Add(3 * time.Minute).Unix() }
		},
		"wrong nonce":    func(i *oidctest.IdP) { i.Mutate = func(c map[string]any) { c["nonce"] = "attacker" } },
		"missing nonce":  func(i *oidctest.IdP) { i.Mutate = func(c map[string]any) { delete(c, "nonce") } },
		"missing sub":    func(i *oidctest.IdP) { i.Mutate = func(c map[string]any) { c["sub"] = "" } },
		"missing iat":    func(i *oidctest.IdP) { i.Mutate = func(c map[string]any) { delete(c, "iat") } },
		"foreign signer": func(i *oidctest.IdP) { i.Alg = "RS256" },
	}
	for name, set := range cases {
		t.Run(name, func(t *testing.T) {
			f.idp.Set(func(i *oidctest.IdP) { i.Alg, i.Mutate = "RS256", nil })
			if name == "foreign signer" {
				// Tokens signed by keys the JWKS no longer has: rotate after discovery but deny refetch.
				f.idp.Set(func(i *oidctest.IdP) { i.FailJWKS = true })
				f.idp.RotateKeys()
				f.clk.Advance(time.Minute)
				defer f.idp.Set(func(i *oidctest.IdP) { i.FailJWKS = false })
			}
			f.idp.Set(set)
			if _, err := f.flow(PurposeLogin, nil); err == nil {
				t.Fatal("accepted")
			}
		})
	}
	f.idp.Set(func(i *oidctest.IdP) {
		i.Alg, i.Mutate = "RS256", func(c map[string]any) { c["aud"] = []string{"spaceman", "other"}; c["azp"] = "spaceman" }
	})
	f.clk.Advance(time.Minute)
	if _, err := f.flow(PurposeLogin, nil); err != nil {
		t.Fatalf("multi-aud with matching azp: %v", err)
	}
	// Expired within the 60 s skew is still accepted.
	f.idp.Set(func(i *oidctest.IdP) {
		i.Mutate = func(c map[string]any) { c["exp"] = f.clk.Now().Add(-30 * time.Second).Unix() }
	})
	if _, err := f.flow(PurposeLogin, nil); err != nil {
		t.Fatalf("within skew: %v", err)
	}
}

// Hallpass HP-OIDC-004: JWKS is cached, refetched on an unknown kid (key rotation) and refetches are rate-limited.
func TestJWKSRotation(t *testing.T) {
	f := newFx(t)
	u := f.user("dave@example.com")
	must(t, f.svc.LinkIdentity(f.ctx, admin, u.ID, f.prov.ID, "user-1"))
	_, err := f.flow(PurposeLogin, nil)
	must(t, err)
	_, err = f.flow(PurposeLogin, nil)
	must(t, err)
	if h := f.idp.JWKSHits(); h != 1 {
		t.Fatalf("JWKS not cached: %d fetches", h)
	}
	f.idp.RotateKeys()
	if _, err := f.flow(PurposeLogin, nil); err == nil {
		t.Fatal("refetch should be rate-limited within the minimum interval")
	}
	f.clk.Advance(11 * time.Second)
	if _, err := f.flow(PurposeLogin, nil); err != nil {
		t.Fatalf("after rotation: %v", err)
	}
	if h := f.idp.JWKSHits(); h != 2 {
		t.Fatalf("fetches %d", h)
	}
}

// Hallpass HP-OIDC-001/005/006: issuer match, https, endpoint origin, private networks, TLS verification.
func TestProviderValidation(t *testing.T) {
	f := newFx(t)
	bad := []ProviderInput{
		{Slug: ptr("x1"), DisplayName: ptr("x"), Issuer: ptr("http://idp.example"), ClientID: ptr("c")},
		{Slug: ptr("x2"), DisplayName: ptr("x"), Issuer: ptr("https://idp.example?x=1"), ClientID: ptr("c")},
		{Slug: ptr("X bad"), DisplayName: ptr("x"), Issuer: ptr("https://idp.example"), ClientID: ptr("c")},
		{Slug: ptr("x3"), DisplayName: ptr("x"), Issuer: ptr("https://idp.example"), ClientID: ptr("c"), Scopes: ptr([]string{"profile"})},
		{Slug: ptr("x4"), DisplayName: ptr("x"), Issuer: ptr("https://idp.example"), ClientID: ptr("c"), Provisioning: ptr(JIT)},
		{Slug: ptr("x5"), DisplayName: ptr("x"), Issuer: ptr("https://idp.example"), ClientID: ptr("c"), CABundlePEM: ptr("not pem")},
		{Slug: ptr("x6"), DisplayName: ptr("x"), Issuer: ptr("https://idp.example"), ClientID: ptr("c"), StepUpFallback: ptr("sms")},
		{Slug: ptr("x7"), DisplayName: ptr(""), Issuer: ptr("https://idp.example"), ClientID: ptr("c")},
		{Slug: ptr("x8"), DisplayName: ptr("x"), Issuer: ptr("https://idp.example"), ClientID: ptr("")},
		{Slug: ptr("x9"), DisplayName: ptr("x"), Issuer: ptr("https://idp.example"), ClientID: ptr("c"), Scopes: ptr([]string{"openid", "a b"})},
		{Slug: ptr("y1"), DisplayName: ptr("x"), Issuer: ptr("https://idp.example"), ClientID: ptr("c"), GroupsClaim: ptr(strings.Repeat("g", 65))},
		{Slug: ptr("y2"), DisplayName: ptr("x"), Issuer: ptr("https://idp.example"), ClientID: ptr("c"), Provisioning: ptr(JIT),
			JIT: &JITRules{Groups: []string{"g"}}},
		{Slug: ptr("y3"), DisplayName: ptr("x"), Issuer: ptr("https://idp.example"), ClientID: ptr("c"), Provisioning: ptr("magic")},
		{Slug: ptr("y4"), DisplayName: ptr("x"), Issuer: ptr("https://idp.example"), ClientID: ptr("c"), ACRValues: ptr([]string{strings.Repeat("a", 129)})},
		{Slug: ptr("y5"), DisplayName: ptr("x"), Issuer: ptr("https://idp.example"), ClientID: ptr("c"), ClientSecret: ptr(strings.Repeat("s", 1025))},
	}
	for i, in := range bad {
		if _, err := f.svc.CreateProvider(f.ctx, admin, in); !isCode(err, apperr.CodeInvalid) {
			t.Errorf("case %d accepted: %v", i, err)
		}
	}
	if _, err := f.svc.CreateProvider(f.ctx, admin, ProviderInput{Slug: ptr("corp"), DisplayName: ptr("x"), Issuer: ptr("https://idp.example"),
		ClientID: ptr("c")}); !isCode(err, apperr.CodeAlreadyExists) {
		t.Errorf("duplicate slug: %v", err)
	}
	start := func(mod func(*ProviderInput)) error {
		in := ProviderInput{Version: f.prov.Version, AllowPrivateNetwork: ptr(true), Issuer: ptr(f.idp.Issuer()), CABundlePEM: ptr(f.idp.CAPEM())}
		mod(&in)
		p, err := f.svc.UpdateProvider(f.ctx, admin, f.prov.ID, in)
		must(t, err)
		f.prov = p
		_, err = f.svc.Start(f.ctx, StartInput{Provider: "corp", Purpose: PurposeLogin})
		return err
	}
	if err := start(func(in *ProviderInput) { in.AllowPrivateNetwork = ptr(false) }); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("private network IdP allowed: %v", err)
	}
	if err := start(func(in *ProviderInput) { in.CABundlePEM = ptr("") }); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("untrusted TLS accepted: %v", err)
	}
	f.idp.Set(func(i *oidctest.IdP) { i.IssuerOverride = "https://other.example" })
	if err := start(func(*ProviderInput) {}); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("issuer mismatch accepted: %v", err)
	}
	f.idp.Set(func(i *oidctest.IdP) { i.IssuerOverride, i.EndpointBase = "", "https://login.other-site.example" })
	if err := start(func(*ProviderInput) {}); !isCode(err, apperr.CodeUnavailable) {
		t.Fatalf("cross-origin endpoint accepted: %v", err)
	}
	if err := start(func(in *ProviderInput) { in.AllowCrossOriginEndpoints = ptr(true) }); err != nil {
		t.Fatalf("explicitly allowed cross-origin endpoint: %v", err)
	}
	f.idp.Set(func(i *oidctest.IdP) { i.EndpointBase = "" })
	if err := start(func(in *ProviderInput) { in.AllowCrossOriginEndpoints = ptr(false) }); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Start(f.ctx, StartInput{Provider: "corp", Purpose: "bogus"}); !isCode(err, apperr.CodeInvalid) {
		t.Fatalf("unknown purpose: %v", err)
	}
	if _, err := f.svc.Start(f.ctx, StartInput{Provider: "nope", Purpose: PurposeLogin}); !isCode(err, apperr.CodeNotFound) {
		t.Fatalf("unknown provider: %v", err)
	}
	// Dev mode allows http on localhost only.
	plain := httptest.NewServer(http.NotFoundHandler())
	defer plain.Close()
	dev := New(f.s, nil, nil, nil, Options{DevMode: true})
	if err := dev.validate(&Provider{Slug: "d", DisplayName: "d", Issuer: plain.URL, ClientID: "c", Scopes: []string{"openid"}, Provisioning: Preprovisioned,
		StepUpFallback: StepUpNone}); err != nil {
		t.Fatalf("dev localhost http: %v", err)
	}
	if err := dev.validate(&Provider{Slug: "d", DisplayName: "d", Issuer: "http://idp.example", ClientID: "c", Scopes: []string{"openid"},
		Provisioning: Preprovisioned, StepUpFallback: StepUpNone}); err == nil {
		t.Fatal("dev non-localhost http allowed")
	}
	if dev.checkEndpoint(&Provider{Issuer: "http://localhost:1"}, "http://localhost:2/token") != nil {
		t.Fatal("dev localhost endpoint refused")
	}
	if site("10.0.0.1") != "10.0.0.1" || site("a.b.example.co.uk") != "example.co.uk" {
		t.Fatal("site")
	}
}

// Hallpass HP-OIDC-007: the client secret is sealed at rest and never returned.
func TestClientSecretWriteOnly(t *testing.T) {
	f := newFx(t)
	b, _ := json.Marshal(f.prov)
	if strings.Contains(string(b), f.idp.ClientSecret) || !f.prov.HasClientSecret {
		t.Fatalf("secret exposed or missing: %s", b)
	}
	var raw []byte
	must(t, f.s.ReadDB().QueryRowContext(f.ctx, `SELECT client_secret_enc FROM oidc_providers WHERE id = ?`, f.prov.ID).Scan(&raw))
	if bytes.Contains(raw, []byte(f.idp.ClientSecret)) {
		t.Fatal("client secret stored in plaintext")
	}
	// Updating without a secret keeps it; "" removes it.
	p, err := f.svc.UpdateProvider(f.ctx, admin, f.prov.ID, ProviderInput{Version: f.prov.Version, DisplayName: ptr("Corp")})
	must(t, err)
	if !p.HasClientSecret {
		t.Fatal("secret dropped by unrelated update")
	}
	p, err = f.svc.UpdateProvider(f.ctx, admin, f.prov.ID, ProviderInput{Version: p.Version, ClientSecret: ptr("")})
	must(t, err)
	if p.HasClientSecret {
		t.Fatal("secret not removed")
	}
	if _, err := f.svc.UpdateProvider(f.ctx, admin, f.prov.ID, ProviderInput{Version: 1}); !isCode(err, apperr.CodeConflict) {
		t.Fatalf("stale version accepted: %v", err)
	}
	if _, err := f.svc.UpdateProvider(f.ctx, admin, "oidc_missing", ProviderInput{Version: 1}); !isCode(err, apperr.CodeNotFound) {
		t.Fatalf("missing provider: %v", err)
	}
	other, err := f.svc.CreateProvider(f.ctx, admin, ProviderInput{Slug: ptr("other"), DisplayName: ptr("Other"), Issuer: ptr("https://idp.example"), ClientID: ptr("c")})
	must(t, err)
	if _, err := f.svc.UpdateProvider(f.ctx, admin, other.ID, ProviderInput{Version: other.Version, Slug: ptr("corp")}); !isCode(err, apperr.CodeAlreadyExists) {
		t.Fatalf("slug collision on update: %v", err)
	}
	ps, err := f.svc.ListProviders(f.ctx)
	if err != nil || len(ps) != 2 {
		t.Fatalf("%d %v", len(ps), err)
	}
}

// Hallpass HP-OIDC-012: outages fail closed; existing sessions continue.
func TestOutage(t *testing.T) {
	f := newFx(t)
	u := f.user("erin@example.com")
	must(t, f.svc.LinkIdentity(f.ctx, admin, u.ID, f.prov.ID, "user-1"))
	res, err := f.flow(PurposeLogin, nil)
	must(t, err)
	f.idp.Set(func(i *oidctest.IdP) { i.FailToken = true })
	if _, err := f.flow(PurposeLogin, nil); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("token outage: %v", err)
	}
	f.idp.Set(func(i *oidctest.IdP) { i.FailToken = false; i.ClientSecret = "rotated-elsewhere" })
	if _, err := f.flow(PurposeLogin, nil); !errors.Is(err, ErrLoginRejected) {
		t.Fatalf("rejected client: %v", err)
	}
	f.idp.Close()
	f.svc.forget(f.prov.ID)
	if _, err := f.svc.Start(f.ctx, StartInput{Provider: "corp", Purpose: PurposeLogin}); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("discovery outage: %v", err)
	}
	if _, err := f.auth.Authenticate(f.ctx, res.SessionToken); err != nil {
		t.Fatalf("existing session broken by outage: %v", err)
	}
	if _, err := f.auth.Login(f.ctx, auth.LoginInput{Email: "erin@example.com", Password: password, Meta: f.meta}); err != nil {
		t.Fatalf("local login unavailable during outage: %v", err)
	}
}

// Hallpass HP-OIDC-013: step-up requires a fresh auth_time (and acr when configured); TOTP fallback when unsupported.
func TestStepUp(t *testing.T) {
	f := newFx(t)
	u := f.user("frank@example.com")
	must(t, f.svc.LinkIdentity(f.ctx, admin, u.ID, f.prov.ID, "user-1"))
	res, err := f.flow(PurposeLogin, nil)
	must(t, err)
	p := f.principal(res.SessionToken)
	if _, err := f.svc.Start(f.ctx, StartInput{Provider: "corp", Purpose: PurposeStepUp}); err == nil {
		t.Fatal("step-up without a session")
	}
	if _, err := f.svc.Start(f.ctx, StartInput{Provider: "corp", Purpose: PurposeStepUp, Principal: f.localSession("frank@example.com")}); err == nil {
		t.Fatal("step-up through SSO for a password session")
	}
	f.clk.Advance(10 * time.Minute)
	if err := f.auth.RequireRecentAuth(p, false); err == nil {
		t.Fatal("stale auth accepted")
	}
	res, err = f.flow(PurposeStepUp, p)
	must(t, err)
	if q := f.idp.LastAuthorizeParams(); q.Get("prompt") != "login" || q.Get("max_age") != "0" {
		t.Fatalf("step-up params %v", q)
	}
	p2 := f.principal(res.SessionToken)
	if p2.Provider != "corp" {
		t.Fatal("provider lost on step-up rotation")
	}
	if err := f.auth.RequireRecentAuth(p2, false); err != nil {
		t.Fatalf("after step-up: %v", err)
	}
	// An old auth_time (no fresh login at the IdP) fails.
	f.idp.Set(func(i *oidctest.IdP) {
		i.Mutate = func(c map[string]any) { c["auth_time"] = f.clk.Now().Add(-time.Hour).Unix() }
	})
	if _, err := f.flow(PurposeStepUp, p2); !errors.Is(err, ErrStepUpFailed) {
		t.Fatalf("stale auth_time: %v", err)
	}
	f.idp.Set(func(i *oidctest.IdP) { i.Mutate, i.OmitAuthTime = nil, true })
	p3 := f.principal(mustToken(t, mustFlow(t, f, PurposeLogin, nil)))
	if _, err := f.flow(PurposeStepUp, p3); !errors.Is(err, ErrStepUpFailed) {
		t.Fatalf("missing auth_time: %v", err)
	}
	pr, err := f.svc.UpdateProvider(f.ctx, admin, f.prov.ID, ProviderInput{Version: f.prov.Version, StepUpFallback: ptr(StepUpLocalTOTP),
		ACRValues: ptr([]string{"urn:mfa"})})
	must(t, err)
	f.prov = pr
	if _, err := f.flow(PurposeStepUp, p3); !isCode(err, apperr.CodeMFARequired) {
		t.Fatalf("totp fallback: %v", err)
	}
	f.idp.Set(func(i *oidctest.IdP) {
		i.OmitAuthTime = false
		i.Mutate = func(c map[string]any) { c["acr"] = "urn:password" }
	})
	if _, err := f.flow(PurposeStepUp, p3); !errors.Is(err, ErrStepUpFailed) {
		t.Fatalf("acr mismatch: %v", err)
	}
	if q := f.idp.LastAuthorizeParams(); q.Get("acr_values") != "urn:mfa" {
		t.Fatalf("acr_values not sent: %v", q)
	}
	f.idp.Set(func(i *oidctest.IdP) { i.Mutate = func(c map[string]any) { c["acr"] = "urn:mfa" } })
	if _, err := f.flow(PurposeStepUp, p3); err != nil {
		t.Fatalf("matching acr: %v", err)
	}
	// The identity at the IdP must be the session's user.
	other := f.user("other@example.com")
	must(t, f.svc.LinkIdentity(f.ctx, admin, other.ID, f.prov.ID, "user-2"))
	p4 := f.principal(mustToken(t, mustFlow(t, f, PurposeLogin, nil)))
	f.idp.Set(func(i *oidctest.IdP) { i.User.Subject = "user-2" })
	if _, err := f.flow(PurposeStepUp, p4); !errors.Is(err, ErrStepUpFailed) {
		t.Fatalf("step-up as another identity: %v", err)
	}
}

func mustFlow(t *testing.T, f *fx, purpose string, p *auth.Principal) *CallbackResult {
	t.Helper()
	r, err := f.flow(purpose, p)
	must(t, err)
	return r
}

func mustToken(t *testing.T, r *CallbackResult) string {
	t.Helper()
	if r.SessionToken == "" {
		t.Fatal("no session token")
	}
	return r.SessionToken
}

// Hallpass HP-OIDC-014: a test login previews the mapping without creating a session and records the evidence.
func TestTestLogin(t *testing.T) {
	f := newFx(t)
	adminP := f.localSession("root@corp.example")
	if _, err := f.svc.Start(f.ctx, StartInput{Provider: "corp", Purpose: PurposeTest}); err == nil {
		t.Fatal("test login without admin session")
	}
	viewer := f.user("viewer@example.com", authz.RoleViewer)
	if _, err := f.svc.Start(f.ctx, StartInput{Provider: "corp", Purpose: PurposeTest, Principal: f.localSession(viewer.Email)}); !isCode(err, apperr.CodeForbidden) {
		t.Fatalf("test login by a viewer: %v", err)
	}
	f.mapGroup("admins", authz.RoleAdmin, authz.Global)
	f.idp.Set(func(i *oidctest.IdP) { i.User.Claims["groups"] = []string{"admins"} })
	if _, err := f.svc.TestResultFor(adminP.User.ID, f.prov.ID); !isCode(err, apperr.CodeNotFound) {
		t.Fatalf("result before any test: %v", err)
	}
	res, err := f.flow(PurposeTest, adminP)
	must(t, err)
	if res.SessionToken != "" || res.Test == nil || res.Test.Claims.Subject != "user-1" || len(res.Test.MappedGrants) != 1 || res.Test.LinkedUserID != "" {
		t.Fatalf("test result %+v", res.Test)
	}
	if got, err := f.svc.TestResultFor(adminP.User.ID, f.prov.ID); err != nil || got != res.Test {
		t.Fatalf("stored result %v", err)
	}
	if p, _ := f.svc.GetProvider(f.ctx, f.prov.ID); p.TestLoginOKAt == nil || p.TestLoginBy != adminP.User.ID {
		t.Fatal("test login not recorded")
	}
	// A disabled provider can still be tested.
	p, err := f.svc.UpdateProvider(f.ctx, admin, f.prov.ID, ProviderInput{Version: f.prov.Version, Enabled: ptr(false)})
	must(t, err)
	f.prov = p
	// Self-link the administrator's identity (signed in locally + at the IdP), then preview again.
	if _, err := f.flow(PurposeLink, adminP); !isCode(err, apperr.CodeNotFound) {
		t.Fatalf("link through a disabled provider: %v", err)
	}
	res, err = f.flow(PurposeTest, adminP)
	must(t, err)
	if res.Test.WouldProvision || res.Test.ProvisionReason == "" {
		t.Fatalf("preprovisioned preview %+v", res.Test)
	}
	p, err = f.svc.UpdateProvider(f.ctx, admin, f.prov.ID, ProviderInput{Version: f.prov.Version, Enabled: ptr(true), Provisioning: ptr(JIT),
		JIT: &JITRules{Groups: []string{"admins"}}})
	must(t, err)
	f.prov = p
	if p.TestLoginOKAt == nil {
		t.Fatal("test evidence dropped by an unrelated change")
	}
	res, err = f.flow(PurposeTest, adminP)
	must(t, err)
	if !res.Test.WouldProvision {
		t.Fatalf("JIT preview %+v", res.Test)
	}
	adminP = f.localSession("root@corp.example")
	res, err = f.flow(PurposeLink, adminP)
	must(t, err)
	if res.UserID != adminP.User.ID {
		t.Fatalf("%+v", res)
	}
	res, err = f.flow(PurposeTest, adminP)
	must(t, err)
	if res.Test.LinkedUserID != adminP.User.ID || len(res.Test.ManualGrants) != 1 {
		t.Fatalf("linked preview %+v", res.Test)
	}
	// Changing the issuer drops the evidence.
	p, err = f.svc.UpdateProvider(f.ctx, admin, f.prov.ID, ProviderInput{Version: f.prov.Version, ClientID: ptr("other")})
	must(t, err)
	if p.TestLoginOKAt != nil {
		t.Fatal("test evidence kept after a client change")
	}
	f.clk.Advance(11 * time.Minute)
	if _, err := f.svc.TestResultFor(adminP.User.ID, f.prov.ID); !isCode(err, apperr.CodeNotFound) {
		t.Fatalf("stale result kept: %v", err)
	}
}

// Hallpass HP-OIDC-009: linking requires a session or an admin invitation; identities are never moved.
func TestLinking(t *testing.T) {
	f := newFx(t)
	a := f.user("a@example.com")
	b := f.user("b@example.com")
	if _, err := f.svc.Start(f.ctx, StartInput{Provider: "corp", Purpose: PurposeLink}); err == nil {
		t.Fatal("anonymous link")
	}
	tok, _, err := f.svc.IssueLinkInvite(f.ctx, admin, a.ID, f.prov.ID)
	must(t, err)
	if _, err := f.svc.Start(f.ctx, StartInput{Provider: "corp", Purpose: PurposeLink, LinkToken: tok + "x"}); !isCode(err, apperr.CodeUnauthorized) {
		t.Fatalf("forged invitation: %v", err)
	}
	st, err := f.svc.Start(f.ctx, StartInput{Provider: "corp", Purpose: PurposeLink, LinkToken: tok})
	must(t, err)
	q, err := f.idp.Authorize(st.RedirectURL)
	must(t, err)
	res, err := f.svc.Callback(f.ctx, CallbackInput{State: q.Get("state"), Code: q.Get("code"), Binding: st.Binding})
	must(t, err)
	if res.UserID != a.ID {
		t.Fatalf("%+v", res)
	}
	if ids, _ := f.svc.Identities(f.ctx, a.ID); len(ids) != 1 || ids[0].LinkedVia != "admin" {
		t.Fatalf("%+v", ids)
	}
	if _, err := f.svc.Start(f.ctx, StartInput{Provider: "corp", Purpose: PurposeLink, LinkToken: tok}); err == nil {
		t.Fatal("link invitation reused")
	}
	if _, _, err := f.svc.IssueLinkInvite(f.ctx, admin, "usr_missing", f.prov.ID); !isCode(err, apperr.CodeNotFound) {
		t.Fatalf("invite for a missing user: %v", err)
	}
	if _, _, err := f.svc.IssueLinkInvite(f.ctx, admin, a.ID, "oidc_missing"); !isCode(err, apperr.CodeNotFound) {
		t.Fatalf("invite for a missing provider: %v", err)
	}
	pb := f.localSession("b@example.com")
	if _, err := f.flow(PurposeLink, pb); !isCode(err, apperr.CodeConflict) {
		t.Fatalf("identity moved to another user: %v", err)
	}
	if err := f.svc.LinkIdentity(f.ctx, admin, b.ID, f.prov.ID, "user-1"); !isCode(err, apperr.CodeConflict) {
		t.Fatalf("admin link moved identity: %v", err)
	}
	must(t, f.svc.LinkIdentity(f.ctx, admin, a.ID, f.prov.ID, "user-1")) // idempotent
	if err := f.svc.LinkIdentity(f.ctx, admin, a.ID, f.prov.ID, ""); !isCode(err, apperr.CodeInvalid) {
		t.Fatalf("empty subject: %v", err)
	}
	if err := f.svc.LinkIdentity(f.ctx, admin, "usr_missing", f.prov.ID, "user-9"); !isCode(err, apperr.CodeNotFound) {
		t.Fatalf("missing user: %v", err)
	}
	// The self-link callback must come back to the same session.
	st, err = f.svc.Start(f.ctx, StartInput{Provider: "corp", Purpose: PurposeLink, Principal: pb})
	must(t, err)
	q, err = f.idp.Authorize(st.RedirectURL)
	must(t, err)
	if _, err := f.svc.Callback(f.ctx, CallbackInput{State: q.Get("state"), Code: q.Get("code"), Binding: st.Binding}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("self-link completed without the session: %v", err)
	}
	pa := f.principal(mustToken(t, mustFlow(t, f, PurposeLogin, nil)))
	must(t, f.svc.UnlinkIdentity(f.ctx, admin, a.ID, f.prov.Issuer, "user-1", false))
	if ss, err := f.auth.ListSessions(f.ctx, a.ID, pa.SessionID); err != nil || len(ss) != 0 {
		t.Fatalf("sessions survived the unlink: %d %v", len(ss), err)
	}
	if _, err := f.flow(PurposeLogin, nil); !errors.Is(err, ErrNotProvisioned) {
		t.Fatalf("unlinked identity still logs in: %v", err)
	}
	if err := f.svc.UnlinkIdentity(f.ctx, admin, a.ID, f.prov.Issuer, "user-1", false); !isCode(err, apperr.CodeNotFound) {
		t.Fatalf("second unlink: %v", err)
	}
}

// A user may unlink their own identity unless it is their only way to sign in.
func TestSelfUnlink(t *testing.T) {
	f := newFx(t, func(in *ProviderInput) {
		in.Provisioning = ptr(JIT)
		in.JIT = &JITRules{EmailDomains: []string{"example.com"}}
	})
	res := mustFlow(t, f, PurposeLogin, nil)
	if err := f.svc.UnlinkIdentity(f.ctx, admin, res.UserID, f.prov.Issuer, "user-1", true); !errors.Is(err, ErrLastSignInMethod) {
		t.Fatalf("unlinked the only sign-in method: %v", err)
	}
	pw := f.user("pw@corp.example")
	must(t, f.svc.LinkIdentity(f.ctx, admin, pw.ID, f.prov.ID, "user-7"))
	must(t, f.svc.UnlinkIdentity(f.ctx, admin, pw.ID, f.prov.Issuer, "user-7", true))
}

// Hallpass HP-OIDC-005: post-login return paths are relative and allowlisted.
func TestSanitizeReturnTo(t *testing.T) {
	for in, want := range map[string]string{
		"/spaces?x=1": "/spaces?x=1", "": "/", "https://evil.example/": "/", "//evil.example": "/", "/\\evil": "/", "/api/v1/x": "/",
		"/auth/oidc/callback": "/", "javascript:alert(1)": "/", "/ok\r\nSet-Cookie: x": "/", "/spaces/spc_1#t=3": "/spaces/spc_1#t=3",
		"/./api/v1/me": "/", "/%2e/auth": "/", "/x\x01": "/",
	} {
		if got := SanitizeReturnTo(in); got != want {
			t.Errorf("%q → %q, want %q", in, got, want)
		}
	}
}

// Disabled users and disabled providers cannot sign in; denied logins are audited.
func TestDisabled(t *testing.T) {
	f := newFx(t)
	u := f.user("gail@example.com")
	must(t, f.svc.LinkIdentity(f.ctx, admin, u.ID, f.prov.ID, "user-1"))
	_, err := f.auth.SetEnabled(f.ctx, admin, u.ID, false)
	must(t, err)
	if _, err := f.flow(PurposeLogin, nil); !errors.Is(err, ErrLoginRejected) {
		t.Fatalf("disabled user: %v", err)
	}
	st, err := f.svc.Start(f.ctx, StartInput{Provider: "corp", Purpose: PurposeLogin})
	must(t, err)
	q, err := f.idp.Authorize(st.RedirectURL)
	must(t, err)
	p, err := f.svc.UpdateProvider(f.ctx, admin, f.prov.ID, ProviderInput{Version: f.prov.Version, Enabled: ptr(false)})
	must(t, err)
	f.prov = p
	if _, err := f.svc.Callback(f.ctx, CallbackInput{State: q.Get("state"), Code: q.Get("code"), Binding: st.Binding}); !errors.Is(err, ErrLoginRejected) {
		t.Fatalf("callback after the provider was disabled: %v", err)
	}
	if _, err := f.svc.Start(f.ctx, StartInput{Provider: "corp", Purpose: PurposeLogin}); !isCode(err, apperr.CodeNotFound) {
		t.Fatalf("disabled provider: %v", err)
	}
	if pp, err := f.svc.EnabledProviders(f.ctx); err != nil || len(pp) != 0 {
		t.Fatalf("disabled provider listed: %+v %v", pp, err)
	}
	var n int
	must(t, f.s.ReadDB().QueryRowContext(f.ctx, `SELECT count(*) FROM audit_events WHERE action = 'user.login.failed'`).Scan(&n))
	if n == 0 {
		t.Fatal("denied SSO login not audited")
	}
}

// Group mappings: domains must be "*" or an existing space; removing the space removes its mappings.
func TestMappings(t *testing.T) {
	f := newFx(t)
	space := f.space()
	for _, in := range []MappingInput{
		{GroupValue: "", Role: authz.RoleViewer, Domain: authz.Global},
		{GroupValue: "g", Role: "superuser", Domain: authz.Global},
		{GroupValue: "g", Role: authz.RoleViewer, Domain: "spc_bad"},
		{GroupValue: "g", Role: authz.RoleViewer, Domain: ids.New(ids.Space)},
	} {
		if _, err := f.svc.AddMapping(f.ctx, admin, f.prov.ID, in); !isCode(err, apperr.CodeInvalid) {
			t.Errorf("%+v accepted: %v", in, err)
		}
	}
	if _, err := f.svc.AddMapping(f.ctx, admin, "oidc_missing", MappingInput{GroupValue: "g", Role: authz.RoleViewer, Domain: authz.Global}); !isCode(err, apperr.CodeNotFound) {
		t.Fatalf("missing provider: %v", err)
	}
	m, err := f.svc.AddMapping(f.ctx, admin, f.prov.ID, MappingInput{GroupValue: " ops ", Role: authz.RoleOperator, Domain: space})
	must(t, err)
	if m.GroupValue != "ops" || m.Domain != space {
		t.Fatalf("%+v", m)
	}
	if _, err := f.svc.AddMapping(f.ctx, admin, f.prov.ID, MappingInput{GroupValue: "ops", Role: authz.RoleOperator, Domain: space}); !isCode(err, apperr.CodeAlreadyExists) {
		t.Fatalf("duplicate mapping: %v", err)
	}
	f.mapGroup("ops", authz.RoleViewer, authz.Global)
	ms, err := f.svc.ListMappings(f.ctx, f.prov.ID)
	if err != nil || len(ms) != 2 {
		t.Fatalf("%+v %v", ms, err)
	}
	must(t, f.svc.DeleteMapping(f.ctx, admin, f.prov.ID, m.ID))
	if err := f.svc.DeleteMapping(f.ctx, admin, f.prov.ID, m.ID); !isCode(err, apperr.CodeNotFound) {
		t.Fatalf("second delete: %v", err)
	}
	m, err = f.svc.AddMapping(f.ctx, admin, f.prov.ID, MappingInput{GroupValue: "ops", Role: authz.RoleOperator, Domain: space})
	must(t, err)
	must(t, f.s.W().Tx(f.ctx, func(tx *store.Tx) error {
		_, err := tx.SQL.ExecContext(f.ctx, `DELETE FROM spaces WHERE id = ?`, space)
		return err
	}))
	if ms, _ := f.svc.ListMappings(f.ctx, f.prov.ID); len(ms) != 1 || ms[0].Domain != authz.Global {
		t.Fatalf("space mapping survived the space: %+v", ms)
	}
	if _, err := f.svc.GetProvider(f.ctx, "oidc_missing"); !isCode(err, apperr.CodeNotFound) {
		t.Fatal(err)
	}
	if err := f.svc.DeleteProvider(f.ctx, admin, "oidc_missing"); !isCode(err, apperr.CodeNotFound) {
		t.Fatal(err)
	}
	_ = m
}

// Hallpass HP-OIDC-008: after an SSO login no access, refresh or ID token is stored anywhere: not in any table and not
// in the database or WAL files on disk.
func TestTokensNotPersisted(t *testing.T) {
	f := newFx(t)
	u := f.user("alice@corp.example", authz.RoleOperator)
	must(t, f.svc.LinkIdentity(f.ctx, admin, u.ID, f.prov.ID, "user-1"))
	if _, err := f.flow(PurposeLogin, nil); err != nil {
		t.Fatal(err)
	}
	access, refresh, id := f.idp.LastTokens()
	if access == "" || refresh == "" || id == "" {
		t.Fatal("mock IdP issued no tokens")
	}
	// The ID token's signature part is unique to this token; its claims segment could legitimately overlap stored
	// claims (subject, issuer), so search for the whole token and the signature.
	sig := id[strings.LastIndex(id, ".")+1:]
	needles := map[string]string{"access token": access, "refresh token": refresh, "id token": id, "id token signature": sig}
	names, err := queryStrings(f.ctx, f.s.ReadDB(), `SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	must(t, err)
	var found []string
	for _, table := range names {
		hits, err := tableContains(f.ctx, f.s.ReadDB(), table, needles)
		must(t, err)
		found = append(found, hits...)
	}
	if len(found) > 0 {
		t.Fatalf("tokens stored: %v", found)
	}
	for _, name := range []string{store.DBFile, store.DBFile + "-wal"} {
		raw, err := os.ReadFile(f.s.Path(name))
		if err != nil {
			continue
		}
		for what, needle := range needles {
			if bytes.Contains(raw, []byte(needle)) {
				t.Fatalf("%s found in %s on disk", what, name)
			}
		}
	}
}

func queryStrings(ctx context.Context, d *sql.DB, q string) ([]string, error) {
	rows, err := d.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// tableContains reports "<what> in <table>.<column>" for every text or blob cell that contains one of the needles.
func tableContains(ctx context.Context, d *sql.DB, table string, needles map[string]string) ([]string, error) {
	rows, err := d.QueryContext(ctx, `SELECT * FROM "`+table+`"`) //nolint:gosec // table names come from sqlite_master
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var found []string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		for i, v := range vals {
			var s string
			switch x := v.(type) {
			case string:
				s = x
			case []byte:
				s = string(x)
			}
			for what, needle := range needles {
				if s != "" && strings.Contains(s, needle) {
					found = append(found, what+" in "+table+"."+cols[i])
				}
			}
		}
	}
	return found, rows.Err()
}
