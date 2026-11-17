package fleet

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Resuming runs an agent restart stopped (ADR-0096). herdr-core ends the process of a run whose node agent restarted
// with the reason stopped_by_restart; central sees it as an ordinary exit. A run that ends that way while it is still
// wanted (desired running) is brought back through the limit-wait machinery of ADR-0079 with the cause agent_restart:
// the same instance, --resume of its session, the same worktree, the task pointed at the new run, under the usual
// slot, budget and account checks of a launch. Control hears one message when it resumes, or when SpaceMan gives up.

const (
	// EndStoppedByRestart is the end reason herdr-core gives a process the agent's restart stopped.
	EndStoppedByRestart = "stopped_by_restart"
	// WaitCauseRestart is the cause of a wait written for such a run (limit_waits.cause; '' is a usage limit).
	WaitCauseRestart = "agent_restart"
	// WaitRestartCap is the fixed reason of a wait given up because the run was stopped by a restart too often.
	WaitRestartCap = "restart_cap"

	// restartMaxResumes bounds the automatic resumes of one chain; the next restart gives the task back and tells control.
	restartMaxResumes = 3
	// restartStableAfter is how long a resumed run must have lived for its end not to count against the cap.
	restartStableAfter = 30 * time.Minute
	// restartResumeDelay is the delay before the first resume, doubling with each attempt (the node's agent is still
	// coming up, and a crash loop must not spin).
	restartResumeDelay = 15 * time.Second
)

