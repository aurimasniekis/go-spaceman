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
	"time"

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
	// fetch fetches the remote's branches from the configured URL (so their commits are in the repository) and returns
	// the commits the remote's branches point at, as the remote itself lists them (git ls-remote), never as refs in the
	// repository, which agents can write. An error refuses the removal.
	fetch func(ctx context.Context, repoDir string) (tips []string, err error)
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
	tips, err := h.fetch(ctx, repo)
	if err != nil {
		var r *WorktreeRefusal
		if errors.As(err, &r) {
			return out, err
		}
		return out, refuse(RefuseFetch, "fetch failed, so whether the remote has the work is not known: %s", truncateStr(err.Error(), 300))
	}
	for _, t := range tips {
		if !isObjectID(t) {
			return out, refuse(RefuseFetch, "the remote listed an invalid commit")
		}
	}

	// From here on no launch prepares a worktree or records its work folder in this space until the removal is done.
	unlock := h.lock()
	defer unlock()
	if who, err := h.inUse(ctx, realDir); err != nil {
		return out, refuse(RefuseLiveRun, "the runs using the worktree are not known: %s", truncateStr(err.Error(), 300))
	} else if who != "" {
		return out, refuse(RefuseLiveRun, "%s", who)
	}
	if err := cleanTree(ctx, g, realDir); err != nil {
		return out, err
	}
	head, err := strictGit(ctx, g, realDir, "rev-parse", "--verify", "--quiet", "HEAD^{commit}")
	if head = strings.TrimSpace(head); err != nil || head == "" {
		return out, refuse(RefuseUnpushed, "the worktree has no commit checked out")
	}
	out.Head = head
	if len(tips) == 0 {
		return out, refuse(RefuseUnpushed, "the remote has no branches")
	}
	seen := map[string]bool{}
	args := []string{"rev-list", "-n", "1", head, "--not"}
	for _, t := range tips {
		if !seen[t] {
			seen[t] = true
			args = append(args, t)
		}
	}
	// Grafts rewrite history for every reader like replace refs do; a repository with them is refused.
	common, err := g.run(ctx, repo, "rev-parse", "--git-common-dir")
	if common = strings.TrimSpace(common); err != nil || common == "" {
		return out, refuse(RefuseGit, "git rev-parse: the repository's git folder is not known")
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(repo, common)
	}
	if _, err := os.Lstat(filepath.Join(common, "info", "grafts")); !errors.Is(err, fs.ErrNotExist) {
		return out, refuse(RefuseUnpushed, "the repository has grafts (info/grafts), so its history can't be trusted")
	}
	ahead, err := strictGit(ctx, g, repo, args...)
	if err != nil {
		return out, refuse(RefuseGit, "git rev-list: %s", truncateStr(err.Error(), 300))
	}
	if strings.TrimSpace(ahead) != "" {
		return out, refuse(RefuseUnpushed, "HEAD %s has commits the remote's branches do not have", short(head))
	}
	// Right before the removal: the folder is still the one checked and HEAD has not moved (a process SpaceMan did not
	// start could commit or swap the folder meanwhile).
	if fi, err := os.Lstat(dir); err != nil || fi.Mode()&fs.ModeSymlink != 0 || !fi.IsDir() {
		return out, refuse(RefuseOutside, "worktrees/%s changed during the removal", req.Name)
	}
	if again, err := filepath.EvalSymlinks(dir); err != nil || again != realDir {
		return out, refuse(RefuseOutside, "worktrees/%s changed during the removal", req.Name)
	}
	if again, err := strictGit(ctx, g, realDir, "rev-parse", "--verify", "--quiet", "HEAD^{commit}"); err != nil || strings.TrimSpace(again) != head {
		return out, refuse(RefuseUnpushed, "HEAD moved during the removal")
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

// cleanTree refuses a worktree with anything of its own that removing the folder would lose:
//   - staged changes, and tracked files whose content differs from HEAD (hashed, never judged by stat data);
//   - tracked files git was told not to look at (assume-unchanged, skip-worktree, sparse checkout): their edits are hidden
//     from git status and from git worktree remove;
//   - untracked files, judged by the tree's .gitignore files only: the repository's info/exclude and core.excludesFile
//     are ignored here, because agents can write them to hide any file;
//   - a git repository anywhere below the top (ignored or not), which may hold its own unpushed work.
func cleanTree(ctx context.Context, g *projectGit, dir string) error {
	// Staged changes: the index against HEAD (no stat data involved).
	staged, err := strictGit(ctx, g, dir, "diff-index", "--cached", "--name-only", "-z", "HEAD", "--")
	if err != nil {
		return refuse(RefuseGit, "git diff-index: %s", truncateStr(err.Error(), 300))
	}
	if p := firstPath(staged); p != "" {
		return refuse(RefuseDirty, "staged changes, e.g. %s", p)
	}
	// Changed files: every tracked file's content against HEAD, through a fresh index read from HEAD, which holds no
	// stat data, so git hashes each file instead of trusting the index's cached stat data (which edits within the same
	// second, or repository configuration, can make match).
	tmp, err := os.MkdirTemp("", "smwt-")
	if err != nil {
		return refuse(RefuseGit, "a temporary index: %s", err.Error())
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	env := []string{"GIT_NO_REPLACE_OBJECTS=1", "GIT_INDEX_FILE=" + filepath.Join(tmp, "index")}
	opts := strictOpts
	for _, args := range [][]string{{"read-tree", "HEAD"}, {"update-index", "-q", "--refresh"}} {
		if _, err := g.runEnv(ctx, dir, 10*time.Minute, env, append(append([]string{}, opts...), args...)...); err != nil {
			return refuse(RefuseGit, "git %s: %s", args[0], truncateStr(err.Error(), 300))
		}
	}
	changed, err := g.runEnv(ctx, dir, 10*time.Minute, env, append(append([]string{}, opts...), "diff-files", "--name-only", "-z", "--")...)
	if err != nil {
		return refuse(RefuseGit, "git diff-files: %s", truncateStr(err.Error(), 300))
	}
	if p := firstPath(changed); p != "" {
		return refuse(RefuseDirty, "changed files, e.g. %s", p)
	}
	tagged, err := strictGit(ctx, g, dir, "ls-files", "-v", "-z")
	if err != nil {
		return refuse(RefuseGit, "git ls-files: %s", truncateStr(err.Error(), 300))
	}
	for e := range strings.SplitSeq(tagged, "\x00") {
		if len(e) > 2 && (e[0] == 'S' || (e[0] >= 'a' && e[0] <= 'z')) {
			return refuse(RefuseDirty, "%s is hidden from git status (assume-unchanged, skip-worktree or sparse checkout)", e[2:])
		}
	}
	others, err := strictGit(ctx, g, dir, "ls-files", "-z", "--others", "--exclude-per-directory=.gitignore")
	if err != nil {
		return refuse(RefuseGit, "git ls-files: %s", truncateStr(err.Error(), 300))
	}
	var untracked []string
	for p := range strings.SplitSeq(others, "\x00") {
		if p != "" {
			untracked = append(untracked, p)
		}
	}
	// An untracked .gitignore can ignore itself and everything beside it: only committed ones may hide files.
	ignored, err := strictGit(ctx, g, dir, "ls-files", "-z", "--others", "--ignored", "--exclude-per-directory=.gitignore")
	if err != nil {
		return refuse(RefuseGit, "git ls-files: %s", truncateStr(err.Error(), 300))
	}
	for p := range strings.SplitSeq(ignored, "\x00") {
		if p != "" && filepath.Base(p) == ".gitignore" {
			untracked = append(untracked, p)
		}
	}
	if len(untracked) > 0 {
		return refuse(RefuseUntracked, "%d untracked file(s), e.g. %s", len(untracked), strings.Join(untracked[:min(len(untracked), 5)], ", "))
	}
	var nested string
	werr := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Name() == ".git" && p != filepath.Join(dir, ".git") {
			nested = p[len(dir)+1:]
			return fs.SkipAll
		}
		return nil
	})
	if werr != nil {
		return refuse(RefuseGit, "reading the worktree: %s", truncateStr(werr.Error(), 300))
	}
	if nested != "" {
		return refuse(RefuseUntracked, "a git repository inside the worktree (%s) may hold work of its own", nested)
	}
	return nil
}

