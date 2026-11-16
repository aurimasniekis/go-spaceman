package notify

import (
	"context"
	"fmt"
	"hash/fnv"
	"log/slog"
	"sort"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/sender"
)

// The home summary (ADR-0089, plan §2.5): one message per link in home, pinned once and then only edited, at most
// once per HomeEvery. It lists the spaces with human actions waiting and how many, only the spaces this link may read
// (the same check as a card: link usable, SpaceRead and HumanRead held, space visible to Telegram, not muted), and is
// checked again when it is sent, so a queued summary never shows what the link lost access to meanwhile.
//
// Its row in telegram_messages (kind home, resource_type "home", resource_id the link) is what lets it be edited after
// a restart without being sent and pinned a second time; resource_version 1 on it says the pin worked.

// MappingHome is the kind of a link's home summary row.
const MappingHome = "home"

// HomeResource is the resource type of that row; the resource ID is the link's.
const HomeResource = "home"

// DefaultHomeEvery is the least time between two edits of a link's summary (plan Q5).
const DefaultHomeEvery = 30 * time.Second

const (
	homeMaxLines = 10
	homeMaxName  = 40
)

// SpaceCount is the number of pending human actions of a space.
type SpaceCount struct {
	SpaceID string
	Name    string
	N       int64
}

// HomeRow is a stored home summary.
type HomeRow struct {
	LinkID    string
	ChatID    int64
	MessageID int
	Pinned    bool
}

type homeLink struct {
	chat   int64
	msg    int  // 0: none sent yet
	pinned bool // the pin worked
	sig    uint64
	last   time.Time // of the last send or edit
	queued bool
}

// home is the summaries' state, in memory: the row of each link, read once at start.
type home struct {
	mu     sync.Mutex
	loaded bool
	links  map[string]*homeLink
	dirty  bool // a human action changed since the counts were last read
	counts []SpaceCount
}

func newHome() *home { return &home{links: map[string]*homeLink{}} }

