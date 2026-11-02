package api

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/aurimasniekis/go-spaceman/internal/auth"
)

// Me describes the signed-in principal: identity, grants, MFA state and the CSRF token for state-changing requests.
type Me struct {
	User        *auth.User `json:"user,omitempty"`
	Root        bool       `json:"root"`
	Method      string     `json:"method"`
	SessionID   string     `json:"session_id"`
	CSRFToken   string     `json:"csrf_token,omitempty"`
	Limited     bool       `json:"limited" doc:"MFA verification or enrollment is still required"`
	NeedsEnroll bool       `json:"needs_enroll"`
	ExpiresAt   time.Time  `json:"expires_at"`
	// Domains are the spaces (or "*") in which the principal holds any grant.
	Domains []string `json:"domains"`
}

type sessionOut struct {
	SetCookie http.Cookie `header:"Set-Cookie"`
	Body      Me
}

func (d *Deps) me(p *auth.Principal) Me {
	m := Me{User: p.User, Root: p.IsRoot(), Method: p.Method, SessionID: p.SessionID, CSRFToken: p.CSRFToken, Limited: p.Limited,
		NeedsEnroll: p.NeedsEnroll, ExpiresAt: p.ExpiresAt, Domains: []string{}}
	if d.Authz != nil {
		for _, s := range p.Subjects() {
			for _, dom := range d.Authz.Domains(s) {
				if !contains(m.Domains, dom) {
					m.Domains = append(m.Domains, dom)
				}
			}
		}
	}
	return m
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// cookie returns the session Set-Cookie ("" clears it).
func (d *Deps) cookie(token string) http.Cookie {
	c := http.Cookie{Name: SessionCookieName(d.SecureCookies), Value: token, Path: "/", HttpOnly: true, Secure: d.SecureCookies, //nolint:gosec // Secure is off only for plain-http development origins (config refuses http in production)
		SameSite: http.SameSiteLaxMode}
	if token == "" {
		c.MaxAge = -1
	}
	return c
}

func (d *Deps) sessionResponse(ctx context.Context, op, token string) (*sessionOut, error) {
	p, err := d.Auth.Authenticate(ctx, token)
	if err != nil {
		return nil, d.fail(ctx, op, err)
	}
	return &sessionOut{SetCookie: d.cookie(token), Body: d.me(p)}, nil
}

func meta(ctx context.Context, ua string) auth.SessionMeta {
	return auth.SessionMeta{IP: ClientIP(ctx), UserAgent: ua}
}

type loginIn struct {
	UserAgent string `header:"User-Agent"`
	Body      struct {
		Email    string `json:"email" maxLength:"254"`
		Password string `json:"password" maxLength:"4096"`
	}
}

type codeIn struct {
	UserAgent string `header:"User-Agent"`
	Body      struct {
		Code string `json:"code" maxLength:"32" doc:"TOTP code or recovery code"`
	}
}

// TOTPEnrollment is a started TOTP enrollment. The secret is shown once to the enrolling user.
type TOTPEnrollment struct {
	Secret string `json:"secret"`
	URI    string `json:"otpauth_uri"`
}

// EnrollConfirmed returns the recovery codes, shown once.
type EnrollConfirmed struct {
	RecoveryCodes []string `json:"recovery_codes"`
	Me            Me       `json:"me"`
}

func registerAuth(api huma.API, d *Deps) {
	tags := []string{"auth"}
	huma.Register(api, huma.Operation{OperationID: "login", Method: http.MethodPost, Path: "/auth/login", Summary: "Sign in with email and password",
		Tags: tags, Security: []map[string][]string{}},
		func(ctx context.Context, in *loginIn) (*sessionOut, error) {
			res, err := d.Auth.Login(ctx, auth.LoginInput{Email: in.Body.Email, Password: in.Body.Password, Meta: meta(ctx, in.UserAgent)})
			if err != nil {
				return nil, d.fail(ctx, "auth.login", err)
			}
			return d.sessionResponse(ctx, "auth.login", res.Token)
		})

	huma.Register(api, huma.Operation{OperationID: "verify-mfa", Method: http.MethodPost, Path: "/auth/mfa/verify", Summary: "Complete sign-in with a TOTP or recovery code",
		Tags: tags},
		func(ctx context.Context, in *codeIn) (*sessionOut, error) {
			tok, err := d.Auth.VerifyMFA(ctx, PrincipalFrom(ctx), in.Body.Code, meta(ctx, in.UserAgent))
			if err != nil {
				return nil, d.fail(ctx, "auth.verify_mfa", err)
			}
			return d.sessionResponse(ctx, "auth.verify_mfa", tok)
		})

	huma.Register(api, huma.Operation{OperationID: "start-enroll", Method: http.MethodPost, Path: "/auth/mfa/enroll", Summary: "Start TOTP enrollment",
		Tags: tags},
		func(ctx context.Context, _ *struct{}) (*struct{ Body TOTPEnrollment }, error) {
			e, err := d.Auth.BeginTOTP(ctx, PrincipalFrom(ctx))
			if err != nil {
				return nil, d.fail(ctx, "auth.begin_totp", err)
			}
			return &struct{ Body TOTPEnrollment }{Body: TOTPEnrollment{Secret: e.Secret, URI: e.URI}}, nil
		})

	huma.Register(api, huma.Operation{OperationID: "confirm-enroll", Method: http.MethodPost, Path: "/auth/mfa/enroll/confirm", Summary: "Confirm TOTP enrollment",
		Tags: tags},
		func(ctx context.Context, in *codeIn) (*struct {
			SetCookie http.Cookie `header:"Set-Cookie"`
			Body      EnrollConfirmed
		}, error) {
			codes, tok, err := d.Auth.ConfirmTOTP(ctx, PrincipalFrom(ctx), in.Body.Code, meta(ctx, in.UserAgent))
			if err != nil {
				return nil, d.fail(ctx, "auth.confirm_totp", err)
			}
			if tok == "" {
				// A limited (sign-in) session stays limited: enrollment never counts as authentication, so sign-in
				// completes with verify-mfa and a later code (Hallpass SR-03). The cookie is unchanged.
				tok = sessionTokenFrom(ctx)
			}
			s, err := d.sessionResponse(ctx, "auth.confirm_totp", tok)
			if err != nil {
				return nil, err
			}
			return &struct {
				SetCookie http.Cookie `header:"Set-Cookie"`
				Body      EnrollConfirmed
			}{SetCookie: s.SetCookie, Body: EnrollConfirmed{RecoveryCodes: codes, Me: s.Body}}, nil
		})

	huma.Register(api, huma.Operation{OperationID: "get-me", Method: http.MethodGet, Path: "/auth/me", Summary: "The signed-in principal",
		Tags: tags},
		func(ctx context.Context, _ *struct{}) (*struct{ Body Me }, error) {
			return &struct{ Body Me }{Body: d.me(PrincipalFrom(ctx))}, nil
		})

	huma.Register(api, huma.Operation{OperationID: "logout", Method: http.MethodPost, Path: "/auth/logout", Summary: "Sign out (revokes this session)",
		Tags: tags, DefaultStatus: http.StatusNoContent},
		func(ctx context.Context, _ *struct{}) (*struct {
			SetCookie http.Cookie `header:"Set-Cookie"`
		}, error) {
			if t := sessionTokenFrom(ctx); t != "" {
				if err := d.Auth.Logout(ctx, t); err != nil {
					return nil, d.fail(ctx, "auth.logout", err)
				}
			}
			return &struct {
				SetCookie http.Cookie `header:"Set-Cookie"`
			}{SetCookie: d.cookie("")}, nil
		})

	huma.Register(api, huma.Operation{OperationID: "reauth-password", Method: http.MethodPost, Path: "/auth/reauth", Summary: "Re-authenticate (step-up)",
		Tags: tags},
		func(ctx context.Context, in *struct {
			UserAgent string `header:"User-Agent"`
			Body      struct {
				Password string `json:"password" maxLength:"4096"`
				Code     string `json:"code,omitempty" maxLength:"32"`
			}
		}) (*sessionOut, error) {
			tok, err := d.Auth.Reauth(ctx, PrincipalFrom(ctx), in.Body.Password, in.Body.Code, meta(ctx, in.UserAgent))
			if err != nil {
				return nil, d.fail(ctx, "auth.reauth", err)
			}
			return d.sessionResponse(ctx, "auth.reauth", tok)
		})

	huma.Register(api, huma.Operation{OperationID: "list-sessions", Method: http.MethodGet, Path: "/auth/sessions", Summary: "Your active sessions",
		Tags: tags},
		func(ctx context.Context, _ *struct{}) (*struct {
			Body struct {
				Items []auth.SessionInfo `json:"items"`
			}
		}, error) {
			p := PrincipalFrom(ctx)
			s, err := d.Auth.ListSessions(ctx, p.User.ID, p.SessionID)
			if err != nil {
				return nil, d.fail(ctx, "auth.list_sessions", err)
			}
			out := &struct {
				Body struct {
					Items []auth.SessionInfo `json:"items"`
				}
			}{}
			out.Body.Items = append([]auth.SessionInfo{}, s...)
			return out, nil
		})

	huma.Register(api, huma.Operation{OperationID: "revoke-session", Method: http.MethodDelete, Path: "/auth/sessions/{session_id}", Summary: "Revoke one of your sessions",
		Tags: tags, DefaultStatus: http.StatusNoContent},
		func(ctx context.Context, in *struct {
			SessionID string `path:"session_id" maxLength:"64"`
		}) (*struct{}, error) {
			p := PrincipalFrom(ctx)
			if err := d.Auth.RevokeSession(ctx, p.User.ID, in.SessionID, "user"); err != nil {
				return nil, d.fail(ctx, "auth.revoke_session", err)
			}
			return &struct{}{}, nil
		})

	huma.Register(api, huma.Operation{OperationID: "change-password", Method: http.MethodPost, Path: "/auth/password", Summary: "Change your password",
		Tags: tags, DefaultStatus: http.StatusNoContent},
		func(ctx context.Context, in *struct {
			Body struct {
				Current string `json:"current" maxLength:"4096"`
				New     string `json:"new" maxLength:"4096"`
			}
		}) (*struct{}, error) {
			if err := d.Auth.ChangePassword(ctx, PrincipalFrom(ctx), in.Body.Current, in.Body.New); err != nil {
				return nil, d.fail(ctx, "auth.change_password", err)
			}
			return &struct{}{}, nil
		})
}
