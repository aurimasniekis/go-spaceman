// Package oidc implements OpenID Connect sign-in for users (SM-AUTH-001/002, ADR-0029): provider configuration, the
// Authorization Code flow with PKCE (S256), ID-token validation, identity linking, just-in-time provisioning and
// group → grant mapping.
//
// Derived from github.com/aurimasniekis/go-hallpass internal/oidc at commit bdd8329 (Apache-2.0, same author), with
// operators replaced by users and Hallpass roles and operator groups replaced by SpaceMan grants: a mapping turns a
// groups-claim value into a role in a domain ("*" or one space). Mapped grants are stored under the user's SSO subject
// ("user:<id>#sso") and apply only to SSO sessions (Hallpass SR-26).
package oidc

import (
	"context"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"encoding/pem"
	"errors"
	"log/slog"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/keyring"
	"github.com/aurimasniekis/go-spaceman/internal/obs"
	"github.com/aurimasniekis/go-spaceman/internal/obs/metrics"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Provisioning modes (Hallpass HP-OIDC-010).
const (
	Preprovisioned = "preprovisioned"
	JIT            = "jit"
)

// Step-up fallbacks (Hallpass HP-OIDC-013).
const (
	StepUpNone      = "none"
	StepUpLocalTOTP = "local_totp"
)

// Provider is an OIDC provider configuration. The client secret is write-only (Hallpass HP-OIDC-007).
type Provider struct {
	ID                        string     `json:"id"`
	Slug                      string     `json:"slug"`
	DisplayName               string     `json:"display_name"`
	Issuer                    string     `json:"issuer"`
	ClientID                  string     `json:"client_id"`
	HasClientSecret           bool       `json:"has_client_secret"`
	Scopes                    []string   `json:"scopes"`
	GroupsClaim               string     `json:"groups_claim" doc:"Claim holding the user's groups; a dotted path walks nested objects (Porch: porch.groups)"`
	Provisioning              string     `json:"provisioning" enum:"preprovisioned,jit"`
	JIT                       JITRules   `json:"jit"`
	AllowPrivateNetwork       bool       `json:"allow_private_network"`
	AllowCrossOriginEndpoints bool       `json:"allow_cross_origin_endpoints"`
	CABundlePEM               string     `json:"ca_bundle_pem"`
	StepUpFallback            string     `json:"stepup_fallback" enum:"none,local_totp"`
	ACRValues                 []string   `json:"acr_values"`
	EndSessionEnabled         bool       `json:"end_session_enabled" doc:"Signing out of an SSO session also sends the browser to the provider's end_session_endpoint (RP-initiated logout), when it advertises one"`
	Enabled                   bool       `json:"enabled"`
	TestLoginOKAt             *time.Time `json:"test_login_ok_at,omitempty"`
	TestLoginBy               string     `json:"test_login_by,omitempty"`
	Version                   int64      `json:"version"`
	CreatedAt                 time.Time  `json:"created_at"`
	UpdatedAt                 time.Time  `json:"updated_at"`

	secretEnc []byte
}

// JITRules restrict just-in-time provisioning; at least one rule is required in JIT mode and every configured rule
// must match (Hallpass HP-OIDC-010).
type JITRules struct {
	// EmailDomains require email_verified=true and an email in one of these domains.
	EmailDomains []string `json:"email_domains" doc:"Require a verified email in one of these domains"`
	// Tenants match the "tid" claim (Microsoft Entra ID).
	Tenants []string `json:"tenants" doc:"Allowed tid claim values (Microsoft Entra ID)"`
	// HostedDomains match the "hd" claim (Google Workspace).
	HostedDomains []string `json:"hosted_domains" doc:"Allowed hd claim values (Google Workspace)"`
	// Groups require at least one of these values in the groups claim.
	Groups []string `json:"groups" doc:"Require at least one of these values in the groups claim"`
}

func (j JITRules) empty() bool {
	return len(j.EmailDomains) == 0 && len(j.Tenants) == 0 && len(j.HostedDomains) == 0 && len(j.Groups) == 0
}

func (j JITRules) normalized() JITRules {
	n := func(s []string) []string {
		if s == nil {
			return []string{}
		}
		return s
	}
	return JITRules{EmailDomains: n(j.EmailDomains), Tenants: n(j.Tenants), HostedDomains: n(j.HostedDomains), Groups: n(j.Groups)}
}

// ProviderInput creates or updates a provider. Nil fields are left unchanged on update. ClientSecret: nil keeps the
// stored secret, "" removes it.
type ProviderInput struct {
	Version                   int64     `json:"-"`
	Slug                      *string   `json:"slug,omitempty" maxLength:"32"`
	DisplayName               *string   `json:"display_name,omitempty" maxLength:"64"`
	Issuer                    *string   `json:"issuer,omitempty" maxLength:"512"`
	ClientID                  *string   `json:"client_id,omitempty" maxLength:"256"`
	ClientSecret              *string   `json:"client_secret,omitempty" maxLength:"1024" doc:"Write-only. Omit to keep the stored secret; an empty string removes it"`
	Scopes                    *[]string `json:"scopes,omitempty" maxItems:"32"`
	GroupsClaim               *string   `json:"groups_claim,omitempty" maxLength:"64"`
	Provisioning              *string   `json:"provisioning,omitempty" enum:"preprovisioned,jit"`
	JIT                       *JITRules `json:"jit,omitempty"`
	AllowPrivateNetwork       *bool     `json:"allow_private_network,omitempty"`
	AllowCrossOriginEndpoints *bool     `json:"allow_cross_origin_endpoints,omitempty"`
	CABundlePEM               *string   `json:"ca_bundle_pem,omitempty" maxLength:"65536"`
	StepUpFallback            *string   `json:"stepup_fallback,omitempty" enum:"none,local_totp"`
	ACRValues                 *[]string `json:"acr_values,omitempty" maxItems:"16"`
	EndSessionEnabled         *bool     `json:"end_session_enabled,omitempty" doc:"RP-initiated logout at the provider when signing out of an SSO session"`
	Enabled                   *bool     `json:"enabled,omitempty"`
}

// Mapping maps one groups-claim value to a SpaceMan role in a domain: "*" (every space and global administration) or
// one space (Hallpass HP-OIDC-011, adapted to grants).
type Mapping struct {
	ID         string     `json:"id"`
	ProviderID string     `json:"provider_id"`
	GroupValue string     `json:"group_value"`
	Role       authz.Role `json:"role" enum:"viewer,operator,admin"`
	Domain     string     `json:"domain" doc:"A space ID, or \"*\" for every space and global administration"`
	CreatedAt  time.Time  `json:"created_at"`
}

// MappingInput adds a mapping.
type MappingInput struct {
	GroupValue string     `json:"group_value" minLength:"1" maxLength:"256"`
	Role       authz.Role `json:"role" enum:"viewer,operator,admin"`
	Domain     string     `json:"domain" doc:"A space ID, or \"*\" for every space and global administration"`
}

// Identity is an external identity linked to a user (Hallpass HP-OIDC-009).
type Identity struct {
	Issuer       string     `json:"issuer"`
	Subject      string     `json:"subject"`
	ProviderID   string     `json:"provider_id"`
	ProviderSlug string     `json:"provider_slug"`
	ProviderName string     `json:"provider_name"`
	UserID       string     `json:"user_id"`
	LinkedVia    string     `json:"linked_via" enum:"admin,self_link,jit"`
	CreatedAt    time.Time  `json:"created_at"`
	LastLoginAt  *time.Time `json:"last_login_at,omitempty"`
}

// Options configure the service.
type Options struct {
	// PublicURL is SpaceMan's external base URL; the redirect URI is PublicURL + CallbackPath.
	PublicURL string
	// DevMode allows http:// issuers on localhost (Hallpass HP-OIDC-005).
	DevMode bool
	// StateTTL bounds pending authorizations (default and maximum 10 minutes, Hallpass HP-OIDC-002).
	StateTTL time.Duration
	// HTTPTimeout bounds each request to the provider.
	HTTPTimeout time.Duration
	// JWKSMinRefetch rate-limits JWKS refetches on unknown key IDs (Hallpass HP-OIDC-004).
	JWKSMinRefetch time.Duration
	// Skew is the accepted clock skew for exp/iat/nbf/auth_time (≤ 60 s, Hallpass HP-OIDC-003).
	Skew   time.Duration
	Clock  clock.Clock
	Logger *slog.Logger
	// Metrics counts SSO login completions by provider slug and outcome. Nil disables.
	Metrics *metrics.Metrics
}

// CallbackPath is the single redirect URI path for all providers (the state identifies the provider).
const CallbackPath = "/api/v1/auth/oidc/callback"

// Service manages providers and runs SSO flows.
type Service struct {
	s     *store.Store
	kr    *keyring.Keyring
	audit *audit.Writer
	auth  *auth.Service
	opts  Options
	log   *slog.Logger

	mu     sync.Mutex
	remote map[string]*remote // provider ID → discovered endpoints, HTTP client and key set (per version)
	tests  map[string]testEntry
}

type testEntry struct {
	res *TestResult
	at  time.Time
}

// New returns the OIDC service.
func New(s *store.Store, kr *keyring.Keyring, aw *audit.Writer, au *auth.Service, o Options) *Service {
	if o.StateTTL <= 0 || o.StateTTL > 10*time.Minute {
		o.StateTTL = 10 * time.Minute
	}
	if o.HTTPTimeout <= 0 {
		o.HTTPTimeout = 10 * time.Second
	}
	if o.JWKSMinRefetch <= 0 {
		o.JWKSMinRefetch = 10 * time.Second
	}
	if o.Skew <= 0 || o.Skew > time.Minute {
		o.Skew = time.Minute
	}
	if o.Clock == nil {
		o.Clock = clock.System{}
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	o.PublicURL = strings.TrimRight(o.PublicURL, "/")
	return &Service{s: s, kr: kr, audit: aw, auth: au, opts: o, log: obs.Component(o.Logger, "oidc"), remote: map[string]*remote{},
		tests: map[string]testEntry{}}
}

// RedirectURI is the exact redirect URI registered with every provider (Hallpass HP-OIDC-005).
func (svc *Service) RedirectURI() string { return svc.opts.PublicURL + CallbackPath }

var slugRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// secretAAD binds a sealed client secret to its provider row (as accounts and MCP servers bind theirs).
func secretAAD(id string) []byte { return []byte("oidc_provider:" + id) }

func providerFromRow(r db.OidcProvider) (*Provider, error) {
	p := &Provider{ID: r.ID, Slug: r.Slug, DisplayName: r.DisplayName, Issuer: r.Issuer, ClientID: r.ClientID, HasClientSecret: len(r.ClientSecretEnc) > 0,
		GroupsClaim: r.GroupsClaim, Provisioning: r.Provisioning, AllowPrivateNetwork: r.AllowPrivateNetwork == 1,
		AllowCrossOriginEndpoints: r.AllowCrossOriginEndpoints == 1, CABundlePEM: r.CaBundlePem, StepUpFallback: r.StepupFallback,
		EndSessionEnabled: r.EndSessionEnabled == 1, Enabled: r.Enabled == 1, TestLoginBy: r.TestLoginBy.String, Version: r.Version, ACRValues: []string{},
		CreatedAt: time.UnixMicro(r.CreatedAt).UTC(), UpdatedAt: time.UnixMicro(r.UpdatedAt).UTC(), secretEnc: r.ClientSecretEnc}
	if err := json.Unmarshal([]byte(r.ScopesJson), &p.Scopes); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(r.JitJson), &p.JIT); err != nil {
		return nil, err
	}
	p.JIT = p.JIT.normalized()
	if r.AcrValues != "" {
		p.ACRValues = strings.Fields(r.AcrValues)
	}
	if r.TestLoginOkAt.Valid {
		t := time.UnixMicro(r.TestLoginOkAt.Int64).UTC()
		p.TestLoginOKAt = &t
	}
	return p, nil
}

// GetProvider returns a provider by ID.
func (svc *Service) GetProvider(ctx context.Context, id string) (*Provider, error) {
	r, err := svc.s.R().GetOIDCProvider(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, apperr.New(apperr.CodeNotFound, "provider not found")
	}
	if err != nil {
		return nil, err
	}
	return providerFromRow(r)
}

func (svc *Service) providerBySlug(ctx context.Context, slug string) (*Provider, error) {
	r, err := svc.s.R().GetOIDCProviderBySlug(ctx, slug)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, apperr.New(apperr.CodeNotFound, "provider not found")
	}
	if err != nil {
		return nil, err
	}
	return providerFromRow(r)
}

