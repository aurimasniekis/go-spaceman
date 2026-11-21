package fleet

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/obs/metrics/metricstest"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// backEnv is switchEnv with switching back on (ADR-0105).
type backEnv struct {
	*switchEnv
	back bool
}

func newBackEnv(t *testing.T) *backEnv {
	t.Helper()
	e := &backEnv{switchEnv: newSwitchEnv(t), back: true}
	e.svc.opts.UsageLimitSettings = func(context.Context, *db.Queries, string) UsageLimitSettings {
		return UsageLimitSettings{Failover: e.failoverOn, KeepAliveMax: e.keepMax, WaitIfResetWithin: e.within, SwitchMaxPerDay: e.budget,
			SwitchBack: e.back}
	}
	return e
}

// resumeAndVerify resumes the due switch wait of run's instance and reports the new run's first turn: the switch that
// started it is verified. It returns the new run.
func (e *backEnv) resumeAndVerify(run string) string {
	e.t.Helper()
	e.clk.Advance(limitRetrySoon)
	if n := e.due(); n != 1 {
		e.t.Fatalf("resumed %d", n)
	}
	newID, _ := e.launchOf(e.instance(run))
	e.inst[newID] = e.instance(run)
	e.exec(`UPDATE runs SET observed = 'running' WHERE id = ?`, newID)
	e.report(newID, "working", 0, nil)
	e.report(newID, "idle", 1, nil)
	return newID
}

// movedAway seeds worker run_w with task tsk_1 on acc_1, limits it there (5-hour, reset in 3 h) and moves it to
// acc_2 (ADR-0090). It returns the run on acc_2, idle after its first turn.
func (e *backEnv) movedAway() string {
	e.t.Helper()
	e.running("run_w", "worker", 2)
	e.task("tsk_1", "run_w", "running")
	e.armAndPark("run_w")
	newID := e.resumeAndVerify("run_w")
	if sw := e.switchOf("run_w"); sw.State != SwitchSucceeded || sw.ToAccountID != "acc_2" || sw.OriginAccountID != "acc_1" {
		e.t.Fatalf("switch away %+v", sw)
	}
	return newID
}

// backs counts the switch-backs from run.
func (e *backEnv) backs(run string) int {
	return e.count(`SELECT count(*) FROM account_switches WHERE from_run_id = ? AND cause = ?`, run, SwitchCauseSwitchBack)
}

// afterReset moves the clock past acc_1's reset (3 h after the limit) plus the resume slack.
func (e *backEnv) afterReset() {
	e.clk.Advance(3*time.Hour + limitResumeSlack)
}

