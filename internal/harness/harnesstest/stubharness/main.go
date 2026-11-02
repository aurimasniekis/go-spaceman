// Command stubharness stands in for Claude Code and Codex in the local development stack (scripts/dev-stack.sh): no
// model is contacted. It prints a banner and ticks. Like a real harness it starts the MCP servers named by
// --mcp-config, and every prompt line "/call <tool> <json arguments>" becomes one tool call whose result is printed. A
// call that needs a person's approval is retried every few seconds, the way an agent calls again after the approval
// notice. A line "/write <path> <text>" writes text (\n for new lines) to a file in its role memory
// ($SPACEMAN_MEMORY_DIR). With "/kb-answer <text>" it acts as a KB support session: it reads its SpaceMan messages and
// answers every KB request with the text, citing README.md. A "/transcript" line writes a small transcript in Claude
// Code's format (fictional token counts, no cost) where Claude Code keeps it. Not part of any release.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	args := os.Args[1:]
	if len(args) == 1 && args[0] == "--version" {
		fmt.Fprintln(os.Stdout, "0.0.0 (stub)")
		return
	}
	wd, _ := os.Getwd()
	fmt.Fprintf(os.Stdout, "\x1b[1;35mstub harness\x1b[0m started in %s\r\n", wd)
	fmt.Fprintf(os.Stdout, "args: %s\r\n", strings.Join(args, " "))
	var mcpConfig, prompt string
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--mcp-config" && i+1 < len(args):
			mcpConfig = args[i+1]
		case args[i] == "--" && i+1 < len(args):
			prompt = args[i+1]
		}
	}
	writes(prompt)
	if strings.Contains(prompt, "/transcript") {
		transcript(args, wd)
	}
	if mcpConfig != "" && (strings.Contains(prompt, "/call ") || strings.Contains(prompt, "/kb-answer ")) {
		calls(mcpConfig, prompt)
	}
	for i := 0; ; i++ {
		fmt.Fprintf(os.Stdout, "\x1b[36mtick\x1b[0m %d\r\n", i)
		time.Sleep(time.Second)
	}
}

// calls runs the prompt's "/call" lines against the first MCP server in the config.
func calls(path, prompt string) {
	var cfg struct {
		Servers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	b, err := os.ReadFile(path) //nolint:gosec // the harness reads the MCP config it was given, as Claude Code does
	if err == nil {
		err = json.Unmarshal(b, &cfg)
	}
	if err != nil || len(cfg.Servers) == 0 {
		fmt.Fprintf(os.Stdout, "mcp config: %v\r\n", err)
		return
	}
	ctx := context.Background()
	for name, s := range cfg.Servers {
		cs, err := mcp.NewClient(&mcp.Implementation{Name: "stubharness", Version: "0.0.0"}, nil).
			Connect(ctx, &mcp.CommandTransport{Command: exec.CommandContext(ctx, s.Command, s.Args...)}, nil) //nolint:gosec // the harness runs the configured server, as Claude Code does
		if err != nil {
			fmt.Fprintf(os.Stdout, "mcp %s: %v\r\n", name, err)
			return
		}
		for _, line := range strings.Split(prompt, "\n") {
			line = strings.TrimSpace(line)
			if text, ok := strings.CutPrefix(line, "/kb-answer "); ok {
				go answerKB(ctx, cs, text)
				continue
			}
			rest, ok := strings.CutPrefix(line, "/call ")
			if !ok {
				continue
			}
			tool, argJSON, _ := strings.Cut(rest, " ")
			var a map[string]any
			if argJSON != "" {
				if err := json.Unmarshal([]byte(argJSON), &a); err != nil {
					fmt.Fprintf(os.Stdout, "%s: bad arguments: %v\r\n", tool, err)
					continue
				}
			}
			call(ctx, cs, tool, a)
		}
		return
	}
}

func call(ctx context.Context, cs *mcp.ClientSession, tool string, a map[string]any) {
	fmt.Fprintf(os.Stdout, "\x1b[33m→ %s\x1b[0m\r\n", tool)
	for attempt := 0; attempt < 200; attempt++ {
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: a})
		if err != nil {
			fmt.Fprintf(os.Stdout, "  error: %v\r\n", err)
			return
		}
		text := ""
		for _, c := range res.Content {
			if t, ok := c.(*mcp.TextContent); ok {
				text += t.Text
			}
		}
		if res.IsError && strings.Contains(text, "needs a person's approval") {
			if attempt == 0 {
				fmt.Fprintf(os.Stdout, "  waiting for approval: %s\r\n", text)
			}
			time.Sleep(3 * time.Second)
			continue
		}
		fmt.Fprintf(os.Stdout, "  %s\r\n", text)
		return
	}
	fmt.Fprintf(os.Stdout, "  gave up waiting for approval\r\n")
}

