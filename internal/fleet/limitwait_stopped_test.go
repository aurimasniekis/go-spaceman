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