// ListProviders returns all providers.
func (svc *Service) ListProviders(ctx context.Context) ([]*Provider, error) {
	rows, err := svc.s.R().ListOIDCProviders(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*Provider, 0, len(rows))
	for _, r := range rows {
		p, err := providerFromRow(r)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// PublicProvider is what the sign-in page may show (no configuration details).
type PublicProvider struct {
	Slug        string `json:"slug"`
	DisplayName string `json:"display_name"`
}

// EnabledProviders lists enabled providers for the sign-in page.
func (svc *Service) EnabledProviders(ctx context.Context) ([]PublicProvider, error) {
	ps, err := svc.ListProviders(ctx)
	if err != nil {
		return nil, err
	}
	out := []PublicProvider{}
	for _, p := range ps {
		if p.Enabled {
			out = append(out, PublicProvider{Slug: p.Slug, DisplayName: p.DisplayName})
		}
	}
	return out, nil
}

func invalid(format string, a ...any) error { return apperr.New(apperr.CodeInvalid, format, a...) }

// validateIssuerURL enforces https (or http on localhost in dev mode) and no query/fragment/userinfo (Hallpass
// HP-OIDC-005).
func validateIssuerURL(raw string, dev bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return invalid("issuer must be an absolute URL without credentials, query or fragment")
	}
	switch {
	case u.Scheme == "https":
		return nil
	case u.Scheme == "http" && dev && isLocalhost(u.Hostname()):
		return nil
	}
	return invalid("issuer must use https (http is allowed only for localhost in development mode)")
}

func isLocalhost(h string) bool { return h == "localhost" || h == "127.0.0.1" || h == "::1" }

func (svc *Service) validate(p *Provider) error {
	if !slugRE.MatchString(p.Slug) {
		return invalid("slug must match %s", slugRE)
	}
	if n := strings.TrimSpace(p.DisplayName); n == "" || len(n) > 64 {
		return invalid("display name is required (at most 64 bytes)")
	}
	if err := validateIssuerURL(p.Issuer, svc.opts.DevMode); err != nil {
		return err
	}
	if p.ClientID == "" || len(p.ClientID) > 256 {
		return invalid("client ID is required (at most 256 bytes)")
	}
	if !slices.Contains(p.Scopes, "openid") {
		return invalid("scopes must include openid")
	}
	for _, s := range p.Scopes {
		if s == "" || strings.ContainsAny(s, " \t\r\n\"") || len(s) > 64 {
			return invalid("invalid scope %q", s)
		}
	}
	if len(p.GroupsClaim) > 64 {
		return invalid("groups claim name is too long")
	}
	switch p.Provisioning {
	case Preprovisioned:
	case JIT:
		if p.JIT.empty() {
			return invalid("JIT provisioning needs at least one restriction (email domains, tenants, hosted domains or groups)")
		}
		if len(p.JIT.Groups) > 0 && p.GroupsClaim == "" {
			return invalid("JIT group restrictions need a groups claim")
		}
	default:
		return invalid("provisioning must be preprovisioned or jit")
	}
	if p.StepUpFallback != StepUpNone && p.StepUpFallback != StepUpLocalTOTP {
		return invalid("stepup_fallback must be none or local_totp")
	}
	for _, a := range p.ACRValues {
		if a == "" || strings.ContainsAny(a, " \t\r\n") || len(a) > 128 {
			return invalid("invalid acr value")
		}
	}
	if p.CABundlePEM != "" {
		if _, err := parseCABundle(p.CABundlePEM); err != nil {
			return err
		}
	}
	return nil
}

func parseCABundle(pemData string) ([]*x509.Certificate, error) {
	var certs []*x509.Certificate
	rest := []byte(pemData)
	for {
		var b *pem.Block
		b, rest = pem.Decode(rest)
		if b == nil {
			break
		}
		if b.Type != "CERTIFICATE" {
			return nil, invalid("CA bundle may contain only CERTIFICATE blocks")
		}
		c, err := x509.ParseCertificate(b.Bytes)
		if err != nil {
			return nil, invalid("CA bundle: %v", err)
		}
		certs = append(certs, c)
	}
	if len(certs) == 0 || strings.TrimSpace(string(rest)) != "" {
		return nil, invalid("CA bundle must be PEM certificates")
	}
	return certs, nil
}

func trimAll(in []string) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func apply(p *Provider, in ProviderInput) {
	set := func(dst *string, v *string) {
		if v != nil {
			*dst = strings.TrimSpace(*v)
		}
	}
	set(&p.Slug, in.Slug)
	set(&p.DisplayName, in.DisplayName)
	set(&p.Issuer, in.Issuer)
	set(&p.ClientID, in.ClientID)
	set(&p.GroupsClaim, in.GroupsClaim)
	set(&p.Provisioning, in.Provisioning)
	set(&p.StepUpFallback, in.StepUpFallback)
	if in.CABundlePEM != nil {
		p.CABundlePEM = *in.CABundlePEM
	}
	if in.Scopes != nil {
		p.Scopes = trimAll(*in.Scopes)
	}
	if in.ACRValues != nil {
		p.ACRValues = trimAll(*in.ACRValues)
	}
	if in.JIT != nil {
		p.JIT = JITRules{EmailDomains: trimAll(in.JIT.EmailDomains), Tenants: trimAll(in.JIT.Tenants), HostedDomains: trimAll(in.JIT.HostedDomains),
			Groups: trimAll(in.JIT.Groups)}
		for i, d := range p.JIT.EmailDomains {
			p.JIT.EmailDomains[i] = strings.ToLower(d)
		}
	}
	for dst, v := range map[*bool]*bool{&p.AllowPrivateNetwork: in.AllowPrivateNetwork, &p.AllowCrossOriginEndpoints: in.AllowCrossOriginEndpoints,
		&p.EndSessionEnabled: in.EndSessionEnabled, &p.Enabled: in.Enabled} {
		if v != nil {
			*dst = *v
		}
	}
}

func b2i(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// CreateProvider adds a provider. Callers require recent authentication (Hallpass HP-AUTH-014).
func (svc *Service) CreateProvider(ctx context.Context, actor audit.Actor, in ProviderInput) (*Provider, error) {
	p := &Provider{ID: ids.New(ids.OIDCProvider), Scopes: []string{"openid", "profile", "email"}, Provisioning: Preprovisioned,
		StepUpFallback: StepUpNone, ACRValues: []string{}}
	apply(p, in)
	p.JIT = p.JIT.normalized()
	if err := svc.validate(p); err != nil {
		return nil, err
	}
	enc, err := svc.sealSecret(p.ID, in.ClientSecret)
	if err != nil {
		return nil, err
	}
	scopes, _ := json.Marshal(p.Scopes)
	jit, _ := json.Marshal(p.JIT)
	err = svc.s.W().TxNamed(ctx, "oidc.create_provider", func(tx *store.Tx) error {
		if _, err := tx.Q.GetOIDCProviderBySlug(ctx, p.Slug); err == nil {
			return apperr.New(apperr.CodeAlreadyExists, "a provider with this slug exists")
		}
		if err := tx.Q.InsertOIDCProvider(ctx, db.InsertOIDCProviderParams{ID: p.ID, Slug: p.Slug, DisplayName: p.DisplayName, Issuer: p.Issuer,
			ClientID: p.ClientID, ClientSecretEnc: enc, ScopesJson: string(scopes), GroupsClaim: p.GroupsClaim, Provisioning: p.Provisioning,
			JitJson: string(jit), AllowPrivateNetwork: b2i(p.AllowPrivateNetwork), AllowCrossOriginEndpoints: b2i(p.AllowCrossOriginEndpoints),
			CaBundlePem: p.CABundlePEM, StepupFallback: p.StepUpFallback, AcrValues: strings.Join(p.ACRValues, " "),
			EndSessionEnabled: b2i(p.EndSessionEnabled), Enabled: b2i(p.Enabled), CreatedAt: tx.NowMicros(), UpdatedAt: tx.NowMicros()}); err != nil {
			return err
		}
		_, err := svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "oidc.provider.created", Target: audit.Target{Type: "oidc_provider", ID: p.ID},
			Details: providerAudit(p, in.ClientSecret != nil && *in.ClientSecret != "")})
		return err
	})
	if err != nil {
		return nil, err
	}
	return svc.GetProvider(ctx, p.ID)
}

