package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/recording"
	"github.com/aurimasniekis/go-spaceman/internal/runner"
)

// seqCentral is a fake central's copy of one run's recording, kept as the real one keeps it (Writer.Append: records
// above the last stored sequence, others dropped as repeats), acknowledging after every recording message, empty ones
// too, with what it holds.
type seqCentral struct {
	t     *testing.T
	a     *Agent
	fc    *wsCentral
	seq   uint64
	recs  []recording.Record
	drops int // records dropped as repeats
	// mute, while set, loses every recording message (not stored, not acknowledged): central is out of reach.
	mute bool
	// only, when set, limits what central keeps and acknowledges to that run: messages about other runs are ignored.
	only string
	// muted counts the recording messages lost while mute was set (the recorder's probes among them), mutedRecs the
	// records in them.
	muted, mutedRecs int
	// barrier, when set, is a sequence central must hold up to before it is sent anything above it: a record above
	// it that arrives sooner is a jump over records central lacks (jumped counts them).
	barrier uint64
	jumped  int
}

// until applies the recording messages central receives until cond holds, or fails.
func (c *seqCentral) until(what string, cond func() bool) {
	c.t.Helper()
	if !c.within(10*time.Second, cond) {
		c.t.Fatalf("timed out waiting for %s (central holds up to %d, dropped %d repeats, recs %d)", what, c.seq, c.drops, len(c.recs))
	}
}

// within applies the recording messages central receives until cond holds (true) or d has passed (false).
func (c *seqCentral) within(d time.Duration, cond func() bool) bool {
	c.t.Helper()
	deadline := time.After(d)
	for !cond() {
		select {
		case env := <-c.fc.msgs:
			if env.Type != proto.TypeRecording {
				continue
			}
			var ch proto.RecordingChunk
			if err := env.Decode(&ch); err != nil {
				c.t.Fatal(err)
			}
			if c.mute {
				c.muted++
				c.mutedRecs += len(ch.Records)
				continue
			}
			if c.only != "" && ch.RunID != c.only {
				continue
			}
			for _, raw := range ch.Records {
				var r recording.Record
				if err := json.Unmarshal(raw, &r); err != nil {
					c.t.Fatal(err)
				}
				if r.Seq <= c.seq {
					c.drops++
					continue
				}
				if c.barrier > 0 && c.seq < c.barrier && r.Seq > c.barrier {
					c.jumped++
				}
				c.seq = r.Seq
				c.recs = append(c.recs, r)
			}
			c.a.recordingAck(proto.RecordingAck{RunID: ch.RunID, Seq: c.seq})
		case <-deadline:
			return false
		}
	}
	return true
}

func (c *seqCentral) has(data string) bool {
	for _, r := range c.recs {
		if strings.Contains(string(r.Data), data) {
			return true
		}
	}
	return false
}

// restartedRecorderEnv is an agent process started after another one recorded runID: the local recording holds
// local records (seq 1..local) and the run directory the acknowledgement saved; the run's program still runs (it is
// adopted). Central holds up to held, records the previous process numbered and shipped while its disk failed.
func restartedRecorderEnv(t *testing.T, runID string, local, saved, held uint64) (*Agent, *seqCentral, string) {
	t.Helper()
	return restartedRecorderEnvWith(t, runID, local, saved, held, nil)
}

