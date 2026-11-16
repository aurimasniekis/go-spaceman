package topics_test

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/sender"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/settings"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/tgtest"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/topics"
)

type memStore struct {
	mu    sync.Mutex
	rows  map[string]map[string]int
	saves int
}

func (s *memStore) Load(_ context.Context, id string) (map[string]int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]int{}
	for k, v := range s.rows[id] {
		out[k] = v
	}
	return out, nil
}

func (s *memStore) Save(_ context.Context, id string, m map[string]int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saves++
	s.rows[id] = m
	return nil
}

// Threads returns every link's thread of key; each test link is in chat (42).
func (s *memStore) Threads(_ context.Context, key string) ([]topics.LinkThread, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []topics.LinkThread
	for id, m := range s.rows {
		if t := m[key]; t != 0 {
			out = append(out, topics.LinkThread{LinkID: id, ChatID: chat, ID: t})
		}
	}
	return out, nil
}

const chat = int64(42)

func setup(t *testing.T) (*tgtest.Server, *botapi.Client, *memStore, func() *topics.Manager) {
	t.Helper()
	srv := tgtest.New(t, tgtest.Options{Limits: tgtest.Limits{Disabled: true}})
	c, err := botapi.New(botapi.Options{BaseURL: srv.URL(), Token: srv.Token(), HTTPClient: http.DefaultClient})
	if err != nil {
		t.Fatal(err)
	}
	st := &memStore{rows: map[string]map[string]int{}}
	return srv, c, st, func() *topics.Manager {
		return topics.New(topics.Options{Store: st, Mode: func(string) string { return settings.ThreadsOn },
			SpaceName: func(context.Context, string) string { return "Alpha" }})
	}
}

func send(t *testing.T, m *topics.Manager, c *botapi.Client, key, text string) botapi.Message {
	t.Helper()
	msg, err := m.Send(context.Background(), c, "tgl_1", chat, key, botapi.SendMessageParams{Text: text})
	if err != nil {
		t.Fatal(err)
	}
	return msg
}

// Route: a space's topic for every kind of message, home for a cross-space one, the main chat when the link's layout
// is off, or auto while the bot has no topics. Each link has its own layout.
func TestRoutePerLink(t *testing.T) {
	var botTopics atomic.Bool
	modes := map[string]string{"tgl_on": settings.ThreadsOn, "tgl_off": settings.ThreadsOff, "tgl_auto": settings.ThreadsAuto, "tgl_unset": ""}
	m := topics.New(topics.Options{Mode: func(id string) string { return modes[id] }, BotTopics: botTopics.Load})
	check := func(link, space, want string) {
		t.Helper()
		if got := m.Route(link, space); got != want {
			t.Errorf("Route(%s, %q) = %q, want %q (bot topics %v)", link, space, got, want, botTopics.Load())
		}
	}
	check("tgl_on", "spc_1", "spc_1")
	check("tgl_on", "", topics.Home)
	check("tgl_off", "spc_1", "")
	check("tgl_auto", "spc_1", "") // auto follows getMe: off
	check("tgl_unset", "spc_1", "")
	if m.Enabled("tgl_auto", chat) || !m.Enabled("tgl_on", chat) || m.Enabled("tgl_off", chat) {
		t.Error("Enabled without bot topics")
	}
	botTopics.Store(true)
	check("tgl_auto", "spc_1", "spc_1")
	check("tgl_unset", "spc_1", "spc_1")
	check("tgl_off", "spc_1", "") // an explicit off wins over the bot
	if !m.Enabled("tgl_auto", chat) || m.Enabled("tgl_auto", -100) {
		t.Error("Enabled with bot topics: auto is on in a private chat, never in a group")
	}
	var nilM *topics.Manager
	if nilM.Route("tgl_on", "spc_1") != "" || nilM.Enabled("tgl_on", chat) {
		t.Error("a nil Manager routes to the main chat")
	}
	if topics.New(topics.Options{}).Route("tgl_on", "spc_1") != "" {
		t.Error("no Mode: off")
	}
}

