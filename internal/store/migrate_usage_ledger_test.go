package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
)

// 0058 adds the usage ledger to a database at 0056 (the head before it): run_usage gets the run's account, role and
// harness (backfilled from the run and its launch spec), existing usage is marked as counted (one mark per session, from
// the latest report), and the new tables work.
func TestUsageLedgerMigrationFrom0056(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "data")
	s, err := Open(ctx, Options{DataDir: dir, Migrations: embeddedUpTo(t, "0056")})
	if err != nil {
		t.Fatal(err)
	}
	run := func(id, session, launch, role string) string {
		return `INSERT INTO runs (id, space_id, node_id, generation, kind, role, harness, instance_id, session_id, desired, observed, launch_json, created_at, created_by, updated_at)
			VALUES ('` + id + `', 'spc_a', 'nod_1', 1, 'worker', '` + role + `', 'claude-code', 'ins_1', '` + session + `', 'stopped', 'exited', '` + launch + `', 0, 't', 0)`
	}
	usage := func(id, models, est string, updated int) string {
		return `INSERT INTO run_usage (run_id, space_id, source, models_json, estimate_json, final, updated_at) VALUES ('` + id + `', 'spc_a', 'claude-transcript', '` + models + `', '` + est + `', 1, ` +
			fmt.Sprint(updated) + `)`
	}
	for _, q := range []string{
		`INSERT INTO nodes (id, name, state, created_at, created_by, updated_at) VALUES ('nod_1', 'n1', 'active', 0, 't', 0)`,
		`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('spc_a', 'aaa', 'A', 'nod_1', 'active', 'a', 0, 't', 0)`,
		run("run_1", "sess-1", `{"account_ref":"acc_1"}`, "impl"),
		run("run_2", "sess-1", `{"account_ref":"acc_2"}`, "impl"), // resumed the same session, later report
		run("run_3", "", `not json`, "rev"),
		run("run_4", "", `{}`, "rev"),
		usage("run_1", `[{"model":"m","input_tokens":10,"requests":1}]`, `{"usd":0.5}`, 1),
		usage("run_2", `[{"model":"m","input_tokens":30,"output_tokens":3,"cache_read_tokens":7,"requests":4}]`, `{"usd":1.5}`, 2),
		usage("run_3", `not json`, ``, 3),
		`INSERT INTO run_usage (run_id, space_id, source, final, updated_at) VALUES ('run_4', 'spc_a', 'unavailable', 1, 4)`,
	} {
		if _, err := s.wdb.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	_ = s.Close()

	s, err = Open(ctx, Options{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	type cols struct{ account, role, harness string }
	want := map[string]cols{"run_1": {"acc_1", "impl", "claude-code"}, "run_2": {"acc_2", "impl", "claude-code"}, "run_3": {"", "rev", "claude-code"}, "run_4": {"", "rev", "claude-code"}}
	for id, w := range want {
		var got cols
		if err := s.rdb.QueryRowContext(ctx, `SELECT account_id, role, harness FROM run_usage WHERE run_id = ?`, id).Scan(&got.account, &got.role, &got.harness); err != nil || got != w {
			t.Errorf("%s: %+v %v, want %+v", id, got, err, w)
		}
	}
	var n int
	if err := s.rdb.QueryRowContext(ctx, `SELECT count(*) FROM usage_ledger_marks`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("marks %d %v, want one for the session and one for run_3 (run_4's usage is unavailable)", n, err)
	}
	var counted string
	var cost float64
	if err := s.rdb.QueryRowContext(ctx, `SELECT counted_json, counted_cost FROM usage_ledger_marks WHERE mark_key = 'sess-1'`).Scan(&counted, &cost); err != nil || cost != 1.5 {
		t.Fatalf("session mark %q %v %v", counted, cost, err)
	}
	var in, out, cr, req int64
	if err := s.rdb.QueryRowContext(ctx, `SELECT json_extract(counted_json, '$[0].in'), json_extract(counted_json, '$[0].out'), json_extract(counted_json, '$[0].cr'), json_extract(counted_json, '$[0].req')
		FROM usage_ledger_marks WHERE mark_key = 'sess-1'`).Scan(&in, &out, &cr, &req); err != nil || in != 30 || out != 3 || cr != 7 || req != 4 {
		t.Fatalf("session counters %d %d %d %d %v from %s", in, out, cr, req, err, counted)
	}
	if err := s.rdb.QueryRowContext(ctx, `SELECT counted_json FROM usage_ledger_marks WHERE mark_key = 'run:run_3'`).Scan(&counted); err != nil || counted != "[]" {
		t.Fatalf("a run with unreadable usage: %q %v", counted, err)
	}
	for _, q := range []string{
		`INSERT INTO usage_buckets (space_id, account_id, role, harness, model, granularity, bucket_start) VALUES ('spc_a', '', 'r', 'claude-code', 'm', 'week', 0)`,
		`INSERT INTO account_windows (account_id, kind, used_pct, observed_at, updated_at) VALUES ('acc_x', 'five_hour', 1, 0, 0)`, // no such account
	} {
		if _, err := s.wdb.ExecContext(ctx, q); err == nil {
			t.Errorf("accepted: %s", q)
		}
	}
	if _, err := s.wdb.ExecContext(ctx, `INSERT INTO usage_buckets (space_id, account_id, role, harness, model, granularity, bucket_start) VALUES ('spc_a', '', 'r', 'claude-code', 'm', 'hour', 0)`); err != nil {
		t.Fatal(err)
	}
}
