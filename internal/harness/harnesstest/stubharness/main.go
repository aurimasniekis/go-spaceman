// Command stubharness stands in for Claude Code and Codex in the local development stack (scripts/dev-stack.sh) and the
// scripted demonstration (test/demo): no model is contacted. It prints a banner and ticks. Like a real harness it starts
// the MCP servers it was configured with (Claude Code's --mcp-config file, or Codex's "-c mcp_servers.<name>.command=…"
// and ".args=[…]" overrides), and every prompt line "/call <tool> <json arguments>" becomes one tool call whose result
// is printed. A call that needs a person's approval is retried every few seconds, the way an agent calls again after the
// approval notice. A line "/write <path> <text>" writes text (\n for new lines) to a file in its role memory
// ($SPACEMAN_MEMORY_DIR). With "/kb-answer <text>" it acts as a KB support session: it reads its SpaceMan messages and
// answers every KB request with the text, citing README.md at the revision of its working tree; "/kb-delay <seconds>"
// makes it work that long on each request first (a busy session). "/await-human [n]" waits until n (default 1) responses
// of people to its human actions have arrived, prints and acknowledges each, and only then goes on with the next line.
// "/dream" acts as a
// dreamer: it reads DREAM.md and the evidence files it lists, appends a note to the first memory listed and reports
// with findings that cite the files it read. A "/transcript" line writes a small transcript in Claude Code's format
// (fictional token counts, no cost) where Claude Code keeps it. A "/fullscreen" line prints a full-screen program's
// start-up the way Claude Code 2.1.317 draws it (alternate screen, mouse and keyboard modes, synchronized output, a
// path in its banner; fullscreen.bin, with neutral text), which the web terminal must survive being reopened after.
// A "/lines <n>" line prints n numbered lines first, a reply longer than the screen for scrolling back. A
// "/styled <n>" line prints n numbered lines in as many 24-bit colours, the way Claude Code styles its output (a
// terminal holding many styles needs much more memory than one of plain text). A "/size" line makes it draw a full-width
// rule and its terminal size at the start and again whenever the terminal is resized (SIGWINCH), the way a full-screen
// program redraws for a new size.
// Not part of any release.
package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/sys/unix"
)

//go:embed fullscreen.bin
var fullscreen []byte

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
	codex := map[string]*server{}
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--mcp-config" && i+1 < len(args):
			mcpConfig = args[i+1]
		case args[i] == "-c" && i+1 < len(args):
			codexOverride(codex, args[i+1])
		case args[i] == "--" && i+1 < len(args):
			prompt = args[i+1]
		}
	}
	writes(prompt)
	longReply(prompt)
	if strings.Contains(prompt, "/fullscreen") {
		_, _ = os.Stdout.Write(fullscreen)
	}
	if strings.Contains(prompt, "/size") {
		reportSize()
	}
	if strings.Contains(prompt, "/transcript") {
		transcript(args, wd)
	}
	if hasMCPLines(prompt) {
		servers := codex
		if mcpConfig != "" {
			servers = claudeServers(mcpConfig)
		}
		calls(servers, prompt)
	}
	for i := 0; ; i++ {
		fmt.Fprintf(os.Stdout, "\x1b[36mtick\x1b[0m %d\r\n", i)
		time.Sleep(time.Second)
	}
}

// reportSize draws a rule as wide as the terminal and "terminal size COLSxROWS" now and after every resize.
func reportSize() {
	draw := func() {
		ws, err := unix.IoctlGetWinsize(int(os.Stdout.Fd()), unix.TIOCGWINSZ)
		if err != nil {
			fmt.Fprintf(os.Stdout, "terminal size unknown: %v\r\n", err)
			return
		}
		fmt.Fprintf(os.Stdout, "%s\r\nterminal size %dx%d\r\n", strings.Repeat("─", int(ws.Col)), ws.Col, ws.Row)
	}
	draw()
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)
	go func() {
		for range ch {
			draw()
		}
	}()
}

