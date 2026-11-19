package agent

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/runner/runnertest"

	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/runner"
)

// A stop that lands between a pre-start failure's abandoned check and its write is not overwritten: the write
// re-checks under the run's lock instead of replacing the stopped row with failed.
func TestLaunchFailStopBetweenCheckAndWrite(t *testing.T) {
	dir := t.TempDir()
	a, f := ccAgent(t, dir, nil, nil)
	a.beforeFailWrite = func() { a.stop(context.Background(), "run_failrace", "kill", 0, "test") }
	a.launch(context.Background(), proto.Command{CommandID: "cmd_run_failrace", Kind: proto.CmdLaunch, SpaceID: ccSpace, RunID: "run_failrace", Generation: 1},
		proto.LaunchPayload{Harness: "no-such-harness", Kind: "worker", Role: "implementor", InstanceID: "ins_cc"})
	if r := getRun(t, a, "run_failrace"); r.State != proto.RunExited || r.Desired != "stopped" || r.Error != "" {
		t.Fatalf("state %s (%s / %s), desired %s", r.State, r.Reason, r.Error, r.Desired)
	}
	notStarted(t, f, "run_failrace")
}

// A launch re-delivered after a crash between Runner.Start and the observe write finds the program running while the
// row has no terminal. A stop that lands between the find and the observe ends the run as stopped before start; the
// adoption must then stop the found program, not record it as running with desired running.
func TestLaunchAdoptStopBetweenFindAndObserve(t *testing.T) {
	dir := t.TempDir()
	a, f := ccAgent(t, dir, nil, nil)
	ctx := context.Background()
	p, err := f.Start(ctx, runner.Spec{Name: RunName("run_adoptrace"), Argv: []string{"/bin/true"},
		Labels: map[string]string{"spaceman.managed": "1", "spaceman.run": "run_adoptrace"}})
	if err != nil {
		t.Fatal(err)
	}
	a.beforeAdoptObserve = func() { a.stop(ctx, "run_adoptrace", "kill", 0, "test") }
	ccLaunch(a, "run_adoptrace")
	r := getRun(t, a, "run_adoptrace")
	if r.Desired != "stopped" || !ended(r.State) {
		t.Fatalf("state %s (%s / %s), desired %s", r.State, r.Reason, r.Error, r.Desired)
	}
	if live := f.LiveByLabel("spaceman.run", "run_adoptrace"); len(live) != 0 {
		t.Fatalf("the found program %s is still live", p.ID)
	}
}

var (
	adoptWaitRun  = ids.New(ids.Run)
	adoptWaitInst = ids.New(ids.Instance)
)

// asyncKillRunner is a runner whose Kill returns before the program is gone: the program exits after exitAfter Get
// calls, or never when exitAfter is negative.
type asyncKillRunner struct {
	*runnertest.Fake
	exitAfter int
	mu        sync.Mutex
	killed    map[string]int // id -> Gets since the kill
}

func (r *asyncKillRunner) Kill(_ context.Context, id string) (*runner.Process, error) {
	r.mu.Lock()
	if r.killed == nil {
		r.killed = map[string]int{}
	}
	r.killed[id] = 0
	r.mu.Unlock()
	return r.Fake.Get(context.Background(), id)
}

func (r *asyncKillRunner) Get(ctx context.Context, id string) (*runner.Process, error) {
	r.mu.Lock()
	n, killed := r.killed[id]
	if killed {
		r.killed[id] = n + 1
	}
	r.mu.Unlock()
	if killed && r.exitAfter >= 0 && n+1 >= r.exitAfter {
		r.Exit(id, 129, "killed")
	}
	return r.Fake.Get(ctx, id)
}

func shortAdoptKillWait(t *testing.T, tries int) {
	t.Helper()
	ot, op := adoptKillTries, adoptKillPoll
	adoptKillTries, adoptKillPoll = tries, time.Millisecond
	t.Cleanup(func() { adoptKillTries, adoptKillPoll = ot, op })
}

