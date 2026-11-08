package fleet

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/eventbus"
	"github.com/aurimasniekis/go-spaceman/internal/keyring/keyringtest"
	"github.com/aurimasniekis/go-spaceman/internal/obs/metrics"
	"github.com/aurimasniekis/go-spaceman/internal/outbox"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
	"github.com/aurimasniekis/go-spaceman/internal/store/storetest"
)

// gwEnv is a fleet service over a store with one space on nod_1 (lease generation 1), whose runs hold gateway tokens.
type gwEnv struct {
	t       *testing.T
	s       *store.Store
	clk     *clock.Fake
	svc     *Service
	woken   atomic.Int32
	ended   atomic.Int32
	endedID chan string
}

func newGWEnv(t *testing.T) *gwEnv {
	t.Helper()
	e := &gwEnv{t: t, clk: clock.NewFake(time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC)), endedID: make(chan string, 16)}
	e.s = storetest.Open(t, storetest.WithClock(e.clk))
	kr := keyringtest.Load(t, e.s)
	e.svc = New(e.s, kr, audit.NewWriter(kr), eventbus.New(), changefeed.New(nil), Options{Clock: e.clk, Metrics: metrics.New(),
		DisconnectGrace: 10 * time.Second, Outbox: outbox.New(e.s, outbox.Options{Enabled: true, Source: "spaceman-test", Clock: e.clk}),
		OnGatewayRevocation: func() { e.woken.Add(1) },
		OnRunEnded:          func(id string) { e.ended.Add(1); e.endedID <- id }})
	now := e.clk.Now().UnixMicro()
	e.exec(`INSERT INTO nodes (id, name, state, protocol_minor, created_at, created_by, updated_at) VALUES ('nod_1', 'n1', 'active', 8, 0, 't', 0)`)
	e.exec(`INSERT INTO nodes (id, name, state, protocol_minor, created_at, created_by, updated_at) VALUES ('nod_2', 'n2', 'active', 8, 0, 't', 0)`)
	e.exec(`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('spc_a', 'aaa', 'A', 'nod_1', 'active', 'a', 0, 't', 0)`)
	e.exec(`INSERT INTO execution_leases (space_id, node_id, generation, state, granted_at, renewed_at, expires_at) VALUES ('spc_a', 'nod_1', 1, 'active', ?, ?, ?)`,
		now, now, now+time.Minute.Microseconds())
	return e
}

func (e *gwEnv) exec(q string, args ...any) {
	e.t.Helper()
	if err := e.s.W().TxNamed(context.Background(), "seed", func(tx *store.Tx) error {
		_, err := tx.SQL.ExecContext(context.Background(), q, args...)
		return err
	}); err != nil {
		e.t.Fatalf("%s: %v", q, err)
	}
}

func (e *gwEnv) count(q string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.s.ReadDB().QueryRowContext(context.Background(), q, args...).Scan(&n); err != nil {
		e.t.Fatalf("%s: %v", q, err)
	}
	return n
}

// run inserts a gateway run in the given observed state, with a live child token crd_<id>, and its launch command.
func (e *gwEnv) run(id, observed string) {
	e.t.Helper()
	e.exec(`INSERT INTO runs (id, space_id, node_id, generation, kind, role, harness, instance_id, desired, observed, launch_json, created_at, created_by, updated_at)
		VALUES (?, 'spc_a', 'nod_1', 1, 'worker', 'implementor', 'claude-code', 'ins_1', 'running', ?, '{"gateway":{"kind":"harbor-pilot","project_id":"prj_web"}}', 0, 't', 0)`,
		id, observed)
	e.exec(`INSERT INTO run_gateway_credentials (run_id, space_id, credential_id, token_enc, expires_at, created_at) VALUES (?, 'spc_a', 'crd_'||?, x'01', ?, 0)`,
		id, id, e.clk.Now().Add(24*time.Hour).UnixMicro())
	e.exec(`INSERT INTO node_commands (id, node_id, space_id, run_id, generation, kind, idempotency_key, state, created_at)
		VALUES ('cmd_'||?, 'nod_1', 'spc_a', ?, 1, 'run.launch', 'launch:'||?, 'pending', 0)`, id, id, id)
}

