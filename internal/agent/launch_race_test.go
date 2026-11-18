package agent

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
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
		r.Fake.Exit(id, 129, "killed")
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
	p, err := ar.Fake.Start(ctx, runner.Spec{Name: RunName(adoptWaitRun), Argv: []string{"/bin/true"},
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
	_, ar, id, r := adoptStoppedRun(t, -1)
	if r.TerminalID != id || !r.Adopted || !ended(r.State) {
		t.Fatalf("row %s terminal %q adopted %v, want ended with terminal %s", r.State, r.TerminalID, r.Adopted, id)
	}
	if ar.killed[id] != 7 {
		t.Fatalf("polled %d times, want exactly the 7 tries", ar.killed[id])
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
	a.stop(ctx, adoptWaitRun, "kill", 0, "test")
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
