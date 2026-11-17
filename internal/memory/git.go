package memory

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
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

// safeOpts also keeps automatic housekeeping (gc --auto, maintenance run --auto) inside the git call: detached, it
// would keep writing into the repository after the call returned.
var safeOpts = []string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-c", "protocol.file.allow=always",
	"-c", "transfer.fsckObjects=true", "-c", "commit.gpgSign=false", "-c", "core.autocrlf=false", "-c", "init.defaultBranch=main",
	"-c", "advice.detachedHead=false", "-c", "merge.conflictStyle=merge",
	"-c", "gc.autoDetach=false", "-c", "maintenance.autoDetach=false",
	"-c", "submodule.recurse=false", "-c", "diff.ignoreSubmodules=all", "-c", "status.submoduleSummary=false",
	"-c", "fetch.recurseSubmodules=false", "-c", "push.recurseSubmodules=no",
	// The node's own ignore and attributes files are not read: a FIFO or a program-bearing path named there would
	// apply to every checkout (the repository's own .gitignore, .gitattributes and info/ files are checked by the walks).
	"-c", "core.excludesFile=/dev/null", "-c", "core.attributesFile=/dev/null"}

// The submodule options above keep git from stepping into a nested repository of a checkout: git runs a child git in
// a populated submodule (status, and the diff behind add and commit), with that repository's own configuration,
// which UnsafeConfig never saw, so its filter drivers would run as the daemon, and its git dir may be another
// repository whose index the child would refresh. `add` ignores diff.ignoreSubmodules, so gitlinks are also refused
// outright before any command that reads the work tree (Gitlinks).

// KillDelay is how long a git that was sent SIGTERM (its context ended) gets to exit before SIGKILL; it also bounds
// how long a grandchild holding the output pipes can block Wait. A variable so tests can shorten it.
var KillDelay = 5 * time.Second

// TermFirst makes a command that outlives its context stop with SIGTERM first and SIGKILL after KillDelay. git removes
// its lock files (index.lock) on SIGTERM; SIGKILL leaves them behind and the next git in the checkout fails.
func TermFirst(cmd *exec.Cmd) {
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = KillDelay
}

// Run executes git in dir and returns stdout.
func (g *Git) Run(ctx context.Context, dir string, id *Identity, args ...string) (string, error) {
	return g.RunInput(ctx, dir, id, nil, args...)
}

// RunInput is Run with stdin. Inside an existing repository the repository's configuration is checked first and git
// is not run when it would execute a configured program (UnsafeConfig).
func (g *Git) RunInput(ctx context.Context, dir string, id *Identity, stdin []byte, args ...string) (string, error) {
	return g.runInput(ctx, dir, id, stdin, false, args...)
}

// RunCheckout is Run for an instance or memory checkout: dir must be a checkout with its own .git directory, and a
// folder without one is refused. Without the requirement a folder holding HEAD, objects/, refs/ and a commondir file
// (no .git) is a bare repository to git, one that can point at another repository, and nothing would have checked it.
// git init and clone are exempt: they create the .git.
func (g *Git) RunCheckout(ctx context.Context, dir string, id *Identity, args ...string) (string, error) {
	return g.runInput(ctx, dir, id, nil, true, args...)
}

func (g *Git) runInput(ctx context.Context, dir string, id *Identity, stdin []byte, checkout bool, args ...string) (string, error) {
	if checkout && len(args) > 0 && args[0] != "init" && args[0] != "clone" {
		// Required for every command (config and version are not Guarded) but init and clone.
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err != nil {
			return "", fmt.Errorf("checkout refused: %w", err)
		}
	}
	if len(args) > 0 && Guarded(args[0]) {
		// The files come first: git reads .git/config (and anything it includes) for any command, and a FIFO there
		// would block it, so nothing runs before the walk has refused links and special files.
		_, statErr := os.Lstat(filepath.Join(dir, ".git"))
		inCheckout := statErr == nil || checkout
		if inCheckout {
			if err := checkoutFiles(dir); err != nil {
				return "", err
			}
			if !treeless[args[0]] {
				if err := workTreeFiles(dir); err != nil {
					return "", err
				}
			}
		}
		gctx, cancel := context.WithTimeout(ctx, GuardTimeout)
		defer cancel()
		list, err := g.run(gctx, dir, nil, nil, ConfigListArgs...)
		if err != nil {
			return "", err
		}
		if err := UnsafeConfig(list, true); err != nil {
			return "", err
		}
		if inCheckout {
			if err := g.checkoutGit(gctx, dir); err != nil {
				return "", err
			}
			if !treeless[args[0]] {
				if err := g.Gitlinks(gctx, dir); err != nil {
					return "", err
				}
			}
		}
	}
	return g.run(ctx, dir, id, stdin, args...)
}

