package agent

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// rmEnv is an agent with a journal, a space folder holding a clone of a local bare repository (the remote) at
// repos/api, and that repository configured with the bare repository's path as its URL. Everything is in temp folders.
type rmEnv struct {
	a       *Agent
	spaceID string
	space   string
	bare    string
	seed    string
	repo    string
	src     proto.RepoSource
}

func newRmEnv(t *testing.T) *rmEnv {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	space, bare, seed := reposFixture(t)
	repo := filepath.Join(space, "repos", "api")
	gitRun(t, space, "clone", "-q", bare, repo)
	data := t.TempDir()
	j, err := OpenJournal(context.Background(), filepath.Join(data, "journal"), nil, clock.System{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = j.Close() })
	return &rmEnv{a: New(j, Options{DataDir: data, Clock: clock.System{}}), spaceID: ids.New(ids.Space), space: space, bare: bare, seed: seed,
		repo: repo, src: proto.RepoSource{ID: "rep_api", Name: "api", Path: "repos/api", URL: bare, DefaultBranch: "main"}}
}

// worktree prepares a writer's worktree as a launch does, with one commit on its branch, pushed when push is set.
func (e *rmEnv) worktree(t *testing.T, branch string, push bool) string {
	t.Helper()
	dir, err := prepareWorktree(context.Background(), e.space, proto.Worktree{Repo: "repos/api", Branch: branch})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, ".gitignore"), "*.log\n")
	writeFile(t, filepath.Join(dir, "work.txt"), "done\n")
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-q", "-m", "work")
	if push {
		gitRun(t, dir, "push", "-q", "origin", branch)
	}
	return dir
}

func (e *rmEnv) remove(name string) (removedWorktree, error) {
	return e.a.removeWorktree(context.Background(), e.spaceID, e.space, e.src, name)
}

