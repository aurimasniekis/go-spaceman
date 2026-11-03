package agent

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/memory"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// prepareWorktree gives a writer its own git worktree (SM-REPO-002): space/worktrees/<branch>, a worktree of the
// repository at space/<repo> on the branch (created from the base when it does not exist). An existing worktree of
// that repository at that folder is reused. Nothing is ever reset, deleted, forced or pushed (SM-REPO-006), and git
// runs with the node's own environment, so the node's git configuration and credentials are used, never replaced
// (SM-REPO-005), with hooks and fsmonitor disabled. It returns the worktree folder.
func prepareWorktree(ctx context.Context, space string, w proto.Worktree) (string, error) {
	git, err := exec.LookPath("git")
	if err != nil {
		return "", errors.New("git is not installed on this node")
	}
	repo := filepath.Join(space, filepath.Clean("/"+w.Repo))
	dir := filepath.Join(space, "worktrees", strings.ReplaceAll(w.Branch, "/", "-"))
	// git runs with the node's configuration and credentials but with hooks and fsmonitor off, and not at all when
	// the repository's own configuration would run a program: agents can write the repository's .git, and the daemon
	// must not run their code (security review SR-M03).
	raw := func(args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, git, append(append(append([]string{}, safeGitOpts...), "-C", repo), args...)...) //nolint:gosec // fixed git subcommands; validated names
		cmd.Env = gitEnv()
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		if err := cmd.Run(); err != nil {
			msg := strings.TrimSpace(errb.String())
			if len(msg) > 300 {
				msg = msg[:300]
			}
			return "", fmt.Errorf("git %s: %s", args[0], msg)
		}
		return out.String(), nil
	}
	guard := func() error {
		list, err := raw("config", "--list", "--name-only", "--show-scope")
		if err != nil {
			return err
		}
		return memory.UnsafeConfig(list, true)
	}
	run := func(args ...string) (string, error) {
		if err := guard(); err != nil {
			return "", err
		}
		return raw(args...)
	}
	if _, err := raw("rev-parse", "--git-dir"); err != nil {
		return "", fmt.Errorf("%s is not a git repository", w.Repo)
	}
	if err := guard(); err != nil {
		return "", fmt.Errorf("%s: %w", w.Repo, err)
	}
	list, err := run("worktree", "list", "--porcelain")
	if err != nil {
		return "", err
	}
	realDir := dir
	if r, err := filepath.EvalSymlinks(filepath.Dir(dir)); err == nil {
		realDir = filepath.Join(r, filepath.Base(dir))
	}
	sc := bufio.NewScanner(strings.NewReader(list))
	for sc.Scan() {
		if p, ok := strings.CutPrefix(sc.Text(), "worktree "); ok && (p == dir || p == realDir) {
			return dir, nil // already this repository's worktree: reuse it as it is
		}
	}
	if _, err := os.Stat(dir); err == nil {
		return "", fmt.Errorf("worktrees/%s exists but is not a worktree of %s; SpaceMan does not overwrite it", filepath.Base(dir), w.Repo)
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return "", err
	}
	if _, err := run("rev-parse", "--verify", "--quiet", "refs/heads/"+w.Branch); err == nil {
		_, err = run("worktree", "add", dir, w.Branch)
		return dir, err
	}
	base := w.Base
	if base == "" {
		base = "HEAD"
	}
	_, err = run("worktree", "add", "-b", w.Branch, dir, base)
	return dir, err
}
