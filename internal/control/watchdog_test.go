package control

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/limits"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/queue"
)

var wdNow = time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

const wdN = 15 * time.Minute

func ago(d time.Duration) time.Time { return wdNow.Add(-d) }

func tp(t time.Time) *time.Time { return &t }

// stalled is a space whose idle control has done nothing for an hour while one message waits for 30 minutes.
func stalled() observation {
	return observation{
		SpaceState: "active",
		Control:    &fleet.Run{ID: "run_ctl", Kind: "control", Role: "control", Desired: "running", Observed: "running", Status: "idle", StartedAt: tp(ago(3 * time.Hour))},
		Act:        activity{at: ago(time.Hour), tool: "spaceman_tasks_list"},
		Started:    ago(5 * time.Hour),
		Unacked:    []time.Time{ago(30 * time.Minute)},
		MaxRuns:    2,
	}
}

func TestWatchdogStalled(t *testing.T) {
	v := assess(stalled(), wdN, wdNow)
	if v.Guard != "" || v.Backlog.Messages != 1 || !v.Last.Equal(ago(time.Hour)) {
		t.Fatalf("verdict %+v", v)
	}
	if got := nudgeText(v.Backlog); !strings.HasPrefix(got, "SpaceMan watchdog: 1 unacknowledged message waiting") {
		t.Fatalf("text %q", got)
	}
	// "done" is idle too (the program finished its turn).
	o := stalled()
	o.Control.Status = "done"
	if v := assess(o, wdN, wdNow); v.Guard != "" {
		t.Fatalf("done: %+v", v)
	}
	// Without a tool call since central started, the last activity is the later of the run's start and central's.
	o = stalled()
	o.Act = activity{}
	if v := assess(o, wdN, wdNow); v.Guard != "" || !v.Last.Equal(ago(3*time.Hour)) {
		t.Fatalf("no activity known: %+v", v)
	}
}

// Every false-positive guard holds on its own, on an otherwise stalled control (ADR-0099 decision 4).
func TestWatchdogGuards(t *testing.T) {
	cases := []struct {
		name  string
		n     time.Duration
		edit  func(o *observation)
		guard string
	}{
		{"watchdog off", 0, func(*observation) {}, guardOff},
		{"space paused", wdN, func(o *observation) { o.SpaceState = "paused" }, guardPaused},
		{"space archived", wdN, func(o *observation) { o.SpaceState = "archived" }, guardPaused},
		{"no control run", wdN, func(o *observation) { o.Control = nil }, guardNoControl},
		{"control starting", wdN, func(o *observation) { o.Control.Observed = "starting" }, guardNoControl},
		{"control ended", wdN, func(o *observation) { o.Control.Observed = "exited" }, guardNoControl},
		{"control being stopped", wdN, func(o *observation) { o.Control.Desired = "stopped" }, guardNoControl},
		{"operator took over", wdN, func(o *observation) { o.Control.Takeover = &fleet.Takeover{} }, guardTakeover},
		{"at a usage limit", wdN, func(o *observation) { o.Control.Limit = &fleet.RunLimit{} }, guardLimit},
		{"parked at a usage limit", wdN, func(o *observation) { o.Parked = true }, guardLimit},
		{"harness prompt", wdN, func(o *observation) { o.Control.Status = "blocked" }, guardHuman},
		{"asked a person last", wdN, func(o *observation) { o.HumanOpen, o.Act.tool = 1, humanAskTool }, guardHuman},
		{"asked a person, last tool unknown", wdN, func(o *observation) { o.HumanOpen, o.Act = 1, activity{} }, guardHuman},
		{"mid-turn", wdN, func(o *observation) { o.Control.Status = "working" }, guardBusy},
		{"no status reported", wdN, func(o *observation) { o.Control.Status = "" }, guardBusy},
		{"tool call running", wdN, func(o *observation) { o.Act.inflight = 1 }, guardBusy},
		{"nothing waits", wdN, func(o *observation) { o.Unacked = nil }, guardNoBacklog},
		{"message too new", wdN, func(o *observation) { o.Unacked = []time.Time{ago(10 * time.Minute)} }, guardNoBacklog},
		{"recent tool call", wdN, func(o *observation) { o.Act.at = ago(5 * time.Minute) }, guardActive},
		{"recent turn", wdN, func(o *observation) { o.Act.turnAt = ago(5 * time.Minute) }, guardActive},
		{"control just started", wdN, func(o *observation) { o.Control.StartedAt = tp(ago(5 * time.Minute)); o.Act = activity{} }, guardActive},
		{"central just started", wdN, func(o *observation) { o.Started = ago(5 * time.Minute); o.Act = activity{} }, guardActive},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := stalled()
			c.edit(&o)
			if v := assess(o, c.n, wdNow); v.Guard != c.guard {
				t.Fatalf("guard %q, want %q (%+v)", v.Guard, c.guard, v)
			}
		})
	}
	// An open question control asked earlier does not hide a stall when control went on to do other things.
	o := stalled()
	o.HumanOpen = 1
	if v := assess(o, wdN, wdNow); v.Guard != "" {
		t.Fatalf("an older open question guarded: %+v", v)
	}
}

