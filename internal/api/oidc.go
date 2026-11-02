package api

import (
	"context"
	"net/http"
	"net/url"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/oidc"
)

// OIDCRules are the authorization rules of OIDC sign-in (SM-AUTH-001/002, ADR-0029). The browser redirects (start and
// callback) are public: the session they complete, the link invitation or the step-up is checked by the OIDC service,
// and the test login requires user:write through Casbin there. Provider changes need a recent re-authentication
// (Hallpass HP-AUTH-014).
var OIDCRules = map[string]Rule{
	"list-sign-in-providers": public(),
	"start-oidc":             public(),
	"oidc-callback":          public(),
	"reauth-totp":            {Session: true, UserOnly: true},
	"list-my-identities":     {Session: true, UserOnly: true},
	"unlink-my-identity":     {Session: true, UserOnly: true, Reauth: true},

	"list-oidc-providers":  Global(authz.UserRead),
	"get-oidc-provider":    Global(authz.UserRead),
	"create-oidc-provider": StepUp(authz.UserWrite),
	"update-oidc-provider": StepUp(authz.UserWrite),
	"delete-oidc-provider": StepUp(authz.UserWrite),
	"list-oidc-mappings":   Global(authz.UserRead),
	"add-oidc-mapping":     StepUp(authz.UserWrite),
	"delete-oidc-mapping":  StepUp(authz.UserWrite),
	"get-oidc-test-login":  Global(authz.UserWrite),
	"list-user-identities": Global(authz.UserRead),
	"link-user-identity":   StepUp(authz.UserWrite),
	"unlink-user-identity": StepUp(authz.UserWrite),
	"issue-link-invite":    StepUp(authz.UserWrite),
}

// OIDCBindingCookieName is the cookie that binds a pending authorization to the browser that started it:
// __Host-sm_oidc with Secure, sm_oidc on plain-http development.
func OIDCBindingCookieName(secure bool) string {
	if secure {
		return "__Host-sm_oidc"
	}
	return "sm_oidc"
}

// ProviderPath identifies an OIDC provider.
type ProviderPath struct {
	ProviderID string `path:"provider_id" maxLength:"64"`
}

// ProviderList is every OIDC provider with the redirect URI to register at each of them.
type ProviderList struct {
	Items       []*oidc.Provider `json:"items"`
	RedirectURI string           `json:"redirect_uri" doc:"Register exactly this redirect URI with every provider"`
}

// SignInProviders are the enabled providers the sign-in page offers.
type SignInProviders struct {
	Items []oidc.PublicProvider `json:"items"`
}

// IdentityList is a user's linked external identities.
type IdentityList struct {
	Items []oidc.Identity `json:"items"`
}

// MappingList is a provider's group mappings.
type MappingList struct {
	Items []oidc.Mapping `json:"items"`
}

// LinkInvite is a single-use invitation that links the next SSO sign-in through a provider to a user.
type LinkInvite struct {
	Token     string    `json:"token" doc:"Shown once"`
	URL       string    `json:"url" doc:"Open this path in the browser to sign in at the provider and link the identity"`
	ExpiresAt time.Time `json:"expires_at"`
}

type providerOut struct {
	ETag string `header:"ETag"`
	Body *oidc.Provider
}

type redirectOut struct {
	Status    int
	Location  string        `header:"Location"`
	SetCookie []http.Cookie `header:"Set-Cookie"`
}

// IdentityQuery names a linked identity.
type IdentityQuery struct {
	Issuer  string `query:"issuer" maxLength:"512" required:"true"`
	Subject string `query:"subject" maxLength:"255" required:"true"`
}

func (d *Deps) bindingCookie(value string, maxAge int) http.Cookie {
	c := http.Cookie{Name: OIDCBindingCookieName(d.SecureCookies), Value: value, Path: "/", HttpOnly: true, Secure: d.SecureCookies, //nolint:gosec // Secure is off only for plain-http development origins
		SameSite: http.SameSiteLaxMode, MaxAge: maxAge}
	if value == "" {
		c.MaxAge = -1
	}
	return c
}

// ssoErrorRedirect sends the browser back to the UI with a stable error code (never internals): the sign-in page, or the
// account page for a signed-in user (step-up, linking, test login).
func ssoErrorRedirect(ctx context.Context, err error, cookies ...http.Cookie) *redirectOut {
	code := string(apperr.CodeOf(err))
	if code == "" {
		code = "internal"
	}
	page := "/login"
	if PrincipalFrom(ctx) != nil {
		page = "/account"
	}
	return &redirectOut{Status: http.StatusSeeOther, Location: page + "?sso_error=" + url.QueryEscape(code), SetCookie: cookies}
}

