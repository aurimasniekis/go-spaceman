package control

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/agenttools"
	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/human"
	"github.com/aurimasniekis/go-spaceman/internal/limits"
	"github.com/aurimasniekis/go-spaceman/internal/obs"
)

// Control lifecycle (SM-CTRL-002, ADR-0056): SpaceMan starts a space's control when work waits for it and stops it after
// it has been idle for a while; the next work starts it again, resuming its session.

// AutostartActor launches control for waiting work; its launches are counted per hour.
var AutostartActor = audit.Actor{Type: audit.ActorSystem, ID: "control-autostart"}

// IdleActor stops control after its idle time.
var IdleActor = audit.Actor{Type: audit.ActorSystem, ID: "control-idle"}

const autostartPrompt = "SpaceMan started you because new work is waiting for control. Check spaceman_messages, the inbox and the queue, then continue."

// lifecycle is the supervisor's auto-start and idle-stop state.
type lifecycle struct {
	// Limit returns a space's effective limit (limits.Service.Value). Nil: auto-start and idle stop are off.
	Limit func(ctx context.Context, spaceID, key string) int64
	// ControlRole names the space role of kind control to launch when the space never had a control run ("": none).
	ControlRole func(ctx context.Context, spaceID string) (string, error)
	// MaxAutostarts bounds the control launches for waiting work per space per Window (then people are asked to look).
	MaxAutostarts int64
	// IdleUnit is one minute of the idle limit (tests shorten it).
	IdleUnit time.Duration

	startMu sync.Mutex
	idleMu  sync.Mutex
	idle    map[string]*time.Timer
}

// ControlWanted starts the space's control for work waiting for it (the notifier's sweep calls it for active spaces
// with unseen control items and no live control). Nothing starts when the space turned auto-start off, is not
// active, has no connected node or no control role; a refused launch (account, budget, relocation) asks people to look,
// once an hour.
func (sv *Supervisor) ControlWanted(ctx context.Context, spaceID string) {
	if sv.Limit == nil || sv.Limit(ctx, spaceID, limits.ControlAutostart) == 0 {
		return
	}
	sv.startMu.Lock()
	defer sv.startMu.Unlock()
	sp, err := sv.fl.GetSpace(ctx, spaceID)
	if err != nil || sp.State != "active" || sp.Lease == nil || !sp.Lease.Valid {
		return // paused, archived or its node is away: the work waits
	}
	now := sv.clock.Now()
	n, err := sv.fl.CountRunsCreatedBy(ctx, spaceID, agenttools.KindControl, AutostartActor, now.Add(-sv.Window))
	if err != nil {
		sv.log.Warn("count control starts", obs.Err(err))
		return
	}
	if n >= sv.MaxAutostarts {
		sv.attention(ctx, spaceID, sp.Name, "control-autostart-limit", fmt.Sprintf("SpaceMan started control %d times in the last %s for waiting work and stopped starting it. Check the last control run's terminal, then launch control again.", n, sv.Window))
		return
	}
	li := fleet.LaunchInput{Kind: agenttools.KindControl, Prompt: autostartPrompt}
	last, err := sv.fl.LatestControlRun(ctx, spaceID)
	switch {
	case err == nil:
		li.Role, li.Harness, li.InstanceID, li.Model, li.Settings = last.Role, last.Harness, last.InstanceID, last.Launch.Model, last.Launch.Settings
		li.AccountRef, li.Workspace = last.Launch.AccountRef, last.Launch.Workspace
		li.SessionID = resumable(last)
	case sv.ControlRole != nil:
		if li.Role, err = sv.ControlRole(ctx, spaceID); err != nil {
			sv.log.Warn("find control role", obs.Err(err))
			return
		}
		li.Kind = "" // the role decides
	}
	if li.Role == "" {
		return // the space has no control role: nothing to start
	}
	r, err := sv.fl.LaunchRun(ctx, AutostartActor, spaceID, li)
	if err != nil {
		var ae *apperr.Error
		if errors.As(err, &ae) && ae.Code == apperr.CodeConflict {
			return // another control became authoritative meanwhile
		}
		sv.log.Warn("start control", slog.String("space.id", spaceID), obs.Err(err))
		sv.attention(ctx, spaceID, sp.Name, "control-autostart-refused", "Work is waiting for control, but SpaceMan could not start it: "+err.Error()+". Fix the cause (control role, account, budget), then launch control or wait for the next event.")
		return
	}
	sv.log.Info("control started for waiting work", slog.String("space.id", spaceID), slog.String(obs.FieldRunID, r.ID))
}

