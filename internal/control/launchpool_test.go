package control

import (
	"context"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/accounts"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// ADR-0090: the supervisor's relaunch and the control autostart name the last control run's account, and the launch
// transaction keeps them inside the role's pool (RequirePoolMember). These tests fail if either caller drops the flag.

// poolEnv is a peopleEnv whose fleet resolves accounts as central does, with failover on and a role pool of acc_3
// only. Control's last run used acc_2 (bound to the space, outside the pool).
func newPoolLaunchEnv(t *testing.T) *peopleEnv {
	t.Helper()
	e := newPeopleEnvOpts(t, fleet.Options{
		ResolveAccount: accounts.Resolve,
		UsageLimitSettings: func(context.Context, *db.Queries, string) fleet.UsageLimitSettings {
			return fleet.UsageLimitSettings{Failover: true}
		},
	})
	e.exec(`DELETE FROM usage_limits`)
	e.exec(`INSERT INTO accounts (id, name, kind, secret_enc, secret_rotated_at, created_at, created_by, updated_at) VALUES
		('acc_3', 'three', 'claude_oauth_token', x'00', 0, 0, 't', 0)`)
	e.exec(`INSERT INTO space_accounts (space_id, account_id, harness, is_default, created_at, created_by) VALUES
		('spc_a', 'acc_2', 'claude-code', 1, 0, 't'), ('spc_a', 'acc_3', 'claude-code', 0, 1, 't')`)
	e.exec(`INSERT INTO space_role_accounts (space_id, role, account_id, priority, created_at, created_by) VALUES ('spc_a', 'control', 'acc_3', 100, 0, 't')`)
	e.exec(`INSERT INTO execution_leases (space_id, node_id, generation, state, granted_at, renewed_at, expires_at) VALUES
		('spc_a', 'nod_1', 1, 'active', 0, 0, ?)`, e.clk.Now().Add(1000*3600*1e9).UnixMicro())
	e.exec(`INSERT INTO runs (id, space_id, node_id, generation, kind, role, harness, instance_id, session_id, turn, desired, observed, launch_json,
		created_at, created_by, updated_at, observed_at) VALUES ('run_ctl', 'spc_a', 'nod_1', 1, 'control', 'control', 'claude-code', ?, 'ses_ctl', 3,
		'running', 'exited', '{"account_ref":"acc_2","prompt":"p"}', 0, 't', 0, 0)`, ids.New(ids.Instance))
	return e
}

func (e *peopleEnv) lastAccount() string {
	e.t.Helper()
	var ref string
	if err := e.s.ReadDB().QueryRowContext(e.ctx, `SELECT COALESCE(json_extract(launch_json, '$.account_ref'), '') FROM runs WHERE id != 'run_ctl' ORDER BY created_at DESC, id DESC LIMIT 1`).Scan(&ref); err != nil {
		e.t.Fatalf("no relaunched run: %v", err)
	}
	return ref
}

func TestSupervisorRelaunchStaysInRolePool(t *testing.T) {
	e := newPoolLaunchEnv(t)
	e.sv.Async = false
	e.sv.OnRunEnded("run_ctl")
	if got := e.lastAccount(); got != "acc_3" {
		t.Fatalf("control restarted on %q, want acc_3 of the role pool (acc_2 is outside it)", got)
	}
}

func TestAutostartStaysInRolePool(t *testing.T) {
	e := newPoolLaunchEnv(t)
	e.exec(`UPDATE runs SET observed = 'exited', desired = 'stopped' WHERE id = 'run_ctl'`)
	e.sv.Limit = func(context.Context, string, string) int64 { return 1 }
	e.sv.ControlWanted(e.ctx, "spc_a")
	if got := e.lastAccount(); got != "acc_3" {
		t.Fatalf("control autostarted on %q, want acc_3 of the role pool (acc_2 is outside it)", got)
	}
}
