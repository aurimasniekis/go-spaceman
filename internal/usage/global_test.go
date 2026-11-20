package usage

import (
	"context"
	"fmt"
	"math/rand/v2"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/keyring/keyringtest"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
	"github.com/aurimasniekis/go-spaceman/internal/store/storetest"
)

// globalNow is the fixed clock of the cross-space tests (not on an hour or a day boundary).
var globalNow = time.Date(2026, 11, 20, 9, 35, 0, 0, time.UTC)

type globalFx struct {
	svc *Service
	s   *store.Store
	ctx context.Context
}

// newGlobalFx opens a store with the given spaces (named "name of <id>"), accounts (named "name of <id>") and bindings
// (space -> accounts).
func newGlobalFx(t testing.TB, spaces, accounts []string, binds map[string][]string, opts ...storetest.Option) *globalFx {
	t.Helper()
	// Seeding writes thousands of ledger rows in one transaction, which the store's lock-time guard would flag; no
	// transaction under test runs here (GlobalSeries only reads).
	s := storetest.Open(t, append([]storetest.Option{storetest.WithClock(clock.NewFake(globalNow)), func(o *store.Options) { o.TxObserver = nil }}, opts...)...)
	kr := keyringtest.Load(t, s)
	ctx := context.Background()
	if err := s.W().TxNamed(ctx, "seed", func(tx *store.Tx) error {
		if _, err := tx.SQL.ExecContext(ctx, `INSERT INTO nodes (id, name, state, created_at, created_by, updated_at) VALUES ('nod_1', 'n1', 'active', 0, 't', 0)`); err != nil {
			return err
		}
		for i, sp := range spaces {
			if _, err := tx.SQL.ExecContext(ctx, `INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at)
				VALUES (?, ?, ?, 'nod_1', 'active', ?, 0, 't', 0)`, sp, fmt.Sprintf("s%d", i), "name of "+sp, sp); err != nil {
				return err
			}
		}
		for _, a := range accounts {
			if _, err := tx.SQL.ExecContext(ctx, `INSERT INTO accounts (id, name, kind, secret_enc, secret_rotated_at, created_at, created_by, updated_at)
				VALUES (?, ?, 'claude_oauth_token', x'00', 0, 0, 't', 0)`, a, "name of "+a); err != nil {
				return err
			}
		}
		for sp, as := range binds {
			for _, a := range as {
				if _, err := tx.SQL.ExecContext(ctx, `INSERT INTO space_accounts (space_id, account_id, harness, is_default, priority, created_at, created_by)
					VALUES (?, ?, 'claude-code', 0, 100, 0, 't')`, sp, a); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return &globalFx{svc: New(s, audit.NewWriter(kr), changefeed.New(nil), t.TempDir(), nil, nil), s: s, ctx: ctx}
}

func (f *globalFx) add(t testing.TB, bs []db.UsageBucket) {
	t.Helper()
	if err := f.s.W().TxNamed(f.ctx, "seed.buckets", func(tx *store.Tx) error {
		st, err := tx.SQL.PrepareContext(f.ctx, `INSERT INTO usage_buckets (space_id, account_id, role, harness, model, granularity, bucket_start, input_tokens,
			output_tokens, cache_write_tokens, cache_read_tokens, reasoning_tokens, requests, cost_usd) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (space_id, account_id, role, harness, model, granularity, bucket_start) DO UPDATE SET
			input_tokens = usage_buckets.input_tokens + excluded.input_tokens, output_tokens = usage_buckets.output_tokens + excluded.output_tokens,
			cache_write_tokens = usage_buckets.cache_write_tokens + excluded.cache_write_tokens,
			cache_read_tokens = usage_buckets.cache_read_tokens + excluded.cache_read_tokens,
			reasoning_tokens = usage_buckets.reasoning_tokens + excluded.reasoning_tokens, requests = usage_buckets.requests + excluded.requests,
			cost_usd = usage_buckets.cost_usd + excluded.cost_usd`)
		if err != nil {
			return err
		}
		defer func() { _ = st.Close() }()
		for _, b := range bs {
			if _, err := st.ExecContext(f.ctx, b.SpaceID, b.AccountID, b.Role, b.Harness, b.Model, b.Granularity, b.BucketStart, b.InputTokens, b.OutputTokens,
				b.CacheWriteTokens, b.CacheReadTokens, b.ReasoningTokens, b.Requests, b.CostUsd); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// run inserts a run of a harness in a space; source is its run_usage source ("" for none).
func (f *globalFx) run(t testing.TB, id, space, role, harness, account, source string, created, ended int64) {
	t.Helper()
	if err := f.s.W().TxNamed(f.ctx, "seed.run", func(tx *store.Tx) error {
		if _, err := tx.SQL.ExecContext(f.ctx, `INSERT INTO runs (id, space_id, node_id, generation, kind, role, harness, instance_id, session_id, desired, observed,
			launch_json, manifest_json, created_at, created_by, updated_at, ended_at) VALUES (?, ?, 'nod_1', 1, 'worker', ?, ?, 'ins_1', '', 'stopped', 'exited',
			?, '{}', ?, 't', ?, ?)`, id, space, role, harness, fmt.Sprintf(`{"account_ref":%q}`, account), created, created, ended); err != nil {
			return err
		}
		if source == "" {
			return nil
		}
		_, err := tx.SQL.ExecContext(f.ctx, `INSERT INTO run_usage (run_id, space_id, source, updated_at, account_id, role, harness) VALUES (?, ?, ?, 0, ?, ?, ?)`,
			id, space, source, account, role, harness)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// allPages reads every page of a cross-space series and joins them.
func (f *globalFx) allPages(t testing.TB, q SeriesQuery, flt Filter, sc Scope, limit int) *GlobalPage {
	t.Helper()
	var out *GlobalPage
	var after *After
	for i := 0; ; i++ {
		p, err := f.svc.GlobalSeries(f.ctx, q, flt, sc, after, limit)
		if err != nil {
			t.Fatal(err)
		}
		if out == nil {
			out = p
		} else {
			if !reflect.DeepEqual(p.Totals, out.Totals) || !reflect.DeepEqual(p.NotCounted, out.NotCounted) {
				t.Fatalf("page %d changed the totals or not-counted runs: %+v / %+v", i, p.Totals, out.Totals)
			}
			out.Rows = append(out.Rows, p.Rows...)
			for k, v := range p.Labels {
				out.Labels[k] = v
			}
		}
		if len(p.Rows) > limit || (p.More && len(p.Rows) != limit) {
			t.Fatalf("page of %d rows with limit %d (more %v)", len(p.Rows), limit, p.More)
		}
		if !p.More {
			return out
		}
		last := p.Rows[len(p.Rows)-1]
		after = &After{Bucket: *last.BucketStart, Group: last.Group}
		if i > 100000 {
			t.Fatal("paging does not end")
		}
	}
}

// randomBuckets returns n random ledger rows: hourly ones in the last 31 days, daily ones in the last 90 (costs in
// eighths, so float sums do not depend on the order).
func randomBuckets(r *rand.Rand, n int, spaces, accounts, models, roles, harnesses []string) []db.UsageBucket {
	pick := func(l []string) string { return l[r.IntN(len(l))] }
	hour := globalNow.Truncate(time.Hour)
	day := globalNow.Truncate(24 * time.Hour)
	out := make([]db.UsageBucket, 0, n)
	for range n {
		b := db.UsageBucket{SpaceID: pick(spaces), AccountID: pick(accounts), Role: pick(roles), Harness: pick(harnesses), Model: pick(models),
			InputTokens: r.Int64N(1000), OutputTokens: r.Int64N(500), CacheWriteTokens: r.Int64N(50), CacheReadTokens: r.Int64N(5000),
			ReasoningTokens: r.Int64N(20), Requests: r.Int64N(5), CostUsd: float64(r.IntN(64)) / 8}
		if r.IntN(3) == 0 {
			b.Granularity, b.BucketStart = GranDay, day.Add(-time.Duration(r.IntN(90))*24*time.Hour).UnixMicro()
		} else {
			b.Granularity, b.BucketStart = GranHour, hour.Add(-time.Duration(r.IntN(31*24))*time.Hour).UnixMicro()
		}
		out = append(out, b)
	}
	return out
}

func randomQuery(r *rand.Rand, groups []string) SeriesQuery {
	q := SeriesQuery{GroupBy: groups[r.IntN(len(groups))]}
	if r.IntN(2) == 0 {
		q.Bucket = GranDay
		span := time.Duration(1+r.IntN(95)) * 24 * time.Hour
		q.To = globalNow.Add(-time.Duration(r.IntN(48)) * time.Hour)
		q.From = q.To.Add(-span)
	} else {
		q.Bucket = GranHour
		span := time.Duration(1+r.IntN(31*24)) * time.Hour
		q.To = globalNow.Add(-time.Duration(r.IntN(48)) * time.Hour).Add(time.Duration(r.IntN(60)) * time.Minute)
		q.From = q.To.Add(-span)
	}
	return q
}

func sameSeries(t *testing.T, name string, got *GlobalPage, want *Series) {
	t.Helper()
	if got.Bucket != want.Bucket || got.GroupBy != want.GroupBy || !got.From.Equal(want.From) || !got.To.Equal(want.To) || got.CostNote != want.CostNote {
		t.Fatalf("%s: header %+v, want %+v", name, got.Series, *want)
	}
	if !reflect.DeepEqual(got.Totals, want.Totals) {
		t.Fatalf("%s: totals %+v, want %+v", name, got.Totals, want.Totals)
	}
	if len(got.Rows) != len(want.Rows) {
		t.Fatalf("%s: %d rows, want %d", name, len(got.Rows), len(want.Rows))
	}
	for i := range got.Rows {
		g, w := got.Rows[i], want.Rows[i]
		if !g.BucketStart.Equal(*w.BucketStart) || g.Group != w.Group {
			t.Fatalf("%s: row %d at %v/%q, want %v/%q", name, i, g.BucketStart, g.Group, w.BucketStart, w.Group)
		}
		g.BucketStart, w.BucketStart = nil, nil
		if !reflect.DeepEqual(g, w) {
			t.Fatalf("%s: row %d %+v, want %+v", name, i, g, w)
		}
	}
}

// The SQL aggregation equals the Go fold of SpaceSeries and AccountSeries over random hour and day buckets, for every
// grouping both offer, with pages of random size.
func TestGlobalSeriesEqualsTheFold(t *testing.T) {
	t.Parallel()
	spaces := []string{"spc_a", "spc_b", "spc_c"}
	accounts := []string{"", "acc_1", "acc_2", "acc_3"}
	models := []string{"m1", "m2", UnknownModel}
	roles := []string{"implementor", "reviewer"}
	harnesses := []string{"claude-code", "codex"}
	for seed := range uint64(12) {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			t.Parallel()
			r := rand.New(rand.NewPCG(seed, 0x5eed))
			f := newGlobalFx(t, spaces, accounts[1:], nil)
			f.add(t, randomBuckets(r, 400+r.IntN(800), spaces, accounts, models, roles, harnesses))
			for i := range 12 {
				// One space, as SpaceSeries sees it.
				sp := spaces[r.IntN(len(spaces))]
				q := randomQuery(r, SpaceGroups)
				want, err := f.svc.SpaceSeries(f.ctx, sp, q)
				if err != nil {
					t.Fatal(err)
				}
				got := f.allPages(t, q, Filter{Spaces: []string{sp}}, Scope{All: true}, 1+r.IntN(40))
				sameSeries(t, fmt.Sprintf("space %s %d %+v", sp, i, q), got, want)
				// The same space through a scope that holds only it.
				got = f.allPages(t, q, Filter{}, Scope{Spaces: []string{sp}}, 1+r.IntN(40))
				sameSeries(t, fmt.Sprintf("scoped space %s %d", sp, i), got, want)

				// One account over a random set of readable spaces, as AccountSeries sees it.
				acc := accounts[1+r.IntN(len(accounts)-1)]
				var readable []string
				for _, s := range spaces {
					if r.IntN(2) == 0 {
						readable = append(readable, s)
					}
				}
				aq := randomQuery(r, AccountGroups)
				wantA, err := f.svc.AccountSeries(f.ctx, acc, aq, func(s string) bool { return slices.Contains(readable, s) })
				if err != nil {
					t.Fatal(err)
				}
				gotA := f.allPages(t, aq, Filter{Accounts: []string{acc}}, Scope{Spaces: readable}, 1+r.IntN(40))
				sameSeries(t, fmt.Sprintf("account %s %v %d", acc, readable, i), gotA, wantA)
			}
		})
	}
}

// leakWorld is two spaces whose usage, accounts, models, roles and runs can be told apart. A reader of spc_a only must
// never see anything of spc_b's.
func leakWorld(t *testing.T) (*globalFx, []db.UsageBucket) {
	t.Helper()
	f := newGlobalFx(t, []string{"spc_a", "spc_b"}, []string{"acc_a", "acc_b", "acc_s"}, map[string][]string{"spc_a": {"acc_a", "acc_s"}, "spc_b": {"acc_b", "acc_s"}})
	r := rand.New(rand.NewPCG(7, 7))
	var bs []db.UsageBucket
	bs = append(bs, randomBuckets(r, 300, []string{"spc_a"}, []string{"acc_a", "acc_s", ""}, []string{"m-shared", "m-a"}, []string{"shared", "role-a"},
		[]string{"claude-code"})...)
	for _, b := range randomBuckets(r, 300, []string{"spc_b"}, []string{"acc_b", "acc_s", ""}, []string{"m-shared", "m-b"}, []string{"shared", "role-b"},
		[]string{"claude-code", "codex"}) {
		b.InputTokens += 1_000_000 // spc_b's rows are unmistakable in any total
		bs = append(bs, b)
	}
	f.add(t, bs)
	at := globalNow.Add(-2 * time.Hour).UnixMicro()
	f.run(t, "run_a_codex", "spc_a", "shared", "codex", "acc_s", "unavailable", at, at+1)
	f.run(t, "run_b_codex", "spc_b", "shared", "codex", "acc_s", "harbor-pilot", at, at+1)
	f.run(t, "run_b_codex2", "spc_b", "role-b", "codex", "acc_b", "", at, at+1)
	f.run(t, "run_b_claude", "spc_b", "shared", "claude-code", "acc_s", "unavailable", at, at+1)
	return f, bs
}

// inRange is the rows a series query reads (SpaceSeries's and AccountSeries's queries select them in SQL).
func inRange(b db.UsageBucket, q SeriesQuery) bool {
	return slices.Contains(granularities(q.Bucket), b.Granularity) && b.BucketStart >= q.From.UnixMicro() && b.BucketStart < q.To.UnixMicro()
}

// matches is the filter as the Go oracle applies it.
func (flt Filter) matches(b db.UsageBucket) bool {
	in := func(l []string, v string) bool { return len(l) == 0 || slices.Contains(l, v) }
	return in(flt.Spaces, b.SpaceID) && in(flt.Accounts, b.AccountID) && in(flt.Models, b.Model) && in(flt.Roles, b.Role) && in(flt.Harnesses, b.Harness)
}

// A reader of space A gets no row, total, label or not-counted run of space B, for every group_by and every filter,
// including filters that name only B's values.
func TestGlobalSeriesNeverLeaksAnotherSpace(t *testing.T) {
	t.Parallel()
	f, bs := leakWorld(t)
	filters := map[string]Filter{
		"none":            {},
		"space B":         {Spaces: []string{"spc_b"}},
		"space A and B":   {Spaces: []string{"spc_a", "spc_b"}},
		"account B":       {Accounts: []string{"acc_b"}},
		"shared account":  {Accounts: []string{"acc_s"}},
		"no account":      {Accounts: []string{""}},
		"model B":         {Models: []string{"m-b"}},
		"shared model":    {Models: []string{"m-shared"}},
		"role B":          {Roles: []string{"role-b"}},
		"shared role":     {Roles: []string{"shared"}},
		"harness codex":   {Harnesses: []string{"codex"}},
		"harness claude":  {Harnesses: []string{"claude-code"}},
		"every dimension": {Spaces: []string{"spc_b", "spc_a"}, Accounts: []string{"acc_s", "acc_b"}, Models: []string{"m-shared", "m-b"}, Roles: []string{"shared", "role-b"}},
	}
	scope := Scope{Spaces: []string{"spc_a"}}
	for _, group := range GlobalGroups {
		for fname, flt := range filters {
			for _, bucket := range []string{GranHour, GranDay} {
				name := group + "/" + fname + "/" + bucket
				q := SeriesQuery{GroupBy: group, Bucket: bucket, From: globalNow.Add(-31 * 24 * time.Hour)}
				if bucket == GranDay {
					q.From = globalNow.Add(-90 * 24 * time.Hour)
				}
				got := f.allPages(t, q, flt, scope, 13)
				nq, _ := q.normalize(globalNow, GlobalGroups)
				want := fold(bs, nq, func(b db.UsageBucket) bool { return b.SpaceID == "spc_a" && flt.matches(b) && inRange(b, nq) })
				if got.Totals.InputTokens >= 1_000_000 {
					t.Fatalf("%s: totals hold space B: %+v", name, got.Totals)
				}
				for _, r := range got.Rows {
					if r.InputTokens >= 1_000_000 || slices.Contains([]string{"spc_b", "acc_b", "m-b", "role-b", "codex"}, r.Group) {
						t.Fatalf("%s: row of space B: %+v", name, r)
					}
				}
				sameSeries(t, name, got, want)
				for k, v := range got.Labels {
					if k == "spc_b" || k == "acc_b" || strings.Contains(v, "spc_b") || strings.Contains(v, "acc_b") {
						t.Fatalf("%s: label of space B: %s=%s", name, k, v)
					}
				}
				switch group {
				case "space":
					if len(want.Rows) > 0 && got.Labels["spc_a"] != "name of spc_a" {
						t.Fatalf("%s: labels %v", name, got.Labels)
					}
				case "account":
					for _, r := range got.Rows {
						if r.Group != "" && got.Labels[r.Group] != "name of "+r.Group {
							t.Fatalf("%s: no label for %s: %v", name, r.Group, got.Labels)
						}
					}
				default:
					if len(got.Labels) != 0 {
						t.Fatalf("%s: labels for %s: %v", name, group, got.Labels)
					}
				}
				// Space A's only unledgered run is a Codex run of role "shared" on the shared account.
				wantNC := []NotCounted{}
				if (len(flt.Spaces) == 0 || slices.Contains(flt.Spaces, "spc_a")) && (len(flt.Accounts) == 0 || slices.Contains(flt.Accounts, "acc_s")) &&
					(len(flt.Roles) == 0 || slices.Contains(flt.Roles, "shared")) && (len(flt.Harnesses) == 0 || slices.Contains(flt.Harnesses, "codex")) {
					wantNC = []NotCounted{{Harness: "codex", Runs: 1}}
				}
				if !reflect.DeepEqual(got.NotCounted, wantNC) {
					t.Fatalf("%s: not counted %+v, want %+v", name, got.NotCounted, wantNC)
				}
			}
		}
	}
	// An empty scope reads nothing, whatever the filter names.
	for fname, flt := range filters {
		got, err := f.svc.GlobalSeries(f.ctx, SeriesQuery{GroupBy: "space"}, flt, Scope{}, nil, 50)
		if err != nil || len(got.Rows) != 0 || got.Totals != (Row{}) || len(got.Labels) != 0 || len(got.NotCounted) != 0 {
			t.Fatalf("empty scope, %s: %+v %v", fname, got, err)
		}
	}
}

// A reader of every space sees both spaces, Codex and Harbor Pilot runs, and Claude Code runs with another source.
func TestGlobalSeriesEveryReadableSpace(t *testing.T) {
	t.Parallel()
	f, bs := leakWorld(t)
	q := SeriesQuery{GroupBy: "space", Bucket: GranDay, From: globalNow.Add(-90 * 24 * time.Hour)}
	got := f.allPages(t, q, Filter{}, Scope{All: true}, 50)
	nq, _ := q.normalize(globalNow, GlobalGroups)
	sameSeries(t, "all", got, fold(bs, nq, func(b db.UsageBucket) bool { return inRange(b, nq) }))
	if got.Labels["spc_a"] != "name of spc_a" || got.Labels["spc_b"] != "name of spc_b" {
		t.Fatalf("labels %v", got.Labels)
	}
	want := []NotCounted{{Harness: "claude-code", Runs: 1}, {Harness: "codex", Runs: 3, HarborPilotRuns: 1}}
	if !reflect.DeepEqual(got.NotCounted, want) {
		t.Fatalf("not counted %+v", got.NotCounted)
	}
	// An account bound to no readable space is not named, though its usage counts: acc_a (bound to spc_a only) once ran
	// in spc_b.
	f.add(t, []db.UsageBucket{{SpaceID: "spc_b", AccountID: "acc_a", Role: "shared", Harness: "claude-code", Model: "m-b", Granularity: GranHour,
		BucketStart: globalNow.Truncate(time.Hour).UnixMicro(), InputTokens: 5}})
	acc := f.allPages(t, SeriesQuery{GroupBy: "account"}, Filter{}, Scope{Spaces: []string{"spc_b"}}, 50)
	hasA := slices.ContainsFunc(acc.Rows, func(r Row) bool { return r.Group == "acc_a" })
	if !hasA || acc.Labels["acc_b"] != "name of acc_b" || acc.Labels["acc_s"] != "name of acc_s" || acc.Labels["acc_a"] != "" {
		t.Fatalf("account labels %v (acc_a row %v)", acc.Labels, hasA)
	}
	// A run outside the range is not counted.
	old := globalNow.Add(-30 * 24 * time.Hour).UnixMicro()
	f.run(t, "run_old_codex", "spc_a", "shared", "codex", "", "", old, old+1)
	if p := f.allPages(t, SeriesQuery{GroupBy: "space"}, Filter{}, Scope{All: true}, 50); !reflect.DeepEqual(p.NotCounted, want) {
		t.Fatalf("old run counted %+v", p.NotCounted)
	}
}

func TestGlobalSeriesValidation(t *testing.T) {
	t.Parallel()
	f := newGlobalFx(t, []string{"spc_a"}, nil, nil)
	many := make([]string, MaxFilterValues+1)
	for i := range many {
		many[i] = fmt.Sprint("v", i)
	}
	for name, c := range map[string]struct {
		q   SeriesQuery
		flt Filter
	}{
		"bad group":      {SeriesQuery{GroupBy: "node"}, Filter{}},
		"hours too long": {SeriesQuery{GroupBy: "space", From: globalNow.Add(-32 * 24 * time.Hour), To: globalNow}, Filter{}},
		"days too long":  {SeriesQuery{GroupBy: "space", Bucket: GranDay, From: globalNow.Add(-801 * 24 * time.Hour), To: globalNow}, Filter{}},
		"spaces":         {SeriesQuery{GroupBy: "space"}, Filter{Spaces: many}},
		"accounts":       {SeriesQuery{GroupBy: "space"}, Filter{Accounts: many}},
		"models":         {SeriesQuery{GroupBy: "space"}, Filter{Models: many}},
		"roles":          {SeriesQuery{GroupBy: "space"}, Filter{Roles: many}},
		"harnesses":      {SeriesQuery{GroupBy: "space"}, Filter{Harnesses: many}},
		"long value":     {SeriesQuery{GroupBy: "space"}, Filter{Models: []string{strings.Repeat("x", 201)}}},
	} {
		if _, err := f.svc.GlobalSeries(f.ctx, c.q, c.flt, Scope{All: true}, nil, 50); apperr.CodeOf(err) != apperr.CodeInvalid {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Exactly 50 values, and duplicates that collapse to 50, are accepted; a 90-day daily range is accepted.
	dup := append(slices.Clone(many[:MaxFilterValues]), many[:10]...)
	if _, err := f.svc.GlobalSeries(f.ctx, SeriesQuery{GroupBy: "model", Bucket: GranDay, From: globalNow.Add(-90 * 24 * time.Hour)},
		Filter{Models: dup}, Scope{All: true}, nil, 50); err != nil {
		t.Fatal(err)
	}
}
