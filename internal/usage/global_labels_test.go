package usage

import (
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// No slice boundary of the totals falls inside a bucket, whatever the span (not a multiple of 4 days, or ending
// mid-day), so a roll-up between the slices' read transactions can't count a day twice or drop its later hours.
func TestSliceBoundsAlignToBuckets(t *testing.T) {
	t.Parallel()
	day := (24 * time.Hour).Microseconds()
	for _, tc := range []struct {
		name     string
		from, to int64
		bucket   int64
	}{
		{"89 days", time.Date(2026, 8, 23, 0, 0, 0, 0, time.UTC).UnixMicro(), globalNow.UnixMicro(), day},
		{"90 days", time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC).UnixMicro(), globalNow.UnixMicro(), day},
		{"3 days", time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC).UnixMicro(), globalNow.UnixMicro(), day},
		{"1 day", time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC).UnixMicro(), globalNow.UnixMicro(), day},
		{"hours", globalNow.Add(-7 * 24 * time.Hour).UnixMicro(), globalNow.UnixMicro(), 1},
	} {
		b := sliceBounds(tc.from, tc.to, tc.bucket)
		if b[0] != tc.from || b[totalSlices] != tc.to {
			t.Fatalf("%s: ends %v", tc.name, b)
		}
		for i := 1; i < totalSlices; i++ {
			if b[i] < b[i-1] || b[i] > tc.to {
				t.Fatalf("%s: boundaries not ordered: %v", tc.name, b)
			}
			if tc.bucket > 1 && b[i]%tc.bucket != 0 {
				t.Fatalf("%s: boundary %d (%v) falls inside a day", tc.name, i, time.UnixMicro(b[i]).UTC())
			}
		}
	}
}

// The totals of a daily range that isn't a multiple of 4 days still equal the sum of its rows.
func TestGlobalSeriesTotalsOnAnUnevenDailyRange(t *testing.T) {
	t.Parallel()
	f := newGlobalFx(t, []string{"spc_a"}, nil, nil)
	var bs []db.UsageBucket
	from := time.Date(2026, 8, 23, 0, 0, 0, 0, time.UTC)
	for d := from; d.Before(globalNow); d = d.Add(24 * time.Hour) {
		bs = append(bs, db.UsageBucket{SpaceID: "spc_a", Role: "r", Harness: "claude-code", Model: "m", Granularity: GranDay, BucketStart: d.UnixMicro(), InputTokens: 1, Requests: 1})
	}
	f.add(t, bs)
	p := f.allPages(t, SeriesQuery{GroupBy: "space", Bucket: GranDay, From: from}, Filter{}, Scope{All: true}, 10)
	if p.Totals.InputTokens != int64(len(bs)) {
		t.Fatalf("totals %d, want %d", p.Totals.InputTokens, len(bs))
	}
}

// An account with ledger rows in space A that is bound only to space B gets no label for a reader of A; one bound to
// A through a role pool does (the account-windows rule).
func TestGlobalSeriesAccountLabelRule(t *testing.T) {
	t.Parallel()
	f := newGlobalFx(t, []string{"spc_a", "spc_b"}, []string{"acc_b", "acc_pool", "acc_direct"},
		map[string][]string{"spc_b": {"acc_b"}, "spc_a": {"acc_direct"}})
	if err := f.s.W().TxNamed(f.ctx, "seed.pool", func(tx *store.Tx) error {
		_, err := tx.SQL.ExecContext(f.ctx, `INSERT INTO space_role_accounts (space_id, role, account_id, priority, created_at, created_by) VALUES ('spc_a', 'r', 'acc_pool', 100, 0, 't')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	at := globalNow.Add(-2 * time.Hour).Truncate(time.Hour).UnixMicro()
	var bs []db.UsageBucket
	for _, acc := range []string{"acc_b", "acc_pool", "acc_direct"} {
		bs = append(bs, db.UsageBucket{SpaceID: "spc_a", AccountID: acc, Role: "r", Harness: "claude-code", Model: "m", Granularity: GranHour, BucketStart: at, InputTokens: 5, Requests: 1})
	}
	f.add(t, bs)
	q := SeriesQuery{GroupBy: "account", Bucket: GranHour}
	p := f.allPages(t, q, Filter{}, Scope{Spaces: []string{"spc_a"}}, 10)
	if len(p.Rows) != 3 {
		t.Fatalf("rows: %+v", p.Rows)
	}
	if _, ok := p.Labels["acc_b"]; ok {
		t.Fatalf("an account bound only to space B is named: %v", p.Labels)
	}
	if p.Labels["acc_pool"] != "name of acc_pool" || p.Labels["acc_direct"] != "name of acc_direct" {
		t.Fatalf("labels %v", p.Labels)
	}
	// A reader of both spaces does see acc_b's name.
	if all := f.allPages(t, q, Filter{}, Scope{Spaces: []string{"spc_a", "spc_b"}}, 10); all.Labels["acc_b"] != "name of acc_b" {
		t.Fatalf("labels %v", all.Labels)
	}
}

// An account bound only through a role pool in space B, with ledger rows in space A, gets no label for a reader of A:
// the role-pool branch of the label rule filters on the readable spaces, as the direct branch does.
func TestGlobalSeriesAccountLabelRulePoolInOtherSpace(t *testing.T) {
	t.Parallel()
	f := newGlobalFx(t, []string{"spc_a", "spc_b"}, []string{"acc_poolb"}, nil)
	if err := f.s.W().TxNamed(f.ctx, "seed.pool", func(tx *store.Tx) error {
		_, err := tx.SQL.ExecContext(f.ctx, `INSERT INTO space_role_accounts (space_id, role, account_id, priority, created_at, created_by) VALUES ('spc_b', 'r', 'acc_poolb', 100, 0, 't')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	at := globalNow.Add(-2 * time.Hour).Truncate(time.Hour).UnixMicro()
	f.add(t, []db.UsageBucket{{SpaceID: "spc_a", AccountID: "acc_poolb", Role: "r", Harness: "claude-code", Model: "m", Granularity: GranHour, BucketStart: at, InputTokens: 5, Requests: 1}})
	q := SeriesQuery{GroupBy: "account", Bucket: GranHour}
	p := f.allPages(t, q, Filter{}, Scope{Spaces: []string{"spc_a"}}, 10)
	if len(p.Rows) != 1 {
		t.Fatalf("rows: %+v", p.Rows)
	}
	if _, ok := p.Labels["acc_poolb"]; ok {
		t.Fatalf("an account bound only through a role pool in space B is named to a reader of A: %v", p.Labels)
	}
	// A reader of both spaces does see the name.
	if all := f.allPages(t, q, Filter{}, Scope{Spaces: []string{"spc_a", "spc_b"}}, 10); all.Labels["acc_poolb"] != "name of acc_poolb" {
		t.Fatalf("labels %v", all.Labels)
	}
}
