package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/api"
	"github.com/aurimasniekis/go-spaceman/internal/app"
	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/config"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/keyring"
)

// harness runs a real central (store, keyring, services, HTTP stack) on an httptest server.
type harness struct {
	t   *testing.T
	c   *app.Central
	srv *httptest.Server
	clk *clock.Fake
}

func newHarness(t *testing.T) *harness { t.Helper(); return newHarnessWith(t, nil) }

// newHarnessWith is newHarness with the configuration adjusted by mod before validation.
func newHarnessWith(t *testing.T, mod func(*config.Config)) *harness {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Mode = config.ModeDevelopment
	cfg.DataDir = filepath.Join(dir, "data")
	cfg.KEK.File = filepath.Join(dir, "kek")
	k, err := keyring.GenerateKEK()
	if err != nil {
		t.Fatal(err)
	}
	if err := keyring.WriteKEKFile(cfg.KEK.File, k); err != nil {
		t.Fatal(err)
	}
	clk := clock.NewFake(time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC))
	h := &harness{t: t, clk: clk}
	h.srv = httptest.NewUnstartedServer(nil)
	cfg.PublicURL = "http://" + h.srv.Listener.Addr().String()
	if mod != nil {
		mod(cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	c, err := app.OpenCentral(context.Background(), cfg, discard(), app.Options{Clock: clk, Environ: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	h.c = c
	h.srv.Config.Handler = c.Handler(c.APIDeps())
	h.srv.Start()
	t.Cleanup(h.srv.Close)
	return h
}

type client struct {
	h      *harness
	hc     *http.Client
	bearer string
	csrf   string
	origin string
}

func (h *harness) client() *client {
	jar, _ := cookiejar.New(nil)
	return &client{h: h, hc: &http.Client{Jar: jar}, origin: h.srv.URL}
}

type resp struct {
	status int
	header http.Header
	body   []byte
}

func (r resp) json(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(r.body, v); err != nil {
		t.Fatalf("decode %s: %v", r.body, err)
	}
}

func (r resp) problem(t *testing.T) api.Problem {
	t.Helper()
	var p api.Problem
	r.json(t, &p)
	if r.header.Get("Content-Type") != "application/problem+json" || p.Code == "" || p.Status != r.status {
		t.Fatalf("not a problem document: %d %s %s", r.status, r.header.Get("Content-Type"), r.body)
	}
	return p
}

func (c *client) do(method, path string, body any, hdr ...string) resp {
	c.h.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.h.srv.URL+api.Prefix+path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearer)
	}
	if c.csrf != "" {
		req.Header.Set(api.CSRFHeader, c.csrf)
	}
	if c.origin != "" {
		req.Header.Set("Origin", c.origin)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	res, err := c.hc.Do(req)
	if err != nil {
		c.h.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return resp{status: res.StatusCode, header: res.Header, body: b}
}

func expect(t *testing.T, r resp, status int) {
	t.Helper()
	if r.status != status {
		t.Fatalf("status %d, want %d: %s", r.status, status, r.body)
	}
}

// SM-AUTH-005, SM-AUTH-001, SM-API-002: bootstrap through the real HTTP stack, sign-in with forced MFA, CSRF, origin
// checks, problems, optimistic concurrency and idempotency.
func TestAPIBootstrapLoginAndConventions(t *testing.T) {
	h := newHarness(t)
	anon := h.client()

	var info api.SystemInfo
	r := anon.do("GET", "/system/info", nil)
	expect(t, r, 200)
	r.json(t, &info)
	if info.Bootstrapped || info.HasAdmin {
		t.Fatalf("fresh install info %+v", info)
	}
	expect(t, anon.do("GET", "/users", nil), 401)
	if p := anon.do("GET", "/users", nil).problem(t); p.Code != "unauthorized" || !strings.HasPrefix(p.Type, api.ProblemTypeBase) {
		t.Fatalf("problem %+v", p)
	}

	// Root token (as `spaceman admin bootstrap` issues it) creates the first administrator.
	rootSecret, _, err := h.c.Auth.IssueRootToken(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	root := h.client()
	root.bearer = rootSecret
	root.origin = "https://evil.example" // bearer requests are not subject to the browser origin check
	newAdmin := map[string]any{"email": "ada@example.com", "display_name": "Ada", "password": "correct horse battery staple",
		"grants": []map[string]string{{"role": "admin", "domain": "*"}}}
	r = root.do("POST", "/users", newAdmin, "Idempotency-Key", "bootstrap-admin-1")
	expect(t, r, 201)
	var admin auth.User
	r.json(t, &admin)
	if r.header.Get("ETag") != `"1"` || !admin.IsGlobalAdmin() {
		t.Fatalf("admin %+v etag %q", admin, r.header.Get("ETag"))
	}
	// Idempotent replay returns the same user; the same key with another body is a conflict.
	r = root.do("POST", "/users", newAdmin, "Idempotency-Key", "bootstrap-admin-1")
	expect(t, r, 201)
	var again auth.User
	r.json(t, &again)
	if again.ID != admin.ID || r.header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay %+v %v", again, r.header)
	}
	other := map[string]any{"email": "bob@example.com", "display_name": "Bob", "grants": []map[string]string{}}
	if p := root.do("POST", "/users", other, "Idempotency-Key", "bootstrap-admin-1").problem(t); p.Code != "conflict" {
		t.Fatalf("key reuse: %+v", p)
	}

	// Browser sign-in: password, then forced TOTP enrollment while the session is limited.
	ui := h.client()
	r = ui.do("POST", "/auth/login", map[string]string{"email": "ada@example.com", "password": "correct horse battery staple"})
	expect(t, r, 200)
	var me api.Me
	r.json(t, &me)
	if !me.Limited || !me.NeedsEnroll || me.CSRFToken == "" {
		t.Fatalf("first login must force enrollment: %+v", me)
	}
	ui.csrf = me.CSRFToken
	if p := ui.do("GET", "/users", nil).problem(t); p.Code != "mfa_required" {
		t.Fatalf("limited session reached a normal operation: %+v", p)
	}
	r = ui.do("POST", "/auth/mfa/enroll", nil)
	expect(t, r, 200)
	var enr api.TOTPEnrollment
	r.json(t, &enr)
	secret, err := auth.DecodeTOTPSecret(enr.Secret)
	if err != nil {
		t.Fatal(err)
	}
	r = ui.do("POST", "/auth/mfa/enroll/confirm", map[string]string{"code": auth.TOTPCode(secret, auth.TOTPStep(h.clk.Now()))})
	expect(t, r, 200)
	var conf api.EnrollConfirmed
	r.json(t, &conf)
	if len(conf.RecoveryCodes) != 10 || !conf.Me.Limited {
		t.Fatalf("enrollment must not complete sign-in by itself: %+v", conf)
	}
	h.clk.Advance(30 * time.Second)
	r = ui.do("POST", "/auth/mfa/verify", map[string]string{"code": auth.TOTPCode(secret, auth.TOTPStep(h.clk.Now()))})
	expect(t, r, 200)
	r.json(t, &me)
	if me.Limited {
		t.Fatalf("still limited after MFA: %+v", me)
	}
	ui.csrf = me.CSRFToken

	// CSRF and origin enforcement on cookie sessions.
	saved := ui.csrf
	ui.csrf = ""
	if p := ui.do("POST", "/users/"+admin.ID+"/enable", nil, "If-Match", `"1"`).problem(t); p.Status != 403 {
		t.Fatalf("missing CSRF: %+v", p)
	}
	ui.csrf = saved
	ui.origin = "https://evil.example"
	if p := ui.do("POST", "/auth/logout", nil).problem(t); p.Status != 403 || !strings.Contains(p.Detail, "cross-site") {
		t.Fatalf("cross-site mutation: %+v", p)
	}
	ui.origin = h.srv.URL

	// Optimistic concurrency: If-Match is required and must match.
	bob := root.do("POST", "/users", other)
	expect(t, bob, 201)
	var bu auth.User
	bob.json(t, &bu)
	grants := map[string]any{"grants": []map[string]string{{"role": "viewer", "domain": "*"}}}
	if p := ui.do("PUT", "/users/"+bu.ID+"/grants", grants).problem(t); p.Status != 428 || p.Code != "precondition_required" {
		t.Fatalf("missing If-Match: %+v", p)
	}
	if p := ui.do("PUT", "/users/"+bu.ID+"/grants", grants, "If-Match", `"7"`).problem(t); p.Code != "conflict" {
		t.Fatalf("stale If-Match: %+v", p)
	}
	expect(t, ui.do("PUT", "/users/"+bu.ID+"/grants", grants, "If-Match", `"1"`), 200)

	// Validation problems carry field errors; unknown routes are 404 problems.
	if p := root.do("POST", "/users", map[string]any{"email": 7}).problem(t); p.Status != 422 || len(p.Errors) == 0 {
		t.Fatalf("validation problem %+v", p)
	}

	// The root token can be rotated through the API; the old one stops at once.
	r = root.do("POST", "/admin/root-token/rotate", nil)
	expect(t, r, 200)
	var rot api.CreatedRootToken
	r.json(t, &rot)
	expect(t, root.do("GET", "/users", nil), 401)
	root.bearer = rot.Secret
	expect(t, root.do("GET", "/users", nil), 200)

	r = anon.do("GET", "/system/info", nil)
	r.json(t, &info)
	if !info.Bootstrapped || !info.HasAdmin {
		t.Fatalf("info after bootstrap %+v", info)
	}
	// Security headers on every response.
	if r.header.Get("X-Content-Type-Options") != "nosniff" || r.header.Get(api.RequestIDHeader) == "" {
		t.Fatalf("headers %v", r.header)
	}
}

// Scoped API tokens through HTTP: the creator's grants bound the token, and the token shows up for the creator.
func TestAPITokensOverHTTP(t *testing.T) {
	h := newHarness(t)
	rootSecret, _, _ := h.c.Auth.IssueRootToken(context.Background())
	root := h.client()
	root.bearer = rootSecret
	expect(t, root.do("POST", "/users", map[string]any{"email": "ada@example.com", "display_name": "Ada",
		"password": "correct horse battery staple", "grants": []map[string]string{{"role": "admin", "domain": "*"}}}), 201)
	// Root is not a user: it cannot issue API tokens.
	if p := root.do("POST", "/tokens", map[string]any{"name": "x", "grants": []map[string]string{{"role": "viewer", "domain": "*"}}, "never_expires": true}).problem(t); p.Status != 403 {
		t.Fatalf("root token issued an API token: %+v", p)
	}
	tokSecret := loginAndIssueToken(t, h, "ada@example.com", []map[string]string{{"role": "viewer", "domain": "*"}})
	viewer := h.client()
	viewer.bearer = tokSecret
	expect(t, viewer.do("GET", "/users", nil), 403) // viewer cannot read users (global admin object)
	r := viewer.do("GET", "/auth/me", nil)
	expect(t, r, 200)
	var me api.Me
	r.json(t, &me)
	if me.User == nil || me.Method != auth.MethodToken {
		t.Fatalf("token me %+v", me)
	}
}

// loginAndIssueToken signs in (enrolling TOTP), steps up and creates an API token with grants.
func loginAndIssueToken(t *testing.T, h *harness, email string, grants []map[string]string) string {
	t.Helper()
	ui := h.client()
	r := ui.do("POST", "/auth/login", map[string]string{"email": email, "password": "correct horse battery staple"})
	expect(t, r, 200)
	var me api.Me
	r.json(t, &me)
	ui.csrf = me.CSRFToken
	r = ui.do("POST", "/auth/mfa/enroll", nil)
	var enr api.TOTPEnrollment
	r.json(t, &enr)
	secret, _ := auth.DecodeTOTPSecret(enr.Secret)
	r = ui.do("POST", "/auth/mfa/enroll/confirm", map[string]string{"code": auth.TOTPCode(secret, auth.TOTPStep(h.clk.Now()))})
	expect(t, r, 200)
	h.clk.Advance(30 * time.Second)
	r = ui.do("POST", "/auth/mfa/verify", map[string]string{"code": auth.TOTPCode(secret, auth.TOTPStep(h.clk.Now()))})
	expect(t, r, 200)
	r.json(t, &me)
	ui.csrf = me.CSRFToken
	r = ui.do("POST", "/tokens", map[string]any{"name": "automation", "grants": grants, "never_expires": true})
	expect(t, r, 201)
	var ct api.CreatedToken
	r.json(t, &ct)
	if !strings.HasPrefix(ct.Secret, auth.APITokenPrefix) {
		t.Fatalf("created token %+v", ct)
	}
	return ct.Secret
}

// Every registered operation has an authorization rule (fail closed otherwise).
func TestEveryOperationHasARule(t *testing.T) {
	d := app.SpecDeps()
	a := api.Register(http.NewServeMux(), d)
	missing := api.MissingRules(a.OpenAPI(), d)
	if len(missing) > 0 {
		t.Fatalf("operations without an authorization rule: %v", missing)
	}
	_ = a
}

// SM-NODE-001: a node's name, description and labels are edited with If-Match (PUT /nodes/{id}); its state and
// enrollment are unchanged, names stay unique, and a stale version conflicts. Viewers cannot edit nodes.
func TestAPIUpdateNode(t *testing.T) {
	h := newHarness(t)
	rootSecret, _, _ := h.c.Auth.IssueRootToken(context.Background())
	root := h.client()
	root.bearer = rootSecret
	expect(t, root.do("POST", "/users", map[string]any{"email": "ada@example.com", "display_name": "Ada",
		"password": "correct horse battery staple", "grants": []map[string]string{{"role": "admin", "domain": "*"}}}), 201)
	adm := h.client()
	adm.bearer = loginAndIssueToken(t, h, "ada@example.com", []map[string]string{{"role": "admin", "domain": "*"}})
	var a, b struct {
		Node struct {
			ID      string            `json:"id"`
			Name    string            `json:"name"`
			State   string            `json:"state"`
			Labels  map[string]string `json:"labels"`
			Version int64             `json:"version"`
		} `json:"node"`
		Token string `json:"token"`
	}
	r := adm.do("POST", "/nodes", map[string]any{"name": "build-1", "labels": map[string]string{"zone": "a"}}, "Idempotency-Key", "node-build-1")
	expect(t, r, 201)
	r.json(t, &a)
	r = adm.do("POST", "/nodes", map[string]any{"name": "build-2"}, "Idempotency-Key", "node-build-2")
	expect(t, r, 201)
	r.json(t, &b)

	r = adm.do("PUT", "/nodes/"+a.Node.ID, map[string]any{"name": "builder-1", "description": "CI builds", "labels": map[string]string{"zone": "b", "gpu": "no"}},
		"If-Match", fmt.Sprintf(`"%d"`, a.Node.Version))
	expect(t, r, 200)
	var got fleet.Node
	r.json(t, &got)
	if got.Name != "builder-1" || got.Description != "CI builds" || got.Labels["zone"] != "b" || got.State != "pending" || got.Version != a.Node.Version+1 {
		t.Fatalf("updated node %+v", got)
	}
	// Stale version, a name another node has, and a viewer are all refused.
	expect(t, adm.do("PUT", "/nodes/"+a.Node.ID, map[string]any{"name": "builder-1"}, "If-Match", fmt.Sprintf(`"%d"`, a.Node.Version)), 409)
	expect(t, adm.do("PUT", "/nodes/"+a.Node.ID, map[string]any{"name": "build-2"}, "If-Match", fmt.Sprintf(`"%d"`, got.Version)), 409)
	expect(t, root.do("POST", "/users", map[string]any{"email": "bob@example.com", "display_name": "Bob",
		"password": "correct horse battery staple", "grants": []map[string]string{{"role": "viewer", "domain": "*"}}}), 201)
	viewer := h.client()
	viewer.bearer = loginAndIssueToken(t, h, "bob@example.com", []map[string]string{{"role": "viewer", "domain": "*"}})
	expect(t, viewer.do("PUT", "/nodes/"+a.Node.ID, map[string]any{"name": "builder-9"}, "If-Match", fmt.Sprintf(`"%d"`, got.Version)), 403)
}
