package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/runner/runnertest"
)

// Role rename on the node (ADR-0093 §3.3, T-H3): the memory.rename command, the move of the role's memory folder, the
// deferral while a run of the role is active, crash recovery, duplicates, and the report until central acknowledges it.

const (
	rnSpace = "spc_rename00000000000000000000"
	rnID    = "rrn_rename00000000000000000001"
)

// rnAgent is a memory agent holding the lease of rnSpace at generation 1.
func rnAgent(t *testing.T) *Agent {
	t.Helper()
	a := memAgent(t)
	a.mem.base = context.Background()
	t.Cleanup(func() { a.drain(10 * time.Second) })
	a.leases[rnSpace] = leaseRow{SpaceID: rnSpace, Generation: 1, ExpiresAt: a.now().Add(time.Hour), Workdir: "rn"}
	return a
}

var rnSeq int

// rnCommand is a memory.rename command for rnID (a new command ID each call, like central's re-queued command).
func rnCommand(t *testing.T, from, to string) proto.Command {
	t.Helper()
	rnSeq++
	b, err := json.Marshal(proto.MemoryRename{RenameID: rnID, SpaceID: rnSpace, From: from, To: to})
	if err != nil {
		t.Fatal(err)
	}
	return proto.Command{CommandID: fmt.Sprintf("cmd_%026d", rnSeq), Kind: proto.CmdMemoryRename, SpaceID: rnSpace, Generation: 1, IdempotencyKey: "role-rename:" + rnID, Payload: b}
}

// rnWait waits until the journaled rename satisfies cond and returns it.
func rnWait(t *testing.T, a *Agent, what string, cond func(renameRow) bool) renameRow {
	t.Helper()
	var r renameRow
	waitUntil(t, what, func() bool {
		var found bool
		r, found, _ = a.j.roleRename(context.Background(), rnID)
		return found && cond(r)
	})
	return r
}

func (m *memManager) mainHead(ctx context.Context, spaceID, role string) string {
	return m.git.RevParse(ctx, m.shared(spaceID, role), "refs/heads/main")
}

// rnSeed gives role a collection with two instances: one with a published note central acknowledged, a later published
// commit central has not, a preserved conflict folder, and an edit left in a checkout (not checkpointed yet). It returns
// the acknowledged head.
func rnSeed(t *testing.T, a *Agent, role string) string {
	t.Helper()
	ctx := context.Background()
	m := a.mem
	one, err := m.prepare(ctx, rnSpace, role, "ins_one")
	if err != nil {
		t.Fatal(err)
	}
	write(t, one, "notes.md", "acked note\n")
	if err := m.checkpointErr(ctx, rnSpace, role, "ins_one", "test"); err != nil {
		t.Fatal(err)
	}
	acked := m.mainHead(ctx, rnSpace, role)
	write(t, one, "conflicts/ins-two-20261102T090000Z/README.md", "preserved\n")
	write(t, one, "later.md", "not acked yet\n")
	if err := m.checkpointErr(ctx, rnSpace, role, "ins_one", "test"); err != nil {
		t.Fatal(err)
	}
	two, err := m.prepare(ctx, rnSpace, role, "ins_two")
	if err != nil {
		t.Fatal(err)
	}
	write(t, two, "unsaved.md", "an edit no checkpoint saw\n")
	st, _ := a.j.memory(ctx, rnSpace, role)
	st.Acked, st.Retries, st.LastError, st.Conflicts, st.ConflictFolders = acked, 4, "error: role renamed", 1, []string{"ins-two-20261102T090000Z"}
	if err := a.j.putMemory(ctx, st); err != nil {
		t.Fatal(err)
	}
	// In-memory state of the old name that the move drops.
	m.mu.Lock()
	m.waiting[colKey(rnSpace, role)] = "msy_old"
	m.syncs[colKey(rnSpace, role)] = time.AfterFunc(time.Hour, func() {})
	m.timers[colKey(rnSpace, role)+"/ins_one"] = time.AfterFunc(time.Hour, func() {})
	m.mu.Unlock()
	return acked
}

