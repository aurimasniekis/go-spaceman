// Command stubharness stands in for Claude Code and Codex in the local development stack (scripts/dev-stack.sh): no
// model is contacted. It prints a banner and ticks. Like a real harness it starts the MCP servers named by
// --mcp-config, and every prompt line "/call <tool> <json arguments>" becomes one tool call whose result is printed. A
// call that needs a person's approval is retried every few seconds, the way an agent calls again after the approval
// notice. Not part of any release.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
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
	if mcpConfig != "" && strings.Contains(prompt, "/call ") {
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
		defer cs.Close()
		for _, line := range strings.Split(prompt, "\n") {
			line = strings.TrimSpace(line)
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
