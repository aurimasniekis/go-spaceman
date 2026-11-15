package topics_test

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/sender"
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
		return topics.New(topics.Options{Store: st, Enabled: func(id string) bool { return id != "off" },
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

func TestRoute(t *testing.T) {
	_, _, _, mk := setup(t)
	m := mk()
	for _, c := range []struct {
		space string
		needs bool
		want  string
	}{{"spc_1", false, "spc_1"}, {"spc_1", true, topics.Needs}, {"off", true, ""}, {"off", false, ""}} {
		if got := m.Route(c.space, c.needs); got != c.want {
			t.Errorf("Route(%s,%v) = %q, want %q", c.space, c.needs, got, c.want)
		}
	}
	var nilM *topics.Manager
	if nilM.Route("spc_1", true) != "" {
		t.Error("a nil Manager routes to the main chat")
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
	n := send(t, m, c, topics.Needs, "three")
	if srv.Topics(chat)[n.MessageThreadID] != topics.NeedsName {
		t.Errorf("needs topic %v", srv.Topics(chat))
	}
	if d := send(t, mk(), c, "spc_1", "four"); d.MessageThreadID != a.MessageThreadID || len(srv.Calls("createForumTopic")) != 2 {
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

// SpaceOf answers only from stored threads, and never for Needs-you or an unknown thread.
func TestSpaceOf(t *testing.T) {
	_, c, _, mk := setup(t)
	m := mk()
	a := send(t, m, c, "spc_1", "one").MessageThreadID
	n := send(t, m, c, topics.Needs, "two").MessageThreadID
	ctx := context.Background()
	if id, ok := mk().SpaceOf(ctx, "tgl_1", a); !ok || id != "spc_1" { // a new Manager: read from the store
		t.Errorf("SpaceOf(%d) = %q, %v", a, id, ok)
	}
	for _, th := range []int{n, 0, 12345} {
		if id, ok := m.SpaceOf(ctx, "tgl_1", th); ok {
			t.Errorf("SpaceOf(%d) = %q, want none", th, id)
		}
	}
	if _, ok := m.SpaceOf(ctx, "tgl_other", a); ok {
		t.Error("another link's lookup must not see this link's topics")
	}
}

// Inside a sender, the topic creation is a second Bot API call, so it is charged against the chat's bucket like any other.
func TestTopicCreationIsChargedThroughTake(t *testing.T) {
	_, c, _, mk := setup(t)
	clk := clock.NewFake(time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC))
	s, err := sender.New(sender.Options{Client: c, Workers: 1, Clock: clk, After: func(d time.Duration) <-chan time.Time {
		clk.Advance(d)
		ch := make(chan time.Time, 1)
		ch <- clk.Now()
		return ch
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.Run(ctx) }()
	m := mk()
	start := clk.Now()
	done := make(chan error, 1)
	err = s.Enqueue(sender.Message{ChatID: chat, Priority: sender.NeedsYou, Done: func(err error) { done <- err },
		Send: func(ctx context.Context, c *botapi.Client) error {
			// The dispatch and two Takes use the chat's burst of 3. The creation is the free first call of Send, so
			// it goes out at once; the SendMessage after it is the call that waits.
			for range 2 {
				if err := sender.Take(ctx); err != nil {
					return err
				}
			}
			_, err := m.Send(ctx, c, "tgl_1", chat, "spc_1", botapi.SendMessageParams{Text: "x"})
			return err
		}})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("send did not finish")
	}
	// The SendMessage after the free creation finds an empty bucket refilling at 1/s: it waits a second.
	if el := clk.Now().Sub(start); el < time.Second {
		t.Errorf("creation + send took %v of virtual time, want at least 1s", el)
	}
}

// gated starts a sender whose waits block until release is closed, so a Take can be held mid-wait.
func gated(t *testing.T, c *botapi.Client, release <-chan struct{}, hold *atomic.Bool) *sender.Sender {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC))
	s, err := sender.New(sender.Options{Client: c, Workers: 1, Clock: clk, After: func(d time.Duration) <-chan time.Time {
		ch := make(chan time.Time, 1)
		go func() {
			if hold.Load() {
				<-release
			}
			clk.Advance(d)
			ch <- clk.Now()
		}()
		return ch
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = s.Run(ctx) }()
	return s
}

// enqueueRecreate queues a send that drains the chat's burst, so that when the topic is gone the re-creation waits in Take.
func enqueueRecreate(t *testing.T, s *sender.Sender, m *topics.Manager, hold *atomic.Bool, started chan<- struct{}, done chan<- error) {
	t.Helper()
	err := s.Enqueue(sender.Message{ChatID: chat, Priority: sender.NeedsYou, Done: func(err error) { done <- err },
		Send: func(ctx context.Context, c *botapi.Client) error {
			for range 2 {
				if err := sender.Take(ctx); err != nil {
					return err
				}
			}
			hold.Store(true) // the drain waits above passed; from here a wait blocks
			started <- struct{}{}
			_, err := m.Send(ctx, c, "tgl_1", chat, "spc_1", botapi.SendMessageParams{Text: "x"})
			return err
		}})
	if err != nil {
		t.Fatal(err)
	}
}

// A topic creation waiting on the buckets must not hold the link's lock: SpaceOf (called for every topic message) answers.
func TestSpaceOfDoesNotBlockWhileCreationWaits(t *testing.T) {
	srv, c, _, mk := setup(t)
	m := mk()
	first := send(t, m, c, "spc_1", "one").MessageThreadID
	other := send(t, m, c, "spc_2", "two").MessageThreadID
	srv.DeleteTopic(chat, first)
	release := make(chan struct{})
	var hold atomic.Bool
	s := gated(t, c, release, &hold)
	started, done := make(chan struct{}, 1), make(chan error, 1)
	enqueueRecreate(t, s, m, &hold, started, done)
	<-started
	time.Sleep(100 * time.Millisecond) // let the send reach the wait
	answered := make(chan string, 1)
	go func() { id, _ := m.SpaceOf(context.Background(), "tgl_1", other); answered <- id }()
	select {
	case id := <-answered:
		if id != "spc_2" {
			t.Errorf("SpaceOf = %q", id)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("SpaceOf blocked behind a topic creation that waits on the buckets")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// Two sends that both find the topic gone and wait for the buckets create one topic between them.
func TestConcurrentRecreationsWaitingInTakeCreateOneTopic(t *testing.T) {
	srv, c, _, mk := setup(t)
	m := mk()
	first := send(t, m, c, "spc_1", "one").MessageThreadID
	srv.DeleteTopic(chat, first)
	release := make(chan struct{})
	started, done := make(chan struct{}, 2), make(chan error, 2)
	// Two senders (one per message), so both sends run at once and both wait.
	var hold1, hold2 atomic.Bool // one per sender: a sender's drain waits must pass while the other one is held
	enqueueRecreate(t, gated(t, c, release, &hold1), m, &hold1, started, done)
	enqueueRecreate(t, gated(t, c, release, &hold2), m, &hold2, started, done)
	<-started
	<-started
	time.Sleep(100 * time.Millisecond)
	close(release)
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if n := len(srv.Calls("createForumTopic")); n != 2 { // the first one, and one re-creation
		t.Errorf("%d topics created, want 2", n)
	}
}