// adoptStoppedRun seeds a program, makes the run stopped before start and adopts the program, with the runner's Kill
// asynchronous. It returns the agent, the program's id and the journal row afterwards.
func adoptStoppedRun(t *testing.T, exitAfter int) (*Agent, *asyncKillRunner, string, runRow) {
	t.Helper()
	dir := t.TempDir()
	var ar *asyncKillRunner
	a, _ := ccAgent(t, dir, nil, func(o *Options) { ar = &asyncKillRunner{Fake: runnertest.New(), exitAfter: exitAfter}; o.Runner = ar })
	ctx := context.Background()
	p, err := ar.Start(ctx, runner.Spec{Name: RunName(adoptWaitRun), Argv: []string{"/bin/true"},
		Labels: map[string]string{"spaceman.managed": "1", "spaceman.run": adoptWaitRun, "spaceman.space": ccSpace}})
	if err != nil {
		t.Fatal(err)
	}
	ccAgentSeedRun(t, a)
	a.stop(ctx, adoptWaitRun, "kill", 0, "test") // no terminal yet: ended as stopped before start
	a.adopt(ctx, a.log, adoptWaitRun, *p)
	return a, ar, p.ID, getRun(t, a, adoptWaitRun)
}

// ccAgentSeedRun journals a pending run with no terminal, as a launch does before it starts the program.
func ccAgentSeedRun(t *testing.T, a *Agent) {
	t.Helper()
	err := a.j.insertRun(context.Background(), runRow{RunID: adoptWaitRun, SpaceID: ccSpace, Generation: 1, TerminalName: RunName(adoptWaitRun),
		InstanceID: adoptWaitInst, Desired: "running", State: proto.RunPending})
	if err != nil {
		t.Fatal(err)
	}
}

// The kill-wait loop: an asynchronous kill is waited for, so the program is gone and the row records no live program.
func TestAdoptStoppedRunWaitsForAsyncKill(t *testing.T) {
	shortAdoptKillWait(t, 50)
	_, ar, id, r := adoptStoppedRun(t, 5)
	if p, _ := ar.Fake.Get(context.Background(), id); p.State != runner.StateFinished {
		t.Fatalf("the program is still live")
	}
	if !ended(r.State) {
		t.Fatalf("row %s, want ended", r.State)
	}
	if ar.killed[id] < 5 {
		t.Fatalf("polled %d times, want the loop to run until the program was gone", ar.killed[id])
	}
}

// A program still live after the wait keeps the row ended and records its terminal for the monitor's finished event.
func TestAdoptStoppedRunStillLiveAfterWait(t *testing.T) {
	shortAdoptKillWait(t, 7)
	a, ar, id, r := adoptStoppedRun(t, -1)
	if r.TerminalID != id || !r.Adopted || !ended(r.State) {
		t.Fatalf("row %s terminal %q adopted %v, want ended with terminal %s", r.State, r.TerminalID, r.Adopted, id)
	}
	if ar.killed[id] != 8 {
		t.Fatalf("polled %d times, want the 7 tries and the last look", ar.killed[id])
	}
	if _, ok := a.endPending.Load(adoptWaitRun); !ok {
		t.Fatal("the end work is not pending for the program's finished event")
	}
	ar.Exit(id, 129, "killed")
	pr, _ := ar.Fake.Get(context.Background(), id)
	row, _ := a.j.getRun(context.Background(), adoptWaitRun)
	a.observe(context.Background(), row, *pr, false) // what the monitor does on the finished event
	if _, ok := a.endPending.Load(adoptWaitRun); ok {
		t.Fatal("the end work stayed pending after the program finished")
	}
}

