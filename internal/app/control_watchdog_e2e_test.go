package app_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/control"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	hns "github.com/aurimasniekis/go-spaceman/internal/harness"
	"github.com/aurimasniekis/go-spaceman/internal/human"
	"github.com/aurimasniekis/go-spaceman/internal/limits"
	"github.com/aurimasniekis/go-spaceman/internal/queue"
	"github.com/aurimasniekis/go-spaceman/internal/store"
)

// SM-CTRL-008, ADR-0099: a live control that leaves a worker's message unacknowledged is reminded through its terminal
// (through the node's idle-gated input, audited), never while it is busy; a tool call that leaves the message waiting
// does not end the episode; reminders that leave the backlog in place raise a human action and, with the restart switch
// on, control is stopped and started again, resuming its session.
func TestControlWatchdog(t *testing.T) {
	t.Parallel()
	e := newFleetEnv(t)
	e.enroll()
	e.startAgent()
	ctx := context.Background()
	admin := audit.Actor{Type: audit.ActorUser, ID: "usr_admin"}
	nodes, _ := e.c.Fleet.ListNodes(ctx)
	eventually(t, "node connected", func() bool { n, _ := e.c.Fleet.GetNode(ctx, nodes[0].ID); return n.Connected })
	sp, err := e.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "watch", Name: "Watch", NodeID: nodes[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	bindStubAccounts(t, e.c, sp.ID)
	eventually(t, "lease granted", func() bool { s, _ := e.c.Fleet.GetSpace(ctx, sp.ID); return s.Lease != nil && s.Lease.Valid })
	ctrl, _ := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Kind: "control", Role: "control", Harness: hns.KindClaude})
	wrk, _ := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Kind: "worker", Role: "implementor", Harness: hns.KindClaude})
	eventually(t, "runs running", func() bool { return e.run1(ctrl.ID).Observed == "running" && e.run1(wrk.ID).Observed == "running" })
	nctx, stop := context.WithCancel(ctx)
	t.Cleanup(stop)
	go e.c.Notifier.Run(nctx) // Serve runs it; the test central is not served
	term := func(runID string) string { return e.run1(runID).TerminalID }
	inputs := func() []string { return e.run.Inputs(term(ctrl.ID)) }
	e.run.SetStatus(term(ctrl.ID), "working")

	// A worker reports; control is told once (the notifier's notice) and leaves the message unacknowledged.
	if res := e.bridgeCall(wrk.ID, e.runToken(wrk.ID), "spaceman_message_send", map[string]any{"to": "control", "subject": "Done", "body": "Finished."}); !res.OK {
		t.Fatalf("send: %+v", res)
	}
	e.run.SetStatus(term(ctrl.ID), "done")
	eventually(t, "notice typed into control", func() bool { return len(inputs()) == 1 })

	// The watchdog on, with "1 minute" = 150 ms: one unanswered reminder escalates, and the restart is off at first.
	unit := 150 * time.Millisecond
	e.c.Supervisor.IdleUnit = unit
	if _, err := e.c.Limits.Set(ctx, admin, sp.ID, 0, limits.Input{Values: map[string]int64{limits.ControlWatchdog: 1, limits.ControlWatchdogNudges: 1, limits.WorkerStartWatchdog: 0}}); err != nil {
		t.Fatal(err)
	}
	nudges := func() []string {
		var out []string
		for _, in := range inputs() {
			if strings.HasPrefix(in, "SpaceMan watchdog:") {
				out = append(out, in)
			}
		}
		return out
	}

	// Busy: nothing is typed, however long it waits.
	e.run.SetStatus(term(ctrl.ID), "working")
	eventually(t, "control working", func() bool { return e.run1(ctrl.ID).Status == "working" })
	for range 3 {
		time.Sleep(unit)
		e.c.Watchdog.Pass(ctx)
	}
	if s, _ := e.c.Watchdog.State(sp.ID); len(nudges()) != 0 || s.Guard != "busy" {
		t.Fatalf("a busy control was nudged: %q, state %+v", nudges(), s)
	}

	// Idle again with the message still waiting: the reminder names the backlog by count only.
	e.run.SetStatus(term(ctrl.ID), "idle")
	eventually(t, "control idle", func() bool { return e.run1(ctrl.ID).Status == "idle" })
	// The pass queues the nudge and audits it in one transaction; the node types it into the terminal later. Passing
	// again while it is in flight would, once the 1-minute gap (150 ms) has gone by on a loaded machine, send a second
	// reminder legitimately, so pass until one is queued, then only wait for it to be typed.
	eventually(t, "watchdog nudge queued", func() bool {
		e.c.Watchdog.Pass(ctx)
		return e.auditCount("control.watchdog_nudged", audit.OutcomeSuccess) >= 1
	})
	eventually(t, "watchdog nudge typed", func() bool { return len(nudges()) >= 1 })
	if n := len(nudges()); n != 1 {
		t.Fatalf("%d nudges typed", n)
	}
	if n := nudges()[0]; !strings.HasPrefix(n, "SpaceMan watchdog: 1 unacknowledged message waiting for you.") || strings.Contains(n, "Finished") {
		t.Fatalf("nudge %q", n)
	}
	if n := e.auditCount("control.watchdog_nudged", audit.OutcomeSuccess); n != 1 {
		t.Fatalf("%d nudge audit events", n)
	}

	// A tool call is activity, so control gets the full quiet time again, but the message still waits: the episode and
	// its count go on (ADR-0099 decision 6).
	if res := e.bridgeCall(ctrl.ID, e.runToken(ctrl.ID), "spaceman_messages", nil); !res.OK {
		t.Fatalf("messages: %+v", res)
	}
	e.c.Watchdog.Pass(ctx)
	if s, _ := e.c.Watchdog.State(sp.ID); s.Guard != "recently_active" || s.Nudges != 1 {
		t.Fatalf("after a tool call: %+v", s)
	}

	// Reminders that leave the backlog escalate: people are told, and with the restart on control starts again (no
	// reminder goes into the terminal that is being stopped).
	if _, err := e.c.Limits.Set(ctx, admin, sp.ID, 1, limits.Input{Values: map[string]int64{limits.ControlWatchdog: 1, limits.ControlWatchdogNudges: 1,
		limits.ControlWatchdogRestart: 1}}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "escalated", func() bool { e.c.Watchdog.Pass(ctx); s, _ := e.c.Watchdog.State(sp.ID); return s.Escalated })
	if len(nudges()) != 1 {
		t.Fatalf("nudged into a control being restarted: %q", nudges())
	}
	acts, err := e.c.Human.List(ctx, sp.ID, human.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	raised := 0
	for _, a := range acts {
		if strings.HasPrefix(a.DedupKey, "control-watchdog:") {
			raised++
			if !strings.Contains(a.Title, "Control is not responding in Watch") || !strings.Contains(a.Body, "1 unacknowledged message") || strings.Contains(a.Body, "Finished") {
				t.Fatalf("action %q: %q", a.Title, a.Body)
			}
		}
	}
	if raised != 1 {
		t.Fatalf("%d watchdog actions", raised)
	}
	eventually(t, "stalled control stopped", func() bool { r := e.run1(ctrl.ID); return !fleet.Live(r.Observed) })
	var again *fleet.Run
	eventually(t, "control started again", func() bool {
		r, err := e.c.Fleet.LatestControlRun(ctx, sp.ID)
		if err != nil || r.ID == ctrl.ID {
			return false
		}
		again = r
		return true
	})
	if again.CreatedBy != "system:"+control.SupervisorActor.ID || again.InstanceID != ctrl.InstanceID {
		t.Fatalf("restart %+v", again)
	}
	if n := e.auditCount("run.stop_requested", audit.OutcomeSuccess); n < 1 {
		t.Fatalf("no stop audited")
	}
}

