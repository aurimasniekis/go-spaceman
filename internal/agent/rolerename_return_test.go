package agent

import (
	"context"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// A node that lost its source-unavailable space never got the release, so it keeps the journaled rename whose closed
// notice central cancelled on relocation. When the space comes back, the relocation's prepare must drop that rename:
// left behind it would hold the names again with nothing left to close it.
func TestRelocPrepareDropsStaleRenamesOfReturningSpace(t *testing.T) {
	a := rnAgent(t)
	ctx := context.Background()
	a.mu.Lock()
	delete(a.leases, rnSpace)
	a.mu.Unlock()
	other := "spc_other0000000000000000000"
	for _, sp := range []string{rnSpace, other} {
		if err := a.j.insertRoleRename(ctx, renameRow{ID: "rrn_" + sp, SpaceID: sp, From: "a", To: "b", State: renamePending}); err != nil {
			t.Fatal(err)
		}
	}
	// A closed rename (the closed notice) must go too, or the old name stays an orphan and a later launch sets central's
	// restored folder aside.
	if err := a.j.insertClosedRoleRename(ctx, renameRow{ID: "rrn_closed", SpaceID: rnSpace, From: "c", To: "d"}); err != nil {
		t.Fatal(err)
	}
	if !a.j.orphanedRole(ctx, rnSpace, "c") {
		t.Fatal("setup: closed rename should orphan its old name")
	}
	if _, err := a.relocPrepare(ctx, nil, rnSpace, proto.RelocatePreparePayload{RelocationID: "rel_00000000000000000000000001", Workdir: "rn"}); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := a.j.roleRename(ctx, "rrn_"+rnSpace); found {
		t.Fatal("the returning space's stale rename stayed journaled")
	}
	if a.j.orphanedRole(ctx, rnSpace, "c") {
		t.Fatal("the closed rename left its old name orphaned")
	}
	if a.j.renameHeld(ctx, rnSpace, "a") {
		t.Fatal("names still held after the space returned")
	}
	if _, found, _ := a.j.roleRename(ctx, "rrn_"+other); !found {
		t.Fatal("another space's rename was dropped")
	}
}

// relocPrepare takes renameMu around the delete: while a rename move holds it, prepare waits.
func TestRelocPrepareWaitsForRenameMu(t *testing.T) {
	a := rnAgent(t)
	ctx := context.Background()
	a.mu.Lock()
	delete(a.leases, rnSpace)
	a.mu.Unlock()
	a.renameMu.Lock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = a.relocPrepare(ctx, nil, rnSpace, proto.RelocatePreparePayload{RelocationID: "rel_00000000000000000000000001", Workdir: "rn"})
	}()
	select {
	case <-done:
		a.renameMu.Unlock()
		t.Fatal("relocPrepare ran while a rename move held renameMu")
	case <-time.After(150 * time.Millisecond):
	}
	a.renameMu.Unlock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("relocPrepare did not finish after renameMu was released")
	}
}
