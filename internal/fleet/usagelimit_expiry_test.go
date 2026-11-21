package fleet

import (
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store"
)

func (e *holdEnv) openIDs() map[string]bool {
	e.t.Helper()
	out := map[string]bool{}
	rows, err := e.s.R().ListOpenUsageLimitsByAccount(e.ctx, "acc_1")
	if err != nil {
		e.t.Fatal(err)
	}
	for _, r := range rows {
		out[r.ID] = true
	}
	return out
}

func (e *holdEnv) clearedBy(id string) string {
	e.t.Helper()
	u, err := e.s.R().GetUsageLimit(e.ctx, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return u.ClearedBy
}

// A hold without a reset always ends: the sweep clears it once its window (5 h, or 7 d for a weekly kind) passed since it
// was seen, however no run is left to report it over. A hold with a known reset is not the sweep's.
func TestUsageLimitUnknownResetExpires(t *testing.T) {
	e := newHoldEnv(t)
	e.hold("ulm_5h", "spc_a", "acc_1", "", proto.LimitFiveHour, nil, dur(time.Hour))
	e.hold("ulm_unk", "spc_a", "acc_1", "", proto.LimitUnknown, nil, dur(time.Hour))
	e.hold("ulm_7d", "spc_b", "acc_1", "", proto.LimitSevenDay, nil, dur(time.Hour))
	e.hold("ulm_known", "spc_b", "acc_1", "", proto.LimitFiveHour, dur(30*24*time.Hour), nil)

	next, err := e.svc.NextLimitWait(e.ctx)
	if err != nil || next.IsZero() || next.After(e.clk.Now().Add(5*time.Hour)) {
		t.Fatalf("next limit-wait round %v (%v), want within 5 h", next, err)
	}

	e.clk.Set(e.clk.Now().Add(4*time.Hour + 59*time.Minute))
	if _, err := e.svc.LimitWaitsDue(e.ctx); err != nil {
		t.Fatal(err)
	}
	if len(e.openIDs()) != 4 {
		t.Fatalf("holds inside their window were cleared: %v", e.openIDs())
	}

	e.clk.Set(e.clk.Now().Add(2 * time.Minute))
	if n, err := e.svc.LimitWaitsDue(e.ctx); err != nil || n != 2 {
		t.Fatalf("sweep: n=%d err=%v, want 2", n, err)
	}
	open := e.openIDs()
	if open["ulm_5h"] || open["ulm_unk"] || !open["ulm_7d"] || !open["ulm_known"] {
		t.Fatalf("open after 5 h: %v", open)
	}
	if by := e.clearedBy("ulm_5h"); by != ClearedByExpiry {
		t.Fatalf("cleared_by %q", by)
	}
	// The account still refuses (the weekly hold and the known reset hold stand); the cleared ones no longer count.
	e.clk.Set(e.clk.Now().Add(7 * 24 * time.Hour))
	if _, err := e.svc.LimitWaitsDue(e.ctx); err != nil {
		t.Fatal(err)
	}
	if open := e.openIDs(); open["ulm_7d"] || !open["ulm_known"] {
		t.Fatalf("open after 7 d: %v", open)
	}
}

func (e *holdEnv) windows(ws ...proto.UsageWindow) {
	e.t.Helper()
	if err := e.s.W().TxNamed(e.ctx, "test.windows", func(tx *store.Tx) error {
		return e.svc.ClearHoldsOnWindowsTx(e.ctx, tx, "acc_1", ws)
	}); err != nil {
		e.t.Fatal(err)
	}
}

// Fresh evidence ends a hold without a reset: a status-line snapshot of the account under 100 % observed after the hold
// was seen. A full window, a snapshot from before the hold, the wrong window for the kind, a per-model weekly kind and a
// hold with a known reset all stay.
func TestUsageLimitClearedByWindowSnapshot(t *testing.T) {
	e := newHoldEnv(t)
	e.hold("ulm_5h", "spc_a", "acc_1", "", proto.LimitFiveHour, nil, dur(time.Hour))
	e.hold("ulm_7d", "spc_b", "acc_1", "", proto.LimitSevenDay, nil, dur(time.Hour))
	e.hold("ulm_opus", "spc_a", "acc_1", "", proto.LimitSevenDayOpus, nil, dur(time.Hour))
	e.hold("ulm_known", "spc_b", "acc_1", "", proto.LimitFiveHour, dur(3*time.Hour), nil)
	e.clk.Set(e.clk.Now().Add(time.Hour))
	now := e.clk.Now()
	win := func(kind string, pct float64, at time.Time) proto.UsageWindow {
		return proto.UsageWindow{Kind: kind, UsedPct: pct, ObservedAt: at}
	}

	e.windows(win(proto.WindowFiveHour, 100, now), win(proto.WindowSevenDay, 100, now))
	e.windows(win(proto.WindowFiveHour, 10, now.Add(-2*time.Hour)), win(proto.WindowSevenDay, 10, now.Add(-2*time.Hour)))
	if len(e.openIDs()) != 4 {
		t.Fatalf("a full or stale snapshot cleared a hold: %v", e.openIDs())
	}

	e.windows(win(proto.WindowFiveHour, 40, now))
	open := e.openIDs()
	if open["ulm_5h"] || !open["ulm_7d"] || !open["ulm_opus"] || !open["ulm_known"] {
		t.Fatalf("after a five-hour snapshot under 100 %%: %v", open)
	}
	if by := e.clearedBy("ulm_5h"); by != ClearedByEvidence {
		t.Fatalf("cleared_by %q", by)
	}
	e.windows(win(proto.WindowSevenDay, 99.5, now))
	if open := e.openIDs(); open["ulm_7d"] || !open["ulm_opus"] || !open["ulm_known"] {
		t.Fatalf("after a weekly snapshot under 100 %%: %v", open)
	}
}
