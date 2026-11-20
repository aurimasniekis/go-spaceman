package store

import (
	"context"
	"path/filepath"
	"testing"
)

// 0071 rebuilds account_switches for switching back (ADR-0105): a database at 0069 upgrades and keeps its switches with
// every column, a usage-limit switch gets its from account as its origin and any other cause none, the cause
// switch_back is accepted, an unknown cause still refused, and the switches still go with their space.
func TestAccountSwitchBackMigrationFrom0069(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "data")
	s, err := Open(ctx, Options{DataDir: dir, Migrations: embeddedUpTo(t, "0069")})
	if err != nil {
		t.Fatal(err)
	}
	ins := `INSERT INTO account_switches (id, space_id, instance_id, role, run_kind, harness, from_run_id, new_run_id, from_account_id, to_account_id, cause,
		limit_kind, limit_resets_at, limit_since, usage_limit_id, limit_wait_id, state, reason, armed_until, version, created_at, updated_at)
		VALUES (?, 'spc_a', 'ins_1', 'r', 'worker', 'claude-code', 'run_1', 'run_2', 'acc_1', 'acc_2', ?, 'five_hour', 9, 8, 'ulm_1', 'lwt_1', 'succeeded',
		'', 7, 3, 5, 6)`
	for _, q := range []string{
		`INSERT INTO nodes (id, name, state, created_at, created_by, updated_at) VALUES ('nod_1', 'n1', 'active', 0, 't', 0)`,
		`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('spc_a', 'aaa', 'A', 'nod_1', 'active', 'a', 0, 't', 0)`,
	} {
		if _, err := s.wdb.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	for id, cause := range map[string]string{"asw_1": "usage_limit", "asw_2": "operator"} {
		if _, err := s.wdb.ExecContext(ctx, ins, id, cause); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.wdb.ExecContext(ctx, ins, "asw_3", "switch_back"); err == nil {
		t.Fatal("0069 already accepts switch_back")
	}
	_ = s.Close()

	s, err = Open(ctx, Options{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	cur, latest, err := SchemaVersions(ctx, s.rdb, EmbeddedMigrations())
	if err != nil || cur != latest || latest < 71 {
		t.Fatalf("after the upgrade: version %d of %d, %v; want all applied, at least 0071", cur, latest, err)
	}
	var (
		newRun, from, to, cause, kind, ulm, lwt, state, origin string
		resets, since, armed, version, created, updated        int64
	)
	if err := s.rdb.QueryRowContext(ctx, `SELECT new_run_id, from_account_id, to_account_id, cause, limit_kind, limit_resets_at, limit_since, usage_limit_id,
		limit_wait_id, state, armed_until, version, created_at, updated_at, origin_account_id FROM account_switches WHERE id = 'asw_1'`).Scan(&newRun, &from, &to,
		&cause, &kind, &resets, &since, &ulm, &lwt, &state, &armed, &version, &created, &updated, &origin); err != nil {
		t.Fatal(err)
	}
	if newRun != "run_2" || from != "acc_1" || to != "acc_2" || cause != "usage_limit" || kind != "five_hour" || resets != 9 || since != 8 || ulm != "ulm_1" ||
		lwt != "lwt_1" || state != "succeeded" || armed != 7 || version != 3 || created != 5 || updated != 6 || origin != "acc_1" {
		t.Fatalf("asw_1 after the upgrade: %s %s %s %s %s %d %d %s %s %s %d %d %d %d %q", newRun, from, to, cause, kind, resets, since, ulm, lwt, state,
			armed, version, created, updated, origin)
	}
	if err := s.rdb.QueryRowContext(ctx, `SELECT origin_account_id FROM account_switches WHERE id = 'asw_2'`).Scan(&origin); err != nil || origin != "" {
		t.Fatalf("operator switch origin %q, %v", origin, err)
	}
	if _, err := s.wdb.ExecContext(ctx, ins, "asw_3", "switch_back"); err != nil {
		t.Fatalf("switch_back refused: %v", err)
	}
	if _, err := s.wdb.ExecContext(ctx, ins, "asw_4", "moon_phase"); err == nil {
		t.Error("an unknown cause was accepted")
	}
	if _, err := s.wdb.ExecContext(ctx, `UPDATE account_switches SET state = 'pending' WHERE id IN ('asw_1', 'asw_2')`); err == nil {
		t.Error("two active switches of one instance were accepted")
	}
	var n int
	for _, ix := range []string{"ux_account_switches_active", "ix_account_switches_space", "ix_account_switches_instance", "ix_account_switches_from_run",
		"ix_account_switches_new_run", "ix_account_switches_to_account", "ix_account_switches_armed", "ix_account_switches_finished"} {
		if err := s.rdb.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'index' AND name = ?`, ix).Scan(&n); err != nil || n != 1 {
			t.Errorf("index %s: %d %v", ix, n, err)
		}
	}
	if _, err := s.wdb.ExecContext(ctx, `DELETE FROM spaces WHERE id = 'spc_a'`); err != nil {
		t.Fatal(err)
	}
	if err := s.rdb.QueryRowContext(ctx, `SELECT count(*) FROM account_switches`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("switches left after the space was deleted: %d, %v", n, err)
	}
}
