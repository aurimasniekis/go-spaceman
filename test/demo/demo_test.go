//go:build demo

// Package demo is SpaceMan's scripted, reproducible end-to-end demonstration (SM-VER-003, the brief's §41): thirty
// steps run in order against a real `spaceman serve` with the embedded UI, two real `spaceman-agent` processes with
// their own isolated herdr-core cores, devpulse (NATS with JetStream and the real go-pulse SDK) and devmcp, driven
// through the browser (web/e2e/demo.spec.ts), the HTTP API and the `spaceman` command line. By default both harness
// kinds are the stub harness; SPACEMAN_DEMO_REAL=1 makes the Claude Code worker a real, budget-bounded Claude Code run.
// Each step appends a JSON line to the evidence file. Run it with `make demo`; docs/DEMO.md explains every step.
package demo

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/aurimasniekis/go-spaceman/internal/recording"
)

// demo holds what the steps share.
type demo struct {
	s    *stack
	ev   *evidence
	a, b *agentProc

	adminEmail, adminPassword string
	uiDir                     string

	pay, docs, ops          string // space IDs
	payWork, docsWork       string // space folders on the node
	opsWork                 string
	kbID, kbMain            string
	connID, issueTV         string
	subPay, subDocs, subAlt string
	sample                  map[string]any

	evtItem                       string
	taskA, taskB                  string
	taskC, taskD, taskE, incident string
	ctrl, w1, w2                  string
	question, todo, custom        string
	approval                      string
	opsGen                        int64
	itemsBeforeBackup             int
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ansi matches terminal escape sequences, so recordings can be searched as text.
var ansi = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(\x07|\x1b\\)`)

func TestDemo(t *testing.T) {
	if _, err := exec.LookPath("hc"); err != nil {
		t.Fatal("the demonstration needs herdr-core (hc) on PATH")
	}
	for _, p := range []string{"git", "pnpm", "node"} {
		if _, err := exec.LookPath(p); err != nil {
			t.Fatalf("the demonstration needs %s on PATH", p)
		}
	}
	s := newStack(t)
	mode := "stub"
	if os.Getenv("SPACEMAN_DEMO_REAL") == "1" {
		mode = "real"
		s.real = true
		s.token = os.Getenv("SPACEMAN_DEMO_CLAUDE_TOKEN")
		var err error
		if s.claudeBin, err = exec.LookPath("claude"); err != nil || s.token == "" {
			t.Fatal("SPACEMAN_DEMO_REAL=1 needs claude on PATH and SPACEMAN_DEMO_CLAUDE_TOKEN (see docs/DEMO.md); not run")
		}
	}
	evPath := os.Getenv("SPACEMAN_DEMO_EVIDENCE")
	if evPath == "" {
		evPath = filepath.Join("/tmp", fmt.Sprintf("spaceman-demo-%s-%s.jsonl", mode, time.Now().UTC().Format("20060102T150405Z")))
	}
	d := &demo{s: s, ev: &evidence{path: evPath, mode: mode}, adminEmail: "alex@example.com", adminPassword: "demo-" + randHex(16),
		uiDir: filepath.Join(s.root, "ui")}
	t.Cleanup(func() {
		left := s.down(t)
		if len(left) > 0 {
			t.Errorf("processes left behind (killed): %v", left)
		}
		t.Log(d.ev.summary())
		fmt.Fprint(os.Stdout, d.ev.summary()) //nolint:forbidigo // the driver's summary table
	})
	if err := os.MkdirAll(d.uiDir, 0o700); err != nil {
		t.Fatal(err)
	}

	if !d.step(t, 0, "Stack: central, two node agents with their own herdr-core cores, devpulse, devmcp", Real, "", d.setup) {
		t.FailNow()
	}
	d.step(t, 1, "UI creation of a pinned space", Real, "browser (Playwright, Chromium) against the embedded UI", d.uiSpace)
	d.step(t, 2, "Accounts, repositories and KB configured", Real, "", d.configure)
	d.step(t, 3, "An event template instantiated with a repository variable", Real, "", d.template)
	d.step(t, 4, "A verified source event becomes a durable inbox item", Real, "devpulse: NATS JetStream and the real go-pulse SDK, not a pulsed deployment", d.event)
	d.step(t, 5, "Control delegates to Claude Code and Codex", d.harnessKind(), d.harnessWhy(), d.delegate)
	d.step(t, 6, "Separate worktrees", Real, "worktrees are created by the real agent with real Git; the programs in them are stubs", d.worktrees)
	d.step(t, 7, "Independent KB support sessions while another is busy", Simulated, "the KB sessions are stub programs that take 12 s per request", d.kbBusy)
	d.step(t, 8, "Correlated KB answers with evidence", Simulated, "answers are written by stub KB sessions through the real KB tools", d.kbAnswers)
	d.step(t, 9, "Role-memory central sync", Simulated, "a stub writes the memory files; checkpoint, sync and sharing are real", d.memory)
	d.step(t, 10, "Operator ordering", Real, "", d.ordering)
	d.step(t, 11, "A blocked top item is skipped for eligible work", Real, "", d.blockedTop)
	d.step(t, 12, "Incident policy and authoritative manual demotion", Real, "", d.incidentStep)
	d.step(t, 13, "A worker question and a human to-do", Simulated, "the asking worker is a stub Codex program", d.workerAsks)
	d.step(t, 14, "A gated operation approval", Simulated, "the gated call comes from the stub control program", d.gated)
	d.step(t, 15, "A custom-schema human action", Simulated, "requested by the stub worker; schema validation is real", d.customAction)
	d.step(t, 16, "A global operator response", Real, "answered in the browser on the global Needs you page", d.globalResponse)
	d.step(t, 17, "A durable reply delivered to the requester", Simulated, "the requester is a stub program fetching through the real bridge", d.replyDelivered)
	d.step(t, 18, "Other work continues", d.harnessKind(), d.harnessWhy(), d.otherWork)
	d.step(t, 19, "Approval and execution shown separately", Simulated, "the approved call is re-made by the stub control program", d.approveExecute)
	d.step(t, 20, "The dreamer reads local files and publishes traceable results", Simulated, "the dreamer is a stub program that reads its workspace", d.dreamStep)
	d.step(t, 21, "Restarts do not duplicate execution", Real, "real SIGTERM restarts of the agent and of central", d.restarts)
	d.step(t, 22, "Disconnect behaviour matches the guarantees", Real, "central stopped beyond lease plus grace", d.disconnect)
	d.step(t, 23, "An interrupted recording remains readable", Real, "the agent is SIGKILLed and the recording's tail is torn by the driver (injected)", d.tornRecording)
	d.step(t, 24, "A clean relocation succeeds", Real, "two agents on one machine, a local bare repository as the remote", d.cleanRelocation)
	d.step(t, 25, "A dirty relocation is refused", Real, "", d.dirtyRelocation)
	d.step(t, 26, "A restore comes up paused without stale execution authority", Real, "", d.restore)
	d.step(t, 27, "Duplicate events do not duplicate inbox items", Real, "", d.duplicates)
	d.step(t, 28, "Two subscribed spaces both receive the intended events", Real, "", d.fanOut)
	d.step(t, 29, "The website shows real screenshots", Real, "", d.screenshots)
	d.step(t, 30, "Mermaid renders", Real, "", d.mermaid)
}

// step runs one demonstration step as a subtest and records its evidence line.
func (d *demo) step(t *testing.T, n int, title, kind, why string, f func(*step)) bool {
	return t.Run(fmt.Sprintf("%02d", n), func(t *testing.T) {
		rec := &record{Step: n, Title: title, Evidence: kind, Why: why, Started: time.Now().UTC()}
		st := &step{t: t, s: d.s, rec: rec}
		defer func() {
			rec.Seconds = time.Since(rec.Started).Seconds()
			rec.Result = "pass"
			if t.Failed() {
				rec.Result = "fail"
				if rec.Failure == "" {
					rec.Failure = "failed (see the test log)"
				}
			}
			d.ev.add(*rec)
			t.Logf("step %d %s: %s", n, rec.Result, title)
		}()
		f(st)
	})
}

func (d *demo) harnessKind() string {
	if d.s.real {
		return Mixed
	}
	return Simulated
}

func (d *demo) harnessWhy() string {
	if d.s.real {
		return "the Claude Code worker is a real Claude Code run (haiku); control and the Codex worker are stub programs (no Codex credential)"
	}
	return "both harness kinds are the stub harness: the delegation mechanics are real, the programs are stubs"
}

// ---- helpers ----

func (d *demo) space(st *step, id string) obj { return st.get("/spaces/" + id) }

func (d *demo) runOf(st *step, space, id string) obj {
	return st.get("/spaces/" + space + "/runs/" + id)
}

func (d *demo) launch(st *step, space string, body map[string]any) string {
	st.t.Helper()
	r := st.call("POST", "/spaces/"+space+"/runs", body, 201)
	id := str(r, "id")
	st.eventually(40*time.Second, "run "+id+" running", func() bool { return str(d.runOf(st, space, id), "observed") == "running" })
	return id
}

func (d *demo) stopRun(st *step, space, id string) {
	st.t.Helper()
	st.call("POST", "/spaces/"+space+"/runs/"+id+"/stop", map[string]any{"mode": "kill", "reason": "demo step done"}, 200)
	st.eventually(40*time.Second, "run "+id+" ended", func() bool {
		o := str(d.runOf(st, space, id), "observed")
		return o == "exited" || o == "failed" || o == "lost"
	})
}

// samePath compares two paths after resolving symbolic links (/tmp is /private/tmp on macOS).
func samePath(a, b string) bool {
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	return err1 == nil && err2 == nil && ra == rb
}

func ended(o string) bool { return o == "exited" || o == "failed" || o == "lost" }

// recordingText is a run's recording as central serves it, without terminal escapes.
func (d *demo) recordingText(st *step, space, run string) (string, obj) {
	st.t.Helper()
	page := st.get("/spaces/" + space + "/runs/" + run + "/recording?limit=5000")
	var b strings.Builder
	for _, r := range objs(page, "records") {
		if raw, err := base64.StdEncoding.DecodeString(str(r, "d")); err == nil {
			b.Write(raw)
		}
	}
	return ansi.ReplaceAllString(b.String(), ""), page
}

func (d *demo) git(st *step, dir string, args ...string) string {
	st.t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = gitEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		st.fatalf("git %s: %v %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(st *step, path, content string) {
	st.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		st.fatalf("%v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		st.fatalf("%v", err)
	}
}

// playwright runs one phase of web/e2e/demo.spec.ts against the stack.
func (d *demo) playwright(st *step, phase string) {
	st.t.Helper()
	cmd := exec.CommandContext(context.Background(), "pnpm", "exec", "playwright", "test", "e2e/demo.spec.ts", "--reporter=list")
	cmd.Dir = filepath.Join(d.s.repo, "web")
	cmd.Env = append(cleanEnv(), "SM_URL="+d.s.url, "SM_DEMO_PHASE="+phase, "SM_DEMO_DIR="+d.uiDir)
	out, err := cmd.CombinedOutput()
	_ = os.WriteFile(filepath.Join(d.s.logs, "playwright-"+phase+".log"), out, 0o600)
	if err != nil || !strings.Contains(string(out), "1 passed") {
		st.fatalf("playwright %s: %v\n%s", phase, err, tail(string(out), 2500))
	}
	st.check("Playwright (Chromium) phase %q passed against %s", phase, d.s.url)
}

func (d *demo) readJSON(st *step, name string) obj {
	st.t.Helper()
	b, err := os.ReadFile(filepath.Join(d.uiDir, name))
	if err != nil {
		st.fatalf("%v", err)
	}
	var o obj
	if err := json.Unmarshal(b, &o); err != nil {
		st.fatalf("%s: %v", name, err)
	}
	return o
}

func (d *demo) writeJSON(st *step, name string, v any) {
	st.t.Helper()
	b, _ := json.Marshal(v)
	write(st, filepath.Join(d.uiDir, name), string(b))
}

// issueEvent writes a go-pulse-github issues event (the verified fixture with another repository, number and title).
func (d *demo) issueEvent(st *step, repo string, number int, title string) string {
	st.t.Helper()
	b, _ := json.Marshal(d.sample)
	var e map[string]any
	_ = json.Unmarshal(b, &e)
	set := func(path string, v any) {
		parts := strings.Split(path, ".")
		m := e
		for _, p := range parts[:len(parts)-1] {
			m = m[p].(map[string]any)
		}
		m[parts[len(parts)-1]] = v
	}
	set("repository.full_name", repo)
	set("payload.repository.full_name", repo)
	set("resource.number", number)
	set("resource.html_url", fmt.Sprintf("https://github.com/%s/issues/%d", repo, number))
	set("payload.issue.number", number)
	set("payload.issue.title", title)
	set("payload.issue.body", "Reported by the settlement on-call engineer.")
	set("actor.login", "sam-rivera")
	p := filepath.Join(d.s.root, "events", fmt.Sprintf("issue-%d.json", number))
	b, _ = json.Marshal(e)
	write(st, p, string(b))
	return p
}

// publish publishes an event to devpulse as the pulse-github publisher (its own NATS credentials).
func (d *demo) publish(st *step, subject, typ, id, file string) string {
	st.t.Helper()
	args := []string{"publish", "-dir", d.s.pulseDir, "-subject", subject, "-type", typ}
	if id != "" {
		args = append(args, "-id", id)
	}
	out := d.s.run(st.t, d.s.root, cleanEnv(), d.s.bin+"/devpulse", append(args, file)...)
	return strings.TrimSpace(out)
}

func (d *demo) deliveries(st *step, space, sub string) []obj {
	return st.items("/spaces/" + space + "/subscriptions/" + sub + "/deliveries")
}

func (d *demo) inboxItems(st *step, space string) []obj {
	return st.items("/spaces/" + space + "/inbox?limit=200")
}

func (d *demo) tasks(st *step) []obj { return objs(st.get("/spaces/"+d.pay+"/tasks"), "tasks") }

func (d *demo) order(st *step) []string {
	var out []string
	for _, x := range d.tasks(st) {
		out = append(out, str(x, "id"))
	}
	return out
}

func indexOf(l []string, v string) int { return slices.Index(l, v) }

// setPolicy sets the payments queue policy (three running top-level tasks, no preemption, escalation never reorders).
func (d *demo) setPolicy(st *step, incidents string) {
	st.t.Helper()
	cur := st.get("/spaces/" + d.pay + "/tasks")
	st.call("PUT", "/spaces/"+d.pay+"/queue/policy", map[string]any{"max_active": 3, "incident_insertion": incidents, "incident_preempts": false,
		"escalation_reorders": false, "version": num(cur, "policy.version")}, 200)
}

func (d *demo) humanActions(st *step) []obj {
	return st.items("/spaces/" + d.pay + "/human-actions?limit=200")
}

// ---- step 0 ----

func (d *demo) setup(st *step) {
	s := d.s
	s.build(st.t)
	st.check("built spaceman (embedded UI), spaceman-agent, devpulse, devmcp and stubharness into %s", s.bin)
	s.up(st.t)
	st.check("central serving %s (random free ports), devpulse NATS on :%d, devmcp on :%d", s.url, s.pulsePort, s.mcpPort)
	info := st.get("/system/info")
	st.note("central version %s", str(info, "version"))
	d.a = s.addAgent(st.t, st, "a", "demo-node-a")
	d.b = s.addAgent(st.t, st, "b", "demo-node-b")
	st.id("node_a", d.a.nodeID)
	st.id("node_b", d.b.nodeID)
	st.check("two nodes enrolled with one-time tokens and connected; each agent has its own herdr-core session (%s, %s), never the default core", d.a.session, d.b.session)
	if v, err := exec.CommandContext(context.Background(), "hc", "--version").Output(); err == nil {
		st.note("herdr-core %s", strings.TrimSpace(string(v)))
	}
	st.call("POST", "/users", map[string]any{"display_name": "Alex Morgan", "email": d.adminEmail, "password": d.adminPassword, "mfa_required": true,
		"grants": []map[string]string{{"role": "admin", "domain": "*"}}}, 201)
	st.check("administrator %s created with the root token (TOTP required; the password is generated per run)", d.adminEmail)
	b, err := os.ReadFile(filepath.Join(s.repo, "internal/events/template/testdata/pulse-github-issues-opened.json"))
	if err == nil {
		err = json.Unmarshal(b, &d.sample)
	}
	if err != nil {
		st.fatalf("sample event: %v", err)
	}
	if s.real {
		st.note("real mode: Claude Code worker uses %s; the credential exists only in the driver's memory and in a SpaceMan account", filepath.Base(s.claudeBin))
	}
}

// ---- step 1 ----

func (d *demo) uiSpace(st *step) {
	d.writeJSON(st, "ui-space-in.json", map[string]string{"email": d.adminEmail, "password": d.adminPassword, "node": "demo-node-a",
		"name": "Payments platform", "slug": "payments", "goals": "Keep the settlement pipeline green; ship the retry-budget change this week."})
	d.playwright(st, "space")
	out := d.readJSON(st, "ui-space-out.json")
	d.pay = str(out, "id")
	sp := d.space(st, d.pay)
	if str(sp, "slug") != "payments" || str(sp, "node_id") != d.a.nodeID || !boolean(sp, "lease.valid") {
		st.fatalf("space %+v", sp)
	}
	d.payWork = str(sp, "workdir")
	st.id("space_payments", d.pay)
	st.check("space %s created in the browser (TOTP sign-in, New space form, Pinned node demo-node-a); the UI showed the lease as valid", d.pay)
	st.check("API: node_id %s, lease generation %d valid", str(sp, "node_id"), num(sp, "lease.generation"))
	ev := st.items("/audit?action=space.created&target_id=" + d.pay)
	if len(ev) != 1 || str(ev[0], "actor_type") != "user" {
		st.fatalf("audit of the creation: %+v", ev)
	}
	st.check("audit: space.created by user %s (not the root token)", str(ev[0], "actor_id"))
	if _, err := os.Stat(filepath.Join(d.uiDir, "shots", "01-space-created.png")); err != nil {
		st.fatalf("browser screenshot: %v", err)
	}
	st.note("browser screenshots of this run: %s", filepath.Join(d.uiDir, "shots"))
}

// ---- step 2 ----

func (d *demo) configure(st *step) {
	s := d.s
	secret := "stub-claude-token"
	if s.real {
		secret = s.token
	}
	claude := st.call("POST", "/accounts", map[string]any{"name": "team-claude", "kind": "claude_oauth_token", "secret": secret, "enabled": true,
		"description": "Claude Code subscription token"}, 201)
	codex := st.call("POST", "/accounts", map[string]any{"name": "team-codex", "kind": "openai_api_key", "secret": "stub-openai-key", "enabled": true,
		"description": "Codex key (a stub value: no Codex credential is available)"}, 201)
	if strings.Contains(fmt.Sprint(claude), secret) || strings.Contains(fmt.Sprint(codex), "stub-openai-key") {
		st.fatalf("an account response contains its secret")
	}
	st.check("accounts team-claude (claude_oauth_token%s) and team-codex (openai_api_key, stub value) created; responses never contain the secret",
		map[bool]string{true: ", the real token", false: ", stub value"}[s.real])

	docs := st.call("POST", "/spaces", map[string]any{"slug": "docs", "name": "Docs site", "node_id": d.a.nodeID, "goals": "Guides match the API."}, 201)
	ops := st.call("POST", "/spaces", map[string]any{"slug": "ops", "name": "Operations", "node_id": d.a.nodeID, "disconnect_policy": "kill",
		"goals": "Runbooks and maintenance jobs."}, 201)
	d.docs, d.docsWork, d.ops, d.opsWork = str(docs, "id"), str(docs, "workdir"), str(ops, "id"), str(ops, "workdir")
	st.id("space_docs", d.docs)
	st.id("space_ops", d.ops)
	for _, sp := range []string{d.pay, d.docs, d.ops} {
		st.call("PUT", "/spaces/"+sp+"/accounts", map[string]any{"items": []map[string]any{{"account_id": str(claude, "id"), "default": true},
			{"account_id": str(codex, "id"), "default": true}}}, 200)
	}
	for _, sp := range []string{d.docs, d.ops} {
		st.eventually(30*time.Second, "lease of "+sp, func() bool { return boolean(d.space(st, sp), "lease.valid") })
	}
	st.check("spaces docs and ops (disconnect policy kill) pinned to demo-node-a; both accounts bound as the defaults of all three spaces")

	// Repositories in the space folders on the node (SpaceMan keeps no repository list; the node's Git is used).
	spaceDir := func(work string) string { return filepath.Join(d.a.dir, "spaces", work) }
	origins := filepath.Join(s.root, "origins")
	api := filepath.Join(spaceDir(d.payWork), "repos", "payments-api")
	d.git(st, "/", "init", "-q", "--bare", "-b", "main", filepath.Join(origins, "payments-api.git"))
	d.git(st, "/", "clone", "-q", filepath.Join(origins, "payments-api.git"), api)
	write(st, filepath.Join(api, "README.md"), "# payments-api\n\nSettlement and refunds services.\n")
	write(st, filepath.Join(api, "services", "settlement", "retry.go"), "package settlement\n\n// RetryBudget is the number of retries per batch.\nconst RetryBudget = 3\n")
	d.git(st, api, "checkout", "-q", "-b", "main")
	d.git(st, api, "add", "-A")
	d.git(st, api, "commit", "-q", "-m", "Start payments-api")
	d.git(st, api, "push", "-q", "-u", "origin", "main")
	kbRepo := filepath.Join(spaceDir(d.payWork), "repos", "payments-kb")
	write(st, filepath.Join(kbRepo, "README.md"), "# Payments knowledge\n\nDecisions live in decisions/.\n")
	write(st, filepath.Join(kbRepo, "decisions", "ADR-0002-retry-budget.md"), "# ADR-0002: Retry budget per batch\n\nStatus: accepted\n\nSettlement retries use a budget of 5 per batch.\n")
	d.git(st, kbRepo, "init", "-q", "-b", "trunk")
	d.git(st, kbRepo, "add", "-A")
	d.git(st, kbRepo, "commit", "-q", "-m", "Start the payments KB")
	d.git(st, kbRepo, "branch", "main")
	d.kbMain = d.git(st, kbRepo, "rev-parse", "main")
	site := filepath.Join(spaceDir(d.docsWork), "repos", "docs-site")
	d.git(st, "/", "init", "-q", "--bare", "-b", "main", filepath.Join(origins, "docs-site.git"))
	d.git(st, "/", "clone", "-q", filepath.Join(origins, "docs-site.git"), site)
	write(st, filepath.Join(site, "index.md"), "# Docs\n")
	d.git(st, site, "checkout", "-q", "-b", "main")
	d.git(st, site, "add", "-A")
	d.git(st, site, "commit", "-q", "-m", "Start the docs")
	d.git(st, site, "push", "-q", "-u", "origin", "main")
	st.check("repositories on demo-node-a: payments/repos/payments-api (pushed to a bare origin), payments/repos/payments-kb (no remote), docs/repos/docs-site (pushed)")

	// Roles: KB sessions and the dreamer run as the Codex kind, so in real mode only the Claude Code worker is real.
	prof := st.call("PUT", "/harness-profiles/new", map[string]any{"slug": "codex-default", "name": "Codex (default model)", "harness": "codex", "model": "", "settings": map[string]any{}}, 200)
	kbT := st.call("PUT", "/role-templates/new", map[string]any{"slug": "kb-keeper", "name": "KB support", "kind": "kb", "profile_id": str(prof, "id"), "settings": map[string]any{}}, 200)
	drT := st.call("PUT", "/role-templates/new", map[string]any{"slug": "dreamer", "name": "Dreamer", "kind": "dreamer", "profile_id": str(prof, "id"), "settings": map[string]any{}}, 200)
	st.call("PUT", "/spaces/"+d.pay+"/roles/kb-support", map[string]any{"template_id": str(kbT, "id"), "settings": map[string]any{}}, 200)
	st.call("PUT", "/spaces/"+d.pay+"/roles/dreamer", map[string]any{"template_id": str(drT, "id"), "settings": map[string]any{}}, 200)
	kb := st.call("POST", "/spaces/"+d.pay+"/kbs", map[string]any{"slug": "payments", "name": "Payments knowledge", "repo": "repos/payments-kb", "base_branch": "main",
		"space_role": "kb-support", "write_policy": "review", "allowed_roles": []string{"control", "implementor-backend", "implementor-frontend"}, "max_sessions": 3,
		"entry_instructions": "Answer from the committed knowledge and cite it.\n/kb-delay 12\n/kb-answer Settlement retries use a budget of 5 per batch (decisions/ADR-0002-retry-budget.md, accepted)."}, 201)
	d.kbID = str(kb, "id")
	st.id("kb", d.kbID)
	st.check("KB payments over repos/payments-kb (base main %s, review write policy, up to 3 sessions, role kb-support)", d.kbMain[:12])

	mcp := st.call("POST", "/mcp-servers", map[string]any{"slug": "tickets", "name": "Ticket tracker", "url": s.mcpURL, "allow_private": true,
		"headers": map[string]string{"Authorization": "Bearer demo-mcp-token"}}, 201)
	st.call("POST", "/mcp-servers/"+str(mcp, "id")+"/grants", map[string]any{"space_id": d.pay, "role": "control"}, 201)
	pol := st.call("POST", "/approval-policies", map[string]any{"space_id": d.pay, "operation": "mcp.tickets.ticket_comment", "uses": 1, "valid_seconds": 3600,
		"note": "Ticket comments are visible to the finance team."}, 201)
	st.id("approval_policy", str(pol, "id"))
	st.check("MCP server tickets (devmcp, bearer token held by SpaceMan) granted to control; ticket_comment needs one person's approval")

	ht := st.call("POST", "/human-types", map[string]any{"slug": "deploy-window", "name": "Deploy window", "description": "Agree on when a production deployment may happen.",
		"request_schema": map[string]any{"type": "object", "required": []string{"environment", "change"}, "additionalProperties": false, "properties": map[string]any{
			"environment": map[string]any{"type": "string", "enum": []string{"staging", "production"}}, "change": map[string]any{"type": "string"},
			"rollback": map[string]any{"type": "string"}}},
		"response_schema": map[string]any{"type": "object", "required": []string{"window_start"}, "additionalProperties": false, "properties": map[string]any{
			"window_start": map[string]any{"type": "string", "format": "date-time"}, "duration_minutes": map[string]any{"type": "integer", "minimum": 5}}},
		"outcomes": []string{"scheduled", "declined"}, "default_priority": "high"}, 201)
	draft := objs(ht, "versions")
	if len(draft) == 0 {
		st.fatalf("human type without a draft: %+v", ht)
	}
	st.call("POST", "/human-types/"+str(ht, "id")+"/publish", nil, 200, ifMatch(draft[0])...)
	st.call("PUT", "/spaces/"+d.pay+"/human-types/"+str(ht, "id"), map[string]any{"label": "Production deploy window"}, 200)
	st.check("custom human-action type deploy-window (request and response JSON Schemas, outcomes scheduled/declined) published as v1 and enabled in payments")

	d.setPolicy(st, "priority")
	st.check("queue policy: up to 3 running top-level tasks, incidents inserted by priority, no preemption, escalation does not reorder")
}

// ---- step 3 ----

func (d *demo) template(st *step) {
	s := d.s
	creds, err := os.ReadFile(filepath.Join(s.pulseDir, "spaceman.creds"))
	if err != nil {
		st.fatalf("%v", err)
	}
	pulseURL := waitFile(st.t, filepath.Join(s.pulseDir, "url"))
	code, body := st.req("POST", "/event-connections", map[string]any{"name": "demo-pulse", "url": pulseURL, "creds": string(creds), "enabled": true})
	if code != 201 || bytes.Contains(body, []byte("SEED")) || bytes.Contains(body, []byte("NATS USER JWT")) {
		st.fatalf("connection: %d %s", code, tail(string(body), 300))
	}
	var conn obj
	_ = json.Unmarshal(body, &conn)
	d.connID = str(conn, "id")
	st.check("event connection to devpulse with SpaceMan's own NATS credentials; the response never echoes them")
	var issue obj
	for _, x := range st.items("/event-templates") {
		if str(x, "slug") == "github-issue" {
			issue = x
		}
	}
	if issue == nil || !boolean(issue, "builtin") {
		st.fatalf("the built-in github-issue template is missing")
	}
	v := objs(issue, "versions")[0]
	d.issueTV = str(v, "id")
	vars := []string{}
	for _, x := range objs(v, "definition.variables") {
		vars = append(vars, str(x, "name"))
	}
	if !slices.Contains(vars, "repository") {
		st.fatalf("template variables %v", vars)
	}
	st.check("built-in template github-issue v%d (%s) with variables %v", num(v, "version"), str(v, "status"), vars)
	ev := map[string]any{"subject": "evt.github.example-org.webhook.issues.opened", "type": "github.webhook.issues.opened.v1", "data": d.sample}
	vv := map[string]string{"account_key": "example-org", "repository": "example-org/payments-api"}
	pv := st.call("POST", "/event-templates/preview", map[string]any{"definition": at(v, "definition"), "vars": vv, "event": ev}, 200)
	st.check("preview of the verified go-pulse-github fixture with repository=example-org/payments-api: %s (it names another repository)", str(pv, "outcome"))
	sub := st.call("POST", "/spaces/"+d.pay+"/subscriptions", map[string]any{"name": "payments-api issues", "connection_id": d.connID, "template_version_id": d.issueTV, "vars": vv}, 201)
	d.subPay = str(sub, "id")
	if p := st.call("POST", "/spaces/"+d.pay+"/subscriptions", map[string]any{"name": "wide", "connection_id": d.connID, "template_version_id": d.issueTV,
		"vars": map[string]string{"account_key": "*", "repository": "example-org/payments-api"}}, 422); str(p, "code") != "validation_failed" {
		st.fatalf("wildcard variable: %+v", p)
	}
	st.check("a variable value '*' is refused (422 validation_failed): variables cannot widen the subject")
	sub2 := st.call("POST", "/spaces/"+d.docs+"/subscriptions", map[string]any{"name": "payments-api issues (docs)", "connection_id": d.connID, "template_version_id": d.issueTV, "vars": vv}, 201)
	d.subDocs = str(sub2, "id")
	st.id("subscription_payments", d.subPay)
	st.id("subscription_docs", d.subDocs)
	if str(sub, "consumer") == str(sub2, "consumer") {
		st.fatalf("both subscriptions share consumer %s", str(sub, "consumer"))
	}
	st.check("the same template version instantiated in payments and docs with repository=example-org/payments-api; grant %s; consumers %s and %s",
		str(sub, "grant"), str(sub, "consumer"), str(sub2, "consumer"))
	for _, x := range [][2]string{{d.pay, d.subPay}, {d.docs, d.subDocs}} {
		st.eventually(40*time.Second, "subscription running", func() bool { return str(st.get("/spaces/"+x[0]+"/subscriptions/"+x[1]), "state") == "running" })
	}
	st.check("both subscriptions running (durable JetStream consumers)")
}

// ---- step 4 ----

func (d *demo) event(st *step) {
	f := d.issueEvent(st, "example-org/payments-api", 101, "Settlement retries exhaust the budget on weekends")
	out := d.publish(st, "evt.github.example-org.webhook.issues.opened", "github.webhook.issues.opened.v1", "demo-issue-101", f)
	st.check("published issue #101 as the pulse-github publisher: %s", out)
	other := d.issueEvent(st, "example-org/billing-ui", 7, "Dark mode contrast")
	d.publish(st, "evt.github.example-org.webhook.issues.opened", "github.webhook.issues.opened.v1", "demo-issue-billing-7", other)
	var del obj
	st.eventually(40*time.Second, "event accepted", func() bool {
		del = find(d.deliveries(st, d.pay, d.subPay), "event_id", "demo-issue-101")
		return del != nil && str(del, "outcome") == "accepted"
	})
	st.eventually(40*time.Second, "other repository filtered", func() bool {
		x := find(d.deliveries(st, d.pay, d.subPay), "event_id", "demo-issue-billing-7")
		return x != nil && str(x, "outcome") == "filtered"
	})
	st.check("delivery demo-issue-101: accepted (stream seq %d, origin %s); an issue of example-org/billing-ui on the same subject: filtered, no item",
		num(del, "stream_seq"), str(del, "origin"))
	d.evtItem = str(del, "inbox_item_id")
	it := st.get("/spaces/" + d.pay + "/inbox/" + d.evtItem)
	if str(it, "source_kind") != "event" || str(it, "subscription_id") != d.subPay || !strings.Contains(str(it, "title"), "#101") {
		st.fatalf("inbox item %+v", it)
	}
	st.id("inbox_item", d.evtItem)
	keys := []string{}
	if m, ok := at(it, "trusted").(map[string]any); ok {
		for k := range m {
			keys = append(keys, k)
		}
		slices.Sort(keys)
	}
	st.check("inbox item %s %q: source_kind event, subscription %s, dedup key %s, trusted metadata keys %v (set by SpaceMan, not the payload)",
		d.evtItem, str(it, "title"), str(it, "subscription_id"), str(it, "dedup_key"), keys)
	for _, x := range d.inboxItems(st, d.pay) {
		if strings.Contains(str(x, "title"), "Dark mode") {
			st.fatalf("the filtered event produced an item")
		}
	}
	st.note("durability: the item is re-read after the central restart in step 21; ack-after-acceptance is covered by TestIngestFanOutAndProvenance")
}

// ---- step 5 ----

func (d *demo) delegate(st *step) {
	a := st.call("POST", "/spaces/"+d.pay+"/tasks", map[string]any{"title": "Make the settlement retry budget time-based", "status": "ready", "priority": "high",
		"from_inbox_item": d.evtItem, "objective": "Retries span the bank maintenance window.", "acceptance": "- [ ] Budget is time-based\n- [ ] Tests"}, 201)
	b := st.call("POST", "/spaces/"+d.pay+"/tasks", map[string]any{"title": "Show the retry budget on the settlement dashboard", "status": "ready",
		"objective": "Operators see how much of the budget is left."}, 201)
	d.taskA, d.taskB = str(a, "id"), str(b, "id")
	st.id("task_a", d.taskA)
	st.id("task_b", d.taskB)
	if it := st.get("/spaces/" + d.pay + "/inbox/" + d.evtItem); str(it, "linked_id") != d.taskA {
		st.fatalf("inbox item not linked to the task: %+v", it)
	}
	st.check("the operator accepted the event as task A %s (the item is linked and done); task B %s added", d.taskA, d.taskB)

	w1 := map[string]any{"task_id": d.taskA, "role": "implementor-backend", "harness": "claude-code", "repo": "repos/payments-api"}
	if d.s.real {
		w1["model"] = "haiku"
		w1["prompt"] = "This is a short SpaceMan demonstration. Do exactly this and nothing else: " +
			"1. Call the MCP tool spaceman_kb_ask with kb \"payments\", kind \"context\" and question \"Are there rules about when settlement may retry?\". " +
			"2. Call the MCP tool spaceman_task_set_status with task_id \"" + d.taskA + "\", status \"review\" and result \"Checked the retry rules (demo).\". " +
			"3. Reply with the word FINISHED. Do not edit files and do not run commands."
	} else {
		w1["prompt"] = "Keep the change small.\n/transcript\n" +
			"/call spaceman_kb_ask " + jsonArg(map[string]any{"kb": "payments", "kind": "context", "question": "Are there rules about when settlement may retry?", "task_id": d.taskA}) + "\n" +
			"/call spaceman_task_set_status " + jsonArg(map[string]any{"task_id": d.taskA, "status": "review", "result": "Retry budget made time-based in branch spaceman/" + d.taskA + "."})
	}
	w2 := map[string]any{"task_id": d.taskB, "role": "implementor-frontend", "harness": "codex", "repo": "repos/payments-api", "prompt": "Ask before you deploy.\n" +
		"/call spaceman_kb_ask " + jsonArg(map[string]any{"kb": "payments", "kind": "decision", "question": "Where is the retry budget recorded?", "task_id": d.taskB}) + "\n" +
		"/call spaceman_human_ask " + jsonArg(map[string]any{"type": "todo", "title": "Confirm the bank's maintenance window", "task_id": d.taskB,
		"request": map[string]any{"steps": []string{"Email the bank integration contact", "Record the window in the runbook"}}}) + "\n" +
		"/call spaceman_human_ask " + jsonArg(map[string]any{"type": "question", "title": "Which dashboard should show the retry budget?", "task_id": d.taskB, "blocks_task": true,
		"priority": "high", "request": map[string]any{"options": []string{"Settlement overview", "Operations wallboard"}}}) + "\n" +
		"/call spaceman_human_ask " + jsonArg(map[string]any{"type": "custom", "custom_type": "deploy-window", "title": "Deploy the dashboard change",
		"request": map[string]any{"environment": "production", "change": "Retry budget panel on the settlement overview", "rollback": "Hide the panel with its flag"}}) + "\n" +
		"/await-human 3\n" +
		"/call spaceman_task_set_status " + jsonArg(map[string]any{"task_id": d.taskB, "status": "review", "result": "Panel added to the dashboard the operator chose."})}
	prompt := "Coordinate the retry-budget change across the settlement services.\n" +
		"/call spaceman_worker_launch " + jsonArg(w1) + "\n" +
		"/call spaceman_worker_launch " + jsonArg(w2) + "\n" +
		"/call spaceman_kb_ask " + jsonArg(map[string]any{"kb": "payments", "kind": "decision", "question": "Which retry budget did we decide for settlement?"}) + "\n" +
		"/call tickets__ticket_comment " + jsonArg(map[string]any{"id": "PAY-409", "body": "Retry budget work delegated to two workers."})
	d.ctrl = d.launch(st, d.pay, map[string]any{"kind": "control", "role": "control", "harness": "codex", "prompt": prompt})
	st.id("control_run", d.ctrl)
	st.check("control run %s launched (Codex kind; its program is the stub, which makes the tool calls its prompt lists)", d.ctrl)
	var runs []obj
	st.eventually(60*time.Second, "two workers launched by control", func() bool {
		runs = nil
		for _, r := range st.items("/spaces/" + d.pay + "/runs") {
			if str(r, "created_by") == "agent:"+d.ctrl && str(r, "kind") == "worker" {
				runs = append(runs, r)
			}
		}
		return len(runs) == 2
	})
	for _, r := range runs {
		switch str(r, "harness") {
		case "claude-code":
			d.w1 = str(r, "id")
		case "codex":
			d.w2 = str(r, "id")
		}
	}
	if d.w1 == "" || d.w2 == "" {
		st.fatalf("workers %+v", runs)
	}
	st.id("worker_claude", d.w1)
	st.id("worker_codex", d.w2)
	for _, id := range []string{d.w1, d.w2} {
		st.eventually(60*time.Second, "worker running", func() bool { return str(d.runOf(st, d.pay, id), "observed") == "running" })
	}
	for _, x := range [][2]string{{d.taskA, d.w1}, {d.taskB, d.w2}} {
		tk := st.get("/spaces/" + d.pay + "/tasks/" + x[0])
		if str(tk, "assignee_run") != x[1] {
			st.fatalf("task %s assignee %s, want %s", x[0], str(tk, "assignee_run"), x[1])
		}
	}
	st.check("control delegated: worker %s (Claude Code, role implementor-backend) for task A and worker %s (Codex, role implementor-frontend) for task B, both created_by agent:%s and running",
		d.w1, d.w2, d.ctrl)
	if d.s.real {
		st.check("the Claude Code worker is the real claude binary (model %s) with the account's token; the Codex worker is a stub (no Codex credential: not exercised for real)",
			str(d.runOf(st, d.pay, d.w1), "launch.model"))
		d.noTokenAtRest(st)
	} else {
		st.note("stub mode: both workers are the stub harness; the launch, account materialization, MCP bridge and worktrees are real")
	}
}

// noTokenAtRest checks that the real credential is not stored in clear in central's data, the logs or herdr-core's state.
func (d *demo) noTokenAtRest(st *step) {
	tok := []byte(d.s.token)
	found := ""
	_ = filepath.Walk(d.s.root, func(p string, fi os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return nil //nolint:nilerr // an unreadable entry is skipped, the walk goes on
		}
		if fi.IsDir() || fi.Size() > 64<<20 || strings.Contains(p, string(filepath.Separator)+"bin"+string(filepath.Separator)) {
			return nil
		}
		if b, err := os.ReadFile(p); err == nil && bytes.Contains(b, tok) { //nolint:gosec // the demo's own files
			found = p
		}
		return nil
	})
	if found != "" {
		st.fatalf("the real credential is stored in clear in %s", strings.TrimPrefix(found, d.s.root))
	}
	st.check("the real credential is not stored in clear anywhere under the demo directory (central data, logs, node runs, herdr-core state)")
}

// ---- step 6 ----

func (d *demo) worktrees(st *step) {
	repo := filepath.Join(d.a.dir, "spaces", d.payWork, "repos", "payments-api")
	wl := d.git(st, repo, "worktree", "list", "--porcelain")
	paths := map[string]string{}
	for _, x := range [][3]string{{d.taskA, d.w1, "A"}, {d.taskB, d.w2, "B"}} {
		tk := st.get("/spaces/" + d.pay + "/tasks/" + x[0])
		ws := str(tk, "workspace")
		if ws != "worktrees/spaceman-"+x[0] {
			st.fatalf("task %s workspace %q", x[0], ws)
		}
		p := filepath.Join(d.a.dir, "spaces", d.payWork, ws)
		if !strings.Contains(wl, p) || !strings.Contains(wl, "refs/heads/spaceman/"+x[0]) {
			st.fatalf("git worktree list does not show %s on spaceman/%s:\n%s", p, x[0], wl)
		}
		if br := d.git(st, p, "branch", "--show-current"); br != "spaceman/"+x[0] {
			st.fatalf("branch %q", br)
		}
		run := d.runOf(st, d.pay, x[1])
		state, cwd := d.a.hcTerminal(str(run, "terminal_id"))
		if cwd != "" && !samePath(cwd, p) {
			st.fatalf("worker %s runs in %q, want %s", x[1], cwd, p)
		}
		paths[x[2]] = p
		st.check("task %s: workspace %s, branch spaceman/%s; herdr-core terminal %s (%s) has cwd %s", x[2], ws, x[0], str(run, "terminal_id"), state, map[bool]string{true: "(not reported)", false: cwd}[cwd == ""])
	}
	if paths["A"] == paths["B"] {
		st.fatalf("both workers share a working tree")
	}
	st.check("git worktree list of payments-api shows the main checkout and two separate worktrees; no working tree is shared")
}

// ---- step 7 ----

func (d *demo) kbBusy(st *step) {
	var snap string
	st.eventually(120*time.Second, "two KB sessions busy at the same time", func() bool {
		det := st.get("/spaces/" + d.pay + "/kbs/" + d.kbID)
		busy := map[string]string{} // session -> request
		for _, r := range objs(det, "requests") {
			if str(r, "state") == "dispatched" && str(r, "session_id") != "" {
				busy[str(r, "session_id")] = str(r, "id") + " (" + str(r, "requester_role") + ")"
			}
		}
		runs := map[string]bool{}
		for _, s := range objs(det, "sessions") {
			if _, ok := busy[str(s, "id")]; ok && str(s, "state") == "active" && str(s, "run_id") != "" {
				runs[str(s, "run_id")] = true
			}
		}
		if len(busy) >= 2 && len(runs) >= 2 {
			parts := []string{}
			for s, r := range busy {
				parts = append(parts, s+" serving "+r)
			}
			slices.Sort(parts)
			snap = strings.Join(parts, "; ")
			return true
		}
		return false
	})
	st.check("at one instant, %s", snap)
	det := st.get("/spaces/" + d.pay + "/kbs/" + d.kbID)
	seen := map[string]string{}
	for _, s := range objs(det, "sessions") {
		if r := str(s, "run_id"); r != "" {
			if kind := str(d.runOf(st, d.pay, r), "kind"); kind != "kb" {
				st.fatalf("session run %s is a %s run", r, kind)
			}
		}
		if prev, ok := seen[str(s, "requester_instance")]; ok {
			st.fatalf("two sessions %s and %s for one requester", prev, str(s, "id"))
		}
		seen[str(s, "requester_instance")] = str(s, "id")
		st.check("session %s: requester %s, branch %s, run %s (kind kb), state %s", str(s, "id"), str(s, "requester_role"), str(s, "branch"), str(s, "run_id"), str(s, "state"))
	}
	st.check("each requesting instance has its own session (SM-INV-013: sessions are supporting agents, not controllers)")
}

// ---- step 8 ----

func (d *demo) kbAnswers(st *step) {
	want := map[string]bool{d.ctrl: true, d.w2: true}
	if !d.s.real {
		want[d.w1] = true
	}
	var reqs []obj
	st.eventually(150*time.Second, "KB requests answered", func() bool {
		reqs = objs(st.get("/spaces/"+d.pay+"/kbs/"+d.kbID), "requests")
		n := 0
		for _, r := range reqs {
			if want[str(r, "requester_run")] && str(r, "state") == "answered" {
				n++
			}
		}
		return n == len(want)
	})
	msgs := st.items("/spaces/" + d.pay + "/messages?limit=200")
	for _, r := range reqs {
		if str(r, "state") != "answered" {
			st.note("request %s from %s: %s", str(r, "id"), str(r, "requester_role"), str(r, "state"))
			continue
		}
		evs := objs(r, "evidence")
		if str(r, "basis") != "committed" || len(evs) == 0 || str(evs[0], "path") != "README.md" || str(evs[0], "revision") != d.kbMain {
			st.fatalf("request %s answer basis %s evidence %+v (KB main %s)", str(r, "id"), str(r, "basis"), evs, d.kbMain)
		}
		var note obj
		for _, m := range msgs {
			if str(m, "to_id") == str(r, "requester_run") && str(m, "correlation_id") == str(r, "id") {
				note = m
			}
		}
		if note == nil {
			st.fatalf("requester %s was not notified about %s", str(r, "requester_run"), str(r, "id"))
		}
		st.check("request %s (%s, %s) answered by session %s, basis committed, evidence %s@%s; notification %s to run %s carries correlation_id %s",
			str(r, "id"), str(r, "requester_role"), str(r, "kind"), str(r, "session_id"), str(evs[0], "path"), str(evs[0], "revision")[:12], str(note, "id"), str(r, "requester_run"), str(r, "id"))
	}
}

// ---- step 9 ----

func (d *demo) memory(st *step) {
	role := "implementor-backend"
	run := d.launch(st, d.pay, map[string]any{"kind": "worker", "role": role, "harness": "codex", "prompt": "Record what we decided.\n" +
		`/write decisions.md # Decisions\n\n- 2026-11-03: settlement retries use a budget of 5 per batch, not per item.\n` + "\n" +
		`/write notes/settlement-client.md # Settlement client\n\nRetries live in services/settlement/retry.go.\n`})
	st.id("memory_run", run)
	st.eventually(30*time.Second, "memory written", func() bool {
		txt, _ := d.recordingText(st, d.pay, run)
		return strings.Contains(txt, "wrote memory notes/settlement-client.md")
	})
	d.stopRun(st, d.pay, run)
	var col obj
	var f obj
	st.eventually(60*time.Second, "memory synced to central", func() bool {
		code, b := st.req("GET", "/spaces/"+d.pay+"/memory/"+role+"/file?path=decisions.md", nil)
		f = nil
		_ = json.Unmarshal(b, &f)
		return code == 200 && strings.Contains(str(f, "content"), "5 per batch")
	})
	col = st.get("/spaces/" + d.pay + "/memory/" + role)
	hist := st.items("/spaces/" + d.pay + "/memory/" + role + "/history")
	st.check("run %s wrote decisions.md and notes/settlement-client.md in $SPACEMAN_MEMORY_DIR; at the run's end the node checkpointed them and central acknowledged head %s (%d history entries)",
		run, str(col, "head")[:12], len(hist))
	st.check("central serves decisions.md from its own copy (GET …/memory/%s/file)", role)
	next := d.launch(st, d.pay, map[string]any{"kind": "worker", "role": role, "harness": "codex", "prompt": "Continue."})
	inst := str(d.runOf(st, d.pay, next), "instance_id")
	p := filepath.Join(d.a.dir, "memory", d.pay, role, "instances", inst, "decisions.md")
	st.eventually(20*time.Second, "memory shared with the next instance", func() bool { b, err := os.ReadFile(p); return err == nil && bytes.Contains(b, []byte("5 per batch")) })
	st.check("a new instance %s of the role starts with decisions.md in its own checkout (shared through the role's history)", inst)
	d.stopRun(st, d.pay, next)
}

