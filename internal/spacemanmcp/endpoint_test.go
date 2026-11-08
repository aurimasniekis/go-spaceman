package spacemanmcp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/spacemanmcp"
)

// The external MCP endpoint offers what the token's grants reach in the read, write and dangerous classes; dangerous
// tools that act on no space (an approval belongs to a space) and sensitive reads are left out, and excluded
// operations never appear.
func TestEndpointTools(t *testing.T) {
	t.Parallel()
	c := catalog(t)
	e := &spacemanmcp.Endpoint{Cat: c}
	all := names(e.EndpointTools([]authz.Grant{{Role: authz.RoleAdmin, Domain: authz.Global}}))
	if slices.Contains(all, "purge_space") || slices.Contains(all, "create_mcp_server") || slices.Contains(all, "update_node") {
		t.Errorf("step-up or space-less dangerous tools offered: %v", all)
	}
	for _, want := range []string{"list_spaces", "create_space", "create_task", "delete_schedule", "save_space_role", "launch_run", "delete_space_role"} {
		if !slices.Contains(all, want) {
			t.Errorf("admin@* misses %s", want)
		}
	}
	for _, tl := range c.Tools() {
		in := slices.Contains(all, tl.Name)
		spaceless := !slices.Contains(tl.PathParams, "space_id")
		switch {
		case tl.Class == spacemanmcp.SensitiveRead && in:
			t.Errorf("sensitive read %s offered", tl.Name)
		case tl.Class == spacemanmcp.Dangerous && spaceless && in:
			t.Errorf("space-less dangerous %s offered", tl.Name)
		case tl.Class == spacemanmcp.Dangerous && !spaceless && !in:
			t.Errorf("dangerous %s in a space not offered to admin@*", tl.Name)
		}
	}
	for op := range c.Excluded() {
		if slices.Contains(all, toName(op)) {
			t.Errorf("excluded %s offered", op)
		}
	}
	viewer := names(e.EndpointTools([]authz.Grant{{Role: authz.RoleViewer, Domain: "spc_a"}}))
	if !slices.Contains(viewer, "list_schedules") || slices.Contains(viewer, "create_task") || slices.Contains(viewer, "delete_schedule") ||
		slices.Contains(viewer, "create_space") {
		t.Errorf("viewer@space tools %v", viewer)
	}
	if got := e.EndpointTools(nil); len(got) != 0 {
		t.Errorf("no grants: %v", names(got))
	}
}

func toName(op string) string {
	b := []byte(op)
	for i := range b {
		if b[i] == '-' {
			b[i] = '_'
		}
	}
	return string(b)
}

// A call through the endpoint runs with the token's own principal and no derived Idempotency-Key: a person drives the
// client, so a repeated create is meant.
func TestExecutorEndpointPrincipal(t *testing.T) {
	t.Parallel()
	c := catalog(t)
	var rec seen
	ex := &spacemanmcp.Executor{Handler: echo(&rec, 201, `{"id":"spc_1"}`)}
	p := &auth.Principal{User: &auth.User{ID: "usr_owner"}, TokenID: "atk_ci", Method: auth.MethodToken, MCPEndpoint: true}
	create, _ := c.Tool("create_space")
	r, err := ex.Execute(context.Background(), spacemanmcp.Call{Principal: p, Tool: create, Args: json.RawMessage(`{"body":{"name":"a","slug":"a"}}`)})
	if err != nil || r.IsError || rec.method != http.MethodPost || rec.header.Get("Idempotency-Key") != "" {
		t.Fatalf("%v %+v %+v", err, r, rec)
	}
}
