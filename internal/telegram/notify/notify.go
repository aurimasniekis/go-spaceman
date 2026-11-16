// Package notify turns the change feed into Telegram notifications without fan-out (ADR-0078, ADR-0083, plan §4.2).
//
// One Notifier runs in the central process. The eventbus change topic is only a doorbell: changes are read from the
// `changes` table after an in-memory cursor that starts at the newest change (history is never replayed, and no cursor
// is stored, so nothing is written per tick). A wake-up opens a 250 ms window; the window's changes are reduced to
// distinct (resource type, resource ID) keys, capped at 500; each key is read once, as the system, and its recipients
// are computed in memory from the cached links (the same Casbin check the API applies to the link's principal, the
// human type's view role, the space's Telegram visibility and content level, and the link's preferences). Nothing
// asks the database per recipient, and a window that sends nothing writes nothing.
//
// A resource's card is one Telegram message: the first notification sends it, later changes (its final state) edit
// it, through the sender's coalescing key (chat, resource), so a newer state replaces an unsent older one. The
// (chat, message) mappings of the cards that were sent are written in batches (one transaction a second at most, or
// 100 rows), never per send.
//
// Telegram's own state (codes, links, settings) writes no change-feed entries, so nothing here sees it; the link
// cache follows the telegram.link, grants.changed and user.changed topics.
package notify

import (
	"context"
	"log/slog"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/eventbus"
	"github.com/aurimasniekis/go-spaceman/internal/human"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/card"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/sender"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/settings"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/topics"
)

// Bounds of the consumer (ADR-0078).
const (
	// Window is how long changes are collected after a wake-up.
	Window = 250 * time.Millisecond
	// MaxKeys caps the distinct resources of one window; beyond it, one "many changes" message goes to each affected chat.
	MaxKeys = 500
	// BatchSize is how many changes one read returns.
	BatchSize = 200
	// maxSpaces caps the spaces remembered for the overflow notice.
	maxSpaces = 500
)

// Resource types the notifier follows.
const (
	TypeHumanAction = "human_action"
	TypeInboxItem   = "inbox_item"
	TypeTask        = "task"
	TypeMessage     = "message"
	TypeRun         = "run"
)

var relevant = map[string]bool{TypeHumanAction: true, TypeInboxItem: true, TypeTask: true, TypeMessage: true, TypeRun: true}

// Key identifies a resource.
type Key struct{ Type, ID string }

func (k Key) String() string { return k.Type + ":" + k.ID }

// Kind says which notification preference governs a resource's notifications.
type Kind uint8

// Kinds.
const (
	KindNone Kind = iota
	KindAction
	KindInbox
	KindTaskReview
	KindFailedRun
	KindMessage
)

// Resource is the state of one resource, read once per window.
type Resource struct {
	Key
	SpaceID string
	Version int64
	Kind    Kind
	// Perm is the read permission the API checks for the resource's type (authz.HumanRead, …).
	Perm authz.Permission
	// ViewRole is the human type's view role (human actions); "" or "viewer" needs nothing beyond Perm.
	ViewRole string
	// Label is the short type name shown on the card ("Question", "Task").
	Label string
	Title string
	// Detail is the body shown only when the space's content level is full.
	Detail string
	// State is a short status line ("open", "resolved: approved").
	State string
	// Priority and Assignee (a user ID) drive the new-action preferences.
	Priority string
	Assignee string
	// Final: the resource reached its final state: a card that exists is edited, and none is sent if there is none.
	Final bool
	// RemindedAt and EscalatedAt (microseconds, 0: never) are when the action's latest reminder and its escalation
	// happened; a card that already exists announces a newer one as a message of its own.
	RemindedAt, EscalatedAt int64
	// Notify: the state is worth a notification when no card exists yet (an open action, a task in review, a failed run).
	Notify bool
	// SpaceName is the space's name, shown in a card's header outside the space's own topic.
	SpaceName string
	// Action is what a human action's card shows beyond the common fields and offers as buttons (nil for the other
	// types). Its Claimed is set per recipient when the card is rendered.
	Action *card.Action
}

// Reader reads a resource once for a window. It returns nil for a resource that is gone or of no interest. scope is
// per window: the reader may keep what it loaded for the window there (type permissions per space) and nowhere else.
type Reader interface {
	Read(ctx context.Context, scope map[string]any, k Key) (*Resource, error)
}

