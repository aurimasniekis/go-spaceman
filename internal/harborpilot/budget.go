package harborpilot

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// Budget scopes and states (Harbor Pilot 0.1.3, contract: Budget status).
const (
	ScopeGlobal    = "global"
	ScopeWorkspace = "workspace"
	ScopeProject   = "project"
	ScopeBinding   = "binding"

	EnforceBlock = "block"
	EnforceWarn  = "warn"

	BudgetOK       = "ok"
	BudgetWarning  = "warning"
	BudgetExceeded = "exceeded"
	BudgetDisabled = "disabled"
)

// Overall states of a space's Harbor Pilot status.
const (
	StatusOff         = "off"         // the space doesn't route runs through Harbor Pilot, or there is no connection
	StatusOK          = "ok"          // no covering budget is near its limit
	StatusWarning     = "warning"     // a covering budget passed its warning level, or a warn-only one is used up
	StatusExceeded    = "exceeded"    // a covering blocking budget is used up: launches are refused
	StatusUnavailable = "unavailable" // budget status can't be read
)

// BudgetsOptions tune the budget status cache.
type BudgetsOptions struct {
	// Fresh is how long a budget status read is reused (default 30 s); a failed read is reused for FailFresh (5 s), so
	// a burst of launches doesn't call a failing gateway once each.
	Fresh, FailFresh time.Duration
	// ProjectFresh is how long a project's workspace is reused (default 10 min).
	ProjectFresh time.Duration
}

// Budgets reads Harbor Pilot budget status for spaces (ADR-0060 decisions 4 and 7, HP-T5): a cache of
// GET /api/v1/budgets/status per connection (single flight, fetched outside any transaction), the choice of budgets
// that cover a space, the early launch refusal (it implements BudgetChecker) and the status the Limits page shows.
type Budgets struct {
	svc *Service
	o   BudgetsOptions

	mu       sync.Mutex
	status   *cached[[]Budget]
	projects map[string]*cached[string]        // project id → workspace id
	usage    map[string]*cached[*ProjectUsage] // project id + from → usage
	flights  map[string]*flight                // key → in-flight read
	now      func() time.Time
}

type cached[T any] struct {
	key string // what the value was read with (connection id, token version, address)
	at  time.Time
	val T
	err error
}

type flight struct {
	done chan struct{}
}

// slot is where a cached value lives; get and set run under Budgets.mu.
type slot[T any] struct {
	get func() *cached[T]
	set func(*cached[T])
}

func mapSlot[T any](m map[string]*cached[T], k string) slot[T] {
	return slot[T]{get: func() *cached[T] { return m[k] }, set: func(c *cached[T]) { m[k] = c }}
}

var _ BudgetChecker = (*Budgets)(nil)

// NewBudgets returns the budget status reader over the connection and space settings of svc.
func NewBudgets(svc *Service, o BudgetsOptions) *Budgets {
	if o.Fresh <= 0 {
		o.Fresh = 30 * time.Second
	}
	if o.FailFresh <= 0 {
		o.FailFresh = 5 * time.Second
	}
	if o.ProjectFresh <= 0 {
		o.ProjectFresh = 10 * time.Minute
	}
	return &Budgets{svc: svc, o: o, projects: map[string]*cached[string]{}, usage: map[string]*cached[*ProjectUsage]{}, flights: map[string]*flight{},
		now: svc.s.Now}
}

// Invalidate drops every cached read, so the next check or status reads Harbor Pilot again.
func (b *Budgets) Invalidate() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.status = nil
	clear(b.projects)
	clear(b.usage)
}

// errStatusUnavailable wraps why budget status can't be read.
type errStatusUnavailable struct{ reason string }

func (e *errStatusUnavailable) Error() string { return e.reason }

func unavailableStatus(format string, args ...any) error {
	return &errStatusUnavailable{reason: fmt.Sprintf(format, args...)}
}

// reader is what a read needs: the connection's client, service token and binding, and a key that changes whenever any
// of them does (so a cached value read with another token or address is never reused).
type reader struct {
	c       *Client
	token   string
	binding string
	key     string
}

