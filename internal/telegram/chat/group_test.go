package chat

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/updates"
)

// groupID is a bound forum supergroup (negative, like every Telegram group ID).
const groupID = -100777

// member is the link of a group member as the dispatcher hands it over: the member's own link, the group's chat.
func member(linkID, space string) updates.Link {
	return updates.Link{ID: linkID, UserID: "usr_1", ChatID: groupID, Group: &updates.Group{ChatID: groupID, SpaceID: space}}
}

func (e *env) gsay(l updates.Link, text string) {
	e.upd++
	e.h.Handle(context.Background(), botapi.Update{UpdateID: e.upd, Message: &botapi.Message{MessageID: int(e.upd), From: &botapi.User{ID: 900},
		Chat: botapi.Chat{ID: groupID, Type: botapi.ChatSupergroup}, Text: text}}, l)
}

func (e *env) gtap(l updates.Link, data string, on botapi.Message) {
	e.upd++
	u := botapi.Update{UpdateID: e.upd, CallbackQuery: &botapi.CallbackQuery{ID: fmt.Sprintf("cb%d", e.upd), From: botapi.User{ID: 900}, Message: &on, Data: data}}
	e.srv.PushUpdate(u)
	e.h.Handle(context.Background(), u, l)
}

func (e *env) group() []botapi.Message { return e.srv.Messages(groupID) }

func (e *env) glast() botapi.Message {
	e.t.Helper()
	ms := e.group()
	if len(ms) == 0 {
		e.t.Fatal("nothing sent to the group")
	}
	return ms[len(ms)-1]
}

// groupCard is a notifier card in the group.
func groupCard() botapi.Message {
	return botapi.Message{MessageID: 9100, Chat: botapi.Chat{ID: groupID, Type: botapi.ChatSupergroup}}
}

// SM-TG-002 (TG-13-1, group mode): a member whose own link no longer acts, or whose link can't read the bound space,
// is refused on every tap and command, and nothing of the space is read or sent for them: the group grants nothing.
func TestGroupUnauthorizedTapperRefused(t *testing.T) {
	e := newEnv(t)
	e.addHuman(spA, hid('1'), "question", "Which one?", `{"options":["red","blue"]}`)
	e.addHuman(spD, hid('2'), "question", "Hidden?", `{"options":["red","blue"]}`)

	// A revoked link (or a disabled user): no principal.
	e.principalErr = errors.New("revoked")
	gone := member("tgl_gone", spA)
	e.gtap(gone, humanData('o', spA, hid('1'), -1), groupCard())
	if e.lastAnswer() != TextGroupNoAccess {
		t.Fatalf("revoked link answered %q", e.lastAnswer())
	}
	e.gsay(gone, "/needsyou")
	if len(e.api.seen()) != 0 || len(e.group()) != 0 {
		t.Fatalf("a revoked link reached the API %v or the group %d", e.api.seen(), len(e.group()))
	}
	e.principalErr = nil

	// A link without a grant on the bound space (the API forbids spD): only the space check is made.
	nogrant := member("tgl_nogrant", spD)
	e.gtap(nogrant, humanData('o', spD, hid('2'), -1), groupCard())
	if e.lastAnswer() != TextGroupNoAccess {
		t.Fatalf("no-grant tap answered %q", e.lastAnswer())
	}
	e.gsay(nogrant, "/needsyou")
	e.gsay(nogrant, "/inbox")
	for _, r := range e.api.seen() {
		if r != "GET /api/v1/spaces/"+spD {
			t.Fatalf("a link without a grant got past the space check: %v", e.api.seen())
		}
	}
	if len(e.group()) != 0 || e.touched(hid('2')) {
		t.Fatalf("sent %d messages to the group", len(e.group()))
	}
}

func (e *env) touched(id string) bool { return e.api.touched(id) }

