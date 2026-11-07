package storetest_test

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/storetest"
)

// A seeded data directory holds a private database that is already at the latest embedded migration, so opening a
// store there applies nothing.
func TestSeedWritesMigratedPrivateDB(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	storetest.Seed(t, dir)

	path := filepath.Join(dir, store.DBFile)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Fatalf("database mode %o, want 600", got)
	}
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := di.Mode().Perm(); got != 0o700 {
		t.Fatalf("data dir mode %o, want 700", got)
	}

	// Read the copy directly, without store.Open, which would migrate it.
	d, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	cur, latest, err := store.SchemaVersions(context.Background(), d, store.EmbeddedMigrations())
	if err != nil {
		t.Fatal(err)
	}
	if latest == 0 || cur != latest {
		t.Fatalf("seeded database at version %d, latest embedded migration %d", cur, latest)
	}

	// Opening it is a normal start: no migration runs, so no pre-migration snapshot is written.
	_ = storetest.Open(t, storetest.WithDir(dir))
	snaps, err := filepath.Glob(filepath.Join(dir, store.BackupsDir, "pre-migrate-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 0 {
		t.Fatalf("opening a seeded store migrated it: %v", snaps)
	}
}

// Seed never touches a database that is already there, so a test that prepared its own (an old schema, a restore
// fixture) keeps it.
func TestSeedLeavesExistingDBAlone(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	if err := store.PrepareDataDir(dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, store.DBFile)
	want := []byte("not a database: Seed must leave this file alone")
	if err := os.WriteFile(path, want, 0o600); err != nil {
		t.Fatal(err)
	}

	storetest.Seed(t, dir)

	got, err := os.ReadFile(path) //nolint:gosec // the test's own temp dir
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("Seed replaced an existing database (%d bytes now)", len(got))
	}
}
