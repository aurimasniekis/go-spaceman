package fleet

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/recording"
)

// A recording message central could not apply at all (the recording cannot be opened, nothing stored before) gets no
// acknowledgement: a node that probes for central's sequence would take an ack of 0 for central's answer (ADR-0097
// addendum). Not being a storage failure, it is logged and leaves the connection open (ADR-0010 addendum); the node
// asks again. One that was applied is acknowledged.
func TestHubRecordingAckOnlyWhenApplied(t *testing.T) {
	e := newStaleEnv(t)
	e.exec(`INSERT INTO nodes (id, name, state, created_at, created_by, updated_at) VALUES ('nod_1', 'n1', 'active', 0, 't', 0)`)
	e.exec(`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('spc_a', 'aaa', 'A', 'nod_1', 'active', 'a', 0, 't', 0)`)
	const runID = "run_00000000000000000000000001"
	e.run(runID, "spc_a", "nod_1", "running", e.us(-time.Hour))
	s := &session{nodeID: "nod_1", out: make(chan []byte, 8), done: make(chan struct{})}
	h := e.svc.hub
	send := func() error {
		b, err := proto.Encode("msg_1", proto.TypeRecording, e.t0, proto.RecordingChunk{RunID: runID, SpaceID: "spc_a"})
		if err != nil {
			t.Fatal(err)
		}
		env, err := proto.Decode(b)
		if err != nil {
			t.Fatal(err)
		}
		return h.handle(e.ctx, s, env)
	}
	// A recordings directory below a regular file cannot be created.
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	e.svc.opts.RecordingsDir = filepath.Join(blocker, "rec")
	e.svc.rec = newRecordings(e.svc.opts.RecordingsDir, nil, nil, e.clk.Now)
	for range 2 { // the open itself, then a refusal in its backoff window
		if err := send(); err != nil {
			t.Fatalf("an open failure ended the connection: %v", err)
		}
		if len(s.out) != 0 {
			t.Fatal("central acknowledged a message it could not apply")
		}
		select {
		case <-s.done:
			t.Fatal("an open failure closed the connection")
		default:
		}
	}
	good := t.TempDir()
	e.svc.opts.RecordingsDir = good
	e.svc.rec = newRecordings(good, nil, nil, e.clk.Now)
	t.Cleanup(e.svc.rec.cancel)
	if err := send(); err != nil {
		t.Fatal(err)
	}
	if len(s.out) != 1 {
		t.Fatalf("an applied (empty) message got %d acknowledgements, want 1", len(s.out))
	}
}

// A message central could not store although it knows a stored sequence is still acknowledged with it: here a chunk
// refused in the reopen backoff after a storage failure on the same connection, which was already closed for that
// failure once (so this refusal does not close it again).
func TestHubRecordingAckOnFailureWithAStoredSequence(t *testing.T) {
	e := newStaleEnv(t)
	e.exec(`INSERT INTO nodes (id, name, state, created_at, created_by, updated_at) VALUES ('nod_1', 'n1', 'active', 0, 't', 0)`)
	e.exec(`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('spc_a', 'aaa', 'A', 'nod_1', 'active', 'a', 0, 't', 0)`)
	const runID = "run_00000000000000000000000001"
	e.run(runID, "spc_a", "nod_1", "running", e.us(-time.Hour))
	s := &session{nodeID: "nod_1", seq: 7, out: make(chan []byte, 8), done: make(chan struct{})}
	dir := t.TempDir()
	e.svc.opts.RecordingsDir = dir
	e.svc.rec = newRecordings(dir, nil, nil, e.clk.Now)
	t.Cleanup(e.svc.rec.cancel)
	var fail atomic.Bool
	e.svc.rec.create = func(path string) (recording.File, error) {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // test path
		if err != nil {
			return nil, err
		}
		return syncFailFile{File: f, fail: &fail}, nil
	}
	chunk := func(seqs ...uint64) proto.RecordingChunk {
		c := proto.RecordingChunk{RunID: runID, SpaceID: "spc_a", Records: []json.RawMessage{}}
		for _, n := range seqs {
			b, err := json.Marshal(recording.Record{Seq: n, T: int64(n), Kind: recording.KindOut, Data: []byte("x")})
			if err != nil {
				t.Fatal(err)
			}
			c.Records = append(c.Records, b)
		}
		return c
	}
	// Stored 1..2, then a storage failure on this connection (the hub would close it once for that).
	if seq, err := e.svc.rec.apply(s.seq, chunk(1, 2)); err != nil || seq != 2 {
		t.Fatalf("first chunk %d %v", seq, err)
	}
	fail.Store(true)
	if _, err := e.svc.rec.apply(s.seq, chunk(3)); !isClose(err) {
		t.Fatalf("storage failure %v, want a close", err)
	}
	b, err := proto.Encode("msg_1", proto.TypeRecording, e.t0, chunk(3))
	if err != nil {
		t.Fatal(err)
	}
	env, err := proto.Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.svc.hub.handle(e.ctx, s, env); err != nil {
		t.Fatalf("a refusal on an already closed-for connection: %v", err)
	}
	if len(s.out) != 1 {
		t.Fatalf("%d acknowledgements of a refused chunk with a stored sequence, want 1", len(s.out))
	}
	got, err := proto.Decode(<-s.out)
	if err != nil {
		t.Fatal(err)
	}
	var ack proto.RecordingAck
	if err := got.Decode(&ack); err != nil || got.Type != proto.TypeRecordingAck || ack.Seq != 2 {
		t.Fatalf("ack %s %+v %v, want recording.ack 2", got.Type, ack, err)
	}
}
