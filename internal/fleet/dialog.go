package fleet

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/outbox"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// RunDialog is one interactive dialog the node recognised on a run's terminal (ADR-0102).
type RunDialog struct {
	ID             string     `json:"id"`
	RunID          string     `json:"run_id"`
	SpaceID        string     `json:"space_id"`
	Dialog         string     `json:"dialog"`
	Signal         string     `json:"signal"`
	Outcome        string     `json:"outcome"` // answered, blocked
	Answer         string     `json:"answer,omitempty"`
	Excerpt        string     `json:"excerpt,omitempty"`
	HarnessVersion string     `json:"harness_version,omitempty"`
	DetectedAt     time.Time  `json:"detected_at"`
	ClearedAt      *time.Time `json:"cleared_at,omitempty"`
}

func dialogFromRow(r db.RunDialog) RunDialog {
	d := RunDialog{ID: r.ID, RunID: r.RunID, SpaceID: r.SpaceID, Dialog: r.Dialog, Signal: r.Signal, Outcome: r.Outcome, Answer: r.Answer,
		Excerpt: r.Excerpt, HarnessVersion: r.HarnessVersion, DetectedAt: time.UnixMicro(r.DetectedAt).UTC()}
	if r.ClearedAt.Valid {
		t := time.UnixMicro(r.ClearedAt.Int64).UTC()
		d.ClearedAt = &t
	}
	return d
}

// RunDialogs lists a run's recognised dialogs, newest first.
func (svc *Service) RunDialogs(ctx context.Context, runID string, limit int) ([]RunDialog, error) {
	rows, err := svc.s.R().ListRunDialogs(ctx, db.ListRunDialogsParams{RunID: runID, Limit: int64(limit)})
	if err != nil {
		return nil, err
	}
	out := make([]RunDialog, 0, len(rows))
	for _, r := range rows {
		out = append(out, dialogFromRow(r))
	}
	return out, nil
}

// StatusLabel is the run status the API, the UI and the agent tools show: the program's own status, or
// blocked_on_dialog while the node reports the run waiting at a dialog SpaceMan does not answer (ADR-0102).
func StatusLabel(status, kind string) string {
	if status == "blocked" && kind == proto.StatusKindDialog {
		return proto.StatusBlockedOnDialog
	}
	return status
}

// runDialog records a node's run.dialog (ADR-0102). Each occurrence is recorded once, whatever the node repeats:
// answered goes on the run's event log (audit) only; blocked is also published (run.dialog_detected) and, after the
// commit, control and the operators are told with the excerpt (Options.OnRunDialog); cleared closes a blocked one.
// A message about a run that is not this node's, or of another generation, is ignored.
func (svc *Service) runDialog(ctx context.Context, nodeID string, d proto.RunDialog) error {
	if !d.Valid() {
		return apperr.New(apperr.CodeInvalid, "malformed run.dialog")
	}
	return svc.s.W().TxNamed(ctx, "fleet.run_dialog", func(tx *store.Tx) error {
		r, err := tx.Q.GetRun(ctx, d.RunID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		} else if err != nil {
			return err
		}
		if r.NodeID != nodeID || r.SpaceID != d.SpaceID || r.Generation != d.Generation {
			return nil
		}
		actor := audit.Actor{Type: audit.ActorNode, ID: nodeID}
		details := map[string]any{"space_id": r.SpaceID, "dialog_id": d.ID, "dialog": d.Dialog, "signal": d.Signal}
		ev := outbox.RunDialog{Run: outboxRun(r), DialogID: d.ID, Dialog: d.Dialog, Signal: d.Signal, HarnessVersion: d.HarnessVersion,
			DetectedAt: d.DetectedAt.UTC()}
		if d.Outcome == proto.DialogCleared {
			n, err := tx.Q.ClearRunDialog(ctx, db.ClearRunDialogParams{ClearedAt: sql.NullInt64{Int64: tx.NowMicros(), Valid: true}, ID: d.ID, RunID: r.ID})
			if err != nil || n == 0 {
				return err
			}
			if _, err := svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "run.dialog_cleared", Target: audit.Target{Type: "run", ID: r.ID},
				Details: details}); err != nil {
				return err
			}
			if svc.opts.Outbox != nil {
				return svc.runEvent(ctx, tx, r, outbox.RunDialogCleared{RunDialog: ev, ClearedAt: tx.Now.UTC()})
			}
			return nil
		}
		row := db.InsertRunDialogParams{ID: d.ID, RunID: r.ID, SpaceID: r.SpaceID, NodeID: nodeID, Dialog: d.Dialog, Signal: d.Signal,
			Outcome: d.Outcome, Answer: d.Answer, Excerpt: d.Excerpt, HarnessVersion: d.HarnessVersion, DetectedAt: d.DetectedAt.UnixMicro(),
			CreatedAt: tx.NowMicros()}
		n, err := tx.Q.InsertRunDialog(ctx, row)
		if err != nil || n == 0 {
			return err // a repeat of a recorded occurrence: nothing more
		}
		action := "run.dialog_answered"
		if d.Outcome == proto.DialogBlocked {
			action = "run.dialog_detected"
		} else {
			details["answer"] = d.Answer
		}
		if _, err := svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: action, Target: audit.Target{Type: "run", ID: r.ID},
			Details: details}); err != nil {
			return err
		}
		if d.Outcome != proto.DialogBlocked {
			return nil // SpaceMan's own answers are logged on the run, not published
		}
		if svc.opts.Outbox != nil {
			if err := svc.runEvent(ctx, tx, r, outbox.RunDialogDetected{RunDialog: ev}); err != nil {
				return err
			}
		}
		if notify := svc.opts.OnRunDialog; notify != nil {
			run := r
			got := dialogFromRow(db.RunDialog{ID: row.ID, RunID: row.RunID, SpaceID: row.SpaceID, NodeID: row.NodeID, Dialog: row.Dialog,
				Signal: row.Signal, Outcome: row.Outcome, Answer: row.Answer, Excerpt: row.Excerpt, HarnessVersion: row.HarnessVersion,
				DetectedAt: row.DetectedAt, CreatedAt: row.CreatedAt})
			tx.AfterCommit(func() { notify(context.WithoutCancel(ctx), run, got) })
		}
		return nil
	})
}
