package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// SM-MCP-004: SpaceMan manages only the skill folders it wrote; the user's own skills are never touched.
func TestSyncSkills(t *testing.T) {
	root := filepath.Join(t.TempDir(), "claude", "skills")
	a := proto.Skill{Name: "hallpass", Version: 1, SHA256: "aa", Files: map[string]string{"SKILL.md": "---\nname: hallpass\n---\n", "references/commands.md": "cmds"}}
	b := proto.Skill{Name: "review", Version: 2, Files: map[string]string{"SKILL.md": "review"}}
	if err := syncSkills(root, []proto.Skill{a, b}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(root, "hallpass", "references", "commands.md")); string(got) != "cmds" {
		t.Fatalf("reference file %q", got)
	}
	fi, _ := os.Stat(filepath.Join(root, "hallpass", "SKILL.md"))
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	// The user's own skill, and a skill of the same name as a SpaceMan one, are left alone.
	user := filepath.Join(root, "mine")
	if err := os.MkdirAll(user, 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(user, "SKILL.md"), []byte("mine"), 0o600)
	if err := os.RemoveAll(filepath.Join(root, "review")); err != nil {
		t.Fatal(err)
	}
	_ = os.MkdirAll(filepath.Join(root, "review"), 0o700)
	_ = os.WriteFile(filepath.Join(root, "review", "SKILL.md"), []byte("user review"), 0o600)
	// Next launch: only hallpass; review (now the user's) and mine stay.
	a.Files["SKILL.md"] = "---\nname: hallpass\n---\nv2\n"
	if err := syncSkills(root, []proto.Skill{a, b}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(root, "review", "SKILL.md")); string(got) != "user review" {
		t.Fatalf("user skill overwritten: %q", got)
	}
	if err := syncSkills(root, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "hallpass")); !os.IsNotExist(err) {
		t.Fatal("stale managed skill kept")
	}
	if _, err := os.Stat(filepath.Join(user, "SKILL.md")); err != nil {
		t.Fatal("user skill removed")
	}
	for _, bad := range []proto.Skill{
		{Name: "../escape", Files: map[string]string{"SKILL.md": "x"}},
		{Name: "ok", Files: map[string]string{"../../etc/x": "x"}},
		{Name: "ok2", Files: map[string]string{"/abs": "x"}},
	} {
		if err := syncSkills(root, []proto.Skill{bad}); err == nil {
			t.Fatalf("unsafe skill accepted: %+v", bad)
		}
	}
}
