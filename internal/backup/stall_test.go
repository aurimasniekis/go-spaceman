package backup_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/backup"
)

// hangingTarget is a local target whose Store blocks until its context ends while hang is set: an upload to object
// storage over a connection that went silent.
type hangingTarget struct {
	backup.LocalTarget
	hang    *atomic.Bool
	entered chan struct{}
}

func (t hangingTarget) Store(ctx context.Context, dir string) error {
	if t.hang.Load() {
		select {
		case t.entered <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return ctx.Err()
	}
	return t.LocalTarget.Store(ctx, dir)
}

// waitIdle waits for the service's background work; a hang fails the test instead of the whole package timing out.
func waitIdle(t *testing.T, svc *backup.Service) {
	t.Helper()
	done := make(chan struct{})
	go func() { svc.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the backup is still running: nothing bounded the stuck stage")
	}
}

// A stage that makes no progress fails the backup with an error naming the stage; the run is recorded as failed,
// busy is cleared, and the next backup starts and succeeds (ADR-0091). While it is stuck, the status shows the stage.
func TestServiceStalledStageFailsAndReleasesBusy(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	hang := &atomic.Bool{}
	hang.Store(true)
	tgt := hangingTarget{LocalTarget: backup.LocalTarget{Dir: filepath.Join(t.TempDir(), "backups")}, hang: hang, entered: make(chan struct{}, 1)}
	svc := backup.NewService(f.st, f.kek, f.aw, f.ch, backup.ServiceOptions{Target: tgt, Sources: f.sources(), StallTimeout: 10 * time.Second})
	first, err := svc.Start(f.ctx, admin, backup.TriggerManual)
	must(t, err)
	select {
	case <-tgt.entered:
	case <-time.After(30 * time.Second):
		waitIdle(t, svc)
		st, _ := svc.Status(f.ctx)
		t.Fatalf("the backup never reached the upload: %+v", st.Items)
	}
	st, err := svc.Status(f.ctx)
	must(t, err)
	if !st.Busy || st.Current == nil || st.Current.ID != first.ID || st.Current.Stage != backup.StageUpload || st.Current.StageStartedAt.IsZero() {
		t.Fatalf("status while stuck: busy %v current %+v", st.Busy, st.Current)
	}
	if it := item(t, st, first.ID); it.State != backup.StateRunning || it.Stage != backup.StageUpload {
		t.Fatalf("item while stuck: %+v", it)
	}
	waitIdle(t, svc)

	st, err = svc.Status(f.ctx)
	must(t, err)
	if st.Busy || st.Current != nil {
		t.Fatalf("still busy after the stall: busy %v current %+v", st.Busy, st.Current)
	}
	it := item(t, st, first.ID)
	if it.State != backup.StateFailed || it.FinishedAt == nil || !strings.Contains(it.Error, "stage upload") || !strings.Contains(it.Error, "no progress") ||
		it.Stage != "" {
		t.Fatalf("stalled run: %+v", it)
	}

	hang.Store(false)
	second, err := svc.Start(f.ctx, admin, backup.TriggerManual)
	if errors.Is(err, backup.ErrRunning) {
		t.Fatal("a stalled backup still blocks the next one")
	}
	must(t, err)
	waitIdle(t, svc)
	st, err = svc.Status(f.ctx)
	must(t, err)
	if it := item(t, st, second.ID); it.State != backup.StateSucceeded {
		t.Fatalf("next backup: %+v", it)
	}
}

// The overall limit stops a backup even while its stage keeps reporting progress, here a memory flush that never
// returns on its own.
func TestServiceMaxDurationFailsBackup(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	svc := backup.NewService(f.st, f.kek, f.aw, f.ch, backup.ServiceOptions{Target: backup.LocalTarget{Dir: filepath.Join(t.TempDir(), "b")},
		Sources: f.sources(), StallTimeout: time.Hour, MaxDuration: 300 * time.Millisecond,
		PrepareMemory: func(ctx context.Context) *backup.MemoryFlush {
			<-ctx.Done()
			return &backup.MemoryFlush{Attempted: true, Note: "stopped"}
		}})
	r, err := svc.Start(f.ctx, admin, backup.TriggerManual)
	must(t, err)
	waitIdle(t, svc)
	st, err := svc.Status(f.ctx)
	must(t, err)
	it := item(t, st, r.ID)
	if st.Busy || it.State != backup.StateFailed || !strings.Contains(it.Error, "stage memory_flush") || !strings.Contains(it.Error, "limit") {
		t.Fatalf("busy %v item %+v", st.Busy, it)
	}
}