// ---- step 10 ----

func (d *demo) ordering(st *step) {
	mk := func(title, prio string) string {
		return str(st.call("POST", "/spaces/"+d.pay+"/tasks", map[string]any{"title": title, "status": "ready", "priority": prio}, 201), "id")
	}
	d.taskC = mk("Rotate the settlement webhook secret", "normal")
	d.taskD = mk("Load-test the retry budget", "high")
	d.taskE = mk("Remove stale feature flags", "low")
	o := d.order(st)
	st.check("priority insertion: D (high) before C (normal) before E (low): positions %d, %d, %d", indexOf(o, d.taskD), indexOf(o, d.taskC), indexOf(o, d.taskE))
	if indexOf(o, d.taskD) >= indexOf(o, d.taskC) || indexOf(o, d.taskC) >= indexOf(o, d.taskE) {
		st.fatalf("insertion order %v", o)
	}
	st.cli("task", "move", d.taskE, "--space", "payments", "--top")
	st.cli("task", "move", d.taskC, "--space", "payments", "--after", d.taskE)
	o = d.order(st)
	if o[0] != d.taskE || o[1] != d.taskC {
		st.fatalf("after the operator's moves %v", o)
	}
	manual := 0
	for _, x := range d.tasks(st) {
		if boolean(x, "manual") {
			manual++
		}
	}
	sel := st.cli("task", "next", "--space", "payments")
	if str(sel, "selected.id") != d.taskE {
		st.fatalf("next %+v", sel)
	}
	st.check("the operator's order wins: E (low) moved to the top and C after it with the CLI; %d tasks marked manual; `task next` selects E", manual)
	st.id("task_c", d.taskC)
	st.id("task_d", d.taskD)
	st.id("task_e", d.taskE)
}

