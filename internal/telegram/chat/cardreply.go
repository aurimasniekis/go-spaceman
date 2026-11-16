package chat

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/updates"
)

// A Telegram reply to a notifier card (reply_to_message set to the card) is about that card, never about the link's
// current space. cardReply looks the replied-to message up in the card mapping:
//
//   - A human action's card: a question's reply is its answer when it names an option (letter, number or label, case
//     insensitive) or, for a free-text question, is the text itself. A closed action is answered with "already
//     resolved". An approval is never decided from free text: its buttons carry a confirm step (and, when enforced, a
//     server-side confirmation) that a sentence can't give.
//   - Anything that can't be an answer (and any other card of the link) is captured into the card's own space, and the
//     capture's acknowledgement says which one. Never the current or default space.
//
// Authority is the buttons': the answer goes through the same respond call as the principal of the replying link, with
// the version the card shows as If-Match, after the same space check (visibility, a group's binding). The API decides.

// Texts of the card replies.
const (
	TextCardReplyResolved = "That request was already answered or closed."
	TextCardReplyApproval = "An approval is decided with the buttons on its card, not with text. Nothing was sent."
	TextCardReplyBig      = "This question has too many options for chat. Open it in the app to answer."
)

// cardReply handles a reply to a card. It reports whether it took the message.
func (h *Handler) cardReply(ctx context.Context, m *botapi.Message, l updates.Link) bool {
	// A group is never answered from a reply: members reply to cards to talk about them, and the buttons there open a
	// form on a new message (cards.go refuses a card tap in a group for the same reason).
	if m.ReplyToMessage == nil || l.Group != nil {
		return false
	}
	chat, msg := m.Chat.ID, m.ReplyToMessage.MessageID
	var spaceID, actionID, linkID string
	var version int64
	if h.o.Card != nil {
		if ref, ok := h.o.Card(ctx, chat, msg); ok {
			spaceID, actionID, linkID, version = ref.SpaceID, ref.ActionID, ref.LinkID, ref.Version
		}
	}
	if actionID == "" && h.o.Resolve != nil {
		// A continuation message of a long card: it shows no version and isn't the card's message, so it answers
		// nothing; the reply goes to the card's space.
		if sp, _, link, ok := h.o.Resolve(ctx, chat, msg); ok && link == l.ID && spaceIDRe.MatchString(sp) {
			return h.routeToSpace(ctx, m, l, sp)
		}
	}
	if actionID == "" {
		return h.cardSpaceReply(ctx, m, l, chat, msg)
	}
	if linkID != l.ID || !spaceIDRe.MatchString(spaceID) || !humanIDRe.MatchString(actionID) {
		return false
	}
	c, ok := h.caller(ctx, l)
	if !ok {
		return true
	}
	s, r, ok := h.space(ctx, c, spaceID)
	if !ok {
		h.send(ctx, l.ChatID, reply{text: failText(r)})
		return true
	}
	a, r := h.getAction(ctx, c, s, actionID)
	if !r.ok() {
		h.send(ctx, l.ChatID, reply{text: failText(r)})
		return true
	}
	if !a.pending() {
		h.send(ctx, l.ChatID, reply{text: TextCardReplyResolved})
		return true
	}
	if a.Version != version {
		h.send(ctx, l.ChatID, reply{text: TextCardChanged})
		h.showCard(ctx, l, s, a, pendingKey{chat, msg}, m.ReplyToMessage.MessageThreadID, h.enforcedFor(ctx, c, s, a))
		return true
	}
	text := strings.TrimSpace(m.Text)
	switch a.Type {
	case "approval":
		h.send(ctx, l.ChatID, reply{text: TextCardReplyApproval})
		return true
	case "question":
		if text != "" {
			return h.questionReply(ctx, m, l, c, s, a, text, msg)
		}
	}
	return h.routeToSpace(ctx, m, l, s.ID)
}

