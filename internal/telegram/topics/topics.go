// Package topics is the thread mode of the private chat (ADR-0089, which amends ADR-0083 decision 16): when a link's
// chat layout is on, every message about a space goes to a topic of its own for that space in the link's private chat,
// and what concerns no single space (the digest, overflow notices) goes to home, the chat without a topic. The layout is
// decided per link (notification_defaults.threads with the link's override: auto, on or off; auto follows the bot's
// getMe has_topics_enabled). A topic is created by the bot on first use and its thread ID is remembered per link in
// telegram_links.topics_json. Messages are addressed to it with message_thread_id.
//
// A topic that was deleted or closed (the send fails with "message thread not found" or TOPIC_CLOSED) is forgotten and
// created again once; if that fails too, or the chat does not have topics enabled for the bot, the message goes to the
// main chat, so a notification is never lost to a topic. A chat that refuses topics is not asked again for a while.
// Authorization is unchanged: topics only choose where a message appears, never whether a link may receive it.
//
// The shared "Needs you" topic of the per-space topics mode (TG-11) is retired: nothing is routed to it, and the first
// time a link's topics are used after the upgrade the bot posts one note in it and drops it from the link's topics. The
// topic itself is never deleted, because deleting a topic deletes its messages (ADR-0089 decision 3).
package topics

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/settings"
)

// Home is the key Route returns for a message that concerns no single space. "" is the chat without a topic
// (ADR-0089 decision 2). Setting it to HomeKey makes home a bot-created topic named HomeName instead; nothing else
// changes.
const Home = ""

// HomeKey is the key of a bot-created home topic, used only when Home is set to it.
const HomeKey = "home"

// HomeName is the home topic's title, when home is a topic.
const HomeName = "🏠 Home"

// LegacyNeeds is the key of the retired shared "Needs you" topic. Nothing is routed to it; a link's stored entry is
// retired on first use (see the package comment).
const LegacyNeeds = "needs"

// LegacyNote is the note posted in the retired "Needs you" topic.
const LegacyNote = "Needs-you items now arrive in each space's own topic. You can delete this topic."

// RefusedFor is how long a chat that refused to create a topic is left alone.
const RefusedFor = 10 * time.Minute

// DefaultCreatesPerSecond bounds topic creation (and the legacy notes) over every chat. Creating a topic is a Bot API
// call the sender's global bucket doesn't count (it runs inside one queued send), so when many links switch at once the
// calls stay near Telegram's limit: the sender's 25/s plus these.
const DefaultCreatesPerSecond = 5

// Store keeps a link's thread IDs (key → message_thread_id).
type Store interface {
	Load(ctx context.Context, linkID string) (map[string]int, error)
	Save(ctx context.Context, linkID string, threads map[string]int) error
}

// Options configure a Manager.
type Options struct {
	Store Store
	// Mode is a link's effective chat layout as it is now (settings.ThreadsAuto, ThreadsOn or ThreadsOff; "" is auto).
	// It is read at send time, from memory. nil: off for every link.
	Mode func(linkID string) string
	// BotTopics reports the bot's getMe has_topics_enabled (nil: false), which decides auto.
	BotTopics func() bool
	// SpaceName is a space's title for its topic (default: the ID).
	SpaceName func(ctx context.Context, spaceID string) string
	// CreatesPerSecond bounds topic creation over every chat (default DefaultCreatesPerSecond).
	CreatesPerSecond float64
	Now              func() time.Time
	Log              *slog.Logger
}

// Manager routes messages to topics.
type Manager struct {
	o       Options
	creates *rate.Limiter

	mu      sync.Mutex // guards links and refused only; never held across I/O
	links   map[string]*linkState
	refused map[int64]time.Time // chat → when to ask again
}

// linkState is one link's thread IDs. mu guards the map and is held only for memory and the store, never across a
// Telegram call, so a lookup (SpaceOf, a send to a topic that exists) never waits for a topic being created. create
// serializes the Telegram calls that change the map (a topic create, the legacy note): two sends of one link must not
// both create a topic; other links don't wait.
type linkState struct {
	create sync.Mutex
	mu     sync.Mutex
	// threads is key → thread; loaded once from the store.
	threads map[string]int
	loaded  bool
}

// New returns a Manager.
func New(o Options) *Manager {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.CreatesPerSecond <= 0 {
		o.CreatesPerSecond = DefaultCreatesPerSecond
	}
	return &Manager{o: o, creates: rate.NewLimiter(rate.Limit(o.CreatesPerSecond), max(1, int(o.CreatesPerSecond))),
		links: map[string]*linkState{}, refused: map[int64]time.Time{}}
}

// modeOn reports whether a chat layout is a topic per space: on, or auto while the bot has topics enabled.
func (m *Manager) modeOn(mode string) bool {
	if m == nil {
		return false
	}
	switch mode {
	case settings.ThreadsOn:
		return true
	case settings.ThreadsAuto, "":
		return m.o.BotTopics != nil && m.o.BotTopics()
	}
	return false
}

