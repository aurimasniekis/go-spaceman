package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/clock"
)

// Stages of a backup run, in order (ADR-0091). The current one is shown in the backup status while the run is busy.
const (
	StageMemoryFlush = "memory_flush" // asking the nodes to checkpoint and sync role memory
	StageSnapshot    = "snapshot"     // VACUUM INTO and sealing the database copy
	StageFiles       = "files"        // sealing recordings, attachments and transcripts on disk
	StageObjects     = "objects"      // copying the objects of a remote artifact store
	StageMemory      = "memory"       // bundling role-memory repositories
	StageManifest    = "manifest"     // the configuration, the manifest and the final fsync
	StageUpload      = "upload"       // storing the backup in the target
	StageRecord      = "record"       // recording the outcome
	StagePrune       = "prune"        // retention after a successful backup
	StageFetch       = "fetch"        // verification: fetching the backup from the target
	StageVerify      = "verify"       // verification: decrypting and checking every object
)

// Default bounds of a backup run (ADR-0091).
const (
	// DefaultStallTimeout fails a backup or verification whose current stage made no progress (a stage change, a
	// file sealed or uploaded, or bytes moved to or from object storage) for this long.
	DefaultStallTimeout = 10 * time.Minute
	// DefaultMaxDuration fails a backup or verification that runs longer than this in total.
	DefaultMaxDuration = time.Hour
	// recordTimeout bounds recording the outcome and retention after the work itself ended.
	recordTimeout = 2 * time.Minute
	// DefaultMemoryFlushTimeout bounds the role-memory flush before a backup when the service is not given a bound.
	DefaultMemoryFlushTimeout = 2 * time.Minute
	// memoryFlushAbandonGrace is how long a backup still waits for a memory flush to return after its bound passed.
	memoryFlushAbandonGrace = 5 * time.Second
)

// Progress is where a running backup or verification is.
type Progress struct {
	ID    string `json:"id" doc:"The backup being made or verified"`
	Stage string `json:"stage" enum:"memory_flush,snapshot,files,objects,memory,manifest,upload,record,prune,fetch,verify"`
	// StageStartedAt is when the current stage began.
	StageStartedAt time.Time `json:"stage_started_at"`
	// StageFiles counts the files sealed, uploaded or checked in the current stage so far.
	StageFiles int `json:"stage_files"`
	// LastProgressAt is the last time the run moved forward (a stage change, a file, or bytes to or from object storage).
	LastProgressAt time.Time `json:"last_progress_at"`
}

// StalledError is the cause of a backup or verification stopped by its watchdog.
type StalledError struct {
	Stage string
	Idle  time.Duration
	Total bool // the overall limit was reached rather than the stall limit
}

func (e *StalledError) Error() string {
	if e.Total {
		return fmt.Sprintf("backup stopped in stage %s: it ran longer than the %s limit", e.Stage, e.Idle)
	}
	return fmt.Sprintf("backup stopped in stage %s: no progress for %s", e.Stage, e.Idle)
}

// tracker follows one run through its stages: it logs each stage at Info, counts files and remembers the last
// progress for the watchdog. Its methods are safe on a nil tracker (Create without a service).
type tracker struct {
	mu      sync.Mutex
	id      string
	stage   string
	stageAt time.Time // clock time, for display
	started time.Time // monotonic, for the stage duration
	last    time.Time // monotonic, for the watchdog
	lastAt  time.Time // clock time, for display
	files   int
	clk     clock.Clock
	log     *slog.Logger
}

func newTracker(id string, clk clock.Clock, log *slog.Logger) *tracker {
	now := time.Now()
	return &tracker{id: id, clk: clk, log: log, started: now, last: now, lastAt: clk.Now()}
}

type trackerKey struct{}

func withTracker(ctx context.Context, t *tracker) context.Context {
	return context.WithValue(ctx, trackerKey{}, t)
}

func trackerFrom(ctx context.Context) *tracker {
	t, _ := ctx.Value(trackerKey{}).(*tracker)
	return t
}

// enter ends the current stage (logged with its file count and duration) and begins stage.
func (t *tracker) enter(stage string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	prev, files, took := t.stage, t.files, time.Since(t.started)
	now := time.Now()
	t.stage, t.stageAt, t.started, t.last, t.lastAt, t.files = stage, t.clk.Now(), now, now, t.clk.Now(), 0
	t.mu.Unlock()
	if prev != "" {
		t.log.Info("backup stage finished", slog.String("stage", prev), slog.Int("files", files), slog.Duration("took", took))
	}
	t.log.Info("backup stage started", slog.String("stage", stage))
}

// end logs the end of the last stage.
func (t *tracker) end() {
	if t == nil {
		return
	}
	t.mu.Lock()
	prev, files, took := t.stage, t.files, time.Since(t.started)
	t.mu.Unlock()
	if prev != "" {
		t.log.Info("backup stage finished", slog.String("stage", prev), slog.Int("files", files), slog.Duration("took", took))
	}
}

// file counts one file done in the current stage.
func (t *tracker) file() {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.files++
	t.last, t.lastAt = time.Now(), t.clk.Now()
	t.mu.Unlock()
}

// ReportProgress records progress of the backup whose context ctx is (a node answered the memory flush, or the wait
// for it ended). It does nothing outside a backup.
func ReportProgress(ctx context.Context) {
	t := trackerFrom(ctx)
	if t == nil {
		return
	}
	t.mu.Lock()
	t.last, t.lastAt = time.Now(), t.clk.Now()
	t.mu.Unlock()
}

// touch records progress within a file (bytes moved).
func (t *tracker) touch() {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.last = time.Now()
	t.mu.Unlock()
}

// idle returns the current stage and how long it has made no progress.
func (t *tracker) idle() (string, time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.stage, time.Since(t.last)
}

func (t *tracker) progress() *Progress {
	t.mu.Lock()
	defer t.mu.Unlock()
	return &Progress{ID: t.id, Stage: t.stage, StageStartedAt: t.stageAt.UTC(), StageFiles: t.files, LastProgressAt: t.lastAt.UTC()}
}

// reader records progress for every read that returns data. Without a tracker it returns r unchanged.
func (t *tracker) reader(r io.Reader) io.Reader {
	if t == nil {
		return r
	}
	return &progressReader{r: r, t: t}
}

type progressReader struct {
	r io.Reader
	t *tracker
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.t.touch()
	}
	return n, err
}

// watch derives a context from ctx that ends with a *StalledError cause when t makes no progress for stall, or when
// total has passed. The returned stop function ends the watchdog; it must be called and may be called again.
func watch(ctx context.Context, t *tracker, stall, total time.Duration) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(ctx)
	done := make(chan struct{})
	tick := min(max(stall/8, time.Millisecond), 10*time.Second)
	go func() {
		tk := time.NewTicker(tick)
		defer tk.Stop()
		deadline := time.NewTimer(total)
		defer deadline.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-deadline.C:
				stage, _ := t.idle()
				cancel(&StalledError{Stage: stage, Idle: total, Total: true})
				return
			case <-tk.C:
				if stage, d := t.idle(); d >= stall {
					cancel(&StalledError{Stage: stage, Idle: stall})
					return
				}
			}
		}
	}()
	var once sync.Once
	return ctx, func() {
		once.Do(func() {
			close(done)
			cancel(nil)
		})
	}
}

// stalled returns the watchdog's cause when it stopped ctx, else nil.
func stalled(ctx context.Context) *StalledError {
	var se *StalledError
	if errors.As(context.Cause(ctx), &se) {
		return se
	}
	return nil
}
