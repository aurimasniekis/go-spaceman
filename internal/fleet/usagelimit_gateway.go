package fleet

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Gateway hold estimates (ADR-0079 decision 9, UL-8). A Harbor Pilot run's limit report carries no reset, so its hold
// opens with an unknown reset and a probe (decision 7). Central's gateway re-check (harborpilot.GatewayWaits) then reads
// Harbor Pilot outside any transaction and applies what it learnt here: the hold's estimated reset (the earliest
// `until` among the quota board's exhausted accounts, or the covering blocking budget's `period_end`) and its next
// re-check in check_at, or its clear when the gateway has recovered.
//
// An estimated hold (source quota_board or budget_status with a reset) refuses launches until its reset, and its
// check_at is only the next re-read: it never lets a probe launch through.
const (
	// GatewayRecheck is how often an estimated gateway hold is read again from Harbor Pilot.
	GatewayRecheck = 15 * time.Minute
	// SourceQuotaBoard and SourceBudgetStatus are a gateway hold's source once Harbor Pilot estimated its reset; they
	// are also cleared_by on a hold Harbor Pilot showed recovered.
	SourceQuotaBoard   = "quota_board"
	SourceBudgetStatus = "budget_status"
)

// estimated reports whether a hold's reset is Harbor Pilot's estimate: check_at is then the next re-read, not a probe.
func estimated(u db.UsageLimit) bool {
	return u.ResetsAt.Valid && (u.Source == SourceQuotaBoard || u.Source == SourceBudgetStatus)
}

// GatewayHold is an open gateway hold as the gateway re-check reads it.
type GatewayHold struct {
	ID      string
	SpaceID string
	// Ref is the gateway reference: harbor-pilot (a quota exhaustion) or harbor-pilot:project:<id> (a budget).
	Ref  string
	Kind string
	// ProjectID is the Harbor Pilot project of a budget hold ("" for the connection-wide quota hold).
	ProjectID string
	Source    string
	ResetsAt  *time.Time
	CheckAt   *time.Time
	SeenAt    time.Time
	Version   int64
}

// Quota reports whether the hold is the connection-wide quota hold (read from the quota board).
func (h GatewayHold) Quota() bool { return h.Ref == gatewayRefHP }

// Estimated reports whether Harbor Pilot already estimated the hold's reset.
func (h GatewayHold) Estimated() bool {
	return h.ResetsAt != nil && (h.Source == SourceQuotaBoard || h.Source == SourceBudgetStatus)
}

// OpenGatewayHolds returns the open gateway holds of every space (at most maxLimitRows), oldest first.
func (svc *Service) OpenGatewayHolds(ctx context.Context) ([]GatewayHold, error) {
	rows, err := svc.s.R().ListOpenGatewayUsageLimits(ctx, maxLimitRows)
	if err != nil {
		return nil, err
	}
	out := make([]GatewayHold, 0, len(rows))
	for _, r := range rows {
		h := GatewayHold{ID: r.ID, SpaceID: r.SpaceID, Ref: r.GatewayRef, Kind: r.Kind, Source: r.Source, ResetsAt: microsPtr(r.ResetsAt),
			CheckAt: microsPtr(r.CheckAt), SeenAt: time.UnixMicro(r.SeenAt).UTC(), Version: r.Version}
		h.ProjectID, _ = strings.CutPrefix(r.GatewayRef, gatewayRefHPProject)
		if h.ProjectID == r.GatewayRef {
			h.ProjectID = ""
		}
		out = append(out, h)
	}
	return out, nil
}

// GatewayEstimate is one re-check's finding about a gateway hold.
type GatewayEstimate struct {
	// Source is what was read: SourceQuotaBoard or SourceBudgetStatus.
	Source string
	// Recovered clears the hold: the accounts it waited for are available again, or no covering budget blocks.
	Recovered bool
	// ResetsAt is the estimated reset (required unless Recovered).
	ResetsAt time.Time
}

// ApplyGatewayEstimate applies a re-check to the hold h, as read: it is skipped (false) when the hold changed or
// cleared meanwhile, and the next round reads it again. A recovered hold is cleared (cleared_by is the source), which
// wakes the waits it kept. Otherwise the hold gets the estimate as its reset, the next re-check as check_at, and the
// waits parked under it are re-timed to the new reset. It returns whether anything was written.
func (svc *Service) ApplyGatewayEstimate(ctx context.Context, h GatewayHold, e GatewayEstimate) (bool, error) {
	if e.Source != SourceQuotaBoard && e.Source != SourceBudgetStatus {
		return false, errors.New("fleet: gateway estimate needs a source")
	}
	if !e.Recovered && e.ResetsAt.IsZero() {
		return false, errors.New("fleet: gateway estimate needs a reset")
	}
	wrote := false
	err := svc.s.W().TxNamed(ctx, "fleet.gateway_hold_estimate", func(tx *store.Tx) error {
		wrote = false
		u, err := tx.Q.GetUsageLimit(ctx, h.ID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		} else if err != nil {
			return err
		}
		if u.ClearedAt.Valid || u.Version != h.Version || u.Scope != LimitScopeGateway {
			return nil
		}
		if e.Recovered {
			wrote = true
			return svc.clearHoldTx(ctx, tx, u, e.Source, u.ID)
		}
		reset := sql.NullInt64{Int64: e.ResetsAt.UnixMicro(), Valid: true}
		check := sql.NullInt64{Int64: tx.Now.Add(GatewayRecheck).UnixMicro(), Valid: true}
		n, err := tx.Q.SetUsageLimitEstimate(ctx, db.SetUsageLimitEstimateParams{ResetsAt: reset, CheckAt: check, Source: e.Source, Now: tx.NowMicros(),
			ID: u.ID, Version: u.Version})
		if err != nil || n == 0 {
			return err
		}
		wrote = true
		if err := svc.record(ctx, tx, u.SpaceID, "usage_limit", u.ID, "updated", u.Version+1); err != nil {
			return err
		}
		if u.ResetsAt == reset {
			return nil
		}
		return svc.retimeHoldWaitsTx(ctx, tx, u, time.UnixMicro(reset.Int64).Add(limitResumeSlack))
	})
	return wrote, err
}

// retimeHoldWaitsTx moves the waiting waits a gateway hold keeps to at, its new estimated reset plus the resume slack:
// those parked under it, to at whichever way it moves, and those put off as account_held whose launch goes through the
// gateway, only when at is earlier (another hold may keep them longer; their resume finds out).
func (svc *Service) retimeHoldWaitsTx(ctx context.Context, tx *store.Tx, u db.UsageLimit, at time.Time) error {
	rows, err := tx.Q.ListHoldWaitsToWake(ctx, db.ListHoldWaitsToWakeParams{Now: tx.NowMicros(), UsageLimitID: u.ID, Lim: limitWaitBatch})
	if err != nil || len(rows) == 0 {
		return err
	}
	moved := false
	for _, w := range rows {
		if w.ResumeAt == at.UnixMicro() {
			continue
		}
		if w.UsageLimitID != u.ID {
			var spec LaunchSpec
			if json.Unmarshal([]byte(w.LaunchJson), &spec) != nil || spec.Gateway == nil || spec.Gateway.Kind == "" || at.UnixMicro() > w.ResumeAt {
				continue
			}
		}
		next := at
		if next.Before(tx.Now) {
			next = tx.Now
		}
		if err := svc.deferWaitTx(ctx, tx, w, next, w.LastError); err != nil {
			return err
		}
		moved = true
	}
	if moved {
		tx.AfterCommit(svc.wakeDeadlines)
	}
	return nil
}
