// Package recording writes and reads SpaceMan terminal recordings (SM-REC-001…004, ADR-0010).
//
// A recording is a sequence of segment files `seg-NNNNNN.smrec.zst`. Each segment is a concatenation of complete zstd
// frames; each frame holds JSONL records:
//
//	{"seq":1,"t":1793616000000000,"k":"open","cols":200,"rows":120,"run":"run_…"}
//	{"seq":2,"t":…,"k":"o","d":"<base64 output>"}
//	{"seq":3,"t":…,"k":"size","cols":120,"rows":40}
//	{"seq":4,"t":…,"k":"gap","why":"reattach"}             // output may be missing here (no stream offsets, S-3)
//	{"seq":5,"t":…,"k":"close","exit_code":0,"reason":"exited"}
//	{"seq":6,"t":…,"k":"trunc","why":"quota"}               // recording stopped early (quota reached)
//
// A frame is written on every flush (at least once per FlushInterval), so after a crash a segment decodes up to its
// last complete frame; Read reports a torn tail as Truncated rather than failing (SM-REC-003). Sequence numbers are
// continuous across segments and survive restarts of the writer (Resume).
//
// A write failure (a full disk, an I/O error) never reaches the recorded program: while it lasts, the writer still
// numbers every record and hands it back to the caller (who ships it to central's copy) without storing it, and it
// tries a new segment again after a backoff, so the local recording resumes once the disk has space (SM-REC-004). The
// records it could not store are a gap in the local copy only.
//
// The Writer serves two callers with one failure path. The node numbers its own records (AddRecord) and retries the
// disk as above (ADR-0097). Central stores a node's numbered records (Append) and acknowledges Stored: there a failure
// is final for the writer, which Append never retries; central drops it and opens a new one on what is durably on
// disk, and the node re-ships everything above the acknowledgement (SM-REC-003, ADR-0010 addendum). Either way a
// failure cuts the segment back to its last sync, and Stored never counts a record written or numbered after it.
package recording

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
)

// Record kinds.
const (
	KindOpen  = "open"
	KindOut   = "o"
	KindSize  = "size"
	KindGap   = "gap"
	KindClose = "close"
	KindTrunc = "trunc"
)

// Record is one recording entry.
type Record struct {
	Seq      uint64 `json:"seq"`
	T        int64  `json:"t" doc:"Unix microseconds"`
	Kind     string `json:"k"`
	Data     []byte `json:"d,omitempty"`
	Cols     int    `json:"cols,omitempty"`
	Rows     int    `json:"rows,omitempty"`
	RunID    string `json:"run,omitempty"`
	ExitCode *int   `json:"exit_code,omitempty"`
	Reason   string `json:"reason,omitempty"`
	Why      string `json:"why,omitempty"`
}

// Defaults.
const (
	SegmentMaxBytes = 8 << 20
	SegmentMaxAge   = 5 * time.Minute
	FlushInterval   = time.Second
	flushBytes      = 64 << 10
	segmentPrefix   = "seg-"
	segmentSuffix   = ".smrec.zst"
)

// File is a segment file the Writer appends to: an *os.File unless Options.Create says otherwise.
type File interface {
	io.Writer
	Sync() error
	Close() error
}

// Options configure a Writer.
type Options struct {
	Dir string
	// Create creates a new segment file (exclusively, mode 0600). Nil uses the filesystem; fault tests inject a disk
	// that fills up.
	Create func(path string) (File, error)
	// MaxBytes bounds the total compressed size of the recording (quota); 0 is unlimited. When exceeded a trunc
	// record is written and further output is dropped — the run itself is never affected (SM-REC-004).
	MaxBytes int64
	Now      func() time.Time
	// ResumeSeq and ResumeSegment continue a recording whose earlier segments are no longer in Dir (central's copy,
	// archived to the artifact store): the next record is numbered above ResumeSeq and the next segment above
	// ResumeSegment, whichever of these and Dir's own segments is higher.
	ResumeSeq     uint64
	ResumeSegment int
	// RetryAfter is how long the writer waits after a write failure before it tries a new segment (doubling after
	// each failure in a row, up to maxRetryAfter); 0 is DefaultRetryAfter.
	RetryAfter time.Duration
}

