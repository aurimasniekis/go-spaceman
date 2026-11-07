// Package hptest runs a real Harbor Pilot gateway (`harbor-pilot serve --demo`, v0.1.1, docs/contracts/harbor-pilot.md)
// for opt-in tests. It is enabled with SPACEMAN_HARBOR_PILOT=1. It uses the binary at HARBOR_PILOT_BIN, or else installs
// the pinned release with `go install`. The gateway listens on loopback with a temporary data directory, and the test's
// cleanup stops it and deletes that directory.
//
// The demo's own `spaceman-runs` binding pins `spaceman.space` and allows no other projects, so it can't be used for
// SpaceMan's per-run tokens. Provision therefore creates the fixture the integration needs:
//   - a `spaceman-test` binding on two demo projects (`allowed_project_ids`) that pins only `spaceman.fleet`;
//   - a tiny blocking daily budget on the second project;
//   - a read-only service principal and its `hpa_` token;
//   - a parent credential with `tokens:mint`.
//
// The demo's upstreams are synthetic, so no provider credentials are involved.
package hptest

import (
	"bytes"
	"context"
	"debug/buildinfo"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Environment variables and the pinned release.
const (
	EnvOptIn = "SPACEMAN_HARBOR_PILOT"
	EnvBin   = "HARBOR_PILOT_BIN"
	Version  = "v0.1.1"
	Module   = "github.com/aurimasniekis/go-harbor-pilot/cmd/harbor-pilot"

	// The demo seed's public administrator (internal/demo/demo.go at v0.1.1). It only exists in demo.db.
	demoAdminEmail    = "admin@example.test"
	demoAdminPassword = "harbor-pilot-demo" //nolint:gosec // Harbor Pilot's documented, public demo credential

	// Names of the fixture Provision creates.
	BindingName = "spaceman-test"
	BudgetName  = "spaceman-test-tiny"
	SPName      = "spaceman-test-reader"
	FleetRef    = "test"
)

// What a cached binary's build information must say (Binary). Variables so tests can pin the test binary instead.
var (
	pinnedPath    = Module
	pinnedVersion = Version
)

// Scopes of the parent credential and the service principal.
var (
	ParentScopes = []string{"gateway:invoke", "gateway:models", "tokens:mint"}
	SPPerms      = []string{"usage.read", "config.read", "activity.read"}
)

// Gateway is a provisioned Harbor Pilot. Its tokens are generated at test time and exist only in its data directory.
type Gateway struct {
	URL                string // origin, e.g. http://127.0.0.1:41234 (Anthropic base; OpenAI base is URL+"/v1")
	DataDir            string
	BindingID          string
	ProjectID          string // the binding's default project (demo "web-app")
	AltProjectID       string // the second allowed project (demo "data-pipeline"), with the tiny blocking budget
	BudgetID           string
	ServicePrincipalID string
	ServiceToken       string // hpa_…: usage.read, config.read, activity.read
	ParentID           string
	ParentToken        string // hpk_…: gateway:invoke, gateway:models, tokens:mint

	hc *http.Client
}

// Enabled reports whether the opt-in is set.
func Enabled() bool { return os.Getenv(EnvOptIn) == "1" }

// Start runs and provisions a demo gateway for t, or skips t when the opt-in is not set.
func Start(t testing.TB) *Gateway {
	t.Helper()
	if !Enabled() {
		t.Skipf("set %s=1 (and optionally %s) to run against a real harbor-pilot serve --demo %s", EnvOptIn, EnvBin, Version)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	bin, err := Binary(ctx)
	if err != nil {
		t.Fatalf("hptest: %v", err)
	}
	dir := t.TempDir()
	g, stop, err := Run(ctx, bin, dir)
	t.Cleanup(stop) // registered after TempDir, so the process stops before its directory is removed
	if err != nil {
		t.Fatalf("hptest: %v\n%s", err, logTail(dir))
	}
	return g
}

// Binary returns HARBOR_PILOT_BIN, or installs the pinned release into the user cache directory (once) and returns it.
// A cached binary is reused only when its build information names the pinned module and version; otherwise it is
// reinstalled. HARBOR_PILOT_BIN is the operator's choice (for example a local build) and is used as is.
func Binary(ctx context.Context) (string, error) {
	if b := os.Getenv(EnvBin); b != "" {
		if _, err := os.Stat(b); err != nil { //nolint:gosec // the operator-chosen test binary
			return "", fmt.Errorf("%s: %w", EnvBin, err)
		}
		return b, nil
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(cache, "spaceman", "hptest", Version)
	bin := filepath.Join(dir, "harbor-pilot")
	if pinned(bin) == nil {
		return bin, nil
	}
	cmd := exec.CommandContext(ctx, "go", "install", Module+"@"+Version) //nolint:gosec // pinned module path and version
	cmd.Env = append(os.Environ(), "GOBIN="+dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("go install %s@%s: %w: %s", Module, Version, err, out)
	}
	if err := pinned(bin); err != nil {
		return "", fmt.Errorf("installed %s: %w", bin, err)
	}
	return bin, nil
}

// pinned checks that bin was built from the pinned module path and version.
func pinned(bin string) error {
	info, err := buildinfo.ReadFile(bin)
	if err != nil {
		return err
	}
	if info.Path != pinnedPath || info.Main.Version != pinnedVersion {
		return fmt.Errorf("built from %s %s, want %s %s", info.Path, info.Main.Version, pinnedPath, pinnedVersion)
	}
	return nil
}

// Run starts `bin serve --demo` on a free loopback port with data in dir, waits until it is ready and provisions it.
// stop is always non-nil and safe to call more than once.
func Run(ctx context.Context, bin, dir string) (*Gateway, func(), error) {
	noop := func() {}
	addr, err := freeAddr()
	if err != nil {
		return nil, noop, err
	}
	logf, err := os.Create(filepath.Join(dir, "serve.log")) //nolint:gosec // test-owned temp dir
	if err != nil {
		return nil, noop, err
	}
	data := filepath.Join(dir, "data")
	// Not CommandContext: the process outlives ctx (which only bounds start-up) until stop.
	cmd := exec.Command(bin, "serve", "--demo", "--listen", addr, "--data-dir", data) //nolint:gosec,noctx // test binary
	cmd.Stdout, cmd.Stderr = logf, logf
	setProcAttr(cmd) // dies with the test binary (Linux) and runs in its own process group
	if err := cmd.Start(); err != nil {
		_ = logf.Close()
		return nil, noop, err
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			signalGroup(cmd, syscall.SIGTERM)
			select {
			case <-exited:
			case <-time.After(10 * time.Second):
				signalGroup(cmd, syscall.SIGKILL)
				<-exited
			}
			_ = logf.Close()
		})
	}
	base := "http://" + addr
	if err := waitReady(ctx, base, exited); err != nil {
		stop()
		return nil, stop, err
	}
	g, err := Provision(ctx, base)
	if err != nil {
		stop()
		return nil, stop, err
	}
	g.DataDir = data
	return g, stop, nil
}

func freeAddr() (string, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0") //nolint:noctx // port probe
	if err != nil {
		return "", err
	}
	defer l.Close()
	return l.Addr().String(), nil
}

func waitReady(ctx context.Context, base string, exited <-chan struct{}) error {
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/readyz", nil)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-exited:
			return errors.New("harbor-pilot exited before it was ready")
		case <-ctx.Done():
			return fmt.Errorf("harbor-pilot not ready: %w", ctx.Err())
		case <-tick.C:
		}
	}
}