func (b *Budgets) reader(ctx context.Context) (*reader, error) {
	conn, err := b.svc.s.R().GetHarborPilotConnection(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, unavailableStatus("no Harbor Pilot connection is configured")
	} else if err != nil {
		return nil, err
	}
	if len(conn.ServiceTokenEnc) == 0 {
		return nil, unavailableStatus("the Harbor Pilot connection has no service-principal token (usage.read, config.read), so budget status can't be read")
	}
	tok, err := b.svc.open(serviceAAD(conn.ID), conn.ServiceTokenEnc, "service-principal")
	if err != nil {
		return nil, err
	}
	c, err := b.svc.client(conn.BaseUrl, conn.AllowInsecureHttp == 1)
	if err != nil {
		return nil, unavailableStatus("the Harbor Pilot connection's address is not usable")
	}
	key := fmt.Sprintf("%s|%d|%d|%s|%d", conn.ID, conn.Version, conn.ServiceTokenVersion, conn.BaseUrl, conn.AllowInsecureHttp)
	return &reader{c: c, token: tok, binding: conn.BindingID, key: key}, nil
}

// load returns the cached value under name when it was read with the same key and is fresh, and otherwise reads it
// once (callers asking meanwhile wait for that read).
func load[T any](ctx context.Context, b *Budgets, name, key string, s slot[T], fresh time.Duration, read func(context.Context) (T, error)) (T, time.Time, error) {
	for {
		b.mu.Lock()
		cur := s.get()
		now := b.now()
		if cur != nil && cur.key == key {
			ttl := fresh
			if cur.err != nil {
				ttl = b.o.FailFresh
			}
			if now.Sub(cur.at) < ttl {
				b.mu.Unlock()
				return cur.val, cur.at, cur.err
			}
		}
		if f := b.flights[name]; f != nil {
			b.mu.Unlock()
			select {
			case <-f.done:
				continue
			case <-ctx.Done():
				var zero T
				return zero, time.Time{}, ctx.Err()
			}
		}
		f := &flight{done: make(chan struct{})}
		b.flights[name] = f
		b.mu.Unlock()

		// The read is shared: one caller's cancelled request doesn't fail the others' (the client bounds it).
		val, err := read(context.WithoutCancel(ctx))
		b.mu.Lock()
		at := b.now()
		s.set(&cached[T]{key: key, at: at, val: val, err: err})
		delete(b.flights, name)
		close(f.done)
		b.mu.Unlock()
		return val, at, err
	}
}

// budgetStatus returns every budget's status (cached).
func (b *Budgets) budgetStatus(ctx context.Context, r *reader) ([]Budget, time.Time, error) {
	return load(ctx, b, "status", r.key, slot[[]Budget]{get: func() *cached[[]Budget] { return b.status }, set: func(c *cached[[]Budget]) { b.status = c }}, b.o.Fresh, func(ctx context.Context) ([]Budget, error) {
		items, err := r.c.BudgetStatus(ctx, r.token)
		if err != nil {
			return nil, statusErr("budget status", err)
		}
		return items, nil
	})
}

// workspace returns a project's workspace (cached).
func (b *Budgets) workspace(ctx context.Context, r *reader, projectID string) (string, error) {
	ws, _, err := load(ctx, b, "project:"+projectID, r.key, mapSlot(b.projects, projectID), b.o.ProjectFresh, func(ctx context.Context) (string, error) {
		p, err := r.c.GetProject(ctx, r.token, projectID)
		if err != nil {
			return "", statusErr("project "+projectID, err)
		}
		return p.Spec.WorkspaceID, nil
	})
	return ws, err
}

func statusErr(what string, err error) error {
	var he *Error
	if errors.As(err, &he) {
		return unavailableStatus("Harbor Pilot %s could not be read: %s", what, he.Reason())
	}
	return err
}

// Covering is a budget that covers a space, as the space's status shows it. Amounts are decimals in the currency.
type Covering struct {
	BudgetID    string    `json:"budget_id"`
	Name        string    `json:"name" doc:"Harbor Pilot's budget name (untrusted text)"`
	Scope       string    `json:"scope" enum:"global,workspace,project,binding" doc:"What the budget covers: every request, the project's workspace, the space's project, or SpaceMan's client binding"`
	ScopeID     string    `json:"scope_id"`
	Period      string    `json:"period"`
	PeriodStart time.Time `json:"period_start"`
	PeriodEnd   time.Time `json:"period_end" doc:"When the current period ends and the budget resets"`
	Currency    string    `json:"currency"`
	Enforcement string    `json:"enforcement" enum:"block,warn" doc:"block: Harbor Pilot refuses requests once it is used up (a hard limit); warn: it only warns"`
	State       string    `json:"state" enum:"ok,warning,exceeded,disabled"`
	Limit       string    `json:"limit"`
	Spent       string    `json:"spent"`
	Reserved    string    `json:"reserved" doc:"Estimated cost of requests in flight"`
	Fraction    float64   `json:"fraction" doc:"(spent + reserved) / limit"`
	Blocking    bool      `json:"blocking" doc:"Enabled, enforced with block, and used up: launches are refused"`
}

