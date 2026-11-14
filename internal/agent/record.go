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

	"github.com/coder/websocket"

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
// central after every flush (at most once a second). Shipping runs in the pump: a send may take up to its write timeout,
// but it never waits for a catch-up pass or another run's send (shipLive).
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

// Recording shipments keep central's copy whole. Central stores only records above the last sequence it stored, so
// a record must never reach it before an older one that is still missing (lost with a connection, or not sent yet):
// the older one would be dropped for good. Live shipments (shipLive) therefore wait while a catch-up pass
// (catchUpRecordings) may still have older records of the run to send, and every send goes on the connection it was
// decided for, so nothing decided before a reconnect lands on the new connection ahead of the catch-up.

// sendRecordingHook, when set (tests), runs before each recording message; an error fails that send (nothing is sent).
var sendRecordingHook func(runID string) error

// ship sends records to central on conn, in messages inside the protocol's limit, in order, and stops at the first
// failure: the run is then marked for the next catch-up pass.
func (a *Agent) ship(ctx context.Context, conn *websocket.Conn, runID, spaceID string, recs []recording.Record) error {
	raws := make([]json.RawMessage, 0, len(recs))
	for _, r := range recs {
		b, err := json.Marshal(r)
		if err == nil {
			raws = append(raws, b)
		}
	}
	at := 0
	for _, part := range shipParts(raws, shipChunkBytes) {
		at += len(part)
		// Counted as sent before the write: a sweep that looks meanwhile must not take the run for clean while this
		// part could still be lost with its connection.
		a.mu.Lock()
		if a.sentSeq == nil {
			a.sentSeq = map[string]uint64{}
		}
		a.sentSeq[runID] = max(a.sentSeq[runID], recs[at-1].Seq)
		a.mu.Unlock()
		var err error
		if sendRecordingHook != nil {
			err = sendRecordingHook(runID)
		}
		if err == nil {
			err = a.sendOn(ctx, conn, proto.TypeRecording, proto.RecordingChunk{RunID: runID, SpaceID: spaceID, Records: part})
		}
		if err != nil {
			a.markUnshipped(runID)
			return err
		}
	}
	return nil
}

// heldRecords are records a live shipment kept back while it waited for a catch-up (sent after the run's local
// recording). Records with no local copy (a close or trunc record after the local disk failed) are always kept;
// others only up to heldMaxBytes, past which lost is set and the catch-up reads the local recording again.
type heldRecords struct {
	recs  []recording.Record
	bytes int
	lost  bool
}

// heldMaxBytes bounds the memory a run's kept-back records take while it waits for a catch-up, counting each record's
// data plus heldRecordOverhead.
const (
	heldMaxBytes       = 1 << 20
	heldRecordOverhead = 64
)

// shipLive ships a recorder's records unless they could overtake older ones central may still be missing: while the
// node is not connected, the run is marked for a catch-up, or the catch-up pass has not caught the run up yet, it
// keeps them back for the pass instead (the pass reads the local recording first, then sends what was kept back).
// It never blocks on a pass or a send of another run.
func (a *Agent) shipLive(ctx context.Context, runID, spaceID string, recs []recording.Record) {
	a.mu.Lock()
	conn := a.conn
	wait := !a.connected || conn == nil || a.unshipped[runID] || a.catching && !a.caughtUp[runID]
	if wait {
		a.markUnshippedLocked(runID)
		a.holdLocked(runID, recs)
	}
	a.mu.Unlock()
	if wait {
		return
	}
	if err := a.ship(ctx, conn, runID, spaceID, recs); err != nil {
		// The run is marked; keep the records back too, so one with no local copy still goes after the catch-up.
		a.mu.Lock()
		a.holdLocked(runID, recs)
		a.mu.Unlock()
	}
}

func (a *Agent) holdLocked(runID string, recs []recording.Record) {
	if a.held == nil {
		a.held = map[string]*heldRecords{}
	}
	h := a.held[runID]
	if h == nil {
		h = &heldRecords{}
		a.held[runID] = h
	}
	h.add(recs)
}

