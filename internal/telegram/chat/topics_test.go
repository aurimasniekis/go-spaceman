package chat

import (
	"context"
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