// restartedRecorderEnvWith is restartedRecorderEnv with other live runs (each with 10 local records and no saved
// acknowledgement) journaled beside runID. Central's copy (seqCentral) is of runID alone: messages about the others are
// received and ignored. before runs once the agent exists, ahead of its connection.
func restartedRecorderEnvWith(t *testing.T, runID string, local, saved, held uint64, others []string, before ...func(a *Agent)) (*Agent, *seqCentral, string) {
	t.Helper()
	a, f := ccAgent(t, t.TempDir(), nil, nil)
	ctx := context.Background()
	var term string
	setup := func(id string, local, saved uint64) {
		p, err := f.Start(ctx, runner.Spec{Name: RunName(id), Argv: []string{"/bin/true"}, Labels: map[string]string{"spaceman.managed": "1", "spaceman.run": id}})
		if err != nil {
			t.Fatal(err)
		}
		if id == runID {
			term = p.ID
		}
		row := runRow{RunID: id, SpaceID: ccSpace, Generation: 1, Kind: "worker", Role: "implementor", Harness: "claude-code", TerminalName: RunName(id)}
		if err := a.j.insertRun(ctx, row); err != nil {
			t.Fatal(err)
		}
		row.TerminalID, row.State, row.Desired = p.ID, proto.RunRunning, "running"
		if err := a.j.updateRun(ctx, row); err != nil {
			t.Fatal(err)
		}
		w, err := recording.Open(recording.Options{Dir: a.RecordingDir(id)})
		if err != nil {
			t.Fatal(err)
		}
		for i := uint64(1); i <= local; i++ {
			r := recording.Record{Kind: recording.KindOut, Data: []byte("before ")}
			if i == 1 {
				r = recording.Record{Kind: recording.KindOpen, Cols: 80, Rows: 24, RunID: id}
			}
			if err := w.Add(r); err != nil {
				t.Fatal(err)
			}
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		if saved > 0 {
			a.saveAck(id, saved)
		}
	}
	setup(runID, local, saved)
	for _, id := range others {
		setup(id, 10, 0)
	}
	c := &seqCentral{t: t, a: a, seq: held}
	if len(others) > 0 {
		c.only = runID
	}
	for i := uint64(1); i <= held; i++ {
		c.recs = append(c.recs, recording.Record{Seq: i, Kind: recording.KindOut, Data: []byte("shipped before the restart ")})
	}
	for _, f := range before {
		f(a)
	}
	c.fc = newWSCentral(t, proto.MaxMinor)
	a.o.ReconnectMax = 10 * time.Millisecond
	runAgent(t, a, c.fc)
	return a, c, term
}

// localAbove returns the local recording's records above seq.
func localAbove(t *testing.T, a *Agent, runID string, seq uint64) []recording.Record {
	t.Helper()
	res, err := recording.Read(a.RecordingDir(runID), seq)
	if err != nil {
		t.Fatal(err)
	}
	return res.Records
}

// ADR-0097 addendum: the previous agent process numbered and shipped records while the node's disk was full, so central
// holds up to 200 while the local recording (and the acknowledgement saved on the same disk) stops at 100. The
// restarted agent learns central's sequence before it numbers anything: the output after the restart reaches central,
// numbered above 200, and no sequence number central holds is used again, locally or in a shipment.
func TestRestartedRecorderNumbersAboveCentral(t *testing.T) {
	const runID = "run_seq"
	a, c, term := restartedRecorderEnv(t, runID, 100, 100, 200)
	// Output from the restart on: the recorder attaches when the agent adopts the run; until central's answer it keeps
	// the output back.
	writeUntil(t, a, c, term, "after the restart")
	if c.drops > 0 {
		t.Fatalf("central dropped %d records as repeats: their sequence numbers were used again", c.drops)
	}
	for _, r := range c.recs[200:] {
		if r.Seq <= 200 {
			t.Fatalf("record %+v shipped at or below central's 200", r)
		}
	}
	// The local recording never holds other content under a sequence number central holds.
	for _, r := range localAbove(t, a, runID, 100) {
		if r.Seq <= 200 {
			t.Fatalf("local record %d (%s) reuses a sequence number central holds", r.Seq, r.Kind)
		}
	}
	if recs := localAbove(t, a, runID, 200); len(recs) == 0 {
		t.Fatal("the local recording holds nothing after the restart")
	}
}

// writeUntil writes output to the run's terminal until central holds it (output written before the recorder's
// attachment is open is lost by the fake runner).
func writeUntil(t *testing.T, a *Agent, c *seqCentral, term, out string) {
	t.Helper()
	fake := a.o.Runner.(interface{ Output(id, data string) })
	for i := 0; !c.within(300*time.Millisecond, func() bool { return c.has(out) }); i++ {
		if i > 30 {
			t.Fatalf("%q never reached central (it holds up to %d, dropped %d repeats)", out, c.seq, c.drops)
		}
		fake.Output(term, out)
	}
}

// ADR-0097 addendum, finding 2: central holds less than it acknowledged before the restart (up to 150, the saved
// acknowledgement says 300; a restored central, or one that acknowledged more than it stored). The first
// acknowledgement of the restarted process is authoritative: the local records above 150 go to central again, and new
// records are still numbered above 300, never at a number central once acknowledged.
func TestRestartedRecorderBelowTheSavedAcknowledgement(t *testing.T) {
	const runID = "run_low"
	a, c, term := restartedRecorderEnv(t, runID, 250, 300, 150)
	c.barrier = 250
	writeUntil(t, a, c, term, "after the restart")
	c.until("the local records above 150 at central", func() bool { return c.seq >= 250 })
	// Contiguous: 151..250 held in order, each once (central drops repeats, so anything else is a hole).
	c.contiguous(151, 250)
	// A repeat of 151..250 (the lowering starts a second sweep) is dropped by central and harmless; a jump is not.
	if c.jumped > 0 {
		t.Fatalf("%d records reached central above 250 before 151..250 did", c.jumped)
	}
	for _, r := range c.recs {
		if strings.Contains(string(r.Data), "after the restart") && r.Seq <= 300 {
			t.Fatalf("new output numbered %d, at or below the acknowledged 300", r.Seq)
		}
	}
	for _, r := range localAbove(t, a, runID, 250) {
		if r.Seq <= 300 {
			t.Fatalf("local record %d (%s) numbered at or below the acknowledged 300", r.Seq, r.Kind)
		}
	}
}

// With probe-first (ADR-0097, v0.1.12) the catch-up of a run whose central sequence is unknown waits for central's answer
// before it reads where to start: central holds 150, the saved acknowledgement says 300, and the catch-up begins from
// the lowered 150, never from 300. Nothing above 250 reaches central before 151..250 do, queued new records included.
func TestLoweredAckDuringTheRunsCatchUp(t *testing.T) {
	const runID = "run_low"
	var mu sync.Mutex
	var froms []uint64
	catchUpRunHook = func(id string, from uint64) {
		if id == runID {
			mu.Lock()
			froms = append(froms, from)
			mu.Unlock()
		}
	}
	t.Cleanup(func() { catchUpRunHook = nil })
	a, c, term := restartedRecorderEnv(t, runID, 250, 300, 150)
	c.barrier = 250
	writeUntil(t, a, c, term, "after the restart")
	c.until("the local records above 150 at central", func() bool { return c.seq >= 250 })
	c.contiguous(151, 250)
	if c.jumped > 0 {
		t.Fatalf("%d records reached central above 250 before 151..250 did", c.jumped)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, f := range froms {
		if f != 150 {
			t.Fatalf("the catch-up began from %d, want the lowered 150 (began from %v)", f, froms)
		}
	}
	if len(froms) == 0 {
		t.Fatal("the catch-up never began")
	}
}

// #254's restart on lowering is the guard for a lowering that arrives after the catch-up began from the restored
// acknowledgement (no longer reachable through the first answer of a run, which the catch-up now waits for). Forced
// here: the run's sequence counts as known (so the catch-up does not wait) with the saved 300 while central holds 150,
// the catch-up begins from 300, and the lowering is applied meanwhile, with new records queued above (the recorder
// numbers above 300). The catch-up must start again from 150 before anything above it goes out and before it marks
// the run caught up: central must end up with 151..250 in order, and never see a record above 250 before them.
func TestLoweringAfterTheCatchUpBeganRestartsIt(t *testing.T) {
	const runID = "run_low3"
	var ag atomic.Pointer[Agent]
	var froms []uint64
	var mu sync.Mutex
	catchUpRunHook = func(id string, from uint64) {
		if id != runID {
			return
		}
		mu.Lock()
		froms = append(froms, from)
		first := len(froms) == 1
		mu.Unlock()
		if !first {
			return
		}
		a := ag.Load()
		a.mu.Lock()
		a.seqKnown[runID] = false // central's answer has not been taken yet
		a.mu.Unlock()
		a.recordingAck(proto.RecordingAck{RunID: runID, Seq: 150})
	}
	t.Cleanup(func() { catchUpRunHook = nil })
	a, c, term := restartedRecorderEnvWith(t, runID, 250, 300, 150, nil, func(a *Agent) {
		ag.Store(a)
		a.mu.Lock()
		if a.seqKnown == nil {
			a.seqKnown = map[string]bool{}
		}
		a.seqKnown[runID] = true
		a.mu.Unlock()
	})
	c.barrier = 250
	writeUntil(t, a, c, term, "after the restart")
	c.until("the local records above 150 at central", func() bool { return c.seq >= 250 })
	c.contiguous(151, 250)
	if c.jumped > 0 {
		t.Fatalf("%d records reached central above 250 before 151..250 did", c.jumped)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(froms) < 2 || froms[0] != 300 || froms[1] != 150 {
		t.Fatalf("the catch-up began from %v, want 300 and then, restarted by the lowering, 150", froms)
	}
}

// countBetween counts the records numbered above lo and up to hi.
func countBetween(recs []recording.Record, lo, hi uint64) int {
	n := 0
	for _, r := range recs {
		if r.Seq > lo && r.Seq <= hi {
			n++
		}
	}
	return n
}

// A program that ends while central is out of reach after the restart: its records, the close among them, wait for
// central's sequence (they are not numbered meanwhile) and reach central once it answers.
func TestRestartedRecorderKeepsTheCloseUntilCentralAnswers(t *testing.T) {
	old := seqProbeEvery
	seqProbeEvery = 50 * time.Millisecond
	t.Cleanup(func() { seqProbeEvery = old })
	const runID = "run_end"
	a, c, term := restartedRecorderEnv(t, runID, 100, 100, 200)
	c.mute = true
	fake := a.o.Runner.(interface {
		Output(id, data string)
		Exit(id string, code int, reason string)
	})
	// The recorder is attached once its gap record is kept back (it numbers nothing while central is silent).
	waitFor(t, "recorder attached", func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.recorders[runID] != nil
	})
	// The recorder asks for central's sequence while it waits: once it has asked, it holds what follows back.
	c.until("the recorder's first probe", func() bool { return c.muted >= 1 })
	fake.Output(term, "last words\r\n")
	fake.Exit(term, 0, "exited")
	// After the exit the recorder keeps asking (it has the close record to ship): the probes that follow came from
	// the pump with the output and the exit already processed.
	base := c.muted
	c.until("probes after the exit", func() bool { return c.muted >= base+3 })
	if recs := localAbove(t, a, runID, 100); len(recs) > 0 {
		t.Fatalf("records numbered before central's sequence was known: %+v", recs)
	}
	c.mute = false
	c.until("the close record at central", func() bool { return len(c.recs) > 0 && c.recs[len(c.recs)-1].Kind == recording.KindClose })
	if c.drops > 0 || c.recs[200].Seq <= 200 {
		t.Fatalf("dropped %d, first record after the restart %+v", c.drops, c.recs[200])
	}
	if !c.has("last words") {
		t.Fatal("the output before the exit did not reach central")
	}
	waitFor(t, "recorder ended", func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.recorders[runID] == nil
	})
}

// The catch-up sweep must re-ship what the local recording holds above central's sequence for a run whose probe answer
// arrived before the sweep reached it. run_aaa's catch-up is held until run_zzz's recorder has central's answer: the
// acknowledgement (200) is then known when the sweep gets to run_zzz, and only the acknowledgement saved at the last
// process (200 as well, restored at the recorder's start) says that records 201..250 are local only. With saved 0 no
// acknowledgement file is restored (an older agent, a failed save): the run must still be compared with its local
// recording.
func testProbeAckBeforeTheSweepReachesTheRun(t *testing.T, block bool, saved uint64) {
	var ag atomic.Pointer[Agent]
	sendRecordingHook = func(id string) error {
		if !block || id != "run_aaa" {
			return nil
		}
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
			if a := ag.Load(); a != nil {
				a.mu.Lock()
				known := a.seqKnown["run_zzz"]
				a.mu.Unlock()
				if known {
					return nil
				}
			}
		}
		return nil
	}
	t.Cleanup(func() { sendRecordingHook = nil })
	_, c, _ := restartedRecorderEnvWith(t, "run_zzz", 250, saved, 200, []string{"run_aaa"}, func(a *Agent) { ag.Store(a) })
	c.until("run_zzz's records 201..250 at central", func() bool { return c.seq >= 250 })
	for i, r := range c.recs[200:250] {
		if r.Seq != uint64(201+i) {
			t.Fatalf("central holds %d after %d, want %d", r.Seq, 200+i, 201+i)
		}
	}
}