// restartEndedTx runs in the report transaction of a worker run that ended stopped_by_restart while desired running.
// It writes a wait that resumes the run, or, when it must not be resumed, leaves the run to the ordinary run-ended
// notice (no task, or the task is in review: the work is done and waits for acceptance) or gives it up after the cap
// (the task goes back to the queue ready and unassigned, control is told to relaunch). sessionID and turn are the
// node's latest for the run.
func (svc *Service) restartEndedTx(ctx context.Context, tx *store.Tx, r db.Run, sessionID string, turn int64) error {
	if r.Kind != "worker" && r.Kind != "diagnostic" { // control is restarted by its supervisor
		return nil
	}
	if _, err := tx.Q.GetLimitWaitByRun(ctx, r.ID); err == nil {
		return nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	taskID, err := activeTaskOf(ctx, tx, r.ID)
	if err != nil || taskID == "" {
		return err // nothing to carry on: the run-ended notice tells control
	}
	if t, err := tx.Q.GetTask(ctx, taskID); err != nil {
		return err
	} else if t.Status == "review" {
		return nil // the worker finished: the task keeps waiting for acceptance, and a resume would only restart an idle agent
	}
	now := tx.Now
	w := db.LimitWait{ID: ids.New(ids.LimitWait), SpaceID: r.SpaceID, RunID: r.ID, TaskID: taskID, RunKind: r.Kind, Role: r.Role, NodeID: r.NodeID,
		InstanceID: r.InstanceID, Harness: r.Harness, LaunchJson: r.LaunchJson, Cause: WaitCauseRestart, State: WaitWaiting, Version: 1,
		CreatedAt: now.UnixMicro(), UpdatedAt: now.UnixMicro()}
	if turn > 0 {
		w.SessionID = sessionID
		if w.SessionID == "" {
			w.SessionID = r.SessionID
		}
	}
	prev, err := tx.Q.GetLimitWaitResumedInto(ctx, db.GetLimitWaitResumedIntoParams{SpaceID: r.SpaceID, NewRunID: r.ID})
	switch {
	case err == nil:
		w.ReleaseAt = prev.ReleaseAt
		// Consecutive restarts count against the cap; a resumed run that lived on for a while starts the count again.
		stable := r.StartedAt.Valid && now.Sub(time.UnixMicro(r.StartedAt.Int64)) >= restartStableAfter
		if prev.Cause == WaitCauseRestart && !stable {
			w.Attempts = prev.Attempts
		}
		// The resumed run was launched with the resume note: a later fresh session needs the original prompt.
		var spec, first LaunchSpec
		if json.Unmarshal([]byte(r.LaunchJson), &spec) == nil && json.Unmarshal([]byte(prev.LaunchJson), &first) == nil {
			spec.Prompt = first.Prompt
			if b, err := json.Marshal(spec); err == nil {
				w.LaunchJson = string(b)
			}
		}
	case !errors.Is(err, sql.ErrNoRows):
		return err
	}
	if old, err := tx.Q.GetActiveLimitWait(ctx, db.GetActiveLimitWaitParams{SpaceID: r.SpaceID, InstanceID: r.InstanceID}); err == nil {
		if err := svc.settleWaitTx(ctx, tx, old, WaitCancelled, WaitSuperseded, LimitWaitActor); err != nil {
			return err
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if !w.ReleaseAt.Valid {
		w.ReleaseAt = sql.NullInt64{Int64: now.Add(svc.ulSettings(ctx, tx.Q, r.SpaceID).MaxWait).UnixMicro(), Valid: true}
	}
	capped := w.Attempts >= restartMaxResumes
	delay := restartResumeDelay << min(w.Attempts, 6)
	w.ResumeAt = now.Add(delay).UnixMicro()
	if err := tx.Q.InsertLimitWait(ctx, db.InsertLimitWaitParams{ID: w.ID, SpaceID: w.SpaceID, RunID: w.RunID, TaskID: w.TaskID, RunKind: w.RunKind,
		Role: w.Role, NodeID: w.NodeID, InstanceID: w.InstanceID, Harness: w.Harness, SessionID: w.SessionID, LaunchJson: w.LaunchJson,
		ResumeAt: w.ResumeAt, ReleaseAt: w.ReleaseAt, Attempts: w.Attempts, Cause: w.Cause, CreatedAt: w.CreatedAt, UpdatedAt: w.UpdatedAt}); err != nil {
		return err
	}
	if err := svc.record(ctx, tx, w.SpaceID, "limit_wait", w.ID, "created", 1); err != nil {
		return err
	}
	if _, err := svc.audit.Append(ctx, tx, audit.Event{Actor: LimitWaitActor, Action: "limit_wait.parked", Target: audit.Target{Type: "limit_wait", ID: w.ID},
		Details: map[string]any{"space_id": w.SpaceID, "run_id": r.ID, "why": EndStoppedByRestart, "cause": WaitCauseRestart, "task_id": taskID,
			"attempts": w.Attempts, "capped": capped, "resume_at": time.UnixMicro(w.ResumeAt).UTC()}}); err != nil {
		return err
	}
	if capped {
		// Restarted too often: not resumed again. The task goes back to the queue and control is told to relaunch it.
		if err := svc.settleWaitTx(ctx, tx, w, WaitReleased, WaitRestartCap, LimitWaitActor); err != nil {
			return err
		}
		status := ""
		if svc.opts.ReleaseTaskTx != nil {
			if status, err = svc.opts.ReleaseTaskTx(ctx, tx, LimitWaitActor, taskID, r.ID, releaseReason(w)); err != nil {
				return err
			}
		}
		w.State, w.LastError, w.Version = WaitReleased, WaitRestartCap, w.Version+1
		if svc.opts.OnLimitWaitTx != nil {
			return svc.opts.OnLimitWaitTx(ctx, tx, LimitWaitEvent{Wait: limitWaitFromRow(w), Change: LimitWaitReleased, TaskStatus: status})
		}
		return nil
	}
	if svc.opts.OnLimitWaitTx != nil {
		if err := svc.opts.OnLimitWaitTx(ctx, tx, LimitWaitEvent{Wait: limitWaitFromRow(w), Change: LimitWaitParked}); err != nil {
			return err
		}
	}
	tx.AfterCommit(svc.wakeDeadlines)
	return nil
}
