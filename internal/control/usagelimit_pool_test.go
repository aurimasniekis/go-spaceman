package control

import (
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/audit"
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

func (e *peopleEnv) pendingPoolRows() int {
	e.t.Helper()
	var n int
	if err := e.s.ReadDB().QueryRowContext(e.ctx, `SELECT count(*) FROM pool_exhausted_notices`).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// A notice whose create is refused (the space's open-action cap, an archived space) stays recorded and is raised by a
// later sweep, exactly once; a sweep that finds the action already there (a crash between the create and the delete)
// only deletes the row.
func TestPoolExhaustedNoticeRetried(t *testing.T) {
	e := newPeopleEnv(t)
	e.sv.msgs = messages.New(e.s, e.aw, e.ch)
	reset := e.clk.Now().Add(2 * time.Hour)
	pool := &fleet.PoolExhausted{Harness: "claude-code", Role: "implementor", Accounts: []string{"acc_1"}, ResetsAt: &reset}

	e.perSpace.Store(1)
	if _, _, err := e.human.Create(e.ctx, audit.System, "spc_a", human.CreateInput{Type: human.TypeMessage, Title: "other", Body: "b",
		Request: []byte(`{"requires_ack":false}`), Requester: human.Requester{Kind: "system", Label: "x"}}); err != nil {
		t.Fatal(err)
	}
	e.park("spc_a", "wt_1", "worker", pool, false)
	if n := len(e.poolNotices("spc_a")); n != 0 || e.pendingPoolRows() != 1 {
		t.Fatalf("rate limited: %d notices, %d recorded, want 0 and 1", n, e.pendingPoolRows())
	}
	if n, err := e.sv.TellPoolExhausted(e.ctx); err != nil || n != 0 || e.pendingPoolRows() != 1 {
		t.Fatalf("still full: raised %d (%v), %d recorded", n, err, e.pendingPoolRows())
	}
	// Archived: refused too, and kept.
	e.exec(`UPDATE spaces SET state = 'archived' WHERE id = 'spc_a'`)
	e.perSpace.Store(100)
	if n, err := e.sv.TellPoolExhausted(e.ctx); err != nil || n != 0 || e.pendingPoolRows() != 1 {
		t.Fatalf("archived: raised %d (%v), %d recorded", n, err, e.pendingPoolRows())
	}
	e.exec(`UPDATE spaces SET state = 'active' WHERE id = 'spc_a'`)
	if n, err := e.sv.TellPoolExhausted(e.ctx); err != nil || n != 1 || e.pendingPoolRows() != 0 || len(e.poolNotices("spc_a")) != 1 {
		t.Fatalf("with room: raised %d (%v), %d recorded, %d notices", n, err, e.pendingPoolRows(), len(e.poolNotices("spc_a")))
	}
	if n, err := e.sv.TellPoolExhausted(e.ctx); err != nil || n != 0 {
		t.Fatalf("again: raised %d (%v)", n, err)
	}

	// A crash after the create, before the delete: the row is still there, the sweep finds the action and drops it.
	e.park("spc_a", "wt_2", "worker", pool, false)
	if len(e.poolNotices("spc_a")) != 1 || e.pendingPoolRows() != 0 {
		t.Fatalf("same window: %d notices, %d recorded", len(e.poolNotices("spc_a")), e.pendingPoolRows())
	}
	e.exec(`INSERT INTO pool_exhausted_notices (space_id, dedup_key, wait_id, title, body, created_at) VALUES ('spc_a', '` +
		PoolExhaustedDedup("spc_a", pool, e.clk.Now()) + `', 'wt_2', 't', 'b', 0)`)
	if n, err := e.sv.TellPoolExhausted(e.ctx); err != nil || n != 0 || e.pendingPoolRows() != 0 || len(e.poolNotices("spc_a")) != 1 {
		t.Fatalf("after a crash: raised %d (%v), %d recorded, %d notices", n, err, e.pendingPoolRows(), len(e.poolNotices("spc_a")))
	}
}

// Two roles' pools of one harness exhausted at the same reset get a notice each, naming their own role; a reset that
// moves by a few seconds stays in its window.
func TestPoolExhaustedNoticePerRole(t *testing.T) {
	e := newPeopleEnv(t)
	e.sv.msgs = messages.New(e.s, e.aw, e.ch)
	reset := e.clk.Now().Add(2 * time.Hour).Truncate(poolWindow).Add(time.Minute)
	pool := func(role string, at time.Time) *fleet.PoolExhausted {
		return &fleet.PoolExhausted{Harness: "claude-code", Role: role, Accounts: []string{"acc_1"}, ResetsAt: &at}
	}
	e.park("spc_a", "wt_1", "worker", pool("implementor", reset), false)
	e.park("spc_a", "wt_2", "worker", pool("reviewer", reset), false)
	l := e.poolNotices("spc_a")
	if len(l) != 2 || l[0].DedupKey == l[1].DedupKey {
		t.Fatalf("%d notices, want 2 with their own keys", len(l))
	}
	for _, a := range l {
		role := "implementor"
		if strings.Contains(a.DedupKey, ":reviewer:") {
			role = "reviewer"
		}
		if !strings.Contains(a.Title, role) || !strings.Contains(a.Body, "role "+role) {
			t.Errorf("notice %q does not name %s: %s", a.Title, role, a.Body)
		}
	}
	e.park("spc_a", "wt_3", "worker", pool("implementor", reset.Add(3*time.Second)), false)
	if n := len(e.poolNotices("spc_a")); n != 2 {
		t.Fatalf("a reset 3 s later raised another notice (%d)", n)
	}
}
