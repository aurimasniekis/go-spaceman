//go:build real

// Package real holds end-to-end suites against real dependencies (SM-VER-002). This file runs the real `spaceman` and
// `spaceman-agent` binaries with a real, isolated herdr-core core. The harness binary is a stub shell script standing
// in for `claude` (real Claude Code / Codex runs need an account credential and are a separate, opt-in suite), so the
// evidence here is: real runner, real launcher shim, real lease guard, real recording, real process lifecycle.
package real

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/recording"
)

type env struct {
	t        *testing.T
	dir      string
	bin      string
	url      string
	root     string
	central  *exec.Cmd
	agent    *exec.Cmd
	agentCfg string
	hcHome   string
	session  string
	socket   string
}

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func build(t *testing.T, out string) {
	t.Helper()
	root, _ := filepath.Abs("../..")
	for _, c := range []string{"spaceman", "spaceman-agent"} {
		cmd := exec.Command("go", "build", "-o", filepath.Join(out, c), "./cmd/"+c)
		cmd.Dir = root
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", c, err, b)
		}
	}
}

func cleanEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		// Harness credentials and session markers from the developer's shell never reach the stack: runs get them only
		// from SpaceMan accounts.
		if strings.HasPrefix(kv, "CLAUDE") || strings.HasPrefix(kv, "ANTHROPIC_") || strings.HasPrefix(kv, "OPENAI_") || strings.HasPrefix(kv, "CODEX_") {
			continue
		}
		if !strings.HasPrefix(kv, "HC_") && !strings.HasPrefix(kv, "SPACEMAN_") && !strings.HasPrefix(kv, "XDG_") {
			out = append(out, kv)
		}
	}
	return out
}

func newEnv(t *testing.T) *env { return newEnvWith(t, "") }

// newEnvWith starts central and configures an agent whose Claude Code binary is claude ("" uses a stub script).
func newEnvWith(t *testing.T, claude string) *env { return newEnvHarnesses(t, claude, "") }

// newEnvHarnesses is newEnvWith with a Codex binary too ("" uses the stub script for either).
func newEnvHarnesses(t *testing.T, claude, codex string) *env {
	t.Helper()
	if _, err := exec.LookPath("hc"); err != nil {
		t.Skip("hc not installed")
	}
	dir, err := os.MkdirTemp("/tmp", "smreal-") // short paths: unix socket limit (S-0)
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, dir: dir, bin: filepath.Join(dir, "bin")}
	build(t, e.bin)
	port, mport := freePort(t), freePort(t)
	e.url = fmt.Sprintf("http://127.0.0.1:%d", port)
	central := filepath.Join(dir, "central.yaml")
	must(t, os.WriteFile(central, []byte(fmt.Sprintf(`mode: development
data_dir: %s/central
public_url: %s
kek: {file: %s/kek}
http: {listen: "127.0.0.1:%d", management_listen: "127.0.0.1:%d"}
nodes: {heartbeat_interval: 1s, lease_ttl: 3s, disconnect_grace: 2s}
log: {level: debug, format: text}
`, dir, e.url, dir, port, mport)), 0o600))
	run(t, e.bin+"/spaceman", "keys", "init-kek", "--out", dir+"/kek")
	out := run(t, e.bin+"/spaceman", "-c", central, "admin", "bootstrap")
	for _, f := range strings.Fields(out) {
		if strings.HasPrefix(f, "spaceman_rt_") {
			e.root = f
		}
	}
	e.central = start(t, filepath.Join(dir, "central.log"), e.bin+"/spaceman", "-c", central, "serve")
	waitHTTP(t, e.url+"/api/v1/system/info")

	// Stub harness: prints, reports nothing, runs until killed.
	stub := filepath.Join(dir, "stub-claude.sh")
	must(t, os.WriteFile(stub, []byte("#!/bin/sh\nif [ \"$1\" = --version ]; then echo '0.0.0 (stub)'; exit 0; fi\necho stub-harness-started \"$@\"\nif [ -n \"$CLAUDE_CODE_OAUTH_TOKEN\" ]; then echo has-token; fi\n"+
		"env | grep -c SPACEMAN_RUN_TOKEN\ni=0; while true; do echo tick-$i; i=$((i+1)); sleep 0.5; done\n"), 0o700))
	claudeBin, codexBin := stub, stub
	if claude != "" {
		claudeBin = claude
	}
	if codex != "" {
		codexBin = codex
	}
	e.session = "smr" + filepath.Base(dir)[7:]
	e.socket = filepath.Join(dir, "hc.sock")
	e.hcHome = filepath.Join(dir, "agent", "hc")
	e.agentCfg = filepath.Join(dir, "agent.yaml")
	must(t, os.WriteFile(e.agentCfg, []byte(fmt.Sprintf(`mode: development
data_dir: %s/agent
central: {url: %s, credential_file: %s/agent/node.credential, reconnect_max: 1s}
runner: {session: %s, socket: %s}
harness: {claude_binary: %s, codex_binary: %s}
log: {level: debug, format: text}
`, dir, e.url, dir, e.session, e.socket, claudeBin, codexBin)), 0o600))
	t.Cleanup(func() {
		e.stopAgent(syscall.SIGTERM)
		if e.central != nil && e.central.Process != nil {
			_ = e.central.Process.Signal(syscall.SIGTERM)
			_ = e.central.Wait()
		}
		// Stop only the isolated core this test started, then remove everything it created.
		cmd := exec.Command("hc", "server", "stop")
		cmd.Env = append(cleanEnv(), "HC_SESSION="+e.session, "HC_SOCKET="+e.socket, "XDG_STATE_HOME="+e.hcHome+"/state", "XDG_CONFIG_HOME="+e.hcHome+"/config")
		_ = cmd.Run()
		if t.Failed() {
			for _, f := range []string{"central.log", "agent.log"} {
				b, _ := os.ReadFile(filepath.Join(dir, f))
				t.Logf("---- %s ----\n%s", f, tail(string(b), 4000))
			}
		}
		if os.Getenv("SM_KEEP") == "" {
			_ = os.RemoveAll(dir)
		} else {
			t.Logf("kept %s", dir)
		}
	})
	return e
}

