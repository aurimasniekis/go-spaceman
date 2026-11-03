// Package config loads and validates SpaceMan configuration for the central service and the node agent.
//
// Configuration comes from a YAML file plus environment overrides (SPACEMAN_* for central, SPACEMAN_AGENT_* for the
// node agent). Unknown keys and unknown override variables are rejected. The runtime mode is "production" unless
// explicitly set to "development"; development-only relaxations are rejected in production.
//
// Loader and environment overlay derived from github.com/aurimasniekis/go-hallpass internal/config (Apache-2.0,
// same author).
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/aurimasniekis/go-spaceman/internal/obs"
)

// Mode is the runtime mode.
type Mode string

// Runtime modes.
const (
	ModeProduction  Mode = "production"
	ModeDevelopment Mode = "development"
)

// Duration is a time.Duration that unmarshals from Go duration strings ("30s", "15m").
type Duration time.Duration

// D returns the value as a time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return fmt.Errorf("line %d: duration must be a string like \"30s\": %w", n.Line, err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	*d = Duration(v)
	return nil
}

// MarshalYAML implements yaml.Marshaler.
func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }

// Config is the central `spaceman` configuration.
type Config struct {
	Mode      Mode            `yaml:"mode"`
	DataDir   string          `yaml:"data_dir"`
	PublicURL string          `yaml:"public_url"`
	KEK       KEKConfig       `yaml:"kek"`
	HTTP      HTTPConfig      `yaml:"http"`
	Log       obs.LogConfig   `yaml:"log"`
	Auth      AuthConfig      `yaml:"auth"`
	Nodes     NodesConfig     `yaml:"nodes"`
	Artifacts ArtifactsConfig `yaml:"artifacts"`
	Harness   HarnessPolicy   `yaml:"harness"`
	Events    EventsConfig    `yaml:"events"`
	Backups   BackupsConfig   `yaml:"backups"`
	Limits    LimitsConfig    `yaml:"limits"`
	Shutdown  ShutdownConfig  `yaml:"shutdown"`
}

// LimitsConfig holds the installation defaults of the limits model (SM-LIMIT-001, ADR-0036). Spaces override them
// through the API or UI; the effective value of each limit, and where it comes from, is shown per space. The attachment
// storage quota stays artifacts.space_quota_bytes. 0 means "no limit" where the field says so.
type LimitsConfig struct {
	// MaxConcurrentRuns bounds the worker and diagnostic runs of one space that may be active at once (1-256, default 8).
	// Control, KB support and dreamer runs have their own pools and never count against it.
	MaxConcurrentRuns int64 `yaml:"max_concurrent_runs"`
	// MaxTopLevelTasks is the default queue limit of top-level tasks running at once (1-20, default 1); a space's queue
	// policy overrides it.
	MaxTopLevelTasks int64 `yaml:"max_top_level_tasks"`
	// MaxKBSessions bounds the active KB support sessions of one space across all its knowledge bases (1-64, default 8),
	// on top of each knowledge base's own max_sessions.
	MaxKBSessions int64 `yaml:"max_kb_sessions"`
	// SupportDepth is how deep support may nest (1-3, default 1): at 1 a KB support session cannot ask a knowledge base,
	// so support never starts more support.
	SupportDepth int64 `yaml:"support_depth"`
	// MaxHumanPending bounds the open human actions of one space (1-5000, default 500).
	MaxHumanPending int64 `yaml:"max_human_pending"`
	// MaxHumanPendingPerRun bounds the open human actions one agent run may have asked (1-500, default 25).
	MaxHumanPendingPerRun int64 `yaml:"max_human_pending_per_run"`
	// MaxToolCallsPerRun bounds the SpaceMan and gateway tool calls of one run (0: no limit; default 5000).
	MaxToolCallsPerRun int64 `yaml:"max_tool_calls_per_run"`
	// IngestPerMinute bounds the events one subscription ingests per minute (1-60000, default 600); the rest waits in
	// the stream.
	IngestPerMinute int64 `yaml:"ingest_per_minute"`
	// TaskDeadlineMinutes is how long a top-level task may run before control and the operators are told (0: no
	// deadline; at most 43200). A passed deadline never stops the task.
	TaskDeadlineMinutes int64 `yaml:"task_deadline_minutes"`
	// BudgetTokens24h is a space's token budget over the last 24 hours (input plus output tokens of runs started in that
	// window; 0: none). Usage is reported late, so this warns and stops new runs; it is not a hard limit.
	BudgetTokens24h int64 `yaml:"budget_tokens_24h"`
	// BudgetCostCents24h is a space's estimated-cost budget over the last 24 hours in US cents (0: none). Estimates come
	// from the harnesses and arrive late: never a hard currency limit.
	BudgetCostCents24h int64 `yaml:"budget_cost_cents_24h"`
	// BudgetTokensPerRun warns when one run's reported tokens pass it (0: none). Runs are never stopped for it.
	BudgetTokensPerRun int64 `yaml:"budget_tokens_per_run"`
	// BudgetWarnPercent is when a budget warning is sent (1-100, default 80).
	BudgetWarnPercent int64 `yaml:"budget_warn_percent"`
}

