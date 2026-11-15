package notify_test

import (
	"context"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/eventbus"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/storetest"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/notify"
)

func TestStoreFeedAndMappings(t *testing.T) {
	ctx := context.Background()
	st := storetest.Open(t)

	f := notify.StoreFeed{S: st}
	if n, err := f.Latest(ctx); err != nil || n != 0 {
		t.Fatalf("Latest on an empty feed = %d, %v", n, err)
	}
	rec := changefeed.New(nil)
	if err := st.W().Tx(ctx, func(tx *store.Tx) error {
		for _, id := range []string{"hum_1", "hum_2", "hum_3"} {
			if err := rec.Record(ctx, tx, "", notify.TypeHumanAction, id, changefeed.Created, 1); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	latest, _ := f.Latest(ctx)
	if latest != 3 {
		t.Fatalf("Latest = %d, want 3", latest)
	}
	rows, err := f.After(ctx, 1, 1)
	if err != nil || len(rows) != 1 || rows[0].Seq != 2 || rows[0].ResourceID != "hum_2" {
		t.Fatalf("After(1, 1) = %+v, %v", rows, err)
	}

	m := notify.StoreMappings{S: st}
	k := notify.Key{Type: notify.TypeHumanAction, ID: "hum_1"}
	if err := m.Save(ctx, []notify.Mapping{{ChatID: 9, MessageID: 5, Key: k, Version: 1, CreatedAt: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	// An edit raises the stored version of the same message.
	if err := m.Save(ctx, []notify.Mapping{{ChatID: 9, MessageID: 5, Key: k, Version: 4, CreatedAt: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	got, err := m.Lookup(ctx, k)
	if err != nil || len(got) != 1 || got[0].ChatID != 9 || got[0].MessageID != 5 || got[0].Version != 4 {
		t.Fatalf("Lookup = %+v, %v", got, err)
	}
	if got, _ := m.Lookup(ctx, notify.Key{Type: notify.TypeHumanAction, ID: "hum_2"}); len(got) != 0 {
		t.Fatalf("Lookup of another resource = %+v", got)
	}
}

// TestLinksLoad: only active links of users who may act are notified, with their preferences, and a failing user
// check empties the list (fail closed).
func TestLinksLoad(t *testing.T) {
	ctx := context.Background()
	st := storetest.Open(t)
	exec := func(q string, args ...any) {
		t.Helper()
		if err := st.W().Exec(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	for _, u := range []string{"usr_a", "usr_b", "usr_c"} {
		exec(`INSERT INTO users (id, display_name, enabled, provisioned_via, created_at, created_by, updated_at) VALUES (?, ?, 1, 'admin', 0, 't', 0)`, u, u)
	}
	link := `INSERT INTO telegram_links (id, user_id, bot_id, tg_user_id, chat_id, state, prefs_json, revoked_at, created_at, updated_at)
		VALUES (?, ?, 7, ?, ?, ?, ?, ?, 0, 0)`
	exec(link, "tgl_a", "usr_a", 1, 11, "active", `{"new_actions":false}`, nil)
	exec(link, "tgl_b", "usr_b", 2, 22, "suspended", "{}", nil)
	exec(link, "tgl_c", "usr_c", 3, 33, "active", "{}", nil)

	check := func(_ context.Context, id string) (bool, error) { return id != "usr_c", nil }
	l := notify.NewLinks(st, eventbus.New(), check, nil, time.Hour)
	ctx2, cancel := context.WithCancel(ctx)
	defer cancel()
	l.Start(ctx2)
	got := l.Snapshot()
	if len(got) != 1 || got[0].ID != "tgl_a" || got[0].ChatID != 11 {
		t.Fatalf("links = %+v, want only the active link of a user who may act", got)
	}
	if p := got[0].Prefs.Effective(notifyDefaults()); p.NewActions {
		t.Error("the link's override (new_actions false) was lost")
	}
}
