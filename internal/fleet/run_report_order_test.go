package fleet

import (
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// Lost update (PR #89 review, TestHarborPilotGatewayDeliveryOnNode flake): Hub.dispatch re-sends an unacknowledged
// launch on every kick, so the agent's duplicate path (agent.command) reads the journal row and reports it from the
// session goroutine while the launch goroutine moves the row to running and reports that. The two reports are not
// ordered: when the duplicate's `starting` report (read before the launch finished) reaches central after the
// `running` one, applyRunReport applies it, because nothing compares the two, and UpdateRunObserved overwrites
// terminal_id with the stale empty value. This test delivers the reports in that order and expects central to keep
// running + its terminal. Central ignores a live report behind the recorded state (ADR-0086).
func TestStaleStartingReportDoesNotOverwriteRunning(t *testing.T) {
	e := newStaleEnv(t)
	e.exec(`INSERT INTO nodes (id, name, state, created_at, created_by, updated_at) VALUES ('nod_1', 'n1', 'active', 0, 't', 0)`)
	e.exec(`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('spc_a', 'aaa', 'A', 'nod_1', 'active', 'a', 0, 't', 0)`)
	e.run("run_dup", "spc_a", "nod_1", "pending", nil)

	rep := func(state, term string) proto.RunReport {
		return proto.RunReport{RunID: "run_dup", SpaceID: "spc_a", Generation: 1, State: state, TerminalID: term}
	}
	// The duplicate path read the journal while the run was starting (no terminal yet) ...
	stale := rep(proto.RunStarting, "")
	// ... the launch goroutine then started the program and reported running ...
	if _, err := e.svc.applyReport(e.ctx, "nod_1", rep(proto.RunStarting, "")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.applyReport(e.ctx, "nod_1", rep(proto.RunRunning, "term_1")); err != nil {
		t.Fatal(err)
	}
	// ... and only now does the duplicate's delayed report arrive.
	if _, err := e.svc.applyReport(e.ctx, "nod_1", stale); err != nil {
		t.Fatal(err)
	}
	if r := e.observed("run_dup"); r.Observed != proto.RunRunning || r.TerminalID != "term_1" {
		t.Fatalf("lost update: central has observed=%q terminal_id=%q, want running / term_1", r.Observed, r.TerminalID)
	}
}

// The guard ignores only reports behind the recorded state: the forward path, a reconnect after central marked the run
// unknown, and an ended report still apply; an ignored report is not fenced (that would stop a live run), and a run
// central wants stopped still gets its stop command (ADR-0086).
func TestRunReportOrderGuard(t *testing.T) {
	type step struct {
		state, term string
		want        string // observed afterwards
		wantTerm    string
	}
	cases := []struct {
		name    string
		initial string
		steps   []step
	}{
		{"forward path", "pending", []step{
			{proto.RunStarting, "", proto.RunStarting, ""},
			{proto.RunStarting, "", proto.RunStarting, ""}, // the managed launch reports starting twice
			{proto.RunRunning, "t1", proto.RunRunning, "t1"},
			{proto.RunStopping, "t1", proto.RunStopping, "t1"},
			{proto.RunExited, "t1", proto.RunExited, "t1"},
		}},
		{"stale running after stopping", "pending", []step{
			{proto.RunRunning, "t1", proto.RunRunning, "t1"},
			{proto.RunStopping, "t1", proto.RunStopping, "t1"},
			{proto.RunRunning, "t1", proto.RunStopping, "t1"},
		}},
		{"same state without the terminal", "pending", []step{
			{proto.RunRunning, "t1", proto.RunRunning, "t1"},
			{proto.RunRunning, "", proto.RunRunning, "t1"},
		}},
		// Same rank, a new terminal: not behind the recorded state, so it applies (a `<=` in the rank check would drop it).
		{"same state with a new terminal", "pending", []step{
			{proto.RunRunning, "t1", proto.RunRunning, "t1"},
			{proto.RunRunning, "t2", proto.RunRunning, "t2"},
		}},
		// pending ranks below starting: a pending report read before the launch's starting report is behind it.
		{"stale pending after starting", "pending", []step{
			{proto.RunStarting, "", proto.RunStarting, ""},
			{proto.RunPending, "", proto.RunStarting, ""},
		}},
		{"stopped before start", "pending", []step{
			{proto.RunStarting, "", proto.RunStarting, ""},
			{proto.RunExited, "", proto.RunExited, ""},
		}},
		{"reconnect after unknown", "unknown", []step{
			{proto.RunRunning, "t1", proto.RunRunning, "t1"},
		}},
		{"adopted after a node restart", "unknown", []step{
			{proto.RunStarting, "", proto.RunStarting, ""},
			{proto.RunRunning, "t2", proto.RunRunning, "t2"},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newStaleEnv(t)
			e.exec(`INSERT INTO nodes (id, name, state, created_at, created_by, updated_at) VALUES ('nod_1', 'n1', 'active', 0, 't', 0)`)
			e.exec(`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('spc_a', 'aaa', 'A', 'nod_1', 'active', 'a', 0, 't', 0)`)
			e.run("run_g", "spc_a", "nod_1", tc.initial, nil)
			for i, s := range tc.steps {
				fenced, err := e.svc.applyReport(e.ctx, "nod_1", proto.RunReport{RunID: "run_g", SpaceID: "spc_a", Generation: 1, State: s.state, TerminalID: s.term})
				if err != nil || fenced {
					t.Fatalf("step %d: fenced=%v err=%v", i, fenced, err)
				}
				if r := e.observed("run_g"); r.Observed != s.want || r.TerminalID != s.wantTerm {
					t.Fatalf("step %d (%s/%q): observed=%q terminal=%q, want %q/%q", i, s.state, s.term, r.Observed, r.TerminalID, s.want, s.wantTerm)
				}
			}
		})
	}
}

// A stale report for a run central wants stopped is not applied but still re-sends the stop.
func TestStaleReportStillStopsUnwantedRun(t *testing.T) {
	e := newStaleEnv(t)
	e.exec(`INSERT INTO nodes (id, name, state, created_at, created_by, updated_at) VALUES ('nod_1', 'n1', 'active', 0, 't', 0)`)
	e.exec(`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('spc_a', 'aaa', 'A', 'nod_1', 'active', 'a', 0, 't', 0)`)
	e.run("run_s", "spc_a", "nod_1", "pending", nil)
	if _, err := e.svc.applyReport(e.ctx, "nod_1", proto.RunReport{RunID: "run_s", SpaceID: "spc_a", Generation: 1, State: proto.RunRunning, TerminalID: "t1"}); err != nil {
		t.Fatal(err)
	}
	e.exec(`UPDATE runs SET desired = 'stopped' WHERE id = 'run_s'`)
	fenced, err := e.svc.applyReport(e.ctx, "nod_1", proto.RunReport{RunID: "run_s", SpaceID: "spc_a", Generation: 1, State: proto.RunStarting})
	if err != nil || fenced {
		t.Fatalf("fenced=%v err=%v", fenced, err)
	}
	if r := e.observed("run_s"); r.Observed != proto.RunRunning || r.TerminalID != "t1" {
		t.Fatalf("observed=%q terminal=%q, want running/t1", r.Observed, r.TerminalID)
	}
	if n := e.count(`SELECT count(*) FROM node_commands WHERE run_id = 'run_s' AND kind = 'run.stop'`); n != 1 {
		t.Fatalf("stop commands = %d, want 1", n)
	}
}
