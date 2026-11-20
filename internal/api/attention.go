package api

import (
	"context"
	"database/sql"
	"math"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/obs/fsstat"
	"github.com/aurimasniekis/go-spaceman/internal/obs/health"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// AttentionRules: the summary is filtered per category by the caller's permissions, so it needs only a session.
var AttentionRules = map[string]Rule{
	"get-attention": FilteredRead(),
}

// Attention windows and thresholds.
const (
	// AttentionRunWindow is how far back failed and lost runs count.
	AttentionRunWindow = 24 * time.Hour
	// AttentionBudgetWindow is how far back budget notices count (the budget window).
	AttentionBudgetWindow = 24 * time.Hour
	// AttentionSwitchWindow is how far back failed account switches count.
	AttentionSwitchWindow = 24 * time.Hour
	// AttentionItemsPerKind caps the items listed per kind (the counts stay complete).
	AttentionItemsPerKind = 20
	// AttentionQuotaPercent is the attachment-storage share of a space's quota that counts as near the limit.
	AttentionQuotaPercent = 90
)

// AttentionSpaceRef is a space the summary may name.
type AttentionSpaceRef struct {
	ID, Slug, Name, State, NodeID string
}

// AttentionNode is a node that needs a person: active but not connected.
type AttentionNode struct {
	ID               string     `json:"id"`
	Name             string     `json:"name"`
	State            string     `json:"state"`
	Connected        bool       `json:"connected"`
	LastDisconnectAt *time.Time `json:"last_disconnect_at,omitempty"`
	Spaces           []string   `json:"spaces" doc:"IDs of readable spaces pinned to the node"`
}

// AttentionSources are the live parts of the summary central supplies (in-memory connection state and readiness).
type AttentionSources struct {
	Spaces       func(ctx context.Context) ([]AttentionSpaceRef, error)
	Nodes        func(ctx context.Context) ([]AttentionNode, error)
	Readiness    func(ctx context.Context) health.Report
	StoreHealth  func() (state, reason string)
	DataDir      string
	MinFreeBytes int64
	MinFreePct   int
	StorageQuota func(ctx context.Context, spaceID string) int64
}

// AttentionSpace is what needs a person in one space. Counts of a category the caller may not read are omitted (nil).
type AttentionSpace struct {
	SpaceID string `json:"space_id"`
	Slug    string `json:"slug"`
	Name    string `json:"name"`
	State   string `json:"state"`
	// Human actions (human_action:read).
	HumanPending    *int64 `json:"human_pending,omitempty"`
	HumanOverdue    *int64 `json:"human_overdue,omitempty"`
	HumanEscalated  *int64 `json:"human_escalated,omitempty"`
	HumanUnassigned *int64 `json:"human_unassigned,omitempty"`
	// Tasks (queue:read).
	TasksBlocked *int64 `json:"tasks_blocked,omitempty"`
	TasksFailed  *int64 `json:"tasks_failed,omitempty"`
	// Event ingestion (subscription:read).
	QuarantineOpen *int64 `json:"quarantine_open,omitempty"`
	// Runs (run:read): unknown now, failed or lost in the last 24 hours.
	RunsUnknown *int64 `json:"runs_unknown,omitempty"`
	RunsFailed  *int64 `json:"runs_failed,omitempty"`
	// Budgets and storage (space_config:read): budget notices of the last 24 hours, attachment storage against quota.
	BudgetWarnings *int64 `json:"budget_warnings,omitempty"`
	BudgetExceeded *int64 `json:"budget_exceeded,omitempty"`
	StorageBytes   *int64 `json:"storage_bytes,omitempty"`
	StorageQuota   *int64 `json:"storage_quota_bytes,omitempty" doc:"0: no quota"`
	StorageNear    bool   `json:"storage_near_quota,omitempty" doc:"Attachment storage is at least 90% of the quota"`
	// Parked runs, account limits, failed account switches (run:read) and incoming cross-space requests (xspace:read).
	RunsParked         *int64 `json:"runs_parked,omitempty" doc:"Runs parked at a usage limit or for a restart that still wait to come back"`
	AccountLimits      *int64 `json:"account_limits,omitempty" doc:"Open account usage limits as this space sees them"`
	AccountSwitchFails *int64 `json:"account_switches_failed,omitempty" doc:"Account switches that failed in the last 24 hours"`
	XSpaceIncoming     *int64 `json:"xspace_incoming,omitempty" doc:"Cross-space requests from other spaces waiting for a decision"`
	// NodeDisconnected: the space's node is enrolled but not connected, so no new work starts (space:read).
	NodeDisconnected bool  `json:"node_disconnected,omitempty"`
	Total            int64 `json:"total" doc:"Items in this space that need a person (the counts above, without the unassigned and overdue subsets)"`
}

// AttentionTarget says where an item lives, so a client can link to it. Type is one of task, run, subscription,
// space_usage, space_budget or xspace_request; ID is that resource's ID (empty for the space pages).
type AttentionTarget struct {
	Type    string `json:"type" enum:"task,run,subscription,space_usage,space_budget,xspace_request"`
	ID      string `json:"id,omitempty"`
	SpaceID string `json:"space_id"`
}

// AttentionItem names one thing that needs a person. Titles are fixed words or the item's own title, never body text.
type AttentionItem struct {
	Kind     string          `json:"kind" enum:"task_blocked,task_failed,quarantine,run_unknown,run_failed,budget_notice,run_parked,account_limit,account_switch_failed,xspace_request"`
	Severity string          `json:"severity" enum:"act,watch" doc:"act: someone has to do something; watch: it may resolve itself"`
	ID       string          `json:"id"`
	SpaceID  string          `json:"space_id"`
	Title    string          `json:"title"`
	Since    time.Time       `json:"since"`
	Target   AttentionTarget `json:"target"`
}

// AttentionOutbox is outbound publication trouble (system:read).
type AttentionOutbox struct {
	Dead     int64 `json:"dead" doc:"Events that will not be published again (dead letters)"`
	Retrying int64 `json:"retrying" doc:"Events that failed at least once and are retried"`
}

// AttentionBackups is the backup state (system:read).
type AttentionBackups struct {
	State            string     `json:"state" enum:"none,ok,failing" doc:"failing: the latest backup or verification failed"`
	LastSucceededAt  *time.Time `json:"last_succeeded_at,omitempty"`
	LastFailedAt     *time.Time `json:"last_failed_at,omitempty"`
	LastVerifyFailed *time.Time `json:"last_verify_failed_at,omitempty"`
}

// AttentionCheck is one readiness check's public result.
type AttentionCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty" doc:"A short code, never a path or error text"`
}

