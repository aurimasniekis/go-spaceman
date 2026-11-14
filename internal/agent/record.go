package agent

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/obs"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/recording"
	"github.com/aurimasniekis/go-spaceman/internal/runner"
)

// RecordingDir is a run's local recording directory.
func (a *Agent) RecordingDir(runID string) string { return filepath.Join(a.runDir(runID), "recording") }

// recordingQuotaFile keeps the recording bound central sent at launch, so a recorder restarted after an agent restart
// applies the same bound.
const recordingQuotaFile = "recording-quota"

// Recording bounds a launch may set: the range of the recording_quota_bytes limit (ADR-0045).
const (
	minRecordingQuota = 1 << 20
	maxRecordingQuota = 64 << 30
)

// saveRecordingQuota stores the run's recording bound (0: the default applies).
func (a *Agent) saveRecordingQuota(runID string, n int64) error {
	if n <= 0 {
		return nil
	}
	n = min(max(n, minRecordingQuota), maxRecordingQuota)
	if err := os.MkdirAll(a.runDir(runID), 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(a.runDir(runID), recordingQuotaFile), []byte(strconv.FormatInt(n, 10)), 0o600)
}

// recordingQuota returns the run's recording bound: what central sent at launch, else proto.DefaultRecordingMaxBytes.
func (a *Agent) recordingQuota(runID string) int64 {
	b, err := os.ReadFile(filepath.Join(a.runDir(runID), recordingQuotaFile)) //nolint:gosec // a fixed name in the run's own directory
	if err != nil {
		return proto.DefaultRecordingMaxBytes
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil || n < minRecordingQuota || n > maxRecordingQuota {
		return proto.DefaultRecordingMaxBytes
	}
	return n
}

// recorder is one run's live recording: cancel ends it, sizes carries the terminal's size changes into it.
type recorder struct {
	cancel context.CancelFunc
	sizes  chan [2]int
}

// recordSize tells a run's recorder the terminal's size changed (or is about to: a hold notes the size before it
// resizes, so the output drawn for the new size follows the record). The recorder writes a size record when the size
// differs from the last one it recorded. It never blocks: a full queue drops the note, and the runner's size event
// that follows the change brings it again.
func (a *Agent) recordSize(runID string, cols, rows int) {
	a.mu.Lock()
	r := a.recorders[runID]
	a.mu.Unlock()
	if r == nil {
		return
	}
	select {
	case r.sizes <- [2]int{cols, rows}:
	default:
	}
}

// record attaches to a run's terminal as an observer and writes everything it outputs until the program ends. It is
// independent of browsers (SM-REC-002): live viewers read the recording, never the other way round, so viewer
// backpressure cannot slow the run (SM-REC-004). Interruptions (overflow, runner restart) re-attach and record a gap,
// because herdr-core streams carry no offsets (S-3).
func (a *Agent) record(ctx context.Context, row runRow, sizes <-chan [2]int) {
	log := a.log.With(slog.String(obs.FieldRunID, row.RunID))
	w, err := recording.Open(recording.Options{Dir: a.RecordingDir(row.RunID), MaxBytes: a.recordingQuota(row.RunID), Now: a.o.Clock.Now, Create: a.o.RecordingCreate})
	if err != nil {
		log.Warn("open recording", obs.Err(err))
		return
	}
	defer func() { _ = w.Close() }()
	flush := time.NewTicker(recording.FlushInterval)
	defer flush.Stop()
	first := w.Seq() == 0
	cols, rows := 0, 0
	for attempt := 0; attempt < 20 && ctx.Err() == nil; attempt++ {
		st, p, err := a.o.Runner.Attach(ctx, row.TerminalID, true, 0, 0)
		if err != nil {
			if errors.Is(err, runner.ErrNotFound) || errors.Is(err, runner.ErrFinished) {
				return
			}
			log.Warn("attach for recording", obs.Err(err))
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
			continue
		}
		var prelude []recording.Record
		if first {
			prelude = append(prelude, recording.Record{Kind: recording.KindOpen, Cols: p.Cols, Rows: p.Rows, RunID: row.RunID})
			first = false
		} else {
			prelude = append(prelude, recording.Record{Kind: recording.KindGap, Why: "reattach"})
			if p.Cols != cols || p.Rows != rows {
				prelude = append(prelude, recording.Record{Kind: recording.KindSize, Cols: p.Cols, Rows: p.Rows})
			}
		}
		cols, rows = p.Cols, p.Rows
		done := a.pump(ctx, row, st, w, flush, prelude, sizes, &cols, &rows)
		_ = st.Close()
		if done {
			return
		}
	}
}

// pump copies one attachment into the recording; it returns true when the program ended. Records are shipped to
// central after every flush (at most once a second); shipping never blocks recording.
// sizes are the terminal's size changes; cols and rows are the size last recorded, kept across reattaches.
func (a *Agent) pump(ctx context.Context, row runRow, st runner.Stream, w *recording.Writer, flush *time.Ticker, prelude []recording.Record,
	sizes <-chan [2]int, cols, rows *int) bool {
	var batch []recording.Record
	// truncated adds the writer's trunc record (quota reached, or the local disk failed) after the records before it,
	// so central's copy shows where and why the recording ended; the run itself is never affected (SM-REC-004).
	truncated := func() {
		if tr, ok := w.Truncation(); ok {
			if tr.Why == "disk" {
				a.log.Warn("recording stopped: the local recording cannot be written; central keeps the records shipped so far",
					slog.String(obs.FieldRunID, row.RunID), obs.Err(w.Err()))
			}
			batch = append(batch, tr)
		}
	}
	add := func(r recording.Record) {
		// After a local loss (a failed disk, then a restart) central may already hold higher sequence numbers:
		// continue above them rather than have central ignore the new records as repeats.
		if acked := a.ackedSeq(row.RunID); acked > w.Seq() {
			w.SkipTo(acked)
		}
		// A stored record has a sequence number; after a write failure only the close record still gets one (it
		// cannot be stored locally, but central can keep it).
		if stored, _ := w.AddRecord(r); stored.Seq != 0 {
			batch = append(batch, stored)
		}
		truncated()
	}
	for _, r := range prelude {
		add(r)
	}
	ship := func() {
		_ = w.Flush()
		truncated()
		if len(batch) > 0 {
			// Central stores records in sequence order and ignores repeats, so a trunc record numbered while its
			// neighbour was being added must not overtake it.
			slices.SortFunc(batch, func(x, y recording.Record) int { return cmp.Compare(x.Seq, y.Seq) })
			a.shipLive(ctx, row.RunID, row.SpaceID, batch)
			batch = nil
		}
	}
	defer ship()
	size := func(sz [2]int) {
		if sz[0] != *cols || sz[1] != *rows {
			*cols, *rows = sz[0], sz[1]
			add(recording.Record{Kind: recording.KindSize, Cols: sz[0], Rows: sz[1]})
		}
	}
	type chunk struct {
		data   []byte
		closed *runner.CloseInfo
		err    error
	}
	ch := make(chan chunk, 64)
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		defer close(ch)
		for {
			d, c, err := st.Next(rctx)
			select {
			case ch <- chunk{d, c, err}:
			case <-rctx.Done():
				return
			}
			if err != nil || c != nil {
				return
			}
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return true
		case <-flush.C:
			ship()
		case sz := <-sizes:
			size(sz)
		case c, ok := <-ch:
			// A size noted before this output arrived goes first (select picks among ready cases at random).
			for drained := false; !drained; {
				select {
				case sz := <-sizes:
					size(sz)
				default:
					drained = true
				}
			}
			if !ok {
				return false
			}
			switch {
			case c.closed != nil:
				add(recording.Record{Kind: recording.KindClose, ExitCode: c.closed.ExitCode, Reason: c.closed.Reason})
				return true
			case c.err != nil:
				return false
			default:
				add(recording.Record{Kind: recording.KindOut, Data: c.data})
			}
		}
	}
}