// In a group only the bound space exists: a card or button naming another space, a /command naming one, and the space
// switch are refused; the link's own chosen space is never used.
func TestGroupPinnedToBoundSpace(t *testing.T) {
	e := newEnv(t)
	e.addHuman(spS, hid('1'), "question", "Other space?", `{"options":["red","blue"]}`)
	e.say("/space sum") // the member's private choice must not leak into the group, nor the group's into it
	m := member("tgl_1", spA)
	e.gtap(m, humanData('o', spS, hid('1'), -1), groupCard())
	if e.lastAnswer() != TextUnavailable || e.touched(hid('1')) {
		t.Fatalf("another space's action: %q", e.lastAnswer())
	}
	e.gtap(m, "sp:"+spS, groupCard())
	if e.lastAnswer() != TextGroupFixed {
		t.Fatalf("space switch answered %q", e.lastAnswer())
	}
	e.gsay(m, "/spaces")
	e.gsay(m, "/space sum")
	if len(e.group()) != 0 {
		t.Fatalf("/spaces or /space answered in the group: %q", e.glast().Text)
	}
	e.gsay(m, "/inbox sum")
	if e.glast().Text != TextUnavailable {
		t.Fatalf("/inbox of another space: %q", e.glast().Text)
	}
	e.gsay(m, "/inbox")
	if got, _ := buttons(e.glast()); !strings.Contains(strings.Join(got, "|"), "First item") || strings.Contains(strings.Join(got, "|"), "Summary item") {
		t.Fatalf("the group's inbox isn't the bound space's: %v", got)
	}
	if cur := e.h.currentSpace("tgl_1"); cur != spS {
		t.Fatalf("the group changed the member's private space to %s", cur)
	}
}

// SM-TG-002: nothing above summary reaches a group. The bound space's content level is full, and the same reads in
// the private chat do show bodies; in the group, the item's body, the request's body and attachments never go out.
func TestGroupNothingAboveSummary(t *testing.T) {
	e := newEnv(t)
	e.addHuman(spA, hid('1'), "question", "Which one?", `{"options":["red","blue"]}`)
	m := member("tgl_1", spA)

	e.gsay(m, "/inbox")
	e.gtap(m, find(e.glast(), "First item"), e.glast())
	e.gtap(m, humanData('o', spA, hid('1'), -1), groupCard())
	var all []string
	for _, g := range e.group() {
		all = append(all, g.Text)
	}
	if j := strings.Join(all, "\n---\n"); !strings.Contains(j, "First item") || !strings.Contains(j, "Which one?") {
		t.Fatalf("the group didn't get the item card and the form:\n%s", j)
	}
	for _, g := range e.group() {
		if strings.Contains(g.Text, "body text") || strings.Contains(g.Text, "do the thing") {
			t.Fatalf("a body reached the group: %q", g.Text)
		}
	}
	if len(e.files) != 0 {
		t.Fatalf("files sent: %v", e.files)
	}
	// The same space in the member's private chat is full: the guard is the group, not the space.
	e.say("/inbox alpha")
	e.tapData(find(e.last(), "First item"), e.last())
	if f := e.open(spA, hid('1')); !strings.Contains(f.Text, "do the thing") {
		t.Fatalf("private form: %q", f.Text)
	}
	found := false
	for _, p := range e.srv.Messages(chatID) {
		found = found || strings.Contains(p.Text, "body text")
	}
	if !found {
		t.Fatal("the private chat got no body: the test doesn't compare like with like")
	}
}

// The Handler's summary rule itself: a group spaceRef is never full, whatever the space says.
func TestGroupSpaceRefNeverFull(t *testing.T) {
	e := newEnv(t)
	if !e.h.full(spaceRef{ID: spA}) {
		t.Fatal("a full space in a private chat isn't full")
	}
	if e.h.full(spaceRef{ID: spA, group: true}) {
		t.Fatal("a full space in a group is full")
	}
	ctx := withGroup(context.Background(), updates.Group{ChatID: groupID, SpaceID: spA})
	s, _, ok := e.h.space(ctx, caller{h: e.h, p: nil}, spA)
	if !ok || !s.group || e.h.full(s) {
		t.Fatalf("Handler.space in a group: %+v %v", s, ok)
	}
	if _, _, ok := e.h.space(ctx, caller{h: e.h}, spS); ok {
		t.Fatal("Handler.space admitted another space in a group")
	}
}

