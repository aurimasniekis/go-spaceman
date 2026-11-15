package memory

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// Automatic housekeeping (git gc --auto, git maintenance run --auto) that a fetch or commit starts runs inside the git
// call, never as a detached background process that keeps writing into the repository after the call returns.
func TestAutoHousekeepingIsSynchronous(t *testing.T) {
	t.Parallel()
	g, err := NewGit()
	if err != nil {
		t.Skip("git not installed")
	}
	ctx := context.Background()
	id := &Identity{Name: "n", Email: "n@x"}
	node, bare := t.TempDir(), filepath.Join(t.TempDir(), "control.git")
	run := func(dir string, args ...string) string {
		t.Helper()
		out, err := g.Run(ctx, dir, id, args...)
		must(t, err)
		return out
	}
	run(node, "init", "--quiet", "--initial-branch=main", ".")
	must(t, g.InitBare(ctx, bare))
	run(bare, "config", "gc.auto", "0")

	// Three fetches, each repacked into its own pack.
	prev := ""
	sync := func(i int) {
		t.Helper()
		must(t, os.WriteFile(filepath.Join(node, "f"+strconv.Itoa(i)), []byte(strconv.Itoa(i)), 0o600))
		run(node, "add", "-A")
		run(node, "commit", "--quiet", "-m", strconv.Itoa(i))
		b, err := g.Bundle(ctx, node, prev, "refs/heads/main", 1<<20)
		must(t, err)
		if _, err := g.Unbundle(ctx, bare, b, "refs/heads/main", "refs/heads/main"); err != nil {
			t.Fatal(err)
		}
		prev = g.RevParse(ctx, node, "HEAD")
	}
	for i := range 3 {
		sync(i)
		run(bare, "repack", "-q")
	}
	if n := packs(t, bare); n < 3 {
		t.Fatalf("setup: %d packs", n)
	}

	// The next fetch finds too many packs and starts automatic housekeeping, which must be done when it returns.
	run(bare, "config", "gc.auto", "1")
	run(bare, "config", "gc.autoPackLimit", "1")
	sync(3)
	if n := packs(t, bare); n != 1 {
		t.Errorf("%d packs right after the fetch: housekeeping ran detached", n)
	}
	if _, err := os.Stat(filepath.Join(bare, "gc.pid")); err == nil {
		t.Error("gc.pid left behind: housekeeping is still running")
	}
}

func packs(t *testing.T, repo string) int {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(repo, "objects", "pack", "*.pack"))
	must(t, err)
	return len(m)
}