func gitShow(t *testing.T, a *Agent, role, file string) string {
	t.Helper()
	out, err := a.mem.git.Run(context.Background(), a.mem.shared(rnSpace, role), nil, "show", "main:"+file)
	if err != nil {
		t.Fatalf("show %s in %s: %v", file, role, err)
	}
	return out
}

// SM-MEM-007: a rename with no run moves the folder, keeps every commit, the preserved conflict folder and the edit no
// checkpoint had seen, moves the sync state (backoff reset, acked kept) and drops the old name's in-memory state.
// Instance checkouts keep working under the new name.
func TestRoleRenameMovesMemory(t *testing.T) {
	a := rnAgent(t)
	ctx := context.Background()
	m := a.mem
	acked := rnSeed(t, a, "implementor")

	a.command(ctx, rnCommand(t, "implementor", "builder"))
	r := rnWait(t, a, "rename done", func(r renameRow) bool { return r.State == renameDone })
	if r.Result != proto.MemoryRenameOK || r.Acked {
		t.Fatalf("journaled outcome %+v", r)
	}
	if exists(filepath.Join(m.root, rnSpace, "implementor")) {
		t.Fatal("the old folder is still there")
	}
	for f, want := range map[string]string{"notes.md": "acked note\n", "later.md": "not acked yet\n", "unsaved.md": "an edit no checkpoint saw\n",
		"conflicts/ins-two-20261102T090000Z/README.md": "preserved\n"} {
		if got := gitShow(t, a, "builder", f); got != want {
			t.Fatalf("%s under the new name: %q", f, got)
		}
	}
	if c := m.collections(); !slices.Equal(c, [][2]string{{rnSpace, "builder"}}) {
		t.Fatalf("collections %v", c)
	}
	st, _ := a.j.memory(ctx, rnSpace, "builder")
	if st.Acked != acked || st.Retries != 0 || st.LastError != "" || st.Conflicts != 1 || !slices.Equal(st.ConflictFolders, []string{"ins-two-20261102T090000Z"}) {
		t.Fatalf("sync state under the new name %+v", st)
	}
	if old, _ := a.j.memory(ctx, rnSpace, "implementor"); old.Acked != "" || old.Retries != 0 {
		t.Fatalf("sync state left under the old name %+v", old)
	}
	// The next sync bundles from the old acknowledged head: everything after it is pending under the new name.
	s := m.statusOf(ctx, rnSpace, "builder")
	if !s.Pending || s.Acked != acked || s.LocalCommits < 2 || s.Instances != 2 {
		t.Fatalf("status under the new name %+v", s)
	}
	m.mu.Lock()
	_, w := m.waiting[colKey(rnSpace, "implementor")]
	_, sy := m.syncs[colKey(rnSpace, "implementor")]
	_, tm := m.timers[colKey(rnSpace, "implementor")+"/ins_one"]
	m.mu.Unlock()
	if w || sy || tm {
		t.Fatalf("in-memory state of the old name kept: waiting %v sync %v timer %v", w, sy, tm)
	}
	// Both names are held from syncing until central acknowledges the ok.
	if !a.j.renameHeld(ctx, rnSpace, "builder") || !a.j.renameHeld(ctx, rnSpace, "implementor") {
		t.Fatal("a name of the open rename is not held")
	}
	// Checkouts still publish and refresh under the new name.
	two := m.instance(rnSpace, "builder", "ins_two")
	write(t, two, "after.md", "written after the move\n")
	if err := m.checkpointErr(ctx, rnSpace, "builder", "ins_two", "test"); err != nil {
		t.Fatal(err)
	}
	if got := gitShow(t, a, "builder", "after.md"); got != "written after the move\n" {
		t.Fatalf("publish after the move: %q", got)
	}
	one, err := m.prepare(ctx, rnSpace, "builder", "ins_one")
	if err != nil || read(t, one, "after.md") != "written after the move\n" {
		t.Fatalf("refresh after the move: %v", err)
	}
	// A checkpoint timer of the old name that fired anyway finds no checkout and writes nothing.
	if err := m.checkpointErr(ctx, rnSpace, "implementor", "ins_one", "idle"); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(m.root, rnSpace, "implementor")) {
		t.Fatal("a late checkpoint recreated the old folder")
	}

	// Central acknowledges the ok: the hold ends.
	a.roleRenamedAck(ctx, proto.MemoryRenamedAck{RenameID: rnID, Result: proto.MemoryRenameOK})
	if r, _, _ := a.j.roleRename(ctx, rnID); !r.Acked {
		t.Fatalf("ack not recorded %+v", r)
	}
	if a.j.renameHeld(ctx, rnSpace, "builder") || a.j.renameHeld(ctx, rnSpace, "implementor") {
		t.Fatal("still held after central acknowledged the ok")
	}
	stopTimers(m)
}