// DefaultRetryAfter is the first wait after a write failure before the writer tries the disk again; maxRetryAfter
// bounds the doubling.
const (
	DefaultRetryAfter = 5 * time.Second
	maxRetryAfter     = time.Minute
)

// Writer appends records. It is safe for concurrent use. Writes never block on readers.
type Writer struct {
	mu       sync.Mutex
	o        Options
	enc      *zstd.Encoder
	f        File
	segIndex int
	segBytes int64
	segStart time.Time
	total    int64
	seq      uint64
	stored   uint64
	// synced is how much of the current segment is known to be on disk (its size at the last successful sync).
	synced int64
	// unsynced is a segment tail written after the last sync that a failure could not cut off (DropUnsynced).
	unsynced *tailCut
	// lost is set once a failure lost records: stored stays below that hole.
	lost    bool
	buf     bytes.Buffer
	stopped bool
	// err is the current write failure (nil while the disk works); retryAt is when the writer tries a new segment
	// again and wait the wait after the next failure in a row.
	err     error
	retryAt time.Time
	wait    time.Duration
	// trunc is the quota truncation record not yet taken by Truncation.
	trunc    *Record
	truncOut bool
}

// tailCut is a segment (number index) that must be cut back to size bytes.
type tailCut struct {
	path  string
	index int
	size  int64
}

// SegmentName returns the file name of segment n.
func SegmentName(n int) string { return fmt.Sprintf("%s%06d%s", segmentPrefix, n, segmentSuffix) }

// IsSegment reports whether name is a segment file name.
func IsSegment(name string) bool {
	_, ok := SegmentIndex(name)
	return ok
}

// SegmentIndex returns the number of a segment file name.
func SegmentIndex(name string) (int, bool) {
	var n int
	if !strings.HasPrefix(name, segmentPrefix) || !strings.HasSuffix(name, segmentSuffix) || len(name) != len(SegmentName(1)) {
		return 0, false
	}
	if _, err := fmt.Sscanf(name, segmentPrefix+"%06d"+segmentSuffix, &n); err != nil || n < 1 {
		return 0, false
	}
	return n, true
}

// Segments lists a recording's segment files in order.
func Segments(dir string) ([]string, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), segmentPrefix) && strings.HasSuffix(e.Name(), segmentSuffix) {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(out)
	return out, nil
}

// Open starts or resumes a recording in o.Dir: an existing recording continues with a new segment and the next
// sequence number (a restarted agent never overwrites or renumbers earlier records).
func Open(o Options) (*Writer, error) {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Create == nil {
		o.Create = createFile
	}
	if o.RetryAfter <= 0 {
		o.RetryAfter = DefaultRetryAfter
	}
	if err := os.MkdirAll(o.Dir, 0o700); err != nil {
		return nil, err
	}
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderConcurrency(1))
	if err != nil {
		return nil, err
	}
	w := &Writer{o: o, enc: enc, segIndex: o.ResumeSegment, seq: o.ResumeSeq, stored: o.ResumeSeq}
	segs, err := Segments(o.Dir)
	if err != nil {
		return nil, err
	}
	for _, s := range segs {
		fi, err := os.Stat(s)
		if err == nil {
			w.total += fi.Size()
		}
		if n, ok := SegmentIndex(filepath.Base(s)); ok && n > w.segIndex {
			w.segIndex = n
		}
	}
	if len(segs) > 0 {
		res, _ := Read(o.Dir, 0)
		if len(res.Records) > 0 && res.Records[len(res.Records)-1].Seq > w.seq {
			w.seq = res.Records[len(res.Records)-1].Seq
		}
		if w.seq > w.stored {
			w.stored = w.seq
		}
	}
	return w, nil
}

func createFile(path string) (File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // path built from Dir and SegmentName
}

// Seq returns the last written sequence number.
func (w *Writer) Seq() uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.seq
}

