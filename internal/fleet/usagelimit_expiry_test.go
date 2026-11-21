package fleet

import (
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/proto"
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
	if n, err := e.svc.LimitWaitsDue(e.ctx); err != nil || n != 1 {
		t.Fatalf("sweep: n=%d err=%v, want 1", n, err)
	}
	open := e.openIDs()
	if open["ulm_5h"] || !open["ulm_unk"] || !open["ulm_7d"] || !open["ulm_known"] {
		t.Fatalf("open after 5 h: %v", open)
	}
	// An unknown kind lasts the keep-alive (6 h): a run waiting out the limit is not freed under it.
	e.clk.Set(e.clk.Now().Add(time.Hour))
	if n, err := e.svc.LimitWaitsDue(e.ctx); err != nil || n != 1 {
		t.Fatalf("sweep: n=%d err=%v, want 1", n, err)
	}
	if open := e.openIDs(); open["ulm_unk"] || !open["ulm_7d"] {
		t.Fatalf("open after 6 h: %v", open)
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
