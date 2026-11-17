package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/agenttools"
	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/human"
	"github.com/aurimasniekis/go-spaceman/internal/limits"
	"github.com/aurimasniekis/go-spaceman/internal/obs"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/queue"
)

// Control watchdog (SM-CTRL-008, ADR-0099): a live control that sits idle while work waits for it (unacknowledged
// messages, reviews whose workers finished, runs that ended, free worker slots with eligible tasks) is reminded through
// a one-line terminal notice, with backoff; when the reminders go unanswered the space's people are told, and control
// is optionally restarted. Busy, limited, person-waiting, taken-over and paused controls are left alone.

// WatchdogActor nudges and (when configured) restarts a stalled control.
var WatchdogActor = audit.Actor{Type: audit.ActorSystem, ID: "control-watchdog"}

// Watchdog timing: one pass a minute; reminders 5 minutes apart at first, doubling up to an hour (in units of the
// supervisor's IdleUnit, one minute outside tests).
const (
	watchEvery     = time.Minute
	nudgeFirstGap  = 5 * time.Minute
	nudgeMaxGap    = time.Hour
	activityExpiry = 24 * time.Hour
	humanAskTool   = "spaceman_human_ask"
)

// Reasons a pass leaves a space alone (the watchdog's state for it says which).
const (
	guardOff       = "off"
	guardPaused    = "space_not_active"
	guardNoControl = "no_live_control"
	guardTakeover  = "taken_over"
	guardLimit     = "usage_limit"
	guardHuman     = "waiting_for_person"
	guardBusy      = "busy"
	guardNoBacklog = "no_backlog"
	guardActive    = "recently_active"
)

// Backlog is the work waiting for control, each item older than the space's watchdog threshold.
type Backlog struct {
	Messages  int `json:"messages"`   // unacknowledged messages to control
	Reviews   int `json:"reviews"`    // tasks in review whose worker finished
	Failed    int `json:"failed"`     // worker or diagnostic runs that failed (a failed launch included) or were lost
	Exited    int `json:"exited"`     // worker or diagnostic runs that exited on their own
	Startable int `json:"startable"`  // eligible ready tasks while worker slots are free
	FreeSlots int `json:"free_slots"` // free worker slots (counted only with startable tasks)
}

// Empty reports whether nothing waits.
func (b Backlog) Empty() bool { return b.Messages+b.Reviews+b.Failed+b.Exited+b.Startable == 0 }

// String names the backlog by counts only: no titles, IDs or content (those are read through the tools).
func (b Backlog) String() string {
	var parts []string
	add := func(n int, one, many string) {
		if n > 0 {
			parts = append(parts, plural(n, one, many))
		}
	}
	add(b.Messages, "unacknowledged message", "unacknowledged messages")
	add(b.Reviews, "task in review with a finished worker", "tasks in review with finished workers")
	add(b.Failed, "failed run", "failed runs")
	add(b.Exited, "ended run", "ended runs")
	if b.Startable > 0 {
		parts = append(parts, fmt.Sprintf("%s with %s free", plural(b.Startable, "ready task", "ready tasks"), plural(b.FreeSlots, "worker slot", "worker slots")))
	}
	return strings.Join(parts, ", ")
}

// nudgeText is the line typed into control's terminal.
func nudgeText(b Backlog) string {
	return "SpaceMan watchdog: " + b.String() + " waiting for you. Check spaceman_messages, the queue (spaceman_tasks_list) and " +
		"spaceman_runs_list, acknowledge what you have handled with spaceman_message_ack, then act on the rest."
}

// activity is what central saw of a run's tool calls and turns (in memory; a restart starts it over).
type activity struct {
	at       time.Time // the last tool call started or finished
	tool     string    // the last tool called
	inflight int       // tool calls still running (one waiting for an approval included)
	turn     int64     // the run's turn when the watchdog last looked
	seen     bool      // turn was read at least once
	turnAt   time.Time // when the watchdog saw the turn advance
	looked   time.Time // when a pass last read the turn (keeps the record of a run without tool calls)
	asked    bool      // the run asked a person (spaceman_human_ask) since central started
	askTurn  int64     // the run's finished turns when it last asked: the asking turn finishes as askTurn+1
}