// Feed is the change feed.
type Feed interface {
	Latest(ctx context.Context) (int64, error)
	After(ctx context.Context, after int64, limit int) ([]changefeed.Change, error)
}

// Cursors keeps the change-feed position across restarts, so the changes made while central was down are delivered
// when it starts again (bounded: see Options.MaxReplay).
type Cursors interface {
	// Load returns the saved position and when it was saved; ok is false when none was.
	Load(ctx context.Context) (seq int64, at time.Time, ok bool, err error)
	Save(ctx context.Context, seq int64, at time.Time) error
}

const (
	// DefaultMaxReplay is how old a saved position may be and still be resumed from; an older one starts at the newest
	// change, so a long outage doesn't flood a chat with history.
	DefaultMaxReplay = 5 * time.Minute
	// cursorEvery is the least time between two saves of an unchanged position: the save time is what tells a restart
	// how long it was down, so an idle central keeps it fresh.
	cursorEvery = time.Minute
	// saveGrace bounds the save at shutdown.
	saveGrace = 5 * time.Second
)

// Authorizer is the authorization engine's in-memory check.
type Authorizer interface {
	AllowedAll(subjects []string, domain string, perm authz.Permission) bool
	HoldsRole(subjects []string, domain string, r authz.Role) bool
}

// Policy is the Telegram settings the notifier reads from memory.
type Policy interface {
	Current() settings.Settings
	Space(spaceID string) settings.Space
}

// Options configure a Notifier. Feed, Reader, Authz, Policy, Links and Sender are required.
type Options struct {
	Feed   Feed
	Reader Reader
	Authz  Authorizer
	Policy Policy
	Links  *Links
	// Sender returns the live outbound sender, or nil while the bot is off (nothing is sent then).
	Sender func() *sender.Sender
	// Store records card mappings (nil: cards are not remembered across restarts).
	Store Mappings
	// AppURL is the Mini App link offered on a card whose body was cut at the message cap (empty: no button).
	AppURL string
	// ItemURL returns the Mini App URL of one resource, for a card's "Open in app" button and its cut body ("" or nil:
	// AppURL). It is per card because the link names the card's own item.
	ItemURL func(spaceID string, k Key) string
	// Topics routes messages to each link's space topics when its chat layout is on (nil: everything goes to the main
	// chat).
	Topics *topics.Manager
	// Pending reads the number of pending human actions per space, once per rebuild of the home summaries (nil: no
	// summaries). Each link's summary keeps only the spaces it may read.
	Pending func(ctx context.Context) ([]SpaceCount, error)
	// Perms loads a space's human-action type permissions, once per space per rebuild of the summaries (nil: the
	// defaults, every type visible to a viewer). A space it fails for is left out of the summaries.
	Perms func(ctx context.Context, spaceID string) (human.PermissionSet, error)
	// NeedsURL is the Mini App's cross-space Needs-you page, the summary's web_app button; without it NeedsLink (the
	// t.me/<bot>?startapp=needs link) is a plain URL button; with neither there is no button. Both are read when a summary
	// is sent (the Mini App host can change while running).
	NeedsURL, NeedsLink func() string
	// SpaceLink is the Mini App link of a space (t.me/<bot>?startapp=space-<id>) for a summary line whose space has no
	// topic (the layout is off); BotUsername builds the topic links. Nil or "": no link.
	SpaceLink   func(spaceID string) string
	BotUsername func() string
	// HomeEvery is the least time between two edits of a link's summary (default DefaultHomeEvery); HomeAfter waits
	// for it (default time.After; tests pass one they control).
	HomeEvery time.Duration
	HomeAfter func(time.Duration) <-chan time.Time
	Bus       *eventbus.Bus
	Log       *slog.Logger
	// Now is the clock (default time.Now); After waits (default time.After; tests pass one they control).
	Now   func() time.Time
	After func(time.Duration) <-chan time.Time
	// Cursors persists the change-feed position (nil: every start begins at the newest change). MaxReplay is how old a
	// saved position may be to resume from it (default DefaultMaxReplay).
	Cursors   Cursors
	MaxReplay time.Duration
	// FlushEvery and FlushRows bound the mapping writes (default 1 s and 100 rows).
	FlushEvery time.Duration
	FlushRows  int
	// RetryAfter is how long after a failed (partial) read of the change feed the rest is read again without waiting
	// for a new change (default 1 s).
	RetryAfter time.Duration
}

