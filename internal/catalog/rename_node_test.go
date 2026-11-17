package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store"
)

// nodeCommands lists the space's node commands of a kind as (generation, key, payload).
func (f *fx) nodeCommands(kind string) []struct {
	gen          int64
	key, payload string
} {
	f.t.Helper()
	rows, err := f.s.ReadDB().QueryContext(f.ctx, `SELECT generation, idempotency_key, payload_json FROM node_commands WHERE kind = ? ORDER BY created_at, id`, kind)
	must(f.t, err)
	defer rows.Close()
	var out []struct {
		gen          int64
		key, payload string
	}
	for rows.Next() {
		var c struct {
			gen          int64
			key, payload string
		}
		must(f.t, rows.Scan(&c.gen, &c.key, &c.payload))
		out = append(out, c)
	}
	must(f.t, rows.Err())
	return out
}

// ADR-0093 T-H2: a rename on a space with a node queues exactly one memory.rename for it in the rename's transaction (at
// the space's lease generation) and runs AfterRename once after the commit.
func TestRenameQueuesMemoryRename(t *testing.T) {
	f := newRenameFx(t)
	f.exec(`INSERT INTO execution_leases (space_id, node_id, generation, state, granted_at, renewed_at, expires_at) VALUES ('spc_a', 'nod_1', 4, 'active', 0, 0, 1)`)
	var after []RoleRename
	f.svc.AfterRename = func(_ context.Context, r RoleRename) { after = append(after, r) }
	_, rn, err := f.svc.RenameSpaceRole(f.ctx, op, "spc_a", "a", "z", 0)
	must(t, err)
	cmds := f.nodeCommands(proto.CmdMemoryRename)
	if len(cmds) != 1 || cmds[0].gen != 4 || cmds[0].key != RenameCommandKey(rn.ID) {
		t.Fatalf("commands %+v", cmds)
	}
	var p proto.MemoryRename
	must(t, json.Unmarshal([]byte(cmds[0].payload), &p))
	if p != (proto.MemoryRename{RenameID: rn.ID, SpaceID: "spc_a", From: "a", To: "z"}) {
		t.Fatalf("payload %+v", p)
	}
	if len(after) != 1 || after[0].ID != rn.ID || after[0].NodeID != "nod_1" {
		t.Fatalf("AfterRename %+v", after)
	}

}

// A node that last negotiated a minor below 14 (or never connected) refuses the rename with node_too_old, before
// anything is written.
func TestRenameRefusesOldNode(t *testing.T) {
	f := newRenameFx(t)
	before := f.renameStateNoID()
	for _, minor := range []int{0, proto.MinorRoleRename - 1} {
		f.exec(`UPDATE nodes SET protocol_minor = ? WHERE id = 'nod_1'`, minor)
		_, _, err := f.svc.RenameSpaceRole(f.ctx, op, "spc_a", "a", "z", 0)
		var rr *RenameRefused
		if apperr.CodeOf(err) != apperr.CodePrecondition || !errors.As(err, &rr) || rr.Reason != RenameNodeTooOld {
			t.Fatalf("minor %d: %v", minor, err)
		}
	}
	if len(f.nodeCommands(proto.CmdMemoryRename)) != 0 || f.role("spc_a", "a") == nil {
		t.Fatal("a refused rename wrote")
	}
	if after := f.renameStateNoID(); !reflect.DeepEqual(before, after) {
		t.Error("a refused rename changed rows")
	}
	f.exec(`UPDATE nodes SET protocol_minor = ? WHERE id = 'nod_1'`, proto.MinorRoleRename)
	_, _, err := f.svc.RenameSpaceRole(f.ctx, op, "spc_a", "a", "z", 0)
	must(t, err)
}

// A failed rename blocks another rename of either name, like a pending one.
func TestRenameBlockedByFailedRename(t *testing.T) {
	f := newRenameFx(t)
	_, rn, err := f.svc.RenameSpaceRole(f.ctx, op, "spc_a", "a", "z", 0)
	must(t, err)
	must(t, f.s.W().TxNamed(f.ctx, "test.failed", func(tx *store.Tx) error {
		_, err := tx.SQL.ExecContext(f.ctx, `UPDATE space_role_renames SET node_state = 'failed' WHERE id = ?`, rn.ID)
		return err
	}))
	_, _, err = f.svc.RenameSpaceRole(f.ctx, op, "spc_a", "z", "y", 0)
	var rr *RenameRefused
	if !errors.As(err, &rr) || rr.Reason != RenameRoleBusy {
		t.Fatalf("rename during a failed node move: %v", err)
	}
}
