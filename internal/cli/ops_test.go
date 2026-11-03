package cli

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sp = "/spaces/" + spaceID

// Updates of resources without a get operation read the object from their list, merge the flags given and send its
// version as If-Match; a slug is replaced by the ID.
func TestUpdateFromList(t *testing.T) {
	f := newFake(t)
	f.json("GET "+sp+"/dreams", 200, map[string]any{"items": []any{
		map[string]any{"id": "drm_1", "slug": "nightly", "name": "Nightly", "space_role": "dreamer", "interval_hours": 24, "version": 4, "next_due_at": "x"}}})
	f.on("PUT "+sp+"/dreams/drm_1", func(w http.ResponseWriter, _ *http.Request, body map[string]any) { reply(w, 200, body) })
	e := f.env()
	got := e.obj(t, "dream", "update", "--space", spaceID, "nightly", "--interval-hours", "12")
	r := f.last("PUT", sp+"/dreams/drm_1")
	if r.header.Get("If-Match") != `"4"` || got["name"] != "Nightly" || got["interval_hours"].(float64) != 12 || got["next_due_at"] != nil {
		t.Fatalf("update %v %v", r.header, got)
	}
	r2 := e.code(t, ExitNotFound, "dream", "update", "--space", spaceID, "weekly", "--name", "x")
	if !strings.Contains(r2.err, `no dream "weekly"`) {
		t.Fatalf("missing: %s", r2.err)
	}

	// Human policies: a stored policy gives the base and If-Match; without one the effective policy is the base and no
	// If-Match is sent. Without --space the installation default changes.
	pol := func(typ, src string, v int) map[string]any {
		return map[string]any{"action_type": typ, "source": src, "version": v, "remind_after_minutes": 10, "remind_every_minutes": 0, "max_reminders": 3,
			"escalate_after_minutes": 0, "escalate_priority": "", "escalate_assignee": "", "requester_ended": "reroute", "hold_minutes": 0,
			"successor_role": "", "wake_idle": false, "rate_per_run_hour": 5, "rate_per_space_hour": 50, "id": "hpo_x"}
	}
	f.json("GET /human-policies", 200, map[string]any{"items": []any{pol("approval", "default", 2)}, "effective": []any{pol("approval", "default", 2), pol("todo", "builtin", 0)}})
	f.json("GET "+sp+"/human-policies", 200, map[string]any{"items": []any{}, "effective": []any{pol("todo", "builtin", 0)}})
	echo := func(w http.ResponseWriter, _ *http.Request, body map[string]any) { reply(w, 200, body) }
	f.on("PUT /human-policies/approval", echo)
	f.on("PUT "+sp+"/human-policies/todo", echo)
	if p := e.obj(t, "human", "policy", "set", "approval", "--escalate-after-minutes", "60"); p["escalate_after_minutes"].(float64) != 60 || p["max_reminders"].(float64) != 3 {
		t.Fatalf("policy %v", p)
	}
	if r := f.last("PUT", "/human-policies/approval"); r.header.Get("If-Match") != `"2"` || r.body["id"] != nil || r.body["source"] != nil {
		t.Fatalf("default policy %v %v", r.header, r.body)
	}
	e.ok(t, "human", "policy", "set", "--space", spaceID, "todo", "--wake-idle")
	if r := f.last("PUT", sp+"/human-policies/todo"); r.header.Get("If-Match") != "" || r.body["wake_idle"] != true || r.body["rate_per_run_hour"].(float64) != 5 {
		t.Fatalf("space policy %v %v", r.header, r.body)
	}
	f.on("DELETE /human-policies/approval", func(w http.ResponseWriter, _ *http.Request, _ map[string]any) { w.WriteHeader(http.StatusNoContent) })
	if r := e.run(t, "human", "policy", "delete", "approval"); r.code != 0 || !strings.Contains(r.err, "Policy removed") {
		t.Fatalf("delete %v", r)
	}
	if out := e.ok(t, "human", "policy", "list"); !strings.Contains(out, "builtin") {
		t.Fatalf("list %s", out)
	}

	// Knowledge bases wrap the object in a detail: its version and fields come from "kb".
	f.json("GET "+sp+"/kbs/kb_1", 200, map[string]any{"kb": map[string]any{"id": "kb_1", "slug": "rb", "name": "Runbooks", "repo": "https://g/r.git",
		"space_role": "kb", "version": 7, "stats": map[string]any{"open": 1}}, "requests": []any{}})
	f.on("PUT "+sp+"/kbs/kb_1", echo)
	e.ok(t, "kb", "update", "--space", spaceID, "kb_1", "--max-sessions", "3")
	if r := f.last("PUT", sp+"/kbs/kb_1"); r.header.Get("If-Match") != `"7"` || r.body["repo"] != "https://g/r.git" || r.body["stats"] != nil || r.body["max_sessions"].(float64) != 3 {
		t.Fatalf("kb update %v %v", r.header, r.body)
	}
}

