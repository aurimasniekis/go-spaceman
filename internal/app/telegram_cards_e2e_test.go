package app_test

import (
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
)

// cardNow is a chat message as Telegram holds it now.
func (c *threadsChat) cardNow(t *testing.T, id int) botapi.Message {
	t.Helper()
	for _, m := range c.e.tg.Messages(42) {
		if m.MessageID == id {
			return m
		}
	}
	t.Fatalf("message %d is gone", id)
	return botapi.Message{}
}

func noKeyboard(m botapi.Message) bool {
	return m.ReplyMarkup == nil || len(m.ReplyMarkup.InlineKeyboard) == 0
}

// ADR-0089 T4: a question's card carries its options; answering it on the web collapses the same message to its final
// line, without a keyboard, and nothing new is sent for it.
func TestTelegramCardCollapsesWhenResolvedOnTheWeb(t *testing.T) {
	t.Parallel()
	c, space := newThreadsChat(t)
	id, card := c.question(t, space, "Ship the release?")
	if qrFind(card, "yes") == "" || qrFind(card, "More…") == "" {
		t.Fatalf("card %q has no quick actions: %+v", card.Text, card.ReplyMarkup)
	}
	n := c.count("Ship the release?")
	expect(t, c.e.root.do("POST", "/spaces/"+space+"/human-actions/"+id+"/respond", map[string]any{"outcome": "answered", "answer": "yes"}, "If-Match", `"1"`), 200)
	eventuallyFor(t, "the card collapsed", 10*time.Second, func() bool {
		m := c.cardNow(t, card.MessageID)
		return m.Text == "✅ Answered · Ship the release?" && noKeyboard(m)
	})
	if got := c.count("Ship the release?"); got != n {
		t.Fatalf("%d messages about the item, want %d (one message per item)", got, n)
	}
}

// ADR-0089 T4: a card's option answers the question from the chat as the tapping link: the API records the answer, the
// card collapses in place, and a second tap on the old buttons acts on nothing.
func TestTelegramCardAnsweredFromItsButton(t *testing.T) {
	t.Parallel()
	c, space := newThreadsChat(t)
	id, card := c.question(t, space, "Pick a colour")
	before := len(c.e.tg.Messages(42))
	c.tap(qrFind(card, "no"), card)
	eventuallyFor(t, "the card collapsed", 10*time.Second, func() bool {
		m := c.cardNow(t, card.MessageID)
		return strings.HasPrefix(m.Text, "✅ Answered · Pick a colour") && noKeyboard(m)
	})
	var a struct {
		Status   string         `json:"status"`
		Response map[string]any `json:"response"`
	}
	r := c.e.root.do("GET", "/spaces/"+space+"/human-actions/"+id, nil)
	expect(t, r, 200)
	r.json(t, &a)
	if a.Status != "resolved" {
		t.Fatalf("action %+v, want resolved", a)
	}
	c.tap(qrFind(card, "yes"), card)
	if got := c.answer(); got != "That request was already answered or closed." {
		t.Fatalf("second tap answered %q", got)
	}
	if got := len(c.e.tg.Messages(42)); got != before {
		t.Fatalf("%d new messages, want none", got-before)
	}
}

// ADR-0089 T4: More… shows the whole form on the card's own message, and Back brings the card back.
func TestTelegramCardMoreShowsTheFormInPlace(t *testing.T) {
	t.Parallel()
	c, space := newThreadsChat(t)
	_, card := c.question(t, space, "Which region?")
	before := len(c.e.tg.Messages(42))
	c.tap(qrFind(card, "More…"), card)
	var form botapi.Message
	eventuallyFor(t, "the form on the card", 10*time.Second, func() bool {
		form = c.cardNow(t, card.MessageID)
		return qrFind(form, "Claim") != "" && qrFind(form, "‹ Back") != ""
	})
	c.tap(qrFind(form, "‹ Back"), form)
	eventuallyFor(t, "the card again", 10*time.Second, func() bool {
		m := c.cardNow(t, card.MessageID)
		return qrFind(m, "More…") != "" && qrFind(m, "Claim") == ""
	})
	if got := len(c.e.tg.Messages(42)); got != before {
		t.Fatalf("%d new messages, want none", got-before)
	}
}
