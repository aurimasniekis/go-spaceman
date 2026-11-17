package app_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/catalog"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	hns "github.com/aurimasniekis/go-spaceman/internal/harness"
	"github.com/aurimasniekis/go-spaceman/internal/memory"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store"
)

// ADR-0093 T-H3 end to end (simulated: in-process central and node agent, fake runner): the node agent speaks protocol
// minor 14 and moves a renamed role's memory folder on memory.rename, central marks the rename done and lifts the
// launch hold, and the role's memory, including what the node wrote after its last sync, reaches central under the new
// name with its history.

// agentRenameEnv is a central with the real node agent connected and a space with the starter roles on it.
type agentRenameEnv struct {
	*fleetEnv
	ctx     context.Context
	nodeID  string
	spaceID string
}

func newAgentRenameEnv(t *testing.T) *agentRenameEnv {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	e := newFleetEnv(t, longLease)
	e.enroll()
	e.startAgent()
	ctx := context.Background()
	nodes, _ := e.c.Fleet.ListNodes(ctx)
	eventually(t, "node connected", func() bool { n, err := e.c.Fleet.GetNode(ctx, nodes[0].ID); return err == nil && n.Connected })
	sp, err := e.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "payments", Name: "Payments", NodeID: nodes[0].ID, StarterRoles: true})
	if err != nil {
		t.Fatal(err)
	}
	bindStubAccounts(t, e.c, sp.ID)
	eventually(t, "lease granted", func() bool { x, _ := e.c.Fleet.GetSpace(ctx, sp.ID); return x.Lease != nil && x.Lease.Valid })
	return &agentRenameEnv{fleetEnv: e, ctx: ctx, nodeID: nodes[0].ID, spaceID: sp.ID}
}

func (e *agentRenameEnv) memDir(role string) string {
	return filepath.Join(e.agentDir, "memory", e.spaceID, role)
}

// runWithMemory launches a run of role, writes file into its memory checkout and returns the run (still running).
func (e *agentRenameEnv) runWithMemory(role, file, content string) *fleet.Run {
	e.t.Helper()
	run, err := e.c.Fleet.LaunchRun(e.ctx, admin, e.spaceID, fleet.LaunchInput{Kind: "worker", Role: role, Harness: hns.KindClaude})
	if err != nil {
		e.t.Fatal(err)
	}
	eventually(e.t, "run running", func() bool { return e.run1(run.ID).Observed == "running" })
	r := e.run1(run.ID)
	if err := os.WriteFile(filepath.Join(e.memDir(role), "instances", r.InstanceID, file), []byte(content), 0o600); err != nil {
		e.t.Fatal(err)
	}
	return r
}

func (e *agentRenameEnv) stopRun(id string) {
	e.t.Helper()
	if _, err := e.c.Fleet.StopRun(e.ctx, admin, id, "kill", "done"); err != nil {
		e.t.Fatal(err)
	}
	eventually(e.t, "run ended", func() bool { return e.run1(id).Observed == "exited" })
}

func (e *agentRenameEnv) synced(role, file string) {
	e.t.Helper()
	eventually(e.t, file+" synced under "+role, func() bool {
		files, err := e.c.Memory.Files(e.ctx, e.spaceID, role)
		return err == nil && slices.Contains(files, file)
	})
}

func (e *agentRenameEnv) rename(from, to string) *catalog.RoleRename {
	e.t.Helper()
	_, rn, err := e.c.Catalog.RenameSpaceRole(e.ctx, admin, e.spaceID, from, to, 0)
	if err != nil {
		e.t.Fatal(err)
	}
	if rn.NodeState != catalog.RenameNodePending || rn.NodeID != e.nodeID {
		e.t.Fatalf("rename %+v", rn)
	}
	return rn
}

func (e *agentRenameEnv) renameState(id string) catalog.RoleRename {
	e.t.Helper()
	l, err := e.c.Catalog.ListRoleRenames(e.ctx, e.spaceID)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, r := range l {
		if r.ID == id {
			return *r
		}
	}
	e.t.Fatalf("no rename %s", id)
	return catalog.RoleRename{}
}

// texts runs a query of one text column and returns its rows.
func (e *agentRenameEnv) texts(q string, args ...any) []string {
	e.t.Helper()
	rows, err := e.c.Store.ReadDB().QueryContext(e.ctx, q, args...)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		e.t.Fatal(err)
	}
	return out
}

// nodeResults are the node's memory.renamed results central recorded for the rename, in order (its audit events).
func (e *agentRenameEnv) nodeResults(id string) []string {
	e.t.Helper()
	return e.texts(`SELECT json_extract(details_json, '$.result') FROM audit_events
		WHERE action = 'space.role_rename_node' AND json_extract(details_json, '$.rename_id') = ? ORDER BY seq`, id)
}

// renameCommands are the states of the rename's memory.rename commands.
func (e *agentRenameEnv) renameCommands(id string) []string {
	e.t.Helper()
	return e.texts(`SELECT state FROM node_commands WHERE kind = ? AND idempotency_key LIKE ? ORDER BY created_at`,
		proto.CmdMemoryRename, catalog.RenameCommandKey(id)+"%")
}

