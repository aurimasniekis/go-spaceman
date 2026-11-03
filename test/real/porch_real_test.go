//go:build real

package real

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestRealPorchOIDC (SM-AUTH-001/002, SM-VER-002): SpaceMan signs in through a real Porch (go-porch, built from the
// sibling checkout named by SPACEMAN_PORCH_SRC into a temp dir; docs/contracts/porch.md) instead of the in-process
// IdP. Porch's upstream is its own test fixture `test/fakegoogle` with `-auto-login` (Porch only signs people in
// through an upstream provider), exactly as Porch's conformance stack uses it.
//
// The browser is a scripted HTTP client with a cookie jar. Porch's /login page is a single-page app; the one step a
// script cannot do is click its "Continue with Google" button, so the script follows that button's href
// (`${login_url}?return_to=…`, web/src/routes/login.tsx at v0.3.0), as Porch's own conformance login shim does.
// Everything else is Porch's server-side flow: authorization request, upstream login, session, step-up, code, iss.
//
// Checked: SpaceMan registered as a confidential Porch client with the groups disclosure; authorization with PKCE
// S256, nonce and the groups scope; RFC 9207 iss; JIT refused while the Porch user is in no allowed group; after the
// user joins the group, porch.groups maps to SpaceMan's admin grant (JIT provisioning keyed on Porch's stable sub);
// replayed callbacks refused; a step-up operation refused outside the re-authentication window, then allowed after a
// step-up through Porch (prompt=login, max_age=0, fresh auth_time, rotated session); leaving the group removes the
// grant on the next sign-in.
func TestRealPorchOIDC(t *testing.T) {
	src := os.Getenv("SPACEMAN_PORCH_SRC")
	if os.Getenv("SPACEMAN_REAL_PORCH") != "1" || src == "" {
		t.Skip("set SPACEMAN_REAL_PORCH=1 and SPACEMAN_PORCH_SRC to a go-porch checkout (make test-real-porch)")
	}
	if _, err := os.Stat(filepath.Join(src, "cmd", "porch")); err != nil {
		t.Skipf("no go-porch checkout at %s", src)
	}
	dir, err := os.MkdirTemp("/tmp", "smporch-")
	must(t, err)
	t.Cleanup(func() {
		if os.Getenv("SM_KEEP") == "" {
			_ = os.RemoveAll(dir)
		}
	})
	bin := filepath.Join(dir, "bin")
	porchVersion := porchBuild(t, src, bin)
	build(t, bin)
	t.Logf("go-porch %s from %s", porchVersion, src)

	pPort, pMgmt, gPort, smPort, smMgmt := freePort(t), freePort(t), freePort(t), freePort(t), freePort(t)
	// Porch on "localhost" and SpaceMan on "127.0.0.1": separate cookie hosts for the shared browser jar.
	porchURL := fmt.Sprintf("http://localhost:%d", pPort)
	fakeURL := fmt.Sprintf("http://127.0.0.1:%d", gPort)
	smURL := fmt.Sprintf("http://127.0.0.1:%d", smPort)

	// Porch's fake Google upstream (TEST ONLY fixture of go-porch), signing in Ada (hd=smith.example) automatically.
	upstreamSecret := porchRandom(t)
	fake := porchStart(t, filepath.Join(dir, "fakegoogle.log"), bin+"/fakegoogle", "-listen", fmt.Sprintf("127.0.0.1:%d", gPort),
		"-issuer", fakeURL, "-client-id", "porch", "-client-secret", upstreamSecret,
		"-redirect-uris", porchURL+"/login/google/callback", "-auto-login", "ada")
	porchStop(t, fake)
	waitHTTP(t, fakeURL+"/.well-known/openid-configuration")

	porchCfg := filepath.Join(dir, "porch.yaml")
	must(t, os.WriteFile(porchCfg, []byte(fmt.Sprintf(`mode: development
data_dir: %s/porch
public_url: %s
http: {listen: "127.0.0.1:%d"}
management: {listen: "127.0.0.1:%d"}
outbound: {allow_cidrs: [127.0.0.0/8]}
log: {level: info, format: text}
`, dir, porchURL, pPort, pMgmt)), 0o600))
	setupOut := porchExec(t, bin+"/porch", "admin", "setup-token", "-config", porchCfg)
	setupToken := regexp.MustCompile(`porch_setup_[A-Za-z0-9_-]+`).FindString(setupOut)
	if setupToken == "" {
		t.Fatal("porch admin setup-token printed no token")
	}
	porchSrv := porchStart(t, filepath.Join(dir, "porch.log"), bin+"/porch", "serve", "-config", porchCfg)
	porchStop(t, porchSrv)
	waitHTTP(t, fmt.Sprintf("http://127.0.0.1:%d/readyz", pMgmt))
	t.Cleanup(func() {
		if t.Failed() {
			for _, f := range []string{"porch.log", "fakegoogle.log", "central.log"} {
				b, _ := os.ReadFile(filepath.Join(dir, f))
				t.Logf("---- %s ----\n%s", f, tail(string(b), 4000))
			}
		}
	})

	// Porch bootstrap through its management API, as an operator would (Porch's test/conformance/bootstrap.py).
	pa := &porchAPI{t: t, base: porchURL, jar: porchJar(t)}
	var me struct {
		CSRF string `json:"csrf_token"`
	}
	pa.call("POST", "/setup", map[string]any{"token": setupToken, "display_name": "Porch Owner"}, &me, 201)
	pa.csrf = me.CSRF
	var prov struct {
		ID string `json:"id"`
	}
	pa.call("POST", "/identity-providers", map[string]any{"slug": "google", "kind": "google", "name": "Google", "issuer": fakeURL,
		"client_id": "porch", "client_secret": upstreamSecret}, &prov, 201)
	pa.call("POST", "/tenants", map[string]any{"slug": "smith", "name": "Smith family"}, nil, 201)
	pa.call("POST", "/tenants/smith/domain-rules", map[string]any{"domain": "smith.example", "provider_id": prov.ID,
		"evidence_kind": "workspace_hd", "mode": "jit"}, nil, 201)
	pa.call("POST", "/tenants/smith/groups", map[string]any{"slug": "spaceman-admins", "name": "SpaceMan administrators"}, nil, 201)
	var app struct {
		Client struct {
			ClientID      string   `json:"client_id"`
			AllowedScopes []string `json:"allowed_scopes"`
			RequirePKCE   bool     `json:"require_pkce"`
		} `json:"client"`
		ClientSecret string `json:"client_secret"`
	}
	pa.call("POST", "/tenants/smith/applications", map[string]any{"slug": "spaceman", "name": "SpaceMan", "kind": "oidc",
		"client": map[string]any{"client_type": "confidential", "redirect_uris": []string{smURL + "/api/v1/auth/oidc/callback"},
			"grant_types": []string{"authorization_code"}, "require_pkce": true,
			// The groups scope must be allowed explicitly: Porch's default is openid profile email offline_access.
			"allowed_scopes": []string{"openid", "profile", "email", "groups"}},
		"disclosure": map[string]any{"email": true, "groups": true, "roles": false, "entitlements": false}}, &app, 201)
	if app.Client.ClientID == "" || app.ClientSecret == "" || !app.Client.RequirePKCE {
		t.Fatalf("porch application: client_id %q, secret set %v, require_pkce %v", app.Client.ClientID, app.ClientSecret != "", app.Client.RequirePKCE)
	}

	// SpaceMan central (development mode allows the http://localhost issuer; the shortest re-authentication window).
	central := filepath.Join(dir, "central.yaml")
	must(t, os.WriteFile(central, []byte(fmt.Sprintf(`mode: development
data_dir: %s/central
public_url: %s
kek: {file: %s/kek}
http: {listen: "127.0.0.1:%d", management_listen: "127.0.0.1:%d"}
auth: {reauth_window: 1m}
log: {level: debug, format: text}
`, dir, smURL, dir, smPort, smMgmt)), 0o600))
	run(t, bin+"/spaceman", "keys", "init-kek", "--out", dir+"/kek")
	e := &env{t: t, dir: dir, bin: bin, url: smURL}
	for _, f := range strings.Fields(run(t, bin+"/spaceman", "-c", central, "admin", "bootstrap")) {
		if strings.HasPrefix(f, "spaceman_rt_") {
			e.root = f
		}
	}
	e.central = start(t, filepath.Join(dir, "central.log"), bin+"/spaceman", "-c", central, "serve")
	porchStop(t, e.central)
	waitHTTP(t, smURL+"/api/v1/system/info")

	var sp struct {
		ID string `json:"id"`
	}
	if st := e.api("POST", "/oidc/providers", map[string]any{"slug": "porch", "display_name": "Porch", "issuer": porchURL,
		"client_id": app.Client.ClientID, "client_secret": app.ClientSecret, "scopes": []string{"openid", "profile", "email", "groups"},
		"groups_claim": "porch.groups", "provisioning": "jit", "jit": map[string]any{"groups": []string{"spaceman-admins"},
			"email_domains": []string{}, "tenants": []string{}, "hosted_domains": []string{}},
		"allow_private_network": true, "enabled": true}, &sp); st != 201 {
		t.Fatalf("create provider: %d", st)
	}
	if st := e.api("POST", "/oidc/providers/"+sp.ID+"/mappings", map[string]any{"group_value": "spaceman-admins", "role": "admin", "domain": "*"}, nil); st != 201 {
		t.Fatalf("add mapping: %d", st)
	}

	b := &porchBrowser{t: t, porch: porchURL, sm: smURL, jar: porchJar(t)}

	// 1. Ada signs in. Porch enrolls her by its domain rule, but she is in no group yet: SpaceMan's JIT rule refuses.
	final, auth := b.signIn("/api/v1/auth/oidc/porch/start?return_to=" + url.QueryEscape("/spaces"))
	if auth.Get("code_challenge_method") != "S256" || auth.Get("code_challenge") == "" || auth.Get("nonce") == "" ||
		auth.Get("response_type") != "code" || auth.Get("scope") != "openid profile email groups" || auth.Get("client_id") != app.Client.ClientID {
		t.Fatalf("authorization request parameters: method %q scope %q", auth.Get("code_challenge_method"), auth.Get("scope"))
	}
	if !strings.HasPrefix(final, "/login?sso_error=") {
		t.Fatalf("sign-in without an allowed group should be refused, ended at %q", final)
	}
	if b.callbackIss != porchURL {
		t.Fatalf("authorization response iss %q, want %q (RFC 9207)", b.callbackIss, porchURL)
	}
	t.Logf("no allowed group: refused with %s (%s)", final, porchDenied(e))
	if b.me() != nil {
		t.Fatal("a refused sign-in left a SpaceMan session")
	}

	var members struct {
		Items []struct {
			UserID string `json:"user_id"`
		} `json:"items"`
	}
	pa.call("GET", "/tenants/smith/members", nil, &members, 200)
	if len(members.Items) != 1 {
		t.Fatalf("porch tenant members after Ada's first sign-in: %d", len(members.Items))
	}
	ada := members.Items[0].UserID
	var adaUser struct {
		PublicID string `json:"public_id"`
	}
	pa.call("GET", "/users/"+ada, nil, &adaUser, 200)

	// 2. Ada joins spaceman-admins in Porch. The next sign-in reuses her Porch session, carries porch.groups and
	// provisions her in SpaceMan with the mapped grant.
	pa.call("PUT", "/tenants/smith/groups/spaceman-admins/members/"+ada, nil, nil, 204)
	final, _ = b.signIn("/api/v1/auth/oidc/porch/start?return_to=" + url.QueryEscape("/spaces"))
	if final != "/spaces" {
		t.Fatalf("sign-in with the group ended at %q (%s; app scopes %v)", final, porchDenied(e), app.Client.AllowedScopes)
	}
	if b.upstreamVisits != 0 {
		t.Fatalf("Porch sent the browser upstream %d times although its session was live", b.upstreamVisits)
	}
	m := b.me()
	if m == nil || m.Method != "oidc" || m.Provider != "porch" || m.Limited || !slices.Contains(m.Domains, "*") {
		t.Fatalf("SSO session %+v", m)
	}
	var idl struct {
		Items []struct {
			Issuer    string `json:"issuer"`
			Subject   string `json:"subject"`
			LinkedVia string `json:"linked_via"`
		} `json:"items"`
	}
	b.api("GET", "/auth/identities", nil, &idl, 200)
	ids := idl.Items
	if len(ids) != 1 || ids[0].Issuer != porchURL || ids[0].Subject != adaUser.PublicID || ids[0].LinkedVia != "jit" || !strings.HasPrefix(ids[0].Subject, "u_") {
		t.Fatalf("identities %+v (Porch public_id %q)", ids, adaUser.PublicID)
	}
	t.Logf("signed in as Porch subject %s with the admin grant from porch.groups", ids[0].Subject)

	// A replayed callback is refused.
	if loc := b.get(b.lastCallback); loc != "/account?sso_error=validation_failed" && !strings.Contains(loc, "sso_error=") {
		t.Fatalf("replayed callback redirected to %q", loc)
	}

	// 3. Step-up. Past the re-authentication window a step-up operation is refused; a step-up through Porch
	// (prompt=login, max_age=0) rotates the session with a fresh auth_time, and the operation then succeeds.
	newUser := func(name string) map[string]any {
		return map[string]any{"email": name + "@example.com", "display_name": name, "password": "correct horse battery staple",
			"grants": []map[string]string{{"role": "viewer", "domain": "*"}}}
	}
	b.api("POST", "/users", newUser("carol"), nil, 201) // inside the window right after the SSO sign-in
	created := newUser("bob")
	t.Log("waiting past the 1-minute re-authentication window")
	time.Sleep(65 * time.Second)
	var prob struct {
		Code string `json:"code"`
	}
	if st := b.api("POST", "/users", created, &prob, 0); st != 401 || prob.Code != "reauth_required" {
		t.Fatalf("step-up operation outside the window: %d %+v", st, prob)
	}
	t.Logf("outside the window: refused with %q", prob.Code)
	before := m.SessionID
	final, auth = b.signIn("/api/v1/auth/oidc/porch/start?purpose=stepup&return_to=" + url.QueryEscape("/account"))
	if auth.Get("prompt") != "login" || auth.Get("max_age") != "0" || auth.Get("code_challenge_method") != "S256" {
		t.Fatalf("step-up authorization: prompt %q max_age %q", auth.Get("prompt"), auth.Get("max_age"))
	}
	if final != "/account" || b.upstreamVisits == 0 || !b.sawStepUp {
		t.Fatalf("step-up ended at %q (upstream visits %d, porch step-up seen %v)", final, b.upstreamVisits, b.sawStepUp)
	}
	m = b.me()
	if m == nil || m.SessionID == before || m.Method != "oidc" {
		t.Fatalf("session after step-up %+v (before %s)", m, before)
	}
	if st := b.api("POST", "/users", created, nil, 201); st != 201 {
		t.Fatalf("step-up operation after the step-up: %d", st)
	}
	var audit struct {
		Items []struct {
			Action string `json:"action"`
		} `json:"items"`
	}
	e.api("GET", "/audit?action=user.reauthenticated", nil, &audit)
	if !slices.ContainsFunc(audit.Items, func(a struct {
		Action string `json:"action"`
	}) bool {
		return a.Action == "user.reauthenticated"
	}) {
		t.Fatalf("no user.reauthenticated audit event: %+v", audit.Items)
	}
	t.Log("step-up through Porch: session rotated, step-up operation allowed, audited")

	// 4. Leaving the group in Porch removes the mapped grant at the next sign-in (grants are synced every login).
	pa.call("DELETE", "/tenants/smith/groups/spaceman-admins/members/"+ada, nil, nil, 204)
	final, _ = b.signIn("/api/v1/auth/oidc/porch/start?return_to=" + url.QueryEscape("/spaces"))
	if final != "/spaces" {
		t.Fatalf("sign-in after leaving the group ended at %q", final)
	}
	if m = b.me(); m == nil || slices.Contains(m.Domains, "*") {
		t.Fatalf("grant kept after leaving the Porch group: %+v", m)
	}
	t.Log("left the Porch group: the next SSO session has no mapped grant")
}

// porchDenied is the reason SpaceMan recorded for the last refused SSO sign-in.
func porchDenied(e *env) string {
	var a struct {
		Items []struct {
			Details map[string]any `json:"details"`
		} `json:"items"`
	}
	e.api("GET", "/audit?action=user.login.failed", nil, &a)
	if len(a.Items) == 0 {
		return "no denial audited"
	}
	return fmt.Sprintf("audited reason %v", a.Items[0].Details["reason"])
}

// porchBuild builds porch and Porch's fake Google fixture from a checkout into out (the checkout is only read).
func porchBuild(t *testing.T, src, out string) string {
	t.Helper()
	for _, c := range [][2]string{{"porch", "./cmd/porch"}, {"fakegoogle", "./test/fakegoogle"}} {
		cmd := exec.Command("go", "build", "-o", filepath.Join(out, c[0]), c[1])
		cmd.Dir = src
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOWORK=off", "GOFLAGS=-mod=readonly")
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", c[0], err, b)
		}
	}
	v, err := exec.Command("git", "-C", src, "describe", "--tags", "--always", "--dirty").Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(v))
}

