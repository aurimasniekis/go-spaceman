package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/memory"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// returnsWithin runs f and fails the test, without waiting for f, when it has not returned after d.
func returnsWithin(t *testing.T, d time.Duration, what string, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); f() }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s did not return within %v", what, d)
	}
}

func shortDeadlines(t *testing.T) {
	t.Helper()
	oc, op, og := checkpointDeadline, prepareDeadline, memory.GuardTimeout
	checkpointDeadline, prepareDeadline = 700*time.Millisecond, 700*time.Millisecond
	// The guard's own bound is out of the way: the agent's deadline is what these tests exercise.
	memory.GuardTimeout = time.Hour
	t.Cleanup(func() { checkpointDeadline, prepareDeadline, memory.GuardTimeout = oc, op, og })
}

func noIndexLock(t *testing.T, dir string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(dir, ".git", "index.lock")); err == nil {
		t.Fatal(".git/index.lock was left behind")
	}
}

// hang makes every git in the checkout block on a FIFO named by an include.path outside .git; it returns a function
// that makes git work again.
func hang(t *testing.T, dir string) (heal func()) {
	t.Helper()
	fifo := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, ".git", "config")
	orig, err := os.ReadFile(cfg) //nolint:gosec // test fixture
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, append(append([]byte{}, orig...), []byte("[include]\n\tpath = "+fifo+"\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	return func() {
		if err := os.WriteFile(cfg, orig, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// A FIFO .gitignore in an instance's checkout: the checkpoint and the next prepare return promptly, nothing is left
// locked, and the next checkpoint (once the FIFO is gone) publishes.
func TestMemoryFifoGitignore(t *testing.T) {
	e := newFFEnv(t)
	dir := e.checkout("ins_a")
	write(t, dir, "notes.md", "one\n")
	if err := syscall.Mkfifo(filepath.Join(dir, ".gitignore"), 0o600); err != nil {
		t.Fatal(err)
	}
	var err error
	returnsWithin(t, 20*time.Second, "checkpoint", func() { err = e.m.checkpointErr(e.ctx, ffSpace, ffRole, "ins_a", "idle") })
	if err == nil || !strings.Contains(err.Error(), "checkout refused") {
		t.Fatalf("checkpoint: %v", err)
	}
	returnsWithin(t, 20*time.Second, "prepare", func() {
		if _, perr := e.m.prepare(e.ctx, ffSpace, ffRole, "ins_a"); perr != nil {
			t.Error(perr)
		}
	})
	noIndexLock(t, dir)
	if err := os.Remove(filepath.Join(dir, ".gitignore")); err != nil {
		t.Fatal(err)
	}
	e.checkpoint("ins_a", "idle")
	if e.sharedHead() == "" || e.head(dir) != e.sharedHead() {
		t.Fatal("the next checkpoint did not publish")
	}
}

// A git that never returns cannot hold the collection lock past the deadlines: checkpoint (pctx) and prepare both end
// at theirs, and the next checkpoint works.
func TestMemoryDeadlinesBoundAHungGit(t *testing.T) {
	e := newFFEnv(t)
	dir := e.checkout("ins_a")
	shortDeadlines(t)
	heal := hang(t, dir)
	write(t, dir, "notes.md", "one\n")
	var err error
	returnsWithin(t, 20*time.Second, "checkpoint", func() { err = e.m.checkpointErr(e.ctx, ffSpace, ffRole, "ins_a", "idle") })
	if err == nil {
		t.Fatal("a hung checkpoint reported success")
	}
	returnsWithin(t, 20*time.Second, "prepare", func() { _, _ = e.m.prepare(e.ctx, ffSpace, ffRole, "ins_a") })
	noIndexLock(t, dir)
	heal()
	e.checkpoint("ins_a", "idle")
	if e.head(dir) != e.sharedHead() || read(t, dir, "notes.md") != "one\n" {
		t.Fatal("the next checkpoint did not publish")
	}
}

// prepare of a new instance waits for the collection lock and then runs under its own deadline: a hang in the
// collection's other work cannot extend it.
func TestPrepareHasItsOwnDeadline(t *testing.T) {
	e := newFFEnv(t)
	dir := e.checkout("ins_a")
	shortDeadlines(t)
	hang(t, dir)
	start := time.Now()
	returnsWithin(t, 20*time.Second, "prepare", func() { _, _ = e.m.prepare(e.ctx, ffSpace, ffRole, "ins_a") })
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("prepare took %v", d)
	}
}

// One instance whose git hangs uses up only its own deadline: the instances after it are still fast-forwarded.
func TestFastForwardIdleDeadlinePerInstance(t *testing.T) {
	e := newFFEnv(t)
	a, b, c := e.checkout("ins_a"), e.checkout("ins_b"), e.checkout("ins_c")
	e.idleRun("ins_b")
	e.idleRun("ins_c")
	shortDeadlines(t)
	hang(t, b)
	write(t, a, "notes.md", "from a\n")
	returnsWithin(t, 30*time.Second, "checkpoint", func() { e.checkpointAsync("ins_a") })
	if got := read(t, c, "notes.md"); got != "from a\n" {
		t.Fatalf("the instance after the hung one was not fast-forwarded: %q", got)
	}
	if e.head(c) != e.sharedHead() {
		t.Fatal("ins_c is not at the shared head")
	}
}

func TestProjectGitEnvSetsNoLazyFetch(t *testing.T) {
	for _, kv := range gitEnv() {
		if kv == "GIT_NO_LAZY_FETCH=1" {
			return
		}
	}
	t.Fatal("GIT_NO_LAZY_FETCH is not in the project git environment")
}

// The project runner also ends a git that outlives its context with SIGTERM (so git removes its lock files).
func TestProjectGitCancelSendsSigterm(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "term")
	bin := filepath.Join(dir, "git")
	script := "#!/bin/sh\ntrap 'touch " + marker + "; exit 0' TERM\nsleep 30 &\nwait\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}
	g := &projectGit{bin: bin}
	returnsWithin(t, 20*time.Second, "rawEnv", func() { _, _ = g.rawEnv(t.Context(), dir, 300*time.Millisecond, nil, "status") })
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("the git was not sent SIGTERM")
	}
}

// The worktree runner ends a git that outlives its context with SIGTERM too, so git can remove its lock files.
func TestPrepareWorktreeCancelSendsSigterm(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "term")
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\ntrap 'touch " + marker + "; exit 0' TERM\nsleep 30 &\nwait\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o700); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	returnsWithin(t, 20*time.Second, "prepareWorktree", func() {
		_, _ = prepareWorktree(ctx, dir, proto.Worktree{Repo: "api", Branch: "b"})
	})
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("the worktree git was not sent SIGTERM")
	}
}