// Confirmations and forms in a group are bound to the member who started them: another member's taps on the same
// message neither complete nor spend them.
func TestGroupConfirmationBoundToTapper(t *testing.T) {
	e := newEnv(t)
	a := e.addHuman(spA, hid('1'), "todo", "Todo", `{"steps":["a"]}`)
	alice, bob := member("tgl_alice", spA), member("tgl_bob", spA)
	e.gtap(alice, humanData('o', spA, hid('1'), -1), groupCard())
	f := e.glast()
	e.gtap(bob, find(f, "Withdraw"), f)
	if e.lastAnswer() != TextFormExpired || len(e.acts()) != 0 {
		t.Fatalf("bob acted on alice's form: %q %v", e.lastAnswer(), e.acts())
	}
	e.gtap(alice, find(f, "Withdraw"), f)
	cf := e.glast()
	data := find(cf, "Confirm")
	if data == "" {
		t.Fatalf("no confirmation: %q", cf.Text)
	}
	e.gtap(bob, data, cf)
	if len(e.acts()) != 0 || a.Status != "open" {
		t.Fatalf("bob completed alice's confirmation: %v", e.acts())
	}
	e.gtap(alice, data, cf)
	if got := e.acts(); len(got) != 1 || got[0] != "cancel" || a.Status != "cancelled" {
		t.Fatalf("alice's confirmation didn't act after bob's tap: %v", got)
	}
}

// An enforced approval is never decided in a group, even when the space allows deciding it in chat: no server
// confirmation is armed and nothing is responded, whatever is tapped.
func TestGroupEnforcedApprovalNeverDecided(t *testing.T) {
	e := newEnv(t)
	e.apConfirm.Store(true)
	e.addHuman(spA, hid('1'), "approval", "Approve merge", `{"action":"Run mcp.github.merge"}`)
	e.api.enforced[hid('1')] = true
	m := member("tgl_1", spA)
	e.gtap(m, humanData('o', spA, hid('1'), -1), groupCard())
	f := e.glast()
	if find(f, "Approve") != "" || find(f, "Deny") != "" {
		t.Fatalf("decision buttons in the group: %v", f.ReplyMarkup)
	}
	for _, v := range []byte("adyb") {
		e.gtap(m, humanData(v, spA, hid('1'), -1), f)
	}
	if len(e.arms) != 0 || e.responded() != 0 {
		t.Fatalf("decided in a group: arms %v responds %v", e.arms, e.api.responds)
	}
	// The same approval in the member's private chat can be decided (the space opted in).
	if p := e.open(spA, hid('1')); find(p, "Approve") == "" {
		t.Fatalf("private form has no Approve: %q", p.Text)
	}
}

// A form opened in a group can't arm an approval even if its flags were otherwise set (the arm guard itself).
func TestGroupFormNeverArms(t *testing.T) {
	e := newEnv(t)
	f := &form{linkID: "tgl_1", spaceID: spA, action: hid('1'), enforced: true, inChat: true, group: true}
	if e.h.arm(f, "approved") || len(e.arms) != 0 {
		t.Fatalf("a group form armed: %v", e.arms)
	}
	f.group = false
	if !e.h.arm(f, "approved") || len(e.arms) != 1 {
		t.Fatalf("a private form didn't arm: %v", e.arms)
	}
}

// Free text in a group is never captured as an inbox item or sent to control; only replies to the bot's prompts are
// taken.
func TestGroupNoCapture(t *testing.T) {
	e := newEnv(t)
	m := member("tgl_1", spA)
	e.upd++
	e.h.Handle(context.Background(), botapi.Update{UpdateID: e.upd, Message: &botapi.Message{MessageID: int(e.upd), From: &botapi.User{ID: 900},
		Chat: botapi.Chat{ID: groupID, Type: botapi.ChatSupergroup}, Text: "just chatting",
		ReplyToMessage: &botapi.Message{MessageID: 1, Chat: botapi.Chat{ID: groupID}}}}, m)
	e.gsay(m, "/control hello")
	if len(e.api.created) != 0 || len(e.group()) != 0 {
		t.Fatalf("captured %v, sent %d", e.api.created, len(e.group()))
	}
}