func porchEnv() []string {
	var out []string
	for _, kv := range cleanEnv() {
		if !strings.HasPrefix(kv, "PORCH_") {
			out = append(out, kv)
		}
	}
	return out
}

// porchExec runs a command and returns its output without ever logging it (it can contain a setup token).
func porchExec(t *testing.T, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Env = porchEnv()
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s %s: %v", filepath.Base(name), args[0], err)
	}
	return string(out)
}

func porchStart(t *testing.T, logFile, name string, args ...string) *exec.Cmd {
	t.Helper()
	f, err := os.Create(logFile)
	must(t, err)
	cmd := exec.Command(name, args...)
	cmd.Env = porchEnv()
	cmd.Stdout, cmd.Stderr = f, f
	must(t, cmd.Start())
	return cmd
}

// porchStop stops a process this test started, at cleanup.
func porchStop(t *testing.T, cmd *exec.Cmd) {
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Signal(syscall.SIGTERM)
			_ = cmd.Wait()
		}
	})
}

func porchRandom(t *testing.T) string {
	b := make([]byte, 24)
	_, err := rand.Read(b)
	must(t, err)
	return hex.EncodeToString(b)
}

func porchJar(t *testing.T) *cookiejar.Jar {
	j, err := cookiejar.New(nil)
	must(t, err)
	return j
}

