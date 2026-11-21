package control

import (
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/messages"
	"github.com/aurimasniekis/go-spaceman/internal/store"
)

func (e *peopleEnv) switchBackEvent(waitID string, ev fleet.LimitWaitEvent) {
	e.t.Helper()
	ev.Wait = fleet.LimitWait{ID: waitID, SpaceID: "spc_a", RunID: "run_" + waitID, RunKind: "worker", Role: "implementor",
		Harness: "claude-code", Cause: fleet.WaitCauseSwitchBack, ResumeAt: e.clk.Now(), CreatedAt: e.clk.Now(), LastError: fleet.WaitMaxWait}
	if err := e.s.W().TxNamed(e.ctx, "switch-back-notice", func(tx *store.Tx) error { return e.sv.OnLimitWaitTx(e.ctx, tx, ev) }); err != nil {
		e.t.Fatal(err)
	}
}

// A switch-back (ADR-0105) tells control once, at its resume: moved back with the switch, or kept on its account when
// the origin could no longer take it. Its park is not announced, and the resume message is deduplicated per switch.
func TestSwitchBackNotices(t *testing.T) {
	e := newPeopleEnv(t)
	e.sv.msgs = messages.New(e.s, e.aw, e.ch)
	sw := &fleet.AccountSwitch{ID: "asw_1", Cause: fleet.SwitchCauseSwitchBack, FromAccountID: "acc_b", ToAccountID: "acc_a", CreatedAt: time.Now()}

	e.switchBackEvent("w1", fleet.LimitWaitEvent{Change: fleet.LimitWaitParked, Switch: sw})
	if n := len(e.controlBodies("spc_a")); n != 0 {
		t.Fatalf("a park was announced (%d)", n)
	}
	e.switchBackEvent("w1", fleet.LimitWaitEvent{Change: fleet.LimitWaitResumed, NewRunID: "run_new", Switch: sw})
	e.switchBackEvent("w1", fleet.LimitWaitEvent{Change: fleet.LimitWaitResumed, NewRunID: "run_new", Switch: sw})
	e.switchBackEvent("w2", fleet.LimitWaitEvent{Change: fleet.LimitWaitResumed, NewRunID: "run_stay"})
	got := e.controlBodies("spc_a")
	if len(got) != 2 {
		t.Fatalf("messages %q", got)
	}
	for _, want := range [][]string{
		{"run_w1", "has reset", "continued it as run run_new on account acc_a", "with its session", "asw_1"},
		{"run_w2", "could no longer take it", "run run_stay on the same account"},
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
}
