package app_test

import (
	"context"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/settings"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/tgtest"
)

// SM-TG-002 (TG-13-2, group mode): a forum supergroup is bound to a space with put-space-telegram's group_chat_id. Binding
// and unbinding need telegram_group:write (admins) on top of space_config:write; the bot must be a member of a
// supergroup with topics (checked with getChat against the fake); a group serves one space (409 elsewhere); a write that
// leaves the field out keeps the binding; 0 unbinds.
func TestTelegramGroupBindOverHTTP(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	actor := audit.Actor{Type: audit.ActorUser, ID: "usr_test"}
	en, _ := h.c.Fleet.CreateNode(ctx, actor, fleet.CreateNodeInput{Name: "node-a"})
	a, err := h.c.Fleet.CreateSpace(ctx, actor, fleet.CreateSpaceInput{Slug: "alpha", Name: "Alpha", NodeID: en.Node.ID})
	if err != nil {
		t.Fatal(err)
	}
	b, err := h.c.Fleet.CreateSpace(ctx, actor, fleet.CreateSpaceInput{Slug: "bravo", Name: "Bravo", NodeID: en.Node.ID})
	if err != nil {
		t.Fatal(err)
	}
	user := func(email string, grants ...auth.GrantInput) *client {
		c := h.client()
		c.bearer = userToken(t, h.c.Auth, email, grants)
		return c
	}
	adminA := user("ada@example.com", auth.GrantInput{Role: authz.RoleAdmin, Domain: a.ID})
	adminB := user("bob@example.com", auth.GrantInput{Role: authz.RoleAdmin, Domain: b.ID})
	operatorA := user("opal@example.com", auth.GrantInput{Role: authz.RoleOperator, Domain: a.ID})
	pathA, pathB := "/spaces/"+a.ID+"/telegram", "/spaces/"+b.ID+"/telegram"

	const forum, forum2, plain, basic, missing = int64(-1001001), int64(-1001002), int64(-1001003), int64(-1001004), int64(-1001005)

	// The bot isn't running: the group can't be checked, so nothing is bound.
	r := adminA.do("PUT", pathA, map[string]any{"enabled": true, "group_chat_id": forum})
	expect(t, r, 503)

	tg := tgtest.New(t, tgtest.Options{Limits: tgtest.Limits{Disabled: true}})
	bot, err := botapi.New(botapi.Options{BaseURL: tg.URL(), Token: tg.Token()})
	if err != nil {
		t.Fatal(err)
	}
	h.c.TelegramUpdates.Dispatcher.SetBot(bot)
	tg.SetChat(botapi.Chat{ID: forum, Type: botapi.ChatSupergroup, Title: "Team", IsForum: true})
	tg.SetChat(botapi.Chat{ID: forum2, Type: botapi.ChatSupergroup, Title: "Other", IsForum: true})
	tg.SetChat(botapi.Chat{ID: plain, Type: botapi.ChatSupergroup, Title: "No topics"})
	tg.SetChat(botapi.Chat{ID: basic, Type: "group", Title: "Basic group"})

	// Only admins bind; an operator can still write the space's other settings.
	expect(t, operatorA.do("PUT", pathA, map[string]any{"enabled": true, "group_chat_id": forum}), 403)
	expect(t, operatorA.do("PUT", pathA, map[string]any{"enabled": true}), 200)
	if g, ok := h.c.TelegramSettings.GroupSpace(forum); ok {
		t.Fatalf("a refused bind took effect: %q", g)
	}

	// Refused groups: not a supergroup, no topics, the bot isn't in it, not a group ID.
	for name, g := range map[string]int64{"no topics": plain, "basic group": basic, "bot not a member": missing, "private chat": 42} {
		if r := adminA.do("PUT", pathA, map[string]any{"enabled": true, "group_chat_id": g}, "If-Match", `"1"`); r.status != 422 {
			t.Errorf("%s: %d %s", name, r.status, r.body)
		}
	}

	// Bound: the response and the cache say so, and the group acts for the space at once.
	r = adminA.do("PUT", pathA, map[string]any{"enabled": true, "group_chat_id": forum}, "If-Match", `"1"`)
	expect(t, r, 200)
	var sp settings.Space
	r.json(t, &sp)
	if sp.GroupChatID != forum {
		t.Fatalf("bound %+v", sp)
	}
	if got, ok := h.c.TelegramSettings.GroupSpace(forum); !ok || got != a.ID {
		t.Fatalf("GroupSpace = %q, %v", got, ok)
	}
	r = adminA.do("GET", pathA, nil)
	r.json(t, &sp)
	if sp.GroupChatID != forum {
		t.Fatalf("get %+v", sp)
	}

	// One group serves one space: another space can't take it (409), and its own settings stay unbound.
	expect(t, adminB.do("PUT", pathB, map[string]any{"enabled": true, "group_chat_id": forum}), 409)
	expect(t, adminB.do("PUT", pathB, map[string]any{"enabled": true}), 200)
	if g, _ := h.c.TelegramSettings.GetSpace(ctx, b.ID); g.GroupChatID != 0 {
		t.Fatalf("the refused space got a group: %+v", g)
	}
	expect(t, adminB.do("PUT", pathB, map[string]any{"enabled": true, "group_chat_id": forum}, "If-Match", `"1"`), 409)

	// A write that leaves the field out keeps the binding; repeating it needs no admin and no getChat.
	r = operatorA.do("PUT", pathA, map[string]any{"enabled": true, "content_level": "summary"}, "If-Match", `"2"`)
	expect(t, r, 200)
	r.json(t, &sp)
	if sp.GroupChatID != forum || sp.ContentLevel != "summary" {
		t.Fatalf("a write without the field %+v", sp)
	}
	tg.SetChat(botapi.Chat{ID: forum, Type: botapi.ChatSupergroup, IsForum: false}) // topics turned off later
	expect(t, operatorA.do("PUT", pathA, map[string]any{"enabled": true, "group_chat_id": forum}, "If-Match", `"3"`), 200)
	tg.SetChat(botapi.Chat{ID: forum, Type: botapi.ChatSupergroup, Title: "Team", IsForum: true})

	// Rebinding to another group, and unbinding, are admin-only too.
	expect(t, operatorA.do("PUT", pathA, map[string]any{"enabled": true, "group_chat_id": 0}, "If-Match", `"4"`), 403)
	r = adminA.do("PUT", pathA, map[string]any{"enabled": true, "group_chat_id": forum2}, "If-Match", `"4"`)
	expect(t, r, 200)
	if _, ok := h.c.TelegramSettings.GroupSpace(forum); ok {
		t.Fatal("the old group still acts for the space")
	}
	if got, ok := h.c.TelegramSettings.GroupSpace(forum2); !ok || got != a.ID {
		t.Fatalf("GroupSpace(new) = %q, %v", got, ok)
	}
	r = adminA.do("PUT", pathA, map[string]any{"enabled": true, "group_chat_id": 0}, "If-Match", `"5"`)
	expect(t, r, 200)
	sp = settings.Space{} // group_chat_id is omitted when none
	r.json(t, &sp)
	if sp.GroupChatID != 0 {
		t.Fatalf("unbound %+v", sp)
	}
	if _, ok := h.c.TelegramSettings.GroupSpace(forum2); ok {
		t.Fatal("an unbound group still acts for the space")
	}

	// The freed group can be bound to the other space.
	expect(t, adminB.do("PUT", pathB, map[string]any{"enabled": true, "group_chat_id": forum}, "If-Match", `"1"`), 200)
	if got, ok := h.c.TelegramSettings.GroupSpace(forum); !ok || got != b.ID {
		t.Fatalf("GroupSpace after the move = %q, %v", got, ok)
	}
	// Deleting the space's settings ends the binding with them.
	expect(t, adminB.do("DELETE", pathB, nil, "If-Match", `"2"`), 204)
	if _, ok := h.c.TelegramSettings.GroupSpace(forum); ok {
		t.Fatal("a deleted space's group still acts")
	}
}
