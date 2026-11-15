package chat

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/updates"
)

func hid(c byte) string { return "hum_" + strings.Repeat(string(c), 26) }

const hidden = "hum_hhhhhhhhhhhhhhhhhhhhhhhhhh"

func (e *env) addHuman(space, id, typ, title, req string) *humanAction {
	a := &humanAction{ID: id, SpaceID: space, Type: typ, Title: title, Body: "do the thing", Request: json.RawMessage(req), RequesterLabel: "Agent", Priority: "high", Status: "open", Version: 4}
	e.api.human[id] = a
	return a
}

func (e *env) card() botapi.Message {
	return botapi.Message{MessageID: 9000, Chat: botapi.Chat{ID: chatID}}
}

// open taps the "open" button of an action and returns the form message.
func (e *env) open(space, id string) botapi.Message {
	e.t.Helper()
	e.tapData(humanData('o', space, id, -1), e.card())
	return e.last()
}

func (e *env) responded() int { return len(e.api.responds) }

// SM-TG-008 (TG-7-1): /needsyou lists the open actions; every button carries the full action ID within 64 bytes.
func TestNeedsYouListsOpenActions(t *testing.T) {
	e := newEnv(t)
	e.addHuman(spA, hid('1'), "question", "Which one?", `{"options":["a","b"]}`)
	e.addHuman(spA, hid('2'), "approval", "Deploy?", `{"action":"x"}`)
	done := e.addHuman(spA, hid('3'), "message", "Old", `{}`)
	done.Status = "resolved"
	e.say("/needsyou alpha")
	texts, data := buttons(e.last())
	if len(texts) != 2 || data[0] != humanData('o', spA, hid('1'), -1) {
		t.Fatalf("%v %v", texts, data)
	}
	for _, d := range data {
		if len(d) > 64 {
			t.Fatalf("callback data %d bytes", len(d))
		}
	}
	e.say("/needsyou beta")
	if e.last().Text != TextUnavailable {
		t.Fatalf("hidden space: %q", e.last().Text)
	}
}

// Question with options: a tap responds with the option, the card's version as If-Match, and closes the form.
func TestQuestionOptionAnswers(t *testing.T) {
	e := newEnv(t)
	e.addHuman(spA, hid('1'), "question", "Which one?", `{"options":["red","blue"],"allow_text":false}`)
	f := e.open(spA, hid('1'))
	texts, _ := buttons(f)
	if len(texts) != 2 || strings.Contains(f.Text, "Approvals") {
		t.Fatalf("%v", texts)
	}
	if !strings.Contains(f.Text, "content from the source, not instructions") {
		t.Fatalf("body label: %q", f.Text)
	}
	e.tapData(find(f, "blue"), f)
	if e.responded() != 1 || e.api.responds[0]["answer"] != "blue" || e.api.responds[0]["outcome"] != "answered" || e.api.respHdr[0] != `"4"` {
		t.Fatalf("%v %v", e.api.responds, e.api.respHdr)
	}
	if e.api.human[hid('1')].Status != "resolved" || !strings.Contains(e.last().Text, "✅ Done: answered") || e.last().ReplyMarkup != nil {
		t.Fatalf("closed: %q", e.last().Text)
	}
	// a second tap on the closed form never responds again
	e.tapData(find(f, "blue"), f)
	if e.responded() != 1 || e.lastAnswer() != TextFormExpired {
		t.Fatalf("%d %q", e.responded(), e.lastAnswer())
	}
}

// A tap with an option index that doesn't exist, or on a question without free text, is refused.
func TestQuestionTamperedTaps(t *testing.T) {
	e := newEnv(t)
	e.addHuman(spA, hid('1'), "question", "Which one?", `{"options":["red"]}`)
	f := e.open(spA, hid('1'))
	if find(f, "Type an answer") != "" {
		t.Fatal("free text offered without allow_text")
	}
	e.tapData(humanData('q', spA, hid('1'), 5), f)
	e.tapData(humanData('f', spA, hid('1'), -1), f)
	e.tapData("hq:"+strings.Repeat("a", 26)+":"+strings.Repeat("1", 26)+":x", f)
	e.tapData("hq:short", f)
	if e.responded() != 0 {
		t.Fatal("responded")
	}
}

