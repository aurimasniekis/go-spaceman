package harborpilot

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/harborpilot/hpfake"
)

func TestParseBaseURL(t *testing.T) {
	for _, tc := range []struct {
		raw              string
		insecure, exempt bool
		ok               bool
		want             string
	}{
		{"https://hp.example.com/", false, true, true, "https://hp.example.com"},
		{"https://hp.example.com/gw//", false, false, true, "https://hp.example.com/gw"},
		{"http://127.0.0.1:8080", false, true, true, "http://127.0.0.1:8080"},
		{"http://localhost:8080", false, true, true, "http://localhost:8080"},
		{"http://127.0.0.1:8080", false, false, false, ""}, // run-facing: no loopback exemption
		{"http://hp.internal", false, true, false, ""},
		{"http://hp.internal", true, false, true, "http://hp.internal"},
		{"https://user:pw@hp.example.com", false, true, false, ""},
		{"https://hp.example.com/?x=1", false, true, false, ""},
		{"https://hp.example.com/#f", false, true, false, ""},
		{"ftp://hp.example.com", true, true, false, ""},
		{"hp.example.com", false, true, false, ""},
		{"", false, true, false, ""},
		// Loopback look-alikes get no plain-http exemption.
		{"http://127.0.0.1.evil.com", false, true, false, ""},
		{"http://localhost.", false, true, false, ""},
		{"http://localhost.evil.com", false, true, false, ""},
		{"http://127.1", false, true, false, ""},
		{"http://0x7f000001", false, true, false, ""},
		{"http://2130706433", false, true, false, ""},
		{"http://0.0.0.0", false, true, false, ""},
		{"http://[::1%25eth0]", false, true, false, ""},
		{"http://evil.com#@127.0.0.1", false, true, false, ""},
		{"http://127.0.0.1@evil.com", false, true, false, ""},
		{"http://[::1]", false, true, true, "http://[::1]"},
		{"http://[::ffff:127.0.0.1]:8080", false, true, true, "http://[::ffff:127.0.0.1]:8080"},
		{"http://LOCALHOST:8080", false, true, true, "http://LOCALHOST:8080"},
	} {
		u, err := ParseBaseURL(tc.raw, tc.insecure, tc.exempt)
		if (err == nil) != tc.ok {
			t.Errorf("%q insecure=%v exempt=%v: err %v", tc.raw, tc.insecure, tc.exempt, err)
			continue
		}
		if tc.ok && u.String() != tc.want {
			t.Errorf("%q: %q, want %q", tc.raw, u.String(), tc.want)
		}
	}
}

