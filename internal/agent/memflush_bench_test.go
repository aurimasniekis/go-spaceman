package agent

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// TestMemoryFlushMeasure measures the flush of a large node (ADR-0045): N clean instance checkouts in one collection, cold
// (an empty fingerprint cache, as after an agent restart) and warm. It runs only when SPACEMAN_MEMFLUSH_MEASURE names
// the instance counts (for example "200,1000,2000"), so it stays out of the default test run. Temp dirs only.
func TestMemoryFlushMeasure(t *testing.T) {
	spec := os.Getenv("SPACEMAN_MEMFLUSH_MEASURE")
	if spec == "" {
		t.Skip("set SPACEMAN_MEMFLUSH_MEASURE=200,1000,2000 to measure")
	}
	for _, s := range strings.Split(spec, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil || n < 1 {
			t.Fatalf("bad count %q", s)
		}
		t.Run("N"+s, func(t *testing.T) { measureFlush(t, n) })
	}
}

func measureFlush(t *testing.T, n int) {
	a := memAgent(t)
	m := a.mem
	ctx := context.Background()
	oldRacy := statusRacy
	statusRacy = 0
	t.Cleanup(func() { statusRacy = oldRacy })
	a.mu.Lock()
	a.connected, a.minor = true, proto.MaxMinor
	a.mu.Unlock()
	cols := statusFleet(t, m, 1, n)
	stopSyncs(m)
	k := cols[0]
	st, err := a.j.memory(ctx, k[0], k[1])
	if err != nil {
		t.Fatal(err)
	}
	st.Acked = heads(ctx, m, cols)[0]
	if err := a.j.putMemory(ctx, st); err != nil {
		t.Fatal(err)
	}
	count := countGit(t, m)
	timed := func(name string, f func()) {
		c0, t0 := count(), time.Now()
		f()
		t.Logf("MEASURE n=%d %-28s %8.2fs %6d git execs", n, name, time.Since(t0).Seconds(), count()-c0)
	}
	// Git refreshes the index of a fresh copy on its first status, so the first pass is the cold one and a second
	// pass with the cache emptied again is the cold pass of a restarted agent.
	timed("statusOf cold (fresh copies)", func() { m.statusOf(ctx, k[0], k[1]) })
	m.statusMu.Lock()
	clear(m.instStatus)
	m.statusMu.Unlock()
	timed("statusOf cold (restart)", func() { m.statusOf(ctx, k[0], k[1]) })
	timed("statusOf warm", func() { m.statusOf(ctx, k[0], k[1]) })
	m.statusMu.Lock()
	clear(m.instStatus)
	m.statusMu.Unlock()
	timed("flushMemoryFor cold", func() {
		_, _ = a.flushMemoryFor(ctx, proto.MemoryFlush{FlushID: "f", SpaceIDs: []string{k[0]}, WaitSeconds: 600})
	})
	stopSyncs(m)
	timed("flushMemoryFor warm", func() {
		_, _ = a.flushMemoryFor(ctx, proto.MemoryFlush{FlushID: "f", SpaceIDs: []string{k[0]}, WaitSeconds: 600})
	})
	stopSyncs(m)
}
