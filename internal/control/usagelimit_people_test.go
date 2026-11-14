package control

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/eventbus"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/human"
	"github.com/aurimasniekis/go-spaceman/internal/keyring/keyringtest"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
	"github.com/aurimasniekis/go-spaceman/internal/store/storetest"
)

// peopleEnv is a supervisor over a fleet and a human service on a fake clock, with two spaces (one account each) and
// a weekly hold in each, three days away.
type peopleEnv struct {
	t        *testing.T
	ctx      context.Context
	clk      *clock.Fake
	s        *store.Store
	human    *human.Service
	sv       *Supervisor
	perSpace atomic.Int64 // the human service's pending-actions limit per space
}

func newPeopleEnv(t *testing.T) *peopleEnv {
	t.Helper()
	e := &peopleEnv{t: t, ctx: context.Background(), clk: clock.NewFake(time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC))}
	e.s = storetest.Open(t, storetest.WithClock(e.clk))
	kr := keyringtest.Load(t, e.s)
	aw, ch := audit.NewWriter(kr), changefeed.New(nil)
	fl := fleet.New(e.s, kr, aw, eventbus.New(), ch, fleet.Options{Clock: e.clk})
	e.human = human.New(e.s, aw, ch, e.clk)
	e.human.PendingLimits = func(context.Context, *db.Queries, string) (int64, int64) { return e.perSpace.Load(), 0 }
	e.sv = NewSupervisor(fl, nil, nil, e.human, nil, e.clk)
	reset := e.clk.Now().Add(3 * 24 * time.Hour).UnixMicro()
	e.exec(`INSERT INTO nodes (id, name, state, created_at, created_by, updated_at) VALUES ('nod_1', 'n1', 'active', 0, 't', 0)`)
	e.exec(`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES
		('spc_a', 'aaa', 'A', 'nod_1', 'active', 'a', 0, 't', 0), ('spc_c', 'ccc', 'C', 'nod_1', 'active', 'c', 0, 't', 0)`)
	e.exec(`INSERT INTO accounts (id, name, kind, secret_enc, secret_rotated_at, created_at, created_by, updated_at) VALUES
		('acc_1', 'one', 'claude_oauth_token', x'00', 0, 0, 't', 0), ('acc_2', 'two', 'claude_oauth_token', x'00', 0, 0, 't', 0)`)
	e.exec(`INSERT INTO usage_limits (id, space_id, scope, account_id, kind, resets_at, source, seen_at, created_at, updated_at) VALUES
		('ulm_a', 'spc_a', 'account', 'acc_1', 'seven_day', ?, 'transcript', 0, 0, 0), ('ulm_c', 'spc_c', 'account', 'acc_2', 'seven_day', ?, 'transcript', 0, 0, 0)`,
		reset, reset)
	return e
}

func (e *peopleEnv) exec(q string, args ...any) {
	e.t.Helper()
	if err := e.s.W().TxNamed(e.ctx, "seed", func(tx *store.Tx) error {
		_, err := tx.SQL.ExecContext(e.ctx, q, args...)
		return err
	}); err != nil {
		e.t.Fatalf("%s: %v", q, err)
	}
}

// told reports whether a hold is marked told.
func (e *peopleEnv) told(id string) bool {
	e.t.Helper()
	var n int
	if err := e.s.ReadDB().QueryRowContext(e.ctx, `SELECT count(*) FROM usage_limits WHERE id = ? AND notified_humans_at IS NOT NULL`, id).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n == 1
}

// notices are a space's people notices.
func (e *peopleEnv) notices(spaceID string) []*human.Action {
	e.t.Helper()
	l, err := e.human.List(e.ctx, spaceID, human.ListFilter{})
	if err != nil {
		e.t.Fatal(err)
	}
	var out []*human.Action
	for _, a := range l {
		if strings.HasPrefix(a.DedupKey, "usage-limit-human:") {
			out = append(out, a)
		}
	}
	return out
}

