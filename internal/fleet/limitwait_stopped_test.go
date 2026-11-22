package fleet

import (
	"context"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store"
)

// A parked worker whose task was handed back (review), finished or blocked is not resumed: nothing is left for a new
// session to do, and it could redo or change work already handed back. The wait is given up with the task_moved reason.
func TestLimitWaitSkipsRunWhoseTaskIsNotRunning(t *testing.T) {
	for _, status := range []string{"review", "done", "blocked"} {
		t.Run(status, func(t *testing.T) {
			e := newParkEnv(t)
			e.limited("run_t", "worker", proto.LimitFiveHour, dur(time.Hour), 0, 2)
			e.task("tsk_t", "run_t", "running")
			e.end("run_t", proto.RunExited, "exited")
			w := e.waitOf("run_t")
			if w.State != WaitWaiting || w.TaskID != "tsk_t" {
				t.Fatalf("wait %+v", w)
			}
			// The worker handed its task back (or it moved on) while the run was parked.
			e.exec(`UPDATE tasks SET status = ? WHERE id = 'tsk_t'`, status)
			runs := e.count(`SELECT count(*) FROM runs`)
			e.clk.Advance(time.Hour + 2*time.Minute)
			e.due()
			if w = e.wait(w.ID); w.State != WaitCancelled || w.LastError != WaitTaskMoved {
				t.Fatalf("wait %+v", w)
			}
			if e.count(`SELECT count(*) FROM runs`) != runs {
				t.Fatal("a run was started")
			}
		})
	}
}

// A worker whose task was already accepted as done (or failed or cancelled) when it is parked at its limit has nothing
// to resume: its wait is given up with task_moved in the park transaction, and control is told nothing (a park message
// would promise a resume message that never comes).
func TestLimitWaitParkedAfterTaskFinishedIsGivenUp(t *testing.T) {
	for _, status := range []string{"done", "failed", "cancelled"} {
		t.Run(status, func(t *testing.T) {
			e := newParkEnv(t)
			e.limited("run_t", "worker", proto.LimitFiveHour, dur(time.Hour), 0, 2)
			e.task("tsk_t", "run_t", status)
			e.end("run_t", proto.RunExited, "exited")
			w := e.waitOf("run_t")
			if w.State != WaitCancelled || w.LastError != WaitTaskMoved || w.TaskID != "tsk_t" {
				t.Fatalf("wait %+v", w)
			}
			if len(e.events) != 0 {
				t.Fatalf("control was told: %+v", e.events)
			}
			runs := e.count(`SELECT count(*) FROM runs`)
			e.clk.Advance(time.Hour + 2*time.Minute)
			e.due()
			if e.count(`SELECT count(*) FROM runs`) != runs {
				t.Fatal("a run was started")
			}
		})
	}
}

// A run holding several tasks is parked with the one still being worked on, even when a finished task was touched
// later: the wait is not given up, and the running task is resumed with it.
func TestLimitWaitParkPrefersActiveTask(t *testing.T) {
	e := newParkEnv(t)
	e.limited("run_t", "worker", proto.LimitFiveHour, dur(time.Hour), 0, 2)
	e.task("tsk_run", "run_t", "running")
	e.task("tsk_old", "run_t", "done")
	e.exec(`UPDATE tasks SET updated_at = 1 WHERE id = 'tsk_run'`)
	e.exec(`UPDATE tasks SET updated_at = 2 WHERE id = 'tsk_old'`)
	e.end("run_t", proto.RunExited, "exited")
	w := e.waitOf("run_t")
	if w.State != WaitWaiting || w.TaskID != "tsk_run" {
		t.Fatalf("wait %+v", w)
	}
	if len(e.events) != 1 || e.events[0].Change != LimitWaitParked {
		t.Fatalf("events %+v", e.events)
	}
}

// Of several tasks being worked on, the running one wins over a later-updated task in review: the run is parked with
// it and the wait resumes.
func TestLimitWaitParkPrefersRunningOverReviewTask(t *testing.T) {
	e := newParkEnv(t)
	e.limited("run_t", "worker", proto.LimitFiveHour, dur(time.Hour), 0, 2)
	e.task("tsk_run", "run_t", "running")
	e.task("tsk_rev", "run_t", "review")
	e.exec(`UPDATE tasks SET updated_at = 1 WHERE id = 'tsk_run'`)
	e.exec(`UPDATE tasks SET updated_at = 2 WHERE id = 'tsk_rev'`)
	e.end("run_t", proto.RunExited, "exited")
	w := e.waitOf("run_t")
	if w.State != WaitWaiting || w.TaskID != "tsk_run" {
		t.Fatalf("wait %+v", w)
	}
	runs := e.count(`SELECT count(*) FROM runs`)
	e.clk.Advance(time.Hour + 2*time.Minute)
	e.due()
	if w = e.wait(w.ID); w.State == WaitCancelled {
		t.Fatalf("wait given up: %+v", w)
	}
	if e.count(`SELECT count(*) FROM runs`) <= runs {
		t.Fatal("the run was not resumed")
	}
}

