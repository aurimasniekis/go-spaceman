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
// missed) used to stay open for good. The expiry sweep ends it: the hold's own window passes, a bound from the time the
// hold was seen. A status-line snapshot is deliberately not evidence: its observed time is when the line was drawn on the
// node, not when the API data was read, so a limited run's own idle report (window under 100 %) would clear a real limit.
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
