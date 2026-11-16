package app_test

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/notify"
	tgsettings "github.com/aurimasniekis/go-spaceman/internal/telegram/settings"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/topics"
)

// ADR-0089 T9: one scenario over the whole chat layout on the fake Bot API. A link reads three spaces and starts with
// the legacy "Needs you" topic. With threads on, each kind of message lands in its own space's topic and the legacy
// topic is retired; an item resolved on the web collapses in place; threads off sends the next message to the main chat;
// and a relink of the same chat and user reuses the topics instead of creating them again.
func TestTelegramThreadsEndToEnd(t *testing.T) {
	t.Parallel()
	c, alpha := newThreadsChat(t)
	e := c.e
	ctx := t.Context()
	beta := c.addSpace(t, "beta", "Beta space")
	gamma := c.addSpace(t, "gamma", "Gamma space")
	spaces := map[string]string{alpha: "SSO space", beta: "Beta space", gamma: "Gamma space"}

	// The chat has the old shared topic from before the upgrade.
	cl, err := botapi.New(botapi.Options{BaseURL: e.tg.URL(), Token: e.tg.Token()})
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := cl.CreateForumTopic(ctx, botapi.CreateForumTopicParams{ChatID: 42, Name: "Needs you"})
	if err != nil {
		t.Fatal(err)
	}
	e.exec(t, `UPDATE telegram_links SET topics_json = ? WHERE id = ?`, `{"needs":`+strconv.Itoa(legacy.MessageThreadID)+`}`, e.linkID)
	c.setThreads(t, tgsettings.ThreadsOn, tgsettings.DeliveryPush)

	// Each kind of message about a space lands in that space's topic.
	topicOf := map[string]int{}
	var firstQuestion botapi.Message
	var firstAction string
	for _, space := range []string{alpha, beta, gamma} {
		id, q := c.question(t, space, "Question of "+space)
		if space == alpha {
			firstAction, firstQuestion = id, q
		}
		for _, m := range []botapi.Message{q, c.notice(t, space, "news of "+space), c.inboxItem(t, space, "Inbox of "+space)} {
			if m.MessageThreadID == 0 || m.MessageThreadID == legacy.MessageThreadID {
				t.Fatalf("%q in thread %d, want the topic of its space", m.Text, m.MessageThreadID)
			}
			if topicOf[space] == 0 {
				topicOf[space] = m.MessageThreadID
			}
			if m.MessageThreadID != topicOf[space] {
				t.Errorf("%q in thread %d, want %d", m.Text, m.MessageThreadID, topicOf[space])
			}
		}
	}
	made := e.tg.Topics(42)
	for space, name := range spaces {
		if made[topicOf[space]] != name {
			t.Errorf("topic %d is %q, want %q", topicOf[space], made[topicOf[space]], name)
		}
	}
	if topicOf[alpha] == topicOf[beta] || topicOf[beta] == topicOf[gamma] || topicOf[alpha] == topicOf[gamma] {
		t.Fatalf("the spaces share topics: %v", topicOf)
	}
	if len(made) != 4 { // three spaces and the legacy topic, which is never deleted
		t.Errorf("topics %v, want one per space and the old one", made)
	}

	// The legacy topic got its note and nothing else, and is no longer the link's.
	if in := e.tg.MessagesIn(42, legacy.MessageThreadID); len(in) != 1 || in[0].Text != topics.LegacyNote {
		t.Errorf("legacy topic messages %+v, want the note only", in)
	}
	eventuallyFor(t, "the legacy key dropped", 10*time.Second, func() bool {
		row, err := e.h.c.Store.R().GetTelegramLink(ctx, e.linkID)
		return err == nil && !strings.Contains(row.TopicsJson, `"needs"`) && strings.Contains(row.TopicsJson, gamma)
	})
	if n := c.count(notify.ThreadsNotice); n != 1 {
		t.Errorf("%d layout announcements, want one", n)
	}

	// A resolved item collapses in its topic and nothing new is sent for it.
	n := c.count("Question of " + alpha)
	expect(t, e.root.do("POST", "/spaces/"+alpha+"/human-actions/"+firstAction+"/respond", map[string]any{"outcome": "answered", "answer": "yes"}, "If-Match", `"1"`), 200)
	eventuallyFor(t, "the card collapsed in place", 10*time.Second, func() bool {
		m := c.cardNow(t, firstQuestion.MessageID)
		return m.Text == "✅ Answered · Question of "+alpha && noKeyboard(m) && m.MessageThreadID == topicOf[alpha]
	})
	if got := c.count("Question of " + alpha); got != n {
		t.Errorf("%d messages about the resolved item, want %d", got, n)
	}

	// A relink of the same chat and user reuses the topics.
	creates := len(e.tg.Calls("createForumTopic"))
	if err := e.h.c.TelegramLink.UnlinkAny(ctx, audit.System, e.linkID); err != nil {
		t.Fatal(err)
	}
	relinkTelegramUser42(t, e, c.ui)
	var relinked string
	for _, l := range listLiveLinks(t, e) {
		if l.TgUserID == 42 {
			relinked = l.ID
		}
	}
	if relinked == "" || relinked == e.linkID {
		t.Fatalf("no new live link for the chat (got %q)", relinked)
	}
	e.linkID = relinked
	for _, space := range []string{alpha, beta, gamma} {
		if m := c.notice(t, space, "after the relink "+space); m.MessageThreadID != topicOf[space] {
			t.Errorf("after the relink %s is in thread %d, want its old topic %d", space, m.MessageThreadID, topicOf[space])
		}
	}
	if got := len(e.tg.Calls("createForumTopic")); got != creates {
		t.Errorf("%d topic creates after the relink, want %d (none)", got-creates, 0)
	}

	// Threads off: the main chat again.
	c.setThreads(t, tgsettings.ThreadsOff, tgsettings.DeliveryPush)
	if m := c.notice(t, beta, "off news"); m.MessageThreadID != 0 {
		t.Errorf("threads off: thread %d, want the main chat", m.MessageThreadID)
	}
}

