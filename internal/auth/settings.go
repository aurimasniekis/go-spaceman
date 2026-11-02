package auth

import (
	"context"
	"database/sql"
	"errors"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
)

// SettingLocalLoginDisabled is the settings key that turns off password login (SSO-only enforcement). Break-glass
// recovery logins are unaffected.
const SettingLocalLoginDisabled = "auth.local_login_disabled"

// ErrLocalLoginDisabled is returned for password logins while SSO-only enforcement is on.
var ErrLocalLoginDisabled = apperr.New(apperr.CodePrecondition, "password login is disabled; sign in with SSO")

// LocalLoginDisabled reports whether SSO-only enforcement is on.
func (svc *Service) LocalLoginDisabled(ctx context.Context) (bool, error) {
	v, err := svc.s.R().GetSetting(ctx, SettingLocalLoginDisabled)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return v.Value == "true", nil
}
