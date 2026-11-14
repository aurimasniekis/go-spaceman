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

// fakeLinker claims /start <payload> and taps on "tglink:" buttons, and answers with a fixed reply.
type fakeLinker struct {
	mu    sync.Mutex
	calls []int64
	reply LinkReply
}

func (f *fakeLinker) Claims(u *botapi.Update) bool {
	return u.Message != nil && strings.HasPrefix(u.Message.Text, "/start ") || u.CallbackQuery != nil && strings.HasPrefix(u.CallbackQuery.Data, "tglink:")
}

func (f *fakeLinker) HandleUnlinked(_ context.Context, u botapi.Update, _ botapi.User) LinkReply {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, u.UpdateID)
	return f.reply
}

// SM-TG-001: the dispatcher hands an unlinked user's linking steps (/start <code>, a link button) to the Linker and
// sends its reply: the tap's answer with the Linker's text, the keyboard removal, the message with its buttons. A
// plain message still gets only the linking instructions, a linked user's /start goes to the Handler, and a refusal
// counts against the unlinked-user reply bounds like the instructions do.
func TestDispatcherLinkSteps(t *testing.T) {
	srv, bot := fakeBot(t)
	rec := &recorder{linked: map[int64]Link{5: {ID: "tgl_1", UserID: "usr_1", ChatID: 5}}}
	kb := &botapi.InlineKeyboardMarkup{InlineKeyboard: [][]botapi.InlineKeyboardButton{{{Text: "Confirm", CallbackData: "tglink:c:x"}}}}
	lk := &fakeLinker{reply: LinkReply{Text: "prompt", Markup: kb, CallbackText: "ok", ClearKeyboard: true}}
	clk := clock.NewFake(time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC))
	d := NewDispatcher(DispatcherOptions{Links: rec, Handler: rec, Linker: lk, Bot: bot, Clock: clk})
	ctx := context.Background()

	d.Dispatch(ctx, privateMsg(1, 101, "/start abc"))
	msgs := srv.Messages(101)
	if len(msgs) != 1 || msgs[0].Text != "prompt" {
		t.Fatalf("messages %+v", msgs)
	}
	tap := botapi.Update{UpdateID: 2, CallbackQuery: &botapi.CallbackQuery{ID: "cb", From: botapi.User{ID: 101}, Message: &msgs[0], Data: "tglink:c:x"}}
	srv.PushUpdate(tap)
	d.Dispatch(ctx, tap)
	if a := srv.CallbackAnswers(); len(a) != 1 || a[0].Text != "ok" {
		t.Fatalf("answers %+v", a)
	}
	if n := len(srv.Calls("editMessageReplyMarkup")); n != 1 {
		t.Fatalf("keyboard removals %d", n)
	}
	d.Dispatch(ctx, privateMsg(3, 5, "/start abc")) // linked: the Handler's
	if got := rec.ids(); len(got) != 1 || got[0] != 3 {
		t.Fatalf("handled %v", got)
	}
	if len(lk.calls) != 2 {
		t.Fatalf("linker calls %v", lk.calls)
	}

	// Refusals are bounded: one reply per user within the hour.
	lk.reply = LinkReply{Text: "invalid", Bounded: true}
	for i := range 3 {
		clk.Advance(2 * time.Second) // past the global bound: only the per-user one holds
		d.Dispatch(ctx, privateMsg(int64(10+i), 202, "/start bad"))
	}
	if msgs := srv.Messages(202); len(msgs) != 1 || msgs[0].Text != "invalid" {
		t.Fatalf("bounded refusals %+v", msgs)
	}
	// A plain message from an unlinked user isn't a linking step.
	clk.Advance(2 * time.Second)
	d.Dispatch(ctx, privateMsg(20, 303, "hello"))
	if msgs := srv.Messages(303); len(msgs) != 1 || msgs[0].Text != UnlinkedText {
		t.Fatalf("plain message %+v", msgs)
	}
	if len(lk.calls) != 5 {
		t.Fatalf("linker calls %v", lk.calls)
	}
}

type fakeUnlinker struct {
	mu    sync.Mutex
	links []Link
}

func (f *fakeUnlinker) Unlink(_ context.Context, l Link) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.links = append(f.links, l)
	return "Unlinked."
}

// SM-TG-001 (TG-4b): /unlink from a linked user goes to the Unlinker, not the Handler (also addressed to the bot,
// /unlink@name); text that merely starts with it, from an unlinked user or with arguments, doesn't. A linked user's
// link that is past its re-confirmation time acts for nobody: the Handler and the Unlinker don't run and the user
// gets the re-confirmation instructions, at most once within the unlinked-user bounds.
func TestDispatcherUnlinkAndReconfirm(t *testing.T) {
	srv, bot := fakeBot(t)
	clk := clock.NewFake(time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC))
	due := clk.Now().Add(time.Hour)
	rec := &recorder{linked: map[int64]Link{5: {ID: "tgl_1", UserID: "usr_1", ChatID: 5, ReconfirmDueAt: due}}}
	ul := &fakeUnlinker{}
	d := NewDispatcher(DispatcherOptions{Links: rec, Handler: rec, Unlinker: ul, Bot: bot, Clock: clk})
	ctx := context.Background()

	d.Dispatch(ctx, privateMsg(1, 5, "/unlink"))
	d.Dispatch(ctx, privateMsg(2, 5, "/unlink@spaceman_bot"))
	d.Dispatch(ctx, privateMsg(3, 5, "/unlink now")) // arguments: not the command
	d.Dispatch(ctx, privateMsg(4, 5, "/unlinked"))
	if len(ul.links) != 2 || ul.links[0].ID != "tgl_1" || ul.links[0].UserID != "usr_1" {
		t.Fatalf("unlinks %+v", ul.links)
	}
	if got := rec.ids(); len(got) != 2 || got[0] != 3 || got[1] != 4 {
		t.Fatalf("handled %v", got)
	}
	if msgs := srv.Messages(5); len(msgs) != 2 || msgs[0].Text != "Unlinked." {
		t.Fatalf("replies %+v", msgs)
	}
	d.Dispatch(ctx, privateMsg(5, 6, "/unlink")) // not linked: nobody's link ends
	if len(ul.links) != 2 {
		t.Fatalf("an unlinked user's /unlink reached the Unlinker: %+v", ul.links)
	}

	clk.Advance(2 * time.Hour) // past the re-confirmation time
	d.Dispatch(ctx, privateMsg(6, 5, "hello"))
	d.Dispatch(ctx, privateMsg(7, 5, "/unlink"))
	if got := rec.ids(); len(got) != 2 {
		t.Fatalf("an overdue link's update reached the Handler: %v", got)
	}
	if len(ul.links) != 3 { // an overdue link can still be ended by its owner
		t.Fatalf("an overdue link's /unlink: %+v", ul.links)
	}
	msgs := srv.Messages(5)
	if len(msgs) != 4 || msgs[2].Text != ReconfirmText || msgs[3].Text != "Unlinked." {
		t.Fatalf("overdue replies %+v", msgs)
	}
}
