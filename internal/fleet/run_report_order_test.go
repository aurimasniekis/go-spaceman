package fleet

import (
	"os"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// Lost update (PR #89 review, TestHarborPilotGatewayDeliveryOnNode flake): Hub.dispatch re-sends an unacknowledged
// launch on every kick, so the agent's duplicate path (agent.command) reads the journal row and reports it from the
// session goroutine while the launch goroutine moves the row to running and reports that. The two reports are not
// ordered: when the duplicate's `starting` report (read before the launch finished) reaches central after the
// `running` one, applyRunReport applies it, because nothing compares the two, and UpdateRunObserved overwrites
// terminal_id with the stale empty value. This test delivers the reports in that order and expects central to keep
// running + its terminal. It fails on main, so it is skipped unless SPACEMAN_LOST_RUN_UPDATE=1 until the fix (a central
// guard or per-run ordering in the agent) is chosen; remove the skip with the fix.
func TestStaleStartingReportDoesNotOverwriteRunning(t *testing.T) {
	if os.Getenv("SPACEMAN_LOST_RUN_UPDATE") != "1" {
		t.Skip("known failing: a late duplicate-launch `starting` report overwrites `running` and clears terminal_id (set SPACEMAN_LOST_RUN_UPDATE=1 to run); awaiting the fix decision")
	}
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
