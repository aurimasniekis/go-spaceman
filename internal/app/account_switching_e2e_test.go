package app_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/human"
	"github.com/aurimasniekis/go-spaceman/internal/limits"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/usage"
)

// Account switching end to end (simulated: a scripted node stands in for the node agent and its Claude Code, central
// on a fake clock with the deadline loop running as in Serve). Nothing here is real-account evidence: that is the
// operator checklist in docs/account-switching-checklist.md.

// usageOf is a run's cumulative usage as its harness would report it (the session's transcript totals).
func usageOf(runID string, in, out int64, windows ...proto.UsageWindow) proto.UsageReport {
	return proto.UsageReport{RunID: runID, Source: proto.UsageClaudeTranscript, Windows: windows,
		Models: []proto.ModelUsage{{Model: "claude-haiku-5-5", Input: &in, Output: &out, Requests: 1}}}
}

// followWait follows a parked run's wait to its resume, renewing the lease while the fake clock jumps.
func followWait(t *testing.T, e *fleetEnv, n *rawNode, clk *clock.Fake, spaceID, runID string) {
	t.Helper()
	ctx := context.Background()
	eventually(t, "resumed", func() bool {
		w, _ := e.c.Fleet.LimitWaitForRun(ctx, runID)
		if w == nil || w.State == fleet.WaitResumed || w.State == fleet.WaitResuming {
			return w != nil
		}
		if w.ResumeAt.After(clk.Now()) {
			clk.Set(w.ResumeAt)
		}
		n.send(t, proto.TypeHeartbeat, proto.Heartbeat{Leases: []proto.LeaseState{}})
		eventually(t, "lease renewed", func() bool { s, _ := e.c.Fleet.GetSpace(ctx, spaceID); return s.Lease != nil && s.Lease.Valid })
		e.c.Human.Kick()
		return false
	})
}

// stopAtIdle plays the node's side of a stop: the run goes idle with its limit, central stops it, the node reports it
// exited.
func stopAtIdle(t *testing.T, n *rawNode, r *fleet.Run, spaceID string, gen int64, turn uint64, session string, l *proto.RunLimit) {
	t.Helper()
	n.send(t, proto.TypeRunReport, proto.RunReport{RunID: r.ID, SpaceID: spaceID, Generation: gen, State: proto.RunRunning, Status: "idle", Turn: turn,
		SessionID: session, Limit: l, ObservedAt: time.Now().UTC()})
	n.command(t, r.ID, proto.CmdStop)
	n.send(t, proto.TypeRunReport, proto.RunReport{RunID: r.ID, SpaceID: spaceID, Generation: gen, State: proto.RunExited, Reason: "stopped", Turn: turn,
		SessionID: session, Limit: l, ObservedAt: time.Now().UTC()})
}

func tokens(t *testing.T, s *usage.Series, group string) int64 {
	t.Helper()
	var n int64
	for _, r := range s.Rows {
		if r.Group == group {
			n += r.InputTokens + r.OutputTokens
		}
	}
	return n
}

