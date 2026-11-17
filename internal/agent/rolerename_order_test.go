package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// ADR-0093 addendum, ordering and forget: the node moves renames in the order it journaled them (central's order), and
// a closed notice journals a forgotten rename as closed, so its old folder is an orphan that no later role or rename
// of that name inherits.

const rnID3 = "rrn_rename00000000000000000003"

// rnCmd is a memory.rename command for any rename id; closed makes it central's closed notice.
func rnCmd(t *testing.T, id, from, to string, closed bool) proto.Command {
	t.Helper()
	rnSeq++
	b, err := json.Marshal(proto.MemoryRename{RenameID: id, SpaceID: rnSpace, From: from, To: to, Closed: closed})
	if err != nil {
		t.Fatal(err)
	}
	key := "role-rename:" + id
	if closed {
		key = "role-rename-closed:" + id
	}
	return proto.Command{CommandID: fmt.Sprintf("cmd_%026d", rnSeq), Kind: proto.CmdMemoryRename, SpaceID: rnSpace, Generation: 1, IdempotencyKey: key, Payload: b}
}

// rnWaitID waits until the journaled rename id satisfies cond.
func rnWaitID(t *testing.T, a *Agent, id, what string, cond func(renameRow) bool) renameRow {
	t.Helper()
	var r renameRow
	waitUntil(t, what, func() bool {
		var found bool
		r, found, _ = a.j.roleRename(context.Background(), id)
		return found && cond(r)
	})
	return r
}

// A later rename of either name (b->c, or a new a->x) waits while an earlier journaled rename a->b has not moved: it is
// deferred, naming the earlier one, and moves once that one is done, without waiting for a run's end or a restart. The
// role's memory ends under the last name, never stranded under b or inherited by x.
func TestRenameWaitsForEarlierRenameOfEitherName(t *testing.T) {
	for _, c := range []struct{ name, from, to, final string }{
		{"the new name renamed again", "builder", "maker", "maker"},
		{"the old name reused and renamed", "implementor", "other", "builder"},
	} {
		t.Run(c.name, func(t *testing.T) {
			a := rnAgent(t)
			ctx := context.Background()
			rnSeed(t, a, "implementor")
			// The earlier rename is journaled (its command arrived first) but has not moved yet.
			if err := a.j.insertRoleRename(ctx, renameRow{ID: rnID, SpaceID: rnSpace, From: "implementor", To: "builder", State: renamePending}); err != nil {
				t.Fatal(err)
			}
			a.handleMemoryRename(ctx, rnCmd(t, rnID2, c.from, c.to, false), true)
			later := rnWaitID(t, a, rnID2, "the later rename deferred", func(r renameRow) bool { return r.Result == proto.MemoryRenameDeferred })
			if later.State != renamePending || !strings.Contains(later.Message, rnID) {
				t.Fatalf("the later rename %+v, want deferred naming %s", later, rnID)
			}
			if !exists(filepath.Join(a.mem.root, rnSpace, "implementor", "instances", "ins_one", "notes.md")) {
				t.Fatal("the later rename moved the folder first")
			}
			// The earlier one moves (its command again); the later one follows at once.
			a.handleMemoryRename(ctx, rnCmd(t, rnID, "implementor", "builder", false), false)
			rnWaitID(t, a, rnID, "the earlier rename", func(r renameRow) bool { return r.State == renameDone })
			rnWaitID(t, a, rnID2, "the later rename", func(r renameRow) bool { return r.State == renameDone })
			if !exists(filepath.Join(a.mem.root, rnSpace, c.final, "instances", "ins_one", "notes.md")) {
				t.Fatalf("the role's memory is not under %s", c.final)
			}
			if c.final == "builder" && exists(filepath.Join(a.mem.root, rnSpace, "other", "instances", "ins_one", "notes.md")) {
				t.Fatal("the reused name's rename inherited the renamed role's memory")
			}
			if c.final == "maker" && exists(filepath.Join(a.mem.root, rnSpace, "builder", "instances", "ins_one", "notes.md")) {
				t.Fatal("the memory was stranded under the intermediate name")
			}
			stopTimers(a.mem)
		})
	}
}

