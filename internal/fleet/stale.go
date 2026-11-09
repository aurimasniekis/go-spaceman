package fleet

import (
	"context"
	"fmt"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/obs/metrics"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// The stale-run sweeper (ADR-0074) ends runs that central still counts as live but whose process is gone, so they free
// their space and node slots without an operator. It only changes central's record: it never sends a stop or a kill,
// so it cannot end a process that is alive. It acts on one of two authoritative signals, each after a grace period:
//
//   - Not on the node (stale_not_on_node). A connected node's hello reports every run in its journal, and central
//     applies those reports before anything else. A run of that node that no accepted report has touched since the
//     connection began, staleRunGrace ago, is not in the node's journal: the node agent restarted and the process did
//     not survive, or the launch never reached it. A run created after the connection began is never a candidate.
//   - Lease lost (stale_lease_lost). The node is not connected, and the space's lease on that node expired more than
//     the disconnect grace plus staleRunGrace ago. The node's lease guard stops a space's runs once its lease and the
//     disconnect grace pass without central (SM-NODE-007), so the process is gone.
//
// Pending runs are never candidates (their launch may still be in flight), nor are runs whose space now belongs to
// another node (relocation ends those). Each run ends in its own transaction, only if it is unchanged since it was
// chosen (same version), through the same path as a node's report of "lost": its credentials are revoked, a run_ended
// event is published, its task is released (OnRunEnded), and an audit record names the reason. Sweeping is idempotent:
// an ended run is no longer a candidate, and a central restart mid-sweep just finds the remaining ones again.

// staleRunGrace is how long the sweeper waits past its signal before it ends a run: after a node connected (its hello
// reports are applied within the connection's first moments), and after a lost lease and the disconnect grace (time
// for the node's lease guard to finish stopping the runs).
const staleRunGrace = 2 * time.Minute

// End reasons of runs the stale-run sweeper ended (their observed state is lost).
const (
	EndStaleNotOnNode = string(metrics.StaleNotOnNode)
	EndStaleLeaseLost = string(metrics.StaleLeaseLost)
)

// staleRun is a run the sweeper would end, when, and why.
type staleRun struct {
	db.ListStaleRunCandidatesRow
	Reason string
	Due    time.Time
}

// staleRuns lists the runs with a stale-run signal, each with the time it becomes due.
func (svc *Service) staleRuns(ctx context.Context) ([]staleRun, error) {
	rows, err := svc.s.R().ListStaleRunCandidates(ctx)
	if err != nil {
		return nil, err
	}
	out := []staleRun{}
	for _, r := range rows {
		c := staleRun{ListStaleRunCandidatesRow: r}
		if since, ok := svc.hub.connectedSince(r.NodeID); ok {
			if !time.UnixMicro(r.LastObservedAt).Before(since) {
				continue // reported (or created) during this connection
			}
			c.Reason, c.Due = EndStaleNotOnNode, since.Add(svc.opts.StaleRunGrace)
		} else {
			if r.LeaseNodeID != r.NodeID || r.LeaseExpiresAt == 0 {
				continue // no lease of this node to have missed
			}
			c.Reason, c.Due = EndStaleLeaseLost, time.UnixMicro(r.LeaseExpiresAt).Add(svc.opts.DisconnectGrace+svc.opts.StaleRunGrace)
		}
		out = append(out, c)
	}
	return out, nil
}

// NextStaleRunSweep is when the stale-run sweep is next due (zero: no run has a stale-run signal). The deadline timer
// calls it; connects and disconnects wake the timer.
func (svc *Service) NextStaleRunSweep(ctx context.Context) (time.Time, error) {
	runs, err := svc.staleRuns(ctx)
	if err != nil {
		return time.Time{}, err
	}
	var next time.Time
	for _, r := range runs {
		if next.IsZero() || r.Due.Before(next) {
			next = r.Due
		}
	}
	return next, nil
}

// SweepStaleRuns ends the runs whose stale-run signal has been due for its grace (ADR-0074) and returns how many it
// ended. The deadline timer calls it on every round, so it only reads unless a run is due.
func (svc *Service) SweepStaleRuns(ctx context.Context) (int, error) {
	runs, err := svc.staleRuns(ctx)
	if err != nil {
		return 0, err
	}
	now, n := svc.now(), 0
	for i := range runs {
		r := &runs[i]
		if now.Before(r.Due) {
			continue
		}
		run, err := svc.s.R().GetRun(ctx, r.ID)
		if err != nil {
			return n, err
		}
		rep := proto.RunReport{RunID: run.ID, SpaceID: run.SpaceID, Generation: run.Generation, State: proto.RunLost, TerminalID: run.TerminalID,
			CoreID: run.CoreID, Identity: run.Identity, PID: int(run.Pid), Reason: r.Reason, Error: staleRunError(r.Reason), Status: run.Status,
			StatusKind: run.StatusKind, Turn: uint64(max(run.Turn, 0)), Adopted: run.Adopted == 1, SessionID: run.SessionID}
		skipped, err := svc.applyRunReport(ctx, run.NodeID, rep, r)
		if err != nil {
			return n, err
		}
		if !skipped {
			n++
		}
	}
	return n, nil
}

// staleRunError is the explanation shown on a run the sweeper ended.
func staleRunError(reason string) string {
	if reason == EndStaleLeaseLost {
		return "central marked this run lost: its node stayed disconnected past the space's lease and the disconnect grace, " +
			"so the node's lease guard has stopped it"
	}
	return "central marked this run lost: its node reconnected and did not report it (the node agent restarted and the process " +
		"did not survive, or the launch never reached the node)"
}

// auditStaleRun records, in the run's end transaction, that the sweeper ended it.
func (svc *Service) auditStaleRun(ctx context.Context, tx *store.Tx, r db.Run, sweep *staleRun) error {
	_, err := svc.audit.Append(ctx, tx, audit.Event{Actor: audit.Actor{Type: audit.ActorSystem, ID: "stale-runs"}, Action: "run.reconciled_stale",
		Target: audit.Target{Type: "run", ID: r.ID}, Details: map[string]any{"space_id": r.SpaceID, "node_id": r.NodeID, "reason": sweep.Reason,
			"previous_state": r.Observed, "last_observed_at": time.UnixMicro(sweep.LastObservedAt).UTC().Format(time.RFC3339), "due_at": sweep.Due.Format(time.RFC3339),
			"summary": fmt.Sprintf("%s run %s (%s) was %s", r.Kind, r.ID, r.Role, r.Observed)}})
	return err
}
