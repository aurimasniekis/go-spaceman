package spacemanmcp_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/aurimasniekis/go-spaceman/internal/api"
	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/spacemanmcp"
)

type seen struct {
	method, uri, body string
	header            http.Header
}

// echo is a stand-in API handler: it records the request and answers with status and body.
func echo(rec *seen, status int, body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*rec = seen{method: r.Method, uri: r.URL.RequestURI(), body: string(b), header: r.Header.Clone()}
		w.Header().Set("ETag", `"7"`)
		w.Header().Set(api.RequestIDHeader, "req_test")
		if status != 0 {
			w.WriteHeader(status)
		}
		_, _ = io.WriteString(w, body)
	})
}

func result(t *testing.T, r *mcp.CallToolResult) spacemanmcp.Result {
	t.Helper()
	if len(r.Content) != 1 {
		t.Fatalf("content %+v", r.Content)
	}
	var out spacemanmcp.Result
	if err := json.Unmarshal([]byte(r.Content[0].(*mcp.TextContent).Text), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func runPrincipal() *auth.Principal {
	return &auth.Principal{User: &auth.User{ID: "usr_owner"}, TokenID: "atk_mcp", TokenPurpose: auth.TokenPurposeSpacemanMCP,
		RunID: "run_a", Method: auth.MethodInternal}
}

// The executor maps arguments onto the operation's request: escaped path parameters, query parameters, the JSON body,
// a quoted If-Match, and a derived Idempotency-Key; a missing space_id takes the server's default space.
func TestExecutorBuildsRequests(t *testing.T) {
	t.Parallel()
	c := catalog(t)
	var rec seen
	ex := &spacemanmcp.Executor{Handler: echo(&rec, 200, `{"ok":true}`)}
	ctx := context.Background()
	p := runPrincipal()

	upd, _ := c.Tool("update_space")
	r, err := ex.Execute(ctx, spacemanmcp.Call{Principal: p, Tool: upd, Args: json.RawMessage(`{"space_id":"spc/x y","if_match":3,"body":{"name":"n"}}`)})
	if err != nil || r.IsError {
		t.Fatalf("%v %+v", err, r)
	}
	if rec.method != http.MethodPut || rec.uri != api.Prefix+"/spaces/spc%2Fx%20y" || rec.header.Get("If-Match") != `"3"` ||
		rec.body != `{"name":"n"}` || rec.header.Get("Content-Type") != "application/json" || rec.header.Get("Idempotency-Key") != "" {
		t.Fatalf("request %+v", rec)
	}
	if got := result(t, r); got.Status != 200 || got.ETag != `"7"` || got.RequestID != "req_test" || string(got.Body) != `{"ok":true}` {
		t.Fatalf("result %+v", got)
	}

	list, _ := c.Tool("list_inbox")
	if _, err := ex.Execute(ctx, spacemanmcp.Call{Principal: p, Tool: list, DefaultSpaceID: "spc_default", Args: json.RawMessage(`{"limit":5}`)}); err != nil {
		t.Fatal(err)
	}
	if rec.method != http.MethodGet || !strings.HasPrefix(rec.uri, api.Prefix+"/spaces/spc_default/inbox?") || !strings.Contains(rec.uri, "limit=5") || rec.body != "" {
		t.Fatalf("request %+v", rec)
	}

	create, _ := c.Tool("create_space")
	args := json.RawMessage(`{"body":{"name":"infra","slug":"infra"}}`)
	if _, err := ex.Execute(ctx, spacemanmcp.Call{Principal: p, Tool: create, Args: args}); err != nil {
		t.Fatal(err)
	}
	key := rec.header.Get("Idempotency-Key")
	reordered := json.RawMessage(`{ "body": {"slug":"infra", "name":"infra"} }`)
	if want, _ := spacemanmcp.IdempotencyKey(p, create, reordered); key == "" || key != want || len(key) > 128 {
		t.Fatalf("key %q, reordered %q", key, want)
	}
	other := *p
	other.RunID = "run_b"
	for _, k := range []func() (string, error){
		func() (string, error) { return spacemanmcp.IdempotencyKey(&other, create, args) },
		func() (string, error) {
			return spacemanmcp.IdempotencyKey(p, create, json.RawMessage(`{"body":{"name":"infra2","slug":"infra"}}`))
		},
	} {
		if k2, _ := k(); k2 == key {
			t.Fatal("distinct calls share an idempotency key")
		}
	}
}

// Bad arguments and API problems are tool errors the model reads, never Go errors.
func TestExecutorErrors(t *testing.T) {
	t.Parallel()
	c := catalog(t)
	var rec seen
	ctx := context.Background()
	get, _ := c.Tool("get_space")
	ex := &spacemanmcp.Executor{Handler: echo(&rec, 404, `{"status":404,"code":"not_found","title":"Not Found","detail":"space not found"}`)}
	r, err := ex.Execute(ctx, spacemanmcp.Call{Principal: runPrincipal(), Tool: get, Args: json.RawMessage(`{"space_id":"spc_x"}`)})
	if err != nil || !r.IsError {
		t.Fatalf("%v %+v", err, r)
	}
	if got := result(t, r); got.Status != 404 || !strings.Contains(string(got.Body), "space not found") {
		t.Fatalf("result %+v", got)
	}
	for _, args := range []string{`{}`, `{"space_id":"spc_x","extra":1}`, `[1]`, `{"space_id":{"a":1}}`, `{"space_id":"a"} {}`} {
		rec = seen{}
		r, err := ex.Execute(ctx, spacemanmcp.Call{Principal: runPrincipal(), Tool: get, Args: json.RawMessage(args)})
		if err != nil || !r.IsError || rec.method != "" {
			t.Errorf("args %s: %v %+v (sent %v)", args, err, r, rec.method != "")
		}
		if got := result(t, r); got.Status != 400 {
			t.Errorf("args %s: %+v", args, got)
		}
	}
	if _, err := ex.Execute(ctx, spacemanmcp.Call{Tool: get}); err == nil {
		t.Error("a call without a principal ran")
	}
}

// Results larger than the cap are cut: the status and request ID stay, the body is dropped with a note.
func TestExecutorCapsResults(t *testing.T) {
	t.Parallel()
	c := catalog(t)
	var rec seen
	big := `{"items":["` + strings.Repeat("x", 5000) + `"]}`
	ex := &spacemanmcp.Executor{Handler: echo(&rec, 0, big), MaxResultBytes: 1024}
	tl, _ := c.Tool("list_nodes")
	r, err := ex.Execute(context.Background(), spacemanmcp.Call{Principal: runPrincipal(), Tool: tl})
	if err != nil || r.IsError {
		t.Fatalf("%v %+v", err, r)
	}
	got := result(t, r)
	if got.Status != 200 || !got.Truncated || got.Bytes != len(big) || got.Body != nil || !strings.Contains(got.Note, "1024-byte") {
		t.Fatalf("result %+v", got)
	}
	ex.MaxResultBytes = 0
	r, _ = ex.Execute(context.Background(), spacemanmcp.Call{Principal: runPrincipal(), Tool: tl})
	if got := result(t, r); got.Truncated || string(got.Body) != big {
		t.Fatalf("default cap: %+v", got.Truncated)
	}
	ex = &spacemanmcp.Executor{Handler: echo(&rec, 200, "not json")}
	r, _ = ex.Execute(context.Background(), spacemanmcp.Call{Principal: runPrincipal(), Tool: tl})
	if got := result(t, r); got.Body != nil || got.Note == "" {
		t.Fatalf("non-JSON: %+v", got)
	}
}