func stopTimers(m *memManager) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, t := range m.syncs {
		t.Stop()
	}
	for _, t := range m.timers {
		t.Stop()
	}
}

// A repeated memory.rename (the same command again, or central's re-queued one) answers ok again from the journal and
// moves nothing twice.
func TestRoleRenameDuplicateAnswersOK(t *testing.T) {
	a := rnAgent(t)
	ctx := context.Background()
	rnSeed(t, a, "implementor")
	c := rnCommand(t, "implementor", "builder")
	a.command(ctx, c)
	rnWait(t, a, "rename done", func(r renameRow) bool { return r.State == renameDone })
	a.roleRenamedAck(ctx, proto.MemoryRenamedAck{RenameID: rnID, Result: proto.MemoryRenameOK})
	head := a.mem.mainHead(ctx, rnSpace, "builder")
	// A leftover under the new name now must not be touched: the rename is done.
	for _, cmd := range []proto.Command{c, rnCommand(t, "implementor", "builder")} {
		a.command(ctx, cmd)
		r := rnWait(t, a, "ok to report again", func(r renameRow) bool { return !r.Acked })
		if r.State != renameDone || r.Result != proto.MemoryRenameOK {
			t.Fatalf("after a repeat %+v", r)
		}
		a.roleRenamedAck(ctx, proto.MemoryRenamedAck{RenameID: rnID, Result: proto.MemoryRenameOK})
	}
	if got := a.mem.mainHead(ctx, rnSpace, "builder"); got != head {
		t.Fatalf("a repeat changed the memory: %s → %s", head, got)
	}
	if exists(filepath.Join(a.mem.root, rnSpace, ".retained")) {
		t.Fatal("a repeat set the moved folder aside")
	}
	// A command reusing the rename ID for other names is refused.
	a.command(ctx, rnCommand(t, "implementor", "other"))
	if exists(filepath.Join(a.mem.root, rnSpace, "other")) {
		t.Fatal("moved by a command with other names")
	}
	stopTimers(a.mem)
}

