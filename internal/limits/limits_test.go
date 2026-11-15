package limits

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/config"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/keyring/keyringtest"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/storetest"
)

var admin = audit.Actor{Type: audit.ActorUser, ID: "usr_admin"}

type note struct {
	space, subject, body, key string
	control                   bool
}

type fx struct {
	t       *testing.T
	ctx     context.Context
	s       *store.Store
	clk     *clock.Fake
	svc     *Service
	mu      sync.Mutex
	stopped []string
	notes   []note
	runs    int
}

func newFx(t *testing.T) *fx {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC))
	s := storetest.Open(t, storetest.WithClock(clk))
	kr := keyringtest.Load(t, s)
	f := &fx{t: t, ctx: context.Background(), s: s, clk: clk}
	f.svc = New(s, audit.NewWriter(kr), changefeed.New(nil), config.Default().LimitValues(), clk, nil)
	f.svc.Stop = func(_ context.Context, _ audit.Actor, runID, _ string) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.stopped = append(f.stopped, runID)
		return nil
	}
	f.svc.Notify = func(_ context.Context, spaceID, subject, body, key string, toControl bool) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.notes = append(f.notes, note{spaceID, subject, body, key, toControl})
		return nil
	}
	f.exec(`INSERT INTO nodes (id, name, state, capacity, created_at, created_by, updated_at) VALUES ('nod_1', 'n1', 'active', 4, 0, 't', 0)`,
		`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('spc_a', 'aaa', 'A', 'nod_1', 'active', 'a', 0, 't', 0)`,
		`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('spc_b', 'bbb', 'B', 'nod_1', 'active', 'b', 0, 't', 0)`)
	return f
}

func (f *fx) exec(qs ...string) {
	f.t.Helper()
	must(f.t, f.s.W().TxNamed(f.ctx, "seed", func(tx *store.Tx) error {
		for _, q := range qs {
			if _, err := tx.SQL.ExecContext(f.ctx, q); err != nil {
				return fmt.Errorf("%s: %w", q, err)
			}
		}
		return nil
	}))
}

// run inserts an active run of a kind; it returns its ID.
func (f *fx) run(space, kind string) string {
	f.t.Helper()
	return f.runIn(space, kind, "running", "running")
}

// runIn inserts a run of a kind with the given desired and observed states; it returns its ID.
func (f *fx) runIn(space, kind, desired, observed string) string {
	f.t.Helper()
	f.runs++
	id := fmt.Sprintf("run_%026d", f.runs)
	f.exec(fmt.Sprintf(`INSERT INTO runs (id, space_id, node_id, generation, kind, role, harness, instance_id, desired, observed, launch_json, created_at, created_by, updated_at)
		VALUES ('%s', '%s', 'nod_1', 1, '%s', '%s', 'claude-code', 'ins_%d', '%s', '%s', '{}', %d, 't', 0)`, id, space, kind, kind, f.runs, desired, observed,
		f.clk.Now().Add(time.Duration(f.runs)*time.Second).UnixMicro()))
	return id
}

