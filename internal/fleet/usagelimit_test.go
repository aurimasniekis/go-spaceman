package fleet

import (
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// ADR-0079: a usage limit on a run report from a session negotiated below minor 12 never reaches the report's apply,
// whatever the node sent; from minor 12 on it does. Hello reports go through the same session filter (session.report).
func TestRunReportLimitFilteredByMinor(t *testing.T) {
	e := newStaleEnv(t)
	var mu sync.Mutex
	var seen []proto.RunReport
	e.svc.onReport = func(r proto.RunReport) { mu.Lock(); seen = append(seen, r); mu.Unlock() }
	reset := e.t0.Add(time.Hour)
	rep := proto.RunReport{RunID: "run_x", SpaceID: "spc_x", Generation: 1, State: proto.RunExited, ObservedAt: e.t0,
		Limit: &proto.RunLimit{Kind: proto.LimitFiveHour, ResetsAt: &reset, Since: e.t0}}
	b, err := proto.Encode("msg_rep", proto.TypeRunReport, e.t0, rep)
	if err != nil {
		t.Fatal(err)
	}
	env, err := proto.Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	for _, minor := range []int{proto.MinorUsageLimit - 1, proto.MinorUsageLimit} {
		s := &session{nodeID: "nod_1", seq: 1, minor: minor, log: slog.New(slog.DiscardHandler)}
		mu.Lock()
		seen = nil
		mu.Unlock()
		_ = e.svc.hub.handle(e.ctx, s, env) // an unknown run: fenced, which doesn't matter here
		mu.Lock()
		got := seen
		mu.Unlock()
		if len(got) != 1 {
			t.Fatalf("minor %d: %d reports applied", minor, len(got))
		}
		if want := minor >= proto.MinorUsageLimit; (got[0].Limit != nil) != want {
			t.Errorf("minor %d: applied limit %+v", minor, got[0].Limit)
		}
		if want := minor >= proto.MinorUsageLimit; (s.report(rep).Limit != nil) != want {
			t.Errorf("minor %d: hello report filter kept %+v", minor, s.report(rep).Limit)
		}
	}
}

// ulEnv is a stale-run env with two spaces on one node sharing one account, and a Harbor Pilot space.
func ulEnv(t *testing.T) *staleEnv {
	t.Helper()
	e := newStaleEnv(t)
	e.exec(`INSERT INTO nodes (id, name, state, created_at, created_by, updated_at) VALUES ('nod_1', 'n1', 'active', 0, 't', 0)`)
	e.exec(`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES
		('spc_a', 'aaa', 'A', 'nod_1', 'active', 'a', 0, 't', 0), ('spc_b', 'bbb', 'B', 'nod_1', 'active', 'b', 0, 't', 0),
		('spc_c', 'ccc', 'C', 'nod_1', 'active', 'c', 0, 't', 0)`)
	e.exec(`INSERT INTO accounts (id, name, kind, secret_enc, secret_rotated_at, created_at, created_by, updated_at) VALUES
		('acc_1', 'one', 'claude_oauth_token', x'00', 0, 0, 't', 0), ('acc_2', 'two', 'claude_oauth_token', x'00', 0, 0, 't', 0)`)
	e.exec(`INSERT INTO space_accounts (space_id, account_id, harness, created_at, created_by) VALUES
		('spc_a', 'acc_1', 'claude-code', 0, 't'), ('spc_b', 'acc_1', 'claude-code', 0, 't'), ('spc_c', 'acc_2', 'claude-code', 0, 't')`)
	return e
}

// runOn seeds a running worker run in space whose launch used account (or, with account "", the Harbor Pilot project).
func (e *staleEnv) runOn(id, space, account, project string) {
	e.t.Helper()
	launch := `{"account_ref":"` + account + `"}`
	if account == "" {
		launch = `{"gateway":{"kind":"harbor-pilot","project_id":"` + project + `"}}`
	}
	e.exec(`INSERT INTO runs (id, space_id, node_id, generation, kind, role, harness, instance_id, desired, observed, launch_json, created_at,
		created_by, updated_at, observed_at) VALUES (?, ?, 'nod_1', 1, 'worker', 'implementor', 'claude-code', ?, 'running', 'running', ?, 0, 't', 0, 0)`,
		id, space, "ins_"+id, launch)
}

func (e *staleEnv) report(id, space string, gen int64, state string, l *proto.RunLimit) bool {
	e.t.Helper()
	fenced, err := e.svc.applyReport(e.ctx, "nod_1", proto.RunReport{RunID: id, SpaceID: space, Generation: gen, State: state, Status: "idle",
		ObservedAt: e.clk.Now(), Limit: l})
	if err != nil {
		e.t.Fatal(err)
	}
	return fenced
}

func (e *staleEnv) runVersion(id string) int64 {
	e.t.Helper()
	r, err := e.s.R().GetRun(e.ctx, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return r.Version
}

// ADR-0079 (UL-4): a limit report stores the run's limit and opens one hold for its account in its space, visible on
// the run and in every space bound to the account; the same report again writes nothing; a report of the same since
// without a reset keeps the stored one; a stale generation is ignored; a cleared report clears the hold everywhere,
// and a run that ends at a limit ends usage_limit.
func TestUsageLimitIngest(t *testing.T) {
	e := ulEnv(t)
	e.runOn("run_a", "spc_a", "acc_1", "")
	e.report("run_a", "spc_a", 1, proto.RunRunning, nil)
	reset := e.t0.Add(3 * time.Hour)
	since := e.t0.Add(-time.Minute)
	lim := &proto.RunLimit{Kind: proto.LimitFiveHour, ResetsAt: &reset, Since: since}

	// A stale generation is fenced and stores nothing.
	if !e.report("run_a", "spc_a", 0, proto.RunRunning, lim) {
		t.Fatal("a stale generation's report was not fenced")
	}
	if n := e.count(`SELECT count(*) FROM runs WHERE id = 'run_a' AND limit_kind <> ''`) + e.count(`SELECT count(*) FROM usage_limits`); n != 0 {
		t.Fatalf("a stale report wrote %d rows", n)
	}

	e.report("run_a", "spc_a", 1, proto.RunRunning, lim)
	run, err := e.svc.GetRun(e.ctx, "run_a")
	if err != nil {
		t.Fatal(err)
	}
	if run.Limit == nil || run.Limit.Kind != proto.LimitFiveHour || run.Limit.ResetsAt == nil || !run.Limit.ResetsAt.Equal(reset) || !run.Limit.Since.Equal(since) {
		t.Fatalf("run limit %+v", run.Limit)
	}
	a, err := e.svc.UsageLimits(e.ctx, "spc_a")
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 1 || a[0].Shared || a[0].Scope != LimitScopeAccount || a[0].AccountID != "acc_1" || a[0].Kind != proto.LimitFiveHour ||
		a[0].FirstRunID != "run_a" || a[0].ResetsAt == nil || !a[0].ResetsAt.Equal(reset) || a[0].CheckAt != nil || a[0].ClearedAt != nil {
		t.Fatalf("space A holds %+v", a)
	}
	// The hold applies to space B (same account), redacted; not to space C (another account).
	b, err := e.svc.UsageLimits(e.ctx, "spc_b")
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != 1 || !b[0].Shared || b[0].ID != "" || b[0].SpaceID != "" || b[0].FirstRunID != "" || b[0].ClearedBy != "" || b[0].Version != 0 ||
		b[0].AccountID != "acc_1" {
		t.Fatalf("space B holds %+v", b)
	}
	if c, err := e.svc.UsageLimits(e.ctx, "spc_c"); err != nil || len(c) != 0 {
		t.Fatalf("space C holds %+v %v", c, err)
	}
	if acc, err := e.svc.AccountUsageLimits(e.ctx, "acc_1"); err != nil || len(acc) != 1 || acc[0].SpaceID != "spc_a" {
		t.Fatalf("account view %+v %v", acc, err)
	}
	if n := e.count(`SELECT count(*) FROM event_outbox WHERE subject = 'evt.spaceman.fleet.usage_limit.reached'`); n != 1 {
		t.Fatalf("%d reached events", n)
	}

	// The same report twice, and the same since without a reset, write nothing and keep the reset.
	v := e.runVersion("run_a")
	changes := e.count(`SELECT count(*) FROM changes`)
	e.report("run_a", "spc_a", 1, proto.RunRunning, lim)
	e.report("run_a", "spc_a", 1, proto.RunRunning, &proto.RunLimit{Kind: proto.LimitFiveHour, Since: since})
	e.report("run_a", "spc_a", 1, proto.RunRunning, &proto.RunLimit{Kind: proto.LimitUnknown, Since: since})
	e.report("run_a", "spc_a", 1, proto.RunRunning, nil)
	if got := e.runVersion("run_a"); got != v {
		t.Fatalf("repeats moved the run's version %d → %d", v, got)
	}
	if got := e.count(`SELECT count(*) FROM changes`); got != changes {
		t.Fatalf("repeats wrote %d changes", got-changes)
	}
	if n := e.count(`SELECT count(*) FROM runs WHERE id = 'run_a' AND limit_kind = 'five_hour' AND limit_resets_at = ?`, reset.UnixMicro()); n != 1 {
		t.Fatal("the stored reset or kind was lost")
	}

	// Another run on the account in space B reports the limit over. Its own space's hold clears, naming it; space A's
	// hold, whose reset is still hours ahead, is only probed now (trust decision), and nothing in space A names run_b.
	e.runOn("run_b", "spc_b", "acc_1", "")
	e.report("run_b", "spc_b", 1, proto.RunRunning, lim)
	if n := e.count(`SELECT count(*) FROM usage_limits WHERE cleared_at IS NULL`); n != 2 {
		t.Fatalf("%d open holds, want one per space", n)
	}
	// A clear naming an older since is late: it is not this limit's clear.
	e.report("run_b", "spc_b", 1, proto.RunRunning, &proto.RunLimit{Cleared: true, Since: since.Add(-time.Minute)})
	if r, _ := e.svc.GetRun(e.ctx, "run_b"); r.Limit == nil {
		t.Fatal("a clear of an older since cleared the run's limit")
	}
	e.report("run_b", "spc_b", 1, proto.RunRunning, &proto.RunLimit{Cleared: true, Since: since})
	if n := e.count(`SELECT count(*) FROM usage_limits WHERE space_id = 'spc_b' AND cleared_by = 'run:run_b'`); n != 1 {
		t.Fatal("space B's own hold was not cleared by its run")
	}
	if n := e.count(`SELECT count(*) FROM usage_limits WHERE space_id = 'spc_a' AND cleared_at IS NULL AND check_at = ?`, e.t0.UnixMicro()); n != 1 {
		t.Fatal("space A's hold, resetting later, was not kept open and probed now")
	}
	if n := e.count(`SELECT count(*) FROM event_outbox WHERE subject = 'evt.spaceman.fleet.usage_limit.cleared'`); n != 1 {
		t.Fatalf("%d cleared events", n)
	}
	if n := e.count(`SELECT count(*) FROM event_outbox WHERE space_id = 'spc_a' AND (correlation_id = 'run_b' OR payload_json LIKE '%run_b%')`) +
		e.count(`SELECT count(*) FROM usage_limits WHERE space_id = 'spc_a' AND (cleared_by LIKE '%run_b%' OR first_run_id = 'run_b')`); n != 0 {
		t.Fatalf("space B's run appears %d times in space A", n)
	}
	if r, _ := e.svc.GetRun(e.ctx, "run_b"); r.Limit != nil {
		t.Fatalf("run_b limit %+v after clear", r.Limit)
	}
	// A clear repeated, or on a run without a limit, does nothing.
	v = e.runVersion("run_b")
	e.report("run_b", "spc_b", 1, proto.RunRunning, &proto.RunLimit{Cleared: true, Since: since})
	if n := e.count(`SELECT count(*) FROM event_outbox WHERE subject = 'evt.spaceman.fleet.usage_limit.cleared'`); n != 1 || e.runVersion("run_b") != v {
		t.Fatalf("%d cleared events after a repeat", n)
	}

	// run_a still has its limit; it exits: the end is usage_limit, and stays so after a later clear.
	e.report("run_a", "spc_a", 1, proto.RunExited, nil)
	if r, _ := e.svc.GetRun(e.ctx, "run_a"); r.EndReason != EndUsageLimit {
		t.Fatalf("end reason %q", r.EndReason)
	}
	e.report("run_a", "spc_a", 1, proto.RunExited, &proto.RunLimit{Cleared: true, Since: since})
	if r, _ := e.svc.GetRun(e.ctx, "run_a"); r.EndReason != EndUsageLimit || r.Limit != nil {
		t.Fatalf("after clear: end reason %q, limit %+v", r.EndReason, r.Limit)
	}
	if n := e.count(`SELECT count(*) FROM event_outbox WHERE subject = 'evt.spaceman.fleet.run.ended' AND payload_json LIKE '%"end_reason":"usage_limit"%'
		AND payload_json NOT LIKE '%"node_reason"%'`); n != 1 {
		t.Fatalf("%d usage_limit run_ended events", n)
	}
}

// ADR-0079 decision 7: a hold whose reset is unknown is probed after an hour; a later known reset fills it in (no new
// event). Gateway holds are connection-wide for a quota and per project for a budget, and apply to the spaces routed
// through the same connection or project.
func TestUsageLimitHoldScopes(t *testing.T) {
	e := ulEnv(t)
	e.exec(`INSERT INTO space_harbor_pilot (space_id, enabled, project_id, created_at, created_by, updated_at, updated_by) VALUES
		('spc_a', 1, 'prj_1', 0, 't', 0, 't'), ('spc_b', 1, 'prj_2', 0, 't', 0, 't')`)
	e.runOn("run_q", "spc_a", "", "prj_1")
	e.runOn("run_g", "spc_a", "", "prj_1")
	e.report("run_q", "spc_a", 1, proto.RunRunning, &proto.RunLimit{Kind: proto.LimitGatewayQuota, Since: e.t0})
	e.report("run_g", "spc_a", 1, proto.RunRunning, &proto.RunLimit{Kind: proto.LimitGatewayBudget, Since: e.t0})
	if n := e.count(`SELECT count(*) FROM usage_limits WHERE scope = 'gateway' AND gateway_ref = 'harbor-pilot' AND kind = 'gateway_quota'
		AND resets_at IS NULL AND check_at = ?`, e.us(time.Hour)); n != 1 {
		t.Fatal("no connection-wide quota hold probed after an hour")
	}
	if n := e.count(`SELECT count(*) FROM usage_limits WHERE gateway_ref = 'harbor-pilot:project:prj_1' AND kind = 'gateway_budget'`); n != 1 {
		t.Fatal("no project budget hold")
	}
	// Space B shares the connection, not the project.
	b, err := e.svc.UsageLimits(e.ctx, "spc_b")
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != 1 || !b[0].Shared || b[0].GatewayRef != "harbor-pilot" {
		t.Fatalf("space B holds %+v", b)
	}
	// A later report with the reset fills it in and drops the probe, without another event.
	reset := e.t0.Add(2 * time.Hour)
	e.report("run_q", "spc_a", 1, proto.RunRunning, &proto.RunLimit{Kind: proto.LimitGatewayQuota, Since: e.t0, ResetsAt: &reset})
	if n := e.count(`SELECT count(*) FROM usage_limits WHERE kind = 'gateway_quota' AND resets_at = ? AND check_at IS NULL`, reset.UnixMicro()); n != 1 {
		t.Fatal("the reset was not filled in")
	}
	if n := e.count(`SELECT count(*) FROM event_outbox WHERE subject = 'evt.spaceman.fleet.usage_limit.reached'`); n != 2 {
		t.Fatalf("%d reached events", n)
	}
}

// ADR-0079 / T3 alignment: the node repeats a clear on every later report, naming the since of the limit it cleared. A
// late clear of an older limit never clears a newer one, on the run or among the holds; a clear clears only holds seen
// no later than the cleared limit; the same limit reclassified (unknown, then a typed kind) supersedes its hold; a run
// that ends with a node reason other than usage_limit keeps it on the run_ended event.
func TestUsageLimitClearSince(t *testing.T) {
	e := ulEnv(t)
	e.runOn("run_a", "spc_a", "acc_1", "")
	e.runOn("run_b", "spc_b", "acc_1", "")
	t1, t2 := e.t0.Add(-2*time.Minute), e.t0.Add(-30*time.Second)
	open := func(id, space string, since time.Time) {
		e.report(id, space, 1, proto.RunRunning, &proto.RunLimit{Kind: proto.LimitFiveHour, Since: since})
	}
	clr := func(id, space string, since time.Time) {
		e.report(id, space, 1, proto.RunRunning, &proto.RunLimit{Cleared: true, Since: since})
	}
	open("run_a", "spc_a", t1)
	clr("run_a", "spc_a", t1)
	open("run_a", "spc_a", t2) // a new window right after the reset
	v := e.runVersion("run_a")
	clr("run_a", "spc_a", t1) // the old clear, repeated late
	if r, _ := e.svc.GetRun(e.ctx, "run_a"); r.Limit == nil || !r.Limit.Since.Equal(t2) || e.runVersion("run_a") != v {
		t.Fatalf("a late clear of t1 touched the t2 limit: %+v", r.Limit)
	}
	if n := e.count(`SELECT count(*) FROM usage_limits WHERE cleared_at IS NULL AND seen_at = ?`, t2.UnixMicro()); n != 1 {
		t.Fatal("a late clear of t1 cleared the t2 hold")
	}

	// run_b's limit was seen before run_a's new window: its clear leaves the newer hold in space A open.
	open("run_b", "spc_b", t1)
	clr("run_b", "spc_b", t1)
	if n := e.count(`SELECT count(*) FROM usage_limits WHERE space_id = 'spc_a' AND cleared_at IS NULL`); n != 1 {
		t.Fatal("a clear of an older window cleared a hold seen after it")
	}
	if n := e.count(`SELECT count(*) FROM usage_limits WHERE space_id = 'spc_b' AND cleared_at IS NULL`); n != 0 {
		t.Fatal("run_b's own hold stayed open")
	}

	// The wrap-up path's unknown limit, then the typed record for the same since: one hold, of the typed kind.
	e.runOn("run_w", "spc_c", "acc_2", "")
	e.report("run_w", "spc_c", 1, proto.RunRunning, &proto.RunLimit{Kind: proto.LimitUnknown, Since: t2})
	e.report("run_w", "spc_c", 1, proto.RunRunning, &proto.RunLimit{Kind: proto.LimitSevenDay, Since: t2})
	if n := e.count(`SELECT count(*) FROM usage_limits WHERE space_id = 'spc_c' AND cleared_at IS NULL AND kind = 'seven_day'`); n != 1 {
		t.Fatal("no typed hold")
	}
	if n := e.count(`SELECT count(*) FROM usage_limits WHERE space_id = 'spc_c' AND cleared_at IS NULL`); n != 1 {
		t.Fatal("the unknown hold was not superseded")
	}

	// The end keeps the node's own reason on the event.
	if _, err := e.svc.applyReport(e.ctx, "nod_1", proto.RunReport{RunID: "run_w", SpaceID: "spc_c", Generation: 1, State: proto.RunExited,
		Reason: "exited", ObservedAt: e.clk.Now()}); err != nil {
		t.Fatal(err)
	}
	if n := e.count(`SELECT count(*) FROM event_outbox WHERE subject = 'evt.spaceman.fleet.run.ended' AND payload_json LIKE '%"end_reason":"usage_limit"%'
		AND payload_json LIKE '%"node_reason":"exited"%'`); n != 1 {
		t.Fatal("run_ended lacks the node's reason")
	}
}

func (e *staleEnv) openHolds(space string) int {
	e.t.Helper()
	return e.count(`SELECT count(*) FROM usage_limits WHERE space_id = ? AND cleared_at IS NULL`, space)
}

// PR #55 review B2, S1: a newer limit (another since) ends the run's old one, so a new window never reuses the old
// window's hold and a late clear of another window can't end it; a hold whose reset passed before a new limit was seen
// is replaced, not reused; a kind change with a new since leaves no ghost hold.
func TestUsageLimitNewWindow(t *testing.T) {
	e := ulEnv(t)
	e.runOn("run_a", "spc_a", "acc_1", "")
	e.runOn("run_b", "spc_b", "acc_1", "")
	ta1, tb, ta2 := e.t0.Add(-3*time.Hour), e.t0.Add(-2*time.Hour), e.t0.Add(-time.Hour)
	five := func(id, space string, since time.Time, reset *time.Time) {
		e.report(id, space, 1, proto.RunRunning, &proto.RunLimit{Kind: proto.LimitFiveHour, Since: since, ResetsAt: reset})
	}
	five("run_a", "spc_a", ta1, nil)
	five("run_b", "spc_b", tb, nil)
	five("run_a", "spc_a", ta2, nil) // a new window
	if n := e.count(`SELECT count(*) FROM usage_limits WHERE space_id = 'spc_a' AND cleared_at IS NULL AND seen_at = ?`, ta2.UnixMicro()); n != 1 || e.openHolds("spc_a") != 1 {
		t.Fatal("space A's new window has no hold of its own")
	}
	e.report("run_b", "spc_b", 1, proto.RunRunning, &proto.RunLimit{Cleared: true, Since: tb})
	if e.openHolds("spc_a") != 1 || e.openHolds("spc_b") != 0 {
		t.Fatalf("after run_b's clear: A %d, B %d open", e.openHolds("spc_a"), e.openHolds("spc_b"))
	}

	// Two runs of one space in consecutive windows: the second window's hold is new, so the first run's clear leaves it.
	e.runOn("run_x", "spc_c", "acc_2", "")
	e.runOn("run_y", "spc_c", "acc_2", "")
	r1, r2 := e.t0.Add(-time.Hour), e.t0.Add(4*time.Hour)
	five("run_x", "spc_c", e.t0.Add(-6*time.Hour), &r1)
	five("run_y", "spc_c", e.t0.Add(-30*time.Minute), &r2)
	e.report("run_x", "spc_c", 1, proto.RunRunning, &proto.RunLimit{Cleared: true, Since: e.t0.Add(-6 * time.Hour)})
	if n := e.count(`SELECT count(*) FROM usage_limits WHERE space_id = 'spc_c' AND cleared_at IS NULL AND resets_at = ?`, r2.UnixMicro()); n != 1 || e.openHolds("spc_c") != 1 {
		t.Fatal("the second window's hold was reused or cleared")
	}

	// S1: five_hour at t1, then seven_day at t2, then the clears: no hold is left.
	e.exec(`INSERT INTO accounts (id, name, kind, secret_enc, secret_rotated_at, created_at, created_by, updated_at) VALUES ('acc_3', 'three', 'claude_oauth_token', x'00', 0, 0, 't', 0)`)
	e.exec(`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('spc_d', 'ddd', 'D', 'nod_1', 'active', 'd', 0, 't', 0)`)
	e.runOn("run_k", "spc_d", "acc_3", "")
	t1, t2 := e.t0.Add(-2*time.Hour), e.t0.Add(-time.Hour)
	five("run_k", "spc_d", t1, nil)
	e.report("run_k", "spc_d", 1, proto.RunRunning, &proto.RunLimit{Kind: proto.LimitSevenDay, Since: t2})
	e.report("run_k", "spc_d", 1, proto.RunRunning, &proto.RunLimit{Cleared: true, Since: t1})
	e.report("run_k", "spc_d", 1, proto.RunRunning, &proto.RunLimit{Cleared: true, Since: t2})
	if n := e.openHolds("spc_d"); n != 0 {
		t.Fatalf("%d ghost holds", n)
	}
}

// PR #55 review S2: a node clock 30 s ahead keeps its since, so identical repeats write nothing after the first and a
// clear naming the raw since matches; a since far ahead (clamped) keeps the stored since instead of writing anew.
func TestUsageLimitClockSkew(t *testing.T) {
	e := ulEnv(t)
	e.runOn("run_a", "spc_a", "acc_1", "")
	ahead := e.t0.Add(30 * time.Second)
	l := &proto.RunLimit{Kind: proto.LimitFiveHour, Since: ahead}
	e.report("run_a", "spc_a", 1, proto.RunRunning, l)
	v := e.runVersion("run_a")
	for range 5 {
		e.report("run_a", "spc_a", 1, proto.RunRunning, l)
	}
	if got := e.runVersion("run_a"); got != v {
		t.Fatalf("repeats with a skewed clock moved the version %d → %d", v, got)
	}
	e.report("run_a", "spc_a", 1, proto.RunRunning, &proto.RunLimit{Cleared: true, Since: ahead})
	if r, _ := e.svc.GetRun(e.ctx, "run_a"); r.Limit != nil || e.openHolds("spc_a") != 0 {
		t.Fatalf("a clear naming the raw since didn't match: %+v", r.Limit)
	}

	e.runOn("run_f", "spc_c", "acc_2", "")
	far := &proto.RunLimit{Kind: proto.LimitFiveHour, Since: e.t0.Add(time.Hour)}
	e.report("run_f", "spc_c", 1, proto.RunRunning, far)
	v = e.runVersion("run_f")
	for i := range 5 {
		e.clk.Set(e.t0.Add(time.Duration(i+1) * time.Second))
		e.report("run_f", "spc_c", 1, proto.RunRunning, far)
	}
	if got := e.runVersion("run_f"); got != v {
		t.Fatalf("repeats of a clamped since moved the version %d → %d", v, got)
	}
}

// PR #55 review S3: a report carrying a limit for a run that already ended doesn't rewrite its end reason.
func TestUsageLimitEndedRunKeepsReason(t *testing.T) {
	e := ulEnv(t)
	e.runOn("run_a", "spc_a", "acc_1", "")
	if _, err := e.svc.applyReport(e.ctx, "nod_1", proto.RunReport{RunID: "run_a", SpaceID: "spc_a", Generation: 1, State: proto.RunExited, Reason: "exited",
		ObservedAt: e.clk.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.applyReport(e.ctx, "nod_1", proto.RunReport{RunID: "run_a", SpaceID: "spc_a", Generation: 1, State: proto.RunExited, Reason: "exited",
		ObservedAt: e.clk.Now(), Limit: &proto.RunLimit{Kind: proto.LimitFiveHour, Since: e.t0}}); err != nil {
		t.Fatal(err)
	}
	if r, _ := e.svc.GetRun(e.ctx, "run_a"); r.EndReason != "exited" {
		t.Fatalf("an ended run's reason was rewritten to %q", r.EndReason)
	}
}

// PR #55 review B1 and the trust decision: another space's hold whose reset passed, or is unknown, is cleared by a
// node's evidence as "evidence", correlated on the hold; one resetting later is only probed. Shared rows are one per
// account and kind however many spaces hold it (N1); an unknown account's view is not found (N2).
func TestUsageLimitCrossSpaceTrust(t *testing.T) {
	e := ulEnv(t)
	e.exec(`INSERT INTO space_accounts (space_id, account_id, harness, created_at, created_by) VALUES ('spc_c', 'acc_1', 'claude-code', 0, 't')`)
	e.runOn("run_a", "spc_a", "acc_1", "")
	e.runOn("run_c", "spc_c", "acc_1", "")
	e.runOn("run_b", "spc_b", "acc_1", "")
	passed := e.t0.Add(-time.Minute)
	e.report("run_a", "spc_a", 1, proto.RunRunning, &proto.RunLimit{Kind: proto.LimitFiveHour, Since: e.t0.Add(-5 * time.Hour), ResetsAt: &passed})
	e.report("run_c", "spc_c", 1, proto.RunRunning, &proto.RunLimit{Kind: proto.LimitFiveHour, Since: e.t0.Add(-4 * time.Hour)})

	// Space B sees one shared row for the account and kind, not one per space.
	b, err := e.svc.UsageLimits(e.ctx, "spc_b")
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != 1 || !b[0].Shared || b[0].ResetsAt != nil || !b[0].SeenAt.Equal(e.t0.Add(-5*time.Hour)) {
		t.Fatalf("space B's shared rows %+v", b)
	}

	e.report("run_b", "spc_b", 1, proto.RunRunning, &proto.RunLimit{Kind: proto.LimitFiveHour, Since: e.t0.Add(-time.Hour)})
	e.report("run_b", "spc_b", 1, proto.RunRunning, &proto.RunLimit{Cleared: true, Since: e.t0.Add(-time.Hour)})
	for _, sp := range []string{"spc_a", "spc_c"} {
		if n := e.count(`SELECT count(*) FROM usage_limits WHERE space_id = ? AND cleared_by = 'evidence'`, sp); n != 1 {
			t.Fatalf("%s's hold was not cleared by evidence", sp)
		}
		if n := e.count(`SELECT count(*) FROM event_outbox WHERE space_id = ? AND subject = 'evt.spaceman.fleet.usage_limit.cleared'
			AND correlation_id IN (SELECT id FROM usage_limits WHERE space_id = ?) AND payload_json NOT LIKE '%run_b%'`, sp, sp); n != 1 {
			t.Fatalf("%s's cleared event isn't correlated on its hold", sp)
		}
	}
	if _, err := e.svc.AccountUsageLimits(e.ctx, "acc_none"); !errors.Is(err, errAccountNotFound) {
		t.Fatalf("unknown account: %v", err)
	}
}
