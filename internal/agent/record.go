package agent

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"math"
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

// SentRecordingSeq returns the highest sequence of the run's recording this agent has handed to a connection (tests).
func (a *Agent) SentRecordingSeq(runID string) uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sentSeq[runID]
}

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
	// Before anything can ask central: its answer to the probe (recordingAck) may reach this process ahead of the
	// catch-up sweep, and an acknowledgement that was not restored by then is not marked restored, so the sweep would
	// take the run for clean and never re-ship the local records above it.
	a.restoreAck(row.RunID)
	w, err := recording.Open(recording.Options{Dir: a.RecordingDir(row.RunID), MaxBytes: a.recordingQuota(row.RunID), Now: a.o.Clock.Now, Create: a.o.RecordingCreate,
		RetryAfter: a.o.RecordingRetryAfter})
	if err != nil {
		log.Warn("open recording", obs.Err(err))
		return
	}
	defer func() { _ = w.Close() }()
	flush := time.NewTicker(recording.FlushInterval)
	defer flush.Stop()
	s := &recShipper{a: a, row: row, w: w}
	// Records kept back for central's sequence still go out once it is known, after the last attachment too.
	defer s.settle(ctx, flush.C)
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
		done := a.pump(ctx, s, st, flush, prelude, sizes, &cols, &rows)
		_ = st.Close()
		if done {
			return
		}
	}
}

// recShipper numbers a recorder's records into its writer and ships them to central after every flush, across the
// recording's attachments.
type recShipper struct {
	a     *Agent
	row   runRow
	w     *recording.Writer
	batch []recording.Record
	// failing is whether the local disk was failing at the last flush.
	failing bool
	// gate keeps records back, unnumbered, while central's sequence of the run is unknown.
	gate seqGate
}

// seqGate holds a recorder's records before they are numbered, while this process does not know central's recording
// sequence of the run (a run it did not launch: adopted after a restart). A process that ran before may have numbered
// and shipped records its failing disk never stored, so the local recording can end below central's copy, and the
// acknowledgement saved on the same disk can be stale: records numbered from either would reuse sequence numbers central
// holds, and central would drop them as repeats (ADR-0097, addendum). Output is kept up to seqGateMaxBytes (counting
// heldRecordOverhead per record); past that it is dropped until the sequence is known and a gap record marks the
// place. Other records (sizes, the close) are always kept.
type seqGate struct {
	recs    []recording.Record
	bytes   int
	dropped bool
	probed  time.Time
}

// seqGateMaxBytes bounds the output a recorder keeps in memory while central's sequence is unknown.
const seqGateMaxBytes = 4 << 20

// seqProbeEvery is how often a recorder that waits for central's sequence asks for it again (probeSeq).
var seqProbeEvery = 2 * time.Second

func (g *seqGate) keep(r recording.Record) {
	if r.Kind == recording.KindOut {
		size := len(r.Data) + heldRecordOverhead
		if g.dropped || g.bytes+size > seqGateMaxBytes {
			if !g.dropped {
				g.dropped = true
				g.recs = append(g.recs, recording.Record{T: r.T, Kind: recording.KindGap, Why: "sequence-unknown"})
			}
			return
		}
		g.bytes += size
	}
	g.recs = append(g.recs, r)
}

func (g *seqGate) take() []recording.Record {
	recs := g.recs
	g.recs, g.bytes, g.dropped = nil, 0, false
	return recs
}

// add numbers a record (after what the gate kept back), or keeps it back while central's sequence is unknown.
func (s *recShipper) add(ctx context.Context, r recording.Record) {
	if !s.release() {
		if r.T == 0 {
			r.T = s.a.o.Clock.Now().UnixMicro() // when it was output, not when it is numbered
		}
		s.gate.keep(r)
		s.a.probeSeq(ctx, s.row, &s.gate)
		return
	}
	s.number(r)
}

// number adds a record to the writer. A record has a sequence number unless the quota dropped it; while the local disk
// fails it is numbered but not stored here, and central keeps it.
func (s *recShipper) number(r recording.Record) {
	if stored, _ := s.w.AddRecord(r); stored.Seq != 0 {
		s.batch = append(s.batch, stored)
	}
	s.truncated()
}

