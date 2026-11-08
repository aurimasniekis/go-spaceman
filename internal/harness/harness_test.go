package harness

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

func ctxFor(p proto.LaunchPayload) Context {
	return Context{AgentBinary: "/opt/spaceman/spaceman-agent", BridgeSocket: "/run/sm/bridge.sock", ConfigDir: "/data/spaces/spc_a/instances/ins_1",
		WorkDir: "/data/spaces/spc_a/work", RunID: "run_1", SpaceID: "spc_a", RunDir: "/data/runs/run_1", Binary: "claude", Payload: p}
}

func TestClaudeBuild(t *testing.T) {
	c := ctxFor(proto.LaunchPayload{Prompt: "fix the bug", SystemText: "You are control.", Model: "claude-opus-5-5",
		Settings: map[string]string{"permission_mode": "acceptEdits"},
		Secrets:  map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "tok", "AWS_SECRET": "nope"}})
	l, err := Claude{}.Build(c)
	if err != nil {
		t.Fatal(err)
	}
	argv := strings.Join(l.Argv, " ")
	for _, want := range []string{"--session-id " + l.SessionID, "--mcp-config /data/runs/run_1/mcp.json --strict-mcp-config", "--model claude-opus-5-5",
		"--permission-mode acceptEdits", "--append-system-prompt You are control.", "-- fix the bug"} {
		if !strings.Contains(argv, want) {
			t.Errorf("argv %q missing %q", argv, want)
		}
	}
	if !reUUID.MatchString(l.SessionID) {
		t.Fatalf("session id %q", l.SessionID)
	}
	if l.Env["CLAUDE_CONFIG_DIR"] != "/data/spaces/spc_a/instances/ins_1/claude" || l.Env["HALLPASS_SESSION"] != "run_1" {
		t.Fatalf("env %v", l.Env)
	}
	for k, v := range l.Env {
		if strings.Contains(v, "tok") && k != "TERM" {
			t.Fatalf("secret leaked into runner env: %s", k)
		}
	}
	if len(l.SecretEnv) != 1 || l.SecretEnv["CLAUDE_CODE_OAUTH_TOKEN"] != "tok" {
		t.Fatalf("secret env %v (unrelated secrets must be dropped)", l.SecretEnv)
	}
	var mcp map[string]map[string]map[string]any
	if err := json.Unmarshal(l.Files["/data/runs/run_1/mcp.json"], &mcp); err != nil || mcp["mcpServers"]["spaceman"]["command"] != "/opt/spaceman/spaceman-agent" {
		t.Fatalf("mcp config %s", l.Files["/data/runs/run_1/mcp.json"])
	}
	if l.SkillsRoot != "/data/spaces/spc_a/instances/ins_1/claude/skills" {
		t.Fatalf("skills root %q", l.SkillsRoot)
	}
	// First-run state is seeded and exactly the run's working directory is trusted (ADR-0021).
	seed := l.MergeJSON["/data/spaces/spc_a/instances/ins_1/claude/.claude.json"]
	projects, _ := seed["projects"].(map[string]any)
	if len(l.MergeJSON) != 1 || seed["hasCompletedOnboarding"] != true || len(projects) != 1 ||
		projects["/data/spaces/spc_a/work"].(map[string]any)["hasTrustDialogAccepted"] != true {
		t.Fatalf("claude state %v", l.MergeJSON)
	}
	// The profile's settings.json keys are owned at every launch (ADR-0059), plus the classic renderer when the profile
	// sets none, so the terminal keeps a scrollback a watch-only viewer can scroll (ADR-0064).
	cfgSettings := "/data/spaces/spc_a/instances/ins_1/claude/settings.json"
	if keys, ok := l.OwnJSON[cfgSettings]; !ok || len(keys) != 1 || keys["tui"] != "default" {
		t.Fatalf("settings keys without settings %v", l.OwnJSON)
	}
	cs := proto.ClaudeSettings{"permissions": map[string]any{"deny": []any{"Read(./secrets/**)"}}, "hooks": map[string]any{"Stop": []any{}}}
	l, err = Claude{}.Build(ctxFor(proto.LaunchPayload{ClaudeSettings: cs}))
	if err != nil {
		t.Fatal(err)
	}
	if keys := l.OwnJSON[cfgSettings]; len(keys) != 3 || keys["hooks"] == nil || keys["permissions"] == nil || keys["tui"] != "default" {
		t.Fatalf("settings keys %v", l.OwnJSON)
	}
	// Every profile key is written unchanged, and the profile's own settings are not modified.
	for k, v := range cs {
		if !reflect.DeepEqual(l.OwnJSON[cfgSettings][k], v) {
			t.Fatalf("settings key %s changed: %v", k, l.OwnJSON[cfgSettings][k])
		}
	}
	if _, set := cs["tui"]; set || len(cs) != 2 {
		t.Fatal("the profile's settings were changed")
	}
	// A profile that picks a renderer keeps it, whichever it is.
	for _, tui := range []any{"fullscreen", "default"} {
		prof := proto.ClaudeSettings{"tui": tui, "model": "opus"}
		l, err = Claude{}.Build(ctxFor(proto.LaunchPayload{ClaudeSettings: prof}))
		if err != nil || !reflect.DeepEqual(l.OwnJSON[cfgSettings], map[string]any{"tui": tui, "model": "opus"}) {
			t.Fatalf("profile renderer %v: %v %v", tui, l.OwnJSON, err)
		}
	}
	if _, err := (Claude{}).Build(ctxFor(proto.LaunchPayload{ClaudeSettings: proto.ClaudeSettings{"permissions": "all"}})); err == nil {
		t.Error("invalid claude settings accepted")
	}
	// Resume uses the given session.
	l, err = Claude{}.Build(ctxFor(proto.LaunchPayload{SessionID: "0b5e3c1e-1111-4222-8333-444455556666", Resume: true}))
	if err != nil || !slices.Contains(l.Argv, "--resume") || slices.Contains(l.Argv, "--session-id") {
		t.Fatalf("resume argv %v %v", l.Argv, err)
	}
	for _, bad := range []proto.LaunchPayload{
		{SessionID: "not-a-uuid", Resume: true},
		{Model: "x; rm -rf /"},
		{Settings: map[string]string{"permission_mode": "yolo"}},
	} {
		if _, err := (Claude{}).Build(ctxFor(bad)); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	if _, err := (Claude{}).Build(Context{}); err == nil {
		t.Error("incomplete context accepted")
	}
}

