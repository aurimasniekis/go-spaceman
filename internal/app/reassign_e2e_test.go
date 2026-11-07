package app_test

import (
	"context"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	hns "github.com/aurimasniekis/go-spaceman/internal/harness"
	"github.com/aurimasniekis/go-spaceman/internal/queue"
)

// SM-CTRL-004: an operator reassigns a task over HTTP (the role and the run working on it). The change needs the
// task's version, is visible on the task, is audited with the previous and new assignee, and never points at another
// space's run. (Viewers cannot edit tasks: update-task needs queue:write, TestQueueOverHTTP and the rules table.)
func TestOperatorReassignsTask(t *testing.T) {
	t.Parallel()
	e := newFleetEnv(t)
	e.enroll()
	e.startAgent()
	ctx := context.Background()
	nodes, _ := e.c.Fleet.ListNodes(ctx)
	nodeID := nodes[0].ID
	eventually(t, "node connected", func() bool { n, _ := e.c.Fleet.GetNode(ctx, nodeID); return n.Connected })
	// The fleet environment's server, driven through the API harness client.
	h := &harness{t: t, srv: e.srv, c: e.c}
	rootSecret, _, _ := h.c.Auth.IssueRootToken(ctx)
	root := h.client()
	root.bearer = rootSecret
	launch := func(slug string) (*fleet.Space, *fleet.Run) {
		sp, err := e.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: slug, Name: slug, NodeID: nodeID})
		if err != nil {
			t.Fatal(err)
		}
		bindStubAccounts(t, e.c, sp.ID)
		eventually(t, "lease granted", func() bool { s, _ := e.c.Fleet.GetSpace(ctx, sp.ID); return s.Lease != nil && s.Lease.Valid })
		run, err := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Kind: "worker", Role: "reviewer", Harness: hns.KindClaude})
		if err != nil {
			t.Fatal(err)
		}
		// Let the node finish starting it (memory checkout included) before the test ends and removes its folders.
		eventually(t, "run running", func() bool { return e.run1(run.ID).Observed == "running" })
		return sp, run
	}
	sp, run := launch("alpha")
	_, foreign := launch("beta")
	base := "/spaces/" + sp.ID

	var task queue.Task
	expect(t, root.do("POST", base+"/tasks", map[string]any{"title": "Review the refunds change", "status": "ready", "assignee_role": "implementor"}), 201)
	var q struct {
		Tasks []queue.Task `json:"tasks"`
	}
	root.do("GET", base+"/tasks", nil).json(t, &q)
	task = q.Tasks[0]
	body := func(role, run string) map[string]any {
		return map[string]any{"title": task.Title, "objective": task.Objective, "acceptance": task.Acceptance, "priority": task.Priority,
			"exclusions": task.Exclusions, "assignee_role": role, "assignee_run": run}
	}

	// Without If-Match, or with a stale version, nothing changes.
	expect(t, root.do("PUT", base+"/tasks/"+task.ID, body("reviewer", run.ID)), 428)
	expect(t, root.do("PUT", base+"/tasks/"+task.ID, body("reviewer", run.ID), "If-Match", `"99"`), 409)
	// Another space's run is refused.
	expect(t, root.do("PUT", base+"/tasks/"+task.ID, body("reviewer", foreign.ID), "If-Match", `"1"`), 422)

	r := root.do("PUT", base+"/tasks/"+task.ID, body("reviewer", run.ID), "If-Match", `"1"`)
	expect(t, r, 200)
	var got queue.Task
	root.do("GET", base+"/tasks/"+task.ID, nil).json(t, &got)
	if got.AssigneeRole != "reviewer" || got.AssigneeRun != run.ID || got.Version != 2 || got.Title != task.Title {
		t.Fatalf("after reassignment %+v", got)
	}

	var page struct {
		Items []struct {
			Action  string         `json:"action"`
			Details map[string]any `json:"details"`
		} `json:"items"`
	}
	root.do("GET", "/audit?action=task.updated&target_id="+task.ID, nil).json(t, &page)
	if len(page.Items) != 1 {
		t.Fatalf("audit %+v", page)
	}
	if d := page.Items[0].Details; d["assignee_role"] != "reviewer" || d["assignee_run"] != run.ID || d["previous_assignee_role"] != "implementor" ||
		d["previous_assignee_run"] != "" {
		t.Fatalf("audit details %+v", d)
	}

	// Clearing the run keeps the role.
	expect(t, root.do("PUT", base+"/tasks/"+task.ID, body("reviewer", ""), "If-Match", `"2"`), 200)
	var cleared queue.Task // a fresh value: an empty assignee run is omitted from the JSON
	root.do("GET", base+"/tasks/"+task.ID, nil).json(t, &cleared)
	if cleared.AssigneeRole != "reviewer" || cleared.AssigneeRun != "" {
		t.Fatalf("after clearing the run %+v", cleared)
	}
}
