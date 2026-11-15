package updates

import (
	"container/list"
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/obs/metrics"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
)

// Unlinked-user reply bounds (ADR-0083 decision 9).
const (
	DefaultUnlinkedPerUser = time.Hour   // one reply per Telegram user per hour
	DefaultUnlinkedGlobal  = time.Second // one reply to any unlinked user per second
	DefaultUnlinkedUsers   = 10000       // users remembered for the per-user bound (LRU)
	sendTimeout            = 15 * time.Second
)

// SlowDownText answers a linked user over the inbound action limit, once until an action is allowed again.
const SlowDownText = "Slow down: too many requests. Try again in a minute."

// UnlinkedText is the only thing an unlinked Telegram user is ever sent. It names no host, user or space.
const UnlinkedText = "This is a private SpaceMan bot. To use it, link your Telegram account in SpaceMan's web UI: " +
	"Account → Telegram → Link Telegram."

// Link is a linked Telegram user as the dispatcher sees it.
type Link struct {
	ID     string // tgl_…
	UserID string
	ChatID int64
	// ReconfirmDueAt is when the link must be re-confirmed from the web (zero: never). Past it the link is treated as
	// suspended: the user gets ReconfirmText and nothing else.
	ReconfirmDueAt time.Time
	// Group is set when the update comes from a group chat bound to a space (group mode, ADR-0083 amendment TG-13).
	// Then ChatID is the group's chat (where replies go) and the link is the tapping person's own, never the binder's.
	Group *Group
}

// Group is the bound group chat an update comes from.
type Group struct {
	ChatID  int64  // the forum supergroup (negative)
	SpaceID string // the space it is bound to: the only space a group update may act on
}

// Groups finds the space a group chat is bound to (settings.Service.GroupSpace). Like Links it answers from memory.
type Groups interface {
	GroupSpace(chatID int64) (spaceID string, ok bool)
}

// GroupUnlinkedText answers a tap in a bound group by a member without a usable link. It is the tap's answer, which
// Telegram shows only to the tapper, and names no host, user or space.
const GroupUnlinkedText = "Link your Telegram account in SpaceMan's web UI first (Account → Telegram)."

// ReconfirmText is what a user whose link needs its periodic web re-confirmation is sent.
const ReconfirmText = "Your SpaceMan link needs to be re-confirmed. Open SpaceMan's web UI: Account → Telegram → Re-confirm."

// Unlinker ends a linked user's own link when they send /unlink in the bot (ADR-0083). The reply is the bot's text.
type Unlinker interface {
	Unlink(ctx context.Context, l Link) string
}

// Links finds the active link of a Telegram user. It must answer from memory: it runs for every update, including
// those of users who aren't linked, and must never read the database on their behalf.
type Links interface {
	Lookup(tgUserID int64) (Link, bool)
}

// Handler handles an update from a linked user (the chat surfaces of later tasks).
type Handler interface {
	Handle(ctx context.Context, u botapi.Update, l Link)
}

// LinkReply is what the dispatcher sends for a linking step (Linker.HandleUnlinked). Every field is optional.
type LinkReply struct {
	// Text (with Entities and Markup) is sent to the user's private chat.
	Text     string
	Entities []botapi.MessageEntity
	Markup   *botapi.InlineKeyboardMarkup
	// CallbackText answers a button tap (a tap is always answered, with an empty answer when this is empty).
	CallbackText string
	// ClearKeyboard removes the keyboard of the message whose button was tapped, so it can't be tapped again.
	ClearKeyboard bool
	// Bounded marks a refusal: its text counts against the unlinked-user reply bounds like UnlinkedText, so a stranger
	// sending made-up codes gets no more replies than one sending anything else.
	Bounded bool
}

// Linker handles the linking steps a user without a link takes in the bot (ADR-0083 decision 2): /start <code> and
// the [Confirm] / [This wasn't me] buttons. Claims must decide from the update alone, without any read.
type Linker interface {
	Claims(u *botapi.Update) bool
	HandleUnlinked(ctx context.Context, u botapi.Update, from botapi.User) LinkReply
}

