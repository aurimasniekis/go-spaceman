package notify_test

import (
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/notify"
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
	if !strings.Contains(msgs[0].Text, "Unread") && !strings.Contains(msgs[0].Text, "open") {
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
	if last := e.fake.Messages(1001)[n-1]; !strings.Contains(last.Text, "tail") {
		t.Errorf("last continuation was not edited: %.40q", last.Text)
	}

	r3 := action("hum_1", 3, true)
	r3.Detail = "Short now."
	e.change("hum_1", r3, 3, changefeed.Updated)
	e.waitExactly(1)
	msgs := e.fake.Messages(1001)
	if len(msgs) != 1 || !strings.Contains(msgs[0].Text, "Short now.") || !strings.Contains(msgs[0].Text, "resolved: approved") {
		t.Fatalf("messages = %+v, want the card alone in its final state", msgs)
	}

	r4 := action("hum_1", 4, true)
	r4.Detail = longBody(12)
	e.change("hum_1", r4, 4, changefeed.Updated)
	e.waitExactly(n)
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
	stop := e.run(e.maps)
	r := action("hum_1", 1, false)
	r.Detail = longBody(12)
	e.change("hum_1", r, 1, changefeed.Created)
	e.waitExactly(3)
	time.Sleep(100 * time.Millisecond) // recorded just after the calls return
	stop()                             // flushes the mappings

	stop = e.run(e.maps)
	defer stop()
	r2 := action("hum_1", 2, true)
	r2.Detail = "Short."
	e.change("hum_1", r2, 2, changefeed.Updated)
	e.waitExactly(1)
}
