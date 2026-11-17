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
