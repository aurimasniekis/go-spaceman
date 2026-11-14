// Package limits is SpaceMan's one limits model (SM-LIMIT-001…003, ADR-0036). Every limit has a bounded installation
// default (config `limits:`), an optional per-space override stored here (versioned, If-Match), and an enforcement point
// in the subsystem that owns the work. The effective value of each limit and where it comes from is one read.
//
// Lowering a limit never stops running work: it only refuses new work beyond it. The single exception is a space's
// explicit on_lower=stop policy for the run concurrency limit, which stops the newest runs beyond the new limit.
// Retention periods (retention_*_days, ADR-0045) are limits too, but shortening one deletes data in the next daily
// retention pass (internal/retention); changing them needs retention:write.
//
// Budgets are usage-based: usage is reported by harnesses, late or not at all, so a budget warns and refuses new runs
// once the reported usage passes it. It is never a hard currency limit, and unknown usage is never counted as zero.
package limits

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/config"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Limit keys.
const (
	ConcurrentRuns     = "max_concurrent_runs"
	TopLevelTasks      = "max_top_level_tasks"
	KBSessions         = "max_kb_sessions"
	SupportDepth       = "support_depth"
	HumanPending       = "max_human_pending"
	HumanPendingPerRun = "max_human_pending_per_run"
	ToolCallsPerRun    = "max_tool_calls_per_run"
	IngestPerMinute    = "ingest_per_minute"
	EventDepth         = "event_depth"
	TaskDeadline       = "task_deadline_minutes"
	StorageQuota       = "storage_quota_bytes"
	BudgetTokens       = "budget_tokens_24h"
	BudgetCost         = "budget_cost_cents_24h"
	BudgetTokensPerRun = "budget_tokens_per_run" //nolint:gosec // G101 false positive: a limit key, not a credential
	BudgetWarnPercent  = "budget_warn_percent"
	// Retention periods (days; 0 keeps) and storage bounds (ADR-0045). The daily retention pass applies them.
	RetainRecordings   = "retention_recordings_days"
	RetainTranscripts  = "retention_transcripts_days"
	RetainAttachments  = "retention_attachments_days"
	RetainHumanActions = "retention_human_actions_days"
	RecordingQuota     = "recording_quota_bytes"
	ExportMax          = "export_max_bytes"
	// Control lifecycle (ADR-0056).
	ControlAutostart = "control_autostart"
	ControlIdleStop  = "control_idle_stop_minutes"
	// A new run's terminal size (ADR-0075).
	TerminalCols = "terminal_cols"
	TerminalRows = "terminal_rows"
	// Usage limits (ADR-0079): failover to another account of the harness, and how long a run waits for a reset with
	// its process kept alive (also the cap of the probe backoff of a hold whose reset is unknown).
	UsageLimitFailover     = "usage_limit_failover"
	UsageLimitKeepAliveMax = "usage_limit_keep_alive_max_minutes"
)

// RetentionKeys are the limits that delete data in the daily retention pass.
var RetentionKeys = []string{RetainRecordings, RetainTranscripts, RetainAttachments, RetainHumanActions}

// TerminalSizeKeys are the limits that set a new run's held terminal size (ADR-0075).
var TerminalSizeKeys = []string{TerminalCols, TerminalRows}

const retentionLower = "The next daily retention pass deletes what is older than the new period; deleted data is not recovered (restore a backup)."

// On-lower policies of the run concurrency limit.
const (
	Drain = "drain" // runs beyond a lowered limit finish; no new ones start until the space is below it (default)
	Stop  = "stop"  // the newest runs beyond a lowered limit are stopped (interrupt, then kill after the grace)
)

// Sources of an effective value.
const (
	SourceInstallation = "installation"
	SourceSpace        = "space"
	SourceQueuePolicy  = "queue_policy"
)