func (f *fx) admit(space, kind string) error {
	f.t.Helper()
	return f.s.W().TxNamed(f.ctx, "admit", func(tx *store.Tx) error {
		return f.svc.Admit(f.ctx, tx, fleet.Admission{SpaceID: space, NodeID: "nod_1", Kind: kind, Role: kind, Actor: admin})
	})
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func limitOf(e *Effective, key string) Limit {
	for _, l := range e.Limits {
		if l.Key == key {
			return l
		}
	}
	return Limit{}
}

// SM-LIMIT-001: one model of installation defaults and space overrides, with a single effective read that names each
// value's source; bounds are validated, edits are versioned, and limits owned elsewhere are shown where they are set.
func TestEffectiveLimitsAndOverrides(t *testing.T) {
	f := newFx(t)
	e, err := f.svc.Effective(f.ctx, "spc_a")
	must(t, err)
	if len(e.Limits) != len(Defs) || e.Version != 0 || e.OnLower != Drain {
		t.Fatalf("effective %+v", e)
	}
	for _, l := range e.Limits {
		if l.Source != SourceInstallation || l.Value != l.Default || l.Enforced == "" || l.OnLower == "" || l.ConfigKey == "" {
			t.Fatalf("default limit %+v", l)
		}
	}
	if l := limitOf(e, StorageQuota); l.ConfigKey != "artifacts.space_quota_bytes" || l.Value != 1<<30 {
		t.Fatalf("storage quota %+v", l)
	}
	if e.Budget == nil || e.Budget.State != BudgetNone || !strings.Contains(e.Budget.Note, "not a hard currency limit") {
		t.Fatalf("budget %+v", e.Budget)
	}
	if len(e.Related) == 0 || e.Related[0].Key != "node_capacity" || !strings.Contains(e.Related[0].Value, "4") {
		t.Fatalf("related %+v", e.Related)
	}
	// Validation: unknown keys, keys owned elsewhere, bounds and the on-lower policy.
	for name, in := range map[string]Input{
		"unknown":   {Values: map[string]int64{"max_magic": 1}},
		"elsewhere": {Values: map[string]int64{TopLevelTasks: 2}},
		"too low":   {Values: map[string]int64{ConcurrentRuns: 0}},
		"too high":  {Values: map[string]int64{SupportDepth: 9}},
		"policy":    {OnLower: "kill"},
	} {
		if _, err := f.svc.Set(f.ctx, admin, "spc_a", 0, in); apperr.CodeOf(err) != apperr.CodeInvalid {
			t.Fatalf("%s: %v", name, err)
		}
	}
	e, err = f.svc.Set(f.ctx, admin, "spc_a", 0, Input{Values: map[string]int64{ConcurrentRuns: 3, ToolCallsPerRun: 0}})
	must(t, err)
	if l := limitOf(e, ConcurrentRuns); l.Value != 3 || l.Source != SourceSpace || *l.Override != 3 || l.Default != 8 || e.Version != 1 {
		t.Fatalf("override %+v version %d", l, e.Version)
	}
	if l := limitOf(e, ToolCallsPerRun); l.Value != 0 || l.Source != SourceSpace || !l.ZeroNone {
		t.Fatalf("zero override %+v", l)
	}
	// Versioned: a stale version (or creating again) is refused; the other space is untouched.
	if _, err := f.svc.Set(f.ctx, admin, "spc_a", 0, Input{}); apperr.CodeOf(err) != apperr.CodePrecondition {
		t.Fatalf("stale version: %v", err)
	}
	if b, _ := f.svc.Effective(f.ctx, "spc_b"); limitOf(b, ConcurrentRuns).Value != 8 {
		t.Fatal("override leaked to another space")
	}
	// Replacing the overrides drops the keys left out: they inherit again.
	e, err = f.svc.Set(f.ctx, admin, "spc_a", 1, Input{Values: map[string]int64{HumanPending: 40}})
	must(t, err)
	if limitOf(e, ConcurrentRuns).Source != SourceInstallation || limitOf(e, HumanPending).Value != 40 || e.Version != 2 {
		t.Fatalf("replace %+v", e)
	}
	// The queue policy owns the top-level task limit.
	f.exec(`INSERT INTO queue_policies (space_id, max_active, incident_insertion, incident_preempts, escalation_reorders, updated_at, updated_by) VALUES ('spc_a', 3, 'top', 0, 0, 0, 'u')`)
	e, _ = f.svc.Effective(f.ctx, "spc_a")
	if l := limitOf(e, TopLevelTasks); l.Value != 3 || l.Source != SourceQueuePolicy || l.EditAt != SourceQueuePolicy {
		t.Fatalf("queue policy %+v", l)
	}
	if _, err := f.svc.Effective(f.ctx, "spc_missing"); apperr.CodeOf(err) != apperr.CodeNotFound {
		t.Fatalf("missing space: %v", err)
	}
	var n int
	must(t, f.s.ReadDB().QueryRowContext(f.ctx, `SELECT count(*) FROM audit_events WHERE action = 'space_limits.updated'`).Scan(&n))
	if n != 2 {
		t.Fatalf("audited %d", n)
	}
}

// SM-LIMIT-001/002: workers and diagnostics share the space's concurrency limit and the node's capacity; control, KB
// support and dreamer runs have their own pools, so neither side can starve the other.
func TestAdmissionPoolsAndCapacity(t *testing.T) {
	f := newFx(t)
	_, err := f.svc.Set(f.ctx, admin, "spc_a", 0, Input{Values: map[string]int64{ConcurrentRuns: 2}})
	must(t, err)
	f.run("spc_a", "worker")
	f.run("spc_a", "diagnostic")
	if err := f.admit("spc_a", "worker"); apperr.CodeOf(err) != apperr.CodeRateLimited || !strings.Contains(err.Error(), "limit 2") {
		t.Fatalf("third worker: %v", err)
	}
	// Support and control are not starved by workers.
	for _, k := range []string{"kb", "control", "dreamer"} {
		if err := f.admit("spc_a", k); err != nil {
			t.Fatalf("%s refused while workers are at their limit: %v", k, err)
		}
	}
	// Many support sessions do not take worker slots either.
	for range 5 {
		f.run("spc_b", "kb")
	}
	if err := f.admit("spc_b", "worker"); err != nil {
		t.Fatalf("worker refused because of support sessions: %v", err)
	}
	// The node's capacity (4) is shared by every space on it.
	f.run("spc_b", "worker")
	f.run("spc_b", "worker")
	if err := f.admit("spc_b", "worker"); apperr.CodeOf(err) != apperr.CodeRateLimited || !strings.Contains(err.Error(), "capacity is 4") {
		t.Fatalf("node capacity: %v", err)
	}
	// Ended runs free their slot.
	f.exec(`UPDATE runs SET desired = 'stopped', observed = 'exited' WHERE space_id = 'spc_b' AND kind = 'worker'`)
	if err := f.admit("spc_b", "worker"); err != nil {
		t.Fatalf("after runs ended: %v", err)
	}
}

// SM-LIMIT-001: the space and node limits count only live runs, with the same predicate as spaceman_runs_list
// (fleet.Live). Runs that exited, failed or were lost (after a node agent restart, or orphaned with desired still
// "running") free their slot; the refusal names the runs it counts.
func TestAdmissionCountsOnlyLiveRuns(t *testing.T) {
	f := newFx(t)
	_, err := f.svc.Set(f.ctx, admin, "spc_a", 0, Input{Values: map[string]int64{ConcurrentRuns: 2}})
	must(t, err)
	// Ended runs whose desired state was never changed: the runs a node agent restart leaves behind (lost), a worker
	// that finished on its own (exited) and a launch that failed.
	for _, obs := range []string{"exited", "failed", "lost"} {
		f.runIn("spc_a", "worker", "running", obs)
		f.runIn("spc_a", "diagnostic", "stopped", obs)
	}
	// A control run is live but is not a worker.
	f.runIn("spc_a", "control", "running", "running")
	if err := f.admit("spc_a", "worker"); err != nil {
		t.Fatalf("ended runs held a slot: %v", err)
	}
	live := f.runIn("spc_a", "worker", "running", "running")
	gone := f.runIn("spc_a", "worker", "running", "unknown")
	err = f.admit("spc_a", "worker")
	if apperr.CodeOf(err) != apperr.CodeRateLimited {
		t.Fatalf("at the limit: %v", err)
	}
	msg := err.Error()
	for _, want := range []string{"2 worker and diagnostic runs active (limit 2)", live + " (worker worker, running)", gone + " (worker worker, unknown)",
		"control, KB support and dreamer runs", "exited, failed or were lost, do not"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("refusal %q lacks %q", msg, want)
		}
	}
	// Once the unknown run is reconciled to lost, its slot is free.
	f.exec(`UPDATE runs SET observed = 'lost' WHERE id = '` + gone + `'`)
	if err := f.admit("spc_a", "worker"); err != nil {
		t.Fatalf("after the stale run was lost: %v", err)
	}

	// The node's capacity (4) uses the same predicate; other spaces' runs are counted but not named.
	other := []string{f.runIn("spc_b", "worker", "running", "starting"), f.runIn("spc_b", "worker", "running", "stopping")}
	f.runIn("spc_b", "worker", "running", "lost")
	f.runIn("spc_b", "worker", "running", "exited")
	pending := f.runIn("spc_a", "diagnostic", "running", "pending")
	_, err = f.svc.Set(f.ctx, admin, "spc_a", 1, Input{Values: map[string]int64{ConcurrentRuns: 8}})
	must(t, err)
	err = f.admit("spc_a", "worker")
	if apperr.CodeOf(err) != apperr.CodeRateLimited || !strings.Contains(err.Error(), "capacity is 4") {
		t.Fatalf("node capacity: %v", err)
	}
	msg = err.Error()
	for _, want := range []string{"runs 4 worker and diagnostic runs", "2 of other spaces and 2 of this space", live, pending + " (diagnostic diagnostic, pending)"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("node refusal %q lacks %q", msg, want)
		}
	}
	for _, id := range other {
		if strings.Contains(msg, id) {
			t.Fatalf("node refusal names another space's run %s: %q", id, msg)
		}
	}
}

