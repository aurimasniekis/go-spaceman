package control

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/agenttools"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/human"
	"github.com/aurimasniekis/go-spaceman/internal/inbox"
	"github.com/aurimasniekis/go-spaceman/internal/messages"
	"github.com/aurimasniekis/go-spaceman/internal/obs"
	"github.com/aurimasniekis/go-spaceman/internal/queue"
)

// Supervisor keeps control informed and running (SM-CTRL-002):
//   - control gets a durable notification for actionable events (a new inbox item or task it did not create, a task
//     made ready or moved to review, blocked or failed by someone else, a worker run that ended); the notifier wakes it
//     when idle, and a
//     control started later reads them then;
//   - a control run that ends while still wanted is relaunched, resuming its session, at most MaxRestarts times per
//     Window; after that SpaceMan stops restarting and asks people to look (an urgent human action);
//   - with no control run live, control is started for waiting work and stopped after its idle time (lifecycle.go,
//     ADR-0056).
type Supervisor struct {
	fl    *fleet.Service
	q     *queue.Service
	msgs  *messages.Service
	human *human.Service
	log   *slog.Logger
	clock clock.Clock

	// Inbox holds the items whose control notice waits for a paused space to resume (SM-EVT-014). Nil: none are.
	Inbox *inbox.Service

	MaxRestarts int64
	Window      time.Duration
	// base is the server's lifetime context (Bind); handlers stop with it.
	base context.Context
	// Async runs handlers in a goroutine (production; hooks are called on request paths). Tests call them directly.
	Async bool

	lifecycle
}

// SupervisorActor creates control restarts; restarts are counted by it.
var SupervisorActor = audit.Actor{Type: audit.ActorSystem, ID: "control-supervisor"}

