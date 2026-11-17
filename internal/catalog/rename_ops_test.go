package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
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
			"close": func() error { _, err := f.svc.CloseRoleRename(f.ctx, admin, "spc_a", id, "", false); return err },
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
		"close other": func() error { _, err := f.svc.CloseRoleRename(f.ctx, admin, "spc_b", id, "", false); return err },
		"retry other": func() error { _, err := f.svc.RetryRoleRename(f.ctx, admin, "spc_b", id); return err },
	} {
		if err := call(); apperr.CodeOf(err) != apperr.CodeNotFound {
			t.Errorf("%s: %v", name, err)
		}
	}
	if f.auditCount("role_rename.retried")+f.auditCount("role_rename.closed") != 0 || f.renameRow(id).NodeState != RenameNodePending {
		t.Error("a refused call audited or changed the rename")
	}
	if _, err := f.svc.CloseRoleRename(f.ctx, admin, "spc_a", id, strings.Repeat("x", MaxRenameCloseReason+1), false); apperr.CodeOf(err) != apperr.CodeInvalid {
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

			got, err := f.svc.CloseRoleRename(f.ctx, admin, "spc_a", id, "node retired", false)
			must(t, err)
			if got.NodeState != RenameNodeAbandoned || got.NodeDoneAt == nil || !strings.Contains(got.NodeMessage, "node retired") ||
				!strings.Contains(got.NodeMessage, "orphaned: a") {
				t.Fatalf("closed rename: %+v", got)
			}
			if c.cmd == "pending" && strings.Join(commandStates(f.fx), ",") != RenameCommandKey(id)+"=pending,"+RenameClosedKey(id)+"=pending" {
				t.Errorf("the undelivered command was cancelled (a node that never journaled the rename must still get it), or no closed notice is queued: %v", commandStates(f.fx))
			}
			if c.cmd == "accepted" && strings.Join(commandStates(f.fx), ",") != RenameCommandKey(id)+"=accepted,"+RenameClosedKey(id)+"=pending" {
				t.Errorf("a command the node holds was touched, or no durable closed notice is queued for it: %v", commandStates(f.fx))
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
			_, _, err = f.svc.RenameSpaceRole(f.ctx, op, "spc_a", "z", "y", 0)
			var rr *RenameRefused
			switch {
			case c.cmd == "accepted" && err != nil: // the node journaled it: its own order holds
				t.Errorf("rename after the close of a rename the node holds: %v", err)
			case c.cmd == "pending" && (!errors.As(err, &rr) || rr.Reason != RenameRoleBusy || !strings.Contains(err.Error(), "closed rename "+id)):
				t.Errorf("rename after the close of an undelivered rename: %v, want role_busy naming the closed rename", err)
			}
			// A second close and a retry are refused.
			if _, err := f.svc.CloseRoleRename(f.ctx, admin, "spc_a", id, "", false); apperr.CodeOf(err) != apperr.CodePrecondition {
				t.Errorf("second close: %v", err)
			}
			if _, err := f.svc.RetryRoleRename(f.ctx, admin, "spc_a", id); apperr.CodeOf(err) != apperr.CodePrecondition {
				t.Errorf("retry after close: %v", err)
			}
		})
	}
}

// renameErr asserts a rename's outcome: nil, or a refusal with the given reason whose message names want.
func renameErr(t *testing.T, err error, reason, want string) {
	t.Helper()
	var rr *RenameRefused
	switch {
	case reason == "" && err != nil:
		t.Fatalf("rename refused: %v", err)
	case reason != "" && (!errors.As(err, &rr) || rr.Reason != reason || !strings.Contains(err.Error(), want)):
		t.Fatalf("rename: %v, want %s naming %q", err, reason, want)
	}
}

