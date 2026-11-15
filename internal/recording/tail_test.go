package recording

import (
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"runtime"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestModeTracker(t *testing.T) {
	var m ModeTracker
	// Split across writes, several parameters, an untracked mode (2026) and a non-private CSI are handled.
	for _, w := range []string{"hi\x1b[?10", "49h\x1b[?25", "l text \x1b[2J\x1b[?1000;1006;2026h", "\x1b[?1000l\x1b", "[?2004h"} {
		m.Write([]byte(w))
	}
	want := []Mode{{1049, true}, {25, false}, {1006, true}, {1000, false}, {2004, true}}
	if got := m.Modes(); !reflect.DeepEqual(got, want) {
		t.Fatalf("modes %v, want %v", got, want)
	}
	// RIS forgets everything; an overlong parameter is ignored; ESC inside a sequence starts a new one.
	m.Write([]byte("\x1bc\x1b[?99999999999h\x1b[?1\x1b[?47h"))
	if got := m.Modes(); !reflect.DeepEqual(got, []Mode{{47, true}}) {
		t.Fatalf("after RIS %v", got)
	}
	m.Write([]byte("\x1b[?" + strings.Repeat("1;", 40) + "25l"))
	if got := m.Modes(); !reflect.DeepEqual(got, []Mode{{47, true}}) {
		t.Fatalf("too many parameters must be ignored: %v", got)
	}
}

func writeRecording(t *testing.T, recs []Record) string {
	t.Helper()
	dir := t.TempDir()
	c := &clk{t: time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)}
	w, err := Open(Options{Dir: dir, Now: c.now})
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range recs {
		must(t, w.Add(r))
		if i%3 == 2 {
			must(t, w.Flush())
		}
	}
	must(t, w.Close())
	return dir
}

func tailOf(t *testing.T, dir string, budget int64) TailResult {
	t.Helper()
	segs, err := Segments(dir)
	if err != nil {
		t.Fatal(err)
	}
	res, err := TailSegments(segs, func(p string) (io.ReadCloser, error) { return os.Open(p) }, budget) //nolint:gosec // test paths
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestTailSegments(t *testing.T) {
	out := func(s string) Record { return Record{Kind: KindOut, Data: []byte(s)} }
	dir := writeRecording(t, []Record{
		{Kind: KindOpen, Cols: 200, Rows: 50, RunID: "run_x"}, // 1
		out("\x1b[?1049h0123456789"),                          // 2: 18 bytes
		{Kind: KindGap, Why: "reattach"},                      // 3
		{Kind: KindSize, Cols: 80, Rows: 24},                  // 4
		out("abcdefghij"),                                     // 5
		out("\x1b[?25l0123"),                                  // 6: 10 bytes
		out("klmnopqrst"),                                     // 7
		{Kind: KindSize, Cols: 100, Rows: 30},                 // 8
		out("uvwxyz"),                                         // 9
	})

	// 16 bytes: records 7..9 (10+6); everything before passed through the tracker.
	res := tailOf(t, dir, 16)
	if len(res.Records) != 3 || res.Records[0].Seq != 7 || res.LastSeq != 9 {
		t.Fatalf("records %+v last %d", res.Records, res.LastSeq)
	}
	if res.Skipped != 18+10+10 || res.Cols != 80 || res.Rows != 24 || res.SkippedGaps != 1 || res.SkippedTrunc {
		t.Fatalf("skipped %d size %dx%d gaps %d", res.Skipped, res.Cols, res.Rows, res.SkippedGaps)
	}
	if !reflect.DeepEqual(res.Modes, []Mode{{1049, true}, {25, false}}) {
		t.Fatalf("modes %v", res.Modes)
	}

	// A budget smaller than the last record still returns it.
	if res := tailOf(t, dir, 1); len(res.Records) != 1 || res.Records[0].Seq != 9 || res.Cols != 100 {
		t.Fatalf("tiny budget %+v", res)
	}

	// A budget larger than the recording returns all of it: nothing skipped, no size or modes needed.
	res = tailOf(t, dir, 1<<20)
	if len(res.Records) != 9 || res.Skipped != 0 || res.Cols != 0 || len(res.Modes) != 0 || res.Gaps != 1 {
		t.Fatalf("whole %+v", res)
	}
}

func TestTailSegmentsLong(t *testing.T) {
	// Many records: the window is compacted as it moves, and the tail is still exact.
	recs := []Record{{Kind: KindOpen, Cols: 80, Rows: 24}}
	for i := range 20000 {
		recs = append(recs, Record{Kind: KindOut, Data: fmt.Appendf(nil, "%07d\n", i)})
	}
	recs = append(recs, Record{Kind: KindTrunc, Why: "quota"}, Record{Kind: KindClose, Reason: "exited"})
	dir := writeRecording(t, recs)
	res := tailOf(t, dir, 8*100)
	if len(res.Records) != 102 || string(res.Records[0].Data) != "0019900\n" || res.Records[101].Kind != KindClose {
		t.Fatalf("tail of %d records, first %q", len(res.Records), res.Records[0].Data)
	}
	if res.Skipped != 8*19900 || res.LastSeq != 20003 {
		t.Fatalf("skipped %d last %d", res.Skipped, res.LastSeq)
	}
	// The default budget (0) is MaxTailBytes.
	if res := tailOf(t, dir, 0); len(res.Records) != len(recs) {
		t.Fatalf("default budget %d", len(res.Records))
	}
}

func TestScanSegmentsStops(t *testing.T) {
	dir := writeRecording(t, []Record{{Kind: KindOut, Data: []byte("a")}, {Kind: KindOut, Data: []byte("b")}, {Kind: KindOut, Data: []byte("c")}})
	segs, _ := Segments(dir)
	stop := errors.New("stop")
	var seen []uint64
	_, err := ScanSegments(segs, func(p string) (io.ReadCloser, error) { return os.Open(p) }, 1, func(r Record) error { //nolint:gosec // test paths
		seen = append(seen, r.Seq)
		if r.Seq == 2 {
			return stop
		}
		return nil
	})
	if !errors.Is(err, stop) || !reflect.DeepEqual(seen, []uint64{2}) {
		t.Fatalf("err %v seen %v", err, seen)
	}
}

func TestTailModesToggledInPrefix(t *testing.T) {
	// The alternate screen and mouse modes change many times in the dropped prefix: only their final state counts.
	var recs []Record
	for i := range 50 {
		on := i%2 == 0
		c := map[bool]byte{true: 'h', false: 'l'}[on]
		recs = append(recs, Record{Kind: KindOut, Data: fmt.Appendf(nil, "\x1b[?1049%c\x1b[?1000;1006%cframe %d\r\n", c, c, i)})
	}
	// Last state in the prefix: 1049 and the mouse off (i = 49), then the bracketed paste on, split over two records.
	recs = append(recs, Record{Kind: KindOut, Data: []byte("\x1b[?20")}, Record{Kind: KindOut, Data: []byte("04hready\r\n")})
	recs = append(recs, Record{Kind: KindOut, Data: []byte("the tail\r\n")})
	res := tailOf(t, writeRecording(t, recs), 10)
	if len(res.Records) != 1 || string(res.Records[0].Data) != "the tail\r\n" {
		t.Fatalf("tail %+v", res.Records)
	}
	want := []Mode{{1049, false}, {1000, false}, {1006, false}, {2004, true}}
	if !reflect.DeepEqual(res.Modes, want) {
		t.Fatalf("modes %v, want %v", res.Modes, want)
	}
}

func TestTailMemoryBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("writes a 50 MB recording")
	}
	// 50 MB of output in 8 KiB records; the tail scanner keeps a 1 MiB window, so the heap must grow by far less than
	// the recording (decoders and the window), not with it.
	chunk := []byte(strings.Repeat("\x1b[32mgreen output line of a long run\x1b[0m\r\n", 8<<10/40)[:8<<10])
	dir := t.TempDir()
	w, err := Open(Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	const total = 50 << 20
	for n := 0; n < total; n += len(chunk) {
		must(t, w.Add(Record{Kind: KindOut, Data: chunk}))
	}
	must(t, w.Close())
	segs, _ := Segments(dir)

	defer debug.SetGCPercent(debug.SetGCPercent(10))
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	base := ms.HeapAlloc
	var peak atomic.Uint64
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		var m runtime.MemStats
		for {
			runtime.ReadMemStats(&m)
			if m.HeapAlloc > peak.Load() {
				peak.Store(m.HeapAlloc)
			}
			select {
			case <-stop:
				return
			case <-time.After(2 * time.Millisecond):
			}
		}
	}()
	res, err := TailSegments(segs, func(p string) (io.ReadCloser, error) { return os.Open(p) }, 1<<20) //nolint:gosec // test paths
	close(stop)
	<-done
	if err != nil {
		t.Fatal(err)
	}
	if res.Skipped+int64(len(res.Records))*int64(len(chunk)) < total || len(res.Records) != 128 {
		t.Fatalf("tail of %d records, %d skipped", len(res.Records), res.Skipped)
	}
	grew := int64(peak.Load()) - int64(base)
	t.Logf("heap grew by %.1f MiB scanning %d MiB", float64(grew)/(1<<20), total>>20)
	if grew > 12<<20 {
		t.Fatalf("heap grew by %d bytes scanning a %d byte recording with a 1 MiB tail", grew, total)
	}
}

