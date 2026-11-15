package link

import (
	"errors"
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/settings"
)

// ADR-0083 decision 3: the cap a link asks for against the policy. Empty takes the default; viewer always; operator
// only up to the default; admin only with link_allow_admin_cap; anything else is invalid. A stored default outside
// viewer/operator never widens.
func TestCapRole(t *testing.T) {
	t.Parallel()
	pol := func(def string, admin bool) settings.Settings {
		s := settings.Defaults()
		s.LinkDefaultMaxRole, s.LinkAllowAdminCap = def, admin
		return s
	}
	cases := []struct {
		req  string
		pol  settings.Settings
		want authz.Role
		code apperr.Code
	}{
		{"", pol("operator", false), authz.RoleOperator, ""},
		{"", pol("viewer", false), authz.RoleViewer, ""},
		{"", pol("admin", true), authz.RoleViewer, ""},
		{"", pol("bogus", false), authz.RoleViewer, ""},
		{"viewer", pol("viewer", false), authz.RoleViewer, ""},
		{"operator", pol("operator", false), authz.RoleOperator, ""},
		{"operator", pol("viewer", true), "", apperr.CodeForbidden},
		{"admin", pol("operator", false), "", apperr.CodeForbidden},
		{"admin", pol("viewer", true), authz.RoleAdmin, ""},
		{"superuser", pol("operator", true), "", apperr.CodeInvalid},
	}
	for _, c := range cases {
		got, err := capRole(c.req, c.pol)
		if c.code != "" {
			var ae *apperr.Error
			if !errors.As(err, &ae) || ae.Code != c.code {
				t.Fatalf("capRole(%q, %s/%v) error %v, want %s", c.req, c.pol.LinkDefaultMaxRole, c.pol.LinkAllowAdminCap, err, c.code)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Fatalf("capRole(%q, %s/%v) = %q, %v; want %q", c.req, c.pol.LinkDefaultMaxRole, c.pol.LinkAllowAdminCap, got, err, c.want)
		}
	}
}

// The bot's prompt shows untrusted names on one line and masks the e-mail; only "/start <payload>" and link buttons
// are linking steps.
func TestPromptTextAndClaims(t *testing.T) {
	t.Parallel()
	if got := clean("Jane\n\u202eDoe\t  x\x00", 64); got != "Jane Doe x" {
		t.Fatalf("clean: %q", got)
	}
	if got := clean(strings.Repeat("é", 100), 64); len([]rune(got)) != 64 {
		t.Fatalf("clean didn't cut: %d runes", len([]rune(got)))
	}
	for in, want := range map[string]string{"jane@example.com": "j***@example.com", "noat": "", "@example.com": "", "": "", "é@x.org": "é***@x.org"} {
		if got := maskEmail(in); got != want {
			t.Fatalf("maskEmail(%q) = %q, want %q", in, got, want)
		}
	}
	s := New(Options{PublicURL: "https://central.example.com"})
	r := s.prompt("id", "Jäne 😀", "j***@example.com")
	if !strings.Contains(r.Text, "central.example.com") || len(r.Entities) != 2 {
		t.Fatalf("prompt %+v", r)
	}
	// Entity offsets are UTF-16: the name's bold run covers exactly the name, emoji included.
	name := r.Entities[1]
	if u := []rune(r.Text); name.Length != 7 || string(u[len([]rune("Link this Telegram account to SpaceMan at central.example.com as ")):][:6]) != "Jäne 😀" {
		t.Fatalf("name entity %+v in %q", name, r.Text)
	}
	for text, want := range map[string]bool{"/start abc": true, "/start  abc ": true, "/start": false, "/start ": false, "/startabc": false, "hi": false} {
		u := botapi.Update{Message: &botapi.Message{Text: text}}
		if got := s.Claims(&u); got != want {
			t.Fatalf("Claims(%q) = %v", text, got)
		}
	}
	if !s.Claims(&botapi.Update{CallbackQuery: &botapi.CallbackQuery{Data: "tglink:c:x"}}) || s.Claims(&botapi.Update{CallbackQuery: &botapi.CallbackQuery{Data: "hum:1"}}) {
		t.Fatal("Claims on callbacks")
	}
}

// SM-TG-001 (PR #86 review): clean turns the invisible filler characters into spaces, so a name made of them is empty
// and one padded with them shows no gap, and keeps at most two combining marks in a row, so stacked marks can't smear
// over the lines around the name. Ordinary stacked marks (Vietnamese, Devanagari) survive.
func TestCleanHardening(t *testing.T) {
	t.Parallel()
	zalgo := "e" + strings.Repeat("\u0301\u0316\u0489", 40) // a run of combining marks (U+0489 is Me: marks of every kind count)
	cases := []struct{ in, want string }{
		{"\u2800\u3164\u115F\u1160\uFFA0", ""},
		{"Jane\u2800\u2800Doe", "Jane Doe"},
		{"\u3164Jane\u115F\u1160\uFFA0 Doe\u2800", "Jane Doe"},
		{"a\u0300\u0301b", "a\u0300\u0301b"},
		{"a\u0300\u0301\u0302\u0303\u0304b", "a\u0300\u0301b"},
		{"x" + strings.Repeat("\u0301", 500), "x\u0301\u0301"},
		{"a\u0301\u0301\u0301 b\u0301\u0301\u0301", "a\u0301\u0301 b\u0301\u0301"},
		{"a\u0301\u0301\u0301b\u0301", "a\u0301\u0301b\u0301"},
		{"ệ", "ệ"},
		{"\u0915\u094D\u0937", "\u0915\u094D\u0937"},
	}
	for _, c := range cases {
		if got := clean(c.in, 64); got != c.want {
			t.Fatalf("clean(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if got := clean(zalgo, 64); len([]rune(got)) > 8 {
		t.Fatalf("zalgo kept %d runes: %q", len([]rune(got)), got)
	}
	// An all-filler name falls back to the account's ID in the bot's prompt, never to blank.
	if n := displayName(&auth.User{ID: "usr_1", DisplayName: "\u2800\u3164"}); n != "usr_1" {
		t.Fatalf("displayName of a filler name: %q", n)
	}
}
