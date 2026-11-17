package agent

import (
	"context"
	"encoding/json"
	"strings"
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
	// muted counts the recording messages lost while mute was set (the recorder's probes among them).
	muted int
}

// until applies the recording messages central receives until cond holds, or fails.
func (c *seqCentral) until(what string, cond func() bool) {
	c.t.Helper()
	if !c.within(10*time.Second, cond) {
		c.t.Fatalf("timed out waiting for %s (central holds up to %d, dropped %d repeats)", what, c.seq, c.drops)
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
	a, f := ccAgent(t, t.TempDir(), nil, nil)
	ctx := context.Background()
	p, err := f.Start(ctx, runner.Spec{Name: RunName(runID), Argv: []string{"/bin/true"}, Labels: map[string]string{"spaceman.managed": "1", "spaceman.run": runID}})
	if err != nil {
		t.Fatal(err)
	}
	row := runRow{RunID: runID, SpaceID: ccSpace, Generation: 1, Kind: "worker", Role: "implementor", Harness: "claude-code", TerminalName: RunName(runID)}
	if err := a.j.insertRun(ctx, row); err != nil {
		t.Fatal(err)
	}
	row.TerminalID, row.State, row.Desired = p.ID, proto.RunRunning, "running"
	if err := a.j.updateRun(ctx, row); err != nil {
		t.Fatal(err)
	}
	w, err := recording.Open(recording.Options{Dir: a.RecordingDir(runID)})
	if err != nil {
		t.Fatal(err)
	}
	for i := uint64(1); i <= local; i++ {
		r := recording.Record{Kind: recording.KindOut, Data: []byte("before ")}
		if i == 1 {
			r = recording.Record{Kind: recording.KindOpen, Cols: 80, Rows: 24, RunID: runID}
		}
		if err := w.Add(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if saved > 0 {
		a.saveAck(runID, saved)
	}
	c := &seqCentral{t: t, a: a, seq: held}
	for i := uint64(1); i <= held; i++ {
		c.recs = append(c.recs, recording.Record{Seq: i, Kind: recording.KindOut, Data: []byte("shipped before the restart ")})
	}
	c.fc = newWSCentral(t, proto.MaxMinor)
	a.o.ReconnectMax = 10 * time.Millisecond
	runAgent(t, a, c.fc)
	return a, c, p.ID
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
	writeUntil(t, a, c, term, "after the restart")
	c.until("the local records above 150 at central", func() bool {
		n := 0
		for _, r := range c.recs {
			if r.Seq > 150 && r.Seq <= 250 {
				n++
			}
		}
		return n == 100
	})
	if c.drops > 0 {
		t.Fatalf("central dropped %d records as repeats", c.drops)
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