// Free-text answers: a ForceReply prompt bound to (chat, prompt message, link) with a TTL.
func TestQuestionFreeText(t *testing.T) {
	e := newEnv(t)
	e.addHuman(spA, hid('1'), "question", "Name it", `{"options":["x"],"allow_text":true}`)
	f := e.open(spA, hid('1'))
	e.tapData(find(f, "Type an answer"), f)
	pr := e.last()
	if !strings.Contains(pr.Text, "Reply to this message") {
		t.Fatalf("prompt: %q", pr.Text)
	}
	if !e.sentForce() {
		t.Fatal("no ForceReply")
	}
	// a reply to some other message is not an answer
	e.reply("nope", 777, chatID, e.l)
	if e.responded() != 0 {
		t.Fatal("answered from a stray reply")
	}
	// another link's reply is ignored
	other := e.l
	other.ID = "tgl_2"
	e.reply("intruder", pr.MessageID, chatID, other)
	if e.responded() != 0 {
		t.Fatal("answered by another link")
	}
	e.reply("  my answer ", pr.MessageID, chatID, e.l)
	if e.responded() != 1 || e.api.responds[0]["answer"] != "my answer" || e.api.respHdr[0] != `"4"` {
		t.Fatalf("%v", e.api.responds)
	}
	// the prompt is single-use
	e.reply("again", pr.MessageID, chatID, e.l)
	if e.responded() != 1 {
		t.Fatal("prompt reused")
	}
}

func TestQuestionFreeTextExpiresAndBlank(t *testing.T) {
	e := newEnv(t)
	e.addHuman(spA, hid('1'), "question", "Name it", `{"allow_text":true}`)
	f := e.open(spA, hid('1'))
	if got := find(f, "Type an answer"); got == "" {
		t.Fatal("no free text button without options")
	}
	e.tapData(find(f, "Type an answer"), f)
	pr := e.last()
	e.reply("   ", pr.MessageID, chatID, e.l)
	if e.responded() != 0 || !strings.Contains(e.last().Text, "text") {
		t.Fatalf("blank: %q", e.last().Text)
	}
	e.clock().Advance(pendingTTL + time.Minute)
	e.reply("late", pr.MessageID, chatID, e.l)
	if e.responded() != 0 {
		t.Fatal("answered after the TTL")
	}
}

// A form that expired, or an unknown one, never responds: the person is told to open it again.
func TestFormExpiry(t *testing.T) {
	e := newEnv(t)
	e.addHuman(spA, hid('1'), "message", "FYI", `{"requires_ack":true}`)
	f := e.open(spA, hid('1'))
	e.clock().Advance(pendingTTL + time.Minute)
	e.tapData(find(f, "Acknowledge"), f)
	if e.responded() != 0 || e.lastAnswer() != TextFormExpired {
		t.Fatalf("%d %q", e.responded(), e.lastAnswer())
	}
	// a form never opened (a forged message)
	e.tapData(humanData('m', spA, hid('1'), -1), botapi.Message{MessageID: 4242, Chat: botapi.Chat{ID: chatID}})
	if e.responded() != 0 {
		t.Fatal("responded from an unknown form")
	}
	// the data names another action than the form shows
	e.addHuman(spA, hid('2'), "message", "Other", `{"requires_ack":true}`)
	f2 := e.open(spA, hid('1'))
	e.tapData(humanData('m', spA, hid('2'), -1), f2)
	if e.responded() != 0 {
		t.Fatal("responded to another action")
	}
}

