package harness

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"

	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// Claude is the Claude Code adapter (verified against 2.1.316, docs/contracts/harnesses.md).
//
//   - isolation: CLAUDE_CONFIG_DIR=<config>/claude per space and instance
//   - session: --session-id <uuid> for a new conversation, --resume <id> to continue one
//   - MCP: --mcp-config <file> --strict-mcp-config (only the SpaceMan bridge; no user-level servers leak in)
//   - prompt: --append-system-prompt <text>; the initial instruction is the positional prompt
//   - auth: CLAUDE_CODE_OAUTH_TOKEN (claude setup-token) or ANTHROPIC_API_KEY, through the launcher shim only; a gateway
//     run (ADR-0060) gets ANTHROPIC_BASE_URL and ANTHROPIC_AUTH_TOKEN (its child token) instead, also through the shim
//     only (it scrubs every inherited ANTHROPIC_* variable), and nothing else: no account secret, no settings key that
//     adds a credential source, no stored login (verified with Claude Code 2.1.317, docs/contracts/harnesses.md)
//
// Runs are interactive inside herdr-core so status arrives via OSC 7501 (verified live) — never print mode.
type Claude struct{}

// Kind implements Harness.
func (Claude) Kind() string { return KindClaude }

// Detect implements Harness.
func (Claude) Detect(ctx context.Context, binary string) proto.HarnessInfo {
	return detect(ctx, KindClaude, binary, []string{CapFixedSessionID, CapResume, CapMCPConfig, CapProgramStatus, CapSafeInput, CapSystemPrompt, CapTranscripts, CapSkills})
}

// ClaudeTUI is the Claude Code renderer ("tui" in settings.json) runs get unless their profile sets one (ADR-0064).
const ClaudeTUI = "default"

var claudePermissionModes = map[string]bool{"default": true, "acceptEdits": true, "plan": true, "auto": true, "dontAsk": true, "bypassPermissions": true}

// ClaudeTerminalPromptTools are the Claude Code tools that end in a person answering in the run's terminal:
// AskUserQuestion's picker, and EnterPlanMode, whose plan ends in ExitPlanMode's approval dialog. Nobody may be watching
// that terminal, so a run waiting there stalls silently; agents ask people through spaceman_human_ask instead, which is
// durable, reminds people and wakes the agent (ADR-0066).
var ClaudeTerminalPromptTools = []string{"AskUserQuestion", "EnterPlanMode"}

// denyTerminalPrompts appends ClaudeTerminalPromptTools to the run's permissions.deny, so Claude Code drops them from
// the tools the model sees (verified with Claude Code 2.1.317 and 2.1.319: a whole-tool deny rule removes the tool from
// the request). The profile's own deny rules come first and are kept, duplicates are dropped, and every other
// permissions key (allow, ask, additionalDirectories, defaultMode) is kept as it is. A profile opts a tool back in by
// listing it in its own permissions.allow (a deny would win over that allow, so SpaceMan leaves that tool's deny out).
// The profile's settings map is never modified: the permissions object is copied.
func denyTerminalPrompts(own map[string]any) {
	perms := map[string]any{}
	if cur, ok := own["permissions"].(map[string]any); ok {
		for k, v := range cur {
			perms[k] = v
		}
	}
	allow := stringsOf(perms["allow"])
	var deny []string
	for _, rule := range stringsOf(perms["deny"]) {
		if !slices.Contains(deny, rule) {
			deny = append(deny, rule)
		}
	}
	for _, tool := range ClaudeTerminalPromptTools {
		if !slices.Contains(allow, tool) && !slices.Contains(deny, tool) {
			deny = append(deny, tool)
		}
	}
	if len(deny) > 0 {
		perms["deny"] = deny
		own["permissions"] = perms
	}
}

