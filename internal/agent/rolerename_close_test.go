package agent

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// ADR-0093 addendum (T-H4): central closes a rename it gave up. The node learns it from a memory.renamed.ack with error
// closed (answering a report, or sent unsolicited), drops a rename that has not moved the folder whatever outcome it
// last reported, lifts its sync hold and never retries it. The old-name folder stays on disk, orphaned.

func rnPendingWithResult(t *testing.T, a *Agent, result, msg string, acked bool) {
	t.Helper()
	ctx := context.Background()
	if err := a.j.insertRoleRename(ctx, renameRow{ID: rnID, SpaceID: rnSpace, From: "implementor", To: "builder", State: renamePending}); err != nil {
		t.Fatal(err)
	}
	if err := a.j.setRoleRenameResult(ctx, rnID, result, msg); err != nil {
		t.Fatal(err)
	}
	if acked {
		if err := a.j.ackRoleRename(ctx, rnID, result, false); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRoleRenameClosedAckDropsPendingAndOrphansFolder(t *testing.T) {
	for _, c := range []struct {
		name, result string
		acked        bool
		ackResult    string
	}{
		{"failed and acknowledged (an unsolicited ack)", proto.MemoryRenameError, true, proto.MemoryRenameError},
		{"deferred, the ack's result differs", proto.MemoryRenameDeferred, true, proto.MemoryRenameError},
		{"failed, answering its report", proto.MemoryRenameError, false, proto.MemoryRenameError},
	} {
		t.Run(c.name, func(t *testing.T) {
			a := rnAgent(t)
			ctx := context.Background()
			rnSeed(t, a, "implementor")
			rnPendingWithResult(t, a, c.result, "rename: permission denied", c.acked)
			if !a.j.renameHeld(ctx, rnSpace, "implementor") || !a.j.renameHeld(ctx, rnSpace, "builder") {
				t.Fatal("an open rename does not hold syncs")
			}

			a.roleRenamedAck(ctx, proto.MemoryRenamedAck{RenameID: rnID, Result: c.ackResult, Error: proto.MemoryRenameClosed})

			r, _, _ := a.j.roleRename(ctx, rnID)
			if r.State != renameDropped || r.Result != proto.MemoryRenameClosed || !r.Acked {
				t.Fatalf("after the closed ack: %+v", r)
			}
			if a.j.renameHeld(ctx, rnSpace, "implementor") || a.j.renameHeld(ctx, rnSpace, "builder") {
				t.Error("a closed rename still holds syncs")
			}
			a.retryRoleRenames(ctx, "") // at the next start: not tried again
			if r, _, _ := a.j.roleRename(ctx, rnID); r.State != renameDropped {
				t.Fatalf("a closed rename was retried: %+v", r)
			}
			// The folder is left in place, with its history and the edit no checkpoint had seen; it is no collection.
			if _, err := os.Stat(filepath.Join(a.mem.root, rnSpace, "implementor", "shared.git")); err != nil {
				t.Fatalf("the orphaned folder is gone: %v", err)
			}
			if _, err := os.Stat(filepath.Join(a.mem.instance(rnSpace, "implementor", "ins_two"), "unsaved.md")); err != nil {
				t.Fatalf("an unsaved edit was lost: %v", err)
			}
			if exists(filepath.Join(a.mem.root, rnSpace, "builder")) {
				t.Error("the new name was created")
			}
			if !a.j.orphanedRole(ctx, rnSpace, "implementor") {
				t.Error("the old name is not marked orphaned")
			}
			if slices.Contains(a.mem.collections(ctx), [2]string{rnSpace, "implementor"}) {
				t.Error("the orphaned folder is still offered as a collection")
			}
			stopTimers(a.mem)
		})
	}
}

// A role of the old name starting again does not inherit the orphan: prepare sets the folder aside to .retained (never
// deleted), starts a fresh collection and settles the closed rename.
func TestPrepareSetsOrphanAside(t *testing.T) {
	a := rnAgent(t)
	ctx := context.Background()
	rnSeed(t, a, "implementor")
	rnPendingWithResult(t, a, proto.MemoryRenameError, "x", true)
	a.roleRenamedAck(ctx, proto.MemoryRenamedAck{RenameID: rnID, Result: proto.MemoryRenameError, Error: proto.MemoryRenameClosed})

	dir, err := a.mem.prepare(ctx, rnSpace, "implementor", "ins_new")
	if err != nil {
		t.Fatalf("prepare of the old name: %v", err)
	}
	if exists(filepath.Join(dir, "notes.md")) {
		t.Fatal("the new role inherited the orphan's files")
	}
	retained, _ := filepath.Glob(filepath.Join(a.mem.root, rnSpace, ".retained", "rename-"+rnID+"-implementor*"))
	if len(retained) != 1 {
		t.Fatalf("retained folders %v", retained)
	}
	if b, err := os.ReadFile(filepath.Join(retained[0], "instances", "ins_two", "unsaved.md")); err != nil || !strings.Contains(string(b), "no checkpoint") {
		t.Fatalf("the retained orphan lost its content: %v", err)
	}
	if a.j.orphanedRole(ctx, rnSpace, "implementor") {
		t.Error("the name is still orphaned after it was set aside")
	}
	if !slices.Contains(a.mem.collections(ctx), [2]string{rnSpace, "implementor"}) {
		t.Error("the new collection is not offered")
	}
	stopTimers(a.mem)
}

// A closed ack does not undo a move: a rename that is moving or done keeps its state (the folder is, or ends, under the new
// name, which is where central's copy is), and a late ok is acknowledged normally.
func TestRoleRenameClosedAckLeavesMovedRename(t *testing.T) {
	a := rnAgent(t)
	ctx := context.Background()
	rnSeed(t, a, "implementor")
	r := renameRow{ID: rnID, SpaceID: rnSpace, From: "implementor", To: "builder", State: renamePending}
	if err := a.j.insertRoleRename(ctx, r); err != nil {
		t.Fatal(err)
	}
	if res, msg := a.moveRoleMemory(ctx, r); res != proto.MemoryRenameOK {
		t.Fatalf("move: %s %s", res, msg)
	}
	a.roleRenamedAck(ctx, proto.MemoryRenamedAck{RenameID: rnID, Result: proto.MemoryRenameOK, Error: proto.MemoryRenameClosed})
	got, _, _ := a.j.roleRename(ctx, rnID)
	if got.State != renameDone || !got.Acked || got.Result != proto.MemoryRenameOK {
		t.Fatalf("after a late ok was answered closed: %+v", got)
	}
	if a.j.orphanedRole(ctx, rnSpace, "implementor") || !exists(filepath.Join(a.mem.root, rnSpace, "builder", "shared.git")) || exists(filepath.Join(a.mem.root, rnSpace, "implementor")) {
		t.Fatal("a done rename was treated as orphaned")
	}
	// A moving rename (a crash mid-move) is not dropped either.
	if err := a.j.setRoleRenameState(ctx, rnID, renameMoving); err != nil {
		t.Fatal(err)
	}
	if dropped, err := a.j.closeRoleRename(ctx, rnID); err != nil || dropped {
		t.Fatalf("closing a moving rename: %v %v", dropped, err)
	}
	stopTimers(a.mem)
}
