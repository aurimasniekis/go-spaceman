package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/harness"
	"github.com/aurimasniekis/go-spaceman/internal/obs"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// Intake of Claude Code's StopFailure hook and status-line wrapper (plan §3.3). Both arrive on the bridge socket,
// already authenticated with the run token and checked for an active run by bridgeCall. What arrives is re-validated
// here: the socket is writable by every process of the run, so a snapshot is only kept when it passes the same typed
// allowlist the sender applies. Nothing here logs or stores text from Claude Code.

// stopScanGap is the shortest time between two StopFailure-started scans of one run.
const stopScanGap = 2 * time.Second

// statusMaxAge is how old a status-line snapshot may be and still fill a limit's reset: the wrapper refreshes every
// minute while Claude Code runs, so an older one belongs to a window that may be over or to an earlier limit.
const statusMaxAge = 5 * time.Minute

// StatusObservation is a run's latest status-line snapshot and when the node received it.
type StatusObservation struct {
	harness.StatusSnapshot
	ObservedAt time.Time `json:"observed_at"`
}

func (a *Agent) stopFailureIntake(ctx context.Context, req bridgeRequest) bridgeResponse {
	var f harness.StopFailure
	if json.Unmarshal(req.Args, &f) != nil || !f.Valid() {
		return bridgeResponse{Error: "bad stop-failure data"}
	}
	// A non-limit class is a run health signal; the limit itself is only ever opened by the transcript scan.
	if f.Error != "rate_limit" {
		a.mu.Lock()
		if a.failureClasses == nil {
			a.failureClasses = map[string]string{}
		}
		changed := a.failureClasses[req.Run] != f.Error
		a.failureClasses[req.Run] = f.Error
		a.mu.Unlock()
		if changed {
			defer a.goTrack(func() { a.reportHookData(context.WithoutCancel(ctx), req.Run) })
		}
	}
	// The run may have ended between the bridge's check and the stores above; its data was dropped then, so drop it again.
	if r, err := a.j.getRun(ctx, req.Run); err != nil || ended(r.State) {
		a.forgetHookData(req.Run)
		return bridgeResponse{OK: true}
	}
	a.log.Debug("claude stop failure", slog.String(obs.FieldRunID, req.Run), slog.String("class", f.Error))
	// The accelerator: scan now instead of after the settle delay.
	// Coalesced per run: a process of the run can send these as fast as it likes, and every scan holds the node-wide scan
	// lock, so at most one scan per run starts in each stopScanGap. It runs in the background, not in the socket's answer.
	now := a.now()
	a.mu.Lock()
	if last, ok := a.stopScans[req.Run]; ok && now.Sub(last) < stopScanGap {
		a.mu.Unlock()
		return bridgeResponse{OK: true}
	}
	if a.stopScans == nil {
		a.stopScans = map[string]time.Time{}
	}
	a.stopScans[req.Run] = now
	a.mu.Unlock()
	if r, err := a.j.getRun(ctx, req.Run); err != nil || ended(r.State) {
		a.forgetHookData(req.Run)
		return bridgeResponse{OK: true}
	}
	a.goTrack(func() { a.limitScan(context.WithoutCancel(ctx), req.Run, false) })
	return bridgeResponse{OK: true}
}

func (a *Agent) statusLineIntake(ctx context.Context, req bridgeRequest) bridgeResponse {
	var s harness.StatusSnapshot
	if json.Unmarshal(req.Args, &s) != nil || !s.Valid() {
		return bridgeResponse{Error: "bad status-line data"}
	}
	a.mu.Lock()
	if a.statusSnaps == nil {
		a.statusSnaps = map[string]StatusObservation{}
	}
	prev := a.statusSnaps[req.Run]
	a.statusSnaps[req.Run] = StatusObservation{StatusSnapshot: s, ObservedAt: a.now()}
	a.mu.Unlock()
	// The run may have ended between the bridge's check and the store above; its data was dropped then, so drop it again.
	r, err := a.j.getRun(ctx, req.Run)
	if err != nil || ended(r.State) {
		a.forgetHookData(req.Run)
		return bridgeResponse{OK: true}
	}
	// An open limit still without a reset time gets the status line's, now rather than at the next report.
	if l := r.Limit.wire(a.now()); l != nil && !l.Cleared && l.ResetsAt == nil && a.fillChanged(l, prev.StatusSnapshot, s) {
		a.goTrack(func() { a.reportHookData(context.WithoutCancel(ctx), req.Run) })
	}
	return bridgeResponse{OK: true}
}

// StatusSnapshot returns the latest status-line snapshot of a run.
func (a *Agent) StatusSnapshot(runID string) (StatusObservation, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	o, ok := a.statusSnaps[runID]
	return o, ok
}

// FailureClass returns the latest non-limit StopFailure class of a run ("" when none).
func (a *Agent) FailureClass(runID string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.failureClasses[runID]
}

func (a *Agent) forgetHookData(runID string) {
	a.mu.Lock()
	delete(a.statusSnaps, runID)
	delete(a.failureClasses, runID)
	delete(a.stopScans, runID)
	a.mu.Unlock()
}

