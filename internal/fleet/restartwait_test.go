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
// follows it, control hears once, and a second round does nothing (ADR-0098).
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

// restartRound ends cur by a restart, resumes it and returns the new run; the clock moves past the wait's resume time.
func (e *parkEnv) restartRound(cur string) string {
	e.t.Helper()
	e.end(cur, proto.RunExited, EndStoppedByRestart)
	e.clk.Advance(5 * time.Minute)
	if n := e.due(); n != 1 {
		e.t.Fatalf("resumed %d", n)
	}
	id, _ := e.launchOf(e.instance("run_w"))
	return id
}

// The cap is pinned, and each wait of a chain resumes 15, 30 and 60 s after the run's end.
func TestRestartBackoffDelays(t *testing.T) {
	if restartMaxResumes != 3 {
		t.Fatalf("cap %d, the ADR says 3", restartMaxResumes)
	}
	e := newParkEnv(t)
	e.running("run_w", "worker", "running", 1)
	e.task("tsk_1", "run_w", "running")
	cur := "run_w"
	for i, d := range []time.Duration{15 * time.Second, 30 * time.Second, 60 * time.Second} {
		end := e.clk.Now()
		e.end(cur, proto.RunExited, EndStoppedByRestart)
		if got := e.waitOf(cur).ResumeAt; got != end.Add(d).UnixMicro() {
			t.Fatalf("restart %d: resume_at +%v, want +%v", i+1, time.UnixMicro(got).Sub(end), d)
		}
		e.clk.Advance(5 * time.Minute)
		e.due()
		cur, _ = e.launchOf(e.instance("run_w"))
	}
}

// A short-lived resumed run carries its count forward; one that lived 30 minutes starts at 0 with a fresh release time;
// a resume out of a usage-limit wait starts at 0 whatever the earlier wait counted.
func TestRestartStableReset(t *testing.T) {
	startedAgo := func(e *parkEnv, id string, ago time.Duration) {
		e.exec(`UPDATE runs SET observed = 'running', started_at = ?, turn = 2 WHERE id = ?`, e.clk.Now().Add(-ago).UnixMicro(), id)
	}
	t.Run("short-lived carries", func(t *testing.T) {
		e := newParkEnv(t)
		e.running("run_w", "worker", "running", 1)
		e.task("tsk_1", "run_w", "running")
		first := e.waitOf(func() string { e.end("run_w", proto.RunExited, EndStoppedByRestart); return "run_w" }())
		e.clk.Advance(time.Minute)
		e.due()
		newID, _ := e.launchOf(e.instance("run_w"))
		startedAgo(e, newID, restartStableAfter-time.Second)
		e.end(newID, proto.RunExited, EndStoppedByRestart)
		w := e.waitOf(newID)
		if w.Attempts != 1 || w.ReleaseAt != first.ReleaseAt {
			t.Fatalf("wait %+v, first release %v", w, first.ReleaseAt)
		}
	})
	t.Run("stable resets count and release", func(t *testing.T) {
		e := newParkEnv(t)
		e.running("run_w", "worker", "running", 1)
		e.task("tsk_1", "run_w", "running")
		e.end("run_w", proto.RunExited, EndStoppedByRestart)
		first := e.waitOf("run_w")
		e.clk.Advance(time.Minute)
		e.due()
		newID, _ := e.launchOf(e.instance("run_w"))
		startedAgo(e, newID, restartStableAfter) // exactly the threshold counts as stable
		e.end(newID, proto.RunExited, EndStoppedByRestart)
		w := e.waitOf(newID)
		if w.Attempts != 0 || w.ReleaseAt.Int64 != e.clk.Now().Add(7*24*time.Hour).UnixMicro() || w.ReleaseAt == first.ReleaseAt {
			t.Fatalf("wait %+v, first release %v", w, first.ReleaseAt)
		}
	})
	t.Run("out of a usage limit wait starts at 0", func(t *testing.T) {
		e := newParkEnv(t)
		e.hold("ulm_a", "spc_a", "acc_1", "", proto.LimitFiveHour, dur(time.Hour), nil)
		e.limited("run_w", "worker", proto.LimitFiveHour, dur(time.Hour), 0, 2)
		e.task("tsk_1", "run_w", "running")
		e.end("run_w", proto.RunExited, "exited")
		e.clk.Set(e.t0.Add(time.Hour + 2*time.Minute))
		if n := e.due(); n != 1 {
			t.Fatalf("resumed %d", n)
		}
		newID, _ := e.launchOf(e.instance("run_w"))
		if got := e.waitOf("run_w").Attempts; got != 1 {
			t.Fatalf("limit wait attempts %d", got)
		}
		startedAgo(e, newID, time.Minute)
		e.end(newID, proto.RunExited, EndStoppedByRestart)
		if w := e.waitOf(newID); w.Cause != WaitCauseRestart || w.Attempts != 0 {
			t.Fatalf("wait %+v", w)
		}
	})
}

// A second restart keeps the original prompt for a later fresh session, and a resumed run restarted before its first turn
// keeps the session; a first run that never finished a turn has none to resume. A diagnostic run is resumed too.
func TestRestartSessionAndPrompt(t *testing.T) {
	e := newParkEnv(t)
	e.running("run_w", "worker", "running", 1)
	e.task("tsk_1", "run_w", "running")
	newID := e.restartRound("run_w")
	// The resumed run has finished no turn yet when the next restart hits.
	e.exec(`UPDATE runs SET session_id = 'ses_run_w', turn = 0 WHERE id = ?`, newID)
	e.end(newID, proto.RunExited, EndStoppedByRestart)
	w := e.waitOf(newID)
	if w.SessionID != "ses_run_w" {
		t.Fatalf("the session was dropped: %+v", w)
	}
	if !strings.Contains(w.LaunchJson, `"do the task"`) || strings.Contains(w.LaunchJson, "agent restarted") {
		t.Fatalf("launch_json %s", w.LaunchJson)
	}

	f := newParkEnv(t)
	f.running("run_w", "worker", "running", 0)
	f.task("tsk_1", "run_w", "running")
	f.end("run_w", proto.RunExited, EndStoppedByRestart)
	if w := f.waitOf("run_w"); w.SessionID != "" {
		t.Fatalf("a run that finished no turn kept session %q", w.SessionID)
	}
	f.clk.Advance(time.Minute)
	f.due()
	if _, p := f.launchOf(f.instance("run_w")); p.Resume || !strings.HasPrefix(p.Prompt, "do the task") {
		t.Fatalf("launch %+v", p)
	}

	d := newParkEnv(t)
	d.running("run_d", "diagnostic", "running", 1)
	d.task("tsk_1", "run_d", "running")
	d.end("run_d", proto.RunExited, EndStoppedByRestart)
	if w := d.waitOf("run_d"); w.Cause != WaitCauseRestart || w.RunKind != "diagnostic" || w.SessionID != "ses_run_d" {
		t.Fatalf("wait %+v", w)
	}
}