// revoked checks the run's token was queued for revocation exactly once, with the reason, and its sealed token dropped.
func (e *gwEnv) revoked(id, reason string) {
	e.t.Helper()
	if n := e.count(`SELECT count(*) FROM gateway_revocations WHERE run_id = ? AND credential_id = 'crd_'||?`, id, id); n != 1 {
		e.t.Fatalf("%s: %d revocations queued, want exactly 1", id, n)
	}
	if n := e.count(`SELECT count(*) FROM gateway_revocations WHERE run_id = ? AND reason = ? AND state = 'pending'`, id, reason); n != 1 {
		e.t.Fatalf("%s: no pending revocation with reason %s", id, reason)
	}
	if n := e.count(`SELECT count(*) FROM run_gateway_credentials WHERE run_id = ? AND token_enc IS NOT NULL`, id); n != 0 {
		e.t.Fatalf("%s: sealed token kept after its revocation was queued", id)
	}
	// HP-T5: the same transaction queues the run's usage read-back, once, first due GatewayUsageFirstRead later.
	if e.count(`SELECT count(*) FROM runs WHERE id = ?`, id) == 1 {
		if n := e.count(`SELECT count(*) FROM run_gateway_usage WHERE run_id = ? AND state = 'pending' AND next_attempt_at = ended_at + ?`,
			id, GatewayUsageFirstRead.Microseconds()); n != 1 {
			e.t.Fatalf("%s: %d usage read-backs queued, want 1", id, n)
		}
	}
}

func (e *gwEnv) notRevoked(id string) {
	e.t.Helper()
	if n := e.count(`SELECT count(*) FROM gateway_revocations WHERE run_id = ?`, id); n != 0 {
		e.t.Fatalf("%s: revocation queued too early", id)
	}
	if n := e.count(`SELECT count(*) FROM run_gateway_usage WHERE run_id = ?`, id); n != 0 {
		e.t.Fatalf("%s: usage read-back queued too early", id)
	}
}

// SM-GW-005, end-path table row "node reports the run ended" (applyReport): exited, failed, lost and killed reports
// each queue the run's revocation once, in the report's transaction, and wake the revoker after the commit. A repeated
// report queues nothing more.
func TestGatewayRevokedWhenRunReportedEnded(t *testing.T) {
	t.Parallel()
	e := newGWEnv(t)
	ctx := context.Background()
	for _, c := range []struct{ id, state, reason string }{{"run_x", proto.RunExited, "exited"}, {"run_f", proto.RunFailed, "exited"},
		{"run_l", proto.RunLost, "lease_expired"}, {"run_k", proto.RunExited, "killed"}} {
		e.run(c.id, proto.RunRunning)
		rep := proto.RunReport{RunID: c.id, SpaceID: "spc_a", Generation: 1, State: c.state, Reason: c.reason}
		if _, err := e.svc.applyReport(ctx, "nod_1", rep); err != nil {
			t.Fatal(err)
		}
		e.revoked(c.id, RevokeRunEnded)
		if _, err := e.svc.applyReport(ctx, "nod_1", rep); err != nil {
			t.Fatal(err)
		}
		e.revoked(c.id, RevokeRunEnded)
	}
	// A report that the run is still running queues nothing.
	e.run("run_r", proto.RunStarting)
	if _, err := e.svc.applyReport(ctx, "nod_1", proto.RunReport{RunID: "run_r", SpaceID: "spc_a", Generation: 1, State: proto.RunRunning}); err != nil {
		t.Fatal(err)
	}
	e.notRevoked("run_r")
	if e.woken.Load() != 4 {
		t.Fatalf("revoker woken %d times, want 4", e.woken.Load())
	}
}

// SM-GW-005, row "undispatched failure" (failUndispatched): a gateway run whose launch can't be sent (gateway_unavailable,
// agent_too_old) queues its token's revocation once.
func TestGatewayRevokedWhenLaunchFails(t *testing.T) {
	t.Parallel()
	e := newGWEnv(t)
	e.run("run_1", proto.RunPending)
	if err := e.svc.failUndispatched(context.Background(), "run_1", "gateway_unavailable", "the mint failed"); err != nil {
		t.Fatal(err)
	}
	e.revoked("run_1", RevokeLaunchFail)
	if err := e.svc.failUndispatched(context.Background(), "run_1", "gateway_unavailable", "again"); err != nil {
		t.Fatal(err)
	}
	e.revoked("run_1", RevokeLaunchFail)
	if r, _ := e.svc.GetRun(context.Background(), "run_1"); r.EndReason != "gateway_unavailable" || r.Error != "the mint failed" {
		t.Fatalf("run %+v", r)
	}
}