// Adopting a program of a run stopped before start does the run-end work observe skips for an ended row: the space's
// idle lease guard is retired and the run's attachments removed.
func TestAdoptStoppedRunRunsEndHooks(t *testing.T) {
	shortAdoptKillWait(t, 50)
	dir := t.TempDir()
	a, f := ccAgent(t, dir, nil, nil)
	ctx := context.Background()
	p, err := f.Start(ctx, runner.Spec{Name: RunName(adoptWaitRun), Argv: []string{"/bin/true"},
		Labels: map[string]string{"spaceman.managed": "1", "spaceman.run": adoptWaitRun, "spaceman.space": ccSpace}})
	if err != nil {
		t.Fatal(err)
	}
	ccAgentSeedRun(t, a)
	guard, err := a.ensureGuard(ctx, ccSpace)
	if err != nil {
		t.Fatal(err)
	}
	attDir := filepath.Join(a.o.DataDir, "spaces", "cc", "instances", adoptWaitInst, attachmentsDir, adoptWaitRun, "att")
	if err := os.MkdirAll(attDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(attDir, "f.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	row, _ := a.j.getRun(ctx, adoptWaitRun) // stopped before start, seeded directly: stop's own end work must not mask adopt's
	row.Desired, row.State, row.Reason = "stopped", proto.RunExited, "stopped before start"
	if err := a.j.updateRun(ctx, row); err != nil {
		t.Fatal(err)
	}
	a.adopt(ctx, a.log, adoptWaitRun, *p)
	a.drain(10 * time.Second)
	if g, err := f.Get(ctx, guard); err == nil && g.State == runner.StateLive {
		t.Fatal("the lease guard is still live")
	}
	if _, err := os.Stat(filepath.Join(attDir, "..")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the run's attachments are still there: %v", err)
	}
}

// A stop before the run has a program retires the lease guard the launch may have started for it.
func TestStopBeforeStartRetiresGuard(t *testing.T) {
	a, f := ccAgent(t, t.TempDir(), nil, nil)
	ctx := context.Background()
	ccAgentSeedRun(t, a)
	guard, err := a.ensureGuard(ctx, ccSpace)
	if err != nil {
		t.Fatal(err)
	}
	a.stop(ctx, adoptWaitRun, "kill", 0, "test")
	a.drain(10 * time.Second)
	if g, err := f.Get(ctx, guard); err == nil && g.State == runner.StateLive {
		t.Fatal("the lease guard is still live")
	}
}

// endHooksFixture starts the space's lease guard and saves an attachment for the run, so a test can tell whether the
// run-end work ran: it retires the guard and removes the attachments.
func endHooksFixture(t *testing.T, a *Agent) (guard, attDir string) {
	t.Helper()
	guard, err := a.ensureGuard(context.Background(), ccSpace)
	if err != nil {
		t.Fatal(err)
	}
	attDir = filepath.Join(a.o.DataDir, "spaces", "cc", "instances", adoptWaitInst, attachmentsDir, adoptWaitRun, "att")
	if err := os.MkdirAll(attDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(attDir, "f.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	return guard, attDir
}

// requireEndHooksRan drains the agent's background work and checks the guard is retired and the attachments gone.
func requireEndHooksRan(t *testing.T, a *Agent, guard, attDir string) {
	t.Helper()
	a.drain(10 * time.Second)
	if g, err := a.o.Runner.Get(context.Background(), guard); err == nil && g.State == runner.StateLive {
		t.Fatal("the lease guard is still live")
	}
	if _, err := os.Stat(filepath.Join(attDir, "..")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the run's attachments are still there: %v", err)
	}
}

// adoptEndedRun is adoptStoppedRun with a live guard and saved attachments, the row ended directly (stop's own end
// work would mask what adopt and observe do).
func adoptEndedRun(t *testing.T, exitAfter int) (a *Agent, ar *asyncKillRunner, id, guard, attDir string) {
	t.Helper()
	a, _ = ccAgent(t, t.TempDir(), nil, func(o *Options) { ar = &asyncKillRunner{Fake: runnertest.New(), exitAfter: exitAfter}; o.Runner = ar })
	ctx := context.Background()
	p, err := ar.Start(ctx, runner.Spec{Name: RunName(adoptWaitRun), Argv: []string{"/bin/true"},
		Labels: map[string]string{"spaceman.managed": "1", "spaceman.run": adoptWaitRun, "spaceman.space": ccSpace}})
	if err != nil {
		t.Fatal(err)
	}
	ccAgentSeedRun(t, a)
	guard, attDir = endHooksFixture(t, a)
	row := getRun(t, a, adoptWaitRun)
	row.Desired, row.State, row.Reason = "stopped", proto.RunExited, "stopped before start"
	if err := a.j.updateRun(ctx, row); err != nil {
		t.Fatal(err)
	}
	a.adopt(ctx, a.log, adoptWaitRun, *p)
	return a, ar, p.ID, guard, attDir
}

// The pending end work runs when the monitor's finished event for the still-live program arrives.
func TestAdoptStoppedRunPendingEndHooksOnFinish(t *testing.T) {
	shortAdoptKillWait(t, 7)
	a, ar, id, guard, attDir := adoptEndedRun(t, -1)
	ar.Exit(id, 129, "killed")
	pr, _ := ar.Fake.Get(context.Background(), id)
	a.observe(context.Background(), getRun(t, a, adoptWaitRun), *pr, false)
	requireEndHooksRan(t, a, guard, attDir)
}

// A program that exits after the last kill-wait read but before the terminal is recorded produces no usable finished
// event: adopt looks once more and runs the end work itself.
func TestAdoptStoppedRunExitsAfterLastWait(t *testing.T) {
	shortAdoptKillWait(t, 7)
	a, _, id, guard, attDir := adoptEndedRun(t, 8) // the 8th Get after the kill is adopt's extra look
	if r := getRun(t, a, adoptWaitRun); r.TerminalID != id {
		t.Fatalf("terminal %q, want %s", r.TerminalID, id)
	}
	if _, ok := a.endPending.Load(adoptWaitRun); ok {
		t.Fatal("the end work is still pending")
	}
	requireEndHooksRan(t, a, guard, attDir)
}

// A launch that fails after the lease guard was started retires the guard and removes the run's attachments.
func TestLaunchFailRunsEndHooks(t *testing.T) {
	a, _ := ccAgent(t, t.TempDir(), nil, nil)
	guard, attDir := endHooksFixture(t, a)
	a.launch(context.Background(), proto.Command{CommandID: "cmd_" + adoptWaitRun, Kind: proto.CmdLaunch, SpaceID: ccSpace, RunID: adoptWaitRun, Generation: 1},
		proto.LaunchPayload{Harness: "no-such-harness", Kind: "worker", Role: "implementor", InstanceID: adoptWaitInst})
	if r := getRun(t, a, adoptWaitRun); r.State != proto.RunFailed {
		t.Fatalf("state %s, want failed", r.State)
	}
	requireEndHooksRan(t, a, guard, attDir)
}

// The runner reconcile ends a pending run with no program (failed) and a running run whose program is gone (lost)
// with the run-end work, not only the attachment removal.
func TestReconcileRunnerEndHooks(t *testing.T) {
	for _, tc := range []struct {
		name    string
		running bool
		want    string
	}{{"pending", false, proto.RunFailed}, {"lost", true, proto.RunLost}} {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := ccAgent(t, t.TempDir(), nil, nil)
			ctx := context.Background()
			ccAgentSeedRun(t, a)
			guard, attDir := endHooksFixture(t, a)
			if tc.running {
				row := getRun(t, a, adoptWaitRun)
				row.State, row.TerminalID = proto.RunRunning, "term_gone"
				if err := a.j.updateRun(ctx, row); err != nil {
					t.Fatal(err)
				}
			}
			if err := a.reconcileRunner(ctx); err != nil {
				t.Fatal(err)
			}
			if r := getRun(t, a, adoptWaitRun); r.State != tc.want {
				t.Fatalf("state %s, want %s", r.State, tc.want)
			}
			requireEndHooksRan(t, a, guard, attDir)
		})
	}
}

// liveGuards lists the space's lease-guard terminals that are still live.
func liveGuards(t *testing.T, f *runnertest.Fake) []runner.Process {
	t.Helper()
	live, err := f.List(context.Background(), map[string]string{"spaceman.guard": ccSpace}, false)
	if err != nil {
		t.Fatal(err)
	}
	return live
}

// A stop that retires the guard before the launch's ensureGuard starts one leaves the guard to the launch: when the
// launch is then abandoned it retires the guard it started, instead of leaking it.
func TestLaunchAbandonedRetiresItsGuard(t *testing.T) {
	a, f := ccAgent(t, t.TempDir(), nil, nil)
	ctx := context.Background()
	space := a.spaceDir("cc")
	if err := os.MkdirAll(space, 0o700); err != nil {
		t.Fatal(err)
	}
	// A file where the workspace folder should be, so the launch fails after ensureGuard ("prepare folders").
	if err := os.WriteFile(filepath.Join(space, "blocked"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	a.beforeEnsureGuard = func() {
		// What a stop does before the launch starts a guard: it ends the row (no program yet) and retires the guard, which
		// is not there. (Written directly, so the stop's own tracked retire cannot run after the guard is started.)
		row := getRun(t, a, "run_leak")
		row.Desired, row.State, row.Reason = "stopped", proto.RunExited, "stopped before start"
		if err := a.j.updateRun(ctx, row); err != nil {
			t.Error(err)
		}
		a.retireGuard(ctx, ccSpace)
	}
	a.launch(ctx, proto.Command{CommandID: "cmd_run_leak", Kind: proto.CmdLaunch, SpaceID: ccSpace, RunID: "run_leak", Generation: 1},
		proto.LaunchPayload{Harness: "claude-code", Kind: "worker", Role: "implementor", InstanceID: "ins_cc", Workspace: "blocked"})
	a.drain(10 * time.Second)
	if r := getRun(t, a, "run_leak"); r.State != proto.RunExited || r.Error != "" {
		t.Fatalf("state %s (%s / %s)", r.State, r.Reason, r.Error)
	}
	if g := liveGuards(t, f); len(g) != 0 {
		t.Fatalf("the abandoned launch leaked %d live guard(s)", len(g))
	}
}

// The runner reconcile re-checks the row under the run's lock before it ends a pending or lost run: a launch or an
// observe that moved the row meanwhile wins, and the reconcile neither overwrites it nor runs the end work again.
func TestReconcileRunnerRechecksUnderRunLock(t *testing.T) {
	for _, tc := range []struct{ name, state, terminal string }{{"pending", proto.RunPending, ""}, {"lost", proto.RunRunning, "term_gone"}} {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := ccAgent(t, t.TempDir(), nil, nil)
			ctx := context.Background()
			ccAgentSeedRun(t, a)
			row := getRun(t, a, adoptWaitRun)
			row.State, row.TerminalID = tc.state, tc.terminal
			if err := a.j.updateRun(ctx, row); err != nil {
				t.Fatal(err)
			}
			var ended atomic.Int32
			a.onRunEndHooks = func(string) { ended.Add(1) }
			unlock := a.runLock(adoptWaitRun) // a launch or an observe is mid-write
			done := make(chan error, 1)
			go func() { done <- a.reconcileRunner(ctx) }()
			time.Sleep(100 * time.Millisecond)
			if r := getRun(t, a, adoptWaitRun); r.State != tc.state {
				t.Fatalf("the reconcile wrote %s without the run's lock", r.State)
			}
			row.State, row.Reason = proto.RunExited, "ended by the lock holder"
			if err := a.j.updateRun(ctx, row); err != nil {
				t.Fatal(err)
			}
			unlock()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if r := getRun(t, a, adoptWaitRun); r.State != proto.RunExited || r.Reason != "ended by the lock holder" {
				t.Fatalf("state %s (%s), want the lock holder's write kept", r.State, r.Reason)
			}
			if n := ended.Load(); n != 0 {
				t.Fatalf("the end work ran %d times for a row the reconcile did not end", n)
			}
		})
	}
}

// Concurrent observes of a finished program (the monitor's and a stop's) run the run-end work exactly once.
func TestObserveEndHooksExactlyOnce(t *testing.T) {
	a, f := ccAgent(t, t.TempDir(), nil, nil)
	ctx := context.Background()
	p, err := f.Start(ctx, runner.Spec{Name: RunName(adoptWaitRun), Argv: []string{"/bin/true"},
		Labels: map[string]string{"spaceman.managed": "1", "spaceman.run": adoptWaitRun, "spaceman.space": ccSpace}})
	if err != nil {
		t.Fatal(err)
	}
	ccAgentSeedRun(t, a)
	row := getRun(t, a, adoptWaitRun)
	row.State, row.TerminalID = proto.RunRunning, p.ID
	if err := a.j.updateRun(ctx, row); err != nil {
		t.Fatal(err)
	}
	f.Exit(p.ID, 0, "exited")
	fin, _ := f.Get(ctx, p.ID)
	var ran atomic.Int32
	a.onRunEndHooks = func(string) { ran.Add(1) }
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			a.observe(ctx, row, *fin, false)
		}()
	}
	close(start)
	wg.Wait()
	a.drain(10 * time.Second)
	if n := ran.Load(); n != 1 {
		t.Fatalf("the run-end work ran %d times, want once", n)
	}
}