// A run of either name active on the node defers the move (nothing changes); it moves once the run has ended.
func TestRoleRenameDeferredWhileRunActive(t *testing.T) {
	for _, role := range []string{"implementor", "builder"} {
		t.Run(role, func(t *testing.T) {
			a := rnAgent(t)
			ctx := context.Background()
			rnSeed(t, a, "implementor")
			run := runRow{RunID: "run_rename00000000000000000001", SpaceID: rnSpace, Generation: 1, Kind: "worker", Role: role, InstanceID: "ins_one",
				TerminalName: "r-x", Desired: "running", State: proto.RunRunning}
			if err := a.j.insertRun(ctx, run); err != nil {
				t.Fatal(err)
			}
			a.command(ctx, rnCommand(t, "implementor", "builder"))
			r := rnWait(t, a, "deferred", func(r renameRow) bool { return r.Result == proto.MemoryRenameDeferred })
			if r.State != renamePending || !strings.Contains(r.Message, run.RunID) {
				t.Fatalf("deferred rename %+v", r)
			}
			if !exists(a.mem.shared(rnSpace, "implementor")) || exists(filepath.Join(a.mem.root, rnSpace, "builder")) {
				t.Fatal("moved while a run was active")
			}
			// Still active: trying again changes nothing and does not report the same deferral again once acknowledged.
			a.roleRenamedAck(ctx, proto.MemoryRenamedAck{RenameID: rnID, Result: proto.MemoryRenameDeferred})
			a.retryRoleRenames(ctx, rnSpace)
			if r, _, _ := a.j.roleRename(ctx, rnID); !r.Acked || r.State != renamePending {
				t.Fatalf("a retry while still busy %+v", r)
			}
			run.State = proto.RunExited
			if err := a.j.updateRun(ctx, run); err != nil {
				t.Fatal(err)
			}
			a.retryRoleRenames(ctx, rnSpace) // what the run-ended path calls
			r = rnWait(t, a, "moved after the run ended", func(r renameRow) bool { return r.State == renameDone })
			if r.Result != proto.MemoryRenameOK || r.Acked || !exists(a.mem.shared(rnSpace, "builder")) {
				t.Fatalf("after the run ended %+v", r)
			}
			stopTimers(a.mem)
		})
	}
}

// A dream run in the space defers the move too: its workspace links the role's checkout under the old name.
func TestRoleRenameDeferredWhileDreamActive(t *testing.T) {
	a := rnAgent(t)
	ctx := context.Background()
	rnSeed(t, a, "implementor")
	if err := a.j.insertRun(ctx, runRow{RunID: "run_rename00000000000000000002", SpaceID: rnSpace, Generation: 1, Kind: "dreamer", Role: "dreamer",
		InstanceID: "ins_d", TerminalName: "r-d", State: proto.RunRunning}); err != nil {
		t.Fatal(err)
	}
	a.command(ctx, rnCommand(t, "implementor", "builder"))
	r := rnWait(t, a, "deferred", func(r renameRow) bool { return r.Result == proto.MemoryRenameDeferred })
	if !strings.Contains(r.Message, "dream run") || exists(filepath.Join(a.mem.root, rnSpace, "builder")) {
		t.Fatalf("dream: %+v", r)
	}
	stopTimers(a.mem)
}

