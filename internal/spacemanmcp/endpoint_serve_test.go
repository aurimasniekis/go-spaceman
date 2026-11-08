package spacemanmcp_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/approvals"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/gateway"
	"github.com/aurimasniekis/go-spaceman/internal/spacemanmcp"
)

type endpointTokens struct{ grants []authz.Grant }

func (f endpointTokens) GetAPIToken(_ context.Context, id string) (*auth.APIToken, error) {
	if id == "atk_gone" {
		return nil, errors.New("boom")
	}
	return &auth.APIToken{ID: id, UserID: "usr_owner", Grants: f.grants}, nil
}

type allowSpaces map[string]bool

func (a allowSpaces) AllowedAll(_ []string, domain string, _ authz.Permission) bool { return a[domain] }

// fakeApprover records each request; err, when set, is returned instead of letting the call run.
type fakeApprover struct {
	mu       sync.Mutex
	err      error
	reqs     []approvals.Requester
	ops      []string
	finished []error
}

func (f *fakeApprover) RequireFor(_ context.Context, r approvals.Requester, op, _ string, _ json.RawMessage) (func(error), error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs, f.ops = append(f.reqs, r), append(f.ops, op)
	if f.err != nil {
		return nil, f.err
	}
	return func(err error) { f.mu.Lock(); f.finished = append(f.finished, err); f.mu.Unlock() }, nil
}

// apiStub answers like the API: status and body, recording the approved-call mark the request carried.
type apiStub struct {
	mu       sync.Mutex
	status   int
	approved []string
}

func (s *apiStub) set(status int) { s.mu.Lock(); s.status = status; s.mu.Unlock() }

func (s *apiStub) marks() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.approved...)
}

func (f *fakeApprover) setErr(err error) { f.mu.Lock(); f.err = err; f.mu.Unlock() }

func (f *fakeApprover) snapshot() ([]approvals.Requester, []string, []error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]approvals.Requester(nil), f.reqs...), append([]string(nil), f.ops...), append([]error(nil), f.finished...)
}

func (s *apiStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.approved = append(s.approved, gateway.ApprovedCall(r.Context()))
	st := s.status
	s.mu.Unlock()
	_, _ = io.Copy(io.Discard, r.Body)
	w.WriteHeader(st)
	_, _ = fmt.Fprintf(w, `{"status":%d}`, st)
}