// longReply prints the numbered lines a "/lines <n>" (at most 5000) or "/styled <n>" (at most 20000) prompt line asks for.
func longReply(prompt string) {
	for _, line := range strings.Split(prompt, "\n") {
		v, ok := strings.CutPrefix(strings.TrimSpace(line), "/lines ")
		if !ok {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil || n < 1 {
			continue
		}
		for i := 1; i <= min(n, 5000); i++ {
			fmt.Fprintf(os.Stdout, "reply line %d of %d\r\n", i, n)
		}
	}
	for _, line := range strings.Split(prompt, "\n") {
		v, ok := strings.CutPrefix(strings.TrimSpace(line), "/styled ")
		if !ok {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil || n < 1 {
			continue
		}
		for i := 1; i <= min(n, 20000); i++ {
			fmt.Fprintf(os.Stdout, "\x1b[38;2;%d;%d;%dmstyled line %d of %d\x1b[0m\r\n", 40+i%200, 40+(i/200)%200, 120+i%97, i, n)
		}
	}
}

// hasMCPLines reports whether the prompt has a line that needs the MCP server.
func hasMCPLines(prompt string) bool {
	for _, line := range strings.Split(prompt, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "/call ") || strings.HasPrefix(line, "/kb-answer ") || strings.HasPrefix(line, "/await-human") || line == "/dream" {
			return true
		}
	}
	return false
}

// server is an MCP server the harness was configured with.
type server struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

// claudeServers reads Claude Code's --mcp-config file.
func claudeServers(path string) map[string]*server {
	var cfg struct {
		Servers map[string]*server `json:"mcpServers"`
	}
	b, err := os.ReadFile(path) //nolint:gosec // the harness reads the MCP config it was given, as Claude Code does
	if err == nil {
		err = json.Unmarshal(b, &cfg)
	}
	if err != nil {
		fmt.Fprintf(os.Stdout, "mcp config: %v\r\n", err)
	}
	return cfg.Servers
}

// codexOverride reads one Codex "-c key=value" override of an MCP server. SpaceMan writes the values as TOML basic
// strings and arrays of them, which decode as JSON.
func codexOverride(out map[string]*server, kv string) {
	key, val, ok := strings.Cut(kv, "=")
	rest, isServer := strings.CutPrefix(key, "mcp_servers.")
	name, field, _ := strings.Cut(rest, ".")
	if !ok || !isServer || name == "" {
		return
	}
	if out[name] == nil {
		out[name] = &server{}
	}
	switch field {
	case "command":
		_ = json.Unmarshal([]byte(val), &out[name].Command)
	case "args":
		_ = json.Unmarshal([]byte(val), &out[name].Args)
	}
}

// calls runs the prompt's MCP lines ("/call", "/kb-answer", "/await-human", "/dream") in order against the first
// configured MCP server.
func calls(servers map[string]*server, prompt string) {
	if len(servers) == 0 {
		fmt.Fprintf(os.Stdout, "no MCP server configured\r\n")
		return
	}
	var delay time.Duration
	for _, line := range strings.Split(prompt, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "/kb-delay "); ok {
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 && n <= 600 {
				delay = time.Duration(n) * time.Second
			}
		}
	}
	ctx := context.Background()
	for name, s := range servers {
		cs, err := mcp.NewClient(&mcp.Implementation{Name: "stubharness", Version: "0.0.0"}, nil).
			Connect(ctx, &mcp.CommandTransport{Command: exec.CommandContext(ctx, s.Command, s.Args...)}, nil) //nolint:gosec // the harness runs the configured server, as Claude Code does
		if err != nil {
			fmt.Fprintf(os.Stdout, "mcp %s: %v\r\n", name, err)
			return
		}
		for _, line := range strings.Split(prompt, "\n") {
			line = strings.TrimSpace(line)
			if text, ok := strings.CutPrefix(line, "/kb-answer "); ok {
				go answerKB(ctx, cs, text, delay)
				continue
			}
			if line == "/await-human" || strings.HasPrefix(line, "/await-human ") {
				n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "/await-human")))
				if err != nil || n < 1 {
					n = 1
				}
				awaitHuman(ctx, cs, n)
				continue
			}
			switch line {
			case "/dream":
				dream(ctx, cs)
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

// answerKB answers KB requests from its messages for as long as the stub runs, working delay on each first.
func answerKB(ctx context.Context, cs *mcp.ClientSession, text string, delay time.Duration) {
	done := map[string]bool{}
	evidence := map[string]string{"path": "README.md"}
	if rev, err := exec.CommandContext(ctx, "git", "rev-parse", "HEAD").Output(); err == nil {
		evidence["revision"] = strings.TrimSpace(string(rev))
	}
	for ; ctx.Err() == nil; time.Sleep(2 * time.Second) {
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "spaceman_messages"})
		if err != nil {
			continue
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
			if delay > 0 {
				fmt.Fprintf(os.Stdout, "\x1b[33m→ working on %s for %s\x1b[0m\r\n", m.CorrelationID, delay)
				time.Sleep(delay)
			}
			fmt.Fprintf(os.Stdout, "\x1b[33m→ answering %s\x1b[0m\r\n", m.CorrelationID)
			_, _ = cs.CallTool(ctx, &mcp.CallToolParams{Name: "spaceman_kb_answer", Arguments: map[string]any{"request_id": m.CorrelationID,
				"answer": strings.ReplaceAll(text, `\n`, "\n"), "basis": "committed", "evidence": []map[string]string{evidence}}})
			_, _ = cs.CallTool(ctx, &mcp.CallToolParams{Name: "spaceman_message_ack", Arguments: map[string]any{"id": m.ID}})
		}
	}
}