// read() leaves out ready tasks no account would launch now (every account of the pool at a usage limit): they wait
// for the reset, not for control (ADR-0099).
func TestControlWatchdogReadSkipsHeldTasks(t *testing.T) {
	t.Parallel()
	e := newFleetEnv(t)
	e.enroll()
	e.startAgent()
	ctx := context.Background()
	admin := audit.Actor{Type: audit.ActorUser, ID: "usr_admin"}
	nodes, _ := e.c.Fleet.ListNodes(ctx)
	eventually(t, "node connected", func() bool { n, _ := e.c.Fleet.GetNode(ctx, nodes[0].ID); return n.Connected })
	sp, err := e.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "held", Name: "Held", NodeID: nodes[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	accs := bindStubAccounts(t, e.c, sp.ID)
	eventually(t, "lease granted", func() bool { s, _ := e.c.Fleet.GetSpace(ctx, sp.ID); return s.Lease != nil && s.Lease.Valid })
	ctrl, _ := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Kind: "control", Role: "control", Harness: hns.KindClaude})
	eventually(t, "control running", func() bool { return e.run1(ctrl.ID).Observed == "running" })
	e.run.SetStatus(e.run1(ctrl.ID).TerminalID, "idle")
	unit := 20 * time.Millisecond
	e.c.Supervisor.IdleUnit = unit
	if _, err := e.c.Limits.Set(ctx, admin, sp.ID, 0, limits.Input{Values: map[string]int64{limits.ControlWatchdog: 1, limits.ControlWatchdogNudges: 0, limits.WorkerStartWatchdog: 0}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.c.Queue.Create(ctx, admin, sp.ID, queue.CreateInput{Title: "Waiting", Status: queue.Ready}); err != nil {
		t.Fatal(err)
	}
	startable := func() int {
		e.c.Watchdog.Pass(ctx)
		s, _ := e.c.Watchdog.State(sp.ID)
		return s.Backlog.Startable
	}
	eventually(t, "a ready task is startable", func() bool { return startable() == 1 })
	now := time.Now().UTC()
	i := 0
	for _, acc := range accs {
		i++
		if err := e.c.Store.W().TxNamed(ctx, "seed", func(tx *store.Tx) error {
			_, err := tx.SQL.ExecContext(ctx, `INSERT INTO usage_limits (id, space_id, scope, account_id, kind, resets_at, source, first_run_id, seen_at, created_at, updated_at)
				VALUES (?, ?, 'account', ?, 'five_hour', ?, 'transcript', 'run_seed', ?, ?, ?)`, fmt.Sprintf("ulm_held_%d", i), sp.ID, acc,
				now.Add(2*time.Hour).UnixMicro(), now.UnixMicro(), now.UnixMicro(), now.UnixMicro())
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	if n := startable(); n != 0 {
		t.Fatalf("a ready task no account would launch counted: %d", n)
	}
}
