//go:build real

package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/harness"
	"github.com/aurimasniekis/go-spaceman/internal/harness/dist"
	"github.com/aurimasniekis/go-spaceman/internal/harness/dist/disttest"
)

// Node-managed Claude Code against the real release host (ADR-0077): the node resolves the requested version, downloads
// and verifies it, and the cached binary runs by absolute path with the updater variables, writing nothing under HOME.
// It downloads about 340 MB, so it is opt-in twice: the real build tag and SPACEMAN_REAL=1. SPACEMAN_REAL_CLAUDE_VERSION
// picks the version (default latest); SPACEMAN_REAL_CLAUDE_CACHE reuses a cache directory between runs (owned by you,
// mode 0700). It never touches an installed claude. Run:
// SPACEMAN_REAL=1 go test -tags real -run TestRealManagedClaude ./internal/agent/
func TestRealManagedClaude(t *testing.T) {
	if os.Getenv("SPACEMAN_REAL") != "1" {
		t.Skip("set SPACEMAN_REAL=1 to download Claude Code from the release host")
	}
	cache := os.Getenv("SPACEMAN_REAL_CLAUDE_CACHE")
	if cache == "" {
		cache = filepath.Join(t.TempDir(), "claude-code")
		t.Cleanup(func() { disttest.MakeWritable(cache) })
	}
	m, err := dist.New(dist.Options{Dir: cache, DownloadTimeout: 20 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	want := os.Getenv("SPACEMAN_REAL_CLAUDE_VERSION")
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()
	res, release, err := m.Acquire(ctx, want)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if !filepath.IsAbs(res.Path) || res.Fallback != "" {
		t.Fatalf("resolution %+v", res)
	}
	if problems := m.Verify(ctx, res.Version); len(problems) > 0 {
		t.Fatalf("verify: %v", problems)
	}
	home := t.TempDir()
	cmd := exec.CommandContext(ctx, res.Path, "--version") //nolint:gosec // the verified cached binary
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + home, "CLAUDE_CONFIG_DIR=" + filepath.Join(home, "cfg")}
	for _, k := range harness.ClaudeUpdaterOffEnv {
		cmd.Env = append(cmd.Env, k+"=1")
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s --version: %v\n%s", res.Path, err, out)
	}
	if !strings.Contains(string(out), res.Version) {
		t.Fatalf("--version printed %q, want %s", out, res.Version)
	}
	if _, err := os.Stat(filepath.Join(home, ".local")); !os.IsNotExist(err) {
		t.Fatalf("the run wrote under HOME/.local: %v", err)
	}
	t.Logf("requested %q ran %s from %s", res.Requested, res.Version, res.Path)
}
