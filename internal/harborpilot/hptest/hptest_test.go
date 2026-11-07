package hptest

import (
	"context"
	"debug/buildinfo"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeServeEnv makes the test binary act as a fake `harbor-pilot serve` (see TestMain), so Start and Run can be tested
// as a real child process without the real binary.
const fakeServeEnv = "HPTEST_FAKE_SERVE"

func TestMain(m *testing.M) {
	switch os.Getenv(fakeServeEnv) {
	case "serve":
		fakeServe()
	case "exit":
		os.Exit(3)
	case "sleep":
		time.Sleep(time.Minute)
		os.Exit(4)
	}
	os.Exit(m.Run())
}

func fakeServe() {
	addr := ""
	for i, a := range os.Args {
		if a == "--listen" && i+1 < len(os.Args) {
			addr = os.Args[i+1]
		}
	}
	l, err := net.Listen("tcp", addr) //nolint:noctx // test fake
	if err != nil {
		os.Exit(2)
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM)
	go func() { <-sig; os.Exit(0) }()
	_ = http.Serve(l, newFake()) //nolint:gosec // test fake on loopback
	os.Exit(1)
}

// selfTest is the contract check every caller relies on: a minted run token works on /v1/models, revoking it is
// idempotent, and a revoked token gets 401. Also: a project outside allowed_project_ids is refused, and the service
// token reads budget status.
func selfTest(t *testing.T, g *Gateway) {
	t.Helper()
	ctx := context.Background()
	refs := map[string]string{"spaceman.space": "spc_hptest", "spaceman.run": "run_hptest", "session": "run_hptest", "project_id": g.ProjectID}
	cred, tok, err := g.Mint(ctx, MintRequest{Name: "hptest self-test", Scopes: []string{"gateway:invoke", "gateway:models"}, TTLSeconds: 900, BoundRefs: refs})
	if err != nil {
		t.Fatal(err)
	}
	if cred.ID == "" || cred.ParentID != g.ParentID || cred.BindingID != g.BindingID || cred.BoundRefs["spaceman.run"] != "run_hptest" {
		t.Fatalf("minted credential %+v", cred)
	}
	if code, _, err := g.Do(ctx, http.MethodGet, "/v1/models", tok, nil); err != nil || code != http.StatusOK {
		t.Fatalf("models with a fresh token: %d %v", code, err)
	}
	for range 2 { // idempotent
		if code, err := g.Revoke(ctx, cred.ID); err != nil || code != http.StatusNoContent {
			t.Fatalf("revoke: %d %v", code, err)
		}
	}
	if code, _, err := g.Do(ctx, http.MethodGet, "/v1/models", tok, nil); err != nil || code != http.StatusUnauthorized {
		t.Fatalf("models with a revoked token: %d %v (want 401)", code, err)
	}
	refs["project_id"] = "prj_not_allowed"
	if _, _, err := g.Mint(ctx, MintRequest{Name: "hptest bad project", BoundRefs: refs}); err == nil || !strings.Contains(err.Error(), "status 400") {
		t.Fatalf("mint for a project outside allowed_project_ids: %v (want a 400)", err)
	}
	code, body, err := g.Do(ctx, http.MethodGet, "/api/v1/budgets/status", g.ServiceToken, nil)
	if err != nil || code != http.StatusOK || !strings.Contains(string(body), BudgetName) {
		t.Fatalf("budget status: %d %v", code, err)
	}
}

// TestRealHarborPilotSelfTest runs the self-test against a real `harbor-pilot serve --demo` (opt-in, make
// test-harbor-pilot).
func TestRealHarborPilotSelfTest(t *testing.T) {
	g := Start(t)
	selfTest(t, g)
	if _, err := os.Stat(filepath.Join(g.DataDir, "demo.db")); err != nil {
		t.Fatalf("demo database not in the temporary data dir: %v", err)
	}
}

// message sends one small Anthropic Messages request with tok and returns the status.
func message(t *testing.T, g *Gateway, tok string) int {
	t.Helper()
	code, _, err := g.Do(context.Background(), http.MethodPost, "/v1/messages", tok, map[string]any{
		"model": "coding/default", "max_tokens": 16, "messages": []map[string]any{{"role": "user", "content": "hello"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return code
}

// usageTest checks per-run usage (v0.1.2+, D-041): two runs' tokens bind different spaceman.run values, and the usage
// summary filtered on each value (and the binding) counts only that run's requests.
func usageTest(t *testing.T, g *Gateway) {
	t.Helper()
	ctx := context.Background()
	want := map[string]int64{"run_hptest_a": 1, "run_hptest_b": 2}
	for run, n := range want {
		tok := runToken(t, g, run, g.ProjectID)
		for range n {
			if code := message(t, g, tok); code != http.StatusOK {
				t.Fatalf("%s: messages status %d", run, code)
			}
		}
	}
	for run, n := range want {
		var u Usage
		deadline := time.Now().Add(15 * time.Second)
		for {
			var err error
			if u, err = g.UsageByRef(ctx, "spaceman.run", run); err != nil {
				t.Fatal(err)
			}
			if u.Attempts() >= n || time.Now().After(deadline) {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
		if u.Attempts() != n || u.Filters.Refs["spaceman.run"] != run || u.Filters.BindingID != g.BindingID {
			t.Fatalf("%s: usage %+v (want %d attempts, filters echoed)", run, u, n)
		}
	}
	if u, err := g.UsageByRef(ctx, "spaceman.run", "run_hptest_none"); err != nil || u.Attempts() != 0 {
		t.Fatalf("usage of a run without requests: %+v %v", u, err)
	}
}

// runToken mints a run's child token bound to project.
func runToken(t *testing.T, g *Gateway, run, project string) string {
	t.Helper()
	_, tok, err := g.Mint(context.Background(), MintRequest{Name: "hptest " + run, Scopes: []string{"gateway:invoke"}, TTLSeconds: 900,
		BoundRefs: map[string]string{"spaceman.space": "spc_hptest", "spaceman.run": run, "project_id": project}})
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// budgetTest checks budget refusals (v0.1.2+, D-040) and the mint-time budget check (v0.1.3, D-043):
//   - a request over the second project's tiny blocking budget gets 429 and is recorded as blocked (budget_exceeded),
//     with an access.request.blocked event for it;
//   - after usageTest spent on the first project, a tiny budget there refuses a check_budget mint with 429
//     budget_exceeded naming the budget, while a mint without check_budget still succeeds.
func budgetTest(t *testing.T, g *Gateway) {
	t.Helper()
	ctx := context.Background()
	const run = "run_hptest_over"
	if code := message(t, g, runToken(t, g, run, g.AltProjectID)); code != http.StatusTooManyRequests {
		t.Fatalf("request over a blocking budget: status %d (want 429)", code)
	}
	// The request row is written by Harbor Pilot's telemetry writer after the response, so wait for it.
	q := url.Values{"ref": {"spaceman.run:" + run}, "binding_id": {g.BindingID}}
	var reqs struct {
		Items []struct {
			ID        string `json:"id"`
			Status    string `json:"status"`
			ErrorKind string `json:"error_kind"`
		} `json:"items"`
	}
	var (
		code int
		body []byte
		err  error
	)
	// The request row and its event are written after the response: poll both within one deadline.
	deadline := time.Now().Add(15 * time.Second)
	for ; ; time.Sleep(200 * time.Millisecond) {
		code, body, err = g.Do(ctx, http.MethodGet, "/api/v1/activity/requests?"+q.Encode(), g.ServiceToken, nil)
		if err != nil || code != http.StatusOK || json.Unmarshal(body, &reqs) != nil || len(reqs.Items) > 0 || time.Now().After(deadline) {
			break
		}
	}
	if err != nil || code != http.StatusOK || len(reqs.Items) != 1 ||
		reqs.Items[0].Status != "blocked" || reqs.Items[0].ErrorKind != "budget_exceeded" {
		t.Fatalf("activity of the refused run: %d %v %+v", code, err, reqs)
	}
	var evs struct {
		Items []struct {
			Envelope struct {
				Name     string            `json:"name"`
				Subjects map[string]string `json:"subjects"`
			} `json:"envelope"`
		} `json:"items"`
	}
	blocked := false
	for ; !blocked; time.Sleep(200 * time.Millisecond) {
		code, body, err = g.Do(ctx, http.MethodGet, "/api/v1/events?limit=50", g.ServiceToken, nil)
		if err != nil || code != http.StatusOK || json.Unmarshal(body, &evs) != nil {
			t.Fatalf("events: %d %v", code, err)
		}
		for _, e := range evs.Items {
			blocked = blocked || (e.Envelope.Name == "access.request.blocked" && e.Envelope.Subjects["request_id"] == reqs.Items[0].ID &&
				e.Envelope.Subjects["binding_id"] == g.BindingID)
		}
		if time.Now().After(deadline) {
			break
		}
	}
	if !blocked {
		t.Fatal("no access.request.blocked event for the budget refusal")
	}

	id, err := g.CreateBudget(ctx, "spaceman-test-spent", g.ProjectID, "0.000001")
	if err != nil {
		t.Fatal(err)
	}
	refs := map[string]string{"spaceman.run": "run_hptest_check", "project_id": g.ProjectID}
	if _, _, err := g.Mint(ctx, MintRequest{Name: "hptest check_budget", BoundRefs: refs, CheckBudget: true}); err == nil ||
		!strings.Contains(err.Error(), "status 429") || !strings.Contains(err.Error(), "budget_exceeded") || !strings.Contains(err.Error(), id) {
		t.Fatalf("check_budget mint over a used-up budget: %v (want 429 budget_exceeded naming %s)", err, id)
	}
	if _, _, err := g.Mint(ctx, MintRequest{Name: "hptest no check", BoundRefs: refs}); err != nil {
		t.Fatalf("mint without check_budget: %v", err)
	}
}

// TestRealHarborPilotRunAccounting checks per-run usage and budget refusals against a real `harbor-pilot serve --demo`
// (opt-in, make test-harbor-pilot).
func TestRealHarborPilotRunAccounting(t *testing.T) {
	g := Start(t)
	usageTest(t, g)
	budgetTest(t, g)
}

func TestStartSkipsWithoutOptIn(t *testing.T) {
	t.Setenv(EnvOptIn, "")
	reached := false
	t.Run("skip", func(t *testing.T) {
		Start(t)
		reached = true
	})
	if reached {
		t.Fatal("Start did not skip without the opt-in")
	}
}

func TestStartWithFakeBinary(t *testing.T) {
	t.Setenv(EnvOptIn, "1")
	t.Setenv(EnvBin, os.Args[0])
	t.Setenv(fakeServeEnv, "serve")
	g := Start(t)
	if !strings.HasPrefix(g.URL, "http://127.0.0.1:") {
		t.Fatalf("not on loopback: %s", g.URL)
	}
	selfTest(t, g)
}

func TestRunFailsWhenTheBinaryExits(t *testing.T) {
	t.Setenv(fakeServeEnv, "exit")
	_, stop, err := Run(context.Background(), os.Args[0], t.TempDir())
	stop()
	if err == nil || !strings.Contains(err.Error(), "exited before it was ready") {
		t.Fatalf("err %v", err)
	}
	if _, stop, err := Run(context.Background(), filepath.Join(t.TempDir(), "missing"), t.TempDir()); err == nil {
		t.Fatal("started a missing binary")
	} else {
		stop()
	}
	if _, _, err := Run(context.Background(), os.Args[0], filepath.Join(t.TempDir(), "missing", "dir")); err == nil {
		t.Fatal("ran without a log file")
	}
}

func TestRunFailsWhenNotReadyInTime(t *testing.T) {
	t.Setenv(fakeServeEnv, "serve")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, stop, err := Run(ctx, os.Args[0], t.TempDir()); err == nil {
		t.Fatal("ready with a cancelled context")
	} else {
		stop()
		stop() // safe twice
	}
}

func TestBinary(t *testing.T) {
	t.Setenv(EnvBin, os.Args[0])
	if b, err := Binary(context.Background()); err != nil || b != os.Args[0] {
		t.Fatalf("%q %v", b, err)
	}
	t.Setenv(EnvBin, filepath.Join(t.TempDir(), "missing"))
	if _, err := Binary(context.Background()); err == nil {
		t.Fatal("accepted a missing HARBOR_PILOT_BIN")
	}
	// Without HARBOR_PILOT_BIN, an installed copy in the cache is reused when its build information is the pinned one
	// (no network: the cache is pre-filled with the test binary, and the test binary's build is pinned).
	self, err := buildinfo.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	pin(t, self.Path, self.Main.Version)
	t.Setenv(EnvBin, "")
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("HOME", cache)
	want := filepath.Join(cache, "spaceman", "hptest", Version, "harbor-pilot")
	if err := os.MkdirAll(filepath.Dir(want), 0o700); err != nil {
		t.Fatal(err)
	}
	copyFile(t, os.Args[0], want)
	if b, err := Binary(context.Background()); err != nil || b != want {
		t.Fatalf("%q %v (want %q)", b, err, want)
	}
	// A cached file that isn't the pinned build is reinstalled, and a failing install is reported (no go on PATH).
	if err := os.WriteFile(want, []byte("not a binary"), 0o600); err != nil {
		t.Fatal(err)
	}
	cp, err := exec.LookPath("cp")
	if err != nil {
		t.Skip("no cp")
	}
	t.Setenv("PATH", t.TempDir())
	if _, err := Binary(context.Background()); err == nil || !strings.Contains(err.Error(), "go install") {
		t.Fatalf("err %v", err)
	}
	// An install that leaves a binary of another version is refused.
	tools := t.TempDir()
	script := "#!/bin/sh\n" + cp + " '" + os.Args[0] + "' \"$GOBIN/harbor-pilot\"\n"
	if err := os.WriteFile(filepath.Join(tools, "go"), []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(tools, "go"), 0o700); err != nil { //nolint:gosec // test-owned fake toolchain
		t.Fatal(err)
	}
	t.Setenv("PATH", tools)
	pin(t, Module, Version)
	if _, err := Binary(context.Background()); err == nil || !strings.Contains(err.Error(), "want "+Module+" "+Version) {
		t.Fatalf("err %v", err)
	}
}

func pin(t *testing.T, path, version string) {
	t.Helper()
	oldPath, oldVersion := pinnedPath, pinnedVersion
	pinnedPath, pinnedVersion = path, version
	t.Cleanup(func() { pinnedPath, pinnedVersion = oldPath, oldVersion })
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	b, err := os.ReadFile(from) //nolint:gosec // the test binary
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, b, 0o700); err != nil { //nolint:gosec // an executable copy in a test-owned dir
		t.Fatal(err)
	}
}

func TestProvisionReportsEachStep(t *testing.T) {
	for _, path := range []string{"/api/v1/session", "/api/v1/client-bindings", "/api/v1/budgets", "/api/v1/service-principals",
		"/api/v1/service-principals/pri_1/tokens", "/api/v1/client-bindings/cbn_1/credentials"} {
		f := newFake()
		f.fail = path
		srv := httptest.NewServer(f)
		_, err := Provision(context.Background(), srv.URL)
		srv.Close()
		if err == nil || !strings.Contains(err.Error(), "status 500") || !strings.Contains(err.Error(), "injected") {
			t.Fatalf("%s: err %v", path, err)
		}
	}
}

func TestProvisionAgainstAnUnknownSeed(t *testing.T) {
	mux := http.NewServeMux()
	f := newFake()
	mux.Handle("/", f)
	mux.HandleFunc("/api/v1/projects", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"items": []named{}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	if _, err := Provision(context.Background(), srv.URL); err == nil || !strings.Contains(err.Error(), "web-app") {
		t.Fatalf("err %v", err)
	}
	if _, err := Provision(context.Background(), "http://127.0.0.1:1"); err == nil {
		t.Fatal("provisioned an unreachable gateway")
	}
}

func TestMintAndRevokeErrors(t *testing.T) {
	srv := httptest.NewServer(newFake())
	defer srv.Close()
	g, err := Provision(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	g.ParentToken = "hpk_wrong"
	if _, _, err := g.Mint(context.Background(), MintRequest{Name: "x"}); err == nil || !strings.Contains(err.Error(), "invalid_api_key") {
		t.Fatalf("mint with a bad parent: %v", err)
	}
	if code, err := g.Revoke(context.Background(), "crd_x"); err != nil || code != http.StatusUnauthorized {
		t.Fatalf("revoke with a bad parent: %d %v", code, err)
	}
	if _, _, err := g.Do(context.Background(), http.MethodPost, "/", "", func() {}); err == nil {
		t.Fatal("encoded a func")
	}
	if _, _, err := g.Do(context.Background(), "bad method", "/", "", nil); err == nil {
		t.Fatal("sent a bad method")
	}
	g.URL = "http://127.0.0.1:1"
	if _, _, err := g.Mint(context.Background(), MintRequest{Name: "x"}); err == nil {
		t.Fatal("minted against an unreachable gateway")
	}
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("not json"))
	}))
	defer srv2.Close()
	g.URL = srv2.URL
	if _, _, err := g.Mint(context.Background(), MintRequest{Name: "x"}); err == nil {
		t.Fatal("decoded a non-JSON mint response")
	}
}

func TestUsageByRefAndCreateBudget(t *testing.T) {
	srv := httptest.NewServer(newFake())
	defer srv.Close()
	ctx := context.Background()
	g, err := Provision(ctx, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	u, err := g.UsageByRef(ctx, "spaceman.run", "run_1")
	if err != nil || u.Attempts() != 3 || u.Filters.Refs["spaceman.run"] != "run_1" || u.Filters.BindingID != g.BindingID {
		t.Fatalf("%+v %v", u, err)
	}
	if id, err := g.CreateBudget(ctx, "b", g.ProjectID, "1"); err != nil || id == "" {
		t.Fatalf("%q %v", id, err)
	}
	g.ServiceToken = "hpa_wrong"
	if _, err := g.UsageByRef(ctx, "spaceman.run", "run_1"); err == nil || !strings.Contains(err.Error(), "status 401") {
		t.Fatalf("usage with a bad service token: %v", err)
	}
	g.admin.csrf = "wrong"
	if _, err := g.CreateBudget(ctx, "b", g.ProjectID, "1"); err == nil || !strings.Contains(err.Error(), "status 403") {
		t.Fatalf("budget without CSRF: %v", err)
	}
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("not json")) }))
	defer srv2.Close()
	g.URL = srv2.URL
	if _, err := g.UsageByRef(ctx, "spaceman.run", "run_1"); err == nil {
		t.Fatal("decoded a non-JSON summary")
	}
	g.URL = "http://127.0.0.1:1"
	if _, err := g.UsageByRef(ctx, "spaceman.run", "run_1"); err == nil {
		t.Fatal("read usage from an unreachable gateway")
	}
}

func TestAdminErrors(t *testing.T) {
	a := &admin{base: "http://127.0.0.1:1", hc: &http.Client{Timeout: time.Second}}
	if err := a.do(context.Background(), http.MethodPost, "/", func() {}, nil, http.StatusOK); err == nil {
		t.Fatal("encoded a func")
	}
	if err := a.do(context.Background(), "bad method", "/", nil, nil, http.StatusOK); err == nil {
		t.Fatal("sent a bad method")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("not json")) }))
	defer srv.Close()
	a.base = srv.URL
	var out map[string]any
	if err := a.do(context.Background(), http.MethodGet, "/", nil, &out, http.StatusOK); err == nil {
		t.Fatal("decoded a non-JSON body")
	}
	if _, err := a.lookup(context.Background(), "/", "x"); err == nil {
		t.Fatal("looked up in a non-JSON list")
	}
}

func TestErrMessageNeverReturnsBodies(t *testing.T) {
	for in, want := range map[string]string{
		`{"error":{"message":"parent lacks tokens:mint","type":"permission_error","code":"permission_error"}}`: "permission_error parent lacks tokens:mint",
		`{"title":"Bad Request","detail":"name is required"}`:                                                  "name is required",
		`{"title":"Not Found"}`:       "Not Found",
		`secret-looking text hpk_abc`: "(unrecognised error body)",
	} {
		if got := errMessage([]byte(in)); got != want {
			t.Errorf("errMessage(%s) = %q, want %q", in, got, want)
		}
	}
}

func TestLogTail(t *testing.T) {
	dir := t.TempDir()
	if logTail(dir) != "" {
		t.Fatal("tail of a missing log")
	}
	if err := os.WriteFile(filepath.Join(dir, "serve.log"), []byte(strings.Repeat("x", 5000)+"end"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := logTail(dir); len(got) != 4096 || !strings.HasSuffix(got, "end") {
		t.Fatalf("tail %d", len(got))
	}
}
