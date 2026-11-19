package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// TestPrepareDreamReturnsLinkTargets: the memory/<role> links point outside the dream workspace, so prepareDream returns
// the real paths they resolve to, for the harness to allow writes to (and nothing else).
func TestPrepareDreamReturnsLinkTargets(t *testing.T) {
	e := newFFEnv(t)
	inst := e.checkout("ins_dreamer")
	space := t.TempDir()
	spec := proto.DreamSpec{ConfigID: "dcfg-1", MemoryRoles: []string{ffRole}, MaxFiles: 1, MaxBytes: 1024}
	dir, targets, err := e.a.prepareDream(e.ctx, ffSpace, space, "run-dream-1", "ins_dreamer", spec)
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(inst)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || targets[0] != want {
		t.Fatalf("targets %v, want [%s]", targets, want)
	}
	got, err := filepath.EvalSymlinks(filepath.Join(dir, "memory", ffRole))
	if err != nil || got != targets[0] {
		t.Fatalf("link resolves to %q (%v), want %q", got, err, targets[0])
	}
	if _, err := os.Stat(targets[0]); err != nil {
		t.Fatal(err)
	}
}
