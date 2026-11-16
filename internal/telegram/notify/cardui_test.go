package notify_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/card"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/notify"
)

// question is an open question action with two options.
func question(id string, version int64) notify.Resource {
	r := action(id, version, false)
	r.Label, r.Title = "Question", "Pick one "+id
	r.Action = &card.Action{ID: id, Type: "question", Requester: "control", Options: []string{"Yes", "No"}, Status: "open"}
	return r
}

// keyboard decodes a call's reply_markup ("" or none: nil).
func keyboard(t *testing.T, c tgtestCall) [][]botapi.InlineKeyboardButton {
	t.Helper()
	raw := c.Params["reply_markup"]
	if raw == "" {
		return nil
	}
	var m botapi.InlineKeyboardMarkup
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("reply_markup %q: %v", raw, err)
	}
	return m.InlineKeyboard
}

type tgtestCall = struct {
	Params map[string]string
}

func call(params map[string]string) tgtestCall { return tgtestCall{Params: params} }

func labels(kb [][]botapi.InlineKeyboardButton) []string {
	var out []string
	for _, r := range kb {
		for _, b := range r {
			out = append(out, b.Text)
		}
	}
	return out
}

func has(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// A card carries its quick actions when it is sent and on every edit; its settled state has none, stated as an empty
// keyboard (an edit never relies on Telegram dropping a keyboard it wasn't sent).
func TestCardKeyboardOnSendAndEdit(t *testing.T) {
	e := newEnv(t, 1)
	defer e.run(nil)()
	e.change("hum_1", question("hum_1", 1), 1, changefeed.Created)
	sends := e.waitCalls("sendMessage", 1)
	kb := labels(keyboard(t, call(sends[0].Params)))
	if !has(kb, "Yes") || !has(kb, "No") || !has(kb, "More…") {
		t.Fatalf("card keyboard %v, want its options and More…", kb)
	}
	for _, r := range keyboard(t, call(sends[0].Params)) {
		for _, b := range r {
			// "h<verb>:<space>:<action>[:n]" without the IDs' prefixes: spc_1 and hum_1 are "1" here.
			if f := strings.Split(b.CallbackData, ":"); b.CallbackData != "" && (len(f) < 3 || f[1] != "1" || f[2] != "1") {
				t.Errorf("callback %q doesn't name the space and action", b.CallbackData)
			}
		}
	}

	q2 := question("hum_1", 2)
	q2.Assignee = "usr_9"
	e.change("hum_1", q2, 2, changefeed.Updated)
	edits := e.waitCalls("editMessageText", 1)
	if kb := labels(keyboard(t, call(edits[0].Params))); !has(kb, "Yes") || !strings.Contains(edits[0].Params["text"], "claimed by someone else") {
		t.Fatalf("edit %q keyboard %v, want the keyboard again and the new state", edits[0].Params["text"], kb)
	}

	fin := question("hum_1", 3)
	fin.Final, fin.Action.Status, fin.Action.Outcome = true, "resolved", "answered"
	e.change("hum_1", fin, 3, changefeed.Updated)
	edits = e.waitCalls("editMessageText", 2)
	if got := edits[1].Params; got["text"] != "✅ Answered · Pick one hum_1" || got["reply_markup"] != `{"inline_keyboard":[]}` {
		t.Fatalf("final edit %q %q, want the compact line and an empty keyboard", got["text"], got["reply_markup"])
	}
	if n := len(e.fake.Calls("sendMessage")); n != 1 {
		t.Errorf("%d sends, want one message for the item", n)
	}
}

// The buttons follow what the link may do (cosmetic: the API decides each tap): no respond permission leaves only
// Open in app, no approval:decide hides Approve and Deny.
func TestCardButtonsFollowTheLink(t *testing.T) {
	e := newEnv(t, 1)
	defer e.run(nil)()
	e.az.mu.Lock()
	e.az.deny["telegram:tgl_1|"+authz.ApprovalDecide.String()] = true
	e.az.mu.Unlock()
	e.change("hum_1", action("hum_1", 1, false), 1, changefeed.Created)
	sends := e.waitCalls("sendMessage", 1)
	if kb := labels(keyboard(t, call(sends[0].Params))); has(kb, "✅ Approve") || has(kb, "❌ Deny") || !has(kb, "More…") {
		t.Fatalf("keyboard %v without approval:decide", kb)
	}
	e.az.mu.Lock()
	e.az.deny["telegram:tgl_1|"+authz.HumanRespond.String()] = true
	e.az.mu.Unlock()
	e.change("hum_2", question("hum_2", 1), 1, changefeed.Created)
	sends = e.waitCalls("sendMessage", 2)
	if kb := keyboard(t, call(sends[1].Params)); len(kb) != 0 {
		t.Fatalf("keyboard %v without human_action:respond", labels(kb))
	}
}

// A card names its space in the main chat, and not in the space's own topic.
func TestCardNamesItsSpaceOutsideItsTopic(t *testing.T) {
	e := newEnv(t, 1)
	defer e.run(nil)()
	r := question("hum_1", 1)
	r.SpaceName = "Alpha"
	e.change("hum_1", r, 1, changefeed.Created)
	if s := e.waitCalls("sendMessage", 1); !strings.HasPrefix(s[0].Params["text"], "❓ Question · Alpha\n") {
		t.Fatalf("main-chat card %q doesn't name its space", s[0].Params["text"])
	}

	te, _, _ := topicsEnv(t)
	defer te.run(nil)()
	te.change("hum_1", r, 1, changefeed.Created)
	m := te.waitText(1001, "❓ Question")
	if len(m) != 1 || m[0].MessageThreadID == 0 || strings.Contains(m[0].Text, "Alpha") {
		t.Fatalf("topic card %+v names its space or isn't in the topic", m)
	}
}

// Losing sight of an item (access, a mute, a hidden space) edits its card to "No longer available here": no title, no
// body, no keyboard, and the continuations that carry the body are deleted.
func TestCardRetractedWhenTheLinkLosesSight(t *testing.T) {
	for name, revoke := range map[string]func(e *env){
		"grants": func(e *env) {
			e.az.mu.Lock()
			delete(e.az.space["telegram:tgl_1"], "spc_1")
			e.az.mu.Unlock()
		},
		"muted": func(e *env) {
			muted := []string{"spc_1"}
			e.editLink(func(l *notify.Link) { l.Prefs.MutedSpaces = &muted })
		},
		"hidden": func(e *env) { e.pol.hide("spc_1") },
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, 1)
			defer e.run(nil)()
			r := question("hum_1", 1)
			r.Detail = longBody(12)
			e.change("hum_1", r, 1, changefeed.Created)
			e.waitExactly(3)
			revoke(e)
			r2 := question("hum_1", 2)
			r2.Detail = longBody(12)
			e.change("hum_1", r2, 2, changefeed.Updated)
			e.waitExactly(1)
			m := e.fake.Messages(1001)[0]
			if m.Text != card.Gone || len(m.Entities) != 0 || m.ReplyMarkup != nil && len(m.ReplyMarkup.InlineKeyboard) != 0 {
				t.Fatalf("retracted card %q %+v %+v", m.Text, m.Entities, m.ReplyMarkup)
			}
			// Nothing more happens for further changes while it stays out of sight.
			edits := len(e.fake.Calls("editMessageText"))
			r3 := question("hum_1", 3)
			e.change("hum_1", r3, 3, changefeed.Updated)
			time.Sleep(50 * time.Millisecond)
			if n := len(e.fake.Calls("editMessageText")); n != edits {
				t.Errorf("%d more edits of a retracted card", n-edits)
			}
		})
	}
}