// ---- step 11 ----

func (d *demo) blockedTop(st *step) {
	st.call("POST", "/spaces/"+d.pay+"/tasks/"+d.taskE+"/blockers", map[string]any{"kind": "external", "note": "waiting for the platform team's flag audit"}, 200)
	sel := st.get("/spaces/" + d.pay + "/queue/next?record=true")
	if str(sel, "selected.id") != d.taskC {
		st.fatalf("next %+v", sel)
	}
	var reason string
	for _, x := range objs(sel, "skipped") {
		if str(x, "task_id") == d.taskE {
			reason = str(x, "reason")
		}
	}
	if reason == "" {
		st.fatalf("the blocked task is not reported as skipped: %+v", sel)
	}
	st.check("top task E blocked (external blocker): the queue selects C, the next eligible task, and records E as skipped: %q", reason)
	dec := st.items("/spaces/" + d.pay + "/queue/decisions")
	if len(dec) == 0 || str(dec[0], "selected_id") != d.taskC {
		st.fatalf("decisions %+v", dec)
	}
	st.check("the selection is recorded in the queue's decision history (%d skipped with reasons)", len(objs(dec[0], "skipped")))
}

// ---- step 12 ----

func (d *demo) incidentStep(st *step) {
	d.setPolicy(st, "top")
	st.check("queue policy changed: new incidents go to the top; escalation does not reorder")
	sub := st.call("POST", "/spaces/"+d.pay+"/subscriptions", map[string]any{"name": "production alerts", "connection_id": d.connID, "custom": map[string]any{
		"subject": "evt.ops.alert.production", "variables": []any{}, "filters": []any{}, "title": "{{data.name}} ({{data.severity}})", "urgency": "urgent",
		"body": "**{{data.name}}** fired: {{data.summary}}", "instructions": "A production alert fired for the payments services.",
		"input_schema": map[string]any{"type": "object", "required": []string{"name", "severity"}}}}, 201)
	d.subAlt = str(sub, "id")
	st.eventually(40*time.Second, "alerts subscription running", func() bool { return str(st.get("/spaces/"+d.pay+"/subscriptions/"+d.subAlt), "state") == "running" })
	f := filepath.Join(d.s.root, "events", "alert.json")
	write(st, f, `{"name":"settlement_lag","severity":"critical","summary":"Settlement batches queue for more than 10 minutes."}`)
	d.publish(st, "evt.ops.alert.production", "ops.alert.v1", "demo-alert-1", f)
	var del obj
	st.eventually(40*time.Second, "alert accepted", func() bool {
		del = find(d.deliveries(st, d.pay, d.subAlt), "event_id", "demo-alert-1")
		return del != nil && str(del, "outcome") == "accepted"
	})
	item := str(del, "inbox_item_id")
	inc := st.call("POST", "/spaces/"+d.pay+"/tasks", map[string]any{"title": "Settlement lag above 10 minutes", "kind": "incident", "priority": "high",
		"status": "ready", "from_inbox_item": item}, 201)
	d.incident = str(inc, "id")
	st.id("incident", d.incident)
	o := d.order(st)
	if o[0] != d.incident {
		st.fatalf("incident not at the top: %v", o)
	}
	st.check("the alert (custom subscription with an input schema) became inbox item %s; the operator made it incident %s, inserted at the top by policy", item, d.incident)
	st.cli("task", "move", d.incident, "--space", "payments", "--bottom")
	o = d.order(st)
	if o[len(o)-1] != d.incident {
		st.fatalf("demotion %v", o)
	}
	st.check("the operator dragged the incident to the bottom (position %d of %d)", indexOf(o, d.incident)+1, len(o))
	out := d.publish(st, "evt.ops.alert.production", "ops.alert.v1", "demo-alert-1", f)
	st.check("the same alert published again with the same event ID: %s", out)
	sv := st.get("/spaces/" + d.pay + "/subscriptions/" + d.subAlt)
	rp := st.call("POST", "/spaces/"+d.pay+"/subscriptions/"+d.subAlt+"/replay", map[string]any{"policy": "all", "reason": "demo: redeliver the alert"}, 202, ifMatch(sv)...)
	st.eventually(40*time.Second, "replayed alert handled", func() bool {
		for _, x := range d.deliveries(st, d.pay, d.subAlt) {
			if str(x, "event_id") == "demo-alert-1" && str(x, "origin") == "replay" {
				return str(x, "outcome") == "duplicate"
			}
		}
		return false
	})
	st.check("an explicit replay %s redelivered it: outcome duplicate, no new inbox item", str(rp, "id"))
	n := 0
	for _, x := range d.inboxItems(st, d.pay) {
		if str(x, "subscription_id") == d.subAlt {
			n++
		}
	}
	if n != 1 {
		st.fatalf("alert items %d", n)
	}
	esc := st.call("POST", "/spaces/"+d.pay+"/tasks/"+d.incident+"/escalate", map[string]any{"reason": "customer impact"}, 200)
	o = d.order(st)
	if o[len(o)-1] != d.incident || str(esc, "priority") != "urgent" {
		st.fatalf("after escalation: priority %s order %v", str(esc, "priority"), o)
	}
	st.check("escalation raised the priority to urgent but did not move it (policy escalation_reorders=false): the incident stays where the operator put it")
}