// covers reports whether a budget applies to a space's runs: its project, the project's workspace, the connection's
// client binding (every run token is minted under it, so its trusted refs carry it) or every request. Account budgets
// apply to whichever upstream account serves a request, not to a space, so they are left out.
func covers(bu Budget, projectID, workspaceID, bindingID string) bool {
	switch bu.Scope {
	case ScopeGlobal:
		return true
	case ScopeProject:
		return projectID != "" && bu.ScopeID == projectID
	case ScopeWorkspace:
		return workspaceID != "" && bu.ScopeID == workspaceID
	case ScopeBinding:
		return bindingID != "" && bu.ScopeID == bindingID
	}
	return false
}

// Covering selects the budgets that cover a space with the given project, workspace and binding, most urgent first.
func CoveringBudgets(items []Budget, projectID, workspaceID, bindingID string) []Covering {
	out := []Covering{}
	for _, bu := range items {
		if !covers(bu, projectID, workspaceID, bindingID) {
			continue
		}
		c := Covering{BudgetID: bounded(Sanitize(bu.BudgetID), 64), Name: bounded(Sanitize(bu.Name), 80), Scope: bu.Scope, ScopeID: bounded(Sanitize(bu.ScopeID), 64),
			Period: bounded(Sanitize(bu.Period), 16), PeriodStart: bu.PeriodStart.UTC(), PeriodEnd: bu.PeriodEnd.UTC(), Currency: bounded(Sanitize(bu.Currency), 8),
			Enforcement: bu.Enforcement, State: bu.State, Limit: FormatE10(bu.LimitE10), Spent: FormatE10(bu.SpentE10), Reserved: FormatE10(bu.ReservedE10),
			Fraction: bu.Fraction}
		if !bu.Enabled {
			c.State = BudgetDisabled
		}
		c.Blocking = c.State == BudgetExceeded && c.Enforcement == EnforceBlock
		out = append(out, c)
	}
	rank := func(c Covering) int {
		switch {
		case c.Blocking:
			return 0
		case c.State == BudgetExceeded:
			return 1
		case c.State == BudgetWarning:
			return 2
		case c.State == BudgetOK:
			return 3
		}
		return 4
	}
	slices.SortStableFunc(out, func(a, b Covering) int {
		if d := rank(a) - rank(b); d != 0 {
			return d
		}
		return strings.Compare(a.Name, b.Name)
	})
	return out
}

// FormatE10 formats an amount in 1e-10 units as a decimal with at least two and at most ten decimals.
func FormatE10(v int64) string {
	neg := v < 0
	if neg {
		v = -v
	}
	s := fmt.Sprintf("%d.%010d", v/1e10, v%1e10)
	s = strings.TrimRight(s, "0")
	if i := strings.IndexByte(s, '.'); len(s)-i-1 < 2 {
		s += strings.Repeat("0", 2-(len(s)-i-1))
	}
	if neg {
		return "-" + s
	}
	return s
}

// covering reads the budgets covering a project (and its workspace and the connection's binding).
func (b *Budgets) covering(ctx context.Context, projectID string) ([]Covering, time.Time, *reader, error) {
	r, err := b.reader(ctx)
	if err != nil {
		return nil, time.Time{}, nil, err
	}
	items, at, err := b.budgetStatus(ctx, r)
	if err != nil {
		return nil, time.Time{}, r, err
	}
	ws := ""
	// The workspace is looked up only when a workspace budget exists: most installations have none.
	if slices.ContainsFunc(items, func(bu Budget) bool { return bu.Scope == ScopeWorkspace }) {
		if ws, err = b.workspace(ctx, r, projectID); err != nil {
			return nil, time.Time{}, r, err
		}
	}
	return CoveringBudgets(items, projectID, ws, r.binding), at, r, nil
}

