package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/app"
	"github.com/aurimasniekis/go-spaceman/internal/cli"
	"github.com/aurimasniekis/go-spaceman/internal/config"
	"github.com/aurimasniekis/go-spaceman/internal/keyring"
	"github.com/aurimasniekis/go-spaceman/internal/store/storetest"
)

// The committed CLI reference (site/reference/cli) matches the commands of this build. SM_CLI_DOCS=1 rewrites it
// (make cli-docs).
func TestCLIReferenceUpToDate(t *testing.T) {
	t.Parallel()
	dir := filepath.Join("..", "..", "site", "reference", "cli")
	root := newRoot(nil)
	if os.Getenv("SM_CLI_DOCS") == "1" {
		if err := cli.WriteDocs(root, dir); err != nil {
			t.Fatal(err)
		}
	}
	want := cli.GenerateDocs(root)
	got, _ := filepath.Glob(filepath.Join(dir, "*.md"))
	names := []string{}
	for _, p := range got {
		names = append(names, filepath.Base(p))
	}
	sort.Strings(names)
	wantNames := []string{}
	for n := range want {
		wantNames = append(wantNames, n)
	}
	sort.Strings(wantNames)
	if strings.Join(names, ",") != strings.Join(wantNames, ",") {
		t.Fatalf("site/reference/cli has %v, the commands generate %v: run make cli-docs", names, wantNames)
	}
	for n, b := range want {
		have, _ := os.ReadFile(filepath.Join(dir, n))
		if !bytes.Equal(have, b) {
			t.Fatalf("site/reference/cli/%s is stale: run make cli-docs", n)
		}
	}
}

// The built binary against a real central (in this process, over loopback HTTP): exit codes, the token file, a create
// with its secret written to a file, and table, JSON and YAML output.
func TestBuiltBinaryAgainstCentral(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("builds the binary")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "spaceman")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	cfg := config.Default()
	cfg.Mode = config.ModeDevelopment
	cfg.DataDir = filepath.Join(dir, "data")
	cfg.KEK.File = filepath.Join(dir, "kek")
	k, _ := keyring.GenerateKEK()
	if err := keyring.WriteKEKFile(cfg.KEK.File, k); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(nil)
	cfg.PublicURL = "http://" + srv.Listener.Addr().String()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	storetest.Seed(t, cfg.DataDir)
	c, err := app.OpenCentral(context.Background(), cfg, slog.New(slog.DiscardHandler), app.Options{Environ: []string{}})
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
	tokFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokFile, []byte(tok+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	run := func(want int, args ...string) (string, string) {
		t.Helper()
		cmd := exec.Command(bin, args...)
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, cli.EnvServer + "=" + srv.URL, cli.EnvTokenFile + "=" + tokFile}
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		err := cmd.Run()
		code := 0
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		if code != want {
			t.Fatalf("spaceman %s: exit %d, want %d\n%s\n%s", strings.Join(args, " "), code, want, out.String(), errb.String())
		}
		return out.String(), errb.String()
	}
	run(0, "system", "info")
	secret := filepath.Join(dir, "n1.enroll")
	out, _ := run(0, "node", "create", "n1", "--secret-file", secret)
	if strings.Contains(out, "spaceman_en") {
		t.Fatalf("enrollment secret printed: %s", out)
	}
	if b, err := os.ReadFile(secret); err != nil || len(bytes.TrimSpace(b)) < 20 {
		t.Fatalf("secret file %v", err)
	}
	run(0, "space", "create", "alpha", "--node", "n1")
	out, _ = run(0, "-o", "json", "space", "get", "alpha")
	var sp map[string]any
	if err := json.Unmarshal([]byte(out), &sp); err != nil || sp["slug"] != "alpha" {
		t.Fatalf("json %s", out)
	}
	if out, _ = run(0, "-o", "yaml", "space", "list"); !strings.Contains(out, "slug: alpha") {
		t.Fatalf("yaml %s", out)
	}
	if out, _ = run(0, "space", "list"); !strings.Contains(out, "SLUG") || !strings.Contains(out, "alpha") {
		t.Fatalf("table %s", out)
	}
	run(cli.ExitUsage, "space", "get")
	run(cli.ExitUsage, "space", "list", "--no-such-flag")
	run(cli.ExitNotFound, "space", "get", "beta")
	run(cli.ExitConflict, "space", "update", "alpha", "--name", "A", "--if-match", "9")
	_, stderr := run(cli.ExitAuth, "token", "create", "x", "--grant", "viewer", "--never-expires")
	if !strings.Contains(stderr, "403") {
		t.Fatalf("root token cannot create API tokens: %s", stderr)
	}
	if err := os.Chmod(tokFile, 0o644); err != nil {
		t.Fatal(err)
	}
	run(cli.ExitAuth, "space", "list")
	run(0, "version")
}
