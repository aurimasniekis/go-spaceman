package api

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// More catch-ups than slots: at most the bound run at once, the rest wait their bounded time and are then refused
// with 503 and Retry-After; a slot given back is taken by a waiting one (ADR-0081).
func TestCatchUpSlotsBoundConcurrentReads(t *testing.T) {
	const slots, readers = 4, 24
	d := &Deps{CatchUps: make(chan struct{}, slots), CatchUpWait: 100 * time.Millisecond}
	var running, peak, done, refused atomic.Int32
	hold := make(chan struct{})
	var wg sync.WaitGroup
	for range readers {
		wg.Go(func() {
			release, ok := d.acquireCatchUp(context.Background())
			if !ok {
				refused.Add(1)
				return
			}
			n := running.Add(1)
			for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
			}
			<-hold
			running.Add(-1)
			done.Add(1)
			release()
		})
	}
	// Every reader beyond the slots gives up after the wait while the slots are held.
	deadline := time.Now().Add(5 * time.Second)
	for refused.Load() < readers-slots && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := refused.Load(); got != readers-slots {
		t.Fatalf("%d refused, want %d", got, readers-slots)
	}
	close(hold)
	wg.Wait()
	if peak.Load() != slots || done.Load() != slots {
		t.Fatalf("peak %d, done %d", peak.Load(), done.Load())
	}

	// A waiting reader gets a slot given back within its wait.
	r1, _ := d.acquireCatchUp(context.Background())
	for range slots - 1 {
		_, _ = d.acquireCatchUp(context.Background())
	}
	time.AfterFunc(20*time.Millisecond, r1)
	if _, ok := d.acquireCatchUp(context.Background()); !ok {
		t.Fatal("a slot given back was not taken")
	}
	// A cancelled request stops waiting at once.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, ok := d.acquireCatchUp(ctx); ok {
		t.Fatal("a cancelled request got a slot")
	}
	p := catchUpBusy()
	if p.Status != http.StatusServiceUnavailable || p.headers.Get("Retry-After") == "" {
		t.Fatalf("refusal %+v", p)
	}
	// No bound configured: every reader proceeds.
	if _, ok := (&Deps{}).acquireCatchUp(context.Background()); !ok {
		t.Fatal("unbounded")
	}
}
