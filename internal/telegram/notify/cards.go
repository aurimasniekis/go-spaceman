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
	"unicode/utf8"

	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/card"
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
	// Thread is the message_thread_id the card was sent in (0: the main chat), meaningful when ThreadKnown. A row from
	// before the thread was recorded has ThreadKnown false.
	Thread      int
	ThreadKnown bool
	// Delete removes the row (Save only): of a continuation message that was deleted (set with Cont) or of a card that
	// was given up.
	Delete bool
}

// Mappings persists card mappings. Save is called in batches, never per send.
type Mappings interface {
	Save(ctx context.Context, rows []Mapping) error
	// Lookup returns the cards sent for a resource (all chats).
	Lookup(ctx context.Context, k Key) ([]Mapping, error)
	// NoticeLinks returns the links that were sent the one-time notice of a type (kind notice), read once at start.
	NoticeLinks(ctx context.Context, typ string) ([]string, error)
	// HomeMessages returns the links' home summaries (kind home), read once at start.
	HomeMessages(ctx context.Context) ([]HomeRow, error)
}

const (
	maxCards   = 20000 // remembered cards (older ones are found again through Lookup)
	maxPending = 5000  // mapping rows waiting for a write; beyond it the oldest are dropped (the cards just can't be edited)
)

type cardKey struct {
	chat int64
	key  Key
}

type cardState struct {
	messageID int
	version   int64
	// thread is the topic the card was sent in (0: the main chat) when threadKnown. The card's continuations and the
	// replies to it stay in it, whatever the chat layout is by now.
	thread      int
	threadKnown bool
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
	// linkID and spaceID are the link and space the card was sent for ("" for a card loaded from its row: the chat
	// handler's lookup then reads the row).
	linkID, spaceID string
	// final: what the card shows is settled. form: the chat handler shows the action's form on the card message (at
	// version), not the card. gone: the card says only card.Gone (the link may no longer see the item).
	final, form, gone bool
	// alert is the last reminder, escalation or assignment alert sent as a reply to the card (0: none); a newer one
	// and the card's final state delete it (best effort: Telegram deletes only within 48 h).
	alert int
	// queued is the newest version of the resource an edit of the card was queued for: the chat handler's edits of an
	// older version are refused from then on, even while that edit waits in the sender (or was given up).
	queued int64
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

	// locks serialize the edits of a chat's cards between the notifier (upsert, retract) and the chat handler
	// (Notifier.EditCard), one stripe per chat: whoever edits second sees what the first one left.
	locks [64]sync.Mutex

	mu      sync.Mutex
	byCard  map[cardKey]cardState
	byMsg   map[[2]int64]Key // (chat, card message) -> resource
	loaded  map[Key]bool
	pending map[[2]int64]Mapping // (chat, message) -> row
	queued  map[cardKey]int      // sends enqueued and not yet finished
	// base is the announce baseline (reminders, escalations, assignee) of a card whose first send is queued or in flight,
	// so a change observed before the send's mapping is recorded is still announced, and only once.
	base map[cardKey]cardState
}

func newCards(st Mappings, rows int, now func() time.Time) *cards {
	return &cards{now: now, store: st, rows: rows, full: make(chan struct{}, 1), byCard: map[cardKey]cardState{}, byMsg: map[[2]int64]Key{}, loaded: map[Key]bool{},
		pending: map[[2]int64]Mapping{}, queued: map[cardKey]int{}, base: map[cardKey]cardState{}}
}

