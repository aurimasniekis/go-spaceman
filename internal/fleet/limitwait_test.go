package fleet

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// parkEnv is holdEnv with the limit-wait hooks recorded: control's messages (events), and the task hooks, which move
// the seeded task rows the way the queue does.
type parkEnv struct {
	*holdEnv
	events    []LimitWaitEvent
	repoints  []string
	releases  []string
	repointOK bool
	// inst maps a seeded run to its (valid) instance ID.
	inst map[string]string
}

func newParkEnv(t *testing.T) *parkEnv {
	t.Helper()
	e := &parkEnv{holdEnv: newHoldEnv(t), repointOK: true, inst: map[string]string{}}
	e.svc.opts.OnLimitWaitTx = func(_ context.Context, _ *store.Tx, ev LimitWaitEvent) error {
		e.events = append(e.events, ev)
		return nil
	}
	e.svc.opts.RepointTaskTx = func(ctx context.Context, tx *store.Tx, _ audit.Actor, task, from, to string) (bool, error) {
		e.repoints = append(e.repoints, task+":"+from+"->"+to)
		if !e.repointOK {
			return false, nil
		}
		_, err := tx.SQL.ExecContext(ctx, `UPDATE tasks SET assignee_run = ? WHERE id = ? AND assignee_run = ?`, to, task, from)
		return true, err
	}
	e.svc.opts.ReleaseTaskTx = func(ctx context.Context, tx *store.Tx, _ audit.Actor, task, run, _ string) (bool, error) {
		e.releases = append(e.releases, task+":"+run)
		_, err := tx.SQL.ExecContext(ctx, `UPDATE tasks SET status = 'ready', assignee_run = '' WHERE id = ? AND assignee_run = ?`, task, run)
		return true, err
	}
	// The first round of a process looks for resumes a stopped central left behind; it's done here, so tests count only
	// their own rounds (TestLimitWaitStuckResumingRecovered simulates a restart).
	e.due()
	return e
}

// limited seeds a live run at its limit on acc_1: kind is the run kind, resets an offset from t0 (nil: unknown), since
// the limit's since. It has finished turn turns of session ses_<id>.
func (e *parkEnv) limited(id, kind, limit string, resets *time.Duration, since time.Duration, turn int) {
	e.t.Helper()
	var r any
	if resets != nil {
		r = e.us(*resets)
	}
	e.exec(`INSERT INTO runs (id, space_id, node_id, generation, kind, role, harness, instance_id, session_id, turn, desired, observed, launch_json,
		created_at, created_by, updated_at, observed_at, limit_kind, limit_resets_at, limit_since)
		VALUES (?, 'spc_a', 'nod_1', 1, ?, 'implementor', 'claude-code', ?, ?, ?, 'running', 'running', ?, 0, 't', 0, 0, ?, ?, ?)`,
		id, kind, e.instance(id), "ses_"+id, turn, `{"account_ref":"acc_1","prompt":"do the task","model":"m1"}`, limit, r, e.us(since))
}

func (e *parkEnv) instance(run string) string {
	if _, ok := e.inst[run]; !ok {
		e.inst[run] = ids.New(ids.Instance)
	}
	return e.inst[run]
}

// task seeds a task of space A assigned to run.
func (e *parkEnv) task(id, run, status string) {
	e.t.Helper()
	e.exec(`INSERT INTO tasks (id, space_id, title, rank, status, assignee_run, created_at, created_by, updated_at) VALUES (?, 'spc_a', 'T', 1, ?, ?, 0, 't', 0)`,
		id, status, run)
}

