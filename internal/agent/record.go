package agent

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/obs"
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
		if first {
			_ = w.Add(recording.Record{Kind: recording.KindOpen, Cols: p.Cols, Rows: p.Rows, RunID: row.RunID})
			first = false
		} else {
			_ = w.Add(recording.Record{Kind: recording.KindGap, Why: "reattach"})
			if p.Cols != cols || p.Rows != rows {
				_ = w.Add(recording.Record{Kind: recording.KindSize, Cols: p.Cols, Rows: p.Rows})
			}
		}
		cols, rows = p.Cols, p.Rows
		done := a.pump(ctx, st, w, flush)
		_ = st.Close()
		if done {
			return
		}
	}
}

// pump copies one attachment into the recording; it returns true when the program ended.
func (a *Agent) pump(ctx context.Context, st runner.Stream, w *recording.Writer, flush *time.Ticker) bool {
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
			_ = w.Flush()
		case c, ok := <-ch:
			if !ok {
				return false
			}
			switch {
			case c.closed != nil:
				_ = w.Add(recording.Record{Kind: recording.KindClose, ExitCode: c.closed.ExitCode, Reason: c.closed.Reason})
				_ = w.Flush()
				return true
			case c.err != nil:
				_ = w.Flush()
				return false
			default:
				_ = w.Add(recording.Record{Kind: recording.KindOut, Data: c.data})
			}
		}
	}
}
