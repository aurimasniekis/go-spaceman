package memory

import (
	"context"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// cfg is one entry of `git config --list --show-scope -z`: the scope and a NUL, then the name, a newline and the value,
// and a NUL. A valueless entry has no newline.
func cfg(scope, name string, value ...string) string {
	if len(value) > 0 {
		return scope + "\x00" + name + "\n" + value[0] + "\x00"
	}
	return scope + "\x00" + name + "\x00"
}

// Security review SR-M04: repository configuration that makes git run a program or reach an unchosen transport is
// refused; the node's own (global, system) and SpaceMan's command-line configuration are not.
func TestUnsafeConfig(t *testing.T) {
	for _, bad := range []string{"filter.lfs.clean", "filter.X.Smudge", "filter.x.process", "merge.ours.driver", "diff.pdf.textconv",
		"diff.x.command", "diff.external", "core.sshCommand", "core.gitProxy", "core.pager", "include.path", "includeIf.gitdir:/x.path",
		"credential.helper", "credential.https://example.test.helper", "protocol.ext.allow", "protocol.allow", "url.ext::sh.insteadOf",
		"remote.origin.uploadpack", "uploadpack.packObjectsHook", "gpg.program", "gpg.ssh.program", "gpg.ssh.defaultKeyCommand", "sequence.editor", "core.editor", "core.worktree", "core.WorkTree", "gc.recentObjectsHook", "hook.x.command", "hook.x.event", "Hook.pre.Command", "trailer.x.cmd", "interactive.diffFilter",
		"difftool.x.cmd", "difftool.x.path", "mergetool.x.cmd", "mergetool.x.path", "browser.x.cmd", "browser.x.path", "man.x.cmd", "man.x.path", "web.browser"} {
		if err := UnsafeConfig(cfg("local", bad, "x"), true); err == nil {
			t.Errorf("%s accepted", bad)
		}
		if err := UnsafeConfig(cfg("worktree", bad), true); err == nil {
			t.Errorf("worktree %s accepted", bad)
		}
		if err := UnsafeConfig(cfg("global", bad, "x")+cfg("system", bad, "x")+cfg("command", bad, "x"), true); err != nil {
			t.Errorf("%s outside the repository refused: %v", bad, err)
		}
	}
	for _, ok := range []string{"core.bare", "remote.origin.url", "remote.origin.fetch", "branch.main.remote", "user.name", "core.hookspath",
		"filter.lfs.required", "merge.conflictstyle", "diff.renames"} {
		if err := UnsafeConfig(cfg("local", ok, "x"), true); err != nil {
			t.Errorf("%s refused: %v", ok, err)
		}
	}
	if err := UnsafeConfig("filter.x.clean\x00", false); err == nil {
		t.Error("unscoped list not checked")
	}
	for sub, want := range map[string]bool{"init": false, "clone": false, "config": false, "add": true, "merge-tree": true, "worktree": true} {
		if Guarded(sub) != want {
			t.Errorf("Guarded(%s) = %v", sub, !want)
		}
	}
}

// submodule.<n>.update and pager.<cmd> are judged by value: `git submodule init` writes the first with a plain mode,
// and the second is a boolean unless it names a program.
func TestUnsafeConfigByValue(t *testing.T) {
	for _, tc := range []struct {
		name, value string
		bad         bool
	}{
		{"submodule.x.update", "rebase", false}, {"submodule.x.update", "merge", false}, {"submodule.x.update", "checkout", false},
		{"submodule.x.update", "none", false}, {"submodule.x.update", "", false},
		{"submodule.x.update", "!touch /tmp/x", true}, {"submodule.x.update", "  !touch /tmp/x", true}, {"submodule.a/b.update", "!x", true},
		{"pager.log", "true", false}, {"pager.log", "false", false}, {"pager.log", "yes", false}, {"pager.log", "OFF", false},
		{"pager.log", "0", false}, {"pager.log", "1", false}, {"pager.log", "", false},
		{"pager.log", "less -R", true}, {"pager.log", "touch /tmp/x", true}, {"pager.diff", "!x", true}, {"pager.log", "truee", true},
		// git does not trim a quoted value: "true " names a program. The submodule check trims, which errs safe.
		{"pager.log", "true ", true}, {"pager.log", " true", true}, {"pager.log", "true\t", true},
		{"submodule.x.update", "rebase ", false},
	} {
		for _, scope := range []string{"local", "worktree"} {
			if err := UnsafeConfig(cfg(scope, tc.name, tc.value), true); (err != nil) != tc.bad {
				t.Errorf("%s %s = %q: refused=%v, want %v (%v)", scope, tc.name, tc.value, err != nil, tc.bad, err)
			}
		}
		if err := UnsafeConfig(cfg("global", tc.name, tc.value), true); err != nil {
			t.Errorf("global %s = %q refused: %v", tc.name, tc.value, err)
		}
	}
	// A valueless key is a boolean true.
	for _, name := range []string{"pager.log", "submodule.x.update"} {
		if err := UnsafeConfig(cfg("local", name), true); err != nil {
			t.Errorf("valueless %s refused: %v", name, err)
		}
	}
}

// What `git submodule init` writes works in a checkout, and a command value still does not.
func TestRunAllowsSubmoduleInitConfig(t *testing.T) {
	g, err := NewGit()
	if err != nil {
		t.Skip("git not installed")
	}
	ctx := t.Context()
	dir := t.TempDir()
	if _, err := g.Run(ctx, dir, nil, "init", "--quiet", "."); err != nil {
		t.Fatal(err)
	}
	for _, kv := range [][2]string{{"submodule.x.update", "rebase"}, {"submodule.x.url", "https://example.test/x.git"}, {"submodule.x.active", "true"}, {"pager.log", "false"}} {
		if _, err := g.Run(ctx, dir, nil, "config", kv[0], kv[1]); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := g.Run(ctx, dir, nil, "status", "--porcelain"); err != nil {
		t.Fatalf("legitimate configuration refused: %v", err)
	}
	if _, err := g.Run(ctx, dir, nil, "config", "submodule.x.update", "!touch "+dir+"/ran"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Run(ctx, dir, nil, "status", "--porcelain"); err == nil {
		t.Fatal("a command-valued submodule.x.update was accepted")
	}
}

// A memory checkout whose configuration defines a clean filter is refused before git runs, so the daemon's
// checkpoint never executes it.
func TestRunRefusesUnsafeRepositoryConfig(t *testing.T) {
	g, err := NewGit()
	if err != nil {
		t.Skip("git not installed")
	}
	ctx := t.Context()
	dir := t.TempDir()
	if _, err := g.Run(ctx, dir, nil, "init", "--quiet", "."); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Run(ctx, dir, nil, "status", "--porcelain"); err != nil {
		t.Fatalf("a clean checkout: %v", err)
	}
	if _, err := g.Run(ctx, dir, nil, "config", "filter.x.clean", "touch "+dir+"/ran"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Run(ctx, dir, nil, "add", "-A"); err == nil {
		t.Fatal("git ran in a checkout that defines a filter")
	}
}

// core.worktree and core.bare in a checkout are refused before git runs, and a bare repository (the shared history)
// still works.
func TestRunRefusesRedirectedWorktree(t *testing.T) {
	g, err := NewGit()
	if err != nil {
		t.Skip("git not installed")
	}
	ctx := t.Context()
	bare := t.TempDir()
	if err := g.InitBare(ctx, bare); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Run(ctx, bare, nil, "rev-parse", "--git-dir"); err != nil {
		t.Fatalf("a bare repository refused: %v", err)
	}
	for _, key := range []string{"core.worktree", "core.bare"} {
		dir, outside := t.TempDir(), t.TempDir()
		if _, err := g.Run(ctx, dir, nil, "init", "--quiet", "."); err != nil {
			t.Fatal(err)
		}
		val := outside
		if key == "core.bare" {
			val = "true"
		}
		if _, err := g.Run(ctx, dir, nil, "config", key, val); err != nil {
			t.Fatal(err)
		}
		for _, sub := range [][]string{{"status"}, {"reset", "--hard"}, {"checkout", "-f", "HEAD"}} {
			// The text proves the guard refused, not git itself (a bare repository fails these anyway).
			if _, err := g.Run(ctx, dir, nil, sub...); err == nil || (key == "core.bare" && !strings.Contains(err.Error(), "checkout refused")) {
				t.Errorf("%s: git %s ran or was refused by git: %v", key, sub[0], err)
			}
		}
	}
}

// A checkout's git dir cannot be redirected to another repository, from outside .git (a symlinked .git, a gitdir file,
// a commondir file) or from inside it (a symlinked folder or file, a hard link to another repository's file, borrowed
// objects): the guard refuses, and nothing of the other repository changes.
func TestRunRefusesRedirectedGitDir(t *testing.T) {
	g, err := NewGit()
	if err != nil {
		t.Skip("git not installed")
	}
	ctx := t.Context()
	id := &Identity{Name: "t", Email: "t@example.com"}
	for _, vec := range []string{"symlink", "gitdir file", "commondir", "refs and objects symlinks", "index symlink",
		"packed-refs symlink", "logs symlink", "COMMIT_EDITMSG hard link", "reflog hard link", "alternates", "grafts", "fifo"} {
		t.Run(vec, func(t *testing.T) {
			victim, dir := t.TempDir(), t.TempDir()
			for _, d := range []string{victim, dir} {
				if _, err := g.Run(ctx, d, nil, "init", "--quiet", "."); err != nil {
					t.Fatal(err)
				}
			}
			must(t, os.WriteFile(filepath.Join(victim, "a.md"), []byte("a\n"), 0o600))
			for _, args := range [][]string{{"add", "-A"}, {"commit", "-q", "-m", "v"}, {"branch", "packed"}, {"pack-refs", "--all"}, {"commit", "-q", "--allow-empty", "-m", "v2"}} {
				if _, err := g.Run(ctx, victim, id, args...); err != nil {
					t.Fatal(err)
				}
			}
			before := gitFiles(t, victim)
			vgit, dgit := filepath.Join(victim, ".git"), filepath.Join(dir, ".git")
			relink := func(name string) {
				must(t, os.RemoveAll(filepath.Join(dgit, name)))
				must(t, os.Symlink(filepath.Join(vgit, name), filepath.Join(dgit, name)))
			}
			switch vec {
			case "symlink":
				must(t, os.RemoveAll(dgit))
				must(t, os.Symlink(vgit, dgit))
			case "gitdir file":
				must(t, os.RemoveAll(dgit))
				must(t, os.WriteFile(dgit, []byte("gitdir: "+vgit+"\n"), 0o600))
			case "commondir":
				must(t, os.WriteFile(filepath.Join(dgit, "commondir"), []byte(vgit+"\n"), 0o600))
			case "refs and objects symlinks":
				relink("refs")
				relink("objects")
			case "index symlink":
				relink("index")
			case "packed-refs symlink":
				relink("packed-refs")
			case "logs symlink":
				relink("logs")
			case "COMMIT_EDITMSG hard link":
				must(t, os.Link(filepath.Join(vgit, "refs", "heads", "main"), filepath.Join(dgit, "COMMIT_EDITMSG")))
			case "reflog hard link":
				must(t, os.MkdirAll(filepath.Join(dgit, "logs"), 0o700))
				must(t, os.Link(filepath.Join(vgit, "logs", "HEAD"), filepath.Join(dgit, "logs", "HEAD")))
			case "alternates":
				must(t, os.WriteFile(filepath.Join(dgit, "objects", "info", "alternates"), []byte(filepath.Join(vgit, "objects")+"\n"), 0o600))
			case "grafts":
				must(t, os.MkdirAll(filepath.Join(dgit, "info"), 0o700))
				must(t, os.WriteFile(filepath.Join(dgit, "info", "grafts"), nil, 0o600))
			case "fifo":
				// git reads .git/config for every command: a FIFO there would block it.
				must(t, os.Remove(filepath.Join(dgit, "config")))
				must(t, syscall.Mkfifo(filepath.Join(dgit, "config"), 0o600))
			}
			must(t, os.WriteFile(filepath.Join(dir, "evil.md"), []byte("evil\n"), 0o600))
			for _, args := range [][]string{{"add", "-A"}, {"commit", "-q", "-m", "x", "--allow-empty"}, {"reset", "--hard"},
				{"merge", "--ff-only", "HEAD"}, {"update-ref", "refs/heads/packed", "HEAD"}} {
				rctx, cancel := context.WithTimeout(ctx, 20*time.Second)
				_, err := g.Run(rctx, dir, id, args...)
				cancel()
				if err == nil || !strings.Contains(err.Error(), "checkout refused") {
					t.Errorf("git %s: not refused by the guard: %v", args[0], err)
				}
			}
			if after := gitFiles(t, victim); !maps.Equal(before, after) {
				for k, v := range before {
					if after[k] != v {
						t.Errorf("victim .git/%s changed", k)
					}
				}
				t.Error("the other repository changed")
			}
		})
	}
}

// gitFiles reads every regular file of a repository's .git outside its object store, plus the commit HEAD names.
func gitFiles(t *testing.T, repo string) map[string]string {
	t.Helper()
	files := map[string]string{}
	root := filepath.Join(repo, ".git")
	must(t, filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if d.IsDir() && rel == "objects" {
			return filepath.SkipDir
		}
		if d.Type().IsRegular() {
			b, err := os.ReadFile(path) //nolint:gosec // test fixture
			if err != nil {
				return err
			}
			files[rel] = string(b)
		}
		return nil
	}))
	return files
}

// A submodule or embedded repository in a checkout is refused before any command that reads the work tree: git would
// run a child git there, with that repository's own configuration (its filter drivers would run as the daemon).
func TestRunRefusesGitlinks(t *testing.T) {
	g, err := NewGit()
	if err != nil {
		t.Skip("git not installed")
	}
	ctx := t.Context()
	id := &Identity{Name: "t", Email: "t@example.com"}
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	marker := filepath.Join(t.TempDir(), "ran")
	must(t, os.MkdirAll(sub, 0o700))
	for _, d := range []string{dir, sub} {
		if _, err := g.Run(ctx, d, nil, "init", "--quiet", "."); err != nil {
			t.Fatal(err)
		}
	}
	must(t, os.WriteFile(filepath.Join(sub, ".gitattributes"), []byte("* filter=x\n"), 0o600))
	must(t, os.WriteFile(filepath.Join(sub, "s"), []byte("s\n"), 0o600))
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-q", "-m", "s"}} {
		if _, err := g.Run(ctx, sub, id, args...); err != nil {
			t.Fatal(err)
		}
	}
	// Adding the embedded repository records a gitlink; it runs nothing in it yet.
	// (The walk refuses a nested .git, so the index is written by git itself.)
	hidden := filepath.Join(t.TempDir(), "dotgit")
	if out, err := exec.Command("git", "-C", dir, "add", "-A").CombinedOutput(); err != nil { //nolint:gosec // test fixture
		t.Fatalf("%v: %s", err, out)
	}
	// Set after the sub's own commits: the guard would refuse them.
	if _, err := g.Run(ctx, sub, nil, "config", "filter.x.clean", "sh -c 'touch "+marker+"; cat'"); err != nil {
		t.Fatal(err)
	}
	must(t, os.WriteFile(filepath.Join(sub, "s"), []byte("changed\n"), 0o600))
	cmds := [][]string{{"status", "--porcelain"}, {"add", "-A"}, {"commit", "-q", "-m", "x"}, {"diff", "--name-only"}, {"reset", "--hard"}}
	// Populated: the walk refuses the nested .git before git runs.
	for _, args := range cmds {
		if _, err := g.Run(ctx, dir, id, args...); err == nil || !strings.Contains(err.Error(), "nested .git") {
			t.Errorf("git %s: not refused as a nested .git: %v", args[0], err)
		}
	}
	// Unpopulated (the index still records the gitlink): the gitlink check refuses.
	must(t, os.Rename(filepath.Join(sub, ".git"), hidden))
	for _, args := range cmds {
		if _, err := g.Run(ctx, dir, id, args...); err == nil || !strings.Contains(err.Error(), "submodule or an embedded git repository") {
			t.Errorf("git %s: not refused: %v", args[0], err)
		}
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the embedded repository's filter ran")
	}
	// Commands that never read the work tree still run (the KB acceptance uses them in repositories with submodules).
	if _, err := g.Run(ctx, dir, nil, "rev-parse", "--git-dir"); err != nil {
		t.Fatalf("rev-parse refused: %v", err)
	}
}

// A checkout whose .git is missing is not a repository: git does not look above it for one.
func TestRunStopsAtTheCheckout(t *testing.T) {
	g, err := NewGit()
	if err != nil {
		t.Skip("git not installed")
	}
	ctx := t.Context()
	parent := t.TempDir()
	if _, err := g.Run(ctx, parent, nil, "init", "--quiet", "."); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(parent, "instances", "ins_a")
	must(t, os.MkdirAll(dir, 0o700))
	must(t, os.WriteFile(filepath.Join(dir, "note.md"), []byte("n\n"), 0o600))
	for _, args := range [][]string{{"status", "--porcelain"}, {"add", "-A"}} {
		if _, err := g.Run(ctx, dir, nil, args...); err == nil {
			t.Errorf("git %s found the repository above the checkout", args[0])
		}
	}
	if out, err := g.Run(ctx, parent, nil, "ls-files"); err != nil || out != "" {
		t.Fatalf("the parent repository's index changed: %q %v", out, err)
	}
}

// git 2.54+ runs hook.<name>.command for hook.<name>.event from .git/config even with core.hooksPath=/dev/null
// (reference-transaction fires on every ref update): the guard refuses such a checkout and the program never runs.
func TestRunRefusesConfigHooks(t *testing.T) {
	g, err := NewGit()
	if err != nil {
		t.Skip("git not installed")
	}
	ctx := t.Context()
	id := &Identity{Name: "t", Email: "t@example.com"}
	dir := t.TempDir()
	marker := filepath.Join(t.TempDir(), "ran")
	if _, err := g.Run(ctx, dir, nil, "init", "--quiet", "."); err != nil {
		t.Fatal(err)
	}
	for _, kv := range [][2]string{{"hook.x.command", "touch " + marker}, {"hook.x.event", "reference-transaction"}} {
		if _, err := g.Run(ctx, dir, nil, "config", kv[0], kv[1]); err != nil {
			t.Fatal(err)
		}
	}
	must(t, os.WriteFile(filepath.Join(dir, "a.md"), []byte("a\n"), 0o600))
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-q", "-m", "x"}, {"update-ref", "refs/heads/y", "HEAD"}} {
		if _, err := g.Run(ctx, dir, id, args...); err == nil || !strings.Contains(err.Error(), "hook.x.") {
			t.Errorf("git %s: not refused: %v", args[0], err)
		}
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a configured hook ran")
	}
}

