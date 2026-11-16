package control

import (
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/human"
	"github.com/aurimasniekis/go-spaceman/internal/messages"
	"github.com/aurimasniekis/go-spaceman/internal/store"
)

func (e *peopleEnv) park(spaceID, waitID, kind string, p *fleet.PoolExhausted, repark bool) {
	e.t.Helper()
	w := fleet.LimitWait{ID: waitID, SpaceID: spaceID, RunID: "run_" + waitID, RunKind: kind, Role: "implementor", Harness: "claude-code",
		ResumeAt: e.clk.Now().Add(time.Hour), CreatedAt: e.clk.Now()}
	if err := e.s.W().TxNamed(e.ctx, "park", func(tx *store.Tx) error {
		return e.sv.OnLimitWaitTx(e.ctx, tx, fleet.LimitWaitEvent{Wait: w, Change: fleet.LimitWaitParked, PoolExhausted: p, Repark: repark})
	}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *peopleEnv) poolNotices(spaceID string) []*human.Action {
	e.t.Helper()
	l, err := e.human.List(e.ctx, spaceID, human.ListFilter{})
	if err != nil {
		e.t.Fatal(err)
	}
	var out []*human.Action
	for _, a := range l {
		if strings.HasPrefix(a.DedupKey, "account-pool-exhausted:") {
			out = append(out, a)
		}
	}
	return out
}

func (e *peopleEnv) controlMessages(spaceID string) []string {
	e.t.Helper()
	rows, err := e.s.ReadDB().QueryContext(e.ctx, `SELECT subject FROM messages WHERE space_id = ? AND to_kind = 'control'`, spaceID)
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

// T10 (Q7): a park with the whole pool limited tells control (a message) and raises one reserved, informational human
// action per space and window; a park with a free account in the pool raises no action, a second park in the same
// window and space only finds the first, another window or space gets its own, and control's own wait is told to the
// people but never to control.
func TestPoolExhaustedNotices(t *testing.T) {
	e := newPeopleEnv(t)
	e.sv.msgs = messages.New(e.s, e.aw, e.ch)
	reset := e.clk.Now().Add(2 * time.Hour)
	later := e.clk.Now().Add(30 * time.Hour)
	pool := func(at *time.Time) *fleet.PoolExhausted {
		return &fleet.PoolExhausted{Harness: "claude-code", Role: "implementor", Accounts: []string{"acc_1", "acc_2"}, ResetsAt: at}
	}

	e.park("spc_a", "wt_0", "worker", nil, false)
	if n := len(e.poolNotices("spc_a")); n != 0 {
		t.Fatalf("a park without an exhausted pool raised %d notices", n)
	}
	if m := e.controlMessages("spc_a"); len(m) != 1 || !strings.Contains(m[0], "parked at its usage limit") {
		t.Fatalf("control messages %q", m)
	}

	e.park("spc_a", "wt_1", "worker", pool(&reset), false)
	l := e.poolNotices("spc_a")
	if len(l) != 1 {
		t.Fatalf("%d notices, want 1", len(l))
	}
	a := l[0]
	if a.Type != human.TypeMessage || !human.ReservedDedup(a.DedupKey) || a.Priority != "normal" ||
		a.DedupKey != PoolExhaustedDedup("spc_a", pool(&reset), e.clk.Now()) || !strings.Contains(a.Body, reset.UTC().Format(time.RFC3339)) ||
		!strings.Contains(a.Body, `"one"`) || !strings.Contains(a.Body, `"two"`) {
		t.Fatalf("notice %+v", a)
	}
	if m := e.controlMessages("spc_a"); len(m) != 2 || !strings.Contains(m[1], "every account of its pool") {
		t.Fatalf("control messages %q", m)
	}
	// Another run parked in the same window raises nothing more for the people, and control hears its own park.
	e.park("spc_a", "wt_2", "worker", pool(&reset), false)
	if n := len(e.poolNotices("spc_a")); n != 1 {
		t.Fatalf("same window raised %d notices", n)
	}
	if m := e.controlMessages("spc_a"); len(m) != 3 {
		t.Fatalf("control messages %q", m)
	}
	// A later window and another space each get their own.
	e.park("spc_a", "wt_3", "worker", pool(&later), false)
	e.park("spc_c", "wt_4", "worker", pool(&reset), false)
	if a, c := len(e.poolNotices("spc_a")), len(e.poolNotices("spc_c")); a != 2 || c != 1 {
		t.Fatalf("notices %d and %d, want 2 and 1", a, c)
	}
	// A control run's wait: the people hear about the pool; control isn't told about its own wait.
	before := len(e.controlMessages("spc_c"))
	e.park("spc_c", "wt_5", "control", pool(&later), false)
	if n := len(e.poolNotices("spc_c")); n != 2 || len(e.controlMessages("spc_c")) != before {
		t.Fatalf("control wait: %d notices, %d control messages (was %d)", n, len(e.controlMessages("spc_c")), before)
	}
}

// The people notice is one dedup key per space, harness and window, in the reserved namespace.
func TestPoolExhaustedDedupReserved(t *testing.T) {
	now := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	at := now.Add(time.Hour)
	k := PoolExhaustedDedup("spc_a", &fleet.PoolExhausted{Harness: "claude-code", ResetsAt: &at}, now)
	if !human.ReservedDedup(k) {
		t.Fatalf("%q is not reserved", k)
	}
	u := PoolExhaustedDedup("spc_a", &fleet.PoolExhausted{Harness: "claude-code"}, now)
	if !human.ReservedDedup(u) || u == k || !strings.HasSuffix(u, "day-20261114") {
		t.Fatalf("unknown-reset key %q (known %q)", u, k)
	}
}

// A launch refused by the headroom gate writes no usage-limit hold row, so its deferral must not be dropped for the lack
// of one: it holds across sweeps until the reset, while an ordinary hold's deferral ends once its row is gone.
func TestDeferStartGateRefusalHoldsUntilReset(t *testing.T) {
	e := newPeopleEnv(t)
	reset := e.clk.Now().Add(2 * time.Hour)
	e.sv.startMu.Lock()
	defer e.sv.startMu.Unlock()

	e.sv.deferStart("spc_a", &fleet.HoldError{Gate: true, Target: fleet.HoldTarget{AccountID: "acc_none"}, Until: reset, Known: true})
	for range 3 {
		if !e.sv.stillDeferred(e.ctx, "spc_a") {
			t.Fatal("a gate refusal's deferral was dropped with no hold row behind it")
		}
		e.clk.Advance(30 * time.Minute)
	}
	e.clk.Advance(time.Hour) // past the reset
	if e.sv.stillDeferred(e.ctx, "spc_a") {
		t.Fatal("still deferred after the reset")
	}

	// An unknown reset waits an hour.
	e.sv.deferStart("spc_a", &fleet.HoldError{Gate: true, Target: fleet.HoldTarget{AccountID: "acc_none"}})
	e.clk.Advance(59 * time.Minute)
	if !e.sv.stillDeferred(e.ctx, "spc_a") {
		t.Fatal("deferral with an unknown reset dropped early")
	}
	e.clk.Advance(2 * time.Minute)
	if e.sv.stillDeferred(e.ctx, "spc_a") {
		t.Fatal("deferral with an unknown reset outlived the hour")
	}

	// Without the gate flag, no open hold row ends the deferral at once.
	e.sv.deferStart("spc_a", &fleet.HoldError{Target: fleet.HoldTarget{AccountID: "acc_none"}, Until: e.clk.Now().Add(time.Hour), Known: true})
	if e.sv.stillDeferred(e.ctx, "spc_a") {
		t.Fatal("a hold's deferral outlived its row")
	}
}
