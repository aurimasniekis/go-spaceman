package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.test", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.test")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// SM-REPO-002/006: each writer gets its own worktree; existing worktrees are reused; nothing is overwritten.
func TestPrepareWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	ctx := context.Background()
	space := t.TempDir()
	repo := filepath.Join(space, "repos", "api")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "add", ".")
	gitRun(t, repo, "commit", "-q", "-m", "init")

	dir, err := prepareWorktree(ctx, space, proto.Worktree{Repo: "repos/api", Branch: "spaceman/tsk-1"})
	if err != nil {
		t.Fatal(err)
	}
	if dir != filepath.Join(space, "worktrees", "spaceman-tsk-1") || gitRun(t, dir, "branch", "--show-current") != "spaceman/tsk-1" {
		t.Fatalf("worktree %s", dir)
	}
	// Work in progress survives a relaunch: the worktree is reused, not reset.
	if err := os.WriteFile(filepath.Join(dir, "wip.txt"), []byte("unsaved"), 0o600); err != nil {
		t.Fatal(err)
	}
	again, err := prepareWorktree(ctx, space, proto.Worktree{Repo: "repos/api", Branch: "spaceman/tsk-1"})
	if err != nil || again != dir {
		t.Fatalf("reuse: %s %v", again, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "wip.txt")); string(b) != "unsaved" {
		t.Fatal("work in progress lost")
	}
	// An existing branch is checked out in a new worktree; a second writer on it is refused by git.
	gitRun(t, repo, "branch", "feature/x")
	if _, err := prepareWorktree(ctx, space, proto.Worktree{Repo: "repos/api", Branch: "feature/x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareWorktree(ctx, space, proto.Worktree{Repo: "repos/api", Branch: "main"}); err == nil {
		t.Fatal("checking out main twice must fail")
	}
	// A foreign folder at the worktree path is never overwritten; a missing repository is a clear error.
	if err := os.MkdirAll(filepath.Join(space, "worktrees", "taken"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareWorktree(ctx, space, proto.Worktree{Repo: "repos/api", Branch: "taken"}); err == nil || !strings.Contains(err.Error(), "does not overwrite") {
		t.Fatalf("foreign folder: %v", err)
	}
	if _, err := prepareWorktree(ctx, space, proto.Worktree{Repo: "repos/none", Branch: "b"}); err == nil || !strings.Contains(err.Error(), "not a git repository") {
		t.Fatalf("missing repo: %v", err)
	}
	if _, err := prepareWorktree(ctx, space, proto.Worktree{Repo: "repos/api", Branch: "from-base", Base: "nope"}); err == nil {
		t.Fatal("unknown base must fail")
	}
}

// Security review SR-M03/SR-M04: an agent that can write a repository's .git cannot make the node daemon run its code
// when the daemon prepares a writer worktree: hooks are not run, and repository configuration that names a program
// (here a clean/smudge filter) makes the daemon refuse git there.
func TestPrepareWorktreeRunsNoRepositoryCode(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	ctx := context.Background()
	space := t.TempDir()
	repo := filepath.Join(space, "repos", "api")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "add", ".")
	gitRun(t, repo, "commit", "-q", "-m", "init")
	marker := filepath.Join(space, "ran")
	hook := filepath.Join(repo, ".git", "hooks", "post-checkout")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o700); err != nil { //nolint:gosec // the hook under test
		t.Fatal(err)
	}
	if _, err := prepareWorktree(ctx, space, proto.Worktree{Repo: "repos/api", Branch: "spaceman/tsk-hook"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the repository's post-checkout hook ran in the node daemon")
	}
	gitRun(t, repo, "config", "filter.evil.smudge", "touch "+marker)
	if err := os.WriteFile(filepath.Join(repo, ".gitattributes"), []byte("* filter=evil\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := prepareWorktree(ctx, space, proto.Worktree{Repo: "repos/api", Branch: "spaceman/tsk-filter"})
	if err == nil || !strings.Contains(err.Error(), "filter.evil.smudge") {
		t.Fatalf("a repository with a filter driver: %v", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the repository's filter ran in the node daemon")
	}
}
