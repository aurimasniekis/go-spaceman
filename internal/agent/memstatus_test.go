package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// countGit points the memory manager's git at a wrapper that counts every exec, and returns the counter.
func countGit(t *testing.T, m *memManager) func() int {
	t.Helper()
	dir := t.TempDir()
	counter := filepath.Join(dir, "count")
	script := "#!/bin/sh\nprintf x >> '" + counter + "'\nexec '" + m.git.Bin + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o700); err != nil { //nolint:gosec // test wrapper
		t.Fatal(err)
	}
	m.git.Bin = filepath.Join(dir, "git")
	return func() int {
		fi, err := os.Stat(counter)
		if err != nil {
			return 0
		}
		return int(fi.Size())
	}
}

// statusFleet makes cols collections with n instance checkouts each: one prepared, the rest copies of it (cheap, and
// to git each is a checkout of its own).
func statusFleet(t *testing.T, m *memManager, cols, n int) [][2]string {
	t.Helper()
	ctx := context.Background()
	var out [][2]string
	for c := range cols {
		sp, role := "spc_s", fmt.Sprintf("role-%d", c)
		first, err := m.prepare(ctx, sp, role, "ins_0")
		if err != nil {
			t.Fatal(err)
		}
		for i := 1; i < n; i++ {
			if err := os.CopyFS(m.instance(sp, role, "ins_"+strconv.Itoa(i)), os.DirFS(first)); err != nil {
				t.Fatal(err)
			}
		}
		out = append(out, [2]string{sp, role})
	}
	return out
}

func statusInstances(t *testing.T, def int) int {
	if v, err := strconv.Atoi(os.Getenv("SPACEMAN_MEMSTATUS_INSTANCES")); err == nil && v > 0 {
		return v
	}
	return def
}

// stopSyncs stops the syncs prepare scheduled: without a central they would fail and retry on their own.
func stopSyncs(m *memManager) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, tm := range m.syncs {
		tm.Stop()
		delete(m.syncs, k)
	}
}

// heads reads each collection's shared head.
func heads(ctx context.Context, m *memManager, cols [][2]string) []string {
	out := make([]string, len(cols))
	for i, k := range cols {
		out[i] = m.git.RevParse(ctx, m.shared(k[0], k[1]), "refs/heads/main")
	}
	return out
}

func waitReport(t *testing.T, ch <-chan proto.MemoryStatus) proto.MemoryStatus {
	t.Helper()
	select {
	case st := <-ch:
		return st
	case <-time.After(3 * time.Minute):
		t.Fatal("no memory status report")
		return proto.MemoryStatus{}
	}
}

func collectionStatus(st proto.MemoryStatus, role string) (proto.MemoryCollectionStatus, bool) {
	for _, c := range st.Collections {
		if c.Role == role {
			return c, true
		}
	}
	return proto.MemoryCollectionStatus{}, false
}

