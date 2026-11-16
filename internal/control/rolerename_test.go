package control

import (
	"context"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/catalog"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
)

// ADR-0093: control's role was renamed (control → lead) after its last run. The supervisor's restart and the autostart
// copy the role from that run, so they resolve it to the current name first; while the node has not moved the role's
// memory yet the launch is held.

func newRenamedControlEnv(t *testing.T, nodeState string) *peopleEnv {
	t.Helper()
	e := newPeopleEnv(t)
	e.exec(`DELETE FROM usage_limits`)
	e.exec(`INSERT INTO role_templates (id, slug, name, kind, created_at, created_by, updated_at) VALUES ('rtp_c', 'control', 'Control', 'control', 0, 't', 0)`)
	e.exec(`INSERT INTO space_roles (space_id, name, template_id, created_at, created_by, updated_at) VALUES ('spc_a', 'lead', 'rtp_c', 0, 't', 0)`)
	e.exec(`INSERT INTO space_role_renames (id, space_id, from_name, to_name, renamed_at, renamed_by, node_id, node_state)
		VALUES ('rrn_1', 'spc_a', 'control', 'lead', 1, 't', 'nod_1', ?)`, nodeState)
	e.exec(`INSERT INTO execution_leases (space_id, node_id, generation, state, granted_at, renewed_at, expires_at) VALUES
		('spc_a', 'nod_1', 1, 'active', 0, 0, ?)`, e.clk.Now().Add(1000*3600*1e9).UnixMicro())
	e.exec(`INSERT INTO runs (id, space_id, node_id, generation, kind, role, harness, instance_id, session_id, turn, desired, observed, launch_json,
		created_at, created_by, updated_at, observed_at) VALUES ('run_ctl', 'spc_a', 'nod_1', 1, 'control', 'control', 'claude-code', ?, 'ses_ctl', 3,
		'running', 'exited', '{"prompt":"p"}', 0, 't', 0, 0)`, ids.New(ids.Instance))
	e.sv.RoleName = func(ctx context.Context, spaceID, name string) (string, error) {
		return catalog.CurrentRoleName(ctx, e.s.R(), spaceID, name)
	}
	e.sv.Async = false
	return e
}

// relaunched is the role of the run launched after run_ctl ("" when none was).
func (e *peopleEnv) relaunched() string {
	e.t.Helper()
	var role string
	err := e.s.ReadDB().QueryRowContext(e.ctx, `SELECT role FROM runs WHERE id != 'run_ctl' ORDER BY created_at DESC, id DESC LIMIT 1`).Scan(&role)
	if err != nil {
		return ""
	}
	return role
}

func TestSupervisorRestartUsesRenamedRole(t *testing.T) {
	e := newRenamedControlEnv(t, "done")
	e.sv.OnRunEnded("run_ctl")
	if got := e.relaunched(); got != "lead" {
		t.Fatalf("control restarted as %q, want the renamed role lead", got)
	}
}

func TestAutostartUsesRenamedRole(t *testing.T) {
	e := newRenamedControlEnv(t, "done")
	e.exec(`UPDATE runs SET desired = 'stopped' WHERE id = 'run_ctl'`)
	e.sv.Limit = func(context.Context, string, string) int64 { return 1 }
	e.sv.ControlWanted(e.ctx, "spc_a")
	if got := e.relaunched(); got != "lead" {
		t.Fatalf("control autostarted as %q, want the renamed role lead", got)
	}
}

// While the node move is pending neither starts control: under the new name the launch is held, and the old name is
// never launched.
func TestRenamedControlHeldWhileNodeMovePending(t *testing.T) {
	e := newRenamedControlEnv(t, "pending")
	e.sv.OnRunEnded("run_ctl")
	if got := e.relaunched(); got != "" {
		t.Fatalf("control restarted as %q while the rename's node move is pending", got)
	}
	e.exec(`UPDATE runs SET desired = 'stopped' WHERE id = 'run_ctl'`)
	e.sv.Limit = func(context.Context, string, string) int64 { return 1 }
	e.sv.ControlWanted(e.ctx, "spc_a")
	if got := e.relaunched(); got != "" {
		t.Fatalf("control autostarted as %q while the rename's node move is pending", got)
	}
	// The move is reported: the next start goes through under the new name.
	e.exec(`UPDATE space_role_renames SET node_state = 'done' WHERE id = 'rrn_1'`)
	e.sv.ControlWanted(e.ctx, "spc_a")
	if got := e.relaunched(); got != "lead" {
		t.Fatalf("control started as %q after the move, want lead", got)
	}
}