// Notifier is the single change-feed consumer.
type Notifier struct {
	o       Options
	log     *slog.Logger
	cards   *cards
	digest  *digestBuf
	notices *notices
	home    *home

	cursor   int64 // Prime sets it before Run starts; after that only the Run goroutine touches it
	primed   bool  // Prime read the start position, so Run starts from cursor instead of reading Latest
	started  bool  // the start position is known (Run goroutine, after Prime)
	savedSeq int64 // the position last written to Cursors, and when
	savedAt  time.Time

	sweepNow  chan struct{} // the link list was reloaded: sweep (see kickSweep)
	ready     chan struct{} // closed once Run has subscribed and read its start position
	readyOnce sync.Once
}

// New returns a Notifier; Run starts it.
func New(o Options) *Notifier {
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.After == nil {
		o.After = time.After
	}
	if o.FlushEvery <= 0 {
		o.FlushEvery = time.Second
	}
	if o.RetryAfter <= 0 {
		o.RetryAfter = time.Second
	}
	if o.HomeAfter == nil {
		o.HomeAfter = time.After
	}
	if o.MaxReplay <= 0 {
		o.MaxReplay = DefaultMaxReplay
	}
	if o.FlushRows <= 0 {
		o.FlushRows = 100
	}
	return &Notifier{o: o, log: o.Log, cards: newCards(o.Store, o.FlushRows, o.Now), digest: newDigestBuf(),
		notices: &notices{sent: map[string]bool{}}, home: newHome(), sweepNow: make(chan struct{}, 1), ready: make(chan struct{})}
}

// Ready is closed once Run is subscribed and has its start position. A change made before that is history that Run
// doesn't replay, so a caller that produces a change and expects a card (a test, say) waits for it first.
func (n *Notifier) Ready() <-chan struct{} { return n.ready }

// Prime reads the start position, so a change made after it is delivered once Run starts. Central calls it before
// anything that can change data (the HTTP listener, the recover passes, the deadline loop), and starts Run later. A
// failed Prime leaves Run to read the start position itself, as before. It must finish before Run starts.
func (n *Notifier) Prime(ctx context.Context) error {
	pos, err := n.startPosition(ctx)
	if err != nil {
		return err
	}
	n.cursor, n.primed = pos, true
	return nil
}

// startPosition is where the feed is followed from: the saved position when it is recent enough (changes made while
// central was down are delivered), the newest change otherwise. A saved position beyond the newest change comes from a
// restore from backup, and is dropped.
func (n *Notifier) startPosition(ctx context.Context) (int64, error) {
	latest, err := n.o.Feed.Latest(ctx)
	if err != nil {
		return 0, err
	}
	if n.o.Cursors == nil {
		return latest, nil
	}
	seq, at, ok, err := n.o.Cursors.Load(ctx)
	if err != nil {
		n.log.Warn("telegram notifier: reading the saved position failed", slog.String("error", err.Error()))
		return latest, nil
	}
	if age := n.o.Now().Sub(at); !ok || seq > latest || age < 0 || age > n.o.MaxReplay {
		return latest, nil
	}
	n.savedSeq, n.savedAt = seq, at
	return seq, nil
}

// saveCursor writes the position when it moved, or when the last write is old (force: always).
func (n *Notifier) saveCursor(ctx context.Context, force bool) {
	if n.o.Cursors == nil || !n.started {
		return
	}
	now := n.o.Now()
	if !force && n.cursor == n.savedSeq && now.Sub(n.savedAt) < cursorEvery {
		return
	}
	if err := n.o.Cursors.Save(ctx, n.cursor, now); err != nil {
		n.log.Warn("telegram notifier: saving the position failed", slog.String("error", err.Error()))
		return
	}
	n.savedSeq, n.savedAt = n.cursor, now
}