// truncated adds the writer's trunc record (quota reached) after the records before it, so central's copy shows where
// and why the recording ended; the run itself is never affected (SM-REC-004).
func (s *recShipper) truncated() {
	if tr, ok := s.w.Truncation(); ok {
		s.batch = append(s.batch, tr)
	}
}

// release moves the writer above what central holds (recordingBase) and numbers what the gate kept back; false while
// central's sequence is unknown. After a local loss (a failed disk, then a restart) central may hold higher sequence
// numbers than the local recording: the records continue above them rather than have central ignore them as repeats.
func (s *recShipper) release() bool {
	base, known := s.a.recordingBase(s.row.RunID)
	if !known {
		return false
	}
	if base > s.w.Seq() {
		s.w.SkipTo(base)
	}
	if len(s.gate.recs) > 0 {
		// An open record kept back above records central already holds (the local recording had none) is a size
		// change in central's copy, not its start.
		above := s.w.Seq() > 0
		for _, r := range s.gate.take() {
			if r.Kind == recording.KindOpen && above {
				r.Kind, r.RunID = recording.KindSize, ""
			}
			s.number(r)
		}
	}
	return true
}

// ship flushes the writer and ships the records numbered since the last flush. Shipping runs in the pump: a send may
// take up to its write timeout, but it never waits for a catch-up pass or another run's send (shipLive).
func (s *recShipper) ship(ctx context.Context) {
	if !s.release() {
		s.a.probeSeq(ctx, s.row, &s.gate)
	}
	_ = s.w.Flush()
	s.diskState()
	s.truncated()
	if len(s.batch) > 0 {
		// Central stores records in sequence order and ignores repeats, so a trunc record numbered while its
		// neighbour was being added must not overtake it.
		slices.SortFunc(s.batch, func(x, y recording.Record) int { return cmp.Compare(x.Seq, y.Seq) })
		s.a.shipLive(ctx, s.row.RunID, s.row.SpaceID, s.batch)
		s.batch = nil
	}
}

// diskState logs when the local disk starts failing and when the writer could store records again. Records keep going
// to central meanwhile (the writer numbers them without storing them), so the live view goes on.
func (s *recShipper) diskState() {
	switch err := s.w.Err(); {
	case err != nil && !s.failing:
		s.failing = true
		s.a.log.Warn("the local recording cannot be written: output still goes to central, and writing is retried",
			slog.String(obs.FieldRunID, s.row.RunID), obs.Err(err))
	case err == nil && s.failing:
		s.failing = false
		s.a.log.Info("the local recording is written again", slog.String(obs.FieldRunID, s.row.RunID))
	}
}

// settle runs after the recording's last attachment while the gate still keeps records back: it ships them once
// central's sequence is known. When the recorder stops first they are dropped, never numbered without it.
func (s *recShipper) settle(ctx context.Context, tick <-chan time.Time) {
	for len(s.gate.recs) > 0 && ctx.Err() == nil {
		select {
		case <-ctx.Done():
		case <-tick:
			s.ship(ctx)
		}
	}
}