// AttentionStorage is central's storage health (system:read): readiness, the database's health and the data
// directory's free space. Node disks are not reported here.
type AttentionStorage struct {
	Readiness     string           `json:"readiness" doc:"ready, degraded, failed, starting or stopping"`
	Checks        []AttentionCheck `json:"checks"`
	StoreState    string           `json:"store_state" enum:"ok,degraded,failed"`
	StoreReason   string           `json:"store_reason,omitempty"`
	DiskTotal     int64            `json:"disk_total_bytes"`
	DiskAvailable int64            `json:"disk_available_bytes"`
	DiskUsedPct   float64          `json:"disk_used_percent"`
	DiskReserve   int64            `json:"disk_reserve_bytes" doc:"Free space below this degrades readiness"`
	BelowReserve  bool             `json:"below_reserve"`
	DiskError     string           `json:"disk_error,omitempty"`
	Problem       bool             `json:"problem" doc:"Readiness is not ready, the database is not ok, or free space is below the reserve"`
}

// Attention is the summary of everything that needs a person, across the spaces and installation areas the caller may
// read (SM-UI-003).
type Attention struct {
	Spaces     []AttentionSpace  `json:"spaces" doc:"Readable spaces with at least one item, most items first"`
	Nodes      []AttentionNode   `json:"nodes,omitempty" doc:"Active nodes that are not connected (node:read)"`
	Outbox     *AttentionOutbox  `json:"outbox,omitempty"`
	Backups    *AttentionBackups `json:"backups,omitempty"`
	Storage    *AttentionStorage `json:"storage,omitempty"`
	Items      []AttentionItem   `json:"items" doc:"The newest items per kind in readable spaces (at most 20 per kind), newest first. Human actions are counted but not listed. Nodes and storage are in their own sections"`
	ItemCounts map[string]int64  `json:"item_counts" doc:"Full number of items per kind in readable spaces, to tell a truncated list"`
	Total      int64             `json:"total"`
	CheckedAt  time.Time         `json:"checked_at"`
}