// Run follows the change feed until ctx ends. It subscribes before it reads the start position, so a change made in
// between is seen; a failed start read is retried by the next wake-up of the loop's safety timer.
func (n *Notifier) Run(ctx context.Context) error {
	sub := n.o.Bus.Subscribe(16, eventbus.TopicChange)
	defer sub.Close()
	// What decides who may see a card changes without a change-feed entry: grants, links and their mutes, the spaces'
	// Telegram settings. The events only say "look again": each sweeps the cards in memory (see sweep).
	access := n.o.Bus.Subscribe(16, eventbus.TopicGrantsChanged, eventbus.TopicTelegramLink, eventbus.TopicTelegramSettings, eventbus.TopicUserChanged)
	defer access.Close()
	n.o.Links.afterReload = n.kickSweep
	n.o.Links.Start(ctx)
	defer n.flush(context.WithoutCancel(ctx)) //nolint:contextcheck // the last mappings are written after shutdown
	// The position is saved last: what wasn't processed before ctx ended (a window still being collected, queued
	// sends the sender drops at shutdown) is after it, and the next start delivers it within the replay bound.
	defer func() {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), saveGrace)
		defer cancel()
		n.saveCursor(sctx, true)
	}()

	retry := time.NewTicker(10 * time.Second)
	defer retry.Stop()
	start := func() {
		pos, err := n.startPosition(ctx)
		if err != nil {
			n.log.Warn("telegram notifier: reading the newest change failed", slog.String("error", err.Error()))
			return
		}
		n.cursor, n.started = pos, true
		n.readyOnce.Do(func() { close(n.ready) })
	}
	if n.primed {
		n.started = true
		n.readyOnce.Do(func() { close(n.ready) }) // subscribed above, and the start position is Prime's
	} else {
		start()
	}
	n.loadNotices(ctx)
	n.loadHome(ctx)
	flush := time.NewTicker(n.o.FlushEvery)
	defer flush.Stop()
	var homeWake <-chan time.Time // armed while a summary waits for its next edit
	armHome := func() {
		if d := n.homeTick(ctx); d > 0 {
			homeWake = n.o.HomeAfter(d)
		}
	}
	var late <-chan time.Time  // armed after an access event: the caches it refreshes may be read a moment later than it
	var again <-chan time.Time // armed once after a failed feed read: the unread rest is processed without a new change
	if n.primed {              // what changed between Prime and the subscription has no wake-up of its own
		if n.process(ctx) {
			again = time.After(n.o.RetryAfter)
		}
		armHome()
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-flush.C:
			n.flush(ctx)
			n.saveCursor(ctx, false)
		case <-n.cards.full:
			n.flush(ctx)
		case <-retry.C:
			if !n.started {
				start()
			}
			n.loadNotices(ctx)
			n.loadHome(ctx)
			armHome()
			n.sweep() // the backstop for an event that was dropped, or seen before the caches it concerns were reloaded
		case <-access.C:
			n.sweep()
			late = time.After(Window)
		case <-n.sweepNow:
			n.sweep()
		case <-late:
			late = nil
			n.sweep()
		case <-homeWake:
			homeWake = nil
			armHome()
		case <-again:
			again = nil
			if n.process(ctx) {
				again = time.After(n.o.RetryAfter)
			}
			armHome()
		case _, ok := <-sub.C:
			if !ok {
				return nil
			}
			if !n.started {
				start()
				continue // history before the start position isn't replayed
			}
			select { // collect for one window; wake-ups during it are the same window
			case <-ctx.Done():
				return ctx.Err()
			case <-n.o.After(Window):
			}
			n.drainWake(sub.C)
			if n.process(ctx) {
				again = time.After(n.o.RetryAfter)
			}
			armHome()
		}
	}
}

func (n *Notifier) drainWake(c <-chan eventbus.Event) {
	for {
		select {
		case <-c:
		default:
			return
		}
	}
}

func (n *Notifier) flush(ctx context.Context) {
	if err := n.cards.flush(ctx); err != nil {
		n.log.Warn("telegram notifier: writing card mappings failed", slog.String("error", err.Error()))
	}
}

// window is what one window collected.
type window struct {
	keys     []Key
	seen     map[Key]changefeed.Change // the newest change of each distinct key
	spaces   map[string]bool           // spaces with relevant changes, for the overflow notice
	overflow bool
}

