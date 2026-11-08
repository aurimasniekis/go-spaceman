package harness

import (
	"context"
	"encoding/json"
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
//   - -c features.shell_snapshot=false: the snapshot would keep the run's environment, its tokens included, in
//     $CODEX_HOME/shell_snapshots (verified with codex-cli 0.166.0 `features list`)
//   - MCP: -c mcp_servers.spaceman.command=… -c mcp_servers.spaceman.args=[…] (TOML overrides)
//   - session: Codex chooses its own session ID; resume is `codex resume <id>` (the ID is discovered from
//     $CODEX_HOME/sessions after launch — not a fixed-ID capability)
//   - auth: an API key becomes $CODEX_HOME/auth.json in API-key mode, as `codex login --with-api-key` writes it: the
//     interactive Codex 0.166.0 ignores OPENAI_API_KEY and CODEX_API_KEY and stops at its sign-in screen (ADR-0054); or a
//     ChatGPT login: $CODEX_HOME/auth.json and auth.lock linked to the node's live copy of the account's login, which
//     Codex refreshes in place (ADR-0053)
//   - skills: $CODEX_HOME/skills/<name>/SKILL.md (verified with codex-cli 0.166.0 `debug prompt-input`)
//   - gateway (ADR-0060): a custom model provider set with five -c overrides (model_provider="harbor-pilot", its name,
//     base_url <origin>/v1, wire_api "responses", env_key "HARBOR_PILOT_TOKEN") and the child token as
//     HARBOR_PILOT_TOKEN through the shim. Codex then needs no auth.json and shows no sign-in screen (verified with
//     codex-cli 0.166.0, docs/contracts/harnesses.md); the node removes any auth.json and login links in CODEX_HOME.
//     The run's model must be one the gateway routes (Codex's own default may not be).
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
	gw, err := gatewayFor(p)
	if err != nil {
		return Launch{}, err
	}
	home := filepath.Join(c.ConfigDir, "codex")
	args := make([]string, 0, len(bridgeArgs(c)))
	for _, a := range bridgeArgs(c) {
		args = append(args, tomlString(a))
	}
	argv := []string{c.Binary, "--no-daemon",
		// Codex's shell snapshot writes the run's environment, SPACEMAN_RUN_TOKEN and a gateway token included, to
		// $CODEX_HOME/shell_snapshots/*.sh, where it outlives the run (found by the HP-T4 real run, codex-cli 0.166.0):
		// secrets reach a run only through its environment (ADR-0009), never a file the harness keeps.
		"-c", "features.shell_snapshot=false",
		"-c", "mcp_servers.spaceman.command=" + tomlString(c.AgentBinary),
		"-c", "mcp_servers.spaceman.args=[" + strings.Join(args, ", ") + "]",
	}
	if gw != nil {
		argv = append(argv, codexGatewayArgs(gw.Origin)...)
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
	if gw != nil {
		// The gateway is the only credential: no API-key auth.json, no ChatGPT login (gatewayFor refused both secrets),
		// and whatever auth.json an earlier launch left is removed; the login links are removed as for an API key.
		l.SecretEnv = map[string]string{CodexGatewayEnv: gw.Token}
		l.RemoveFiles = []string{filepath.Join(home, "auth.json")}
		return l, nil
	}
	if key := firstNonEmpty(p.Secrets["OPENAI_API_KEY"], p.Secrets["CODEX_API_KEY"]); key != "" {
		b, err := json.Marshal(map[string]string{"auth_mode": "apikey", "OPENAI_API_KEY": key})
		if err != nil {
			return Launch{}, err
		}
		l.SecretFiles = map[string][]byte{filepath.Join(home, "auth.json"): b}
	}
	if seed := p.Secrets[CodexAuthKey]; seed != "" {
		if p.AccountRef == "" {
			return Launch{}, errors.New("codex: a ChatGPT login needs its account")
		}
		l.Login = &Login{Account: p.AccountRef, Seed: []byte(seed)}
	}
	return l, nil
}

// CodexGatewayEnv is the variable a gateway run's Codex reads its token from (the provider's env_key).
const CodexGatewayEnv = "HARBOR_PILOT_TOKEN"

// codexGatewayProvider is the model provider ID a gateway run's Codex uses.
const codexGatewayProvider = "harbor-pilot"

// codexGatewayArgs are the -c overrides that send Codex's model traffic to the gateway's Responses API (the shape the
// HP-T1 spike verified with codex-cli 0.166.0).
func codexGatewayArgs(origin string) []string {
	pfx := "model_providers." + codexGatewayProvider + "."
	return []string{
		"-c", "model_provider=" + tomlString(codexGatewayProvider),
		"-c", pfx + "name=" + tomlString("Harbor Pilot"),
		"-c", pfx + "base_url=" + tomlString(origin+"/v1"),
		"-c", pfx + "wire_api=" + tomlString("responses"),
		"-c", pfx + "env_key=" + tomlString(CodexGatewayEnv),
	}
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}
