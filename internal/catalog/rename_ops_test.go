package catalog

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

var admin = audit.Actor{Type: audit.ActorUser, ID: "usr_admin"}

// renameRow reads a rename's stored row.
func (f *fx) renameRow(id string) db.SpaceRoleRename {
	f.t.Helper()
	r, err := f.s.R().GetSpaceRoleRename(f.ctx, id)
	must(f.t, err)
	return r
}

// auditCount counts audit events of an action.
func (f *fx) auditCount(action string) int {
	f.t.Helper()
	var n int
	must(f.t, f.s.ReadDB().QueryRowContext(f.ctx, `SELECT count(*) FROM audit_events WHERE action = ?`, action).Scan(&n))
	return n
}

// stuck renames a→z on a space with a node and sets the rename's state and its first command's state.
func (f *renameFx) stuck(state, cmdState string) string {
	f.t.Helper()
	_, rn, err := f.svc.RenameSpaceRole(f.ctx, op, "spc_a", "a", "z", 0)
	must(f.t, err)
	f.exec(`UPDATE space_role_renames SET node_state = ?, node_message = 'permission denied' WHERE id = ?`, state, rn.ID)
	f.exec(`UPDATE node_commands SET state = ? WHERE kind = 'memory.rename'`, cmdState)
	return rn.ID
}

func commandStates(f *fx) []string {
	f.t.Helper()
	rows, err := f.s.ReadDB().QueryContext(f.ctx, `SELECT idempotency_key || '=' || state FROM node_commands WHERE kind = 'memory.rename' ORDER BY created_at, id`)
	must(f.t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		must(f.t, rows.Scan(&s))
		out = append(out, s)
	}
	must(f.t, rows.Err())
	return out
}

// Retry of a failed rename queues a new command (the node accepted the first and reported the error), makes the rename
// pending again and audits it. Retry of a pending rename whose node is too old queues too and says so; one whose command
// still waits for the node only wakes delivery.
func TestRetryRoleRename(t *testing.T) {
	f := newRenameFx(t)
	var woke []RoleRename
	f.svc.AfterRenameRetry = func(_ context.Context, r RoleRename) { woke = append(woke, r) }

	id := f.stuck(RenameNodeFailed, "accepted")
	got, err := f.svc.RetryRoleRename(f.ctx, admin, "spc_a", id)
	must(t, err)
	if !got.Queued || got.NodeTooOld || got.Rename.NodeState != RenameNodePending {
		t.Fatalf("retry of a failed rename: %+v %+v", got, got.Rename)
	}
	if want := []string{RenameCommandKey(id) + "=accepted", RenameCommandKey(id) + ":1=pending"}; strings.Join(commandStates(f.fx), ",") != strings.Join(want, ",") {
		t.Fatalf("commands %v, want %v", commandStates(f.fx), want)
	}
	if len(woke) != 1 || f.auditCount("role_rename.retried") != 1 {
		t.Fatalf("hook %d, audit %d", len(woke), f.auditCount("role_rename.retried"))
	}
	if f.renameRow(id).NodeMessage != "permission denied" {
		t.Error("retry dropped the node's last message")
	}

	// The retry's command still waits: a second retry only wakes delivery.
	got, err = f.svc.RetryRoleRename(f.ctx, admin, "spc_a", id)
	must(t, err)
	if got.Queued || len(commandStates(f.fx)) != 2 {
		t.Fatalf("second retry queued again: %+v %v", got, commandStates(f.fx))
	}

	// Pending and too old: the command is queued, and the response says the node cannot take it yet.
	f.exec(`UPDATE node_commands SET state = 'accepted'`)
	f.exec(`UPDATE nodes SET protocol_minor = ? WHERE id = 'nod_1'`, proto.MinorRoleRename-1)
	got, err = f.svc.RetryRoleRename(f.ctx, admin, "spc_a", id)
	must(t, err)
	if !got.Queued || !got.NodeTooOld || got.Rename.NodeState != RenameNodePending {
		t.Fatalf("retry on a too-old node: %+v", got)
	}
	if n := len(commandStates(f.fx)); n != 3 {
		t.Fatalf("commands %v", commandStates(f.fx))
	}
}

// Retry and close refuse a rename that is not open (409 rename_not_open), and one that is unknown or another space's
// (404). Nothing changes.
func TestRenameOpsRefusals(t *testing.T) {
	f := newRenameFx(t)
	id := f.stuck(RenameNodePending, "accepted")
	for _, state := range []string{RenameNodeDone, RenameNodeNotNeeded, RenameNodeAbandoned} {
		f.exec(`UPDATE space_role_renames SET node_state = ? WHERE id = ?`, state, id)
		for name, call := range map[string]func() error{
			"retry": func() error { _, err := f.svc.RetryRoleRename(f.ctx, admin, "spc_a", id); return err },
			"close": func() error { _, err := f.svc.CloseRoleRename(f.ctx, admin, "spc_a", id, ""); return err },
		} {
			err := call()
			var rr *RenameRefused
			if apperr.CodeOf(err) != apperr.CodePrecondition || !errors.As(err, &rr) || rr.Reason != RenameNotOpen {
				t.Errorf("%s of a %s rename: %v", name, state, err)
			}
		}
		if f.renameRow(id).NodeState != state {
			t.Errorf("a refused call changed the %s rename", state)
		}
	}
	f.exec(`UPDATE space_role_renames SET node_state = 'pending' WHERE id = ?`, id)
	for name, call := range map[string]func() error{
		"retry unknown": func() error {
			_, err := f.svc.RetryRoleRename(f.ctx, admin, "spc_a", "rrn_00000000000000000000000000")
			return err
		},
		"close other": func() error { _, err := f.svc.CloseRoleRename(f.ctx, admin, "spc_b", id, ""); return err },
		"retry other": func() error { _, err := f.svc.RetryRoleRename(f.ctx, admin, "spc_b", id); return err },
	} {
		if err := call(); apperr.CodeOf(err) != apperr.CodeNotFound {
			t.Errorf("%s: %v", name, err)
		}
	}
	if f.auditCount("role_rename.retried")+f.auditCount("role_rename.closed") != 0 || f.renameRow(id).NodeState != RenameNodePending {
		t.Error("a refused call audited or changed the rename")
	}
	if _, err := f.svc.CloseRoleRename(f.ctx, admin, "spc_a", id, strings.Repeat("x", MaxRenameCloseReason+1)); apperr.CodeOf(err) != apperr.CodeInvalid {
		t.Errorf("a long reason: %v", err)
	}
}