// collect reads every change after the cursor, in batches, into a bounded window.
func (n *Notifier) collect(ctx context.Context) (*window, error) {
	w := &window{seen: map[Key]changefeed.Change{}, spaces: map[string]bool{}}
	// A cursor beyond the newest change comes from a restore from backup: the feed was rewound.
	if latest, err := n.o.Feed.Latest(ctx); err == nil && n.cursor > latest {
		n.cursor = latest
	}
	for {
		rows, err := n.o.Feed.After(ctx, n.cursor, BatchSize)
		if err != nil {
			return w, err
		}
		for _, c := range rows {
			n.cursor = c.Seq
			if !relevant[c.ResourceType] {
				continue
			}
			k := Key{c.ResourceType, c.ResourceID}
			if _, ok := w.seen[k]; !ok {
				if len(w.keys) >= MaxKeys {
					w.overflow = true
					if c.SpaceID != "" && len(w.spaces) < maxSpaces {
						w.spaces[c.SpaceID] = true
					}
					continue
				}
				w.keys = append(w.keys, k)
			}
			w.seen[k] = c
			if c.SpaceID != "" && len(w.spaces) < maxSpaces {
				w.spaces[c.SpaceID] = true
			}
		}
		if len(rows) < BatchSize {
			return w, nil
		}
	}
}

// process handles one window. It reports whether reading the feed failed part-way (the rest is still unread).
func (n *Notifier) process(ctx context.Context) (retry bool) {
	w, err := n.collect(ctx)
	if err != nil {
		n.log.Warn("telegram notifier: reading changes failed", slog.String("error", err.Error()))
		retry = true
	}
	n.handle(ctx, w)
	return retry
}

func (n *Notifier) handle(ctx context.Context, w *window) {
	if len(w.keys) == 0 && !w.overflow {
		return
	}
	snd := n.o.Sender()
	if snd == nil {
		return // the bot is off: nothing is sent, and nothing is replayed when it comes up
	}
	links := n.o.Links.Snapshot()
	if len(links) == 0 {
		return
	}
	now := n.o.Now()
	scope := map[string]any{}
	if w.overflow {
		// Above the cap the details are skipped: one "many changes" notice per chat (plan 4.2 step 3).
		n.overflow(snd, links, w.spaces, now)
		n.homeTouch()
		return
	}
	for _, k := range w.keys {
		if k.Type == TypeHumanAction {
			n.homeTouch()
		}
		r, err := n.o.Reader.Read(ctx, scope, k)
		if err != nil {
			n.log.Warn("telegram notifier: reading a resource failed", slog.String("type", k.Type), slog.String("error", err.Error()))
			continue
		}
		if r == nil {
			continue
		}
		n.deliver(ctx, snd, links, r, now, w.seen[k].Action == changefeed.Created)
	}
}

// kickSweep asks Run for a sweep, after the link list was reloaded (a mute, a link's grants or its user changed).
func (n *Notifier) kickSweep() {
	select {
	case n.sweepNow <- struct{}{}:
	default:
	}
}

// sweep retracts the cards in memory whose link may no longer see the item: its access was revoked, the space muted
// or hidden, the link overdue for re-confirmation. It is the eager counterpart of the check deliver and a queued send
// make, and uses the same ones (may, Visible) on what the card was sent with, so it can only take content away: a
// card is never edited to show more, and nothing is read. A retraction is queued like any other (it is checked
// against the current link when it is sent, and a card already retracted costs nothing).
func (n *Notifier) sweep() {
	snd := n.o.Sender()
	if snd == nil {
		return
	}
	cs := n.cards.sweepable()
	if len(cs) == 0 {
		return
	}
	links := n.o.Links.Snapshot()
	byID := make(map[string]*Link, len(links))
	for i := range links {
		byID[links[i].ID] = &links[i]
	}
	set, now := n.o.Policy.Current(), n.o.Now()
	for i := range cs {
		l := byID[cs[i].link]
		if l == nil || l.ChatID != cs[i].chat {
			continue // unlinked or moved: the bot no longer writes to that chat for this link
		}
		if !n.o.Policy.Space(cs[i].r.SpaceID).Visible() || !n.may(l, &cs[i].r, set, now) {
			n.retract(snd, l, &cs[i].r)
		}
	}
}

