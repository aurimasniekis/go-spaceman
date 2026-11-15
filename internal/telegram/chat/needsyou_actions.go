package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/settings"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/updates"
)

// Common actions on a needs-you card (ADR-0083, plan §2.4, TG-7-3): on every pending form, whatever its type, a person can
// claim (assign to themself) or release it, comment on it, ask the requester for more information (a clarification),
// list its attachments, and withdraw (cancel) it.
//
//   - Claim, release and cancel send the card's version as If-Match. A card that went stale gets a conflict, the server's
//     current state is read and the card is redrawn from it; nothing is overwritten. Comments and clarifications take no
//     version (they are discussion and never a response).
//   - Comment and Ask prompt with a ForceReply bound to (chat, prompt message, link) with the usual TTL, single use. A
//     photo or document sent as the reply goes through the API's upload and becomes an attachment of the comment.
//   - Cancel never acts on the first tap: it sends a confirmation message whose Confirm button is bound to (chat, that
//     message, link, space, action, version), once, within pendingTTL (takeConfirm, as the stop and incident confirmations
//     are). Approvals are never cancelled from the chat: the server refuses it for Telegram and the button isn't offered.
//   - Files lists the action's attachments (the request's and its comments') and sends them back as inbox items do:
//     only where the content level is full, with protect_content, read through the API as the link's principal first.

const (
	// promptComment and promptClarify are prompt.field for the text of a comment and of a clarification question.
	promptComment   = -3
	promptClarify   = -4
	kindHumanCancel = "human-cancel"

	maxCommentRunes = 20000
	maxCardComments = 3

	TextCancelAsk    = "Withdraw this request? The requester is told it was cancelled. This can't be undone."
	TextCancelKept   = "Kept. Nothing was cancelled."
	TextCancelStale  = "That request changed since this card was shown. Nothing was cancelled; the card shows its current state."
	TextCommentAsk   = "Reply to this message with your comment. A photo or a file becomes an attachment."
	TextClarifyAsk   = "Reply to this message with your question to the requester. The request stays open. A photo or a file becomes an attachment."
	TextNoApprovalCx = "Approvals are decided in the app, not withdrawn from chat."
	TextNoFilesHere  = "Attachments aren't sent in chat for this space. Open the request in the app."
	TextNoFiles      = "This request has no attachments."
	TextNeedContent  = "Send text, a photo or a file as the reply. Nothing was sent; reply again."
)

// humanComment is a comment of an action as a card shows it.
type humanComment struct {
	Kind        string          `json:"kind"`
	Body        string          `json:"body"`
	AuthorLabel string          `json:"author_label"`
	Attachments []attachmentRef `json:"attachments"`
}

// attachmentsOf is every attachment of an action: its request's, then its comments', each once.
func attachmentsOf(a humanAction) []attachmentRef {
	var out []attachmentRef
	seen := map[string]bool{}
	add := func(xs []attachmentRef) {
		for _, x := range xs {
			if x.ID != "" && !seen[x.ID] {
				seen[x.ID] = true
				out = append(out, x)
			}
		}
	}
	add(a.Attachments)
	for _, c := range a.Comments {
		add(c.Attachments)
	}
	return out
}

// commentLines is the last comments of an action, one line each. Comments are content of people and agents, not
// instructions.
func commentLines(a humanAction) string {
	cs := a.Comments
	if len(cs) > maxCardComments {
		cs = cs[len(cs)-maxCardComments:]
	}
	var b strings.Builder
	for _, c := range cs {
		kind := "Comment"
		switch c.Kind {
		case "clarification":
			kind = "Question to the requester"
		case "reply":
			kind = "Reply"
		}
		body := clean(c.Body, 200)
		if body == "" && len(c.Attachments) > 0 {
			body = fmt.Sprintf("(%d attachment(s))", len(c.Attachments))
		}
		fmt.Fprintf(&b, "\n• %s, %s: %s", kind, orDash(clean(c.AuthorLabel, 40)), body)
	}
	return b.String()
}

