package agent

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/memory"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// Relocation's view of the space folder's repositories (ADR-0037). Git runs with the node's own environment, so the
// node's Git configuration and credentials are used and never replaced (SM-REPO-005), but with hooks and fsmonitor
// disabled: agents can write a repository's .git, and inspecting or exporting it must not run their code. Nothing in a
// repository is reset, committed to a branch, pushed or deleted (SM-REPO-006); an export only adds temporary refs under
// refs/spaceman-relocation/ and removes them again.

const exportRefs = "refs/spaceman-relocation/"

var safeGitOpts = []string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-c", "advice.detachedHead=false",
	"-c", "commit.gpgSign=false", "-c", "tag.gpgSign=false", "-c", "core.autocrlf=false", "-c", "init.defaultBranch=main",
	// The ext transport runs a program named in the URL; a remote an agent wrote into .git/config must never reach it.
	"-c", "protocol.ext.allow=never"}

// projectGit runs git for a space's repositories.
type projectGit struct{ bin string }

func newProjectGit() (*projectGit, error) {
	p, err := exec.LookPath("git")
	if err != nil {
		return nil, errors.New("git is not installed on this node")
	}
	return &projectGit{bin: p}, nil
}

// gitEnv is the node's environment without variables that would point git at another repository or index.
func gitEnv(extra ...string) []string {
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_COMMON_DIR",
			"GIT_NAMESPACE", "GIT_PREFIX", "GIT_CONFIG_PARAMETERS", "GIT_CONFIG_COUNT":
			continue
		}
		if strings.HasPrefix(k, "GIT_CONFIG_KEY_") || strings.HasPrefix(k, "GIT_CONFIG_VALUE_") {
			continue
		}
		env = append(env, kv)
	}
	return append(env, append([]string{"GIT_TERMINAL_PROMPT=0", "LC_ALL=C"}, extra...)...)
}

// runEnv runs git in dir; inside an existing repository it first refuses repository configuration that would run a
// program (memory.UnsafeConfig), because agents can write the repositories' .git.
func (g *projectGit) runEnv(ctx context.Context, dir string, timeout time.Duration, env []string, args ...string) (string, error) {
	if len(args) > 0 && memory.Guarded(args[0]) {
		list, err := g.rawEnv(ctx, dir, timeout, nil, "config", "--list", "--name-only", "--show-scope")
		if err != nil {
			return "", err
		}
		if err := memory.UnsafeConfig(list, true); err != nil {
			return "", err
		}
	}
	return g.rawEnv(ctx, dir, timeout, env, args...)
}

func (g *projectGit) rawEnv(ctx context.Context, dir string, timeout time.Duration, env []string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, g.bin, append(append([]string{}, safeGitOpts...), args...)...) //nolint:gosec // fixed git subcommands; validated arguments
	cmd.Dir = dir
	cmd.Env = gitEnv(env...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return out.String(), fmt.Errorf("git %s: %s", args[0], firstNonEmpty(msg, err.Error()))
	}
	return out.String(), nil
}

func (g *projectGit) run(ctx context.Context, dir string, args ...string) (string, error) {
	return g.runEnv(ctx, dir, 2*time.Minute, nil, args...)
}