// A reminder replies to the card; the next one deletes the one before; the settled card deletes the last.
func TestAlertsReplyToTheCard(t *testing.T) {
	e := newEnv(t, 1)
	defer e.run(nil)()
	e.change("hum_1", action("hum_1", 1, false), 1, changefeed.Created)
	cardID := e.waitExactlyMsgs(1)[0].MessageID
	a := action("hum_1", 1, false)
	a.RemindedAt = e.clk.Now().Add(time.Hour).UnixMicro()
	e.update(a, 1)
	first := e.waitExactlyMsgs(2)[1]
	if first.ReplyToMessage == nil || first.ReplyToMessage.MessageID != cardID || !strings.HasPrefix(first.Text, "Reminder:") {
		t.Fatalf("reminder %+v isn't a reply to the card %d", first, cardID)
	}
	a.RemindedAt = e.clk.Now().Add(2 * time.Hour).UnixMicro()
	e.update(a, 1)
	deadline := time.Now().Add(10 * time.Second)
	for {
		ms := e.fake.Messages(1001)
		if len(ms) == 2 && ms[1].MessageID != first.MessageID {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("messages %d, want the card and the newest reminder only", len(ms))
		}
		time.Sleep(2 * time.Millisecond)
	}
	e.update(action("hum_1", 2, true), 2)
	e.waitExactlyMsgs(1)
}