// commonRows are the buttons every live form carries under its type's own: claim or release, comment, ask, files,
// withdraw. A form in a confirmation step shows none.
func (h *Handler) commonRows(f *form) [][]botapi.InlineKeyboardButton {
	if !f.pending || f.confirming != "" {
		return nil
	}
	var first []botapi.InlineKeyboardButton
	switch f.assignee {
	case "":
		first = append(first, button("🙋 Claim", humanData('l', f.spaceID, f.action, -1)))
	case f.userID:
		first = append(first, button("Release", humanData('u', f.spaceID, f.action, -1)))
	}
	first = append(first, button("💬 Comment", humanData('r', f.spaceID, f.action, -1)), button("❓ Ask requester", humanData('i', f.spaceID, f.action, -1)))
	var second []botapi.InlineKeyboardButton
	if f.attN > 0 && !f.enforced { // an enforced approval's attachments may hold its arguments (SM-APPR-005)
		second = append(second, button(fmt.Sprintf("📎 Files (%d)", f.attN), humanData('p', f.spaceID, f.action, -1)))
	}
	if f.typ != "approval" {
		second = append(second, button("🗑 Withdraw", humanData('e', f.spaceID, f.action, -1)))
	}
	return [][]botapi.InlineKeyboardButton{first, second}
}

// withCommon puts the common rows under a view's own.
func (h *Handler) withCommon(f *form, rp reply) reply {
	rows := h.commonRows(f)
	if len(rows) == 0 {
		return rp
	}
	if rp.markup != nil {
		rows = append(append([][]botapi.InlineKeyboardButton(nil), rp.markup.InlineKeyboard...), rows...)
	}
	rp.markup = markup(rows...)
	return rp
}

// syncForm brings a form's header, version and assignee up to date with what the server holds, without touching the
// state a person has built up in it (checked steps, wizard answers).
func (h *Handler) syncForm(f *form, a humanAction, s spaceRef) {
	h.mu.Lock()
	defer h.mu.Unlock()
	f.version, f.assignee, f.pending, f.attN = a.Version, a.Assignee, a.pending(), len(attachmentsOf(a))
	f.head = h.formHead(a, s, f, f.ap)
}

// getAction reads an action of the space.
func (h *Handler) getAction(ctx context.Context, c caller, s spaceRef, id string) (humanAction, result) {
	var a humanAction
	r := c.get(ctx, "/spaces/"+s.ID+"/human-actions/"+id, &a)
	if r.ok() && (a.ID != id || a.SpaceID != s.ID) {
		r.status = http.StatusNotFound
	}
	return a, r
}

// formByAction returns the link's live form of an action, if it has one.
func (h *Handler) formByAction(linkID, spaceID, actionID string) *form {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.o.Clock.Now()
	for _, f := range h.forms {
		if f.linkID == linkID && f.spaceID == spaceID && f.action == actionID && now.Sub(f.at) <= pendingTTL {
			return f
		}
	}
	return nil
}

// reloadCard redraws a card from the server's current state (after a conflict): the card is replaced by a fresh form
// of the action, or closed when the action is no longer pending. Nothing the person had built up in the old card is
// sent anywhere.
func (h *Handler) reloadCard(ctx context.Context, c caller, l updates.Link, s spaceRef, f *form) {
	a, r := h.getAction(ctx, c, s, f.action)
	at := &botapi.Message{MessageID: f.key.msg}
	if !r.ok() {
		h.dropForm(f.key)
		h.show(ctx, l.ChatID, at, reply{text: f.head + "\n\n" + failText(r)})
		return
	}
	nf := h.newForm(a, l, s)
	nf.key = f.key
	var ap *enforcedApproval
	if a.Type == "approval" {
		ap = h.approvalOf(ctx, c, s, a, nf)
	}
	nf.ap = ap
	rp := h.renderForm(nf, a, s, ap)
	if !a.pending() {
		h.dropForm(f.key)
	} else {
		h.addForm(nf)
	}
	h.show(ctx, l.ChatID, at, rp)
	if a.pending() {
		h.wizardPrompt(ctx, nf)
	}
}

