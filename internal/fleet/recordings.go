package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/artifacts"
	"github.com/aurimasniekis/go-spaceman/internal/obs"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/recording"
)

// Recordings is central's copy of node recordings (SM-REC-002/003): records shipped by nodes are stored per run under
// <data>/recordings/<run>/ in the same segment format, and fanned out to live viewers. Viewers never apply
// backpressure to nodes: each viewer has a bounded queue and is marked lagged (it then catches up from storage)
// instead of slowing the shipper (SM-REC-004).
//
// The run folder is a spool while the node ships the recording (S3 has no append). With the local artifact store it
// is also where the recording stays. With another store (S3, ADR-0048) a recording is archived when it closes or its
// run ends: every segment is uploaded to recordings/<run>/<segment> and then removed locally. Reads merge both places
// by segment name, so a recording reads the same while it is being archived, and records arriving after the archive
// (a node re-shipping its tail) continue in a new local segment numbered after the archived ones.
type Recordings struct {
	dir   string
	store artifacts.Store
	// archive is set when the store is not the spool folder itself.
	archive bool
	log     *slog.Logger
	mu      sync.Mutex
	w       map[string]*recording.Writer
	// subs are live viewers per run.
	subs map[string]map[*Viewer]struct{}
	// archiving marks runs whose segments are being uploaded; the channel closes when that ends.
	archiving map[string]chan struct{}
	now       func() time.Time
	ctx       context.Context // cancelled by Close: ends archive uploads
	cancel    context.CancelFunc
	wg        sync.WaitGroup
}

// Viewer is one live subscription.
type Viewer struct {
	C chan recording.Record
	// lagged is set by apply (under r.mu) and read by the viewer's own goroutine, so it is atomic.
	lagged atomic.Bool
	run    string
}

// Lagged reports whether the viewer dropped records (it must re-read from storage after its last seq).
func (v *Viewer) Lagged() bool { return v.lagged.Load() }

// TakeLagged reports whether the viewer dropped records and clears the flag, so a viewer catches up from storage once
// per lag and then streams live records again. Clear it before re-reading: records dropped during the re-read set it
// again.
func (v *Viewer) TakeLagged() bool { return v.lagged.Swap(false) }

var reRunID = regexp.MustCompile(`^run_[0-9a-z]{26}$`)