// SM-GW-005, row "stop, cancel or pause" (stopRunTx through StopRun and SetSpaceState): the token is revoked when the
// stop is requested; the node's later report of the end queues nothing more.
func TestGatewayRevokedWhenRunStopped(t *testing.T) {
	t.Parallel()
	e := newGWEnv(t)
	ctx := context.Background()
	e.run("run_1", proto.RunRunning)
	if _, err := e.svc.StopRun(ctx, audit.System, "run_1", "interrupt", "done"); err != nil {
		t.Fatal(err)
	}
	e.revoked("run_1", RevokeRunStopped)
	if _, err := e.svc.applyReport(ctx, "nod_1", proto.RunReport{RunID: "run_1", SpaceID: "spc_a", Generation: 1, State: proto.RunExited, Reason: "stopped"}); err != nil {
		t.Fatal(err)
	}
	e.revoked("run_1", RevokeRunStopped)

	// Pausing the space stops its active runs, each revoked once.
	e.run("run_2", proto.RunRunning)
	e.run("run_3", proto.RunPending)
	sp, err := e.svc.GetSpace(ctx, "spc_a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.SetSpaceState(ctx, audit.System, "spc_a", sp.Version, "paused", PauseEmergency); err != nil {
		t.Fatal(err)
	}
	e.revoked("run_2", RevokeRunStopped)
	e.revoked("run_3", RevokeRunStopped)
}

// SM-GW-005, row "relocation, source unavailable" (finalizeRelocation): the space's live tokens are revoked once. Fixed
// with it: the runs it marks lost now end like reported runs (counted, run_ended published, OnRunEnded called).
func TestGatewayRevokedOnRelocationAndRunsEnded(t *testing.T) {
	t.Parallel()
	e := newGWEnv(t)
	ctx := context.Background()
	e.run("run_1", proto.RunRunning)
	e.run("run_2", "unknown")
	e.run("run_done", proto.RunExited)
	e.exec(`UPDATE run_gateway_credentials SET token_enc = NULL WHERE run_id = 'run_done'`) // its end path ran already
	e.exec(`UPDATE spaces SET desired_node_id = 'nod_2', state = 'paused' WHERE id = 'spc_a'`)
	e.exec(`INSERT INTO relocations (id, space_id, source_node_id, dest_node_id, mode, state, generation_before, created_at, created_by, updated_at)
		VALUES ('rel_1', 'spc_a', 'nod_1', 'nod_2', 'source_unavailable', 'finalizing', 1, 0, 't', 0)`)
	r, err := e.s.R().GetRelocation(ctx, "rel_1")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.svc.finalizeRelocation(ctx, r); err != nil {
		t.Fatal(err)
	}
	e.revoked("run_1", RevokeRelocated)
	e.revoked("run_2", RevokeRelocated)
	e.notRevoked("run_done")
	for _, id := range []string{"run_1", "run_2"} {
		if n := e.count(`SELECT count(*) FROM event_outbox WHERE type = ? AND correlation_id = ?`, outbox.RunEndedName.Type(), id); n != 1 {
			t.Fatalf("%s: %d run_ended events, want 1", id, n)
		}
		if run, _ := e.svc.GetRun(ctx, id); run.Observed != proto.RunLost || run.EndReason != relocatedSourceUnavailable {
			t.Fatalf("%s: %+v", id, run)
		}
	}
	if e.ended.Load() != 2 {
		t.Fatalf("OnRunEnded called %d times, want 2", e.ended.Load())
	}
	if n := e.count(`SELECT count(*) FROM event_outbox WHERE type = ? AND correlation_id = 'run_done'`, outbox.RunEndedName.Type()); n != 0 {
		t.Fatal("an already ended run was ended again")
	}
}

// SM-GW-005, row "space purge" (PurgeSpace): the revocations are queued before the runs are deleted, and survive it
// (the queue is not tied to the runs).
func TestGatewayRevokedOnPurge(t *testing.T) {
	t.Parallel()
	e := newGWEnv(t)
	e.run("run_1", proto.RunExited)
	e.exec(`UPDATE spaces SET state = 'archived' WHERE id = 'spc_a'`)
	if err := e.svc.PurgeSpace(context.Background(), audit.System, "spc_a"); err != nil {
		t.Fatal(err)
	}
	if e.count(`SELECT count(*) FROM runs`) != 0 || e.count(`SELECT count(*) FROM run_gateway_credentials`) != 0 {
		t.Fatal("purge kept runs or their credentials")
	}
	if e.count(`SELECT count(*) FROM gateway_revocations WHERE credential_id = 'crd_run_1' AND reason = ? AND state = 'pending'`, RevokePurged) != 1 {
		t.Fatal("purge lost the revocation")
	}
}

