package fleet

import (
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// lastPark returns the newest park event.
func (e *switchEnv) lastPark() LimitWaitEvent {
	e.t.Helper()
	for i := len(e.events) - 1; i >= 0; i-- {
		if e.events[i].Change == LimitWaitParked {
			return e.events[i]
		}
	}
	e.t.Fatal("no park event")
	return LimitWaitEvent{}
}

// Every account of the pool limited: the wait resumes at the pool's earliest reset (not the run's own account's, which
// is later), control's park event carries the exhaustion, and the resume re-resolves to the account that is free by
// then, even though the parked run's launch names the other one.
func TestPoolExhaustedWaitsForEarliestReset(t *testing.T) {
	e := newSwitchEnv(t)
	e.hold("ulm_b", "spc_c", "acc_2", "", proto.LimitSevenDay, dur(time.Hour), nil)
	e.running("run_p", "worker", 2)
	e.report("run_p", "idle", 2, e.limit(proto.LimitFiveHour, dur(3*time.Hour)))
	if sw := e.switchOf("run_p"); sw.State != SwitchFellBack || sw.Reason != SwitchNoEligibleAccount {
		t.Fatalf("switch %+v", sw)
	}
	e.endRun("run_p", proto.RunExited, "exited", 2)
	w := e.waitOf("run_p")
	want := e.clk.Now().Add(time.Hour + limitResumeSlack)
	if w.ResumeAt != want.UnixMicro() || w.SwitchToAccountID != "" {
		t.Fatalf("wait resumes at %v (target %q), want the pool's earliest reset %v", time.UnixMicro(w.ResumeAt).Sub(e.clk.Now()), w.SwitchToAccountID, time.Hour+limitResumeSlack)
	}
	ev := e.lastPark()
	if ev.PoolExhausted == nil || ev.PoolExhausted.ResetsAt == nil || !ev.PoolExhausted.ResetsAt.Equal(e.clk.Now().Add(time.Hour)) ||
		ev.PoolExhausted.Harness != "claude-code" || len(ev.PoolExhausted.Accounts) != 2 {
		t.Fatalf("park event %+v", ev.PoolExhausted)
	}
	if n := e.due(); n != 0 {
		t.Fatalf("resumed %d before the pool's reset", n)
	}
	e.clk.Advance(time.Hour + limitResumeSlack)
	if n := e.due(); n != 1 {
		t.Fatalf("resumed %d at the pool's reset: %+v", n, e.waitOf("run_p"))
	}
	if _, p := e.launchOf(e.instance("run_p")); p.AccountRef != "acc_2" || !p.Resume {
		t.Fatalf("resume launched on %q (resume %v), want acc_2, the account free by then", p.AccountRef, p.Resume)
	}
}

// With the whole pool limited, a hold whose reset is unknown doesn't free its account, and a
// pool whose resets are all unknown keeps ADR-0079's own wait.
func TestPoolExhaustedUnknownResets(t *testing.T) {
	e := newSwitchEnv(t)
	e.hold("ulm_b", "spc_c", "acc_2", "", proto.LimitSevenDay, nil, nil)
	e.running("run_u", "worker", 2)
	e.report("run_u", "idle", 2, e.limit(proto.LimitFiveHour, dur(3*time.Hour)))
	e.endRun("run_u", proto.RunExited, "exited", 2)
	// acc_2's reset is unknown: the earliest known one is the run's own account's.
	if w := e.waitOf("run_u"); w.ResumeAt != e.clk.Now().Add(3*time.Hour+limitResumeSlack).UnixMicro() {
		t.Fatalf("wait resumes in %v", time.UnixMicro(w.ResumeAt).Sub(e.clk.Now()))
	}
	if ev := e.lastPark(); ev.PoolExhausted == nil || ev.PoolExhausted.ResetsAt == nil || !ev.PoolExhausted.ResetsAt.Equal(e.clk.Now().Add(3*time.Hour)) {
		t.Fatalf("park event %+v", ev.PoolExhausted)
	}
}

// A pool with a free account isn't exhausted: a spent switch budget leaves ADR-0079's wait on the run's own account
// and no notice, and failover off never reads the pool.
func TestPoolNotExhausted(t *testing.T) {
	e := newSwitchEnv(t)
	e.budget = 0
	e.running("run_n", "worker", 2)
	e.report("run_n", "idle", 2, e.limit(proto.LimitFiveHour, dur(3*time.Hour)))
	e.endRun("run_n", proto.RunExited, "exited", 2)
	if w := e.waitOf("run_n"); w.ResumeAt != e.clk.Now().Add(3*time.Hour+limitResumeSlack).UnixMicro() {
		t.Fatalf("wait resumes in %v", time.UnixMicro(w.ResumeAt).Sub(e.clk.Now()))
	}
	if ev := e.lastPark(); ev.PoolExhausted != nil {
		t.Fatalf("exhausted: %+v", ev.PoolExhausted)
	}

	e.failoverOn, e.budget = false, 6
	e.hold("ulm_b", "spc_c", "acc_2", "", proto.LimitSevenDay, dur(time.Hour), nil)
	e.running("run_o", "worker", 2)
	e.report("run_o", "idle", 2, e.limit(proto.LimitFiveHour, dur(3*time.Hour)))
	e.endRun("run_o", proto.RunExited, "exited", 2)
	if w := e.waitOf("run_o"); w.ResumeAt != e.clk.Now().Add(3*time.Hour+limitResumeSlack).UnixMicro() {
		t.Fatalf("failover off: wait resumes in %v", time.UnixMicro(w.ResumeAt).Sub(e.clk.Now()))
	}
	if ev := e.lastPark(); ev.PoolExhausted != nil {
		t.Fatalf("failover off: exhausted %+v", ev.PoolExhausted)
	}
}

// A switch whose target is gone by its resume, with nothing else eligible, becomes the same wait: the pool's earliest
// reset, and the exhaustion told.
func TestPoolExhaustedAtSwitchResume(t *testing.T) {
	e := newSwitchEnv(t)
	e.running("run_s", "worker", 2)
	e.armAndPark("run_s")
	e.hold("ulm_b", "spc_c", "acc_2", "", proto.LimitSevenDay, dur(time.Hour), nil)
	e.clk.Advance(limitRetrySoon)
	if n := e.due(); n != 0 {
		t.Fatalf("resumed %d with the pool exhausted", n)
	}
	w := e.waitOf("run_s")
	if w.SwitchToAccountID != "" || w.ResumeAt != e.clk.Now().Add(time.Hour+limitResumeSlack-limitRetrySoon).UnixMicro() {
		t.Fatalf("wait %+v", w)
	}
	if ev := e.lastPark(); ev.PoolExhausted == nil || ev.PoolExhausted.ResetsAt == nil {
		t.Fatalf("park event %+v", ev)
	}
}

// A role's pool never widens (review of PRs 158 and 159): with the role's accounts (acc_1, acc_3) all limited and
// acc_2, bound to the space but outside the role's pool, free, the pool is exhausted, the wait ends at the role pool's
// earliest reset, and the resume never lands on acc_2 or names the old account.
func TestPoolExhaustedRolePoolNeverWidens(t *testing.T) {
	e := newSwitchEnv(t)
	e.exec(`INSERT INTO space_accounts (space_id, account_id, harness, created_at, created_by) VALUES ('spc_a', 'acc_3', 'claude-code', 1, 't')`)
	e.exec(`INSERT INTO space_role_accounts (space_id, role, account_id, priority, created_at, created_by) VALUES ('spc_a', 'implementor', 'acc_1', 100, 0, 't'),
		('spc_a', 'implementor', 'acc_3', 100, 0, 't')`)
	e.hold("ulm_b", "spc_c", "acc_3", "", proto.LimitSevenDay, dur(time.Hour), nil)
	e.running("run_r", "worker", 2)
	e.report("run_r", "idle", 2, e.limit(proto.LimitFiveHour, dur(3*time.Hour)))
	if sw := e.switchOf("run_r"); sw.State != SwitchFellBack || sw.Reason != SwitchNoEligibleAccount || sw.ToAccountID != "" {
		t.Fatalf("switch %+v: acc_2 is outside the role's pool and must not be chosen", sw)
	}
	e.endRun("run_r", proto.RunExited, "exited", 2)
	ev := e.lastPark()
	if ev.PoolExhausted == nil || len(ev.PoolExhausted.Accounts) != 2 || ev.PoolExhausted.Accounts[0] != "acc_1" && ev.PoolExhausted.Accounts[1] != "acc_1" {
		t.Fatalf("pool %+v", ev.PoolExhausted)
	}
	if w := e.waitOf("run_r"); w.ResumeAt != e.clk.Now().Add(time.Hour+limitResumeSlack).UnixMicro() {
		t.Fatalf("wait resumes in %v", time.UnixMicro(w.ResumeAt).Sub(e.clk.Now()))
	}
	// The pool changed meanwhile: the run's account is no longer in it. The resume still stays in the pool.
	e.exec(`DELETE FROM space_role_accounts WHERE account_id = 'acc_1'`)
	e.exec(`UPDATE usage_limits SET cleared_at = 1 WHERE account_id = 'acc_1'`)
	e.clk.Advance(time.Hour + limitResumeSlack)
	if n := e.due(); n != 1 {
		t.Fatalf("resumed %d: %+v", n, e.waitOf("run_r"))
	}
	if _, p := e.launchOf(e.instance("run_r")); p.AccountRef != "acc_3" {
		t.Fatalf("resume launched on %q, want acc_3 of the role's pool (never acc_1 or acc_2)", p.AccountRef)
	}
}
