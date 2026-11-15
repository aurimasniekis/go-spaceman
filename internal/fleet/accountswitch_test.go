package fleet

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/obs/metrics/metricstest"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// switchEnv is parkEnv with failover on and the account-switch settings (ADR-0090): acc_1 (the runs' account, the
// default) and acc_2 are bound to space A for Claude Code; acc_3 exists unbound.
type switchEnv struct {
	*parkEnv
	failoverOn bool
	within     time.Duration
	budget     int
	// notices are the runs control was told are kept alive at a limit (OnRunLimitTx with an opened limit).
	notices []string
}

func newSwitchEnv(t *testing.T) *switchEnv {
	t.Helper()
	e := &switchEnv{parkEnv: newParkEnv(t), failoverOn: true, within: 15 * time.Minute, budget: 6}
	e.svc.opts.UsageLimitSettings = func(context.Context, *db.Queries, string) UsageLimitSettings {
		return UsageLimitSettings{Failover: e.failoverOn, KeepAliveMax: e.keepMax, WaitIfResetWithin: e.within, SwitchMaxPerDay: e.budget}
	}
	e.exec(`INSERT INTO accounts (id, name, kind, secret_enc, secret_rotated_at, created_at, created_by, updated_at) VALUES
		('acc_3', 'three', 'claude_oauth_token', x'00', 0, 0, 't', 0)`)
	e.svc.opts.OnRunLimitTx = func(ctx context.Context, tx *store.Tx, r *Run, _, opened *RunLimit) error {
		// As control does: no "kept alive" notice for a run with an armed switch.
		if opened == nil || !Live(r.Observed) {
			return nil
		}
		if _, err := tx.Q.GetPendingAccountSwitchByRun(ctx, r.ID); err == nil {
			return nil
		}
		e.notices = append(e.notices, r.ID)
		return nil
	}
	return e
}

// running seeds a live run on acc_1 without a limit: it has finished turn turns of session ses_<id>.
func (e *switchEnv) running(id, kind string, turn int) {
	e.t.Helper()
	e.exec(`INSERT INTO runs (id, space_id, node_id, generation, kind, role, harness, instance_id, session_id, turn, desired, observed, launch_json,
		created_at, created_by, updated_at, observed_at) VALUES (?, 'spc_a', 'nod_1', 1, ?, 'implementor', 'claude-code', ?, ?, ?, 'running', 'running', ?, 0, 't', 0, 0)`,
		id, kind, e.instance(id), "ses_"+id, turn, `{"account_ref":"acc_1","prompt":"do the task","model":"m1"}`)
}

// report sends a running report for run with status, turn and limit (nil: none).
func (e *switchEnv) report(run, status string, turn int, l *proto.RunLimit) {
	e.t.Helper()
	r := e.observed(run)
	if _, err := e.svc.applyReport(e.ctx, "nod_1", proto.RunReport{RunID: run, SpaceID: "spc_a", Generation: 1, State: proto.RunRunning, Status: status,
		Turn: uint64(turn), SessionID: r.SessionID, Limit: l, ObservedAt: e.clk.Now()}); err != nil { //nolint:gosec // small test values
		e.t.Fatal(err)
	}
}

// limit is a limit of kind seen now, resetting after resets (nil: unknown).
func (e *switchEnv) limit(kind string, resets *time.Duration) *proto.RunLimit {
	l := &proto.RunLimit{Kind: kind, Since: e.clk.Now()}
	if resets != nil {
		at := e.clk.Now().Add(*resets)
		l.ResetsAt = &at
	}
	return l
}

func (e *switchEnv) switchOf(run string) db.AccountSwitch {
	e.t.Helper()
	sw, err := e.s.R().GetLatestAccountSwitchByRun(e.ctx, run)
	if err != nil {
		e.t.Fatalf("switch of %s: %v", run, err)
	}
	return sw
}

func (e *switchEnv) switches(run string) int {
	return e.count(`SELECT count(*) FROM account_switches WHERE from_run_id = ?`, run)
}

func (e *switchEnv) metric(result string) float64 {
	return metricstest.Value(e.t, e.m, "spaceman_account_switches_total", "result", result)
}

// endRun reports run ended with state and reason at turn.
func (e *switchEnv) endRun(run, state, reason string, turn int) {
	e.t.Helper()
	r := e.observed(run)
	if _, err := e.svc.applyReport(e.ctx, "nod_1", proto.RunReport{RunID: run, SpaceID: "spc_a", Generation: 1, State: state, Reason: reason,
		Turn: uint64(turn), SessionID: r.SessionID, ObservedAt: e.clk.Now()}); err != nil { //nolint:gosec // small test values
		e.t.Fatal(err)
	}
}

// armAndPark limits run on acc_1 (5-hour, reset in 3 h) while busy, then reports it idle: it is parked for a switch
// and its node reports the stop.
func (e *switchEnv) armAndPark(run string) db.AccountSwitch {
	e.t.Helper()
	e.report(run, "working", 2, e.limit(proto.LimitFiveHour, dur(3*time.Hour)))
	if sw := e.switchOf(run); sw.State != SwitchPending {
		e.t.Fatalf("switch %+v, want pending", sw)
	}
	e.report(run, "idle", 2, nil)
	sw := e.switchOf(run)
	if sw.State != SwitchParked {
		e.t.Fatalf("switch %+v, want parked", sw)
	}
	e.endRun(run, proto.RunExited, "stopped", 2)
	return sw
}

