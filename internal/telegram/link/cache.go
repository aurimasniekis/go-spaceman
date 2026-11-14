package link

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/eventbus"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/updates"
)

// Cache is the dispatcher's in-memory view of the active links (updates.Links): every update looks its sender up here,
// so a Telegram user's updates never read the database. It follows the in-process eventbus topic telegram.link (plan
// §3.1, §4.2): the key is a link ID to re-read, or "*" to reload them all. The bus only reduces latency (a full queue
// drops events), so it also reloads everything on an interval, and a failed read empties the cache until a read works:
// a revoked link must never keep acting because the database was unreachable.
type Cache struct {
	store    *store.Store
	bus      *eventbus.Bus
	log      *slog.Logger
	interval time.Duration

	mu   sync.RWMutex
	byTG map[int64]updates.Link
	byID map[string]int64 // link ID -> Telegram user ID
	// OnRemove is told when a link leaves the cache (the per-link limiter drops its buckets). Set before Start.
	OnRemove func(linkID string)
}

// NewCache returns an empty cache; Start fills it. interval is the safety reload (zero: one minute).
func NewCache(st *store.Store, bus *eventbus.Bus, log *slog.Logger, interval time.Duration) *Cache {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if interval <= 0 {
		interval = time.Minute
	}
	return &Cache{store: st, bus: bus, log: log, interval: interval, byTG: map[int64]updates.Link{}, byID: map[string]int64{}}
}

var _ updates.Links = (*Cache)(nil)

// Lookup returns the active link of a Telegram user.
func (c *Cache) Lookup(tgUserID int64) (updates.Link, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	l, ok := c.byTG[tgUserID]
	return l, ok
}

// Start subscribes to telegram.link, loads every active link and follows the bus until ctx ends. It subscribes before
// it loads, so a change in between isn't lost. A failed first load leaves the cache empty (every user unlinked) and
// the interval reload retries.
func (c *Cache) Start(ctx context.Context) {
	sub := c.bus.Subscribe(256, eventbus.TopicTelegramLink)
	if err := c.reloadAll(ctx); err != nil {
		c.log.Warn("telegram: loading the links failed; every user is unlinked until it works", slog.String("error", err.Error()))
	}
	go c.run(ctx, sub)
}

func (c *Cache) run(ctx context.Context, sub *eventbus.Subscription) {
	defer sub.Close()
	tick := time.NewTicker(c.interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-sub.C:
			if !ok {
				return
			}
			c.apply(ctx, ev.Key)
		case <-tick.C:
			if err := c.reloadAll(ctx); err != nil {
				c.log.Warn("telegram: reloading the links failed", slog.String("error", err.Error()))
			}
		}
	}
}

func (c *Cache) apply(ctx context.Context, key string) {
	var err error
	if key == "" || key == "*" {
		err = c.reloadAll(ctx)
	} else {
		err = c.reloadOne(ctx, key)
	}
	if err != nil {
		c.log.Warn("telegram: re-reading a link failed", slog.String("error", err.Error()))
	}
}

func fromRow(id, userID string, chat int64, due sql.NullInt64) updates.Link {
	l := updates.Link{ID: id, UserID: userID, ChatID: chat}
	if due.Valid {
		l.ReconfirmDueAt = time.UnixMicro(due.Int64).UTC()
	}
	return l
}

// reloadAll replaces the cache with the database's active links. On failure it empties the cache (fail closed).
func (c *Cache) reloadAll(ctx context.Context) error {
	rows, err := c.store.R().ListActiveTelegramLinks(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	old := c.byID
	c.byTG, c.byID = map[int64]updates.Link{}, map[string]int64{}
	if err != nil {
		c.removed(old, nil)
		return err
	}
	for _, r := range rows {
		c.byTG[r.TgUserID] = fromRow(r.ID, r.UserID, r.ChatID, r.ReconfirmDueAt)
		c.byID[r.ID] = r.TgUserID
	}
	c.removed(old, c.byID)
	return nil
}

// reloadOne re-reads one link: an active one is (re)stored, anything else leaves the cache, and so does a link whose
// read failed.
func (c *Cache) reloadOne(ctx context.Context, id string) error {
	r, err := c.store.R().GetTelegramLink(ctx, id)
	c.mu.Lock()
	defer c.mu.Unlock()
	if old, ok := c.byID[id]; ok {
		delete(c.byTG, old)
		delete(c.byID, id)
	}
	if err != nil {
		c.notifyRemoved(id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	if r.State != "active" {
		c.notifyRemoved(id)
		return nil
	}
	c.byTG[r.TgUserID] = fromRow(r.ID, r.UserID, r.ChatID, r.ReconfirmDueAt)
	c.byID[r.ID] = r.TgUserID
	return nil
}

func (c *Cache) removed(old, now map[string]int64) {
	for id := range old {
		if _, ok := now[id]; !ok {
			c.notifyRemoved(id)
		}
	}
}

func (c *Cache) notifyRemoved(id string) {
	if c.OnRemove != nil {
		c.OnRemove(id)
	}
}
