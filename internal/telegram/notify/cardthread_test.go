package notify_test

import (
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/notify"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/settings"
)

// cardParts waits until the chat holds n messages of the card (everything but the layout announcement) and returns
// them.
func (e *env) cardParts(n int) []botapi.Message {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var out []botapi.Message
		for _, m := range e.fake.Messages(1001) {
			if m.Text != notify.ThreadsNotice {
				out = append(out, m)
			}
		}
		if len(out) == n {
			return out
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("%d card messages, want %d", len(out), n)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// grow sends the card's body again, longer, and waits for the sends to reach sends and the card to be n parts.
func (e *env) grow(paras int, v int64, sends, n int) []botapi.Message {
	e.t.Helper()
	r := action("hum_1", v, false)
	r.Detail = longBody(paras)
	e.change("hum_1", r, v, changefeed.Updated)
	e.waitCalls("sendMessage", sends)
	return e.cardParts(n)
}

// threads sets link 1's layout (through its preferences, which the sender reads under a lock).
func (e *env) threads(mode string) {
	e.setLinkPrefs(func(p *notify.Prefs) { p.Threads = &mode })
}

func (e *env) allInThread(parts []botapi.Message, thread int) {
	e.t.Helper()
	for i, m := range parts {
		if m.MessageThreadID != thread {
			e.t.Errorf("part %d is in thread %d, want %d (%.12q)", i+1, m.MessageThreadID, thread, m.Text)
		}
	}
}

func (e *env) firstCard(body int) {
	e.t.Helper()
	r := action("hum_1", 1, false)
	r.Detail = longBody(body)
	e.change("hum_1", r, 1, changefeed.Created)
}

// A card that grows after the layout changed keeps every part in the thread the card was sent in: threads off then
// on, the card stays in the main chat; no topic is created and no layout notice is sent for it.
func TestCardGrowsInItsThreadWhenThreadsTurnOn(t *testing.T) {
	e, _, _ := topicsEnv(t)
	e.threads(settings.ThreadsOff)
	defer e.run(e.maps)()
	e.firstCard(12)
	e.allInThread(e.cardParts(3), 0)

	e.threads(settings.ThreadsOn)
	parts := e.grow(20, 2, 4, 4)
	e.allInThread(parts, 0)
	for _, m := range parts[1:] {
		if m.ReplyToMessage == nil || m.ReplyToMessage.MessageID != parts[0].MessageID {
			t.Errorf("message %d is not a reply to the card", m.MessageID)
		}
	}
	if n := len(e.fake.Calls("createForumTopic")); n != 0 {
		t.Errorf("%d topics created for a card that isn't in a topic", n)
	}
	for _, m := range e.fake.Messages(1001) {
		if m.Text == notify.ThreadsNotice {
			t.Error("the layout notice was sent for a card in the main chat")
		}
	}
}

// And the other way: a card in a space topic keeps growing there after threads are turned off.
func TestCardGrowsInItsTopicWhenThreadsTurnOff(t *testing.T) {
	e, _, _ := topicsEnv(t)
	defer e.run(e.maps)()
	e.firstCard(12)
	e.waitCalls("sendMessage", 4) // three parts and the layout notice
	parts := e.cardParts(3)
	topic := parts[0].MessageThreadID
	if topic == 0 {
		t.Fatal("the card is in the main chat, want a topic")
	}
	e.allInThread(parts, topic)

	e.threads(settings.ThreadsOff)
	e.allInThread(e.grow(20, 2, 5, 4), topic)
}

// After a restart the thread comes from the stored mapping.
func TestCardThreadSurvivesARestart(t *testing.T) {
	e, _, _ := topicsEnv(t)
	e.flushEvery = 2 * time.Millisecond
	stop := e.run(e.maps)
	e.firstCard(12)
	e.waitCalls("sendMessage", 4)
	e.waitRows(4) // the card, two continuations and the notice
	topic := e.cardParts(3)[0].MessageThreadID
	stop()

	e.maps.mu.Lock()
	for k, r := range e.maps.rows {
		if !r.Cont && r.Kind != notify.MappingNotice && !r.ThreadKnown {
			t.Errorf("card row %v was stored without its thread", k)
		}
	}
	e.maps.mu.Unlock()

	e.threads(settings.ThreadsOff)
	defer e.run(e.maps)()
	e.allInThread(e.grow(20, 2, 5, 4), topic)
}

// A row from before the thread was recorded sends the new part the way it always did: by the current layout.
func TestCardWithoutAThreadFallsBackToTheLayout(t *testing.T) {
	e, _, _ := topicsEnv(t)
	e.flushEvery = 2 * time.Millisecond
	stop := e.run(e.maps)
	e.firstCard(12)
	e.waitCalls("sendMessage", 4)
	e.waitRows(4)
	topic := e.cardParts(3)[0].MessageThreadID
	stop()

	e.maps.mu.Lock()
	for k, r := range e.maps.rows {
		r.Thread, r.ThreadKnown = 0, false
		e.maps.rows[k] = r
	}
	e.maps.mu.Unlock()

	e.threads(settings.ThreadsOff)
	defer e.run(e.maps)()
	parts := e.grow(20, 2, 5, 4)
	if last := parts[len(parts)-1]; last.MessageThreadID != 0 || !strings.HasPrefix(last.Text, "(4/4)") {
		t.Errorf("last part %.12q in thread %d, want the current layout's main chat (card in %d)", last.Text, last.MessageThreadID, topic)
	}
}
