package harness

import (
	"errors"
	"slices"
	"strings"

	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// gateway is a gateway run's delivery (ADR-0060 decision 6): where the harness sends its model traffic and the run's
// child token.
type gateway struct {
	Origin string
	Token  string
}

// gatewayFor checks a launch for a gateway (nil without one). The address and the token travel together, and a gateway
// launch carries no other credential: the harness is pointed only at the gateway, never at a direct account as well
// (no fallback, Q1). Anything else is a central bug, and the launch is refused rather than started on a guess.
func gatewayFor(p proto.LaunchPayload) (*gateway, error) {
	tok := p.Secrets[proto.SecretGatewayToken]
	if p.Gateway == nil {
		if _, ok := p.Secrets[proto.SecretGatewayToken]; ok {
			return nil, errors.New("gateway: the launch carries a gateway token without the gateway's address")
		}
		return nil, nil
	}
	if tok == "" {
		return nil, errors.New("gateway: the launch names a gateway but carries no token")
	}
	if strings.ContainsAny(tok, "\x00\r\n") {
		return nil, errors.New("gateway: the token is malformed")
	}
	if p.AccountRef != "" {
		return nil, errors.New("gateway: a gateway launch can't name an account")
	}
	for k := range p.Secrets {
		if k != proto.SecretGatewayToken {
			return nil, errors.New("gateway: a gateway launch carries another credential (" + k + ")")
		}
	}
	origin, err := p.Gateway.Origin()
	if err != nil {
		return nil, err
	}
	return &gateway{Origin: origin, Token: tok}, nil
}

// claudeCredentialSettings are Claude Code settings.json keys that give Claude Code another credential or credential
// source (verified in the 2.1.317 settings schema): a gateway run refuses a profile that sets them.
var claudeCredentialSettings = []string{"apiKeyHelper", "proxyAuthHelper", "awsCredentialExport", "awsAuthRefresh", "gcpAuthRefresh", "forceLoginMethod"}

// claudeCredentialEnv reports settings.json env variables that would give a gateway run another credential or another
// endpoint, or let something else see the token on its way (Claude Code applies settings env to its own process):
//   - every ANTHROPIC_* (a key, a token, a base URL; this also refuses harmless model pins such as ANTHROPIC_MODEL);
//   - every CLAUDE_CODE_* except claudeSafeEnv: 2.1.317 has over 800 of them, among them OAuth tokens, key and token
//     file descriptors, host-managed auth, other base URLs, proxies, client certificates and the switches to another
//     provider (Bedrock, Vertex, Foundry), so new ones are refused until someone checks them;
//   - the transport variables that reroute or open up its TLS: HTTP(S)_PROXY and ALL_PROXY in either case,
//     NODE_TLS_REJECT_UNAUTHORIZED, NODE_EXTRA_CA_CERTS, SSL_CERT_FILE, SSL_CERT_DIR and NODE_OPTIONS.
func claudeCredentialEnv(name string) bool {
	switch {
	case strings.HasPrefix(name, "ANTHROPIC_"):
		return true
	case strings.HasPrefix(name, "CLAUDE_CODE_"):
		return !claudeSafeEnv[name]
	}
	return claudeTransportEnv[strings.ToUpper(name)]
}

// claudeSafeEnv are the CLAUDE_CODE_* variables a gateway run's profile may set: output, UI and behaviour switches
// that carry no credential or endpoint (each checked in the 2.1.317 binary).
var claudeSafeEnv = map[string]bool{
	"CLAUDE_CODE_AUTO_COMPACT_WINDOW":          true,
	"CLAUDE_CODE_DISABLE_1M_CONTEXT":           true,
	"CLAUDE_CODE_DISABLE_ADAPTIVE_THINKING":    true,
	"CLAUDE_CODE_DISABLE_ALTERNATE_SCREEN":     true,
	"CLAUDE_CODE_DISABLE_AUTO_MEMORY":          true,
	"CLAUDE_CODE_DISABLE_BACKGROUND_TASKS":     true,
	"CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS":   true,
	"CLAUDE_CODE_DISABLE_FEEDBACK_SURVEY":      true,
	"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": true,
	"CLAUDE_CODE_DISABLE_TERMINAL_TITLE":       true,
	"CLAUDE_CODE_DISABLE_THINKING":             true,
	"CLAUDE_CODE_EFFORT_LEVEL":                 true,
	"CLAUDE_CODE_MAX_OUTPUT_TOKENS":            true,
	"CLAUDE_CODE_MAX_RETRIES":                  true,
	"CLAUDE_CODE_MAX_TOOL_USE_CONCURRENCY":     true,
	"CLAUDE_CODE_NO_FLICKER":                   true,
	"CLAUDE_CODE_SUBAGENT_MODEL":               true,
}

// claudeTransportEnv are the variables (upper-cased) that send Claude Code's traffic through a proxy or weaken its TLS
// checks: with allow_insecure_http a proxy would see the token in clear text, and a trusted extra CA lets one
// impersonate the gateway.
var claudeTransportEnv = map[string]bool{
	"HTTP_PROXY": true, "HTTPS_PROXY": true, "ALL_PROXY": true,
	"NODE_TLS_REJECT_UNAUTHORIZED": true, "NODE_EXTRA_CA_CERTS": true, "SSL_CERT_FILE": true, "SSL_CERT_DIR": true,
	"NODE_OPTIONS": true,
}

// gatewayClaudeSettings refuses a profile's Claude Code settings that would put another credential or endpoint next to
// the gateway's.
func gatewayClaudeSettings(s proto.ClaudeSettings) error {
	for _, k := range claudeCredentialSettings {
		if _, ok := s[k]; ok {
			return errors.New("claude: a gateway run's claude_settings can't set " + k + " (it would give Claude Code another credential)")
		}
	}
	env, _ := s["env"].(map[string]any)
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		if claudeCredentialEnv(k) {
			return errors.New("claude: a gateway run's claude_settings can't set env." + k + " (it would give Claude Code another credential or endpoint)")
		}
	}
	return nil
}
