package fleet

import (
	"context"
	"sort"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// Reasons a space's role memory was not flushed before a backup (ADR-0045).
const (
	FlushNodeDisconnected = "node_disconnected" // the space's node is not connected
	FlushProtocolTooOld   = "protocol_too_old"  // the node's agent predates protocol minor 5 (no memory.flush)
	FlushTimedOut         = "timed_out"         // the node did not answer within the wait
	FlushNotAcknowledged  = "not_acknowledged"  // the node answered, but central had not acknowledged all of the memory
)

// flushGrace is how long central waits beyond the node's own bounded wait for its answer: a quarter of the wait,
// between 1 and 10 seconds.
func flushGrace(wait time.Duration) time.Duration {
	return min(max(wait/4, time.Second), 10*time.Second)
}

// MemoryFlushResult is one space's outcome of FlushMemory.
type MemoryFlushResult struct {
	SpaceID   string `json:"space_id"`
	SpaceSlug string `json:"space_slug"`
	NodeID    string `json:"node_id"`
	NodeName  string `json:"node_name,omitempty"`
	Flushed   bool   `json:"flushed"`
	Reason    string `json:"reason,omitempty" enum:",node_disconnected,protocol_too_old,timed_out,not_acknowledged"`
	Detail    string `json:"detail,omitempty"`
}

type pendingFlush struct {
	node string
	ch   chan proto.MemoryFlushed
}

// flushed delivers a node's memory.flushed to the waiting FlushMemory. An answer for a flush that is not waiting, or
// from another node than the one asked, is ignored.
func (h *Hub) flushed(nodeID string, res proto.MemoryFlushed) {
	h.flushMu.Lock()
	p := h.flushes[res.FlushID]
	if p != nil && p.node == nodeID {
		delete(h.flushes, res.FlushID)
	} else {
		p = nil
	}
	h.flushMu.Unlock()
	if p != nil {
		select {
		case p.ch <- res:
		default:
		}
	}
}

func (h *Hub) session(nodeID string) *session {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sessions[nodeID]
}

// FlushMemory asks every connected node to checkpoint and sync the role memory of its spaces now, and waits for the
// answers at most wait (plus a short grace): the backup preparation of SM-MEM-005. Archived spaces are skipped. It
// returns one result per space with role memory, sorted by slug: flushed, or not with the reason (the node is not
// connected, its agent is too old, it did not answer in time, or central had not acknowledged everything). It runs
// outside any transaction and never fails the caller: a storage error returns what is known so far.
func (svc *Service) FlushMemory(ctx context.Context, wait time.Duration) ([]MemoryFlushResult, error) {
	if wait < time.Second {
		wait = time.Minute
	}
	wait = min(wait, proto.MemoryFlushMaxWait*time.Second)
	h := svc.hub
	q := svc.s.R()
	spaces, err := q.ListSpaces(ctx)
	if err != nil {
		return nil, err
	}
	type spaceInfo struct {
		id, slug string
		known    bool // central holds role memory for it
	}
	byNode := map[string][]spaceInfo{}
	for _, sp := range spaces {
		if sp.State == "archived" {
			continue
		}
		cols, err := q.ListMemoryCollections(ctx, sp.ID)
		if err != nil {
			return nil, err
		}
		byNode[sp.NodeID] = append(byNode[sp.NodeID], spaceInfo{id: sp.ID, slug: sp.Slug, known: len(cols) > 0})
	}
	out := []MemoryFlushResult{}
	stale := func(nodeID, name string, sps []spaceInfo, reason, detail string) {
		for _, sp := range sps {
			if sp.known {
				out = append(out, MemoryFlushResult{SpaceID: sp.id, SpaceSlug: sp.slug, NodeID: nodeID, NodeName: name, Reason: reason, Detail: detail})
			}
		}
	}
	type waiter struct {
		node, name, id string
		spaces         []spaceInfo
		p              *pendingFlush
	}
	var waiters []waiter
	defer func() {
		h.flushMu.Lock()
		for _, w := range waiters {
			delete(h.flushes, w.id)
		}
		h.flushMu.Unlock()
	}()
	for nodeID, sps := range byNode {
		name := ""
		if n, err := q.GetNode(ctx, nodeID); err == nil {
			name = n.Name
		}
		s := h.session(nodeID)
		switch {
		case s == nil:
			stale(nodeID, name, sps, FlushNodeDisconnected, "")
			continue
		case s.minor < proto.MinorMemoryFlush:
			stale(nodeID, name, sps, FlushProtocolTooOld, "")
			continue
		}
		w := waiter{node: nodeID, name: name, id: ids.New("mfl"), spaces: sps, p: &pendingFlush{node: nodeID, ch: make(chan proto.MemoryFlushed, 1)}}
		req := proto.MemoryFlush{FlushID: w.id, WaitSeconds: int(wait / time.Second)}
		for _, sp := range sps {
			req.SpaceIDs = append(req.SpaceIDs, sp.id)
		}
		h.flushMu.Lock()
		if h.flushes == nil {
			h.flushes = map[string]*pendingFlush{}
		}
		h.flushes[w.id] = w.p
		h.flushMu.Unlock()
		waiters = append(waiters, w)
		if err := s.send(proto.TypeMemoryFlush, svc.now(), req); err != nil {
			w.p.ch <- proto.MemoryFlushed{FlushID: "send-failed"}
		}
	}
	deadline := time.NewTimer(wait + flushGrace(wait))
	defer deadline.Stop()
	for _, w := range waiters {
		var res proto.MemoryFlushed
		select {
		case res = <-w.p.ch:
		case <-deadline.C:
			stale(w.node, w.name, w.spaces, FlushTimedOut, "")
			continue
		case <-ctx.Done():
			stale(w.node, w.name, w.spaces, FlushTimedOut, "the backup was stopped")
			continue
		}
		if res.FlushID != w.id {
			stale(w.node, w.name, w.spaces, FlushNodeDisconnected, "the request could not be sent")
			continue
		}
		got := map[string]proto.MemoryFlushSpace{}
		for _, r := range res.Spaces {
			got[r.SpaceID] = r
		}
		for _, sp := range w.spaces {
			r, ok := got[sp.id]
			switch {
			case !ok:
				if sp.known {
					out = append(out, MemoryFlushResult{SpaceID: sp.id, SpaceSlug: sp.slug, NodeID: w.node, NodeName: w.name, Reason: FlushTimedOut,
						Detail: "the node did not report this space"})
				}
			case r.Flushed:
				if sp.known || r.Collections > 0 {
					out = append(out, MemoryFlushResult{SpaceID: sp.id, SpaceSlug: sp.slug, NodeID: w.node, NodeName: w.name, Flushed: true})
				}
			default:
				out = append(out, MemoryFlushResult{SpaceID: sp.id, SpaceSlug: sp.slug, NodeID: w.node, NodeName: w.name, Reason: FlushNotAcknowledged,
					Detail: truncate(r.Detail, 300)})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].SpaceSlug != out[j].SpaceSlug {
			return out[i].SpaceSlug < out[j].SpaceSlug
		}
		return out[i].SpaceID < out[j].SpaceID
	})
	return out, nil
}
