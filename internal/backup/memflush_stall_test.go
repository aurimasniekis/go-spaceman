package backup_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/backup"
)

// succeededWithStaleMemory checks that backup id succeeded, verifies, and records the memory flush as attempted with
// note containing note (when not empty) and stale spaces as given.
func succeededWithStaleMemory(t *testing.T, f *fixture, svc *backup.Service, dir, id, note string, stale int) {
	t.Helper()
	st, err := svc.Status(f.ctx)
	must(t, err)
	it := item(t, st, id)
	if st.Busy || it.State != backup.StateSucceeded || it.Error != "" {
		t.Fatalf("busy %v item %+v", st.Busy, it)
	}
	mf := it.MemoryFlush
	if mf == nil || !mf.Attempted || len(mf.Stale) != stale || (note != "" && !strings.Contains(mf.Note, note)) {
		t.Fatalf("memory flush: %+v", mf)
	}
	rep, err := backup.Verify(f.ctx, filepath.Join(dir, id), backup.VerifyOptions{KEK: f.kek, TempDir: t.TempDir()})
	if err != nil || !rep.OK {
		t.Fatalf("verify: %+v %v", rep, err)
	}
}

// SM-MEM-005, ADR-0091: a memory flush that waits until it is stopped (a node that never answers) cannot fail the
// backup. The flush is stopped by its own bound (MemoryFlushTimeout), which the snapshot does not share: the
// backup goes on, succeeds and records the space as possibly stale. Before the fix the flush ran under the backup's
// watchdog, which stopped the run in stage memory_flush and failed the snapshot ("open database …: context canceled").
// The stall limit is far above the flush bound (MemoryFlushTimeout), so load cannot make the watchdog the one to stop it.
func TestServiceMemoryFlushStallDoesNotFailBackup(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	dir := filepath.Join(t.TempDir(), "backups")
	svc := backup.NewService(f.st, f.kek, f.aw, f.ch, backup.ServiceOptions{Target: backup.LocalTarget{Dir: dir}, Sources: f.sources(),
		StallTimeout: time.Minute, MemoryFlushTimeout: 300 * time.Millisecond,
		PrepareMemory: func(ctx context.Context) *backup.MemoryFlush {
			<-ctx.Done()
			return &backup.MemoryFlush{Attempted: true, Flushed: []string{},
				Stale: []backup.StaleMemory{{SpaceID: "spc_a", SpaceSlug: "aaa", NodeID: "nod_1", Reason: "timed_out", Detail: "the backup was stopped"}}}
		}})
	b, err := svc.Start(f.ctx, admin, backup.TriggerManual)
	must(t, err)
	waitIdle(t, svc)
	succeededWithStaleMemory(t, f, svc, dir, b.ID, "", 1)
}

// A memory flush that ignores its context and never returns is abandoned shortly after its bound
// (MemoryFlushTimeout): the backup goes on without its result, records the flush as unfinished and succeeds.
func TestServiceMemoryFlushHookStuck(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	dir := filepath.Join(t.TempDir(), "backups")
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	reported := make(chan struct{})
	svc := backup.NewService(f.st, f.kek, f.aw, f.ch, backup.ServiceOptions{Target: backup.LocalTarget{Dir: dir}, Sources: f.sources(),
		MemoryFlushTimeout: 300 * time.Millisecond,
		PrepareMemory: func(ctx context.Context) *backup.MemoryFlush {
			backup.ReportProgress(ctx)
			close(reported)
			<-release
			return nil
		}})
	start := time.Now()
	b, err := svc.Start(f.ctx, admin, backup.TriggerManual)
	must(t, err)
	<-reported
	waitIdle(t, svc)
	if d := time.Since(start); d > 2*time.Minute {
		t.Fatalf("the backup took %s", d)
	}
	succeededWithStaleMemory(t, f, svc, dir, b.ID, "did not finish within 300ms", 0)
	backup.ReportProgress(f.ctx) // outside a backup: nothing to do
}