// loadHome reads the stored summaries once; until that worked nothing is sent or pinned, so a failed read never leads
// to a second summary. A failed read is tried again by the Run loop's retry timer.
func (n *Notifier) loadHome(ctx context.Context) {
	h := n.home
	h.mu.Lock()
	done := h.loaded
	h.mu.Unlock()
	if done || n.o.Store == nil {
		return
	}
	rows, err := n.o.Store.HomeMessages(ctx)
	if err != nil {
		n.log.Warn("telegram notifier: reading the home summaries failed", slog.String("error", err.Error()))
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range rows {
		h.links[r.LinkID] = &homeLink{chat: r.ChatID, msg: r.MessageID, pinned: r.Pinned}
	}
	h.loaded = true
	h.dirty = true // what changed while the bot was down shows on the first window
}

// homeTouch notes that a human action changed: the summaries are rebuilt by the next homeTick.
func (n *Notifier) homeTouch() {
	if n.o.Pending == nil {
		return
	}
	n.home.mu.Lock()
	n.home.dirty = true
	n.home.mu.Unlock()
}

// homeTick rebuilds the summaries if a human action changed, and queues an edit (or the first send) for each link that
// is due. It returns how long to wait before it must run again (0: nothing waits), for links whose last edit is less
// than HomeEvery old.
func (n *Notifier) homeTick(ctx context.Context) time.Duration {
	h := n.home
	if n.o.Pending == nil {
		return 0
	}
	h.mu.Lock()
	if !h.loaded && n.o.Store != nil || !h.dirty {
		h.mu.Unlock()
		return 0
	}
	h.mu.Unlock()
	snd := n.o.Sender()
	if snd == nil {
		return n.homeEvery() // the bot is off: look again later
	}
	// The flag is cleared before the read, so a change that lands during it marks the summaries dirty again.
	h.mu.Lock()
	h.dirty = false
	h.mu.Unlock()
	counts, err := n.o.Pending(ctx) // one read for every link
	if err != nil {
		n.log.Warn("telegram notifier: reading the pending counts failed", slog.String("error", err.Error()))
		h.mu.Lock()
		h.dirty = true
		h.mu.Unlock()
		return n.homeEvery() // try again
	}
	every, now := n.homeEvery(), n.o.Now()
	var wait time.Duration
	links := n.o.Links.Snapshot()
	h.mu.Lock()
	h.counts = counts
	h.mu.Unlock()
	for i := range links {
		l := &links[i]
		if l.ChatID <= 0 || !l.Usable(now) {
			continue
		}
		h.mu.Lock()
		hl := h.links[l.ID]
		if hl == nil {
			hl = &homeLink{}
			h.links[l.ID] = hl
		}
		if hl.chat != l.ChatID { // the link moved to another chat: that chat has no summary
			if hl.msg != 0 {
				n.cards.pend(Mapping{ChatID: hl.chat, MessageID: hl.msg, Kind: MappingHome, Delete: true})
			}
			hl.chat, hl.msg, hl.pinned, hl.sig = l.ChatID, 0, false, 0
		}
		due := hl.msg == 0 || hl.last.IsZero() || now.Sub(hl.last) >= every
		if !due {
			if d := every - now.Sub(hl.last); wait == 0 || d < wait {
				wait = d
			}
			h.dirty = true // rebuilt when the wait is over
		}
		skip := !due || hl.queued
		if hl.queued {
			h.dirty = true // the send in flight may have read older counts
			if wait == 0 || wait > time.Second {
				wait = time.Second
			}
		}
		if !skip {
			hl.queued = true
		}
		h.mu.Unlock()
		if !skip {
			n.enqueueHome(snd, l.ID, l.ChatID)
		}
	}
	return wait
}

func (n *Notifier) homeEvery() time.Duration {
	if n.o.HomeEvery > 0 {
		return n.o.HomeEvery
	}
	return DefaultHomeEvery
}

// homeView is what a link's summary shows now: the text, its entities, its buttons and how many actions wait.
func (n *Notifier) homeView(ctx context.Context, l *Link, counts []SpaceCount) (text string, ents []botapi.MessageEntity, mk *botapi.InlineKeyboardMarkup, total int64) {
	set := n.o.Policy.Current()
	now := n.o.Now()
	subj := l.Subjects(set.LinkSSOGrants, now)
	var vis []SpaceCount
	for _, c := range counts {
		if c.N <= 0 || !n.o.Policy.Space(c.SpaceID).Visible() || l.Prefs.IsMuted(c.SpaceID) ||
			!n.o.Authz.AllowedAll(subj, c.SpaceID, authz.SpaceRead) || !n.o.Authz.AllowedAll(subj, c.SpaceID, authz.HumanRead) {
			continue
		}
		vis = append(vis, c)
		total += c.N
	}
	sort.SliceStable(vis, func(i, j int) bool {
		if vis[i].N != vis[j].N {
			return vis[i].N > vis[j].N
		}
		return vis[i].Name < vis[j].Name
	})
	if total == 0 {
		text = "Needs you\nNothing is waiting for you."
	} else {
		text = fmt.Sprintf("Needs you — %d", total)
	}
	off := len(utf16.Encode([]rune(text)))
	for i, c := range vis {
		if i == homeMaxLines {
			text += fmt.Sprintf("\n… and %d more spaces", len(vis)-i)
			break
		}
		name := clip(oneLine(c.Name), homeMaxName)
		if name == "" {
			name = c.SpaceID
		}
		line := fmt.Sprintf("\n• %s — %d", name, c.N)
		off += len(utf16.Encode([]rune(line)))
		text += line
		if u := n.spaceLink(ctx, l, c.SpaceID); u != "" {
			text += " ▸"
			ents = append(ents, botapi.MessageEntity{Type: "text_link", Offset: off + 1, Length: 1, URL: u})
			off += 2
		}
	}
	var webApp, direct string
	if n.o.NeedsURL != nil {
		webApp = n.o.NeedsURL()
	}
	if n.o.NeedsLink != nil {
		direct = n.o.NeedsLink()
	}
	switch {
	case webApp != "":
		mk = &botapi.InlineKeyboardMarkup{InlineKeyboard: [][]botapi.InlineKeyboardButton{{{Text: "Open Needs you", WebApp: &botapi.WebAppInfo{URL: webApp}}}}}
	case direct != "":
		// No web_app button (no Mini App host here): the direct link (startapp=needs) opens the same page.
		mk = &botapi.InlineKeyboardMarkup{InlineKeyboard: [][]botapi.InlineKeyboardButton{{{Text: "Open Needs you", URL: direct}}}}
	}
	return text, ents, mk, total
}

// spaceLink is where a space's line goes: its topic in the link's chat when it has one, else the Mini App's
// space-<id> link ("": no link).
func (n *Notifier) spaceLink(ctx context.Context, l *Link, spaceID string) string {
	if t, ok := n.o.Topics.ThreadOf(ctx, l.ID, spaceID); ok && n.o.BotUsername != nil {
		if bot := n.o.BotUsername(); bot != "" {
			return fmt.Sprintf("https://t.me/%s/%d", bot, t)
		}
	}
	if n.o.SpaceLink != nil {
		return n.o.SpaceLink(spaceID)
	}
	return ""
}

func homeSig(text string, ents []botapi.MessageEntity, mk *botapi.InlineKeyboardMarkup) uint64 {
	h := fnv.New64a()
	_, _ = fmt.Fprintf(h, "%s\x00%v", text, ents)
	if mk != nil {
		for _, row := range mk.InlineKeyboard {
			for _, b := range row {
				app := ""
				if b.WebApp != nil {
					app = b.WebApp.URL // by value: a pointer would print as an address
				}
				_, _ = fmt.Fprintf(h, "\x00%s\x00%s\x00%s", b.Text, b.URL, app)
			}
		}
	}
	return h.Sum64() | 1
}

func (n *Notifier) homeRow(linkID string, chat int64, msg int, pinned bool) {
	v := int64(0)
	if pinned {
		v = 1
	}
	n.cards.pend(Mapping{ChatID: chat, MessageID: msg, LinkID: linkID, Key: Key{Type: HomeResource, ID: linkID}, Version: v,
		CreatedAt: n.o.Now(), Kind: MappingHome})
}

func (n *Notifier) enqueueHome(snd *sender.Sender, linkID string, chat int64) {
	h := n.home
	release := func() {
		h.mu.Lock()
		if hl := h.links[linkID]; hl != nil {
			hl.queued = false
		}
		h.mu.Unlock()
	}
	err := snd.Enqueue(sender.Message{ChatID: chat, Priority: sender.Edit, Key: "home", Done: func(error) { release() },
		Send: func(ctx context.Context, c *botapi.Client) error { return n.sendHome(ctx, c, linkID, chat) }})
	if err != nil {
		release()
		n.log.Debug("telegram notifier: home summary refused", slog.String("error", err.Error()))
	}
}

// sendHome sends the summary the first time (and pins it), or edits it. It runs when the sender gets to it: the link
// and what it may read are checked then, from memory.
func (n *Notifier) sendHome(ctx context.Context, c *botapi.Client, linkID string, chat int64) error {
	h := n.home
	cur := n.currentLink(linkID, chat)
	if cur == nil || !cur.Usable(n.o.Now()) {
		return nil
	}
	h.mu.Lock()
	counts := h.counts
	hl := h.links[linkID]
	if hl == nil || hl.chat != chat {
		h.mu.Unlock()
		return nil
	}
	msg, pinned, sig := hl.msg, hl.pinned, hl.sig
	h.mu.Unlock()

	text, ents, mk, total := n.homeView(ctx, cur, counts)
	nsig := homeSig(text, ents, mk)
	if msg == 0 && total == 0 {
		return nil // nothing to say yet
	}
	if msg != 0 && nsig == sig && pinned {
		return nil
	}
	if msg != 0 && nsig != sig {
		_, err := c.EditMessageText(ctx, botapi.EditMessageTextParams{ChatID: chat, MessageID: msg, Text: text, Entities: ents,
			LinkPreviewOptions: &botapi.LinkPreviewOptions{IsDisabled: true}, ReplyMarkup: mk})
		switch {
		case err == nil || isNotModified(err):
		case isGone(err):
			// The person deleted it: it is sent and pinned again.
			n.cards.pend(Mapping{ChatID: chat, MessageID: msg, Kind: MappingHome, Delete: true})
			msg, pinned = 0, false
		default:
			return err
		}
	}
	if msg == 0 {
		p := botapi.SendMessageParams{ChatID: chat, Text: text, Entities: ents, DisableNotification: true,
			LinkPreviewOptions: &botapi.LinkPreviewOptions{IsDisabled: true}}
		if mk != nil {
			p.ReplyMarkup = mk
		}
		m, err := n.route(linkID, "")(ctx, c, p)
		if err != nil {
			return err
		}
		msg, pinned = m.MessageID, false
		n.homeRow(linkID, chat, msg, false)
	}
	h.mu.Lock()
	if hl.chat != chat { // the link moved while this was sent: that chat's state is not this message's
		h.mu.Unlock()
		return nil
	}
	hl.msg, hl.sig, hl.last = msg, nsig, n.o.Now()
	h.mu.Unlock()
	if !pinned {
		if err := c.PinChatMessage(ctx, botapi.PinChatMessageParams{ChatID: chat, MessageID: msg, DisableNotification: true}); err != nil {
			n.log.Debug("telegram notifier: pinning the home summary failed", slog.String("error", err.Error()))
			return nil // the summary is there; the pin is tried again with the next edit
		}
		h.mu.Lock()
		hl.pinned = true
		h.mu.Unlock()
		n.homeRow(linkID, chat, msg, true)
	}
	return nil
}