func TestCodexBuild(t *testing.T) {
	c := ctxFor(proto.LaunchPayload{Prompt: "add tests", SystemText: "Standing rules.", Settings: map[string]string{"sandbox": "workspace-write", "approval": "on-request"},
		Secrets: map[string]string{"OPENAI_API_KEY": "sk", "CLAUDE_CODE_OAUTH_TOKEN": "other"}})
	c.Binary = "codex"
	l, err := Codex{}.Build(c)
	if err != nil {
		t.Fatal(err)
	}
	if l.Argv[1] != "--no-daemon" {
		t.Fatalf("--no-daemon must come first: %v", l.Argv)
	}
	argv := strings.Join(l.Argv, " ")
	for _, want := range []string{"-c features.shell_snapshot=false", `mcp_servers.spaceman.command="/opt/spaceman/spaceman-agent"`, `mcp_servers.spaceman.args=["mcp-bridge", "--run", "run_1", "--socket", "/run/sm/bridge.sock"]`,
		"-s workspace-write", "-a on-request", "-- Standing rules.\n\nadd tests"} {
		if !strings.Contains(argv, want) {
			t.Errorf("argv %q missing %q", argv, want)
		}
	}
	if l.Env["CODEX_HOME"] != "/data/spaces/spc_a/instances/ins_1/codex" || len(l.SecretEnv) != 1 || l.SecretEnv["OPENAI_API_KEY"] != "sk" {
		t.Fatalf("env %v secrets %v", l.Env, l.SecretEnv)
	}
	if !reflect.DeepEqual(l.RemoveDirs, []string{"/data/spaces/spc_a/instances/ins_1/codex/shell_snapshots"}) {
		t.Fatalf("remove dirs %v", l.RemoveDirs)
	}
	// Interactive Codex takes an API key only from a stored login (ADR-0054): the key becomes auth.json in API-key mode.
	if got := string(l.SecretFiles["/data/spaces/spc_a/instances/ins_1/codex/auth.json"]); got != `{"OPENAI_API_KEY":"sk","auth_mode":"apikey"}` || len(l.SecretFiles) != 1 {
		t.Fatalf("secret files %v", l.SecretFiles)
	}
	// Skills load from $CODEX_HOME/skills (codex-cli 0.166.0); an API-key launch has no login but still names where a
	// login would be linked, so links left by an earlier ChatGPT login are removed.
	if l.SkillsRoot != "/data/spaces/spc_a/instances/ins_1/codex/skills" || l.Login != nil ||
		l.LoginLinks["/data/spaces/spc_a/instances/ins_1/codex/auth.json"] != "auth.json" || l.LoginLinks["/data/spaces/spc_a/instances/ins_1/codex/auth.lock"] != "auth.lock" {
		t.Fatalf("skills %q login %+v links %v", l.SkillsRoot, l.Login, l.LoginLinks)
	}
	// A ChatGPT login (ADR-0053) becomes the node's shared login file, never an environment variable.
	cl := ctxFor(proto.LaunchPayload{AccountRef: "acc_plan", Secrets: map[string]string{CodexAuthKey: `{"auth_mode":"chatgpt"}`}})
	cl.Binary = "codex"
	l, err = Codex{}.Build(cl)
	if err != nil || l.Login == nil || l.Login.Account != "acc_plan" || string(l.Login.Seed) != `{"auth_mode":"chatgpt"}` || len(l.SecretEnv) != 0 || len(l.SecretFiles) != 0 {
		t.Fatalf("ChatGPT login %+v secrets %v %v", l.Login, l.SecretEnv, err)
	}
	if _, err := (Codex{}).Build(func() Context {
		c := ctxFor(proto.LaunchPayload{Secrets: map[string]string{CodexAuthKey: "{}"}})
		c.Binary = "codex"
		return c
	}()); err == nil {
		t.Fatal("a login without its account was accepted")
	}
	l, err = Codex{}.Build(func() Context {
		c := ctxFor(proto.LaunchPayload{Resume: true, SessionID: "019a2b3c-4d5e-7f00"})
		c.Binary = "codex"
		return c
	}())
	if err != nil || !strings.Contains(strings.Join(l.Argv, " "), "resume 019a2b3c-4d5e-7f00") {
		t.Fatalf("resume %v %v", l.Argv, err)
	}
	for _, bad := range []proto.LaunchPayload{{Resume: true}, {Settings: map[string]string{"sandbox": "root"}}, {Settings: map[string]string{"approval": "always"}}} {
		if _, err := (Codex{}).Build(ctxFor(bad)); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

func TestTOMLString(t *testing.T) {
	if got := tomlString("a\"b\\c\n"); got != `"a\"b\\c\n"` {
		t.Fatal(got)
	}
}

func TestRegistry(t *testing.T) {
	for _, k := range []string{KindClaude, KindCodex} {
		h, err := Get(k)
		if err != nil || h.Kind() != k {
			t.Fatal(k, err)
		}
	}
	if _, err := Get("opencode"); err == nil {
		t.Fatal("unimplemented harness accepted")
	}
}

// SM-HARN-003/004, SM-INV-014: each adapter advertises exactly the capabilities verified for it. SpaceMan starts every
// run as a standalone session, a topology in which Claude Code's native team messaging (SendMessage) does not reach
// other runs (docs/contracts/harnesses.md), so no adapter advertises messaging.native_team and runs talk only through
// SpaceMan's durable messages. A harness that is not installed advertises nothing.
func TestCapabilitiesAreExplicit(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "harness")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho 'fake 9.8.7'\n"), 0o700); err != nil { //nolint:gosec // test stub must be executable
		t.Fatal(err)
	}
	want := map[string][]string{
		KindClaude: {CapFixedSessionID, CapResume, CapMCPConfig, CapProgramStatus, CapSafeInput, CapSystemPrompt, CapTranscripts, CapSkills},
		KindCodex:  {CapResume, CapMCPConfig, CapProgramStatus, CapSafeInput, CapTranscripts, CapSkills},
	}
	for kind, caps := range want {
		h, err := Get(kind)
		if err != nil {
			t.Fatal(err)
		}
		info := h.Detect(context.Background(), bin)
		if !info.Available || info.Version != "9.8.7" || !slices.Equal(info.Capabilities, caps) {
			t.Fatalf("%s: %+v", kind, info)
		}
		if slices.Contains(info.Capabilities, CapNativeTeam) {
			t.Fatalf("%s advertises native team messaging", kind)
		}
		missing := h.Detect(context.Background(), filepath.Join(t.TempDir(), "absent"))
		if missing.Available || len(missing.Capabilities) != 0 || missing.Error != "not installed" {
			t.Fatalf("%s not installed: %+v", kind, missing)
		}
	}
}

