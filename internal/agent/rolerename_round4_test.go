package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// A rename the node journaled that fails before moving, with the same error each time, never reports again, so the
// node learns of an admin's close only from central's closed notice (queued durably on every close). The later rename
// builder->maker waited (deferred) behind the first; the notice drops the first and wakes the second, which moves.
func TestClosedNoticeReleasesLaterRenameBehindFailedJournaledRename(t *testing.T) {
	a := rnAgent(t)
	ctx := context.Background()
	rnSeed(t, a, "implementor")
	dir := filepath.Join(a.mem.root, rnSpace)
	if err := os.MkdirAll(filepath.Join(dir, "builder"), 0o700); err != nil { // leftover under the new name
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil { // .retained cannot be created: an error before moving
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	a.command(ctx, rnCommand(t, "implementor", "builder"))
	rnWait(t, a, "failed", func(r renameRow) bool { return r.Result == proto.MemoryRenameError })
	a.roleRenamedAck(ctx, proto.MemoryRenamedAck{RenameID: rnID, Result: proto.MemoryRenameError}) // central acked while open
	a.handleMemoryRename(ctx, rnCmd(t, rnID2, "builder", "maker", false), true)
	rnWaitID(t, a, rnID2, "deferred", func(r renameRow) bool { return r.Result == proto.MemoryRenameDeferred })
	for i := 0; i < 3; i++ { // restarts alone never settle it: nothing tells the node of the close
		a.retryRoleRenames(ctx, "")
	}
	if r, _, _ := a.j.roleRename(ctx, rnID2); r.State != renamePending {
		t.Fatalf("the later rename moved before the earlier one ended: %+v", r)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	// The closed notice (the durable one, delivered at connect) drops the first and wakes the second (M16).
	a.handleMemoryRename(ctx, rnCmd(t, rnID, "implementor", "builder", true), true)
	if r, _, _ := a.j.roleRename(ctx, rnID); r.State != renameDropped || r.Result != proto.MemoryRenameClosed {
		t.Fatalf("the failed rename after the notice: %+v", r)
	}
	r2 := rnWaitID(t, a, rnID2, "the later rename moves after the notice", func(r renameRow) bool { return r.State == renameDone })
	if r2.Result != proto.MemoryRenameOK {
		t.Fatalf("later rename %+v", r2)
	}
	stopTimers(a.mem)
}

// An error ack to a rename that has not moved drops it, and the renames that waited for it are tried again at once
// (renameSettled, for a dropped rename), not at the next start.
func TestRenameSettledAfterErrorAckWakesDeferredRename(t *testing.T) {
	a := rnAgent(t)
	ctx := context.Background()
	if err := a.j.insertRoleRename(ctx, renameRow{ID: rnID, SpaceID: rnSpace, From: "implementor", To: "builder", State: renamePending}); err != nil {
		t.Fatal(err)
	}
	if err := a.j.setRoleRenameResult(ctx, rnID, proto.MemoryRenameError, "boom"); err != nil {
		t.Fatal(err)
	}
	a.handleMemoryRename(ctx, rnCmd(t, rnID2, "builder", "maker", false), true)
	rnWaitID(t, a, rnID2, "deferred", func(r renameRow) bool { return r.Result == proto.MemoryRenameDeferred })

	a.roleRenamedAck(ctx, proto.MemoryRenamedAck{RenameID: rnID, Result: proto.MemoryRenameError, Error: "unknown rename"})
	if r, _, _ := a.j.roleRename(ctx, rnID); r.State != renameDropped {
		t.Fatalf("the refused rename: %+v", r)
	}
	rnWaitID(t, a, rnID2, "the deferred rename after the error ack", func(r renameRow) bool { return r.State == renameDone })
	stopTimers(a.mem)
}

// A rename already moving still orders a later rename of either name (it finishes first).
func TestEarlierOpenRenameIncludesMoving(t *testing.T) {
	a := rnAgent(t)
	ctx := context.Background()
	r1 := renameRow{ID: rnID, SpaceID: rnSpace, From: "implementor", To: "builder", State: renamePending}
	r2 := renameRow{ID: rnID2, SpaceID: rnSpace, From: "builder", To: "maker", State: renamePending}
	for _, r := range []renameRow{r1, r2} {
		if err := a.j.insertRoleRename(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.j.setRoleRenameState(ctx, rnID, renameMoving); err != nil {
		t.Fatal(err)
	}
	if got, err := a.j.earlierOpenRename(ctx, r2); err != nil || got.ID != rnID {
		t.Fatalf("earlier open rename of a later one while the first is moving: %q %v", got.ID, err)
	}
}

// The closed notice of a rename the node never journaled is journaled dropped, closed and acknowledged by itself, not as
// a pending row that the notice's close then happens to drop; a later retry of open renames moves nothing.
func TestInsertClosedRoleRenameWritesDropped(t *testing.T) {
	a := rnAgent(t)
	ctx := context.Background()
	rnSeed(t, a, "implementor")
	if err := a.j.insertClosedRoleRename(ctx, renameRow{ID: rnID, SpaceID: rnSpace, From: "implementor", To: "builder"}); err != nil {
		t.Fatal(err)
	}
	r, found, err := a.j.roleRename(ctx, rnID)
	if err != nil || !found || r.State != renameDropped || r.Result != proto.MemoryRenameClosed || !r.Acked {
		t.Fatalf("journaled %+v %v %v", r, found, err)
	}
	if open, _ := a.j.openRoleRenames(ctx); len(open) != 0 {
		t.Fatalf("a closed notice left an open rename: %+v", open)
	}
	a.retryRoleRenames(ctx, "")
	if exists(filepath.Join(a.mem.root, rnSpace, "builder")) {
		t.Fatal("a retry moved a closed rename")
	}
	stopTimers(a.mem)
}