// Todo: step toggles are local, complete needs every step, and responds with completed.
func TestTodoStepsAndComplete(t *testing.T) {
	e := newEnv(t)
	e.addHuman(spA, hid('1'), "todo", "Do it", `{"steps":["one","two"]}`)
	f := e.open(spA, hid('1'))
	if find(f, "Mark completed") != "" {
		t.Fatal("complete offered before the steps")
	}
	e.tapData(find(f, "one"), f)
	if !strings.Contains(e.last().ReplyMarkup.InlineKeyboard[0][0].Text, "✅") {
		t.Fatalf("not toggled: %v", e.last().ReplyMarkup)
	}
	e.tapData(humanData('c', spA, hid('1'), -1), e.last()) // forged complete
	if e.responded() != 0 || !strings.Contains(e.lastAnswer(), "Check every step") {
		t.Fatalf("%d %q", e.responded(), e.lastAnswer())
	}
	e.tapData(find(e.last(), "one"), e.last()) // untoggle
	e.tapData(find(e.last(), "one"), e.last())
	e.tapData(find(e.last(), "two"), e.last())
	done := find(e.last(), "Mark completed")
	if done == "" {
		t.Fatal("no complete button")
	}
	e.tapData(done, e.last())
	if e.responded() != 1 || e.api.responds[0]["outcome"] != "completed" {
		t.Fatalf("%v", e.api.responds)
	}
}

func TestTodoWithoutSteps(t *testing.T) {
	e := newEnv(t)
	e.addHuman(spA, hid('1'), "todo", "Simple", `{}`)
	f := e.open(spA, hid('1'))
	e.tapData(find(f, "Mark completed"), f)
	if e.responded() != 1 {
		t.Fatal("not completed")
	}
}

// Message: acknowledge, or informational (dismiss); both respond acknowledged.
func TestMessageAckAndInformational(t *testing.T) {
	e := newEnv(t)
	e.addHuman(spA, hid('1'), "message", "Needs ack", `{"requires_ack":true}`)
	e.addHuman(spA, hid('2'), "message", "Just so you know", `{"requires_ack":false}`)
	f := e.open(spA, hid('1'))
	if !strings.Contains(f.Text, "acknowledgement") {
		t.Fatalf("%q", f.Text)
	}
	e.tapData(find(f, "Acknowledge"), f)
	f2 := e.open(spA, hid('2'))
	if !strings.Contains(f2.Text, "For your information") || find(f2, "Acknowledge") != "" {
		t.Fatalf("%q", f2.Text)
	}
	e.tapData(find(f2, "Dismiss"), f2)
	if e.responded() != 2 || e.api.responds[0]["outcome"] != "acknowledged" || e.api.responds[1]["outcome"] != "acknowledged" {
		t.Fatalf("%v", e.api.responds)
	}
}

const wizSchema = `{"type":"object","properties":{"env":{"type":"string","enum":["dev","prod"]},"notes":{"type":"string"},"count":{"type":"integer"},"urgent":{"type":"boolean"},"extra":{"type":"string"}},"required":["env","notes","count"]}`

func (e *env) addCustom(id, schema string, outcomes []string, ui string) *humanAction {
	a := e.addHuman(spA, id, "custom", "Fill me", `{}`)
	a.TypeSlug = "deploy"
	a.TypeSpec = &struct {
		Outcomes       []string        `json:"outcomes"`
		ResponseSchema json.RawMessage `json:"response_schema"`
		UI             json.RawMessage `json:"ui"`
	}{outcomes, json.RawMessage(schema), json.RawMessage(ui)}
	return a
}

