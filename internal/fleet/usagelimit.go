package fleet

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/outbox"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// EndUsageLimit is the end reason of a run that ended while it was at a usage limit (ADR-0079): central records it in
// place of the node's reason unless central itself had asked the run to stop.
const EndUsageLimit = "usage_limit"

// Usage-limit hold scopes and the gateway references (ADR-0079 decision 7). A Harbor Pilot quota exhaustion is
// connection-wide (no account behind the one connection is eligible), so it holds every routed space; a blocking budget
// belongs to one Harbor Pilot project and holds only the spaces charged to it.
const (
	LimitScopeAccount = "account"
	LimitScopeGateway = "gateway"
	// gatewayRefHP is the gateway reference of the one Harbor Pilot connection; gatewayRefHPProject+<project id> is a
	// project's budget.
	gatewayRefHP        = GatewayKindHarborPilot
	gatewayRefHPProject = GatewayKindHarborPilot + ":project:"
	// limitProbeFirst is when a hold whose reset is unknown is first probed (ADR-0079 decisions 5 and 7).
	limitProbeFirst = time.Hour
	// maxLimitRows bounds a usage-limit or limit-wait listing.
	maxLimitRows = 200
)

// RunLimit is a run's open usage limit as its node reported it (minor ≥ 12, ADR-0079).
type RunLimit struct {
	Kind     string     `json:"kind" enum:"five_hour,seven_day,seven_day_opus,seven_day_sonnet,seven_day_overage_included,overage,gateway_quota,gateway_budget,unknown"`
	ResetsAt *time.Time `json:"resets_at,omitempty" doc:"When the limit resets; absent when unknown (Claude Code's wrap-up path and gateway limits carry none)"`
	Since    time.Time  `json:"since" doc:"When the node first saw the limit"`
}

// UsageLimit is a hold on an account (or gateway reference) that one space's run reported (ADR-0079). A hold applies to
// every space using the account: a space's listing also shows the open holds other spaces opened on its accounts,
// marked shared and without that space's identifiers.
type UsageLimit struct {
	ID         string     `json:"id,omitempty" doc:"Absent on a shared hold"`
	SpaceID    string     `json:"space_id,omitempty" doc:"The space whose run opened it; absent on a shared hold"`
	Shared     bool       `json:"shared,omitempty" doc:"Opened by another space's run on an account or gateway this space uses; it holds this space too"`
	Scope      string     `json:"scope" enum:"account,gateway"`
	AccountID  string     `json:"account_id,omitempty"`
	GatewayRef string     `json:"gateway_ref,omitempty" doc:"harbor-pilot (a quota exhaustion: the whole connection) or harbor-pilot:project:<id> (a project's budget)"`
	Kind       string     `json:"kind" enum:"five_hour,seven_day,seven_day_opus,seven_day_sonnet,seven_day_overage_included,overage,gateway_quota,gateway_budget,unknown"`
	ResetsAt   *time.Time `json:"resets_at,omitempty" doc:"Absent when unknown"`
	CheckAt    *time.Time `json:"check_at,omitempty" doc:"When a hold with an unknown reset is next probed"`
	Source     string     `json:"source" enum:"transcript,quota_board,budget_status,operator"`
	FirstRunID string     `json:"first_run_id,omitempty"`
	SeenAt     time.Time  `json:"seen_at"`
	ClearedAt  *time.Time `json:"cleared_at,omitempty"`
	ClearedBy  string     `json:"cleared_by,omitempty"`
	Version    int64      `json:"version,omitempty"`
}

func usageLimitFromRow(r db.UsageLimit) UsageLimit {
	return UsageLimit{ID: r.ID, SpaceID: r.SpaceID, Scope: r.Scope, AccountID: r.AccountID, GatewayRef: r.GatewayRef, Kind: r.Kind,
		ResetsAt: microsPtr(r.ResetsAt), CheckAt: microsPtr(r.CheckAt), Source: r.Source, FirstRunID: r.FirstRunID,
		SeenAt: time.UnixMicro(r.SeenAt).UTC(), ClearedAt: microsPtr(r.ClearedAt), ClearedBy: r.ClearedBy, Version: r.Version}
}

