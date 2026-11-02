package oidc

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"path"
	"slices"
	"strings"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/keyring"
	"github.com/aurimasniekis/go-spaceman/internal/obs/metrics"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Flow purposes.
const (
	PurposeLogin  = "login"
	PurposeStepUp = "stepup"
	PurposeTest   = "test"
	PurposeLink   = "link"
)

// Errors returned by the flow. Messages are safe to show; they never reveal whether an account exists beyond
// "not provisioned".
var (
	ErrInvalidState    = apperr.New(apperr.CodeInvalid, "sign-in session expired or invalid; start again")
	ErrNotProvisioned  = apperr.New(apperr.CodeForbidden, "this identity is not provisioned in SpaceMan; ask an administrator")
	ErrLoginRejected   = apperr.New(apperr.CodeForbidden, "sign-in was rejected")
	ErrStepUpFailed    = apperr.New(apperr.CodeForbidden, "the identity provider did not confirm a fresh login")
	ErrTokenValidation = apperr.New(apperr.CodeUnauthorized, "the identity provider's response could not be validated")
)

// StartInput begins an authorization.
type StartInput struct {
	Provider string // slug
	Purpose  string
	ReturnTo string
	// Principal is the current session: required for stepup, test (administrators) and self-linking.
	Principal *auth.Principal
	// LinkToken is an admin-issued link invitation (purpose link without a session).
	LinkToken string
}

// StartResult tells the HTTP layer where to redirect and which binding cookie to set.
type StartResult struct {
	RedirectURL string
	Binding     string
	ExpiresAt   time.Time
}

func random32() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func hashOf(domain, v string) []byte {
	h := sha256.Sum256([]byte("spaceman/oidc/" + domain + "/v1|" + v))
	return h[:]
}

// SanitizeReturnTo accepts only same-origin relative paths outside /api and /auth (Hallpass HP-OIDC-005, SR-24).
func SanitizeReturnTo(p string) string {
	if p == "" || len(p) > 512 || !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") || strings.ContainsAny(p, "\\\r\n\t") {
		return "/"
	}
	u, err := url.Parse(p)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil {
		return "/"
	}
	for _, c := range p {
		if c < 0x20 || c == 0x7f {
			return "/"
		}
	}
	// Dot segments (also percent-encoded: u.Path is decoded) would let the browser resolve "/./api/…" or
	// "/x/../auth/…" past the deny list below; no UI route needs them.
	for _, seg := range strings.Split(u.Path, "/") {
		if seg == "." || seg == ".." {
			return "/"
		}
	}
	if strings.HasPrefix(u.Path, "/api/") || strings.HasPrefix(u.Path, "/auth/") || u.Path == "/api" || u.Path == "/auth" {
		return "/"
	}
	// Redirect writers (net/http's Redirect among them) clean everything before "?" as a path, fragment included:
	// "/#/../api/…" would become "/api/…" (SR-24). Accept only input that such cleaning leaves unchanged.
	pre, _, _ := strings.Cut(p, "?")
	if cleaned := path.Clean(pre); cleaned != pre && cleaned+"/" != pre {
		return "/"
	}
	return p
}

func verifierAAD(stateHash []byte) []byte {
	return append([]byte("spaceman/oidc-pkce/v1|"), stateHash...)
}

// sessionPrincipal reports whether p is a browser session of a user (not an API token or the root token).
func sessionPrincipal(p *auth.Principal) bool {
	return p != nil && p.User != nil && p.TokenID == "" && p.RootTokenID == ""
}

