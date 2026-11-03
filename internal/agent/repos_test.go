package agent

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// reposFixture is a local bare repository (the remote) with one commit on main and an empty space folder.
func reposFixture(t *testing.T) (space, bare, seed string) {
	t.Helper()
	root := t.TempDir()
	space, bare, seed = filepath.Join(root, "space"), filepath.Join(root, "remote", "api.git"), filepath.Join(root, "seed")
	gitRun(t, root, "init", "-q", "--bare", "-b", "main", bare)
	gitRun(t, root, "clone", "-q", bare, seed)
	writeFile(t, filepath.Join(seed, "README.md"), "api\n")
	gitRun(t, seed, "checkout", "-q", "-b", "main")
	gitRun(t, seed, "add", ".")
	gitRun(t, seed, "commit", "-q", "-m", "init")
	gitRun(t, seed, "push", "-q", "-u", "origin", "main")
	if err := os.MkdirAll(filepath.Join(space, "work"), 0o700); err != nil {
		t.Fatal(err)
	}
	return space, bare, seed
}

func testAgent() *Agent {
	return &Agent{o: Options{Clock: clock.System{}}, log: slog.New(slog.DiscardHandler)}
}

// SM-REPO-001/003/005/006, ADR-0044: the node clones a configured repository from its URL with its own Git, fetches it on
// request, reports the remote, the default branch locally and remotely, the last fetch, changes, untracked files,
// unpushed commits and worktrees, lists repositories nobody configured, and never overwrites a folder or runs a
// repository's own programs.
func TestSyncRepos(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	space, bare, seed := reposFixture(t)
	a := testAgent()
	ctx := context.Background()
	api := proto.RepoSource{ID: "rep_api", Name: "api", Path: "repos/api", URL: bare, DefaultBranch: "main", ClonePolicy: proto.CloneAuto}
	lazy := proto.RepoSource{ID: "rep_lazy", Name: "lazy", Path: "lazy", URL: bare, DefaultBranch: "main", ClonePolicy: proto.CloneOnDemand}
	manual := proto.RepoSource{ID: "rep_notes", Name: "notes", Path: "notes", DefaultBranch: "main", ClonePolicy: proto.CloneManual}
	all := []proto.RepoSource{api, lazy, manual}

	rep := a.syncRepos(ctx, "spc_1", space, proto.ReposSyncPayload{Repositories: all, Clone: []string{"rep_api"}})
	byID := func(r proto.RepoStatusReport, id string) proto.RepoState {
		for _, s := range r.Repositories {
			if s.ID == id {
				return s
			}
		}
		t.Fatalf("no state for %s in %+v", id, r)
		return proto.RepoState{}
	}
	st := byID(rep, "rep_api")
	head := gitRun(t, seed, "rev-parse", "HEAD")
	if !st.Present || !st.Cloned || st.Error != "" || st.Remote == nil || !st.Remote.Reachable || st.Remote.Head != head || st.DefaultHead != head ||
		st.Tracking != head || st.LastFetchAt == nil || st.Check == nil || st.Check.Branch != "main" || st.Check.Dirty != 0 {
		t.Fatalf("cloned %+v (check %+v)", st, st.Check)
	}
	if s := byID(rep, "rep_lazy"); s.Present || s.Cloned {
		t.Fatalf("on_demand cloned without being asked: %+v", s)
	}
	if s := byID(rep, "rep_notes"); s.Present || s.Remote != nil || s.Error != "" {
		t.Fatalf("manual %+v", s)
	}
	dir := filepath.Join(space, "repos", "api")

	// Work in the clone: a worktree with an unpushed commit and changes, a change in the main folder; upstream moves.
	gitRun(t, dir, "worktree", "add", "-q", "-b", "spaceman/tsk_1", filepath.Join(space, "worktrees", "spaceman-tsk_1"), "main")
	wt := filepath.Join(space, "worktrees", "spaceman-tsk_1")
	writeFile(t, filepath.Join(wt, "feature.go"), "package api\n")
	gitRun(t, wt, "add", ".")
	gitRun(t, wt, "commit", "-q", "-m", "feature")
	writeFile(t, filepath.Join(wt, "draft.txt"), "untracked\n")
	writeFile(t, filepath.Join(wt, "feature.go"), "package api // edited\n")
	writeFile(t, filepath.Join(dir, "README.md"), "api, edited\n")
	writeFile(t, filepath.Join(seed, "CHANGES.md"), "upstream\n")
	gitRun(t, seed, "add", ".")
	gitRun(t, seed, "commit", "-q", "-m", "upstream change")
	gitRun(t, seed, "push", "-q")
	upstream := gitRun(t, seed, "rev-parse", "HEAD")
	// A repository nobody configured.
	gitRun(t, space, "init", "-q", "-b", "main", filepath.Join(space, "scratch"))

	rep = a.syncRepos(ctx, "spc_1", space, proto.ReposSyncPayload{Repositories: all, Clone: []string{"rep_api", "rep_lazy"}, Fetch: []string{"rep_api"}})
	st = byID(rep, "rep_api")
	if !st.Fetched || st.Cloned || st.Tracking != upstream || st.DefaultHead != head || st.Remote.Head != upstream || st.Check.Dirty != 1 {
		t.Fatalf("fetched %+v (check %+v)", st, st.Check)
	}
	if len(st.Check.Worktrees) != 1 || st.Check.Worktrees[0].Dirty != 1 || st.Check.Worktrees[0].Untracked != 1 {
		t.Fatalf("worktrees %+v", st.Check.Worktrees)
	}
	unpushed := 0
	for _, b := range st.Check.Branches {
		if b.Name == "spaceman/tsk_1" {
			unpushed = b.Unpushed
		}
	}
	if unpushed != 1 {
		t.Fatalf("unpushed %+v", st.Check.Branches)
	}
	// Fetching never touched the local branch or the work tree.
	if gitRun(t, dir, "rev-parse", "main") != head || readFile(t, filepath.Join(dir, "README.md")) != "api, edited\n" {
		t.Fatal("the fetch changed local state")
	}
	if s := byID(rep, "rep_lazy"); !s.Cloned || !s.Present {
		t.Fatalf("on_demand clone %+v", s)
	}
	if len(rep.Unlisted) != 1 || rep.Unlisted[0] != "scratch" {
		t.Fatalf("unlisted %v", rep.Unlisted)
	}

	// A folder in the way is never overwritten; a missing remote is reported, not fatal.
	writeFile(t, filepath.Join(space, "taken", "keep.txt"), "mine\n")
	taken := proto.RepoSource{ID: "rep_taken", Name: "taken", Path: "taken", URL: bare, DefaultBranch: "main", ClonePolicy: proto.CloneAuto}
	gone := proto.RepoSource{ID: "rep_gone", Name: "gone", Path: "gone", URL: filepath.Join(filepath.Dir(bare), "missing.git"), DefaultBranch: "main",
		ClonePolicy: proto.CloneAuto}
	helper := proto.RepoSource{ID: "rep_ext", Name: "ext", Path: "ext", URL: "ext::sh%-c%touch%" + filepath.Join(space, "pwned"), DefaultBranch: "main",
		ClonePolicy: proto.CloneAuto}
	escape := proto.RepoSource{ID: "rep_esc", Name: "esc", Path: "../outside", URL: bare, DefaultBranch: "main", ClonePolicy: proto.CloneAuto}
	rep = a.syncRepos(ctx, "spc_1", space, proto.ReposSyncPayload{Repositories: []proto.RepoSource{taken, gone, helper, escape},
		Clone: []string{"rep_taken", "rep_gone", "rep_ext", "rep_esc"}})
	if s := byID(rep, "rep_taken"); s.Present || !strings.Contains(s.Error, "does not overwrite") || readFile(t, filepath.Join(space, "taken", "keep.txt")) != "mine\n" {
		t.Fatalf("taken %+v", s)
	}
	if s := byID(rep, "rep_gone"); s.Present || s.Remote == nil || s.Remote.Reachable || s.Error == "" {
		t.Fatalf("gone %+v", s)
	}
	if s := byID(rep, "rep_ext"); s.Present || !strings.Contains(s.Error, "remote helper") {
		t.Fatalf("ext %+v", s)
	}
	if _, err := os.Stat(filepath.Join(space, "pwned")); err == nil {
		t.Fatal("the ext transport ran a program")
	}
	if s := byID(rep, "rep_esc"); s.Error != "invalid repository folder" {
		t.Fatalf("escape %+v", s)
	}

	// A repository whose own configuration would run a program is not touched (it is reported).
	gitRun(t, dir, "config", "core.sshCommand", "touch "+filepath.Join(space, "pwned"))
	rep = a.syncRepos(ctx, "spc_1", space, proto.ReposSyncPayload{Repositories: []proto.RepoSource{api}, Fetch: []string{"rep_api"}})
	if s := byID(rep, "rep_api"); s.Fetched || !strings.Contains(s.Error, "core.sshcommand") && !strings.Contains(s.Error, "core.sshCommand") {
		t.Fatalf("unsafe config %+v", s)
	}
}