// An observe built from a snapshot taken before the run ended (a live program) does not move the ended row back, so
// the program's later finished event does not run the end work a second time.
func TestObserveStaleLiveSnapshotKeepsEndedRow(t *testing.T) {
	a, f := ccAgent(t, t.TempDir(), nil, nil)
	ctx := context.Background()
	p, err := f.Start(ctx, runner.Spec{Name: RunName(adoptWaitRun), Argv: []string{"/bin/true"},
		Labels: map[string]string{"spaceman.managed": "1", "spaceman.run": adoptWaitRun, "spaceman.space": ccSpace}})
	if err != nil {
		t.Fatal(err)
	}
	ccAgentSeedRun(t, a)
	row := getRun(t, a, adoptWaitRun)
	row.State, row.TerminalID = proto.RunRunning, p.ID
	if err := a.j.updateRun(ctx, row); err != nil {
		t.Fatal(err)
	}
	live, _ := f.Get(ctx, p.ID)
	f.Exit(p.ID, 0, "exited")
	fin, _ := f.Get(ctx, p.ID)
	var ran atomic.Int32
	a.onRunEndHooks = func(string) { ran.Add(1) }
	a.observe(ctx, row, *fin, false)  // the stop's observe ends the run
	a.observe(ctx, row, *live, false) // the monitor's stale snapshot
	a.observe(ctx, row, *fin, false)  // the finished event
	a.drain(10 * time.Second)
	if r := getRun(t, a, adoptWaitRun); !ended(r.State) {
		t.Fatalf("state %s, want ended", r.State)
	}
	if n := ran.Load(); n != 1 {
		t.Fatalf("the run-end work ran %d times, want once", n)
	}
}

