package agent

import (
	"context"
	"testing"

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
