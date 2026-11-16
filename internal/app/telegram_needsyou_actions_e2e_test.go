package app_test

import (
	"strconv"
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/human"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
)

type commonState struct {
	Status   string `json:"status"`
	Assignee string `json:"assignee"`
	Version  int64  `json:"version"`
	Comments []struct {
		Kind        string `json:"kind"`
		Body        string `json:"body"`
		Attachments []struct {
			MIME string `json:"mime"`
		} `json:"attachments"`
	} `json:"comments"`
}

func (c *approvalChat) common(id string) commonState {
	c.t.Helper()
	r := c.s.root.do("GET", "/spaces/"+c.s.space+"/human-actions/"+id, nil)
	expect(c.t, r, 200)
	var a commonState
	r.json(c.t, &a)
	return a
}

func (c *approvalChat) question(title string) string {
	c.t.Helper()
	r := c.s.root.do("POST", "/spaces/"+c.s.space+"/human-actions", map[string]any{"type": "question", "title": title,
		"request": map[string]any{"options": []string{"yes", "no"}}})
	expect(c.t, r, 201)
	var a struct {
		ID string `json:"id"`
	}
	r.json(c.t, &a)
	return a.ID
}

// replyPhoto sends a photo with a caption as the reply to a prompt.
func (c *approvalChat) replyPhoto(to int, caption string, content []byte) {
	id := c.tg.AddFile("p.jpg", content, int64(len(content)))
	c.push(&botapi.Message{MessageID: int(c.n + 1), From: &botapi.User{ID: 42}, Chat: botapi.Chat{ID: 42, Type: botapi.ChatPrivate}, Caption: caption,
		Photo:          []botapi.PhotoSize{{FileID: id, Width: 10, Height: 10, FileSize: int64(len(content))}},
		ReplyToMessage: &botapi.Message{MessageID: to, Chat: botapi.Chat{ID: 42}}}, nil)
}