// The SQL behind the limits and fleet.Live (spaceman_runs_list) agree on every observed and desired state, with one
// rule on top (ADR-0079, Q4): a live run waiting for its usage limit to reset is left out of the space's count but kept
// in the node's.
func TestLiveRunQueriesMatchRunsList(t *testing.T) {
	f := newFx(t)
	want, waiting := 0, map[string]bool{}
	for _, obs := range []string{"pending", "starting", "running", "stopping", "unknown", "exited", "failed", "lost"} {
		for _, desired := range []string{"running", "stopped"} {
			f.runIn("spc_a", "worker", desired, obs)
			if fleet.Live(obs) {
				want++
			}
			// The same states again, each at a usage limit.
			id := f.runIn("spc_a", "worker", desired, obs)
			f.exec(`UPDATE runs SET limit_kind = 'five_hour', limit_since = 1 WHERE id = '` + id + `'`)
			if fleet.Live(obs) {
				waiting[id] = true
			}
		}
	}
	inSpace, err := f.s.R().ListLiveWorkRunsInSpace(f.ctx, "spc_a")
	must(t, err)
	onNode, err := f.s.R().ListLiveWorkRunsOnNode(f.ctx, "nod_1")
	must(t, err)
	if len(inSpace) != want || len(onNode) != want+len(waiting) || want != 10 || len(waiting) != 10 {
		t.Fatalf("live runs: space %d, node %d, fleet.Live %d, limit-waiting %d (want 10, 20, 10, 10)", len(inSpace), len(onNode), want, len(waiting))
	}
	for _, r := range inSpace {
		if !fleet.Live(r.Observed) || waiting[r.ID] {
			t.Fatalf("space counted %s (%s, limit-waiting %v)", r.ID, r.Observed, waiting[r.ID])
		}
	}
	for _, r := range onNode {
		if !fleet.Live(r.Observed) {
			t.Fatalf("node counted a %s run", r.Observed)
		}
	}
}

