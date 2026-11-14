package notify_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/eventbus"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/notify"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/sender"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/settings"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/tgtest"
)

var t0 = time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)

// feed is an in-memory change feed that counts its reads.
type feed struct {
	mu      sync.Mutex
	rows    []changefeed.Change
	reads   atomic.Int64 // After calls
	latests atomic.Int64
}

func (f *feed) Latest(context.Context) (int64, error) {
	f.latests.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.rows) == 0 {
		return 0, nil
	}
	return f.rows[len(f.rows)-1].Seq, nil
}

func (f *feed) After(_ context.Context, after int64, limit int) ([]changefeed.Change, error) {
	f.reads.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []changefeed.Change
	for _, r := range f.rows {
		if r.Seq > after {
			out = append(out, r)
			if len(out) == limit {
				break
			}
		}
	}
	return out, nil
}

func (f *feed) add(space, typ, id, action string, version int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows = append(f.rows, changefeed.Change{Seq: int64(len(f.rows) + 1), SpaceID: space, ResourceType: typ, ResourceID: id, Action: action, Version: version})
}

// reader serves resources from a map and counts reads.
type reader struct {
	mu    sync.Mutex
	items map[notify.Key]notify.Resource
	reads atomic.Int64
}

func (r *reader) Read(_ context.Context, _ map[string]any, k notify.Key) (*notify.Resource, error) {
	r.reads.Add(1)
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.items[k]
	if !ok {
		return nil, nil
	}
	return &v, nil
}

func (r *reader) set(res notify.Resource) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.items[res.Key] = res
}

// authorizer allows subject -> space pairs (every permission), and roles by subject.
type authorizer struct {
	mu    sync.Mutex
	space map[string]map[string]bool // subject -> space -> allowed
	roles map[string]bool            // "subject|role"
}

func (a *authorizer) AllowedAll(subjects []string, domain string, _ authz.Permission) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, s := range subjects {
		if !a.space[s][domain] {
			return false
		}
	}
	return len(subjects) > 0
}

func (a *authorizer) HoldsRole(subjects []string, _ string, r authz.Role) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, s := range subjects {
		if !a.roles[s+"|"+string(r)] {
			return false
		}
	}
	return len(subjects) > 0
}

type policy struct {
	set    settings.Settings
	hidden map[string]bool
	level  string
}

func (p *policy) Current() settings.Settings { return p.set }
func (p *policy) Space(id string) settings.Space {
	s := settings.DefaultSpace(id)
	s.Enabled = !p.hidden[id]
	if p.level != "" {
		s.ContentLevel = p.level
	}
	return s
}

// mappings is an in-memory telegram_messages that counts writes.
type mappings struct {
	mu      sync.Mutex
	rows    map[[2]int64]notify.Mapping
	saves   int
	lookups int
}

func (m *mappings) Save(_ context.Context, rows []notify.Mapping) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.saves++
	for _, r := range rows {
		m.rows[[2]int64{r.ChatID, int64(r.MessageID)}] = r
	}
	return nil
}

func (m *mappings) Lookup(_ context.Context, k notify.Key) ([]notify.Mapping, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lookups++
	var out []notify.Mapping
	for _, r := range m.rows {
		if r.Key == k {
			out = append(out, r)
		}
	}
	return out, nil
}

func (m *mappings) counts() (saves, rows int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.saves, len(m.rows)
}

type env struct {
	t      *testing.T
	fake   *tgtest.Server
	clk    *clock.Fake
	snd    *sender.Sender
	bus    *eventbus.Bus
	feed   *feed
	rd     *reader
	az     *authorizer
	pol    *policy
	maps   *mappings
	links  *notify.Links
	window chan struct{} // the notifier's window ends when the test sends here
	n      *notify.Notifier
	stop   func()
}

func linkFor(i int) notify.Link {
	return notify.Link{ID: fmt.Sprintf("tgl_%d", i), UserID: fmt.Sprintf("usr_%d", i), ChatID: int64(1000 + i)}
}

