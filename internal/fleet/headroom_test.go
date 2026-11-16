package fleet

import (
	"context"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
	"github.com/aurimasniekis/go-spaceman/internal/usage"
)

// usageStub is the usage ledger of a test: used percentage and reset (offset from t0) per account.
type usageStub struct {
	used  map[string]float64
	reset map[string]time.Duration
}

// headroomEnv is holdEnv with a settable headroom gate and a stub for the ledger. Space A binds acc_1 (default) and
// acc_2.
func headroomEnv(t *testing.T, minPct int, stub *usageStub) *holdEnv {
	t.Helper()
	e := newHoldEnv(t)
	e.svc.opts.UsageLimitSettings = func(context.Context, *db.Queries, string) UsageLimitSettings {
		return UsageLimitSettings{Failover: e.failover, KeepAliveMax: e.keepMax, MinHeadroomPercent: minPct}
	}
	e.svc.opts.AccountHeadroom = func(_ context.Context, _ *db.Queries, id string) (float64, time.Time, bool, error) {
		u, ok := stub.used[id]
		if !ok {
			return 0, time.Time{}, false, nil
		}
		var at time.Time
		if d, ok := stub.reset[id]; ok {
			at = e.t0.Add(d)
		}
		return u, at, true, nil
	}
	return e
}

func TestHeadroomGateOffByDefault(t *testing.T) {
	// A usage of 100 % changes nothing while the gate is 0, even with an AccountHeadroom source.
	e := headroomEnv(t, 0, &usageStub{used: map[string]float64{"acc_1": 100, "acc_2": 100}})
	if r, err := e.launch("spc_a", ""); err != nil || r.Launch.AccountRef != "acc_1" {
		t.Fatalf("gate off: %v %+v", err, r)
	}
	// And no source at all, with a gate set: nothing is known, nothing is refused.
	e.svc.opts.AccountHeadroom = nil
	e.svc.opts.UsageLimitSettings = func(context.Context, *db.Queries, string) UsageLimitSettings {
		return UsageLimitSettings{MinHeadroomPercent: 50}
	}
	if r, err := e.launch("spc_a", ""); err != nil || r.Launch.AccountRef != "acc_1" {
		t.Fatalf("no source: %v %+v", err, r)
	}
}

func TestHeadroomGateBoundary(t *testing.T) {
	for _, tc := range []struct {
		name string
		used float64
		want string // account of the launch; "" = refused
	}{
		{"above the threshold", 79, "acc_1"},
		{"exactly at the threshold", 80, "acc_1"}, // 20 % free is "at least 20 %"
		{"just below", 80.5, "acc_2"},
		{"full", 100, "acc_2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := headroomEnv(t, 20, &usageStub{used: map[string]float64{"acc_1": tc.used}})
			r, err := e.launch("spc_a", "")
			if err != nil || r.Launch.AccountRef != tc.want {
				t.Fatalf("%v %+v", err, r)
			}
			// A resume naming its old account moves off it the same way.
			if r, err := e.launch("spc_a", "acc_1"); err != nil || r.Launch.AccountRef != tc.want {
				t.Fatalf("named: %v %+v", err, r)
			}
		})
	}
}

// An account without known usage is eligible; an account with known usage below the gate is skipped for it.
func TestHeadroomGateUnknownUsageIsEligible(t *testing.T) {
	e := headroomEnv(t, 50, &usageStub{used: map[string]float64{"acc_1": 95}}) // acc_2 unknown
	if r, err := e.launch("spc_a", ""); err != nil || r.Launch.AccountRef != "acc_2" {
		t.Fatalf("%v %+v", err, r)
	}
	// Nothing known anywhere: the usual choice.
	e = headroomEnv(t, 50, &usageStub{})
	if r, err := e.launch("spc_a", ""); err != nil || r.Launch.AccountRef != "acc_1" {
		t.Fatalf("%v %+v", err, r)
	}
}

// No account of the pool passes: the launch is refused until the earliest reset, writing nothing.
func TestHeadroomGateNoEligibleAccount(t *testing.T) {
	e := headroomEnv(t, 20, &usageStub{
		used:  map[string]float64{"acc_1": 95, "acc_2": 99},
		reset: map[string]time.Duration{"acc_1": 3 * time.Hour, "acc_2": time.Hour},
	})
	before := e.writes()
	_, err := e.launch("spc_a", "")
	h := wantHold(t, err, "headroom gate", "20%", "2026-11-08 18:33 UTC")
	if !h.Known || !h.Until.Equal(e.t0.Add(time.Hour).UTC()) {
		t.Fatalf("until %v known %v", h.Until, h.Known)
	}
	if e.writes() != before {
		t.Fatal("a refused launch wrote rows")
	}
	// Unknown resets: still refused, with no time.
	e = headroomEnv(t, 20, &usageStub{used: map[string]float64{"acc_1": 95, "acc_2": 99}})
	if h := wantHold(t, mustErr(e.launch("spc_a", "")), "headroom gate"); h.Known {
		t.Fatalf("known reset %+v", h)
	}
}

func mustErr(_ *Run, err error) error { return err }

// A scoped role pool never widens: with every pool account below the gate the launch is refused, even though the
// space binds an account outside the pool with plenty of headroom.
func TestHeadroomGateScopedPoolNeverWidens(t *testing.T) {
	e := headroomEnv(t, 20, &usageStub{used: map[string]float64{"acc_1": 95, "acc_2": 10}})
	e.exec(`INSERT INTO space_role_accounts (space_id, role, account_id, priority, created_at, created_by) VALUES ('spc_a', 'implementor', 'acc_1', 100, 0, 't')`)
	_, err := e.launch("spc_a", "")
	wantHold(t, err, "headroom gate")
	if n := e.count(`SELECT count(*) FROM runs`); n != 0 {
		t.Fatalf("%d runs", n)
	}
	// With room in the pool it is used, and a pool of two takes the one with room.
	e.exec(`INSERT INTO space_role_accounts (space_id, role, account_id, priority, created_at, created_by) VALUES ('spc_a', 'implementor', 'acc_2', 200, 0, 't')`)
	if r, err := e.launch("spc_a", ""); err != nil || r.Launch.AccountRef != "acc_2" {
		t.Fatalf("pool of two: %v %+v", err, r)
	}
}