// SM-TG-003, SM-TG-006 (TG-7-3): the common actions on a needs-you card through the fake Bot API, the real dispatcher,
// chat, in-process API, human service and attachments. Claim assigns the linked user with the card's version; a card
// that went stale gets a conflict and is refreshed, and nothing is overwritten. Comment and Ask requester use ForceReply
// (kinds comment and clarification, neither a response); a photo reply is uploaded and attached to the comment. Files
// sends it back with protect_content. Withdraw needs its bound confirmation and cancels the action; an approval is never
// withdrawn from the chat, even with crafted data.
func TestTelegramNeedsYouCommonActions(t *testing.T) {
	t.Parallel()
	c := newApprovalChat(t)
	id := c.question("Which environment?")
	f := c.open(id)

	// Claim.
	c.tap(f, "Claim")
	if a := c.common(id); a.Assignee != c.s.userID || a.Status != human.InProgress {
		t.Fatalf("claim: %+v", a)
	}
	card := c.message(f.MessageID)
	if !strings.Contains(card.Text, "claimed by you") || buttonData(card, "Release") == "" {
		t.Fatalf("card after claim: %q", card.Text)
	}

	// Comment and clarification by ForceReply; neither answers the question.
	c.tap(card, "Comment")
	c.say("staging is down", c.last().MessageID)
	c.tap(c.message(f.MessageID), "Ask requester")
	c.say("which region?", c.last().MessageID)
	a := c.common(id)
	if len(a.Comments) != 2 || a.Comments[0].Kind != "comment" || a.Comments[0].Body != "staging is down" ||
		a.Comments[1].Kind != "clarification" || a.Comments[1].Body != "which region?" || a.Status != human.InProgress {
		t.Fatalf("comments: %+v", a)
	}
	if !strings.Contains(c.message(f.MessageID).Text, "Question to the requester") {
		t.Fatalf("card: %q", c.message(f.MessageID).Text)
	}

	// A photo as the reply becomes an attachment of the comment, and Files sends it back protected.
	png := []byte("\x89PNG\r\n\x1a\n-pixels-")
	c.tap(c.message(f.MessageID), "Comment")
	c.replyPhoto(c.last().MessageID, "the error", png)
	a = c.common(id)
	if len(a.Comments) != 3 || a.Comments[2].Body != "the error" || len(a.Comments[2].Attachments) != 1 || a.Comments[2].Attachments[0].MIME != "image/png" {
		t.Fatalf("attachment comment: %+v", a.Comments)
	}
	c.tap(c.message(f.MessageID), "Files (1)")
	if calls := c.tg.Calls("sendPhoto", "sendDocument"); len(calls) != 1 {
		t.Fatalf("files sent: %d", len(calls))
	}

	// Stale card: someone else changes the action; the chat's Release conflicts and the card is refreshed.
	cur := c.common(id)
	r := c.s.root.do("POST", "/spaces/"+c.s.space+"/human-actions/"+id+"/assign", map[string]any{"assignee": ""}, "If-Match", `"`+strconv.FormatInt(cur.Version, 10)+`"`)
	expect(t, r, 200)
	c.tap(c.message(f.MessageID), "Release")
	if a := c.common(id); a.Assignee != "" {
		t.Fatalf("a stale card changed the action: %+v", a)
	}
	if card = c.message(f.MessageID); strings.Contains(card.Text, "Claimed") || buttonData(card, "Claim") == "" {
		t.Fatalf("card not refreshed: %q", card.Text)
	}

	// Withdraw: the first tap asks, a Confirm bound to its message cancels; the same data on the card does nothing.
	c.tap(card, "Withdraw")
	ask := c.last()
	if c.common(id).Status == human.Cancelled {
		t.Fatal("cancelled on the first tap")
	}
	data := buttonData(ask, "Confirm")
	c.tapData(c.message(f.MessageID), data)
	if c.common(id).Status == human.Cancelled {
		t.Fatal("a Confirm on another message cancelled")
	}
	c.tapData(ask, data)
	if a := c.common(id); a.Status != human.Cancelled {
		t.Fatalf("cancel: %+v", a)
	}

	// An approval is never withdrawn from the chat: no button, crafted data refused, nothing changes.
	adv := c.advisory("Deploy it")
	af := c.open(adv)
	if buttonData(af, "Withdraw") != "" || buttonData(af, "Claim") == "" {
		t.Fatalf("approval card: %v", af.ReplyMarkup)
	}
	sp, hum := strings.TrimPrefix(c.s.space, "spc_"), strings.TrimPrefix(adv, "hum_")
	c.tapData(af, "he:"+sp+":"+hum)
	c.tapData(af, "hv:"+sp+":"+hum+":1")
	if c.action(adv).Status != "open" {
		t.Fatal("an approval was withdrawn from the chat")
	}
}

// SM-TG-006 (TG-7-3): a card opened by an operator link, then the link capped to viewer: the server refuses Claim,
// Comment, Ask requester and Withdraw+Confirm through the real dispatcher, API and human service. Each leaves the action
// unchanged: same version, no comments, still open and unassigned.
func TestTelegramNeedsYouViewerRefusedEndToEnd(t *testing.T) {
	t.Parallel()
	c := newApprovalChat(t)
	id := c.question("Which environment?")
	f := c.open(id)
	before := c.common(id)
	c.s.setLinkRole(t, "viewer")

	c.tap(f, "Claim")
	c.tap(f, "Comment")
	c.say("hello", c.last().MessageID)
	c.tap(f, "Ask requester")
	c.say("why?", c.last().MessageID)
	c.tap(f, "Withdraw")
	ask := c.last()
	c.tap(ask, "Confirm")

	a := c.common(id)
	if a.Version != before.Version || a.Status != before.Status || a.Assignee != "" || len(a.Comments) != 0 {
		t.Fatalf("a viewer link changed the action: before %+v after %+v", before, a)
	}
	if a.Status == human.Cancelled {
		t.Fatal("cancelled by a viewer")
	}
}
