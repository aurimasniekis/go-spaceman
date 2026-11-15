package app_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	hns "github.com/aurimasniekis/go-spaceman/internal/harness"
	"github.com/aurimasniekis/go-spaceman/internal/human"
)

// nextLaunch makes the space keep questions whose requester ended for the next run of its agent instance.
func (w *wakeEnv) nextLaunch(t *testing.T) {
	t.Helper()
	p := human.Builtin(human.TypeQuestion)
	p.RequesterEnded, p.HoldMinutes, p.WakeIdle = human.EndedNextLaunch, 60, true
	if _, err := w.c.Human.SetPolicy(w.ctx, admin, w.spaceID, human.TypeQuestion, 0, p); err != nil {
		t.Fatal(err)
	}
}

// respond answers actionID and returns where its notification went.
func (w *wakeEnv) respond(t *testing.T, actionID string) human.Notification {
	t.Helper()
	a, err := w.c.Human.Respond(w.ctx, admin, w.spaceID, actionID, 0, human.RespondInput{Outcome: "answered", Response: human.Response{Answer: "eu-west-1"}})
	if err != nil {
		t.Fatal(err)
	}
	return a.Notifications[0]
}

// SM-HUM-009, ADR-0056: with requester_ended next_launch, a response for a control that is no longer running is held
// for control's agent instance. That is waiting work for control: control is auto-started on the same instance and the
// held response is delivered to it, without waiting for the hold to end.
func TestHeldResponseForControlInstanceStartsControl(t *testing.T) {
	t.Parallel()
	w := newWakeEnv(t, 6)
	w.nextLaunch(t)
	asker := w.run1(w.asker)
	n := w.respond(t, w.actionID)
	if n.TargetKind != human.TargetInstance || n.TargetInstance != asker.InstanceID || n.HoldUntil == nil {
		t.Fatalf("not held for control's instance: %+v", n)
	}
	eventually(t, "control started for the held response", func() bool { return len(w.controls()) == 1 })
	started := w.controls()[0]
	if started.CreatedBy != "system:control-autostart" || started.InstanceID != asker.InstanceID {
		t.Fatalf("started by %q on instance %q, want %q", started.CreatedBy, started.InstanceID, asker.InstanceID)
	}
	eventually(t, "held response pinned to the new run", func() bool {
		a, err := w.c.Human.Get(w.ctx, w.spaceID, w.actionID)
		return err == nil && a.Notifications[0].TargetRun == started.ID
	})
	w.noticed(t, started)
}

