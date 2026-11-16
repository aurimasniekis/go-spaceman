package chat

import (
	"context"
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
)

func allButtons(m botapi.Message) []botapi.InlineKeyboardButton {
	var out []botapi.InlineKeyboardButton
	if m.ReplyMarkup != nil {
		for _, r := range m.ReplyMarkup.InlineKeyboard {
			out = append(out, r...)
		}
	}
	return out
}

// In home (no pinned space, no current space) /needs lists the actions of every space the link may read, grouped by
// space, and ends with a "See all" web_app button for /human.
func TestNeedsInHomeListsAcrossSpaces(t *testing.T) {
	e := newEnv(t)
	e.h.o.PageSize = 10
	e.h.o.AppPage = func(p string) string { return "https://app.example.com" + p }
	e.addHuman(spA, hid('1'), "question", "Which one?", `{}`)
	e.addHuman(spA, hid('2'), "approval", "Deploy?", `{}`)
	e.addHuman(spS, hid('3'), "message", "Heads up", `{}`)
	e.say("/needs")
	m := e.last()
	if !strings.Contains(m.Text, "Needs you — 3") || !strings.Contains(m.Text, "• Alpha — 2") {
		t.Fatalf("home list %q", m.Text)
	}
	bs := allButtons(m)
	var opens []string
	var seeAll *botapi.InlineKeyboardButton
	for i, b := range bs {
		if strings.HasPrefix(b.CallbackData, "ho:") {
			opens = append(opens, b.CallbackData)
		}
		if b.Text == "See all" {
			seeAll = &bs[i]
		}
	}
	if len(opens) < 2 || !strings.Contains(strings.Join(opens, " "), humanData('o', spA, hid('1'), -1)) {
		t.Errorf("open buttons %v", opens)
	}
	if seeAll == nil || seeAll.WebApp == nil || seeAll.WebApp.URL != "https://app.example.com/human" {
		t.Errorf("See all button %+v", seeAll)
	}
	// A current space doesn't narrow home to it.
	e.h.setCurrentNow("tgl_1", spA)
	e.say("/needs")
	if !strings.Contains(e.last().Text, "Needs you — 3") {
		t.Errorf("with a current space %q", e.last().Text)
	}
}

// Home leaves out what the link may not see through Telegram (a hidden space, a space whose content level is off) and
// what it muted; the total counts only what is shown.
func TestNeedsInHomeLeavesOutWhatTheLinkCannotSee(t *testing.T) {
	e := newEnv(t)
	e.h.o.PageSize = 10
	e.addHuman(spA, hid('1'), "question", "Visible one", `{}`)
	e.addHuman(spB, hid('2'), "question", "In a hidden space", `{}`)
	e.addHuman(spC, hid('3'), "question", "Content off", `{}`)
	e.addHuman(spS, hid('4'), "question", "In a muted space", `{}`)
	e.h.o.Muted = func(_, space string) bool { return space == spS }
	e.say("/needs")
	m := e.last()
	if !strings.Contains(m.Text, "Needs you — 1") {
		t.Fatalf("total %q", m.Text)
	}
	for _, bad := range []string{"hidden", "Content off", "muted", "Beta", "Gamma"} {
		if strings.Contains(m.Text, bad) {
			t.Errorf("home shows %q: %q", bad, m.Text)
		}
	}
	for _, b := range allButtons(m) {
		for _, bad := range []string{"hidden space", "Content off", "muted space"} {
			if strings.Contains(b.Text, bad) {
				t.Errorf("button %q", b.Text)
			}
		}
	}
}

// In a space's topic /needs lists that space only, with no cross-space "See all".
func TestNeedsInATopicListsThatSpaceOnly(t *testing.T) {
	e := newEnv(t)
	e.h.o.PageSize = 10
	e.h.o.AppPage = func(p string) string { return "https://app.example.com" + p }
	e.threadSpace = map[int]string{77: spA}
	e.addHuman(spA, hid('1'), "question", "In alpha", `{}`)
	e.addHuman(spS, hid('2'), "question", "In sum", `{}`)
	e.upd++
	e.h.Handle(context.Background(), botapi.Update{UpdateID: e.upd, Message: &botapi.Message{MessageID: int(e.upd), MessageThreadID: 77, From: &botapi.User{ID: chatID},
		Chat: botapi.Chat{ID: chatID, Type: botapi.ChatPrivate}, Text: "/needs"}}, e.l)
	m := e.last()
	if !strings.Contains(m.Text, "Needs you in Space") && !strings.Contains(m.Text, "Needs you in") {
		t.Fatalf("topic list %q", m.Text)
	}
	for _, b := range allButtons(m) {
		if strings.Contains(b.Text, "sum") || b.Text == "See all" {
			t.Errorf("button %q", b.Text)
		}
	}
	if n := len(allButtons(m)); n != 1 {
		t.Errorf("%d buttons, want the one action", n)
	}
}

// /spaces rows get a "Go to topic" URL button when the space has a topic.
func TestSpacesRowsLinkToTheirTopics(t *testing.T) {
	e := newEnv(t)
	e.h.o.ThreadOf = func(_ context.Context, _, space string) (int, bool) { return 42, space == spA }
	e.say("/spaces")
	var goTo []botapi.InlineKeyboardButton
	for _, b := range allButtons(e.last()) {
		if b.Text == "Go to topic" {
			goTo = append(goTo, b)
		}
	}
	if len(goTo) != 1 || goTo[0].URL != "https://t.me/spaceman_test_bot/42" {
		t.Fatalf("Go to topic buttons %+v", goTo)
	}
	e.h.o.ThreadOf = nil
	e.say("/spaces")
	for _, b := range allButtons(e.last()) {
		if b.Text == "Go to topic" {
			t.Fatal("a topic button without topics")
		}
	}
}
