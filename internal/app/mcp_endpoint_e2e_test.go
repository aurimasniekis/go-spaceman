package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/aurimasniekis/go-spaceman/internal/api"
	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/approvals"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/human"
	"github.com/aurimasniekis/go-spaceman/internal/store"
)

// bearerTransport adds an Authorization header (and optional extra headers) to every request.
type bearerTransport struct {
	bearer string
	extra  map[string]string
}

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if b.bearer != "" {
		r.Header.Set("Authorization", "Bearer "+b.bearer)
	}
	for k, v := range b.extra {
		r.Header.Set(k, v)
	}
	return http.DefaultTransport.RoundTrip(r)
}

// mcpConnect connects the go MCP SDK's client to the external MCP endpoint over streamable HTTP.
func mcpConnect(ctx context.Context, h *harness, bearer string) (*mcp.ClientSession, error) {
	c := mcp.NewClient(&mcp.Implementation{Name: "operator-client", Version: "test"}, nil)
	return c.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: h.srv.URL + api.MCPPath,
		HTTPClient: &http.Client{Transport: bearerTransport{bearer: bearer}}}, nil)
}

func callText(t *testing.T, r *mcp.CallToolResult) string {
	t.Helper()
	if len(r.Content) != 1 {
		t.Fatalf("content %+v", r.Content)
	}
	return r.Content[0].(*mcp.TextContent).Text
}