// resumable returns the session a new control run resumes: the last run's, unless it never finished a turn (a session
// with no turn was never saved, and resuming it fails).
func resumable(r *fleet.Run) string {
	if r.Turn == 0 {
		return ""
	}
	return r.SessionID
}

func (sv *Supervisor) attention(ctx context.Context, spaceID, spaceName, kind, body string) {
	_, _, err := sv.human.Create(ctx, audit.System, spaceID, human.CreateInput{Type: human.TypeTodo, Priority: "high",
		Title:     "Control could not start in " + spaceName,
		Body:      body,
		Requester: human.Requester{Kind: "system", Label: "Control supervisor"},
		Request:   []byte(`{"steps":["Open the space's runs and read the last control run's terminal","Fix the cause (control role, account, budget or node)","Launch control again"]}`),
		DedupKey:  fmt.Sprintf("%s:%s", kind, sv.clock.Now().Truncate(sv.Window).Format(time.RFC3339))})
	if err != nil {
		sv.log.Warn("raise control attention", obs.Err(err))
	}
}

// OnRunIdle arms the idle stop of a control run whose program finished its turn (fleet OnRunIdle).
func (sv *Supervisor) OnRunIdle(runID string) {
	sv.do(func(ctx context.Context) {
		if r, err := sv.fl.GetRun(ctx, runID); err == nil && r.Kind == agenttools.KindControl {
			sv.armIdle(ctx, r)
		}
	})
}

// ArmIdleTimers arms the idle stop of every control run idle at start (timers do not survive a central restart; each
// gets its full idle time again). Serve runs it once.
func (sv *Supervisor) ArmIdleTimers(ctx context.Context) {
	ids, err := sv.fl.IdleControlRuns(ctx)
	if err != nil {
		sv.log.Warn("list idle control runs", obs.Err(err))
		return
	}
	for _, id := range ids {
		if r, err := sv.fl.GetRun(ctx, id); err == nil {
			sv.armIdle(ctx, r)
		}
	}
}

// armIdle (re)arms a control run's idle stop. A control run waiting for its usage limit to reset is waiting, not idle
// (ADR-0079 decision 3): its timer is dropped, and the idle report that follows the clear arms a new one.
func (sv *Supervisor) armIdle(ctx context.Context, r *fleet.Run) {
	if sv.Limit == nil {
		return
	}
	mins := sv.Limit(ctx, r.SpaceID, limits.ControlIdleStop)
	sv.idleMu.Lock()
	defer sv.idleMu.Unlock()
	if t := sv.idle[r.ID]; t != nil {
		t.Stop()
		delete(sv.idle, r.ID)
	}
	if mins <= 0 || r.Limit != nil {
		return
	}
	id, turn := r.ID, r.Turn
	sv.idle[id] = time.AfterFunc(time.Duration(mins)*sv.IdleUnit, func() {
		sv.do(func(ctx context.Context) { sv.idleCheck(ctx, id, turn) })
	})
}

// idleCheck stops a control run that is still idle at the same turn it was armed at. Control stays while an operator
// has its terminal or something waits for it that it has not been shown (the notifier is about to wake it); the timer
// starts over then.
func (sv *Supervisor) idleCheck(ctx context.Context, runID string, turn int64) {
	sv.idleMu.Lock()
	delete(sv.idle, runID)
	sv.idleMu.Unlock()
	r, err := sv.fl.GetRun(ctx, runID)
	if err != nil || r.Desired != "running" || r.Observed != "running" || r.Turn != turn || (r.Status != "idle" && r.Status != "done") {
		return // ended, stopping, or working again: its next idle point arms a new timer
	}
	if r.Limit != nil {
		return // at a usage limit since the timer was armed: the clear arms a new one (ADR-0079 decision 3)
	}
	waiting, err := sv.msgs.Waiting(ctx, r.SpaceID, r.ID, r.Role, true)
	if err != nil {
		sv.log.Warn("check waiting control items", obs.Err(err))
		return
	}
	if r.Takeover != nil || waiting {
		sv.armIdle(ctx, r)
		return
	}
	mins := sv.Limit(ctx, r.SpaceID, limits.ControlIdleStop)
	if mins <= 0 {
		return
	}
	if _, err := sv.fl.StopRun(ctx, IdleActor, r.ID, "interrupt", fmt.Sprintf("idle for %d minutes; SpaceMan starts control again when work arrives", mins)); err != nil {
		sv.log.Warn("stop idle control", slog.String(obs.FieldRunID, r.ID), obs.Err(err))
		return
	}
	sv.log.Info("idle control stopped", slog.String(obs.FieldRunID, r.ID), slog.Int64("idle_minutes", mins))
}
