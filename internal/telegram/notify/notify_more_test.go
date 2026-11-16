package notify_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/card"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/notify"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/sender"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/settings"
)

// holdChat occupies the chat's single in-flight slot until the returned function is called, so cards queued for it
// wait in the sender.
func (e *env) holdChat(chat int64) (release func()) {
	e.t.Helper()
	started, block := make(chan struct{}), make(chan struct{})
	err := e.snd.Enqueue(sender.Message{ChatID: chat, Priority: sender.Notification, Key: "hold",
		Send: func(context.Context, *botapi.Client) error { close(started); <-block; return nil }})
	if err != nil {
		e.t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		e.t.Fatal("the chat's slot was not taken")
	}
	return func() { close(block) }
}

// editLink replaces the first link with a changed copy (a snapshot is never modified in place).
func (e *env) editLink(f func(*notify.Link)) {
	ls := append([]notify.Link(nil), e.links.Snapshot()...)
	f(&ls[0])
	e.links.Set(ls)
}

// TestSendTimeRecheck: a card that waited in the sender while the link's access ended is dropped at send time, for a
// first send and for an edit (ADR-0078: the check is repeated from memory when the call runs).
func TestSendTimeRecheck(t *testing.T) {
	revokes := map[string]func(e *env){
		"grants revoked": func(e *env) {
			e.az.mu.Lock()
			delete(e.az.space["telegram:tgl_1"], "spc_1")
			e.az.mu.Unlock()
		},
		"link gone": func(e *env) { e.links.Set(nil) },
		"grants revoked and link cache cleared": func(e *env) {
			e.az.mu.Lock()
			delete(e.az.space["telegram:tgl_1"], "spc_1")
			e.az.mu.Unlock()
			e.links.Set(nil)
		},
		"space hidden": func(e *env) { e.pol.hide("spc_1") },
		"space muted": func(e *env) {
			muted := []string{"spc_1"}
			e.editLink(func(l *notify.Link) { l.Prefs.MutedSpaces = &muted })
		},
		"reconfirm overdue": func(e *env) {
			e.editLink(func(l *notify.Link) { l.ReconfirmAt = t0.Add(-time.Minute) })
		},
	}
	for name, revoke := range revokes {
		for _, edit := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/edit=%v", name, edit), func(t *testing.T) {
				e := newEnv(t, 1)
				stop := e.run(nil)
				defer stop()
				if edit { // the card exists
					e.rd.set(action("hum_1", 1, false))
					e.feed.add("spc_1", notify.TypeHumanAction, "hum_1", changefeed.Created, 1)
					e.ring()
					e.endWindow()
					e.waitCalls("sendMessage", 1)
					time.Sleep(100 * time.Millisecond) // the card is recorded just after the call returns
				}
				release := e.holdChat(1001)
				before := e.feed.reads.Load()
				e.rd.set(action("hum_1", 2, true))
				if !edit {
					e.rd.set(action("hum_1", 1, false))
				}
				e.feed.add("spc_1", notify.TypeHumanAction, "hum_1", changefeed.Updated, 2)
				e.ring()
				e.endWindow()
				e.settle(before) // the card is queued behind the held slot
				revoke(e)
				release()
				time.Sleep(300 * time.Millisecond)
				if n := len(e.fake.Calls("sendMessage")); (edit && n != 1) || (!edit && n != 0) {
					t.Errorf("%d sends after the access ended", n)
				}
				// A card that exists is retracted: its one edit says only that it is gone, without a keyboard. A link that
				// is gone gets nothing at all.
				edits := e.fake.Calls("editMessageText")
				want := 0
				if edit && name != "link gone" && name != "grants revoked and link cache cleared" {
					want = 1
				}
				if len(edits) != want {
					t.Errorf("%d edits after the access ended, want %d", len(edits), want)
				}
				for _, c := range edits {
					if c.Params["text"] != card.Gone || c.Params["entities"] != "" || c.Params["reply_markup"] != `{"inline_keyboard":[]}` {
						t.Errorf("retraction shows %q %q %q", c.Params["text"], c.Params["entities"], c.Params["reply_markup"])
					}
				}
			})
		}
	}
}

// TestSendTimeRecheckKeepsAllowedSends: the same held-slot setup without a revocation still sends (the recheck drops
// nothing it shouldn't).
func TestSendTimeRecheckKeepsAllowedSends(t *testing.T) {
	e := newEnv(t, 1)
	stop := e.run(nil)
	defer stop()
	release := e.holdChat(1001)
	before := e.feed.reads.Load()
	e.rd.set(action("hum_1", 1, false))
	e.feed.add("spc_1", notify.TypeHumanAction, "hum_1", changefeed.Created, 1)
	e.ring()
	e.endWindow()
	e.settle(before)
	release()
	e.waitCalls("sendMessage", 1)
}

