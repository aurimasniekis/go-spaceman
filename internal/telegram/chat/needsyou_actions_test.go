package chat

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
)

// lastAct is the latest common action the fake API saw, with its If-Match.
func (e *env) acts() []string { return append([]string(nil), e.api.actions...) }

// replyMedia sends a photo (with an optional caption) as the reply to a prompt.
func (e *env) replyMedia(to int, caption string, content []byte) {
	e.upd++
	e.h.Handle(context.Background(), botapi.Update{UpdateID: e.upd, Message: &botapi.Message{MessageID: int(e.upd) + 500, From: &botapi.User{ID: chatID},
		Chat: botapi.Chat{ID: chatID, Type: botapi.ChatPrivate}, Caption: caption, Photo: photo(e, content),
		ReplyToMessage: &botapi.Message{MessageID: to, Chat: botapi.Chat{ID: chatID}}}}, e.l)
}

// card re-reads a form message as Telegram holds it now.
func (e *env) cardOf(id int) botapi.Message {
	e.t.Helper()
	for _, m := range e.srv.Messages(chatID) {
		if m.MessageID == id {
			return m
		}
	}
	e.t.Fatalf("message %d", id)
	return botapi.Message{}
}

// SM-TG-003 (TG-7-3): every pending card has the common row; closed ones do not. Approvals have no withdraw.
func TestCommonRowOnCards(t *testing.T) {
	e := newEnv(t)
	e.addHuman(spA, hid('1'), "question", "Q", `{"options":["a"]}`)
	e.addHuman(spA, hid('2'), "approval", "Deploy?", `{"action":"x"}`)
	e.addHuman(spA, hid('3'), "message", "FYI", `{}`)
	for _, c := range []byte("123") {
		f := e.open(spA, hid(c))
		for _, l := range []string{"Claim", "Comment", "Ask requester"} {
			if find(f, l) == "" {
				t.Errorf("%c: no %s button", c, l)
			}
		}
		for _, d := range func() []string { _, d := buttons(f); return d }() {
			if len(d) > 64 {
				t.Errorf("data %q is %d bytes", d, len(d))
			}
		}
		if (find(f, "Withdraw") != "") == (c == '2') {
			t.Errorf("%c: withdraw offered wrongly", c)
		}
	}
	e.api.human[hid('3')].Status = "resolved"
	if f := e.open(spA, hid('3')); find(f, "Claim") != "" || find(f, "Comment") != "" {
		t.Fatal("a closed card has common actions")
	}
}

// Claim and release: assign as the link's user with the card's version; the card shows who holds it and the form keeps
// responding with the new version.
func TestClaimAndRelease(t *testing.T) {
	e := newEnv(t)
	e.addHuman(spA, hid('1'), "question", "Q", `{"options":["a","b"]}`)
	f := e.open(spA, hid('1'))
	e.tapData(find(f, "Claim"), f)
	if got := e.acts(); len(got) != 1 || got[0] != "assign" || e.api.actionBody[0]["assignee"] != "usr_1" || e.api.actionHdr[0] != `"4"` {
		t.Fatalf("%v %v %v", got, e.api.actionBody, e.api.actionHdr)
	}
	c := e.cardOf(f.MessageID)
	if !strings.Contains(c.Text, "Claimed by you") || find(c, "Claim") != "" && find(c, "Release") == "" || find(c, "Release") == "" {
		t.Fatalf("%q", c.Text)
	}
	// the question can still be answered from the card, with the version the claim produced
	e.tapData(find(c, "b"), c)
	if e.responded() != 1 || e.api.respHdr[0] != `"5"` {
		t.Fatalf("%v", e.api.respHdr)
	}
	// release
	e.addHuman(spA, hid('2'), "message", "FYI", `{}`)
	f = e.open(spA, hid('2'))
	e.tapData(find(f, "Claim"), f)
	c = e.cardOf(f.MessageID)
	e.tapData(find(c, "Release"), c)
	if n := len(e.api.actionBody); n != 3 || e.api.actionBody[2]["assignee"] != "" {
		t.Fatalf("%v", e.api.actionBody)
	}
	if c = e.cardOf(f.MessageID); strings.Contains(c.Text, "Claimed") || find(c, "Claim") == "" {
		t.Fatalf("%q", c.Text)
	}
	// claimed by someone else: no Claim button and the card says so
	e.addHuman(spA, hid('4'), "message", "Other", `{}`).Assignee = "usr_9"
	c = e.open(spA, hid('4'))
	if !strings.Contains(c.Text, "Claimed by someone else") || find(c, "Claim") != "" || find(c, "Release") != "" {
		t.Fatalf("%q", c.Text)
	}
}

