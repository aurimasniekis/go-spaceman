package backup_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/backup"
)

func TestSweepStaging(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	dir := filepath.Join(t.TempDir(), "backups")
	must(t, os.MkdirAll(dir, 0o700))
	old := time.Now().Add(-2 * time.Hour)
	mk := func(name string, age time.Time) string {
		p := filepath.Join(dir, name)
		must(t, os.MkdirAll(filepath.Join(p, "sub"), 0o700))
		must(t, os.WriteFile(filepath.Join(p, "sub", "f"), []byte("x"), 0o600))
		must(t, os.Chtimes(p, age, age))
		return p
	}
	orphan := mk(".bkp_0123456789abcdefghijklmnop.tmp-123456", old)
	fresh := mk(".bkp_0123456789abcdefghijklmnoq.tmp-654321", time.Now())
	other := mk(".bkp_short.tmp-1", old)
	other2 := mk("notes.tmp-1", old)
	plain := mk("bkp_0123456789abcdefghijklmnop", old)

	// A symlink with a matching name must not be followed or removed.
	outside := t.TempDir()
	must(t, os.WriteFile(filepath.Join(outside, "keep"), []byte("x"), 0o600))
	link := filepath.Join(dir, ".bkp_0123456789abcdefghijklmnor.tmp-777")
	must(t, os.Symlink(outside, link))

	svc := f.service(backup.LocalTarget{Dir: dir}, 2)
	must(t, svc.Recover(f.ctx))
	if _, err := os.Lstat(orphan); !os.IsNotExist(err) {
		t.Fatalf("orphan staging dir still there: %v", err)
	}
	for _, p := range []string{fresh, other, other2, plain, link, filepath.Join(outside, "keep")} {
		if _, err := os.Lstat(p); err != nil {
			t.Fatalf("%s must be left alone: %v", p, err)
		}
	}
	if n, err := svc.SweepStaging(f.ctx); err != nil || n != 0 {
		t.Fatalf("second sweep: %d %v", n, err)
	}
}

const (
	orphanA = ".bkp_0123456789abcdefghijklmnop.tmp-123456"
	orphanB = "bkp_0123456789abcdefghijklmnoq"
)

func mkOld(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	must(t, os.MkdirAll(filepath.Join(p, "sub"), 0o700))
	must(t, os.WriteFile(filepath.Join(p, "sub", "f"), []byte("x"), 0o600))
	old := time.Now().Add(-3 * time.Hour)
	must(t, os.Chtimes(p, old, old))
	return p
}

func holdLock(t *testing.T, p string) *os.File {
	t.Helper()
	fd, err := os.Open(p)
	must(t, err)
	must(t, syscall.Flock(int(fd.Fd()), syscall.LOCK_EX|syscall.LOCK_NB))
	return fd
}

func stagingSwept(t *testing.T, f *fixture) []map[string]any {
	t.Helper()
	rows, err := f.st.ReadDB().QueryContext(f.ctx, `SELECT details_json FROM audit_events WHERE action = 'backup.staging_swept' ORDER BY seq`)
	must(t, err)
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var s string
		must(t, rows.Scan(&s))
		var m map[string]any
		must(t, json.Unmarshal([]byte(s), &m))
		out = append(out, m)
	}
	must(t, rows.Err())
	return out
}

// A staging directory an old mtime would condemn survives while a running backup holds its lock, and is swept once
// the lock is gone.
func TestSweepStagingSkipsLockedDir(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	dir := filepath.Join(t.TempDir(), "backups")
	live := mkOld(t, dir, orphanA)
	fd := holdLock(t, live)
	svc := f.service(backup.LocalTarget{Dir: dir}, 2)
	if n, err := svc.SweepStaging(f.ctx); err != nil || n != 0 {
		t.Fatalf("sweep with a held lock: %d %v", n, err)
	}
	if _, err := os.Lstat(live); err != nil {
		t.Fatalf("locked staging dir removed: %v", err)
	}
	must(t, fd.Close())
	if n, err := svc.SweepStaging(f.ctx); err != nil || n != 1 {
		t.Fatalf("sweep after release: %d %v", n, err)
	}
	if _, err := os.Lstat(live); !os.IsNotExist(err) {
		t.Fatalf("released staging dir still there: %v", err)
	}
}