// A refused chat is not Enabled for a while, and comes back after RefusedFor.
func TestEnabledFollowsRefusal(t *testing.T) {
	srv, c, st, _ := setup(t)
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	m := topics.New(topics.Options{Store: st, Mode: func(string) string { return settings.ThreadsOn }, Now: clock})
	srv.DisableTopics(chat)
	if !m.Enabled("tgl_1", chat) {
		t.Fatal("on before any refusal")
	}
	if got := send(t, m, c, "spc_1", "one"); got.MessageThreadID != 0 {
		t.Fatalf("thread %d, want the main chat", got.MessageThreadID)
	}
	if m.Enabled("tgl_1", chat) {
		t.Error("a refused chat is not enabled")
	}
	mu.Lock()
	now = now.Add(topics.RefusedFor + time.Second)
	mu.Unlock()
	if !m.Enabled("tgl_1", chat) {
		t.Error("enabled again after RefusedFor")
	}
}

// A topic is created once, remembered across a restart (a new Manager on the same store), and named after the space.
func TestCreatedOnceAndRemembered(t *testing.T) {
	srv, c, st, mk := setup(t)
	m := mk()
	a := send(t, m, c, "spc_1", "one")
	b := send(t, m, c, "spc_1", "two")
	if a.MessageThreadID == 0 || a.MessageThreadID != b.MessageThreadID {
		t.Fatalf("threads %d %d", a.MessageThreadID, b.MessageThreadID)
	}
	if got := srv.Topics(chat)[a.MessageThreadID]; got != "Alpha" {
		t.Errorf("topic name %q", got)
	}
	if d := send(t, mk(), c, "spc_1", "four"); d.MessageThreadID != a.MessageThreadID || len(srv.Calls("createForumTopic")) != 1 {
		t.Errorf("after a restart: thread %d, %d topics created", d.MessageThreadID, len(srv.Calls("createForumTopic")))
	}
	if st.rows["tgl_1"]["spc_1"] != a.MessageThreadID {
		t.Errorf("stored %v", st.rows)
	}
}

func TestMainChatForNoKey(t *testing.T) {
	srv, c, _, mk := setup(t)
	if m := send(t, mk(), c, "", "hi"); m.MessageThreadID != 0 || len(srv.Calls("createForumTopic")) != 0 {
		t.Errorf("thread %d", m.MessageThreadID)
	}
}

func TestConcurrentSendsCreateOneTopic(t *testing.T) {
	srv, c, _, mk := setup(t)
	m := mk()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); send(t, m, c, "spc_1", "x") }()
	}
	wg.Wait()
	if n := len(srv.Calls("createForumTopic")); n != 1 {
		t.Errorf("%d topics created", n)
	}
}

// A deleted topic is created again and the stored ID is replaced; a closed one too.
func TestDeletedAndClosedTopicsAreRecreated(t *testing.T) {
	srv, c, st, mk := setup(t)
	m := mk()
	first := send(t, m, c, "spc_1", "one").MessageThreadID
	srv.DeleteTopic(chat, first)
	second := send(t, m, c, "spc_1", "two").MessageThreadID
	if second == 0 || second == first || st.rows["tgl_1"]["spc_1"] != second {
		t.Fatalf("deleted: thread %d (was %d), stored %v", second, first, st.rows)
	}
	srv.CloseTopic(chat, second)
	if third := send(t, m, c, "spc_1", "three").MessageThreadID; third == 0 || third == second {
		t.Fatalf("closed: thread %d (was %d)", third, second)
	}
}

// When the topic can't be created again, the message still goes out, to the main chat.
func TestFallsBackToTheMainChat(t *testing.T) {
	srv, c, _, mk := setup(t)
	m := mk()
	first := send(t, m, c, "spc_1", "one").MessageThreadID
	srv.DeleteTopic(chat, first)
	srv.DisableTopics(chat)
	if got := send(t, m, c, "spc_1", "two"); got.MessageThreadID != 0 || got.Text != "two" {
		t.Fatalf("message %+v", got)
	}
	// The chat is left alone afterwards, not asked again for every message.
	send(t, m, c, "spc_1", "three")
	if n := len(srv.Calls("createForumTopic")); n != 2 {
		t.Errorf("%d create calls, want 2", n)
	}
}