// ship sends records to central in messages inside the protocol's limit, in order, and stops at the first failure: the
// run is then marked for the catch-up after reconnecting.
func (a *Agent) ship(ctx context.Context, runID, spaceID string, recs []recording.Record) error {
	raws := make([]json.RawMessage, 0, len(recs))
	for _, r := range recs {
		b, err := json.Marshal(r)
		if err == nil {
			raws = append(raws, b)
		}
	}
	for _, part := range shipParts(raws, shipChunkBytes) {
		if err := a.send(ctx, proto.TypeRecording, proto.RecordingChunk{RunID: runID, SpaceID: spaceID, Records: part}); err != nil {
			a.markUnshipped(runID)
			return err
		}
	}
	return nil
}

// shipLive ships a recorder's records (already in the local recording) unless they could overtake older records not
// yet in central's copy: central keeps only records above the last sequence it stored, so a newer record arriving
// first would lose the older ones for good. While the node is not connected, a catch-up pass runs, or the run is
// marked for one, the records are left to the catch-up, which reads them from the local recording. Once the pass has
// caught the run up, its live shipments go ahead (they are newer than everything the pass sent).
func (a *Agent) shipLive(ctx context.Context, runID, spaceID string, recs []recording.Record) {
	a.shipGate.RLock()
	defer a.shipGate.RUnlock()
	a.mu.Lock()
	wait := !a.connected || a.unshipped[runID] || a.catchUps > 0 && !a.caughtUp[runID]
	if wait {
		a.markUnshippedLocked(runID)
	}
	a.mu.Unlock()
	if !wait {
		_ = a.ship(ctx, runID, spaceID, recs)
	}
}

// shipChunkBytes bounds the records of one recording message, well inside proto.MaxMessage. A burst of output
// (megabytes within one flush interval) used to go as one message; central closed the connection for a message over
// MaxMessage, and the records in it were lost from its copy.
const shipChunkBytes = 1 << 20

