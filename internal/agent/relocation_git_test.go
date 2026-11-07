package agent

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

func writeFile(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(b)
}

// relocationFixture is a space folder with one pushed repository (lib) and one with every kind of local-only state
// (app): an unpushed branch, a changed tracked file, an untracked file, a deleted file, a stash, a worktree with its own
// change, and loose files outside any repository.
func relocationFixture(t *testing.T) (space, origin string) {
	t.Helper()
	root := t.TempDir()
	space = filepath.Join(root, "space")
	origin = filepath.Join(root, "origin")
	for _, name := range []string{"app", "lib"} {
		bare := filepath.Join(origin, name+".git")
		gitRun(t, root, "init", "-q", "--bare", "-b", "main", bare)
		dir := filepath.Join(space, name)
		gitRun(t, root, "clone", "-q", bare, dir)
		writeFile(t, filepath.Join(dir, "README.md"), name+"\n")
		writeFile(t, filepath.Join(dir, "gone.txt"), "to delete\n")
		gitRun(t, dir, "checkout", "-q", "-b", "main")
		gitRun(t, dir, "add", ".")
		gitRun(t, dir, "commit", "-q", "-m", "init "+name)
		gitRun(t, dir, "push", "-q", "-u", "origin", "main")
	}
	app := filepath.Join(space, "app")
	gitRun(t, app, "checkout", "-q", "-b", "feature")
	writeFile(t, filepath.Join(app, "feature.txt"), "local only\n")
	gitRun(t, app, "add", ".")
	gitRun(t, app, "commit", "-q", "-m", "unpushed work")
	writeFile(t, filepath.Join(app, "stashed.txt"), "stash me\n")
	gitRun(t, app, "add", "stashed.txt")
	gitRun(t, app, "stash", "push", "-q", "-m", "half done")
	writeFile(t, filepath.Join(app, "README.md"), "app, edited\n")
	writeFile(t, filepath.Join(app, "new.txt"), "untracked\n")
	if err := os.Remove(filepath.Join(app, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	gitRun(t, app, "worktree", "add", "-q", "-b", "wt-branch", filepath.Join(space, "worktrees", "wt-branch"), "main")
	writeFile(t, filepath.Join(space, "worktrees", "wt-branch", "wt.txt"), "worktree change\n")
	writeFile(t, filepath.Join(space, "work", "notes", "plan.md"), "loose notes\n")
	if err := os.Symlink("plan.md", filepath.Join(space, "work", "notes", "latest.md")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(space, "instances", "ins_1", "claude", "projects", "x", "s.jsonl"), "{}\n")
	return space, origin
}

// SM-SPACE-005, SM-REPO-003/006: the inspection finds every kind of local-only state; an export carries all of it to a
// new folder (branches, uncommitted changes as uncommitted changes, stashes, worktrees, loose files) without changing
// the source; a pushed repository is cloned at the source's commit.
func TestRelocationInspectExportRestore(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	ctx := context.Background()
	g, err := newProjectGit()
	if err != nil {
		t.Fatal(err)
	}
	space, _ := relocationFixture(t)
	repos, loose, insts, errs := g.inspectSpace(ctx, space)
	if len(errs) > 0 {
		t.Fatalf("inspect errors: %v", errs)
	}
	if len(repos) != 2 || repos[0].Path != "app" || repos[1].Path != "lib" {
		t.Fatalf("repositories %+v", repos)
	}
	app, lib := repos[0], repos[1]
	if app.Branch != "feature" || app.Dirty != 2 || app.Untracked != 1 || app.Stashes != 1 || len(app.Remotes) != 1 || len(app.Worktrees) != 1 {
		t.Fatalf("app check %+v", app)
	}
	var feature proto.BranchCheck
	for _, b := range app.Branches {
		if b.Name == "feature" {
			feature = b
		}
	}
	if feature.Unpushed != 1 || !feature.NoUpstream {
		t.Fatalf("feature branch %+v", feature)
	}
	if w := app.Worktrees[0]; w.Path != "worktrees/wt-branch" || w.Branch != "wt-branch" || w.Untracked != 1 {
		t.Fatalf("worktree %+v", w)
	}
	if lib.Dirty+lib.Untracked+lib.Stashes != 0 || lib.Branches[0].Unpushed != 0 || lib.Branches[0].NoUpstream {
		t.Fatalf("lib should be clean: %+v", lib)
	}
	if loose.Files != 2 || !slices.Contains(loose.Paths, "work/notes/plan.md") {
		t.Fatalf("loose %+v", loose)
	}
	if len(insts) != 1 || insts[0].InstanceID != "ins_1" || insts[0].Files != 1 {
		t.Fatalf("instances %+v", insts)
	}
	issues := checkIssuesForTest(repos, loose)
	if issues == 0 {
		t.Fatal("no issues found")
	}

	// Export: the source is unchanged afterwards.
	before := gitRun(t, filepath.Join(space, "app"), "status", "--porcelain")
	work := t.TempDir()
	ex, err := g.exportRepo(ctx, space, "app", filepath.Join(work, "repo-0.bundle"))
	if err != nil {
		t.Fatal(err)
	}
	if after := gitRun(t, filepath.Join(space, "app"), "status", "--porcelain"); after != before {
		t.Fatalf("export changed the source: %q → %q", before, after)
	}
	if refs := gitRun(t, filepath.Join(space, "app"), "for-each-ref", exportRefs); refs != "" {
		t.Fatalf("temporary refs left behind: %s", refs)
	}
	if ex.Snapshot == "" || len(ex.Stashes) != 1 || len(ex.Worktrees) != 1 || ex.Worktrees[0].Snapshot == "" {
		t.Fatalf("export %+v", ex)
	}
	l, err := walkSpace(space)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeLoose(space, l.loose, filepath.Join(work, "loose.tar.gz"), 1<<20); err != nil {
		t.Fatal(err)
	}

	// Restore into a new space folder.
	dest := filepath.Join(t.TempDir(), "space")
	if err := os.MkdirAll(dest, 0o700); err != nil {
		t.Fatal(err)
	}
	pr, err := g.restoreRepo(ctx, dest, ex, filepath.Join(work, "repo-0.bundle"))
	if err != nil {
		t.Fatal(err)
	}
	if pr.Head != app.Head || pr.Branch != "feature" || pr.Dirty != 2 || pr.Untracked != 1 || pr.Stashes != 1 || pr.Worktrees != 1 {
		t.Fatalf("restored %+v (source head %s)", pr, app.Head)
	}
	dapp := filepath.Join(dest, "app")
	if readFile(t, filepath.Join(dapp, "README.md")) != "app, edited\n" || readFile(t, filepath.Join(dapp, "new.txt")) != "untracked\n" {
		t.Fatal("uncommitted changes not restored")
	}
	if _, err := os.Stat(filepath.Join(dapp, "gone.txt")); !os.IsNotExist(err) {
		t.Fatal("a deleted file came back")
	}
	if readFile(t, filepath.Join(dest, "worktrees", "wt-branch", "wt.txt")) != "worktree change\n" {
		t.Fatal("worktree change not restored")
	}
	if st := gitRun(t, dapp, "status", "--porcelain"); st != before {
		t.Fatalf("restored status %q, source %q", st, before)
	}
	if msg := gitRun(t, dapp, "stash", "list", "--format=%gs"); !strings.Contains(msg, "half done") {
		t.Fatalf("stash list %q", msg)
	}
	if gitRun(t, dapp, "config", "branch.main.remote") != "origin" || gitRun(t, dapp, "rev-parse", "origin/main") == "" {
		t.Fatal("upstream configuration or remote-tracking refs lost")
	}
	if refs := gitRun(t, dapp, "for-each-ref", exportRefs); refs != "" {
		t.Fatalf("temporary refs on the destination: %s", refs)
	}
	n, err := readLoose(dest, filepath.Join(work, "loose.tar.gz"), 1<<20)
	if err != nil || n != 2 {
		t.Fatalf("loose files %d %v", n, err)
	}
	if readFile(t, filepath.Join(dest, "work", "notes", "latest.md")) != "loose notes\n" {
		t.Fatal("symlink not restored")
	}
	// The same archive again: nothing existing is overwritten.
	if _, err := readLoose(dest, filepath.Join(work, "loose.tar.gz"), 1<<20); err == nil {
		t.Fatal("loose files overwrote existing ones")
	}

	// A pushed repository is cloned at the source's commit.
	rr := proto.RelocateRepo{Path: lib.Path, Remotes: lib.Remotes, Branch: lib.Branch, Head: lib.Head, Branches: lib.Branches}
	pl, err := g.cloneRepo(ctx, dest, rr)
	if err != nil {
		t.Fatal(err)
	}
	if pl.Head != lib.Head || pl.Branch != "main" || pl.Dirty+pl.Untracked != 0 {
		t.Fatalf("cloned %+v", pl)
	}
}

func checkIssuesForTest(repos []proto.RepoCheck, loose proto.LooseCheck) int {
	n := loose.Files
	for _, r := range repos {
		n += r.Dirty + r.Untracked + r.Stashes
		for _, b := range r.Branches {
			n += b.Unpushed
		}
	}
	return n
}

// Remote URLs never carry credentials to central.
func TestRedactURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://user:tok3n@example.test/a.git": "https://example.test/a.git",
		"https://example.test/a.git":            "https://example.test/a.git",
		"git@example.test:a/b.git":              "git@example.test:a/b.git",
		"ssh://git@example.test/a/b.git":        "ssh://git@example.test/a/b.git",
		"ssh://git:pw@example.test/a/b.git":     "ssh://example.test/a/b.git",
		"https://user@example.test/a.git":       "https://example.test/a.git",
		"/srv/git/a.git":                        "/srv/git/a.git",
	} {
		got, _ := redactURL(in)
		if got != want {
			t.Errorf("redact %q = %q, want %q", in, got, want)
		}
	}
}

