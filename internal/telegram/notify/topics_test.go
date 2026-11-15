package notify_test

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/notify"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/settings"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/topics"
)

// threadStore is an in-memory telegram_links.topics_json.
type threadStore struct {
	mu   sync.Mutex
	rows map[string]map[string]int
}

func (s *threadStore) Load(_ context.Context, id string) (map[string]int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]int{}
	for k, v := range s.rows[id] {
		out[k] = v
	}
	return out, nil
}

func (s *threadStore) Save(_ context.Context, id string, m map[string]int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows[id] = m
	return nil
}

func (s *threadStore) get(id string) map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rows[id]
}

// topicsEnv is one link whose chat layout comes from the global default (threads on unless a test changes it), with
// the bot's getMe topics flag in botTopics. The space topics flag is off: routing must not read it.
func topicsEnv(t *testing.T) (*env, *atomic.Bool, *threadStore) {
	t.Helper()
	e := newEnv(t, 1)
	e.pol.set.NotificationDefaults.Threads = settings.ThreadsOn
	e.pol.topics = false
	bot := &atomic.Bool{}
	st := &threadStore{rows: map[string]map[string]int{}}
	e.tp = topics.New(topics.Options{
		Store:     st,
		Mode:      func(id string) string { return e.links.ThreadsMode(id, e.pol.Current().NotificationDefaults) },
		BotTopics: bot.Load,
		SpaceName: func(_ context.Context, id string) string {
			return map[string]string{"spc_1": "Alpha", "spc_2": "Beta"}[id]
		},
		Now: e.clk.Now,
	})
	return e, bot, st
}

// setLinkPrefs changes link 1's preferences (as a reload of the link cache would).
func (e *env) setLinkPrefs(f func(p *notify.Prefs)) {
	ls := append([]notify.Link(nil), e.links.Snapshot()...)
	f(&ls[0].Prefs)
	e.links.Set(ls)
}

func message(id string) notify.Resource {
	return notify.Resource{Key: notify.Key{Type: notify.TypeMessage, ID: id}, SpaceID: "spc_1", Version: 1, Kind: notify.KindMessage,
		Perm: authz.HumanRead, Label: "Message", Title: "Hello " + id, Priority: "normal", Notify: true}
}

func (e *env) emit(r notify.Resource, action string) {
	e.t.Helper()
	before := e.feed.reads.Load()
	e.rd.set(r)
	e.feed.add(r.SpaceID, r.Type, r.ID, action, r.Version)
	e.ring()
	e.endWindow()
	e.settle(before)
}

// threadOf returns the thread of the n-th message of the chat (0: the main chat).
func (e *env) threadOf(chat int64, n int) int {
	e.t.Helper()
	msgs := e.fake.Messages(chat)
	if len(msgs) <= n {
		e.t.Fatalf("%d messages, want more than %d", len(msgs), n)
	}
	return msgs[n].MessageThreadID
}

// threadWith returns the thread of the chat's message whose text starts with prefix.
func (e *env) threadWith(chat int64, prefix string) int {
	e.t.Helper()
	for _, m := range e.waitText(chat, prefix) {
		if strings.HasPrefix(m.Text, prefix) {
			return m.MessageThreadID
		}
	}
	return -1
}

