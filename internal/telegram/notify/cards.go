package notify

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/render"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/sender"
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
	// Cont marks a continuation message: the rest of a long body, sent as a reply to the card (Key names the card's
	// resource).
	Cont bool
	// Delete removes the row of a continuation message that was deleted (Save only; set with Cont).
	Delete bool
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
	// conts are the continuation messages of a long body, in order.
	conts []cont
	// sig is the signature of what the card shows (0: unknown, after a restart).
	sig uint64
	// remindedSeen and escalatedSeen are the reminder and escalation instants (microseconds) already announced to this
	// chat. A card loaded after a restart starts at the time it was last touched, so what happened before is not announced
	// again. assignee is the action's assignee as last seen, once known.
	remindedSeen, escalatedSeen int64
	assignee                    string
	known                       bool
}

// cont is a continuation message and a signature of what it shows (0 after a restart), so a retried update doesn't
// edit again what is already right (every edit counts against the chat's rate limit).
type cont struct {
	id  int
	sig uint64
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
		if _, ok := c.byCard[ck]; !ok && !m.Cont {
			seen := m.CreatedAt.UnixMicro()
			c.byCard[ck] = card{messageID: m.MessageID, version: m.Version, remindedSeen: seen, escalatedSeen: seen}
		}
	}
	for _, m := range rows { // continuations after the cards, oldest first (message IDs grow)
		ck := cardKey{m.ChatID, k}
		if cd, ok := c.byCard[ck]; ok && m.Cont && !slices.ContainsFunc(cd.conts, func(x cont) bool { return x.id == m.MessageID }) {
			cd.conts = append(slices.Clone(cd.conts), cont{id: m.MessageID})
			slices.SortFunc(cd.conts, func(a, b cont) int { return a.id - b.id })
			c.byCard[ck] = cd
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
	if m.Cont {
		c.recordCont(m, 0)
		return
	}
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
	if c.store != nil {
		for _, ct := range cur.conts { // refresh the continuation rows with the card's, so retention never prunes them before it
			cm := m
			cm.MessageID, cm.Cont = ct.id, true
			c.pending[[2]int64{m.ChatID, int64(ct.id)}] = cm
		}
	}
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

// recordCont remembers a continuation message of a card and queues its mapping.
func (c *cards) recordCont(m Mapping, sig uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ck := cardKey{m.ChatID, m.Key}
	if cd, ok := c.byCard[ck]; ok {
		cd.conts = slices.Clone(cd.conts)
		if i := slices.IndexFunc(cd.conts, func(x cont) bool { return x.id == m.MessageID }); i >= 0 {
			cd.conts[i].sig = sig
		} else {
			cd.conts = append(cd.conts, cont{m.MessageID, sig})
		}
		c.byCard[ck] = cd
	}
	if c.store != nil {
		c.pending[[2]int64{m.ChatID, int64(m.MessageID)}] = m
	}
}

func (c *cards) setSig(chat int64, k Key, sig uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cd, ok := c.byCard[cardKey{chat, k}]; ok {
		cd.sig = sig
		c.byCard[cardKey{chat, k}] = cd
	}
}

// dropConts forgets continuation messages that were deleted and queues the removal of their rows, so a restart doesn't
// load them again.
func (c *cards) dropConts(chat int64, k Key, n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ck := cardKey{chat, k}
	if cd, ok := c.byCard[ck]; ok && n < len(cd.conts) {
		if c.store != nil {
			for _, ct := range cd.conts[n:] {
				c.pending[[2]int64{chat, int64(ct.id)}] = Mapping{ChatID: chat, MessageID: ct.id, Key: k, Cont: true, Delete: true}
			}
		}
		cd.conts = slices.Clone(cd.conts[:n])
		c.byCard[ck] = cd
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
// the card of a send that completed ahead of it in the chat's queue. parts[0] is the card; the others are its
// continuation messages, replies to the card that are edited, sent or deleted along with it.
func (c *cards) upsert(ctx context.Context, cl *botapi.Client, send sendFunc, linkID string, chat int64, r *Resource, spaceID string, parts []cardPart, protect, silent bool) error {
	sig := parts[0].sig()
	// The sender paid for the first Bot API call of this job; every further one pays for itself (sender.Take).
	paid := false
	pay := func() error {
		if !paid {
			paid = true
			return nil
		}
		return sender.Take(ctx)
	}
	if cd := c.get(chat, r.Key); cd != nil {
		var err error
		if cd.sig != sig || cd.version != r.Version { // a retry after a failed continuation doesn't edit the card again (edits count against the rate limit)
			_ = pay()
			_, err = cl.EditMessageText(ctx, botapi.EditMessageTextParams{ChatID: chat, MessageID: cd.messageID, Text: parts[0].Text, Entities: parts[0].Entities, ReplyMarkup: parts[0].markup()})
		}
		switch {
		case err == nil || isNotModified(err):
			c.record(Mapping{ChatID: chat, MessageID: cd.messageID, LinkID: linkID, SpaceID: spaceID, Key: r.Key, Version: r.Version, CreatedAt: c.now()}, r)
			c.setSig(chat, r.Key, sig)
			return c.syncConts(ctx, cl, pay, send, linkID, chat, r, spaceID, cd.messageID, parts, protect)
		case isGone(err) && !r.Final:
			_ = c.deleteConts(ctx, cl, pay, chat, r.Key, 0) // best effort: the card is sent again, with new continuations
			c.forget(chat, r.Key)                           // the message was deleted: send the card again
		case isGone(err):
			_ = c.deleteConts(ctx, cl, pay, chat, r.Key, 0)
			c.forget(chat, r.Key)
			return nil
		default:
			return err
		}
	}
	if err := pay(); err != nil {
		return err
	}
	m, err := send(ctx, cl, botapi.SendMessageParams{ChatID: chat, Text: parts[0].Text, Entities: parts[0].Entities, ReplyMarkup: parts[0].markupAny(), ProtectContent: protect, DisableNotification: silent})
	if err != nil {
		return err
	}
	c.record(Mapping{ChatID: chat, MessageID: m.MessageID, LinkID: linkID, SpaceID: spaceID, Key: r.Key, Version: r.Version, CreatedAt: c.now()}, r)
	c.setSig(chat, r.Key, sig)
	return c.syncConts(ctx, cl, pay, send, linkID, chat, r, spaceID, m.MessageID, parts, protect)
}

// syncConts brings the continuation messages in line with parts[1:]: existing ones are edited, missing ones sent as
// replies to the card (silently: the card already notified), and ones the body no longer needs deleted.
func (c *cards) syncConts(ctx context.Context, cl *botapi.Client, pay func() error, send sendFunc, linkID string, chat int64, r *Resource, spaceID string, cardID int, parts []cardPart, protect bool) error {
	cd := c.get(chat, r.Key)
	if cd == nil {
		return nil
	}
	have := cd.conts
	for i, p := range parts[1:] {
		sig := p.sig()
		if i < len(have) {
			if have[i].sig == sig {
				continue
			}
			if err := pay(); err != nil {
				return err
			}
			_, err := cl.EditMessageText(ctx, botapi.EditMessageTextParams{ChatID: chat, MessageID: have[i].id, Text: p.Text, Entities: p.Entities, ReplyMarkup: p.markup()})
			if err == nil || isNotModified(err) {
				c.recordCont(Mapping{ChatID: chat, MessageID: have[i].id, LinkID: linkID, SpaceID: spaceID, Key: r.Key, Version: r.Version, CreatedAt: c.now(), Cont: true}, sig)
				continue
			}
			if !isGone(err) {
				return err
			}
			// A continuation was deleted: drop it and the ones after it and send them again, so the order holds.
			if err := c.deleteConts(ctx, cl, pay, chat, r.Key, i); err != nil {
				return err
			}
			have = have[:i]
		}
		if err := pay(); err != nil {
			return err
		}
		m, err := send(ctx, cl, botapi.SendMessageParams{ChatID: chat, Text: p.Text, Entities: p.Entities, ReplyMarkup: p.markupAny(),
			ReplyParameters: &botapi.ReplyParameters{MessageID: cardID}, ProtectContent: protect, DisableNotification: true})
		if err != nil {
			return err
		}
		c.recordCont(Mapping{ChatID: chat, MessageID: m.MessageID, LinkID: linkID, SpaceID: spaceID, Key: r.Key, Version: r.Version, CreatedAt: c.now(), Cont: true}, sig)
		have = append(slices.Clone(have), cont{m.MessageID, sig})
	}
	if len(have) > len(parts)-1 {
		return c.deleteConts(ctx, cl, pay, chat, r.Key, len(parts)-1)
	}
	return nil
}

// deleteConts deletes a card's continuation messages from the nth on, last first, and forgets the ones deleted. A
// message already gone counts as deleted; any other failure (a rate limit, say) is returned, so the sender tries
// again and no continuation is left behind.
func (c *cards) deleteConts(ctx context.Context, cl *botapi.Client, pay func() error, chat int64, k Key, from int) error {
	cd := c.get(chat, k)
	if cd == nil {
		return nil
	}
	for j := len(cd.conts) - 1; j >= from; j-- {
		if err := pay(); err != nil {
			c.dropConts(chat, k, j+1)
			return err
		}
		if err := cl.DeleteMessage(ctx, chat, cd.conts[j].id); err != nil && !isDeleteGone(err) {
			c.dropConts(chat, k, j+1)
			return err
		}
	}
	c.dropConts(chat, k, from)
	return nil
}

// isDeleteGone: the message to delete is gone or can no longer be deleted.
func isDeleteGone(err error) bool {
	var e *botapi.Error
	return errors.As(err, &e) && e.Code == 400 &&
		(strings.Contains(e.Description, "message to delete not found") || strings.Contains(e.Description, "message can't be deleted"))
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

// Card text limits. A card at content level full carries the whole body: the first message up to maxMessage UTF-16
// units, the rest in continuation messages ("(2/3)"), at most maxParts messages in all (the Mini App has the rest).
const (
	maxTitle   = 200
	maxMessage = 4096
	maxParts   = 4
	contMark   = "(%d/%d)\n"
)

// cardPart is one message of a card: text, entities and, on the last one of a cut body, a Mini App button.
type cardPart struct {
	Text     string
	Entities []botapi.MessageEntity
	Buttons  []render.Button
}

// sig is a signature of what the part shows.
func (p cardPart) sig() uint64 {
	h := fnv.New64a()
	_, _ = fmt.Fprintf(h, "%s\x00%v\x00%v", p.Text, p.Entities, p.Buttons)
	return h.Sum64() | 1 // never 0, which means unknown
}

func (p cardPart) markup() *botapi.InlineKeyboardMarkup {
	if len(p.Buttons) == 0 {
		return nil
	}
	row := make([]botapi.InlineKeyboardButton, len(p.Buttons))
	for i, b := range p.Buttons {
		row[i] = botapi.InlineKeyboardButton{Text: b.Text, WebApp: &botapi.WebAppInfo{URL: b.WebAppURL}}
	}
	return &botapi.InlineKeyboardMarkup{InlineKeyboard: [][]botapi.InlineKeyboardButton{row}}
}

// markupAny is markup as the interface sendMessage takes (nil stays nil).
func (p cardPart) markupAny() botapi.ReplyMarkup {
	if m := p.markup(); m != nil {
		return m
	}
	return nil
}

func units(s string) int { return len(utf16.Encode([]rune(s))) }

func entities(es []render.Entity, shift int) []botapi.MessageEntity {
	out := make([]botapi.MessageEntity, len(es))
	for i, e := range es {
		out[i] = botapi.MessageEntity{Type: e.Type, Offset: e.Offset + shift, Length: e.Length, URL: e.URL, Language: e.Language}
	}
	return out
}

// renderCard builds a card's messages: the first has the bold title, the state line and the start of the body; at
// content level full the body is Markdown turned into entities (never raw HTML; links only http, https and mailto)
// and split into continuation messages when it doesn't fit. appURL, when set, is the Mini App link of a cut body.
func renderCard(r *Resource, full bool, appURL string) []cardPart {
	head := r.Label + ": " + clip(oneLine(r.Title), maxTitle)
	var b strings.Builder
	b.WriteString(head)
	if r.State != "" {
		b.WriteString("\n")
		b.WriteString(r.State)
	}
	bold := []botapi.MessageEntity{{Type: "bold", Offset: 0, Length: units(head)}}
	if !full {
		b.WriteString("\n\nOpen the app for details.")
		return []cardPart{{Text: b.String(), Entities: bold}}
	}
	if strings.TrimSpace(r.Detail) == "" {
		return []cardPart{{Text: b.String(), Entities: bold}}
	}
	b.WriteString("\n\n")
	prefix := units(b.String())
	// Every part is cut to the same size, small enough for the first (the title and state before the body).
	msg := render.Parse(r.Detail).Entities(render.Options{Limit: maxMessage - prefix, MaxParts: maxParts, ContinueURL: appURL})
	if len(msg.Parts) == 0 {
		return []cardPart{{Text: b.String(), Entities: bold}}
	}
	first := msg.Parts[0]
	parts := []cardPart{{Text: b.String() + first.Text, Entities: append(bold, entities(first.Entities, prefix)...), Buttons: first.Buttons}}
	for i, p := range msg.Parts[1:] {
		mark := fmt.Sprintf(contMark, i+2, len(msg.Parts))
		parts = append(parts, cardPart{Text: mark + p.Text, Entities: entities(p.Entities, units(mark)), Buttons: p.Buttons})
	}
	return parts
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