func logTail(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, "serve.log")) //nolint:gosec // test-owned temp dir
	if err != nil {
		return ""
	}
	if len(b) > 4096 {
		b = b[len(b)-4096:]
	}
	return string(b)
}

// admin is a signed-in control-plane session (cookie plus CSRF token).
type admin struct {
	base string
	hc   *http.Client
	csrf string
}

func (a *admin) do(ctx context.Context, method, path string, body, out any, want int) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.base+path, r)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", a.base)
	if a.csrf != "" {
		req.Header.Set("X-CSRF-Token", a.csrf)
	}
	resp, err := a.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != want {
		return fmt.Errorf("%s %s: status %d: %s", method, path, resp.StatusCode, errMessage(data))
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("%s %s: %w", method, path, err)
		}
	}
	return nil
}

// errMessage returns a Harbor Pilot error's message (OpenAI shape or problem+json), never a whole body.
func errMessage(b []byte) string {
	var e struct {
		Error struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
		Detail string `json:"detail"`
		Title  string `json:"title"`
	}
	if json.Unmarshal(b, &e) == nil {
		switch {
		case e.Error.Message != "":
			return strings.TrimSpace(e.Error.Code + " " + e.Error.Message)
		case e.Detail != "":
			return e.Detail
		case e.Title != "":
			return e.Title
		}
	}
	return "(unrecognised error body)"
}