// TestProtectContent: a card with a body at the full content level can't be forwarded or saved (plan 3.8); a summary
// card, and a full card without a body, are not protected.
func TestProtectContent(t *testing.T) {
	for _, tc := range []struct {
		level, detail string
		want          bool
	}{
		{settings.ContentFull, "Please approve", true},
		{settings.ContentFull, "", false},
		{settings.ContentSummary, "Please approve", false},
	} {
		t.Run(tc.level+"/"+tc.detail, func(t *testing.T) {
			e := newEnv(t, 1)
			e.pol.level = tc.level
			stop := e.run(nil)
			defer stop()
			r := action("hum_1", 1, false)
			r.Detail = tc.detail
			e.rd.set(r)
			e.feed.add("spc_1", notify.TypeHumanAction, "hum_1", changefeed.Created, 1)
			e.ring()
			e.endWindow()
			c := e.waitCalls("sendMessage", 1)
			if got := c[0].Params["protect_content"] == "true"; got != tc.want {
				t.Errorf("protect_content = %q, want protected = %v", c[0].Params["protect_content"], tc.want)
			}
		})
	}
}

// TestFlushTickWritesNothingWhenIdle: the mapping flush tick opens no write when no mapping is waiting, and writes
// the waiting ones once.
func TestFlushTickWritesNothingWhenIdle(t *testing.T) {
	e := newEnv(t, 1)
	e.flushEvery = 20 * time.Millisecond
	stop := e.run(e.maps)
	defer stop()
	time.Sleep(200 * time.Millisecond) // ten ticks with nothing to write
	if saves, _ := e.maps.counts(); saves != 0 {
		t.Fatalf("%d mapping writes while idle, want none", saves)
	}
	e.rd.set(action("hum_1", 1, false))
	e.feed.add("spc_1", notify.TypeHumanAction, "hum_1", changefeed.Created, 1)
	e.ring()
	e.endWindow()
	e.waitCalls("sendMessage", 1)
	time.Sleep(300 * time.Millisecond)
	if saves, rows := e.maps.counts(); saves != 1 || rows != 1 {
		t.Errorf("%d writes for %d rows after one card, want one write of one row", saves, rows)
	}
}

// TestFeedErrorRetry: a read of the change feed that fails part-way leaves the rest unread; it is read again shortly
// after, without a new change.
func TestFeedErrorRetry(t *testing.T) {
	e := newEnv(t, 1)
	e.retry = 50 * time.Millisecond
	stop := e.run(nil)
	defer stop()
	const n = notify.BatchSize + 50
	for i := range n {
		id := fmt.Sprintf("hum_%d", i)
		e.rd.set(action(id, 1, false))
		e.feed.add("spc_1", notify.TypeHumanAction, id, changefeed.Created, 1)
	}
	e.feed.failAt.Store(e.feed.reads.Load() + 2) // the second batch fails
	e.ring()
	e.endWindow()
	e.waitCalls("sendMessage", n) // all of them, with no further ring
	seen := map[string]bool{}
	for _, c := range e.fake.Calls("sendMessage") {
		seen[strings.Split(c.Params["text"], "\n")[1]] = true // the title line
	}
	if len(seen) != n {
		t.Errorf("%d distinct cards, want %d", len(seen), n)
	}
}

