package agent

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/obs"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/recording"
	"github.com/aurimasniekis/go-spaceman/internal/runner"
)

// RecordingDir is a run's local recording directory.
func (a *Agent) RecordingDir(runID string) string { return filepath.Join(a.runDir(runID), "recording") }

// record attaches to a run's terminal as an observer and writes everything it outputs until the program ends. It is
// independent of browsers (SM-REC-002): live viewers read the recording, never the other way round, so viewer
// backpressure cannot slow the run (SM-REC-004). Interruptions (overflow, runner restart) re-attach and record a gap,
// because herdr-core streams carry no offsets (S-3).
func (a *Agent) record(ctx context.Context, row runRow) {
	log := a.log.With(slog.String(obs.FieldRunID, row.RunID))
	w, err := recording.Open(recording.Options{Dir: a.RecordingDir(row.RunID), MaxBytes: 512 << 20, Now: a.o.Clock.Now})
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
		done := a.pump(ctx, row, st, w, flush, prelude)
		_ = st.Close()
		if done {
			return
		}
	}
}

// pump copies one attachment into the recording; it returns true when the program ended. Records are shipped to
// central after every flush (at most once a second); shipping never blocks recording.
func (a *Agent) pump(ctx context.Context, row runRow, st runner.Stream, w *recording.Writer, flush *time.Ticker, prelude []recording.Record) bool {
	var batch []recording.Record
	add := func(r recording.Record) {
		if stored, err := w.AddRecord(r); err == nil && stored.Seq != 0 {
			batch = append(batch, stored)
		}
	}
	for _, r := range prelude {
		add(r)
	}
	ship := func() {
		_ = w.Flush()
		if len(batch) > 0 {
			a.ship(ctx, row.RunID, row.SpaceID, batch)
			batch = nil
		}
	}
	defer ship()
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
		case c, ok := <-ch:
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

// ship sends records to central; when that fails the run is caught up from the local recording after reconnect.
func (a *Agent) ship(ctx context.Context, runID, spaceID string, recs []recording.Record) {
	raws := make([]json.RawMessage, 0, len(recs))
	for _, r := range recs {
		b, err := json.Marshal(r)
		if err == nil {
			raws = append(raws, b)
		}
	}
	if err := a.send(ctx, proto.TypeRecording, proto.RecordingChunk{RunID: runID, SpaceID: spaceID, Records: raws}); err != nil {
		a.markUnshipped(runID)
	}
}

func (a *Agent) markUnshipped(runID string) {
	a.mu.Lock()
	if a.unshipped == nil {
		a.unshipped = map[string]bool{}
	}
	a.unshipped[runID] = true
	a.mu.Unlock()
}

// catchUpRecordings re-ships local recordings after the last acknowledged sequence (after connecting). Central stores
// each sequence once, so overlap is harmless.
func (a *Agent) catchUpRecordings(ctx context.Context) {
	runs, err := a.j.runs(ctx, false)
	if err != nil {
		return
	}
	for _, r := range runs {
		a.mu.Lock()
		acked, known := a.acked[r.RunID]
		dirty := a.unshipped[r.RunID]
		delete(a.unshipped, r.RunID)
		a.mu.Unlock()
		if known && !dirty {
			continue
		}
		res, err := recording.Read(a.RecordingDir(r.RunID), acked)
		if err != nil || len(res.Records) == 0 {
			continue
		}
		for start := 0; start < len(res.Records); start += 256 {
			end := min(start+256, len(res.Records))
			a.ship(ctx, r.RunID, r.SpaceID, res.Records[start:end])
		}
	}
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