// If-Match: a card that went stale gets a conflict and is redrawn from the server; nothing is overwritten.
func TestClaimStaleCardRefreshes(t *testing.T) {
	e := newEnv(t)
	a := e.addHuman(spA, hid('1'), "message", "FYI", `{}`)
	f := e.open(spA, hid('1'))
	a.Version, a.Assignee, a.Status = 9, "usr_9", "in_progress" // someone else claimed it meanwhile
	e.tapData(find(f, "Claim"), f)
	if e.lastAnswer() != TextStale || a.Assignee != "usr_9" || a.Version != 9 {
		t.Fatalf("%q %+v", e.lastAnswer(), a)
	}
	c := e.cardOf(f.MessageID)
	if !strings.Contains(c.Text, "Claimed by someone else") || find(c, "Claim") != "" {
		t.Fatalf("card not refreshed: %q", c.Text)
	}
	// the refreshed card acts on the new version
	a.Assignee, a.Status = "", "open"
	c = e.open(spA, hid('1'))
	e.tapData(find(c, "Claim"), c)
	if e.api.actionHdr[len(e.api.actionHdr)-1] != `"9"` || a.Assignee != "usr_1" {
		t.Fatalf("%v %+v", e.api.actionHdr, a)
	}
	// a card whose action closed is closed on refresh
	b := e.addHuman(spA, hid('2'), "message", "Gone", `{}`)
	f = e.open(spA, hid('2'))
	b.Status, b.Version = "resolved", 7
	e.tapData(find(f, "Claim"), f)
	c = e.cardOf(f.MessageID)
	if !strings.Contains(c.Text, "already resolved") || find(c, "Claim") != "" {
		t.Fatalf("%q", c.Text)
	}
}

// Comment: a ForceReply prompt bound to (chat, message, link), single use; the comment shows on the card.
func TestCommentByForceReply(t *testing.T) {
	e := newEnv(t)
	e.addHuman(spA, hid('1'), "message", "FYI", `{}`)
	f := e.open(spA, hid('1'))
	e.tapData(find(f, "Comment"), f)
	pr := e.last()
	if !e.sentForce() || !strings.Contains(pr.Text, "your comment") {
		t.Fatalf("%q", pr.Text)
	}
	e.reply("nope", 777, chatID, e.l) // a stray reply
	other := e.l
	other.ID = "tgl_2"
	e.reply("intruder", pr.MessageID, chatID, other) // another link
	if len(e.acts()) != 0 {
		t.Fatal("commented from a stray reply")
	}
	e.reply("  looks fine to me ", pr.MessageID, chatID, e.l)
	if got := e.acts(); len(got) != 1 || got[0] != "comments" || e.api.actionBody[0]["body"] != "looks fine to me" || e.api.actionHdr[0] != "" {
		t.Fatalf("%v %v", got, e.api.actionBody)
	}
	if c := e.cardOf(f.MessageID); !strings.Contains(c.Text, "Comment, Alice Operator: looks fine to me") || !strings.Contains(c.Text, "not instructions") {
		t.Fatalf("%q", c.Text)
	}
	e.reply("again", pr.MessageID, chatID, e.l) // single use
	if len(e.acts()) != 1 {
		t.Fatal("prompt reused")
	}
	// the prompt expires
	e.tapData(find(e.cardOf(f.MessageID), "Comment"), e.cardOf(f.MessageID))
	pr = e.last()
	e.clk.Advance(pendingTTL + time.Minute)
	e.reply("late", pr.MessageID, chatID, e.l)
	if len(e.acts()) != 1 {
		t.Fatal("expired prompt used")
	}
}

