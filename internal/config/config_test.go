package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func writeFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadDefaultsNeedPublicURL(t *testing.T) {
	_, err := Load("", nil)
	if err == nil || !strings.Contains(err.Error(), "public_url is required") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadFileAndEnv(t *testing.T) {
	p := writeFile(t, "public_url: https://spaceman.example.com\nnodes:\n  disconnect_grace: 90s\n")
	cfg, err := Load(p, []string{"SPACEMAN_HTTP_LISTEN=:9443", "SPACEMAN_AGENT_DATA_DIR=/x", "SPACEMAN_KEK=ignored", "PATH=/bin"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTP.Listen != ":9443" || cfg.Nodes.DisconnectGrace.D() != 90*time.Second {
		t.Errorf("cfg = %+v", cfg)
	}
}

func TestUnknownKeysRejected(t *testing.T) {
	p := writeFile(t, "public_url: https://x.example.com\nbogus: 1\n")
	if _, err := Load(p, nil); err == nil || !strings.Contains(err.Error(), "bogus") {
		t.Errorf("unknown yaml key: %v", err)
	}
	p = writeFile(t, "public_url: https://x.example.com\n")
	if _, err := Load(p, []string{"SPACEMAN_HTTP_LISTN=:1"}); err == nil || !strings.Contains(err.Error(), "SPACEMAN_HTTP_LISTN") {
		t.Errorf("unknown env: %v", err)
	}
	if _, err := Load(writeFile(t, "a: 1\n---\nb: 2\n"), nil); err == nil {
		t.Error("multiple documents accepted")
	}
}

func TestProductionRestrictions(t *testing.T) {
	cfg := Default()
	cfg.PublicURL = "http://spaceman.example.com"
	cfg.Auth.RequireTOTP = false
	cfg.Auth.PasswordMinLength = 8
	cfg.HTTP.ManagementListen = cfg.HTTP.Listen
	cfg.Nodes.LeaseTTL = cfg.Nodes.HeartbeatInterval
	err := cfg.Validate()
	for _, want := range []string{"must use https", "require_totp", "password_min_length", "management_listen must differ", "lease_ttl"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in %v", want, err)
		}
	}
	cfg.Mode = ModeDevelopment
	cfg.PublicURL = "http://127.0.0.1:8080"
	cfg.HTTP.ManagementListen = "127.0.0.1:9090"
	cfg.Nodes.LeaseTTL = Duration(45 * time.Second)
	if err := cfg.Validate(); err != nil {
		t.Errorf("development config rejected: %v", err)
	}
	cfg.Mode = ModeProduction
	cfg.Auth.RequireTOTP, cfg.Auth.PasswordMinLength = true, 12
	if err := cfg.Validate(); err != nil {
		t.Errorf("loopback http must be allowed in production: %v", err)
	}
}

func TestAgentValidation(t *testing.T) {
	cfg := DefaultAgent()
	cfg.Central.URL = "https://spaceman.example.com"
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	cfg.Runner.Session = "default"
	cfg.Runner.Socket = "/" + strings.Repeat("s", MaxSocketPath)
	err := cfg.Validate()
	for _, want := range []string{"must not be \"default\"", "unix socket paths must be at most"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in %v", want, err)
		}
	}
}

func TestLoadAgentEnv(t *testing.T) {
	cfg, err := LoadAgent("", []string{"SPACEMAN_AGENT_CENTRAL_URL=https://c.example.com", "SPACEMAN_AGENT_CAPACITY=8", "SPACEMAN_HTTP_LISTEN=:1"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Capacity != 8 || cfg.Central.URL != "https://c.example.com" {
		t.Errorf("cfg = %+v", cfg)
	}
}

func TestEnvKeysCoverEveryField(t *testing.T) {
	keys := EnvKeys()
	for _, k := range []string{"SPACEMAN_HTTP_MANAGEMENT_LISTEN", "SPACEMAN_NODES_LEASE_TTL", "SPACEMAN_ARTIFACTS_S3_BUCKET"} {
		if !slices.Contains(keys, k) {
			t.Errorf("missing %s", k)
		}
	}
	if !slices.Contains(AgentEnvKeys(), "SPACEMAN_AGENT_RUNNER_SOCKET") {
		t.Error("missing agent runner socket key")
	}
}

// SM-OBS-004: outbound publication is off by default; enabling it needs a Pulse URL (tls:// in production) and an
// absolute creds file, and the source is a bounded token.
func TestOutboundEvents(t *testing.T) {
	cfg := Default()
	cfg.PublicURL = "https://spaceman.example.com"
	if cfg.Events.Outbound.Enabled || cfg.Events.Outbound.Source != "spaceman" || cfg.Validate() != nil {
		t.Fatalf("defaults %+v %v", cfg.Events.Outbound, cfg.Validate())
	}
	cfg.Events.Outbound.Enabled = true
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "events.outbound.url is required") || !strings.Contains(err.Error(), "creds_file") {
		t.Fatalf("enabled without url: %v", err)
	}
	cfg.Events.Outbound.URL, cfg.Events.Outbound.CredsFile = "nats://pulse.example.com:4222", "/etc/spaceman/pulse.creds"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "tls://") {
		t.Fatalf("plain nats in production: %v", err)
	}
	for _, bad := range []string{"https://pulse.example.com", "tls://user:pw@pulse.example.com:4222", "::"} {
		cfg.Events.Outbound.URL = bad
		if cfg.Validate() == nil {
			t.Errorf("accepted url %q", bad)
		}
	}
	cfg.Events.Outbound.URL, cfg.Events.Outbound.CAFile = "tls://pulse.example.com:4222", "ca.pem"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "ca_file") {
		t.Fatalf("relative ca: %v", err)
	}
	cfg.Events.Outbound.CAFile = "/etc/spaceman/pulse-ca.pem"
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	cfg.Events.Outbound.Source = "Space Man\r\n"
	cfg.Events.Outbound.Retention = Duration(time.Minute)
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "source") || !strings.Contains(err.Error(), "retention") {
		t.Fatalf("source/retention: %v", err)
	}
	c2, err := Load(writeFile(t, "public_url: https://x.example.com\n"), []string{"SPACEMAN_EVENTS_OUTBOUND_ENABLED=true",
		"SPACEMAN_EVENTS_OUTBOUND_URL=nats://127.0.0.1:4222", "SPACEMAN_EVENTS_OUTBOUND_CREDS_FILE=/run/pulse.creds", "SPACEMAN_EVENTS_OUTBOUND_SOURCE=spaceman-lab"})
	if err != nil || !c2.Events.Outbound.Enabled || c2.Events.Outbound.Source != "spaceman-lab" {
		t.Fatalf("env: %+v %v", c2, err)
	}
}