// Every write that ends a run takes the observe lock after the run's lock (runLock, then observeLock). While another
// writer holds the observe lock mid-step, the reconcile's lost and pending branches wait, then see the row the holder
// ended and neither overwrite it nor run the end work again.
func TestReconcileRunnerEndTakesObserveLock(t *testing.T) {
	for _, tc := range []struct{ name, state, terminal string }{{"pending", proto.RunPending, ""}, {"lost", proto.RunRunning, "term_gone"}} {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := ccAgent(t, t.TempDir(), nil, nil)
			ctx := context.Background()
			ccAgentSeedRun(t, a)
			row := getRun(t, a, adoptWaitRun)
			row.State, row.TerminalID = tc.state, tc.terminal
			if err := a.j.updateRun(ctx, row); err != nil {
				t.Fatal(err)
			}
			var ran atomic.Int32
			a.onRunEndHooks = func(string) { ran.Add(1) }
			unlockObs := a.observeLock(adoptWaitRun) // an observe is between its read and its write
			done := make(chan error, 1)
			go func() { done <- a.reconcileRunner(ctx) }()
			time.Sleep(100 * time.Millisecond)
			if r := getRun(t, a, adoptWaitRun); r.State != tc.state {
				t.Fatalf("the reconcile wrote %s without the observe lock", r.State)
			}
			row.State, row.Reason = proto.RunExited, "ended by the lock holder"
			if err := a.j.updateRun(ctx, row); err != nil {
				t.Fatal(err)
			}
			unlockObs()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			a.drain(10 * time.Second)
			if r := getRun(t, a, adoptWaitRun); r.State != proto.RunExited || r.Reason != "ended by the lock holder" {
				t.Fatalf("state %s (%s), want the lock holder's write kept", r.State, r.Reason)
			}
			if n := ran.Load(); n != 0 {
				t.Fatalf("the end work ran %d times for a row the reconcile did not end", n)
			}
		})
	}
}

