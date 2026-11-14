package notify

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
)

// Mapping is a card the bot sent: where it is, and which version of the resource it shows (telegram_messages).
type Mapping struct {
	ChatID    int64
	MessageID int
	LinkID    string
	SpaceID   string
	Key       Key
	Version   int64
	CreatedAt time.Time
}

// Mappings persists card mappings. Save is called in batches, never per send.
type Mappings interface {
	Save(ctx context.Context, rows []Mapping) error
	// Lookup returns the cards sent for a resource (all chats).
	Lookup(ctx context.Context, k Key) ([]Mapping, error)
}

const (
	maxCards   = 20000 // remembered cards (older ones are found again through Lookup)
	maxPending = 5000  // mapping rows waiting for a write; beyond it the oldest are dropped (the cards just can't be edited)
)

type cardKey struct {
	chat int64
	key  Key
}

type card struct {
	messageID int
	version   int64
}

type cards struct {
	store Mappings
	rows  int
	full  chan struct{}

	mu      sync.Mutex
	byCard  map[cardKey]card
	loaded  map[Key]bool
	pending map[[2]int64]Mapping // (chat, message) -> row
	queued  map[cardKey]int      // sends enqueued and not yet finished
}

func newCards(st Mappings, rows int) *cards {
	return &cards{store: st, rows: rows, full: make(chan struct{}, 1), byCard: map[cardKey]card{}, loaded: map[Key]bool{},
		pending: map[[2]int64]Mapping{}, queued: map[cardKey]int{}}
}

func (c *cards) get(chat int64, k Key) *card {
	c.mu.Lock()
	defer c.mu.Unlock()
	if v, ok := c.byCard[cardKey{chat, k}]; ok {
		return &v
	}
	return nil
}

// load reads a resource's cards from the database once, when the resource isn't known to this process (it was
// notified about before a restart). A failed read is not remembered, so the next window tries again.
func (c *cards) load(ctx context.Context, k Key) {
	if c.store == nil {
		return
	}
	c.mu.Lock()
	done := c.loaded[k]
	c.mu.Unlock()
	if done {
		return
	}
	rows, err := c.store.Lookup(ctx, k)
	if err != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bound()
	c.loaded[k] = true
	for _, m := range rows {
		ck := cardKey{m.ChatID, k}
		if _, ok := c.byCard[ck]; !ok {
			c.byCard[ck] = card{messageID: m.MessageID, version: m.Version}
		}
	}
}

// bound keeps the maps small. Must hold c.mu.
func (c *cards) bound() {
	for k := range c.byCard {
		if len(c.byCard) < maxCards {
			break
		}
		delete(c.byCard, k)
		delete(c.loaded, k.key) // so the database is asked again for a resource whose card was forgotten
	}
	for k := range c.loaded {
		if len(c.loaded) < maxCards {
			break
		}
		delete(c.loaded, k)
	}
}

func (c *cards) record(m Mapping) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bound()
	c.byCard[cardKey{m.ChatID, m.Key}] = card{messageID: m.MessageID, version: m.Version}
	if c.store == nil {
		return
	}
	for k := range c.pending {
		if len(c.pending) < maxPending {
			break
		}
		delete(c.pending, k)
	}
	c.pending[[2]int64{m.ChatID, int64(m.MessageID)}] = m
	if len(c.pending) >= c.rows {
		select {
		case c.full <- struct{}{}:
		default:
		}
	}
}

func (c *cards) forget(chat int64, k Key) {
	c.mu.Lock()
	delete(c.byCard, cardKey{chat, k})
	c.mu.Unlock()
}