func (g *projectGit) line(ctx context.Context, dir string, args ...string) string {
	out, err := g.run(ctx, dir, args...)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

func (g *projectGit) count(ctx context.Context, dir string, args ...string) int {
	n, _ := strconv.Atoi(g.line(ctx, dir, append([]string{"rev-list", "--count"}, args...)...))
	return n
}

func firstNonEmpty(s ...string) string {
	for _, x := range s {
		if x != "" {
			return x
		}
	}
	return ""
}

// redactURL removes user information (credentials) from a remote URL. An SSH URL keeps a user name without a password
// (ssh://git@host/…): it names the account to log in as, not a credential, and the URL does not work without it.
func redactURL(raw string) (string, bool) {
	if u, err := url.Parse(raw); err == nil && u.User != nil && u.Scheme != "" {
		if _, hasPassword := u.User.Password(); !hasPassword && proto.IsSSHURL(raw) {
			return raw, false
		}
		u.User = nil
		return u.String(), true
	}
	return raw, false
}

// spaceLayout is what a walk of the space folder finds.
type spaceLayout struct {
	repos      []string // relative paths of repositories (a .git directory)
	linked     []string // relative paths of folders with a .git file (linked worktrees, outside any repository)
	loose      []string // relative paths of files in no repository (regular files and symlinks)
	looseBytes int64
	instances  []proto.InstanceCheck
}

const maxWalkDepth = 8

// walkSpace classifies the space folder without following symlinks. instances/ holds native harness state.
func walkSpace(space string) (spaceLayout, error) {
	var l spaceLayout
	var walk func(rel string, depth int) error
	walk = func(rel string, depth int) error {
		dir := filepath.Join(space, rel)
		if rel != "" {
			if fi, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
				if fi.IsDir() {
					l.repos = append(l.repos, filepath.ToSlash(rel))
				} else {
					l.linked = append(l.linked, filepath.ToSlash(rel))
				}
				return nil
			}
		}
		if depth > maxWalkDepth {
			return fmt.Errorf("%s is nested deeper than %d folders", rel, maxWalkDepth)
		}
		ents, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, e := range ents {
			child := filepath.Join(rel, e.Name())
			if rel == "" && slices.Contains(reservedDirs, e.Name()) {
				continue // the harnesses' own state, whatever its type (a planted symlink too), is never carried
			}
			if rel == "" && strings.HasPrefix(e.Name(), ".spaceman-") {
				continue // SpaceMan's own markers
			}
			switch {
			case e.IsDir():
				if err := walk(child, depth+1); err != nil {
					return err
				}
			case e.Type().IsRegular() || e.Type()&fs.ModeSymlink != 0:
				l.loose = append(l.loose, filepath.ToSlash(child))
				if fi, err := e.Info(); err == nil && e.Type().IsRegular() {
					l.looseBytes += fi.Size()
				}
			}
		}
		return nil
	}
	if err := walk("", 0); err != nil {
		return l, err
	}
	insts, _ := os.ReadDir(filepath.Join(space, "instances"))
	for _, i := range insts {
		if !i.IsDir() {
			continue
		}
		ic := proto.InstanceCheck{InstanceID: i.Name()}
		_ = filepath.WalkDir(filepath.Join(space, "instances", i.Name()), func(_ string, d fs.DirEntry, err error) error {
			if err == nil && d.Type().IsRegular() {
				ic.Files++
				if fi, err := d.Info(); err == nil {
					ic.Bytes += fi.Size()
				}
			}
			return nil
		})
		l.instances = append(l.instances, ic)
	}
	return l, nil
}

// realPath resolves symlinks (macOS: /var is /private/var) so git's absolute paths compare.
func realPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// inspectSpace inspects every repository, worktree and loose file of the space folder.
func (g *projectGit) inspectSpace(ctx context.Context, space string) (repos []proto.RepoCheck, loose proto.LooseCheck, instances []proto.InstanceCheck, errs []string) {
	repos, instances, errs = []proto.RepoCheck{}, []proto.InstanceCheck{}, []string{}
	l, err := walkSpace(space)
	if err != nil {
		return repos, loose, instances, []string{"inspect the space folder: " + err.Error()}
	}
	instances = append(instances, l.instances...)
	loose = proto.LooseCheck{Files: len(l.loose), Bytes: l.looseBytes}
	for i, p := range l.loose {
		if i == 20 {
			break
		}
		loose.Paths = append(loose.Paths, p)
	}
	claimed := map[string]bool{}
	for _, rel := range l.repos {
		rc := g.inspectRepo(ctx, space, rel)
		for _, w := range rc.Worktrees {
			claimed[w.Path] = true
		}
		repos = append(repos, rc)
	}
	for _, p := range l.linked {
		if !claimed[p] {
			errs = append(errs, p+" is a linked worktree or submodule of a repository outside the space folder; SpaceMan cannot relocate it")
		}
	}
	return repos, loose, instances, errs
}

func statusCounts(out string) (dirty, untracked int, paths []string) {
	entries := strings.Split(out, "\x00")
	for i := 0; i < len(entries); i++ {
		e := entries[i]
		if len(e) < 4 {
			continue
		}
		xy, p := e[:2], e[3:]
		if xy == "??" {
			untracked++
		} else {
			dirty++
		}
		if xy[0] == 'R' || xy[0] == 'C' {
			i++ // the rename's source path follows
		}
		if len(paths) < 20 {
			paths = append(paths, p)
		}
	}
	return dirty, untracked, paths
}