// commonAction handles the verbs of the common row. It reports whether the verb was one of them.
func (h *Handler) commonAction(ctx context.Context, q *botapi.CallbackQuery, l updates.Link, c caller, s spaceRef, f *form, verb byte) bool {
	switch verb {
	case 'l', 'u':
		if (verb == 'l' && f.assignee != "") || (verb == 'u' && f.assignee != f.userID) {
			h.answer(ctx, q.ID, TextStale)
			h.reloadCard(ctx, c, l, s, f)
			return true
		}
		assignee := l.UserID
		if verb == 'u' {
			assignee = ""
		}
		r := c.do(ctx, http.MethodPost, "/spaces/"+s.ID+"/human-actions/"+f.action+"/assign", map[string]any{"assignee": assignee},
			map[string]string{"If-Match": fmt.Sprintf(`"%d"`, f.version)})
		if !r.ok() {
			h.answer(ctx, q.ID, failText(r))
			if conflict(r) {
				h.reloadCard(ctx, c, l, s, f)
			}
			return true
		}
		done := "Claimed"
		if verb == 'u' {
			done = "Released"
		}
		h.answer(ctx, q.ID, done)
		h.resync(ctx, c, l, s, f, r)
	case 'r', 'i':
		h.answer(ctx, q.ID, "")
		text, field, hint := TextCommentAsk, promptComment, "Your comment"
		if verb == 'i' {
			text, field, hint = TextClarifyAsk, promptClarify, "Your question"
		}
		m, ok := h.send(ctx, l.ChatID, reply{text: text, force: &botapi.ForceReply{ForceReply: true, InputFieldPlaceholder: hint}})
		if ok {
			h.addPrompt(pendingKey{l.ChatID, m.MessageID}, prompt{linkID: l.ID, form: f.key, field: field})
		}
	case 'p':
		h.files(ctx, q, l, c, s, f)
	case 'e':
		if f.typ == "approval" {
			h.answer(ctx, q.ID, TextNoApprovalCx)
			return true
		}
		h.answer(ctx, q.ID, "")
		rows := []botapi.InlineKeyboardButton{
			styled(button("Confirm: cancel request", cancelConfirmData(s.ID, f.action, f.version)), botapi.StyleDanger),
			button("Keep it", humanData('g', s.ID, f.action, -1))}
		m, ok := h.send(ctx, l.ChatID, reply{text: TextCancelAsk + "\n\n" + typeMark(f.typ) + clean(f.title, 200), markup: markup(rows), protect: true})
		if ok {
			h.qrAddConfirm(pendingKey{l.ChatID, m.MessageID}, confirmation{linkID: l.ID, kind: kindHumanCancel, spaceID: s.ID, id: f.action, decision: "cancel", version: f.version})
		}
	default:
		return false
	}
	return true
}

// cancelConfirmData is the Confirm button's data: "hv:<space>:<action>:<version>", the version being the card's.
func cancelConfirmData(spaceID, actionID string, version int64) string {
	return fmt.Sprintf("hv:%s:%s:%d", strings.TrimPrefix(spaceID, "spc_"), strings.TrimPrefix(actionID, "hum_"), version)
}

// resync takes the action a successful call returned (or reads it again) into the form and redraws the card.
func (h *Handler) resync(ctx context.Context, c caller, l updates.Link, s spaceRef, f *form, r result) {
	var a humanAction
	if json.Unmarshal(r.body, &a) != nil || a.ID != f.action || a.SpaceID != s.ID {
		var gr result
		if a, gr = h.getAction(ctx, c, s, f.action); !gr.ok() {
			return
		}
	}
	if !a.pending() {
		h.dropForm(f.key)
		h.show(ctx, l.ChatID, &botapi.Message{MessageID: f.key.msg}, reply{text: f.head + "\n\nThis is already " + clean(orDash(a.Status), 20) + "."})
		return
	}
	h.syncForm(f, a, s)
	h.draw(ctx, f)
}

