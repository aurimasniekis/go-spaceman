package telegram

import (
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/api"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
)

// AutoExcluded keeps public, step-up and personal operations from Telegram, except Telegram self-service ones, which
// never escape step-up; Classify prefers the table.
func TestAutoExcluded(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		r    api.Rule
		auto bool
	}{
		"public":               {api.Rule{Public: true}, true},
		"step-up":              {api.Rule{Perm: authz.UserWrite, Reauth: true, MFA: true}, true},
		"reauth only":          {api.Rule{Session: true, Reauth: true}, true},
		"user only":            {api.Rule{Session: true, UserOnly: true}, true},
		"personal session":     {api.Rule{Session: true}, true},
		"filtered read":        {api.FilteredRead(), false},
		"space permission":     {api.InSpace(authz.InboxRead), false},
		"telegram self":        {api.Rule{Session: true, UserOnly: true, TelegramSelf: true}, false},
		"self with step-up":    {api.Rule{Session: true, UserOnly: true, TelegramSelf: true, Reauth: true}, true},
		"public telegram self": {api.Rule{Public: true, TelegramSelf: true}, true},
	} {
		if why, got := AutoExcluded(tc.r); got != tc.auto || (got && why == "") {
			t.Errorf("%s: auto-excluded %v (%q), want %v", name, got, why, tc.auto)
		}
	}
	if c, ok := Classify("list-spaces", api.FilteredRead()); !ok || c != api.TelegramBoth {
		t.Errorf("list-spaces: %q %v", c, ok)
	}
	if c, ok := Classify("not-an-op", api.Rule{Session: true}); !ok || c != api.TelegramExcluded {
		t.Errorf("auto-excluded unknown op: %q %v", c, ok)
	}
	if _, ok := Classify("not-an-op", api.InSpace(authz.InboxRead)); ok {
		t.Error("an unlisted op that its rule doesn't exclude must be unclassified")
	}
}