// ADR-0090 §3.4, the whole path: a worker that reaches its 5-hour limit while busy has a switch armed (pending) to the
// pool's other account; control isn't told yet and nothing stops the run. At its idle point the run is parked with a
// wait due at once on the target, and stopped. The job resumes it at once as a new run of the same instance on acc_2
// with --resume of the same session and the switch prompt; the task follows. The new run's first turn marks the switch
// succeeded. Every step is audited, the start and the end are published, and the metric counts both.
func TestAccountSwitchSucceeds(t *testing.T) {
	e := newSwitchEnv(t)
	e.running("run_w", "worker", 2)
	e.task("tsk_1", "run_w", "running")

	e.report("run_w", "working", 2, e.limit(proto.LimitFiveHour, dur(3*time.Hour)))
	sw := e.switchOf("run_w")
	if sw.State != SwitchPending || sw.FromAccountID != "acc_1" || sw.ToAccountID != "acc_2" || sw.LimitKind != proto.LimitFiveHour || sw.UsageLimitID == "" ||
		!sw.ArmedUntil.Valid || sw.ArmedUntil.Int64 != e.clk.Now().Add(switchArmMax).UnixMicro() || sw.Cause != SwitchCauseUsageLimit {
		t.Fatalf("armed switch %+v", sw)
	}
	if r := e.observed("run_w"); r.Desired != "running" || e.count(`SELECT count(*) FROM limit_waits`) != 0 {
		t.Fatal("a busy run was parked")
	}
	// The keep-alive park leaves a run with an armed switch to its switch; its idle-point cap is the job's next deadline.
	if at := e.nextAt(); !at.Equal(e.clk.Now().Add(switchArmMax)) {
		t.Fatalf("next %v, want the arm cap", at.Sub(e.clk.Now()))
	}

	e.clk.Advance(time.Minute)
	e.report("run_w", "idle", 2, nil)
	w := e.waitOf("run_w")
	if w.ResumeAt != e.clk.Now().UnixMicro() || w.SwitchToAccountID != "acc_2" || w.AccountSwitchID != sw.ID || w.SessionID != "ses_run_w" || w.TaskID != "tsk_1" {
		t.Fatalf("switch wait %+v", w)
	}
	if sw = e.switchOf("run_w"); sw.State != SwitchParked || sw.LimitWaitID != w.ID {
		t.Fatalf("parked switch %+v", sw)
	}
	if r := e.observed("run_w"); r.Desired != "stopped" || e.count(`SELECT count(*) FROM node_commands WHERE run_id = 'run_w' AND kind = ?`, proto.CmdStop) != 1 {
		t.Fatalf("parked run not stopped: %+v", r)
	}
	if ev := e.events[len(e.events)-1]; ev.Change != LimitWaitParked || ev.Switch == nil || ev.Switch.ID != sw.ID {
		t.Fatalf("park event %+v", ev)
	}
	// It doesn't resume before the parked run has ended.
	if n := e.due(); n != 0 {
		t.Fatalf("resumed %d before the run ended", n)
	}
	e.endRun("run_w", proto.RunExited, "stopped", 2)
	if r := e.observed("run_w"); r.EndReason != EndUsageLimit {
		t.Fatalf("end reason %q", r.EndReason)
	}
	e.clk.Advance(limitRetrySoon)
	if n := e.due(); n != 1 {
		t.Fatalf("resumed %d", n)
	}
	newID, p := e.launchOf(e.instance("run_w"))
	if newID == "run_w" || p.AccountRef != "acc_2" || !p.Resume || p.SessionID != "ses_run_w" || p.Prompt != switchPrompt || p.Model != "m1" {
		t.Fatalf("switch launch %s %+v", newID, p)
	}
	if sw = e.switchOf("run_w"); sw.State != SwitchResumed || sw.NewRunID != newID || sw.ToAccountID != "acc_2" {
		t.Fatalf("resumed switch %+v", sw)
	}
	if e.count(`SELECT count(*) FROM tasks WHERE id = 'tsk_1' AND assignee_run = ?`, newID) != 1 {
		t.Fatal("task not re-pointed")
	}
	if ev := e.events[len(e.events)-1]; ev.Change != LimitWaitResumed || ev.Switch == nil || ev.Switch.NewRunID != newID || ev.Switch.ToAccountID != "acc_2" {
		t.Fatalf("resume event %+v", ev)
	}
	var manifest string
	_ = e.s.ReadDB().QueryRowContext(e.ctx, `SELECT manifest_json FROM runs WHERE id = ?`, newID).Scan(&manifest)
	if !strings.Contains(manifest, `"account_switch"`) || !strings.Contains(manifest, sw.ID) {
		t.Fatalf("manifest %s", manifest)
	}

	// The new run starts and finishes its first turn: verified.
	e.exec(`UPDATE runs SET observed = 'running' WHERE id = ?`, newID)
	e.report(newID, "working", 0, nil)
	if sw = e.switchOf("run_w"); sw.State != SwitchResumed {
		t.Fatalf("switch %s before a turn", sw.State)
	}
	e.report(newID, "idle", 1, nil)
	if sw = e.switchOf("run_w"); sw.State != SwitchSucceeded {
		t.Fatalf("switch %+v, want succeeded", sw)
	}
	for _, a := range []string{"account.switch_started", "account.switch_parked", "account.switch_resumed", "account.switch_succeeded"} {
		if e.count(`SELECT count(*) FROM audit_events WHERE action = ? AND target_id = 'run_w'`, a) != 1 {
			t.Errorf("audit %s missing", a)
		}
	}
	if n := e.count(`SELECT count(*) FROM event_outbox WHERE subject LIKE 'evt.spaceman.fleet.account_switch.%'`); n != 2 {
		t.Errorf("%d switch events, want started and completed", n)
	}
	if n := e.count(`SELECT count(*) FROM changes WHERE resource_type = 'account_switch' AND resource_id = ?`, sw.ID); n != 4 {
		t.Errorf("%d change records, want 4 (created, parked, resumed, succeeded)", n)
	}
	if e.metric("started") != 1 || e.metric("succeeded") != 1 {
		t.Errorf("metrics started %v succeeded %v", e.metric("started"), e.metric("succeeded"))
	}
	if list, err := e.svc.AccountSwitches(e.ctx, "spc_a"); err != nil || len(list) != 1 || list[0].State != SwitchSucceeded {
		t.Fatalf("listing %+v %v", list, err)
	}
}