// observation is one space's state as a watchdog pass reads it.
type observation struct {
	SpaceState string
	Control    *fleet.Run // the space's newest control run (nil: none)
	Parked     bool       // control has a usage-limit wait, or the space's control is parked
	Act        activity
	Started    time.Time   // when the watchdog started (activity before it is unknown)
	HumanOpen  int64       // open human actions control's run asked
	Unacked    []time.Time // when each unacknowledged message to control started waiting
	Tasks      []*queue.Task
	Runs       []*fleet.Run    // the space's recent runs, live ones always included
	MaxRuns    int64           // the space's max_concurrent_runs
	Held       map[string]bool // roles (of eligible ready tasks) no account would launch now: every one is limited
}

// verdict is a pass's judgement of one space.
type verdict struct {
	Guard   string    // non-empty: no nudge, and why
	Backlog Backlog   // what waits (also filled when a guard holds)
	Oldest  time.Time // when the oldest unacknowledged message or startable task started waiting (zero: none)
	Last    time.Time // control's last activity
}

// counted reports whether the backlog was read: a pass that stops at the space, the control run or the switch leaves
// it empty without anything having been handled.
func (v verdict) counted() bool {
	return v.Guard != guardOff && v.Guard != guardPaused && v.Guard != guardNoControl
}

// assess decides whether control is stalled: there is backlog older than n and control has done nothing for n, and no
// guard holds (ADR-0099 decisions 2-4). It is pure, so every guard is tested on its own.
func assess(o observation, n time.Duration, now time.Time) verdict {
	if n <= 0 {
		return verdict{Guard: guardOff}
	}
	if o.SpaceState != "active" {
		return verdict{Guard: guardPaused}
	}
	c := o.Control
	if c == nil || c.Desired != "running" || c.Observed != "running" {
		return verdict{Guard: guardNoControl}
	}
	last := latest(o.Act.at, o.Act.turnAt, o.Started)
	if c.StartedAt != nil {
		last = latest(last, *c.StartedAt)
	}
	v := verdict{Last: last}
	v.Backlog, v.Oldest = backlog(o, last, now.Add(-n))
	switch {
	case c.Takeover != nil:
		v.Guard = guardTakeover
	case c.Limit != nil || o.Parked:
		v.Guard = guardLimit
	case c.Status == "blocked" || o.HumanOpen > 0 && waitsOnAsk(o):
		v.Guard = guardHuman
	case o.Act.inflight > 0 || c.Status != "idle" && c.Status != "done":
		v.Guard = guardBusy
	case v.Backlog.Empty():
		v.Guard = guardNoBacklog
	case now.Sub(last) < n:
		v.Guard = guardActive
	}
	return v
}

// waitsOnAsk reports whether control, with a question of its run still open, is waiting for the answer: it asked in
// its latest turn (the asking turn may have finished since, but no turn after it has; other tool calls in that turn,
// such as telling a worker, do not count). With nothing known of its tool calls (central restarted), any open question
// control asked counts until the watchdog sees a turn of it finish.
func waitsOnAsk(o observation) bool {
	if o.Act.asked {
		return o.Control.Turn <= o.Act.askTurn+1
	}
	return o.Act.tool == "" && o.Act.turnAt.IsZero()
}

