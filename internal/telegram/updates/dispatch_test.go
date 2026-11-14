package updates

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/obs/metrics"
	"github.com/aurimasniekis/go-spaceman/internal/obs/metrics/metricstest"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/tgtest"
)

func fakeBot(t *testing.T) (*tgtest.Server, *botapi.Client) {
	t.Helper()
	srv := tgtest.New(t, tgtest.Options{Limits: tgtest.Limits{Disabled: true}})
	c, err := botapi.New(botapi.Options{BaseURL: srv.URL(), Token: srv.Token()})
	if err != nil {
		t.Fatal(err)
	}
	return srv, c
}

// SM-TG-004: an unlinked user gets the linking instructions at most once an hour, at most one such reply goes out per
// second over every user, and nothing else happens for them: no handler call and no other Bot API call. The per-user
// memory is an LRU, so many users can't grow it without bound.
func TestDispatcherUnlinkedUsers(t *testing.T) {
	srv, bot := fakeBot(t)
	clk := clock.NewFake(time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC))
	m := metrics.New()
	rec := &recorder{}
	d := NewDispatcher(DispatcherOptions{Links: rec, Handler: rec, Bot: bot, Clock: clk, Metrics: m, UnlinkedUsers: 3})
	ctx := context.Background()
	replies := func(chat int64) int { return len(srv.Messages(chat)) }

	d.Dispatch(ctx, privateMsg(1, 101, "/start"))
	d.Dispatch(ctx, privateMsg(2, 101, "hello"))
	d.Dispatch(ctx, botapi.Update{UpdateID: 3, CallbackQuery: &botapi.CallbackQuery{ID: "cb", From: botapi.User{ID: 101}, Data: "x"}})
	if replies(101) != 1 {
		t.Fatalf("user 101 got %d replies, want 1", replies(101))
	}
	if got := srv.Messages(101)[0].Text; got != UnlinkedText {
		t.Fatalf("reply %q", got)
	}
	// Another user within the global second gets nothing yet, and still gets the reply once the second has passed.
	d.Dispatch(ctx, privateMsg(4, 102, "hi"))
	if replies(102) != 0 {
		t.Fatal("the global bound let a second reply through within a second")
	}
	clk.Advance(time.Second)
	d.Dispatch(ctx, privateMsg(5, 102, "hi"))
	if replies(102) != 1 {
		t.Fatalf("user 102 got %d replies after the global second", replies(102))
	}
	clk.Advance(30 * time.Minute)
	d.Dispatch(ctx, privateMsg(6, 101, "again"))
	if replies(101) != 1 {
		t.Fatal("a second reply within the hour")
	}
	clk.Advance(31 * time.Minute)
	d.Dispatch(ctx, privateMsg(7, 101, "again"))
	if replies(101) != 2 {
		t.Fatalf("user 101 got %d replies after the hour, want 2", replies(101))
	}
	// A membership change from an unlinked user (starting or blocking the bot) is never answered.
	clk.Advance(2 * time.Hour)
	d.Dispatch(ctx, botapi.Update{UpdateID: 8, MyChatMember: &botapi.ChatMemberUpdated{Chat: botapi.Chat{ID: 103, Type: botapi.ChatPrivate},
		From: botapi.User{ID: 103}, NewChatMember: botapi.ChatMember{Status: botapi.MemberKicked}}})
	if replies(103) != 0 {
		t.Fatal("a membership change was answered")
	}
	// The LRU holds at most 3 users.
	for i := range int64(5) {
		clk.Advance(time.Second)
		d.Dispatch(ctx, privateMsg(20+i, 200+i, "x"))
	}
	if n := d.replied.len(); n != 3 {
		t.Fatalf("LRU holds %d users, want 3", n)
	}
	for _, c := range srv.Calls() {
		if c.Method != "sendMessage" {
			t.Fatalf("unexpected Bot API call %s for an unlinked user", c.Method)
		}
	}
	if n := len(rec.ids()); n != 0 {
		t.Fatalf("the handler ran %d times for unlinked users", n)
	}
	if v := metricstest.Value(t, m, "spaceman_telegram_updates_total", "outcome", "unlinked_replied"); v != 8 {
		t.Fatalf("unlinked_replied %v", v)
	}
}