// ADR-0093 addendum, ordering: a closed rename a->z whose command the node has not journaled (pending, sent, or
// rejected and queued again at connect) keeps blocking renames of either name, the old one (a new role a renamed) and
// the new one (z->y), so a later move never reaches the node first. Once the node journals the command (accepted), the
// names are free.
func TestClosedUndeliveredRenameBlocksBothNames(t *testing.T) {
	for _, state := range []string{"pending", "sent", "rejected"} {
		t.Run(state, func(t *testing.T) {
			f := newRenameFx(t)
			id := f.stuck(RenameNodePending, state)
			_, err := f.svc.CloseRoleRename(f.ctx, admin, "spc_a", id, "node offline", false)
			must(t, err)
			_, _, err = f.svc.RenameSpaceRole(f.ctx, op, "spc_a", "z", "y", 0)
			renameErr(t, err, RenameRoleBusy, "closed rename "+id)
			f.exec(`INSERT INTO space_roles (space_id, name, template_id, created_at, created_by, updated_at) VALUES ('spc_a', 'a', 'rtp_1', 1, 't', 1)`)
			_, _, err = f.svc.RenameSpaceRole(f.ctx, op, "spc_a", "a", "x", 0)
			renameErr(t, err, RenameRoleBusy, "closed rename "+id)
			// A rename onto the closed rename's old name (x->a) or its new name (x->z) is held as well.
			f.exec(`INSERT INTO space_roles (space_id, name, template_id, created_at, created_by, updated_at) VALUES ('spc_a', 'x', 'rtp_1', 1, 't', 1)`)
			f.exec(`DELETE FROM space_roles WHERE space_id = 'spc_a' AND name = 'a'`) // so only the closed rename can refuse x->a
			for _, to := range []string{"a", "z"} {
				_, _, err = f.svc.RenameSpaceRole(f.ctx, op, "spc_a", "x", to, 0)
				renameErr(t, err, RenameRoleBusy, "closed rename "+id)
			}
			f.exec(`INSERT INTO space_roles (space_id, name, template_id, created_at, created_by, updated_at) VALUES ('spc_a', 'a', 'rtp_1', 1, 't', 1)`)
			// Another role's rename is not held.
			_, _, err = f.svc.RenameSpaceRole(f.ctx, op, "spc_a", "c", "d", 0)
			renameErr(t, err, "", "")

			// A re-queued command that is pending again still blocks; once the node accepts the newest one, nothing does.
			f.exec(`UPDATE node_commands SET state = 'accepted' WHERE idempotency_key = ?`, RenameCommandKey(id))
			_, _, err = f.svc.RenameSpaceRole(f.ctx, op, "spc_a", "z", "y", 0)
			renameErr(t, err, "", "")
		})
	}
}

