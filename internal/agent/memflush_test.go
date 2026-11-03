package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// SM-MEM-005: memory.flush checkpoints every instance of the requested spaces at once and reports a space as flushed
// only when central acknowledged all of its history, after a bounded wait; spaces without memory are trivially
// flushed and invalid IDs are refused.
func TestMemoryFlushForBackup(t *testing.T) {
	a := memAgent(t)
	ctx := context.Background()
	m := a.mem
	const sp, role = "spc_flush", "implementor-backend"
	inst, err := m.prepare(ctx, sp, role, "ins_one")
	if err != nil {
		t.Fatal(err)
	}
	write(t, inst, "payouts.md", "- reconcile first\n")
	if st := m.statusOf(ctx, sp, role); st.Uncheckpointed != 1 {
		t.Fatalf("before the flush: %+v", st)
	}

	// Not connected: the flush checkpoints, but central cannot acknowledge within the wait.
	start := time.Now()
	out, ok := a.flushMemoryFor(ctx, proto.MemoryFlush{FlushID: "mfl_1", SpaceIDs: []string{sp, "spc_none", "../etc"}, WaitSeconds: 1})
	if !ok || out.FlushID != "mfl_1" || len(out.Spaces) != 3 {
		t.Fatalf("flush: %+v %v", out, ok)
	}
	if d := time.Since(start); d < time.Second || d > 5*time.Second {
		t.Fatalf("the wait was not bounded by wait_seconds: %s", d)
	}
	got := map[string]proto.MemoryFlushSpace{}
	for _, s := range out.Spaces {
		got[s.SpaceID] = s
	}
	if s := got[sp]; s.Flushed || s.Collections != 1 || !strings.Contains(s.Detail, "not acknowledged by central") {
		t.Fatalf("unacknowledged space: %+v", s)
	}
	if s := got["spc_none"]; !s.Flushed || s.Collections != 0 {
		t.Fatalf("space without memory: %+v", s)
	}
	if s := got["../etc"]; s.Flushed || s.Detail != "invalid space ID" {
		t.Fatalf("invalid space: %+v", s)
	}
	st := m.statusOf(ctx, sp, role)
	if st.Uncheckpointed != 0 || !st.Pending {
		t.Fatalf("the flush did not checkpoint: %+v", st)
	}

	// Central acknowledges the head: the next flush reports the space flushed at once.
	js, _ := a.j.memory(ctx, sp, role)
	js.Acked = st.Head
	if err := a.j.putMemory(ctx, js); err != nil {
		t.Fatal(err)
	}
	start = time.Now()
	out, ok = a.flushMemoryFor(ctx, proto.MemoryFlush{FlushID: "mfl_2", SpaceIDs: []string{sp}, WaitSeconds: 30})
	if !ok || len(out.Spaces) != 1 || !out.Spaces[0].Flushed || out.Spaces[0].Detail != "" {
		t.Fatalf("acknowledged space: %+v", out)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("a clean space waited %s", d)
	}

	// A cancelled flush (the connection ended) answers nothing.
	write(t, inst, "more.md", "x\n")
	cctx, cancel := context.WithCancel(ctx)
	time.AfterFunc(300*time.Millisecond, cancel)
	if _, ok := a.flushMemoryFor(cctx, proto.MemoryFlush{FlushID: "mfl_3", SpaceIDs: []string{sp}, WaitSeconds: 30}); ok {
		t.Fatal("a cancelled flush answered")
	}
}
