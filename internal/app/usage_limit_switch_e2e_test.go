package app_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/limits"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// ADR-0090, plan T3 (simulated: a scripted minor-12 node stands in for the node agent and its Claude Code, central on
// a fake clock, the deadline loop running as in Serve). A space with usage-limit failover on has two Claude accounts.
// A worker on account A reports a limit record (its 5-hour limit, three hours from reset) while busy: nothing stops it
// yet, and control isn't told to keep it alive. At its idle point central stops it; its end is usage_limit. At once a
// new run of the same instance is launched on account B with --resume of the same session and the switch prompt; its
// dispatch carries account B's credential, the task follows it, and control gets exactly one message about the switch
// (no "at its usage limit", "parked" or "resumed" messages). The new run's first turn marks the switch succeeded.
func TestUsageLimitSwitchesAccount(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(time.Now().UTC().Truncate(time.Second))
	e := newFleetEnvClock(t, clk)
	e.enroll()
	ctx := context.Background()
	n, sp := hvNode(t, e, "ulswitch", proto.MinorUsageLimit, false)
	runLimitWaits(t, e)
	if _, err := e.c.Limits.Set(ctx, admin, sp.ID, 0, limits.Input{Values: map[string]int64{limits.UsageLimitFailover: 1}}); err != nil {
		t.Fatal(err)
	}
	accB := bindAnotherClaude(t, e, sp.ID)
	w1, gen, task := assignedWorker(t, e, n, sp.ID, "Switch me", "")
	accA := w1.Launch.AccountRef
	if accA == "" || accA == accB {
		t.Fatalf("worker on account %q (B is %q)", accA, accB)
	}

	since := clk.Now()
	reset := since.Add(3 * time.Hour)
	limited := &proto.RunLimit{Kind: proto.LimitFiveHour, ResetsAt: &reset, Since: since}
	n.send(t, proto.TypeRunReport, proto.RunReport{RunID: w1.ID, SpaceID: sp.ID, Generation: gen, State: proto.RunRunning, Status: "working", Turn: 2,
		SessionID: "ses-switch", Limit: limited, ObservedAt: time.Now().UTC()})
	eventually(t, "switch armed", func() bool {
		l, _ := e.c.Fleet.AccountSwitches(ctx, sp.ID)
		return len(l) == 1 && l[0].State == fleet.SwitchPending && l[0].FromAccountID == accA && l[0].ToAccountID == accB
	})
	if r := e.run1(w1.ID); r.Desired != "running" {
		t.Fatal("a busy run was stopped")
	}

	// Its idle point: central stops it, and the node reports the stop.
	n.send(t, proto.TypeRunReport, proto.RunReport{RunID: w1.ID, SpaceID: sp.ID, Generation: gen, State: proto.RunRunning, Status: "idle", Turn: 2,
		SessionID: "ses-switch", Limit: limited, ObservedAt: time.Now().UTC()})
	n.command(t, w1.ID, proto.CmdStop)
	n.send(t, proto.TypeRunReport, proto.RunReport{RunID: w1.ID, SpaceID: sp.ID, Generation: gen, State: proto.RunExited, Reason: "stopped", Turn: 2,
		SessionID: "ses-switch", Limit: limited, ObservedAt: time.Now().UTC()})
	eventually(t, "parked", func() bool {
		r := e.run1(w1.ID)
		return r.Observed == proto.RunExited && r.EndReason == fleet.EndUsageLimit
	})
	// A round that ran before the end put the resume off (the run hadn't ended, or, while the fake clock jumps, the
	// lease looked expired): follow the wait's resume time, renewing the lease each time, until it resumes.
	eventually(t, "resumed", func() bool {
		w, _ := e.c.Fleet.LimitWaitForRun(ctx, w1.ID)
		if w == nil || w.State == fleet.WaitResumed || w.State == fleet.WaitResuming {
			return w != nil
		}
		if w.ResumeAt.After(clk.Now()) {
			clk.Set(w.ResumeAt)
		}
		n.send(t, proto.TypeHeartbeat, proto.Heartbeat{Leases: []proto.LeaseState{}})
		eventually(t, "lease renewed", func() bool { s, _ := e.c.Fleet.GetSpace(ctx, sp.ID); return s.Lease != nil && s.Lease.Valid })
		e.c.Human.Kick()
		return false
	})

	cmd, p := n.launchFor(t, w1.InstanceID, w1.ID)
	if p.AccountRef != accB || !p.Resume || p.SessionID != "ses-switch" || p.Kind != "worker" || p.Role != w1.Role ||
		!strings.Contains(p.Prompt, "moved this session to another subscription account") {
		t.Fatalf("switch launch %+v", p)
	}
	if p.Secrets["CLAUDE_CODE_OAUTH_TOKEN"] != "stub-claude-token-2" {
		t.Fatalf("the resumed run's dispatch doesn't carry account B's credential (%d secrets)", len(p.Secrets))
	}
	resumed := e.run1(cmd.RunID)
	if resumed.InstanceID != w1.InstanceID || resumed.ID == w1.ID || resumed.Launch.AccountRef != accB {
		t.Fatalf("resumed run %+v", resumed)
	}
	eventually(t, "task re-pointed", func() bool { return e.task(sp.ID, task.ID).AssigneeRun == resumed.ID })
	eventually(t, "control told once", func() bool { return systemToControl(t, e, sp.ID, "moved to another account") == 1 })

	// The new run finishes its first turn: the switch succeeded.
	n.send(t, proto.TypeRunReport, proto.RunReport{RunID: resumed.ID, SpaceID: sp.ID, Generation: cmd.Generation, State: proto.RunRunning, Status: "idle",
		Turn: 1, SessionID: "ses-switch", ObservedAt: time.Now().UTC()})
	eventually(t, "switch succeeded", func() bool {
		l, _ := e.c.Fleet.AccountSwitches(ctx, sp.ID)
		return len(l) == 1 && l[0].State == fleet.SwitchSucceeded && l[0].NewRunID == resumed.ID
	})
	time.Sleep(300 * time.Millisecond)
	for _, s := range []string{"run at its usage limit", "parked at its usage limit", "resumed after its usage limit", "run ended"} {
		if c := systemToControl(t, e, sp.ID, s); c != 0 {
			t.Errorf("control got %d %q messages for a switched run", c, s)
		}
	}
	if c := systemToControl(t, e, sp.ID, "moved to another account"); c != 1 {
		t.Fatalf("control got %d switch messages", c)
	}
	l, _ := e.c.Messages.List(ctx, sp.ID, 200)
	for _, m := range l {
		if strings.Contains(m.Subject, "moved to another account") && (!strings.Contains(m.Body, resumed.ID) || !strings.Contains(m.Body, w1.ID)) {
			t.Fatalf("switch message %q doesn't name both runs", m.Body)
		}
	}
}
