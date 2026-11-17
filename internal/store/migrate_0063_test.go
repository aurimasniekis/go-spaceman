package store

import (
	"context"
	"path/filepath"
	"testing"
)

// 0063 widens space_role_renames.node_state with abandoned (ADR-0093 addendum, T-H4): a database at 0062 upgrades and
// keeps its renames with every column, and the rebuilt table still refuses an unknown state and a rename to the same
// name, and still goes with its space.
func TestRoleRenameAbandonedMigrationFrom0062(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "data")
	s, err := Open(ctx, Options{DataDir: dir, Migrations: embeddedUpTo(t, "0062")})
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO nodes (id, name, state, created_at, created_by, updated_at) VALUES ('nod_1', 'n1', 'active', 0, 't', 0)`,
		`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('spc_a', 'aaa', 'A', 'nod_1', 'active', 'a', 0, 't', 0)`,
		`INSERT INTO space_role_renames (id, space_id, from_name, to_name, renamed_at, renamed_by, node_id, node_state, node_message, node_done_at, central_moved_at)
		 VALUES ('rrn_1', 'spc_a', 'a', 'b', 5, 'usr_1', 'nod_1', 'done', 'moved', 7, 6)`,
		`INSERT INTO space_role_renames (id, space_id, from_name, to_name, renamed_at, renamed_by, node_id, node_state) VALUES ('rrn_2', 'spc_a', 'b', 'c', 8, 'usr_1', 'nod_1', 'pending')`,
	} {
		if _, err := s.wdb.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if _, err := s.wdb.ExecContext(ctx, `UPDATE space_role_renames SET node_state = 'abandoned' WHERE id = 'rrn_2'`); err == nil {
		t.Fatal("0062 already accepts abandoned")
	}
	_ = s.Close()

	s, err = Open(ctx, Options{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	cur, latest, err := SchemaVersions(ctx, s.rdb, EmbeddedMigrations())
	if err != nil || cur != latest || latest < 63 {
		t.Fatalf("after the upgrade: version %d of %d, %v; want all applied, at least 0063", cur, latest, err)
	}
	var (
		from, to, by, node, state, msg string
		at, doneAt, movedAt            int64
	)
	if err := s.rdb.QueryRowContext(ctx, `SELECT from_name, to_name, renamed_at, renamed_by, node_id, node_state, node_message, node_done_at, central_moved_at
		FROM space_role_renames WHERE id = 'rrn_1'`).Scan(&from, &to, &at, &by, &node, &state, &msg, &doneAt, &movedAt); err != nil {
		t.Fatal(err)
	}
	if from != "a" || to != "b" || at != 5 || by != "usr_1" || node != "nod_1" || state != "done" || msg != "moved" || doneAt != 7 || movedAt != 6 {
		t.Fatalf("rrn_1 after the upgrade: %s %s %d %s %s %s %q %d %d", from, to, at, by, node, state, msg, doneAt, movedAt)
	}
	if _, err := s.wdb.ExecContext(ctx, `UPDATE space_role_renames SET node_state = 'abandoned', node_message = 'permission denied' WHERE id = 'rrn_2'`); err != nil {
		t.Fatalf("abandoned refused: %v", err)
	}
	if _, err := s.wdb.ExecContext(ctx, `UPDATE space_role_renames SET node_state = 'moving' WHERE id = 'rrn_2'`); err == nil {
		t.Error("an unknown node state was accepted")
	}
	if _, err := s.wdb.ExecContext(ctx, `INSERT INTO space_role_renames (id, space_id, from_name, to_name, renamed_at, renamed_by, node_state)
		VALUES ('rrn_3', 'spc_a', 'x', 'x', 0, 't', 'pending')`); err == nil {
		t.Error("a rename to the same name was accepted")
	}
	var n int
	for _, ix := range []string{"ix_space_role_renames_from", "ix_space_role_renames_to", "ix_space_role_renames_open", "ix_space_role_renames_node",
		"ix_space_role_renames_unmoved"} {
		if err := s.rdb.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'index' AND name = ?`, ix).Scan(&n); err != nil || n != 1 {
			t.Errorf("index %s: %d %v", ix, n, err)
		}
	}
	if _, err := s.wdb.ExecContext(ctx, `DELETE FROM spaces WHERE id = 'spc_a'`); err != nil {
		t.Fatal(err)
	}
	if err := s.rdb.QueryRowContext(ctx, `SELECT count(*) FROM space_role_renames`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("renames left after the space was deleted: %d, %v", n, err)
	}
}