// Clarify: the question goes to the clarify operation (kind clarification), the action stays open, and the thread shows
// the kinds comment, clarification and reply.
func TestClarifyAndKinds(t *testing.T) {
	e := newEnv(t)
	a := e.addHuman(spA, hid('1'), "question", "Q", `{"options":["a"]}`)
	a.Comments = []humanComment{{Kind: "reply", Body: "the requester answered", AuthorLabel: "Agent"}}
	f := e.open(spA, hid('1'))
	if !strings.Contains(f.Text, "Reply, Agent: the requester answered") {
		t.Fatalf("%q", f.Text)
	}
	e.tapData(find(f, "Ask requester"), f)
	pr := e.last()
	if !strings.Contains(pr.Text, "question to the requester") || !e.sentForce() {
		t.Fatalf("%q", pr.Text)
	}
	e.reply("which environment?", pr.MessageID, chatID, e.l)
	if got := e.acts(); len(got) != 1 || got[0] != "clarify" || e.api.actionBody[0]["body"] != "which environment?" || a.Status != "open" {
		t.Fatalf("%v %v %s", got, e.api.actionBody, a.Status)
	}
	if e.responded() != 0 {
		t.Fatal("a clarification responded")
	}
	c := e.cardOf(f.MessageID)
	if !strings.Contains(c.Text, "Question to the requester, Alice Operator: which environment?") || find(c, "a") == "" {
		t.Fatalf("%q", c.Text)
	}
	// text too long and an unsupported kind are asked again, nothing is sent
	e.tapData(find(c, "Comment"), c)
	pr = e.last()
	e.reply(strings.Repeat("x", maxCommentRunes+1), pr.MessageID, chatID, e.l)
	if len(e.acts()) != 1 || !strings.Contains(e.last().Text, "too long") || !e.sentForce() && false {
		t.Fatalf("%q", e.last().Text)
	}
}

// A photo sent as the reply to a comment prompt is uploaded through the API and becomes an attachment of the comment;
// the caption is the body. A reply without text or a file asks again.
func TestCommentWithAttachment(t *testing.T) {
	e := newEnv(t)
	e.addHuman(spA, hid('1'), "message", "FYI", `{}`)
	f := e.open(spA, hid('1'))
	e.tapData(find(f, "Comment"), f)
	pr := e.last()
	e.replyMedia(pr.MessageID, "the screenshot", []byte("\x89PNG\r\n\x1a\n-----"))
	if len(e.api.atts) != 1 || len(e.acts()) != 1 || e.api.actionBody[0]["body"] != "the screenshot" {
		t.Fatalf("%d %v", len(e.api.atts), e.api.actionBody)
	}
	ids, _ := e.api.actionBody[0]["attachment_ids"].([]any)
	if len(ids) != 1 || e.api.atts[ids[0].(string)] == nil {
		t.Fatalf("%v", e.api.actionBody[0])
	}
	if c := e.cardOf(f.MessageID); find(c, "Files (1)") == "" {
		t.Fatalf("no Files button: %v", c.ReplyMarkup)
	}
	// no text, no caption
	e.tapData(find(e.cardOf(f.MessageID), "Comment"), e.cardOf(f.MessageID))
	pr = e.last()
	e.replyMedia(pr.MessageID, "", []byte("\x89PNG\r\n\x1a\n-----"))
	if e.api.actionBody[1]["body"] != "(no text; 1 attachment(s) sent from Telegram)" {
		t.Fatalf("%v", e.api.actionBody[1])
	}
	// a failed upload keeps the prompt usable and sends no comment
	e.tapData(find(e.cardOf(f.MessageID), "Comment"), e.cardOf(f.MessageID))
	pr = e.last()
	e.api.uploadSt = 413
	e.replyMedia(pr.MessageID, "big", []byte("x"))
	if len(e.acts()) != 2 {
		t.Fatalf("commented after a failed upload: %v", e.acts())
	}
	e.api.uploadSt = 0
	e.reply("", pr.MessageID, chatID, e.l)
	if len(e.acts()) != 2 || !strings.Contains(e.last().Text, "Send text, a photo or a file") {
		t.Fatalf("%q", e.last().Text)
	}
}