// One-time secrets of the new commands are shown only on request: --show-secret prints them once, --secret-file writes
// them (and every field that reveals them) to a new 0600 file; without either the call is never made.
func TestSecretsOnRequest(t *testing.T) {
	f := newFake(t)
	f.json("POST /users/usr_1/reset-token", 200, map[string]any{"token": "spaceman_rs_secret1", "expires_at": "2026-11-04T00:00:00Z"})
	f.json("POST /users/usr_1/link-invite", 200, map[string]any{"token": "lnk_secret2", "url": "https://c/link?t=lnk_secret2", "expires_at": "x"})
	f.json("POST /auth/mfa/enroll", 200, map[string]any{"secret": "JBSWY3DPEHPK3PXP", "otpauth_uri": "otpauth://totp/x?secret=JBSWY3DPEHPK3PXP"})
	f.json("POST /auth/mfa/enroll/confirm", 200, map[string]any{"recovery_codes": []any{"aaaa-1111", "bbbb-2222"}, "me": map[string]any{"root": false}})
	f.json("POST /admin/root-token/rotate", 200, map[string]any{"secret": "spaceman_rt_new", "root_token": map[string]any{"id": "rtk_2"}})
	e := f.env()

	r := e.code(t, ExitUsage, "user", "reset-token", "usr_1")
	if !strings.Contains(r.err, "--show-secret") || f.count("POST", "/users/usr_1/reset-token") != 0 {
		t.Fatalf("refusal %s", r.err)
	}
	r = e.run(t, "user", "reset-token", "usr_1", "--show-secret")
	if !strings.Contains(r.out, "spaceman_rs_secret1") || !strings.Contains(r.err, "shown only once") {
		t.Fatalf("shown %v", r)
	}
	dir := t.TempDir()
	inv := filepath.Join(dir, "invite")
	out := e.ok(t, "-o", "json", "user", "link-invite", "usr_1", "--provider", "oip_1", "--secret-file", inv)
	if strings.Contains(out, "lnk_secret2") {
		t.Fatalf("invite token printed %s", out)
	}
	if b, _ := os.ReadFile(inv); string(b) != "https://c/link?t=lnk_secret2\n" {
		t.Fatalf("invite file %q", b)
	}
	if b := f.last("POST", "/users/usr_1/link-invite").body; b["provider_id"] != "oip_1" {
		t.Fatalf("invite body %v", b)
	}
	uri := filepath.Join(dir, "totp")
	if out := e.ok(t, "mfa", "enroll", "--secret-file", uri); strings.Contains(out, "JBSWY3DPEHPK3PXP") {
		t.Fatalf("totp secret printed %s", out)
	}
	codes := filepath.Join(dir, "codes")
	code := writeFile(t, "code", "123456\n", 0o600)
	if out := e.ok(t, "mfa", "confirm", "--code-file", code, "--secret-file", codes); strings.Contains(out, "aaaa-1111") {
		t.Fatalf("codes printed %s", out)
	}
	if b, _ := os.ReadFile(codes); string(b) != "aaaa-1111\nbbbb-2222\n" {
		t.Fatalf("codes file %q", b)
	}
	if st, _ := os.Stat(codes); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode())
	}
	if b := f.last("POST", "/auth/mfa/enroll/confirm").body; b["code"] != "123456" {
		t.Fatalf("code %v", b)
	}
	e.code(t, ExitUsage, "system", "root-token", "rotate", "--show-secret")
	if out := e.ok(t, "system", "root-token", "rotate", "--yes", "--show-secret"); !strings.Contains(out, "spaceman_rt_new") {
		t.Fatalf("rotate %s", out)
	}
	e.code(t, ExitUsage, "mfa", "recovery-codes", "--show-secret") // needs --yes

	// Passwords only from files or standard input, and only one input from standard input.
	f.on("POST /auth/password", func(w http.ResponseWriter, _ *http.Request, _ map[string]any) { w.WriteHeader(http.StatusNoContent) })
	cur := writeFile(t, "cur", "old pass\n", 0o600)
	pw := env{server: e.server, token: e.token, stdin: "new pass\n"}
	if r := pw.run(t, "password", "change", "--current-password-file", cur, "--new-password-stdin"); r.code != 0 || !strings.Contains(r.err, "Password changed") {
		t.Fatalf("change %v", r)
	}
	if b := f.last("POST", "/auth/password").body; b["current"] != "old pass" || b["new"] != "new pass" {
		t.Fatalf("password body %v", b)
	}
	pw.code(t, ExitUsage, "password", "change", "--current-password-stdin", "--new-password-stdin")
}