// An include.path naming a FIFO outside .git must not hang the guard: git blocks reading it, and the guard's own
// timeout ends the call.
func TestRunGuardTimesOutOnIncludedFifo(t *testing.T) {
	g, err := NewGit()
	if err != nil {
		t.Skip("git not installed")
	}
	old := GuardTimeout
	GuardTimeout = time.Second
	t.Cleanup(func() { GuardTimeout = old })
	ctx := t.Context()
	dir := t.TempDir()
	if _, err := g.Run(ctx, dir, nil, "init", "--quiet", "."); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(t.TempDir(), "fifo")
	must(t, syscall.Mkfifo(fifo, 0o600))
	cfg := filepath.Join(dir, ".git", "config")
	f, err := os.OpenFile(cfg, os.O_APPEND|os.O_WRONLY, 0)
	must(t, err)
	_, err = f.WriteString("[include]\n\tpath = " + fifo + "\n")
	must(t, err)
	must(t, f.Close())
	start := time.Now()
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err := g.Run(rctx, dir, nil, "status", "--porcelain"); err == nil {
		t.Fatal("git ran with a FIFO include")
	}
	if d := time.Since(start); d > 15*time.Second {
		t.Fatalf("the guard took %v", d)
	}
}

// Hooks are never run (core.hooksPath is forced), so a project repository's symlinked hooks are not a refusal.
func TestRunAllowsSymlinkedHooks(t *testing.T) {
	g, err := NewGit()
	if err != nil {
		t.Skip("git not installed")
	}
	ctx := t.Context()
	dir := t.TempDir()
	if _, err := g.Run(ctx, dir, nil, "init", "--quiet", "."); err != nil {
		t.Fatal(err)
	}
	must(t, os.MkdirAll(filepath.Join(dir, "scripts"), 0o700))
	must(t, os.WriteFile(filepath.Join(dir, "scripts", "hook"), []byte("#!/bin/sh\n"), 0o700))
	must(t, os.MkdirAll(filepath.Join(dir, ".git", "hooks"), 0o700))
	must(t, os.Symlink("../../scripts/hook", filepath.Join(dir, ".git", "hooks", "pre-commit")))
	if _, err := g.Run(ctx, dir, nil, "status", "--porcelain"); err != nil {
		t.Fatalf("symlinked hook refused: %v", err)
	}
}

