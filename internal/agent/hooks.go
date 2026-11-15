package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/harness"
	"github.com/aurimasniekis/go-spaceman/internal/obs"
)

// Intake of Claude Code's StopFailure hook and status-line wrapper (plan §3.3). Both arrive on the bridge socket,
// already authenticated with the run token and checked for an active run by bridgeCall. What arrives is re-validated
// here: the socket is writable by every process of the run, so a snapshot is only kept when it passes the same typed
// allowlist the sender applies. Nothing here logs or stores text from Claude Code.

// stopScanGap is the shortest time between two StopFailure-started scans of one run.
const stopScanGap = 2 * time.Second

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
		a.failureClasses[req.Run] = f.Error
		a.mu.Unlock()
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
	a.statusSnaps[req.Run] = StatusObservation{StatusSnapshot: s, ObservedAt: a.now()}
	a.mu.Unlock()
	// The run may have ended between the bridge's check and the store above; its data was dropped then, so drop it again.
	if r, err := a.j.getRun(ctx, req.Run); err != nil || ended(r.State) {
		a.forgetHookData(req.Run)
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
