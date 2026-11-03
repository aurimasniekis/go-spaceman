package oidc

import (
	"context"
	"database/sql"
	"errors"
	"net/url"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Installation-wide sign-in settings ported from go-hallpass internal/oidc/admin.go at bdd8329 (ADR-0045): SSO-only
// enforcement (password sign-in off), the direct provider of the sign-in page, and RP-initiated logout.

// ErrSSOOnlyPrecondition explains why SSO-only enforcement cannot be enabled or kept (Hallpass HP-AUTH-015, HP-OIDC-014).
var ErrSSOOnlyPrecondition = apperr.New(apperr.CodePrecondition,
	"password sign-in can be turned off only after an administrator completed an SSO test login on an enabled provider, and while at least one "+
		"enabled global administrator has a linked identity on an enabled provider")

// SettingDirectProvider is the settings key naming (by ID) the provider the sign-in page goes to directly.
const SettingDirectProvider = "auth.direct_provider"

// SignInSettings are the installation's sign-in settings with what SSO-only enforcement needs.
type SignInSettings struct {
	LocalLoginDisabled bool   `json:"local_login_disabled" doc:"SSO-only enforcement: password sign-in is refused (break-glass recovery still works)"`
	DirectProvider     string `json:"direct_provider" doc:"Slug of the provider the sign-in page goes to directly; empty: the page offers every sign-in method"`
	// TestedProviders and SSOAdmins are the preconditions of SSO-only enforcement.
	TestedProviders int64 `json:"tested_providers" doc:"Enabled providers with a recorded successful test login"`
	SSOAdmins       int64 `json:"sso_admins" doc:"Enabled global administrators (manual grant) with a linked identity on an enabled provider"`
	SSOOnlyReady    bool  `json:"sso_only_ready" doc:"Whether password sign-in may be turned off now"`
}

// ssoOnlyReady reports whether SSO-only enforcement is safe: an enabled provider with a recorded test login and at
// least one enabled global administrator with a linked identity on an enabled provider.
func ssoOnlyReady(ctx context.Context, q *db.Queries) (tested, admins int64, ok bool, err error) {
	if tested, err = q.CountTestedOIDCProviders(ctx); err != nil {
		return 0, 0, false, err
	}
	if admins, err = q.CountSSOAdmins(ctx); err != nil {
		return 0, 0, false, err
	}
	return tested, admins, tested > 0 && admins > 0, nil
}

// checkSSOOnlyStillSafe refuses changes (disabling, changing or deleting providers, unlinking identities) that would
// leave SSO-only enforcement on without a working SSO administrator path (Hallpass HP-AUTH-015).
func checkSSOOnlyStillSafe(ctx context.Context, tx *store.Tx) error {
	on, err := auth.LocalLoginDisabledQ(ctx, tx.Q)
	if err != nil || !on {
		return err
	}
	_, _, ok, err := ssoOnlyReady(ctx, tx.Q)
	if err != nil {
		return err
	}
	if !ok {
		return apperr.New(apperr.CodePrecondition, "this change would leave password sign-in off without an SSO administrator or a tested provider; turn password sign-in back on first")
	}
	return nil
}

// SignInSettings returns the sign-in settings.
func (svc *Service) SignInSettings(ctx context.Context) (*SignInSettings, error) {
	q := svc.s.R()
	out := &SignInSettings{}
	var err error
	if out.LocalLoginDisabled, err = auth.LocalLoginDisabledQ(ctx, q); err != nil {
		return nil, err
	}
	if out.DirectProvider, err = DirectProvider(ctx, q); err != nil {
		return nil, err
	}
	if out.TestedProviders, out.SSOAdmins, out.SSOOnlyReady, err = ssoOnlyReady(ctx, q); err != nil {
		return nil, err
	}
	return out, nil
}

// SignInSettingsInput changes the sign-in settings; nil fields stay unchanged.
type SignInSettingsInput struct {
	LocalLoginDisabled *bool   `json:"local_login_disabled,omitempty" doc:"Turn password sign-in off (SSO-only) or back on"`
	DirectProvider     *string `json:"direct_provider,omitempty" maxLength:"32" doc:"Slug of an enabled provider, or empty to turn direct sign-in off"`
}

// SetSignInSettings changes the sign-in settings in one transaction (the direct provider first, so an unknown or disabled
// slug fails before anything changes). Callers require a recent step-up (Hallpass HP-AUTH-014).
func (svc *Service) SetSignInSettings(ctx context.Context, actor audit.Actor, in SignInSettingsInput) (*SignInSettings, error) {
	err := svc.s.W().TxNamed(ctx, "oidc.sign_in_settings", func(tx *store.Tx) error {
		if in.DirectProvider != nil {
			if err := svc.setDirectProvider(ctx, tx, actor, *in.DirectProvider); err != nil {
				return err
			}
		}
		if in.LocalLoginDisabled != nil {
			return svc.setLocalLoginDisabled(ctx, tx, actor, *in.LocalLoginDisabled)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return svc.SignInSettings(ctx)
}

// setLocalLoginDisabled turns SSO-only enforcement on or off.
func (svc *Service) setLocalLoginDisabled(ctx context.Context, tx *store.Tx, actor audit.Actor, disabled bool) error {
	cur, err := auth.LocalLoginDisabledQ(ctx, tx.Q)
	if err != nil {
		return err
	}
	if disabled {
		if _, _, ok, err := ssoOnlyReady(ctx, tx.Q); err != nil {
			return err
		} else if !ok {
			return ErrSSOOnlyPrecondition
		}
	}
	if cur == disabled {
		return nil
	}
	v := "false"
	if disabled {
		v = "true"
	}
	if err := tx.Q.UpsertSetting(ctx, db.UpsertSettingParams{Key: auth.SettingLocalLoginDisabled, Value: v, UpdatedAt: tx.NowMicros(),
		UpdatedBy: sql.NullString{String: actorRef(actor), Valid: true}}); err != nil {
		return err
	}
	_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "auth.local_login.changed", Details: map[string]any{"disabled": disabled}})
	return err
}

// DirectProvider returns the slug of the provider the sign-in page goes to directly, or "" when none is set or the
// provider is gone or disabled.
func DirectProvider(ctx context.Context, q *db.Queries) (string, error) {
	v, err := q.GetSetting(ctx, SettingDirectProvider)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && v.Value == "") {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	p, err := q.GetOIDCProvider(ctx, v.Value)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && p.Enabled != 1) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return p.Slug, nil
}

// setDirectProvider makes the sign-in page go straight to the enabled provider with this slug ("" turns it off).
// Password sign-in, while enabled, stays reachable at /login?password=1.
func (svc *Service) setDirectProvider(ctx context.Context, tx *store.Tx, actor audit.Actor, slug string) error {
	id := ""
	if slug != "" {
		p, err := tx.Q.GetOIDCProviderBySlug(ctx, slug)
		if errors.Is(err, sql.ErrNoRows) {
			return invalid("no sign-in provider %q", slug)
		}
		if err != nil {
			return err
		}
		if p.Enabled != 1 {
			return invalid("enable the provider %q before signing in with it directly", slug)
		}
		id = p.ID
	}
	if cur, err := tx.Q.GetSetting(ctx, SettingDirectProvider); err == nil && cur.Value == id {
		return nil
	} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err := tx.Q.UpsertSetting(ctx, db.UpsertSettingParams{Key: SettingDirectProvider, Value: id, UpdatedAt: tx.NowMicros(),
		UpdatedBy: sql.NullString{String: actorRef(actor), Valid: true}}); err != nil {
		return err
	}
	_, err := svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "auth.direct_provider.changed", Details: map[string]any{"provider": slug}})
	return err
}

