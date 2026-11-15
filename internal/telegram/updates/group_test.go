package updates

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
)

const boundGroup, otherGroup = -100500, -100600

type groupsFake map[int64]string

func (g groupsFake) GroupSpace(chatID int64) (string, bool) {
	s, ok := g[chatID]
	return s, ok
}

// linkRecorder records the link each handled update was handed over with.
type linkRecorder struct {
	mu    sync.Mutex
	links []Link
}

func (r *linkRecorder) Handle(_ context.Context, _ botapi.Update, l Link) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.links = append(r.links, l)
}

func (r *linkRecorder) got() []Link {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Link(nil), r.links...)
}

func groupMsg(id, from int64, chat int64, text string) botapi.Update {
	return botapi.Update{UpdateID: id, Message: &botapi.Message{MessageID: int(id), From: &botapi.User{ID: from, FirstName: "U"},
		Chat: botapi.Chat{ID: chat, Type: botapi.ChatSupergroup}, Text: text}}
}

func groupTap(id, from int64, chat int64) botapi.Update {
	return botapi.Update{UpdateID: id, CallbackQuery: &botapi.CallbackQuery{ID: "cb" + string(rune('a'+id)), From: botapi.User{ID: from},
		Message: &botapi.Message{MessageID: 77, Chat: botapi.Chat{ID: chat, Type: botapi.ChatSupergroup}}, Data: "ho"}}
}

// SM-TG-002 (TG-13-1): in a bound group every tap and command is handed over as the TAPPING person's own link, never
// the binder's or anyone else's; the reply chat is the group and the only space is the bound one.
func TestDispatcherGroupUsesTheTappersOwnLink(t *testing.T) {
	_, bot := fakeBot(t)
	rec := &linkRecorder{}
	links := &recorder{linked: map[int64]Link{
		7: {ID: "tgl_binder", UserID: "usr_binder", ChatID: 7},
		8: {ID: "tgl_tapper", UserID: "usr_tapper", ChatID: 8},
	}}
	d := NewDispatcher(DispatcherOptions{Links: links, Handler: rec, Bot: bot, Groups: groupsFake{boundGroup: "spc_bound"}})
	ctx := context.Background()
	d.Dispatch(ctx, groupTap(1, 8, boundGroup))
	d.Dispatch(ctx, groupMsg(2, 8, boundGroup, "/needsyou"))
	got := rec.got()
	if len(got) != 2 {
		t.Fatalf("handled %d updates", len(got))
	}
	for _, l := range got {
		if l.ID != "tgl_tapper" || l.UserID != "usr_tapper" || l.ChatID != boundGroup || l.Group == nil ||
			*l.Group != (Group{ChatID: boundGroup, SpaceID: "spc_bound"}) {
			t.Fatalf("handed over %+v (group %+v)", l, l.Group)
		}
	}
	// The private chat of the same person still gets a plain link (no group).
	d.Dispatch(ctx, privateMsg(3, 8, "/needsyou"))
	if l := rec.got()[2]; l.Group != nil || l.ChatID != 8 {
		t.Fatalf("private update handed over as %+v", l)
	}
}

// SM-TG-002: a member without a link is refused on a tap (the tap's answer, seen only by them) and gets nothing for a
// message; the handler never runs, and the linking steps are never taken in a group.
func TestDispatcherGroupRefusesUnlinkedTapper(t *testing.T) {
	srv, bot := fakeBot(t)
	rec := &linkRecorder{}
	lk := &fakeLinker{}
	links := &recorder{linked: map[int64]Link{7: {ID: "tgl_binder", UserID: "usr_binder", ChatID: 7}}}
	d := NewDispatcher(DispatcherOptions{Links: links, Handler: rec, Linker: lk, Bot: bot, Groups: groupsFake{boundGroup: "spc_bound"}})
	ctx := context.Background()
	tap := groupTap(1, 9, boundGroup)
	srv.PushUpdate(tap)
	d.Dispatch(ctx, tap)
	if a := srv.CallbackAnswers(); len(a) != 1 || a[0].Text != GroupUnlinkedText || !a[0].ShowAlert {
		t.Fatalf("answers %+v", a)
	}
	d.Dispatch(ctx, groupMsg(2, 9, boundGroup, "/needsyou"))
	d.Dispatch(ctx, groupMsg(3, 9, boundGroup, "/start abcdef"))
	if len(rec.got()) != 0 || len(lk.calls) != 0 {
		t.Fatalf("handled %v, linker %v", rec.got(), lk.calls)
	}
	if n := len(srv.Messages(boundGroup)) + len(srv.Messages(9)); n != 0 {
		t.Fatalf("sent %d messages for an unlinked member", n)
	}
}