// Q4 (operator answer, ADR-0090 decision 2): a limited account that resets within usage_limit_wait_if_reset_within
// keeps the run waiting as ADR-0079 does: no switch is recorded and the wait resumes at the reset on the same account.
// A reset just outside the window switches, and so does an unknown reset.
func TestAccountSwitchNearResetWaits(t *testing.T) {
	e := newSwitchEnv(t)
	e.running("run_n", "worker", 1)
	e.report("run_n", "idle", 1, e.limit(proto.LimitFiveHour, dur(15*time.Minute)))
	if n := e.switches("run_n"); n != 0 {
		t.Fatalf("%d switches for a reset within the window", n)
	}
	if r := e.observed("run_n"); r.Desired != "running" {
		t.Fatal("a near-reset run was stopped")
	}
	e.endRun("run_n", proto.RunExited, "exited", 1)
	if w := e.waitOf("run_n"); w.SwitchToAccountID != "" || w.ResumeAt != e.clk.Now().Add(17*time.Minute).UnixMicro() {
		t.Fatalf("near-reset wait %+v", w)
	}
	if n := e.switches("run_n"); n != 0 {
		t.Fatalf("its end decided %d switches", n)
	}

	e.running("run_o", "worker", 1)
	e.report("run_o", "working", 1, e.limit(proto.LimitFiveHour, dur(16*time.Minute)))
	if sw := e.switchOf("run_o"); sw.State != SwitchPending || sw.ToAccountID != "acc_2" {
		t.Fatalf("a reset outside the window: %+v", sw)
	}
	if strings.Contains(strings.Join(e.notices, ","), "run_o") {
		t.Fatal("control told to keep alive a run with an armed switch")
	}

	// The armed switch's limit is re-read with a reset inside the window: it falls back and the run waits.
	e.report("run_o", "working", 1, &proto.RunLimit{Kind: proto.LimitFiveHour, Since: e.clk.Now(), ResetsAt: ptrTime(e.clk.Now().Add(10 * time.Minute))})
	if sw := e.switchOf("run_o"); sw.State != SwitchFellBack || sw.Reason != SwitchResetNear {
		t.Fatalf("re-read reset near: %+v", sw)
	}
	// Control now hears the run is kept alive (the notice held back while the switch was armed).
	if !strings.Contains(strings.Join(e.notices, ","), "run_o") {
		t.Fatalf("control not told after the fallback: %v", e.notices)
	}

	e.running("run_u", "worker", 1)
	e.report("run_u", "working", 1, e.limit(proto.LimitUnknown, nil))
	if sw := e.switchOf("run_u"); sw.State != SwitchPending {
		t.Fatalf("an unknown reset: %+v", sw)
	}
}

func ptrTime(t time.Time) *time.Time { return &t }

// Fallbacks at the decision: with every other pool account held (in any space), the switch is recorded as fell_back
// (no_eligible_account) and the run waits as ADR-0079 does; with the daily budget spent likewise (budget_spent), and a
// budget of 0 never switches. Failover off decides nothing at all.
func TestAccountSwitchFallbacksAtDecision(t *testing.T) {
	e := newSwitchEnv(t)
	e.hold("ulm_b", "spc_c", "acc_2", "", proto.LimitSevenDay, dur(48*time.Hour), nil)
	e.running("run_h", "worker", 1)
	e.report("run_h", "idle", 1, e.limit(proto.LimitFiveHour, dur(3*time.Hour)))
	if sw := e.switchOf("run_h"); sw.State != SwitchFellBack || sw.Reason != SwitchNoEligibleAccount || sw.ToAccountID != "" {
		t.Fatalf("no eligible account: %+v", sw)
	}
	if r := e.observed("run_h"); r.Desired != "running" {
		t.Fatal("a run without a target was stopped")
	}
	// Its end parks it as ADR-0079 does, without deciding again.
	e.endRun("run_h", proto.RunExited, "exited", 1)
	if w := e.waitOf("run_h"); w.SwitchToAccountID != "" || w.ResumeAt != e.clk.Now().Add(3*time.Hour+2*time.Minute).UnixMicro() {
		t.Fatalf("fallback wait %+v", w)
	}
	if n := e.switches("run_h"); n != 1 {
		t.Fatalf("%d switch rows, want the one fallback", n)
	}
	if e.metric("fell_back") != 1 {
		t.Fatalf("fell_back metric %v", e.metric("fell_back"))
	}
	e.exec(`UPDATE usage_limits SET cleared_at = 1 WHERE id = 'ulm_b'`)

	// Budget: one switch in the last 24 h of the instance, budget 1.
	e.budget = 1
	e.running("run_b", "worker", 1)
	e.exec(`INSERT INTO account_switches (id, space_id, instance_id, role, run_kind, harness, from_run_id, from_account_id, to_account_id, cause, limit_kind,
		state, created_at, updated_at) VALUES ('asw_old', 'spc_a', ?, 'implementor', 'worker', 'claude-code', 'run_prev', 'acc_2', 'acc_1', 'usage_limit',
		'five_hour', 'succeeded', ?, ?)`, e.instance("run_b"), e.clk.Now().Add(-23*time.Hour).UnixMicro(), e.clk.Now().UnixMicro())
	e.report("run_b", "idle", 1, e.limit(proto.LimitFiveHour, dur(3*time.Hour)))
	if sw := e.switchOf("run_b"); sw.State != SwitchFellBack || sw.Reason != SwitchBudgetSpent {
		t.Fatalf("budget spent: %+v", sw)
	}
	e.budget = 0
	e.running("run_z", "worker", 1)
	e.report("run_z", "idle", 1, e.limit(proto.LimitFiveHour, dur(3*time.Hour)))
	if sw := e.switchOf("run_z"); sw.State != SwitchFellBack || sw.Reason != SwitchBudgetSpent {
		t.Fatalf("budget 0: %+v", sw)
	}

	e.budget, e.failoverOn = 6, false
	e.running("run_f", "worker", 1)
	e.report("run_f", "idle", 1, e.limit(proto.LimitFiveHour, dur(3*time.Hour)))
	if n := e.switches("run_f"); n != 0 {
		t.Fatalf("failover off decided %d switches", n)
	}
	// A run without a finished turn has nothing to resume: it waits.
	e.failoverOn = true
	e.running("run_t", "worker", 0)
	e.report("run_t", "idle", 0, e.limit(proto.LimitFiveHour, dur(3*time.Hour)))
	if n := e.switches("run_t"); n != 0 {
		t.Fatalf("a run without a turn decided %d switches", n)
	}
}

