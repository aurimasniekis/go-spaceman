package updates

import (
	"context"
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
	if a := srv.CallbackAnswers(); len(a) != 1 || a[0].Text != GroupUnlinkedText {
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
	if a := srv.CallbackAnswers(); len(a) != 1 || a[0].Text != GroupUnlinkedText {
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
	if calls := srv.Calls("leaveChat"); len(calls) != 1 || calls[0].Params["chat_id"] != "-100600" {
		t.Fatalf("leaveChat calls %+v", calls)
	}
}