// Every topic is gone, also a freshly created one: exactly one recreate, then the main chat, and the send ends.
func TestAllTopicsGoneRecreatesOnceThenMainChat(t *testing.T) {
	srv, c, _, mk := setup(t)
	m := mk()
	send(t, m, c, "spc_1", "one") // makes the topic
	srv.GoneTopics(chat)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second) // a send that never ends fails here
	defer cancel()
	got, err := m.Send(ctx, c, "tgl_1", chat, "spc_1", botapi.SendMessageParams{Text: "two"})
	if err != nil || got.MessageThreadID != 0 || got.Text != "two" {
		t.Fatalf("message %+v, error %v, want one in the main chat", got, err)
	}
	if n := len(srv.Calls("createForumTopic")); n != 2 { // the first topic, then one recreate
		t.Errorf("%d topics created, want 1 + 1 recreate", n)
	}
	inMain := 0
	for _, msg := range srv.Messages(chat) {
		if msg.MessageThreadID == 0 {
			inMain++
		}
	}
	if inMain != 1 {
		t.Errorf("%d messages in the main chat, want 1", inMain)
	}
}

// A rate limit on createForumTopic is returned for the sender to repeat; the chat is not marked as refusing topics, and
// the next attempt creates the topic once.
func TestCreateRateLimitIsReturnedAndNotRemembered(t *testing.T) {
	srv, c, _, mk := setup(t)
	m := mk()
	srv.FailNext("createForumTopic", 429, "Too Many Requests: retry after 1", 1)
	if _, err := m.Send(context.Background(), c, "tgl_1", chat, "spc_1", botapi.SendMessageParams{Text: "one"}); err == nil {
		t.Fatal("want the rate limit error")
	}
	if got := send(t, m, c, "spc_1", "two"); got.MessageThreadID == 0 {
		t.Fatal("the chat was treated as refusing topics")
	}
	if n := len(srv.Calls("createForumTopic")); n != 2 { // the refused call, then one real create
		t.Errorf("%d create calls, want 2", n)
	}
	if n := len(srv.Topics(chat)); n != 1 {
		t.Errorf("%d topics exist, want 1", n)
	}
}

// SpaceOf answers only from stored threads, and never for the legacy Needs-you topic, a home topic or an unknown thread.
func TestSpaceOf(t *testing.T) {
	_, c, st, mk := setup(t)
	m := mk()
	a := send(t, m, c, "spc_1", "one").MessageThreadID
	n, h := 777, 778
	st.mu.Lock()
	st.rows["tgl_1"][topics.LegacyNeeds], st.rows["tgl_1"][topics.HomeKey] = n, h
	st.mu.Unlock()
	m = mk() // reads the store again
	ctx := context.Background()
	if id, ok := mk().SpaceOf(ctx, "tgl_1", a); !ok || id != "spc_1" { // a new Manager: read from the store
		t.Errorf("SpaceOf(%d) = %q, %v", a, id, ok)
	}
	for _, th := range []int{n, h, 0, 12345} {
		if id, ok := m.SpaceOf(ctx, "tgl_1", th); ok {
			t.Errorf("SpaceOf(%d) = %q, want none", th, id)
		}
	}
	if _, ok := m.SpaceOf(ctx, "tgl_other", a); ok {
		t.Error("another link's lookup must not see this link's topics")
	}
}

// ThreadOf is the forward lookup: a space's stored topic, never created by asking, none for the legacy or home keys.
func TestThreadOf(t *testing.T) {
	_, c, _, mk := setup(t)
	m := mk()
	ctx := context.Background()
	if th, ok := m.ThreadOf(ctx, "tgl_1", "spc_1"); ok {
		t.Fatalf("ThreadOf before any topic = %d", th)
	}
	a := send(t, m, c, "spc_1", "one").MessageThreadID
	if th, ok := mk().ThreadOf(ctx, "tgl_1", "spc_1"); !ok || th != a {
		t.Errorf("ThreadOf = %d, %v; want %d", th, ok, a)
	}
	for _, k := range []string{"", topics.HomeKey, topics.LegacyNeeds} {
		if th, ok := m.ThreadOf(ctx, "tgl_1", k); ok {
			t.Errorf("ThreadOf(%q) = %d", k, th)
		}
	}
	var nilm *topics.Manager
	if _, ok := nilm.ThreadOf(ctx, "tgl_1", "spc_1"); ok {
		t.Error("a nil Manager has no topics")
	}
}

