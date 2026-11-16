package chat

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/card"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/updates"
)

// cardReg plays the notifier's side of a card: what each card message is and shows, with the notifier's EditCard rule.
type cardReg struct {
	mu    sync.Mutex
	refs  map[int]CardRef
	final map[int]bool
	edits []string // "msg version form final" of the edits that ran
}

func (r *cardReg) edit(chatID int64, msg int, actionID string, version int64, form, final bool, edit func() bool) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	ref, ok := r.refs[msg]
	if ok && ref.ActionID == actionID && (ref.Version > version || ref.Version == version && r.final[msg] && !final) {
		return false
	}
	if !edit() {
		return false
	}
	if ok {
		ref.Version = version
		r.refs[msg], r.final[msg] = ref, final
	}
	r.edits = append(r.edits, strings.Join([]string{strings.TrimPrefix(actionID, "hum_")[:1], map[bool]string{true: "form", false: "card"}[form], map[bool]string{true: "final", false: "live"}[final]}, " "))
	return true
}

// withCards gives the handler a notifier side and returns it.
func (e *env) withCards() *cardReg {
	r := &cardReg{refs: map[int]CardRef{}, final: map[int]bool{}}
	e.h.o.Card = func(_ context.Context, chat int64, msg int) (CardRef, bool) {
		r.mu.Lock()
		defer r.mu.Unlock()
		ref, ok := r.refs[msg]
		return ref, ok && chat == chatID
	}
	e.h.o.EditCard = r.edit
	e.h.o.CardCaps = func(string, int64, string, string) card.Caps { return card.Caps{Respond: true, Decide: true} }
	return r
}

// postCard sends an action's card the way the notifier does (the shared renderer) and records it for link.
func (e *env) postCard(r *cardReg, a *humanAction, link string) botapi.Message {
	e.t.Helper()
	bc, err := botapi.New(botapi.Options{BaseURL: e.srv.URL(), Token: e.srv.Token()})
	if err != nil {
		e.t.Fatal(err)
	}
	v := e.h.cardView(e.l, spaceRef{ID: a.SpaceID, Name: "Alpha"}, *a, 0, false)
	p := card.Render(v, card.Caps{Respond: true, Decide: true})[0]
	m, err := bc.SendMessage(context.Background(), botapi.SendMessageParams{ChatID: chatID, Text: p.Text, Entities: p.Entities, ReplyMarkup: p.Markup()})
	if err != nil {
		e.t.Fatal(err)
	}
	r.mu.Lock()
	r.refs[m.MessageID] = CardRef{SpaceID: a.SpaceID, ActionID: a.ID, LinkID: link, Version: a.Version}
	r.mu.Unlock()
	return m
}

func (e *env) messageCount() int { return len(e.srv.Messages(chatID)) }

// A card's option answers the question as the tapping link with the card's version, and the card collapses in place
// to its final line; nothing new is sent. A second tap on the old buttons does nothing but say so.
func TestCardOptionAnswersAndCollapses(t *testing.T) {
	e := newEnv(t)
	reg := e.withCards()
	a := e.addHuman(spA, hid('1'), "question", "Which one?", `{"options":["red","blue"]}`)
	c := e.postCard(reg, a, "tgl_1")
	n := e.messageCount()
	e.tapData(find(c, "blue"), c)
	if e.responded() != 1 || e.api.responds[0]["answer"] != "blue" || e.api.respHdr[0] != `"4"` {
		t.Fatalf("%v %v", e.api.responds, e.api.respHdr)
	}
	got := e.cardOf(c.MessageID)
	if got.Text != "✅ Answered · Which one?" || got.ReplyMarkup != nil && len(got.ReplyMarkup.InlineKeyboard) != 0 {
		t.Fatalf("card after the answer: %q %+v", got.Text, got.ReplyMarkup)
	}
	if e.messageCount() != n {
		t.Fatalf("%d new messages, want none (one message per item)", e.messageCount()-n)
	}
	e.tapData(find(c, "red"), c)
	if e.responded() != 1 || e.lastAnswer() != TextCardSettled {
		t.Fatalf("second tap: %d responds, answer %q", e.responded(), e.lastAnswer())
	}
}

