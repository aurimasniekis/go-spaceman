package telegram

import (
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/api"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
)

// ADR-0093 on Telegram: renaming a role and listing its renames are not offered in the bot (both are excluded, like
// the role's delete and save), whatever the rule says.
func TestRoleRenameIsExcludedFromTelegram(t *testing.T) {
	t.Parallel()
	for _, op := range []string{"rename-space-role", "list-space-role-renames", "retry-space-role-rename", "close-space-role-rename"} {
		if c, ok := Classify(op, api.InSpace(authz.SpaceConfigWrite)); !ok || c != api.TelegramExcluded {
			t.Errorf("%s: %q (classified %v), want %q", op, c, ok, api.TelegramExcluded)
		}
	}
}