// Start creates a single-use server-side state (state, nonce, PKCE verifier; 10 minutes) and returns the provider's
// authorization URL (Hallpass HP-OIDC-002). PKCE is always S256 (Porch accepts nothing else).
func (svc *Service) Start(ctx context.Context, in StartInput) (*StartResult, error) {
	p, err := svc.providerBySlug(ctx, in.Provider)
	if err != nil {
		return nil, err
	}
	if !p.Enabled && in.Purpose != PurposeTest {
		return nil, apperr.New(apperr.CodeNotFound, "provider not found")
	}
	var userID, sessionDisplay string
	switch in.Purpose {
	case PurposeLogin:
	case PurposeStepUp:
		if !sessionPrincipal(in.Principal) || in.Principal.Method != auth.MethodOIDC {
			return nil, apperr.New(apperr.CodePrecondition, "step-up through SSO needs an SSO session")
		}
		userID, sessionDisplay = in.Principal.User.ID, in.Principal.SessionID
	case PurposeTest:
		if !svc.canTest(in.Principal) {
			return nil, apperr.New(apperr.CodeForbidden, "only global administrators can run an SSO test login")
		}
		userID, sessionDisplay = in.Principal.User.ID, in.Principal.SessionID
	case PurposeLink:
		switch {
		case in.LinkToken != "":
			if userID, err = svc.checkLinkToken(ctx, in.LinkToken, p.ID); err != nil {
				return nil, err
			}
		case sessionPrincipal(in.Principal) && in.Principal.MFASatisfied():
			// Account linking needs recent authentication, with MFA where policy requires it or a factor is enrolled
			// (Hallpass HP-AUTH-014, SR-10).
			if err := svc.auth.RequireRecentAuth(in.Principal, in.Principal.User.MFARequired); err != nil {
				return nil, err
			}
			userID, sessionDisplay = in.Principal.User.ID, in.Principal.SessionID
		default:
			return nil, apperr.New(apperr.CodeUnauthorized, "linking needs a signed-in session or a link invitation")
		}
	default:
		return nil, invalid("unknown purpose")
	}
	r, err := svc.discover(ctx, p)
	if err != nil {
		return nil, err
	}
	state, err := random32()
	if err != nil {
		return nil, err
	}
	nonce, err := random32()
	if err != nil {
		return nil, err
	}
	binding, err := random32()
	if err != nil {
		return nil, err
	}
	verifier := oauth2.GenerateVerifier()
	sh := hashOf("state", state)
	venc, err := svc.kr.Seal(keyring.PurposeToken, verifierAAD(sh), []byte(verifier))
	if err != nil {
		return nil, err
	}
	now := svc.opts.Clock.Now()
	exp := now.Add(svc.opts.StateTTL)
	returnTo := SanitizeReturnTo(in.ReturnTo)
	err = svc.s.W().TxNamed(ctx, "oidc.start", func(tx *store.Tx) error {
		return tx.Q.InsertAuthState(ctx, db.InsertAuthStateParams{StateHash: sh, ProviderID: p.ID, Purpose: in.Purpose, NonceHash: hashOf("nonce", nonce),
			VerifierEnc: venc, BindingHash: hashOf("binding", binding), ReturnTo: returnTo,
			UserID: sql.NullString{String: userID, Valid: userID != ""}, SessionDisplay: sql.NullString{String: sessionDisplay, Valid: sessionDisplay != ""},
			CreatedAt: now.UnixMicro(), ExpiresAt: exp.UnixMicro()})
	})
	if err != nil {
		return nil, err
	}
	opts := []oauth2.AuthCodeOption{oauth2.S256ChallengeOption(verifier), gooidc.Nonce(nonce)}
	if in.Purpose == PurposeStepUp || in.Purpose == PurposeTest || in.Purpose == PurposeLink {
		// Upstream step-up (Hallpass HP-OIDC-013); test and link also require a fresh login at the IdP.
		opts = append(opts, oauth2.SetAuthURLParam("prompt", "login"), oauth2.SetAuthURLParam("max_age", "0"))
	}
	if len(p.ACRValues) > 0 {
		opts = append(opts, oauth2.SetAuthURLParam("acr_values", strings.Join(p.ACRValues, " ")))
	}
	conf := svc.oauthConfig(p, r, "")
	return &StartResult{RedirectURL: conf.AuthCodeURL(state, opts...), Binding: binding, ExpiresAt: exp}, nil
}

// canTest reports whether a session may run (and complete) an SSO test login: a full, MFA-complete browser session
// of a user whom Casbin allows user administration in the global domain, never a limited one (Hallpass SR-48).
func (svc *Service) canTest(p *auth.Principal) bool {
	if !sessionPrincipal(p) || p.Limited || svc.auth == nil || svc.auth.Authz() == nil {
		return false
	}
	return svc.auth.Authz().AllowedAll(p.Subjects(), authz.Global, authz.UserWrite)
}

func (svc *Service) oauthConfig(p *Provider, r *remote, secret string) *oauth2.Config {
	return &oauth2.Config{ClientID: p.ClientID, ClientSecret: secret, Endpoint: r.provider.Endpoint(), RedirectURL: svc.RedirectURI(), Scopes: p.Scopes}
}