// NewSupervisor returns a supervisor.
func NewSupervisor(fl *fleet.Service, q *queue.Service, msgs *messages.Service, hs *human.Service, log *slog.Logger, c clock.Clock) *Supervisor {
	if c == nil {
		c = clock.System{}
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Supervisor{fl: fl, q: q, msgs: msgs, human: hs, log: obs.Component(log, "control"), clock: c, MaxRestarts: 3, Window: time.Hour, Async: true,
		base: context.Background(), lifecycle: lifecycle{MaxAutostarts: 6, IdleUnit: time.Minute, idle: map[string]*time.Timer{}}}
}

// Bind ties the handlers to the server's lifetime (Serve calls it before accepting connections).
func (sv *Supervisor) Bind(ctx context.Context) { sv.base = ctx }

func (sv *Supervisor) do(f func(ctx context.Context)) {
	ctx := sv.base
	if ctx.Err() != nil {
		return
	}
	if sv.Async {
		go f(ctx)
		return
	}
	f(ctx)
}

func (sv *Supervisor) notify(ctx context.Context, spaceID, dedup, subject, body string) {
	if _, _, err := sv.msgs.Send(ctx, audit.System, spaceID, messages.From{Kind: "system", Label: "SpaceMan"},
		messages.SendInput{To: "control", Kind: "notification", Subject: subject, Body: body, DedupKey: dedup}); err != nil {
		sv.log.Warn("notify control", obs.Err(err))
	}
}

// Notify sends control a durable notification (incident decisions from the queue, SM-QUEUE-009).
func (sv *Supervisor) Notify(spaceID, dedup, subject, body string) {
	sv.do(func(ctx context.Context) { sv.notify(ctx, spaceID, dedup, subject, body) })
}

// OnInboxItem notifies control of a new inbox item it did not add itself. An item whose notice is deferred (its
// subscription defers while the space is paused, SM-EVT-014) is announced when the space resumes instead, and a human
// response routed to control is not announced twice: control already has the response's own notification.
func (sv *Supervisor) OnInboxItem(it *inbox.Item) {
	if it.NoticeDeferred || (it.SourceKind == inbox.SourceHumanResponse && it.Trusted["routed_to"] == "control") {
		return
	}
	sv.do(func(ctx context.Context) {
		if it.SourceKind == "worker" && it.SourceID != "" {
			if r, err := sv.fl.GetRun(ctx, it.SourceID); err == nil && r.Kind == agenttools.KindControl {
				return
			}
		}
		sv.notifyItem(ctx, it, "")
	})
}

func (sv *Supervisor) notifyItem(ctx context.Context, it *inbox.Item, note string) {
	sv.notify(ctx, it.SpaceID, "inbox:"+it.ID, "New inbox item: "+it.Title,
		fmt.Sprintf("Inbox item %s (%s, %s urgency).%s Read it with spaceman_inbox_get.", it.ID, it.SourceKind, it.Urgency, note))
}

// OnSpaceState sends the notices deferred while the space was paused once it is active again (fleet OnSpaceState).
func (sv *Supervisor) OnSpaceState(spaceID, state string) {
	if state != "active" || sv.Inbox == nil {
		return
	}
	sv.do(func(ctx context.Context) { sv.flushDeferred(ctx, spaceID) })
}

// FlushDeferred sends the deferred notices of every active space: those of a resume central did not finish handling
// before it stopped. Serve runs it once at start.
func (sv *Supervisor) FlushDeferred(ctx context.Context) {
	if sv.Inbox == nil {
		return
	}
	spaces, err := sv.Inbox.SpacesWithDeferredNotices(ctx)
	if err != nil {
		sv.log.Warn("list deferred inbox notices", obs.Err(err))
		return
	}
	for _, id := range spaces {
		sv.flushDeferred(ctx, id)
	}
}

// flushDeferred tells control about the items that arrived while its space was paused, oldest first. Each notice is
// sent before its flag is cleared and deduplicated by item, so a crash in between sends it once more, never zero times.
// Items control has already handled (it reads the inbox itself under Hold) are cleared without a notice.
func (sv *Supervisor) flushDeferred(ctx context.Context, spaceID string) {
	for range 20 { // at most 20 rounds of 500 items in one go; the rest waits for the next resume or start
		items, err := sv.Inbox.DeferredNotices(ctx, spaceID)
		if err != nil {
			sv.log.Warn("list deferred inbox notices", obs.Err(err))
			return
		}
		if len(items) == 0 {
			return
		}
		if sp, err := sv.fl.GetSpace(ctx, spaceID); err != nil || sp.State != "active" {
			return // paused again (or gone) meanwhile: the notices wait for the next resume
		}
		for _, it := range items {
			if it.Status == inbox.StatusNew {
				sv.notifyItem(ctx, it, " It arrived while the space was paused.")
			}
			if err := sv.Inbox.NoticeSent(ctx, it); err != nil {
				sv.log.Warn("clear deferred inbox notice", obs.Err(err))
				return
			}
		}
	}
}

// OnTaskCreated notifies control of a task someone else added to the queue (an operator, the API, another service):
// control decides when it runs. Tasks control creates itself are not announced.
func (sv *Supervisor) OnTaskCreated(actor audit.Actor, t *queue.Task) {
	sv.do(func(ctx context.Context) {
		if actor.Type == audit.ActorAgent {
			if r, err := sv.fl.GetRun(ctx, actor.ID); err == nil && r.Kind == agenttools.KindControl {
				return
			}
		}
		sv.notify(ctx, t.SpaceID, "task-new:"+t.ID, "New task: "+t.Title,
			fmt.Sprintf("Task %s (%s, %s priority) was added to the queue. See whether it can start with spaceman_queue_next.", t.ID, t.Status, t.Priority))
	})
}

// OnTaskStatus notifies control when a task needs its decision: made ready (it can start), or moved to review,
// blocked or failed, by someone else.
func (sv *Supervisor) OnTaskStatus(actor audit.Actor, t *queue.Task, from string) {
	if t.Status != queue.Ready && t.Status != "review" && t.Status != "blocked" && t.Status != "failed" {
		return
	}
	if t.Status == from {
		return
	}
	sv.do(func(ctx context.Context) {
		if actor.Type == audit.ActorAgent {
			if r, err := sv.fl.GetRun(ctx, actor.ID); err == nil && r.Kind == agenttools.KindControl {
				return
			}
		}
		sv.notify(ctx, t.SpaceID, fmt.Sprintf("task:%s:%d", t.ID, t.Version), fmt.Sprintf("Task %s: %s", t.Status, t.Title),
			fmt.Sprintf("Task %s moved from %s to %s. %s", t.ID, from, t.Status, t.Result))
	})
}

// OnRunEnded handles a run that ended: control is told about workers; an unexpected control end is recovered.
func (sv *Supervisor) OnRunEnded(runID string) {
	sv.do(func(ctx context.Context) {
		r, err := sv.fl.GetRun(ctx, runID)
		if err != nil {
			return
		}
		if r.Kind != agenttools.KindControl {
			// A run parked at its usage limit is announced by its parked message (OnLimitWaitTx), not as ended.
			if w, err := sv.fl.LimitWaitForRun(ctx, r.ID); err != nil || w != nil {
				return
			}
			task := ""
			if l, _, err := sv.q.List(ctx, r.SpaceID); err == nil {
				for _, t := range l {
					if t.AssigneeRun == r.ID {
						task = fmt.Sprintf(" Its task %s (%s) is %s.", t.ID, t.Title, t.Status)
					}
				}
			}
			detail := r.Observed
			if r.Error != "" {
				detail += ": " + r.Error
			}
			sv.notify(ctx, r.SpaceID, "run-ended:"+r.ID, fmt.Sprintf("%s run ended (%s)", r.Role, r.Observed),
				fmt.Sprintf("Run %s (%s, %s) ended: %s.%s", r.ID, r.Kind, r.Role, detail, task))
			return
		}
		if r.Desired != "running" {
			return // an operator (or control's own stop) ended it
		}
		sv.recover(ctx, r)
	})
}

func (sv *Supervisor) recover(ctx context.Context, r *fleet.Run) {
	if err := sv.fl.MarkEndedRunStopped(ctx, r.ID); err != nil {
		sv.log.Warn("settle ended control run", obs.Err(err))
		return
	}
	// A usage limit is not a crash (ADR-0079 decision 7): restarting would run into the same limit, burn the restart
	// budget and page people. Nothing is restarted, counted or raised; control comes back at the reset (its limit wait,
	// or auto-start for waiting work once the hold lets a launch through).
	if r.EndReason == fleet.EndUsageLimit {
		sv.log.Info("control ended at a usage limit; not restarted", slog.String(obs.FieldRunID, r.ID))
		return
	}
	sp, err := sv.fl.GetSpace(ctx, r.SpaceID)
	if err != nil || sp.State != "active" {
		return // paused or archived: no new work starts
	}
	now := sv.clock.Now()
	n, err := sv.fl.CountRunsCreatedBy(ctx, r.SpaceID, agenttools.KindControl, SupervisorActor, now.Add(-sv.Window))
	if err != nil {
		sv.log.Warn("count control restarts", obs.Err(err))
		return
	}
	if n >= sv.MaxRestarts {
		detail := r.Observed
		if r.Error != "" {
			detail += ": " + r.Error
		}
		_, _, err := sv.human.Create(ctx, audit.System, r.SpaceID, human.CreateInput{Type: human.TypeTodo, Priority: "urgent",
			Title:     "Control keeps stopping in " + sp.Name,
			Body:      fmt.Sprintf("Control was restarted %d times in the last %s and ended again (%s). SpaceMan stopped restarting it. Check the last run's terminal, then launch control again.", n, sv.Window, detail),
			Requester: human.Requester{Kind: "system", Label: "Control supervisor"},
			Request:   []byte(`{"steps":["Open the last control run and read its terminal","Fix the cause (account, harness, prompt or node)","Launch control again"]}`),
			DedupKey:  fmt.Sprintf("control-failing:%s", now.Truncate(sv.Window).Format(time.RFC3339))})
		if err != nil {
			sv.log.Warn("raise control attention", obs.Err(err))
		}
		return
	}
	li := fleet.LaunchInput{Kind: agenttools.KindControl, Role: r.Role, Harness: r.Harness, InstanceID: r.InstanceID, SessionID: resumable(r),
		Model: r.Launch.Model, Settings: r.Launch.Settings, AccountRef: r.Launch.AccountRef, Workspace: r.Launch.Workspace,
		Prompt: "SpaceMan restarted you after your previous session ended unexpectedly. Check spaceman_messages, the inbox and the queue, then continue."}
	if li.SessionID == "" {
		li.Prompt = r.Launch.Prompt + "\n\n" + li.Prompt
	}
	if _, err := sv.fl.LaunchRun(ctx, SupervisorActor, r.SpaceID, li); err != nil {
		if h, ok := fleet.AsHold(err); ok {
			// Its account is held: auto-start brings control back once the hold lets a launch through.
			sv.startMu.Lock()
			sv.deferStart(r.SpaceID, h)
			sv.startMu.Unlock()
			sv.log.Info("control restart deferred: its account is held at a usage limit", slog.String(obs.FieldRunID, r.ID), slog.Time("until", h.Until))
			return
		}
		sv.log.Warn("restart control", slog.String(obs.FieldRunID, r.ID), obs.Err(err))
	}
}
