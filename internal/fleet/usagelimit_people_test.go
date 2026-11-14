package fleet

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// ADR-0079 Q5 (UL-9): the holds whose space's people are told are the open, untold ones with a known reset further away
// than the space's usage_limit_notify_after_hours (0: never), in spaces that aren't archived. A 5-hour wait under the
// default 12 h, an unknown reset, a space that turned it off, a hold already told and a cleared hold are not; what waits
// on a hold counts the space's live runs at that limit on the account and the waits parked under it, with their tasks.
func TestLongHoldsToTell(t *testing.T) {
	e := ulEnv(t)
	after := map[string]time.Duration{"spc_a": DefaultLimitNotifyAfter, "spc_b": 0, "spc_c": time.Hour}
	e.svc.opts.UsageLimitSettings = func(_ context.Context, _ *db.Queries, spaceID string) UsageLimitSettings {
		return UsageLimitSettings{NotifyAfter: after[spaceID]}
	}
	now := e.clk.Now()
	weekly, fiveHour := now.Add(3*24*time.Hour), now.Add(3*time.Hour)
	limit := func(kind string, reset *time.Time) *proto.RunLimit {
		return &proto.RunLimit{Kind: kind, ResetsAt: reset, Since: now}
	}
	for _, r := range []struct {
		id, space, account string
		l                  *proto.RunLimit
	}{
		{"run_a1", "spc_a", "acc_1", limit(proto.LimitSevenDay, &weekly)},
		{"run_a2", "spc_a", "acc_1", limit(proto.LimitSevenDay, &weekly)},
		{"run_a3", "spc_a", "acc_2", limit(proto.LimitFiveHour, &fiveHour)}, // 3 h < 12 h
		{"run_b1", "spc_b", "acc_1", limit(proto.LimitSevenDay, &weekly)},   // spc_b: never
		{"run_c1", "spc_c", "acc_2", limit(proto.LimitFiveHour, &fiveHour)}, // 3 h > 1 h
		{"run_c2", "spc_c", "acc_1", limit(proto.LimitUnknown, nil)},        // unknown reset
	} {
		e.runOn(r.id, r.space, r.account, "")
		e.report(r.id, r.space, 1, proto.RunRunning, r.l)
	}
	e.exec(`INSERT INTO tasks (id, space_id, title, rank, status, assignee_run, created_at, created_by, updated_at) VALUES ('tsk_1', 'spc_a', 'T', 1, 'running', 'run_a1', 0, 't', 0)`)
	holdID := func(space, account, kind string) string {
		u, err := e.s.R().GetOpenUsageLimit(e.ctx, db.GetOpenUsageLimitParams{SpaceID: space, Scope: LimitScopeAccount, AccountID: account, Kind: kind})
		if err != nil {
			t.Fatalf("hold %s %s %s: %v", space, account, kind, err)
		}
		return u.ID
	}
	aWeekly, cFive := holdID("spc_a", "acc_1", proto.LimitSevenDay), holdID("spc_c", "acc_2", proto.LimitFiveHour)
	// A third run of space A ended at the weekly limit and was parked under the hold, with a task.
	e.runOn("run_old", "spc_a", "acc_1", "")
	e.exec(`UPDATE runs SET observed = 'exited', desired = 'stopped', end_reason = 'usage_limit' WHERE id = 'run_old'`)
	e.exec(`INSERT INTO limit_waits (id, space_id, run_id, task_id, run_kind, role, node_id, instance_id, harness, session_id, launch_json, usage_limit_id,
		resume_at, state, created_at, updated_at) VALUES ('lwt_1', 'spc_a', 'run_old', 'tsk_2', 'worker', 'implementor', 'nod_1', 'ins_old', 'claude-code', '', '{}', ?, ?, 'waiting', 0, 0)`,
		aWeekly, weekly.UnixMicro())

	got := func() map[string]LongHold {
		t.Helper()
		hs, err := e.svc.LongHoldsToTell(e.ctx)
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]LongHold{}
		for _, h := range hs {
			m[h.Hold.ID] = h
		}
		return m
	}
	hs := got()
	if len(hs) != 2 {
		t.Fatalf("holds to tell %+v, want space A's weekly and space C's 5-hour", hs)
	}
	a := hs[aWeekly]
	if a.Space != "A" || a.SpaceSlug != "aaa" || a.Runs != 3 || a.Tasks != 2 || a.NotifyAfter != DefaultLimitNotifyAfter ||
		a.Subject != `account "one" is at its weekly usage limit` || !a.Hold.ResetsAt.Equal(weekly) {
		t.Fatalf("space A's hold %+v", a)
	}
	if c := hs[cFive]; c.Runs != 1 || c.Tasks != 0 || c.Subject != `account "two" is at its 5-hour usage limit` {
		t.Fatalf("space C's hold %+v", c)
	}

	// Told once: never again, and the change is recorded.
	v := e.count(`SELECT version FROM usage_limits WHERE id = ?`, aWeekly)
	if err := e.svc.MarkHoldTold(e.ctx, aWeekly); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.MarkHoldTold(e.ctx, aWeekly); err != nil {
		t.Fatal(err)
	}
	if n := e.count(`SELECT version FROM usage_limits WHERE id = ? AND notified_humans_at IS NOT NULL`, aWeekly); n != v+1 {
		t.Fatalf("version after told twice %d, want %d", n, v+1)
	}
	if hs := got(); len(hs) != 1 || hs[cFive].Hold.ID == "" {
		t.Fatalf("after telling A: %+v", hs)
	}
	// Space C's threshold rises above its wait; then it's lowered again, but the space is archived.
	after["spc_c"] = 4 * time.Hour
	if hs := got(); len(hs) != 0 {
		t.Fatalf("a 3 h wait under a 4 h threshold: %+v", hs)
	}
	after["spc_c"] = time.Hour
	e.exec(`UPDATE spaces SET state = 'archived' WHERE id = 'spc_c'`)
	if hs := got(); len(hs) != 0 {
		t.Fatalf("archived space: %+v", hs)
	}
	e.exec(`UPDATE spaces SET state = 'active' WHERE id = 'spc_c'`)
	// Time passes: the reset is now less than the threshold away.
	e.clk.Set(fiveHour.Add(-time.Hour))
	if hs := got(); len(hs) != 0 {
		t.Fatalf("a reset an hour away: %+v", hs)
	}
	// A cleared hold is never told.
	e.clk.Set(now)
	e.report("run_c1", "spc_c", 1, proto.RunRunning, &proto.RunLimit{Kind: proto.LimitFiveHour, ResetsAt: &fiveHour, Since: now, Cleared: true})
	if hs := got(); len(hs) != 0 {
		t.Fatalf("cleared hold: %+v", hs)
	}
}

