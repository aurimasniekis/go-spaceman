package usage

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/store/db"
	"github.com/aurimasniekis/go-spaceman/internal/store/storetest"
)

// benchWorld is the plan's benchmark ledger (UI-T1 criterion 3): 200k bucket rows over 50 spaces, 10 accounts and 5
// models, hourly over the last 31 days and daily over the last 90.
func benchWorld(b testing.TB) *globalFx {
	b.Helper()
	var spaces, accounts []string
	for i := range 50 {
		spaces = append(spaces, fmt.Sprintf("spc_%02d", i))
	}
	for i := range 10 {
		accounts = append(accounts, fmt.Sprintf("acc_%02d", i))
	}
	models := []string{"m1", "m2", "m3", "m4", "m5"}
	binds := map[string][]string{}
	for i, s := range spaces {
		binds[s] = []string{accounts[i%10], accounts[(i+3)%10]}
	}
	f := newGlobalFx(b, spaces, accounts, binds, storetest.WithReaders(0)) // the production reader pool
	r := rand.New(rand.NewPCG(42, 42))
	type key struct {
		space, account, model, gran string
		start                       int64
	}
	seen := map[key]bool{}
	var bs []db.UsageBucket
	hour := globalNow.Truncate(time.Hour)
	day := globalNow.Truncate(24 * time.Hour)
	for len(bs) < 200_000 {
		k := key{space: spaces[r.IntN(50)], account: accounts[r.IntN(10)], model: models[r.IntN(5)]}
		if len(bs) < 170_000 {
			k.gran, k.start = GranHour, hour.Add(-time.Duration(r.IntN(31*24))*time.Hour).UnixMicro()
		} else {
			k.gran, k.start = GranDay, day.Add(-time.Duration(r.IntN(90))*24*time.Hour).UnixMicro()
		}
		if seen[k] {
			continue
		}
		seen[k] = true
		bs = append(bs, db.UsageBucket{SpaceID: k.space, AccountID: k.account, Role: "implementor", Harness: "claude-code", Model: k.model, Granularity: k.gran,
			BucketStart: k.start, InputTokens: r.Int64N(1000), OutputTokens: r.Int64N(500), CacheReadTokens: r.Int64N(5000), Requests: 1, CostUsd: 0.25})
	}
	f.add(b, bs)
	for i := range 2000 { // runs for the not-counted query
		at := globalNow.Add(-time.Duration(r.IntN(90*24)) * time.Hour).UnixMicro()
		h := "claude-code"
		if i%4 == 0 {
			h = "codex"
		}
		f.run(b, fmt.Sprintf("run_%05d", i), spaces[i%50], "implementor", h, accounts[i%10], "", at, at+int64(time.Hour/time.Microsecond))
	}
	return f
}

func benchSeries(b *testing.B, q SeriesQuery, sc Scope) {
	f := benchWorld(b)
	b.ResetTimer()
	for b.Loop() {
		if _, err := f.svc.GlobalSeries(f.ctx, q, Filter{}, sc, nil, 200); err != nil {
			b.Fatal(err)
		}
	}
}

// One request (first page of 200, totals, labels, not-counted runs): 30 days of hours by space, every space.
func BenchmarkGlobalSeries30DaysBySpace(b *testing.B) {
	benchSeries(b, SeriesQuery{GroupBy: "space", From: globalNow.Add(-30 * 24 * time.Hour), To: globalNow}, Scope{All: true})
}

// The same for a reader of 5 spaces (the IN list).
func BenchmarkGlobalSeries30DaysBySpaceFiveSpaces(b *testing.B) {
	benchSeries(b, SeriesQuery{GroupBy: "space", From: globalNow.Add(-30 * 24 * time.Hour), To: globalNow},
		Scope{Spaces: []string{"spc_01", "spc_02", "spc_03", "spc_04", "spc_05"}})
}

// 90 days of days by model, every space.
func BenchmarkGlobalSeries90DaysByModel(b *testing.B) {
	benchSeries(b, SeriesQuery{GroupBy: "model", Bucket: GranDay, From: globalNow.Add(-90 * 24 * time.Hour), To: globalNow}, Scope{All: true})
}

// The 90-day daily query reads the ledger through the time index, never with a full table scan. Run with -v to print
// the plan (recorded in the pull request).
func TestGlobalSeriesQueryPlan(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var query string
	f := newGlobalFx(t, []string{"spc_a"}, nil, nil, storetest.WithReadObserver(func(q string) {
		if strings.Contains(q, "-- name: SumUsageSeries ") {
			mu.Lock()
			query = q
			mu.Unlock()
		}
	}))
	if _, err := f.svc.GlobalSeries(f.ctx, SeriesQuery{GroupBy: "space"}, Filter{}, Scope{All: true}, nil, 10); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if query == "" {
		t.Fatal("the series query was not observed")
	}
	from := globalNow.Add(-90 * 24 * time.Hour).Truncate(24 * time.Hour).UnixMicro()
	for name, args := range map[string][]any{
		"every space": {int64(86_400_000_000), "space", `["hour","day"]`, from, globalNow.UnixMicro(), nil, nil, nil, nil, nil, int64(-1), "", int64(201)},
		"some spaces": {int64(86_400_000_000), "space", `["hour","day"]`, from, globalNow.UnixMicro(), `["spc_a","spc_b"]`, nil, `["m1"]`, nil, nil, int64(-1), "", int64(201)},
	} {
		rows, err := f.s.ReadDB().QueryContext(f.ctx, "EXPLAIN QUERY PLAN "+query, args...)
		if err != nil {
			t.Fatal(err)
		}
		var plan []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			plan = append(plan, detail)
		}
		_ = rows.Close()
		t.Logf("%s:\n  %s", name, strings.Join(plan, "\n  "))
		for _, d := range plan {
			if strings.HasPrefix(d, "SCAN b") && !strings.Contains(d, "INDEX") {
				t.Fatalf("%s: full scan of usage_buckets: %s", name, d)
			}
		}
		if !strings.Contains(strings.Join(plan, "\n"), "ix_usage_buckets_time") {
			t.Fatalf("%s: the time index is not used:\n%s", name, strings.Join(plan, "\n"))
		}
	}
}