func task(id, status, run string, updated time.Time, waiting ...string) *queue.Task {
	return &queue.Task{ID: id, Status: status, AssigneeRun: run, UpdatedAt: updated, Waiting: waiting}
}

func run(id, kind, desired, observed, status string, ended *time.Time) *fleet.Run {
	return &fleet.Run{ID: id, Kind: kind, Desired: desired, Observed: observed, Status: status, EndedAt: ended}
}

// Each backlog signal counts what is old enough and only that (ADR-0099 decision 2).
func TestWatchdogSignals(t *testing.T) {
	old, young := ago(30*time.Minute), ago(5*time.Minute)
	o := stalled()
	o.Unacked = []time.Time{ago(2 * time.Hour), old, young}
	o.Runs = []*fleet.Run{
		run("run_rev_exited", "worker", "running", "exited", "done", tp(old)),
		run("run_rev_done", "worker", "running", "running", "done", nil),
		run("run_rev_working", "worker", "running", "running", "working", nil),
		run("run_failed", "worker", "running", "failed", "", tp(old)),
		run("run_lost", "diagnostic", "running", "lost", "", tp(old)),
		run("run_exited", "worker", "running", "exited", "", tp(old)),
		run("run_young", "worker", "running", "exited", "", tp(young)),
		run("run_stopped", "worker", "stopped", "exited", "", tp(old)),                    // someone asked it to stop
		run("run_before_ctl", "worker", "running", "failed", "", tp(ago(4*time.Hour))),    // before this control started
		run("run_before_act", "worker", "running", "failed", "", tp(ago(90*time.Minute))), // control acted after it ended
		run("run_kb", "kb", "running", "failed", "", tp(old)),
	}
	limited := run("run_limit", "worker", "running", "failed", "", tp(old))
	limited.EndReason = fleet.EndUsageLimit // parked: it comes back through its limit wait
	o.Runs = append(o.Runs, limited)
	o.Tasks = []*queue.Task{
		task("tsk_1", queue.Review, "run_rev_exited", old),
		task("tsk_2", queue.Review, "run_rev_done", old),
		task("tsk_3", queue.Review, "run_rev_working", old), // its worker is still at it
		task("tsk_4", queue.Review, "run_gone", old),        // a run too old to be listed has ended
		task("tsk_5", queue.Review, "run_rev_exited", young),
	}
	v := assess(o, wdN, wdNow)
	want := Backlog{Messages: 2, Reviews: 3, Failed: 2, Exited: 2}
	if v.Guard != "" || v.Backlog != want {
		t.Fatalf("backlog %+v, want %+v", v.Backlog, want)
	}
	if got := v.Backlog.String(); got != "2 unacknowledged messages, 3 tasks in review with finished workers, 2 failed runs, 2 ended runs" {
		t.Fatalf("text %q", got)
	}
}