// Def describes one limit: its bounds, unit, what it bounds, where it is enforced and what lowering it does.
type Def struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Unit        string `json:"unit" enum:"count,bytes,per_minute,minutes,days,tokens,usd_cents,percent,depth,switch,columns,rows" doc:"switch: 1 on, 0 off"`
	Scope       string `json:"scope" doc:"What one limit value bounds"`
	Min         int64  `json:"min"`
	Max         int64  `json:"max"`
	ZeroNone    bool   `json:"zero_means_none" doc:"0 means no limit"`
	ConfigKey   string `json:"config_key" doc:"The installation default in the configuration file"`
	Enforced    string `json:"enforced" doc:"Where SpaceMan enforces it"`
	OnLower     string `json:"on_lower" doc:"What happens to running work when it is lowered"`
	Overridable bool   `json:"overridable" doc:"Spaces override it here; otherwise where it is set per space is named in edit_at"`
	EditAt      string `json:"edit_at,omitempty" enum:",queue_policy" doc:"Where the space value is edited when it is not overridable here"`
}

const neverStops = "Running work continues; only new work beyond the limit is refused."

// Defs is the limits model, in display order.
var Defs = []Def{
	{Key: ConcurrentRuns, Label: "Concurrent runs", Unit: "count", Scope: "worker and diagnostic runs active in the space at once",
		Enforced: "run launch (API, control, UI)", OnLower: "Follows the space's on_lower policy: drain (default) lets runs finish; stop stops the newest runs beyond the limit.", Overridable: true},
	{Key: TopLevelTasks, Label: "Running top-level tasks", Unit: "count", Scope: "top-level queue tasks running at once",
		Enforced: "queue selection", OnLower: "Running tasks continue; no further task is selected until the space is below the limit.", EditAt: SourceQueuePolicy},
	{Key: KBSessions, Label: "KB support sessions", Unit: "count", Scope: "active KB support sessions in the space, across its knowledge bases (each KB also has its own max_sessions)",
		Enforced: "KB scheduler", OnLower: "Active sessions continue until they sleep; further requests queue.", Overridable: true},
	{Key: SupportDepth, Label: "Support depth", Unit: "depth", Scope: "how deep support may nest (1: a KB session cannot ask a knowledge base)",
		Enforced: "KB ask tool", OnLower: "Sessions already started continue; deeper requests are refused.", Overridable: true},
	{Key: HumanPending, Label: "Pending human actions", Unit: "count", Scope: "open human actions in the space",
		Enforced: "human-action creation", OnLower: "Open actions stay; new ones are refused until some are resolved.", Overridable: true},
	{Key: HumanPendingPerRun, Label: "Pending human actions per run", Unit: "count", Scope: "open human actions one agent run asked",
		Enforced: "human-action creation (agent tools)", OnLower: neverStops, Overridable: true},
	{Key: ToolCallsPerRun, Label: "Tool calls per run", Unit: "count", Scope: "SpaceMan and MCP gateway tool calls of one run", ZeroNone: true,
		Enforced: "agent tool gateway", OnLower: "The run keeps running; its further tool calls are refused.", Overridable: true},
	{Key: IngestPerMinute, Label: "Event ingestion rate", Unit: "per_minute", Scope: "events one subscription ingests per minute; the rest waits in the stream",
		Enforced: "event ingestion manager", OnLower: "Nothing is dropped: events wait in the durable stream.", Overridable: true},
	{Key: EventDepth, Label: "Event automation depth", Unit: "depth", Scope: "SpaceMan ingestions an inbound event may already have passed through (its spaceman-depth header); deeper events are quarantined to stop loops between installations",
		Enforced: "event ingestion (before the inbox item)", OnLower: "Items already created stay; deeper events from then on are quarantined, where they can be reprocessed.", Overridable: true},
	{Key: TaskDeadline, Label: "Task deadline", Unit: "minutes", Scope: "how long a top-level task may run before control and the operators are told", ZeroNone: true,
		Enforced: "deadline timer (notifies, never stops)", OnLower: "Tasks past the new deadline are reported; none is stopped.", Overridable: true},
	{Key: StorageQuota, Label: "Attachment storage", Unit: "bytes", Scope: "attachment bytes stored for the space",
		Enforced: "attachment upload and event payload retention", OnLower: "Stored files stay; new uploads are refused while the space is over it.", Overridable: true},
	{Key: BudgetTokens, Label: "Token budget (24 h)", Unit: "tokens", Scope: "reported input and output tokens of runs started in the last 24 hours", ZeroNone: true,
		Enforced: "run launch (usage-based, reported late)", OnLower: "Running runs continue; new runs are refused while reported usage is over it.", Overridable: true},
	{Key: BudgetCost, Label: "Estimated cost budget (24 h)", Unit: "usd_cents", Scope: "harness cost estimates of runs started in the last 24 hours", ZeroNone: true,
		Enforced: "run launch (estimates, reported late)", OnLower: "Running runs continue; new runs are refused while estimates are over it.", Overridable: true},
	{Key: BudgetTokensPerRun, Label: "Token budget per run", Unit: "tokens", Scope: "reported tokens of one run", ZeroNone: true,
		Enforced: "usage reports (warning only)", OnLower: "Never stops a run; operators are warned once per run.", Overridable: true},
	{Key: BudgetWarnPercent, Label: "Budget warning", Unit: "percent", Scope: "share of a budget at which operators are warned",
		Enforced: "usage reports", OnLower: "Only changes when the warning is sent.", Overridable: true},
	{Key: RecordingQuota, Label: "Terminal recording size", Unit: "bytes", Scope: "compressed size of one run's terminal recording on the node",
		Enforced: "the node's recorder, from the value sent at launch", OnLower: "Runs already started keep the size they started with; new runs get the new size.", Overridable: true},
	{Key: RetainRecordings, Label: "Recording retention", Unit: "days", Scope: "days central keeps a run's terminal recording after the run ended", ZeroNone: true,
		Enforced: "daily retention pass (never while the run is active)", OnLower: retentionLower, Overridable: true},
	{Key: RetainTranscripts, Label: "Transcript retention", Unit: "days", Scope: "days central keeps a run's stored native transcript", ZeroNone: true,
		Enforced: "daily retention pass", OnLower: retentionLower, Overridable: true},
	{Key: RetainAttachments, Label: "Attachment retention", Unit: "days", Scope: "days an attachment is kept once every item using it is finished; never deleted while an open item uses it", ZeroNone: true,
		Enforced: "daily retention pass", OnLower: retentionLower, Overridable: true},
	{Key: RetainHumanActions, Label: "Finished human-action retention", Unit: "days", Scope: "days a resolved, cancelled or expired human action is kept; kept while an open task, follow-up or undelivered response refers to it", ZeroNone: true,
		Enforced: "daily retention pass", OnLower: retentionLower, Overridable: true},
	{Key: ExportMax, Label: "Export size", Unit: "bytes", Scope: "size of one space export archive",
		Enforced: "space export (refused before it starts when larger)", OnLower: "Only later exports are refused.", Overridable: true},
	{Key: ControlAutostart, Label: "Start control on new work", Unit: "switch",
		Scope:    "launch the space's control role when work arrives for control (an inbox item, a task to decide on, a worker that ended) and no control run is live; it resumes the last control's session",
		Enforced: "control supervisor (skipped while paused, and when the launch is refused: no control role, account or budget; at most 6 starts an hour)",
		OnLower:  "Turning it off never stops a running control; control is no longer started for new work.", Overridable: true},
	{Key: ControlIdleStop, Label: "Stop idle control", Unit: "minutes",
		Scope: "how long control may sit idle (turn finished, nothing waiting for it, no operator at its terminal) before SpaceMan stops it; with control started on new work, the next event starts it again", ZeroNone: true,
		Enforced: "control supervisor (idle timer)", OnLower: "Applies from control's next idle point.", Overridable: true},
	{Key: TerminalCols, Label: "Terminal columns", Unit: "columns",
		Scope: "the width new runs' terminals are held at (40-400; 0: the node's default size); applies only with terminal rows set too", ZeroNone: true,
		Enforced: "run launch (the node holds the size)", OnLower: terminalLower, Overridable: true},
	{Key: TerminalRows, Label: "Terminal rows", Unit: "rows",
		Scope: "the height new runs' terminals are held at (10-200; 0: the node's default size); applies only with terminal columns set too", ZeroNone: true,
		Enforced: "run launch (the node holds the size)", OnLower: terminalLower, Overridable: true},
	{Key: UsageLimitFailover, Label: "Usage-limit failover", Unit: "switch",
		Scope:    "when the account a launch or resume would use is held at a usage limit, use another account bound to the space for the same harness that is not held (a space routed through an LLM gateway never falls back to a direct account)",
		Enforced: "run launch (account resolution)", OnLower: "Turning it off never stops a run; later launches on a held account are refused until its reset.", Overridable: true},
	{Key: UsageLimitKeepAliveMax, Label: "Usage-limit keep-alive", Unit: "minutes",
		Scope:    "how long a run at a usage limit waits for the reset with its process kept alive; also the longest gap between checks of a hold whose reset time is unknown",
		Enforced: "usage-limit holds (probe backoff)", OnLower: "Applies from the next check of a hold.", Overridable: true},
}