// backlog counts what waits for control longer than cutoff, and returns when the oldest of what only control's handling
// clears (an unacknowledged message, a startable task) started waiting.
func backlog(o observation, last, cutoff time.Time) (Backlog, time.Time) {
	var b Backlog
	var oldest time.Time
	older := func(t time.Time) {
		if oldest.IsZero() || t.Before(oldest) {
			oldest = t
		}
	}
	for _, t := range o.Unacked {
		if !t.After(cutoff) {
			b.Messages++
			older(t)
		}
	}
	runs := make(map[string]*fleet.Run, len(o.Runs))
	live := int64(0)
	var ctlStart time.Time
	if o.Control.StartedAt != nil {
		ctlStart = *o.Control.StartedAt
	}
	for _, r := range o.Runs {
		runs[r.ID] = r
		if r.Kind != agenttools.KindWorker && r.Kind != "diagnostic" {
			continue
		}
		if fleet.Live(r.Observed) {
			if r.Limit == nil {
				live++ // a run waiting for its usage limit frees its slot (limits.Admit)
			}
			continue
		}
		// Ended on its own (nobody asked it to stop), not parked at a usage limit, after this control started and after
		// control last did anything, and long enough ago.
		if r.Desired != "running" || r.EndReason == fleet.EndUsageLimit || r.EndedAt == nil {
			continue
		}
		if !r.EndedAt.After(ctlStart) || !r.EndedAt.After(last) || r.EndedAt.After(cutoff) {
			continue
		}
		if r.Observed == "exited" {
			b.Exited++
		} else {
			b.Failed++
		}
	}
	startable := 0
	var readySince time.Time
	for _, t := range o.Tasks {
		if t.UpdatedAt.After(cutoff) {
			continue
		}
		switch t.Status {
		case queue.Review:
			// Like an ended run, a review counts until control's first activity after it entered review (its last
			// change): control was told, and a review may rightly wait for a long time (a person merging, say).
			if !t.UpdatedAt.After(last) {
				continue
			}
			if r := runs[t.AssigneeRun]; r == nil || !fleet.Live(r.Observed) || r.Status == "done" {
				b.Reviews++
			}
		case queue.Ready:
			// Eligible now, and some account would launch it (a task whose every account is limited waits for a reset).
			if len(t.Waiting) == 0 && !o.Held[t.AssigneeRole] {
				startable++
				if readySince.IsZero() || t.UpdatedAt.Before(readySince) {
					readySince = t.UpdatedAt
				}
			}
		}
	}
	if free := o.MaxRuns - live; startable > 0 && free > 0 {
		b.Startable, b.FreeSlots = startable, int(free)
		older(readySince)
	}
	return b, oldest
}

func latest(ts ...time.Time) time.Time {
	var out time.Time
	for _, t := range ts {
		if t.After(out) {
			out = t
		}
	}
	return out
}

// nudgeGap is the wait after the k-th nudge of an episode: 5 minutes, doubling, at most an hour, where minute is one
// minute (tests shorten it, like the idle unit).
func nudgeGap(k int, minute time.Duration) time.Duration {
	d := nudgeFirstGap
	for i := 1; i < k && d < nudgeMaxGap; i++ {
		d *= 2
	}
	return time.Duration(int64(min(d, nudgeMaxGap)/time.Minute)) * minute
}

// episode is one stretch of the same backlog left waiting by one control run. It ends when that backlog empties or
// shrinks (decision 6), not when control merely takes a turn: a nudge into an idle program always starts one.
type episode struct {
	runID     string
	seen      Backlog   // the backlog at the last pass that read it
	oldest    time.Time // its oldest waiting item then
	nudges    int
	first     time.Time
	last      time.Time
	next      time.Time
	escalated bool
	restarted bool
}

// WatchState is a space's watchdog state (read by the API and CLI, a follow-up).
type WatchState struct {
	Guard        string    `json:"guard,omitempty"`
	Backlog      Backlog   `json:"backlog"`
	LastActivity time.Time `json:"last_activity"`
	ControlRun   string    `json:"control_run,omitempty"`
	Nudges       int       `json:"nudges"`
	LastNudge    time.Time `json:"last_nudge"`
	NextNudge    time.Time `json:"next_nudge"`
	Escalated    bool      `json:"escalated"`
	CheckedAt    time.Time `json:"checked_at"`
}

// Status is the state's one-word summary: "off" (the watchdog is off or the space not active), "idle" (nothing is being
// nudged, including a control that is not live), "nudging" or "escalated".
func (s WatchState) Status() string {
	switch {
	case s.Guard == guardOff || s.Guard == guardPaused:
		return "off"
	case s.Guard == guardNoControl:
		return "idle"
	case s.Escalated:
		return "escalated"
	case s.Nudges > 0:
		return "nudging"
	}
	return "idle"
}