// SM-MEM-004: status reporting is coalesced and cached. Acknowledgements and checkpoints run no status git inline,
// reports come at most once per interval and hold only the collections that changed, and an unchanged instance
// checkout (an ended instance's) costs no git: the git execs of a report do not grow with the number of instance
// folders. SPACEMAN_MEMSTATUS_INSTANCES sets the instances per collection (4 collections); the default keeps it quick.
func TestMemoryStatusExecsBounded(t *testing.T) {
	a := memAgent(t)
	m := a.mem
	ctx := context.Background()
	oldInterval, oldRacy := statusInterval, statusRacy
	statusInterval, statusRacy = 300*time.Millisecond, 0 // copies made a moment ago are not racy for this test
	t.Cleanup(func() { statusInterval, statusRacy = oldInterval, oldRacy })
	reports := make(chan proto.MemoryStatus, 64)
	m.statusSent = func(st proto.MemoryStatus) { reports <- st }
	a.mu.Lock()
	a.connected, a.minor = true, proto.MaxMinor
	a.mu.Unlock()
	n := statusInstances(t, 25)
	cols := statusFleet(t, m, 4, n)
	stopSyncs(m)
	hs := heads(ctx, m, cols)
	count := countGit(t, m)
	ack := func() {
		for i, k := range cols {
			id := fmt.Sprintf("msy_%d", i)
			m.mu.Lock()
			m.waiting[colKey(k[0], k[1])] = id
			m.mu.Unlock()
			m.acked(ctx, proto.MemoryAck{SyncID: id, SpaceID: k[0], Role: k[1], Result: proto.MemoryOK, Head: hs[i]})
		}
	}

	// The first report after connecting counts every instance (git status may refresh an index, and git rewrites
	// racily clean ones, so the next report or two count some again); after that the counts are cached.
	t0, c0 := time.Now(), count()
	m.resume(ctx)
	stopSyncs(m)
	if st := waitReport(t, reports); len(st.Collections) != len(cols) || st.Collections[0].Instances != n {
		t.Fatalf("first report: %+v", st)
	}
	for i := 0; ; i++ {
		c := count()
		m.statusChangedAll()
		waitReport(t, reports)
		if count()-c <= len(cols)*4 {
			break
		}
		if i == 5 {
			t.Fatalf("the cache never settled: a full report still ran %d execs", count()-c)
		}
	}
	prime := count() - c0
	t.Logf("prime: instances=%d collections=%d execs=%d in %s", n*len(cols), len(cols), prime, time.Since(t0).Round(time.Millisecond))

	// Steady state: rounds of acknowledgements and checkpoint-like changes, as fast as they come.
	const rounds = 10
	c1, t1 := count(), time.Now()
	inline := 0
	m.reportMu.Lock() // a report due meanwhile waits, so what the burst itself runs is counted alone
	for range rounds {
		before := count()
		ack()
		m.statusChanged(cols[0][0], cols[0][1])
		inline += count() - before
	}
	m.reportMu.Unlock()
	sent := 0
	deadline := time.Now().Add(3 * time.Minute)
	for {
		m.statusMu.Lock()
		idle := m.statusTimer == nil
		m.statusMu.Unlock()
		if idle && sent > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("reports never settled")
		}
		select {
		case st := <-reports:
			sent++
			if len(st.Collections) > len(cols) {
				t.Fatalf("report of %d collections", len(st.Collections))
			}
		case <-time.After(50 * time.Millisecond):
		}
	}
	el := time.Since(t1)
	steady := count() - c1
	t.Logf("steady: acks=%d reports=%d execs=%d (inline %d) in %s, %.1f execs/s", rounds*len(cols), sent, steady, inline, el.Round(time.Millisecond), float64(steady)/el.Seconds())
	// Each ack resolves the shared head once (2 execs with the guard's config check); no status runs inline.
	if want := rounds * len(cols) * 2; inline != want {
		t.Fatalf("inline execs = %d, want %d (an ack's own head check only)", inline, want)
	}
	// Reports are coalesced: the acks of a burst do not each send one.
	if maxReports := int(el/statusInterval) + 2; sent > maxReports {
		t.Fatalf("%d reports in %s, want at most %d", sent, el, maxReports)
	}
	// A report costs git per collection (its head, and its unsynced commits), never per unchanged instance.
	if bound := inline + sent*len(cols)*4; steady > bound {
		t.Fatalf("steady-state execs = %d, want at most %d whatever the instance count (%d)", steady, bound, n*len(cols))
	}

	// Correct within the interval: a change in an ended instance is counted at the next report, and only that
	// checkout is asked again.
	write(t, m.instance(cols[1][0], cols[1][1], "ins_1"), "notes.md", "late note\n")
	c2 := count()
	m.statusChanged(cols[1][0], cols[1][1])
	st := waitReport(t, reports)
	got, ok := collectionStatus(st, cols[1][1])
	if !ok || len(st.Collections) != 1 || got.Uncheckpointed != 1 || got.Instances != n {
		t.Fatalf("after an edit: %+v", st)
	}
	if one := count() - c2; one > 4+7 {
		t.Fatalf("a report after one edit ran %d execs, want at most %d", one, 4+7)
	}
}