func (e *env) waitExactlyMsgs(n int) []botapi.Message {
	e.t.Helper()
	e.waitExactly(n)
	return e.fake.Messages(1001)
}

// Card finds a card from memory the moment it is sent, before its row is written, with the link it went to and the
// version it shows.
func TestCardLookupFromMemoryFirst(t *testing.T) {
	e := newEnv(t, 1)
	e.flushEvery = time.Hour
	defer e.run(e.maps)()
	e.change("hum_1", question("hum_1", 1), 1, changefeed.Created)
	m := e.waitExactlyMsgs(1)[0]
	if _, rows := e.maps.counts(); rows != 0 {
		t.Fatalf("%d rows written already; the test needs an unflushed card", rows)
	}
	var got notify.Mapping
	deadline := time.Now().Add(5 * time.Second)
	for ok := false; !ok; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the card was never found")
		}
		got, ok = e.n.Card(context.Background(), 1001, m.MessageID)
	}
	if got.LinkID != "tgl_1" || got.SpaceID != "spc_1" || got.Key.ID != "hum_1" || got.Version != 1 {
		t.Fatalf("card %+v", got)
	}
	if _, ok := e.n.Card(context.Background(), 1001, m.MessageID+50); ok {
		t.Fatal("a message that is no card was found")
	}
}

// EditCard: the chat's edit of an older version is refused; its form at the card's version stays when the notifier
// sees that version again; the notifier's settled state replaces it, and a chat edit can't undo that.
func TestEditCardOrdering(t *testing.T) {
	e := newEnv(t, 1)
	defer e.run(nil)()
	e.change("hum_1", question("hum_1", 1), 1, changefeed.Created)
	id := e.waitExactlyMsgs(1)[0].MessageID
	k := notify.Key{Type: notify.TypeHumanAction, ID: "hum_1"}
	ran := 0
	edit := func() bool { ran++; return true }
	waitCard := func() {
		deadline := time.Now().Add(5 * time.Second)
		for {
			if _, ok := e.n.Card(context.Background(), 1001, id); ok {
				return
			}
			if time.Now().After(deadline) {
				t.Fatal("no card")
			}
			time.Sleep(time.Millisecond)
		}
	}
	waitCard()
	if !e.n.EditCard(1001, id, k, notify.ChatEdit{Version: 2, Form: true}, edit) || ran != 1 {
		t.Fatal("the chat's form at a newer version was refused")
	}
	if e.n.EditCard(1001, id, k, notify.ChatEdit{Version: 1, Form: true}, edit) || ran != 1 {
		t.Fatal("an edit of an older version ran")
	}
	edits := len(e.fake.Calls("editMessageText"))
	q2 := question("hum_1", 2)
	e.change("hum_1", q2, 2, changefeed.Updated) // the change the chat itself made: its form stays
	time.Sleep(50 * time.Millisecond)
	if n := len(e.fake.Calls("editMessageText")); n != edits {
		t.Fatalf("the notifier drew over the chat's form of the same version (%d edits)", n-edits)
	}
	fin := question("hum_1", 3)
	fin.Final, fin.Action.Status, fin.Action.Outcome = true, "resolved", "answered"
	e.change("hum_1", fin, 3, changefeed.Updated)
	e.waitCalls("editMessageText", edits+1)
	if e.n.EditCard(1001, id, k, notify.ChatEdit{Version: 3, Form: true}, edit) || ran != 1 {
		t.Fatal("the chat's form replaced the settled card")
	}
}