func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func run(t *testing.T, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Env = cleanEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
	return string(out)
}

func start(t *testing.T, logFile, name string, args ...string) *exec.Cmd {
	t.Helper()
	f, err := os.Create(logFile)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(name, args...)
	cmd.Env = cleanEnv()
	cmd.Stdout, cmd.Stderr = f, f
	must(t, cmd.Start())
	return cmd
}

func waitHTTP(t *testing.T, url string) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if res, err := http.Get(url); err == nil {
			res.Body.Close()
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("%s did not come up", url)
}

func (e *env) api(method, path string, body any, out any) int {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.url+"/api/v1"+path, rd)
	req.Header.Set("Authorization", "Bearer "+e.root)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if out != nil {
		_ = json.Unmarshal(b, out)
	}
	if res.StatusCode >= 300 && out == nil {
		e.t.Logf("%s %s: %d %s", method, path, res.StatusCode, b)
	}
	return res.StatusCode
}

func (e *env) startAgent() {
	e.agent = start(e.t, filepath.Join(e.dir, "agent.log"), e.bin+"/spaceman-agent", "-c", e.agentCfg, "run")
}

func (e *env) stopAgent(sig syscall.Signal) {
	if e.agent != nil && e.agent.Process != nil {
		_ = e.agent.Process.Signal(sig)
		_, _ = e.agent.Process.Wait()
		e.agent = nil
	}
}

func eventually(t *testing.T, d time.Duration, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out: %s", what)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// hcTerminals counts live terminals in the test's own core carrying a label (`hc ls --label K=V --json`).
func (e *env) hcTerminals(t *testing.T, label string) int {
	t.Helper()
	cmd := exec.Command("hc", "ls", "--label", label, "--json")
	cmd.Env = append(cleanEnv(), "HC_SESSION="+e.session, "HC_SOCKET="+e.socket, "XDG_STATE_HOME="+e.hcHome+"/state", "XDG_CONFIG_HOME="+e.hcHome+"/config")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("hc ls: %v", err)
	}
	var list []json.RawMessage
	if json.Unmarshal(out, &list) == nil {
		return len(list)
	}
	var obj struct {
		Terminals []json.RawMessage `json:"terminals"`
	}
	if err := json.Unmarshal(out, &obj); err != nil || obj.Terminals == nil {
		t.Fatalf("unexpected hc ls output: %s", out)
	}
	return len(obj.Terminals)
}

type run1 struct {
	ID, Observed, Desired, TerminalID string
	Adopted                           bool
}

func (e *env) getRun(space, id string) run1 {
	var r struct {
		ID         string `json:"id"`
		Observed   string `json:"observed"`
		Desired    string `json:"desired"`
		TerminalID string `json:"terminal_id"`
		Adopted    bool   `json:"adopted"`
	}
	e.api("GET", "/spaces/"+space+"/runs/"+id, nil, &r)
	return run1{r.ID, r.Observed, r.Desired, r.TerminalID, r.Adopted}
}

