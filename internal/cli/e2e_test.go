package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/events/pulsetest"
)

// SM-API-003: the remote CLI against a real in-process central, through the full HTTP stack: creates with
// idempotency keys, updates with If-Match read just before, stale versions as conflicts (exit 3), missing objects as
// not found (exit 4), refused principals as auth errors (exit 5), secrets from files and standard input only, and
// one-time secrets written to 0600 files instead of the terminal.
func TestCLIAgainstCentral(t *testing.T) {
	c := startCentral(t)
	e := c.env()

	// Public system info works without a token; everything else needs one.
	if out := (env{server: c.srv.URL}).ok(t, "system", "info"); !strings.Contains(out, "bootstrapped:") {
		t.Fatalf("system info: %s", out)
	}
	if r := (env{server: c.srv.URL}).code(t, ExitAuth, "space", "list"); !strings.Contains(r.err, "no API token") {
		t.Fatalf("no token: %s", r.err)
	}
	if r := (env{server: c.srv.URL, token: "spaceman_rt_wrong"}).code(t, ExitAuth, "space", "list"); !strings.Contains(r.err, "401") {
		t.Fatalf("bad token: %s", r.err)
	}
	if me := e.obj(t, "whoami"); me["root"] != true {
		t.Fatalf("whoami %v", me)
	}

	// Nodes: the enrollment token goes to a 0600 file, not the output.
	secretPath := filepath.Join(t.TempDir(), "n1.enroll")
	out := e.ok(t, "node", "create", "n1", "--label", "zone=eu", "--secret-file", secretPath)
	if strings.Contains(out, "spaceman_en") || !strings.Contains(out, "written to") {
		t.Fatalf("enrollment secret printed: %s", out)
	}
	st, err := os.Stat(secretPath)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("secret file %v %v", st, err)
	}
	e.code(t, ExitUsage, "node", "enrollment", "n1", "--secret-file", secretPath) // never overwrites
	node := e.obj(t, "node", "get", "n1")
	if str(node, "name") != "n1" || node["labels"].(map[string]any)["zone"] != "eu" {
		t.Fatalf("node %v", node)
	}
	if out := e.ok(t, "node", "list"); !strings.Contains(out, "n1") || !strings.Contains(out, "CONNECTED") {
		t.Fatalf("node list %s", out)
	}
	e.ok(t, "node", "create", "n2")
	e.ok(t, "node", "disable", "n2")
	e.ok(t, "node", "enable", "n2")
	e.code(t, ExitUsage, "node", "revoke", "n2")
	if n := e.obj(t, "node", "revoke", "n2", "--yes"); str(n, "state") != "revoked" {
		t.Fatalf("revoke %v", n)
	}

	// Spaces: create with an idempotency key (a replay returns the same space), update with If-Match.
	sp := e.obj(t, "space", "create", "payments", "--name", "Payments", "--node", "n1", "--idempotency-key", "cli-test-space-1")
	again := e.obj(t, "space", "create", "payments", "--name", "Payments", "--node", "n1", "--idempotency-key", "cli-test-space-1")
	if str(sp, "id") == "" || str(again, "id") != str(sp, "id") {
		t.Fatalf("idempotent create %v %v", sp, again)
	}
	e.code(t, ExitConflict, "space", "create", "payments", "--node", "n1")
	upd := e.obj(t, "space", "update", "payments", "--goals", "Ship payouts")
	if str(upd, "goals") != "Ship payouts" || str(upd, "name") != "Payments" {
		t.Fatalf("update kept other fields? %v", upd)
	}
	r := e.code(t, ExitConflict, "space", "update", "payments", "--goals", "x", "--if-match", "1")
	if !strings.Contains(r.err, "409") {
		t.Fatalf("stale If-Match: %s", r.err)
	}
	e.code(t, ExitNotFound, "space", "get", "no-such-space")
	e.code(t, ExitNotFound, "space", "get", "spc_00000000000000000000000000")
	if s := e.obj(t, "space", "pause", "payments", "--mode", "hold"); str(s, "state") != "paused" {
		t.Fatalf("pause %v", s)
	}
	e.ok(t, "space", "resume", "payments")
	if out := e.ok(t, "-o", "yaml", "space", "get", "payments"); !strings.Contains(out, "slug: payments") || !strings.Contains(out, "state: active") {
		t.Fatalf("yaml %s", out)
	}

	// Relocation: no relocation yet (404), and a precheck needs a connected source.
	e.code(t, ExitNotFound, "space", "relocate", "status", "payments")
	e.code(t, ExitUsage, "space", "relocate", "start", "payments", "--to", "n2", "--source-unavailable")
	if out := e.ok(t, "space", "relocate", "history", "payments"); !strings.Contains(out, "No relocations") && !strings.Contains(out, "MODE") {
		t.Fatalf("history %s", out)
	}

	// Inbox with --all pagination, triage with If-Match.
	for _, title := range []string{"one", "two", "three"} {
		e.ok(t, "inbox", "create", "--space", "payments", "--title", title, "--body", "body of "+title, "--urgency", "high")
	}
	all := e.obj(t, "inbox", "list", "--space", "payments", "--limit", "1", "--all")
	if got := items(t, all, "items"); len(got) != 3 {
		t.Fatalf("all pages: %d", len(got))
	}
	page := e.run(t, "inbox", "list", "--space", "payments", "--limit", "1")
	if !strings.Contains(page.err, "--cursor") {
		t.Fatalf("pagination hint: %s", page.err)
	}
	item := items(t, all, "items")[0]
	if it := e.obj(t, "inbox", "set-status", "--space", "payments", str(item, "id"), "done", "--reason", "handled"); str(it, "status") != "done" {
		t.Fatalf("triage %v", it)
	}
	bodyFile := writeFile(t, "body.md", "# From a file\n", 0o644)
	if it := e.obj(t, "inbox", "create", "--space", "payments", "--title", "file", "--body-file", bodyFile); str(it, "body") != "# From a file\n" {
		t.Fatalf("body file %v", it)
	}
	e.ok(t, "inbox", "get", "--space", "payments", str(item, "id"))

	// Queue.
	t1 := e.obj(t, "task", "create", "--space", "payments", "--title", "first")
	t2 := e.obj(t, "task", "create", "--space", "payments", "--title", "second", "--priority", "high")
	e.ok(t, "task", "move", "--space", "payments", str(t2, "id"), "--bottom")
	e.code(t, ExitUsage, "task", "move", "--space", "payments", str(t2, "id"))
	e.ok(t, "task", "move", "--space", "payments", str(t1, "id"), "--after", str(t2, "id"))
	if out := e.ok(t, "task", "list", "--space", "payments"); !strings.Contains(out, "first") || !strings.Contains(out, "second") {
		t.Fatalf("task list %s", out)
	}
	if tk := e.obj(t, "task", "update", "--space", "payments", str(t1, "id"), "--objective", "do it"); str(tk, "objective") != "do it" || str(tk, "title") != "first" {
		t.Fatalf("task update %v", tk)
	}
	if tk := e.obj(t, "task", "set-status", "--space", "payments", str(t1, "id"), "cancelled", "--reason", "dup"); str(tk, "status") != "cancelled" {
		t.Fatalf("task status %v", tk)
	}
	e.ok(t, "task", "get", "--space", "payments", str(t2, "id"))

	// Schedules.
	sc := e.obj(t, "schedule", "create", "--space", "payments", "--name", "nightly", "--kind", "cron", "--cron", "0 2 * * *",
		"--timezone", "UTC", "--target", "inbox", "--title", "Nightly review")
	if out := e.ok(t, "schedule", "list", "--space", "payments"); !strings.Contains(out, "nightly") {
		t.Fatalf("schedules %s", out)
	}
	if s := e.obj(t, "schedule", "update", "--space", "payments", str(sc, "id"), "--title", "Nightly check"); str(s, "title") != "Nightly check" || str(s, "cron") != "0 2 * * *" {
		t.Fatalf("schedule update %v", s)
	}
	e.ok(t, "schedule", "get", "--space", "payments", str(sc, "id"))
	e.ok(t, "schedule", "delete", "--space", "payments", str(sc, "id"))
	e.code(t, ExitNotFound, "schedule", "get", "--space", "payments", str(sc, "id"))

	// Limits: If-Match is optional for the first save and required afterwards (the CLI reads it).
	e.ok(t, "limits", "set", "--space", "payments", "--set", "max_concurrent_runs=2")
	lim := e.obj(t, "limits", "set", "--space", "payments", "--set", "ingest_per_minute=30")
	found := 0
	for _, l := range items(t, lim, "limits") {
		if str(l, "source") == "space" {
			found++
		}
	}
	if found != 2 {
		t.Fatalf("overrides kept: %v", lim)
	}
	e.ok(t, "limits", "set", "--space", "payments", "--unset", "ingest_per_minute")
	if out := e.ok(t, "limits", "get", "--space", "payments"); !strings.Contains(out, "max_concurrent_runs") {
		t.Fatalf("limits %s", out)
	}
	e.code(t, ExitUsage, "limits", "set", "--space", "payments")
	e.code(t, ExitUsage, "limits", "set", "--space", "payments", "--set", "x")
	e.ok(t, "limits", "definitions")

	// Accounts: the secret comes from standard input or a file and is never echoed.
	sec := env{server: e.server, token: e.token, stdin: "sk-ant-test-secret-value\n"}
	acct := sec.obj(t, "account", "create", "--name", "ci", "--kind", "anthropic_api_key", "--secret-stdin")
	if strings.Contains(sec.ok(t, "account", "get", str(acct, "id")), "sk-ant") {
		t.Fatal("secret echoed")
	}
	secFile := writeFile(t, "secret", "sk-ant-rotated\n", 0o600)
	if a := e.obj(t, "account", "update", str(acct, "id"), "--secret-file", secFile, "--description", "rotated"); a["secret_version"].(float64) != 2 || str(a, "name") != "ci" {
		t.Fatalf("rotate %v", a)
	}
	e.code(t, ExitUsage, "account", "create", "--name", "x", "--secret", "inline")
	e.ok(t, "account", "list")
	e.ok(t, "account", "kinds")
	e.ok(t, "account", "delete", str(acct, "id"))

	// Prompts: create, draft (If-Match from the draft's version), publish.
	pr := e.obj(t, "prompt", "create", "reviewer", "--name", "Reviewer", "--kind", "role", "--body", "Review carefully.")
	e.ok(t, "prompt", "draft", "reviewer", "--body", "Review very carefully.")
	pv := e.obj(t, "prompt", "publish", str(pr, "id"))
	if str(pv, "state") != "published" {
		t.Fatalf("publish %v", pv)
	}
	e.code(t, ExitUsage, "prompt", "publish", "reviewer")
	e.ok(t, "prompt", "list")
	e.ok(t, "prompt", "get", "reviewer")
	e.ok(t, "skill", "list")

	// Profiles, role templates and space roles (the GET operations added for the CLI).
	prof := e.obj(t, "profile", "create", "fast", "--name", "Fast", "--harness", "claude-code", "--model", "haiku")
	if p := e.obj(t, "profile", "get", "fast"); str(p, "id") != str(prof, "id") {
		t.Fatalf("profile get %v", p)
	}
	if p := e.obj(t, "profile", "update", "fast", "--model", "sonnet"); str(p, "model") != "sonnet" || str(p, "name") != "Fast" {
		t.Fatalf("profile update %v", p)
	}
	tpl := e.obj(t, "role", "template", "create", "helper", "--name", "Helper", "--kind", "worker", "--profile", str(prof, "id"))
	if u := e.obj(t, "role", "template", "update", "helper", "--description", "helps"); str(u, "description") != "helps" || str(u, "kind") != "worker" {
		t.Fatalf("template update %v", u)
	}
	if tpls := e.obj(t, "role", "template", "list"); len(items(t, tpls, "items")) == 0 {
		t.Fatal("no templates")
	}
	e.ok(t, "role", "template", "get", str(tpl, "slug"))
	e.code(t, ExitNotFound, "role", "template", "get", "no-such-template")
	e.ok(t, "role", "set", "--space", "payments", "helper", "--template", str(tpl, "id"), "--instructions", "Help.")
	if rl := e.obj(t, "role", "get", "--space", "payments", "helper"); str(rl, "instructions") != "Help." {
		t.Fatalf("role get %v", rl)
	}
	e.ok(t, "role", "list", "--space", "payments")
	e.ok(t, "role", "preview", "--space", "payments", "helper")
	e.ok(t, "role", "delete", "--space", "payments", "helper")
	e.code(t, ExitNotFound, "role", "get", "--space", "payments", "helper")

	// Events: a Pulse connection with credentials from a file; templates; subscriptions.
	nats := pulsetest.Start(t)
	credsFile := writeFile(t, "spaceman.creds", string(nats.Creds("spaceman")), 0o600)
	conn := e.obj(t, "event", "connection", "create", "--name", "pulse", "--url", nats.URL, "--creds-file", credsFile)
	if g := e.obj(t, "event", "connection", "get", str(conn, "id")); str(g, "name") != "pulse" || strings.Contains(e.ok(t, "-o", "json", "event", "connection", "get", str(conn, "id")), "SEED") {
		t.Fatalf("connection get %v", g)
	}
	e.ok(t, "event", "connection", "update", str(conn, "id"), "--stream", "PULSE")
	e.ok(t, "event", "connection", "list")
	etpls := e.obj(t, "event", "template", "list")
	if len(items(t, etpls, "items")) == 0 {
		t.Fatal("no built-in event templates")
	}
	e.ok(t, "event", "template", "get", str(items(t, etpls, "items")[0], "id"))
	e.ok(t, "event", "subscription", "list", "--space", "payments")

	// MCP gateway and approval policies.
	srv := e.obj(t, "mcp", "server", "create", "docs", "--name", "Docs", "--url", "https://mcp.example.com/mcp")
	hdr := writeFile(t, "headers.json", `{"Authorization":"Bearer upstream-secret"}`, 0o600)
	if s := e.obj(t, "mcp", "server", "update", str(srv, "id"), "--headers-file", hdr); s["headers_version"].(float64) < 1 || strings.Contains(e.ok(t, "-o", "json", "mcp", "server", "get", str(srv, "id")), "upstream-secret") {
		t.Fatalf("headers %v", s)
	}
	g := e.obj(t, "mcp", "grant", "add", str(srv, "id"), "--space", "payments", "--role", "worker")
	e.ok(t, "mcp", "grant", "list", str(srv, "id"))
	e.ok(t, "mcp", "grant", "revoke", str(srv, "id"), str(g, "id"))
	e.ok(t, "mcp", "server", "list")
	pol := e.obj(t, "approval-policy", "create", "mcp.docs.*", "--space", "payments", "--note", "ask first")
	if p := e.obj(t, "approval-policy", "get", str(pol, "id")); str(p, "operation") != "mcp.docs.*" {
		t.Fatalf("policy %v", p)
	}
	e.ok(t, "approval-policy", "list")
	e.ok(t, "approval-policy", "delete", str(pol, "id"))
	e.code(t, ExitNotFound, "approval-policy", "get", str(pol, "id"))
	e.ok(t, "mcp", "server", "delete", str(srv, "id"))

	// Cross-space: peers (a first save without If-Match, then with), settings and requests.
	e.ok(t, "space", "create", "billing", "--node", "n1")
	peer := e.obj(t, "xspace", "peer", "set", "--space", "payments", "billing", "--accept-questions", "--disclosure", "summary")
	if in := peer["inbound"].(map[string]any); in["questions"] != true {
		t.Fatalf("peer %v", peer)
	}
	peer = e.obj(t, "xspace", "peer", "set", "--space", "payments", "billing", "--send-questions")
	if in, outb := peer["inbound"].(map[string]any), peer["outbound"].(map[string]any); in["questions"] != true || outb["questions"] != true {
		t.Fatalf("peer merge %v", peer)
	}
	e.ok(t, "xspace", "peer", "get", "--space", "payments", "billing")
	if out := e.ok(t, "xspace", "peer", "list"); !strings.Contains(out, "billing") {
		t.Fatalf("all peers %s", out)
	}
	e.ok(t, "xspace", "settings", "set", "--space", "payments", "--max-depth", "2")
	e.ok(t, "xspace", "settings", "get", "--space", "payments")
	e.ok(t, "xspace", "request", "list")
	e.ok(t, "xspace", "request", "list", "--space", "payments", "--direction", "incoming")
	e.ok(t, "xspace", "peer", "delete", "--space", "payments", "billing")

	// Users and tokens: the root token is not a user, so it cannot create API tokens (exit 5).
	e.ok(t, "user", "create", "--email", "grace@example.com", "--display-name", "Grace", "--grant", "admin")
	usr := env{server: e.server, token: e.token, stdin: "correct horse battery staple\n"}.
		obj(t, "user", "create", "--email", "ada@example.com", "--display-name", "Ada", "--grant", "operator:payments", "--password-stdin")
	if g := usr["grants"].([]any)[0].(map[string]any); g["domain"] != str(sp, "id") {
		t.Fatalf("grant resolved %v", usr)
	}
	e.ok(t, "user", "disable", str(usr, "id"))
	e.ok(t, "user", "enable", str(usr, "id"))
	if u := e.obj(t, "user", "grants", str(usr, "id"), "--grant", "viewer"); u["grants"].([]any)[0].(map[string]any)["domain"] != "*" {
		t.Fatalf("grants %v", u)
	}
	e.code(t, ExitUsage, "user", "grants", str(usr, "id"), "--grant", "owner")
	e.ok(t, "user", "list")
	e.code(t, ExitAuth, "token", "create", "ci", "--grant", "viewer", "--never-expires")
	e.ok(t, "token", "list", "--all-users")

	// Human actions, runs, backups and the audit log.
	e.ok(t, "human", "list")
	e.ok(t, "human", "list", "--space", "payments", "--pending")
	e.ok(t, "run", "list", "--space", "payments")
	e.ok(t, "system", "backup", "list")
	if out := e.ok(t, "audit", "list", "--action", "space.created", "--all"); !strings.Contains(out, "space.created") {
		t.Fatalf("audit %s", out)
	}
	if v := e.obj(t, "audit", "verify"); v["ok"] != true {
		t.Fatalf("audit verify %v", v)
	}

	newGroupsAgainstCentral(t, c, e, str(usr, "id"))

	// Archive and purge (needs --yes).
	e.ok(t, "space", "archive", "billing")
	e.code(t, ExitUsage, "space", "purge", "billing")
	e.ok(t, "space", "purge", "billing", "--yes")
	e.code(t, ExitNotFound, "space", "get", "billing")
	e.ok(t, "space", "restore", "payments")
}
