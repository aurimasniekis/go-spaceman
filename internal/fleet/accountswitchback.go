package fleet

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Switching back (ADR-0105). A run that a usage limit moved to another account of its pool (ADR-0090) goes back to
// the account its chain of usage-limit switches started from (the origin) once that account has reset, at an idle point
// between turns, never mid-turn. Only a run whose current account came from a usage-limit switch qualifies: the
// instance's newest switch that moved something is a usage-limit switch that put it on the account it runs on now. The
// origin must have no open hold whose reset plus limitResumeSlack is still ahead (an unknown reset blocks), and must be
// what switchTargetTx returns with it preferred (bound, enabled, in the role's pool, not held, through the headroom
// gate). The move is ADR-0090's park with an immediate resume, decided and parked in the transaction that sees the idle
// point: a switch row of cause switch_back and a limit wait of cause switch_back due at once on the origin. It is
// checked at every idle report, and by the limit-wait job at the origin's reset and at most every switchBackRecheck.

const (
	// WaitCauseSwitchBack is the cause of a switch-back's limit wait (limit_waits.cause).
	WaitCauseSwitchBack = "switch_back"
	// SwitchBackOff is the fixed reason of a switch-back that fell back because the space turned switching back off
	// before the run resumed.
	SwitchBackOff = "switch_back_off"
	// switchBackMinGap is the ping-pong guard: an instance is switched back at most once in this window, longer than a
	// five-hour usage window.
	switchBackMinGap = 6 * time.Hour
	// switchBackRecheck is how often the limit-wait job looks at runs that qualify but for a reason that has no time of
	// its own (a hold with an unknown reset, the pool, the headroom gate, a dialog or takeover).
	switchBackRecheck = 10 * time.Minute
)

// betweenTurns is a program status at which a run may be switched back: its turn ended without an error.
func betweenTurns(s string) bool { return s == "idle" || s == "done" }

// switchBackOrigin returns the origin account a run would go back to and the account it is on, or "" when the run is
// not one a usage limit moved: its instance's newest switch that moved something must be a usage-limit switch, settled
// (succeeded, or failed with the run resumed fresh or waiting there), that put it on the account its launch names now.
// Gateway runs, runs of kinds that aren't parked, and runs moved for any other cause never qualify.
func switchBackOrigin(ctx context.Context, q *db.Queries, r db.Run) (origin, current string, err error) {
	if !parkKind(r.Kind) {
		return "", "", nil
	}
	scope, account, _, ok := limitScope(r, "")
	if !ok || scope != LimitScopeAccount {
		return "", "", nil
	}
	sw, err := q.GetNewestAccountSwitchOfInstance(ctx, db.GetNewestAccountSwitchOfInstanceParams{SpaceID: r.SpaceID, InstanceID: r.InstanceID})
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	} else if err != nil {
		return "", "", err
	}
	if sw.Cause != SwitchCauseUsageLimit || sw.State != SwitchSucceeded && sw.State != SwitchFailed || sw.OriginAccountID == "" ||
		sw.ToAccountID != account || sw.OriginAccountID == account {
		return "", "", nil
	}
	return sw.OriginAccountID, account, nil
}

// originReadyAt is when an account's usage limits allow a switch back to it: the latest reset plus limitResumeSlack of
// its open holds in any space (zero with none). known is false while a hold's reset is unknown: it blocks until the
// hold clears or its probe is due (a hold due for its probe, at now in Unix microseconds, doesn't count: the switch
// back is its probe).
func originReadyAt(ctx context.Context, q *db.Queries, accountID string, now int64) (at time.Time, known bool, err error) {
	rows, err := q.ListOpenUsageLimitsByAccount(ctx, accountID)
	if err != nil {
		return time.Time{}, false, err
	}
	_, probes := holdState(rows, now)
	for _, u := range rows {
		if slices.ContainsFunc(probes, func(p db.UsageLimit) bool { return p.ID == u.ID }) {
			continue
		}
		if !u.ResetsAt.Valid {
			return time.Time{}, false, nil
		}
		if t := time.UnixMicro(u.ResetsAt.Int64).Add(limitResumeSlack); t.After(at) {
			at = t
		}
	}
	return at, true, nil
}