func providerAudit(p *Provider, secretSet bool) map[string]any {
	return map[string]any{"slug": p.Slug, "issuer": p.Issuer, "client_id": p.ClientID, "provisioning": p.Provisioning, "enabled": p.Enabled,
		"allow_private_network": p.AllowPrivateNetwork, "allow_cross_origin_endpoints": p.AllowCrossOriginEndpoints, "end_session_enabled": p.EndSessionEnabled, "client_secret_changed": secretSet}
}

func (svc *Service) sealSecret(id string, secret *string) ([]byte, error) {
	if secret == nil || *secret == "" {
		return nil, nil
	}
	if len(*secret) > 1024 {
		return nil, invalid("client secret is too long")
	}
	return svc.kr.Seal(keyring.PurposeCredential, secretAAD(id), []byte(*secret))
}

func (svc *Service) clientSecret(p *Provider) (string, error) {
	if len(p.secretEnc) == 0 {
		return "", nil
	}
	b, err := svc.kr.Open(keyring.PurposeCredential, secretAAD(p.ID), p.secretEnc)
	return string(b), err
}

// UpdateProvider changes a provider (optimistic concurrency by Version). Changing the issuer or client resets the
// test-login evidence. Callers require recent authentication (Hallpass HP-AUTH-014).
func (svc *Service) UpdateProvider(ctx context.Context, actor audit.Actor, id string, in ProviderInput) (*Provider, error) {
	p, err := svc.GetProvider(ctx, id)
	if err != nil {
		return nil, err
	}
	prevIssuer, prevClient := p.Issuer, p.ClientID
	apply(p, in)
	if err := svc.validate(p); err != nil {
		return nil, err
	}
	enc := p.secretEnc
	if in.ClientSecret != nil {
		if enc, err = svc.sealSecret(p.ID, in.ClientSecret); err != nil {
			return nil, err
		}
	}
	testAt, testBy := sql.NullInt64{}, sql.NullString{}
	if p.TestLoginOKAt != nil && p.Issuer == prevIssuer && p.ClientID == prevClient {
		testAt = sql.NullInt64{Int64: p.TestLoginOKAt.UnixMicro(), Valid: true}
		testBy = sql.NullString{String: p.TestLoginBy, Valid: p.TestLoginBy != ""}
	}
	scopes, _ := json.Marshal(p.Scopes)
	jit, _ := json.Marshal(p.JIT)
	err = svc.s.W().TxNamed(ctx, "oidc.update_provider", func(tx *store.Tx) error {
		if other, err := tx.Q.GetOIDCProviderBySlug(ctx, p.Slug); err == nil && other.ID != p.ID {
			return apperr.New(apperr.CodeAlreadyExists, "a provider with this slug exists")
		}
		n, err := tx.Q.UpdateOIDCProvider(ctx, db.UpdateOIDCProviderParams{Slug: p.Slug, DisplayName: p.DisplayName, Issuer: p.Issuer, ClientID: p.ClientID,
			ClientSecretEnc: enc, ScopesJson: string(scopes), GroupsClaim: p.GroupsClaim, Provisioning: p.Provisioning, JitJson: string(jit),
			AllowPrivateNetwork: b2i(p.AllowPrivateNetwork), AllowCrossOriginEndpoints: b2i(p.AllowCrossOriginEndpoints), CaBundlePem: p.CABundlePEM,
			StepupFallback: p.StepUpFallback, AcrValues: strings.Join(p.ACRValues, " "), EndSessionEnabled: b2i(p.EndSessionEnabled),
			Enabled: b2i(p.Enabled), TestLoginOkAt: testAt, TestLoginBy: testBy, UpdatedAt: tx.NowMicros(), ID: p.ID, Version: in.Version})
		if err != nil {
			return err
		}
		if n == 0 {
			return apperr.New(apperr.CodeConflict, "provider was modified concurrently; reload and retry")
		}
		// Disabling the provider or clearing its test-login evidence must not leave SSO-only enforcement without a
		// working SSO administrator path (Hallpass HP-AUTH-015).
		if err := checkSSOOnlyStillSafe(ctx, tx); err != nil {
			return err
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "oidc.provider.updated", Target: audit.Target{Type: "oidc_provider", ID: p.ID},
			Details: providerAudit(p, in.ClientSecret != nil)})
		return err
	})
	if err != nil {
		return nil, err
	}
	svc.forget(p.ID)
	return svc.GetProvider(ctx, p.ID)
}

