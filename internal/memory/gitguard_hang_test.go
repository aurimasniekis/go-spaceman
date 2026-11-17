package memory

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// within runs f and fails the test, without waiting for f, when it has not returned after d.
func within(t *testing.T, d time.Duration, what string, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); f() }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s did not return within %v", what, d)
	}
}

func newCheckout(t *testing.T) (*Git, string) {
	t.Helper()
	g, err := NewGit()
	if err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	if _, err := g.Run(t.Context(), dir, nil, "init", "--quiet", "."); err != nil {
		t.Fatal(err)
	}
	return g, dir
}

func noLock(t *testing.T, dir string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(dir, ".git", "index.lock")); err == nil {
		t.Fatal(".git/index.lock was left behind")
	}
}

// A FIFO in the work tree (a .gitignore or any file, at any depth) is refused before git runs: git would block on it
// for ever while holding index.lock. Nothing is left behind and the checkout works once the FIFO is gone.
func TestRunRefusesFifoInWorkTree(t *testing.T) {
	for _, rel := range []string{".gitignore", ".gitattributes", "sub/deep/.gitignore", "notes.md"} {
		t.Run(rel, func(t *testing.T) {
			g, dir := newCheckout(t)
			must(t, os.MkdirAll(filepath.Join(dir, filepath.Dir(rel)), 0o700))
			must(t, syscall.Mkfifo(filepath.Join(dir, rel), 0o600))
			for _, sub := range []string{"add", "status", "commit", "reset"} {
				ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
				start := time.Now()
				_, err := g.Run(ctx, dir, &Identity{Name: "t", Email: "t@example.com"}, sub, "-A")
				cancel()
				if err == nil || !strings.Contains(err.Error(), "checkout refused") {
					t.Errorf("git %s: not refused by the guard: %v", sub, err)
				}
				if time.Since(start) > 5*time.Second {
					t.Errorf("git %s took %v", sub, time.Since(start))
				}
			}
			noLock(t, dir)
			must(t, os.Remove(filepath.Join(dir, rel)))
			must(t, os.WriteFile(filepath.Join(dir, "a.md"), []byte("a\n"), 0o600))
			if _, err := g.Run(t.Context(), dir, nil, "add", "-A"); err != nil {
				t.Fatalf("the next add: %v", err)
			}
		})
	}
}

// A git whose context ends while it holds index.lock gets SIGTERM first and removes the lock; SIGKILL would leave it
// and every later git in the checkout would fail. The FIFO is placed past the guard (g.run) to make git hang.
func TestRunCancelLeavesNoIndexLock(t *testing.T) {
	g, dir := newCheckout(t)
	must(t, os.WriteFile(filepath.Join(dir, "a.md"), []byte("a\n"), 0o600))
	must(t, syscall.Mkfifo(filepath.Join(dir, ".gitignore"), 0o600))
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	start := time.Now()
	within(t, 30*time.Second, "git add on a FIFO", func() {
		if _, err := g.run(ctx, dir, nil, nil, "add", "-A"); err == nil {
			t.Error("git add on a FIFO .gitignore succeeded")
		}
	})
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("took %v", d)
	}
	noLock(t, dir)
	must(t, os.Remove(filepath.Join(dir, ".gitignore")))
	if _, err := g.Run(t.Context(), dir, nil, "add", "-A"); err != nil {
		t.Fatalf("the next add: %v", err)
	}
}

// A git that ignores SIGTERM and whose grandchild holds the output pipes is killed after KillDelay and Wait returns.
func TestRunKillsAfterWaitDelay(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "git")
	must(t, os.WriteFile(bin, []byte("#!/bin/sh\ntrap '' TERM\nsleep 60 &\nwhile :; do sleep 1; done\n"), 0o700)) //nolint:gosec // test fixture
	old := KillDelay
	KillDelay = 200 * time.Millisecond
	t.Cleanup(func() { KillDelay = old })
	g := &Git{Bin: bin}
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	within(t, 15*time.Second, "a git that ignores SIGTERM", func() {
		if _, err := g.run(ctx, t.TempDir(), nil, nil, "status"); err == nil {
			t.Error("expected an error")
		}
	})
}

// The guard's own bounds: each of these hangs on a FIFO and must end at GuardTimeout.
func hangingInclude(t *testing.T, dir string) {
	t.Helper()
	fifo := filepath.Join(t.TempDir(), "fifo")
	must(t, syscall.Mkfifo(fifo, 0o600))
	f, err := os.OpenFile(filepath.Join(dir, ".git", "config"), os.O_APPEND|os.O_WRONLY, 0)
	must(t, err)
	_, err = f.WriteString("[include]\n\tpath = " + fifo + "\n")
	must(t, err)
	must(t, f.Close())
}

