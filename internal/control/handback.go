package control

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/agenttools"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/limits"
	"github.com/aurimasniekis/go-spaceman/internal/obs"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/queue"
)

// Worker hand-back check (ADR-0099 addendum, limit worker_idle_watchdog_minutes): a live worker whose turn ended while
// its task is still running, and that neither handed the task back (review, blocked) nor messaged control, is reminded
// once in its terminal; if that reminder goes unanswered as long again, control is told once. A worker that is
// waiting for control's answer is not reminded: control is told instead.

// handback is one idle stretch of a worker run whose task is still running. It lives in the watchdog's memory only, so
// a restart of central starts every stretch over at the first pass that sees it.
type handback struct {
	space     string
	task      string
	version   int64     // the task's version when the stretch began: any change of the task ends it
	turn      int64     // the run's finished turns when the stretch began
	msgAt     time.Time // when the run last messaged control, as of the stretch's start: a newer message ends it
	since     time.Time // the first pass that saw the stretch
	replied   bool      // the stretch began right after the run messaged control: it waits for control's answer
	nudged    time.Time // when the worker was reminded (zero: not yet)
	nudgeTurn int64     // its finished turns then: the reminder's own turn finishes as nudgeTurn+1
	told      bool      // control was told: nothing more happens in this stretch
}

// workerWait is what a pass reads of an idle worker run before it acts.
type workerWait struct {
	HumanOpen int64     // open human actions the run asked
	TakenOver bool      // an operator holds its terminal
	MsgAt     time.Time // when it last sent control a message (zero: never)
	MsgOpen   bool      // one of its messages to control is still open (not acknowledged, or a request not resolved)
}

// handbackText is the line typed into the worker's terminal.
func handbackText(taskID string) string {
	return "SpaceMan watchdog: your turn ended but task " + taskID + " is still running. If the work is done, set the task to review " +
		"with spaceman_task_set_status and a short result, and message control. If you are blocked or waiting, message control " +
		"(spaceman_message_send) saying what you need. (If you are only waiting for background work you started, message control " +
		"saying so: SpaceMan cannot always tell.)"
}

// checkHandbacks runs the hand-back check over one space's live worker runs.
func (w *Watchdog) checkHandbacks(ctx context.Context, spaceID string) {
	n := time.Duration(w.limit(ctx, spaceID, limits.WorkerIdleWatchdog)) * w.sv.IdleUnit
	if n <= 0 {
		w.forgetHandbacks(spaceID, nil)
		return
	}
	runs, tasks, err := w.workers(ctx, spaceID)
	if err != nil {
		w.sv.log.Warn("watchdog: read workers", slog.String("space.id", spaceID), obs.Err(err))
		return
	}
	now := w.sv.clock.Now()
	live := map[string]bool{}
	for _, r := range runs {
		if r.Kind != agenttools.KindWorker || r.Desired != "running" || r.Observed != "running" {
			continue
		}
		live[r.ID] = true
		if ctx.Err() != nil {
			return
		}
		w.handbackRun(ctx, spaceID, r, tasks[r.ID], n, now)
	}
	w.forgetHandbacks(spaceID, live)
}

// forgetHandbacks drops a space's stretches and working times of runs not in live (all of them when live is nil).
func (w *Watchdog) forgetHandbacks(spaceID string, live map[string]bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for id, h := range w.idle {
		if h.space == spaceID && !live[id] {
			delete(w.idle, id)
		}
	}
	for id, b := range w.busy {
		if b.space == spaceID && !live[id] {
			delete(w.busy, id)
		}
	}
}

// busyMark is when a pass last saw a live worker run working.
type busyMark struct {
	space string
	at    time.Time
}