func (g *projectGit) status(ctx context.Context, dir string) (int, int, []string, error) {
	out, err := g.run(ctx, dir, "status", "--porcelain=v1", "-z", "--untracked-files=normal")
	if err != nil {
		return 0, 0, nil, err
	}
	d, u, p := statusCounts(out)
	return d, u, p, nil
}

// inspectRepo reports a repository's remotes, branches (with what is not pushed), changes, stashes and worktrees.
func (g *projectGit) inspectRepo(ctx context.Context, space, rel string) proto.RepoCheck {
	dir := filepath.Join(space, filepath.FromSlash(rel))
	rc := proto.RepoCheck{Path: rel, Remotes: []proto.GitRemote{}, Branches: []proto.BranchCheck{}, Worktrees: []proto.WorktreeCheck{}}
	if _, err := g.run(ctx, dir, "rev-parse", "--git-dir"); err != nil {
		rc.Error = "not a usable git repository: " + err.Error()
		return rc
	}
	for _, name := range strings.Fields(g.line(ctx, dir, "remote")) {
		u, red := redactURL(g.line(ctx, dir, "remote", "get-url", name))
		rc.Remotes = append(rc.Remotes, proto.GitRemote{Name: name, URL: u, Redacted: red})
	}
	rc.Head = g.line(ctx, dir, "rev-parse", "--verify", "--quiet", "HEAD")
	rc.Branch = g.line(ctx, dir, "symbolic-ref", "--short", "--quiet", "HEAD")
	refs := g.line(ctx, dir, "for-each-ref", "--format=%(refname:short)%00%(objectname)%00%(upstream)", "refs/heads")
	for _, ln := range strings.Split(refs, "\n") {
		f := strings.Split(ln, "\x00")
		if len(f) != 3 || f[0] == "" {
			continue
		}
		b := proto.BranchCheck{Name: f[0], Commit: f[1], Remote: g.line(ctx, dir, "config", "--get", "branch."+f[0]+".remote"),
			Merge: g.line(ctx, dir, "config", "--get", "branch."+f[0]+".merge")}
		if f[2] != "" && g.line(ctx, dir, "rev-parse", "--verify", "--quiet", f[2]) != "" {
			b.Unpushed = g.count(ctx, dir, f[2]+".."+"refs/heads/"+f[0])
		} else {
			b.NoUpstream = true
			b.Unpushed = g.count(ctx, dir, "refs/heads/"+f[0], "--not", "--remotes")
		}
		rc.Branches = append(rc.Branches, b)
	}
	if rc.Branch == "" && rc.Head != "" {
		rc.Detached = g.count(ctx, dir, "HEAD", "--not", "--branches", "--remotes")
	}
	var err error
	if rc.Dirty, rc.Untracked, rc.Paths, err = g.status(ctx, dir); err != nil {
		rc.Error = err.Error()
		return rc
	}
	if st := g.line(ctx, dir, "stash", "list", "--format=%H"); st != "" {
		rc.Stashes = len(strings.Split(st, "\n"))
	}
	wts, err := g.worktrees(ctx, dir)
	if err != nil {
		rc.Error = err.Error()
		return rc
	}
	realSpace := realPath(space)
	for _, w := range wts {
		wc := proto.WorktreeCheck{Branch: w.branch, Head: w.head}
		relp, err := filepath.Rel(realSpace, realPath(w.path))
		if err != nil || strings.HasPrefix(relp, "..") {
			wc.Path, wc.Error = w.path, "a worktree outside the space folder; SpaceMan cannot relocate it"
			rc.Worktrees = append(rc.Worktrees, wc)
			continue
		}
		wc.Path = filepath.ToSlash(relp)
		if wc.Dirty, wc.Untracked, _, err = g.status(ctx, w.path); err != nil {
			wc.Error = err.Error()
		}
		rc.Worktrees = append(rc.Worktrees, wc)
	}
	return rc
}

type worktree struct{ path, head, branch string }

