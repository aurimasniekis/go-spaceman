package fleet

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/accounts"
	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/obs/metrics"
	"github.com/aurimasniekis/go-spaceman/internal/outbox"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Account switching (ADR-0090, plan T3). When a run's usage limit opens, failover is on, the run kind is parked
// (control, worker, diagnostic), the run finished a turn (it has a session to resume), the limited account's reset is
// further away than usage_limit_wait_if_reset_within (an unknown reset is further away), the instance's daily switch
// budget isn't spent and the pool has an eligible account other than the run's, the report transaction arms a switch
// (state pending). At the run's next idle point (an idle, done or error status), or after switchArmMax, the run is
// parked as ADR-0079 parks it, with a limit wait due at once whose resume uses the target account (parked). The
// limit-wait job resumes it as a new run of the same instance with --resume of its session (resumed); the new run's
// first turn marks the switch succeeded, and a resume that ends before one, or a limit on the new account at once,
// marks it failed. Otherwise the run waits as ADR-0079 does, and a switch that could not go on is fell_back.
//
// Everything a switch writes goes in the transaction that decides it: the switch row, its change record, its audit
// event and its outbox event. Nothing is sent to a running process and no credential moves: the new run gets its
// account's secret through the launch path, as any run does.

// Switch states.
const (
	SwitchPending   = "pending"
	SwitchParked    = "parked"
	SwitchResumed   = "resumed"
	SwitchSucceeded = "succeeded"
	SwitchFailed    = "failed"
	SwitchFellBack  = "fell_back"
	SwitchCancelled = "cancelled"
)

// Switch causes: an automatic switch away from an account at its usage limit, and the move back to the chain's origin
// account once that account has reset (ADR-0105).
const (
	SwitchCauseUsageLimit = "usage_limit"
	SwitchCauseSwitchBack = "switch_back"
)

// Fixed reasons a switch records (never raw text). A wait's own reasons (WaitSuperseded, WaitTaskMoved, …) are used
// as they are when a switch's wait is given up.
const (
	SwitchNoEligibleAccount = "no_eligible_account"
	SwitchBudgetSpent       = "budget_spent"
	SwitchFailoverOff       = "failover_off"
	SwitchResetNear         = "reset_near"
	SwitchResumeFailed      = "resume_failed"
	SwitchLimitedAtOnce     = "limited_at_once"
	SwitchLimitCleared      = "limit_cleared"
	SwitchRunStopped        = "run_stopped"
	SwitchNoSession         = "no_session"
	SwitchNoAccount         = "no_account"
)

const (
	// switchArmMax is how long an armed switch waits for the run's idle point before the run is interrupted (§3.4).
	switchArmMax = 10 * time.Minute
	// switchBudgetWindow is the window of usage_limit_switch_max_per_day.
	switchBudgetWindow = 24 * time.Hour
	// DefaultSwitchWaitIfResetWithin and DefaultSwitchMaxPerDay are the installation defaults (ADR-0090 decision 5).
	DefaultSwitchWaitIfResetWithin = 15 * time.Minute
	DefaultSwitchMaxPerDay         = 6
)

// SwitchActor arms, parks and settles account switches.
var SwitchActor = audit.Actor{Type: audit.ActorSystem, ID: "account-switch"}

