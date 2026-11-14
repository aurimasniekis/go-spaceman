package fleet

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// blockingMemory is a MemoryStore whose Apply waits for release (a git write still running in the repository).
type blockingMemory struct {
	started, release chan struct{}
	done             atomic.Bool
}

func (m *blockingMemory) Apply(context.Context, string, proto.MemorySync) proto.MemoryAck {
	close(m.started)
	<-m.release
	m.done.Store(true)
	return proto.MemoryAck{Result: proto.MemoryError}
}

func (*blockingMemory) Report(context.Context, string, proto.MemoryStatus) {}

// Central's shutdown waits for the work a node connection started: a memory sync still writing into its repository
// when the connection closes finishes before Shutdown returns (and so before Close closes the store and the data
// directory can be removed).
func TestHubShutdownWaitsForConnectionWork(t *testing.T) {
	e := newStaleEnv(t)
	h := e.svc.hub
	mem := &blockingMemory{started: make(chan struct{}), release: make(chan struct{})}
	h.Memory = mem
	s := &session{nodeID: "nod_1", out: make(chan []byte, 4), done: make(chan struct{}), memory: make(chan struct{}, 1)}
	b, err := proto.Encode("msg_1", proto.TypeMemorySync, e.t0, proto.MemorySync{SyncID: "syn_1", SpaceID: "spc_a", Role: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	env, err := proto.Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	// The connection (ServeHTTP's count) hands the sync to its own goroutine and ends.
	h.work.Add(1)
	if err := h.handle(e.ctx, s, env); err != nil {
		t.Fatal(err)
	}
	h.work.Done()
	<-mem.started

	shut := make(chan struct{})
	go func() { h.Shutdown("test", time.Minute); close(shut) }()
	select {
	case <-shut:
		t.Fatal("Shutdown returned while a memory sync was still running")
	case <-time.After(100 * time.Millisecond):
	}
	close(mem.release)
	<-shut
	if !mem.done.Load() {
		t.Fatal("Shutdown returned before the memory sync finished")
	}
	h.mu.Lock()
	stopped := h.stopped
	h.mu.Unlock()
	if !stopped {
		t.Fatal("Shutdown left the hub accepting connections")
	}
}
