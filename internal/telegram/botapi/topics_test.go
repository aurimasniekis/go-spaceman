package botapi_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
)

// newTopic creates a topic in the user's private chat and returns its thread ID.
func newTopic(t *testing.T, c *botapi.Client, name string) int {
	t.Helper()
	ft, err := c.CreateForumTopic(context.Background(), botapi.CreateForumTopicParams{ChatID: userChat, Name: name})
	if err != nil {
		t.Fatal(err)
	}
	return ft.MessageThreadID
}

// GetMe reports the two topic flags the fake was told to report (BotFather's settings); they are returned only by getMe.
func TestGetMeTopicFlags(t *testing.T) {
	srv, c := newPair(t, noLimits())
	ctx := context.Background()
	me, err := c.GetMe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if me.HasTopicsEnabled || me.AllowsUsersToCreateTopics {
		t.Fatalf("defaults %+v, want both off", me)
	}
	srv.SetBotTopics(true, false)
	me, err = c.GetMe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !me.HasTopicsEnabled || me.AllowsUsersToCreateTopics {
		t.Fatalf("after SetBotTopics(true, false): %+v", me)
	}
	srv.SetBotTopics(true, true)
	if me, _ = c.GetMe(ctx); !me.HasTopicsEnabled || !me.AllowsUsersToCreateTopics {
		t.Fatalf("after SetBotTopics(true, true): %+v", me)
	}
}

// EditForumTopic renames a topic: 0–128 characters, an empty name keeps the current one; a topic that was deleted or
// never existed can't be edited.
func TestEditForumTopic(t *testing.T) {
	srv, c := newPair(t, noLimits())
	ctx := context.Background()
	thread := newTopic(t, c, "Alpha")
	if err := c.EditForumTopic(ctx, botapi.EditForumTopicParams{ChatID: userChat, MessageThreadID: thread, Name: "Beta"}); err != nil {
		t.Fatal(err)
	}
	if got := srv.Topics(userChat)[thread]; got != "Beta" {
		t.Fatalf("name %q, want Beta", got)
	}
	if err := c.EditForumTopic(ctx, botapi.EditForumTopicParams{ChatID: userChat, MessageThreadID: thread}); err != nil {
		t.Fatal(err)
	}
	if got := srv.Topics(userChat)[thread]; got != "Beta" {
		t.Fatalf("empty name changed the topic to %q", got)
	}
	apiErr(t, c.EditForumTopic(ctx, botapi.EditForumTopicParams{ChatID: userChat, MessageThreadID: thread, Name: strings.Repeat("x", 129)}), 400, "topic name is invalid")
	apiErr(t, c.EditForumTopic(ctx, botapi.EditForumTopicParams{ChatID: userChat, MessageThreadID: 999, Name: "x"}), 400, "message thread not found")
	srv.DeleteTopic(userChat, thread)
	apiErr(t, c.EditForumTopic(ctx, botapi.EditForumTopicParams{ChatID: userChat, MessageThreadID: thread, Name: "x"}), 400, "message thread not found")
}

