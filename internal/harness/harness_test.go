package harness

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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
	for _, want := range []string{`mcp_servers.spaceman.command="/opt/spaceman/spaceman-agent"`, `mcp_servers.spaceman.args=["mcp-bridge", "--run", "run_1", "--socket", "/run/sm/bridge.sock"]`,
		"-s workspace-write", "-a on-request", "-- Standing rules.\n\nadd tests"} {
		if !strings.Contains(argv, want) {
			t.Errorf("argv %q missing %q", argv, want)
		}
	}
	if l.Env["CODEX_HOME"] != "/data/spaces/spc_a/instances/ins_1/codex" || len(l.SecretEnv) != 1 || l.SecretEnv["OPENAI_API_KEY"] != "sk" {
		t.Fatalf("env %v secrets %v", l.Env, l.SecretEnv)
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
	if err != nil || l.Login == nil || l.Login.Account != "acc_plan" || string(l.Login.Seed) != `{"auth_mode":"chatgpt"}` || len(l.SecretEnv) != 0 {
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
