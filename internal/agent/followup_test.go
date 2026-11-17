package agent

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/recording"
)

// endRun marks a journaled run as ended.
func (e *shipEnv) endRun(id string) {
	e.t.Helper()
	ctx := context.Background()
	r, err := e.a.j.getRun(ctx, id)
	if err != nil {
		e.t.Fatal(err)
	}
	r.State, r.Desired = proto.RunExited, "stopped"
	if err := e.a.j.updateRun(ctx, r); err != nil {
		e.t.Fatal(err)
	}
}

func slowEnded(t *testing.T, rate int64) {
	old := endedCatchUpRate
	endedCatchUpRate = rate
	t.Cleanup(func() { endedCatchUpRate = old })
}

// A run that ended during the pass and is served from the waiting set inside another run's catch-up is not paced:
// its history would hold the outer run's waiting live output back at an ended run's pace.
func TestCatchUpDoesNotPaceARunServedInsideAnotherCatchUp(t *testing.T) {
	slowEnded(t, 100<<10)
	e := newShipEnv(t)
	for _, id := range []string{"run_0", "run_e"} {
		e.run(id)
		e.addSized(id, 1024, 200-heldRecordOverhead) // 200 KiB counted: 2 s at 100 KiB/s
		e.endRun(id)
	}
	var at atomic.Pointer[time.Time]
	shipBatchHook = func(id string, batch int) {
		if id == "run_0" && batch == 1 && at.Load() == nil {
			now := time.Now()
			at.Store(&now)
			e.a.markUnshipped("run_e") // it ended during the pass: its output waits for it
		}
	}
	e.connect()
	e.drain("run_e", 1024)
	began := at.Load()
	if began == nil {
		t.Fatal("the pass never reached run_0")
	}
	if d := time.Since(*began); d > time.Second {
		t.Fatalf("the nested run's history took %s: it was paced inside another run's catch-up", d)
	}
	e.whole("run_0", 1, 1024)
}

// Between two runs of the sweep a run whose live output waits is served first, also when the run before it has
// nothing to wait for after a batch.
func TestCatchUpServesWaitingRunsBeforeEachRun(t *testing.T) {
	e := newShipEnv(t)
	e.run("run_a")
	e.add("run_a", 600)
	e.endRun("run_a")
	e.run("run_z")
	e.add("run_z", 3)
	e.endRun("run_z")
	e.a.markUnshipped("run_z") // its live output waits (marked before the sweep reaches run_a)
	var mu sync.Mutex
	var order []string
	shipBatchHook = func(id string, _ int) {
		mu.Lock()
		defer mu.Unlock()
		if len(order) == 0 {
			order = append(order, id)
		}
	}
	e.connect()
	e.whole("run_z", 1, 3)
	e.whole("run_a", 1, 600)
	mu.Lock()
	defer mu.Unlock()
	if len(order) != 1 || order[0] != "run_z" {
		t.Fatalf("first batch of the sweep was %v, want run_z's (it waits)", order)
	}
}

// A live run waiting while a paced ended run sleeps between batches is served within the sleep, not after it.
func TestPaceCatchUpServesWaitingRunsWhileItSleeps(t *testing.T) {
	slowEnded(t, 50<<10) // a 256-record batch counts 50 KiB: about 1 s of sleep after it
	e := newShipEnv(t)
	e.run("run_0")
	e.addSized("run_0", 1024, 200-heldRecordOverhead)
	e.endRun("run_0")
	e.run("run_l")
	e.connect()
	e.drain("run_0", 256)
	sent := time.Now()
	e.live("run_l", e.add("run_l", 1))
	e.drain("run_l", 1)
	if d := time.Since(sent); d > 400*time.Millisecond {
		t.Fatalf("a live run's output waited %s while a paced run slept", d)
	}
}

