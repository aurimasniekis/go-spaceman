package fleet

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// flushSession registers a connected node session on minor 5 or later whose outbound queue holds queue messages and is
// drained only by the test.
func flushSession(e *staleEnv, node string, queue int) *session {
	e.t.Helper()
	s := &session{nodeID: node, out: make(chan []byte, queue), kick: make(chan struct{}, 1), done: make(chan struct{}),
		log: slog.New(slog.DiscardHandler), minor: proto.MaxMinor}
	e.t.Cleanup(func() { close(s.done) })
	e.svc.hub.mu.Lock()
	e.svc.hub.sessions[node] = s
	e.svc.hub.mu.Unlock()
	return s
}

// SM-MEM-005: a busy node whose outbound queue is full (its writer never drains it) cannot hold the backup's memory
// flush past its bound. The deadline runs from the start, the request to the busy node gives up when it passes, and
// another node is still asked and answers: the busy node's spaces are reported as timed out with a detail, the other
// node's as flushed, within the wait plus the grace. Before the fix, the request blocked on the full queue with no
// bound, so the flush never returned and the other node was never asked.
func TestFlushMemoryBusyNodeFullQueue(t *testing.T) {
	t.Parallel()
	e := newStaleEnv(t)
	e.exec(`INSERT INTO nodes (id, name, state, created_at, created_by, updated_at) VALUES ('nod_1', 'busy', 'active', 0, 't', 0),
		('nod_2', 'idle', 'active', 0, 't', 0)`)
	e.exec(`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES
		('spc_a', 'aaa', 'A', 'nod_1', 'active', 'a', 0, 't', 0), ('spc_b', 'bbb', 'B', 'nod_2', 'active', 'b', 0, 't', 0)`)
	e.exec(`INSERT INTO memory_collections (id, space_id, role, created_at, updated_at) VALUES ('mco_a', 'spc_a', 'implementor', 0, 0),
		('mco_b', 'spc_b', 'implementor', 0, 0)`)
	flushSession(e, "nod_1", 0) // unbuffered and never read: every send blocks
	idle := flushSession(e, "nod_2", 8)
	go func() {
		// The idle node answers its request at once.
		select {
		case b := <-idle.out:
			env, err := proto.Decode(b)
			if err != nil {
				return
			}
			var req proto.MemoryFlush
			if env.Decode(&req) != nil {
				return
			}
			e.svc.hub.flushed("nod_2", proto.MemoryFlushed{FlushID: req.FlushID, Spaces: []proto.MemoryFlushSpace{{SpaceID: "spc_b", Flushed: true, Collections: 1}}})
		case <-time.After(30 * time.Second):
		}
	}()

	type out struct {
		res []MemoryFlushResult
		err error
	}
	done := make(chan out, 1)
	progress := make(chan struct{}, 4)
	start := time.Now()
	go func() {
		r, err := e.svc.FlushMemory(e.ctx, time.Second, func() { progress <- struct{}{} })
		done <- out{r, err}
	}()
	var got out
	select {
	case got = <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("FlushMemory is still blocked: the send to the busy node has no bound")
	}
	// wait 1 s + grace 1 s, with slack for a loaded machine.
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("the flush took %s", d)
	}
	if got.err != nil || len(got.res) != 2 {
		t.Fatalf("results: %+v %v", got.res, got.err)
	}
	a, b := got.res[0], got.res[1]
	if a.SpaceID != "spc_a" || a.Flushed || a.Reason != FlushTimedOut || !strings.Contains(a.Detail, "queue") || a.NodeName != "busy" {
		t.Fatalf("busy node: %+v", a)
	}
	if b.SpaceID != "spc_b" || !b.Flushed || b.NodeName != "idle" {
		t.Fatalf("idle node: %+v", b)
	}
	if n := len(progress); n != 2 {
		t.Fatalf("progress reported %d times, want once per node", n)
	}
}

// A caller's deadline shorter than the wait caps what nodes are told to wait, and a node that does not answer is
// reported as timed out with the wait as the reason, not as a stopped backup.
func TestFlushMemoryCapsWaitToContextDeadline(t *testing.T) {
	t.Parallel()
	e := newStaleEnv(t)
	e.exec(`INSERT INTO nodes (id, name, state, created_at, created_by, updated_at) VALUES ('nod_1', 'quiet', 'active', 0, 't', 0)`)
	e.exec(`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES
		('spc_a', 'aaa', 'A', 'nod_1', 'active', 'a', 0, 't', 0)`)
	e.exec(`INSERT INTO memory_collections (id, space_id, role, created_at, updated_at) VALUES ('mco_a', 'spc_a', 'implementor', 0, 0)`)
	s := flushSession(e, "nod_1", 8)
	ctx, cancel := context.WithTimeout(e.ctx, 2*time.Second)
	defer cancel()
	res, err := e.svc.FlushMemory(ctx, time.Minute, nil)
	if err != nil || len(res) != 1 {
		t.Fatalf("results: %+v %v", res, err)
	}
	if r := res[0]; r.Flushed || r.Reason != FlushTimedOut || !strings.Contains(r.Detail, "no answer within") {
		t.Fatalf("result: %+v", r)
	}
	env, err := proto.Decode(<-s.out)
	if err != nil {
		t.Fatal(err)
	}
	var req proto.MemoryFlush
	if env.Decode(&req) != nil || req.WaitSeconds < 1 || req.WaitSeconds > 2 {
		t.Fatalf("request: %+v", req)
	}
}
