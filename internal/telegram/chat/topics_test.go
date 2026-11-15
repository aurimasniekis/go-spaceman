package chat

import (
	"context"
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
)

func (e *env) sayIn(thread int, text string) {
	e.upd++
	e.h.Handle(context.Background(), botapi.Update{UpdateID: e.upd, Message: &botapi.Message{MessageID: int(e.upd), MessageThreadID: thread,
		From: &botapi.User{ID: chatID}, Chat: botapi.Chat{ID: chatID, Type: botapi.ChatPrivate}, Text: text}}, e.l)
}

// A reply goes to the topic the user wrote in; a message outside any topic is answered in the main chat.
func TestRepliesFollowTheTopic(t *testing.T) {
	e := newEnv(t)
	topic, err := e.makeTopic()
	if err != nil {
		t.Fatal(err)
	}
	e.sayIn(topic.MessageThreadID, "/spaces")
	if got := e.last().MessageThreadID; got != topic.MessageThreadID {
		t.Fatalf("reply in thread %d, want %d", got, topic.MessageThreadID)
	}
	e.say("/spaces")
	if got := e.last().MessageThreadID; got != 0 {
		t.Fatalf("reply in thread %d, want the main chat", got)
	}
}

// The topic was deleted after the user wrote in it: the answer still arrives, in the main chat.
func TestReplyToADeletedTopicGoesToTheMainChat(t *testing.T) {
	e := newEnv(t)
	topic, err := e.makeTopic()
	if err != nil {
		t.Fatal(err)
	}
	e.srv.DeleteTopic(chatID, topic.MessageThreadID)
	e.sayIn(topic.MessageThreadID, "/spaces")
	if got := e.last(); got.MessageThreadID != 0 || got.Text == "" {
		t.Fatalf("reply %+v, want one in the main chat", got)
	}
}

func (e *env) makeTopic() (botapi.ForumTopic, error) {
	c, err := botapi.New(botapi.Options{BaseURL: e.srv.URL(), Token: e.srv.Token()})
	if err != nil {
		return botapi.ForumTopic{}, err
	}
	return c.CreateForumTopic(context.Background(), botapi.CreateForumTopicParams{ChatID: chatID, Name: "Alpha"})
}

func (e *env) itemsIn(space string) int {
	e.api.mu.Lock()
	defer e.api.mu.Unlock()
	n := 0
	for _, it := range e.api.items {
		if it.SpaceID == space && strings.HasPrefix(it.Title, "topic note") {
			n++
		}
	}
	return n
}

// H1: a message written in a space's topic acts on that space, not on the link's current space.
func TestTopicPinsTheTargetSpace(t *testing.T) {
	e := newEnv(t)
	e.threadSpace = map[int]string{7: spA}
	e.say("/space " + spS) // the current space is another one
	e.sayIn(7, "topic note one")
	if e.itemsIn(spA) != 1 || e.itemsIn(spS) != 0 {
		t.Fatalf("A has %d, current space has %d; want 1 and 0", e.itemsIn(spA), e.itemsIn(spS))
	}
	// Outside a known topic the current space still applies.
	e.sayIn(0, "topic note two")
	e.sayIn(99, "topic note three")
	if e.itemsIn(spS) != 2 || e.itemsIn(spA) != 1 {
		t.Fatalf("A has %d, current space has %d; want 1 and 2: %q", e.itemsIn(spA), e.itemsIn(spS), e.texts())
	}
}

// H1: when the topic's space is no longer allowed, the write is refused and does not fall back to the current space.
func TestTopicOfARevokedSpaceIsRefused(t *testing.T) {
	e := newEnv(t)
	e.threadSpace = map[int]string{7: spA}
	e.say("/space " + spS)
	e.hideA.Store(true) // the grant is gone: A is no longer visible
	e.sayIn(7, "topic note one")
	if e.itemsIn(spA) != 0 || e.itemsIn(spS) != 0 || e.createdN() != 0 {
		t.Fatalf("items landed: A %d, current %d", e.itemsIn(spA), e.itemsIn(spS))
	}
	if got := e.last(); got.Text != TextUnavailable {
		t.Fatalf("answer %q, want %q", got.Text, TextUnavailable)
	}
}

// H1: a command without an argument also works on the topic's space.
func TestTopicPinsCommandsWithoutAnArgument(t *testing.T) {
	e := newEnv(t)
	e.threadSpace = map[int]string{7: spA}
	e.say("/space " + spS)
	e.sayIn(7, "/inbox")
	if got := e.last().Text; !strings.HasPrefix(got, "Inbox of Space aa") {
		t.Fatalf("/inbox in A's topic answered %q", got)
	}
}

// A space renamed to another space's ID must not take over that space's topic: the pin resolves by ID only.
func TestTopicPinIgnoresSpacesNamedLikeItsID(t *testing.T) {
	e := newEnv(t)
	e.threadSpace = map[int]string{7: spA}
	e.api.listFirst = []spaceRef{{ID: spViewer, Slug: "x", Name: strings.ToUpper(spA), State: "active"}}
	e.say("/space " + spS)
	e.sayIn(7, "topic note one")
	if e.itemsIn(spA) != 1 || e.itemsIn(spViewer) != 0 || e.itemsIn(spS) != 0 {
		t.Fatalf("A %d, impostor %d, current %d; want 1, 0, 0", e.itemsIn(spA), e.itemsIn(spViewer), e.itemsIn(spS))
	}
}

// Control mode of S does not swallow text written in A's topic (it is captured into A), and /control there targets A.
func TestTopicPinAgainstControlMode(t *testing.T) {
	e := newEnv(t)
	e.threadSpace = map[int]string{7: spA}
	e.say("/control " + spS)
	e.sayIn(7, "topic note one")
	if e.itemsIn(spA) != 1 || e.itemsIn(spS) != 0 {
		t.Fatalf("A %d, S %d; want 1, 0: %q", e.itemsIn(spA), e.itemsIn(spS), e.texts())
	}
	e.sayIn(7, "/control")
	if got := e.last().Text; !strings.Contains(got, "Control mode for Space aa") {
		t.Fatalf("/control in A's topic answered %q", got)
	}
}

// An explicit /space sent from a topic still moves the current space. (Mutant: cmdSpace calling setCurrent(ctx,…) fails.)
func TestExplicitSpaceFromATopicMovesCurrent(t *testing.T) {
	e := newEnv(t)
	e.threadSpace = map[int]string{7: spS}
	e.say("/space " + spS)
	e.sayIn(7, "/space alpha")
	e.sayIn(0, "topic note one")
	if e.itemsIn(spA) != 1 || e.itemsIn(spS) != 0 {
		t.Fatalf("A %d, S %d; want 1, 0: %q", e.itemsIn(spA), e.itemsIn(spS), e.texts())
	}
}
