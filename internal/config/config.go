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
	// Mode is "production" (default) or "development". Development mode allows plain http, disabling TOTP, shorter
	// passwords and other relaxations that production rejects.
	Mode Mode `yaml:"mode"`
	// DataDir holds the database, artifacts and backups by default. Absolute, on a local filesystem (network
	// filesystems are refused); created 0700.
	DataDir string `yaml:"data_dir"`
	// PublicURL is the URL browsers and nodes use to reach central (required; https in production).
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
	Retention RetentionConfig `yaml:"retention"`
	Shutdown  ShutdownConfig  `yaml:"shutdown"`
	Telegram  TelegramConfig  `yaml:"telegram"`
}

// TelegramConfig holds the Telegram settings that belong to the deployment rather than the API (ADR-0083). The bot
// itself (token, mode, Mini App host) is configured through the API and Terraform.
type TelegramConfig struct {
	// APIBaseURL is the Bot API endpoint the bot token is sent to: https, or http on a loopback host (a fake server in
	// tests, a local Bot API server). It is deliberately settable only in central's own configuration (this file or
	// its environment override), never through the API or Terraform: whoever sets it receives the token.
	APIBaseURL string `yaml:"api_base_url"`
}

// RetentionConfig holds the installation-wide retention that cannot be set per space (ADR-0045). Per-space retention
// periods are limits (limits.retention_*_days, overridable per space); one daily pass applies them all.
type RetentionConfig struct {
	// AuditDays deletes audit events older than this many days in the daily retention pass (0: keep forever, the
	// default; otherwise 30-36500). The hash chain keeps verifying from the first retained event, and a reader whose
	// cursor is older is told that events were pruned. Audit is installation-wide: it cannot be set per space.
	AuditDays int64 `yaml:"audit_days"`
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
	// EventDepth bounds automation across installations (1-64, default 8): an inbound event whose spaceman-depth
	// header says it already passed through this many SpaceMan ingestions is quarantined instead of becoming an inbox
	// item, so an event loop between installations ends (SM-EVT-016, ADR-0049).
	EventDepth int64 `yaml:"event_depth"`
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
	// RetentionRecordingsDays deletes central's copy of a run's terminal recording this many days after the run ended
	// (0: keep until deleted or the space is purged, the default; at most 36500). The node's local copy is separate.
	RetentionRecordingsDays int64 `yaml:"retention_recordings_days"`
	// RetentionTranscriptsDays deletes a run's stored native transcript this many days after it was stored (0: keep).
	RetentionTranscriptsDays int64 `yaml:"retention_transcripts_days"`
	// RetentionAttachmentsDays deletes attachments older than this many days once every item that uses them is
	// finished (done or rejected inbox items without an open task or human action). An attachment that an open item
	// uses is never deleted (0: keep). Unreferenced uploads are collected after a day regardless.
	RetentionAttachmentsDays int64 `yaml:"retention_attachments_days"`
	// RetentionHumanActionsDays deletes finished human actions (resolved, cancelled, expired) this many days after
	// they finished, with their comments and notifications, unless an open task, an open follow-up or an undelivered
	// response still refers to them (0: keep).
	RetentionHumanActionsDays int64 `yaml:"retention_human_actions_days"`
	// RecordingQuotaBytes bounds one run's terminal recording on the node (1 MiB-64 GiB, default 512 MiB). When it is
	// reached the recording ends with a truncation record; the run is unaffected.
	RecordingQuotaBytes int64 `yaml:"recording_quota_bytes"`
	// ExportMaxBytes bounds one space export archive (1 MiB-64 GiB, default 2 GiB): a larger export is refused.
	ExportMaxBytes int64 `yaml:"export_max_bytes"`
	// ControlAutostart starts a space's control role when work arrives for control and no control run is live (1: on,
	// the default; 0: off). It is skipped while the space is paused and when the launch is refused (no control role,
	// account or budget); at most 6 starts an hour per space (ADR-0056).
	ControlAutostart int64 `yaml:"control_autostart"`
	// ControlIdleStopMinutes stops a control run that has been idle this long (finished its turn, nothing waiting for
	// it, no operator at its terminal); the next event starts it again, resuming its session (0: never; default 30; at
	// most 10080).
	ControlIdleStopMinutes int64 `yaml:"control_idle_stop_minutes"`
	// TerminalCols is the width new runs' terminals are held at (ADR-0075): 40-400 columns, or 0 (the default) for the
	// node's runner.terminal_size. A size applies only when terminal_rows is set too.
	TerminalCols int64 `yaml:"terminal_cols"`
	// TerminalRows is the height new runs' terminals are held at: 10-200 rows, or 0 (the default) for the node's
	// runner.terminal_size. A size applies only when terminal_cols is set too.
	TerminalRows int64 `yaml:"terminal_rows"`
}

