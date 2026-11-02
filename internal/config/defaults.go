package config

import "time"

// Default returns the central configuration defaults (docs/operator/configuration.md).
func Default() *Config {
	return &Config{
		Mode:      ModeProduction,
		DataDir:   "/var/lib/spaceman",
		PublicURL: "",
		KEK:       KEKConfig{Source: "file", File: "/etc/spaceman/kek"},
		HTTP: HTTPConfig{
			Listen:            ":8080",
			ManagementListen:  "127.0.0.1:9090",
			MaxBodyBytes:      1 << 20,
			MaxUploadBytes:    25 << 20,
			ReadHeaderTimeout: Duration(10 * time.Second),
		},
		Log: defaultLog(),
		Auth: AuthConfig{
			SessionIdle:       Duration(30 * time.Minute),
			SessionAbsolute:   Duration(12 * time.Hour),
			ReauthWindow:      Duration(5 * time.Minute),
			PasswordMinLength: 12,
			RequireTOTP:       true,
			StreamRevalidate:  Duration(30 * time.Second),
		},
		Nodes: NodesConfig{
			HeartbeatInterval: Duration(10 * time.Second),
			LeaseTTL:          Duration(45 * time.Second),
			DisconnectGrace:   Duration(2 * time.Minute),
			EnrollmentTTL:     Duration(24 * time.Hour),
			CommandTimeout:    Duration(30 * time.Second),
		},
		Artifacts: ArtifactsConfig{Backend: "local", SpaceQuotaBytes: 1 << 30},
		Harness:   HarnessPolicy{MaxPermissionMode: "auto"},
		Shutdown:  ShutdownConfig{Timeout: Duration(30 * time.Second)},
	}
}

// DefaultAgent returns the node agent configuration defaults.
func DefaultAgent() *AgentConfig {
	return &AgentConfig{
		Mode:    ModeProduction,
		DataDir: "/var/lib/spaceman-agent",
		Central: CentralConfig{ //nolint:gosec // G101 false positive: a file path, not a credential
			CredentialFile: "/var/lib/spaceman-agent/node.credential",
			ReconnectMax:   Duration(30 * time.Second),
		},
		Runner: RunnerConfig{
			HCBinary: "hc",
			Session:  "spaceman",
			Socket:   "/run/spaceman-agent/hc.sock",
		},
		Harness: HarnessConfig{
			ClaudeBinary: "claude",
			CodexBinary:  "codex",
			GitBinary:    "git",
		},
		Log:      defaultLog(),
		Capacity: 4,
	}
}
