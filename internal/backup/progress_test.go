package backup

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/clock"
)

// The watchdog leaves a slow stage alone while it makes progress (bytes moved through a tracked reader) and stops it
// once it goes quiet, naming the stage.
func TestWatchProgressAndStall(t *testing.T) {
	t.Parallel()
	tr := newTracker("bkp_x", clock.System{}, slog.New(slog.DiscardHandler))
	tr.enter(StageObjects)
	ctx, stop := watch(withTracker(context.Background(), tr), tr, 200*time.Millisecond, time.Hour)
	defer stop()
	r := trackerFrom(ctx).reader(strings.NewReader(strings.Repeat("x", 64)))
	buf := make([]byte, 1)
	for range 10 { // 10 × 50 ms of reads: longer than the stall limit in total, never quiet for it
		time.Sleep(50 * time.Millisecond)
		if _, err := r.Read(buf); err != nil {
			t.Fatal(err)
		}
		if ctx.Err() != nil {
			t.Fatalf("stopped while progressing: %v", context.Cause(ctx))
		}
	}
	select {
	case <-ctx.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("a quiet stage was not stopped")
	}
	se := stalled(ctx)
	if se == nil || se.Stage != StageObjects || se.Total || !errors.Is(context.Cause(ctx), error(se)) {
		t.Fatalf("cause: %v", context.Cause(ctx))
	}
	if p := tr.progress(); p.Stage != StageObjects || p.ID != "bkp_x" {
		t.Fatalf("progress: %+v", p)
	}
}

// Stopping the watchdog does not report a stall.
func TestWatchStopIsNotAStall(t *testing.T) {
	t.Parallel()
	tr := newTracker("bkp_x", clock.System{}, slog.New(slog.DiscardHandler))
	ctx, stop := watch(context.Background(), tr, time.Millisecond, time.Hour)
	stop()
	<-ctx.Done()
	if stalled(ctx) != nil {
		t.Fatalf("cause: %v", context.Cause(ctx))
	}
}

// ReportProgress (a node answering the memory flush) moves the tracker's last progress, for the status and the
// watchdog; outside a backup it does nothing.
func TestReportProgress(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC))
	tr := newTracker("bkp_x", clk, slog.New(slog.DiscardHandler))
	tr.enter(StageMemoryFlush)
	before := tr.progress().LastProgressAt
	clk.Advance(time.Minute)
	time.Sleep(5 * time.Millisecond)
	_, idle := tr.idle()
	ReportProgress(withTracker(context.Background(), tr))
	if p := tr.progress(); !p.LastProgressAt.Equal(before.Add(time.Minute)) || p.Stage != StageMemoryFlush {
		t.Fatalf("progress: %+v (before %s)", p, before)
	}
	if _, d := tr.idle(); d >= idle {
		t.Fatalf("idle %s, was %s", d, idle)
	}
	ReportProgress(context.Background())
}
