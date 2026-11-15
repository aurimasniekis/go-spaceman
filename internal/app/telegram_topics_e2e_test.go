package app_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/messages"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
	tgsettings "github.com/aurimasniekis/go-spaceman/internal/telegram/settings"
)

// SM-TG-008 (TG-11): a space with `topics` on gets a topic in the link's private chat; a notifier card goes to it, the
// thread is stored on the link, a deleted topic is created again, and a chat reply follows the topic it was written in.
func TestTelegramTopicsModeRoutesCardsAndReplies(t *testing.T) {
	t.Parallel()
	c, space := newQRChat(t)
	e := c.e
	ctx := context.Background()
	on := true
	if _, err := e.h.c.TelegramSettings.PutSpace(ctx, audit.System, space, 1, tgsettings.SpaceInput{Enabled: &on, Topics: &on}); err != nil {
		t.Fatal(err)
	}
	if err := e.h.c.TelegramSettings.Load(ctx); err != nil {
		t.Fatal(err)
	}
	run := ids.New(ids.Run)
	e.exec(t, `INSERT INTO runs (id, space_id, node_id, generation, kind, role, harness, instance_id, desired, observed, launch_json, created_at, created_by, updated_at, observed_at)
		VALUES (?, ?, 'nod_oidc', 1, 'control', 'control', 'claude-code', 'ins_c', 'running', 'running', '{}', 0, 't', 0, 0)`, run, space)
	notice := func(body string) botapi.Message {
		t.Helper()
		if _, _, err := e.h.c.Messages.Send(ctx, audit.System, space, messages.From{Kind: "run", ID: run, Label: "control"},
			messages.SendInput{To: "operators", Kind: "request", Subject: "Status", Body: body}); err != nil {
			t.Fatal(err)
		}
		var card botapi.Message
		eventuallyFor(t, "the card for "+body, 10*time.Second, func() bool {
			for _, m := range e.tg.Messages(42) {
				if strings.Contains(m.Text, body) {
					card = m
					return true
				}
			}
			return false
		})
		return card
	}

	card := notice("first news")
	if card.MessageThreadID == 0 {
		t.Fatalf("the card went to the main chat: %+v", card)
	}
	if name := e.tg.Topics(42)[card.MessageThreadID]; name == "" {
		t.Fatalf("topics %v, want the card's thread", e.tg.Topics(42))
	}
	var stored string
	eventuallyFor(t, "the thread stored on the link", 10*time.Second, func() bool {
		row, err := e.h.c.Store.R().GetTelegramLink(ctx, e.linkID)
		stored = row.TopicsJson
		return err == nil && strings.Contains(stored, space)
	})

	// A command written in the topic is answered there.
	c.push(&botapi.Message{MessageID: 900, MessageThreadID: card.MessageThreadID, From: &botapi.User{ID: 42}, Chat: botapi.Chat{ID: 42, Type: botapi.ChatPrivate}, Text: "/spaces"}, nil)
	eventuallyFor(t, "the answer in the topic", 10*time.Second, func() bool {
		ms := e.tg.Messages(42)
		return len(ms) > 1 && ms[len(ms)-1].MessageThreadID == card.MessageThreadID && ms[len(ms)-1].MessageID != card.MessageID
	})

	// The topic is deleted: the next card creates it again.
	e.tg.DeleteTopic(42, card.MessageThreadID)
	next := notice("second news")
	if next.MessageThreadID == 0 || next.MessageThreadID == card.MessageThreadID {
		t.Fatalf("second card in thread %d (first %d), want a new topic", next.MessageThreadID, card.MessageThreadID)
	}
}