// Stored returns N such that every record at or below N that this recording will ever hold is durably stored and
// contiguous: written and synced to disk (or already stored when the writer was opened), with no record missing below
// N other than one its sender never had (a jump the sender made itself, SkipTo or a node's own lost records). Unlike
// Seq it never counts buffered records, a failed flush, or a close that could not be stored: it is the only value a
// receiver may acknowledge, and the sender may forget every record at or below it.
//
// A failure cuts the segment back to its last synced size (DropUnsynced), so a writer opened later never takes a
// record written after the last successful sync for stored, and Stored stays below the hole a failure leaves in this
// writer: records numbered while the disk failed (AddRecord) never count, even after the disk recovered.
func (w *Writer) Stored() uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.stored
}

// Add appends one record (assigning seq and time) and flushes when the buffer is large.
func (w *Writer) Add(r Record) error {
	_, err := w.AddRecord(r)
	return err
}

// AddRecord is Add returning the record with its sequence and time; its Seq is 0 when the record was dropped (quota
// reached). While the disk fails (the error returned) the record is numbered but not stored: the caller still ships
// it to central's copy, and the writer tries the disk again once its backoff has passed.
func (w *Writer) AddRecord(r Record) (Record, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.o.Now()
	w.retryLocked(now)
	if w.stopped && r.Kind != KindClose {
		return Record{}, nil
	}
	w.seq++
	r.Seq = w.seq
	if r.T == 0 {
		r.T = now.UnixMicro()
	}
	if w.err != nil {
		return r, w.err
	}
	line, err := json.Marshal(r)
	if err != nil {
		return Record{}, err
	}
	w.buf.Write(line)
	w.buf.WriteByte('\n')
	if w.buf.Len() >= flushBytes {
		return r, w.flushLocked()
	}
	return r, nil
}

// Append stores a record that already carries its sequence number (central's copy of a node recording). Records at
// or below the last stored sequence are ignored, so re-shipping after a reconnect is idempotent; a jump in sequence is
// kept as-is (readers count it as a gap). A write failure is final for Append (unlike AddRecord, it never retries the
// disk): the records it lost exist only at the sender, so the caller drops the writer and opens a new one on the
// directory (after DropUnsynced succeeded), whose Stored tells the sender where to re-ship from.
func (w *Writer) Append(r Record) (bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return false, w.err
	}
	if r.Seq <= w.seq {
		return false, nil
	}
	if w.stopped && r.Kind != KindClose {
		return false, nil
	}
	w.seq = r.Seq
	line, err := json.Marshal(r)
	if err != nil {
		return false, err
	}
	w.buf.Write(line)
	w.buf.WriteByte('\n')
	if w.buf.Len() >= flushBytes {
		return true, w.flushLocked()
	}
	return true, nil
}

// SkipTo moves the next sequence number past seq (no-op when the writer is already there). A node whose local
// recording lost records that central already stores (a failed disk, then a restart) continues above central's copy
// instead of renumbering; readers count the jump as a gap.
func (w *Writer) SkipTo(seq uint64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if seq > w.seq {
		w.seq = seq
	}
}

// Truncation returns, once, the trunc record that ended this recording early (quota reached), so the caller can ship
// it with the records before it.
func (w *Writer) Truncation() (Record, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.trunc == nil || w.truncOut {
		return Record{}, false
	}
	w.truncOut = true
	return *w.trunc, true
}

// Flush writes buffered records as one complete zstd frame and syncs it.
func (w *Writer) Flush() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.flushLocked()
}