// A card button acts only for the link the card was sent to, and only on the action the card is: anything else is
// refused before the API is called.
func TestCardTapIsTheTappingLinksOwn(t *testing.T) {
	e := newEnv(t)
	reg := e.withCards()
	a := e.addHuman(spA, hid('1'), "message", "FYI", `{"requires_ack":true}`)
	b := e.addHuman(spA, hid('2'), "message", "Other", `{"requires_ack":true}`)
	theirs := e.postCard(reg, a, "tgl_other")
	e.tapData(find(theirs, "Acknowledge"), theirs)
	if e.responded() != 0 || e.lastAnswer() != TextUnavailable {
		t.Fatalf("another link's card: %d responds, %q", e.responded(), e.lastAnswer())
	}
	mine := e.postCard(reg, a, "tgl_1")
	e.tapData(humanData('m', spA, b.ID, -1), mine) // the data names another action than the card's
	if e.responded() != 0 || e.lastAnswer() != TextUnavailable {
		t.Fatalf("mismatched action: %d responds, %q", e.responded(), e.lastAnswer())
	}
	e.tapData(humanData('m', spA, a.ID, -1), botapi.Message{MessageID: 4242, Chat: botapi.Chat{ID: chatID}}) // no card at all
	if e.responded() != 0 || e.lastAnswer() != TextUnavailable {
		t.Fatalf("no card: %d responds, %q", e.responded(), e.lastAnswer())
	}
	// Without the notifier's lookup a card button does nothing.
	e.h.o.Card = nil
	e.tapData(find(mine, "Acknowledge"), mine)
	if e.responded() != 0 {
		t.Fatal("responded without a card lookup")
	}
}

// A hidden button protects nothing and a shown one permits nothing: a viewer's forged tap reaches the API as the
// viewer's link and is refused there.
func TestCardButtonsAreCosmetic(t *testing.T) {
	e := newEnv(t)
	reg := e.withCards()
	a := e.addHuman(spViewer, hid('1'), "message", "FYI", `{"requires_ack":true}`)
	c := e.postCard(reg, a, "tgl_1")
	e.tapData(humanData('m', spViewer, a.ID, -1), c)
	if e.responded() != 1 || e.api.human[a.ID].Status != "open" || e.lastAnswer() != TextUnavailable {
		t.Fatalf("viewer's tap: %d responds, status %q, answer %q", e.responded(), e.api.human[a.ID].Status, e.lastAnswer())
	}
}

// A card that is behind the server acts on nothing: the person sees the current card first.
func TestCardBehindTheServerActsOnNothing(t *testing.T) {
	e := newEnv(t)
	reg := e.withCards()
	a := e.addHuman(spA, hid('1'), "todo", "Steps", `{"steps":["one"]}`)
	c := e.postCard(reg, a, "tgl_1")
	a.Version, a.Title = 5, "Steps (renamed)"
	e.tapData(find(c, "Mark completed"), c)
	if e.responded() != 0 || e.lastAnswer() != TextCardChanged {
		t.Fatalf("%d responds, %q", e.responded(), e.lastAnswer())
	}
	if got := e.cardOf(c.MessageID); !strings.Contains(got.Text, "Steps (renamed)") || find(got, "Mark completed") == "" {
		t.Fatalf("the card wasn't redrawn: %q", got.Text)
	}
	e.tapData(find(c, "Mark completed"), c)
	if e.responded() != 1 || e.api.responds[0]["outcome"] != "completed" || e.api.respHdr[0] != `"5"` {
		t.Fatalf("%v %v", e.api.responds, e.api.respHdr)
	}
}

// More… turns the card into the whole form on the same message; Back draws the card again.
func TestCardMoreAndBack(t *testing.T) {
	e := newEnv(t)
	reg := e.withCards()
	a := e.addHuman(spA, hid('1'), "question", "Which one?", `{"options":["red","blue"]}`)
	c := e.postCard(reg, a, "tgl_1")
	n := e.messageCount()
	e.tapData(find(c, "More…"), c)
	f := e.cardOf(c.MessageID)
	if !strings.Contains(f.Text, "Pick an answer") || find(f, "Claim") == "" || find(f, "‹ Back") == "" {
		t.Fatalf("form on the card: %q", f.Text)
	}
	e.tapData(find(f, "Claim"), f)
	if acts := e.acts(); len(acts) != 1 || acts[0] != "assign" || e.api.actionHdr[0] != `"4"` {
		t.Fatalf("claim from the form on the card: %v %v", acts, e.api.actionHdr)
	}
	f = e.cardOf(c.MessageID)
	if !strings.Contains(f.Text, "claimed by you") || find(f, "Release") == "" {
		t.Fatalf("form after the claim: %q", f.Text)
	}
	e.tapData(find(f, "‹ Back"), f)
	back := e.cardOf(c.MessageID)
	if find(back, "More…") == "" || find(back, "red") == "" || !strings.Contains(back.Text, "claimed by you") {
		t.Fatalf("card after Back: %q", back.Text)
	}
	if e.messageCount() != n {
		t.Fatalf("%d new messages, want none", e.messageCount()-n)
	}
	e.tapData(find(back, "blue"), back) // the card's version moved with the claim (5): the answer uses it
	if e.responded() != 1 || e.api.respHdr[0] != `"5"` {
		t.Fatalf("%v %v", e.api.responds, e.api.respHdr)
	}
}

