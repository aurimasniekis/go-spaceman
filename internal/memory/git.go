package memory

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Git runs the git command line for memory repositories. Every call uses a scrubbed environment (no inherited GIT_*
// variables, no system or global config) and options that keep a repository's own files from running code: agents can
// write to their checkout, including .git, so hooks and fsmonitor are disabled and pushes and fetches name repository
// paths, never configured remotes.
type Git struct {
	Bin string
}

// NewGit finds git on PATH.
func NewGit() (*Git, error) {
	p, err := exec.LookPath("git")
	if err != nil {
		return nil, errors.New("git is not installed")
	}
	return &Git{Bin: p}, nil
}

// Identity is the author of memory commits.
type Identity struct{ Name, Email string }

var safeOpts = []string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-c", "protocol.file.allow=always",
	"-c", "transfer.fsckObjects=true", "-c", "commit.gpgSign=false", "-c", "core.autocrlf=false", "-c", "init.defaultBranch=main",
	"-c", "advice.detachedHead=false", "-c", "merge.conflictStyle=merge"}

// Run executes git in dir and returns stdout.
func (g *Git) Run(ctx context.Context, dir string, id *Identity, args ...string) (string, error) {
	return g.RunInput(ctx, dir, id, nil, args...)
}

// RunInput is Run with stdin.
func (g *Git) RunInput(ctx context.Context, dir string, id *Identity, stdin []byte, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, g.Bin, append(append([]string{}, safeOpts...), args...)...) //nolint:gosec // fixed git subcommands; arguments are validated by callers
	cmd.Dir = dir
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0", "LC_ALL=C"}
	if id != nil {
		env = append(env, "GIT_AUTHOR_NAME="+id.Name, "GIT_AUTHOR_EMAIL="+id.Email, "GIT_COMMITTER_NAME="+id.Name, "GIT_COMMITTER_EMAIL="+id.Email)
	}
	cmd.Env = env
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if len(msg) > 500 {
			msg = msg[:500]
		}
		return out.String(), fmt.Errorf("git %s: %w: %s", args[0], err, msg)
	}
	return out.String(), nil
}

// RevParse returns the commit a ref names, or "" when it does not exist.
func (g *Git) RevParse(ctx context.Context, dir, ref string) string {
	out, err := g.Run(ctx, dir, nil, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// IsAncestor reports whether a is an ancestor of (or equal to) b.
func (g *Git) IsAncestor(ctx context.Context, dir, a, b string) bool {
	_, err := g.Run(ctx, dir, nil, "merge-base", "--is-ancestor", a, b)
	return err == nil
}

// Count returns the number of commits in b that are not in a ("" for a counts all of b).
func (g *Git) Count(ctx context.Context, dir, a, b string) int {
	spec := b
	if a != "" {
		spec = a + ".." + b
	}
	out, err := g.Run(ctx, dir, nil, "rev-list", "--count", spec)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(out))
	return n
}

// InitBare creates a bare repository with main as its default branch.
func (g *Git) InitBare(ctx context.Context, dir string) error {
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); err == nil {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	_, err := g.Run(ctx, dir, nil, "init", "--quiet", "--bare", "--initial-branch=main", ".")
	return err
}

// Bundle writes the commits of ref that are not in base ("" for all) and returns the bundle bytes.
func (g *Git) Bundle(ctx context.Context, dir, base, ref string, maxBytes int) ([]byte, error) {
	f, err := os.CreateTemp("", "spaceman-memory-*.bundle")
	if err != nil {
		return nil, err
	}
	path := f.Name()
	_ = f.Close()
	defer os.Remove(path)
	args := []string{"bundle", "create", "--quiet", path, ref}
	if base != "" {
		args = append(args, "^"+base)
	}
	if _, err := g.Run(ctx, dir, nil, args...); err != nil {
		return nil, err
	}
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if fi.Size() > int64(maxBytes) {
		return nil, fmt.Errorf("%w: %d bytes", ErrTooLarge, fi.Size())
	}
	return os.ReadFile(path) //nolint:gosec // our own temporary file
}

// ErrTooLarge is returned for a change too large to sync in one message.
var ErrTooLarge = errors.New("the memory change is too large to sync")

// ErrMissingBase is returned when a bundle builds on commits the receiving repository does not have.
var ErrMissingBase = errors.New("the receiving repository lacks the bundle's base commits")

// Unbundle verifies a bundle against the repository and fetches its ref into target, returning the fetched commit.
func (g *Git) Unbundle(ctx context.Context, dir string, bundle []byte, ref, target string) (string, error) {
	f, err := os.CreateTemp("", "spaceman-memory-*.bundle")
	if err != nil {
		return "", err
	}
	path := f.Name()
	defer os.Remove(path)
	if _, err := f.Write(bundle); err != nil {
		_ = f.Close()
		return "", err
	}
	_ = f.Close()
	if _, err := g.Run(ctx, dir, nil, "bundle", "verify", path); err != nil {
		if strings.Contains(err.Error(), "prerequisite") {
			return "", ErrMissingBase
		}
		return "", fmt.Errorf("invalid bundle: %w", err)
	}
	if _, err := g.Run(ctx, dir, nil, "fetch", "--quiet", "--no-tags", path, "+"+ref+":"+target); err != nil {
		return "", err
	}
	return g.RevParse(ctx, dir, target), nil
}

var reHex = regexp.MustCompile(`^[0-9a-f]{40,64}$`)

// ValidCommit reports whether s looks like a full commit ID.
func ValidCommit(s string) bool { return reHex.MatchString(s) }
