//go:build real

package real

import (
	"encoding/json"
	"fmt"
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
	_ = json.NewDecoder(r.Body).Decode(&body)
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
// Code CLI refuses its file tools' writes to another instance's folder and to any instance's settings, while the run's
// own auto memory and the space's checkouts stay writable; a PermissionRequest hook whose agent binary is missing
// still denies. No model and no credentials: a local fake API scripts the tool calls (print mode, a throwaway HOME,
// config dir and space folder). Opt-in: SPACEMAN_REAL_CLAUDE_CLI=1 with claude on PATH.
func TestRealClaudeCLIDeniesOtherInstances(t *testing.T) {
	if os.Getenv("SPACEMAN_REAL_CLAUDE_CLI") != "1" {
		t.Skip("set SPACEMAN_REAL_CLAUDE_CLI=1 (with claude on PATH) to check SpaceMan's permission rules against the real Claude Code CLI")
	}
	claude, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude not installed")
	}
	root := t.TempDir()
	space, _ := filepath.EvalSymlinks(root)
	space = filepath.Join(space, "space")
	home := filepath.Join(root, "home")
	work := filepath.Join(space, "worktrees", "w1")
	outside := filepath.Join(root, "outside.txt")
	for _, d := range []string{home, work, filepath.Join(space, "instances", "ins_a", "claude"), filepath.Join(space, "instances", "ins_b", "claude")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	other := filepath.Join(space, "instances", "ins_b", "claude", "settings.json")
	for p, s := range map[string]string{other: `{"theme": "dark"}`, outside: "outside"} {
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := filepath.Join(space, "instances", "ins_a")
	l, err := harness.Claude{}.Build(harness.Context{AgentBinary: filepath.Join(root, "missing", "spaceman-agent"), ConfigDir: cfg, SpaceDir: space,
		SpaceInstances: []string{"ins_a", "ins_b"}, WorkDir: work, RunID: "run_1", SpaceID: "spc_1", RunDir: root, Binary: claude,
		Payload: proto.LaunchPayload{Kind: "worker"}})
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
	own := filepath.Join(cfg, "claude", "settings.json")
	memory := filepath.Join(cfg, "claude", "projects", strings.NewReplacer("/", "-", ".", "-").Replace(work), "memory", "note.md")
	later := filepath.Join(space, "instances", "ins_c", "claude", "settings.json") // an instance created after the launch
	write := func(p string) map[string]any {
		return map[string]any{"name": "Write", "input": map[string]any{"file_path": p, "content": `{"disableAllHooks": true}`}}
	}
	script := []map[string]any{
		{"name": "Read", "input": map[string]any{"file_path": other}},
		{"name": "Edit", "input": map[string]any{"file_path": other, "old_string": `"theme"`, "new_string": `"disableAllHooks": true, "theme"`}},
		write(other),
		write(filepath.Join(space, "instances", "ins_b", "claude", "projects", "p", "memory", "MEMORY.md")),
		write(filepath.Join(space, "instances", "ins_b", "claude", "CLAUDE.md")),
		write(later),
		{"name": "Read", "input": map[string]any{"file_path": own}},
		{"name": "Edit", "input": map[string]any{"file_path": own, "old_string": `"hooks"`, "new_string": `"disableAllHooks": true, "hooks"`}},
		write(memory),
		write(filepath.Join(space, "worktrees", "other.txt")),
		{"name": "Read", "input": map[string]any{"file_path": outside}},
	}
	api := &scriptedAPI{script: script, results: map[string]toolResult{}}
	srv := httptest.NewServer(api)
	defer srv.Close()
	cmd := exec.Command(claude, "-p", "go", "--model", "claude-sonnet-4-5", "--permission-mode", "acceptEdits") //nolint:gosec // the test's own CLI
	cmd.Dir = work
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "CLAUDE_CONFIG_DIR=" + filepath.Join(cfg, "claude"),
		"ANTHROPIC_BASE_URL=" + srv.URL, "ANTHROPIC_AUTH_TOKEN=dummy", "DISABLE_AUTOUPDATER=1",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1"}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("claude: %v\n%s", err, out)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	const denied = "denied by your permission settings"
	expect := []struct {
		what    string
		denied  bool
		message string
	}{
		{"read another instance's settings", false, ""},
		{"edit another instance's settings", true, denied},
		{"write another instance's settings", true, denied},
		{"write another instance's memory", true, denied},
		{"write another instance's CLAUDE.md", true, denied},
		{"write a later instance's settings", true, denied},
		{"read the own settings", false, ""},
		{"edit the own settings", true, denied},
		{"write the own auto memory", false, ""},
		{"write in the space's checkouts", false, ""},
		{"read outside the working directories (hook binary missing)", true, "SpaceMan denied this request"},
	}
	for i, e := range expect {
		r, ok := api.results[fmt.Sprintf("toolu_%02d", i)]
		if !ok || r.IsError != e.denied || !strings.Contains(r.Text, e.message) {
			t.Errorf("%s: %+v (answered %v)", e.what, r, ok)
		}
	}
	if b, _ := os.ReadFile(other); string(b) != `{"theme": "dark"}` {
		t.Errorf("another instance's settings changed: %s", b)
	}
	if _, err := os.Stat(later); err == nil {
		t.Error("a later instance's settings were written")
	}
	if b, _ := os.ReadFile(own); strings.Contains(string(b), "disableAllHooks") {
		t.Errorf("own settings changed: %s", b)
	}
}