// TestBudgetUpdates extends the budget to changes of cards that exist: 50 actions with 5 linked users are updated
// in one window. Per process, each resource costs one read per window and one mapping lookup in its lifetime (the
// card may predate a restart); a cold process pays the lookups again; the feed is read once per window.
func TestBudgetUpdates(t *testing.T) {
	e := newEnv(t, 5)
	stop := e.run(e.maps)
	const res, links = 50, 5
	for i := range res {
		id := fmt.Sprintf("hum_%d", i)
		e.rd.set(action(id, 1, false))
		e.feed.add("spc_1", notify.TypeHumanAction, id, changefeed.Created, 1)
	}
	e.ring()
	e.endWindow()
	e.waitCalls("sendMessage", res*links)
	time.Sleep(100 * time.Millisecond)
	if l := e.maps.lookupCount(); l != 0 {
		t.Errorf("%d mapping lookups for new cards, want none (no card can exist)", l)
	}

	update := func(version int64) {
		before, readsBefore := e.feed.reads.Load(), e.rd.reads.Load()
		for i := range res {
			id := fmt.Sprintf("hum_%d", i)
			e.rd.set(action(id, version, true))
			e.feed.add("spc_1", notify.TypeHumanAction, id, changefeed.Updated, version)
		}
		e.ring()
		e.endWindow()
		e.waitCalls("editMessageText", res*links*int(version-1))
		time.Sleep(100 * time.Millisecond)
		if d := e.feed.reads.Load() - before; d > 1 {
			t.Errorf("%d feed reads for one window of %d changes, want one", d, res)
		}
		if d := e.rd.reads.Load() - readsBefore; d > res {
			t.Errorf("%d resource reads for %d resources", d, res)
		}
	}
	update(2)
	if l := e.maps.lookupCount(); l > res {
		t.Errorf("%d mapping lookups for %d resources, want at most one each", l, res)
	}
	warm := e.maps.lookupCount()
	update(3)
	if l := e.maps.lookupCount(); l != warm {
		t.Errorf("mapping lookups grew from %d to %d on a second update: a known resource is looked up once", warm, l)
	}

	stop() // a cold process: the cards are only in the database
	e.maps.resetLookups()
	stop = e.run(e.maps)
	defer stop()
	before := e.rd.reads.Load()
	for i := range res {
		id := fmt.Sprintf("hum_%d", i)
		e.rd.set(action(id, 4, true))
		e.feed.add("spc_1", notify.TypeHumanAction, id, changefeed.Updated, 4)
	}
	e.ring()
	e.endWindow()
	e.waitCalls("editMessageText", res*links*3)
	if l := e.maps.lookupCount(); l != res {
		t.Errorf("cold process: %d mapping lookups for %d resources, want one each", l, res)
	}
	if d := e.rd.reads.Load() - before; d != res {
		t.Errorf("cold process: %d resource reads, want %d", d, res)
	}
	if c := len(e.fake.Calls("sendMessage")); c != res*links {
		t.Errorf("%d sends in all, want %d: updates edit", c, res*links)
	}
}

// TestSendTimeRecheckChatMoved: a card queued for a chat is dropped when its link moved to another chat while it
// waited (the chat-ID match in the send-time check).
func TestSendTimeRecheckChatMoved(t *testing.T) {
	e := newEnv(t, 1)
	stop := e.run(nil)
	defer stop()
	release := e.holdChat(1001)
	before := e.feed.reads.Load()
	e.rd.set(action("hum_1", 1, false))
	e.feed.add("spc_1", notify.TypeHumanAction, "hum_1", changefeed.Created, 1)
	e.ring()
	e.endWindow()
	e.settle(before)
	e.editLink(func(l *notify.Link) { l.ChatID = 9999 })
	release()
	time.Sleep(300 * time.Millisecond)
	if n := len(e.fake.Calls("sendMessage")); n != 0 {
		t.Errorf("%d sends to a chat the link left", n)
	}
}

// TestContentLevelLoweredWhileQueued: a card queued at the full level shows no body when the space was lowered to
// summary while it waited, and sends nothing when it was lowered to off.
func TestContentLevelLoweredWhileQueued(t *testing.T) {
	for _, tc := range []struct {
		lowered string
		sends   int
	}{{settings.ContentSummary, 1}, {settings.ContentOff, 0}} {
		t.Run(tc.lowered, func(t *testing.T) {
			e := newEnv(t, 1)
			e.pol.level = settings.ContentFull
			stop := e.run(nil)
			defer stop()
			release := e.holdChat(1001)
			before := e.feed.reads.Load()
			e.rd.set(action("hum_1", 1, false))
			e.feed.add("spc_1", notify.TypeHumanAction, "hum_1", changefeed.Created, 1)
			e.ring()
			e.endWindow()
			e.settle(before)
			e.pol.mu.Lock()
			e.pol.level = tc.lowered
			e.pol.mu.Unlock()
			release()
			time.Sleep(300 * time.Millisecond)
			c := e.fake.Calls("sendMessage")
			if len(c) != tc.sends {
				t.Fatalf("%d sends, want %d", len(c), tc.sends)
			}
			for _, call := range c {
				if strings.Contains(call.Params["text"], "Please approve") || call.Params["protect_content"] == "true" {
					t.Errorf("a full-level body or protection after lowering: %q", call.Params["text"])
				}
			}
		})
	}
}
