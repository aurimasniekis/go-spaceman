package botapi_test

import (
	"context"
	"testing"
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
