// Package harness builds launches for coding-agent harnesses (SM-HARN-001…004): Claude Code and Codex.
//
// A harness never sees another space's configuration: every space and agent instance gets its own native config
// directory (CLAUDE_CONFIG_DIR, CODEX_HOME) under the node agent's data directory (ADR-0008). Secrets are returned
// separately (SecretEnv) and are delivered only through the launcher shim's 0600 file (ADR-0009) — never through
// the runner's environment, which herdr-core persists (spike S-1).
//
// Only CLI flags verified against the installed versions are used (docs/contracts/harnesses.md). Capabilities are
// advertised explicitly, separating verified from unverified behavior (SM-HARN-003, SM-INV-014).
package harness

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// Kinds.
const (
	KindClaude = "claude-code"
	KindCodex  = "codex"
)

// Capabilities a harness may advertise.
const (
	CapFixedSessionID = "session.fixed_id"         // the session ID can be chosen at launch
	CapResume         = "session.resume"           // a previous session can be resumed
	CapMCPConfig      = "mcp.config"               // MCP servers can be injected per launch
	CapProgramStatus  = "status.osc7501"           // reports idle/working/done via OSC 7501 (verified live)
	CapSafeInput      = "input.terminal_send_idle" // safe input via the runner's idle-gated send
	CapNativeTeam     = "messaging.native_team"    // native team messaging (not across standalone runs)
	CapTranscripts    = "transcripts.jsonl"
	CapSystemPrompt   = "prompt.append_system"
	CapSkills         = "skills.files" // loads <config>/skills/<name>/SKILL.md (verified for Claude Code 2.1.316 and codex-cli 0.166.0)
)

// Context is everything a harness needs to build one launch.
type Context struct {
	// AgentBinary is the spaceman-agent executable (for the MCP bridge and the launcher shim).
	AgentBinary string
	// BridgeSocket is the node agent's local socket the MCP bridge connects to.
	BridgeSocket string
	// ConfigDir is the instance's harness config root (per space and instance).
	ConfigDir string
	// WorkDir is where the program runs (inside the space folder).
	WorkDir string
	RunID   string
	SpaceID string
	// RunDir holds per-run files (MCP config, the secret file). 0700.
	RunDir  string
	Binary  string
	Payload proto.LaunchPayload
	// MemoryDir is the instance's checkout of its role memory ("" when memory is unavailable on the node).
	MemoryDir string
}

// Launch is a built launch.
type Launch struct {
	Argv []string
	Env  map[string]string // non-secret
	// Files are written (0600) before the launch; keys are absolute paths.
	Files map[string][]byte
	// MergeJSON patches JSON object files the harness itself also writes (keys are absolute paths): objects are merged
	// recursively and other values are set, everything else in the file is kept. A missing file is created (0600).
	MergeJSON map[string]map[string]any
	// SkillsRoot is where the harness loads per-user skills from (<root>/<name>/SKILL.md); empty when the harness's skill
	// loading is not verified, and then skills are not materialized.
	SkillsRoot string
	// SecretEnv is passed to the launcher shim only.
	SecretEnv map[string]string
	// Login is a login file the harness reads and rewrites itself (a Codex ChatGPT login, ADR-0053); nil otherwise.
	Login *Login
	// SecretFiles are credential files the harness reads from its config dir (absolute paths): written by the node with
	// mode 0600 through a temporary file and a rename, so a link left at the path is replaced, never followed. Codex
	// takes an API key only from a stored login (ADR-0054).
	SecretFiles map[string][]byte
	// LoginLinks are where the harness looks for a login file, as paths in its config dir mapped to file names in the
	// node's shared login folder. With Login they are linked to the account's live copy; without it, links left from
	// an earlier login are removed (a regular file there is the harness's own and is kept).
	LoginLinks map[string]string
	// SessionID is the conversation ID when the harness lets SpaceMan choose it.
	SessionID string
}