// Downloads go to --out (never a terminal), refuse to overwrite without --force and never leave a partial file.
func TestDownloadsAndUploads(t *testing.T) {
	f := newFake(t)
	content := []byte("\x89PNG\r\n\x1a\nbinary\x00\x1b[2J")
	f.on("GET "+sp+"/attachments/att_1/content", func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(content)
	})
	f.on("GET "+sp+"/runs/run_1/transcript", func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
		problem(w, 404, "not_found", "no stored transcript")
	})
	e := f.env()
	out := filepath.Join(t.TempDir(), "a.png")
	if r := e.code(t, ExitUsage, "attachment", "download", "--space", spaceID, "att_1"); !strings.Contains(r.err, "--out is required") {
		t.Fatalf("no out %s", r.err)
	}
	if r := e.run(t, "attachment", "download", "--space", spaceID, "att_1", "--out", out); r.code != 0 || !strings.Contains(r.err, fmt.Sprintf("Wrote %d bytes", len(content))) {
		t.Fatalf("download %v", r)
	}
	if b, _ := os.ReadFile(out); !bytes.Equal(b, content) {
		t.Fatalf("content %q", b)
	}
	if st, _ := os.Stat(out); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode())
	}
	if r := e.code(t, ExitUsage, "attachment", "download", "--space", spaceID, "att_1", "--out", out); !strings.Contains(r.err, "--force") {
		t.Fatalf("overwrite %s", r.err)
	}
	if err := os.WriteFile(out, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.ok(t, "attachment", "download", "--space", spaceID, "att_1", "--out", out, "--force")
	if b, _ := os.ReadFile(out); !bytes.Equal(b, content) {
		t.Fatalf("forced %q", b)
	}
	// Standard output works when it is not a terminal (a pipe; here a buffer).
	if r := e.run(t, "attachment", "download", "--space", spaceID, "att_1", "--out", "-"); r.code != 0 || r.out != string(content) {
		t.Fatalf("stdout %q", r.out)
	}
	missing := filepath.Join(t.TempDir(), "t.jsonl")
	e.code(t, ExitNotFound, "run", "transcript", "download", "--space", spaceID, "run_1", "--out", missing)
	if ents, _ := os.ReadDir(filepath.Dir(missing)); len(ents) != 0 {
		t.Fatalf("partial files left: %v", ents)
	}
	e.code(t, ExitUsage, "run", "transcript", "delete", "--space", spaceID, "run_1")
	f.on("DELETE "+sp+"/runs/run_1/recording", func(w http.ResponseWriter, _ *http.Request, _ map[string]any) { w.WriteHeader(http.StatusNoContent) })
	if r := e.run(t, "run", "recording", "delete", "--space", spaceID, "run_1", "--yes"); r.code != 0 || !strings.Contains(r.err, "Recording deleted") {
		t.Fatalf("delete %v", r)
	}

	// Uploads are multipart forms with one file field and an Idempotency-Key.
	f.on("POST "+sp+"/attachments", func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
		reply(w, 201, map[string]any{"id": "att_2", "uri": "spaceman://attachment/att_2"})
	})
	src := writeFile(t, "shot.png", string(content), 0o644)
	if got := e.obj(t, "attachment", "upload", "--space", spaceID, src); got["uri"] != "spaceman://attachment/att_2" {
		t.Fatalf("upload %v", got)
	}
	r := f.last("POST", sp+"/attachments")
	mt, params, err := mime.ParseMediaType(r.header.Get("Content-Type"))
	if err != nil || mt != "multipart/form-data" || r.header.Get("Idempotency-Key") == "" {
		t.Fatalf("upload headers %v", r.header)
	}
	part, err := multipart.NewReader(bytes.NewReader(r.raw), params["boundary"]).NextPart()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(part)
	if part.FormName() != "file" || part.FileName() != "shot.png" || !bytes.Equal(b, content) {
		t.Fatalf("part %s %s %q", part.FormName(), part.FileName(), b)
	}
	e.code(t, ExitUsage, "attachment", "upload", "--space", spaceID, filepath.Join(t.TempDir(), "none"))

	// Memory files: printed without control characters, or written unchanged with --out.
	f.json("GET "+sp+"/memory/worker/file", 200, map[string]any{"path": "notes.md", "content": "line\x1b[31m red\x07\nnext"})
	if out := e.ok(t, "memory", "file", "--space", spaceID, "worker", "--path", "notes.md"); out != "line[31m red\nnext\n" {
		t.Fatalf("printable %q", out)
	}
	if q := f.last("GET", sp+"/memory/worker/file").query; q != "path=notes.md" {
		t.Fatalf("query %s", q)
	}
	mf := filepath.Join(t.TempDir(), "notes.md")
	e.ok(t, "memory", "file", "--space", spaceID, "worker", "--path", "notes.md", "--out", mf)
	if b, _ := os.ReadFile(mf); string(b) != "line\x1b[31m red\x07\nnext" {
		t.Fatalf("raw file %q", b)
	}
	e.code(t, ExitUsage, "memory", "file", "--space", spaceID, "worker", "--path", "notes.md", "--out", mf)
	if out := e.ok(t, "-o", "json", "memory", "file", "--space", spaceID, "worker", "--path", "notes.md"); !strings.Contains(out, `"content"`) {
		t.Fatalf("json %s", out)
	}
	f.json("GET "+sp+"/memory/worker/files", 200, map[string]any{"items": []any{"a.md", "b/c.md"}})
	if out := e.ok(t, "memory", "files", "--space", spaceID, "worker"); !strings.Contains(out, "PATH") || !strings.Contains(out, "b/c.md") {
		t.Fatalf("files %s", out)
	}

	// Recording records show the decoded size of their output.
	f.json("GET "+sp+"/runs/run_1/recording", 200, map[string]any{"records": []any{map[string]any{"seq": 1, "k": "o", "t": 5, "d": base64.StdEncoding.EncodeToString([]byte("hello"))}}, "next_after": 1})
	if out := e.ok(t, "run", "recording", "get", "--space", spaceID, "run_1", "--after", "0"); !strings.Contains(out, "5") || !strings.Contains(out, "BYTES") {
		t.Fatalf("records %s", out)
	}
}