// writes runs the prompt's "/write" lines in the role memory folder.
func writes(prompt string) {
	dir := os.Getenv("SPACEMAN_MEMORY_DIR")
	for _, line := range strings.Split(prompt, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "/write ")
		if !ok || dir == "" {
			continue
		}
		name, text, _ := strings.Cut(rest, " ")
		p := filepath.Join(dir, filepath.Clean("/"+name))           //nolint:gosec // cleaned under the memory folder; a development stub
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err == nil { //nolint:gosec // see above
			content := strings.ReplaceAll(text, `\n`, "\n")
			err = os.WriteFile(p, []byte(content), 0o600) //nolint:gosec // see above
			fmt.Fprintf(os.Stdout, "wrote memory %s (%v)\r\n", name, err)
		}
	}
}

// answerKB answers KB requests from its messages for as long as the stub runs.
func answerKB(ctx context.Context, cs *mcp.ClientSession, text string) {
	done := map[string]bool{}
	for {
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "spaceman_messages"})
		if err != nil {
			return
		}
		var inbox struct {
			Messages []struct {
				ID            string `json:"id"`
				Kind          string `json:"kind"`
				CorrelationID string `json:"correlation_id"`
			} `json:"messages"`
		}
		if b, err := json.Marshal(res.StructuredContent); err == nil {
			_ = json.Unmarshal(b, &inbox)
		}
		for _, m := range inbox.Messages {
			if m.Kind != "request" || !strings.HasPrefix(m.CorrelationID, "kbq_") || done[m.ID] {
				continue
			}
			done[m.ID] = true
			fmt.Fprintf(os.Stdout, "\x1b[33m→ answering %s\x1b[0m\r\n", m.CorrelationID)
			_, _ = cs.CallTool(ctx, &mcp.CallToolParams{Name: "spaceman_kb_answer", Arguments: map[string]any{"request_id": m.CorrelationID,
				"answer": strings.ReplaceAll(text, `\n`, "\n"), "basis": "committed", "evidence": []map[string]string{{"path": "README.md"}}}})
			_, _ = cs.CallTool(ctx, &mcp.CallToolParams{Name: "spaceman_message_ack", Arguments: map[string]any{"id": m.ID}})
		}
		time.Sleep(2 * time.Second)
	}
}

// transcript writes a Claude-Code-shaped session transcript with made-up usage for the dev stack's screenshots.
func transcript(args []string, wd string) {
	cfg := os.Getenv("CLAUDE_CONFIG_DIR")
	session := ""
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--session-id" {
			session = args[i+1]
		}
	}
	if cfg == "" || session == "" {
		return
	}
	dir := filepath.Join(cfg, "projects", strings.NewReplacer("/", "-", ".", "-").Replace(wd))
	if err := os.MkdirAll(dir, 0o700); err != nil { //nolint:gosec // the stub's own config folder
		return
	}
	lines := []string{
		`{"type":"user","message":{"role":"user","content":"(stub)"}}`,
		`{"type":"assistant","message":{"id":"msg_stub_1","model":"claude-sonnet-5-5","usage":{"input_tokens":1840,"output_tokens":612,"cache_creation_input_tokens":9120,"cache_read_input_tokens":40210}}}`,
		`{"type":"assistant","message":{"id":"msg_stub_2","model":"claude-sonnet-5-5","usage":{"input_tokens":920,"output_tokens":1374,"cache_creation_input_tokens":2048,"cache_read_input_tokens":51330}}}`,
	}
	_ = os.WriteFile(filepath.Join(dir, session+".jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o600) //nolint:gosec // the stub's own config folder
}