const terminalLower = "Runs already started keep their size; an operator with terminal takeover can resize a live run's terminal."

// TerminalSize returns the size a new run's terminal is held at from a space's effective limits; ok is false when
// either is 0 (the node's default size applies).
func (v Values) TerminalSize() (cols, rows int, ok bool) {
	c, r := v.Get(TerminalCols), v.Get(TerminalRows)
	if c == 0 || r == 0 {
		return 0, 0, false
	}
	return int(c), int(r), true
}

func init() {
	for i := range Defs {
		r, ok := config.LimitRanges[Defs[i].Key]
		if !ok {
			panic("limits: no range for " + Defs[i].Key)
		}
		Defs[i].Min, Defs[i].Max = r[0], r[1]
		Defs[i].ConfigKey = "limits." + Defs[i].Key
		if Defs[i].Key == StorageQuota {
			Defs[i].ConfigKey = "artifacts.space_quota_bytes"
		}
	}
}

func def(key string) (Def, bool) {
	i := slices.IndexFunc(Defs, func(d Def) bool { return d.Key == key })
	if i < 0 {
		return Def{}, false
	}
	return Defs[i], true
}

// Values are the effective limits of one space.
type Values struct {
	vals    map[string]int64
	OnLower string
}

// Get returns a limit's effective value.
func (v Values) Get(key string) int64 { return v.vals[key] }