// CheckoutIdentity refuses a checkout whose git dir, common dir or work tree is not its own folder. Agents can write
// the checkout including .git, so .git could be a symlink or a `gitdir:` file, or carry a commondir file, pointing
// at another repository whose refs and index git would then move; core.bare and core.worktree move the work tree.
// The checkout's .git must be a real directory that is both its git dir and its common dir, and its work tree the folder.
// Inside it, nothing may lead elsewhere either (ownGitDir).
//
// Residual risk (time of check to time of use): the checks run before git starts, and the agent can still write
// the checkout while git runs, so a link planted in that window is followed. ADR-0094 (checkout guard) records it
// and the structural fixes (run only while no process of the instance is alive, or in a daemon-owned copy).
func (g *Git) CheckoutIdentity(ctx context.Context, dir string) error {
	if err := checkoutFiles(dir); err != nil {
		return err
	}
	gctx, cancel := context.WithTimeout(ctx, GuardTimeout)
	defer cancel()
	return g.checkoutGit(gctx, dir)
}

// GuardTimeout bounds the guard's own git calls: a FIFO that an include.path (or a file git reads) names outside
// .git would block git for ever. A variable so tests can shorten it.
var GuardTimeout = 30 * time.Second

// checkoutFiles is the part of CheckoutIdentity that never runs git: .git is a plain directory and nothing in it
// leads elsewhere.
func checkoutFiles(dir string) error {
	dotGit := filepath.Join(dir, ".git")
	fi, err := os.Lstat(dotGit)
	if err != nil {
		return fmt.Errorf("checkout refused: %w", err)
	}
	if !fi.IsDir() {
		return errors.New("checkout refused: .git is not a plain directory (symlink or gitdir file)")
	}
	return ownGitDir(dotGit)
}

// workTreeFiles refuses a FIFO or a device anywhere in the work tree (the top-level .git excluded, a nested one refused): git opens every file it adds
// and reads every .gitignore and .gitattributes, and a FIFO there blocks it for ever while it holds index.lock. The
// type comes with the directory listing, so the walk costs no stat per file. Memory checkouts are small.
func workTreeFiles(dir string) error {
	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() == ".git" {
			if path == filepath.Join(dir, ".git") {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			// git reads a nested repository's HEAD for status and add, so a FIFO there (or a gitdir: file
			// pointing at one) would block it. Memory has no use for embedded repositories.
			rel, _ := filepath.Rel(dir, path)
			return fmt.Errorf("checkout refused: %s is a nested .git, and memory holds no embedded repositories", filepath.ToSlash(rel))
		}
		if d.IsDir() {
			return nil
		}
		if d.Type()&(fs.ModeNamedPipe|fs.ModeDevice|fs.ModeCharDevice) != 0 {
			rel, _ := filepath.Rel(dir, path)
			return fmt.Errorf("checkout refused: %s is a FIFO or a device file, which would block git", filepath.ToSlash(rel))
		}
		return nil
	})
}

// checkoutGit asks git where it thinks the git dir, common dir and work tree are.
func (g *Git) checkoutGit(ctx context.Context, dir string) error {
	dotGit := filepath.Join(dir, ".git")
	out, err := g.run(ctx, dir, nil, nil, "rev-parse", "--path-format=absolute", "--absolute-git-dir", "--git-common-dir", "--show-toplevel")
	if err != nil {
		return fmt.Errorf("checkout refused: git does not see a work tree here (core.bare?): %w", err)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 3 {
		return errors.New("checkout refused: unexpected rev-parse output")
	}
	wantGit, err := filepath.EvalSymlinks(dotGit)
	if err != nil {
		return fmt.Errorf("checkout refused: %w", err)
	}
	wantTop, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return fmt.Errorf("checkout refused: %w", err)
	}
	for i, want := range []string{wantGit, wantGit, wantTop} {
		got, err := filepath.EvalSymlinks(lines[i])
		if err != nil || got != want {
			return fmt.Errorf("checkout refused: the %s is not the checkout's own", [...]string{"git dir", "common dir", "work tree"}[i])
		}
	}
	return nil
}