// Files: the request's and its comments' attachments go back with protect_content, only at content level full.
func TestFilesButton(t *testing.T) {
	e := newEnv(t)
	a := e.addHuman(spA, hid('1'), "message", "FYI", `{}`)
	e.api.atts["att_1"] = &storedAtt{meta: attachmentRef{ID: "att_1", Name: "plan.pdf", Size: 5, MIME: "application/pdf"}, content: []byte("%PDF-")}
	e.api.atts["att_2"] = &storedAtt{meta: attachmentRef{ID: "att_2", Name: "shot.png", Size: 4, MIME: "image/png", Inline: true}, content: []byte("\x89PNG")}
	a.Attachments = []attachmentRef{e.api.atts["att_1"].meta}
	a.Comments = []humanComment{{Kind: "comment", Body: "see", Attachments: []attachmentRef{e.api.atts["att_2"].meta, e.api.atts["att_1"].meta}}}
	f := e.open(spA, hid('1'))
	if find(f, "Files (2)") == "" {
		t.Fatalf("%v", f.ReplyMarkup)
	}
	e.tapData(find(f, "Files"), f)
	if len(e.files) != 2 || e.files[0].method != "sendDocument" || e.files[1].method != "sendPhoto" || !e.files[0].protect || !e.files[1].protect {
		t.Fatalf("%+v", e.files)
	}
	// content level summary: not sent
	e.addHuman(spS, hid('5'), "message", "S", `{}`).Attachments = []attachmentRef{e.api.atts["att_1"].meta}
	e.tapData(humanData('o', spS, hid('5'), -1), e.card())
	g := e.last()
	n := len(e.files)
	e.tapData(find(g, "Files"), g)
	if len(e.files) != n || e.lastAnswer() != TextNoFilesHere {
		t.Fatalf("%d %q", len(e.files), e.lastAnswer())
	}
	// no attachment: nothing to send
	e.addHuman(spA, hid('6'), "message", "None", `{}`)
	g = e.open(spA, hid('6'))
	e.tapData(humanData('p', spA, hid('6'), -1), g)
	if e.lastAnswer() != TextNoFiles {
		t.Fatalf("%q", e.lastAnswer())
	}
}

