package events

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aurimasniekis/go-pulse/pkg/pulse"
)

// Halting a durable subscription must not strand an event the server delivers while the halt is in progress. With
// pulse's Subscription.Stop such an event stays unacknowledged, and the consumer started next (a paused space
// resuming, a reconfiguration) only receives it once AckWait expires; that made TestPausedSpacePolicies time out
// waiting for the catch-up. halt drains instead, so every event reaches the next consumer well before AckWait.
func TestHaltDoesNotStrandAnEventDeliveredDuringTheStop(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	cl, err := pulse.Connect(f.nats.URL, pulse.WithCreds(f.nats.Creds("spaceman")), pulse.WithName("halt-test"))
	must(t, err)
	t.Cleanup(cl.Close)
	const ackWait = 3 * time.Second
	var got atomic.Int64
	start := func() *subRun {
		t.Helper()
		ctx, stop := context.WithCancel(f.ctx)
		sub, err := cl.Subscribe(ctx, issueSubject, func(context.Context, pulse.Event) error { got.Add(1); return nil },
			pulse.Durable("halt-test"), pulse.AckWait(ackWait))
		if err != nil {
			stop()
			t.Fatal(err)
		}
		return &subRun{sub: sub, stop: stop}
	}
	run := start()
	t.Cleanup(func() { run.halt(f.ctx) })
	// The race needs the publish to land while the halt runs; vary the timing so some iterations hit it (with Stop,
	// a few in every hundred do).
	for i := range 100 {
		time.Sleep(time.Duration(i%5) * 20 * time.Millisecond)
		before := got.Load()
		published := make(chan struct{})
		go func() {
			defer close(published)
			if _, err := f.pub.Publish(f.ctx, issueSubject, issueType, issuePayload(t, "")); err != nil {
				t.Error(err)
			}
		}()
		time.Sleep(time.Duration(i%4) * 200 * time.Microsecond)
		run.halt(f.ctx)
		<-published
		run = start()
		began := time.Now()
		for got.Load() == before {
			if took := time.Since(began); took > ackWait*2/3 {
				t.Fatalf("iteration %d: the event published during the halt has not arrived after %v (AckWait %v): it was stranded", i, took, ackWait)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
}
