package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/runner"
	"github.com/aurimasniekis/go-spaceman/internal/runner/runnertest"
)

// ADR-0094: fast-forward of idle memory checkouts.

const (
	ffSpace = "spc_01m7md" + "ffffffffffffffffffff"
	ffRole  = "reviewer"
)

type ffEnv struct {
	t    *testing.T
	a    *Agent
	m    *memManager
	ctx  context.Context
	runs int
}

func newFFEnv(t *testing.T) *ffEnv {
	t.Helper()
	a := memAgent(t)
	return &ffEnv{t: t, a: a, m: a.mem, ctx: context.Background()}
}

// checkout prepares an instance's checkout of the test role.
func (e *ffEnv) checkout(instance string) string {
	e.t.Helper()
	dir, err := e.m.prepare(e.ctx, ffSpace, ffRole, instance)
	if err != nil {
		e.t.Fatal(err)
	}
	return dir
}

// run journals a run of instance (of role) in the given state and harness status and returns it.
func (e *ffEnv) run(instance, role, state, status string) runRow {
	e.t.Helper()
	e.runs++
	id := "run_01m7md" + strings.Repeat("f", 18) + string(rune('a'+e.runs/26)) + string(rune('a'+e.runs%26))
	r := runRow{RunID: id, SpaceID: ffSpace, Generation: 1, Kind: "worker", Role: role, Harness: "claude", InstanceID: instance,
		TerminalName: RunName(id), Desired: "running"}
	if err := e.a.j.insertRun(e.ctx, r); err != nil {
		e.t.Fatal(err)
	}
	r.State, r.Status = state, status
	if err := e.a.j.updateRun(e.ctx, r); err != nil {
		e.t.Fatal(err)
	}
	return r
}

// idleRun journals a live run of instance whose turn ended.
func (e *ffEnv) idleRun(instance string) runRow {
	return e.run(instance, ffRole, proto.RunRunning, "idle")
}

func (e *ffEnv) head(dir string) string { return e.m.git.RevParse(e.ctx, dir, "HEAD") }

func (e *ffEnv) sharedHead() string {
	return e.m.git.RevParse(e.ctx, e.m.shared(ffSpace, ffRole), "refs/heads/main")
}

func (e *ffEnv) git(dir string, args ...string) string {
	e.t.Helper()
	out, err := e.m.git.Run(e.ctx, dir, ident("ins_test"), args...)
	if err != nil {
		e.t.Fatal(err)
	}
	return out
}

func (e *ffEnv) checkpoint(instance, reason string) {
	e.t.Helper()
	if err := e.m.checkpointErr(e.ctx, ffSpace, ffRole, instance, reason); err != nil {
		e.t.Fatal(err)
	}
}

// checkpointAsync is checkpoint for another goroutine: it reports a failure without stopping the test from there.
func (e *ffEnv) checkpointAsync(instance string) {
	if err := e.m.checkpointErr(e.ctx, ffSpace, ffRole, instance, "idle"); err != nil {
		e.t.Error(err)
	}
}

func (e *ffEnv) conflicts() int {
	st, _ := e.a.j.memory(e.ctx, ffSpace, ffRole)
	return st.Conflicts
}

// A checkpoint that moves the shared head fast-forwards the other instances whose runs are between turns, and only
// those; the files appear in their folders and their HEAD is the shared head.
func TestFastForwardIdleCheckout(t *testing.T) {
	e := newFFEnv(t)
	idle, busy, other := e.checkout("ins_idle"), e.checkout("ins_busy"), e.checkout("ins_other")
	e.idleRun("ins_idle")
	e.run("ins_busy", ffRole, proto.RunRunning, "working")
	e.idleRun("ins_other")
	before := e.head(busy)

	write(t, other, "notes.md", "# Notes\n\n- from other\n")
	e.checkpoint("ins_other", "idle")

	if got := read(t, idle, "notes.md"); got != "# Notes\n\n- from other\n" {
		t.Fatalf("idle instance not fast-forwarded: %q", got)
	}
	if e.head(idle) != e.sharedHead() {
		t.Fatal("idle instance HEAD is not the shared head")
	}
	if e.head(busy) != before {
		t.Fatal("an instance in a turn was moved")
	}
	if _, err := os.Stat(filepath.Join(busy, "notes.md")); err == nil {
		t.Fatal("an instance in a turn got the file")
	}
	// Nothing moved the shared head except the checkpoint; the ff never writes it.
	if n := e.m.git.Count(e.ctx, e.m.shared(ffSpace, ffRole), "", "refs/heads/main"); n != 2 {
		t.Fatalf("shared history has %d commits, want 2", n)
	}
}

