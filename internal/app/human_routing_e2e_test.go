package app_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	hns "github.com/aurimasniekis/go-spaceman/internal/harness"
	"github.com/aurimasniekis/go-spaceman/internal/human"
	"github.com/aurimasniekis/go-spaceman/internal/messages"
)

// SM-HUM-009 end to end: a worker asks a person; the answer is stored for the worker's run; central restarts and the
// worker's run ends before it read the answer. The answer survives, is routed to control with its provenance, wakes
// control when it is idle, and is never shown to another worker.
func TestHumanResponseRoutedWhenTheRequesterEnds(t *testing.T) {
	// Not parallel: central restarts must finish within the node lease, which parallel tests can starve.
	e := newFleetEnv(t)
	e.enroll()
	e.startAgent()
	ctx := context.Background()
	nodes, _ := e.c.Fleet.ListNodes(ctx)
	nodeID := nodes[0].ID
	eventually(t, "node connected", func() bool { n, _ := e.c.Fleet.GetNode(ctx, nodeID); return n.Connected })
	sp, err := e.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "route", Name: "Route", NodeID: nodeID})
	if err != nil {
		t.Fatal(err)
	}
	bindStubAccounts(t, e.c, sp.ID)
	eventually(t, "lease granted", func() bool { s, _ := e.c.Fleet.GetSpace(ctx, sp.ID); return s.Lease != nil && s.Lease.Valid })
	ctrl, _ := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Kind: "control", Role: "control", Harness: hns.KindClaude})
	wrk, _ := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Kind: "worker", Role: "implementor", Harness: hns.KindClaude})
	peer, _ := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Kind: "worker", Role: "implementor", Harness: hns.KindClaude})
	eventually(t, "runs running", func() bool {
		return e.run1(ctrl.ID).Observed == "running" && e.run1(wrk.ID).Observed == "running" && e.run1(peer.ID).Observed == "running"
	})
	term := func(runID string) string { return e.run1(runID).TerminalID }
	e.run.SetStatus(term(ctrl.ID), "working")
	e.run.SetStatus(term(peer.ID), "idle")

	res := e.bridgeCall(wrk.ID, e.runToken(wrk.ID), "spaceman_human_ask", map[string]any{"type": "question", "title": "Which region?"})
	if !res.OK {
		t.Fatalf("ask: %+v", res)
	}
	var asked struct {
		ActionID string `json:"action_id"`
	}
	_ = json.Unmarshal(res.Result, &asked)
	a, err := e.c.Human.Respond(ctx, admin, sp.ID, asked.ActionID, 0, human.RespondInput{Outcome: "answered", Response: human.Response{Answer: "eu-west-1"}})
	if err != nil {
		t.Fatal(err)
	}
	if n := a.Notifications[0]; n.TargetKind != human.TargetRun || n.TargetRun != wrk.ID {
		t.Fatalf("for the live requester %+v", n)
	}

	e.restartCentral()
	eventually(t, "reconnected after central restart", func() bool { n, _ := e.c.Fleet.GetNode(ctx, nodeID); return n.Connected })
	if _, err := e.c.Fleet.StopRun(ctx, admin, wrk.ID, "kill", "session lost"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "worker ended", func() bool { return e.run1(wrk.ID).Observed == "exited" || e.run1(wrk.ID).Observed == "failed" })
	nctx, stop := context.WithCancel(ctx)
	t.Cleanup(stop)
	go e.c.Notifier.Run(nctx)
	go e.c.Human.Run(nctx) // Serve runs both; the test central is not served
	eventually(t, "routed to control", func() bool {
		g, _ := e.c.Human.Get(ctx, sp.ID, asked.ActionID)
		return g.Notifications[0].TargetKind == human.TargetControl
	})
	g, _ := e.c.Human.Get(ctx, sp.ID, asked.ActionID)
	if n := g.Notifications[0]; n.OriginalRun != wrk.ID || n.RoutedFrom != "run:"+wrk.ID || n.RouteReason != human.ReasonRequesterEnded {
		t.Fatalf("provenance %+v", n)
	}
	// Control is woken once idle; the notice carries no content.
	e.run.SetStatus(term(ctrl.ID), "done")
	eventually(t, "notice typed into control", func() bool { return len(e.run.Inputs(term(ctrl.ID))) == 1 })
	if in := e.run.Inputs(term(ctrl.ID))[0]; !strings.Contains(in, "1 human response") || strings.Contains(in, "eu-west-1") {
		t.Fatalf("notice %q", in)
	}
	var inbox messages.Inbox
	res = e.bridgeCall(ctrl.ID, e.runToken(ctrl.ID), "spaceman_messages", nil)
	_ = json.Unmarshal(res.Result, &inbox)
	if len(inbox.Human) != 1 || inbox.Human[0].OriginalRun != wrk.ID || inbox.Human[0].RouteReason != human.ReasonRequesterEnded {
		t.Fatalf("control inbox %+v", res)
	}
	var seen human.Action
	res = e.bridgeCall(ctrl.ID, e.runToken(ctrl.ID), "spaceman_human_get", map[string]any{"action_id": asked.ActionID})
	_ = json.Unmarshal(res.Result, &seen)
	if seen.ResponseHidden || !strings.Contains(string(seen.Response), "eu-west-1") {
		t.Fatalf("control reads the response: %+v", res)
	}
	// Another worker of the same role never sees it.
	res = e.bridgeCall(peer.ID, e.runToken(peer.ID), "spaceman_messages", nil)
	if !res.OK || strings.Contains(string(res.Result), asked.ActionID) {
		t.Fatalf("peer inbox %+v", res)
	}
	res = e.bridgeCall(peer.ID, e.runToken(peer.ID), "spaceman_human_get", map[string]any{"action_id": asked.ActionID})
	if !res.OK || strings.Contains(string(res.Result), "eu-west-1") {
		t.Fatalf("peer reads the response: %+v", res)
	}
	if len(e.run.Inputs(term(peer.ID))) != 0 {
		t.Fatalf("peer noticed: %v", e.run.Inputs(term(peer.ID)))
	}
	if res := e.bridgeCall(ctrl.ID, e.runToken(ctrl.ID), "spaceman_message_ack", map[string]any{"id": inbox.Human[0].ID}); !res.OK {
		t.Fatalf("ack %+v", res)
	}
}
