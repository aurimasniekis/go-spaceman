package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/aurimasniekis/go-spaceman/internal/api"
	"github.com/aurimasniekis/go-spaceman/internal/app"
	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/config"
	"github.com/aurimasniekis/go-spaceman/internal/keyring"
)

var specOnce = sync.OnceValues(func() ([]byte, error) {
	a := api.Register(http.NewServeMux(), app.SpecDeps())
	return json.Marshal(a.OpenAPI())
})

// env is what one CLI invocation sees.
type env struct {
	server string
	token  string
	extra  map[string]string
	stdin  string
	tr     http.RoundTripper
}

type result struct {
	out, err string
	code     int
}

// newRootFor builds a root like cmd/spaceman's, without the local commands.
func newRootFor(a *App) *cobra.Command {
	root := &cobra.Command{Use: "spaceman", SilenceUsage: true, SilenceErrors: true}
	root.CompletionOptions.DisableDefaultCmd = true
	a.BindFlags(root)
	root.AddGroup(&cobra.Group{ID: GroupID, Title: "Remote"})
	root.AddCommand(a.Commands()...)
	return root
}

func (e env) app() *App {
	vars := map[string]string{EnvServer: e.server, EnvToken: e.token}
	for k, v := range e.extra {
		vars[k] = v
	}
	return &App{SpecSource: specOnce, Getenv: func(k string) string { return vars[k] }, Stdin: strings.NewReader(e.stdin),
		Transport: e.tr, PollInterval: 10 * time.Millisecond}
}

// run executes one CLI invocation in process.
func (e env) run(t *testing.T, args ...string) result {
	t.Helper()
	root := newRootFor(e.app())
	var out, errb bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errb)
	root.SetArgs(args)
	err := root.ExecuteContext(context.Background())
	if err != nil {
		PrintError(&errb, err)
	}
	return result{out: out.String(), err: errb.String(), code: ExitCode(err)}
}

// ok runs and requires exit code 0.
func (e env) ok(t *testing.T, args ...string) string {
	t.Helper()
	r := e.run(t, args...)
	if r.code != 0 {
		t.Fatalf("spaceman %s: exit %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), r.code, r.out, r.err)
	}
	return r.out
}

// code runs and requires an exit code.
func (e env) code(t *testing.T, want int, args ...string) result {
	t.Helper()
	r := e.run(t, args...)
	if r.code != want {
		t.Fatalf("spaceman %s: exit %d, want %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), r.code, want, r.out, r.err)
	}
	return r
}

// obj runs with -o json and decodes the object.
func (e env) obj(t *testing.T, args ...string) map[string]any {
	t.Helper()
	out := e.ok(t, append([]string{"-o", "json"}, args...)...)
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("spaceman %s: not JSON: %v\n%s", strings.Join(args, " "), err, out)
	}
	return m
}

func items(t *testing.T, m map[string]any, key string) []map[string]any {
	t.Helper()
	raw, _ := m[key].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		out = append(out, r.(map[string]any))
	}
	return out
}

func str(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

// central is a real in-process central (store, keyring, services, HTTP stack) on an httptest server.
type central struct {
	c    *app.Central
	srv  *httptest.Server
	root string // root token
}

func startCentral(t *testing.T) *central {
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
	srv := httptest.NewUnstartedServer(nil)
	cfg.PublicURL = "http://" + srv.Listener.Addr().String()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	clk := clock.NewFake(time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC))
	c, err := app.OpenCentral(context.Background(), cfg, slog.New(slog.DiscardHandler), app.Options{Clock: clk, Environ: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	srv.Config.Handler = c.Handler(c.APIDeps())
	srv.Start()
	t.Cleanup(srv.Close)
	tok, _, err := c.Auth.IssueRootToken(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return &central{c: c, srv: srv, root: tok}
}

func (c *central) env() env { return env{server: c.srv.URL, token: c.root} }

func writeFile(t *testing.T, name, content string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	return p
}

var _ = io.Discard