// Without Options.UsageLimitSettings the threshold is the 12 h default.
func TestLongHoldsDefaultThreshold(t *testing.T) {
	e := ulEnv(t)
	now := e.clk.Now()
	for _, c := range []struct {
		run   string
		reset time.Duration
		kind  string
	}{{"run_1", 11 * time.Hour, proto.LimitSevenDay}, {"run_2", 13 * time.Hour, proto.LimitSevenDay}, {"run_3", 12 * time.Hour, proto.LimitSevenDayOpus}} {
		reset := now.Add(c.reset)
		account := map[string]string{"run_1": "acc_1", "run_2": "acc_2", "run_3": "acc_1"}[c.run]
		e.runOn(c.run, "spc_a", account, "")
		e.report(c.run, "spc_a", 1, proto.RunRunning, &proto.RunLimit{Kind: c.kind, ResetsAt: &reset, Since: now})
	}
	// A reset exactly the threshold away is not further away: run_3's weekly Opus hold is not told.
	hs, err := e.svc.LongHoldsToTell(e.ctx)
	if err != nil || len(hs) != 1 || hs[0].Subject != `account "two" is at its weekly usage limit` {
		t.Fatalf("holds %+v %v", hs, err)
	}
}

// Holds that stay untold can't starve later ones (PR #63 review N1): more than two pages of untold holds in a space
// that turned the notice off, all with the same reset, come before space A's hold in the scan order, and its hold is
// still found; a hold of an unknown kind with a known reset is named without a kind.
func TestLongHoldsNotStarved(t *testing.T) {
	e := ulEnv(t)
	e.svc.opts.UsageLimitSettings = func(_ context.Context, _ *db.Queries, spaceID string) UsageLimitSettings {
		if spaceID == "spc_b" {
			return UsageLimitSettings{}
		}
		return UsageLimitSettings{NotifyAfter: DefaultLimitNotifyAfter}
	}
	reset := e.clk.Now().Add(3 * 24 * time.Hour).UnixMicro()
	var vals []string
	var args []any
	for i := range 2*maxLimitRows + 50 {
		vals = append(vals, "(?, 'spc_b', 'gateway', '', ?, 'gateway_quota', ?, 'quota_board', 0, 0, 0)")
		args = append(args, fmt.Sprintf("ulm_b_%03d", i), fmt.Sprintf("gw_%03d", i), reset)
	}
	e.exec(`INSERT INTO usage_limits (id, space_id, scope, account_id, gateway_ref, kind, resets_at, source, seen_at, created_at, updated_at) VALUES `+
		strings.Join(vals, ", "), args...)
	// Between ulm_b_200 and ulm_b_201 in id order, so on the second page, and seen last.
	e.exec(`INSERT INTO usage_limits (id, space_id, scope, account_id, kind, resets_at, source, seen_at, created_at, updated_at)
		VALUES ('ulm_b_2005', 'spc_a', 'account', 'acc_2', 'unknown', ?, 'operator', 1, 0, 0)`, reset)
	hs, err := e.svc.LongHoldsToTell(e.ctx)
	if err != nil || len(hs) != 1 || hs[0].Hold.ID != "ulm_b_2005" || hs[0].Subject != `account "two" is at a usage limit` {
		t.Fatalf("holds %+v %v", hs, err)
	}
}