// Custom: a wizard walks a flat schema (enum, free text, integer, boolean, optional skip) and submits the data with
// the chosen outcome.
func TestCustomWizard(t *testing.T) {
	e := newEnv(t)
	e.addCustom(hid('1'), wizSchema, []string{"done", "blocked"}, `{"order":["env","notes","count","urgent","extra"],"fields":{"env":{"label":"Environment"}}}`)
	f := e.open(spA, hid('1'))
	if !strings.Contains(f.Text, "Environment — step 1 of 5") {
		t.Fatalf("%q", f.Text)
	}
	e.tapData(find(f, "prod"), f)
	// notes: a ForceReply prompt follows the form edit
	if !e.sentForce() || !strings.Contains(e.last().Text, "Reply with: notes") {
		t.Fatalf("%q", e.last().Text)
	}
	e.reply("all good", e.last().MessageID, chatID, e.l)
	// count: not a number first
	e.reply("x", e.last().MessageID, chatID, e.l)
	if !strings.Contains(e.last().Text, "whole number") {
		t.Fatalf("%q", e.last().Text)
	}
	e.reply("7", e.last().MessageID, chatID, e.l)
	// urgent: boolean buttons, extra: optional with skip
	form := e.formMsg()
	e.tapData(find(form, "Yes"), form)
	form = e.formMsg()
	e.tapData(find(form, "Skip"), form)
	form = e.formMsg()
	if !strings.Contains(form.Text, "Your answers") || !strings.Contains(form.Text, "Environment: prod") {
		t.Fatalf("summary: %q", form.Text)
	}
	e.tapData(find(form, "Submit: blocked"), form)
	if e.responded() != 1 {
		t.Fatalf("%v", e.api.responds)
	}
	r := e.api.responds[0]
	data := r["data"].(map[string]any)
	if r["outcome"] != "blocked" || data["env"] != "prod" || data["notes"] != "all good" || data["count"] != float64(7) || data["urgent"] != true {
		t.Fatalf("%v", r)
	}
	if _, ok := data["extra"]; ok {
		t.Fatal("skipped field sent")
	}
}

func TestCustomWizardCancelAndServerRefusal(t *testing.T) {
	e := newEnv(t)
	e.addCustom(hid('1'), `{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"]}`, []string{"done"}, `{}`)
	f := e.open(spA, hid('1'))
	e.tapData(find(f, "Cancel"), f)
	if !strings.Contains(e.last().Text, "Cancelled") || e.responded() != 0 {
		t.Fatalf("%q", e.last().Text)
	}
	f = e.open(spA, hid('1'))
	e.tapData(find(f, "Yes"), f)
	e.api.badDetail = "the response does not match"
	form := e.formMsg()
	e.tapData(find(form, "Submit"), form)
	if !strings.HasPrefix(e.lastAnswer(), "Not accepted: the response does not match") {
		t.Fatalf("%q", e.lastAnswer())
	}
}

// A schema that isn't simple gets a link to the Mini App form, never a wizard.
func TestCustomComplexSchemaLinksToApp(t *testing.T) {
	e := newEnv(t)
	e.h.o.AppURL = func(space, id string) string { return "https://app.example.com/a/" + id }
	e.addCustom(hid('1'), `{"type":"object","properties":{"nested":{"type":"object"}}}`, []string{"done"}, `{}`)
	e.addCustom(hid('2'), `{"type":"object","properties":{"x":{"type":"string","pattern":"^a"}}}`, []string{"done"}, `{}`)
	e.addCustom(hid('3'), `{"type":"object","oneOf":[{}]}`, []string{"done"}, `{}`)
	e.addCustom(hid('4'), `{"type":"object","properties":{"x":{"type":"string"}}}`, nil, `{}`)
	for _, c := range []byte{'1', '2', '3', '4'} {
		f := e.open(spA, hid(c))
		if !strings.Contains(f.Text, TextOpenInApp) || f.ReplyMarkup == nil || f.ReplyMarkup.InlineKeyboard[0][0].WebApp == nil {
			t.Fatalf("%c: %q %v", c, f.Text, f.ReplyMarkup)
		}
		e.tapData(humanData('s', spA, hid(c), 0), f)
	}
	if e.responded() != 0 {
		t.Fatal("responded")
	}
	e.h.o.AppURL = nil
	f := e.open(spA, hid('1'))
	if f.ReplyMarkup != nil || !strings.Contains(f.Text, TextOpenInApp) {
		t.Fatalf("%q", f.Text)
	}
}

