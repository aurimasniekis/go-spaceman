package fleet

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Account holds at launch (ADR-0079 decision 7). A launch or resume whose resolved account (or gateway reference) has
// an open usage-limit hold in any space is refused in the launch transaction, which only reads, so a refusal writes
// nothing: no change-feed record, no event. A hold whose reset has passed no longer refuses (the deadline job clears
// it). A hold that is due for its probe (check_at) lets exactly one launch through: the launch claims the probe in its
// own transaction by moving check_at on, so a second launch is refused until the next probe time.

const (
	// DefaultLimitKeepAliveMax caps the probe backoff when the space setting is not known (ADR-0079 Q3: 6 h).
	DefaultLimitKeepAliveMax = 6 * time.Hour
	// holdTimeLayout is how a refusal names a time: UTC, to the minute.
	holdTimeLayout = "2006-01-02 15:04 UTC"
)

// UsageLimitSettings are a space's usage-limit settings the launch reads (ADR-0079 decision 12).
type UsageLimitSettings struct {
	// Failover lets Resolve pick another bound account of the same harness without an open hold (default off, Q1).
	Failover bool
	// KeepAliveMax caps the backoff between probes of a hold whose reset is unknown.
	KeepAliveMax time.Duration
}

// HoldTarget is what a launch would use that a usage-limit hold can hold: an account, or the gateway references of a
// gateway run (the connection, and the project for its budget).
type HoldTarget struct {
	AccountID   string
	GatewayRefs []string
}

func (h HoldTarget) empty() bool { return h.AccountID == "" && len(h.GatewayRefs) == 0 }

func holdTarget(account string, gw *GatewaySpec) HoldTarget {
	if gw != nil && gw.Kind != "" {
		refs := []string{gatewayRefHP}
		if gw.ProjectID != "" {
			refs = append(refs, gatewayRefHPProject+gw.ProjectID)
		}
		return HoldTarget{GatewayRefs: refs}
	}
	return HoldTarget{AccountID: account}
}

// HoldError refuses a launch on a held account (CodeRateLimited). Until is when a launch can next get through: the
// reset, or the next probe when the reset is unknown (Known false).
type HoldError struct {
	Target HoldTarget
	Until  time.Time
	Known  bool
	err    *apperr.Error
}

func (e *HoldError) Error() string { return e.err.Error() }

// Unwrap exposes the apperr.Error, so the API maps it to 429 with its text.
func (e *HoldError) Unwrap() error { return e.err }

// AsHold returns the hold that refused a launch, if one did.
func AsHold(err error) (*HoldError, bool) {
	var h *HoldError
	ok := errors.As(err, &h)
	return h, ok
}

// openHolds returns the open holds on what a launch would use, in every space (Q8).
func openHolds(ctx context.Context, q *db.Queries, t HoldTarget) ([]db.UsageLimit, error) {
	if t.AccountID != "" {
		return q.ListOpenUsageLimitsByAccount(ctx, t.AccountID)
	}
	var out []db.UsageLimit
	for _, ref := range t.GatewayRefs {
		rs, err := q.ListOpenUsageLimitsByGateway(ctx, ref)
		if err != nil {
			return nil, err
		}
		out = append(out, rs...)
	}
	return out, nil
}

// holdState sorts open holds at now: those that still refuse a launch, and those due for their probe. A hold whose
// reset has passed is neither. Any kind holds the whole account, seven_day_opus and seven_day_sonnet included (ADR-0079
// decision 7): a launch can't promise which model it will use.
func holdState(rows []db.UsageLimit, now int64) (blocking, probes []db.UsageLimit) {
	for _, u := range rows {
		switch {
		case u.ResetsAt.Valid && u.ResetsAt.Int64 <= now:
		case u.CheckAt.Valid && u.CheckAt.Int64 <= now:
			probes = append(probes, u)
		default:
			blocking = append(blocking, u)
		}
	}
	return blocking, probes
}

// freeAt is when a hold stops refusing launches: its next probe if that comes first, otherwise its reset. known is
// false when it waits for a probe (the reset is unknown, or a probe was asked for before it).
func freeAt(u db.UsageLimit) (at int64, known bool) {
	switch {
	case u.CheckAt.Valid && (!u.ResetsAt.Valid || u.CheckAt.Int64 < u.ResetsAt.Int64):
		return u.CheckAt.Int64, false
	case u.ResetsAt.Valid:
		return u.ResetsAt.Int64, true
	}
	return 0, false
}