// A worker on account A hits its 5-hour limit with a far reset: it is switched to account B, and the same session
// continues there (the same session ID, resumed). The usage ledger books what each account's run used, per account and
// per role, with the resumed run only counted for what the session grew by; the windows the runs reported are kept per
// account.
func TestAccountSwitchEndToEnd(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(time.Now().UTC().Truncate(time.Second))
	e := newFleetEnvClock(t, clk)
	e.enroll()
	ctx := context.Background()
	n, sp := hvNode(t, e, "asw-e2e", proto.MinorAccountUsage, false)
	runLimitWaits(t, e)
	if _, err := e.c.Limits.Set(ctx, admin, sp.ID, 0, limits.Input{Values: map[string]int64{limits.UsageLimitFailover: 1}}); err != nil {
		t.Fatal(err)
	}
	accB := bindAnotherClaude(t, e, sp.ID)
	w1, gen, task := assignedWorker(t, e, n, sp.ID, "Switch and count", "")
	accA := w1.Launch.AccountRef
	if accA == "" || accA == accB {
		t.Fatalf("worker on account %q (B is %q)", accA, accB)
	}
	const session = "ses-e2e-switch"

	// The run works and its usage and A's windows are reported; a second worker (another role) uses A too.
	reset := clk.Now().Add(3 * time.Hour)
	n.send(t, proto.TypeRunReport, proto.RunReport{RunID: w1.ID, SpaceID: sp.ID, Generation: gen, State: proto.RunRunning, Status: "working", Turn: 1,
		SessionID: session, ObservedAt: time.Now().UTC()})
	n.send(t, proto.TypeUsage, usageOf(w1.ID, 1000, 100, proto.UsageWindow{Kind: proto.WindowFiveHour, UsedPct: 96, ResetsAt: &reset, ObservedAt: clk.Now()}))
	other, _ := launchGen(t, e, n, sp.ID, fleet.LaunchInput{Role: "scribe", Kind: "worker", Harness: "claude-code", Prompt: "write", AccountRef: accA})
	n.send(t, proto.TypeUsage, usageOf(other.ID, 400, 40))
	eventually(t, "A's usage booked", func() bool {
		s, err := e.c.Usage.SpaceSeries(ctx, sp.ID, usage.SeriesQuery{GroupBy: "account"})
		return err == nil && tokens(t, s, accA) == 1100+440
	})

	// The limit: armed while busy, executed at the idle point; the new run resumes the same session on B.
	limited := &proto.RunLimit{Kind: proto.LimitFiveHour, ResetsAt: &reset, Since: clk.Now()}
	n.send(t, proto.TypeRunReport, proto.RunReport{RunID: w1.ID, SpaceID: sp.ID, Generation: gen, State: proto.RunRunning, Status: "working", Turn: 2,
		SessionID: session, Limit: limited, ObservedAt: time.Now().UTC()})
	eventually(t, "switch armed", func() bool {
		l, _ := e.c.Fleet.AccountSwitches(ctx, sp.ID)
		return len(l) == 1 && l[0].State == fleet.SwitchPending && l[0].ToAccountID == accB
	})
	stopAtIdle(t, n, w1, sp.ID, gen, 2, session, limited)
	followWait(t, e, n, clk, sp.ID, w1.ID)
	cmd, p := n.launchFor(t, w1.InstanceID, w1.ID)
	if !p.Resume || p.SessionID != session || p.AccountRef != accB || p.Secrets["CLAUDE_CODE_OAUTH_TOKEN"] != "stub-claude-token-2" {
		t.Fatalf("switch launch: resume %v session %q account %q", p.Resume, p.SessionID, p.AccountRef)
	}
	resumed := e.run1(cmd.RunID)
	if resumed.InstanceID != w1.InstanceID || resumed.Launch.AccountRef != accB {
		t.Fatalf("resumed run %+v", resumed)
	}
	eventually(t, "task follows", func() bool { return e.task(sp.ID, task.ID).AssigneeRun == resumed.ID })
	n.send(t, proto.TypeRunReport, proto.RunReport{RunID: resumed.ID, SpaceID: sp.ID, Generation: cmd.Generation, State: proto.RunRunning, Status: "idle",
		Turn: 1, SessionID: session, ObservedAt: time.Now().UTC()})
	eventually(t, "switch succeeded", func() bool {
		l, _ := e.c.Fleet.AccountSwitches(ctx, sp.ID)
		return len(l) == 1 && l[0].State == fleet.SwitchSucceeded && l[0].NewRunID == resumed.ID
	})

	// The resumed run's transcript is the same session: its totals include what A's run had used. Only the growth is B's.
	n.send(t, proto.TypeUsage, usageOf(resumed.ID, 1500, 150, proto.UsageWindow{Kind: proto.WindowFiveHour, UsedPct: 12, ObservedAt: clk.Now()}))
	eventually(t, "B's usage booked", func() bool {
		s, err := e.c.Usage.SpaceSeries(ctx, sp.ID, usage.SeriesQuery{GroupBy: "account"})
		return err == nil && tokens(t, s, accB) == 550
	})
	byAcc, err := e.c.Usage.SpaceSeries(ctx, sp.ID, usage.SeriesQuery{GroupBy: "account"})
	if err != nil {
		t.Fatal(err)
	}
	if a, b := tokens(t, byAcc, accA), tokens(t, byAcc, accB); a != 1540 || b != 550 || byAcc.Totals.TotalTokens != 2090 {
		t.Fatalf("per account: A %d B %d total %d (session counted once)", a, b, byAcc.Totals.TotalTokens)
	}
	byRole, err := e.c.Usage.SpaceSeries(ctx, sp.ID, usage.SeriesQuery{GroupBy: "role"})
	if err != nil {
		t.Fatal(err)
	}
	if free, scribe := tokens(t, byRole, "free"), tokens(t, byRole, "scribe"); free != 1650 || scribe != 440 {
		// role free: A's run 1100 plus the resumed run's 550; scribe 440.
		t.Fatalf("per role: free %d scribe %d", free, scribe)
	}
	accSeries, err := e.c.Usage.AccountSeries(ctx, accB, usage.SeriesQuery{GroupBy: "role"}, func(string) bool { return true })
	if err != nil || tokens(t, accSeries, "free") != 550 || tokens(t, accSeries, "scribe") != 0 {
		t.Fatalf("account B by role: %v %+v", err, accSeries)
	}
	for acc, want := range map[string]float64{accA: 96, accB: 12} {
		w, err := e.c.Usage.WindowsOf(ctx, acc)
		if err != nil || w == nil || len(w.Windows) != 1 || w.Windows[0].UsedPct != want {
			t.Fatalf("windows of %s: %v %+v", acc, err, w)
		}
	}
	eventually(t, "control told once", func() bool { return systemToControl(t, e, sp.ID, "moved to another account") == 1 })
}

