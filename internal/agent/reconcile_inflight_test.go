package agent

import (
	"context"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/runner"
	"github.com/aurimasniekis/go-spaceman/internal/runner/runnertest"
)

// reconcileAtStart runs a runner reconcile (what the monitor does on the runner's "snapshot" event) while a launch is in
// flight: right before the program starts, so the journal holds the run but the runner does not yet.
type reconcileAtStart struct {
	*runnertest.Fake
	before func()
}

func (r *reconcileAtStart) Start(ctx context.Context, s runner.Spec) (*runner.Process, error) {
	r.before()
	return r.Fake.Start(ctx, s)
}

// The monitor's reconcile of the journal against the runner must not judge a run whose launch is still in flight: its
// process is not in the runner yet, and a reconcile that marks it lost or failed ends a run that is about to start (the
// launch then abandons it, and every bridge call of the run answers "run is not active").
func TestReconcileRunnerLeavesARunWhoseLaunchIsInFlight(t *testing.T) {
	dir := t.TempDir()
	var a *Agent
	var f *runnertest.Fake
	a, f = ccAgent(t, dir, nil, func(o *Options) {
		fk := o.Runner.(*runnertest.Fake)
		o.Runner = &reconcileAtStart{Fake: fk, before: func() {
			if err := a.reconcileRunner(context.Background()); err != nil {
				t.Error(err)
			}
		}}
	})
	_ = f
	ccLaunch(a, "run_inflight")
	if r := getRun(t, a, "run_inflight"); r.State != proto.RunRunning {
		t.Fatalf("run %s (%s %s), want running", r.State, r.Reason, r.Error)
	}
}

// listHook runs a hook after the wrapped runner answers a List, so a test can change the world between a reconcile's
// reads of the runner and of the journal.
type listHook struct {
	*runnertest.Fake
	after func(labels map[string]string)
}

func (r *listHook) List(ctx context.Context, labels map[string]string, all bool) ([]runner.Process, error) {
	out, err := r.Fake.List(ctx, labels, all)
	r.after(labels)
	return out, err
}

// A launch that completes between the reconcile's runner List and its journal read leaves a run the first List did not
// show. The reconcile must look at the runner again for it, or it marks the live run lost.
func TestReconcileRunnerRelistFindsARunLaunchedAfterTheFirstList(t *testing.T) {
	var a *Agent
	done := false
	a, _ = ccAgent(t, t.TempDir(), nil, func(o *Options) {
		o.Runner = &listHook{Fake: o.Runner.(*runnertest.Fake), after: func(l map[string]string) {
			if l["spaceman.managed"] == "1" && !done {
				done = true
				ccLaunch(a, "run_relist") // finishes entirely before the reconcile reads the journal
			}
		}}
	})
	if err := a.reconcileRunner(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !done {
		t.Fatal("the launch hook never ran")
	}
	if r := getRun(t, a, "run_relist"); r.State != proto.RunRunning {
		t.Fatalf("run %s (%s %s), want running", r.State, r.Reason, r.Error)
	}
}

// A run that ends between the reconcile's journal read and the fresh re-read is already settled: the reconcile must not
// overwrite it with lost or failed.
func TestReconcileRunnerKeepsARunThatEndedAfterTheJournalRead(t *testing.T) {
	var a *Agent
	armed := false
	a, _ = ccAgent(t, t.TempDir(), nil, func(o *Options) {
		o.Runner = &listHook{Fake: o.Runner.(*runnertest.Fake), after: func(l map[string]string) {
			if l["spaceman.run"] == "run_ended" && armed {
				armed = false
				r, err := a.j.getRun(context.Background(), "run_ended")
				if err != nil {
					t.Error(err)
					return
				}
				r.State = proto.RunExited
				if err := a.j.updateRun(context.Background(), r); err != nil {
					t.Error(err)
				}
			}
		}}
	})
	ctx := context.Background()
	row := runRow{RunID: "run_ended", SpaceID: ccSpace, Generation: 1, Kind: "worker", Role: "implementor", Harness: "claude-code",
		InstanceID: "ins_cc", TerminalName: RunName("run_ended"), Desired: "running", State: proto.RunPending}
	if err := a.j.insertRun(ctx, row); err != nil {
		t.Fatal(err)
	}
	row.State = proto.RunRunning
	if err := a.j.updateRun(ctx, row); err != nil {
		t.Fatal(err)
	}
	armed = true
	if err := a.reconcileRunner(ctx); err != nil {
		t.Fatal(err)
	}
	if armed {
		t.Fatal("the re-list never happened")
	}
	if r := getRun(t, a, "run_ended"); r.State != proto.RunExited {
		t.Fatalf("run %s (%s %s), want exited", r.State, r.Reason, r.Error)
	}
}

// A launch that fails must clear its in-flight mark, or the reconcile skips the run forever.
func TestLaunchClearsItsInFlightMarkWhenItFails(t *testing.T) {
	a, _ := ccAgent(t, t.TempDir(), nil, nil)
	delete(a.leases, ccSpace)
	ccLaunch(a, "run_nolease")
	if r := getRun(t, a, "run_nolease"); r.State != proto.RunFailed {
		t.Fatalf("run %s, want failed", r.State)
	}
	if a.launchRunning("run_nolease") {
		t.Fatal("launchRunning is still true after the launch failed")
	}
}
