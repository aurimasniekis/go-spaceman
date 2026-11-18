package agent

import (
	"context"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/obs"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// capsCheck is how often capsLoop compares the clock with the last report (tests shorten it; the interval itself
// follows the agent's clock).
var capsCheck = time.Minute

// detectCapabilities is the detection takeCapabilities runs (tests replace it to order concurrent detections).
var detectCapabilities = (*Agent).capabilities

// takeCapabilities detects the inventory again and caches it. It returns the cached inventory and its generation:
// detections may overlap (a session's hello and capsLoop), and one that finishes after a detection that started later
// is discarded, returning that newer inventory instead.
func (a *Agent) takeCapabilities(ctx context.Context) (proto.Capabilities, uint64) {
	a.mu.Lock()
	a.capsStarts++
	start := a.capsStarts
	a.mu.Unlock()
	c := detectCapabilities(a, ctx)
	a.mu.Lock()
	defer a.mu.Unlock()
	if start < a.capsTaken {
		return a.caps, a.capsGen
	}
	a.caps, a.capsAt, a.capsTaken = c, a.now(), start
	a.capsGen++
	return c, a.capsGen
}

// capsLoop keeps central's copy of the capabilities current between hellos (ADR-0077, minor ≥ 11): it detects them
// again and re-reports them when the managed Claude Code cache or a channel target changes, and once the last
// detection is proto.NodeCapabilitiesInterval old (unmanaged harnesses included, so a binary updated outside SpaceMan
// is reported). While disconnected it only refreshes the cache the next hello sends.
func (a *Agent) capsLoop(ctx context.Context) {
	var changes <-chan struct{}
	if m := a.o.Claude.Dist; m != nil {
		changes = m.Changes()
	}
	t := time.NewTicker(capsCheck)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-changes:
		case <-t.C:
			a.mu.Lock()
			// Nothing detected yet: the first session's own detection (before its hello) takes it, not the loop.
			due := !a.capsAt.IsZero() && a.now().Sub(a.capsAt) >= proto.NodeCapabilitiesInterval
			a.mu.Unlock()
			if !due {
				continue
			}
		}
		c, gen := a.takeCapabilities(ctx)
		a.reportCapabilities(ctx, c, gen)
	}
}

// capsAfterHello re-reports the capabilities when they were taken again after the connection's hello was built (a
// change between that and central's welcome is not lost until the next interval).
func (a *Agent) capsAfterHello(ctx context.Context, helloGen uint64) {
	a.mu.Lock()
	c, gen := a.caps, a.capsGen
	a.mu.Unlock()
	if capsAfterHelloRead != nil {
		capsAfterHelloRead(c)
	}
	if gen != helloGen {
		a.reportCapabilities(ctx, c, gen)
	}
}

// capsAfterHelloRead, when set, runs in capsAfterHello between reading the cache and reporting it (tests hold the
// goroutine there to let capsLoop report a newer generation first).
var capsAfterHelloRead func(proto.Capabilities)

// reportCapabilities sends generation gen of the capabilities unless that generation, or a newer one, was already
// sent (a hello carries its own). The check and the send are one step under capsSendMu, so a report that read an older
// generation and was slow to send can't land after a newer one and leave central with stale capabilities.
func (a *Agent) reportCapabilities(ctx context.Context, c proto.Capabilities, gen uint64) {
	a.capsSendMu.Lock()
	defer a.capsSendMu.Unlock()
	a.mu.Lock()
	sent := a.capsSent
	a.mu.Unlock()
	if gen <= sent || !a.protocolAtLeast(proto.MinorHarnessVersion) {
		return
	}
	if a.sendCapabilities(ctx, c) {
		a.mu.Lock()
		a.capsSent = max(a.capsSent, gen)
		a.mu.Unlock()
	}
}

// sendCapabilities sends a node.capabilities report to a central that negotiated minor 11 or later; an older central
// only learns capabilities from hello.
func (a *Agent) sendCapabilities(ctx context.Context, c proto.Capabilities) bool {
	if !a.protocolAtLeast(proto.MinorHarnessVersion) {
		return false
	}
	if err := a.send(ctx, proto.TypeNodeCapabilities, proto.NodeCapabilities{Capabilities: c}); err != nil {
		a.log.Debug("capabilities report not delivered; the next hello reports them", obs.Err(err))
		return false
	}
	return true
}
