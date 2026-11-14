package updates

import (
	"container/list"
	"context"
	"log/slog"
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

// UnlinkedText is the only thing an unlinked Telegram user is ever sent. It names no host, user or space.
const UnlinkedText = "This is a private SpaceMan bot. To use it, link your Telegram account in SpaceMan's web UI: " +
	"Account → Telegram → Link Telegram."

// Link is a linked Telegram user as the dispatcher sees it.
type Link struct {
	ID     string // tgl_…
	UserID string
	ChatID int64
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

// Dispatcher routes one update. v1 serves private chats only:
//   - the bot leaves any group, supergroup or channel it is added to, without reading anything about it;
//   - messages and button taps from anything but a private chat with a person are ignored;
//   - a linked user's update goes to the Handler;
//   - an unlinked user gets UnlinkedText at most once per UnlinkedPerUser, and at most one such reply goes out per
//     UnlinkedGlobal over every user; otherwise the update is dropped. Nothing is read for an unlinked user.
type Dispatcher struct {
	links   Links
	handler Handler
	bot     atomic.Pointer[botRef]
	clock   clock.Clock
	metrics *metrics.Metrics
	log     *slog.Logger

	perUser, global time.Duration

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
	d := &Dispatcher{links: o.Links, handler: o.Handler, clock: o.Clock, metrics: o.Metrics, log: o.Log,
		perUser: o.UnlinkedPerUser, global: o.UnlinkedGlobal, replied: newLRU(o.UnlinkedUsers)}
	d.SetBot(o.Bot)
	return d
}

type botRef struct{ Bot }

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
		d.leaveGroup(ctx, c)
		return
	}
	from, ok := sender(&u)
	if !ok {
		d.metrics.TelegramUpdate(metrics.TelegramUpdateIgnored)
		return
	}
	if d.links != nil {
		if l, ok := d.links.Lookup(from.ID); ok {
			d.metrics.TelegramUpdate(metrics.TelegramUpdateLinked)
			if d.handler != nil {
				d.handler.Handle(ctx, u, l)
			}
			return
		}
	}
	d.unlinked(ctx, &u, from.ID)
}

// leaveGroup leaves a group, supergroup or channel the bot was added to (v1 is private chats only; group binding is a
// later task). A status change that already removed the bot needs nothing.
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