// handbackRun advances one live worker run's stretch.
func (w *Watchdog) handbackRun(ctx context.Context, spaceID string, r *fleet.Run, t *queue.Task, n time.Duration, now time.Time) {
	w.mu.Lock()
	h := w.idle[r.ID]
	// Working includes "turn finished, background shells, monitors or sub-agents still running" (status_kind background,
	// reported as working): no reminder then. Where the program reports plain idle while background work runs, the check
	// cannot tell (ADR-0099 addendum).
	working := r.Status != "idle" && r.Status != "done" || r.Status == "working" && r.StatusKind == proto.StatusKindBackground
	if a := w.acts[r.ID]; a != nil && a.inflight > 0 {
		working = true
	}
	busyAt := w.busy[r.ID].at
	if working {
		w.busy[r.ID] = busyMark{space: spaceID, at: now}
	}
	drop := func() {
		delete(w.idle, r.ID)
		h = nil
	}
	switch {
	case t == nil || t.Status != queue.Running || r.Limit != nil:
		// No running task to hand back (a preempted task is ready again), or the run waits for its usage limit.
		drop()
	case h != nil && (h.task != t.ID || h.version != t.Version):
		drop() // the task changed: its status, or anything else about it
	case h != nil && h.nudged.IsZero() && (working || r.Turn != h.turn):
		drop() // a new turn before the reminder: the worker is doing something
	case h != nil && !h.nudged.IsZero() && r.Turn > h.nudgeTurn+1:
		drop() // more than the reminder's own turn: something else woke the worker
	}
	w.mu.Unlock()
	if t == nil || t.Status != queue.Running || r.Limit != nil || working {
		return // a reminded worker's own turn only pauses its stretch
	}
	wt, err := w.waits(ctx, spaceID, r.ID)
	if err != nil {
		w.sv.log.Warn("watchdog: read a worker's waits", slog.String(obs.FieldRunID, r.ID), obs.Err(err))
		return
	}
	w.mu.Lock()
	if wt.HumanOpen > 0 || wt.TakenOver {
		// Waiting for a person, or an operator is at its terminal: not stalled, and the clock starts over afterwards.
		delete(w.idle, r.ID)
		w.mu.Unlock()
		return
	}
	replied := false
	if h != nil && wt.MsgAt.After(h.msgAt) {
		// The run messaged control (answering the reminder, say): that stretch is over, and the next one waits on control.
		delete(w.idle, r.ID)
		h, replied = nil, true
	}
	if h == nil {
		// The stretch begins now. It waits on control when the run messaged control after the watchdog last saw it working
		// (its turn ended with that message), or when it just answered with a message.
		// After a restart busyAt is zero: any message then counts as the last act, the cautious side (no reminder).
		replied = replied || wt.MsgAt.After(busyAt)
		w.idle[r.ID] = &handback{space: spaceID, task: t.ID, version: t.Version, turn: r.Turn, msgAt: wt.MsgAt, since: now, replied: replied}
		w.mu.Unlock()
		return
	}
	act := ""
	switch {
	case h.told:
	case h.nudged.IsZero() && now.Sub(h.since) >= n && (wt.MsgOpen || h.replied):
		act = "waiting"
	case h.nudged.IsZero() && now.Sub(h.since) >= n:
		act = "remind"
	case !h.nudged.IsZero() && now.Sub(h.nudged) >= n:
		act = "unanswered"
	}
	since, nudged := h.since, h.nudged
	w.mu.Unlock()
	mins := func(d time.Duration) int64 { return int64(d / w.sv.IdleUnit) }
	task := "task " + t.ID + " (" + t.Title + ")"
	dedup := "worker-idle:" + r.ID + ":" + strconv.FormatInt(since.UnixMicro(), 10)
	switch act {
	case "remind":
		details := map[string]any{"space_id": spaceID, "task_id": t.ID, "idle_minutes": mins(now.Sub(since))}
		if err := w.remind(ctx, r, handbackText(t.ID), details); err != nil {
			// Refused (the worker turned busy, limited or taken over meanwhile) or failed: try again at the next pass.
			w.sv.log.Info("watchdog: worker reminder not sent", slog.String(obs.FieldRunID, r.ID), obs.Err(err))
			return
		}
		w.mu.Lock()
		h.nudged, h.nudgeTurn = now, r.Turn
		w.mu.Unlock()
		w.sv.log.Info("watchdog: worker reminded to hand back its task", slog.String("space.id", spaceID), slog.String(obs.FieldRunID, r.ID))
	case "waiting":
		w.tell(ctx, spaceID, dedup, "Worker run "+r.ID+" is waiting for you",
			fmt.Sprintf("Worker run %s (role %s) has been idle for %d minutes with %s still running. Its last message to you is still open "+
				"or was the last thing it did, so it is probably waiting for your answer: answer it (spaceman_messages shows it), or set the "+
				"task's status yourself.", r.ID, r.Role, mins(now.Sub(since)), task))
		w.markTold(h)
	case "unanswered":
		w.tell(ctx, spaceID, dedup, "Worker run "+r.ID+" did not hand back its task",
			fmt.Sprintf("Worker run %s (role %s) finished its turn %d minutes ago, but %s is still running: the run neither set it to review "+
				"or blocked nor messaged you. SpaceMan reminded it in its terminal %d minutes ago without an answer. Check its terminal "+
				"(spaceman_runs_list for its status), then tell the worker what to do or set the task's status yourself.",
				r.ID, r.Role, mins(now.Sub(since)), task, mins(now.Sub(nudged))))
		w.markTold(h)
	}
}

func (w *Watchdog) markTold(h *handback) {
	w.mu.Lock()
	h.told = true
	w.mu.Unlock()
}

// readWait reads an idle worker run's open human actions, takeover and messages to control.
func (w *Watchdog) readWait(ctx context.Context, spaceID, runID string) (workerWait, error) {
	var out workerWait
	var err error
	if out.HumanOpen, err = w.sv.human.PendingByRun(ctx, runID); err != nil {
		return out, err
	}
	tk, err := w.sv.fl.ActiveTakeover(ctx, runID)
	if err != nil {
		return out, err
	}
	out.TakenOver = tk != nil
	out.MsgAt, out.MsgOpen, err = w.sv.msgs.FromRunToControl(ctx, spaceID, runID)
	return out, err
}

// deliverWorker types the reminder into the worker's terminal (idle-gated safe input) and audits it.
func (w *Watchdog) deliverWorker(ctx context.Context, r *fleet.Run, text string, details map[string]any) error {
	return w.sv.fl.NudgeRun(ctx, WatchdogActor, r.ID, text, "worker.watchdog_nudged", details)
}
