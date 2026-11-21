package agent

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// Worktree removal (ADR-0106). Every removal of a worktree goes through removeWorktreeSafely, whatever asks for it (an
// agent tool, the scheduled cleanup or the auto-prune of a finished task). It removes only what can be rebuilt from the
// remote: a worktree of a configured repository, inside the space's worktrees folder, that no live or launching run
// uses, with nothing changed or untracked, whose HEAD the remote has (checked after a fetch). Anything it can't be sure
// of is refused: a failed fetch, a git error, a run whose work folder is not known. It never forces: git's own
// "worktree remove" (which refuses an unclean or locked worktree by itself) followed by "worktree prune". The branch is
// kept: removing a worktree deletes a checkout, never a ref.

// workdirFile holds a run's work folder (the space folder's real path joined with the folder the run works in), written
// at launch under the space's worktree lock, so a removal knows which runs use which worktree. It survives an agent
// restart in the run's directory.
const workdirFile = "workdir"

// Refusal codes of a worktree removal (WorktreeRefusal.Code).
const (
	RefuseInvalid     = "invalid"      // a malformed request: repository folder or worktree name
	RefuseOutside     = "outside"      // not a folder directly inside the space's worktrees folder, or reached through a symlink
	RefuseNotWorktree = "not_worktree" // not a linked worktree of that repository (or its main folder)
	RefuseLocked      = "locked"       // git has the worktree locked
	RefuseNoRemote    = "no_remote"    // the repository has no configured URL to check the HEAD against
	RefuseFetch       = "fetch_failed" // the fetch that refreshes the remote-tracking refs failed
	RefuseLiveRun     = "live_run"     // a live or launching run works in it, or a live run's work folder is not known
	RefuseDirty       = "dirty"        // changed tracked files
	RefuseUntracked   = "untracked"    // untracked files that are not ignored
	RefuseUnpushed    = "unpushed"     // HEAD is not reachable from the remote's tracking refs, or there is no HEAD
	RefuseGit         = "git_error"    // a git command failed, so the state is not known
)

// WorktreeRefusal is why a worktree was not removed.
type WorktreeRefusal struct {
	Code   string
	Reason string
}

func (e *WorktreeRefusal) Error() string { return "worktree not removed: " + e.Reason }

func refuse(code, format string, args ...any) error {
	return &WorktreeRefusal{Code: code, Reason: fmt.Sprintf(format, args...)}
}

// RefusalCode is the refusal code of a removal error, or "" when err is not a refusal.
func RefusalCode(err error) string {
	var r *WorktreeRefusal
	if errors.As(err, &r) {
		return r.Code
	}
	return ""
}

// worktreeRemoval asks for one worktree to be removed.
type worktreeRemoval struct {
	Space string // the space folder
	Repo  string // the repository folder, relative to the space folder
	Name  string // the worktree's folder name in the space's worktrees folder
}

// removalHooks are the parts of a removal that need the agent.
type removalHooks struct {
	// fetch refreshes the repository's remote-tracking refs of one remote from the configured URL, dropping the refs of
	// branches the remote no longer has, and returns that remote's name. An error refuses the removal.
	fetch func(ctx context.Context, repoDir string) (remote string, err error)
	// lock takes the space's worktree lock (launches take it while they prepare a worktree and record their work
	// folder) and returns its unlock.
	lock func() func()
	// inUse says which live or launching run uses dir ("" when none). It is called under lock; an error refuses.
	inUse func(ctx context.Context, dir string) (string, error)
}

// removedWorktree is what a removal removed.
type removedWorktree struct {
	Path   string // relative to the space folder
	Branch string // kept
	Head   string
}