// CallbackInput is the provider's redirect back to SpaceMan.
type CallbackInput struct {
	State, Code string
	Error       string
	// Issuer is the authorization response's iss parameter (RFC 9207, sent by Porch); when present it must equal the
	// provider's issuer.
	Issuer    string
	Binding   string // value of the binding cookie
	Principal *auth.Principal
	Meta      auth.SessionMeta
}

// CallbackResult is the outcome per purpose.
type CallbackResult struct {
	Purpose    string
	ProviderID string
	ReturnTo   string
	// SessionToken is set for login and stepup (the rotated session).
	SessionToken string
	UserID       string
	Test         *TestResult
}

// Claims are the ID-token claims SpaceMan uses (everything else is ignored).
type Claims struct {
	Issuer        string   `json:"iss"`
	Subject       string   `json:"sub"`
	Email         string   `json:"email,omitempty"`
	EmailVerified bool     `json:"email_verified"`
	Name          string   `json:"name,omitempty"`
	Groups        []string `json:"groups,omitempty"`
	Tenant        string   `json:"tid,omitempty"`
	HostedDomain  string   `json:"hd,omitempty"`
	AuthTime      *int64   `json:"auth_time,omitempty"`
	ACR           string   `json:"acr,omitempty"`
	AMR           []string `json:"amr,omitempty"`
}

// TestResult is the claim-mapping preview of a test login (Hallpass HP-OIDC-014). No session is created.
type TestResult struct {
	Claims          Claims        `json:"claims"`
	LinkedUserID    string        `json:"linked_user_id,omitempty"`
	WouldProvision  bool          `json:"would_provision"`
	ProvisionReason string        `json:"provision_reason,omitempty"`
	MappedGrants    []authz.Grant `json:"mapped_grants" doc:"Grants the mappings give this identity (applied to SSO sessions only)"`
	ManualGrants    []authz.Grant `json:"manual_grants" doc:"Grants of the linked user that every session has"`
}

