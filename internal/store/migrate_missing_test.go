package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const mig4 = "-- +goose Up\nCREATE TABLE t4 (id INTEGER PRIMARY KEY) STRICT;\n-- +goose Down\nSELECT 1;\n"

func tableExists(t *testing.T, s *Store, name string) bool {
	t.Helper()
	var n int
	if err := s.wdb.QueryRowContext(context.Background(), "SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?", name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

// A migration lower than the database's version that was never applied (0002 arriving after 0003 already ran, because
// branches merged out of number order) refuses the open with an error naming it, whether or not newer migrations are
// pending, instead of goose skipping it silently. Nothing is applied and no snapshot is written.
func TestMissingLowerMigrationRefused(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "data")
	s, err := Open(ctx, Options{DataDir: dir, Migrations: migFS(map[string]string{"0001_a.sql": mig1, "0003_c.sql": mig4})})
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	mig5 := strings.ReplaceAll(mig4, "t4", "t5")
	for _, files := range []map[string]string{
		{"0001_a.sql": mig1, "0002_b.sql": mig2, "0003_c.sql": mig4},                     // cur == latest: goose skipped 0002 silently
		{"0001_a.sql": mig1, "0002_b.sql": mig2, "0003_c.sql": mig4, "0004_d.sql": mig5}, // cur < latest: goose failed vaguely
	} {
		_, err := Open(ctx, Options{DataDir: dir, Migrations: migFS(files)})
		if !errors.Is(err, ErrMissingMigrations) {
			t.Fatalf("%d migrations: want ErrMissingMigrations, got %v", len(files), err)
		}
		for _, want := range []string{"version 3", "0002 (0002_b.sql)", "restore a backup", "renumbered above 3"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not mention %q", err, want)
			}
		}
	}

	s, err = Open(ctx, Options{DataDir: dir, SkipMigrate: true, Migrations: migFS(map[string]string{"0001_a.sql": mig1, "0003_c.sql": mig4})})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if tableExists(t, s, "t2") || tableExists(t, s, "t5") {
		t.Fatal("a refused open applied a migration")
	}
	if entries, _ := os.ReadDir(filepath.Join(dir, BackupsDir)); len(entries) != 0 {
		t.Fatalf("a refused open wrote snapshots: %v", entries)
	}
}

// A database restored from an older backup (here the pre-migration snapshot, at schema 2) has no gaps below its version:
// it opens and migrates forward to the binary's latest, in order.
func TestRestoredOlderSchemaMigratesForward(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	dir := filepath.Join(root, "data")
	v2 := map[string]string{"0001_a.sql": mig1, "0002_b.sql": mig2}
	s, err := Open(ctx, Options{DataDir: dir, Migrations: migFS(v2)})
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	v3 := map[string]string{"0001_a.sql": mig1, "0002_b.sql": mig2, "0003_c.sql": mig4}
	if s, err = Open(ctx, Options{DataDir: dir, Migrations: migFS(v3)}); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	entries, _ := os.ReadDir(filepath.Join(dir, BackupsDir))
	if len(entries) != 1 {
		t.Fatalf("want one pre-migration snapshot, got %v", entries)
	}
	snap, err := os.ReadFile(filepath.Join(dir, BackupsDir, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}

	restored := filepath.Join(root, "restored")
	if err := os.MkdirAll(restored, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(restored, DBFile), snap, 0o600); err != nil {
		t.Fatal(err)
	}
	v4 := map[string]string{"0001_a.sql": mig1, "0002_b.sql": mig2, "0003_c.sql": mig4, "0004_d.sql": strings.ReplaceAll(mig4, "t4", "t5")}
	s, err = Open(ctx, Options{DataDir: restored, Migrations: migFS(v4)})
	if err != nil {
		t.Fatalf("restored schema-2 database did not migrate forward: %v", err)
	}
	defer s.Close()
	cur, latest, err := SchemaVersions(ctx, s.wdb, migFS(v4))
	if err != nil || cur != 4 || latest != 4 {
		t.Fatalf("cur=%d latest=%d err=%v", cur, latest, err)
	}
	if !tableExists(t, s, "t4") || !tableExists(t, s, "t5") {
		t.Fatal("forward migrations did not run")
	}
}

// The real schema: central restored from a schema-0047 backup migrates forward to the latest; a database whose 0046 row
// is gone (0046 never applied) is refused with the file named.
func TestEmbeddedMigrationsRestoredAndMissing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "data")
	s, err := Open(ctx, Options{DataDir: dir, Migrations: embeddedUpTo(t, "0047")})
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if s, err = Open(ctx, Options{DataDir: dir}); err != nil {
		t.Fatalf("schema-0047 database did not migrate forward: %v", err)
	}
	cur, latest, err := SchemaVersions(ctx, s.wdb, EmbeddedMigrations())
	if err != nil || cur != latest {
		t.Fatalf("cur=%d latest=%d err=%v", cur, latest, err)
	}
	if _, err := s.wdb.ExecContext(ctx, "DELETE FROM "+gooseTable+" WHERE version_id = 46"); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	_, err = Open(ctx, Options{DataDir: dir})
	if !errors.Is(err, ErrMissingMigrations) || !strings.Contains(err.Error(), "0046 (0046_lease_disconnect_grace.sql)") {
		t.Fatalf("want ErrMissingMigrations naming 0046, got %v", err)
	}
}