// ---- step 13 ----

func (d *demo) workerAsks(st *step) {
	st.eventually(60*time.Second, "worker's human actions", func() bool {
		for _, a := range d.humanActions(st) {
			if str(a, "requester_run") != d.w2 {
				continue
			}
			switch str(a, "type") {
			case "question":
				d.question = str(a, "id")
			case "todo":
				d.todo = str(a, "id")
			case "custom":
				d.custom = str(a, "id")
			}
		}
		return d.question != "" && d.todo != "" && d.custom != ""
	})
	st.id("question", d.question)
	st.id("todo", d.todo)
	st.id("custom", d.custom)
	q := st.get("/spaces/" + d.pay + "/human-actions/" + d.question)
	if str(q, "task_id") != d.taskB || !boolean(q, "blocks_task") {
		st.fatalf("question %+v", q)
	}
	tb := st.get("/spaces/" + d.pay + "/tasks/" + d.taskB)
	blocked := false
	for _, b := range objs(tb, "blockers") {
		blocked = blocked || (str(b, "kind") == "human_action" && str(b, "ref_id") == d.question)
	}
	if !blocked {
		st.fatalf("task B has no human-action blocker: %+v", at(tb, "blockers"))
	}
	st.check("worker %s asked question %s %q (blocking task B: blocker on the task) and to-do %s, requester recorded by SpaceMan, not the request body", d.w2, d.question, str(q, "title"), d.todo)
	st.cli("human", "respond", d.todo, "--space", "payments", "--outcome", "completed", "--note", "The bank confirmed 01:00-03:00 UTC.")
	td := st.get("/spaces/" + d.pay + "/human-actions/" + d.todo)
	st.check("the to-do was completed from the CLI: status %s, outcome %s", str(td, "status"), str(td, "outcome"))
}