// worktrees lists a repository's linked worktrees (not the main one; prunable ones are skipped).
func (g *projectGit) worktrees(ctx context.Context, dir string) ([]worktree, error) {
	out, err := g.run(ctx, dir, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	var all []worktree
	var cur *worktree
	prunable := map[int]bool{}
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		ln := sc.Text()
		switch {
		case strings.HasPrefix(ln, "worktree "):
			all = append(all, worktree{path: strings.TrimPrefix(ln, "worktree ")})
			cur = &all[len(all)-1]
		case cur != nil && strings.HasPrefix(ln, "HEAD "):
			cur.head = strings.TrimPrefix(ln, "HEAD ")
		case cur != nil && strings.HasPrefix(ln, "branch "):
			cur.branch = strings.TrimPrefix(strings.TrimPrefix(ln, "branch "), "refs/heads/")
		case cur != nil && strings.HasPrefix(ln, "prunable"):
			prunable[len(all)-1] = true
		}
	}
	main := realPath(dir)
	var out2 []worktree
	for i, w := range all {
		if realPath(w.path) == main || prunable[i] {
			continue
		}
		out2 = append(out2, w)
	}
	return out2, nil
}

var exportIdent = []string{"GIT_AUTHOR_NAME=SpaceMan relocation", "GIT_AUTHOR_EMAIL=relocation@spaceman.invalid",
	"GIT_COMMITTER_NAME=SpaceMan relocation", "GIT_COMMITTER_EMAIL=relocation@spaceman.invalid"}

// snapshot returns a commit holding the work tree's changed and untracked (not ignored) files on top of HEAD, built in a
// temporary index so the repository's own index, work tree and refs are untouched; "" when nothing changed.
func (g *projectGit) snapshot(ctx context.Context, dir, head string) (string, error) {
	f, err := os.CreateTemp("", "spaceman-relocation-index-*")
	if err != nil {
		return "", err
	}
	idx := f.Name()
	_ = f.Close()
	_ = os.Remove(idx)
	defer os.Remove(idx)
	env := append([]string{"GIT_INDEX_FILE=" + idx}, exportIdent...)
	if head != "" {
		if _, err := g.runEnv(ctx, dir, 2*time.Minute, env, "read-tree", head); err != nil {
			return "", err
		}
	}
	if _, err := g.runEnv(ctx, dir, 5*time.Minute, env, "add", "-A", "--", "."); err != nil {
		return "", err
	}
	tree, err := g.runEnv(ctx, dir, time.Minute, env, "write-tree")
	if err != nil {
		return "", err
	}
	tree = strings.TrimSpace(tree)
	if head != "" && g.line(ctx, dir, "rev-parse", head+"^{tree}") == tree {
		return "", nil
	}
	if head == "" {
		if empty := g.line(ctx, dir, "hash-object", "-t", "tree", "/dev/null"); empty == tree {
			return "", nil
		}
	}
	args := []string{"commit-tree", tree, "-m", "SpaceMan relocation snapshot of uncommitted work (not on any branch)"}
	if head != "" {
		args = append(args, "-p", head)
	}
	c, err := g.runEnv(ctx, dir, time.Minute, env, args...)
	return strings.TrimSpace(c), err
}

