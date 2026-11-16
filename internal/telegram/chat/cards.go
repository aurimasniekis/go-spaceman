package chat

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/card"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/updates"
)

// Cards acted on in place (ADR-0089, plan §2.6, T4). A notifier card carries the action's quick actions and More… as
// explicit "h<verb>:<space>:<action>" buttons. A tap on one, with no form on that message yet, adopts the card message:
// the action is read as the tapping link, the form is built on the card's own message (never a new one), and the verb
// runs on it. More… shows the whole form there; its Back (and Back from a step a quick action opened) draws the card
// again. Once the action is settled the card becomes its compact final line.
//
// Authority: the tap acts as the tapping link (its principal, through the in-process API, with the card's version as
// If-Match). The card must be this link's (the notifier's record of the message names the link it was sent to), name
// the same space and action as the button, and show the version the server holds; otherwise nothing is done and the
// current card is shown. Which buttons a card draws is cosmetic: hiding one protects nothing, the API decides.
//
// Ordering: the notifier and this package edit the same message. Every edit of a card message goes through
// Options.EditCard (the notifier's card lock): an edit is skipped when the card already shows a newer state or the
// item was retracted, and the notifier's own later state of the resource replaces whatever was drawn here.

// Texts of the card taps.
const (
	TextCardSettled = "That request was already answered or closed."
	TextCardChanged = "That request changed since this card was shown. The card shows its current state; tap again."
)

// CardRef is a notifier card as this package sees it (Options.Card).
type CardRef struct {
	SpaceID, ActionID, LinkID string
	// Version is the action's version the card shows.
	Version int64
}

// cardVerbs are the verbs a card's own buttons carry: they may adopt the card message when no form is on it yet. The
// steps after them (confirming an approval, a wizard's answers) never adopt anything: they need the form the first
// step built.
const cardVerbs = "qfcmadn"

func isCardVerb(v byte) bool { return strings.IndexByte(cardVerbs, v) >= 0 }

// cardTap handles a card button tapped on a message with no live form: it adopts the card message as the form of the
// action and runs the verb on it. Holds formMu.
func (h *Handler) cardTap(ctx context.Context, q *botapi.CallbackQuery, l updates.Link, c caller, s spaceRef, actionID string, verb byte, n int) {
	key := pendingKey{q.Message.Chat.ID, q.Message.MessageID}
	if h.o.Card == nil || l.Group != nil {
		h.answer(ctx, q.ID, TextFormExpired)
		return
	}
	ref, ok := h.o.Card(ctx, key.chat, key.msg)
	if !ok || ref.LinkID != l.ID || ref.SpaceID != s.ID || ref.ActionID != actionID {
		// Not this link's card of this action: the button data and the message don't belong together.
		h.answer(ctx, q.ID, TextUnavailable)
		return
	}
	a, r := h.getAction(ctx, c, s, actionID)
	if !r.ok() {
		h.answer(ctx, q.ID, failText(r))
		return
	}
	thread := q.Message.MessageThreadID
	if !a.pending() {
		h.answer(ctx, q.ID, TextCardSettled)
		h.showCard(ctx, l, s, a, key, thread, false)
		return
	}
	if a.Version != ref.Version {
		// The card is behind the server: nothing is acted on; the person sees the current card first.
		h.answer(ctx, q.ID, TextCardChanged)
		h.showCard(ctx, l, s, a, key, thread, h.enforcedFor(ctx, c, s, a))
		return
	}
	f := h.newForm(a, l, s)
	f.key, f.fromCard, f.thread = key, true, thread
	if a.Type == "approval" {
		f.ap = h.approvalOf(ctx, c, s, a, f)
	}
	f.head = h.formHead(a, s, f, f.ap)
	if verb == card.VerbMore {
		f.back = backForm
		h.addForm(f)
		if !h.draw(ctx, f) {
			h.dropForm(f.key)
			h.answer(ctx, q.ID, TextCardChanged)
			return
		}
		h.answer(ctx, q.ID, "")
		h.wizardPrompt(ctx, f)
		return
	}
	f.back = backCard
	if verb == card.VerbComplete && f.typ == "todo" {
		for i := range f.done { // "Mark completed" on the card completes it as a whole
			f.done[i] = true
		}
	}
	h.addForm(f)
	h.formAction(ctx, q, l, c, s, f, verb, n)
}

// enforcedFor reports whether an approval counts as enforced (its body is never shown): the same rule as the form's.
func (h *Handler) enforcedFor(ctx context.Context, c caller, s spaceRef, a humanAction) bool {
	if a.Type != "approval" {
		return false
	}
	f := &form{}
	h.approvalOf(ctx, c, s, a, f)
	return f.enforced
}

// Where a form opened from a card goes back to: the card (a quick action opened a step), or the form (More…).
const (
	backCard = 1
	backForm = 2
)

