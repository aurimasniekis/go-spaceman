package control

import (
	"context"
	"fmt"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/agenttools"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/store"
)

// Dedup keys of the usage-limit notices: one "limited" and one "cleared" per run and limit window (its since).
func limitedKey(runID string, l *fleet.RunLimit) string {
	return fmt.Sprintf("usage-limit:%s:%d", runID, l.Since.UnixMicro())
}

func limitClearedKey(runID string, l *fleet.RunLimit) string {
	return fmt.Sprintf("usage-limit-cleared:%s:%d", runID, l.Since.UnixMicro())
}

// OnRunLimitTx tells control, inside the report transaction, when a run's usage limit opens and when it ends (fleet
// OnRunLimitTx, ADR-0079 decision 11): exactly one message per run and window each way, deduplicated on the run and the
// limit's since, and written only on that change, never per report. A "cleared" message goes out only for a window
// control was told about. Control's own limit is not announced to it: it couldn't act on it, and its notices wait for
// the clear anyway.
func (sv *Supervisor) OnRunLimitTx(ctx context.Context, tx *store.Tx, r *fleet.Run, ended, opened *fleet.RunLimit) error {
	if r.Kind == agenttools.KindControl {
		return nil
	}
	if ended != nil {
		told, err := sv.msgs.ControlNotifiedTx(ctx, tx, r.SpaceID, limitedKey(r.ID, ended))
		if err != nil {
			return err
		}
		if told {
			body := fmt.Sprintf("Run %s (%s, %s) is past its %s usage limit and can take notices again.%s", r.ID, r.Kind, r.Role, ended.Kind, limitTask(ctx, tx, r.ID))
			if _, err := sv.msgs.NotifyControlTx(ctx, tx, r.SpaceID, limitClearedKey(r.ID, ended), fmt.Sprintf("%s run usage limit cleared", r.Role), body); err != nil {
				return err
			}
		}
	}
	// Only a run kept alive at its limit is announced; one that ended with it is reported by its run-ended notice.
	if opened == nil || !fleet.Live(r.Observed) {
		return nil
	}
	reset := "an unknown time (Claude Code didn't say)"
	if opened.ResetsAt != nil {
		reset = opened.ResetsAt.UTC().Format(time.RFC3339)
	}
	body := fmt.Sprintf("Run %s (%s, %s) reached its %s usage limit; it resets at %s.%s SpaceMan keeps the run alive and its harness continues "+
		"by itself after the reset. Until then it doesn't count against the space's run limit and gets no notices (messages to it wait). "+
		"Don't stop, replace or relaunch it: you get one more message when the limit clears.", r.ID, r.Kind, r.Role, opened.Kind, reset, limitTask(ctx, tx, r.ID))
	_, err := sv.msgs.NotifyControlTx(ctx, tx, r.SpaceID, limitedKey(r.ID, opened), fmt.Sprintf("%s run at its usage limit", r.Role), body)
	return err
}

// limitTask names the task assigned to the run, if any (" Its task …").
func limitTask(ctx context.Context, tx *store.Tx, runID string) string {
	id, err := tx.Q.GetTaskIDForRun(ctx, runID)
	if err != nil {
		return ""
	}
	t, err := tx.Q.GetTask(ctx, id)
	if err != nil {
		return ""
	}
	return fmt.Sprintf(" Its task %s (%s) is %s.", t.ID, t.Title, t.Status)
}

// OnLimitWaitTx tells control, inside the transaction of the change, when a run is parked at its usage limit, when it
// is resumed and when its wait is released (fleet OnLimitWaitTx, ADR-0079 decisions 4–6 and 11): one message per wait
// and change, deduplicated on the wait. A re-park of a resumed run (an unknown reset probed again) and the later
// resumes of the same chain are not announced again. Control's own wait isn't announced to it: a resumed control is
// told by its prompt, a fresh session's prompt included.
func (sv *Supervisor) OnLimitWaitTx(ctx context.Context, tx *store.Tx, ev fleet.LimitWaitEvent) error {
	w := ev.Wait
	if w.RunKind == agenttools.KindControl {
		return nil
	}
	task := ""
	if w.TaskID != "" {
		if t, err := tx.Q.GetTask(ctx, w.TaskID); err == nil {
			task = fmt.Sprintf(" Its task is %s (%s).", t.ID, t.Title)
		}
	}
	var key, subject, body string
	switch ev.Change {
	case fleet.LimitWaitParked:
		if ev.Repark {
			return nil
		}
		release := ""
		if w.ReleaseAt != nil {
			release = fmt.Sprintf(" If it hasn't resumed by %s, SpaceMan gives up on it and its task goes back to the queue, ready and unassigned.",
				w.ReleaseAt.UTC().Format(time.RFC3339))
		}
		key, subject = "limit-wait:"+w.ID, fmt.Sprintf("%s run parked at its usage limit", w.Role)
		body = fmt.Sprintf("Run %s (%s, %s) is parked at its usage limit: its process ended and its session is kept. SpaceMan resumes it as a new run "+
			"of the same instance at %s (or when the account's hold lets it through).%s%s Don't relaunch or reassign it; you get one message when it "+
			"resumes. Its wait is %s (spaceman limits waits).", w.RunID, w.RunKind, w.Role, w.ResumeAt.UTC().Format(time.RFC3339), task, release, w.ID)
	case fleet.LimitWaitResumed:
		if !ev.FirstResume && !ev.Fresh {
			return nil
		}
		key, subject = "limit-wait-resumed:"+w.ID, fmt.Sprintf("%s run resumed after its usage limit", w.Role)
		body = fmt.Sprintf("Run %s (%s, %s), parked at its usage limit, was resumed as run %s on the same instance with its session.%s",
			w.RunID, w.RunKind, w.Role, ev.NewRunID, taskNow(task, ev.NewRunID))
		if ev.Fresh {
			body = fmt.Sprintf("Run %s (%s, %s), parked at its usage limit, was resumed as run %s in a fresh session: the space moved to another node "+
				"since, so its conversation could not be carried over. It was given its original prompt again and works from the workspace's state.%s",
				w.RunID, w.RunKind, w.Role, ev.NewRunID, taskNow(task, ev.NewRunID))
		}
	case fleet.LimitWaitReleased:
		key, subject = "limit-wait-released:"+w.ID, fmt.Sprintf("%s run given up at its usage limit", w.Role)
		body = fmt.Sprintf("Run %s (%s, %s) waited at its usage limit since %s and was not resumed; SpaceMan gave up on it (%s).%s",
			w.RunID, w.RunKind, w.Role, w.CreatedAt.UTC().Format(time.RFC3339), w.LastError, task)
		if w.TaskID != "" {
			body += " The task is back in the queue, ready and unassigned: launch a worker for it when an account is free."
		}
	default:
		return nil
	}
	_, err := sv.msgs.NotifyControlTx(ctx, tx, w.SpaceID, key, subject, body)
	return err
}

func taskNow(task, runID string) string {
	if task == "" {
		return ""
	}
	return task + " It now points at run " + runID + "."
}
