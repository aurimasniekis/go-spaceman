package recording

import (
	"bytes"
	"os"
	"path/filepath"
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