// flush writes the pending mappings in one transaction; it writes nothing when there are none.
func (c *cards) flush(ctx context.Context) error {
	c.mu.Lock()
	if len(c.pending) == 0 || c.store == nil {
		c.mu.Unlock()
		return nil
	}
	rows := make([]Mapping, 0, len(c.pending))
	for _, m := range c.pending {
		rows = append(rows, m)
	}
	c.pending = map[[2]int64]Mapping{}
	c.mu.Unlock()
	if err := c.store.Save(ctx, rows); err != nil {
		c.mu.Lock() // keep them for the next flush (a newer row for the same message wins)
		for _, m := range rows {
			k := [2]int64{m.ChatID, int64(m.MessageID)}
			if _, ok := c.pending[k]; !ok && len(c.pending) < maxPending {
				c.pending[k] = m
			}
		}
		c.mu.Unlock()
		return err
	}
	return nil
}

// upsert sends the resource's card, or edits it when it was sent before. It runs on a sender goroutine, so it sees
// the card of a send that completed ahead of it in the chat's queue.
func (c *cards) upsert(ctx context.Context, cl *botapi.Client, linkID string, chat int64, r *Resource, spaceID, text string, ents []botapi.MessageEntity) error {
	if cd := c.get(chat, r.Key); cd != nil {
		_, err := cl.EditMessageText(ctx, botapi.EditMessageTextParams{ChatID: chat, MessageID: cd.messageID, Text: text, Entities: ents})
		switch {
		case err == nil || isNotModified(err):
			c.record(Mapping{ChatID: chat, MessageID: cd.messageID, LinkID: linkID, SpaceID: spaceID, Key: r.Key, Version: r.Version, CreatedAt: time.Now()})
			return nil
		case isGone(err) && !r.Final:
			c.forget(chat, r.Key) // the message was deleted: send the card again
		case isGone(err):
			c.forget(chat, r.Key)
			return nil
		default:
			return err
		}
	}
	m, err := cl.SendMessage(ctx, botapi.SendMessageParams{ChatID: chat, Text: text, Entities: ents})
	if err != nil {
		return err
	}
	c.record(Mapping{ChatID: chat, MessageID: m.MessageID, LinkID: linkID, SpaceID: spaceID, Key: r.Key, Version: r.Version, CreatedAt: time.Now()})
	return nil
}

func isNotModified(err error) bool {
	var e *botapi.Error
	return errors.As(err, &e) && e.Code == 400 && strings.Contains(e.Description, "message is not modified")
}

// isGone: the message to edit is gone or can no longer be edited.
func isGone(err error) bool {
	var e *botapi.Error
	return errors.As(err, &e) && e.Code == 400 &&
		(strings.Contains(e.Description, "message to edit not found") || strings.Contains(e.Description, "message can't be edited"))
}

// Card text limits: a card is a notification, not the full body (the Mini App has that).
const (
	maxTitle  = 200
	maxDetail = 600
)

// renderCard builds a card's plain text and its entities (the first line in bold; offsets in UTF-16 code units).
func renderCard(r *Resource, full bool) (string, []botapi.MessageEntity) {
	head := r.Label + ": " + clip(oneLine(r.Title), maxTitle)
	var b strings.Builder
	b.WriteString(head)
	if r.State != "" {
		b.WriteString("\n")
		b.WriteString(r.State)
	}
	if full && r.Detail != "" {
		b.WriteString("\n\n")
		b.WriteString(clip(r.Detail, maxDetail))
	} else if !full {
		b.WriteString("\n\nOpen the app for details.")
	}
	return b.String(), []botapi.MessageEntity{{Type: "bold", Offset: 0, Length: len(utf16.Encode([]rune(head)))}}
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// clip cuts s to n runes (with an ellipsis), never inside a rune.
func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}

// sendQueued reports whether a send of the card is waiting in the sender (its card doesn't exist yet).
func (c *cards) sendQueued(chat int64, k Key) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.queued[cardKey{chat, k}] > 0
}

// queue counts a send in the sender; the returned function ends it (the sender's Done).
func (c *cards) queue(chat int64, k Key) func(error) {
	ck := cardKey{chat, k}
	c.mu.Lock()
	c.queued[ck]++
	c.mu.Unlock()
	return func(error) {
		c.mu.Lock()
		if c.queued[ck]--; c.queued[ck] <= 0 {
			delete(c.queued, ck)
		}
		c.mu.Unlock()
	}
}
