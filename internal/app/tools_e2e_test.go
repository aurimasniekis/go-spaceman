package app_test

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/agent"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	hns "github.com/aurimasniekis/go-spaceman/internal/harness"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// toolResult is the bridge's answer on the agent socket.
type toolResult struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result"`
	Error  string          `json:"error"`
	Code   string          `json:"code"`
}

// bridgeCall makes one call on the agent's bridge socket, as `spaceman-agent mcp-bridge` does.
func (e *fleetEnv) bridgeCall(run, token, tool string, args any) toolResult {
	e.t.Helper()
	c, err := net.DialTimeout("unix", e.bridge, 2*time.Second)
	if err != nil {
		e.t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(40 * time.Second))
	a, _ := json.Marshal(args)
	if err := json.NewEncoder(c).Encode(map[string]any{"run": run, "token": token, "tool": tool, "args": json.RawMessage(a)}); err != nil {
		e.t.Fatal(err)
	}
	var res toolResult
	if err := json.NewDecoder(bufio.NewReader(c)).Decode(&res); err != nil {
		e.t.Fatal(err)
	}
	return res
}

// runToken reads a run's token from the shim's secrets file (the fake runner never runs the shim).
func (e *fleetEnv) runToken(runID string) string {
	e.t.Helper()
	var m map[string]string
	b, err := os.ReadFile(filepath.Join(e.agentDir, "runs", runID, agent.SecretsFile))
	if err != nil || json.Unmarshal(b, &m) != nil {
		e.t.Fatalf("secrets file: %v", err)
	}
	return m["SPACEMAN_RUN_TOKEN"]
}