func newEnv(t *testing.T, nLinks int) *env {
	t.Helper()
	clk := clock.NewFake(t0)
	fake := tgtest.New(t, tgtest.Options{Clock: clk})
	c, err := botapi.New(botapi.Options{BaseURL: fake.URL(), Token: fake.Token()})
	if err != nil {
		t.Fatal(err)
	}
	snd, err := sender.New(sender.Options{Client: c, Workers: 1, Clock: clk, Limits: sender.Limits{GlobalPerSecond: 25, ChatPerSecond: 1, ChatBurst: 3, GroupPerMinute: 20, QueueTotal: 5000, QueuePerChat: 1000},
		After: func(d time.Duration) <-chan time.Time {
			clk.Advance(d)
			ch := make(chan time.Time, 1)
			ch <- clk.Now()
			return ch
		}})
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, fake: fake, clk: clk, snd: snd, bus: eventbus.New(), feed: &feed{}, rd: &reader{items: map[notify.Key]notify.Resource{}},
		az: &authorizer{space: map[string]map[string]bool{}, roles: map[string]bool{}}, pol: &policy{set: settings.Defaults(), hidden: map[string]bool{}},
		maps: &mappings{rows: map[[2]int64]notify.Mapping{}}, window: make(chan struct{}, 1)}
	e.links = notify.NewLinks(nil, e.bus, nil, nil, 0)
	var ls []notify.Link
	for i := 1; i <= nLinks; i++ {
		l := linkFor(i)
		ls = append(ls, l)
		e.allow(l, "spc_1")
	}
	e.links.Set(ls)
	ctx, cancel := context.WithCancel(context.Background())
	sdone := make(chan struct{})
	go func() { _ = snd.Run(ctx); close(sdone) }()
	e.stop = func() { cancel(); <-sdone }
	t.Cleanup(func() { e.stop() })
	return e
}

func (e *env) allow(l notify.Link, space string) {
	e.az.mu.Lock()
	defer e.az.mu.Unlock()
	for _, s := range []string{"telegram:" + l.ID, "user:" + l.UserID} {
		if e.az.space[s] == nil {
			e.az.space[s] = map[string]bool{}
		}
		e.az.space[s][space] = true
	}
}

// run starts the notifier; its windows end when the test calls endWindow.
func (e *env) run(m notify.Mappings) (stop func()) {
	e.t.Helper()
	opts := notify.Options{Feed: e.feed, Reader: e.rd, Authz: e.az, Policy: e.pol, Links: e.links, Sender: func() *sender.Sender { return e.snd },
		Bus: e.bus, Now: e.clk.Now,
		After: func(time.Duration) <-chan time.Time {
			ch := make(chan time.Time, 1)
			go func() { <-e.window; ch <- time.Now() }()
			return ch
		}}
	if m != nil {
		opts.Store = m
	}
	n := notify.New(opts)
	e.n = n
	started := e.feed.latests.Load()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = n.Run(ctx); close(done) }()
	// Run reads its start position first; wait for it.
	deadline := time.Now().Add(10 * time.Second)
	for e.feed.latests.Load() == started {
		if time.Now().After(deadline) {
			e.t.Fatal("notifier did not start")
		}
		time.Sleep(time.Millisecond)
	}
	return func() { cancel(); <-done }
}

func (e *env) ring() { e.bus.Publish(eventbus.Event{Topic: eventbus.TopicChange}) }

func (e *env) endWindow() { e.window <- struct{}{} }