// A limit whose reset is near (inside usage_limit_wait_if_reset_within) is waited out on the same account: nothing
// is switched, the run keeps its process and is not stopped, control is told once, and when the limit clears the same
// run goes on (one cleared message, no new run, no switch).
func TestAccountSwitchWaitsWhenResetNear(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(time.Now().UTC().Truncate(time.Second))
	e := newFleetEnvClock(t, clk)
	e.enroll()
	ctx := context.Background()
	n, sp := hvNode(t, e, "asw-near", proto.MinorAccountUsage, false)
	runLimitWaits(t, e)
	if _, err := e.c.Limits.Set(ctx, admin, sp.ID, 0, limits.Input{Values: map[string]int64{limits.UsageLimitFailover: 1}}); err != nil {
		t.Fatal(err)
	}
	bindAnotherClaude(t, e, sp.ID)
	w1, gen, task := assignedWorker(t, e, n, sp.ID, "Wait it out", "")
	const session = "ses-e2e-near"

	reset := clk.Now().Add(10 * time.Minute) // inside the default 15 minutes
	limited := &proto.RunLimit{Kind: proto.LimitFiveHour, ResetsAt: &reset, Since: clk.Now()}
	report := func(status string, l *proto.RunLimit) {
		n.send(t, proto.TypeRunReport, proto.RunReport{RunID: w1.ID, SpaceID: sp.ID, Generation: gen, State: proto.RunRunning, Status: status, Turn: 2,
			SessionID: session, Limit: l, ObservedAt: time.Now().UTC()})
	}
	report("working", limited)
	report("idle", limited)
	eventually(t, "control told", func() bool { return systemToControl(t, e, sp.ID, "at its usage limit") == 1 })
	time.Sleep(300 * time.Millisecond)
	if l, _ := e.c.Fleet.AccountSwitches(ctx, sp.ID); len(l) != 0 {
		t.Fatalf("%d switches decided for a near reset", len(l))
	}
	if r := e.run1(w1.ID); r.Desired != "running" || r.Observed != proto.RunRunning {
		t.Fatalf("a run waiting for a near reset was stopped: %+v", r)
	}
	if w, _ := e.c.Fleet.LimitWaitForRun(ctx, w1.ID); w != nil {
		t.Fatalf("a kept-alive run was parked: %+v", w)
	}

	// The reset passes and the harness clears the limit: the same run continues on the same account and session.
	clk.Set(reset.Add(time.Minute))
	report("idle", &proto.RunLimit{Cleared: true, Since: limited.Since})
	eventually(t, "limit cleared", func() bool { return e.run1(w1.ID).Limit == nil })
	eventually(t, "cleared message", func() bool { return systemToControl(t, e, sp.ID, "usage limit cleared") == 1 })
	time.Sleep(300 * time.Millisecond)
	if l, _ := e.c.Fleet.AccountSwitches(ctx, sp.ID); len(l) != 0 {
		t.Fatalf("%d switches after the clear", len(l))
	}
	if c := systemToControl(t, e, sp.ID, "moved to another account"); c != 0 {
		t.Fatalf("control got %d switch messages for a wait", c)
	}
	if r := e.run1(w1.ID); r.Observed != proto.RunRunning || r.SessionID != session || e.task(sp.ID, task.ID).AssigneeRun != w1.ID {
		t.Fatalf("the run or its task moved: %+v", r)
	}
}