// The race between the chat drawing its form on a card and the notifier settling it: whatever the order, the
// notifier's final state is what the message shows in the end.
func TestChatEditsRaceTheNotifiersFinalState(t *testing.T) {
	for round := range 20 {
		e := newEnv(t, 1)
		stop := e.run(nil)
		e.change("hum_1", question("hum_1", 1), 1, changefeed.Created)
		id := e.waitExactlyMsgs(1)[0].MessageID
		for {
			if _, ok := e.n.Card(context.Background(), 1001, id); ok {
				break
			}
			time.Sleep(time.Millisecond)
		}
		cl, err := botapi.New(botapi.Options{BaseURL: e.fake.URL(), Token: e.fake.Token(), HTTPClient: http.DefaultClient})
		if err != nil {
			t.Fatal(err)
		}
		k := notify.Key{Type: notify.TypeHumanAction, ID: "hum_1"}
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 15 {
				e.n.EditCard(1001, id, k, notify.ChatEdit{Version: 1, Form: true}, func() bool {
					_, err := cl.EditMessageText(context.Background(), botapi.EditMessageTextParams{ChatID: 1001, MessageID: id,
						Text: "form view " + strings.Repeat("·", i+1), ReplyMarkup: &botapi.InlineKeyboardMarkup{InlineKeyboard: [][]botapi.InlineKeyboardButton{{{Text: "x", CallbackData: "x"}}}}})
					return err == nil
				})
			}
		}()
		fin := question("hum_1", 2)
		fin.Final, fin.Action.Status, fin.Action.Outcome = true, "resolved", "answered"
		e.change("hum_1", fin, 2, changefeed.Updated)
		wg.Wait()
		deadline := time.Now().Add(10 * time.Second)
		for {
			m := e.fake.Messages(1001)[0]
			if m.Text == "✅ Answered · Pick one hum_1" && (m.ReplyMarkup == nil || len(m.ReplyMarkup.InlineKeyboard) == 0) {
				break
			}
			if time.Now().After(deadline) {
				for _, c := range e.fake.Calls("editMessageText") {
					t.Logf("%d %.30q", c.Status, c.Params["text"])
				}
				t.Fatalf("round %d: the card ends as %q, want the notifier's final state", round, m.Text)
			}
			time.Sleep(2 * time.Millisecond)
		}
		stop()
	}
}

// When the card message was deleted and a continuation can't be deleted either, the rows of the old continuations
// are still dropped (the two dropConts calls in upsertOnce's isGone branches), for a live and for a settled card.
func TestDeletedCardDropsItsContinuationRows(t *testing.T) {
	for _, final := range []bool{false, true} {
		t.Run(map[bool]string{false: "live", true: "final"}[final], func(t *testing.T) {
			e := newEnv(t, 1)
			e.flushEvery = 2 * time.Millisecond
			defer e.run(e.maps)()
			e.firstCard(12)
			old := e.waitExactlyMsgs(3)
			e.waitRows(3)
			cl, err := botapi.New(botapi.Options{BaseURL: e.fake.URL(), Token: e.fake.Token(), HTTPClient: http.DefaultClient})
			if err != nil {
				t.Fatal(err)
			}
			for err := cl.DeleteMessage(context.Background(), 1001, old[0].MessageID); err != nil; err = cl.DeleteMessage(context.Background(), 1001, old[0].MessageID) {
				if !strings.Contains(err.Error(), "429") {
					t.Fatal(err)
				}
				e.clk.Advance(time.Second) // the fake's per-chat rate limit
			}
			e.fake.FailNext("deleteMessage", 500, "Internal Server Error", 0) // the last continuation won't go
			r := action("hum_1", 2, final)
			r.Detail = longBody(12)
			e.change("hum_1", r, 2, changefeed.Updated)
			e.waitCalls("editMessageText", 1) // "message to edit not found"
			stale := func() (n int) {
				e.maps.mu.Lock()
				defer e.maps.mu.Unlock()
				for _, m := range old[1:] {
					if _, ok := e.maps.rows[[2]int64{1001, int64(m.MessageID)}]; ok {
						n++
					}
				}
				return n
			}
			deadline := time.Now().Add(10 * time.Second)
			for stale() != 0 {
				if time.Now().After(deadline) {
					t.Fatalf("%d rows of the old continuations are left", stale())
				}
				time.Sleep(2 * time.Millisecond)
			}
		})
	}
}