// originReadyTx reports whether the origin is eligible to take the run back now (ADR-0105 decision 3): its limits have
// reset (plus slack) and switchTargetTx, with the origin preferred, returns it.
func (svc *Service) originReadyTx(ctx context.Context, tx *store.Tx, spaceID, harness, role, current, origin string) (bool, error) {
	at, known, err := originReadyAt(ctx, tx.Q, origin, tx.NowMicros())
	if err != nil || !known || at.After(tx.Now) {
		return false, err
	}
	target, err := svc.switchTargetTx(ctx, tx, spaceID, harness, role, current, origin)
	return target == origin, err
}

// switchBackBlockedTx reports whether something about the run, its instance or its space keeps it where it is now
// (ADR-0105 decisions 4 and 7): a blocked dialog, a takeover, an active switch or wait of the instance, a space that
// isn't active or is relocating, a node without the space's lease, a switch-back within switchBackMinGap, or a spent
// daily switch budget.
func (svc *Service) switchBackBlockedTx(ctx context.Context, tx *store.Tx, r db.Run, ul UsageLimitSettings) (bool, error) {
	if ds, err := tx.Q.ListOpenRunDialogs(ctx, r.ID); err != nil || len(ds) > 0 {
		return true, err
	}
	if _, err := tx.Q.GetActiveTakeover(ctx, r.ID); err == nil {
		return true, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return true, err
	}
	if _, err := tx.Q.GetActiveAccountSwitch(ctx, db.GetActiveAccountSwitchParams{SpaceID: r.SpaceID, InstanceID: r.InstanceID}); err == nil {
		return true, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return true, err
	}
	if _, err := tx.Q.GetActiveLimitWait(ctx, db.GetActiveLimitWaitParams{SpaceID: r.SpaceID, InstanceID: r.InstanceID}); err == nil {
		return true, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return true, err
	}
	sp, err := tx.Q.GetSpace(ctx, r.SpaceID)
	if err != nil {
		return true, err
	}
	if sp.State != "active" || sp.DesiredNodeID.Valid || sp.NodeID != r.NodeID {
		return true, nil
	}
	lease, err := tx.Q.GetLease(ctx, r.SpaceID)
	if errors.Is(err, sql.ErrNoRows) || err == nil && (lease.State != "active" || !tx.Now.Before(time.UnixMicro(lease.ExpiresAt)) || lease.NodeID != sp.NodeID) {
		return true, nil
	} else if err != nil {
		return true, err
	}
	if last, err := tx.Q.GetNewestSwitchBackOfInstance(ctx, db.GetNewestSwitchBackOfInstanceParams{SpaceID: r.SpaceID, InstanceID: r.InstanceID}); err == nil {
		if tx.Now.Sub(time.UnixMicro(last.CreatedAt)) < switchBackMinGap {
			return true, nil
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return true, err
	}
	used, err := tx.Q.CountAccountSwitchesSince(ctx, db.CountAccountSwitchesSinceParams{SpaceID: r.SpaceID, InstanceID: r.InstanceID,
		CreatedAt: tx.Now.Add(-switchBackWindow()).UnixMicro()})
	if err != nil {
		return true, err
	}
	return ul.SwitchMaxPerDay <= 0 || used >= int64(ul.SwitchMaxPerDay), nil
}

// switchBackWindow is the daily switch budget's window, which switch-backs count toward.
func switchBackWindow() time.Duration { return switchBudgetWindow }

// switchBackTx moves a live run between turns back to its chain's origin account when everything allows it
// (ADR-0105): the switch-back is recorded, the run parked with a wait due at once on the origin, and stopped. why is
// recorded on the wait's audit event. It reports whether the run was moved.
func (svc *Service) switchBackTx(ctx context.Context, tx *store.Tx, r db.Run, why string) (bool, error) {
	if r.Observed != proto.RunRunning || r.Desired != "running" || !betweenTurns(r.Status) || r.LimitKind != "" {
		return false, nil
	}
	origin, current, err := switchBackOrigin(ctx, tx.Q, r)
	if err != nil || origin == "" {
		return false, err
	}
	ul := svc.ulSettings(ctx, tx.Q, r.SpaceID)
	if !ul.Failover || !ul.SwitchBack {
		return false, nil
	}
	if session, err := resumableSession(ctx, tx.Q, r); err != nil || session == "" {
		return false, err
	}
	if blocked, err := svc.switchBackBlockedTx(ctx, tx, r, ul); err != nil || blocked {
		return false, err
	}
	if ready, err := svc.originReadyTx(ctx, tx, r.SpaceID, r.Harness, r.Role, current, origin); err != nil || !ready {
		return false, err
	}
	sw, err := svc.insertSwitchTx(ctx, tx, r, limitState{}, SwitchCauseSwitchBack, current, origin, SwitchPending, "")
	if err != nil {
		return false, err
	}
	w, err := svc.parkWithTx(ctx, tx, r, limitState{}, why, &sw)
	if err != nil {
		return false, err
	}
	if w == nil {
		// The run already has a wait: nothing moves.
		_, _, err := svc.moveSwitchTx(ctx, tx, sw, SwitchCancelled, WaitSuperseded, nil)
		return false, err
	}
	msg := "SpaceMan moves the run back to its original account, whose usage limit has reset; it continues there"
	if w.State == WaitCancelled {
		msg = parkGivenUpStop // its task is finished: nothing continues
	}
	if err := svc.stopRunTx(ctx, tx, r, "interrupt", msg); err != nil {
		return false, err
	}
	tx.AfterCommit(func() { svc.hub.Kick(r.NodeID) })
	return true, nil
}

// switchBackAtIdleTx runs at the end of a report transaction of a live run: at an idle point between turns, the run
// goes back to its origin account when it qualifies (ADR-0105 decision 4).
func (svc *Service) switchBackAtIdleTx(ctx context.Context, tx *store.Tx, runID, status string) error {
	if !betweenTurns(status) {
		return nil
	}
	r, err := tx.Q.GetRun(ctx, runID)
	if err != nil {
		return err
	}
	_, err = svc.switchBackTx(ctx, tx, r, "idle")
	return err
}

// switchBackCandidates lists the runs that may go back to their origin account, in spaces with switching back on.
// The setting is per space, so it is applied page by page: pages of maxLimitRows are read until maxLimitRows candidates
// of spaces with it on are found, so spaces with it off never use up the limit.
func (svc *Service) switchBackCandidates(ctx context.Context, q *db.Queries) ([]db.ListSwitchBackCandidatesRow, error) {
	return svc.switchBackCandidatesPage(ctx, q, maxLimitRows)
}

// switchBackCandidatesPage reads the candidates page by page, a page of page rows at a time, and returns at most
// maxLimitRows of them.
func (svc *Service) switchBackCandidatesPage(ctx context.Context, q *db.Queries, page int64) ([]db.ListSwitchBackCandidatesRow, error) {
	on := map[string]bool{}
	var out []db.ListSwitchBackCandidatesRow
	after := ""
	for {
		rows, err := q.ListSwitchBackCandidates(ctx, db.ListSwitchBackCandidatesParams{ID: after, Limit: page})
		if err != nil {
			return nil, err
		}
		for _, c := range rows {
			r, err := q.GetRun(ctx, c.RunID)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			} else if err != nil {
				return nil, err
			}
			v, ok := on[r.SpaceID]
			if !ok {
				ul := svc.ulSettings(ctx, q, r.SpaceID)
				v = ul.Failover && ul.SwitchBack
				on[r.SpaceID] = v
			}
			if v {
				out = append(out, c)
				if int64(len(out)) >= maxLimitRows {
					return out, nil
				}
			}
		}
		if len(rows) == 0 || int64(len(rows)) < page {
			return out, nil
		}
		after = rows[len(rows)-1].RunID
	}
}