func i64(v int64) *int64 { return &v }

func micros(v int64) time.Time { return time.UnixMicro(v).UTC() }

// capItems keeps the newest AttentionItemsPerKind items of each kind, newest first.
func capItems(items []AttentionItem) []AttentionItem {
	slices.SortStableFunc(items, func(a, b AttentionItem) int {
		if c := a.Since.Compare(b.Since); c != 0 {
			return -c
		}
		return strings.Compare(a.ID, b.ID)
	})
	out := make([]AttentionItem, 0, len(items))
	per := map[string]int{}
	for _, it := range items {
		if per[it.Kind]++; per[it.Kind] <= AttentionItemsPerKind {
			out = append(out, it)
		}
	}
	return out
}

func microsTime(v int64) *time.Time {
	if v == 0 {
		return nil
	}
	t := time.UnixMicro(v).UTC()
	return &t
}

// RegisterAttention registers the attention summary.
func RegisterAttention(src *AttentionSources) func(api huma.API, d *Deps) {
	return func(api huma.API, d *Deps) {
		huma.Register(api, huma.Operation{OperationID: "get-attention", Method: http.MethodGet, Path: "/attention",
			Summary: "What needs a person", Tags: []string{"system"},
			Description: "One summary of pending human actions, blocked and failed tasks, quarantined events, runs in unknown or failed states, " +
				"parked runs, open account limits, failed account switches, incoming cross-space requests, budgets near their limit, attachment storage near its quota, disconnected nodes, outbound dead letters, failing backups and storage " +
				"problems. Each category is included only where you may read it. `items` names the newest of each kind (at most 20 per kind, counts in `item_counts`). Computed on request (no network I/O, one or two queries per category); clients refresh it " +
				"when the change feed reports a change."},
			func(ctx context.Context, in *struct {
				Space string `query:"space" maxLength:"64" doc:"Only this space (and no installation-wide sections)"`
			}) (*struct{ Body Attention }, error) {
				out, err := d.attention(ctx, src, in.Space)
				if err != nil {
					return nil, d.fail(ctx, "attention.get", err)
				}
				return &struct{ Body Attention }{Body: *out}, nil
			})
	}
}

