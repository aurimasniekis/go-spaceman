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

// staleEnv is a fleet service on a fake clock with seeded nodes, spaces, leases and runs, for the stale-run sweeper.
type staleEnv struct {
	t     *testing.T
	ctx   context.Context
	t0    time.Time
	clk   *clock.Fake
	s     *store.Store
	m     *metrics.Metrics
	svc   *Service
	mu    sync.Mutex
	ended []string
}

func newStaleEnv(t *testing.T) *staleEnv {
	t.Helper()
	e := &staleEnv{t: t, ctx: context.Background(), t0: time.Date(2026, 11, 8, 17, 33, 0, 0, time.UTC)}
	e.clk = clock.NewFake(e.t0)
	e.s = storetest.Open(t, storetest.WithClock(e.clk))
	kr := keyringtest.Load(t, e.s)
	e.m = metrics.New()
	e.svc = New(e.s, kr, audit.NewWriter(kr), eventbus.New(), changefeed.New(nil), Options{Clock: e.clk, Metrics: e.m, DisconnectGrace: time.Minute,
		Outbox:     outbox.New(e.s, outbox.Options{Enabled: true, Source: "spaceman-test", Clock: e.clk}),
		OnRunEnded: func(id string) { e.mu.Lock(); e.ended = append(e.ended, id); e.mu.Unlock() }})
	return e
}

func (e *staleEnv) exec(q string, args ...any) {
	e.t.Helper()
	if err := e.s.W().TxNamed(e.ctx, "seed", func(tx *store.Tx) error {
		_, err := tx.SQL.ExecContext(e.ctx, q, args...)
		return err
	}); err != nil {
		e.t.Fatalf("%s: %v", q, err)
	}
}

func (e *staleEnv) count(q string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.s.ReadDB().QueryRowContext(e.ctx, q, args...).Scan(&n); err != nil {
		e.t.Fatalf("%s: %v", q, err)
	}
	return n
}

func (e *staleEnv) us(d time.Duration) int64 { return e.t0.Add(d).UnixMicro() }

// run seeds a worker run; observedAt nil means no node ever reported it.
func (e *staleEnv) run(id, space, node, observed string, observedAt any) {
	e.t.Helper()
	e.exec(`INSERT INTO runs (id, space_id, node_id, generation, kind, role, harness, instance_id, desired, observed, launch_json, created_at,
		created_by, updated_at, observed_at) VALUES (?, ?, ?, 1, 'worker', 'implementor', 'claude-code', ?, 'running', ?, '{}', ?, 't', 0, ?)`,
		id, space, node, "ins_"+id, observed, e.us(-2*time.Hour), observedAt)
}

func (e *staleEnv) command(id, node, space, run, kind, state string) {
	e.t.Helper()
	e.exec(`INSERT INTO node_commands (id, node_id, space_id, run_id, generation, kind, idempotency_key, state, created_at) VALUES (?, ?, ?, ?, 1, ?, ?, ?, 0)`,
		id, node, space, run, kind, id, state)
}

// connect registers a connection for node the way Hub.run does: the node's live runs are read at connect, the hello's
// run IDs recorded, and applied says whether every hello report applied.
func (e *staleEnv) connect(node string, applied bool, hello ...string) *session {
	e.t.Helper()
	live, err := e.s.R().ListLiveRunVersionsByNode(e.ctx, node)
	if err != nil {
		e.t.Fatal(err)
	}
	s := &session{nodeID: node, connectedAt: e.clk.Now(), liveAtConnect: map[string]int64{}, inHello: map[string]bool{}, helloApplied: applied}
	for _, r := range live {
		s.liveAtConnect[r.ID] = r.Version
	}
	for _, id := range hello {
		s.inHello[id] = true
	}
	e.svc.hub.mu.Lock()
	e.svc.hub.sessions[node] = s
	e.svc.hub.mu.Unlock()
	return s
}

func (e *staleEnv) disconnect(node string) {
	e.svc.hub.mu.Lock()
	delete(e.svc.hub.sessions, node)
	e.svc.hub.mu.Unlock()
}

