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
	// resync holds, per run whose writer failed, the node connection the failure happened on (SM-REC-003, ADR-0010
	// addendum): on that connection nothing is stored past the gap the failure left (records already on their way
	// there must not overtake the records central lost); the node's re-ship on its next connection clears it.
	resync map[string]uint64
	// unsynced holds failed writers whose unsynced segment tail could not be cut off yet: the run's recording is not
	// opened again until it is (recording.Writer.DropUnsynced).
	unsynced map[string]*recording.Writer
	// failed holds, per run whose writer failed (or could not be opened), when the recording may be opened again and what
	// was durably stored then. Until then chunks are refused without touching the disk (reopenBackoff).
	failed map[string]failure
	// closedOn holds, per run, the node connection the hub was last told to close for a storage failure: one close per
	// (run, connection), because every close marks all of the node's live runs unknown (Hub.onDisconnect).
	closedOn map[string]uint64
	// touched is when a run's marks above were last set or used; pruneLocked drops the marks of runs not seen for
	// markMaxAge.
	touched   map[string]time.Time
	nextPrune time.Time
	// reopenBackoff is how long a failed recording is not reopened. A disk that keeps failing would otherwise cost every
	// chunk a read of the whole recording under mu, which blocks every run and viewer.
	reopenBackoff time.Duration
	// subs are live viewers per run.
	subs map[string]map[*Viewer]struct{}
	// archiving marks runs whose segments are being uploaded; the channel closes when that ends.
	archiving map[string]chan struct{}
	now       func() time.Time
	// create overrides how segment files are created (fault tests); nil uses the filesystem.
	create func(path string) (recording.File, error)
	ctx    context.Context // cancelled by Close: ends archive uploads
	cancel context.CancelFunc
	wg     sync.WaitGroup
	// closed is set by Close (under mu): chunks are refused from then on.
	closed bool
}

// errRecordingsClosed is returned for a chunk that arrives after Close.
var errRecordingsClosed = errors.New("recordings closed")

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
		resync: map[string]uint64{}, closedOn: map[string]uint64{}, touched: map[string]time.Time{}, failed: map[string]failure{}, reopenBackoff: defaultReopenBackoff, unsynced: map[string]*recording.Writer{}, archiving: map[string]chan struct{}{}, now: now, ctx: ctx, cancel: cancel}
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

// defaultReopenBackoff is how long central waits before it opens a recording again after a store failure.
const defaultReopenBackoff = 5 * time.Second

// failure is a recording that central could not store: it is not opened again before retry.
type failure struct {
	retry  time.Time
	stored uint64
	// storage is set when the failure lost a chunk the node must re-ship (a write or sync failure), so a refusal in
	// the backoff window closes the connection like the failure did. A recording that could not even be opened
	// (permissions, the object store) is not: see apply.
	storage bool
}

// markMaxAge is how long the marks of a run nobody has shipped to are kept; markPruneEvery is how often they are swept.
const (
	markMaxAge     = time.Hour
	markPruneEvery = time.Minute
)

// closeConn is the error apply returns when the hub must close the node connection: central lost a chunk that only a
// re-ship on a new connection can bring back. Any other error leaves the connection open.
type closeConn struct{ err error }

func (e *closeConn) Error() string { return e.err.Error() }
func (e *closeConn) Unwrap() error { return e.err }

// errReopenBackoff is returned for a chunk refused while a failed recording waits to be reopened. Like any store
// failure it is acked with the stored sequence and closes the node connection (the node re-ships on its next one).
var errReopenBackoff = errors.New("recording store failed recently, not reopened yet")