// DeleteProvider removes a provider with its mappings and linked identities. Users who were linked to it lose their
// SSO-sourced grants, which only SSO sign-ins maintain (Hallpass HP-OIDC-011, SR-26); the mappings of another provider
// restore them at the next SSO sign-in through it. SSO grants never count toward the last-administrator guard, so
// this can never remove the last administrator.
func (svc *Service) DeleteProvider(ctx context.Context, actor audit.Actor, id string) error {
	var affected []string
	err := svc.s.W().TxNamed(ctx, "oidc.delete_provider", func(tx *store.Tx) error {
		var err error
		if affected, err = tx.Q.ListUserIDsForProvider(ctx, id); err != nil {
			return err
		}
		n, err := tx.Q.DeleteOIDCProvider(ctx, id)
		if err != nil {
			return err
		}
		if n == 0 {
			return apperr.New(apperr.CodeNotFound, "provider not found")
		}
		for _, uid := range affected {
			if err := svc.auth.DropSSOGrantsTx(ctx, tx, uid); err != nil {
				return err
			}
		}
		if err := checkSSOOnlyStillSafe(ctx, tx); err != nil {
			return err
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "oidc.provider.deleted", Target: audit.Target{Type: "oidc_provider", ID: id},
			Details: map[string]any{"unlinked_users": len(affected)}})
		return err
	})
	if err == nil {
		svc.forget(id)
	}
	return err
}