// shared is how another space's open hold appears in this space: without the other space's identifiers.
func (u UsageLimit) shared() UsageLimit {
	u.ID, u.SpaceID, u.FirstRunID, u.ClearedBy, u.Version, u.Shared = "", "", "", "", 0, true
	return u
}

// LimitWait is a parked run that should come back as a new run of the same instance (ADR-0079 decision 4). The launch
// input it resumes with is not part of it.
type LimitWait struct {
	ID           string     `json:"id"`
	SpaceID      string     `json:"space_id"`
	RunID        string     `json:"run_id"`
	TaskID       string     `json:"task_id,omitempty"`
	RunKind      string     `json:"run_kind" enum:"control,worker,kb,dreamer,diagnostic"`
	Role         string     `json:"role"`
	NodeID       string     `json:"node_id"`
	InstanceID   string     `json:"instance_id"`
	Harness      string     `json:"harness" enum:"claude-code,codex"`
	UsageLimitID string     `json:"usage_limit_id,omitempty"`
	ResumeAt     time.Time  `json:"resume_at"`
	ReleaseAt    *time.Time `json:"release_at,omitempty" doc:"When a worker's task is given back to the queue; absent for control"`
	State        string     `json:"state" enum:"waiting,resuming,resumed,released,cancelled,failed"`
	NewRunID     string     `json:"new_run_id,omitempty"`
	Attempts     int64      `json:"attempts"`
	LastError    string     `json:"last_error,omitempty"`
	Version      int64      `json:"version"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

func limitWaitFromRow(r db.LimitWait) LimitWait {
	return LimitWait{ID: r.ID, SpaceID: r.SpaceID, RunID: r.RunID, TaskID: r.TaskID, RunKind: r.RunKind, Role: r.Role, NodeID: r.NodeID,
		InstanceID: r.InstanceID, Harness: r.Harness, UsageLimitID: r.UsageLimitID, ResumeAt: time.UnixMicro(r.ResumeAt).UTC(),
		ReleaseAt: microsPtr(r.ReleaseAt), State: r.State, NewRunID: r.NewRunID, Attempts: r.Attempts, LastError: r.LastError, Version: r.Version,
		CreatedAt: time.UnixMicro(r.CreatedAt).UTC(), UpdatedAt: time.UnixMicro(r.UpdatedAt).UTC()}
}

func runLimitOf(r db.Run) *RunLimit {
	if r.LimitKind == "" {
		return nil
	}
	out := &RunLimit{Kind: r.LimitKind, ResetsAt: microsPtr(r.LimitResetsAt)}
	if r.LimitSince.Valid {
		out.Since = time.UnixMicro(r.LimitSince.Int64).UTC()
	}
	return out
}

// limitState is a run's stored limit: kind ” is none (both times NULL).
type limitState struct {
	kind          string
	resets, since sql.NullInt64
}

func storedLimit(r db.Run) limitState {
	return limitState{kind: r.LimitKind, resets: r.LimitResetsAt, since: r.LimitSince}
}

// reportedLimit is the limit a report asks central to store, and false when it carries no limit information (nil, or
// not a limit at all), so the stored one stays. A repeat of the stored limit (the same since) never loses what is
// stored: a missing reset keeps the stored reset, and an unknown kind keeps the stored kind (review follow-up). A
// cleared report clears only the limit it names by its since: the node repeats a clear on every later report, so a
// late one for an older limit must never clear a newer open one.
func reportedLimit(cur limitState, rep proto.RunReport, now time.Time) (limitState, bool) {
	if rep.Limit == nil {
		return limitState{}, false
	}
	l, ok := rep.Limit.Normalized(now)
	if !ok {
		return limitState{}, false
	}
	if l.Cleared {
		if cur.kind != "" && cur.since.Valid && cur.since.Int64 != l.Since.UnixMicro() && cur.since.Int64 != rep.Limit.Since.UnixMicro() {
			return limitState{}, false
		}
		return limitState{}, true
	}
	next := limitState{kind: l.Kind, since: sql.NullInt64{Int64: l.Since.UnixMicro(), Valid: true}}
	if l.ResetsAt != nil {
		next.resets = sql.NullInt64{Int64: l.ResetsAt.UnixMicro(), Valid: true}
	}
	if cur.kind != "" && cur.since == next.since {
		if !next.resets.Valid {
			next.resets = cur.resets
		}
		if next.kind == proto.LimitUnknown {
			next.kind = cur.kind
		}
	}
	return next, true
}

// limitScope is the hold a run's limit places: its resolved account, or for a gateway run the gateway reference. ok is
// false for a run with neither (nothing to hold).
func limitScope(r db.Run, kind string) (scope, account, gatewayRef string, ok bool) {
	var spec LaunchSpec
	if err := json.Unmarshal([]byte(r.LaunchJson), &spec); err != nil {
		return "", "", "", false
	}
	switch {
	case spec.Gateway != nil && spec.Gateway.Kind != "":
		if kind == proto.LimitGatewayBudget && spec.Gateway.ProjectID != "" {
			return LimitScopeGateway, "", gatewayRefHPProject + spec.Gateway.ProjectID, true
		}
		return LimitScopeGateway, "", gatewayRefHP, true
	case spec.AccountRef != "":
		return LimitScopeAccount, spec.AccountRef, "", true
	}
	return "", "", "", false
}

func outboxUsageLimit(r db.UsageLimit) outbox.UsageLimit {
	return outbox.UsageLimit{SpaceID: r.SpaceID, LimitID: r.ID, Scope: r.Scope, AccountID: r.AccountID, GatewayRef: r.GatewayRef, Kind: r.Kind,
		ResetsAt: microsPtr(r.ResetsAt), RunID: r.FirstRunID}
}

// openHoldTx opens the hold a run's new limit places in the run's space, or, when one is already open for the same
// account (or gateway reference) and kind, moves its reset to a later known one. A new hold publishes one
// usage_limit.reached event; a moved reset publishes none. A hold with an unknown reset gets its first probe time.
func (svc *Service) openHoldTx(ctx context.Context, tx *store.Tx, r db.Run, lim limitState) error {
	scope, account, gw, ok := limitScope(r, lim.kind)
	if !ok {
		return nil
	}
	now := tx.NowMicros()
	cur, err := tx.Q.GetOpenUsageLimit(ctx, db.GetOpenUsageLimitParams{SpaceID: r.SpaceID, Scope: scope, AccountID: account, GatewayRef: gw, Kind: lim.kind})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		row := db.UsageLimit{ID: ids.New(ids.UsageLimit), SpaceID: r.SpaceID, Scope: scope, AccountID: account, GatewayRef: gw, Kind: lim.kind,
			ResetsAt: lim.resets, Source: "transcript", FirstRunID: r.ID, SeenAt: lim.since.Int64, Version: 1, CreatedAt: now, UpdatedAt: now}
		if !lim.resets.Valid {
			row.CheckAt = sql.NullInt64{Int64: tx.Now.Add(limitProbeFirst).UnixMicro(), Valid: true}
		}
		if !lim.since.Valid {
			row.SeenAt = now
		}
		if err := tx.Q.InsertUsageLimit(ctx, db.InsertUsageLimitParams{ID: row.ID, SpaceID: row.SpaceID, Scope: row.Scope, AccountID: row.AccountID,
			GatewayRef: row.GatewayRef, Kind: row.Kind, ResetsAt: row.ResetsAt, CheckAt: row.CheckAt, Source: row.Source, FirstRunID: row.FirstRunID,
			SeenAt: row.SeenAt, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}); err != nil {
			return err
		}
		if _, err := svc.opts.Outbox.Enqueue(ctx, tx, outbox.Event{Payload: outbox.UsageLimitReached{UsageLimit: outboxUsageLimit(row)}, Correlation: r.ID}); err != nil {
			return err
		}
		return svc.record(ctx, tx, row.SpaceID, "usage_limit", row.ID, "created", row.Version)
	case err != nil:
		return err
	}
	if !lim.resets.Valid || (cur.ResetsAt.Valid && cur.ResetsAt.Int64 >= lim.resets.Int64) {
		return nil
	}
	if n, err := tx.Q.SetUsageLimitReset(ctx, db.SetUsageLimitResetParams{ResetsAt: lim.resets, Now: now, ID: cur.ID, Version: cur.Version}); err != nil || n == 0 {
		return err
	}
	version := cur.Version + 1
	// A known reset needs no probe.
	if cur.CheckAt.Valid {
		n, err := tx.Q.SetUsageLimitCheck(ctx, db.SetUsageLimitCheckParams{CheckAt: sql.NullInt64{}, Now: now, ID: cur.ID, Version: version})
		if err != nil {
			return err
		}
		version += n
	}
	return svc.record(ctx, tx, cur.SpaceID, "usage_limit", cur.ID, "updated", version)
}

// clearHoldsTx clears the holds of the cleared limit's kind that the run's account (or gateway reference) has in every
// space: the run's node reported the limit over, so the account works again wherever it is used (ADR-0079 decision 7).
// Only holds first seen no later than the cleared limit are cleared: a clear that reaches central late (on a hello
// after a disconnect) is no evidence about a window seen after it. Each cleared hold publishes one usage_limit.cleared
// event in its own space.
func (svc *Service) clearHoldsTx(ctx context.Context, tx *store.Tx, r db.Run, cleared limitState) error {
	kind := cleared.kind
	scope, account, gw, ok := limitScope(r, kind)
	if !ok {
		return nil
	}
	var rows []db.UsageLimit
	var err error
	if scope == LimitScopeAccount {
		rows, err = tx.Q.ListOpenUsageLimitsByAccount(ctx, account)
	} else {
		rows, err = tx.Q.ListOpenUsageLimitsByGateway(ctx, gw)
	}
	if err != nil {
		return err
	}
	by := "run:" + r.ID
	for _, u := range rows {
		if u.Kind != kind || cleared.since.Valid && u.SeenAt > cleared.since.Int64 {
			continue
		}
		if err := svc.clearHoldTx(ctx, tx, u, by, r.ID); err != nil {
			return err
		}
	}
	return nil
}

// clearHoldTx clears one open hold, publishing its usage_limit.cleared event.
func (svc *Service) clearHoldTx(ctx context.Context, tx *store.Tx, u db.UsageLimit, by, correlation string) error {
	n, err := tx.Q.ClearUsageLimit(ctx, db.ClearUsageLimitParams{Now: tx.NowMicros(), ClearedBy: by, ID: u.ID})
	if err != nil || n == 0 {
		return err
	}
	if _, err := svc.opts.Outbox.Enqueue(ctx, tx, outbox.Event{Payload: outbox.UsageLimitCleared{UsageLimit: outboxUsageLimit(u), ClearedBy: by},
		Correlation: correlation}); err != nil {
		return err
	}
	return svc.record(ctx, tx, u.SpaceID, "usage_limit", u.ID, "updated", u.Version+1)
}

// applyLimitTx stores a run's new limit (fenced by generation; a repeat writes nothing) and opens or clears the holds
// it places. It returns how many versions the run row moved.
func (svc *Service) applyLimitTx(ctx context.Context, tx *store.Tx, r db.Run, cur, next limitState) (int64, error) {
	n, err := tx.Q.SetRunLimit(ctx, db.SetRunLimitParams{LimitKind: next.kind, LimitResetsAt: next.resets, LimitSince: next.since, Now: tx.NowMicros(),
		ID: r.ID, Generation: r.Generation})
	if err != nil || n == 0 {
		return 0, err
	}
	switch {
	case next.kind != "":
		// The same limit reclassified (the same since, a newer record with another kind, such as the wrap-up path's
		// unknown followed by a typed record): the hold this run opened for the old kind is superseded.
		if cur.kind != "" && cur.kind != next.kind && cur.since == next.since {
			if err := svc.supersedeHoldTx(ctx, tx, r, cur.kind); err != nil {
				return 0, err
			}
		}
		err = svc.openHoldTx(ctx, tx, r, next)
	case cur.kind != "":
		err = svc.clearHoldsTx(ctx, tx, r, cur)
	}
	return n, err
}

// supersedeHoldTx clears the open hold of kind in the run's space if this run opened it.
func (svc *Service) supersedeHoldTx(ctx context.Context, tx *store.Tx, r db.Run, kind string) error {
	scope, account, gw, ok := limitScope(r, kind)
	if !ok {
		return nil
	}
	u, err := tx.Q.GetOpenUsageLimit(ctx, db.GetOpenUsageLimitParams{SpaceID: r.SpaceID, Scope: scope, AccountID: account, GatewayRef: gw, Kind: kind})
	if errors.Is(err, sql.ErrNoRows) || err == nil && u.FirstRunID != r.ID {
		return nil
	} else if err != nil {
		return err
	}
	return svc.clearHoldTx(ctx, tx, u, "run:"+r.ID, r.ID)
}

// UsageLimits returns a space's holds, newest first (cleared ones included), followed by the open holds other spaces
// opened on the accounts the space binds or the gateway it routes through: those hold this space too (ADR-0079 Q8)
// and are shown without the other space's identifiers.
func (svc *Service) UsageLimits(ctx context.Context, spaceID string) ([]UsageLimit, error) {
	q := svc.s.R()
	if _, err := q.GetSpace(ctx, spaceID); errors.Is(err, sql.ErrNoRows) {
		return nil, errSpaceNotFound
	} else if err != nil {
		return nil, err
	}
	rows, err := q.ListUsageLimitsBySpace(ctx, db.ListUsageLimitsBySpaceParams{SpaceID: spaceID, Limit: maxLimitRows})
	if err != nil {
		return nil, err
	}
	out := make([]UsageLimit, 0, len(rows))
	for _, r := range rows {
		out = append(out, usageLimitFromRow(r))
	}
	var shared []db.UsageLimit
	bound, err := q.ListSpaceAccounts(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, b := range bound {
		if seen[b.AccountID] {
			continue
		}
		seen[b.AccountID] = true
		rs, err := q.ListOpenUsageLimitsByAccount(ctx, b.AccountID)
		if err != nil {
			return nil, err
		}
		shared = append(shared, rs...)
	}
	if hp, err := q.GetSpaceHarborPilot(ctx, spaceID); err == nil && hp.Enabled == 1 {
		for _, ref := range []string{gatewayRefHP, gatewayRefHPProject + hp.ProjectID} {
			rs, err := q.ListOpenUsageLimitsByGateway(ctx, ref)
			if err != nil {
				return nil, err
			}
			shared = append(shared, rs...)
		}
	} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	for _, r := range shared {
		if r.SpaceID != spaceID && len(out) < 2*maxLimitRows {
			out = append(out, usageLimitFromRow(r).shared())
		}
	}
	return out, nil
}

// AccountUsageLimits returns the open holds on an account in every space (the account-scoped view; account:read).
func (svc *Service) AccountUsageLimits(ctx context.Context, accountID string) ([]UsageLimit, error) {
	rows, err := svc.s.R().ListOpenUsageLimitsByAccount(ctx, accountID)
	if err != nil {
		return nil, err
	}
	out := make([]UsageLimit, 0, len(rows))
	for _, r := range rows {
		out = append(out, usageLimitFromRow(r))
	}
	return out, nil
}

// LimitWaits returns a space's parked runs' waits, newest first.
func (svc *Service) LimitWaits(ctx context.Context, spaceID string) ([]LimitWait, error) {
	q := svc.s.R()
	if _, err := q.GetSpace(ctx, spaceID); errors.Is(err, sql.ErrNoRows) {
		return nil, errSpaceNotFound
	} else if err != nil {
		return nil, err
	}
	rows, err := q.ListLimitWaitsBySpace(ctx, db.ListLimitWaitsBySpaceParams{SpaceID: spaceID, Limit: maxLimitRows})
	if err != nil {
		return nil, err
	}
	out := make([]LimitWait, 0, len(rows))
	for _, r := range rows {
		out = append(out, limitWaitFromRow(r))
	}
	return out, nil
}
