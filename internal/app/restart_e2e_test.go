package app_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	hns "github.com/aurimasniekis/go-spaceman/internal/harness"
	"github.com/aurimasniekis/go-spaceman/internal/runner"
)

// SM-NODE-009, SM-INV (restarts never duplicate execution): restart reconciliation beyond the plain agent/central
// restarts of TestFleetLifecycle.
//   - The agent dies as soon as the runner has started a run's program, before the journal recorded it: the
//     restarted agent finds the program by its labels and central's re-delivered launch is answered by adoption, not
//     a second start.
//   - A run that exits while the agent is down is reported as exited and is not relaunched.
//   - Central and the agent restart together: the running run is kept.
func TestRestartReconciliation(t *testing.T) {
	e := newFleetEnv(t)
	ctx := context.Background()
	e.enroll()
	e.startAgent()
	nodes, _ := e.c.Fleet.ListNodes(ctx)
	nodeID := nodes[0].ID
	eventually(t, "node connected", func() bool { n, _ := e.c.Fleet.GetNode(ctx, nodeID); return n.Connected })
	sp, err := e.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "restarts", Name: "Restarts", NodeID: nodeID})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "lease granted", func() bool { s, _ := e.c.Fleet.GetSpace(ctx, sp.ID); return s.Lease != nil && s.Lease.Valid })

	// 1. Crash between "program started" and "start recorded".
	crashed := make(chan struct{})
	var once sync.Once
	cancel, agentDone, journal := e.agentFn, e.done, e.agentJ
	e.run.AfterStart = func(p runner.Process) {
		if p.Labels["spaceman.run"] == "" {
			return // the lease guard
		}
		once.Do(func() {
			// Kill the agent and close its journal before Start returns: the launch's own bookkeeping then runs against
			// a dead agent and the start is never recorded, as after a crash. (Closing twice is safe.)
			cancel()
			select {
			case <-agentDone:
			case <-time.After(2 * time.Second):
			}
			_ = journal.Close()
			close(crashed)
		})
	}
	r, err := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Kind: "worker", Role: "implementor-backend", Harness: hns.KindClaude, Prompt: "x"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-crashed:
	case <-time.After(10 * time.Second):
		t.Fatal("run never started")
	}
	e.stopAgent()
	e.run.AfterStart = nil
	starts := e.run.StartCount()
	if n := len(e.run.LiveByLabel("spaceman.run", r.ID)); n != 1 {
		t.Fatalf("live processes after crash = %d, want 1", n)
	}
	orig := e.run.LiveByLabel("spaceman.run", r.ID)[0].ID
	e.startAgent()
	// The restarted agent finds the unrecorded program by its labels and adopts it; it never starts a second one.
	eventually(t, "crashed launch running on the original program", func() bool {
		x := e.run1(r.ID)
		return x.Observed == "running" && x.TerminalID == orig && x.Adopted
	})
	time.Sleep(600 * time.Millisecond) // heartbeats, reconcile and any command re-delivery
	if got := e.run.StartCount(); got != starts {
		t.Fatalf("adoption started the program again: starts %d → %d", starts, got)
	}
	if n := len(e.run.LiveByLabel("spaceman.run", r.ID)); n != 1 {
		t.Fatalf("live processes after adoption = %d, want 1", n)
	}

	// 2. The program exits while the agent is down: reported as exited, never relaunched.
	procs := e.run.LiveByLabel("spaceman.run", r.ID)
	e.stopAgent()
	e.run.Exit(procs[0].ID, 0, "exited")
	e.startAgent()
	eventually(t, "exit observed after restart", func() bool { return e.run1(r.ID).Observed == "exited" })
	time.Sleep(600 * time.Millisecond)
	if got := e.run.StartCount(); got != starts {
		t.Fatalf("exited run relaunched: starts %d → %d", starts, got)
	}

	// 3. Central and the agent restart together while a run is live.
	r2, err := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Kind: "worker", Role: "reviewer", Harness: hns.KindCodex, Prompt: "y"})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "second run running", func() bool { return e.run1(r2.ID).Observed == "running" })
	starts = e.run.StartCount()
	e.stopAgent()
	e.restartCentral()
	e.startAgent()
	eventually(t, "reconnected", func() bool { n, _ := e.c.Fleet.GetNode(ctx, nodeID); return n.Connected })
	eventually(t, "second run running after both restarts", func() bool { return e.run1(r2.ID).Observed == "running" })
	time.Sleep(600 * time.Millisecond)
	if got := e.run.StartCount(); got != starts {
		t.Fatalf("restart of central and agent started programs: starts %d → %d", starts, got)
	}
	if n := len(e.run.LiveByLabel("spaceman.run", r2.ID)); n != 1 {
		t.Fatalf("live processes = %d, want 1", n)
	}
}