// holdError builds the refusal for the holds that still refuse: the one that ends last decides the time.
func holdError(ctx context.Context, q *db.Queries, t HoldTarget, blocking []db.UsageLimit) *HoldError {
	var last db.UsageLimit
	var at int64
	known := true
	for _, u := range blocking {
		a, k := freeAt(u)
		// No reset and no probe time (a = 0) lets nothing through but a clear, so it ends last of all.
		if a == 0 && !k {
			last, at, known = u, 0, false
			break
		}
		if at == 0 || a > at || (a == at && !k) {
			last, at, known = u, a, k
		}
	}
	what := holdSubject(ctx, q, t, last)
	until := time.UnixMicro(at).UTC()
	var msg string
	switch {
	case at == 0:
		msg = what + " (reset time unknown); launches on it are refused until the hold is cleared"
	case known:
		msg = fmt.Sprintf("%s until %s; launches on it are refused until then", what, until.Format(holdTimeLayout))
	default:
		msg = fmt.Sprintf("%s (reset time unknown, re-checking at %s); launches on it are refused until then", what, until.Format(holdTimeLayout))
	}
	if at == 0 {
		until = time.Time{}
	}
	return &HoldError{Target: t, Until: until, Known: known && at != 0, err: apperr.New(apperr.CodeRateLimited, "%s", msg)}
}

// holdSubject names what is held, never another space.
func holdSubject(ctx context.Context, q *db.Queries, t HoldTarget, u db.UsageLimit) string {
	if u.Scope == LimitScopeGateway {
		if u.Kind == proto.LimitGatewayBudget {
			return "the space's Harbor Pilot project budget is exhausted"
		}
		return "the Harbor Pilot gateway has no account available (quota exhausted)"
	}
	name := t.AccountID
	if a, err := q.GetAccount(ctx, t.AccountID); err == nil {
		name = a.Name
	}
	return fmt.Sprintf("account %q is at its %s usage limit", name, limitKindLabel(u.Kind))
}

func limitKindLabel(kind string) string {
	switch kind {
	case proto.LimitFiveHour:
		return "5-hour"
	case proto.LimitSevenDay:
		return "weekly"
	case proto.LimitSevenDayOpus:
		return "weekly Opus"
	case proto.LimitSevenDaySonnet:
		return "weekly Sonnet"
	case proto.LimitSevenDayOverage:
		return "weekly overage"
	case proto.LimitOverage:
		return "overage"
	}
	return "a"
}

// probeBackoff is the time to the next probe of a hold whose reset is unknown, after a probe at now: it doubles from
// limitProbeFirst (the time since the hold was seen, which the first probe makes 1 h) and is capped at keepAliveMax.
func probeBackoff(u db.UsageLimit, now int64, keepAliveMax time.Duration) time.Duration {
	if keepAliveMax <= 0 {
		keepAliveMax = DefaultLimitKeepAliveMax
	}
	d := time.Duration(now-u.SeenAt) * time.Microsecond
	return min(max(d, limitProbeFirst), max(keepAliveMax, limitProbeFirst))
}

// holdCheckTx refuses a launch on a held account (or gateway reference) in the launch transaction, or lets it through
// as the probe of the holds that are due. A refusal writes nothing. A probe moves each due hold's check_at on: with
// backoff while its reset is unknown, otherwise to none (one probe decides, then the reset does). The probes reports
// whether this launch is one.
func (svc *Service) holdCheckTx(ctx context.Context, tx *store.Tx, t HoldTarget, keepAliveMax time.Duration) (probe bool, err error) {
	if t.empty() {
		return false, nil
	}
	rows, err := openHolds(ctx, tx.Q, t)
	if err != nil {
		return false, err
	}
	now := tx.NowMicros()
	blocking, probes := holdState(rows, now)
	if len(blocking) > 0 {
		return false, holdError(ctx, tx.Q, t, blocking)
	}
	for _, u := range probes {
		next := sql.NullInt64{}
		if !u.ResetsAt.Valid {
			next = sql.NullInt64{Int64: tx.Now.Add(probeBackoff(u, now, keepAliveMax)).UnixMicro(), Valid: true}
		}
		n, err := tx.Q.SetUsageLimitCheck(ctx, db.SetUsageLimitCheckParams{CheckAt: next, Now: now, ID: u.ID, Version: u.Version})
		if err != nil {
			return false, err
		}
		if n == 0 {
			// Someone else claimed it first: this launch is not the probe.
			return false, holdError(ctx, tx.Q, t, []db.UsageLimit{u})
		}
		if err := svc.record(ctx, tx, u.SpaceID, "usage_limit", u.ID, "updated", u.Version+1); err != nil {
			return false, err
		}
	}
	return len(probes) > 0, nil
}

