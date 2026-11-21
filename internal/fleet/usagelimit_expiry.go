package fleet

import (
	"context"
	"log/slog"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// An open account hold always ends (ADR-0079 amendment, "holds always end"). A hold whose reset is unknown has no time
// that ends it and only a probe that a launch may claim, so a hold whose run was gone (or whose limit report was
// missed) used to stay open for good. Two things end it now: the hold's own window passes (the expiry sweep, a bound
// from the time the hold was seen), or fresh evidence from the account shows it over (a status-line snapshot of the
// account's usage under 100 %).
const (
	// ClearedByExpiry is cleared_by on a hold with an unknown reset that outlived its window.
	ClearedByExpiry = "expired"
	// limitFiveHourWindow and limitSevenDayWindow are how long a limit of that window can last at most.
	limitFiveHourWindow = 5 * time.Hour
	limitSevenDayWindow = 7 * 24 * time.Hour
)

// holdMaxAge is how long a hold of a kind with an unknown reset can last: the kind's window. A kind with no window of
// its own (unknown, overage) gets the default keep-alive, so a run still waiting out the limit is never freed under it
// before it is parked; a limit that is still on simply opens a new hold at the next launch.
func holdMaxAge(kind string) time.Duration {
	switch kind {
	case proto.LimitFiveHour:
		return limitFiveHourWindow
	case proto.LimitSevenDay, proto.LimitSevenDayOpus, proto.LimitSevenDaySonnet, proto.LimitSevenDayOverage:
		return limitSevenDayWindow
	}
	return DefaultLimitKeepAliveMax
}

// holdExpiry is when an account hold with an unknown reset ends at the latest.
func holdExpiry(u db.UsageLimit) time.Time {
	return time.UnixMicro(u.SeenAt).Add(holdMaxAge(u.Kind))
}

// NextHoldExpiry is the earliest time an open account hold with an unknown reset expires (zero: none).
func (svc *Service) NextHoldExpiry(ctx context.Context) (time.Time, error) {
	rows, err := svc.s.R().ListOpenUnknownResetAccountLimits(ctx, maxLimitRows)
	if err != nil {
		return time.Time{}, err
	}
	var next time.Time
	for _, u := range rows {
		if at := holdExpiry(u); next.IsZero() || at.Before(next) {
			next = at
		}
	}
	return next, nil
}

// expireHolds ends the open account holds with an unknown reset whose window passed, and returns how many it ended.
func (svc *Service) expireHolds(ctx context.Context, now time.Time) (int, error) {
	rows, err := svc.s.R().ListOpenUnknownResetAccountLimits(ctx, maxLimitRows)
	if err != nil {
		return 0, err
	}
	due := false
	for _, u := range rows {
		if !holdExpiry(u).After(now) {
			due = true
			break
		}
	}
	if !due {
		return 0, nil
	}
	n := 0
	err = svc.s.W().TxNamed(ctx, "fleet.usage_limit_expire", func(tx *store.Tx) error {
		n = 0
		rows, err := tx.Q.ListOpenUnknownResetAccountLimits(ctx, maxLimitRows)
		if err != nil {
			return err
		}
		for _, u := range rows {
			if holdExpiry(u).After(tx.Now) {
				continue
			}
			if err := svc.clearHoldTx(ctx, tx, u, ClearedByExpiry, u.ID); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	if n > 0 {
		svc.opts.Log.Info("usage-limit holds expired", slog.Int("count", n))
	}
	return n, err
}

// windowFor is the usage window that shows a hold of kind over, or "" when none does on its own. A per-model weekly
// limit (Opus, Sonnet) isn't visible in the account's weekly total, so a snapshot can't clear it. A kind without one
// window (unknown, overage) needs both account windows in the snapshot, each under 100 %.
func windowFor(kind string) (window string, all bool) {
	switch kind {
	case proto.LimitFiveHour:
		return proto.WindowFiveHour, false
	case proto.LimitSevenDay, proto.LimitSevenDayOverage:
		return proto.WindowSevenDay, false
	case proto.LimitUnknown, proto.LimitOverage:
		return "", true
	}
	return "", false
}

// ClearHoldsOnWindowsTx ends the open holds on an account that a fresh status-line snapshot shows over, in the
// transaction that books the snapshot (the usage ledger calls it with the windows it validated). Only a hold whose reset
// is unknown counts (a known reset is trusted, ADR-0079 decision 7), and only a snapshot observed after the hold was
// seen: a window under 100 % that predates the limit says nothing. It clears the hold in every space, as cleared_by
// "evidence", like another space's run that proved it over.
func (svc *Service) ClearHoldsOnWindowsTx(ctx context.Context, tx *store.Tx, accountID string, windows []proto.UsageWindow) error {
	if accountID == "" || len(windows) == 0 {
		return nil
	}
	rows, err := tx.Q.ListOpenUsageLimitsByAccount(ctx, accountID)
	if err != nil || len(rows) == 0 {
		return err
	}
	// A snapshot older than the window the account already holds (a late report of another run) is not evidence.
	stored, err := tx.Q.ListAccountWindows(ctx, accountID)
	if err != nil {
		return err
	}
	newest := map[string]int64{}
	for _, w := range stored {
		newest[w.Kind] = w.ObservedAt
	}
	for _, u := range rows {
		if u.ResetsAt.Valid {
			continue
		}
		window, all := windowFor(u.Kind)
		if window == "" && !all {
			continue
		}
		over, seen := true, map[string]bool{}
		for _, w := range windows {
			if w.ObservedAt.UnixMicro() <= u.SeenAt || w.ObservedAt.UnixMicro() < newest[w.Kind] || window != "" && w.Kind != window {
				continue
			}
			seen[w.Kind] = true
			if w.UsedPct >= 100 {
				over = false
			}
		}
		// A kind without one window of its own needs both account windows in the snapshot.
		if !over || window != "" && !seen[window] || window == "" && (!seen[proto.WindowFiveHour] || !seen[proto.WindowSevenDay]) {
			continue
		}
		if err := svc.clearHoldTx(ctx, tx, u, ClearedByEvidence, u.ID); err != nil {
			return err
		}
	}
	return nil
}
