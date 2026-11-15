package chat

import (
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/telegram/commands"
)

// TestMenuCommandsHaveHandlers keeps the menu, the private handlers and the group set from drifting apart.
func TestMenuCommandsHaveHandlers(t *testing.T) {
	menu := commands.Menu()
	if len(menu) == 0 {
		t.Fatal("empty menu")
	}
	for _, m := range menu {
		if privateHandlers[m.Command] == nil {
			t.Errorf("menu command /%s has no handler", m.Command)
		}
		if m.Command != strings.ToLower(m.Command) {
			t.Errorf("menu command %q is not lower-case", m.Command)
		}
	}
	for _, c := range commands.All() {
		if privateHandlers[c.Name] == nil {
			t.Errorf("/%s has no private handler", c.Name)
		}
		if c.Group != (groupHandlers[c.Name] != nil) {
			t.Errorf("/%s: group flag %v disagrees with the group handler", c.Name, c.Group)
		}
	}
	for name := range privateHandlers {
		if _, ok := commands.Lookup(name); !ok {
			t.Errorf("handler /%s is not in the table", name)
		}
	}
	for _, gone := range []string{"new", "digest"} {
		if _, ok := commands.Lookup(gone); ok {
			t.Errorf("/%s is not implemented and must not be in the table", gone)
		}
	}
}

func TestHelpListsTheMenu(t *testing.T) {
	e := newEnv(t)
	e.say("/help")
	got := e.last().Text
	for _, m := range commands.Menu() {
		if !strings.Contains(got, "/"+m.Command+" - "+m.Description) {
			t.Errorf("help lacks /%s: %q", m.Command, got)
		}
	}
}

func TestSettingsOpensMiniApp(t *testing.T) {
	e := newEnv(t)
	e.say("/settings")
	if got := e.last().Text; got != TextNoMiniApp {
		t.Fatalf("without a host: %q", got)
	}
	e.h.o.AppPage = func(p string) string { return "https://tg.example.com" + p }
	e.say("/settings")
	m := e.last()
	if m.Text != TextOpenApp || m.ReplyMarkup == nil || len(m.ReplyMarkup.InlineKeyboard) != 1 ||
		m.ReplyMarkup.InlineKeyboard[0][0].WebApp == nil || m.ReplyMarkup.InlineKeyboard[0][0].WebApp.URL != "https://tg.example.com/settings" {
		t.Fatalf("settings reply %+v", m)
	}
}

func TestNeedsIsNeedsYou(t *testing.T) {
	e := newEnv(t)
	e.say("/needsyou")
	want := e.last().Text
	e.say("/needs")
	if got := e.last().Text; got != want {
		t.Fatalf("/needs %q, /needsyou %q", got, want)
	}
}