// deliver enqueues the resource's card for every link that may see it, and the reminders, escalations and
// assignments of a card that exists as messages of their own.
func (n *Notifier) deliver(ctx context.Context, snd *sender.Sender, links []Link, r *Resource, now time.Time, created bool) {
	set := n.o.Policy.Current()
	sp := n.o.Policy.Space(r.SpaceID)
	loaded := false
	load := func() {
		if !created && !loaded {
			n.cards.load(ctx, r.Key) // a card may exist from before a restart
			loaded = true
		}
	}
	if !sp.Visible() {
		// A hidden space shows nothing: a card that exists says only that it is gone.
		for i := range links {
			load()
			n.retract(snd, &links[i], r)
		}
		return
	}
	if r.Final || !r.Notify {
		n.digest.drop(r.Key) // it no longer needs anybody's attention
	}
	for i := range links {
		l := &links[i]
		if !n.may(l, r, set, now) {
			// Access lost, the space muted, or the link overdue for re-confirmation: the card stops showing the item.
			load()
			n.retract(snd, l, r)
			continue
		}
		load()
		p := l.Prefs.Effective(set.NotificationDefaults)
		card, queued := n.cards.state(l.ChatID, r.Key)
		if card == nil {
			// A card that is still waiting in the sender is replaced by the newest state (its final one too); with no
			// card at all, only a state worth a notification, that the link wants, sends one, or collects into its digest.
			if queued {
				// The card's first send is on its way (maybe already at Telegram, its mapping not yet recorded): what
				// happened since it was queued is still announced, against the baseline taken when it was queued.
				if r.Kind == KindAction && !r.Final {
					n.announce(snd, l, r, p)
				}
			} else {
				if r.Final || !r.Notify {
					continue
				}
				switch delivery(p, r, l.UserID) {
				case settings.DeliveryDigest:
					if l.Zone != nil && p.Digest { // a digest that can never be sent collects nothing
						n.digest.add(l.ID, r, digestSection(r), now)
					}
					continue
				case settings.DeliveryPush:
				default:
					continue
				}
			}
		} else if r.Kind == KindAction && !r.Final {
			n.announce(snd, l, r, p)
		}
		if card != nil && !card.gone && card.version >= r.Version && r.Version != 0 &&
			(!r.Final || len(card.conts) == 0 && card.alert == 0) {
			// The card already shows this state. Not a settled one that still has continuations or an alert: the chat
			// handler's own edit settled the message (EditCard), but the rest of the card is the upsert's to clean up.
			continue
		}
		n.enqueue(snd, l, r, card != nil)
	}
}

// announce sends what happened to a pending action whose card exists: an escalation, an assignment to the link's user,
// or a reminder, as a message of its own (an edit makes no sound). The link's preferences decide: escalations and
// "assigned to me" are flags, reminders are push, digest or off.
func (n *Notifier) announce(snd *sender.Sender, l *Link, r *Resource, p settings.EffectivePrefs) {
	reminded, escalated, reassigned := n.cards.observe(l.ChatID, r)
	mine := r.Assignee == l.UserID
	switch {
	case escalated && p.Escalations:
		n.enqueueAlert(snd, l, r, "Escalated")
	case reassigned && mine && p.AssignedToMe:
		n.enqueueAlert(snd, l, r, "Assigned to you")
	case reminded && p.Reminders == settings.DeliveryPush:
		n.enqueueAlert(snd, l, r, "Reminder")
	case reminded && p.Reminders == settings.DeliveryDigest:
		if l.Zone != nil && p.Digest {
			n.digest.add(l.ID, r, secReminders, n.o.Now())
		}
	}
}

// enqueueAlert sends a short message about a pending action. Like a card it is checked and rendered at send time, and it
// carries the title only: the body is the Mini App's.
func (n *Notifier) enqueueAlert(snd *sender.Sender, l *Link, r *Resource, what string) {
	chat, linkID, res := l.ChatID, l.ID, *r
	err := snd.Enqueue(sender.Message{ChatID: chat, Priority: sender.NeedsYou, Key: "alert:" + what + ":" + r.String(),
		Send: func(ctx context.Context, c *botapi.Client) error {
			cur := n.currentLink(linkID, chat)
			if cur == nil || !n.stillMay(linkID, chat, &res) {
				return nil
			}
			head := what + ": " + res.Label
			text := head + "\n" + clip(oneLine(res.Title), maxTitle)
			p := botapi.SendMessageParams{ChatID: chat, Text: text, DisableNotification: n.silent(cur, n.o.Now()),
				Entities: []botapi.MessageEntity{{Type: "bold", Offset: 0, Length: len(utf16.Encode([]rune(head)))}}, ProtectContent: true}
			m, err := n.sendAlert(ctx, c, linkID, chat, &res, p)
			if err == nil {
				n.cards.touch(linkID, chat, &res, n.o.Now()) // the mapping's time carries the announcement across a restart
				if old := n.cards.swapAlert(chat, res.Key, m.MessageID); old != 0 && old != m.MessageID && sender.Take(ctx) == nil {
					_ = c.DeleteMessage(ctx, chat, old) // the alert before it (best effort: within 48 h)
				}
			}
			return err
		}})
	if err != nil {
		n.log.Debug("telegram notifier: alert refused", slog.String("error", err.Error()))
	}
}

