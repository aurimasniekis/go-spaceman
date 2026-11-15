package notify

import (
	"context"
	"log/slog"
	"sync"

	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/sender"
)

// Kinds of telegram_messages rows the notifier writes.
const (
	// MappingCard is a resource's card.
	MappingCard = "card"
	// MappingNotice is a one-time notice to a link; its key says which (NoticeThreads).
	MappingNotice = "notice"
	// MappingCont is the rest of a long card body, sent as a reply to the card.
	MappingCont = "cont"
)

// NoticeThreads is the resource type of the one-time announcement of the topic-per-space layout (ADR-0089); the
// resource ID is the link's.
const NoticeThreads = "threads"

// ThreadsNotice is the announcement, sent home once per link when its first message lands in a space topic.
const ThreadsNotice = "Messages about each space now arrive in that space's own topic. The daily digest and other " +
	"messages about several spaces stay here. To keep everything in one chat, set Chat layout to \"One chat\" in the Mini App settings."

// notices remembers which links were announced the topic layout. It is read once from telegram_messages when the
// notifier starts (one query, not one per recipient) and kept in memory; until that read worked nothing is announced,
// so a link never gets the announcement twice because the database was unreachable.
type notices struct {
	mu     sync.Mutex
	loaded bool
	sent   map[string]bool // link IDs announced, or with an announcement on its way
}

// loadNotices reads the links already announced, once; a failed read is tried again by the Run loop's retry timer.
func (n *Notifier) loadNotices(ctx context.Context) {
	n.notices.mu.Lock()
	done := n.notices.loaded
	n.notices.mu.Unlock()
	if done || n.o.Store == nil {
		return
	}
	ids, err := n.o.Store.NoticeLinks(ctx, NoticeThreads)
	if err != nil {
		n.log.Warn("telegram notifier: reading the sent notices failed", slog.String("error", err.Error()))
		return
	}
	n.notices.mu.Lock()
	defer n.notices.mu.Unlock()
	for _, id := range ids {
		n.notices.sent[id] = true
	}
	n.notices.loaded = true
}

// announceThreads queues the link's one-time announcement of the topic layout, home. The row it leaves in
// telegram_messages (kind notice) keeps it from being sent again after a restart; a send that fails for good lets a
// later topic message try again.
func (n *Notifier) announceThreads(linkID string, chat int64) {
	nt := n.notices
	nt.mu.Lock()
	if !nt.loaded || nt.sent[linkID] {
		nt.mu.Unlock()
		return
	}
	nt.sent[linkID] = true
	nt.mu.Unlock()
	snd := n.o.Sender()
	unmark := func() {
		nt.mu.Lock()
		delete(nt.sent, linkID)
		nt.mu.Unlock()
	}
	if snd == nil {
		unmark()
		return
	}
	err := snd.Enqueue(sender.Message{ChatID: chat, Priority: sender.Notification, Key: "notice:" + NoticeThreads,
		Done: func(err error) {
			if err != nil {
				unmark()
			}
		},
		Send: func(ctx context.Context, c *botapi.Client) error {
			if n.currentLink(linkID, chat) == nil {
				return nil // the link is gone or moved: nobody to tell
			}
			m, err := n.route(linkID, "")(ctx, c, botapi.SendMessageParams{ChatID: chat, Text: ThreadsNotice, DisableNotification: true})
			if err != nil {
				return err
			}
			n.cards.pend(Mapping{ChatID: chat, MessageID: m.MessageID, LinkID: linkID, Kind: MappingNotice,
				Key: Key{Type: NoticeThreads, ID: linkID}, CreatedAt: n.o.Now()})
			return nil
		}})
	if err != nil {
		unmark()
		n.log.Debug("telegram notifier: notice refused", slog.String("error", err.Error()))
	}
}
