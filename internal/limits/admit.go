package limits

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// BudgetNote is shown wherever a budget is: SpaceMan never claims a hard currency limit.
const BudgetNote = "Budgets use the usage the harnesses report, which arrives late (often only when a run ends) or not at all. " +
	"A budget warns and stops new runs once reported usage passes it; it is not a hard currency limit, runs in progress are never stopped, " +
	"and unknown usage is not counted."

// workKinds share the space's run concurrency limit and the node's capacity. Control (one per space), KB support (its
// own pool per knowledge base and space) and dreamer runs (one per dream configuration) never compete with them, so
// workers cannot starve support and support cannot starve workers (SM-LIMIT-002).
func workKind(kind string) bool { return kind == "worker" || kind == "diagnostic" }

// notCounted tells a refused caller which runs are not part of the count, so a mismatch with spaceman_runs_list (which
// shows every live run of the space) can be explained.
const notCounted = "Only live worker and diagnostic runs count; control, KB support and dreamer runs, and runs that exited, failed or were lost, do not"

// countedRun is a live run named in a concurrency refusal.
type countedRun struct{ id, kind, role, observed string }

// describeRuns lists counted runs by ID, kind, role and observed state ("none" when empty).
func describeRuns(runs []countedRun) string {
	if len(runs) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(runs))
	for _, r := range runs {
		parts = append(parts, fmt.Sprintf("%s (%s %s, %s)", r.id, r.kind, r.role, r.observed))
	}
	return strings.Join(parts, ", ")
}