// The launch-time headroom gate (usage_limit_min_headroom_percent) reads the ledger's windows, which a run's usage
// report fills: a new launch skips an account whose window is nearly used and takes the next one, is refused with a
// wait when every account of the pool is below the threshold, and a running agent is never moved because of it.
func TestHeadroomGateEndToEnd(t *testing.T) {
	t.Parallel()
	e := newFleetEnv(t)
	e.enroll()
	ctx := context.Background()
	n, sp := hvNode(t, e, "asw-gate", proto.MinorAccountUsage, false)
	accB := bindAnotherClaude(t, e, sp.ID)
	if _, err := e.c.Limits.Set(ctx, admin, sp.ID, 0, limits.Input{Values: map[string]int64{limits.UsageLimitMinHeadroomPercent: 20}}); err != nil {
		t.Fatal(err)
	}
	worker := fleet.LaunchInput{Role: "free", Kind: "worker", Harness: "claude-code", Prompt: "go"}
	w1, gen := launchGen(t, e, n, sp.ID, worker)
	accA := w1.Launch.AccountRef
	n.send(t, proto.TypeRunReport, proto.RunReport{RunID: w1.ID, SpaceID: sp.ID, Generation: gen, State: proto.RunRunning, Status: "working", Turn: 1,
		SessionID: "ses-gate", ObservedAt: time.Now().UTC()})
	now := time.Now().UTC()
	resetA := now.Add(30 * time.Minute)
	report := func(runID string, used float64, resets time.Time, acc string) {
		n.send(t, proto.TypeUsage, usageOf(runID, 10, 1, proto.UsageWindow{Kind: proto.WindowFiveHour, UsedPct: used, ResetsAt: &resets, ObservedAt: time.Now().UTC()}))
		eventually(t, "window kept", func() bool {
			w, err := e.c.Usage.WindowsOf(ctx, acc)
			return err == nil && w != nil && len(w.Windows) == 1 && w.Windows[0].UsedPct == used
		})
	}

	// Exactly at the threshold (80 % used leaves 20 %) passes; 95 % does not.
	report(w1.ID, 80, resetA, accA)
	w2, err := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, worker)
	if err != nil || w2.Launch.AccountRef != accA {
		t.Fatalf("a launch at the threshold: %v account %q (A %q)", err, w2.Launch.AccountRef, accA)
	}
	report(w1.ID, 95, resetA, accA)
	w3, err := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, worker)
	if err != nil || w3.Launch.AccountRef != accB {
		t.Fatalf("a launch with A nearly used: %v account %q (B %q)", err, w3.Launch.AccountRef, accB)
	}
	if r := e.run1(w1.ID); r.Desired != "running" || r.Observed == proto.RunExited {
		t.Fatal("a running agent was moved by the headroom gate")
	}

	// B is nearly used too: the pool has no eligible account, the launch waits until the earliest reset.
	resetB := now.Add(20 * time.Minute)
	report(w3.ID, 91, resetB, accB)
	_, err = e.c.Fleet.LaunchRun(ctx, admin, sp.ID, worker)
	h, ok := fleet.AsHold(err)
	if !ok || !h.Gate || !h.Known {
		t.Fatalf("launch with the whole pool below the gate: %v", err)
	}
	if h.Until.Before(resetB.Add(-time.Minute)) || h.Until.After(resetB.Add(5*time.Minute)) {
		t.Fatalf("the wait ends %v, the earliest reset is %v", h.Until, resetB)
	}
	// A reset more than an hour away is capped: the wait ends at most an hour from now (usage.WindowMaxAge).
	farNow := time.Now().UTC()
	report(w1.ID, 96, farNow.Add(2*time.Hour), accA)
	report(w3.ID, 92, farNow.Add(3*time.Hour), accB)
	_, err = e.c.Fleet.LaunchRun(ctx, admin, sp.ID, worker)
	h, ok = fleet.AsHold(err)
	if !ok || !h.Gate {
		t.Fatalf("launch with both resets far away: %v", err)
	}
	if limit := time.Now().UTC().Add(usage.WindowMaxAge); h.Until.After(limit) {
		t.Fatalf("the wait ends %v, more than %v from now (%v)", h.Until, usage.WindowMaxAge, limit)
	}
	// The gate off (0) changes nothing.
	if _, err := e.c.Limits.Set(ctx, admin, sp.ID, 1, limits.Input{Values: map[string]int64{limits.UsageLimitMinHeadroomPercent: 0}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, worker); err != nil {
		t.Fatalf("a launch with the gate off: %v", err)
	}
}

