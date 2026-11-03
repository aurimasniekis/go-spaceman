package backup_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/backup"
)

// SM-MEM-005: the service flushes role memory before every backup and before the snapshot, and records the outcome in
// the sealed manifest (with a warning naming stale spaces) and on the listed item; without a hook it records that no
// flush was attempted; the offline CLI records that it could not ask the nodes; manifests without the record (older
// backups) still verify.
func TestServiceRecordsMemoryFlush(t *testing.T) {
	f := newFixture(t)
	dir := filepath.Join(t.TempDir(), "backups")
	var calls int
	var snapshotted bool
	at := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	svc := backup.NewService(f.st, f.kek, f.aw, f.ch, backup.ServiceOptions{Target: backup.LocalTarget{Dir: dir}, Sources: f.sources(), KeepLast: 5,
		Interval: time.Hour, PrepareMemory: func(context.Context) *backup.MemoryFlush {
			calls++
			// Runs before the backup's staging directory exists: nothing has been snapshotted yet.
			matches, _ := filepath.Glob(filepath.Join(dir, ".*"))
			snapshotted = len(matches) > 0
			return &backup.MemoryFlush{Attempted: true, StartedAt: &at, Flushed: []string{"spc_a"},
				Stale: []backup.StaleMemory{{SpaceID: "spc_b", SpaceSlug: "billing", NodeID: "nod_b", Reason: "node_disconnected"}}}
		}})
	b, err := svc.Start(f.ctx, admin, backup.TriggerManual)
	must(t, err)
	svc.Wait()
	if calls != 1 || snapshotted {
		t.Fatalf("hook calls %d, after the snapshot %v", calls, snapshotted)
	}
	st, err := svc.Status(f.ctx)
	must(t, err)
	it := item(t, st, b.ID)
	if it.State != backup.StateSucceeded || it.MemoryFlush == nil || !it.MemoryFlush.Attempted || len(it.MemoryFlush.Flushed) != 1 ||
		len(it.MemoryFlush.Stale) != 1 || it.MemoryFlush.Stale[0].SpaceSlug != "billing" {
		t.Fatalf("item: %+v / %+v", it, it.MemoryFlush)
	}
	m, err := backup.ReadManifest(filepath.Join(dir, b.ID))
	must(t, err)
	if len(m.Warnings) == 0 || !strings.Contains(strings.Join(m.Warnings, "; "), "role memory possibly stale for spaces billing (node_disconnected)") {
		t.Fatalf("warnings: %v", m.Warnings)
	}
	rep, err := backup.Verify(f.ctx, filepath.Join(dir, b.ID), backup.VerifyOptions{KEK: f.kek, TempDir: t.TempDir()})
	if err != nil || !rep.OK {
		t.Fatalf("verify: %+v %v", rep, err)
	}

	// No hook (no node connection): recorded as not attempted, with an empty list rather than none.
	svc2 := f.service(backup.LocalTarget{Dir: dir}, 5)
	b2, err := svc2.Start(f.ctx, admin, backup.TriggerManual)
	must(t, err)
	svc2.Wait()
	st, _ = svc2.Status(f.ctx)
	if mf := item(t, st, b2.ID).MemoryFlush; mf == nil || mf.Attempted || mf.Note == "" || mf.Stale == nil || mf.Flushed == nil {
		t.Fatalf("without a hook: %+v", mf)
	}

	// The offline CLI backup says why it could not flush; a backup without the record verifies too.
	off := f.create(filepath.Join(t.TempDir(), "plain"))
	if off.Manifest.MemoryFlush != nil {
		t.Fatalf("plain create recorded a flush: %+v", off.Manifest.MemoryFlush)
	}
	if rep, err := backup.Verify(f.ctx, off.Dir, backup.VerifyOptions{KEK: f.kek, TempDir: t.TempDir()}); err != nil || !rep.OK {
		t.Fatalf("verify without a flush record: %+v %v", rep, err)
	}
	res, err := backup.Create(f.ctx, f.source(), f.kek, filepath.Join(t.TempDir(), "cli"), backup.Options{Sources: f.sources(), MemoryFlush: backup.OfflineMemoryFlush()})
	must(t, err)
	if mf := res.Manifest.MemoryFlush; mf == nil || mf.Attempted || !strings.Contains(mf.Note, "offline") || len(res.Manifest.Warnings) != 0 {
		t.Fatalf("offline: %+v %v", mf, res.Manifest.Warnings)
	}
}