// ADR-0105, the whole path: a run moved to acc_2 at acc_1's limit stays there while acc_1 is limited and while it is
// busy; at its first idle point after acc_1's reset (plus slack) it is parked with a switch-back wait due at once on
// acc_1 and stopped. The job resumes it on acc_1 with --resume of the same session and the switch-back note, the task
// follows, and its first turn marks the switch succeeded. It is audited, published and counted with its cause.
func TestSwitchBackAfterResetAtIdle(t *testing.T) {
	e := newBackEnv(t)
	run := e.movedAway()

	// acc_1 still limited: an idle point changes nothing.
	e.report(run, "working", 1, nil)
	e.report(run, "idle", 2, nil)
	if n := e.backs(run); n != 0 {
		t.Fatalf("switched back before the reset: %d", n)
	}
	// Just before the reset plus slack: still nothing.
	e.clk.Advance(3*time.Hour + limitResumeSlack - limitRetrySoon - time.Second)
	e.report(run, "working", 2, nil)
	e.report(run, "idle", 3, nil)
	if n := e.backs(run); n != 0 {
		t.Fatalf("switched back before the reset's slack: %d", n)
	}
	e.clk.Advance(time.Second)
	// Mid-turn: never, at its report or by the job.
	e.report(run, "working", 3, nil)
	e.due()
	e.clk.Advance(switchBackRecheck)
	e.due()
	if n := e.backs(run); n != 0 || e.observed(run).Desired != "running" {
		t.Fatal("a busy run was switched back")
	}
	e.report(run, "idle", 4, nil)
	sw := e.switchOf(run)
	if sw.Cause != SwitchCauseSwitchBack || sw.State != SwitchParked || sw.FromAccountID != "acc_2" || sw.ToAccountID != "acc_1" ||
		sw.OriginAccountID != "acc_1" || sw.LimitKind != "" || sw.ArmedUntil.Valid || sw.UsageLimitID != "" {
		t.Fatalf("switch-back %+v", sw)
	}
	w := e.waitOf(run)
	if w.Cause != WaitCauseSwitchBack || w.ResumeAt != e.clk.Now().UnixMicro() || w.SwitchToAccountID != "acc_1" || w.AccountSwitchID != sw.ID ||
		w.UsageLimitID != "" || w.TaskID != "tsk_1" || w.SessionID != "ses_run_w" || w.Attempts != 0 ||
		!w.ReleaseAt.Valid || w.ReleaseAt.Int64 != e.clk.Now().Add(DefaultLimitMaxWait).UnixMicro() {
		t.Fatalf("switch-back wait %+v", w)
	}
	if r := e.observed(run); r.Desired != "stopped" || e.count(`SELECT count(*) FROM node_commands WHERE run_id = ? AND kind = ?`, run, proto.CmdStop) != 1 {
		t.Fatalf("run not stopped: %+v", r)
	}
	if ev := e.events[len(e.events)-1]; ev.Change != LimitWaitParked || ev.Switch == nil || ev.Switch.Cause != SwitchCauseSwitchBack {
		t.Fatalf("park event %+v", ev)
	}

	e.endRun(run, proto.RunExited, "stopped", 4)
	e.clk.Advance(limitRetrySoon)
	if n := e.due(); n != 1 {
		t.Fatalf("resumed %d", n)
	}
	home, p := e.launchOf(e.instance("run_w"))
	if home == run || p.AccountRef != "acc_1" || !p.Resume || p.SessionID != "ses_run_w" || p.Prompt != switchBackPrompt {
		t.Fatalf("switch-back launch %s %+v", home, p)
	}
	if e.count(`SELECT count(*) FROM tasks WHERE id = 'tsk_1' AND assignee_run = ?`, home) != 1 {
		t.Fatal("task not re-pointed")
	}
	if sw = e.switchOf(run); sw.State != SwitchResumed || sw.NewRunID != home {
		t.Fatalf("resumed switch-back %+v", sw)
	}
	if ev := e.events[len(e.events)-1]; ev.Change != LimitWaitResumed || ev.Switch == nil || ev.Switch.Cause != SwitchCauseSwitchBack || ev.Wait.Cause != WaitCauseSwitchBack {
		t.Fatalf("resume event %+v", ev)
	}
	var manifest string
	_ = e.s.ReadDB().QueryRowContext(e.ctx, `SELECT manifest_json FROM runs WHERE id = ?`, home).Scan(&manifest)
	if !strings.Contains(manifest, `"switch_back_resume"`) || !strings.Contains(manifest, sw.ID) {
		t.Fatalf("manifest %s", manifest)
	}
	e.inst[home] = e.instance("run_w")
	e.exec(`UPDATE runs SET observed = 'running' WHERE id = ?`, home)
	e.report(home, "working", 0, nil)
	e.report(home, "idle", 1, nil)
	if sw = e.switchOf(run); sw.State != SwitchSucceeded {
		t.Fatalf("switch-back %+v, want succeeded", sw)
	}
	// The API view carries the cause and the origin (what the CLI and the web show).
	var view *AccountSwitch
	if all, err := e.svc.AccountSwitches(e.ctx, "spc_a"); err != nil {
		t.Fatal(err)
	} else {
		for i := range all {
			if all[i].ID == sw.ID {
				view = &all[i]
			}
		}
	}
	if view == nil || view.Cause != "switch_back" || view.OriginAccountID != "acc_1" || view.ToAccountID != "acc_1" || view.LimitKind != "" {
		t.Fatalf("API view of the switch-back %+v", view)
	}
	// Home again: nothing more to do.
	e.clk.Advance(time.Minute)
	e.report(home, "working", 1, nil)
	e.report(home, "idle", 2, nil)
	if n := e.backs(home); n != 0 {
		t.Fatalf("a run on its origin was switched again: %d", n)
	}
	if n := e.count(`SELECT count(*) FROM audit_events WHERE action = 'account.switch_succeeded' AND details_json LIKE '%"cause":"switch_back"%'`); n != 1 {
		t.Fatalf("switch-back success audited %d times", n)
	}
	if n := e.count(`SELECT count(*) FROM event_outbox WHERE subject LIKE '%account_switch.completed%' AND payload_json LIKE '%"cause":"switch_back"%'`); n != 1 {
		t.Fatalf("switch-back completion published %d times", n)
	}
	for _, c := range []struct {
		cause, result string
		want          float64
	}{{"switch_back", "started", 1}, {"switch_back", "succeeded", 1}, {"usage_limit", "started", 1}, {"usage_limit", "succeeded", 1}} {
		if v := metricstest.Value(e.t, e.m, "spaceman_account_switches_total", "cause", c.cause, "result", c.result); v != c.want {
			t.Errorf("metric %s/%s = %v, want %v", c.cause, c.result, v, c.want)
		}
	}
}