// ADR-0089 T5: the pinned home summary counts the pending items of the spaces the link may read, is sent once and
// pinned, and is then edited (not sent again) when the counts change. The edit waits for the summary's interval, which
// the test's clock passes.
func TestTelegramThreadsHomeSummaryCounts(t *testing.T) {
	t.Parallel()
	c, alpha := newThreadsChat(t)
	e := c.e
	beta := c.addSpace(t, "beta", "Beta space")
	c.setThreads(t, tgsettings.ThreadsOn, tgsettings.DeliveryDigest)

	summary := func() (botapi.Message, bool) {
		for _, m := range e.tg.Messages(42) {
			if strings.HasPrefix(m.Text, "Needs you") {
				return m, true
			}
		}
		return botapi.Message{}, false
	}
	c.question(t, alpha, "First question")
	var home botapi.Message
	eventuallyFor(t, "the summary sent", 10*time.Second, func() bool {
		var ok bool
		home, ok = summary()
		return ok
	})
	if home.MessageThreadID != 0 {
		t.Errorf("the summary is in thread %d, want home", home.MessageThreadID)
	}
	eventuallyFor(t, "the summary pinned", 10*time.Second, func() bool { return len(e.tg.Pinned(42)) == 1 && e.tg.Pinned(42)[0] == home.MessageID })

	c.question(t, alpha, "Second question")
	c.question(t, beta, "Third question")
	// Approvals are viewable by admins only, and the link is an operator: they are not counted, and a space with
	// nothing else is not listed.
	gamma := c.addSpace(t, "gamma", "Gamma space")
	for _, sp := range []string{beta, gamma} {
		expect(t, e.root.do("PUT", "/spaces/"+sp+"/human-type-permissions/approval", map[string]any{"view_role": "admin", "respond_role": "admin"}), 200)
	}
	for sp, n := range map[string]int{beta: 1, gamma: 2} {
		for i := range n {
			expect(t, e.root.do("POST", "/spaces/"+sp+"/human-actions", map[string]any{"type": "approval", "title": "Restricted approval " + strconv.Itoa(i),
				"request": map[string]any{"action": "deploy"}}), 201)
		}
	}
	e.h.clk.Advance(2 * notify.DefaultHomeEvery) // the summary is edited at most once per interval
	eventuallyFor(t, "the summary counting three items", 30*time.Second, func() bool {
		m := c.cardNow(t, home.MessageID)
		return strings.HasPrefix(m.Text, "Needs you — 3") && strings.Contains(m.Text, "SSO space — 2") && strings.Contains(m.Text, "Beta space — 1") && !strings.Contains(m.Text, "Gamma")
	})
	if n := c.count("Needs you"); n != 1 {
		t.Errorf("%d summaries in the chat, want the one edited in place", n)
	}
	if p := e.tg.Pinned(42); len(p) != 1 {
		t.Errorf("pins %v, want only the summary", p)
	}
}
