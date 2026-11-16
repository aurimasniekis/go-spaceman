package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/catalog"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store"
)

// renameNodeEnv is a central with one enrolled node, a space with the starter roles, and helpers to drive a role
// rename's node half with a scripted node connection (ADR-0093 §3.3, protocol minor 14).
type renameNodeEnv struct {
	*fleetEnv
	ctx     context.Context
	nodeID  string
	spaceID string
}

func newRenameNodeEnv(t *testing.T) *renameNodeEnv {
	t.Helper()
	e := newFleetEnv(t)
	e.enroll()
	ctx := context.Background()
	nodes, err := e.c.Fleet.ListNodes(ctx)
	if err != nil || len(nodes) != 1 {
		t.Fatalf("nodes: %v %v", nodes, err)
	}
	sp, err := e.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "payments", Name: "Payments", NodeID: nodes[0].ID, StarterRoles: true})
	if err != nil {
		t.Fatal(err)
	}
	return &renameNodeEnv{fleetEnv: e, ctx: ctx, nodeID: nodes[0].ID, spaceID: sp.ID}
}

// connect dials the node at maxMinor and waits until central has recorded the connection and granted the lease.
func (e *renameNodeEnv) connect(maxMinor int) (*rawNode, int64) {
	e.t.Helper()
	n := dialRawNode(e.t, e.fleetEnv, maxMinor)
	n.next(e.t, proto.TypeWelcome)
	var g proto.LeaseGrant
	if err := n.next(e.t, proto.TypeLeaseGrant).Decode(&g); err != nil {
		e.t.Fatal(err)
	}
	return n, g.Generation
}

func (e *renameNodeEnv) rename(from, to string) (*catalog.RoleRename, error) {
	_, rn, err := e.c.Catalog.RenameSpaceRole(e.ctx, admin, e.spaceID, from, to, 0)
	return rn, err
}