// SM-AUTH-007 (ADR-0033): the periodic stream re-check is bounded to 1 s … 60 s.
func TestStreamRevalidateBounds(t *testing.T) {
	for d, ok := range map[time.Duration]bool{time.Second: true, 30 * time.Second: true, time.Minute: true, 61 * time.Second: false, 500 * time.Millisecond: false} {
		cfg := Default()
		cfg.Mode = ModeDevelopment
		cfg.PublicURL = "http://127.0.0.1:8080"
		cfg.Auth.StreamRevalidate = Duration(d)
		err := cfg.Validate()
		if ok != (err == nil) || (!ok && !strings.Contains(err.Error(), "stream_revalidate")) {
			t.Errorf("stream_revalidate %s: %v", d, err)
		}
	}
}

// The command-line client's and the enrollment command's variables may be exported on the central host; central
// ignores them instead of refusing to start, and still rejects real typos.
func TestClientVariablesIgnoredByCentral(t *testing.T) {
	env := []string{"SPACEMAN_PUBLIC_URL=https://spaceman.example.com", "SPACEMAN_SERVER=https://spaceman.example.com",
		"SPACEMAN_TOKEN_FILE=/home/op/.config/spaceman/token", "SPACEMAN_OUTPUT=json", "SPACEMAN_SPACE=payments",
		"SPACEMAN_ALLOW_INSECURE_HTTP=1", "SPACEMAN_ALLOW_NETWORK_FS=1", "SPACEMAN_ENROLL_TOKEN=x", "SPACEMAN_ENDPOINT=https://spaceman.example.com"}
	if _, err := Load("", env); err != nil {
		t.Fatal(err)
	}
	if _, err := Load("", append(env, "SPACEMAN_HTTP_LISTN=:1")); err == nil || !strings.Contains(err.Error(), "SPACEMAN_HTTP_LISTN") {
		t.Fatalf("typo accepted: %v", err)
	}
}