// A crash after the folder was renamed, before the journal recorded it, is finished at start: the folder is not set
// aside as a leftover and the sync state moves.
func TestRoleRenameCrashMidMove(t *testing.T) {
	a := rnAgent(t)
	ctx := context.Background()
	acked := rnSeed(t, a, "implementor")
	if err := a.j.insertRoleRename(ctx, renameRow{ID: rnID, SpaceID: rnSpace, From: "implementor", To: "builder", State: renameMoving}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(a.mem.root, rnSpace)
	if err := os.Rename(filepath.Join(dir, "implementor"), filepath.Join(dir, "builder")); err != nil {
		t.Fatal(err)
	}
	a.retryRoleRenames(ctx, "") // agent start
	r, _, _ := a.j.roleRename(ctx, rnID)
	if r.State != renameDone || r.Result != proto.MemoryRenameOK {
		t.Fatalf("after the restart %+v", r)
	}
	if exists(filepath.Join(dir, ".retained")) || gitShow(t, a, "builder", "notes.md") != "acked note\n" {
		t.Fatal("the moved folder was set aside or lost")
	}
	if st, _ := a.j.memory(ctx, rnSpace, "builder"); st.Acked != acked || st.Retries != 0 {
		t.Fatalf("sync state %+v", st)
	}
	stopTimers(a.mem)
}

// A crash after the journal said moving but before the folder was renamed: the start renames it.
func TestRoleRenameCrashBeforeMove(t *testing.T) {
	a := rnAgent(t)
	ctx := context.Background()
	rnSeed(t, a, "implementor")
	if err := a.j.insertRoleRename(ctx, renameRow{ID: rnID, SpaceID: rnSpace, From: "implementor", To: "builder", State: renameMoving}); err != nil {
		t.Fatal(err)
	}
	a.retryRoleRenames(ctx, "")
	if r, _, _ := a.j.roleRename(ctx, rnID); r.State != renameDone || !exists(a.mem.shared(rnSpace, "builder")) || exists(filepath.Join(a.mem.root, rnSpace, "implementor")) {
		t.Fatalf("after the restart %+v", r)
	}
	stopTimers(a.mem)
}

// Nothing under either name: the rename is ok (the role never had memory on this node).
func TestRoleRenameNothingToMove(t *testing.T) {
	a := rnAgent(t)
	ctx := context.Background()
	a.command(ctx, rnCommand(t, "implementor", "builder"))
	r := rnWait(t, a, "rename done", func(r renameRow) bool { return r.State == renameDone })
	if r.Result != proto.MemoryRenameOK || exists(filepath.Join(a.mem.root, rnSpace, "builder")) {
		t.Fatalf("nothing to move %+v", r)
	}
}

// A folder already under the new name is a leftover: it is set aside under .retained, never merged, and its sync state
// is dropped; the renamed role's folder and state take the name.
func TestRoleRenameRetainsLeftover(t *testing.T) {
	for _, withFrom := range []bool{true, false} {
		t.Run(map[bool]string{true: "moved", false: "nothing to move"}[withFrom], func(t *testing.T) {
			a := rnAgent(t)
			ctx := context.Background()
			m := a.mem
			left, err := m.prepare(ctx, rnSpace, "builder", "ins_left")
			if err != nil {
				t.Fatal(err)
			}
			write(t, left, "leftover.md", "an old builder's note\n")
			if err := m.checkpointErr(ctx, rnSpace, "builder", "ins_left", "test"); err != nil {
				t.Fatal(err)
			}
			if err := a.j.putMemory(ctx, memRow{SpaceID: rnSpace, Role: "builder", Acked: "deadbeef", Retries: 7}); err != nil {
				t.Fatal(err)
			}
			var acked string
			if withFrom {
				acked = rnSeed(t, a, "implementor")
			}
			a.command(ctx, rnCommand(t, "implementor", "builder"))
			rnWait(t, a, "rename done", func(r renameRow) bool { return r.State == renameDone })
			kept := filepath.Join(m.root, rnSpace, ".retained", "rename-"+rnID+"-builder")
			if b, err := os.ReadFile(filepath.Join(kept, "instances", "ins_left", "leftover.md")); err != nil || string(b) != "an old builder's note\n" {
				t.Fatalf("leftover not retained: %q %v", b, err)
			}
			st, _ := a.j.memory(ctx, rnSpace, "builder")
			if st.Acked != acked || st.Retries != 0 {
				t.Fatalf("sync state %+v (want acked %q)", st, acked)
			}
			want := [][2]string{}
			if withFrom {
				want = [][2]string{{rnSpace, "builder"}}
				if _, err := m.git.Run(ctx, m.shared(rnSpace, "builder"), nil, "show", "main:leftover.md"); err == nil {
					t.Fatal("the leftover was merged into the renamed role")
				}
			}
			if c := m.collections(); !slices.Equal(c, want) && len(c)+len(want) > 0 {
				t.Fatalf("collections %v, want %v (.retained is not one)", c, want)
			}
			if validMemoryKey(rnSpace, ".retained", "ins_x") {
				t.Fatal(".retained is a valid role key")
			}
			stopTimers(m)
		})
	}
}

// A move that fails is reported as error, stays journaled, and is tried again at the next start (not at a run's end).
func TestRoleRenameErrorRetriedAtStart(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	a := rnAgent(t)
	ctx := context.Background()
	rnSeed(t, a, "implementor")
	dir := filepath.Join(a.mem.root, rnSpace)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	a.command(ctx, rnCommand(t, "implementor", "builder"))
	r := rnWait(t, a, "failed", func(r renameRow) bool { return r.Result == proto.MemoryRenameError })
	if r.State == renameDone || !strings.Contains(r.Message, "move the folder") || !exists(a.mem.shared(rnSpace, "implementor")) {
		t.Fatalf("failed move %+v", r)
	}
	a.roleRenamedAck(ctx, proto.MemoryRenamedAck{RenameID: rnID, Result: proto.MemoryRenameError})
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	a.retryRoleRenames(ctx, rnSpace) // a run ended: a failed move waits for the start
	if r, _, _ := a.j.roleRename(ctx, rnID); r.State == renameDone {
		t.Fatal("a failed move was retried at a run's end")
	}
	a.retryRoleRenames(ctx, "") // agent start
	if r, _, _ := a.j.roleRename(ctx, rnID); r.State != renameDone || r.Result != proto.MemoryRenameOK || r.Acked {
		t.Fatalf("after the start %+v", r)
	}
	stopTimers(a.mem)
}

// A rename the node has not journaled is refused without a valid lease (central queues it again); a payload for another
// space is refused.
func TestRoleRenameNeedsLease(t *testing.T) {
	a := rnAgent(t)
	ctx := context.Background()
	c := rnCommand(t, "implementor", "builder")
	c.Generation = 2
	a.command(ctx, c)
	if _, found, _ := a.j.roleRename(ctx, rnID); found {
		t.Fatal("journaled without a lease")
	}
	c = rnCommand(t, "implementor", "builder")
	c.SpaceID = "spc_" + strings.Repeat("1", 26)
	a.command(ctx, c)
	if _, found, _ := a.j.roleRename(ctx, rnID); found {
		t.Fatal("journaled for another space")
	}
}

// An ack with an error (central does not know the rename) drops a rename that has not moved; an ack of an older outcome
// does not acknowledge the current one.
func TestRoleRenameAcks(t *testing.T) {
	a := rnAgent(t)
	ctx := context.Background()
	if err := a.j.insertRoleRename(ctx, renameRow{ID: rnID, SpaceID: rnSpace, From: "implementor", To: "builder", State: renamePending}); err != nil {
		t.Fatal(err)
	}
	if err := a.j.setRoleRenameResult(ctx, rnID, proto.MemoryRenameOK, ""); err != nil {
		t.Fatal(err)
	}
	a.roleRenamedAck(ctx, proto.MemoryRenamedAck{RenameID: rnID, Result: proto.MemoryRenameDeferred})
	if r, _, _ := a.j.roleRename(ctx, rnID); r.Acked {
		t.Fatal("an ack of an older outcome acknowledged the current one")
	}
	a.roleRenamedAck(ctx, proto.MemoryRenamedAck{RenameID: rnID, Result: proto.MemoryRenameOK, Error: "unknown rename"})
	r, _, _ := a.j.roleRename(ctx, rnID)
	if !r.Acked || r.State != renameDropped {
		t.Fatalf("refused rename %+v", r)
	}
	if a.j.renameHeld(ctx, rnSpace, "implementor") {
		t.Fatal("a dropped rename holds the name")
	}
	a.retryRoleRenames(ctx, "")
	if r, _, _ := a.j.roleRename(ctx, rnID); r.State != renameDropped {
		t.Fatalf("a dropped rename was tried again %+v", r)
	}
}

// rnCentral accepts the node, welcomes it at minor and forwards what it sends; send writes to the node.
type rnCentral struct {
	srv  *httptest.Server
	msgs chan proto.Envelope
	out  chan []byte
}

func newRNCentral(t *testing.T, minor int) *rnCentral {
	t.Helper()
	fc := &rnCentral{msgs: make(chan proto.Envelope, 256), out: make(chan []byte, 16)}
	fc.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{proto.Subprotocol}})
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		conn.SetReadLimit(proto.MaxMessage)
		go func() {
			for {
				select {
				case b := <-fc.out:
					if conn.Write(r.Context(), websocket.MessageText, b) != nil {
						return
					}
				case <-r.Context().Done():
					return
				}
			}
		}()
		for {
			_, b, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			env, err := proto.Decode(b)
			if err != nil {
				return
			}
			fc.msgs <- env
			if env.Type == proto.TypeHello {
				fc.send(t, proto.TypeWelcome, proto.Welcome{NodeID: "nod_test", Minor: minor, HeartbeatInterval: time.Hour})
			}
		}
	}))
	t.Cleanup(fc.srv.Close)
	return fc
}

