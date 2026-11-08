package harness

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"

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

// ClaudePermissionHookArgs are the spaceman-agent arguments of the PermissionRequest hook every run gets (ADR-0068).
var ClaudePermissionHookArgs = []string{"hook", "permission-request"}

// ClaudePermissionHookCommand is the shell command of SpaceMan's PermissionRequest hook. It fails closed: when the agent
// binary is missing, crashes or exits non-zero, a fixed deny (ClaudeStaticPermissionDeny) is printed instead, because
// Claude Code shows the dialog for a failed hook (exit code 2 does not deny this event), and the run would stall there.
// A hook that runs past its timeout still ends at the dialog (ADR-0068).
func ClaudePermissionHookCommand(agentBinary string) string {
	command := shellQuote(agentBinary)
	for _, a := range ClaudePermissionHookArgs {
		command += " " + a
	}
	return command + " 2>/dev/null || printf '%s' " + shellQuote(ClaudeStaticPermissionDeny())
}

// denyPermissionDialogs appends SpaceMan's PermissionRequest hook to the run's hooks. Claude Code still shows a
// permission dialog in auto mode for an "ask" rule, for a read outside the working directories and when the classifier
// falls back to asking after repeated blocks; nobody answers it in an unattended run, so the run would stall. The hook
// (spaceman-agent hook permission-request, shipped with the node agent) answers every such dialog with a deny whose
// message points the model at spaceman_human_ask. It can only deny, never allow. The profile's own hooks, of every
// event, come first and are kept; the entry is not added twice. A hooks value that is not an object, or a
// PermissionRequest value that is not a list, is not valid Claude Code settings and is replaced. The profile's settings
// map is never modified: the hooks object is copied.
func denyPermissionDialogs(own map[string]any, agentBinary string) {
	command := ClaudePermissionHookCommand(agentBinary)
	hooks := map[string]any{}
	if cur, ok := own["hooks"].(map[string]any); ok {
		for k, v := range cur {
			hooks[k] = v
		}
	}
	var groups []any
	switch l := hooks["PermissionRequest"].(type) {
	case []any:
		groups = append(groups, l...)
	case []map[string]any:
		for _, g := range l {
			groups = append(groups, g)
		}
	}
	for _, g := range groups {
		if hasHookCommand(g, command) {
			own["hooks"] = hooks
			return
		}
	}
	groups = append(groups, map[string]any{"matcher": "*", "hooks": []any{map[string]any{"type": "command", "command": command, "timeout": 30}}})
	hooks["PermissionRequest"] = groups
	own["hooks"] = hooks
}

// hasHookCommand reports whether a hooks matcher group already runs command.
func hasHookCommand(group any, command string) bool {
	g, ok := group.(map[string]any)
	if !ok {
		return false
	}
	var hs []any
	switch l := g["hooks"].(type) {
	case []any:
		hs = l
	case []map[string]any:
		for _, h := range l {
			hs = append(hs, h)
		}
	}
	for _, h := range hs {
		if m, ok := h.(map[string]any); ok && m["type"] == "command" && m["command"] == command {
			return true
		}
	}
	return false
}

// ClaudeSpaceCredentialFiles are the harness login files a space folder can hold, in its instances' config dirs
// (relative to the space folder, Claude Code permission-rule globs). The own-space directory a run gets would let its
// file tools read them without a dialog, so they are denied (ADR-0068).
var ClaudeSpaceCredentialFiles = []string{"instances/*/claude/.credentials.json", "instances/*/codex/auth.json"}

// ClaudeSpaceProtectedFiles are the files of every instance's config dirs (the run's own included) that configure the
// harness: settings that can run commands at the next launch (hooks, helpers, env), instructions it loads (CLAUDE.md,
// agents, commands, skills, output styles), plugins, and Codex's whole config dir. The run's file tools get Edit deny
// rules for them, so a run can't plant settings in another instance, or in its own, without a dialog (ADR-0068, T-40).
// They are listed one by one rather than as instances/** because the run's own Claude Code writes its auto memory under
// its own config dir (projects/*/memory) with the same file tools, and a deny rule wins over that (verified with Claude
// Code 2.1.317); Claude Code itself writes these files without its tools, so the rules don't stop it.
var ClaudeSpaceProtectedFiles = []string{
	"instances/*/claude/settings.json", "instances/*/claude/settings.local.json", "instances/*/claude/settings.json.spaceman-keys",
	"instances/*/claude/.claude.json", "instances/*/claude/CLAUDE.md", "instances/*/claude/agents/**", "instances/*/claude/commands/**",
	"instances/*/claude/skills/**", "instances/*/claude/plugins/**", "instances/*/claude/output-styles/**", "instances/*/claude/hooks/**",
	"instances/*/codex/**",
}

// MaxInstanceDenyRules caps the per-instance Edit deny rules a run gets: with more other instances than this, it gets
// none of them, only the rules for every instance's harness configuration (ClaudeSpaceProtectedFiles) and the key
// stripping at launch, so other instances' memory and transcripts are then writable (ADR-0068, T-40).
const MaxInstanceDenyRules = 50

