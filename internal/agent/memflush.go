package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// flushPoll is how often a bounded flush wait looks again whether central acknowledged everything.
const flushPoll = 200 * time.Millisecond

// flushSpaceMemory checkpoints every instance of the space's role memory and offers each collection to central at once.
// It returns the number of collections of the space on this node.
func (a *Agent) flushSpaceMemory(ctx context.Context, spaceID, reason string) int {
	m := a.mem
	if m.git == nil {
		return 0
	}
	n := 0
	for _, k := range m.collections() {
		if k[0] != spaceID {
			continue
		}
		n++
		insts, _ := os.ReadDir(filepath.Join(m.root, spaceID, k[1], "instances"))
		for _, i := range insts {
			if validMemoryKey(spaceID, k[1], i.Name()) {
				_ = m.checkpointErr(ctx, spaceID, k[1], i.Name(), reason)
			}
		}
		m.scheduleSync(spaceID, k[1], 0)
	}
	return n
}

// memoryClean reports whether every collection of the space is checkpointed and acknowledged by central, and if not,
// why (a few words).
func (a *Agent) memoryClean(ctx context.Context, spaceID string) (bool, string) {
	cols := a.spaceMemory(ctx, spaceID)
	if ctx.Err() != nil {
		// Git does not run under an ended context, so the status read above means nothing.
		return false, "stopped before the memory could be checked"
	}
	for _, st := range cols {
		switch {
		case st.Uncheckpointed > 0:
			return false, fmt.Sprintf("role %s: %d uncheckpointed file(s)", st.Role, st.Uncheckpointed)
		case st.LocalCommits > 0 || st.Pending:
			msg := fmt.Sprintf("role %s: %d commit(s) not acknowledged by central", st.Role, st.LocalCommits)
			if st.LastError != "" {
				msg += " (" + truncateStr(st.LastError, 120) + ")"
			}
			return false, msg
		}
	}
	return true, ""
}

// memoryFlush answers central's memory.flush (protocol minor 5, ADR-0045): the requested spaces' role memory is
// checkpointed and synced, and the node waits (bounded) for central's acknowledgements before it reports each space.
func (a *Agent) memoryFlush(ctx context.Context, req proto.MemoryFlush) {
	out, ok := a.flushMemoryFor(ctx, req)
	if !ok {
		return // the connection ended; central counts these spaces as not flushed
	}
	if err := a.send(ctx, proto.TypeMemoryFlushed, out); err != nil {
		a.log.Warn("send memory flush result", "error", err.Error())
	}
}

// flushMemoryFor does the flush of a memory.flush request and returns its answer; false when ctx ended first.
func (a *Agent) flushMemoryFor(ctx context.Context, req proto.MemoryFlush) (proto.MemoryFlushed, bool) {
	wait := time.Duration(req.WaitSeconds) * time.Second
	if wait <= 0 || req.WaitSeconds > proto.MemoryFlushMaxWait {
		wait = time.Minute
	}
	spaces := req.SpaceIDs
	if len(spaces) > proto.MemoryFlushMaxSpaces {
		spaces = spaces[:proto.MemoryFlushMaxSpaces]
	}
	out := proto.MemoryFlushed{FlushID: req.FlushID, Spaces: make([]proto.MemoryFlushSpace, 0, len(spaces))}
	valid := make([]string, 0, len(spaces))
	cols := map[string]int{}
	for _, id := range spaces {
		if !safeName(strings.ReplaceAll(id, "_", "-")) {
			out.Spaces = append(out.Spaces, proto.MemoryFlushSpace{SpaceID: truncateStr(id, 64), Detail: "invalid space ID"})
			continue
		}
		valid = append(valid, id)
		cols[id] = a.flushSpaceMemory(ctx, id, "backup")
	}
	deadline := time.Now().Add(wait)
	pending := map[string]string{}
	for {
		clear(pending)
		for _, id := range valid {
			if ok, why := a.memoryClean(ctx, id); !ok {
				pending[id] = why
			}
		}
		if ctx.Err() != nil {
			return out, false
		}
		if len(pending) == 0 || time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return out, false
		case <-time.After(flushPoll):
		}
	}
	for _, id := range valid {
		s := proto.MemoryFlushSpace{SpaceID: id, Flushed: true, Collections: cols[id]}
		if why, ok := pending[id]; ok {
			s.Flushed, s.Detail = false, "not acknowledged within "+wait.String()+": "+why
		}
		out.Spaces = append(out.Spaces, s)
	}
	return out, true
}
