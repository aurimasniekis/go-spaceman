package recording

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

type clk struct{ t time.Time }

func (c *clk) now() time.Time { c.t = c.t.Add(time.Millisecond); return c.t }

func TestWriteReadResume(t *testing.T) {
	dir := t.TempDir()
	c := &clk{t: time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)}
	w, err := Open(Options{Dir: dir, Now: c.now})
	if err != nil {
		t.Fatal(err)
	}
	must(t, w.Add(Record{Kind: KindOpen, Cols: 200, Rows: 120, RunID: "run_x"}))
	must(t, w.Add(Record{Kind: KindOut, Data: []byte("hello\r\n")}))
	must(t, w.Flush())
	must(t, w.Add(Record{Kind: KindSize, Cols: 80, Rows: 24}))
	must(t, w.Close())

	// A restarted writer resumes the sequence in a new segment.
	w2, err := Open(Options{Dir: dir, Now: c.now})
	if err != nil {
		t.Fatal(err)
	}
	if w2.Seq() != 3 {
		t.Fatalf("resumed seq %d", w2.Seq())
	}
	must(t, w2.Add(Record{Kind: KindGap, Why: "reattach"}))
	code := 0
	must(t, w2.Add(Record{Kind: KindClose, ExitCode: &code, Reason: "exited"}))
	must(t, w2.Close())

	res, err := Read(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Records) != 5 || res.Truncated || res.Gaps != 1 {
		t.Fatalf("result %+v", res)
	}
	if !bytes.Equal(res.Records[1].Data, []byte("hello\r\n")) || res.Records[4].Kind != KindClose || res.Records[4].Seq != 5 {
		t.Fatalf("records %+v", res.Records)
	}
	segs, _ := Segments(dir)
	if len(segs) != 2 {
		t.Fatalf("segments %v", segs)
	}
	if after, _ := Read(dir, 3); len(after.Records) != 2 {
		t.Fatalf("read after 3: %+v", after.Records)
	}
}

// SM-REC-003: an interrupted writer leaves a torn last frame; everything flushed before it is readable.
func TestTornSegmentRecovers(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		must(t, w.Add(Record{Kind: KindOut, Data: []byte("line\n")}))
		must(t, w.Flush())
	}
	must(t, w.Add(Record{Kind: KindOut, Data: bytes.Repeat([]byte("x"), 5000)}))
	must(t, w.Close())
	segs, _ := Segments(dir)
	fi, _ := os.Stat(segs[0])
	// Simulate a crash mid-frame: cut the last frame in half.
	must(t, os.Truncate(segs[0], fi.Size()-20))
	res, err := Read(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated || len(res.Records) < 3 {
		t.Fatalf("torn read: truncated=%v records=%d", res.Truncated, len(res.Records))
	}
}