// `git clone --local` hard-links object files: the hard-link check must allow them.
func TestRunAllowsLocalCloneObjects(t *testing.T) {
	g, err := NewGit()
	if err != nil {
		t.Skip("git not installed")
	}
	ctx := t.Context()
	id := &Identity{Name: "t", Email: "t@example.com"}
	src, dst := t.TempDir(), filepath.Join(t.TempDir(), "clone")
	if _, err := g.Run(ctx, src, nil, "init", "--quiet", "."); err != nil {
		t.Fatal(err)
	}
	must(t, os.WriteFile(filepath.Join(src, "a.md"), []byte("a\n"), 0o600))
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-q", "-m", "a"}} {
		if _, err := g.Run(ctx, src, id, args...); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := g.Run(ctx, src, nil, "clone", "--quiet", "--local", src, dst); err != nil {
		t.Fatal(err)
	}
	linked := false
	must(t, filepath.WalkDir(filepath.Join(dst, ".git", "objects"), func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if fi, e := d.Info(); e == nil && fi.Sys().(*syscall.Stat_t).Nlink > 1 {
				linked = true
			}
		}
		return err
	}))
	if !linked {
		t.Skip("this git did not hard-link objects")
	}
	if _, err := g.Run(ctx, dst, id, "status", "--porcelain"); err != nil {
		t.Fatalf("a local clone refused: %v", err)
	}
}