// add keeps records back within heldMaxBytes: close and trunc records always (possibly the only copy, SM-REC-004),
// other output while it fits; past that, lost is set and the catch-up reads the local recording again.
func (h *heldRecords) add(recs []recording.Record) {
	for _, r := range recs {
		if r.Kind == recording.KindClose || r.Kind == recording.KindTrunc {
			h.recs = append(h.recs, r)
			continue
		}
		size := len(r.Data) + heldRecordOverhead
		if h.bytes+size > heldMaxBytes {
			h.lost = true
			continue
		}
		h.recs = append(h.recs, r)
		h.bytes += size
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

// startCatchUp asks for a catch-up pass on the current connection (before a relocation).
func (a *Agent) startCatchUp() {
	a.mu.Lock()
	start := a.connected && a.startCatchUpLocked()
	a.mu.Unlock()
	if start {
		go a.catchUpRecordings()
	}
}

// startCatchUpLocked asks for a catch-up pass on the current connection: true when the caller must start it, false
// when the running pass sweeps again on that connection instead (one pass at a time). Every run counts as not caught
// up until the pass reaches it.
func (a *Agent) startCatchUpLocked() bool {
	a.caughtUp = map[string]bool{}
	a.catchUpGen++
	a.catchUpConn, a.catchUpCtx = a.conn, a.connCtx
	if a.catching {
		a.catchUpAgain = true
		return false
	}
	a.catching = true
	return true
}

// catchUpRecordings is the catch-up pass: it sweeps the journaled runs, re-shipping each run's local records above
// central's acknowledged sequence where central may lack some, and sweeps again while another pass was asked for or
// a run waits for it (a run started during a sweep). It ends when a sweep left no run waiting, or at the first failed
// send (the connection is gone; the next connection's pass resumes from what central acknowledged). Central stores
// each sequence once, so re-sent records are harmless. Runs are caught up one at a time: a run with a long backlog
// delays the others (their live output is kept back meanwhile, see shipLive).
func (a *Agent) catchUpRecordings() {
	retry := time.Second
	for {
		a.mu.Lock()
		conn, ctx, gen := a.catchUpConn, a.catchUpCtx, a.catchUpGen
		a.catchUpAgain = false
		a.mu.Unlock()
		err := a.catchUpSweep(ctx, conn, gen)
		a.mu.Lock()
		again := a.catchUpAgain
		done := !again && err == nil && len(a.unshipped) == 0
		// A failure while the connection is still the current one (the journal could not be read, say) is retried;
		// one that came with the connection's end waits for the next connection's pass.
		gone := err != nil && (!a.connected || a.conn != conn || ctx == nil || ctx.Err() != nil)
		if done || !again && gone {
			a.catching = false
			a.mu.Unlock()
			return
		}
		a.mu.Unlock()
		if err == nil {
			retry = time.Second
		}
		if err != nil && !again {
			select {
			case <-ctx.Done():
			case <-time.After(retry):
			}
			retry = min(retry*2, 30*time.Second)
		}
	}
}

// catchUpSweep is one sweep of the catch-up pass over the journal.
func (a *Agent) catchUpSweep(ctx context.Context, conn *websocket.Conn, gen uint64) error {
	if ctx == nil || conn == nil {
		return errors.New("not connected")
	}
	runs, err := a.j.runs(ctx, false)
	if err != nil {
		return err
	}
	journaled := map[string]bool{}
	for _, r := range runs {
		journaled[r.RunID] = true
		a.mu.Lock()
		acked, known := a.acked[r.RunID]
		dirty := a.unshipped[r.RunID] || a.held[r.RunID] != nil
		clean := !dirty && (a.caughtUp[r.RunID] || known && a.sentSeq[r.RunID] <= acked)
		if clean && a.catchUpGen == gen {
			// Everything sent so far was acknowledged (or this pass sent it): nothing to re-ship.
			a.caughtUp[r.RunID] = true
		}
		a.mu.Unlock()
		if clean {
			continue
		}
		if err := a.catchUpRun(ctx, conn, gen, r, acked); err != nil {
			return err
		}
	}
	// Marks and kept-back records of runs no longer in the journal have nothing left to ship. Decided on a fresh
	// listing: a run started during this sweep (journaled before its program starts) is not in the one above.
	now, err := a.j.runs(ctx, false)
	if err != nil {
		return err
	}
	for _, r := range now {
		journaled[r.RunID] = true
	}
	a.mu.Lock()
	for id := range a.unshipped {
		if !journaled[id] && a.held[id] == nil {
			delete(a.unshipped, id)
		}
	}
	// The journal keeps ended runs (it never deletes them), so this drops only records of IDs that were never
	// journaled: none in practice; it keeps a stray entry from making the pass sweep forever.
	for id := range a.held {
		if !journaled[id] {
			delete(a.held, id)
			delete(a.unshipped, id)
		}
	}
	a.mu.Unlock()
	return nil
}

// shipBatchHook, when set (tests), runs after each batch a catch-up ships.
var shipBatchHook func(runID string, batch int)

// catchUpRun ships a run's local records above from in batches, then what live shipments kept back meanwhile, until
// nothing more was kept back, and marks the run caught up (atomically with that check, under the lock shipLive decides
// under).
func (a *Agent) catchUpRun(ctx context.Context, conn *websocket.Conn, gen uint64, r runRow, from uint64) error {
	batch := 0
	send := func(recs []recording.Record) error {
		for start := 0; start < len(recs); start += 256 {
			end := min(start+256, len(recs))
			if err := a.ship(ctx, conn, r.RunID, r.SpaceID, recs[start:end]); err != nil {
				return err
			}
			batch++
			if shipBatchHook != nil {
				shipBatchHook(r.RunID, batch)
			}
			from = max(from, recs[end-1].Seq)
		}
		return nil
	}
	readDisk := func() error {
		res, err := recording.Read(a.RecordingDir(r.RunID), from)
		if err != nil {
			return nil //nolint:nilerr // an unreadable local recording has nothing to re-ship; only a failed send ends the pass
		}
		return send(res.Records)
	}
	a.mu.Lock()
	delete(a.unshipped, r.RunID)
	a.mu.Unlock()
	if err := readDisk(); err != nil {
		return err
	}
	for {
		a.mu.Lock()
		h := a.held[r.RunID]
		delete(a.held, r.RunID)
		delete(a.unshipped, r.RunID)
		if h == nil {
			// Caught up for the request this sweep serves; after a newer one (a reconnect) the next sweep decides.
			if a.catchUpGen == gen {
				a.caughtUp[r.RunID] = true
			}
			a.mu.Unlock()
			return nil
		}
		a.mu.Unlock()
		if h.lost {
			// Some output was not kept back: the local recording has it.
			if err := readDisk(); err != nil {
				a.putBack(r.RunID, h.recs, from, true)
				return err
			}
		}
		var later []recording.Record
		for _, rec := range h.recs {
			if rec.Seq > from {
				later = append(later, rec)
			}
		}
		if err := send(later); err != nil {
			// What did not go out is kept back again for the next pass: a record with no local copy exists only here.
			a.putBack(r.RunID, later, from, false)
			return err
		}
	}
}

// putBack keeps a run's records above from back again, in front of any kept back since (they are older), after a
// catch-up could not send them, within the same bound as holdLocked; lost carries over that some output was not
// kept back.
func (a *Agent) putBack(runID string, recs []recording.Record, from uint64, lost bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	back := &heldRecords{lost: lost}
	var older []recording.Record
	for _, r := range recs {
		if r.Seq > from {
			older = append(older, r)
		}
	}
	back.add(older)
	if since := a.held[runID]; since != nil {
		back.add(since.recs)
		back.lost = back.lost || since.lost
	}
	if len(back.recs) > 0 || back.lost {
		if a.held == nil {
			a.held = map[string]*heldRecords{}
		}
		a.held[runID] = back
		a.markUnshippedLocked(runID)
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
