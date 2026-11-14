package settings

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/eventbus"
	"github.com/aurimasniekis/go-spaceman/internal/keyring/keyringtest"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/storetest"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/sender"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/tgtest"
)

var admin = audit.Actor{Type: audit.ActorUser, ID: "usr_admin"}

func newSvc(t *testing.T, bus *eventbus.Bus) (*Service, *store.Store) {
	t.Helper()
	s := storetest.Open(t)
	kr := keyringtest.Load(t, s)
	ctx := context.Background()
	if err := s.W().TxNamed(ctx, "seed", func(tx *store.Tx) error {
		for _, q := range []string{
			`INSERT INTO nodes (id, name, state, created_at, created_by, updated_at) VALUES ('nod_1', 'n1', 'active', 0, 't', 0)`,
			`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('spc_a', 'aaa', 'A', 'nod_1', 'active', 'a', 0, 't', 0)`,
			`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('spc_b', 'bbb', 'B', 'nod_1', 'active', 'b', 0, 't', 0)`,
		} {
			if _, err := tx.SQL.ExecContext(ctx, q); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return New(s, audit.NewWriter(kr), bus, nil), s
}

func code(t *testing.T, err error, want apperr.Code) {
	t.Helper()
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code != want {
		t.Fatalf("error %v, want %s", err, want)
	}
}

func count(t *testing.T, s *store.Store, q string, args ...any) int {
	t.Helper()
	var n int
	if err := s.ReadDB().QueryRowContext(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestGlobalSettingsLifecycle(t *testing.T) {
	t.Parallel()
	svc, s := newSvc(t, nil)
	ctx := context.Background()
	out, err := svc.Get(ctx)
	if err != nil || out.Stored || out.Version != 0 || out.Settings != Defaults() {
		t.Fatalf("before any write: %+v %v", out, err)
	}
	// If-Match on a missing row; out-of-range values refused before any write.
	_, err = svc.Put(ctx, admin, 1, Input{})
	code(t, err, apperr.CodeNotFound)
	_, err = svc.Put(ctx, admin, 0, Input{LinksPerUser: ptr[int64](4)})
	code(t, err, apperr.CodeInvalid)
	_, err = svc.Put(ctx, admin, 0, Input{MiniAppSessionIdleSeconds: ptr[int64](3600), MiniAppSessionAbsoluteSeconds: ptr[int64](900)})
	code(t, err, apperr.CodeInvalid)
	if n := count(t, s, `SELECT count(*) FROM telegram_settings`); n != 0 {
		t.Fatalf("a refused write stored a row (%d)", n)
	}

	out, err = svc.Put(ctx, admin, 0, Input{LinksPerUser: ptr[int64](2), InboundLimits: &InboundLimitsInput{UpdatesPerSecond: ptr[int64](5)}})
	if err != nil || !out.Stored || out.Version != 1 || out.LinksPerUser != 2 || out.InboundLimits.UpdatesPerSecond != 5 ||
		out.InboundLimits.ActionsPerMinute != 30 || out.UpdatedBy != "user:usr_admin" {
		t.Fatalf("created %+v %v", out, err)
	}
	var raw string
	if err := s.ReadDB().QueryRowContext(ctx, `SELECT settings_json FROM telegram_settings`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw != `{"links_per_user":2,"inbound_limits":{"updates_per_second":5}}` {
		t.Fatalf("stored %s: only the fields sent are stored", raw)
	}
	if svc.Current() != out.Settings {
		t.Fatal("the cache didn't follow the write")
	}
	_, err = svc.Put(ctx, admin, 0, Input{})
	code(t, err, apperr.CodeConflict) // exists: replace with If-Match
	_, err = svc.Put(ctx, admin, 7, Input{})
	code(t, err, apperr.CodeConflict) // stale

	// A replacement replaces: what isn't sent goes back to its default.
	out, err = svc.Put(ctx, admin, 1, Input{TopicsModeDefault: ptr(true)})
	if err != nil || out.Version != 2 || out.LinksPerUser != 1 || !out.TopicsModeDefault {
		t.Fatalf("replaced %+v %v", out, err)
	}
	code(t, svc.Delete(ctx, admin, 1), apperr.CodeConflict)
	if err := svc.Delete(ctx, admin, 2); err != nil {
		t.Fatal(err)
	}
	code(t, svc.Delete(ctx, admin, 2), apperr.CodeNotFound)
	if out, _ := svc.Get(ctx); out.Stored || out.Settings != Defaults() || svc.Current() != Defaults() {
		t.Fatalf("after reset %+v", out)
	}
	for action, want := range map[string]int{"telegram.settings.created": 1, "telegram.settings.updated": 1, "telegram.settings.deleted": 1} {
		if n := count(t, s, `SELECT count(*) FROM audit_events WHERE action = ? AND actor_id = 'usr_admin'`, action); n != want {
			t.Errorf("%s audited %d times", action, n)
		}
	}
}

// A stored row that no longer validates (bounds tightened later, or a hand edit) falls back to the defaults instead
// of stopping Telegram.
func TestOutOfBoundsRowFallsBack(t *testing.T) {
	t.Parallel()
	svc, s := newSvc(t, nil)
	ctx := context.Background()
	for _, raw := range []string{`{"links_per_user":9}`, `{"links_per_user":"two"}`} {
		if err := s.W().TxNamed(ctx, "seed", func(tx *store.Tx) error {
			_, err := tx.SQL.ExecContext(ctx, `INSERT OR REPLACE INTO telegram_settings (singleton, settings_json, updated_at, updated_by) VALUES (1, ?, 0, 't')`, raw)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if err := svc.Load(ctx); err != nil {
			t.Fatal(err)
		}
		if svc.Current() != Defaults() {
			t.Fatalf("%s: %+v", raw, svc.Current())
		}
	}
}

func TestSpaceSettingsLifecycle(t *testing.T) {
	// Not parallel: a cascade delete holds the write lock for 100+ ms (HP-DB-004), so only one runs at a time.
	svc, s := newSvc(t, nil)
	ctx := context.Background()
	sp, err := svc.GetSpace(ctx, "spc_a")
	if err != nil || sp.Stored || sp.Version != 0 || sp.Enabled || sp.ContentLevel != ContentFull {
		t.Fatalf("defaults %+v %v", sp, err)
	}
	if svc.Space("spc_a").Visible() || len(svc.VisibleSpaces()) != 0 {
		t.Fatal("a space without settings is visible")
	}
	_, err = svc.GetSpace(ctx, "spc_missing")
	code(t, err, apperr.CodeNotFound)
	_, err = svc.PutSpace(ctx, admin, "spc_missing", 0, SpaceInput{})
	code(t, err, apperr.CodeNotFound)
	_, err = svc.PutSpace(ctx, admin, "spc_a", 1, SpaceInput{})
	code(t, err, apperr.CodeNotFound)
	_, err = svc.PutSpace(ctx, admin, "spc_a", 0, SpaceInput{ContentLevel: ptr("everything")})
	code(t, err, apperr.CodeInvalid)

	sp, err = svc.PutSpace(ctx, admin, "spc_a", 0, SpaceInput{Enabled: ptr(true), TerminalWatch: ptr(true)})
	if err != nil || !sp.Stored || sp.Version != 1 || !sp.Enabled || !sp.TerminalWatch || sp.ContentLevel != ContentFull ||
		sp.EnforcedApprovals != ApprovalsWebOnly || sp.CreatedBy != "user:usr_admin" {
		t.Fatalf("created %+v %v", sp, err)
	}
	if got := svc.Space("spc_a"); !got.Visible() || !got.TerminalWatch {
		t.Fatalf("cache %+v", got)
	}
	_, err = svc.PutSpace(ctx, admin, "spc_a", 0, SpaceInput{})
	code(t, err, apperr.CodeConflict)
	_, err = svc.PutSpace(ctx, admin, "spc_a", 3, SpaceInput{})
	code(t, err, apperr.CodeConflict)
	sp, err = svc.PutSpace(ctx, admin, "spc_a", 1, SpaceInput{Enabled: ptr(true), ContentLevel: ptr(ContentSummary), EnforcedApprovals: ptr(ApprovalsConfirm),
		Topics: ptr(true)})
	if err != nil || sp.Version != 2 || sp.TerminalWatch || !sp.Topics || sp.ContentLevel != ContentSummary || sp.EnforcedApprovals != ApprovalsConfirm {
		t.Fatalf("replaced %+v %v", sp, err)
	}
	if _, err := svc.PutSpace(ctx, admin, "spc_b", 0, SpaceInput{}); err != nil {
		t.Fatal(err)
	}
	list, err := svc.ListSpaces(ctx)
	if err != nil || len(list) != 2 || list[0].SpaceID != "spc_a" || list[1].SpaceID != "spc_b" {
		t.Fatalf("list %+v %v", list, err)
	}
	if v := svc.VisibleSpaces(); len(v) != 1 || v[0] != "spc_a" {
		t.Fatalf("visible %v (spc_b is stored but not enabled)", v)
	}
	code(t, svc.DeleteSpace(ctx, admin, "spc_a", 1), apperr.CodeConflict)
	if err := svc.DeleteSpace(ctx, admin, "spc_a", 2); err != nil {
		t.Fatal(err)
	}
	code(t, svc.DeleteSpace(ctx, admin, "spc_a", 2), apperr.CodeNotFound)
	if svc.Space("spc_a").Visible() {
		t.Fatal("a deleted space stayed visible")
	}
	for action, want := range map[string]int{"telegram.space.created": 2, "telegram.space.updated": 1, "telegram.space.deleted": 1} {
		if n := count(t, s, `SELECT count(*) FROM audit_events WHERE action = ?`, action); n != want {
			t.Errorf("%s audited %d times", action, n)
		}
	}
	// A purged space takes its settings with it.
	if _, err := svc.PutSpace(ctx, admin, "spc_a", 0, SpaceInput{Enabled: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	if err := s.W().TxNamed(ctx, "purge", func(tx *store.Tx) error {
		_, err := tx.SQL.ExecContext(ctx, `DELETE FROM spaces WHERE id = 'spc_a'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if n := count(t, s, `SELECT count(*) FROM space_telegram_settings WHERE space_id = 'spc_a'`); n != 0 {
		t.Fatal("space purge left its Telegram settings")
	}
	// The purge publishes (fleet does), the cache reloads, and the purged space is gone from it.
	if err := svc.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if svc.Space("spc_a").Visible() || slices.Contains(svc.VisibleSpaces(), "spc_a") {
		t.Fatal("a purged space stayed visible")
	}
	// An archived space with Telegram enabled is not visible either; restoring it shows it again.
	if _, err := svc.PutSpace(ctx, admin, "spc_b", 1, SpaceInput{Enabled: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	if !svc.Space("spc_b").Visible() {
		t.Fatal("an enabled, active space is not visible")
	}
	setState := func(state string) {
		t.Helper()
		if err := s.W().TxNamed(ctx, "state", func(tx *store.Tx) error {
			_, err := tx.SQL.ExecContext(ctx, `UPDATE spaces SET state = ? WHERE id = 'spc_b'`, state)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if err := svc.Load(ctx); err != nil {
			t.Fatal(err)
		}
	}
	setState("archived")
	if svc.Space("spc_b").Visible() || len(svc.VisibleSpaces()) != 0 {
		t.Fatal("an archived space stayed visible")
	}
	setState("active")
	if !svc.Space("spc_b").Visible() {
		t.Fatal("a restored space stayed hidden")
	}
	// No change-feed rows: Telegram state never reaches watch streams.
	if n := count(t, s, `SELECT count(*) FROM changes WHERE resource_type LIKE '%telegram%'`); n != 0 {
		t.Fatalf("%d change-feed rows", n)
	}
}

type recordingSetter struct {
	mu  sync.Mutex
	got []sender.Limits
	err error
}

func (r *recordingSetter) SetLimits(l sender.Limits) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, l)
	return r.err
}

func (r *recordingSetter) last() (sender.Limits, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.got) == 0 {
		return sender.Limits{}, 0
	}
	return r.got[len(r.got)-1], len(r.got)
}

// A change committed anywhere reaches the bound sender through the event bus, without a restart: a second service on
// the same database (as if another writer committed) only learns of it from the bus event.
func TestChangeReachesSenderOverTheBus(t *testing.T) {
	t.Parallel()
	bus := eventbus.New()
	svc, s := newSvc(t, bus)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reader := New(s, nil, bus, nil)
	done := make(chan struct{})
	go func() { defer close(done); reader.Run(ctx) }()
	defer func() { cancel(); <-done }()

	rec := &recordingSetter{}
	reader.BindSender(rec)
	if l, n := rec.last(); n != 1 || l != sender.DefaultLimits() {
		t.Fatalf("bound with %+v (%d calls), want the defaults at once", l, n)
	}
	if _, err := svc.Put(ctx, admin, 0, Input{SendLimits: &SendLimitsInput{GlobalPerSecond: ptr(10.0), QueuePerChat: ptr[int64](20)}}); err != nil {
		t.Fatal(err)
	}
	want := sender.DefaultLimits()
	want.GlobalPerSecond, want.QueuePerChat = 10, 20
	waitFor(t, func() bool { l, _ := rec.last(); return l == want })
	if reader.Current().SendLimits.GlobalPerSecond != 10 {
		t.Fatal("the reader's cache didn't follow")
	}
	// A space change reloads too, without calling the sender again (its limits didn't change).
	_, n := rec.last()
	if _, err := svc.PutSpace(ctx, admin, "spc_b", 0, SpaceInput{Enabled: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return reader.Space("spc_b").Visible() })
	if _, n2 := rec.last(); n2 != n {
		t.Fatalf("a space change re-applied the send limits (%d → %d calls)", n, n2)
	}
	// A refused SetLimits only logs; the binding stays.
	rec.mu.Lock()
	rec.err = errors.New("refused")
	rec.mu.Unlock()
	if _, err := svc.Put(ctx, admin, 1, Input{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { l, _ := rec.last(); return l == sender.DefaultLimits() })
}

// The real sender accepts every limit set the settings accept, through BindSender.
func TestBindRealSender(t *testing.T) {
	t.Parallel()
	svc, _ := newSvc(t, nil)
	snd, err := sender.New(sender.Options{Client: testClient(t)})
	if err != nil {
		t.Fatal(err)
	}
	svc.BindSender(snd)
	ctx := context.Background()
	if _, err := svc.Put(ctx, admin, 0, Input{SendLimits: &SendLimitsInput{GlobalPerSecond: ptr(30.0), ChatPerSecond: ptr(0.5), ChatBurst: ptr[int64](1),
		GroupPerMinute: ptr[int64](1), QueueTotal: ptr[int64](1), QueuePerChat: ptr[int64](1)}}); err != nil {
		t.Fatal(err)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func testClient(t *testing.T) *botapi.Client {
	t.Helper()
	fake := tgtest.New(t, tgtest.Options{})
	c, err := botapi.New(botapi.Options{BaseURL: fake.URL(), Token: fake.Token()})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// A purge or archive publishes on the bus; the running service reloads and drops the space without any other write.
func TestArchiveAndPurgeEventsDropTheSpace(t *testing.T) {
	// Not parallel: a cascade delete holds the write lock for 100+ ms (HP-DB-004), so only one runs at a time.
	bus := eventbus.New()
	svc, s := newSvc(t, bus)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); svc.Run(ctx) }()
	defer func() { cancel(); <-done }()
	if _, err := svc.PutSpace(ctx, admin, "spc_a", 0, SpaceInput{Enabled: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	exec := func(q string) {
		t.Helper()
		if err := s.W().TxNamed(ctx, "x", func(tx *store.Tx) error { _, err := tx.SQL.ExecContext(ctx, q); return err }); err != nil {
			t.Fatal(err)
		}
		bus.Publish(eventbus.Event{Topic: eventbus.TopicTelegramSettings, Key: "spc_a"})
	}
	exec(`UPDATE spaces SET state = 'archived' WHERE id = 'spc_a'`)
	waitFor(t, func() bool { return !svc.Space("spc_a").Visible() })
	exec(`UPDATE spaces SET state = 'active' WHERE id = 'spc_a'`)
	waitFor(t, func() bool { return svc.Space("spc_a").Visible() })
	exec(`DELETE FROM spaces WHERE id = 'spc_a'`)
	waitFor(t, func() bool { return !svc.Space("spc_a").Visible() })
}

// A failed Load is retried with a backoff until it succeeds, then the cache is reloaded periodically.
func TestRunRetriesAFailedLoad(t *testing.T) {
	t.Parallel()
	svc, _ := newSvc(t, nil)
	svc.retryMin, svc.retryMax, svc.reloadEvery = time.Millisecond, 5*time.Millisecond, 10*time.Millisecond
	var mu sync.Mutex
	calls := 0
	svc.loader = func(ctx context.Context) error {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls <= 3 {
			return errors.New("database busy")
		}
		return svc.Load(ctx)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); svc.Run(ctx) }()
	defer func() { cancel(); <-done }()
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return calls >= 6 // 3 failures, the successful load, then periodic reloads
	})
	svc.mu.RLock()
	loaded := svc.loaded
	svc.mu.RUnlock()
	if !loaded {
		t.Fatal("the cache was never loaded")
	}
}

// A stored row with one out-of-bounds field keeps its valid fields: a stricter link cap is never widened.
func TestOutOfBoundsFieldKeepsTheStricterCap(t *testing.T) {
	t.Parallel()
	svc, s := newSvc(t, nil)
	ctx := context.Background()
	if err := s.W().TxNamed(ctx, "seed", func(tx *store.Tx) error {
		_, err := tx.SQL.ExecContext(ctx, `INSERT INTO telegram_settings (singleton, settings_json, updated_at, updated_by) VALUES (1, ?, 0, 't')`,
			`{"link_default_max_role":"viewer","links_per_user":9,"inbound_limits":{"updates_per_second":5}}`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Load(ctx); err != nil {
		t.Fatal(err)
	}
	got := svc.Current()
	if got.LinkDefaultMaxRole != RoleViewer || got.InboundLimits.UpdatesPerSecond != 5 || got.LinksPerUser != Defaults().LinksPerUser {
		t.Fatalf("effective %+v: want viewer cap and the valid limit kept, the invalid field at its default", got)
	}
}

func seedRaw(t *testing.T, svc *Service, s *store.Store, js string) Settings {
	t.Helper()
	ctx := context.Background()
	if err := s.W().TxNamed(ctx, "seed", func(tx *store.Tx) error {
		_, err := tx.SQL.ExecContext(ctx, `INSERT INTO telegram_settings (singleton, settings_json, updated_at, updated_by) VALUES (1, ?, 0, 't')`, js)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Load(ctx); err != nil {
		t.Fatal(err)
	}
	return svc.Current()
}

// One invalid sibling inside a nested object keeps the other stored leaves of that object.
func TestInvalidNestedSiblingKeepsStoredLeaves(t *testing.T) {
	t.Parallel()
	svc, s := newSvc(t, nil)
	got := seedRaw(t, svc, s, `{"inbound_limits":{"actions_per_minute":5,"updates_per_second":500},"send_limits":{"chat_burst":2,"global_per_second":99}}`)
	d := Defaults()
	if got.InboundLimits.ActionsPerMinute != 5 || got.InboundLimits.UpdatesPerSecond != d.InboundLimits.UpdatesPerSecond {
		t.Fatalf("inbound_limits %+v: want actions_per_minute 5 kept, updates_per_second at its default", got.InboundLimits)
	}
	if got.SendLimits.ChatBurst != 2 || got.SendLimits.GlobalPerSecond != d.SendLimits.GlobalPerSecond {
		t.Fatalf("send_limits %+v: want chat_burst 2 kept, global_per_second at its default", got.SendLimits)
	}
}

// An unknown stored link cap falls back to viewer, the stricter choice, not to the operator default.
func TestUnknownLinkCapFallsBackToViewer(t *testing.T) {
	t.Parallel()
	svc, s := newSvc(t, nil)
	if got := seedRaw(t, svc, s, `{"link_default_max_role":"guest","links_per_user":2}`); got.LinkDefaultMaxRole != RoleViewer || got.LinksPerUser != 2 {
		t.Fatalf("effective %+v: want viewer cap and links_per_user 2", got)
	}
}