// statusLineReset is the reset time the status line's rate-limit windows give a limit of the given kind: the window
// that is full (≥ 100 % used) and, for the wrap-up path's unknown kind, the later of the two. Nil when there is none. The
// bound on the value (Unix seconds in 1e9–4e9) was applied when the snapshot was accepted; proto.RunLimit.Normalized
// applies the limit's own bounds (after since, within MaxLimitReset) before anything is sent or stored.
func statusLineReset(kind string, s harness.StatusSnapshot) *time.Time {
	var cands []*harness.Window
	switch kind {
	case proto.LimitFiveHour:
		cands = []*harness.Window{s.FiveHour}
	case proto.LimitSevenDay:
		cands = []*harness.Window{s.SevenDay}
	case proto.LimitUnknown:
		cands = []*harness.Window{s.FiveHour, s.SevenDay}
	}
	var best int64
	for _, w := range cands {
		if w != nil && w.UsedPct >= 100 && w.ResetsAt > best {
			best = w.ResetsAt
		}
	}
	if best == 0 {
		return nil
	}
	t := time.Unix(best, 0).UTC()
	return &t
}

// fillChanged reports whether the new snapshot gives the open limit l a reset (one that survives the limit's bounds)
// that the previous one did not, so a status line refreshing every minute does not repeat the same report.
func (a *Agent) fillChanged(l *proto.RunLimit, prev, next harness.StatusSnapshot) bool {
	fill := func(s harness.StatusSnapshot) *time.Time {
		r := statusLineReset(l.Kind, s)
		if r == nil {
			return nil
		}
		c := *l
		c.ResetsAt, c.Source = r, proto.LimitSourceStatusLine
		if n, ok := c.Normalized(a.now()); ok {
			return n.ResetsAt
		}
		return nil
	}
	n, p := fill(next), fill(prev)
	return n != nil && (p == nil || !p.Equal(*n))
}

// withHookData adds what the hooks gave the node to a run's report (minor ≥ 13; ForMinor strips it below): the
// non-limit StopFailure class, and, on an open limit whose transcript record carried no reset time, the reset the status
// line's window gives (source statusline). A reset the transcript gave is never replaced. The limit is copied, never
// changed in place: it points into the journal row.
func (a *Agent) withHookData(rep proto.RunReport) proto.RunReport {
	a.mu.Lock()
	class, held := a.failureClasses[rep.RunID]
	rep.Failure = proto.FailureClass(class)
	// A held empty class is a failure that ended (the run started a new turn): central is told to forget it.
	rep.FailureCleared = held && class == ""
	snap, haveSnap := a.statusSnaps[rep.RunID]
	a.mu.Unlock()
	if l := rep.Limit; l != nil && !l.Cleared && l.ResetsAt == nil && haveSnap && a.now().Sub(snap.ObservedAt) <= statusMaxAge {
		if r := statusLineReset(l.Kind, snap.StatusSnapshot); r != nil {
			filled := *l
			filled.ResetsAt, filled.Source = r, proto.LimitSourceStatusLine
			// A reset outside the limit's bounds is dropped, with its source, as central would.
			if n, ok := filled.Normalized(a.now()); ok && n.ResetsAt != nil {
				rep.Limit = &n
			}
		}
	}
	return rep
}

// reportHookData sends a run's report again when a hook gave it something to say: a StopFailure class (when it
// changed), or a status-line window that fills the reset of its open limit. Central stores a repeat without writing.
func (a *Agent) reportHookData(ctx context.Context, runID string) {
	if !a.protocolAtLeast(proto.MinorAccountUsage) {
		return
	}
	r, err := a.j.getRun(ctx, runID)
	if err != nil || ended(r.State) {
		return
	}
	a.report(ctx, r)
}

// failureEnded handles a run's working status. On a new turn (status just changed to working) it forgets the failure
// class and keeps an empty entry so the next reports tell central it is over (RunReport.FailureCleared). When the status
// was already working and the node holds no entry at all (it restarted, so a class central stored earlier is unknown to
// it), it keeps the same empty entry, so the first working report after the start clears the stored class; central makes
// a repeated clear a no-op. A held class is kept: a StopFailure that arrives while the status is already working (late)
// belongs to the turn in progress, so it stays until the next turn starts. It runs before the report of that status.
func (a *Agent) failureEnded(runID string, newTurn bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.failureClasses == nil {
		a.failureClasses = map[string]string{}
	}
	if _, held := a.failureClasses[runID]; newTurn || !held {
		a.failureClasses[runID] = ""
	}
}

// reportAllHookData sends the reports of the runs that hold hook data, after a welcome that settled on minor 13 (the
// hello is built before the minor is known and carries none of it).
func (a *Agent) reportAllHookData(ctx context.Context) {
	a.mu.Lock()
	ids := map[string]bool{}
	for id := range a.failureClasses {
		ids[id] = true
	}
	for id := range a.statusSnaps {
		ids[id] = true
	}
	a.mu.Unlock()
	for id := range ids {
		a.reportHookData(ctx, id)
	}
}