func TestSweepReshipsAboveTheAckWhenTheProbeAckComesFirst(t *testing.T) {
	testProbeAckBeforeTheSweepReachesTheRun(t, true, 200)
}

func TestSweepReshipsAboveTheAckWithoutTheProbeAckFirst(t *testing.T) {
	testProbeAckBeforeTheSweepReachesTheRun(t, false, 200)
}

func TestSweepReshipsWithoutASavedAckWhenTheProbeAckComesFirst(t *testing.T) {
	testProbeAckBeforeTheSweepReachesTheRun(t, true, 0)
}

func TestSweepReshipsWithoutASavedAckWithoutTheProbeAckFirst(t *testing.T) {
	testProbeAckBeforeTheSweepReachesTheRun(t, false, 0)
}

// The gate keeps output within seqGateMaxBytes and marks what it dropped with one gap record; other records are kept.
func TestSeqGateBound(t *testing.T) {
	var g seqGate
	big := make([]byte, seqGateMaxBytes/2)
	g.keep(recording.Record{Kind: recording.KindOut, Data: big})
	g.keep(recording.Record{Kind: recording.KindOut, Data: big}) // over the bound with the overhead: dropped
	g.keep(recording.Record{Kind: recording.KindOut, Data: []byte("x")})
	g.keep(recording.Record{Kind: recording.KindSize, Cols: 10, Rows: 5})
	g.keep(recording.Record{Kind: recording.KindClose})
	var kinds []string
	for _, r := range g.take() {
		kinds = append(kinds, r.Kind)
	}
	if got := strings.Join(kinds, ","); got != "o,gap,size,close" {
		t.Fatalf("kept %s, want o,gap,size,close", got)
	}
	if len(g.recs) != 0 || g.bytes != 0 || g.dropped {
		t.Fatalf("take left %+v", g)
	}
}

