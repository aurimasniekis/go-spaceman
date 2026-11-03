package app_test

import (
	"context"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/api"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/human"
	"github.com/aurimasniekis/go-spaceman/internal/queue"
)

// SM-HUM-* over HTTP: a blocking question on a task, If-Match on responses, type-specific outcomes, comments, the global
// view filtered to readable spaces, and approval decisions that need approval:decide.
func TestHumanActionsOverHTTP(t *testing.T) {
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
	other, err := h.c.Fleet.CreateSpace(ctx, actor, fleet.CreateSpaceInput{Slug: "beta", Name: "Beta", NodeID: en.Node.ID})
	if err != nil {
		t.Fatal(err)
	}
	base := "/spaces/" + sp.ID

	var task queue.Task
	root.do("POST", base+"/tasks", map[string]any{"title": "Pick a database", "status": "ready"}).json(t, &task)
	r := root.do("POST", base+"/human-actions", map[string]any{"type": "question", "title": "SQLite or Postgres?", "task_id": task.ID, "blocks_task": true,
		"request": map[string]any{"options": []string{"SQLite", "Postgres"}}})
	expect(t, r, 201)
	var q human.Action
	r.json(t, &q)
	if q.RequesterKind != "operator" || q.Status != "open" || r.header.Get("ETag") != `"1"` {
		t.Fatalf("created %+v", q)
	}
	var blocked queue.Task
	root.do("GET", base+"/tasks/"+task.ID, nil).json(t, &blocked)
	if len(blocked.Waiting) == 0 {
		t.Fatalf("task not blocked: %+v", blocked)
	}
	expect(t, root.do("POST", base+"/human-actions/"+q.ID+"/comments", map[string]any{"body": "Postgres is overkill here"}), 201)
	expect(t, root.do("POST", base+"/human-actions/"+q.ID+"/respond", map[string]any{"outcome": "answered", "answer": "SQLite"}), 428)
	expect(t, root.do("POST", base+"/human-actions/"+q.ID+"/respond", map[string]any{"outcome": "approved"}, "If-Match", `"1"`), 422)
	var ans human.Action
	r = root.do("POST", base+"/human-actions/"+q.ID+"/respond", map[string]any{"outcome": "answered", "answer": "SQLite"}, "If-Match", `"1"`)
	expect(t, r, 200)
	r.json(t, &ans)
	if ans.Status != "resolved" || ans.Outcome != "answered" || len(ans.Comments) != 1 {
		t.Fatalf("answered %+v", ans)
	}
	expect(t, root.do("POST", base+"/human-actions/"+q.ID+"/respond", map[string]any{"outcome": "answered", "answer": "Postgres"}, "If-Match", `"2"`), 409)
	var unblocked queue.Task
	root.do("GET", base+"/tasks/"+task.ID, nil).json(t, &unblocked)
	if len(unblocked.Waiting) != 0 || unblocked.Status != "ready" {
		t.Fatalf("task still blocked: %+v", unblocked)
	}

	var ap, apOther human.Action
	root.do("POST", base+"/human-actions", map[string]any{"type": "approval", "title": "Deploy to production?", "request": map[string]any{"action": "deploy v1.2"}}).json(t, &ap)
	root.do("POST", "/spaces/"+other.ID+"/human-actions", map[string]any{"type": "todo", "title": "Elsewhere"}).json(t, &apOther)

	expect(t, root.do("POST", "/users", map[string]any{"email": "vic@example.com", "display_name": "Vic", "password": "correct horse battery staple",
		"grants": []map[string]string{{"role": "viewer", "domain": sp.ID}}}), 201)
	viewer := h.client()
	viewer.bearer = loginAndIssueToken(t, h, "vic@example.com", []map[string]string{{"role": "viewer", "domain": sp.ID}})
	var pend api.PendingHumanActions
	viewer.do("GET", "/human-actions", nil).json(t, &pend)
	if len(pend.Items) != 1 || pend.Items[0].ID != ap.ID || pend.Total != 1 || pend.Counts[other.ID] != 0 {
		t.Fatalf("viewer global view %+v", pend)
	}
	expect(t, viewer.do("GET", "/spaces/"+other.ID+"/human-actions", nil), 404) // unreadable spaces are hidden
	expect(t, viewer.do("POST", base+"/human-actions/"+ap.ID+"/respond", map[string]any{"outcome": "approved"}, "If-Match", `"1"`), 403)

	root.do("GET", "/human-actions", nil).json(t, &pend)
	if pend.Total != 2 {
		t.Fatalf("root global view %+v", pend)
	}
	expect(t, root.do("POST", base+"/human-actions/"+ap.ID+"/respond", map[string]any{"outcome": "approved"}, "If-Match", `"1"`), 200)
	var l api.HumanActionList
	root.do("GET", base+"/human-actions?pending=true", nil).json(t, &l)
	if len(l.Items) != 0 {
		t.Fatalf("pending after responses %+v", l)
	}
}

