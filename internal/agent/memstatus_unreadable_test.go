package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// An instance whose git status the checkout guard refuses (a nested .git in its work tree) and that holds an unpublished
// commit must not read as clean: its status is unknown, so the space stays unclean until the commit is published.
func TestMemoryStatusUnreadableCheckoutIsNotClean(t *testing.T) {
	a := memAgent(t)
	ctx := context.Background()
	m := a.mem
	old := statusRacy
	statusRacy = 0
	t.Cleanup(func() { statusRacy = old })
	col := statusFleet(t, m, 1, 2)[0]
	sp, role := col[0], col[1]
	stopSyncs(m)
	i1 := m.instance(sp, role, "ins_1")
	write(t, i1, "local.md", "- unpublished\n")
	localCommit(t, i1, "local")
	// Central has acknowledged the shared head, so only the instance's own state can make the space unclean.
	js, _ := a.j.memory(ctx, sp, role)
	js.Acked = m.git.RevParse(ctx, m.shared(sp, role), "refs/heads/main")
	if err := a.j.putMemory(ctx, js); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(i1, "sub", ".git"), 0o750); err != nil {
		t.Fatal(err)
	}
	if ok, _ := a.memoryClean(ctx, sp); ok {
		t.Fatal("an instance whose status the guard refuses read as clean")
	}
	head := m.git.RevParse(ctx, m.shared(sp, role), "refs/heads/main")
	if unc, local, ok := m.instanceStatus(ctx, i1, head); !ok || unc < 1 || local < 1 {
		t.Fatalf("unreadable checkout: uncheckpointed=%d localCommits=%d ok=%v, want both >= 1", unc, local, ok)
	}
	// A folder without a .git of its own is still not counted.
	plain := filepath.Join(filepath.Dir(i1), "ins_plain")
	if err := os.MkdirAll(plain, 0o750); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := m.instanceStatus(ctx, plain, head); ok {
		t.Fatal("a folder without .git was counted")
	}
}