// contiguous fails unless central's records from..to (sequence numbers) are held in order, each once.
func (c *seqCentral) contiguous(from, to uint64) {
	c.t.Helper()
	if uint64(len(c.recs)) < to {
		c.t.Fatalf("central holds %d records, want %d", len(c.recs), to)
	}
	for i, r := range c.recs[from-1 : to] {
		if r.Seq != from+uint64(i) {
			c.t.Fatalf("central holds %d at position %d, want %d", r.Seq, from+uint64(i), from+uint64(i))
		}
	}
}

// ADR-0097 addendum, residual: a catch-up that starts from the restored acknowledgement (300) must not ship anything
// above it before central's sequence is known. An ended run has no recorder to ask, so its catch-up asks itself.
// Central lost records above 150 it had acknowledged: with the local recording at 250 nothing is above 300, so
// nothing would ever re-ship 151..250 if the catch-up took 300 for what central holds.
func TestEndedRunCatchUpProbesBeforeShipping(t *testing.T) {
	old := seqProbeEvery
	seqProbeEvery = 20 * time.Millisecond
	t.Cleanup(func() { seqProbeEvery = old })
	const runID = "run_ended"
	_, c, _ := restartedRecorderEnvWith(t, runID, 250, 300, 150, nil, func(a *Agent) {
		ctx := context.Background()
		row, err := a.j.getRun(ctx, runID)
		if err != nil {
			t.Fatal(err)
		}
		row.State, row.Desired = proto.RunExited, "stopped"
		if err := a.j.updateRun(ctx, row); err != nil {
			t.Fatal(err)
		}
	})
	c.mute = true
	// Central is out of reach: the catch-up asks and sends nothing else.
	c.until("the catch-up's probes", func() bool { return c.muted >= 3 })
	if c.mutedRecs > 0 {
		t.Fatalf("%d records left the node before central's sequence was known", c.mutedRecs)
	}
	c.mute = false
	c.barrier = 250
	c.until("records 151..250 at central", func() bool { return c.seq >= 250 })
	c.contiguous(151, 250)
	if c.jumped > 0 {
		t.Fatalf("%d records reached central above 250 before 151..250 did", c.jumped)
	}
}