// The gate removes accounts and never adds a held one back: a held account is still refused by its hold.
func TestHeadroomGateAndHoldTogether(t *testing.T) {
	e := headroomEnv(t, 20, &usageStub{used: map[string]float64{"acc_1": 95}})
	e.hold("ulm_1", "spc_b", "acc_2", "", "five_hour", dur(time.Hour), nil)
	e.failover = true
	// acc_1 is below the gate and acc_2 is held: nothing is eligible, and the refusal is one of the two.
	if _, err := e.launch("spc_a", ""); err == nil {
		t.Fatal("launched with every account gated or held")
	}
}

// A running agent is never moved because of the gate: its run, account and launch are untouched, and lowering an
// account's headroom while it runs does nothing.
func TestHeadroomGateLeavesRunningRunsAlone(t *testing.T) {
	stub := &usageStub{used: map[string]float64{}}
	e := headroomEnv(t, 20, stub)
	r, err := e.launch("spc_a", "")
	if err != nil || r.Launch.AccountRef != "acc_1" {
		t.Fatalf("%v %+v", err, r)
	}
	stub.used["acc_1"] = 99
	e.report(r.ID, "spc_a", r.Generation, "running", nil)
	if n := e.count(`SELECT count(*) FROM account_switches`); n != 0 {
		t.Fatalf("%d switches", n)
	}
	got, err := e.svc.GetRun(e.ctx, r.ID)
	if err != nil || got.Launch.AccountRef != "acc_1" || got.Desired != "running" {
		t.Fatalf("run %v %+v", err, got)
	}
	// A new launch does move.
	r2, err := e.launch("spc_a", "")
	if err != nil || r2.Launch.AccountRef != "acc_2" {
		t.Fatalf("new launch: %v %+v", err, r2)
	}
	// The move is recorded as the gate's, not a usage limit's.
	if fo, _ := manifestOf(t, r2)["account_failover"].(map[string]any); fo["from"] != "acc_1" || fo["to"] != "acc_2" || fo["reason"] != "headroom" {
		t.Fatalf("account_failover %v", fo)
	}
}

// A switch's resume is gated too: the target of a switch away from acc_1 skips an account below the gate, and is none
// when no eligible account is left.
func TestHeadroomGateSwitchTarget(t *testing.T) {
	stub := &usageStub{used: map[string]float64{"acc_2": 95}}
	e := headroomEnv(t, 20, stub)
	target := func() string {
		var id string
		if err := e.s.W().TxNamed(e.ctx, "target", func(tx *store.Tx) error {
			var err error
			id, err = e.svc.switchTargetTx(e.ctx, tx, "spc_a", "claude-code", "implementor", "acc_1", "")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	if id := target(); id != "" {
		t.Fatalf("target %q with the only other account gated", id)
	}
	stub.used["acc_2"] = 10
	if id := target(); id != "acc_2" {
		t.Fatalf("target %q", id)
	}
}

// A window whose reset has passed no longer counts, and a ledger error leaves the account eligible.
func TestHeadroomGateStaleWindowAndLedgerError(t *testing.T) {
	e := headroomEnv(t, 20, &usageStub{used: map[string]float64{"acc_1": 99}, reset: map[string]time.Duration{"acc_1": -time.Minute}})
	if r, err := e.launch("spc_a", ""); err != nil || r.Launch.AccountRef != "acc_1" {
		t.Fatalf("stale window: %v %+v", err, r)
	}
	e.svc.opts.AccountHeadroom = func(context.Context, *db.Queries, string) (float64, time.Time, bool, error) {
		return 99, time.Time{}, true, context.DeadlineExceeded
	}
	if r, err := e.launch("spc_a", ""); err != nil || r.Launch.AccountRef != "acc_1" {
		t.Fatalf("ledger error: %v %+v", err, r)
	}
}

// With the real usage ledger wired (usage.AccountHeadroomFunc, as central does), an account whose most-used window leaves
// less than the gate's headroom is skipped, and a window nobody has refreshed for longer than usage.WindowMaxAge is not
// counted.
func TestHeadroomGateWithLedger(t *testing.T) {
	e := headroomEnv(t, 20, &usageStub{})
	e.svc.opts.AccountHeadroom = usage.AccountHeadroomFunc(e.svc.now)
	now := e.svc.now()
	e.exec(`INSERT INTO account_windows (account_id, kind, used_pct, resets_at, observed_at, run_id, updated_at) VALUES ('acc_1', 'five_hour', 95, ?, ?, '', ?)`,
		now.Add(2*time.Hour).UnixMicro(), now.UnixMicro(), now.UnixMicro())
	if r, err := e.launch("spc_a", ""); err != nil || r.Launch.AccountRef != "acc_2" {
		t.Fatalf("an account at 95%% was not skipped: %v %+v", err, r)
	}
	// Nobody has refreshed the window for too long: it no longer counts.
	e.exec(`UPDATE account_windows SET observed_at = ? WHERE account_id = 'acc_1'`, now.Add(-2*usage.WindowMaxAge).UnixMicro())
	if r, err := e.launch("spc_a", ""); err != nil || r.Launch.AccountRef != "acc_1" {
		t.Fatalf("a stale window counted: %v %+v", err, r)
	}
}
