package app_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store"
)

// rawNode is a scripted node connection speaking a chosen protocol range (no agent behind it).
type rawNode struct {
	conn *websocket.Conn
	ch   chan proto.Envelope
}

func dialRawNode(t *testing.T, e *fleetEnv, maxMinor int) *rawNode {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	u := "ws" + strings.TrimPrefix(e.srv.URL, "http") + proto.ConnectPath
	conn, _, err := websocket.Dial(ctx, u, &websocket.DialOptions{Subprotocols: []string{proto.Subprotocol},
		HTTPHeader: http.Header{"Authorization": {"Bearer " + e.cred}}})
	if err != nil {
		t.Fatal(err)
	}
	conn.SetReadLimit(proto.MaxMessage)
	t.Cleanup(func() { _ = conn.CloseNow() })
	n := &rawNode{conn: conn, ch: make(chan proto.Envelope, 64)}
	n.send(t, proto.TypeHello, proto.Hello{AgentVersion: "test", MinMinor: proto.MinMinor, MaxMinor: maxMinor, Runs: []proto.RunReport{}, Leases: []proto.LeaseState{}})
	go func() {
		for {
			_, b, err := conn.Read(ctx)
			if err != nil {
				close(n.ch)
				return
			}
			if env, err := proto.Decode(b); err == nil {
				n.ch <- env
			}
		}
	}()
	return n
}

func (n *rawNode) send(t *testing.T, typ string, body any) {
	t.Helper()
	b, err := proto.Encode("msg_test"+typ, typ, time.Now(), body)
	if err != nil {
		t.Fatal(err)
	}
	if err := n.conn.Write(context.Background(), websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
}

// next returns the next message of type typ (others are skipped), or fails after a timeout.
func (n *rawNode) next(t *testing.T, typ string) proto.Envelope {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case env, ok := <-n.ch:
			if !ok {
				t.Fatalf("connection closed waiting for %s", typ)
			}
			if env.Type == typ {
				return env
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s", typ)
		}
	}
}