// The same for a live run whose recorder numbers new records (above the restored 300) as soon as central answers, so
// they are queued behind the catch-up while it still starts from the stale 300: nothing above 250 may reach central
// before 151..250 do.
func TestLoweredAckWithQueuedRecordsShipsTheGapFirst(t *testing.T) {
	old := seqProbeEvery
	seqProbeEvery = 20 * time.Millisecond
	t.Cleanup(func() { seqProbeEvery = old })
	const runID = "run_low2"
	batches := 0
	shipBatchHook = func(id string, batch int) {
		if id == runID {
			batches++
		}
	}
	t.Cleanup(func() { shipBatchHook = nil })
	a, c, term := restartedRecorderEnv(t, runID, 250, 300, 150)
	c.barrier = 250
	writeUntil(t, a, c, term, "after the restart")
	c.until("records 151..250 at central", func() bool { return c.seq >= 250 })
	c.contiguous(151, 250)
	if c.jumped > 0 {
		t.Fatalf("%d records reached central above 250 before 151..250 did", c.jumped)
	}
	if batches == 0 {
		t.Fatal("the catch-up shipped nothing")
	}
}

// A run this process launched (nothing on disk) already knows central's sequence: uncomparedRecording must not mark it
// restored, or its first acknowledgement would be taken for a restored one and the run re-compared for nothing.
func TestLaunchedRunIsNotMarkedUncompared(t *testing.T) {
	a, _ := ccAgent(t, t.TempDir(), nil, nil)
	a.launchedRecording("run_launched")
	a.uncomparedRecording("run_launched")
	a.uncomparedRecording("run_foreign")
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.restored["run_launched"] {
		t.Fatal("a run this process launched was marked restored")
	}
	if !a.restored["run_foreign"] {
		t.Fatal("a run this process did not launch was not marked restored")
	}
}