// A rename journaled before another, or of other names, is not held by it.
func TestEarlierOpenRenameOnlyLooksBackAtSharedNames(t *testing.T) {
	a := rnAgent(t)
	ctx := context.Background()
	rows := []renameRow{
		{ID: rnID, SpaceID: rnSpace, From: "implementor", To: "builder", State: renamePending},
		{ID: rnID2, SpaceID: rnSpace, From: "reviewer", To: "critic", State: renamePending},
		{ID: rnID3, SpaceID: rnSpace, From: "builder", To: "maker", State: renamePending},
	}
	for _, r := range rows {
		if err := a.j.insertRoleRename(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	for i, want := range []string{"", "", rnID} {
		got, err := a.j.earlierOpenRename(ctx, rows[i])
		if err != nil || got.ID != want {
			t.Errorf("earlier open rename of %s: %q %v, want %q", rows[i].ID, got.ID, err, want)
		}
	}
	// A dropped earlier rename holds nothing.
	if _, err := a.j.closeRoleRename(ctx, rnID); err != nil {
		t.Fatal(err)
	}
	if got, _ := a.j.earlierOpenRename(ctx, rows[2]); got.ID != "" {
		t.Errorf("a dropped rename still holds a later one: %q", got.ID)
	}
}

// The closed notice of a forgotten rename the node never journaled (it was offline): it is journaled dropped and
// closed without a lease and without moving anything, and acknowledged. The old folder is an orphan: never offered for
// sync, and a later rename of the old name (a new role of that name that never ran here) sets it aside instead of
// moving it, so the target never inherits it.
func TestClosedNoticeOrphansOldFolder(t *testing.T) {
	a := rnAgent(t)
	ctx := context.Background()
	rnSeed(t, a, "implementor")
	delete(a.leases, rnSpace) // nothing on disk changes: no lease is needed
	a.handleMemoryRename(ctx, rnCmd(t, rnID, "implementor", "builder", true), true)
	r, found, err := a.j.roleRename(ctx, rnID)
	if err != nil || !found || r.State != renameDropped || r.Result != proto.MemoryRenameClosed || !r.Acked {
		t.Fatalf("journaled %+v %v %v", r, found, err)
	}
	if !exists(filepath.Join(a.mem.root, rnSpace, "implementor", "instances", "ins_one", "notes.md")) || exists(filepath.Join(a.mem.root, rnSpace, "builder")) {
		t.Fatal("the closed notice moved the folder")
	}
	if !a.j.orphanedRole(ctx, rnSpace, "implementor") || a.j.renameHeld(ctx, rnSpace, "builder") {
		t.Fatal("the old name is not an orphan, or the new one is held")
	}
	// The cancelled move never comes back; a repeated notice changes nothing.
	a.handleMemoryRename(ctx, rnCmd(t, rnID, "implementor", "builder", true), false)
	if r2, _, _ := a.j.roleRename(ctx, rnID); r2 != r {
		t.Fatalf("a repeated notice changed the rename: %+v", r2)
	}

	// Central reused the name and renamed it: implementor -> other. The orphan is set aside, other gets nothing.
	a.leases[rnSpace] = leaseRow{SpaceID: rnSpace, Generation: 1, ExpiresAt: a.now().Add(time.Hour), Workdir: "rn"}
	a.handleMemoryRename(ctx, rnCmd(t, rnID2, "implementor", "other", false), true)
	rnWaitID(t, a, rnID2, "the later rename", func(r renameRow) bool { return r.State == renameDone })
	if exists(filepath.Join(a.mem.root, rnSpace, "other", "instances", "ins_one", "notes.md")) {
		t.Fatal("the later rename inherited the orphaned folder")
	}
	retained, _ := filepath.Glob(filepath.Join(a.mem.root, rnSpace, ".retained", "rename-"+rnID+"-implementor*", "instances", "ins_one", "notes.md"))
	if len(retained) != 1 {
		t.Fatalf("the orphan was not kept aside: %v", retained)
	}
	if a.j.orphanedRole(ctx, rnSpace, "implementor") {
		t.Error("the orphan marker stayed after it was set aside")
	}
	stopTimers(a.mem)
}

// The closed notice drops a journaled rename that has not moved (the node had the move command, then central forgot
// it), and leaves a done one: its folder is under the new name already.
func TestClosedNoticeDropsPendingLeavesDone(t *testing.T) {
	a := rnAgent(t)
	ctx := context.Background()
	rnSeed(t, a, "implementor")
	if err := a.j.insertRoleRename(ctx, renameRow{ID: rnID, SpaceID: rnSpace, From: "implementor", To: "builder", State: renamePending}); err != nil {
		t.Fatal(err)
	}
	a.handleMemoryRename(ctx, rnCmd(t, rnID, "implementor", "builder", true), true)
	if r, _, _ := a.j.roleRename(ctx, rnID); r.State != renameDropped || r.Result != proto.MemoryRenameClosed {
		t.Fatalf("pending rename after the notice: %+v", r)
	}
	a.retryRoleRenames(ctx, "")
	if r, _, _ := a.j.roleRename(ctx, rnID); r.State != renameDropped || !exists(filepath.Join(a.mem.root, rnSpace, "implementor", "instances", "ins_one", "notes.md")) {
		t.Fatalf("a dropped rename moved: %+v", r)
	}

	rnSeed(t, a, "reviewer")
	r2 := renameRow{ID: rnID2, SpaceID: rnSpace, From: "reviewer", To: "critic", State: renamePending}
	if err := a.j.insertRoleRename(ctx, r2); err != nil {
		t.Fatal(err)
	}
	if res, msg := a.moveRoleMemory(ctx, r2); res != proto.MemoryRenameOK {
		t.Fatalf("move: %s %s", res, msg)
	}
	a.handleMemoryRename(ctx, rnCmd(t, rnID2, "reviewer", "critic", true), true)
	if r, _, _ := a.j.roleRename(ctx, rnID2); r.State != renameDone {
		t.Fatalf("a done rename after the notice: %+v", r)
	}
	stopTimers(a.mem)
}