// Watchdog watches every active space's live control.
type Watchdog struct {
	sv      *Supervisor
	wake    chan struct{}
	kicked  map[string]bool // spaces asked for a pass by Kick (guarded by mu)
	started time.Time

	mu       sync.Mutex
	acts     map[string]*activity // by run
	eps      map[string]*episode  // by space
	state    map[string]WatchState
	restarts map[string]bool // control runs the watchdog stopped to restart

	// Seams for tests; NewWatchdog points them at the supervisor's services.
	turnOf   func(ctx context.Context, runID string) (int64, bool)
	spaces   func(ctx context.Context) ([]string, error)
	observe  func(ctx context.Context, spaceID string) (observation, error)
	nudge    func(ctx context.Context, r *fleet.Run, text string, details map[string]any) error
	escalate func(ctx context.Context, spaceID string, r *fleet.Run, ep *episode, b Backlog, quiet time.Duration)
}

// NewWatchdog returns the supervisor's watchdog (sv.Watchdog); Run starts it.
func NewWatchdog(sv *Supervisor) *Watchdog {
	w := &Watchdog{sv: sv, wake: make(chan struct{}, 1), kicked: map[string]bool{}, started: sv.clock.Now(), acts: map[string]*activity{}, eps: map[string]*episode{},
		state: map[string]WatchState{}, restarts: map[string]bool{}}
	w.turnOf, w.spaces, w.observe, w.nudge, w.escalate = w.runTurn, w.activeSpaces, w.read, w.deliver, w.raise
	sv.Watchdog = w
	return w
}

// Tools wraps the agent tool dispatcher: each call of a run counts as its activity, and a call still running (waiting
// for an approval, say) marks the run busy.
func (w *Watchdog) Tools(next fleet.ToolDispatcher) fleet.ToolDispatcher {
	return func(ctx context.Context, p *fleet.RunPrincipal, tool string, args json.RawMessage) (json.RawMessage, error) {
		if tool == proto.ToolCatalog {
			return next(ctx, p, tool, args)
		}
		w.toolStarted(p.RunID, tool)
		defer w.toolFinished(p.RunID)
		out, err := next(ctx, p, tool, args)
		if tool == humanAskTool && err == nil {
			w.asked(ctx, p.RunID)
		}
		return out, err
	}
}

func (w *Watchdog) toolStarted(runID, tool string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	a := w.act(runID)
	a.at, a.tool = w.sv.clock.Now(), tool
	a.inflight++
}

func (w *Watchdog) toolFinished(runID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	a := w.act(runID)
	a.at = w.sv.clock.Now()
	if a.inflight > 0 {
		a.inflight--
	}
}

// asked records that a run asked a person, with its finished turns now (the asking turn is still running).
func (w *Watchdog) asked(ctx context.Context, runID string) {
	turn, ok := w.turnOf(ctx, runID)
	w.mu.Lock()
	defer w.mu.Unlock()
	a := w.act(runID)
	if !ok {
		turn = a.turn // the turn the last pass saw: at worst the guard ends a turn early
	}
	a.asked, a.askTurn = true, turn
}

// runTurn reads a run's finished turns.
func (w *Watchdog) runTurn(ctx context.Context, runID string) (int64, bool) {
	r, err := w.sv.fl.GetRun(ctx, runID)
	if err != nil {
		w.sv.log.Warn("watchdog: read the asking run", slog.String(obs.FieldRunID, runID), obs.Err(err))
		return 0, false
	}
	return r.Turn, true
}

// act returns a run's activity record; callers hold mu.
func (w *Watchdog) act(runID string) *activity {
	a := w.acts[runID]
	if a == nil {
		a = &activity{}
		w.acts[runID] = a
	}
	return a
}

// Kick asks for a pass over one space soon (its control run turned idle).
func (w *Watchdog) Kick(spaceID string) {
	w.mu.Lock()
	w.kicked[spaceID] = true
	w.mu.Unlock()
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// Run passes over the spaces once a minute, and over a kicked space when kicked, until ctx ends.
func (w *Watchdog) Run(ctx context.Context) {
	t := time.NewTicker(watchEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.Pass(ctx)
		case <-w.wake:
			w.mu.Lock()
			ids := slices.Sorted(maps.Keys(w.kicked))
			clear(w.kicked)
			w.mu.Unlock()
			for _, id := range ids {
				if ctx.Err() != nil {
					return
				}
				w.check(ctx, id)
			}
		}
	}
}

// State returns a space's watchdog state as the last pass saw it.
func (w *Watchdog) State(spaceID string) (WatchState, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	s, ok := w.state[spaceID]
	return s, ok
}

