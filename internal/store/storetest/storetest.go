// Package storetest opens throwaway stores for tests and enforces the short-transaction rule (Hallpass HP-DB-004).
package storetest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/store"
)

// MaxTxDuration is the longest a write transaction may hold the write lock in tests (Hallpass HP-DB-004). Time spent
// waiting for the writer and SQLITE_BUSY retries does not count (ADR-0070).
// SM_TEST_TX_MAX overrides it for slow runners; the default matches the requirement. CI on GitHub's hosted runners uses
// 250ms (ADR-0062), the race steps more.
var MaxTxDuration = func() time.Duration {
	if v, err := time.ParseDuration(os.Getenv("SM_TEST_TX_MAX")); err == nil && v > 0 {
		return v
	}
	return 100 * time.Millisecond
}()

// Option customizes Open.
type Option func(*store.Options)

// WithClock sets the store clock.
func WithClock(c clock.Clock) Option { return func(o *store.Options) { o.Clock = c } }

// WithReadObserver reports the SQL text of every query run through the store's R() to observe (store.Options.ReadObserver).
func WithReadObserver(observe func(query string)) Option {
	return func(o *store.Options) { o.ReadObserver = observe }
}

// WithDir uses dir as the data directory instead of a fresh temp dir.
func WithDir(dir string) Option { return func(o *store.Options) { o.DataDir = dir } }

// Open opens a store in a new temporary data directory and closes it at test cleanup. Any write transaction that holds
// the write lock for longer than MaxTxDuration fails the test, whether it committed or failed, unless its context was
// canceled or timed out (a canceled callback may hold the lock until it notices).
func Open(t testing.TB, opts ...Option) *store.Store {
	t.Helper()
	var mu sync.Mutex
	o := store.Options{
		DataDir: filepath.Join(t.TempDir(), "data"),
		TxObserver: func(name string, d time.Duration, err error) {
			if d > MaxTxDuration && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				mu.Lock()
				defer mu.Unlock()
				outcome := ""
				if err != nil {
					outcome = fmt.Sprintf(" and failed (%v)", err)
				}
				t.Errorf("write transaction %q held the write lock for %s (> %s, Hallpass HP-DB-004)%s", name, d, MaxTxDuration, outcome)
			}
		},
	}
	for _, opt := range opts {
		opt(&o)
	}
	Seed(t, o.DataDir)
	s, err := store.Open(context.Background(), o)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// Seed puts a freshly migrated database into dir unless dir already holds one, so that opening a store there (directly
// or through central) finds the schema current and skips the migrations. The template is built once per test binary by
// store.Open itself, through the real migration path; each copy is the same as a fresh install. Applying every
// migration costs seconds per store under the race detector (pure-Go SQLite), which made it the largest part of most
// store-backed tests; migration behavior itself is tested in internal/store with store.Open directly.
func Seed(t testing.TB, dir string) {
	t.Helper()
	path := filepath.Join(dir, store.DBFile)
	if _, err := os.Lstat(path); err == nil {
		return
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("seed store: %v", err)
	}
	b, err := migratedDB()
	if err != nil {
		t.Fatalf("seed store: %v", err)
	}
	if err := store.PrepareDataDir(dir); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatalf("seed store: %v", err)
	}
}

var template struct {
	once sync.Once
	db   []byte
	err  error
}

// migratedDB returns the bytes of a database that store.Open created and fully migrated, then closed (which
// checkpoints and removes the WAL, so the main file is complete).
func migratedDB() ([]byte, error) {
	template.once.Do(func() {
		tmp, err := os.MkdirTemp("", "storetest-template-")
		if err != nil {
			template.err = err
			return
		}
		defer func() { _ = os.RemoveAll(tmp) }()
		dir := filepath.Join(tmp, "data")
		s, err := store.Open(context.Background(), store.Options{DataDir: dir})
		if err != nil {
			template.err = fmt.Errorf("build template: %w", err)
			return
		}
		if err := s.Close(); err != nil {
			template.err = fmt.Errorf("close template: %w", err)
			return
		}
		if _, err := os.Lstat(filepath.Join(dir, store.DBFile+"-wal")); err == nil {
			template.err = errors.New("template WAL left behind after close")
			return
		}
		template.db, template.err = os.ReadFile(filepath.Join(dir, store.DBFile)) //nolint:gosec // our own temp dir
	})
	return template.db, template.err
}