// BackupsConfig configures where central keeps backups and the backup schedule (SM-STORE-005, ADR-0031). Manual
// backups (API, UI, CLI) use the same target whether or not the schedule is enabled.
type BackupsConfig struct {
	// Enabled turns the schedule on: one backup every interval, then retention.
	Enabled bool `yaml:"enabled"`
	// Interval is the time between scheduled backups (15m to 168h).
	Interval Duration `yaml:"interval"`
	// KeepLast is how many complete backups retention keeps at least (the newest ones; 1-1000).
	KeepLast int `yaml:"keep_last"`
	// MaxAge deletes backups older than this beyond the newest one (0 keeps them, bounded by keep_last only).
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
	// Enabled turns publication on (needs url and creds_file).
	Enabled bool `yaml:"enabled"`
	// URL is the Pulse NATS URL: tls://host:port (required in production) or nats://host:port.
	URL string `yaml:"url"`
	// CredsFile is the Pulse client .creds file (user JWT and nkey seed) with a publish:evt.spaceman.> grant.
	CredsFile string `yaml:"creds_file"`
	// CAFile is an optional extra CA bundle (PEM) for tls:// URLs.
	CAFile string `yaml:"ca_file"`
	// Source is the ce-source and the spaceman-origin header (default "spaceman"); give each installation its own.
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
	// File is the KEK file (absolute in production). Keep it outside data_dir, readable only by central, and back it
	// up offline apart from the backups: without it backups and secrets cannot be decrypted.
	File string `yaml:"file"`
}

// HTTPConfig configures the web/API listener and the separate management listener (SM-OBS-002).
type HTTPConfig struct {
	// Listen is the host:port of the web UI, API and node connections.
	Listen string `yaml:"listen"`
	// ManagementListen serves /healthz, /readyz and /metrics only (empty: disabled). It is unauthenticated: keep it
	// private. It must differ from listen.
	ManagementListen string `yaml:"management_listen"`
	// TLSCertFile is the TLS certificate (PEM). With tls_key_file (set together) central terminates TLS itself;
	// otherwise terminate it at a reverse proxy.
	TLSCertFile string `yaml:"tls_cert_file"`
	// TLSKeyFile is the private key for tls_cert_file.
	TLSKeyFile string `yaml:"tls_key_file"`
	// TrustedProxies lists the CIDR prefixes of reverse proxies whose X-Forwarded-For is trusted; other peers' headers
	// are ignored.
	TrustedProxies []string `yaml:"trusted_proxies"`
	// MaxBodyBytes bounds one API request body (attachment uploads have their own bound).
	MaxBodyBytes int64 `yaml:"max_body_bytes"`
	// MaxUploadBytes bounds one attachment upload (SM-ATT-002).
	MaxUploadBytes int64 `yaml:"max_upload_bytes"`
	// ReadHeaderTimeout bounds how long a client may take to send request headers.
	ReadHeaderTimeout Duration `yaml:"read_header_timeout"`
	// WriteIdleTimeout bounds how long one piece (32 KiB) of a response may wait for the client to read it (1s to
	// 10m). It bounds progress, not the whole response: a slow download that keeps moving is never cut off, but a
	// client that stops reading is disconnected instead of holding the response in central's memory. WebSocket
	// connections (nodes, terminals) keep their own timeouts.
	WriteIdleTimeout Duration `yaml:"write_idle_timeout"`
}

// AuthConfig configures operator authentication (SM-AUTH-001).
type AuthConfig struct {
	// SessionIdle ends a browser session after this much inactivity (1m to 24h).
	SessionIdle Duration `yaml:"session_idle"`
	// SessionAbsolute ends a browser session this long after sign-in, whatever its activity (session_idle to 720h).
	SessionAbsolute Duration `yaml:"session_absolute"`
	// ReauthWindow is how long a step-up re-authentication counts for sensitive actions (1m to 1h).
	ReauthWindow Duration `yaml:"reauth_window"`
	// PasswordMinLength is the shortest password accepted (at least 12 in production).
	PasswordMinLength int `yaml:"password_min_length"`
	// RequireTOTP makes TOTP mandatory for every password account. Production requires it; privileged accounts need
	// TOTP even in development mode.
	RequireTOTP bool `yaml:"require_totp"`
	// LocalLoginDisabled is accepted but not read: password sign-in is turned off (SSO-only) at run time on the Sign-in
	// providers page or with `spaceman oidc settings set --sso-only`, which first checks that a tested provider and an
	// administrator who can sign in with SSO exist (ADR-0045).
	LocalLoginDisabled bool `yaml:"local_login_disabled"`
	// StreamRevalidate is how often an open live stream (change feed, terminal) and a node connection re-check their
	// credential and authorization. Revocations committed by central end affected streams at once; this interval
	// bounds everything else (expiry, idle timeout, changes made while central was stopped). 1 s to 60 s, default
	// 30 s (SM-AUTH-007, ADR-0033).
	StreamRevalidate Duration `yaml:"stream_revalidate"`
}

