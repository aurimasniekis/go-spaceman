// Package store owns the SQLite database: connection pools, write transactions, migrations, the process lock,
// integrity checks and the backup primitive (Hallpass PLAN §4, ADR-0002).
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"time"

	_ "modernc.org/sqlite" // database/sql driver "sqlite"

	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/obs/metrics"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// File names inside the data directory.
const (
	DBFile        = "spaceman.db"
	LockFile      = "spaceman.lock"
	BackupsDir    = "backups"
	RecordingsDir = "recordings"
	QuarantineDir = "quarantine"
)

// Options configure Open.
type Options struct {
	DataDir string
	// Readers is the reader pool size (default max(4, GOMAXPROCS)).
	Readers int
	// AllowNetworkFS skips the network-filesystem refusal (development only).
	AllowNetworkFS bool
	// PreMigrationSnapshots is how many pre-migration snapshots to keep (default 3).
	PreMigrationSnapshots int
	// SkipMigrate opens without applying migrations (used by offline tools that only read).
	SkipMigrate bool
	Logger      *slog.Logger
	Clock       clock.Clock
	// TxObserver, if set, receives the duration and outcome of every write transaction.
	TxObserver TxObserver
	// Metrics, if set, records write transaction duration and results, busy retries and writer wait time
	// (Hallpass HP-DB-003, Hallpass HP-DB-004). Nil disables metrics.
	Metrics *metrics.Metrics
	// BusyTimeout overrides SQLite's busy_timeout (default 5s). Tests only.
	BusyTimeout time.Duration
	// Migrations overrides the embedded central migrations (the node agent journal and tests use their own).
	Migrations fs.FS
}

// Store is an open SpaceMan database owned exclusively by this process.
type Store struct {
	dir    string
	lock   *Lock
	wdb    *sql.DB
	rdb    *sql.DB
	writer *Writer
	reader *db.Queries
	log    *slog.Logger
	clock  clock.Clock
	health *Health

	closeOnce sync.Once
	closeErr  error
}

// Open prepares the data directory, takes the process lock, opens the pools, checks integrity and migrates.
func Open(ctx context.Context, opts Options) (*Store, error) {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Clock == nil {
		opts.Clock = clock.System{}
	}
	if opts.Readers <= 0 {
		opts.Readers = max(4, runtime.GOMAXPROCS(0))
	}
	if opts.PreMigrationSnapshots <= 0 {
		opts.PreMigrationSnapshots = 3
	}
	if opts.BusyTimeout <= 0 {
		opts.BusyTimeout = DefaultBusyTimeout
	}
	if opts.Migrations == nil {
		sub, err := fs.Sub(migrationFS, "migrations")
		if err != nil {
			return nil, err
		}
		opts.Migrations = sub
	}
	dir, err := filepath.Abs(opts.DataDir)
	if err != nil {
		return nil, fmt.Errorf("data dir: %w", err)
	}
	if err := PrepareDataDir(dir); err != nil {
		return nil, err
	}
	if !opts.AllowNetworkFS {
		if err := CheckLocalFilesystem(dir); err != nil {
			return nil, err
		}
	}
	lock, err := AcquireLock(filepath.Join(dir, LockFile))
	if err != nil {
		return nil, err
	}
	s := &Store{dir: dir, lock: lock, log: opts.Logger, clock: opts.Clock, health: newHealth(opts.Clock)}
	if err := s.open(ctx, opts); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) open(ctx context.Context, opts Options) error {
	path := filepath.Join(s.dir, DBFile)
	var err error
	if s.wdb, err = sql.Open("sqlite", writerDSN(path, opts.BusyTimeout)); err != nil {
		return fmt.Errorf("open writer: %w", err)
	}
	s.wdb.SetMaxOpenConns(1)
	s.wdb.SetMaxIdleConns(1)
	s.wdb.SetConnMaxLifetime(0)
	s.wdb.SetConnMaxIdleTime(0)
	if err := s.wdb.PingContext(ctx); err != nil {
		err = classify(fmt.Errorf("open database: %w", err))
		if errors.Is(err, ErrCorrupt) {
			return fmt.Errorf("%w; restore from the latest verified backup (site/operators/backup.md)", err)
		}
		return err
	}
	if err := secureFiles(s.dir); err != nil {
		return err
	}
	if err := QuickCheck(ctx, s.wdb); err != nil {
		return err
	}
	if !opts.SkipMigrate {
		if err := migrate(ctx, s.wdb, opts.Migrations, s.dir, opts.PreMigrationSnapshots, s.clock, s.log); err != nil {
			return err
		}
	} else if err := checkSchemaNotNewer(ctx, s.wdb, opts.Migrations); err != nil {
		return err
	}
	if s.rdb, err = sql.Open("sqlite", readerDSN(path, opts.BusyTimeout)); err != nil {
		return fmt.Errorf("open readers: %w", err)
	}
	s.rdb.SetMaxOpenConns(opts.Readers)
	s.rdb.SetMaxIdleConns(opts.Readers)
	if err := VerifyPragmas(ctx, s.wdb, 1, false, opts.BusyTimeout); err != nil {
		return err
	}
	if err := VerifyPragmas(ctx, s.rdb, opts.Readers, true, opts.BusyTimeout); err != nil {
		return err
	}
	if err := secureFiles(s.dir); err != nil {
		return err
	}
	s.writer = &Writer{db: s.wdb, clock: s.clock, health: s.health, observer: opts.TxObserver, metrics: opts.Metrics}
	s.health.mu.Lock()
	s.health.probe = s.writer.spaceProbe
	s.health.mu.Unlock()
	s.reader = db.New(s.rdb)
	opts.Metrics.SetStoreSizes(s.Sizes())
	return nil
}

