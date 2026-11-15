package agent

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/coder/websocket"

	"github.com/aurimasniekis/go-spaceman/internal/obs"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/recording"
	"github.com/aurimasniekis/go-spaceman/internal/store"
)

// The run journal used to keep every ended run and the hello re-sent all of them. Now the hello carries the runs central
// needs, the journal deletes ended runs past a retention period, and the node learns what central has seen without a
// protocol change:
//
// Central answers a hello with a reconcile (after it applied the hello's reports) that lists the node's runs it still
// considers active. An ended run whose final state this process sent and that is not in that list is acknowledged:
// central holds an ended state for it. A run central still lists (its report did not apply) stays unacknowledged, and is
// sent again by the next hello. Acknowledgements are kept in memory only: after a restart every ended run is unacknowledged
// until the next connection has been through that exchange, so nothing is deleted on trust.
const (
	// helloRecentWindow is how far back the hello still sends an unacknowledged ended run itself; an older one goes in
	// the follow-up after the welcome.
	helloRecentWindow = 24 * time.Hour
	// followUpSlice is how many follow-up reports go out before a pause (followUpPause), outside the connect deadline.
	followUpSlice = 100
)

var followUpPause = 250 * time.Millisecond

// retention returns the configured journal retention, never shorter than claudePinWindow: the journal's runs are where
// the cache's pinned versions are learned (claudePinned, ADR-0077).
func (a *Agent) retention() time.Duration {
	if a.o.JournalRetention < claudePinWindow {
		return claudePinWindow
	}
	return a.o.JournalRetention
}

// helloRuns splits the journal into the runs the hello carries (live and adoptable ones, ended ones central has not been
// shown yet that ended recently) and the rest, which follow after the welcome. Ended runs central acknowledged, or that
// an earlier connection of this process already sent (older than the recent window), are in neither.
func (a *Agent) helloRuns(ctx context.Context) (hello, follow []runRow, err error) {
	runs, err := a.j.runs(ctx, false)
	if err != nil {
		return nil, nil, err
	}
	since := a.now().Add(-helloRecentWindow).UnixMicro()
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, r := range runs {
		switch {
		case !ended(r.State):
			hello = append(hello, r)
		case a.finalAcked[r.RunID] >= r.UpdatedAt:
		case r.UpdatedAt >= since:
			hello = append(hello, r)
		case a.finalSent[r.RunID] < r.UpdatedAt:
			follow = append(follow, r)
		}
	}
	return hello, follow, nil
}

// noteSent records that the final state of the ended runs in rs went out.
func (a *Agent) noteSent(rs []runRow) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.finalSent == nil {
		a.finalSent = map[string]int64{}
	}
	for _, r := range rs {
		if ended(r.State) && r.UpdatedAt > a.finalSent[r.RunID] {
			a.finalSent[r.RunID] = r.UpdatedAt
		}
	}
}

// sendFollowUp reports the older unacknowledged ended runs in slices, on conn (the connection ctx belongs to). A run
// whose row changed since the snapshot rs was taken is skipped: the change is reported on its own.
func (a *Agent) sendFollowUp(ctx context.Context, conn *websocket.Conn, rs []runRow) {
	for len(rs) > 0 {
		n := min(followUpSlice, len(rs))
		for _, r := range rs[:n] {
			cur, err := a.j.getRun(ctx, r.RunID)
			if err != nil || cur.UpdatedAt != r.UpdatedAt {
				continue
			}
			if err := a.sendOn(ctx, conn, proto.TypeRunReport, r.report(a.now())); err != nil {
				return
			}
			a.noteSent([]runRow{r})
		}
		rs = rs[n:]
		if len(rs) == 0 {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(followUpPause):
		}
	}
}

// finalAck handles central's reconcile: the ended runs this process sent that central no longer lists as active are
// acknowledged. An ended run central still lists as active did not get its final state applied (a restored backup, a
// report lost with the connection, an apply failure): it is unacknowledged again and reported at once, well inside the
// stale sweeper's grace (ADR-0074), which would otherwise end it lost and fence the real final state. It then prunes.
func (a *Agent) finalAck(ctx context.Context, rec proto.Reconcile) {
	active := make(map[string]bool, len(rec.Runs))
	for _, d := range rec.Runs {
		active[d.RunID] = true
	}
	runs, err := a.j.runs(ctx, false)
	if err != nil {
		return
	}
	var resend []runRow
	a.mu.Lock()
	if a.finalAcked == nil {
		a.finalAcked = map[string]int64{}
	}
	for _, r := range runs {
		if ended(r.State) && active[r.RunID] {
			delete(a.finalAcked, r.RunID)
			delete(a.finalSent, r.RunID)
			resend = append(resend, r)
			continue
		}
		if ended(r.State) && !active[r.RunID] && a.finalSent[r.RunID] >= r.UpdatedAt && r.UpdatedAt > a.finalAcked[r.RunID] {
			a.finalAcked[r.RunID] = r.UpdatedAt
		}
	}
	a.mu.Unlock()
	for _, r := range resend {
		a.report(ctx, r)
	}
	a.pruneRuns(ctx)
}