// cardMoved reports whether the card under a form shows a newer state than the form: the notifier replaced the form
// with the card of a newer version, so the form is stale.
func (h *Handler) cardMoved(ctx context.Context, f *form) bool {
	if !f.fromCard || h.o.Card == nil {
		return false
	}
	ref, ok := h.o.Card(ctx, f.key.chat, f.key.msg)
	return ok && ref.Version > f.version
}

// cardView is an action as its card shows it in this chat (the notifier's view of the same action).
func (h *Handler) cardView(l updates.Link, s spaceRef, a humanAction, thread int, enforced bool) card.View {
	var req struct {
		Options     []string `json:"options"`
		AllowText   bool     `json:"allow_text"`
		RequiresAck bool     `json:"requires_ack"`
	}
	_ = json.Unmarshal(a.Request, &req)
	ca := &card.Action{ID: a.ID, Type: a.Type, Requester: a.RequesterLabel, Options: req.Options, AllowText: req.AllowText,
		RequiresAck: req.RequiresAck, Enforced: enforced, Status: a.Status, Outcome: a.Outcome, Reminders: a.ReminderCount,
		Escalated: a.EscalatedAt != nil}
	switch a.Assignee {
	case "":
	case l.UserID:
		ca.Claimed = "you"
	default:
		ca.Claimed = "someone else"
	}
	v := card.View{Label: card.Label(a.Type, a.TypeSlug), Title: a.Title, SpaceID: s.ID, Space: s.title(), ShowSpace: thread == 0,
		Priority: a.Priority, Full: h.full(s), Final: !a.pending(), Action: ca}
	if v.Full && !enforced {
		v.Body = a.Body
	}
	return v
}

// showCard draws the card of an action on its message again (settled when the action is), with the link's buttons:
// the first message of the card only; the notifier keeps the continuations. Through EditCard, so a newer state the
// notifier drew wins.
func (h *Handler) showCard(ctx context.Context, l updates.Link, s spaceRef, a humanAction, key pendingKey, thread int, enforced bool) bool {
	v := h.cardView(l, s, a, thread, enforced)
	var caps card.Caps
	if h.o.CardCaps != nil {
		caps = h.o.CardCaps(l.ID, key.chat, s.ID, a.ID)
	}
	p := card.Render(v, caps)[0]
	return h.editCard(ctx, key, a.ID, a.Version, false, v.Final, reply{text: p.Text, entities: p.Entities, markup: p.EditMarkup()})
}

// editCard puts rp on a card message through Options.EditCard. It never sends a new message: a card that can't be
// edited is left to the notifier.
func (h *Handler) editCard(ctx context.Context, key pendingKey, actionID string, version int64, form, final bool, rp reply) bool {
	do := func() bool {
		b := h.bot()
		if b == nil {
			return false
		}
		mk := rp.markup
		if mk == nil {
			mk = &botapi.InlineKeyboardMarkup{InlineKeyboard: [][]botapi.InlineKeyboardButton{}} // every edit states the keyboard
		}
		ectx, cancel := context.WithTimeout(ctx, apiTimeout)
		defer cancel()
		_, err := b.EditMessageText(ectx, botapi.EditMessageTextParams{ChatID: key.chat, MessageID: key.msg, Text: rp.text, Entities: rp.entities,
			LinkPreviewOptions: &botapi.LinkPreviewOptions{IsDisabled: true}, ReplyMarkup: mk})
		return err == nil || isNotModified(err)
	}
	if h.o.EditCard == nil {
		return do()
	}
	return h.o.EditCard(key.chat, key.msg, actionID, version, form, final, do)
}

func isNotModified(err error) bool {
	var e *botapi.Error
	return errors.As(err, &e) && e.Code == 400 && strings.Contains(e.Description, "message is not modified")
}

// settleCard collapses the card under a form whose action was just settled (answered, decided, completed, cancelled):
// a is the action the API returned; when it can't be read, the form's own view of it with the outcome stands in.
func (h *Handler) settleCard(ctx context.Context, l updates.Link, s spaceRef, f *form, body []byte, status, outcome string) {
	var a humanAction
	if json.Unmarshal(body, &a) != nil || a.ID != f.action || a.SpaceID != s.ID || a.pending() {
		a = humanAction{ID: f.action, SpaceID: s.ID, Type: f.typ, Title: f.title, Status: status, Outcome: outcome, Version: f.version + 1}
	}
	h.showCard(ctx, l, s, a, f.key, f.thread, f.enforced)
}

// backToCard drops a card's form and draws the card again from the server's current state.
func (h *Handler) backToCard(ctx context.Context, l updates.Link, c caller, s spaceRef, f *form) {
	h.dropForm(f.key)
	a, r := h.getAction(ctx, c, s, f.action)
	if !r.ok() {
		return // the notifier's card stays as the notifier left it
	}
	h.showCard(ctx, l, s, a, f.key, f.thread, f.enforced)
}
