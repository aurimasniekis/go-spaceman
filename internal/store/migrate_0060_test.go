package store

import (
	"context"
	"path/filepath"
	"testing"
)

// 0060 adds memory_conflicts.folder: a database at 0059 with a conflict keeps it, and the row gets ” (no folder is
// known for a conflict recorded before); a fresh database has the column and a default of ”.
func TestMemoryConflictFolderMigrationFrom0059(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "data")
	s, err := Open(ctx, Options{DataDir: dir, Migrations: embeddedUpTo(t, "0059")})
	if err != nil {
		t.Fatal(err)
	}
	if hasColumn(ctx, t, s, "memory_conflicts", "folder") {
		t.Fatal("0059 already has memory_conflicts.folder")
	}
	for _, q := range []string{
		`INSERT INTO nodes (id, name, state, created_at, created_by, updated_at) VALUES ('nod_1', 'n1', 'active', 0, 't', 0)`,
		`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('spc_a', 'aaa', 'A', 'nod_1', 'active', 'a', 0, 't', 0)`,
		`INSERT INTO memory_collections (id, space_id, role, created_at, updated_at) VALUES ('mem_1', 'spc_a', 'reviewer', 0, 0)`,
		`INSERT INTO memory_conflicts (id, collection_id, source, ref, created_at) VALUES ('mcf_1', 'mem_1', 'node', 'refs/conflicts/x', 5)`,
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
	if err != nil || cur != latest || latest < 60 {
		t.Fatalf("after the upgrade: version %d of %d, %v; want all applied, at least 0060", cur, latest, err)
	}
	if !hasColumn(ctx, t, s, "memory_conflicts", "folder") {
		t.Fatal("memory_conflicts.folder missing after the upgrade")
	}
	var folder, ref string
	if err := s.rdb.QueryRowContext(ctx, `SELECT folder, ref FROM memory_conflicts WHERE id = 'mcf_1'`).Scan(&folder, &ref); err != nil {
		t.Fatal(err)
	}
	if folder != "" || ref != "refs/conflicts/x" {
		t.Errorf("existing conflict: folder %q ref %q, want '' and its ref kept", folder, ref)
	}

	fresh, err := Open(ctx, Options{DataDir: filepath.Join(t.TempDir(), "fresh")})
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	cur, latest, err = SchemaVersions(ctx, fresh.rdb, EmbeddedMigrations())
	if err != nil || cur != latest || cur < 60 {
		t.Fatalf("fresh database: version %d of %d, %v", cur, latest, err)
	}
	if !hasColumn(ctx, t, fresh, "memory_conflicts", "folder") {
		t.Fatal("a fresh database lacks memory_conflicts.folder")
	}
}