// The external MCP endpoint (ADR-0063 §External endpoint, SM-MCP-009) with a real MCP client over HTTP: an operator's
// API token initializes, lists the tools its grants reach (no excluded operation, nothing beyond its space), reads and
// writes at once, and needs another person's approval for a dangerous call, which then runs once. Its own user cannot
// approve. Internal tokens, browsers, sessions and the root token are refused; calls are audited via the token on the
// mcp_endpoint channel.
func TestMCPEndpoint(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	sys := audit.Actor{Type: audit.ActorSystem, ID: "test"}
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
	var alpha, beta struct {
		ID string `json:"id"`
	}
	r := root.do("POST", "/spaces", map[string]any{"slug": "alpha", "name": "Alpha", "node_id": en.Node.ID})
	expect(t, r, 201)
	r.json(t, &alpha)
	r = root.do("POST", "/spaces", map[string]any{"slug": "beta", "name": "Beta", "node_id": en.Node.ID})
	expect(t, r, 201)
	r.json(t, &beta)

	// Olga administers alpha and uses a token for it; Bob administers everything and approves.
	olgaGrants := []auth.GrantInput{{Role: authz.RoleAdmin, Domain: alpha.ID}}
	olga, err := h.c.Auth.Create(ctx, sys, auth.CreateInput{Email: "olga@example.com", DisplayName: "Olga", Password: "correct horse battery staple",
		Grants: olgaGrants})
	if err != nil {
		t.Fatal(err)
	}
	bob, err := h.c.Auth.Create(ctx, sys, auth.CreateInput{Email: "bob@example.com", DisplayName: "Bob",
		Grants: []auth.GrantInput{{Role: authz.RoleAdmin, Domain: authz.Global}}})
	if err != nil {
		t.Fatal(err)
	}
	secret := loginAndIssueToken(t, h, "olga@example.com", []map[string]string{{"role": "admin", "domain": alpha.ID}})
	tok, err := h.c.Auth.AuthenticateAPIToken(ctx, secret, "")
	if err != nil {
		t.Fatal(err)
	}
	tokenID := tok.TokenID

	// initialize
	cs, err := mcpConnect(ctx, h, secret)
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	defer cs.Close()
	if ir := cs.InitializeResult(); ir == nil || ir.ServerInfo == nil || ir.ServerInfo.Name != "spaceman" {
		t.Fatalf("initialize result %+v", ir)
	}

	// tools/list: what the token's grants reach in alpha; nothing global, nothing excluded.
	var names []string
	byName := map[string]*mcp.Tool{}
	for tl, err := range cs.Tools(ctx, nil) {
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, tl.Name)
		byName[tl.Name] = tl
	}
	for _, want := range []string{"list_schedules", "create_schedule", "delete_schedule", "get_space", "create_task", "launch_run"} {
		if byName[want] == nil {
			t.Fatalf("tools/list misses %s: %v", want, names)
		}
	}
	if d := byName["delete_schedule"].Annotations.DestructiveHint; !byName["list_schedules"].Annotations.ReadOnlyHint ||
		byName["create_schedule"].Annotations.ReadOnlyHint || d == nil || !*d {
		t.Fatalf("annotations %+v / %+v / %+v", byName["list_schedules"].Annotations, byName["create_schedule"].Annotations, byName["delete_schedule"].Annotations)
	}
	for _, absent := range []string{"create_space", "list_nodes", "create_mcp_server", "list_recordings"} {
		if byName[absent] != nil {
			t.Fatalf("tools/list offers %s to a token for one space", absent)
		}
	}
	excluded := h.c.SpacemanCatalog().Excluded()
	for _, n := range names {
		if _, ok := excluded[strings.ReplaceAll(n, "_", "-")]; ok {
			t.Fatalf("tools/list offers the excluded %s", n)
		}
	}
	for _, op := range []string{"create-api-token", "respond-human-action", "login", "set-user-grants"} {
		if _, ok := excluded[op]; !ok || byName[strings.ReplaceAll(op, "-", "_")] != nil {
			t.Fatalf("%s should be excluded and absent", op)
		}
	}

	// A write call runs at once.
	sched := map[string]any{"name": "weekly review", "kind": "cron", "cron": "0 9 * * 1", "timezone": "UTC", "missed": "run_once",
		"target": "inbox", "title": "Weekly review", "body": "Look at the queue."}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "create_schedule", Arguments: map[string]any{"space_id": alpha.ID, "body": sched}})
	if err != nil {
		t.Fatal(err)
	}
	var created struct {
		Status int `json:"status"`
		Body   struct {
			ID string `json:"id"`
		} `json:"body"`
	}
	if err := json.Unmarshal([]byte(callText(t, res)), &created); err != nil || res.IsError || created.Status != 201 || created.Body.ID == "" {
		t.Fatalf("create_schedule: %v %s", err, callText(t, res))
	}
	// A read call; space_id defaults to the token's one space.
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "list_schedules", Arguments: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || !strings.Contains(callText(t, res), created.Body.ID) {
		t.Fatalf("list_schedules: %s", callText(t, res))
	}
	// Another space is missing for this token.
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "list_schedules", Arguments: map[string]any{"space_id": beta.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(callText(t, res), `"status":404`) {
		t.Fatalf("list_schedules in another space: %s", callText(t, res))
	}

	// A dangerous call needs an approval first; nothing runs.
	del := &mcp.CallToolParams{Name: "delete_schedule", Arguments: map[string]any{"space_id": alpha.ID, "schedule_id": created.Body.ID}}
	res, err = cs.CallTool(ctx, del)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(callText(t, res), "needs a person's approval") {
		t.Fatalf("dangerous call without an approval: %s", callText(t, res))
	}
	// A dangerous call in a space the token cannot use asks nobody.
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "delete_schedule", Arguments: map[string]any{"space_id": beta.ID, "schedule_id": "sch_x"}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || strings.Contains(callText(t, res), "approval") {
		t.Fatalf("dangerous call in another space: %s", callText(t, res))
	}
	if l, _ := h.c.Approvals.List(ctx, beta.ID, 10); len(l) != 0 {
		t.Fatalf("approvals in beta: %+v", l)
	}
	// So does a diagnostic launch.
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "launch_run", Arguments: map[string]any{"space_id": alpha.ID,
		"body": map[string]any{"kind": "diagnostic", "role": "control"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(callText(t, res), "needs a person's approval") {
		t.Fatalf("diagnostic launch without an approval: %s", callText(t, res))
	}
	l, err := h.c.Approvals.List(ctx, alpha.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	var appr *approvals.Approval
	for _, a := range l {
		if a.Operation == "mcp.endpoint.dangerous.delete_schedule" {
			appr = a
		}
	}
	if appr == nil || len(l) != 2 || appr.RequesterRun != approvals.TokenRequesterPrefix+tokenID || appr.RequesterRole != "mcp_endpoint" {
		t.Fatalf("approvals %+v", l)
	}
	act, err := h.c.Human.Get(ctx, alpha.ID, appr.HumanActionID)
	if err != nil {
		t.Fatal(err)
	}
	if act.RequesterKind != "operator" || !strings.Contains(act.Body, "Another person must decide") {
		t.Fatalf("approval request %+v", act)
	}
	// The token's own user can't approve: neither through the token nor in another session of theirs.
	for _, by := range []audit.Actor{
		{Type: audit.ActorUser, ID: olga.ID, Via: "api_token:" + tokenID},
		{Type: audit.ActorUser, ID: olga.ID},
		{Type: audit.ActorUser, ID: olga.ID, Via: "api_token:" + tokenID, Channel: audit.ChannelMCPEndpoint},
	} {
		if _, err := h.c.Human.Respond(ctx, by, alpha.ID, act.ID, act.Version, human.RespondInput{Outcome: "approved"}); !errors.Is(err, apperr.ErrForbidden) {
			t.Fatalf("%+v approved its own token's request: %v", by, err)
		}
	}
	// Still pending.
	res, err = cs.CallTool(ctx, del)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(callText(t, res), "is waiting for a person") {
		t.Fatalf("dangerous call while pending: %s", callText(t, res))
	}
	if _, err := h.c.Human.Respond(ctx, audit.Actor{Type: audit.ActorUser, ID: bob.ID}, alpha.ID, act.ID, act.Version,
		human.RespondInput{Outcome: "approved"}); err != nil {
		t.Fatal(err)
	}
	// Approved by another person: runs once.
	res, err = cs.CallTool(ctx, del)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || !strings.Contains(callText(t, res), `"status":204`) {
		t.Fatalf("approved dangerous call: %s", callText(t, res))
	}
	expect(t, root.do("GET", "/spaces/"+alpha.ID+"/schedules/"+created.Body.ID, nil), 404)
	res, err = cs.CallTool(ctx, del)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(callText(t, res), "needs a person's approval") {
		t.Fatalf("second dangerous call on a one-use approval: %s", callText(t, res))
	}

	// Audit: the operation as the token's user via the token on the mcp_endpoint channel, and each tool call.
	var page struct {
		Items []audit.Record `json:"items"`
	}
	root.do("GET", "/audit?action=schedule.created&target_id="+created.Body.ID, nil).json(t, &page)
	if len(page.Items) != 1 || page.Items[0].ActorID != olga.ID || page.Items[0].Details["via"] != "api_token:"+tokenID ||
		page.Items[0].Details["channel"] != "mcp_endpoint" {
		t.Fatalf("schedule.created audit %+v", page.Items)
	}
	page.Items = nil
	root.do("GET", "/audit?action=mcp_endpoint.tool_called&limit=50", nil).json(t, &page)
	results := map[string]int{}
	for _, it := range page.Items {
		if it.ActorID != olga.ID || it.TargetID != tokenID || it.Details["via"] != "api_token:"+tokenID || it.Details["channel"] != "mcp_endpoint" {
			t.Fatalf("tool call audit %+v", it)
		}
		results[it.Details["tool"].(string)+":"+it.Details["result"].(string)]++
	}
	if results["create_schedule:ok"] != 1 || results["list_schedules:ok"] != 1 || results["delete_schedule:approval_required"] != 3 ||
		results["delete_schedule:ok"] != 1 || results["launch_run:approval_required"] != 1 {
		t.Fatalf("tool call audit results %v", results)
	}

	// Refusals at the edge.
	post := func(hdr map[string]string, bearer string) int {
		t.Helper()
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, h.srv.URL+api.MCPPath,
			strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		res, err := (&http.Client{Transport: bearerTransport{bearer: bearer, extra: hdr}}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if res.Header.Get("Access-Control-Allow-Origin") != "" {
			t.Fatalf("CORS header on the MCP endpoint")
		}
		return res.StatusCode
	}
	if s := post(nil, secret); s != 200 {
		t.Fatalf("plain tools/list: %d", s)
	}
	if s := post(map[string]string{"Origin": "https://evil.example.com"}, secret); s != 403 {
		t.Fatalf("browser origin: %d", s)
	}
	if s := post(map[string]string{"Origin": h.srv.URL}, secret); s != 403 {
		t.Fatalf("own origin (a browser): %d", s)
	}
	if s := post(map[string]string{"Sec-Fetch-Site": "cross-site"}, secret); s != 403 {
		t.Fatalf("browser fetch: %d", s)
	}
	// Without a bearer the API's cross-site check answers first (a POST with no session and no same origin).
	if s := post(nil, ""); s != 403 {
		t.Fatalf("no token: %d", s)
	}
	if s := post(nil, rootSecret); s != 403 {
		t.Fatalf("root token: %d", s)
	}
	if _, err := mcpConnect(ctx, h, rootSecret); err == nil {
		t.Fatal("root token connected")
	}

	// An internal token (purpose spaceman_mcp) is refused: here a bearer token whose row says it is internal.
	tok2, secret2, err := h.c.Auth.CreateAPIToken(ctx, &auth.Principal{User: olga, Method: auth.MethodPassword}, "second",
		[]auth.GrantInput{{Role: authz.RoleViewer, Domain: alpha.ID}}, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	if s := post(nil, secret2); s != 200 {
		t.Fatalf("second token before: %d", s)
	}
	if err := h.c.Store.W().Tx(ctx, func(tx *store.Tx) error {
		_, err := tx.SQL.ExecContext(ctx, `UPDATE api_tokens SET purpose = ? WHERE id = ?`, auth.TokenPurposeSpacemanMCP, tok2.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if s := post(nil, secret2); s != 401 {
		t.Fatalf("internal token: %d", s)
	}
	if _, err := mcpConnect(ctx, h, secret2); err == nil {
		t.Fatal("internal token connected")
	}
}
