//go:build demo

package demo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// stack is the demonstration's installation: a real `spaceman serve` (with the embedded UI), two real `spaceman-agent`
// processes each with its own herdr-core core, devpulse (a Pulse-shaped NATS server) and devmcp (a fake ticket tracker
// behind the MCP gateway). Everything lives under one short /tmp directory; nothing is shared with the developer's
// environment or the default herdr-core core.
type stack struct {
	repo      string // repository root
	root      string // /tmp/smdemo-XXXXXX
	bin       string
	logs      string
	url       string
	port      int
	mport     int
	pulsePort int
	mcpPort   int
	pulseDir  string
	mcpURL    string
	cfgPath   string
	dataDir   string // central's current data directory (a restore moves it)
	rootToken string
	tokenFile string
	central   *exec.Cmd
	pulse     *exec.Cmd
	mcp       *exec.Cmd
	agents    map[string]*agentProc
	procs     []*exec.Cmd
	logN      int

	// Real mode: a real Claude Code binary and its credential (held only in memory and in the SpaceMan account).
	real      bool
	claudeBin string
	token     string
}

// agentProc is one node agent and its isolated herdr-core core.
type agentProc struct {
	name, node, dir, cfg, session, socket, hcHome string
	cmd                                           *exec.Cmd
	nodeID                                        string
}

// Timings of central's node configuration: short so the disconnect step does not take long, long enough that a
// central or agent restart stays well inside lease plus grace.
const (
	heartbeat = time.Second
	leaseTTL  = 8 * time.Second
	grace     = 6 * time.Second
)

func freePort(tb testing.TB) int {
	tb.Helper()
	l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// cleanEnv is the environment of every process the demo starts: nothing from a surrounding herdr-core terminal,
// SpaceMan configuration, XDG directories or harness credentials of the developer's shell. Runs get credentials only
// from SpaceMan accounts.
func cleanEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch {
		case strings.HasPrefix(k, "HC_"), strings.HasPrefix(k, "SPACEMAN_"), strings.HasPrefix(k, "XDG_"), strings.HasPrefix(k, "SM_"),
			strings.HasPrefix(k, "CLAUDE"), strings.HasPrefix(k, "ANTHROPIC_"), strings.HasPrefix(k, "OPENAI_"), strings.HasPrefix(k, "CODEX_"):
			continue
		}
		out = append(out, kv)
	}
	return out
}

// gitEnv makes the driver's own Git commands independent of the developer's Git configuration (signing, hooks).
func gitEnv() []string {
	return append(cleanEnv(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=Alex Morgan", "GIT_AUTHOR_EMAIL=alex@example.com",
		"GIT_COMMITTER_NAME=Alex Morgan", "GIT_COMMITTER_EMAIL=alex@example.com")
}

func newStack(tb testing.TB) *stack {
	tb.Helper()
	repo, err := filepath.Abs("../..")
	if err != nil {
		tb.Fatal(err)
	}
	root, err := os.MkdirTemp("/tmp", "smdemo-") // short: unix socket paths are limited to 103 bytes
	if err != nil {
		tb.Fatal(err)
	}
	s := &stack{repo: repo, root: root, bin: filepath.Join(root, "bin"), logs: filepath.Join(root, "logs"), agents: map[string]*agentProc{}}
	for _, d := range []string{s.bin, s.logs} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			tb.Fatal(err)
		}
	}
	return s
}

func (s *stack) run(tb testing.TB, dir string, env []string, name string, args ...string) string {
	tb.Helper()
	cmd := exec.CommandContext(context.Background(), name, args...) //nolint:gosec // the demo runs the programs it built
	cmd.Dir = dir
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		tb.Fatalf("%s %s: %v\n%s", filepath.Base(name), strings.Join(args, " "), err, tail(string(out), 3000))
	}
	return string(out)
}

