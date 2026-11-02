package config

import (
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/obs"
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
	v.durationRange("auth.stream_revalidate", c.Auth.StreamRevalidate, time.Second, 5*time.Minute)
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
		if (s.AccessKeyFile == "") != (s.SecretKeyFile == "") {
			v.add("artifacts.s3.access_key_file and secret_key_file must be set together")
		}
		if s.InsecureAllowed && prod {
			v.add("artifacts.s3.insecure_allowed is development-only")
		}
	default:
		v.add("artifacts.backend must be \"local\" or \"s3\", got %q", c.Artifacts.Backend)
	}
	v.durationRange("shutdown.timeout", c.Shutdown.Timeout, time.Second, 10*time.Minute)
	return v.err()
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
