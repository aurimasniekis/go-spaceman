package fleet

import (
	"context"
	"database/sql"
	"errors"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/obs"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Run terminal sizes (ADR-0075). The size belongs to the run, not to a viewer: an operator with terminal:takeover sets
// it (while the terminal is taken over, only the holder), and every viewer's browser scales the run's real size to
// its window. herdr-core resizes a program only while an interactive attach is open (docs/contracts/herdr-core.md
// S-7), so the node holds a sized run's terminal through an attach of its own that never types; central sends it the
// complete set of its runs' sizes (terminal.sizes, node protocol minor 10) after it connects and on every change.
// Clearing the size closes that attach and the terminal goes back to the node's default (runner.terminal_size).

// TerminalSize is the size a run's terminal is held at.
type TerminalSize struct {
	Cols int `json:"cols" minimum:"40" maximum:"400"`
	Rows int `json:"rows" minimum:"10" maximum:"200"`
}

// terminalSizeOf returns a run row's held size, or nil (the node's default).
func terminalSizeOf(cols, rows sql.NullInt64) *TerminalSize {
	if !cols.Valid || !rows.Valid {
		return nil
	}
	return &TerminalSize{Cols: int(cols.Int64), Rows: int(rows.Int64)}
}

// TerminalSizing reports whether a node can hold run terminal sizes: connected at node protocol minor 10 or later.
func (svc *Service) TerminalSizing(nodeID string) bool {
	minor, ok := svc.hub.minor(nodeID)
	return ok && minor >= proto.MinorTerminalSize
}

// SetTerminalSize sets (size non-nil) or clears (nil: the node's default) a running run's terminal size. While the
// terminal is taken over only the holder may change it. The run's node must be connected at minor 10 or later.
func (svc *Service) SetTerminalSize(ctx context.Context, actor audit.Actor, runID string, size *TerminalSize) (*Run, error) {
	if size != nil {
		if err := proto.CheckTerminalSize(size.Cols, size.Rows); err != nil {
			return nil, apperr.New(apperr.CodeInvalid, "%s", err.Error())
		}
	}
	r, err := svc.s.R().GetRun(ctx, runID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errRunNotFound
	} else if err != nil {
		return nil, err
	}
	if err := sizeable(r.Desired, r.Observed); err != nil {
		return nil, err
	}
	switch minor, ok := svc.hub.minor(r.NodeID); {
	case !ok:
		return nil, apperr.New(apperr.CodeUnavailable, "the run's node is not connected")
	case minor < proto.MinorTerminalSize:
		return nil, apperr.New(apperr.CodeConflict, "the run's node agent is too old to resize terminals (node protocol 1.%d needed); upgrade spaceman-agent", proto.MinorTerminalSize)
	}
	err = svc.s.W().TxNamed(ctx, "fleet.terminal_size", func(tx *store.Tx) error {
		r, err := tx.Q.GetRun(ctx, runID)
		if err != nil {
			return err
		}
		if err := sizeable(r.Desired, r.Observed); err != nil {
			return err
		}
		t, on, err := activeTakeover(ctx, tx.Q, runID, tx.Now)
		if err != nil {
			return err
		}
		if on && t.Holder != holderOf(actor) {
			who := t.HolderLabel
			if who == "" {
				who = t.Holder
			}
			return apperr.New(apperr.CodeForbidden, "the terminal is taken over by %s: only they can resize it", who)
		}
		prev := terminalSizeOf(r.TerminalCols, r.TerminalRows)
		var cols, rows sql.NullInt64
		if size != nil {
			cols, rows = sql.NullInt64{Int64: int64(size.Cols), Valid: true}, sql.NullInt64{Int64: int64(size.Rows), Valid: true}
		}
		if (prev == nil && size == nil) || (prev != nil && size != nil && *prev == *size) {
			return nil // unchanged
		}
		if _, err := tx.Q.SetRunTerminalSize(ctx, db.SetRunTerminalSizeParams{TermCols: cols, TermRows: rows, Now: tx.NowMicros(), ID: r.ID}); err != nil {
			return err
		}
		details := map[string]any{"space_id": r.SpaceID}
		if size != nil {
			details["cols"], details["rows"] = size.Cols, size.Rows
		} else {
			details["cleared"] = true
		}
		if prev != nil {
			details["previous"] = map[string]any{"cols": prev.Cols, "rows": prev.Rows}
		}
		if _, err := svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "run.terminal_resized", Target: audit.Target{Type: "run", ID: r.ID},
			Details: details}); err != nil {
			return err
		}
		tx.AfterCommit(func() { svc.hub.pushTerminalSizes(context.WithoutCancel(ctx), r.NodeID) })
		return svc.record(ctx, tx, r.SpaceID, "run", r.ID, "updated", r.Version+1)
	})
	if err != nil {
		return nil, err
	}
	return svc.GetRun(ctx, runID)
}

func sizeable(desired, observed string) error {
	if desired != "running" || ended(observed) || observed == proto.RunStopping {
		return apperr.New(apperr.CodePrecondition, "only a running run's terminal can be resized")
	}
	return nil
}

// ---- hub side ----

// pushTerminalSizes sends the node the complete set of its runs' terminal sizes (after connect and on every change).
func (h *Hub) pushTerminalSizes(ctx context.Context, nodeID string) {
	h.mu.Lock()
	s := h.sessions[nodeID]
	h.mu.Unlock()
	if s == nil || s.minor < proto.MinorTerminalSize {
		return
	}
	if err := h.sendTerminalSizes(ctx, s); err != nil {
		s.log.Warn("send terminal sizes", obs.Err(err))
	}
}

func (h *Hub) sendTerminalSizes(ctx context.Context, s *session) error {
	s.sizes.Lock()
	defer s.sizes.Unlock()
	rows, err := h.svc.s.R().ListNodeTerminalSizes(ctx, s.nodeID)
	if err != nil {
		return err
	}
	st := proto.TerminalSizes{Sizes: []proto.TerminalSize{}}
	for _, r := range rows {
		st.Sizes = append(st.Sizes, proto.TerminalSize{SpaceID: r.SpaceID, RunID: r.ID, Generation: r.Generation, Cols: int(r.TermCols), Rows: int(r.TermRows)})
	}
	return s.send(proto.TypeTerminalSizes, h.svc.now(), st)
}