func (e *env) waitText(chat int64, prefix string) []botapi.Message {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		ms := e.fake.Messages(chat)
		for _, m := range ms {
			if strings.HasPrefix(m.Text, prefix) {
				return ms
			}
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("no message starting with %q in %d messages", prefix, len(ms))
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func inSpace(r notify.Resource, space string) notify.Resource { r.SpaceID = space; return r }

// SM-TG-008 (ADR-0089): with the link's layout on, every kind of message lands in its space's topic: needs-you cards
// and their alerts, inbox items, tasks in review, messages to the operators and failed runs; two spaces get two topics,
// the shared "Needs you" topic is never created, and the space topics flag is not read.
func TestThreadsEveryKindLandsInItsSpaceTopic(t *testing.T) {
	e, _, _ := topicsEnv(t)
	e.allow(linkFor(1), "spc_2")
	push := settings.DeliveryPush
	e.setLinkPrefs(func(p *notify.Prefs) { p.InboxItems, p.TasksInReview, p.FailedRuns = &push, &push, &push })
	stop := e.run(e.maps)
	defer stop()

	kinds := []notify.Resource{
		action("hum_1", 1, false),
		inboxItem("inb_1", "Read me"),
		{Key: notify.Key{Type: notify.TypeTask, ID: "tsk_1"}, Version: 1, Kind: notify.KindTaskReview, Perm: authz.QueueRead, Label: "Task", Title: "Review me", State: "review", Notify: true},
		message("msg_1"),
		{Key: notify.Key{Type: notify.TypeRun, ID: "run_1"}, Version: 1, Kind: notify.KindFailedRun, Perm: authz.RunRead, Label: "Run", Title: "Worker", State: "failed", Notify: true},
	}
	for _, space := range []string{"spc_1", "spc_2"} {
		for _, r := range kinds {
			r = inSpace(r, space)
			r.ID += space
			r.Title += " in " + space
			e.emit(r, changefeed.Created)
		}
	}
	// A reminder of the first action: the alert goes to the action's space topic too.
	a := action("hum_1spc_1", 1, false)
	e.waitMappings(10)
	a.RemindedAt = e.clk.Now().Add(time.Hour).UnixMicro()
	e.update(a, 1)
	e.waitText(1001, "Reminder: ")

	made := e.fake.Topics(1001)
	if len(made) != 2 {
		t.Fatalf("topics %v, want Alpha and Beta only", made)
	}
	byName := map[string]int{}
	for th, name := range made {
		byName[name] = th
	}
	for _, m := range e.fake.Messages(1001) {
		want := 0 // home
		switch {
		case strings.Contains(m.Text, "spc_1"):
			want = byName["Alpha"]
		case strings.Contains(m.Text, "spc_2"):
			want = byName["Beta"]
		case m.Text != notify.ThreadsNotice:
			t.Errorf("unexpected message %q", m.Text)
		}
		if m.MessageThreadID != want || want == 0 && m.Text != notify.ThreadsNotice {
			t.Errorf("%q in thread %d, want %d", m.Text, m.MessageThreadID, want)
		}
	}
	// Per space: five cards in each topic, plus the reminder in Alpha's.
	if n := len(e.fake.MessagesIn(1001, byName["Alpha"])); n != 6 {
		t.Errorf("%d messages in Alpha's topic, want 5 cards and the reminder", n)
	}
	if n := len(e.fake.MessagesIn(1001, byName["Beta"])); n != 5 {
		t.Errorf("%d messages in Beta's topic, want 5 cards", n)
	}
	if th := e.threadWith(1001, "Reminder: "); th != byName["Alpha"] {
		t.Errorf("reminder in thread %d, want Alpha's %d", th, byName["Alpha"])
	}
}

// The digest and the "many changes" notice concern several spaces: they go home (the chat without a topic).
func TestThreadsDigestAndOverflowGoHome(t *testing.T) {
	e, _, _ := topicsEnv(t)
	e.setZones(t, "UTC")
	e.clk.Set(time.Date(2026, 11, 14, 8, 0, 0, 0, time.UTC))
	stop := e.run(e.maps)
	defer stop()
	_, _ = e.n.NextDigest(context.Background())                 // first sight of the link
	e.emit(inboxItem("inb_1", "Digest me"), changefeed.Created) // digest by default
	e.emit(message("msg_1"), changefeed.Created)                // a topic message first, so the topic exists
	e.waitText(1001, "Message: ")
	e.clk.Set(time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC))
	if n, err := e.n.DigestDue(context.Background()); n != 1 || err != nil {
		t.Fatalf("DigestDue = %d, %v", n, err)
	}
	if th := e.threadWith(1001, "Daily digest"); th != 0 {
		t.Errorf("digest in thread %d, want home", th)
	}

	before := e.feed.reads.Load()
	for i := range notify.MaxKeys + 1 {
		e.feed.add("spc_1", notify.TypeMessage, "msg_x"+string(rune('a'+i%26))+string(rune('a'+i/26)), changefeed.Created, 1)
	}
	e.ring()
	e.endWindow()
	e.settle(before)
	if th := e.threadWith(1001, "Many changes"); th != 0 {
		t.Errorf("overflow notice in thread %d, want home", th)
	}
}

// Fallbacks: threads=off (global default, or the link's own override over "on") and auto while getMe has topics off
// use the main chat and create no topic; auto follows the bot once getMe reports topics.
func TestThreadsFallbacks(t *testing.T) {
	t.Run("off by default", func(t *testing.T) {
		e, bot, _ := topicsEnv(t)
		bot.Store(true)
		e.pol.set.NotificationDefaults.Threads = settings.ThreadsOff
		stop := e.run(e.maps)
		defer stop()
		e.emit(action("hum_1", 1, false), changefeed.Created)
		e.waitCalls("sendMessage", 1)
		if e.threadOf(1001, 0) != 0 || len(e.fake.Calls("createForumTopic")) != 0 {
			t.Error("threads=off must use the main chat")
		}
	})
	t.Run("the link's override", func(t *testing.T) {
		e, _, _ := topicsEnv(t)
		off := settings.ThreadsOff
		e.setLinkPrefs(func(p *notify.Prefs) { p.Threads = &off })
		stop := e.run(e.maps)
		defer stop()
		e.emit(message("msg_1"), changefeed.Created)
		e.waitCalls("sendMessage", 1)
		if e.threadOf(1001, 0) != 0 || len(e.fake.Calls("createForumTopic")) != 0 {
			t.Error("the link's off must win over the default on")
		}
	})
	t.Run("auto follows getMe", func(t *testing.T) {
		e, bot, _ := topicsEnv(t)
		e.pol.set.NotificationDefaults.Threads = settings.ThreadsAuto
		stop := e.run(e.maps)
		defer stop()
		e.emit(message("msg_1"), changefeed.Created)
		e.waitCalls("sendMessage", 1)
		if e.threadOf(1001, 0) != 0 || len(e.fake.Calls("createForumTopic")) != 0 {
			t.Fatal("auto with getMe topics off must use the main chat")
		}
		bot.Store(true)
		e.emit(message("msg_2"), changefeed.Created)
		e.waitMessages(1001, 2)
		if e.threadWith(1001, "Message: Hello msg_2") == 0 {
			t.Error("auto with getMe topics on must use the space topic")
		}
	})
}

// The layout is read when a message is sent, not when it was queued: a link that turns threads off while its card waits
// gets the card in the main chat.
func TestThreadsModeReadAtSendTime(t *testing.T) {
	e, _, _ := topicsEnv(t)
	stop := e.run(e.maps)
	defer stop()
	wait, release := e.holdReplies()
	e.emit(message("msg_1"), changefeed.Created) // its reply is held: the chat's next message waits behind it
	wait(1)
	e.emit(message("msg_2"), changefeed.Created)
	off := settings.ThreadsOff
	e.setLinkPrefs(func(p *notify.Prefs) { p.Threads = &off })
	release()
	e.waitText(1001, "Message: Hello msg_2")
	if th := e.threadWith(1001, "Message: Hello msg_2"); th != 0 {
		t.Errorf("second card in thread %d, want the main chat (the link turned threads off)", th)
	}
	if th := e.threadWith(1001, "Message: Hello msg_1"); th == 0 {
		t.Error("the first card was sent before the change, in the topic")
	}
}

// A card of the per-space topics mode still edits in place after the upgrade: an edit addresses the message only. A
// main-chat card of a link that switches to threads keeps editing in place too.
func TestThreadsOldMainChatCardEditsInPlace(t *testing.T) {
	e, _, _ := topicsEnv(t)
	off := settings.ThreadsOff
	e.setLinkPrefs(func(p *notify.Prefs) { p.Threads = &off })
	stop := e.run(e.maps)
	defer stop()
	e.emit(action("hum_1", 1, false), changefeed.Created)
	e.waitCalls("sendMessage", 1)
	first := e.fake.Messages(1001)[0]
	e.setLinkPrefs(func(p *notify.Prefs) { p.Threads = nil }) // the default: on
	e.emit(action("hum_1", 2, true), changefeed.Updated)
	edits := e.waitCalls("editMessageText", 1)
	if edits[0].Params["message_id"] != strconv.Itoa(first.MessageID) {
		t.Errorf("edit of message %s, want the old card %d", edits[0].Params["message_id"], first.MessageID)
	}
	if n := len(e.fake.Calls("createForumTopic")); n != 0 {
		t.Errorf("%d topics created for an edit", n)
	}
}

// Upgrade: a link with the legacy "Needs you" topic gets its needs-you cards in the space topic, never in the legacy
// one; the legacy topic gets one note and is dropped from the link's topics, without being deleted.
func TestThreadsLegacyNeedsTopicRetired(t *testing.T) {
	e, _, st := topicsEnv(t)
	legacy := e.createTopic(1001, "Needs you")
	st.rows["tgl_1"] = map[string]int{topics.LegacyNeeds: legacy}
	stop := e.run(e.maps)
	defer stop()
	e.emit(action("hum_1", 1, false), changefeed.Created)
	e.emit(action("hum_2", 1, false), changefeed.Created)
	e.waitText(1001, "Approval: Deploy hum_2")
	in := e.fake.MessagesIn(1001, legacy)
	if len(in) != 1 || in[0].Text != topics.LegacyNote {
		t.Fatalf("legacy topic messages %+v, want the note only", in)
	}
	for _, id := range []string{"hum_1", "hum_2"} {
		if th := e.threadWith(1001, "Approval: Deploy "+id); th == 0 || th == legacy {
			t.Errorf("%s in thread %d, want the space topic (legacy %d)", id, th, legacy)
		}
	}
	if _, kept := st.get("tgl_1")[topics.LegacyNeeds]; kept {
		t.Errorf("stored %v, want the needs key dropped", st.get("tgl_1"))
	}
	if n := len(e.fake.Calls("deleteForumTopic")); n != 0 {
		t.Error("the legacy topic must not be deleted")
	}
}

// The layout is announced home once per link: on its first message in a topic, kept as a notice row, and never again,
// also after a restart. The notices are read once when the notifier starts, not per recipient.
func TestThreadsAnnouncedOncePerLink(t *testing.T) {
	e, _, _ := topicsEnv(t)
	stop := e.run(e.maps)
	e.emit(message("msg_1"), changefeed.Created)
	e.emit(message("msg_2"), changefeed.Created)
	e.waitText(1001, notify.ThreadsNotice)
	e.waitMessages(1001, 3)
	deadline := time.Now().Add(10 * time.Second)
	for {
		rows, _ := e.maps.notices()
		if len(rows) == 1 {
			if rows[0].LinkID != "tgl_1" || rows[0].Key.Type != notify.NoticeThreads || rows[0].SpaceID != "" {
				t.Fatalf("notice row %+v", rows[0])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("notice rows %v, want one", rows)
		}
		time.Sleep(5 * time.Millisecond)
	}
	stop()

	stop = e.run(e.maps) // a restart: the row keeps it from being sent again
	defer stop()
	e.emit(message("msg_3"), changefeed.Created)
	e.waitText(1001, "Message: Hello msg_3")
	time.Sleep(100 * time.Millisecond)
	n := 0
	for _, m := range e.fake.Messages(1001) {
		if m.Text == notify.ThreadsNotice {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d announcements, want one", n)
	}
	if _, reads := e.maps.notices(); reads != 2 {
		t.Errorf("%d notice reads, want one per notifier start", reads)
	}
}

// A link that never lands in a topic (threads off) is never announced the layout.
func TestThreadsNoAnnouncementInTheMainChat(t *testing.T) {
	e, _, _ := topicsEnv(t)
	e.pol.set.NotificationDefaults.Threads = settings.ThreadsOff
	stop := e.run(e.maps)
	defer stop()
	e.emit(message("msg_1"), changefeed.Created)
	e.waitCalls("sendMessage", 1)
	time.Sleep(100 * time.Millisecond)
	if n := len(e.fake.Calls("sendMessage")); n != 1 {
		t.Errorf("%d sends, want the card only", n)
	}
}

func TestTopicDeletedIsRecreated(t *testing.T) {
	e, _, _ := topicsEnv(t)
	e.pol.set.NotificationDefaults.Threads = settings.ThreadsOn
	stop := e.run(nil)
	defer stop()

	e.emit(action("hum_1", 1, false), changefeed.Created)
	e.waitCalls("sendMessage", 1)
	old := e.threadOf(1001, 0)
	e.fake.DeleteTopic(1001, old)
	e.emit(action("hum_2", 1, false), changefeed.Created)
	e.waitCalls("createForumTopic", 2)
	msgs := e.waitText(1001, "Approval: Deploy hum_2")
	if got := msgs[len(msgs)-1].MessageThreadID; got == 0 || got == old {
		t.Errorf("thread %d, want a new topic (old %d)", got, old)
	}
}

func TestTopicClosedFallsBackToTheMainChat(t *testing.T) {
	e, _, _ := topicsEnv(t)
	stop := e.run(nil)
	defer stop()

	e.emit(action("hum_1", 1, false), changefeed.Created)
	e.waitCalls("sendMessage", 1)
	e.fake.CloseTopic(1001, e.threadOf(1001, 0))
	e.fake.DisableTopics(1001) // it can't be recreated either
	e.emit(action("hum_2", 1, false), changefeed.Created)
	if th := e.threadWith(1001, "Approval: Deploy hum_2"); th != 0 {
		t.Errorf("thread %d, want the main chat", th)
	}
}

func TestTopicsRefusedUsesTheMainChat(t *testing.T) {
	e, _, _ := topicsEnv(t)
	e.fake.DisableTopics(1001)
	stop := e.run(e.maps)
	defer stop()

	e.emit(action("hum_1", 1, false), changefeed.Created)
	e.emit(message("msg_1"), changefeed.Created)
	e.waitCalls("sendMessage", 2)
	if e.threadOf(1001, 0) != 0 || e.threadOf(1001, 1) != 0 {
		t.Error("a chat without topics gets the main chat")
	}
	if n := len(e.fake.Calls("createForumTopic")); n != 1 {
		t.Errorf("%d topic attempts, want one (the chat is left alone for a while)", n)
	}
	if e.tp.Enabled("tgl_1", 1001) {
		t.Error("a refused chat is not enabled")
	}
}

func (e *env) waitMessages(chat int64, n int) []botapi.Message {
	e.t.Helper()
	for i := 0; i < 5000; i++ {
		if m := e.fake.Messages(chat); len(m) >= n {
			return m
		}
		time.Sleep(2 * time.Millisecond)
	}
	e.t.Fatalf("waited for %d messages", n)
	return nil
}

// createTopic makes a topic in the fake, as the bot did before the upgrade.
func (e *env) createTopic(chat int64, name string) int {
	e.t.Helper()
	c, err := botapi.New(botapi.Options{BaseURL: e.fake.URL(), Token: e.fake.Token(), HTTPClient: http.DefaultClient})
	if err != nil {
		e.t.Fatal(err)
	}
	tp, err := c.CreateForumTopic(context.Background(), botapi.CreateForumTopicParams{ChatID: chat, Name: name})
	if err != nil {
		e.t.Fatal(err)
	}
	return tp.MessageThreadID
}

// A long card in threads mode: the continuations are replies inside the card's topic, and they are edited and deleted
// along with the card.
func TestThreadsContinuationsFollowTheCardInTheTopic(t *testing.T) {
	e, _, _ := topicsEnv(t)
	defer e.run(e.maps)()
	r := action("hum_1", 1, false)
	r.Detail = longBody(12)
	e.change("hum_1", r, 1, changefeed.Created)
	e.waitCalls("sendMessage", 4) // the card, two continuations and the layout notice
	cardThread := 0
	var card botapi.Message
	deadline := time.Now().Add(10 * time.Second)
	for {
		for _, m := range e.fake.Messages(1001) {
			if m.ReplyToMessage == nil && m.Text != notify.ThreadsNotice {
				card = m
			}
		}
		if card.MessageID != 0 && len(e.fake.MessagesIn(1001, card.MessageThreadID)) == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("messages %+v, want a card and 2-3 continuations in one topic", e.fake.Messages(1001))
		}
		time.Sleep(2 * time.Millisecond)
	}
	cardThread = card.MessageThreadID
	if cardThread == 0 {
		t.Fatal("the card is in the main chat, want its space's topic")
	}
	for _, m := range e.fake.MessagesIn(1001, cardThread) {
		if m.MessageID != card.MessageID && (m.ReplyToMessage == nil || m.ReplyToMessage.MessageID != card.MessageID) {
			t.Errorf("message %d is not a reply to the card", m.MessageID)
		}
	}
	for _, m := range e.fake.Messages(1001) {
		if m.Text != notify.ThreadsNotice && m.MessageThreadID != cardThread {
			t.Errorf("%q in thread %d, want %d", m.Text[:10], m.MessageThreadID, cardThread)
		}
	}

	r2 := action("hum_1", 2, false)
	r2.Detail = longBody(12) + "tail"
	e.change("hum_1", r2, 2, changefeed.Updated)
	e.waitCalls("editMessageText", 2)
	deadline = time.Now().Add(10 * time.Second)
	for {
		ms := e.fake.MessagesIn(1001, cardThread)
		if len(ms) == 3 && strings.Contains(ms[len(ms)-1].Text, "tail") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the last continuation never got the tail: %+v", ms)
		}
		time.Sleep(2 * time.Millisecond)
	}

	r3 := action("hum_1", 3, true)
	r3.Detail = "Short now."
	e.change("hum_1", r3, 3, changefeed.Updated)
	deadline = time.Now().Add(10 * time.Second)
	for len(e.fake.MessagesIn(1001, cardThread)) != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("%d messages in the topic, want the card alone", len(e.fake.MessagesIn(1001, cardThread)))
		}
		time.Sleep(2 * time.Millisecond)
	}
}
