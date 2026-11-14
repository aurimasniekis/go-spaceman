package queue

import (
	"context"
	"database/sql"
	"errors"

	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/outbox"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Tasks of runs parked at a usage limit (ADR-0079 decisions 4–6). A parked worker's task stays running and assigned to
// the ended run while it waits. Its resume re-points the task to the new run, and a release after the maximum wait gives
// it back to the queue. Both run inside fleet's transaction, through hooks wired by the app, so the task changes
// together with the wait.

// WaitTaskMoved is a cancelled limit wait's reason when its task is finished, back in the queue, or given to another run.
const WaitTaskMoved = "task_moved"

// RepointRunTaskTx points a task from a parked run to the run that resumed it, inside the caller's transaction. It
// changes nothing, and returns false, unless the task is still assigned to the parked run and not finished.
func (svc *Service) RepointRunTaskTx(ctx context.Context, tx *store.Tx, actor audit.Actor, taskID, fromRun, toRun string) (bool, error) {
	r, err := tx.Q.GetTask(ctx, taskID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	if !waitKeeps(r.Status, r.AssigneeRun, fromRun) {
		return false, nil
	}
	if _, err := tx.Q.UpdateTaskFields(ctx, db.UpdateTaskFieldsParams{Title: r.Title, Objective: r.Objective, Acceptance: r.Acceptance, Priority: r.Priority,
		ExclusionsJson: r.ExclusionsJson, AssigneeRole: r.AssigneeRole, AssigneeRun: toRun, Workspace: r.Workspace, Now: tx.NowMicros(), ID: r.ID,
		Version: r.Version}); err != nil {
		return false, err
	}
	if err := svc.changes.Record(ctx, tx, r.SpaceID, "task", r.ID, "updated", r.Version+1); err != nil {
		return false, err
	}
	_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "task.updated", Target: audit.Target{Type: "task", ID: r.ID},
		Details: map[string]any{"space_id": r.SpaceID, "assignee_run": toRun, "previous_assignee_run": fromRun, "reason": "usage_limit_resume"}})
	return err == nil, err
}

// ReleaseRunTaskTx gives a parked run's task back, inside the caller's transaction (ADR-0079 decision 6): a running or
// blocked task becomes ready with its assignee run cleared; a task in review stays in review (its work is done and
// waits for acceptance) with only its assignee cleared. It returns the task's status after the change, or "" when it
// changed nothing: the task is no longer the run's, or is finished. The caller tells control; OnStatus is not called.
func (svc *Service) ReleaseRunTaskTx(ctx context.Context, tx *store.Tx, actor audit.Actor, taskID, runID, reason string) (string, error) {
	r, err := tx.Q.GetTask(ctx, taskID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	} else if err != nil {
		return "", err
	}
	if !waitKeeps(r.Status, r.AssigneeRun, runID) {
		return "", nil
	}
	if r.Status == Review {
		if _, err := tx.Q.UpdateTaskFields(ctx, db.UpdateTaskFieldsParams{Title: r.Title, Objective: r.Objective, Acceptance: r.Acceptance, Priority: r.Priority,
			ExclusionsJson: r.ExclusionsJson, AssigneeRole: r.AssigneeRole, Workspace: r.Workspace, Now: tx.NowMicros(), ID: r.ID, Version: r.Version}); err != nil {
			return "", err
		}
		if err := svc.changes.Record(ctx, tx, r.SpaceID, "task", r.ID, "updated", r.Version+1); err != nil {
			return "", err
		}
		_, err := svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "task.updated", Target: audit.Target{Type: "task", ID: r.ID},
			Details: map[string]any{"space_id": r.SpaceID, "assignee_run": "", "previous_assignee_run": runID, "reason": reason}})
		if err != nil {
			return "", err
		}
		return Review, nil
	}
	if _, err := tx.Q.SetTaskStatus(ctx, db.SetTaskStatusParams{Status: Ready, StatusReason: reason, Result: r.Result, Now: tx.NowMicros(), ID: r.ID,
		Version: r.Version}); err != nil {
		return "", err
	}
	if _, err := tx.Q.UpdateTaskFields(ctx, db.UpdateTaskFieldsParams{Title: r.Title, Objective: r.Objective, Acceptance: r.Acceptance, Priority: r.Priority,
		ExclusionsJson: r.ExclusionsJson, AssigneeRole: r.AssigneeRole, Workspace: r.Workspace, Now: tx.NowMicros(), ID: r.ID, Version: r.Version + 1}); err != nil {
		return "", err
	}
	if err := svc.changes.Record(ctx, tx, r.SpaceID, "task", r.ID, "updated", r.Version+2); err != nil {
		return "", err
	}
	t := fromRow(r)
	if _, err := svc.Outbox.Enqueue(ctx, tx, outbox.Event{Depth: t.AutomationDepth, Payload: outbox.TaskStatusChanged{Task: eventTask(t), From: r.Status, To: Ready},
		Correlation: r.ID}); err != nil {
		return "", err
	}
	_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "task.status", Target: audit.Target{Type: "task", ID: r.ID},
		Details: map[string]any{"space_id": r.SpaceID, "from": r.Status, "to": Ready, "reason": reason, "previous_assignee_run": runID}})
	if err != nil {
		return "", err
	}
	return Ready, nil
}

// waitKeeps reports whether a task still belongs to a parked run: assigned to it and still being worked on (running,
// blocked or in review). Anything else ends the run's claim on it.
func waitKeeps(status, assignee, runID string) bool {
	return assignee == runID && (status == Running || status == Blocked || status == Review)
}

// settleLimitWaitsTx cancels the waits of runs parked on a task that no longer belongs to them: the task finished, went
// back to the queue or was given to another run (the review follow-up of UL-2). It runs in the transaction that changed
// the task, and writes only when such a wait exists.
func (svc *Service) settleLimitWaitsTx(ctx context.Context, tx *store.Tx, actor audit.Actor, taskID, status, assignee string) error {
	ws, err := tx.Q.ListTaskLimitWaits(ctx, taskID)
	if err != nil {
		return err
	}
	for _, w := range ws {
		if waitKeeps(status, assignee, w.RunID) {
			continue
		}
		n, err := tx.Q.UpdateLimitWait(ctx, db.UpdateLimitWaitParams{State: "cancelled", ResumeAt: w.ResumeAt, NewRunID: w.NewRunID, Attempts: w.Attempts,
			LastError: WaitTaskMoved, Now: tx.NowMicros(), ID: w.ID, Version: w.Version})
		if err != nil {
			return err
		}
		if n == 0 {
			continue
		}
		if err := svc.changes.Record(ctx, tx, w.SpaceID, "limit_wait", w.ID, "updated", w.Version+1); err != nil {
			return err
		}
		if _, err := svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "limit_wait.cancelled", Target: audit.Target{Type: "limit_wait", ID: w.ID},
			Details: map[string]any{"space_id": w.SpaceID, "run_id": w.RunID, "task_id": taskID, "reason": WaitTaskMoved}}); err != nil {
			return err
		}
	}
	return nil
}
