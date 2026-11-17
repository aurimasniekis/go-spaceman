package memory

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
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

// A checkout's git dir cannot be redirected to another repository: a symlinked .git, a gitdir file and a commondir
// file are refused by the guard, and the other repository's ref and index stay as they were.
func TestRunRefusesRedirectedGitDir(t *testing.T) {
	g, err := NewGit()
	if err != nil {
		t.Skip("git not installed")
	}
	ctx := t.Context()
	id := &Identity{Name: "t", Email: "t@example.com"}
	for _, vec := range []string{"symlink", "gitdir file", "commondir"} {
		t.Run(vec, func(t *testing.T) {
			victim, dir := t.TempDir(), t.TempDir()
			for _, d := range []string{victim, dir} {
				if _, err := g.Run(ctx, d, nil, "init", "--quiet", "."); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(victim, "a.md"), []byte("a\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			for _, args := range [][]string{{"add", "-A"}, {"commit", "-q", "-m", "v"}} {
				if _, err := g.Run(ctx, victim, id, args...); err != nil {
					t.Fatal(err)
				}
			}
			head := g.RevParse(ctx, victim, "HEAD")
			index, err := os.ReadFile(filepath.Join(victim, ".git", "index"))
			if err != nil {
				t.Fatal(err)
			}
			switch vec {
			case "symlink":
				must(t, os.RemoveAll(filepath.Join(dir, ".git")))
				must(t, os.Symlink(filepath.Join(victim, ".git"), filepath.Join(dir, ".git")))
			case "gitdir file":
				must(t, os.RemoveAll(filepath.Join(dir, ".git")))
				must(t, os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: "+filepath.Join(victim, ".git")+"\n"), 0o600))
			case "commondir":
				must(t, os.WriteFile(filepath.Join(dir, ".git", "commondir"), []byte(filepath.Join(victim, ".git")+"\n"), 0o600))
			}
			must(t, os.WriteFile(filepath.Join(dir, "evil.md"), []byte("evil\n"), 0o600))
			for _, args := range [][]string{{"add", "-A"}, {"commit", "-q", "-m", "x", "--allow-empty"}, {"reset", "--hard"}} {
				_, err := g.Run(ctx, dir, id, args...)
				if err == nil || !strings.Contains(err.Error(), "checkout refused") {
					t.Errorf("git %s: not refused by the guard: %v", args[0], err)
				}
			}
			if got := g.RevParse(ctx, victim, "HEAD"); got != head {
				t.Errorf("victim HEAD moved: %s -> %s", head, got)
			}
			if got, _ := os.ReadFile(filepath.Join(victim, ".git", "index")); !bytes.Equal(got, index) {
				t.Error("victim index changed")
			}
		})
	}
}