// ADR-0079 decision 3 (Q4): a run kept alive at its usage limit frees its slot in the space's concurrency limit, but
// still uses the node's capacity, since its process stays up. The space refusal says so.
func TestLimitWaitingRunFreesSpaceSlotNotNode(t *testing.T) {
	f := newFx(t)
	_, err := f.svc.Set(f.ctx, admin, "spc_a", 0, Input{Values: map[string]int64{ConcurrentRuns: 1}})
	must(t, err)
	limited := f.run("spc_a", "worker")
	err = f.admit("spc_a", "worker")
	if apperr.CodeOf(err) != apperr.CodeRateLimited || !strings.Contains(err.Error(), "waiting for a usage limit to reset") {
		t.Fatalf("at the space limit: %v", err)
	}
	f.exec(`UPDATE runs SET limit_kind = 'five_hour', limit_since = 1 WHERE id = '` + limited + `'`)
	if err := f.admit("spc_a", "worker"); err != nil {
		t.Fatalf("a limit-waiting run held its space slot: %v", err)
	}
	// The node's capacity (4) still counts it: three more runs fill the node, and the refusal names it.
	_, err = f.svc.Set(f.ctx, admin, "spc_a", 1, Input{Values: map[string]int64{ConcurrentRuns: 8}})
	must(t, err)
	f.run("spc_a", "worker")
	f.run("spc_b", "worker")
	f.run("spc_b", "worker")
	err = f.admit("spc_a", "worker")
	if apperr.CodeOf(err) != apperr.CodeRateLimited || !strings.Contains(err.Error(), "capacity is 4") || !strings.Contains(err.Error(), limited) {
		t.Fatalf("node capacity with a limit-waiting run: %v", err)
	}
	// Once the limit clears, the run counts in the space again.
	f.exec(`UPDATE runs SET limit_kind = '', limit_since = NULL WHERE id = '`+limited+`'`,
		`UPDATE runs SET observed = 'exited' WHERE space_id = 'spc_b'`)
	_, err = f.svc.Set(f.ctx, admin, "spc_a", 2, Input{Values: map[string]int64{ConcurrentRuns: 2}})
	must(t, err)
	if err := f.admit("spc_a", "worker"); apperr.CodeOf(err) != apperr.CodeRateLimited || !strings.Contains(err.Error(), limited) {
		t.Fatalf("after the clear: %v", err)
	}
}