// The target is re-resolved at the resume (§3.4 step 4, §3.8): a target unbound since the park gives way to the next
// eligible account; with none left the switch falls back, the wait loses its target and waits for the reset, and
// control hears the ADR-0079 "parked" message.
func TestAccountSwitchTargetGone(t *testing.T) {
	e := newSwitchEnv(t)
	e.running("run_d", "worker", 2)
	sw := e.armAndPark("run_d")
	e.exec(`DELETE FROM space_accounts WHERE space_id = 'spc_a' AND account_id = 'acc_2'`)
	e.exec(`INSERT INTO space_accounts (space_id, account_id, harness, created_at, created_by) VALUES ('spc_a', 'acc_3', 'claude-code', 1, 't')`)
	if n := e.due(); n != 1 {
		t.Fatalf("resumed %d", n)
	}
	_, p := e.launchOf(e.instance("run_d"))
	if p.AccountRef != "acc_3" || p.SessionID != "ses_run_d" {
		t.Fatalf("launch %+v", p)
	}
	if got := e.switchOf("run_d"); got.ID != sw.ID || got.State != SwitchResumed || got.ToAccountID != "acc_3" {
		t.Fatalf("switch %+v", got)
	}
	if e.count(`SELECT count(*) FROM audit_events WHERE action = 'account.switch_retargeted' AND target_id = 'run_d'`) != 1 {
		t.Fatal("re-targeting not audited")
	}

	e.running("run_e", "worker", 2)
	e.armAndPark("run_e")
	e.exec(`DELETE FROM accounts WHERE id = 'acc_3'`)
	before := len(e.events)
	if n := e.due(); n != 0 {
		t.Fatalf("resumed %d with no account left", n)
	}
	got := e.switchOf("run_e")
	w := e.waitOf("run_e")
	if got.State != SwitchFellBack || got.Reason != SwitchNoEligibleAccount || w.SwitchToAccountID != "" || w.State != WaitWaiting ||
		w.ResumeAt != e.clk.Now().Add(3*time.Hour+2*time.Minute).UnixMicro() {
		t.Fatalf("fallback: switch %+v wait %+v", got, w)
	}
	if len(e.events) != before+1 || e.events[before].Change != LimitWaitParked || e.events[before].Switch != nil {
		t.Fatalf("control events %+v", e.events[before:])
	}
}

// A switch's resume that ends before its first turn fails the switch (resume_failed); ADR-0079's fresh retry starts
// the instance again at once, on the account it moved to, in a fresh session with its original prompt.
func TestAccountSwitchResumeFailed(t *testing.T) {
	e := newSwitchEnv(t)
	e.running("run_r", "worker", 2)
	sw := e.armAndPark("run_r")
	e.clk.Advance(limitRetrySoon)
	e.due()
	newID, _ := e.launchOf(e.instance("run_r"))
	e.exec(`UPDATE runs SET observed = 'running' WHERE id = ?`, newID)
	e.endRun(newID, proto.RunFailed, "error", 0)
	if got := e.switchOf("run_r"); got.ID != sw.ID || got.State != SwitchFailed || got.Reason != SwitchResumeFailed {
		t.Fatalf("switch %+v", got)
	}
	retry := e.waitOf(newID)
	if retry.SwitchToAccountID != "acc_2" || retry.AccountSwitchID != "" || retry.ResumeAt != e.clk.Now().UnixMicro() {
		t.Fatalf("retry wait %+v", retry)
	}
	if n := e.due(); n != 1 {
		t.Fatalf("retried %d", n)
	}
	_, p := e.launchOf(e.instance("run_r"))
	if p.AccountRef != "acc_2" || p.Resume || p.SessionID != "" || !strings.HasPrefix(p.Prompt, "do the task") || !strings.Contains(p.Prompt, switchFreshPrompt) {
		t.Fatalf("fresh retry %+v", p)
	}
	if e.metric("failed") != 1 {
		t.Fatalf("failed metric %v", e.metric("failed"))
	}
}

