package app_test

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	hns "github.com/aurimasniekis/go-spaceman/internal/harness"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// ADR-0093 over HTTP: renaming a role needs space configuration write (a viewer is denied, an operator is allowed).
// Its refusals keep the documented contract: an invalid or same name is 422 validation_failed, a stale If-Match is
// 409 precondition_failed, a busy role is 409 role_busy, a taken name is 409 name_taken, a node below protocol minor 14
// is 409 node_too_old, a missing role is 404. A
// renamed-away name is 404 and names its successor; the renames list is newest first.
func TestRoleRenameOverHTTP(t *testing.T) {
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
	base := "/spaces/" + sp.ID

	viewer := h.client()
	viewer.bearer = userToken(t, h.c.Auth, "vera@example.com", []auth.GrantInput{{Role: authz.RoleViewer, Domain: sp.ID}})
	op := h.client()
	op.bearer = userToken(t, h.c.Auth, "oscar@example.com", []auth.GrantInput{{Role: authz.RoleOperator, Domain: sp.ID}})
	rename := func(c *client, from, to string, hdr ...string) resp {
		return c.do("POST", base+"/roles/"+from+"/rename", map[string]any{"new_name": to}, hdr...)
	}
	revisionOf := func(name string) string {
		var r struct {
			Revision int64 `json:"revision"`
		}
		root.do("GET", base+"/roles/"+name, nil).json(t, &r)
		return `"` + strconv.FormatInt(r.Revision, 10) + `"`
	}

	// Authorization: a viewer is denied, and an operator (space configuration write) is allowed.
	expect(t, rename(viewer, "reviewer", "reviewer-2"), 403)
	expect(t, rename(op, "nobody", "nobody-2"), 404)
	p := rename(op, "reviewer", "Bad Name").problem(t)
	if p.Status != 422 || p.Code != "validation_failed" {
		t.Fatalf("invalid name: %+v", p)
	}
	p = rename(op, "reviewer", "reviewer").problem(t)
	if p.Status != 422 || p.Code != "validation_failed" {
		t.Fatalf("same name: %+v", p)
	}
	p = rename(op, "reviewer", "reviewer-2", "If-Match", `"99"`).problem(t)
	if p.Status != 409 || p.Code != "precondition_failed" {
		t.Fatalf("stale If-Match: %+v", p)
	}
	// A busy role (a live run of implementor) is refused and changes nothing. A launch needs a connected node, so the
	// live run (observed pending, as InsertRun writes it) is written directly.
	if err := h.c.Store.W().TxNamed(ctx, "test.busy_run", func(tx *store.Tx) error {
		now := h.clk.Now().UnixMicro()
		return tx.Q.InsertRun(ctx, db.InsertRunParams{ID: "run_busy", SpaceID: sp.ID, NodeID: en.Node.ID, Generation: 1, Kind: "worker",
			Role: "implementor", Harness: hns.KindClaude, InstanceID: "ins_busy", SessionID: "ses_busy", LaunchJson: "{}", ManifestJson: "{}",
			CreatedAt: now, CreatedBy: "usr_test", UpdatedAt: now})
	}); err != nil {
		t.Fatal(err)
	}
	p = rename(op, "implementor", "implementor-2").problem(t)
	if p.Status != 409 || p.Code != "role_busy" || !strings.Contains(p.Detail, "run run_busy") {
		t.Fatalf("busy role: %+v", p)
	}
	expect(t, root.do("GET", base+"/roles/implementor", nil), 200)
	// A taken name (a role of the space already has it) is 409 name_taken.
	p = rename(op, "reviewer", "implementor").problem(t)
	if p.Status != 409 || p.Code != "name_taken" {
		t.Fatalf("taken name: %+v", p)
	}

	// A node that has not spoken protocol minor 14 (this one never connected) cannot move the role's memory: 409
	// node_too_old, and nothing changes. Once it has (the minor its connect records), the rename goes ahead.
	p = rename(op, "reviewer", "reviewer-2").problem(t)
	if p.Status != 409 || p.Code != "node_too_old" || !strings.Contains(p.Detail, "node-a") {
		t.Fatalf("old node: %+v", p)
	}
	expect(t, root.do("GET", base+"/roles/reviewer", nil), 200)
	if err := h.c.Store.W().TxNamed(ctx, "test.node_minor", func(tx *store.Tx) error {
		_, err := tx.SQL.ExecContext(ctx, `UPDATE nodes SET protocol_minor = ? WHERE id = ?`, proto.MinorRoleRename, en.Node.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	// The rename: 200 with the renamed role and its rename record, pending until the node moves the memory.
	var renamed struct {
		Name   string `json:"name"`
		Rename struct {
			From      string `json:"from"`
			To        string `json:"to"`
			NodeState string `json:"node_state"`
		} `json:"rename"`
	}
	r = rename(op, "reviewer", "reviewer-2", "If-Match", revisionOf("reviewer"))
	expect(t, r, 200)
	r.json(t, &renamed)
	if renamed.Name != "reviewer-2" || renamed.Rename.From != "reviewer" || renamed.Rename.To != "reviewer-2" || renamed.Rename.NodeState != "pending" {
		t.Fatalf("renamed %+v", renamed)
	}
	// The old name is 404 and names its successor; the new name resolves.
	p = root.do("GET", base+"/roles/reviewer", nil).problem(t)
	if p.Status != 404 || !strings.Contains(p.Detail, "renamed to reviewer-2") {
		t.Fatalf("old name: %+v", p)
	}
	expect(t, root.do("GET", base+"/roles/reviewer-2", nil), 200)

	// A second rename (no If-Match needed) lists newest first.
	expect(t, rename(op, "planner", "planner-2"), 200)
	var list struct {
		Items []struct {
			From string `json:"from"`
			To   string `json:"to"`
		} `json:"items"`
	}
	r = viewer.do("GET", base+"/role-renames", nil)
	expect(t, r, 200)
	r.json(t, &list)
	if len(list.Items) != 2 || list.Items[0].From != "planner" || list.Items[1].From != "reviewer" {
		t.Fatalf("renames, newest first: %+v", list.Items)
	}
}