// Every failure class SpaceMan distinguishes, from answers in Harbor Pilot 0.1.3's shapes; tokens never appear in an
// error, and the client keeps no more than a bounded, sanitised message.
func TestClientErrorClasses(t *testing.T) {
	const canary = "hpk_CANARYcanary0123456789"
	var status atomic.Int64
	var body atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(int(status.Load()))
		_, _ = w.Write([]byte(body.Load().(string)))
	}))
	defer srv.Close()
	c, err := NewClient(srv.URL, ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	gw := func(typ, msg, code string) string {
		cs := "null"
		if code != "" {
			cs = `"` + code + `"`
		}
		return `{"error":{"message":"` + msg + `","type":"` + typ + `","param":null,"code":` + cs + `}}`
	}
	for _, tc := range []struct {
		name   string
		status int
		body   string
		class  Class
		reason string
		retry  bool
	}{
		{"revoked parent", 401, gw("invalid_request_error", "invalid, expired or revoked gateway credential", "invalid_api_key"), ClassUnauthorized, "invalid, expired or revoked", false},
		{"no mint scope", 403, gw("permission_error", "invalid, expired or revoked gateway credential: parent credential lacks tokens:mint", ""), ClassLacksMint, "parent token lacks tokens:mint", false},
		{"child as parent", 403, gw("permission_error", "child credentials cannot mint further credentials", ""), ClassChildCannotMint, "child credential", false},
		{"other 403", 403, gw("permission_error", "no eligible account is available for model x", ""), ClassForbidden, "permission denied", false},
		{"invalid", 400, gw("invalid_request_error", "body.bound_refs.project_id: project must be the client's project or one of its allowed projects", ""), ClassInvalid, "allowed projects", false},
		{"older gateway", 400, gw("invalid_request_error", `invalid body: json: unknown field \"check_budget\"`, ""), ClassTooOld, "older than Harbor Pilot 0.1.3", false},
		{"not found", 404, gw("invalid_request_error", "no child credential with this id is owned by the caller", "model_not_found"), ClassNotFound, "not found", false},
		{"budget", 429, `{"error":{"type":"insufficient_quota","code":"budget_exceeded","budget_id":"bdg_1","message":"budget daily is exhausted for this day (1 of 1 USD) (bdg_1)","param":null}}`, ClassBudgetExceeded, "budget daily is exhausted", false},
		{"throttled", 429, gw("rate_limit_exceeded", "too many failed authentications", "rate_limit_exceeded"), ClassRateLimited, "rate limited", true},
		{"internal mint error", 502, gw("api_error", "could not mint the credential", ""), ClassUpstream, "status 502", true},
		{"problem+json", 403, `{"title":"Forbidden","status":403,"detail":"missing permission usage.read"}`, ClassForbidden, "missing permission usage.read", false},
		{"redirect", 302, ``, ClassProtocol, "redirects are not followed", false},
		{"unexpected", 418, `teapot`, ClassProtocol, "status 418", false},
		{"token in message", 400, gw("invalid_request_error", "bad token "+canary+" given", ""), ClassInvalid, "hpk_…", false},
	} {
		status.Store(int64(tc.status))
		body.Store(tc.body)
		_, _, err := c.Mint(context.Background(), canary, MintRequest{Name: "x"})
		var he *Error
		if !errors.As(err, &he) {
			t.Fatalf("%s: %T %v", tc.name, err, err)
		}
		if he.Class != tc.class || !strings.Contains(he.Reason(), tc.reason) || he.Retryable() != tc.retry || he.Status != tc.status {
			t.Errorf("%s: class %s status %d reason %q retry %v", tc.name, he.Class, he.Status, he.Reason(), he.Retryable())
		}
		if strings.Contains(he.Error(), canary) || strings.Contains(he.Error(), "CANARY") {
			t.Errorf("%s: the token is in the error: %q", tc.name, he.Error())
		}
		if tc.class == ClassBudgetExceeded && (he.BudgetID != "bdg_1" || he.Code != "budget_exceeded") {
			t.Errorf("budget refusal %+v", he)
		}
	}

	// A success without a credential is a protocol error, not a token.
	status.Store(201)
	body.Store(`{"credential":{},"token":""}`)
	if _, _, err := c.Mint(context.Background(), canary, MintRequest{Name: "x"}); !IsClass(err, ClassProtocol) {
		t.Fatalf("empty mint answer: %v", err)
	}
	status.Store(200)
	body.Store(`not json`)
	if _, err := c.BudgetStatus(context.Background(), "hpa_x"); !IsClass(err, ClassProtocol) {
		t.Fatalf("unreadable budget status: %v", err)
	}

	// Transport failures (the gateway is gone) are retryable and say nothing about the request.
	srv.Close()
	_, _, err = c.Mint(context.Background(), canary, MintRequest{Name: "x"})
	var te *Error
	if !errors.As(err, &te) || te.Class != ClassTransport || !te.Retryable() || strings.Contains(err.Error(), canary) || strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("transport failure: %v", err)
	}
}

// Redirects are never followed: a token is never sent to wherever a redirect points.
func TestClientDoesNotFollowRedirects(t *testing.T) {
	var hit atomic.Bool
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit.Store(true) }))
	defer elsewhere.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	c, _ := NewClient(srv.URL, ClientOptions{})
	if err := c.Revoke(context.Background(), "hpk_secret_parent", "crd_1"); !IsClass(err, ClassProtocol) {
		t.Fatalf("redirect: %v", err)
	}
	if hit.Load() {
		t.Fatal("the redirect was followed")
	}
}