// Service stores space overrides and computes effective limits.
type Service struct {
	s        *store.Store
	audit    *audit.Writer
	changes  *changefeed.Recorder
	defaults map[string]int64
	clock    clock.Clock
	log      *slog.Logger
	// Stop stops a run (on_lower=stop); nil: the policy cannot stop runs and behaves like drain.
	Stop func(ctx context.Context, actor audit.Actor, runID, reason string) error
	// Notify sends a notification to a space's operators (and to control when toControl), deduplicated by key.
	Notify func(ctx context.Context, spaceID, subject, body, key string, toControl bool) error
	// OnChange is called after a commit that changed limits (wakes the deadline timer).
	OnChange func()
	// AuditDays is the installation's audit retention (config retention.audit_days; 0 keeps), shown with the limits.
	AuditDays int64
}

// New returns the service with the installation defaults (already validated by the configuration).
func New(s *store.Store, aw *audit.Writer, ch *changefeed.Recorder, defaults map[string]int64, c clock.Clock, log *slog.Logger) *Service {
	if c == nil {
		c = clock.System{}
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	d := map[string]int64{}
	maps.Copy(d, defaults)
	return &Service{s: s, audit: aw, changes: ch, defaults: d, clock: c, log: log}
}

// RetentionChanged reports whether replacing a space's overrides with in would change one of its retention overrides
// (set, cleared or a different value). The API requires retention:write for such a change.
func (svc *Service) RetentionChanged(ctx context.Context, spaceID string, in Input) (bool, error) {
	return svc.KeysChanged(ctx, spaceID, in, RetentionKeys)
}

// TerminalSizeChanged reports whether replacing a space's overrides with in would change its new-run terminal size
// override. A new run is held at that size, which other people see: the API requires terminal:takeover (ADR-0075).
func (svc *Service) TerminalSizeChanged(ctx context.Context, spaceID string, in Input) (bool, error) {
	return svc.KeysChanged(ctx, spaceID, in, TerminalSizeKeys)
}

// KeysChanged reports whether replacing a space's overrides with in would set, clear or change one of keys' overrides.
func (svc *Service) KeysChanged(ctx context.Context, spaceID string, in Input, keys []string) (bool, error) {
	cur := map[string]int64{}
	row, err := svc.s.R().GetSpaceLimits(ctx, spaceID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return false, err
	default:
		cur = parseOverrides(row.ValuesJson)
	}
	for _, k := range keys {
		a, inCur := cur[k]
		b, inNew := in.Values[k]
		if inCur != inNew || a != b {
			return true, nil
		}
	}
	return false, nil
}

// Default returns a limit's installation default.
func (svc *Service) Default(key string) int64 { return svc.defaults[key] }

func parseOverrides(js string) map[string]int64 {
	out := map[string]int64{}
	_ = json.Unmarshal([]byte(js), &out)
	return out
}

// For returns the effective limits of a space, read with q (inside a transaction, or the read pool).
func (svc *Service) For(ctx context.Context, q *db.Queries, spaceID string) (Values, error) {
	v := Values{vals: map[string]int64{}, OnLower: Drain}
	maps.Copy(v.vals, svc.defaults)
	row, err := q.GetSpaceLimits(ctx, spaceID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return v, err
	default:
		for k, n := range parseOverrides(row.ValuesJson) {
			if d, ok := def(k); ok && d.Overridable {
				v.vals[k] = n
			}
		}
		v.OnLower = row.OnLower
	}
	if p, err := q.GetQueuePolicy(ctx, spaceID); err == nil {
		v.vals[TopLevelTasks] = p.MaxActive
	}
	return v, nil
}

// Value returns one effective limit of a space from the read pool; on a storage error it returns the default.
func (svc *Service) Value(ctx context.Context, q *db.Queries, spaceID, key string) int64 {
	if q == nil {
		q = svc.s.R()
	}
	v, err := svc.For(ctx, q, spaceID)
	if err != nil {
		svc.log.Warn("read space limits", slog.String("space.id", spaceID), slog.Any("error", err))
		return svc.defaults[key]
	}
	return v.Get(key)
}

// Limit is one limit's effective value in a space.
type Limit struct {
	Def
	Value    int64  `json:"value"`
	Default  int64  `json:"default" doc:"The installation default"`
	Override *int64 `json:"override,omitempty" doc:"The space's own value, when it sets one"`
	Source   string `json:"source" enum:"installation,space,queue_policy"`
}

// Effective is the effective limits of a space with their sources.
type Effective struct {
	SpaceID   string     `json:"space_id"`
	Limits    []Limit    `json:"limits"`
	OnLower   string     `json:"on_lower" enum:"drain,stop" doc:"What lowering the concurrent-runs limit does to running runs"`
	Related   []Related  `json:"related" doc:"Limits set elsewhere (per knowledge base, per action type, per MCP server, per node)"`
	Budget    *Budget    `json:"budget"`
	Version   int64      `json:"version" doc:"Version of the space's overrides (0: none stored)"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
	UpdatedBy string     `json:"updated_by,omitempty"`
}

// Related is a limit owned by another configuration, shown with the effective limits.
type Related struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	Value  string `json:"value"`
	Source string `json:"source"`
	EditAt string `json:"edit_at" doc:"Where it is configured"`
}

// Effective returns the effective limits of a space, each with its source.
func (svc *Service) Effective(ctx context.Context, spaceID string) (*Effective, error) {
	q := svc.s.R()
	if _, err := q.GetSpace(ctx, spaceID); err != nil {
		return nil, apperr.New(apperr.CodeNotFound, "space not found")
	}
	out := &Effective{SpaceID: spaceID, OnLower: Drain, Limits: []Limit{}, Related: []Related{}}
	over := map[string]int64{}
	if row, err := q.GetSpaceLimits(ctx, spaceID); err == nil {
		over = parseOverrides(row.ValuesJson)
		out.OnLower, out.Version, out.UpdatedBy = row.OnLower, row.Version, row.UpdatedBy
		t := time.UnixMicro(row.UpdatedAt).UTC()
		out.UpdatedAt = &t
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	qp, qpErr := q.GetQueuePolicy(ctx, spaceID)
	for _, d := range Defs {
		l := Limit{Def: d, Default: svc.defaults[d.Key], Value: svc.defaults[d.Key], Source: SourceInstallation}
		if n, ok := over[d.Key]; ok && d.Overridable {
			l.Value, l.Source = n, SourceSpace
			l.Override = &n
		}
		if d.Key == TopLevelTasks && qpErr == nil {
			l.Value, l.Source = qp.MaxActive, SourceQueuePolicy
		}
		out.Limits = append(out.Limits, l)
	}
	out.Related = svc.related(ctx, q, spaceID)
	b, err := svc.budget(ctx, q, spaceID, out.values())
	if err != nil {
		return nil, err
	}
	out.Budget = b
	return out, nil
}

func (e *Effective) values() Values {
	v := Values{vals: map[string]int64{}, OnLower: e.OnLower}
	for _, l := range e.Limits {
		v.vals[l.Key] = l.Value
	}
	return v
}

func (svc *Service) related(ctx context.Context, q *db.Queries, spaceID string) []Related {
	out := []Related{}
	if sp, err := q.GetSpace(ctx, spaceID); err == nil {
		if n, err := q.GetNode(ctx, sp.NodeID); err == nil {
			v := "unknown until the node connects"
			if n.Capacity > 0 {
				v = fmt.Sprintf("%d worker and diagnostic runs", n.Capacity)
			}
			out = append(out, Related{Key: "node_capacity", Label: "Node capacity (" + n.Name + ")", Value: v, Source: "node",
				EditAt: "the node agent's capacity setting; shared by every space on the node"})
		}
	}
	if kbs, err := q.ListKBs(ctx, spaceID); err == nil {
		for _, k := range kbs {
			out = append(out, Related{Key: "kb_max_sessions:" + k.Slug, Label: "Sessions of knowledge base " + k.Slug, Value: fmt.Sprintf("%d", k.MaxSessions),
				Source: "knowledge base", EditAt: "Knowledge section"})
		}
	}
	out = append(out,
		Related{Key: "human_rates", Label: "Human-action rate limits", Value: "per action type", Source: "human-action policies", EditAt: "Human actions → Policies"},
		Related{Key: "mcp_server_limits", Label: "MCP gateway timeouts, concurrency and result size", Value: "per MCP server", Source: "installation", EditAt: "MCP servers"},
		Related{Key: "dream_budgets", Label: "Dream time, token and evidence budgets", Value: "per dream configuration", Source: "dreams", EditAt: "Memory → Dreams"},
	)
	audit := "kept forever"
	if svc.AuditDays > 0 {
		audit = fmt.Sprintf("%d days", svc.AuditDays)
	}
	out = append(out, Related{Key: "audit_retention", Label: "Audit retention", Value: audit, Source: "installation",
		EditAt: "the configuration file (retention.audit_days); the audit log is installation-wide"})
	return out
}

// Input replaces a space's overrides: keys left out inherit the installation default.
type Input struct {
	Values  map[string]int64 `json:"values" doc:"Overrides by limit key; a key left out inherits the installation default"`
	OnLower string           `json:"on_lower,omitempty" enum:"drain,stop" doc:"What lowering the concurrent-runs limit does to running runs (default drain)"`
}

// Set replaces a space's overrides. version is the stored version (If-Match); 0 is accepted only when none is stored.
// With on_lower=stop, lowering the concurrent-runs limit stops the newest runs beyond it after the commit.
func (svc *Service) Set(ctx context.Context, actor audit.Actor, spaceID string, version int64, in Input) (*Effective, error) {
	v := &apperr.Validation{}
	if in.OnLower == "" {
		in.OnLower = Drain
	}
	if in.OnLower != Drain && in.OnLower != Stop {
		v.Add("on_lower", "must be drain or stop")
	}
	if in.Values == nil {
		in.Values = map[string]int64{}
	}
	for _, k := range slices.Sorted(maps.Keys(in.Values)) {
		d, ok := def(k)
		switch {
		case !ok:
			v.Add("values."+k, "is not a limit")
		case !d.Overridable:
			v.Add("values."+k, "is set in the %s, not here", d.EditAt)
		case in.Values[k] < d.Min || in.Values[k] > d.Max:
			v.Add("values."+k, "must be within %d..%d", d.Min, d.Max)
		case in.Values[k] != 0 && in.Values[k] < config.TerminalLimitFloor[k]:
			v.Add("values."+k, "must be 0 (the node's default) or within %d..%d", config.TerminalLimitFloor[k], d.Max)
		}
	}
	if err := v.Err(); err != nil {
		return nil, err
	}
	js, _ := json.Marshal(in.Values)
	var lowered bool
	err := svc.s.W().TxNamed(ctx, "limits.set", func(tx *store.Tx) error {
		if _, err := tx.Q.GetSpace(ctx, spaceID); err != nil {
			return apperr.New(apperr.CodeNotFound, "space not found")
		}
		before, err := svc.For(ctx, tx.Q, spaceID)
		if err != nil {
			return err
		}
		row, err := tx.Q.GetSpaceLimits(ctx, spaceID)
		action, next := "created", int64(1)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if version != 0 {
				return apperr.New(apperr.CodePrecondition, "the space has no stored limits (version 0)")
			}
			err = tx.Q.InsertSpaceLimits(ctx, db.InsertSpaceLimitsParams{SpaceID: spaceID, ValuesJson: string(js), OnLower: in.OnLower,
				CreatedAt: tx.NowMicros(), CreatedBy: actorRef(actor), UpdatedAt: tx.NowMicros(), UpdatedBy: actorRef(actor)})
		case err != nil:
			return err
		default:
			if version != row.Version {
				return apperr.New(apperr.CodePrecondition, "the space's limits changed since version %d (now %d)", version, row.Version)
			}
			action, next = "updated", row.Version+1
			var n int64
			n, err = tx.Q.UpdateSpaceLimits(ctx, db.UpdateSpaceLimitsParams{ValuesJson: string(js), OnLower: in.OnLower, Now: tx.NowMicros(),
				By: actorRef(actor), SpaceID: spaceID, Version: version})
			if err == nil && n == 0 {
				err = apperr.New(apperr.CodePrecondition, "the space's limits changed concurrently")
			}
		}
		if err != nil {
			return err
		}
		after, err := svc.For(ctx, tx.Q, spaceID)
		if err != nil {
			return err
		}
		changed := map[string]any{}
		for _, d := range Defs {
			if a, b := before.Get(d.Key), after.Get(d.Key); a != b {
				changed[d.Key] = map[string]int64{"from": a, "to": b}
			}
		}
		lowered = after.Get(ConcurrentRuns) < before.Get(ConcurrentRuns)
		if err := svc.changes.Record(ctx, tx, spaceID, "space_limits", spaceID, action, next); err != nil {
			return err
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "space_limits.updated", Target: audit.Target{Type: "space", ID: spaceID},
			Details: map[string]any{"space_id": spaceID, "changed": changed, "on_lower": in.OnLower, "overrides": len(in.Values)}})
		return err
	})
	if err != nil {
		return nil, err
	}
	if svc.OnChange != nil {
		svc.OnChange()
	}
	if lowered && in.OnLower == Stop {
		svc.stopBeyond(ctx, actor, spaceID)
	}
	return svc.Effective(ctx, spaceID)
}

// stopBeyond stops the newest worker and diagnostic runs beyond the space's concurrency limit (on_lower=stop).
func (svc *Service) stopBeyond(ctx context.Context, actor audit.Actor, spaceID string) {
	if svc.Stop == nil {
		return
	}
	limit := svc.Value(ctx, nil, spaceID, ConcurrentRuns)
	runs, err := svc.s.R().ListActiveWorkRunsInSpace(ctx, spaceID)
	if err != nil {
		svc.log.Warn("list runs beyond a lowered limit", slog.String("space.id", spaceID), slog.Any("error", err))
		return
	}
	// Newest first: the oldest runs, which have done the most work, keep running.
	for i := 0; i < len(runs)-int(limit); i++ {
		if err := svc.Stop(ctx, actor, runs[i].ID, fmt.Sprintf("the space's concurrent-runs limit was lowered to %d (on_lower: stop)", limit)); err != nil {
			svc.log.Warn("stop a run beyond a lowered limit", slog.String("run.id", runs[i].ID), slog.Any("error", err))
		}
	}
}

func actorRef(a audit.Actor) string {
	if a.ID == "" {
		return a.Type
	}
	return a.Type + ":" + a.ID
}
