package fleet

import (
	"context"
	"database/sql"
	"errors"
	"maps"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// DeleteRecording deletes central's copy of a run's recording (an operator with recording:delete, or retention), then
// records the deletion: the run's recording_deleted_at, an audit event and a change. The files go first, outside any
// transaction; a crash in between leaves a run whose recording is gone but not yet marked, which the next deletion
// (or retention pass) marks. A recording of an active run is refused: its node is still shipping it. The node's own
// copy is separate and not affected.
func (svc *Service) DeleteRecording(ctx context.Context, actor audit.Actor, spaceID, runID string, details map[string]any) error {
	r, err := svc.s.R().GetRun(ctx, runID)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && r.SpaceID != spaceID) {
		return apperr.New(apperr.CodeNotFound, "run not found")
	} else if err != nil {
		return err
	}
	if !r.EndedAt.Valid {
		return apperr.New(apperr.CodeConflict, "the run is still active; its recording can be deleted once it ended")
	}
	if err := svc.rec.Delete(ctx, runID); err != nil {
		return err
	}
	return svc.s.W().TxNamed(ctx, "fleet.recording_delete", func(tx *store.Tx) error {
		n, err := tx.Q.MarkRecordingDeleted(ctx, db.MarkRecordingDeletedParams{RecordingDeletedAt: sql.NullInt64{Int64: tx.NowMicros(), Valid: true}, ID: runID})
		if err != nil || n == 0 {
			return err // already marked: deleted before (the files were removed again, harmlessly)
		}
		if err := svc.record(ctx, tx, spaceID, "recording", runID, changefeed.Deleted, 0); err != nil {
			return err
		}
		d := map[string]any{"space_id": spaceID}
		maps.Copy(d, details)
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "recording.deleted", Target: audit.Target{Type: "run", ID: runID}, Details: d})
		return err
	})
}
