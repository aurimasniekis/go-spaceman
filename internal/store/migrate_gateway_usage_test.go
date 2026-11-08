package store

import (
	"context"
	"path/filepath"
	"testing"
)

// 0043 adds the Harbor Pilot usage read-back table to a database at 0041 without touching existing rows; a row is
// deleted with its run, and its state is one of the four.
func TestGatewayUsageMigrationFrom0041(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "data")
	s, err := Open(ctx, Options{DataDir: dir, Migrations: embeddedUpTo(t, "0041")})
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO nodes (id, name, state, created_at, created_by, updated_at) VALUES ('nod_1', 'n1', 'active', 0, 't', 0)`,
		`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('spc_a', 'aaa', 'A', 'nod_1', 'active', 'a', 0, 't', 0)`,
		`INSERT INTO runs (id, space_id, node_id, generation, kind, role, harness, instance_id, desired, observed, launch_json, created_at, created_by, updated_at)
			VALUES ('run_1', 'spc_a', 'nod_1', 1, 'worker', 'w', 'codex', 'ins_1', 'running', 'running', '{}', 0, 't', 0)`,
		`INSERT INTO run_usage (run_id, space_id, source, final, updated_at) VALUES ('run_1', 'spc_a', 'unavailable', 1, 0)`,
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
	var n int
	if err := s.rdb.QueryRowContext(ctx, `SELECT count(*) FROM run_usage WHERE run_id = 'run_1' AND source = 'unavailable'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("usage after migration: %d %v", n, err)
	}
	if _, err := s.wdb.ExecContext(ctx, `INSERT INTO run_gateway_usage (run_id, space_id, state, ended_at, next_attempt_at, updated_at) VALUES ('run_1', 'spc_a', 'other', 0, 0, 0)`); err == nil {
		t.Fatal("an unknown state was accepted")
	}
	if _, err := s.wdb.ExecContext(ctx, `INSERT INTO run_gateway_usage (run_id, space_id, ended_at, next_attempt_at, updated_at) VALUES ('run_1', 'spc_a', 0, 0, 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.wdb.ExecContext(ctx, `DELETE FROM runs WHERE id = 'run_1'`); err != nil {
		t.Fatal(err)
	}
	if err := s.rdb.QueryRowContext(ctx, `SELECT count(*) FROM run_gateway_usage`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("read-back rows after the run was deleted: %d %v", n, err)
	}
}