// The limit-wait job switches back a run that sits idle through the reset: it wakes at the origin's reset plus slack
// (not before), and looks again at most every switchBackRecheck.
func TestSwitchBackByTheJobAtTheReset(t *testing.T) {
	e := newBackEnv(t)
	run := e.movedAway()
	var resets int64
	if err := e.s.ReadDB().QueryRowContext(e.ctx, `SELECT resets_at FROM usage_limits WHERE account_id = 'acc_1' AND cleared_at IS NULL`).Scan(&resets); err != nil {
		t.Fatal(err)
	}
	readyAt := time.UnixMicro(resets).Add(limitResumeSlack)
	// Off the recheck grid, so only a wake at the reset itself lands on readyAt.
	e.clk.Advance(7 * time.Minute)
	for i := 0; i < 100; i++ {
		at := e.nextAt()
		if at.After(readyAt) {
			t.Fatalf("next at %v, after the origin's reset %v", at, readyAt)
		}
		if at.After(e.clk.Now()) {
			if d := at.Sub(e.clk.Now()); d > switchBackRecheck {
				t.Fatalf("next in %v, more than the recheck", d)
			}
			e.clk.Set(at)
		}
		e.due()
		if e.backs(run) > 0 {
			break
		}
	}
	sw := e.switchOf(run)
	if sw.Cause != SwitchCauseSwitchBack || sw.State != SwitchParked || sw.CreatedAt != readyAt.UnixMicro() {
		t.Fatalf("switch-back %+v at %v, want parked at %v", sw, time.UnixMicro(sw.CreatedAt), readyAt)
	}
}