// On a remote target the staging folder also holds the renamed bkp_<id> directory of an upload that crashed; the sweep
// removes it unless it is locked. The audit event names what was removed.
func TestSweepStagingS3(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	dir := filepath.Join(t.TempDir(), ".staging")
	orphan := mkOld(t, dir, orphanB)
	tmp := mkOld(t, dir, orphanA)
	locked := mkOld(t, dir, "bkp_0123456789abcdefghijklmnor")
	fd := holdLock(t, locked)
	defer fd.Close()
	svc := f.service(backup.S3Target{Bucket: "b", Staging: dir}, 2)
	n, err := svc.SweepStaging(f.ctx)
	if err != nil || n != 2 {
		t.Fatalf("sweep: %d %v", n, err)
	}
	for _, p := range []string{orphan, tmp} {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Fatalf("%s still there: %v", p, err)
		}
	}
	if _, err := os.Lstat(locked); err != nil {
		t.Fatalf("locked upload dir removed: %v", err)
	}
	ev := stagingSwept(t, f)
	if len(ev) != 1 || ev[0]["count"] != float64(2) {
		t.Fatalf("audit events: %+v", ev)
	}
	var names []string
	for _, v := range ev[0]["names"].([]any) {
		names = append(names, v.(string))
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{orphanA, orphanB}) {
		t.Fatalf("audited names: %v", names)
	}
}

// Every backup sweeps first; a sweep during a running backup does nothing.
func TestSweepStagingBeforeBackupAndWhileBusy(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	dir := filepath.Join(t.TempDir(), "backups")
	first := mkOld(t, dir, orphanA)
	entered, release := make(chan struct{}), make(chan struct{})
	svc := backup.NewService(f.st, f.kek, f.aw, f.ch, backup.ServiceOptions{Target: backup.LocalTarget{Dir: dir}, Sources: f.sources(), KeepLast: 2,
		Interval: time.Hour, Config: func() []byte { return []byte("mode: development\n") },
		PrepareMemory: func(ctx context.Context) *backup.MemoryFlush {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
			}
			return &backup.MemoryFlush{}
		}})
	_, err := svc.Start(f.ctx, admin, backup.TriggerManual)
	must(t, err)
	<-entered // the sweep before the backup has run by now
	if _, err := os.Lstat(first); !os.IsNotExist(err) {
		t.Fatalf("orphan not swept before the backup: %v", err)
	}
	second := mkOld(t, dir, ".bkp_0123456789abcdefghijklmnos.tmp-9")
	if n, err := svc.SweepStaging(f.ctx); err != nil || n != 0 {
		t.Fatalf("sweep while busy: %d %v", n, err)
	}
	if _, err := os.Lstat(second); err != nil {
		t.Fatalf("swept while busy: %v", err)
	}
	close(release)
	svc.Wait()
	ev := stagingSwept(t, f)
	if len(ev) != 1 || ev[0]["count"] != float64(1) {
		t.Fatalf("audit events: %+v", ev)
	}
}

// The staging lock follows the directory through Create's rename and lasts until Result.Close.
func TestCreateHoldsStagingLock(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	dest := filepath.Join(t.TempDir(), orphanB)
	res, err := backup.Create(f.ctx, f.source(), f.kek, dest, backup.Options{ID: orphanB, Sources: f.sources()})
	must(t, err)
	probe := func() error {
		fd, err := os.Open(dest)
		must(t, err)
		defer fd.Close()
		err = syscall.Flock(int(fd.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			_ = syscall.Flock(int(fd.Fd()), syscall.LOCK_UN)
		}
		return err
	}
	if err := probe(); !errors.Is(err, syscall.EWOULDBLOCK) {
		t.Fatalf("backup dir not locked after Create: %v", err)
	}
	res.Close()
	res.Close()
	must(t, probe())
}