// The legacy Needs-you topic: on the first topic send after the upgrade, one note is posted in it and its key is
// dropped from the link's topics; the topic is never deleted. A second send, a new Manager on the same store and many
// concurrent sends post no second note.
func TestLegacyNeedsRetiredOnceUnderConcurrentSends(t *testing.T) {
	srv, c, st, mk := setup(t)
	legacy, err := c.CreateForumTopic(context.Background(), botapi.CreateForumTopicParams{ChatID: chat, Name: "Needs you"})
	if err != nil {
		t.Fatal(err)
	}
	st.rows["tgl_1"] = map[string]int{topics.LegacyNeeds: legacy.MessageThreadID}
	m := mk()
	if id, ok := m.SpaceOf(context.Background(), "tgl_1", legacy.MessageThreadID); ok {
		t.Fatalf("SpaceOf(legacy) = %q before retirement", id)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); send(t, m, c, "spc_1", "x") }()
	}
	wg.Wait()
	send(t, mk(), c, "spc_2", "y") // after a restart
	notes := srv.MessagesIn(chat, legacy.MessageThreadID)
	if len(notes) != 1 || notes[0].Text != topics.LegacyNote {
		t.Fatalf("messages in the legacy topic %+v, want the one note", notes)
	}
	st.mu.Lock()
	_, kept := st.rows["tgl_1"][topics.LegacyNeeds]
	st.mu.Unlock()
	if kept {
		t.Errorf("stored %v, want the needs key dropped", st.rows["tgl_1"])
	}
	if _, ok := srv.Topics(chat)[legacy.MessageThreadID]; !ok || len(srv.Calls("deleteForumTopic")) != 0 {
		t.Error("the legacy topic must never be deleted")
	}
	if n := len(srv.Calls("createForumTopic")); n != 3 { // the legacy one (test setup), spc_1, spc_2
		t.Errorf("%d topics created, want 3", n)
	}
}

// A legacy topic the user already deleted: the key is dropped without a note, and the send goes on.
func TestLegacyNeedsGoneIsDroppedQuietly(t *testing.T) {
	srv, c, st, mk := setup(t)
	legacy, err := c.CreateForumTopic(context.Background(), botapi.CreateForumTopicParams{ChatID: chat, Name: "Needs you"})
	if err != nil {
		t.Fatal(err)
	}
	srv.DeleteTopic(chat, legacy.MessageThreadID)
	st.rows["tgl_1"] = map[string]int{topics.LegacyNeeds: legacy.MessageThreadID}
	if got := send(t, mk(), c, "spc_1", "x"); got.MessageThreadID == 0 {
		t.Fatal("the send must still reach the space topic")
	}
	if _, kept := st.rows["tgl_1"][topics.LegacyNeeds]; kept {
		t.Errorf("stored %v", st.rows["tgl_1"])
	}
	if ms := srv.MessagesIn(chat, legacy.MessageThreadID); len(ms) != 0 {
		t.Errorf("messages in a gone topic %v", ms)
	}
}

// A rate limit on the note leaves the key for a later send, which posts the note then.
func TestLegacyNeedsNoteRateLimitedIsRetried(t *testing.T) {
	srv, c, st, mk := setup(t)
	legacy, err := c.CreateForumTopic(context.Background(), botapi.CreateForumTopicParams{ChatID: chat, Name: "Needs you"})
	if err != nil {
		t.Fatal(err)
	}
	st.rows["tgl_1"] = map[string]int{topics.LegacyNeeds: legacy.MessageThreadID}
	m := mk()
	srv.FailNext("sendMessage", 429, "Too Many Requests: retry after 1", 1)
	send(t, m, c, "spc_1", "one")
	if _, kept := st.rows["tgl_1"][topics.LegacyNeeds]; !kept {
		t.Fatal("the key was dropped although the note was not sent")
	}
	send(t, m, c, "spc_1", "two")
	if notes := srv.MessagesIn(chat, legacy.MessageThreadID); len(notes) != 1 {
		t.Fatalf("notes %+v, want one", notes)
	}
	if _, kept := st.rows["tgl_1"][topics.LegacyNeeds]; kept {
		t.Error("the key must be dropped after the note")
	}
}

