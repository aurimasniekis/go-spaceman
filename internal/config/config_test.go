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
