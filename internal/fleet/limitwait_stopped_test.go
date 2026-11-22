package fleet

import (
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
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