// SM-LIMIT-002: lowering the concurrency limit never stops running work by default (drain); only an explicit
// on_lower=stop stops the newest runs beyond the new limit.
func TestLoweringNeverStopsUnlessPolicySays(t *testing.T) {
	f := newFx(t)
	a, b, c := f.run("spc_a", "worker"), f.run("spc_a", "worker"), f.run("spc_a", "worker")
	_, err := f.svc.Set(f.ctx, admin, "spc_a", 0, Input{Values: map[string]int64{ConcurrentRuns: 1}})
	must(t, err)
	if len(f.stopped) != 0 {
		t.Fatalf("drain stopped %v", f.stopped)
	}
	if err := f.admit("spc_a", "worker"); apperr.CodeOf(err) != apperr.CodeRateLimited {
		t.Fatalf("new work beyond a lowered limit: %v", err)
	}
	// Raising it (with stop) stops nothing; lowering with stop stops the newest beyond it.
	_, err = f.svc.Set(f.ctx, admin, "spc_a", 1, Input{Values: map[string]int64{ConcurrentRuns: 3}, OnLower: Stop})
	must(t, err)
	if len(f.stopped) != 0 {
		t.Fatalf("raising stopped %v", f.stopped)
	}
	_, err = f.svc.Set(f.ctx, admin, "spc_a", 2, Input{Values: map[string]int64{ConcurrentRuns: 1}, OnLower: Stop})
	must(t, err)
	if len(f.stopped) != 2 || f.stopped[0] != c || f.stopped[1] != b {
		t.Fatalf("stop policy stopped %v (want %s, %s; keep %s)", f.stopped, c, b, a)
	}
	// Other limits never stop anything when lowered.
	f.stopped = nil
	_, err = f.svc.Set(f.ctx, admin, "spc_a", 3, Input{Values: map[string]int64{ConcurrentRuns: 1, HumanPending: 1, KBSessions: 1, ToolCallsPerRun: 1}, OnLower: Stop})
	must(t, err)
	if len(f.stopped) != 0 {
		t.Fatalf("lowering other limits stopped %v", f.stopped)
	}
}

