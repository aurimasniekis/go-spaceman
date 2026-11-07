package cli

import (
	"strings"
	"testing"

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
	e.obj(t, "space", "create", "payments", "--name", "Payments", "--node", "n1")

	e.code(t, ExitUsage, "harbor-pilot", "connect", "--base-url", f.URL(), "--parent-token", f.Parent)
	parent := writeFile(t, "parent", f.Parent+"\n", 0o600)
	service := writeFile(t, "service", f.Service+"\n", 0o600)
	out := e.ok(t, "harbor-pilot", "connect", "--base-url", f.URL(), "--allow-insecure-http", "--parent-token-file", parent, "--service-token-file", service)
	if strings.Contains(out, f.Parent) || strings.Contains(out, f.Service) || !strings.Contains(out, "bnd_fake") {
		t.Fatalf("connect output %s", out)
	}
	conn := e.obj(t, "hp", "connect", "--run-base-url", "https://hp.example.com")
	if str(conn, "run_base_url") != "https://hp.example.com" || conn["parent_token_version"].(float64) != 1 || conn["has_service_token"] != true || conn["version"].(float64) != 2 {
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
	if r := e.run(t, "harbor-pilot", "disconnect", "--yes"); r.code == 0 || !strings.Contains(r.err+r.out, "route runs through Harbor Pilot") {
		t.Fatalf("disconnect while enabled: %+v", r)
	}
	e.ok(t, "harbor-pilot", "unset-space", "--space", "payments")
	e.ok(t, "harbor-pilot", "disconnect", "--yes")
	if r := e.run(t, "harbor-pilot", "get"); r.code == 0 {
		t.Fatalf("connection after disconnect: %+v", r)
	}
}