// AccountSwitch is a run's move to another account of its pool at a usage limit (ADR-0090).
type AccountSwitch struct {
	ID              string     `json:"id"`
	SpaceID         string     `json:"space_id"`
	InstanceID      string     `json:"instance_id"`
	Role            string     `json:"role"`
	RunKind         string     `json:"run_kind" enum:"control,worker,kb,dreamer,diagnostic"`
	Harness         string     `json:"harness" enum:"claude-code,codex"`
	FromRunID       string     `json:"from_run_id"`
	NewRunID        string     `json:"new_run_id,omitempty"`
	FromAccountID   string     `json:"from_account_id"`
	ToAccountID     string     `json:"to_account_id,omitempty"`
	Cause           string     `json:"cause" enum:"usage_limit,operator,switch_back" doc:"usage_limit: moved away from an account at its limit; operator: moved by a person; switch_back: moved back to the account a chain of usage-limit switches started from, after its reset (ADR-0105)"`
	OriginAccountID string     `json:"origin_account_id,omitempty" doc:"The account the chain of usage-limit switches started from; a switch_back moves the run there"`
	LimitKind       string     `json:"limit_kind" doc:"The limit that caused a usage_limit switch; empty for a switch_back"`
	LimitResetsAt   *time.Time `json:"limit_resets_at,omitempty"`
	UsageLimitID    string     `json:"usage_limit_id,omitempty"`
	LimitWaitID     string     `json:"limit_wait_id,omitempty"`
	State           string     `json:"state" enum:"pending,parked,resumed,succeeded,failed,fell_back,cancelled"`
	Reason          string     `json:"reason,omitempty"`
	ArmedUntil      *time.Time `json:"armed_until,omitempty"`
	Version         int64      `json:"version"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

func accountSwitchFromRow(r db.AccountSwitch) AccountSwitch {
	return AccountSwitch{ID: r.ID, SpaceID: r.SpaceID, InstanceID: r.InstanceID, Role: r.Role, RunKind: r.RunKind, Harness: r.Harness,
		FromRunID: r.FromRunID, NewRunID: r.NewRunID, FromAccountID: r.FromAccountID, ToAccountID: r.ToAccountID, Cause: r.Cause, OriginAccountID: r.OriginAccountID,
		LimitKind: r.LimitKind, LimitResetsAt: microsPtr(r.LimitResetsAt), UsageLimitID: r.UsageLimitID, LimitWaitID: r.LimitWaitID,
		State: r.State, Reason: r.Reason, ArmedUntil: microsPtr(r.ArmedUntil), Version: r.Version,
		CreatedAt: time.UnixMicro(r.CreatedAt).UTC(), UpdatedAt: time.UnixMicro(r.UpdatedAt).UTC()}
}

func outboxSwitch(r db.AccountSwitch) outbox.AccountSwitch {
	return outbox.AccountSwitch{SpaceID: r.SpaceID, SwitchID: r.ID, InstanceID: r.InstanceID, FromRunID: r.FromRunID, NewRunID: r.NewRunID,
		FromAccountID: r.FromAccountID, ToAccountID: r.ToAccountID, Cause: r.Cause, LimitKind: r.LimitKind, State: r.State, Reason: r.Reason}
}

func switchActive(state string) bool {
	return state == SwitchPending || state == SwitchParked || state == SwitchResumed
}

// idleStatus is a program status at which a switch may stop the run: its turn has ended.
func idleStatus(s string) bool { return s == "idle" || s == "done" || s == "error" }

// switchDecision is what the report transaction decided for a run whose limit opened: no decision (the run isn't one
// that parks, its limit isn't an account's, failover is off, or the reset is near enough to wait for: nothing is
// recorded), fall back (recorded with its reason: nothing to resume, no account to move from, no eligible account,
// budget spent), or switch to target. A limit on a run that could switch is never left with neither a decision nor a
// wait.
type switchDecision struct {
	decided bool
	target  string
	reason  string
	from    string
}

// decideSwitchTx decides whether a run at lim switches (ADR-0090 decisions 1, 2 and 5). r carries the run's latest
// turn and session.
func (svc *Service) decideSwitchTx(ctx context.Context, tx *store.Tx, r db.Run, lim limitState, ul UsageLimitSettings) (switchDecision, error) {
	if !ul.Failover || !parkKind(r.Kind) || lim.kind == "" || svc.opts.ResolveAccount == nil {
		return switchDecision{}, nil
	}
	scope, from, _, ok := limitScope(r, lim.kind)
	if ok && scope != LimitScopeAccount {
		return switchDecision{}, nil // a gateway limit: its own hold, no account to move from
	}
	// Q4: a reset within the window is waited for (ADR-0079): the run stays alive at a limit that is about to end, with
	// control told. That is the decision itself and isn't a fallback, so nothing is recorded.
	if lim.resets.Valid && time.UnixMicro(lim.resets.Int64).Sub(tx.Now) <= ul.WaitIfResetWithin {
		return switchDecision{}, nil
	}
	d := switchDecision{decided: true, from: from}
	if !ok {
		d.reason = SwitchNoAccount // the run's launch names no account: it can't be moved off one
		return d, nil
	}
	if _, ok, err := limitSwitchSession(ctx, tx.Q, r); err != nil {
		return switchDecision{}, err
	} else if !ok {
		d.reason = SwitchNoSession // nothing to resume: the run waits
		return d, nil
	}
	used, err := tx.Q.CountAccountSwitchesSince(ctx, db.CountAccountSwitchesSinceParams{SpaceID: r.SpaceID, InstanceID: r.InstanceID,
		CreatedAt: tx.Now.Add(-switchBudgetWindow).UnixMicro()})
	if err != nil {
		return d, err
	}
	if ul.SwitchMaxPerDay <= 0 || used >= int64(ul.SwitchMaxPerDay) {
		d.reason = SwitchBudgetSpent
		return d, nil
	}
	if d.target, err = svc.switchTargetTx(ctx, tx, r.SpaceID, r.Harness, r.Role, from, ""); err != nil {
		return d, err
	}
	if d.target == "" {
		d.reason = SwitchNoEligibleAccount
	}
	return d, nil
}

// switchTargetTx returns the account a switch away from from moves to: prefer when it is still eligible, otherwise the
// best eligible account of the run's pool (accounts.Resolve's failover order), or "" when none is. Eligible: bound to
// the space for the harness, enabled, in the role's pool when it has one, with no open hold that hasn't reset (any
// space, probe-due ones included), and not from.
func (svc *Service) switchTargetTx(ctx context.Context, tx *store.Tx, spaceID, harness, role, from, prefer string) (string, error) {
	now := tx.NowMicros()
	ul := UsageLimitSettings{}
	if svc.opts.UsageLimitSettings != nil {
		ul = svc.opts.UsageLimitSettings(ctx, tx.Q, spaceID)
	}
	// A switch's resume is a new start: the headroom gate removes accounts here too.
	avoid := svc.withHeadroom(ctx, tx.Q, ul, func(id string) (bool, error) {
		if id == from {
			return true, nil
		}
		return accountHeld(ctx, tx.Q, id, now)
	})
	// Resolve from the limited account: it is bound (so a space without a default still resolves) and avoided, so the
	// pool's order decides. A fresh retry's wait doesn't know it (from is empty): the role's pool decides first, and
	// only a role without a pool of its own (the space pool is every bound account, so it can't widen) falls back to
	// the preferred account as the anchor, for a space with no default. A scoped pool with no usable account is none.
	resolve := func(av func(string) (bool, error)) (string, error) {
		if from != "" {
			id, err := svc.opts.ResolveAccount(ctx, tx, spaceID, harness, role, from, av)
			if err == nil || apperr.CodeOf(err) != apperr.CodePrecondition {
				return id, err
			}
		}
		id, err := svc.opts.ResolveAccount(ctx, tx, spaceID, harness, role, "", av)
		if err == nil || apperr.CodeOf(err) != apperr.CodePrecondition || from != "" || prefer == "" {
			return id, err
		}
		scoped, serr := accounts.RoleScoped(ctx, tx, spaceID, harness, role)
		if serr != nil {
			return "", serr
		}
		if scoped {
			return "", err
		}
		return svc.opts.ResolveAccount(ctx, tx, spaceID, harness, role, prefer, av)
	}
	if prefer != "" {
		// The preferred account stays only while it is an eligible member of the run's pool: resolving with every other
		// account avoided returns it only if the pool (the role's, when it has one) still holds it. Gone, held, unbound,
		// disabled or out of the role's pool, the pool decides.
		only := func(id string) (bool, error) {
			if id != prefer {
				return true, nil
			}
			return avoid(id)
		}
		if id, err := resolve(only); err == nil && id == prefer {
			if held, err := only(id); err != nil || !held {
				return id, err
			}
		} else if err != nil && apperr.CodeOf(err) != apperr.CodePrecondition {
			return "", err
		}
	}
	id, err := resolve(avoid)
	if err != nil {
		if apperr.CodeOf(err) == apperr.CodePrecondition {
			return "", nil // a configuration that names no usable account: nothing to switch to
		}
		return "", err
	}
	if held, err := avoid(id); err != nil || held {
		return "", err
	}
	return id, nil
}

// chainOriginTx returns the account a usage-limit switch of r away from from belongs to (ADR-0105 decision 2): the
// origin of the chain when the instance's newest switch is a usage-limit switch that moved it to from, otherwise from.
func chainOriginTx(ctx context.Context, tx *store.Tx, r db.Run, from string) (string, error) {
	prev, err := tx.Q.GetNewestAccountSwitchOfInstance(ctx, db.GetNewestAccountSwitchOfInstanceParams{SpaceID: r.SpaceID, InstanceID: r.InstanceID})
	if errors.Is(err, sql.ErrNoRows) {
		return from, nil
	} else if err != nil {
		return "", err
	}
	if prev.Cause == SwitchCauseUsageLimit && prev.ToAccountID == from && prev.OriginAccountID != "" {
		return prev.OriginAccountID, nil
	}
	return from, nil
}

// insertSwitchTx writes a new switch row of cause in state with its change record, audit event, outbox event and
// metric. A usage-limit switch records the origin of its chain (ADR-0105); a switch-back's origin is its target.
func (svc *Service) insertSwitchTx(ctx context.Context, tx *store.Tx, r db.Run, lim limitState, cause, from, to, state, reason string) (db.AccountSwitch, error) {
	now := tx.NowMicros()
	sw := db.AccountSwitch{ID: ids.New(ids.AccountSwitch), SpaceID: r.SpaceID, InstanceID: r.InstanceID, Role: r.Role, RunKind: r.Kind, Harness: r.Harness,
		FromRunID: r.ID, FromAccountID: from, ToAccountID: to, Cause: cause, LimitKind: lim.kind, LimitResetsAt: lim.resets,
		LimitSince: lim.since, State: state, Reason: reason, Version: 1, CreatedAt: now, UpdatedAt: now}
	switch cause {
	case SwitchCauseUsageLimit:
		origin, err := chainOriginTx(ctx, tx, r, from)
		if err != nil {
			return sw, err
		}
		sw.OriginAccountID = origin
	case SwitchCauseSwitchBack:
		sw.OriginAccountID = to
	}
	if state == SwitchPending && cause != SwitchCauseSwitchBack {
		sw.ArmedUntil = sql.NullInt64{Int64: tx.Now.Add(switchArmMax).UnixMicro(), Valid: true}
	}
	if scope, account, gw, ok := limitScope(r, lim.kind); ok && lim.kind != "" {
		if u, err := tx.Q.GetOpenUsageLimit(ctx, db.GetOpenUsageLimitParams{SpaceID: r.SpaceID, Scope: scope, AccountID: account, GatewayRef: gw, Kind: lim.kind}); err == nil {
			sw.UsageLimitID = u.ID
		} else if !errors.Is(err, sql.ErrNoRows) {
			return sw, err
		}
	}
	if err := tx.Q.InsertAccountSwitch(ctx, db.InsertAccountSwitchParams{ID: sw.ID, SpaceID: sw.SpaceID, InstanceID: sw.InstanceID, Role: sw.Role,
		RunKind: sw.RunKind, Harness: sw.Harness, FromRunID: sw.FromRunID, FromAccountID: sw.FromAccountID, ToAccountID: sw.ToAccountID, Cause: sw.Cause,
		LimitKind: sw.LimitKind, LimitResetsAt: sw.LimitResetsAt, LimitSince: sw.LimitSince, UsageLimitID: sw.UsageLimitID, State: sw.State,
		Reason: sw.Reason, ArmedUntil: sw.ArmedUntil, CreatedAt: sw.CreatedAt, UpdatedAt: sw.UpdatedAt, OriginAccountID: sw.OriginAccountID}); err != nil {
		return sw, err
	}
	if err := svc.record(ctx, tx, sw.SpaceID, "account_switch", sw.ID, "created", 1); err != nil {
		return sw, err
	}
	if err := svc.switchEventTx(ctx, tx, sw); err != nil {
		return sw, err
	}
	if sw.ArmedUntil.Valid {
		tx.AfterCommit(svc.wakeDeadlines) // the armed switch's idle-point deadline
	}
	return sw, nil
}

// switchEventTx audits a switch's new state and publishes it: started when it is armed, completed when it ends.
// Intermediate steps (parked, resumed) are audited only.
func (svc *Service) switchEventTx(ctx context.Context, tx *store.Tx, sw db.AccountSwitch) error {
	action, result := "", metrics.SwitchResult("")
	var ev outbox.Payload
	switch sw.State {
	case SwitchPending:
		action, result, ev = "account.switch_started", metrics.SwitchStarted, outbox.AccountSwitchStarted{AccountSwitch: outboxSwitch(sw)}
	case SwitchParked:
		action = "account.switch_parked"
	case SwitchResumed:
		action = "account.switch_resumed"
	case SwitchSucceeded:
		action, result, ev = "account.switch_succeeded", metrics.SwitchSucceeded, outbox.AccountSwitchCompleted{AccountSwitch: outboxSwitch(sw)}
	case SwitchFailed:
		action, result, ev = "account.switch_failed", metrics.SwitchFailed, outbox.AccountSwitchCompleted{AccountSwitch: outboxSwitch(sw)}
	case SwitchFellBack:
		action, result, ev = "account.switch_fell_back", metrics.SwitchFellBack, outbox.AccountSwitchCompleted{AccountSwitch: outboxSwitch(sw)}
	case SwitchCancelled:
		action, result, ev = "account.switch_cancelled", metrics.SwitchCancelled, outbox.AccountSwitchCompleted{AccountSwitch: outboxSwitch(sw)}
	}
	details := map[string]any{"space_id": sw.SpaceID, "run_id": sw.FromRunID, "from_account_id": sw.FromAccountID, "to_account_id": sw.ToAccountID,
		"limit_kind": sw.LimitKind, "state": sw.State, "cause": sw.Cause}
	if sw.Reason != "" {
		details["reason"] = sw.Reason
	}
	if sw.NewRunID != "" {
		details["new_run_id"] = sw.NewRunID
	}
	if _, err := svc.audit.Append(ctx, tx, audit.Event{Actor: SwitchActor, Action: action, Target: audit.Target{Type: "run", ID: sw.FromRunID},
		Details: details}); err != nil {
		return err
	}
	if ev != nil {
		if _, err := svc.opts.Outbox.Enqueue(ctx, tx, outbox.Event{Payload: ev, Correlation: sw.FromRunID}); err != nil {
			return err
		}
	}
	if result != "" {
		cause := metrics.SwitchCause(sw.Cause)
		tx.AfterCommit(func() { svc.opts.Metrics.AccountSwitch(cause, result) })
	}
	return nil
}

// moveSwitchTx moves a switch to state (version-guarded; a switch that changed meanwhile is left alone) with its
// change record and events. It returns the row as written and whether it moved.
func (svc *Service) moveSwitchTx(ctx context.Context, tx *store.Tx, sw db.AccountSwitch, state, reason string, set func(*db.AccountSwitch)) (db.AccountSwitch, bool, error) {
	next := sw
	next.State, next.Reason = state, reason
	if set != nil {
		set(&next)
	}
	n, err := tx.Q.UpdateAccountSwitch(ctx, db.UpdateAccountSwitchParams{State: next.State, ToAccountID: next.ToAccountID, NewRunID: next.NewRunID,
		LimitWaitID: next.LimitWaitID, Reason: next.Reason, Now: tx.NowMicros(), ID: sw.ID, Version: sw.Version})
	if err != nil || n == 0 {
		return sw, false, err
	}
	next.Version, next.UpdatedAt = sw.Version+1, tx.NowMicros()
	if err := svc.record(ctx, tx, next.SpaceID, "account_switch", next.ID, "updated", next.Version); err != nil {
		return next, true, err
	}
	return next, true, svc.switchEventTx(ctx, tx, next)
}

// settleSwitchTx ends a switch (id) that is still active with state and reason; a switch that is gone or already
// ended is left alone.
func (svc *Service) settleSwitchTx(ctx context.Context, tx *store.Tx, id, state, reason string) error {
	if id == "" {
		return nil
	}
	sw, err := tx.Q.GetAccountSwitch(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	if !switchActive(sw.State) {
		return nil
	}
	_, _, err = svc.moveSwitchTx(ctx, tx, sw, state, reason, nil)
	return err
}

// switchOnLimitTx runs in the report transaction after a run's limit changed (cur -> next), before control hears about
// it. A new window on a run another switch started fails that switch (the new account was limited at once). A new
// window on a live run decides a switch (armed, or recorded as fell back). An armed switch whose limit cleared is
// cancelled, and one whose limit now resets within the wait window falls back to waiting.
func (svc *Service) switchOnLimitTx(ctx context.Context, tx *store.Tx, r db.Run, cur, next limitState, live bool) error {
	opened := next.kind != "" && (cur.kind == "" || cur.since != next.since)
	if opened {
		if sw, err := tx.Q.GetResumedAccountSwitchByNewRun(ctx, r.ID); err == nil {
			if _, _, err := svc.moveSwitchTx(ctx, tx, sw, SwitchFailed, SwitchLimitedAtOnce, nil); err != nil {
				return err
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	pending, err := tx.Q.GetPendingAccountSwitchByRun(ctx, r.ID)
	hasPending := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	ul := svc.ulSettings(ctx, tx.Q, r.SpaceID)
	switch {
	case hasPending && next.kind == "":
		_, _, err = svc.moveSwitchTx(ctx, tx, pending, SwitchCancelled, SwitchLimitCleared, nil)
		return err
	case hasPending && next.resets.Valid && time.UnixMicro(next.resets.Int64).Sub(tx.Now) <= ul.WaitIfResetWithin:
		if _, _, err = svc.moveSwitchTx(ctx, tx, pending, SwitchFellBack, SwitchResetNear, nil); err != nil {
			return err
		}
		if live {
			return svc.keptAliveNoticeTx(ctx, tx, r.ID)
		}
		return nil
	case hasPending || !opened || !live || r.Desired != "running":
		return nil
	}
	_, err = svc.armSwitchTx(ctx, tx, r, next, ul)
	return err
}

// armSwitchTx decides and records a switch for a run at lim: the armed (pending) row, a fell_back row, or nothing.
func (svc *Service) armSwitchTx(ctx context.Context, tx *store.Tx, r db.Run, lim limitState, ul UsageLimitSettings) (*db.AccountSwitch, error) {
	d, err := svc.decideSwitchTx(ctx, tx, r, lim, ul)
	if err != nil || !d.decided {
		return nil, err
	}
	// The instance may have only one active switch: an older one (a resumed switch that wasn't verified) gives way. A
	// resumed switch whose new run is limited at once was already failed by switchOnLimitTx, earlier in the same report.
	if old, err := tx.Q.GetActiveAccountSwitch(ctx, db.GetActiveAccountSwitchParams{SpaceID: r.SpaceID, InstanceID: r.InstanceID}); err == nil {
		if _, _, err := svc.moveSwitchTx(ctx, tx, old, SwitchCancelled, WaitSuperseded, nil); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if d.target == "" {
		_, err := svc.insertSwitchTx(ctx, tx, r, lim, SwitchCauseUsageLimit, d.from, "", SwitchFellBack, d.reason)
		return nil, err
	}
	sw, err := svc.insertSwitchTx(ctx, tx, r, lim, SwitchCauseUsageLimit, d.from, d.target, SwitchPending, "")
	if err != nil {
		return nil, err
	}
	return &sw, nil
}

// switchAtIdleTx parks a run whose switch is armed when the report shows it at an idle point: the run is stopped and
// its wait is due at once on the target account. It runs at the end of the report transaction, after the run's own
// change record.
func (svc *Service) switchAtIdleTx(ctx context.Context, tx *store.Tx, runID, status string) error {
	if !idleStatus(status) {
		return nil
	}
	sw, err := tx.Q.GetPendingAccountSwitchByRun(ctx, runID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	r, err := tx.Q.GetRun(ctx, runID)
	if err != nil {
		return err
	}
	if r.Desired != "running" || !Live(r.Observed) || r.LimitKind == "" {
		return nil
	}
	return svc.parkSwitchedTx(ctx, tx, r, sw, "idle")
}

// parkSwitchedTx parks a live run for its armed switch and stops it.
func (svc *Service) parkSwitchedTx(ctx context.Context, tx *store.Tx, r db.Run, sw db.AccountSwitch, why string) error {
	// Failover turned off since the switch was armed: the run stays alive at its limit as ADR-0079 keeps it.
	if !svc.ulSettings(ctx, tx.Q, r.SpaceID).Failover {
		if _, _, err := svc.moveSwitchTx(ctx, tx, sw, SwitchFellBack, SwitchFailoverOff, nil); err != nil {
			return err
		}
		return svc.keptAliveNoticeTx(ctx, tx, r.ID)
	}
	w, err := svc.parkWithTx(ctx, tx, r, storedLimit(r), why, &sw)
	if err != nil {
		return err
	}
	if w == nil {
		// The run already has a wait (or can't be parked): the switch can't go on, and must not stay armed.
		if _, _, err := svc.moveSwitchTx(ctx, tx, sw, SwitchCancelled, WaitSuperseded, nil); err != nil {
			return err
		}
		return svc.keptAliveNoticeTx(ctx, tx, r.ID)
	}
	if err := svc.stopRunTx(ctx, tx, r, "interrupt", "SpaceMan moves the run to another account of its pool at its usage limit; it continues there"); err != nil {
		return err
	}
	tx.AfterCommit(func() { svc.hub.Kick(r.NodeID) })
	return nil
}

// keptAliveNoticeTx tells control (OnRunLimitTx) about a live run's open limit after its armed switch ended without
// moving it: the "kept alive" notice was held back while the switch was armed, and the run now waits as ADR-0079
// keeps it. Control's notice is deduplicated per run and limit window, so a notice already sent isn't repeated.
func (svc *Service) keptAliveNoticeTx(ctx context.Context, tx *store.Tx, runID string) error {
	if svc.opts.OnRunLimitTx == nil {
		return nil
	}
	r, err := tx.Q.GetRun(ctx, runID)
	if err != nil {
		return err
	}
	if r.LimitKind == "" || !Live(r.Observed) || r.Desired != "running" {
		return nil
	}
	return svc.opts.OnRunLimitTx(ctx, tx, runFromRow(r), nil, runLimitOf(r))
}

// parkEndedTx parks a run that ended at its limit without central asking (ADR-0079 decision 4), switching it when its
// armed switch, or a decision now, says so. A run whose window was already decided (fell back) waits.
func (svc *Service) parkEndedTx(ctx context.Context, tx *store.Tx, r db.Run, lim limitState, why string) error {
	var sw *db.AccountSwitch
	pending, err := tx.Q.GetPendingAccountSwitchByRun(ctx, r.ID)
	switch {
	case err == nil:
		sw = &pending
	case !errors.Is(err, sql.ErrNoRows):
		return err
	default:
		latest, err := tx.Q.GetLatestAccountSwitchByRun(ctx, r.ID)
		decided := err == nil && latest.LimitSince == lim.since
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if !decided {
			if sw, err = svc.armSwitchTx(ctx, tx, r, lim, svc.ulSettings(ctx, tx.Q, r.SpaceID)); err != nil {
				return err
			}
		}
	}
	_, err = svc.parkWithTx(ctx, tx, r, lim, why, sw)
	return err
}

// switchParkedTx runs in parkWithTx when a switch's wait was written: the switch is parked and linked to its wait.
func (svc *Service) switchParkedTx(ctx context.Context, tx *store.Tx, sw db.AccountSwitch, waitID string) error {
	_, _, err := svc.moveSwitchTx(ctx, tx, sw, SwitchParked, "", func(n *db.AccountSwitch) { n.LimitWaitID = waitID })
	return err
}

// switchEndedTx runs in the report transaction of a run's end: an armed switch of the run that its end didn't park is
// cancelled (somebody stopped the run), and a switch that started the run is decided: succeeded when the run had a
// turn, failed otherwise (its --resume didn't work; ADR-0079's fresh retry follows).
func (svc *Service) switchEndedTx(ctx context.Context, tx *store.Tx, r db.Run, turn int64) error {
	if sw, err := tx.Q.GetPendingAccountSwitchByRun(ctx, r.ID); err == nil {
		if _, _, err := svc.moveSwitchTx(ctx, tx, sw, SwitchCancelled, SwitchRunStopped, nil); err != nil {
			return err
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	sw, err := tx.Q.GetResumedAccountSwitchByNewRun(ctx, r.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	switch {
	case turn > 0:
		_, _, err = svc.moveSwitchTx(ctx, tx, sw, SwitchSucceeded, "", nil)
	case r.Desired == "stopped":
		// An operator stopped the resumed run before its first turn: not a failed --resume.
		_, _, err = svc.moveSwitchTx(ctx, tx, sw, SwitchCancelled, SwitchRunStopped, nil)
	default:
		_, _, err = svc.moveSwitchTx(ctx, tx, sw, SwitchFailed, SwitchResumeFailed, nil)
	}
	return err
}

// switchTurnTx marks the switch that started a run succeeded at the run's first finished turn without a limit.
func (svc *Service) switchTurnTx(ctx context.Context, tx *store.Tx, r db.Run, turn int64, lim limitState) error {
	if r.Turn > 0 || turn == 0 || lim.kind != "" {
		return nil
	}
	sw, err := tx.Q.GetResumedAccountSwitchByNewRun(ctx, r.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	_, _, err = svc.moveSwitchTx(ctx, tx, sw, SwitchSucceeded, "", nil)
	return err
}

// parkArmedRuns parks the runs whose armed switch outlived switchArmMax without an idle point: they are interrupted,
// as the keep-alive park does.
func (svc *Service) parkArmedRuns(ctx context.Context, now time.Time) (int, error) {
	rows, err := svc.s.R().ListArmedAccountSwitchesDue(ctx, db.ListArmedAccountSwitchesDueParams{ArmedUntil: sql.NullInt64{Int64: now.UnixMicro(), Valid: true},
		Limit: limitWaitBatch})
	if err != nil || len(rows) == 0 {
		return 0, err
	}
	n := 0
	var errs []error
	for _, row := range rows {
		parked := false
		err := svc.s.W().TxNamed(ctx, "fleet.account_switch_park", func(tx *store.Tx) error {
			parked = false
			sw, err := tx.Q.GetAccountSwitch(ctx, row.ID)
			if err != nil {
				return err
			}
			if sw.State != SwitchPending || sw.Version != row.Version {
				return nil
			}
			r, err := tx.Q.GetRun(ctx, sw.FromRunID)
			if err != nil {
				return err
			}
			if r.Desired != "running" || !Live(r.Observed) || r.LimitKind == "" {
				// The run ended, was stopped or its limit cleared without the report settling the switch.
				_, _, err := svc.moveSwitchTx(ctx, tx, sw, SwitchCancelled, SwitchRunStopped, nil)
				return err
			}
			if err := svc.parkSwitchedTx(ctx, tx, r, sw, "switch_arm_max"); err != nil {
				return err
			}
			parked = true
			return nil
		})
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if parked {
			n++
		}
	}
	return n, errors.Join(errs...)
}

// switchResumeCheckTx re-resolves, in the claim transaction, the account a switch's wait resumes on: the target if it
// is still eligible, otherwise the pool's next eligible account (the wait and the switch follow it). With failover off
// or no eligible account left, the switch falls back: the wait loses its target and waits as ADR-0079 does, until
// its reset (control hears it is parked). It reports whether the wait goes on resuming now.
func (svc *Service) switchResumeCheckTx(ctx context.Context, tx *store.Tx, w *db.LimitWait) (bool, error) {
	if w.SwitchToAccountID == "" {
		return true, nil
	}
	ul := svc.ulSettings(ctx, tx.Q, w.SpaceID)
	old, err := tx.Q.GetRun(ctx, w.RunID)
	if err != nil {
		return false, err
	}
	// The account the parked run was limited on is never the target (a fresh retry's run had no limit: none is).
	from := ""
	if scope, account, _, ok := limitScope(old, old.LimitKind); ok && scope == LimitScopeAccount && old.LimitKind != "" {
		from = account
	}
	target, reason := "", SwitchFailoverOff
	if ul.Failover {
		if target, err = svc.switchTargetTx(ctx, tx, w.SpaceID, w.Harness, w.Role, from, w.SwitchToAccountID); err != nil {
			return false, err
		}
		reason = SwitchNoEligibleAccount
	}
	if target == w.SwitchToAccountID {
		return true, nil
	}
	now := tx.NowMicros()
	resumeAt := w.ResumeAt
	var exhausted *PoolExhausted
	if target == "" {
		var hold *db.UsageLimit
		if w.UsageLimitID != "" {
			if u, err := tx.Q.GetUsageLimit(ctx, w.UsageLimitID); err == nil && !u.ClearedAt.Valid {
				hold = &u
			} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return false, err
			}
		}
		at := waitResumeAt(tx.Now, storedLimit(old), hold, w.Attempts, ul.KeepAliveMax)
		// No eligible account left: the whole pool is limited, and the wait ends at its earliest reset.
		if at, exhausted, err = svc.poolWaitTx(ctx, tx, ul, w.SpaceID, w.Harness, w.Role, at); err != nil {
			return false, err
		}
		resumeAt = at.UnixMicro()
	}
	n, err := tx.Q.SetLimitWaitSwitch(ctx, db.SetLimitWaitSwitchParams{SwitchToAccountID: target, ResumeAt: resumeAt, Now: now, ID: w.ID, Version: w.Version})
	if err != nil || n == 0 {
		return false, err
	}
	w.SwitchToAccountID, w.ResumeAt, w.Version, w.UpdatedAt = target, resumeAt, w.Version+1, now
	if err := svc.record(ctx, tx, w.SpaceID, "limit_wait", w.ID, "updated", w.Version); err != nil {
		return false, err
	}
	if w.AccountSwitchID != "" {
		sw, err := tx.Q.GetAccountSwitch(ctx, w.AccountSwitchID)
		if err != nil {
			return false, err
		}
		if switchActive(sw.State) {
			if target == "" {
				_, _, err = svc.moveSwitchTx(ctx, tx, sw, SwitchFellBack, reason, nil)
			} else {
				err = svc.retargetSwitchTx(ctx, tx, sw, target)
			}
			if err != nil {
				return false, err
			}
		}
	}
	if target != "" {
		return true, nil
	}
	// A fallback keeps ADR-0079's messages: control hears the run is parked until its reset.
	if svc.opts.OnLimitWaitTx != nil && w.AccountSwitchID != "" {
		if err := svc.opts.OnLimitWaitTx(ctx, tx, LimitWaitEvent{Wait: limitWaitFromRow(*w), Change: LimitWaitParked, PoolExhausted: exhausted}); err != nil {
			return false, err
		}
	}
	tx.AfterCommit(svc.wakeDeadlines)
	return resumeAt <= now, nil
}

// retargetSwitchTx points a parked switch at another target (its first one is held or gone), audited with both.
func (svc *Service) retargetSwitchTx(ctx context.Context, tx *store.Tx, sw db.AccountSwitch, target string) error {
	n, err := tx.Q.UpdateAccountSwitch(ctx, db.UpdateAccountSwitchParams{State: sw.State, ToAccountID: target, NewRunID: sw.NewRunID,
		LimitWaitID: sw.LimitWaitID, Reason: sw.Reason, Now: tx.NowMicros(), ID: sw.ID, Version: sw.Version})
	if err != nil || n == 0 {
		return err
	}
	if err := svc.record(ctx, tx, sw.SpaceID, "account_switch", sw.ID, "updated", sw.Version+1); err != nil {
		return err
	}
	_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: SwitchActor, Action: "account.switch_retargeted", Target: audit.Target{Type: "run", ID: sw.FromRunID},
		Details: map[string]any{"space_id": sw.SpaceID, "from_account_id": sw.FromAccountID, "previous_to_account_id": sw.ToAccountID, "to_account_id": target,
			"state": sw.State}})
	return err
}

// switchResumedTx runs in a switch wait's resume transaction: the switch is resumed with its new run and the account
// the launch resolved.
func (svc *Service) switchResumedTx(ctx context.Context, tx *store.Tx, w db.LimitWait, runID string) (*AccountSwitch, error) {
	if w.AccountSwitchID == "" {
		return nil, nil
	}
	sw, err := tx.Q.GetAccountSwitch(ctx, w.AccountSwitchID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	if sw.State != SwitchParked {
		return nil, nil
	}
	nr, err := tx.Q.GetRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	to := sw.ToAccountID
	var spec LaunchSpec
	if json.Unmarshal([]byte(nr.LaunchJson), &spec) == nil && spec.AccountRef != "" {
		to = spec.AccountRef
	}
	next, ok, err := svc.moveSwitchTx(ctx, tx, sw, SwitchResumed, "", func(n *db.AccountSwitch) { n.NewRunID, n.ToAccountID = runID, to })
	if err != nil || !ok {
		return nil, err
	}
	out := accountSwitchFromRow(next)
	return &out, nil
}

// switchOfWait returns a wait's switch for an event, or nil.
func (svc *Service) switchOfWait(ctx context.Context, q *db.Queries, w db.LimitWait) (*AccountSwitch, error) {
	if w.AccountSwitchID == "" {
		return nil, nil
	}
	sw, err := q.GetAccountSwitch(ctx, w.AccountSwitchID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	out := accountSwitchFromRow(sw)
	return &out, nil
}

// AccountSwitches returns a space's account switches, newest first.
func (svc *Service) AccountSwitches(ctx context.Context, spaceID string) ([]AccountSwitch, error) {
	q := svc.s.R()
	if _, err := q.GetSpace(ctx, spaceID); errors.Is(err, sql.ErrNoRows) {
		return nil, errSpaceNotFound
	} else if err != nil {
		return nil, err
	}
	rows, err := q.ListAccountSwitchesBySpace(ctx, db.ListAccountSwitchesBySpaceParams{SpaceID: spaceID, Limit: maxLimitRows})
	if err != nil {
		return nil, err
	}
	out := make([]AccountSwitch, 0, len(rows))
	for _, r := range rows {
		out = append(out, accountSwitchFromRow(r))
	}
	return out, nil
}

// resumableSession is the session a switch resumes: the run's, once it finished a turn, or the session it was itself
// resumed with (a run moved here that hit a limit again before its first turn still has the conversation).
func resumableSession(ctx context.Context, q *db.Queries, r db.Run) (string, error) {
	if r.SessionID == "" {
		return "", nil
	}
	if r.Turn > 0 {
		return r.SessionID, nil
	}
	prev, err := q.GetLimitWaitResumedInto(ctx, db.GetLimitWaitResumedIntoParams{SpaceID: r.SpaceID, NewRunID: r.ID})
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	} else if err != nil {
		return "", err
	}
	if prev.SessionID != r.SessionID {
		return "", nil
	}
	return r.SessionID, nil
}

// limitSwitchSession is the session a usage-limit switch resumes: resumableSession's, or the run's own when it is a fresh
// run, or a run itself resumed fresh (its wait had no session), whose first turn ended at the limit. The turn count only
// moves on a finished turn, and a turn that ends in the limit error doesn't count, but the harness took the run's prompt,
// so the session's transcript exists and resumes (a resume that still fails is retried fresh, ADR-0079). ok is false
// when there is nothing to switch with: no session, a resumed run whose session changed (the resume didn't take), or the
// fresh retry of a failed resume (it already had its one retry). A fresh-resumed run with no session of its own is ok
// with an empty session: the switch relaunches it fresh on the target account.
//
// The fallback is asymmetric on purpose. An original fresh run with no session never started a session, so there is
// nothing to resume and it falls back no_session. A run resumed fresh from a wait with no session had its prompt taken
// on an earlier run, so its first turn ended at the limit with the work already begun: it is relaunched fresh instead.
func limitSwitchSession(ctx context.Context, q *db.Queries, r db.Run) (session string, ok bool, err error) {
	if session, err = resumableSession(ctx, q, r); err != nil || session != "" {
		return session, err == nil, err
	}
	if r.Turn > 0 {
		return "", false, nil
	}
	prev, err := q.GetLimitWaitResumedInto(ctx, db.GetLimitWaitResumedIntoParams{SpaceID: r.SpaceID, NewRunID: r.ID})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return r.SessionID, r.SessionID != "", nil
	case err != nil:
		return "", false, err
	case prev.SessionID != "":
		return "", false, nil // a resumed run whose session changed: the resume didn't take
	}
	// prev started this run fresh. If prev's own run was a resume of a session, this is the fresh retry of that failed
	// resume: it stops here, so a switch can't loop. The retry is detected by its shape (a fresh start whose wait's run
	// was itself resumed with a session), not by a flag. A false positive, a run that only looks like a retry, stops a
	// switch it could have made; that is the safe side, since the run waits and nothing loops.
	if first, err := q.GetLimitWaitResumedInto(ctx, db.GetLimitWaitResumedIntoParams{SpaceID: r.SpaceID, NewRunID: prev.RunID}); err == nil && first.SessionID != "" {
		return "", false, nil
	} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", false, err
	}
	return r.SessionID, true, nil
}

// armedSwitch reports whether a run has an armed switch waiting for its idle point.
func (svc *Service) armedSwitch(ctx context.Context, q *db.Queries, runID string) (bool, error) {
	_, err := q.GetPendingAccountSwitchByRun(ctx, runID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}