// refusals: every check refuses with its own reason and leaves the checkout exactly as it was.
func TestFastForwardRefusals(t *testing.T) {
	cases := []struct {
		name   string
		reason string
		setup  func(e *ffEnv, dir string, r runRow)
	}{
		{"unsafe config", ffUnsafeConfig, func(e *ffEnv, dir string, _ runRow) {
			f, err := os.OpenFile(filepath.Join(dir, ".git", "config"), os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				e.t.Fatal(err)
			}
			_, _ = f.WriteString("[core]\n\tpager = cat\n")
			_ = f.Close()
		}},
		{"merge", ffMerging, gitFile("MERGE_HEAD", false)},
		{"rebase merge", ffRebasing, gitFile("rebase-merge", true)},
		{"rebase apply", ffRebasing, gitFile("rebase-apply", true)},
		{"cherry-pick", ffCherryPicking, gitFile("CHERRY_PICK_HEAD", false)},
		{"revert", ffReverting, gitFile("REVERT_HEAD", false)},
		{"bisect", ffBisecting, gitFile("BISECT_LOG", false)},
		{"index lock", ffIndexLocked, gitFile("index.lock", false)},
		{"core.worktree", ffUnsafeConfig, func(e *ffEnv, dir string, _ runRow) {
			e.git(dir, "config", "core.worktree", filepath.Join(dir, "elsewhere"))
		}},
		{"core.bare", ffUnsafeConfig, func(e *ffEnv, dir string, _ runRow) {
			e.git(dir, "config", "core.bare", "true")
		}},
		{"staged", ffStaged, func(e *ffEnv, dir string, _ runRow) {
			write(e.t, dir, "staged.md", "staged\n")
			e.git(dir, "add", "staged.md")
		}},
		{"modified", ffModified, func(e *ffEnv, dir string, _ runRow) { write(e.t, dir, "README.md", "changed\n") }},
		{"deleted", ffModified, func(e *ffEnv, dir string, _ runRow) {
			if err := os.Remove(filepath.Join(dir, "README.md")); err != nil {
				e.t.Fatal(err)
			}
		}},
		{"untracked", ffUntracked, func(e *ffEnv, dir string, _ runRow) { write(e.t, dir, "new.md", "new\n") }},
		{"untracked in folder", ffUntracked, func(e *ffEnv, dir string, _ runRow) { write(e.t, dir, "sub/deep/new.md", "new\n") }},
		{"ignored", ffIgnored, func(e *ffEnv, dir string, _ runRow) {
			write(e.t, dir, ".git/info/exclude", "*.tmp\n")
			write(e.t, dir, "scratch.tmp", "local only\n")
		}},
		{"diverged", ffDiverged, func(e *ffEnv, dir string, _ runRow) {
			write(e.t, dir, "mine.md", "mine\n")
			e.git(dir, "add", "-A")
			e.git(dir, "commit", "--quiet", "-m", "local")
		}},
		{"no live run", ffNoLiveRun, func(e *ffEnv, _ string, r runRow) {
			r.State = proto.RunExited
			_ = e.a.j.updateRun(e.ctx, r)
			e.run("ins_a", ffRole, proto.RunExited, "idle") // ended runs don't count either
		}},
		{"starting", ffRunNotRunning, func(e *ffEnv, _ string, r runRow) {
			r.State = proto.RunStarting
			_ = e.a.j.updateRun(e.ctx, r)
		}},
		{"stopping", ffRunNotRunning, func(e *ffEnv, _ string, r runRow) {
			r.State = proto.RunStopping
			_ = e.a.j.updateRun(e.ctx, r)
		}},
		{"a second run pending", ffRunNotRunning, func(e *ffEnv, _ string, _ runRow) {
			e.run("ins_a", ffRole, proto.RunPending, "")
		}},
		{"working", ffTurnNotEnded, func(e *ffEnv, _ string, r runRow) {
			r.Status = "working"
			_ = e.a.j.updateRun(e.ctx, r)
		}},
		{"blocked", ffTurnNotEnded, func(e *ffEnv, _ string, r runRow) {
			r.Status = "blocked"
			_ = e.a.j.updateRun(e.ctx, r)
		}},
		{"no status", ffTurnNotEnded, func(e *ffEnv, _ string, r runRow) {
			r.Status = ""
			_ = e.a.j.updateRun(e.ctx, r)
		}},
		{"dreamer run of the instance in a turn", ffTurnNotEnded, func(e *ffEnv, _ string, _ runRow) {
			e.run("ins_a", "dreamer", proto.RunRunning, "working")
		}},
		{"checkpoint pending", ffCheckpointDue, func(e *ffEnv, _ string, r runRow) {
			e.m.kick(r)
			e.t.Cleanup(func() {
				e.m.mu.Lock()
				if tm := e.m.timers[colKey(r.SpaceID, r.Role)+"/"+r.InstanceID]; tm != nil {
					tm.Stop()
				}
				e.m.mu.Unlock()
			})
		}},
		{"taken over", ffTakenOver, func(e *ffEnv, _ string, r runRow) {
			e.a.mu.Lock()
			e.a.takeovers[r.RunID] = &takeover{runID: r.RunID}
			e.a.mu.Unlock()
		}},
		{"input sent, turn not reported", ffInputPending, func(e *ffEnv, _ string, r runRow) {
			if err := e.m.deliverInput(r, func() error { return nil }); err != nil {
				e.t.Fatal(err)
			}
		}},
		{"dreamer run of the instance, its checkpoint pending", ffCheckpointDue, func(e *ffEnv, _ string, r runRow) {
			r.State = proto.RunExited
			_ = e.a.j.updateRun(e.ctx, r)
			d := e.run("ins_a", "dreamer", proto.RunRunning, "idle")
			e.m.kick(d) // keyed by the dreamer run's own role, not the collection's
			e.t.Cleanup(func() {
				e.m.mu.Lock()
				if tm := e.m.timers[colKey(d.SpaceID, d.Role)+"/"+d.InstanceID]; tm != nil {
					tm.Stop()
				}
				e.m.mu.Unlock()
			})
		}},
		{"input in delivery", ffInputInDelivery, func(e *ffEnv, _ string, _ runRow) {
			g := e.m.inputGate("ins_a")
			g.Lock()
			e.t.Cleanup(g.Unlock)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newFFEnv(t)
			dir, other := e.checkout("ins_a"), e.checkout("ins_b")
			r := e.idleRun("ins_a")
			write(t, other, "notes.md", "from b\n")
			e.checkpoint("ins_b", "idle") // also fast-forwards ins_a: the baseline below proves it
			if e.head(dir) != e.sharedHead() {
				t.Fatal("baseline: the idle instance was not fast-forwarded")
			}
			write(t, other, "notes.md", "from b\nagain\n")
			if _, err := e.m.publish(e.ctx, ffSpace, ffRole, "ins_b", "idle"); err != nil { // moves the head, no fast-forward
				t.Fatal(err)
			}
			c.setup(e, dir, r)
			head, status := e.head(dir), statusOf(t, e, dir)
			got, considered := e.m.fastForwardIdle(e.ctx, ffSpace, ffRole, "ins_b")["ins_a"]
			if c.reason == ffNoLiveRun {
				// An instance without a live run is not even considered (run start refreshes it); the idle check, which
				// the fast-forward repeats under its locks, says why.
				if considered {
					t.Fatalf("an instance without a live run was considered: %q", got)
				}
				got = e.m.idleReason(e.ctx, "ins_a")
			}
			if got != c.reason {
				t.Fatalf("reason %q, want %q", got, c.reason)
			}
			if e.head(dir) != head || statusOf(t, e, dir) != status {
				t.Fatal("a refused fast-forward changed the checkout")
			}
			if c.reason != ffDiverged && c.reason != ffUnsafeConfig && strings.Contains(readOr(dir, "notes.md"), "again") {
				t.Fatal("the refused checkout got the new content")
			}
		})
	}
}

