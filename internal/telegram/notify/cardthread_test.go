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

// A card whose topic was closed or deleted can't take a new part: the card is sent again with all its parts, together
// in one thread, and the new part is not lost.
func TestCardGrowsAfterItsTopicIsClosedOrDeleted(t *testing.T) {
	for _, tc := range []struct {
		name string
		kill func(e *env, thread int)
	}{
		{"closed", func(e *env, th int) { e.fake.CloseTopic(1001, th) }},
		{"deleted", func(e *env, th int) { e.fake.DeleteTopic(1001, th) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, _, _ := topicsEnv(t)
			defer e.run(e.maps)()
			e.firstCard(12)
			e.waitCalls("sendMessage", 4)
			old := e.cardParts(3)[0].MessageThreadID
			if old == 0 {
				t.Fatal("the card is in the main chat, want a topic")
			}
			tc.kill(e, old)

			r := action("hum_1", 2, false)
			r.Detail = longBody(20)
			e.change("hum_1", r, 2, changefeed.Updated)
			parts := e.cardParts(4)
			th := parts[0].MessageThreadID
			if th == old {
				t.Errorf("the card is still in the %s topic %d", tc.name, old)
			}
			e.allInThread(parts, th)
			if last := parts[3]; !strings.HasPrefix(last.Text, "(4/4)") {
				t.Errorf("last part %.12q, want (4/4)", last.Text)
			}
		})
	}
}

// A settled card whose topic is closed is kept and edited in place to its compact final line: nothing is sent again,
// and its continuations (which carry the body) are deleted.
func TestFinalCardIsKeptWhenItsTopicIsClosed(t *testing.T) {
	e, _, _ := topicsEnv(t)
	defer e.run(e.maps)()
	e.firstCard(12)
	e.waitCalls("sendMessage", 4)
	before := e.cardParts(3)
	e.fake.CloseTopic(1001, before[0].MessageThreadID)

	r := action("hum_1", 2, true)
	r.Detail = longBody(20)
	e.change("hum_1", r, 2, changefeed.Updated)
	parts := e.cardParts(1)
	if parts[0].MessageID != before[0].MessageID || parts[0].Text != "✅ Approved · Deploy hum_1" {
		t.Errorf("the card %.40q was not kept and edited to its final line", parts[0].Text)
	}
	if mk := parts[0].ReplyMarkup; mk != nil && len(mk.InlineKeyboard) != 0 {
		t.Errorf("the settled card keeps a keyboard: %+v", mk)
	}
	ok := 0
	for _, c := range e.fake.Calls("sendMessage") {
		if c.Status == 200 {
			ok++
		}
	}
	if ok != 4 {
		t.Errorf("%d messages sent, want the first 4 only (the card was sent again)", ok)
	}
}

// A delete that fails part way while a card is given up leaves no row of the old parts behind: after a restart the new
// card has only its own parts.
func TestRetireQueuesRowsOfPartsItCouldNotDelete(t *testing.T) {
	e, _, _ := topicsEnv(t)
	e.flushEvery = 2 * time.Millisecond
	stop := e.run(e.maps)
	e.firstCard(12)
	e.waitCalls("sendMessage", 4)
	old := e.cardParts(3)
	e.waitRows(4)
	e.fake.CloseTopic(1001, old[0].MessageThreadID)
	e.fake.FailNext("deleteMessage", 500, "Internal Server Error", 0) // the last continuation can't be deleted

	r := action("hum_1", 2, false)
	r.Detail = longBody(20)
	e.change("hum_1", r, 2, changefeed.Updated)
	deadline := time.Now().Add(10 * time.Second)
	for n := 0; n < 4; { // the new card's four parts (the old part that wouldn't delete is still in the closed topic)
		if time.Now().After(deadline) {
			t.Fatalf("%d parts in the new topic, want 4", n)
		}
		time.Sleep(2 * time.Millisecond)
		n = 0
		for _, m := range e.fake.Messages(1001) {
			if m.MessageThreadID != old[0].MessageThreadID && m.Text != notify.ThreadsNotice {
				n++
			}
		}
	}
	stale := func() (n int) {
		e.maps.mu.Lock()
		defer e.maps.mu.Unlock()
		for _, m := range old {
			if _, ok := e.maps.rows[[2]int64{1001, int64(m.MessageID)}]; ok {
				n++
			}
		}
		return n
	}
	deadline = time.Now().Add(10 * time.Second)
	for stale() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%d rows of the old parts are left", stale())
		}
		time.Sleep(2 * time.Millisecond)
	}
	stop()

	deletes := len(e.fake.Calls("deleteMessage"))
	defer e.run(e.maps)()
	r.Version = 3
	e.change("hum_1", r, 3, changefeed.Updated)
	time.Sleep(100 * time.Millisecond)
	if n := len(e.fake.Calls("deleteMessage")); n != deletes {
		t.Errorf("%d deletes after the restart, the card took old parts with it", n-deletes)
	}
}
