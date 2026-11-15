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
	"time"
	"unicode/utf16"

	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/eventbus"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/sender"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/settings"
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
	Bus   *eventbus.Bus
	Log   *slog.Logger
	// Now is the clock (default time.Now); After waits (default time.After; tests pass one they control).
	Now   func() time.Time
	After func(time.Duration) <-chan time.Time
	// FlushEvery and FlushRows bound the mapping writes (default 1 s and 100 rows).
	FlushEvery time.Duration
	FlushRows  int
	// RetryAfter is how long after a failed (partial) read of the change feed the rest is read again without waiting
	// for a new change (default 1 s).
	RetryAfter time.Duration
}

// Notifier is the single change-feed consumer.
type Notifier struct {
	o      Options
	log    *slog.Logger
	cards  *cards
	digest *digestBuf

	cursor int64 // only the Run goroutine touches it
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
	if o.FlushRows <= 0 {
		o.FlushRows = 100
	}
	return &Notifier{o: o, log: o.Log, cards: newCards(o.Store, o.FlushRows, o.Now), digest: newDigestBuf()}
}

// Run follows the change feed until ctx ends. It subscribes before it reads the start position, so a change made in
// between is seen; a failed start read is retried by the next wake-up of the loop's safety timer.
func (n *Notifier) Run(ctx context.Context) error {
	sub := n.o.Bus.Subscribe(16, eventbus.TopicChange)
	defer sub.Close()
	n.o.Links.Start(ctx)
	defer n.flush(context.WithoutCancel(ctx)) //nolint:contextcheck // the last mappings are written after shutdown

	var started bool
	retry := time.NewTicker(10 * time.Second)
	defer retry.Stop()
	start := func() {
		latest, err := n.o.Feed.Latest(ctx)
		if err != nil {
			n.log.Warn("telegram notifier: reading the newest change failed", slog.String("error", err.Error()))
			return
		}
		n.cursor, started = latest, true
	}
	start()
	flush := time.NewTicker(n.o.FlushEvery)
	defer flush.Stop()
	var again <-chan time.Time // armed once after a failed feed read: the unread rest is processed without a new change
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-flush.C:
			n.flush(ctx)
		case <-n.cards.full:
			n.flush(ctx)
		case <-retry.C:
			if !started {
				start()
			}
		case <-again:
			again = nil
			if n.process(ctx) {
				again = time.After(n.o.RetryAfter)
			}
		case _, ok := <-sub.C:
			if !ok {
				return nil
			}
			if !started {
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
		return
	}
	for _, k := range w.keys {
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

// deliver enqueues the resource's card for every link that may see it, and the reminders, escalations and
// assignments of a card that exists as messages of their own.
func (n *Notifier) deliver(ctx context.Context, snd *sender.Sender, links []Link, r *Resource, now time.Time, created bool) {
	set := n.o.Policy.Current()
	sp := n.o.Policy.Space(r.SpaceID)
	if !sp.Visible() {
		return
	}
	if r.Final || !r.Notify {
		n.digest.drop(r.Key) // it no longer needs anybody's attention
	}
	loaded := false
	for i := range links {
		l := &links[i]
		if !n.may(l, r, set, now) {
			continue
		}
		if !created && !loaded {
			// A card may exist from before a restart, but only for a link that may see the resource.
			n.cards.load(ctx, r.Key)
			loaded = true
		}
		p := l.Prefs.Effective(set.NotificationDefaults)
		card := n.cards.get(l.ChatID, r.Key)
		if card == nil {
			// A card that is still waiting in the sender is replaced by the newest state (its final one too); with no
			// card at all, only a state worth a notification, that the link wants, sends one, or collects into its digest.
			if !n.cards.sendQueued(l.ChatID, r.Key) {
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
		if card != nil && card.version >= r.Version && r.Version != 0 {
			continue // the card already shows this state
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
			_, err := c.SendMessage(ctx, botapi.SendMessageParams{ChatID: chat, Text: text, DisableNotification: n.silent(cur, n.o.Now()),
				Entities: []botapi.MessageEntity{{Type: "bold", Offset: 0, Length: len(utf16.Encode([]rune(head)))}}, ProtectContent: true})
			if err == nil {
				n.cards.touch(linkID, chat, &res, n.o.Now()) // the mapping's time carries the announcement across a restart
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
		done = n.cards.queue(chat, r.Key)
	}
	err := snd.Enqueue(sender.Message{ChatID: chat, Priority: prio, Key: "card:" + r.String(), Done: done,
		Send: func(ctx context.Context, c *botapi.Client) error {
			// The send may run long after it was queued (behind a chat's rate limit): access is checked again, from
			// memory, so a revoked link, grant, mute or hidden space never gets a card that was queued before.
			cur := n.currentLink(linkID, chat)
			if cur == nil || !n.stillMay(linkID, chat, &res) {
				return nil
			}
			// The card is rendered here, from the space's current content level: a body queued at "full" is never sent
			// after the space was lowered to "summary" while it waited.
			full := n.o.Policy.Space(res.SpaceID).ContentLevel == settings.ContentFull
			text, entities := renderCard(&res, full)
			protect := full && res.Detail != ""                                                                               // plan 3.8: a card with a body can't be forwarded or saved
			return n.cards.upsert(ctx, c, linkID, chat, &res, res.SpaceID, text, entities, protect, n.silent(cur, n.o.Now())) // quiet hours: no sound
		}})
	if err != nil {
		if done != nil {
			done(err)
		}
		n.log.Debug("telegram notifier: enqueue refused", slog.String("error", err.Error()))
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
			chat := l.ChatID
			_ = snd.Enqueue(sender.Message{ChatID: chat, Priority: sender.Notification, Key: "many",
				Send: func(ctx context.Context, c *botapi.Client) error {
					_, err := c.SendMessage(ctx, botapi.SendMessageParams{ChatID: chat, Text: "Many changes at once — open the app to see them."})
					return err
				}})
			break // one notice per chat, whatever the number of spaces
		}
	}
}