// A checkout already at the shared head is reported up to date; with the kill switch nothing is considered.
func TestFastForwardUpToDateAndDisabled(t *testing.T) {
	e := newFFEnv(t)
	dir, other := e.checkout("ins_a"), e.checkout("ins_b")
	e.idleRun("ins_a")
	write(t, other, "notes.md", "from b\n")
	e.checkpoint("ins_b", "idle")
	if got := e.m.fastForwardIdle(e.ctx, ffSpace, ffRole, "ins_b"); got["ins_a"] != ffUpToDate {
		t.Fatalf("reason %q, want %q", got["ins_a"], ffUpToDate)
	}

	e.a.o.NoMemoryFastForward = true
	head := e.head(dir)
	write(t, other, "notes.md", "from b\nagain\n")
	e.checkpoint("ins_b", "idle")
	if e.head(dir) != head {
		t.Fatal("memory.fast_forward off still moved an idle checkout")
	}
	if got := e.m.fastForwardIdle(e.ctx, ffSpace, ffRole, "ins_b"); got != nil {
		t.Fatalf("disabled fast-forward considered %v", got)
	}
	// Run start refreshes as before: a clean checkout is fast-forwarded (refresh), one with ignored files too.
	write(t, dir, ".git/info/exclude", "*.tmp\n")
	write(t, dir, "scratch.tmp", "local\n")
	if got := e.m.refreshAtStart(e.ctx, ffSpace, ffRole, "ins_a"); got != ffDisabled {
		t.Fatalf("refreshAtStart %q, want %q", got, ffDisabled)
	}
	if e.head(dir) != e.sharedHead() {
		t.Fatal("run start with the switch off no longer refreshes")
	}
}

