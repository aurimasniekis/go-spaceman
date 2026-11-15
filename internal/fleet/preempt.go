package fleet

import (
	"context"
	"database/sql"
	"errors"

	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Worker handling for incident preemption (SM-QUEUE-009, ADR-0050). The queue calls these inside its own write
// transaction; each returns whether it acted, so the queue can record what really happened. Nothing here touches the
// network: commands are queued and the caller kicks the node after the commit (KickRun).

// activeWorker returns the run when it is a non-control run of the space that is wanted and has not ended.
func activeWorker(ctx context.Context, tx *store.Tx, spaceID, runID string) (db.Run, bool, error) {
	r, err := tx.Q.GetRun(ctx, runID)
	if errors.Is(err, sql.ErrNoRows) {
		return r, false, nil
	} else if err != nil {
		return r, false, err
	}
	if r.SpaceID != spaceID || r.Kind == "control" || r.Desired != "running" || ended(r.Observed) {
		return r, false, nil
	}
	return r, true, nil
}

// WorkerActive reports whether runID is a live worker (any non-control run) of the space.
func (svc *Service) WorkerActive(ctx context.Context, tx *store.Tx, spaceID, runID string) (bool, error) {
	_, ok, err := activeWorker(ctx, tx, spaceID, runID)
	return ok, err
}

// PauseWorkerTx interrupts the worker's current turn: the program keeps running, idle, with its session, until it
// is resumed with a notice. A terminal an operator has taken over is left alone (false).
func (svc *Service) PauseWorkerTx(ctx context.Context, tx *store.Tx, actor audit.Actor, spaceID, runID, key, reason string) (bool, error) {
	r, ok, err := activeWorker(ctx, tx, spaceID, runID)
	if err != nil || !ok {
		return false, err
	}
	if _, on, err := activeTakeover(ctx, tx.Q, r.ID, tx.Now); err != nil || on {
		return false, err
	}
	if err := svc.queueCommand(ctx, tx, r.NodeID, r.SpaceID, r.ID, r.Generation, proto.CmdInterrupt, "interrupt:"+r.ID+":"+key,
		proto.StopPayload{Reason: reason}); err != nil {
		return false, err
	}
	_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "run.paused", Target: audit.Target{Type: "run", ID: r.ID},
		Details: map[string]any{"space_id": spaceID, "reason": reason}})
	return err == nil, err
}

// StopWorkerTx stops the worker politely (interrupt, then kill after the grace); its session stays resumable.
func (svc *Service) StopWorkerTx(ctx context.Context, tx *store.Tx, actor audit.Actor, spaceID, runID, reason string) (bool, error) {
	r, ok, err := activeWorker(ctx, tx, spaceID, runID)
	if err != nil || !ok {
		return false, err
	}
	if err := svc.stopRunTx(ctx, tx, r, "interrupt", reason); err != nil {
		return false, err
	}
	_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "run.stop_requested", Target: audit.Target{Type: "run", ID: r.ID},
		Details: map[string]any{"mode": "interrupt", "reason": reason}})
	return err == nil, err
}

// NoticeWorkerTx queues an idle-gated notice for a live worker (false when it is not live, is taken over or is waiting
// for its usage limit to reset).
func (svc *Service) NoticeWorkerTx(ctx context.Context, tx *store.Tx, spaceID, runID, text string) (bool, error) {
	r, ok, err := activeWorker(ctx, tx, spaceID, runID)
	if err != nil || !ok || r.LimitKind != "" {
		return false, err
	}
	if _, on, err := activeTakeover(ctx, tx.Q, r.ID, tx.Now); err != nil || on {
		return false, err
	}
	if _, err := svc.QueueNotice(ctx, tx, r.ID, text); err != nil {
		return false, err
	}
	return true, nil
}