// exportRepo writes a bundle of every ref of the repository plus snapshot commits of its main work tree and worktrees
// and every stash entry, to out. The temporary refs it needs are removed again.
func (g *projectGit) exportRepo(ctx context.Context, space, rel, out string) (proto.ExportedRepo, error) {
	dir := filepath.Join(space, filepath.FromSlash(rel))
	rc := g.inspectRepo(ctx, space, rel)
	if rc.Error != "" {
		return proto.ExportedRepo{}, errors.New(rc.Error)
	}
	ex := proto.ExportedRepo{Path: rel, Blob: filepath.Base(out), Branch: rc.Branch, Head: rc.Head, Remotes: rc.Remotes, Branches: rc.Branches,
		Worktrees: []proto.ExportedWorktree{}, Stashes: []proto.ExportedStash{}}
	var refs []string
	defer func() {
		for _, r := range refs {
			_, _ = g.run(context.WithoutCancel(ctx), dir, "update-ref", "-d", r)
		}
	}()
	pin := func(name, commit string) error {
		ref := exportRefs + name
		if _, err := g.run(ctx, dir, "update-ref", ref, commit); err != nil {
			return err
		}
		refs = append(refs, ref)
		return nil
	}
	var err error
	if ex.Snapshot, err = g.snapshot(ctx, dir, rc.Head); err != nil {
		return ex, fmt.Errorf("snapshot the work tree: %w", err)
	}
	if ex.Snapshot != "" {
		if err := pin("snapshot/main", ex.Snapshot); err != nil {
			return ex, err
		}
	}
	for i, w := range rc.Worktrees {
		ew := proto.ExportedWorktree{Path: w.Path, Branch: w.Branch, Head: w.Head}
		if ew.Snapshot, err = g.snapshot(ctx, filepath.Join(space, filepath.FromSlash(w.Path)), w.Head); err != nil {
			return ex, fmt.Errorf("snapshot worktree %s: %w", w.Path, err)
		}
		if ew.Snapshot != "" {
			if err := pin(fmt.Sprintf("snapshot/wt-%d", i), ew.Snapshot); err != nil {
				return ex, err
			}
		}
		if w.Branch == "" && w.Head != "" {
			if err := pin(fmt.Sprintf("head/wt-%d", i), w.Head); err != nil {
				return ex, err
			}
		}
		ex.Worktrees = append(ex.Worktrees, ew)
	}
	if rc.Branch == "" && rc.Head != "" {
		if err := pin("head/main", rc.Head); err != nil {
			return ex, err
		}
	}
	if st := g.line(ctx, dir, "stash", "list", "--format=%H%x1f%gs"); st != "" {
		for i, ln := range strings.Split(st, "\n") {
			h, msg, _ := strings.Cut(ln, "\x1f")
			ex.Stashes = append(ex.Stashes, proto.ExportedStash{Commit: h, Message: msg})
			if err := pin(fmt.Sprintf("stash/%d", i), h); err != nil {
				return ex, err
			}
		}
	}
	if g.line(ctx, dir, "for-each-ref", "--count=1", "--format=%(refname)") == "" {
		ex.Blob = "" // an empty repository: nothing to bundle
		return ex, nil
	}
	if _, err := g.runEnv(ctx, dir, 30*time.Minute, nil, "bundle", "create", "--quiet", out, "--all"); err != nil {
		return ex, fmt.Errorf("bundle: %w", err)
	}
	return ex, nil
}

// cloneRepo materializes a repository whose state is pushed: a clone from its remote at the source's commit, with the
// source's local branches and worktrees. A repository with a SpaceMan SSH key (rr.Source carries it) is cloned from its
// configured URL with that key and its pinned host keys, as a normal clone is (ADR-0057); any other repository with the
// destination node's own Git authentication. The clone is the only step that reaches the remote: the checkout,
// branches, upstreams and worktrees that follow are local.
func (g *projectGit) cloneRepo(ctx context.Context, space string, rr proto.RelocateRepo) (proto.PreparedRepo, error) {
	dir := filepath.Join(space, filepath.FromSlash(rr.Path))
	if len(rr.Remotes) == 0 {
		return proto.PreparedRepo{}, fmt.Errorf("%s has no remote to clone", rr.Path)
	}
	origin := rr.Remotes[0]
	for _, r := range rr.Remotes {
		if r.Name == "origin" {
			origin = r
		}
	}
	src, keyed := relocSource(rr)
	from := origin
	if keyed {
		// The key is only ever used with the configured URL: clone from the remote that has it, else from that URL under
		// origin's name, whose URL is set back to the source's after the clone.
		from = proto.GitRemote{Name: origin.Name, URL: src.URL}
		for _, r := range rr.Remotes {
			if r.URL == src.URL {
				from = r
				break
			}
		}
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return proto.PreparedRepo{}, err
	}
	args := []string{"clone", "--quiet", "--origin", from.Name}
	if rr.Head != "" {
		args = append(args, "--no-checkout")
	} else if rr.Branch != "" {
		args = append(args, "--branch", rr.Branch)
	}
	if err := g.cloneWith(ctx, space, src, append(args, "--", from.URL, dir)); err != nil {
		return proto.PreparedRepo{}, fmt.Errorf("clone %s: %w", rr.Path, sshError(err, keyed))
	}
	for _, r := range rr.Remotes {
		switch {
		case r.Name != from.Name:
			if _, err := g.run(ctx, dir, "remote", "add", r.Name, r.URL); err != nil {
				return proto.PreparedRepo{}, err
			}
		case r.URL != from.URL:
			if _, err := g.run(ctx, dir, "remote", "set-url", r.Name, r.URL); err != nil {
				return proto.PreparedRepo{}, err
			}
		}
	}
	if rr.Head != "" {
		co := []string{"checkout", "--quiet", "--force"}
		if rr.Branch != "" {
			co = append(co, "-B", rr.Branch, rr.Head)
		} else {
			co = append(co, "--detach", rr.Head)
		}
		if _, err := g.run(ctx, dir, co...); err != nil {
			return proto.PreparedRepo{}, fmt.Errorf("check out %s at the source's commit: %w", rr.Path, err)
		}
	}
	for _, b := range rr.Branches {
		if b.Name != rr.Branch && g.line(ctx, dir, "rev-parse", "--verify", "--quiet", "refs/heads/"+b.Name) == "" {
			if _, err := g.run(ctx, dir, "branch", "--no-track", b.Name, b.Commit); err != nil {
				return proto.PreparedRepo{}, fmt.Errorf("branch %s of %s is not on the remote: %w", b.Name, rr.Path, err)
			}
		}
		if err := g.setUpstream(ctx, dir, b); err != nil {
			return proto.PreparedRepo{}, err
		}
	}
	if err := g.addWorktrees(ctx, space, dir, rr.Worktrees); err != nil {
		return proto.PreparedRepo{}, err
	}
	return g.prepared(ctx, rr.Path, dir, false, len(rr.Worktrees))
}