// accountHeld reports, for failover, whether an account has an open hold that hasn't reset: a launch on it would be
// refused or would be a probe, so another account without one is better.
func accountHeld(ctx context.Context, q *db.Queries, accountID string, now int64) (bool, error) {
	rows, err := q.ListOpenUsageLimitsByAccount(ctx, accountID)
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(rows, func(u db.UsageLimit) bool { return !u.ResetsAt.Valid || u.ResetsAt.Int64 > now }), nil
}

// HoldOpen reports, outside any transaction, whether a launch on t would still be refused, and until when. Control
// autostart uses it to stay deferred without trying a launch every sweep.
func (svc *Service) HoldOpen(ctx context.Context, t HoldTarget) (*HoldError, error) {
	if t.empty() {
		return nil, nil
	}
	q := svc.s.R()
	rows, err := openHolds(ctx, q, t)
	if err != nil {
		return nil, err
	}
	blocking, _ := holdState(rows, svc.opts.Clock.Now().UnixMicro())
	if len(blocking) == 0 {
		return nil, nil
	}
	return holdError(ctx, q, t, blocking), nil
}

// ClearedHold is a hold an operator cleared, as the space's control is told about it.
type ClearedHold struct {
	SpaceID     string
	LimitID     string
	Scope       string
	AccountID   string
	AccountName string
	GatewayRef  string
	Kind        string
}

// UsageLimitClear is the result of an operator's "clear now".
type UsageLimitClear struct {
	Cleared []UsageLimit `json:"cleared" doc:"The holds this request cleared"`
	// HeldElsewhere is set when other spaces still hold the account (or gateway reference): a space-level clear lifts
	// only this space's row, and a launch here is still refused while another space's hold is open.
	HeldElsewhere bool   `json:"held_elsewhere" doc:"Other spaces still hold the account or gateway reference, so launches here may still be refused"`
	Note          string `json:"note,omitempty"`
}

const heldElsewhereNote = "Only this space's hold was cleared. Other spaces still hold the account, and a launch here is refused while their hold is open; clearing every space's hold needs account permission (account:write)."

// ClearSpaceUsageLimit clears one of a space's own holds (an operator's "clear now", ADR-0079 decision 7). It never
// touches another space's row, so the account may still be held elsewhere; the result says so.
func (svc *Service) ClearSpaceUsageLimit(ctx context.Context, actor audit.Actor, spaceID, limitID string) (*UsageLimitClear, error) {
	out := &UsageLimitClear{Cleared: []UsageLimit{}}
	var cleared []ClearedHold
	err := svc.s.W().TxNamed(ctx, "fleet.usage_limit_clear", func(tx *store.Tx) error {
		out.Cleared, out.HeldElsewhere, cleared = out.Cleared[:0], false, nil
		u, err := tx.Q.GetUsageLimit(ctx, limitID)
		if errors.Is(err, sql.ErrNoRows) || err == nil && u.SpaceID != spaceID {
			return apperr.New(apperr.CodeNotFound, "usage-limit hold not found in this space")
		} else if err != nil {
			return err
		}
		if u.ClearedAt.Valid {
			return apperr.New(apperr.CodePrecondition, "the hold is already cleared")
		}
		by := actorRef(actor)
		if err := svc.clearHoldTx(ctx, tx, u, by, u.ID); err != nil {
			return err
		}
		t := HoldTarget{AccountID: u.AccountID}
		if u.Scope == LimitScopeGateway {
			t = HoldTarget{GatewayRefs: []string{u.GatewayRef}}
		}
		rest, err := openHolds(ctx, tx.Q, t)
		if err != nil {
			return err
		}
		now := tx.NowMicros()
		blocking, probes := holdState(rest, now)
		out.HeldElsewhere = len(blocking)+len(probes) > 0
		u.ClearedAt, u.ClearedBy, u.Version = sql.NullInt64{Int64: now, Valid: true}, by, u.Version+1
		out.Cleared = append(out.Cleared, usageLimitFromRow(u))
		cleared = append(cleared, svc.clearedHold(ctx, tx.Q, u))
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "usage_limit.cleared", Target: audit.Target{Type: "usage_limit", ID: u.ID},
			Details: map[string]any{"space_id": spaceID, "scope": u.Scope, "account_id": u.AccountID, "gateway_ref": u.GatewayRef, "kind": u.Kind,
				"held_elsewhere": out.HeldElsewhere}})
		return err
	})
	if err != nil {
		return nil, err
	}
	if out.HeldElsewhere {
		out.Note = heldElsewhereNote
	}
	svc.holdsCleared(cleared)
	return out, nil
}