// Close closes both pools (checkpointing the WAL) and releases the process lock.
func (s *Store) Close() error {
	s.closeOnce.Do(s.closeNow) //nolint:contextcheck // Close is called without a caller context
	return s.closeErr
}

func (s *Store) closeNow() {
	// One deadline for the whole close (the checkpoint and both pools' drains), so shutdown stays inside the service
	// manager's stop timeout (closeDrainTimeout).
	deadline := time.Now().Add(closeDrainTimeout)
	var errs []error
	if s.rdb != nil {
		errs = append(errs, s.rdb.Close(), drainConns(s.rdb, "reader", deadline))
	}
	if s.wdb != nil {
		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		_, _ = s.wdb.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)")
		cancel()
		errs = append(errs, s.wdb.Close(), drainConns(s.wdb, "writer", deadline))
	}
	if s.lock != nil {
		errs = append(errs, s.lock.Release())
	}
	s.closeErr = errors.Join(errs...)
}

// closeDrainTimeout bounds the whole of Close: the WAL checkpoint and the wait for connections that were in use when it
// was called, both pools sharing the one deadline. With central's shutdown.timeout (30 s by default) and the 5 s for
// gateway mints, a stop then takes at most 43 s, inside the units' TimeoutStopSec=45s (site/operators/upgrade.md). A
// variable so a test can shorten it.
var closeDrainTimeout = 8 * time.Second

// drainConns waits until every connection of a closed pool is closed. sql.DB.Close closes only idle connections: one
// still in use (a query or transaction of a goroutine that has not stopped yet) closes when it is released. Until then
// it keeps the database file open, and closing it later (the last connection of the process checkpoints the WAL under
// an exclusive lock) races a store opened next on the same directory, so Close does not release the process lock
// before.
//
// When the deadline passes first, the lock is released anyway (a stuck goroutine must not keep the node from shutting
// down) and the error says how many connections were still in use.
func drainConns(d *sql.DB, pool string, deadline time.Time) error {
	for d.Stats().OpenConnections > 0 {
		if time.Now().After(deadline) {
			return fmt.Errorf("close %s pool: %d connections still in use after %s", pool, d.Stats().OpenConnections, closeDrainTimeout)
		}
		time.Sleep(5 * time.Millisecond)
	}
	return nil
}

// W returns the single writer.
func (s *Store) W() *Writer { return s.writer }

// Now returns the store clock's current time (the time write transactions see).
func (s *Store) Now() time.Time { return s.clock.Now().UTC() }

// R returns typed queries over the read-only pool.
func (s *Store) R() *db.Queries { return s.reader }

// ReadDB returns the read-only pool for dynamic queries (filters, pagination).
func (s *Store) ReadDB() *sql.DB { return s.rdb }

// Dir returns the absolute data directory.
func (s *Store) Dir() string { return s.dir }

// Health returns the storage health tracker.
func (s *Store) Health() *Health { return s.health }

// Path returns the absolute path of a file inside the data directory.
func (s *Store) Path(elem ...string) string {
	return filepath.Join(append([]string{s.dir}, elem...)...)
}

// DefaultBusyTimeout is SQLite's busy_timeout for every connection.
const DefaultBusyTimeout = 5 * time.Second

func baseDSN(busy time.Duration) url.Values {
	q := url.Values{}
	// _busy_timeout, not a _pragma: the driver runs it first, before _auto_vacuum (which reads the file) and the
	// _pragma list. Set as a _pragma, the writer's auto_vacuum ran without a busy handler and an open failed at once
	// with SQLITE_BUSY whenever another connection briefly held the file (for example while closing and checkpointing).
	q.Set("_busy_timeout", strconv.FormatInt(busy.Milliseconds(), 10))
	for _, p := range []string{
		"foreign_keys(1)",
		"synchronous(FULL)",
		"trusted_schema(0)",
		"cache_size(-32000)",
		"temp_store(MEMORY)",
	} {
		q.Add("_pragma", p)
	}
	return q
}

func writerDSN(path string, busy time.Duration) string {
	q := baseDSN(busy)
	q.Set("_auto_vacuum", "INCREMENTAL") // only effective when the database is created
	q.Add("_pragma", "journal_mode(WAL)")
	q.Set("_txlock", "immediate") // BEGIN IMMEDIATE: take the write lock up front
	return "file:" + path + "?" + q.Encode()
}

func readerDSN(path string, busy time.Duration) string {
	q := baseDSN(busy)
	q.Add("_pragma", "query_only(1)")
	return "file:" + path + "?" + q.Encode()
}

// secureFiles tightens permissions of the database files to 0600 (Hallpass HP-DB-010).
func secureFiles(dir string) error {
	for _, name := range []string{DBFile, DBFile + "-wal", DBFile + "-shm", LockFile} {
		p := filepath.Join(dir, name)
		fi, err := os.Stat(p)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("stat %s: %w", p, err)
		}
		if fi.Mode().Perm()&0o077 != 0 {
			if err := os.Chmod(p, 0o600); err != nil {
				return fmt.Errorf("chmod %s: %w", p, err)
			}
		}
	}
	return nil
}

// ReadTx runs fn in a read-only transaction on the reader pool, giving fn a consistent snapshot of the database.
func (s *Store) ReadTx(ctx context.Context, fn func(q *db.Queries) error) error {
	tx, err := s.rdb.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return classify(err)
	}
	defer func() { _ = tx.Rollback() }()
	return classify(fn(db.New(tx)))
}