func gatewayPayload(p proto.LaunchPayload) proto.LaunchPayload {
	p.Gateway = &proto.GatewayLaunch{Kind: proto.GatewayHarborPilot, BaseURL: "https://hp.example.com/"}
	if p.Secrets == nil {
		p.Secrets = map[string]string{proto.SecretGatewayToken: "hpk_child_1"}
	}
	return p
}

// SM-GW-003 (ADR-0060 decision 6): a Claude Code gateway run gets exactly ANTHROPIC_BASE_URL and ANTHROPIC_AUTH_TOKEN,
// only through the shim's secrets (never the runner env), its stored login is removed, and the ADR-0021 first-run seed
// stays. Another credential next to the gateway's, or a settings key that adds one, refuses the launch.
func TestClaudeGatewayBuild(t *testing.T) {
	c := ctxFor(gatewayPayload(proto.LaunchPayload{Prompt: "go", SessionID: "11111111-2222-4333-8444-555555555555",
		ClaudeSettings: proto.ClaudeSettings{"env": map[string]any{"FOO": "bar"}}}))
	l, err := Claude{}.Build(c)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"ANTHROPIC_BASE_URL": "https://hp.example.com", "ANTHROPIC_AUTH_TOKEN": "hpk_child_1"}
	if !reflect.DeepEqual(l.SecretEnv, want) {
		t.Fatalf("secret env %v", l.SecretEnv)
	}
	for k, v := range l.Env {
		if strings.HasPrefix(k, "ANTHROPIC_") || strings.Contains(v, "hpk_child_1") || strings.Contains(v, "hp.example.com") {
			t.Fatalf("gateway env in the runner env: %s", k)
		}
	}
	if strings.Contains(strings.Join(l.Argv, " "), "hpk_child_1") {
		t.Fatal("the token is in argv")
	}
	if !reflect.DeepEqual(l.RemoveFiles, []string{"/data/spaces/spc_a/instances/ins_1/claude/.credentials.json"}) {
		t.Fatalf("remove %v", l.RemoveFiles)
	}
	if l.SessionID != "11111111-2222-4333-8444-555555555555" || l.MergeJSON["/data/spaces/spc_a/instances/ins_1/claude/.claude.json"]["hasCompletedOnboarding"] != true {
		t.Fatalf("session %q seed %v", l.SessionID, l.MergeJSON)
	}

	bad := map[string]proto.LaunchPayload{
		"another credential":   gatewayPayload(proto.LaunchPayload{Secrets: map[string]string{proto.SecretGatewayToken: "hpk", "CLAUDE_CODE_OAUTH_TOKEN": "x"}}),
		"another credential ":  gatewayPayload(proto.LaunchPayload{Secrets: map[string]string{proto.SecretGatewayToken: "hpk", "ANTHROPIC_API_KEY": "x"}}),
		"carries no token":     gatewayPayload(proto.LaunchPayload{Secrets: map[string]string{}}),
		"can't name an accoun": gatewayPayload(proto.LaunchPayload{AccountRef: "acc_1"}),
		"apiKeyHelper":         gatewayPayload(proto.LaunchPayload{ClaudeSettings: proto.ClaudeSettings{"apiKeyHelper": "/bin/key"}}),
		"env.ANTHROPIC_BASE_URL": gatewayPayload(proto.LaunchPayload{ClaudeSettings: proto.ClaudeSettings{
			"env": map[string]any{"ANTHROPIC_BASE_URL": "https://elsewhere.example.com"}}}),
		"env.CLAUDE_CODE_USE_BEDROCK": gatewayPayload(proto.LaunchPayload{ClaudeSettings: proto.ClaudeSettings{"env": map[string]any{"CLAUDE_CODE_USE_BEDROCK": "1"}}}),
		"plain http":                  {Gateway: &proto.GatewayLaunch{Kind: proto.GatewayHarborPilot, BaseURL: "http://127.0.0.1:4000"}, Secrets: map[string]string{proto.SecretGatewayToken: "hpk"}},
		"without the gateway's addr":  {Secrets: map[string]string{proto.SecretGatewayToken: "hpk"}},
	}
	for want, p := range bad {
		if _, err := (Claude{}).Build(ctxFor(p)); err == nil || !strings.Contains(err.Error(), strings.TrimSpace(want)) {
			t.Errorf("%s: %v", want, err)
		}
		cx := ctxFor(p)
		cx.Binary = "codex"
		if strings.HasPrefix(want, "env.") || want == "apiKeyHelper" {
			continue // Claude Code settings
		}
		if _, err := (Codex{}).Build(cx); err == nil {
			t.Errorf("codex %s: accepted", want)
		}
	}
	// Plain http only when the connection allows it.
	p := gatewayPayload(proto.LaunchPayload{})
	p.Gateway.BaseURL, p.Gateway.AllowInsecureHTTP = "http://127.0.0.1:4000", true
	if l, err := (Claude{}).Build(ctxFor(p)); err != nil || l.SecretEnv["ANTHROPIC_BASE_URL"] != "http://127.0.0.1:4000" {
		t.Fatalf("insecure allowed: %v %v", l.SecretEnv["ANTHROPIC_BASE_URL"], err)
	}
}