// BackupsConfig configures where central keeps backups and the backup schedule (SM-STORE-005, ADR-0031). Manual
// backups (API, UI, CLI) use the same target whether or not the schedule is enabled.
type BackupsConfig struct {
	// Enabled turns the schedule on: one backup every Interval, then retention.
	Enabled  bool     `yaml:"enabled"`
	Interval Duration `yaml:"interval"`
	// KeepLast is how many complete backups retention keeps at least (the newest ones).
	KeepLast int `yaml:"keep_last"`
	// MaxAge deletes backups older than this beyond the newest one (0 keeps them, bounded by KeepLast only).
	MaxAge Duration `yaml:"max_age"`
	// Target is "local" (default) or "s3".
	Target string `yaml:"target"`
	// LocalDir is the local target directory (default <data_dir>/backups). Prefer another disk.
	LocalDir string   `yaml:"local_dir"`
	S3       S3Config `yaml:"s3"`
}

// EventsConfig configures SpaceMan's own outbound events. Inbound Pulse connections and subscriptions are managed
// through the API, not here (SM-OBS-005).
type EventsConfig struct {
	Outbound OutboundConfig `yaml:"outbound"`
}

// OutboundConfig configures publication of SpaceMan's events to Pulse through the transactional outbox
// (SM-OBS-004, ADR-0030). It is off by default; while off, no events are recorded.
type OutboundConfig struct {
	Enabled bool `yaml:"enabled"`
	// URL is the Pulse NATS URL: tls://host:port (required in production) or nats://host:port.
	URL string `yaml:"url"`
	// CredsFile is the Pulse client .creds file (user JWT and nkey seed) with a publish:evt.spaceman.> grant.
	CredsFile string `yaml:"creds_file"`
	// CAFile is an optional extra CA bundle (PEM) for tls:// URLs.
	CAFile string `yaml:"ca_file"`
	// Source is ce-source and the spaceman-origin header (default "spaceman"); give each installation its own.
	Source string `yaml:"source"`
	// Retention is how long published and dead-lettered events are kept (default 168h).
	Retention Duration `yaml:"retention"`
}

// HarnessPolicy holds policy ceilings applied to every launch, whatever layer set the value (SM-PROMPT-003).
type HarnessPolicy struct {
	// MaxPermissionMode is the most permissive Claude Code permission mode a run may get: plan, default, dontAsk,
	// acceptEdits, auto (default) or bypassPermissions. A higher mode from a profile, role or launch is lowered to it.
	MaxPermissionMode string `yaml:"max_permission_mode"`
}

// KEKConfig locates the master key-encryption key.
type KEKConfig struct {
	// Source is "file" (default) or "env" (reads base64 from SPACEMAN_KEK; discouraged).
	Source string `yaml:"source"`
	File   string `yaml:"file"`
}

// HTTPConfig configures the web/API listener and the separate management listener (SM-OBS-002).
type HTTPConfig struct {
	Listen string `yaml:"listen"`
	// ManagementListen serves /healthz, /readyz and /metrics only. It should not be exposed publicly.
	ManagementListen string   `yaml:"management_listen"`
	TLSCertFile      string   `yaml:"tls_cert_file"`
	TLSKeyFile       string   `yaml:"tls_key_file"`
	TrustedProxies   []string `yaml:"trusted_proxies"`
	MaxBodyBytes     int64    `yaml:"max_body_bytes"`
	// MaxUploadBytes bounds one attachment upload (SM-ATT-002).
	MaxUploadBytes    int64    `yaml:"max_upload_bytes"`
	ReadHeaderTimeout Duration `yaml:"read_header_timeout"`
}