// Login is a credential the node keeps as one live file per account, shared by every instance that uses the account:
// the harness refreshes it in place, and a refresh token works only once, so instances must not hold copies.
type Login struct {
	// Account is the account ID (one live copy per account and node).
	Account string
	// Seed is the login as central holds it. It replaces the live copy only when it differs from the seed the live
	// copy started from (the operator rotated it); otherwise the live copy, possibly refreshed, is kept.
	Seed []byte
}

// CodexAuthKey is the launch-payload key of a Codex ChatGPT login (internal/accounts.CodexAuthKey); never an env var.
const CodexAuthKey = "CODEX_AUTH_JSON"

// Harness builds launches for one harness kind.
type Harness interface {
	Kind() string
	// Detect reports whether the harness is installed and its version and capabilities.
	Detect(ctx context.Context, binary string) proto.HarnessInfo
	Build(c Context) (Launch, error)
}

// Registry is the compiled-in harness set (SM-ARCH-003: adapters are compiled in).
var Registry = map[string]Harness{KindClaude: Claude{}, KindCodex: Codex{}}

// Get returns a harness by kind.
func Get(kind string) (Harness, error) {
	h, ok := Registry[kind]
	if !ok {
		return nil, fmt.Errorf("unknown harness %q", kind)
	}
	return h, nil
}

var reVersion = regexp.MustCompile(`\d+\.\d+\.\d+`)

func detect(ctx context.Context, kind, binary string, caps []string) proto.HarnessInfo {
	info := proto.HarnessInfo{Kind: kind, Capabilities: caps}
	path, err := exec.LookPath(binary)
	if err != nil {
		info.Error = "not installed"
		info.Capabilities = []string{}
		return info
	}
	info.Path = path
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, path, "--version").Output() //nolint:gosec // operator-configured binary
	if err != nil {
		info.Error = "version check failed"
		return info
	}
	info.Version = reVersion.FindString(string(out))
	info.Available = true
	return info
}

// NewUUID returns a random RFC 4122 v4 UUID (Claude Code session IDs).
func NewUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

var reUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// validModel accepts model identifiers without whitespace or shell metacharacters (argv, never a shell line).
var reModel = regexp.MustCompile(`^[A-Za-z0-9._:/\[\]-]{1,128}$`)

func checkCommon(c Context) error {
	switch {
	case c.ConfigDir == "" || c.WorkDir == "" || c.RunDir == "" || c.RunID == "":
		return errors.New("harness: incomplete launch context")
	case c.Payload.Model != "" && !reModel.MatchString(c.Payload.Model):
		return errors.New("harness: invalid model name")
	}
	return nil
}

// secretEnv picks the payload secrets a harness accepts (anything else is dropped, never forwarded blindly).
func secretEnv(secrets map[string]string, allowed ...string) map[string]string {
	out := map[string]string{}
	for _, k := range allowed {
		if v, ok := secrets[k]; ok && v != "" {
			out[k] = v
		}
	}
	return out
}

// commonEnv is the non-secret environment every harness run gets.
func commonEnv(c Context) map[string]string {
	env := map[string]string{
		"SPACEMAN_RUN_ID":   c.RunID,
		"SPACEMAN_SPACE_ID": c.SpaceID,
		// Hallpass scopes access requests per agent session (Hallpass ADR-0061; not a security boundary).
		"HALLPASS_SESSION": c.RunID,
		"TERM":             "xterm-256color",
		"COLORTERM":        "truecolor",
	}
	if c.MemoryDir != "" {
		env["SPACEMAN_MEMORY_DIR"] = c.MemoryDir
	}
	return env
}

func bridgeArgs(c Context) []string {
	return []string{"mcp-bridge", "--run", c.RunID, "--socket", c.BridgeSocket}
}

// tomlString quotes s as a TOML basic string.
func tomlString(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`, "\t", `\t`)
	return `"` + r.Replace(s) + `"`
}