func (c *cards) get(chat int64, k Key) *cardState {
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
			c.byCard[ck] = cardState{messageID: m.MessageID, version: m.Version, remindedSeen: seen, escalatedSeen: seen, thread: m.Thread, threadKnown: m.ThreadKnown}
			c.byMsg[[2]int64{m.ChatID, int64(m.MessageID)}] = k
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
		delete(c.byMsg, [2]int64{k.chat, int64(c.byCard[k].messageID)})
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
	if ok && cur.messageID != m.MessageID {
		delete(c.byMsg, [2]int64{m.ChatID, int64(cur.messageID)})
	}
	cur.messageID, cur.version = m.MessageID, m.Version
	c.byMsg[[2]int64{m.ChatID, int64(m.MessageID)}] = m.Key
	if m.LinkID != "" {
		cur.linkID, cur.spaceID = m.LinkID, m.SpaceID
	}
	if m.ThreadKnown {
		cur.thread, cur.threadKnown = m.Thread, true
	} else {
		m.Thread, m.ThreadKnown = cur.thread, cur.threadKnown // a refresh keeps the thread the card was sent in
	}
	if r != nil {
		// The notifier's own state of the card: what the chat handler or a retraction left is replaced.
		cur.final, cur.form, cur.gone = r.Final, false, false
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
// It reads and writes under one lock, so a version recorded meanwhile (an edit of the notifier or the chat) is kept.
func (c *cards) touch(linkID string, chat int64, r *Resource, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cd, ok := c.byCard[cardKey{chat, r.Key}]
	if !ok {
		return
	}
	m := Mapping{ChatID: chat, MessageID: cd.messageID, LinkID: linkID, SpaceID: r.SpaceID, Key: r.Key, Version: cd.version, CreatedAt: now, Thread: cd.thread, ThreadKnown: cd.threadKnown}
	if c.store != nil {
		for _, ct := range cd.conts { // the continuation rows move forward with the card's, as record does
			cm := m
			cm.MessageID, cm.Cont = ct.id, true
			c.pending[[2]int64{chat, int64(ct.id)}] = cm
		}
	}
	c.pendLocked(m)
}

// retracted: the card says only card.Gone and nothing of the item is left (no continuation, no alert).
func (cd *cardState) retracted() bool { return cd.gone && len(cd.conts) == 0 && cd.alert == 0 }

func (c *cards) forget(chat int64, k Key) {
	c.mu.Lock()
	if cd, ok := c.byCard[cardKey{chat, k}]; ok {
		delete(c.byMsg, [2]int64{chat, int64(cd.messageID)})
	}
	delete(c.byCard, cardKey{chat, k})
	c.mu.Unlock()
}

// chatLock is the lock of a chat's card edits.
func (c *cards) chatLock(chat int64) *sync.Mutex {
	return &c.locks[uint64(chat)%uint64(len(c.locks))] //nolint:gosec // G115: any stripe will do for a negative ID
}

// byMessage returns the card a chat message is, from memory (nil: not known here).
func (c *cards) byMessage(chat int64, msg int) (Key, *cardState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	k, ok := c.byMsg[[2]int64{chat, int64(msg)}]
	if !ok {
		return Key{}, nil
	}
	cd, ok := c.byCard[cardKey{chat, k}]
	if !ok || cd.messageID != msg {
		return Key{}, nil
	}
	return k, &cd
}

// shown records what the chat handler put on a card message: the action at version, as its form or as the card again
// (a settled one when final). The signature is unknown from now on, so the notifier's next edit isn't skipped.
func (c *cards) shown(chat int64, k Key, version int64, form, final bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cd, ok := c.byCard[cardKey{chat, k}]; ok {
		cd.version, cd.form, cd.final, cd.sig = version, form, final, 0
		c.byCard[cardKey{chat, k}] = cd
	}
}

// want records that an edit of the card to version is queued.
func (c *cards) want(chat int64, k Key, version int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cd, ok := c.byCard[cardKey{chat, k}]; ok && version > cd.queued {
		cd.queued = version
		c.byCard[cardKey{chat, k}] = cd
	}
}

// markGone records that a card now says only card.Gone.
func (c *cards) markGone(chat int64, k Key) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cd, ok := c.byCard[cardKey{chat, k}]; ok {
		cd.gone, cd.form, cd.sig = true, false, 0
		c.byCard[cardKey{chat, k}] = cd
	}
}

// swapAlert remembers the alert just sent as a reply to a card and returns the one before it (0: none).
func (c *cards) swapAlert(chat int64, k Key, id int) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	cd, ok := c.byCard[cardKey{chat, k}]
	if !ok {
		return 0
	}
	old := cd.alert
	cd.alert = id
	c.byCard[cardKey{chat, k}] = cd
	return old
}