// reFanout matches a loose-object fan-out folder (objects/ab).
var reFanout = regexp.MustCompile(`^[0-9a-f]{2}$`)

// ownGitDir walks a checkout's .git and refuses anything that makes git read or write outside it:
//
//   - a symlink anywhere (refs, objects, index, packed-refs, logs, config, …): git follows it, and its lockfiles
//     too, so a symlinked index or refs folder moves another repository's index or branches;
//   - a FIFO or device, which would block or redirect git's reads;
//   - a regular file with more than one hard link outside the object store: git rewrites some files in place
//     (COMMIT_EDITMSG, FETCH_HEAD, MERGE_MSG, reflogs appended to), so a hard link to another repository's ref or
//     reflog, made by an agent running as the same user as that repository's owner, would be overwritten there.
//     Object files are exempt: `git clone --local` hard-links them, and git never writes an existing object;
//   - objects/info/alternates (and http-alternates): objects borrowed from another repository, which a push would
//     copy into the shared memory;
//   - info/grafts, which rewrites history as git sees it (replace refs are off: GIT_NO_REPLACE_OBJECTS).
//
// hooks/ is not walked: hooks never run (core.hooksPath is forced, hook.* config is refused), and a project
// repository may legitimately symlink them (.git/hooks/pre-commit -> ../../scripts/hook). lfs/objects is skipped
// like the object fan-out: git never reads through it for the commands SpaceMan runs, and it can hold tens of
// thousands of files.
//
// The loose-object fan-out folders are checked themselves but not listed, so the walk costs a few hundred entries
// whatever the size of the object store: a symlinked object file inside one is only read, and its content is named
// by its hash. Linked worktrees' and submodules' git dirs (worktrees/, modules/) are walked like the rest.
func ownGitDir(dotGit string) error {
	refuse := func(rel, why string) error {
		return fmt.Errorf("checkout refused: .git/%s %s", filepath.ToSlash(rel), why)
	}
	return filepath.WalkDir(dotGit, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("checkout refused: %w", err)
		}
		if path == dotGit {
			return nil
		}
		rel, err := filepath.Rel(dotGit, path)
		if err != nil {
			return err
		}
		slash := filepath.ToSlash(rel)
		t := d.Type()
		switch {
		case t&fs.ModeSymlink != 0:
			return refuse(rel, "is a symlink")
		case t.IsDir():
			if inObjects(slash) && reFanout.MatchString(d.Name()) {
				return filepath.SkipDir
			}
			if skipGitDir(slash) {
				return filepath.SkipDir
			}
			return nil
		case !t.IsRegular() && t&fs.ModeSocket == 0:
			return refuse(rel, "is not a regular file")
		}
		switch slash {
		case "objects/info/alternates", "objects/info/http-alternates":
			return refuse(rel, "borrows objects from another repository")
		case "info/grafts":
			return refuse(rel, "rewrites the history git sees")
		}
		if t.IsRegular() && !inObjects(slash) {
			fi, err := d.Info()
			if err != nil {
				return fmt.Errorf("checkout refused: %w", err)
			}
			if st, ok := fi.Sys().(*syscall.Stat_t); ok && st.Nlink > 1 {
				return refuse(rel, "has another hard link (it may be another repository's file)")
			}
		}
		return nil
	})
}

// skipGitDir names the folders the walk does not enter: hooks and the LFS object store, of the checkout and of each
// submodule's git dir (modules/<name>/…).
func skipGitDir(rel string) bool {
	if strings.HasPrefix(rel, "modules/") {
		return strings.HasSuffix(rel, "/hooks") || strings.HasSuffix(rel, "/lfs/objects")
	}
	return rel == "hooks" || rel == "lfs/objects"
}