// globUnsafe are the characters a path must not hold to be used in a Claude Code permission-rule glob: escaping them is
// not documented, so a space folder or instance name with one gets no directory at all (N2).
const globUnsafe = "*?[]{}\\!"

// addSpaceDirectory adds the run's own space folder to permissions.additionalDirectories, so reading the space's files
// (plans, notes, other checkouts of the space) from a run that starts in a checkout does not end in a "read outside the
// working directories" dialog. Only that space's folder is added, never the node's whole spaces folder. Deny rules keep
// the run's file tools away from the instances' config dirs: Read and Edit for the login files
// (ClaudeSpaceCredentialFiles), Edit for the harness configuration of every instance (ClaudeSpaceProtectedFiles) and
// Edit for the whole folder of every other instance that exists at launch (instances, listing the space's instances
// folder, without the run's own, configDir's name): their settings, memory and transcripts; at most
// MaxInstanceDenyRules of them, else none. A space folder holding a
// glob character gets no directory and no rules, since the rules could not be written exactly; a folder in instances
// named with one is not an instance and is skipped. The
// profile's directories and deny rules come first and are kept, duplicates are dropped and every other permissions key
// is kept; the profile's settings map is never modified (ADR-0068).
func addSpaceDirectory(own map[string]any, spaceDir, configDir string, instances []string) {
	if spaceDir == "" || strings.ContainsAny(spaceDir, globUnsafe) {
		return
	}
	self := filepath.Base(configDir)
	var others []string
	for _, id := range instances {
		// SpaceMan's instance IDs never hold a glob character; a folder named like that is not an instance (a run may
		// have made it), so it is skipped rather than costing every later launch its directory.
		if id == "" || id == "." || id == ".." || strings.ContainsAny(id, globUnsafe+"/") {
			continue
		}
		if id != self && !slices.Contains(others, id) {
			others = append(others, id)
		}
	}
	slices.Sort(others)
	if len(others) > MaxInstanceDenyRules {
		// A space with very many instances would get a settings file that grows with each one: the named configuration
		// paths and the key stripping at launch still cover every instance's settings (T-40).
		others = nil
	}
	perms := map[string]any{}
	if cur, ok := own["permissions"].(map[string]any); ok {
		for k, v := range cur {
			perms[k] = v
		}
	}
	var dirs []string
	for _, d := range append(stringsOf(perms["additionalDirectories"]), spaceDir) {
		if !slices.Contains(dirs, d) {
			dirs = append(dirs, d)
		}
	}
	perms["additionalDirectories"] = dirs
	deny := stringsOf(perms["deny"])
	add := func(tool, rel string) {
		// "//" starts an absolute path in a Claude Code permission rule.
		if rule := tool + "(/" + filepath.ToSlash(filepath.Join(spaceDir, rel)) + ")"; !slices.Contains(deny, rule) {
			deny = append(deny, rule)
		}
	}
	for _, f := range ClaudeSpaceCredentialFiles {
		add("Read", f)
		add("Edit", f)
	}
	for _, f := range ClaudeSpaceProtectedFiles {
		add("Edit", f)
	}
	for _, id := range others {
		add("Edit", "instances/"+id+"/**")
	}
	perms["deny"] = deny
	own["permissions"] = perms
}

// ClaudeUnsafeSettingsKeys are the top-level Claude Code settings (path.Match patterns) that run commands, add
// credentials or environment, change what hooks, plugins, the sandbox or the auto-mode classifier allow, or reroute
// the harness: only the profile or SpaceMan may define them, so the node removes any it did not set from the run's
// settings.json at every launch (Launch.StripJSON, ADR-0068). Keys Claude Code keeps there itself (feedbackSurveyState,
// theme choices and the like) are not among them and stay. Checked against Claude Code 2.1.317's settings schema.
var ClaudeUnsafeSettingsKeys = []string{
	"disableAllHooks", "hooks", "env", "*Helper", "*Helpers", "awsAuthRefresh", "awsCredentialExport", "gcpAuthRefresh",
	"processWrapper", "statusLine", "subagentStatusLine", "fileSuggestion", "sshHostCommands", "permissions", "autoMode",
	"sandbox", "enabledPlugins", "extraKnownMarketplaces", "pluginConfigs", "enableAllProjectMcpServers",
	"enabledMcpjsonServers", "allowedHttpHookUrls", "httpHookAllowedEnvVars", "autoMemoryDirectory",
}

// shellQuote quotes s for the shell Claude Code runs a command hook with.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

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
	denyPermissionDialogs(own, c.AgentBinary)
	addSpaceDirectory(own, c.SpaceDir, c.ConfigDir, c.SpaceInstances)
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
		OwnJSON:   map[string]map[string]any{filepath.Join(cfg, "settings.json"): own},
		StripJSON: map[string][]string{filepath.Join(cfg, "settings.json"): ClaudeUnsafeSettingsKeys},
		// Verified: Claude Code loads $CLAUDE_CONFIG_DIR/skills/<name>/SKILL.md (spike S-6).
		SkillsRoot:  filepath.Join(cfg, "skills"),
		SecretEnv:   secrets,
		RemoveFiles: remove,
		SessionID:   session,
	}, nil
}