func (w *Writer) flushLocked() error {
	if w.buf.Len() == 0 || w.err != nil {
		return w.err
	}
	now := w.o.Now()
	if w.f == nil || w.segBytes >= SegmentMaxBytes || now.Sub(w.segStart) >= SegmentMaxAge {
		if err := w.rotateLocked(now); err != nil {
			return w.failLocked(now, fmt.Errorf("recording segment: %w", err))
		}
	}
	frame := w.enc.EncodeAll(w.buf.Bytes(), nil)
	if w.o.MaxBytes > 0 && w.total+int64(len(frame)) > w.o.MaxBytes && !w.stopped {
		// Quota reached: record the truncation (tiny) and stop recording output.
		w.stopped = true
		w.buf.Reset()
		w.seq++
		tr := Record{Seq: w.seq, T: now.UnixMicro(), Kind: KindTrunc, Why: "quota"}
		w.trunc = &tr
		line, _ := json.Marshal(tr)
		frame = w.enc.EncodeAll(append(line, '\n'), nil)
	}
	n, err := w.f.Write(frame)
	w.segBytes += int64(n)
	w.total += int64(n)
	w.buf.Reset()
	if err != nil {
		return w.failLocked(now, fmt.Errorf("recording write: %w", err))
	}
	if err := w.f.Sync(); err != nil {
		return w.failLocked(now, fmt.Errorf("recording sync: %w", err))
	}
	w.synced = w.segBytes
	if !w.lost {
		w.stored = w.seq
	}
	w.wait = 0
	return nil
}

// failLocked records a write failure: buffered records are lost locally (the caller has them), and the segment is
// closed and cut back to its last synced size (removed when nothing of it was synced, its number then used again): a
// frame written after the last sync may still decode from the page cache although it never reached the disk, and a
// writer opened later must not take it for stored. A failure leaves a hole in the local copy, so Stored never moves
// past it again. AddRecord tries a new segment after the backoff (retryLocked); Append never does (see Append).
func (w *Writer) failLocked(now time.Time, err error) error {
	w.err = err
	w.lost = true
	w.buf.Reset()
	if w.f != nil {
		_ = w.f.Close()
		w.f = nil
		if w.segBytes > w.synced || w.synced == 0 {
			w.unsynced = &tailCut{path: filepath.Join(w.o.Dir, SegmentName(w.segIndex)), index: w.segIndex, size: w.synced}
			_ = w.dropUnsyncedLocked()
		}
	}
	if w.wait == 0 {
		w.wait = w.o.RetryAfter
	}
	w.retryAt = now.Add(w.wait)
	w.wait = min(2*w.wait, max(maxRetryAfter, w.o.RetryAfter))
	return err
}

// retryLocked clears a write failure once its backoff has passed: the next flush starts a new segment, and a failure
// there starts a longer backoff.
func (w *Writer) retryLocked(now time.Time) {
	if w.err != nil && !now.Before(w.retryAt) {
		// A tail the failure could not cut off is tried again first, so a segment removed now gives its number back.
		_ = w.dropUnsyncedLocked()
		w.err = nil
	}
}

func (w *Writer) rotateLocked(now time.Time) error {
	if w.f != nil {
		err := w.f.Close()
		w.f = nil
		if err != nil {
			return err
		}
	}
	w.segIndex++
	f, err := w.o.Create(filepath.Join(w.o.Dir, SegmentName(w.segIndex)))
	if err != nil {
		w.segIndex--
		return err
	}
	w.f, w.segBytes, w.synced, w.segStart = f, 0, 0, now
	return nil
}

// DropUnsynced cuts off what a failure left on disk after the last successful sync (failLocked tries it at once).
// It returns an error while that could not be done; a writer must not be opened on the directory until it succeeds.
func (w *Writer) DropUnsynced() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.dropUnsyncedLocked()
}

func (w *Writer) dropUnsyncedLocked() error {
	if w.unsynced == nil {
		return nil
	}
	if w.unsynced.size == 0 {
		// Nothing of the segment is synced: an empty or partial file is no segment. Removing it also keeps a disk that
		// keeps failing from leaving one behind per attempt.
		if err := os.Remove(w.unsynced.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			// The segment keeps its number while it is on disk: the next one must not reuse it.
			return fmt.Errorf("recording: remove the unsynced segment: %w", err)
		}
		if w.f == nil && w.unsynced.index == w.segIndex {
			// The file is gone and no later segment was started: the next segment takes its number.
			w.segIndex--
		}
	} else if err := os.Truncate(w.unsynced.path, w.unsynced.size); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("recording: cut the unsynced tail: %w", err)
	}
	w.unsynced = nil
	return nil
}

// Close flushes and closes the current segment.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	err := w.flushLocked()
	if w.f != nil {
		if cerr := w.f.Close(); err == nil {
			err = cerr
		}
		w.f = nil
	}
	return err
}

