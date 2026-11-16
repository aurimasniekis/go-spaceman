package notify_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/eventbus"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
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
	// A card is found by its message, and an unknown message is ErrNoCard.
	c, err := m.Card(ctx, 9, 5)
	if err != nil || c.Key != k || c.Version != 4 || c.LinkID != "" {
		t.Fatalf("Card = %+v, %v", c, err)
	}
	if _, err := m.Card(ctx, 9, 6); !errors.Is(err, notify.ErrNoCard) {
		t.Fatalf("Card of an unknown message: %v", err)
	}
	// A continuation message is found with its card's resource, but is never a card.
	if err := m.Save(ctx, []notify.Mapping{{ChatID: 9, MessageID: 6, Key: k, Version: 4, CreatedAt: time.Now(), Cont: true}}); err != nil {
		t.Fatal(err)
	}
	got, err = m.Lookup(ctx, k)
	if err != nil || len(got) != 2 || got[0].Cont || !got[1].Cont || got[1].MessageID != 6 {
		t.Fatalf("Lookup with a continuation = %+v, %v", got, err)
	}
	if _, err := m.Card(ctx, 9, 6); !errors.Is(err, notify.ErrNoCard) {
		t.Fatalf("a continuation is not a card: %v", err)
	}
	// A deleted continuation's row is removed; a card is never removed this way.
	if err := m.Save(ctx, []notify.Mapping{{ChatID: 9, MessageID: 6, Key: k, Cont: true, Delete: true}, {ChatID: 9, MessageID: 5, Key: k, Delete: true}}); err != nil {
		t.Fatal(err)
	}
	if got, err = m.Lookup(ctx, k); err != nil || len(got) != 1 || got[0].MessageID != 5 {
		t.Fatalf("Lookup after deleting the continuation = %+v, %v", got, err)
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

	// usr_a has chosen a zone; usr_b has an invalid one (none); usr_c none.
	exec(`INSERT INTO notification_prefs (user_id, prefs_json, updated_at, version) VALUES ('usr_a', '{"timezone":"Asia/Kolkata"}', 0, 1)`)
	exec(`INSERT INTO notification_prefs (user_id, prefs_json, updated_at, version) VALUES ('usr_b', '{"timezone":"Mars/Base"}', 0, 1)`)

	check := func(_ context.Context, id string) (bool, error) { return id != "usr_c", nil }
	bus := eventbus.New()
	l := notify.NewLinks(st, bus, check, nil, time.Hour)
	changed := make(chan struct{}, 4)
	l.OnChange = func() { changed <- struct{}{} }
	ctx2, cancel := context.WithCancel(ctx)
	defer cancel()
	l.Start(ctx2)
	got := l.Snapshot()
	if len(got) != 1 || got[0].ID != "tgl_a" || got[0].ChatID != 11 {
		t.Fatalf("links = %+v, want only the active link of a user who may act", got)
	}
	if got[0].Zone == nil || got[0].Zone.String() != "Asia/Kolkata" {
		t.Errorf("zone = %v, want the user's Asia/Kolkata", got[0].Zone)
	}
	select {
	case <-changed:
	default:
		t.Error("the first load changes the digest schedule: OnChange expected")
	}
	if p := got[0].Prefs.Effective(notifyDefaults()); p.NewActions {
		t.Error("the link's override (new_actions false) was lost")
	}
}

