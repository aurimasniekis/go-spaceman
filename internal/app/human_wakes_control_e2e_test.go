package app_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/config"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	hns "github.com/aurimasniekis/go-spaceman/internal/harness"
	"github.com/aurimasniekis/go-spaceman/internal/human"
	"github.com/aurimasniekis/go-spaceman/internal/messages"
)

// wakeEnv is a space with its starter roles and control auto-start on, whose control asked a person a question and was
// then stopped: no control runs when the answer comes.
type wakeEnv struct {
	*fleetEnv
	ctx      context.Context
	spaceID  string
	actionID string
	asker    string
}

func newWakeEnv(t *testing.T, maxAutostarts int64) *wakeEnv {
	e := newFleetEnv(t, func(c *config.Config) { c.Limits.ControlAutostart = 1 })
	e.c.Supervisor.MaxAutostarts = maxAutostarts // set before the sweep runs
	e.enroll()
	e.startAgent()
	ctx := context.Background()
	nodes, _ := e.c.Fleet.ListNodes(ctx)
	eventually(t, "node connected", func() bool { n, _ := e.c.Fleet.GetNode(ctx, nodes[0].ID); return n.Connected })
	sp, err := e.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "wake", Name: "Wake", NodeID: nodes[0].ID, StarterRoles: true})
	if err != nil {
		t.Fatal(err)
	}
	bindStubAccounts(t, e.c, sp.ID)
	eventually(t, "lease granted", func() bool { s, _ := e.c.Fleet.GetSpace(ctx, sp.ID); return s.Lease != nil && s.Lease.Valid })
	ctrl, err := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Kind: "control", Role: "control", Harness: hns.KindClaude})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "control running", func() bool { return e.run1(ctrl.ID).Observed == "running" })
	res := e.bridgeCall(ctrl.ID, e.runToken(ctrl.ID), "spaceman_human_ask", map[string]any{"type": "question", "title": "Which region?"})
	if !res.OK {
		t.Fatalf("ask: %+v", res)
	}
	var asked struct {
		ActionID string `json:"action_id"`
	}
	_ = json.Unmarshal(res.Result, &asked)
	// The control stops (all agents down, or its idle stop): nothing of kind control is live any more.
	if _, err := e.c.Fleet.StopRun(ctx, admin, ctrl.ID, "kill", "agents down"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "control ended", func() bool { o := e.run1(ctrl.ID).Observed; return o == "exited" || o == "failed" })
	nctx, stop := context.WithCancel(ctx)
	t.Cleanup(stop)
	go e.c.Notifier.Run(nctx) // Serve runs both; the test central is not served
	go e.c.Human.Run(nctx)
	return &wakeEnv{fleetEnv: e, ctx: ctx, spaceID: sp.ID, actionID: asked.ActionID, asker: ctrl.ID}
}

// answer responds to the question; the response goes to control with its provenance.
func (w *wakeEnv) answer(t *testing.T) {
	t.Helper()
	a, err := w.c.Human.Respond(w.ctx, admin, w.spaceID, w.actionID, 0, human.RespondInput{Outcome: "answered", Response: human.Response{Answer: "eu-west-1"}})
	if err != nil {
		t.Fatal(err)
	}
	if n := a.Notifications[0]; n.TargetKind != human.TargetControl || n.RouteReason != human.ReasonRequesterEnded || n.OriginalRun != w.asker {
		t.Fatalf("routed %+v", n)
	}
}

// controls lists the space's control runs other than the asker.
func (w *wakeEnv) controls() []*fleet.Run {
	runs, _ := w.c.Fleet.ListRuns(w.ctx, w.spaceID, 50)
	var out []*fleet.Run
	for _, r := range runs {
		if r.Kind == "control" && r.ID != w.asker {
			out = append(out, r)
		}
	}
	return out
}

// noticed waits for run to be idle and told about the response exactly once, reads and acknowledges it, and checks
// it is not told again.
func (w *wakeEnv) noticed(t *testing.T, run *fleet.Run) {
	t.Helper()
	eventually(t, "control running", func() bool { return w.run1(run.ID).Observed == "running" })
	term := w.run1(run.ID).TerminalID
	w.run.SetStatus(term, "idle")
	w.c.Notifier.Kick()
	eventually(t, "control told about the response", func() bool { return len(w.run.Inputs(term)) >= 1 })
	if in := w.run.Inputs(term); len(in) != 1 || !strings.Contains(in[0], "1 human response") || strings.Contains(in[0], "eu-west-1") {
		t.Fatalf("notices %q", in)
	}
	var inbox messages.Inbox
	res := w.bridgeCall(run.ID, w.runToken(run.ID), "spaceman_messages", nil)
	_ = json.Unmarshal(res.Result, &inbox)
	if len(inbox.Human) != 1 || inbox.Human[0].OriginalRun != w.asker || inbox.Human[0].RouteReason != human.ReasonRequesterEnded {
		t.Fatalf("control inbox %+v", res)
	}
	if res := w.bridgeCall(run.ID, w.runToken(run.ID), "spaceman_message_ack", map[string]any{"id": inbox.Human[0].ID}); !res.OK {
		t.Fatalf("ack %+v", res)
	}
	w.run.SetStatus(term, "working")
	w.run.SetStatus(term, "idle")
	w.c.Notifier.Kick()
	time.Sleep(time.Second)
	if in := w.run.Inputs(term); len(in) != 1 {
		t.Fatalf("told again after the ack: %q", in)
	}
}

// SM-HUM-009, SM-CTRL-002, ADR-0056: a response for a control that is no longer running is work for control. Control
// is auto-started for it and told about it once; after the acknowledgement no later control is told again.
func TestHumanResponseStartsControl(t *testing.T) {
	w := newWakeEnv(t, 6)
	w.answer(t)
	eventually(t, "control started for the response", func() bool { return len(w.controls()) == 1 })
	started := w.controls()[0]
	if started.CreatedBy != "system:control-autostart" {
		t.Fatalf("started by %q", started.CreatedBy)
	}
	w.noticed(t, started)

	// Stopped again and launched by hand: the acknowledged response is not announced again.
	if _, err := w.c.Fleet.StopRun(w.ctx, admin, started.ID, "kill", "done"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "control ended", func() bool { o := w.run1(started.ID).Observed; return o == "exited" || o == "failed" })
	again, err := w.c.Fleet.LaunchRun(w.ctx, admin, w.spaceID, fleet.LaunchInput{Kind: "control", Role: "control", Harness: hns.KindClaude})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "control running", func() bool { return w.run1(again.ID).Observed == "running" })
	w.run.SetStatus(w.run1(again.ID).TerminalID, "idle")
	w.c.Notifier.Kick()
	time.Sleep(time.Second)
	if in := w.run.Inputs(w.run1(again.ID).TerminalID); len(in) != 0 {
		t.Fatalf("a later control was told about an acknowledged response: %q", in)
	}
}

// A response that arrives while control cannot be auto-started (here: its starts for the hour are used up) waits for
// control; the control a person launches later is told about it exactly once.
func TestHumanResponseReachesAControlStartedLater(t *testing.T) {
	w := newWakeEnv(t, 0)
	w.answer(t)
	time.Sleep(time.Second)
	if c := w.controls(); len(c) != 0 {
		t.Fatalf("control started past its limit: %+v", c[0])
	}
	ctrl, err := w.c.Fleet.LaunchRun(w.ctx, admin, w.spaceID, fleet.LaunchInput{Kind: "control", Role: "control", Harness: hns.KindClaude})
	if err != nil {
		t.Fatal(err)
	}
	w.noticed(t, ctrl)
}
