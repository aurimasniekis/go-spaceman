package agent

import (
	"context"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/coder/websocket"

	"github.com/aurimasniekis/go-spaceman/internal/ids"
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

// pruneEvery is how often the journal is pruned while the process runs, besides after each reconcile: a node whose
// connection never drops would otherwise never prune.
var pruneEvery = time.Hour

// pruneLoop prunes the journal every pruneEvery until ctx ends. It deletes only what the acknowledgements this process
// holds allow, so running it before a connection has been through the reconcile deletes nothing.
func (a *Agent) pruneLoop(ctx context.Context) {
	t := time.NewTicker(pruneEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.pruneRuns(ctx)
			a.sweepOrphanRecordings(ctx)
		}
	}
}

// orphanRecordingAge is how long an unreferenced recording directory must have been untouched before the sweep removes it.
const orphanRecordingAge = 24 * time.Hour

// sweepOrphanRecordings removes the recording directories earlier releases left behind when they pruned a run, and only
// those that hold no segments: the directory of a run with a well-formed ID and no journal row (in any state, so the
// journal's pins and in-use sets are untouched), with nothing pending to ship and nothing in it modified for
// orphanRecordingAge. A recording that has segments and no journal row is always kept: a lost or reset journal makes
// every run look orphaned, and without central's confirmation the node cannot tell the two apart, so it does not reclaim
// legacy recordings. The sweep stays inside the runs folder, never follows a symlink and logs one line per sweep. It
// returns how many directories it removed.
func (a *Agent) sweepOrphanRecordings(ctx context.Context) int {
	root := filepath.Join(a.o.DataDir, "runs")
	entries, err := os.ReadDir(root)
	if err != nil {
		return 0
	}
	rows, err := a.j.runs(ctx, false)
	if err != nil {
		return 0
	}
	journaled := make(map[string]bool, len(rows))
	for _, r := range rows {
		journaled[r.RunID] = true
	}
	cutoff := a.now().Add(-orphanRecordingAge)
	var n int
	var freed int64
	for _, e := range entries {
		id := e.Name()
		if ctx.Err() != nil {
			break
		}
		// The cheap checks first; the pending check (which may read the recording) is last.
		if !e.IsDir() || !ids.Valid(ids.Run, id) || journaled[id] { // IsDir is false for a symlink
			continue
		}
		dir := a.RecordingDir(id)
		if fi, err := os.Lstat(dir); err != nil || !fi.IsDir() {
			continue
		}
		size, newest, ok := recordingTreeStat(dir)
		if !ok || newest.After(cutoff) || a.recordingPending(id) {
			continue
		}
		a.removeRecording(id)
		n++
		freed += size
	}
	if n > 0 {
		a.log.Info("orphan recordings removed", slog.Int("dirs", n), slog.Int64("bytes", freed))
	}
	return n
}

// removeRecording deletes a run's recording directory, then its quota and acknowledgement files and the run directory
// itself when nothing else is in it (a run directory holding anything else stays as it is). A malformed run ID removes
// nothing.
func (a *Agent) removeRecording(runID string) {
	if !ids.Valid(ids.Run, runID) { // the ID becomes a path below
		a.log.Warn("recording directory kept: malformed run ID", slog.Int("id_len", len(runID)))
		return
	}
	if err := os.RemoveAll(a.RecordingDir(runID)); err != nil {
		a.log.Warn("recording directory removal", slog.String(obs.FieldRunID, runID), obs.Err(err))
		return
	}
	run := a.runDir(runID)
	left, err := os.ReadDir(run)
	if err != nil {
		return
	}
	recordingFiles := map[string]bool{recordingQuotaFile: true, recordingAckedFile: true, recordingAckedFile + ".tmp": true}
	for _, e := range left {
		if !recordingFiles[e.Name()] || !e.Type().IsRegular() {
			return
		}
	}
	for name := range recordingFiles {
		_ = os.Remove(filepath.Join(run, name))
	}
	_ = os.Remove(run) // fails (and stays) if anything appeared meanwhile
}

// recordingTreeStat returns the total size of the files under dir and the newest modification time of anything in it
// (links are not followed); ok is false when the tree could not be read.
func recordingTreeStat(dir string) (size int64, newest time.Time, ok bool) {
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		if fi.ModTime().After(newest) {
			newest = fi.ModTime()
		}
		if fi.Mode().IsRegular() {
			size += fi.Size()
		}
		return nil
	})
	return size, newest, err == nil
}

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
	a.followSnap = map[string]int64{}
	for _, r := range runs {
		switch {
		case !ended(r.State):
			hello = append(hello, r)
		case a.finalAcked[r.RunID] >= r.UpdatedAt:
		case r.UpdatedAt >= since:
			hello = append(hello, r)
		case a.finalSent[r.RunID] < r.UpdatedAt:
			follow = append(follow, r)
			a.followSnap[r.RunID] = r.UpdatedAt
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
// Not reported inline: a run this process has not sent yet, and a run in this connection's follow-up snapshot, sent or
// not. Central builds the reconcile before it reads anything the follow-up sends, so such a run is still listed
// active, and the follow-up is its one report. Neither is acknowledged or pruned.
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
			if a.finalSent[r.RunID] < r.UpdatedAt || r.UpdatedAt <= a.followSnap[r.RunID] {
				// Not sent yet by this process, or carried by this connection's follow-up: no inline report.
				continue
			}
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
// transcript upload or a dream's evidence positions for it are pending. A pruned run's recording directory is removed
// with it (only reached once the recording is fully acknowledged).
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
		// The run is gone from the journal and its recording is fully acknowledged (recordingPending above): nothing
		// references the directory any more.
		a.removeRecording(r.RunID)
		a.mu.Lock()
		delete(a.finalAcked, r.RunID)
		delete(a.finalSent, r.RunID)
		delete(a.acked, r.RunID)
		delete(a.restored, r.RunID)
		delete(a.seqKnown, r.RunID)
		delete(a.seqFloor, r.RunID)
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
	if !known {
		// Nothing is known about central's copy: any segment is pending, and there is no need to decode it.
		segs, err := recording.Segments(dir)
		return err != nil || len(segs) > 0
	}
	res, err := readRecording(dir, acked)
	if err != nil {
		return true // unreadable: keep, don't guess
	}
	return len(res.Records) > 0
}

// readRecording decodes a recording directory; tests replace it to see whether a decode happened.
var readRecording = recording.Read

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
