package fleet

import (
	"testing"
	"time"
)

// A connection that a newer one replaced records nothing when it ends: the node is connected again, so its runs keep
// their observed state and no disconnect is recorded. The newest connection's end records both (SM-NODE-005).
func TestReplacedSessionDisconnect(t *testing.T) {
	e := newStaleEnv(t)
	e.exec(`INSERT INTO nodes (id, name, state, created_at, created_by, updated_at) VALUES ('nod_1', 'n1', 'active', 0, 't', 0)`)
	e.exec(`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('spc_a', 'aaa', 'A', 'nod_1', 'active', 'a', 0, 't', 0)`)
	e.run("run_1", "spc_a", "nod_1", "running", e.us(-time.Hour))
	h := e.svc.hub
	s1 := &session{nodeID: "nod_1", seq: 1, counted: true}
	s2 := &session{nodeID: "nod_1", seq: 2, counted: true}
	// s2 replaced s1: it claimed the node in its fleet.node_connect transaction and was registered.
	h.mu.Lock()
	h.seq, h.newest["nod_1"], h.sessions["nod_1"] = 2, 2, s2
	h.mu.Unlock()

	h.onDisconnect(e.ctx, s1)
	if r := e.observed("run_1"); r.Observed != "running" {
		t.Fatalf("the replaced connection's end marked the run %s", r.Observed)
	}
	if n := e.count(`SELECT count(*) FROM nodes WHERE id = 'nod_1' AND last_disconnect_at IS NOT NULL`); n != 0 {
		t.Fatal("the replaced connection's end recorded a disconnect")
	}
	if !h.Connected("nod_1") {
		t.Fatal("the replaced connection's end unregistered the newer one")
	}

	h.onDisconnect(e.ctx, s2)
	if r := e.observed("run_1"); r.Observed != RunUnknown {
		t.Fatalf("the newest connection's end left the run %s", r.Observed)
	}
	if n := e.count(`SELECT count(*) FROM nodes WHERE id = 'nod_1' AND last_disconnect_at IS NOT NULL`); n != 1 {
		t.Fatal("the newest connection's end recorded no disconnect")
	}
	if h.Connected("nod_1") {
		t.Fatal("the newest connection is still registered")
	}
}
