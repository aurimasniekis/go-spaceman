package app_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/aurimasniekis/go-spaceman/internal/agent"
	"github.com/aurimasniekis/go-spaceman/internal/api"
	"github.com/aurimasniekis/go-spaceman/internal/approvals"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/gateway"
	hns "github.com/aurimasniekis/go-spaceman/internal/harness"
	"github.com/aurimasniekis/go-spaceman/internal/human"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/spacemanmcp"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// SM-MCP-005..009, ADR-0063 (AM-8) end to end over the real run path: harness MCP client → `spaceman-agent mcp-bridge`
// (stdio) → agent socket → node connection → central gateway → SpaceMan MCP server → API handler chain.
//
//  1. A control run lists spaces through the stdio bridge.
//  2. A write tool runs at once with approval "none".
//  3. A dangerous tool asks for an approval, runs exactly once after a person approves, and asks again on a replay.
//  4. Revoking the server's token mid-run refuses the next call, with the reason.
//  5. A role the server was not granted to sees no tools and is refused.
//  6. The audit chain attributes the calls to the owner, via the token, for the run, and verifies.
//  7. The external endpoint answers a short call from a real HTTP MCP client with a user's own API token.
func TestSpacemanMCPOverBridge(t *testing.T) {
	// Not parallel: t.Setenv changes the process environment (the stdio bridge reads SPACEMAN_RUN_TOKEN).
	e := newFleetEnv(t)
	e.enroll()
	e.startAgent()
	ctx := context.Background()
	nodes, _ := e.c.Fleet.ListNodes(ctx)
	eventually(t, "node connected", func() bool { n, _ := e.c.Fleet.GetNode(ctx, nodes[0].ID); return n.Connected })
	sp, err := e.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "infra", Name: "Infra", NodeID: nodes[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	bindStubAccounts(t, e.c, sp.ID)

	sys := audit.Actor{Type: audit.ActorSystem, ID: "test"}
	adminAll := []auth.GrantInput{{Role: authz.RoleAdmin, Domain: authz.Global}}
	ada, err := e.c.Auth.Create(ctx, sys, auth.CreateInput{Email: "ada@example.com", DisplayName: "Ada", Grants: adminAll})
	if err != nil {
		t.Fatal(err)
	}
	ops, err := e.c.Auth.Create(ctx, sys, auth.CreateInput{Email: "ops@example.com", DisplayName: "Ops",
		Grants: []auth.GrantInput{{Role: authz.RoleOperator, Domain: sp.ID}}})
	if err != nil {
		t.Fatal(err)
	}
	adaP := &auth.Principal{User: ada, Method: auth.MethodPassword}
	adaA := audit.Actor{Type: audit.ActorUser, ID: ada.ID}

	// The server with its defaults (admin preset, approval none), granted to the control role only.
	srv, err := e.c.Gateway.CreateSpaceman(ctx, adaP, adaA, gateway.SpacemanInput{Slug: "sm", Name: "SpaceMan"})
	if err != nil {
		t.Fatal(err)
	}
	tokenID := srv.Spaceman.TokenID
	if srv.Spaceman.Approval != gateway.ApprovalNone || srv.Spaceman.OwnerUserID != ada.ID {
		t.Fatalf("created %+v", srv.Spaceman)
	}
	if _, err := e.c.Gateway.AddGrant(ctx, adaA, srv.ID, sp.ID, "control", nil); err != nil {
		t.Fatal(err)
	}
	eventually(t, "lease granted", func() bool { x, _ := e.c.Fleet.GetSpace(ctx, sp.ID); return x.Lease != nil && x.Lease.Valid })
	ctrl, err := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Kind: "control", Role: "control", Harness: hns.KindClaude})
	if err != nil {
		t.Fatal(err)
	}
	wrk, err := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Kind: "worker", Role: "implementor", Harness: hns.KindClaude})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "runs running", func() bool { return e.run1(ctrl.ID).Observed == "running" && e.run1(wrk.ID).Observed == "running" })
	if m := string(mustJSON(t, e.run1(ctrl.ID).Manifest)); !strings.Contains(m, `"slug":"sm"`) || !strings.Contains(m, `"kind":"spaceman"`) ||
		strings.Contains(m, tokenID) {
		t.Fatalf("control manifest: %s", m)
	}
	ctrlTok := e.runToken(ctrl.ID)

	// 1. The harness side of the control run: an MCP client on the real stdio bridge.
	t.Setenv("SPACEMAN_RUN_TOKEN", ctrlTok)
	cr, sw := io.Pipe()
	sr, cw := io.Pipe()
	bctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- agent.MCPBridge(bctx, sr, sw, ctrl.ID, e.bridge) }()
	t.Cleanup(func() {
		cancel()
		_ = cw.Close()
		_ = sw.Close()
		if err := <-done; err != nil && bctx.Err() == nil {
			t.Errorf("bridge: %v", err)
		}
	})
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "harness", Version: "test"}, nil).Connect(ctx, &mcp.IOTransport{Reader: cr, Writer: cw}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range tools.Tools {
		names = append(names, tl.Name)
	}
	if !slices.Contains(names, "sm__list_spaces") || !slices.Contains(names, "sm__delete_schedule") || !slices.Contains(names, "spaceman_inbox_add") ||
		slices.Contains(names, "sm__respond_human_action") || slices.Contains(names, "sm__create_spaceman_mcp") {
		t.Fatalf("bridge tools (%d): %v", len(names), names)
	}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "sm__list_spaces", Arguments: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if out := mcpResult(t, res); res.IsError || out.Status != 200 || !strings.Contains(string(out.Body), sp.ID) {
		t.Fatalf("list_spaces over the bridge: %+v", out)
	}

	// Calls on the agent socket, as `spaceman-agent mcp-bridge` makes them, return structured codes.
	call := func(run, tok, tool string, args any) (toolResult, spacemanmcp.Result) {
		t.Helper()
		r := e.bridgeCall(run, tok, tool, args)
		var out spacemanmcp.Result
		if r.OK {
			var ctr mcp.CallToolResult
			if err := json.Unmarshal(r.Result, &ctr); err != nil {
				t.Fatalf("%s result: %v", tool, err)
			}
			out = mcpResult(t, &ctr)
		}
		return r, out
	}

	// 2. A write tool runs at once: no approval is asked for.
	sched := map[string]any{"name": "weekly review", "kind": "cron", "cron": "0 9 * * 1", "timezone": "UTC", "missed": "run_once",
		"target": "inbox", "title": "Weekly review", "body": "Look at the queue."}
	r, out := call(ctrl.ID, ctrlTok, "sm__create_schedule", map[string]any{"space_id": sp.ID, "body": sched})
	var created struct {
		ID string `json:"id"`
	}
	if !r.OK || out.Status != 201 || json.Unmarshal(out.Body, &created) != nil || created.ID == "" {
		t.Fatalf("create_schedule: %+v %+v", r, out)
	}
	if l, _ := e.c.Approvals.List(ctx, sp.ID, 10); len(l) != 0 {
		t.Fatalf("a write with approval none asked for approvals: %+v", l)
	}

	// 3. A dangerous tool: refused until a person approves, then runs exactly once.
	del := map[string]any{"space_id": sp.ID, "schedule_id": created.ID}
	if r, _ := call(ctrl.ID, ctrlTok, "sm__delete_schedule", del); r.OK || r.Code != "precondition_failed" || !strings.Contains(r.Error, "needs a person's approval") {
		t.Fatalf("dangerous call without an approval: %+v", r)
	}
	l, err := e.c.Approvals.List(ctx, sp.ID, 10)
	if err != nil || len(l) != 1 || l[0].Operation != "mcp.sm.dangerous.delete_schedule" || l[0].State != approvals.Pending || l[0].RequesterRun != ctrl.ID {
		t.Fatalf("approvals: %+v %v", l, err)
	}
	appr := l[0]
	if r, _ := call(ctrl.ID, ctrlTok, "sm__delete_schedule", del); r.OK || r.Code != "precondition_failed" {
		t.Fatalf("dangerous call while pending: %+v", r)
	}
	if _, err := e.c.Schedules.Get(ctx, sp.ID, created.ID); err != nil {
		t.Fatalf("the schedule went before the approval: %v", err)
	}
	act, err := e.c.Human.Get(ctx, sp.ID, appr.HumanActionID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.c.Human.Respond(ctx, audit.Actor{Type: audit.ActorUser, ID: ops.ID}, sp.ID, act.ID, act.Version, human.RespondInput{Outcome: "approved"}); err != nil {
		t.Fatal(err)
	}
	if r, out := call(ctrl.ID, ctrlTok, "sm__delete_schedule", del); !r.OK || out.Status != 204 {
		t.Fatalf("approved dangerous call: %+v %+v", r, out)
	}
	if _, err := e.c.Schedules.Get(ctx, sp.ID, created.ID); err == nil {
		t.Fatal("the approved delete left the schedule")
	}
	if r, _ := call(ctrl.ID, ctrlTok, "sm__delete_schedule", del); r.OK || r.Code != "precondition_failed" {
		t.Fatalf("replay of a one-use approval: %+v", r)
	}
	a, err := e.c.Approvals.Get(ctx, sp.ID, appr.ID)
	if err != nil || a.State != approvals.Approved || a.UsesLeft != 0 || len(a.Executions) != 1 || a.Executions[0].State != approvals.Succeeded {
		t.Fatalf("approval record: %+v %v", a, err)
	}
	if l, _ := e.c.Approvals.List(ctx, sp.ID, 10); len(l) != 2 {
		t.Fatalf("the replay should ask again: %+v", l)
	}

	// 5. The implementor role was not granted the server: no tools in its catalog, and calls are refused.
	wrkTok := e.runToken(wrk.ID)
	if r := e.bridgeCall(wrk.ID, wrkTok, proto.ToolCatalog, nil); !r.OK || strings.Contains(string(r.Result), "sm__") {
		t.Fatalf("ungranted role's catalog: %+v", r)
	}
	if r, _ := call(wrk.ID, wrkTok, "sm__list_spaces", map[string]any{}); r.OK || r.Code != "forbidden" {
		t.Fatalf("ungranted role's call: %+v", r)
	}

	// 6. Audit: the operations as the owner, via the internal token, for the control run; the gateway's tool calls with
	// the operation and class; and the chain verifies.
	recs := func(action string) []audit.Record {
		t.Helper()
		rows, err := e.c.Store.R().ListAuditPage(ctx, db.ListAuditPageParams{Lim: 100, Action: sql.NullString{String: action, Valid: true}})
		if err != nil {
			t.Fatal(err)
		}
		var out []audit.Record
		for _, row := range rows {
			out = append(out, audit.DecodeRecord(row))
		}
		return out
	}
	for _, action := range []string{"schedule.created", "schedule.deleted"} {
		rs := recs(action)
		if len(rs) != 1 || rs[0].ActorID != ada.ID || rs[0].Details["via"] != auth.ViaSpacemanMCP+tokenID || rs[0].Details["run"] != ctrl.ID {
			t.Fatalf("%s audit: %+v", action, rs)
		}
	}
	calls := map[string]int{}
	for _, rec := range recs("mcp_server.tool_called") {
		// The gateway's own record names the run as the actor, the server as the target, and the operation and class.
		if rec.ActorType != audit.ActorAgent || rec.ActorID != ctrl.ID || rec.TargetID != srv.ID {
			t.Fatalf("tool call audit: %+v", rec)
		}
		op, _ := rec.Details["operation"].(string)
		class, _ := rec.Details["class"].(string)
		calls[op+":"+class+":"+rec.Outcome]++
	}
	if calls["list-spaces:read:success"] == 0 || calls["create-schedule:write:success"] != 1 || calls["delete-schedule:dangerous:success"] != 1 {
		t.Fatalf("tool call audit: %v", calls)
	}
	if v, err := audit.Verify(ctx, e.c.Store.R()); err != nil || !v.OK {
		t.Fatalf("audit chain: %+v %v", v, err)
	}

	// 7. The external endpoint: a user's own API token on a real HTTP MCP client.
	_, secret, err := e.c.Auth.CreateAPIToken(ctx, adaP, "client", adminAll, 24*time.Hour, false)
	if err != nil {
		t.Fatal(err)
	}
	ext, err := mcp.NewClient(&mcp.Implementation{Name: "operator-client", Version: "test"}, nil).Connect(ctx,
		&mcp.StreamableClientTransport{Endpoint: e.srv.URL + api.MCPPath, HTTPClient: &http.Client{Transport: bearerTransport{bearer: secret}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ext.Close()
	res, err = ext.CallTool(ctx, &mcp.CallToolParams{Name: "list_spaces", Arguments: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if out := mcpResult(t, res); res.IsError || out.Status != 200 || !strings.Contains(string(out.Body), sp.ID) {
		t.Fatalf("list_spaces on the endpoint: %+v", out)
	}
	if rs := recs("mcp_endpoint.tool_called"); len(rs) != 1 || rs[0].ActorID != ada.ID || rs[0].Details["channel"] != audit.ChannelMCPEndpoint {
		t.Fatalf("endpoint audit: %+v", rs)
	}

	// 4. Revoking the server's token mid-run: the next call is refused with the reason, and the catalog empties.
	if _, err := e.c.Auth.RevokeAPIToken(ctx, adaA, tokenID); err != nil {
		t.Fatal(err)
	}
	if r, _ := call(ctrl.ID, ctrlTok, "sm__list_spaces", map[string]any{}); r.OK || r.Code != "forbidden" ||
		!strings.Contains(r.Error, "revoked") || !strings.Contains(r.Error, "re-issue") {
		t.Fatalf("call after the token was revoked: %+v", r)
	}
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "sm__list_spaces", Arguments: map[string]any{}})
	if err == nil && !res.IsError {
		t.Fatalf("stdio call after the token was revoked: %+v", res)
	}
	if r := e.bridgeCall(ctrl.ID, ctrlTok, proto.ToolCatalog, nil); !r.OK || strings.Contains(string(r.Result), "sm__") {
		t.Fatalf("revoked token's catalog: %+v", r)
	}
	// The run itself goes on: its built-in tools still answer.
	if r := e.bridgeCall(ctrl.ID, ctrlTok, proto.ToolCatalog, nil); !r.OK || !strings.Contains(string(r.Result), "spaceman_") {
		t.Fatalf("built-in catalog after the revocation: %+v", r)
	}
	if e.run1(ctrl.ID).Observed != "running" {
		t.Fatal("revoking the token stopped the run")
	}
}