// awaitHuman waits (up to 20 minutes) until n responses of people to its human actions have arrived, printing each
// with the action's response and acknowledging it.
func awaitHuman(ctx context.Context, cs *mcp.ClientSession, n int) {
	for i := 0; i < n; i++ {
		if !awaitOneHuman(ctx, cs) {
			return
		}
	}
}

func awaitOneHuman(ctx context.Context, cs *mcp.ClientSession) bool {
	fmt.Fprintf(os.Stdout, "\x1b[33m→ waiting for a person's response\x1b[0m\r\n")
	for deadline := time.Now().Add(20 * time.Minute); time.Now().Before(deadline); time.Sleep(2 * time.Second) {
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "spaceman_messages"})
		if err != nil || res.IsError {
			continue
		}
		var inbox struct {
			Human []humanNotice `json:"human_responses"`
		}
		if b, err := json.Marshal(res.StructuredContent); err == nil {
			_ = json.Unmarshal(b, &inbox)
		}
		var h humanNotice
		for _, x := range inbox.Human {
			if !seenHuman[x.ID] {
				h = x
				break
			}
		}
		if h.ID == "" {
			continue
		}
		seenHuman[h.ID] = true
		answer := ""
		if got, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "spaceman_human_get", Arguments: map[string]any{"action_id": h.ActionID}}); err == nil {
			var a struct {
				Response json.RawMessage `json:"response"`
			}
			if b, err := json.Marshal(got.StructuredContent); err == nil {
				_ = json.Unmarshal(b, &a)
			}
			answer = string(a.Response)
		}
		fmt.Fprintf(os.Stdout, "human response to %s (%s): %s %s\r\n", h.ActionID, h.Title, h.Outcome, answer)
		_, _ = cs.CallTool(ctx, &mcp.CallToolParams{Name: "spaceman_message_ack", Arguments: map[string]any{"id": h.ID}})
		return true
	}
	fmt.Fprintf(os.Stdout, "gave up waiting for a person\r\n")
	return false
}

// humanNotice is a person's response to one of the run's human actions, as spaceman_messages lists it.
type humanNotice struct {
	ID       string `json:"id"`
	ActionID string `json:"action_id"`
	Title    string `json:"title"`
	Outcome  string `json:"outcome"`
}

// seenHuman holds the responses already printed (the stub has one MCP session).
var seenHuman = map[string]bool{}

// dream reads the dream workspace (DREAM.md and the evidence files it lists), appends a note to the first memory listed
// and reports, citing the files it read.
func dream(ctx context.Context, cs *mcp.ClientSession) {
	manifest, err := os.ReadFile("DREAM.md")
	if err != nil {
		fmt.Fprintf(os.Stdout, "dream: %v\r\n", err)
		return
	}
	var memories, evidence []string
	for _, line := range strings.Split(string(manifest), "\n") {
		_, rest, ok := strings.Cut(line, "- `")
		p, _, closed := strings.Cut(rest, "`")
		switch {
		case !ok || !closed:
		case strings.HasPrefix(p, "memory/"):
			memories = append(memories, strings.TrimSuffix(p, "/"))
		case strings.HasPrefix(p, "evidence/"):
			evidence = append(evidence, p)
		}
	}
	var findings []map[string]string
	var read []string
	for _, p := range evidence {
		b, err := os.ReadFile(filepath.Clean(p)) //nolint:gosec // a file DREAM.md lists in the dream's own workspace; a development stub
		if err != nil {
			continue
		}
		lines := strings.Count(string(b), "\n")
		read = append(read, fmt.Sprintf("%s (%d bytes, %d lines)", p, len(b), lines))
		findings = append(findings, map[string]string{"kind": "insight", "summary": fmt.Sprintf("Read %d transcript lines of a worker session.", lines), "evidence": p})
	}
	fmt.Fprintf(os.Stdout, "dream: read DREAM.md, %d memories, evidence: %s\r\n", len(memories), strings.Join(read, ", "))
	in := map[string]any{"summary": fmt.Sprintf("Read DREAM.md and %d evidence file(s): %s.", len(read), strings.Join(read, ", ")), "no_change": true}
	if len(memories) > 0 && len(read) > 0 {
		note := filepath.Join(filepath.Clean(memories[0]), "dream-notes.md")
		f, err := os.OpenFile(note, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // the dream's own memory checkout
		if err == nil {
			_, _ = fmt.Fprintf(f, "- %s: consolidated %s\n", time.Now().UTC().Format("2006-01-02"), strings.Join(read, ", "))
			_ = f.Close()
			in["no_change"] = false
			in["changes"] = []string{"dream-notes.md"}
		}
	}
	if len(findings) > 0 {
		in["findings"] = findings
	}
	call(ctx, cs, "spaceman_dream_report", in)
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
