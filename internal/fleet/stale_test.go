package fleet

import (
	"context"
	"sync"
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
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/storetest"
)

// ADR-0074: the stale-run sweeper ends, after its grace, exactly the runs with an authoritative signal that their
// process is gone (a connected node's hello did not report them; a disconnected node's lease and grace passed). It
// leaves live, pending, newly launched and relocated runs alone, ends each run once, and a node that reports a swept
// run later can neither revive it nor end it again.
func TestStaleRunSweeper(t *testing.T) {
	ctx := context.Background()
	t0 := time.Date(2026, 11, 8, 17, 33, 0, 0, time.UTC)
	clk := clock.NewFake(t0)
	s := storetest.Open(t, storetest.WithClock(clk))
	kr := keyringtest.Load(t, s)
	m := metrics.New()
	var mu sync.Mutex
	var endedRuns []string
	svc := New(s, kr, audit.NewWriter(kr), eventbus.New(), changefeed.New(nil), Options{Clock: clk, Metrics: m, DisconnectGrace: time.Minute,
		Outbox:     outbox.New(s, outbox.Options{Enabled: true, Source: "spaceman-test", Clock: clk}),
		OnRunEnded: func(id string) { mu.Lock(); endedRuns = append(endedRuns, id); mu.Unlock() }})
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
	us := func(d time.Duration) int64 { return t0.Add(d).UnixMicro() }
	exec(`INSERT INTO nodes (id, name, state, created_at, created_by, updated_at) VALUES ('nod_1', 'n1', 'active', 0, 't', 0), ('nod_2', 'n2', 'active', 0, 't', 0)`)
	exec(`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES
		('spc_a', 'aaa', 'A', 'nod_1', 'active', 'a', 0, 't', 0), ('spc_b', 'bbb', 'B', 'nod_2', 'active', 'b', 0, 't', 0),
		('spc_c', 'ccc', 'C', 'nod_1', 'active', 'c', 0, 't', 0)`)
	// spc_a's lease is on the connected node and current; spc_b's node is gone and its lease expired at t0.
	exec(`INSERT INTO execution_leases (space_id, node_id, generation, state, granted_at, renewed_at, expires_at) VALUES
		('spc_a', 'nod_1', 1, 'active', 0, ?, ?), ('spc_b', 'nod_2', 1, 'active', 0, ?, ?), ('spc_c', 'nod_1', 1, 'active', 0, ?, ?)`,
		us(0), us(time.Hour), us(-45*time.Second), us(0), us(0), us(time.Hour))
	run := func(id, space, node, desired, observed string, created int64, observedAt any) {
		t.Helper()
		exec(`INSERT INTO runs (id, space_id, node_id, generation, kind, role, harness, instance_id, desired, observed, launch_json, created_at,
			created_by, updated_at, observed_at) VALUES (?, ?, ?, 1, 'worker', 'implementor', 'claude-code', ?, ?, ?, '{}', ?, 't', 0, ?)`,
			id, space, node, "ins_"+id, desired, observed, created, observedAt)
	}
	// nod_1 reconnected at t0 (its agent restarted).
	svc.hub.sessions["nod_1"] = &session{nodeID: "nod_1", since: t0}
	run("run_gone", "spc_a", "nod_1", "running", "unknown", us(-2*time.Hour), us(-time.Hour))  // not in the node's hello
	run("run_live", "spc_a", "nod_1", "running", "running", us(-2*time.Hour), us(time.Second)) // reported in the hello
	run("run_new", "spc_a", "nod_1", "running", "unknown", us(30*time.Second), nil)            // launched after the hello
	run("run_pend", "spc_a", "nod_1", "running", "pending", us(-2*time.Hour), nil)             // launch maybe in flight
	run("run_lost", "spc_a", "nod_1", "running", "lost", us(-2*time.Hour), us(-time.Hour))     // already ended
	run("run_away", "spc_b", "nod_2", "running", "running", us(-2*time.Hour), us(-time.Hour))  // node and lease gone
	run("run_moved", "spc_c", "nod_2", "running", "unknown", us(-2*time.Hour), us(-time.Hour)) // space now on nod_1
	exec(`INSERT INTO run_credentials (mac, run_id, created_at) VALUES (x'01', 'run_gone', 0), (x'02', 'run_away', 0)`)

	sweep := func(at time.Duration, want int) {
		t.Helper()
		clk.Set(t0.Add(at))
		n, err := svc.SweepStaleRuns(ctx)
		if err != nil || n != want {
			t.Fatalf("sweep at t0+%s: %d runs, %v (want %d)", at, n, err, want)
		}
	}
	next := func(want time.Duration) {
		t.Helper()
		at, err := svc.NextStaleRunSweep(ctx)
		if err != nil || !at.Equal(t0.Add(want)) {
			t.Fatalf("next sweep %s, %v (want t0+%s)", at, err, want)
		}
	}
	stale := func(reason string) float64 {
		return metricstest.Value(t, m, "spaceman_runs_reconciled_stale_total", "reason", reason)
	}

	// Not on the node: due staleRunGrace after the connection began, not before.
	next(staleRunGrace)
	sweep(staleRunGrace-time.Second, 0)
	sweep(staleRunGrace, 1)
	r, err := svc.GetRun(ctx, "run_gone")
	if err != nil || r.Observed != proto.RunLost || r.EndReason != EndStaleNotOnNode || r.Error == "" || r.EndedAt == nil {
		t.Fatalf("swept run: %+v %v", r, err)
	}
	for _, id := range []string{"run_live", "run_new", "run_pend", "run_moved"} {
		if r, _ := svc.GetRun(ctx, id); r.Observed == proto.RunLost {
			t.Fatalf("%s was swept", id)
		}
	}
	if n := count(`SELECT count(*) FROM run_credentials WHERE run_id = 'run_gone' AND revoked_at IS NOT NULL`); n != 1 {
		t.Fatal("the swept run's credential was not revoked")
	}
	if n := count(`SELECT count(*) FROM audit_events WHERE action = 'run.reconciled_stale' AND target_id = 'run_gone'`); n != 1 {
		t.Fatalf("audit records = %d", n)
	}
	if n := count(`SELECT count(*) FROM event_outbox WHERE type = ? AND correlation_id = 'run_gone'`, outbox.RunEndedName.Type()); n != 1 {
		t.Fatalf("run_ended events = %d", n)
	}
	if v := stale(EndStaleNotOnNode); v != 1 {
		t.Fatalf("stale_not_on_node = %v", v)
	}

	// Lease lost: due when the lease, the disconnect grace and staleRunGrace have passed (t0 + 1m + 2m).
	next(time.Minute + staleRunGrace)
	sweep(time.Minute+staleRunGrace-time.Second, 0)
	sweep(time.Minute+staleRunGrace, 1)
	if r, _ := svc.GetRun(ctx, "run_away"); r.Observed != proto.RunLost || r.EndReason != EndStaleLeaseLost {
		t.Fatalf("lease-lost run: %+v", r)
	}
	if v := stale(EndStaleLeaseLost); v != 1 {
		t.Fatalf("stale_lease_lost = %v", v)
	}

	// Idempotent: nothing is due any more, and sweeping again ends nothing twice.
	if at, err := svc.NextStaleRunSweep(ctx); err != nil || !at.IsZero() {
		t.Fatalf("next sweep after everything was swept: %s %v", at, err)
	}
	sweep(time.Hour, 0)
	mu.Lock()
	if len(endedRuns) != 2 {
		t.Fatalf("OnRunEnded calls: %v", endedRuns)
	}
	mu.Unlock()

	// A run that changed after the sweeper chose it is left alone (the report won the race).
	run("run_race", "spc_a", "nod_1", "running", "unknown", us(-2*time.Hour), us(-time.Hour))
	cands, err := svc.staleRuns(ctx)
	if err != nil || len(cands) != 1 || cands[0].ID != "run_race" {
		t.Fatalf("candidates %+v %v", cands, err)
	}
	if _, err := svc.applyReport(ctx, "nod_1", proto.RunReport{RunID: "run_race", SpaceID: "spc_a", Generation: 1, State: proto.RunRunning}); err != nil {
		t.Fatal(err)
	}
	if skipped, err := svc.applyRunReport(ctx, "nod_1", proto.RunReport{RunID: "run_race", SpaceID: "spc_a", Generation: 1, State: proto.RunLost,
		Reason: EndStaleNotOnNode}, &cands[0]); err != nil || !skipped {
		t.Fatalf("stale sweep applied over a newer report: skipped %v, %v", skipped, err)
	}
	if r, _ := svc.GetRun(ctx, "run_race"); r.Observed != proto.RunRunning {
		t.Fatalf("raced run: %+v", r)
	}

	// The lost node comes back and reports the swept run, first as running, then as exited: neither revives it nor
	// ends it again.
	delete(svc.hub.sessions, "nod_1")
	svc.hub.sessions["nod_2"] = &session{nodeID: "nod_2", since: t0.Add(time.Hour)}
	before, _ := svc.GetRun(ctx, "run_away")
	endedMetric := metricstest.Value(t, m, "spaceman_runs_ended_total", "harness", "claude-code", "reason", "lost")
	for _, st := range []string{proto.RunRunning, proto.RunExited, proto.RunLost} {
		fenced, err := svc.applyReport(ctx, "nod_2", proto.RunReport{RunID: "run_away", SpaceID: "spc_b", Generation: 1, State: st, Reason: "exited"})
		if err != nil || !fenced {
			t.Fatalf("report %s for a swept run: fenced %v, %v", st, fenced, err)
		}
	}
	if r, _ := svc.GetRun(ctx, "run_away"); r.Observed != proto.RunLost || r.EndReason != EndStaleLeaseLost || r.Version != before.Version {
		t.Fatalf("a late report changed the swept run: %+v", r)
	}
	if v := metricstest.Value(t, m, "spaceman_runs_ended_total", "harness", "claude-code", "reason", "lost"); v != endedMetric {
		t.Fatalf("the swept run ended again: %v, was %v", v, endedMetric)
	}
	if n := count(`SELECT count(*) FROM event_outbox WHERE type = ? AND correlation_id = 'run_away'`, outbox.RunEndedName.Type()); n != 1 {
		t.Fatalf("run_ended events for the swept run = %d", n)
	}
}