// Somebody stopping a parked run (control's worker_stop) gives up its wait: neither the reset nor an account switch
// continues it afterwards.
func TestLimitWaitCancelledWhenParkedRunStopped(t *testing.T) {
	e := newParkEnv(t)
	e.limited("run_s", "worker", proto.LimitFiveHour, dur(time.Hour), 0, 2)
	e.task("tsk_s", "run_s", "running")
	e.end("run_s", proto.RunExited, "exited")
	w := e.waitOf("run_s")
	if w.State != WaitWaiting {
		t.Fatalf("wait %+v", w)
	}
	if _, err := e.svc.StopRun(e.ctx, audit.System, "run_s", "interrupt", "task handed back"); err != nil {
		t.Fatal(err)
	}
	if w = e.wait(w.ID); w.State != WaitCancelled || w.LastError != WaitRunStopped {
		t.Fatalf("wait %+v", w)
	}
	runs := e.count(`SELECT count(*) FROM runs`)
	e.clk.Advance(time.Hour + 2*time.Minute)
	e.due()
	if e.count(`SELECT count(*) FROM runs`) != runs {
		t.Fatal("a stopped run was resumed")
	}
}

// A stop that lands after the resume's claim (the wait is resuming) and before its launch commits is not lost: it
// gives up the wait, and the launch (whose transaction finds the wait changed) rolls back, so no new run appears.
func TestLimitWaitStopDuringResumingIsKept(t *testing.T) {
	e := newParkEnv(t)
	e.limited("run_r", "worker", proto.LimitFiveHour, dur(time.Hour), 0, 2)
	e.task("tsk_r", "run_r", "running")
	e.end("run_r", proto.RunExited, "exited")
	w := e.waitOf("run_r")
	// The claim: the wait is resuming (as resume commits it), the parked run's end already recorded.
	e.exec(`UPDATE limit_waits SET state = ?, version = version + 1 WHERE id = ?`, WaitResuming, w.ID)
	claimed := e.wait(w.ID)
	// The stop arrives on the ended parked run between the claim and the launch.
	if _, err := e.svc.StopRun(e.ctx, audit.System, "run_r", "interrupt", "operator stop"); err != nil {
		t.Fatal(err)
	}
	if got := e.wait(w.ID); got.State != WaitCancelled || got.LastError != WaitRunStopped {
		t.Fatalf("wait %+v", got)
	}
	// The launch, as resume builds it from its claim.
	runs := e.count(`SELECT count(*) FROM runs`)
	in, err := waitLaunch(claimed, false)
	if err != nil {
		t.Fatal(err)
	}
	in.inTx = func(ctx context.Context, tx *store.Tx, runID string) error {
		return e.svc.resumedTx(ctx, tx, LimitWaitActor, claimed, runID, false)
	}
	if _, err := e.svc.LaunchRun(e.ctx, LimitWaitActor, claimed.SpaceID, in); err == nil || apperr.CodeOf(err) != apperr.CodeConflict {
		t.Fatalf("launch error %v, want a conflict", err)
	}
	if e.count(`SELECT count(*) FROM runs`) != runs {
		t.Fatal("a run was started after the stop")
	}
	if got := e.wait(w.ID); got.State != WaitCancelled || got.LastError != WaitRunStopped {
		t.Fatalf("wait after the launch %+v", got)
	}
}

// A park that is given up because the run's task is finished leaves the instance's other waiting wait alone: it isn't
// superseded, and no wait is written for the given-up run (one active wait per instance).
func TestLimitWaitGivenUpParkKeepsOtherWait(t *testing.T) {
	e := newParkEnv(t)
	e.limited("run_a", "worker", proto.LimitFiveHour, dur(time.Hour), 0, 2)
	e.task("tsk_a", "run_a", "running")
	e.end("run_a", proto.RunExited, "exited")
	first := e.waitOf("run_a")
	e.inst["run_b"] = e.instance("run_a") // the same instance
	e.limited("run_b", "worker", proto.LimitFiveHour, dur(time.Hour), 0, 2)
	e.task("tsk_b", "run_b", "done")
	e.end("run_b", proto.RunExited, "exited")
	if w := e.wait(first.ID); w.State != WaitWaiting {
		t.Fatalf("the existing wait was displaced: %+v", w)
	}
	if n := e.count(`SELECT count(*) FROM limit_waits WHERE run_id = 'run_b'`); n != 0 {
		t.Fatalf("%d waits written for the given-up park", n)
	}
	if len(e.events) != 1 { // only run_a's park
		t.Fatalf("control was told: %+v", e.events)
	}
}