// Approvals are not answered here (TG-7-2): the form only says to open the app, and no tap can respond.
func TestApprovalStaysInApp(t *testing.T) {
	e := newEnv(t)
	e.addHuman(spA, hid('1'), "approval", "Deploy?", `{"action":"deploy"}`)
	f := e.open(spA, hid('1'))
	if !strings.Contains(f.Text, TextApprovalInApp) || f.ReplyMarkup != nil {
		t.Fatalf("%q", f.Text)
	}
	for _, v := range []byte("qtcmfwksx") {
		e.tapData(humanData(v, spA, hid('1'), 0), f)
	}
	if e.responded() != 0 {
		t.Fatal("an approval was answered")
	}
}

// Hidden, missing, forbidden and unreadable-space actions all get the same answer, and nothing is responded.
func TestHumanGenericRefusals(t *testing.T) {
	e := newEnv(t)
	e.addHuman(spA, hidden, "message", "x", `{}`)
	e.addHuman(spB, hid('2'), "message", "Telegram off", `{}`)
	e.addHuman(spD, hid('3'), "message", "forbidden", `{}`)
	e.addHuman(spA, hid('4'), "message", "in another space", `{}`)
	for _, c := range [][2]string{{spA, hidden}, {spA, hid('9')}, {spB, hid('2')}, {spD, hid('3')}, {spC, hid('2')}, {spA, hid('2')}} {
		e.tapData(humanData('o', c[0], c[1], -1), e.card())
		if e.lastAnswer() != TextUnavailable {
			t.Fatalf("%v: %q", c, e.lastAnswer())
		}
	}
	e.tapData("ho:bad:ids", e.card())
	if e.lastAnswer() != TextUnavailable || e.responded() != 0 {
		t.Fatal("malformed data")
	}
}

// A card resolved from the mapping: bare "ho" opens the action only for the link the card was sent to.
func TestCardResolvedFromMapping(t *testing.T) {
	e := newEnv(t)
	e.addHuman(spA, hid('1'), "message", "FYI", `{"requires_ack":true}`)
	e.tapData("ho", e.card()) // no resolver
	if e.lastAnswer() != TextUnavailable {
		t.Fatalf("%q", e.lastAnswer())
	}
	link := "tgl_1"
	e.h.o.Resolve = func(_ context.Context, chat int64, msg int) (string, string, string, bool) {
		return spA, hid('1'), link, chat == chatID && msg == 9000
	}
	e.tapData("ho", e.card())
	if !strings.Contains(e.last().Text, "FYI") {
		t.Fatalf("%q", e.last().Text)
	}
	link = "tgl_other"
	e.tapData("ho", e.card())
	if e.lastAnswer() != TextUnavailable {
		t.Fatalf("%q", e.lastAnswer())
	}
}

// A form shown at version 4 responds with 4: when the action changed meanwhile the response is refused, not applied.
func TestStaleFormAndFinalAction(t *testing.T) {
	e := newEnv(t)
	a := e.addHuman(spA, hid('1'), "message", "FYI", `{"requires_ack":true}`)
	f := e.open(spA, hid('1'))
	a.Version = 5
	e.tapData(find(f, "Acknowledge"), f)
	if e.lastAnswer() != "That request changed or was already answered. Reopen it from /needsyou." || a.Status != "open" {
		t.Fatalf("%q", e.lastAnswer())
	}
	// the form was dropped, so a second tap never retries
	e.tapData(find(f, "Acknowledge"), f)
	if e.lastAnswer() != TextFormExpired || e.responded() != 1 {
		t.Fatalf("%q %d", e.lastAnswer(), e.responded())
	}
	a.Status = "resolved"
	a.Outcome = "acknowledged"
	f = e.open(spA, hid('1'))
	if !strings.Contains(f.Text, "already resolved") || f.ReplyMarkup != nil {
		t.Fatalf("%q", f.Text)
	}
}

