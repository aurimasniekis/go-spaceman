package app_test

import (
	"context"
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/human"
	"github.com/aurimasniekis/go-spaceman/internal/inbox"
)

// SM-INBOX-001 end to end (ADR-0049): with the space's question policy set to response_inbox routed over HTTP, the
// answer to a question whose requesting run is gone is routed to the operators and also becomes an inbox item of the
// space (source human_response) that names the action but never carries the answer; control is told about the item.
// A policy that omits response_inbox (an older client) is stored as off.
func TestRoutedResponseBecomesInboxItemOverHTTP(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	rootSecret, _, _ := h.c.Auth.IssueRootToken(ctx)
	root := h.client()
	root.bearer = rootSecret
	en, _ := h.c.Fleet.CreateNode(ctx, admin, fleet.CreateNodeInput{Name: "node-a"})
	sp, err := h.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "alpha", Name: "Alpha", NodeID: en.Node.ID})
	if err != nil {
		t.Fatal(err)
	}
	body := map[string]any{"remind_after_minutes": 0, "remind_every_minutes": 0, "max_reminders": 1, "escalate_after_minutes": 0,
		"escalate_priority": "", "escalate_assignee": "", "requester_ended": "reroute", "hold_minutes": 60, "successor_role": "",
		"wake_idle": true, "rate_per_run_hour": 10, "rate_per_space_hour": 100}
	var pol human.Policy
	root.do("PUT", "/spaces/"+sp.ID+"/human-policies/todo", body).json(t, &pol)
	if pol.ResponseInbox != human.ResponseInboxOff {
		t.Fatalf("omitted response_inbox stored as %q", pol.ResponseInbox)
	}
	body["response_inbox"] = "sometimes"
	expect(t, root.do("PUT", "/spaces/"+sp.ID+"/human-policies/question", body), 422)
	body["response_inbox"] = "routed"
	root.do("PUT", "/spaces/"+sp.ID+"/human-policies/question", body).json(t, &pol)
	if pol.ResponseInbox != human.ResponseInboxRouted {
		t.Fatalf("policy %+v", pol)
	}

	const answer = "louilou-the-answer" // never in an ID (Crockford base32 has no i, l, o, u)
	a, _, err := h.c.Human.Create(ctx, admin, sp.ID, human.CreateInput{Type: human.TypeQuestion, Title: "Which region?",
		Requester: human.Requester{Kind: "agent", ID: "ins_gone", Role: "implementor", Run: "run_gone"}})
	if err != nil {
		t.Fatal(err)
	}
	r := root.do("POST", "/spaces/"+sp.ID+"/human-actions/"+a.ID+"/respond", map[string]any{"outcome": "answered", "answer": answer},
		"If-Match", `"1"`)
	expect(t, r, 200)
	var got human.Action
	r.json(t, &got)
	if n := got.Notifications[0]; n.TargetKind != human.TargetOperators || n.RouteReason != human.ReasonRequesterEnded {
		t.Fatalf("routing %+v", n)
	}
	var l struct{ Items []*inbox.Item }
	root.do("GET", "/spaces/"+sp.ID+"/inbox", nil).json(t, &l)
	if len(l.Items) != 1 {
		t.Fatalf("inbox %+v", l.Items)
	}
	it := l.Items[0]
	var full inbox.Item
	raw := root.do("GET", "/spaces/"+sp.ID+"/inbox/"+it.ID, nil)
	raw.json(t, &full)
	if full.SourceKind != inbox.SourceHumanResponse || full.SourceID != a.ID || full.Title != "Response: Which region?" ||
		full.Trusted["routed_to"] != "operators" || full.Trusted["action_id"] != a.ID || strings.Contains(string(raw.body), answer) {
		t.Fatalf("item %s", raw.body)
	}
	eventually(t, "control told about the item", func() bool {
		ms, _ := h.c.Messages.List(ctx, sp.ID, 100)
		for _, m := range ms {
			if m.ToKind == "control" && strings.Contains(m.Body, it.ID) {
				return !strings.Contains(m.Body, answer)
			}
		}
		return false
	})
}