// Messages land in the thread they are sent to: MessagesIn separates the chat without a topic from each topic.
func TestMessagesInRecordsThread(t *testing.T) {
	srv, c := newPair(t, noLimits())
	ctx := context.Background()
	thread := newTopic(t, c, "Space")
	send := func(th int, text string) botapi.Message {
		m, err := c.SendMessage(ctx, botapi.SendMessageParams{ChatID: userChat, MessageThreadID: th, Text: text})
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	a := send(thread, "in topic")
	b := send(0, "no topic")
	send(thread, "in topic again")
	if got := srv.MessagesIn(userChat, thread); len(got) != 2 || got[0].MessageID != a.MessageID {
		t.Fatalf("MessagesIn(topic) = %+v", got)
	}
	if got := srv.MessagesIn(userChat, 0); len(got) != 1 || got[0].MessageID != b.MessageID {
		t.Fatalf("MessagesIn(0) = %+v", got)
	}
}

// PinChatMessage pins any message in a private chat, UnpinChatMessage without an ID removes the most recent pin, and
// the pinned list follows deletes.
func TestPinAndUnpin(t *testing.T) {
	srv, c := newPair(t, noLimits())
	ctx := context.Background()
	m1, _ := c.SendMessage(ctx, botapi.SendMessageParams{ChatID: userChat, Text: "one"})
	m2, _ := c.SendMessage(ctx, botapi.SendMessageParams{ChatID: userChat, Text: "two"})
	if err := c.PinChatMessage(ctx, botapi.PinChatMessageParams{ChatID: userChat, MessageID: m1.MessageID}); err != nil {
		t.Fatal(err)
	}
	if err := c.PinChatMessage(ctx, botapi.PinChatMessageParams{ChatID: userChat, MessageID: m2.MessageID, DisableNotification: true}); err != nil {
		t.Fatal(err)
	}
	if got := srv.Pinned(userChat); len(got) != 2 || got[0] != m1.MessageID || got[1] != m2.MessageID {
		t.Fatalf("pinned %v", got)
	}
	if calls := srv.Calls("pinChatMessage"); len(calls) != 2 || calls[1].Params["disable_notification"] != "true" {
		t.Fatalf("pin calls %+v", calls)
	}
	// Without a message_id the most recent pin goes first.
	if err := c.UnpinChatMessage(ctx, botapi.UnpinChatMessageParams{ChatID: userChat}); err != nil {
		t.Fatal(err)
	}
	if got := srv.Pinned(userChat); len(got) != 1 || got[0] != m1.MessageID {
		t.Fatalf("after unpin of the latest: %v", got)
	}
	if err := c.UnpinChatMessage(ctx, botapi.UnpinChatMessageParams{ChatID: userChat, MessageID: m1.MessageID}); err != nil {
		t.Fatal(err)
	}
	if got := srv.Pinned(userChat); len(got) != 0 {
		t.Fatalf("after unpin by ID: %v", got)
	}
	apiErr(t, c.PinChatMessage(ctx, botapi.PinChatMessageParams{ChatID: userChat, MessageID: 9999}), 400, "message to pin not found")

	// Deleting a pinned message removes its pin.
	if err := c.PinChatMessage(ctx, botapi.PinChatMessageParams{ChatID: userChat, MessageID: m2.MessageID}); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteMessage(ctx, userChat, m2.MessageID); err != nil {
		t.Fatal(err)
	}
	if got := srv.Pinned(userChat); len(got) != 0 {
		t.Fatalf("pin survived its message's delete: %v", got)
	}
}

// DeleteMessages deletes 1–100 messages at once and skips the ones it can't find.
func TestDeleteMessages(t *testing.T) {
	srv, c := newPair(t, noLimits())
	ctx := context.Background()
	var ids []int
	for i := range 3 {
		m, err := c.SendMessage(ctx, botapi.SendMessageParams{ChatID: userChat, Text: fmt.Sprintf("message %d", i)})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, m.MessageID)
	}
	apiErr(t, c.DeleteMessages(ctx, userChat, nil), 400, "message identifiers must be 1-100")
	big := make([]int, 101)
	apiErr(t, c.DeleteMessages(ctx, userChat, big), 400, "message identifiers must be 1-100")
	if err := c.DeleteMessages(ctx, userChat, []int{ids[0], ids[1], 4242}); err != nil {
		t.Fatal(err)
	}
	if got := srv.Messages(userChat); len(got) != 1 || got[0].MessageID != ids[2] {
		t.Fatalf("left %+v, want only message %d", got, ids[2])
	}
	want := fmt.Sprintf("[%d,%d,4242]", ids[0], ids[1])
	if calls := srv.Calls("deleteMessages"); len(calls) != 3 || calls[2].Params["message_ids"] != want {
		t.Fatalf("calls %+v, want message_ids %s", calls, want)
	}
}
