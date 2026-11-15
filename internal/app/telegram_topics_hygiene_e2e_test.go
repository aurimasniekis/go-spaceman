package app_test

import (
	"context"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
	tglink "github.com/aurimasniekis/go-spaceman/internal/telegram/link"
)

// TestTelegramTopicsRenameAndRelinkReuseTopics (ADR-0089 T7): a space's rename edits its topic in the link's chat
// (editForumTopic, the same topic), and a relink of the same chat and Telegram user reuses the revoked link's topics. In
// neither case does the fake Bot API see a second createForumTopic.
func TestTelegramTopicsRenameAndRelinkReuseTopics(t *testing.T) {
	e := newMiniAppEnv(t)
	ui := enrolledSession(t, e.h, "mini@example.com")
	e.startBot(t)
	ctx := t.Context()
	e.h.c.TelegramLinks.Start(ctx)
	bot, err := botapi.New(botapi.Options{BaseURL: e.tg.URL(), Token: e.tg.Token()})
	if err != nil {
		t.Fatal(err)
	}
	send := func(linkID string) botapi.Message {
		t.Helper()
		m, err := e.h.c.TelegramTopics.Send(ctx, bot, linkID, 42, e.space, botapi.SendMessageParams{Text: "a message"})
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	creates := func() int { return len(e.tg.Calls("createForumTopic")) }
	edits := func() int { return len(e.tg.Calls("editForumTopic")) }
	rename := func(name string) {
		t.Helper()
		sp, err := e.h.c.Fleet.GetSpace(ctx, e.space)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.h.c.Fleet.UpdateSpace(ctx, audit.System, e.space, sp.Version, fleet.UpdateSpaceInput{Name: name, DisconnectPolicy: "interrupt"}); err != nil {
			t.Fatal(err)
		}
	}
	// topicNamed waits until the fake's topic thread has the title.
	topicNamed := func(thread int, title string) {
		t.Helper()
		eventuallyFor(t, "the topic to be called "+title, 10*time.Second, func() bool { return e.tg.Topics(42)[thread] == title })
	}

	first := send("tgl_mini")
	if n := creates(); n != 1 {
		t.Fatalf("%d topic creates after the first message, want 1", n)
	}

	rename("Renamed space")
	topicNamed(first.MessageThreadID, "Renamed space")
	if n, c := edits(), creates(); n != 1 || c != 1 {
		t.Fatalf("after the rename: %d edits, %d creates; want 1 and 1", n, c)
	}

	// Unlink, then link the same chat again through the bot and the web: the new link takes the old link's topics.
	if err := e.h.c.TelegramLink.UnlinkAny(ctx, audit.System, "tgl_mini"); err != nil {
		t.Fatal(err)
	}
	relinkTelegramUser42(t, e, ui)
	var relinked string
	for _, l := range listLiveLinks(t, e) {
		if l.TgUserID == 42 {
			relinked = l.ID
		}
	}
	if relinked == "" || relinked == "tgl_mini" {
		t.Fatalf("no new live link for the chat (got %q)", relinked)
	}
	if got := send(relinked); got.MessageThreadID != first.MessageThreadID {
		t.Fatalf("the relinked chat got thread %d, want the old topic %d", got.MessageThreadID, first.MessageThreadID)
	}
	if n := creates(); n != 1 {
		t.Fatalf("%d topic creates after the relink, want 1", n)
	}

	rename("Renamed again")
	topicNamed(first.MessageThreadID, "Renamed again")
	if n, c := edits(), creates(); n != 2 || c != 1 {
		t.Fatalf("after the second rename: %d edits, %d creates; want 2 and 1", n, c)
	}
}

// relinkTelegramUser42 links Telegram user 42 again: a link code made in the web session, /start with the code, the
// [Confirm] button in the bot, then the web confirmation.
func relinkTelegramUser42(t *testing.T, e *miniAppEnv, ui *client) {
	t.Helper()
	from := botapi.User{ID: 42, FirstName: "Mini", Username: "mini"}
	r := ui.do("POST", "/me/telegram/link-codes", nil)
	expect(t, r, 201)
	var code tglink.Code
	r.json(t, &code)

	e.tg.PushUpdate(botapi.Update{UpdateID: 7001, Message: &botapi.Message{MessageID: 900, From: &from,
		Chat: botapi.Chat{ID: 42, Type: botapi.ChatPrivate}, Text: "/start " + code.Code}})
	var button *botapi.Message
	var confirm string
	eventuallyFor(t, "the bot's confirmation buttons", 10*time.Second, func() bool {
		msgs := e.tg.Messages(42)
		if len(msgs) == 0 {
			return false
		}
		m := msgs[len(msgs)-1]
		if m.ReplyMarkup == nil || len(m.ReplyMarkup.InlineKeyboard) == 0 || len(m.ReplyMarkup.InlineKeyboard[0]) == 0 {
			return false
		}
		button, confirm = &m, m.ReplyMarkup.InlineKeyboard[0][0].CallbackData
		return confirm != ""
	})
	e.tg.PushUpdate(botapi.Update{UpdateID: 7002, CallbackQuery: &botapi.CallbackQuery{ID: "cb-relink", From: from, Data: confirm, Message: button}})
	eventuallyFor(t, "the code confirmed in Telegram", 10*time.Second, func() bool {
		g := ui.do("GET", "/me/telegram/link-codes/"+code.ID, nil)
		var c tglink.Code
		g.json(t, &c)
		return c.State == tglink.StateTelegramConfirmed
	})
	expect(t, ui.do("POST", "/me/telegram/link-codes/"+code.ID+"/confirm", map[string]string{"max_role": "operator"}), 201)
}

// listLiveLinks returns the live Telegram links as the store has them.
func listLiveLinks(t *testing.T, e *miniAppEnv) []struct {
	ID       string
	TgUserID int64
} {
	t.Helper()
	rows, err := e.h.c.Store.R().ListLiveTelegramLinks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	out := make([]struct {
		ID       string
		TgUserID int64
	}, 0, len(rows))
	for _, r := range rows {
		out = append(out, struct {
			ID       string
			TgUserID int64
		}{r.ID, r.TgUserID})
	}
	return out
}