// What keeps a run on the account it was moved to (ADR-0105 decisions 1, 3, 4, 6 and 7): for each, an idle point after
// the reset doesn't switch it back; once the obstacle is gone (where it can be), the next idle point does.
func TestSwitchBackNotWhen(t *testing.T) {
	for _, c := range []struct {
		name        string
		block, undo func(e *backEnv, run string)
	}{
		{"origin held with an unknown reset", func(e *backEnv, _ string) {
			e.hold("ulm_x", "spc_a", "acc_1", "", proto.LimitUnknown, nil, nil)
		}, func(e *backEnv, _ string) { e.exec(`UPDATE usage_limits SET cleared_at = 1 WHERE id = 'ulm_x'`) }},
		{"origin held again in another space", func(e *backEnv, _ string) {
			e.hold("ulm_x", "spc_b", "acc_1", "", proto.LimitSevenDay, dur(24*time.Hour), nil)
		}, func(e *backEnv, _ string) { e.exec(`UPDATE usage_limits SET cleared_at = 1 WHERE id = 'ulm_x'`) }},
		{"origin out of the role's pool", func(e *backEnv, _ string) {
			e.exec(`INSERT INTO space_role_accounts (space_id, role, account_id, created_at, created_by) VALUES ('spc_a', 'implementor', 'acc_2', 0, 't')`)
		}, func(e *backEnv, _ string) {
			e.exec(`INSERT INTO space_role_accounts (space_id, role, account_id, created_at, created_by) VALUES ('spc_a', 'implementor', 'acc_1', 0, 't')`)
		}},
		{"origin disabled, another account free", func(e *backEnv, _ string) {
			// Never to another account of the pool instead: only the origin comes back.
			e.exec(`INSERT INTO space_accounts (space_id, account_id, harness, created_at, created_by) VALUES ('spc_a', 'acc_3', 'claude-code', 2, 't')`)
			e.exec(`UPDATE accounts SET enabled = 0 WHERE id = 'acc_1'`)
		},
			func(e *backEnv, _ string) { e.exec(`UPDATE accounts SET enabled = 1 WHERE id = 'acc_1'`) }},
		{"origin unbound", func(e *backEnv, _ string) {
			e.exec(`DELETE FROM space_accounts WHERE space_id = 'spc_a' AND account_id = 'acc_1'`)
		},
			func(e *backEnv, _ string) {
				e.exec(`INSERT INTO space_accounts (space_id, account_id, harness, created_at, created_by) VALUES ('spc_a', 'acc_1', 'claude-code', 0, 't')`)
			}},
		{"setting off", func(e *backEnv, _ string) { e.back = false }, func(e *backEnv, _ string) { e.back = true }},
		{"failover off", func(e *backEnv, _ string) { e.failoverOn = false }, func(e *backEnv, _ string) { e.failoverOn = true }},
		{"daily budget spent", func(e *backEnv, _ string) { e.budget = 1 }, func(e *backEnv, _ string) { e.budget = 6 }},
		{"blocked dialog open", func(e *backEnv, run string) {
			e.exec(`INSERT INTO run_dialogs (id, run_id, space_id, node_id, dialog, signal, outcome, detected_at, created_at)
				VALUES ('dlg_1', ?, 'spc_a', 'nod_1', 'unknown', 'quiet', 'blocked', 0, 0)`, run)
		}, func(e *backEnv, _ string) { e.exec(`UPDATE run_dialogs SET cleared_at = 1 WHERE id = 'dlg_1'`) }},
		{"terminal taken over", func(e *backEnv, run string) {
			e.exec(`INSERT INTO run_takeovers (id, run_id, space_id, holder, started_at, expires_at) VALUES ('tko_1', ?, 'spc_a', 'usr_1', 0, ?)`,
				run, e.clk.Now().Add(time.Hour).UnixMicro())
		}, func(e *backEnv, _ string) { e.exec(`UPDATE run_takeovers SET ended_at = 1 WHERE id = 'tko_1'`) }},
		{"active switch of the instance", func(e *backEnv, run string) {
			// An older row in an active state: the newest switch is still the usage-limit one, so only the active-switch guard holds it.
			e.exec(`INSERT INTO account_switches (id, space_id, instance_id, role, run_kind, harness, from_run_id, from_account_id, cause, limit_kind, state,
				created_at, updated_at) VALUES ('asw_act', 'spc_a', ?, 'implementor', 'worker', 'claude-code', 'run_old', 'acc_1', 'operator', '', 'pending', 0, 0)`,
				e.instance(run))
		}, func(e *backEnv, _ string) {
			e.exec(`UPDATE account_switches SET state = 'cancelled' WHERE id = 'asw_act'`)
		}},
		{"active limit wait of the instance", func(e *backEnv, run string) {
			e.exec(`INSERT INTO limit_waits (id, space_id, run_id, run_kind, role, node_id, instance_id, harness, launch_json, resume_at, state, created_at, updated_at)
				VALUES ('lwt_act', 'spc_a', ?, 'worker', 'implementor', 'nod_1', ?, 'claude-code', '{}', 0, 'waiting', 0, 0)`, run, e.instance(run))
		}, func(e *backEnv, _ string) { e.exec(`DELETE FROM limit_waits WHERE id = 'lwt_act'`) }},
		{"lease revoked", func(e *backEnv, _ string) {
			e.exec(`UPDATE execution_leases SET state = 'revoked' WHERE space_id = 'spc_a'`)
		},
			func(e *backEnv, _ string) {
				e.exec(`UPDATE execution_leases SET state = 'active' WHERE space_id = 'spc_a'`)
			}},
		{"lease expired", func(e *backEnv, _ string) {
			e.exec(`UPDATE execution_leases SET expires_at = ? WHERE space_id = 'spc_a'`, e.us(-time.Minute))
		},
			func(e *backEnv, _ string) {
				e.exec(`UPDATE execution_leases SET expires_at = ? WHERE space_id = 'spc_a'`, e.us(1000*time.Hour))
			}},
		{"lease on another node", func(e *backEnv, _ string) {
			e.exec(`INSERT INTO nodes (id, name, state, created_at, created_by, updated_at) VALUES ('nod_2', 'n2', 'active', 0, 't', 0)`)
			e.exec(`UPDATE execution_leases SET node_id = 'nod_2' WHERE space_id = 'spc_a'`)
		}, func(e *backEnv, _ string) {
			e.exec(`UPDATE execution_leases SET node_id = 'nod_1' WHERE space_id = 'spc_a'`)
		}},
		{"space paused", func(e *backEnv, _ string) { e.exec(`UPDATE spaces SET state = 'paused' WHERE id = 'spc_a'`) },
			func(e *backEnv, _ string) { e.exec(`UPDATE spaces SET state = 'active' WHERE id = 'spc_a'`) }},
		{"moved for another cause", func(e *backEnv, _ string) {
			e.exec(`UPDATE account_switches SET cause = 'operator' WHERE from_run_id = 'run_w'`)
		}, func(e *backEnv, _ string) {
			e.exec(`UPDATE account_switches SET cause = 'usage_limit' WHERE from_run_id = 'run_w'`)
		}},
		{"relaunched on another account since", func(e *backEnv, run string) {
			e.exec(`UPDATE runs SET launch_json = json_set(launch_json, '$.account_ref', 'acc_3') WHERE id = ?`, run)
		}, func(e *backEnv, run string) {
			e.exec(`UPDATE runs SET launch_json = json_set(launch_json, '$.account_ref', 'acc_2') WHERE id = ?`, run)
		}},
		{"gateway run", func(e *backEnv, run string) {
			e.exec(`UPDATE runs SET launch_json = json_set(launch_json, '$.gateway', json('{"kind":"harbor-pilot","project_id":"prj_1"}')) WHERE id = ?`, run)
		}, nil},
		{"no session to resume", func(e *backEnv, run string) {
			e.exec(`UPDATE runs SET session_id = '' WHERE id = ?`, run)
		}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newBackEnv(t)
			run := e.movedAway()
			e.afterReset()
			c.block(e, run)
			e.report(run, "working", 1, nil)
			e.report(run, "idle", 2, nil)
			e.clk.Advance(switchBackRecheck)
			e.due()
			if n := e.backs(run); n != 0 || e.observed(run).Desired != "running" {
				t.Fatalf("switched back (%d)", n)
			}
			if c.undo == nil {
				return
			}
			c.undo(e, run)
			e.report(run, "working", 2, nil)
			e.report(run, "idle", 3, nil)
			if sw := e.switchOf(run); sw.Cause != SwitchCauseSwitchBack || sw.State != SwitchParked {
				t.Fatalf("not switched back once unblocked: %+v", sw)
			}
		})
	}
}