// RegisterOIDC registers OIDC sign-in, provider administration and identity linking.
func RegisterOIDC(svc *oidc.Service) func(api huma.API, d *Deps) {
	return func(api huma.API, d *Deps) {
		registerOIDCSignIn(api, d, svc)
		registerOIDCAdmin(api, d, svc)
	}
}

func registerOIDCSignIn(api huma.API, d *Deps, svc *oidc.Service) {
	tags := []string{"auth"}
	noAuth := []map[string][]string{}
	huma.Register(api, huma.Operation{OperationID: "list-sign-in-providers", Method: http.MethodGet, Path: "/auth/oidc/providers",
		Summary: "Enabled sign-in providers", Description: "Slug and display name only; the sign-in page offers one button per provider.",
		Tags: tags, Security: noAuth},
		func(ctx context.Context, _ *struct{}) (*struct{ Body SignInProviders }, error) {
			l, err := svc.EnabledProviders(ctx)
			if err != nil {
				return nil, d.fail(ctx, "oidc.enabled_providers", err)
			}
			return &struct{ Body SignInProviders }{Body: SignInProviders{Items: l}}, nil
		})

	huma.Register(api, huma.Operation{OperationID: "start-oidc", Method: http.MethodGet, Path: "/auth/oidc/{provider}/start",
		Summary: "Start an OIDC sign-in, step-up, link or test login (browser redirect)",
		Description: "Redirects to the provider with a single-use state, a nonce and a PKCE S256 challenge, and sets a binding cookie. " +
			"Errors redirect to the sign-in page (or the account page) with `sso_error=<code>`.",
		Tags: tags, Security: noAuth, DefaultStatus: http.StatusFound},
		func(ctx context.Context, in *struct {
			Provider  string `path:"provider" maxLength:"32"`
			Purpose   string `query:"purpose" enum:"login,stepup,link,test" default:"login"`
			ReturnTo  string `query:"return_to" maxLength:"512"`
			LinkToken string `query:"link_token" maxLength:"128"`
		}) (*redirectOut, error) {
			if in.Purpose == "" {
				in.Purpose = oidc.PurposeLogin
			}
			res, err := svc.Start(ctx, oidc.StartInput{Provider: in.Provider, Purpose: in.Purpose, ReturnTo: in.ReturnTo, Principal: PrincipalFrom(ctx),
				LinkToken: in.LinkToken})
			if err != nil {
				_ = d.fail(ctx, "oidc.start", err) // logs unexpected errors; the browser only sees the code
				return ssoErrorRedirect(ctx, err), nil
			}
			return &redirectOut{Status: http.StatusFound, Location: res.RedirectURL,
				SetCookie: []http.Cookie{d.bindingCookie(res.Binding, int(res.ExpiresAt.Sub(d.now()).Seconds()))}}, nil
		})

	huma.Register(api, huma.Operation{OperationID: "oidc-callback", Method: http.MethodGet, Path: "/auth/oidc/callback",
		Summary: "OIDC redirect URI (browser redirect)",
		Description: "Completes the flow the state names: sets the session cookie after a sign-in or step-up and redirects into the UI. " +
			"The ID token is validated (signature, issuer, audience, nonce, times); access and refresh tokens are discarded.",
		Tags: tags, Security: noAuth, DefaultStatus: http.StatusSeeOther},
		func(ctx context.Context, in *struct {
			State     string      `query:"state" maxLength:"256"`
			Code      string      `query:"code" maxLength:"2048"`
			Error     string      `query:"error" maxLength:"256"`
			Issuer    string      `query:"iss" maxLength:"512"`
			UserAgent string      `header:"User-Agent"`
			Binding   http.Cookie `cookie:"__Host-sm_oidc"`
			DevBind   http.Cookie `cookie:"sm_oidc"`
		}) (*redirectOut, error) {
			binding := in.Binding.Value
			if !d.SecureCookies {
				binding = in.DevBind.Value
			}
			unset := d.bindingCookie("", -1)
			res, err := svc.Callback(ctx, oidc.CallbackInput{State: in.State, Code: in.Code, Error: in.Error, Issuer: in.Issuer, Binding: binding,
				Principal: PrincipalFrom(ctx), Meta: meta(ctx, in.UserAgent)})
			if err != nil {
				_ = d.fail(ctx, "oidc.callback", err)
				return ssoErrorRedirect(ctx, err, unset), nil
			}
			switch res.Purpose {
			case oidc.PurposeLogin, oidc.PurposeStepUp:
				return &redirectOut{Status: http.StatusSeeOther, Location: oidc.SanitizeReturnTo(res.ReturnTo), SetCookie: []http.Cookie{unset, d.cookie(res.SessionToken)}}, nil
			case oidc.PurposeTest:
				// The preview is fetched by the providers page (get-oidc-test-login); nothing sensitive goes into the URL.
				return &redirectOut{Status: http.StatusSeeOther, Location: "/sign-in-providers?test_login=" + url.QueryEscape(res.ProviderID),
					SetCookie: []http.Cookie{unset}}, nil
			}
			return &redirectOut{Status: http.StatusSeeOther, Location: oidc.SanitizeReturnTo(res.ReturnTo), SetCookie: []http.Cookie{unset}}, nil
		})

	huma.Register(api, huma.Operation{OperationID: "reauth-totp", Method: http.MethodPost, Path: "/auth/reauth/totp",
		Summary:     "Re-authenticate an SSO session with your SpaceMan TOTP code",
		Description: "Only for SSO sessions whose provider cannot prove a fresh login and allows the local TOTP fallback.", Tags: tags},
		func(ctx context.Context, in *codeIn) (*sessionOut, error) {
			tok, err := svc.ReauthTOTP(ctx, PrincipalFrom(ctx), in.Body.Code, meta(ctx, in.UserAgent))
			if err != nil {
				return nil, d.fail(ctx, "oidc.reauth_totp", err)
			}
			return d.sessionResponse(ctx, "oidc.reauth_totp", tok)
		})

	huma.Register(api, huma.Operation{OperationID: "list-my-identities", Method: http.MethodGet, Path: "/auth/identities",
		Summary: "Your linked sign-in identities", Tags: tags},
		func(ctx context.Context, _ *struct{}) (*struct{ Body IdentityList }, error) {
			l, err := svc.Identities(ctx, PrincipalFrom(ctx).User.ID)
			if err != nil {
				return nil, d.fail(ctx, "oidc.my_identities", err)
			}
			return &struct{ Body IdentityList }{Body: IdentityList{Items: append([]oidc.Identity{}, l...)}}, nil
		})

	huma.Register(api, huma.Operation{OperationID: "unlink-my-identity", Method: http.MethodDelete, Path: "/auth/identities",
		Summary:     "Unlink one of your sign-in identities",
		Description: "Refused when it is your only way to sign in. Your SSO-mapped grants are dropped and every session of yours ends.",
		Tags:        tags, DefaultStatus: http.StatusNoContent},
		func(ctx context.Context, in *IdentityQuery) (*struct{}, error) {
			p := PrincipalFrom(ctx)
			if err := svc.UnlinkIdentity(ctx, Actor(ctx), p.User.ID, in.Issuer, in.Subject, true); err != nil {
				return nil, d.fail(ctx, "oidc.unlink_self", err)
			}
			return &struct{}{}, nil
		})
}