func (fc *rnCentral) send(t *testing.T, typ string, body any) {
	b, err := proto.Encode("msg_"+typ, typ, time.Now(), body)
	if err != nil {
		t.Error(err)
		return
	}
	fc.out <- b
}

// next decodes the next message of type typ into v, failing after a timeout.
func (fc *rnCentral) next(t *testing.T, typ string, v any) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case env := <-fc.msgs:
			if env.Type == typ {
				if err := env.Decode(v); err != nil {
					t.Fatal(err)
				}
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s", typ)
		}
	}
}

// Over a connection: the node advertises minor 14; a journaled outcome central has not acknowledged is sent again after
// the welcome; until the ack the new name does not sync; after it, the role syncs under the new name from the old
// acknowledged head.
func TestRoleRenameReportedUntilAcked(t *testing.T) {
	a := rnAgent(t)
	ctx := context.Background()
	acked := rnSeed(t, a, "implementor")
	stopTimers(a.mem)
	a.mem.waiting = map[string]string{}
	if err := a.j.insertRoleRename(ctx, renameRow{ID: rnID, SpaceID: rnSpace, From: "implementor", To: "builder", State: renamePending}); err != nil {
		t.Fatal(err)
	}
	a.retryRoleRenames(ctx, "") // moves; the report can't go out (not connected)
	a.o.Runner = runnertest.New()
	fc := newRNCentral(t, proto.MaxMinor)
	runAgent2(t, a, fc.srv.URL)
	var hello proto.Hello
	fc.next(t, proto.TypeHello, &hello)
	if hello.MaxMinor < proto.MinorRoleRename {
		t.Fatalf("the agent advertises minor %d, below %d", hello.MaxMinor, proto.MinorRoleRename)
	}
	var rep proto.MemoryRenamed
	fc.next(t, proto.TypeMemoryRenamed, &rep)
	if rep.RenameID != rnID || rep.SpaceID != rnSpace || rep.Result != proto.MemoryRenameOK {
		t.Fatalf("report %+v", rep)
	}
	// No sync of either name before the ack (the welcome's resume offered every collection).
	deadline := time.After(300 * time.Millisecond)
wait:
	for {
		select {
		case env := <-fc.msgs:
			if env.Type == proto.TypeMemorySync {
				t.Fatal("synced before central acknowledged the rename")
			}
		case <-deadline:
			break wait
		}
	}
	fc.send(t, proto.TypeMemoryRenamedAck, proto.MemoryRenamedAck{RenameID: rnID, Result: proto.MemoryRenameOK})
	var sy proto.MemorySync
	fc.next(t, proto.TypeMemorySync, &sy)
	if sy.SpaceID != rnSpace || sy.Role != "builder" || sy.Parent != acked {
		t.Fatalf("sync after the ack %+v (want parent %s)", sy, acked)
	}
	waitUntil(t, "ack journaled", func() bool { r, _, _ := a.j.roleRename(ctx, rnID); return r.Acked })
}

// runAgent2 runs the agent against a central at url until the test ends.
func runAgent2(t *testing.T, a *Agent, url string) {
	t.Helper()
	a.o.CentralURL, a.o.Credential = url, "spaceman_nd_test"
	a.o.BridgeSocket = filepath.Join(t.TempDir(), "b.sock")
	if a.o.ReconnectMax == 0 {
		a.o.ReconnectMax = 100 * time.Millisecond
	}
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = a.Run(runCtx); close(done) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}
