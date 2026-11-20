package events

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aurimasniekis/go-pulse/pkg/pulse"

	"github.com/aurimasniekis/go-spaceman/internal/obs/metrics/metricstest"
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

// When a halt's drain times out, the handlers' context is cancelled and the events still buffered in the consumer
// reach the handler with that cancelled context. They must not report the subscription as running (a throttled
// state) after the manager has set it to paused.
func TestDrainTimeoutDoesNotSetRunningAfterPause(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	// One per 10 s, burst 1: later events wait in throttle. RateFor runs first in every handler call, so it sees the
	// handler's context: count the calls that arrive cancelled, which is the path the guard under test handles.
	var cancelledCalls atomic.Int64
	f.svc.o.RateFor = func(ctx context.Context, _ string) int64 {
		if ctx.Err() != nil {
			cancelledCalls.Add(1)
		}
		return 6
	}
	stop, err := f.svc.CreateSubscription(f.ctx, admin, "spc_a", SubscriptionInput{Name: "stopped", ConnectionID: f.conn.ID, TemplateVersionID: f.issue.Versions[0].ID,
		Vars: map[string]string{"account_key": "example-org", "repository": "example-org/tool"}, WhilePaused: PausedStop})
	must(t, err)
	state := func() string { s, _ := f.svc.GetSubscription(f.ctx, "spc_a", stop.ID); return s.State }
	eventually(t, "running", func() bool { return state() == "running" })
	for i := range 4 {
		f.publish(issueSubject, issueType, issuePayload(t, ""), "late-"+string(rune('a'+i)))
	}
	eventually(t, "the first event admitted", func() bool { return f.outcomes("spc_a", stop.ID)[OutcomeAccepted] == 1 })
	f.svc.stateMu.Lock()
	before := f.svc.states[stop.ID]
	f.svc.stateMu.Unlock()
	f.setSpaceState("spc_a", "paused") // the halt drains: the second event holds in the throttle until drainTimeout
	// The halt reports once its consumer has closed, which is after every late handler call. The state recorded then is
	// what the halt left, before the manager writes paused: a late write shows here every time, not only when a sample
	// happens to land in the window.
	eventually(t, "the halt reported", func() bool {
		f.haltedMu.Lock()
		defer f.haltedMu.Unlock()
		return f.halted[stop.ID] != nil
	})
	f.haltedMu.Lock()
	left := f.haltState[stop.ID]
	f.haltedMu.Unlock()
	if left != before {
		t.Fatalf("a late handler call changed the state during the halt: %+v, want the state from before it (%+v)", left, before)
	}
	if n := cancelledCalls.Load(); n == 0 {
		t.Fatal("no handler call reached throttle with a cancelled context: the drain-timeout path was not exercised")
	}
	eventually(t, "paused", func() bool { return state() == "paused" })
	// The cancelled calls must not count as throttled either (deterministic).
	if n := metricstest.Value(t, f.mt, "spaceman_ingest_deliveries_total", "delivery", "durable", "origin", "live", "result", "rate_limited"); n != 0 {
		t.Fatalf("rate_limited deliveries counted from cancelled handlers: %v", n)
	}
	// Nor as errors: the handler guard skips the metric when its ctx is cancelled.
	if n := metricstest.Value(t, f.mt, "spaceman_ingest_deliveries_total", "delivery", "durable", "origin", "live", "result", "error"); n != 0 {
		t.Fatalf("error deliveries counted from cancelled handlers: %v", n)
	}
}
