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

func dreamSpec(roles ...string) proto.DreamSpec {
	return proto.DreamSpec{ConfigID: "dcfg-1", MemoryRoles: roles, MaxFiles: 1, MaxBytes: 1024}
}

func resolved(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// TestPrepareDreamWildcardTargetsOnlyThisSpace: "*" links the roles of the run's own space, never another space's.
func TestPrepareDreamWildcardTargetsOnlyThisSpace(t *testing.T) {
	e := newFFEnv(t)
	mine := e.checkout("ins_dreamer")
	const otherSpace = "spc_01m7md" + "eeeeeeeeeeeeeeeeeeee"
	other, err := e.m.prepare(e.ctx, otherSpace, "stranger", "ins_x")
	if err != nil {
		t.Fatal(err)
	}
	_, targets, err := e.a.prepareDream(e.ctx, ffSpace, t.TempDir(), "run-dream-1", "ins_dreamer", dreamSpec("*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || targets[0] != resolved(t, mine) {
		t.Fatalf("targets %v, want only [%s]", targets, resolved(t, mine))
	}
	for _, tg := range targets {
		if tg == resolved(t, other) {
			t.Fatalf("another space's memory %s is a target", tg)
		}
	}
}

// TestPrepareDreamSeveralRolesTargetsAll: every explicitly listed role contributes its target.
func TestPrepareDreamSeveralRolesTargetsAll(t *testing.T) {
	e := newFFEnv(t)
	space := t.TempDir()
	dir, targets, err := e.a.prepareDream(e.ctx, ffSpace, space, "run-dream-1", "ins_dreamer", dreamSpec(ffRole, "planner", "tester"))
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 3 {
		t.Fatalf("targets %v, want 3", targets)
	}
	for i, role := range []string{ffRole, "planner", "tester"} {
		if want := resolved(t, filepath.Join(dir, "memory", role)); targets[i] != want {
			t.Fatalf("target %d = %s, want %s", i, targets[i], want)
		}
	}
}

// TestPrepareDreamInvalidRoleHasNoTarget: a role name that is not a valid memory key is skipped, not linked.
func TestPrepareDreamInvalidRoleHasNoTarget(t *testing.T) {
	e := newFFEnv(t)
	dir, targets, err := e.a.prepareDream(e.ctx, ffSpace, t.TempDir(), "run-dream-1", "ins_dreamer", dreamSpec("../x"))
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 0 {
		t.Fatalf("targets %v, want none", targets)
	}
	if _, err := os.Lstat(filepath.Join(dir, "memory", "..", "x")); err == nil {
		t.Fatal("an invalid role was linked")
	}
}

// TestPrepareDreamFailureReturnsNoTargets: a failed prepare grants nothing.
func TestPrepareDreamFailureReturnsNoTargets(t *testing.T) {
	e := newFFEnv(t)
	e.checkout("ins_dreamer")
	dir, targets, err := e.a.prepareDream(e.ctx, ffSpace, t.TempDir(), "bad run!", "ins_dreamer", dreamSpec(ffRole))
	if err == nil {
		t.Fatal("expected an error")
	}
	if dir != "" || targets != nil {
		t.Fatalf("got dir %q targets %v, want none", dir, targets)
	}
}