func (f *fx) usage(runID, space string, tokens int64, usd float64) {
	f.t.Helper()
	m, _ := json.Marshal([]proto.ModelUsage{{Model: "m", Input: &tokens, Output: new(int64), Requests: 1}})
	est := ""
	if usd > 0 {
		b, _ := json.Marshal(proto.CostEstimate{USD: usd, Source: "harness", Assumptions: "list prices"})
		est = string(b)
	}
	f.exec(fmt.Sprintf(`INSERT INTO run_usage (run_id, space_id, source, models_json, estimate_json, updated_at) VALUES ('%s', '%s', '%s', '%s', '%s', 0)
		ON CONFLICT (run_id) DO UPDATE SET models_json = excluded.models_json, estimate_json = excluded.estimate_json`, runID, space, proto.UsageClaudeTranscript, m, est))
}

// SM-LIMIT-001/003: budgets use reported usage, warn once at the warning share, stop new runs when passed, never stop
// running ones, never count unknown usage, and never claim a hard currency limit.
func TestBudgetsWarnAndStopAdmission(t *testing.T) {
	f := newFx(t)
	_, err := f.svc.Set(f.ctx, admin, "spc_a", 0, Input{Values: map[string]int64{BudgetTokens: 1000, BudgetCost: 500, BudgetTokensPerRun: 1000}})
	must(t, err)
	r1 := f.run("spc_a", "worker")
	f.run("spc_a", "worker") // unknown usage: not counted, not zero
	f.usage(r1, "spc_a", 850, 1.25)
	f.svc.OnUsage(f.ctx, r1)
	e, _ := f.svc.Effective(f.ctx, "spc_a")
	if b := e.Budget; b.State != BudgetWarning || b.Tokens != 850 || b.CostCents != 125 || b.UnknownRuns != 1 || b.Runs != 2 || len(b.Over) != 1 {
		t.Fatalf("budget %+v", b)
	}
	if len(f.notes) != 2 || f.notes[0].subject != "Budget warning" || !strings.Contains(f.notes[0].body, "per-run budget is 1000") || !strings.Contains(f.notes[1].body, "not a hard currency limit") {
		t.Fatalf("notes %+v", f.notes)
	}
	if err := f.admit("spc_a", "worker"); err != nil {
		t.Fatalf("a warning refused a launch: %v", err)
	}
	// A second report at the same level sends nothing new.
	f.svc.OnUsage(f.ctx, r1)
	if len(f.notes) != 2 {
		t.Fatalf("repeated warning: %+v", f.notes)
	}
	// Past the budget: new runs of every kind are refused; nothing is stopped.
	f.usage(r1, "spc_a", 1200, 1.25)
	f.svc.OnUsage(f.ctx, r1)
	if len(f.notes) != 4 || f.notes[3].subject != "Budget exceeded" || len(f.stopped) != 0 {
		t.Fatalf("exceeded notes %+v stopped %v", f.notes, f.stopped)
	}
	for _, k := range []string{"worker", "kb", "control"} {
		if err := f.admit("spc_a", k); apperr.CodeOf(err) != apperr.CodeRateLimited || !strings.Contains(err.Error(), "not a hard limit") {
			t.Fatalf("%s over budget: %v", k, err)
		}
	}
	if err := f.admit("spc_b", "worker"); err != nil {
		t.Fatalf("another space refused: %v", err)
	}
	// The window moves on: after 24 hours the old runs no longer count.
	f.clk.Advance(25 * time.Hour)
	if err := f.admit("spc_a", "worker"); err != nil {
		t.Fatalf("after the window: %v", err)
	}
}