// A launch of a configured repository clones it first when its folder is missing (first use), unless it is manual.
func TestEnsureRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	space, bare, _ := reposFixture(t)
	a := testAgent()
	ctx := context.Background()
	src := proto.RepoSource{ID: "rep_api", Name: "api", Path: "api", URL: bare, DefaultBranch: "main", ClonePolicy: proto.CloneOnDemand}
	if err := a.ensureRepo(ctx, "spc_1", space, src); err != nil {
		t.Fatal(err)
	}
	if !isRepoDir(filepath.Join(space, "api")) {
		t.Fatal("not cloned")
	}
	if err := a.ensureRepo(ctx, "spc_1", space, src); err != nil {
		t.Fatalf("second time: %v", err)
	}
	dir, err := prepareWorktree(ctx, space, proto.Worktree{Repo: "api", Branch: "spaceman/tsk_2"})
	if err != nil || gitRun(t, dir, "branch", "--show-current") != "spaceman/tsk_2" {
		t.Fatalf("worktree %q %v", dir, err)
	}
	manual := proto.RepoSource{ID: "rep_m", Name: "m", Path: "m", URL: bare, DefaultBranch: "main", ClonePolicy: proto.CloneManual}
	if err := a.ensureRepo(ctx, "spc_1", space, manual); err != nil || isRepoDir(filepath.Join(space, "m")) {
		t.Fatalf("manual cloned: %v", err)
	}
}