// Free worker slots count only with an eligible ready task that has waited long enough; a run waiting for its usage
// limit frees its slot.
func TestWatchdogFreeSlots(t *testing.T) {
	old := ago(30 * time.Minute)
	base := func() observation {
		o := stalled()
		o.Unacked = nil
		o.MaxRuns = 2
		o.Tasks = []*queue.Task{task("tsk_r", queue.Ready, "", old)}
		return o
	}
	o := base()
	if v := assess(o, wdN, wdNow); v.Guard != "" || v.Backlog.Startable != 1 || v.Backlog.FreeSlots != 2 {
		t.Fatalf("free slots %+v", v)
	}
	if got := o2s(o); got != "1 ready task with 2 worker slots free" {
		t.Fatalf("text %q", got)
	}
	o = base()
	o.Runs = []*fleet.Run{run("run_a", "worker", "running", "running", "working", nil), run("run_b", "diagnostic", "running", "starting", "", nil)}
	if v := assess(o, wdN, wdNow); v.Guard != guardNoBacklog {
		t.Fatalf("no free slot: %+v", v)
	}
	o.Runs[0].Limit = &fleet.RunLimit{}
	if v := assess(o, wdN, wdNow); v.Backlog.Startable != 1 || v.Backlog.FreeSlots != 1 {
		t.Fatalf("a limited run frees its slot: %+v", v)
	}
	o = base()
	o.Tasks = []*queue.Task{task("tsk_w", queue.Ready, "", old, "depends on tsk_x"), task("tsk_y", queue.Ready, "", ago(time.Minute)),
		task("tsk_b", queue.Backlog, "", old)}
	if v := assess(o, wdN, wdNow); v.Guard != guardNoBacklog {
		t.Fatalf("waiting, young or backlog tasks counted: %+v", v)
	}
}

func o2s(o observation) string { return assess(o, wdN, wdNow).Backlog.String() }

func TestWatchdogNudgeGap(t *testing.T) {
	want := []int64{5, 10, 20, 40, 60, 60, 60}
	for i, w := range want {
		if got := nudgeGap(i+1, time.Minute); got != time.Duration(w)*time.Minute {
			t.Fatalf("gap after nudge %d: %s, want %d m", i+1, got, w)
		}
	}
	if got := nudgeGap(2, 100*time.Millisecond); got != time.Second {
		t.Fatalf("scaled gap %s", got)
	}
}

// wdEnv drives the watchdog's episode logic with fakes for the space's state and the delivery.
type wdEnv struct {
	t         *testing.T
	clk       *clock.Fake
	w         *Watchdog
	o         observation
	nudges    []string
	refuse    error
	escalated int
	lim       map[string]int64
}

func newWDEnv(t *testing.T) *wdEnv {
	e := &wdEnv{t: t, clk: clock.NewFake(wdNow), lim: map[string]int64{limits.ControlWatchdog: 15, limits.ControlWatchdogNudges: 3}}
	sv := NewSupervisor(nil, nil, nil, nil, nil, e.clk)
	sv.Limit = func(_ context.Context, _, key string) int64 { return e.lim[key] }
	e.w = NewWatchdog(sv)
	e.w.started = wdNow.Add(-5 * time.Hour)
	e.o = stalled()
	e.o.Act = activity{}
	e.w.spaces = func(context.Context) ([]string, error) { return []string{"spc_a"}, nil }
	e.w.observe = func(context.Context, string) (observation, error) {
		o := e.o
		c := *e.o.Control
		o.Control = &c
		return o, nil
	}
	e.w.nudge = func(_ context.Context, r *fleet.Run, text string, _ map[string]any) error {
		if e.refuse != nil {
			return e.refuse
		}
		e.nudges = append(e.nudges, text)
		return nil
	}
	e.w.escalate = func(_ context.Context, _ string, _ *fleet.Run, ep *episode, _ Backlog, _ time.Duration) {
		e.escalated++
	}
	return e
}

