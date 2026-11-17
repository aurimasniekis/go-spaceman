package app_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/coder/websocket"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/catalog"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store"
)

// ADR-0093 addendum, T-H4 (simulated: a scripted node connection). Retry of a failed rename sends the node a new
// memory.rename (a new key); the node's ok then ends it as for any rename.
func TestRoleRenameRetryResendsCommand(t *testing.T) {
	t.Parallel()
	e := newRenameNodeEnv(t)
	n, _ := e.connect(proto.MaxMinor)
	rn, err := e.rename("reviewer", "critic")
	if err != nil {
		t.Fatal(err)
	}
	n.memoryRename(t, proto.AckAccepted)
	n.renamed(t, rn.ID, e.spaceID, proto.MemoryRenameError, "rename: permission denied")
	if s := e.state(rn.ID); s.NodeState != catalog.RenameNodeFailed {
		t.Fatalf("not failed: %+v", s)
	}

	got, err := e.c.Catalog.RetryRoleRename(e.ctx, admin, e.spaceID, rn.ID)
	if err != nil || !got.Queued || got.NodeTooOld || got.Rename.NodeState != catalog.RenameNodePending {
		t.Fatalf("retry: %+v %v", got, err)
	}
	c, p := n.memoryRename(t, proto.AckAccepted)
	if c.IdempotencyKey != catalog.RenameCommandKey(rn.ID)+":1" || p.RenameID != rn.ID {
		t.Fatalf("re-sent command %+v %+v", c, p)
	}
	n.renamed(t, rn.ID, e.spaceID, proto.MemoryRenameOK, "")
	if s := e.state(rn.ID); s.NodeState != catalog.RenameNodeDone {
		t.Fatalf("after the retry's ok: %+v", s)
	}
	// Done is not open: a retry or a close is refused.
	if _, err := e.c.Catalog.RetryRoleRename(e.ctx, admin, e.spaceID, rn.ID); apperr.CodeOf(err) != apperr.CodePrecondition {
		t.Errorf("retry of a done rename: %v", err)
	}
}

// Close of a failed rename: the connected node is told at once (memory.renamed.ack with error closed), launches of both
// names and a relocation are no longer held, and a late ok from the node is answered closed and changes nothing.
func TestRoleRenameCloseLiftsHoldsAndTellsNode(t *testing.T) {
	t.Parallel()
	e := newRenameNodeEnv(t)
	n, _ := e.connect(proto.MaxMinor)
	rn, err := e.rename("reviewer", "critic")
	if err != nil {
		t.Fatal(err)
	}
	n.memoryRename(t, proto.AckAccepted)
	n.renamed(t, rn.ID, e.spaceID, proto.MemoryRenameError, "rename: permission denied")
	if e.held("critic") == nil || e.held("reviewer") == nil {
		t.Fatal("a failed rename does not hold launches")
	}

	got, err := e.c.Catalog.CloseRoleRename(e.ctx, admin, e.spaceID, rn.ID, "node retired", false)
	if err != nil || got.NodeState != catalog.RenameNodeAbandoned {
		t.Fatalf("close: %+v %v", got, err)
	}
	var ack proto.MemoryRenamedAck
	if err := n.next(t, proto.TypeMemoryRenamedAck).Decode(&ack); err != nil {
		t.Fatal(err)
	}
	if ack.RenameID != rn.ID || ack.Error != proto.MemoryRenameClosed {
		t.Fatalf("the node was told %+v", ack)
	}
	if e.held("critic") != nil || e.held("reviewer") != nil {
		t.Fatal("a closed rename still holds launches")
	}
	_, err = e.c.Fleet.StartRelocation(e.ctx, admin, e.spaceID, fleet.StartRelocationInput{DestinationNodeID: "nod_elsewhere", Precheck: true})
	if err != nil && strings.Contains(err.Error(), "has not moved its memory yet") {
		t.Fatalf("a closed rename still refuses a relocation: %v", err)
	}
	// A late ok (the node moved after all) is answered closed and changes nothing; so does a repeated error.
	n.send(t, proto.TypeMemoryRenamed, proto.MemoryRenamed{RenameID: rn.ID, SpaceID: e.spaceID, Result: proto.MemoryRenameOK})
	var late proto.MemoryRenamedAck // a fresh value: omitted fields would keep the earlier ack's
	if err := n.next(t, proto.TypeMemoryRenamedAck).Decode(&late); err != nil || late.Error != proto.MemoryRenameClosed || late.Result != proto.MemoryRenameOK {
		t.Fatalf("late ok answered %+v %v", late, err)
	}
	if s := e.state(rn.ID); s.NodeState != catalog.RenameNodeAbandoned || s.NodeDoneAt == nil || !strings.Contains(s.NodeMessage, "node retired") {
		t.Fatalf("after the late ok: %+v", s)
	}
}