// Node offline (§3.8): a parked switch whose node has no valid lease is put off with node_offline and stays parked;
// the node's hello brings it forward and it resumes on the target.
func TestAccountSwitchNodeOffline(t *testing.T) {
	e := newSwitchEnv(t)
	e.running("run_o", "worker", 2)
	e.armAndPark("run_o")
	e.exec(`UPDATE execution_leases SET state = 'revoked' WHERE space_id = 'spc_a'`)
	e.clk.Advance(limitRetrySoon)
	if n := e.due(); n != 0 {
		t.Fatalf("resumed %d without a lease", n)
	}
	if w := e.waitOf("run_o"); w.LastError != WaitNodeOffline || w.State != WaitWaiting || w.SwitchToAccountID != "acc_2" {
		t.Fatalf("wait %+v", w)
	}
	if sw := e.switchOf("run_o"); sw.State != SwitchParked {
		t.Fatalf("switch %+v", sw)
	}
	e.exec(`UPDATE execution_leases SET state = 'active' WHERE space_id = 'spc_a'`)
	e.svc.wakeWaitsOnNode(e.ctx, "nod_1")
	if n := e.due(); n != 1 {
		t.Fatalf("resumed %d after the hello", n)
	}
	if _, p := e.launchOf(e.instance("run_o")); p.AccountRef != "acc_2" || !p.Resume {
		t.Fatalf("launch %+v", p)
	}
}

// The new account limited at once: a limit on the resumed run before its first turn fails its switch
// (limited_at_once) and is a new limit: another switch, to the next eligible account, keeping the session; with no
// account left, the next switch falls back and the run waits.
func TestAccountSwitchNewAccountLimitedAtOnce(t *testing.T) {
	e := newSwitchEnv(t)
	e.exec(`INSERT INTO space_accounts (space_id, account_id, harness, created_at, created_by) VALUES ('spc_a', 'acc_3', 'claude-code', 1, 't')`)
	e.running("run_a", "worker", 2)
	first := e.armAndPark("run_a")
	e.clk.Advance(limitRetrySoon)
	e.due()
	r2, _ := e.launchOf(e.instance("run_a"))
	e.exec(`UPDATE runs SET observed = 'running' WHERE id = ?`, r2)
	e.report(r2, "idle", 0, e.limit(proto.LimitSevenDay, dur(72*time.Hour)))
	if got, _ := e.s.R().GetAccountSwitch(e.ctx, first.ID); got.State != SwitchFailed || got.Reason != SwitchLimitedAtOnce {
		t.Fatalf("first switch %+v", got)
	}
	second := e.switchOf(r2)
	if second.State != SwitchParked || second.FromAccountID != "acc_2" || second.ToAccountID != "acc_3" {
		t.Fatalf("second switch %+v", second)
	}
	if w := e.waitOf(r2); w.SessionID != "ses_run_a" || w.SwitchToAccountID != "acc_3" {
		t.Fatalf("second wait %+v", w)
	}
	e.endRun(r2, proto.RunExited, "stopped", 0)
	e.clk.Advance(limitRetrySoon)
	e.due()
	r3, p := e.launchOf(e.instance("run_a"))
	if p.AccountRef != "acc_3" || p.SessionID != "ses_run_a" || !p.Resume {
		t.Fatalf("third run's launch %+v", p)
	}
	// acc_3 is limited at once too; acc_1's and acc_2's holds are open: no account left.
	e.exec(`UPDATE runs SET observed = 'running' WHERE id = ?`, r3)
	e.report(r3, "idle", 0, e.limit(proto.LimitFiveHour, dur(5*time.Hour)))
	if got := e.switchOf(r3); got.State != SwitchFellBack || got.Reason != SwitchNoEligibleAccount {
		t.Fatalf("third switch %+v", got)
	}
	if got, _ := e.s.R().GetAccountSwitch(e.ctx, second.ID); got.State != SwitchFailed || got.Reason != SwitchLimitedAtOnce {
		t.Fatalf("second switch %+v", got)
	}
}

// Control is switched like a worker (§3.8): its wait resumes first, on the target, and takes the space's single
// control slot back; its park and resume carry the switch (control itself is never told).
func TestAccountSwitchControlRun(t *testing.T) {
	e := newSwitchEnv(t)
	e.running("run_c", "control", 3)
	e.report("run_c", "idle", 3, e.limit(proto.LimitFiveHour, dur(3*time.Hour)))
	cw := e.waitOf("run_c")
	if cw.SwitchToAccountID != "acc_2" || cw.ReleaseAt.Valid {
		t.Fatalf("control's switch wait %+v", cw)
	}
	e.endRun("run_c", proto.RunExited, "stopped", 3)
	e.clk.Advance(limitRetrySoon)
	if n := e.due(); n != 1 {
		t.Fatalf("resumed %d", n)
	}
	newID, p := e.launchOf(e.instance("run_c"))
	if p.Kind != "control" || p.AccountRef != "acc_2" || p.SessionID != "ses_run_c" {
		t.Fatalf("control's launch %+v", p)
	}
	if sw := e.switchOf("run_c"); sw.State != SwitchResumed || sw.NewRunID != newID || sw.RunKind != "control" {
		t.Fatalf("control's switch %+v", sw)
	}
}