// pruneRuns deletes the ended runs past the retention that nothing needs any more and returns how many it deleted. A run
// stays while any of these holds: it is not ended (live and adoptable runs, and so the versions GC's in-use set reads);
// its final state is not acknowledged by central (or changed since); it ended within the retention (never below the pin
// window: the cache's pins read the journal); its recording is not fully acknowledged or is still being written; a
// transcript upload or a dream's evidence positions for it are pending.
func (a *Agent) pruneRuns(ctx context.Context) int {
	runs, err := a.j.runs(ctx, false)
	if err != nil {
		return 0
	}
	cutoff := a.now().Add(-a.retention()).UnixMicro()
	n := 0
	for _, r := range runs {
		if !ended(r.State) || r.UpdatedAt >= cutoff {
			continue
		}
		a.mu.Lock()
		acked := a.finalAcked[r.RunID] >= r.UpdatedAt
		a.mu.Unlock()
		if !acked || a.recordingPending(r.RunID) {
			continue
		}
		ok, err := a.j.deleteEndedRun(ctx, r)
		if err != nil {
			a.log.Warn("journal prune", slog.String(obs.FieldRunID, r.RunID), obs.Err(err))
			continue
		}
		if !ok {
			continue
		}
		n++
		a.mu.Lock()
		delete(a.finalAcked, r.RunID)
		delete(a.finalSent, r.RunID)
		delete(a.acked, r.RunID)
		delete(a.sentSeq, r.RunID)
		delete(a.caughtUp, r.RunID)
		a.mu.Unlock()
	}
	if n > 0 {
		a.log.Info("journal pruned", slog.Int("ended_runs", n))
	}
	return n
}

// recordingPending reports whether a run's recording may still have something to ship: a live recorder, records central
// has not acknowledged, kept-back records, or segments on disk while central's acknowledgement is unknown (the same test
// the relocation check uses).
func (a *Agent) recordingPending(runID string) bool {
	a.mu.Lock()
	_, live := a.recorders[runID]
	dirty := a.unshipped[runID] || a.held[runID] != nil
	acked, known := a.acked[runID]
	a.mu.Unlock()
	if live || dirty {
		return true
	}
	dir := a.RecordingDir(runID)
	if _, err := os.Stat(dir); err != nil {
		return false
	}
	res, err := recording.Read(dir, acked)
	if err != nil {
		return true // unreadable: keep, don't guess
	}
	return len(res.Records) > 0 || !known && hasSegments(dir)
}

// deleteEndedRun deletes an ended run's row and its finished transcript upload row, and reports whether it deleted the
// run: the row must still be ended and unchanged since r was read, no transcript upload may be unfinished and no dream's
// evidence positions may reference it.
func (j *Journal) deleteEndedRun(ctx context.Context, r runRow) (bool, error) {
	deleted := false
	err := j.s.W().TxNamed(ctx, "journal.prune_run", func(tx *store.Tx) error {
		var held int
		if err := tx.SQL.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM dream_pending WHERE run_id = ?)
			+ (SELECT COUNT(*) FROM transcript_uploads WHERE run_id = ? AND done = 0)`, r.RunID, r.RunID).Scan(&held); err != nil {
			return err
		}
		if held > 0 {
			return nil
		}
		res, err := tx.SQL.ExecContext(ctx, `DELETE FROM runs WHERE run_id = ? AND updated_at = ? AND state IN ('exited', 'failed', 'lost')`,
			r.RunID, r.UpdatedAt)
		if err != nil {
			return err
		}
		c, _ := res.RowsAffected()
		if deleted = c == 1; deleted {
			_, err = tx.SQL.ExecContext(ctx, `DELETE FROM transcript_uploads WHERE run_id = ? AND done = 1`, r.RunID)
		}
		return err
	})
	return deleted, err
}