// Pool exhaustion: the run's account hits its limit while the rest of the pool is already held. Nothing can be
// switched to, so the run is parked until the earliest reset of the pool, and control and the space's people hear of
// it. A second limited run in the same window raises no second notice.
func TestPoolExhaustionNoticedOnce(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(time.Now().UTC().Truncate(time.Second))
	e := newFleetEnvClock(t, clk)
	e.enroll()
	ctx := context.Background()
	n, sp := hvNode(t, e, "asw-pool", proto.MinorAccountUsage, false)
	runLimitWaits(t, e)
	if _, err := e.c.Limits.Set(ctx, admin, sp.ID, 0, limits.Input{Values: map[string]int64{limits.UsageLimitFailover: 1, limits.ConcurrentRuns: 4}}); err != nil {
		t.Fatal(err)
	}
	accB := bindAnotherClaude(t, e, sp.ID)
	w1, gen, _ := assignedWorker(t, e, n, sp.ID, "Pool first", "")
	accA := w1.Launch.AccountRef
	w2, gen2 := launchGen(t, e, n, sp.ID, fleet.LaunchInput{Role: "free", Kind: "worker", Harness: "claude-code", Prompt: "go", AccountRef: accA})

	// Account B is already held (the hold row is seeded directly, as in the hold tests) (its 5-hour limit, 2 hours from reset); A's limit resets later.
	now := clk.Now()
	resetB, resetA := now.Add(2*time.Hour), now.Add(8*time.Hour) // A's is beyond the keep-alive
	if err := e.c.Store.W().TxNamed(ctx, "seed", func(tx *store.Tx) error {
		_, err := tx.SQL.ExecContext(ctx, `INSERT INTO usage_limits (id, space_id, scope, account_id, kind, resets_at, source, first_run_id, seen_at, created_at, updated_at)
			VALUES ('ulm_pool_b', ?, 'account', ?, 'five_hour', ?, 'transcript', 'run_seed', ?, ?, ?)`, sp.ID, accB, resetB.UnixMicro(), now.UnixMicro(), now.UnixMicro(), now.UnixMicro())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	limited := &proto.RunLimit{Kind: proto.LimitFiveHour, ResetsAt: &resetA, Since: now}
	for i, r := range []struct {
		run *fleet.Run
		gen int64
	}{{w1, gen}, {w2, gen2}} {
		n.send(t, proto.TypeRunReport, proto.RunReport{RunID: r.run.ID, SpaceID: sp.ID, Generation: r.gen, State: proto.RunRunning, Status: "working", Turn: 2,
			SessionID: "ses-pool-" + string(rune('a'+i)), Limit: limited, ObservedAt: time.Now().UTC()})
		eventually(t, "limit stored", func() bool { return e.run1(r.run.ID).Limit != nil })
		stopAtIdle(t, n, r.run, sp.ID, r.gen, 2, "ses-pool-"+string(rune('a'+i)), limited)
		eventually(t, "parked", func() bool { return e.run1(r.run.ID).EndReason == fleet.EndUsageLimit })
	}
	// Each limit was weighed for a switch and fell back: no account was eligible, and nothing was moved.
	l, _ := e.c.Fleet.AccountSwitches(ctx, sp.ID)
	if len(l) != 2 {
		t.Fatalf("%d switch records, want 2", len(l))
	}
	for _, sw := range l {
		if sw.State != fleet.SwitchFellBack || sw.Reason != "no_eligible_account" || sw.ToAccountID != "" || sw.NewRunID != "" {
			t.Fatalf("a switch with the whole pool held: %+v", sw)
		}
	}
	for _, id := range []string{w1.ID, w2.ID} {
		w, _ := e.c.Fleet.LimitWaitForRun(ctx, id)
		if w == nil || w.SwitchToAccountID != "" || w.ResumeAt.Before(resetB) || w.ResumeAt.After(resetB.Add(5*time.Minute)) {
			t.Fatalf("the wait of %s must end at the pool's earliest reset (%v): %+v", id, resetB, w)
		}
	}
	notices := func() int {
		l, err := e.c.Human.List(ctx, sp.ID, human.ListFilter{})
		if err != nil {
			t.Fatal(err)
		}
		c := 0
		for _, a := range l {
			if strings.HasPrefix(a.DedupKey, "account-pool-exhausted:") {
				c++
			}
		}
		return c
	}
	eventually(t, "one notice", func() bool { return notices() == 1 })
	time.Sleep(300 * time.Millisecond)
	if c := notices(); c != 1 {
		t.Fatalf("%d pool-exhausted notices for two runs in one window", c)
	}
	// Control is told of each park (the notice to people is the single one).
	if c := systemToControl(t, e, sp.ID, "every account of its pool is at its usage limit"); c != 2 {
		t.Fatalf("control got %d pool-exhausted messages", c)
	}
}