// SM-MCP-001/003, ADR-0012: agent tools through the real bridge socket, node connection and central. The catalog
// depends on the run kind, central authenticates the run token itself, workers cannot use control tools, and actions
// are attributed to the run.
func TestAgentToolsOverBridge(t *testing.T) {
	e := newFleetEnv(t)
	e.enroll()
	e.startAgent()
	ctx := context.Background()
	admin := audit.Actor{Type: audit.ActorUser, ID: "usr_admin"}
	nodes, _ := e.c.Fleet.ListNodes(ctx)
	eventually(t, "node connected", func() bool { n, _ := e.c.Fleet.GetNode(ctx, nodes[0].ID); return n.Connected })
	sp, err := e.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "tools", Name: "Tools", NodeID: nodes[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	bindStubAccounts(t, e.c, sp.ID)
	eventually(t, "lease granted", func() bool { s, _ := e.c.Fleet.GetSpace(ctx, sp.ID); return s.Lease != nil && s.Lease.Valid })
	ctrl, err := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Kind: "control", Role: "control", Harness: hns.KindClaude})
	if err != nil {
		t.Fatal(err)
	}
	wrk, err := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Kind: "worker", Role: "implementor", Harness: hns.KindClaude})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "runs running", func() bool { return e.run1(ctrl.ID).Observed == "running" && e.run1(wrk.ID).Observed == "running" })
	ct, wt := e.runToken(ctrl.ID), e.runToken(wrk.ID)

	catalog := func(run, tok string) []string {
		res := e.bridgeCall(run, tok, proto.ToolCatalog, nil)
		if !res.OK {
			t.Fatalf("catalog: %+v", res)
		}
		var l []proto.AgentTool
		_ = json.Unmarshal(res.Result, &l)
		var names []string
		for _, x := range l {
			var sch map[string]any
			if json.Unmarshal(x.InputSchema, &sch) != nil || sch["type"] != "object" {
				t.Fatalf("%s: input schema %s", x.Name, x.InputSchema)
			}
			names = append(names, x.Name)
		}
		return names
	}
	cn, wn := strings.Join(catalog(ctrl.ID, ct), " "), strings.Join(catalog(wrk.ID, wt), " ")
	if !strings.Contains(cn, "spaceman_task_create") || strings.Contains(wn, "spaceman_task_create") || !strings.Contains(wn, "spaceman_human_ask") {
		t.Fatalf("catalogs:\ncontrol: %s\nworker: %s", cn, wn)
	}

	// Authentication: a wrong token is refused by the node; another run's token is refused by central.
	if res := e.bridgeCall(ctrl.ID, "spaceman_rn_wrong", "spaceman_tasks_list", nil); res.OK || !strings.Contains(res.Error, "unauthenticated") {
		t.Fatalf("bad token: %+v", res)
	}
	if res := e.bridgeCall(wrk.ID, wt, "spaceman_task_create", map[string]any{"title": "x"}); res.OK || res.Code != "forbidden" {
		t.Fatalf("worker using a control tool: %+v", res)
	}
	if res := e.bridgeCall(ctrl.ID, ct, "spaceman_tasks_list", map[string]any{"nope": 1}); res.OK || res.Code != "validation_failed" {
		t.Fatalf("unknown argument: %+v", res)
	}
	if res := e.bridgeCall(ctrl.ID, ct, "spaceman_nope", nil); res.OK || res.Code != "not_found" {
		t.Fatalf("unknown tool: %+v", res)
	}

	// A worker reports a finding to the inbox; control turns it into a task; the worker asks a person, blocking it.
	res := e.bridgeCall(wrk.ID, wt, "spaceman_inbox_add", map[string]any{"body": "Flaky test in settlement\n\nTestRetry fails 1 in 20 runs."})
	if !res.OK {
		t.Fatalf("inbox add: %+v", res)
	}
	var item struct {
		ID         string `json:"id"`
		SourceKind string `json:"source_kind"`
		SourceID   string `json:"source_id"`
		Title      string `json:"title"`
	}
	_ = json.Unmarshal(res.Result, &item)
	if item.SourceKind != "worker" || item.SourceID != wrk.ID || item.Title != "Flaky test in settlement" {
		t.Fatalf("item %+v", item)
	}
	res = e.bridgeCall(ctrl.ID, ct, "spaceman_task_create", map[string]any{"title": "Fix the flaky retry test", "ready": true, "from_inbox_item": item.ID})
	if !res.OK {
		t.Fatalf("task create: %+v", res)
	}
	var task struct {
		ID       string `json:"id"`
		Status   string `json:"status"`
		OriginID string `json:"origin_id"`
	}
	_ = json.Unmarshal(res.Result, &task)
	if task.Status != "ready" || task.OriginID != item.ID {
		t.Fatalf("task %+v", task)
	}
	got, _ := e.c.Inbox.Get(ctx, sp.ID, item.ID)
	if got.Status != "done" || got.LinkedID != task.ID {
		t.Fatalf("inbox item not linked: %+v", got)
	}
	res = e.bridgeCall(wrk.ID, wt, "spaceman_human_ask", map[string]any{"type": "question", "title": "Quarantine the test?", "task_id": task.ID,
		"blocks_task": true, "request": map[string]any{"options": []string{"yes", "no"}}})
	if !res.OK {
		t.Fatalf("human ask: %+v", res)
	}
	var asked struct {
		ActionID string `json:"action_id"`
	}
	_ = json.Unmarshal(res.Result, &asked)
	a, err := e.c.Human.Get(ctx, sp.ID, asked.ActionID)
	if err != nil || a.RequesterKind != "agent" || a.RequesterRun != wrk.ID || a.RequesterRole != "implementor" {
		t.Fatalf("human action %+v %v", a, err)
	}
	// Workers may only change their own task, and only to review or blocked.
	if res := e.bridgeCall(wrk.ID, wt, "spaceman_task_set_status", map[string]any{"task_id": task.ID, "status": "review"}); res.OK || res.Code != "forbidden" {
		t.Fatalf("worker changing an unassigned task: %+v", res)
	}
	if res := e.bridgeCall(wrk.ID, wt, "spaceman_human_cancel", map[string]any{"action_id": asked.ActionID, "reason": "resolved myself"}); !res.OK {
		t.Fatalf("cancel own action: %+v", res)
	}
	// The audit log attributes the agents' actions to their runs.
	recs, err := e.c.Store.R().ListAuditPage(ctx, db.ListAuditPageParams{ActorID: sql.NullString{String: wrk.ID, Valid: true}, Lim: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) < 3 || recs[0].ActorType != audit.ActorAgent {
		t.Fatalf("worker audit records = %d", len(recs))
	}
	// A run that ended loses its tools.
	if _, err := e.c.Fleet.StopRun(ctx, admin, wrk.ID, "kill", "done"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "worker ended", func() bool { o := e.run1(wrk.ID).Observed; return o == "exited" || o == "failed" })
	if res := e.bridgeCall(wrk.ID, wt, "spaceman_tasks_list", nil); res.OK {
		t.Fatalf("ended run still has tools: %+v", res)
	}
}