// SM-LIMIT-001: tool calls per run are counted and refused beyond the limit; the run is not stopped and listing the
// catalog is not a call.
func TestToolCallLimit(t *testing.T) {
	f := newFx(t)
	_, err := f.svc.Set(f.ctx, admin, "spc_a", 0, Input{Values: map[string]int64{ToolCallsPerRun: 2}})
	must(t, err)
	runID := f.run("spc_a", "worker")
	calls := 0
	h := f.svc.ToolCalls(func(context.Context, *fleet.RunPrincipal, string, json.RawMessage) (json.RawMessage, error) {
		calls++
		return json.RawMessage(`{}`), nil
	})
	p := &fleet.RunPrincipal{RunID: runID, SpaceID: "spc_a", Kind: "worker"}
	for i := range 2 {
		if _, err := h(f.ctx, p, "spaceman_inbox_list", nil); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if _, err := h(f.ctx, p, proto.ToolCatalog, nil); err != nil {
		t.Fatalf("catalog: %v", err)
	}
	if _, err := h(f.ctx, p, "spaceman_inbox_list", nil); apperr.CodeOf(err) != apperr.CodeRateLimited || !strings.Contains(err.Error(), "limit (2)") {
		t.Fatalf("over the limit: %v", err)
	}
	if calls != 3 {
		t.Fatalf("dispatched %d", calls)
	}
	row, err := f.s.R().GetRunToolCalls(f.ctx, runID)
	if err != nil || row.Calls != 2 || row.Refused != 1 || len(f.stopped) != 0 {
		t.Fatalf("counter %+v %v stopped %v", row, err, f.stopped)
	}
	// Raising the limit lets the run continue.
	_, err = f.svc.Set(f.ctx, admin, "spc_a", 1, Input{Values: map[string]int64{ToolCallsPerRun: 0}})
	must(t, err)
	if _, err := h(f.ctx, p, "spaceman_inbox_list", nil); err != nil {
		t.Fatalf("no limit: %v", err)
	}
}

// SM-LIMIT-001/002: a top-level task past the space's deadline is reported once to control and the operators and keeps
// running; the timer knows the next deadline.
func TestTaskDeadlinesNotifyNeverStop(t *testing.T) {
	f := newFx(t)
	started := f.clk.Now().UnixMicro()
	f.exec(fmt.Sprintf(`INSERT INTO tasks (id, space_id, title, rank, status, created_at, created_by, updated_at, started_at)
		VALUES ('tsk_1', 'spc_a', 'Long job', 1, 'running', 0, 't', 0, %d)`, started))
	if next, err := f.svc.NextDeadline(f.ctx); err != nil || !next.IsZero() {
		t.Fatalf("no deadline configured: %v %v", next, err)
	}
	_, err := f.svc.Set(f.ctx, admin, "spc_a", 0, Input{Values: map[string]int64{TaskDeadline: 30}})
	must(t, err)
	next, err := f.svc.NextDeadline(f.ctx)
	if err != nil || !next.Equal(f.clk.Now().Add(30*time.Minute)) {
		t.Fatalf("next deadline %v %v", next, err)
	}
	if n, err := f.svc.Deadlines(f.ctx); err != nil || n != 0 {
		t.Fatalf("early: %d %v", n, err)
	}
	f.clk.Advance(31 * time.Minute)
	if n, err := f.svc.Deadlines(f.ctx); err != nil || n != 1 {
		t.Fatalf("due: %d %v", n, err)
	}
	if n, _ := f.svc.Deadlines(f.ctx); n != 0 {
		t.Fatal("reported twice")
	}
	if len(f.notes) != 1 || !f.notes[0].control || f.notes[0].subject != "Task deadline passed" {
		t.Fatalf("notes %+v", f.notes)
	}
	var status string
	must(t, f.s.ReadDB().QueryRowContext(f.ctx, `SELECT status FROM tasks WHERE id = 'tsk_1'`).Scan(&status))
	if status != "running" {
		t.Fatalf("task %s after its deadline", status)
	}
	if next, _ := f.svc.NextDeadline(f.ctx); !next.IsZero() {
		t.Fatalf("a reported task is still scheduled: %v", next)
	}
}