// Approve on the card keeps the two steps: the card shows Confirm in place, Back returns to the card, and only Confirm
// decides. Confirm never acts on a card that wasn't asked first.
func TestCardApproveTakesTwoSteps(t *testing.T) {
	e := newEnv(t)
	reg := e.withCards()
	a := e.addHuman(spA, hid('1'), "approval", "Deploy?", `{"action":"deploy v2"}`)
	c := e.postCard(reg, a, "tgl_1")
	e.tapData(humanData('y', spA, a.ID, -1), c) // the second step alone
	if e.responded() != 0 || e.lastAnswer() != TextFormExpired {
		t.Fatalf("confirm without approve: %d, %q", e.responded(), e.lastAnswer())
	}
	e.tapData(find(c, "Approve"), c)
	step := e.cardOf(c.MessageID)
	if e.responded() != 0 || find(step, "Confirm approve") == "" {
		t.Fatalf("approve step: %d responds, %q", e.responded(), step.Text)
	}
	e.tapData(find(step, "Back"), step)
	if back := e.cardOf(c.MessageID); find(back, "Approve") == "" || find(back, "More…") == "" {
		t.Fatalf("Back didn't return to the card: %q", back.Text)
	}
	e.tapData(find(c, "Approve"), c)
	step = e.cardOf(c.MessageID)
	e.tapData(find(step, "Confirm approve"), step)
	if e.responded() != 1 || e.api.responds[0]["outcome"] != "approved" {
		t.Fatalf("%v", e.api.responds)
	}
	if got := e.cardOf(c.MessageID); got.Text != "✅ Approved · Deploy?" {
		t.Fatalf("card after the decision: %q", got.Text)
	}
}

// An enforced approval in a web-only space can't be decided from its card, even with forged buttons.
func TestCardEnforcedApprovalStaysWebOnly(t *testing.T) {
	e := newEnv(t)
	reg := e.withCards()
	a := e.addHuman(spA, hid('1'), "approval", "Merge?", `{"action":"merge"}`)
	e.api.enforced[a.ID] = true
	c := e.postCard(reg, a, "tgl_1")
	for _, v := range []byte{'a', 'd'} {
		e.tapData(humanData(v, spA, a.ID, -1), c)
		if e.responded() != 0 || len(e.arms) != 0 || e.lastAnswer() != TextUnavailable {
			t.Fatalf("forged %c: %d responds, %d arms, %q", v, e.responded(), len(e.arms), e.lastAnswer())
		}
	}
	if strings.Contains(e.cardOf(c.MessageID).Text, "SECRET") {
		t.Fatal("arguments shown")
	}
}

// Deny from the card asks for a note with a prompt (a ForceReply, not a form) and collapses the card once answered.
func TestCardDenyWithNote(t *testing.T) {
	e := newEnv(t)
	reg := e.withCards()
	a := e.addHuman(spA, hid('1'), "approval", "Deploy?", `{"action":"deploy v2"}`)
	c := e.postCard(reg, a, "tgl_1")
	e.tapData(find(c, "Deny"), c)
	pr := e.last()
	if !strings.Contains(pr.Text, "Why are you denying") {
		t.Fatalf("prompt %q", pr.Text)
	}
	e.reply("not now", pr.MessageID, chatID, e.l)
	if e.responded() != 1 || e.api.responds[0]["outcome"] != "denied" || e.api.responds[0]["note"] != "not now" {
		t.Fatalf("%v", e.api.responds)
	}
	if got := e.cardOf(c.MessageID); got.Text != "✅ Denied · Deploy?" && got.Text != "❌ Denied · Deploy?" {
		t.Fatalf("card after the denial: %q", got.Text)
	}
}

