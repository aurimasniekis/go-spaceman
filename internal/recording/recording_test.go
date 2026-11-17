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
// record still gets a sequence number for central's copy. What was synced stays readable; the torn frame written after
// it is cut off (it was never synced, so no reader may take it for stored).
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
	if len(res.Records) != 2 || res.Truncated || string(res.Records[1].Data) != "before\n" {
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

// failSync is a segment file whose Sync fails while fail is set.
type failSync struct {
	File
	fail *bool
}

func (f failSync) Sync() error {
	if *f.fail {
		return syscall.EIO
	}
	return f.File.Sync()
}

// Stored is the highest sequence that was synced: it never counts buffered records, a failed flush, or the trunc
// record a failure adds, while Seq does (the hub acks Stored, so a node never prunes records central lacks).
func TestStoredDoesNotAdvanceOnFailedFlush(t *testing.T) {
	dir := t.TempDir()
	fail := false
	w, err := Open(Options{Dir: dir, Create: func(path string) (File, error) {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, err
		}
		return failSync{File: f, fail: &fail}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	for i := uint64(1); i <= 2; i++ {
		if _, err := w.Append(Record{Seq: i, Kind: KindOut, Data: []byte("a")}); err != nil {
			t.Fatal(err)
		}
	}
	if w.Stored() != 0 {
		t.Fatalf("stored %d before any flush", w.Stored())
	}
	must(t, w.Flush())
	if w.Stored() != 2 {
		t.Fatalf("stored %d after flush, want 2", w.Stored())
	}
	fail = true
	if _, err := w.Append(Record{Seq: 3, Kind: KindOut, Data: []byte("b")}); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err == nil {
		t.Fatal("flush succeeded")
	}
	if _, err := w.Append(Record{Seq: 4, Kind: KindOut}); err == nil {
		t.Fatal("append after failure succeeded")
	}
	if w.Stored() != 2 {
		t.Fatalf("stored %d after a failed flush, want 2", w.Stored())
	}
	if w.Seq() <= 2 {
		t.Fatalf("seq %d should be above stored", w.Seq())
	}
	_ = w.Close()
	// A reopened writer starts from what is on disk.
	w2, err := Open(Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if w2.Stored() < 2 || w2.Stored() != w2.Seq() {
		t.Fatalf("reopened stored %d seq %d", w2.Stored(), w2.Seq())
	}
}

// failSyncCreate creates segment files whose Sync fails while fail is set (the write itself reaches the page cache).
func failSyncCreate(fail *bool) func(string) (File, error) {
	return func(path string) (File, error) {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, err
		}
		return failSync{File: f, fail: fail}, nil
	}
}

// SM-REC-003: a frame written before a failed sync may still decode from the page cache although it never reached the
// disk. The failure cuts it off, so a writer opened afterwards (central reopens one after every failure) never counts
// it as stored: Stored is the last synced record, and a re-shipped copy of the lost record is stored again.
func TestStoredNeverTrustsATailWrittenAfterAFailure(t *testing.T) {
	dir := t.TempDir()
	fail := false
	w, err := Open(Options{Dir: dir, Create: failSyncCreate(&fail)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Append(Record{Seq: 1, Kind: KindOut, Data: []byte("a")}); err != nil {
		t.Fatal(err)
	}
	must(t, w.Flush())
	fail = true
	if _, err := w.Append(Record{Seq: 2, Kind: KindOut, Data: []byte("b")}); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err == nil {
		t.Fatal("flush with a failing sync succeeded")
	}
	_ = w.Close()
	if err := w.DropUnsynced(); err != nil {
		t.Fatal(err)
	}
	res, err := Read(dir, 0)
	if err != nil || len(res.Records) != 1 || res.Truncated {
		t.Fatalf("after the failed sync the segment holds %+v (%v), want record 1 alone", res, err)
	}
	fail = false
	w2, err := Open(Options{Dir: dir, Create: failSyncCreate(&fail)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w2.Close() }()
	if w2.Stored() != 1 || w2.Seq() != 1 {
		t.Fatalf("reopened writer: stored %d seq %d, want 1", w2.Stored(), w2.Seq())
	}
	for seq := uint64(2); seq <= 3; seq++ {
		if ok, err := w2.Append(Record{Seq: seq, Kind: KindOut, Data: []byte{byte('a' + seq - 1)}}); !ok || err != nil {
			t.Fatalf("re-shipped record %d: stored %v %v", seq, ok, err)
		}
	}
	must(t, w2.Flush())
	if w2.Stored() != 3 {
		t.Fatalf("stored %d after the re-ship, want 3", w2.Stored())
	}
	res, _ = Read(dir, 0)
	if len(res.Records) != 3 || res.Gaps != 0 {
		t.Fatalf("recording after the re-ship: %d records, %d gaps", len(res.Records), res.Gaps)
	}
}

// When the unsynced tail cannot be cut off at once, DropUnsynced keeps failing until it can, and only then is the
// tail gone (central does not reopen the recording meanwhile).
func TestDropUnsyncedRetriesUntilTheTailIsCut(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions")
	}
	dir := t.TempDir()
	fail := false
	w, err := Open(Options{Dir: dir, Create: failSyncCreate(&fail)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Append(Record{Seq: 1, Kind: KindOut, Data: []byte("a")}); err != nil {
		t.Fatal(err)
	}
	must(t, w.Flush())
	seg := filepath.Join(dir, SegmentName(1))
	synced, err := os.Stat(seg)
	if err != nil {
		t.Fatal(err)
	}
	fail = true
	if _, err := w.Append(Record{Seq: 2, Kind: KindOut, Data: []byte("b")}); err != nil {
		t.Fatal(err)
	}
	must(t, os.Chmod(seg, 0o400)) // the cut fails
	if err := w.Flush(); err == nil {
		t.Fatal("flush with a failing sync succeeded")
	}
	if err := w.DropUnsynced(); err == nil {
		t.Fatal("the tail was reported cut while the segment could not be written")
	}
	must(t, os.Chmod(seg, 0o600))
	must(t, w.DropUnsynced())
	must(t, w.DropUnsynced()) // nothing left to cut
	if fi, err := os.Stat(seg); err != nil || fi.Size() != synced.Size() {
		t.Fatalf("segment after the cut: %v %v, want %d bytes", fi, err, synced.Size())
	}
}

// A writer resumed past archived records starts both its sequence and its stored sequence there.
func TestOpenResumeSeqIsStored(t *testing.T) {
	w, err := Open(Options{Dir: t.TempDir(), ResumeSegment: 2, ResumeSeq: 7})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if w.Seq() != 7 || w.Stored() != 7 {
		t.Fatalf("seq %d stored %d, want 7 and 7", w.Seq(), w.Stored())
	}
}