// A launch's pre-start failure ends the run under the observe lock too: while an observe holds it mid-step, the failed
// write waits, then the row is written as failed and the end work runs once.
func TestLaunchFailTakesObserveLock(t *testing.T) {
	a, _ := ccAgent(t, t.TempDir(), nil, nil)
	ctx := context.Background()
	const runID = "run_failobs"
	var ran atomic.Int32
	a.onRunEndHooks = func(string) { ran.Add(1) }
	unlockObs := a.observeLock(runID)
	done := make(chan struct{})
	go func() {
		a.launch(ctx, proto.Command{CommandID: "cmd_" + runID, Kind: proto.CmdLaunch, SpaceID: ccSpace, RunID: runID, Generation: 1},
			proto.LaunchPayload{Harness: "no-such-harness", Kind: "worker", Role: "implementor", InstanceID: "ins_cc"})
		close(done)
	}()
	// The launch journals the run before it can fail it: wait for that row, then give the failed write time to land.
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := a.j.getRun(ctx, runID); err == nil || time.Now().After(deadline) {
			break
		}
	}
	time.Sleep(100 * time.Millisecond)
	if r := getRun(t, a, runID); r.State == proto.RunFailed {
		t.Fatalf("the launch failed the run without the observe lock")
	}
	unlockObs()
	<-done
	a.drain(10 * time.Second)
	if r := getRun(t, a, runID); r.State != proto.RunFailed {
		t.Fatalf("state %s (%s / %s), want failed", r.State, r.Reason, r.Error)
	}
	if n := ran.Load(); n != 1 {
		t.Fatalf("the end work ran %d times, want once", n)
	}
}

