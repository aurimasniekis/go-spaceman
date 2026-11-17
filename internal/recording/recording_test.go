package recording

import (
	"bytes"
	"errors"
	"fmt"
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

// SM-REC-004 (fault: disk exhaustion): a write failure never fails callers that only add output. While the disk is
// full every record is still numbered and handed back (the caller ships it to central's copy) without being stored,
// and no trunc record ends the recording. Once the backoff has passed and the disk has space, the writer starts a new
// segment and the local recording goes on; what reached the disk before stays readable (the torn frame is reported as
// Truncated) and the records it could not store are a gap in the local copy.
func TestDiskFullRecovers(t *testing.T) {
	dir := t.TempDir()
	left := int64(1 << 30)
	creates := 0
	now := time.Unix(1_800_000_000, 0)
	w, err := Open(Options{Dir: dir, Now: func() time.Time { return now }, RetryAfter: 5 * time.Second, Create: func(path string) (File, error) {
		creates++
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
	left = 10 // the disk fills up in the middle of the next frame
	must(t, w.Add(Record{Kind: KindOut, Data: bytes.Repeat([]byte("lost "), 100)}))
	if err := w.Flush(); !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("flush error %v, want ENOSPC", err)
	}
	if tr, ok := w.Truncation(); ok {
		t.Fatalf("a disk failure ended the recording: %+v", tr)
	}
	// While the disk is full (and before the backoff passed) output is numbered for central but not stored.
	r, err := w.AddRecord(Record{Kind: KindOut, Data: []byte("central only")})
	if r.Seq != 4 || !errors.Is(err, syscall.ENOSPC) || string(r.Data) != "central only" {
		t.Fatalf("output while the disk is full %+v %v", r, err)
	}
	left = 1 << 30 // space freed, but the backoff has not passed yet
	now = now.Add(4 * time.Second)
	if r, err := w.AddRecord(Record{Kind: KindOut, Data: []byte("still waiting")}); r.Seq != 5 || err == nil {
		t.Fatalf("output before the backoff passed %+v %v", r, err)
	}
	if w.Err() == nil {
		t.Fatal("failure cleared before the backoff passed")
	}
	before := creates
	now = now.Add(2 * time.Second)
	r, err = w.AddRecord(Record{Kind: KindOut, Data: []byte("after\n")})
	if r.Seq != 6 || err != nil {
		t.Fatalf("output after the disk recovered %+v %v", r, err)
	}
	must(t, w.Flush())
	if w.Err() != nil || creates != before+1 {
		t.Fatalf("recovery: err %v, segments created %d", w.Err(), creates-before)
	}
	code := 0
	cl, err := w.AddRecord(Record{Kind: KindClose, ExitCode: &code, Reason: "exited"})
	if cl.Seq != 7 || err != nil {
		t.Fatalf("close after recovery %+v %v", cl, err)
	}
	must(t, w.Close())
	res, err := Read(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	var seqs []uint64
	for _, r := range res.Records {
		seqs = append(seqs, r.Seq)
	}
	if fmt.Sprint(seqs) != "[1 2 6 7]" || !res.Truncated || res.Gaps < 1 || string(res.Records[2].Data) != "after\n" {
		t.Fatalf("local recording after recovery: seqs %v %+v", seqs, res)
	}
	// A writer reopened later continues after the last stored record; SkipTo moves it above records central kept
	// that the disk lost.
	w2, err := Open(Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if w2.Seq() != 7 {
		t.Fatalf("resumed at %d", w2.Seq())
	}
	w2.SkipTo(9)
	w2.SkipTo(3) // never backwards
	must(t, w2.Add(Record{Kind: KindGap, Why: "reattach"}))
	must(t, w2.Close())
	res, _ = Read(dir, 0)
	if last := res.Records[len(res.Records)-1]; last.Seq != 10 {
		t.Fatalf("after resume: last %+v", last)
	}
}

// A disk that stays full is tried again with a growing backoff, and the attempts that wrote nothing leave no empty
// segments behind.
func TestDiskFullBacksOff(t *testing.T) {
	dir := t.TempDir()
	full := false
	creates := 0
	now := time.Unix(1_800_000_000, 0)
	w, err := Open(Options{Dir: dir, Now: func() time.Time { return now }, RetryAfter: time.Second, Create: func(path string) (File, error) {
		creates++
		if full {
			return nil, syscall.ENOSPC
		}
		return createFile(path)
	}})
	if err != nil {
		t.Fatal(err)
	}
	must(t, w.Add(Record{Kind: KindOpen, Cols: 80, Rows: 24}))
	must(t, w.Flush())
	full = true
	now = now.Add(SegmentMaxAge) // the next flush starts a new segment, which cannot be created
	must(t, w.Add(Record{Kind: KindOut, Data: []byte("x")}))
	if err := w.Flush(); !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("flush %v", err)
	}
	// Attempts at 1 s, then 2 s, then 4 s after each failure: one create per attempt, none in between.
	for _, wait := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second} {
		before := creates
		now = now.Add(wait - time.Millisecond)
		_, _ = w.AddRecord(Record{Kind: KindOut, Data: []byte("x")})
		_ = w.Flush()
		if creates != before {
			t.Fatalf("tried before the %s backoff passed", wait)
		}
		now = now.Add(time.Millisecond)
		_, _ = w.AddRecord(Record{Kind: KindOut, Data: []byte("x")})
		if err := w.Flush(); !errors.Is(err, syscall.ENOSPC) || creates != before+1 {
			t.Fatalf("attempt after %s: %v, creates %d", wait, err, creates-before)
		}
	}
	full = false
	now = now.Add(8 * time.Second)
	must(t, w.Add(Record{Kind: KindOut, Data: []byte("back")}))
	must(t, w.Close())
	segs, _ := Segments(dir)
	if len(segs) != 2 || filepath.Base(segs[1]) != SegmentName(2) {
		t.Fatalf("segments %v", segs)
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

// flakyFile is a segment file whose writes fail, writing nothing, while *fail is set.
type flakyFile struct {
	f      *os.File
	fail   *bool
	onFail func()
}

func (d *flakyFile) Write(p []byte) (int, error) {
	if *d.fail {
		if d.onFail != nil {
			d.onFail()
		}
		return 0, syscall.ENOSPC
	}
	return d.f.Write(p)
}
func (d *flakyFile) Sync() error  { return d.f.Sync() }
func (d *flakyFile) Close() error { return d.f.Close() }

func flakyWriter(t *testing.T, dir string, fail *bool, now *time.Time, onFail func(path string)) *Writer {
	t.Helper()
	w, err := Open(Options{Dir: dir, Now: func() time.Time { return *now }, RetryAfter: time.Second, Create: func(path string) (File, error) {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, err
		}
		d := &flakyFile{f: f, fail: fail}
		if onFail != nil {
			d.onFail = func() { onFail(path) }
		}
		return d, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// A segment that took no byte before the write failed is removed and its number is used again by the next one: no
// empty segment stays behind and the numbering has no hole.
func TestFailedFirstWriteLeavesNoEmptySegment(t *testing.T) {
	dir := t.TempDir()
	fail := true
	now := time.Unix(1_800_000_000, 0)
	w := flakyWriter(t, dir, &fail, &now, nil)
	must(t, w.Add(Record{Kind: KindOut, Data: []byte("a")}))
	if err := w.Flush(); !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("flush %v", err)
	}
	if segs, _ := Segments(dir); len(segs) != 0 {
		t.Fatalf("an empty segment stayed behind: %v", segs)
	}
	fail = false
	now = now.Add(2 * time.Second)
	must(t, w.Add(Record{Kind: KindOut, Data: []byte("b")}))
	must(t, w.Close())
	segs, _ := Segments(dir)
	if len(segs) != 1 || filepath.Base(segs[0]) != SegmentName(1) {
		t.Fatalf("segments %v, want only %s", segs, SegmentName(1))
	}
}

// When the empty segment cannot be removed it keeps its number: the next segment must not try to create it again.
func TestFailedFirstWriteKeepsTheNumberOfASegmentLeftBehind(t *testing.T) {
	dir := t.TempDir()
	fail := true
	now := time.Unix(1_800_000_000, 0)
	w := flakyWriter(t, dir, &fail, &now, func(path string) {
		// Make os.Remove fail: the path becomes a directory that is not empty.
		_ = os.Remove(path)
		_ = os.Mkdir(path, 0o700)
		_ = os.WriteFile(filepath.Join(path, "keep"), []byte("x"), 0o600)
	})
	must(t, w.Add(Record{Kind: KindOut, Data: []byte("a")}))
	if err := w.Flush(); !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("flush %v", err)
	}
	fail = false
	now = now.Add(2 * time.Second)
	must(t, w.Add(Record{Kind: KindOut, Data: []byte("b")}))
	must(t, w.Flush())
	if w.Err() != nil {
		t.Fatalf("the next segment failed: %v", w.Err())
	}
	if _, err := os.Stat(filepath.Join(dir, SegmentName(2))); err != nil {
		t.Fatalf("the next segment is not number 2: %v", err)
	}
	must(t, w.Close())
}

// After a flush succeeded the backoff starts over: a later failure waits RetryAfter again, not the doubled wait of the
// failures before.
func TestBackoffStartsOverAfterASuccessfulFlush(t *testing.T) {
	dir := t.TempDir()
	fail := true
	now := time.Unix(1_800_000_000, 0)
	w := flakyWriter(t, dir, &fail, &now, nil)
	must(t, w.Add(Record{Kind: KindOut, Data: []byte("a")}))
	_ = w.Flush() // the first failure: waits 1 s, the next failure would wait 2 s
	now = now.Add(time.Second)
	fail = false
	must(t, w.Add(Record{Kind: KindOut, Data: []byte("b")}))
	must(t, w.Flush())
	fail = true
	must(t, w.Add(Record{Kind: KindOut, Data: []byte("c")}))
	if err := w.Flush(); err == nil {
		t.Fatal("the write did not fail")
	}
	now = now.Add(time.Second)
	if _, err := w.AddRecord(Record{Kind: KindOut, Data: []byte("d")}); err != nil {
		t.Fatalf("still backing off one RetryAfter after the failure: %v", err)
	}
}

// A last segment that holds no decodable record (torn at its first frame) does not hide the records of the segment
// before it, and a recording whose segments are all torn has sequence 0.
func TestLastSeqInSkipsATornOnlyLastSegment(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		must(t, w.Add(Record{Kind: KindOut, Data: []byte("x")}))
	}
	must(t, w.Close())
	if got, err := LastSeqIn(dir); err != nil || got != 3 {
		t.Fatalf("LastSeqIn %d %v, want 3", got, err)
	}
	must(t, os.WriteFile(filepath.Join(dir, SegmentName(2)), []byte{0x28, 0xb5, 0x2f, 0xfd, 0x04, 0x58, 0x99}, 0o600))
	if got, err := LastSeqIn(dir); err != nil || got != 3 {
		t.Fatalf("LastSeqIn with a torn last segment %d %v, want 3", got, err)
	}
	torn := t.TempDir()
	must(t, os.WriteFile(filepath.Join(torn, SegmentName(1)), []byte("not a frame"), 0o600))
	if got, err := LastSeqIn(torn); err != nil || got != 0 {
		t.Fatalf("LastSeqIn of only torn segments %d %v, want 0", got, err)
	}
}