// The change feed is printed one line per change until --count, as JSON Lines with -o json; gaps go to stderr and an
// ended stream exits 2.
func TestWatch(t *testing.T) {
	f := newFake(t)
	end := false
	f.on("GET /watch", func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		if r.Header.Get("Accept") != "text/event-stream" {
			problem(w, 406, "not_acceptable", "accept")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, ": comment\n\nevent: heartbeat\ndata: {}\n\n")
		fmt.Fprint(w, "event: gap\ndata: {\"after\":3,\"oldest\":9}\n\n")
		for i := 10; i < 13; i++ {
			fmt.Fprintf(w, "event: change\nid: %d\ndata: {\"seq\":%d,\"at\":1762160400000000,\"space_id\":\"%s\",\"resource_type\":\"task\",\"resource_id\":\"tsk_%d\",\"action\":\"updated\",\"version\":2}\n\n", i, i, spaceID, i)
		}
		if end {
			fmt.Fprint(w, "event: end\ndata: {\"reason\":\"unauthenticated\"}\n\n")
		}
	})
	e := f.env()
	r := e.run(t, "watch", "--after", "3", "--count", "2")
	if r.code != 0 || strings.Count(r.out, "\n") != 2 || !strings.Contains(r.out, "tsk_11") || strings.Contains(r.out, "tsk_12") || !strings.Contains(r.err, "gap") {
		t.Fatalf("watch %v", r)
	}
	if q := f.last("GET", "/watch").query; q != "after=3" {
		t.Fatalf("query %s", q)
	}
	r = e.run(t, "-o", "json", "watch")
	if lines := strings.Split(strings.TrimSpace(r.out), "\n"); len(lines) != 3 || !strings.HasPrefix(lines[0], `{"action":"updated"`) {
		t.Fatalf("json lines %q", r.out)
	}
	if out := e.ok(t, "-o", "yaml", "watch", "--count", "1"); !strings.Contains(out, "resource_type: task") {
		t.Fatalf("yaml %s", out)
	}
	end = true
	if r := e.code(t, ExitAPI, "watch"); !strings.Contains(r.err, "unauthenticated") {
		t.Fatalf("end %s", r.err)
	}
	f.on("GET /watch", func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
		problem(w, 401, "unauthenticated", "no")
	})
	e.code(t, ExitAuth, "watch")
}