// A stop before start (no terminal yet) ends the row under the observe lock too: it waits for an observe that is
// between its read and its write, instead of ending the row underneath it.
func TestStopBeforeStartTakesObserveLock(t *testing.T) {
	a, _ := ccAgent(t, t.TempDir(), nil, nil)
	ctx := context.Background()
	ccAgentSeedRun(t, a)
	var ran atomic.Int32
	a.onRunEndHooks = func(string) { ran.Add(1) }
	unlockObs := a.observeLock(adoptWaitRun)
	done := make(chan struct{})
	go func() { a.stop(ctx, adoptWaitRun, "kill", 0, "test"); close(done) }()
	time.Sleep(100 * time.Millisecond)
	if r := getRun(t, a, adoptWaitRun); ended(r.State) || r.Desired != "running" {
		t.Fatalf("the stop wrote %s (desired %s) without the observe lock", r.State, r.Desired)
	}
	unlockObs()
	<-done
	a.drain(10 * time.Second)
	if r := getRun(t, a, adoptWaitRun); r.State != proto.RunExited || r.Reason != "stopped before start" {
		t.Fatalf("state %s (%s)", r.State, r.Reason)
	}
	if n := ran.Load(); n != 1 {
		t.Fatalf("the end work ran %d times, want once", n)
	}
}