// Over HTTP: retry and close need the admin-only space_role_rename:admin (a viewer and an operator are 403, an admin may),
// an unknown rename is 404, one that is not open is 409 rename_not_open, and a pending rename whose node is too old can
// be retried (node_too_old) and closed.
func TestRoleRenameRetryCloseOverHTTP(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	secret, _, _ := h.c.Auth.IssueRootToken(ctx)
	root := h.client()
	root.bearer = secret
	en, err := h.c.Fleet.CreateNode(ctx, admin, fleet.CreateNodeInput{Name: "node-a"})
	if err != nil {
		t.Fatal(err)
	}
	var sp struct{ ID string }
	r := root.do("POST", "/spaces", map[string]any{"slug": "payments", "name": "Payments", "node_id": en.Node.ID, "starter_roles": true})
	expect(t, r, 201)
	r.json(t, &sp)
	viewer, op := h.client(), h.client()
	viewer.bearer = userToken(t, h.c.Auth, "vera@example.com", []auth.GrantInput{{Role: authz.RoleViewer, Domain: sp.ID}})
	op.bearer = userToken(t, h.c.Auth, "oscar@example.com", []auth.GrantInput{{Role: authz.RoleOperator, Domain: sp.ID}})
	setMinor := func(minor int) {
		t.Helper()
		if err := h.c.Store.W().TxNamed(ctx, "test.minor", func(tx *store.Tx) error {
			_, err := tx.SQL.ExecContext(ctx, `UPDATE nodes SET protocol_minor = ? WHERE id = ?`, minor, en.Node.ID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	setMinor(proto.MinorRoleRename)
	var renamed struct {
		Rename struct {
			ID string `json:"id"`
		} `json:"rename"`
	}
	r = op.do("POST", "/spaces/"+sp.ID+"/roles/reviewer/rename", map[string]any{"new_name": "critic"})
	expect(t, r, 200)
	r.json(t, &renamed)
	id := renamed.Rename.ID
	setMinor(proto.MinorRoleRename - 1) // the node is downgraded after the rename: pending, and too old
	state := func() string {
		l, err := h.c.Catalog.ListRoleRenames(ctx, sp.ID)
		if err != nil || len(l) != 1 {
			t.Fatalf("renames %v %v", l, err)
		}
		return l[0].NodeState
	}

	base := "/spaces/" + sp.ID + "/role-renames/"
	for _, c := range []*client{viewer, op} {
		expect(t, c.do("POST", base+id+"/retry", nil), 403)
		expect(t, c.do("POST", base+id+"/close", map[string]any{}), 403)
		expect(t, c.do("POST", base+id+"/close", map[string]any{"forget_node": true}), 403)
	}
	if state() != catalog.RenameNodePending {
		t.Fatal("a refused call changed the rename")
	}
	expect(t, root.do("POST", base+"rrn_00000000000000000000000000/retry", nil), 404)
	expect(t, root.do("POST", base+"rrn_00000000000000000000000000/close", map[string]any{}), 404)

	var retried struct {
		Queued     bool `json:"queued"`
		NodeTooOld bool `json:"node_too_old"`
		Rename     struct {
			NodeState string `json:"node_state"`
		} `json:"rename"`
	}
	r = root.do("POST", base+id+"/retry", nil)
	expect(t, r, 200)
	r.json(t, &retried)
	if !retried.NodeTooOld || retried.Rename.NodeState != "pending" {
		t.Fatalf("retry on a too-old node: %+v", retried)
	}

	var closed struct {
		NodeState   string `json:"node_state"`
		NodeMessage string `json:"node_message"`
	}
	r = root.do("POST", base+id+"/close", map[string]any{"reason": "node too old to upgrade"})
	expect(t, r, 200)
	r.json(t, &closed)
	if closed.NodeState != "abandoned" || !strings.Contains(closed.NodeMessage, "node too old to upgrade") {
		t.Fatalf("close: %+v", closed)
	}
	for _, path := range []string{"/retry", "/close"} {
		p := root.do("POST", base+id+path, map[string]any{}).problem(t)
		if p.Status != 409 || p.Code != "rename_not_open" {
			t.Fatalf("%s of a closed rename: %+v", path, p)
		}
	}
	// The node never took the command: forget_node is still accepted on the closed rename, once.
	expect(t, root.do("POST", base+id+"/close", map[string]any{"forget_node": true}), 200)
	if p := root.do("POST", base+id+"/close", map[string]any{"forget_node": true}).problem(t); p.Status != 409 || p.Code != "rename_not_open" {
		t.Fatalf("a second forget: %+v", p)
	}
	_, err = h.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Kind: "worker", Role: "critic", Harness: "claude-code", Prompt: "go"})
	var pe *fleet.RenamePendingError
	if errors.As(err, &pe) {
		t.Fatalf("still held after the close: %v", err)
	}
}

// ADR-0093 addendum, ordering (simulated: a scripted node connection). A rename closed while the node is offline keeps
// its command queued and keeps blocking renames of both names until the node journals it: a later rename of either
// name must not reach the node first. The node's accept of the closed rename's command releases the names.
func TestRoleRenameClosedOfflineBlocksUntilDelivered(t *testing.T) {
	t.Parallel()
	e := newRenameNodeEnv(t)
	n, _ := e.connect(proto.MaxMinor) // records minor 14
	_ = n.conn.Close(websocket.StatusNormalClosure, "")
	eventually(t, "node disconnected", func() bool { nd, _ := e.c.Fleet.GetNode(e.ctx, e.nodeID); return !nd.Connected })
	rn, err := e.rename("reviewer", "critic")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.c.Catalog.CloseRoleRename(e.ctx, admin, e.spaceID, rn.ID, "node offline", false); err != nil {
		t.Fatal(err)
	}
	var rr *catalog.RenameRefused
	if _, err := e.rename("critic", "judge"); !errors.As(err, &rr) || rr.Reason != catalog.RenameRoleBusy || !strings.Contains(err.Error(), rn.ID) {
		t.Fatalf("a rename of the closed rename's new name before the node has it: %v", err)
	}
	n, _ = e.connect(proto.MaxMinor)
	c, p := n.memoryRename(t, proto.AckAccepted)
	if p.RenameID != rn.ID || p.Closed || c.IdempotencyKey != catalog.RenameCommandKey(rn.ID) {
		t.Fatalf("delivered %+v %+v", c, p)
	}
	eventually(t, "names released", func() bool {
		_, err := e.rename("critic", "judge")
		return err == nil
	})
}

// ADR-0093 addendum, forget (simulated). Close with forget_node of a rename whose node is offline cancels its command and
// releases both names at once. When the node comes back it is sent the closed notice first (a memory.rename with closed
// set, under its own key), then the later rename of the new name: never the cancelled move.
func TestRoleRenameForgetNodeReleasesNames(t *testing.T) {
	t.Parallel()
	e := newRenameNodeEnv(t)
	n, _ := e.connect(proto.MaxMinor)
	_ = n.conn.Close(websocket.StatusNormalClosure, "")
	eventually(t, "node disconnected", func() bool { nd, _ := e.c.Fleet.GetNode(e.ctx, e.nodeID); return !nd.Connected })
	rn, err := e.rename("reviewer", "critic")
	if err != nil {
		t.Fatal(err)
	}
	got, err := e.c.Catalog.CloseRoleRename(e.ctx, admin, e.spaceID, rn.ID, "node retired", true)
	if err != nil || got.NodeState != catalog.RenameNodeAbandoned {
		t.Fatalf("forget: %+v %v", got, err)
	}
	later, err := e.rename("critic", "judge")
	if err != nil {
		t.Fatalf("a rename of the new name after the forget: %v", err)
	}
	n, _ = e.connect(proto.MaxMinor)
	c, p := n.memoryRename(t, proto.AckAccepted)
	if !p.Closed || p.RenameID != rn.ID || c.IdempotencyKey != catalog.RenameClosedKey(rn.ID) {
		t.Fatalf("first command %+v %+v, want the closed notice", c, p)
	}
	c, p = n.memoryRename(t, proto.AckAccepted)
	if p.Closed || p.RenameID != later.ID || p.From != "critic" || p.To != "judge" {
		t.Fatalf("second command %+v %+v, want the later rename", c, p)
	}
}

// A closed rename whose command the node refused (no valid lease at that moment) is queued again at the node's next
// connect, like an open one, so the node still journals it; until then it blocks both names.
func TestRoleRenameClosedRejectedIsRequeued(t *testing.T) {
	t.Parallel()
	e := newRenameNodeEnv(t)
	n, _ := e.connect(proto.MaxMinor)
	rn, err := e.rename("reviewer", "critic")
	if err != nil {
		t.Fatal(err)
	}
	n.memoryRename(t, proto.AckRejected)
	eventually(t, "command rejected", func() bool { return strings.Contains(e.state(rn.ID).NodeMessage, "refused") })
	_ = n.conn.Close(websocket.StatusNormalClosure, "")
	eventually(t, "node disconnected", func() bool { nd, _ := e.c.Fleet.GetNode(e.ctx, e.nodeID); return !nd.Connected })
	if _, err := e.c.Catalog.CloseRoleRename(e.ctx, admin, e.spaceID, rn.ID, "", false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.rename("critic", "judge"); err == nil {
		t.Fatal("a closed rename the node refused does not block its names")
	}
	n, _ = e.connect(proto.MaxMinor)
	c, p := n.memoryRename(t, proto.AckAccepted)
	if p.RenameID != rn.ID || p.Closed || c.IdempotencyKey != catalog.RenameCommandKey(rn.ID)+":1" {
		t.Fatalf("re-queued %+v %+v", c, p)
	}
}

// ADR-0093 addendum 3 (simulated): a rename closed while the node is offline is still delivered when the node connects;
// the node moves the folder and reports ok, and central answers that report with the closed reason (harmless: it drops the
// node's rename and sync hold) while the rename stays abandoned.
func TestRoleRenameClosedOfflineOkIsAnsweredClosed(t *testing.T) {
	t.Parallel()
	e := newRenameNodeEnv(t)
	n, _ := e.connect(proto.MaxMinor)
	_ = n.conn.Close(websocket.StatusNormalClosure, "")
	eventually(t, "node disconnected", func() bool { nd, _ := e.c.Fleet.GetNode(e.ctx, e.nodeID); return !nd.Connected })
	rn, err := e.rename("reviewer", "critic")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.c.Catalog.CloseRoleRename(e.ctx, admin, e.spaceID, rn.ID, "node offline", false); err != nil {
		t.Fatal(err)
	}
	n, _ = e.connect(proto.MaxMinor)
	if _, p := n.memoryRename(t, proto.AckAccepted); p.RenameID != rn.ID || p.Closed {
		t.Fatalf("delivered %+v", p)
	}
	n.send(t, proto.TypeMemoryRenamed, proto.MemoryRenamed{RenameID: rn.ID, SpaceID: e.spaceID, Result: proto.MemoryRenameOK})
	var ack proto.MemoryRenamedAck
	if err := n.next(t, proto.TypeMemoryRenamedAck).Decode(&ack); err != nil {
		t.Fatal(err)
	}
	if ack.RenameID != rn.ID || ack.Error != "closed" {
		t.Fatalf("ack %+v, want the closed reason", ack)
	}
	if s := e.state(rn.ID); s.NodeState != catalog.RenameNodeAbandoned {
		t.Fatalf("the rename left abandoned: %+v", s)
	}
}