// cancelConfirmTap handles the Confirm ("hv") and Keep ("hg") buttons of a cancel confirmation. They act on the
// confirmation message only: the tap's (chat, message) must hold this link's confirmation for this space, action and
// version, once, within pendingTTL. Anything else, such as the same data on another message, does nothing.
func (h *Handler) cancelConfirmTap(ctx context.Context, q *botapi.CallbackQuery, l updates.Link, c caller, s spaceRef, actionID string, verb byte, version int64) {
	k := pendingKey{q.Message.Chat.ID, q.Message.MessageID}
	if verb == 'g' {
		h.qr.mu.Lock()
		if cf, ok := h.qr.confirm[k]; ok && cf.linkID == l.ID && cf.kind == kindHumanCancel && cf.spaceID == s.ID && cf.id == actionID {
			delete(h.qr.confirm, k)
			h.qr.mu.Unlock()
			h.answer(ctx, q.ID, "")
			h.show(ctx, l.ChatID, q.Message, reply{text: TextCancelKept})
			return
		}
		h.qr.mu.Unlock()
		h.answer(ctx, q.ID, TextConfirmGone)
		return
	}
	if _, ok := h.takeConfirm(k, l.ID, kindHumanCancel, s.ID, actionID, "cancel", version); !ok {
		h.answer(ctx, q.ID, TextConfirmGone)
		return
	}
	h.formMu.Lock()
	defer h.formMu.Unlock()
	r := c.do(ctx, http.MethodPost, "/spaces/"+s.ID+"/human-actions/"+actionID+"/cancel", map[string]any{"reason": "Withdrawn from Telegram"},
		map[string]string{"If-Match": fmt.Sprintf(`"%d"`, version)})
	f := h.formByAction(l.ID, s.ID, actionID)
	if !r.ok() {
		h.answer(ctx, q.ID, failText(r))
		if conflict(r) {
			h.show(ctx, l.ChatID, q.Message, reply{text: TextCancelStale})
			if f != nil {
				h.reloadCard(ctx, c, l, s, f)
			}
		} else {
			h.show(ctx, l.ChatID, q.Message, reply{text: "Nothing was cancelled: " + failText(r)})
		}
		return
	}
	h.answer(ctx, q.ID, "Cancelled")
	h.show(ctx, l.ChatID, q.Message, reply{text: "🗑 The request was cancelled."})
	if f != nil {
		h.dropForm(f.key)
		h.show(ctx, l.ChatID, &botapi.Message{MessageID: f.key.msg}, reply{text: f.head + "\n\n🗑 Cancelled."})
	}
}

// files sends an action's attachments back as files, the way an inbox item's are: the content level must be full, each
// is read through the API as the link's principal first, and each goes with protect_content.
func (h *Handler) files(ctx context.Context, q *botapi.CallbackQuery, l updates.Link, c caller, s spaceRef, f *form) {
	if f.enforced {
		h.answer(ctx, q.ID, TextNoFilesHere)
		return
	}
	if h.o.Space(s.ID).ContentLevel != settings.ContentFull {
		h.answer(ctx, q.ID, TextNoFilesHere)
		return
	}
	a, r := h.getAction(ctx, c, s, f.action)
	if !r.ok() {
		h.answer(ctx, q.ID, failText(r))
		return
	}
	atts := attachmentsOf(a)
	if len(atts) == 0 {
		h.answer(ctx, q.ID, TextNoFiles)
		return
	}
	h.answer(ctx, q.ID, "")
	h.sendAttachments(ctx, c, l, s, atts)
}