// Archives from another node cannot write outside the space folder or through a symlink.
func TestReadLooseRefusesUnsafePaths(t *testing.T) {
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "a", "x.txt"), "x")
	if err := os.Symlink(t.TempDir(), filepath.Join(src, "a", "link")); err != nil {
		t.Fatal(err)
	}
	arch := filepath.Join(t.TempDir(), "l.tar.gz")
	if err := writeLoose(src, []string{"a/link", "a/x.txt"}, arch, 1<<20); err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	if _, err := readLoose(dest, arch, 1<<20); err != nil {
		t.Fatal(err)
	}
	// A crafted archive with a traversal path is refused.
	bad := filepath.Join(t.TempDir(), "bad.tar.gz")
	writeTar(t, bad, "../escape.txt")
	if _, err := readLoose(t.TempDir(), bad, 1<<20); err == nil {
		t.Fatal("traversal accepted")
	}
	// A file under a symlinked folder is refused (the symlink would be followed).
	d2 := t.TempDir()
	if err := os.Symlink(t.TempDir(), filepath.Join(d2, "a")); err != nil {
		t.Fatal(err)
	}
	through := filepath.Join(t.TempDir(), "through.tar.gz")
	writeTar(t, through, "a/inside.txt")
	if _, err := readLoose(d2, through, 1<<20); err == nil {
		t.Fatal("wrote through a symlink")
	}
}