// ---- step 14 ----

func (d *demo) gated(st *step) {
	var apv obj
	st.eventually(60*time.Second, "approval requested", func() bool {
		l := st.items("/spaces/" + d.pay + "/approvals")
		apv = find(l, "operation", "mcp.tickets.ticket_comment")
		return apv != nil
	})
	d.approval = str(apv, "id")
	st.id("approval", d.approval)
	if str(apv, "state") != "pending" || str(apv, "requester_run") != d.ctrl || len(objs(apv, "executions")) != 0 {
		st.fatalf("approval %+v", apv)
	}
	ha := st.get("/spaces/" + d.pay + "/human-actions/" + str(apv, "human_action_id"))
	st.check("control's call tickets__ticket_comment was held: approval %s pending, bound to run %s and argument digest %s…, human action %s (%s); no execution",
		d.approval, d.ctrl, str(apv, "args_digest")[:12], str(ha, "id"), str(ha, "type"))
	txt, _ := d.recordingText(st, d.pay, d.ctrl)
	if !strings.Contains(txt, "waiting for approval") {
		st.fatalf("control's terminal does not show it waiting")
	}
	st.check("control's terminal recording shows the call waiting for a person's approval")
}

// ---- step 15 ----

func (d *demo) customAction(st *step) {
	c := st.get("/spaces/" + d.pay + "/human-actions/" + d.custom)
	if str(c, "type_slug") != "deploy-window" || num(c, "type_version") != 1 {
		st.fatalf("custom action %+v", c)
	}
	st.check("worker's custom action %s: type deploy-window v%d (%s), request validated against the type's request schema", d.custom, num(c, "type_version"), str(c, "type_version_id"))
	p := st.call("POST", "/spaces/"+d.pay+"/human-actions", map[string]any{"type": "custom", "type_slug": "deploy-window", "title": "Invalid",
		"request": map[string]any{"environment": "moon"}}, 422)
	st.check("a request that breaks the schema is refused: %s", str(p, "code"))
	p = st.call("POST", "/spaces/"+d.pay+"/human-actions/"+d.custom+"/respond", map[string]any{"outcome": "scheduled", "data": map[string]any{"duration_minutes": 30}}, 422, ifMatch(c)...)
	st.check("a response without the required window_start is refused: %s", str(p, "code"))
	c = st.get("/spaces/" + d.pay + "/human-actions/" + d.custom)
	r := st.call("POST", "/spaces/"+d.pay+"/human-actions/"+d.custom+"/respond", map[string]any{"outcome": "scheduled",
		"data": map[string]any{"window_start": "2026-11-05T06:00:00Z", "duration_minutes": 30}}, 200, ifMatch(c)...)
	if str(r, "outcome") != "scheduled" || num(r, "type_version") != 1 {
		st.fatalf("custom response %+v", r)
	}
	st.check("the schema-valid response is stored with outcome scheduled; the action keeps type version 1 (a custom outcome grants nothing)")
}