// firstPath is the first path of a NUL-separated git list, or "".
func firstPath(out string) string {
	for p := range strings.SplitSeq(out, "\x00") {
		if p != "" {
			return p
		}
	}
	return ""
}

// isObjectID reports whether s is a full SHA-1 or SHA-256 object name.
func isObjectID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// strictGit runs git for the removal's checks with what the repository's configuration must not change (strictOpts),
// and history is read as it is, without replace
// refs (GIT_NO_REPLACE_OBJECTS), which agents can write to make any commit look like an ancestor of another.
func strictGit(ctx context.Context, g *projectGit, dir string, args ...string) (string, error) {
	return g.runEnv(ctx, dir, 2*time.Minute, []string{"GIT_NO_REPLACE_OBJECTS=1"},
		append(append([]string{}, strictOpts...), args...)...)
}

// strictOpts are the configuration values the removal's checks never take from the repository: full stat comparison,
// case-sensitive names (core.ignoreCase would let an untracked WORK.TXT pass as the tracked work.txt) and file modes.
var strictOpts = []string{"-c", "core.trustctime=true", "-c", "core.checkStat=default", "-c", "core.ignoreCase=false", "-c", "core.fileMode=true"}

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
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec // a fixed name in the run's own directory
	if err != nil {
		return err
	}
	_, err = f.WriteString(p)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(rd, workdirFile)); err != nil {
		return err
	}
	if d, err := os.Open(rd); err == nil { //nolint:gosec // the run's own directory
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
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
		w := strings.TrimSpace(string(b))
		if w == "" || !filepath.IsAbs(w) {
			return fmt.Sprintf("live run %s has an unreadable work folder record", r.RunID), nil
		}
		if w = filepath.Clean(w); within(w, dir) || within(dir, w) {
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
	fetch := func(ctx context.Context, repoDir string) ([]string, error) {
		if src.URL == "" {
			return nil, refuse(RefuseNoRemote, "repository %s has no configured URL to check the work against", src.Path)
		}
		if err := proto.ValidateRepoURL(src.URL, true); err != nil {
			return nil, refuse(RefuseNoRemote, "repository %s: url %s", src.Path, err.Error())
		}
		auth, err := newRepoAuth(ctx, src)
		if err != nil {
			return nil, err
		}
		defer auth.Close()
		l := a.repoLock(spaceID)
		l.Lock()
		defer l.Unlock()
		name := g.remoteName(ctx, repoDir, src.URL)
		// The fetch brings the remote's commits into the repository (as fetchSource does: tracking refs only, nothing
		// pruned); the remote's own list of its branches then says what it has. A branch pushed after the fetch names a
		// commit the repository lacks, and the check fails closed on it.
		if _, err := g.runEnv(ctx, repoDir, fetchTimeout, auth.Env(), "fetch", "--quiet", "--no-tags", "--", src.URL,
			"+refs/heads/*:refs/remotes/"+name+"/*"); err != nil {
			return nil, sshError(err, src.SSHKey != "")
		}
		ls, err := g.runEnv(ctx, repoDir, lsRemoteBudget, auth.Env(), "ls-remote", "--heads", "--", src.URL)
		if err != nil {
			return nil, sshError(err, src.SSHKey != "")
		}
		var tips []string
		for ln := range strings.SplitSeq(strings.TrimSpace(ls), "\n") {
			if sha, _, ok := strings.Cut(ln, "\t"); ok {
				tips = append(tips, sha)
			}
		}
		return tips, nil
	}
	return removeWorktreeSafely(ctx, g, worktreeRemoval{Space: space, Repo: src.Path, Name: name}, removalHooks{
		fetch: fetch,
		lock:  func() func() { return a.worktreeLock(spaceID) },
		inUse: func(ctx context.Context, dir string) (string, error) { return a.worktreeInUse(ctx, spaceID, dir) },
	})
}
