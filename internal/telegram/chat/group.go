package chat

import (
	"context"
	"strings"

	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/settings"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/updates"
)

// Group mode (ADR-0083 amendment TG-13, plan §3.7): a forum supergroup bound to one space. Its security core:
//
//   - Per-tapper authorization. The dispatcher hands over the tapping person's OWN link (never the binder's, never the
//     group's) and handleGroup checks, before anything else, that this link's principal may read the bound space. Every
//     operation then runs as that principal through the in-process API, so its grants decide (a viewer can't do an
//     operator's action). A tapper without a link is refused by the dispatcher.
//   - One space. Every space a group update touches goes through Handler.space, which refuses any space but the bound
//     one; /spaces, /space and the space buttons are refused, and the link's chosen space is never used.
//   - Summary only. Every spaceRef of a group update is marked (spaceRef.group) and Handler.full answers false for it,
//     so nothing above summary is rendered; the Bot API client refuses attachments to any group chat as a backstop.
//   - Bound to the tapper. Forms, confirmations and prompts are keyed by the link that opened them, so another member
//     can't complete them; enforced approvals are never decided in a group (no server confirmation is armed).

// Texts of group mode.
const (
	TextGroupNoAccess = "Your SpaceMan account can't use this space. Ask an administrator."
	TextGroupFixed    = "This group is bound to one space."
)

// groupCommands are the commands the bot answers in a group (the others are ignored without a reply).
var groupCommands = map[string]bool{"inbox": true, "needsyou": true, "queue": true, "runs": true, "messages": true,
	"spaces": true, "space": true, "control": true}

type groupKey struct{}

func withGroup(ctx context.Context, g updates.Group) context.Context {
	return context.WithValue(ctx, groupKey{}, g)
}

// groupOf returns the bound group of the update being handled (ok false: a private chat).
func groupOf(ctx context.Context) (updates.Group, bool) {
	g, ok := ctx.Value(groupKey{}).(updates.Group)
	return g, ok
}

// handleGroup handles an update of a bound group as the tapping person's own link.
func (h *Handler) handleGroup(ctx context.Context, u botapi.Update, l updates.Link) {
	g := *l.Group
	q := u.CallbackQuery
	// A tap is refused with an alert only its tapper sees. A command has no callback query to answer, so it gets a
	// short reply to it that names no content (TextGroupNoAccess, TextGroupFixed); other messages get nothing.
	refuse := func(text string) {
		switch {
		case q != nil:
			h.answer(withGroup(ctx, g), q.ID, text)
		case u.Message != nil:
			if _, _, ok := command(u.Message.Text); ok {
				h.replyIn(ctx, u.Message, text)
			}
		}
	}
	// A command that isn't the bot's own group command is none of its business: stay silent before any lookup, so a
	// member without access can't make the bot reveal (by its refusal) that they have a link.
	if q == nil && u.Message != nil {
		if cmd, to, _, ok := commandFor(u.Message.Text); ok {
			// A command addressed to another bot is that bot's. An unknown own username fails closed.
			if to != "" && !strings.EqualFold(to, h.botUsername()) || !groupCommands[cmd] {
				return
			}
		}
	}
	if !settings.IsGroupChat(g.ChatID) || l.ChatID != g.ChatID || !spaceIDRe.MatchString(g.SpaceID) {
		refuse(TextGroupNoAccess)
		return
	}
	ctx = withGroup(ctx, g)
	// The tapper's own link must be able to read the bound space; the group grants nothing.
	p := h.principal(ctx, l)
	if p == nil {
		refuse(TextGroupNoAccess)
		return
	}
	if _, _, ok := h.space(ctx, caller{h: h, p: p}, g.SpaceID); !ok {
		refuse(TextGroupNoAccess)
		return
	}
	if q != nil {
		if q.Data == "sl" || strings.HasPrefix(q.Data, "sp:") {
			refuse(TextGroupFixed)
			return
		}
		h.callback(ctx, q, l)
		return
	}
	m := u.Message
	if m == nil {
		return
	}
	if cmd, _, arg, ok := commandFor(m.Text); ok {
		switch cmd {
		case "inbox":
			h.cmdInbox(ctx, l, arg)
		case "needsyou":
			h.cmdNeedsYou(ctx, l, arg)
		case "queue":
			h.cmdQueue(ctx, l, arg)
		case "runs":
			h.cmdRuns(ctx, l, arg)
		case "messages":
			h.cmdMessages(ctx, l, arg)
		case "spaces", "space", "control":
			// They don't apply to a group; the reply shows nothing of any space.
			h.replyIn(ctx, m, TextGroupFixed)
		}
		// Any other command is none of the bot's: nothing is answered.
		return
	}
	// A reply to one of the bot's prompts; any other message (capture, control text) is never taken in a group.
	if m.ReplyToMessage != nil && !h.promptReply(ctx, m, l) && !h.rejectReply(ctx, m, l) {
		h.qrReply(ctx, m, l)
	}
}

// replyIn answers a command of a group with a short text as a reply to it, in its topic.
func (h *Handler) replyIn(ctx context.Context, m *botapi.Message, text string) {
	h.send(ctx, m.Chat.ID, reply{text: text, replyTo: m.MessageID, thread: m.MessageThreadID})
}

// full reports whether bodies of s may be sent: the space's content level is full and s isn't shown in a group.
func (h *Handler) full(s spaceRef) bool {
	return h.o.Space != nil && settings.Full(h.o.Space(s.ID), s.group)
}
