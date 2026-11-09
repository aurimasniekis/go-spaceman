package agent

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/recording"
	"github.com/aurimasniekis/go-spaceman/internal/runner"
	"github.com/aurimasniekis/go-spaceman/internal/runner/runnertest"
)

// sizeAgent is an agent on the in-memory runner with a valid lease for spc_size (generation 3) and one live run.
func sizeAgent(t *testing.T) (*Agent, *runnertest.Fake, runRow) {
	t.Helper()
	dir := t.TempDir()
	j, err := OpenJournal(context.Background(), filepath.Join(dir, "journal"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = j.Close() })
	f := runnertest.New()
	a := New(j, Options{DataDir: dir, Runner: f, DefaultCols: 200, DefaultRows: 120}) // the fake starts programs at 200x120
	a.mem.base = context.Background()
	t.Cleanup(a.stopHolds)
	t.Cleanup(a.stopRecorders)
	a.leases["spc_size"] = leaseRow{SpaceID: "spc_size", Generation: 3, ExpiresAt: time.Now().Add(time.Hour)}
	return a, f, liveRun(t, a, f, "run_one")
}

func liveRun(t *testing.T, a *Agent, f *runnertest.Fake, runID string) runRow {
	t.Helper()
	ctx := context.Background()
	p, err := f.Start(ctx, runner.Spec{Name: RunName(runID), Argv: []string{"/bin/true"}, Labels: map[string]string{"spaceman.managed": "1"}})
	if err != nil {
		t.Fatal(err)
	}
	row := runRow{RunID: runID, SpaceID: "spc_size", Generation: 3, Kind: "worker", Role: "implementor", Harness: "claude-code", TerminalName: RunName(runID)}
	if err := a.j.insertRun(ctx, row); err != nil {
		t.Fatal(err)
	}
	row.TerminalID, row.State, row.Desired = p.ID, proto.RunRunning, "running"
	if err := a.j.updateRun(ctx, row); err != nil {
		t.Fatal(err)
	}
	return row
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func sizes(runs ...proto.TerminalSize) proto.TerminalSizes { return proto.TerminalSizes{Sizes: runs} }

// ADR-0075: the node holds a sized run's terminal through one interactive attach that never types, resizes it in
// place, and closes it when the size is cleared (the terminal goes back to its start size), so no attach leaks.
func TestTerminalSizeHold(t *testing.T) {
	a, f, row := sizeAgent(t)
	ctx := context.Background()
	term := row.TerminalID
	size := func() (int, int) { c, r, _ := f.Size(term); return c, r }
	want := proto.TerminalSize{SpaceID: "spc_size", RunID: row.RunID, Generation: 3, Cols: 120, Rows: 40}

	a.applyTerminalSizes(ctx, sizes(want))
	waitFor(t, "held at 120x40", func() bool { c, r := size(); return c == 120 && r == 40 })
	if n := f.TakeoverAttached(term); n != 1 {
		t.Fatalf("%d attaches, want 1", n)
	}
	// The same set again changes nothing; a new size resizes the same attach.
	a.applyTerminalSizes(ctx, sizes(want))
	want.Cols, want.Rows = 80, 24
	a.applyTerminalSizes(ctx, sizes(want))
	waitFor(t, "resized to 80x24", func() bool { c, r := size(); return c == 80 && r == 24 })
	if n := f.TakeoverAttached(term); n != 1 {
		t.Fatalf("%d attaches after a resize, want 1", n)
	}
	// Out-of-bounds sizes are ignored like a cleared one.
	a.applyTerminalSizes(ctx, sizes(proto.TerminalSize{SpaceID: "spc_size", RunID: row.RunID, Generation: 3, Cols: 20, Rows: 5}))
	waitFor(t, "hold closed", func() bool { return f.TakeoverAttached(term) == 0 })
	if c, r := size(); c != 200 || r != 120 {
		t.Fatalf("after the hold closed the terminal is %dx%d, want its start size 200x120", c, r)
	}
	// Clearing: back to the start size, no attach.
	a.applyTerminalSizes(ctx, sizes(want))
	waitFor(t, "held again", func() bool { return f.TakeoverAttached(term) == 1 })
	a.applyTerminalSizes(ctx, sizes())
	waitFor(t, "cleared", func() bool { return f.TakeoverAttached(term) == 0 })
	if c, r := size(); c != 200 || r != 120 {
		t.Fatalf("cleared terminal is %dx%d", c, r)
	}
	// The hold never typed anything.
	if typed := f.Typed(term); typed != "" {
		t.Fatalf("the hold wrote %d input bytes", len(typed))
	}
	if in := f.Inputs(term); len(in) != 0 {
		t.Fatalf("input sent: %q", in)
	}
}

// ADR-0075: holds close when the run ends, when the space's lease is revoked (fencing) and at shutdown; a wrong
// generation or a space without a lease is never held, and a run whose terminal is not live yet is held once it is.
func TestTerminalSizeHoldEnds(t *testing.T) {
	a, f, row := sizeAgent(t)
	ctx := context.Background()
	two := liveRun(t, a, f, "run_two")
	at := func(r runRow, c, rows int) proto.TerminalSize {
		return proto.TerminalSize{SpaceID: r.SpaceID, RunID: r.RunID, Generation: r.Generation, Cols: c, Rows: rows}
	}

	// Run end closes the hold.
	a.applyTerminalSizes(ctx, sizes(at(row, 100, 30), at(two, 90, 30)))
	waitFor(t, "both held", func() bool { return f.TakeoverAttached(row.TerminalID) == 1 && f.TakeoverAttached(two.TerminalID) == 1 })
	f.Exit(row.TerminalID, 0, "exited")
	waitFor(t, "hold of the ended run closed", func() bool { return f.TakeoverAttached(row.TerminalID) == 0 })
	a.mu.Lock()
	_, stillHeld := a.holds[row.RunID]
	a.mu.Unlock()
	if stillHeld {
		t.Fatal("the ended run's hold is still listed")
	}

	// Fencing: another generation is not held; revoking the lease closes the space's holds.
	a.applyTerminalSizes(ctx, sizes(proto.TerminalSize{SpaceID: "spc_size", RunID: two.RunID, Generation: 2, Cols: 90, Rows: 30}))
	waitFor(t, "stale generation released", func() bool { return f.TakeoverAttached(two.TerminalID) == 0 })
	a.applyTerminalSizes(ctx, sizes(at(two, 90, 30)))
	waitFor(t, "held again", func() bool { return f.TakeoverAttached(two.TerminalID) == 1 })
	a.revoke(ctx, proto.LeaseRevoke{SpaceID: "spc_size", Generation: 3, Reason: "test", Mode: "interrupt"})
	waitFor(t, "fenced hold closed", func() bool { return f.TakeoverAttached(two.TerminalID) == 0 })
	a.applyTerminalSizes(ctx, sizes(at(two, 90, 30)))
	time.Sleep(100 * time.Millisecond)
	if n := f.TakeoverAttached(two.TerminalID); n != 0 {
		t.Fatalf("held without a lease: %d attaches", n)
	}

	// A run not live yet is held once it is (observe); shutdown closes every hold. (The revoke stopped the space's
	// runs, so this uses new ones under a new lease.)
	a.leases["spc_size"] = leaseRow{SpaceID: "spc_size", Generation: 3, ExpiresAt: time.Now().Add(time.Hour)}
	two = liveRun(t, a, f, "run_four")
	pend := runRow{RunID: "run_three", SpaceID: "spc_size", Generation: 3, Kind: "worker", Role: "implementor", Harness: "claude-code", TerminalName: RunName("run_three")}
	if err := a.j.insertRun(ctx, pend); err != nil {
		t.Fatal(err)
	}
	pend.Desired, pend.State = "running", proto.RunPending
	a.applyTerminalSizes(ctx, sizes(at(two, 90, 30), at(pend, 120, 40)))
	waitFor(t, "live run held", func() bool { return f.TakeoverAttached(two.TerminalID) == 1 })
	p, err := f.Start(ctx, runner.Spec{Name: pend.TerminalName, Argv: []string{"/bin/true"}})
	if err != nil {
		t.Fatal(err)
	}
	a.observe(ctx, pend, *p, false)
	waitFor(t, "started run held", func() bool { return f.TakeoverAttached(p.ID) == 1 })
	a.stopHolds()
	waitFor(t, "shutdown closed every hold", func() bool { return f.TakeoverAttached(two.TerminalID) == 0 && f.TakeoverAttached(p.ID) == 0 })
	for _, id := range []string{row.TerminalID, two.TerminalID, p.ID} {
		if typed := f.Typed(id); typed != "" {
			t.Fatalf("a hold typed into %s", id)
		}
	}
}

// ADR-0075: the recording notes each size change (the hold's own resizes and the runner's size events, such as the
// hold closing), so a replay redraws at the size the output was drawn for.
func TestTerminalSizeRecorded(t *testing.T) {
	a, f, row := sizeAgent(t)
	ctx := context.Background()
	evs, err := f.Subscribe(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for ev := range evs {
			if ev.Kind != "size" {
				continue
			}
			if cols, rows, ok := runnerSize(ev.Data); ok {
				if r, err := a.j.runByTerminal(ctx, ev.TerminalID); err == nil {
					a.recordSize(r.RunID, cols, rows)
				}
			}
		}
	}()
	a.startRecorder(ctx, row)
	// Output sent before the recorder's attach is open would be lost (the fake drops it), so wait for its first paint.
	waitFor(t, "recorder attached", func() bool {
		res, err := recording.Read(a.RecordingDir(row.RunID), 0)
		return err == nil && len(res.Records) >= 2
	})
	// Output and size changes reach the recorder on separate paths, so a change is ordered after the output recorded
	// before it; a size noted before output arrives is recorded first (the hold notes it before it resizes).
	recorded := func(want string) func() bool {
		return func() bool {
			res, err := recording.Read(a.RecordingDir(row.RunID), 0)
			if err != nil {
				return false
			}
			for _, r := range res.Records {
				if string(r.Data) == want {
					return true
				}
			}
			return false
		}
	}
	f.Output(row.TerminalID, "at 200 columns\r\n")
	waitFor(t, "output at 200 columns recorded", recorded("at 200 columns\r\n"))
	a.applyTerminalSizes(ctx, sizes(proto.TerminalSize{SpaceID: "spc_size", RunID: row.RunID, Generation: 3, Cols: 100, Rows: 30}))
	waitFor(t, "held", func() bool { c, _, _ := f.Size(row.TerminalID); return c == 100 })
	f.Output(row.TerminalID, "at 100 columns\r\n")
	waitFor(t, "output at 100 columns recorded", recorded("at 100 columns\r\n"))
	a.applyTerminalSizes(ctx, sizes())
	waitFor(t, "released", func() bool { c, _, _ := f.Size(row.TerminalID); return c == 200 })
	f.Output(row.TerminalID, "back at 200\r\n")
	type step struct {
		kind       string
		cols, rows int
		data       string
	}
	var got []step
	waitFor(t, "size records", func() bool {
		res, err := recording.Read(a.RecordingDir(row.RunID), 0)
		if err != nil {
			return false
		}
		got = got[:0]
		for _, r := range res.Records {
			if r.Kind == recording.KindOut && !strings.HasPrefix(string(r.Data), "at ") && !strings.HasPrefix(string(r.Data), "back ") {
				continue // the fake's first paint
			}
			got = append(got, step{r.Kind, r.Cols, r.Rows, string(r.Data)})
		}
		return len(got) >= 6
	})
	want := []step{{kind: recording.KindOpen, cols: 200, rows: 120}, {kind: recording.KindOut, data: "at 200 columns\r\n"},
		{kind: recording.KindSize, cols: 100, rows: 30}, {kind: recording.KindOut, data: "at 100 columns\r\n"},
		{kind: recording.KindSize, cols: 200, rows: 120}, {kind: recording.KindOut, data: "back at 200\r\n"}}
	if len(got) != len(want) {
		t.Fatalf("records %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("record %d is %+v, want %+v (all: %+v)", i, got[i], want[i], got)
		}
	}
}
