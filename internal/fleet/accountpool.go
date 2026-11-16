package fleet

import (
	"context"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/accounts"
	"github.com/aurimasniekis/go-spaceman/internal/store"
)

// Pool exhausted (ADR-0090, plan T10). With failover on, when every account of a run's pool (the role's pool when it
// has one, otherwise the space's accounts for the harness) holds an open usage limit, nothing can take the run over:
// its wait resumes at the earliest reset in the pool, not at its own account's. The resume re-resolves its account
// through the launch path (the role's pool, never widened), so it lands on whichever account is free by then.

// PoolExhausted describes a pool whose accounts are all limited: the wait's control message and the people notice read it.
type PoolExhausted struct {
	Harness, Role string
	// Accounts are the pool's account IDs.
	Accounts []string
	// ResetsAt is the earliest reset in the pool; nil when no limit of the pool has a known reset.
	ResetsAt *time.Time
}

// poolExhaustedTx reports the exhaustion of the pool of role for harness, or nil when the pool is empty or any of its
// accounts is free. An account is free when it has no open hold that hasn't reset (as accountHeld judges); it frees up
// when the last of its holds resets, and not at all, as far as anyone knows, when one of them has no known reset.
func (svc *Service) poolExhaustedTx(ctx context.Context, tx *store.Tx, spaceID, harness, role string) (*PoolExhausted, error) {
	ids, err := accounts.PoolAccounts(ctx, tx, spaceID, harness, role)
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	now := tx.NowMicros()
	var earliest int64
	known := false
	for _, id := range ids {
		rows, err := tx.Q.ListOpenUsageLimitsByAccount(ctx, id)
		if err != nil {
			return nil, err
		}
		var free int64
		blocked, unknown := false, false
		for _, u := range rows {
			switch {
			case !u.ResetsAt.Valid:
				blocked, unknown = true, true
			case u.ResetsAt.Int64 > now:
				blocked = true
				free = max(free, u.ResetsAt.Int64)
			}
		}
		if !blocked {
			return nil, nil
		}
		if !unknown && (!known || free < earliest) {
			earliest, known = free, true
		}
	}
	p := &PoolExhausted{Harness: harness, Role: role, Accounts: ids}
	if known {
		t := time.UnixMicro(earliest).UTC()
		p.ResetsAt = &t
	}
	return p, nil
}

// poolWaitTx moves a wait's resume time at (computed from the limited account's own limit) to the earliest reset in
// the pool when the whole pool is limited. It returns the new time and the exhaustion, or at and nil when the pool
// still has a free account, failover is off, or the limit isn't an account's.
func (svc *Service) poolWaitTx(ctx context.Context, tx *store.Tx, ul UsageLimitSettings, spaceID, harness, role string, at time.Time) (time.Time, *PoolExhausted, error) {
	if !ul.Failover {
		return at, nil, nil
	}
	p, err := svc.poolExhaustedTx(ctx, tx, spaceID, harness, role)
	if err != nil || p == nil {
		return at, nil, err
	}
	if p.ResetsAt != nil {
		if early := p.ResetsAt.Add(limitResumeSlack); early.Before(at) {
			at = early
		}
	}
	if at.Before(tx.Now) {
		at = tx.Now
	}
	return at, p, nil
}
