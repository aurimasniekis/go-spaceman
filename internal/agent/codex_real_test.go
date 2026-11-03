//go:build real

package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/harness"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// The installed Codex reads what the node lays out for it (SM-MCP-004, SM-ACCT-002, ADR-0053), without a credential or a
// model call: a skill materialized into $CODEX_HOME/skills appears in the model's prompt input, and a login file
// linked into $CODEX_HOME through the node's shared login folder is the one Codex uses. The login here is an API-key
// file with a fake key (a ChatGPT login cannot be made without an account); the linking is the same. Run:
// go test -tags real -run TestRealCodexReadsNodeLayout ./internal/agent/
func TestRealCodexReadsNodeLayout(t *testing.T) {
	bin, err := exec.LookPath("codex")
	if err != nil {
		t.Skip("codex not installed")
	}
	data := t.TempDir()
	cfg := filepath.Join(data, "spaces", "s", "instances", "i", "codex-config")
	c := harness.Context{AgentBinary: "/bin/true", BridgeSocket: "/tmp/none.sock", ConfigDir: cfg, WorkDir: filepath.Join(data, "work"), RunID: "run_real",
		SpaceID: "spc_real", RunDir: filepath.Join(data, "run"), Binary: bin,
		Payload: proto.LaunchPayload{AccountRef: "acc_real", Secrets: map[string]string{harness.CodexAuthKey: `{"auth_mode":"apikey","OPENAI_API_KEY":"sk-spaceman-real-test-not-a-key"}`}}}
	l, err := harness.Codex{}.Build(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(c.WorkDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := syncSkills(l.SkillsRoot, []proto.Skill{{Name: "refunds-runbook", Version: 1, SHA256: "x",
		Files: map[string]string{"SKILL.md": "---\nname: refunds-runbook\ndescription: Use when a refund is stuck in pending.\n---\n\n# Refunds runbook\n"}}}); err != nil {
		t.Fatal(err)
	}
	a := &Agent{o: Options{DataDir: data}}
	if err := a.linkLogin(l.Login, l.LoginLinks); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, args...) //nolint:gosec // the test's own binary and arguments
		cmd.Dir = c.WorkDir
		cmd.Env = []string{"HOME=" + filepath.Join(data, "home"), "PATH=" + os.Getenv("PATH"), "CODEX_HOME=" + l.Env["CODEX_HOME"]}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("codex %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	if out := run("debug", "prompt-input", "hi"); !strings.Contains(out, "refunds-runbook: Use when a refund is stuck in pending.") {
		t.Fatalf("the materialized skill is not in the prompt input:\n%.2000s", out)
	}
	if out := run("login", "status"); !strings.Contains(out, "API key configured") {
		t.Fatalf("codex did not read the linked login: %s", out)
	}
	if fi, err := os.Lstat(filepath.Join(l.Env["CODEX_HOME"], "auth.json")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("auth.json is not the node's link: %v %v", fi, err)
	}
}