// SM-STORE-002 (ADR-0048): the S3 artifact backend needs a bucket, absolute key files and a safe prefix that does not
// overlap the backups' prefix in the same bucket.
func TestArtifactsS3Validation(t *testing.T) {
	base := func() *Config {
		cfg := Default()
		cfg.PublicURL = "https://spaceman.example.com"
		cfg.Artifacts.Backend = "s3"
		cfg.Artifacts.S3 = S3Config{Endpoint: "https://s3.example.com", Bucket: "sm", Prefix: "spaceman/artifacts/",
			AccessKeyFile: "/etc/spaceman/s3-ak", SecretKeyFile: "/etc/spaceman/s3-sk"}
		return cfg
	}
	if err := base().Validate(); err != nil {
		t.Fatalf("valid s3 backend: %v", err)
	}
	for name, c := range map[string]struct {
		mod  func(*Config)
		want string
	}{
		"bucket":   {func(c *Config) { c.Artifacts.S3.Bucket = "" }, "artifacts.s3.bucket"},
		"key file": {func(c *Config) { c.Artifacts.S3.SecretKeyFile = "sk" }, "absolute paths"},
		"no keys":  {func(c *Config) { c.Artifacts.S3.AccessKeyFile, c.Artifacts.S3.SecretKeyFile = "", "" }, "absolute paths"},
		"ca":       {func(c *Config) { c.Artifacts.S3.CAFile = "ca.pem" }, "artifacts.s3.ca_file"},
		"prefix":   {func(c *Config) { c.Artifacts.S3.Prefix = "../x/" }, "artifacts.s3.prefix must"},
		"no slash": {func(c *Config) { c.Artifacts.S3.Prefix = "x" }, "artifacts.s3.prefix must"},
		"insecure": {func(c *Config) { c.Artifacts.S3.InsecureAllowed = true }, "development-only"},
		"overlap": {func(c *Config) {
			c.Backups.Target = "s3"
			c.Backups.S3 = c.Artifacts.S3
			c.Backups.S3.Prefix = "spaceman/"
		}, "overlap"},
	} {
		cfg := base()
		c.mod(cfg)
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	cfg := base()
	cfg.Backups.Target = "s3"
	cfg.Backups.S3 = cfg.Artifacts.S3
	cfg.Backups.S3.Prefix = "spaceman/backups/"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("separate prefixes in one bucket: %v", err)
	}
}

// nodes.lease_ttl is bounded above by MaxLeaseTTL: the stale-run sweeper's lease margin assumes a node never holds a lease
// longer than that past central's last renewal or revoke (ADR-0074).
func TestLeaseTTLBound(t *testing.T) {
	cfg := Default()
	cfg.PublicURL = "https://spaceman.example.com"
	cfg.Nodes.LeaseTTL = Duration(MaxLeaseTTL)
	if err := cfg.Validate(); err != nil {
		t.Fatalf("lease_ttl at the bound rejected: %v", err)
	}
	cfg.Nodes.LeaseTTL = Duration(MaxLeaseTTL + time.Second)
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "nodes.lease_ttl must be at most 10m0s") {
		t.Fatalf("lease_ttl past the bound: %v", err)
	}
}

// Node-managed Claude Code (design 2026-11-12 §3): off by default, channel latest, https-only base URL.
func TestClaudeCodeConfig(t *testing.T) {
	cfg, err := LoadAgent("", []string{
		"SPACEMAN_AGENT_CENTRAL_URL=https://c.example.com",
		"SPACEMAN_AGENT_HARNESS_CLAUDE_CHANNEL=2.1.322",
		"SPACEMAN_AGENT_HARNESS_CLAUDE_REFRESH=1h",
	})
	if err != nil {
		t.Fatal(err)
	}
	c := cfg.Harness.Claude
	if c.Manage || c.SelfUpdate || c.Channel != "2.1.322" || c.Refresh.D() != time.Hour || c.CacheKeep != 3 || c.CacheKeepPinned != 5 ||
		c.BaseURL != "https://downloads.claude.ai/claude-code-releases" || c.DownloadTimeout.D() != 15*time.Minute {
		t.Fatalf("harness.claude = %+v", c)
	}
	if d := DefaultAgent().Harness.Claude; d.Manage || d.Channel != "latest" {
		t.Fatalf("defaults %+v", d)
	}

	bad := DefaultAgent()
	bad.Central.URL = "https://c.example.com"
	bad.Harness.Claude = ClaudeCodeConfig{
		Channel: "rc", BaseURL: "http://mirror.example.com", CacheKeep: 0,
		Refresh: Duration(time.Second), DownloadTimeout: Duration(3 * time.Hour), Manage: true, SelfUpdate: true,
	}
	err = bad.Validate()
	for _, want := range []string{
		"harness.claude.channel must be latest, stable or an exact X.Y.Z", "harness.claude.base_url", "must use https",
		"harness.claude.cache_keep must be within 1..50", "harness.claude.cache_keep_pinned must be within 1..50", "harness.claude.refresh must be within",
		"harness.claude.download_timeout must be within", "harness.claude.self_update can't be on with harness.claude.manage",
	} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in %v", want, err)
		}
	}
}
