package memory

import (
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// Security review SR-M04: repository configuration that makes git run a program or reach an unchosen transport is
// refused; the node's own (global, system) and SpaceMan's command-line configuration are not.
func TestUnsafeConfig(t *testing.T) {
	for _, bad := range []string{"filter.lfs.clean", "filter.X.Smudge", "filter.x.process", "merge.ours.driver", "diff.pdf.textconv",
		"diff.x.command", "diff.external", "core.sshCommand", "core.gitProxy", "core.pager", "include.path", "includeIf.gitdir:/x.path",
		"credential.helper", "credential.https://example.test.helper", "protocol.ext.allow", "protocol.allow", "url.ext::sh.insteadOf",
		"remote.origin.uploadpack", "uploadpack.packObjectsHook", "gpg.program", "gpg.ssh.program", "sequence.editor", "core.editor", "core.worktree", "core.WorkTree", "gc.recentObjectsHook"} {
		if err := UnsafeConfig("local\t"+bad+"\n", true); err == nil {
			t.Errorf("%s accepted", bad)
		}
		if err := UnsafeConfig("worktree\t"+bad, true); err == nil {
			t.Errorf("worktree %s accepted", bad)
		}
		if err := UnsafeConfig("global\t"+bad+"\nsystem\t"+bad+"\ncommand\t"+bad, true); err != nil {
			t.Errorf("%s outside the repository refused: %v", bad, err)
		}
	}
	for _, ok := range []string{"core.bare", "remote.origin.url", "remote.origin.fetch", "branch.main.remote", "user.name", "core.hookspath",
		"filter.lfs.required", "merge.conflictstyle", "diff.renames"} {
		if err := UnsafeConfig("local\t"+ok, true); err != nil {
			t.Errorf("%s refused: %v", ok, err)
		}
	}
	if err := UnsafeConfig("filter.x.clean\n", false); err == nil {
		t.Error("unscoped list not checked")
	}
	for sub, want := range map[string]bool{"init": false, "clone": false, "config": false, "add": true, "merge-tree": true, "worktree": true} {
		if Guarded(sub) != want {
			t.Errorf("Guarded(%s) = %v", sub, !want)
		}
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
				must(t, syscall.Mkfifo(filepath.Join(dgit, "MERGE_MSG"), 0o600))
			}
			must(t, os.WriteFile(filepath.Join(dir, "evil.md"), []byte("evil\n"), 0o600))
			for _, args := range [][]string{{"add", "-A"}, {"commit", "-q", "-m", "x", "--allow-empty"}, {"reset", "--hard"},
				{"merge", "--ff-only", "HEAD"}, {"update-ref", "refs/heads/packed", "HEAD"}} {
				_, err := g.Run(ctx, dir, id, args...)
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
	if _, err := g.Run(ctx, dir, nil, "add", "-A"); err != nil {
		t.Fatal(err)
	}
	// Set after the sub's own commits: the guard would refuse them.
	if _, err := g.Run(ctx, sub, nil, "config", "filter.x.clean", "sh -c 'touch "+marker+"; cat'"); err != nil {
		t.Fatal(err)
	}
	must(t, os.WriteFile(filepath.Join(sub, "s"), []byte("changed\n"), 0o600))
	for _, args := range [][]string{{"status", "--porcelain"}, {"add", "-A"}, {"commit", "-q", "-m", "x"}, {"diff", "--name-only"}, {"reset", "--hard"}} {
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