// A change of the user's zone reaches the links and moves the digest schedule (OnChange), and nothing else does.
func TestLinksZoneChangeKicksTheDigest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st := storetest.Open(t)
	exec := func(q string, args ...any) {
		t.Helper()
		if err := st.W().Exec(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`INSERT INTO users (id, display_name, enabled, provisioned_via, created_at, created_by, updated_at) VALUES ('usr_a', 'a', 1, 'admin', 0, 't', 0)`)
	exec(`INSERT INTO telegram_links (id, user_id, bot_id, tg_user_id, chat_id, state, prefs_json, created_at, updated_at) VALUES ('tgl_a', 'usr_a', 7, 1, 11, 'active', '{}', 0, 0)`)
	bus := eventbus.New()
	l := notify.NewLinks(st, bus, func(context.Context, string) (bool, error) { return true, nil }, nil, time.Hour)
	changed := make(chan struct{}, 8)
	l.OnChange = func() { changed <- struct{}{} }
	l.Start(ctx)
	<-changed
	exec(`INSERT INTO notification_prefs (user_id, prefs_json, updated_at, version) VALUES ('usr_a', '{"timezone":"Europe/Vilnius"}', 0, 1)`)
	bus.Publish(eventbus.Event{Topic: eventbus.TopicUserChanged})
	select {
	case <-changed:
	case <-time.After(5 * time.Second):
		t.Fatal("a new zone did not kick the digest schedule")
	}
	if z := l.Snapshot()[0].Zone; z == nil || z.String() != "Europe/Vilnius" {
		t.Fatalf("zone = %v", z)
	}
	bus.Publish(eventbus.Event{Topic: eventbus.TopicUserChanged}) // nothing changed
	select {
	case <-changed:
		t.Error("an unchanged reload must not kick the deadline loop")
	case <-time.After(300 * time.Millisecond):
	}
}