// SM-TG-004: v1 is private chats only. The bot leaves a group it is added to (and does nothing when it is removed),
// ignores messages and taps from groups, other bots and mismatched chats, and hands a linked user's update to the
// handler without replying itself. A failing send is counted, not fatal.
func TestDispatcherRouting(t *testing.T) {
	srv, bot := fakeBot(t)
	m := metrics.New()
	rec := &recorder{linked: map[int64]Link{5: {ID: "tgl_1", UserID: "usr_1", ChatID: 5}}}
	d := NewDispatcher(DispatcherOptions{Links: rec, Handler: rec, Bot: bot, Metrics: m})
	ctx := context.Background()
	group := botapi.Chat{ID: -100777, Type: botapi.ChatSupergroup, Title: "G"}
	member := func(id int64, status string, chat botapi.Chat) botapi.Update {
		return botapi.Update{UpdateID: id, MyChatMember: &botapi.ChatMemberUpdated{Chat: chat, From: botapi.User{ID: 5},
			NewChatMember: botapi.ChatMember{Status: status}}}
	}

	d.Dispatch(ctx, member(1, botapi.MemberMember, group))
	if calls := srv.Calls("leaveChat"); len(calls) != 1 || calls[0].Params["chat_id"] != "-100777" {
		t.Fatalf("leaveChat calls %+v", calls)
	}
	d.Dispatch(ctx, member(2, botapi.MemberLeft, group))
	d.Dispatch(ctx, member(3, botapi.MemberKicked, group))
	if n := len(srv.Calls("leaveChat")); n != 1 {
		t.Fatalf("left %d times", n)
	}
	inGroup := privateMsg(4, 5, "x")
	inGroup.Message.Chat = group
	fromBot := privateMsg(5, 6, "x")
	fromBot.Message.From.IsBot = true
	mismatched := privateMsg(6, 5, "x")
	mismatched.Message.Chat.ID = 9
	tapInGroup := botapi.Update{UpdateID: 7, CallbackQuery: &botapi.CallbackQuery{ID: "q", From: botapi.User{ID: 5}, Message: &botapi.Message{Chat: group}}}
	noSender := botapi.Update{UpdateID: 8, Message: &botapi.Message{Chat: botapi.Chat{ID: 5, Type: botapi.ChatPrivate}}}
	edited := botapi.Update{UpdateID: 9, EditedMessage: privateMsg(9, 5, "x").Message}
	for _, u := range []botapi.Update{inGroup, fromBot, mismatched, tapInGroup, noSender, edited, {UpdateID: 10}} {
		d.Dispatch(ctx, u)
	}
	if n := len(rec.ids()); n != 0 {
		t.Fatalf("ignored updates reached the handler: %v", rec.ids())
	}
	if v := metricstest.Value(t, m, "spaceman_telegram_updates_total", "outcome", "ignored"); v != 9 {
		t.Fatalf("ignored %v", v)
	}

	d.Dispatch(ctx, privateMsg(11, 5, "hello"))
	d.Dispatch(ctx, botapi.Update{UpdateID: 12, CallbackQuery: &botapi.CallbackQuery{ID: "q2", From: botapi.User{ID: 5},
		Message: &botapi.Message{Chat: botapi.Chat{ID: 5, Type: botapi.ChatPrivate}}}})
	d.Dispatch(ctx, member(13, botapi.MemberKicked, botapi.Chat{ID: 5, Type: botapi.ChatPrivate}))
	if got := rec.ids(); len(got) != 3 || got[0] != 11 || got[2] != 13 {
		t.Fatalf("linked updates handled %v", got)
	}
	if n := len(srv.Calls("sendMessage")); n != 0 {
		t.Fatalf("the dispatcher replied to a linked user itself (%d sends)", n)
	}

	// A failing reply or leave is counted.
	srv.FailNext("leaveChat", 400, "Bad Request: chat not found", 0)
	d.Dispatch(ctx, member(14, botapi.MemberAdministrator, group))
	srv.FailNext("sendMessage", 403, "Forbidden: bot was blocked by the user", 0)
	d.Dispatch(ctx, privateMsg(15, 300, "x"))
	if v := metricstest.Value(t, m, "spaceman_telegram_updates_total", "outcome", "failed"); v != 2 {
		t.Fatalf("failed %v", v)
	}
}