// Err returns the current write failure (disk full, I/O), nil while the disk works; meanwhile the writer numbers records
// without storing them and never blocks the run.
func (w *Writer) Err() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.err
}

// ReadResult is a decoded recording.
type ReadResult struct {
	Records []Record `json:"records"`
	// Truncated is set when a segment ends in an incomplete frame (an interrupted writer); everything before it is
	// returned.
	Truncated bool `json:"truncated"`
	// Gaps counts sequence discontinuities and explicit gap records.
	Gaps int `json:"gaps"`
}

// Read decodes every segment, returning records with seq > after.
func Read(dir string, after uint64) (ReadResult, error) {
	segs, err := Segments(dir)
	if err != nil {
		return ReadResult{}, err
	}
	return ReadSegments(segs, func(p string) (io.ReadCloser, error) { return os.Open(p) }, after) //nolint:gosec // paths from Segments
}

// ReadSegments decodes the named segments in order, each opened by open (a file, or an object of the artifact
// store), returning records with seq > after.
func ReadSegments(names []string, open func(name string) (io.ReadCloser, error), after uint64) (ReadResult, error) {
	var recs []Record
	res, err := ScanSegments(names, open, after, func(r Record) error {
		recs = append(recs, r)
		return nil
	})
	res.Records = recs
	return res, err
}

// ScanSegments decodes the named segments in order and calls fn with each record with seq > after, one at a time,
// so a reader never holds the whole recording. An error from fn stops the scan and is returned. The result's Records
// is nil; Truncated and Gaps cover what was scanned.
func ScanSegments(names []string, open func(name string) (io.ReadCloser, error), after uint64, fn func(Record) error) (ReadResult, error) {
	var res ReadResult
	var last uint64
	for _, s := range names {
		rc, err := open(s)
		if err != nil {
			return res, err
		}
		var ferr error
		torn, err := readSegment(rc, func(r Record) bool {
			if last != 0 && r.Seq != last+1 {
				res.Gaps++
			}
			last = r.Seq
			if r.Kind == KindGap {
				res.Gaps++
			}
			if r.Seq > after {
				ferr = fn(r)
			}
			return ferr == nil
		})
		_ = rc.Close()
		if ferr != nil {
			return res, ferr
		}
		if err != nil {
			return res, err
		}
		res.Truncated = res.Truncated || torn
	}
	return res, nil
}

// LastSeq returns the sequence number of the last record of one segment (0 when it holds none).
func LastSeq(r io.Reader) uint64 {
	var last uint64
	_, _ = readSegment(r, func(rec Record) bool { last = rec.Seq; return true })
	return last
}

// LastSeqIn returns the sequence number of the last record of a recording directory: that of its last segment holding
// one, decoding segments from the end (0 when none holds a record).
func LastSeqIn(dir string) (uint64, error) {
	segs, err := Segments(dir)
	if err != nil {
		return 0, err
	}
	for i := len(segs) - 1; i >= 0; i-- {
		f, err := os.Open(segs[i]) //nolint:gosec // paths from Segments
		if err != nil {
			return 0, err
		}
		last := LastSeq(f)
		_ = f.Close()
		if last > 0 {
			return last, nil
		}
	}
	return 0, nil
}

// readSegment decodes one segment, calling emit with each record until it returns false.
func readSegment(f io.Reader, emit func(Record) bool) (torn bool, err error) {
	dec, err := zstd.NewReader(f, zstd.WithDecoderConcurrency(1))
	if err != nil {
		return false, err
	}
	defer dec.Close()
	br := bufio.NewReaderSize(dec, 256<<10)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 && line[len(line)-1] == '\n' {
			var r Record
			if json.Unmarshal(line, &r) == nil {
				if !emit(r) {
					return torn, nil
				}
			} else {
				torn = true
			}
		} else if len(line) > 0 {
			torn = true
		}
		if errors.Is(err, io.EOF) {
			return torn, nil
		}
		if err != nil {
			// A torn final frame (crash mid-write) or a corrupted tail: keep what decoded.
			return true, nil
		}
	}
}
