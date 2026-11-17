package control

import (
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/messages"
	"github.com/aurimasniekis/go-spaceman/internal/store"
)

func (e *peopleEnv) restartEvent(waitID string, ev fleet.LimitWaitEvent) {
	e.t.Helper()
	ev.Wait = fleet.LimitWait{ID: waitID, SpaceID: "spc_a", RunID: "run_" + waitID, TaskID: "tsk_x", RunKind: "worker", Role: "implementor",
		Harness: "claude-code", Cause: fleet.WaitCauseRestart, ResumeAt: e.clk.Now().Add(time.Minute), CreatedAt: e.clk.Now(), LastError: fleet.WaitRestartCap}
	if err := e.s.W().TxNamed(e.ctx, "restart-notice", func(tx *store.Tx) error { return e.sv.OnLimitWaitTx(e.ctx, tx, ev) }); err != nil {
		e.t.Fatal(err)
	}
}

func (e *peopleEnv) controlBodies(spaceID string) []string {
	e.t.Helper()
	rows, err := e.s.ReadDB().QueryContext(e.ctx, `SELECT body FROM messages WHERE space_id = ? AND to_kind = 'control' ORDER BY id`, spaceID)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		e.t.Fatal(err)
	}
	return out
}

// A restart wait tells control once: the resumed text when it resumes, and the relaunch text, with how the task was
// left, when SpaceMan gives up (ADR-0098). A park is not announced.
func TestRestartCauseNotices(t *testing.T) {
	e := newPeopleEnv(t)
	e.sv.msgs = messages.New(e.s, e.aw, e.ch)

	e.restartEvent("w1", fleet.LimitWaitEvent{Change: fleet.LimitWaitParked})
	if n := len(e.controlBodies("spc_a")); n != 0 {
		t.Fatalf("a park was announced (%d)", n)
	}
	e.restartEvent("w1", fleet.LimitWaitEvent{Change: fleet.LimitWaitResumed, NewRunID: "run_new"})
	e.restartEvent("w2", fleet.LimitWaitEvent{Change: fleet.LimitWaitReleased, TaskStatus: "ready"})
	e.restartEvent("w3", fleet.LimitWaitEvent{Change: fleet.LimitWaitReleased, TaskStatus: "review"})
	got := e.controlBodies("spc_a")
	if len(got) != 3 {
		t.Fatalf("messages %q", got)
	}
	for _, want := range [][]string{
		{"stopped by an agent restart", "resumed as run run_new", "with its session", "Don't relaunch it"},
		{"relaunch", "restart_cap", "back in the queue, ready and unassigned"},
		{"relaunch", "stays in review", "waiting for acceptance"},
	} {
		found := false
		for _, b := range got {
			ok := true
			for _, w := range want {
				ok = ok && strings.Contains(b, w)
			}
			found = found || ok
		}
		if !found {
			t.Fatalf("no message with %q in %q", want, got)
		}
	}
	// A second resumed event for the same wait is deduplicated.
	e.restartEvent("w1", fleet.LimitWaitEvent{Change: fleet.LimitWaitResumed, NewRunID: "run_new"})
	if n := len(e.controlBodies("spc_a")); n != 3 {
		t.Fatalf("not deduplicated: %d", n)
	}
}