// Admit refuses a launch beyond the space's concurrency limit, the node's capacity, or an exceeded budget. It runs
// inside the launch transaction, so concurrent launches cannot both take the last slot.
func (svc *Service) Admit(ctx context.Context, tx *store.Tx, a fleet.Admission) error {
	v, err := svc.For(ctx, tx.Q, a.SpaceID)
	if err != nil {
		return err
	}
	if workKind(a.Kind) {
		// Only live runs count (fleet.Live, the runs spaceman_runs_list shows): a run that exited, failed or was lost
		// frees its slot even while its desired state is still "running".
		runs, err := tx.Q.ListLiveWorkRunsInSpace(ctx, a.SpaceID)
		if err != nil {
			return err
		}
		if limit := v.Get(ConcurrentRuns); int64(len(runs)) >= limit {
			counted := make([]countedRun, 0, len(runs))
			for _, r := range runs {
				counted = append(counted, countedRun{r.ID, r.Kind, r.Role, r.Observed})
			}
			return apperr.New(apperr.CodeRateLimited, "the space has %d worker and diagnostic runs active (limit %d): %s. %s; start more when one ends",
				len(runs), limit, describeRuns(counted), notCounted)
		}
		capacity, err := tx.Q.GetNodeCapacity(ctx, a.NodeID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if capacity > 0 {
			onNode, err := tx.Q.ListLiveWorkRunsOnNode(ctx, a.NodeID)
			if err != nil {
				return err
			}
			if int64(len(onNode)) >= capacity {
				// Other spaces' runs are only counted, never named: this space's caller may not see them.
				var own []countedRun
				others := 0
				for _, r := range onNode {
					if r.SpaceID != a.SpaceID {
						others++
						continue
					}
					own = append(own, countedRun{r.ID, r.Kind, r.Role, r.Observed})
				}
				return apperr.New(apperr.CodeRateLimited, "the space's node runs %d worker and diagnostic runs (its capacity is %d, shared by every space on the node): "+
					"%d of other spaces and %d of this space (%s). %s; start more when one ends", len(onNode), capacity, others, len(own), describeRuns(own), notCounted)
			}
		}
	}
	b, err := svc.budget(ctx, tx.Q, a.SpaceID, v)
	if err != nil {
		return err
	}
	if b.State == BudgetExceeded {
		return apperr.New(apperr.CodeRateLimited, "the space's reported usage of the last 24 hours is over its budget (%s); no new runs start until it is "+
			"below or the budget is raised. Usage is reported late, so this is not a hard limit", b.summary())
	}
	return nil
}

// Budget states.
const (
	BudgetNone     = "none"
	BudgetOK       = "ok"
	BudgetWarning  = "warning"
	BudgetExceeded = "exceeded"
)

// Budget is a space's reported usage over the budget window against its budgets.
type Budget struct {
	WindowHours   int64    `json:"window_hours"`
	Tokens        int64    `json:"tokens" doc:"Reported input and output tokens of runs started in the window (known usage only)"`
	TokensLimit   int64    `json:"tokens_limit" doc:"0: no token budget"`
	CostCents     int64    `json:"cost_cents" doc:"Sum of the harnesses' own cost estimates, in US cents"`
	CostLimit     int64    `json:"cost_limit_cents" doc:"0: no cost budget"`
	Runs          int64    `json:"runs" doc:"Runs started in the window"`
	UnknownRuns   int64    `json:"unknown_runs" doc:"Runs in the window whose usage is not known (not counted)"`
	EstimatedRuns int64    `json:"estimated_runs" doc:"Runs with a cost estimate"`
	WarnPercent   int64    `json:"warn_percent"`
	State         string   `json:"state" enum:"none,ok,warning,exceeded"`
	Over          []string `json:"over" doc:"Budgets at or past the warning share"`
	Note          string   `json:"note"`
}

func (b *Budget) summary() string {
	s := ""
	if b.TokensLimit > 0 {
		s = fmt.Sprintf("%d of %d tokens", b.Tokens, b.TokensLimit)
	}
	if b.CostLimit > 0 {
		if s != "" {
			s += ", "
		}
		s += fmt.Sprintf("estimated $%.2f of $%.2f", float64(b.CostCents)/100, float64(b.CostLimit)/100)
	}
	return s
}

// usageTokens returns a run's reported input and output tokens, and whether they are known.
func usageTokens(source, models string) (int64, bool) {
	if source == "" || source == proto.UsageUnavailable {
		return 0, false
	}
	var ms []proto.ModelUsage
	if json.Unmarshal([]byte(models), &ms) != nil {
		return 0, false
	}
	var n int64
	for _, m := range ms {
		if m.Input != nil {
			n += *m.Input
		}
		if m.Output != nil {
			n += *m.Output
		}
	}
	return n, true
}

func level(used, limit, warn int64) string {
	switch {
	case limit <= 0:
		return BudgetNone
	case used >= limit:
		return BudgetExceeded
	case used*100 >= limit*warn:
		return BudgetWarning
	}
	return BudgetOK
}

const budgetWindow = 24

// budget computes a space's budget state from the usage reported so far.
func (svc *Service) budget(ctx context.Context, q *db.Queries, spaceID string, v Values) (*Budget, error) {
	b := &Budget{WindowHours: budgetWindow, TokensLimit: v.Get(BudgetTokens), CostLimit: v.Get(BudgetCost), WarnPercent: v.Get(BudgetWarnPercent),
		State: BudgetNone, Over: []string{}, Note: BudgetNote}
	if b.TokensLimit == 0 && b.CostLimit == 0 {
		return b, nil
	}
	since := svc.clock.Now().Add(-budgetWindow * 60 * 60 * 1e9).UnixMicro()
	rows, err := q.ListSpaceUsageSince(ctx, db.ListSpaceUsageSinceParams{SpaceID: spaceID, CreatedAt: since})
	if err != nil {
		return nil, err
	}
	var usd float64
	for _, r := range rows {
		b.Runs++
		n, known := usageTokens(r.Source, r.ModelsJson)
		if !known {
			b.UnknownRuns++
			continue
		}
		b.Tokens += n
		if r.EstimateJson != "" {
			var e proto.CostEstimate
			if json.Unmarshal([]byte(r.EstimateJson), &e) == nil {
				usd += e.USD
				b.EstimatedRuns++
			}
		}
	}
	b.CostCents = int64(math.Round(usd * 100))
	b.State = BudgetOK
	for _, x := range []struct {
		key         string
		used, limit int64
	}{{BudgetTokens, b.Tokens, b.TokensLimit}, {BudgetCost, b.CostCents, b.CostLimit}} {
		switch level(x.used, x.limit, b.WarnPercent) {
		case BudgetExceeded:
			b.State = BudgetExceeded
			b.Over = append(b.Over, x.key)
		case BudgetWarning:
			if b.State == BudgetOK {
				b.State = BudgetWarning
			}
			b.Over = append(b.Over, x.key)
		}
	}
	return b, nil
}

// OnUsage checks a run's space and run budgets after a usage report and warns the operators once when a budget passes
// its warning share or its limit. It never stops a run.
func (svc *Service) OnUsage(ctx context.Context, runID string) {
	r, err := svc.s.R().GetRun(ctx, runID)
	if err != nil {
		return
	}
	v, err := svc.For(ctx, svc.s.R(), r.SpaceID)
	if err != nil {
		return
	}
	warn := v.Get(BudgetWarnPercent)
	if limit := v.Get(BudgetTokensPerRun); limit > 0 {
		if u, err := svc.s.R().GetRunUsage(ctx, runID); err == nil {
			if n, known := usageTokens(u.Source, u.ModelsJson); known {
				if lv := level(n, limit, warn); lv == BudgetWarning || lv == BudgetExceeded {
					svc.notice(ctx, r.SpaceID, "run", runID, BudgetTokensPerRun, lv, n, limit,
						fmt.Sprintf("Run %s (%s) reported %d tokens; its per-run budget is %d. The run is not stopped.", runID, r.Role, n, limit))
				}
			}
		}
	}
	b, err := svc.budget(ctx, svc.s.R(), r.SpaceID, v)
	if err != nil || b.State == BudgetNone {
		return
	}
	for _, x := range []struct {
		key         string
		used, limit int64
		what        string
	}{{BudgetTokens, b.Tokens, b.TokensLimit, fmt.Sprintf("%d of %d tokens", b.Tokens, b.TokensLimit)},
		{BudgetCost, b.CostCents, b.CostLimit, fmt.Sprintf("an estimated $%.2f of $%.2f", float64(b.CostCents)/100, float64(b.CostLimit)/100)}} {
		lv := level(x.used, x.limit, warn)
		if lv != BudgetWarning && lv != BudgetExceeded {
			continue
		}
		body := fmt.Sprintf("Runs started in this space over the last 24 hours reported %s (%d runs with unknown usage are not counted).", x.what, b.UnknownRuns)
		if lv == BudgetExceeded {
			body += " New runs are refused until reported usage is below the budget or the budget is raised; runs in progress continue."
		}
		svc.notice(ctx, r.SpaceID, "space", "", x.key, lv, x.used, x.limit, body+" "+BudgetNote)
	}
}

// notice records a budget notice and tells the operators, once per run (scope run) or once per window (scope space).
func (svc *Service) notice(ctx context.Context, spaceID, scope, runID, budget, lv string, used, limit int64, body string) {
	now := svc.clock.Now()
	if last, err := svc.s.R().LastBudgetNotice(ctx, db.LastBudgetNoticeParams{SpaceID: spaceID, Scope: scope, RunID: runID, Budget: budget, Level: lv}); err == nil {
		if scope == "run" || now.Sub(fromMicros(last.At)) < budgetWindow*60*60*1e9 {
			return
		}
	}
	id := ids.New("bdn")
	err := svc.s.W().TxNamed(ctx, "limits.budget_notice", func(tx *store.Tx) error {
		if err := tx.Q.InsertBudgetNotice(ctx, db.InsertBudgetNoticeParams{ID: id, SpaceID: spaceID, Scope: scope, RunID: runID, Budget: budget, Level: lv,
			Used: used, LimitValue: limit, At: tx.NowMicros()}); err != nil {
			return err
		}
		if err := svc.changes.Record(ctx, tx, spaceID, "space_limits", spaceID, "updated", 0); err != nil {
			return err
		}
		_, err := svc.audit.Append(ctx, tx, audit.Event{Actor: audit.Actor{Type: audit.ActorSystem, ID: "limits"}, Action: "budget." + lv,
			Target: audit.Target{Type: "space", ID: spaceID}, Details: map[string]any{"space_id": spaceID, "budget": budget, "scope": scope, "run_id": runID,
				"used": used, "limit": limit}})
		return err
	})
	if err != nil {
		svc.log.Warn("record a budget notice", slog.String("space.id", spaceID), slog.Any("error", err))
		return
	}
	if svc.Notify != nil {
		subject := "Budget warning"
		if lv == BudgetExceeded {
			subject = "Budget exceeded"
		}
		if err := svc.Notify(ctx, spaceID, subject, body, "budget:"+id, false); err != nil {
			svc.log.Warn("notify a budget notice", slog.String("space.id", spaceID), slog.Any("error", err))
		}
	}
}

// ToolCalls wraps the agent tool dispatcher with the per-run tool-call limit. Listing the catalog is not a call. A
// refused call leaves the run running; the agent is told why.
func (svc *Service) ToolCalls(next fleet.ToolDispatcher) fleet.ToolDispatcher {
	return func(ctx context.Context, p *fleet.RunPrincipal, tool string, args json.RawMessage) (json.RawMessage, error) {
		if tool != proto.ToolCatalog {
			if err := svc.countToolCall(ctx, p); err != nil {
				return nil, err
			}
		}
		return next(ctx, p, tool, args)
	}
}

func (svc *Service) countToolCall(ctx context.Context, p *fleet.RunPrincipal) error {
	var limit, calls int64
	refused := false
	err := svc.s.W().TxNamed(ctx, "limits.tool_call", func(tx *store.Tx) error {
		refused = false
		v, err := svc.For(ctx, tx.Q, p.SpaceID)
		if err != nil {
			return err
		}
		limit = v.Get(ToolCallsPerRun)
		if row, err := tx.Q.GetRunToolCalls(ctx, p.RunID); err == nil {
			calls = row.Calls
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		refused = limit > 0 && calls >= limit
		ok, no := int64(1), int64(0)
		if refused {
			ok, no = 0, 1
		}
		return tx.Q.CountRunToolCall(ctx, db.CountRunToolCallParams{RunID: p.RunID, SpaceID: p.SpaceID, Calls: ok, Refused: no, Now: tx.NowMicros()})
	})
	if err != nil {
		return err
	}
	if refused {
		return apperr.New(apperr.CodeRateLimited, "this run has made %d tool calls, its limit (%d); finish your work without more tool calls or ask an operator to raise max_tool_calls_per_run", calls, limit)
	}
	return nil
}