// Replace refs are off (GIT_NO_REPLACE_OBJECTS): an agent's refs/replace entry does not change what git reads.
func TestRunIgnoresReplaceRefs(t *testing.T) {
	g, err := NewGit()
	if err != nil {
		t.Skip("git not installed")
	}
	ctx := t.Context()
	id := &Identity{Name: "t", Email: "t@example.com"}
	dir := t.TempDir()
	if _, err := g.Run(ctx, dir, nil, "init", "--quiet", "."); err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{"first", "second"} {
		if _, err := g.Run(ctx, dir, id, "commit", "-q", "--allow-empty", "-m", m); err != nil {
			t.Fatal(err)
		}
	}
	first := strings.TrimSpace(mustOut(t, g, dir, "rev-parse", "HEAD~1"))
	second := strings.TrimSpace(mustOut(t, g, dir, "rev-parse", "HEAD"))
	must(t, os.MkdirAll(filepath.Join(dir, ".git", "refs", "replace"), 0o700))
	must(t, os.WriteFile(filepath.Join(dir, ".git", "refs", "replace", first), []byte(second+"\n"), 0o600))
	if out := mustOut(t, g, dir, "cat-file", "-p", first); !strings.Contains(out, "first") {
		t.Fatalf("a replace ref changed what git reads: %q", out)
	}
}

func mustOut(t *testing.T, g *Git, dir string, args ...string) string {
	t.Helper()
	out, err := g.Run(t.Context(), dir, nil, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
