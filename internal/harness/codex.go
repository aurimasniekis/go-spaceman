package harness

import (
	"context"
	"errors"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// Codex is the Codex CLI adapter (verified against codex-cli 0.163.0, docs/contracts/harnesses.md).
//
//   - isolation: CODEX_HOME=<config>/codex per space and instance
//   - --no-daemon: required under herdr-core, otherwise commands run outside hc (hc manual)
//   - MCP: -c mcp_servers.spaceman.command=… -c mcp_servers.spaceman.args=[…] (TOML overrides)
//   - session: Codex chooses its own session ID; resume is `codex resume <id>` (the ID is discovered from
//     $CODEX_HOME/sessions after launch — not a fixed-ID capability)
//   - auth: OPENAI_API_KEY / CODEX_API_KEY through the launcher shim, or a ChatGPT login: $CODEX_HOME/auth.json and
//     auth.lock linked to the node's live copy of the account's login, which Codex refreshes in place (ADR-0053)
//   - skills: $CODEX_HOME/skills/<name>/SKILL.md (verified with codex-cli 0.166.0 `debug prompt-input`)
type Codex struct{}

// Kind implements Harness.
func (Codex) Kind() string { return KindCodex }

// Detect implements Harness.
func (Codex) Detect(ctx context.Context, binary string) proto.HarnessInfo {
	return detect(ctx, KindCodex, binary, []string{CapResume, CapMCPConfig, CapProgramStatus, CapSafeInput, CapTranscripts, CapSkills})
}

var reCodexSession = regexp.MustCompile(`^[0-9A-Za-z-]{8,64}$`)

var codexSandbox = map[string]bool{"read-only": true, "workspace-write": true, "danger-full-access": true}
var codexApproval = map[string]bool{"on-request": true, "never": true, "untrusted": true, "on-failure": true}

// Build implements Harness.
func (Codex) Build(c Context) (Launch, error) {
	if err := checkCommon(c); err != nil {
		return Launch{}, err
	}
	p := c.Payload
	home := filepath.Join(c.ConfigDir, "codex")
	args := make([]string, 0, len(bridgeArgs(c)))
	for _, a := range bridgeArgs(c) {
		args = append(args, tomlString(a))
	}
	argv := []string{c.Binary, "--no-daemon",
		"-c", "mcp_servers.spaceman.command=" + tomlString(c.AgentBinary),
		"-c", "mcp_servers.spaceman.args=[" + strings.Join(args, ", ") + "]",
	}
	if p.Model != "" {
		argv = append(argv, "-m", p.Model)
	}
	if s := p.Settings["sandbox"]; s != "" {
		if !codexSandbox[s] {
			return Launch{}, errors.New("codex: unknown sandbox mode")
		}
		argv = append(argv, "-s", s)
	}
	if a := p.Settings["approval"]; a != "" {
		if !codexApproval[a] {
			return Launch{}, errors.New("codex: unknown approval policy")
		}
		argv = append(argv, "-a", a)
	}
	if p.Resume {
		if !reCodexSession.MatchString(p.SessionID) {
			return Launch{}, errors.New("codex: resume needs a session_id")
		}
		argv = append(argv, "resume", p.SessionID)
	}
	prompt := p.Prompt
	if p.SystemText != "" && !p.Resume {
		// Codex has no verified append-system-prompt flag; standing instructions lead the first message instead.
		prompt = strings.TrimSpace(p.SystemText + "\n\n" + p.Prompt)
	}
	if prompt != "" {
		argv = append(argv, "--", prompt)
	}
	env := commonEnv(c)
	env["CODEX_HOME"] = home
	l := Launch{
		Argv:       argv,
		Env:        env,
		Files:      map[string][]byte{},
		SkillsRoot: filepath.Join(home, "skills"),
		SecretEnv:  secretEnv(p.Secrets, "OPENAI_API_KEY", "CODEX_API_KEY"),
		SessionID:  p.SessionID,
		LoginLinks: map[string]string{filepath.Join(home, "auth.json"): "auth.json", filepath.Join(home, "auth.lock"): "auth.lock"},
	}
	if seed := p.Secrets[CodexAuthKey]; seed != "" {
		if p.AccountRef == "" {
			return Launch{}, errors.New("codex: a ChatGPT login needs its account")
		}
		l.Login = &Login{Account: p.AccountRef, Seed: []byte(seed)}
	}
	return l, nil
}