// SM-REC-004: a full quota truncates the recording visibly and never blocks or errors the writer.
func TestQuotaTruncates(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(Options{Dir: dir, MaxBytes: 300})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		must(t, w.Add(Record{Kind: KindOut, Data: []byte(filepath.Join("random-ish", string(rune('a'+i%26)), "content-that-compresses-poorly-1234567890"))}))
		must(t, w.Flush())
	}
	must(t, w.Close())
	res, err := Read(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	last := res.Records[len(res.Records)-1]
	if last.Kind != KindTrunc || last.Why != "quota" {
		t.Fatalf("expected a trunc record last, got %+v", last)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestAppendIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	for _, seq := range []uint64{1, 2, 2, 1, 3, 7} {
		if _, err := w.Append(Record{Seq: seq, Kind: KindOut, Data: []byte{byte('a' + seq)}}); err != nil {
			t.Fatal(err)
		}
	}
	must(t, w.Close())
	res, _ := Read(dir, 0)
	if len(res.Records) != 4 || res.Gaps != 1 || res.Records[3].Seq != 7 {
		t.Fatalf("append result %+v", res)
	}
}

// fullDisk is a segment file that accepts limit bytes in total and then fails like a full disk (ENOSPC), after
// having written part of the frame.
type fullDisk struct {
	f     *os.File
	left  *int64
	syncs int
}

func (d *fullDisk) Write(p []byte) (int, error) {
	if int64(len(p)) <= *d.left {
		*d.left -= int64(len(p))
		return d.f.Write(p)
	}
	n := int(*d.left)
	*d.left = 0
	_, _ = d.f.Write(p[:n])
	return n, syscall.ENOSPC
}
func (d *fullDisk) Sync() error  { d.syncs++; return d.f.Sync() }
func (d *fullDisk) Close() error { return d.f.Close() }

// SM-REC-004 (fault: disk exhaustion): a write failure stops recording output without failing callers that only add
// output; the writer hands out a "disk" trunc record (once) numbered after every record it assigned, and the close
// record still gets a sequence number for central's copy. What reached the disk stays readable, the torn frame is
// reported as Truncated.
func TestDiskFullTruncates(t *testing.T) {
	dir := t.TempDir()
	left := int64(1 << 30)
	w, err := Open(Options{Dir: dir, Create: func(path string) (File, error) {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, err
		}
		return &fullDisk{f: f, left: &left}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	must(t, w.Add(Record{Kind: KindOpen, Cols: 80, Rows: 24, RunID: "run_x"}))
	must(t, w.Add(Record{Kind: KindOut, Data: []byte("before\n")}))
	must(t, w.Flush())
	if _, ok := w.Truncation(); ok {
		t.Fatal("truncation before any failure")
	}
	left = 10 // the disk fills up in the middle of the next frame
	must(t, w.Add(Record{Kind: KindOut, Data: bytes.Repeat([]byte("lost "), 100)}))
	if err := w.Flush(); !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("flush error %v, want ENOSPC", err)
	}
	tr, ok := w.Truncation()
	if !ok || tr.Kind != KindTrunc || tr.Why != "disk" || tr.Seq != 4 {
		t.Fatalf("truncation %+v %v", tr, ok)
	}
	if _, again := w.Truncation(); again {
		t.Fatal("truncation handed out twice")
	}
	if r, err := w.AddRecord(Record{Kind: KindOut, Data: []byte("dropped")}); r.Seq != 0 || err == nil {
		t.Fatalf("output after failure %+v %v", r, err)
	}
	code := 0
	cl, err := w.AddRecord(Record{Kind: KindClose, ExitCode: &code, Reason: "exited"})
	if cl.Seq != 5 || err == nil {
		t.Fatalf("close after failure %+v %v", cl, err)
	}
	_ = w.Close()
	res, err := Read(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Records) != 2 || !res.Truncated || string(res.Records[1].Data) != "before\n" {
		t.Fatalf("after disk full: %+v", res)
	}
	// A writer reopened later (space freed) continues after the last stored record; SkipTo moves it above records
	// central kept that the disk lost.
	w2, err := Open(Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if w2.Seq() != 2 {
		t.Fatalf("resumed at %d", w2.Seq())
	}
	w2.SkipTo(5)
	w2.SkipTo(3) // never backwards
	must(t, w2.Add(Record{Kind: KindGap, Why: "reattach"}))
	must(t, w2.Close())
	res, _ = Read(dir, 0)
	if last := res.Records[len(res.Records)-1]; last.Seq != 6 || res.Gaps < 2 {
		t.Fatalf("after resume: last %+v gaps %d", last, res.Gaps)
	}
}

// The quota trunc record is also handed out, so central's copy ends with it too.
func TestQuotaTruncationHandedOut(t *testing.T) {
	w, err := Open(Options{Dir: t.TempDir(), MaxBytes: 100})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		must(t, w.Add(Record{Kind: KindOut, Data: bytes.Repeat([]byte{byte('a' + i)}, 64)}))
		must(t, w.Flush())
	}
	tr, ok := w.Truncation()
	if !ok || tr.Why != "quota" {
		t.Fatalf("quota truncation %+v %v", tr, ok)
	}
	must(t, w.Close())
}
