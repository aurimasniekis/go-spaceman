package topics

import (
	"context"
	"encoding/json"

	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// StoreTopics keeps thread IDs in telegram_links.topics_json (a JSON object of key → thread ID). Writing it changes no
// change-feed entry and does not bump the link's version: the column is the bot's own bookkeeping.
type StoreTopics struct{ S *store.Store }

// Load reads a link's thread IDs; malformed content reads as none.
func (t StoreTopics) Load(ctx context.Context, linkID string) (map[string]int, error) {
	row, err := t.S.R().GetTelegramLink(ctx, linkID)
	if err != nil {
		return nil, err
	}
	out := map[string]int{}
	if err := json.Unmarshal([]byte(row.TopicsJson), &out); err != nil {
		clear(out) // malformed content reads as none
	}
	return out, nil
}

// Save replaces a link's thread IDs.
func (t StoreTopics) Save(ctx context.Context, linkID string, threads map[string]int) error {
	b, err := json.Marshal(threads)
	if err != nil {
		return err
	}
	return t.S.W().TxNamed(ctx, "telegram.topics", func(tx *store.Tx) error {
		_, err := tx.Q.SetTelegramLinkTopics(ctx, db.SetTelegramLinkTopicsParams{TopicsJson: string(b), ID: linkID})
		return err
	})
}

// Threads returns the thread of key in every live link that has one: one read of the links, not one per link.
func (t StoreTopics) Threads(ctx context.Context, key string) ([]LinkThread, error) {
	rows, err := t.S.R().ListLiveTelegramLinks(ctx)
	if err != nil {
		return nil, err
	}
	var out []LinkThread
	for _, r := range rows {
		m := map[string]int{}
		if err := json.Unmarshal([]byte(r.TopicsJson), &m); err != nil {
			continue // malformed content reads as none
		}
		if id := m[key]; id != 0 {
			out = append(out, LinkThread{LinkID: r.ID, ChatID: r.ChatID, ID: id})
		}
	}
	return out, nil
}
