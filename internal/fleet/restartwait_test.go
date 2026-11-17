package fleet

import (
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// running seeds a live run without a limit: kind is the run kind, desired its desired state, turn its finished turns.
func (e *parkEnv) running(id, kind, desired string, turn int) {
	e.t.Helper()
	e.exec(`INSERT INTO runs (id, space_id, node_id, generation, kind, role, harness, instance_id, session_id, turn, desired, observed, launch_json,
		created_at, created_by, updated_at, observed_at)
		VALUES (?, 'spc_a', 'nod_1', 1, ?, 'implementor', 'claude-code', ?, ?, ?, ?, 'running', ?, 0, 't', 0, 0)`,
		id, kind, e.instance(id), "ses_"+id, turn, desired, `{"prompt":"do the task","model":"m1"}`)
}

// An agent restart stops a wanted worker run: it is resumed on the same instance with --resume of its session, its task
// follows it, control hears once, and a second round does nothing (ADR-0096).
func TestRestartStoppedRunResumes(t *testing.T) {
	e := newParkEnv(t)
	e.running("run_w", "worker", "running", 2)
	e.task("tsk_1", "run_w", "running")
	e.end("run_w", proto.RunExited, EndStoppedByRestart)

	w := e.waitOf("run_w")
	if w.Cause != WaitCauseRestart || w.State != WaitWaiting || w.TaskID != "tsk_1" || w.SessionID != "ses_run_w" || w.Attempts != 0 ||
		w.ResumeAt != e.us(restartResumeDelay) || !w.ReleaseAt.Valid || w.UsageLimitID != "" {
		t.Fatalf("wait %+v", w)
	}
	if n := e.due(); n != 0 {
		t.Fatalf("resumed %d before the delay", n)
	}
	e.clk.Advance(restartResumeDelay)
	if n := e.due(); n != 1 {
		t.Fatalf("resumed %d", n)
	}
	newID, p := e.launchOf(e.instance("run_w"))
	if newID == "run_w" || !p.Resume || p.SessionID != "ses_run_w" || p.Prompt != restartPrompt || p.Model != "m1" {
		t.Fatalf("resume launch %s %+v", newID, p)
	}
	if got := e.wait(w.ID); got.State != WaitResumed || got.NewRunID != newID || got.Attempts != 1 {
		t.Fatalf("resumed wait %+v", got)
	}
	if e.count(`SELECT count(*) FROM tasks WHERE id = 'tsk_1' AND assignee_run = ?`, newID) != 1 {
		t.Fatal("the task was not pointed at the new run")
	}
	var manifest string
	_ = e.s.ReadDB().QueryRowContext(e.ctx, `SELECT manifest_json FROM runs WHERE id = ?`, newID).Scan(&manifest)
	if !strings.Contains(manifest, `"agent_restart_resume"`) {
		t.Fatalf("manifest %s", manifest)
	}
	if last := e.events[len(e.events)-1]; last.Change != LimitWaitResumed || last.Wait.Cause != WaitCauseRestart || last.NewRunID != newID {
		t.Fatalf("events %+v", e.events)
	}
	if n := e.due(); n != 0 {
		t.Fatalf("a second round resumed %d more", n)
	}
}

// A run that keeps being stopped by restarts is resumed at most restartMaxResumes times; the next end gives its task
// back (ready, unassigned) and tells control, and writes no further resume.
func TestRestartResumeIsCapped(t *testing.T) {
	e := newParkEnv(t)
	e.running("run_w", "worker", "running", 1)
	e.task("tsk_1", "run_w", "running")
	cur := "run_w"
	for i := 1; i <= restartMaxResumes; i++ {
		e.end(cur, proto.RunExited, EndStoppedByRestart)
		e.clk.Advance(10 * time.Minute)
		if n := e.due(); n != 1 {
			t.Fatalf("restart %d: resumed %d", i, n)
		}
		cur, _ = e.launchOf(e.instance("run_w"))
		if got := e.waitOf(e.lastEnded(i, cur)).Attempts; got != int64(i) {
			t.Fatalf("restart %d: attempts %d", i, got)
		}
	}
	before := len(e.releases)
	e.end(cur, proto.RunExited, EndStoppedByRestart)
	w := e.waitOf(cur)
	if w.State != WaitReleased || w.LastError != WaitRestartCap || w.Cause != WaitCauseRestart {
		t.Fatalf("capped wait %+v", w)
	}
	if len(e.releases) != before+1 || e.count(`SELECT count(*) FROM tasks WHERE id = 'tsk_1' AND status = 'ready' AND assignee_run = ''`) != 1 {
		t.Fatalf("the task was not given back: %v", e.releases)
	}
	if ev := e.events[len(e.events)-1]; ev.Change != LimitWaitReleased || ev.TaskStatus != "ready" || ev.Wait.Cause != WaitCauseRestart {
		t.Fatalf("event %+v", ev)
	}
	e.clk.Advance(time.Hour)
	if n := e.due(); n != 0 {
		t.Fatalf("a capped run was resumed (%d)", n)
	}
}

// lastEnded is the run whose wait the i-th restart wrote: the run before cur in the chain, read from the waits.
func (e *parkEnv) lastEnded(_ int, cur string) string {
	var id string
	if err := e.s.ReadDB().QueryRowContext(e.ctx, `SELECT run_id FROM limit_waits WHERE new_run_id = ?`, cur).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

// A run that lived on after its resume starts the count again, so an occasional restart never exhausts the cap.
func TestRestartStableRunResetsCap(t *testing.T) {
	e := newParkEnv(t)
	e.running("run_w", "worker", "running", 1)
	e.task("tsk_1", "run_w", "running")
	e.end("run_w", proto.RunExited, EndStoppedByRestart)
	e.clk.Advance(time.Minute)
	e.due()
	newID, _ := e.launchOf(e.instance("run_w"))
	// The resumed run ran for an hour, then another restart stopped it.
	e.exec(`UPDATE runs SET observed = 'running', started_at = ?, turn = 3 WHERE id = ?`, e.clk.Now().UnixMicro(), newID)
	e.clk.Advance(time.Hour)
	e.end(newID, proto.RunExited, EndStoppedByRestart)
	if w := e.waitOf(newID); w.Attempts != 0 || w.State != WaitWaiting {
		t.Fatalf("wait %+v", w)
	}
}

// Nothing is resumed for a run somebody stopped, a control run (its supervisor restarts it), a task in review (its work
// is done) or a run without a task; the ordinary run-ended notice covers those.
func TestRestartNotResumed(t *testing.T) {
	e := newParkEnv(t)
	cases := []struct{ id, kind, desired, status string }{
		{"run_stopped", "worker", "stopped", "running"},
		{"run_ctl", "control", "running", ""},
		{"run_review", "worker", "running", "review"},
		{"run_notask", "worker", "running", ""},
	}
	for i, c := range cases {
		e.running(c.id, c.kind, c.desired, 1)
		if c.status != "" {
			e.task("tsk_"+string(rune('a'+i)), c.id, c.status)
		}
		e.end(c.id, proto.RunExited, EndStoppedByRestart)
		if n := e.count(`SELECT count(*) FROM limit_waits WHERE run_id = ?`, c.id); n != 0 {
			t.Fatalf("%s: %d waits written", c.id, n)
		}
	}
	e.clk.Advance(time.Hour)
	if n := e.due(); n != 0 {
		t.Fatalf("resumed %d", n)
	}
	// A normal exit of a wanted worker is not a restart either.
	e.running("run_ok", "worker", "running", 1)
	e.task("tsk_ok", "run_ok", "running")
	e.end("run_ok", proto.RunExited, "exited")
	if n := e.count(`SELECT count(*) FROM limit_waits WHERE run_id = 'run_ok'`); n != 0 {
		t.Fatal("a normal exit was parked")
	}
}
