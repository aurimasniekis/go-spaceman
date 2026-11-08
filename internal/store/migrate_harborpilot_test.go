package store

import (
	"context"
	"path/filepath"
	"testing"
)

// 0040 adds the Harbor Pilot tables to a database at 0039 without touching existing rows, and its constraints hold:
// one connection per central, a token lifetime of 15 min to 7 days, a project for an enabled space, the budget
// fallback's two values, space settings deleted with their space, and run tokens deleted with their run while the
// revocation queue (no foreign key) survives.
func TestHarborPilotMigrationFrom0039(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "data")
	s, err := Open(ctx, Options{DataDir: dir, Migrations: embeddedUpTo(t, "0039")})
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO nodes (id, name, state, created_at, created_by, updated_at) VALUES ('nod_1', 'n1', 'active', 0, 't', 0)`,
		`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('spc_a', 'aaa', 'A', 'nod_1', 'active', 'a', 0, 't', 0)`,
	} {
		if _, err := s.wdb.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	var n int
	if err := s.rdb.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE name = 'harbor_pilot_connection'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("0039 already has the table: %d %v", n, err)
	}
	_ = s.Close()

	s, err = Open(ctx, Options{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.rdb.QueryRowContext(ctx, `SELECT count(*) FROM spaces WHERE id = 'spc_a'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("space after migration: %d %v", n, err)
	}
	conn := `INSERT INTO harbor_pilot_connection (id, base_url, parent_token_enc, validated_at, created_at, created_by, updated_at, updated_by)
		VALUES (?, 'https://hp.example.com', x'00', 0, 0, 't', 0, 't')`
	if _, err := s.wdb.ExecContext(ctx, conn, "hpc_1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.wdb.ExecContext(ctx, conn, "hpc_2"); err == nil {
		t.Fatal("a second connection was accepted")
	}
	space := `INSERT INTO space_harbor_pilot (space_id, enabled, project_id, ttl_seconds, on_budget_status_unavailable, created_at, created_by, updated_at, updated_by)
		VALUES ('spc_a', ?, ?, ?, ?, 0, 't', 0, 't')`
	for _, bad := range [][]any{{1, "", 86400, "proceed"}, {0, "", 899, "proceed"}, {0, "", 604801, "proceed"}, {0, "", 86400, "fallback"}} {
		if _, err := s.wdb.ExecContext(ctx, space, bad...); err == nil {
			t.Fatalf("space settings %v were accepted", bad)
		}
	}
	if _, err := s.wdb.ExecContext(ctx, space, 1, "prj_1", 900, "refuse"); err != nil {
		t.Fatal(err)
	}
	var ttl int
	var harnesses, policy string
	if err := s.rdb.QueryRowContext(ctx, `SELECT ttl_seconds, harnesses_json, on_budget_status_unavailable FROM space_harbor_pilot`).Scan(&ttl, &harnesses, &policy); err != nil ||
		ttl != 900 || harnesses != "[]" || policy != "refuse" {
		t.Fatalf("stored settings %d %q %q %v", ttl, harnesses, policy, err)
	}
	// A run's token row goes with its run.
	for _, q := range []string{
		`INSERT INTO runs (id, space_id, node_id, generation, kind, role, harness, instance_id, desired, observed, status, launch_json, created_at, created_by, updated_at)
		 VALUES ('run_1', 'spc_a', 'nod_1', 1, 'worker', 'implementor', 'codex', 'ins_1', 'running', 'running', 'idle', '{}', 0, 't', 0)`,
		`INSERT INTO run_gateway_credentials (run_id, space_id, credential_id, token_enc, expires_at, created_at) VALUES ('run_1', 'spc_a', 'crd_run', x'00', 1, 0)`,
		`DELETE FROM runs WHERE id = 'run_1'`,
	} {
		if _, err := s.wdb.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := s.rdb.QueryRowContext(ctx, `SELECT count(*) FROM run_gateway_credentials`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("run token rows after deleting the run: %d %v", n, err)
	}
	for _, q := range []string{
		`INSERT INTO gateway_revocations (credential_id, run_id, space_id, reason, expires_at, next_attempt_at, requested_at) VALUES ('crd_1', 'run_x', 'spc_a', 'run ended', 1, 1, 1)`,
		`DELETE FROM spaces WHERE id = 'spc_a'`,
	} {
		if _, err := s.wdb.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := s.rdb.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM space_harbor_pilot) * 10 + (SELECT count(*) FROM gateway_revocations)`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("after deleting the space: %d %v (want settings gone, revocation kept)", n, err)
	}
	if _, err := s.wdb.ExecContext(ctx, `INSERT INTO gateway_revocations (credential_id, reason, expires_at, next_attempt_at, requested_at, state) VALUES ('crd_2', 'x', 1, 1, 1, 'lost')`); err == nil {
		t.Fatal("an unknown revocation state was accepted")
	}
}