// An armed switch is cancelled when its limit clears (limit_cleared) or somebody stops the run (run_stopped); a run
// that stays busy past switchArmMax is interrupted and parked for its switch by the job.
func TestAccountSwitchArmedSettles(t *testing.T) {
	e := newSwitchEnv(t)
	e.running("run_c", "worker", 1)
	l := e.limit(proto.LimitFiveHour, dur(3*time.Hour))
	e.report("run_c", "working", 1, l)
	e.report("run_c", "working", 1, &proto.RunLimit{Kind: l.Kind, Since: l.Since, Cleared: true})
	if sw := e.switchOf("run_c"); sw.State != SwitchCancelled || sw.Reason != SwitchLimitCleared {
		t.Fatalf("cleared: %+v", sw)
	}

	e.running("run_s", "worker", 1)
	e.report("run_s", "working", 1, e.limit(proto.LimitFiveHour, dur(3*time.Hour)))
	e.exec(`UPDATE runs SET desired = 'stopped' WHERE id = 'run_s'`)
	e.endRun("run_s", proto.RunExited, "stopped", 1)
	if sw := e.switchOf("run_s"); sw.State != SwitchCancelled || sw.Reason != SwitchRunStopped {
		t.Fatalf("stopped: %+v", sw)
	}
	if e.count(`SELECT count(*) FROM limit_waits WHERE run_id = 'run_s'`) != 0 {
		t.Fatal("a run somebody stopped was parked")
	}

	e.running("run_b", "worker", 1)
	e.report("run_b", "working", 1, e.limit(proto.LimitFiveHour, dur(3*time.Hour)))
	e.clk.Advance(switchArmMax - time.Second)
	e.due()
	if sw := e.switchOf("run_b"); sw.State != SwitchPending {
		t.Fatalf("parked before the cap: %+v", sw)
	}
	e.clk.Advance(time.Second)
	if n := e.due(); n != 1 {
		t.Fatalf("acted on %d at the cap", n)
	}
	if sw := e.switchOf("run_b"); sw.State != SwitchParked {
		t.Fatalf("at the cap: %+v", sw)
	}
	if r := e.observed("run_b"); r.Desired != "stopped" || e.count(`SELECT count(*) FROM node_commands WHERE run_id = 'run_b' AND kind = ?`, proto.CmdStop) != 1 {
		t.Fatal("busy run not interrupted at the cap")
	}
	if e.metric("cancelled") != 2 {
		t.Fatalf("cancelled metric %v", e.metric("cancelled"))
	}
}

// A run that ends at its limit by itself (the error path ended the process) is decided at its end: parked for a
// switch, due at once.
func TestAccountSwitchRunEndedAtLimit(t *testing.T) {
	e := newSwitchEnv(t)
	e.running("run_x", "worker", 2)
	r := e.observed("run_x")
	reset := e.clk.Now().Add(3 * time.Hour)
	if _, err := e.svc.applyReport(e.ctx, "nod_1", proto.RunReport{RunID: "run_x", SpaceID: "spc_a", Generation: 1, State: proto.RunExited, Reason: "exited",
		Turn: 2, SessionID: r.SessionID, Limit: &proto.RunLimit{Kind: proto.LimitFiveHour, ResetsAt: &reset, Since: e.clk.Now()}, ObservedAt: e.clk.Now()}); err != nil {
		t.Fatal(err)
	}
	sw := e.switchOf("run_x")
	w := e.waitOf("run_x")
	if sw.State != SwitchParked || w.AccountSwitchID != sw.ID || w.SwitchToAccountID != "acc_2" || w.ResumeAt != e.clk.Now().UnixMicro() {
		t.Fatalf("switch %+v wait %+v", sw, w)
	}
	if n := e.due(); n != 1 {
		t.Fatalf("resumed %d", n)
	}
	if got := e.switchOf("run_x"); got.State != SwitchResumed {
		t.Fatalf("switch %+v", got)
	}
}