func newRecordings(dir string, store artifacts.Store, log *slog.Logger, now func() time.Time) *Recordings {
	if store == nil {
		store = artifacts.NewLocal(map[string]string{artifacts.NSRecordings: dir})
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	archive := true
	if l, ok := store.(*artifacts.Local); ok && dir != "" && l.Root(artifacts.NSRecordings) == filepath.Clean(dir) {
		archive = false
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Recordings{dir: dir, store: store, archive: archive, log: log, w: map[string]*recording.Writer{}, subs: map[string]map[*Viewer]struct{}{},
		archiving: map[string]chan struct{}{}, now: now, ctx: ctx, cancel: cancel}
}

// Dir returns a run's spool folder.
func (r *Recordings) Dir(runID string) (string, error) {
	if !reRunID.MatchString(runID) {
		return "", errors.New("invalid run id")
	}
	return filepath.Join(r.dir, runID), nil
}

// waitArchiveLocked waits, with r.mu held (and released while waiting), until no archive of the run is running.
func (r *Recordings) waitArchiveLocked(runID string) {
	for {
		ch := r.archiving[runID]
		if ch == nil {
			return
		}
		r.mu.Unlock()
		<-ch
		r.mu.Lock()
	}
}

func (r *Recordings) writer(runID string) (*recording.Writer, error) {
	r.waitArchiveLocked(runID)
	if w := r.w[runID]; w != nil {
		return w, nil
	}
	dir, err := r.Dir(runID)
	if err != nil {
		return nil, err
	}
	o := recording.Options{Dir: dir, Now: r.now}
	if r.archive {
		if local, _ := recording.Segments(dir); len(local) == 0 {
			// Continue an archived recording rather than renumber it (records at or below its last sequence are
			// re-shipments and are skipped).
			if o.ResumeSegment, o.ResumeSeq, err = r.archivedTail(r.ctx, runID); err != nil {
				return nil, err
			}
		}
	}
	w, err := recording.Open(o)
	if err != nil {
		return nil, err
	}
	r.w[runID] = w
	return w, nil
}

// archivedTail returns the highest archived segment number of a run and the last sequence number in it.
func (r *Recordings) archivedTail(ctx context.Context, runID string) (int, uint64, error) {
	objs, err := r.store.List(ctx, artifacts.RecordingPrefix(runID))
	if err != nil || len(objs) == 0 {
		return 0, 0, err
	}
	last := objs[len(objs)-1]
	n, _ := recording.SegmentIndex(filepath.Base(last.Key))
	o, err := r.store.Open(ctx, last.Key)
	if err != nil {
		return 0, 0, err
	}
	defer o.Close()
	return n, recording.LastSeq(o), nil
}

// apply stores a chunk and returns the highest stored sequence.
func (r *Recordings) apply(c proto.RecordingChunk) (uint64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	w, err := r.writer(c.RunID)
	if err != nil {
		return 0, err
	}
	closed := false
	for _, raw := range c.Records {
		var rec recording.Record
		if json.Unmarshal(raw, &rec) != nil || rec.Seq == 0 {
			continue
		}
		stored, err := w.Append(rec)
		if err != nil {
			return w.Seq(), err
		}
		if !stored {
			continue
		}
		for v := range r.subs[c.RunID] {
			select {
			case v.C <- rec:
			default:
				v.lagged.Store(true)
			}
		}
		if rec.Kind == recording.KindClose {
			closed = true
		}
	}
	if err := w.Flush(); err != nil {
		return w.Seq(), err
	}
	seq := w.Seq()
	if closed {
		_ = w.Close()
		delete(r.w, c.RunID)
		r.archiveLater(c.RunID)
	}
	return seq, nil
}

// archiveLater archives a run's recording in the background (nothing to do with the local store).
func (r *Recordings) archiveLater(runID string) {
	if !r.archive || r.ctx.Err() != nil {
		return
	}
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		if err := r.Archive(r.ctx, runID); err != nil && r.ctx.Err() == nil {
			r.log.Warn("recording not archived to the artifact store; it stays in the local spool and is retried at the next start",
				slog.String("run.id", runID), obs.Err(err))
		}
	}()
}

// RunEnded closes a run's open writer (the node may never send its close record) and archives the recording.
func (r *Recordings) RunEnded(runID string) {
	if !reRunID.MatchString(runID) {
		return
	}
	r.mu.Lock()
	if w := r.w[runID]; w != nil {
		_ = w.Close()
		delete(r.w, runID)
	}
	r.mu.Unlock()
	r.archiveLater(runID)
}

// Archive uploads a run's spooled segments to the artifact store and removes them locally. A recording that is still
// being written is left alone (it is archived when it closes). With the local store there is nothing to do.
func (r *Recordings) Archive(ctx context.Context, runID string) error {
	if !r.archive {
		return nil
	}
	dir, err := r.Dir(runID)
	if err != nil {
		return err
	}
	r.mu.Lock()
	if r.w[runID] != nil || r.archiving[runID] != nil {
		r.mu.Unlock()
		return nil
	}
	ch := make(chan struct{})
	r.archiving[runID] = ch
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.archiving, runID)
		r.mu.Unlock()
		close(ch)
	}()
	segs, err := recording.Segments(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	for _, p := range segs {
		if err := artifacts.PutFile(ctx, r.store, artifacts.RecordingKey(runID, filepath.Base(p)), p, 0, ""); err != nil {
			return err
		}
	}
	_ = os.Remove(dir) // only when empty
	return nil
}

