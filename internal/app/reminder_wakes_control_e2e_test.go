package app_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/config"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	hns "github.com/aurimasniekis/go-spaceman/internal/harness"
	"github.com/aurimasniekis/go-spaceman/internal/messages"
)

// reminderEnv is a space with its starter roles and control auto-start on, whose control is running. The messages
// service decides when scheduled messages are due by a fake clock, so a reminder falls due without waiting.
type reminderEnv struct {
	*fleetEnv
	ctx     context.Context
	clk     *clock.Fake
	spaceID string
	ctrl    *fleet.Run
}

func newReminderEnv(t *testing.T, maxAutostarts int64) *reminderEnv {
	e := newFleetEnv(t, func(c *config.Config) { c.Limits.ControlAutostart = 1 })
	clk := clock.NewFake(time.Now())
	e.c.Messages.Clock = clk // before anything reads messages
	e.c.Supervisor.MaxAutostarts = maxAutostarts
	e.enroll()
	e.startAgent()
	ctx := context.Background()
	nodes, _ := e.c.Fleet.ListNodes(ctx)
	eventually(t, "node connected", func() bool { n, _ := e.c.Fleet.GetNode(ctx, nodes[0].ID); return n.Connected })
	sp, err := e.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "remind", Name: "Remind", NodeID: nodes[0].ID, StarterRoles: true})
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
	nctx, stop := context.WithCancel(ctx)
	t.Cleanup(stop)
	go e.c.Notifier.Run(nctx) // Serve runs it; the test central is not served
	return &reminderEnv{fleetEnv: e, ctx: ctx, clk: clk, spaceID: sp.ID, ctrl: ctrl}
}

// schedule has control send itself a reminder through its tool.
func (w *reminderEnv) schedule(t *testing.T, args map[string]any) messages.Message {
	t.Helper()
	args["to"] = "control"
	res := w.bridgeCall(w.ctrl.ID, w.runToken(w.ctrl.ID), "spaceman_message_send", args)
	if !res.OK {
		t.Fatalf("schedule: %+v", res)
	}
	var m messages.Message
	_ = json.Unmarshal(res.Result, &m)
	if m.DeliverAt == nil || m.State != messages.Stored {
		t.Fatalf("not scheduled: %s", res.Result)
	}
	return m
}

func (w *reminderEnv) stopControl(t *testing.T, id string) {
	t.Helper()
	if _, err := w.c.Fleet.StopRun(w.ctx, admin, id, "kill", "agents down"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "control ended", func() bool { o := w.run1(id).Observed; return o == "exited" || o == "failed" })
}

// others lists the space's control runs other than the first.
func (w *reminderEnv) others() []*fleet.Run {
	runs, _ := w.c.Fleet.ListRuns(w.ctx, w.spaceID, 50)
	var out []*fleet.Run
	for _, r := range runs {
		if r.Kind == "control" && r.ID != w.ctrl.ID {
			out = append(out, r)
		}
	}
	return out
}

// due moves the reminder clock past a delay and wakes the notifier.
func (w *reminderEnv) due(d time.Duration) {
	w.clk.Advance(d + time.Second)
	w.c.Notifier.Kick()
}

// told waits for run to be idle and told about one message exactly once, reads it, acknowledges it, and checks it
// is not told again.
func (w *reminderEnv) told(t *testing.T, run *fleet.Run, id string) {
	t.Helper()
	eventually(t, "control running", func() bool { return w.run1(run.ID).Observed == "running" })
	term := w.run1(run.ID).TerminalID
	w.run.SetStatus(term, "idle")
	w.c.Notifier.Kick()
	eventually(t, "control told about the reminder", func() bool { return len(w.run.Inputs(term)) >= 1 })
	if in := w.run.Inputs(term); len(in) != 1 || !strings.Contains(in[0], "1 new message") || strings.Contains(in[0], "Check the deploy") {
		t.Fatalf("notices %q", in)
	}
	var inbox messages.Inbox
	res := w.bridgeCall(run.ID, w.runToken(run.ID), "spaceman_messages", nil)
	_ = json.Unmarshal(res.Result, &inbox)
	if len(inbox.Messages) != 1 || inbox.Messages[0].ID != id || inbox.Messages[0].DeliverAt == nil {
		t.Fatalf("control inbox %s", res.Result)
	}
	if res := w.bridgeCall(run.ID, w.runToken(run.ID), "spaceman_message_ack", map[string]any{"id": id}); !res.OK {
		t.Fatalf("ack %+v", res)
	}
	w.run.SetStatus(term, "working")
	w.run.SetStatus(term, "idle")
	w.due(time.Hour)
	time.Sleep(time.Second)
	if in := w.run.Inputs(term); len(in) != 1 {
		t.Fatalf("told again after the ack: %q", in)
	}
}