// TestRealNodeEndToEnd: enroll → connect → pinned space → launch through the shim under a lease guard → recording →
// agent restart adopts (no duplicate process) → SIGKILLed agent: the lease guard ends the run after lease + grace
// (SM-NODE-004/007/008/009, SM-REC-002, SM-ACCT-004).
// launchRun enrolls a node, starts the agent, creates a pinned space (disconnect policy kill) and launches one stub run,
// waiting until it runs. It returns the node, space and run IDs.
func (e *env) launchRun(t *testing.T) (nodeID, spaceID, runID string) {
	t.Helper()
	nodeID, spaceID = e.setupSpace(t, "stub-token") // the stub harness ignores the credential
	runID = e.launch(t, spaceID, map[string]any{"kind": "worker", "role": "implementor-backend", "harness": "claude-code", "prompt": "hello"})
	return nodeID, spaceID, runID
}

// setupSpace enrolls a node, starts the agent, creates a pinned space (disconnect policy kill) and binds a Claude
// account holding token as its default (SM-ACCT-003). It returns the node and space IDs once the lease is granted.
func (e *env) setupSpace(t *testing.T, token string) (nodeID, spaceID string) {
	return e.setupSpaceWith(t, "claude_oauth_token", token)
}

// setupSpaceWith is setupSpace with an account of another kind (for example openai_api_key or codex_chatgpt_login).
func (e *env) setupSpaceWith(t *testing.T, kind, secret string) (nodeID, spaceID string) {
	t.Helper()
	var en struct {
		Node  struct{ ID string } `json:"node"`
		Token string              `json:"token"`
	}
	if st := e.api("POST", "/nodes", map[string]any{"name": "real-node"}, &en); st != 201 {
		t.Fatalf("create node: %d", st)
	}
	run(t, e.bin+"/spaceman-agent", "-c", e.agentCfg, "enroll", "--token", en.Token)
	if fi, err := os.Stat(filepath.Join(e.dir, "agent", "node.credential")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("credential file %v %v", fi, err)
	}
	e.startAgent()
	eventually(t, 20*time.Second, "node connected", func() bool {
		var n struct{ Connected bool }
		e.api("GET", "/nodes/"+en.Node.ID, nil, &n)
		return n.Connected
	})
	var sp struct{ ID string }
	if st := e.api("POST", "/spaces", map[string]any{"slug": "real-space", "name": "Real", "node_id": en.Node.ID, "disconnect_policy": "kill"}, &sp); st != 201 {
		t.Fatalf("create space: %d", st)
	}
	var acc struct{ ID string }
	if st := e.api("POST", "/accounts", map[string]any{"name": "real-" + strings.ReplaceAll(kind, "_", "-"), "kind": kind, "secret": secret}, &acc); st != 201 {
		t.Fatalf("create account: %d", st)
	}
	if st := e.api("PUT", "/spaces/"+sp.ID+"/accounts", map[string]any{"items": []map[string]any{{"account_id": acc.ID, "default": true}}}, nil); st != 200 {
		t.Fatalf("bind account: %d", st)
	}
	time.Sleep(1500 * time.Millisecond) // lease grant on the next heartbeat
	return en.Node.ID, sp.ID
}

// launch starts a run and waits until it runs.
func (e *env) launch(t *testing.T, spaceID string, body map[string]any) string {
	t.Helper()
	var r struct{ ID string }
	if st := e.api("POST", "/spaces/"+spaceID+"/runs", body, &r); st != 201 {
		t.Fatalf("launch: %d", st)
	}
	eventually(t, 20*time.Second, "run running", func() bool { return e.getRun(spaceID, r.ID).Observed == "running" })
	return r.ID
}

// hcState returns "<state>/<reason>" of a terminal in the test's own core.
func (e *env) hcState(id string) string {
	cmd := exec.Command("hc", "get", id, "--json")
	cmd.Env = append(cleanEnv(), "HC_SESSION="+e.session, "HC_SOCKET="+e.socket, "XDG_STATE_HOME="+e.hcHome+"/state", "XDG_CONFIG_HOME="+e.hcHome+"/config")
	out, err := cmd.Output()
	if err != nil {
		return "error"
	}
	var term struct {
		State  string `json:"state"`
		Reason string `json:"reason"`
	}
	_ = json.Unmarshal(out, &term)
	return term.State + "/" + term.Reason
}