// switchBackTx itself refuses a run that is not between turns (the job reads its candidates before its transaction).
func TestSwitchBackTxRefusesABusyRun(t *testing.T) {
	e := newBackEnv(t)
	run := e.movedAway()
	e.afterReset()
	e.exec(`UPDATE runs SET status = 'working' WHERE id = ?`, run)
	err := e.s.W().TxNamed(e.ctx, "test", func(tx *store.Tx) error {
		r, err := tx.Q.GetRun(e.ctx, run)
		if err != nil {
			return err
		}
		moved, err := e.svc.switchBackTx(e.ctx, tx, r, "idle")
		if moved {
			t.Error("a busy run was switched back")
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := e.backs(run); n != 0 {
		t.Fatalf("switch-backs %d", n)
	}
}

// A switch-back's origin that can no longer take the run when the wait is claimed (limited again meanwhile): the
// switch falls back and the run resumes at once on the account it was on, told it wasn't moved.
func TestSwitchBackOriginGoneAtResume(t *testing.T) {
	e := newBackEnv(t)
	run := e.movedAway()
	e.afterReset()
	e.report(run, "working", 1, nil)
	e.report(run, "idle", 2, nil)
	if sw := e.switchOf(run); sw.State != SwitchParked {
		t.Fatalf("switch-back %+v", sw)
	}
	e.endRun(run, proto.RunExited, "stopped", 2)
	e.hold("ulm_y", "spc_a", "acc_1", "", proto.LimitUnknown, nil, nil)
	e.clk.Advance(limitRetrySoon)
	if n := e.due(); n != 1 {
		t.Fatalf("resumed %d", n)
	}
	next, p := e.launchOf(e.instance("run_w"))
	if next == run || p.AccountRef != "acc_2" || !p.Resume || p.SessionID != "ses_run_w" || p.Prompt != switchBackStayPrompt {
		t.Fatalf("stay launch %s %+v", next, p)
	}
	if sw := e.switchOf(run); sw.State != SwitchFellBack || sw.Reason != SwitchNoEligibleAccount {
		t.Fatalf("switch-back %+v, want fell_back", sw)
	}
	if w := e.waitOf(run); w.SwitchToAccountID != "" || w.State != WaitResumed || w.NewRunID != next {
		t.Fatalf("wait %+v", w)
	}
}

// A switch-back that falls back at the claim has already stopped and restarted the run: it counts toward the ping-pong
// guard and the daily switch budget like one that moved it.
func TestSwitchBackFellBackCounts(t *testing.T) {
	e := newBackEnv(t)
	run := e.movedAway()
	e.afterReset()
	e.report(run, "working", 1, nil)
	e.report(run, "idle", 2, nil)
	e.endRun(run, proto.RunExited, "stopped", 2)
	e.hold("ulm_y", "spc_a", "acc_1", "", proto.LimitUnknown, nil, nil)
	e.clk.Advance(limitRetrySoon)
	if n := e.due(); n != 1 {
		t.Fatalf("resumed %d", n)
	}
	if sw := e.switchOf(run); sw.State != SwitchFellBack {
		t.Fatalf("switch-back %+v, want fell_back", sw)
	}
	next, _ := e.launchOf(e.instance("run_w"))
	e.inst[next] = e.instance("run_w")
	e.exec(`UPDATE runs SET observed = 'running' WHERE id = ?`, next)
	e.report(next, "working", 0, nil)
	e.report(next, "idle", 1, nil)
	e.exec(`UPDATE usage_limits SET cleared_at = 1 WHERE id = 'ulm_y'`)
	// Within the guard: not again.
	e.report(next, "working", 1, nil)
	e.report(next, "idle", 2, nil)
	if n := e.backs(next); n != 0 {
		t.Fatalf("switched back within the guard of a fallen-back switch-back: %d", n)
	}
	// Past the guard the budget still holds: the switch away and the fallen-back switch-back are two of the day's.
	e.clk.Advance(switchBackMinGap)
	e.budget = 2
	e.report(next, "working", 2, nil)
	e.report(next, "idle", 3, nil)
	if n := e.backs(next); n != 0 {
		t.Fatalf("switched back with the budget spent by a fallen-back switch-back: %d", n)
	}
	e.budget = 3
	e.report(next, "working", 3, nil)
	e.report(next, "idle", 4, nil)
	if sw := e.switchOf(next); sw.Cause != SwitchCauseSwitchBack || sw.State != SwitchParked {
		t.Fatalf("not switched back with budget left: %+v", sw)
	}
}

// Spaces with switching back off don't use up the candidate page: a run of a space with it on is found however many
// runs of other spaces sort before it.
func TestSwitchBackCandidatesSkipSpacesWithItOff(t *testing.T) {
	e := newBackEnv(t)
	run := e.movedAway()
	e.svc.opts.UsageLimitSettings = func(_ context.Context, _ *db.Queries, space string) UsageLimitSettings {
		return UsageLimitSettings{Failover: true, KeepAliveMax: e.keepMax, WaitIfResetWithin: e.within, SwitchMaxPerDay: e.budget, SwitchBack: space == "spc_a"}
	}
	for _, id := range []string{"run_000", "run_001", "run_002"} {
		e.exec(`INSERT INTO runs (id, space_id, node_id, generation, kind, role, harness, instance_id, session_id, turn, status, desired, observed, launch_json,
			created_at, created_by, updated_at, observed_at) VALUES (?, 'spc_b', 'nod_1', 1, 'worker', 'implementor', 'claude-code', ?, 's', 1, 'idle', 'running', 'running',
			'{"account_ref":"acc_2"}', 0, 't', 0, 0)`, id, "ins_"+id)
		e.exec(`INSERT INTO account_switches (id, space_id, instance_id, role, run_kind, harness, from_run_id, from_account_id, to_account_id, cause, limit_kind,
			state, origin_account_id, created_at, updated_at) VALUES (?, 'spc_b', ?, 'implementor', 'worker', 'claude-code', 'run_old', 'acc_1', 'acc_2', 'usage_limit',
			'5h', 'succeeded', 'acc_1', 1, 1)`, "asw_"+id, "ins_"+id)
	}
	rows, err := e.svc.switchBackCandidatesPage(e.ctx, e.s.R(), 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].RunID != run {
		t.Fatalf("candidates %+v, want only %s", rows, run)
	}
}

// The candidates never exceed maxLimitRows: a first page of spaces with switching back off and eligible runs of one space
// is followed by a page of eligible runs, and the total stops at the cap.
func TestSwitchBackCandidatesCappedAtMaxLimitRows(t *testing.T) {
	e := newBackEnv(t)
	e.svc.opts.UsageLimitSettings = func(_ context.Context, _ *db.Queries, space string) UsageLimitSettings {
		return UsageLimitSettings{Failover: true, KeepAliveMax: e.keepMax, WaitIfResetWithin: e.within, SwitchMaxPerDay: e.budget, SwitchBack: space == "spc_b"}
	}
	seed := func(id, space string) {
		e.exec(`INSERT INTO runs (id, space_id, node_id, generation, kind, role, harness, instance_id, session_id, turn, status, desired, observed, launch_json,
			created_at, created_by, updated_at, observed_at) VALUES (?, ?, 'nod_1', 1, 'worker', 'implementor', 'claude-code', ?, 's', 1, 'idle', 'running', 'running',
			'{"account_ref":"acc_2"}', 0, 't', 0, 0)`, id, space, "ins_"+id)
		e.exec(`INSERT INTO account_switches (id, space_id, instance_id, role, run_kind, harness, from_run_id, from_account_id, to_account_id, cause, limit_kind,
			state, origin_account_id, created_at, updated_at) VALUES (?, ?, ?, 'implementor', 'worker', 'claude-code', 'run_old', 'acc_1', 'acc_2', 'usage_limit',
			'5h', 'succeeded', 'acc_1', 1, 1)`, "asw_"+id, space, "ins_"+id)
	}
	// The first page (maxLimitRows rows) holds 50 runs of spc_a, which is off, then 150 of spc_b; the next page holds
	// maxLimitRows more of spc_b. Without the cap the total would be 350.
	for i := range 50 {
		seed(fmt.Sprintf("run_a%03d", i), "spc_a")
	}
	for i := range maxLimitRows - 50 {
		seed(fmt.Sprintf("run_b%03d", i), "spc_b")
	}
	for i := range maxLimitRows {
		seed(fmt.Sprintf("run_c%03d", i), "spc_b")
	}
	rows, err := e.svc.switchBackCandidates(e.ctx, e.s.R())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != maxLimitRows {
		t.Fatalf("%d candidates, want exactly maxLimitRows (%d)", len(rows), maxLimitRows)
	}
}

// A chain A -> B -> C goes back to A in one move once A has reset, even while B is still limited (ADR-0105 decision 2).
func TestSwitchBackToTheChainOrigin(t *testing.T) {
	e := newBackEnv(t)
	e.exec(`INSERT INTO space_accounts (space_id, account_id, harness, created_at, created_by) VALUES ('spc_a', 'acc_3', 'claude-code', 2, 't')`)
	onB := e.movedAway()
	// acc_2 limits too, resetting in 6 h: moved on to acc_3, the chain's origin stays acc_1.
	e.report(onB, "working", 1, e.limit(proto.LimitFiveHour, dur(6*time.Hour)))
	e.report(onB, "idle", 1, nil)
	if sw := e.switchOf(onB); sw.State != SwitchParked || sw.FromAccountID != "acc_2" || sw.ToAccountID != "acc_3" || sw.OriginAccountID != "acc_1" {
		t.Fatalf("second switch %+v", sw)
	}
	e.endRun(onB, proto.RunExited, "stopped", 1)
	onC := e.resumeAndVerify(onB)
	e.afterReset()
	e.report(onC, "working", 1, nil)
	e.report(onC, "idle", 2, nil)
	if sw := e.switchOf(onC); sw.Cause != SwitchCauseSwitchBack || sw.FromAccountID != "acc_3" || sw.ToAccountID != "acc_1" {
		t.Fatalf("switch-back %+v, want acc_3 -> acc_1", sw)
	}
}

// Ping-pong guard (ADR-0105 decision 7): after a switch-back, an origin that limits again is left by the ordinary
// switch away (a new chain at the origin), and the run is not switched back again within switchBackMinGap of the last
// switch-back; after it, it is.
func TestSwitchBackPingPongGuard(t *testing.T) {
	e := newBackEnv(t)
	run := e.movedAway()
	e.afterReset()
	e.report(run, "working", 1, nil)
	e.report(run, "idle", 2, nil)
	back := e.switchOf(run)
	e.endRun(run, proto.RunExited, "stopped", 2)
	home := e.resumeAndVerify("run_w")
	// acc_1 limits again an hour later (reset in 1 h): away to acc_2 again, a chain starting at acc_1.
	e.clk.Advance(time.Hour)
	e.report(home, "working", 1, e.limit(proto.LimitFiveHour, dur(time.Hour)))
	e.report(home, "idle", 1, nil)
	away := e.switchOf(home)
	if away.Cause != SwitchCauseUsageLimit || away.State != SwitchParked || away.FromAccountID != "acc_1" || away.OriginAccountID != "acc_1" {
		t.Fatalf("switch away again %+v", away)
	}
	e.endRun(home, proto.RunExited, "stopped", 1)
	onB := e.resumeAndVerify(home)
	// acc_1 has reset again, but the last switch-back was less than switchBackMinGap ago.
	e.clk.Advance(time.Hour + limitResumeSlack)
	e.report(onB, "working", 1, nil)
	e.report(onB, "idle", 2, nil)
	if n := e.backs(onB); n != 0 {
		t.Fatalf("switched back within the guard: %d", n)
	}
	e.clk.Set(time.UnixMicro(back.CreatedAt).Add(switchBackMinGap))
	e.report(onB, "working", 2, nil)
	e.report(onB, "idle", 3, nil)
	if sw := e.switchOf(onB); sw.Cause != SwitchCauseSwitchBack || sw.State != SwitchParked || sw.ToAccountID != "acc_1" {
		t.Fatalf("not switched back after the guard: %+v", sw)
	}
}
