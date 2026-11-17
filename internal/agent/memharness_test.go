package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsHarnessPath(t *testing.T) {
	for p, want := range map[string]bool{
		".claude/skills/x/SKILL.md": true, ".claude": true, "a/b/.claude/commands/y.md": true, ".codex/config.toml": true,
		".agents/skills/z/SKILL.md": true, ".mcp.json": true, "notes/.mcp.json": false, "CLAUDE.md": false, "AGENTS.md": false,
		"notes/claude.md": false, "decisions.md": false, "conflicts/ins-1/.claude/settings.json": true,
	} {
		if got := isHarnessPath(p); got != want {
			t.Errorf("isHarnessPath(%q) = %v, want %v", p, got, want)
		}
	}
}

func shown(t *testing.T, m *memManager, shared, path string) bool {
	t.Helper()
	out, err := m.git.Run(context.Background(), shared, nil, "ls-tree", "-r", "--name-only", "refs/heads/main")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range strings.Fields(out) {
		if f == path {
			return true
		}
	}
	return false
}

// A skill or command one instance writes into memory is not published and not received by another instance;
// CLAUDE.md and AGENTS.md notes still travel.
func TestMemoryHarnessConfigDoesNotTravel(t *testing.T) {
	a := memAgent(t)
	ctx := context.Background()
	m := a.mem
	one, err := m.prepare(ctx, "spc_a", "implementor-backend", "ins_one")
	if err != nil {
		t.Fatal(err)
	}
	write(t, one, ".claude/skills/x/SKILL.md", "evil")
	write(t, one, ".claude/commands/y.md", "evil")
	write(t, one, ".claude/settings.json", "{}")
	write(t, one, "notes/.claude/commands/z.md", "evil")
	write(t, one, ".codex/config.toml", "evil")
	write(t, one, "CLAUDE.md", "# a note\n")
	write(t, one, "AGENTS.md", "# a note\n")
	m.checkpoint(ctx, "spc_a", "implementor-backend", "ins_one", "idle")

	shared := m.shared("spc_a", "implementor-backend")
	for _, p := range []string{".claude/skills/x/SKILL.md", ".claude/commands/y.md", ".claude/settings.json", "notes/.claude/commands/z.md", ".codex/config.toml"} {
		if shown(t, m, shared, p) {
			t.Fatalf("%s reached the shared history", p)
		}
	}
	if !shown(t, m, shared, "CLAUDE.md") || !shown(t, m, shared, "AGENTS.md") {
		t.Fatal("notes did not reach the shared history")
	}
	two, err := m.prepare(ctx, "spc_a", "implementor-backend", "ins_two")
	if err != nil {
		t.Fatal(err)
	}
	if read(t, two, "CLAUDE.md") != "# a note\n" {
		t.Fatal("notes were not received")
	}
	for _, p := range []string{".claude", "notes/.claude", ".codex"} {
		if _, err := os.Stat(filepath.Join(two, p)); err == nil {
			t.Fatalf("%s was materialised in another instance", p)
		}
	}
}