func (e *env) waitCalls(method string, n int) []tgtest.Call {
	e.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		c := e.fake.Calls(method)
		if len(c) >= n {
			return c
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("waited for %d %s calls, have %d", n, method, len(c))
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func action(id string, version int64, final bool) notify.Resource {
	st := "open"
	if final {
		st = "resolved: approved"
	}
	return notify.Resource{Key: notify.Key{Type: notify.TypeHumanAction, ID: id}, SpaceID: "spc_1", Version: version, Kind: notify.KindAction,
		Perm: authz.HumanRead, Label: "Approval", Title: "Deploy " + id, Detail: "Please approve", State: st, Priority: "normal", Notify: !final, Final: final}
}

// settle waits until the notifier has handled a ring: the feed was read after it.
func (e *env) settle(readsBefore int64) {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for e.feed.reads.Load() <= readsBefore {
		if time.Now().After(deadline) {
			e.t.Fatal("the notifier did not read the feed")
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(30 * time.Millisecond)
}

// TestBudget is the ADR-0078 measured acceptance (plan §4.2): 1000 changes for 50 actions in one window with 5
// linked users cost at most 50 resource reads and 5 × 50 sends; an idle notifier reads no changes; a window that sends
// nothing writes nothing.
func TestBudget(t *testing.T) {
	e := newEnv(t, 5)
	stop := e.run(e.maps)
	defer stop()

	for i := range 1000 {
		id := fmt.Sprintf("hum_%d", i%50)
		e.rd.set(action(id, 1, false))
		e.feed.add("spc_1", notify.TypeHumanAction, id, changefeed.Created, 1)
	}
	e.ring()
	e.endWindow()
	e.waitCalls("sendMessage", 250)
	time.Sleep(100 * time.Millisecond)

	if r := e.rd.reads.Load(); r > 50 {
		t.Errorf("%d resource reads for 50 distinct actions, want at most 50", r)
	}
	if c := len(e.fake.Calls("sendMessage")); c > 5*50 {
		t.Errorf("%d sends, want at most %d", c, 5*50)
	}
	if c := len(e.fake.Calls("editMessageText")); c != 0 {
		t.Errorf("%d edits for new cards", c)
	}
	// 250 mappings in batches, not per send.
	deadline := time.Now().Add(5 * time.Second)
	for {
		saves, rows := e.maps.counts()
		if rows == 250 {
			if saves > 250/100+2 {
				t.Errorf("%d mapping writes for 250 rows, want batches of 100 or one a second", saves)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d mapping rows (%d writes), want 250", rows, saves)
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Idle: no changes reads and no writes.
	reads, saves, _ := e.feed.reads.Load(), 0, 0
	s0, _ := e.maps.counts()
	time.Sleep(200 * time.Millisecond)
	if e.feed.reads.Load() != reads {
		t.Error("the notifier read changes while idle")
	}
	if s1, _ := e.maps.counts(); s1 != s0 {
		t.Errorf("mapping writes while idle: %d -> %d", s0, s1)
	}
	_ = saves

	// A window of changes nobody may see sends and writes nothing.
	e.pol.hidden["spc_1"] = true
	e.rd.set(action("hum_x", 1, false))
	before := e.feed.reads.Load()
	e.feed.add("spc_1", notify.TypeHumanAction, "hum_x", changefeed.Created, 1)
	e.ring()
	e.endWindow()
	e.settle(before)
	if c := len(e.fake.Calls("sendMessage")); c != 250 {
		t.Errorf("%d sends after a window for a hidden space, want 250", c)
	}
	if s1, _ := e.maps.counts(); s1 != s0 {
		t.Errorf("a window that sent nothing wrote mappings: %d -> %d", s0, s1)
	}
}

func TestStartsAtTheNewestChange(t *testing.T) {
	e := newEnv(t, 1)
	e.rd.set(action("hum_old", 1, false))
	e.feed.add("spc_1", notify.TypeHumanAction, "hum_old", changefeed.Created, 1)
	stop := e.run(nil)
	defer stop()

	before := e.feed.reads.Load()
	e.rd.set(action("hum_new", 1, false))
	e.feed.add("spc_1", notify.TypeHumanAction, "hum_new", changefeed.Created, 1)
	e.ring()
	e.endWindow()
	calls := e.waitCalls("sendMessage", 1)
	e.settle(before)
	if n := len(e.fake.Calls("sendMessage")); n != 1 || !strings.Contains(calls[0].Params["text"], "hum_new") {
		t.Errorf("sends = %d, first %q: history must not be replayed", n, calls[0].Params["text"])
	}
}

func TestWindowIsBounded(t *testing.T) {
	e := newEnv(t, 2)
	stop := e.run(nil)
	defer stop()
	const n = 1200
	for i := range n {
		id := fmt.Sprintf("hum_%d", i)
		e.rd.set(action(id, 1, false))
		e.feed.add("spc_1", notify.TypeHumanAction, id, changefeed.Created, 1)
	}
	e.ring()
	e.endWindow()
	e.waitCalls("sendMessage", 2*notify.MaxKeys+2)
	time.Sleep(100 * time.Millisecond)
	if r := e.rd.reads.Load(); r != notify.MaxKeys {
		t.Errorf("%d reads, want the cap %d", r, notify.MaxKeys)
	}
	many := 0
	for _, c := range e.fake.Calls("sendMessage") {
		if strings.Contains(c.Params["text"], "Many changes") {
			many++
		}
	}
	if many != 2 {
		t.Errorf("%d overflow notices for 2 chats, want one each", many)
	}
	if c := len(e.fake.Calls("sendMessage")); c != 2*notify.MaxKeys+2 {
		t.Errorf("%d sends, want %d", c, 2*notify.MaxKeys+2)
	}
}

func TestRecipients(t *testing.T) {
	e := newEnv(t, 4)
	// link 2: no grant in the space; link 3: muted; link 4: lacks the type's view role.
	e.az.mu.Lock()
	delete(e.az.space["telegram:tgl_2"], "spc_1")
	e.az.mu.Unlock()
	muted := []string{"spc_1"}
	ls := e.links.Snapshot()
	ls[2].Prefs.MutedSpaces = &muted
	e.links.Set(ls)
	e.az.mu.Lock()
	e.az.roles["telegram:tgl_1|operator"], e.az.roles["user:usr_1|operator"] = true, true
	e.az.roles["telegram:tgl_3|operator"], e.az.roles["user:usr_3|operator"] = true, true
	e.az.mu.Unlock()
	stop := e.run(nil)
	defer stop()

	r := action("hum_1", 1, false)
	r.ViewRole = "operator"
	e.rd.set(r)
	e.feed.add("spc_1", notify.TypeHumanAction, "hum_1", changefeed.Created, 1)
	e.ring()
	e.endWindow()
	calls := e.waitCalls("sendMessage", 1)
	time.Sleep(100 * time.Millisecond)
	if n := len(e.fake.Calls("sendMessage")); n != 1 {
		t.Fatalf("%d sends, want only link 1's", n)
	}
	if c := calls[0].Params["chat_id"]; c != "1001" {
		t.Errorf("sent to chat %s, want 1001", c)
	}
}

func TestPreferences(t *testing.T) {
	e := newEnv(t, 2)
	off := false
	ls := e.links.Snapshot()
	ls[1].Prefs.NewActions = &off
	e.links.Set(ls)
	stop := e.run(nil)
	defer stop()
	e.rd.set(action("hum_1", 1, false))
	e.feed.add("spc_1", notify.TypeHumanAction, "hum_1", changefeed.Created, 1)
	e.ring()
	e.endWindow()
	e.waitCalls("sendMessage", 1)
	time.Sleep(100 * time.Millisecond)
	if n := len(e.fake.Calls("sendMessage")); n != 1 {
		t.Errorf("%d sends, want 1 (the other link turned new actions off)", n)
	}
}

// TestCardEdit: the final state edits the card that was sent, in the same chat and message, and sends nothing new.
func TestCardEdit(t *testing.T) {
	e := newEnv(t, 1)
	stop := e.run(e.maps)
	defer stop()

	e.rd.set(action("hum_1", 1, false))
	e.feed.add("spc_1", notify.TypeHumanAction, "hum_1", changefeed.Created, 1)
	e.ring()
	e.endWindow()
	e.waitCalls("sendMessage", 1)

	before := e.feed.reads.Load()
	e.rd.set(action("hum_1", 2, true))
	e.feed.add("spc_1", notify.TypeHumanAction, "hum_1", changefeed.Updated, 2)
	e.ring()
	e.endWindow()
	e.settle(before)
	edits := e.waitCalls("editMessageText", 1)
	if n := len(e.fake.Calls("sendMessage")); n != 1 {
		t.Errorf("%d sends, want the one card", n)
	}
	msgs := e.fake.Messages(1001)
	if len(msgs) != 1 || !strings.Contains(msgs[0].Text, "resolved: approved") {
		t.Fatalf("messages = %+v, want one card in its final state", msgs)
	}
	if edits[0].Params["message_id"] != fmt.Sprint(msgs[0].MessageID) {
		t.Errorf("edited message %s, want %d", edits[0].Params["message_id"], msgs[0].MessageID)
	}
	if len(msgs[0].Entities) != 1 || msgs[0].Entities[0].Type != "bold" {
		t.Errorf("entities = %+v, want the bold head", msgs[0].Entities)
	}

	// The same version again changes nothing; a resolved action nobody was told about sends nothing.
	before = e.feed.reads.Load()
	e.feed.add("spc_1", notify.TypeHumanAction, "hum_1", changefeed.Updated, 2)
	e.rd.set(action("hum_2", 1, true))
	e.feed.add("spc_1", notify.TypeHumanAction, "hum_2", changefeed.Created, 1)
	e.ring()
	e.endWindow()
	e.settle(before)
	if len(e.fake.Calls("editMessageText")) != 1 || len(e.fake.Calls("sendMessage")) != 1 {
		t.Errorf("calls after no-ops: %d edits, %d sends", len(e.fake.Calls("editMessageText")), len(e.fake.Calls("sendMessage")))
	}
}

// TestCardEditAfterRestart: the mapping survives in telegram_messages, so a new process edits the old card.
func TestCardEditAfterRestart(t *testing.T) {
	e := newEnv(t, 1)
	stop := e.run(e.maps)
	e.rd.set(action("hum_1", 1, false))
	e.feed.add("spc_1", notify.TypeHumanAction, "hum_1", changefeed.Created, 1)
	e.ring()
	e.endWindow()
	e.waitCalls("sendMessage", 1)
	time.Sleep(100 * time.Millisecond) // the card is recorded just after the call returns
	stop()                             // flushes the mapping
	if _, rows := e.maps.counts(); rows != 1 {
		t.Fatalf("%d mappings after shutdown, want 1", rows)
	}

	stop = e.run(e.maps)
	defer stop()
	before := e.feed.reads.Load()
	e.rd.set(action("hum_1", 2, true))
	e.feed.add("spc_1", notify.TypeHumanAction, "hum_1", changefeed.Updated, 2)
	e.ring()
	e.endWindow()
	e.settle(before)
	e.waitCalls("editMessageText", 1)
	if n := len(e.fake.Calls("sendMessage")); n != 1 {
		t.Errorf("%d sends, want the original card only", n)
	}
}

// TestCoalescedStates: while the sender is busy a newer state replaces an unsent older one (one message per chat and
// resource, showing the newest state).
func TestCoalescedStates(t *testing.T) {
	e := newEnv(t, 1)
	stop := e.run(nil)
	defer stop()
	for v := int64(1); v <= 3; v++ {
		before := e.feed.reads.Load()
		e.rd.set(action("hum_1", v, v == 3))
		e.feed.add("spc_1", notify.TypeHumanAction, "hum_1", changefeed.Updated, v)
		e.ring()
		e.endWindow()
		e.settle(before)
	}
	time.Sleep(100 * time.Millisecond)
	msgs := e.fake.Messages(1001)
	if len(msgs) != 1 {
		t.Fatalf("%d messages for one action, want its single card", len(msgs))
	}
	if !strings.Contains(msgs[0].Text, "resolved") {
		t.Errorf("card %q doesn't show the final state", msgs[0].Text)
	}
}

func TestContentLevel(t *testing.T) {
	for level, wantBody := range map[string]bool{settings.ContentFull: true, settings.ContentSummary: false} {
		t.Run(level, func(t *testing.T) {
			e := newEnv(t, 1)
			e.pol.level = level
			stop := e.run(nil)
			defer stop()
			e.rd.set(action("hum_1", 1, false))
			e.feed.add("spc_1", notify.TypeHumanAction, "hum_1", changefeed.Created, 1)
			e.ring()
			e.endWindow()
			c := e.waitCalls("sendMessage", 1)
			if got := strings.Contains(c[0].Params["text"], "Please approve"); got != wantBody {
				t.Errorf("content level %s: body shown = %v in %q", level, got, c[0].Params["text"])
			}
		})
	}
}

func notifyDefaults() settings.NotificationDefaults { return settings.Defaults().NotificationDefaults }