// Callback validates the response and completes the purpose. The state is consumed first (single use) whatever
// happens next.
func (svc *Service) Callback(ctx context.Context, in CallbackInput) (_ *CallbackResult, cbErr error) {
	if in.State == "" || len(in.State) > 128 {
		return nil, ErrInvalidState
	}
	var st db.OidcAuthState
	err := svc.s.W().TxNamed(ctx, "oidc.take_state", func(tx *store.Tx) error {
		var err error
		st, err = tx.Q.TakeAuthState(ctx, hashOf("state", in.State))
		return err
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrInvalidState
	}
	if err != nil {
		return nil, err
	}
	now := svc.opts.Clock.Now()
	if now.After(time.UnixMicro(st.ExpiresAt)) || subtle.ConstantTimeCompare(st.BindingHash, hashOf("binding", in.Binding)) != 1 {
		return nil, ErrInvalidState
	}
	p, err := svc.GetProvider(ctx, st.ProviderID)
	if err != nil {
		return nil, ErrInvalidState
	}
	if st.Purpose == PurposeLogin {
		defer func() { svc.observeLogin(p.Slug, cbErr) }()
	}
	if in.Error != "" {
		svc.log.Info("identity provider returned an error", "provider", p.Slug, "error", truncate(in.Error, 64))
		return nil, ErrLoginRejected
	}
	if in.Issuer != "" && in.Issuer != p.Issuer {
		// RFC 9207: a response that names another issuer is a mix-up attempt.
		svc.log.Warn("authorization response from another issuer", "provider", p.Slug)
		return nil, ErrTokenValidation
	}
	if !p.Enabled && st.Purpose != PurposeTest {
		return nil, ErrLoginRejected
	}
	r, err := svc.discover(ctx, p)
	if err != nil {
		return nil, err
	}
	verifier, err := svc.kr.Open(keyring.PurposeToken, verifierAAD(st.StateHash), st.VerifierEnc)
	if err != nil {
		return nil, ErrInvalidState
	}
	secret, err := svc.clientSecret(p)
	if err != nil {
		return nil, err
	}
	tok, err := svc.oauthConfig(p, r, secret).Exchange(gooidc.ClientContext(ctx, r.client), in.Code, oauth2.VerifierOption(string(verifier)))
	if err != nil {
		var re *oauth2.RetrieveError
		if errors.As(err, &re) && re.Response != nil {
			// Only the status: the response body could echo request data.
			svc.log.Warn("oidc code exchange failed", "provider", p.Slug, "status", re.Response.StatusCode)
			if re.Response.StatusCode < 500 {
				return nil, ErrLoginRejected
			}
			return nil, ErrProviderUnavailable
		}
		svc.log.Warn("oidc token endpoint unavailable", "provider", p.Slug, "error", err.Error())
		return nil, ErrProviderUnavailable
	}
	raw, _ := tok.Extra("id_token").(string)
	// Access and refresh tokens are dropped here: SpaceMan has no use for them (Hallpass HP-OIDC-008).
	tok = nil
	claims, err := svc.verifyIDToken(ctx, p, r, raw, st.NonceHash)
	if err != nil {
		svc.log.Warn("id token rejected", "provider", p.Slug, "error", err.Error())
		return nil, ErrTokenValidation
	}
	res := &CallbackResult{Purpose: st.Purpose, ReturnTo: st.ReturnTo, ProviderID: p.ID}
	switch st.Purpose {
	case PurposeLogin:
		res.SessionToken, res.UserID, err = svc.login(ctx, p, claims, in.Meta)
	case PurposeStepUp:
		res.SessionToken, err = svc.stepUp(ctx, p, st, claims, in)
		res.UserID = st.UserID.String
	case PurposeTest:
		res.Test, err = svc.testLogin(ctx, p, st, claims, in.Principal)
	case PurposeLink:
		res.UserID, err = svc.link(ctx, p, st, claims, in)
	default:
		err = ErrInvalidState
	}
	if err != nil {
		return nil, err
	}
	return res, nil
}

// observeLogin counts an SSO login completion for a known provider.
func (svc *Service) observeLogin(slug string, err error) {
	var o metrics.LoginOutcome
	switch {
	case err == nil:
		o = metrics.LoginSuccess
	case errors.Is(err, ErrLoginRejected), errors.Is(err, ErrNotProvisioned):
		o = metrics.LoginDenied
	case errors.Is(err, ErrTokenValidation), errors.Is(err, ErrInvalidState):
		o = metrics.LoginFailure
	default:
		o = metrics.LoginError
	}
	svc.opts.Metrics.OIDCLogin(slug, o)
	svc.opts.Metrics.UserLogin(metrics.LoginOIDC, o)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// verifyIDToken checks signature (RS256/ES256/EdDSA via JWKS), iss, aud, azp, exp/iat/nbf with ≤ 60 s skew and the
// nonce (Hallpass HP-OIDC-003).
func (svc *Service) verifyIDToken(ctx context.Context, p *Provider, r *remote, raw string, nonceHash []byte) (*Claims, error) {
	if raw == "" {
		return nil, errors.New("token response has no id_token")
	}
	now := svc.opts.Clock.Now()
	v := gooidc.NewVerifier(p.Issuer, r.keys, &gooidc.Config{ClientID: p.ClientID, SupportedSigningAlgs: signingAlgs,
		Now: func() time.Time { return now.Add(-svc.opts.Skew) }})
	t, err := v.Verify(ctx, raw)
	if err != nil {
		return nil, err
	}
	var extra struct {
		AZP string `json:"azp"`
		IAT int64  `json:"iat"`
		NBF *int64 `json:"nbf"`
	}
	if err := t.Claims(&extra); err != nil {
		return nil, err
	}
	if (len(t.Audience) > 1 || extra.AZP != "") && extra.AZP != p.ClientID {
		return nil, errors.New("azp does not match the client ID")
	}
	if extra.IAT == 0 || time.Unix(extra.IAT, 0).After(now.Add(svc.opts.Skew)) {
		return nil, errors.New("iat missing or in the future")
	}
	if extra.NBF != nil && time.Unix(*extra.NBF, 0).After(now.Add(svc.opts.Skew)) {
		return nil, errors.New("token not yet valid")
	}
	if subtle.ConstantTimeCompare(hashOf("nonce", t.Nonce), nonceHash) != 1 {
		return nil, errors.New("nonce mismatch")
	}
	var c Claims
	if err := t.Claims(&c); err != nil {
		return nil, err
	}
	if p.GroupsClaim != "" {
		c.Groups = nil
		var all map[string]json.RawMessage
		if err := t.Claims(&all); err == nil {
			c.Groups = parseGroups(claimAt(all, p.GroupsClaim))
		}
	} else {
		c.Groups = nil
	}
	if c.Subject == "" || len(c.Subject) > 255 {
		return nil, errors.New("sub missing or too long")
	}
	c.Email = strings.ToLower(strings.TrimSpace(c.Email))
	return &c, nil
}

// claimAt returns the claim named path. A top-level claim with exactly that name wins, so names containing dots
// (`https://example.com/groups`) keep working; otherwise a dotted path walks nested objects (`porch.groups`).
func claimAt(all map[string]json.RawMessage, path string) json.RawMessage {
	if raw, ok := all[path]; ok {
		return raw
	}
	parts := strings.Split(path, ".")
	if len(parts) < 2 {
		return nil
	}
	cur := all
	for _, part := range parts[:len(parts)-1] {
		var next map[string]json.RawMessage
		if json.Unmarshal(cur[part], &next) != nil {
			return nil
		}
		cur = next
	}
	return cur[parts[len(parts)-1]]
}

// parseGroups accepts a string array or a single string; anything else yields no groups.
func parseGroups(raw json.RawMessage) []string {
	var arr []string
	if json.Unmarshal(raw, &arr) == nil {
		if len(arr) > 1000 {
			arr = arr[:1000]
		}
		return arr
	}
	var one string
	if json.Unmarshal(raw, &one) == nil && one != "" {
		return []string{one}
	}
	return nil
}

// mapped evaluates the allowlisted mappings for the claims' groups (Hallpass HP-OIDC-011): one grant per distinct
// (role, domain), sorted, with the first matching group value as its source reference.
func (svc *Service) mapped(ctx context.Context, p *Provider, groups []string) ([]auth.SSOGrant, error) {
	ms, err := svc.ListMappings(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	var out []auth.SSOGrant
	for _, m := range ms {
		if !slices.Contains(groups, m.GroupValue) {
			continue
		}
		if slices.ContainsFunc(out, func(g auth.SSOGrant) bool { return g.Role == m.Role && g.Domain == m.Domain }) {
			continue
		}
		out = append(out, auth.SSOGrant{Role: m.Role, Domain: m.Domain, GroupValue: m.GroupValue})
	}
	slices.SortFunc(out, func(a, b auth.SSOGrant) int {
		if c := strings.Compare(a.Domain, b.Domain); c != 0 {
			return c
		}
		return strings.Compare(string(a.Role), string(b.Role))
	})
	return out, nil
}

func grantList(gs []auth.SSOGrant) []authz.Grant {
	out := make([]authz.Grant, 0, len(gs))
	for _, g := range gs {
		out = append(out, authz.Grant{Role: g.Role, Domain: g.Domain, Source: "sso", SourceRef: g.GroupValue})
	}
	return out
}

func grantSummary(gs []auth.SSOGrant) []string {
	out := make([]string, 0, len(gs))
	for _, g := range gs {
		out = append(out, string(g.Role)+"@"+g.Domain)
	}
	return out
}

// jitAllowed checks every configured JIT restriction (Hallpass HP-OIDC-010).
func jitAllowed(rules JITRules, c *Claims) (bool, string) {
	if rules.empty() {
		return false, "no JIT restrictions configured"
	}
	if len(rules.EmailDomains) > 0 {
		at := strings.LastIndexByte(c.Email, '@')
		if !c.EmailVerified || at < 0 || !slices.Contains(rules.EmailDomains, c.Email[at+1:]) {
			return false, "verified email domain not allowed"
		}
	}
	if len(rules.Tenants) > 0 && !slices.Contains(rules.Tenants, c.Tenant) {
		return false, "tenant not allowed"
	}
	if len(rules.HostedDomains) > 0 && !slices.Contains(rules.HostedDomains, c.HostedDomain) {
		return false, "hosted domain not allowed"
	}
	if len(rules.Groups) > 0 && !slices.ContainsFunc(c.Groups, func(g string) bool { return slices.Contains(rules.Groups, g) }) {
		return false, "no allowed group"
	}
	return true, ""
}

// login resolves (iss, sub) to a user — never by email (Hallpass HP-OIDC-009) — provisions JIT users, syncs the SSO
// grants and creates the session, all in one transaction.
func (svc *Service) login(ctx context.Context, p *Provider, c *Claims, meta auth.SessionMeta) (string, string, error) {
	grants, err := svc.mapped(ctx, p, c.Groups)
	if err != nil {
		return "", "", err
	}
	var token, userID string
	err = svc.s.W().TxNamed(ctx, "oidc.login", func(tx *store.Tx) error {
		ident, err := tx.Q.GetUserIdentity(ctx, db.GetUserIdentityParams{Issuer: c.Issuer, Subject: c.Subject})
		switch {
		case err == nil:
			userID = ident.UserID
		case errors.Is(err, sql.ErrNoRows):
			if p.Provisioning != JIT {
				return &deniedError{reason: "not_provisioned", pub: ErrNotProvisioned}
			}
			if ok, why := jitAllowed(p.JIT, c); !ok {
				return &deniedError{reason: "jit_restricted: " + why, pub: ErrNotProvisioned}
			}
			if userID, err = svc.provisionTx(ctx, tx, p, c); err != nil {
				return err
			}
		default:
			return err
		}
		u, err := tx.Q.GetUser(ctx, userID)
		if err != nil || u.Enabled != 1 {
			return &deniedError{reason: "user_disabled", pub: ErrLoginRejected}
		}
		if _, err := svc.auth.SyncSSOGrantsTx(ctx, tx, userID, grants, audit.Actor{Type: audit.ActorSystem, ID: "oidc:" + p.Slug}); err != nil {
			return err
		}
		if err := tx.Q.TouchUserIdentity(ctx, db.TouchUserIdentityParams{LastLoginAt: sql.NullInt64{Int64: tx.NowMicros(), Valid: true}, Issuer: c.Issuer,
			Subject: c.Subject}); err != nil {
			return err
		}
		meta.Provider = p.Slug
		if token, err = svc.auth.CreateSessionTx(ctx, tx, userID, auth.MethodOIDC, meta); err != nil {
			return err
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: audit.Actor{Type: audit.ActorUser, ID: userID}, Action: "user.login",
			SourceIP: meta.IP, Details: map[string]any{"method": auth.MethodOIDC, "provider": p.Slug, "sso_grants": grantSummary(grants)}})
		return err
	})
	var de *deniedError
	if errors.As(err, &de) {
		svc.recordDenied(ctx, p, c, meta, de.reason)
		return "", "", de.pub
	}
	if err != nil {
		return "", "", err
	}
	return token, userID, nil
}

// deniedError aborts the login transaction; login() records the audit row in its own transaction afterwards so the
// record survives the rollback.
type deniedError struct {
	reason string
	pub    error
}

func (e *deniedError) Error() string { return e.reason }

func (svc *Service) recordDenied(ctx context.Context, p *Provider, c *Claims, meta auth.SessionMeta, reason string) {
	err := svc.s.W().TxNamed(ctx, "oidc.login_denied", func(tx *store.Tx) error {
		_, err := svc.audit.Append(ctx, tx, audit.Event{Actor: audit.Actor{Type: audit.ActorSystem, ID: "oidc:" + p.Slug}, Action: "user.login.failed",
			Outcome: audit.OutcomeDenied, SourceIP: meta.IP, Details: map[string]any{"method": auth.MethodOIDC, "provider": p.Slug, "reason": reason,
				"subject_hash": base64.RawURLEncoding.EncodeToString(hashOf("subject", c.Issuer+"|"+c.Subject)[:12])}})
		return err
	})
	if err != nil {
		svc.log.Error("audit of denied sso login failed", "error", err.Error())
	}
}

// provisionTx creates a JIT user with no manual grants (Hallpass HP-OIDC-010). An email already used by another user
// is not merged and not reused (Hallpass HP-OIDC-009).
func (svc *Service) provisionTx(ctx context.Context, tx *store.Tx, p *Provider, c *Claims) (string, error) {
	id := ids.New(ids.User)
	var email sql.NullString
	if c.EmailVerified && c.Email != "" {
		if e, err := auth.NormalizeEmail(c.Email); err == nil {
			if _, err := tx.Q.GetUserByEmail(ctx, sql.NullString{String: e, Valid: true}); err == nil {
				return "", &deniedError{reason: "jit_email_conflict", pub: apperr.New(apperr.CodeConflict,
					"a user with this email already exists; ask an administrator to link your SSO identity")}
			}
			email = sql.NullString{String: e, Valid: true}
		}
	}
	name := strings.TrimSpace(c.Name)
	if name == "" {
		name = email.String
	}
	if name == "" {
		name = "SSO user"
	}
	if len(name) > 128 {
		name = name[:128]
	}
	if err := tx.Q.InsertUser(ctx, db.InsertUserParams{ID: id, Email: email, DisplayName: name, Enabled: 1, ProvisionedVia: "jit",
		CreatedAt: tx.NowMicros(), CreatedBy: "oidc:" + p.Slug, UpdatedAt: tx.NowMicros()}); err != nil {
		return "", err
	}
	if err := tx.Q.InsertUserIdentity(ctx, db.InsertUserIdentityParams{Issuer: c.Issuer, Subject: c.Subject, ProviderID: p.ID, UserID: id, LinkedVia: "jit",
		CreatedAt: tx.NowMicros()}); err != nil {
		return "", err
	}
	svc.auth.PublishUserChangedTx(tx, id)
	_, err := svc.audit.Append(ctx, tx, audit.Event{Actor: audit.Actor{Type: audit.ActorSystem, ID: "oidc:" + p.Slug}, Action: "user.created",
		Target: audit.Target{Type: "user", ID: id}, Details: map[string]any{"via": "jit", "provider": p.Slug, "email": email.String}})
	return id, err
}

// stepUp verifies a fresh upstream login: auth_time must be at or after the step-up start (minus skew), and acr must
// match when configured (Hallpass HP-OIDC-013).
func (svc *Service) stepUp(ctx context.Context, p *Provider, st db.OidcAuthState, c *Claims, in CallbackInput) (string, error) {
	pr := in.Principal
	if !sessionPrincipal(pr) || pr.Method != auth.MethodOIDC || pr.User.ID != st.UserID.String || pr.SessionID != st.SessionDisplay.String {
		return "", ErrInvalidState
	}
	ident, err := svc.s.R().GetUserIdentity(ctx, db.GetUserIdentityParams{Issuer: c.Issuer, Subject: c.Subject})
	if err != nil || ident.UserID != pr.User.ID {
		return "", ErrStepUpFailed
	}
	if c.AuthTime == nil {
		if p.StepUpFallback == StepUpLocalTOTP {
			return "", apperr.New(apperr.CodeMFARequired, "this identity provider cannot prove a fresh login; confirm with your SpaceMan TOTP code")
		}
		return "", ErrStepUpFailed
	}
	started := time.UnixMicro(st.CreatedAt)
	if time.Unix(*c.AuthTime, 0).Before(started.Add(-svc.opts.Skew)) {
		return "", ErrStepUpFailed
	}
	if len(p.ACRValues) > 0 && !slices.Contains(p.ACRValues, c.ACR) {
		return "", ErrStepUpFailed
	}
	return svc.auth.ReauthExternal(ctx, pr, p.Slug, in.Meta)
}

// testLogin records a successful end-to-end test login and returns the mapping preview without creating a session
// (Hallpass HP-OIDC-014).
func (svc *Service) testLogin(ctx context.Context, p *Provider, st db.OidcAuthState, c *Claims, pr *auth.Principal) (*TestResult, error) {
	if !svc.canTest(pr) || pr.User.ID != st.UserID.String {
		return nil, ErrInvalidState
	}
	grants, err := svc.mapped(ctx, p, c.Groups)
	if err != nil {
		return nil, err
	}
	res := &TestResult{Claims: *c, MappedGrants: grantList(grants), ManualGrants: []authz.Grant{}}
	if res.Claims.Groups == nil {
		res.Claims.Groups = []string{}
	}
	if ident, err := svc.s.R().GetUserIdentity(ctx, db.GetUserIdentityParams{Issuer: c.Issuer, Subject: c.Subject}); err == nil {
		res.LinkedUserID = ident.UserID
		if u, err := svc.auth.Get(ctx, ident.UserID); err == nil {
			res.ManualGrants = append(res.ManualGrants, u.Grants...)
		}
	} else if p.Provisioning == JIT {
		res.WouldProvision, res.ProvisionReason = jitAllowed(p.JIT, c)
	} else {
		res.ProvisionReason = "not provisioned (preprovisioned mode)"
	}
	err = svc.s.W().TxNamed(ctx, "oidc.test_login", func(tx *store.Tx) error {
		if err := tx.Q.SetOIDCTestLogin(ctx, db.SetOIDCTestLoginParams{TestLoginOkAt: sql.NullInt64{Int64: tx.NowMicros(), Valid: true},
			TestLoginBy: sql.NullString{String: pr.User.ID, Valid: true}, ID: p.ID}); err != nil {
			return err
		}
		_, err := svc.audit.Append(ctx, tx, audit.Event{Actor: audit.Actor{Type: audit.ActorUser, ID: pr.User.ID}, Action: "oidc.provider.test_login",
			Target: audit.Target{Type: "oidc_provider", ID: p.ID}, Details: map[string]any{"mapped_grants": grantSummary(grants), "linked": res.LinkedUserID != ""}})
		return err
	})
	if err != nil {
		return nil, err
	}
	svc.mu.Lock()
	svc.tests[pr.User.ID+"|"+p.ID] = testEntry{res: res, at: svc.opts.Clock.Now()}
	svc.mu.Unlock()
	return res, nil
}

// TestResultFor returns the latest test-login preview of userID for providerID (kept in memory for 10 minutes; the
// claims are never stored).
func (svc *Service) TestResultFor(userID, providerID string) (*TestResult, error) {
	svc.mu.Lock()
	defer svc.mu.Unlock()
	now := svc.opts.Clock.Now()
	for k, e := range svc.tests {
		if now.Sub(e.at) > 10*time.Minute {
			delete(svc.tests, k)
		}
	}
	e, ok := svc.tests[userID+"|"+providerID]
	if !ok {
		return nil, apperr.New(apperr.CodeNotFound, "no recent test login for this provider; run one first")
	}
	return e.res, nil
}

// link binds (iss, sub) to the user who started the flow: a signed-in session on both sides, or an admin-issued link
// invitation (Hallpass HP-OIDC-009). An identity linked to someone else is never moved.
func (svc *Service) link(ctx context.Context, p *Provider, st db.OidcAuthState, c *Claims, in CallbackInput) (string, error) {
	uid := st.UserID.String
	if st.SessionDisplay.Valid && (!sessionPrincipal(in.Principal) || in.Principal.SessionID != st.SessionDisplay.String || in.Principal.User.ID != uid) {
		return "", ErrInvalidState
	}
	via := "self_link"
	if !st.SessionDisplay.Valid {
		via = "admin"
	}
	err := svc.s.W().TxNamed(ctx, "oidc.link", func(tx *store.Tx) error {
		if ident, err := tx.Q.GetUserIdentity(ctx, db.GetUserIdentityParams{Issuer: c.Issuer, Subject: c.Subject}); err == nil {
			if ident.UserID == uid {
				return nil
			}
			return apperr.New(apperr.CodeConflict, "this SSO identity is already linked to another user")
		}
		if err := tx.Q.InsertUserIdentity(ctx, db.InsertUserIdentityParams{Issuer: c.Issuer, Subject: c.Subject, ProviderID: p.ID, UserID: uid, LinkedVia: via,
			CreatedAt: tx.NowMicros()}); err != nil {
			return err
		}
		svc.auth.PublishUserChangedTx(tx, uid)
		_, err := svc.audit.Append(ctx, tx, audit.Event{Actor: audit.Actor{Type: audit.ActorUser, ID: uid}, Action: "user.identity.linked",
			Target: audit.Target{Type: "user", ID: uid}, SourceIP: in.Meta.IP, Details: map[string]any{"provider": p.Slug, "via": via}})
		return err
	})
	return uid, err
}

// ReauthTOTP is the local-TOTP step-up fallback (Hallpass HP-OIDC-013): only for SSO sessions whose provider is enabled
// and configured with stepup_fallback = local_totp (Hallpass SR-03). Other sessions re-authenticate upstream or with a
// password.
func (svc *Service) ReauthTOTP(ctx context.Context, pr *auth.Principal, code string, meta auth.SessionMeta) (string, error) {
	if !sessionPrincipal(pr) || pr.Method != auth.MethodOIDC || pr.Provider == "" {
		return "", auth.ErrTOTPStepUpNotAllowed
	}
	p, err := svc.providerBySlug(ctx, pr.Provider)
	if err != nil && !errors.Is(err, apperr.ErrNotFound) {
		return "", err
	}
	if err != nil || !p.Enabled || p.StepUpFallback != StepUpLocalTOTP {
		return "", auth.ErrTOTPStepUpNotAllowed
	}
	return svc.auth.ReauthTOTP(ctx, pr, code, meta)
}