// Without links, a handler or a bot, nothing is sent and nothing fails: central before a bot is configured.
func TestDispatcherUnconfigured(t *testing.T) {
	m := metrics.New()
	d := NewDispatcher(DispatcherOptions{Metrics: m})
	d.Dispatch(context.Background(), privateMsg(1, 5, "x"))
	d.Dispatch(context.Background(), botapi.Update{UpdateID: 2, MyChatMember: &botapi.ChatMemberUpdated{Chat: botapi.Chat{ID: -1, Type: botapi.ChatGroup},
		NewChatMember: botapi.ChatMember{Status: botapi.MemberMember}}})
	if v := metricstest.Value(t, m, "spaceman_telegram_updates_total", "outcome", "unlinked_dropped"); v != 1 {
		t.Fatalf("unlinked_dropped %v", v)
	}
	if v := metricstest.Value(t, m, "spaceman_telegram_updates_total", "outcome", "ignored"); v != 1 {
		t.Fatalf("ignored %v", v)
	}
}

// A panicking handler is recovered and counted; the worker goes on with the next update.
func TestIntakeRecoversPanics(t *testing.T) {
	m := metrics.New()
	in := NewIntake(IntakeOptions{Workers: 1, Rate: -1, Metrics: m})
	var mu sync.Mutex
	var seen []int64
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go in.Run(ctx, func(_ context.Context, u botapi.Update) {
		if u.UpdateID == 1 {
			panic("boom")
		}
		mu.Lock()
		seen = append(seen, u.UpdateID)
		mu.Unlock()
	})
	in.Offer(privateMsg(1, 5, "x"))
	in.Offer(privateMsg(2, 5, "x"))
	waitFor(t, "update 2", func() bool { mu.Lock(); defer mu.Unlock(); return len(seen) == 1 })
	if v := metricstest.Value(t, m, "spaceman_telegram_updates_total", "outcome", "failed"); v != 1 {
		t.Fatalf("failed %v", v)
	}
}

// The dedupe ring remembers the most recent DedupeSize update_ids and forgets older ones.
func TestIntakeDedupeRing(t *testing.T) {
	in := NewIntake(IntakeOptions{QueueSize: 100, Workers: 1, DedupeSize: 3})
	for id := int64(1); id <= 4; id++ {
		if r := in.Offer(privateMsg(id, 5, "x")); r != Accepted {
			t.Fatalf("offer %d: %v", id, r)
		}
	}
	if r := in.Offer(privateMsg(4, 5, "x")); r != Duplicate {
		t.Fatalf("recent duplicate: %v", r)
	}
	if r := in.Offer(privateMsg(1, 5, "x")); r != Accepted {
		t.Fatalf("an ID older than the ring: %v", r)
	}
	if len(in.seen) != 3 {
		t.Fatalf("remembered %d IDs", len(in.seen))
	}
}