// SM-TG-002: the link's private form of an action holds the body; a Withdraw tapped in the group looks up the group's
// form of the action, never the private one, so nothing of the private form is posted into the group.
func TestGroupCancelNeverShowsPrivateForm(t *testing.T) {
	e := newEnv(t)
	m := member("tgl_1", spA)
	// The forms live in a map, so which one a chat-less lookup finds is random: several actions make a missing chat
	// filter fail in one run, not only in some.
	for i := byte('1'); i <= '8'; i++ {
		e.addHuman(spA, hid(i), "todo", "Todo", `{"steps":["a"]}`)
		if p := e.open(spA, hid(i)); !strings.Contains(p.Text, "do the thing") {
			t.Fatalf("the private form has no body, so the probe proves nothing: %q", p.Text)
		}
		e.gtap(m, humanData('o', spA, hid(i), -1), groupCard())
		f := e.glast()
		e.gtap(m, find(f, "Withdraw"), f)
		cf := e.glast()
		data := find(cf, "Confirm")
		if data == "" {
			t.Fatalf("no confirmation: %q", cf.Text)
		}
		e.gtap(m, data, cf)
	}
	if got := e.acts(); len(got) != 8 {
		t.Fatalf("the withdraws didn't happen: %v", got)
	}
	for _, g := range e.group() {
		if strings.Contains(g.Text, "do the thing") {
			t.Fatalf("the private form's body reached the group: %q", g.Text)
		}
	}
	// A withdraw in the group neither found nor closed the private forms (read from the map, not through the lookup).
	e.h.mu.Lock()
	defer e.h.mu.Unlock()
	n := 0
	for k := range e.h.forms {
		if k.chat == chatID {
			n++
		}
	}
	if n != 8 {
		t.Fatalf("the group's withdraws closed private forms: %d of 8 left", n)
	}
}

// formByAction finds the form of the chat asked about, not the link's form in another chat.
func TestGroupFormByActionMatchesChat(t *testing.T) {
	e := newEnv(t)
	e.addHuman(spA, hid('1'), "todo", "Todo", `{"steps":["a"]}`)
	e.open(spA, hid('1'))
	e.gtap(member("tgl_1", spA), humanData('o', spA, hid('1'), -1), groupCard())
	for i := 0; i < 20; i++ { // the forms are in a map: a lookup that ignores the chat finds either, at random
		for _, c := range []int64{chatID, groupID} {
			if f := e.h.formByAction("tgl_1", spA, hid('1'), c); f == nil || f.key.chat != c {
				t.Fatalf("the form of chat %d: %+v", c, f)
			}
		}
	}
	if e.h.formByAction("tgl_1", spA, hid('1'), -100999) != nil {
		t.Fatal("found a form in a chat that has none")
	}
}

// Only bare commands and ones addressed to this bot are answered in a group; "/cmd@OtherBot" is another bot's.
func TestGroupIgnoresCommandsForOtherBots(t *testing.T) {
	e := newEnv(t)
	e.addHuman(spA, hid('1'), "question", "Which one?", `{"options":["a","b"]}`)
	m := member("tgl_1", spA)
	for _, c := range []string{"/needsyou@OtherBot", "/inbox@other_bot", "/needsyou@spaceman_test_botx"} {
		e.gsay(m, c)
	}
	if n := len(e.group()); n != 0 {
		t.Fatalf("answered a command for another bot: %d messages, %q", n, e.group()[0].Text)
	}
	e.gsay(m, "/needsyou@Spaceman_Test_Bot")
	if len(e.group()) == 0 {
		t.Fatal("a command addressed to our bot got no answer")
	}
	before := len(e.group())
	e.gsay(m, "/needsyou")
	if len(e.group()) <= before {
		t.Fatal("a bare command got no answer")
	}
}