// relocSource is the configured repository whose SpaceMan SSH key a relocated repository clones with, and whether it has one.
func relocSource(rr proto.RelocateRepo) (proto.RepoSource, bool) {
	if rr.Source == nil || rr.Source.SSHKey == "" || rr.Source.URL == "" {
		return proto.RepoSource{}, false
	}
	return *rr.Source, true
}

// cloneWith runs a clone with the repository's key (newRepoAuth: a private agent, the pinned host keys and an ssh
// without configuration) when src has one, else with the node's own Git authentication.
func (g *projectGit) cloneWith(ctx context.Context, space string, src proto.RepoSource, args []string) error {
	auth, err := newRepoAuth(ctx, src)
	if err != nil {
		return err
	}
	defer auth.Close()
	_, err = g.runEnv(ctx, space, 30*time.Minute, auth.Env(), args...)
	return err
}

func (g *projectGit) setUpstream(ctx context.Context, dir string, b proto.BranchCheck) error {
	if b.Remote == "" || b.Merge == "" {
		return nil
	}
	if _, err := g.run(ctx, dir, "config", "branch."+b.Name+".remote", b.Remote); err != nil {
		return err
	}
	_, err := g.run(ctx, dir, "config", "branch."+b.Name+".merge", b.Merge)
	return err
}

func (g *projectGit) addWorktrees(ctx context.Context, space, dir string, wts []proto.ExportedWorktree) error {
	for _, w := range wts {
		// The path comes from the source node's report. It must stay inside the space folder, outside its reserved
		// folders, and must not pass through a symlink: the repository just checked out may contain committed
		// symlinks, and git would create the worktree wherever one points (security review SR-M02).
		if !safeRel(w.Path) || underReserved(filepath.Clean(filepath.FromSlash(w.Path)), "dreams") {
			return fmt.Errorf("invalid worktree path %q", w.Path)
		}
		rel := filepath.FromSlash(w.Path)
		if err := noSymlinkParents(space, rel); err != nil {
			return fmt.Errorf("worktree %s: %w", w.Path, err)
		}
		wdir := filepath.Join(space, rel)
		if _, err := os.Lstat(wdir); err == nil {
			return fmt.Errorf("worktree %s: the folder already exists on the destination", w.Path)
		}
		if err := os.MkdirAll(filepath.Dir(wdir), 0o700); err != nil {
			return err
		}
		// MkdirAll may have created parents; re-check that none of them is (or became) a symlink.
		if err := noSymlinkParents(space, rel); err != nil {
			return fmt.Errorf("worktree %s: %w", w.Path, err)
		}
		args := []string{"worktree", "add", "--quiet"}
		if w.Branch != "" {
			args = append(args, wdir, w.Branch)
		} else {
			args = append(args, "--detach", wdir, w.Head)
		}
		if _, err := g.run(ctx, dir, args...); err != nil {
			return fmt.Errorf("worktree %s: %w", w.Path, err)
		}
		if w.Snapshot != "" {
			if _, err := g.run(ctx, wdir, "restore", "--source="+w.Snapshot, "--worktree", "--", ":/"); err != nil {
				return fmt.Errorf("restore the changes of worktree %s: %w", w.Path, err)
			}
		}
	}
	return nil
}

