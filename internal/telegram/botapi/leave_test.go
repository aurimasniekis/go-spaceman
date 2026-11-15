package botapi_test

import (
	"context"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
)

// LeaveChat leaves a group; the fake refuses it for a private chat, as Telegram does.
func TestLeaveChat(t *testing.T) {
	srv, c := newPair(t, noLimits())
	ctx := context.Background()
	if err := c.LeaveChat(ctx, -100123); err != nil {
		t.Fatal(err)
	}
	apiErr(t, c.LeaveChat(ctx, 42), 400, "private chats")
	if calls := srv.Calls("leaveChat"); len(calls) != 2 || calls[0].Params["chat_id"] != "-100123" {
		t.Fatalf("calls %+v", calls)
	}
}

// GetChat finds only a chat the bot is in (one registered with SetChat), like Telegram, and reports its type and forum
// mode: group mode binds only forum supergroups.
func TestGetChat(t *testing.T) {
	srv, c := newPair(t, noLimits())
	ctx := context.Background()
	srv.SetChat(botapi.Chat{ID: -100200, Type: botapi.ChatSupergroup, Title: "Team", IsForum: true})
	ch, err := c.GetChat(ctx, -100200)
	if err != nil {
		t.Fatal(err)
	}
	if ch.ID != -100200 || ch.Type != botapi.ChatSupergroup || !ch.IsForum || ch.Title != "Team" {
		t.Fatalf("chat %+v", ch)
	}
	apiErr(t, func() error { _, err := c.GetChat(ctx, -100999); return err }(), 400, "chat not found")
}