// ArchiveSpooled archives every spooled recording whose run has ended (ended reports it): those left by a failed
// upload or a stop before the upload finished. It returns how many recordings it archived.
func (r *Recordings) ArchiveSpooled(ctx context.Context, ended func(ctx context.Context, runID string) bool) (int, error) {
	if !r.archive {
		return 0, nil
	}
	ents, err := os.ReadDir(r.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	} else if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range ents {
		if !e.IsDir() || !reRunID.MatchString(e.Name()) || !ended(ctx, e.Name()) {
			continue
		}
		if err := r.Archive(ctx, e.Name()); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// Segments returns the names of a run's stored segments in order: those in the spool and those in the store.
func (r *Recordings) Segments(ctx context.Context, runID string) ([]string, error) {
	dir, err := r.Dir(runID)
	if err != nil {
		return nil, err
	}
	names := map[string]bool{}
	ents, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	for _, e := range ents {
		if recording.IsSegment(e.Name()) {
			names[e.Name()] = true
		}
	}
	if r.archive {
		objs, err := r.store.List(ctx, artifacts.RecordingPrefix(runID))
		if err != nil {
			return nil, err
		}
		for _, o := range objs {
			names[filepath.Base(o.Key)] = true
		}
	}
	out := make([]string, 0, len(names))
	for n := range names {
		out = append(out, n)
	}
	sort.Strings(out)
	return out, nil
}

// OpenSegment opens one segment of a run: the spool file, else the archived object.
func (r *Recordings) OpenSegment(ctx context.Context, runID, name string) (io.ReadCloser, error) {
	dir, err := r.Dir(runID)
	if err != nil {
		return nil, err
	}
	if !recording.IsSegment(name) {
		return nil, errors.New("invalid segment name")
	}
	f, err := os.Open(filepath.Join(dir, name)) //nolint:gosec // a validated run ID and segment name below the spool
	switch {
	case err == nil:
		return f, nil
	case !errors.Is(err, fs.ErrNotExist) || !r.archive:
		return nil, err
	}
	return r.store.Open(ctx, artifacts.RecordingKey(runID, name))
}

// Subscribe registers a live viewer of a run.
func (r *Recordings) Subscribe(runID string) *Viewer {
	v := &Viewer{C: make(chan recording.Record, 512), run: runID}
	r.mu.Lock()
	if r.subs[runID] == nil {
		r.subs[runID] = map[*Viewer]struct{}{}
	}
	r.subs[runID][v] = struct{}{}
	r.mu.Unlock()
	return v
}

// Unsubscribe removes a viewer.
func (r *Recordings) Unsubscribe(v *Viewer) {
	r.mu.Lock()
	delete(r.subs[v.run], v)
	if len(r.subs[v.run]) == 0 {
		delete(r.subs, v.run)
	}
	r.mu.Unlock()
}

// Read returns stored records after a sequence.
func (r *Recordings) Read(ctx context.Context, runID string, after uint64) (recording.ReadResult, error) {
	names, open, err := r.stored(ctx, runID)
	if err != nil || len(names) == 0 {
		return recording.ReadResult{Records: []recording.Record{}}, err
	}
	return recording.ReadSegments(names, open, after)
}

// Scan calls fn with each stored record after a sequence, one at a time, so a reader of a long recording holds only
// the record in hand (a live terminal's catch-up).
func (r *Recordings) Scan(ctx context.Context, runID string, after uint64, fn func(recording.Record) error) (recording.ReadResult, error) {
	names, open, err := r.stored(ctx, runID)
	if err != nil || len(names) == 0 {
		return recording.ReadResult{}, err
	}
	return recording.ScanSegments(names, open, after, fn)
}

// Tail returns the end of a run's stored recording whose output fits budget bytes, and the state a terminal needs to
// draw it (ADR-0081).
func (r *Recordings) Tail(ctx context.Context, runID string, budget int64) (recording.TailResult, error) {
	names, open, err := r.stored(ctx, runID)
	if err != nil || len(names) == 0 {
		return recording.TailResult{Records: []recording.Record{}}, err
	}
	return recording.TailSegments(names, open, budget)
}

// stored flushes the run's writer and returns its segment names and how to open them.
func (r *Recordings) stored(ctx context.Context, runID string) ([]string, func(string) (io.ReadCloser, error), error) {
	if _, err := r.Dir(runID); err != nil {
		return nil, nil, err
	}
	r.mu.Lock()
	if w := r.w[runID]; w != nil {
		_ = w.Flush()
	}
	r.mu.Unlock()
	names, err := r.Segments(ctx, runID)
	if err != nil {
		return nil, nil, err
	}
	return names, func(name string) (io.ReadCloser, error) { return r.OpenSegment(ctx, runID, name) }, nil
}

// Delete removes a run's stored recording (permission recording:delete), in the spool and in the store. The node's
// local copy is separate.
func (r *Recordings) Delete(ctx context.Context, runID string) error {
	dir, err := r.Dir(runID)
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.waitArchiveLocked(runID)
	if w := r.w[runID]; w != nil {
		_ = w.Close()
		delete(r.w, runID)
	}
	r.mu.Unlock()
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if !r.archive {
		return nil
	}
	objs, err := r.store.List(ctx, artifacts.RecordingPrefix(runID))
	if err != nil {
		return err
	}
	for _, o := range objs {
		if err := r.store.Delete(ctx, o.Key); err != nil {
			return err
		}
	}
	return nil
}

// Close stops archive uploads in progress (they are retried at the next start) and flushes every open writer.
func (r *Recordings) Close() {
	r.cancel()
	r.wg.Wait()
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, w := range r.w {
		_ = w.Close()
		delete(r.w, id)
	}
}
