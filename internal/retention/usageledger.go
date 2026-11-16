package retention

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Retention of the usage ledger and of closed limit rows (ADR-0090, Q9). These are installation-wide fixed periods, not
// space limits: the figures are small aggregates, and a per-space override would make a space's usage history differ
// from the account's.
const (
	// HourlyBucketDays is how long hourly usage buckets are kept before they are rolled into daily buckets.
	HourlyBucketDays = 90
	// DailyBucketDays is how long daily usage buckets are kept.
	DailyBucketDays = 730
	// WindowSampleDays is how long account window samples (the chart points) are kept.
	WindowSampleDays = 30
	// ClosedLimitDays is how long closed holds, finished waits and finished switches are kept.
	ClosedLimitDays = 180
	// LedgerMarkDays is how long a session's counted counters are kept after its last usage report. Usage reported for a
	// session idle for longer counts its whole history again, so this is the same as the longest closed-row period.
	LedgerMarkDays = ClosedLimitDays

	ledgerPruneBatch = 200
)

const day = 24 * time.Hour

// pruneUsageLedger rolls old hourly buckets into daily ones, deletes old daily buckets, window samples and ledger marks,
// and deletes closed holds, finished waits and finished switches. Every step runs in short transactions and adds to
// counts; a failing step does not stop the others.
func (svc *Service) pruneUsageLedger(ctx context.Context, counts Counts) error {
	now := svc.o.Clock.Now().UTC()
	var errs []error
	n, err := svc.rollUpBuckets(ctx, now)
	counts[KindUsageBuckets] += n
	errs = append(errs, err)

	dailyCut := now.Add(-DailyBucketDays * day).Truncate(day).UnixMicro()
	err = svc.s.W().TxNamed(ctx, "retention.usage_daily", func(tx *store.Tx) error {
		var err error
		n, err = tx.Q.DeleteDailyBucketsBefore(ctx, dailyCut)
		return err
	})
	counts[KindUsageBuckets] += n
	errs = append(errs, err)

	batched := func(name, kind string, cut time.Duration, del func(tx *store.Tx, cutoff, lim int64) (int64, error)) {
		cutoff := now.Add(-cut).UnixMicro()
		var total int64
		for ctx.Err() == nil {
			var n int64
			if err := svc.s.W().TxNamed(ctx, name, func(tx *store.Tx) error {
				var err error
				n, err = del(tx, cutoff, ledgerPruneBatch)
				return err
			}); err != nil {
				errs = append(errs, err)
				break
			}
			total += n
			if n < ledgerPruneBatch {
				break
			}
		}
		counts[kind] += total
	}
	batched("retention.window_samples", KindWindowSamples, WindowSampleDays*day, func(tx *store.Tx, c, l int64) (int64, error) {
		return tx.Q.PruneAccountWindowSamples(ctx, db.PruneAccountWindowSamplesParams{Cutoff: c, Lim: l})
	})
	batched("retention.ledger_marks", KindLedgerMarks, LedgerMarkDays*day, func(tx *store.Tx, c, l int64) (int64, error) {
		return tx.Q.PruneUsageLedgerMarks(ctx, db.PruneUsageLedgerMarksParams{Cutoff: c, Lim: l})
	})
	batched("retention.usage_limits", KindUsageLimits, ClosedLimitDays*day, func(tx *store.Tx, c, l int64) (int64, error) {
		return tx.Q.PruneClosedUsageLimits(ctx, db.PruneClosedUsageLimitsParams{Cutoff: sql.NullInt64{Int64: c, Valid: true}, Lim: l})
	})
	batched("retention.limit_waits", KindLimitWaits, ClosedLimitDays*day, func(tx *store.Tx, c, l int64) (int64, error) {
		return tx.Q.PruneClosedLimitWaits(ctx, db.PruneClosedLimitWaitsParams{Cutoff: c, Lim: l})
	})
	batched("retention.account_switches", KindAccountSwitches, ClosedLimitDays*day, func(tx *store.Tx, c, l int64) (int64, error) {
		return tx.Q.PruneClosedAccountSwitches(ctx, db.PruneClosedAccountSwitchesParams{Cutoff: c, Lim: l})
	})
	return errors.Join(errs...)
}

// rollUpBuckets adds the hourly buckets older than HourlyBucketDays to daily buckets (UTC days) and deletes them, one
// whole day per transaction, so a day is either all rolled up or not at all. It returns the hourly rows rolled up.
func (svc *Service) rollUpBuckets(ctx context.Context, now time.Time) (int64, error) {
	// Whole days only: the day that contains the cutoff keeps its hourly rows until it is entirely older.
	limit := now.Add(-HourlyBucketDays * day).Truncate(day).UnixMicro()
	var total int64
	for ctx.Err() == nil {
		oldest, err := svc.s.R().OldestHourlyBucketBefore(ctx, limit)
		if errors.Is(err, sql.ErrNoRows) {
			return total, nil
		} else if err != nil {
			return total, err
		}
		from := time.UnixMicro(oldest).UTC().Truncate(day)
		to := from.Add(day)
		var n int64
		if err := svc.s.W().TxNamed(ctx, "retention.usage_rollup", func(tx *store.Tx) error {
			if err := tx.Q.RollUpHourlyBuckets(ctx, db.RollUpHourlyBucketsParams{DayStart: from.UnixMicro(), FromStart: from.UnixMicro(), ToStart: to.UnixMicro()}); err != nil {
				return err
			}
			var err error
			n, err = tx.Q.DeleteHourlyBuckets(ctx, db.DeleteHourlyBucketsParams{BucketStart: from.UnixMicro(), BucketStart_2: to.UnixMicro()})
			return err
		}); err != nil {
			return total, err
		}
		total += n
	}
	return total, ctx.Err()
}
