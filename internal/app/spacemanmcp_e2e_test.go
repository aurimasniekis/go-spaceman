package app_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/aurimasniekis/go-spaceman/internal/api"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/spacemanmcp"
)

func mcpResult(t *testing.T, r *mcp.CallToolResult) spacemanmcp.Result {
	t.Helper()
	var out spacemanmcp.Result
	if err := json.Unmarshal([]byte(r.Content[0].(*mcp.TextContent).Text), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// SpaceMan MCP tools (ADR-0063, AM-4) run through the API's in-process handler with the internal token's principal and
// answer exactly as the API does over HTTP for the same grants: 200, 403 and 404 (a space the caller cannot read is
// missing), with the same problem codes. A retried create returns the first result instead of a second resource.
func TestSpacemanMCPExecutorParity(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	rootSecret, _, err := h.c.Auth.IssueRootToken(ctx)
	if err != nil {
		t.Fatal(err)
	}
	root := h.client()
	root.bearer = rootSecret
	en, err := h.c.Fleet.CreateNode(ctx, audit.Actor{Type: audit.ActorUser, ID: "usr_test"}, fleet.CreateNodeInput{Name: "node-a"})
	if err != nil {
		t.Fatal(err)
	}
	var a, b struct {
		ID string `json:"id"`
	}
	r := root.do("POST", "/spaces", map[string]any{"slug": "alpha", "name": "Alpha", "node_id": en.Node.ID})
	expect(t, r, 201)
	r.json(t, &a)
	r = root.do("POST", "/spaces", map[string]any{"slug": "beta", "name": "Beta", "node_id": en.Node.ID})
	expect(t, r, 201)
	r.json(t, &b)

	sys := audit.Actor{Type: audit.ActorSystem, ID: "test"}
	viewerGrants := []auth.GrantInput{{Role: authz.RoleViewer, Domain: a.ID}}
	vera, err := h.c.Auth.Create(ctx, sys, auth.CreateInput{Email: "vera@example.com", DisplayName: "Vera", Password: "correct horse battery staple", Grants: viewerGrants})
	if err != nil {
		t.Fatal(err)
	}
	adminGrants := []auth.GrantInput{{Role: authz.RoleAdmin, Domain: authz.Global}}
	ada, err := h.c.Auth.Create(ctx, sys, auth.CreateInput{Email: "ada@example.com", DisplayName: "Ada", Grants: adminGrants})
	if err != nil {
		t.Fatal(err)
	}
	bearer := h.client()
	bearer.bearer = loginAndIssueToken(t, h, "vera@example.com", []map[string]string{{"role": "viewer", "domain": a.ID}})
	principal := func(owner *auth.User, grants []auth.GrantInput, run string) *auth.Principal {
		t.Helper()
		tok, err := h.c.Auth.CreateInternalToken(ctx, &auth.Principal{User: owner}, auth.InternalTokenInput{Purpose: auth.TokenPurposeSpacemanMCP,
			Name: "SpaceMan", Grants: grants, NeverExpires: true})
		if err != nil {
			t.Fatal(err)
		}
		p, err := h.c.Auth.InternalPrincipal(ctx, tok.ID, run)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	veraMCP := principal(vera, viewerGrants, "run_vera")

	d := h.c.APIDeps()
	cat, err := spacemanmcp.Build(api.Register(http.NewServeMux(), d).OpenAPI(), d.RuleFor)
	if err != nil {
		t.Fatal(err)
	}
	ex := &spacemanmcp.Executor{Handler: d.InProcessHandler()}
	call := func(p *auth.Principal, tool string, args any) (spacemanmcp.Result, bool) {
		t.Helper()
		tl, ok := cat.Tool(tool)
		if !ok {
			t.Fatalf("no tool %s", tool)
		}
		raw, _ := json.Marshal(args)
		res, err := ex.Execute(ctx, spacemanmcp.Call{Principal: p, Tool: tl, Args: raw})
		if err != nil {
			t.Fatal(err)
		}
		return mcpResult(t, res), res.IsError
	}
	code := func(body []byte) string {
		var p struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(body, &p)
		return p.Code
	}

	for _, tc := range []struct {
		name, tool   string
		args         map[string]any
		method, path string
		body         any
		want         int
	}{
		{"own space", "get_space", map[string]any{"space_id": a.ID}, "GET", "/spaces/" + a.ID, nil, 200},
		{"other space", "get_space", map[string]any{"space_id": b.ID}, "GET", "/spaces/" + b.ID, nil, 404},
		{"other space's runs", "list_runs", map[string]any{"space_id": b.ID}, "GET", "/spaces/" + b.ID + "/runs", nil, 404},
		{"missing task", "get_task", map[string]any{"space_id": a.ID, "task_id": "tsk_00000000000000000000000000"},
			"GET", "/spaces/" + a.ID + "/tasks/tsk_00000000000000000000000000", nil, 404},
		{"write as viewer", "update_space", map[string]any{"space_id": a.ID, "if_match": "1", "body": map[string]any{"name": "Renamed"}},
			"PUT", "/spaces/" + a.ID, map[string]any{"name": "Renamed"}, 403},
		{"global read", "list_nodes", map[string]any{}, "GET", "/nodes", nil, 403},
	} {
		got, isErr := call(veraMCP, tc.tool, tc.args)
		var hr resp
		if tc.method == "PUT" {
			hr = bearer.do(tc.method, tc.path, tc.body, "If-Match", `"1"`)
		} else {
			hr = bearer.do(tc.method, tc.path, tc.body)
		}
		if got.Status != tc.want || hr.status != tc.want || isErr != (tc.want >= 400) || code(got.Body) != code(hr.body) {
			t.Errorf("%s: MCP %d (isError %v, %s), HTTP %d (%s), want %d", tc.name, got.Status, isErr, got.Body, hr.status, hr.body, tc.want)
		}
		if got.RequestID == "" {
			t.Errorf("%s: no request ID", tc.name)
		}
	}

	// A retried create (same run, same arguments) is the first create, not a second space.
	adaMCP := principal(ada, adminGrants, "run_ada")
	args := map[string]any{"body": map[string]any{"slug": "gamma", "name": "Gamma", "node_id": en.Node.ID}}
	first, isErr := call(adaMCP, "create_space", args)
	second, isErr2 := call(adaMCP, "create_space", args)
	var s1, s2 struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(first.Body, &s1)
	_ = json.Unmarshal(second.Body, &s2)
	if isErr || isErr2 || first.Status != 201 || second.Status/100 != 2 || s1.ID == "" || s1.ID != s2.ID {
		t.Fatalf("retried create: %+v / %+v", first, second)
	}
	spaces, err := h.c.Fleet.ListSpaces(ctx, func(string) bool { return true })
	if err != nil || len(spaces) != 3 {
		t.Fatalf("%d spaces after a retried create, want 3", len(spaces))
	}
	// Different arguments are a different create; a stale version is the API's conflict.
	if res, isErr := call(adaMCP, "create_space", map[string]any{"body": map[string]any{"slug": "delta", "name": "Delta", "node_id": en.Node.ID}}); isErr || res.Status != 201 {
		t.Fatalf("second create: %+v", res)
	}
	cur, _ := call(adaMCP, "get_space", map[string]any{"space_id": a.ID})
	var sp map[string]any
	_ = json.Unmarshal(cur.Body, &sp)
	upd := map[string]any{"name": "Renamed"}
	for _, k := range []string{"description", "disconnect_policy", "goals", "standing_instructions"} {
		upd[k] = sp[k]
	}
	if res, isErr := call(adaMCP, "update_space", map[string]any{"space_id": a.ID, "if_match": "99", "body": upd}); !isErr || res.Status != 412 && res.Status != 409 {
		t.Fatalf("stale if_match: %+v", res)
	}
	if res, isErr := call(adaMCP, "update_space", map[string]any{"space_id": a.ID, "if_match": sp["version"], "body": upd}); isErr || res.Status != 200 || res.ETag == "" {
		t.Fatalf("update with if_match: %+v", res)
	}

	// The call is audited as the owner, through the internal token, for the run.
	r = root.do("GET", "/audit?action=space.created", nil)
	expect(t, r, 200)
	var page struct {
		Items []struct {
			ActorID string `json:"actor_id"`
			Details struct {
				Run string `json:"run"`
				Via string `json:"via"`
			} `json:"details"`
		} `json:"items"`
	}
	r.json(t, &page)
	found := false
	for _, rec := range page.Items {
		if rec.Details.Run == "run_ada" && rec.ActorID == ada.ID && rec.Details.Via == auth.ViaSpacemanMCP+adaMCP.TokenID {
			found = true
		}
	}
	if !found {
		t.Errorf("no space.created audit record by Ada via the SpaceMan MCP for run_ada: %s", r.body)
	}
}