// AuthConfig configures operator authentication (SM-AUTH-001).
type AuthConfig struct {
	SessionIdle        Duration `yaml:"session_idle"`
	SessionAbsolute    Duration `yaml:"session_absolute"`
	ReauthWindow       Duration `yaml:"reauth_window"`
	PasswordMinLength  int      `yaml:"password_min_length"`
	RequireTOTP        bool     `yaml:"require_totp"`
	LocalLoginDisabled bool     `yaml:"local_login_disabled"`
	// StreamRevalidate is how often an open live stream (change feed, terminal) and a node connection re-check their
	// credential and authorization. Revocations committed by central end affected streams at once; this interval
	// bounds everything else (expiry, idle timeout, changes made while central was stopped). 1 s to 60 s, default
	// 30 s (SM-AUTH-007, ADR-0033).
	StreamRevalidate Duration `yaml:"stream_revalidate"`
}

// NodesConfig configures node connectivity and execution leases (ADR-0006).
type NodesConfig struct {
	HeartbeatInterval Duration `yaml:"heartbeat_interval"`
	// LeaseTTL is how long one renewal of a space's execution lease is valid.
	LeaseTTL Duration `yaml:"lease_ttl"`
	// DisconnectGrace is how long a node keeps runs going after losing central before stopping them (SM-NODE-007).
	DisconnectGrace Duration `yaml:"disconnect_grace"`
	EnrollmentTTL   Duration `yaml:"enrollment_ttl"`
	CommandTimeout  Duration `yaml:"command_timeout"`
}

// ArtifactsConfig selects the artifact store (SM-STORE-002).
type ArtifactsConfig struct {
	// Backend is "local" (default: <data_dir>/artifacts) or "s3".
	Backend  string   `yaml:"backend"`
	LocalDir string   `yaml:"local_dir"`
	S3       S3Config `yaml:"s3"`
	// SpaceQuotaBytes bounds the attachments stored per space (SM-ATT-002).
	SpaceQuotaBytes int64 `yaml:"space_quota_bytes"`
}

// S3Config configures an S3-compatible bucket. Secrets come from files, never inline values.
type S3Config struct {
	Endpoint        string `yaml:"endpoint"`
	Region          string `yaml:"region"`
	Bucket          string `yaml:"bucket"`
	Prefix          string `yaml:"prefix"`
	AccessKeyFile   string `yaml:"access_key_file"`
	SecretKeyFile   string `yaml:"secret_key_file"`
	UsePathStyle    bool   `yaml:"use_path_style"`
	InsecureAllowed bool   `yaml:"insecure_allowed"`
	// AllowPrivateNetwork lets the endpoint resolve to a private, loopback or link-local address (self-hosted MinIO).
	AllowPrivateNetwork bool `yaml:"allow_private_network"`
	// CAFile adds trust roots (PEM) for the endpoint; verification is never disabled.
	CAFile string `yaml:"ca_file"`
}

// ShutdownConfig bounds graceful shutdown.
type ShutdownConfig struct {
	Timeout Duration `yaml:"timeout"`
}

// IsProduction reports whether the configuration runs in production mode.
func (c *Config) IsProduction() bool { return c.Mode != ModeDevelopment }

// Load reads the central configuration from path (optional) and environ (os.Environ() form).
func Load(path string, environ []string) (*Config, error) {
	cfg := Default()
	if err := load(path, environ, EnvPrefix, nonConfigEnv, cfg); err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func load[T any](path string, environ []string, prefix string, skip map[string]bool, cfg *T) error {
	if path != "" {
		f, err := os.Open(path) //nolint:gosec // operator-supplied config path
		if err != nil {
			return fmt.Errorf("open config: %w", err)
		}
		defer f.Close()
		if err := decode(f, cfg); err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
	}
	return applyEnv(cfg, environ, prefix, skip)
}

func decode(r io.Reader, cfg any) error {
	data, err := io.ReadAll(io.LimitReader(r, 1<<20))
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("config must contain a single YAML document")
	}
	return nil
}