// shipParts splits encoded records into runs of at most limit bytes each, in order; a record larger than limit goes
// alone.
func shipParts(raws []json.RawMessage, limit int) [][]json.RawMessage {
	var parts [][]json.RawMessage
	start, size := 0, 0
	for i, r := range raws {
		if i > start && size+len(r) > limit {
			parts = append(parts, raws[start:i])
			start, size = i, 0
		}
		size += len(r)
	}
	if start < len(raws) {
		parts = append(parts, raws[start:])
	}
	return parts
}

func (a *Agent) markUnshipped(runID string) {
	a.mu.Lock()
	a.markUnshippedLocked(runID)
	a.mu.Unlock()
}

func (a *Agent) markUnshippedLocked(runID string) {
	if a.unshipped == nil {
		a.unshipped = map[string]bool{}
	}
	a.unshipped[runID] = true
}

// beginCatchUp starts a catch-up pass (catchUpRecordings ends it): live shipments wait until it is done.
func (a *Agent) beginCatchUp() {
	a.shipGate.Lock()
	a.mu.Lock()
	a.catchUps++
	a.caughtUp = map[string]bool{}
	a.mu.Unlock()
	a.shipGate.Unlock()
}

// catchUpRecordings re-ships local recordings after the last acknowledged sequence (after connecting, or before a
// relocation), and ends the pass beginCatchUp (or the welcome) started. Central stores each sequence once, so overlap
// is harmless. Each run is caught up once per pass; from then on its live shipments go ahead (shipLive). It stops at
// the first failed send (the connection is gone; the next pass resumes from what central acknowledged). Runs started
// during the pass are caught up before it ends, so no live shipment waits for a pass that is no longer coming.
func (a *Agent) catchUpRecordings(ctx context.Context) {
	a.mu.Lock()
	if a.caughtUp == nil {
		a.caughtUp = map[string]bool{}
	}
	a.mu.Unlock()
	ended := false
	defer func() {
		if !ended {
			a.mu.Lock()
			a.catchUps--
			a.mu.Unlock()
		}
	}()
	for first := true; ; first = false {
		runs, err := a.j.runs(ctx, false)
		if err != nil {
			return
		}
		journaled := map[string]bool{}
		for _, r := range runs {
			journaled[r.RunID] = true
			a.mu.Lock()
			acked, known := a.acked[r.RunID]
			dirty := a.unshipped[r.RunID]
			if !dirty && (known || !first || a.caughtUp[r.RunID]) {
				// Nothing to re-ship: everything shipped so far reached a live connection.
				a.caughtUp[r.RunID] = true
				a.mu.Unlock()
				continue
			}
			a.mu.Unlock()
			if err := a.catchUpRun(ctx, r, acked); err != nil {
				return
			}
		}
		// The pass ends when no run waits for it (a run started meanwhile may have), in the same lock live shipments
		// decide under.
		a.mu.Lock()
		waiting := false
		for id := range a.unshipped {
			if journaled[id] {
				waiting = true
			} else {
				delete(a.unshipped, id) // removed from the journal: nothing left to ship
			}
		}
		if !waiting {
			a.catchUps--
			ended = true
		}
		a.mu.Unlock()
		if ended {
			return
		}
	}
}

// catchUpRunHook, when set (tests), runs after each pass over a run's local recording.
var catchUpRunHook func(runID string)

// catchUpRun ships a run's local records above from, in batches, until no live shipment waited meanwhile, then marks
// the run caught up for this pass (atomically with that check, under the lock shipLive decides under).
func (a *Agent) catchUpRun(ctx context.Context, r runRow, from uint64) error {
	for {
		a.mu.Lock()
		delete(a.unshipped, r.RunID)
		a.mu.Unlock()
		res, err := recording.Read(a.RecordingDir(r.RunID), from)
		if err == nil {
			for start := 0; start < len(res.Records); start += 256 {
				end := min(start+256, len(res.Records))
				if err := a.ship(ctx, r.RunID, r.SpaceID, res.Records[start:end]); err != nil {
					return err
				}
			}
			if n := len(res.Records); n > 0 {
				from = res.Records[n-1].Seq
			}
		}
		if catchUpRunHook != nil {
			catchUpRunHook(r.RunID)
		}
		a.mu.Lock()
		again := a.unshipped[r.RunID]
		if !again {
			a.caughtUp[r.RunID] = true
		}
		a.mu.Unlock()
		if !again {
			return nil
		}
	}
}

// ackedSeq is central's highest stored recording sequence of a run (0 when unknown).
func (a *Agent) ackedSeq(runID string) uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.acked[runID]
}

func (a *Agent) recordingAck(ack proto.RecordingAck) {
	a.mu.Lock()
	if a.acked == nil {
		a.acked = map[string]uint64{}
	}
	if ack.Seq > a.acked[ack.RunID] {
		a.acked[ack.RunID] = ack.Seq
	}
	a.mu.Unlock()
}