// History an older agent already pushed with harness configuration in it is not materialised: not in a new checkout,
// not by a fast-forward or a merge of an existing one, and not by a conflict preserve.
func TestMemoryHarnessConfigInOldHistoryIsNotMaterialised(t *testing.T) {
	a := memAgent(t)
	ctx := context.Background()
	m := a.mem
	one, err := m.prepare(ctx, "spc_a", "implementor-backend", "ins_one")
	if err != nil {
		t.Fatal(err)
	}
	two, err := m.prepare(ctx, "spc_a", "implementor-backend", "ins_two")
	if err != nil {
		t.Fatal(err)
	}
	// Simulate an older agent: commit and push with no filter, straight from a scratch clone.
	shared := m.shared("spc_a", "implementor-backend")
	old := filepath.Join(t.TempDir(), "old")
	run := func(dir string, args ...string) {
		t.Helper()
		if _, err := m.git.Run(ctx, dir, ident("ins_old"), args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(old, 0o700); err != nil {
		t.Fatal(err)
	}
	run(old, "clone", "--quiet", shared, ".")
	write(t, old, ".claude/skills/x/SKILL.md", "evil")
	write(t, old, ".claude/commands/y.md", "evil")
	write(t, old, "sub/.claude/commands/y.md", "evil")
	write(t, old, "CLAUDE.md", "# old note\n")
	run(old, "add", "-A")
	run(old, "commit", "--quiet", "-m", "old agent")
	run(old, "push", "--quiet", shared, "HEAD:refs/heads/main")
	if !shown(t, m, shared, ".claude/skills/x/SKILL.md") {
		t.Fatal("test setup: old history lacks the harness paths")
	}

	// A new checkout.
	three, err := m.prepare(ctx, "spc_a", "implementor-backend", "ins_three")
	if err != nil {
		t.Fatal(err)
	}
	// An existing clean checkout moved by the checkpoint's fast-forward, and by prepare's refresh.
	m.checkpoint(ctx, "spc_a", "implementor-backend", "ins_one", "idle")
	if _, err := m.prepare(ctx, "spc_a", "implementor-backend", "ins_two"); err != nil {
		t.Fatal(err)
	}
	for name, dir := range map[string]string{"new checkout": three, "fast-forwarded": one, "refreshed": two} {
		if read(t, dir, "CLAUDE.md") != "# old note\n" {
			t.Fatalf("%s: the note was not received", name)
		}
		for _, p := range []string{".claude", "sub/.claude"} {
			if _, err := os.Stat(filepath.Join(dir, p)); err == nil {
				t.Fatalf("%s: %s was materialised", name, p)
			}
		}
	}
	// A checkpoint of such a checkout does not delete the paths from history either way, and stays clean.
	if st := m.statusOf(ctx, "spc_a", "implementor-backend"); st.Uncheckpointed != 0 {
		t.Fatalf("uncheckpointed after receiving old history: %+v", st)
	}
}

// A symlink or a plain file named like a harness directory, in history an older agent pushed, is not materialised
// either: the sparse patterns must match a non-directory of that name.
func TestMemoryHarnessNonDirectoryInOldHistoryIsNotMaterialised(t *testing.T) {
	a := memAgent(t)
	ctx := context.Background()
	m := a.mem
	if _, err := m.prepare(ctx, "spc_a", "implementor-backend", "ins_one"); err != nil {
		t.Fatal(err)
	}
	shared := m.shared("spc_a", "implementor-backend")
	old := filepath.Join(t.TempDir(), "old")
	if err := os.MkdirAll(old, 0o700); err != nil {
		t.Fatal(err)
	}
	run := func(_ string, args ...string) string {
		t.Helper()
		out, err := m.git.Run(ctx, old, ident("ins_old"), args...)
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(out)
	}
	run("", "clone", "--quiet", shared, ".")
	write(t, old, "CLAUDE.md", "# old note\n")
	write(t, old, "blob", "cfg")
	write(t, old, "target", "cfg")
	linkBlob := run("", "hash-object", "-w", "target") // content "cfg": the link target
	fileBlob := run("", "hash-object", "-w", "blob")
	for path, spec := range map[string][2]string{
		".claude":     {"120000", linkBlob}, // symlink .claude -> cfg
		"sub/.claude": {"100644", fileBlob}, // plain file named like the directory
		".codex":      {"120000", linkBlob},
	} {
		run("", "update-index", "--add", "--cacheinfo", spec[0]+","+spec[1]+","+path)
	}
	run("", "add", "CLAUDE.md")
	run("", "commit", "--quiet", "-m", "old agent")
	run("", "push", "--quiet", shared, "HEAD:refs/heads/main")
	if !shown(t, m, shared, ".claude") || !shown(t, m, shared, "sub/.claude") {
		t.Fatal("test setup: old history lacks the non-directory paths")
	}
	two, err := m.prepare(ctx, "spc_a", "implementor-backend", "ins_two")
	if err != nil {
		t.Fatal(err)
	}
	if read(t, two, "CLAUDE.md") != "# old note\n" {
		t.Fatal("the note was not received")
	}
	for _, p := range []string{".claude", "sub/.claude", ".codex"} {
		if _, err := os.Lstat(filepath.Join(two, p)); err == nil {
			t.Fatalf("%s was materialised", p)
		}
	}
}

// A checkout created before this filter existed holds such files; the next prepare removes them from the working tree.
func TestMemoryHarnessConfigRemovedFromExistingCheckout(t *testing.T) {
	a := memAgent(t)
	ctx := context.Background()
	m := a.mem
	one, err := m.prepare(ctx, "spc_a", "implementor-backend", "ins_one")
	if err != nil {
		t.Fatal(err)
	}
	// Drop the guard as an older node would have, then track a skill.
	for _, f := range []string{"info/exclude", "info/sparse-checkout"} {
		_ = os.Remove(filepath.Join(one, ".git", f))
	}
	if _, err := m.git.Run(ctx, one, nil, "config", "--local", "core.sparseCheckout", "false"); err != nil {
		t.Fatal(err)
	}
	write(t, one, ".claude/skills/x/SKILL.md", "evil")
	if _, err := m.git.Run(ctx, one, nil, "add", "-A"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.git.Run(ctx, one, ident("ins_one"), "commit", "--quiet", "-m", "old"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.prepare(ctx, "spc_a", "implementor-backend", "ins_one"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(one, ".claude")); err == nil {
		t.Fatal(".claude stayed in the working tree")
	}
}

// A symlink named like a harness directory is staged by `add -A` and materialised elsewhere unless the patterns also
// match non-directories.
func TestMemoryHarnessSymlinkedDirDoesNotTravel(t *testing.T) {
	a := memAgent(t)
	ctx := context.Background()
	m := a.mem
	one, err := m.prepare(ctx, "spc_a", "implementor-backend", "ins_one")
	if err != nil {
		t.Fatal(err)
	}
	write(t, one, "cfg/skills/x/SKILL.md", "evil")
	for _, n := range []string{".claude", ".codex", ".agents"} {
		if err := os.Symlink("cfg", filepath.Join(one, n)); err != nil {
			t.Fatal(err)
		}
	}
	m.checkpoint(ctx, "spc_a", "implementor-backend", "ins_one", "idle")
	shared := m.shared("spc_a", "implementor-backend")
	for _, p := range []string{".claude", ".codex", ".agents"} {
		if shown(t, m, shared, p) {
			t.Fatalf("symlink %s reached the shared history", p)
		}
	}
	two, err := m.prepare(ctx, "spc_a", "implementor-backend", "ins_two")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{".claude", ".codex", ".agents"} {
		if _, err := os.Lstat(filepath.Join(two, p)); err == nil {
			t.Fatalf("symlink %s was materialised in another instance", p)
		}
	}
}

// A conflict preserve does not copy harness configuration under conflicts/.
func TestMemoryHarnessConfigSkippedByPreserve(t *testing.T) {
	a := memAgent(t)
	ctx := context.Background()
	m := a.mem
	one, err := m.prepare(ctx, "spc_a", "implementor-backend", "ins_one")
	if err != nil {
		t.Fatal(err)
	}
	two, err := m.prepare(ctx, "spc_a", "implementor-backend", "ins_two")
	if err != nil {
		t.Fatal(err)
	}
	write(t, one, "decisions.md", "one\n")
	m.checkpoint(ctx, "spc_a", "implementor-backend", "ins_one", "idle")
	write(t, two, "decisions.md", "two\n")
	write(t, two, ".claude/skills/x/SKILL.md", "evil")
	if _, err := m.git.Run(ctx, two, nil, "add", "-f", "--sparse", "-A"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.git.Run(ctx, two, ident("ins_two"), "commit", "--quiet", "-m", "local"); err != nil {
		t.Fatal(err)
	}
	shared := m.shared("spc_a", "implementor-backend")
	if _, err := m.git.Run(ctx, two, nil, "fetch", "--quiet", "--no-tags", shared, "main"); err != nil {
		t.Fatal(err)
	}
	head, theirs := m.git.RevParse(ctx, two, "HEAD"), m.git.RevParse(ctx, two, "FETCH_HEAD")
	if _, err := m.preserve(ctx, "spc_a", "implementor-backend", "ins_two", head, theirs, []string{"decisions.md", ".claude/skills/x/SKILL.md"}); err != nil {
		t.Fatal(err)
	}
	if m, _ := filepath.Glob(filepath.Join(two, "conflicts", "ins_two-*", "decisions.md")); len(m) != 1 {
		t.Fatal("the ordinary file was not preserved")
	}
	if m, _ := filepath.Glob(filepath.Join(two, "conflicts", "ins_two-*", ".claude")); len(m) != 0 {
		t.Fatalf("harness configuration was preserved: %v", m)
	}
}

// The sparse-checkout file is SpaceMan's: a changed one is rewritten.
func TestMemoryHarnessSparseFileRewritten(t *testing.T) {
	a := memAgent(t)
	ctx := context.Background()
	one, err := a.mem.prepare(ctx, "spc_a", "implementor-backend", "ins_one")
	if err != nil {
		t.Fatal(err)
	}
	sp := filepath.Join(one, ".git", "info", "sparse-checkout")
	if err := os.WriteFile(sp, []byte("/*\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := a.mem.prepare(ctx, "spc_a", "implementor-backend", "ins_one"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(sp); string(b) != harnessSparse() {
		t.Fatalf("sparse-checkout not rewritten: %q", b)
	}
}

// guard writes only real files inside the checkout's .git: a link there must not carry the write to a file outside,
// and a hard link must not be appended to.
func TestMemoryGuardRefusesLinks(t *testing.T) {
	setup := func(t *testing.T) (m *memManager, inst, outside string) {
		t.Helper()
		a := memAgent(t)
		m = a.mem
		inst, err := m.prepare(context.Background(), "spc_a", "implementor-backend", "ins_one")
		if err != nil {
			t.Fatal(err)
		}
		return m, inst, filepath.Join(t.TempDir(), "outside")
	}
	unchanged := func(t *testing.T, p string) {
		t.Helper()
		if b, err := os.ReadFile(p); err != nil || string(b) != "precious\n" {
			t.Fatalf("outside file changed: %q %v", b, err)
		}
	}
	mk := func(t *testing.T, p string) {
		t.Helper()
		if err := os.WriteFile(p, []byte("precious\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("symlinked sparse-checkout", func(t *testing.T) {
		m, inst, outside := setup(t)
		mk(t, outside)
		sp := filepath.Join(inst, ".git", "info", "sparse-checkout")
		_ = os.Remove(sp)
		if err := os.Symlink(outside, sp); err != nil {
			t.Fatal(err)
		}
		if err := m.guard(context.Background(), inst); err == nil {
			t.Fatal("guard accepted a symlinked sparse-checkout")
		}
		unchanged(t, outside)
	})
	t.Run("sparse-checkout symlinked to another file in .git", func(t *testing.T) {
		m, inst, _ := setup(t)
		cfg := filepath.Join(inst, ".git", "config")
		before, _ := os.ReadFile(cfg)
		sp := filepath.Join(inst, ".git", "info", "sparse-checkout")
		_ = os.Remove(sp)
		if err := os.Symlink(filepath.Join("..", "config"), sp); err != nil {
			t.Fatal(err)
		}
		if err := m.guard(context.Background(), inst); err == nil {
			t.Fatal("guard accepted a symlinked sparse-checkout")
		}
		if after, _ := os.ReadFile(cfg); string(after) != string(before) {
			t.Fatal("guard wrote through the link")
		}
	})
	t.Run("symlinked info", func(t *testing.T) {
		m, inst, outside := setup(t)
		if err := os.MkdirAll(outside, 0o700); err != nil {
			t.Fatal(err)
		}
		mk(t, filepath.Join(outside, "sparse-checkout"))
		mk(t, filepath.Join(outside, "exclude"))
		info := filepath.Join(inst, ".git", "info")
		if err := os.RemoveAll(info); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, info); err != nil {
			t.Fatal(err)
		}
		if err := m.guard(context.Background(), inst); err == nil {
			t.Fatal("guard accepted a symlinked info")
		}
		unchanged(t, filepath.Join(outside, "sparse-checkout"))
		unchanged(t, filepath.Join(outside, "exclude"))
	})
	t.Run("symlinked .git", func(t *testing.T) {
		m, inst, outside := setup(t)
		real := outside + "-git"
		if err := os.Rename(filepath.Join(inst, ".git"), real); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(real, filepath.Join(inst, ".git")); err != nil {
			t.Fatal(err)
		}
		_ = os.Remove(filepath.Join(real, "info", "exclude"))
		if err := m.guard(context.Background(), inst); err == nil {
			t.Fatal("guard accepted a symlinked .git")
		}
		if _, err := os.Stat(filepath.Join(real, "info", "exclude")); err == nil {
			t.Fatal("guard wrote through a symlinked .git")
		}
	})
	t.Run("symlinked config", func(t *testing.T) {
		m, inst, outside := setup(t)
		mk(t, outside)
		cfg := filepath.Join(inst, ".git", "config")
		_ = os.Remove(cfg)
		if err := os.Symlink(outside, cfg); err != nil {
			t.Fatal(err)
		}
		if err := m.guard(context.Background(), inst); err == nil {
			t.Fatal("guard accepted a symlinked config")
		}
		unchanged(t, outside)
	})
	t.Run("dangling symlinked config", func(t *testing.T) {
		m, inst, outside := setup(t)
		cfg := filepath.Join(inst, ".git", "config")
		_ = os.Remove(cfg)
		if err := os.Symlink(outside, cfg); err != nil {
			t.Fatal(err)
		}
		if err := m.guard(context.Background(), inst); err == nil {
			t.Fatal("guard accepted a dangling symlinked config")
		}
		if _, err := os.Lstat(outside); err == nil {
			t.Fatal("guard created the link's target")
		}
	})
	t.Run("hard-linked exclude", func(t *testing.T) {
		m, inst, outside := setup(t)
		mk(t, outside)
		ex := filepath.Join(inst, ".git", "info", "exclude")
		_ = os.Remove(ex)
		if err := os.Link(outside, ex); err != nil {
			t.Skipf("no hard links: %v", err)
		}
		if err := m.guard(context.Background(), inst); err == nil {
			t.Fatal("guard accepted a hard-linked exclude")
		}
		unchanged(t, outside)
	})
}

// Running guard again leaves the exclude file as it is: the block is not appended twice, and other lines stay.
func TestMemoryGuardExcludeRewriteIsIdempotent(t *testing.T) {
	a := memAgent(t)
	ctx := context.Background()
	inst, err := a.mem.prepare(ctx, "spc_a", "implementor-backend", "ins_one")
	if err != nil {
		t.Fatal(err)
	}
	ex := filepath.Join(inst, ".git", "info", "exclude")
	if err := os.WriteFile(ex, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := a.mem.guard(ctx, inst); err != nil {
			t.Fatal(err)
		}
	}
	b, _ := os.ReadFile(ex)
	if string(b) != "mine\n"+harnessExclude() {
		t.Fatalf("exclude = %q", b)
	}
}