func (g *projectGit) prepared(ctx context.Context, rel, dir string, exported bool, worktrees int) (proto.PreparedRepo, error) {
	p := proto.PreparedRepo{Path: rel, Exported: exported, Worktrees: worktrees, Head: g.line(ctx, dir, "rev-parse", "--verify", "--quiet", "HEAD"),
		Branch: g.line(ctx, dir, "symbolic-ref", "--short", "--quiet", "HEAD")}
	var err error
	if p.Dirty, p.Untracked, _, err = g.status(ctx, dir); err != nil {
		return p, err
	}
	if st := g.line(ctx, dir, "stash", "list", "--format=%H"); st != "" {
		p.Stashes = len(strings.Split(st, "\n"))
	}
	return p, nil
}

// restoreRepo materializes an exported repository from its bundle: every ref, the remotes and upstreams, the checked-out
// branch, the work tree's uncommitted changes (as uncommitted changes again), the stashes and the worktrees.
func (g *projectGit) restoreRepo(ctx context.Context, space string, ex proto.ExportedRepo, bundle string) (proto.PreparedRepo, error) {
	dir := filepath.Join(space, filepath.FromSlash(ex.Path))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return proto.PreparedRepo{}, err
	}
	if _, err := g.run(ctx, dir, "init", "--quiet", "."); err != nil {
		return proto.PreparedRepo{}, err
	}
	if bundle != "" {
		if _, err := g.run(ctx, dir, "bundle", "verify", "--quiet", bundle); err != nil {
			return proto.PreparedRepo{}, fmt.Errorf("verify the bundle of %s: %w", ex.Path, err)
		}
		if _, err := g.runEnv(ctx, dir, 30*time.Minute, nil, "fetch", "--quiet", "--no-tags", "--update-head-ok", bundle,
			"+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*", "+refs/remotes/*:refs/remotes/*", "+refs/notes/*:refs/notes/*",
			"+"+exportRefs+"*:"+exportRefs+"*"); err != nil {
			return proto.PreparedRepo{}, fmt.Errorf("fetch the bundle of %s: %w", ex.Path, err)
		}
	}
	defer func() {
		for _, r := range strings.Fields(g.line(context.WithoutCancel(ctx), dir, "for-each-ref", "--format=%(refname)", exportRefs)) {
			_, _ = g.run(context.WithoutCancel(ctx), dir, "update-ref", "-d", r)
		}
	}()
	for _, r := range ex.Remotes {
		if _, err := g.run(ctx, dir, "remote", "add", r.Name, r.URL); err != nil {
			return proto.PreparedRepo{}, err
		}
	}
	for _, b := range ex.Branches {
		if err := g.setUpstream(ctx, dir, b); err != nil {
			return proto.PreparedRepo{}, err
		}
	}
	switch {
	case ex.Branch != "":
		if _, err := g.run(ctx, dir, "symbolic-ref", "HEAD", "refs/heads/"+ex.Branch); err != nil {
			return proto.PreparedRepo{}, err
		}
		if ex.Head != "" {
			if _, err := g.run(ctx, dir, "checkout", "--quiet", "--force", ex.Branch); err != nil {
				return proto.PreparedRepo{}, err
			}
		}
	case ex.Head != "":
		if _, err := g.run(ctx, dir, "checkout", "--quiet", "--force", "--detach", ex.Head); err != nil {
			return proto.PreparedRepo{}, err
		}
	}
	if ex.Snapshot != "" {
		if _, err := g.run(ctx, dir, "restore", "--source="+ex.Snapshot, "--worktree", "--", ":/"); err != nil {
			return proto.PreparedRepo{}, fmt.Errorf("restore the uncommitted changes of %s: %w", ex.Path, err)
		}
	}
	for i := len(ex.Stashes) - 1; i >= 0; i-- { // oldest first, so stash@{0} is the newest again
		s := ex.Stashes[i]
		if _, err := g.runEnv(ctx, dir, time.Minute, exportIdent, "stash", "store", "-m", s.Message, s.Commit); err != nil {
			return proto.PreparedRepo{}, fmt.Errorf("restore stash %d of %s: %w", i, ex.Path, err)
		}
	}
	if err := g.addWorktrees(ctx, space, dir, ex.Worktrees); err != nil {
		return proto.PreparedRepo{}, err
	}
	return g.prepared(ctx, ex.Path, dir, true, len(ex.Worktrees))
}

