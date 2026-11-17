package fleet

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// A recording message central could not apply at all (the recording cannot be opened) gets no acknowledgement: a node
// that probes for central's sequence would take it for central's answer. One that was applied is acknowledged.
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
	if err := send(); err == nil {
		t.Fatal("a recording that cannot be opened was applied")
	}
	if len(s.out) != 0 {
		t.Fatal("central acknowledged a message it could not apply")
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