// Commands that build their bodies: queue policy (read just before), dependencies, assignment, space accounts, skills
// from a directory, OIDC providers, identities and quarantine.
func TestBodyBuilders(t *testing.T) {
	f := newFake(t)
	echo := func(w http.ResponseWriter, _ *http.Request, body map[string]any) { reply(w, 200, body) }
	e := f.env()

	f.json("GET "+sp+"/tasks", 200, map[string]any{"tasks": []any{}, "policy": map[string]any{"max_active": 1, "incident_insertion": "top",
		"incident_preempts": true, "escalation_reorders": false, "version": 5}})
	f.on("PUT "+sp+"/queue/policy", echo)
	e.ok(t, "task", "set-policy", "--space", spaceID, "--max-active", "3")
	if b := f.last("PUT", sp+"/queue/policy").body; b["max_active"].(float64) != 3 || b["version"].(float64) != 5 || b["incident_insertion"] != "top" {
		t.Fatalf("policy %v", b)
	}

	f.on("PUT "+sp+"/tasks/tsk_1/dependencies", echo)
	e.code(t, ExitUsage, "task", "depends", "--space", spaceID, "tsk_1")
	e.code(t, ExitUsage, "task", "depends", "--space", spaceID, "tsk_1", "--on", "tsk_2", "--none")
	e.ok(t, "task", "depends", "--space", spaceID, "tsk_1", "--none")
	if b := f.last("PUT", sp+"/tasks/tsk_1/dependencies").body; len(b["depends_on"].([]any)) != 0 {
		t.Fatalf("none %v", b)
	}
	e.ok(t, "task", "depends", "--space", spaceID, "tsk_1", "--on", "tsk_2,tsk_3")
	if b := f.last("PUT", sp+"/tasks/tsk_1/dependencies").body; len(b["depends_on"].([]any)) != 2 {
		t.Fatalf("on %v", b)
	}

	// SM-CTRL-004: reassigning a task merges the given assignee into the current task and sends its version.
	f.json("GET "+sp+"/tasks/tsk_1", 200, map[string]any{"id": "tsk_1", "title": "first", "priority": "normal", "assignee_role": "planner",
		"assignee_run": "run_old", "exclusions": []any{}, "version": 7})
	f.on("PUT "+sp+"/tasks/tsk_1", echo)
	e.ok(t, "task", "update", "--space", spaceID, "tsk_1", "--run", "run_new", "--role", "implementor")
	if r := f.last("PUT", sp+"/tasks/tsk_1"); r.body["assignee_run"] != "run_new" || r.body["assignee_role"] != "implementor" || r.body["title"] != "first" ||
		r.header.Get("If-Match") != `"7"` {
		t.Fatalf("reassign %v %v", r.header, r.body)
	}
	e.ok(t, "task", "update", "--space", spaceID, "tsk_1", "--run", "")
	if r := f.last("PUT", sp+"/tasks/tsk_1"); r.body["assignee_run"] != "" || r.body["assignee_role"] != "planner" {
		t.Fatalf("clear run %v", r.body)
	}

	f.json("GET "+sp+"/human-actions/hac_1", 200, map[string]any{"id": "hac_1", "version": 3})
	f.on("POST "+sp+"/human-actions/hac_1/assign", echo)
	e.code(t, ExitUsage, "human", "assign", "--space", spaceID, "hac_1")
	e.code(t, ExitUsage, "human", "assign", "--space", spaceID, "hac_1", "--assignee", "usr_1", "--unassign")
	e.ok(t, "human", "assign", "--space", spaceID, "hac_1", "--unassign")
	if r := f.last("POST", sp+"/human-actions/hac_1/assign"); r.body["assignee"] != "" || r.header.Get("If-Match") != `"3"` {
		t.Fatalf("unassign %v %v", r.header, r.body)
	}
	e.ok(t, "human", "assign", "--space", spaceID, "hac_1", "--assignee", "usr_1")

	f.on("PUT "+sp+"/accounts", echo)
	e.code(t, ExitUsage, "account", "set-space", "--space", spaceID)
	e.code(t, ExitUsage, "account", "set-space", "--space", spaceID, "--account", "acc_1", "--default", "acc_9")
	e.ok(t, "account", "set-space", "--space", spaceID, "--account", "acc_1", "--account", "acc_2", "--default", "acc_2")
	items := f.last("PUT", sp+"/accounts").body["items"].([]any)
	if len(items) != 2 || items[0].(map[string]any)["default"] != false || items[1].(map[string]any)["default"] != true {
		t.Fatalf("accounts %v", items)
	}

	dir := t.TempDir()
	for name, body := range map[string]string{"SKILL.md": "---\nname: x\n---\n", "ref/a.md": "a", ".git/HEAD": "ref"} {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	f.on("POST /skills", echo)
	e.code(t, ExitUsage, "skill", "create", "x", "--name", "X")
	e.code(t, ExitUsage, "skill", "create", "x", "--name", "X", "--dir", t.TempDir())
	e.ok(t, "skill", "create", "x", "--name", "X", "--dir", dir)
	files := f.last("POST", "/skills").body["files"].(map[string]any)
	if len(files) != 2 || files["ref/a.md"] != "a" {
		t.Fatalf("files %v", files)
	}
	f.json("GET /skills/x", 200, map[string]any{"id": "skl_1", "versions": []any{map[string]any{"state": "draft", "version": 2}}})
	f.on("PUT /skills/skl_1/draft", echo)
	e.ok(t, "skill", "draft", "x", "--dir", dir, "--notes", "n")
	if r := f.last("PUT", "/skills/skl_1/draft"); r.header.Get("If-Match") != `"2"` || r.body["notes"] != "n" {
		t.Fatalf("skill draft %v %v", r.header, r.body)
	}

	sec := writeFile(t, "oidc.secret", "client-secret-value\n", 0o600)
	f.on("POST /oidc/providers", func(w http.ResponseWriter, _ *http.Request, body map[string]any) {
		reply(w, 201, map[string]any{"id": "oip_1", "slug": body["slug"]})
	})
	e.ok(t, "oidc", "provider", "create", "porch", "--issuer", "https://porch.example.com", "--client-id", "sm", "--client-secret-file", sec, "--scope", "openid,groups")
	if r := f.last("POST", "/oidc/providers"); r.body["client_secret"] != "client-secret-value" || len(r.body["scopes"].([]any)) != 2 || r.header.Get("Idempotency-Key") == "" {
		t.Fatalf("oidc create %v", r.body)
	}
	f.json("GET /oidc/providers/oip_1", 200, map[string]any{"id": "oip_1", "version": 6, "issuer": "https://porch.example.com"})
	f.on("PATCH /oidc/providers/oip_1", echo)
	e.code(t, ExitUsage, "oidc", "provider", "update", "oip_1")
	e.code(t, ExitUsage, "oidc", "provider", "update", "oip_1", "--client-secret-file", sec, "--remove-client-secret")
	e.ok(t, "oidc", "provider", "update", "oip_1", "--remove-client-secret")
	if r := f.last("PATCH", "/oidc/providers/oip_1"); r.header.Get("If-Match") != `"6"` || len(r.body) != 1 || r.body["client_secret"] != "" {
		t.Fatalf("oidc update %v %v", r.header, r.body)
	}
	e.code(t, ExitUsage, "oidc", "provider", "delete", "oip_1")
	f.json("GET /spaces", 200, map[string]any{"items": []any{map[string]any{"id": spaceID, "slug": "payments"}}})
	f.on("POST /oidc/providers/oip_1/mappings", echo)
	e.ok(t, "oidc", "mapping", "add", "oip_1", "--group", "ops", "--role", "operator", "--domain", "payments")
	if b := f.last("POST", "/oidc/providers/oip_1/mappings").body; b["domain"] != spaceID {
		t.Fatalf("mapping %v", b)
	}
	e.ok(t, "oidc", "mapping", "add", "oip_1", "--group", "admins", "--role", "admin", "--domain", "*")
	if b := f.last("POST", "/oidc/providers/oip_1/mappings").body; b["domain"] != "*" {
		t.Fatalf("mapping * %v", b)
	}

	f.on("DELETE /auth/identities", func(w http.ResponseWriter, _ *http.Request, _ map[string]any) { w.WriteHeader(http.StatusNoContent) })
	e.code(t, ExitUsage, "identity", "unlink", "--issuer", "https://i", "--yes")
	e.code(t, ExitUsage, "identity", "unlink", "--issuer", "https://i", "--subject", "u_1")
	e.ok(t, "identity", "unlink", "--issuer", "https://i", "--subject", "u_1", "--yes")
	if q := f.last("DELETE", "/auth/identities").query; !strings.Contains(q, "subject=u_1") || !strings.Contains(q, "issuer=") {
		t.Fatalf("unlink query %s", q)
	}

	f.on("POST "+sp+"/subscriptions/sub_1/quarantine/qe_1", echo)
	e.code(t, ExitUsage, "event", "subscription", "quarantine", "discard", "--space", spaceID, "sub_1", "qe_1")
	e.ok(t, "event", "subscription", "quarantine", "discard", "--space", spaceID, "sub_1", "qe_1", "--yes")
	if b := f.last("POST", sp+"/subscriptions/sub_1/quarantine/qe_1").body; b["action"] != "discard" {
		t.Fatalf("discard %v", b)
	}
	e.ok(t, "event", "subscription", "quarantine", "reprocess", "--space", spaceID, "sub_1", "qe_1")
	if b := f.last("POST", sp+"/subscriptions/sub_1/quarantine/qe_1").body; b["action"] != "reprocess" {
		t.Fatalf("reprocess %v", b)
	}

	e.code(t, ExitUsage, "prompt", "diff", "--from", "pv_1")
	f.json("GET /prompt-diff", 200, map[string]any{"lines": []any{map[string]any{"op": "+", "text": "new line"}}})
	if out := e.ok(t, "prompt", "diff", "--from", "pv_1", "--to", "pv_2"); !strings.Contains(out, "new line") {
		t.Fatalf("diff %s", out)
	}

	// Human type drafts start from the latest published version when there is no draft (no If-Match).
	f.json("GET /human-types/deploy", 200, map[string]any{"id": "hty_1", "versions": []any{
		map[string]any{"state": "published", "number": 1, "version": 1, "outcomes": []any{"yes"}, "notes": "v1"},
		map[string]any{"state": "published", "number": 2, "version": 1, "outcomes": []any{"yes", "no"}, "id": "htv_2"}}})
	f.on("PUT /human-types/hty_1/draft", echo)
	e.ok(t, "human", "type", "draft", "deploy", "--notes", "v3")
	if r := f.last("PUT", "/human-types/hty_1/draft"); r.header.Get("If-Match") != "" || len(r.body["outcomes"].([]any)) != 2 || r.body["notes"] != "v3" || r.body["id"] != nil {
		t.Fatalf("type draft %v %v", r.header, r.body)
	}
	e.code(t, ExitUsage, "human", "type", "publish", "deploy")
}