func TestRealNodeEndToEnd(t *testing.T) {
	e := newEnv(t)
	nodeID, spaceID, runID := e.launchRun(t)

	// The secret file was consumed by the shim; the run token reached the harness env only.
	runDir := filepath.Join(e.dir, "agent", "runs", runID)
	if _, err := os.Stat(filepath.Join(runDir, "secrets.json")); !os.IsNotExist(err) {
		t.Fatalf("secret file not unlinked by the launcher shim: %v", err)
	}
	terms, _ := os.ReadFile(filepath.Join(e.hcHome, "state", "herdr-core", e.session, "terminals.json"))
	if strings.Contains(string(terms), "spaceman_rn_") {
		t.Fatal("run token persisted in herdr-core state")
	}
	eventually(t, 10*time.Second, "recording has output", func() bool {
		res, err := recording.Read(filepath.Join(runDir, "recording"), 0)
		if err != nil {
			return false
		}
		var out strings.Builder
		for _, rec := range res.Records {
			out.Write(rec.Data)
		}
		return strings.Contains(out.String(), "stub-harness-started") && strings.Contains(out.String(), "tick-1")
	})

	// Agent restart (SIGTERM): the run keeps going and is adopted, not relaunched.
	before := e.getRun(spaceID, runID).TerminalID
	e.stopAgent(syscall.SIGTERM)
	e.startAgent()
	eventually(t, 20*time.Second, "run running after agent restart", func() bool {
		x := e.getRun(spaceID, runID)
		return x.Observed == "running" && x.TerminalID == before
	})

	// Central restart (SIGTERM, then serve again on the same data): the agent reconnects, central reconciles, and the
	// run continues on the same terminal; herdr-core holds exactly one terminal for it (no duplicate execution).
	_ = e.central.Process.Signal(syscall.SIGTERM)
	_ = e.central.Wait()
	e.central = start(t, filepath.Join(e.dir, "central-2.log"), e.bin+"/spaceman", "-c", filepath.Join(e.dir, "central.yaml"), "serve")
	waitHTTP(t, e.url+"/api/v1/system/info")
	eventually(t, 20*time.Second, "node reconnected after central restart", func() bool {
		var n struct{ Connected bool }
		e.api("GET", "/nodes/"+nodeID, nil, &n)
		return n.Connected
	})
	eventually(t, 20*time.Second, "run running after central restart", func() bool {
		x := e.getRun(spaceID, runID)
		return x.Observed == "running" && x.TerminalID == before
	})
	time.Sleep(3 * time.Second) // heartbeats, reconcile, command re-delivery
	if n := e.hcTerminals(t, "spaceman.run="+runID); n != 1 {
		t.Fatalf("herdr-core terminals for the run after restarts = %d, want 1", n)
	}

	// Dead daemon: SIGKILL the agent. The lease guard (a separate process inside herdr-core) ends the run once the
	// lease file expires (lease 3s + grace 2s + guard slack 10s), with no daemon involved.
	e.stopAgent(syscall.SIGKILL)
	state := func() string { return e.hcState(before) }
	eventually(t, 40*time.Second, "lease guard ended the run of a dead agent", func() bool { return state() == "finished/killed" })
	t.Logf("run terminal after dead-daemon enforcement: %s", state())
}

// TestRealDisconnectEnforcement (SM-NODE-007): central goes away while the agent stays up. The agent starts nothing new
// and, for a space with disconnect policy "kill", ends its run after lease + grace. When central returns, the run is
// reported ended and is not relaunched.
func TestRealDisconnectEnforcement(t *testing.T) {
	e := newEnv(t)
	_, spaceID, runID := e.launchRun(t)
	term := e.getRun(spaceID, runID).TerminalID
	_ = e.central.Process.Signal(syscall.SIGTERM)
	_ = e.central.Wait()
	// Lease 3s + grace 2s, enforced by the live agent (not the lease guard, whose slack is longer).
	eventually(t, 20*time.Second, "agent ended the run without central", func() bool { return strings.HasPrefix(e.hcState(term), "finished/") })
	t.Logf("run terminal after disconnect enforcement: %s", e.hcState(term))
	e.central = start(t, filepath.Join(e.dir, "central-2.log"), e.bin+"/spaceman", "-c", filepath.Join(e.dir, "central.yaml"), "serve")
	waitHTTP(t, e.url+"/api/v1/system/info")
	eventually(t, 20*time.Second, "central learns the run ended", func() bool {
		o := e.getRun(spaceID, runID).Observed
		return o == "exited" || o == "lost"
	})
	time.Sleep(3 * time.Second)
	if n := e.hcTerminals(t, "spaceman.run="+runID); n != 0 {
		t.Fatalf("live terminals for the run after reconnect = %d, want 0 (no relaunch)", n)
	}
}

var _ = bufio.NewReader