// ADR-0065, SM-CTRL-002: control schedules a reminder to itself and stops. Nothing happens before it is due; when it
// is, control is auto-started for it and told about it exactly once.
func TestReminderStartsControl(t *testing.T) {
	t.Parallel()
	w := newReminderEnv(t, 6)
	m := w.schedule(t, map[string]any{"subject": "Check the deploy", "delay": "6h", "dedup_key": "deploy"})
	// Re-scheduling with the same key returns the pending reminder.
	if again := w.schedule(t, map[string]any{"subject": "Check the deploy", "delay": "7h", "dedup_key": "deploy"}); again.ID != m.ID {
		t.Fatalf("dedup: %s vs %s", again.ID, m.ID)
	}
	w.stopControl(t, w.ctrl.ID)
	w.due(5 * time.Hour)
	time.Sleep(time.Second)
	if c := w.others(); len(c) != 0 {
		t.Fatalf("control started before the reminder was due: %+v", c[0])
	}
	w.due(time.Hour)
	eventually(t, "control started for the reminder", func() bool { return len(w.others()) == 1 })
	started := w.others()[0]
	if started.CreatedBy != "system:control-autostart" {
		t.Fatalf("started by %q", started.CreatedBy)
	}
	w.told(t, started, m.ID)
}

// ADR-0065: a running control that scheduled a reminder is woken by it when it is due (a run otherwise never receives
// its own messages).
func TestReminderWakesRunningControl(t *testing.T) {
	t.Parallel()
	w := newReminderEnv(t, 6)
	m := w.schedule(t, map[string]any{"subject": "Check the deploy", "deliver_at": w.clk.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339)})
	term := w.run1(w.ctrl.ID).TerminalID
	w.run.SetStatus(term, "idle")
	w.c.Notifier.Kick()
	time.Sleep(time.Second)
	if in := w.run.Inputs(term); len(in) != 0 {
		t.Fatalf("told before due: %q", in)
	}
	w.due(10 * time.Minute)
	w.told(t, w.ctrl, m.ID)
	if c := w.others(); len(c) != 0 {
		t.Fatalf("another control started: %+v", c[0])
	}
}

// ADR-0065: a reminder cancelled before it is due (by control's tool, or by an operator) is never delivered and
// starts nothing; the pending list shows what is left.
func TestReminderCancelled(t *testing.T) {
	t.Parallel()
	w := newReminderEnv(t, 6)
	a := w.schedule(t, map[string]any{"subject": "A", "delay": "1h"})
	b := w.schedule(t, map[string]any{"subject": "B", "delay": "2h"})
	var list struct {
		Items []messages.Message `json:"items"`
	}
	res := w.bridgeCall(w.ctrl.ID, w.runToken(w.ctrl.ID), "spaceman_message_scheduled", nil)
	_ = json.Unmarshal(res.Result, &list)
	if len(list.Items) != 2 || list.Items[0].ID != a.ID {
		t.Fatalf("scheduled %s", res.Result)
	}
	if res := w.bridgeCall(w.ctrl.ID, w.runToken(w.ctrl.ID), "spaceman_message_cancel", map[string]any{"message_id": a.ID}); !res.OK {
		t.Fatalf("cancel %+v", res)
	}
	if _, err := w.c.Messages.Cancel(w.ctx, admin, w.spaceID, b.ID); err != nil {
		t.Fatal(err)
	}
	if l, _ := w.c.Messages.Scheduled(w.ctx, w.spaceID); len(l) != 0 {
		t.Fatalf("still scheduled %+v", l)
	}
	w.stopControl(t, w.ctrl.ID)
	w.due(3 * time.Hour)
	time.Sleep(time.Second)
	if c := w.others(); len(c) != 0 {
		t.Fatalf("control started for a cancelled reminder: %+v", c[0])
	}
}

// ADR-0065: a reminder that falls due while control cannot be auto-started (its starts are used up) waits; the control
// a person launches later is told about it exactly once.
func TestReminderReachesAControlStartedLater(t *testing.T) {
	t.Parallel()
	w := newReminderEnv(t, 0)
	m := w.schedule(t, map[string]any{"subject": "Check the deploy", "delay": "1m"})
	w.stopControl(t, w.ctrl.ID)
	w.due(time.Minute)
	time.Sleep(time.Second)
	if c := w.others(); len(c) != 0 {
		t.Fatalf("control started past its limit: %+v", c[0])
	}
	ctrl, err := w.c.Fleet.LaunchRun(w.ctx, admin, w.spaceID, fleet.LaunchInput{Kind: "control", Role: "control", Harness: hns.KindClaude})
	if err != nil {
		t.Fatal(err)
	}
	w.told(t, ctrl, m.ID)
}
