package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/obs"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/runner"
)

// Run terminal sizes on the node (ADR-0075). herdr-core takes no size when it starts a program and resizes a program
// only while an interactive attach is open; when that attach closes the program goes back to the core's headless size
// (docs/contracts/herdr-core.md S-6, S-7). So central sends the complete set of the node's sized runs
// (terminal.sizes), and the node holds each one's terminal through an interactive attach on a connection of its own
// (runner.Takeover), resized to the run's size. The hold never writes: nothing typed reaches the program through it.
// It is closed when the size is cleared, the run ends, the space's lease is revoked or expires, and when the agent
// stops.

// holdRetry is how long a hold whose attach failed waits before the node tries again (while the size is still wanted).
var holdRetry = 2 * time.Second

type sizeHold struct {
	want   proto.TerminalSize
	ready  chan struct{} // closed once the attach succeeded or failed
	stream runner.Stream
	cancel context.CancelFunc
}

// applyTerminalSizes reconciles the node's holds with central's complete set.
func (a *Agent) applyTerminalSizes(ctx context.Context, st proto.TerminalSizes) {
	want := map[string]proto.TerminalSize{}
	for _, s := range st.Sizes {
		if proto.CheckTerminalSize(s.Cols, s.Rows) == nil {
			want[s.RunID] = s
		}
	}
	a.mu.Lock()
	a.sizeWant = want
	a.mu.Unlock()
	a.reconcileHolds(ctx)
}

// reconcileHolds opens, resizes and closes holds so they match the wanted sizes. A wanted run whose terminal is not
// live yet is held once it is (observe calls this again).
func (a *Agent) reconcileHolds(ctx context.Context) {
	a.mu.Lock()
	var stop []*sizeHold
	var resize []*sizeHold
	var start []proto.TerminalSize
	for runID, h := range a.holds {
		w, ok := a.sizeWant[runID]
		switch {
		case !ok || w.Generation != h.want.Generation || w.SpaceID != h.want.SpaceID:
			stop = append(stop, h)
			delete(a.holds, runID)
		case w.Cols != h.want.Cols || w.Rows != h.want.Rows:
			h.want = w
			resize = append(resize, h)
		}
	}
	for runID, w := range a.sizeWant {
		if _, ok := a.holds[runID]; !ok {
			start = append(start, w)
		}
	}
	a.mu.Unlock()
	for _, h := range stop {
		a.releaseHold(h)
	}
	for _, h := range resize {
		a.resizeHold(h)
	}
	for _, w := range start {
		a.startHold(ctx, w)
	}
}

// releaseHold closes a hold: the terminal goes back to the node's default size. The recording notes that size first,
// so the program's redraw for it follows the record (the runner's size event arrives on another connection, usually
// after the redraw, and then repeats it). A takeover still open would keep the terminal at the held size (herdr-core
// sizes a terminal by its newest interactive attach), so it is resized to the default too.
func (a *Agent) releaseHold(h *sizeHold) {
	if a.o.DefaultCols > 0 && a.o.DefaultRows > 0 {
		a.recordSize(h.want.RunID, a.o.DefaultCols, a.o.DefaultRows)
	}
	h.cancel()
	if a.o.DefaultCols > 0 && a.o.DefaultRows > 0 {
		a.resizeTakeover(h.want.RunID, a.o.DefaultCols, a.o.DefaultRows)
	}
}

// resizeHold applies a hold's new size: the recording notes it first, so the output drawn for it follows the record.
func (a *Agent) resizeHold(h *sizeHold) {
	select {
	case <-h.ready:
	default:
		return // still attaching: the attach resizes to h.want when it opens
	}
	if h.stream == nil {
		return
	}
	a.mu.Lock()
	w := h.want
	a.mu.Unlock()
	a.recordSize(w.RunID, w.Cols, w.Rows)
	if err := h.stream.Resize(w.Cols, w.Rows); err != nil {
		a.log.Warn("terminal resize failed", slog.String(obs.FieldRunID, w.RunID), obs.Err(err))
	}
	a.resizeTakeover(w.RunID, w.Cols, w.Rows)
}

// resizeTakeover resizes the run's open takeover attach, if any, so it does not keep an older size.
func (a *Agent) resizeTakeover(runID string, cols, rows int) {
	a.mu.Lock()
	t := a.takeovers[runID]
	a.mu.Unlock()
	if t == nil {
		return
	}
	select {
	case <-t.ready:
	default:
		return
	}
	if t.stream != nil {
		_ = t.stream.Resize(cols, rows)
	}
}

