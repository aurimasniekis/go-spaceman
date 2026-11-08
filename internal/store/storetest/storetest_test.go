package storetest_test

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

// guardTB records the guard's failures instead of failing the test, so a test can check which transactions tripped it.
type guardTB struct {
	testing.TB
	mu   sync.Mutex
	errs []string
}

func (g *guardTB) Errorf(format string, args ...any) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.errs = append(g.errs, fmt.Sprintf(format, args...))
}

func (g *guardTB) tripped(name string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, e := range g.errs {
		if strings.Contains(e, fmt.Sprintf("%q", name)) {
			return true
		}
	}
	return false
}

// holdWriter stands for a slow transaction body: it keeps the write lock for d. Only this test does that on purpose.
func holdWriter(d time.Duration) { time.Sleep(d) }

// Hallpass HP-DB-004 guard (ADR-0070): a transaction that holds the write lock for longer than MaxTxDuration fails the
// test; one that only waits that long behind another writer does not.
func TestGuardCountsLockHoldNotWait(t *testing.T) {
	g := &guardTB{TB: t}
	s := storetest.Open(g)
	ctx := context.Background()
	hold := 2 * storetest.MaxTxDuration

	var holding atomic.Bool
	slow := make(chan error, 1)
	go func() {
		slow <- s.W().TxNamed(ctx, "test.hold", func(tx *store.Tx) error {
			holding.Store(true)
			holdWriter(hold)
			return nil
		})
	}()
	for !holding.Load() {
		time.Sleep(time.Millisecond)
	}
	start := time.Now()
	if err := s.W().TxNamed(ctx, "test.wait", func(tx *store.Tx) error { return nil }); err != nil {
		t.Fatal(err)
	}
	waited := time.Since(start)
	if err := <-slow; err != nil {
		t.Fatal(err)
	}
	if waited <= storetest.MaxTxDuration {
		t.Fatalf("test.wait waited only %s behind the writer, want more than %s", waited, storetest.MaxTxDuration)
	}
	if g.tripped("test.wait") {
		t.Errorf("the guard counted the wait for the writer (%s): %v", waited, g.errs)
	}
	if !g.tripped("test.hold") {
		t.Errorf("a transaction holding the lock for %s did not trip the %s guard: %v", hold, storetest.MaxTxDuration, g.errs)
	}
}
