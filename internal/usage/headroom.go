package usage

import (
	"context"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// WindowMaxAge is how old an account window's last observation may be and still count as known: a window nobody has
// reported for longer may have moved (or its account may be idle), so the headroom gate treats it as unknown.
const WindowMaxAge = time.Hour

// MostUsedWindow returns the account's most-used current window: its used percentage and reset time (zero when the
// window gave none). A window whose reset has passed (it started over) or whose last observation is older than
// WindowMaxAge is stale and does not count; with no current window known is false.
func MostUsedWindow(ctx context.Context, q *db.Queries, accountID string, now time.Time) (usedPct float64, resetsAt time.Time, known bool, err error) {
	ws, err := q.ListAccountWindows(ctx, accountID)
	if err != nil {
		return 0, time.Time{}, false, err
	}
	for _, w := range ws {
		if now.Sub(time.UnixMicro(w.ObservedAt)) > WindowMaxAge {
			continue
		}
		var reset time.Time
		if w.ResetsAt.Valid {
			reset = time.UnixMicro(w.ResetsAt.Int64).UTC()
			if !reset.After(now) {
				continue
			}
		}
		if !known || w.UsedPct > usedPct {
			usedPct, resetsAt, known = w.UsedPct, reset, true
		}
	}
	return usedPct, resetsAt, known, nil
}

// AccountHeadroomFunc is the read the launch-time headroom gate takes as fleet.Options.AccountHeadroom, on the given
// clock.
func AccountHeadroomFunc(now func() time.Time) func(ctx context.Context, q *db.Queries, accountID string) (float64, time.Time, bool, error) {
	return func(ctx context.Context, q *db.Queries, accountID string) (float64, time.Time, bool, error) {
		return MostUsedWindow(ctx, q, accountID, now())
	}
}
