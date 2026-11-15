package app_test

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/inbox"
	"github.com/aurimasniekis/go-spaceman/internal/messages"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/notify"
	tgsettings "github.com/aurimasniekis/go-spaceman/internal/telegram/settings"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/topics"
)

// threadsChat is a linked chat with the notifier running, for the chat-layout tests (ADR-0089).
type threadsChat struct {
	*qrChat
	run string // a control run of the first space, the sender of messages to the operators
}

func newThreadsChat(t *testing.T) (*threadsChat, string) {
	t.Helper()
	c, space := newQRChat(t)
	run := ids.New(ids.Run)
	c.e.exec(t, `INSERT INTO runs (id, space_id, node_id, generation, kind, role, harness, instance_id, desired, observed, launch_json, created_at, created_by, updated_at, observed_at)
		VALUES (?, ?, 'nod_oidc', 1, 'control', 'control', 'claude-code', 'ins_c', 'running', 'running', '{}', 0, 't', 0, 0)`, run, space)
	tc := &threadsChat{qrChat: c, run: run}
	tc.waitNotifier(t, space)
	return tc, space
}

// waitNotifier sends probe messages until one arrives: the notifier starts at the newest change and never replays
// history, so a change made before its start would never be notified.
func (c *threadsChat) waitNotifier(t *testing.T, space string) {
	t.Helper()
	ctx := context.Background()
	for i := 0; ; i++ {
		body := "probe " + strconv.Itoa(i)
		if _, _, err := c.e.h.c.Messages.Send(ctx, audit.System, space, messages.From{Kind: "run", ID: c.run, Label: "control"},
			messages.SendInput{To: "operators", Kind: "request", Subject: "Probe", Body: body}); err != nil {
			t.Fatal(err)
		}
		for range 50 {
			if c.count(body) > 0 {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		if i == 30 {
			t.Fatal("the notifier never delivered a probe")
		}
	}
}

// setThreads stores notification_defaults (the inbound limits newQRChat set are kept) and waits until the settings
// cache has it.
func (c *threadsChat) setThreads(t *testing.T, threads string, inboxItems string) {
	t.Helper()
	ctx := context.Background()
	s := c.e.h.c.TelegramSettings
	cur, err := s.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	apm, burst := int64(600), int64(100)
	in := tgsettings.Input{InboundLimits: &tgsettings.InboundLimitsInput{ActionsPerMinute: &apm, ActionsBurst: &burst},
		NotificationDefaults: &tgsettings.NotificationDefaultsInput{Threads: &threads, InboxItems: &inboxItems}}
	if _, err := s.Put(ctx, audit.System, cur.Version, in); err != nil {
		t.Fatal(err)
	}
	if err := s.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if got := s.Current().NotificationDefaults.Threads; got != threads {
		t.Fatalf("threads %q, want %q", got, threads)
	}
}

// addSpace makes a second space the user and the link can read, visible to Telegram.
func (c *threadsChat) addSpace(t *testing.T, slug, name string) string {
	t.Helper()
	e := c.e
	id := ids.New(ids.Space)
	e.exec(t, `INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES (?, ?, ?, 'nod_oidc', 'active', 'w', 0, 't', 0)`, id, slug, name)
	e.exec(t, `INSERT INTO role_grants (subject, role, domain, source, created_at, created_by) VALUES (?, 'operator', ?, 'manual', 0, 't')`, auth.UserSubject(e.userID), id)
	e.exec(t, `INSERT INTO role_grants (subject, role, domain, source, created_at, created_by) VALUES (?, 'operator', ?, 'telegram', 0, 't')`, auth.TelegramSubject(e.linkID), id)
	ctx := context.Background()
	if err := e.h.c.Authz.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	on := true
	if _, err := e.h.c.TelegramSettings.PutSpace(ctx, audit.System, id, 0, tgsettings.SpaceInput{Enabled: &on}); err != nil {
		t.Fatal(err)
	}
	if err := e.h.c.TelegramSettings.Load(ctx); err != nil {
		t.Fatal(err)
	}
	return id
}

// card waits for the chat's message containing text.
func (c *threadsChat) card(t *testing.T, text string) botapi.Message {
	t.Helper()
	var card botapi.Message
	eventuallyFor(t, "the message with "+text, 10*time.Second, func() bool {
		for _, m := range c.e.tg.Messages(42) {
			if strings.Contains(m.Text, text) {
				card = m
				return true
			}
		}
		return false
	})
	return card
}

func (c *threadsChat) notice(t *testing.T, space, body string) botapi.Message {
	t.Helper()
	if _, _, err := c.e.h.c.Messages.Send(context.Background(), audit.System, space, messages.From{Kind: "run", ID: c.run, Label: "control"},
		messages.SendInput{To: "operators", Kind: "request", Subject: "Status", Body: body}); err != nil {
		t.Fatal(err)
	}
	return c.card(t, body)
}

func (c *threadsChat) question(t *testing.T, space, title string) (string, botapi.Message) {
	t.Helper()
	r := c.e.root.do("POST", "/spaces/"+space+"/human-actions", map[string]any{"type": "question", "title": title,
		"request": map[string]any{"options": []string{"yes", "no"}}})
	expect(t, r, 201)
	var a struct {
		ID string `json:"id"`
	}
	r.json(t, &a)
	return a.ID, c.card(t, title)
}

func (c *threadsChat) inboxItem(t *testing.T, space, title string) botapi.Message {
	t.Helper()
	if _, _, err := c.e.h.c.Inbox.Create(context.Background(), audit.System, space, inbox.CreateInput{SourceKind: "operator", Title: title,
		ContentType: inbox.Markdown, Body: "x"}); err != nil {
		t.Fatal(err)
	}
	return c.card(t, title)
}

func (c *threadsChat) count(text string) int {
	n := 0
	for _, m := range c.e.tg.Messages(42) {
		if strings.Contains(m.Text, text) {
			n++
		}
	}
	return n
}

// SM-TG-008 (ADR-0089): with threads on, every kind of message about a space lands in that space's topic, two spaces
// get two topics and no shared "Needs you" topic; the thread is stored on the link; the layout is announced home once;
// a command written in a topic is answered there; a deleted topic is created again.
func TestTelegramThreadsRouteEachSpaceToItsTopic(t *testing.T) {
	t.Parallel()
	c, alpha := newThreadsChat(t)
	e := c.e
	ctx := context.Background()
	beta := c.addSpace(t, "beta", "Beta space")
	c.setThreads(t, tgsettings.ThreadsOn, tgsettings.DeliveryPush)

	topicOf := map[string]int{}
	for _, space := range []string{alpha, beta} {
		cards := []botapi.Message{c.notice(t, space, "news of "+space)}
		_, q := c.question(t, space, "Question of "+space)
		cards = append(cards, q, c.inboxItem(t, space, "Inbox of "+space))
		for _, m := range cards {
			if m.MessageThreadID == 0 {
				t.Fatalf("%q went to the main chat", m.Text)
			}
			if topicOf[space] == 0 {
				topicOf[space] = m.MessageThreadID
			}
			if m.MessageThreadID != topicOf[space] {
				t.Errorf("%q in thread %d, want the space's topic %d", m.Text, m.MessageThreadID, topicOf[space])
			}
		}
	}
	made := e.tg.Topics(42)
	if len(made) != 2 || made[topicOf[alpha]] != "SSO space" || made[topicOf[beta]] != "Beta space" {
		t.Fatalf("topics %v, want one per space named after it", made)
	}
	for _, name := range made {
		if name == "Needs you" {
			t.Fatal("the shared Needs-you topic must not be created")
		}
	}
	eventuallyFor(t, "the threads stored on the link", 10*time.Second, func() bool {
		row, err := e.h.c.Store.R().GetTelegramLink(ctx, e.linkID)
		return err == nil && strings.Contains(row.TopicsJson, alpha) && strings.Contains(row.TopicsJson, beta)
	})

	// The announcement: home, once.
	if a := c.card(t, notify.ThreadsNotice); a.MessageThreadID != 0 {
		t.Errorf("the announcement in thread %d, want home", a.MessageThreadID)
	}
	if n := c.count(notify.ThreadsNotice); n != 1 {
		t.Errorf("%d announcements, want one", n)
	}

	// A command written in a topic is answered there.
	c.push(&botapi.Message{MessageID: 900, MessageThreadID: topicOf[alpha], From: &botapi.User{ID: 42}, Chat: botapi.Chat{ID: 42, Type: botapi.ChatPrivate}, Text: "/spaces"}, nil)
	eventuallyFor(t, "the answer in the topic", 10*time.Second, func() bool {
		ms := e.tg.Messages(42)
		return len(ms) > 1 && ms[len(ms)-1].MessageThreadID == topicOf[alpha] && !strings.Contains(ms[len(ms)-1].Text, alpha)
	})

	// The topic is deleted: the next card creates it again.
	e.tg.DeleteTopic(42, topicOf[alpha])
	if next := c.notice(t, alpha, "second news"); next.MessageThreadID == 0 || next.MessageThreadID == topicOf[alpha] {
		t.Fatalf("second card in thread %d (first %d), want a new topic", next.MessageThreadID, topicOf[alpha])
	}
}

// Fallbacks (ADR-0089): auto while getMe reports topics off uses the main chat, and so does threads=off; a card sent
// to the main chat keeps editing in place after the link switches to topics.
func TestTelegramThreadsFallbacks(t *testing.T) {
	t.Parallel()
	c, space := newThreadsChat(t)
	e := c.e

	// The default is auto, and the fake bot's getMe has topics off.
	if c.e.h.c.TelegramBot.HasTopics() || e.h.c.TelegramSettings.Current().NotificationDefaults.Threads != tgsettings.ThreadsAuto {
		t.Fatal("want the default auto and a bot without topics")
	}
	if m := c.notice(t, space, "auto news"); m.MessageThreadID != 0 {
		t.Fatalf("auto without bot topics: thread %d, want the main chat", m.MessageThreadID)
	}
	actID, old := c.question(t, space, "Old question")
	if old.MessageThreadID != 0 {
		t.Fatalf("the question went to thread %d", old.MessageThreadID)
	}

	// Threads on: the old main-chat card is edited in place when its action ends; new messages go to the topic.
	c.setThreads(t, tgsettings.ThreadsOn, tgsettings.DeliveryDigest)
	r := e.root.do("GET", "/spaces/"+space+"/human-actions/"+actID, nil)
	expect(t, r, 200)
	var a struct {
		Version int64 `json:"version"`
	}
	r.json(t, &a)
	expect(t, e.root.do("POST", "/spaces/"+space+"/human-actions/"+actID+"/cancel", map[string]any{"reason": "not needed"},
		"If-Match", `"`+strconv.FormatInt(a.Version, 10)+`"`), 200)
	eventuallyFor(t, "the old card edited in place", 10*time.Second, func() bool {
		for _, call := range e.tg.Calls("editMessageText") {
			if call.Params["message_id"] == strconv.Itoa(old.MessageID) {
				return true
			}
		}
		return false
	})
	if n := c.count("Old question"); n != 1 {
		t.Errorf("%d messages for the old question, want its one card", n)
	}
	if m := c.notice(t, space, "topic news"); m.MessageThreadID == 0 {
		t.Fatal("threads on: the card went to the main chat")
	}

	// Threads off again: the main chat.
	c.setThreads(t, tgsettings.ThreadsOff, tgsettings.DeliveryDigest)
	if m := c.notice(t, space, "off news"); m.MessageThreadID != 0 {
		t.Fatalf("threads off: thread %d, want the main chat", m.MessageThreadID)
	}
}

// Upgrade (ADR-0089 decision 3): a link that has the legacy "Needs you" topic gets its needs-you cards in the space
// topic, never in the legacy one, which gets one note and is dropped from the link's topics.
func TestTelegramThreadsRetireTheLegacyNeedsTopic(t *testing.T) {
	t.Parallel()
	c, space := newThreadsChat(t)
	e := c.e
	ctx := context.Background()
	cl, err := botapi.New(botapi.Options{BaseURL: e.tg.URL(), Token: e.tg.Token()})
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := cl.CreateForumTopic(ctx, botapi.CreateForumTopicParams{ChatID: 42, Name: "Needs you"})
	if err != nil {
		t.Fatal(err)
	}
	e.exec(t, `UPDATE telegram_links SET topics_json = ? WHERE id = ?`, `{"needs":`+strconv.Itoa(legacy.MessageThreadID)+`}`, e.linkID)
	c.setThreads(t, tgsettings.ThreadsOn, tgsettings.DeliveryDigest)

	_, q1 := c.question(t, space, "First after the upgrade")
	_, q2 := c.question(t, space, "Second after the upgrade")
	for _, m := range []botapi.Message{q1, q2} {
		if m.MessageThreadID == 0 || m.MessageThreadID == legacy.MessageThreadID {
			t.Errorf("%q in thread %d, want the space topic (legacy %d)", m.Text, m.MessageThreadID, legacy.MessageThreadID)
		}
	}
	in := e.tg.MessagesIn(42, legacy.MessageThreadID)
	if len(in) != 1 || in[0].Text != topics.LegacyNote {
		t.Fatalf("legacy topic messages %+v, want the note only", in)
	}
	eventuallyFor(t, "the legacy key dropped", 10*time.Second, func() bool {
		row, err := e.h.c.Store.R().GetTelegramLink(ctx, e.linkID)
		return err == nil && !strings.Contains(row.TopicsJson, `"needs"`) && strings.Contains(row.TopicsJson, space)
	})
}