// The endpoint served over HTTP to the go MCP SDK client, with fakes around it: tools for the token's grants, read
// calls at once, dangerous calls and diagnostic launches through the approver (bound to the token, the token's user
// never deciding) and then marked approved for the API, refusals as tool errors, and an audit event per call.
func TestEndpointServe(t *testing.T) {
	t.Parallel()
	const space = "spc_0000000000000000000000000a"
	api := &apiStub{status: 200}
	appr := &fakeApprover{}
	var mu sync.Mutex
	var events []audit.Event
	e := &spacemanmcp.Endpoint{Cat: catalog(t), Handler: api, Approvals: appr, Authz: allowSpaces{space: true},
		Tokens: endpointTokens{grants: []authz.Grant{{Role: authz.RoleAdmin, Domain: space}}},
		Record: func(_ context.Context, ev audit.Event) error {
			mu.Lock()
			defer mu.Unlock()
			events = append(events, ev)
			return errors.New("audit store down") // logged, never fails the call
		}}
	principal := &auth.Principal{User: &auth.User{ID: "usr_owner", DisplayName: "Olga"}, TokenID: "atk_1", Method: auth.MethodToken, MCPEndpoint: true}
	spacemanmcp.SetEndpointPrincipal(e, func(r *http.Request) *auth.Principal {
		switch r.Header.Get("X-Test") {
		case "none":
			return nil
		case "plain":
			p := *principal
			p.MCPEndpoint = false
			return &p
		case "gone":
			p := *principal
			p.TokenID = "atk_gone"
			return &p
		}
		return principal
	})
	srv := httptest.NewServer(e)
	defer srv.Close()

	for hdr, want := range map[string]int{"none": 403, "plain": 403, "gone": 500} {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL, strings.NewReader(`{}`))
		req.Header.Set("X-Test", hdr)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		if res.StatusCode != want {
			t.Errorf("%s: %d, want %d", hdr, res.StatusCode, want)
		}
	}

	ctx := context.Background()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "1"}, nil).Connect(ctx, &mcp.StreamableClientTransport{Endpoint: srv.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	tools := map[string]*mcp.Tool{}
	for tl, err := range cs.Tools(ctx, nil) {
		if err != nil {
			t.Fatal(err)
		}
		tools[tl.Name] = tl
	}
	if tools["list_schedules"] == nil || tools["delete_schedule"] == nil || tools["create_space"] != nil ||
		!strings.Contains(tools["delete_schedule"].Description, "another person's approval") {
		t.Fatalf("tools %d", len(tools))
	}
	call := func(name string, args any) (*mcp.CallToolResult, string) {
		t.Helper()
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		return res, res.Content[0].(*mcp.TextContent).Text
	}

	// A read runs at once, without an approval, with the default space.
	if res, txt := call("list_schedules", map[string]any{}); res.IsError || !strings.Contains(txt, `"status":200`) {
		t.Fatalf("read: %s", txt)
	}
	// A dangerous call is approved first (bound to the token, the token's user excluded), then marked approved.
	if res, txt := call("delete_schedule", map[string]any{"schedule_id": "sch_1"}); res.IsError {
		t.Fatalf("approved dangerous call: %s", txt)
	}
	reqs, ops, fin := appr.snapshot()
	if len(reqs) != 1 || ops[0] != "mcp.endpoint.dangerous.delete_schedule" || reqs[0].SpaceID != space ||
		reqs[0].Key != "api_token:atk_1" || reqs[0].SelfUserID != "usr_owner" || reqs[0].Role != "mcp_endpoint" ||
		reqs[0].Actor.Channel != audit.ChannelMCPEndpoint || len(fin) != 1 || fin[0] != nil {
		t.Fatalf("approval %+v %v %v", reqs, ops, fin)
	}
	if m := api.marks(); m[len(m)-1] != "delete-schedule" || m[0] != "" {
		t.Fatalf("approved marks %v", m)
	}
	// A diagnostic launch is dangerous too; an API error finishes the approval as failed.
	api.set(503)
	if res, _ := call("launch_run", map[string]any{"space_id": space, "body": map[string]any{"kind": " Diagnostic "}}); !res.IsError {
		t.Fatal("failed launch is not an error")
	}
	_, ops, fin = appr.snapshot()
	if m := api.marks(); ops[1] != "mcp.endpoint.dangerous.launch_run" || fin[1] == nil || m[len(m)-1] != "launch-run" {
		t.Fatalf("diagnostic launch: %v %v %v", ops, fin, m)
	}
	api.set(200)
	// Pending approval: a tool error naming it, and nothing reaches the API.
	n := len(api.marks())
	appr.setErr(fmt.Errorf("%w: %w", apperr.New(apperr.CodePrecondition, "needs a person's approval"), approvals.ErrRequired))
	if res, txt := call("delete_schedule", map[string]any{"schedule_id": "sch_2"}); !res.IsError || !strings.Contains(txt, "needs a person's approval") {
		t.Fatalf("pending: %s", txt)
	}
	// An internal failure is not shown in detail.
	appr.setErr(errors.New("database is locked"))
	if res, txt := call("delete_schedule", map[string]any{"schedule_id": "sch_3"}); !res.IsError || txt != "internal error" {
		t.Fatalf("internal failure: %s", txt)
	}
	appr.setErr(nil)
	// A space the token can't use asks nobody.
	reqs, _, _ = appr.snapshot()
	asked := len(reqs)
	if res, txt := call("delete_schedule", map[string]any{"space_id": "spc_other", "schedule_id": "sch_4"}); !res.IsError || !strings.Contains(txt, "space not found") {
		t.Fatalf("other space: %s", txt)
	}
	if reqs, _, _ = appr.snapshot(); len(reqs) != asked || len(api.marks()) != n {
		t.Fatal("asked for or ran a call in another space")
	}
	// Oversized arguments are refused.
	if res, txt := call("list_schedules", map[string]any{"q": strings.Repeat("x", 300<<10)}); !res.IsError || !strings.Contains(txt, "larger than") {
		t.Fatalf("large args: %.80s", txt)
	}

	mu.Lock()
	defer mu.Unlock()
	results := map[string]int{}
	for _, ev := range events {
		if ev.Action != "mcp_endpoint.tool_called" || ev.Actor.Via != "api_token:atk_1" || ev.Actor.Channel != audit.ChannelMCPEndpoint ||
			ev.Actor.ID != "usr_owner" || ev.Target.ID != "atk_1" {
			t.Fatalf("event %+v", ev)
		}
		results[fmt.Sprint(ev.Details["tool"], ":", ev.Details["result"])]++
	}
	if results["list_schedules:ok"] != 1 || results["delete_schedule:ok"] != 1 || results["launch_run:api_error"] != 1 ||
		results["delete_schedule:approval_required"] != 1 || results["delete_schedule:internal"] != 1 || results["delete_schedule:not_found"] != 1 ||
		results["list_schedules:validation_failed"] != 1 {
		t.Fatalf("audit results %v", results)
	}
}

// Without an approval gate, or in no space, a dangerous call is refused rather than run.
func TestEndpointDangerousNeedsGateAndSpace(t *testing.T) {
	t.Parallel()
	api := &apiStub{status: 200}
	grants := []authz.Grant{{Role: authz.RoleAdmin, Domain: "spc_0000000000000000000000000a"}, {Role: authz.RoleAdmin, Domain: "spc_0000000000000000000000000b"}}
	e := &spacemanmcp.Endpoint{Cat: catalog(t), Handler: api, Authz: allowSpaces{"spc_0000000000000000000000000a": true}, Tokens: endpointTokens{grants: grants},
		Record: func(context.Context, audit.Event) error { return nil }}
	p := &auth.Principal{User: &auth.User{ID: "usr_owner", Email: "o@example.com"}, TokenID: "atk_1", Method: auth.MethodToken, MCPEndpoint: true}
	spacemanmcp.SetEndpointPrincipal(e, func(*http.Request) *auth.Principal { return p })
	srv := httptest.NewServer(e)
	defer srv.Close()
	ctx := context.Background()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "1"}, nil).Connect(ctx, &mcp.StreamableClientTransport{Endpoint: srv.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	for args, want := range map[string]string{
		`{"schedule_id":"s"}`: "pass space_id", // two spaces: no default
		`{"space_id":"spc_0000000000000000000000000a","schedule_id":"s"}`: "approvals are not available",
	} {
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "delete_schedule", Arguments: json.RawMessage(args)})
		if err != nil {
			t.Fatal(err)
		}
		if txt := res.Content[0].(*mcp.TextContent).Text; !res.IsError || !strings.Contains(txt, want) {
			t.Errorf("%s: %s", args, txt)
		}
	}
	if len(api.marks()) != 0 {
		t.Fatal("a dangerous call reached the API")
	}
}