// Pass checks every active space once.
func (w *Watchdog) Pass(ctx context.Context) {
	ids, err := w.spaces(ctx)
	if err != nil {
		w.sv.log.Warn("watchdog: list spaces", obs.Err(err))
		return
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		w.check(ctx, id)
	}
	// A space that dropped out of the active list (paused or archived) is no longer watched: its state says so, and
	// keeps its episode in case it resumes.
	w.mu.Lock()
	for id, st := range w.state {
		if !slices.Contains(ids, id) && st.Guard != guardPaused {
			w.state[id] = WatchState{Guard: guardPaused, CheckedAt: w.sv.clock.Now()}
		}
	}
	w.mu.Unlock()
	w.prune()
}

// prune forgets the activity of runs not seen for a day.
func (w *Watchdog) prune() {
	now := w.sv.clock.Now()
	w.mu.Lock()
	defer w.mu.Unlock()
	for id, a := range w.acts {
		if a.inflight == 0 && now.Sub(latest(a.at, a.turnAt, a.looked)) > activityExpiry {
			delete(w.acts, id)
		}
	}
}

func (w *Watchdog) limit(ctx context.Context, spaceID, key string) int64 {
	if w.sv.Limit == nil {
		return 0
	}
	return w.sv.Limit(ctx, spaceID, key)
}

// check runs the watchdog for one space.
func (w *Watchdog) check(ctx context.Context, spaceID string) {
	n := time.Duration(w.limit(ctx, spaceID, limits.ControlWatchdog)) * w.sv.IdleUnit
	now := w.sv.clock.Now()
	if n <= 0 {
		w.settle(spaceID, WatchState{Guard: guardOff, CheckedAt: now}, nil)
		return
	}
	o, err := w.observe(ctx, spaceID)
	if err != nil {
		w.sv.log.Warn("watchdog: read space", slog.String("space.id", spaceID), obs.Err(err))
		return
	}
	if o.Control != nil {
		// A new turn is activity: control answered something, even without a tool call.
		w.mu.Lock()
		a := w.act(o.Control.ID)
		if a.seen && o.Control.Turn != a.turn {
			a.turnAt = now
		}
		a.turn, a.seen, a.looked = o.Control.Turn, true, now
		o.Act = *a
		w.mu.Unlock()
	}
	o.Started = w.started
	v := assess(o, n, now)
	st := WatchState{Guard: v.Guard, Backlog: v.Backlog, LastActivity: v.Last, CheckedAt: now}
	if o.Control != nil {
		st.ControlRun = o.Control.ID
	}
	w.mu.Lock()
	ep := w.eps[spaceID]
	if ep != nil && (o.Control == nil || ep.runID != o.Control.ID || v.counted() && ep.progress(v)) {
		// Another control run, or the backlog emptied or shrank (whoever handled it): the episode is over.
		delete(w.eps, spaceID)
		ep = nil
	}
	if ep != nil && v.counted() {
		ep.seen, ep.oldest = v.Backlog, v.Oldest
	}
	w.mu.Unlock()
	if v.Guard != "" {
		// A guard (control busy, waiting on a person, recently active, …) only pauses a running episode: it keeps its
		// count and its backoff, so a control that answers each nudge with a turn but leaves the backlog is still
		// reminded less and less often, and escalated.
		w.settle(spaceID, st, ep)
		return
	}
	w.mu.Lock()
	if ep == nil {
		ep = &episode{runID: o.Control.ID, seen: v.Backlog, oldest: v.Oldest}
		w.eps[spaceID] = ep
	}
	due := ep.nudges == 0 || !now.Before(ep.next)
	w.mu.Unlock()
	if !due {
		w.settle(spaceID, st, ep)
		return
	}
	if k := w.limit(ctx, spaceID, limits.ControlWatchdogNudges); k > 0 && int64(ep.nudges) >= k && !ep.escalated {
		w.escalate(ctx, spaceID, o.Control, ep, v.Backlog, now.Sub(v.Last))
		ep.escalated = true
		if ep.restarted {
			w.settle(spaceID, st, ep)
			return // control is being stopped to start again: no reminder into a terminal that is going away
		}
	}
	details := map[string]any{"space_id": spaceID, "nudge": ep.nudges + 1, "messages": v.Backlog.Messages, "reviews": v.Backlog.Reviews,
		"failed": v.Backlog.Failed, "exited": v.Backlog.Exited, "startable": v.Backlog.Startable, "quiet_minutes": int64(now.Sub(v.Last) / time.Minute)}
	if err := w.nudge(ctx, o.Control, nudgeText(v.Backlog), details); err != nil {
		// Refused (control turned busy, limited or taken over meanwhile) or failed: try again at the next pass.
		w.sv.log.Info("watchdog: nudge not sent", slog.String(obs.FieldRunID, o.Control.ID), obs.Err(err))
		w.settle(spaceID, st, ep)
		return
	}
	w.mu.Lock()
	if ep.nudges == 0 {
		ep.first = now
	}
	ep.nudges++
	ep.last = now
	ep.next = now.Add(nudgeGap(ep.nudges, w.sv.IdleUnit))
	w.mu.Unlock()
	w.sv.log.Info("watchdog: control nudged", slog.String("space.id", spaceID), slog.String(obs.FieldRunID, o.Control.ID), slog.Int("nudge", ep.nudges))
	w.settle(spaceID, st, ep)
}

