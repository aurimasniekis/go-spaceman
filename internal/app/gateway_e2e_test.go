package app_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/aurimasniekis/go-spaceman/internal/agent"
	"github.com/aurimasniekis/go-spaceman/internal/approvals"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/gateway"
	hns "github.com/aurimasniekis/go-spaceman/internal/harness"
	"github.com/aurimasniekis/go-spaceman/internal/human"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// SM-MCP-001/002, ADR-0023 end to end: an external MCP server registered centrally is granted to one space role. A
// harness's own MCP client, talking to the real stdio bridge, sees the server's tools namespaced next to SpaceMan's and
// gets the upstream's result as the upstream sent it; it never sees the server's address or credentials. Another role
// sees nothing, and the run's upstream session ends with the run.
func TestGatewayOverBridge(t *testing.T) {
	var gotAuth []string
	up := mcp.NewServer(&mcp.Implementation{Name: "tickets", Version: "1.0"}, nil)
	type in struct {
		ID string `json:"id"`
	}
	mcp.AddTool(up, &mcp.Tool{Name: "ticket_get", Description: "Read a ticket."}, func(_ context.Context, req *mcp.CallToolRequest, a in) (*mcp.CallToolResult, any, error) {
		gotAuth = append(gotAuth, req.Extra.Header.Get("Authorization"))
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ticket " + a.ID + ": refunds stuck"}}}, nil, nil
	})
	srv := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return up }, nil))
	t.Cleanup(srv.Close)

	e := newFleetEnv(t)
	e.enroll()
	e.startAgent()
	ctx := context.Background()
	admin := audit.Actor{Type: audit.ActorUser, ID: "usr_admin"}
	nodes, _ := e.c.Fleet.ListNodes(ctx)
	eventually(t, "node connected", func() bool { n, _ := e.c.Fleet.GetNode(ctx, nodes[0].ID); return n.Connected })
	sp, err := e.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "gateway", Name: "Gateway", NodeID: nodes[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	bindStubAccounts(t, e.c, sp.ID)
	s, err := e.c.Gateway.Create(ctx, admin, gateway.Input{Slug: "tickets", Name: "Tickets", URL: srv.URL, AllowPrivate: true, Enabled: true,
		Headers: map[string]string{"Authorization": "Bearer upstream-secret"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.c.Gateway.AddGrant(ctx, admin, s.ID, sp.ID, "triage", nil); err != nil {
		t.Fatal(err)
	}
	eventually(t, "lease granted", func() bool { x, _ := e.c.Fleet.GetSpace(ctx, sp.ID); return x.Lease != nil && x.Lease.Valid })
	wrk, err := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Kind: "worker", Role: "triage", Harness: hns.KindClaude})
	if err != nil {
		t.Fatal(err)
	}
	other, err := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Kind: "worker", Role: "implementor", Harness: hns.KindClaude})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "runs running", func() bool { return e.run1(wrk.ID).Observed == "running" && e.run1(other.ID).Observed == "running" })
	if m := e.run1(wrk.ID).Manifest; !strings.Contains(string(mustJSON(t, m)), `"slug":"tickets"`) {
		t.Fatalf("manifest lacks the granted server: %s", mustJSON(t, m))
	}

	// The harness side: an MCP client on the real stdio bridge.
	t.Setenv("SPACEMAN_RUN_TOKEN", e.runToken(wrk.ID))
	cr, sw := io.Pipe()
	sr, cw := io.Pipe()
	bctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- agent.MCPBridge(bctx, sr, sw, wrk.ID, e.bridge) }()
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
	if !slices.Contains(names, "tickets__ticket_get") || !slices.Contains(names, "spaceman_inbox_add") {
		t.Fatalf("bridge tools: %v", names)
	}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "tickets__ticket_get", Arguments: map[string]any{"id": "T-7"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || len(res.Content) != 1 || res.Content[0].(*mcp.TextContent).Text != "ticket T-7: refunds stuck" {
		t.Fatalf("result: %+v", res)
	}
	if len(gotAuth) != 1 || gotAuth[0] != "Bearer upstream-secret" {
		t.Fatalf("upstream saw %v", gotAuth)
	}

	// Another role in the same space is not granted the server.
	res2 := e.bridgeCall(other.ID, e.runToken(other.ID), proto.ToolCatalog, nil)
	if !res2.OK || strings.Contains(string(res2.Result), "tickets__") {
		t.Fatalf("ungranted catalog: %+v", res2)
	}
	if r := e.bridgeCall(other.ID, e.runToken(other.ID), "tickets__ticket_get", map[string]any{"id": "T-7"}); r.OK || r.Code != "forbidden" {
		t.Fatalf("ungranted call: %+v", r)
	}

	// The upstream session ends with the run.
	if e.c.Gateway.Sessions() != 1 {
		t.Fatalf("sessions: %d", e.c.Gateway.Sessions())
	}
	if _, err := e.c.Fleet.StopRun(ctx, admin, wrk.ID, "kill", "done"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "session closed", func() bool { return e.c.Gateway.Sessions() == 0 })
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// SM-APPR-001…004, ADR-0024 end to end: a policy gates a gateway tool. The agent's first call returns at once with an
// approval request; the upstream is not called. After a person approves, the same call runs exactly once; changed
// arguments need a new approval. Approval and execution are recorded separately.
func TestGatedGatewayCall(t *testing.T) {
	var calls atomic.Int32
	up := mcp.NewServer(&mcp.Implementation{Name: "deploys", Version: "1.0"}, nil)
	type in struct {
		Service string `json:"service"`
	}
	mcp.AddTool(up, &mcp.Tool{Name: "deploy"}, func(_ context.Context, _ *mcp.CallToolRequest, a in) (*mcp.CallToolResult, any, error) {
		calls.Add(1)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "deployed " + a.Service}}}, nil, nil
	})
	srv := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return up }, nil))
	t.Cleanup(srv.Close)

	e := newFleetEnv(t)
	e.enroll()
	e.startAgent()
	ctx := context.Background()
	admin := audit.Actor{Type: audit.ActorUser, ID: "usr_admin"}
	nodes, _ := e.c.Fleet.ListNodes(ctx)
	eventually(t, "node connected", func() bool { n, _ := e.c.Fleet.GetNode(ctx, nodes[0].ID); return n.Connected })
	sp, err := e.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "gated", Name: "Gated", NodeID: nodes[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	bindStubAccounts(t, e.c, sp.ID)
	s, err := e.c.Gateway.Create(ctx, admin, gateway.Input{Slug: "deploys", Name: "Deploys", URL: srv.URL, AllowPrivate: true, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.c.Gateway.AddGrant(ctx, admin, s.ID, sp.ID, "*", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.c.Approvals.CreatePolicy(ctx, admin, approvals.PolicyInput{SpaceID: sp.ID, Operation: "mcp.deploys.deploy"}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "lease granted", func() bool { x, _ := e.c.Fleet.GetSpace(ctx, sp.ID); return x.Lease != nil && x.Lease.Valid })
	ctrl, err := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Kind: "control", Role: "control", Harness: hns.KindClaude})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "run running", func() bool { return e.run1(ctrl.ID).Observed == "running" })
	tok := e.runToken(ctrl.ID)

	res := e.bridgeCall(ctrl.ID, tok, "deploys__deploy", map[string]any{"service": "settlement"})
	if res.OK || res.Code != "precondition_failed" || !strings.Contains(res.Error, "needs a person's approval") || calls.Load() != 0 {
		t.Fatalf("ungated call: %+v (upstream calls %d)", res, calls.Load())
	}
	l, err := e.c.Approvals.List(ctx, sp.ID, 10)
	if err != nil || len(l) != 1 || l[0].State != approvals.Pending {
		t.Fatalf("approvals: %+v %v", l, err)
	}
	// An operator of the space approves (the API checks approval:decide; this exercises the service the API calls).
	ops, err := e.c.Auth.Create(ctx, admin, auth.CreateInput{Email: "ops@example.test", DisplayName: "Ops", Grants: []auth.GrantInput{{Role: authz.RoleOperator, Domain: sp.ID}}})
	if err != nil {
		t.Fatal(err)
	}
	opsActor := audit.Actor{Type: audit.ActorUser, ID: ops.ID}
	approve := func(actionID string) {
		t.Helper()
		act, err := e.c.Human.Get(ctx, sp.ID, actionID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.c.Human.Respond(ctx, opsActor, sp.ID, act.ID, act.Version, human.RespondInput{Outcome: "approved"}); err != nil {
			t.Fatal(err)
		}
	}
	approve(l[0].HumanActionID)

	if r := e.bridgeCall(ctrl.ID, tok, "deploys__deploy", map[string]any{"service": "ledger"}); r.OK || calls.Load() != 0 {
		t.Fatalf("changed arguments ran: %+v", r)
	}
	res = e.bridgeCall(ctrl.ID, tok, "deploys__deploy", map[string]any{"service": "settlement"})
	if !res.OK || calls.Load() != 1 || !strings.Contains(string(res.Result), "deployed settlement") {
		t.Fatalf("approved call: %+v (upstream calls %d)", res, calls.Load())
	}
	if r := e.bridgeCall(ctrl.ID, tok, "deploys__deploy", map[string]any{"service": "settlement"}); r.OK || calls.Load() != 1 {
		t.Fatalf("replayed: %+v", r)
	}
	a, err := e.c.Approvals.Get(ctx, sp.ID, l[0].ID)
	if err != nil || a.State != approvals.Approved || a.UsesLeft != 0 || len(a.Executions) != 1 || a.Executions[0].State != approvals.Succeeded {
		t.Fatalf("approval record: %+v %v", a, err)
	}

	// Authorization is checked again at execution: an approval by a user who was disabled since is revoked unused.
	l, _ = e.c.Approvals.List(ctx, sp.ID, 10)
	var next *approvals.Approval
	for _, x := range l {
		if x.State == approvals.Pending && strings.Contains(x.ArgsPreview, "settlement") {
			next = x
		}
	}
	if next == nil {
		t.Fatalf("no new request after the replay: %+v", l)
	}
	approve(next.HumanActionID)
	if _, err := e.c.Auth.Create(ctx, admin, auth.CreateInput{Email: "admin@example.test", DisplayName: "Admin", Grants: []auth.GrantInput{{Role: authz.RoleAdmin, Domain: "*"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.c.Auth.SetEnabled(ctx, admin, ops.ID, false); err != nil {
		t.Fatal(err)
	}
	if r := e.bridgeCall(ctrl.ID, tok, "deploys__deploy", map[string]any{"service": "settlement"}); r.OK || calls.Load() != 1 {
		t.Fatalf("ran on a disabled approver's approval: %+v", r)
	}
	if a, _ := e.c.Approvals.Get(ctx, sp.ID, next.ID); a.State != approvals.Revoked {
		t.Fatalf("approval of a disabled user: %+v", a)
	}
}