// SM-MEM-005 (simulated: scripted node connections): FlushMemory asks only nodes that negotiated protocol minor 5 and
// waits for them at most the given bound. A node on minor 3 is reported as too old without being asked, a node that
// does not answer in time as timed out, an answer naming another flush is ignored, and a space the node reports as not
// acknowledged keeps the node's reason. Spaces without role memory are left out; archived spaces are not asked about.
func TestFlushMemoryBoundsAndReasons(t *testing.T) {
	e := newFleetEnv(t)
	e.enroll()
	ctx := context.Background()
	nodes, _ := e.c.Fleet.ListNodes(ctx)
	nodeID := nodes[0].ID
	mk := func(slug string, memory bool) string {
		sp, err := e.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: slug, Name: slug, NodeID: nodeID})
		if err != nil {
			t.Fatal(err)
		}
		if memory {
			if err := e.c.Store.W().TxNamed(ctx, "test.memory", func(tx *store.Tx) error {
				_, err := tx.SQL.ExecContext(ctx, `INSERT INTO memory_collections (id, space_id, role, created_at, updated_at) VALUES (?, ?, 'implementor', 0, 0)`,
					"mco_"+slug, sp.ID)
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}
		return sp.ID
	}
	withMem, noMem := mk("withmem", true), mk("nomem", false)

	// Not connected.
	res, err := e.c.Fleet.FlushMemory(ctx, time.Second)
	if err != nil || len(res) != 1 || res[0].SpaceID != withMem || res[0].Flushed || res[0].Reason != fleet.FlushNodeDisconnected || res[0].NodeName != "node-a" {
		t.Fatalf("disconnected: %+v %v", res, err)
	}

	// An agent on minor 3 is never sent memory.flush.
	old := dialRawNode(t, e, 3)
	old.next(t, proto.TypeWelcome)
	eventually(t, "old node connected", func() bool { n, _ := e.c.Fleet.GetNode(ctx, nodeID); return n.Connected })
	res, err = e.c.Fleet.FlushMemory(ctx, time.Second)
	if err != nil || len(res) != 1 || res[0].Reason != fleet.FlushProtocolTooOld {
		t.Fatalf("minor 3: %+v %v", res, err)
	}
	_ = old.conn.Close(websocket.StatusNormalClosure, "")
	eventually(t, "old node gone", func() bool { n, _ := e.c.Fleet.GetNode(ctx, nodeID); return !n.Connected })

	// A minor-4 node that never answers: the call returns within the bound (wait plus grace).
	n := dialRawNode(t, e, proto.MinorMemoryFlush)
	n.next(t, proto.TypeWelcome)
	eventually(t, "node connected", func() bool { x, _ := e.c.Fleet.GetNode(ctx, nodeID); return x.Connected })
	start := time.Now()
	res, err = e.c.Fleet.FlushMemory(ctx, time.Second)
	if d := time.Since(start); d > 4*time.Second {
		t.Fatalf("the flush waited %s", d)
	}
	if err != nil || len(res) != 1 || res[0].Reason != fleet.FlushTimedOut {
		t.Fatalf("no answer: %+v %v", res, err)
	}
	var req proto.MemoryFlush
	if err := n.next(t, proto.TypeMemoryFlush).Decode(&req); err != nil || req.WaitSeconds != 1 || len(req.SpaceIDs) != 2 {
		t.Fatalf("request: %+v %v", req, err)
	}

	// The node answers: a stale flush ID is ignored, the right one counts.
	type out struct {
		res []fleet.MemoryFlushResult
		err error
	}
	done := make(chan out, 1)
	go func() {
		r, err := e.c.Fleet.FlushMemory(ctx, 5*time.Second)
		done <- out{r, err}
	}()
	if err := n.next(t, proto.TypeMemoryFlush).Decode(&req); err != nil {
		t.Fatal(err)
	}
	n.send(t, proto.TypeMemoryFlushed, proto.MemoryFlushed{FlushID: "mfl_other", Spaces: []proto.MemoryFlushSpace{{SpaceID: withMem, Flushed: true, Collections: 1}}})
	n.send(t, proto.TypeMemoryFlushed, proto.MemoryFlushed{FlushID: req.FlushID, Spaces: []proto.MemoryFlushSpace{
		{SpaceID: withMem, Flushed: false, Collections: 1, Detail: "role implementor: 2 commit(s) not acknowledged by central"},
		{SpaceID: noMem, Flushed: true}}})
	got := <-done
	if got.err != nil || len(got.res) != 1 || got.res[0].SpaceID != withMem || got.res[0].Reason != fleet.FlushNotAcknowledged ||
		!strings.Contains(got.res[0].Detail, "2 commit(s)") {
		t.Fatalf("answered: %+v %v", got.res, got.err)
	}

	// Archived spaces are not asked about.
	sp, _ := e.c.Fleet.GetSpace(ctx, withMem)
	if _, err := e.c.Fleet.SetSpaceState(ctx, admin, withMem, sp.Version, "archived", ""); err != nil {
		t.Fatal(err)
	}
	go func() {
		r, err := e.c.Fleet.FlushMemory(ctx, 5*time.Second)
		done <- out{r, err}
	}()
	if err := n.next(t, proto.TypeMemoryFlush).Decode(&req); err != nil || len(req.SpaceIDs) != 1 || req.SpaceIDs[0] != noMem {
		t.Fatalf("request after archiving: %+v %v", req, err)
	}
	n.send(t, proto.TypeMemoryFlushed, proto.MemoryFlushed{FlushID: req.FlushID, Spaces: []proto.MemoryFlushSpace{{SpaceID: noMem, Flushed: true}}})
	if got := <-done; got.err != nil || len(got.res) != 0 {
		t.Fatalf("after archiving: %+v %v", got.res, got.err)
	}
}
