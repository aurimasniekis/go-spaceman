package notify

import (
	"context"
	"errors"
	"strings"

	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/card"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/sender"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/settings"
)

// Cards edited in place (ADR-0089 decisions 4, 6 and 9, plan §2.6 and T4): the card is rendered for each recipient by
// the card package, with its buttons; the chat handler acts on the card message and can show the action's form on it
// (EditCard), and the notifier's own later state of the resource replaces whatever the chat put there.

// view is a resource as the card shows it to one link, in one chat, now.
func (n *Notifier) view(l *Link, chat int64, r *Resource) card.View {
	full := settings.Full(n.o.Policy.Space(r.SpaceID), settings.IsGroupChat(chat))
	v := card.View{Label: r.Label, Title: r.Title, SpaceID: r.SpaceID, Space: r.SpaceName, ShowSpace: n.outsideTopic(l.ID, chat, r),
		State: r.State, Priority: r.Priority, Full: full, Final: r.Final}
	if full {
		v.Body = r.Detail
	}
	if r.Action != nil {
		a := *r.Action
		switch r.Assignee {
		case "":
			a.Claimed = ""
		case l.UserID:
			a.Claimed = "you"
		default:
			a.Claimed = "someone else"
		}
		v.Action = &a
	}
	return v
}

// outsideTopic reports whether the card is (or will be) outside the space's own topic, so its header names the space:
// a card that exists is where it was sent; a new one goes where the link's layout sends it.
func (n *Notifier) outsideTopic(linkID string, chat int64, r *Resource) bool {
	if cd := n.cards.get(chat, r.Key); cd != nil && cd.threadKnown {
		return cd.thread == 0
	}
	return n.o.Topics.Route(linkID, r.SpaceID) == ""
}

// caps is what a link's card offers, from memory (the link's subjects, the space's settings). Cosmetic only: a tap is
// authorized again by the API as the tapping link. A group chat gets no buttons (and no web_app button, which groups
// refuse).
func (n *Notifier) caps(l *Link, chat int64, r *Resource) card.Caps {
	if settings.IsGroupChat(chat) {
		return card.Caps{}
	}
	c := card.Caps{AppURL: n.itemURL(r.SpaceID, r.Key)}
	if r.Action != nil {
		subj := l.Subjects(n.o.Policy.Current().LinkSSOGrants, n.o.Now())
		c.Respond = n.o.Authz.AllowedAll(subj, r.SpaceID, authz.HumanRespond)
		c.Decide = n.o.Authz.AllowedAll(subj, r.SpaceID, authz.ApprovalDecide)
		c.EnforcedInChat = n.o.Policy.Space(r.SpaceID).EnforcedApprovals == settings.ApprovalsConfirm
	}
	return c
}

func (n *Notifier) itemURL(spaceID string, k Key) string {
	if n.o.ItemURL != nil {
		if u := n.o.ItemURL(spaceID, k); u != "" {
			return u
		}
	}
	return n.o.AppURL
}

// retract queues the edit of a link's card to card.Gone, when the link has a card of the resource that still shows
// it. It shares the card's coalescing key, so it replaces a content edit still waiting in the sender.
func (n *Notifier) retract(snd *sender.Sender, l *Link, r *Resource) {
	cd, _ := n.cards.state(l.ChatID, r.Key)
	if cd == nil || cd.gone {
		return
	}
	chat, linkID, k := l.ChatID, l.ID, r.Key
	err := snd.Enqueue(sender.Message{ChatID: chat, Priority: sender.Edit, Key: "card:" + r.String(),
		Send: func(ctx context.Context, c *botapi.Client) error {
			if n.currentLink(linkID, chat) == nil {
				return nil // unlinked or moved: the bot no longer writes to that chat for this link
			}
			return n.cards.retract(ctx, c, chat, k)
		}})
	if err != nil {
		n.log.Debug("telegram notifier: retraction refused")
	}
}