// SM-GW-005, row "lease loss": runs a node left unknown keep their tokens while the node may come back; once the lease
// and the disconnect grace (plus the interrupt margin) have passed, the sweep revokes them once. The runs stay unknown.
func TestGatewayLeaseLossSweep(t *testing.T) {
	t.Parallel()
	e := newGWEnv(t)
	ctx := context.Background()
	e.run("run_1", "unknown")
	e.run("run_2", proto.RunRunning) // still reported: not swept
	next, err := e.svc.NextGatewayLeaseSweep(ctx)
	want := e.clk.Now().Add(time.Minute + 10*time.Second + leaseSweepMargin)
	if err != nil || !next.Equal(want) {
		t.Fatalf("next sweep %v %v, want %v", next, err, want)
	}
	if n, err := e.svc.SweepGatewayLeases(ctx); err != nil || n != 0 {
		t.Fatalf("early sweep %d %v", n, err)
	}
	e.notRevoked("run_1")
	e.clk.Advance(time.Minute + 10*time.Second + leaseSweepMargin)
	if n, err := e.svc.SweepGatewayLeases(ctx); err != nil || n != 1 {
		t.Fatalf("sweep %d %v", n, err)
	}
	e.revoked("run_1", RevokeLeaseLost)
	e.notRevoked("run_2")
	if n, _ := e.svc.SweepGatewayLeases(ctx); n != 0 {
		t.Fatalf("second sweep %d", n)
	}
	e.revoked("run_1", RevokeLeaseLost)
	if run, _ := e.svc.GetRun(ctx, "run_1"); run.Observed != "unknown" {
		t.Fatalf("the sweep changed the run: %s", run.Observed)
	}
	if next, _ := e.svc.NextGatewayLeaseSweep(ctx); !next.IsZero() {
		t.Fatalf("nothing left to sweep, next %v", next)
	}
}

// The new end reasons: a gateway that can't give a token fails the run as gateway_unavailable, an agent that can't
// deliver one as agent_too_old (HP-T4 raises it).
func TestGatewayUndispatchableReasons(t *testing.T) {
	t.Parallel()
	if undispatchable(ErrGatewayUnavailable) != "gateway_unavailable" || undispatchable(ErrAgentTooOld) != "agent_too_old" {
		t.Fatal("end reasons")
	}
}

// fakeGateway hands out a fixed token; while block is non-nil, Token waits on it (a slow Harbor Pilot).
type fakeGateway struct {
	calls atomic.Int32
	block chan struct{}
}