// A response held for a worker's instance is not work for control: control is not started for it.
func TestHeldResponseForWorkerInstanceDoesNotStartControl(t *testing.T) {
	t.Parallel()
	w := newWakeEnv(t, 6)
	w.nextLaunch(t)
	// A control runs while the worker asks and ends, so the worker's "run ended" message to control starts nothing.
	ctrl, err := w.c.Fleet.LaunchRun(w.ctx, admin, w.spaceID, fleet.LaunchInput{Kind: "control", Role: "control", Harness: hns.KindClaude})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "control running", func() bool { return w.run1(ctrl.ID).Observed == "running" })
	wrk, err := w.c.Fleet.LaunchRun(w.ctx, admin, w.spaceID, fleet.LaunchInput{Kind: "worker", Role: "implementor", Harness: hns.KindClaude})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "worker running", func() bool { return w.run1(wrk.ID).Observed == "running" })
	res := w.bridgeCall(wrk.ID, w.runToken(wrk.ID), "spaceman_human_ask", map[string]any{"type": "question", "title": "Which bucket?"})
	if !res.OK {
		t.Fatalf("ask: %+v", res)
	}
	var asked struct {
		ActionID string `json:"action_id"`
	}
	_ = json.Unmarshal(res.Result, &asked)
	if _, err := w.c.Fleet.StopRun(w.ctx, admin, wrk.ID, "kill", "done"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "worker ended", func() bool { o := w.run1(wrk.ID).Observed; return o == "exited" || o == "failed" })
	// Control has dealt with everything addressed to it; then it stops.
	eventually(t, "run-ended message resolved", func() bool {
		msgs, _ := w.c.Messages.List(w.ctx, w.spaceID, 50)
		for _, m := range msgs {
			if m.ToKind == "control" && m.State != "resolved" {
				_, err := w.c.Messages.Resolve(w.ctx, admin, w.spaceID, m.ID, "seen")
				return err == nil
			}
		}
		return false
	})
	if _, err := w.c.Fleet.StopRun(w.ctx, admin, ctrl.ID, "kill", "done"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "control ended", func() bool { o := w.run1(ctrl.ID).Observed; return o == "exited" || o == "failed" })

	n := w.respond(t, asked.ActionID)
	if n.TargetKind != human.TargetInstance || n.TargetInstance != w.run1(wrk.ID).InstanceID {
		t.Fatalf("not held for the worker's instance: %+v", n)
	}
	for range 3 {
		w.c.Notifier.Kick()
		time.Sleep(500 * time.Millisecond)
	}
	for _, c := range w.controls() {
		if c.ID != ctrl.ID {
			t.Fatalf("control started for a worker's held response: %s by %s", c.ID, c.CreatedBy)
		}
	}
	// It stays held for the worker's next launch.
	if a, err := w.c.Human.Get(w.ctx, w.spaceID, asked.ActionID); err != nil || a.Notifications[0].TargetKind != human.TargetInstance {
		t.Fatalf("held response moved: %+v %v", a, err)
	}
}

// A held response is pinned to its instance's next run when that run launches, not only when something else happens
// to wake the deadline loop afterwards (the run reporting idle, the hold ending an hour later): the launch itself kicks
// the loop. The fake runner reports the new run as working, so nothing else wakes the loop here.
func TestHeldResponsePinnedWhenItsInstanceLaunches(t *testing.T) {
	t.Parallel()
	w := newWakeEnv(t, 6)
	w.nextLaunch(t)
	wrk, err := w.c.Fleet.LaunchRun(w.ctx, admin, w.spaceID, fleet.LaunchInput{Kind: "worker", Role: "implementor", Harness: hns.KindClaude})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "worker running", func() bool { return w.run1(wrk.ID).Observed == "running" })
	res := w.bridgeCall(wrk.ID, w.runToken(wrk.ID), "spaceman_human_ask", map[string]any{"type": "question", "title": "Which bucket?"})
	if !res.OK {
		t.Fatalf("ask: %+v", res)
	}
	var asked struct {
		ActionID string `json:"action_id"`
	}
	_ = json.Unmarshal(res.Result, &asked)
	if _, err := w.c.Fleet.StopRun(w.ctx, admin, wrk.ID, "kill", "done"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "worker ended", func() bool { o := w.run1(wrk.ID).Observed; return o == "exited" || o == "failed" })
	instance := w.run1(wrk.ID).InstanceID
	if n := w.respond(t, asked.ActionID); n.TargetKind != human.TargetInstance || n.TargetInstance != instance {
		t.Fatalf("not held for the worker's instance: %+v", n)
	}
	// Let the rounds the response and the run's end started finish, so that only the launch can wake the loop.
	time.Sleep(time.Second)
	next, err := w.c.Fleet.LaunchRun(w.ctx, admin, w.spaceID, fleet.LaunchInput{Kind: "worker", Role: "implementor", Harness: hns.KindClaude, InstanceID: instance})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "held response pinned to the instance's new run", func() bool {
		a, err := w.c.Human.Get(w.ctx, w.spaceID, asked.ActionID)
		return err == nil && a.Notifications[0].TargetRun == next.ID
	})
	// The node is still starting the run (pinning needs only the pending run): let it finish before cleanup.
	eventually(t, "new run running", func() bool { return w.run1(next.ID).Observed == "running" })
}