// AddMapping adds a group → grant mapping (Hallpass HP-OIDC-011). The domain is "*" or a space that exists.
func (svc *Service) AddMapping(ctx context.Context, actor audit.Actor, providerID string, in MappingInput) (*Mapping, error) {
	groupValue := strings.TrimSpace(in.GroupValue)
	if groupValue == "" || len(groupValue) > 256 {
		return nil, invalid("group value is required (at most 256 bytes)")
	}
	if !in.Role.Valid() {
		return nil, invalid("unknown role %q", in.Role)
	}
	if !authz.ValidDomain(in.Domain) {
		return nil, invalid("domain must be a space ID or \"*\"")
	}
	m := &Mapping{ID: ids.New(ids.OIDCMapping), ProviderID: providerID, GroupValue: groupValue, Role: in.Role, Domain: in.Domain}
	space := sql.NullString{String: in.Domain, Valid: in.Domain != authz.Global}
	err := svc.s.W().TxNamed(ctx, "oidc.add_mapping", func(tx *store.Tx) error {
		if _, err := tx.Q.GetOIDCProvider(ctx, providerID); err != nil {
			return apperr.New(apperr.CodeNotFound, "provider not found")
		}
		if space.Valid {
			if _, err := tx.Q.GetSpace(ctx, space.String); errors.Is(err, sql.ErrNoRows) {
				return invalid("space %s does not exist", space.String)
			} else if err != nil {
				return err
			}
		}
		if err := tx.Q.InsertOIDCMapping(ctx, db.InsertOIDCMappingParams{ID: m.ID, ProviderID: providerID, GroupValue: groupValue,
			Role: string(in.Role), Domain: in.Domain, SpaceID: space, CreatedAt: tx.NowMicros()}); store.IsConstraint(err) {
			return apperr.Wrap(apperr.CodeAlreadyExists, err, "this mapping exists already")
		} else if err != nil {
			return err
		}
		m.CreatedAt = tx.Now.UTC()
		_, err := svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "oidc.mapping.added", Target: audit.Target{Type: "oidc_provider", ID: providerID},
			Details: map[string]any{"group_value": groupValue, "role": in.Role, "domain": in.Domain}})
		return err
	})
	if err != nil {
		return nil, err
	}
	return m, nil
}

