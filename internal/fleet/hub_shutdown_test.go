package fleet

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/aurimasniekis/go-spaceman/internal/audit"
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
	h.workAdd()
	if err := h.handle(e.ctx, s, env); err != nil {
		t.Fatal(err)
	}
	h.workDone()
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

// shutdownNode serves the hub over HTTP and enrolls a node, returning the WebSocket URL and the node's credential.
func shutdownNode(t *testing.T, e *staleEnv) (string, string) {
	t.Helper()
	srv := httptest.NewServer(e.svc.hub)
	t.Cleanup(srv.Close)
	en, err := e.svc.CreateNode(e.ctx, audit.System, CreateNodeInput{Name: "node-a"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.svc.Enroll(e.ctx, en.Token, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	return "ws" + strings.TrimPrefix(srv.URL, "http") + proto.ConnectPath, res.Credential
}

func dialNode(t *testing.T, u, cred string) *websocket.Conn {
	t.Helper()
	conn, _, err := websocket.Dial(context.Background(), u, &websocket.DialOptions{Subprotocols: []string{proto.Subprotocol},
		HTTPHeader: http.Header{"Authorization": {"Bearer " + cred}}})
	if err != nil {
		t.Fatal(err)
	}
	conn.SetReadLimit(proto.MaxMessage)
	t.Cleanup(func() { _ = conn.CloseNow() })
	return conn
}

// sendHello sends the node's hello; a connection central already closed may refuse the write.
func sendHello(t *testing.T, conn *websocket.Conn) error {
	t.Helper()
	b, err := proto.Encode("msg_hello", proto.TypeHello, time.Now(), proto.Hello{AgentVersion: "test", MinMinor: proto.MinMinor,
		MaxMinor: proto.MaxMinor, Runs: []proto.RunReport{}, Leases: []proto.LeaseState{}})
	if err != nil {
		t.Fatal(err)
	}
	return conn.Write(context.Background(), websocket.MessageText, b)
}

// readClosed reads until the connection ends and fails if central welcomed the node first.
func readClosed(t *testing.T, conn *websocket.Conn) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for {
		_, b, err := conn.Read(ctx)
		if err != nil {
			return err
		}
		if env, err := proto.Decode(b); err == nil && env.Type == proto.TypeWelcome {
			t.Fatal("central welcomed a node after its shutdown began")
		}
	}
}

// Once central's shutdown has begun, a node that connects is refused at once (going away), before its hello is read.
func TestHubShutdownRefusesNewConnections(t *testing.T) {
	e := newStaleEnv(t)
	u, cred := shutdownNode(t, e)
	e.svc.hub.Shutdown("test", time.Minute)
	conn := dialNode(t, u, cred)
	_ = sendHello(t, conn)
	err := readClosed(t, conn)
	if websocket.CloseStatus(err) != websocket.StatusGoingAway {
		t.Fatalf("connection after shutdown ended with %v, want going away", err)
	}
	if n := e.count(`SELECT count(*) FROM nodes WHERE last_connect_at IS NOT NULL`); n != 0 {
		t.Fatalf("a connection after shutdown recorded the node as connected (%d)", n)
	}
}

// A connection accepted just before shutdown began, whose hello arrives after Shutdown closed the live connections, is
// not registered (nothing would close it again) and ends, so Shutdown does not wait for it.
func TestHubShutdownRefusesConnectionAcceptedBefore(t *testing.T) {
	e := newStaleEnv(t)
	h := e.svc.hub
	u, cred := shutdownNode(t, e)
	conn := dialNode(t, u, cred)
	// ServeHTTP counted the connection: it passed the check at accept.
	deadline := time.Now().Add(10 * time.Second)
	for h.working.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the connection was never counted")
		}
		time.Sleep(5 * time.Millisecond)
	}
	shut := make(chan struct{})
	go func() { h.Shutdown("test", time.Minute); close(shut) }()
	for {
		h.mu.Lock()
		stopped := h.stopped
		h.mu.Unlock()
		if stopped {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if err := sendHello(t, conn); err != nil {
		t.Fatal(err)
	}
	_ = readClosed(t, conn)
	select {
	case <-shut:
	case <-time.After(10 * time.Second):
		t.Fatal("Shutdown kept waiting for a connection that registered after it closed the live ones")
	}
}

// A memory sync still queued behind another when its connection ends is dropped instead of waiting for its turn, so
// Shutdown does not wait for it (its context never ends: the connection's work runs without cancellation).
func TestHubShutdownDropsQueuedMemorySync(t *testing.T) {
	e := newStaleEnv(t)
	h := e.svc.hub
	mem := &blockingMemory{started: make(chan struct{}), release: make(chan struct{})}
	h.Memory = mem
	s := &session{nodeID: "nod_1", out: make(chan []byte, 4), done: make(chan struct{}), memory: make(chan struct{}, 1)}
	s.memory <- struct{}{} // another sync of this node is running
	b, err := proto.Encode("msg_1", proto.TypeMemorySync, e.t0, proto.MemorySync{SyncID: "syn_1", SpaceID: "spc_a", Role: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	env, err := proto.Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	h.workAdd()
	if err := h.handle(e.ctx, s, env); err != nil {
		t.Fatal(err)
	}
	h.workDone()
	close(s.done) // the connection ended
	shut := make(chan struct{})
	go func() { h.Shutdown("test", time.Minute); close(shut) }()
	select {
	case <-shut:
	case <-time.After(10 * time.Second):
		t.Fatal("Shutdown waited for a memory sync queued behind another after its connection ended")
	}
	select {
	case <-mem.started:
		t.Fatal("the queued memory sync ran after its connection ended")
	default:
	}
}
