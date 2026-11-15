package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// SM-HARN-008: migration 0049 adds nodes.capabilities_updated_at; an existing node reported its capabilities in its
// last hello, so it takes last_connect_at (NULL for a node that never connected).
func TestNodeCapabilitiesUpdatedMigration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "data")
	s, err := Open(ctx, Options{DataDir: dir, Migrations: embeddedUpTo(t, "0048")})
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO nodes (id, name, state, created_at, created_by, updated_at, last_connect_at) VALUES ('nod_1', 'n1', 'active', 0, 't', 0, 1700000000000000)`,
		`INSERT INTO nodes (id, name, state, created_at, created_by, updated_at) VALUES ('nod_2', 'n2', 'pending', 0, 't', 0)`,
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
	for id, want := range map[string]sql.NullInt64{"nod_1": {Int64: 1700000000000000, Valid: true}, "nod_2": {}} {
		var got sql.NullInt64
		if err := s.rdb.QueryRowContext(ctx, `SELECT capabilities_updated_at FROM nodes WHERE id = ?`, id).Scan(&got); err != nil || got != want {
			t.Fatalf("%s: %v %v, want %v", id, got, err, want)
		}
	}
}