// Refusal is the text of a launch refused for a used-up budget: it names the budget, its spend and when it resets.
func Refusal(c Covering) string {
	return fmt.Sprintf("Harbor Pilot budget %q (%s %s) is used up for this %s: %s of %s %s spent; it resets at %s. Harbor Pilot refuses this space's model requests until then",
		c.Name, c.Scope, c.ScopeID, c.Period, c.Spent, c.Limit, c.Currency, c.PeriodEnd.Format(time.RFC3339))
}

// CheckBudget implements BudgetChecker: a launch is refused (rate_limited) when a covering budget enforced with block is
// used up. Warn-only budgets never refuse. When budget status can't be read, the space's on_budget_status_unavailable
// decides: proceed (Harbor Pilot still enforces at mint and on every request) or refuse (unavailable). That includes
// SpaceMan's own failures on the way (the service-principal token can't be opened, a database read fails): they are
// logged, and the person sees only a fixed reason.
func (b *Budgets) CheckBudget(ctx context.Context, spaceID string, s *SpaceSettings) error {
	cov, _, _, err := b.covering(ctx, s.ProjectID)
	var ue *errStatusUnavailable
	if err != nil && !errors.As(err, &ue) && ctx.Err() == nil {
		b.svc.log().Error("harbor pilot budget check: status can't be read", slog.String("space.id", spaceID), "err", err)
		err = unavailableStatus("SpaceMan could not read its Harbor Pilot connection or token; see central's log")
	}
	switch {
	case errors.As(err, &ue):
		if s.OnBudgetStatusUnavailable == BudgetRefuse {
			return apperr.New(apperr.CodeUnavailable, "Harbor Pilot budget status can't be read (%s), and this space refuses launches then (on_budget_status_unavailable: refuse)", ue.reason)
		}
		b.svc.log().Warn("harbor pilot budget status unavailable: launch proceeds (the gateway still enforces)", slog.String("space.id", spaceID), slog.String("reason", ue.reason))
		return nil
	case err != nil:
		return err
	}
	for _, c := range cov {
		if c.Blocking {
			return apperr.New(apperr.CodeRateLimited, "%s", Refusal(c))
		}
	}
	return nil
}

// ProjectUsage is the space's project's usage over every client of Harbor Pilot (not only SpaceMan): other clients may
// share the project, so it is labelled "all clients".
type ProjectUsage struct {
	ProjectID    string       `json:"project_id"`
	Clients      string       `json:"clients" enum:"all" doc:"Always all: every client of the project, not only this space's runs"`
	From         time.Time    `json:"from"`
	To           time.Time    `json:"to"`
	Window       string       `json:"window" doc:"What From covers: the current period of the project's budget (e.g. day), or last_30_days"`
	Requests     int64        `json:"requests"`
	Input        int64        `json:"input_tokens"`
	Output       int64        `json:"output_tokens"`
	CacheRead    int64        `json:"cache_read_tokens"`
	CacheWrite   int64        `json:"cache_write_tokens"`
	UnknownUsage int64        `json:"unknown_usage_requests" doc:"Requests whose usage the provider did not report (not counted as zero)"`
	Costs        []CostAmount `json:"costs" doc:"Per category and currency; categories are never summed"`
}

// SpaceStatus is a space's Harbor Pilot status for the Limits page.
type SpaceStatus struct {
	SpaceID                   string        `json:"space_id"`
	Source                    string        `json:"source" enum:"harbor-pilot"`
	State                     string        `json:"state" enum:"off,ok,warning,exceeded,unavailable" doc:"exceeded: a covering blocking budget is used up and launches are refused"`
	Connected                 bool          `json:"connected"`
	Enabled                   bool          `json:"enabled"`
	ProjectID                 string        `json:"project_id"`
	Harnesses                 []string      `json:"harnesses"`
	OnBudgetStatusUnavailable string        `json:"on_budget_status_unavailable" enum:"proceed,refuse"`
	Budgets                   []Covering    `json:"budgets" doc:"Budgets covering the space: its project, the project's workspace, the connection's client binding, and global ones"`
	Unavailable               string        `json:"unavailable,omitempty" doc:"Why budget status can't be read"`
	FetchedAt                 *time.Time    `json:"fetched_at,omitempty" doc:"When Harbor Pilot was read (status is reused for 30 s)"`
	Usage                     *ProjectUsage `json:"usage,omitempty"`
	UsageUnavailable          string        `json:"usage_unavailable,omitempty"`
}

