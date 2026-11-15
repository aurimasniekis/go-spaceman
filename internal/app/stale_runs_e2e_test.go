package app_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	hns "github.com/aurimasniekis/go-spaceman/internal/harness"
	"github.com/aurimasniekis/go-spaceman/internal/limits"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store"
)

// SM-LIMIT-001, SM-NODE-009, ADR-0074: a node agent restart whose runs' processes did not survive frees their slots
// without an operator. The runs in the agent's journal are reported lost in its hello; a run central still counts that
// the node never had is swept to lost once the grace passes. The space's limit then admits new workers again.
func TestNodeAgentRestartFreesRunCapacity(t *testing.T) {
	// Parallel despite restartCentral: every fleetEnv has its own data directory, central and HTTP server, so the
	// restart only drops this test's node connection.
	t.Parallel()
	e := newFleetEnv(t)
	e.staleGrace = time.Second
	e.restartCentral()
	ctx := context.Background()
	e.enroll()
	e.startAgent()
	nodes, _ := e.c.Fleet.ListNodes(ctx)
	eventually(t, "connected", func() bool { n, _ := e.c.Fleet.GetNode(ctx, nodes[0].ID); return n.Connected })
	sp, err := e.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "agent-restart", Name: "Agent restart", NodeID: nodes[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	bindStubAccounts(t, e.c, sp.ID)
	if _, err := e.c.Limits.Set(ctx, admin, sp.ID, 0, limits.Input{Values: map[string]int64{limits.ConcurrentRuns: 3}}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "lease", func() bool { s, _ := e.c.Fleet.GetSpace(ctx, sp.ID); return s.Lease != nil && s.Lease.Valid })
	launch := func() (*fleet.Run, error) {
		return e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Kind: "worker", Role: "dev", Harness: hns.KindClaude})
	}
	var runs []*fleet.Run
	for range 2 {
		r, err := launch()
		if err != nil {
			t.Fatal(err)
		}
		runs = append(runs, r)
	}
	for _, r := range runs {
		eventually(t, "running", func() bool { return e.run1(r.ID).Observed == proto.RunRunning })
	}
	// A third run central counts that the node never had (its launch was lost with the agent's state).
	orphan := "run_01orphan0000000000000000"
	if err := e.c.Store.W().TxNamed(ctx, "seed", func(tx *store.Tx) error {
		_, err := tx.SQL.ExecContext(ctx, `INSERT INTO runs (id, space_id, node_id, generation, kind, role, harness, instance_id, desired, observed,
			launch_json, created_at, created_by, updated_at, observed_at)
			SELECT ?, space_id, node_id, generation, 'worker', 'dev', harness, 'ins_orphan', 'running', 'running', '{}', created_at, 't', updated_at, observed_at
			FROM runs WHERE id = ?`, orphan, runs[0].ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	_, err = launch()
	if apperr.CodeOf(err) != apperr.CodeRateLimited || !strings.Contains(err.Error(), orphan+" (worker dev, running)") {
		t.Fatalf("launch at the limit: %v", err)
	}

	// The node agent restarts; its runs' processes did not survive.
	e.stopAgent()
	for _, r := range runs {
		_ = e.run.Remove(ctx, e.run1(r.ID).TerminalID)
	}
	e.startAgent()
	for _, r := range runs {
		eventually(t, "reported lost", func() bool { return e.run1(r.ID).Observed == proto.RunLost })
	}
	// fleetEnv does not run Serve's deadline timer, so the test drives the sweep the timer would run.
	eventually(t, "orphan swept", func() bool {
		if _, err := e.c.Fleet.SweepStaleRuns(ctx); err != nil {
			t.Fatal(err)
		}
		return e.run1(orphan).Observed == proto.RunLost
	})
	if o := e.run1(orphan); o.EndReason != fleet.EndStaleNotOnNode || o.Error == "" {
		t.Fatalf("swept orphan: %+v", o)
	}
	// The slots are free again: the space admits its full limit.
	for range 3 {
		if _, err := launch(); err != nil {
			t.Fatalf("launch after the restart: %v", err)
		}
	}
}

// ADR-0074: the sweeper leaves every run of a connection alone when one of its hello reports failed to apply (it cannot
// tell what the node has), and a later connection whose hello applied decides. Driven through the real hub.
func TestStaleRunSweepSkipsFailedHello(t *testing.T) {
	// Parallel despite restartCentral: the restart is local to this test's fleetEnv (see above).
	t.Parallel()
	e := newFleetEnv(t)
	e.staleGrace = 200 * time.Millisecond
	e.restartCentral()
	ctx := context.Background()
	e.enroll()
	n := dialRawNode(t, e, proto.MaxMinor)
	nodes, _ := e.c.Fleet.ListNodes(ctx)
	eventually(t, "connected", func() bool { nd, _ := e.c.Fleet.GetNode(ctx, nodes[0].ID); return nd.Connected })
	sp, err := e.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "failed-hello", Name: "Failed hello", NodeID: nodes[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	_ = n.conn.CloseNow()
	eventually(t, "disconnected", func() bool { nd, _ := e.c.Fleet.GetNode(ctx, nodes[0].ID); return !nd.Connected })
	// A run the node reported once and no longer has.
	gone := "run_01gone00000000000000000000"
	if err := e.c.Store.W().TxNamed(ctx, "seed", func(tx *store.Tx) error {
		_, err := tx.SQL.ExecContext(ctx, `INSERT INTO runs (id, space_id, node_id, generation, kind, role, harness, instance_id, desired, observed,
			launch_json, created_at, created_by, updated_at, observed_at) VALUES (?, ?, ?, 1, 'worker', 'dev', 'claude-code', 'ins_gone', 'running',
			'unknown', '{}', 0, 't', 0, 1)`, gone, sp.ID, nodes[0].ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	sweep := func() int {
		t.Helper()
		k, err := e.c.Fleet.SweepStaleRuns(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return k
	}

	// The node reconnects with a hello whose only report cannot apply (an unknown state).
	bad := proto.Hello{AgentVersion: "test", MinMinor: proto.MinMinor, MaxMinor: proto.MaxMinor, Leases: []proto.LeaseState{},
		Runs: []proto.RunReport{{RunID: "run_01other0000000000000000000", SpaceID: sp.ID, Generation: 1, State: "bogus"}}}
	n = dialRawNodeHello(t, e, bad)
	n.next(t, proto.TypeWelcome)
	n.next(t, proto.TypeReconcile) // sent after the hello's reports were applied
	time.Sleep(3 * e.staleGrace)
	if k := sweep(); k != 0 || e.run1(gone).Observed != "unknown" {
		t.Fatalf("a connection with a failed hello report swept %d runs: %+v", k, e.run1(gone))
	}
	_ = n.conn.CloseNow()
	eventually(t, "disconnected", func() bool { nd, _ := e.c.Fleet.GetNode(ctx, nodes[0].ID); return !nd.Connected })

	// A clean hello without the run: it is swept once the grace passes.
	n = dialRawNode(t, e, proto.MaxMinor)
	n.next(t, proto.TypeReconcile)
	eventually(t, "swept", func() bool { sweep(); return e.run1(gone).Observed == proto.RunLost })
	if r := e.run1(gone); r.EndReason != fleet.EndStaleNotOnNode || r.Desired != "stopped" {
		t.Fatalf("swept run: %+v", r)
	}
}