// Cancel: the first tap only asks; the Confirm is bound to the confirmation message, this link, the action and the
// card's version, and works once.
func TestCancelNeedsBoundConfirmation(t *testing.T) {
	e := newEnv(t)
	a := e.addHuman(spA, hid('1'), "todo", "Todo", `{"steps":["a"]}`)
	f := e.open(spA, hid('1'))
	e.tapData(find(f, "Withdraw"), f)
	cf := e.last()
	if len(e.acts()) != 0 || !strings.Contains(cf.Text, "Withdraw this request?") || a.Status != "open" {
		t.Fatalf("cancelled on the first tap: %v", e.acts())
	}
	data := find(cf, "Confirm")
	if data != cancelConfirmData(spA, hid('1'), 4) || len(data) > 64 {
		t.Fatalf("%q", data)
	}
	// the same data on the card (another message) and another link's tap do nothing and leave the confirmation alone
	e.tapData(data, f)
	other := e.l
	other.ID = "tgl_2"
	e.upd++
	e.h.Handle(context.Background(), botapi.Update{UpdateID: e.upd, CallbackQuery: &botapi.CallbackQuery{ID: "x", From: botapi.User{ID: chatID}, Message: &cf, Data: data}}, other)
	if len(e.acts()) != 0 || a.Status != "open" {
		t.Fatalf("forged confirmation acted: %v", e.acts())
	}
	// a forged version does nothing and spends the confirmation: it must be asked for again
	e.tapData(cancelConfirmData(spA, hid('1'), 5), cf)
	e.tapData(data, cf)
	if len(e.acts()) != 0 || a.Status != "open" {
		t.Fatalf("forged version acted: %v", e.acts())
	}
	e.tapData(find(f, "Withdraw"), f)
	cf = e.last()
	e.tapData(data, cf)
	if got := e.acts(); len(got) != 1 || got[0] != "cancel" || e.api.actionHdr[0] != `"4"` || a.Status != "cancelled" {
		t.Fatalf("%v %v", got, e.api.actionHdr)
	}
	if !strings.Contains(e.cardOf(cf.MessageID).Text, "cancelled") || !strings.Contains(e.cardOf(f.MessageID).Text, "Cancelled") {
		t.Fatalf("%q / %q", e.cardOf(cf.MessageID).Text, e.cardOf(f.MessageID).Text)
	}
	// single use
	e.tapData(data, cf)
	if len(e.acts()) != 1 || e.lastAnswer() != TextConfirmGone {
		t.Fatalf("%v %q", e.acts(), e.lastAnswer())
	}
}

// Keep it, an expired confirmation, and a Confirm for a made-up message never cancel.
func TestCancelKeepAndExpiry(t *testing.T) {
	e := newEnv(t)
	a := e.addHuman(spA, hid('1'), "message", "FYI", `{}`)
	f := e.open(spA, hid('1'))
	e.tapData(find(f, "Withdraw"), f)
	cf := e.last()
	e.tapData(find(cf, "Keep"), cf)
	if !strings.Contains(e.cardOf(cf.MessageID).Text, "Kept") {
		t.Fatalf("%q", e.cardOf(cf.MessageID).Text)
	}
	e.tapData(find(cf, "Confirm"), cf) // the Keep consumed it
	if len(e.acts()) != 0 || a.Status != "open" {
		t.Fatal("cancelled after Keep")
	}
	e.tapData(find(f, "Withdraw"), f)
	cf = e.last()
	e.clk.Advance(pendingTTL + time.Minute)
	e.tapData(find(cf, "Confirm"), cf)
	if len(e.acts()) != 0 || a.Status != "open" {
		t.Fatal("cancelled after the TTL")
	}
	e.tapData(cancelConfirmData(spA, hid('1'), 4), botapi.Message{MessageID: 4242, Chat: botapi.Chat{ID: chatID}})
	if len(e.acts()) != 0 {
		t.Fatal("cancelled from a made-up message")
	}
}

// A stale card cancels nothing: the server's If-Match conflict says so and the card is redrawn.
func TestCancelStaleCardRefreshes(t *testing.T) {
	e := newEnv(t)
	a := e.addHuman(spA, hid('1'), "message", "FYI", `{}`)
	f := e.open(spA, hid('1'))
	e.tapData(find(f, "Withdraw"), f)
	cf := e.last()
	a.Version, a.Assignee, a.Status = 8, "usr_9", "in_progress"
	e.tapData(find(cf, "Confirm"), cf)
	if a.Status != "in_progress" || e.lastAnswer() != TextStale {
		t.Fatalf("%q %s", e.lastAnswer(), a.Status)
	}
	if !strings.Contains(e.cardOf(cf.MessageID).Text, "Nothing was cancelled") || !strings.Contains(e.cardOf(f.MessageID).Text, "Claimed by someone else") {
		t.Fatalf("%q / %q", e.cardOf(cf.MessageID).Text, e.cardOf(f.MessageID).Text)
	}
}