// switchBackRuns is the limit-wait job's switch-back pass: a run whose origin's reset (plus slack) passed since the last
// pass is looked at now, and every candidate at most every switchBackRecheck. It returns how many runs it moved.
func (svc *Service) switchBackRuns(ctx context.Context, now time.Time) (int, error) {
	q := svc.s.R()
	rows, err := svc.switchBackCandidates(ctx, q)
	if err != nil || len(rows) == 0 {
		return 0, err
	}
	svc.lw.mu.Lock()
	full := svc.lw.backFull.IsZero() || !now.Before(svc.lw.backFull.Add(switchBackRecheck))
	if full {
		svc.lw.backFull = now
	}
	prev := svc.lw.backPass
	svc.lw.backPass = now
	svc.lw.mu.Unlock()
	n := 0
	var errs []error
	for _, c := range rows {
		if !full {
			at, known, err := originReadyAt(ctx, q, c.OriginAccountID, now.UnixMicro())
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if !known || !at.After(prev) || at.After(now) {
				continue
			}
		}
		moved := false
		err := svc.s.W().TxNamed(ctx, "fleet.account_switch_back", func(tx *store.Tx) error {
			r, err := tx.Q.GetRun(ctx, c.RunID)
			if err != nil {
				return err
			}
			moved, err = svc.switchBackTx(ctx, tx, r, "idle")
			return err
		})
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if moved {
			n++
		}
	}
	return n, errors.Join(errs...)
}