// dropAlert deletes a card's last alert. One that is gone or too old to delete is forgotten; any other failure keeps
// it for the next try and is returned.
func (c *cards) dropAlert(ctx context.Context, cl *botapi.Client, pay func() error, chat int64, k Key) error {
	old := c.swapAlert(chat, k, 0)
	if old == 0 {
		return nil
	}
	err := pay()
	if err == nil {
		if err = cl.DeleteMessage(ctx, chat, old); isDeleteGone(err) {
			err = nil
		}
	}
	if err != nil {
		c.swapAlert(chat, k, old)
	}
	return err
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

// payer returns the charge of a sender job's Bot API calls: the sender paid for the first one; every further one
// pays for itself (sender.Take).
func payer(ctx context.Context) func() error {
	paid := false
	return func() error {
		if !paid {
			paid = true
			return nil
		}
		return sender.Take(ctx)
	}
}

// upsert sends the resource's card, or edits it when it was sent before. It runs on a sender goroutine, so it sees
// the card of a send that completed ahead of it in the chat's queue. parts[0] is the card; the others are its
// continuation messages, replies to the card that are edited, sent or deleted along with it. It holds the chat's card
// lock, so an edit of the chat handler (Notifier.EditCard) lands before or after it, never in between.
func (c *cards) upsert(ctx context.Context, cl *botapi.Client, send sendFunc, linkID string, chat int64, r *Resource, spaceID string, parts []cardPart, protect, silent bool) error {
	mu := c.chatLock(chat)
	mu.Lock()
	defer mu.Unlock()
	pay := payer(ctx)
	err := c.upsertOnce(ctx, cl, pay, send, linkID, chat, r, spaceID, parts, protect, silent)
	if errors.Is(err, errCardThreadGone) {
		// The topic the card lives in was closed or deleted, so a part can't follow it: a non-final card is sent again,
		// with its continuations, the way a card whose message was deleted is.
		if r.Final {
			// A settled card is not sent again: the card message stays as it is (edited to this state, or already showing
			// it when a retry skipped the edit). The part that couldn't be sent is not there (the Mini App has the
			// rest); the parts before it are still right.
			return nil
		}
		c.retire(ctx, cl, pay, chat, r.Key)
		err = c.upsertOnce(ctx, cl, pay, send, linkID, chat, r, spaceID, parts, protect, silent)
	}
	return err
}

// errCardThreadGone: a continuation could not be sent because the card's topic is closed or gone.
var errCardThreadGone = errors.New("notify: the card's topic is gone")

// retire gives up a card whose topic is gone: its continuations and the card are deleted (best effort; a deleted
// topic took them along) and forgotten, rows included, so the card is sent again.
func (c *cards) retire(ctx context.Context, cl *botapi.Client, pay func() error, chat int64, k Key) {
	cd := c.get(chat, k)
	if cd == nil {
		return
	}
	_ = c.deleteConts(ctx, cl, pay, chat, k, 0)
	if pay() == nil {
		_ = cl.DeleteMessage(ctx, chat, cd.messageID)
	}
	c.dropConts(chat, k, 0) // what deleteConts left after a failure: queue its rows' removal before forgetting them
	c.forget(chat, k)
	c.pend(Mapping{ChatID: chat, MessageID: cd.messageID, Key: k, Delete: true})
}

// retract edits a card the link may no longer see (its access, a mute, the space hidden) to card.Gone: no title, no
// body, no keyboard; its continuations (which carry the body) and its last alert are deleted. Nothing is rendered from
// the resource, so it needs no recipient check. A card already retracted, or none, costs nothing.
func (c *cards) retract(ctx context.Context, cl *botapi.Client, chat int64, k Key) error {
	mu := c.chatLock(chat)
	mu.Lock()
	defer mu.Unlock()
	cd := c.get(chat, k)
	if cd == nil || cd.retracted() {
		return nil
	}
	pay := payer(ctx)
	if cd.gone {
		// A retry: the card already says it is gone, but parts that carry the body (or the alert) are left.
		return errors.Join(c.dropAlert(ctx, cl, pay, chat, k), c.deleteConts(ctx, cl, pay, chat, k, 0))
	}
	_ = pay()
	_, err := cl.EditMessageText(ctx, botapi.EditMessageTextParams{ChatID: chat, MessageID: cd.messageID, Text: card.Gone,
		ReplyMarkup: cardPart{}.EditMarkup()})
	switch {
	case err == nil || isNotModified(err):
		c.markGone(chat, k)
		// A failure is retried by the sender: retracted() stays false until the alert and the continuations are gone.
		return errors.Join(c.dropAlert(ctx, cl, pay, chat, k), c.deleteConts(ctx, cl, pay, chat, k, 0))
	case isGone(err):
		_ = c.deleteConts(ctx, cl, pay, chat, k, 0)
		c.dropConts(chat, k, 0)
		c.forget(chat, k)
		c.pend(Mapping{ChatID: chat, MessageID: cd.messageID, Key: k, Delete: true})
		return nil
	}
	return err
}

// upsertOnce is one try of upsert; pay charges each Bot API call after the first.
func (c *cards) upsertOnce(ctx context.Context, cl *botapi.Client, pay func() error, send sendFunc, linkID string, chat int64, r *Resource, spaceID string, parts []cardPart, protect, silent bool) error {
	sig := partSig(parts[0])
	if cd := c.get(chat, r.Key); cd != nil {
		// The chat handler may have put a newer state on the card message (its form after the person's own change), or
		// this very version as a form: that stays until the resource moves on. A settled state always replaces a form.
		if r.Version != 0 && (cd.version > r.Version || cd.version == r.Version && cd.form && !r.Final) {
			return nil
		}
		var err error
		if cd.sig != sig || cd.version != r.Version { // a retry after a failed continuation doesn't edit the card again (edits count against the rate limit)
			_ = pay()
			_, err = cl.EditMessageText(ctx, botapi.EditMessageTextParams{ChatID: chat, MessageID: cd.messageID, Text: parts[0].Text, Entities: parts[0].Entities, ReplyMarkup: parts[0].EditMarkup()})
		}
		switch {
		case err == nil || isNotModified(err):
			c.record(Mapping{ChatID: chat, MessageID: cd.messageID, LinkID: linkID, SpaceID: spaceID, Key: r.Key, Version: r.Version, CreatedAt: c.now(), Thread: cd.thread, ThreadKnown: cd.threadKnown}, r)
			c.setSig(chat, r.Key, sig)
			if r.Final {
				_ = c.dropAlert(ctx, cl, pay, chat, r.Key) // settled: the reminder about it is noise now (best effort)
			}
			return c.syncConts(ctx, cl, pay, send, linkID, chat, r, spaceID, cd.messageID, parts, protect)
		case isGone(err) && !r.Final:
			_ = c.deleteConts(ctx, cl, pay, chat, r.Key, 0) // best effort: the card is sent again, with new continuations
			c.dropConts(chat, r.Key, 0)                     // the rows of any left after a failure
			c.forget(chat, r.Key)                           // the message was deleted: send the card again
		case isGone(err):
			_ = c.deleteConts(ctx, cl, pay, chat, r.Key, 0)
			c.dropConts(chat, r.Key, 0)
			c.forget(chat, r.Key)
			return nil
		default:
			return err
		}
	}
	if err := pay(); err != nil {
		return err
	}
	m, err := send(ctx, cl, botapi.SendMessageParams{ChatID: chat, Text: parts[0].Text, Entities: parts[0].Entities, ReplyMarkup: markupAny(parts[0]), ProtectContent: protect, DisableNotification: silent})
	if err != nil {
		return err
	}
	// The thread the send reported is where the card is, wherever the layout sent it.
	c.record(Mapping{ChatID: chat, MessageID: m.MessageID, LinkID: linkID, SpaceID: spaceID, Key: r.Key, Version: r.Version, CreatedAt: c.now(), Thread: m.MessageThreadID, ThreadKnown: true}, r)
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
		sig := partSig(p)
		if i < len(have) {
			if have[i].sig == sig {
				continue
			}
			if err := pay(); err != nil {
				return err
			}
			_, err := cl.EditMessageText(ctx, botapi.EditMessageTextParams{ChatID: chat, MessageID: have[i].id, Text: p.Text, Entities: p.Entities, ReplyMarkup: p.EditMarkup()})
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
		m, err := c.sendInCardThread(ctx, cl, send, chat, r.Key, botapi.SendMessageParams{ChatID: chat, Text: p.Text, Entities: p.Entities, ReplyMarkup: markupAny(p),
			ReplyParameters: &botapi.ReplyParameters{MessageID: cardID}, ProtectContent: protect, DisableNotification: true})
		if err != nil {
			if botapi.IsTopicGone(err) {
				if cd := c.get(chat, r.Key); cd != nil && cd.threadKnown && cd.thread != 0 {
					return fmt.Errorf("%w: %w", errCardThreadGone, err)
				}
			}
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

// sendInCardThread sends p, a reply to a card, into the thread the card was sent in: straight to the chat with
// MessageThreadID set, never through the layout's routing, which may by now name another topic (or none) and would
// split the card across topics. The caller has paid for the call (sender.Take). A card whose thread is unknown (a row
// from before the thread was recorded) goes through fallback, the current routing. Being direct, it has no recovery of
// its own for a closed or deleted topic: the caller handles botapi.IsTopicGone (syncConts retires the card). It is the
// call for any further message that replies to a card.
func (c *cards) sendInCardThread(ctx context.Context, cl *botapi.Client, fallback sendFunc, chat int64, k Key, p botapi.SendMessageParams) (botapi.Message, error) {
	cd := c.get(chat, k)
	if cd == nil || !cd.threadKnown {
		return fallback(ctx, cl, p)
	}
	p.ChatID, p.MessageThreadID = chat, cd.thread
	return cl.SendMessage(ctx, p)
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

// cardPart is one message of a card (rendered by the card package).
type cardPart = card.Part

// partSig is a signature of what a part shows.
func partSig(p cardPart) uint64 {
	h := fnv.New64a()
	_, _ = fmt.Fprintf(h, "%s\x00%v\x00%v\x00%v", p.Text, p.Entities, p.Keyboard, p.Buttons)
	return h.Sum64() | 1 // never 0, which means unknown
}

// markupAny is a part's keyboard as the interface sendMessage takes (nil stays nil: a new message without buttons).
func markupAny(p cardPart) botapi.ReplyMarkup {
	if m := p.Markup(); m != nil {
		return m
	}
	return nil
}

// state returns, under one lock, the card (nil: none) and whether a send of the card is waiting in the sender. One
// snapshot, so a send finishing between two reads can't make the card look absent and the send gone at once.
func (c *cards) state(chat int64, k Key) (cd *cardState, queued bool) {
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
			c.base[ck] = cardState{remindedSeen: r.RemindedAt, escalatedSeen: r.EscalatedAt, assignee: r.Assignee, known: true}
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

const maxTitle = card.MaxTitle

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// clip cuts s to n runes (with an ellipsis), never inside a rune.
func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}
