package auth

import (
	"context"
	"database/sql"
	"errors"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// SettingLocalLoginDisabled is the settings key that turns off password login (SSO-only enforcement, ported from
// Hallpass HP-AUTH-015, ADR-0045). It is changed through the sign-in settings API (internal/oidc), never by the
// configuration file. Break-glass recovery logins are unaffected.
const SettingLocalLoginDisabled = "auth.local_login_disabled"

// ErrLocalLoginDisabled is returned for password logins while SSO-only enforcement is on.
var ErrLocalLoginDisabled = apperr.New(apperr.CodePrecondition, "password login is disabled; sign in with SSO")

// ErrLastSSOAdmin protects SSO-only installations against administrator lockout (Hallpass HP-AUTH-015, SR-06).
var ErrLastSSOAdmin = apperr.New(apperr.CodePrecondition,
	"password sign-in is off and this change would leave no enabled global administrator who can sign in with SSO; turn password sign-in back on first")

// LocalLoginDisabled reports whether SSO-only enforcement is on.
func (svc *Service) LocalLoginDisabled(ctx context.Context) (bool, error) {
	return LocalLoginDisabledQ(ctx, svc.s.R())
}

// LocalLoginDisabledQ reports whether SSO-only enforcement is on, read with q (a transaction or the read pool).
func LocalLoginDisabledQ(ctx context.Context, q *db.Queries) (bool, error) {
	v, err := q.GetSetting(ctx, SettingLocalLoginDisabled)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return v.Value == "true", nil
}

// ssoAdminGuard remembers how many SSO-capable administrators existed before a change; check refuses the change when
// SSO-only enforcement is on and the change removed the last one (Hallpass opauth ssoAdminGuard).
type ssoAdminGuard struct {
	on     bool
	before int64
}

func newSSOAdminGuard(ctx context.Context, q *db.Queries) (ssoAdminGuard, error) {
	on, err := LocalLoginDisabledQ(ctx, q)
	if err != nil || !on {
		return ssoAdminGuard{}, err
	}
	n, err := q.CountSSOAdmins(ctx)
	return ssoAdminGuard{on: true, before: n}, err
}

func (g ssoAdminGuard) check(ctx context.Context, q *db.Queries) error {
	if !g.on || g.before == 0 {
		return nil
	}
	n, err := q.CountSSOAdmins(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrLastSSOAdmin
	}
	return nil
}
