package api

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
)

// RecoveryOut is a redeemed break-glass recovery login.
type RecoveryOut struct {
	Me Me `json:"me"`
	// PasswordReset is a single-use password reset token, issued when the account has a local password; using it ends
	// every session of the user, including this one.
	PasswordReset *ResetToken `json:"password_reset,omitempty"`
	MFAReset      bool        `json:"mfa_reset" doc:"The account's TOTP factor and recovery codes were removed"`
}

// ResetToken is a single-use password reset token.
type ResetToken struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// BreakGlassStatus reports an unacknowledged break-glass use (`spaceman admin recover`).
type BreakGlassStatus struct {
	Alert bool       `json:"alert"`
	At    *time.Time `json:"at,omitempty" doc:"Time of the most recent unacknowledged use"`
}

// registerRecovery adds the account-recovery operations: password reset with an administrator-issued token, the
// break-glass recovery login issued by the local CLI, recovery-code regeneration and the break-glass alert
// (Hallpass HP-AUTH-010/016).
func registerRecovery(api huma.API, d *Deps) {
	tags := []string{"auth"}
	huma.Register(api, huma.Operation{OperationID: "reset-password", Method: http.MethodPost, Path: "/auth/reset", Summary: "Set a new password with a reset token",
		Description: "Reset tokens are issued by an administrator (or by a recovery login). Every session of the user ends.",
		Tags:        tags, Security: []map[string][]string{}, DefaultStatus: http.StatusNoContent},
		func(ctx context.Context, in *struct {
			Body struct {
				Token    string `json:"token" maxLength:"128"`
				Password string `json:"password" maxLength:"4096"`
			}
		}) (*struct{}, error) {
			if err := d.Auth.ResetPassword(ctx, in.Body.Token, in.Body.Password); err != nil {
				return nil, d.fail(ctx, "auth.reset_password", err)
			}
			return &struct{}{}, nil
		})

	huma.Register(api, huma.Operation{OperationID: "recovery-login", Method: http.MethodPost, Path: "/auth/recover", Summary: "Sign in with a break-glass recovery token",
		Description: "Recovery tokens come only from `spaceman admin recover` on the server. The user's other sessions, TOTP factor " +
			"and recovery codes are removed; the session is limited until a new factor is enrolled when policy requires one.",
		Tags: tags, Security: []map[string][]string{}},
		func(ctx context.Context, in *struct {
			UserAgent string `header:"User-Agent"`
			Body      struct {
				Token string `json:"token" maxLength:"128"`
			}
		}) (*struct {
			SetCookie http.Cookie `header:"Set-Cookie"`
			Body      RecoveryOut
		}, error) {
			res, err := d.Auth.RecoveryLogin(ctx, in.Body.Token, meta(ctx, in.UserAgent))
			if err != nil {
				return nil, d.fail(ctx, "auth.recovery_login", err)
			}
			out := &struct {
				SetCookie http.Cookie `header:"Set-Cookie"`
				Body      RecoveryOut
			}{SetCookie: d.cookie(res.Token), Body: RecoveryOut{Me: d.me(res.Principal), MFAReset: res.MFAReset}}
			if res.ResetToken != "" {
				out.Body.PasswordReset = &ResetToken{Token: res.ResetToken, ExpiresAt: res.ResetExpiresAt}
			}
			return out, nil
		})

	huma.Register(api, huma.Operation{OperationID: "regenerate-recovery-codes", Method: http.MethodPost, Path: "/auth/mfa/recovery-codes",
		Summary: "Replace your recovery codes", Description: "The previous codes stop working. Needs a recent re-authentication with TOTP.", Tags: tags},
		func(ctx context.Context, _ *struct{}) (*struct {
			Body struct {
				RecoveryCodes []string `json:"recovery_codes"`
			}
		}, error) {
			codes, err := d.Auth.RegenerateRecoveryCodes(ctx, PrincipalFrom(ctx))
			if err != nil {
				return nil, d.fail(ctx, "auth.recovery_codes", err)
			}
			out := &struct {
				Body struct {
					RecoveryCodes []string `json:"recovery_codes"`
				}
			}{}
			out.Body.RecoveryCodes = codes
			return out, nil
		})

	huma.Register(api, huma.Operation{OperationID: "get-break-glass", Method: http.MethodGet, Path: "/admin/break-glass", Summary: "Unacknowledged break-glass use",
		Tags: []string{"admin"}},
		func(ctx context.Context, _ *struct{}) (*struct{ Body BreakGlassStatus }, error) {
			alert, at, err := d.Auth.BreakGlassAlert(ctx)
			if err != nil {
				return nil, d.fail(ctx, "auth.breakglass", err)
			}
			out := &struct{ Body BreakGlassStatus }{Body: BreakGlassStatus{Alert: alert}}
			if alert {
				out.Body.At = &at
			}
			return out, nil
		})

	huma.Register(api, huma.Operation{OperationID: "acknowledge-break-glass", Method: http.MethodPost, Path: "/admin/break-glass/acknowledge",
		Summary: "Acknowledge break-glass use", Description: "Clears the alert for every break-glass use recorded so far (audited).",
		Tags: []string{"admin"}, DefaultStatus: http.StatusNoContent},
		func(ctx context.Context, _ *struct{}) (*struct{}, error) {
			if err := d.Auth.AcknowledgeBreakGlass(ctx, Actor(ctx)); err != nil {
				return nil, d.fail(ctx, "auth.breakglass_ack", err)
			}
			return &struct{}{}, nil
		})
}
