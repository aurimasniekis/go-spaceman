package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/recording"
)

func TestShipPartsStayInsideAMessage(t *testing.T) {
	raw := func(n int) json.RawMessage { return json.RawMessage(`"` + strings.Repeat("x", n-2) + `"`) }
	// 22 MB of output in one flush interval, as a burst of 64 KiB records: no message may pass MaxMessage.
	var raws []json.RawMessage
	for range 22 << 20 >> 16 {
		raws = append(raws, raw(64<<10*4/3))
	}
	parts := shipParts(raws, shipChunkBytes)
	n := 0
	for _, p := range parts {
		size := 0
		for _, r := range p {
			size += len(r)
		}
		if size > shipChunkBytes {
			t.Fatalf("part of %d bytes", size)
		}
		b, err := proto.Encode("msg_x", proto.TypeRecording, time.Now(), proto.RecordingChunk{RunID: "run_x", SpaceID: "spc_x", Records: p})
		if err != nil || len(b) > proto.MaxMessage {
			t.Fatalf("message of %d bytes: %v", len(b), err)
		}
		n += len(p)
	}
	if n != len(raws) || len(parts) < 20 {
		t.Fatalf("%d records in %d parts", n, len(parts))
	}
	// Order is kept, a record larger than the limit goes alone, and nothing gives no parts.
	parts = shipParts([]json.RawMessage{raw(10), raw(30), raw(10), raw(10)}, 20)
	if len(parts) != 3 || len(parts[0]) != 1 || len(parts[1]) != 1 || len(parts[2]) != 2 {
		t.Fatalf("parts %v", parts)
	}
	if shipParts(nil, 20) != nil {
		t.Fatal("parts of nothing")
	}
}

