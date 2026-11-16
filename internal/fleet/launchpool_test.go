package fleet

import (
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/accounts"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// ADR-0090: the launch transaction itself keeps a role's pool from widening. A resume decides on its account in a claim
// transaction and launches in another; a pool change between the two must not put the launch on the removed account.

// newPoolEnv is a switchEnv whose launches resolve their account as central does (accounts.Resolve).
func newPoolEnv(t *testing.T) *switchEnv {
	t.Helper()
	e := newSwitchEnv(t)
	e.svc.opts.ResolveAccount = accounts.Resolve
	return e
}

// movePool binds acc_3 and gives the role a pool of acc_1 and acc_3, so acc_2 (bound to the space) is outside it.
func (e *switchEnv) movePool() {
	e.t.Helper()
	e.exec(`INSERT INTO space_accounts (space_id, account_id, harness, created_at, created_by) VALUES ('spc_a', 'acc_3', 'claude-code', 1, 't')`)
	e.exec(`INSERT INTO space_role_accounts (space_id, role, account_id, priority, created_at, created_by) VALUES ('spc_a', 'implementor', 'acc_1', 100, 0, 't'),
		('spc_a', 'implementor', 'acc_3', 100, 0, 't')`)
}

// relaunch builds the resume launch of run's wait (as the claim leaves it) and launches it, as resume does after its claim.
func (e *switchEnv) relaunch(run string, mutate func(*LaunchInput)) string {
	e.t.Helper()
	in, err := waitLaunch(e.waitOf(run), false)
	if err != nil {
		e.t.Fatal(err)
	}
	if mutate != nil {
		mutate(&in)
	}
	if _, err := e.svc.LaunchRun(e.ctx, holdActor, "spc_a", in); err != nil {
		e.t.Fatal(err)
	}
	_, p := e.launchOf(e.instance(run))
	return p.AccountRef
}

// A switch's target that leaves the role's pool between the claim and the launch is not used: the pool picks.
func TestLaunchPoolSwitchTargetRemovedAfterClaim(t *testing.T) {
	e := newPoolEnv(t)
	e.running("run_x", "worker", 2)
	if sw := e.armAndPark("run_x"); sw.ToAccountID != "acc_2" {
		t.Fatalf("target %s", sw.ToAccountID)
	}
	e.clk.Advance(limitRetrySoon)
	e.movePool() // after the claim would have named acc_2
	if got := e.relaunch("run_x", nil); got != "acc_3" {
		t.Fatalf("launched on %q, want acc_3 of the role pool (never the removed acc_2)", got)
	}
}

// An ADR-0079 resume with failover on (no switch target: the launch names the parked run's own account) whose account
// left the pool after the claim does not name it.
func TestLaunchPoolFailoverResumeAccountRemovedAfterClaim(t *testing.T) {
	e := newPoolEnv(t)
	e.running("run_y", "worker", 2)
	e.armAndPark("run_y")
	e.movePool()
	e.exec(`DELETE FROM space_role_accounts WHERE account_id = 'acc_1'`)
	e.exec(`UPDATE usage_limits SET cleared_at = 1`) // acc_1's hold has cleared, so only the pool check keeps the launch off it
	got := e.relaunch("run_y", func(in *LaunchInput) { in.AccountRef = "acc_1" })
	if got != "acc_3" {
		t.Fatalf("the resume launched on %q, want acc_3 (acc_1 left the role pool)", got)
	}
}

// A pool-exhausted resume (no target) whose parked account left the pool launches from the pool.
func TestLaunchPoolExhaustedResumeAccountRemovedAfterClaim(t *testing.T) {
	e := newPoolEnv(t)
	e.movePool()
	e.hold("ulm_b", "spc_c", "acc_3", "", proto.LimitSevenDay, dur(time.Hour), nil)
	e.running("run_r", "worker", 2)
	e.report("run_r", "idle", 2, e.limit(proto.LimitFiveHour, dur(3*time.Hour)))
	e.endRun("run_r", proto.RunExited, "exited", 2)
	e.exec(`DELETE FROM space_role_accounts WHERE account_id = 'acc_1'`) // after the claim
	e.exec(`UPDATE usage_limits SET cleared_at = 1`)
	if got := e.relaunch("run_r", nil); got != "acc_3" {
		t.Fatalf("launched on %q, want acc_3", got)
	}
}

// An operator or API launch that names an account keeps it, even outside the role's pool (behaviour unchanged).
func TestLaunchPoolExplicitOperatorRefKept(t *testing.T) {
	e := newPoolEnv(t)
	e.movePool()
	e.running("run_o", "worker", 2)
	e.armAndPark("run_o")
	if got := e.relaunch("run_o", func(in *LaunchInput) { in.AccountRef, in.RequirePoolMember = "acc_2", false }); got != "acc_2" {
		t.Fatalf("explicit ref launched on %q, want acc_2", got)
	}
}

// With failover off role pools don't apply (ADR-0090 decision 3), so the check is a no-op: a run on the space default,
// outside the pool, keeps its account.
func TestLaunchPoolFailoverOffKeepsRef(t *testing.T) {
	e := newPoolEnv(t)
	e.running("run_z", "worker", 2)
	e.armAndPark("run_z")
	e.movePool()
	e.exec(`DELETE FROM space_role_accounts WHERE account_id = 'acc_1'`)
	e.failoverOn = false
	if got := e.relaunch("run_z", func(in *LaunchInput) { in.AccountRef = "acc_2" }); got != "acc_2" {
		t.Fatalf("launched on %q, want acc_2 kept (failover off: role pools don't apply)", got)
	}
}

// A resume whose account is still a pool member keeps it, even with a higher-priority member present.
func TestLaunchPoolMemberKeptOverHigherPriority(t *testing.T) {
	e := newPoolEnv(t)
	e.running("run_m", "worker", 2)
	e.armAndPark("run_m")
	e.movePool()
	e.exec(`UPDATE space_role_accounts SET priority = 10 WHERE account_id = 'acc_1'`)
	e.exec(`UPDATE space_role_accounts SET priority = 100 WHERE account_id = 'acc_3'`)
	e.exec(`UPDATE usage_limits SET cleared_at = 1`)
	if got := e.relaunch("run_m", func(in *LaunchInput) { in.AccountRef = "acc_3" }); got != "acc_3" {
		t.Fatalf("launched on %q, want the member acc_3 kept", got)
	}
}