// The mint body always carries check_budget (also when false): an older gateway refuses the field, which is how
// SpaceMan detects it. Every call has a timeout.
func TestClientMintBodyAndTimeout(t *testing.T) {
	f := hpfake.New(t)
	c, _ := NewClient(f.URL(), ClientOptions{})
	cred, tok, err := c.Mint(context.Background(), f.Parent, MintRequest{Name: "n", Scopes: RunScopes, TTLSeconds: 900, BoundRefs: map[string]string{"project_id": "prj_web"}})
	if err != nil || !strings.HasPrefix(tok, "hpk_") || cred.BindingID != "bnd_fake" {
		t.Fatalf("mint %+v %v", cred, err)
	}
	if !strings.Contains(f.Bodies[0], `"check_budget":false`) {
		t.Fatalf("mint body %s", f.Bodies[0])
	}
	if err := c.Revoke(context.Background(), f.Parent, cred.ID); err != nil {
		t.Fatal(err)
	}
	if err := c.Revoke(context.Background(), f.Parent, "crd_unknown"); !IsClass(err, ClassNotFound) {
		t.Fatalf("unknown credential: %v", err)
	}
	f.Set(func(f *hpfake.HP) { f.Old = true })
	if _, _, err := c.Mint(context.Background(), f.Parent, MintRequest{Name: "n"}); !IsClass(err, ClassTooOld) {
		t.Fatalf("old gateway: %v", err)
	}
	b, err := c.BudgetStatus(context.Background(), f.Service)
	if err != nil || len(b) != 1 || b[0].State != "ok" || b[0].LimitE10 != 10000000000 {
		t.Fatalf("budgets %+v %v", b, err)
	}
	p, err := c.GetProject(context.Background(), f.Service, "web-app")
	if err != nil || p.ID != "prj_web" || p.Spec.WorkspaceID != "wsp_1" {
		t.Fatalf("project %+v %v", p, err)
	}

	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(500 * time.Millisecond) }))
	defer slow.Close()
	sc, _ := NewClient(slow.URL, ClientOptions{Timeout: 50 * time.Millisecond})
	if _, err := sc.BudgetStatus(context.Background(), "hpa_x"); !IsClass(err, ClassTransport) || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("timeout: %v", err)
	}
	if _, err := NewClient("http://hp.example.com", ClientOptions{}); err == nil {
		t.Fatal("plain http to a remote host was accepted")
	}
}

func TestSanitize(t *testing.T) {
	long := strings.Repeat("x", 1000)
	if got := Sanitize(long); len([]rune(got)) != maxMessage {
		t.Fatalf("length %d", len([]rune(got)))
	}
	if got := Sanitize("a\nb\x1b[31mc\t hpa_SECRETsecret d"); got != "a b [31mc hpa_… d" {
		t.Fatalf("sanitised %q", got)
	}
}

// The default transport uses no proxy (it would see every token) and never dials link-local (cloud metadata),
// unspecified or multicast addresses; private and loopback addresses stay allowed.
func TestClientTransportGuard(t *testing.T) {
	t.Parallel()
	c, err := NewClient("https://hp.example.com", ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	tr, ok := c.hc.Transport.(*http.Transport)
	if !ok || tr.Proxy != nil || tr.DialContext == nil {
		t.Fatalf("default transport: proxy set %v, dial check %v", ok && tr.Proxy != nil, ok && tr.DialContext != nil)
	}
	for addr, denied := range map[string]bool{
		"169.254.169.254:80": true, "[fe80::1]:443": true, "0.0.0.0:443": true, "[::]:443": true, "224.0.0.1:443": true,
		"[ff02::1]:443": true, "[::ffff:169.254.169.254]:80": true, "0.1.2.3:443": true, "255.255.255.255:443": true,
		// AWS's IPv6 metadata address, and link-local IPv4 reached through NAT64 (both prefixes) and 6to4.
		"[fd00:ec2::254]:80": true, "[64:ff9b::a9fe:a9fe]:80": true, "[64:ff9b:1::a9fe:a9fe]:80": true, "[2002:a9fe:a9fe::1]:80": true,
		"[64:ff9b::e000:1]:443": true,
		// The same translation prefixes to ordinary addresses stay allowed, as does another unique-local address.
		"[64:ff9b::cb00:7107]:443": false, "[2002:cb00:7107::1]:443": false, "[fd00:ec2::253]:443": false, "[fd12::1]:443": false,
		"127.0.0.1:443": false, "[::1]:443": false, "10.0.0.5:443": false, "192.168.1.2:443": false, "203.0.113.7:443": false,
	} {
		if err := dialControl("tcp", addr, nil); (err != nil) != denied {
			t.Errorf("%s: %v, want denied=%v", addr, err, denied)
		}
	}
	for _, base := range []string{"https://169.254.169.254", "https://[fe80::1]", "https://0.0.0.0"} {
		c, err := NewClient(base, ClientOptions{Timeout: 2 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = c.Mint(context.Background(), "hpk_secret_parent", MintRequest{Name: "x"})
		var he *Error
		if !errors.As(err, &he) || he.Class != ClassTransport || !strings.Contains(he.Message, "link-local") || strings.Contains(err.Error(), "hpk_secret") {
			t.Errorf("%s: %v", base, err)
		}
	}
}