// questionReply answers a question from a reply's text.
func (h *Handler) questionReply(ctx context.Context, m *botapi.Message, l updates.Link, c caller, s spaceRef, a humanAction, text string, msg int) bool {
	f := h.newForm(a, l, s)
	f.key, f.fromCard, f.thread = pendingKey{m.Chat.ID, msg}, true, m.ReplyToMessage.MessageThreadID
	if f.big {
		h.send(ctx, l.ChatID, reply{text: TextCardReplyBig})
		return true
	}
	answer := text
	if len(f.options) > 0 {
		idx, ambiguous := matchOption(f.options, text)
		switch {
		case ambiguous:
			h.send(ctx, l.ChatID, reply{text: "Which option? " + optionsHint(f.options) + "\nNothing was sent."})
			return true
		case idx >= 0:
			answer = f.options[idx]
		case !f.allowText:
			// Not an answer: it goes to the card's space like any other note, and the person is told.
			return h.routeToSpace(ctx, m, l, s.ID)
		}
	}
	if n := len([]rune(answer)); n > maxAnswerRunes {
		h.send(ctx, l.ChatID, reply{text: fmt.Sprintf("That answer is too long (%d characters, at most %d). Nothing was sent.", n, maxAnswerRunes)})
		return true
	}
	fl := h.formLock(m.Chat.ID)
	fl.Lock()
	defer fl.Unlock()
	if h.respond(ctx, c, l, nil, s, f, map[string]any{"outcome": "answered", "answer": answer}) {
		h.send(ctx, l.ChatID, reply{text: "Answered: " + clean(answer, 200)})
	}
	return true
}

// matchOption finds the option a reply names: by letter (A is the first), by number (1 is the first) or by its label,
// ignoring case and surrounding space, a trailing "." or ")" on a letter or number. idx is -1 when none matches;
// ambiguous when the reply names more than one option (a label "2" next to the option that number 2 means).
func matchOption(options []string, text string) (idx int, ambiguous bool) {
	t := strings.TrimSpace(text)
	hits := map[int]bool{}
	for i, o := range options {
		if strings.EqualFold(strings.TrimSpace(o), t) {
			hits[i] = true
		}
	}
	short := strings.TrimRight(t, ".):")
	if len([]rune(short)) == 1 {
		if r := []rune(strings.ToUpper(short))[0]; r >= 'A' && r <= 'Z' && int(r-'A') < len(options) {
			hits[int(r-'A')] = true
		}
	}
	if n, err := strconv.Atoi(short); err == nil && n >= 1 && n <= len(options) {
		hits[n-1] = true
	}
	switch len(hits) {
	case 0:
		return -1, false
	case 1:
		for i := range hits {
			return i, false
		}
	}
	return -1, true
}

// optionsHint lists the options with the letters a reply may use.
func optionsHint(options []string) string {
	var b strings.Builder
	b.WriteString("Reply with a letter, a number or the option itself:")
	for i, o := range options {
		if i >= 26 {
			break
		}
		fmt.Fprintf(&b, "\n%c. %s", 'A'+i, clean(o, 80))
	}
	return b.String()
}

// cardSpaceReply routes a reply to a card that is no human action's (an item, a message the operators sent...) to the
// card's own space, when the card is this link's.
func (h *Handler) cardSpaceReply(ctx context.Context, m *botapi.Message, l updates.Link, chat int64, msg int) bool {
	if h.o.CardSpace == nil {
		return false
	}
	sp, link, ok := h.o.CardSpace(ctx, chat, msg)
	if !ok || link != l.ID || !spaceIDRe.MatchString(sp) {
		return false
	}
	return h.routeToSpace(ctx, m, l, sp)
}

// routeToSpace captures a reply into spaceID instead of the link's current space (the space of a card the reply is
// to).
func (h *Handler) routeToSpace(ctx context.Context, m *botapi.Message, l updates.Link, spaceID string) bool {
	h.capture(context.WithValue(ctx, pinKey{}, spaceID), m, l)
	return true
}
