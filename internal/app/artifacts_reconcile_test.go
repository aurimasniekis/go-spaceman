package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/config"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// The loop runs a pass at once, again on every tick (a failed pass included), and returns when ctx ends.
func TestReconcileLoop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var n atomic.Int32
	passed := make(chan int32, 16)
	done := make(chan struct{})
	go func() {
		defer close(done)
		reconcileLoop(ctx, quietLog(), 10*time.Millisecond, func(context.Context) (fleet.ReconcileResult, error) {
			passed <- n.Add(1)
			if n.Load() == 2 {
				return fleet.ReconcileResult{}, errors.New("listing failed")
			}
			return fleet.ReconcileResult{Removed: 1}, nil
		})
	}()
	for want := int32(1); want <= 3; want++ {
		select {
		case got := <-passed:
			if got != want {
				t.Fatalf("pass %d, want %d", got, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("pass %d never ran", want)
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("loop did not stop with its context")
	}
}

// With the interval 0 (or no fleet) the sweep never starts: reconcileRecordings returns at once and touches nothing
// (a nil Fleet and Store would panic otherwise).
func TestReconcileRecordingsDisabled(t *testing.T) {
	for name, c := range map[string]*Central{
		"interval zero": {Cfg: &config.Config{}, Log: quietLog()},
		"no fleet":      {Cfg: &config.Config{Artifacts: config.ArtifactsConfig{RecordingReconcileInterval: config.Duration(time.Hour)}}, Log: quietLog()},
	} {
		done := make(chan struct{})
		go func() { defer close(done); c.reconcileRecordings(context.Background()) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: reconcileRecordings did not return", name)
		}
	}
}
