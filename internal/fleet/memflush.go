package fleet

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/obs"
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
//
// The whole call is bounded by wait plus the grace, from its start: the reads, the requests and the answers. A request
// is queued on each node's connection concurrently and gives up at the deadline, so a busy node whose outbound queue
// stays full is reported as timed out and never holds up the other nodes or the caller. progress, when not nil, is
// called once per node as its outcome is known (it answered, or it was given up on). Each node's outcome is logged.
func (svc *Service) FlushMemory(ctx context.Context, wait time.Duration, progress func()) ([]MemoryFlushResult, error) {
	if wait < time.Second {
		wait = time.Minute
	}
	wait = min(wait, proto.MemoryFlushMaxWait*time.Second)
	// A caller's deadline shorter than the wait bounds it too: nodes are not told to wait longer than the call can, and
	// the grace for their answer to arrive fits inside the deadline as well (a node using its whole wait would
	// otherwise answer after it, and its results would be lost as timed out).
	var remaining time.Duration
	hasDeadline := false
	if dl, ok := ctx.Deadline(); ok {
		hasDeadline = true
		remaining = time.Until(dl)
		wait = max(min(wait, remaining-flushGrace(remaining)), time.Second)
	}
	bound := wait + flushGrace(wait)
	if hasDeadline {
		bound = min(bound, max(remaining, time.Second))
	}
	dctx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()
	if progress == nil {
		progress = func() {}
	}
	log := obs.Component(svc.opts.Log, "fleet")
	h := svc.hub
	q := svc.s.R()
	spaces, err := q.ListSpaces(dctx)
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
		cols, err := q.ListMemoryCollections(dctx, sp.ID)
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
	nodeLog := func(nodeID, name string, sps []spaceInfo, outcome, detail string) {
		attrs := []any{slog.String(obs.FieldNodeID, nodeID), slog.String("node.name", name), slog.Int("spaces", len(sps)),
			slog.String("outcome", outcome)}
		if detail != "" {
			attrs = append(attrs, slog.String("detail", detail))
		}
		if outcome == "flushed" {
			log.Info("memory flush before a backup: node answered", attrs...)
		} else {
			log.Warn("memory flush before a backup: node not flushed", attrs...)
		}
	}
	type waiter struct {
		node, name, id string
		spaces         []spaceInfo
		p              *pendingFlush
		queued         chan struct{} // closed once the request is on the node's outbound queue
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
		if n, err := q.GetNode(dctx, nodeID); err == nil {
			name = n.Name
		}
		s := h.session(nodeID)
		switch {
		case s == nil:
			stale(nodeID, name, sps, FlushNodeDisconnected, "")
			nodeLog(nodeID, name, sps, FlushNodeDisconnected, "")
			continue
		case s.minor < proto.MinorMemoryFlush:
			stale(nodeID, name, sps, FlushProtocolTooOld, "")
			nodeLog(nodeID, name, sps, FlushProtocolTooOld, "")
			continue
		}
		w := waiter{node: nodeID, name: name, id: ids.New("mfl"), spaces: sps, p: &pendingFlush{node: nodeID, ch: make(chan proto.MemoryFlushed, 1)},
			queued: make(chan struct{})}
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
		go func() {
			err := s.sendCtx(dctx, proto.TypeMemoryFlush, svc.now(), req)
			switch {
			case err == nil:
				close(w.queued)
			case dctx.Err() == nil:
				select {
				case w.p.ch <- proto.MemoryFlushed{FlushID: "send-failed"}:
				default:
				}
			}
		}()
	}
	// await returns a node's answer, or false once the deadline passed without one. An answer already here counts even
	// when the deadline passed while waiting for an earlier node.
	await := func(ch chan proto.MemoryFlushed) (proto.MemoryFlushed, bool) {
		select {
		case res := <-ch:
			return res, true
		default:
		}
		select {
		case res := <-ch:
			return res, true
		case <-dctx.Done():
			return proto.MemoryFlushed{}, false
		}
	}
	for _, w := range waiters {
		res, ok := await(w.p.ch)
		if !ok {
			detail := fmt.Sprintf("no answer within %s", bound)
			select {
			case <-w.queued:
			default:
				detail = "the request could not be queued in time: the node's outbound queue is full"
			}
			if errors.Is(ctx.Err(), context.Canceled) {
				detail = "the backup was stopped"
			}
			stale(w.node, w.name, w.spaces, FlushTimedOut, detail)
			nodeLog(w.node, w.name, w.spaces, FlushTimedOut, detail)
			progress()
			continue
		}
		progress()
		if res.FlushID != w.id {
			stale(w.node, w.name, w.spaces, FlushNodeDisconnected, "the request could not be sent")
			nodeLog(w.node, w.name, w.spaces, FlushNodeDisconnected, "the request could not be sent")
			continue
		}
		got := map[string]proto.MemoryFlushSpace{}
		for _, r := range res.Spaces {
			got[r.SpaceID] = r
		}
		notFlushed := 0
		for _, sp := range w.spaces {
			r, ok := got[sp.id]
			switch {
			case !ok:
				if sp.known {
					notFlushed++
					out = append(out, MemoryFlushResult{SpaceID: sp.id, SpaceSlug: sp.slug, NodeID: w.node, NodeName: w.name, Reason: FlushTimedOut,
						Detail: "the node did not report this space"})
				}
			case r.Flushed:
				if sp.known || r.Collections > 0 {
					out = append(out, MemoryFlushResult{SpaceID: sp.id, SpaceSlug: sp.slug, NodeID: w.node, NodeName: w.name, Flushed: true})
				}
			default:
				notFlushed++
				out = append(out, MemoryFlushResult{SpaceID: sp.id, SpaceSlug: sp.slug, NodeID: w.node, NodeName: w.name, Reason: FlushNotAcknowledged,
					Detail: truncate(r.Detail, 300)})
			}
		}
		if notFlushed > 0 {
			nodeLog(w.node, w.name, w.spaces, "partly_flushed", fmt.Sprintf("%d space(s) not flushed", notFlushed))
		} else {
			nodeLog(w.node, w.name, w.spaces, "flushed", "")
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
