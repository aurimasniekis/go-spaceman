package config

import "github.com/aurimasniekis/go-spaceman/internal/obs"

// AgentConfig is the node `spaceman-agent` configuration.
type AgentConfig struct {
	Mode    Mode          `yaml:"mode"`
	DataDir string        `yaml:"data_dir"`
	Central CentralConfig `yaml:"central"`
	Runner  RunnerConfig  `yaml:"runner"`
	Harness HarnessConfig `yaml:"harness"`
	Log     obs.LogConfig `yaml:"log"`
	// ManagementListen serves /healthz, /readyz and /metrics for the node (empty: disabled).
	ManagementListen string `yaml:"management_listen"`
	// Capacity is the maximum number of concurrent runs this node accepts (SM-LIMIT-001).
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
	// ReconnectMax caps the reconnect backoff.
	ReconnectMax Duration `yaml:"reconnect_max"`
}

// RunnerConfig configures the herdr-core runner (ADR-0007).
type RunnerConfig struct {
	// HCBinary is the herdr-core client binary ("hc").
	HCBinary string `yaml:"hc_binary"`
	// Session is the dedicated hc session; never the operator's "default" core.
	Session string `yaml:"session"`
	// Socket is the hc socket path. It must be short (macOS sun_path ≤ 103 bytes); default /run or /tmp based.
	Socket string `yaml:"socket"`
}

// HarnessConfig locates harness binaries on the node.
type HarnessConfig struct {
	ClaudeBinary string `yaml:"claude_binary"`
	CodexBinary  string `yaml:"codex_binary"`
	GitBinary    string `yaml:"git_binary"`
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