// progress reports whether the backlog emptied or shrank since the episode last saw it: fewer unacknowledged messages
// or startable tasks, or the oldest of them handled. Reviews and ended runs count only until control's next activity,
// so their dropping out is no sign of handling and is not judged; when they were all that waited, the backlog empties.
func (ep *episode) progress(v verdict) bool {
	b := v.Backlog
	return b.Empty() || b.Messages < ep.seen.Messages || b.Startable < ep.seen.Startable || !ep.oldest.IsZero() && v.Oldest.After(ep.oldest)
}

// settle records a space's state after a pass.
func (w *Watchdog) settle(spaceID string, st WatchState, ep *episode) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if ep != nil {
		st.Nudges, st.LastNudge, st.NextNudge, st.Escalated = ep.nudges, ep.last, ep.next, ep.escalated
	}
	if st.Guard == guardOff {
		delete(w.eps, spaceID)
	}
	w.state[spaceID] = st
}

// activeSpaces lists the active spaces.
func (w *Watchdog) activeSpaces(ctx context.Context) ([]string, error) {
	sps, err := w.sv.fl.ListSpaces(ctx, nil)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, s := range sps {
		if s.State == "active" {
			out = append(out, s.ID)
		}
	}
	return out, nil
}

// watchRuns is how many of a space's newest runs a pass reads (live runs are always included).
const watchRuns = 200

// read gathers a space's observation from the supervisor's services.
func (w *Watchdog) read(ctx context.Context, spaceID string) (observation, error) {
	var o observation
	sp, err := w.sv.fl.GetSpace(ctx, spaceID)
	if err != nil {
		return o, err
	}
	o.SpaceState = sp.State
	last, err := w.sv.fl.LatestControlRun(ctx, spaceID)
	if ae := (*apperr.Error)(nil); errors.As(err, &ae) && ae.Code == apperr.CodeNotFound {
		return o, nil // the space never had a control run
	} else if err != nil {
		return o, err
	}
	if last.Desired != "running" || last.Observed != "running" {
		return o, nil // no live control: auto-start and recovery own that
	}
	if o.Control, err = w.sv.fl.GetRun(ctx, last.ID); err != nil { // GetRun sets the takeover
		return o, err
	}
	if lw, err := w.sv.fl.LimitWaitForRun(ctx, last.ID); err != nil {
		return o, err
	} else if lw != nil {
		o.Parked = true
	}
	if parked, err := w.sv.fl.ControlParked(ctx, spaceID); err != nil {
		return o, err
	} else if parked {
		o.Parked = true
	}
	if o.HumanOpen, err = w.sv.human.PendingByRun(ctx, last.ID); err != nil {
		return o, err
	}
	if o.Unacked, err = w.sv.msgs.Unacknowledged(ctx, spaceID, last.ID, last.Role, true); err != nil {
		return o, err
	}
	if o.Tasks, _, err = w.sv.q.List(ctx, spaceID); err != nil {
		return o, err
	}
	if o.Runs, err = w.sv.fl.ListRuns(ctx, spaceID, watchRuns); err != nil {
		return o, err
	}
	o.MaxRuns = w.limit(ctx, spaceID, limits.ConcurrentRuns)
	// Ready tasks no account would launch now (every account of their role's pool is limited) are not waiting on
	// control: they start after a reset. Read once per role.
	for _, t := range o.Tasks {
		if t.Status != queue.Ready || len(t.Waiting) > 0 {
			continue
		}
		if _, done := o.Held[t.AssigneeRole]; done {
			continue
		}
		held, err := w.sv.fl.LaunchHeld(ctx, spaceID, t.AssigneeRole)
		if err != nil {
			return o, err
		}
		if o.Held == nil {
			o.Held = map[string]bool{}
		}
		o.Held[t.AssigneeRole] = held
	}
	return o, nil
}

