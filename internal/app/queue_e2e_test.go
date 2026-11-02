package app_test

import (
	"context"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/api"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/inbox"
	"github.com/aurimasniekis/go-spaceman/internal/queue"
)

// SM-QUEUE-*, SM-INBOX-003 over HTTP: an inbox item becomes a task (the item is linked and done), operator moves,
// the next eligible task with skip reasons, If-Match on state changes, and read-only viewers.
func TestQueueOverHTTP(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	rootSecret, _, _ := h.c.Auth.IssueRootToken(ctx)
	root := h.client()
	root.bearer = rootSecret
	actor := audit.Actor{Type: audit.ActorUser, ID: "usr_test"}
	en, _ := h.c.Fleet.CreateNode(ctx, actor, fleet.CreateNodeInput{Name: "node-a"})
	sp, err := h.c.Fleet.CreateSpace(ctx, actor, fleet.CreateSpaceInput{Slug: "alpha", Name: "Alpha", NodeID: en.Node.ID})
	if err != nil {
		t.Fatal(err)
	}
	base := "/spaces/" + sp.ID

	var item inbox.Item
	root.do("POST", base+"/inbox", map[string]any{"body": "Add rate limiting to the refunds API"}).json(t, &item)
	r := root.do("POST", base+"/tasks", map[string]any{"title": "Rate-limit refunds", "status": "ready", "from_inbox_item": item.ID,
		"acceptance": "- [ ] 429 after 100 requests/min"})
	expect(t, r, 201)
	var task queue.Task
	r.json(t, &task)
	if task.OriginKind != "inbox_item" || task.OriginID != item.ID || r.header.Get("ETag") != `"1"` {
		t.Fatalf("task %+v", task)
	}
	var linked inbox.Item
	root.do("GET", base+"/inbox/"+item.ID, nil).json(t, &linked)
	if linked.Status != "done" || linked.LinkedKind != "task" || linked.LinkedID != task.ID {
		t.Fatalf("inbox item not linked: %+v", linked)
	}

	var b queue.Task
	root.do("POST", base+"/tasks", map[string]any{"title": "Upgrade Go", "status": "ready", "priority": "high"}).json(t, &b)
	var q api.QueueOut
	root.do("GET", base+"/tasks", nil).json(t, &q)
	if len(q.Tasks) != 2 || q.Tasks[0].ID != b.ID || q.Policy.MaxActive != 1 {
		t.Fatalf("queue %+v", q)
	}
	expect(t, root.do("POST", base+"/tasks/"+b.ID+"/move", map[string]any{"to": "bottom"}), 200)
	root.do("GET", base+"/tasks", nil).json(t, &q)
	if q.Tasks[0].ID != task.ID || !q.Tasks[1].Manual {
		t.Fatalf("after move %+v", q.Tasks)
	}
	expect(t, root.do("POST", base+"/tasks/"+task.ID+"/blockers", map[string]any{"kind": "external", "note": "waiting for the API gateway upgrade"}), 200)
	var sel queue.Selection
	root.do("GET", base+"/queue/next?record=true", nil).json(t, &sel)
	if sel.Selected == nil || sel.Selected.ID != b.ID || len(sel.Skipped) != 1 || sel.Skipped[0].TaskID != task.ID {
		t.Fatalf("next %+v", sel)
	}
	expect(t, root.do("POST", base+"/tasks/"+b.ID+"/status", map[string]any{"status": "running"}), 428)
	expect(t, root.do("POST", base+"/tasks/"+b.ID+"/status", map[string]any{"status": "running"}, "If-Match", `"2"`), 200)

	expect(t, root.do("POST", "/users", map[string]any{"email": "vic@example.com", "display_name": "Vic", "password": "correct horse battery staple",
		"grants": []map[string]string{{"role": "viewer", "domain": sp.ID}}}), 201)
	viewer := h.client()
	viewer.bearer = loginAndIssueToken(t, h, "vic@example.com", []map[string]string{{"role": "viewer", "domain": sp.ID}})
	expect(t, viewer.do("GET", base+"/tasks", nil), 200)
	expect(t, viewer.do("POST", base+"/tasks/"+task.ID+"/move", map[string]any{"to": "top"}), 403)
}