// A notice row is stored with its kind, is not a card (Lookup and Card don't see it), and NoticeLinks lists its link.
func TestStoreNoticeRows(t *testing.T) {
	ctx := context.Background()
	st := storetest.Open(t)
	if err := st.W().Exec(ctx, `INSERT INTO users (id, display_name, enabled, provisioned_via, created_at, created_by, updated_at) VALUES ('usr_a', 'a', 1, 'admin', 0, 't', 0)`); err != nil {
		t.Fatal(err)
	}
	if err := st.W().Exec(ctx, `INSERT INTO telegram_links (id, user_id, bot_id, tg_user_id, chat_id, state, prefs_json, created_at, updated_at)
		VALUES ('tgl_a', 'usr_a', 7, 1, 11, 'active', '{}', 0, 0)`); err != nil {
		t.Fatal(err)
	}
	m := notify.StoreMappings{S: st}
	k := notify.Key{Type: notify.NoticeThreads, ID: "tgl_a"}
	if err := m.Save(ctx, []notify.Mapping{{ChatID: 11, MessageID: 3, LinkID: "tgl_a", Kind: notify.MappingNotice, Key: k, CreatedAt: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	ids, err := m.NoticeLinks(ctx, notify.NoticeThreads)
	if err != nil || len(ids) != 1 || ids[0] != "tgl_a" {
		t.Fatalf("NoticeLinks = %v, %v", ids, err)
	}
	if ids, _ := m.NoticeLinks(ctx, "other"); len(ids) != 0 {
		t.Errorf("NoticeLinks(other) = %v", ids)
	}
	if got, _ := m.Lookup(ctx, k); len(got) != 0 {
		t.Errorf("a notice is not a card: Lookup = %+v", got)
	}
	if _, err := m.Card(ctx, 11, 3); !errors.Is(err, notify.ErrNoCard) {
		t.Errorf("a notice is not a card: Card error %v", err)
	}
}

// A home summary row is stored with its kind and pin flag, is listed by HomeMessages, is no card, and survives the
// retention prune like a notice.
func TestStoreHomeRows(t *testing.T) {
	ctx := context.Background()
	st := storetest.Open(t)
	if err := st.W().Exec(ctx, `INSERT INTO users (id, display_name, enabled, provisioned_via, created_at, created_by, updated_at) VALUES ('usr_a', 'a', 1, 'admin', 0, 't', 0)`); err != nil {
		t.Fatal(err)
	}
	if err := st.W().Exec(ctx, `INSERT INTO telegram_links (id, user_id, bot_id, tg_user_id, chat_id, state, prefs_json, created_at, updated_at)
		VALUES ('tgl_a', 'usr_a', 7, 1, 11, 'active', '{}', 0, 0)`); err != nil {
		t.Fatal(err)
	}
	m := notify.StoreMappings{S: st}
	k := notify.Key{Type: notify.HomeResource, ID: "tgl_a"}
	if err := m.Save(ctx, []notify.Mapping{{ChatID: 11, MessageID: 5, LinkID: "tgl_a", Kind: notify.MappingHome, Key: k, Version: 1, CreatedAt: time.Unix(1, 0)}}); err != nil {
		t.Fatal(err)
	}
	rows, err := m.HomeMessages(ctx)
	if err != nil || len(rows) != 1 || rows[0] != (notify.HomeRow{LinkID: "tgl_a", ChatID: 11, MessageID: 5, Pinned: true}) {
		t.Fatalf("HomeMessages = %+v, %v", rows, err)
	}
	if got, _ := m.Lookup(ctx, k); len(got) != 0 {
		t.Errorf("a home summary is not a card: Lookup = %+v", got)
	}
	if err := m.Save(ctx, []notify.Mapping{{ChatID: 11, MessageID: 5, Kind: notify.MappingHome, Delete: true}}); err != nil {
		t.Fatal(err)
	}
	if rows, _ := m.HomeMessages(ctx); len(rows) != 0 {
		t.Fatalf("a deleted home row is still listed: %+v", rows)
	}
	if err := m.Save(ctx, []notify.Mapping{{ChatID: 11, MessageID: 5, LinkID: "tgl_a", Kind: notify.MappingHome, Key: k, Version: 1, CreatedAt: time.Unix(1, 0)}}); err != nil {
		t.Fatal(err)
	}
	var pruned int64
	if err := st.W().Tx(ctx, func(tx *store.Tx) (err error) {
		pruned, err = tx.Q.PruneTelegramMessages(ctx, db.PruneTelegramMessagesParams{Cutoff: time.Now().UnixMicro(), Lim: 100})
		return err
	}); err != nil || pruned != 0 {
		t.Errorf("prune removed %d home rows (%v), want 0", pruned, err)
	}
}

// The thread a card was sent in is stored (0 is the main chat, and different from unknown), survives a write that
// doesn't know it, and a row without one reads back as unknown.
func TestStoreCardThread(t *testing.T) {
	ctx := context.Background()
	m := notify.StoreMappings{S: storetest.Open(t)}
	k := notify.Key{Type: notify.TypeHumanAction, ID: "hum_1"}
	now := time.Now()
	if err := m.Save(ctx, []notify.Mapping{
		{ChatID: 9, MessageID: 1, Key: k, Version: 1, CreatedAt: now, Thread: 7, ThreadKnown: true},
		{ChatID: 9, MessageID: 2, Key: k, Version: 1, CreatedAt: now, Thread: 0, ThreadKnown: true},
		{ChatID: 9, MessageID: 3, Key: k, Version: 1, CreatedAt: now},
	}); err != nil {
		t.Fatal(err)
	}
	// A later write without the thread (an older process, a refresh) leaves the stored one alone.
	if err := m.Save(ctx, []notify.Mapping{{ChatID: 9, MessageID: 1, Key: k, Version: 2, CreatedAt: now}}); err != nil {
		t.Fatal(err)
	}
	got, err := m.Lookup(ctx, k)
	if err != nil || len(got) != 3 {
		t.Fatalf("Lookup = %+v, %v", got, err)
	}
	for _, r := range got {
		switch r.MessageID {
		case 1:
			if !r.ThreadKnown || r.Thread != 7 || r.Version != 2 {
				t.Errorf("card 1 = %+v, want thread 7 kept", r)
			}
		case 2:
			if !r.ThreadKnown || r.Thread != 0 {
				t.Errorf("card 2 = %+v, want the main chat, known", r)
			}
		case 3:
			if r.ThreadKnown {
				t.Errorf("card 3 = %+v, want an unknown thread", r)
			}
		}
	}
}