// build compiles the UI (unless SM_DEMO_SKIP_UI_BUILD=1 and web/dist exists), central with the embedded UI, the agent
// and the development tools into the demo's own bin directory.
func (s *stack) build(tb testing.TB) {
	tb.Helper()
	if _, err := os.Stat(filepath.Join(s.repo, "web", "dist", "index.html")); os.Getenv("SM_DEMO_SKIP_UI_BUILD") != "1" || err != nil {
		s.run(tb, filepath.Join(s.repo, "web"), cleanEnv(), "pnpm", "run", "build")
	}
	s.run(tb, s.repo, cleanEnv(), "go", "build", "-tags", "embedui", "-o", s.bin+"/", "./cmd/spaceman", "./cmd/spaceman-agent")
	s.run(tb, s.repo, cleanEnv(), "go", "build", "-o", s.bin+"/", "./internal/events/pulsetest/devpulse", "./internal/gateway/gatewaytest/devmcp",
		"./internal/harness/harnesstest/stubharness")
}

// start runs a long-lived process with its output in logs/<name>-N.log.
func (s *stack) start(tb testing.TB, name string, env []string, prog string, args ...string) *exec.Cmd {
	tb.Helper()
	s.logN++
	f, err := os.OpenFile(filepath.Join(s.logs, fmt.Sprintf("%s-%d.log", name, s.logN)), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		tb.Fatal(err)
	}
	cmd := exec.CommandContext(context.Background(), prog, args...) //nolint:gosec // the demo runs the programs it built
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = f, f
	if err := cmd.Start(); err != nil {
		tb.Fatal(err)
	}
	s.procs = append(s.procs, cmd)
	go func() { _ = cmd.Wait(); _ = f.Close() }()
	return cmd
}

// stop signals a process and waits (up to 20 s, then SIGKILL) until it is gone.
func stop(cmd *exec.Cmd, sig syscall.Signal) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(sig)
	for i := 0; i < 200; i++ {
		if cmd.Process.Signal(syscall.Signal(0)) != nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = cmd.Process.Kill()
	time.Sleep(200 * time.Millisecond)
}

func waitFile(tb testing.TB, path string) string {
	tb.Helper()
	for i := 0; i < 200; i++ {
		if b, err := os.ReadFile(path); err == nil && len(bytes.TrimSpace(b)) > 0 {
			return strings.TrimSpace(string(b))
		}
		time.Sleep(100 * time.Millisecond)
	}
	tb.Fatalf("%s did not appear", path)
	return ""
}