// pump copies one attachment into the recording; it returns true when the program ended. Records are shipped to
// central after every flush (at most once a second, recShipper.ship).
// sizes are the terminal's size changes; cols and rows are the size last recorded, kept across reattaches.
func (a *Agent) pump(ctx context.Context, s *recShipper, st runner.Stream, flush *time.Ticker, prelude []recording.Record,
	sizes <-chan [2]int, cols, rows *int) bool {
	for _, r := range prelude {
		s.add(ctx, r)
	}
	defer s.ship(ctx)
	size := func(sz [2]int) {
		if sz[0] != *cols || sz[1] != *rows {
			*cols, *rows = sz[0], sz[1]
			s.add(ctx, recording.Record{Kind: recording.KindSize, Cols: sz[0], Rows: sz[1]})
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
			s.ship(ctx)
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
				s.add(ctx, recording.Record{Kind: recording.KindClose, ExitCode: c.closed.ExitCode, Reason: c.closed.Reason})
				return true
			case c.err != nil:
				return false
			default:
				s.add(ctx, recording.Record{Kind: recording.KindOut, Data: c.data})
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
// recording). Close and trunc records are always kept (one numbered while the local disk failed has no local copy);
// others only up to heldMaxBytes, past which lost is set and the catch-up reads the local recording again (output
// the failing disk did not store is then a gap in central's copy).
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
// each sequence once, so re-sent records are harmless. Runs are caught up one at a time, and a run's live output is
// kept back until its turn (shipLive), so a sweep serves the runs that wait first: live runs before ended ones, and
// before each further run any run whose live output waits meanwhile (catchUpWaiting). After an agent restart no
// acknowledgement is known and every journaled run's recording is re-shipped: without that order the live terminals
// stayed blank until the whole history of the ended runs had gone out again.
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
	// Live runs first: their output is what viewers wait for.
	slices.SortStableFunc(runs, func(x, y runRow) int { return cmp.Compare(endedRank(x), endedRank(y)) })
	journaled := map[string]bool{}
	served := map[string]bool{}
	for _, r := range runs {
		journaled[r.RunID] = true
		if err := a.catchUpWaiting(ctx, conn, gen, served); err != nil {
			return err
		}
		if served[r.RunID] {
			continue
		}
		a.restoreAck(r.RunID)
		a.mu.Lock()
		acked, known := a.acked[r.RunID]
		dirty := a.unshipped[r.RunID] || a.held[r.RunID] != nil
		// A restored acknowledgement says what central stored, not what this process sent: the local recording may
		// hold records above it (catchUpRun compares).
		clean := !dirty && (a.caughtUp[r.RunID] || known && !a.restored[r.RunID] && a.sentSeq[r.RunID] <= acked)
		if clean && a.catchUpGen == gen {
			// Everything sent so far was acknowledged (or this pass sent it): nothing to re-ship.
			a.caughtUp[r.RunID] = true
		}
		a.mu.Unlock()
		if clean {
			continue
		}
		if err := a.catchUpRun(ctx, conn, gen, r, acked, served, false); err != nil {
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
	// Records of IDs that are not journaled (pruned runs are only deleted with their recording fully shipped, so
	// none in practice) are dropped, which keeps a stray entry from making the pass sweep forever.
	for id := range a.held {
		if !journaled[id] {
			delete(a.held, id)
			delete(a.unshipped, id)
		}
	}
	a.mu.Unlock()
	return nil
}

func endedRank(r runRow) int {
	if ended(r.State) {
		return 1
	}
	return 0
}

// catchUpWaiting catches up the runs whose live output waits for the pass (shipLive marked them or kept records back)
// ahead of the rest of the sweep, each at most once per sweep (served). It runs before each run of the sweep and
// between the batches of each run's catch-up, so a run's live output never waits for another run's backlog, only for
// the batch on its way. A run not in the journal is left to the end of the sweep.
func (a *Agent) catchUpWaiting(ctx context.Context, conn *websocket.Conn, gen uint64, served map[string]bool) error {
	for {
		a.mu.Lock()
		var waiting []string
		for id := range a.unshipped {
			if !served[id] {
				waiting = append(waiting, id)
			}
		}
		for id := range a.held {
			if !served[id] && !a.unshipped[id] {
				waiting = append(waiting, id)
			}
		}
		a.mu.Unlock()
		if len(waiting) == 0 {
			return nil
		}
		slices.Sort(waiting)
		for _, id := range waiting {
			r, err := a.j.getRun(ctx, id)
			if err != nil {
				served[id] = true
				continue
			}
			a.restoreAck(id)
			if err := a.catchUpRun(ctx, conn, gen, r, a.ackedSeq(id), served, true); err != nil {
				return err
			}
		}
	}
}

// shipBatchHook, when set (tests), runs after each batch a catch-up ships.
var shipBatchHook func(runID string, batch int)

// endedCatchUpRate bounds how fast a catch-up re-ships an ended run's recording, in bytes of output per second (each
// record counted with heldRecordOverhead). Ended runs have no viewer waiting for live output, and after an agent
// restart without known acknowledgements their history can be large (ADR-0097); live runs are never paced.
var endedCatchUpRate int64 = 8 << 20

// catchUpRun ships a run's local records above from in batches, then what live shipments kept back meanwhile, until
// nothing more was kept back, and marks the run caught up (atomically with that check, under the lock shipLive decides
// under). Between batches it serves the other runs whose live output waits (catchUpWaiting with served, which this run
// joins), and an ended run's batches are paced by endedCatchUpRate, unless it is served nested inside another run's
// catch-up (nested): pacing there would hold the outer run's waiting live output back at the pace of an ended run.
func (a *Agent) catchUpRun(ctx context.Context, conn *websocket.Conn, gen uint64, r runRow, from uint64, served map[string]bool, nested bool) error {
	served[r.RunID] = true
	batch := 0
	paced := ended(r.State) && !nested
	began, sentBytes := time.Now(), int64(0)
	others := func() error { return a.catchUpWaiting(ctx, conn, gen, served) }
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
			if err := others(); err != nil {
				return err
			}
			if paced {
				for _, rec := range recs[start:end] {
					sentBytes += int64(len(rec.Data)) + heldRecordOverhead
				}
				if err := paceCatchUp(ctx, began, sentBytes, others); err != nil {
					return err
				}
			}
		}
		return nil
	}
	readDisk := func() error {
		// Nothing above from on disk (the usual case for a run whose acknowledgement was restored after a restart):
		// the last segment says so without decoding the whole recording.
		if last, err := recording.LastSeqIn(a.RecordingDir(r.RunID)); err == nil && last <= from {
			return nil
		}
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
			// Everything on disk above the restored acknowledgement was sent: sentSeq and acked decide from now on.
			delete(a.restored, r.RunID)
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

// paceCatchUp waits until sent bytes fit endedCatchUpRate since began, serving runs whose live output waits (others)
// at least every 50 ms meanwhile.
func paceCatchUp(ctx context.Context, began time.Time, sent int64, others func() error) error {
	if endedCatchUpRate <= 0 {
		return nil
	}
	due := began.Add(time.Duration(float64(sent) / float64(endedCatchUpRate) * float64(time.Second)))
	for {
		wait := time.Until(due)
		if wait <= 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(min(wait, 50*time.Millisecond)):
		}
		if err := others(); err != nil {
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

// recordingAck takes central's acknowledgement of a run's recording: the highest sequence it stores. The first one
// this process gets for a run also tells it central's sequence (seqKnown), which a recorder of a run it did not launch
// waits for before numbering anything (seqGate). A first acknowledgement below the one restored from the run
// directory is authoritative: central lost records it had acknowledged (a restore, a lost disk), or acknowledged
// more than it stored (before central acknowledged only what it durably stored). The run's acknowledgement is
// lowered, so a catch-up re-ships what the local recording holds above it, while new records are still numbered above
// the restored value (seqFloor): a jump is a gap, a reused number would be dropped (ADR-0097, addendum). Later
// acknowledgements only ever raise it: one older than another arrives on the same connection behind it.
func (a *Agent) recordingAck(ack proto.RecordingAck) {
	a.mu.Lock()
	if a.acked == nil {
		a.acked = map[string]uint64{}
	}
	if a.seqKnown == nil {
		a.seqKnown = map[string]bool{}
	}
	first := !a.seqKnown[ack.RunID]
	a.seqKnown[ack.RunID] = true
	prev, had := a.acked[ack.RunID]
	lowered := first && had && ack.Seq < prev
	advanced := ack.Seq > prev
	start := false
	switch {
	case lowered:
		if a.seqFloor == nil {
			a.seqFloor = map[string]uint64{}
		}
		a.seqFloor[ack.RunID] = max(a.seqFloor[ack.RunID], prev)
		a.acked[ack.RunID] = ack.Seq
		a.markUnshippedLocked(ack.RunID)
		start = a.connected && a.startCatchUpLocked()
	case advanced:
		a.acked[ack.RunID] = ack.Seq
	}
	a.mu.Unlock()
	if lowered || advanced {
		a.saveAck(ack.RunID, ack.Seq)
	}
	if lowered {
		a.log.Warn("central holds less of a run's recording than it acknowledged before: re-shipping the local recording above what it holds",
			slog.String(obs.FieldRunID, ack.RunID), slog.Uint64("acknowledged", prev), slog.Uint64("holds", ack.Seq))
	}
	if start {
		go a.catchUpRecordings()
	}
}

// recordingBase is the sequence a run's new records are numbered above (central's acknowledgement, or the restored
// one it lowered) and whether central's sequence is known to this process at all.
func (a *Agent) recordingBase(runID string) (uint64, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return max(a.acked[runID], a.seqFloor[runID]), a.seqKnown[runID]
}

// launchedRecording notes that this process started a run's program with no recording on disk: central cannot hold
// records of it that this process did not number, so its recorder numbers from the start without asking.
func (a *Agent) launchedRecording(runID string) {
	if segs, err := recording.Segments(a.RecordingDir(runID)); err != nil && !errors.Is(err, fs.ErrNotExist) || len(segs) > 0 {
		return
	}
	if _, err := os.Stat(filepath.Join(a.runDir(runID), recordingAckedFile)); err == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.seqKnown == nil {
		a.seqKnown = map[string]bool{}
	}
	a.seqKnown[runID] = true
}

// probeSeq asks central for its recording sequence of a run: an empty recording message, which every central version
// applies (storing nothing) and answers with a recording.ack like any other. At most once per seqProbeEvery, and only
// while connected; a lost probe or answer is asked again.
func (a *Agent) probeSeq(ctx context.Context, row runRow, g *seqGate) {
	now := time.Now()
	if !g.probed.IsZero() && now.Sub(g.probed) < seqProbeEvery {
		return
	}
	a.mu.Lock()
	conn, up := a.conn, a.connected
	a.mu.Unlock()
	if !up || conn == nil {
		return
	}
	g.probed = now
	_ = a.sendOn(ctx, conn, proto.TypeRecording, proto.RecordingChunk{RunID: row.RunID, SpaceID: row.SpaceID, Records: []json.RawMessage{}})
}

// recordingAckedFile keeps central's last acknowledged recording sequence of a run, so a restarted agent's catch-up
// pass skips what central already stores instead of re-shipping every journaled recording (ADR-0097).
const recordingAckedFile = "recording-acked"

// ackMargin is how far above its local recording a stored acknowledgement may be and still be believed (restoreAck):
// records central stored from before a local loss, plus the ones a failed disk never kept.
const ackMargin = 1 << 20

// saveAck stores a run's acknowledged sequence in its run directory (replaced whole, so a reader never sees half a
// number). A failed write (a full disk) only leaves an older value: the catch-up then re-ships a little more.
func (a *Agent) saveAck(runID string, seq uint64) {
	// Serialised with removeRecording's cleanup: an acknowledgement written between its last file removal and the
	// directory's removal would leave a run directory holding only this file.
	a.ackFileMu.Lock()
	defer a.ackFileMu.Unlock()
	dir := a.runDir(runID)
	if _, err := os.Stat(dir); err != nil {
		return // pruned meanwhile: never recreate the directory
	}
	tmp := filepath.Join(dir, recordingAckedFile+".tmp")
	if err := os.WriteFile(tmp, []byte(strconv.FormatUint(seq, 10)), 0o600); err != nil {
		_ = os.Remove(tmp)
		return
	}
	if err := os.Rename(tmp, filepath.Join(dir, recordingAckedFile)); err != nil {
		_ = os.Remove(tmp)
	}
}

// restoreAck loads a run's stored acknowledgement when this process has none (after a restart). The run is marked
// restored until a catch-up compared it with the local recording.
func (a *Agent) restoreAck(runID string) {
	a.mu.Lock()
	_, known := a.acked[runID]
	a.mu.Unlock()
	if known {
		return
	}
	b, err := os.ReadFile(filepath.Join(a.runDir(runID), recordingAckedFile)) //nolint:gosec // a fixed name in the run's own directory
	if err != nil {
		return
	}
	seq, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	if err != nil || seq == 0 {
		return
	}
	// Central's sequence cannot be far above what this node ever numbered: a value near the top of the range or far
	// above the local recording would make later records skip numbers or never ship, so it is not trusted.
	if seq >= math.MaxUint64-ackMargin {
		return
	}
	if last, err := recording.LastSeqIn(a.RecordingDir(runID)); err == nil && seq > last+ackMargin {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, known := a.acked[runID]; known {
		return
	}
	if a.acked == nil {
		a.acked = map[string]uint64{}
	}
	if a.restored == nil {
		a.restored = map[string]bool{}
	}
	a.acked[runID], a.restored[runID] = seq, true
}
