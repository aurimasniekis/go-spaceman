package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// 0050 (ADR-0079) adds a run's usage limit and the usage_limits and limit_waits tables: existing runs have no limit,
// the CHECKs keep kinds, scopes and states on their allowlists, an account window has one open row per space, an
// instance one active wait, and purging a space's runs and the space removes its rows.
func TestUsageLimitsMigration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "data")
	s, err := Open(ctx, Options{DataDir: dir, Migrations: embeddedUpTo(t, "0049")})
	if err != nil {
		t.Fatal(err)
	}
	exec := func(s *Store, q string, args ...any) error { _, err := s.wdb.ExecContext(ctx, q, args...); return err }
	for _, q := range []string{
		`INSERT INTO nodes (id, name, state, created_at, created_by, updated_at) VALUES ('nod_1', 'n1', 'active', 0, 't', 0)`,
		`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('spc_a', 'aaa', 'A', 'nod_1', 'active', 'a', 0, 't', 0)`,
		`INSERT INTO runs (id, space_id, node_id, generation, kind, role, harness, instance_id, desired, observed, launch_json, created_at, created_by, updated_at)
			VALUES ('run_old', 'spc_a', 'nod_1', 1, 'worker', 'w', 'claude-code', 'ins_1', 'running', 'running', '{}', 0, 't', 0)`,
	} {
		if err := exec(s, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	_ = s.Close()

	s, err = Open(ctx, Options{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var kind string
	var resets, since sql.NullInt64
	if err := s.rdb.QueryRowContext(ctx, `SELECT limit_kind, limit_resets_at, limit_since FROM runs WHERE id = 'run_old'`).Scan(&kind, &resets, &since); err != nil ||
		kind != "" || resets.Valid || since.Valid {
		t.Fatalf("existing run: %q %v %v %v", kind, resets, since, err)
	}

	// The run's limit, through the generated query: fenced by generation, and a repeat changes nothing.
	q := db.New(s.wdb)
	set := db.SetRunLimitParams{LimitKind: "five_hour", LimitResetsAt: sql.NullInt64{Int64: 2_000, Valid: true}, LimitSince: sql.NullInt64{Int64: 1_000, Valid: true},
		Now: 1_000, ID: "run_old", Generation: 1}
	for i, want := range []int64{1, 0} {
		if n, err := q.SetRunLimit(ctx, set); err != nil || n != want {
			t.Fatalf("set #%d: %d %v", i, n, err)
		}
	}
	stale := set
	stale.Generation, stale.LimitKind = 2, "seven_day"
	if n, err := q.SetRunLimit(ctx, stale); err != nil || n != 0 {
		t.Fatalf("stale generation: %d %v", n, err)
	}
	if n, err := q.SetRunLimit(ctx, db.SetRunLimitParams{Now: 2_000, ID: "run_old", Generation: 1}); err != nil || n != 1 {
		t.Fatalf("clear: %d %v", n, err)
	}
	if exec(s, `UPDATE runs SET limit_kind = NULL WHERE id = 'run_old'`) == nil {
		t.Error("NULL limit_kind accepted")
	}
	// runs.limit_kind has no CHECK, so a kind added later needs no runs-table rebuild (usage_limits.kind has one).
	if err := exec(s, `UPDATE runs SET limit_kind = 'some_new_kind' WHERE id = 'run_old'`); err != nil {
		t.Errorf("runs.limit_kind refuses a new kind: %v", err)
	}

	ul := db.InsertUsageLimitParams{ID: "ulm_1", SpaceID: "spc_a", Scope: "account", AccountID: "acc_1", Kind: "five_hour", Source: "transcript",
		FirstRunID: "run_old", SeenAt: 1_000, CreatedAt: 1_000, UpdatedAt: 1_000}
	if err := q.InsertUsageLimit(ctx, ul); err != nil {
		t.Fatal(err)
	}
	dup := ul
	dup.ID = "ulm_2"
	if q.InsertUsageLimit(ctx, dup) == nil {
		t.Fatal("second open row for the same account window")
	}
	for name, mut := range map[string]func(*db.InsertUsageLimitParams){
		"scope":            func(p *db.InsertUsageLimitParams) { p.Scope = "node" },
		"kind":             func(p *db.InsertUsageLimitParams) { p.Kind = "" },
		"source":           func(p *db.InsertUsageLimitParams) { p.Source = "screen" },
		"account no id":    func(p *db.InsertUsageLimitParams) { p.AccountID = "" },
		"account with ref": func(p *db.InsertUsageLimitParams) { p.GatewayRef = "hp/p" },
		"gateway with acc": func(p *db.InsertUsageLimitParams) { p.Scope = "gateway"; p.GatewayRef = "hp/p" },
	} {
		p := ul
		p.ID, p.Kind = "ulm_x", "seven_day"
		mut(&p)
		if q.InsertUsageLimit(ctx, p) == nil {
			t.Errorf("%s: accepted %+v", name, p)
		}
	}
	gw := ul
	gw.ID, gw.Scope, gw.AccountID, gw.GatewayRef, gw.Kind = "ulm_g", "gateway", "", "hpc_1/proj", "gateway_quota"
	gw.CheckAt = sql.NullInt64{Int64: 1_900, Valid: true}
	if err := q.InsertUsageLimit(ctx, gw); err != nil {
		t.Fatal(err)
	}
	// Every kind on the Go allowlist passes the CHECK (here seven_day_overage_included, with a known reset).
	ov := ul
	ov.ID, ov.Kind, ov.ResetsAt = "ulm_ov", "seven_day_overage_included", sql.NullInt64{Int64: 9_000, Valid: true}
	if err := q.InsertUsageLimit(ctx, ov); err != nil {
		t.Fatal(err)
	}
	if at, err := q.NextUsageLimitDue(ctx); err != nil || at != 9_000 {
		t.Fatalf("next due: %d %v", at, err)
	}
	if at, err := q.NextUsageLimitCheck(ctx); err != nil || at != 1_900 {
		t.Fatalf("next check: %d %v", at, err)
	}
	if rows, err := q.ListUsageLimitsToCheck(ctx, sql.NullInt64{Int64: 2_000, Valid: true}); err != nil || len(rows) != 1 || rows[0].ID != "ulm_g" {
		t.Fatalf("to check: %+v %v", rows, err)
	}
	if n, err := q.SetUsageLimitCheck(ctx, db.SetUsageLimitCheckParams{CheckAt: sql.NullInt64{Int64: 2_900, Valid: true}, Now: 2_000, ID: "ulm_g", Version: 1}); err != nil || n != 1 {
		t.Fatalf("set check: %d %v", n, err)
	}
	if rows, err := q.ListOpenUsageLimitsByAccount(ctx, "acc_1"); err != nil || len(rows) != 2 || rows[0].ID != "ulm_1" || rows[1].ID != "ulm_ov" {
		t.Fatalf("open by account: %+v %v", rows, err)
	}
	if n, err := q.ClearUsageLimit(ctx, db.ClearUsageLimitParams{Now: 3_000, ClearedBy: "reset", ID: "ulm_1"}); err != nil || n != 1 {
		t.Fatalf("clear: %d %v", n, err)
	}
	if err := q.InsertUsageLimit(ctx, dup); err != nil {
		t.Fatalf("a new window after the first cleared: %v", err)
	}

	// An account-level clear lifts the hold for that account and kind in every space, nothing else.
	if err := exec(s, `INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('spc_b', 'bbb', 'B', 'nod_1', 'active', 'b', 0, 't', 0)`); err != nil {
		t.Fatal(err)
	}
	other := ul
	other.ID, other.SpaceID = "ulm_b", "spc_b"
	if err := q.InsertUsageLimit(ctx, other); err != nil {
		t.Fatal(err)
	}
	if n, err := q.ClearUsageLimitsByAccount(ctx, db.ClearUsageLimitsByAccountParams{Now: 4_000, ClearedBy: "operator", AccountID: "acc_1", Kind: "five_hour"}); err != nil || n != 2 {
		t.Fatalf("clear by account: %d %v", n, err)
	}
	if rows, err := q.ListOpenUsageLimitsByAccount(ctx, "acc_1"); err != nil || len(rows) != 1 || rows[0].ID != "ulm_ov" {
		t.Fatalf("open after clear by account: %+v %v", rows, err)
	}

	w := db.InsertLimitWaitParams{ID: "lwt_1", SpaceID: "spc_a", RunID: "run_old", RunKind: "worker", Role: "w", NodeID: "nod_1", InstanceID: "ins_1",
		Harness: "claude-code", LaunchJson: "{}", UsageLimitID: "ulm_2", ResumeAt: 5_000, CreatedAt: 1_000, UpdatedAt: 1_000}
	if err := q.InsertLimitWait(ctx, w); err != nil {
		t.Fatal(err)
	}
	if got, err := q.GetLimitWait(ctx, "lwt_1"); err != nil || got.State != "waiting" || got.Attempts != 0 || got.ReleaseAt.Valid {
		t.Fatalf("defaults: %+v %v", got, err)
	}
	if err := exec(s, `INSERT INTO runs (id, space_id, node_id, generation, kind, role, harness, instance_id, desired, observed, launch_json, created_at, created_by, updated_at)
			VALUES ('run_2', 'spc_a', 'nod_1', 1, 'worker', 'w', 'claude-code', 'ins_1', 'stopped', 'exited', '{}', 0, 't', 0)`); err != nil {
		t.Fatal(err)
	}
	second := w
	second.ID, second.RunID = "lwt_2", "run_2"
	if q.InsertLimitWait(ctx, second) == nil {
		t.Fatal("second active wait for the same instance")
	}
	dupRun := w
	dupRun.ID, dupRun.InstanceID = "lwt_3", "ins_2"
	if q.InsertLimitWait(ctx, dupRun) == nil {
		t.Fatal("second wait for the same run")
	}
	if due, err := q.ListDueLimitWaits(ctx, db.ListDueLimitWaitsParams{ResumeAt: 5_000, Limit: 10}); err != nil || len(due) != 1 {
		t.Fatalf("due: %+v %v", due, err)
	}
	if n, err := q.UpdateLimitWait(ctx, db.UpdateLimitWaitParams{State: "resumed", ResumeAt: 5_000, NewRunID: "run_3", Attempts: 1, Now: 5_000, ID: "lwt_1", Version: 1}); err != nil || n != 1 {
		t.Fatalf("update: %d %v", n, err)
	}
	if err := q.InsertLimitWait(ctx, second); err != nil {
		t.Fatalf("a new wait once the first resumed: %v", err)
	}
	if exec(s, `UPDATE limit_waits SET state = 'parked' WHERE id = 'lwt_2'`) == nil {
		t.Error("unknown wait state accepted")
	}

	// Purging a space (fleet.purge_space) deletes its runs, then the space: the waits and limits go with them.
	for _, q := range []string{`DELETE FROM node_commands WHERE space_id = 'spc_a'`, `DELETE FROM runs WHERE space_id = 'spc_a'`, `DELETE FROM spaces WHERE id = 'spc_a'`} {
		if err := exec(s, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	for _, tbl := range []string{"usage_limits", "limit_waits"} {
		var n int
		if err := s.rdb.QueryRowContext(ctx, `SELECT count(*) FROM `+tbl+` WHERE space_id = 'spc_a'`).Scan(&n); err != nil || n != 0 {
			t.Errorf("%s after purge: %d %v", tbl, n, err)
		}
	}
}