func (a *Agent) startHold(ctx context.Context, w proto.TerminalSize) {
	if _, ok := a.lease(w.SpaceID, w.Generation); !ok {
		return // fenced: central resends the set once the lease is granted again
	}
	r, err := a.j.getRun(ctx, w.RunID)
	if err != nil || r.SpaceID != w.SpaceID || r.Generation != w.Generation || r.TerminalID == "" || ended(r.State) {
		return // not live yet (observe retries), or not this node's run
	}
	log := a.log.With(slog.String(obs.FieldRunID, w.RunID))
	hctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	h := &sizeHold{want: w, ready: make(chan struct{}), cancel: cancel}
	a.mu.Lock()
	if a.holds == nil {
		a.holds = map[string]*sizeHold{}
	}
	if _, ok := a.holds[w.RunID]; ok {
		a.mu.Unlock()
		cancel()
		return
	}
	a.holds[w.RunID] = h
	a.mu.Unlock()
	go func() {
		st, _, err := a.o.Runner.Takeover(hctx, r.TerminalID)
		if err != nil {
			close(h.ready)
			if hctx.Err() == nil {
				log.Warn("terminal size attach failed", obs.Err(err))
				a.dropHold(h, true)
			}
			return
		}
		h.stream = st
		close(h.ready)
		defer func() { _ = st.Close() }()
		go func() {
			<-hctx.Done()
			_ = st.Close()
		}()
		// From here on resizeHold applies changes; one made while the attach was opening is caught by applying until
		// the wanted size is the one applied.
		var applied proto.TerminalSize
		for {
			a.mu.Lock()
			w := h.want
			a.mu.Unlock()
			if w.Cols == applied.Cols && w.Rows == applied.Rows {
				break
			}
			a.recordSize(w.RunID, w.Cols, w.Rows)
			if err := st.Resize(w.Cols, w.Rows); err != nil {
				log.Warn("terminal resize failed", obs.Err(err))
			}
			a.resizeTakeover(w.RunID, w.Cols, w.Rows)
			applied = w
		}
		// The hold only keeps the attach open: output is drained (the recorder's own attach records it) and nothing
		// is ever written to it.
		for {
			_, closed, err := st.Next(hctx)
			if hctx.Err() != nil {
				return // released
			}
			if closed != nil {
				a.dropHold(h, false) // the run ended
				return
			}
			if err != nil {
				log.Warn("terminal size attach closed", obs.Err(err))
				a.dropHold(h, true)
				return
			}
		}
	}()
}

// dropHold forgets a hold if it is still the run's current one; with retry the node tries again after holdRetry
// while the size is still wanted (the attach failed or its connection to the core dropped).
func (a *Agent) dropHold(h *sizeHold, retry bool) {
	a.mu.Lock()
	if cur := a.holds[h.want.RunID]; cur == h {
		delete(a.holds, h.want.RunID)
	}
	if !retry {
		delete(a.sizeWant, h.want.RunID)
	}
	a.mu.Unlock()
	h.cancel()
	if retry {
		time.AfterFunc(holdRetry, func() {
			a.mu.Lock()
			stopping := a.holdsStopped
			a.mu.Unlock()
			if !stopping {
				a.reconcileHolds(a.mem.base)
			}
		})
	}
}

// holdWanted reports whether a run has a wanted size the node does not hold yet (observe then opens the hold).
func (a *Agent) holdWanted(runID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, want := a.sizeWant[runID]
	_, held := a.holds[runID]
	return want && !held
}

// stopSpaceHolds closes the holds of a space whose lease was revoked or expired (fencing). Central sends the set again
// with the next lease.
func (a *Agent) stopSpaceHolds(spaceID string) {
	a.mu.Lock()
	var stop []*sizeHold
	for runID, h := range a.holds {
		if h.want.SpaceID == spaceID {
			stop = append(stop, h)
			delete(a.holds, runID)
		}
	}
	for runID, w := range a.sizeWant {
		if w.SpaceID == spaceID {
			delete(a.sizeWant, runID)
		}
	}
	a.mu.Unlock()
	for _, h := range stop {
		h.cancel()
	}
}

// stopHolds closes every hold (agent shutdown).
func (a *Agent) stopHolds() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.holdsStopped = true
	for id, h := range a.holds {
		h.cancel()
		delete(a.holds, id)
	}
	a.sizeWant = nil
}

// runnerSize decodes a runner "size" event's payload ({"cols":N,"rows":M}).
func runnerSize(data []byte) (cols, rows int, ok bool) {
	var s struct {
		Cols int `json:"cols"`
		Rows int `json:"rows"`
	}
	if json.Unmarshal(data, &s) != nil || s.Cols <= 0 || s.Rows <= 0 {
		return 0, 0, false
	}
	return s.Cols, s.Rows, true
}
