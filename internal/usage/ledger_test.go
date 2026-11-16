package usage

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/keyring/keyringtest"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/storetest"
)

func mu(model string, in, out, cw, cr, req int64) proto.ModelUsage {
	return proto.ModelUsage{Model: model, Input: &in, Output: &out, CacheWrite: &cw, CacheRead: &cr, Requests: req}
}

func TestComputeDelta(t *testing.T) {
	t.Parallel()
	// A first report counts everything; the same report again counts nothing; a bigger one counts the growth.
	d := computeDelta(nil, 0, []proto.ModelUsage{mu("m", 10, 5, 0, 100, 2)}, &proto.CostEstimate{USD: 1})
	if len(d.Models) != 1 || d.Models[0].Input != 10 || d.Models[0].CacheRead != 100 || d.Models[0].Requests != 2 || d.Cost != 1 {
		t.Fatalf("first %+v", d)
	}
	d2 := computeDelta(d.Ledger, d.LedgerCost, []proto.ModelUsage{mu("m", 10, 5, 0, 100, 2)}, &proto.CostEstimate{USD: 1})
	if len(d2.Models) != 0 || d2.Cost != 0 {
		t.Fatalf("repeat %+v", d2)
	}
	d3 := computeDelta(d.Ledger, d.LedgerCost, []proto.ModelUsage{mu("m", 15, 5, 0, 130, 3), mu("n", 1, 1, 0, 0, 1)}, &proto.CostEstimate{USD: 1.5})
	if len(d3.Models) != 2 || d3.Models[0].Model != "m" || d3.Models[0].Input != 5 || d3.Models[0].CacheRead != 30 || d3.Models[0].Output != 0 ||
		d3.Models[1].Model != "n" || math.Abs(d3.Cost-0.5) > 1e-9 {
		t.Fatalf("growth %+v", d3)
	}
	// A stale report (lower counters) adds nothing and does not lower the marks, so the newer report after it adds nothing either.
	stale := computeDelta(d3.Ledger, d3.LedgerCost, []proto.ModelUsage{mu("m", 12, 5, 0, 110, 2)}, &proto.CostEstimate{USD: 1.2})
	if len(stale.Models) != 0 || stale.Cost != 0 {
		t.Fatalf("stale %+v", stale)
	}
	again := computeDelta(stale.Ledger, stale.LedgerCost, []proto.ModelUsage{mu("m", 15, 5, 0, 130, 3), mu("n", 1, 1, 0, 0, 1)}, &proto.CostEstimate{USD: 1.5})
	if len(again.Models) != 0 || again.Cost != 0 {
		t.Fatalf("after stale %+v", again)
	}
	// Unknown (absent) counters count as zero; bad names, duplicates, negatives, huge values and bad costs are bounded.
	odd := computeDelta(nil, 0, []proto.ModelUsage{
		{Model: "m", Requests: 1},
		{Model: "bad name\n<script>", Input: i64(3)},
		{Model: "m", Input: i64(4)},
		{Model: "m", Input: i64(2)},
		{Model: "x", Input: i64(-5), Output: i64(math.MaxInt64)},
	}, &proto.CostEstimate{USD: math.NaN()})
	got := map[string]counters{}
	for _, c := range odd.Models {
		got[c.Model] = c
	}
	if got["m"].Input != 4 || got["m"].Requests != 1 || got[UnknownModel].Input != 3 || got["x"].Input != 0 || got["x"].Output != maxCounter || len(got) != 3 || odd.Cost != 0 {
		t.Fatalf("odd %+v", odd)
	}
	if huge := computeDelta(nil, 0, nil, &proto.CostEstimate{USD: 1e12}); huge.Cost != maxCost {
		t.Fatalf("cost bound %v", huge.Cost)
	}
	if got := (Delta{}).costModel(); got != UnknownModel {
		t.Fatal(got)
	}
}

type ledgerFx struct {
	svc *Service
	ctx context.Context
	clk *clock.Fake
	s   *store.Store
}

const (
	lrA = "run_01aaaaaaaaaaaaaaaaaaaaaaaa" // account 1
	lrB = "run_01bbbbbbbbbbbbbbbbbbbbbbbb" // account 2, resumes lrA's session
	lrC = "run_01cccccccccccccccccccccccc" // account 1 again, resumes the same session
	lrD = "run_01dddddddddddddddddddddddd" // no session id, no account
)