func (d *Deps) attention(ctx context.Context, src *AttentionSources, only string) (*Attention, error) {
	p := PrincipalFrom(ctx)
	now := d.now()
	can := func(space string, perm authz.Permission) bool {
		return d.Authz != nil && d.Authz.AllowedAll(p.Subjects(), space, perm)
	}
	refs, err := src.Spaces(ctx)
	if err != nil {
		return nil, err
	}
	byID := map[string]*AttentionSpace{}
	var order []string
	for _, r := range refs {
		if (only != "" && r.ID != only) || !can(r.ID, authz.SpaceRead) {
			continue
		}
		byID[r.ID] = &AttentionSpace{SpaceID: r.ID, Slug: r.Slug, Name: r.Name, State: r.State}
		order = append(order, r.ID)
	}
	get := func(space string, perm authz.Permission) *AttentionSpace {
		s := byID[space]
		if s == nil || !can(space, perm) {
			return nil
		}
		return s
	}
	q := d.Store.R()
	var items []AttentionItem
	counts := map[string]int64{}
	// add records an item of a space the caller may read with the permission its category needs.
	add := func(perm authz.Permission, it AttentionItem) bool {
		if get(it.SpaceID, perm) == nil {
			return false
		}
		items = append(items, it)
		return true
	}
	hum, err := q.AttentionHuman(ctx, sql.NullInt64{Int64: now.UnixMicro(), Valid: true})
	if err != nil {
		return nil, err
	}
	for _, r := range hum {
		if s := get(r.SpaceID, authz.HumanRead); s != nil {
			s.HumanPending, s.HumanOverdue, s.HumanEscalated, s.HumanUnassigned = i64(r.Pending), i64(r.Overdue), i64(r.Escalated), i64(r.Unassigned)
			s.Total += r.Pending
		}
	}
	tasks, err := q.AttentionTasks(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range tasks {
		if s := get(r.SpaceID, authz.QueueRead); s != nil {
			s.TasksBlocked, s.TasksFailed = i64(r.Blocked), i64(r.Failed)
			s.Total += r.Blocked + r.Failed
			counts["task_blocked"] += r.Blocked
			counts["task_failed"] += r.Failed
		}
	}
	taskItems, err := q.AttentionTaskItems(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range taskItems {
		kind := "task_blocked"
		if r.Status == "failed" {
			kind = "task_failed"
		}
		add(authz.QueueRead, AttentionItem{Kind: kind, Severity: "act", ID: r.ID, SpaceID: r.SpaceID, Title: r.Title, Since: micros(r.Since),
			Target: AttentionTarget{Type: "task", ID: r.ID, SpaceID: r.SpaceID}})
	}
	quar, err := q.AttentionQuarantine(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range quar {
		if s := get(r.SpaceID, authz.SubscriptionRead); s != nil {
			s.QuarantineOpen = i64(r.Open)
			s.Total += r.Open
			counts["quarantine"] += r.Open
		}
	}
	quarItems, err := q.AttentionQuarantineItems(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range quarItems {
		add(authz.SubscriptionRead, AttentionItem{Kind: "quarantine", Severity: "act", ID: r.ID, SpaceID: r.SpaceID, Title: "Quarantined event", Since: micros(r.Since),
			Target: AttentionTarget{Type: "subscription", ID: r.SubscriptionID, SpaceID: r.SpaceID}})
	}
	runSince := sql.NullInt64{Int64: now.Add(-AttentionRunWindow).UnixMicro(), Valid: true}
	runs, err := q.AttentionRuns(ctx, runSince)
	if err != nil {
		return nil, err
	}
	for _, r := range runs {
		if s := get(r.SpaceID, authz.RunRead); s != nil {
			s.RunsUnknown, s.RunsFailed = i64(r.Unknown), i64(r.Failed)
			s.Total += r.Unknown + r.Failed
			counts["run_unknown"] += r.Unknown
			counts["run_failed"] += r.Failed
		}
	}
	runItems, err := q.AttentionRunItems(ctx, runSince)
	if err != nil {
		return nil, err
	}
	for _, r := range runItems {
		kind, sev, title := "run_failed", "watch", "Run "+r.Observed+": "+r.Role
		if r.Observed == "unknown" {
			kind, sev = "run_unknown", "act"
		}
		add(authz.RunRead, AttentionItem{Kind: kind, Severity: sev, ID: r.ID, SpaceID: r.SpaceID, Title: title, Since: micros(r.Since),
			Target: AttentionTarget{Type: "run", ID: r.ID, SpaceID: r.SpaceID}})
	}
	budgets, err := q.AttentionBudgets(ctx, now.Add(-AttentionBudgetWindow).UnixMicro())
	if err != nil {
		return nil, err
	}
	for _, r := range budgets {
		if s := get(r.SpaceID, authz.SpaceConfigRead); s != nil {
			s.BudgetWarnings, s.BudgetExceeded = i64(r.Warnings), i64(r.Exceeded)
			s.Total += r.Warnings + r.Exceeded
			counts["budget_notice"] += r.Warnings + r.Exceeded
		}
	}
	budgetItems, err := q.AttentionBudgetItems(ctx, now.Add(-AttentionBudgetWindow).UnixMicro())
	if err != nil {
		return nil, err
	}
	for _, r := range budgetItems {
		sev := "watch"
		if r.Level == "exceeded" {
			sev = "act"
		}
		add(authz.SpaceConfigRead, AttentionItem{Kind: "budget_notice", Severity: sev, ID: r.ID, SpaceID: r.SpaceID, Title: "Budget " + r.Level + ": " + r.Budget,
			Since: micros(r.Since), Target: AttentionTarget{Type: "space_budget", SpaceID: r.SpaceID}})
	}
	parked, err := q.AttentionParkedRuns(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range parked {
		s := get(r.SpaceID, authz.RunRead)
		if s == nil {
			continue
		}
		if s.RunsParked == nil {
			s.RunsParked = i64(r.Total)
			s.Total += r.Total
			counts["run_parked"] += r.Total
		}
		title := "Run parked at a usage limit: " + r.Role
		if r.Cause != "" {
			title = "Run parked for a restart: " + r.Role
		}
		add(authz.RunRead, AttentionItem{Kind: "run_parked", Severity: "watch", ID: r.ID, SpaceID: r.SpaceID, Title: title, Since: micros(r.Since),
			Target: AttentionTarget{Type: "run", ID: r.RunID, SpaceID: r.SpaceID}})
	}
	limits, err := q.AttentionAccountLimits(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range limits {
		s := get(r.SpaceID, authz.RunRead)
		if s == nil {
			continue
		}
		if s.AccountLimits == nil {
			s.AccountLimits = i64(r.Total)
			s.Total += r.Total
			counts["account_limit"] += r.Total
		}
		add(authz.RunRead, AttentionItem{Kind: "account_limit", Severity: "watch", ID: r.ID, SpaceID: r.SpaceID, Title: "Account limit reached: " + r.Kind,
			Since: micros(r.Since), Target: AttentionTarget{Type: "space_usage", SpaceID: r.SpaceID}})
	}
	switches, err := q.AttentionAccountSwitches(ctx, now.Add(-AttentionSwitchWindow).UnixMicro())
	if err != nil {
		return nil, err
	}
	for _, r := range switches {
		s := get(r.SpaceID, authz.RunRead)
		if s == nil {
			continue
		}
		if s.AccountSwitchFails == nil {
			s.AccountSwitchFails = i64(r.Total)
			s.Total += r.Total
			counts["account_switch_failed"] += r.Total
		}
		add(authz.RunRead, AttentionItem{Kind: "account_switch_failed", Severity: "act", ID: r.ID, SpaceID: r.SpaceID, Title: "Account switch failed: " + r.Role,
			Since: micros(r.Since), Target: AttentionTarget{Type: "run", ID: r.FromRunID, SpaceID: r.SpaceID}})
	}
	incoming, err := q.AttentionXSpaceIncoming(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range incoming {
		s := get(r.SpaceID, authz.XSpaceRead)
		if s == nil {
			continue
		}
		if s.XSpaceIncoming == nil {
			s.XSpaceIncoming = i64(r.Total)
			s.Total += r.Total
			counts["xspace_request"] += r.Total
		}
		add(authz.XSpaceRead, AttentionItem{Kind: "xspace_request", Severity: "act", ID: r.ID, SpaceID: r.SpaceID, Title: r.Title, Since: micros(r.Since),
			Target: AttentionTarget{Type: "xspace_request", ID: r.ID, SpaceID: r.SpaceID}})
	}
	bytes, err := q.AttentionAttachmentBytes(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range bytes {
		if s := get(r.SpaceID, authz.SpaceConfigRead); s != nil && src.StorageQuota != nil {
			quota := src.StorageQuota(ctx, r.SpaceID)
			s.StorageBytes, s.StorageQuota = i64(r.Bytes), i64(quota)
			if quota > 0 && r.Bytes*100 >= quota*AttentionQuotaPercent {
				s.StorageNear = true
				s.Total++
			}
		}
	}
	// Node connection state is in memory; every reader of a space learns whether its node is connected, and node:read
	// holders get the node list.
	var nodes []AttentionNode
	down := map[string]bool{}
	if src.Nodes != nil {
		if nodes, err = src.Nodes(ctx); err != nil {
			return nil, err
		}
		for _, n := range nodes {
			down[n.ID] = n.State == "active" && !n.Connected
		}
		for _, r := range refs {
			if s := byID[r.ID]; s != nil && down[r.NodeID] && r.State != "archived" {
				s.NodeDisconnected = true
				s.Total++
			}
		}
	}
	out := &Attention{Spaces: []AttentionSpace{}, Items: capItems(items), ItemCounts: counts, CheckedAt: now.UTC()}
	for _, id := range order {
		if s := byID[id]; s.Total > 0 || only != "" {
			out.Spaces = append(out.Spaces, *s)
			out.Total += s.Total
		}
	}
	slices.SortStableFunc(out.Spaces, func(a, b AttentionSpace) int { return int(b.Total - a.Total) })
	if only != "" {
		return out, nil
	}
	if can(authz.Global, authz.NodeRead) {
		for _, n := range nodes {
			if !down[n.ID] {
				continue
			}
			n.Spaces = []string{}
			for _, r := range refs {
				if r.NodeID == n.ID && byID[r.ID] != nil && r.State != "archived" {
					n.Spaces = append(n.Spaces, r.ID)
				}
			}
			out.Nodes = append(out.Nodes, n)
			if len(n.Spaces) == 0 { // otherwise already counted in its spaces
				out.Total++
			}
		}
	}
	if can(authz.Global, authz.SystemRead) {
		ob, err := q.AttentionOutbox(ctx)
		if err != nil {
			return nil, err
		}
		out.Outbox = &AttentionOutbox{Dead: ob.Dead, Retrying: ob.Retrying}
		out.Total += ob.Dead
		bk, err := q.AttentionBackups(ctx)
		if err != nil {
			return nil, err
		}
		out.Backups = backupsState(bk)
		if out.Backups.State == "failing" {
			out.Total++
		}
		out.Storage = src.storage(ctx)
		if out.Storage.Problem {
			out.Total++
		}
	}
	return out, nil
}

func backupsState(bk db.AttentionBackupsRow) *AttentionBackups {
	b := &AttentionBackups{State: "none", LastSucceededAt: microsTime(bk.LastSucceeded), LastFailedAt: microsTime(bk.LastFailed),
		LastVerifyFailed: microsTime(bk.LastVerifyFailed)}
	switch {
	case bk.LastFailed > bk.LastSucceeded || (bk.LastVerifyFailed > 0 && bk.LastVerifyFailed >= bk.LastSucceeded):
		b.State = "failing"
	case bk.LastSucceeded > 0:
		b.State = "ok"
	}
	return b
}

func (src *AttentionSources) storage(ctx context.Context) *AttentionStorage {
	st := &AttentionStorage{Readiness: string(health.StatusReady), Checks: []AttentionCheck{}, StoreState: "ok"}
	if src.Readiness != nil {
		r := src.Readiness(ctx)
		st.Readiness = string(r.Status)
		for _, c := range r.Checks {
			st.Checks = append(st.Checks, AttentionCheck{Name: c.Name, Status: string(c.Status), Error: c.Error})
		}
	}
	if src.StoreHealth != nil {
		st.StoreState, st.StoreReason = src.StoreHealth()
	}
	if src.DataDir != "" {
		u, err := fsstat.Stat(src.DataDir)
		if err != nil {
			st.DiskError = "unavailable"
		} else {
			st.DiskTotal, st.DiskAvailable, st.DiskUsedPct = clampBytes(u.Total), clampBytes(u.Avail), u.UsedPercent()
			st.DiskReserve = clampBytes(u.Reserve(src.MinFreeBytes, src.MinFreePct))
			st.BelowReserve = u.BelowReserve(src.MinFreeBytes, src.MinFreePct)
		}
	}
	st.Problem = st.Readiness != string(health.StatusReady) || st.StoreState != "ok" || st.BelowReserve || st.DiskError != ""
	return st
}

// clampBytes converts a filesystem size to int64 (sizes beyond 8 EiB are capped).
func clampBytes(v uint64) int64 {
	if v > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(v)
}
