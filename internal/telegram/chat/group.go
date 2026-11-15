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
	refuse := func(text string) {
		if q != nil {
			h.answer(ctx, q.ID, text)
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
	if cmd, arg, ok := command(m.Text); ok {
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
		}
		// /spaces, /space and /control don't apply to a group; nothing else is answered there.
		return
	}
	// A reply to one of the bot's prompts; any other message (capture, control text) is never taken in a group.
	if m.ReplyToMessage != nil && !h.promptReply(ctx, m, l) && !h.rejectReply(ctx, m, l) {
		h.qrReply(ctx, m, l)
	}
}

// full reports whether bodies of s may be sent: the space's content level is full and s isn't shown in a group.
func (h *Handler) full(s spaceRef) bool {
	return h.o.Space != nil && settings.Full(h.o.Space(s.ID), s.group)
}