// A retraction whose continuation delete fails is retried until nothing of the body is left: the card already saying
// it is gone doesn't end the retries.
func TestRetractionRetriesLeftoverContinuations(t *testing.T) {
	e := newEnv(t, 1)
	defer e.run(nil)()
	r := question("hum_1", 1)
	r.Detail = longBody(12)
	e.change("hum_1", r, 1, changefeed.Created)
	e.waitExactly(3)
	muted := []string{"spc_1"}
	e.editLink(func(l *notify.Link) { l.Prefs.MutedSpaces = &muted })
	e.fake.FailNext("deleteMessage", 500, "Internal Server Error", 0)
	r2 := question("hum_1", 2)
	r2.Detail = longBody(12)
	e.change("hum_1", r2, 2, changefeed.Updated)
	e.waitExactly(1)
	if m := e.fake.Messages(1001)[0]; m.Text != card.Gone {
		t.Fatalf("card %q", m.Text)
	}
}

// A card the chat settled itself (the answer given on the card) still loses its continuations and its reminder when the
// notifier then gets the settled change: the chat's edit already shows that version, but the rest is the upsert's.
func TestChatSettledCardDropsContinuationsAndAlert(t *testing.T) {
	e := newEnv(t, 1)
	defer e.run(nil)()
	r := question("hum_1", 1)
	r.Detail = longBody(12)
	e.change("hum_1", r, 1, changefeed.Created)
	e.waitExactly(3)
	id := e.fake.Messages(1001)[0].MessageID
	a := question("hum_1", 1)
	a.Detail = longBody(12)
	a.RemindedAt = e.clk.Now().Add(time.Hour).UnixMicro()
	e.update(a, 1)
	e.waitExactly(4)
	k := notify.Key{Type: notify.TypeHumanAction, ID: "hum_1"}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, ok := e.n.Card(context.Background(), 1001, id); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no card")
		}
		time.Sleep(time.Millisecond)
	}
	if !e.n.EditCard(1001, id, k, notify.ChatEdit{Version: 2, Final: true}, func() bool { return true }) {
		t.Fatal("the chat's settled edit was refused")
	}
	fin := question("hum_1", 2)
	fin.Detail = longBody(12)
	fin.Final, fin.Action.Status, fin.Action.Outcome = true, "resolved", "answered"
	e.change("hum_1", fin, 2, changefeed.Updated)
	e.waitExactly(1)
}

// The chat handler's edit never runs over a card that was retracted, even at the same version.
func TestEditCardRefusedOnRetractedCard(t *testing.T) {
	e := newEnv(t, 1)
	defer e.run(nil)()
	e.change("hum_1", question("hum_1", 1), 1, changefeed.Created)
	id := e.waitExactlyMsgs(1)[0].MessageID
	muted := []string{"spc_1"}
	e.editLink(func(l *notify.Link) { l.Prefs.MutedSpaces = &muted })
	e.change("hum_1", question("hum_1", 2), 2, changefeed.Updated)
	deadline := time.Now().Add(5 * time.Second)
	for e.fake.Messages(1001)[0].Text != card.Gone {
		if time.Now().After(deadline) {
			t.Fatal("card not retracted")
		}
		time.Sleep(time.Millisecond)
	}
	k := notify.Key{Type: notify.TypeHumanAction, ID: "hum_1"}
	ran := 0
	for _, v := range []int64{2, 3} {
		if e.n.EditCard(1001, id, k, notify.ChatEdit{Version: v}, func() bool { ran++; return true }) || ran != 0 {
			t.Fatalf("EditCard at version %d redrew a retracted card", v)
		}
	}
}
