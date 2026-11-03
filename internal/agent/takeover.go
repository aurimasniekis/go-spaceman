package agent

import (
	"context"
	"errors"
	"log/slog"

	"github.com/aurimasniekis/go-spaceman/internal/obs"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/runner"
)

// Terminal takeover on the node (SM-UI-004, ADR-0046). Central sends the complete set of the node's active takeovers
// (takeover.state); for each one the node opens an interactive attach to the run's terminal (runner.Takeover) and
// types the holder's keystrokes (takeover.input) into it. While a run is taken over the node refuses run.input for it,
// so no automated input reaches the terminal even from commands queued before the takeover began. Keystrokes are
// never journaled or logged.

type takeover struct {
	id     string
	runID  string
	ready  chan struct{} // closed once the attach succeeded or failed
	stream runner.Stream
	cancel context.CancelFunc
}

// takenOver reports whether a run's terminal is taken over (automated input is refused).
func (a *Agent) takenOver(runID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, ok := a.takeovers[runID]
	return ok
}

// applyTakeovers reconciles the node's takeovers with central's complete set.
func (a *Agent) applyTakeovers(ctx context.Context, st proto.TakeoverState) {
	want := map[string]proto.Takeover{}
	for _, t := range st.Takeovers {
		want[t.RunID] = t
	}
	a.mu.Lock()
	var stop []*takeover
	for runID, cur := range a.takeovers {
		if w, ok := want[runID]; !ok || w.TakeoverID != cur.id {
			stop = append(stop, cur)
			delete(a.takeovers, runID)
		}
	}
	var start []proto.Takeover
	for runID, w := range want {
		if _, ok := a.takeovers[runID]; !ok {
			start = append(start, w)
		}
	}
	a.mu.Unlock()
	for _, t := range stop {
		t.cancel()
	}
	for _, w := range start {
		a.startTakeover(ctx, w)
	}
}

func (a *Agent) startTakeover(ctx context.Context, w proto.Takeover) {
	log := a.log.With(slog.String(obs.FieldRunID, w.RunID))
	fail := func(reason string) {
		_ = a.send(ctx, proto.TypeTakeoverEnded, proto.TakeoverEnded{TakeoverID: w.TakeoverID, RunID: w.RunID, Reason: reason})
	}
	if _, ok := a.lease(w.SpaceID, w.Generation); !ok {
		fail("no valid lease for this space and generation")
		return
	}
	r, err := a.j.getRun(ctx, w.RunID)
	if err != nil || r.SpaceID != w.SpaceID || r.TerminalID == "" || ended(r.State) {
		fail("the run has no live terminal on this node")
		return
	}
	// Mark the run taken over at once, so run.input commands arriving while the attach opens are refused too.
	tctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	t := &takeover{id: w.TakeoverID, runID: w.RunID, ready: make(chan struct{}), cancel: cancel}
	a.mu.Lock()
	if a.takeovers == nil {
		a.takeovers = map[string]*takeover{}
	}
	a.takeovers[w.RunID] = t
	a.mu.Unlock()
	go func() {
		st, _, err := a.o.Runner.Takeover(tctx, r.TerminalID)
		if err != nil {
			close(t.ready)
			log.Warn("takeover attach failed", obs.Err(err))
			a.dropTakeover(t)
			fail("could not attach to the terminal")
			return
		}
		t.stream = st
		close(t.ready)
		defer func() { _ = st.Close() }()
		go func() {
			<-tctx.Done()
			_ = st.Close()
		}()
		// Output is drained (the recorder's own attach records it); the stream ends when the terminal ends, the
		// takeover is released, or the connection to the core fails.
		for {
			_, closed, err := st.Next(tctx)
			if tctx.Err() != nil {
				return // released
			}
			if closed != nil {
				a.dropTakeover(t) // the terminal ended: the run's report ends the takeover centrally
				return
			}
			if err != nil {
				a.dropTakeover(t)
				fail("the terminal's attach closed")
				return
			}
		}
	}()
}

// dropTakeover forgets a takeover if it is still the run's current one.
func (a *Agent) dropTakeover(t *takeover) {
	a.mu.Lock()
	if cur := a.takeovers[t.runID]; cur == t {
		delete(a.takeovers, t.runID)
	}
	a.mu.Unlock()
	t.cancel()
}

// takeoverInput types the holder's keystrokes when they belong to the run's current takeover.
func (a *Agent) takeoverInput(in proto.TakeoverInput) {
	if len(in.Data) == 0 || len(in.Data) > proto.MaxTakeoverInput {
		return
	}
	a.mu.Lock()
	t := a.takeovers[in.RunID]
	a.mu.Unlock()
	if t == nil || t.id != in.TakeoverID {
		return
	}
	select {
	case <-t.ready:
	default:
		return // still attaching: the session's reader never waits on the core
	}
	if t.stream == nil {
		return
	}
	if err := t.stream.Write(in.Data); err != nil && !errors.Is(err, context.Canceled) {
		a.log.Warn("takeover input not delivered", slog.String(obs.FieldRunID, in.RunID), obs.Err(err)) // never the bytes
	}
}

// stopTakeovers detaches every takeover (agent shutdown).
func (a *Agent) stopTakeovers() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for id, t := range a.takeovers {
		t.cancel()
		delete(a.takeovers, id)
	}
}
