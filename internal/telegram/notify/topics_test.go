package notify_test

import (
	"context"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/notify"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/topics"
)

func topicsEnv(t *testing.T) *env {
	t.Helper()
	e := newEnv(t, 1)
	e.pol.topics = true
	e.tp = topics.New(topics.Options{
		Enabled:   func(id string) bool { sp := e.pol.Space(id); return sp.Topics && sp.Visible() },
		SpaceName: func(context.Context, string) string { return "Alpha" },
		Now:       e.clk.Now,
	})
	return e
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

func TestTopicsRouteCards(t *testing.T) {
	e := topicsEnv(t)
	stop := e.run(e.maps)
	defer stop()

	e.emit(action("hum_1", 1, false), changefeed.Created)
	e.emit(message("msg_1"), changefeed.Created)
	e.waitCalls("sendMessage", 2)
	topicsMade := e.fake.Topics(1001)
	if len(topicsMade) != 2 {
		t.Fatalf("topics %v, want Needs you and the space's", topicsMade)
	}
	needs, space := e.threadOf(1001, 0), e.threadOf(1001, 1)
	if topicsMade[needs] != "Needs you" || topicsMade[space] != "Alpha" {
		t.Errorf("action thread %d, message thread %d, topics %v", needs, space, topicsMade)
	}

	// A second action reuses the topic; resolving the first edits its card in place.
	e.emit(action("hum_2", 1, false), changefeed.Created)
	e.waitCalls("sendMessage", 3)
	if e.threadOf(1001, 2) != needs || len(e.fake.Calls("createForumTopic")) != 2 {
		t.Errorf("second action thread %d, %d topics created", e.threadOf(1001, 2), len(e.fake.Calls("createForumTopic")))
	}
	e.emit(action("hum_1", 2, true), changefeed.Updated)
	e.waitCalls("editMessageText", 1)
}

func TestTopicsOffForTheSpace(t *testing.T) {
	e := topicsEnv(t)
	e.pol.topics = false
	stop := e.run(e.maps)
	defer stop()

	e.emit(action("hum_1", 1, false), changefeed.Created)
	e.waitCalls("sendMessage", 1)
	if e.threadOf(1001, 0) != 0 || len(e.fake.Calls("createForumTopic")) != 0 {
		t.Errorf("a space without topics mode must use the main chat")
	}
}

func TestTopicDeletedIsRecreated(t *testing.T) {
	e := topicsEnv(t)
	stop := e.run(e.maps)
	defer stop()

	e.emit(action("hum_1", 1, false), changefeed.Created)
	e.waitCalls("sendMessage", 1)
	old := e.threadOf(1001, 0)
	e.fake.DeleteTopic(1001, old)
	e.emit(action("hum_2", 1, false), changefeed.Created)
	e.waitCalls("createForumTopic", 2)
	e.waitCalls("sendMessage", 3) // the failed send, then the one in the new topic
	msgs := e.waitMessages(1001, 2)
	if got := msgs[len(msgs)-1].MessageThreadID; got == 0 || got == old {
		t.Errorf("thread %d, want a new topic (old %d)", got, old)
	}
}

func TestTopicClosedFallsBackToTheMainChat(t *testing.T) {
	e := topicsEnv(t)
	stop := e.run(e.maps)
	defer stop()

	e.emit(action("hum_1", 1, false), changefeed.Created)
	e.waitCalls("sendMessage", 1)
	e.fake.CloseTopic(1001, e.threadOf(1001, 0))
	e.fake.DisableTopics(1001) // it can't be recreated either
	e.emit(action("hum_2", 1, false), changefeed.Created)
	e.waitCalls("sendMessage", 3)
	msgs := e.waitMessages(1001, 2)
	if last := msgs[len(msgs)-1]; last.MessageThreadID != 0 {
		t.Errorf("thread %d, want the main chat", last.MessageThreadID)
	}
}

func TestTopicsRefusedUsesTheMainChat(t *testing.T) {
	e := topicsEnv(t)
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