// keyboardEditor is the optional part of a Bot that removes a message's keyboard (*botapi.Client has it).
type keyboardEditor interface {
	EditMessageReplyMarkup(ctx context.Context, p botapi.EditMessageReplyMarkupParams) (botapi.Message, error)
}

// Bot is the part of the Bot API the dispatcher calls itself. *botapi.Client satisfies it.
type Bot interface {
	SendMessage(ctx context.Context, p botapi.SendMessageParams) (botapi.Message, error)
	LeaveChat(ctx context.Context, chatID int64) error
	AnswerCallbackQuery(ctx context.Context, p botapi.AnswerCallbackQueryParams) error
}

// DispatcherOptions configure a Dispatcher. Zero durations and sizes take the defaults.
type DispatcherOptions struct {
	// Links is nil until linking exists: then every user is unlinked.
	Links Links
	// Handler is nil until the chat surfaces exist: then linked users' updates are dropped.
	Handler Handler
	// Linker handles the linking steps of users without a link (nil: they only ever get UnlinkedText).
	Linker Linker
	// Unlinker handles /unlink from a linked user (nil: /unlink goes to the Handler like any message).
	Unlinker Unlinker
	// Groups maps a group chat to its bound space (nil: no group mode; the bot leaves every group).
	Groups Groups
	// GroupGrace is how long the bot waits in an unbound forum supergroup before it leaves (default 15 minutes).
	GroupGrace time.Duration
	// Limiter enforces the per-link inbound limits (nil: none). Every linked user's update costs one KindAction.
	Limiter *Limiter
	// Bot sends the dispatcher's own replies; SetBot replaces it when the bot is (re)configured. Without one, nothing
	// is sent.
	Bot Bot

	UnlinkedPerUser time.Duration
	UnlinkedGlobal  time.Duration
	UnlinkedUsers   int

	Clock   clock.Clock
	Metrics *metrics.Metrics
	Log     *slog.Logger
}

// Dispatcher routes one update. It serves private chats, and supergroups bound to a space (group mode):
//   - the bot leaves any group, supergroup or channel it is added to that isn't bound, without reading anything about it;
//   - in a bound supergroup, a button tap, a command or a reply goes to the Handler as the tapping person's own link
//     (groupUpdate); membership in the group grants nothing;
//   - other messages and button taps from anything but a private chat with a person are ignored;
//   - a linked user's update goes to the Handler;
//   - an unlinked user gets UnlinkedText at most once per UnlinkedPerUser, and at most one such reply goes out per
//     UnlinkedGlobal over every user; otherwise the update is dropped. Nothing is read for an unlinked user, with one
//     exception: a linking step the Linker claims (/start <code>, a link button), which reads only the code it names
//     (a 256-bit secret, or its hash in a button the bot sent) and the Telegram user's own link.
type Dispatcher struct {
	links   Links
	handler atomic.Pointer[handlerRef]
	linker  Linker
	unlink  Unlinker
	groups  Groups
	limiter *Limiter
	bot     atomic.Pointer[botRef]
	clock   clock.Clock
	metrics *metrics.Metrics
	log     *slog.Logger

	perUser, global time.Duration
	grace           time.Duration // how long the bot waits in an unbound forum supergroup (groupwait.go)
	wait            groupWait

	mu         sync.Mutex
	lastGlobal time.Time
	replied    *lru
}