// liveRun journals a live run of the space (pending, as a launch first writes it) and, when work is set, its work folder.
func (e *rmEnv) liveRun(t *testing.T, spaceID, work string) string {
	t.Helper()
	id := ids.New(ids.Run)
	if err := e.a.j.insertRun(context.Background(), runRow{RunID: id, SpaceID: spaceID, Generation: 1, Kind: "worker", Role: "r", InstanceID: "ins_1", TerminalName: "t_" + id}); err != nil {
		t.Fatal(err)
	}
	if work != "" {
		if err := e.a.recordWorkdir(id, e.space, work); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func (e *rmEnv) endRun(t *testing.T, id string) {
	t.Helper()
	r, err := e.a.j.getRun(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	r.State = proto.RunExited
	if err := e.a.j.updateRun(context.Background(), r); err != nil {
		t.Fatal(err)
	}
}

func wantRefused(t *testing.T, err error, code string) {
	t.Helper()
	if got := RefusalCode(err); got != code {
		t.Fatalf("want refusal %q, got %q (%v)", code, got, err)
	}
}

func mustExist(t *testing.T, p string) {
	t.Helper()
	if _, err := os.Lstat(p); err != nil {
		t.Fatalf("%s must still be there: %v", p, err)
	}
}

// ADR-0106: a clean worktree whose HEAD the remote has is removed with git worktree remove and prune; ignored files go
// with it, the branch stays, and a later launch on the branch gets a fresh worktree at the same commit.
func TestRemoveWorktreeSafelyRemovesPushedWork(t *testing.T) {
	e := newRmEnv(t)
	dir := e.worktree(t, "spaceman/tsk-1", true)
	writeFile(t, filepath.Join(dir, "build.log"), "ignored\n")
	head := gitRun(t, dir, "rev-parse", "HEAD")

	got, err := e.remove("spaceman-tsk-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Path != "worktrees/spaceman-tsk-1" || got.Branch != "spaceman/tsk-1" || got.Head != head {
		t.Fatalf("removed %+v", got)
	}
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the worktree folder is still there: %v", err)
	}
	if list := gitRun(t, e.repo, "worktree", "list", "--porcelain"); strings.Contains(list, "spaceman-tsk-1") {
		t.Fatalf("git still lists the worktree:\n%s", list)
	}
	if b := gitRun(t, e.repo, "rev-parse", "refs/heads/spaceman/tsk-1"); b != head {
		t.Fatalf("the branch moved or went: %s", b)
	}
	again, err := prepareWorktree(context.Background(), e.space, proto.Worktree{Repo: "repos/api", Branch: "spaceman/tsk-1"})
	if err != nil || gitRun(t, again, "rev-parse", "HEAD") != head {
		t.Fatalf("relaunch on the branch: %s %v", again, err)
	}
	// A worktree with nothing of its own (HEAD on the pushed main) goes too.
	if _, err := prepareWorktree(context.Background(), e.space, proto.Worktree{Repo: "repos/api", Branch: "spaceman/tsk-2", Base: "origin/main"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.remove("spaceman-tsk-2"); err != nil {
		t.Fatal(err)
	}
}

// ADR-0106: changed tracked files, staged files and untracked files that are not ignored are refused, and the worktree
// is left exactly as it was.
func TestRemoveWorktreeSafelyRefusesUncleanTrees(t *testing.T) {
	e := newRmEnv(t)
	dir := e.worktree(t, "spaceman/dirty", true)

	writeFile(t, filepath.Join(dir, "README.md"), "changed\n")
	_, err := e.remove("spaceman-dirty")
	wantRefused(t, err, RefuseDirty)
	if b, _ := os.ReadFile(filepath.Join(dir, "README.md")); string(b) != "changed\n" {
		t.Fatal("the change was lost")
	}
	gitRun(t, dir, "checkout", "--", "README.md")

	writeFile(t, filepath.Join(dir, "staged.txt"), "staged\n")
	gitRun(t, dir, "add", "staged.txt")
	_, err = e.remove("spaceman-dirty")
	wantRefused(t, err, RefuseDirty)
	gitRun(t, dir, "reset", "-q", "staged.txt")

	// staged.txt is now untracked; one in a new folder is also seen (--untracked-files=all).
	_, err = e.remove("spaceman-dirty")
	wantRefused(t, err, RefuseUntracked)
	if err := os.Remove(filepath.Join(dir, "staged.txt")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "deep", "er", "notes.md"), "mine\n")
	_, err = e.remove("spaceman-dirty")
	wantRefused(t, err, RefuseUntracked)
	mustExist(t, filepath.Join(dir, "deep", "er", "notes.md"))
}

// ADR-0106: a HEAD the remote does not have is refused: a commit never pushed, and a pushed branch since deleted on the
// remote (its stale tracking ref stays, but only the remote's own branch list counts).
func TestRemoveWorktreeSafelyRefusesUnpushedWork(t *testing.T) {
	e := newRmEnv(t)
	dir := e.worktree(t, "spaceman/local", false)
	_, err := e.remove("spaceman-local")
	wantRefused(t, err, RefuseUnpushed)
	mustExist(t, filepath.Join(dir, "work.txt"))

	gone := e.worktree(t, "spaceman/gone", true)
	gitRun(t, e.seed, "push", "-q", "origin", "--delete", "spaceman/gone")
	if gitRun(t, e.repo, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/spaceman/gone") == "" {
		t.Fatal("setup: the stale tracking ref must exist before the removal")
	}
	_, err = e.remove("spaceman-gone")
	wantRefused(t, err, RefuseUnpushed)
	mustExist(t, filepath.Join(gone, "work.txt"))

	// A pushed commit followed by a local one is refused too.
	gitRun(t, gone, "push", "-q", "origin", "spaceman/gone")
	writeFile(t, filepath.Join(gone, "more.txt"), "more\n")
	gitRun(t, gone, "add", ".")
	gitRun(t, gone, "commit", "-q", "-m", "more")
	_, err = e.remove("spaceman-gone")
	wantRefused(t, err, RefuseUnpushed)
}

// ADR-0106: when the fetch fails (or there is no configured URL) whether the remote has the work is not known, so the
// removal fails closed, even for work the stale tracking refs say is pushed.
func TestRemoveWorktreeSafelyFailsClosedWithoutAFetch(t *testing.T) {
	e := newRmEnv(t)
	dir := e.worktree(t, "spaceman/pushed", true)
	e.src.URL = filepath.Join(t.TempDir(), "missing.git")
	_, err := e.remove("spaceman-pushed")
	wantRefused(t, err, RefuseFetch)
	mustExist(t, dir)

	e.src.URL = ""
	_, err = e.remove("spaceman-pushed")
	wantRefused(t, err, RefuseNoRemote)
	mustExist(t, dir)

	// A fetch hook's own error is a fetch failure too.
	g, _ := newProjectGit()
	_, err = removeWorktreeSafely(context.Background(), g, worktreeRemoval{Space: e.space, Repo: "repos/api", Name: "spaceman-pushed"}, removalHooks{
		fetch: func(context.Context, string) ([]string, error) { return nil, errors.New("network down") },
		lock:  func() func() { return func() {} },
		inUse: func(context.Context, string) (string, error) { return "", nil },
	})
	wantRefused(t, err, RefuseFetch)
	mustExist(t, dir)
}

// ADR-0106: a worktree a live run works in (or in a folder inside it, or a folder that contains it) is refused, and so is
// any worktree while a live run of the space has no recorded work folder. Ended runs and other spaces' runs don't count.
func TestRemoveWorktreeSafelyRefusesALiveRun(t *testing.T) {
	e := newRmEnv(t)
	dir := e.worktree(t, "spaceman/busy", true)

	owner := e.liveRun(t, e.spaceID, dir)
	_, err := e.remove("spaceman-busy")
	wantRefused(t, err, RefuseLiveRun)
	if !strings.Contains(err.Error(), owner) {
		t.Fatalf("the refusal must name the run: %v", err)
	}
	e.endRun(t, owner)

	inside := e.liveRun(t, e.spaceID, filepath.Join(dir, "sub"))
	_, err = e.remove("spaceman-busy")
	wantRefused(t, err, RefuseLiveRun)
	e.endRun(t, inside)

	whole := e.liveRun(t, e.spaceID, e.space)
	_, err = e.remove("spaceman-busy")
	wantRefused(t, err, RefuseLiveRun)
	e.endRun(t, whole)

	unknown := e.liveRun(t, e.spaceID, "")
	_, err = e.remove("spaceman-busy")
	wantRefused(t, err, RefuseLiveRun)
	e.endRun(t, unknown)

	// A live run elsewhere in the space, and one of another space in "this" folder, don't block it.
	e.liveRun(t, e.spaceID, filepath.Join(e.space, "work"))
	e.liveRun(t, ids.New(ids.Space), dir)
	if _, err := e.remove("spaceman-busy"); err != nil {
		t.Fatal(err)
	}

	// The check's own error refuses.
	dir2 := e.worktree(t, "spaceman/busy2", true)
	g, _ := newProjectGit()
	_, err = removeWorktreeSafely(context.Background(), g, worktreeRemoval{Space: e.space, Repo: "repos/api", Name: "spaceman-busy2"}, removalHooks{
		fetch: func(context.Context, string) ([]string, error) { return nil, nil },
		lock:  func() func() { return func() {} },
		inUse: func(context.Context, string) (string, error) { return "", errors.New("journal unreadable") },
	})
	wantRefused(t, err, RefuseLiveRun)
	mustExist(t, dir2)
}

// ADR-0106: a removal waits for a launch that holds the space's worktree lock, and then sees the run the launch recorded,
// so a launch onto a worktree never races its removal.
func TestRemoveWorktreeSafelyWaitsForALaunch(t *testing.T) {
	e := newRmEnv(t)
	dir := e.worktree(t, "spaceman/race", true)
	unlock := e.a.worktreeLock(e.spaceID) // a launch preparing its worktree
	done := make(chan error, 1)
	go func() {
		_, err := e.remove("spaceman-race")
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("the removal did not wait for the launch: %v", err)
	case <-time.After(500 * time.Millisecond):
	}
	e.liveRun(t, e.spaceID, dir) // the launch records its work folder, then lets go
	unlock()
	select {
	case err := <-done:
		wantRefused(t, err, RefuseLiveRun)
	case <-time.After(30 * time.Second):
		t.Fatal("the removal never finished")
	}
	mustExist(t, filepath.Join(dir, "work.txt"))
}

// ADR-0106: only a real folder directly inside the space's real worktrees folder that is a linked worktree of the named
// repository can be removed: no traversal, no symlink (of the worktrees folder, the worktree or the repository's
// parents), no unregistered folder, never a main folder, never a locked worktree.
func TestRemoveWorktreeSafelyRefusesPathEscapes(t *testing.T) {
	e := newRmEnv(t)
	dir := e.worktree(t, "spaceman/kept", true)
	for _, name := range []string{"", ".", "..", "../repos", "a/b", "spaceman-kept/..", "x\\y"} {
		_, err := e.remove(name)
		wantRefused(t, err, RefuseInvalid)
	}
	for _, repo := range []string{"", "../repos/api", "/repos/api", "worktrees/spaceman-kept", "repos/../repos/api"} {
		src := e.src
		src.Path = repo
		_, err := e.a.removeWorktree(context.Background(), e.spaceID, e.space, src, "spaceman-kept")
		wantRefused(t, err, RefuseInvalid)
	}

	// A worktree registered outside the worktrees folder, reached through a symlink inside it.
	other := filepath.Join(e.space, "other", "side")
	gitRun(t, e.repo, "worktree", "add", "-q", "-b", "side", other, "origin/main")
	if err := os.Symlink(other, filepath.Join(e.space, "worktrees", "side")); err != nil {
		t.Fatal(err)
	}
	_, err := e.remove("side")
	wantRefused(t, err, RefuseOutside)
	mustExist(t, other)

	// A symlink in the worktrees folder to a real worktree beside it: names must not alias.
	if err := os.Symlink(dir, filepath.Join(e.space, "worktrees", "alias")); err != nil {
		t.Fatal(err)
	}
	_, err = e.remove("alias")
	wantRefused(t, err, RefuseOutside)
	mustExist(t, filepath.Join(dir, "work.txt"))

	// A folder in worktrees that is not one of the repository's worktrees; the repository's main folder by symlink.
	writeFile(t, filepath.Join(e.space, "worktrees", "loose", "f.txt"), "x\n")
	_, err = e.remove("loose")
	wantRefused(t, err, RefuseNotWorktree)
	if err := os.Symlink(e.repo, filepath.Join(e.space, "worktrees", "main")); err != nil {
		t.Fatal(err)
	}
	_, err = e.remove("main")
	wantRefused(t, err, RefuseOutside)
	_, err = e.remove("missing")
	wantRefused(t, err, RefuseOutside)

	// A repository named through a symlinked parent folder.
	if err := os.Symlink(filepath.Join(e.space, "repos"), filepath.Join(e.space, "linked")); err != nil {
		t.Fatal(err)
	}
	src := e.src
	src.Path = "linked/api"
	_, err = e.a.removeWorktree(context.Background(), e.spaceID, e.space, src, "spaceman-kept")
	wantRefused(t, err, RefuseOutside)

	// A locked worktree.
	gitRun(t, e.repo, "worktree", "lock", dir)
	_, err = e.remove("spaceman-kept")
	wantRefused(t, err, RefuseLocked)
	gitRun(t, e.repo, "worktree", "unlock", dir)
	mustExist(t, filepath.Join(dir, "work.txt"))

	// The worktrees folder itself as a symlink to a folder outside the space.
	e2 := newRmEnv(t)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(e2.space, "worktrees")); err != nil {
		t.Fatal(err)
	}
	gitRun(t, e2.repo, "worktree", "add", "-q", "-b", "x", filepath.Join(outside, "x"), "origin/main")
	_, err = e2.remove("x")
	wantRefused(t, err, RefuseOutside)
	mustExist(t, filepath.Join(outside, "x", "README.md"))
}

// ADR-0106: every launch records its work folder under the space's worktree lock (the worktree's real path, or the
// folder it works in otherwise), and a removal compares against it.
func TestRecordWorkdir(t *testing.T) {
	e := newRmEnv(t)
	dir := e.worktree(t, "spaceman/rec", true)
	id := e.liveRun(t, e.spaceID, dir)
	b, err := os.ReadFile(filepath.Join(e.a.runDir(id), workdirFile))
	if err != nil || string(b) != realPath(dir) {
		t.Fatalf("recorded %q %v, want %q", b, err, realPath(dir))
	}
	// A work folder that does not exist yet is recorded under the space folder's real path.
	id2 := e.liveRun(t, e.spaceID, filepath.Join(e.space, "work", "new"))
	if b, _ := os.ReadFile(filepath.Join(e.a.runDir(id2), workdirFile)); string(b) != filepath.Join(realPath(e.space), "work", "new") {
		t.Fatalf("recorded %q", b)
	}
}

// ADR-0106 (review): edits git was told to hide (assume-unchanged, skip-worktree), untracked files hidden by the
// repository's own exclude rules, and a git repository inside an ignored folder are all refused.
func TestRemoveWorktreeSafelyRefusesHiddenWork(t *testing.T) {
	e := newRmEnv(t)
	dir := e.worktree(t, "spaceman/hidden", true)
	for _, flag := range []string{"--assume-unchanged", "--skip-worktree"} {
		gitRun(t, dir, "update-index", flag, "work.txt")
		writeFile(t, filepath.Join(dir, "work.txt"), "hidden edit\n")
		_, err := e.remove("spaceman-hidden")
		wantRefused(t, err, RefuseDirty)
		if b, _ := os.ReadFile(filepath.Join(dir, "work.txt")); string(b) != "hidden edit\n" {
			t.Fatal("the hidden edit was lost")
		}
		gitRun(t, dir, "update-index", strings.Replace(flag, "--", "--no-", 1), "work.txt")
		gitRun(t, dir, "checkout", "--", "work.txt")
	}

	// An exclude rule the repository's own configuration adds does not make an untracked file ignorable.
	gd := gitRun(t, e.repo, "rev-parse", "--git-common-dir")
	if !filepath.IsAbs(gd) {
		gd = filepath.Join(e.repo, gd)
	}
	writeFile(t, filepath.Join(gd, "info", "exclude"), "*\n")
	writeFile(t, filepath.Join(dir, "notes.md"), "mine\n")
	_, err := e.remove("spaceman-hidden")
	wantRefused(t, err, RefuseUntracked)
	writeFile(t, filepath.Join(gd, "info", "exclude"), "")
	if err := os.Remove(filepath.Join(dir, "notes.md")); err != nil {
		t.Fatal(err)
	}

	// A repository inside an ignored folder may hold its own unpushed work.
	nested := filepath.Join(dir, "dep.log")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	gitRun(t, nested, "init", "-q")
	_, err = e.remove("spaceman-hidden")
	wantRefused(t, err, RefuseUntracked)
	mustExist(t, filepath.Join(nested, ".git"))
}

// ADR-0106 (review): refs in the repository can't vouch for a commit: a symbolic ref planted in the remote's namespace
// pointing at the local branch does not make an unpushed HEAD pushed.
func TestRemoveWorktreeSafelyIgnoresPlantedRefs(t *testing.T) {
	e := newRmEnv(t)
	dir := e.worktree(t, "spaceman/planted", false)
	gitRun(t, e.repo, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/heads/spaceman/planted")
	gitRun(t, e.repo, "update-ref", "refs/remotes/origin/fake", "refs/heads/spaceman/planted")
	_, err := e.remove("spaceman-planted")
	wantRefused(t, err, RefuseUnpushed)
	mustExist(t, filepath.Join(dir, "work.txt"))
}

// ADR-0106 (review): a live run whose work folder record is empty or not absolute counts as using every worktree.
func TestRemoveWorktreeSafelyRefusesAnUnreadableRecord(t *testing.T) {
	e := newRmEnv(t)
	dir := e.worktree(t, "spaceman/rec2", true)
	for _, content := range []string{"", "  ", "worktrees/spaceman-rec2"} {
		id := e.liveRun(t, e.spaceID, "")
		if err := os.MkdirAll(e.a.runDir(id), 0o700); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(e.a.runDir(id), workdirFile), content)
		_, err := e.remove("spaceman-rec2")
		wantRefused(t, err, RefuseLiveRun)
		e.endRun(t, id)
	}
	mustExist(t, dir)
	if _, err := e.remove("spaceman-rec2"); err != nil {
		t.Fatal(err)
	}
}

// ADR-0106: a detached HEAD the remote has is removed; a detached commit of its own is refused.
func TestRemoveWorktreeSafelyDetachedHead(t *testing.T) {
	e := newRmEnv(t)
	dir := e.worktree(t, "spaceman/det", true)
	gitRun(t, dir, "checkout", "-q", "--detach")
	writeFile(t, filepath.Join(dir, "x.txt"), "x\n")
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-q", "-m", "detached")
	_, err := e.remove("spaceman-det")
	wantRefused(t, err, RefuseUnpushed)
	gitRun(t, dir, "checkout", "-q", "--detach", "origin/spaceman/det")
	got, err := e.remove("spaceman-det")
	if err != nil || got.Branch != "" {
		t.Fatalf("detached pushed HEAD: %+v %v", got, err)
	}
}

// ADR-0106 (second review): replace refs and grafts can't fake the pushed check, an untracked .gitignore can't hide
// itself and its folder, and repository configuration can't make git skip a content check.
func TestRemoveWorktreeSafelyResistsRepositoryTricks(t *testing.T) {
	e := newRmEnv(t)
	dir := e.worktree(t, "spaceman/tricks", false)
	head := gitRun(t, dir, "rev-parse", "HEAD")
	tip := gitRun(t, e.repo, "rev-parse", "origin/main")
	// The remote's tip pretends to descend from the unpushed HEAD.
	gitRun(t, e.repo, "replace", "--graft", tip, head)
	_, err := e.remove("spaceman-tricks")
	wantRefused(t, err, RefuseUnpushed)
	gitRun(t, e.repo, "replace", "-d", tip)
	gd := gitRun(t, e.repo, "rev-parse", "--git-common-dir")
	if !filepath.IsAbs(gd) {
		gd = filepath.Join(e.repo, gd)
	}
	gitRun(t, dir, "push", "-q", "origin", "spaceman/tricks") // pushed: only the grafts file refuses now
	writeFile(t, filepath.Join(gd, "info", "grafts"), tip+" "+head+"\n")
	_, err = e.remove("spaceman-tricks")
	wantRefused(t, err, RefuseUnpushed)
	if err := os.Remove(filepath.Join(gd, "info", "grafts")); err != nil {
		t.Fatal(err)
	}

	// An untracked .gitignore that ignores itself and its folder.
	writeFile(t, filepath.Join(dir, "notes", ".gitignore"), "*\n")
	writeFile(t, filepath.Join(dir, "notes", "precious.md"), "mine\n")
	_, err = e.remove("spaceman-tricks")
	wantRefused(t, err, RefuseUntracked)
	mustExist(t, filepath.Join(dir, "notes", "precious.md"))
	if err := os.RemoveAll(filepath.Join(dir, "notes")); err != nil {
		t.Fatal(err)
	}

	// Stat checks turned down in the repository's configuration, and an edit of the same size with the old times.
	gitRun(t, e.repo, "config", "core.trustctime", "false")
	gitRun(t, e.repo, "config", "core.checkStat", "minimal")
	p := filepath.Join(dir, "work.txt")
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "update-index", "--refresh")
	writeFile(t, p, "dome\n") // same size as "done\n"
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	if st := gitRun(t, dir, "status", "--porcelain"); st != "" {
		t.Logf("setup: git status already sees the edit (%q); the check still must", st)
	}
	_, err = e.remove("spaceman-tricks")
	wantRefused(t, err, RefuseDirty)
	if b, _ := os.ReadFile(p); string(b) != "dome\n" {
		t.Fatal("the edit was lost")
	}
}