// SM-HUM-009/011 over HTTP: installation defaults and space overrides with their effective sources, If-Match on
// policies, and routing an undeliverable response to control.
func TestHumanPoliciesOverHTTP(t *testing.T) {
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
	body := map[string]any{"remind_after_minutes": 30, "remind_every_minutes": 60, "max_reminders": 2, "escalate_after_minutes": 120,
		"escalate_priority": "urgent", "escalate_assignee": "", "requester_ended": "reroute", "hold_minutes": 60, "successor_role": "reviewer",
		"wake_idle": true, "rate_per_run_hour": 10, "rate_per_space_hour": 100}
	var pol human.Policy
	root.do("PUT", "/human-policies/all", body).json(t, &pol)
	if pol.ActionType != "all" || pol.Version != 1 || pol.SpaceID != "" {
		t.Fatalf("default %+v", pol)
	}
	expect(t, root.do("PUT", "/human-policies/all", body, "If-Match", `"9"`), 409)
	expect(t, root.do("PUT", "/human-policies/poll", body), 422)
	bad := map[string]any{}
	for k, v := range body {
		bad[k] = v
	}
	bad["max_reminders"] = 0
	expect(t, root.do("PUT", "/human-policies/question", bad), 422)
	body["remind_after_minutes"] = 5
	root.do("PUT", "/spaces/"+sp.ID+"/human-policies/question", body).json(t, &pol)
	if pol.SpaceID != sp.ID || pol.ActionType != "question" {
		t.Fatalf("space %+v", pol)
	}
	var list api.HumanPolicies
	root.do("GET", "/spaces/"+sp.ID+"/human-policies", nil).json(t, &list)
	src := map[string]string{}
	for _, p := range list.Effective {
		src[p.ActionType] = p.Source
	}
	if len(list.Items) != 1 || src["question"] != "space:question" || src["todo"] != "default:all" {
		t.Fatalf("effective %+v", list)
	}
	root.do("GET", "/human-policies", nil).json(t, &list)
	if len(list.Items) != 1 || len(list.Effective) != 5 {
		t.Fatalf("defaults %+v", list)
	}
	expect(t, root.do("DELETE", "/spaces/"+sp.ID+"/human-policies/question", nil), 204)
	expect(t, root.do("DELETE", "/spaces/"+sp.ID+"/human-policies/question", nil), 404)
	expect(t, root.do("DELETE", "/human-policies/all", nil), 204)

	// An agent's question whose run is gone and no control runs: undeliverable until a person routes it.
	a, _, err := h.c.Human.Create(ctx, admin, sp.ID, human.CreateInput{Type: human.TypeQuestion, Title: "Which?",
		Requester: human.Requester{Kind: "agent", Role: "implementor", Run: "run_gone"}})
	if err != nil {
		t.Fatal(err)
	}
	var ans human.Action
	root.do("POST", "/spaces/"+sp.ID+"/human-actions/"+a.ID+"/respond", map[string]any{"outcome": "answered", "answer": "this"}, "If-Match", `"1"`).json(t, &ans)
	n := ans.Notifications[0]
	if n.TargetKind != human.TargetOperators {
		t.Fatalf("undeliverable %+v", n)
	}
	path := "/spaces/" + sp.ID + "/human-actions/" + a.ID + "/notifications/" + n.ID + "/route"
	expect(t, root.do("POST", path, map[string]any{"to": "role:reviewer"}), 409)
	var routed human.Action
	root.do("POST", path, map[string]any{"to": "control"}).json(t, &routed)
	if x := routed.Notifications[0]; x.TargetKind != human.TargetControl || x.RouteReason != human.ReasonOperator {
		t.Fatalf("routed %+v", x)
	}
}
