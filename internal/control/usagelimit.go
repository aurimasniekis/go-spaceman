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