// ClearAccountUsageLimits clears every space's open holds on an account (of one kind, or every kind when kind is
// empty): accounts are global objects, so this needs account permission (ADR-0079 decision 7). Every space whose
// row was cleared has its control told once.
func (svc *Service) ClearAccountUsageLimits(ctx context.Context, actor audit.Actor, accountID, kind string) (*UsageLimitClear, error) {
	if kind != "" && (proto.LimitKind(kind) != kind || kind == proto.LimitGatewayQuota || kind == proto.LimitGatewayBudget) {
		return nil, apperr.New(apperr.CodeInvalid, "kind: must be an account usage-limit kind")
	}
	out := &UsageLimitClear{Cleared: []UsageLimit{}}
	var cleared []ClearedHold
	err := svc.s.W().TxNamed(ctx, "fleet.usage_limit_clear_account", func(tx *store.Tx) error {
		out.Cleared, cleared = out.Cleared[:0], nil
		if _, err := tx.Q.GetAccount(ctx, accountID); errors.Is(err, sql.ErrNoRows) {
			return errAccountNotFound
		} else if err != nil {
			return err
		}
		rows, err := tx.Q.ListOpenUsageLimitsByAccount(ctx, accountID)
		if err != nil {
			return err
		}
		by := actorRef(actor)
		now := tx.NowMicros()
		var limitIDs []string
		for _, u := range rows {
			if kind != "" && u.Kind != kind {
				continue
			}
			if err := svc.clearHoldTx(ctx, tx, u, by, u.ID); err != nil {
				return err
			}
			u.ClearedAt, u.ClearedBy, u.Version = sql.NullInt64{Int64: now, Valid: true}, by, u.Version+1
			out.Cleared = append(out.Cleared, usageLimitFromRow(u))
			cleared = append(cleared, svc.clearedHold(ctx, tx.Q, u))
			limitIDs = append(limitIDs, u.ID)
		}
		if len(limitIDs) == 0 {
			return nil // nothing was open: nothing to audit
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "account.usage_limits_cleared", Target: audit.Target{Type: "account", ID: accountID},
			Details: map[string]any{"kind": kind, "usage_limits": limitIDs}})
		return err
	})
	if err != nil {
		return nil, err
	}
	svc.holdsCleared(cleared)
	return out, nil
}

func (svc *Service) clearedHold(ctx context.Context, q *db.Queries, u db.UsageLimit) ClearedHold {
	h := ClearedHold{SpaceID: u.SpaceID, LimitID: u.ID, Scope: u.Scope, AccountID: u.AccountID, GatewayRef: u.GatewayRef, Kind: u.Kind}
	if u.AccountID != "" {
		if a, err := q.GetAccount(ctx, u.AccountID); err == nil {
			h.AccountName = a.Name
		}
	}
	return h
}

func (svc *Service) holdsCleared(hs []ClearedHold) {
	if len(hs) > 0 && svc.opts.OnHoldsCleared != nil {
		svc.opts.OnHoldsCleared(hs)
	}
}
