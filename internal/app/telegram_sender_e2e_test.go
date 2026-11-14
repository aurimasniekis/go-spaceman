package app_test

import (
	"context"
	"errors"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/config"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/sender"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/tgtest"
)

// SM-TG-009, plan §5.2 TG-3b/TG-3c: the send limits of the settings reach the outbound sender, whichever came first.
// The limits are set before any bot exists; the sender made by a later put-telegram-bot starts with them, a later
// settings change reaches it, and a sender made by a reconfiguration (another mode) starts with the newest ones.
func TestTelegramSendLimitsReachTheSender(t *testing.T) {
	t.Parallel()
	tg := tgtest.New(t, tgtest.Options{Limits: tgtest.Limits{Disabled: true}})
	h := newHarnessWith(t, func(c *config.Config) { c.Telegram.APIBaseURL = tg.URL() })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.c.TelegramUpdates.Run(ctx)
	go h.c.TelegramSettings.Run(ctx)
	h.c.TelegramBot.Start(ctx) // no bot yet: no sender
	if h.c.TelegramBot.Sender() != nil {
		t.Fatal("a sender exists without a bot")
	}

	rootSecret, _, err := h.c.Auth.IssueRootToken(ctx)
	if err != nil {
		t.Fatal(err)
	}
	root := h.client()
	root.bearer = rootSecret
	limits := func(global float64, perChat int) map[string]any {
		return map[string]any{"send_limits": map[string]any{"global_per_second": global, "chat_per_second": 0.5, "chat_burst": 2,
			"group_per_minute": 10, "queue_total": 1000, "queue_per_chat": perChat}}
	}
	want := func(global float64, perChat int) sender.Limits {
		return sender.Limits{GlobalPerSecond: global, ChatPerSecond: 0.5, ChatBurst: 2, GroupPerMinute: 10, QueueTotal: 1000, QueuePerChat: perChat}
	}
	atLimits := func(l sender.Limits) func() bool {
		return func() bool { s := h.c.TelegramBot.Sender(); return s != nil && s.Limits() == l }
	}

	// 1. The limits change while there is no bot.
	expect(t, root.do("PUT", "/telegram/settings", limits(10, 20)), 200)
	eventually(t, "the settings are applied", func() bool { return h.c.TelegramSettings.Current().SendLimits.GlobalPerSecond == 10 })

	// 2. The bot is configured after startup: its sender starts with those limits.
	expect(t, root.do("PUT", "/telegram/bot", map[string]any{"bot_token": tg.Token(), "enabled": true, "mode": "polling"}), 200)
	first := h.c.TelegramBot.Sender()
	if first == nil || first.Limits() != want(10, 20) {
		t.Fatalf("the sender made after the limits were set has %+v", first)
	}

	// 3. A later change reaches the running sender, once (the sender is the same one).
	expect(t, root.do("PUT", "/telegram/settings", limits(5, 30), "If-Match", `"1"`), 200)
	eventually(t, "the change reaches the sender", atLimits(want(5, 30)))
	if h.c.TelegramBot.Sender() != first {
		t.Fatal("a settings change replaced the sender")
	}

	// 4. A reconfiguration replaces the sender; the new one has the newest limits.
	expect(t, root.do("PUT", "/telegram/bot", map[string]any{"enabled": true, "mode": "polling", "miniapp_host": "tg.example.com"}, "If-Match", `"1"`), 200)
	second := h.c.TelegramBot.Sender()
	// The first sender was stopped, not just dropped: its Run returned, so it refuses work.
	late := sender.Message{ChatID: 1, Priority: sender.Interactive, Send: func(context.Context, *botapi.Client) error { return nil }}
	if err := first.Enqueue(late); !errors.Is(err, sender.ErrClosed) {
		t.Fatalf("the replaced sender still accepts work (enqueue: %v)", err)
	}
	if second == nil || second == first || second.Limits() != want(5, 30) {
		t.Fatalf("the replacement sender has %+v (same as before: %v)", second, second == first)
	}

	// 5. Turning the bot off removes the sender.
	expect(t, root.do("DELETE", "/telegram/bot", nil, "If-Match", `"2"`), 204)
	if h.c.TelegramBot.Sender() != nil {
		t.Fatal("a sender outlived the bot")
	}
}
