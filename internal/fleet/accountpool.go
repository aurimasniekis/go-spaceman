package fleet

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/accounts"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
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
	return poolExhausted(ctx, tx.Q, tx.NowMicros(), spaceID, harness, role)
}

// poolExhausted is poolExhaustedTx on any queries, at now (Unix microseconds).
func poolExhausted(ctx context.Context, q *db.Queries, now int64, spaceID, harness, role string) (*PoolExhausted, error) {
	ids, err := accounts.PoolAccountsQ(ctx, q, spaceID, harness, role)
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	var earliest int64
	known := false
	for _, id := range ids {
		rows, err := q.ListOpenUsageLimitsByAccount(ctx, id)
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

// LaunchHeld reports whether a new run of role in the space could get no account now: every account of the pool it
// would launch from has an open usage limit that has not reset (as poolExhaustedTx judges). The role's harness comes
// from its profile (the space role's, else its template's); when it is unknown (no such role, or no profile), every
// harness the space has accounts for is judged, and the launch is held only when all of their pools are. A pool with no
// usable account, or a space with no accounts, is never judged held: what a launch would do then is not known here.
// The control watchdog reads it so ready tasks no account can run are not reported as work waiting (ADR-0099).
func (svc *Service) LaunchHeld(ctx context.Context, spaceID, role string) (bool, error) {
	held := false
	err := svc.s.ReadTx(ctx, func(q *db.Queries) error {
		now := svc.opts.Clock.Now().UnixMicro()
		harnesses, err := roleHarnesses(ctx, q, spaceID, role)
		if err != nil || len(harnesses) == 0 {
			return err
		}
		for _, h := range harnesses {
			p, err := poolExhausted(ctx, q, now, spaceID, h, role)
			if err != nil || p == nil {
				return err
			}
		}
		held = true
		return nil
	})
	return held, err
}

// roleHarnesses is the harness a run of role launches with, or, when that is unknown, every harness the space has
// accounts bound for.
func roleHarnesses(ctx context.Context, q *db.Queries, spaceID, role string) ([]string, error) {
	if role != "" {
		sr, err := q.GetSpaceRole(ctx, db.GetSpaceRoleParams{SpaceID: spaceID, Name: role})
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return nil, err
		default:
			profileID := sr.ProfileID
			if profileID == "" {
				if tpl, err := q.GetRoleTemplate(ctx, sr.TemplateID); err == nil {
					profileID = tpl.ProfileID
				} else if !errors.Is(err, sql.ErrNoRows) {
					return nil, err
				}
			}
			if profileID != "" {
				if p, err := q.GetProfile(ctx, profileID); err == nil && p.Harness != "" {
					return []string{p.Harness}, nil
				} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
					return nil, err
				}
			}
		}
	}
	rows, err := q.ListSpaceAccounts(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range rows {
		if !slices.Contains(out, r.Harness) {
			out = append(out, r.Harness)
		}
	}
	return out, nil
}