// The reconnect catch-up re-ships a recording in batches of 256 records; with large records a batch is far over
// proto.MaxMessage. A central that enforces the limit (as the real one does) receives every record, in order, in
// messages inside it.
func TestCatchUpShipsLargeRecordingInBoundedMessages(t *testing.T) {
	dir := t.TempDir()
	a, _ := ccAgent(t, dir, nil, nil)
	ctx := context.Background()
	row := runRow{RunID: "run_big", SpaceID: ccSpace, Generation: 1, Kind: "worker", Role: "implementor", Harness: "claude-code", TerminalName: RunName("run_big")}
	if err := a.j.insertRun(ctx, row); err != nil {
		t.Fatal(err)
	}
	row.State, row.Desired = proto.RunExited, "stopped"
	if err := a.j.updateRun(ctx, row); err != nil {
		t.Fatal(err)
	}
	w, err := recording.Open(recording.Options{Dir: a.RecordingDir("run_big")})
	if err != nil {
		t.Fatal(err)
	}
	const n = 400 // 48 KiB each: a 256-record batch is about 16 MiB of JSON
	for i := range n {
		if err := w.Add(recording.Record{Kind: recording.KindOut, Data: bytes.Repeat([]byte{byte('a' + i%26)}, 48<<10)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	fc := newWSCentral(t, proto.MaxMinor)
	runAgent(t, a, fc)
	var seqs []uint64
	deadline := time.After(20 * time.Second)
	for len(seqs) < n {
		select {
		case env := <-fc.msgs:
			if env.Type == proto.TypeHello && len(seqs) > 0 {
				t.Fatal("the node reconnected: central closed the connection")
			}
			if env.Type != proto.TypeRecording {
				continue
			}
			var c proto.RecordingChunk
			if err := env.Decode(&c); err != nil {
				t.Fatal(err)
			}
			for _, raw := range c.Records {
				var r recording.Record
				if err := json.Unmarshal(raw, &r); err != nil {
					t.Fatal(err)
				}
				seqs = append(seqs, r.Seq)
			}
		case <-deadline:
			t.Fatalf("central received %d of %d records", len(seqs), n)
		}
	}
	for i, s := range seqs {
		if s != uint64(i+1) {
			t.Fatalf("record %d has seq %d", i, s)
		}
	}
}

// shipEnv is an agent connected to a fake central that keeps, per run, what central's writer keeps: records above the
// last sequence it stored, in arrival order (Writer.Append).
type shipEnv struct {
	t      *testing.T
	a      *Agent
	fc     *wsCentral
	stored map[string][]uint64
	writer map[string]*recording.Writer
	// lose, when set, is asked about each recording message central receives: true loses it (not stored, not
	// acknowledged), as if it went with its connection.
	lose func(c proto.RecordingChunk) bool
	// drop, when set, closes the connection on a message, losing it (wsCentral.drop); skip loses it and keeps the
	// connection (wsCentral.skip).
	drop func(env proto.Envelope) bool
	skip func(env proto.Envelope) bool
}

func newShipEnv(t *testing.T) *shipEnv {
	a, _ := ccAgent(t, t.TempDir(), nil, nil)
	t.Cleanup(func() { sendRecordingHook, shipBatchHook = nil, nil })
	return &shipEnv{t: t, a: a, stored: map[string][]uint64{}, writer: map[string]*recording.Writer{}}
}

// run journals a run (before connect, or during a pass: the agent journals a run before its program starts).
func (e *shipEnv) run(id string) {
	e.t.Helper()
	row := runRow{RunID: id, SpaceID: ccSpace, Generation: 1, Kind: "worker", Role: "implementor", Harness: "claude-code", TerminalName: RunName(id)}
	if err := e.a.j.insertRun(context.Background(), row); err != nil {
		e.t.Fatal(err)
	}
	w, err := recording.Open(recording.Options{Dir: e.a.RecordingDir(id)})
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { _ = w.Close() })
	e.writer[id] = w
}

// add records n output records of a run locally, as its recorder does before shipping them.
func (e *shipEnv) add(id string, n int) []recording.Record { return e.addSized(id, n, 1) }

// addSized is add with records of size bytes each.
func (e *shipEnv) addSized(id string, n, size int) []recording.Record {
	e.t.Helper()
	var out []recording.Record
	for range n {
		r, err := e.writer[id].AddRecord(recording.Record{Kind: recording.KindOut, Data: bytes.Repeat([]byte("x"), size)})
		if err != nil {
			e.t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := e.writer[id].Flush(); err != nil {
		e.t.Fatal(err)
	}
	return out
}

func (e *shipEnv) live(id string, recs []recording.Record) {
	e.a.shipLive(context.Background(), id, ccSpace, recs)
}

// connect connects the agent and returns once central's welcome started the first pass.
func (e *shipEnv) connect() {
	e.t.Helper()
	e.fc = newWSCentral(e.t, proto.MaxMinor)
	e.fc.drop, e.fc.skip = e.drop, e.skip
	e.a.o.ReconnectMax = 10 * time.Millisecond
	runAgent(e.t, e.a, e.fc)
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(time.Millisecond) {
		e.a.mu.Lock()
		up := e.a.connected
		e.a.mu.Unlock()
		if up {
			return
		}
		if time.Now().After(deadline) {
			e.t.Fatal("not connected")
		}
	}
}

// drain applies the recording messages central has received until it holds want records of the run, or fails.
func (e *shipEnv) drain(id string, want int) {
	e.t.Helper()
	deadline := time.After(10 * time.Second)
	for len(e.stored[id]) < want {
		select {
		case env := <-e.fc.msgs:
			if env.Type != proto.TypeRecording {
				continue
			}
			var c proto.RecordingChunk
			if err := env.Decode(&c); err != nil {
				e.t.Fatal(err)
			}
			if e.lose != nil && e.lose(c) {
				continue
			}
			for _, raw := range c.Records {
				var r recording.Record
				_ = json.Unmarshal(raw, &r)
				if s := e.stored[c.RunID]; len(s) == 0 || r.Seq > s[len(s)-1] {
					e.stored[c.RunID] = append(s, r.Seq)
				}
			}
			// Central acknowledges what it stored (RecordingAck), as the real one does after applying a message.
			if s := e.stored[c.RunID]; len(s) > 0 {
				e.a.recordingAck(proto.RecordingAck{RunID: c.RunID, Seq: s[len(s)-1]})
			}
		case <-deadline:
			e.t.Fatalf("central stored %d records of %s, want %d (last %v)", len(e.stored[id]), id, want, tailOf(e.stored[id]))
		}
	}
}

func tailOf(s []uint64) []uint64 { return s[max(0, len(s)-5):] }

// whole checks that central stored exactly from..to of a run, in order: nothing lost or out of order.
func (e *shipEnv) whole(id string, from, to uint64) {
	e.t.Helper()
	e.drain(id, int(to-from+1))
	for i, s := range e.stored[id] {
		if s != from+uint64(i) {
			e.t.Fatalf("central stored %s %v…, want %d-%d", id, e.stored[id][max(0, i-3):min(len(e.stored[id]), i+3)], from, to)
		}
	}
}

// idle waits until no catch-up pass runs.
func (e *shipEnv) idle() {
	e.t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		e.a.mu.Lock()
		busy := e.a.catching
		e.a.mu.Unlock()
		if !busy {
			return
		}
		if time.Now().After(deadline) {
			e.t.Fatal("the catch-up pass did not end")
		}
	}
}

// A live shipment between two batches of the run's catch-up waits: arriving first, it would make central drop the
// batches still to come.
func TestLiveShipmentWaitsForTheCatchUpBatches(t *testing.T) {
	e := newShipEnv(t)
	e.run("run_r")
	e.add("run_r", 600) // three batches
	once := false
	shipBatchHook = func(id string, batch int) {
		if batch == 1 && !once {
			once = true
			e.live(id, e.add(id, 1))
		}
	}
	e.connect()
	e.idle()
	e.whole("run_r", 1, 601)
}

// A run journaled during a pass whose first live shipment waited is caught up before the pass ends; its later
// shipments go straight to central (B1: its first records were lost).
func TestCatchUpOfARunStartedDuringThePass(t *testing.T) {
	e := newShipEnv(t)
	e.run("run_a")
	e.add("run_a", 600)
	started := false
	shipBatchHook = func(id string, batch int) {
		if id == "run_a" && batch == 1 && !started {
			started = true
			e.run("run_b")
			e.live("run_b", e.add("run_b", 3))
		}
	}
	e.connect()
	e.idle()
	e.live("run_b", e.add("run_b", 1))
	e.whole("run_b", 1, 4)
	e.whole("run_a", 1, 600)
}

// Another pass asked for (a relocation) while the pass ships a run neither marks the run caught up early nor lets
// its next live shipment overtake the batches still to come (B2).
func TestSecondPassRequestWhileARunIsShipped(t *testing.T) {
	e := newShipEnv(t)
	e.run("run_r")
	e.add("run_r", 600)
	asked := false
	shipBatchHook = func(id string, batch int) {
		if batch == 1 && !asked {
			asked = true
			e.a.startCatchUp()
			// One pass at a time: the running pass sweeps again instead of a second one starting beside it.
			e.a.mu.Lock()
			again := e.a.catchUpAgain
			e.a.mu.Unlock()
			if !again {
				t.Error("a second pass started beside the running one")
			}
			e.live(id, e.add(id, 1))
		}
	}
	e.connect()
	e.idle()
	e.whole("run_r", 1, 601)
}

// Records sent on a connection that then went away, and never acknowledged, are sent again on the next connection,
// before the run's next live shipment (S1).
func TestCatchUpResendsUnacknowledgedRecords(t *testing.T) {
	e := newShipEnv(t)
	e.run("run_r")
	e.add("run_r", 10)
	e.a.recordingAck(proto.RecordingAck{RunID: "run_r", Seq: 5})
	e.a.sentSeq = map[string]uint64{"run_r": 10} // 6-10 went out on the old connection and were lost
	e.connect()
	e.idle()
	e.live("run_r", e.add("run_r", 1))
	e.whole("run_r", 6, 11)
}

// A close record with no local copy (the local disk failed) that waited for the catch-up is sent after it (S3).
func TestCatchUpSendsRecordsWithoutLocalCopy(t *testing.T) {
	e := newShipEnv(t)
	e.run("run_r")
	e.add("run_r", 5)
	code := 0
	e.live("run_r", []recording.Record{{Seq: 6, Kind: recording.KindClose, ExitCode: &code, Reason: "exited"}}) // not connected yet
	e.connect()
	e.idle()
	e.whole("run_r", 1, 6)
}

// A failed send stops the run's catch-up: no later batch goes past the hole (central would keep it and drop the
// missing one). The pass tries again (here the connection is still there) from what central acknowledged.
func TestCatchUpStopsAtTheFirstFailedSend(t *testing.T) {
	e := newShipEnv(t)
	e.run("run_r")
	e.add("run_r", 800) // four batches
	var sends atomic.Int32
	sendRecordingHook = func(string) error {
		if sends.Add(1) == 2 {
			return errors.New("connection lost")
		}
		return nil
	}
	e.connect()
	e.drain("run_r", 256)
	select {
	case env := <-e.fc.msgs:
		if env.Type == proto.TypeRecording {
			t.Fatal("a batch was sent past the failed one")
		}
	case <-time.After(300 * time.Millisecond): // the retry waits a second
	}
	e.whole("run_r", 1, 800)
	e.idle()
}

// A pass still sweeping for the old connection must not mark a run caught up for the new connection's pass: here the
// run's only batch was lost with the old connection, and the run's next live record would otherwise go ahead of it
// (B1′, the reviewer's probe).
func TestCatchUpOfTheOldConnectionDoesNotCountForTheNewOne(t *testing.T) {
	e := newShipEnv(t)
	e.run("run_x")
	e.add("run_x", 10)
	lost := false
	e.lose = func(c proto.RecordingChunk) bool {
		if !lost {
			lost = true
			return true
		}
		return false
	}
	reconnected := false
	shipBatchHook = func(string, int) {
		if !reconnected {
			reconnected = true
			e.a.mu.Lock()
			e.a.startCatchUpLocked() // what a new connection's welcome does
			e.a.mu.Unlock()
		}
	}
	e.connect()
	e.drain("run_x", 0)
	e.idle()
	e.live("run_x", e.add("run_x", 1))
	e.whole("run_x", 1, 11)
}

// A real reconnect in the middle of a pass: central drops the connection on the first batch (losing it); the old
// pass's next batch must not go out on the new connection, the new connection's pass sweeps again (catchUpAgain)
// from what central acknowledged, and the run ends up whole.
func TestCatchUpAcrossAReconnect(t *testing.T) {
	e := newShipEnv(t)
	e.run("run_x")
	e.add("run_x", 600) // three batches
	var dropped atomic.Bool
	e.drop = func(env proto.Envelope) bool {
		return env.Type == proto.TypeRecording && dropped.CompareAndSwap(false, true)
	}
	var waited atomic.Bool
	shipBatchHook = func(string, int) {
		if !waited.CompareAndSwap(false, true) {
			return
		}
		// Hold the old pass until the new connection's welcome asked for a sweep on it.
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
			e.a.mu.Lock()
			again := e.a.catchUpAgain
			e.a.mu.Unlock()
			if again {
				return
			}
		}
		t.Error("the agent did not reconnect")
	}
	e.connect()
	e.whole("run_x", 1, 600)
	e.idle()
	e.live("run_x", e.add("run_x", 1))
	e.whole("run_x", 1, 601)
}

// A run caught up on the old connection, whose last live record was lost with it, is caught up again on the new
// connection's pass before its next record (the caught-up marks start afresh for every pass).
func TestCatchUpAgainAfterAReconnect(t *testing.T) {
	e := newShipEnv(t)
	e.run("run_x")
	e.add("run_x", 10)
	e.connect()
	e.whole("run_x", 1, 10)
	e.idle()
	e.lose = func(c proto.RecordingChunk) bool { return true }
	e.live("run_x", e.add("run_x", 1))               // 11, lost with the connection
	for deadline := time.After(5 * time.Second); ; { // the lost message
		select {
		case env := <-e.fc.msgs:
			if env.Type != proto.TypeRecording {
				continue
			}
		case <-deadline:
			t.Fatal("record 11 was not sent")
		}
		break
	}
	e.lose = nil
	e.a.startCatchUp() // a new pass (a reconnect's welcome does the same)
	e.idle()
	e.live("run_x", e.add("run_x", 1))
	e.whole("run_x", 1, 12)
}

// More than heldMaxBytes of live output while the pass ships the run: what could not be kept back is read again from
// the local recording.
func TestCatchUpReadsAgainWhatCouldNotBeKeptBack(t *testing.T) {
	e := newShipEnv(t)
	e.run("run_x")
	e.add("run_x", 300) // two batches
	once := false
	shipBatchHook = func(id string, batch int) {
		if batch == 2 && !once {
			once = true
			e.live(id, e.addSized(id, 24, 64<<10)) // 1.5 MiB: past the bound
		}
	}
	e.connect()
	e.idle()
	e.live("run_x", e.add("run_x", 1))
	e.whole("run_x", 1, 325)
}

// A live send that fails keeps its records back, so a record with no local copy still reaches central after the
// catch-up (a close record after the local disk failed).
func TestFailedLiveShipmentKeepsRecordsWithoutLocalCopy(t *testing.T) {
	e := newShipEnv(t)
	e.run("run_x")
	e.add("run_x", 5)
	e.connect()
	e.whole("run_x", 1, 5)
	e.idle()
	sendRecordingHook = func(string) error { return errors.New("connection lost") }
	code := 0
	e.live("run_x", []recording.Record{{Seq: 6, Kind: recording.KindClose, ExitCode: &code, Reason: "exited"}})
	sendRecordingHook = nil
	e.a.startCatchUp()
	e.idle()
	e.whole("run_x", 1, 6)
}

// A live shipment of several messages that a reconnect interrupts: its later messages must not go out on the new
// connection, where they would arrive before the catch-up re-sends the message lost with the old one.
func TestLiveShipmentStaysOnItsConnection(t *testing.T) {
	e := newShipEnv(t)
	e.run("run_x")
	e.add("run_x", 10)
	var armed atomic.Bool
	e.drop = func(env proto.Envelope) bool {
		return env.Type == proto.TypeRecording && armed.CompareAndSwap(true, false)
	}
	e.connect()
	e.whole("run_x", 1, 10)
	e.idle()

	e.a.mu.Lock()
	old := e.a.conn
	e.a.mu.Unlock()
	reconnected := func() bool {
		e.a.mu.Lock()
		defer e.a.mu.Unlock()
		return e.a.connected && e.a.conn != old
	}
	var calls atomic.Int32
	partTwoOut := make(chan struct{})
	sendRecordingHook = func(string) error {
		switch calls.Add(1) {
		case 1: // the live shipment's first message: central drops the connection on it
		case 2: // its second message: held until the agent has reconnected
			for deadline := time.Now().Add(10 * time.Second); !reconnected(); time.Sleep(time.Millisecond) {
				if time.Now().After(deadline) {
					t.Error("the agent did not reconnect")
					break
				}
			}
			time.AfterFunc(20*time.Millisecond, func() { close(partTwoOut) })
		default: // the new connection's catch-up waits until the second message has gone out (or failed)
			<-partTwoOut
		}
		return nil
	}
	armed.Store(true)
	e.live("run_x", e.addSized("run_x", 30, 64<<10)) // three messages
	e.idle()
	e.live("run_x", e.add("run_x", 1))
	e.whole("run_x", 1, 41)
}

// Records kept back that a catch-up could not send go back for the next pass: a close record with no local copy
// exists only there (B1″, the reviewer's probe).
func TestCatchUpKeepsBackWhatItCouldNotSend(t *testing.T) {
	e := newShipEnv(t)
	e.run("run_x")
	e.add("run_x", 300) // two batches
	code := 0
	once := false
	shipBatchHook = func(id string, batch int) {
		if batch == 1 && !once {
			once = true
			e.live(id, []recording.Record{{Seq: 301, Kind: recording.KindClose, ExitCode: &code, Reason: "exited"}}) // no local copy
		}
	}
	var sends atomic.Int32
	sendRecordingHook = func(string) error {
		if sends.Add(1) == 3 { // the send of what was kept back, once; the connection stays
			return errors.New("write failed")
		}
		return nil
	}
	e.connect()
	e.whole("run_x", 1, 301)
	e.idle()
}

// A send counts as sent before it is written: a pass asked for while it is on its way (a reconnect) must not take the
// run for clean, or the record, lost with the old connection, would be missing before the run's next one (S1″).
func TestLiveShipmentOnItsWayIsNotTakenForSent(t *testing.T) {
	e := newShipEnv(t)
	e.run("run_x")
	e.add("run_x", 10)
	var seen atomic.Int32 // recording messages central received
	var lose atomic.Bool
	e.skip = func(env proto.Envelope) bool {
		if env.Type != proto.TypeRecording {
			return false
		}
		seen.Add(1)
		return lose.CompareAndSwap(true, false)
	}
	e.connect()
	e.whole("run_x", 1, 10)
	e.idle()
	before := seen.Load()
	once := false
	sendRecordingHook = func(string) error {
		if once {
			return nil
		}
		once = true
		e.a.startCatchUp() // a reconnect's pass, while record 11 is on its way
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
			e.a.mu.Lock()
			busy := e.a.catching
			e.a.mu.Unlock()
			if !busy {
				break
			}
		}
		// Let central receive what the pass sent (if anything), then lose the live message itself.
		for deadline := time.Now().Add(300 * time.Millisecond); seen.Load() == before && time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		}
		lose.Store(true)
		return nil
	}
	e.live("run_x", e.add("run_x", 1))
	sendRecordingHook = nil
	e.live("run_x", e.add("run_x", 1))
	e.whole("run_x", 1, 12)
}

// The memory a run keeps back is bounded counting each record's overhead, not only its data: many small records
// overflow it (and are then read again from the local recording).
func TestLiveShipmentKeptBackIsBounded(t *testing.T) {
	e := newShipEnv(t)
	e.run("run_x")
	e.live("run_x", e.add("run_x", 20000)) // not connected: kept back; 20 KB of data, 1.3 MB with overhead
	e.a.mu.Lock()
	h := e.a.held["run_x"]
	e.a.mu.Unlock()
	if h == nil || !h.lost || h.bytes > heldMaxBytes || len(h.recs) >= 20000 {
		t.Fatalf("kept back %d records, %d bytes, lost %v", len(h.recs), h.bytes, h.lost)
	}
	e.connect()
	e.whole("run_x", 1, 20000)
}