// SM-TG-004: the webhook (through the fake Bot API server's delivery) and polling feed the same queue and dispatcher:
// an update delivered by webhook and one polled after switching modes both reach the one handler, a duplicate across
// the switch is handled once, and polling confirms what it queued.
func TestWebhookAndPollingShareTheDispatcher(t *testing.T) {
	rec := &recorder{linked: map[int64]Link{5: {ID: "tgl_1", UserID: "usr_1", ChatID: 5}}}
	s := New(Options{Intake: IntakeOptions{Rate: -1}, Dispatcher: DispatcherOptions{Links: rec, Handler: rec}})
	mux := http.NewServeMux()
	mux.Handle(WebhookPath, s.Webhook)
	hs := httptest.NewServer(mux)
	defer hs.Close()
	cfg := testConfig(t)
	if err := s.Webhook.Set(cfg); err != nil {
		t.Fatal(err)
	}
	tg := tgtest.New(t, tgtest.Options{Limits: tgtest.Limits{Disabled: true}, WebhookTarget: hs.URL + WebhookPath + cfg.HookID})
	bot, err := botapi.New(botapi.Options{BaseURL: tg.URL(), Token: tg.Token()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)
	waitFor(t, "workers", s.Webhook.running.Load)

	// The fake sends the secret registered with setWebhook; a wrong one is refused.
	if code, err := tg.DeliverRaw(ctx, "nope", body(t, privateMsg(1, 5, "x"))); err != nil || code != http.StatusUnauthorized {
		t.Fatalf("wrong secret: %d %v", code, err)
	}
	if err := bot.SetWebhook(ctx, botapi.SetWebhookParams{URL: "https://central.example.com" + WebhookPath + cfg.HookID, SecretToken: cfg.Secret,
		AllowedUpdates: AllowedUpdates}); err != nil {
		t.Fatal(err)
	}
	if code, err := tg.Deliver(ctx, privateMsg(1, 5, "by webhook")); err != nil || code != http.StatusOK {
		t.Fatalf("webhook delivery: %d %v", code, err)
	}
	waitFor(t, "the webhook update", func() bool { return len(rec.ids()) == 1 })

	// Switch to polling: the webhook route goes dark and getUpdates feeds the same intake.
	if err := s.Webhook.Set(nil); err != nil {
		t.Fatal(err)
	}
	if err := bot.DeleteWebhook(ctx, false); err != nil {
		t.Fatal(err)
	}
	tg.PushUpdate(privateMsg(1, 5, "redelivered"))
	tg.PushUpdate(privateMsg(2, 5, "by polling"))
	pollCtx, stopPoll := context.WithCancel(ctx)
	polled := make(chan error, 1)
	go func() { polled <- s.Poll(pollCtx, bot, PollerOptions{Timeout: time.Second}) }()
	waitFor(t, "the polled update", func() bool { return len(rec.ids()) == 2 })
	waitFor(t, "polling to confirm", func() bool { return len(tg.Pending()) == 0 })
	stopPoll()
	if err := <-polled; !errors.Is(err, context.Canceled) {
		t.Fatalf("poll ended with %v", err)
	}
	if got := rec.ids(); got[0] != 1 || got[1] != 2 {
		t.Fatalf("handled %v", got)
	}
	if code, err := tg.DeliverRaw(ctx, cfg.Secret, body(t, privateMsg(3, 5, "x"))); err != nil || code != http.StatusNotFound {
		t.Fatalf("webhook in polling mode: %d %v", code, err)
	}
}

type scriptedGetter struct {
	mu      sync.Mutex
	offsets []int64
	answers []func() ([]botapi.Update, error)
}

func (g *scriptedGetter) GetUpdates(ctx context.Context, p botapi.GetUpdatesParams) ([]botapi.Update, error) {
	g.mu.Lock()
	g.offsets = append(g.offsets, p.Offset)
	if len(g.answers) == 0 {
		g.mu.Unlock()
		<-ctx.Done()
		return nil, ctx.Err()
	}
	a := g.answers[0]
	g.answers = g.answers[1:]
	g.mu.Unlock()
	return a()
}

// The poller backs off on errors (Telegram's retry_after when given, otherwise doubling from 1 s), confirms a batch
// only after queueing it, and waits instead of dropping when the queue is full.
func TestPollerBackoffAndBackpressure(t *testing.T) {
	m := metrics.New()
	in := NewIntake(IntakeOptions{QueueSize: 1, Workers: 1, Metrics: m})
	g := &scriptedGetter{answers: []func() ([]botapi.Update, error){
		func() ([]botapi.Update, error) {
			return nil, &botapi.Error{Method: "getUpdates", Code: 429, RetryAfter: 7 * time.Second}
		},
		func() ([]botapi.Update, error) { return nil, errors.New("connection reset") },
		func() ([]botapi.Update, error) { return nil, errors.New("connection reset") },
		func() ([]botapi.Update, error) {
			return []botapi.Update{privateMsg(10, 5, "a"), privateMsg(11, 5, "b")}, nil
		},
	}}
	var mu sync.Mutex
	var sleeps []time.Duration
	p := NewPoller(g, in, PollerOptions{Metrics: m, Sleep: func(_ context.Context, d time.Duration) error {
		mu.Lock()
		sleeps = append(sleeps, d)
		mu.Unlock()
		return nil
	}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()
	// No worker runs: update 10 fills the queue and the poller waits on 11 without asking Telegram for more.
	waitFor(t, "update 10 queued", func() bool { return len(in.shards[0]) == 1 })
	time.Sleep(20 * time.Millisecond)
	g.mu.Lock()
	calls := len(g.offsets)
	g.mu.Unlock()
	if calls != 4 {
		t.Fatalf("getUpdates called %d times while the queue was full, want 4", calls)
	}
	<-in.shards[0] // make room: 11 goes in and the next call confirms both
	waitFor(t, "the confirming call", func() bool { g.mu.Lock(); defer g.mu.Unlock(); return len(g.offsets) == 5 })
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run ended with %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(sleeps) != 3 || sleeps[0] != 7*time.Second || sleeps[1] != time.Second || sleeps[2] != 2*time.Second {
		t.Fatalf("sleeps %v, want [7s 1s 2s]", sleeps)
	}
	if g.offsets[3] != 0 || g.offsets[4] != 12 {
		t.Fatalf("offsets %v", g.offsets)
	}
	if v := metricstest.Value(t, m, "spaceman_telegram_poll_errors_total"); v != 3 {
		t.Fatalf("poll errors %v", v)
	}
}
