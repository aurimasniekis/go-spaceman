// Package topics is topics mode of the private chat (ADR-0083 decision 16, plan §1.2): a space whose Telegram settings
// have `topics` on gets a topic of its own in the link's private chat, and what needs the user (open actions) goes to a
// "Needs you" topic. A topic is created by the bot on first use and its thread ID is remembered per link in
// telegram_links.topics_json. Messages are addressed to it with message_thread_id.
//
// A topic that was deleted or closed (the send fails with "message thread not found" or TOPIC_CLOSED) is forgotten and
// created again once; if that fails too, or the chat does not have topics enabled for the bot, the message goes to the
// main chat, so a notification is never lost to a topic. A chat that refuses topics is not asked again for a while.
// Authorization is unchanged: topics only choose where a message appears, never whether a link may receive it.
package topics

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
)

// Needs is the key of the Needs-you topic; every other key is a space ID.
const Needs = "needs"

// NeedsName is the Needs-you topic's title.
const NeedsName = "Needs you"

// RefusedFor is how long a chat that refused to create a topic is left alone.
const RefusedFor = 10 * time.Minute

// Store keeps a link's thread IDs (key → message_thread_id).
type Store interface {
	Load(ctx context.Context, linkID string) (map[string]int, error)
	Save(ctx context.Context, linkID string, threads map[string]int) error
}

// Options configure a Manager. Enabled is required.
type Options struct {
	Store Store
	// Enabled reports whether a space is in topics mode (its Telegram settings have topics on and it is visible).
	Enabled func(spaceID string) bool
	// SpaceName is a space's title for its topic (default: the ID).
	SpaceName func(ctx context.Context, spaceID string) string
	Now       func() time.Time
	Log       *slog.Logger
}

// Manager routes messages to topics.
type Manager struct {
	o Options

	mu      sync.Mutex // guards links and refused only; never held across I/O
	links   map[string]*linkState
	refused map[int64]time.Time // chat → when to ask again
}

// linkState is one link's thread IDs, with a lock of its own that is held across the topic create and the save.
type linkState struct {
	mu      sync.Mutex
	threads map[string]int // key → thread
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
	return &Manager{o: o, links: map[string]*linkState{}, refused: map[int64]time.Time{}}
}

// Route returns the topic key a message about a space goes to: Needs for something that needs the user, the space's ID
// otherwise, and "" for the main chat (topics mode is off for the space). A nil Manager routes everything to the main chat.
func (m *Manager) Route(spaceID string, needsYou bool) string {
	if m == nil || m.o.Enabled == nil || !m.o.Enabled(spaceID) {
		return ""
	}
	if needsYou {
		return Needs
	}
	return spaceID
}

func (m *Manager) name(ctx context.Context, key string) string {
	if key == Needs {
		return NeedsName
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
	if m.isRefused(chatID) {
		return 0, nil
	}
	ls := m.link(linkID)
	ls.mu.Lock()
	defer ls.mu.Unlock() // held across the create call: two sends of one link must not both create a topic; other links don't wait
	if !m.loadLocked(ctx, ls, linkID) {
		return 0, nil
	}
	if t := ls.threads[key]; t != 0 {
		return t, nil
	}
	t, err := c.CreateForumTopic(ctx, botapi.CreateForumTopicParams{ChatID: chatID, Name: m.name(ctx, key)})
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
	ls.threads[key] = t.MessageThreadID
	m.saveLocked(ctx, ls, linkID)
	return t.MessageThreadID, nil
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

// loadLocked reads a link's stored threads once; false when the store failed (nothing is used then).
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
// thread IDs the bot itself created and stored (never from anything a user sent); the Needs-you topic and unknown
// threads answer false. It says which space a topic stands for, not whether the link may act on it: the caller
// authorizes that.
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
		if t == thread && k != Needs {
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

// saveLocked persists a link's threads. A failure is only logged: the topic exists in Telegram but may not be
// remembered, so after a restart the next send can create it a second time (a duplicate topic, never a lost message).
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
