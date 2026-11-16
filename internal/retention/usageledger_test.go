package retention

import (
	"fmt"
	"testing"
	"time"
)

// ADR-0090 Q9: hourly buckets 90 days (then rolled into daily rows), daily 730 days, window samples 30 days, closed
// holds, waits and switches 180 days; open and active rows stay however old.
func TestPassPrunesTheUsageLedger(t *testing.T) {
	t.Parallel()
	f := newFx(t, 0)
	now := f.clk.Now()
	at := func(d time.Duration) int64 { return now.Add(-d).UnixMicro() }
	const dayD = 24 * time.Hour
	utc := func(y int, m time.Month, d, h int) int64 { return time.Date(y, m, d, h, 0, 0, 0, time.UTC).UnixMicro() }

	bucket := func(gran string, start int64, in int, model string) string {
		return fmt.Sprintf(`INSERT INTO usage_buckets (space_id, account_id, role, harness, model, granularity, bucket_start, input_tokens, output_tokens, requests, cost_usd)
			VALUES ('spc_a', 'acc_1', 'w', 'claude-code', '%s', '%s', %d, %d, 1, 1, 0.5)`, model, gran, start, in)
	}
	// The cutoff is 2026-08-05 09:00: whole UTC days before 08-05 roll up, the day of the cutoff keeps its hours.
	f.exec(
		bucket("hour", utc(2026, 8, 4, 10), 10, "m1"),
		bucket("hour", utc(2026, 8, 4, 15), 20, "m1"),
		bucket("hour", utc(2026, 8, 4, 15), 5, "m2"),
		bucket("day", utc(2026, 8, 4, 0), 100, "m1"), // a daily row that exists already is added to
		bucket("hour", utc(2026, 8, 5, 2), 7, "m1"),  // the cutoff's own day: kept hourly
		bucket("hour", at(10*dayD), 9, "m1"),
		bucket("day", utc(2024, 11, 2, 0), 1, "m1"), // older than 730 days
		bucket("day", utc(2024, 11, 4, 0), 2, "m1"), // younger
	)
	f.exec(`INSERT INTO accounts (id, name, kind, secret_enc, secret_rotated_at, created_at, created_by, updated_at) VALUES ('acc_1', 'a1', 'claude_oauth_token', x'00', 0, 0, 't', 0)`)
	sample := func(slot int, ago time.Duration) string {
		return fmt.Sprintf(`INSERT INTO account_window_samples (account_id, kind, slot, used_pct, observed_at) VALUES ('acc_1', 'five_hour', %d, 10, %d)`, slot, at(ago))
	}
	f.exec(sample(1, 31*dayD), sample(2, 29*dayD),
		fmt.Sprintf(`INSERT INTO usage_ledger_marks (space_id, mark_key, updated_at) VALUES ('spc_a', 'old', %d), ('spc_a', 'new', %d)`, at(181*dayD), at(179*dayD)))
	f.exec(`INSERT INTO runs (id, space_id, node_id, generation, kind, role, harness, instance_id, desired, observed, launch_json, created_at, created_by, updated_at)
		VALUES ('run_1', 'spc_a', 'nod_1', 1, 'worker', 'w', 'claude-code', 'ins', 'stopped', 'exited', '{}', 0, 't', 0),
		       ('run_2', 'spc_a', 'nod_1', 1, 'worker', 'w', 'claude-code', 'ins2', 'stopped', 'exited', '{}', 0, 't', 0),
		       ('run_3', 'spc_a', 'nod_1', 1, 'worker', 'w', 'claude-code', 'ins3', 'running', 'running', '{}', 0, 't', 0)`)
	limit := func(id, kind string, cleared any, seen int64) string {
		return fmt.Sprintf(`INSERT INTO usage_limits (id, space_id, scope, account_id, kind, source, seen_at, cleared_at, created_at, updated_at)
			VALUES ('%s', 'spc_a', 'account', 'acc_1', '%s', 'transcript', %d, %v, %d, %d)`, id, kind, seen, cleared, seen, seen)
	}
	wait := func(id, run, inst, state string, updated int64) string {
		return fmt.Sprintf(`INSERT INTO limit_waits (id, space_id, run_id, run_kind, role, node_id, instance_id, harness, launch_json, resume_at, state, created_at, updated_at)
			VALUES ('%s', 'spc_a', '%s', 'worker', 'w', 'nod_1', '%s', 'claude-code', '{}', 0, '%s', %d, %d)`, id, run, inst, state, updated, updated)
	}
	sw := func(id, inst, state string, updated int64) string {
		return fmt.Sprintf(`INSERT INTO account_switches (id, space_id, instance_id, role, run_kind, harness, from_run_id, from_account_id, cause, limit_kind, state, created_at, updated_at)
			VALUES ('%s', 'spc_a', '%s', 'w', 'worker', 'claude-code', 'run_1', 'acc_1', 'usage_limit', 'five_hour', '%s', %d, %d)`, id, inst, state, updated, updated)
	}
	f.exec(
		limit("ul_old", "five_hour", at(181*dayD), at(200*dayD)),
		limit("ul_new", "seven_day", at(179*dayD), at(200*dayD)),
		limit("ul_open", "overage", "NULL", at(300*dayD)),
		wait("lw_old", "run_1", "ins", "resumed", at(181*dayD)),
		wait("lw_young", "run_2", "ins2", "failed", at(179*dayD)),
		wait("lw_open", "run_3", "ins3", "waiting", at(300*dayD)),
		sw("sw_old", "ins", "succeeded", at(181*dayD)),
		sw("sw_young", "ins2", "failed", at(179*dayD)),
		sw("sw_active", "ins3", "parked", at(300*dayD)),
	)

	p, err := f.svc.Run(f.ctx, admin)
	must(t, err)
	if p.Deleted[KindUsageBuckets] != 4 || p.Deleted[KindWindowSamples] != 1 || p.Deleted[KindLedgerMarks] != 1 || p.Deleted[KindUsageLimits] != 1 ||
		p.Deleted[KindLimitWaits] != 1 || p.Deleted[KindAccountSwitches] != 1 {
		// 3 hourly rows rolled up + 1 daily row past 730 days.
		t.Fatalf("counts %v", p.Deleted)
	}
	row := func(gran string, start int64, model string) (int, int) {
		var n, in int
		must(t, f.s.ReadDB().QueryRowContext(f.ctx, `SELECT count(*), COALESCE(sum(input_tokens), 0) FROM usage_buckets WHERE granularity = ? AND bucket_start = ? AND model = ?`,
			gran, start, model).Scan(&n, &in))
		return n, in
	}
	if n, in := row("day", utc(2026, 8, 4, 0), "m1"); n != 1 || in != 130 {
		t.Fatalf("rolled m1 %d %d, want one row of 130 (100 existing + 10 + 20)", n, in)
	}
	if n, in := row("day", utc(2026, 8, 4, 0), "m2"); n != 1 || in != 5 {
		t.Fatalf("rolled m2 %d %d", n, in)
	}
	if n, _ := row("hour", utc(2026, 8, 4, 10), "m1"); n != 0 {
		t.Fatal("a rolled-up hour is still there")
	}
	if n, _ := row("hour", utc(2026, 8, 5, 2), "m1"); n != 1 {
		t.Fatal("the cutoff's own day was rolled up early")
	}
	if n, _ := row("hour", at(10*dayD), "m1"); n != 1 {
		t.Fatal("a recent hour was rolled up")
	}
	if n, _ := row("day", utc(2024, 11, 2, 0), "m1"); n != 0 {
		t.Fatal("a daily row past 730 days stayed")
	}
	if n, _ := row("day", utc(2024, 11, 4, 0), "m1"); n != 1 {
		t.Fatal("a daily row inside 730 days went")
	}
	for q, want := range map[string]int{
		`SELECT count(*) FROM account_window_samples`:                1,
		`SELECT count(*) FROM usage_ledger_marks`:                    1,
		`SELECT count(*) FROM usage_limits WHERE id = 'ul_old'`:      0,
		`SELECT count(*) FROM usage_limits WHERE id <> 'ul_old'`:     2,
		`SELECT count(*) FROM limit_waits WHERE id = 'lw_old'`:       0,
		`SELECT count(*) FROM limit_waits WHERE id <> 'lw_old'`:      2,
		`SELECT count(*) FROM account_switches WHERE id = 'sw_old'`:  0,
		`SELECT count(*) FROM account_switches WHERE id <> 'sw_old'`: 2,
		`SELECT sum(input_tokens) FROM usage_buckets`:                130 + 5 + 7 + 9 + 2,
		`SELECT sum(requests) FROM usage_buckets WHERE model = 'm1'`: 6,
	} {
		if got := f.count(q); got != want {
			t.Errorf("%s = %d, want %d", q, got, want)
		}
	}
	// A second pass has nothing left to do.
	p, err = f.svc.Run(f.ctx, admin)
	must(t, err)
	for _, k := range []string{KindUsageBuckets, KindWindowSamples, KindLedgerMarks, KindUsageLimits, KindLimitWaits, KindAccountSwitches} {
		if p.Deleted[k] != 0 {
			t.Errorf("second pass deleted %s: %d", k, p.Deleted[k])
		}
	}
}