func (e *agentRenameEnv) launchHeld(role string) bool {
	e.t.Helper()
	run, err := e.c.Fleet.LaunchRun(e.ctx, admin, e.spaceID, fleet.LaunchInput{Kind: "worker", Role: role, Harness: hns.KindClaude})
	var pe *fleet.RenamePendingError
	if errors.As(err, &pe) {
		return true
	}
	if err != nil {
		e.t.Fatalf("launch %s: %v", role, err)
	}
	e.stopRun(run.ID)
	return false
}

// done waits for the rename to be done and checks the memory moved and synced under the new name with no loss. before are
// the history commits the role had before the rename: each must still be in the new collection's history, and there must
// be at least one commit more than that (the write after the rename).
func (e *agentRenameEnv) done(rn *catalog.RoleRename, files map[string]string, before []memory.Commit) {
	e.t.Helper()
	eventually(e.t, "rename done", func() bool { return e.renameState(rn.ID).NodeState == catalog.RenameNodeDone })
	if pathExists(e.memDir(rn.From)) || !pathExists(filepath.Join(e.memDir(rn.To), "shared.git")) {
		e.t.Fatal("the node's folder did not move")
	}
	for f, want := range files {
		e.synced(rn.To, f)
		if got, err := e.c.Memory.File(e.ctx, e.spaceID, rn.To, f); err != nil || got != want {
			e.t.Fatalf("%s under %s: %q %v", f, rn.To, got, err)
		}
	}
	// Central's head is the node's: nothing the node holds is still to sync.
	eventually(e.t, "node and central agree", func() bool {
		c, err := e.c.Memory.Get(e.ctx, e.spaceID, rn.To)
		return err == nil && c.Node != nil && !c.Node.Pending && c.Node.Acked == c.Head
	})
	hist, err := e.c.Memory.History(e.ctx, e.spaceID, rn.To, 100)
	if err != nil || len(hist) < len(before)+1 {
		e.t.Fatalf("history under %s: %d commits %v", rn.To, len(hist), err)
	}
	have := make(map[string]bool, len(hist))
	for _, c := range hist {
		have[c.ID] = true
	}
	for _, c := range before {
		if !have[c.ID] {
			e.t.Fatalf("commit %s from before the rename is not in the history under %s", c.ID, rn.To)
		}
	}
	cols, err := e.c.Memory.List(e.ctx, e.spaceID)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, c := range cols {
		if c.Role == rn.From {
			e.t.Fatalf("a collection under the old name %s", rn.From)
		}
	}
	if e.launchHeld(rn.To) {
		e.t.Fatal("the launch hold was not lifted")
	}
}

func pathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// The node is online: memory.rename is delivered and accepted, the node moves the folder (with an edit it had not
// synced) and reports ok, central acknowledges it, the rename is done, the hold is lifted and the memory syncs under
// the new name with the history from before the rename.
func TestRoleRenameAgentMovesMemory(t *testing.T) {
	t.Parallel()
	e := newAgentRenameEnv(t)
	r := e.runWithMemory("implementor", "decisions.md", "- settle in batches of 500\n")
	e.stopRun(r.ID)
	e.synced("implementor", "decisions.md")
	before, err := e.c.Memory.History(e.ctx, e.spaceID, "implementor", 100)
	if err != nil {
		t.Fatal(err)
	}
	// Written after the last checkpoint and sync: it must not be lost.
	inst := filepath.Join(e.memDir("implementor"), "instances", r.InstanceID)
	if err := os.WriteFile(filepath.Join(inst, "late.md"), []byte("written after the sync\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	rn := e.rename("implementor", "builder")
	e.done(rn, map[string]string{"decisions.md": "- settle in batches of 500\n", "late.md": "written after the sync\n"}, before)
	if got := e.nodeResults(rn.ID); !slices.Equal(got, []string{proto.MemoryRenameOK}) {
		t.Fatalf("node reports %v", got)
	}
	if got := e.renameCommands(rn.ID); !slices.Equal(got, []string{"accepted"}) && !slices.Equal(got, []string{"done"}) {
		t.Fatalf("memory.rename commands %v", got)
	}
}

// ADR-0093 T-L3 (simulated: in-process central and node agent, fake runner): the node is disconnected when its role is
// renamed. The rename commits at once and stays pending: a launch of the new name is held, and the node writes more memory
// under the old name while it is away. When the node reconnects it moves the folder, the late write with it, and central
// ends with one collection under the new name whose history holds the commit from before the disconnect and the one
// after it. The online rename is TestRoleRenameAgentMovesMemory.
func TestRoleRenameAgentMemoryMoveWithNodeOffline(t *testing.T) {
	t.Parallel()
	e := newAgentRenameEnv(t)
	r := e.runWithMemory("implementor", "decisions.md", "- settle in batches of 500\n")
	e.stopRun(r.ID)
	e.synced("implementor", "decisions.md")
	before, err := e.c.Memory.History(e.ctx, e.spaceID, "implementor", 100)
	if err != nil {
		t.Fatal(err)
	}

	e.stopAgent()
	eventually(t, "node disconnected", func() bool { n, err := e.c.Fleet.GetNode(e.ctx, e.nodeID); return err == nil && !n.Connected })
	rn := e.rename("implementor", "builder")
	if !e.launchHeld("builder") {
		t.Fatal("a launch of the new name is not held while the node is away")
	}
	if st := e.renameState(rn.ID); st.NodeState != catalog.RenameNodePending {
		t.Fatalf("while the node is away: %+v", st)
	}
	// Written on the node while it is away: the folder is still under the old name.
	inst := filepath.Join(e.memDir("implementor"), "instances", r.InstanceID)
	if err := os.WriteFile(filepath.Join(inst, "late.md"), []byte("written while the node was away\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	e.startAgent()
	e.done(rn, map[string]string{"decisions.md": "- settle in batches of 500\n", "late.md": "written while the node was away\n"}, before)
	if got := e.nodeResults(rn.ID); !slices.Equal(got, []string{proto.MemoryRenameOK}) {
		t.Fatalf("node reports %v", got)
	}
	cols, err := e.c.Memory.List(e.ctx, e.spaceID)
	if err != nil {
		t.Fatal(err)
	}
	var toCols int
	for _, c := range cols {
		if c.Role == rn.To {
			toCols++
		}
	}
	if toCols != 1 {
		t.Fatalf("%d collections under %s, want 1", toCols, rn.To)
	}
}

// A run of the role central does not know is live (its row says it ended) defers the move: the node reports deferred
// and the rename stays pending and held. When the run ends on the node, the node moves the folder and reports ok.
func TestRoleRenameAgentDeferredWhileRunActive(t *testing.T) {
	t.Parallel()
	e := newAgentRenameEnv(t)
	r := e.runWithMemory("implementor", "decisions.md", "- keep\n")
	// Central loses track of the run (its row ends) while it still runs on the node.
	if err := e.c.Store.W().TxNamed(e.ctx, "test.run_ended", func(tx *store.Tx) error {
		_, err := tx.SQL.ExecContext(e.ctx, `UPDATE runs SET observed = 'exited' WHERE id = ?`, r.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	rn := e.rename("implementor", "builder")
	eventually(t, "deferred", func() bool { return slices.Contains(e.nodeResults(rn.ID), proto.MemoryRenameDeferred) })
	if st := e.renameState(rn.ID); st.NodeState != catalog.RenameNodePending && st.NodeState != catalog.RenameNodeDone {
		t.Fatalf("after the deferral %+v", st)
	}
	if !pathExists(e.memDir("implementor")) {
		t.Fatal("moved while the run was active")
	}
	if st := e.renameState(rn.ID); st.NodeState == catalog.RenameNodePending {
		if !strings.Contains(st.NodeMessage, r.ID) {
			t.Fatalf("deferral message %q", st.NodeMessage)
		}
		if !e.launchHeld("builder") {
			t.Fatal("a launch of the new name was not held")
		}
	}
	// The run ends on the node (central already thinks so).
	if procs, err := e.run.List(e.ctx, map[string]string{"spaceman.run": r.ID}, false); err != nil || len(procs) != 1 {
		t.Fatalf("the run's process: %v %v", procs, err)
	} else {
		e.run.Exit(procs[0].ID, 0, "")
	}
	e.done(rn, map[string]string{"decisions.md": "- keep\n"}, nil)
	if got := e.nodeResults(rn.ID); got[len(got)-1] != proto.MemoryRenameOK {
		t.Fatalf("node reports %v", got)
	}
}

// A move that fails is reported as error (the rename is failed and stays held); the node tries it again at its next
// start, and the ok then ends the rename.
func TestRoleRenameAgentErrorRetriedAtStart(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	e := newAgentRenameEnv(t)
	r := e.runWithMemory("implementor", "decisions.md", "- retry\n")
	e.stopRun(r.ID)
	e.synced("implementor", "decisions.md")
	space := filepath.Join(e.agentDir, "memory", e.spaceID)
	if err := os.Chmod(space, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(space, 0o700) })
	rn := e.rename("implementor", "builder")
	eventually(t, "failed", func() bool { return e.renameState(rn.ID).NodeState == catalog.RenameNodeFailed })
	if st := e.renameState(rn.ID); !strings.Contains(st.NodeMessage, "move the folder") {
		t.Fatalf("failed rename %+v", st)
	}
	if !e.launchHeld("builder") || !e.launchHeld("implementor") {
		t.Fatal("a failed rename does not hold launches")
	}
	if err := os.Chmod(space, 0o700); err != nil {
		t.Fatal(err)
	}
	e.stopAgent()
	e.startAgent()
	e.done(rn, map[string]string{"decisions.md": "- retry\n"}, nil)
	if got := e.nodeResults(rn.ID); !slices.Equal(got, []string{proto.MemoryRenameError, proto.MemoryRenameOK}) {
		t.Fatalf("node reports %v", got)
	}
}