// removeWorktreeSafely removes one worktree, or refuses with a *WorktreeRefusal and leaves everything as it was.
func removeWorktreeSafely(ctx context.Context, g *projectGit, req worktreeRemoval, h removalHooks) (removedWorktree, error) {
	var out removedWorktree
	if !validRepoPath(req.Repo) {
		return out, refuse(RefuseInvalid, "invalid repository folder")
	}
	if req.Name == "" || req.Name == "." || req.Name == ".." || strings.ContainsAny(req.Name, "/\\\x00") {
		return out, refuse(RefuseInvalid, "invalid worktree name")
	}
	repoRel := filepath.FromSlash(req.Repo)
	repo := filepath.Join(req.Space, repoRel)
	if err := noSymlinkParents(req.Space, repoRel); err != nil {
		return out, refuse(RefuseOutside, "the repository folder %s", err)
	}
	if fi, err := os.Lstat(repo); err != nil || fi.Mode()&fs.ModeSymlink != 0 || !isRepoDir(repo) {
		return out, refuse(RefuseNotWorktree, "%s is not a repository's main folder", req.Repo)
	}
	// The worktree must be a real folder directly inside a real worktrees folder of the space: no symlink anywhere on
	// the way, and its resolved path must still be there (the space folder itself may sit behind a symlink).
	root := filepath.Join(req.Space, "worktrees")
	dir := filepath.Join(root, req.Name)
	for _, p := range []string{root, dir} {
		fi, err := os.Lstat(p)
		if err != nil {
			return out, refuse(RefuseOutside, "worktrees/%s is not there", req.Name)
		}
		if fi.Mode()&fs.ModeSymlink != 0 || !fi.IsDir() {
			return out, refuse(RefuseOutside, "%s is a symlink or not a folder", p[len(req.Space)+1:])
		}
	}
	realSpace, err1 := filepath.EvalSymlinks(req.Space)
	realDir, err2 := filepath.EvalSymlinks(dir)
	if err1 != nil || err2 != nil || filepath.Dir(realDir) != filepath.Join(realSpace, "worktrees") {
		return out, refuse(RefuseOutside, "worktrees/%s resolves outside the space's worktrees folder", req.Name)
	}
	out.Path = filepath.ToSlash(filepath.Join("worktrees", req.Name))

	// It must be one of this repository's linked worktrees (never the main one) and not locked.
	wt, err := listedWorktree(ctx, g, repo, dir, realDir)
	if err != nil {
		return out, err
	}
	if wt.locked {
		return out, refuse(RefuseLocked, "git has %s locked", out.Path)
	}
	out.Branch = wt.branch

	// The fetch runs before the lock: it can take minutes, and launches must not wait for it. What it gives (fresher
	// remote-tracking refs) only makes the check below stricter, whatever happens to the worktree meanwhile.
	if h.fetch == nil {
		return out, refuse(RefuseNoRemote, "no remote to check against")
	}
	remote, err := h.fetch(ctx, repo)
	if err != nil {
		var r *WorktreeRefusal
		if errors.As(err, &r) {
			return out, err
		}
		return out, refuse(RefuseFetch, "fetch failed, so whether the remote has the work is not known: %s", truncateStr(err.Error(), 300))
	}
	if !validRef(remote) || strings.Contains(remote, "/") {
		return out, refuse(RefuseFetch, "invalid remote name")
	}

	// From here on no launch prepares a worktree or records its work folder in this space until the removal is done.
	unlock := h.lock()
	defer unlock()
	if who, err := h.inUse(ctx, realDir); err != nil {
		return out, refuse(RefuseLiveRun, "the runs using the worktree are not known: %s", truncateStr(err.Error(), 300))
	} else if who != "" {
		return out, refuse(RefuseLiveRun, "%s", who)
	}
	st, err := g.run(ctx, realDir, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignore-submodules=none")
	if err != nil {
		return out, refuse(RefuseGit, "git status: %s", truncateStr(err.Error(), 300))
	}
	if dirty, untracked, paths := statusCounts(st); dirty > 0 {
		return out, refuse(RefuseDirty, "%d changed file(s), e.g. %s", dirty, strings.Join(paths, ", "))
	} else if untracked > 0 {
		return out, refuse(RefuseUntracked, "%d untracked file(s), e.g. %s", untracked, strings.Join(paths, ", "))
	}
	head, err := g.run(ctx, realDir, "rev-parse", "--verify", "--quiet", "HEAD^{commit}")
	if head = strings.TrimSpace(head); err != nil || head == "" {
		return out, refuse(RefuseUnpushed, "the worktree has no commit checked out")
	}
	out.Head = head
	ahead, err := g.run(ctx, repo, "rev-list", "-n", "1", head, "--not", "--remotes="+remote)
	if err != nil {
		return out, refuse(RefuseGit, "git rev-list: %s", truncateStr(err.Error(), 300))
	}
	if strings.TrimSpace(ahead) != "" {
		return out, refuse(RefuseUnpushed, "HEAD %s has commits the remote %s does not have", short(head), remote)
	}
	// Never --force: git refuses by itself an unclean or locked worktree, a second check of what is above.
	if _, err := g.run(ctx, repo, "worktree", "remove", realDir); err != nil {
		return out, refuse(RefuseGit, "git worktree remove: %s", truncateStr(err.Error(), 300))
	}
	if _, err := g.run(ctx, repo, "worktree", "prune"); err != nil {
		return out, fmt.Errorf("the worktree was removed but git worktree prune failed: %w", err)
	}
	return out, nil
}

type listedTree struct {
	branch string
	locked bool
}

// listedWorktree finds dir among repo's linked worktrees (git worktree list), never the main worktree.
func listedWorktree(ctx context.Context, g *projectGit, repo, dir, realDir string) (listedTree, error) {
	list, err := g.run(ctx, repo, "worktree", "list", "--porcelain")
	if err != nil {
		return listedTree{}, refuse(RefuseGit, "git worktree list: %s", truncateStr(err.Error(), 300))
	}
	var cur *listedTree
	found, first := false, true
	sc := bufio.NewScanner(strings.NewReader(list))
	for sc.Scan() {
		ln := sc.Text()
		if p, ok := strings.CutPrefix(ln, "worktree "); ok {
			if found {
				break
			}
			if !first && (p == dir || p == realDir || realPath(p) == realDir) {
				found, cur = true, &listedTree{}
			}
			first = false
			continue
		}
		if !found {
			continue
		}
		switch {
		case strings.HasPrefix(ln, "branch "):
			cur.branch = strings.TrimPrefix(strings.TrimPrefix(ln, "branch "), "refs/heads/")
		case ln == "locked" || strings.HasPrefix(ln, "locked "):
			cur.locked = true
		}
	}
	if !found {
		return listedTree{}, refuse(RefuseNotWorktree, "worktrees/%s is not a linked worktree of this repository", filepath.Base(dir))
	}
	return *cur, nil
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// worktreeLock is the space's worktree lock: a launch holds it while it prepares its worktree and records its work
// folder, a removal while it checks and removes.
func (a *Agent) worktreeLock(spaceID string) func() { return a.runLock("worktrees/" + spaceID) }

// recordWorkdir writes a launch's work folder into the run's directory (workdirFile), as the space folder's real path
// joined with the folder relative to it, so it compares with a removal's resolved worktree path.
func (a *Agent) recordWorkdir(runID, space, work string) error {
	rel, err := filepath.Rel(space, work)
	if err != nil {
		return err
	}
	rd := a.runDir(runID)
	if err := os.MkdirAll(rd, 0o700); err != nil {
		return err
	}
	p := filepath.Join(realPath(space), rel)
	if r, err := filepath.EvalSymlinks(work); err == nil {
		p = r
	}
	tmp := filepath.Join(rd, workdirFile+".tmp")
	if err := os.WriteFile(tmp, []byte(p), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(rd, workdirFile))
}

// worktreeInUse names the space's live or launching run that works in dir, in a folder inside it, or in a folder that
// contains it; a live run whose work folder is not recorded (launched by an older agent, or still launching) counts as
// using it. Called under the space's worktree lock.
func (a *Agent) worktreeInUse(ctx context.Context, spaceID, dir string) (string, error) {
	runs, err := a.j.runs(ctx, true)
	if err != nil {
		return "", err
	}
	for _, r := range runs {
		if r.SpaceID != spaceID {
			continue
		}
		b, err := os.ReadFile(filepath.Join(a.runDir(r.RunID), workdirFile)) //nolint:gosec // a fixed name in the run's own directory
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Sprintf("live run %s has no recorded work folder (still launching, or started by an older agent)", r.RunID), nil
		}
		if err != nil {
			return "", err
		}
		if w := filepath.Clean(string(b)); within(w, dir) || within(dir, w) {
			return fmt.Sprintf("live run %s works in it", r.RunID), nil
		}
	}
	return "", nil
}

// within reports whether p is base or inside it.
func within(p, base string) bool {
	rel, err := filepath.Rel(base, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// removeWorktree removes a worktree of a configured repository of the space (ADR-0106), through removeWorktreeSafely.
// The fetch uses the repository's configured URL and SpaceMan key, never the URL in the repository's own
// configuration, which agents can change.
func (a *Agent) removeWorktree(ctx context.Context, spaceID, space string, src proto.RepoSource, name string) (removedWorktree, error) {
	g, err := newProjectGit()
	if err != nil {
		return removedWorktree{}, refuse(RefuseGit, "%s", err.Error())
	}
	fetch := func(ctx context.Context, repoDir string) (string, error) {
		if src.URL == "" {
			return "", refuse(RefuseNoRemote, "repository %s has no configured URL to check the work against", src.Path)
		}
		if err := proto.ValidateRepoURL(src.URL, true); err != nil {
			return "", refuse(RefuseNoRemote, "repository %s: url %s", src.Path, err.Error())
		}
		auth, err := newRepoAuth(ctx, src)
		if err != nil {
			return "", err
		}
		defer auth.Close()
		l := a.repoLock(spaceID)
		l.Lock()
		defer l.Unlock()
		name := g.remoteName(ctx, repoDir, src.URL)
		// --prune drops only this remote's tracking refs of branches the remote no longer has, so a branch deleted on
		// the remote stops counting as pushed. Local branches and the work tree are untouched.
		if _, err := g.runEnv(ctx, repoDir, fetchTimeout, auth.Env(), "fetch", "--quiet", "--no-tags", "--prune", "--", src.URL,
			"+refs/heads/*:refs/remotes/"+name+"/*"); err != nil {
			return "", sshError(err, src.SSHKey != "")
		}
		return name, nil
	}
	return removeWorktreeSafely(ctx, g, worktreeRemoval{Space: space, Repo: src.Path, Name: name}, removalHooks{
		fetch: fetch,
		lock:  func() func() { return a.worktreeLock(spaceID) },
		inUse: func(ctx context.Context, dir string) (string, error) { return a.worktreeInUse(ctx, spaceID, dir) },
	})
}