func shortGuard(t *testing.T) {
	t.Helper()
	old := GuardTimeout
	GuardTimeout = time.Second
	t.Cleanup(func() { GuardTimeout = old })
}

func TestCheckoutIdentityHasTimeout(t *testing.T) {
	g, dir := newCheckout(t)
	hangingInclude(t, dir)
	shortGuard(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	within(t, 20*time.Second, "CheckoutIdentity", func() {
		if err := g.CheckoutIdentity(ctx, dir); err == nil {
			t.Error("a hanging git was accepted")
		}
	})
}

func TestGitlinksHasTimeout(t *testing.T) {
	g, dir := newCheckout(t)
	must(t, syscall.Mkfifo(filepath.Join(dir, ".git", "index"), 0o600))
	shortGuard(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	within(t, 20*time.Second, "Gitlinks", func() {
		if err := g.Gitlinks(ctx, dir); err == nil {
			t.Error("a hanging git was accepted")
		}
	})
}

// The walk skips hooks and the LFS object store (of the checkout and of each submodule's git dir), and only those.
func TestRunSkipsHooksAndLFSObjects(t *testing.T) {
	g, dir := newCheckout(t)
	dgit := filepath.Join(dir, ".git")
	link := func(rel string) {
		must(t, os.MkdirAll(filepath.Join(dgit, filepath.Dir(rel)), 0o700))
		must(t, os.Symlink("/nonexistent", filepath.Join(dgit, rel)))
	}
	skipped := []string{"hooks/pre-commit", "lfs/objects/ab/cd/abcdef", "modules/x/hooks/pre-commit", "modules/x/lfs/objects/ab/cd/abcdef", "modules/a/b/hooks/h"}
	for _, rel := range skipped {
		link(rel)
	}
	if _, err := g.Run(t.Context(), dir, nil, "status", "--porcelain"); err != nil {
		t.Fatalf("a skipped folder was walked: %v", err)
	}
	for _, rel := range []string{"lfs/other", "modules/x/refs/heads/y", "lfs/tmp/x"} {
		link(rel)
		if _, err := g.Run(t.Context(), dir, nil, "status", "--porcelain"); err == nil {
			t.Errorf("a symlink at .git/%s was accepted", rel)
		}
		must(t, os.Remove(filepath.Join(dgit, rel)))
	}
}

func TestSkipGitDir(t *testing.T) {
	for rel, want := range map[string]bool{"hooks": true, "lfs/objects": true, "modules/x/hooks": true, "modules/x/lfs/objects": true,
		"modules/a/b/hooks": true, "refs": false, "lfs": false, "modules/x": false, "modules/x/refs": false, "worktrees/w/hooks": false} {
		if skipGitDir(rel) != want {
			t.Errorf("skipGitDir(%q) = %v", rel, !want)
		}
	}
}

// The node's own ignore and attributes files are not read, and a repository's config cannot name one: a FIFO there
// would block git.
func TestRunForcesExcludesAndAttributesFiles(t *testing.T) {
	for _, key := range []string{"core.excludesFile", "core.attributesFile"} {
		t.Run(key, func(t *testing.T) {
			g, dir := newCheckout(t)
			fifo := filepath.Join(t.TempDir(), "fifo")
			must(t, syscall.Mkfifo(fifo, 0o600))
			must(t, os.WriteFile(filepath.Join(dir, "a.md"), []byte("a\n"), 0o600))
			if _, err := g.Run(t.Context(), dir, nil, "config", key, fifo); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			within(t, 20*time.Second, "git add", func() {
				if _, err := g.Run(ctx, dir, nil, "add", "-A"); err != nil {
					t.Errorf("add: %v", err)
				}
			})
			noLock(t, dir)
		})
	}
}

// Both runners keep git from fetching missing objects lazily.
func TestRunSetsNoLazyFetch(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "git")
	must(t, os.WriteFile(bin, []byte("#!/bin/sh\nenv\n"), 0o700)) //nolint:gosec // test fixture
	out, err := (&Git{Bin: bin}).run(t.Context(), t.TempDir(), nil, nil, "status")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "GIT_NO_LAZY_FETCH=1\n") {
		t.Fatalf("GIT_NO_LAZY_FETCH is not set: %q", out)
	}
}