// A link overdue for its web re-confirmation acts for nobody in a group either.
func TestDispatcherGroupRefusesOverdueLink(t *testing.T) {
	srv, bot := fakeBot(t)
	clk := clock.NewFake(time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC))
	rec := &linkRecorder{}
	links := &recorder{linked: map[int64]Link{8: {ID: "tgl_tapper", UserID: "usr_tapper", ChatID: 8, ReconfirmDueAt: clk.Now()}}}
	d := NewDispatcher(DispatcherOptions{Links: links, Handler: rec, Bot: bot, Clock: clk, Groups: groupsFake{boundGroup: "spc_bound"}})
	tap := groupTap(1, 8, boundGroup)
	srv.PushUpdate(tap)
	d.Dispatch(context.Background(), tap)
	if len(rec.got()) != 0 {
		t.Fatal("an overdue link acted in a group")
	}
	if a := srv.CallbackAnswers(); len(a) != 1 || a[0].Text != GroupUnlinkedText || !a[0].ShowAlert {
		t.Fatalf("answers %+v", a)
	}
}

// Only a bound supergroup counts, only commands, replies and taps from a person are taken, and the bot leaves a group
// that isn't bound but stays in a bound one.
func TestDispatcherGroupScope(t *testing.T) {
	srv, bot := fakeBot(t)
	rec := &linkRecorder{}
	links := &recorder{linked: map[int64]Link{8: {ID: "tgl_tapper", UserID: "usr_tapper", ChatID: 8}}}
	d := NewDispatcher(DispatcherOptions{Links: links, Handler: rec, Bot: bot, Groups: groupsFake{boundGroup: "spc_bound", -42: "spc_plain"}})
	ctx := context.Background()

	chatter := groupMsg(1, 8, boundGroup, "hello all")
	unbound := groupMsg(2, 8, otherGroup, "/needsyou")
	plainGroup := groupMsg(3, 8, -42, "/needsyou")
	plainGroup.Message.Chat.Type = botapi.ChatGroup
	anon := groupMsg(4, 1087968824, boundGroup, "/needsyou") // an anonymous admin posts as a bot account
	anon.Message.From.IsBot = true
	for _, u := range []botapi.Update{chatter, unbound, plainGroup, anon} {
		d.Dispatch(ctx, u)
	}
	if len(rec.got()) != 0 || links.lookups != 0 {
		t.Fatalf("handled %v, %d link lookups", rec.got(), links.lookups)
	}
	// Activity in chats that can't be bound (no topics, a basic group) makes the bot leave them; the bound group isn't.
	if calls := srv.Calls("leaveChat"); len(calls) != 2 || calls[0].Params["chat_id"] != "-100600" || calls[1].Params["chat_id"] != "-42" {
		t.Fatalf("leaveChat calls after chatter %+v", calls)
	}
	reply := groupMsg(5, 8, boundGroup, "the reason")
	reply.Message.ReplyToMessage = &botapi.Message{MessageID: 3, Chat: botapi.Chat{ID: boundGroup, Type: botapi.ChatSupergroup}}
	d.Dispatch(ctx, reply)
	if len(rec.got()) != 1 {
		t.Fatal("a reply in the bound group wasn't handed over")
	}

	member := func(id int64, chat int64) botapi.Update {
		return botapi.Update{UpdateID: id, MyChatMember: &botapi.ChatMemberUpdated{Chat: botapi.Chat{ID: chat, Type: botapi.ChatSupergroup},
			From: botapi.User{ID: 8}, NewChatMember: botapi.ChatMember{Status: botapi.MemberAdministrator}}}
	}
	d.Dispatch(ctx, member(6, boundGroup))
	d.Dispatch(ctx, member(7, otherGroup))
	if calls := srv.Calls("leaveChat"); len(calls) != 3 || calls[2].Params["chat_id"] != "-100600" {
		t.Fatalf("leaveChat calls %+v", calls)
	}
}

// TG-13-2: binding checks that the bot is a member, so the bot stays in a forum supergroup that isn't bound yet (and
// reads nothing there), but leaves a chat that can never be bound: a channel, a basic group, a supergroup without
// topics. It also leaves a group that lost its binding (LeaveGroup), and never a private chat.
func TestDispatcherGroupLeavePolicy(t *testing.T) {
	srv, bot := fakeBot(t)
	d := NewDispatcher(DispatcherOptions{Links: &recorder{}, Handler: &linkRecorder{}, Bot: bot, Groups: groupsFake{boundGroup: "spc_bound"}})
	ctx := context.Background()
	member := func(id int64, chat botapi.Chat, status string) {
		d.Dispatch(ctx, botapi.Update{UpdateID: id, MyChatMember: &botapi.ChatMemberUpdated{Chat: chat, From: botapi.User{ID: 8},
			NewChatMember: botapi.ChatMember{Status: status}}})
	}
	member(1, botapi.Chat{ID: -100801, Type: botapi.ChatSupergroup, IsForum: true}, botapi.MemberAdministrator) // waits for a binding
	member(2, botapi.Chat{ID: -100802, Type: botapi.ChatSupergroup}, botapi.MemberMember)                       // no topics
	member(3, botapi.Chat{ID: -100803, Type: botapi.ChatGroup}, botapi.MemberMember)
	member(4, botapi.Chat{ID: -100804, Type: "channel"}, botapi.MemberAdministrator)
	var left []string
	for _, c := range srv.Calls("leaveChat") {
		left = append(left, c.Params["chat_id"])
	}
	if strings.Join(left, ",") != "-100802,-100803,-100804" {
		t.Fatalf("left %v", left)
	}
	d.LeaveGroup(ctx, -100801)
	d.LeaveGroup(ctx, 42) // never a private chat
	d.LeaveGroup(ctx, 0)
	calls := srv.Calls("leaveChat")
	if len(calls) != 4 || calls[3].Params["chat_id"] != "-100801" {
		t.Fatalf("leaveChat calls %+v", calls)
	}
}