// startFails fails the program's start (not the lease guard's), so a launch takes the held fail("start") path.
type startFails struct{ *runnertest.Fake }

func (r *startFails) Start(ctx context.Context, s runner.Spec) (*runner.Process, error) {
	if s.Labels["spaceman.run"] != "" {
		return nil, errors.New("start refused by test")
	}
	return r.Fake.Start(ctx, s)
}

// A start that fails while the launch holds the run's lock fails the run under the observe lock too: it waits for an
// observe between its read and its write (reconcile's found-program branch), instead of failing the row underneath it.
func TestHeldStartFailTakesObserveLock(t *testing.T) {
	a, _ := ccAgent(t, t.TempDir(), nil, func(o *Options) { o.Runner = &startFails{Fake: o.Runner.(*runnertest.Fake)} })
	ctx := context.Background()
	const runID = "run_heldstartfail"
	var ran atomic.Int32
	a.onRunEndHooks = func(string) { ran.Add(1) }
	unlockObs := a.observeLock(runID)
	done := make(chan struct{})
	go func() { ccLaunch(a, runID); close(done) }()
	// The launch writes starting (no observe lock needed) and then blocks in the held fail: wait for that state.
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if r, err := a.j.getRun(ctx, runID); (err == nil && (r.State == proto.RunStarting || r.State == proto.RunFailed)) || time.Now().After(deadline) {
			break
		}
	}
	time.Sleep(100 * time.Millisecond)
	if r := getRun(t, a, runID); r.State == proto.RunFailed {
		t.Fatalf("the held start failure wrote failed without the observe lock")
	}
	unlockObs()
	<-done
	a.drain(10 * time.Second)
	if r := getRun(t, a, runID); r.State != proto.RunFailed || r.Error != "start: start refused by test" {
		t.Fatalf("state %s (%s / %s), want failed with the start error", r.State, r.Reason, r.Error)
	}
	if n := ran.Load(); n != 1 {
		t.Fatalf("the end work ran %d times, want once", n)
	}
}

// A stop of a run the node never received takes the observe lock: it waits for an observe between its read and its
// write instead of journaling the run as stopped underneath it.
func TestEndUnlaunchedTakesObserveLock(t *testing.T) {
	a, _ := ccAgent(t, t.TempDir(), nil, nil)
	ctx := context.Background()
	const runID = "run_endunlaunchedobs"
	unlockObs := a.observeLock(runID)
	done := make(chan bool)
	go func() {
		done <- a.endUnlaunched(ctx, proto.Command{CommandID: "cmd_stop_" + runID, Kind: proto.CmdStop, SpaceID: ccSpace, RunID: runID, Generation: 1})
	}()
	time.Sleep(100 * time.Millisecond)
	if _, err := a.j.getRun(ctx, runID); err == nil {
		t.Fatalf("the stop journaled the run without the observe lock")
	}
	unlockObs()
	if !<-done {
		t.Fatal("the run was known")
	}
	a.drain(10 * time.Second)
	if r := getRun(t, a, runID); r.State != proto.RunExited || r.Desired != "stopped" || r.Reason != "stopped before start" {
		t.Fatalf("state %s (%s), desired %s", r.State, r.Reason, r.Desired)
	}
}