// inObjects reports whether a path relative to .git is inside an object store: the checkout's own, or one of a
// linked worktree's or submodule's git dir.
func inObjects(rel string) bool {
	if strings.HasPrefix(rel, "objects/") {
		return true
	}
	return (strings.HasPrefix(rel, "modules/") || strings.HasPrefix(rel, "worktrees/")) && strings.Contains(rel, "/objects/")
}

// treeless lists the subcommands SpaceMan runs that never read the work tree, so a gitlink there cannot make git
// step into a nested repository; every other guarded subcommand is checked by Gitlinks first. The KB acceptance runs
// only these in project repositories, which may have submodules.
var treeless = map[string]bool{
	"rev-parse": true, "merge-base": true, "merge-tree": true, "commit-tree": true, "update-ref": true, "rev-list": true,
	"ls-tree": true, "ls-files": true, "cat-file": true, "show": true, "log": true, "bundle": true, "fetch": true,
	"push": true, "worktree": true, "for-each-ref": true, "symbolic-ref": true,
}

// Gitlinks refuses a checkout whose index records a gitlink (a submodule or an embedded repository). For a gitlink
// whose folder holds a .git, git runs a child git in that folder to see whether it is modified, with that
// repository's configuration (filter drivers run as the daemon) and in whatever git dir its .git names (another
// repository's index is refreshed). `git add` does this whatever diff.ignoreSubmodules says. Memory has no use for
// submodules, so an index with a gitlink is refused before any command that reads the work tree.
func (g *Git) Gitlinks(ctx context.Context, dir string) error {
	ctx, cancel := context.WithTimeout(ctx, GuardTimeout)
	defer cancel()
	out, err := g.run(ctx, dir, nil, nil, "ls-files", "--stage", "-z")
	if err != nil {
		return fmt.Errorf("checkout refused: %w", err)
	}
	for e := range strings.SplitSeq(out, "\x00") {
		if strings.HasPrefix(e, "160000 ") {
			_, path, _ := strings.Cut(e, "\t")
			return fmt.Errorf("checkout refused: %s is a submodule or an embedded git repository; remove it from the index (git rm --cached)", path)
		}
	}
	return nil
}

func (g *Git) run(ctx context.Context, dir string, id *Identity, stdin []byte, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, g.Bin, append(append([]string{}, safeOpts...), args...)...) //nolint:gosec // fixed git subcommands; arguments are validated by callers
	cmd.Dir = dir
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0", "LC_ALL=C", "GIT_NO_REPLACE_OBJECTS=1", "GIT_NO_LAZY_FETCH=1"}
	// git looks for a repository in dir and never above it: a checkout whose .git is missing must not make git find
	// a repository in a parent folder and work there.
	if abs, err := filepath.Abs(dir); err == nil {
		env = append(env, "GIT_CEILING_DIRECTORIES="+filepath.Dir(abs))
	}
	if id != nil {
		env = append(env, "GIT_AUTHOR_NAME="+id.Name, "GIT_AUTHOR_EMAIL="+id.Email, "GIT_COMMITTER_NAME="+id.Name, "GIT_COMMITTER_EMAIL="+id.Email)
	}
	cmd.Env = env
	// SIGTERM first so git removes its lock files; a grandchild holding the pipes open must not block Wait.
	TermFirst(cmd)
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

// ConflictFolders lists the folders directly under conflicts/ at a commit (the places preserved conflicting memory
// is kept for reconciling); "" lists none. A commit that cannot be read is an error, not an empty list, so a caller
// never mistakes a failed read for "every folder is gone".
func (g *Git) ConflictFolders(ctx context.Context, dir, commit string) ([]string, error) {
	if commit == "" {
		return nil, nil
	}
	out, err := g.Run(ctx, dir, nil, "ls-tree", "-d", "--name-only", "-z", commit, "conflicts/")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, f := range strings.Split(out, "\x00") {
		if n := strings.TrimPrefix(f, "conflicts/"); n != "" && n != f && !strings.Contains(n, "/") {
			names = append(names, n)
		}
	}
	return names, nil
}
