package agent

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/runner"
	"github.com/aurimasniekis/go-spaceman/internal/runner/runnertest"
)

// A flush's checkpoint does not fast-forward the idle instances: that takes the collection lock (an unbounded wait) and
// runs one git per idle instance, which a flush under a deadline cannot afford. A checkpoint without a deadline does.
func TestFlushCheckpointSkipsFastForwardIdle(t *testing.T) {
	e := newFFEnv(t)
	f := runnertest.New()
	e.a.o.Runner = f
	e.a.leases[ffSpace] = leaseRow{SpaceID: ffSpace, Generation: 1, ExpiresAt: time.Now().Add(time.Hour), Workdir: "ff"}
	e.checkout("ins_a")
	other := e.checkout("ins_b")
	r := e.idleRun("ins_a")
	p, err := f.Start(e.ctx, runner.Spec{Name: r.TerminalName, Argv: []string{"x"}})
	if err != nil {
		t.Fatal(err)
	}
	f.SetStatus(p.ID, "idle")
	r.TerminalID = p.ID
	if err := e.a.j.updateRun(e.ctx, r); err != nil {
		t.Fatal(err)
	}
	stopSyncs(e.m)

	var mu sync.Mutex
	reached := 0
	release := make(chan struct{})
	e.m.ffHook = func(string) {
		mu.Lock()
		reached++
		mu.Unlock()
		<-release // a fast-forward that never finishes
	}
	defer close(release)
	write(t, other, "flush.md", "flushed\n")
	errc := make(chan error, 1)
	go func() {
		errc <- e.m.checkpointBy(e.ctx, ffSpace, ffRole, "ins_b", "backup", time.Now().Add(time.Minute))
	}()
	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("checkpoint: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("a flush checkpoint waited for the fast-forward of the idle instances")
	}
	mu.Lock()
	defer mu.Unlock()
	if reached != 0 {
		t.Fatalf("a flush checkpoint fast-forwarded %d idle instances", reached)
	}
}

// instanceStatus never reads a checkout as clean when its context ended before git ran (waiting for another reader
// or for a git slot), and caches nothing then.
func TestInstanceStatusCtxEndFailsClosed(t *testing.T) {
	a := memAgent(t)
	m := a.mem
	cols := statusFleet(t, m, 1, 2)
	stopSyncs(m)
	inst := m.instance(cols[0][0], cols[0][1], "ins_1")
	head := heads(context.Background(), m, cols)[0]
	if head == "" {
		t.Fatal("no shared head")
	}
	check := func(what string) {
		t.Helper()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		u, l, ok := m.instanceStatus(ctx, inst, head)
		if !ok || u < 1 || l < 1 {
			t.Fatalf("%s: read as (%d, %d, %v), want unclean", what, u, l, ok)
		}
		m.statusMu.Lock()
		_, cached := m.instStatus[inst]
		m.statusMu.Unlock()
		if cached {
			t.Fatalf("%s: the unread status was cached", what)
		}
	}

	// Waiting for another reader of the same checkout.
	busy := make(chan struct{})
	m.statusMu.Lock()
	m.statusBusy[inst] = busy
	m.statusMu.Unlock()
	check("busy reader")
	m.statusMu.Lock()
	delete(m.statusBusy, inst)
	m.statusMu.Unlock()

	// Waiting for a git slot.
	for range cap(m.statusSlots) {
		m.statusSlots <- struct{}{}
	}
	check("no slot")
	for range cap(m.statusSlots) {
		<-m.statusSlots
	}

	// With a live context the checkout reads clean (and is then cached).
	if u, l, ok := m.instanceStatus(context.Background(), inst, head); !ok || u != 0 || l != 0 {
		t.Fatalf("clean checkout read as (%d, %d, %v)", u, l, ok)
	}
}

// The git readers of the whole node stay within statusWorkers, also when several status passes run at once.
func TestStatusSlotsBoundConcurrentReaders(t *testing.T) {
	a := memAgent(t)
	m := a.mem
	const cols, n = 4, 10
	fleet := statusFleet(t, m, cols, n)
	stopSyncs(m)
	dir := t.TempDir()
	live, peakFile := filepath.Join(dir, "live"), filepath.Join(dir, "peak")
	if err := os.Mkdir(live, 0o700); err != nil {
		t.Fatal(err)
	}
	// Only "git status" runs under a reader slot: it is what the wrapper tracks.
	script := "#!/bin/sh\ncase \"$*\" in *status*)\n  touch '" + live + "'/$$\n  ls '" + live + "' | wc -l >> '" + peakFile + "'\n  sleep 0.05\n  rm -f '" + live + "'/$$\n  ;;\nesac\nexec '" + m.git.Bin + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o700); err != nil { //nolint:gosec // test wrapper
		t.Fatal(err)
	}
	m.git.Bin = filepath.Join(dir, "git")

	var wg sync.WaitGroup
	for _, k := range fleet {
		wg.Go(func() { m.statusOf(context.Background(), k[0], k[1]) })
	}
	wg.Wait()
	b, _ := os.ReadFile(peakFile) //nolint:gosec // test file
	peak := 0
	for _, s := range strings.Fields(string(b)) {
		if v, err := strconv.Atoi(s); err == nil {
			peak = max(peak, v)
		}
	}
	t.Logf("peak concurrent git status = %d (bound %d)", peak, statusWorkers)
	if peak < 2 {
		t.Fatalf("peak %d: the wrapper did not see concurrent readers", peak)
	}
	if peak > statusWorkers {
		t.Fatalf("peak %d concurrent git readers, bound is %d", peak, statusWorkers)
	}
}