// stringsOf returns a settings list ([]string or a JSON []any of strings, as Validate accepts) as a new []string.
func stringsOf(v any) []string {
	var out []string
	switch l := v.(type) {
	case []string:
		out = append(out, l...)
	case []any:
		for _, e := range l {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

// Build implements Harness.
func (Claude) Build(c Context) (Launch, error) {
	if err := checkCommon(c); err != nil {
		return Launch{}, err
	}
	p := c.Payload
	gw, err := gatewayFor(p)
	if err != nil {
		return Launch{}, err
	}
	cfg := filepath.Join(c.ConfigDir, "claude")
	mcpPath := filepath.Join(c.RunDir, "mcp.json")
	mcp, err := json.MarshalIndent(map[string]any{"mcpServers": map[string]any{
		"spaceman": map[string]any{"type": "stdio", "command": c.AgentBinary, "args": bridgeArgs(c)},
	}}, "", "  ")
	if err != nil {
		return Launch{}, err
	}
	argv := []string{c.Binary}
	session := p.SessionID
	switch {
	case p.Resume && session != "":
		if !reUUID.MatchString(session) {
			return Launch{}, errors.New("claude: session_id must be a UUID")
		}
		argv = append(argv, "--resume", session)
	case session != "":
		if !reUUID.MatchString(session) {
			return Launch{}, errors.New("claude: session_id must be a UUID")
		}
		argv = append(argv, "--session-id", session)
	default:
		session = NewUUID()
		argv = append(argv, "--session-id", session)
	}
	argv = append(argv, "--mcp-config", mcpPath, "--strict-mcp-config")
	if p.Model != "" {
		argv = append(argv, "--model", p.Model)
	}
	if m := p.Settings["permission_mode"]; m != "" {
		if !claudePermissionModes[m] {
			return Launch{}, errors.New("claude: unknown permission_mode")
		}
		argv = append(argv, "--permission-mode", m)
	}
	if c.MemoryDir != "" {
		argv = append(argv, "--add-dir", c.MemoryDir)
	}
	if p.SystemText != "" {
		argv = append(argv, "--append-system-prompt", p.SystemText)
	}
	if p.Prompt != "" {
		argv = append(argv, "--", p.Prompt)
	}
	// The profile's Claude Code settings (ADR-0059) are top-level keys of the run's user settings.json, set at every
	// launch; keys the profile no longer has are removed, and keys Claude Code keeps there itself stay.
	if problems := p.ClaudeSettings.Validate(); len(problems) > 0 {
		return Launch{}, errors.New("claude: invalid claude_settings: " + problems[0])
	}
	if gw != nil {
		if err := gatewayClaudeSettings(p.ClaudeSettings); err != nil {
			return Launch{}, err
		}
	}
	own := map[string]any{}
	for k, v := range p.ClaudeSettings {
		own[k] = v
	}
	// Without a "tui" setting Claude Code may pick its full-screen renderer (alternate screen, its own virtualized
	// scrollback; its fresh-install default once a config dir has started, or a server-side flag), as the operator's
	// runs did: the run's terminal would then never hold earlier output, so a watch-only viewer, who sends no input, could
	// not scroll back. The classic renderer writes to the normal screen and its scrollback. A profile that sets "tui"
	// (for example "fullscreen") decides itself (ADR-0064, verified with Claude Code 2.1.317).
	if _, set := own["tui"]; !set {
		own["tui"] = ClaudeTUI
	}
	denyTerminalPrompts(own)
	env := commonEnv(c)
	env["CLAUDE_CONFIG_DIR"] = cfg
	secrets := secretEnv(p.Secrets, "CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY")
	var remove []string
	if gw != nil {
		// The gateway is the only credential: the origin and the token go through the shim (not Spec.Env, which
		// herdr-core persists and the shim scrubs of ANTHROPIC_*), and a login stored in the config dir (a /login inside an
		// earlier run) is removed so Claude Code can't fall back to it.
		secrets = map[string]string{"ANTHROPIC_BASE_URL": gw.Origin, "ANTHROPIC_AUTH_TOKEN": gw.Token}
		remove = []string{filepath.Join(cfg, ".credentials.json")}
	}
	return Launch{
		Argv:  argv,
		Env:   env,
		Files: map[string][]byte{mcpPath: mcp},
		// A fresh CLAUDE_CONFIG_DIR stops at the first-run theme picker and then at the folder-trust prompt, which would
		// leave an unattended run waiting forever. SpaceMan provisions the run's working directory, so it marks first-run
		// as done and trusts exactly that directory, nothing else (ADR-0021, verified with Claude Code 2.1.316).
		MergeJSON: map[string]map[string]any{filepath.Join(cfg, ".claude.json"): {
			"hasCompletedOnboarding": true,
			"theme":                  "dark",
			"projects":               map[string]any{c.WorkDir: map[string]any{"hasTrustDialogAccepted": true}},
		}},
		OwnJSON: map[string]map[string]any{filepath.Join(cfg, "settings.json"): own},
		// Verified: Claude Code loads $CLAUDE_CONFIG_DIR/skills/<name>/SKILL.md (spike S-6).
		SkillsRoot:  filepath.Join(cfg, "skills"),
		SecretEnv:   secrets,
		RemoveFiles: remove,
		SessionID:   session,
	}, nil
}
