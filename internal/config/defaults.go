package config

import (
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/harness/dist"
)

// Default returns the central configuration defaults (site/reference/configuration.md).
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
			WriteIdleTimeout:  Duration(time.Minute),
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
		Events:    EventsConfig{Outbound: OutboundConfig{Source: "spaceman", Retention: Duration(7 * 24 * time.Hour)}},
		Backups:   BackupsConfig{Interval: Duration(24 * time.Hour), KeepLast: 7, Target: "local"},
		Limits: LimitsConfig{MaxConcurrentRuns: 8, MaxTopLevelTasks: 1, MaxKBSessions: 8, SupportDepth: 1, MaxHumanPending: 500,
			MaxHumanPendingPerRun: 25, MaxToolCallsPerRun: 5000, IngestPerMinute: 600, EventDepth: 8, BudgetWarnPercent: 80, RecordingQuotaBytes: 512 << 20,
			ExportMaxBytes: 2 << 30, ControlAutostart: 1, ControlIdleStopMinutes: 30,
			UsageLimitKeepAliveMaxMinutes: 360, UsageLimitMaxWaitHours: 168},
		Shutdown: ShutdownConfig{Timeout: Duration(30 * time.Second)},
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
			// herdr-core starts programs at 200x120 when nobody is attached; agents laid out for 120 rows are far taller
			// than a browser shows.
			TerminalSize: "160x48",
		},
		Harness: HarnessConfig{
			ClaudeBinary: "claude",
			CodexBinary:  "codex",
			GitBinary:    "git",
			Claude: ClaudeCodeConfig{
				Manage:          false, // operators opt in (ADR-0077)
				Channel:         dist.ChannelLatest,
				BaseURL:         dist.DefaultBaseURL,
				CacheKeep:       dist.DefaultKeep,
				CacheKeepPinned: dist.DefaultKeepPinned,
				Refresh:         Duration(dist.DefaultRefresh),
				DownloadTimeout: Duration(dist.DefaultDownloadTimeout),
			},
		},
		Log:      defaultLog(),
		Capacity: 4,
	}
}