// on reports whether a link's effective chat layout (Options.Mode) is a topic per space.
func (m *Manager) on(linkID string) bool {
	if m == nil || m.o.Mode == nil {
		return false
	}
	return m.modeOn(m.o.Mode(linkID))
}

// Enabled reports whether messages to the link's private chat go to topics now: the link's layout is on (see on) and
// the chat hasn't refused a topic recently. A group chat is never in thread mode here (groups route on their own).
func (m *Manager) Enabled(linkID string, chatID int64) bool {
	return chatID > 0 && m.on(linkID) && !m.isRefused(chatID)
}

// EnabledFor is Enabled for a layout the caller already knows (a link's preferences just saved, which the notifier's
// own view only learns of once the change commits): mode is settings.ThreadsAuto, ThreadsOn or ThreadsOff.
func (m *Manager) EnabledFor(mode string, chatID int64) bool {
	return chatID > 0 && m.modeOn(mode) && !m.isRefused(chatID)
}

// Route returns the topic key of a message about a space to a link: the space's ID for every kind of message, Home for
// a message that concerns no single space (spaceID ""), and "" (the main chat) when the link's layout is off. It is
// meant to be called at send time, so a layout changed while the message waited applies. A nil Manager routes
// everything to the main chat. A chat that refuses topics is handled by Send, which falls back to the main chat.
func (m *Manager) Route(linkID, spaceID string) string {
	if !m.on(linkID) {
		return ""
	}
	if spaceID == "" {
		return Home
	}
	return spaceID
}

func (m *Manager) name(ctx context.Context, key string) string {
	if key == HomeKey {
		return HomeName
	}
	if m.o.SpaceName != nil {
		if n := m.o.SpaceName(ctx, key); n != "" {
			return clip(n, 128)
		}
	}
	return clip(key, 128)
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

// Send sends p to the topic key of the link's chat (key "": the main chat), creating the topic if there is none and
// recovering from a deleted or closed one (see the package comment). p.MessageThreadID is set here.
func (m *Manager) Send(ctx context.Context, c *botapi.Client, linkID string, chatID int64, key string, p botapi.SendMessageParams) (botapi.Message, error) {
	p.ChatID = chatID
	thread := 0
	if key != "" && m != nil {
		var err error
		if thread, err = m.thread(ctx, c, linkID, chatID, key); err != nil {
			return botapi.Message{}, err
		}
	}
	for try := 0; ; try++ {
		p.MessageThreadID = thread
		msg, err := c.SendMessage(ctx, p)
		if err == nil || thread == 0 || !botapi.IsTopicGone(err) {
			return msg, err
		}
		m.forget(ctx, linkID, key, thread)
		if try == 0 {
			var terr error
			if thread, terr = m.thread(ctx, c, linkID, chatID, key); terr != nil {
				return botapi.Message{}, terr
			}
		} else {
			thread = 0
		}
	}
}

// thread returns the thread of a key, creating its topic; 0 when there is none to use. A rate limit (429) is returned
// so the sender repeats the whole send after the wait, and nothing is remembered about it.
func (m *Manager) thread(ctx context.Context, c *botapi.Client, linkID string, chatID int64, key string) (int, error) {
	if chatID <= 0 || m.isRefused(chatID) {
		return 0, nil
	}
	ls := m.link(linkID)
	t, ok, legacy := m.lookup(ctx, ls, linkID, key)
	if !ok {
		return 0, nil
	}
	if t != 0 && !legacy {
		return t, nil
	}
	ls.create.Lock()
	defer ls.create.Unlock()
	if legacy {
		m.retireLegacy(ctx, c, ls, linkID, chatID)
	}
	if t, ok, _ = m.lookup(ctx, ls, linkID, key); !ok || t != 0 { // another send of the link made it meanwhile
		return t, nil
	}
	if m.isRefused(chatID) { // a create that went before refused
		return 0, nil
	}
	if err := m.creates.Wait(ctx); err != nil {
		return 0, err
	}
	topic, err := c.CreateForumTopic(ctx, botapi.CreateForumTopicParams{ChatID: chatID, Name: m.name(ctx, key)})
	if err != nil {
		var be *botapi.Error
		if errors.As(err, &be) && (be.Code == 429 || be.Code >= 500) {
			return 0, err
		}
		// Topics off for the bot in this chat: use the main chat and ask again later.
		m.mu.Lock()
		m.refused[chatID] = m.o.Now().Add(RefusedFor)
		m.mu.Unlock()
		m.o.Log.Debug("telegram topics: a topic was not created", slog.String("error", err.Error()))
		return 0, nil
	}
	ls.mu.Lock()
	defer ls.mu.Unlock()
	ls.threads[key] = topic.MessageThreadID
	m.saveLocked(ctx, ls, linkID)
	return topic.MessageThreadID, nil
}

// lookup returns the stored thread of a key (0: none), false when the link's topics can't be read, and whether the
// link still has the legacy Needs-you topic to retire.
func (m *Manager) lookup(ctx context.Context, ls *linkState, linkID, key string) (thread int, ok, legacy bool) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if !m.loadLocked(ctx, ls, linkID) {
		return 0, false, false
	}
	_, legacy = ls.threads[LegacyNeeds]
	return ls.threads[key], true, legacy
}

