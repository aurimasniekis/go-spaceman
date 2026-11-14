package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
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

// Central keeps only records above the last sequence it stored, so a live shipment must never overtake records a
// catch-up still has to send: those would be lost for good. Here a run's records 11-20 were lost (marked unshipped),
// a live shipment arrives while the run is marked and another while the pass is shipping it, and one after the pass.
// What central would store is every record, 1-23, in order.
func TestLiveShipmentsWaitForTheCatchUp(t *testing.T) {
	dir := t.TempDir()
	a, _ := ccAgent(t, dir, nil, nil)
	ctx := context.Background()
	const run = "run_order"
	row := runRow{RunID: run, SpaceID: ccSpace, Generation: 1, Kind: "worker", Role: "implementor", Harness: "claude-code", TerminalName: RunName(run)}
	if err := a.j.insertRun(ctx, row); err != nil {
		t.Fatal(err)
	}
	w, err := recording.Open(recording.Options{Dir: a.RecordingDir(run)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	add := func(n int) []recording.Record {
		var out []recording.Record
		for range n {
			r, err := w.AddRecord(recording.Record{Kind: recording.KindOut, Data: []byte("x")})
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, r)
		}
		if err := w.Flush(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	add(10)

	fc := newWSCentral(t, proto.MaxMinor)
	runAgent(t, a, fc)
	// What central stores: records above the last stored sequence, in arrival order.
	var stored []uint64
	receive := func(want int) {
		t.Helper()
		deadline := time.After(10 * time.Second)
		for len(stored) < want {
			select {
			case env := <-fc.msgs:
				if env.Type != proto.TypeRecording {
					continue
				}
				var c proto.RecordingChunk
				if err := env.Decode(&c); err != nil {
					t.Fatal(err)
				}
				for _, raw := range c.Records {
					var r recording.Record
					_ = json.Unmarshal(raw, &r)
					if len(stored) == 0 || r.Seq > stored[len(stored)-1] {
						stored = append(stored, r.Seq)
					}
				}
			case <-deadline:
				t.Fatalf("central stored %v, want %d records", stored, want)
			}
		}
	}
	receive(10) // the first pass after connecting: the run was unknown

	// Records 11-20 were lost (a failed send marks the run); a live shipment of 21 must wait.
	add(10)
	a.markUnshipped(run)
	a.shipLive(ctx, run, ccSpace, add(1))
	select {
	case env := <-fc.msgs:
		if env.Type == proto.TypeRecording {
			t.Fatal("a live shipment went ahead of the records the catch-up has to send")
		}
	case <-time.After(200 * time.Millisecond):
	}

	// The next pass; while it ships the run, another live shipment (22) waits and is caught up in the same pass.
	calls := 0
	catchUpRunHook = func(id string) {
		if id == run && calls == 0 {
			a.shipLive(ctx, run, ccSpace, add(1))
		}
		calls++
	}
	t.Cleanup(func() { catchUpRunHook = nil })
	a.beginCatchUp()
	a.catchUpRecordings(ctx)
	receive(22)
	// After the pass, live shipments go straight to central.
	a.shipLive(ctx, run, ccSpace, add(1))
	receive(23)
	for i, s := range stored {
		if s != uint64(i+1) {
			t.Fatalf("central stored %v", stored)
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.catchUps != 0 || a.unshipped[run] {
		t.Fatalf("after the pass: %d passes running, run marked %v", a.catchUps, a.unshipped[run])
	}
}
