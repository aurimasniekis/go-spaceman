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
//   - control gets a durable notification for actionable events (a new inbox item it did not create, a task moved to
//     review, blocked or failed by someone else, a worker run that ended); the notifier wakes it when idle, and a
//     control started later reads them then;
//   - a control run that ends while still wanted is relaunched, resuming its session, at most MaxRestarts times per
//     Window; after that SpaceMan stops restarting and asks people to look (an urgent human action).
type Supervisor struct {
	fl    *fleet.Service
	q     *queue.Service
	msgs  *messages.Service
	human *human.Service
	log   *slog.Logger
	clock clock.Clock

	MaxRestarts int64
	Window      time.Duration
	// base is the server's lifetime context (Bind); handlers stop with it.
	base context.Context
	// Async runs handlers in a goroutine (production; hooks are called on request paths). Tests call them directly.
	Async bool
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
		base: context.Background()}
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

// OnInboxItem notifies control of a new inbox item it did not add itself.
func (sv *Supervisor) OnInboxItem(it *inbox.Item) {
	sv.do(func(ctx context.Context) {
		if it.SourceKind == "worker" && it.SourceID != "" {
			if r, err := sv.fl.GetRun(ctx, it.SourceID); err == nil && r.Kind == agenttools.KindControl {
				return
			}
		}
		sv.notify(ctx, it.SpaceID, "inbox:"+it.ID, "New inbox item: "+it.Title,
			fmt.Sprintf("Inbox item %s (%s, %s urgency). Read it with spaceman_inbox_get.", it.ID, it.SourceKind, it.Urgency))
	})
}

// OnTaskStatus notifies control when a task needs its decision: moved to review, blocked or failed by someone else.
func (sv *Supervisor) OnTaskStatus(actor audit.Actor, t *queue.Task, from string) {
	if t.Status != "review" && t.Status != "blocked" && t.Status != "failed" {
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
	li := fleet.LaunchInput{Kind: agenttools.KindControl, Role: r.Role, Harness: r.Harness, InstanceID: r.InstanceID, SessionID: r.SessionID,
		Model: r.Launch.Model, Settings: r.Launch.Settings, AccountRef: r.Launch.AccountRef, Workspace: r.Launch.Workspace,
		Prompt: "SpaceMan restarted you after your previous session ended unexpectedly. Check spaceman_messages, the inbox and the queue, then continue."}
	if li.SessionID == "" {
		li.Prompt = r.Launch.Prompt + "\n\n" + li.Prompt
	}
	if _, err := sv.fl.LaunchRun(ctx, SupervisorActor, r.SpaceID, li); err != nil {
		sv.log.Warn("restart control", slog.String(obs.FieldRunID, r.ID), obs.Err(err))
	}
}
