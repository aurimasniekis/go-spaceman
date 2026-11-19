package control

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
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
		{"asked a person last", wdN, func(o *observation) { o.HumanOpen, o.Act.tool, o.Act.asked, o.Act.askTurn = 1, humanAskTool, true, 7 }, guardHuman},
		{"asked, then told a worker in the same turn", wdN, func(o *observation) {
			o.HumanOpen, o.Act.tool, o.Act.asked, o.Act.askTurn = 1, "spaceman_message_send", true, 7
		}, guardHuman},
		{"asked, and the asking turn finished", wdN, func(o *observation) {
			o.HumanOpen, o.Act.tool, o.Act.asked, o.Act.askTurn, o.Control.Turn = 1, "spaceman_task_set_status", true, 6, 7
		}, guardHuman},
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
	// An open question control asked earlier does not hide a stall when control went on to do other things: one asked
	// before central started (last tool known, no ask seen), and one asked two turns ago.
	o := stalled()
	o.HumanOpen = 1
	if v := assess(o, wdN, wdNow); v.Guard != "" {
		t.Fatalf("an older open question guarded: %+v", v)
	}
	o.Act.asked, o.Act.askTurn, o.Control.Turn = true, 5, 7
	if v := assess(o, wdN, wdNow); v.Guard != "" {
		t.Fatalf("a question asked before control's latest turn guarded: %+v", v)
	}
	// Answered: nothing open, no guard, even in the asking turn.
	o.HumanOpen, o.Act.askTurn = 0, 7
	if v := assess(o, wdN, wdNow); v.Guard != "" {
		t.Fatalf("an answered question guarded: %+v", v)
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
		task("tsk_0", queue.Review, "run_rev_exited", ago(90*time.Minute)), // control acted after it entered review
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
	// The oldest item is the oldest message (reviews and ended runs are not judged for it).
	if !v.Oldest.Equal(ago(2 * time.Hour)) {
		t.Fatalf("oldest %v", v.Oldest)
	}
}