// Content level summary hides the body; a revoked link acts for nobody.
func TestHumanContentLevelAndRevokedLink(t *testing.T) {
	e := newEnv(t)
	a := e.addHuman(spS, hid('1'), "message", "Summarized", `{"requires_ack":true}`)
	a.Body = "SECRET BODY"
	f := e.open(spS, hid('1'))
	if strings.Contains(f.Text, "SECRET") || !strings.Contains(f.Text, "aren't shown in chat") {
		t.Fatalf("%q", f.Text)
	}
	e.principalErr = context.Canceled
	e.tapData(find(f, "Acknowledge"), f)
	if e.lastAnswer() != TextNoAccess || e.responded() != 0 {
		t.Fatalf("%q", e.lastAnswer())
	}
	// taps from another chat do nothing
	e.principalErr = nil
	other := botapi.Message{MessageID: f.MessageID, Chat: botapi.Chat{ID: 555}}
	e.tapData(find(f, "Acknowledge"), other)
	if e.responded() != 0 {
		t.Fatal("responded from another chat")
	}
}

func TestParseWizardLimits(t *testing.T) {
	big := `{"type":"object","properties":{`
	for i := 0; i < 9; i++ {
		if i > 0 {
			big += ","
		}
		big += `"f` + string(rune('a'+i)) + `":{"type":"string"}`
	}
	big += `}}`
	e := newEnv(t)
	a := e.addCustom(hid('1'), big, []string{"done"}, `{}`)
	if parseWizard(*a) != nil {
		t.Fatal("too many fields")
	}
	a = e.addCustom(hid('2'), `{"type":"object","properties":{"c":{"type":"string","enum":["1","2","3","4","5","6","7","8","9"]}}}`, []string{"done"}, `{}`)
	if parseWizard(*a) != nil {
		t.Fatal("too many choices")
	}
	a = e.addCustom(hid('3'), `{"type":"object","properties":{"c":{"type":"array"}}}`, []string{"done"}, `{}`)
	if parseWizard(*a) != nil {
		t.Fatal("array field")
	}
}

func (e *env) clock() *clock.Fake { return e.clk }

// sentForce reports whether the latest send asked for a reply (ForceReply).
func (e *env) sentForce() bool {
	if len(e.sent) == 0 {
		return false
	}
	_, ok := e.sent[len(e.sent)-1].ReplyMarkup.(*botapi.ForceReply)
	return ok
}

// formMsg is the latest message with inline buttons.
func (e *env) formMsg() botapi.Message {
	e.t.Helper()
	ms := e.srv.Messages(chatID)
	for i := len(ms) - 1; i >= 0; i-- {
		if ms[i].ReplyMarkup != nil {
			return ms[i]
		}
	}
	e.t.Fatal("no form message")
	return botapi.Message{}
}

// reply sends a text as a reply to a message of the bot.
func (e *env) reply(text string, to int, chat int64, l updates.Link) {
	e.upd++
	e.h.Handle(context.Background(), botapi.Update{UpdateID: e.upd, Message: &botapi.Message{MessageID: int(e.upd) + 500, From: &botapi.User{ID: chat},
		Chat: botapi.Chat{ID: chat, Type: botapi.ChatPrivate}, Text: text, ReplyToMessage: &botapi.Message{MessageID: to, Chat: botapi.Chat{ID: chat}}}}, l)
}

// More steps than a keyboard holds, and an empty outcome, are answered in the app rather than broken in chat.
func TestBigTodoAndEmptyOutcome(t *testing.T) {
	e := newEnv(t)
	steps := make([]string, maxSteps+1)
	for i := range steps {
		steps[i] = "s"
	}
	b, _ := json.Marshal(map[string]any{"steps": steps})
	e.addHuman(spA, hid('1'), "todo", "Big", string(b))
	f := e.open(spA, hid('1'))
	if !strings.Contains(f.Text, TextOpenInApp) {
		t.Fatalf("%q", f.Text)
	}
	e.tapData(humanData('c', spA, hid('1'), -1), f)
	a := e.addCustom(hid('2'), `{"type":"object","properties":{}}`, []string{""}, `{}`)
	if parseWizard(*a) != nil || e.responded() != 0 {
		t.Fatal("empty outcome accepted")
	}
}