// deliver types the nudge into control's terminal (idle-gated safe input) and audits it.
func (w *Watchdog) deliver(ctx context.Context, r *fleet.Run, text string, details map[string]any) error {
	return w.sv.fl.NudgeRun(ctx, WatchdogActor, r.ID, text, "control.watchdog_nudged", details)
}

const watchdogRestartPrompt = "SpaceMan's watchdog restarted you: work was waiting for you and you did not answer its reminders. " +
	"Check spaceman_messages, the inbox and the queue, then continue."

// raise tells the space's people that control does not answer (a human action, deduplicated per episode) and, when the
// space turned it on, restarts control within the supervisor's restart budget.
func (w *Watchdog) raise(ctx context.Context, spaceID string, r *fleet.Run, ep *episode, b Backlog, quiet time.Duration) {
	sv := w.sv
	name := spaceID
	if sp, err := sv.fl.GetSpace(ctx, spaceID); err == nil {
		name = sp.Name
	}
	restart := w.limit(ctx, spaceID, limits.ControlWatchdogRestart) == 1
	note := "Restarting control is off for this space (limit control_watchdog_restart)."
	if restart {
		n, err := sv.fl.CountRunsCreatedBy(ctx, spaceID, agenttools.KindControl, SupervisorActor, sv.clock.Now().Add(-sv.Window))
		switch {
		case err != nil:
			sv.log.Warn("watchdog: count control restarts", obs.Err(err))
			restart = false
		case n >= sv.MaxRestarts:
			restart = false
			note = fmt.Sprintf("Control was already restarted %d times in the last %s, so the watchdog did not restart it.", n, sv.Window)
		default:
			note = "The watchdog is restarting control, resuming its session."
		}
	}
	_, _, err := sv.human.Create(ctx, audit.System, spaceID, human.CreateInput{Type: human.TypeTodo, Priority: "high",
		Title: "Control is not responding in " + name,
		Body: fmt.Sprintf("Control has done nothing for %d minutes while %s wait for it. SpaceMan reminded it %d times in its terminal without an answer. %s",
			int64(quiet/time.Minute), b.String(), ep.nudges, note),
		Requester: human.Requester{Kind: "system", Label: "Control watchdog"},
		Request:   []byte(`{"steps":["Open the control run and read its terminal","Answer it, or stop and launch control again"]}`),
		DedupKey:  fmt.Sprintf("control-watchdog:%s:%d", r.ID, ep.first.UnixMicro()), Internal: true})
	if err != nil {
		sv.log.Warn("watchdog: raise attention", obs.Err(err))
	}
	if !restart {
		return
	}
	w.mu.Lock()
	w.restarts[r.ID] = true
	w.mu.Unlock()
	if _, err := sv.fl.StopRun(ctx, WatchdogActor, r.ID, "interrupt", "control did not answer the watchdog's reminders; SpaceMan starts it again"); err != nil {
		w.mu.Lock()
		delete(w.restarts, r.ID)
		w.mu.Unlock()
		sv.log.Warn("watchdog: stop control", slog.String(obs.FieldRunID, r.ID), obs.Err(err))
		return
	}
	ep.restarted = true
}

// takeRestart reports (once) whether the watchdog stopped this control run to start it again.
func (w *Watchdog) takeRestart(runID string) bool {
	if w == nil {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	ok := w.restarts[runID]
	delete(w.restarts, runID)
	return ok
}