// Due waits resume control first, then account switches, then the oldest (§3.4 step 4).
func TestAccountSwitchResumeOrder(t *testing.T) {
	e := newSwitchEnv(t)
	ins := func(id, kind, to string, created int64) {
		e.exec(`INSERT INTO runs (id, space_id, node_id, generation, kind, role, harness, instance_id, desired, observed, launch_json, created_at, created_by,
			updated_at) VALUES (?, 'spc_a', 'nod_1', 1, ?, 'implementor', 'claude-code', ?, 'stopped', 'exited', '{}', 0, 't', 0)`, id, kind, e.instance(id))
		e.exec(`INSERT INTO limit_waits (id, space_id, run_id, run_kind, role, node_id, instance_id, harness, launch_json, resume_at, switch_to_account_id,
			created_at, updated_at) VALUES (?, 'spc_a', ?, ?, 'implementor', 'nod_1', ?, 'claude-code', '{}', 0, ?, ?, ?)`, "lwt_"+id, id, kind, e.instance(id), to,
			created, created)
	}
	ins("run_old", "worker", "", 1)
	ins("run_sw", "worker", "acc_2", 2)
	ins("run_ctl", "control", "", 3)
	rows, err := e.s.R().ListDueLimitWaits(e.ctx, db.ListDueLimitWaitsParams{ResumeAt: e.clk.Now().UnixMicro(), Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, w := range rows {
		got = append(got, w.RunID)
	}
	if strings.Join(got, ",") != "run_ctl,run_sw,run_old" {
		t.Fatalf("order %v", got)
	}
}

// A parked switch whose wait is given up (an operator releases it) is cancelled with the wait's reason.
func TestAccountSwitchWaitReleased(t *testing.T) {
	e := newSwitchEnv(t)
	e.running("run_q", "worker", 2)
	e.exec(`UPDATE execution_leases SET state = 'revoked' WHERE space_id = 'spc_a'`)
	sw := e.armAndPark("run_q")
	w := e.waitOf("run_q")
	if _, err := e.svc.ReleaseLimitWait(e.ctx, holdActor, "spc_a", w.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.s.R().GetAccountSwitch(e.ctx, sw.ID); got.State != SwitchCancelled || got.Reason != WaitReleasedByOp {
		t.Fatalf("switch %+v", got)
	}
}

// The target must still be in the run's role pool at the resume: an operator who takes it out of the pool moves the
// switch to the pool's next account (a role pool never widens).
func TestAccountSwitchTargetLeftRolePool(t *testing.T) {
	e := newSwitchEnv(t)
	e.exec(`INSERT INTO space_accounts (space_id, account_id, harness, created_at, created_by) VALUES ('spc_a', 'acc_3', 'claude-code', 1, 't')`)
	e.running("run_p", "worker", 2)
	if sw := e.armAndPark("run_p"); sw.ToAccountID != "acc_2" {
		t.Fatalf("target %s", sw.ToAccountID)
	}
	e.exec(`INSERT INTO space_role_accounts (space_id, role, account_id, priority, created_at, created_by) VALUES ('spc_a', 'implementor', 'acc_1', 100, 0, 't'),
		('spc_a', 'implementor', 'acc_3', 100, 0, 't')`)
	e.clk.Advance(limitRetrySoon)
	if n := e.due(); n != 1 {
		t.Fatalf("resumed %d", n)
	}
	if _, p := e.launchOf(e.instance("run_p")); p.AccountRef != "acc_3" {
		t.Fatalf("resumed on %s, want the role pool's acc_3", p.AccountRef)
	}
}

// A space with several accounts and no default still switches (the pool is resolved from the limited account).
func TestAccountSwitchNoDefaultAccount(t *testing.T) {
	e := newSwitchEnv(t)
	e.exec(`UPDATE space_accounts SET is_default = 0 WHERE space_id = 'spc_a'`)
	e.running("run_n", "worker", 1)
	e.report("run_n", "working", 1, e.limit(proto.LimitFiveHour, dur(3*time.Hour)))
	if sw := e.switchOf("run_n"); sw.State != SwitchPending || sw.ToAccountID != "acc_2" {
		t.Fatalf("switch %+v", sw)
	}
}

// Failover turned off while a switch is armed: at the idle point the switch falls back (failover_off), the run stays
// alive at its limit as ADR-0079 keeps it, and control hears it is kept alive.
func TestAccountSwitchFailoverOffWhileArmed(t *testing.T) {
	e := newSwitchEnv(t)
	e.running("run_f", "worker", 1)
	e.report("run_f", "working", 1, e.limit(proto.LimitFiveHour, dur(3*time.Hour)))
	e.failoverOn = false
	e.report("run_f", "idle", 1, nil)
	if sw := e.switchOf("run_f"); sw.State != SwitchFellBack || sw.Reason != SwitchFailoverOff {
		t.Fatalf("switch %+v", sw)
	}
	if r := e.observed("run_f"); r.Desired != "running" || e.count(`SELECT count(*) FROM limit_waits WHERE run_id = 'run_f'`) != 0 {
		t.Fatal("the run was parked with failover off")
	}
	if strings.Join(e.notices, ",") != "run_f" {
		t.Fatalf("control notices %v", e.notices)
	}
}

// A failed switch's fresh retry has no limited account to resolve from; in a space without a default account it still
// keeps the account the switch moved to (review of PR 159).
func TestAccountSwitchResumeFailedNoDefault(t *testing.T) {
	e := newSwitchEnv(t)
	e.exec(`UPDATE space_accounts SET is_default = 0 WHERE space_id = 'spc_a'`)
	e.running("run_r", "worker", 2)
	e.armAndPark("run_r")
	e.clk.Advance(limitRetrySoon)
	e.due()
	newID, _ := e.launchOf(e.instance("run_r"))
	e.exec(`UPDATE runs SET observed = 'running' WHERE id = ?`, newID)
	e.endRun(newID, proto.RunFailed, "error", 0)
	if n := e.due(); n != 1 {
		t.Fatalf("retried %d: wait %+v", n, e.waitOf(newID))
	}
	if _, p := e.launchOf(e.instance("run_r")); p.AccountRef != "acc_2" || p.Resume {
		t.Fatalf("fresh retry %+v", p)
	}
	if w := e.waitOf(newID); w.SwitchToAccountID != "acc_2" {
		t.Fatalf("retry wait %+v", w)
	}
}

// The resume launches on the switch's target, not on what the launch would pick by itself: the target (acc_3) is not
// the pool's first choice and the limited account's hold has cleared before the resume, so ignoring the target would
// resume on acc_1.
func TestAccountSwitchResumeUsesTarget(t *testing.T) {
	e := newSwitchEnv(t)
	e.exec(`INSERT INTO space_accounts (space_id, account_id, harness, created_at, created_by) VALUES ('spc_a', 'acc_3', 'claude-code', 1, 't')`)
	e.hold("ulm_b", "spc_c", "acc_2", "", proto.LimitSevenDay, dur(48*time.Hour), nil)
	e.running("run_t", "worker", 2)
	if sw := e.armAndPark("run_t"); sw.ToAccountID != "acc_3" {
		t.Fatalf("target %s", sw.ToAccountID)
	}
	e.exec(`UPDATE usage_limits SET cleared_at = 1 WHERE account_id IN ('acc_1', 'acc_2')`)
	if n := e.due(); n != 1 {
		t.Fatalf("resumed %d", n)
	}
	if _, p := e.launchOf(e.instance("run_t")); p.AccountRef != "acc_3" || !p.Resume {
		t.Fatalf("launch %+v", p)
	}
}

// When the target is gone at the resume, the limited account is never the replacement, even though its hold has
// cleared and it is the space's default: the pool's next account is.
func TestAccountSwitchRetargetAvoidsLimitedAccount(t *testing.T) {
	e := newSwitchEnv(t)
	e.exec(`INSERT INTO space_accounts (space_id, account_id, harness, created_at, created_by) VALUES ('spc_a', 'acc_3', 'claude-code', 1, 't')`)
	e.running("run_g", "worker", 2)
	if sw := e.armAndPark("run_g"); sw.ToAccountID != "acc_2" {
		t.Fatalf("target %s", sw.ToAccountID)
	}
	e.exec(`DELETE FROM space_accounts WHERE space_id = 'spc_a' AND account_id = 'acc_2'`)
	e.exec(`UPDATE usage_limits SET cleared_at = 1 WHERE account_id = 'acc_1'`)
	if n := e.due(); n != 1 {
		t.Fatalf("resumed %d", n)
	}
	if _, p := e.launchOf(e.instance("run_g")); p.AccountRef != "acc_3" {
		t.Fatalf("resumed on %s, want acc_3", p.AccountRef)
	}
}

// The daily budget is a rolling 24 h window: a switch older than that no longer counts.
func TestAccountSwitchBudgetWindowRolls(t *testing.T) {
	e := newSwitchEnv(t)
	e.budget = 1
	e.running("run_b", "worker", 1)
	e.exec(`INSERT INTO account_switches (id, space_id, instance_id, role, run_kind, harness, from_run_id, from_account_id, to_account_id, cause, limit_kind,
		state, created_at, updated_at) VALUES ('asw_old', 'spc_a', ?, 'implementor', 'worker', 'claude-code', 'run_prev', 'acc_2', 'acc_1', 'usage_limit',
		'five_hour', 'succeeded', ?, ?)`, e.instance("run_b"), e.clk.Now().Add(-23*time.Hour).UnixMicro(), e.clk.Now().UnixMicro())
	e.report("run_b", "working", 1, e.limit(proto.LimitFiveHour, dur(3*time.Hour)))
	if sw := e.switchOf("run_b"); sw.State != SwitchFellBack || sw.Reason != SwitchBudgetSpent {
		t.Fatalf("inside the window: %+v", sw)
	}
	e.clk.Advance(2 * time.Hour) // the old switch is now 25 h old
	e.running("run_c", "worker", 1)
	e.exec(`UPDATE runs SET instance_id = ? WHERE id = 'run_c'`, e.instance("run_b"))
	e.report("run_c", "working", 1, e.limit(proto.LimitFiveHour, dur(3*time.Hour)))
	if sw := e.switchOf("run_c"); sw.State != SwitchPending {
		t.Fatalf("after the window rolled: %+v", sw)
	}
}

// An armed switch whose run already has a wait can't park it: the switch is cancelled (not left armed) and control
// hears the run is kept alive.
func TestAccountSwitchParkWithExistingWaitCancels(t *testing.T) {
	e := newSwitchEnv(t)
	e.running("run_w", "worker", 2)
	e.report("run_w", "working", 2, e.limit(proto.LimitFiveHour, dur(3*time.Hour)))
	e.exec(`INSERT INTO limit_waits (id, space_id, run_id, run_kind, role, node_id, instance_id, harness, launch_json, resume_at, created_at, updated_at)
		VALUES ('lwt_pre', 'spc_a', 'run_w', 'worker', 'implementor', 'nod_1', ?, 'claude-code', '{}', ?, 0, 0)`, e.instance("run_w"), e.clk.Now().Add(24*time.Hour).UnixMicro())
	e.report("run_w", "idle", 2, nil)
	if sw := e.switchOf("run_w"); sw.State != SwitchCancelled || sw.Reason != WaitSuperseded {
		t.Fatalf("switch %+v", sw)
	}
	if r := e.observed("run_w"); r.Desired != "running" {
		t.Fatal("run stopped without a wait of its own")
	}
	if strings.Join(e.notices, ",") != "run_w" {
		t.Fatalf("control notices %v", e.notices)
	}
}

// The keep-alive job leaves a run with an armed switch to the switch (its idle point or switchArmMax).
func TestAccountSwitchKeepAliveSkipsArmed(t *testing.T) {
	e := newSwitchEnv(t)
	e.keepMax = time.Minute
	e.running("run_k", "worker", 2)
	e.report("run_k", "working", 2, e.limit(proto.LimitFiveHour, dur(3*time.Hour)))
	e.clk.Advance(2 * time.Minute) // past the keep-alive, inside switchArmMax
	if n, err := e.svc.parkDueRuns(e.ctx, e.clk.Now()); err != nil || n != 0 {
		t.Fatalf("parked %d: %v", n, err)
	}
	if sw := e.switchOf("run_k"); sw.State != SwitchPending {
		t.Fatalf("switch %+v", sw)
	}
	if r := e.observed("run_k"); r.Desired != "running" {
		t.Fatal("the keep-alive stopped a run with an armed switch")
	}
}

// A resumed run an operator stops before its first turn is a stopped run, not a failed --resume.
func TestAccountSwitchResumedRunStopped(t *testing.T) {
	e := newSwitchEnv(t)
	e.running("run_o", "worker", 2)
	e.armAndPark("run_o")
	e.due()
	newID, _ := e.launchOf(e.instance("run_o"))
	e.exec(`UPDATE runs SET observed = 'running', desired = 'stopped' WHERE id = ?`, newID)
	e.endRun(newID, proto.RunExited, "stopped", 0)
	if sw := e.switchOf("run_o"); sw.State != SwitchCancelled || sw.Reason != SwitchRunStopped {
		t.Fatalf("switch %+v", sw)
	}
}
