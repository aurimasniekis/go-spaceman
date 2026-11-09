package config

import (
	"fmt"
	"maps"
	"net"
	"net/netip"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/obs"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// ValidationError aggregates every configuration problem found.
type ValidationError struct{ Problems []string }

func (e *ValidationError) Error() string {
	return "invalid configuration:\n  - " + strings.Join(e.Problems, "\n  - ")
}

// Validate checks the central configuration, including production-mode restrictions.
func (c *Config) Validate() error {
	v := &validator{}
	v.mode(c.Mode)
	prod := c.IsProduction()
	if !filepath.IsAbs(c.DataDir) {
		v.add("data_dir must be an absolute path")
	}
	v.publicURL("public_url", c.PublicURL, prod)
	switch c.KEK.Source {
	case "file":
		if prod && !filepath.IsAbs(c.KEK.File) {
			v.add("kek.file must be an absolute path in production")
		}
	case "env":
	default:
		v.add("kek.source must be \"file\" or \"env\", got %q", c.KEK.Source)
	}
	v.listen("http.listen", c.HTTP.Listen)
	if c.HTTP.ManagementListen != "" {
		v.listen("http.management_listen", c.HTTP.ManagementListen)
		if c.HTTP.ManagementListen == c.HTTP.Listen {
			v.add("http.management_listen must differ from http.listen (SM-OBS-002)")
		}
	}
	if (c.HTTP.TLSCertFile == "") != (c.HTTP.TLSKeyFile == "") {
		v.add("http.tls_cert_file and http.tls_key_file must be set together")
	}
	for _, p := range c.HTTP.TrustedProxies {
		v.prefix("http.trusted_proxies", p)
	}
	v.positive64("http.max_body_bytes", c.HTTP.MaxBodyBytes)
	v.positive64("http.max_upload_bytes", c.HTTP.MaxUploadBytes)
	v.logConfig(c.Log)

	v.durationRange("auth.session_idle", c.Auth.SessionIdle, time.Minute, 24*time.Hour)
	v.durationRange("auth.session_absolute", c.Auth.SessionAbsolute, c.Auth.SessionIdle.D(), 30*24*time.Hour)
	v.durationRange("auth.reauth_window", c.Auth.ReauthWindow, time.Minute, time.Hour)
	v.durationRange("auth.stream_revalidate", c.Auth.StreamRevalidate, time.Second, time.Minute)
	if c.Auth.PasswordMinLength < 12 && prod {
		v.add("auth.password_min_length must be at least 12 in production")
	}
	if !c.Auth.RequireTOTP && prod {
		v.add("auth.require_totp may only be disabled in development mode")
	}

	v.durationRange("nodes.heartbeat_interval", c.Nodes.HeartbeatInterval, time.Second, time.Minute)
	if c.Nodes.LeaseTTL.D() < 3*c.Nodes.HeartbeatInterval.D() {
		v.add("nodes.lease_ttl must be at least 3× nodes.heartbeat_interval so one missed heartbeat does not expire leases")
	}
	v.durationRange("nodes.disconnect_grace", c.Nodes.DisconnectGrace, 0, time.Hour)
	v.durationRange("nodes.enrollment_ttl", c.Nodes.EnrollmentTTL, time.Minute, 30*24*time.Hour)
	v.durationRange("nodes.command_timeout", c.Nodes.CommandTimeout, time.Second, 10*time.Minute)

	switch c.Harness.MaxPermissionMode {
	case "plan", "default", "dontAsk", "acceptEdits", "auto", "bypassPermissions":
	default:
		v.add("harness.max_permission_mode must be plan, default, dontAsk, acceptEdits, auto or bypassPermissions, got %q", c.Harness.MaxPermissionMode)
	}

	switch c.Artifacts.Backend {
	case "local":
		if c.Artifacts.LocalDir != "" && !filepath.IsAbs(c.Artifacts.LocalDir) {
			v.add("artifacts.local_dir must be an absolute path")
		}
	case "s3":
		s := c.Artifacts.S3
		if s.Bucket == "" || s.Endpoint == "" {
			v.add("artifacts.s3.bucket and artifacts.s3.endpoint are required for the s3 backend")
		}
		if !filepath.IsAbs(s.AccessKeyFile) || !filepath.IsAbs(s.SecretKeyFile) {
			v.add("artifacts.s3.access_key_file and artifacts.s3.secret_key_file must be absolute paths")
		}
		if s.CAFile != "" && !filepath.IsAbs(s.CAFile) {
			v.add("artifacts.s3.ca_file must be an absolute path")
		}
		if !validS3Prefix(s.Prefix) {
			v.add("artifacts.s3.prefix must be empty or like spaceman/artifacts/ (letters, digits, - _ . /; ends with a slash)")
		}
		if s.InsecureAllowed && prod {
			v.add("artifacts.s3.insecure_allowed is development-only")
		}
		if b := c.Backups.S3; c.Backups.Target == "s3" && b.Bucket == s.Bucket && b.Endpoint == s.Endpoint &&
			(strings.HasPrefix(b.Prefix, s.Prefix) || strings.HasPrefix(s.Prefix, b.Prefix)) {
			v.add("artifacts.s3.prefix and backups.s3.prefix overlap in the same bucket: give each its own prefix")
		}
	default:
		v.add("artifacts.backend must be \"local\" or \"s3\", got %q", c.Artifacts.Backend)
	}
	v.outbound(c.Events.Outbound, prod)
	v.backups(c.Backups, prod)
	v.limits(c)
	if a := c.Retention.AuditDays; a != 0 && (a < 30 || a > 36_500) {
		v.add("retention.audit_days must be 0 (keep forever) or within 30..36500, got %d", a)
	}
	v.durationRange("shutdown.timeout", c.Shutdown.Timeout, time.Second, 10*time.Minute)
	return v.err()
}

// reSource bounds events.outbound.source: it is sent as ce-source and as a header value.
var reSource = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

func (v *validator) outbound(o OutboundConfig, prod bool) {
	if !reSource.MatchString(o.Source) {
		v.add("events.outbound.source must match %s, got %q", reSource, o.Source)
	}
	v.durationRange("events.outbound.retention", o.Retention, time.Hour, 90*24*time.Hour)
	if !o.Enabled {
		return
	}
	u, err := url.Parse(o.URL)
	switch {
	case o.URL == "":
		v.add("events.outbound.url is required when events.outbound.enabled is true")
	case err != nil || u.Host == "" || (u.Scheme != "nats" && u.Scheme != "tls"):
		v.add("events.outbound.url must be nats://host:port or tls://host:port, got %q", o.URL)
	case u.User != nil:
		v.add("events.outbound.url must not contain credentials; use events.outbound.creds_file")
	case prod && u.Scheme != "tls" && !isLoopback(u.Hostname()):
		v.add("events.outbound.url must use tls:// in production")
	}
	if !filepath.IsAbs(o.CredsFile) {
		v.add("events.outbound.creds_file must be an absolute path when events.outbound.enabled is true")
	}
	if o.CAFile != "" && !filepath.IsAbs(o.CAFile) {
		v.add("events.outbound.ca_file must be an absolute path")
	}
}

func (v *validator) backups(b BackupsConfig, prod bool) {
	v.durationRange("backups.interval", b.Interval, 15*time.Minute, 7*24*time.Hour)
	if b.KeepLast < 1 || b.KeepLast > 1000 {
		v.add("backups.keep_last must be within 1..1000, got %d", b.KeepLast)
	}
	if b.MaxAge != 0 && b.MaxAge.D() < b.Interval.D() {
		v.add("backups.max_age must be 0 (no age limit) or at least backups.interval")
	}
	switch b.Target {
	case "local":
		if b.LocalDir != "" && !filepath.IsAbs(b.LocalDir) {
			v.add("backups.local_dir must be an absolute path")
		}
	case "s3":
		s := b.S3
		if s.Bucket == "" || s.Endpoint == "" {
			v.add("backups.s3.bucket and backups.s3.endpoint are required for the s3 target")
		}
		if !filepath.IsAbs(s.AccessKeyFile) || !filepath.IsAbs(s.SecretKeyFile) {
			v.add("backups.s3.access_key_file and backups.s3.secret_key_file must be absolute paths")
		}
		if s.CAFile != "" && !filepath.IsAbs(s.CAFile) {
			v.add("backups.s3.ca_file must be an absolute path")
		}
		if s.InsecureAllowed && prod {
			v.add("backups.s3.insecure_allowed is development-only")
		}
	default:
		v.add("backups.target must be \"local\" or \"s3\", got %q", b.Target)
	}
}

var reS3Prefix = regexp.MustCompile(`^([A-Za-z0-9._-]+/)*$`)

// validS3Prefix accepts "" or a slash-terminated prefix of safe segments (no . or .. segments).
func validS3Prefix(p string) bool {
	if len(p) > 256 || !reS3Prefix.MatchString(p) {
		return false
	}
	for _, part := range strings.Split(strings.TrimSuffix(p, "/"), "/") {
		if part == "." || part == ".." {
			return false
		}
	}
	return true
}

// LimitRanges are the bounds of every limit in the limits model, keyed by its limits.* key: installation defaults and
// space overrides are both checked against them (ADR-0036). 0 is allowed only where the lower bound is 0 ("no limit").
var LimitRanges = map[string][2]int64{
	"max_concurrent_runs":       {1, 256},
	"max_top_level_tasks":       {1, 20},
	"max_kb_sessions":           {1, 64},
	"support_depth":             {1, 3},
	"max_human_pending":         {1, 5000},
	"max_human_pending_per_run": {1, 500},
	"max_tool_calls_per_run":    {0, 1_000_000},
	"ingest_per_minute":         {1, 60_000},
	"event_depth":               {1, 64},
	"task_deadline_minutes":     {0, 43_200},
	"storage_quota_bytes":       {1 << 20, 1 << 40},
	"budget_tokens_24h":         {0, 1 << 40},
	"budget_cost_cents_24h":     {0, 100_000_000},
	"budget_tokens_per_run":     {0, 1 << 40},
	"budget_warn_percent":       {1, 100},
	// Retention periods in days (0: keep) and storage bounds (ADR-0045).
	"retention_recordings_days":    {0, 36_500},
	"retention_transcripts_days":   {0, 36_500},
	"retention_attachments_days":   {0, 36_500},
	"retention_human_actions_days": {0, 36_500},
	"recording_quota_bytes":        {1 << 20, 64 << 30},
	"export_max_bytes":             {1 << 20, 64 << 30},
	// Control lifecycle (ADR-0056): a switch (0 off, 1 on) and minutes (0: never).
	"control_autostart":         {0, 1},
	"control_idle_stop_minutes": {0, 10_080},
	// A new run's terminal size (ADR-0075): 0 is the node's runner.terminal_size; otherwise the terminal size bounds
	// (TerminalLimitFloor).
	"terminal_cols": {0, proto.TerminalMaxCols},
	"terminal_rows": {0, proto.TerminalMaxRows},
}

// TerminalLimitFloor is the smallest value other than 0 that the terminal_cols and terminal_rows limits accept.
var TerminalLimitFloor = map[string]int64{"terminal_cols": proto.TerminalMinCols, "terminal_rows": proto.TerminalMinRows}

// LimitValues returns the installation default of every limit, keyed like LimitRanges.
func (c *Config) LimitValues() map[string]int64 {
	l := c.Limits
	return map[string]int64{"max_concurrent_runs": l.MaxConcurrentRuns, "max_top_level_tasks": l.MaxTopLevelTasks,
		"max_kb_sessions": l.MaxKBSessions, "support_depth": l.SupportDepth, "max_human_pending": l.MaxHumanPending,
		"max_human_pending_per_run": l.MaxHumanPendingPerRun, "max_tool_calls_per_run": l.MaxToolCallsPerRun,
		"ingest_per_minute": l.IngestPerMinute, "event_depth": l.EventDepth, "task_deadline_minutes": l.TaskDeadlineMinutes,
		"storage_quota_bytes": c.Artifacts.SpaceQuotaBytes, "budget_tokens_24h": l.BudgetTokens24h,
		"budget_cost_cents_24h": l.BudgetCostCents24h, "budget_tokens_per_run": l.BudgetTokensPerRun, "budget_warn_percent": l.BudgetWarnPercent,
		"retention_recordings_days": l.RetentionRecordingsDays, "retention_transcripts_days": l.RetentionTranscriptsDays,
		"retention_attachments_days": l.RetentionAttachmentsDays, "retention_human_actions_days": l.RetentionHumanActionsDays,
		"recording_quota_bytes": l.RecordingQuotaBytes, "export_max_bytes": l.ExportMaxBytes,
		"control_autostart": l.ControlAutostart, "control_idle_stop_minutes": l.ControlIdleStopMinutes,
		"terminal_cols": l.TerminalCols, "terminal_rows": l.TerminalRows}
}

func (v *validator) limits(c *Config) {
	vals := c.LimitValues()
	for _, k := range slices.Sorted(maps.Keys(vals)) {
		n, r := vals[k], LimitRanges[k]
		name := "limits." + k
		if k == "storage_quota_bytes" {
			name = "artifacts.space_quota_bytes"
		}
		if n < r[0] || n > r[1] {
			v.add("%s must be within %d..%d, got %d", name, r[0], r[1], n)
		} else if floor := TerminalLimitFloor[k]; n != 0 && n < floor {
			v.add("%s must be 0 (the node's default) or within %d..%d, got %d", name, floor, r[1], n)
		}
	}
}

// hcSessionRe mirrors herdr-core session naming (lowercase token).
var hcSessionRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// MaxSocketPath is the longest unix socket path accepted on every supported platform (macOS sun_path is 104 bytes
// including the terminating NUL; herdr-core reports "this system allows 103").
const MaxSocketPath = 103

// Validate checks the node agent configuration.
func (c *AgentConfig) Validate() error {
	v := &validator{}
	v.mode(c.Mode)
	prod := c.IsProduction()
	if !filepath.IsAbs(c.DataDir) {
		v.add("data_dir must be an absolute path")
	}
	v.publicURL("central.url", c.Central.URL, prod)
	if !filepath.IsAbs(c.Central.CredentialFile) {
		v.add("central.credential_file must be an absolute path")
	}
	v.durationRange("central.reconnect_max", c.Central.ReconnectMax, time.Second, 10*time.Minute)
	if c.Runner.HCBinary == "" {
		v.add("runner.hc_binary is required")
	}
	if !hcSessionRe.MatchString(c.Runner.Session) {
		v.add("runner.session must match %s", hcSessionRe)
	} else if c.Runner.Session == "default" {
		v.add("runner.session must not be \"default\": SpaceMan never shares the operator's herdr-core core")
	}
	if !filepath.IsAbs(c.Runner.Socket) {
		v.add("runner.socket must be an absolute path")
	} else if len(c.Runner.Socket) > MaxSocketPath {
		v.add("runner.socket is %d bytes; unix socket paths must be at most %d bytes", len(c.Runner.Socket), MaxSocketPath)
	}
	if _, _, err := ParseTerminalSize(c.Runner.TerminalSize); err != nil {
		v.add("runner.terminal_size: %v", err)
	}
	if c.ManagementListen != "" {
		v.listen("management_listen", c.ManagementListen)
	}
	if c.Capacity < 1 || c.Capacity > 256 {
		v.add("capacity must be within 1..256")
	}
	v.logConfig(c.Log)
	return v.err()
}

func defaultLog() obs.LogConfig { return obs.LogConfig{Level: "info", Format: "json"} }

type validator struct{ problems []string }

func (v *validator) add(format string, args ...any) {
	v.problems = append(v.problems, fmt.Sprintf(format, args...))
}

func (v *validator) mode(m Mode) {
	switch m {
	case ModeProduction, ModeDevelopment:
	default:
		v.add("mode must be %q or %q, got %q", ModeProduction, ModeDevelopment, m)
	}
}

// publicURL requires an absolute http(s) URL without credentials, query or fragment; production requires https,
// except for loopback hosts.
func (v *validator) publicURL(name, raw string, prod bool) {
	if raw == "" {
		v.add("%s is required", name)
		return
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		v.add("%s must be an absolute http(s) URL, got %q", name, raw)
		return
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		v.add("%s must not contain credentials, a query or a fragment", name)
	}
	if prod && u.Scheme != "https" && !isLoopback(u.Hostname()) {
		v.add("%s must use https in production", name)
	}
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.IsLoopback()
}

func (v *validator) logConfig(l obs.LogConfig) {
	switch l.Level {
	case "debug", "info", "warn", "error":
	default:
		v.add("log.level must be debug, info, warn or error, got %q", l.Level)
	}
	switch l.Format {
	case "json", "text":
	default:
		v.add("log.format must be json or text, got %q", l.Format)
	}
}

func (v *validator) positive64(name string, n int64) {
	if n <= 0 {
		v.add("%s must be positive", name)
	}
}

func (v *validator) durationRange(name string, d Duration, lo, hi time.Duration) {
	if d.D() < lo || d.D() > hi {
		v.add("%s must be within %s..%s, got %s", name, lo, hi, d.D())
	}
}

func (v *validator) listen(name, addr string) {
	if _, port, err := net.SplitHostPort(addr); err != nil || port == "" {
		v.add("%s must be host:port, got %q", name, addr)
	}
}

func (v *validator) prefix(name, s string) {
	if _, err := netip.ParsePrefix(s); err != nil {
		v.add("%s entry %q is not a CIDR prefix", name, s)
	}
}

func (v *validator) err() error {
	if len(v.problems) == 0 {
		return nil
	}
	return &ValidationError{Problems: v.problems}
}
