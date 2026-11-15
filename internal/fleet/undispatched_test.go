package fleet

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/eventbus"
	"github.com/aurimasniekis/go-spaceman/internal/keyring/keyringtest"
	"github.com/aurimasniekis/go-spaceman/internal/obs/metrics"
	"github.com/aurimasniekis/go-spaceman/internal/obs/metrics/metricstest"
	"github.com/aurimasniekis/go-spaceman/internal/outbox"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/storetest"
)

// A run whose launch can't be dispatched ends like a run a node reports ended: the end is counted, run_ended is
// published, OnRunEnded is called and the space's draining relocation goes on. Failing it again (its launch is still
// listed while the first failure commits) only cancels its commands: nothing is counted or published twice.
func TestFailUndispatchedEndsTheRunOnce(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC))
	s := storetest.Open(t, storetest.WithClock(clk))
	kr := keyringtest.Load(t, s)
	m := metrics.New()
	var endedCalls atomic.Int32
	svc := New(s, kr, audit.NewWriter(kr), eventbus.New(), changefeed.New(nil), Options{Clock: clk, Metrics: m,
		Outbox:     outbox.New(s, outbox.Options{Enabled: true, Source: "spaceman-test", Clock: clk}),
		OnRunEnded: func(string) { endedCalls.Add(1) }})
	exec := func(q string, args ...any) {
		t.Helper()
		if err := s.W().TxNamed(ctx, "seed", func(tx *store.Tx) error {
			_, err := tx.SQL.ExecContext(ctx, q, args...)
			return err
		}); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := s.ReadDB().QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return n
	}
	exec(`INSERT INTO nodes (id, name, state, protocol_minor, created_at, created_by, updated_at) VALUES ('nod_1', 'n1', 'active', 7, 0, 't', 0)`)
	exec(`INSERT INTO nodes (id, name, state, protocol_minor, created_at, created_by, updated_at) VALUES ('nod_2', 'n2', 'active', 7, 0, 't', 0)`)
	exec(`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('spc_a', 'aaa', 'A', 'nod_1', 'active', 'a', 0, 't', 0)`)
	// The relocation stopped the space's runs; this one's launch was never sent, so only its failure ends it.
	exec(`INSERT INTO runs (id, space_id, node_id, generation, kind, role, harness, instance_id, desired, observed, launch_json, created_at, created_by, updated_at)
		VALUES ('run_1', 'spc_a', 'nod_1', 1, 'worker', 'implementor', 'claude-code', 'ins_1', 'stopped', 'pending', '{}', 0, 't', 0)`)
	exec(`INSERT INTO node_commands (id, node_id, space_id, run_id, generation, kind, idempotency_key, state, created_at)
		VALUES ('cmd_1', 'nod_1', 'spc_a', 'run_1', 1, 'run.launch', 'launch:run_1', 'pending', 0)`)
	exec(`INSERT INTO relocations (id, space_id, source_node_id, dest_node_id, mode, state, generation_before, created_at, created_by, updated_at)
		VALUES ('rel_1', 'spc_a', 'nod_1', 'nod_2', 'clean', 'draining', 1, 0, 't', 0)`)

	ended := func() float64 {
		return metricstest.Value(t, m, "spaceman_runs_ended_total", "harness", "claude-code", "reason", string(metrics.RunLaunchFailed))
	}
	runEnded := `SELECT count(*) FROM event_outbox WHERE type = ? AND correlation_id = 'run_1'`
	typ := outbox.RunEndedName.Type()

	if err := svc.failUndispatched(ctx, "run_1", "node_too_old", "the node is too old"); err != nil {
		t.Fatal(err)
	}
	r, err := svc.GetRun(ctx, "run_1")
	if err != nil || r.Observed != "failed" || r.EndReason != "node_too_old" {
		t.Fatalf("run %+v %v", r, err)
	}
	if v := ended(); v != 1 {
		t.Fatalf("runs ended (launch_failed) = %v", v)
	}
	if n := count(runEnded, typ); n != 1 {
		t.Fatalf("run_ended events = %d", n)
	}
	if n := endedCalls.Load(); n != 1 {
		t.Fatalf("OnRunEnded calls = %d", n)
	}
	deadline := time.Now().Add(10 * time.Second)
	for count(`SELECT count(*) FROM relocations WHERE id = 'rel_1' AND state = 'checking'`) != 1 {
		if time.Now().After(deadline) {
			t.Fatal("the draining relocation did not go on after the run ended")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Again, for an already-ended run with a command still pending: the command is cancelled, and that is all.
	exec(`INSERT INTO node_commands (id, node_id, space_id, run_id, generation, kind, idempotency_key, state, created_at)
		VALUES ('cmd_2', 'nod_1', 'spc_a', 'run_1', 1, 'run.launch', 'launch:run_1:again', 'pending', 0)`)
	audits := count(`SELECT count(*) FROM audit_events WHERE action = 'run.launch_failed' AND target_id = 'run_1'`)
	if err := svc.failUndispatched(ctx, "run_1", "account_unavailable", "the account is unusable"); err != nil {
		t.Fatal(err)
	}
	if n := count(`SELECT count(*) FROM node_commands WHERE run_id = 'run_1' AND state = 'cancelled'`); n != 2 {
		t.Fatalf("cancelled commands = %d", n)
	}
	if r, _ := svc.GetRun(ctx, "run_1"); r.EndReason != "node_too_old" {
		t.Fatalf("an ended run was failed again: %+v", r)
	}
	if v, n, calls := ended(), count(runEnded, typ), endedCalls.Load(); v != 1 || n != 1 || calls != 1 {
		t.Fatalf("ended twice: metric %v, run_ended events %d, OnRunEnded calls %d", v, n, calls)
	}
	if n := count(`SELECT count(*) FROM audit_events WHERE action = 'run.launch_failed' AND target_id = 'run_1'`); n != audits {
		t.Fatalf("launch_failed audited again: %d, was %d", n, audits)
	}
}