// A stored acknowledgement is never trusted blindly.
func TestRestoreAckBounds(t *testing.T) {
	a := pruneAgent(t, t.TempDir())
	id := ids.New(ids.Run)
	local := id
	w, err := recording.Open(recording.Options{Dir: a.RecordingDir(id)})
	if err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if err := w.Add(recording.Record{Kind: recording.KindOut, Data: []byte("x")}); err != nil {
			t.Fatal(err)
		}
	}
	_ = w.Close()
	write := func(s string) {
		if err := os.WriteFile(filepath.Join(a.runDir(id), recordingAckedFile), []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	try := func(s string) (uint64, bool) {
		a.mu.Lock()
		delete(a.acked, id)
		delete(a.restored, id)
		a.mu.Unlock()
		write(s)
		a.restoreAck(id)
		a.mu.Lock()
		defer a.mu.Unlock()
		v, ok := a.acked[id]
		if ok != a.restored[id] {
			t.Fatalf("%q: acked %v but restored %v", s, ok, a.restored[id])
		}
		return v, ok
	}
	if v, ok := try("3"); !ok || v != 3 {
		t.Fatalf("a plausible acknowledgement was dropped: %d %v", v, ok)
	}
	if v, ok := try("0"); ok {
		t.Fatalf("a stored 0 was restored as %d", v)
	}
	for _, s := range []string{strconv.FormatUint(5+ackMargin+1, 10), "18446744073709551615", "18446744073709551000", "-1", "x"} {
		if v, ok := try(s); ok {
			t.Fatalf("%q restored as %d", s, v)
		}
	}
	// A run with no local recording to compare with: only the range check applies.
	id = ids.New(ids.Run)
	if err := os.MkdirAll(a.runDir(id), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, ok := try("18446744073709551615"); ok {
		t.Fatal("a sequence at the top of the range was restored")
	}
	if v, ok := try("1000"); !ok || v != 1000 {
		t.Fatalf("an acknowledgement of a run without a local recording was dropped: %d %v", v, ok)
	}
	id = local
	if v, ok := try(strconv.FormatUint(5+ackMargin, 10)); !ok || v != 5+ackMargin {
		t.Fatalf("an acknowledgement at the margin was dropped: %d %v", v, ok)
	}
}

// Prune removes a run's acknowledgement file with its directory and forgets the run's restored mark.
func TestPruneRemovesAckFileAndRestoredMark(t *testing.T) {
	ctx := context.Background()
	a := pruneAgent(t, t.TempDir())
	oldID, recentID := ids.New(ids.Run), ids.New(ids.Run)
	seedRun(t, a, oldID, proto.RunExited, 45*day)
	seedRun(t, a, recentID, proto.RunExited, time.Hour)
	for _, id := range []string{oldID, recentID} {
		w, err := recording.Open(recording.Options{Dir: a.RecordingDir(id)})
		if err != nil {
			t.Fatal(err)
		}
		_ = w.Close()
		a.saveAck(id, 0)
		a.saveAck(id, 7)
		a.mu.Lock()
		if a.restored == nil {
			a.restored = map[string]bool{}
		}
		a.restored[id] = true
		a.mu.Unlock()
		r, _ := a.j.getRun(ctx, id)
		a.noteSent([]runRow{r})
	}
	a.finalAck(ctx, proto.Reconcile{})
	if _, err := os.Stat(a.runDir(oldID)); !os.IsNotExist(err) {
		entries, _ := os.ReadDir(a.runDir(oldID))
		t.Fatalf("the pruned run's directory stays (%v): %v", entries, err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.restored[oldID] {
		t.Fatal("the pruned run is still marked restored")
	}
	if !a.restored[recentID] {
		t.Fatal("a run that stays lost its restored mark")
	}
	if _, err := os.Stat(filepath.Join(a.runDir(recentID), recordingAckedFile)); err != nil {
		t.Fatalf("the acknowledgement file of a run that stays was removed: %v", err)
	}
}

// An acknowledgement saved while the run's directory is being removed never leaves a directory holding only that file.
func TestSaveAckRacingPrune(t *testing.T) {
	a := pruneAgent(t, t.TempDir())
	for i := range 300 {
		id := ids.New(ids.Run)
		w, err := recording.Open(recording.Options{Dir: a.RecordingDir(id)})
		if err != nil {
			t.Fatal(err)
		}
		_ = w.Close()
		a.saveAck(id, uint64(i+1))
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); a.saveAck(id, uint64(i+2)) }()
		go func() { defer wg.Done(); a.removeRecording(id) }()
		wg.Wait()
		if entries, err := os.ReadDir(a.runDir(id)); err == nil {
			t.Fatalf("run directory left behind holding %d entries", len(entries))
		}
	}
}

// A kept-back open record above records central holds is a size change in its copy, not the start of the recording.
func TestReleaseTurnsAHeldOpenIntoASize(t *testing.T) {
	const runID = "run_open"
	a := pruneAgent(t, t.TempDir())
	w, err := recording.Open(recording.Options{Dir: a.RecordingDir(runID)})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	s := &recShipper{a: a, w: w, row: runRow{RunID: runID}}
	s.gate.keep(recording.Record{Kind: recording.KindOpen, Cols: 80, Rows: 24, RunID: runID})
	a.recordingAck(proto.RecordingAck{RunID: runID, Seq: 10})
	if !s.release() {
		t.Fatal("not released although central's sequence is known")
	}
	if len(s.batch) != 1 || s.batch[0].Kind != recording.KindSize || s.batch[0].RunID != "" || s.batch[0].Seq != 11 {
		t.Fatalf("batch %+v, want one size record numbered 11", s.batch)
	}
	// With nothing of the run above, the open record stays one.
	s2 := &recShipper{a: a, w: mustWriter(t, a, "run_fresh"), row: runRow{RunID: "run_fresh"}}
	s2.gate.keep(recording.Record{Kind: recording.KindOpen, Cols: 80, Rows: 24, RunID: "run_fresh"})
	a.mu.Lock()
	a.seqKnown["run_fresh"] = true
	a.mu.Unlock()
	s2.release()
	if len(s2.batch) != 1 || s2.batch[0].Kind != recording.KindOpen {
		t.Fatalf("batch %+v, want the open record", s2.batch)
	}
}

func mustWriter(t *testing.T, a *Agent, id string) *recording.Writer {
	t.Helper()
	w, err := recording.Open(recording.Options{Dir: a.RecordingDir(id)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return w
}