func (e *staleEnv) sweep(at time.Time, want int) {
	e.t.Helper()
	e.clk.Set(at)
	n, err := e.svc.SweepStaleRuns(e.ctx)
	if err != nil || n != want {
		e.t.Fatalf("sweep at %s: %d runs, %v (want %d)", at, n, err, want)
	}
}

func (e *staleEnv) next(want time.Time) {
	e.t.Helper()
	at, err := e.svc.NextStaleRunSweep(e.ctx)
	if err != nil || !at.Equal(want) {
		e.t.Fatalf("next sweep %s, %v (want %s)", at, err, want)
	}
}

func (e *staleEnv) observed(id string) *Run {
	e.t.Helper()
	r, err := e.svc.GetRun(e.ctx, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

// ADR-0074: after a node agent restart the sweeper ends, after its grace, exactly the runs that were live when the node
// connected and that its hello did not report. It never ends a run the hello reported, a run created or changed after
// the connection began, a pending run, a run with an unacknowledged launch, a run no node ever reported, or any run of a
// connection whose hello reports did not all apply; a clock step does not move its grace. A swept run's undelivered
// commands are cancelled and its desired state becomes stopped.
func TestStaleRunSweeperNotOnNode(t *testing.T) {
	e := newStaleEnv(t)
	e.exec(`INSERT INTO nodes (id, name, state, created_at, created_by, updated_at) VALUES ('nod_1', 'n1', 'active', 0, 't', 0), ('nod_3', 'n3', 'active', 0, 't', 0)`)
	e.exec(`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES
		('spc_a', 'aaa', 'A', 'nod_1', 'active', 'a', 0, 't', 0), ('spc_d', 'ddd', 'D', 'nod_3', 'active', 'd', 0, 't', 0)`)
	e.run("run_gone", "spc_a", "nod_1", "unknown", e.us(-time.Hour))    // not in the node's hello
	e.run("run_live", "spc_a", "nod_1", "unknown", e.us(-time.Hour))    // in the hello
	e.run("run_pend", "spc_a", "nod_1", "pending", nil)                 // launch maybe in flight
	e.run("run_unrep", "spc_a", "nod_1", "unknown", nil)                // pending, then unknown on disconnect: never reported
	e.run("run_sent", "spc_a", "nod_1", "unknown", e.us(-time.Hour))    // a launch is still unacknowledged
	e.run("run_changed", "spc_a", "nod_1", "unknown", e.us(-time.Hour)) // changes after the connection began
	e.run("run_lost", "spc_a", "nod_1", "lost", e.us(-time.Hour))       // already ended
	e.run("run_d", "spc_d", "nod_3", "unknown", e.us(-time.Hour))       // its node's hello had a report that failed
	e.command("cmd_1", "nod_1", "spc_a", "run_unrep", "run.launch", "sent")
	e.command("cmd_2", "nod_1", "spc_a", "run_sent", "run.launch", "pending")
	e.command("cmd_3", "nod_1", "spc_a", "run_gone", "run.stop", "pending")
	e.exec(`INSERT INTO run_credentials (mac, run_id, created_at) VALUES (x'01', 'run_gone', 0)`)

	// nod_1 reconnects at t0 (its agent restarted); its hello reports run_live. nod_3's hello had a report that failed.
	e.connect("nod_1", true, "run_live")
	e.connect("nod_3", false)
	e.run("run_new", "spc_a", "nod_1", "unknown", e.us(-time.Hour)) // created after the connection began
	e.exec(`UPDATE runs SET version = version + 1 WHERE id = 'run_changed'`)
	// The clock steps back an hour while the hello's report is applied: the report is stored an hour before the
	// connection began. A comparison of stored times with the connection's start would take run_live for unreported.
	e.clk.Set(e.t0.Add(-time.Hour))
	if _, err := e.svc.applyReport(e.ctx, "nod_1", proto.RunReport{RunID: "run_live", SpaceID: "spc_a", Generation: 1, State: proto.RunRunning}); err != nil {
		t.Fatal(err)
	}
	// The grace runs on the clock's own reading since the connection: after the step back, it is due a grace later.
	e.next(e.t0.Add(staleRunGrace))
	e.sweep(e.t0.Add(-time.Hour+staleRunGrace-time.Second), 0)
	e.sweep(e.t0.Add(staleRunGrace-time.Second), 0)
	e.sweep(e.t0.Add(staleRunGrace), 1)

	r := e.observed("run_gone")
	if r.Observed != proto.RunLost || r.EndReason != EndStaleNotOnNode || r.Error == "" || r.EndedAt == nil || r.Desired != "stopped" {
		t.Fatalf("swept run: %+v", r)
	}
	for _, id := range []string{"run_live", "run_pend", "run_unrep", "run_sent", "run_changed", "run_new", "run_d"} {
		if r := e.observed(id); r.Observed == proto.RunLost || r.Desired != "running" {
			t.Fatalf("%s was swept: %+v", id, r)
		}
	}
	if n := e.count(`SELECT count(*) FROM node_commands WHERE id = 'cmd_3' AND state = 'cancelled'`); n != 1 {
		t.Fatal("the swept run's queued command was not cancelled")
	}
	if n := e.count(`SELECT count(*) FROM node_commands WHERE id IN ('cmd_1', 'cmd_2') AND state IN ('pending', 'sent')`); n != 2 {
		t.Fatal("an unswept run's launch was cancelled")
	}
	if n := e.count(`SELECT count(*) FROM run_credentials WHERE run_id = 'run_gone' AND revoked_at IS NOT NULL`); n != 1 {
		t.Fatal("the swept run's credential was not revoked")
	}
	if n := e.count(`SELECT count(*) FROM audit_events WHERE action = 'run.reconciled_stale' AND target_id = 'run_gone'`); n != 1 {
		t.Fatalf("audit records = %d", n)
	}
	if n := e.count(`SELECT count(*) FROM event_outbox WHERE type = ? AND correlation_id = 'run_gone'`, outbox.RunEndedName.Type()); n != 1 {
		t.Fatalf("run_ended events = %d", n)
	}
	if v := metricstest.Value(t, e.m, "spaceman_runs_reconciled_stale_total", "reason", EndStaleNotOnNode); v != 1 {
		t.Fatalf("stale_not_on_node = %v", v)
	}

	// Idempotent, and nothing else becomes due however long the connections last.
	if at, err := e.svc.NextStaleRunSweep(e.ctx); err != nil || !at.IsZero() {
		t.Fatalf("next sweep after the sweep: %s %v", at, err)
	}
	e.sweep(e.t0.Add(24*time.Hour), 0)
	e.mu.Lock()
	if len(e.ended) != 1 {
		t.Fatalf("OnRunEnded calls: %v", e.ended)
	}
	e.mu.Unlock()

	// A later connection is a new view: its hello decides, after its own grace. One that replaced the connection the
	// sweeper chose a run on makes that choice void.
	s1 := e.connect("nod_1", true, "run_live")
	cands, err := e.svc.staleRuns(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	e.connect("nod_1", true, "run_live")
	for i := range cands {
		if cands[i].session != s1 {
			continue
		}
		cands[i].ready = true
		run := e.observed(cands[i].ID)
		if skipped, err := e.svc.applyRunReport(e.ctx, "nod_1", proto.RunReport{RunID: run.ID, SpaceID: run.SpaceID, Generation: run.Generation,
			State: proto.RunLost, Reason: EndStaleNotOnNode}, &cands[i]); err != nil || !skipped {
			t.Fatalf("a sweep chosen on a replaced connection applied: skipped %v, %v", skipped, err)
		}
	}
	if len(cands) == 0 {
		t.Fatal("no candidates on the new connection")
	}
}

// ADR-0074: the runs of a node that stayed disconnected end once the space's lease, the larger of the grace it was
// granted with and the current one, and staleLeaseMargin have passed, not before; a reconnect while the sweep runs
// voids it; a node that reports a swept run later can neither revive it nor end it again.
func TestStaleRunSweeperLeaseLost(t *testing.T) {
	e := newStaleEnv(t)
	e.exec(`INSERT INTO nodes (id, name, state, created_at, created_by, updated_at) VALUES ('nod_1', 'n1', 'active', 0, 't', 0), ('nod_2', 'n2', 'active', 0, 't', 0)`)
	e.exec(`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES
		('spc_b', 'bbb', 'B', 'nod_2', 'active', 'b', 0, 't', 0), ('spc_c', 'ccc', 'C', 'nod_1', 'active', 'c', 0, 't', 0)`)
	// spc_b's lease on nod_2 expired 45 s before t0, granted with a 5-minute grace (the current one is 1 minute).
	e.exec(`INSERT INTO execution_leases (space_id, node_id, generation, state, granted_at, renewed_at, expires_at, grace_us) VALUES
		('spc_b', 'nod_2', 1, 'active', 0, 0, ?, ?), ('spc_c', 'nod_1', 1, 'active', 0, 0, ?, 0)`,
		e.us(-45*time.Second), (5 * time.Minute).Microseconds(), e.us(-45*time.Second))
	e.run("run_away", "spc_b", "nod_2", "running", e.us(-time.Hour))
	e.run("run_moved", "spc_c", "nod_2", "unknown", e.us(-time.Hour)) // its space is now on nod_1: relocation's
	e.exec(`INSERT INTO run_credentials (mac, run_id, created_at) VALUES (x'02', 'run_away', 0)`)

	due := e.t0.Add(-45*time.Second + 5*time.Minute + staleLeaseMargin)
	e.next(due)
	e.sweep(due.Add(-time.Second), 0)

	// The node reconnects between the sweeper's choice and the run's end transaction: its hello decides now.
	e.clk.Set(due)
	cands, err := e.svc.staleRuns(e.ctx)
	if err != nil || len(cands) != 1 || cands[0].ID != "run_away" || !cands[0].ready {
		t.Fatalf("candidates %+v %v", cands, err)
	}
	e.connect("nod_2", true, "run_away")
	if skipped, err := e.svc.applyRunReport(e.ctx, "nod_2", proto.RunReport{RunID: "run_away", SpaceID: "spc_b", Generation: 1, State: proto.RunLost,
		Reason: EndStaleLeaseLost}, &cands[0]); err != nil || !skipped {
		t.Fatalf("a lease-lost sweep applied after a reconnect: skipped %v, %v", skipped, err)
	}
	e.disconnect("nod_2")

	e.sweep(due, 1)
	if r := e.observed("run_away"); r.Observed != proto.RunLost || r.EndReason != EndStaleLeaseLost || r.Desired != "stopped" {
		t.Fatalf("lease-lost run: %+v", r)
	}
	if r := e.observed("run_moved"); r.Observed == proto.RunLost {
		t.Fatal("a relocated run was swept")
	}
	if v := metricstest.Value(t, e.m, "spaceman_runs_reconciled_stale_total", "reason", EndStaleLeaseLost); v != 1 {
		t.Fatalf("stale_lease_lost = %v", v)
	}

	// The lost node comes back and reports the swept run, as running, exited and lost: none revives or re-ends it.
	e.connect("nod_2", true, "run_away")
	before := e.observed("run_away")
	endedMetric := metricstest.Value(t, e.m, "spaceman_runs_ended_total", "harness", "claude-code", "reason", "lost")
	for _, st := range []string{proto.RunRunning, proto.RunExited, proto.RunLost} {
		fenced, err := e.svc.applyReport(e.ctx, "nod_2", proto.RunReport{RunID: "run_away", SpaceID: "spc_b", Generation: 1, State: st, Reason: "exited"})
		if err != nil || !fenced {
			t.Fatalf("report %s for a swept run: fenced %v, %v", st, fenced, err)
		}
	}
	if r := e.observed("run_away"); r.Observed != proto.RunLost || r.EndReason != EndStaleLeaseLost || r.Version != before.Version {
		t.Fatalf("a late report changed the swept run: %+v", r)
	}
	if v := metricstest.Value(t, e.m, "spaceman_runs_ended_total", "harness", "claude-code", "reason", "lost"); v != endedMetric {
		t.Fatalf("the swept run ended again: %v, was %v", v, endedMetric)
	}
	if n := e.count(`SELECT count(*) FROM event_outbox WHERE type = ? AND correlation_id = 'run_away'`, outbox.RunEndedName.Type()); n != 1 {
		t.Fatalf("run_ended events for the swept run = %d", n)
	}
}

// ADR-0074: a run that changed after the sweeper chose it is left alone (the report won the race).
func TestStaleRunSweeperVersionRace(t *testing.T) {
	e := newStaleEnv(t)
	e.exec(`INSERT INTO nodes (id, name, state, created_at, created_by, updated_at) VALUES ('nod_1', 'n1', 'active', 0, 't', 0)`)
	e.exec(`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('spc_a', 'aaa', 'A', 'nod_1', 'active', 'a', 0, 't', 0)`)
	e.run("run_race", "spc_a", "nod_1", "unknown", e.us(-time.Hour))
	e.connect("nod_1", true)
	e.clk.Set(e.t0.Add(staleRunGrace))
	cands, err := e.svc.staleRuns(e.ctx)
	if err != nil || len(cands) != 1 || cands[0].ID != "run_race" || !cands[0].ready {
		t.Fatalf("candidates %+v %v", cands, err)
	}
	if _, err := e.svc.applyReport(e.ctx, "nod_1", proto.RunReport{RunID: "run_race", SpaceID: "spc_a", Generation: 1, State: proto.RunRunning}); err != nil {
		t.Fatal(err)
	}
	if skipped, err := e.svc.applyRunReport(e.ctx, "nod_1", proto.RunReport{RunID: "run_race", SpaceID: "spc_a", Generation: 1, State: proto.RunLost,
		Reason: EndStaleNotOnNode}, &cands[0]); err != nil || !skipped {
		t.Fatalf("stale sweep applied over a newer report: skipped %v, %v", skipped, err)
	}
	if r := e.observed("run_race"); r.Observed != proto.RunRunning {
		t.Fatalf("raced run: %+v", r)
	}

	// A launch queued for a run after the sweeper chose it (it does not change the run's version) voids the choice too.
	e.run("run_relaunch", "spc_a", "nod_1", "unknown", e.us(-time.Hour))
	e.connect("nod_1", true, "run_race")
	e.clk.Set(e.t0.Add(2 * staleRunGrace))
	cands, err = e.svc.staleRuns(e.ctx)
	if err != nil || len(cands) != 1 || cands[0].ID != "run_relaunch" || !cands[0].ready {
		t.Fatalf("candidates %+v %v", cands, err)
	}
	e.command("cmd_l", "nod_1", "spc_a", "run_relaunch", "run.launch", "pending")
	if skipped, err := e.svc.applyRunReport(e.ctx, "nod_1", proto.RunReport{RunID: "run_relaunch", SpaceID: "spc_a", Generation: 1, State: proto.RunLost,
		Reason: EndStaleNotOnNode}, &cands[0]); err != nil || !skipped {
		t.Fatalf("stale sweep applied over a queued launch: skipped %v, %v", skipped, err)
	}
}

// ADR-0074: a hello report that fails after the sweeper chose a run (helloApplied turns false on the same connection)
// voids the choice: the end transaction re-checks it.
func TestStaleRunSweeperHelloAppliedRecheck(t *testing.T) {
	e := newStaleEnv(t)
	e.exec(`INSERT INTO nodes (id, name, state, created_at, created_by, updated_at) VALUES ('nod_1', 'n1', 'active', 0, 't', 0)`)
	e.exec(`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('spc_a', 'aaa', 'A', 'nod_1', 'active', 'a', 0, 't', 0)`)
	e.run("run_gone", "spc_a", "nod_1", "unknown", e.us(-time.Hour))
	s := e.connect("nod_1", true)
	e.clk.Set(e.t0.Add(staleRunGrace))
	cands, err := e.svc.staleRuns(e.ctx)
	if err != nil || len(cands) != 1 || cands[0].ID != "run_gone" || !cands[0].ready || cands[0].session != s {
		t.Fatalf("candidates %+v %v", cands, err)
	}
	e.svc.hub.mu.Lock()
	s.helloApplied = false
	e.svc.hub.mu.Unlock()
	if skipped, err := e.svc.applyRunReport(e.ctx, "nod_1", proto.RunReport{RunID: "run_gone", SpaceID: "spc_a", Generation: 1, State: proto.RunLost,
		Reason: EndStaleNotOnNode}, &cands[0]); err != nil || !skipped {
		t.Fatalf("a sweep applied after a hello report failed: skipped %v, %v", skipped, err)
	}
	if r := e.observed("run_gone"); r.Observed != "unknown" || r.Desired != "running" {
		t.Fatalf("run changed: %+v", r)
	}
}

// ADR-0074: a run no node ever reported (observed_at NULL) is never swept, even with its launch acknowledged (so the
// unacknowledged-launch guard does not cover it): the candidate query leaves it out, and the end transaction refuses it
// when it is chosen anyway.
func TestStaleRunSweeperNeverObserved(t *testing.T) {
	e := newStaleEnv(t)
	e.exec(`INSERT INTO nodes (id, name, state, created_at, created_by, updated_at) VALUES ('nod_1', 'n1', 'active', 0, 't', 0)`)
	e.exec(`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('spc_a', 'aaa', 'A', 'nod_1', 'active', 'a', 0, 't', 0)`)
	e.run("run_unrep", "spc_a", "nod_1", "unknown", nil)
	e.command("cmd_1", "nod_1", "spc_a", "run_unrep", "run.launch", "accepted")
	s := e.connect("nod_1", true)
	e.clk.Set(e.t0.Add(staleRunGrace))
	cands, err := e.svc.staleRuns(e.ctx)
	if err != nil || len(cands) != 0 {
		t.Fatalf("a never-reported run is a candidate: %+v %v", cands, err)
	}
	e.sweep(e.t0.Add(staleRunGrace), 0)

	// Chosen anyway (as if it had been reported when chosen): the end transaction still refuses it.
	r := e.observed("run_unrep")
	sweep := &staleRun{Reason: EndStaleNotOnNode, Due: e.t0.Add(staleRunGrace), ready: true, session: s}
	sweep.ID, sweep.NodeID, sweep.SpaceID, sweep.Version, sweep.Observed = r.ID, r.NodeID, r.SpaceID, r.Version, r.Observed
	if skipped, err := e.svc.applyRunReport(e.ctx, "nod_1", proto.RunReport{RunID: r.ID, SpaceID: r.SpaceID, Generation: r.Generation, State: proto.RunLost,
		Reason: EndStaleNotOnNode}, sweep); err != nil || !skipped {
		t.Fatalf("a never-reported run was swept: skipped %v, %v", skipped, err)
	}
	if r := e.observed("run_unrep"); r.Observed != "unknown" {
		t.Fatalf("run changed: %+v", r)
	}
}

// ADR-0074: a revoked lease is never the basis of a lease-lost sweep. Its stored expiry is the revoke time, but the
// node may hold the lease up to lease_ttl longer, so the space's runs wait for the node to report.
func TestStaleRunSweeperRevokedLease(t *testing.T) {
	e := newStaleEnv(t)
	e.exec(`INSERT INTO nodes (id, name, state, created_at, created_by, updated_at) VALUES ('nod_2', 'n2', 'active', 0, 't', 0)`)
	e.exec(`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('spc_b', 'bbb', 'B', 'nod_2', 'active', 'b', 0, 't', 0)`)
	e.exec(`INSERT INTO execution_leases (space_id, node_id, generation, state, granted_at, renewed_at, expires_at, grace_us) VALUES
		('spc_b', 'nod_2', 1, 'revoked', 0, 0, ?, 0)`, e.us(-time.Hour))
	e.run("run_away", "spc_b", "nod_2", "unknown", e.us(-2*time.Hour))
	e.next(time.Time{})
	e.sweep(e.t0.Add(24*time.Hour), 0)
	if r := e.observed("run_away"); r.Observed != "unknown" {
		t.Fatalf("a run of a revoked lease was swept: %+v", r)
	}
	// The same lease while active is swept (the test above would pass vacuously otherwise).
	e.exec(`UPDATE execution_leases SET state = 'active' WHERE space_id = 'spc_b'`)
	e.sweep(e.t0.Add(24*time.Hour), 1)
}
