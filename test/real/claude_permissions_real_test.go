//go:build real

package real

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/agent"
	"github.com/aurimasniekis/go-spaceman/internal/harness"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// scriptedAPI is a fake Anthropic Messages API: each main-loop request (one that offers the full tool list) is
// answered with the next scripted tool_use, and every tool_result Claude Code sends back is recorded by tool_use ID.
type scriptedAPI struct {
	mu      sync.Mutex
	script  []map[string]any
	next    int
	results map[string]toolResult
	request string // the body of the first main-loop request (system prompt and messages), before any tool ran
}

type toolResult struct {
	IsError bool
	Text    string
}

func (f *scriptedAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		_, _ = w.Write([]byte("{}"))
		return
	}
	var body struct {
		Stream   bool              `json:"stream"`
		Model    string            `json:"model"`
		Tools    []json.RawMessage `json:"tools"`
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	raw, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(raw, &body)
	if strings.Contains(r.URL.Path, "count_tokens") {
		_, _ = w.Write([]byte(`{"input_tokens":10}`))
		return
	}
	f.mu.Lock()
	for _, m := range body.Messages {
		var blocks []struct {
			Type      string          `json:"type"`
			ToolUseID string          `json:"tool_use_id"`
			IsError   bool            `json:"is_error"`
			Content   json.RawMessage `json:"content"`
		}
		if json.Unmarshal(m.Content, &blocks) != nil {
			continue
		}
		for _, b := range blocks {
			if b.Type == "tool_result" {
				f.results[b.ToolUseID] = toolResult{IsError: b.IsError, Text: string(b.Content)}
			}
		}
	}
	if len(body.Tools) > 5 && f.request == "" {
		f.request = string(raw)
	}
	block := map[string]any{"type": "text", "text": "done"}
	stop := "end_turn"
	if len(body.Tools) > 5 && f.next < len(f.script) {
		block = map[string]any{"type": "tool_use", "id": fmt.Sprintf("toolu_%02d", f.next), "name": f.script[f.next]["name"], "input": f.script[f.next]["input"]}
		stop = "tool_use"
		f.next++
	}
	f.mu.Unlock()
	usage := map[string]any{"input_tokens": 1, "output_tokens": 1}
	if !body.Stream {
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "msg_1", "type": "message", "role": "assistant", "model": body.Model,
			"content": []any{block}, "stop_reason": stop, "stop_sequence": nil, "usage": usage})
		return
	}
	w.Header().Set("content-type", "text/event-stream")
	sse := func(ev string, d any) {
		b, _ := json.Marshal(d)
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev, b)
	}
	sse("message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": "msg_1", "type": "message", "role": "assistant",
		"model": body.Model, "content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": usage}})
	if block["type"] == "text" {
		sse("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
		sse("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": "done"}})
	} else {
		in, _ := json.Marshal(block["input"])
		sse("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "tool_use",
			"id": block["id"], "name": block["name"], "input": map[string]any{}}})
		sse("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "input_json_delta", "partial_json": string(in)}})
	}
	sse("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	sse("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop, "stop_sequence": nil}, "usage": map[string]any{"output_tokens": 1}})
	sse("message_stop", map[string]any{"type": "message_stop"})
}

