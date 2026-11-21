package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// flushPoll is how often a bounded flush wait looks again whether central acknowledged everything.
const flushPoll = 200 * time.Millisecond

// flushPollMax is the longest pause between two looks: the pause doubles from flushPoll, because every look reads the
// state of every instance of the space and a large node has over a thousand.
const flushPollMax = 2 * time.Second

// flushSpacesParallel is how many spaces one memory.flush works on at the same time.
const flushSpacesParallel = 4

// flushSpaceMemory checkpoints every instance of the space's role memory that has something to checkpoint and offers
// each collection to central at once. It returns the number of collections of the space on this node.
func (a *Agent) flushSpaceMemory(ctx context.Context, spaceID, reason string) int {
	n, _ := a.flushSpaceMemoryBy(ctx, spaceID, reason, time.Time{})
	return n
}

// flushSpaceMemoryBy is flushSpaceMemory that stops looking at instances once deadline passed (the zero time: never).
// It returns the number of collections of the space and how many instances it did not get to look at.
//
// Flushes of one space take turns: every backup sends its own memory.flush, and a flush outlives central's bounded wait,
// so overlapping flushes would otherwise each walk every instance. An instance with no uncheckpointed file and no
// unpublished commit is skipped (its counts come from the status cache, memstatus.go, so an ended instance's checkout
// costs no git once read; a cold cache reads them in parallel, bounded node-wide). The most recently used instances
// come first, so the ones with work are reached before the checkouts of long-ended runs. A checkpoint that started is
// never cut short by the deadline, only the looking at further instances is. When ctx ends (the connection or the agent
// stopped) the flush stops at once, quietly.
func (a *Agent) flushSpaceMemoryBy(ctx context.Context, spaceID, reason string, deadline time.Time) (collections, unchecked int) {
	m := a.mem
	if m.git == nil {
		return 0, 0
	}
	release, ok := m.flushTurn(ctx, spaceID)
	if !ok {
		return 0, 0
	}
	defer release()
	scan := ctx
	if !deadline.IsZero() {
		var cancel context.CancelFunc
		scan, cancel = context.WithDeadline(ctx, deadline)
		defer cancel()
	}
	for _, k := range m.collections(ctx) {
		if k[0] != spaceID {
			continue
		}
		collections++
		head := m.git.RevParse(ctx, m.shared(spaceID, k[1]), "refs/heads/main")
		names := recentFirst(filepath.Join(m.root, spaceID, k[1], "instances"), func(n string) bool { return validMemoryKey(spaceID, k[1], n) })
		var skipped atomic.Int64
		eachParallel(len(names), func(i int) {
			if scan.Err() != nil {
				skipped.Add(1)
				return
			}
			if head != "" {
				// A scan that ends while git runs reads as unclean (never cached), so the instance is checkpointed below
				// only if the scan is still running: otherwise it counts as not looked at.
				unc, local, ok := m.instanceStatus(scan, m.instance(spaceID, k[1], names[i]), head)
				if scan.Err() != nil {
					skipped.Add(1)
					return
				}
				if ok && unc == 0 && local == 0 {
					return // nothing to checkpoint
				}
			}
			_ = m.checkpointErr(ctx, spaceID, k[1], names[i], reason)
		})
		unchecked += int(skipped.Load())
		if ctx.Err() != nil {
			return collections, unchecked
		}
		m.scheduleSync(spaceID, k[1], 0)
	}
	return collections, unchecked
}

// recentFirst lists a folder's entries that keep accepts, the most recently modified first (an instance in use or just
// ended before the checkouts of runs that ended long ago).
func recentFirst(dir string, keep func(name string) bool) []string {
	ents, _ := os.ReadDir(dir)
	type ent struct {
		name string
		mod  time.Time
	}
	var es []ent
	for _, e := range ents {
		if !keep(e.Name()) {
			continue
		}
		var mod time.Time
		if fi, err := e.Info(); err == nil {
			mod = fi.ModTime()
		}
		es = append(es, ent{e.Name(), mod})
	}
	slices.SortStableFunc(es, func(x, y ent) int { return y.mod.Compare(x.mod) })
	names := make([]string, len(es))
	for i, e := range es {
		names[i] = e.name
	}
	return names
}

// flushTurn waits for the space's flush slot; false when ctx ended first.
func (m *memManager) flushTurn(ctx context.Context, spaceID string) (release func(), ok bool) {
	m.mu.Lock()
	slot := m.flushing[spaceID]
	if slot == nil {
		slot = make(chan struct{}, 1)
		m.flushing[spaceID] = slot
	}
	m.mu.Unlock()
	select {
	case slot <- struct{}{}:
		return func() { <-slot }, true
	case <-ctx.Done():
		return nil, false
	}
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
			msg := fmt.Sprintf("role %s: %d uncheckpointed file(s)", st.Role, st.Uncheckpointed)
			if st.LastError != "" {
				msg += " (" + truncateStr(st.LastError, 120) + ")"
			}
			return false, msg
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
	start := time.Now()
	deadline := start.Add(wait)
	out := proto.MemoryFlushed{FlushID: req.FlushID, Spaces: make([]proto.MemoryFlushSpace, 0, len(spaces))}
	valid := make([]string, 0, len(spaces))
	for _, id := range spaces {
		if !safeName(strings.ReplaceAll(id, "_", "-")) {
			out.Spaces = append(out.Spaces, proto.MemoryFlushSpace{SpaceID: truncateStr(id, 64), Detail: "invalid space ID"})
			continue
		}
		valid = append(valid, id)
	}
	// The spaces are flushed side by side (a slow one does not hold the rest back) and share one budget that starts when
	// the request arrived: what the node did not get to by then is named stale, never waited for beyond the budget.
	cols := make([]int, len(valid))
	unchecked := make([]int, len(valid))
	spaceSlots := make(chan struct{}, flushSpacesParallel)
	var wg sync.WaitGroup
	for i, id := range valid {
		wg.Add(1)
		spaceSlots <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-spaceSlots }()
			cols[i], unchecked[i] = a.flushSpaceMemoryBy(ctx, id, "backup", deadline)
		}()
	}
	wg.Wait()
	if ctx.Err() != nil {
		return out, false
	}
	pending := map[string]string{}
	todo := map[string]bool{}
	for i, id := range valid {
		if unchecked[i] > 0 {
			pending[id] = fmt.Sprintf("%d instance(s) not checked in time", unchecked[i])
		} else {
			todo[id] = true
		}
	}
	// Only the spaces not yet clean are looked at again, each time a little later than the last.
	for poll := flushPoll; len(todo) > 0; poll = min(poll*2, flushPollMax) {
		for id := range todo {
			if ok, why := a.memoryClean(ctx, id); ok {
				delete(todo, id)
				delete(pending, id)
			} else {
				pending[id] = why
			}
		}
		if ctx.Err() != nil {
			return out, false
		}
		left := time.Until(deadline)
		if len(todo) == 0 || left <= 0 {
			break
		}
		select {
		case <-ctx.Done():
			return out, false
		case <-time.After(min(poll, left)):
		}
	}
	for i, id := range valid {
		s := proto.MemoryFlushSpace{SpaceID: id, Flushed: true, Collections: cols[i]}
		if why, ok := pending[id]; ok {
			s.Flushed, s.Detail = false, "not acknowledged within "+wait.String()+": "+why
		}
		out.Spaces = append(out.Spaces, s)
	}
	return out, true
}