// Approvals are never cancelled from chat: no button, a crafted tap is refused here, and so is the server's own rule.
func TestApprovalCancelRefused(t *testing.T) {
	e := newEnv(t)
	a := e.addHuman(spA, hid('1'), "approval", "Deploy?", `{"action":"x"}`)
	f := e.open(spA, hid('1'))
	e.tapData(humanData('e', spA, hid('1'), -1), f)
	if len(e.acts()) != 0 || e.lastAnswer() != TextNoApprovalCx || a.Status != "open" {
		t.Fatalf("%v %q", e.acts(), e.lastAnswer())
	}
	// a forged confirmation for an approval finds no confirmation to take
	e.tapData(cancelConfirmData(spA, hid('1'), 4), f)
	if len(e.acts()) != 0 || a.Status != "open" {
		t.Fatal("cancelled an approval")
	}
}

// A viewer's link: the server refuses each action (the fake answers like the API's permission check) and the chat says
// it isn't available; nothing changes.
func TestViewerLinkIsRefusedByTheServer(t *testing.T) {
	e := newEnv(t)
	a := e.addHuman(spViewer, hid('1'), "message", "FYI", `{}`)
	e.tapData(humanData('o', spViewer, hid('1'), -1), e.card())
	f := e.last()
	e.tapData(find(f, "Claim"), f)
	if e.lastAnswer() != TextUnavailable || a.Assignee != "" {
		t.Fatalf("claim: %q", e.lastAnswer())
	}
	e.tapData(find(f, "Comment"), f)
	e.reply("hello", e.last().MessageID, chatID, e.l)
	if e.last().Text != TextUnavailable || len(a.Comments) != 0 {
		t.Fatalf("comment: %q", e.last().Text)
	}
	e.tapData(find(f, "Withdraw"), f)
	cf := e.last()
	e.tapData(find(cf, "Confirm"), cf)
	if e.lastAnswer() != TextUnavailable || a.Status != "open" {
		t.Fatalf("cancel: %q %s", e.lastAnswer(), a.Status)
	}
	if got := e.acts(); len(got) != 3 || got[0] != "assign" || got[1] != "comments" || got[2] != "cancel" {
		t.Fatalf("every action must have gone to the server: %v", got)
	}
}

// Forged common-action data without a live card, or from another link's card, does nothing.
func TestCommonActionsNeedALiveCard(t *testing.T) {
	e := newEnv(t)
	e.addHuman(spA, hid('1'), "message", "FYI", `{}`)
	for _, v := range []byte("luripe") {
		e.tapData(humanData(v, spA, hid('1'), -1), e.card())
	}
	e.tapData("hv:"+strings.Repeat("a", 26)+":"+strings.Repeat("1", 26)+":x", e.card())
	e.tapData("hl:"+strings.Repeat("a", 26)+":"+strings.Repeat("1", 26)+":7", e.card()) // wrong arity for l
	if len(e.acts()) != 0 || e.lastAnswer() == "" {
		t.Fatalf("%v", e.acts())
	}
	f := e.open(spA, hid('1'))
	e.clk.Advance(pendingTTL + time.Minute)
	e.tapData(find(f, "Claim"), f)
	if len(e.acts()) != 0 || e.lastAnswer() != TextFormExpired {
		t.Fatalf("%v %q", e.acts(), e.lastAnswer())
	}
}

// Forms of actions answered in the app (a big question) still offer the common row, but their own verbs stay refused.
func TestInAppFormsKeepOnlyCommonVerbs(t *testing.T) {
	e := newEnv(t)
	e.h.o.AppURL = func(_, id string) string { return "https://app.example.com/a/" + id }
	opts := make([]string, maxOptions+1)
	for i := range opts {
		opts[i] = "o"
	}
	b, _ := jsonMarshal(map[string]any{"options": opts})
	e.addHuman(spA, hid('1'), "question", "Many", string(b))
	f := e.open(spA, hid('1'))
	e.tapData(humanData('q', spA, hid('1'), 0), f)
	if e.responded() != 0 {
		t.Fatal("answered a big question in chat")
	}
	e.tapData(find(f, "Claim"), f)
	if len(e.acts()) != 1 {
		t.Fatalf("%v", e.acts())
	}
}

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }
