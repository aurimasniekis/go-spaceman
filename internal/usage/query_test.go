package usage

import (
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/store"
)

func TestSpaceSeriesGroupsAndTotals(t *testing.T) {
	t.Parallel()
	f := newLedger(t)
	f.report(lrA, false, 0.5, mu("m", 100, 10, 0, 50, 2)) // acc_1 / implementor
	f.report(lrD, false, 0.25, mu("n", 40, 4, 0, 0, 1))   // no account
	f.clk.Advance(time.Hour)
	f.report(lrA, false, 0.75, mu("m", 160, 16, 0, 50, 3))

	s, err := f.svc.SpaceSeries(f.ctx, "spc_a", SeriesQuery{GroupBy: "account"})
	if err != nil {
		t.Fatal(err)
	}
	if s.Bucket != GranHour || s.CostNote != CostBasis || len(s.Rows) != 3 {
		t.Fatalf("series %+v", s)
	}
	// 100+10+50 + 40+4 + 60+6 = 270 tokens; cost is the estimate's growth, 0.75 + 0.25.
	if s.Totals.TotalTokens != 270 || s.Totals.InputTokens != 200 || s.Totals.Requests != 4 || s.Totals.TotalCostUSD < 0.99 || s.Totals.TotalCostUSD > 1.01 {
		t.Fatalf("totals %+v", s.Totals)
	}
	if s.Rows[0].Group != "" || s.Rows[1].Group != "acc_1" || s.Rows[2].Group != "acc_1" || !s.Rows[2].BucketStart.After(*s.Rows[1].BucketStart) {
		t.Fatalf("rows %+v", s.Rows)
	}
	// Daily buckets fold the hourly rows into one UTC day.
	d, err := f.svc.SpaceSeries(f.ctx, "spc_a", SeriesQuery{Bucket: GranDay, GroupBy: "role"})
	if err != nil || len(d.Rows) != 1 || d.Rows[0].Group != "implementor" || d.Rows[0].TotalTokens != 270 {
		t.Fatalf("day %+v %v", d, err)
	}
	// Another space has nothing; the range bounds the rows.
	if o, err := f.svc.SpaceSeries(f.ctx, "spc_other", SeriesQuery{GroupBy: "role"}); err != nil || len(o.Rows) != 0 {
		t.Fatalf("other %+v %v", o, err)
	}
	past, err := f.svc.SpaceSeries(f.ctx, "spc_a", SeriesQuery{GroupBy: "role", From: f.clk.Now().Add(-48 * time.Hour), To: f.clk.Now().Add(-24 * time.Hour)})
	if err != nil || len(past.Rows) != 0 {
		t.Fatalf("past %+v %v", past, err)
	}
}

func TestSeriesQueryValidation(t *testing.T) {
	t.Parallel()
	f := newLedger(t)
	now := f.clk.Now()
	for name, q := range map[string]SeriesQuery{
		"bad group":    {GroupBy: "space"},
		"bad bucket":   {Bucket: "week", GroupBy: "role"},
		"inverted":     {GroupBy: "role", From: now, To: now.Add(-time.Hour)},
		"too long":     {GroupBy: "role", From: now.Add(-60 * 24 * time.Hour), To: now},
		"day too long": {Bucket: GranDay, GroupBy: "role", From: now.Add(-900 * 24 * time.Hour), To: now},
	} {
		if _, err := f.svc.SpaceSeries(f.ctx, "spc_a", q); apperr.CodeOf(err) != apperr.CodeInvalid {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// An account's series counts only the spaces the caller may read; totals never include the others.
func TestAccountSeriesFiltersSpaces(t *testing.T) {
	t.Parallel()
	f := newLedger(t)
	f.report(lrA, false, 0.5, mu("m", 100, 10, 0, 0, 2))
	all, err := f.svc.AccountSeries(f.ctx, "acc_1", SeriesQuery{GroupBy: "space"}, func(string) bool { return true })
	if err != nil || len(all.Rows) != 1 || all.Rows[0].Group != "spc_a" || all.Totals.TotalTokens != 110 {
		t.Fatalf("all %+v %v", all, err)
	}
	none, err := f.svc.AccountSeries(f.ctx, "acc_1", SeriesQuery{GroupBy: "space"}, func(string) bool { return false })
	if err != nil || len(none.Rows) != 0 || none.Totals.TotalTokens != 0 || none.Totals.TotalCostUSD != 0 {
		t.Fatalf("none %+v %v", none, err)
	}
	if _, err := f.svc.AccountSeries(f.ctx, "acc_missing", SeriesQuery{GroupBy: "space"}, func(string) bool { return true }); apperr.CodeOf(err) != apperr.CodeNotFound {
		t.Fatalf("missing: %v", err)
	}
}

func TestWindowsViews(t *testing.T) {
	t.Parallel()
	f := newLedger(t)
	now := f.clk.Now()
	if err := f.s.W().TxNamed(f.ctx, "seed", func(tx *store.Tx) error {
		if _, err := tx.SQL.ExecContext(f.ctx, `INSERT INTO space_accounts (space_id, account_id, harness, is_default, priority, created_at, created_by) VALUES ('spc_a', 'acc_1', 'claude-code', 1, 100, 0, 't'), ('spc_a', 'acc_2', 'claude-code', 0, 100, 1, 't')`); err != nil {
			return err
		}
		_, err := tx.SQL.ExecContext(f.ctx, `INSERT INTO account_windows (account_id, kind, used_pct, resets_at, observed_at, run_id, updated_at) VALUES ('acc_1', 'five_hour', 42.5, ?, ?, '', 0)`,
			now.Add(time.Hour).UnixMicro(), now.UnixMicro())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	ws, err := f.svc.SpaceWindows(f.ctx, "spc_a")
	if err != nil || len(ws) != 2 || ws[0].AccountID != "acc_1" || ws[0].Name != "a1" || len(ws[0].Windows) != 1 || ws[0].Windows[0].UsedPct != 42.5 ||
		ws[0].Windows[0].ResetsAt == nil || ws[1].Name != "a2" || len(ws[1].Windows) != 0 {
		t.Fatalf("space windows %+v %v", ws, err)
	}
	w, err := f.svc.WindowsOf(f.ctx, "acc_1")
	if err != nil || w.Name != "a1" || len(w.Windows) != 1 || w.Windows[0].Kind != "five_hour" {
		t.Fatalf("account windows %+v %v", w, err)
	}
	if sp, err := f.svc.AccountSpaces(f.ctx, "acc_1"); err != nil || len(sp) != 1 || sp[0] != "spc_a" {
		t.Fatalf("spaces %v %v", sp, err)
	}
	if _, err := f.svc.WindowsOf(f.ctx, "acc_x"); apperr.CodeOf(err) != apperr.CodeNotFound {
		t.Fatal(err)
	}
}