type named struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (a *admin) lookup(ctx context.Context, path, name string) (string, error) {
	var l struct {
		Items []named `json:"items"`
	}
	if err := a.do(ctx, http.MethodGet, path, nil, &l, http.StatusOK); err != nil {
		return "", err
	}
	for _, it := range l.Items {
		if it.Name == name {
			return it.ID, nil
		}
	}
	return "", fmt.Errorf("%s: no %q (not the v0.1.1 demo seed?)", path, name)
}

// Provision signs in as the demo administrator at base and creates the fixture described in the package comment.
func Provision(ctx context.Context, base string) (*Gateway, error) {
	jar, _ := cookiejar.New(nil)
	a := &admin{base: base, hc: &http.Client{Jar: jar, Timeout: 30 * time.Second}}
	var sess struct {
		CSRF string `json:"csrf_token"`
	}
	if err := a.do(ctx, http.MethodPost, "/api/v1/session", map[string]string{"email": demoAdminEmail, "password": demoAdminPassword}, &sess, http.StatusOK); err != nil {
		return nil, fmt.Errorf("sign in: %w", err)
	}
	a.csrf = sess.CSRF
	g := &Gateway{URL: base, hc: &http.Client{Timeout: 30 * time.Second}}
	plan, err := a.lookup(ctx, "/api/v1/routing-plans", "team-default")
	if err != nil {
		return nil, err
	}
	if g.ProjectID, err = a.lookup(ctx, "/api/v1/projects", "web-app"); err != nil {
		return nil, err
	}
	if g.AltProjectID, err = a.lookup(ctx, "/api/v1/projects", "data-pipeline"); err != nil {
		return nil, err
	}
	var o named
	if err := a.do(ctx, http.MethodPost, "/api/v1/client-bindings", map[string]any{"name": BindingName, "spec": map[string]any{
		"display_name": "SpaceMan test", "owner_kind": "none", "owner_id": "", "plan_id": plan,
		"project_id": g.ProjectID, "allowed_project_ids": []string{g.ProjectID, g.AltProjectID},
		"expected_harness": "", "allowed_models": []string{},
		"limits":        map[string]any{"requests_per_minute": 0, "max_concurrency": 0, "max_request_bytes": 0},
		"trust":         map[string]any{"session_hints": false, "project_hints": false, "intent_hints": false},
		"external_refs": map[string]string{"spaceman.fleet": FleetRef}, "enabled": true,
	}}, &o, http.StatusCreated); err != nil {
		return nil, fmt.Errorf("binding: %w", err)
	}
	g.BindingID = o.ID
	if err := a.do(ctx, http.MethodPost, "/api/v1/budgets", map[string]any{"name": BudgetName, "spec": map[string]any{
		"scope": "project", "scope_id": g.AltProjectID, "period": "day", "limit": "0.000001", "currency": "USD",
		"cost_kinds": []string{"actual_api_charge", "estimated_api_charge"}, "enforcement": "block", "warn_at": 0.8,
		"timezone": "", "enabled": true,
	}}, &o, http.StatusCreated); err != nil {
		return nil, fmt.Errorf("budget: %w", err)
	}
	g.BudgetID = o.ID
	if err := a.do(ctx, http.MethodPost, "/api/v1/service-principals", map[string]any{"name": SPName, "permissions": SPPerms}, &o, http.StatusCreated); err != nil {
		return nil, fmt.Errorf("service principal: %w", err)
	}
	g.ServicePrincipalID = o.ID
	var spt struct {
		Plaintext string `json:"plaintext"`
	}
	if err := a.do(ctx, http.MethodPost, "/api/v1/service-principals/"+o.ID+"/tokens", map[string]string{"name": "hptest"}, &spt, http.StatusCreated); err != nil {
		return nil, fmt.Errorf("service principal token: %w", err)
	}
	g.ServiceToken = spt.Plaintext
	var par mintResponse
	if err := a.do(ctx, http.MethodPost, "/api/v1/client-bindings/"+g.BindingID+"/credentials", map[string]any{"name": "spaceman-parent", "scopes": ParentScopes}, &par, http.StatusCreated); err != nil {
		return nil, fmt.Errorf("parent credential: %w", err)
	}
	g.ParentID, g.ParentToken = par.Credential.ID, par.Token
	if !strings.HasPrefix(g.ParentToken, "hpk_") || !strings.HasPrefix(g.ServiceToken, "hpa_") {
		return nil, errors.New("unexpected token format (want hpk_ parent and hpa_ service token)")
	}
	return g, nil
}