// TG-13-2 (reviewed lifecycle): the bot waits in an unbound forum supergroup for at most the grace, then leaves unless
// the group got bound. After a restart it has forgotten the wait, so the first message or tap from an unbound group
// makes it leave at once. Nothing from such a group is ever handed to the handler.
func TestDispatcherUnboundForumGroupWaitIsBounded(t *testing.T) {
	srv, bot := fakeBot(t)
	clk := clock.NewFake(time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC))
	rec := &linkRecorder{}
	groups := groupsFake{}
	d := NewDispatcher(DispatcherOptions{Links: &recorder{linked: map[int64]Link{8: {ID: "tgl_8", UserID: "usr_8", ChatID: 8}}}, Handler: rec, Bot: bot,
		Groups: groups, Clock: clk, GroupGrace: 15 * time.Minute})
	ctx := context.Background()
	forum := func(id int64) botapi.Chat { return botapi.Chat{ID: id, Type: botapi.ChatSupergroup, IsForum: true} }
	added := func(n, id int64) {
		d.Dispatch(ctx, botapi.Update{UpdateID: n, MyChatMember: &botapi.ChatMemberUpdated{Chat: forum(id), From: botapi.User{ID: 8},
			NewChatMember: botapi.ChatMember{Status: botapi.MemberAdministrator}}})
	}
	left := func() int { return len(srv.Calls("leaveChat")) }

	added(1, -100901) // never bound
	added(2, -100902) // bound within the grace
	added(3, -100903) // bound, then loses it
	if left() != 0 {
		t.Fatal("the bot left a forum supergroup at once")
	}
	// Chatter and a command from a group it waits in: nothing is read or handed over, and it stays.
	chatter := groupMsg(4, 8, -100901, "/needsyou")
	chatter.Message.Chat = forum(-100901)
	d.Dispatch(ctx, chatter)
	if len(rec.got()) != 0 || left() != 0 {
		t.Fatalf("handled %v, left %d", rec.got(), left())
	}
	clk.Advance(14 * time.Minute)
	groups[-100902] = "spc_x" // bound in time
	d.SweepGroups(ctx)
	if left() != 0 {
		t.Fatal("left before the grace ended")
	}
	clk.Advance(2 * time.Minute)
	d.SweepGroups(ctx)
	calls := srv.Calls("leaveChat")
	if len(calls) != 2 || calls[0].Params["chat_id"] == "-100902" || calls[1].Params["chat_id"] == "-100902" {
		t.Fatalf("after the grace: %+v", calls)
	}
	// A restart: a new dispatcher knows no wait, so the first activity of an unbound forum group makes it leave.
	d2 := NewDispatcher(DispatcherOptions{Links: &recorder{}, Handler: rec, Bot: bot, Groups: groupsFake{}, Clock: clk})
	forgotten := groupMsg(5, 8, -100904, "/needsyou")
	forgotten.Message.Chat = forum(-100904)
	d2.Dispatch(ctx, forgotten)
	calls = srv.Calls("leaveChat")
	if len(calls) != 3 || calls[2].Params["chat_id"] != "-100904" || len(rec.got()) != 0 {
		t.Fatalf("after a restart: %+v handled %v", calls, rec.got())
	}
	// The same activity in the group the bot waits in (a tap too) is dropped without leaving.
	d.Dispatch(ctx, botapi.Update{UpdateID: 6, MyChatMember: &botapi.ChatMemberUpdated{Chat: forum(-100905), From: botapi.User{ID: 8},
		NewChatMember: botapi.ChatMember{Status: botapi.MemberMember}}})
	tap := botapi.Update{UpdateID: 7, CallbackQuery: &botapi.CallbackQuery{ID: "q", From: botapi.User{ID: 8}, Data: "x",
		Message: &botapi.Message{MessageID: 1, Chat: forum(-100905)}}}
	d.Dispatch(ctx, tap)
	if n := len(srv.Calls("leaveChat")); n != 3 || len(rec.got()) != 0 {
		t.Fatalf("a tap in a waiting group: %d leaves, handled %v", n, rec.got())
	}
}