// may is the recipient check, all in memory. It never allows more than the API would for the link's principal: the
// link and its user's subjects must both hold the space's read permission and the resource type's, and the human
// type's view role.
func (n *Notifier) may(l *Link, r *Resource, set settings.Settings, now time.Time) bool {
	if !l.Usable(now) {
		return false
	}
	subj := l.Subjects(set.LinkSSOGrants, now)
	if !n.o.Authz.AllowedAll(subj, r.SpaceID, authz.SpaceRead) || !n.o.Authz.AllowedAll(subj, r.SpaceID, r.Perm) {
		return false
	}
	if r.ViewRole != "" && r.ViewRole != "viewer" && !n.o.Authz.HoldsRole(subj, r.SpaceID, authz.Role(r.ViewRole)) {
		return false
	}
	return !l.Prefs.IsMuted(r.SpaceID)
}

var priorityRank = map[string]int{"low": 0, "normal": 1, "high": 2, "urgent": 3}

// delivery applies the preferences to a new notification: push, digest or off. An action assigned to the link's user
// always counts when "assigned to me" is on, whatever its priority; other new actions need "new actions" and the
// minimum priority.
func delivery(p settings.EffectivePrefs, r *Resource, userID string) string {
	push := func(ok bool) string {
		if ok {
			return settings.DeliveryPush
		}
		return settings.DeliveryOff
	}
	switch r.Kind {
	case KindAction:
		if r.Assignee != "" && r.Assignee == userID && p.AssignedToMe {
			return settings.DeliveryPush
		}
		return push(p.NewActions && priorityRank[r.Priority] >= priorityRank[p.MinPriority])
	case KindInbox:
		return p.InboxItems
	case KindTaskReview:
		return p.TasksInReview
	case KindFailedRun:
		return p.FailedRuns
	case KindMessage:
		return push(p.OperatorMessages)
	}
	return settings.DeliveryOff
}

// digestSection is where a resource that goes to the digest is listed.
func digestSection(r *Resource) int {
	switch r.Kind {
	case KindTaskReview:
		return secTasks
	case KindFailedRun:
		return secRuns
	}
	return secInbox
}

func (n *Notifier) enqueue(snd *sender.Sender, l *Link, r *Resource, edit bool) {
	prio := sender.Notification
	switch {
	case edit:
		prio = sender.Edit
	case r.Kind == KindAction:
		prio = sender.NeedsYou
	}
	chat, linkID, res := l.ChatID, l.ID, *r
	var done func(error)
	if !edit {
		done = n.cards.queue(chat, r)
	} else {
		n.cards.want(chat, r.Key, r.Version) // from now on the chat's edits of an older version lose
	}
	err := snd.Enqueue(sender.Message{ChatID: chat, Priority: prio, Key: "card:" + r.String(), Done: done,
		Send: func(ctx context.Context, c *botapi.Client) error {
			// The send may run long after it was queued (behind a chat's rate limit): access is checked again, from
			// memory, so a revoked link, grant, mute or hidden space never gets a card that was queued before.
			cur := n.currentLink(linkID, chat)
			if cur == nil {
				return nil
			}
			if !n.stillMay(linkID, chat, &res) {
				return n.cards.retract(ctx, c, chat, res.Key) // a card that exists stops showing the item; none is sent
			}
			// The card is rendered here, from the space's current content level: a body queued at "full" is never sent
			// after the space was lowered to "summary" while it waited. A group chat only ever gets the summary.
			v := n.view(cur, chat, &res)
			parts := card.Render(v, n.caps(cur, chat, &res))
			protect := v.Full && res.Detail != ""                                                                                            // plan 3.8: a card with a body can't be forwarded or saved
			return n.cards.upsert(ctx, c, n.sender(&res, linkID), linkID, chat, &res, res.SpaceID, parts, protect, n.silent(cur, n.o.Now())) // quiet hours: no sound
		}})
	if err != nil {
		if done != nil {
			done(err)
		}
		n.log.Debug("telegram notifier: enqueue refused", slog.String("error", err.Error()))
	}
}

