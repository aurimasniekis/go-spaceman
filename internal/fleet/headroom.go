package fleet

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/accounts"
	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/obs"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
	"github.com/aurimasniekis/go-spaceman/internal/usage"
)

// The launch-time headroom gate (ADR-0090 decision 4, usage_limit_min_headroom_percent). It only removes accounts from
// the set a launch may use: the pool never widens, and a running agent is never moved because of it.

// headroomOn reports whether the gate is active for a launch.
func (svc *Service) headroomOn(ul UsageLimitSettings) bool {
	return ul.MinHeadroomPercent > 0 && svc.opts.AccountHeadroom != nil
}

// belowHeadroom reports whether an account's most-used known window leaves less than the configured percentage free
// (100 - used < min; exactly at the threshold passes), and when that window resets (zero when unknown). An account
// with no known usage is never below.
func (svc *Service) belowHeadroom(ctx context.Context, q *db.Queries, ul UsageLimitSettings, accountID string) (bool, time.Time, error) {
	if !svc.headroomOn(ul) {
		return false, time.Time{}, nil
	}
	used, resets, known, err := svc.opts.AccountHeadroom(ctx, q, accountID)
	if err != nil {
		// Fail open: a ledger that can't be read leaves the account eligible.
		svc.opts.Log.Warn("headroom gate: read account usage", slog.String("account.id", accountID), obs.Err(err))
		return false, time.Time{}, nil //nolint:nilerr // fail open: an unreadable usage ledger never blocks a launch
	}
	// No usage known leaves the account eligible, and a window whose reset has passed is stale and no longer counts.
	if !known || (!resets.IsZero() && !resets.After(svc.now())) {
		return false, time.Time{}, nil
	}
	return 100-used < float64(ul.MinHeadroomPercent), resets, nil
}

// withHeadroom adds the gate to a launch's avoid function (nil: avoid nothing but what the gate removes).
func (svc *Service) withHeadroom(ctx context.Context, q *db.Queries, ul UsageLimitSettings, avoid func(string) (bool, error)) func(string) (bool, error) {
	if !svc.headroomOn(ul) {
		return avoid
	}
	return func(id string) (bool, error) {
		if avoid != nil {
			if held, err := avoid(id); err != nil || held {
				return held, err
			}
		}
		below, _, err := svc.belowHeadroom(ctx, q, ul, id)
		return below, err
	}
}

// checkHeadroom refuses a launch whose chosen account is below the gate, which means Resolve found no eligible account
// in the pool (it prefers any other). The refusal is a HoldError (429) until the earliest known reset of the pool's
// accounts (at most usage.WindowMaxAge from now), so the caller waits as it does for an exhausted account.
func (svc *Service) checkHeadroom(ctx context.Context, tx *store.Tx, ul UsageLimitSettings, spaceID, harness, role, chosen string) error {
	below, _, err := svc.belowHeadroom(ctx, tx.Q, ul, chosen)
	if err != nil || !below {
		return err
	}
	pool, err := accounts.PoolAccounts(ctx, tx, spaceID, harness, role)
	if err != nil {
		return err
	}
	var earliest time.Time
	for _, id := range append([]string{chosen}, pool...) {
		if b, at, err := svc.belowHeadroom(ctx, tx.Q, ul, id); err == nil && b && !at.IsZero() && (earliest.IsZero() || at.Before(earliest)) {
			earliest = at
		}
	}
	// A window counts for the gate only while it is fresh (usage.WindowMaxAge), so the account may become eligible again
	// that long from now whatever the reset says: the wait never exceeds it (a seven-day reset would defer for days).
	if !earliest.IsZero() {
		if limit := svc.now().Add(usage.WindowMaxAge); earliest.After(limit) {
			earliest = limit
		}
	}
	msg := fmt.Sprintf("every %s account usable here has less than %d%% of a usage window left (the launch headroom gate); launches wait for a reset", harness, ul.MinHeadroomPercent)
	if !earliest.IsZero() {
		msg = fmt.Sprintf("every %s account usable here has less than %d%% of a usage window left (the launch headroom gate); the earliest reset (or recheck) is %s",
			harness, ul.MinHeadroomPercent, earliest.UTC().Format(holdTimeLayout))
	}
	return &HoldError{Gate: true, Target: HoldTarget{AccountID: chosen}, Until: earliest, Known: !earliest.IsZero(), err: apperr.New(apperr.CodeRateLimited, "%s", msg)}
}
