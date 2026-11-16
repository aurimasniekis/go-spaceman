package notify_test

import (
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/notify"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/settings"
)

func longBody(paras int) string {
	var b strings.Builder
	for i := 0; i < paras; i++ {
		fmt.Fprintf(&b, "**Part %d:** %s\n\n", i, strings.Repeat("word ", 150))
	}
	return b.String()
}

func (e *env) change(id string, r notify.Resource, v int64, kind string) {
	e.rd.set(r)
	before := e.feed.reads.Load()
	e.feed.add("spc_1", notify.TypeHumanAction, id, kind, v)
	e.ring()
	e.endWindow()
	e.settle(before)
}

// waitExactly waits until the chat holds n messages (a call is recorded a moment before its effect is visible).
func (e *env) waitExactly(n int) {
	e.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for len(e.fake.Messages(1001)) != n {
		if time.Now().After(deadline) {
			e.t.Fatalf("%d messages, want %d", len(e.fake.Messages(1001)), n)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// waitLastText waits until the chat's last message contains want (the call that edits it is logged a moment before
// its effect is visible).
func (e *env) waitLastText(chat int64, want string) {
	e.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if m := e.fake.Messages(chat); len(m) > 0 && strings.Contains(m[len(m)-1].Text, want) {
			return
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("the last message never contained %q", want)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// waitRows waits until the mapping store holds n rows.
func (e *env) waitRows(n int) {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, rows := e.maps.counts(); rows == n {
			return
		}
		if time.Now().After(deadline) {
			_, rows := e.maps.counts()
			e.t.Fatalf("%d mapping rows, want %d", rows, n)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestLongBodySplitsIntoContinuations: the whole body arrives, as formatting and not raw Markdown, in a card plus
// numbered replies that each fit a message.
func TestLongBodySplitsIntoContinuations(t *testing.T) {
	e := newEnv(t, 1)
	defer e.run(e.maps)()
	r := action("hum_1", 1, false)
	r.Detail = longBody(12) // about 9000 characters
	e.change("hum_1", r, 1, changefeed.Created)
	e.waitCalls("sendMessage", 3)
	e.waitExactly(3)
	msgs := e.fake.Messages(1001)
	if len(msgs) < 3 || len(msgs) > 4 {
		t.Fatalf("%d messages, want a card and 2-3 continuations", len(msgs))
	}
	for i, m := range msgs {
		if n := len(utf16.Encode([]rune(m.Text))); n > 4096 {
			t.Errorf("message %d has %d units", i, n)
		}
		if strings.Contains(m.Text, "**") {
			t.Errorf("message %d shows raw markdown", i)
		}
		if i > 0 {
			if want := fmt.Sprintf("(%d/%d)\n", i+1, len(msgs)); !strings.HasPrefix(m.Text, want) {
				t.Errorf("message %d starts %q, want %q", i, m.Text[:8], want)
			}
			if m.ReplyToMessage == nil || m.ReplyToMessage.MessageID != msgs[0].MessageID {
				t.Errorf("message %d is not a reply to the card", i)
			}
		}
	}
	if len(msgs[0].Entities) < 2 || msgs[0].Entities[0].Type != "bold" {
		t.Errorf("card entities = %+v, want the bold head and the body's formatting", msgs[0].Entities)
	}
	if !strings.Contains(msgs[0].Text, "Priority normal") {
		t.Errorf("card has no state line: %q", msgs[0].Text[:40])
	}
}

// TestContinuationsFollowTheCard: a change edits the continuations in place, deletes the ones the shorter body no
// longer needs, and sends more when the body grows.
func TestContinuationsFollowTheCard(t *testing.T) {
	e := newEnv(t, 1)
	defer e.run(e.maps)()
	r := action("hum_1", 1, false)
	r.Detail = longBody(12)
	e.change("hum_1", r, 1, changefeed.Created)
	e.waitCalls("sendMessage", 3)
	e.waitExactly(3)
	n := len(e.fake.Messages(1001))

	r2 := action("hum_1", 2, false)
	r2.Detail = longBody(12) + "tail"
	e.change("hum_1", r2, 2, changefeed.Updated)
	e.waitCalls("editMessageText", 2) // the card (version changed) and the continuation that changed; the others stay
	e.waitExactly(n)
	e.waitLastText(1001, "tail") // the edit's effect shows a moment after the call is logged

	r3 := action("hum_1", 3, true)
	r3.Detail = "Short now."
	e.change("hum_1", r3, 3, changefeed.Updated)
	e.waitExactly(1)
	msgs := e.fake.Messages(1001)
	if len(msgs) != 1 || strings.Contains(msgs[0].Text, "Short now.") || !strings.Contains(msgs[0].Text, "✅ Approved") {
		t.Fatalf("messages = %+v, want the card alone in its final state", msgs)
	}

	r4 := action("hum_1", 4, true) // a settled card never shows a body again, however long
	r4.Detail = longBody(12)
	e.change("hum_1", r4, 4, changefeed.Updated)
	time.Sleep(50 * time.Millisecond)
	e.waitExactly(1)
}

// TestLongBodyIsCapped: a body beyond four messages ends with the "continued in the app" line.
func TestLongBodyIsCapped(t *testing.T) {
	e := newEnv(t, 1)
	defer e.run(e.maps)()
	r := action("hum_1", 1, false)
	r.Detail = longBody(60)
	e.change("hum_1", r, 1, changefeed.Created)
	e.waitExactly(4)
	msgs := e.fake.Messages(1001)
	if !strings.Contains(msgs[3].Text, "continued in the app") {
		t.Errorf("last message does not point to the app: %q", msgs[3].Text[len(msgs[3].Text)-60:])
	}
}

// TestContinuationsSurviveARestart: the continuation IDs are in the mapping, so a new process deletes the ones a
// shorter body no longer needs.
func TestContinuationsSurviveARestart(t *testing.T) {
	e := newEnv(t, 1)
	e.flushEvery = 2 * time.Millisecond
	stop := e.run(e.maps)
	r := action("hum_1", 1, false)
	r.Detail = longBody(12)
	e.change("hum_1", r, 1, changefeed.Created)
	e.waitExactly(3)
	e.waitRows(3) // the card and both continuations reached the store
	stop()

	stop = e.run(e.maps)
	defer stop()
	r2 := action("hum_1", 2, true)
	r2.Detail = "Short."
	e.change("hum_1", r2, 2, changefeed.Updated)
	e.waitExactly(1)
}

// TestDeletedContinuationsAreNotReloaded: after a shrink, a grow and a restart, the next update edits the three messages
// that exist; the continuations the shrink deleted are gone from the store and are not loaded again.
func TestDeletedContinuationsAreNotReloaded(t *testing.T) {
	e := newEnv(t, 1)
	e.flushEvery = 2 * time.Millisecond
	stop := e.run(e.maps)
	defer func() { stop() }()
	r := action("hum_1", 1, false)
	r.Detail = longBody(12)
	e.change("hum_1", r, 1, changefeed.Created)
	e.waitExactly(3)
	e.waitRows(3)

	r2 := action("hum_1", 2, false)
	r2.Detail = "Short."
	e.change("hum_1", r2, 2, changefeed.Updated)
	e.waitExactly(1)
	e.waitRows(1) // the deleted continuations' rows are removed

	r3 := action("hum_1", 3, false)
	r3.Detail = longBody(12)
	e.change("hum_1", r3, 3, changefeed.Updated)
	e.waitExactly(3)
	e.waitRows(3)
	sends := len(e.fake.Calls("sendMessage"))
	stop()

	stop = e.run(e.maps)
	r4 := action("hum_1", 4, false)
	r4.Detail = longBody(12) + "tail"
	e.change("hum_1", r4, 4, changefeed.Updated)
	e.waitLastText(1001, "tail")
	e.waitExactly(3)
	if n := len(e.fake.Calls("sendMessage")); n != sends {
		t.Errorf("%d sends after the restart, want none (%d before): a dead continuation was reloaded and resent", n-sends, sends)
	}
	if n := len(e.fake.Calls("deleteMessage")); n != 2 {
		t.Errorf("%d deletes in all, want the 2 of the shrink", n)
	}
}

// TestContinuationOffersTheAppButtonInAPrivateChat: a cut body in a private chat at full level ends in the Mini App
// button, which is the link ItemURL gives for that card.
func TestContinuationOffersTheAppButtonInAPrivateChat(t *testing.T) {
	const link = "https://mini.example/spaces/spc_1/human?action=hum_1"
	e := newEnv(t, 1)
	e.itemURL = func(spaceID string, k notify.Key) string {
		if k.Type == notify.TypeHumanAction && k.ID == "hum_1" && spaceID == "spc_1" {
			return link
		}
		return ""
	}
	defer e.run(nil)()
	r := action("hum_1", 1, false)
	r.Detail = longBody(60) // longer than the four parts: the body is cut, so the last part gets the button
	e.change("hum_1", r, 1, changefeed.Created)
	e.waitCalls("sendMessage", 4)
	time.Sleep(100 * time.Millisecond)
	found := false
	for _, m := range e.fake.Messages(1001) {
		if m.ReplyMarkup == nil {
			continue
		}
		for _, row := range m.ReplyMarkup.InlineKeyboard {
			for _, b := range row {
				if b.WebApp != nil && b.WebApp.URL == link && b.Text == "Open in app" {
					found = true
				}
			}
		}
	}
	if !found {
		t.Errorf("no message carries the Open in app button for %s", link)
	}
}

// TestNoContinuationsInGroupsOrAtSummary: a long body goes out as continuations only to a private chat at content
// level full; a group chat and a summary-level space get the card alone, without the body.
func TestNoContinuationsInGroupsOrAtSummary(t *testing.T) {
	for name, tc := range map[string]struct {
		chat  int64
		level string
	}{"group": {-1001, ""}, "summary": {1001, settings.ContentSummary}} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, 1)
			e.pol.level = tc.level
			l := linkFor(1)
			l.ChatID = tc.chat
			e.links.Set([]notify.Link{l})
			defer e.run(nil)()
			r := action("hum_1", 1, false)
			r.Detail = longBody(12)
			e.change("hum_1", r, 1, changefeed.Created)
			e.waitCalls("sendMessage", 1)
			time.Sleep(100 * time.Millisecond) // a wrongly queued continuation would arrive by now
			msgs := e.fake.Messages(tc.chat)
			if len(msgs) != 1 {
				t.Fatalf("%d messages, want the card alone", len(msgs))
			}
			if strings.Contains(msgs[0].Text, "Part 0") {
				t.Errorf("the card shows the body: %.60q", msgs[0].Text)
			}
		})
	}
}

// TestContinuationRowsMoveForwardWhenTheCardIsRecorded: retention prunes by created_at, so when the card is recorded
// again later (an edit with no change to the continuations), their rows move forward with it and are not pruned
// before the card.
func TestContinuationRowsMoveForwardWhenTheCardIsRecorded(t *testing.T) {
	e := newEnv(t, 1)
	e.flushEvery = 2 * time.Millisecond
	defer e.run(e.maps)()
	r := action("hum_1", 1, false)
	r.Detail = longBody(12)
	e.change("hum_1", r, 1, changefeed.Created)
	e.waitExactly(3)
	e.waitRows(3)

	later := e.clk.Now().Add(time.Hour)
	e.clk.Set(later)
	r2 := action("hum_1", 2, false)
	r2.Detail = longBody(12) // the same body: the continuations are not edited, only the card is
	e.change("hum_1", r2, 2, changefeed.Updated)
	e.waitCalls("editMessageText", 1)

	deadline := time.Now().Add(10 * time.Second)
	for {
		conts, moved := 0, 0
		e.maps.mu.Lock()
		for _, m := range e.maps.rows {
			if !m.Cont {
				continue
			}
			conts++
			if m.CreatedAt.Equal(later) {
				moved++
			}
		}
		e.maps.mu.Unlock()
		if conts == 2 && moved == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d of %d continuation rows moved to the later record time, want 2 of 2", moved, conts)
		}
		time.Sleep(time.Millisecond)
	}
}