func (e *wdEnv) pass(d time.Duration) WatchState {
	e.t.Helper()
	e.clk.Advance(d)
	e.w.Pass(context.Background())
	s, _ := e.w.State("spc_a")
	return s
}

// Nudges back off (5, 10, 20 minutes, …); after the configured number go unanswered, people are told once and the
// nudges continue at the backoff.
func TestWatchdogBackoffAndEscalation(t *testing.T) {
	e := newWDEnv(t)
	s := e.pass(0)
	if len(e.nudges) != 1 || s.Nudges != 1 || !s.NextNudge.Equal(e.clk.Now().Add(5*time.Minute)) {
		t.Fatalf("first pass: %d nudges, state %+v", len(e.nudges), s)
	}
	for _, step := range []struct {
		after time.Duration
		want  int
	}{{4 * time.Minute, 1}, {time.Minute, 2}, {9 * time.Minute, 2}, {time.Minute, 3}, {19 * time.Minute, 3}} {
		e.pass(step.after)
		if len(e.nudges) != step.want || e.escalated != 0 {
			t.Fatalf("after %s: %d nudges, %d escalations, want %d and none", step.after, len(e.nudges), e.escalated, step.want)
		}
	}
	// The third nudge went unanswered for its whole gap: people are told, and control is reminded again.
	s = e.pass(time.Minute)
	if e.escalated != 1 || len(e.nudges) != 4 || !s.Escalated {
		t.Fatalf("escalation: %d, nudges %d, state %+v", e.escalated, len(e.nudges), s)
	}
	e.pass(40 * time.Minute)
	e.pass(60 * time.Minute)
	if e.escalated != 1 || len(e.nudges) != 6 {
		t.Fatalf("after escalating: %d escalations, %d nudges", e.escalated, len(e.nudges))
	}
}

// Any activity of control ends the episode: the count starts over and control gets the full quiet time again.
func TestWatchdogActivityEndsEpisode(t *testing.T) {
	e := newWDEnv(t)
	e.pass(0)
	e.pass(5 * time.Minute)
	if len(e.nudges) != 2 {
		t.Fatalf("%d nudges", len(e.nudges))
	}
	// A tool call (through the dispatcher wrapper) right after the nudge.
	e.clk.Advance(time.Minute)
	e.w.toolStarted("run_ctl", "spaceman_messages")
	e.w.toolFinished("run_ctl")
	if s := e.pass(0); s.Guard != guardActive || s.Nudges != 0 {
		t.Fatalf("after a tool call: %+v", s)
	}
	e.pass(14 * time.Minute)
	if len(e.nudges) != 2 {
		t.Fatalf("nudged %d times within the quiet time", len(e.nudges))
	}
	if s := e.pass(time.Minute); len(e.nudges) != 3 || s.Nudges != 1 {
		t.Fatalf("new episode: %d nudges, state %+v", len(e.nudges), s)
	}
	// A new turn counts too, even without a tool call.
	e.o.Control.Turn++
	if s := e.pass(time.Minute); s.Guard != guardActive || s.Nudges != 0 {
		t.Fatalf("after a turn: %+v", s)
	}
	// Handling the backlog ends it as well.
	e.o.Unacked = nil
	if s := e.pass(time.Hour); s.Guard != guardNoBacklog || len(e.nudges) != 3 {
		t.Fatalf("no backlog: %+v, %d nudges", s, len(e.nudges))
	}
}