// DeleteMapping removes a mapping; affected users lose the grant at their next SSO sign-in (Hallpass HP-OIDC-011).
func (svc *Service) DeleteMapping(ctx context.Context, actor audit.Actor, providerID, id string) error {
	return svc.s.W().TxNamed(ctx, "oidc.delete_mapping", func(tx *store.Tx) error {
		n, err := tx.Q.DeleteOIDCMapping(ctx, db.DeleteOIDCMappingParams{ID: id, ProviderID: providerID})
		if err != nil {
			return err
		}
		if n == 0 {
			return apperr.New(apperr.CodeNotFound, "mapping not found")
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "oidc.mapping.deleted", Target: audit.Target{Type: "oidc_provider", ID: providerID},
			Details: map[string]any{"mapping_id": id}})
		return err
	})
}

// ListMappings returns a provider's mappings.
func (svc *Service) ListMappings(ctx context.Context, providerID string) ([]Mapping, error) {
	rows, err := svc.s.R().ListOIDCMappings(ctx, providerID)
	if err != nil {
		return nil, err
	}
	out := make([]Mapping, len(rows))
	for i, r := range rows {
		out[i] = Mapping{ID: r.ID, ProviderID: r.ProviderID, GroupValue: r.GroupValue, Role: authz.Role(r.Role), Domain: r.Domain,
			CreatedAt: time.UnixMicro(r.CreatedAt).UTC()}
	}
	return out, nil
}
