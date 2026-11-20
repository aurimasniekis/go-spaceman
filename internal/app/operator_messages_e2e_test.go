package app_test

import (
	"context"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/api"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/messages"
)

// GET /messages over HTTP: operator-addressed messages across the spaces the caller can read, never another space's
// and never messages to control or runs; paging by cursor is stable; the unread count follows the readable spaces.
func TestOperatorMessagesOverHTTP(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	rootSecret, _, _ := h.c.Auth.IssueRootToken(ctx)
	root := h.client()
	root.bearer = rootSecret
	en, _ := h.c.Fleet.CreateNode(ctx, admin, fleet.CreateNodeInput{Name: "node-a"})
	a, err := h.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "alpha", Name: "Alpha", NodeID: en.Node.ID})
	if err != nil {
		t.Fatal(err)
	}
	b, err := h.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "beta", Name: "Beta", NodeID: en.Node.ID})
	if err != nil {
		t.Fatal(err)
	}
	from := messages.From{Kind: "run", ID: "run_e2e", Label: "agent"}
	send := func(space, to, kind, subject string) *messages.Message {
		t.Helper()
		m, _, err := h.c.Messages.Send(ctx, admin, space, from, messages.SendInput{To: to, Kind: kind, Subject: subject, Body: "x"})
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	var aIDs []string
	for _, s := range []string{"a1", "a2", "a3", "a4", "a5"} {
		aIDs = append(aIDs, send(a.ID, "operators", "message", s).ID)
	}
	req := send(a.ID, "operators", "request", "a-request")
	send(a.ID, "control", "message", "a-control")
	send(a.ID, "role:worker", "message", "a-role")
	bMsg := send(b.ID, "operators", "message", "b1")
	send(b.ID, "control", "message", "b-control")

	list := func(c *client, query string) api.OperatorMessageList {
		t.Helper()
		var l api.OperatorMessageList
		c.do("GET", "/messages"+query, nil).json(t, &l)
		return l
	}

	// Root reads both spaces: only operator messages, newest first, with the space named.
	l := list(root, "?limit=200")
	if len(l.Items) != 7 || l.UnreadCount != 7 {
		t.Fatalf("root items %d unread %d", len(l.Items), l.UnreadCount)
	}
	for _, it := range l.Items {
		if it.ToKind != "operators" || it.SpaceName == "" || it.SpaceSlug == "" {
			t.Fatalf("bad item %+v", it.Message)
		}
	}
	if l.Items[0].ID != bMsg.ID || l.Items[0].SpaceName != "Beta" {
		t.Fatalf("newest first: %+v", l.Items[0].Message)
	}

	// Paging is stable: pages of 3 return every message exactly once, in order.
	seen := map[string]bool{}
	var order []string
	cur := ""
	for i := 0; i < 10; i++ {
		q := "?limit=3"
		if cur != "" {
			q += "&cursor=" + cur
		}
		p := list(root, q)
		for _, it := range p.Items {
			if seen[it.ID] {
				t.Fatalf("message %s repeated across pages", it.ID)
			}
			seen[it.ID] = true
			order = append(order, it.ID)
		}
		if p.NextCursor == "" {
			break
		}
		cur = p.NextCursor
	}
	if len(order) != 7 {
		t.Fatalf("paged %d messages, want 7", len(order))
	}
	for i := range order {
		if order[i] != l.Items[i].ID {
			t.Fatalf("page order differs at %d", i)
		}
	}
	expect(t, root.do("GET", "/messages?cursor=garbage", nil), 422)
	expect(t, root.do("GET", "/messages?limit=500", nil), 422)

	// Filters: kind, space (ID or slug) and state.
	if k := list(root, "?kind=request"); len(k.Items) != 1 || k.Items[0].ID != req.ID {
		t.Fatalf("kind filter %+v", k.Items)
	}
	if s := list(root, "?space=beta"); len(s.Items) != 1 || s.Items[0].ID != bMsg.ID || s.UnreadCount != 7 {
		t.Fatalf("space filter %d unread %d", len(s.Items), s.UnreadCount)
	}
	expect(t, root.do("POST", "/spaces/"+a.ID+"/messages/"+aIDs[0]+"/ack", nil), 200)
	if o := list(root, "?limit=200"); len(o.Items) != 6 || o.UnreadCount != 6 {
		t.Fatalf("open after ack: %d unread %d", len(o.Items), o.UnreadCount)
	}
	if all := list(root, "?state=all&limit=200"); len(all.Items) != 7 || all.UnreadCount != 6 {
		t.Fatalf("all after ack: %d unread %d", len(all.Items), all.UnreadCount)
	}

	// A user who reads only space A sees none of space B's messages, and no messages to control or roles.
	expect(t, root.do("POST", "/users", map[string]any{"email": "vic@example.com", "display_name": "Vic", "password": "correct horse battery staple",
		"grants": []map[string]string{{"role": "viewer", "domain": a.ID}}}), 201)
	viewer := h.client()
	viewer.bearer = loginAndIssueToken(t, h, "vic@example.com", []map[string]string{{"role": "viewer", "domain": a.ID}})
	v := list(viewer, "?state=all&limit=200")
	if len(v.Items) != 6 || v.UnreadCount != 5 {
		t.Fatalf("viewer items %d unread %d", len(v.Items), v.UnreadCount)
	}
	for _, it := range v.Items {
		if it.SpaceID != a.ID || it.ToKind != "operators" {
			t.Fatalf("viewer saw %+v", it.Message)
		}
	}
	if s := list(viewer, "?space="+b.ID+"&state=all"); len(s.Items) != 0 {
		t.Fatalf("unreadable space leaked %d messages", len(s.Items))
	}
	expect(t, h.client().do("GET", "/messages", nil), 401)
}