// ADR-0093 addendum, forget: close with forget_node cancels the rename's undelivered commands, which releases both
// names, and queues the closed notice for the node (a memory.rename with closed set, outside the move-command keys).
// It works on an open rename (closing it) and on a closed one the node has not journaled; a second forget, or one of a
// rename the node holds, is 409 rename_not_open.
func TestCloseForgetNode(t *testing.T) {
	for _, c := range []struct {
		name       string
		closeFirst bool
		cmd        string
	}{{"open, pending", false, "pending"}, {"open, sent", false, "sent"}, {"closed, pending", true, "pending"}, {"closed, rejected", true, "rejected"}} {
		t.Run(c.name, func(t *testing.T) {
			f := newRenameFx(t)
			var told []RoleRename
			f.svc.AfterRenameClose = func(_ context.Context, r RoleRename) { told = append(told, r) }
			id := f.stuck(RenameNodePending, c.cmd)
			if c.closeFirst {
				_, err := f.svc.CloseRoleRename(f.ctx, admin, "spc_a", id, "", false)
				must(t, err)
			}
			got, err := f.svc.CloseRoleRename(f.ctx, admin, "spc_a", id, "node retired", true)
			must(t, err)
			if got.NodeState != RenameNodeAbandoned || !strings.Contains(got.NodeMessage, "orphaned: a") {
				t.Fatalf("forgotten rename: %+v", got)
			}
			want := RenameCommandKey(id) + "=cancelled" // a rejected one too: the connect re-queue must not send it again
			if got := strings.Join(commandStates(f.fx), ","); got != want+","+RenameClosedKey(id)+"=pending" {
				t.Fatalf("commands %s, want the move %s and the closed notice pending", got, want)
			}
			var payload string
			must(t, f.s.ReadDB().QueryRowContext(f.ctx, `SELECT payload_json FROM node_commands WHERE idempotency_key = ?`, RenameClosedKey(id)).Scan(&payload))
			var p proto.MemoryRename
			must(t, json.Unmarshal([]byte(payload), &p))
			if !p.Closed || p.RenameID != id || p.From != "a" || p.To != "z" {
				t.Fatalf("closed notice %+v", p)
			}
			if f.auditCount("role_rename.forgotten") != 1 || len(told) != map[bool]int{true: 2, false: 1}[c.closeFirst] {
				t.Errorf("audit %d, hook %d", f.auditCount("role_rename.forgotten"), len(told))
			}
			var details string
			must(t, f.s.ReadDB().QueryRowContext(f.ctx, `SELECT details_json FROM audit_events WHERE action = 'role_rename.forgotten'`).Scan(&details))
			wantCancelled := map[string]string{"pending": "1", "sent": "1", "rejected": "1"}[c.cmd]
			if !strings.Contains(details, `"cancelled_commands":`+wantCancelled) {
				t.Errorf("audit details %s, want cancelled_commands %s", details, wantCancelled)
			}
			// The names are released.
			_, _, err = f.svc.RenameSpaceRole(f.ctx, op, "spc_a", "z", "y", 0)
			renameErr(t, err, "", "")
			// A second forget has nothing to do.
			_, err = f.svc.CloseRoleRename(f.ctx, admin, "spc_a", id, "", true)
			var rr *RenameRefused
			if apperr.CodeOf(err) != apperr.CodePrecondition || !errors.As(err, &rr) || rr.Reason != RenameNotOpen {
				t.Errorf("second forget: %v", err)
			}
		})
	}
	t.Run("node holds it", func(t *testing.T) {
		f := newRenameFx(t)
		id := f.stuck(RenameNodePending, "accepted")
		_, err := f.svc.CloseRoleRename(f.ctx, admin, "spc_a", id, "", false)
		must(t, err)
		_, err = f.svc.CloseRoleRename(f.ctx, admin, "spc_a", id, "", true)
		if apperr.CodeOf(err) != apperr.CodePrecondition || len(commandStates(f.fx)) != 2 {
			t.Errorf("forget of a rename the node journaled: %v %v", err, commandStates(f.fx))
		}
	})
	t.Run("space left the node", func(t *testing.T) {
		f := newRenameFx(t)
		id := f.stuck(RenameNodePending, "pending")
		f.exec(`INSERT INTO nodes (id, name, state, protocol_minor, created_at, created_by, updated_at) VALUES ('nod_2', 'n2', 'active', 14, 0, 't', 0)`)
		f.exec(`UPDATE spaces SET node_id = 'nod_2' WHERE id = 'spc_a'`)
		_, err := f.svc.CloseRoleRename(f.ctx, admin, "spc_a", id, "", true)
		must(t, err)
		if got := strings.Join(commandStates(f.fx), ","); got != RenameCommandKey(id)+"=cancelled" {
			t.Errorf("commands %s: no notice for a node the space left", got)
		}
	})
}

// A closed rename whose newest command the node rejected stays "undelivered" only while the space is on that node: once
// the space relocates (which cancels only pending and sent commands), nothing is delivered there again and the names are
// free.
func TestClosedRejectedRenameReleasesNamesAfterRelocation(t *testing.T) {
	f := newRenameFx(t)
	id := f.stuck(RenameNodePending, "rejected")
	_, err := f.svc.CloseRoleRename(f.ctx, admin, "spc_a", id, "", false)
	must(t, err)
	_, _, err = f.svc.RenameSpaceRole(f.ctx, op, "spc_a", "z", "y", 0)
	renameErr(t, err, RenameRoleBusy, "closed rename "+id)
	f.exec(`INSERT INTO nodes (id, name, state, protocol_minor, created_at, created_by, updated_at) VALUES ('nod_2', 'n2', 'active', 14, 0, 't', 0)`)
	f.exec(`UPDATE spaces SET node_id = 'nod_2' WHERE id = 'spc_a'`)
	_, _, err = f.svc.RenameSpaceRole(f.ctx, op, "spc_a", "z", "y", 0)
	renameErr(t, err, "", "")
}
