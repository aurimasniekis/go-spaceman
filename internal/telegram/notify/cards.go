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
	// Kind is the row's kind: MappingCard ("" too) or MappingNotice.
	Kind string
}

// Mappings persists card mappings. Save is called in batches, never per send.
type Mappings interface {
	Save(ctx context.Context, rows []Mapping) error
	// Lookup returns the cards sent for a resource (all chats).
	Lookup(ctx context.Context, k Key) ([]Mapping, error)
	// NoticeLinks returns the links that were sent the one-time notice of a type (kind notice), read once at start.
	NoticeLinks(ctx context.Context, typ string) ([]string, error)
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
	// remindedSeen and escalatedSeen are the reminder and escalation instants (microseconds) already announced to this
	// chat. A card loaded after a restart starts at the time it was last touched, so what happened before is not announced
	// again. assignee is the action's assignee as last seen, once known.
	remindedSeen, escalatedSeen int64
	assignee                    string
	known                       bool
}

type cards struct {
	now   func() time.Time
	store Mappings
	rows  int
	full  chan struct{}

	mu      sync.Mutex
	byCard  map[cardKey]card
	loaded  map[Key]bool
	pending map[[2]int64]Mapping // (chat, message) -> row
	queued  map[cardKey]int      // sends enqueued and not yet finished
	// base is the announce baseline (reminders, escalations, assignee) of a card whose first send is queued or in flight,
	// so a change observed before the send's mapping is recorded is still announced, and only once.
	base map[cardKey]card
}

func newCards(st Mappings, rows int, now func() time.Time) *cards {
	return &cards{now: now, store: st, rows: rows, full: make(chan struct{}, 1), byCard: map[cardKey]card{}, loaded: map[Key]bool{},
		pending: map[[2]int64]Mapping{}, queued: map[cardKey]int{}, base: map[cardKey]card{}}
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
			seen := m.CreatedAt.UnixMicro()
			c.byCard[ck] = card{messageID: m.MessageID, version: m.Version, remindedSeen: seen, escalatedSeen: seen}
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

// record remembers a sent or edited card. r is the state it shows (nil: only the mapping is refreshed).
func (c *cards) record(m Mapping, r *Resource) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bound()
	ck := cardKey{m.ChatID, m.Key}
	cur, ok := c.byCard[ck]
	if !ok {
		cur = c.base[ck] // what was observed while the first send was on its way
	}
	cur.messageID, cur.version = m.MessageID, m.Version
	if r != nil {
		cur.remindedSeen, cur.escalatedSeen = max(cur.remindedSeen, r.RemindedAt), max(cur.escalatedSeen, r.EscalatedAt)
		if !cur.known {
			cur.assignee, cur.known = r.Assignee, true
		}
	}
	c.byCard[cardKey{m.ChatID, m.Key}] = cur
	c.pendLocked(m)
}

// pend queues a mapping row for the next batch write without remembering it as a card (a notice).
func (c *cards) pend(m Mapping) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pendLocked(m)
}

// pendLocked queues a row for the next batch write. Must hold c.mu.
func (c *cards) pendLocked(m Mapping) {
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

// observe notes the reminder, escalation and assignee state of the resource for a chat's card and says what changed
// since the last observation: a reminder or escalation newer than the ones announced, and an assignment (the assignee
// became a different one than the last seen, which is known). The first sight of an assignee announces nothing.
func (c *cards) observe(chat int64, r *Resource) (reminded, escalated, reassigned bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ck := cardKey{chat, r.Key}
	cur, ok := c.byCard[ck]
	inBase := false
	if !ok {
		if cur, inBase = c.base[ck]; !inBase {
			return false, false, false
		}
	}
	reminded, escalated = r.RemindedAt > cur.remindedSeen, r.EscalatedAt > cur.escalatedSeen
	reassigned = cur.known && cur.assignee != r.Assignee
	cur.remindedSeen, cur.escalatedSeen = max(cur.remindedSeen, r.RemindedAt), max(cur.escalatedSeen, r.EscalatedAt)
	cur.assignee, cur.known = r.Assignee, true
	if inBase {
		c.base[ck] = cur
	} else {
		c.byCard[ck] = cur
	}
	return reminded, escalated, reassigned
}

// touch refreshes the stored time of a card, which is also when its reminders and escalations were last announced.
func (c *cards) touch(linkID string, chat int64, r *Resource, now time.Time) {
	if cd := c.get(chat, r.Key); cd != nil {
		c.record(Mapping{ChatID: chat, MessageID: cd.messageID, LinkID: linkID, SpaceID: r.SpaceID, Key: r.Key, Version: cd.version, CreatedAt: now}, nil)
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

// sendFunc sends a message, to the topic its caller chose (or the main chat).
type sendFunc func(ctx context.Context, cl *botapi.Client, p botapi.SendMessageParams) (botapi.Message, error)

// upsert sends the resource's card, or edits it when it was sent before. It runs on a sender goroutine, so it sees
// the card of a send that completed ahead of it in the chat's queue.
func (c *cards) upsert(ctx context.Context, cl *botapi.Client, send sendFunc, linkID string, chat int64, r *Resource, spaceID, text string, ents []botapi.MessageEntity, protect, silent bool) error {
	if cd := c.get(chat, r.Key); cd != nil {
		_, err := cl.EditMessageText(ctx, botapi.EditMessageTextParams{ChatID: chat, MessageID: cd.messageID, Text: text, Entities: ents})
		switch {
		case err == nil || isNotModified(err):
			c.record(Mapping{ChatID: chat, MessageID: cd.messageID, LinkID: linkID, SpaceID: spaceID, Key: r.Key, Version: r.Version, CreatedAt: c.now()}, r)
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
	m, err := send(ctx, cl, botapi.SendMessageParams{ChatID: chat, Text: text, Entities: ents, ProtectContent: protect, DisableNotification: silent})
	if err != nil {
		return err
	}
	c.record(Mapping{ChatID: chat, MessageID: m.MessageID, LinkID: linkID, SpaceID: spaceID, Key: r.Key, Version: r.Version, CreatedAt: c.now()}, r)
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

// state returns, under one lock, the card (nil: none) and whether a send of the card is waiting in the sender. One
// snapshot, so a send finishing between two reads can't make the card look absent and the send gone at once.
func (c *cards) state(chat int64, k Key) (cd *card, queued bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ck := cardKey{chat, k}
	if v, ok := c.byCard[ck]; ok {
		cd = &v
	}
	return cd, c.queued[ck] > 0
}

// queue counts a send in the sender; the returned function ends it (the sender's Done).
func (c *cards) queue(chat int64, r *Resource) func(error) {
	k := r.Key
	ck := cardKey{chat, k}
	c.mu.Lock()
	c.queued[ck]++
	if _, ok := c.byCard[ck]; !ok {
		if _, ok := c.base[ck]; !ok {
			c.base[ck] = card{remindedSeen: r.RemindedAt, escalatedSeen: r.EscalatedAt, assignee: r.Assignee, known: true}
		}
	}
	c.mu.Unlock()
	return func(error) {
		c.mu.Lock()
		if c.queued[ck]--; c.queued[ck] <= 0 {
			delete(c.queued, ck)
			delete(c.base, ck)
		}
		c.mu.Unlock()
	}
}
