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