// A guard pauses an episode without advancing it, and a refused nudge is not counted.
func TestWatchdogGuardPausesAndRefusal(t *testing.T) {
	e := newWDEnv(t)
	e.pass(0)
	e.o.Control.Status = "working"
	for range 10 {
		e.pass(10 * time.Minute)
	}
	if len(e.nudges) != 1 {
		t.Fatalf("a busy control was nudged: %d", len(e.nudges))
	}
	e.o.Control.Status = "idle"
	e.refuse = errors.New("refused")
	if s := e.pass(0); s.Nudges != 1 || len(e.nudges) != 1 {
		t.Fatalf("refused nudge counted: %+v", s)
	}
	e.refuse = nil
	if s := e.pass(0); s.Nudges != 2 || len(e.nudges) != 2 {
		t.Fatalf("retry: %+v", s)
	}
	// A space whose watchdog is turned off forgets its episode.
	e.lim[limits.ControlWatchdog] = 0
	if s := e.pass(time.Hour); s.Guard != guardOff || len(e.nudges) != 2 {
		t.Fatalf("off: %+v", s)
	}
	e.lim[limits.ControlWatchdog] = 15
	if s := e.pass(0); s.Nudges != 1 {
		t.Fatalf("turned on again: %+v", s)
	}
}

// Escalation never happens with control_watchdog_nudges 0, and a new control run starts a new episode.
func TestWatchdogNoEscalationAndNewRun(t *testing.T) {
	e := newWDEnv(t)
	e.lim[limits.ControlWatchdogNudges] = 0
	for range 8 {
		e.pass(time.Hour)
	}
	if e.escalated != 0 || len(e.nudges) != 8 {
		t.Fatalf("%d escalations, %d nudges", e.escalated, len(e.nudges))
	}
	e.o.Control.ID = "run_ctl2"
	if s := e.pass(0); s.Nudges != 1 || s.ControlRun != "run_ctl2" {
		t.Fatalf("new run: %+v", s)
	}
}

// A tool call still running (an approval, say) marks control busy until it returns; the catalog call is not activity.
func TestWatchdogToolsWrapper(t *testing.T) {
	e := newWDEnv(t)
	release := make(chan struct{})
	started := make(chan struct{})
	next := func(context.Context, *fleet.RunPrincipal, string, json.RawMessage) (json.RawMessage, error) {
		close(started)
		<-release
		return nil, nil
	}
	d := e.w.Tools(func(ctx context.Context, p *fleet.RunPrincipal, tool string, args json.RawMessage) (json.RawMessage, error) {
		return next(ctx, p, tool, args)
	})
	done := make(chan struct{})
	go func() {
		_, _ = d(context.Background(), &fleet.RunPrincipal{RunID: "run_ctl"}, "spaceman_human_ask", nil)
		close(done)
	}()
	<-started
	if s := e.pass(time.Hour); s.Guard != guardBusy {
		t.Fatalf("in-flight call: %+v", s)
	}
	close(release)
	<-done
	e.w.mu.Lock()
	a := *e.w.acts["run_ctl"]
	e.w.mu.Unlock()
	if a.inflight != 0 || a.tool != "spaceman_human_ask" {
		t.Fatalf("activity %+v", a)
	}
	if _, err := e.w.Tools(func(context.Context, *fleet.RunPrincipal, string, json.RawMessage) (json.RawMessage, error) {
		return nil, nil
	})(
		context.Background(), &fleet.RunPrincipal{RunID: "run_other"}, proto.ToolCatalog, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.w.acts["run_other"]; ok {
		t.Fatal("the catalog call counted as activity")
	}
}

func TestWatchdogTakeRestart(t *testing.T) {
	var w *Watchdog
	if w.takeRestart("run_x") {
		t.Fatal("nil watchdog restarts")
	}
	e := newWDEnv(t)
	e.w.restarts["run_ctl"] = true
	if !e.w.takeRestart("run_ctl") || e.w.takeRestart("run_ctl") {
		t.Fatal("a restart is taken exactly once")
	}
}