// SpaceStatus returns a space's Harbor Pilot status: its covering budgets, their states and the project's usage.
func (b *Budgets) SpaceStatus(ctx context.Context, spaceID string) (*SpaceStatus, error) {
	set, err := b.svc.GetSpace(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	out := &SpaceStatus{SpaceID: spaceID, Source: proto.UsageHarborPilot, State: StatusOff, Connected: set.Connected, Enabled: set.Enabled, ProjectID: set.ProjectID,
		Harnesses: set.Harnesses, OnBudgetStatusUnavailable: set.OnBudgetStatusUnavailable, Budgets: []Covering{}}
	if !set.Enabled || !set.Connected {
		return out, nil
	}
	cov, at, r, err := b.covering(ctx, set.ProjectID)
	var ue *errStatusUnavailable
	switch {
	case errors.As(err, &ue):
		out.State, out.Unavailable = StatusUnavailable, ue.reason
		return out, nil
	case err != nil:
		return nil, err
	}
	out.Budgets, out.FetchedAt = cov, &at
	out.State = StatusOK
	for _, c := range cov {
		switch {
		case c.Blocking:
			out.State = StatusExceeded
		case (c.State == BudgetWarning || c.State == BudgetExceeded) && out.State == StatusOK:
			out.State = StatusWarning
		}
	}
	out.Usage, err = b.projectUsage(ctx, r, set.ProjectID, cov)
	if errors.As(err, &ue) {
		out.UsageUnavailable = ue.reason
	} else if err != nil {
		return nil, err
	}
	return out, nil
}

// projectUsage reads the project's usage over all clients, from the start of its project budget's current period (the
// earliest, when several), or over the last 30 days without one (cached like budget status).
func (b *Budgets) projectUsage(ctx context.Context, r *reader, projectID string, cov []Covering) (*ProjectUsage, error) {
	var from time.Time
	window := "last_30_days"
	for _, c := range cov {
		if c.Scope == ScopeProject && !c.PeriodStart.IsZero() && (from.IsZero() || c.PeriodStart.Before(from)) {
			from, window = c.PeriodStart, c.Period
		}
	}
	key := projectID + "|" + from.Format(time.RFC3339)
	b.mu.Lock()
	if len(b.usage) > 256 { // old periods' entries: bounded, never needed again
		clear(b.usage)
	}
	b.mu.Unlock()
	pu, _, err := load(ctx, b, "usage:"+key, r.key, mapSlot(b.usage, key), b.o.Fresh, func(ctx context.Context) (*ProjectUsage, error) {
		sum, err := r.c.UsageSummary(ctx, r.token, UsageQuery{GroupBy: "project", From: from})
		if err != nil {
			return nil, statusErr("usage", err)
		}
		pu := &ProjectUsage{ProjectID: projectID, Clients: "all", From: sum.From.UTC(), To: sum.To.UTC(), Window: window, Costs: []CostAmount{}}
		for _, g := range sum.Groups {
			if g.Key != projectID {
				continue
			}
			pu.Requests, pu.Input, pu.Output = g.Attempts, g.Tokens.Input, g.Tokens.Output
			pu.CacheRead, pu.CacheWrite, pu.UnknownUsage = g.Tokens.CacheRead, g.Tokens.CacheWrite, g.Tokens.UnknownUsage
			pu.Costs = cleanCosts(g.Costs)
		}
		return pu, nil
	})
	return pu, err
}

// cleanCosts bounds Harbor Pilot's cost entries (untrusted text) and drops malformed amounts.
func cleanCosts(in []CostAmount) []CostAmount {
	out := []CostAmount{}
	for _, c := range in {
		if _, err := strconv.ParseFloat(c.Amount, 64); err != nil || len(out) >= 16 {
			continue
		}
		out = append(out, CostAmount{Kind: bounded(Sanitize(c.Kind), 32), Currency: bounded(Sanitize(c.Currency), 8), Amount: bounded(c.Amount, 32)})
	}
	return out
}
