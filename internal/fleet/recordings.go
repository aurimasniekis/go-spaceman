package fleet

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/recording"
)

// Recordings is central's copy of node recordings (SM-REC-002/003): records shipped by nodes are stored per run under
// <data>/recordings/<run>/ in the same segment format, and fanned out to live viewers. Viewers never apply
// backpressure to nodes: each viewer has a bounded queue and is marked lagged (it then catches up from storage)
// instead of slowing the shipper (SM-REC-004).
type Recordings struct {
	dir string
	mu  sync.Mutex
	w   map[string]*recording.Writer
	// subs are live viewers per run.
	subs map[string]map[*Viewer]struct{}
	now  func() time.Time
}

// Viewer is one live subscription.
type Viewer struct {
	C      chan recording.Record
	lagged bool
	run    string
}

// Lagged reports whether the viewer dropped records (it must re-read from storage after its last seq).
func (v *Viewer) Lagged() bool { return v.lagged }

var reRunID = regexp.MustCompile(`^run_[0-9a-z]{26}$`)

func newRecordings(dir string, now func() time.Time) *Recordings {
	return &Recordings{dir: dir, w: map[string]*recording.Writer{}, subs: map[string]map[*Viewer]struct{}{}, now: now}
}

// Dir returns a run's recording directory.
func (r *Recordings) Dir(runID string) (string, error) {
	if !reRunID.MatchString(runID) {
		return "", errors.New("invalid run id")
	}
	return filepath.Join(r.dir, runID), nil
}

func (r *Recordings) writer(runID string) (*recording.Writer, error) {
	if w := r.w[runID]; w != nil {
		return w, nil
	}
	dir, err := r.Dir(runID)
	if err != nil {
		return nil, err
	}
	w, err := recording.Open(recording.Options{Dir: dir, Now: r.now})
	if err != nil {
		return nil, err
	}
	r.w[runID] = w
	return w, nil
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
				v.lagged = true
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
	}
	return seq, nil
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
func (r *Recordings) Read(runID string, after uint64) (recording.ReadResult, error) {
	dir, err := r.Dir(runID)
	if err != nil {
		return recording.ReadResult{}, err
	}
	r.mu.Lock()
	if w := r.w[runID]; w != nil {
		_ = w.Flush()
	}
	r.mu.Unlock()
	res, err := recording.Read(dir, after)
	if errors.Is(err, os.ErrNotExist) {
		return recording.ReadResult{Records: []recording.Record{}}, nil
	}
	return res, err
}

// Delete removes a run's stored recording (permission recording:delete). The node's local copy is separate.
func (r *Recordings) Delete(runID string) error {
	dir, err := r.Dir(runID)
	if err != nil {
		return err
	}
	r.mu.Lock()
	if w := r.w[runID]; w != nil {
		_ = w.Close()
		delete(r.w, runID)
	}
	r.mu.Unlock()
	return os.RemoveAll(dir)
}

// Close flushes every open writer.
func (r *Recordings) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, w := range r.w {
		_ = w.Close()
		delete(r.w, id)
	}
}