func writeTar(t *testing.T, path, name string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zw := gzip.NewWriter(f)
	tw := tar.NewWriter(zw)
	if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Size: 1, Mode: 0o600}); err != nil {
		t.Fatal(err)
	}
	_, _ = tw.Write([]byte("x"))
	_ = tw.Close()
	_ = zw.Close()
}

func writeLinks(t *testing.T, path string, links [][2]string) {
	t.Helper()
	f, err := os.Create(path) //nolint:gosec // test path
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zw := gzip.NewWriter(f)
	tw := tar.NewWriter(zw)
	for _, l := range links {
		if err := tw.WriteHeader(&tar.Header{Name: l[0], Typeflag: tar.TypeSymlink, Linkname: l[1], Mode: 0o777}); err != nil {
			t.Fatal(err)
		}
	}
	_ = tw.Close()
	_ = zw.Close()
}

// Security review SR-L04: symlinks of the same archive cannot be chained to write outside the space folder, and a
// symlink cannot take the place of the harnesses' instances folder.
func TestReadLooseRefusesChainedAndReservedLinks(t *testing.T) {
	outside := t.TempDir()
	chained := filepath.Join(t.TempDir(), "chained.tar.gz")
	writeLinks(t, chained, [][2]string{{"a", outside}, {"a/b", "/etc/passwd"}})
	if _, err := readLoose(t.TempDir(), chained, 1<<20); err == nil {
		t.Fatal("a symlink inside another symlink of the archive was created")
	}
	if _, err := os.Lstat(filepath.Join(outside, "b")); err == nil {
		t.Fatal("the archive wrote outside the space folder")
	}
	reserved := filepath.Join(t.TempDir(), "reserved.tar.gz")
	writeLinks(t, reserved, [][2]string{{"instances", outside}})
	if _, err := readLoose(t.TempDir(), reserved, 1<<20); err == nil {
		t.Fatal("a symlink named instances was accepted")
	}
}

// Security review SR-M02: worktree paths from the source's report must stay inside the destination's space folder and
// never pass through a symlink (for example one the repository just checked out committed).
func TestAddWorktreesRefusesEscapes(t *testing.T) {
	g, err := newProjectGit()
	if err != nil {
		t.Skip("git not installed")
	}
	ctx := context.Background()
	space := t.TempDir()
	repo := filepath.Join(space, "proj")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "init", "-q", "-b", "main")
	writeFile(t, filepath.Join(repo, "README.md"), "x")
	gitRun(t, repo, "add", ".")
	gitRun(t, repo, "commit", "-q", "-m", "init")
	gitRun(t, repo, "branch", "side")
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(repo, "sub")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"../escape", "/abs/path", "instances/ins_1/x", "dreams/run_x", "proj/sub/git"} {
		if err := g.addWorktrees(ctx, space, repo, []proto.ExportedWorktree{{Path: p, Branch: "side"}}); err == nil {
			t.Errorf("worktree path %q accepted", p)
		}
	}
	if ents, _ := os.ReadDir(outside); len(ents) != 0 {
		t.Fatal("a worktree was created outside the space folder")
	}
	if err := g.addWorktrees(ctx, space, repo, []proto.ExportedWorktree{{Path: "worktrees/side", Branch: "side"}}); err != nil {
		t.Fatalf("a normal worktree path: %v", err)
	}
}