// porchAPI is an operator session on Porch's management API (/api/v1, CSRF header and Origin as Porch requires).
type porchAPI struct {
	t    *testing.T
	base string
	jar  *cookiejar.Jar
	csrf string
}

func (p *porchAPI) call(method, path string, body, out any, want int) {
	p.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, p.base+"/api/v1"+path, rd)
	req.Header.Set("Origin", p.base)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if p.csrf != "" {
		req.Header.Set("X-Porch-CSRF", p.csrf)
	}
	res, err := (&http.Client{Jar: p.jar, Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		p.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode != want {
		// Problem bodies only: request bodies (which carry secrets) are never echoed here.
		p.t.Fatalf("porch %s %s: %d %s", method, path, res.StatusCode, tail(string(b), 600))
	}
	if out != nil {
		must(p.t, json.Unmarshal(b, out))
	}
}

// porchBrowser is a scripted browser: one cookie jar across SpaceMan, Porch and the fake upstream, following
// redirects by hand so every hop can be inspected.
type porchBrowser struct {
	t              *testing.T
	porch, sm      string
	jar            *cookiejar.Jar
	csrf           string
	lastCallback   string
	callbackIss    string
	upstreamVisits int
	sawStepUp      bool
}

func (b *porchBrowser) client() *http.Client {
	return &http.Client{Jar: b.jar, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// get requests u and returns the Location of a redirect ("" otherwise).
func (b *porchBrowser) get(u string) string {
	b.t.Helper()
	res, err := b.client().Get(u)
	if err != nil {
		b.t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, res.Body)
	res.Body.Close()
	if res.StatusCode < 300 || res.StatusCode >= 400 {
		return ""
	}
	loc, err := res.Request.URL.Parse(res.Header.Get("Location"))
	if err != nil {
		b.t.Fatal(err)
	}
	if loc.Host == res.Request.URL.Host {
		return loc.RequestURI()
	}
	return loc.String()
}

// signIn starts at a SpaceMan path and follows the flow until SpaceMan redirects into its UI. It returns that UI
// path and the parameters of the authorization request SpaceMan sent to Porch.
func (b *porchBrowser) signIn(startPath string) (string, url.Values) {
	b.t.Helper()
	b.upstreamVisits, b.sawStepUp = 0, false
	var auth url.Values
	next := b.sm + startPath
	for hop := 0; hop < 25; hop++ {
		u, err := url.Parse(next)
		must(b.t, err)
		origin := u.Scheme + "://" + u.Host
		switch {
		case origin == b.porch && u.Path == "/oauth2/authorize":
			auth = u.Query()
		case origin == b.porch && u.Path == "/login" && u.Query().Get("stepup") == "" && u.Query().Get("error") == "":
			// The SPA's "Continue with Google" button.
			next = b.porch + "/login/google?return_to=" + url.QueryEscape(u.Query().Get("return_to"))
			continue
		case origin == b.porch && u.Path == "/login/google" && u.Query().Get("intent") == "stepup":
			b.sawStepUp = true
		case origin == b.porch:
		case origin == b.sm && u.Path == "/api/v1/auth/oidc/callback":
			b.lastCallback, b.callbackIss = next, u.Query().Get("iss")
		case origin == b.sm && strings.HasPrefix(u.Path, "/api/"):
		case origin == b.sm:
			return u.RequestURI(), auth
		default:
			b.upstreamVisits++
		}
		loc := b.get(next)
		if loc == "" {
			b.t.Fatalf("flow stopped at %s://%s%s without a redirect", u.Scheme, u.Host, u.Path)
		}
		if strings.HasPrefix(loc, "/") {
			loc = origin + loc
		}
		next = loc
	}
	b.t.Fatal("sign-in flow did not finish within 25 redirects")
	return "", nil
}

type porchMe struct {
	Method    string   `json:"method"`
	Provider  string   `json:"provider"`
	SessionID string   `json:"session_id"`
	CSRFToken string   `json:"csrf_token"`
	Limited   bool     `json:"limited"`
	Domains   []string `json:"domains"`
}

func (b *porchBrowser) me() *porchMe {
	b.t.Helper()
	var m porchMe
	if st := b.api("GET", "/auth/me", nil, &m, 0); st != 200 {
		return nil
	}
	b.csrf = m.CSRFToken
	return &m
}

// api calls SpaceMan's API with the browser session (want 0 accepts any status).
func (b *porchBrowser) api(method, path string, body, out any, want int) int {
	b.t.Helper()
	var rd io.Reader
	if body != nil {
		j, _ := json.Marshal(body)
		rd = bytes.NewReader(j)
	}
	req, _ := http.NewRequest(method, b.sm+"/api/v1"+path, rd)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", b.sm) // same-origin, as the UI's fetches are
	if b.csrf != "" {
		req.Header.Set("X-CSRF-Token", b.csrf)
	}
	res, err := b.client().Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if out != nil {
		_ = json.Unmarshal(raw, out)
	}
	if want != 0 && res.StatusCode != want {
		b.t.Fatalf("spaceman %s %s: %d %s", method, path, res.StatusCode, tail(string(raw), 600))
	}
	return res.StatusCode
}
