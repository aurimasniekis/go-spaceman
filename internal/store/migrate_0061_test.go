package store

import (
	"context"
	"path/filepath"
	"testing"
)

// 0061 adds space_role_renames (ADR-0093): a database at 0060 upgrades and keeps its roles; the table takes a pending
// rename, refuses an unknown node state and a rename to the same name, and goes with its space.
func TestSpaceRoleRenamesMigrationFrom0060(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "data")
	s, err := Open(ctx, Options{DataDir: dir, Migrations: embeddedUpTo(t, "0060")})
	if err != nil {
		t.Fatal(err)
	}
	if hasColumn(ctx, t, s, "space_role_renames", "id") {
		t.Fatal("0060 already has space_role_renames")
	}
	for _, q := range []string{
		`INSERT INTO nodes (id, name, state, created_at, created_by, updated_at) VALUES ('nod_1', 'n1', 'active', 0, 't', 0)`,
		`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('spc_a', 'aaa', 'A', 'nod_1', 'active', 'a', 0, 't', 0)`,
		`INSERT INTO role_templates (id, slug, name, kind, created_at, created_by, updated_at) VALUES ('rtp_1', 'w', 'W', 'worker', 0, 't', 0)`,
		`INSERT INTO space_roles (space_id, name, template_id, created_at, created_by, updated_at) VALUES ('spc_a', 'reviewer', 'rtp_1', 0, 't', 0)`,
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
	cur, latest, err := SchemaVersions(ctx, s.rdb, EmbeddedMigrations())
	if err != nil || cur != latest || latest < 61 {
		t.Fatalf("after the upgrade: version %d of %d, %v; want all applied, at least 0061", cur, latest, err)
	}
	var n int
	if err := s.rdb.QueryRowContext(ctx, `SELECT count(*) FROM space_roles WHERE space_id = 'spc_a' AND name = 'reviewer'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("the role did not survive the upgrade: %d, %v", n, err)
	}
	ins := `INSERT INTO space_role_renames (id, space_id, from_name, to_name, renamed_at, renamed_by, node_id, node_state) VALUES (?, 'spc_a', ?, ?, 0, 't', 'nod_1', ?)`
	if _, err := s.wdb.ExecContext(ctx, ins, "rrn_1", "reviewer", "critic", "pending"); err != nil {
		t.Fatalf("a pending rename: %v", err)
	}
	if _, err := s.wdb.ExecContext(ctx, ins, "rrn_2", "a", "b", "moving"); err == nil {
		t.Error("an unknown node state was accepted")
	}
	if _, err := s.wdb.ExecContext(ctx, ins, "rrn_3", "a", "a", "done"); err == nil {
		t.Error("a rename to the same name was accepted")
	}
	var msg string
	if err := s.rdb.QueryRowContext(ctx, `SELECT node_message FROM space_role_renames WHERE id = 'rrn_1' AND central_moved_at IS NULL AND node_done_at IS NULL`).Scan(&msg); err != nil || msg != "" {
		t.Fatalf("defaults: %q, %v", msg, err)
	}
	if _, err := s.wdb.ExecContext(ctx, `DELETE FROM spaces WHERE id = 'spc_a'`); err != nil {
		t.Fatal(err)
	}
	if err := s.rdb.QueryRowContext(ctx, `SELECT count(*) FROM space_role_renames`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("renames left after the space was deleted: %d, %v", n, err)
	}
}