// An action assigned or escalated to the caller is in_progress, and /needsyou lists it with the open ones.
func TestNeedsYouListsInProgress(t *testing.T) {
	e := newEnv(t)
	e.addHuman(spA, hid('1'), "question", "Open one", `{"options":["a"]}`)
	esc := e.addHuman(spA, hid('2'), "question", "Escalated one", `{"options":["a"]}`)
	esc.Status = "in_progress"
	e.addHuman(spA, hid('3'), "message", "Done", `{}`).Status = "cancelled"
	e.say("/needsyou alpha")
	texts, data := buttons(e.last())
	if len(texts) != 2 || !slices.Contains(data, humanData('o', spA, hid('2'), -1)) || !slices.Contains(data, humanData('o', spA, hid('1'), -1)) {
		t.Fatalf("%v %v", texts, data)
	}
	// and its form is live: it answers
	f := e.open(spA, hid('2'))
	e.tapData(find(f, "a"), f)
	if e.responded() != 1 {
		t.Fatal("escalated action not answered")
	}
}

// A form is bound to the link it was shown to: after an unlink and relink in the same chat (a new link ID), a tap on
// the old form message responds to nothing. (Mutant: dropping `f.linkID != linkID` in formFor makes this fail.)
func TestFormBoundToLink(t *testing.T) {
	e := newEnv(t)
	e.addHuman(spA, hid('1'), "question", "Which one?", `{"options":["red","blue"]}`)
	f := e.open(spA, hid('1'))
	e.l.ID = "tgl_2"
	e.tapData(find(f, "blue"), f)
	if e.responded() != 0 || e.lastAnswer() != TextFormExpired {
		t.Fatalf("%d %q", e.responded(), e.lastAnswer())
	}
	e.l.ID = "tgl_1"
	e.tapData(find(f, "blue"), f)
	if e.responded() != 1 {
		t.Fatal("the original link can't answer")
	}
}

// A link holds at most maxFormsPerLink live forms: opening one more retires the oldest.
func TestFormCapPerLink(t *testing.T) {
	e := newEnv(t)
	var forms []botapi.Message
	for i := 0; i <= maxFormsPerLink; i++ {
		id := hid(byte('i' + i)) // never 'h': that ID is the hidden one
		e.addHuman(spA, id, "question", "Q", `{"options":["x"]}`)
		forms = append(forms, e.open(spA, id))
		e.clk.Advance(time.Second)
	}
	e.tapData(find(forms[0], "x"), forms[0])
	if e.responded() != 0 || e.lastAnswer() != TextFormExpired {
		t.Fatalf("oldest form still live: %d %q", e.responded(), e.lastAnswer())
	}
	e.tapData(find(forms[1], "x"), forms[1])
	last := forms[len(forms)-1]
	e.tapData(find(last, "x"), last)
	if e.responded() != 2 {
		t.Fatalf("live forms refused: %d", e.responded())
	}
}

// A question with more options than a keyboard takes is answered in the app, like a large todo.
func TestManyOptionsOpensInApp(t *testing.T) {
	e := newEnv(t)
	e.h.o.AppURL = func(space, id string) string { return "https://app.example.com/a/" + id }
	opts := make([]string, maxOptions+1)
	for i := range opts {
		opts[i] = "o" + strings.Repeat("x", i)
	}
	b, _ := json.Marshal(map[string]any{"options": opts, "allow_text": true})
	e.addHuman(spA, hid('1'), "question", "Many", string(b))
	f := e.open(spA, hid('1'))
	if !strings.Contains(f.Text, TextOpenInApp) || f.ReplyMarkup == nil || len(f.ReplyMarkup.InlineKeyboard) != 1 || f.ReplyMarkup.InlineKeyboard[0][0].WebApp == nil {
		t.Fatalf("%q %+v", f.Text, f.ReplyMarkup)
	}
	e.tapData(humanData('q', spA, hid('1'), 0), f)
	e.tapData(humanData('f', spA, hid('1'), -1), f)
	if e.responded() != 0 {
		t.Fatal("answered in chat")
	}
	// exactly maxOptions stay in the chat
	b, _ = json.Marshal(map[string]any{"options": opts[:maxOptions]})
	e.addHuman(spA, hid('2'), "question", "Twenty", string(b))
	f = e.open(spA, hid('2'))
	if strings.Contains(f.Text, TextOpenInApp) || len(f.ReplyMarkup.InlineKeyboard) != maxOptions {
		t.Fatalf("%q", f.Text)
	}
}

