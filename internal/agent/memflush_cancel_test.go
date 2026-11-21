package agent

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// lockedBuffer is a bytes.Buffer safe for concurrent log writes.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// SM-MEM-005: a memory flush stops quietly when its context ends (the connection or the agent stopped): overlapping
// flushes of a space take turns instead of each walking every instance, no checkpoint starts after the cancellation,
// and none is logged as failed. Instances with nothing to checkpoint run no checkpoint at all.
func TestMemoryFlushStopsQuietlyOnCancel(t *testing.T) {
	a := memAgent(t)
	m := a.mem
	logs := &lockedBuffer{}
	a.log = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ctx := context.Background()
	const n, flushes = 40, 8
	cols := statusFleet(t, m, 1, n)
	stopSyncs(m)
	for i := range n {
		write(t, m.instance(cols[0][0], cols[0][1], fmt.Sprintf("ins_%d", i)), "note.md", fmt.Sprintf("note %d\n", i))
	}
	count := countGit(t, m)

	// Already cancelled: nothing runs and nothing is logged.
	done, cancel0 := context.WithCancel(ctx)
	cancel0()
	for range flushes {
		a.flushSpaceMemory(done, cols[0][0], "backup")
	}
	if c := count(); c != 0 {
		t.Fatalf("a cancelled flush ran %d git processes", c)
	}

	// Cancelled while overlapping flushes run (a shutdown during backups): the rest stop at once.
	cctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	for range flushes {
		wg.Go(func() { a.flushSpaceMemory(cctx, cols[0][0], "backup") })
	}
	deadline := time.Now().Add(2 * time.Minute)
	for count() < 30 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	atCancel := count()
	wg.Wait()
	after := count() - atCancel
	warns := strings.Count(logs.String(), "memory checkpoint failed")
	t.Logf("instances=%d flushes=%d: execs before cancel=%d, after cancel=%d, checkpoint warnings=%d", n, flushes, atCancel, after, warns)
	if warns != 0 {
		t.Fatalf("%d checkpoint warnings after a cancellation:\n%s", warns, logs.String())
	}
	// At most the git calls running at the cancellation finish: one per status worker, plus the guard's in-flight call.
	if after > statusWorkers+2 {
		t.Fatalf("%d git processes after the cancellation", after)
	}
	if js, _ := a.j.memory(ctx, cols[0][0], cols[0][1]); strings.Contains(js.LastError, "canceled") {
		t.Fatalf("a cancellation was recorded as an error: %q", js.LastError)
	}

	// Flushed again: every instance with a note is checkpointed once; a second flush finds nothing to checkpoint and
	// runs git only for the collection, not per instance.
	a.flushSpaceMemory(ctx, cols[0][0], "backup")
	if st := m.statusOf(ctx, cols[0][0], cols[0][1]); st.Uncheckpointed != 0 {
		t.Fatalf("after the flush: %+v", st)
	}
	old := statusRacy
	statusRacy = 0
	t.Cleanup(func() { statusRacy = old })
	for i := 0; i < 10; i++ { // settles the cache after the checkpoints rewrote the indexes
		before := count()
		m.statusOf(ctx, cols[0][0], cols[0][1])
		if count()-before <= 2 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	c := count()
	a.flushSpaceMemory(ctx, cols[0][0], "backup")
	if again := count() - c; again > 4 {
		t.Fatalf("a flush with nothing to checkpoint ran %d git processes for %d instances", again, n)
	}
}
