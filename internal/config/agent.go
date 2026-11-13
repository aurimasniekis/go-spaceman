package config

import (
	"fmt"

	"github.com/aurimasniekis/go-spaceman/internal/obs"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// AgentConfig is the node `spaceman-agent` configuration.
type AgentConfig struct {
	// Mode is "production" (default) or "development" (allows a plain http central URL).
	Mode Mode `yaml:"mode"`
	// DataDir holds the node journal, the agent's herdr-core home, space folders and memory (absolute).
	DataDir string        `yaml:"data_dir"`
	Central CentralConfig `yaml:"central"`
	Runner  RunnerConfig  `yaml:"runner"`
	Harness HarnessConfig `yaml:"harness"`
	Log     obs.LogConfig `yaml:"log"`
	// ManagementListen is validated (host:port) but not served by this build: the node agent has no management
	// listener yet (docs/LIMITATIONS.md).
	ManagementListen string `yaml:"management_listen"`
	// Capacity is the maximum number of concurrent runs this node accepts (SM-LIMIT-001; 1-256).
	Capacity int `yaml:"capacity"`
}

// CentralConfig locates the central service and the node credential.
type CentralConfig struct {
	// URL is the central public URL (https://…). The agent connects to <URL>/agent/v1/connect.
	URL string `yaml:"url"`
	// CredentialFile holds the node credential written by `spaceman-agent enroll` (0600).
	CredentialFile string `yaml:"credential_file"`
	// CAFile optionally adds a CA certificate for the central TLS endpoint.
	CAFile string `yaml:"ca_file"`
	// ReconnectMax caps the reconnect backoff (1s to 10m).
	ReconnectMax Duration `yaml:"reconnect_max"`
}

// RunnerConfig configures the herdr-core runner (ADR-0007).
type RunnerConfig struct {
	// HCBinary is the herdr-core client binary ("hc").
	HCBinary string `yaml:"hc_binary"`
	// Session is the dedicated hc session (lowercase token); never the operator's "default" core.
	Session string `yaml:"session"`
	// Socket is the hc socket path. It must be short (macOS sun_path ≤ 103 bytes); default /run or /tmp based.
	Socket string `yaml:"socket"`
	// TerminalSize is the size programs start at, COLSxROWS (default 160x48; herdr-core's own default is 200x120).
	// Watch-only viewers never resize a program, so this is the size agents lay out for. Programs already running keep
	// their size.
	TerminalSize string `yaml:"terminal_size"`
}

// HarnessConfig locates harness binaries on the node.
type HarnessConfig struct {
	// ClaudeBinary is the Claude Code executable (name on PATH or absolute path).
	ClaudeBinary string `yaml:"claude_binary"`
	// CodexBinary is the Codex executable (name on PATH or absolute path).
	CodexBinary string `yaml:"codex_binary"`
	// GitBinary is accepted but not read by this build: the agent runs "git" from PATH.
	GitBinary string           `yaml:"git_binary"`
	Claude    ClaudeCodeConfig `yaml:"claude"`
}

// ClaudeCodeConfig configures node-managed Claude Code (ADR-0077): the node resolves a version, downloads it into a
// verified per-version cache under data_dir, keeps the newest few, and launches runs from it by absolute path.
type ClaudeCodeConfig struct {
	// Manage turns node-managed Claude Code on: runs launch the verified cached binary by absolute path. Off (default):
	// runs use claude_binary through the herdr-core core's PATH.
	Manage bool `yaml:"manage"`
	// Channel is the node's default version: latest (default), stable or an exact X.Y.Z.
	Channel string `yaml:"channel"`
	// BaseURL is the release server (https only; redirects must stay on its host); change it only to use a mirror.
	BaseURL string `yaml:"base_url"`
	// CacheKeep is how many of the newest versions the cache keeps besides pinned and in-use ones (1-50).
	CacheKeep int `yaml:"cache_keep"`
	// CacheKeepPinned is how many versions profiles pinned the cache keeps at most, the most recently launched ones
	// within 30 days (1-50). A version a live or adoptable run uses is always kept, whatever the cap.
	CacheKeepPinned int `yaml:"cache_keep_pinned"`
	// Refresh is how long a channel is memoised and how often the node re-resolves, prefetches it and collects the
	// cache's garbage (1m to 24h).
	Refresh Duration `yaml:"refresh"`
	// DownloadTimeout bounds one version download, and how long a launch waits in starting when no version it may run
	// is cached (30s to 2h).
	DownloadTimeout Duration `yaml:"download_timeout"`
	// SelfUpdate lets Claude Code update itself in runs when manage is off. Off (default): every Claude Code run gets
	// DISABLE_AUTOUPDATER=1 and DISABLE_UPDATES=1. It can't be on with manage, whose runs never update themselves.
	SelfUpdate bool `yaml:"self_update"`
}

// IsProduction reports whether the agent runs in production mode.
func (c *AgentConfig) IsProduction() bool { return c.Mode != ModeDevelopment }

// LoadAgent reads the node agent configuration.
func LoadAgent(path string, environ []string) (*AgentConfig, error) {
	cfg := DefaultAgent()
	if err := load(path, environ, AgentEnvPrefix, nonConfigAgentEnv, cfg); err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// ParseTerminalSize reads a COLSxROWS terminal size (runner.terminal_size): 40-400 columns and 10-200 rows.
func ParseTerminalSize(s string) (cols, rows int, err error) {
	if _, err := fmt.Sscanf(s, "%dx%d", &cols, &rows); err != nil || fmt.Sprintf("%dx%d", cols, rows) != s {
		return 0, 0, fmt.Errorf("must be COLSxROWS, like 160x48 (got %q)", s)
	}
	if proto.CheckTerminalSize(cols, rows) != nil {
		return 0, 0, fmt.Errorf("must be %d-%d columns and %d-%d rows (got %s)", proto.TerminalMinCols, proto.TerminalMaxCols,
			proto.TerminalMinRows, proto.TerminalMaxRows, s)
	}
	return cols, rows, nil
}