// end reports the run ended (exited) with its node's reason, keeping its stored limit.
func (e *parkEnv) end(id, state, reason string) {
	e.t.Helper()
	if _, err := e.svc.applyReport(e.ctx, "nod_1", proto.RunReport{RunID: id, SpaceID: "spc_a", Generation: 1, State: state, Reason: reason,
		ObservedAt: e.clk.Now()}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *parkEnv) waitOf(run string) db.LimitWait {
	e.t.Helper()
	w, err := e.s.R().GetLimitWaitByRun(e.ctx, run)
	if err != nil {
		e.t.Fatalf("wait of %s: %v", run, err)
	}
	return w
}

func (e *parkEnv) wait(id string) db.LimitWait {
	e.t.Helper()
	w, err := e.s.R().GetLimitWait(e.ctx, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return w
}

func (e *parkEnv) due() int {
	e.t.Helper()
	n, err := e.svc.LimitWaitsDue(e.ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *parkEnv) nextAt() time.Time {
	e.t.Helper()
	at, err := e.svc.NextLimitWait(e.ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	return at
}

// launchOf returns the launch command of the instance's newest run.
func (e *parkEnv) launchOf(instance string) (string, proto.LaunchPayload) {
	e.t.Helper()
	var runID, payload string
	if err := e.s.ReadDB().QueryRowContext(e.ctx, `SELECT r.id, c.payload_json FROM runs r JOIN node_commands c ON c.run_id = r.id AND c.kind = ?
		WHERE r.instance_id = ? ORDER BY r.created_at DESC, r.id DESC LIMIT 1`, proto.CmdLaunch, instance).Scan(&runID, &payload); err != nil {
		e.t.Fatalf("launch of %s: %v", instance, err)
	}
	var p proto.LaunchPayload
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		e.t.Fatal(err)
	}
	return runID, p
}

func (e *parkEnv) writeCount() int {
	return e.count(`SELECT (SELECT count(*) FROM changes) + (SELECT count(*) FROM audit_events) + (SELECT coalesce(sum(version), 0) FROM limit_waits)`)
}

func TestParkAtAndResumeAt(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)
	us := func(d time.Duration) int64 { return t0.Add(d).UnixMicro() }
	run := func(kind string, resets *time.Duration) db.Run {
		r := db.Run{LimitKind: kind, LimitSince: nullUs(us(0))}
		if resets != nil {
			r.LimitResetsAt = nullUs(us(*resets))
		}
		return r
	}
	kam := 6 * time.Hour
	for _, c := range []struct {
		name string
		r    db.Run
		want time.Duration
	}{
		{"5-hour reset within the keep-alive: parked at the keep-alive if still limited", run(proto.LimitFiveHour, dur(5*time.Hour)), 6 * time.Hour},
		{"weekly reset beyond the keep-alive: parked at once", run(proto.LimitSevenDay, dur(72*time.Hour)), 0},
		{"unknown subscription reset: the keep-alive", run(proto.LimitUnknown, nil), 6 * time.Hour},
		{"gateway limit without a reset: 30 minutes", run(proto.LimitGatewayQuota, nil), 30 * time.Minute},
		{"gateway limit with a near reset: the keep-alive", run(proto.LimitGatewayBudget, dur(2*time.Hour)), 6 * time.Hour},
	} {
		if got := parkAt(c.r, kam); !got.Equal(t0.Add(c.want)) {
			t.Errorf("%s: park at %v, want +%v", c.name, got.Sub(t0), c.want)
		}
	}

	now := t0.Add(time.Hour)
	known := limitState{kind: proto.LimitSevenDay, resets: nullUs(us(72 * time.Hour))}
	unknown := limitState{kind: proto.LimitUnknown}
	probe := db.UsageLimit{CheckAt: nullUs(us(90 * time.Minute))}
	holdReset := db.UsageLimit{ResetsAt: nullUs(us(10 * time.Hour))}
	for _, c := range []struct {
		name     string
		lim      limitState
		hold     *db.UsageLimit
		attempts int64
		want     time.Duration
	}{
		{"known reset: 2 minutes after it", known, nil, 0, 72*time.Hour + 2*time.Minute},
		{"a passed reset: now", limitState{kind: proto.LimitFiveHour, resets: nullUs(us(0))}, nil, 0, time.Hour},
		{"unknown, the hold's reset", unknown, &holdReset, 0, 10*time.Hour + 2*time.Minute},
		{"unknown, the hold's probe", unknown, &probe, 0, 90 * time.Minute},
		{"unknown, no hold: 1 h", unknown, nil, 0, 2 * time.Hour},
		{"unknown, second attempt: 2 h", unknown, nil, 1, 3 * time.Hour},
		{"unknown, many attempts: capped at the keep-alive", unknown, nil, 40, 7 * time.Hour},
	} {
		if got := waitResumeAt(now, c.lim, c.hold, c.attempts, kam); !got.Equal(t0.Add(c.want)) {
			t.Errorf("%s: resume at +%v, want +%v", c.name, got.Sub(t0), c.want)
		}
	}
}

// ADR-0079 decisions 4 and 5 (UL-7, fake clock): a worker that ends at its 5-hour limit is parked, with its task and
// session; at the reset the job resumes it as a new run of the same instance with --resume of its session, the task
// follows it, and control hears once each way. When the resumed run ends at a limit again, the new wait carries the
// release time and attempts forward and control isn't told again.
func TestLimitWaitParkAndResume(t *testing.T) {
	t.Parallel()
	e := newParkEnv(t)
	e.hold("ulm_a", "spc_a", "acc_1", "", proto.LimitFiveHour, dur(3*time.Hour), nil)
	e.limited("run_w", "worker", proto.LimitFiveHour, dur(3*time.Hour), 0, 2)
	e.task("tsk_1", "run_w", "running")
	e.end("run_w", proto.RunExited, "exited")

	if r := e.observed("run_w"); r.EndReason != EndUsageLimit {
		t.Fatalf("end reason %q", r.EndReason)
	}
	w := e.waitOf("run_w")
	if w.State != WaitWaiting || w.TaskID != "tsk_1" || w.SessionID != "ses_run_w" || w.InstanceID != e.instance("run_w") || w.UsageLimitID != "ulm_a" ||
		w.ResumeAt != e.us(3*time.Hour+2*time.Minute) || !w.ReleaseAt.Valid || w.ReleaseAt.Int64 != e.us(7*24*time.Hour) {
		t.Fatalf("wait %+v", w)
	}
	if len(e.events) != 1 || e.events[0].Change != LimitWaitParked || e.events[0].Repark {
		t.Fatalf("events %+v", e.events)
	}
	if at := e.nextAt(); !at.Equal(e.t0.Add(3*time.Hour + 2*time.Minute)) {
		t.Fatalf("next %v", at)
	}

	// Before the reset nothing happens, and nothing is written.
	e.clk.Advance(time.Hour)
	before := e.writeCount()
	if n := e.due(); n != 0 || e.writeCount() != before {
		t.Fatalf("a round before the reset acted on %d, wrote %d", n, e.writeCount()-before)
	}

	e.clk.Set(e.t0.Add(3*time.Hour + 2*time.Minute))
	if n := e.due(); n != 1 {
		t.Fatalf("resumed %d", n)
	}
	newID, p := e.launchOf(e.instance("run_w"))
	if newID == "run_w" || p.InstanceID != e.instance("run_w") || p.SessionID != "ses_run_w" || !p.Resume || p.Prompt != resumePrompt || p.Model != "m1" ||
		p.AccountRef != "acc_1" {
		t.Fatalf("resume launch %s %+v", newID, p)
	}
	w = e.wait(w.ID)
	if w.State != WaitResumed || w.NewRunID != newID || w.Attempts != 1 {
		t.Fatalf("resumed wait %+v", w)
	}
	if e.count(`SELECT count(*) FROM tasks WHERE id = 'tsk_1' AND assignee_run = ?`, newID) != 1 || len(e.repoints) != 1 {
		t.Fatalf("task not re-pointed: %v", e.repoints)
	}
	if ev := e.events[len(e.events)-1]; ev.Change != LimitWaitResumed || ev.NewRunID != newID || ev.Fresh || !ev.FirstResume {
		t.Fatalf("resume event %+v", ev)
	}
	var manifest string
	_ = e.s.ReadDB().QueryRowContext(e.ctx, `SELECT manifest_json FROM runs WHERE id = ?`, newID).Scan(&manifest)
	if !strings.Contains(manifest, `"usage_limit_resume"`) || !strings.Contains(manifest, `"session_resumed":true`) {
		t.Fatalf("manifest %s", manifest)
	}
	if n := e.due(); n != 0 {
		t.Fatalf("a second round resumed %d more", n)
	}

	// The resumed run reaches a limit again (an unknown one) and ends there: re-parked, carrying the release time and
	// attempts, without a new message to control.
	e.clk.Advance(time.Hour)
	e.exec(`UPDATE runs SET observed = 'running', turn = 1, session_id = 'ses_run_w', limit_kind = 'unknown', limit_since = ? WHERE id = ?`,
		e.clk.Now().UnixMicro(), newID)
	e.end(newID, proto.RunLost, "lost")
	w2 := e.waitOf(newID)
	if w2.ID == w.ID || w2.Attempts != 1 || w2.ReleaseAt != w.ReleaseAt || w2.TaskID != "tsk_1" {
		t.Fatalf("re-park %+v (first %+v)", w2, w)
	}
	if !strings.Contains(w2.LaunchJson, `"prompt":"do the task"`) {
		t.Fatalf("the re-park lost the original prompt: %s", w2.LaunchJson)
	}
	if ev := e.events[len(e.events)-1]; ev.Change != LimitWaitParked || !ev.Repark {
		t.Fatalf("re-park event %+v", ev)
	}
	// Unknown reset, one attempt so far: 2 h of backoff (the hold's own reset passed, so it doesn't decide).
	if want := e.clk.Now().Add(2 * time.Hour).UnixMicro(); w2.ResumeAt != want {
		t.Fatalf("re-park resume at %v, want +2h", time.UnixMicro(w2.ResumeAt).Sub(e.clk.Now()))
	}
}

// Decision 4: a run that outlives the keep-alive at its limit is stopped and parked; one whose known reset is further
// away is parked at once. Its end, which central asked for, still ends usage_limit. A run somebody stopped is not
// parked, and kb runs are left alone.
func TestLimitWaitKeepAliveMax(t *testing.T) {
	t.Parallel()
	e := newParkEnv(t)
	e.limited("run_u", "worker", proto.LimitUnknown, nil, 0, 1)
	e.limited("run_k", "kb", proto.LimitUnknown, nil, 0, 1)
	e.limited("run_c", "control", proto.LimitSevenDay, dur(72*time.Hour), 0, 3)
	if at := e.nextAt(); !at.Equal(e.t0) {
		t.Fatalf("next %v, want t0 (the weekly control is due at once)", at)
	}
	if n := e.due(); n != 1 {
		t.Fatalf("parked %d at once, want 1", n)
	}
	c := e.waitOf("run_c")
	if r := e.observed("run_c"); r.Desired != "stopped" || c.ReleaseAt.Valid || c.ResumeAt != e.us(72*time.Hour+2*time.Minute) {
		t.Fatalf("control parked: desired %s, wait %+v", r.Desired, c)
	}
	if e.count(`SELECT count(*) FROM node_commands WHERE run_id = 'run_c' AND kind = ?`, proto.CmdStop) != 1 {
		t.Fatal("no stop for the parked control")
	}
	if at := e.nextAt(); !at.Equal(e.t0.Add(6 * time.Hour)) {
		t.Fatalf("next %v, want the keep-alive", at)
	}
	e.clk.Advance(6*time.Hour - time.Second)
	if n := e.due(); n != 0 {
		t.Fatalf("parked %d before the keep-alive", n)
	}
	e.clk.Advance(time.Second)
	if n := e.due(); n != 1 {
		t.Fatalf("parked %d at the keep-alive", n)
	}
	e.waitOf("run_u")
	if e.count(`SELECT count(*) FROM limit_waits WHERE run_id = 'run_k'`) != 0 || e.observed("run_k").Desired != "running" {
		t.Fatal("a kb run was parked")
	}
	e.end("run_k", proto.RunExited, "exited")
	if e.count(`SELECT count(*) FROM limit_waits WHERE run_id = 'run_k'`) != 0 {
		t.Fatal("a kb run that ended at its limit was parked")
	}
	before := e.writeCount()
	if n := e.due(); n != 0 || e.writeCount() != before {
		t.Fatal("a parked run was parked again")
	}
	// The node reports the stop: the run ended because of its limit.
	e.end("run_u", proto.RunExited, "stopped")
	if r := e.observed("run_u"); r.EndReason != EndUsageLimit {
		t.Fatalf("a parked run ended %q", r.EndReason)
	}

	// An operator stopped this one: its end at the limit isn't parked.
	e.limited("run_s", "worker", proto.LimitFiveHour, dur(time.Hour), 0, 1)
	e.exec(`UPDATE runs SET desired = 'stopped' WHERE id = 'run_s'`)
	e.end("run_s", proto.RunExited, "stopped")
	if e.count(`SELECT count(*) FROM limit_waits WHERE run_id = 'run_s'`) != 0 || e.observed("run_s").EndReason == EndUsageLimit {
		t.Fatal("a run somebody stopped was parked")
	}
}

// Decision 5: a wait that can't resume yet is put off with a fixed reason and doesn't write again until something
// changes; the node's hello and the space's resume bring it forward. A held account puts it off to the reset, a task
// that moved or a space that was archived gives it up, and a parked run that hasn't ended yet holds it back.
func TestLimitWaitDeferrals(t *testing.T) {
	t.Parallel()
	e := newParkEnv(t)
	park := func(id string) db.LimitWait {
		e.t.Helper()
		e.limited(id, "worker", proto.LimitFiveHour, dur(time.Hour), e.clk.Now().Sub(e.t0), 1)
		e.end(id, proto.RunExited, "exited")
		return e.waitOf(id)
	}
	w := park("run_n")
	e.clk.Advance(time.Hour + 2*time.Minute)

	// Node away (the lease expired).
	e.exec(`UPDATE execution_leases SET expires_at = ? WHERE space_id = 'spc_a'`, e.us(time.Minute))
	e.due()
	if w = e.wait(w.ID); w.State != WaitWaiting || w.LastError != WaitNodeOffline || w.ResumeAt != e.clk.Now().Add(limitRetryNode).UnixMicro() {
		t.Fatalf("node away: %+v", w)
	}
	before := e.writeCount()
	e.due()
	e.svc.wakeWaitsOnNode(e.ctx, "nod_1") // still away... the hello is what renews the lease
	if e.writeCount() != before+2 {
		t.Fatalf("a woken wait wrote %d", e.writeCount()-before)
	}
	// The node is back: its hello granted the lease and woke the wait.
	e.exec(`UPDATE execution_leases SET expires_at = ? WHERE space_id = 'spc_a'`, e.us(1000*time.Hour))
	e.svc.wakeWaitsOnNode(e.ctx, "nod_1")
	if n := e.due(); n != 1 || e.wait(w.ID).State != WaitResumed {
		t.Fatalf("after the hello: %d, %+v", n, e.wait(w.ID))
	}

	// Space paused.
	w = park("run_p")
	e.clk.Advance(time.Hour + 2*time.Minute)
	e.exec(`UPDATE spaces SET state = 'paused' WHERE id = 'spc_a'`)
	e.due()
	if w = e.wait(w.ID); w.LastError != WaitSpacePaused || w.ResumeAt != e.clk.Now().Add(limitRetryPaused).UnixMicro() {
		t.Fatalf("paused: %+v", w)
	}
	e.exec(`UPDATE spaces SET state = 'active' WHERE id = 'spc_a'`)
	e.svc.wakeWaits(e.ctx, []string{"spc_a"}, WaitSpacePaused)
	if n := e.due(); n != 1 {
		t.Fatalf("after the space resumed: %d", n)
	}

	// The account is held again (another window): put off to that reset.
	w = park("run_h")
	e.clk.Advance(time.Hour + 2*time.Minute)
	reset := e.clk.Now().Add(3 * time.Hour).Sub(e.t0)
	e.hold("ulm_again", "spc_b", "acc_1", "", proto.LimitSevenDay, &reset, nil)
	e.due()
	if w = e.wait(w.ID); w.LastError != WaitAccountHeld || w.ResumeAt != e.us(reset+limitResumeSlack) {
		t.Fatalf("held: %+v", w)
	}
	e.exec(`UPDATE usage_limits SET cleared_at = 1 WHERE id = 'ulm_again'`)

	// The parked run hasn't ended yet (central's stop is on its way).
	e.limited("run_e", "worker", proto.LimitFiveHour, dur(time.Hour), e.clk.Now().Sub(e.t0), 1)
	e.clk.Advance(6 * time.Hour)
	e.due() // parks it at the keep-alive
	we := e.waitOf("run_e")
	e.clk.Set(time.UnixMicro(we.ResumeAt))
	e.due()
	if we = e.wait(we.ID); we.LastError != WaitRunEnding || we.State != WaitWaiting {
		t.Fatalf("run not ended: %+v", we)
	}

	// The task went elsewhere: the wait is given up, not resumed.
	w = park("run_t")
	e.task("tsk_t", "run_other", "running")
	e.exec(`UPDATE limit_waits SET task_id = 'tsk_t' WHERE id = ?`, w.ID)
	e.clk.Advance(time.Hour + 2*time.Minute)
	e.due()
	if w = e.wait(w.ID); w.State != WaitCancelled || w.LastError != WaitTaskMoved {
		t.Fatalf("task moved: %+v", w)
	}

	// The task moves while the resume is launching (the queue no longer re-points it): no run, the wait is given up.
	w = park("run_race")
	e.task("tsk_race", "run_race", "running")
	e.exec(`UPDATE limit_waits SET task_id = 'tsk_race' WHERE id = ?`, w.ID)
	e.repointOK = false
	e.clk.Advance(time.Hour + 2*time.Minute)
	runs := e.count(`SELECT count(*) FROM runs`)
	e.due()
	if w = e.wait(w.ID); w.State != WaitCancelled || w.LastError != WaitTaskMoved || e.count(`SELECT count(*) FROM runs`) != runs {
		t.Fatalf("task moved mid-resume: %+v", w)
	}
	e.repointOK = true

	// The space was archived.
	w = park("run_a")
	e.exec(`UPDATE spaces SET state = 'archived' WHERE id = 'spc_a'`)
	e.clk.Advance(time.Hour + 2*time.Minute)
	e.due()
	if w = e.wait(w.ID); w.State != WaitCancelled || w.LastError != WaitSpaceArchived {
		t.Fatalf("archived: %+v", w)
	}
}

// The UL-2 review follow-up: a wait left resuming by a central that stopped mid-resume (no run was started: a resume
// commits its run and its wait together) goes back to waiting on the first round and resumes.
func TestLimitWaitStuckResumingRecovered(t *testing.T) {
	t.Parallel()
	e := newParkEnv(t)
	e.limited("run_r", "worker", proto.LimitFiveHour, dur(-time.Hour), -2*time.Hour, 1)
	e.end("run_r", proto.RunExited, "exited")
	w := e.waitOf("run_r")
	e.exec(`UPDATE limit_waits SET state = 'resuming', updated_at = ? WHERE id = ?`, e.us(-time.Minute), w.ID)
	e.svc.lw.recovered = false // as when this process started
	if at := e.nextAt(); !at.Equal(e.t0) {
		t.Fatalf("next %v: the first round must look for stuck resumes", at)
	}
	if n := e.due(); n != 2 {
		t.Fatalf("recovered and resumed %d, want 2", n)
	}
	if w = e.wait(w.ID); w.State != WaitResumed {
		t.Fatalf("stuck wait %+v", w)
	}
	// One that a running process is resuming right now (updated after it started) is left alone.
	e.limited("run_q", "worker", proto.LimitFiveHour, dur(-time.Hour), -2*time.Hour, 1)
	e.end("run_q", proto.RunExited, "exited")
	q := e.waitOf("run_q")
	e.exec(`UPDATE limit_waits SET state = 'resuming', updated_at = ? WHERE id = ?`, e.clk.Now().UnixMicro(), q.ID)
	e.clk.Advance(limitResumingStale - time.Second)
	e.svc.lw.recoverAt = e.clk.Now() // the periodic look for stuck resumes is due
	e.due()
	if q = e.wait(q.ID); q.State != WaitResuming {
		t.Fatalf("an in-flight resume was recovered: %+v", q)
	}
	e.clk.Advance(2 * time.Second)
	e.svc.lw.recoverAt = e.clk.Now()
	e.due()
	if q = e.wait(q.ID); q.State != WaitResumed {
		t.Fatalf("a stale resume was not recovered: %+v", q)
	}
}

// The 2026-11-13 lesson: a mass reset doesn't stampede. Control resumes first, then the oldest, at most
// limitResumeBatch per round, and the next round waits limitResumeSpacing.
func TestLimitWaitResumeBatched(t *testing.T) {
	t.Parallel()
	e := newParkEnv(t)
	for i := range 7 {
		e.limited(fmt.Sprintf("run_%d", i), "worker", proto.LimitFiveHour, dur(time.Hour), 0, 1)
		e.end(fmt.Sprintf("run_%d", i), proto.RunExited, "exited")
		e.clk.Advance(time.Second)
	}
	e.limited("run_ctl", "control", proto.LimitFiveHour, dur(time.Hour), 0, 1)
	e.end("run_ctl", proto.RunExited, "exited")
	e.clk.Set(e.t0.Add(2 * time.Hour))
	if n := e.due(); n != limitResumeBatch {
		t.Fatalf("resumed %d in one round", n)
	}
	if e.waitOf("run_ctl").State != WaitResumed || e.waitOf("run_0").State != WaitResumed || e.waitOf("run_6").State != WaitWaiting {
		t.Fatal("not control first, then the oldest")
	}
	if at := e.nextAt(); !at.Equal(e.clk.Now().Add(limitResumeSpacing)) {
		t.Fatalf("next round at %v, want spaced", at.Sub(e.clk.Now()))
	}
	e.clk.Advance(limitResumeSpacing)
	if n := e.due(); n != 3 {
		t.Fatalf("second round resumed %d", n)
	}
	if at := e.nextAt(); !at.IsZero() {
		t.Fatalf("next %v with nothing left", at)
	}
}

// Decision 6 (Q2): a worker's wait is released at its maximum wait, its task goes back to the queue and control is
// told; control's wait never is, even after a month. An operator can release or resume one now.
func TestLimitWaitRelease(t *testing.T) {
	t.Parallel()
	e := newParkEnv(t)
	e.limited("run_w", "worker", proto.LimitSevenDay, dur(8*24*time.Hour), 0, 1)
	e.task("tsk_w", "run_w", "running")
	e.end("run_w", proto.RunExited, "exited")
	e.limited("run_c", "control", proto.LimitSevenDay, dur(8*24*time.Hour), 0, 1)
	e.end("run_c", proto.RunExited, "exited")
	w, c := e.waitOf("run_w"), e.waitOf("run_c")
	if at := e.nextAt(); !at.Equal(e.t0.Add(7 * 24 * time.Hour)) {
		t.Fatalf("next %v, want the release", at.Sub(e.t0))
	}
	e.clk.Set(e.t0.Add(7 * 24 * time.Hour))
	if n := e.due(); n != 1 {
		t.Fatalf("released %d", n)
	}
	if w = e.wait(w.ID); w.State != WaitReleased || w.LastError != WaitMaxWait || len(e.releases) != 1 || e.releases[0] != "tsk_w:run_w" {
		t.Fatalf("release %+v %v", w, e.releases)
	}
	if ev := e.events[len(e.events)-1]; ev.Change != LimitWaitReleased || ev.Wait.ID != w.ID {
		t.Fatalf("release event %+v", ev)
	}
	e.clk.Set(e.t0.Add(30 * 24 * time.Hour))
	e.exec(`UPDATE execution_leases SET expires_at = ?`, e.us(1000*24*time.Hour))
	e.due() // resumes control (its reset passed); never releases it
	if c = e.wait(c.ID); c.State != WaitResumed {
		t.Fatalf("control's wait %+v", c)
	}

	// Operator actions: a released wait can't be resumed or released again; another space's wait isn't found.
	if _, err := e.svc.ResumeLimitWait(e.ctx, holdActor, "spc_a", w.ID); apperr.CodeOf(err) != apperr.CodePrecondition {
		t.Fatalf("resume a released wait: %v", err)
	}
	if _, err := e.svc.ReleaseLimitWait(e.ctx, holdActor, "spc_b", w.ID); apperr.CodeOf(err) != apperr.CodeNotFound {
		t.Fatalf("release from another space: %v", err)
	}
	// Resume now: before its resume time, and refused (with the wait unchanged) while the node is away.
	e.limited("run_m", "worker", proto.LimitSevenDay, dur(40*24*time.Hour), e.clk.Now().Sub(e.t0), 1)
	e.end("run_m", proto.RunExited, "exited")
	m := e.waitOf("run_m")
	e.exec(`UPDATE execution_leases SET expires_at = 0`)
	if _, err := e.svc.ResumeLimitWait(e.ctx, holdActor, "spc_a", m.ID); apperr.CodeOf(err) != apperr.CodePrecondition ||
		!strings.Contains(err.Error(), "not connected") {
		t.Fatalf("resume now with the node away: %v", err)
	}
	if got := e.wait(m.ID); got.State != WaitWaiting || got.ResumeAt != m.ResumeAt {
		t.Fatalf("a refused resume-now moved the wait: %+v", got)
	}
	e.exec(`UPDATE execution_leases SET expires_at = ?`, e.us(1000*24*time.Hour))
	// Refused by a hold: the wait keeps its resume time and records the reason.
	heldTill := e.clk.Now().Add(time.Hour).Sub(e.t0)
	e.hold("ulm_now", "spc_b", "acc_1", "", proto.LimitFiveHour, &heldTill, nil)
	for range 2 {
		if _, err := e.svc.ResumeLimitWait(e.ctx, holdActor, "spc_a", m.ID); apperr.CodeOf(err) != apperr.CodeRateLimited {
			t.Fatalf("resume now on a held account: %v", err)
		}
		got := e.wait(m.ID)
		if got.State != WaitWaiting || got.ResumeAt != m.ResumeAt || got.LastError != WaitAccountHeld {
			t.Fatalf("held resume-now: %+v", got)
		}
	}
	e.exec(`UPDATE usage_limits SET cleared_at = 1 WHERE id = 'ulm_now'`)
	got, err := e.svc.ResumeLimitWait(e.ctx, holdActor, "spc_a", m.ID)
	if err != nil || got.State != WaitResumed || got.NewRunID == "" {
		t.Fatalf("resume now: %+v %v", got, err)
	}
	var actor string
	_ = e.s.ReadDB().QueryRowContext(e.ctx, `SELECT actor_id FROM audit_events WHERE action = 'limit_wait.resumed' AND target_id = ?`, m.ID).Scan(&actor)
	if actor != holdActor.ID {
		t.Fatalf("resume now audited as %q", actor)
	}
}

func nullUs(v int64) sql.NullInt64 { return sql.NullInt64{Int64: v, Valid: true} }

// Decision 5: when the space moved to another node since the run was parked, its session (node-local) can't be
// resumed: the run starts a fresh session there with its original prompt and a note, and control hears of it.
func TestLimitWaitRelocatedFreshSession(t *testing.T) {
	t.Parallel()
	e := newParkEnv(t)
	e.limited("run_r", "worker", proto.LimitFiveHour, dur(time.Hour), 0, 3)
	e.end("run_r", proto.RunExited, "exited")
	e.exec(`INSERT INTO nodes (id, name, state, created_at, created_by, updated_at) VALUES ('nod_2', 'n2', 'active', 0, 't', 0)`)
	e.exec(`UPDATE spaces SET node_id = 'nod_2' WHERE id = 'spc_a'`)
	e.exec(`UPDATE execution_leases SET node_id = 'nod_2', generation = 2 WHERE space_id = 'spc_a'`)
	e.clk.Advance(time.Hour + 2*time.Minute)
	if n := e.due(); n != 1 {
		t.Fatalf("resumed %d", n)
	}
	id, p := e.launchOf(e.instance("run_r"))
	if p.SessionID != "" || p.Resume || !strings.HasPrefix(p.Prompt, "do the task\n\n") || !strings.HasSuffix(p.Prompt, freshPrompt) {
		t.Fatalf("relocated resume %+v", p)
	}
	var node string
	var gen int64
	_ = e.s.ReadDB().QueryRowContext(e.ctx, `SELECT node_id, generation FROM runs WHERE id = ?`, id).Scan(&node, &gen)
	if node != "nod_2" || gen != 2 {
		t.Fatalf("resumed on %s generation %d", node, gen)
	}
	if ev := e.events[len(e.events)-1]; ev.Change != LimitWaitResumed || !ev.Fresh {
		t.Fatalf("event %+v", ev)
	}
}