// ---- step 16 ----

func (d *demo) globalResponse(st *step) {
	q := st.get("/spaces/" + d.pay + "/human-actions/" + d.question)
	d.writeJSON(st, "ui-respond-in.json", map[string]string{"title": str(q, "title"), "answer": "Settlement overview", "space": "Payments platform"})
	d.playwright(st, "respond")
	q = st.get("/spaces/" + d.pay + "/human-actions/" + d.question)
	if str(q, "outcome") != "answered" || !strings.Contains(fmt.Sprint(at(q, "response")), "Settlement overview") {
		st.fatalf("question after the browser answer: %+v", q)
	}
	st.check("the question was answered in the browser from the global Needs you page (it lists actions of every readable space): outcome answered, answer \"Settlement overview\"")
	ev := st.items("/audit?target_id=" + d.question)
	who := ""
	for _, e := range ev {
		if strings.Contains(str(e, "action"), "respond") || strings.Contains(str(e, "action"), "resolved") {
			who = str(e, "actor_type") + ":" + str(e, "actor_id")
		}
	}
	st.check("audit records the response by %s", who)
}

// ---- step 17 ----

func (d *demo) replyDelivered(st *step) {
	var n obj
	st.eventually(60*time.Second, "response delivered to the requester", func() bool {
		q := st.get("/spaces/" + d.pay + "/human-actions/" + d.question)
		ns := objs(q, "notifications")
		if len(ns) == 0 {
			return false
		}
		n = ns[0]
		return str(n, "target_kind") == "run" && str(n, "target_run") == d.w2 && (str(n, "state") == "delivered" || str(n, "state") == "acknowledged")
	})
	st.check("notification %s for the requester run %s (target_kind run): state %s, delivered at %s", str(n, "id"), d.w2, str(n, "state"), str(n, "delivered_at"))
	st.eventually(30*time.Second, "the worker printed the answer", func() bool {
		txt, _ := d.recordingText(st, d.pay, d.w2)
		return strings.Contains(txt, "human response to "+d.question) && strings.Contains(txt, "Settlement overview")
	})
	st.check("the requesting worker fetched it through its MCP bridge and its terminal recording shows the answer \"Settlement overview\"; it acknowledged it")
	st.eventually(30*time.Second, "acknowledged", func() bool {
		ns := objs(st.get("/spaces/"+d.pay+"/human-actions/"+d.question), "notifications")
		return len(ns) > 0 && str(ns[0], "state") == "acknowledged"
	})
	st.check("the response was stored durably until the requester took it (pending → delivered → acknowledged); the other worker never received it")
}

// ---- step 18 ----

func (d *demo) otherWork(st *step) {
	if str(st.get("/spaces/"+d.pay+"/approvals/"+d.approval), "state") != "pending" {
		st.fatalf("the approval is no longer pending")
	}
	st.eventually(60*time.Second, "task B in review", func() bool { return str(st.get("/spaces/"+d.pay+"/tasks/"+d.taskB), "status") == "review" })
	st.check("while control's approval is still pending, the Codex worker went on after the answer and moved task B to review")
	limit := 30 * time.Second
	if d.s.real {
		limit = 240 * time.Second
	}
	st.eventually(limit, "task A in review", func() bool { return str(st.get("/spaces/"+d.pay+"/tasks/"+d.taskA), "status") == "review" })
	tA := st.get("/spaces/" + d.pay + "/tasks/" + d.taskA)
	st.check("the Claude Code worker moved task A to review (%q)%s", str(tA, "result"), map[bool]string{true: " — a real Claude Code run (haiku) used SpaceMan's tools", false: ""}[d.s.real])
	for _, id := range []string{d.ctrl, d.w2} {
		if o := str(d.runOf(st, d.pay, id), "observed"); o != "running" {
			st.fatalf("run %s %s", id, o)
		}
	}
	sel := st.get("/spaces/" + d.pay + "/queue/next")
	st.check("control and the workers keep running; the queue still offers eligible work (%s)", str(sel, "selected.title"))
	if d.s.real {
		d.stopRun(st, d.pay, d.w1)
		u := st.get("/spaces/" + d.pay + "/runs/" + d.w1 + "/usage")
		st.check("the real Claude Code run was stopped once its task was in review (budget bound); usage known=%v source=%s estimate=%v",
			boolean(u, "known"), str(u, "source"), at(u, "estimate.usd"))
	}
}

// ---- step 19 ----

func (d *demo) approveExecute(st *step) {
	apv := st.get("/spaces/" + d.pay + "/approvals/" + d.approval)
	ha := st.get("/spaces/" + d.pay + "/human-actions/" + str(apv, "human_action_id"))
	st.call("POST", "/spaces/"+d.pay+"/human-actions/"+str(ha, "id")+"/respond", map[string]any{"outcome": "approved", "note": "Fine to tell finance."}, 200, ifMatch(ha)...)
	apv = st.get("/spaces/" + d.pay + "/approvals/" + d.approval)
	st.check("decision (made through the API with the root token): approval %s is %s by %s at %s, %d use left — no execution has happened yet (%d executions)",
		d.approval, str(apv, "state"), str(apv, "decided_by"), str(apv, "decided_at"), num(apv, "uses_left"), len(objs(apv, "executions")))
	if str(apv, "state") != "approved" {
		st.fatalf("approval %+v", apv)
	}
	st.eventually(60*time.Second, "execution recorded", func() bool {
		apv = st.get("/spaces/" + d.pay + "/approvals/" + d.approval)
		ex := objs(apv, "executions")
		return len(ex) == 1 && str(ex[0], "state") == "succeeded"
	})
	ex := objs(apv, "executions")[0]
	st.check("execution: %s succeeded (started %s, finished %s) when control called again; uses left %d", str(ex, "id"), str(ex, "started_at"), str(ex, "finished_at"), num(apv, "uses_left"))
	st.eventually(30*time.Second, "control saw the result", func() bool {
		txt, _ := d.recordingText(st, d.pay, d.ctrl)
		return strings.Contains(txt, "commented on PAY-409")
	})
	time.Sleep(4 * time.Second)
	if n := len(objs(st.get("/spaces/"+d.pay+"/approvals/"+d.approval), "executions")); n != 1 {
		st.fatalf("executions %d", n)
	}
	st.check("the upstream answer reached control (\"commented on PAY-409\"); the approval and its single execution are separate records")
}

// ---- step 20 ----

func (d *demo) dreamStep(st *step) {
	cfg := st.call("POST", "/spaces/"+d.pay+"/dreams", map[string]any{"slug": "nightly", "name": "Nightly consolidation", "space_role": "dreamer",
		"memory_roles": []string{"implementor-backend"}, "transcript_roles": []string{"implementor-backend"}, "max_minutes": 10, "instructions": "Consolidate.\n/dream"}, 201)
	r := st.call("POST", "/spaces/"+d.pay+"/dreams/"+str(cfg, "id")+"/start", nil, 201)
	run := str(r, "run_id")
	st.id("dream_run", run)
	var dr obj
	st.eventually(90*time.Second, "dream succeeded", func() bool {
		l := st.items("/spaces/" + d.pay + "/dreams/" + str(cfg, "id") + "/runs")
		if len(l) == 0 {
			return false
		}
		dr = l[0]
		return str(dr, "state") == "succeeded" || str(dr, "state") == "failed"
	})
	if str(dr, "state") != "succeeded" {
		st.fatalf("dream %+v", dr)
	}
	m, err := os.ReadFile(filepath.Join(d.a.dir, "spaces", d.payWork, "dreams", run, "DREAM.md"))
	if err != nil || !strings.Contains(string(m), "evidence/implementor-backend/") {
		st.fatalf("DREAM.md on the node: %v %q", err, tail(string(m), 300))
	}
	st.check("the node prepared the dream workspace locally: DREAM.md lists the implementor-backend memory and new transcript evidence copied from the worker's session files on the node")
	fs := objs(dr, "findings")
	if len(fs) == 0 || !strings.HasPrefix(str(fs[0], "evidence"), "evidence/implementor-backend/") {
		st.fatalf("findings %+v", fs)
	}
	st.check("dream run %s (run %s) reported %d finding(s) citing %s; summary %q", str(dr, "id"), run, len(fs), str(fs[0], "evidence"), tail(str(dr, "summary"), 160))
	st.eventually(60*time.Second, "dream memory synced", func() bool {
		code, b := st.req("GET", "/spaces/"+d.pay+"/memory/implementor-backend/file?path=dream-notes.md", nil)
		return code == 200 && bytes.Contains(b, []byte("consolidated"))
	})
	st.check("its memory change dream-notes.md was checkpointed after the report and synced to central")
	ev := st.items("/audit?action=dream.reported")
	if len(ev) == 0 || str(ev[0], "actor_id") != run {
		st.fatalf("audit %+v", ev)
	}
	st.check("audit: dream.reported by agent run %s (traceable to the run, its evidence files and its memory commit)", run)
}

// ---- step 21 ----

func (d *demo) liveRuns(st *step, space string) map[string]string {
	out := map[string]string{}
	for _, r := range st.items("/spaces/" + space + "/runs") {
		if str(r, "observed") == "running" {
			out[str(r, "id")] = str(r, "terminal_id")
		}
	}
	return out
}

func (d *demo) sameRuns(st *step, what string, before map[string]string) {
	st.t.Helper()
	st.eventually(40*time.Second, "runs running on the same terminals after "+what, func() bool {
		for id, term := range before {
			r := d.runOf(st, d.pay, id)
			if str(r, "observed") != "running" || str(r, "terminal_id") != term {
				return false
			}
		}
		return true
	})
	time.Sleep(3 * time.Second) // heartbeats, reconciliation, command re-delivery
	for id := range before {
		if n := d.a.hcTerminals(st.t, id); n != 1 {
			st.fatalf("after %s: %d terminals for run %s", what, n, id)
		}
	}
}

func (d *demo) restarts(st *step) {
	before := d.liveRuns(st, d.pay)
	nRuns := len(st.items("/spaces/" + d.pay + "/runs"))
	if len(before) < 3 {
		st.fatalf("only %d live runs", len(before))
	}
	stop(d.a.cmd, syscall.SIGTERM)
	d.s.startAgent(st.t, d.a)
	st.eventually(30*time.Second, "agent reconnected", func() bool { return boolean(st.get("/nodes/"+d.a.nodeID), "connected") })
	d.sameRuns(st, "the agent restart", before)
	st.check("agent restarted (SIGTERM): all %d live runs adopted on their terminals; herdr-core holds exactly one terminal per run", len(before))
	d.s.stopCentral(syscall.SIGTERM)
	d.s.startCentral(st.t)
	st.eventually(30*time.Second, "agent reconnected to the new central", func() bool { return boolean(st.get("/nodes/"+d.a.nodeID), "connected") })
	d.sameRuns(st, "the central restart", before)
	if n := len(st.items("/spaces/" + d.pay + "/runs")); n != nRuns {
		st.fatalf("runs %d → %d: something was relaunched", nRuns, n)
	}
	it := st.get("/spaces/" + d.pay + "/inbox/" + d.evtItem)
	st.check("central restarted (SIGTERM, same data directory): the node reconnected, the same %d runs continue on the same terminals, no run was added (%d runs), and the event's inbox item %s is still there", len(before), nRuns, str(it, "id"))
}

// ---- step 22 ----