// HP-T4 review F2: a gateway run's Claude Code settings env may set only the allowlisted CLAUDE_CODE_* switches; every
// other CLAUDE_CODE_* (credentials, endpoints, proxies, client certificates, new ones), every ANTHROPIC_* and the
// proxy and TLS variables in either case are refused.
func TestClaudeGatewayEnv(t *testing.T) {
	refused := []string{
		"ANTHROPIC_BASE_URL", "ANTHROPIC_API_KEY", "ANTHROPIC_MODEL",
		"CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_API_BASE_URL", "CLAUDE_CODE_HOST_AUTH_ENV_VAR",
		"CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST", "CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR",
		"CLAUDE_CODE_GATEWAY_TOKEN_FILE_DESCRIPTOR", "CLAUDE_CODE_ENABLE_PROXY_AUTH_HELPER", "CLAUDE_CODE_PROXY_URL",
		"CLAUDE_CODE_HTTPS_PROXY", "CLAUDE_CODE_CLIENT_CERT", "CLAUDE_CODE_CLIENT_KEY", "CLAUDE_CODE_SOMETHING_NEW",
		"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy",
		"NODE_TLS_REJECT_UNAUTHORIZED", "NODE_EXTRA_CA_CERTS", "SSL_CERT_FILE", "NODE_OPTIONS",
	}
	for _, k := range refused {
		p := gatewayPayload(proto.LaunchPayload{ClaudeSettings: proto.ClaudeSettings{"env": map[string]any{"FOO": "1", k: "x"}}})
		if _, err := (Claude{}).Build(ctxFor(p)); err == nil || !strings.Contains(err.Error(), "env."+k+" ") {
			t.Errorf("%s: %v", k, err)
		}
	}
	allowed := map[string]any{"FOO": "1", "NO_PROXY": "example.com", "CLAUDE_CODE_MAX_OUTPUT_TOKENS": "8000",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1", "CLAUDE_CODE_NO_FLICKER": "1"}
	if _, err := (Claude{}).Build(ctxFor(gatewayPayload(proto.LaunchPayload{ClaudeSettings: proto.ClaudeSettings{"env": allowed}}))); err != nil {
		t.Fatal(err)
	}
	// Without a gateway nothing is refused: a direct account's profile keeps its env.
	if _, err := (Claude{}).Build(ctxFor(proto.LaunchPayload{ClaudeSettings: proto.ClaudeSettings{"env": map[string]any{"HTTPS_PROXY": "http://p"}}})); err != nil {
		t.Fatal(err)
	}
}

// SM-GW-003 (ADR-0060 decision 6, the HP-T1 spike's shape): a Codex gateway run gets the five model-provider
// overrides and HARBOR_PILOT_TOKEN through the shim only, no auth.json (any left is removed) and no login.
func TestCodexGatewayBuild(t *testing.T) {
	c := ctxFor(gatewayPayload(proto.LaunchPayload{Prompt: "go", Model: "coding/default", Settings: map[string]string{"sandbox": "read-only"}}))
	c.Binary = "codex"
	l, err := Codex{}.Build(c)
	if err != nil {
		t.Fatal(err)
	}
	argv := strings.Join(l.Argv, " ")
	for _, want := range []string{
		`-c model_provider="harbor-pilot"`,
		`-c model_providers.harbor-pilot.name="Harbor Pilot"`,
		`-c model_providers.harbor-pilot.base_url="https://hp.example.com/v1"`,
		`-c model_providers.harbor-pilot.wire_api="responses"`,
		`-c model_providers.harbor-pilot.env_key="HARBOR_PILOT_TOKEN"`,
		"-m coding/default", "-s read-only", "-- go",
	} {
		if !strings.Contains(argv, want) {
			t.Errorf("argv %q missing %q", argv, want)
		}
	}
	if l.Argv[1] != "--no-daemon" || strings.Index(argv, "model_provider=") > strings.Index(argv, "-- go") {
		t.Fatalf("argv order %v", l.Argv)
	}
	if strings.Contains(argv, "hpk_child_1") {
		t.Fatal("the token is in argv")
	}
	if !reflect.DeepEqual(l.SecretEnv, map[string]string{"HARBOR_PILOT_TOKEN": "hpk_child_1"}) {
		t.Fatalf("secret env %v", l.SecretEnv)
	}
	for k, v := range l.Env {
		if strings.Contains(v, "hpk_child_1") || k == "HARBOR_PILOT_TOKEN" {
			t.Fatalf("token in the runner env: %s", k)
		}
	}
	home := "/data/spaces/spc_a/instances/ins_1/codex"
	if len(l.SecretFiles) != 0 || l.Login != nil || !reflect.DeepEqual(l.RemoveFiles, []string{home + "/auth.json"}) ||
		l.LoginLinks[home+"/auth.json"] != "auth.json" || l.LoginLinks[home+"/auth.lock"] != "auth.lock" {
		t.Fatalf("secret files %v login %+v remove %v links %v", l.SecretFiles, l.Login, l.RemoveFiles, l.LoginLinks)
	}
	if !reflect.DeepEqual(l.RemoveDirs, []string{home + "/shell_snapshots"}) || !strings.Contains(argv, "-c features.shell_snapshot=false") {
		t.Fatalf("snapshots: remove dirs %v argv %q", l.RemoveDirs, argv)
	}
	for _, secrets := range []map[string]string{
		{proto.SecretGatewayToken: "hpk", "OPENAI_API_KEY": "sk"},
		{proto.SecretGatewayToken: "hpk", CodexAuthKey: "{}"},
	} {
		cx := ctxFor(gatewayPayload(proto.LaunchPayload{Secrets: secrets}))
		cx.Binary = "codex"
		if _, err := (Codex{}).Build(cx); err == nil || !strings.Contains(err.Error(), "another credential") {
			t.Errorf("%v: %v", keysOf(secrets), err)
		}
	}
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
