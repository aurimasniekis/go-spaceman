package app_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/tgtest"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/updates"
)

func tgRandom(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// SM-TG-004: central's public handler serves the Telegram webhook as its own route group, outside the API handler:
// 404 until a bot is configured; then a wrong secret is refused with 401 and audited (once a minute, without the
// token), a body over 256 KiB is 413 (declared or chunked), a valid update of about 200 KiB is accepted, and a valid update needs no session, CSRF token or Origin (the API's middleware
// doesn't apply). An update from an unlinked user reaches the dispatcher, which sends only the linking instructions.
func TestTelegramWebhookRoute(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.c.TelegramUpdates.Run(ctx)

	hookID, secret := tgRandom(t, 16), tgRandom(t, 32)
	url := h.srv.URL + updates.WebhookPath + hookID
	tg := tgtest.New(t, tgtest.Options{Limits: tgtest.Limits{Disabled: true}, WebhookTarget: url})
	update := func(id int64) []byte {
		b, err := json.Marshal(botapi.Update{UpdateID: id, Message: &botapi.Message{MessageID: 1, From: &botapi.User{ID: 4242, FirstName: "Stranger"},
			Chat: botapi.Chat{ID: 4242, Type: botapi.ChatPrivate}, Text: "hello"}})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	deliver := func(secret string, b []byte) int {
		t.Helper()
		code, err := tg.DeliverRaw(ctx, secret, b)
		if err != nil {
			t.Fatal(err)
		}
		return code
	}

	if code := deliver(secret, update(1)); code != http.StatusNotFound {
		t.Fatalf("before a bot is configured: %d", code)
	}
	bot, err := botapi.New(botapi.Options{BaseURL: tg.URL(), Token: tg.Token()})
	if err != nil {
		t.Fatal(err)
	}
	h.c.TelegramUpdates.Dispatcher.SetBot(bot)
	if err := h.c.TelegramUpdates.Webhook.Set(&updates.WebhookConfig{HookID: hookID, Secret: secret}); err != nil {
		t.Fatal(err)
	}

	for range 3 {
		if code := deliver(tgRandom(t, 32), update(2)); code != http.StatusUnauthorized {
			t.Fatalf("wrong secret: %d", code)
		}
	}
	var n int
	var details string
	if err := h.c.Store.ReadDB().QueryRowContext(ctx, `SELECT count(*), coalesce(max(details_json), '') FROM audit_events WHERE action = 'telegram.webhook.refused' AND outcome = 'denied'`).
		Scan(&n, &details); err != nil || n != 1 {
		t.Fatalf("refusal audit rows: %d %v", n, err)
	}
	if strings.Contains(details, secret) {
		t.Fatal("the audit row holds the secret")
	}
	if code := deliver(secret, bytes.Repeat([]byte(" "), updates.MaxBodyBytes+1)); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body: %d", code)
	}
	// A chunked body declares no length, so only the streamed limit can refuse it.
	chunked, err := http.NewRequestWithContext(ctx, http.MethodPost, url, io.MultiReader(bytes.NewReader(update(2)),
		bytes.NewReader(bytes.Repeat([]byte(" "), 640<<10))))
	if err != nil {
		t.Fatal(err)
	}
	chunked.ContentLength, chunked.TransferEncoding = -1, []string{"chunked"}
	chunked.Header.Set("Content-Type", "application/json")
	chunked.Header.Set(updates.SecretHeader, secret)
	resp, err := http.DefaultClient.Do(chunked)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("chunked 640 KiB body: %d", resp.StatusCode)
	}
	// The fake posts with no cookie, CSRF token or Origin: the API's same-origin check would refuse it.
	if code := deliver(secret, update(3)); code != http.StatusOK {
		t.Fatalf("valid update: %d", code)
	}
	// A large valid update takes several reads; the body's read deadline is on the real clock, not central's (fake)
	// one, which is days behind here.
	large, err := json.Marshal(botapi.Update{UpdateID: 5, Message: &botapi.Message{MessageID: 2, From: &botapi.User{ID: 4243, FirstName: "Other"},
		Chat: botapi.Chat{ID: 4243, Type: botapi.ChatPrivate}, Text: strings.Repeat("a", 200<<10)}})
	if err != nil {
		t.Fatal(err)
	}
	if code := deliver(secret, large); code != http.StatusOK {
		t.Fatalf("valid %d KiB update: %d", len(large)>>10, code)
	}
	deadline := time.Now().Add(10 * time.Second)
	for len(tg.Messages(4242)) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the unlinked user got no instructions")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if msgs := tg.Messages(4242); len(msgs) != 1 || msgs[0].Text != updates.UnlinkedText {
		t.Fatalf("messages to the unlinked user: %+v", msgs)
	}
	for _, c := range tg.Calls() {
		if c.Method != "sendMessage" {
			t.Fatalf("unexpected Bot API call %s", c.Method)
		}
	}
	// Other paths under /telegram/ aren't the webhook.
	resp, err = http.Post(h.srv.URL+updates.WebhookPath, "application/json", bytes.NewReader(update(4)))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("the bare prefix: %d", resp.StatusCode)
	}
}
