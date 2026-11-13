package store

import (
	"context"
	"path/filepath"
	"testing"
)

// 0048 (ADR-0077) adds a harness profile's version pin and a run's launched harness version: existing profiles follow
// the node's channel (”) and existing runs report none (”).
func TestHarnessVersionMigration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "data")
	s, err := Open(ctx, Options{DataDir: dir, Migrations: embeddedUpTo(t, "0047")})
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO harness_profiles (id, slug, name, harness, created_at, created_by, updated_at) VALUES ('hpf_1', 'claude', 'Claude', 'claude-code', 0, 't', 0)`,
		`INSERT INTO nodes (id, name, state, created_at, created_by, updated_at) VALUES ('nod_1', 'n1', 'active', 0, 't', 0)`,
		`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('spc_a', 'aaa', 'A', 'nod_1', 'active', 'a', 0, 't', 0)`,
		`INSERT INTO runs (id, space_id, node_id, generation, kind, role, harness, instance_id, desired, observed, launch_json, created_at, created_by, updated_at)
			VALUES ('run_old', 'spc_a', 'nod_1', 1, 'worker', 'w', 'claude-code', 'ins_1', 'running', 'running', '{}', 0, 't', 0)`,
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
	var pin string
	if err := s.rdb.QueryRowContext(ctx, `SELECT harness_version FROM harness_profiles WHERE id = 'hpf_1'`).Scan(&pin); err != nil || pin != "" {
		t.Fatalf("existing profile: %q %v", pin, err)
	}
	var v, req, fb string
	if err := s.rdb.QueryRowContext(ctx, `SELECT harness_version, harness_requested, harness_fallback FROM runs WHERE id = 'run_old'`).Scan(&v, &req, &fb); err != nil ||
		v != "" || req != "" || fb != "" {
		t.Fatalf("existing run: %q %q %q %v", v, req, fb, err)
	}
	// NOT NULL: a NULL is refused rather than stored as "no pin".
	if _, err := s.wdb.ExecContext(ctx, `UPDATE harness_profiles SET harness_version = NULL WHERE id = 'hpf_1'`); err == nil {
		t.Error("NULL pin stored")
	}
}