// A typed answer from the card: the prompt is bound to the card's form; the answer collapses the card.
func TestCardTypedAnswer(t *testing.T) {
	e := newEnv(t)
	reg := e.withCards()
	a := e.addHuman(spA, hid('1'), "question", "Name it", `{"options":["x"],"allow_text":true}`)
	c := e.postCard(reg, a, "tgl_1")
	e.tapData(find(c, "Answer"), c)
	pr := e.last()
	e.reply("my name", pr.MessageID, chatID, e.l)
	if e.responded() != 1 || e.api.responds[0]["answer"] != "my name" {
		t.Fatalf("%v", e.api.responds)
	}
	if got := e.cardOf(c.MessageID); got.Text != "✅ Answered · Name it" {
		t.Fatalf("card: %q", got.Text)
	}
}

// When the notifier already drew a newer state (here: settled) on the card, the chat's collapse doesn't overwrite it,
// and a form on a card the notifier moved past is dropped.
func TestCardNotifierStateWins(t *testing.T) {
	e := newEnv(t)
	reg := e.withCards()
	a := e.addHuman(spA, hid('1'), "message", "FYI", `{"requires_ack":true}`)
	c := e.postCard(reg, a, "tgl_1")
	e.tapData(find(c, "More…"), c)
	// The notifier moves the card on (another person acknowledged it elsewhere): version 6, settled.
	reg.mu.Lock()
	ref := reg.refs[c.MessageID]
	ref.Version = 6
	reg.refs[c.MessageID], reg.final[c.MessageID] = ref, true
	reg.mu.Unlock()
	a.Status, a.Outcome, a.Version = "resolved", "acknowledged", 6
	f := e.cardOf(c.MessageID)
	e.tapData(find(f, "Claim"), f) // a tap on the stale form
	if acts := e.acts(); len(acts) != 0 {
		t.Fatalf("acted from a stale form: %v", acts)
	}
	if e.lastAnswer() != TextFormExpired {
		t.Fatalf("answer %q", e.lastAnswer())
	}
	// A card verb on the settled card: nothing acts, and the chat's redraw of version 6 settled is allowed but the
	// notifier's own state stays authoritative (the registry refuses an older or live edit).
	before := len(reg.edits)
	e.tapData(humanData('m', spA, a.ID, -1), c)
	if e.responded() != 0 || e.lastAnswer() != TextCardSettled {
		t.Fatalf("%d %q", e.responded(), e.lastAnswer())
	}
	for _, ed := range reg.edits[before:] {
		if !strings.HasSuffix(ed, "final") {
			t.Fatalf("a live edit ran over the settled card: %v", reg.edits[before:])
		}
	}
}

// Groups never adopt a card message (the notifier sends no cards there).
func TestCardTapInGroupRefused(t *testing.T) {
	e := newEnv(t)
	reg := e.withCards()
	a := e.addHuman(spA, hid('1'), "message", "FYI", `{"requires_ack":true}`)
	c := e.postCard(reg, a, "tgl_1")
	l := e.l
	l.Group = &updates.Group{ChatID: -100, SpaceID: spA}
	q := &botapi.CallbackQuery{ID: "cbg", From: botapi.User{ID: chatID}, Message: &c, Data: find(c, "Acknowledge")}
	c2 := caller{h: e.h, p: e.h.principal(context.Background(), l)}
	e.h.cardTap(context.Background(), q, l, c2, spaceRef{ID: spA}, a.ID, 'm', -1)
	if e.responded() != 0 {
		t.Fatal("a group tap adopted a card")
	}
}

// A quick action that leaves its form behind (here: the typed-answer prompt is pending) doesn't break the card's
// other buttons: More… starts over from the card.
func TestCardMoreAfterAPendingPrompt(t *testing.T) {
	e := newEnv(t)
	reg := e.withCards()
	a := e.addHuman(spA, hid('1'), "question", "Name it", `{"options":["x"],"allow_text":true}`)
	c := e.postCard(reg, a, "tgl_1")
	e.tapData(find(c, "Answer"), c)
	e.tapData(find(c, "More…"), c)
	if f := e.cardOf(c.MessageID); find(f, "‹ Back") == "" || find(f, "Claim") == "" {
		t.Fatalf("More… after a prompt: %q (answer %q)", f.Text, e.lastAnswer())
	}
}
