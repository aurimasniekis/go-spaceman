package fleet

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

const (
	// DefaultLimitNotifyAfter is how far away a hold's reset must be for the space's people to be told (Q5: 12 h, so
	// weekly limits and not 5-hour ones).
	DefaultLimitNotifyAfter = 12 * time.Hour
	// minLimitNotifyAfter is the smallest threshold a space can set (usage_limit_notify_after_hours ≥ 1): holds whose
	// reset is closer are never read.
	minLimitNotifyAfter = time.Hour
)

// LongHold is an open hold of a space whose reset is further away than the space's people-notice threshold and whose
// people weren't told yet (ADR-0079 Q5): what the human action says. Runs counts the space's runs waiting on it (kept
// alive at the limit, or parked under it), Tasks their tasks.
type LongHold struct {
	Hold UsageLimit
	// Space and SpaceSlug are the space's name and slug; Subject names what is held ("account \"x\" is at its weekly
	// usage limit").
	Space, SpaceSlug, Subject string
	Runs, Tasks               int
	NotifyAfter               time.Duration
}

// LongHoldsToTell returns the open holds whose space's people should be told about, now: a known reset further away
// than the space's usage_limit_notify_after_hours (0: never), not yet told, the space not archived. A hold with an
// unknown reset is not announced: nobody knows the wait is long. A hold whose reset moves later is announced when it
// crosses the threshold; once told, a hold never is again (MarkHoldTold). Every untold hold is read, a page at a
// time, so holds that stay untold (a space's threshold above their wait, or a deferred notice) can't starve others.
func (svc *Service) LongHoldsToTell(ctx context.Context) ([]LongHold, error) {
	q := svc.s.R()
	now := svc.now()
	p := db.ListUsageLimitsToTellPeopleParams{After: sql.NullInt64{Int64: now.Add(minLimitNotifyAfter).UnixMicro(), Valid: true},
		CursorReset: sql.NullInt64{Int64: math.MaxInt64, Valid: true}, Lim: maxLimitRows}
	var rows []db.UsageLimit
	for {
		page, err := q.ListUsageLimitsToTellPeople(ctx, p)
		if err != nil {
			return nil, err
		}
		rows = append(rows, page...)
		if len(page) < maxLimitRows {
			break
		}
		last := page[len(page)-1]
		p.CursorReset, p.CursorID = last.ResetsAt, last.ID
	}
	settings := map[string]UsageLimitSettings{}
	var out []LongHold
	var live []db.Run
	liveRead := false
	for _, u := range rows {
		ul, ok := settings[u.SpaceID]
		if !ok {
			ul = svc.ulSettings(ctx, q, u.SpaceID)
			settings[u.SpaceID] = ul
		}
		if ul.NotifyAfter <= 0 || time.UnixMicro(u.ResetsAt.Int64).Sub(now) <= ul.NotifyAfter {
			continue
		}
		sp, err := q.GetSpace(ctx, u.SpaceID)
		if err != nil {
			return nil, err
		}
		h := LongHold{Hold: usageLimitFromRow(u), Space: sp.Name, SpaceSlug: sp.Slug, NotifyAfter: ul.NotifyAfter,
			Subject: holdSubject(ctx, q, HoldTarget{AccountID: u.AccountID}, u)}
		if !liveRead {
			if live, err = q.ListLimitedLiveRuns(ctx, maxLimitRows); err != nil {
				return nil, err
			}
			liveRead = true
		}
		if h.Runs, h.Tasks, err = holdWaiting(ctx, q, u, live); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, nil
}

// holdWaiting counts the space's runs waiting on a hold and their tasks: live runs at the hold's limit on its account
// (or gateway reference), and waits parked under it.
func holdWaiting(ctx context.Context, q *db.Queries, u db.UsageLimit, live []db.Run) (runs, tasks int, err error) {
	for _, r := range live {
		if r.SpaceID != u.SpaceID || r.LimitKind != u.Kind {
			continue
		}
		if scope, account, gw, ok := limitScope(r, r.LimitKind); !ok || scope != u.Scope || account != u.AccountID || gw != u.GatewayRef {
			continue
		}
		runs++
		if _, err := q.GetTaskIDForRun(ctx, r.ID); err == nil {
			tasks++
		} else if !errors.Is(err, sql.ErrNoRows) {
			return 0, 0, err
		}
	}
	waits, err := q.ListLimitWaitsBySpace(ctx, db.ListLimitWaitsBySpaceParams{SpaceID: u.SpaceID, Limit: maxLimitRows})
	if err != nil {
		return 0, 0, err
	}
	for _, w := range waits {
		if w.UsageLimitID != u.ID || (w.State != "waiting" && w.State != "resuming") {
			continue
		}
		runs++
		if w.TaskID != "" {
			tasks++
		}
	}
	return runs, tasks, nil
}

// MarkHoldTold records that a hold's people were told (usage_limits.notified_humans_at): it is never announced again.
func (svc *Service) MarkHoldTold(ctx context.Context, limitID string) error {
	return svc.s.W().TxNamed(ctx, "fleet.usage_limit_told", func(tx *store.Tx) error {
		n, err := tx.Q.MarkUsageLimitHumansNotified(ctx, db.MarkUsageLimitHumansNotifiedParams{Now: tx.NowMicros(), ID: limitID})
		if err != nil || n == 0 {
			return err
		}
		u, err := tx.Q.GetUsageLimit(ctx, limitID)
		if err != nil {
			return err
		}
		return svc.record(ctx, tx, u.SpaceID, "usage_limit", u.ID, "updated", u.Version)
	})
}