// MintRequest is the body of POST /v1/harbor/credentials (exactly these fields; Harbor Pilot refuses unknown ones).
type MintRequest struct {
	Name       string            `json:"name"`
	Scopes     []string          `json:"scopes,omitempty"`
	TTLSeconds int               `json:"ttl_seconds,omitempty"`
	BoundRefs  map[string]string `json:"bound_refs,omitempty"`
}

// Credential is a minted child credential.
type Credential struct {
	ID        string            `json:"id"`
	BindingID string            `json:"binding_id"`
	ParentID  string            `json:"parent_id"`
	Scopes    []string          `json:"scopes"`
	BoundRefs map[string]string `json:"bound_refs"`
	ExpiresAt time.Time         `json:"expires_at"`
}

type mintResponse struct {
	Credential Credential `json:"credential"`
	Token      string     `json:"token"`
}

// Mint mints a child credential with the parent token and returns it with its token.
func (g *Gateway) Mint(ctx context.Context, in MintRequest) (Credential, string, error) {
	code, body, err := g.Do(ctx, http.MethodPost, "/v1/harbor/credentials", g.ParentToken, in)
	if err != nil {
		return Credential{}, "", err
	}
	if code != http.StatusCreated {
		return Credential{}, "", fmt.Errorf("mint: status %d: %s", code, errMessage(body))
	}
	var out mintResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return Credential{}, "", err
	}
	return out.Credential, out.Token, nil
}

// Revoke revokes a child credential with the parent token and returns the status (204 when revoked or already revoked).
func (g *Gateway) Revoke(ctx context.Context, id string) (int, error) {
	code, _, err := g.Do(ctx, http.MethodDelete, "/v1/harbor/credentials/"+id, g.ParentToken, nil)
	return code, err
}

// Do sends a request with a bearer token and returns the status and (at most 1 MiB of) the body.
func (g *Gateway) Do(ctx context.Context, method, path, token string, body any) (int, []byte, error) {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.URL+path, r)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := g.hc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, b, err
}
