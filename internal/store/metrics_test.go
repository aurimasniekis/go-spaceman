package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/obs/metrics"
	"github.com/aurimasniekis/go-spaceman/internal/obs/metrics/metricstest"
)

// Hallpass HP-DB-003/004/011: write transactions, busy retries, writer wait and DB/WAL sizes are exported.
func TestWriterMetrics(t *testing.T) {
	t.Parallel()
	m := metrics.New()
	s := openTest(t, Options{Metrics: m, BusyTimeout: 5 * time.Millisecond})
	ctx := context.Background()
	if v := metricstest.Value(t, m, "spaceman_store_db_bytes"); v == 0 {
		t.Error("db size not exported at open")
	}
	if err := s.W().TxNamed(ctx, "test.put", func(tx *Tx) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := s.W().TxNamed(ctx, "test.fail", func(tx *Tx) error { return fmt.Errorf("x: %w", ErrConflict) }); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	<-holdWriteLock(t, s.Path(DBFile), 0) // warm up
	released := holdWriteLock(t, s.Path(DBFile), 3*time.Second)
	if err := s.W().TxNamed(ctx, "test.busy", func(tx *Tx) error { return nil }); !errors.Is(err, ErrBusy) {
		t.Fatalf("want busy, got %v", err)
	}
	<-released
	for _, c := range []struct{ name, result string }{{"test.put", "ok"}, {"test.fail", "conflict"}, {"test.busy", "busy"}} {
		if v := metricstest.Value(t, m, "spaceman_store_tx_total", "name", c.name, "result", c.result); v != 1 {
			t.Errorf("%s/%s = %v", c.name, c.result, v)
		}
	}
	if v := metricstest.Value(t, m, "spaceman_store_tx_busy_retries_total"); v != txMaxAttempts-1 {
		t.Errorf("busy retries = %v, want %d", v, txMaxAttempts-1)
	}
	if v := metricstest.Value(t, m, "spaceman_store_writer_wait_seconds"); v < 3 {
		t.Errorf("writer wait samples = %v", v)
	}
	if err := s.Checkpoint(ctx, "TRUNCATE"); err != nil {
		t.Fatal(err)
	}
}

// Hallpass HP-DB-003: write transactions waiting for the single writer connection are visible as queue depth.
func TestWriterQueueDepth(t *testing.T) {
	t.Parallel()
	m := metrics.New()
	s := openTest(t, Options{Metrics: m, BusyTimeout: 10 * time.Second})
	ctx := context.Background()
	// Another connection holds the database write lock for a second, so BEGIN IMMEDIATE has to wait.
	released := holdWriteLock(t, s.Path(DBFile), time.Second)
	done := make(chan error, 1)
	go func() { done <- s.W().TxNamed(ctx, "test.wait", func(tx *Tx) error { return nil }) }()
	deadline := time.Now().Add(5 * time.Second)
	for metricstest.Value(t, m, "spaceman_store_writer_queue_depth") != 1 {
		if time.Now().After(deadline) {
			t.Fatal("queued writer not reported")
		}
		time.Sleep(5 * time.Millisecond)
	}
	<-released
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if v := metricstest.Value(t, m, "spaceman_store_writer_queue_depth"); v != 0 {
		t.Fatalf("queue depth after the transaction = %v", v)
	}
}

func TestTxResultClassification(t *testing.T) {
	t.Parallel()
	for err, want := range map[error]metrics.TxResult{
		nil: metrics.TxOK, context.Canceled: metrics.TxCanceled, ErrBusy: metrics.TxBusy, ErrFull: metrics.TxFull, ErrIO: metrics.TxIO,
		ErrCorrupt: metrics.TxCorrupt, ErrConstraint: metrics.TxConstraint, ErrConflict: metrics.TxConflict, sql.ErrNoRows: metrics.TxNotFound,
		ErrNotFound: metrics.TxNotFound, errors.New("other"): metrics.TxError,
	} {
		if got := TxResult(err); got != want {
			t.Errorf("TxResult(%v) = %s, want %s", err, got, want)
		}
	}
}

// ADR-0070: the observer gets how long the transaction held the write lock, not how long it waited for it. Another
// connection holds the database write lock, so BEGIN IMMEDIATE waits; the end-to-end duration metric still counts the
// wait.
func TestTxObserverGetsLockHoldTime(t *testing.T) {
	t.Parallel()
	const wait = 300 * time.Millisecond
	m := metrics.New()
	var held atomic.Int64
	s := openTest(t, Options{Metrics: m, BusyTimeout: 10 * time.Second, TxObserver: func(name string, d time.Duration, err error) {
		if name == "test.wait" && err == nil {
			held.Store(int64(d))
		}
	}})
	ctx := context.Background()
	<-holdWriteLock(t, s.Path(DBFile), 0) // warm up
	released := holdWriteLock(t, s.Path(DBFile), wait)
	start := time.Now()
	if err := s.W().TxNamed(ctx, "test.wait", func(tx *Tx) error { return nil }); err != nil {
		t.Fatal(err)
	}
	total := time.Since(start)
	<-released
	if total < wait/2 {
		t.Fatalf("the transaction did not wait for the lock (took %s)", total)
	}
	if d := time.Duration(held.Load()); d <= 0 || d >= total/2 {
		t.Fatalf("observed %s for a transaction that waited %s for the lock: want the hold time only", d, total)
	}
	if v := metricstest.Value(t, m, "spaceman_store_writer_wait_seconds"); v < 1 {
		t.Errorf("writer wait samples = %v", v)
	}
}