func newLedger(t *testing.T) *ledgerFx {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 11, 2, 9, 35, 0, 0, time.UTC))
	s := storetest.Open(t, storetest.WithClock(clk))
	kr := keyringtest.Load(t, s)
	ctx := context.Background()
	qs := []string{
		`INSERT INTO nodes (id, name, state, created_at, created_by, updated_at) VALUES ('nod_1', 'n1', 'active', 0, 't', 0)`,
		`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('spc_a', 'aaa', 'A', 'nod_1', 'active', 'a', 0, 't', 0)`,
		`INSERT INTO accounts (id, name, kind, secret_enc, secret_rotated_at, created_at, created_by, updated_at) VALUES
			('acc_1', 'a1', 'claude_oauth_token', x'00', 0, 0, 't', 0), ('acc_2', 'a2', 'claude_oauth_token', x'00', 0, 0, 't', 0)`,
	}
	run := func(id, account, session string) string {
		launch := "{}"
		if account != "" {
			launch = fmt.Sprintf(`{"account_ref":"%s"}`, account)
		}
		return fmt.Sprintf(`INSERT INTO runs (id, space_id, node_id, generation, kind, role, harness, instance_id, session_id, desired, observed, launch_json, manifest_json, created_at, created_by, updated_at)
			VALUES ('%s', 'spc_a', 'nod_1', 1, 'worker', 'implementor', 'claude-code', 'ins_1', '%s', 'running', 'running', '%s', '{}', 0, 't', 0)`, id, session, launch)
	}
	qs = append(qs, run(lrA, "acc_1", "sess-1"), run(lrB, "acc_2", "sess-1"), run(lrC, "acc_1", "sess-1"), run(lrD, "", ""))
	if err := s.W().TxNamed(ctx, "seed", func(tx *store.Tx) error {
		for _, q := range qs {
			if _, err := tx.SQL.ExecContext(ctx, q); err != nil {
				return fmt.Errorf("%s: %w", q, err)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return &ledgerFx{svc: New(s, audit.NewWriter(kr), changefeed.New(nil), t.TempDir(), nil, nil), ctx: ctx, clk: clk, s: s}
}

func (f *ledgerFx) report(run string, final bool, usd float64, models ...proto.ModelUsage) {
	f.svc.Report(f.ctx, "nod_1", proto.UsageReport{RunID: run, Source: proto.UsageClaudeTranscript, Models: models, Final: final,
		Estimate: &proto.CostEstimate{USD: usd, Source: "t", Assumptions: "t"}})
}

type bucket struct {
	account, model string
	hour           int64
	in, out, req   int64
	cost           float64
}

func (f *ledgerFx) buckets(t *testing.T) []bucket {
	t.Helper()
	rows, err := f.s.ReadDB().QueryContext(f.ctx, `SELECT account_id, model, bucket_start, input_tokens, output_tokens, requests, cost_usd FROM usage_buckets
		WHERE granularity = 'hour' ORDER BY bucket_start, account_id, model`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []bucket
	for rows.Next() {
		var b bucket
		if err := rows.Scan(&b.account, &b.model, &b.hour, &b.in, &b.out, &b.req, &b.cost); err != nil {
			t.Fatal(err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func sumIn(bs []bucket, account string) (in int64) {
	for _, b := range bs {
		if account == "*" || b.account == account {
			in += b.in
		}
	}
	return in
}

// Repeats, out-of-order reports and the final report count each token once.
func TestLedgerRepeatsOutOfOrderAndFinal(t *testing.T) {
	t.Parallel()
	f := newLedger(t)
	hour := HourStart(f.clk.Now())
	f.report(lrA, false, 0.1, mu("m", 100, 10, 0, 0, 2))
	f.report(lrA, false, 0.1, mu("m", 100, 10, 0, 0, 2)) // a repeat
	f.report(lrA, false, 0.3, mu("m", 300, 30, 0, 0, 6)) // growth
	f.report(lrA, false, 0.2, mu("m", 200, 20, 0, 0, 4)) // stale: the node's report arrived after a newer one
	f.clk.Advance(time.Hour)
	f.report(lrA, false, 0.3, mu("m", 300, 30, 0, 0, 6)) // the newer one again after the stale one: nothing new
	f.report(lrA, true, 0.5, mu("m", 450, 45, 0, 0, 9))  // the final report adds only the rest, in the new hour
	f.report(lrA, false, 9, mu("m", 9999, 1, 0, 0, 1))   // after the final report nothing is booked
	f.report(lrA, true, 9, mu("m", 9999, 1, 0, 0, 1))

	bs := f.buckets(t)
	if len(bs) != 2 {
		t.Fatalf("%+v", bs)
	}
	if b := bs[0]; b.hour != hour || b.account != "acc_1" || b.model != "m" || b.in != 300 || b.out != 30 || b.req != 6 || math.Abs(b.cost-0.3) > 1e-9 {
		t.Fatalf("first hour %+v", b)
	}
	if b := bs[1]; b.hour != hour+time.Hour.Microseconds() || b.in != 150 || b.out != 15 || b.req != 3 || math.Abs(b.cost-0.2) > 1e-9 {
		t.Fatalf("second hour %+v", b)
	}
	var role, harness, acct string
	if err := f.s.ReadDB().QueryRowContext(f.ctx, `SELECT role, harness, account_id FROM run_usage WHERE run_id = ?`, lrA).Scan(&role, &harness, &acct); err != nil ||
		role != "implementor" || harness != "claude-code" || acct != "acc_1" {
		t.Fatalf("run_usage columns %q %q %q %v", role, harness, acct, err)
	}
}

// A chain of switches: the resumed runs continue the same transcript, so their cumulative counters start where the
// earlier run's ended; each account is booked only with what it used.
func TestLedgerSwitchChain(t *testing.T) {
	t.Parallel()
	f := newLedger(t)
	f.report(lrA, true, 1, mu("m", 1000, 100, 0, 0, 10)) // account 1 until its limit
	f.report(lrB, false, 1.4, mu("m", 1300, 130, 0, 0, 14))
	f.report(lrB, true, 1.6, mu("m", 1500, 150, 0, 0, 16)) // account 2 used 500
	f.report(lrC, true, 2, mu("m", 1700, 170, 0, 0, 18))   // back on account 1 after its reset: 200 more
	f.report(lrB, true, 2, mu("m", 1500, 150, 0, 0, 16))   // a late repeat of B's final: nothing
	bs := f.buckets(t)
	if got1, got2 := sumIn(bs, "acc_1"), sumIn(bs, "acc_2"); got1 != 1200 || got2 != 500 {
		t.Fatalf("account 1 %d, account 2 %d: %+v", got1, got2, bs)
	}
	var cost1, cost2 float64
	for _, b := range bs {
		if b.account == "acc_1" {
			cost1 += b.cost
		} else {
			cost2 += b.cost
		}
	}
	if math.Abs(cost1-1.4) > 1e-9 || math.Abs(cost2-0.6) > 1e-9 {
		t.Fatalf("costs %v %v", cost1, cost2)
	}
}

// A run without a session id or an account is booked under the empty account, on its own marks; unknown stays unknown.
func TestLedgerRunWithoutSessionOrAccount(t *testing.T) {
	t.Parallel()
	f := newLedger(t)
	f.svc.Report(f.ctx, "nod_1", proto.UsageReport{RunID: lrD, Source: proto.UsageClaudeTranscript, Models: []proto.ModelUsage{{Model: "m", Input: i64(5), Requests: 1}},
		Windows: []proto.UsageWindow{{Kind: "five_hour", UsedPct: 10, ObservedAt: f.clk.Now()}}})
	f.svc.Report(f.ctx, "nod_1", proto.UsageReport{RunID: lrD, Source: proto.UsageClaudeTranscript, Models: []proto.ModelUsage{{Model: "m", Input: i64(5), Requests: 1}}})
	f.svc.Report(f.ctx, "nod_1", proto.UsageReport{RunID: lrA, Source: proto.UsageUnavailable, Final: true})
	bs := f.buckets(t)
	if len(bs) != 1 || bs[0].account != "" || bs[0].in != 5 || bs[0].out != 0 {
		t.Fatalf("%+v", bs)
	}
	if n := countRows(t, f, `SELECT count(*) FROM account_windows`); n != 0 {
		t.Fatal("a run without an account kept a window")
	}
}

func countRows(t *testing.T, f *ledgerFx, q string, args ...any) int {
	t.Helper()
	var n int
	if err := f.s.ReadDB().QueryRowContext(f.ctx, q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The newest observation per account and kind wins; an older one is ignored; windows are validated again.
func TestAccountWindows(t *testing.T) {
	t.Parallel()
	f := newLedger(t)
	now := f.clk.Now()
	win := func(kind string, pct float64, resets *time.Time, observed time.Time) proto.UsageWindow {
		return proto.UsageWindow{Kind: kind, UsedPct: pct, ResetsAt: resets, ObservedAt: observed}
	}
	at := func(d time.Duration) *time.Time { t := now.Add(d); return &t }
	rep := func(run string, ws ...proto.UsageWindow) {
		f.svc.Report(f.ctx, "nod_1", proto.UsageReport{RunID: run, Source: proto.UsageClaudeTranscript, Windows: ws})
	}
	rep(lrA, win("five_hour", 40, at(2*time.Hour), now.Add(-time.Minute)), win("seven_day", 10, at(48*time.Hour), now.Add(-time.Minute)))
	rep(lrA, win("five_hour", 30, at(time.Hour), now.Add(-5*time.Minute))) // older: ignored
	// Run B is on another account: its windows are that account's.
	rep(lrB, win("five_hour", 90, at(time.Hour), now.Add(-2*time.Minute)))
	rep(lrA, win("five_hour", 55.5, nil, now))
	type w struct {
		used   float64
		resets sql.NullInt64
		run    string
	}
	get := func(acct, kind string) (w, bool) {
		var x w
		err := f.s.ReadDB().QueryRowContext(f.ctx, `SELECT used_pct, resets_at, run_id FROM account_windows WHERE account_id = ? AND kind = ?`, acct, kind).Scan(&x.used, &x.resets, &x.run)
		return x, err == nil
	}
	if x, ok := get("acc_1", "five_hour"); !ok || x.used != 55.5 || x.resets.Valid || x.run != lrA {
		t.Fatalf("acc_1 five_hour %+v %v", x, ok)
	}
	if x, ok := get("acc_1", "seven_day"); !ok || x.used != 10 || !x.resets.Valid {
		t.Fatalf("acc_1 seven_day %+v", x)
	}
	if x, ok := get("acc_2", "five_hour"); !ok || x.used != 90 || x.run != lrB {
		t.Fatalf("acc_2 %+v", x)
	}
	// Invalid windows are dropped: unknown kind, negative, NaN, from the future or from before the limit window; 150 % is
	// clamped; a reset in the past or too far ahead is dropped without the window.
	rep(lrA, win("seven_day_opus", 1, nil, now), win("seven_day", -1, nil, now), win("seven_day", math.NaN(), nil, now), win("five_hour", 1, nil, now.Add(time.Hour)),
		win("five_hour", 1, nil, now.Add(-9*24*time.Hour)), win("five_hour", 1, nil, time.Time{}))
	if x, _ := get("acc_1", "five_hour"); x.used != 55.5 {
		t.Fatalf("an invalid window replaced the stored one: %+v", x)
	}
	f.clk.Advance(time.Minute)
	rep(lrA, win("five_hour", 150, at(9*24*time.Hour), now.Add(30*time.Second)), win("seven_day", 20, at(-time.Hour), now.Add(30*time.Second)))
	if x, _ := get("acc_1", "five_hour"); x.used != 100 || x.resets.Valid {
		t.Fatalf("clamp %+v", x)
	}
	if x, _ := get("acc_1", "seven_day"); x.used != 20 || x.resets.Valid {
		t.Fatalf("past reset %+v", x)
	}
	// Samples: one per account, kind and ten-minute slot, the newest observation in it.
	if n := countRows(t, f, `SELECT count(*) FROM account_window_samples WHERE account_id = 'acc_1' AND kind = 'five_hour'`); n != 1 {
		t.Fatalf("samples in one slot: %d", n)
	}
	f.clk.Advance(20 * time.Minute)
	rep(lrA, win("five_hour", 60, nil, f.clk.Now()))
	if n := countRows(t, f, `SELECT count(*) FROM account_window_samples WHERE account_id = 'acc_1' AND kind = 'five_hour'`); n != 2 {
		t.Fatalf("samples in two slots: %d", n)
	}
	// A deleted account takes its windows with it.
	if err := f.s.W().TxNamed(f.ctx, "del", func(tx *store.Tx) error {
		_, err := tx.SQL.ExecContext(f.ctx, `DELETE FROM accounts WHERE id = 'acc_2'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, f, `SELECT count(*) FROM account_windows WHERE account_id = 'acc_2'`); n != 0 {
		t.Fatal("windows of a deleted account stayed")
	}
	// A run whose account was deleted after launch keeps no window and breaks nothing.
	rep(lrB, win("five_hour", 5, nil, f.clk.Now()))
	if n := countRows(t, f, `SELECT count(*) FROM account_windows WHERE account_id = 'acc_2'`); n != 0 {
		t.Fatal("a window for a deleted account was stored")
	}
}

// The headroom read gives the most-used current window; stale and past-reset windows are unknown.
func TestMostUsedWindow(t *testing.T) {
	t.Parallel()
	f := newLedger(t)
	now := f.clk.Now()
	read := func(acct string) (float64, time.Time, bool) {
		u, r, k, err := AccountHeadroomFunc(f.clk.Now)(f.ctx, f.s.R(), acct)
		if err != nil {
			t.Fatal(err)
		}
		return u, r, k
	}
	if _, _, k := read("acc_1"); k {
		t.Fatal("no window is known")
	}
	r5, r7 := now.Add(2*time.Hour), now.Add(48*time.Hour)
	f.svc.Report(f.ctx, "nod_1", proto.UsageReport{RunID: lrA, Source: proto.UsageClaudeTranscript, Windows: []proto.UsageWindow{
		{Kind: "five_hour", UsedPct: 40, ResetsAt: &r5, ObservedAt: now}, {Kind: "seven_day", UsedPct: 72, ResetsAt: &r7, ObservedAt: now}}})
	if u, r, k := read("acc_1"); !k || u != 72 || !r.Equal(r7) {
		t.Fatalf("most used %v %v %v", u, r, k)
	}
	f.clk.Advance(3 * time.Hour) // the five-hour reset passed and the observations are stale
	if _, _, k := read("acc_1"); k {
		t.Fatal("stale windows counted")
	}
}

// A run that reported before it knew its session id keeps its counted counters when the id appears.
func TestLedgerSessionLearnedLater(t *testing.T) {
	t.Parallel()
	f := newLedger(t)
	exec := func(q string) {
		if err := f.s.W().TxNamed(f.ctx, "x", func(tx *store.Tx) error { _, err := tx.SQL.ExecContext(f.ctx, q); return err }); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE runs SET session_id = '' WHERE id = '` + lrA + `'`)
	f.report(lrA, false, 0, mu("m", 100, 1, 0, 0, 1))
	exec(`UPDATE runs SET session_id = 'sess-1' WHERE id = '` + lrA + `'`)
	f.report(lrA, true, 0, mu("m", 150, 1, 0, 0, 2))
	if got := sumIn(f.buckets(t), "acc_1"); got != 150 {
		t.Fatalf("counted %d, want 150", got)
	}
}

// A far-future observation is dropped, not clamped to now: no window is stored for it.
func TestAccountWindowFutureObservationDropped(t *testing.T) {
	t.Parallel()
	f := newLedger(t)
	now := f.clk.Now()
	for _, ahead := range []time.Duration{time.Hour, 24 * time.Hour} {
		f.svc.Report(f.ctx, "nod_1", proto.UsageReport{RunID: lrA, Source: proto.UsageClaudeTranscript, Windows: []proto.UsageWindow{
			{Kind: "seven_day", UsedPct: 50, ObservedAt: now.Add(ahead)}}})
	}
	if n := countRows(t, f, `SELECT count(*) FROM account_windows`) + countRows(t, f, `SELECT count(*) FROM account_window_samples`); n != 0 {
		t.Fatalf("a future observation was kept (%d rows)", n)
	}
	// Within the skew it is kept, as of now.
	f.svc.Report(f.ctx, "nod_1", proto.UsageReport{RunID: lrA, Source: proto.UsageClaudeTranscript, Windows: []proto.UsageWindow{
		{Kind: "seven_day", UsedPct: 50, ObservedAt: now.Add(time.Minute)}}})
	if n := countRows(t, f, `SELECT count(*) FROM account_windows WHERE observed_at = ?`, now.UnixMicro()); n != 1 {
		t.Fatalf("a window inside the skew: %d", n)
	}
}

// A mark the ledger cannot read fails the report's transaction: nothing is booked and the run's usage row is not stored,
// so the node's next report is not lost as a repeat.
func TestLedgerUnreadableMark(t *testing.T) {
	t.Parallel()
	f := newLedger(t)
	if err := f.s.W().TxNamed(f.ctx, "x", func(tx *store.Tx) error {
		_, err := tx.SQL.ExecContext(f.ctx, `INSERT INTO usage_ledger_marks (space_id, mark_key, counted_json, counted_cost, updated_at) VALUES ('spc_a', 'sess-1', 'not json', 0, 0)`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	f.report(lrA, false, 1, mu("m", 100, 1, 0, 0, 1))
	if got := f.buckets(t); len(got) != 0 {
		t.Fatalf("booked with an unreadable mark: %+v", got)
	}
	if n := countRows(t, f, `SELECT count(*) FROM run_usage WHERE run_id = ?`, lrA); n != 0 {
		t.Fatal("the report was stored although its booking failed")
	}
}

// A report whose run was read before the session id was learned (a stale read) books against the session's mark, not
// the old run key: the growth the session already holds is not booked twice.
func TestLedgerStaleSessionRead(t *testing.T) {
	t.Parallel()
	f := newLedger(t)
	exec := func(q string) {
		if err := f.s.W().TxNamed(f.ctx, "x", func(tx *store.Tx) error { _, err := tx.SQL.ExecContext(f.ctx, q); return err }); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE runs SET session_id = '' WHERE id = '` + lrA + `'`)
	f.report(lrA, false, 0, mu("m", 100, 1, 0, 0, 1)) // filed under run:<id>
	stale, err := f.s.R().GetRun(f.ctx, lrA)
	if err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE runs SET session_id = 'sess-1' WHERE id = '` + lrA + `'`)
	f.report(lrA, false, 0, mu("m", 150, 1, 0, 0, 2)) // moves the mark to the session key
	// The stale report is booked with the run as read before (no session id), as a concurrent Report would.
	rep := proto.UsageReport{RunID: lrA, Source: proto.UsageClaudeTranscript, Models: []proto.ModelUsage{mu("m", 120, 1, 0, 0, 2)}}
	if err := f.s.W().TxNamed(f.ctx, "stale", func(tx *store.Tx) error { return f.svc.account(f.ctx, tx, stale, rep) }); err != nil {
		t.Fatal(err)
	}
	if got := sumIn(f.buckets(t), "acc_1"); got != 150 {
		t.Fatalf("counted %d, want 150 (the stale report's growth was booked twice)", got)
	}
	// Both keys holding marks: the higher counters win, so neither key books what the other holds.
	exec(`INSERT INTO usage_ledger_marks (space_id, mark_key, counted_json, counted_cost, updated_at) VALUES ('spc_a', 'run:` + lrA + `', '[{"model":"m","in":400,"out":0,"cw":0,"cr":0,"req":0}]', 0, 0)
		ON CONFLICT DO UPDATE SET counted_json = excluded.counted_json`)
	f.report(lrA, true, 0, mu("m", 450, 1, 0, 0, 3))
	if got := sumIn(f.buckets(t), "acc_1"); got != 200 {
		t.Fatalf("counted %d after the merge, want 200 (150 plus the growth over the higher mark, 50)", got)
	}
}

// A window that has reset or went stale is skipped even when it is the most used; the next current one answers.
func TestMostUsedWindowSkipsStaleAndReset(t *testing.T) {
	t.Parallel()
	f := newLedger(t)
	now := f.clk.Now()
	read := func() (float64, bool) {
		u, _, k, err := AccountHeadroomFunc(f.clk.Now)(f.ctx, f.s.R(), "acc_1")
		if err != nil {
			t.Fatal(err)
		}
		return u, k
	}
	r5, r7 := now.Add(30*time.Minute), now.Add(48*time.Hour)
	f.svc.Report(f.ctx, "nod_1", proto.UsageReport{RunID: lrA, Source: proto.UsageClaudeTranscript, Windows: []proto.UsageWindow{
		{Kind: "five_hour", UsedPct: 95, ResetsAt: &r5, ObservedAt: now}, {Kind: "seven_day", UsedPct: 30, ResetsAt: &r7, ObservedAt: now}}})
	if u, k := read(); !k || u != 95 {
		t.Fatalf("most used %v %v", u, k)
	}
	f.clk.Advance(40 * time.Minute) // the five-hour window reset (95 % no longer counts); the seven-day one is still current
	f.svc.Report(f.ctx, "nod_1", proto.UsageReport{RunID: lrA, Source: proto.UsageClaudeTranscript, Windows: []proto.UsageWindow{
		{Kind: "seven_day", UsedPct: 31, ResetsAt: &r7, ObservedAt: f.clk.Now()}}})
	if u, k := read(); !k || u != 31 {
		t.Fatalf("after the reset %v %v, want the seven-day window", u, k)
	}
	f.clk.Advance(2 * time.Hour) // everything stale: unknown, not zero
	if u, k := read(); k || u != 0 {
		t.Fatalf("stale %v %v", u, k)
	}
}