func endRuns(t *testing.T, ids ...string) func(a *Agent) {
	return func(a *Agent) {
		ctx := context.Background()
		for _, id := range ids {
			row, err := a.j.getRun(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			row.State, row.Desired = proto.RunExited, "stopped"
			if err := a.j.updateRun(ctx, row); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func shortSeqWait(t *testing.T, wait time.Duration) {
	oldP, oldW := seqProbeEvery, seqKnownWait
	seqProbeEvery, seqKnownWait = 20*time.Millisecond, wait
	t.Cleanup(func() { seqProbeEvery, seqKnownWait = oldP, oldW })
}

// A probe answer that comes after seqKnownWait, for an ended run with no saved acknowledgement: the late answer must
// not make the sweep take the run for clean (the local records above it still have to be shipped).
func TestLateProbeAnswerStillShipsTheRun(t *testing.T) {
	shortSeqWait(t, 200*time.Millisecond)
	const runID = "run_late"
	_, c, _ := restartedRecorderEnvWith(t, runID, 250, 0, 150, nil, endRuns(t, runID))
	time.Sleep(350 * time.Millisecond) // central answers nothing until after seqKnownWait
	if !c.within(5*time.Second, func() bool { return c.seq >= 250 }) {
		t.Fatalf("central holds %d, want 250: records above the late answer were never shipped", c.seq)
	}
}

// Central never answers one ended run (only=run_b): that run is skipped, the ended run after it still catches up.
func TestUnansweredRunDoesNotBlockTheSweep(t *testing.T) {
	shortSeqWait(t, 200*time.Millisecond)
	_, c, _ := restartedRecorderEnvWith(t, "run_b", 250, 0, 150, []string{"run_a"}, endRuns(t, "run_b", "run_a"))
	if !c.within(8*time.Second, func() bool { return c.seq >= 250 }) {
		t.Fatalf("central holds %d of run_b, want 250", c.seq)
	}
}

// A run central never answers is retried after a backoff, not in a loop: over a few seconds the probes stay few.
func TestUnansweredRunBacksOff(t *testing.T) {
	shortSeqWait(t, 100*time.Millisecond)
	_, c, _ := restartedRecorderEnvWith(t, "run_b", 20, 0, 0, []string{"run_a"}, endRuns(t, "run_b", "run_a"))
	c.mute = true
	c.within(2500*time.Millisecond, func() bool { return false })
	// A sweep makes about 5-10 probes per run (100 ms wait, 20 ms apart); a hot loop would make over a hundred.
	if c.muted < 3 || c.muted > 60 {
		t.Fatalf("%d probes in 2.5 s: want a few sweeps separated by a backoff", c.muted)
	}
}
