//go:build real

package real

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/agent"
	"github.com/aurimasniekis/go-spaceman/internal/harness"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// TestRealClaudeStatusLineWrapper (plan §3.3): with the settings SpaceMan writes for a run, the real Claude Code CLI runs
// the status-line command (SpaceMan's wrapper) with the documented JSON on stdin; the wrapper hands the allowlisted
// subset to the node agent's socket with the run token, and runs the profile's own status-line command with the same
// input. Print mode never runs the status line, so Claude Code runs interactively under a pseudo-terminal (script(1)) for
// a few seconds; it needs no model and no credentials (the status line runs at session start; a throwaway HOME and
// config dir, an API address nothing listens on). A local socket stands in for the node agent.
// Opt-in: SPACEMAN_REAL=1 with claude, go and script on PATH.
func TestRealClaudeStatusLineWrapper(t *testing.T) {
	if os.Getenv("SPACEMAN_REAL") != "1" {
		t.Skip("set SPACEMAN_REAL=1 (with claude and go on PATH) to check the status-line wrapper against the real Claude Code CLI")
	}
	claude, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude not installed")
	}
	script, err := exec.LookPath("script")
	if err != nil {
		t.Skip("script(1) not installed")
	}
	root, _ := filepath.EvalSymlinks(t.TempDir())
	home, cfg, work, bin := filepath.Join(root, "home"), filepath.Join(root, "cfg"), filepath.Join(root, "work"), filepath.Join(root, "bin")
	for _, d := range []string{home, work, bin, filepath.Join(cfg, "claude")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	agentBin := filepath.Join(bin, "spaceman-agent")
	build := exec.Command("go", "build", "-o", agentBin, "github.com/aurimasniekis/go-spaceman/cmd/spaceman-agent") //nolint:gosec // fixed arguments
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	// The node agent's stand-in: records what the wrapper sends.
	sock := filepath.Join(root, "b.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var mu sync.Mutex
	var calls []map[string]any
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				line, err := bufio.NewReader(c).ReadBytes('\n')
				var req map[string]any
				if err == nil && json.Unmarshal(line, &req) == nil {
					mu.Lock()
					calls = append(calls, req)
					mu.Unlock()
				}
				_, _ = c.Write([]byte(`{"ok":true}` + "\n"))
			}()
		}
	}()

	// The profile's own status line records the JSON it gets and prints a marker.
	seen := filepath.Join(root, "profile-stdin.json")
	l, err := harness.Claude{}.Build(harness.Context{AgentBinary: agentBin, BridgeSocket: sock, ConfigDir: cfg, SpaceDir: root,
		WorkDir: work, RunID: "run_1", SpaceID: "spc_1", RunDir: root, Binary: claude,
		Payload: proto.LaunchPayload{Kind: "worker", ClaudeSettings: proto.ClaudeSettings{
			"statusLine": map[string]any{"type": "command", "command": "cat > '" + seen + "'; printf 'profile-line'"},
		}}})
	if err != nil {
		t.Fatal(err)
	}
	for p, keys := range l.OwnJSON {
		if err := agent.OwnJSONKeysFile(nil, p, keys, l.StripJSON[p]...); err != nil {
			t.Fatal(err)
		}
	}
	for p, patch := range l.MergeJSON {
		if err := agent.MergeJSONFile(p, patch); err != nil {
			t.Fatal(err)
		}
	}
	// Interactive Claude Code on a pseudo-terminal, stdin held open for a while, then ended.
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, script, "-qfc", "'"+claude+"'", "/dev/null") //nolint:gosec // the test's own CLI
	cmd.Dir = work
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "CLAUDE_CONFIG_DIR=" + filepath.Join(cfg, "claude"),
		"ANTHROPIC_BASE_URL=http://127.0.0.1:9", "ANTHROPIC_AUTH_TOKEN=dummy", "DISABLE_AUTOUPDATER=1", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
		"SPACEMAN_RUN_TOKEN=real-test-token", "TERM=xterm-256color"}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); _ = cmd.Wait() }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		mu.Lock()
		n := len(calls)
		mu.Unlock()
		_, statErr := os.Stat(seen)
		if (n > 0 && statErr == nil) || time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	b, err := os.ReadFile(seen)
	if err != nil {
		t.Fatalf("Claude Code did not run SpaceMan's status line, or the wrapper did not run the profile's command: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("status line input is not JSON: %v\n%s", err, b)
	}
	if sid, _ := doc["session_id"].(string); sid == "" {
		t.Errorf("no session_id in %s", b)
	}
	if m, _ := doc["model"].(map[string]any); m == nil || m["id"] == nil {
		t.Errorf("no model.id in %s", b)
	}
	if _, ok := doc["version"].(string); !ok {
		t.Errorf("no version in %s", b)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) == 0 {
		t.Fatal("the wrapper sent nothing to the agent socket")
	}
	c := calls[len(calls)-1]
	if c["run"] != "run_1" || c["token"] != "real-test-token" || c["tool"] != agent.ToolStatusLine {
		t.Fatalf("call %v", c)
	}
	args, _ := json.Marshal(c["args"])
	var snap harness.StatusSnapshot
	if err := json.Unmarshal(args, &snap); err != nil || !snap.Valid() || snap.SessionID == "" || snap.ModelID == "" {
		t.Fatalf("snapshot %s: %+v %v", args, snap, err)
	}
}
