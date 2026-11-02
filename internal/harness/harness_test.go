package harness

import (
	"encoding/json"
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