func (s *stack) waitHTTP(tb testing.TB) {
	tb.Helper()
	for i := 0; i < 300; i++ {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, s.url+"/api/v1/system/info", nil)
		if res, err := http.DefaultClient.Do(req); err == nil {
			_ = res.Body.Close()
			if res.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	tb.Fatal("central did not come up")
}

// up starts devpulse, devmcp and central (bootstrapped, with a root token) on free loopback ports.
func (s *stack) up(tb testing.TB) {
	tb.Helper()
	s.port, s.mport, s.pulsePort, s.mcpPort = freePort(tb), freePort(tb), freePort(tb), freePort(tb)
	s.url = fmt.Sprintf("http://127.0.0.1:%d", s.port)
	s.pulseDir = filepath.Join(s.root, "pulse")
	if err := os.MkdirAll(s.pulseDir, 0o700); err != nil {
		tb.Fatal(err)
	}
	s.pulse = s.start(tb, "devpulse", cleanEnv(), s.bin+"/devpulse", "serve", "-dir", s.pulseDir, "-port", strconv.Itoa(s.pulsePort))
	s.mcp = s.start(tb, "devmcp", cleanEnv(), s.bin+"/devmcp", "-port", strconv.Itoa(s.mcpPort), "-token", "demo-mcp-token", "-url-file", filepath.Join(s.root, "mcp-url"))
	pulseURL := waitFile(tb, filepath.Join(s.pulseDir, "url"))
	waitFile(tb, filepath.Join(s.pulseDir, "spaceman.creds"))
	s.mcpURL = waitFile(tb, filepath.Join(s.root, "mcp-url"))
	s.dataDir = filepath.Join(s.root, "central")
	s.cfgPath = filepath.Join(s.root, "central.yaml")
	s.writeCentralConfig(tb, pulseURL)
	s.run(tb, s.root, cleanEnv(), s.bin+"/spaceman", "keys", "init-kek", "--out", filepath.Join(s.root, "kek"))
	out := s.run(tb, s.root, cleanEnv(), s.bin+"/spaceman", "-c", s.cfgPath, "admin", "bootstrap")
	for _, f := range strings.Fields(out) {
		if strings.HasPrefix(f, "spaceman_rt_") {
			s.rootToken = f
		}
	}
	if s.rootToken == "" {
		tb.Fatal("bootstrap printed no root token")
	}
	s.tokenFile = filepath.Join(s.root, "root-token")
	if err := os.WriteFile(s.tokenFile, []byte(s.rootToken+"\n"), 0o600); err != nil {
		tb.Fatal(err)
	}
	s.startCentral(tb)
}

func (s *stack) writeCentralConfig(tb testing.TB, pulseURL string) {
	tb.Helper()
	cfg := fmt.Sprintf(`mode: development
data_dir: %s
public_url: %s
kek: {file: %s/kek}
http: {listen: "127.0.0.1:%d", management_listen: "127.0.0.1:%d"}
nodes: {heartbeat_interval: %s, lease_ttl: %s, disconnect_grace: %s}
log: {level: info, format: text}
events: {outbound: {enabled: true, url: "%s", creds_file: %s/spaceman.creds, source: spaceman-demo}}
`, s.dataDir, s.url, s.root, s.port, s.mport, heartbeat, leaseTTL, grace, pulseURL, s.pulseDir)
	if err := os.WriteFile(s.cfgPath, []byte(cfg), 0o600); err != nil {
		tb.Fatal(err)
	}
}

func (s *stack) startCentral(tb testing.TB) {
	tb.Helper()
	s.central = s.start(tb, "central", cleanEnv(), s.bin+"/spaceman", "-c", s.cfgPath, "serve")
	s.waitHTTP(tb)
}

func (s *stack) stopCentral(sig syscall.Signal) {
	stop(s.central, sig)
	s.central = nil
}

// addAgent creates a node through the API, writes the agent's configuration (its own data directory, herdr-core
// session and socket) and enrolls it with the one-time token.
func (s *stack) addAgent(tb testing.TB, st *step, name, node string) *agentProc {
	tb.Helper()
	dir := filepath.Join(s.root, name)
	// The agent's MCP bridge socket sits next to the runner socket, so each agent's sockets get a folder of their own.
	a := &agentProc{name: name, node: node, dir: dir, cfg: filepath.Join(s.root, name+".yaml"), socket: filepath.Join(dir, "hc.sock"),
		hcHome: filepath.Join(dir, "hc"), session: "smd" + strings.ToLower(strings.TrimPrefix(filepath.Base(s.root), "smdemo-")) + name}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		tb.Fatal(err)
	}
	claude, codex := s.bin+"/stubharness", s.bin+"/stubharness"
	if s.real {
		claude = s.claudeBin
	}
	cfg := fmt.Sprintf(`mode: development
data_dir: %s
central: {url: %s, credential_file: %s/node.credential, reconnect_max: 1s}
runner: {session: %s, socket: %s}
harness: {claude_binary: %s, codex_binary: %s}
capacity: 16
log: {level: info, format: text}
`, dir, s.url, dir, a.session, a.socket, claude, codex)
	if err := os.WriteFile(a.cfg, []byte(cfg), 0o600); err != nil {
		tb.Fatal(err)
	}
	en := st.call("POST", "/nodes", map[string]any{"name": node, "description": "Demo node " + strings.ToUpper(name[len(name)-1:])}, 201)
	a.nodeID = str(en, "node.id")
	s.run(tb, s.root, cleanEnv(), s.bin+"/spaceman-agent", "-c", a.cfg, "enroll", "--token", str(en, "token"))
	s.agents[name] = a
	s.startAgent(tb, a)
	st.eventually(30*time.Second, node+" connected", func() bool { return boolean(st.get("/nodes/"+a.nodeID), "connected") })
	return a
}

func (s *stack) startAgent(tb testing.TB, a *agentProc) {
	tb.Helper()
	a.cmd = s.start(tb, a.name, cleanEnv(), s.bin+"/spaceman-agent", "-c", a.cfg, "run")
}

// hcEnv addresses an agent's own herdr-core core, never the default one.
func (a *agentProc) hcEnv() []string {
	return append(cleanEnv(), "HC_SESSION="+a.session, "HC_SOCKET="+a.socket, "XDG_STATE_HOME="+a.hcHome+"/state", "XDG_CONFIG_HOME="+a.hcHome+"/config",
		"XDG_RUNTIME_DIR="+a.hcHome+"/runtime")
}