// MaxLeaseTTL bounds nodes.lease_ttl. A node keeps a space's lease for up to lease_ttl after central last renewed it,
// whatever central records later (a revoke stores the revoke time as the expiry), so the bound keeps that window well
// inside the stale-run sweeper's margin past a lease's expiry (ADR-0074, 15 minutes).
const MaxLeaseTTL = 10 * time.Minute

// NodesConfig configures node connectivity and execution leases (ADR-0006).
type NodesConfig struct {
	// HeartbeatInterval is how often a connected node reports in and renews its leases (1s to 1m).
	HeartbeatInterval Duration `yaml:"heartbeat_interval"`
	// LeaseTTL is how long one renewal of a space's execution lease is valid (at least 3x heartbeat_interval, at most
	// 10m).
	LeaseTTL Duration `yaml:"lease_ttl"`
	// DisconnectGrace is how long a node keeps runs going after losing central before stopping them (SM-NODE-007;
	// 0 to 1h).
	DisconnectGrace Duration `yaml:"disconnect_grace"`
	// EnrollmentTTL is how long a node enrollment token stays valid (1m to 720h).
	EnrollmentTTL Duration `yaml:"enrollment_ttl"`
	// CommandTimeout is validated (1s to 10m) but not read by this build; node commands use their own fixed timeouts.
	CommandTimeout Duration `yaml:"command_timeout"`
}

// ArtifactsConfig selects the artifact store (SM-STORE-002, ADR-0048): where attachment content, central's copies of
// recordings and run transcripts are kept.
type ArtifactsConfig struct {
	// Backend is "local" (default: attachments in local_dir, recordings and transcripts in <data_dir>) or "s3" (the
	// bucket in s3; recordings being shipped and transcripts being received are spooled in <data_dir> until complete).
	// Switching backends does not move stored artifacts: stop central and run `spaceman artifacts migrate`.
	Backend string `yaml:"backend"`
	// LocalDir is the local backend's attachment directory (default <data_dir>/artifacts); with s3 it holds the upload
	// spool (tmp/).
	LocalDir string `yaml:"local_dir"`
	// S3 is the bucket of the s3 backend; its prefix must not overlap backups.s3's in the same bucket.
	S3 S3Config `yaml:"s3"`
	// SpaceQuotaBytes bounds the attachments stored per space (SM-ATT-002).
	SpaceQuotaBytes int64 `yaml:"space_quota_bytes"`
}

// S3Config configures an S3-compatible bucket. Secrets come from files, never inline values.
type S3Config struct {
	// Endpoint is the S3 API endpoint URL (required for s3).
	Endpoint string `yaml:"endpoint"`
	// Region is the bucket's region.
	Region string `yaml:"region"`
	// Bucket is the bucket name (required for s3).
	Bucket string `yaml:"bucket"`
	// Prefix is prepended to every object key.
	Prefix string `yaml:"prefix"`
	// AccessKeyFile holds the access key ID (an absolute path, required with secret_key_file).
	AccessKeyFile string `yaml:"access_key_file"`
	// SecretKeyFile holds the secret access key.
	SecretKeyFile string `yaml:"secret_key_file"`
	// UsePathStyle addresses the bucket in the path instead of the host name (MinIO and most self-hosted stores).
	UsePathStyle bool `yaml:"use_path_style"`
	// InsecureAllowed permits a plain http endpoint (development mode only).
	InsecureAllowed bool `yaml:"insecure_allowed"`
	// AllowPrivateNetwork lets the endpoint resolve to a private, loopback or link-local address (self-hosted MinIO).
	AllowPrivateNetwork bool `yaml:"allow_private_network"`
	// CAFile adds trust roots (PEM) for the endpoint; verification is never disabled.
	CAFile string `yaml:"ca_file"`
}

// ShutdownConfig bounds graceful shutdown.
type ShutdownConfig struct {
	// Timeout is how long central waits for requests and background work to finish on stop (1s to 10m). Keep it below
	// the service manager's stop timeout.
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