func TestTailStartsAtACharacter(t *testing.T) {
	out := func(s string) Record { return Record{Kind: KindOut, Data: []byte(s)} }
	// The cut falls inside a DECSET: the mode it completes counts, and its end is not drawn as text.
	res := tailOf(t, writeRecording(t, []Record{out("earlier output\x1b[?10"), out("49hfull screen app")}), 20)
	if len(res.Records) != 1 || string(res.Records[0].Data) != "full screen app" || !reflect.DeepEqual(res.Modes, []Mode{{1049, true}}) {
		t.Fatalf("records %q modes %v", res.Records[0].Data, res.Modes)
	}
	if res.Skipped != int64(len("earlier output\x1b[?10")+3) {
		t.Fatalf("skipped %d", res.Skipped)
	}
	// Inside a colour (a CSI that sets no mode), a title (OSC, across two records) and a UTF-8 character.
	for _, c := range []struct {
		recs []Record
		want string
	}{
		{[]Record{out("aaaa\x1b[38;2;1"), out("0;20;30mgreen")}, "green"},
		{[]Record{out("aaaa\x1b]0;a long ti"), out("tle"), out(" still\x07after")}, "after"},
		{[]Record{out("aaaa\x1b]0;t\x1b"), out("\\after")}, "after"},
		{[]Record{out("aaaa\xe2\x94"), out("\x80─ rule")}, "─ rule"},
		{[]Record{out("aaaa"), out("plain")}, "plain"},
	} {
		res := tailOf(t, writeRecording(t, c.recs), int64(len(c.recs[len(c.recs)-1].Data)+len(c.recs[len(c.recs)-2].Data)-1))
		var got strings.Builder
		for _, r := range res.Records {
			got.Write(r.Data)
		}
		if got.String() != c.want {
			t.Fatalf("%v: tail %q, want %q", c.recs, got.String(), c.want)
		}
	}
}