// nextSwitchBack is when the switch-back pass is next due, or zero with no candidate: the earliest origin reset (plus
// slack) not yet looked at, and the next full recheck.
func (svc *Service) nextSwitchBack(ctx context.Context, q *db.Queries, now time.Time) (time.Time, error) {
	rows, err := svc.switchBackCandidates(ctx, q)
	if err != nil || len(rows) == 0 {
		return time.Time{}, err
	}
	svc.lw.mu.Lock()
	full, pass := svc.lw.backFull, svc.lw.backPass
	svc.lw.mu.Unlock()
	next := now
	if !full.IsZero() {
		next = full.Add(switchBackRecheck)
	}
	for _, c := range rows {
		at, known, err := originReadyAt(ctx, q, c.OriginAccountID, now.UnixMicro())
		if err != nil {
			return time.Time{}, err
		}
		if known && at.After(pass) && at.Before(next) {
			next = at
		}
	}
	return next, nil
}

// switchBackResumeCheckTx re-checks, in the claim transaction of a switch-back's wait, that the origin still takes the
// run (ADR-0105 decision 5). When it doesn't (limited meanwhile, out of the pool, failover or switching back turned
// off), the switch falls back and the wait loses its target: the run resumes at once on the account it was on. A
// switch-back's resume always goes on.
func (svc *Service) switchBackResumeCheckTx(ctx context.Context, tx *store.Tx, w *db.LimitWait) (bool, error) {
	if w.SwitchToAccountID == "" {
		return true, nil
	}
	var spec LaunchSpec
	_ = json.Unmarshal([]byte(w.LaunchJson), &spec)
	current := spec.AccountRef
	ul := svc.ulSettings(ctx, tx.Q, w.SpaceID)
	reason := ""
	switch {
	case !ul.Failover:
		reason = SwitchFailoverOff
	case !ul.SwitchBack:
		reason = SwitchBackOff
	default:
		ready, err := svc.originReadyTx(ctx, tx, w.SpaceID, w.Harness, w.Role, current, w.SwitchToAccountID)
		if err != nil {
			return false, err
		}
		if ready {
			return true, nil
		}
		reason = SwitchNoEligibleAccount
	}
	now := tx.NowMicros()
	n, err := tx.Q.SetLimitWaitSwitch(ctx, db.SetLimitWaitSwitchParams{SwitchToAccountID: "", ResumeAt: w.ResumeAt, Now: now, ID: w.ID, Version: w.Version})
	if err != nil || n == 0 {
		return false, err
	}
	w.SwitchToAccountID, w.Version, w.UpdatedAt = "", w.Version+1, now
	if err := svc.record(ctx, tx, w.SpaceID, "limit_wait", w.ID, "updated", w.Version); err != nil {
		return false, err
	}
	if err := svc.settleSwitchTx(ctx, tx, w.AccountSwitchID, SwitchFellBack, reason); err != nil {
		return false, err
	}
	return true, nil
}