// writeLoose archives the loose files (paths relative to space) as tar.gz: regular files and symlinks only.
func writeLoose(space string, paths []string, out string, maxBytes int64) error {
	f, err := os.OpenFile(out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) //nolint:gosec // the agent's own relocation folder
	if err != nil {
		return err
	}
	defer f.Close()
	zw := gzip.NewWriter(f)
	tw := tar.NewWriter(zw)
	var total int64
	for _, rel := range paths {
		p := filepath.Join(space, filepath.FromSlash(rel))
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		hdr := &tar.Header{Name: rel, ModTime: fi.ModTime(), Mode: int64(fi.Mode().Perm())}
		switch {
		case fi.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			hdr.Typeflag, hdr.Linkname = tar.TypeSymlink, target
		case fi.Mode().IsRegular():
			hdr.Typeflag, hdr.Size = tar.TypeReg, fi.Size()
			total += fi.Size()
			if total > maxBytes {
				return fmt.Errorf("the loose files exceed %d bytes", maxBytes)
			}
		default:
			continue
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if hdr.Typeflag == tar.TypeReg {
			src, err := os.Open(p) //nolint:gosec // a file in the space folder, found by the walk (symlinks are not followed)
			if err != nil {
				return err
			}
			_, err = io.CopyN(tw, src, fi.Size())
			_ = src.Close()
			if err != nil {
				return err
			}
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return zw.Close()
}

// readLoose unpacks a loose-file archive into space. Paths must stay inside the folder, nothing existing is
// overwritten, and symlinks are created last so no file is ever written through one.
func readLoose(space, archive string, maxBytes int64) (int, error) {
	f, err := os.Open(archive) //nolint:gosec // the agent's own relocation folder
	if err != nil {
		return 0, err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return 0, err
	}
	tr := tar.NewReader(zr)
	type link struct{ path, target string }
	var links []link
	n := 0
	var total int64
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return n, err
		}
		clean := filepath.Clean(filepath.FromSlash(hdr.Name))
		if clean == "." || filepath.IsAbs(clean) || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || clean == ".." ||
			underReserved(clean) || clean == "worktrees" {
			return n, fmt.Errorf("unsafe path %q in the archive", hdr.Name)
		}
		dst := filepath.Join(space, clean)
		if err := noSymlinkParents(space, clean); err != nil {
			return n, err
		}
		if _, err := os.Lstat(dst); err == nil {
			return n, fmt.Errorf("%s already exists on the destination", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeSymlink:
			links = append(links, link{clean, hdr.Linkname})
		case tar.TypeReg:
			total += hdr.Size
			if total > maxBytes {
				return n, fmt.Errorf("the archive exceeds %d bytes", maxBytes)
			}
			if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
				return n, err
			}
			out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, os.FileMode(hdr.Mode&0o755)|0o600) //nolint:gosec // checked above
			if err != nil {
				return n, err
			}
			_, err = io.CopyN(out, tr, hdr.Size)
			_ = out.Close()
			if err != nil {
				return n, err
			}
			n++
		}
	}
	// Each symlink is checked again when it is created: an earlier link of the same archive may now be a parent of
	// its path (security review SR-L04).
	for _, l := range links {
		if err := noSymlinkParents(space, l.path); err != nil {
			return n, err
		}
		dst := filepath.Join(space, l.path)
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return n, err
		}
		if err := noSymlinkParents(space, l.path); err != nil {
			return n, err
		}
		if err := os.Symlink(l.target, dst); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// noSymlinkParents refuses a path whose existing parent folders (inside space) include a symlink.
func noSymlinkParents(space, rel string) error {
	cur := space
	parts := strings.Split(filepath.Dir(rel), string(filepath.Separator))
	for _, p := range parts {
		if p == "." || p == "" {
			continue
		}
		cur = filepath.Join(cur, p)
		fi, err := os.Lstat(cur)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%s goes through a symlink", rel)
		}
	}
	return nil
}