func (e *renameNodeEnv) state(id string) catalog.RoleRename {
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

// held reports whether a launch of role is held by a rename (fleet.RenamePendingError); any other outcome is not held.
func (e *renameNodeEnv) held(role string) *fleet.RenamePendingError {
	e.t.Helper()
	_, err := e.c.Fleet.LaunchRun(e.ctx, admin, e.spaceID, fleet.LaunchInput{Kind: "worker", Role: role, Harness: "claude-code", Prompt: "go"})
	var pe *fleet.RenamePendingError
	if errors.As(err, &pe) {
		return pe
	}
	return nil
}

// memoryRename reads commands until a memory.rename arrives; ack (unless "") is the node's answer to it.
func (n *rawNode) memoryRename(t *testing.T, ack string) (proto.Command, proto.MemoryRename) {
	t.Helper()
	for {
		var c proto.Command
		if err := n.next(t, proto.TypeCommand).Decode(&c); err != nil {
			t.Fatal(err)
		}
		if c.Kind != proto.CmdMemoryRename {
			n.send(t, proto.TypeAck, proto.Ack{CommandID: c.CommandID, Result: proto.AckAccepted})
			continue
		}
		if err := c.Validate(); err != nil {
			t.Fatalf("the command does not validate: %v", err)
		}
		var p proto.MemoryRename
		if err := json.Unmarshal(c.Payload, &p); err != nil {
			t.Fatal(err)
		}
		if err := p.Validate(); err != nil {
			t.Fatalf("the payload does not validate: %v", err)
		}
		if ack != "" {
			n.send(t, proto.TypeAck, proto.Ack{CommandID: c.CommandID, Result: ack, Reason: "no valid lease for this space and generation"})
		}
		return c, p
	}
}

// noMemoryRename fails when a memory.rename arrives within d.
func (n *rawNode) noMemoryRename(t *testing.T, d time.Duration) {
	t.Helper()
	deadline := time.After(d)
	for {
		select {
		case env, ok := <-n.ch:
			if !ok {
				return
			}
			var c proto.Command
			if env.Type == proto.TypeCommand && env.Decode(&c) == nil && c.Kind == proto.CmdMemoryRename {
				t.Fatalf("memory.rename delivered: %+v", c)
			}
		case <-deadline:
			return
		}
	}
}

func (n *rawNode) renamed(t *testing.T, id, spaceID, result, msg string) proto.MemoryRenamedAck {
	t.Helper()
	n.send(t, proto.TypeMemoryRenamed, proto.MemoryRenamed{RenameID: id, SpaceID: spaceID, Result: result, Message: msg})
	var ack proto.MemoryRenamedAck
	if err := n.next(t, proto.TypeMemoryRenamedAck).Decode(&ack); err != nil {
		t.Fatal(err)
	}
	if ack.RenameID != id || ack.Result != result {
		t.Fatalf("ack %+v for %s %s", ack, id, result)
	}
	return ack
}

// ADR-0093 T-H2 (simulated: a scripted node connection): a rename on a space whose node speaks minor 14 queues one
// memory.rename, delivered at the space's lease generation. The node's reports move the rename explicitly: deferred
// keeps it pending, error makes it failed, ok makes it done (final). Pending and failed hold launches of the role and
// refuse a relocation; done lifts the hold. Every report is acknowledged; a repeated one changes nothing, as does one
// after done, and an unknown rename is acknowledged with an error so the node stops sending it.
func TestRoleRenameNodeMoveStateMachine(t *testing.T) {
	t.Parallel()
	e := newRenameNodeEnv(t)
	n, gen := e.connect(proto.MaxMinor)

	rn, err := e.rename("reviewer", "critic")
	if err != nil {
		t.Fatal(err)
	}
	if rn.NodeState != catalog.RenameNodePending || rn.NodeID != e.nodeID {
		t.Fatalf("rename %+v", rn)
	}
	c, p := n.memoryRename(t, proto.AckAccepted)
	if c.Generation != gen || c.SpaceID != e.spaceID || c.IdempotencyKey != catalog.RenameCommandKey(rn.ID) ||
		p != (proto.MemoryRename{RenameID: rn.ID, SpaceID: e.spaceID, From: "reviewer", To: "critic"}) {
		t.Fatalf("command %+v payload %+v (lease generation %d)", c, p, gen)
	}
	if pe := e.held("critic"); pe == nil || pe.RenameID != rn.ID || pe.Failed {
		t.Fatalf("pending: launch not held: %+v", pe)
	}

	// deferred: still pending, with the node's reason.
	if ack := n.renamed(t, rn.ID, e.spaceID, proto.MemoryRenameDeferred, "a run of the role is still active"); ack.Error != "" {
		t.Fatalf("deferred ack %+v", ack)
	}
	if s := e.state(rn.ID); s.NodeState != catalog.RenameNodePending || s.NodeMessage != "a run of the role is still active" || s.NodeDoneAt != nil {
		t.Fatalf("after deferred: %+v", s)
	}
	// error: failed, still held; the node retries at its next start.
	n.renamed(t, rn.ID, e.spaceID, proto.MemoryRenameError, "rename: permission denied")
	if s := e.state(rn.ID); s.NodeState != catalog.RenameNodeFailed || s.NodeMessage != "rename: permission denied" {
		t.Fatalf("after error: %+v", s)
	}
	pe := e.held("critic")
	if pe == nil || !pe.Failed {
		t.Fatalf("failed: launch not held: %+v", pe)
	}
	if pe := e.held("reviewer"); pe == nil {
		t.Fatal("failed: a launch of the old name is not held")
	}
	// An open rename refuses a relocation of the space.
	_, err = e.c.Fleet.StartRelocation(e.ctx, admin, e.spaceID, fleet.StartRelocationInput{DestinationNodeID: "nod_elsewhere", Precheck: true})
	if apperr.CodeOf(err) != apperr.CodePrecondition || !strings.Contains(err.Error(), "has not moved its memory yet") {
		t.Fatalf("relocation during an open rename: %v", err)
	}
	// ok: done, the hold lifts.
	n.renamed(t, rn.ID, e.spaceID, proto.MemoryRenameOK, "")
	done := e.state(rn.ID)
	if done.NodeState != catalog.RenameNodeDone || done.NodeDoneAt == nil || done.NodeMessage != "" {
		t.Fatalf("after ok: %+v", done)
	}
	if pe := e.held("critic"); pe != nil {
		t.Fatalf("done: launch still held: %+v", pe)
	}
	// A repeated ok and a late error change nothing.
	n.renamed(t, rn.ID, e.spaceID, proto.MemoryRenameOK, "")
	n.renamed(t, rn.ID, e.spaceID, proto.MemoryRenameError, "late")
	if s := e.state(rn.ID); s.NodeState != catalog.RenameNodeDone || !s.NodeDoneAt.Equal(*done.NodeDoneAt) || s.NodeMessage != "" {
		t.Fatalf("after repeats: %+v", s)
	}
	// Unknown renames are acknowledged with an error, so the node stops sending them.
	if ack := n.renamed(t, "rrn_00000000000000000000000000", e.spaceID, proto.MemoryRenameOK, ""); ack.Error == "" {
		t.Fatalf("unknown rename: %+v", ack)
	}
}

// A memory.rename the node refused is recorded on the rename and queued again on the node's next connection (with a new
// key); a connection below minor 14 is never sent one, and the rename waits for a connection that can take it. A
// rename on a space whose node last connected below minor 14 is refused (node_too_old).
func TestRoleRenameCommandRedeliveryAndMinor(t *testing.T) {
	t.Parallel()
	e := newRenameNodeEnv(t)
	n, _ := e.connect(proto.MaxMinor)
	rn, err := e.rename("planner", "architect")
	if err != nil {
		t.Fatal(err)
	}
	n.memoryRename(t, proto.AckRejected)
	eventually(t, "the refusal recorded", func() bool { return strings.Contains(e.state(rn.ID).NodeMessage, "the node refused the command") })
	if s := e.state(rn.ID); s.NodeState != catalog.RenameNodePending {
		t.Fatalf("after a refusal: %+v", s)
	}
	_ = n.conn.Close(websocket.StatusNormalClosure, "")
	eventually(t, "node disconnected", func() bool { nd, _ := e.c.Fleet.GetNode(e.ctx, e.nodeID); return !nd.Connected })

	// A connection at minor 13 gets no memory.rename; the rename stays pending and still holds the role.
	old, _ := e.connect(proto.MinorRoleRename - 1)
	old.noMemoryRename(t, 1500*time.Millisecond)
	if s := e.state(rn.ID); s.NodeState != catalog.RenameNodePending {
		t.Fatalf("on an old connection: %+v", s)
	}
	if e.held("architect") == nil {
		t.Fatal("the role is not held")
	}
	// The node now last spoke minor 13: another rename is refused before anything changes.
	if _, err := e.rename("implementor", "builder"); apperr.CodeOf(err) != apperr.CodePrecondition || !strings.Contains(err.Error(), catalog.RenameNodeTooOld) {
		t.Fatalf("rename on an old node: %v", err)
	}
	if roles, err := e.c.Catalog.ListSpaceRoles(e.ctx, e.spaceID); err != nil || !slices.ContainsFunc(roles, func(r *catalog.SpaceRole) bool { return r.Name == "implementor" }) {
		t.Fatalf("the refused rename changed the role: %v", err)
	}
	_ = old.conn.Close(websocket.StatusNormalClosure, "")
	eventually(t, "old node disconnected", func() bool { nd, _ := e.c.Fleet.GetNode(e.ctx, e.nodeID); return !nd.Connected })

	// Back at minor 14: the command goes out again, under a new key, and the node's ok ends the rename.
	n, gen := e.connect(proto.MaxMinor)
	c, p := n.memoryRename(t, proto.AckAccepted)
	if c.IdempotencyKey != catalog.RenameCommandKey(rn.ID)+":1" || c.Generation != gen || p.RenameID != rn.ID {
		t.Fatalf("re-queued command %+v %+v (generation %d)", c, p, gen)
	}
	n.renamed(t, rn.ID, e.spaceID, proto.MemoryRenameOK, "")
	if s := e.state(rn.ID); s.NodeState != catalog.RenameNodeDone {
		t.Fatalf("after ok: %+v", s)
	}
	if e.held("architect") != nil {
		t.Fatal("still held after the move")
	}
}

// A rename while the node is away commits at once; its memory.rename is delivered when the node connects, at the lease
// generation of that connection.
func TestRoleRenameWhileNodeOffline(t *testing.T) {
	t.Parallel()
	e := newRenameNodeEnv(t)
	n, first := e.connect(proto.MaxMinor) // records minor 14
	_ = n.conn.Close(websocket.StatusNormalClosure, "")
	eventually(t, "node disconnected", func() bool { nd, _ := e.c.Fleet.GetNode(e.ctx, e.nodeID); return !nd.Connected })
	rn, err := e.rename("reviewer", "critic")
	if err != nil || rn.NodeState != catalog.RenameNodePending {
		t.Fatalf("offline rename: %+v %v", rn, err)
	}
	// The lease is revoked meanwhile (as a pause or a relocation away and back would), so the next grant has a newer
	// generation than the one the command was queued with: it goes out at the new one, or the node would refuse it.
	if err := e.c.Store.W().TxNamed(e.ctx, "test.revoke", func(tx *store.Tx) error {
		_, err := tx.SQL.ExecContext(e.ctx, `UPDATE execution_leases SET state = 'revoked' WHERE space_id = ?`, e.spaceID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	n, gen := e.connect(proto.MaxMinor)
	if gen <= first {
		t.Fatalf("the lease was granted again at generation %d, not after %d", gen, first)
	}
	c, p := n.memoryRename(t, proto.AckAccepted)
	if c.Generation != gen || p.RenameID != rn.ID || c.IdempotencyKey != catalog.RenameCommandKey(rn.ID) {
		t.Fatalf("command %+v %+v (generation %d)", c, p, gen)
	}
	n.renamed(t, rn.ID, e.spaceID, proto.MemoryRenameOK, "")
	if s := e.state(rn.ID); s.NodeState != catalog.RenameNodeDone {
		t.Fatalf("after ok: %+v", s)
	}
}
