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
//	{"seq":6,"t":…,"k":"trunc","why":"quota"}               // recording stopped early (quota or disk)
//
// A frame is written on every flush (at least once per FlushInterval), so after a crash a segment decodes up to its
// last complete frame; Read reports a torn tail as Truncated rather than failing (SM-REC-003). Sequence numbers are
// continuous across segments and survive restarts of the writer (Resume).
package recording

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// Options configure a Writer.
type Options struct {
	Dir string
	// MaxBytes bounds the total compressed size of the recording (quota); 0 is unlimited. When exceeded a trunc
	// record is written and further output is dropped — the run itself is never affected (SM-REC-004).
	MaxBytes int64
	Now      func() time.Time
}

// Writer appends records. It is safe for concurrent use. Writes never block on readers.
type Writer struct {
	mu       sync.Mutex
	o        Options
	enc      *zstd.Encoder
	f        *os.File
	segIndex int
	segBytes int64
	segStart time.Time
	total    int64
	seq      uint64
	buf      bytes.Buffer
	stopped  bool
	err      error
}

// SegmentName returns the file name of segment n.
func SegmentName(n int) string { return fmt.Sprintf("%s%06d%s", segmentPrefix, n, segmentSuffix) }

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
	if err := os.MkdirAll(o.Dir, 0o700); err != nil {
		return nil, err
	}
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderConcurrency(1))
	if err != nil {
		return nil, err
	}
	w := &Writer{o: o, enc: enc}
	segs, err := Segments(o.Dir)
	if err != nil {
		return nil, err
	}
	for _, s := range segs {
		fi, err := os.Stat(s)
		if err == nil {
			w.total += fi.Size()
		}
		var n int
		if _, err := fmt.Sscanf(filepath.Base(s), segmentPrefix+"%06d"+segmentSuffix, &n); err == nil && n > w.segIndex {
			w.segIndex = n
		}
	}
	if len(segs) > 0 {
		res, _ := Read(o.Dir, 0)
		if len(res.Records) > 0 {
			w.seq = res.Records[len(res.Records)-1].Seq
		}
	}
	return w, nil
}

// Seq returns the last written sequence number.
func (w *Writer) Seq() uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.seq
}

// Add appends one record (assigning seq and time) and flushes when the buffer is large.
func (w *Writer) Add(r Record) error {
	_, err := w.AddRecord(r)
	return err
}

// AddRecord is Add returning the stored record (with its sequence and time); ok is false when the record was dropped
// (quota reached).
func (w *Writer) AddRecord(r Record) (Record, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return Record{}, w.err
	}
	if w.stopped && r.Kind != KindClose {
		return Record{}, nil
	}
	w.seq++
	r.Seq = w.seq
	if r.T == 0 {
		r.T = w.o.Now().UnixMicro()
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
// kept as-is (readers count it as a gap).
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
			w.err = err
			return err
		}
	}
	frame := w.enc.EncodeAll(w.buf.Bytes(), nil)
	if w.o.MaxBytes > 0 && w.total+int64(len(frame)) > w.o.MaxBytes && !w.stopped {
		// Quota reached: record the truncation (tiny) and stop recording output.
		w.stopped = true
		w.buf.Reset()
		w.seq++
		trunc, _ := json.Marshal(Record{Seq: w.seq, T: now.UnixMicro(), Kind: KindTrunc, Why: "quota"})
		frame = w.enc.EncodeAll(append(trunc, '\n'), nil)
	}
	n, err := w.f.Write(frame)
	w.segBytes += int64(n)
	w.total += int64(n)
	w.buf.Reset()
	if err != nil {
		w.err = fmt.Errorf("recording write: %w", err)
		return w.err
	}
	if err := w.f.Sync(); err != nil {
		w.err = fmt.Errorf("recording sync: %w", err)
		return w.err
	}
	return nil
}

func (w *Writer) rotateLocked(now time.Time) error {
	if w.f != nil {
		if err := w.f.Close(); err != nil {
			return err
		}
	}
	w.segIndex++
	f, err := os.OpenFile(filepath.Join(w.o.Dir, SegmentName(w.segIndex)), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	w.f, w.segBytes, w.segStart = f, 0, now
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

// Err returns the first write error (disk full, I/O); after it the writer drops records without blocking the run.
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
	var res ReadResult
	var last uint64
	for _, s := range segs {
		torn, err := readSegment(s, func(r Record) {
			if last != 0 && r.Seq != last+1 {
				res.Gaps++
			}
			last = r.Seq
			if r.Kind == KindGap {
				res.Gaps++
			}
			if r.Seq > after {
				res.Records = append(res.Records, r)
			}
		})
		if err != nil {
			return res, err
		}
		res.Truncated = res.Truncated || torn
	}
	return res, nil
}

func readSegment(path string, emit func(Record)) (torn bool, err error) {
	f, err := os.Open(path) //nolint:gosec // path from Segments
	if err != nil {
		return false, err
	}
	defer f.Close()
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
				emit(r)
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
