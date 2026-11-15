package topics_test

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
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
