package store

import (
	"context"
	"path/filepath"
	"testing"
)

// 0064 adds space_roles.description (ADR-0095): a database at 0060, the latest migration before it on this branch (0061-0063
// belong to other branches of the release and the migrator needs no contiguous numbers), keeps its roles and they get ”
// (use the template's description); a fresh database has the column.
func TestSpaceRoleDescriptionMigrationFrom0060(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "data")
	s, err := Open(ctx, Options{DataDir: dir, Migrations: embeddedUpTo(t, "0060")})
	if err != nil {
		t.Fatal(err)
	}
	if hasColumn(ctx, t, s, "space_roles", "description") {
		t.Fatal("0060 already has space_roles.description")
	}
	for _, q := range []string{
		`INSERT INTO nodes (id, name, state, created_at, created_by, updated_at) VALUES ('nod_1', 'n1', 'active', 0, 't', 0)`,
		`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('spc_a', 'aaa', 'A', 'nod_1', 'active', 'a', 0, 't', 0)`,
		`INSERT INTO role_templates (id, slug, name, kind, created_at, created_by, updated_at) VALUES ('rtp_1', 'impl', 'Impl', 'worker', 0, 't', 0)`,
		`INSERT INTO space_roles (space_id, name, template_id, created_at, created_by, updated_at) VALUES ('spc_a', 'impl-a', 'rtp_1', 0, 't', 0)`,
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
	if err != nil || cur != latest || latest < 64 {
		t.Fatalf("after the upgrade: version %d of %d, %v; want all applied, at least 0064", cur, latest, err)
	}
	var desc, tpl string
	if err := s.rdb.QueryRowContext(ctx, `SELECT description, template_id FROM space_roles WHERE space_id = 'spc_a' AND name = 'impl-a'`).Scan(&desc, &tpl); err != nil {
		t.Fatal(err)
	}
	if desc != "" || tpl != "rtp_1" {
		t.Errorf("existing role: description %q template %q, want '' and the template kept", desc, tpl)
	}

	fresh, err := Open(ctx, Options{DataDir: filepath.Join(t.TempDir(), "fresh")})
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	if !hasColumn(ctx, t, fresh, "space_roles", "description") {
		t.Fatal("a fresh database lacks space_roles.description")
	}
}