// LogoutURL returns the provider's RP-initiated logout URL when the provider has it enabled and advertises an
// end_session_endpoint, else "". It ends the provider session only at that provider; it is not a global logout
// (Hallpass HP-OIDC-015). The ID token is not retained, so no id_token_hint is sent: client_id and
// post_logout_redirect_uri (the public URL plus postLogoutRedirect) identify the client. Some providers (Porch v0.3.0)
// end their own session only with an id_token_hint and otherwise only redirect (docs/contracts/porch.md).
func (svc *Service) LogoutURL(ctx context.Context, providerSlug, postLogoutRedirect string) (string, error) {
	if providerSlug == "" {
		return "", nil
	}
	p, err := svc.providerBySlug(ctx, providerSlug)
	if apperr.CodeOf(err) == apperr.CodeNotFound {
		return "", nil
	}
	if err != nil || !p.EndSessionEnabled || !p.Enabled {
		return "", err
	}
	r, err := svc.discover(ctx, p)
	if err != nil || r.endSession == "" {
		return "", err
	}
	u, err := url.Parse(r.endSession)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("client_id", p.ClientID)
	if postLogoutRedirect != "" {
		q.Set("post_logout_redirect_uri", svc.opts.PublicURL+SanitizeReturnTo(postLogoutRedirect))
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}
