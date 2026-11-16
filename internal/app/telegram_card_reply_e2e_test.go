package app_test

import (
	"strings"
	"testing"
	"time"
)

// inboxCount is how many inbox items a space has.
func (c *threadsChat) inboxCount(t *testing.T, space string) int {
	t.Helper()
	r := c.e.root.do("GET", "/spaces/"+space+"/inbox", nil)
	expect(t, r, 200)
	var page struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	r.json(t, &page)
	return len(page.Items)
}

// A text reply to a question card of another space answers that card (the same API call as its buttons) and never
// reaches the inbox of the space the chat would otherwise write to. A reply that isn't an answer goes to the card's own
// space, and the chat says which.
func TestTelegramReplyToCardAnswersItsAction(t *testing.T) {
	t.Parallel()
	c, home := newThreadsChat(t)
	other := c.addSpace(t, "ops", "Ops")
	c.say("/space "+home, nil) // the chat's current space is the first one, not the card's
	id, card := c.question(t, other, "Roll back?")
	home0, other0 := c.inboxCount(t, home), c.inboxCount(t, other)

	c.say("No", &card)
	var a struct {
		Status   string         `json:"status"`
		Response map[string]any `json:"response"`
	}
	r := c.e.root.do("GET", "/spaces/"+other+"/human-actions/"+id, nil)
	expect(t, r, 200)
	r.json(t, &a)
	if a.Status != "resolved" || a.Response["answer"] != "no" {
		t.Fatalf("action %+v, want resolved with the answer no", a)
	}
	if got := c.last().Text; got != "Answered: no" {
		t.Fatalf("confirmation %q", got)
	}
	eventuallyFor(t, "the card collapsed", 10*time.Second, func() bool {
		return strings.HasPrefix(c.cardNow(t, card.MessageID).Text, "✅ Answered · Roll back?")
	})
	if c.inboxCount(t, home) != home0 || c.inboxCount(t, other) != other0 {
		t.Fatalf("inbox changed by an answer: home %d→%d, other %d→%d", home0, c.inboxCount(t, home), other0, c.inboxCount(t, other))
	}

	// A closed action says so.
	c.say("yes", &card)
	if got := c.last().Text; got != "That request was already answered or closed." {
		t.Fatalf("reply to a closed card: %q", got)
	}

	// Not an answer: it lands in the card's space, never the chat's current one.
	_, card2 := c.question(t, other, "Scale up?")
	c.say("let me think about it", &card2)
	if !strings.HasPrefix(c.last().Text, "Added to the inbox of") {
		t.Fatalf("ack %q", c.last().Text)
	}
	if c.inboxCount(t, other) != other0+1 || c.inboxCount(t, home) != home0 {
		t.Fatalf("unmatched reply: home %d→%d, other %d→%d", home0, c.inboxCount(t, home), other0, c.inboxCount(t, other))
	}
}