// retireLegacy posts the note in the retired Needs-you topic and drops the topic from the link's topics. The caller
// holds ls.create, so it runs once per link however many sends race; after the key is dropped and saved, it never runs
// again for the link (idempotent across restarts too, unless the save failed). A rate limit or a server error leaves the
// key for a later send to try again; any other failure (the topic is gone, the chat refuses) drops the key without a
// note, since there is nothing left to tell.
func (m *Manager) retireLegacy(ctx context.Context, c *botapi.Client, ls *linkState, linkID string, chatID int64) {
	ls.mu.Lock()
	thread, ok := ls.threads[LegacyNeeds]
	ls.mu.Unlock()
	if !ok {
		return
	}
	if thread != 0 {
		if err := m.creates.Wait(ctx); err != nil {
			return
		}
		_, err := c.SendMessage(ctx, botapi.SendMessageParams{ChatID: chatID, MessageThreadID: thread, Text: LegacyNote, DisableNotification: true})
		var be *botapi.Error
		if err != nil && (!errors.As(err, &be) || be.Code == 429 || be.Code >= 500) {
			m.o.Log.Debug("telegram topics: the legacy topic's note was not sent; trying later", slog.String("error", err.Error()))
			return
		}
	}
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if ls.threads[LegacyNeeds] == thread {
		delete(ls.threads, LegacyNeeds)
		m.saveLocked(ctx, ls, linkID)
	}
}

// Refused reports whether a chat refused a topic lately, so it is not asked again yet. A nil Manager refuses nothing.
func (m *Manager) Refused(chatID int64) bool {
	return m != nil && m.isRefused(chatID)
}

func (m *Manager) isRefused(chatID int64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.o.Now().Before(m.refused[chatID])
}

// link returns a link's state, making it on first use.
func (m *Manager) link(linkID string) *linkState {
	m.mu.Lock()
	defer m.mu.Unlock()
	ls := m.links[linkID]
	if ls == nil {
		ls = &linkState{}
		m.links[linkID] = ls
	}
	return ls
}

// loadLocked reads a link's stored threads once; false when the store failed (nothing is used then). Must hold ls.mu.
func (m *Manager) loadLocked(ctx context.Context, ls *linkState, linkID string) bool {
	if ls.loaded {
		return true
	}
	saved := map[string]int{}
	if m.o.Store != nil {
		var err error
		if saved, err = m.o.Store.Load(ctx, linkID); err != nil {
			m.o.Log.Warn("telegram topics: reading a link's topics failed", slog.String("error", err.Error()))
			return false
		}
		if saved == nil {
			saved = map[string]int{}
		}
	}
	ls.threads, ls.loaded = saved, true
	return true
}

// SpaceOf is the reverse lookup: the space a thread of the link's private chat belongs to. It answers only from the
// thread IDs the bot itself created and stored (never from anything a user sent); the legacy Needs-you topic, a home
// topic and unknown threads answer false. It says which space a topic stands for, not whether the link may act on it:
// the caller authorizes that. It never waits for a topic being created.
func (m *Manager) SpaceOf(ctx context.Context, linkID string, thread int) (string, bool) {
	if m == nil || thread == 0 {
		return "", false
	}
	ls := m.link(linkID)
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if !m.loadLocked(ctx, ls, linkID) {
		return "", false
	}
	for k, t := range ls.threads {
		if t == thread && k != LegacyNeeds && k != HomeKey {
			return k, true
		}
	}
	return "", false
}

// forget drops a thread that Telegram says is gone, unless another send already replaced it.
func (m *Manager) forget(ctx context.Context, linkID, key string, thread int) {
	ls := m.link(linkID)
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if ls.threads[key] != thread {
		return
	}
	delete(ls.threads, key)
	m.saveLocked(ctx, ls, linkID)
}

// Forget drops everything remembered in memory about a link (it was revoked). The stored row goes with the link.
func (m *Manager) Forget(linkID string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.links, linkID)
}

// saveLocked persists a link's threads. Must hold ls.mu. A failure is only logged: the topic exists in Telegram but may
// not be remembered, so after a restart the next send can create it a second time (a duplicate topic, never a lost
// message).
func (m *Manager) saveLocked(ctx context.Context, ls *linkState, linkID string) {
	if m.o.Store == nil {
		return
	}
	cp := make(map[string]int, len(ls.threads))
	for k, v := range ls.threads {
		cp[k] = v
	}
	if err := m.o.Store.Save(ctx, linkID, cp); err != nil {
		m.o.Log.Warn("telegram topics: saving a link's topics failed", slog.String("error", err.Error()))
	}
}