// hcTerminals counts the live terminals of a run in the agent's core.
func (a *agentProc) hcTerminals(tb testing.TB, runID string) int {
	tb.Helper()
	cmd := exec.CommandContext(context.Background(), "hc", "ls", "--label", "spaceman.run="+runID, "--json")
	cmd.Env = a.hcEnv()
	out, err := cmd.Output()
	if err != nil {
		tb.Fatalf("hc ls: %v", err)
	}
	var list []json.RawMessage
	if json.Unmarshal(out, &list) == nil {
		return len(list)
	}
	var o struct {
		Terminals []json.RawMessage `json:"terminals"`
	}
	if err := json.Unmarshal(out, &o); err != nil {
		tb.Fatalf("hc ls output: %s", out)
	}
	return len(o.Terminals)
}

// hcTerminal reads a terminal of the agent's core ("state/reason" and its working directory).
func (a *agentProc) hcTerminal(id string) (state, cwd string) {
	cmd := exec.CommandContext(context.Background(), "hc", "get", id, "--json")
	cmd.Env = a.hcEnv()
	out, err := cmd.Output()
	if err != nil {
		return "error", ""
	}
	var t struct {
		State  string `json:"state"`
		Reason string `json:"reason"`
		Cwd    string `json:"cwd"`
	}
	_ = json.Unmarshal(out, &t)
	return t.State + "/" + t.Reason, t.Cwd
}

// down stops everything the demo started (agents, central, devpulse, devmcp, the agents' own herdr-core cores) and
// reports any process still running from the demo's directory. It removes the directory unless SM_KEEP=1.
func (s *stack) down(tb testing.TB) []string {
	for _, a := range s.agents {
		stop(a.cmd, syscall.SIGTERM)
	}
	stop(s.central, syscall.SIGTERM)
	stop(s.pulse, syscall.SIGTERM)
	stop(s.mcp, syscall.SIGTERM)
	for _, c := range s.procs {
		stop(c, syscall.SIGTERM)
	}
	for _, a := range s.agents {
		cmd := exec.CommandContext(context.Background(), "hc", "server", "stop")
		cmd.Env = a.hcEnv()
		_ = cmd.Run()
	}
	// Anything left that was started from the demo's directory (its binaries, sockets and data are all under it).
	var left []string
	for i := 0; i < 50; i++ {
		left = leftovers(s.root)
		if len(left) == 0 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if len(left) > 0 {
		for _, l := range left {
			if pid, err := strconv.Atoi(strings.Fields(l)[0]); err == nil {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	if os.Getenv("SM_KEEP") == "1" {
		tb.Logf("kept %s", s.root)
	} else if strings.HasPrefix(s.root, "/tmp/smdemo-") {
		_ = os.RemoveAll(s.root)
	}
	return left
}

// leftovers lists processes started from dir ("pid command"): their command line mentions it, or, for herdr-core's
// own processes (whose command line does not), they hold files under it open.
func leftovers(dir string) []string {
	out, err := exec.CommandContext(context.Background(), "ps", "-axo", "pid=,command=").Output()
	if err != nil {
		return nil
	}
	var l []string
	self := strconv.Itoa(os.Getpid())
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, self+" ") || strings.Contains(line, "ps -axo") {
			continue
		}
		if strings.Contains(line, dir) {
			l = append(l, line)
			continue
		}
		f := strings.Fields(line)
		if len(f) >= 2 && filepath.Base(f[1]) == "hc" {
			if files, err := exec.CommandContext(context.Background(), "lsof", "-p", f[0]).Output(); err == nil && bytes.Contains(files, []byte(dir)) {
				l = append(l, line)
			}
		}
	}
	return l
}

func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

// api is one request to central's API with the root token.
func (s *stack) api(method, path string, body any, hdr ...string) (int, []byte, http.Header, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, nil, err
		}
		rd = bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, s.url+"/api/v1"+path, rd)
	if err != nil {
		return 0, nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+s.rootToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	return res.StatusCode, b, res.Header, err
}