// sendAlert sends a reminder, escalation or assignment alert as a reply to the resource's card, in the card's thread,
// so it makes a sound next to the item. Without a card (or when the card or its topic is gone) it goes where the
// layout sends it, on its own.
func (n *Notifier) sendAlert(ctx context.Context, c *botapi.Client, linkID string, chat int64, r *Resource, p botapi.SendMessageParams) (botapi.Message, error) {
	route := n.sender(r, linkID)
	cd := n.cards.get(chat, r.Key)
	if cd == nil || cd.gone {
		return route(ctx, c, p)
	}
	p.ReplyParameters = &botapi.ReplyParameters{MessageID: cd.messageID}
	m, err := n.cards.sendInCardThread(ctx, c, route, chat, r.Key, p)
	if err == nil || !botapi.IsTopicGone(err) && !isReplyGone(err) {
		return m, err
	}
	if err := sender.Take(ctx); err != nil {
		return botapi.Message{}, err
	}
	p.ReplyParameters = nil
	return route(ctx, c, p)
}

// isReplyGone: the message a reply was addressed to is gone.
func isReplyGone(err error) bool {
	var e *botapi.Error
	return errors.As(err, &e) && e.Code == 400 && strings.Contains(e.Description, "message to be replied not found")
}

// CardStore finds a card's row by its message (StoreMappings).
type CardStore interface {
	Card(ctx context.Context, chatID int64, messageID int) (Mapping, error)
}

// Card returns the card a chat message is, with the link and space it was sent for and the version it shows: from
// memory first, so a card sent within the last flush interval (its row not yet written) is found, then from the
// store. ok is false for a message that is no card the bot recorded.
func (n *Notifier) Card(ctx context.Context, chat int64, msg int) (Mapping, bool) {
	if k, cd := n.cards.byMessage(chat, msg); cd != nil && cd.linkID != "" {
		return Mapping{ChatID: chat, MessageID: msg, LinkID: cd.linkID, SpaceID: cd.spaceID, Key: k, Version: cd.version,
			Kind: MappingCard, Thread: cd.thread, ThreadKnown: cd.threadKnown}, true
	}
	cs, ok := n.o.Store.(CardStore)
	if !ok {
		return Mapping{}, false
	}
	m, err := cs.Card(ctx, chat, msg)
	if err != nil {
		return Mapping{}, false
	}
	if _, cd := n.cards.byMessage(chat, msg); cd != nil && cd.version > m.Version {
		m.Version = cd.version // memory is newer than the row (an edit not yet flushed)
	}
	return m, true
}

// ChatEdit describes what the chat handler puts on a card message: the action at Version, as its form (Form) or as
// the card again, settled when Final.
type ChatEdit struct {
	Version     int64
	Form, Final bool
}

// EditCard runs edit, the chat handler's edit of a card message, under the chat's card lock, unless the card already
// shows something that must win: a later version (shown, or queued in the sender), the same version settled while the
// chat's isn't, or card.Gone (the link may no longer see it). It reports whether edit ran and succeeded; the card then counts as showing what e says,
// so the notifier's next edit of the same version doesn't undo it, while a newer state of the resource replaces it.
// A message that is no card known here is edited as is.
func (n *Notifier) EditCard(chat int64, msg int, k Key, e ChatEdit, edit func() bool) bool {
	mu := n.cards.chatLock(chat)
	mu.Lock()
	defer mu.Unlock()
	cd := n.cards.get(chat, k)
	if cd != nil && cd.messageID != msg {
		cd = nil
	}
	if cd != nil && (cd.gone || cd.version > e.Version || cd.queued > e.Version || cd.version == e.Version && cd.final && !e.Final) {
		return false
	}
	if !edit() {
		return false
	}
	if cd != nil {
		n.cards.shown(chat, k, e.Version, e.Form, e.Final)
	}
	return true
}

// CardCaps is what a link's card of a human action offers (the buttons the notifier would draw), for the chat
// handler's redraw of the card. Cosmetic only.
func (n *Notifier) CardCaps(linkID string, chat int64, spaceID, actionID string) card.Caps {
	l := n.currentLink(linkID, chat)
	if l == nil {
		return card.Caps{}
	}
	return n.caps(l, chat, &Resource{Key: Key{TypeHumanAction, actionID}, SpaceID: spaceID, Action: &card.Action{}})
}
