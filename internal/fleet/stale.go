package fleet

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/obs/metrics"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// The stale-run sweeper (ADR-0074) ends runs that central still counts as live but whose process is gone, so they free
// their space and node slots without an operator. It only changes central's record: it never sends a stop or a kill.
// Its rule is that it must never end a run whose process exists or might still start: a run it ends wrongly is fenced
// and killed at the node's next report of it (SM-NODE-006). So when in doubt, it does not sweep. It acts on one of two
// signals, each after a grace period:
//
//   - Not on the node (stale_not_on_node). A connected node's hello reports every run in its journal. A run that was
//     live in central's record when the node connected (read in the fleet.node_connect transaction), that the hello did
//     not contain, and that nothing has changed since (same version), is not on the node: the node agent restarted and
//     the process did not survive. The grace is measured from the connection on the clock's monotonic reading, never
//     against a stored wall-clock time. A connection whose hello reports did not all apply is left alone entirely, and
//     a run created after the connection began is never a candidate.
//   - Lease lost (stale_lease_lost). The node is not connected, and the space's lease on that node expired more than
//     the disconnect grace (the larger of the one the lease was granted with and the current one) plus
//     staleLeaseMargin ago. The node's lease guard stops a space's runs once its lease and the grace pass without
//     central (SM-NODE-007), so the process is gone. The guard compares central's expiry with the node's own clock, so
//     this assumes the node's clock is synchronised (NTP); the margin covers any plausible skew. Only an active lease
//     counts: a revoke stores the revoke time as the expiry, while the node may hold the lease for up to lease_ttl
//     (config.MaxLeaseTTL, 10 minutes) longer, so the runs of a revoked lease wait for the node's report instead.
//
// A run is never a candidate while it is pending, while a launch command for it is unacknowledged (pending or sent), or
// when no node has ever reported it: its launch may still be in flight. Runs whose space now belongs to another node
// are left to relocation. Each run ends in its own transaction, only if it is unchanged since it was chosen (same
// version) and its signal still holds (the same connection for not-on-node, no connection for lease-lost), through the
// same path as a node's report of "lost": its credentials are revoked, its unacknowledged commands are cancelled, its
// desired state becomes stopped, a run_ended event is published, OnRunEnded runs, and an audit record names the reason.
// Sweeping is idempotent: an ended run is no longer a candidate, and a central restart mid-sweep finds the remaining
// ones again (on a new connection, after a new grace).

// staleRunGrace is how long after a node connected the sweeper waits before it ends the runs its hello did not report.
const staleRunGrace = 2 * time.Minute

// staleLeaseMargin is how long past a disconnected node's lease expiry plus disconnect grace the sweeper waits before
// it ends the space's runs on that node: time for the node's lease guard to finish stopping them, and far more than any
// plausible clock skew between central and a node whose clock is synchronised.
const staleLeaseMargin = 15 * time.Minute

// End reasons of runs the stale-run sweeper ended (their observed state is lost).
const (
	EndStaleNotOnNode = string(metrics.StaleNotOnNode)
	EndStaleLeaseLost = string(metrics.StaleLeaseLost)
)

// staleRun is a run the sweeper would end, when, and why.
type staleRun struct {
	db.ListStaleRunCandidatesRow
	Reason string
	// Due is when the run becomes due on the wall clock (for the deadline timer and the audit record); ready says
	// whether it is due now, decided on the signal's own basis.
	Due   time.Time
	ready bool
	// session is the connection whose hello did not report the run (stale_not_on_node).
	session *session
}

// staleRuns lists the runs with a stale-run signal, each with the time it becomes due.
func (svc *Service) staleRuns(ctx context.Context) ([]staleRun, error) {
	rows, err := svc.s.R().ListStaleRunCandidates(ctx)
	if err != nil {
		return nil, err
	}
	now, out := svc.now(), []staleRun{}
	for _, r := range rows {
		c := staleRun{ListStaleRunCandidatesRow: r}
		if s, applied := svc.hub.staleView(r.NodeID); s != nil {
			v, live := s.liveAtConnect[r.ID]
			if !applied || !live || v != r.Version || s.inHello[r.ID] {
				continue // the hello reported it, a report failed, it is newer than the connection, or it changed since
			}
			left := svc.opts.StaleRunGrace - svc.opts.Clock.Now().Sub(s.connectedAt)
			c.Reason, c.Due, c.ready, c.session = EndStaleNotOnNode, now.Add(max(left, 0)), left <= 0, s
		} else {
			if r.LeaseNodeID != r.NodeID || r.LeaseExpiresAt == 0 {
				continue // no active lease of this node to have missed (a revoked one is not joined)
			}
			grace := max(time.Duration(r.LeaseGraceUs)*time.Microsecond, svc.opts.DisconnectGrace)
			c.Reason, c.Due = EndStaleLeaseLost, time.UnixMicro(r.LeaseExpiresAt).Add(grace+svc.opts.StaleLeaseMargin)
			c.ready = !now.Before(c.Due)
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
	n := 0
	for i := range runs {
		r := &runs[i]
		if !r.ready {
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

// stillStale re-checks a sweep's signal in the run's end transaction: the run is unchanged since the sweeper chose it,
// still live past its launch, reported by a node, has no unacknowledged launch, and its signal holds (the same
// connection whose hello did not report it, or still no connection).
func (svc *Service) stillStale(ctx context.Context, tx *store.Tx, r db.Run, sweep *staleRun) (bool, error) {
	if r.Version != sweep.Version || !Live(r.Observed) || r.Observed == proto.RunPending || !r.ObservedAt.Valid {
		return false, nil
	}
	if n, err := tx.Q.CountUnackedRunLaunches(ctx, sql.NullString{String: r.ID, Valid: true}); err != nil || n > 0 {
		return false, err
	}
	s, applied := svc.hub.staleView(r.NodeID)
	if sweep.Reason == EndStaleNotOnNode {
		return s != nil && s == sweep.session && applied, nil
	}
	return s == nil, nil // the node reconnected: its hello decides now
}

// staleRunError is the explanation shown on a run the sweeper ended.
func staleRunError(reason string) string {
	if reason == EndStaleLeaseLost {
		return "central marked this run lost: its node stayed disconnected past the space's lease and the disconnect grace, " +
			"so the node's lease guard has stopped it"
	}
	return "central marked this run lost: its node reconnected and its hello did not report it (the node agent restarted and " +
		"the process did not survive)"
}

// auditStaleRun records, in the run's end transaction, that the sweeper ended it.
func (svc *Service) auditStaleRun(ctx context.Context, tx *store.Tx, r db.Run, sweep *staleRun) error {
	_, err := svc.audit.Append(ctx, tx, audit.Event{Actor: audit.Actor{Type: audit.ActorSystem, ID: "stale-runs"}, Action: "run.reconciled_stale",
		Target: audit.Target{Type: "run", ID: r.ID}, Details: map[string]any{"space_id": r.SpaceID, "node_id": r.NodeID, "reason": sweep.Reason,
			"previous_state": r.Observed, "last_observed_at": time.UnixMicro(sweep.LastObservedAt).UTC().Format(time.RFC3339), "due_at": sweep.Due.UTC().Format(time.RFC3339),
			"summary": fmt.Sprintf("%s run %s (%s) was %s", r.Kind, r.ID, r.Role, r.Observed)}})
	return err
}
