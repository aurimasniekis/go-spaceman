package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
)

// newGroupsAgainstCentral covers the command groups added to close the API gaps (ADR-0040 amendment) against the real
// central of TestCLIAgainstCentral: updates through lists, raw downloads, the change feed, one-time secrets and
// user-only operations with a user's own API token. The space "payments", the prompt "reviewer" and the user adaID exist.
func newGroupsAgainstCentral(t *testing.T, c *central, e env, adaID string) {
	t.Helper()
	const s = "payments"

	// Tasks: blockers, dependencies, escalation, decisions and the queue policy (read just before).
	t3 := e.obj(t, "task", "create", "--space", s, "--title", "third")
	t4 := e.obj(t, "task", "create", "--space", s, "--title", "fourth")
	blocked := e.obj(t, "task", "block", "--space", s, str(t3, "id"), "--kind", "external", "--note", "vendor")
	bl := blocked["blockers"].([]any)[0].(map[string]any)
	e.ok(t, "task", "unblock", "--space", s, str(t3, "id"), str(bl, "id"))
	if tk := e.obj(t, "task", "depends", "--space", s, str(t4, "id"), "--on", str(t3, "id")); len(tk["depends_on"].([]any)) != 1 {
		t.Fatalf("depends %v", tk)
	}
	e.ok(t, "task", "depends", "--space", s, str(t4, "id"), "--none")
	if tk := e.obj(t, "task", "escalate", "--space", s, str(t3, "id"), "--reason", "customer impact"); str(tk, "priority") != "urgent" {
		t.Fatalf("escalate %v", tk)
	}
	if p := e.obj(t, "task", "set-policy", "--space", s, "--max-active", "2"); p["max_active"].(float64) != 2 {
		t.Fatalf("queue policy %v", p)
	}
	e.ok(t, "task", "set-policy", "--space", s, "--incident-insertion", "priority")
	e.ok(t, "task", "decisions", "--space", s)

	// Human actions: create, comment, assign, move; types and policies.
	ha := e.obj(t, "human", "create", "--space", s, "--type", "question", "--title", "Which region?", "--body", "eu or us")
	e.ok(t, "human", "comment", "--space", s, str(ha, "id"), "--body", "asking ops")
	if a := e.obj(t, "human", "assign", "--space", s, str(ha, "id"), "--assignee", adaID); str(a, "assignee") != adaID {
		t.Fatalf("assign %v", a)
	}
	e.ok(t, "human", "assign", "--space", s, str(ha, "id"), "--unassign")
	e.ok(t, "human", "move", "--space", s, str(ha, "id"))
	ht := e.obj(t, "human", "type", "create", "deploy-check", "--name", "Deploy check", "--outcome", "go,hold")
	e.ok(t, "human", "type", "draft", "deploy-check", "--notes", "first")
	if pub := e.obj(t, "human", "type", "publish", str(ht, "slug")); pub["latest"].(float64) != 1 {
		t.Fatalf("type publish %v", pub)
	}
	e.ok(t, "human", "type", "draft", "deploy-check", "--notes", "second") // a new draft, from the published version
	e.ok(t, "human", "type", "list")
	e.ok(t, "human", "type", "options", "--space", s)
	if en := e.obj(t, "human", "type", "enable", "--space", s, str(ht, "id")); len(items(t, en, "items")) != 1 {
		t.Fatalf("enable %v", en)
	}
	e.ok(t, "human", "type", "enabled", "--space", s)
	e.ok(t, "human", "type", "disable", "--space", s, str(ht, "id"))
	if p := e.obj(t, "human", "policy", "set", "approval", "--remind-after-minutes", "30"); p["remind_after_minutes"].(float64) != 30 {
		t.Fatalf("default policy %v", p)
	}
	if p := e.obj(t, "human", "policy", "set", "approval", "--max-reminders", "4"); p["remind_after_minutes"].(float64) != 30 || p["max_reminders"].(float64) != 4 {
		t.Fatalf("policy update kept fields? %v", p)
	}
	e.ok(t, "human", "policy", "set", "--space", s, "all", "--requester-ended", "reroute")
	if out := e.ok(t, "human", "policy", "list", "--space", s); !strings.Contains(out, "space") {
		t.Fatalf("space policies %s", out)
	}
	e.ok(t, "human", "policy", "delete", "--space", s, "all")
	e.ok(t, "human", "policy", "delete", "approval")
	e.ok(t, "approval", "list", "--space", s)

	// Messages, knowledge bases, memory and dreams.
	msg := e.obj(t, "message", "send", "--space", s, "--to", "control", "--subject", "Freeze deploys", "--body", "until Monday")
	e.ok(t, "message", "get", "--space", s, str(msg, "id"))
	if out := e.ok(t, "message", "list", "--space", s); !strings.Contains(out, "Freeze deploys") {
		t.Fatalf("messages %s", out)
	}
	e.ok(t, "message", "resolve", "--space", s, str(msg, "id"), "--resolution", "done")
	e.ok(t, "kb", "list", "--space", s)
	e.ok(t, "memory", "list", "--space", s)
	dtpl := e.obj(t, "role", "template", "create", "dreaming", "--name", "Dreaming", "--kind", "dreamer")
	e.ok(t, "role", "set", "--space", s, "dreamer", "--template", str(dtpl, "id"))
	dr := e.obj(t, "dream", "create", "--space", s, "nightly", "--name", "Nightly", "--role", "dreamer", "--interval-hours", "24")
	if d := e.obj(t, "dream", "update", "--space", s, "nightly", "--interval-hours", "12"); d["interval_hours"].(float64) != 12 || str(d, "name") != "Nightly" {
		t.Fatalf("dream update %v", d)
	}
	e.code(t, ExitConflict, "dream", "update", "--space", s, str(dr, "id"), "--name", "x", "--if-match", "1")
	e.ok(t, "dream", "runs", "--space", s, str(dr, "id"))

	// Attachments: upload, metadata, and the raw content route back to a file.
	content := "\x89PNG\r\n\x1a\n" + strings.Repeat("x", 64)
	src := writeFile(t, "pixel.png", content, 0o644)
	att := e.obj(t, "attachment", "upload", "--space", s, src)
	if m := e.obj(t, "attachment", "get", "--space", s, str(att, "id")); str(m, "name") != "pixel.png" {
		t.Fatalf("attachment %v", m)
	}
	dst := filepath.Join(t.TempDir(), "back.png")
	e.ok(t, "attachment", "download", "--space", s, str(att, "id"), "--out", dst)
	if b, _ := os.ReadFile(dst); !bytes.Equal(b, []byte(content)) {
		t.Fatalf("downloaded %q", b)
	}

	// Accounts and MCP servers of a space, role options, usage and prompts.
	acct := e.obj(t, "account", "create", "--name", "bound", "--kind", "anthropic_api_key", "--secret-file", writeFile(t, "k", "sk-ant-bound\n", 0o600))
	e.ok(t, "account", "set-space", "--space", s, "--account", str(acct, "id"), "--default", str(acct, "id"))
	if out := e.ok(t, "account", "space", "--space", s); !strings.Contains(out, "bound") {
		t.Fatalf("space accounts %s", out)
	}
	e.ok(t, "mcp", "space-servers", "--space", s)
	e.ok(t, "role", "options", "--space", s)
	if u := e.obj(t, "space", "usage", s); u["known_runs"] == nil {
		t.Fatalf("usage %v", u)
	}
	pr := e.obj(t, "prompt", "get", "reviewer")
	v1 := str(pr["versions"].([]any)[0].(map[string]any), "id")
	e.ok(t, "prompt", "draft", "reviewer", "--body", "Review with care.")
	e.code(t, ExitUsage, "prompt", "discard-draft", str(pr, "id"))
	e.ok(t, "prompt", "discard-draft", str(pr, "id"), "--yes")
	if f := e.obj(t, "prompt", "fork", v1, "reviewer-2", "--name", "Reviewer 2"); str(f, "slug") != "reviewer-2" {
		t.Fatalf("fork %v", f)
	}
	rb := e.obj(t, "prompt", "rollback", str(pr, "id"), "--version", v1)
	e.ok(t, "prompt", "diff", "--from", v1, "--to", str(rb, "id"))

	// SSO configuration and user identities (no browser involved).
	sec := writeFile(t, "client.secret", "oidc-client-secret\n", 0o600)
	prov := e.obj(t, "oidc", "provider", "create", "porch", "--display-name", "Porch", "--issuer", "https://porch.example.com",
		"--client-id", "spaceman", "--client-secret-file", sec, "--scope", "openid,email,groups", "--groups-claim", "groups")
	if strings.Contains(e.ok(t, "-o", "json", "oidc", "provider", "get", str(prov, "id")), "oidc-client-secret") {
		t.Fatal("client secret readable")
	}
	if p := e.obj(t, "oidc", "provider", "update", str(prov, "id"), "--display-name", "Porch SSO"); str(p, "display_name") != "Porch SSO" || str(p, "issuer") != "https://porch.example.com" {
		t.Fatalf("provider update %v", p)
	}
	mp := e.obj(t, "oidc", "mapping", "add", str(prov, "id"), "--group", "pay-ops", "--role", "operator", "--domain", s)
	e.ok(t, "oidc", "mapping", "list", str(prov, "id"))
	e.ok(t, "oidc", "mapping", "delete", str(prov, "id"), str(mp, "id"))
	e.ok(t, "oidc", "provider", "list")
	(env{server: c.srv.URL}).ok(t, "oidc", "sign-in-providers")
	e.ok(t, "user", "identity", "link", adaID, "--provider", str(prov, "id"), "--subject", "u_ada")
	if out := e.ok(t, "user", "identity", "list", adaID); !strings.Contains(out, "u_ada") {
		t.Fatalf("identities %s", out)
	}
	e.ok(t, "user", "identity", "unlink", adaID, "--issuer", "https://porch.example.com", "--subject", "u_ada", "--yes")
	e.ok(t, "oidc", "provider", "delete", str(prov, "id"), "--yes")

	// One-time secrets go to 0600 files on request.
	resetFile := filepath.Join(t.TempDir(), "ada.reset")
	e.code(t, ExitUsage, "user", "reset-token", adaID)
	if out := e.ok(t, "user", "reset-token", adaID, "--secret-file", resetFile); strings.Contains(out, "spaceman_") {
		t.Fatalf("reset token printed %s", out)
	}

	// User-only operations with Ada's own API token; the root token is not a user (exit 5).
	e.code(t, ExitAuth, "session", "list")
	ctx := context.Background()
	lr, err := c.c.Auth.Login(ctx, auth.LoginInput{Email: "ada@example.com", Password: "correct horse battery staple", Meta: auth.SessionMeta{IP: "127.0.0.1", UserAgent: "test"}})
	if err != nil {
		t.Fatal(err)
	}
	_, secret, err := c.c.Auth.CreateAPIToken(ctx, lr.Principal, "cli", []auth.GrantInput{{Role: authz.Role("viewer"), Domain: "*"}}, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	ada := env{server: c.srv.URL, token: secret}
	sessions := ada.obj(t, "session", "list")
	if len(items(t, sessions, "items")) == 0 {
		t.Fatalf("sessions %v", sessions)
	}
	ada.ok(t, "session", "revoke", str(items(t, sessions, "items")[0], "id"))
	ada.ok(t, "identity", "list")
	uri := filepath.Join(t.TempDir(), "totp.uri")
	ada.ok(t, "mfa", "enroll", "--secret-file", uri)
	if b, _ := os.ReadFile(uri); !strings.HasPrefix(string(b), "otpauth://") {
		t.Fatalf("otpauth %q", b)
	}
	ada.code(t, ExitAPI, "mfa", "confirm", "--code-file", writeFile(t, "code", "000000\n", 0o600), "--show-secret")
	cur := writeFile(t, "cur", "correct horse battery staple\n", 0o600)
	env{server: c.srv.URL, token: secret, stdin: "an even better passphrase\n"}.ok(t, "password", "change", "--current-password-file", cur, "--new-password-stdin")

	// The instance's status (SM-OBS-006), system notices, the outbox and the root token's metadata; the change feed.
	if st := e.obj(t, "system", "status"); !strings.HasPrefix(str(st, "boot_id"), "boot_") || st["schema"] == nil || st["counts"] == nil {
		t.Fatalf("system status %v", st)
	}
	if out := e.ok(t, "system", "status"); !strings.Contains(out, "boot_id") {
		t.Fatalf("system status table %s", out)
	}
	e.ok(t, "system", "outbox")
	e.ok(t, "system", "restore", "get")
	e.ok(t, "system", "break-glass", "get")
	if rt := e.obj(t, "system", "root-token", "get"); str(rt, "hint") == "" {
		t.Fatalf("root token %v", rt)
	}
	e.code(t, ExitUsage, "system", "root-token", "revoke")
	if out := e.ok(t, "-o", "json", "watch", "--count", "2"); strings.Count(out, "\n") != 2 || !strings.Contains(out, `"resource_type"`) {
		t.Fatalf("watch %s", out)
	}
}