// sender is the send function for a resource's messages to a link: its card and its alerts go to the space's topic when
// the link's chat layout is a topic per space, and to the main chat otherwise.
func (n *Notifier) sender(r *Resource, linkID string) sendFunc {
	return n.route(linkID, r.SpaceID)
}

// route is the send function for a link's messages about a space (spaceID "": home, for what concerns no single
// space). The topic is chosen when the message is sent, never when it was queued: a layout the link changed while the
// message waited applies (ADR-0089). The first message of a link that lands in a topic queues the link's one-time
// announcement of the new layout.
func (n *Notifier) route(linkID, spaceID string) sendFunc {
	return func(ctx context.Context, c *botapi.Client, p botapi.SendMessageParams) (botapi.Message, error) {
		key := n.o.Topics.Route(linkID, spaceID)
		if key == "" {
			return c.SendMessage(ctx, p)
		}
		msg, err := n.o.Topics.Send(ctx, c, linkID, p.ChatID, key, p)
		if err == nil && msg.MessageThreadID != 0 {
			n.announceThreads(linkID, p.ChatID)
		}
		return msg, err
	}
}

// stillMay re-runs the recipient check at send time against the current links, settings and space visibility. A link
// that is gone, moved to another chat, or no longer allowed drops the send.
func (n *Notifier) stillMay(linkID string, chat int64, r *Resource) bool {
	if !n.o.Policy.Space(r.SpaceID).Visible() {
		return false
	}
	l := n.currentLink(linkID, chat)
	return l != nil && n.may(l, r, n.o.Policy.Current(), n.o.Now())
}

// MayRead reports whether the link may see the space as it is now, all in memory: the link is current (not revoked,
// not moved to another chat) and usable, its subjects hold the space's read permission, and the space is visible to
// Telegram. It is the check a topic rename makes before it writes a space's name into a chat (no database read).
func (n *Notifier) MayRead(linkID string, chat int64, spaceID string) bool {
	if !n.o.Policy.Space(spaceID).Visible() {
		return false
	}
	l := n.currentLink(linkID, chat)
	if l == nil {
		return false
	}
	now := n.o.Now()
	return l.Usable(now) && n.o.Authz.AllowedAll(l.Subjects(n.o.Policy.Current().LinkSSOGrants, now), spaceID, authz.SpaceRead)
}

// Muted reports whether the link muted a space, from memory (false for a link that is gone).
func (n *Notifier) Muted(linkID, spaceID string) bool {
	links := n.o.Links.Snapshot()
	for i := range links {
		if links[i].ID == linkID {
			return links[i].Prefs.IsMuted(spaceID)
		}
	}
	return false
}

// currentLink is the link as it is now (nil: gone, or moved to another chat).
func (n *Notifier) currentLink(linkID string, chat int64) *Link {
	links := n.o.Links.Snapshot()
	for i := range links {
		if links[i].ID == linkID {
			if links[i].ChatID != chat {
				return nil
			}
			return &links[i]
		}
	}
	return nil
}

// overflow sends one "many changes" message per chat that may read one of the window's spaces.
func (n *Notifier) overflow(snd *sender.Sender, links []Link, spaces map[string]bool, now time.Time) {
	set := n.o.Policy.Current()
	for i := range links {
		l := &links[i]
		if !l.Usable(now) {
			continue
		}
		subj := l.Subjects(set.LinkSSOGrants, now)
		for sp := range spaces {
			if !n.o.Policy.Space(sp).Visible() || l.Prefs.IsMuted(sp) || !n.o.Authz.AllowedAll(subj, sp, authz.SpaceRead) {
				continue
			}
			chat, send := l.ChatID, n.route(l.ID, "") // home: the notice is about several spaces
			_ = snd.Enqueue(sender.Message{ChatID: chat, Priority: sender.Notification, Key: "many",
				Send: func(ctx context.Context, c *botapi.Client) error {
					_, err := send(ctx, c, botapi.SendMessageParams{ChatID: chat, Text: "Many changes at once — open the app to see them."})
					return err
				}})
			break // one notice per chat, whatever the number of spaces
		}
	}
}
