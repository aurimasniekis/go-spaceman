package updates

import (
	"context"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/obs/metrics"
	"github.com/aurimasniekis/go-spaceman/internal/obs/metrics/metricstest"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
)

// allowed tries n units of k for link and counts those allowed.
func allowed(lm *Limiter, link string, k Kind, n int) int {
	ok := 0
	for range n {
		if lm.Allow(link, k) {
			ok++
		}
	}
	return ok
}

func TestLimiterBuckets(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC))
	lm := NewLimiter(InboundLimits{}, clk)
	if lm.Limits() != DefaultInboundLimits() {
		t.Fatalf("zero limits %+v, want the defaults", lm.Limits())
	}
	// Actions: a burst of 10, then 30/min (one every 2 s).
	for i := range 10 {
		if !lm.Allow("tgl_a", KindAction) {
			t.Fatalf("action %d refused within the burst", i)
		}
	}
	if lm.Allow("tgl_a", KindAction) {
		t.Fatal("the 11th action in a burst was allowed")
	}
	if !lm.Allow("tgl_b", KindAction) {
		t.Fatal("another link shares the first one's bucket")
	}
	clk.Advance(2 * time.Second)
	if n := allowed(lm, "tgl_a", KindAction, 2); n != 1 {
		t.Fatalf("%d actions allowed 2 s after the burst, want 1", n)
	}
	// Inbox items and downloads are counted apart from actions and per hour.
	for range 60 {
		if !lm.Allow("tgl_a", KindInboxItem) {
			t.Fatal("inbox item refused within 60/h")
		}
	}
	if lm.Allow("tgl_a", KindInboxItem) {
		t.Fatal("the 61st inbox item in an hour was allowed")
	}
	for range 50 {
		lm.Allow("tgl_a", KindDownload)
	}
	if lm.Allow("tgl_a", KindDownload) {
		t.Fatal("the 51st download in an hour was allowed")
	}
	clk.Advance(time.Minute)
	if !lm.Allow("tgl_a", KindInboxItem) {
		t.Fatal("an inbox item slot didn't come back after a minute")
	}

	// New limits apply at once to existing links, without a restart.
	lm.SetLimits(InboundLimits{ActionsPerMinute: 60, ActionsBurst: 2, InboxItemsPerHour: 1, DownloadsPerHour: 1})
	clk.Advance(time.Hour)
	if n := allowed(lm, "tgl_a", KindAction, 3); n != 2 {
		t.Fatalf("%d of 3 actions allowed, want the new burst of 2", n)
	}
	clk.Advance(time.Second)
	if !lm.Allow("tgl_a", KindAction) {
		t.Fatal("the new rate of 1/s didn't apply")
	}
	if n := allowed(lm, "tgl_a", KindInboxItem, 2); n != 1 {
		t.Fatalf("%d inbox items allowed, want the new limit of 1/h", n)
	}

	// Forget and idle sweeping keep memory bounded.
	lm.Forget("tgl_b")
	clk.Advance(3 * time.Hour)
	lm.Allow("tgl_c", KindAction)
	lm.mu.Lock()
	n := len(lm.links)
	lm.mu.Unlock()
	if n != 1 {
		t.Fatalf("%d links remembered after the idle sweep, want 1", n)
	}
}

// Plan §3.5: a linked user over the action limit gets a single "slow down" answer, and the update isn't handled. A
// button tap is still answered (Telegram requires it) but without text after the first refusal.
func TestDispatcherThrottlesLinkedUsers(t *testing.T) {
	srv, bot := fakeBot(t)
	clk := clock.NewFake(time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC))
	m := metrics.New()
	rec := &recorder{linked: map[int64]Link{5: {ID: "tgl_1", UserID: "usr_1", ChatID: 5}}}
	lm := NewLimiter(InboundLimits{ActionsPerMinute: 60, ActionsBurst: 2}, clk)
	d := NewDispatcher(DispatcherOptions{Links: rec, Handler: rec, Bot: bot, Clock: clk, Metrics: m, Limiter: lm})
	ctx := context.Background()
	tap := func(id int64, qid string) botapi.Update {
		u := botapi.Update{UpdateID: id, CallbackQuery: &botapi.CallbackQuery{ID: qid, From: botapi.User{ID: 5}, Data: "x",
			Message: &botapi.Message{Chat: botapi.Chat{ID: 5, Type: botapi.ChatPrivate}}}}
		srv.PushUpdate(u)
		return u
	}

	d.Dispatch(ctx, privateMsg(1, 5, "a"))
	d.Dispatch(ctx, privateMsg(2, 5, "b"))
	d.Dispatch(ctx, privateMsg(3, 5, "c")) // over the burst: one slow-down message
	d.Dispatch(ctx, privateMsg(4, 5, "d")) // still over: nothing more
	d.Dispatch(ctx, tap(5, "q1"))          // a tap gets its answer, without text now
	if got := rec.ids(); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("handled %v, want only the first two", got)
	}
	msgs := srv.Messages(5)
	if len(msgs) != 1 || msgs[0].Text != SlowDownText {
		t.Fatalf("messages %+v, want one slow-down notice", msgs)
	}
	if a := srv.CallbackAnswers(); len(a) != 1 || a[0].Text != "" {
		t.Fatalf("callback answers %+v", a)
	}
	if v := metricstest.Value(t, m, "spaceman_telegram_updates_total", "outcome", "throttled"); v != 3 {
		t.Fatalf("throttled %v", v)
	}

	// After an allowed action, the next refusal warns again; for a tap, in its answer.
	clk.Advance(time.Second)
	d.Dispatch(ctx, privateMsg(6, 5, "e"))
	d.Dispatch(ctx, tap(7, "q2"))
	if a := srv.CallbackAnswers(); len(a) != 2 || a[1].Text != SlowDownText {
		t.Fatalf("callback answers %+v", a)
	}
	if got := rec.ids(); len(got) != 3 {
		t.Fatalf("handled %v", got)
	}
	if len(srv.Messages(5)) != 1 {
		t.Fatal("a throttled tap sent a message")
	}
}

func TestIntakeSetRate(t *testing.T) {
	t.Parallel()
	in := NewIntake(IntakeOptions{Rate: 20})
	in.SetRate(5)
	if in.limiter.Limit() != 5 || in.limiter.Burst() != 5 {
		t.Fatalf("limit %v burst %d", in.limiter.Limit(), in.limiter.Burst())
	}
	in.SetRate(0) // ignored
	if in.limiter.Limit() != 5 {
		t.Fatal("a zero rate was applied")
	}
	NewIntake(IntakeOptions{Rate: -1}).SetRate(5) // unlimited: nothing to change, no panic
}