// Close of a failed rename, and of a pending one whose node is too old, gives the move up: the rename is abandoned with
// the reason, its undelivered command is cancelled, the holds lift (another rename of either name is allowed, no open
// rename holds a launch), and the role row keeps the new name.
func TestCloseRoleRename(t *testing.T) {
	for _, c := range []struct {
		name, state, cmd string
		minor            int
	}{
		{"failed", RenameNodeFailed, "accepted", proto.MinorRoleRename},
		{"pending on a too-old node", RenameNodePending, "pending", proto.MinorRoleRename - 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newRenameFx(t)
			var told []RoleRename
			f.svc.AfterRenameClose = func(_ context.Context, r RoleRename) { told = append(told, r) }
			id := f.stuck(c.state, c.cmd)
			f.exec(`UPDATE nodes SET protocol_minor = ? WHERE id = 'nod_1'`, c.minor)
			// Open: another rename of the new name is held, and so is a launch.
			if _, _, err := f.svc.RenameSpaceRole(f.ctx, op, "spc_a", "z", "y", 0); err == nil {
				t.Fatal("an open rename did not block another rename")
			}
			if _, err := f.s.R().OpenRoleRenameHold(f.ctx, db.OpenRoleRenameHoldParams{SpaceID: "spc_a", Role: "z"}); err != nil {
				t.Fatalf("an open rename holds nothing: %v", err)
			}

			got, err := f.svc.CloseRoleRename(f.ctx, admin, "spc_a", id, "node retired")
			must(t, err)
			if got.NodeState != RenameNodeAbandoned || got.NodeDoneAt == nil || !strings.Contains(got.NodeMessage, "node retired") ||
				!strings.Contains(got.NodeMessage, "orphaned: a") {
				t.Fatalf("closed rename: %+v", got)
			}
			if c.cmd == "pending" && strings.Join(commandStates(f.fx), ",") != RenameCommandKey(id)+"=pending" {
				t.Errorf("the undelivered command was cancelled (a node that never journaled the rename must still get it): %v", commandStates(f.fx))
			}
			if c.cmd == "accepted" && strings.Join(commandStates(f.fx), ",") != RenameCommandKey(id)+"=accepted" {
				t.Errorf("a command the node holds was touched: %v", commandStates(f.fx))
			}
			if len(told) != 1 || told[0].ID != id {
				t.Fatalf("AfterRenameClose %+v", told)
			}
			if f.auditCount("role_rename.closed") != 1 {
				t.Errorf("audit events: %d", f.auditCount("role_rename.closed"))
			}
			if _, err := f.s.R().OpenRoleRenameHold(f.ctx, db.OpenRoleRenameHoldParams{SpaceID: "spc_a", Role: "z"}); err == nil {
				t.Error("a closed rename still holds launches")
			}
			if open, err := f.s.R().ListOpenSpaceRoleRenames(f.ctx, "spc_a"); err != nil || len(open) != 0 {
				t.Errorf("open renames after the close: %v %v", open, err)
			}
			f.role("spc_a", "z") // exists under the new name
			if _, err := f.s.R().GetSpaceRole(f.ctx, dbRole("spc_a", "a")); !errors.Is(err, sql.ErrNoRows) {
				t.Errorf("the old name came back: %v", err)
			}
			// The name chain still resolves, and another rename is no longer blocked by the closed one.
			if cur, err := f.svc.CurrentRoleName(f.ctx, "spc_a", "a"); err != nil || cur != "z" {
				t.Errorf("a resolves to %q, %v", cur, err)
			}
			if _, _, err := f.svc.RenameSpaceRole(f.ctx, op, "spc_a", "z", "y", 0); err != nil && !strings.Contains(err.Error(), "node") {
				t.Errorf("rename after the close: %v", err)
			}
			// A second close and a retry are refused.
			if _, err := f.svc.CloseRoleRename(f.ctx, admin, "spc_a", id, ""); apperr.CodeOf(err) != apperr.CodePrecondition {
				t.Errorf("second close: %v", err)
			}
			if _, err := f.svc.RetryRoleRename(f.ctx, admin, "spc_a", id); apperr.CodeOf(err) != apperr.CodePrecondition {
				t.Errorf("retry after close: %v", err)
			}
		})
	}
}
