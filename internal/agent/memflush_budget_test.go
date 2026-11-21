package agent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// A flush whose budget is spent names what it did not look at instead of running past the budget: no git runs for an
// instance after the deadline, and the checkouts it never reached are counted, not read as clean.
func TestFlushSpaceMemoryByDeadline(t *testing.T) {
	a := memAgent(t)
	m := a.mem
	ctx := context.Background()
	cols := statusFleet(t, m, 1, 12)
	stopSyncs(m)
	count := countGit(t, m)
	c0 := count()
	n, unchecked := a.flushSpaceMemoryBy(ctx, cols[0][0], "backup", time.Now().Add(-time.Second))
	if n != 1 || unchecked != 12 {
		t.Fatalf("collections=%d unchecked=%d, want 1 and 12", n, unchecked)
	}
	if got := count() - c0; got > 6 { // the collection's own head lookup and sync only, none per instance
		t.Fatalf("%d git execs after the deadline", got)
	}
	stopSyncs(m)
	if _, unchecked := a.flushSpaceMemoryBy(ctx, cols[0][0], "backup", time.Now().Add(time.Minute)); unchecked != 0 {
		t.Fatalf("unchecked=%d with time left", unchecked)
	}
	stopSyncs(m)
}

// Readers of one cold checkout share the work: the status report and a flush that start together read each checkout
// once, and every reader gets the same counts.
func TestInstanceStatusSingleReader(t *testing.T) {
	a := memAgent(t)
	m := a.mem
	ctx := context.Background()
	oldRacy := statusRacy
	statusRacy = 0
	t.Cleanup(func() { statusRacy = oldRacy })
	cols := statusFleet(t, m, 1, 2)
	stopSyncs(m)
	head := heads(ctx, m, cols)[0]
	inst := m.instance(cols[0][0], cols[0][1], "ins_1")
	for i := 0; ; i++ { // git rewrites a fresh checkout's index for a while: read until a read is cached
		m.instanceStatus(ctx, inst, head)
		m.statusMu.Lock()
		_, hit := m.instStatus[inst]
		m.statusMu.Unlock()
		if hit {
			break
		}
		if i == 20 {
			t.Fatal("counts never cached")
		}
		time.Sleep(300 * time.Millisecond)
	}
	m.statusMu.Lock()
	clear(m.instStatus)
	m.statusMu.Unlock()
	count := countGit(t, m)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if unc, local, ok := m.instanceStatus(ctx, inst, head); !ok || unc != 0 || local != 0 {
				t.Errorf("status = %d, %d, %v", unc, local, ok)
			}
		}()
	}
	wg.Wait()
	single := count()
	m.statusMu.Lock()
	clear(m.instStatus)
	m.statusMu.Unlock()
	m.instanceStatus(ctx, inst, head)
	if one := count() - single; single != one {
		t.Fatalf("8 concurrent readers ran %d git execs, one reader %d", single, one)
	}
}

// A flush that cannot get its space's turn within the budget (another flush holds it) answers at the deadline with the
// space's instances named as not looked at, instead of waiting for the turn past the budget.
func TestFlushSpaceMemoryByTurnHeld(t *testing.T) {
	a := memAgent(t)
	ctx := context.Background()
	cols := statusFleet(t, a.mem, 1, 3)
	stopSyncs(a.mem)
	release, ok := a.mem.flushTurn(ctx, cols[0][0])
	if !ok {
		t.Fatal("no turn")
	}
	defer release()
	t0 := time.Now()
	n, unchecked := a.flushSpaceMemoryBy(ctx, cols[0][0], "backup", time.Now().Add(300*time.Millisecond))
	if n != 1 || unchecked < 1 {
		t.Fatalf("collections=%d unchecked=%d", n, unchecked)
	}
	if d := time.Since(t0); d > 5*time.Second {
		t.Fatalf("took %v for a 300ms budget", d)
	}
}

// The relocation flush shares one budget for its walk and its wait, and is not clean when the walk did not reach every
// instance.
func TestRelocFlushBounded(t *testing.T) {
	a := memAgent(t)
	ctx := context.Background()
	cols := statusFleet(t, a.mem, 1, 3)
	stopSyncs(a.mem)
	release, _ := a.mem.flushTurn(ctx, cols[0][0])
	defer release()
	t0 := time.Now()
	if a.relocFlush(ctx, cols[0][0], 500*time.Millisecond) {
		t.Fatal("reported clean though nothing was checked")
	}
	if d := time.Since(t0); d > 5*time.Second {
		t.Fatalf("took %v for a 500ms budget", d)
	}
}

// A checkpoint that waited for the collection lock past its budget does not start; one with time left does.
func TestCheckpointByBudget(t *testing.T) {
	a := memAgent(t)
	m := a.mem
	ctx := context.Background()
	cols := statusFleet(t, m, 1, 2)
	stopSyncs(m)
	sp, role := cols[0][0], cols[0][1]
	unlock := m.lock(colKey(sp, role))
	errc := make(chan error, 1)
	go func() { errc <- m.checkpointBy(ctx, sp, role, "ins_1", "backup", time.Now().Add(200*time.Millisecond)) }()
	time.Sleep(500 * time.Millisecond)
	unlock()
	select {
	case err := <-errc:
		if !errors.Is(err, errBudgetSpent) {
			t.Fatalf("err = %v, want the budget error", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("checkpoint did not return")
	}
	if err := m.checkpointBy(ctx, sp, role, "ins_1", "backup", time.Now().Add(time.Minute)); err != nil {
		t.Fatalf("checkpoint with budget: %v", err)
	}
	stopSyncs(m)
}