func (g *fakeGateway) PreLaunch(context.Context, string, string) error { return nil }
func (g *fakeGateway) Route(context.Context, *store.Tx, string, string, string, string) (*GatewaySpec, error) {
	return nil, nil
}
func (g *fakeGateway) Token(ctx context.Context, _ *Run, _ string) (map[string]string, error) {
	g.calls.Add(1)
	if g.block != nil {
		select {
		case <-g.block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return map[string]string{proto.SecretGatewayToken: "hpk_child_fake"}, nil
}
func (g *fakeGateway) Address(context.Context) (*proto.GatewayLaunch, error) {
	return &proto.GatewayLaunch{Kind: proto.GatewayHarborPilot, BaseURL: "https://hp.example.com"}, nil
}

func launchRow(runID string) db.NodeCommand {
	return db.NodeCommand{ID: "cmd_" + runID, Kind: proto.CmdLaunch, SpaceID: "spc_a", RunID: sql.NullString{String: runID, Valid: true}, Generation: 1,
		PayloadJson: `{"harness":"claude-code","role":"implementor","kind":"worker"}`}
}

// F2 (ADR-0060 Q1): an agent older than proto.MinorGatewayToken would drop the run's token and start the harness on the
// node user's own login. A gateway launch to it fails as agent_too_old before anything is minted; a node at that minor
// gets the token in the launch's secrets.
func TestGatewayLaunchNeedsAgentMinor(t *testing.T) {
	t.Parallel()
	e := newGWEnv(t)
	g := &fakeGateway{}
	e.svc.opts.Gateway = g
	e.run("run_1", proto.RunPending)
	ctx := context.Background()
	_, err := e.svc.hub.materialize(ctx, launchRow("run_1"), "nod_1", proto.MinorGatewayToken-1)
	if !errors.Is(err, ErrAgentTooOld) || undispatchable(err) != "agent_too_old" || !strings.Contains(err.Error(), "protocol minor 9") {
		t.Fatalf("old agent: %v", err)
	}
	if g.calls.Load() != 0 {
		t.Fatal("a token was minted for an agent that would drop it")
	}
	if proto.MaxMinor < proto.MinorGatewayToken {
		t.Fatal("this agent can't deliver gateway tokens")
	}
	msg, err := e.svc.hub.materialize(ctx, launchRow("run_1"), "nod_1", proto.MaxMinor)
	if err != nil {
		t.Fatal(err)
	}
	var p proto.LaunchPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil || p.Secrets[proto.SecretGatewayToken] != "hpk_child_fake" || len(p.Secrets) != 1 {
		t.Fatalf("payload secrets %v %v", p.Secrets, err)
	}
	if p.Gateway == nil || p.Gateway.Kind != proto.GatewayHarborPilot || p.Gateway.BaseURL != "https://hp.example.com" {
		t.Fatalf("payload gateway %+v", p.Gateway)
	}
}

// F3: a slow mint never holds up the node's dispatch loop. The loop waits at most GatewayMintWait, leaves the launch
// pending and sends the node's other commands (here a stop of another run); the mint goes on in the background, once,
// and kicks the loop when it is done, which then sends the launch.
func TestGatewayMintDoesNotBlockDispatch(t *testing.T) {
	t.Parallel()
	e := newGWEnv(t)
	g := &fakeGateway{block: make(chan struct{})}
	e.svc.opts.Gateway = g
	e.svc.opts.GatewayMintWait = 50 * time.Millisecond
	e.run("run_1", proto.RunPending)
	e.exec(`UPDATE node_commands SET payload_json = ? WHERE id = 'cmd_run_1'`, launchRow("run_1").PayloadJson)
	e.exec(`INSERT INTO node_commands (id, node_id, space_id, run_id, generation, kind, idempotency_key, state, payload_json, created_at)
		VALUES ('cmd_stop', 'nod_1', 'spc_a', 'run_9', 1, 'run.stop', 'stop:run_9', 'pending', '{}', 1)`)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &session{nodeID: "nod_1", out: make(chan []byte, 64), kick: make(chan struct{}, 1), done: make(chan struct{}), log: slog.New(slog.DiscardHandler),
		minor: proto.MaxMinor}
	defer close(s.done)
	h := e.svc.hub
	h.mu.Lock()
	h.sessions["nod_1"] = s
	h.mu.Unlock()
	go h.dispatchLoop(ctx, s)
	next := func(within time.Duration) proto.Command {
		t.Helper()
		deadline := time.After(within)
		for {
			select {
			case b := <-s.out:
				env, err := proto.Decode(b)
				if err != nil {
					t.Fatal(err)
				}
				if env.Type != proto.TypeCommand {
					continue
				}
				var c proto.Command
				if err := env.Decode(&c); err != nil {
					t.Fatal(err)
				}
				return c
			case <-deadline:
				t.Fatalf("no command within %v", within)
			}
		}
	}
	start := time.Now()
	s.kick <- struct{}{}
	if c := next(5 * time.Second); c.CommandID != "cmd_stop" {
		t.Fatalf("first command sent %s, want the stop", c.CommandID)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("the stop waited %v behind the mint", d)
	}
	// Another kick while the mint is still running starts no second mint.
	s.kick <- struct{}{}
	time.Sleep(200 * time.Millisecond)
	if n := g.calls.Load(); n != 1 {
		t.Fatalf("%d mints started, want 1", n)
	}
	close(g.block)
	for {
		c := next(5 * time.Second)
		if c.CommandID == "cmd_run_1" {
			var p proto.LaunchPayload
			if err := json.Unmarshal(c.Payload, &p); err != nil || p.Secrets[proto.SecretGatewayToken] != "hpk_child_fake" {
				t.Fatalf("launch secrets %v %v", p.Secrets, err)
			}
			return
		}
	}
}

// HP-T3 review N1: only the dispatch pass that starts a mint waits for it. With several slow mints pending, a later pass
// returns at once for each of them, so a stop queued behind them is sent without waiting GatewayMintWait per launch.
func TestGatewayMintWaitsOnlyWhenStarted(t *testing.T) {
	t.Parallel()
	e := newGWEnv(t)
	g := &fakeGateway{block: make(chan struct{})}
	e.svc.opts.Gateway = g
	const wait = 400 * time.Millisecond
	e.svc.opts.GatewayMintWait = wait
	for _, id := range []string{"run_1", "run_2", "run_3"} {
		e.run(id, proto.RunPending)
		e.exec(`UPDATE node_commands SET payload_json = ? WHERE id = ?`, launchRow(id).PayloadJson, "cmd_"+id)
	}
	stop := func(id string) {
		e.exec(`INSERT INTO node_commands (id, node_id, space_id, run_id, generation, kind, idempotency_key, state, payload_json, created_at)
			VALUES (?, 'nod_1', 'spc_a', 'run_9', 1, 'run.stop', ?, 'pending', '{}', 1)`, id, "stop:"+id)
	}
	stop("cmd_stop1")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &session{nodeID: "nod_1", out: make(chan []byte, 64), kick: make(chan struct{}, 1), done: make(chan struct{}), log: slog.New(slog.DiscardHandler),
		minor: proto.MaxMinor}
	defer close(s.done)
	h := e.svc.hub
	h.mu.Lock()
	h.sessions["nod_1"] = s
	h.mu.Unlock()
	go h.dispatchLoop(ctx, s)
	waitFor := func(id string, within time.Duration) time.Duration {
		t.Helper()
		start := time.Now()
		deadline := time.After(within)
		for {
			select {
			case b := <-s.out:
				env, err := proto.Decode(b)
				if err != nil {
					t.Fatal(err)
				}
				var c proto.Command
				if env.Type == proto.TypeCommand && env.Decode(&c) == nil && c.CommandID == id {
					return time.Since(start)
				}
			case <-deadline:
				t.Fatalf("%s not sent within %v", id, within)
			}
		}
	}
	// The first pass starts the three mints and waits for each of them once.
	s.kick <- struct{}{}
	waitFor("cmd_stop1", 10*wait)
	if n := g.calls.Load(); n != 3 {
		t.Fatalf("%d mints started, want 3", n)
	}
	// The mints are still running: the next pass waits for none of them.
	stop("cmd_stop2")
	s.kick <- struct{}{}
	if d := waitFor("cmd_stop2", 10*wait); d >= wait {
		t.Fatalf("the stop waited %v behind mints already running (one wait is %v)", d, wait)
	}
	if n := g.calls.Load(); n != 3 {
		t.Fatalf("%d mints started, want 3", n)
	}
	close(g.block)
	for _, id := range []string{"cmd_run_1", "cmd_run_2", "cmd_run_3"} {
		waitFor(id, 10*wait)
	}
}

// HP-T3 review N3: central's shutdown waits for background mints before it closes the store, cancelling those that
// outlast the grace, and starts no new one.
func TestStopMintsWaitsForBackgroundMints(t *testing.T) {
	t.Parallel()
	e := newGWEnv(t)
	g := &fakeGateway{block: make(chan struct{})}
	e.svc.opts.Gateway = g
	e.svc.opts.GatewayMintWait = 10 * time.Millisecond
	e.run("run_1", proto.RunPending)
	h := e.svc.hub
	ctx := context.Background()
	run, err := e.svc.GetRun(ctx, "run_1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.gatewayToken(ctx, "nod_1", run, "run_1"); !errors.Is(err, errMintPending) {
		t.Fatalf("slow mint: %v", err)
	}
	start := time.Now()
	h.StopMints(100 * time.Millisecond) // the mint blocks until cancelled
	if d := time.Since(start); d < 100*time.Millisecond {
		t.Fatalf("returned after %v, before the grace", d)
	}
	done := make(chan struct{})
	go func() { h.mintWG.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("a background mint is still running after StopMints")
	}
	if _, err := h.gatewayToken(ctx, "nod_1", run, "run_1"); !errors.Is(err, errMintPending) || g.calls.Load() != 1 {
		t.Fatalf("after shutdown: %v, %d mints", err, g.calls.Load())
	}
}
