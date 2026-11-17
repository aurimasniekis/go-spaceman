package app_test

import (
	"context"
	"errors"
	"strings"
	"testing"

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

	got, err := e.c.Catalog.CloseRoleRename(e.ctx, admin, e.spaceID, rn.ID, "node retired")
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
	if err := n.next(t, proto.TypeMemoryRenamedAck).Decode(&ack); err != nil || ack.Error != proto.MemoryRenameClosed || ack.Result != proto.MemoryRenameOK {
		t.Fatalf("late ok answered %+v %v", ack, err)
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
	_, err = h.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Kind: "worker", Role: "critic", Harness: "claude-code", Prompt: "go"})
	var pe *fleet.RenamePendingError
	if errors.As(err, &pe) {
		t.Fatalf("still held after the close: %v", err)
	}
}
