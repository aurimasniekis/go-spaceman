package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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
	// The checkpoint is refused too, and records why: the space's reason must say so.
	if err := m.checkpointErr(ctx, sp, role, "ins_1", "idle"); err == nil || !strings.Contains(err.Error(), "checkout refused") {
		t.Fatalf("checkpoint: %v", err)
	}
	if ok, why := a.memoryClean(ctx, sp); ok {
		t.Fatal("an instance whose status the guard refuses read as clean")
	} else if !strings.Contains(why, "checkout refused") {
		t.Fatalf("the reason %q does not say why the space is unclean", why)
	}
	head := m.git.RevParse(ctx, m.shared(sp, role), "refs/heads/main")
	if unc, local, ok := m.instanceStatus(ctx, i1, head); !ok || unc < 1 || local < 1 {
		t.Fatalf("unreadable checkout: uncheckpointed=%d localCommits=%d ok=%v, want both >= 1", unc, local, ok)
	}
	m.statusMu.Lock()
	_, cached := m.instStatus[i1]
	m.statusMu.Unlock()
	if cached {
		t.Fatal("an unreadable checkout was cached")
	}
	// Once readable again, the commit is published and the space is clean.
	if err := os.RemoveAll(filepath.Join(i1, "sub")); err != nil {
		t.Fatal(err)
	}
	a.flushSpaceMemory(ctx, sp, "backup")
	stopSyncs(m)
	head = m.git.RevParse(ctx, m.shared(sp, role), "refs/heads/main")
	if unc, local, ok := m.instanceStatus(ctx, i1, head); !ok || unc != 0 || local != 0 {
		t.Fatalf("a readable checkout stayed unclean after the flush: uncheckpointed=%d localCommits=%d ok=%v", unc, local, ok)
	}
	// A stray file in instances/ is not a checkout and is not counted.
	stray := filepath.Join(filepath.Dir(i1), "stray.txt")
	write(t, filepath.Dir(i1), "stray.txt", "x")
	if _, _, ok := m.instanceStatus(ctx, stray, head); ok {
		t.Fatal("a stray file was counted")
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