func (e *peopleEnv) tell() int {
	e.t.Helper()
	n, err := e.sv.TellLongHolds(e.ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	return n
}

// ADR-0079 Q5 (UL-9, PR #63 review): one round raises one notice per long hold and marks each hold told; a later round
// raises nothing more.
func TestTellLongHolds(t *testing.T) {
	e := newPeopleEnv(t)
	e.perSpace.Store(100)
	if n := e.tell(); n != 2 {
		t.Fatalf("raised %d, want 2", n)
	}
	for _, c := range []struct{ space, hold string }{{"spc_a", "ulm_a"}, {"spc_c", "ulm_c"}} {
		l := e.notices(c.space)
		if len(l) != 1 || l[0].DedupKey != LongHoldDedup(c.hold) || l[0].CorrelationID != c.hold || !e.told(c.hold) {
			t.Fatalf("%s: notices %+v, told %v", c.space, l, e.told(c.hold))
		}
	}
	if n := e.tell(); n != 0 || len(e.notices("spc_a")) != 1 {
		t.Fatalf("second round raised %d", n)
	}
}

// A space at its pending-actions limit defers its notice: the round goes on with the other space, marks nothing in
// the full one and raises its notice on a later round, once there's room.
func TestTellLongHoldsDeferred(t *testing.T) {
	e := newPeopleEnv(t)
	e.perSpace.Store(1)
	if _, _, err := e.human.Create(e.ctx, audit.System, "spc_a", human.CreateInput{Type: human.TypeMessage, Title: "other", Body: "b",
		Request: []byte(`{"requires_ack":false}`), Requester: human.Requester{Kind: "system", Label: "x"}}); err != nil {
		t.Fatal(err)
	}
	if n := e.tell(); n != 1 {
		t.Fatalf("raised %d, want 1 (space C only)", n)
	}
	if e.told("ulm_a") || len(e.notices("spc_a")) != 0 || !e.told("ulm_c") || len(e.notices("spc_c")) != 1 {
		t.Fatalf("after a full space: A told %v (%d notices), C told %v", e.told("ulm_a"), len(e.notices("spc_a")), e.told("ulm_c"))
	}
	if n := e.tell(); n != 0 || e.told("ulm_a") {
		t.Fatalf("still full: raised %d, A told %v", n, e.told("ulm_a"))
	}
	e.perSpace.Store(100)
	if n := e.tell(); n != 1 || !e.told("ulm_a") || len(e.notices("spc_a")) != 1 {
		t.Fatalf("with room: raised %d, A told %v", n, e.told("ulm_a"))
	}
}

// A crash between raising the notice and marking the hold: the next round finds the action by its dedup key, raises no
// second one and marks the hold.
func TestTellLongHoldsAfterCrash(t *testing.T) {
	e := newPeopleEnv(t)
	e.perSpace.Store(100)
	e.exec(`UPDATE usage_limits SET notified_humans_at = 1 WHERE id = 'ulm_c'`)
	if _, created, err := e.human.Create(e.ctx, audit.System, "spc_a", human.CreateInput{Type: human.TypeMessage, Title: "t", Body: "b",
		Request: []byte(`{"requires_ack":false}`), Requester: human.Requester{Kind: "system", Label: "Usage limits"},
		DedupKey: LongHoldDedup("ulm_a")}); err != nil || !created {
		t.Fatal(created, err)
	}
	if n := e.tell(); n != 0 {
		t.Fatalf("raised %d after a crash, want 0", n)
	}
	if !e.told("ulm_a") || len(e.notices("spc_a")) != 1 {
		t.Fatalf("A told %v, %d notices", e.told("ulm_a"), len(e.notices("spc_a")))
	}
}

// The notice names the hold and the commands that clear it: this space's hold (limits clear, with its ID and the
// space), the space's holds (limits holds), and every space's holds on the account (account clear-holds, account:write).
func TestLongHoldText(t *testing.T) {
	now := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	reset := now.Add(3*24*time.Hour + 4*time.Hour)
	acct := fleet.LongHold{Hold: fleet.UsageLimit{ID: "ulm_1", SpaceID: "spc_a", Scope: fleet.LimitScopeAccount, AccountID: "acc_1", ResetsAt: &reset},
		Space: "Alpha", SpaceSlug: "alpha", Subject: `account "one" is at its weekly usage limit`, Runs: 2, Tasks: 1, NotifyAfter: 12 * time.Hour}
	title, body := longHoldText(acct, now)
	if title != `Account "one" is at its weekly usage limit until 2026-11-17 13:00 UTC` {
		t.Errorf("title %q", title)
	}
	for _, want := range []string{"In Alpha, account \"one\"", "in about 3 days 4 hours", "2 runs are waiting on it (1 with a task)",
		"`spaceman limits clear ulm_1 --space alpha` clears this hold (ulm_1) in this space only",
		"`spaceman limits holds --space alpha`", "stay refused while any other space holds it",
		"account:write runs `spaceman account clear-holds acc_1`", "longer than 12 hours"} {
		if !strings.Contains(body, want) {
			t.Errorf("account body lacks %q:\n%s", want, body)
		}
	}
	gw := acct
	gw.Hold.Scope, gw.Hold.AccountID, gw.Hold.GatewayRef, gw.Runs = fleet.LimitScopeGateway, "", "hp:prj_1", 0
	gw.Subject = "the space's Harbor Pilot project budget is exhausted"
	_, body = longHoldText(gw, now)
	for _, want := range []string{"No run of the space is waiting on it", "through the gateway", "Harbor Pilot",
		"`spaceman limits clear ulm_1 --space alpha` clears this hold (ulm_1) in this space only"} {
		if !strings.Contains(body, want) {
			t.Errorf("gateway body lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "clear-holds") {
		t.Errorf("gateway body names account clear-holds:\n%s", body)
	}
}