func (r *Recordings) writer(runID string) (*recording.Writer, error) {
	r.waitArchiveLocked(runID)
	if w := r.w[runID]; w != nil {
		return w, nil
	}
	if f := r.unsynced[runID]; f != nil {
		// What a failure wrote after the last sync must be gone before the recording is read back as stored.
		if err := f.DropUnsynced(); err != nil {
			return nil, err
		}
		delete(r.unsynced, runID)
	}
	dir, err := r.Dir(runID)
	if err != nil {
		return nil, err
	}
	o := recording.Options{Dir: dir, Now: r.now, Create: r.create}
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

// apply stores a chunk that arrived on node connection conn (the hub's connection number) and returns the highest
// durably stored sequence, the only value the node may be acked (recording.Writer.Stored). Any error means central did
// not store the chunk. Only a *closeConn error asks the hub to close the connection, so the node re-ships everything
// above its acknowledgement on the next one (SM-REC-003, ADR-0010 addendum): that is a storage failure (a write or
// flush), at most once per (run, connection), since a close costs every live run of the node. Other errors (the
// recording could not be opened: permissions, the object store) are logged and the chunk is acked with what is
// stored; on this connection nothing is stored past the hole they leave. A write or flush failure also closes the writer and drops it, so
// the next chunk reopens the recording from what is durably on disk, and until the node re-ships, a chunk on the same
// connection that starts past Stored()+1 (records sent behind the failed ones) is not stored: it is acked Stored().
func (r *Recordings) apply(conn uint64, c proto.RecordingChunk) (uint64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		// A chunk from a connection the hub has not finished tearing down must not open a writer, and so create the
		// run's folder, after Close flushed the writers and the data directory is being removed.
		return 0, errRecordingsClosed
	}
	now := r.now()
	r.pruneLocked(now)
	if f, ok := r.failed[c.RunID]; ok && r.w[c.RunID] == nil {
		if now.Before(f.retry) {
			// Refuse without reading the recording from disk; the node's chunk needs a re-ship when the failure was a
			// storage one, so the connection is closed (once) for it.
			r.touched[c.RunID] = now
			if f.storage {
				return f.stored, r.closeLocked(conn, c.RunID, errReopenBackoff)
			}
			return f.stored, errReopenBackoff
		}
	}
	w, err := r.writer(c.RunID)
	if err != nil {
		f := r.failed[c.RunID]
		f.retry = now.Add(r.reopenBackoff)
		r.failed[c.RunID] = f
		// Nothing of this connection's later chunks may be stored past what is stored now.
		r.resync[c.RunID] = conn
		r.touched[c.RunID] = now
		r.log.Warn("recording not opened; chunk not stored, the node keeps it", slog.String("run.id", c.RunID), obs.Err(err))
		if f.storage {
			return f.stored, r.closeLocked(conn, c.RunID, err)
		}
		return f.stored, err
	}
	recs := make([]recording.Record, 0, len(c.Records))
	for _, raw := range c.Records {
		var rec recording.Record
		if json.Unmarshal(raw, &rec) != nil || rec.Seq == 0 {
			continue
		}
		recs = append(recs, rec)
	}
	if failedOn, ok := r.resync[c.RunID]; ok {
		stored := w.Stored()
		switch first := firstAbove(recs, stored); {
		case failedOn != conn:
			// A new connection: the node's catch-up re-ships from its acknowledgement first, so a jump here is one
			// the node made itself (records it never had), which a recording keeps as a gap.
			delete(r.resync, c.RunID)
		case first == 0:
			// Only repeats: nothing to store.
		case first > stored+1:
			return stored, nil
		default:
			delete(r.resync, c.RunID)
		}
	}
	closed := false
	for _, rec := range recs {
		stored, err := w.Append(rec)
		if err != nil {
			return r.dropFailedLocked(conn, c.RunID, w), r.closeLocked(conn, c.RunID, err)
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
		return r.dropFailedLocked(conn, c.RunID, w), r.closeLocked(conn, c.RunID, err)
	}
	seq := w.Stored()
	delete(r.failed, c.RunID)
	if closed {
		_ = w.Close()
		delete(r.w, c.RunID)
		delete(r.resync, c.RunID)
		delete(r.closedOn, c.RunID)
		delete(r.touched, c.RunID)
		r.archiveLater(c.RunID)
	}
	return seq, nil
}

// closeLocked wraps a storage failure on connection conn as the error that makes the hub close the connection, once per
// (run, connection): later failures of the run on the same connection (chunks already on their way) return err as is.
func (r *Recordings) closeLocked(conn uint64, runID string, err error) error {
	if c, ok := r.closedOn[runID]; ok && c == conn {
		return err
	}
	r.closedOn[runID] = conn
	return &closeConn{err: err}
}

// pruneLocked forgets the marks of runs not seen for markMaxAge (the run ended without RunEnded, or the node never
// came back): the maps would otherwise grow with every run that ever failed.
func (r *Recordings) pruneLocked(now time.Time) {
	if now.Before(r.nextPrune) {
		return
	}
	r.nextPrune = now.Add(markPruneEvery)
	for id, at := range r.touched {
		if now.Sub(at) >= markMaxAge && r.w[id] == nil {
			r.forgetLocked(id)
		}
	}
}

// ConnEnded drops the resync marks of node connection conn, which has ended: a resync mark guards a live connection
// (chunks on it already on their way must not overtake the lost records) and nothing else, so it lives exactly as long
// as the connection and is never aged out while it is open (pruneLocked would let the next chunk be stored and acked
// past the gap).
func (r *Recordings) ConnEnded(conn uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, c := range r.resync {
		if c == conn {
			delete(r.resync, id)
		}
	}
}

// forgetLocked drops the age-based marks of a run (failed, closedOn, touched), not resync: that one belongs to a live
// connection and ends with it (ConnEnded), or with the run (RunEnded, the recording's close). An unsynced tail that cannot be cut yet keeps its entry (and the run its
// failure mark): reading the recording back would take that tail for stored.
func (r *Recordings) forgetLocked(runID string) {
	if f := r.unsynced[runID]; f != nil {
		if f.DropUnsynced() != nil {
			return
		}
		delete(r.unsynced, runID)
	}
	delete(r.failed, runID)
	delete(r.closedOn, runID)
	delete(r.touched, runID)
}

// firstAbove returns the sequence of the first record above seq (0 when there is none).
func firstAbove(recs []recording.Record, seq uint64) uint64 {
	for _, r := range recs {
		if r.Seq > seq {
			return r.Seq
		}
	}
	return 0
}

// dropFailedLocked closes a writer that failed on connection conn and forgets it, returning what it had durably
// stored. The run is marked for a resync on that connection; a writer whose unsynced tail is still on disk is kept
// until it can be cut off.
func (r *Recordings) dropFailedLocked(conn uint64, runID string, w *recording.Writer) uint64 {
	_ = w.Close()
	if r.w[runID] == w {
		delete(r.w, runID)
	}
	r.resync[runID] = conn
	r.touched[runID] = r.now()
	r.failed[runID] = failure{retry: r.now().Add(r.reopenBackoff), stored: w.Stored(), storage: true}
	if w.DropUnsynced() != nil {
		r.unsynced[runID] = w
	}
	return w.Stored()
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
	r.forgetLocked(runID)
	delete(r.resync, runID)
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
	r.closed = true
	for id, w := range r.w {
		_ = w.Close()
		delete(r.w, id)
	}
}
