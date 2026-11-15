package cli

import (
	"context"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
)

// SpaceMan MCP servers through the CLI (ADR-0063): created with an internal token owned by the caller, edited with the
// token unchanged, re-issued with a new token, granted and deleted. The root token is not a user and cannot own one.
func TestSpacemanMCPCommands(t *testing.T) {
	c := startCentral(t)
	e := c.env()
	e.ok(t, "node", "create", "n1")
	e.ok(t, "space", "create", "payments", "--name", "Payments", "--node", "n1")
	e.code(t, ExitAuth, "mcp", "spaceman", "create", "infra", "--name", "SpaceMan")

	ctx := context.Background()
	admin := []auth.GrantInput{{Role: authz.RoleAdmin, Domain: authz.Global}}
	u, err := c.c.Auth.Create(ctx, audit.System, auth.CreateInput{Email: "ada@example.com", DisplayName: "Ada", Grants: admin})
	if err != nil {
		t.Fatal(err)
	}
	_, secret, err := c.c.Auth.CreateAPIToken(ctx, &auth.Principal{User: u}, "cli", admin, 30*24*time.Hour, false)
	if err != nil {
		t.Fatal(err)
	}
	ada := env{server: e.server, token: secret}

	sm := ada.obj(t, "mcp", "spaceman", "create", "infra", "--name", "SpaceMan", "--tool", "get_recording")
	smi, _ := sm["spaceman"].(map[string]any)
	if str(sm, "kind") != "spaceman" || sm["enabled"] != true || smi == nil || str(smi, "preset") != "admin" || str(smi, "approval") != "none" ||
		str(smi, "owner_user_id") != u.ID || str(smi, "token_state") != "active" {
		t.Fatalf("create %v", sm)
	}
	// Created by a 30-day token, the internal token expires with it.
	if smi["never_expires"] != false || smi["expires_at"] == nil {
		t.Fatalf("child token expiry %v", smi)
	}
	up := ada.obj(t, "mcp", "spaceman", "update", str(sm, "id"), "--approval", "writes")
	if upi, _ := up["spaceman"].(map[string]any); str(upi, "approval") != "writes" || str(upi, "token_id") != str(smi, "token_id") ||
		str(up, "name") != "SpaceMan" || len(upi["tools"].([]any)) != 1 || str(upi, "preset") != "admin" {
		t.Fatalf("update %v", up)
	}
	re := ada.obj(t, "mcp", "spaceman", "reissue", str(sm, "id"), "--grant", "viewer:payments", "--expires-in-days", "7")
	rei, _ := re["spaceman"].(map[string]any)
	if str(rei, "token_id") == str(smi, "token_id") || len(rei["grants"].([]any)) != 1 {
		t.Fatalf("reissue %v", re)
	}
	ada.code(t, ExitAPI, "mcp", "spaceman", "create", "bad", "--name", "Bad", "--approval", "always")
	ada.ok(t, "mcp", "grant", "add", str(sm, "id"), "--space", "payments", "--role", "control")
	// The generic edit refuses a SpaceMan MCP server.
	ada.code(t, ExitAPI, "mcp", "server", "update", str(sm, "id"), "--name", "Renamed")
	ada.ok(t, "mcp", "server", "check", str(sm, "id"))
	ada.ok(t, "mcp", "server", "delete", str(sm, "id"))
}