// A review counts only until control's first activity after it entered review: a review that waits for a person's
// merge for hours is no backlog once control has acted since (ADR-0099 decision 2).
func TestWatchdogReviewWaitingForPeople(t *testing.T) {
	o := stalled()
	o.Unacked = nil
	o.Runs = []*fleet.Run{run("run_w", "worker", "running", "exited", "done", tp(ago(8*time.Hour)))}
	o.Tasks = []*queue.Task{task("tsk_pr", queue.Review, "run_w", ago(8*time.Hour))}
	if v := assess(o, wdN, wdNow); v.Guard != guardNoBacklog || v.Backlog.Reviews != 0 {
		t.Fatalf("a review waiting for a merge counted: %+v", v)
	}
	// Control has done nothing since the task entered review: it counts.
	o.Act = activity{at: ago(9 * time.Hour)}
	o.Started, o.Control.StartedAt = ago(10*time.Hour), tp(ago(10*time.Hour))
	if v := assess(o, wdN, wdNow); v.Guard != "" || v.Backlog.Reviews != 1 {
		t.Fatalf("a review control never saw: %+v", v)
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
	// A task whose role no account would launch now (all at a usage limit) waits for a reset, not for control.
	o = base()
	o.Tasks = []*queue.Task{task("tsk_r", queue.Ready, "", old), task("tsk_i", queue.Ready, "", old)}
	o.Tasks[1].AssigneeRole = "implementor"
	o.Held = map[string]bool{"": true, "implementor": false}
	if v := assess(o, wdN, wdNow); v.Guard != "" || v.Backlog.Startable != 1 {
		t.Fatalf("a held task counted: %+v", v)
	}
	o.Held["implementor"] = true
	if v := assess(o, wdN, wdNow); v.Guard != guardNoBacklog {
		t.Fatalf("every account limited: %+v", v)
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
	at        []time.Time // when each nudge was sent
	refuse    error
	escalated int
	escAt     []int // the nudge count at each escalation
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
	e.w.turnOf = func(context.Context, string) (int64, bool) { return e.o.Control.Turn, true }
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
		e.at = append(e.at, e.clk.Now())
		return nil
	}
	e.w.escalate = func(_ context.Context, _ string, _ *fleet.Run, ep *episode, _ Backlog, _ time.Duration) {
		e.escalated++
		e.escAt = append(e.escAt, ep.nudges)
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

// A nudge into an idle program always starts a turn. Control answering every nudge with a turn while the backlog stays
// keeps one episode: the gaps grow and people are told after the configured number (ADR-0099 decision 6).
func TestWatchdogTurnsWithoutHandlingEscalate(t *testing.T) {
	e := newWDEnv(t)
	e.lim[limits.ControlWatchdog] = 2                                                       // quiet time shorter than the gaps, so the backoff shows
	e.o.Tasks = []*queue.Task{task("tsk_r", queue.Review, "run_gone", ago(30*time.Minute))} // a review as well
	e.pass(0)
	var s WatchState
	for range 4 * 60 {
		if e.at[len(e.at)-1].Equal(e.clk.Now()) {
			e.o.Control.Turn++ // control answers the nudge with a turn, and leaves the messages
		}
		s = e.pass(time.Minute)
	}
	var gaps []time.Duration
	for i := 1; i < len(e.at); i++ {
		gaps = append(gaps, e.at[i].Sub(e.at[i-1]))
	}
	want := []time.Duration{5 * time.Minute, 10 * time.Minute, 20 * time.Minute, 40 * time.Minute, time.Hour, time.Hour}
	if !slices.Equal(gaps, want) {
		t.Fatalf("gaps %v, want %v", gaps, want)
	}
	if !slices.Equal(e.escAt, []int{3}) || !s.Escalated || s.Nudges != 7 {
		t.Fatalf("escalations after nudges %v, state %+v", e.escAt, s)
	}
}

// The episode follows the backlog: tool calls and turns keep it, more waiting keeps it, and it ends when the backlog
// shrinks, its oldest item is handled, or it empties, whoever handled it.
func TestWatchdogEpisodeFollowsBacklog(t *testing.T) {
	e := newWDEnv(t)
	e.o.Unacked = []time.Time{ago(time.Hour), ago(30 * time.Minute)}
	e.pass(0)
	e.pass(5 * time.Minute)
	if len(e.nudges) != 2 {
		t.Fatalf("%d nudges", len(e.nudges))
	}
	// A tool call right after the nudge is activity, not handling: the episode keeps its count.
	e.clk.Advance(time.Minute)
	e.w.toolStarted("run_ctl", "spaceman_tasks_list")
	e.w.toolFinished("run_ctl")
	if s := e.pass(0); s.Guard != guardActive || s.Nudges != 2 {
		t.Fatalf("after a tool call: %+v", s)
	}
	// Another message arrives: the backlog grew, same episode; the next nudge is the third once control is quiet again.
	e.o.Unacked = append(e.o.Unacked, ago(20*time.Minute))
	if s := e.pass(15 * time.Minute); s.Nudges != 3 || len(e.nudges) != 3 {
		t.Fatalf("grown backlog: %d nudges, state %+v", len(e.nudges), s)
	}
	// Control acknowledges the newest message: the backlog shrank (from what the episode saw last), the episode ends,
	// and the next nudge is the first again.
	e.o.Unacked = e.o.Unacked[:2]
	e.clk.Advance(time.Minute)
	e.w.toolStarted("run_ctl", "spaceman_message_ack")
	e.w.toolFinished("run_ctl")
	if s := e.pass(0); s.Nudges != 0 {
		t.Fatalf("after an acknowledgement: %+v", s)
	}
	if s := e.pass(15 * time.Minute); s.Nudges != 1 || len(e.nudges) != 4 {
		t.Fatalf("new episode: %d nudges, state %+v", len(e.nudges), s)
	}
	// The oldest message is handled and a new one takes its place: same count, but the oldest advanced.
	e.o.Unacked = []time.Time{e.o.Unacked[1], ago(16 * time.Minute)}
	if s := e.pass(time.Minute); s.Guard != "" || s.Nudges != 1 || len(e.nudges) != 5 {
		t.Fatalf("oldest handled: %d nudges, state %+v", len(e.nudges), s)
	}
	// An operator acknowledges everything while control does nothing (and while it is busy): the episode ends.
	e.pass(5 * time.Minute)
	if len(e.nudges) != 6 {
		t.Fatalf("%d nudges", len(e.nudges))
	}
	e.o.Control.Status = "working"
	saved := e.o.Unacked
	e.o.Unacked = nil
	if s := e.pass(time.Minute); s.Guard != guardBusy || s.Nudges != 0 {
		t.Fatalf("cleared while busy: %+v", s)
	}
	e.o.Control.Status = "idle"
	e.o.Unacked = saved
	if s := e.pass(time.Minute); s.Nudges != 1 {
		t.Fatalf("a backlog after the cleared one starts a new episode: %+v", s)
	}
	e.o.Unacked = nil
	if s := e.pass(time.Minute); s.Guard != guardNoBacklog || s.Nudges != 0 {
		t.Fatalf("cleared: %+v", s)
	}
	e.o.Unacked = saved
	if s := e.pass(time.Minute); s.Nudges != 1 || e.escalated != 0 {
		t.Fatalf("after clearing: %+v, %d escalations", s, e.escalated)
	}
}

// An episode of reviews only ends when control's next turn takes them out of the backlog: a message after that starts
// a new episode.
func TestWatchdogReviewEpisodeEnds(t *testing.T) {
	e := newWDEnv(t)
	e.o.Unacked = nil
	e.o.Tasks = []*queue.Task{task("tsk_r", queue.Review, "run_gone", ago(30*time.Minute))}
	e.pass(0)
	e.pass(5 * time.Minute)
	if len(e.nudges) != 2 {
		t.Fatalf("%d nudges", len(e.nudges))
	}
	e.o.Control.Turn++
	if s := e.pass(time.Minute); s.Guard != guardNoBacklog || s.Nudges != 0 {
		t.Fatalf("after a turn: %+v", s)
	}
	e.o.Unacked = []time.Time{ago(time.Hour)}
	if s := e.pass(15 * time.Minute); s.Nudges != 1 || len(e.nudges) != 3 {
		t.Fatalf("new episode: %d nudges, state %+v", len(e.nudges), s)
	}
}

// Starting one of the ready tasks shrinks the backlog: the episode ends.
func TestWatchdogStartedTaskEndsEpisode(t *testing.T) {
	e := newWDEnv(t)
	e.o.Tasks = []*queue.Task{task("tsk_a", queue.Ready, "", ago(20*time.Minute)), task("tsk_b", queue.Ready, "", ago(20*time.Minute))}
	e.pass(0)
	if s := e.pass(5 * time.Minute); s.Nudges != 2 || s.Backlog.Startable != 2 {
		t.Fatalf("%+v", s)
	}
	e.o.Tasks = e.o.Tasks[:1]
	e.o.Runs = []*fleet.Run{run("run_b", "worker", "running", "running", "working", nil)}
	if s := e.pass(time.Minute); s.Nudges != 1 || len(e.nudges) != 3 {
		t.Fatalf("after a start: %d nudges, state %+v", len(e.nudges), s)
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
	e.o.Control.Turn = 4 // the asking turn becomes the fifth
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
	if a.inflight != 0 || a.tool != "spaceman_human_ask" || !a.asked || a.askTurn != 4 {
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

// A backlog that began with reviews alone has no oldest message or task (zero): a message or a startable task that
// arrives later is more backlog, not handling, so the count carries on to escalation.
func TestWatchdogLaterArrivalKeepsReviewEpisode(t *testing.T) {
	e := newWDEnv(t)
	e.o.Unacked = nil
	e.o.Tasks = []*queue.Task{task("tsk_r", queue.Review, "run_gone", ago(30*time.Minute))}
	e.pass(0)
	if s := e.pass(5 * time.Minute); s.Nudges != 2 {
		t.Fatalf("%+v", s)
	}
	e.o.Unacked = []time.Time{ago(20 * time.Minute)}
	if s := e.pass(10 * time.Minute); s.Nudges != 3 || s.Backlog.Reviews != 1 || s.Backlog.Messages != 1 {
		t.Fatalf("a message reset the count: %+v", s)
	}
	e.o.Tasks = append(e.o.Tasks, task("tsk_s", queue.Ready, "", ago(30*time.Minute)))
	if s := e.pass(20 * time.Minute); s.Nudges < 4 || !s.Escalated || e.escalated != 1 {
		t.Fatalf("a startable task reset the count: %+v, %d escalations", s, e.escalated)
	}
}

// After a central restart (nothing known of control's tool calls) an open question holds the guard only until the
// watchdog sees a turn of control advance; control that takes turns without tool calls is nudged.
func TestWatchdogHumanWaitEndsWithTurnAfterRestart(t *testing.T) {
	e := newWDEnv(t)
	e.o.HumanOpen = 1
	if s := e.pass(0); s.Guard != guardHuman || len(e.nudges) != 0 {
		t.Fatalf("first pass: %+v", s)
	}
	e.o.Control.Turn++
	if s := e.pass(30 * time.Minute); s.Guard != guardActive || len(e.nudges) != 0 {
		t.Fatalf("a turn is activity: %+v", s)
	}
	if s := e.pass(15 * time.Minute); s.Guard != "" || s.Nudges != 1 || len(e.nudges) != 1 {
		t.Fatalf("not nudged after a turn: %+v", s)
	}
	// With a tool call known, and none after the asking turn, the question still holds.
	if waitsOnAsk(observation{Control: &fleet.Run{Turn: 3}, Act: activity{tool: "x", asked: true, askTurn: 2}}) != true {
		t.Fatal("asked in the latest turn")
	}
}

// The ready task is what the oldest-waiting time follows.
func TestWatchdogReadyTaskFeedsOldest(t *testing.T) {
	o := stalled()
	o.Unacked = nil
	o.Tasks = []*queue.Task{task("tsk_a", queue.Ready, "", ago(40*time.Minute)), task("tsk_b", queue.Ready, "", ago(20*time.Minute))}
	if v := assess(o, wdN, wdNow); !v.Oldest.Equal(ago(40 * time.Minute)) {
		t.Fatalf("oldest %v", v.Oldest)
	}
	o.Unacked = []time.Time{ago(60 * time.Minute)}
	if v := assess(o, wdN, wdNow); !v.Oldest.Equal(ago(60 * time.Minute)) {
		t.Fatalf("oldest with a message %v", v.Oldest)
	}
}

// A pass that did not read the backlog (the space is paused) leaves the episode alone: the empty backlog it reports is
// not handling.
func TestWatchdogPausedSpaceKeepsEpisode(t *testing.T) {
	e := newWDEnv(t)
	e.pass(0)
	e.o.SpaceState = "paused"
	if s := e.pass(time.Minute); s.Guard != guardPaused || s.Nudges != 1 {
		t.Fatalf("paused: %+v", s)
	}
	e.o.SpaceState = "active"
	if s := e.pass(5 * time.Minute); s.Nudges != 2 {
		t.Fatalf("the episode was lost: %+v", s)
	}
}

// A refused spaceman_human_ask is no question: it starts no guard. When the run's turn can't be read, the turn the
// last pass saw stands in.
func TestWatchdogHumanAskRecording(t *testing.T) {
	e := newWDEnv(t)
	e.o.Control.Turn = 7
	e.pass(0)
	call := func(err error) {
		d := e.w.Tools(func(context.Context, *fleet.RunPrincipal, string, json.RawMessage) (json.RawMessage, error) {
			return nil, err
		})
		_, _ = d(context.Background(), &fleet.RunPrincipal{RunID: "run_ctl"}, "spaceman_human_ask", nil)
	}
	call(errors.New("refused"))
	e.w.mu.Lock()
	asked := e.w.acts["run_ctl"].asked
	e.w.mu.Unlock()
	if asked {
		t.Fatal("a failed human_ask started the guard")
	}
	e.w.turnOf = func(context.Context, string) (int64, bool) { return 0, false }
	call(nil)
	e.w.mu.Lock()
	a := *e.w.acts["run_ctl"]
	e.w.mu.Unlock()
	if !a.asked || a.askTurn != 7 {
		t.Fatalf("fallback turn: %+v", a)
	}
}

// A kicked space is checked once and its mark cleared, so it is not checked again by the next wake.
func TestWatchdogKickClearsMark(t *testing.T) {
	e := newWDEnv(t)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	done := make(chan struct{})
	go func() { e.w.Run(ctx); close(done) }()
	e.w.Kick("spc_a")
	deadline := time.Now().Add(5 * time.Second)
	for {
		if s, _ := e.w.State("spc_a"); s.Nudges == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the kicked space was not checked")
		}
		time.Sleep(5 * time.Millisecond)
	}
	e.w.mu.Lock()
	left := len(e.w.kicked)
	e.w.mu.Unlock()
	if left != 0 {
		t.Fatalf("%d kicks left", left)
	}
	stop()
	<-done
}

// A space that drops out of the active list (paused, archived) is reported as not watched, not as nudging or
// escalated as it last was, and keeps its episode for when it comes back.
func TestWatchdogPassMarksDroppedSpacePaused(t *testing.T) {
	e := newWDEnv(t)
	if s := e.pass(0); s.Nudges != 1 || s.Status() != "nudging" {
		t.Fatalf("first pass: %+v", s)
	}
	e.w.spaces = func(context.Context) ([]string, error) { return nil, nil }
	s := e.pass(time.Minute)
	if s.Guard != guardPaused || s.Status() != "off" {
		t.Fatalf("dropped space: %+v", s)
	}
	e.w.mu.Lock()
	ep := e.w.eps["spc_a"]
	e.w.mu.Unlock()
	if ep == nil || ep.nudges != 1 {
		t.Fatalf("the episode was lost: %+v", ep)
	}
	e.w.spaces = func(context.Context) ([]string, error) { return []string{"spc_a"}, nil }
	if s := e.pass(5 * time.Minute); s.Nudges != 2 {
		t.Fatalf("resumed: %+v", s)
	}
}

func TestWatchStateStatus(t *testing.T) {
	for _, tc := range []struct {
		st   WatchState
		want string
	}{
		{WatchState{Guard: guardOff}, "off"},
		{WatchState{Guard: guardPaused, Nudges: 2}, "off"},
		{WatchState{Guard: guardNoControl, Nudges: 2}, "idle"},
		{WatchState{Guard: guardNoBacklog}, "idle"},
		{WatchState{Guard: guardBusy, Nudges: 1}, "nudging"},
		{WatchState{Nudges: 3, Escalated: true}, "escalated"},
	} {
		if got := tc.st.Status(); got != tc.want {
			t.Errorf("%+v: %s, want %s", tc.st, got, tc.want)
		}
	}
}

// A worker run at turn 0 with no activity past worker_start_watchdog_minutes gets control exactly one notification
// naming the run and its task; a run with recent tool activity, or one that took a turn, gets none.
func TestWatchdogWorkerStuckAtZero(t *testing.T) {
	e := newWDEnv(t)
	e.lim[limits.WorkerStartWatchdog] = 10
	mk := func(id string) *fleet.Run {
		return &fleet.Run{ID: id, Kind: "worker", Role: "reviewer", Desired: "running", Observed: "running", Status: "working", StartedAt: tp(wdNow)}
	}
	stuck, busy, moving, away := mk("run_stuck"), mk("run_busy"), mk("run_moving"), mk("run_away")
	away.Observed = "unknown"
	tasks := map[string]*queue.Task{"run_stuck": {ID: "tsk_1", Title: "Review the thing"}}
	var sent, dedups []string
	e.w.workers = func(context.Context, string) ([]*fleet.Run, map[string]*queue.Task, error) {
		return []*fleet.Run{stuck, busy, moving, away}, tasks, nil
	}
	e.w.tell = func(_ context.Context, _, dedup, _, body string) {
		dedups = append(dedups, dedup)
		sent = append(sent, body)
	}
	e.pass(9 * time.Minute)
	if len(sent) != 0 {
		t.Fatalf("before the threshold: %v", sent)
	}
	moving.Turn = 1
	e.w.mu.Lock()
	e.w.act("run_busy").at = e.clk.Now() // a tool call 9 minutes in
	e.w.mu.Unlock()
	e.pass(2 * time.Minute)
	e.pass(time.Minute)
	if len(sent) != 1 || dedups[0] != "worker-not-started:run_stuck" || !strings.Contains(sent[0], "run_stuck") || !strings.Contains(sent[0], "tsk_1") ||
		strings.Contains(sent[0], "worker_stop") {
		t.Fatalf("notifications %v %v", dedups, sent)
	}
	// The busy run is flagged only once its own quiet time passes.
	e.pass(10 * time.Minute)
	if len(sent) != 2 || dedups[1] != "worker-not-started:run_busy" {
		t.Fatalf("after the busy run went quiet: %v", dedups)
	}
}

// Activity before a restart of central is unknown, not stuck: the clock starts at central's start.
func TestWatchdogWorkerStartAfterRestart(t *testing.T) {
	e := newWDEnv(t)
	e.lim[limits.WorkerStartWatchdog] = 10
	e.w.started = wdNow.Add(5 * time.Minute) // central came up 5 minutes after the run started, with no record of its calls
	r := &fleet.Run{ID: "run_old", Kind: "worker", Role: "reviewer", Desired: "running", Observed: "running", StartedAt: tp(wdNow)}
	var sent []string
	e.w.workers = func(context.Context, string) ([]*fleet.Run, map[string]*queue.Task, error) {
		return []*fleet.Run{r}, nil, nil
	}
	e.w.tell = func(_ context.Context, _, _, _, body string) { sent = append(sent, body) }
	e.pass(12 * time.Minute) // 12 minutes after the run, 7 after central started
	if len(sent) != 0 {
		t.Fatalf("flagged before a full interval after the restart: %v", sent)
	}
	e.pass(4 * time.Minute)
	if len(sent) != 1 {
		t.Fatalf("not flagged after a quiet interval: %v", sent)
	}
	// Runs that ended, wait at a limit, or are control are not stuck.
	for _, x := range []*fleet.Run{
		{ID: "a", Kind: "worker", Desired: "running", Observed: "exited", StartedAt: tp(ago(time.Hour))},
		{ID: "b", Kind: "worker", Desired: "running", Observed: "running", Limit: &fleet.RunLimit{}, StartedAt: tp(ago(time.Hour))},
		{ID: "c", Kind: "control", Desired: "running", Observed: "running", StartedAt: tp(ago(time.Hour))}} {
		if got := e.w.stuckAtZero([]*fleet.Run{x}, 10*time.Minute, wdNow.Add(time.Hour)); len(got) != 0 {
			t.Errorf("%s: not stuck: %+v", x.ID, got)
		}
	}
}