func (d *demo) disconnect(st *step) {
	opsRun := d.launch(st, d.ops, map[string]any{"kind": "worker", "role": "implementor", "harness": "codex", "prompt": "Watch the maintenance job."})
	st.id("ops_run", opsRun)
	payRuns := d.liveRuns(st, d.pay)
	terms := map[string]string{opsRun: str(d.runOf(st, d.ops, opsRun), "terminal_id")}
	for id, term := range payRuns {
		terms[id] = term
	}
	d.s.stopCentral(syscall.SIGTERM)
	t0 := time.Now()
	time.Sleep(3 * time.Second)
	for id, term := range terms {
		if state, _ := d.a.hcTerminal(term); !strings.HasPrefix(state, "live") {
			st.fatalf("run %s ended %s after the disconnect, before its lease could expire (%s)", id, time.Since(t0).Round(time.Second), state)
		}
	}
	st.check("central stopped; 3 s later all %d runs are still live (lease %s, grace %s)", len(terms), leaseTTL, grace)
	endedAt := map[string]time.Duration{}
	deadline := time.Now().Add(leaseTTL + grace + 30*time.Second)
	for len(endedAt) < len(terms) && time.Now().Before(deadline) {
		for id, term := range terms {
			if _, ok := endedAt[id]; ok {
				continue
			}
			if state, _ := d.a.hcTerminal(term); strings.HasPrefix(state, "finished") {
				endedAt[id] = time.Since(t0)
				st.note("run %s: %s after %s", id, state, endedAt[id].Round(100*time.Millisecond))
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	if len(endedAt) != len(terms) {
		st.fatalf("%d of %d runs ended without central", len(endedAt), len(terms))
	}
	first := time.Hour
	for _, e := range endedAt {
		first = min(first, e)
	}
	if first < leaseTTL-heartbeat {
		st.fatalf("a run ended after %s, before the lease could expire", first)
	}
	opsState, _ := d.a.hcTerminal(terms[opsRun])
	st.check("the node ended every run on its own after lease plus grace (first after %s): the ops run (policy kill) %s, payments runs (policy interrupt) interrupted", first.Round(100*time.Millisecond), opsState)
	d.s.startCentral(st.t)
	st.eventually(30*time.Second, "agent reconnected", func() bool { return boolean(st.get("/nodes/"+d.a.nodeID), "connected") })
	st.eventually(40*time.Second, "central learns the runs ended", func() bool {
		for id := range payRuns {
			if !ended(str(d.runOf(st, d.pay, id), "observed")) {
				return false
			}
		}
		return ended(str(d.runOf(st, d.ops, opsRun), "observed"))
	})
	time.Sleep(4 * time.Second)
	for id := range terms {
		if n := d.a.hcTerminals(st.t, id); n != 0 {
			st.fatalf("run %s has %d live terminals after the reconnect (relaunched)", id, n)
		}
	}
	st.check("when central returned the node reported the ends (exited/lost) and nothing was relaunched: no live terminal for any of the %d runs", len(terms))
	for _, r := range st.items("/spaces/" + d.pay + "/runs") {
		if _, old := terms[str(r, "id")]; !old && str(r, "observed") == "running" {
			st.note("new run after the reconnect: %s (%s, created by %s): a separate decision, not a relaunch of an ended run", str(r, "id"), str(r, "kind"), str(r, "created_by"))
		}
	}
}

// ---- step 23 ----

func (d *demo) tornRecording(st *step) {
	run := d.launch(st, d.ops, map[string]any{"kind": "worker", "role": "implementor", "harness": "codex", "prompt": "Print progress."})
	st.id("run", run)
	term := str(d.runOf(st, d.ops, run), "terminal_id")
	dir := filepath.Join(d.a.dir, "runs", run, "recording")
	// Several seconds of output, so the segment holds several complete zstd frames (one per flush).
	st.eventually(30*time.Second, "recording has several frames of output", func() bool {
		res, err := recording.Read(dir, 0)
		return err == nil && len(res.Records) >= 8
	})
	stop(d.a.cmd, syscall.SIGKILL)
	restarted := false
	defer func() { // whatever happens below, the node comes back for the later steps
		if !restarted {
			d.s.startAgent(st.t, d.a)
		}
	}()
	segs, _ := filepath.Glob(filepath.Join(dir, "seg-*"))
	slices.Sort(segs)
	if len(segs) == 0 {
		st.fatalf("no segments in %s", dir)
	}
	last := segs[len(segs)-1]
	pre, err := recording.Read(dir, 0)
	if err != nil || len(pre.Records) == 0 {
		st.fatalf("recording after the SIGKILL: %v", err)
	}
	// The crash interrupted the write of the next frame: half of a complete zstd frame is appended (as in the fault suite).
	line, _ := json.Marshal(recording.Record{Seq: pre.Records[len(pre.Records)-1].Seq + 1, Kind: recording.KindOut, Data: []byte("torn write")})
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		st.fatalf("%v", err)
	}
	frame := enc.EncodeAll(append(line, '\n'), nil)
	f, err := os.OpenFile(last, os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // the demo's own run folder
	if err != nil {
		st.fatalf("%v", err)
	}
	_, _ = f.Write(frame[:len(frame)/2])
	_ = f.Close()
	res, err := recording.Read(dir, 0)
	if err != nil || !res.Truncated || len(res.Records) != len(pre.Records) {
		st.fatalf("torn recording: %d records (before %d), truncated=%v, err %v", len(res.Records), len(pre.Records), res.Truncated, err)
	}
	st.check("agent SIGKILLed mid-run (%d records on disk), then half of a zstd frame appended to %s (an injected torn write): the local recording still reads all %d complete records and reports truncated=%v",
		len(pre.Records), filepath.Base(last), len(res.Records), res.Truncated)
	restarted = true
	d.s.startAgent(st.t, d.a)
	st.eventually(30*time.Second, "agent reconnected", func() bool { return boolean(st.get("/nodes/"+d.a.nodeID), "connected") })
	st.eventually(40*time.Second, "run adopted", func() bool {
		r := d.runOf(st, d.ops, run)
		return str(r, "observed") == "running" && str(r, "terminal_id") == term
	})
	var page obj
	st.eventually(40*time.Second, "central's copy continues", func() bool {
		txt, p := d.recordingText(st, d.ops, run)
		page = p
		return strings.Contains(txt, "tick") && len(objs(p, "records")) > len(res.Records)
	})
	st.check("the restarted agent adopted the run on terminal %s; central's copy is readable through the API: %d records, gaps %d, truncated %v", term, len(objs(page, "records")), num(page, "gaps"), boolean(page, "truncated"))
	if res2, err := recording.Read(dir, 0); err != nil || len(res2.Records) < len(res.Records) {
		st.fatalf("local recording after the restart: %v", err)
	} else {
		st.check("the local recording stays readable after the restart (%d records, gaps %d)", len(res2.Records), res2.Gaps)
	}
	d.stopRun(st, d.ops, run)
}

// ---- step 24 ----

func (d *demo) cleanRelocation(st *step) {
	sp := d.space(st, d.docs)
	gen := num(sp, "lease.generation")
	src := filepath.Join(d.a.dir, "spaces", d.docsWork, "repos", "docs-site")
	head := d.git(st, src, "rev-parse", "HEAD")
	st.call("POST", "/spaces/"+d.docs+"/relocation", map[string]any{"destination_node_id": d.b.nodeID, "precheck": true}, 202)
	var r obj
	st.eventually(60*time.Second, "precheck", func() bool {
		r = st.get("/spaces/" + d.docs + "/relocation")
		return str(r, "state") == "checked" || str(r, "state") == "failed"
	})
	if str(r, "state") != "checked" || len(objs(r, "issues")) != 0 {
		st.fatalf("precheck %s issues %+v error %s", str(r, "state"), at(r, "issues"), str(r, "error"))
	}
	st.check("precheck of docs → demo-node-b: checked, no issues (%d repositories inspected); nothing changed", len(objs(r, "report.repositories")))
	st.call("POST", "/spaces/"+d.docs+"/state", map[string]any{"state": "paused", "mode": "hold"}, 200, ifMatch(d.space(st, d.docs))...)
	r = st.call("POST", "/spaces/"+d.docs+"/relocation", map[string]any{"destination_node_id": d.b.nodeID}, 202)
	st.id("relocation", str(r, "id"))
	st.eventually(90*time.Second, "relocation done", func() bool {
		r = st.get("/spaces/" + d.docs + "/relocation")
		return slices.Contains([]string{"done", "blocked", "failed"}, str(r, "state"))
	})
	if str(r, "state") != "done" {
		st.fatalf("relocation %s: %s %+v", str(r, "state"), str(r, "error"), at(r, "issues"))
	}
	sp = d.space(st, d.docs)
	if str(sp, "node_id") != d.b.nodeID || str(sp, "state") != "paused" || num(r, "generation_after") <= gen {
		st.fatalf("after relocation: node %s state %s generation %d → %d", str(sp, "node_id"), str(sp, "state"), gen, num(r, "generation_after"))
	}
	st.check("relocation %s done: docs now on demo-node-b, still paused, lease generation %d → %d (the source is fenced); native sessions %s", str(r, "id"), gen, num(r, "generation_after"), str(r, "continuity.native_sessions"))
	dst := filepath.Join(d.b.dir, "spaces", d.docsWork, "repos", "docs-site")
	if got := d.git(st, dst, "rev-parse", "HEAD"); got != head {
		st.fatalf("destination head %s, source %s", got, head)
	}
	st.check("the destination has repos/docs-site at the source's commit %s; source folder: %s", head[:12], str(r, "source_state"))
	st.call("POST", "/spaces/"+d.docs+"/state", map[string]any{"state": "active"}, 200, ifMatch(d.space(st, d.docs))...)
	st.eventually(30*time.Second, "lease on node b", func() bool {
		x := d.space(st, d.docs)
		return boolean(x, "lease.valid") && str(x, "lease.node_id") == d.b.nodeID
	})
	run := d.launch(st, d.docs, map[string]any{"kind": "worker", "role": "implementor", "harness": "codex", "prompt": "Check the guides."})
	if str(d.runOf(st, d.docs, run), "node_id") != d.b.nodeID || d.b.hcTerminals(st.t, run) != 1 {
		st.fatalf("run %s is not on node b", run)
	}
	st.check("resumed: a new run %s starts on demo-node-b in its own herdr-core core", run)
	d.stopRun(st, d.docs, run)
}

// ---- step 25 ----

func (d *demo) dirtyRelocation(st *step) {
	wt := filepath.Join(d.a.dir, "spaces", d.payWork, "worktrees", "spaceman-"+d.taskB)
	wip := filepath.Join(wt, "dashboard-wip.md")
	write(st, wip, "Retry budget panel: work in progress.\n")
	st.check("uncommitted work left in task B's worktree (%s)", strings.TrimPrefix(wip, d.a.dir))
	r := st.call("POST", "/spaces/"+d.pay+"/relocation", map[string]any{"destination_node_id": d.b.nodeID, "stop_runs": true}, 202)
	st.id("relocation", str(r, "id"))
	st.eventually(90*time.Second, "relocation blocked", func() bool {
		r = st.get("/spaces/" + d.pay + "/relocation")
		return slices.Contains([]string{"blocked", "done", "failed"}, str(r, "state"))
	})
	if str(r, "state") != "blocked" {
		st.fatalf("dirty relocation %s: %s", str(r, "state"), str(r, "error"))
	}
	kinds := map[string]int{}
	for _, is := range objs(r, "issues") {
		kinds[str(is, "code")]++
	}
	st.check("relocation %s refused at the check (state blocked, step %s): issues %v", str(r, "id"), str(r, "blocked_step"), kinds)
	sp := d.space(st, d.pay)
	if str(sp, "node_id") != d.a.nodeID {
		st.fatalf("the space moved: %+v", sp)
	}
	if _, err := os.Stat(filepath.Join(d.b.dir, "spaces", d.payWork)); err == nil {
		st.fatalf("the destination has a payments folder")
	}
	st.call("POST", "/spaces/"+d.pay+"/relocation/cancel", nil, 200)
	r = st.get("/spaces/" + d.pay + "/relocation")
	sp = d.space(st, d.pay)
	if b, err := os.ReadFile(wip); err != nil || !bytes.Contains(b, []byte("in progress")) {
		st.fatalf("the uncommitted work is gone: %v", err)
	}
	st.check("cancelled (%s): payments stays on demo-node-a (%s), nothing was created on demo-node-b, the uncommitted file is untouched", str(r, "state"), str(sp, "state"))
}

// ---- step 26 ----

func (d *demo) restore(st *step) {
	s := d.s
	run := d.launch(st, d.ops, map[string]any{"kind": "worker", "role": "implementor", "harness": "codex", "prompt": "Nightly job."})
	st.id("run_before_backup", run)
	d.opsGen = num(d.runOf(st, d.ops, run), "generation")
	term := str(d.runOf(st, d.ops, run), "terminal_id")
	for _, x := range d.inboxItems(st, d.pay) {
		if str(x, "subscription_id") == d.subPay {
			d.itemsBeforeBackup++
		}
	}
	b := st.call("POST", "/system/backups", nil, 202)
	id := str(b, "id")
	st.id("backup", id)
	var it obj
	st.eventually(90*time.Second, "backup finished", func() bool {
		it = find(st.items("/system/backups"), "id", id)
		return it != nil && str(it, "state") != "running"
	})
	if str(it, "state") != "succeeded" {
		st.fatalf("backup %+v", it)
	}
	st.call("POST", "/system/backups/"+id+"/verify", nil, 202)
	st.eventually(60*time.Second, "verification", func() bool { return !boolean(st.get("/system/backups"), "busy") })
	st.check("backup %s through the API while central ran: %d files, %d bytes (sealed); verified", id, num(it, "files"), num(it, "bytes"))
	dir := filepath.Join(s.dataDir, "backups", id)
	if _, err := os.Stat(filepath.Join(dir, "manifest.json")); err != nil {
		if m, _ := filepath.Glob(filepath.Join(s.dataDir, "backups", "*"+id+"*")); len(m) > 0 {
			dir = m[0]
		}
	}
	s.stopCentral(syscall.SIGTERM)
	t0 := time.Now()
	restored := filepath.Join(s.root, "central-restored")
	out := s.run(st.t, s.root, cleanEnv(), s.bin+"/spaceman", "-c", s.cfgPath, "backup", "restore", dir, "--data", restored)
	st.check("central stopped; `spaceman backup restore` into a new data directory: %s", strings.SplitN(strings.TrimSpace(out), "\n", 2)[0])
	s.dataDir = restored
	s.writeCentralConfig(st.t, waitFile(st.t, filepath.Join(s.pulseDir, "url")))
	s.startCentral(st.t)
	st.check("central was down %s for the restore (less than lease plus grace, %s) and came up on the restored data directory", time.Since(t0).Round(100*time.Millisecond), leaseTTL+grace)
	rs := st.get("/system/restore")
	if !boolean(rs, "restored") || str(rs, "state.backup_id") != id {
		st.fatalf("restore notice %+v", rs)
	}
	st.check("restore notice: backup %s, %d spaces paused, %d leases renewed, %d runs marked unknown, %d subscriptions reset, %d approvals revoked",
		id, arrLen(rs, "state.paused_spaces"), num(rs, "state.leases_renewed"), num(rs, "state.runs_unknown"),
		num(rs, "state.subscriptions_reset"), num(rs, "state.approvals_revoked"))
	for _, sp := range []string{d.pay, d.docs, d.ops} {
		if x := d.space(st, sp); str(x, "state") != "paused" {
			st.fatalf("space %s is %s after the restore", sp, str(x, "state"))
		}
	}
	st.check("every space came up paused (hold)")
	st.eventually(40*time.Second, "new lease after the reconnect", func() bool {
		x := d.space(st, d.ops)
		return boolean(x, "lease.valid") && num(x, "lease.generation") > d.opsGen
	})
	st.eventually(40*time.Second, "the run from before is stopped", func() bool { return ended(str(d.runOf(st, d.ops, run), "observed")) })
	r := d.runOf(st, d.ops, run)
	state, _ := d.a.hcTerminal(term)
	gen := num(d.space(st, d.ops), "lease.generation")
	st.check("ops lease generation %d → %d; the run %s from before the backup is fenced: desired %s, observed %s, terminal %s", d.opsGen, gen, run, str(r, "desired"), str(r, "observed"), state)
	if str(r, "desired") != "stopped" {
		st.fatalf("old run desired %s", str(r, "desired"))
	}
	if code, b := st.req("POST", "/spaces/"+d.ops+"/runs", map[string]any{"kind": "worker", "role": "implementor", "harness": "codex"}); code < 400 {
		st.fatalf("launch accepted in a space paused by the restore: %d %s", code, b)
	} else {
		st.check("a launch in the paused space is refused (%d)", code)
	}
	st.call("POST", "/system/restore/acknowledge", nil, 200)
	st.call("POST", "/spaces/"+d.ops+"/state", map[string]any{"state": "active"}, 200, ifMatch(d.space(st, d.ops))...)
	st.eventually(30*time.Second, "lease valid", func() bool { return boolean(d.space(st, d.ops), "lease.valid") })
	nr := d.launch(st, d.ops, map[string]any{"kind": "worker", "role": "implementor", "harness": "codex", "prompt": "After the restore."})
	if g := num(d.runOf(st, d.ops, nr), "generation"); g <= d.opsGen {
		st.fatalf("new run generation %d", g)
	}
	st.check("after the operator acknowledged the restore and resumed ops, a new run %s starts under the new generation", nr)
	d.stopRun(st, d.ops, nr)
}

// ---- step 27 ----

func (d *demo) duplicates(st *step) {
	count := func() int {
		n := 0
		for _, x := range d.inboxItems(st, d.pay) {
			if str(x, "subscription_id") == d.subPay {
				n++
			}
		}
		return n
	}
	st.eventually(30*time.Second, "subscription running after the restore", func() bool { return str(st.get("/spaces/"+d.pay+"/subscriptions/"+d.subPay), "state") == "running" })
	dups := 0
	deadline := time.Now().Add(15 * time.Second)
	for dups == 0 && time.Now().Before(deadline) {
		for _, x := range d.deliveries(st, d.pay, d.subPay) {
			if str(x, "event_id") == "demo-issue-101" && str(x, "outcome") == "duplicate" {
				dups++
			}
		}
		time.Sleep(time.Second)
	}
	if n := count(); n != d.itemsBeforeBackup {
		st.fatalf("inbox items from the subscription after the restore: %d (before the backup %d)", n, d.itemsBeforeBackup)
	}
	st.check("after the restore (its subscription reset): %d duplicate deliveries of demo-issue-101 recorded within 15 s, and still %d inbox item from the subscription", dups, d.itemsBeforeBackup)
	f := filepath.Join(d.s.root, "events", "issue-101.json")
	out := d.publish(st, "evt.github.example-org.webhook.issues.opened", "github.webhook.issues.opened.v1", "demo-issue-101", f)
	st.check("issue #101 published again with the same event ID: %s", out)
	sv := st.get("/spaces/" + d.pay + "/subscriptions/" + d.subPay)
	rp := st.call("POST", "/spaces/"+d.pay+"/subscriptions/"+d.subPay+"/replay", map[string]any{"policy": "all", "reason": "demo: duplicates must not duplicate items"}, 202, ifMatch(sv)...)
	st.eventually(40*time.Second, "replay handled", func() bool {
		for _, x := range d.deliveries(st, d.pay, d.subPay) {
			if str(x, "replay_id") == str(rp, "id") && str(x, "event_id") == "demo-issue-101" {
				return str(x, "outcome") == "duplicate"
			}
		}
		return false
	})
	if n := count(); n != d.itemsBeforeBackup || n != 1 {
		st.fatalf("inbox items from the subscription: %d (before the backup %d)", n, d.itemsBeforeBackup)
	}
	st.check("explicit replay %s: demo-issue-101 handled again with outcome duplicate; the space still has exactly %d inbox item from the subscription", str(rp, "id"), d.itemsBeforeBackup)
}

// ---- step 28 ----

func (d *demo) fanOut(st *step) {
	st.eventually(30*time.Second, "docs subscription running", func() bool { return str(st.get("/spaces/"+d.docs+"/subscriptions/"+d.subDocs), "state") == "running" })
	f := d.issueEvent(st, "example-org/payments-api", 102, "Refund API returns 500 for split payments")
	d.publish(st, "evt.github.example-org.webhook.issues.opened", "github.webhook.issues.opened.v1", "demo-issue-102", f)
	items := map[string]string{}
	for _, x := range [][2]string{{d.pay, d.subPay}, {d.docs, d.subDocs}} {
		var del obj
		st.eventually(40*time.Second, "event accepted in "+x[0], func() bool {
			del = find(d.deliveries(st, x[0], x[1]), "event_id", "demo-issue-102")
			return del != nil && str(del, "outcome") == "accepted"
		})
		it := st.get("/spaces/" + x[0] + "/inbox/" + str(del, "inbox_item_id"))
		if str(it, "space_id") != x[0] || str(it, "subscription_id") != x[1] {
			st.fatalf("item %+v", it)
		}
		items[x[0]] = str(it, "id")
	}
	if items[d.pay] == items[d.docs] {
		st.fatalf("one item for two spaces")
	}
	st.check("issue #102: payments got inbox item %s (subscription %s) and docs got %s (subscription %s), each through its own durable consumer", items[d.pay], d.subPay, items[d.docs], d.subDocs)
	for _, x := range [][2]string{{d.pay, d.subPay}, {d.docs, d.subDocs}} {
		accepted := false
		for _, del := range d.deliveries(st, x[0], x[1]) {
			accepted = accepted || (str(del, "event_id") == "demo-issue-101" && str(del, "outcome") == "accepted")
		}
		if !accepted {
			st.fatalf("issue #101 was never accepted in %s", x[0])
		}
	}
	st.check("issue #101 from step 4 also reached both spaces; both spaces are paused after the restore and still ingest durably")
}

// ---- step 29 ----

func (d *demo) screenshots(st *step) {
	s := d.s
	out := s.run(st.t, filepath.Join(s.repo, "site"), cleanEnv(), "node", "scripts/check-shots.mjs")
	st.check("site/scripts/check-shots.mjs: %s (every <Shot> on a page exists in both themes and is inventoried)", strings.TrimSpace(out))
	inv, err := os.ReadFile(filepath.Join(s.repo, "docs", "SCREENSHOTS.md"))
	if err != nil {
		st.fatalf("%v", err)
	}
	row := regexp.MustCompile("(?m)^\\| `([a-z0-9-]+)` \\|[^|]*\\|[^|]*\\| (\\w+)")
	current, todo := 0, []string{}
	for _, m := range row.FindAllStringSubmatch(string(inv), -1) {
		if m[2] != "current" {
			todo = append(todo, m[1]+" ("+m[2]+")")
			continue
		}
		current++
		for _, theme := range []string{"dark", "light"} {
			p := filepath.Join(s.repo, "site", "public", "screenshots", m[1]+"-"+theme+".png")
			f, err := os.Open(p)
			if err != nil {
				st.fatalf("%s: %v", m[1], err)
			}
			c, err := png.DecodeConfig(f)
			_ = f.Close()
			if err != nil || c.Width != 1440 || c.Height != 900 {
				st.fatalf("%s-%s.png: %v %dx%d", m[1], theme, err, c.Width, c.Height)
			}
		}
	}
	st.check("docs/SCREENSHOTS.md inventories %d current shots; each exists as a real 1440×900 PNG in both themes (captured from the real UI by make screenshots)", current)
	if len(todo) > 0 {
		st.note("inventory rows not current (not claimed): %s", strings.Join(todo, ", "))
	}
	shots, _ := filepath.Glob(filepath.Join(d.uiDir, "shots", "*.png"))
	st.check("this run's own browser captured %d screenshots of the real UI (kept with SM_KEEP=1, not committed)", len(shots))
}

// ---- step 30 ----

func (d *demo) mermaid(st *step) {
	s := d.s
	out := s.run(st.t, filepath.Join(s.repo, "site"), cleanEnv(), "pnpm", "check:mermaid")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	st.check("site check:mermaid (the real Mermaid parser in headless Chromium over docs/, site/ and README.md): %s", lines[len(lines)-1])
}