// NewDispatcher returns a dispatcher.
func NewDispatcher(o DispatcherOptions) *Dispatcher {
	if o.UnlinkedPerUser <= 0 {
		o.UnlinkedPerUser = DefaultUnlinkedPerUser
	}
	if o.UnlinkedGlobal <= 0 {
		o.UnlinkedGlobal = DefaultUnlinkedGlobal
	}
	if o.UnlinkedUsers <= 0 {
		o.UnlinkedUsers = DefaultUnlinkedUsers
	}
	if o.Clock == nil {
		o.Clock = clock.System{}
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.GroupGrace <= 0 {
		o.GroupGrace = DefaultGroupGrace
	}
	d := &Dispatcher{grace: o.GroupGrace, links: o.Links, linker: o.Linker, unlink: o.Unlinker, groups: o.Groups, limiter: o.Limiter, clock: o.Clock, metrics: o.Metrics, log: o.Log,
		perUser: o.UnlinkedPerUser, global: o.UnlinkedGlobal, replied: newLRU(o.UnlinkedUsers)}
	d.SetBot(o.Bot)
	d.SetHandler(o.Handler)
	return d
}

type botRef struct{ Bot }

type handlerRef struct{ Handler }

// SetHandler replaces the handler of linked users' updates (nil: they are dropped). The chat surfaces need the API
// and the bot, which exist after the dispatcher does.
func (d *Dispatcher) SetHandler(h Handler) {
	if h == nil {
		d.handler.Store(nil)
		return
	}
	d.handler.Store(&handlerRef{h})
}

// Bot returns the client the dispatcher replies with (nil: none), for the chat surfaces that reply the same way.
func (d *Dispatcher) Bot() Bot { return d.client() }

// SetBot replaces the client the dispatcher replies with (nil: send nothing).
func (d *Dispatcher) SetBot(b Bot) {
	if b == nil {
		d.bot.Store(nil)
		return
	}
	d.bot.Store(&botRef{b})
}

func (d *Dispatcher) client() Bot {
	if r := d.bot.Load(); r != nil {
		return r.Bot
	}
	return nil
}

// sender returns the person an update comes from and whether it is from a private chat with them; ok is false for
// anything the dispatcher doesn't route to a link (other kinds, bots, chats that aren't private, mismatched chats).
func sender(u *botapi.Update) (from botapi.User, ok bool) {
	switch {
	case u.Message != nil:
		m := u.Message
		if m.From == nil || m.Chat.Type != botapi.ChatPrivate || m.Chat.ID != m.From.ID {
			return botapi.User{}, false
		}
		from = *m.From
	case u.CallbackQuery != nil:
		q := u.CallbackQuery
		if m := q.Message; m != nil && (m.Chat.Type != botapi.ChatPrivate || m.Chat.ID != q.From.ID) {
			return botapi.User{}, false
		}
		from = q.From
	case u.MyChatMember != nil:
		c := u.MyChatMember
		if c.Chat.Type != botapi.ChatPrivate || c.Chat.ID != c.From.ID {
			return botapi.User{}, false
		}
		from = c.From
	default:
		return botapi.User{}, false
	}
	return from, from.ID > 0 && !from.IsBot
}

// Dispatch handles one update; the Intake's workers call it.
func (d *Dispatcher) Dispatch(ctx context.Context, u botapi.Update) {
	if c := u.MyChatMember; c != nil && c.Chat.Type != botapi.ChatPrivate {
		if _, ok := d.boundGroup(c.Chat); ok {
			d.metrics.TelegramUpdate(metrics.TelegramUpdateIgnored)
			return
		}
		if canBeBound(c.Chat) && c.NewChatMember.Status != botapi.MemberLeft && c.NewChatMember.Status != botapi.MemberKicked {
			// A forum supergroup waits for its binding (binding checks that the bot is a member): nothing is read or
			// answered there until then, and not for longer than the grace (groupwait.go).
			d.startWaiting(c.Chat.ID)
			d.metrics.TelegramUpdate(metrics.TelegramUpdateIgnored)
			return
		}
		d.leaveGroup(ctx, c)
		return
	}
	d.SweepGroups(ctx)
	if g, from, ok := d.groupUpdate(&u); ok {
		d.dispatchGroup(ctx, u, g, from)
		return
	}
	if d.groupActivity(ctx, &u) {
		return
	}
	from, ok := sender(&u)
	if !ok {
		d.metrics.TelegramUpdate(metrics.TelegramUpdateIgnored)
		return
	}
	if d.links != nil {
		if l, ok := d.links.Lookup(from.ID); ok {
			if !l.ReconfirmDueAt.IsZero() && !d.clock.Now().Before(l.ReconfirmDueAt) {
				// An overdue link acts for nobody, but its owner can still end it.
				if d.unlink != nil && isUnlink(&u) {
					d.send(ctx, from.ID, d.unlink.Unlink(ctx, l))
					return
				}
				d.reconfirm(ctx, &u, from.ID)
				return
			}
			if d.limiter != nil {
				if ok, warn := d.limiter.allow(l.ID, KindAction); !ok {
					d.throttled(ctx, &u, l, warn)
					return
				}
			}
			d.metrics.TelegramUpdate(metrics.TelegramUpdateLinked)
			if d.unlink != nil && isUnlink(&u) {
				d.send(ctx, from.ID, d.unlink.Unlink(ctx, l))
				return
			}
			if h := d.handler.Load(); h != nil {
				h.Handle(ctx, u, l)
			}
			return
		}
	}
	if d.linker != nil && d.linker.Claims(&u) {
		d.link(ctx, &u, from)
		return
	}
	d.unlinked(ctx, &u, from.ID)
}

// boundGroup returns the group mode binding of a chat: only a supergroup with a negative ID bound to a visible space.
func (d *Dispatcher) boundGroup(c botapi.Chat) (Group, bool) {
	if d.groups == nil || c.Type != botapi.ChatSupergroup || c.ID >= 0 {
		return Group{}, false
	}
	sp, ok := d.groups.GroupSpace(c.ID)
	if !ok || sp == "" {
		return Group{}, false
	}
	return Group{ChatID: c.ID, SpaceID: sp}, true
}

// groupUpdate picks out an update of a bound group: a button tap on one of the group's messages, or a message from a
// person. The person is always the update's From (the tapper or the author), never the chat or a sender_chat: an
// anonymous admin posts as a bot account and is ignored.
func (d *Dispatcher) groupUpdate(u *botapi.Update) (Group, botapi.User, bool) {
	var chat botapi.Chat
	var from botapi.User
	switch {
	case u.CallbackQuery != nil:
		q := u.CallbackQuery
		if q.Message == nil {
			return Group{}, botapi.User{}, false
		}
		chat, from = q.Message.Chat, q.From
	case u.Message != nil:
		m := u.Message
		if m.From == nil {
			return Group{}, botapi.User{}, false
		}
		chat, from = m.Chat, *m.From
	default:
		return Group{}, botapi.User{}, false
	}
	if chat.Type == botapi.ChatPrivate {
		return Group{}, botapi.User{}, false
	}
	g, ok := d.boundGroup(chat)
	return g, from, ok && from.ID > 0 && !from.IsBot
}

// dispatchGroup routes an update of a bound group. It is authorized as the tapping person's OWN link and nothing else:
// a member without an active link (or with one overdue for re-confirmation) gets a refusal on a tap (the tap's answer,
// seen only by them) and nothing for a message. Only commands and replies to a message reach the Handler; other
// chatter in the group is never read, and the linking steps (/start, /unlink) are never taken in a group.
func (d *Dispatcher) dispatchGroup(ctx context.Context, u botapi.Update, g Group, from botapi.User) {
	if m := u.Message; m != nil && !strings.HasPrefix(strings.TrimSpace(m.Text), "/") && m.ReplyToMessage == nil {
		d.metrics.TelegramUpdate(metrics.TelegramUpdateIgnored)
		return
	}
	var l Link
	ok := false
	if d.links != nil {
		l, ok = d.links.Lookup(from.ID)
	}
	if !ok || !l.ReconfirmDueAt.IsZero() && !d.clock.Now().Before(l.ReconfirmDueAt) {
		d.groupRefuse(ctx, &u)
		return
	}
	if d.limiter != nil {
		if ok, warn := d.limiter.allow(l.ID, KindAction); !ok {
			if u.CallbackQuery != nil {
				d.throttled(ctx, &u, l, warn)
			}
			return
		}
	}
	d.metrics.TelegramUpdate(metrics.TelegramUpdateLinked)
	l.ChatID = g.ChatID
	l.Group = &g
	if h := d.handler.Load(); h != nil {
		h.Handle(ctx, u, l)
	}
}

// groupRefuse answers a bound group's update from a member who can't act: a tap gets GroupUnlinkedText as an alert
// (an answerCallbackQuery with show_alert, which only the tapper sees); a message gets nothing, so strangers can't make the bot talk in the group.
func (d *Dispatcher) groupRefuse(ctx context.Context, u *botapi.Update) {
	d.metrics.TelegramUpdate(metrics.TelegramUpdateUnlinkedDropped)
	bot := d.client()
	if bot == nil || u.CallbackQuery == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	if err := bot.AnswerCallbackQuery(ctx, botapi.AnswerCallbackQueryParams{CallbackQueryID: u.CallbackQuery.ID, Text: GroupUnlinkedText, ShowAlert: true}); err != nil {
		d.log.Debug("telegram: a group button tap was not answered", slog.String("error", err.Error()))
	}
}

// isUnlink reports whether a message is the /unlink command (alone, or addressed to a bot: /unlink@name).
func isUnlink(u *botapi.Update) bool {
	if u.Message == nil {
		return false
	}
	t := strings.TrimSpace(u.Message.Text)
	return t == "/unlink" || strings.HasPrefix(t, "/unlink@") && !strings.ContainsAny(t, " \t\n")
}

// send sends a plain text to a user's private chat; a failure is only logged.
func (d *Dispatcher) send(ctx context.Context, chatID int64, text string) {
	bot := d.client()
	if bot == nil || text == "" {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	if _, err := bot.SendMessage(ctx, botapi.SendMessageParams{ChatID: chatID, Text: text, LinkPreviewOptions: &botapi.LinkPreviewOptions{IsDisabled: true}}); err != nil {
		d.log.Debug("telegram: a reply was not sent", slog.String("error", err.Error()))
		d.metrics.TelegramUpdate(metrics.TelegramUpdateFailed)
	}
}

// reconfirm answers a linked user whose periodic web re-confirmation is overdue: the link does nothing until then, so
// the update goes nowhere and the user gets the instructions, within the same bounds as an unlinked user's.
func (d *Dispatcher) reconfirm(ctx context.Context, u *botapi.Update, tgUserID int64) {
	if bot := d.client(); bot != nil && u.CallbackQuery != nil {
		d.answerCallback(ctx, bot, u.CallbackQuery.ID)
	}
	if u.MyChatMember != nil || d.client() == nil || !d.allowReply(tgUserID) {
		d.metrics.TelegramUpdate(metrics.TelegramUpdateUnlinkedDropped)
		return
	}
	d.send(ctx, tgUserID, ReconfirmText)
	d.metrics.TelegramUpdate(metrics.TelegramUpdateUnlinkedReplied)
}

// link runs a linking step of a user without a link and sends its reply: the tap's answer always, the keyboard
// removal when asked, and the text unless it is a refusal over the unlinked-user reply bounds.
func (d *Dispatcher) link(ctx context.Context, u *botapi.Update, from botapi.User) {
	d.metrics.TelegramUpdate(metrics.TelegramUpdateLink)
	r := d.linker.HandleUnlinked(ctx, *u, from)
	bot := d.client()
	if bot == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	if q := u.CallbackQuery; q != nil {
		if err := bot.AnswerCallbackQuery(ctx, botapi.AnswerCallbackQueryParams{CallbackQueryID: q.ID, Text: r.CallbackText}); err != nil {
			d.log.Debug("telegram: a link button tap was not answered", slog.String("error", err.Error()))
		}
		if e, ok := bot.(keyboardEditor); ok && r.ClearKeyboard && q.Message != nil && q.Message.MessageID != 0 {
			if _, err := e.EditMessageReplyMarkup(ctx, botapi.EditMessageReplyMarkupParams{ChatID: from.ID, MessageID: q.Message.MessageID}); err != nil {
				d.log.Debug("telegram: a link keyboard was not removed", slog.String("error", err.Error()))
			}
		}
	}
	if r.Text == "" || r.Bounded && !d.allowReply(from.ID) {
		return
	}
	p := botapi.SendMessageParams{ChatID: from.ID, Text: r.Text, Entities: r.Entities, LinkPreviewOptions: &botapi.LinkPreviewOptions{IsDisabled: true}}
	if r.Markup != nil {
		p.ReplyMarkup = r.Markup
	}
	if _, err := bot.SendMessage(ctx, p); err != nil {
		d.log.Debug("telegram: a link reply was not sent", slog.String("error", err.Error()))
		d.metrics.TelegramUpdate(metrics.TelegramUpdateFailed)
	}
}

// throttled answers an update over the link's action limit: a button tap always gets its (required) answer, with the
// slow-down text the first time; a message gets the text as a reply only the first time. Nothing else happens.
func (d *Dispatcher) throttled(ctx context.Context, u *botapi.Update, l Link, warn bool) {
	d.metrics.TelegramUpdate(metrics.TelegramUpdateThrottled)
	bot := d.client()
	if bot == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	switch {
	case u.CallbackQuery != nil:
		p := botapi.AnswerCallbackQueryParams{CallbackQueryID: u.CallbackQuery.ID}
		if warn {
			p.Text = SlowDownText
			p.ShowAlert = l.Group != nil
		}
		if err := bot.AnswerCallbackQuery(ctx, p); err != nil {
			d.log.Debug("telegram: a throttled button tap was not answered", slog.String("error", err.Error()))
		}
	case u.Message != nil && warn:
		if _, err := bot.SendMessage(ctx, botapi.SendMessageParams{ChatID: l.ChatID, Text: SlowDownText}); err != nil {
			d.log.Debug("telegram: the slow-down notice was not sent", slog.String("error", err.Error()))
		}
	}
}

// LeaveGroup makes the bot leave a group that no longer acts for a space (its binding or its space is gone). Failures
// are logged; the group is ignored by the dispatcher either way.
func (d *Dispatcher) LeaveGroup(ctx context.Context, chatID int64) {
	d.wait.mu.Lock()
	delete(d.wait.deadline, chatID)
	d.wait.mu.Unlock()
	bot := d.client()
	if bot == nil || chatID >= 0 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	if err := bot.LeaveChat(ctx, chatID); err != nil {
		d.log.Warn("telegram: leaving a group that lost its binding failed", slog.String("error", err.Error()))
		d.metrics.TelegramUpdate(metrics.TelegramUpdateFailed)
		return
	}
	d.metrics.TelegramUpdate(metrics.TelegramUpdateLeftChat)
}

// leaveGroup leaves a group, supergroup or channel the bot was added to that can't be bound to a space. A status change that already removed the bot needs nothing.
func (d *Dispatcher) leaveGroup(ctx context.Context, c *botapi.ChatMemberUpdated) {
	switch c.NewChatMember.Status {
	case botapi.MemberLeft, botapi.MemberKicked:
		d.metrics.TelegramUpdate(metrics.TelegramUpdateIgnored)
		return
	}
	bot := d.client()
	if bot == nil {
		d.metrics.TelegramUpdate(metrics.TelegramUpdateIgnored)
		return
	}
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	if err := bot.LeaveChat(ctx, c.Chat.ID); err != nil {
		d.log.Warn("telegram: leaving a group failed", slog.String("error", err.Error()))
		d.metrics.TelegramUpdate(metrics.TelegramUpdateFailed)
		return
	}
	d.metrics.TelegramUpdate(metrics.TelegramUpdateLeftChat)
}

// unlinked answers a user without a link: the instructions, within the per-user and global bounds, or nothing. Only
// a message or a button tap is answered; a membership change (the user blocking or starting the bot) never is. A
// button tap is always acknowledged with an empty answerCallbackQuery, so the client's spinner stops (Telegram requires
// an answer); Telegram only sends one for a button on a message the bot sent, so it isn't a channel strangers control.
func (d *Dispatcher) unlinked(ctx context.Context, u *botapi.Update, tgUserID int64) {
	bot := d.client()
	if bot != nil && u.CallbackQuery != nil {
		d.answerCallback(ctx, bot, u.CallbackQuery.ID)
	}
	if bot == nil || u.MyChatMember != nil || !d.allowReply(tgUserID) {
		d.metrics.TelegramUpdate(metrics.TelegramUpdateUnlinkedDropped)
		return
	}
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	// In a private chat the chat ID is the user ID.
	if _, err := bot.SendMessage(ctx, botapi.SendMessageParams{ChatID: tgUserID, Text: UnlinkedText,
		LinkPreviewOptions: &botapi.LinkPreviewOptions{IsDisabled: true}}); err != nil {
		d.log.Debug("telegram: the linking instructions were not sent", slog.String("error", err.Error()))
		d.metrics.TelegramUpdate(metrics.TelegramUpdateFailed)
		return
	}
	d.metrics.TelegramUpdate(metrics.TelegramUpdateUnlinkedReplied)
}

// answerCallback acknowledges a button tap without text. A failure is only logged: the update is still handled.
func (d *Dispatcher) answerCallback(ctx context.Context, bot Bot, id string) {
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	if err := bot.AnswerCallbackQuery(ctx, botapi.AnswerCallbackQueryParams{CallbackQueryID: id}); err != nil {
		d.log.Debug("telegram: a button tap was not answered", slog.String("error", err.Error()))
	}
}

// allowReply takes a reply slot for tgUserID: none while the global interval hasn't passed (without touching the
// user's entry, so they can get one later), none while the user's own interval hasn't passed.
func (d *Dispatcher) allowReply(tgUserID int64) bool {
	now := d.clock.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.lastGlobal.IsZero() && now.Sub(d.lastGlobal) < d.global {
		return false
	}
	if at, ok := d.replied.get(tgUserID); ok && now.Sub(at) < d.perUser {
		return false
	}
	d.replied.put(tgUserID, now)
	d.lastGlobal = now
	return true
}

// lru maps Telegram user IDs to their last reply, holding at most size entries (the least recently used goes first).
type lru struct {
	size  int
	order *list.List // front: most recent; values are *lruEntry
	items map[int64]*list.Element
}

type lruEntry struct {
	id int64
	at time.Time
}

func newLRU(size int) *lru {
	return &lru{size: size, order: list.New(), items: make(map[int64]*list.Element, size)}
}

func (l *lru) get(id int64) (time.Time, bool) {
	e, ok := l.items[id]
	if !ok {
		return time.Time{}, false
	}
	l.order.MoveToFront(e)
	return e.Value.(*lruEntry).at, true //nolint:forcetypeassert // the list holds only *lruEntry
}

func (l *lru) put(id int64, at time.Time) {
	if e, ok := l.items[id]; ok {
		e.Value.(*lruEntry).at = at //nolint:forcetypeassert // the list holds only *lruEntry
		l.order.MoveToFront(e)
		return
	}
	l.items[id] = l.order.PushFront(&lruEntry{id: id, at: at})
	if l.order.Len() > l.size {
		old := l.order.Back()
		l.order.Remove(old)
		delete(l.items, old.Value.(*lruEntry).id) //nolint:forcetypeassert // the list holds only *lruEntry
	}
}

func (l *lru) len() int { return l.order.Len() }