// commentReply takes the reply to a comment or clarification prompt: its text or caption is the body, and a photo or a
// document is uploaded through the API and attached. The prompt is already consumed; a reply that can't be used asks
// again with a fresh ForceReply prompt bound to the new message, at most maxPromptRetries times.
//
// It runs without formMu: the downloads and uploads can take a while and must not block other taps. The form is looked
// up briefly first, the files are moved, and the form is looked up again under formMu before the comment is posted. If
// the form or the action went away meanwhile, the uploaded attachments stay unlinked (the server owns them until they
// are attached or expire).
func (h *Handler) commentReply(ctx context.Context, m *botapi.Message, l updates.Link, k pendingKey, p prompt) {
	retry := func(text string) {
		if p.tries >= maxPromptRetries {
			h.send(ctx, l.ChatID, reply{text: text + " Nothing was sent. Tap the button on the card to try again."})
			return
		}
		p.tries++
		nm, ok := h.send(ctx, l.ChatID, reply{text: text, force: &botapi.ForceReply{ForceReply: true, InputFieldPlaceholder: "Your reply"}})
		if ok {
			h.addPrompt(pendingKey{l.ChatID, nm.MessageID}, p)
		}
	}
	h.formMu.Lock()
	f0 := h.formFor(p.form, l.ID)
	var spaceID, actionID string
	if f0 != nil {
		spaceID, actionID = f0.spaceID, f0.action
	}
	h.formMu.Unlock()
	if f0 == nil {
		h.send(ctx, l.ChatID, reply{text: TextFormExpired})
		return
	}
	if m.MediaGroupID != "" {
		// An album's other parts reply to the same prompt: only the first is taken, the rest are dropped quietly rather
		// than captured as inbox items.
		h.mu.Lock()
		h.refused[albumKey{l.ID, m.MediaGroupID}] = h.o.Clock.Now()
		h.mu.Unlock()
	}
	pc, ok := extract(m)
	if !ok {
		if len(m.Video)+len(m.VideoNote)+len(m.Voice)+len(m.Audio)+len(m.Animation)+len(m.Sticker) > 0 {
			retry("I can only take text, photos and files here. Nothing was sent; reply again.")
		} else {
			retry(TextNeedContent)
		}
		return
	}
	if n := len([]rune(pc.text)); n > maxCommentRunes {
		retry(fmt.Sprintf("That is too long (%d characters, at most %d). Nothing was sent; reply again with a shorter one.", n, maxCommentRunes))
		return
	}
	for _, fr := range pc.files {
		if fr.size > botapi.MaxDownloadBytes {
			retry(TextTooBig)
			return
		}
	}
	c, ok := h.caller(ctx, l)
	if !ok {
		return
	}
	s, r, ok := h.space(ctx, c, spaceID)
	if !ok {
		retry(failText(r) + " Nothing was sent.")
		return
	}
	var ids []string
	for _, fr := range pc.files {
		if h.o.Allow != nil && !h.o.Allow(l.ID, updates.KindDownload) {
			retry(TextSlow)
			return
		}
		id, text := h.fetchAndUpload(ctx, c, s, fr)
		if text != "" {
			retry(text)
			return
		}
		ids = append(ids, id)
	}
	body := pc.text
	if body == "" {
		body = fmt.Sprintf("(no text; %d attachment(s) sent from Telegram)", len(ids))
	}
	in := map[string]any{"body": body}
	if len(ids) > 0 {
		in["attachment_ids"] = ids
	}
	h.formMu.Lock() // only the comment call and the redraw run under the lock
	defer h.formMu.Unlock()
	f := h.formFor(p.form, l.ID)
	if f == nil {
		h.send(ctx, l.ChatID, reply{text: TextFormExpired + " Nothing was sent."})
		return
	}
	path, done := "/comments", "Comment added."
	if p.field == promptClarify {
		path, done = "/clarify", "Question sent to the requester. The request stays open."
	}
	r = c.do(ctx, http.MethodPost, "/spaces/"+s.ID+"/human-actions/"+actionID+path, in, nil)
	if !r.ok() {
		if conflict(r) || r.status == http.StatusForbidden || r.status == http.StatusNotFound {
			h.send(ctx, l.ChatID, reply{text: failText(r)}) // final: the action changed or isn't yours to comment on
		} else {
			retry(failText(r) + " Nothing was sent; reply again.")
		}
		return
	}
	h.send(ctx, l.ChatID, reply{text: done})
	if a, gr := h.getAction(ctx, c, s, actionID); gr.ok() && a.pending() {
		h.syncForm(f, a, s)
		h.draw(ctx, f)
	}
}
