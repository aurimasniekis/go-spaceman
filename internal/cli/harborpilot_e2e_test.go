package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/harborpilot/hpfake"
)

// SM-GW-001/002 through the CLI: connecting reads the tokens from files and never prints them; a later connect without
// token flags keeps them (the If-Match comes from the connection read first); a space's settings merge the flags given
// into the settings in force; disconnecting is refused while a space uses Harbor Pilot.
func TestHarborPilotCommands(t *testing.T) {
	c := startCentral(t)
	e := c.env()
	f := hpfake.New(t)
	e.ok(t, "node", "create", "n1")
	space := e.obj(t, "space", "create", "payments", "--name", "Payments", "--node", "n1")

	e.code(t, ExitUsage, "harbor-pilot", "connect", "--base-url", f.URL(), "--parent-token", f.Parent)
	parent := writeFile(t, "parent", f.Parent+"\n", 0o600)
	service := writeFile(t, "service", f.Service+"\n", 0o600)
	out := e.ok(t, "harbor-pilot", "connect", "--base-url", f.URL(), "--allow-insecure-http", "--parent-token-file", parent, "--service-token-file", service)
	if strings.Contains(out, f.Parent) || strings.Contains(out, f.Service) || !strings.Contains(out, "bnd_fake") {
		t.Fatalf("connect output %s", out)
	}
	// A new gateway address needs the tokens again: the stored ones are never sent to it.
	if r := e.run(t, "hp", "connect", "--run-base-url", "https://hp.example.com"); r.code == 0 || !strings.Contains(r.err+r.out, "re-enter the tokens") {
		t.Fatalf("address change without tokens: %+v", r)
	}
	conn := e.obj(t, "hp", "connect", "--run-base-url", "https://hp.example.com", "--parent-token-file", parent, "--service-token-file", service)
	if str(conn, "run_base_url") != "https://hp.example.com" || conn["parent_token_version"].(float64) != 2 || conn["has_service_token"] != true || conn["version"].(float64) != 2 {
		t.Fatalf("second connect %v", conn)
	}

	sp := e.obj(t, "harbor-pilot", "set-space", "--space", "payments", "--enabled", "--project", "prj_web", "--harness", "codex")
	if sp["enabled"] != true || str(sp, "project_id") != "prj_web" || sp["ttl_seconds"].(float64) != 86400 {
		t.Fatalf("set-space %v", sp)
	}
	sp = e.obj(t, "harbor-pilot", "set-space", "--space", "payments", "--ttl", "7200", "--on-budget-status-unavailable", "refuse")
	if sp["enabled"] != true || str(sp, "project_id") != "prj_web" || sp["ttl_seconds"].(float64) != 7200 || str(sp, "on_budget_status_unavailable") != "refuse" ||
		sp["version"].(float64) != 2 {
		t.Fatalf("merged set-space %v", sp)
	}
	if got := e.obj(t, "harbor-pilot", "space", "--space", "payments"); got["harnesses"].([]any)[0] != "codex" {
		t.Fatalf("space %v", got)
	}
	// A space operator changes the rest (the stored project is kept), but not the project: that needs account:write.
	ctx := context.Background()
	u, err := c.c.Auth.Create(ctx, audit.System, auth.CreateInput{Email: "oscar@example.com", DisplayName: "Oscar",
		Grants: []auth.GrantInput{{Role: authz.RoleOperator, Domain: str(space, "id")}}})
	if err != nil {
		t.Fatal(err)
	}
	u, _ = c.c.Auth.Get(ctx, u.ID)
	_, secret, err := c.c.Auth.CreateAPIToken(ctx, &auth.Principal{User: u}, "cli", []auth.GrantInput{{Role: authz.RoleOperator, Domain: str(space, "id")}}, 24*time.Hour, false)
	if err != nil {
		t.Fatal(err)
	}
	op := env{server: e.server, token: secret}
	if sp := op.obj(t, "harbor-pilot", "set-space", "--space", "payments", "--ttl", "3600"); str(sp, "project_id") != "prj_web" || sp["ttl_seconds"].(float64) != 3600 {
		t.Fatalf("operator set-space %v", sp)
	}
	if r := op.run(t, "harbor-pilot", "set-space", "--space", "payments", "--project", "prj_data"); r.code == 0 || !strings.Contains(r.err+r.out, "needs account:write") {
		t.Fatalf("operator project change: %+v", r)
	}
	if r := e.run(t, "harbor-pilot", "disconnect", "--yes"); r.code == 0 || !strings.Contains(r.err+r.out, "route runs through Harbor Pilot") {
		t.Fatalf("disconnect while enabled: %+v", r)
	}
	e.ok(t, "harbor-pilot", "unset-space", "--space", "payments")
	e.ok(t, "harbor-pilot", "disconnect", "--yes")
	if r := e.run(t, "harbor-pilot", "get"); r.code == 0 {
		t.Fatalf("connection after disconnect: %+v", r)
	}
}