// The wizard checks the schema's limits for each typed value, refuses NaN and Inf, and never cuts a long text.
func TestWizardFieldValidation(t *testing.T) {
	e := newEnv(t)
	schema := `{"type":"object","properties":{
		"name":{"type":"string","minLength":3,"maxLength":5},
		"qty":{"type":"integer","minimum":1,"maximum":10},
		"ratio":{"type":"number","minimum":0,"maximum":1},
		"free":{"type":"string"}},"required":["name","qty","ratio","free"]}`
	e.addCustom(hid('1'), schema, []string{"done"}, `{"order":["name","qty","ratio","free"]}`)
	f := e.open(spA, hid('1'))
	_ = f
	say := func(text, want string) {
		t.Helper()
		before := len(e.srv.Messages(chatID))
		e.reply(text, e.promptMsg(), chatID, e.l)
		got := e.last().Text
		if want == "" {
			return
		}
		if !strings.Contains(got, want) || len(e.srv.Messages(chatID)) == before {
			t.Fatalf("%q: %q, want %q", text, got, want)
		}
	}
	say("ab", "at least 3")
	say("abcdef", "at most 5")
	say("abc", "")
	say("0", "at least 1")
	say("11", "at most 10")
	say("1.5", "whole number")
	say("5", "")
	for _, bad := range []string{"NaN", "Inf", "-Inf", "+Inf", "infinity", "nan"} {
		say(bad, "a number")
	}
	say("-0.1", "at least 0")
	say("1.1", "at most 1")
	say("0.5", "")
	// free text over the default limit is refused with a notice, not cut
	say(strings.Repeat("x", maxWizardText+1), "at most 2000 characters")
	if len(e.api.responds) != 0 {
		t.Fatal("responded")
	}
	say(strings.Repeat("x", maxWizardText), "")
	form := e.formMsg()
	if !strings.Contains(form.Text, "Your answers") {
		t.Fatalf("%q", form.Text)
	}
	e.tapData(find(form, "Submit: done"), form)
	data := e.api.responds[0]["data"].(map[string]any)
	if data["name"] != "abc" || data["qty"] != float64(5) || data["ratio"] != 0.5 || len(data["free"].(string)) != maxWizardText {
		t.Fatalf("%v", data)
	}
}

// A question's free-text answer over the limit is refused with a notice and the prompt stays.
func TestQuestionAnswerTooLong(t *testing.T) {
	e := newEnv(t)
	e.addHuman(spA, hid('1'), "question", "Name it", `{"allow_text":true}`)
	f := e.open(spA, hid('1'))
	e.tapData(find(f, "Type an answer"), f)
	pr := e.last().MessageID
	e.reply(strings.Repeat("x", maxAnswerRunes+1), pr, chatID, e.l)
	if !strings.Contains(e.last().Text, "too long") || e.responded() != 0 {
		t.Fatalf("%q", e.last().Text)
	}
	e.reply("fine", pr, chatID, e.l)
	if e.responded() != 1 || e.api.responds[0]["answer"] != "fine" {
		t.Fatalf("%v", e.api.responds)
	}
}

// promptMsg is the latest ForceReply prompt message.
func (e *env) promptMsg() int {
	e.t.Helper()
	for i := len(e.sent) - 1; i >= 0; i-- {
		if _, ok := e.sent[i].ReplyMarkup.(*botapi.ForceReply); ok {
			ms := e.srv.Messages(chatID)
			for j := len(ms) - 1; j >= 0; j-- {
				if ms[j].Text == e.sent[i].Text {
					return ms[j].MessageID
				}
			}
		}
	}
	e.t.Fatal("no prompt")
	return 0
}