func registerOIDCAdmin(api huma.API, d *Deps, svc *oidc.Service) {
	tags := []string{"sign-in providers"}
	huma.Register(api, huma.Operation{OperationID: "list-oidc-providers", Method: http.MethodGet, Path: "/oidc/providers",
		Summary: "OIDC sign-in providers (never their client secrets)", Tags: tags},
		func(ctx context.Context, _ *struct{}) (*struct{ Body ProviderList }, error) {
			l, err := svc.ListProviders(ctx)
			if err != nil {
				return nil, d.fail(ctx, "oidc.list", err)
			}
			return &struct{ Body ProviderList }{Body: ProviderList{Items: l, RedirectURI: svc.RedirectURI()}}, nil
		})
	huma.Register(api, huma.Operation{OperationID: "get-oidc-provider", Method: http.MethodGet, Path: "/oidc/providers/{provider_id}",
		Summary: "An OIDC provider (never its client secret)", Tags: tags},
		func(ctx context.Context, in *ProviderPath) (*providerOut, error) {
			p, err := svc.GetProvider(ctx, in.ProviderID)
			if err != nil {
				return nil, d.fail(ctx, "oidc.get", err)
			}
			return &providerOut{ETag: ETag(p.Version), Body: p}, nil
		})
	huma.Register(api, huma.Operation{OperationID: "create-oidc-provider", Method: http.MethodPost, Path: "/oidc/providers",
		Summary: "Add an OIDC provider", Description: "The client secret is sealed and can never be read back. Discovery runs at the first sign-in.",
		Tags: tags, DefaultStatus: http.StatusCreated},
		func(ctx context.Context, in *struct {
			IdempotencyKey
			Body oidc.ProviderInput
		}) (*providerOut, error) {
			// The idempotency fingerprint never includes the client secret.
			fp := map[string]any{"slug": in.Body.Slug, "issuer": in.Body.Issuer, "client_id": in.Body.ClientID}
			p, _, err := Idempotent(ctx, d, "create-oidc-provider", in.IdempotencyKey, fp, false, func() (*oidc.Provider, error) {
				return svc.CreateProvider(ctx, Actor(ctx), in.Body)
			})
			if err != nil {
				return nil, d.fail(ctx, "oidc.create", err)
			}
			return &providerOut{ETag: ETag(p.Version), Body: p}, nil
		})
	huma.Register(api, huma.Operation{OperationID: "update-oidc-provider", Method: http.MethodPatch, Path: "/oidc/providers/{provider_id}",
		Summary: "Change an OIDC provider", Description: "Omitted fields stay unchanged. Changing the issuer or client ID clears the test-login evidence.",
		Tags: tags},
		func(ctx context.Context, in *struct {
			ProviderPath
			IfMatch
			Body oidc.ProviderInput
		}) (*providerOut, error) {
			v, err := in.Version()
			if err != nil {
				return nil, d.fail(ctx, "oidc.update", err)
			}
			in.Body.Version = v
			p, err := svc.UpdateProvider(ctx, Actor(ctx), in.ProviderID, in.Body)
			if err != nil {
				return nil, d.fail(ctx, "oidc.update", err)
			}
			return &providerOut{ETag: ETag(p.Version), Body: p}, nil
		})
	huma.Register(api, huma.Operation{OperationID: "delete-oidc-provider", Method: http.MethodDelete, Path: "/oidc/providers/{provider_id}",
		Summary: "Delete an OIDC provider", Description: "Its mappings and linked identities go too; linked users lose their SSO-mapped grants.",
		Tags: tags, DefaultStatus: http.StatusNoContent},
		func(ctx context.Context, in *ProviderPath) (*struct{}, error) {
			if err := svc.DeleteProvider(ctx, Actor(ctx), in.ProviderID); err != nil {
				return nil, d.fail(ctx, "oidc.delete", err)
			}
			return &struct{}{}, nil
		})
	huma.Register(api, huma.Operation{OperationID: "list-oidc-mappings", Method: http.MethodGet, Path: "/oidc/providers/{provider_id}/mappings",
		Summary: "A provider's group mappings", Tags: tags},
		func(ctx context.Context, in *ProviderPath) (*struct{ Body MappingList }, error) {
			l, err := svc.ListMappings(ctx, in.ProviderID)
			if err != nil {
				return nil, d.fail(ctx, "oidc.mappings", err)
			}
			return &struct{ Body MappingList }{Body: MappingList{Items: l}}, nil
		})
	huma.Register(api, huma.Operation{OperationID: "add-oidc-mapping", Method: http.MethodPost, Path: "/oidc/providers/{provider_id}/mappings",
		Summary: "Map a groups-claim value to a role in a domain",
		Description: "Applied at every SSO sign-in: the user's SSO grants are replaced by the grants of the groups they hold now. " +
			"SSO grants apply only to SSO sessions.",
		Tags: tags, DefaultStatus: http.StatusCreated},
		func(ctx context.Context, in *struct {
			ProviderPath
			Body oidc.MappingInput
		}) (*struct{ Body *oidc.Mapping }, error) {
			m, err := svc.AddMapping(ctx, Actor(ctx), in.ProviderID, in.Body)
			if err != nil {
				return nil, d.fail(ctx, "oidc.add_mapping", err)
			}
			return &struct{ Body *oidc.Mapping }{Body: m}, nil
		})
	huma.Register(api, huma.Operation{OperationID: "delete-oidc-mapping", Method: http.MethodDelete, Path: "/oidc/providers/{provider_id}/mappings/{mapping_id}",
		Summary: "Remove a group mapping", Description: "Users lose the grant at their next SSO sign-in.", Tags: tags, DefaultStatus: http.StatusNoContent},
		func(ctx context.Context, in *struct {
			ProviderPath
			MappingID string `path:"mapping_id" maxLength:"64"`
		}) (*struct{}, error) {
			if err := svc.DeleteMapping(ctx, Actor(ctx), in.ProviderID, in.MappingID); err != nil {
				return nil, d.fail(ctx, "oidc.delete_mapping", err)
			}
			return &struct{}{}, nil
		})
	huma.Register(api, huma.Operation{OperationID: "get-oidc-test-login", Method: http.MethodGet, Path: "/oidc/providers/{provider_id}/test-login",
		Summary:     "Your latest test login's claim-mapping preview",
		Description: "Start a test login with `/auth/oidc/{slug}/start?purpose=test`; the preview is kept in memory for 10 minutes and no session is created.",
		Tags:        tags},
		func(ctx context.Context, in *ProviderPath) (*struct{ Body *oidc.TestResult }, error) {
			r, err := svc.TestResultFor(PrincipalFrom(ctx).ActorID(), in.ProviderID)
			if err != nil {
				return nil, d.fail(ctx, "oidc.test_result", err)
			}
			return &struct{ Body *oidc.TestResult }{Body: r}, nil
		})

	utags := []string{"users"}
	huma.Register(api, huma.Operation{OperationID: "list-user-identities", Method: http.MethodGet, Path: "/users/{user_id}/identities",
		Summary: "A user's linked sign-in identities", Tags: utags},
		func(ctx context.Context, in *UserPath) (*struct{ Body IdentityList }, error) {
			l, err := svc.Identities(ctx, in.ID)
			if err != nil {
				return nil, d.fail(ctx, "oidc.identities", err)
			}
			return &struct{ Body IdentityList }{Body: IdentityList{Items: append([]oidc.Identity{}, l...)}}, nil
		})
	huma.Register(api, huma.Operation{OperationID: "link-user-identity", Method: http.MethodPost, Path: "/users/{user_id}/identities",
		Summary: "Link an expected identity to a user", Description: "The subject is the provider's stable `sub` (with Porch, the `u_…` user ID; never an email).",
		Tags: utags, DefaultStatus: http.StatusNoContent},
		func(ctx context.Context, in *struct {
			UserPath
			Body struct {
				ProviderID string `json:"provider_id" maxLength:"64"`
				Subject    string `json:"subject" minLength:"1" maxLength:"255"`
			}
		}) (*struct{}, error) {
			if err := svc.LinkIdentity(ctx, Actor(ctx), in.ID, in.Body.ProviderID, in.Body.Subject); err != nil {
				return nil, d.fail(ctx, "oidc.link_admin", err)
			}
			return &struct{}{}, nil
		})
	huma.Register(api, huma.Operation{OperationID: "unlink-user-identity", Method: http.MethodDelete, Path: "/users/{user_id}/identities",
		Summary: "Unlink an identity from a user", Description: "The user's SSO-mapped grants are dropped and every session of theirs ends.",
		Tags: utags, DefaultStatus: http.StatusNoContent},
		func(ctx context.Context, in *struct {
			UserPath
			IdentityQuery
		}) (*struct{}, error) {
			if err := svc.UnlinkIdentity(ctx, Actor(ctx), in.ID, in.Issuer, in.Subject, false); err != nil {
				return nil, d.fail(ctx, "oidc.unlink_admin", err)
			}
			return &struct{}{}, nil
		})
	huma.Register(api, huma.Operation{OperationID: "issue-link-invite", Method: http.MethodPost, Path: "/users/{user_id}/link-invite",
		Summary:     "Issue a single-use SSO link invitation",
		Description: "Hand the URL to the user out of band: it links the identity they sign in with at the provider to this user (72 hours).",
		Tags:        utags},
		func(ctx context.Context, in *struct {
			UserPath
			Body struct {
				ProviderID string `json:"provider_id" maxLength:"64"`
			}
		}) (*struct{ Body LinkInvite }, error) {
			p, err := svc.GetProvider(ctx, in.Body.ProviderID)
			if err != nil {
				return nil, d.fail(ctx, "oidc.link_invite", err)
			}
			tok, exp, err := svc.IssueLinkInvite(ctx, Actor(ctx), in.ID, p.ID)
			if err != nil {
				return nil, d.fail(ctx, "oidc.link_invite", err)
			}
			return &struct{ Body LinkInvite }{Body: LinkInvite{Token: tok, ExpiresAt: exp.UTC(),
				URL: Prefix + "/auth/oidc/" + url.PathEscape(p.Slug) + "/start?purpose=link&link_token=" + url.QueryEscape(tok)}}, nil
		})
}