// Topic creation is bounded over every chat: with one create a second, a burst of new topics is spread out.
func TestCreatesAreRateLimited(t *testing.T) {
	_, c, st, _ := setup(t)
	m := topics.New(topics.Options{Store: st, Mode: func(string) string { return settings.ThreadsOn }, CreatesPerSecond: 10})
	start := time.Now()
	for i := 0; i < 13; i++ { // a burst of 10, then 3 more at 10/s
		if _, err := m.Send(context.Background(), c, "tgl_"+string(rune('a'+i)), chat, "spc_1", botapi.SendMessageParams{Text: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	if d := time.Since(start); d < 250*time.Millisecond {
		t.Errorf("13 creates took %v, want them spread by the limit", d)
	}
}

// renameEnv is a Manager whose renames go through a real sender running against the fake, with the space named title.
func renameEnv(t *testing.T, title *atomic.Value, may func(linkID, spaceID string) bool) (*tgtest.Server, *botapi.Client, *memStore, *topics.Manager) {
	t.Helper()
	srv, c, st, _ := setup(t)
	snd, err := sender.New(sender.Options{Client: c})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = snd.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	if may == nil {
		may = func(string, string) bool { return true }
	}
	m := topics.New(topics.Options{Store: st, Mode: func(string) string { return settings.ThreadsOn },
		SpaceName: func(context.Context, string) string { return title.Load().(string) },
		MayRead:   func(linkID string, _ int64, spaceID string) bool { return may(linkID, spaceID) },
		Sender:    func() *sender.Sender { return snd }})
	return srv, c, st, m
}

// waitFor polls until ok holds (renames are queued and sent on the sender's goroutines).
func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A rename edits the existing topic (no second create), and a rename to the title a topic already has is skipped.
func TestRenamedEditsTheTopicOnceAndSkipsNoOps(t *testing.T) {
	var title atomic.Value
	title.Store("Alpha")
	srv, c, _, m := renameEnv(t, &title, nil)
	first := send(t, m, c, "spc_1", "one")
	if n := len(srv.Calls("createForumTopic")); n != 1 {
		t.Fatalf("%d creates before the rename, want 1", n)
	}

	title.Store("Beta")
	m.Renamed(context.Background(), "spc_1", "Beta")
	waitFor(t, "the rename", func() bool { return srv.Topics(chat)[first.MessageThreadID] == "Beta" })
	if n := len(srv.Calls("editForumTopic")); n != 1 {
		t.Fatalf("%d edits after one rename, want 1", n)
	}

	m.Renamed(context.Background(), "spc_1", "Beta")  // the same title again: nothing to change
	m.Renamed(context.Background(), "spc_2", "Gamma") // a space with no topic: nothing to change
	time.Sleep(50 * time.Millisecond)
	if n := len(srv.Calls("editForumTopic")); n != 1 {
		t.Errorf("%d edits after a no-op rename, want 1", n)
	}
	if n := len(srv.Calls("createForumTopic")); n != 1 {
		t.Errorf("%d creates after the rename, want 1: a rename never creates a topic", n)
	}
	send(t, m, c, "spc_1", "two") // the topic is still the one used
	if n := len(srv.Calls("createForumTopic")); n != 1 {
		t.Errorf("%d creates after a send to the renamed space, want 1", n)
	}
}

// A topic that was deleted is left alone by a rename (no error, no create); the next send creates it again.
func TestRenamedDeletedTopicIsLeftAlone(t *testing.T) {
	var title atomic.Value
	title.Store("Alpha")
	srv, c, _, m := renameEnv(t, &title, nil)
	first := send(t, m, c, "spc_1", "one")
	srv.DeleteTopic(chat, first.MessageThreadID)
	title.Store("Beta")
	m.Renamed(context.Background(), "spc_1", "Beta")
	time.Sleep(50 * time.Millisecond)
	if n := len(srv.Calls("createForumTopic")); n != 1 {
		t.Fatalf("%d creates, want 1: a rename does not recreate a deleted topic", n)
	}
	if got := send(t, m, c, "spc_1", "two"); got.MessageThreadID == first.MessageThreadID {
		t.Fatal("the deleted topic was used again")
	}
}

// A link that may not read the space (no read permission, not visible, revoked) gets no edit, whether it lost access
// before the rename was queued or between the queueing and the send; a link that may still read it does.
func TestRenamedSkipsLinksThatMayNotRead(t *testing.T) {
	var title atomic.Value
	title.Store("Alpha")
	var allowed atomic.Bool
	allowed.Store(true)
	srv, c, _, m := renameEnv(t, &title, func(string, string) bool { return allowed.Load() })
	first := send(t, m, c, "spc_1", "one")

	allowed.Store(false) // a live link without read access on the space
	title.Store("Secret")
	m.Renamed(context.Background(), "spc_1", "Secret")
	time.Sleep(100 * time.Millisecond)
	if n := len(srv.Calls("editForumTopic")); n != 0 {
		t.Fatalf("%d edits for a link without read access, want 0", n)
	}
	allowed.Store(true)
	m.Renamed(context.Background(), "spc_1", "Secret")
	waitFor(t, "the rename of a link that may read", func() bool { return srv.Topics(chat)[first.MessageThreadID] == "Secret" })
}

// A link revoked (forgotten) or losing access between Renamed and the send gets no editForumTopic, and the send does not
// bring the link's state back.
func TestRenamedLinkRevokedBeforeTheSendGetsNoEdit(t *testing.T) {
	var title atomic.Value
	title.Store("Alpha")
	var checks atomic.Int32
	// The check at queue time passes (the first rename check); the one at send time finds the link revoked.
	srv, c, _, m := renameEnv(t, &title, func(string, string) bool { return checks.Add(1) == 1 })
	send(t, m, c, "spc_1", "one")
	title.Store("Beta")
	m.Renamed(context.Background(), "spc_1", "Beta")
	m.Forget("tgl_1")
	time.Sleep(150 * time.Millisecond)
	if n := checks.Load(); n < 2 {
		t.Fatalf("%d checks, want one at queue time and one at send time", n)
	}
	if n := len(srv.Calls("editForumTopic")); n != 0 {
		t.Fatalf("%d edits for a link revoked after the rename was queued, want 0", n)
	}
}

// A rename committed while a topic is being created ends with the new title: the create used the old one, and the
// title is read again once the topic is saved.
func TestRenameDuringCreateEndsWithTheNewTitle(t *testing.T) {
	var calls atomic.Int32
	srv, c, st, _ := setup(t)
	snd, err := sender.New(sender.Options{Client: c})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = snd.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	m := topics.New(topics.Options{Store: st, Mode: func(string) string { return settings.ThreadsOn },
		SpaceName: func(context.Context, string) string { // the first read is the create's; the space is renamed after it
			if calls.Add(1) == 1 {
				return "Alpha"
			}
			return "Beta"
		},
		MayRead: func(string, int64, string) bool { return true },
		Sender:  func() *sender.Sender { return snd }})
	msg := send(t, m, c, "spc_1", "one")
	if got := srv.Topics(chat)[msg.MessageThreadID]; got != "Beta" {
		t.Fatalf("the new topic is titled %q, want Beta", got)
	}
}

// Two renames in quick succession end with the latest name, whatever order they are sent in.
func TestQuickRenamesEndWithTheLatestName(t *testing.T) {
	var title atomic.Value
	title.Store("Alpha")
	srv, c, _, m := renameEnv(t, &title, nil)
	first := send(t, m, c, "spc_1", "one")
	title.Store("Beta")
	m.Renamed(context.Background(), "spc_1", "Beta")
	title.Store("Gamma")
	m.Renamed(context.Background(), "spc_1", "Beta") // a late call carrying a stale name: the title is read when sent
	waitFor(t, "the latest name", func() bool { return srv.Topics(chat)[first.MessageThreadID] == "Gamma" })
	time.Sleep(100 * time.Millisecond)
	if got := srv.Topics(chat)[first.MessageThreadID]; got != "Gamma" {
		t.Fatalf("the topic ends titled %q, want Gamma", got)
	}
}