// TestRealClaudeCLIDeniesOtherInstances (ADR-0068, T-40): with the settings SpaceMan writes for a run, the real Claude
// Code CLI refuses its file tools' writes to another instance's folder and to any instance's settings, instructions
// (CLAUDE.md, rules), cached remote settings, memory and transcripts, also with more instances than
// MaxInstanceDenyRules, while the run's own auto memory (moved outside the space folder, with the memory it kept in its
// config dir before) and the space's checkouts stay writable; a PermissionRequest hook whose agent binary is missing
// still denies. No model and no credentials: a local fake API scripts the tool calls (print mode, a throwaway HOME,
// config dir and space folder). Opt-in: SPACEMAN_REAL_CLAUDE_CLI=1 with claude and git on PATH.
func TestRealClaudeCLIDeniesOtherInstances(t *testing.T) {
	if os.Getenv("SPACEMAN_REAL_CLAUDE_CLI") != "1" {
		t.Skip("set SPACEMAN_REAL_CLAUDE_CLI=1 (with claude on PATH) to check SpaceMan's permission rules against the real Claude Code CLI")
	}
	claude, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude not installed")
	}
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	space := filepath.Join(root, "space")
	home := filepath.Join(root, "home")
	repo := filepath.Join(space, "repo")
	work := filepath.Join(space, "worktrees", "w1")
	outside := filepath.Join(root, "outside.txt")
	// More other instances than MaxInstanceDenyRules, so the run gets no per-instance rules: only the rules for every
	// instance's configuration, memory and transcripts protect them.
	instances := []string{"ins_a", "ins_b"}
	for i := range harness.MaxInstanceDenyRules {
		instances = append(instances, fmt.Sprintf("ins_x%03d", i))
	}
	for _, d := range []string{home, repo} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range instances {
		if err := os.MkdirAll(filepath.Join(space, "instances", id, "claude"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// The working directory is a linked worktree, as SpaceMan's task checkouts are: Claude Code keys its project state
	// by the main checkout.
	gitEnv := append(os.Environ(), "HOME="+home, "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	for _, args := range [][]string{{"-C", repo, "init", "-q"}, {"-C", repo, "commit", "-q", "--allow-empty", "-m", "init"}, {"-C", repo, "worktree", "add", "-q", work}} {
		cmd := exec.Command("git", args...) //nolint:gosec // fixed arguments
		cmd.Env = gitEnv
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	other := filepath.Join(space, "instances", "ins_b", "claude", "settings.json")
	for p, s := range map[string]string{other: `{"theme": "dark"}`, outside: "outside"} {
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := filepath.Join(space, "instances", "ins_a")
	claudeDir := filepath.Join(cfg, "claude")
	run := func(autoMem string, script []map[string]any) *scriptedAPI {
		t.Helper()
		l, err := harness.Claude{}.Build(harness.Context{AgentBinary: filepath.Join(root, "missing", "spaceman-agent"), ConfigDir: cfg, SpaceDir: space,
			SpaceInstances: instances, WorkDir: work, RunID: "run_1", SpaceID: "spc_1", RunDir: root, Binary: claude,
			Payload: proto.LaunchPayload{Kind: "worker"}, AutoMemoryDir: autoMem})
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
		api := &scriptedAPI{script: script, results: map[string]toolResult{}}
		srv := httptest.NewServer(api)
		defer srv.Close()
		cmd := exec.Command(claude, "-p", "go", "--model", "claude-sonnet-4-5", "--permission-mode", "acceptEdits") //nolint:gosec // the test's own CLI
		cmd.Dir = work
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "CLAUDE_CONFIG_DIR=" + claudeDir,
			"ANTHROPIC_BASE_URL=" + srv.URL, "ANTHROPIC_AUTH_TOKEN=dummy", "DISABLE_AUTOUPDATER=1",
			"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1"}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("claude: %v\n%s", err, out)
		}
		return api
	}

	// Before: a node without the move (no autoMemoryDirectory) keeps the auto memory in the config dir, under the key
	// SpaceMan computes for the worktree's project; that memory is what the move must carry over.
	oldMem := filepath.Join(claudeDir, "projects", agent.ClaudeProjectKey(agent.ClaudeProjectRoot(work)), "memory")
	if agent.ClaudeProjectRoot(work) != repo {
		t.Fatalf("project root %s", agent.ClaudeProjectRoot(work))
	}
	api := run("", nil)
	if !strings.Contains(api.request, oldMem) {
		t.Fatalf("Claude Code's default auto memory is not %s:\n%.3000s", oldMem, api.request)
	}
	const marker = "remember-the-blue-lighthouse"
	if err := os.MkdirAll(oldMem, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldMem, "MEMORY.md"), []byte("- "+marker+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// After: the node moves the auto memory out of the space folder, carrying the old memory over.
	autoMem, err := agent.PrepareAutoMemory(claudeDir, filepath.Join(root, "automemory", "spc_1", "ins_a"), work)
	if err != nil {
		t.Fatal(err)
	}
	own := filepath.Join(claudeDir, "settings.json")
	later := filepath.Join(space, "instances", "ins_c", "claude") // an instance created after the launch
	otherCfg := filepath.Join(space, "instances", "ins_b", "claude")
	write := func(p string) map[string]any {
		return map[string]any{"name": "Write", "input": map[string]any{"file_path": p, "content": `{"disableAllHooks": true}`}}
	}
	expect := []struct {
		what    string
		call    map[string]any
		denied  bool
		message string
	}{
		{"read another instance's settings", map[string]any{"name": "Read", "input": map[string]any{"file_path": other}}, false, ""},
		{"edit another instance's settings", map[string]any{"name": "Edit", "input": map[string]any{"file_path": other, "old_string": `"theme"`,
			"new_string": `"disableAllHooks": true, "theme"`}}, true, deniedBySettings},
		{"write another instance's settings", write(other), true, deniedBySettings},
		{"write another instance's memory", write(filepath.Join(otherCfg, "projects", "p", "memory", "MEMORY.md")), true, deniedBySettings},
		{"write another instance's transcript", write(filepath.Join(otherCfg, "projects", "p", "0b5e3c1e-1111-4222-8333-444455556666.jsonl")), true, deniedBySettings},
		{"write another instance's CLAUDE.md", write(filepath.Join(otherCfg, "CLAUDE.md")), true, deniedBySettings},
		{"write another instance's rules", write(filepath.Join(otherCfg, "rules", "x.md")), true, deniedBySettings},
		{"write another instance's policy limits", write(filepath.Join(otherCfg, "policy-limits.json")), true, deniedBySettings},
		{"write another instance's shell snapshot", write(filepath.Join(otherCfg, "shell-snapshots", "snapshot-bash-1-x.sh")), true, deniedBySettings},
		{"write another instance's session environment", write(filepath.Join(otherCfg, "session-env", "s", "hook-0.sh")), true, deniedBySettings},
		{"write another instance's plan file (not loaded at launch; over the cap, no per-instance rules)", write(filepath.Join(otherCfg, "plans", "p.md")), false, ""},
		{"write a later instance's settings", write(filepath.Join(later, "settings.json")), true, deniedBySettings},
		{"write a later instance's rules", write(filepath.Join(later, "rules", "x.md")), true, deniedBySettings},
		{"write a later instance's memory", write(filepath.Join(later, "projects", "p", "memory", "MEMORY.md")), true, deniedBySettings},
		{"read the own settings", map[string]any{"name": "Read", "input": map[string]any{"file_path": own}}, false, ""},
		{"edit the own settings", map[string]any{"name": "Edit", "input": map[string]any{"file_path": own, "old_string": `"hooks"`,
			"new_string": `"disableAllHooks": true, "hooks"`}}, true, deniedBySettings},
		{"write the own rules", write(filepath.Join(claudeDir, "rules", "x.md")), true, deniedBySettings},
		{"write the own remote settings", write(filepath.Join(claudeDir, "remote-settings.json")), true, deniedBySettings},
		{"write the own old auto memory", write(filepath.Join(oldMem, "note.md")), true, deniedBySettings},
		{"read the own auto memory", map[string]any{"name": "Read", "input": map[string]any{"file_path": filepath.Join(autoMem, "MEMORY.md")}}, false, marker},
		{"write the own auto memory", write(filepath.Join(autoMem, "note.md")), false, ""},
		{"write in the space's checkouts", write(filepath.Join(space, "worktrees", "other.txt")), false, ""},
		{"read outside the working directories (hook binary missing)", map[string]any{"name": "Read", "input": map[string]any{"file_path": outside}},
			true, "SpaceMan denied this request"},
	}
	var script []map[string]any
	for _, e := range expect {
		script = append(script, e.call)
	}
	api = run(autoMem, script)
	api.mu.Lock()
	defer api.mu.Unlock()
	for i, e := range expect {
		r, ok := api.results[fmt.Sprintf("toolu_%02d", i)]
		if !ok || r.IsError != e.denied || !strings.Contains(r.Text, e.message) {
			t.Errorf("%s: %+v (answered %v)", e.what, r, ok)
		}
	}
	if !strings.Contains(api.request, autoMem) || !strings.Contains(api.request, marker) {
		t.Errorf("the moved auto memory (with the old memory) is not loaded:\n%.3000s", api.request)
	}
	if b, _ := os.ReadFile(other); string(b) != `{"theme": "dark"}` {
		t.Errorf("another instance's settings changed: %s", b)
	}
	if _, err := os.Stat(later); err == nil {
		t.Error("a later instance's folder was written")
	}
	if b, _ := os.ReadFile(own); strings.Contains(string(b), "disableAllHooks") {
		t.Errorf("own settings changed: %s", b)
	}
	for _, p := range []string{filepath.Join(otherCfg, "rules"), filepath.Join(otherCfg, "projects"), filepath.Join(claudeDir, "rules"),
		filepath.Join(claudeDir, "remote-settings.json")} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("%s was written", p)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(autoMem, "note.md")); !strings.Contains(string(b), "disableAllHooks") {
		t.Errorf("own auto memory not written: %q", b)
	}
}

const deniedBySettings = "denied by your permission settings"