// Run start uses the content checks: an ignored file (which a fast-forward could overwrite) keeps the checkout where it
// is; a clean one is fast-forwarded.
func TestRefreshAtStartChecks(t *testing.T) {
	e := newFFEnv(t)
	dir, other := e.checkout("ins_a"), e.checkout("ins_b")
	write(t, other, "notes.md", "from b\n")
	e.checkpoint("ins_b", "idle") // ins_a has no live run: not touched here
	if e.head(dir) == e.sharedHead() {
		t.Fatal("an instance without a live run was fast-forwarded outside run start")
	}
	write(t, dir, ".git/info/exclude", "notes.md\n")
	write(t, dir, "notes.md", "local, ignored\n")
	head := e.head(dir)
	if _, err := e.m.prepare(e.ctx, ffSpace, ffRole, "ins_a"); err != nil { // run start
		t.Fatal(err)
	}
	if read(t, dir, "notes.md") != "local, ignored\n" || e.head(dir) != head {
		t.Fatal("run start overwrote an ignored file")
	}
	if got := e.m.refreshAtStart(e.ctx, ffSpace, ffRole, "ins_a"); got != ffIgnored {
		t.Fatalf("reason %q, want %q", got, ffIgnored)
	}
	if err := os.Remove(filepath.Join(dir, "notes.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.prepare(e.ctx, ffSpace, ffRole, "ins_a"); err != nil {
		t.Fatal(err)
	}
	if read(t, dir, "notes.md") != "from b\n" {
		t.Fatal("run start did not fast-forward a clean checkout")
	}
}

// The original symptom (ADR-0094): after a dream checkpoint, an instance between turns records a fact the dream already
// wrote. With the fast-forward its folder already holds the dream's text, the edit changes nothing and no conflict is
// preserved. Without it (the switch off, the behaviour before) the same edit on the old text conflicts.
func TestDreamThenIdenticalEditDoesNotConflict(t *testing.T) {
	for _, ff := range []bool{true, false} {
		name := "fast-forward"
		if !ff {
			name = "switch off (before)"
		}
		t.Run(name, func(t *testing.T) {
			e := newFFEnv(t)
			e.a.o.NoMemoryFastForward = !ff
			worker := e.checkout("ins_worker")
			write(t, worker, "notes.md", "# Notes\n\n- retries: 3\n- timeout: 5s\n")
			e.checkpoint("ins_worker", "idle")
			e.idleRun("ins_worker")
			// The dream (a dreamer run, its own checkout of this role) consolidates the notes.
			dream := e.checkout("ins_dreamer")
			e.run("ins_dreamer", "dreamer", proto.RunRunning, "working")
			write(t, dream, "notes.md", "# Notes\n\n- retries: 3\n- timeout: 10s (raised)\n- cache: on\n")
			e.checkpoint("ins_dreamer", "dream")

			// The worker's next turn records "cache: on", which it learned too: it adds the line unless the note has it.
			cur := read(t, worker, "notes.md")
			if !strings.Contains(cur, "- cache: on\n") {
				cur += "- cache: on\n"
			}
			write(t, worker, "notes.md", cur)
			e.checkpoint("ins_worker", "idle")

			conflicts := e.conflicts()
			_, statErr := os.Stat(filepath.Join(worker, "conflicts"))
			if ff {
				if conflicts != 0 || statErr == nil {
					t.Fatalf("conflicts %d, conflicts folder present %v", conflicts, statErr == nil)
				}
				if read(t, worker, "notes.md") != "# Notes\n\n- retries: 3\n- timeout: 10s (raised)\n- cache: on\n" {
					t.Fatalf("notes %q", read(t, worker, "notes.md"))
				}
				return
			}
			if conflicts != 1 || statErr != nil {
				t.Fatalf("before the change this conflicted; got conflicts %d", conflicts)
			}
		})
	}
}

// Appending to the same file in two instances, the second between turns, no longer conflicts.
func TestIdleAppendAfterOtherAppendMerges(t *testing.T) {
	e := newFFEnv(t)
	one, two := e.checkout("ins_one"), e.checkout("ins_two")
	e.idleRun("ins_one")
	e.idleRun("ins_two")
	write(t, one, "log.md", "# Log\n")
	e.checkpoint("ins_one", "idle")
	write(t, one, "log.md", "# Log\n- one\n")
	e.checkpoint("ins_one", "idle")
	write(t, two, "log.md", read(t, two, "log.md")+"- two\n")
	e.checkpoint("ins_two", "idle")
	if e.conflicts() != 0 {
		t.Fatal("an append between turns conflicted")
	}
	if got := read(t, one, "log.md"); got != "# Log\n- one\n- two\n" {
		t.Fatalf("log %q", got)
	}
}

// Node-delivered input (run.input) waits while a fast-forward of the instance's checkout merges, and reaches the run
// only after the merge ended.
func TestInputWaitsForFastForward(t *testing.T) {
	e := newFFEnv(t)
	f := runnertest.New()
	e.a.o.Runner = f
	e.a.leases[ffSpace] = leaseRow{SpaceID: ffSpace, Generation: 1, ExpiresAt: time.Now().Add(time.Hour), Workdir: "ff"}
	dir, other := e.checkout("ins_a"), e.checkout("ins_b")
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

	inMerge, release := make(chan struct{}), make(chan struct{})
	e.m.ffHook = func(string) { close(inMerge); <-release }
	write(t, other, "notes.md", "from b\n")
	done := make(chan struct{})
	go func() { defer close(done); e.checkpointAsync("ins_b") }()
	<-inMerge

	delivered := make(chan struct{})
	go func() {
		defer close(delivered)
		pl, _ := json.Marshal(proto.InputPayload{Text: "hello"})
		e.a.command(e.ctx, proto.Command{CommandID: "cmd_01m7md" + strings.Repeat("f", 20), Kind: proto.CmdInput, SpaceID: ffSpace, RunID: r.RunID,
			Generation: 1, IdempotencyKey: "input-1", Payload: pl})
	}()
	select {
	case <-delivered:
		t.Fatal("input was delivered during the fast-forward")
	case <-time.After(300 * time.Millisecond):
	}
	if len(f.Inputs(p.ID)) != 0 {
		t.Fatal("the run got input during the fast-forward")
	}
	close(release)
	<-done
	<-delivered
	if got := f.Inputs(p.ID); len(got) != 1 || got[0] != "hello" {
		t.Fatalf("inputs %v", got)
	}
	if read(t, dir, "notes.md") != "from b\n" {
		t.Fatal("the fast-forward did not complete")
	}
}

// A run that starts during a fast-forward: its prepare waits for the collection lock, so it never sees a half-moved
// checkout, and a run journaled before the fast-forward makes the instance not idle.
func TestRunStartDuringFastForward(t *testing.T) {
	e := newFFEnv(t)
	dir, other := e.checkout("ins_a"), e.checkout("ins_b")
	e.idleRun("ins_a")

	inMerge, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	e.m.ffHook = func(string) { once.Do(func() { close(inMerge); <-release }) }
	write(t, other, "notes.md", "from b\n")
	done := make(chan struct{})
	go func() { defer close(done); e.checkpointAsync("ins_b") }()
	<-inMerge

	prepared := make(chan string, 1)
	e.run("ins_a", ffRole, proto.RunPending, "") // the launch journals its run, then prepares memory
	go func() {
		d, _ := e.m.prepare(e.ctx, ffSpace, ffRole, "ins_a")
		prepared <- d
	}()
	select {
	case <-prepared:
		t.Fatal("a run start's prepare ran during the fast-forward")
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	<-done
	if d := <-prepared; d != dir || read(t, dir, "notes.md") != "from b\n" || e.head(dir) != e.sharedHead() {
		t.Fatal("the run did not start on the fast-forwarded checkout")
	}

	// The other order: the new run is journaled first, so the next fast-forward skips the instance.
	write(t, other, "notes.md", "from b\nagain\n")
	if _, err := e.m.publish(e.ctx, ffSpace, ffRole, "ins_b", "idle"); err != nil {
		t.Fatal(err)
	}
	if got := e.m.fastForwardIdle(e.ctx, ffSpace, ffRole, "ins_b"); got["ins_a"] != ffRunNotRunning {
		t.Fatalf("reason %q, want %q", got["ins_a"], ffRunNotRunning)
	}
}

// A turn that starts after the first checks (the status turns working) is caught by the re-check right before the
// merge.
func TestTurnStartBetweenChecks(t *testing.T) {
	e := newFFEnv(t)
	dir, other := e.checkout("ins_a"), e.checkout("ins_b")
	r := e.idleRun("ins_a")
	write(t, other, "notes.md", "from b\n")
	if _, err := e.m.publish(e.ctx, ffSpace, ffRole, "ins_b", "idle"); err != nil {
		t.Fatal(err)
	}
	head := e.head(dir)
	e.m.ffGated = func(string) {
		r.Status = "working"
		_ = e.a.j.updateRun(e.ctx, r)
	}
	if got := e.m.fastForwardIdle(e.ctx, ffSpace, ffRole, "ins_b")["ins_a"]; got != ffTurnNotEnded || e.head(dir) != head {
		t.Fatalf("reason %q, moved %v", got, e.head(dir) != head)
	}
	// A file written between the checks is caught the same way.
	r.Status = "idle"
	_ = e.a.j.updateRun(e.ctx, r)
	e.m.ffGated = func(string) { write(t, dir, "late.md", "late\n") }
	if got := e.m.fastForwardIdle(e.ctx, ffSpace, ffRole, "ins_b")["ins_a"]; got != ffUntracked || e.head(dir) != head {
		t.Fatalf("reason %q, moved %v", got, e.head(dir) != head)
	}
}

// A busy instance's checkout is not even read: the idle check comes before any git command, so no fetch lands in it,
// and its reason is the turn, not the input being delivered to it.
func TestBusyInstanceNotTouched(t *testing.T) {
	e := newFFEnv(t)
	other, dir := e.checkout("ins_b"), e.checkout("ins_a") // ins_a starts from shared history: it has a FETCH_HEAD
	e.run("ins_a", ffRole, proto.RunRunning, "working")
	fetchHead := filepath.Join(dir, ".git", "FETCH_HEAD")
	before, err := os.ReadFile(fetchHead)
	if err != nil {
		t.Fatal(err)
	}
	g := e.m.inputGate("ins_a")
	g.Lock() // input is being delivered: it started the turn
	defer g.Unlock()
	write(t, other, "notes.md", "from b\n")
	if _, err := e.m.publish(e.ctx, ffSpace, ffRole, "ins_b", "idle"); err != nil {
		t.Fatal(err)
	}
	if got := e.m.fastForwardIdle(e.ctx, ffSpace, ffRole, "ins_b")["ins_a"]; got != ffTurnNotEnded {
		t.Fatalf("reason %q, want %q", got, ffTurnNotEnded)
	}
	if after, _ := os.ReadFile(fetchHead); string(after) != string(before) {
		t.Fatal("the busy checkout was fetched into")
	}
}

// Input the node sent keeps the instance busy until the run reports a later turn, even while its status still says
// idle; once the turn is reported, the instance follows the head again. Input that failed marks nothing.
func TestInputPendingUntilTurnReported(t *testing.T) {
	e := newFFEnv(t)
	dir, other := e.checkout("ins_a"), e.checkout("ins_b")
	r := e.idleRun("ins_a")
	if err := e.m.deliverInput(r, func() error { return errors.New("busy") }); err == nil {
		t.Fatal("the send error was lost")
	}
	write(t, other, "notes.md", "one\n")
	e.checkpoint("ins_b", "idle")
	if read(t, dir, "notes.md") != "one\n" {
		t.Fatal("failed input kept the instance busy")
	}
	if err := e.m.deliverInput(r, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	write(t, other, "notes.md", "one\ntwo\n")
	if _, err := e.m.publish(e.ctx, ffSpace, ffRole, "ins_b", "idle"); err != nil {
		t.Fatal(err)
	}
	if got := e.m.fastForwardIdle(e.ctx, ffSpace, ffRole, "ins_b")["ins_a"]; got != ffInputPending {
		t.Fatalf("reason %q, want %q", got, ffInputPending)
	}
	r.Turn++ // the run reported the turn the input started (working, then idle again)
	if err := e.a.j.updateRun(e.ctx, r); err != nil {
		t.Fatal(err)
	}
	if got := e.m.fastForwardIdle(e.ctx, ffSpace, ffRole, "ins_b")["ins_a"]; got != "" || read(t, dir, "notes.md") != "one\ntwo\n" {
		t.Fatalf("reason %q after the turn was reported", got)
	}
}

// An ignored file created after the checks, at a path the new history adds, is not overwritten: the merge refuses.
func TestIgnoredFileAfterChecksNotOverwritten(t *testing.T) {
	e := newFFEnv(t)
	dir, other := e.checkout("ins_a"), e.checkout("ins_b")
	e.idleRun("ins_a")
	write(t, dir, ".git/info/exclude", "notes.md\n")
	head := e.head(dir)
	e.m.ffHook = func(string) { write(t, dir, "notes.md", "local, ignored\n") }
	write(t, other, "notes.md", "from b\n")
	if _, err := e.m.publish(e.ctx, ffSpace, ffRole, "ins_b", "idle"); err != nil {
		t.Fatal(err)
	}
	if got := e.m.fastForwardIdle(e.ctx, ffSpace, ffRole, "ins_b")["ins_a"]; got != ffMergeFailed {
		t.Fatalf("reason %q, want %q", got, ffMergeFailed)
	}
	if read(t, dir, "notes.md") != "local, ignored\n" || e.head(dir) != head {
		t.Fatal("an ignored file was overwritten by the fast-forward")
	}
}

// Run start leaves the checkout alone while another live run of the same instance exists: it may be writing there.
func TestRefreshAtStartWithAnotherLiveRun(t *testing.T) {
	e := newFFEnv(t)
	dir, other := e.checkout("ins_a"), e.checkout("ins_b")
	e.run("ins_a", ffRole, proto.RunRunning, "working")
	write(t, other, "notes.md", "from b\n")
	e.checkpoint("ins_b", "idle")
	head := e.head(dir)
	e.run("ins_a", ffRole, proto.RunPending, "") // the new launch
	if _, err := e.m.prepare(e.ctx, ffSpace, ffRole, "ins_a"); err != nil {
		t.Fatal(err)
	}
	if e.head(dir) != head {
		t.Fatal("run start moved a checkout another live run of the instance uses")
	}
	if got := e.m.refreshAtStart(e.ctx, ffSpace, ffRole, "ins_a"); got != ffOtherLiveRun {
		t.Fatalf("reason %q, want %q", got, ffOtherLiveRun)
	}
}

// A takeover that starts while a fast-forward merges waits for the merge before the operator gets the terminal.
func TestTakeoverWaitsForFastForward(t *testing.T) {
	e := newFFEnv(t)
	e.checkout("ins_a")
	other := e.checkout("ins_b")
	e.idleRun("ins_a")
	inMerge, release := make(chan struct{}), make(chan struct{})
	e.m.ffHook = func(string) { close(inMerge); <-release }
	write(t, other, "notes.md", "from b\n")
	done := make(chan struct{})
	go func() { defer close(done); e.checkpointAsync("ins_b") }()
	<-inMerge
	waited := make(chan struct{})
	go func() { defer close(waited); e.m.awaitInputGate("ins_a") }()
	select {
	case <-waited:
		t.Fatal("a takeover did not wait for the merge")
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	<-done
	<-waited
}

// startTakeover itself waits for the gate: the operator's terminal is attached only after a merge in progress ended.
func TestStartTakeoverWaitsForGate(t *testing.T) {
	e := newFFEnv(t)
	f := runnertest.New()
	e.a.o.Runner = f
	e.a.leases[ffSpace] = leaseRow{SpaceID: ffSpace, Generation: 1, ExpiresAt: time.Now().Add(time.Hour), Workdir: "ff"}
	r := e.idleRun("ins_a")
	p, err := f.Start(e.ctx, runner.Spec{Name: r.TerminalName, Argv: []string{"x"}})
	if err != nil {
		t.Fatal(err)
	}
	r.TerminalID = p.ID
	if err := e.a.j.updateRun(e.ctx, r); err != nil {
		t.Fatal(err)
	}
	g := e.m.inputGate("ins_a")
	g.Lock() // a fast-forward is merging
	started := make(chan struct{})
	go func() {
		defer close(started)
		e.a.startTakeover(e.ctx, proto.Takeover{TakeoverID: "tko_1", SpaceID: ffSpace, RunID: r.RunID, Generation: 1})
	}()
	select {
	case <-started:
		g.Unlock()
		t.Fatal("the takeover did not wait for the merge")
	case <-time.After(300 * time.Millisecond):
	}
	if !e.a.takenOver(r.RunID) {
		g.Unlock()
		t.Fatal("the takeover was not registered before waiting: a later fast-forward would not see it")
	}
	if f.TakeoverAttached(p.ID) != 0 {
		g.Unlock()
		t.Fatal("the operator's terminal was attached during the merge")
	}
	g.Unlock()
	<-started
	e.a.mu.Lock()
	tk := e.a.takeovers[r.RunID]
	e.a.mu.Unlock()
	if tk != nil {
		<-tk.ready
		tk.cancel()
	}
}

// statusReason classifies porcelain output: staged before modified before untracked before ignored, renames skip
// their source path.
func TestStatusReason(t *testing.T) {
	cases := map[string]string{
		"":                                   "",
		"M  a.md\x00":                        ffStaged,
		" M a.md\x00":                        ffModified,
		" D a.md\x00":                        ffModified,
		"?? a.md\x00":                        ffUntracked,
		"!! a.tmp\x00":                       ffIgnored,
		"!! a.tmp\x00?? b.md\x00":            ffUntracked,
		"R  new.md\x00old.md\x00":            ffStaged,
		"?? b.md\x00 M a.md\x00A  c.md\x00":  ffStaged,
		"!! x\x00?? y\x00 M z\x00":           ffModified,
		"R  new.md\x00?? old.md\x00!! z\x00": ffStaged,
	}
	for in, want := range cases {
		if got := statusReason(in); got != want {
			t.Errorf("statusReason(%q) = %q, want %q", in, got, want)
		}
	}
}

func gitFile(name string, dir bool) func(e *ffEnv, d string, _ runRow) {
	return func(e *ffEnv, d string, _ runRow) {
		p := filepath.Join(d, ".git", name)
		var err error
		if dir {
			err = os.Mkdir(p, 0o700)
		} else {
			err = os.WriteFile(p, []byte("x\n"), 0o600)
		}
		if err != nil {
			e.t.Fatal(err)
		}
	}
}

func statusOf(t *testing.T, e *ffEnv, dir string) string {
	t.Helper()
	out, _ := e.m.git.Run(e.ctx, dir, nil, "status", "--porcelain", "--ignored", "--untracked-files=all")
	return out
}

func readOr(dir, name string) string {
	b, _ := os.ReadFile(filepath.Join(dir, name))
	return string(b)
}
