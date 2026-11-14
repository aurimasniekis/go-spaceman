package tgbot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/eventbus"
	"github.com/aurimasniekis/go-spaceman/internal/keyring/keyringtest"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/storetest"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/tgtest"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/updates"
)

var admin = audit.Actor{Type: audit.ActorUser, ID: "usr_admin"}

const publicURL = "https://central.example.com"

// lockedBuffer collects the service's logs (every level, the Bot API client's included).
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

type env struct {
	t    *testing.T
	svc  *Service
	s    *store.Store
	up   *updates.Service
	tg   *tgtest.Server
	clk  *clock.Fake
	logs *lockedBuffer
	bus  *eventbus.Bus
}

// newEnv builds the service over a fresh store, a running inbound side and a fake Bot API whose webhook deliveries
// reach the inbound side's webhook at the path setWebhook registered (the registered host is not reachable).
func newEnv(t *testing.T, public string) *env {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC))
	s := storetest.Open(t, storetest.WithClock(clk))
	kr := keyringtest.Load(t, s)
	e := &env{t: t, s: s, clk: clk, logs: &lockedBuffer{}, bus: eventbus.New()}
	log := slog.New(slog.NewJSONHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	e.up = updates.New(updates.Options{Intake: updates.IntakeOptions{Log: log}, Webhook: updates.WebhookOptions{Log: log},
		Dispatcher: updates.DispatcherOptions{Log: log}})
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls := e.tg.Calls("setWebhook")
		if len(calls) == 0 {
			http.NotFound(w, r)
			return
		}
		u, _ := url.Parse(calls[len(calls)-1].Params["url"])
		r.URL.Path = u.Path
		e.up.Webhook.ServeHTTP(w, r)
	}))
	t.Cleanup(hook.Close)
	e.tg = tgtest.New(t, tgtest.Options{Limits: tgtest.Limits{Disabled: true}, WebhookTarget: hook.URL, Clock: clk})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); e.up.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	e.svc = New(s, audit.NewWriter(kr), kr, Options{BaseURL: e.tg.URL(), PublicURL: public, CallTimeout: 5 * time.Second, Updates: e.up,
		PollerOptions: updates.PollerOptions{Timeout: time.Second}, Bus: e.bus, Log: log})
	return e
}

func (e *env) put(version int64, in Input) *Bot {
	e.t.Helper()
	b, err := e.svc.Put(context.Background(), admin, version, in)
	if err != nil {
		e.t.Fatal(err)
	}
	return b
}

// noToken fails if the token appears in any of the texts.
func noToken(t *testing.T, token string, texts ...string) {
	t.Helper()
	secretPart := token[strings.IndexByte(token, ':')+1:]
	for _, s := range texts {
		if strings.Contains(s, secretPart) {
			t.Fatalf("the bot token is in %q", s)
		}
	}
}

func appErr(t *testing.T, err error, want apperr.Code) *apperr.Error {
	t.Helper()
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code != want {
		t.Fatalf("error %v, want %s", err, want)
	}
	return ae
}

func fieldMsg(t *testing.T, err error, field, want string) {
	t.Helper()
	ae := appErr(t, err, apperr.CodeInvalid)
	for _, f := range ae.Fields {
		if f.Field == field && strings.Contains(f.Message, want) {
			return
		}
	}
	t.Fatalf("fields %+v, want %s ~ %q", ae.Fields, field, want)
}

func (e *env) query(q string, args ...any) string {
	e.t.Helper()
	var out string
	if err := e.s.ReadDB().QueryRowContext(context.Background(), q, args...).Scan(&out); err != nil {
		e.t.Fatalf("%s: %v", q, err)
	}
	return out
}

// SM-TG-014 (plan §3.6): the token is sealed and write-only. It is checked with getMe, then never in an answer, the
// stored row (in clear), an audit entry or a log line (the client's per-call debug lines included); reads show only
// has_token, token_version, the bot's ID and username. The webhook secret is generated and sealed too.
func TestTokenIsSealedAndWriteOnly(t *testing.T) {
	t.Parallel()
	e := newEnv(t, publicURL)
	ctx := context.Background()
	token := e.tg.Token()
	b := e.put(0, Input{BotToken: token, Enabled: true})
	if !b.HasToken || b.TokenVersion != 1 || b.BotID != e.tg.Bot().ID || b.Username != "spaceman_test_bot" || b.Mode != ModeWebhook || b.Version != 1 {
		t.Fatalf("bot %+v", b)
	}
	if len(e.tg.Calls("getMe")) == 0 {
		t.Fatal("the token was not checked with getMe")
	}
	got, err := e.svc.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	out1, _ := json.Marshal(b)
	out2, _ := json.Marshal(got)
	secret := e.tg.WebhookSecret()
	if len(secret) != 43 {
		t.Fatalf("webhook secret of %d characters", len(secret))
	}
	stored := e.query(`SELECT CAST(token_enc AS TEXT) || CAST(webhook_secret_enc AS TEXT) FROM telegram_bot`)
	audits := e.query(`SELECT group_concat(details_json, ' ') FROM audit_events WHERE action LIKE 'telegram.bot.%'`)
	noToken(t, token, string(out1), string(out2), stored, audits, e.logs.String())
	for _, s := range []string{string(out1), string(out2), stored, audits, e.logs.String()} {
		if strings.Contains(s, secret) {
			t.Fatalf("the webhook secret is in %q", s)
		}
	}
	if !strings.Contains(audits, `"token_changed":true`) {
		t.Fatalf("audit %s", audits)
	}
	if !strings.Contains(e.logs.String(), "getMe") {
		t.Fatal("the client's debug lines were not captured: the log check proves nothing")
	}
	// A sealed value opens only for its own column.
	if _, err := e.svc.kr.Open("credential", secretAAD, []byte(e.query(`SELECT CAST(token_enc AS TEXT) FROM telegram_bot`))); err == nil {
		t.Fatal("the token opened as the webhook secret")
	}

	// An update without a token keeps it and its version; a new one bumps the version and rotates the secret.
	b = e.put(1, Input{Enabled: true, Mode: ModeWebhook})
	if b.TokenVersion != 1 || e.tg.WebhookSecret() != secret {
		t.Fatalf("an update without a token changed it: %+v", b)
	}
	b = e.put(2, Input{BotToken: token, Enabled: true})
	if b.TokenVersion != 2 || e.tg.WebhookSecret() == secret {
		t.Fatalf("a new token: version %d, secret rotated %v", b.TokenVersion, e.tg.WebhookSecret() != secret)
	}
}

// SM-TG-014, SM-TG-013 (plan §3.4.1, §3.5, §5.3): every bot setting is validated before anything is stored or sent to Telegram.
func TestPutValidation(t *testing.T) {
	t.Parallel()
	e := newEnv(t, publicURL)
	ctx := context.Background()
	token := e.tg.Token()
	put := func(in Input) error {
		_, err := e.svc.Put(ctx, admin, 0, in)
		return err
	}
	fieldMsg(t, put(Input{}), "bot_token", "is required")
	fieldMsg(t, put(Input{BotToken: "not-a-token"}), "bot_token", "is not a bot token")
	fieldMsg(t, put(Input{BotToken: token, Mode: "push"}), "mode", "webhook or polling")
	for host, want := range map[string]string{
		"192.0.2.1":                              "not an IP address",
		"[2001:db8::1]":                          "not an IP address",
		"2001:db8::1":                            "not an IP address",
		"central.example.com":                    "must differ from the host of central's public URL",
		"tg.example.com:8443":                    "without scheme, port or path",
		"https://tg.example.com":                 "without scheme, port or path",
		"tg.example.com/app":                     "without scheme, port or path",
		"TG.example.com":                         "lower case",
		"localhost":                              "fully qualified",
		"tg..example.com":                        "not a valid DNS name",
		"-tg.example.com":                        "not a valid DNS name",
		"tg_app.example.com":                     "not a valid DNS name",
		"127.1":                                  "must not end in a number",
		"tg.123":                                 "must not end in a number",
		"tg.example.0x7f":                        "must not end in a number",
		"tg.example.0x":                          "must not end in a number",
		"0x7f.1":                                 "must not end in a number",
		"0177.0.0.1.07":                          "must not end in a number",
		strings.Repeat("a", 64) + ".example.com": "not a valid DNS name",
	} {
		fieldMsg(t, put(Input{BotToken: token, MiniAppHost: host}), "miniapp_host", want)
	}
	for _, c := range []struct {
		list []string
		want string
	}{
		{[]string{"149.154.160.1"}, "is not a CIDR"},
		{[]string{"149.154.160.1/20"}, `use "149.154.160.0/20"`},
		{[]string{"149.154.160.0/20", "149.154.160.0/20"}, "listed twice"},
		{make([]string, MaxAllowlist+1), "more than 32"},
		{[]string{"::ffff:149.154.160.0/116"}, "IPv4-mapped"},
		{[]string{"149.154.160.0/20", "::ffff:0:0/96"}, "IPv4-mapped"},
	} {
		fieldMsg(t, put(Input{BotToken: token, WebhookIPAllowlist: c.list}), "webhook_ip_allowlist", c.want)
	}
	if len(e.tg.Calls()) != 0 {
		t.Fatalf("an invalid input reached Telegram: %d calls", len(e.tg.Calls()))
	}

	// A token Telegram refuses is refused without being stored; Telegram unreachable is "unavailable".
	e.tg.FailNext("getMe", http.StatusUnauthorized, "Unauthorized", 0)
	fieldMsg(t, put(Input{BotToken: token}), "bot_token", "Telegram refused the token")
	e.tg.FailNext("getMe", http.StatusBadGateway, "Bad Gateway", 0)
	err := put(Input{BotToken: token})
	_ = appErr(t, err, apperr.CodeUnavailable)
	noToken(t, token, err.Error())
	if _, err := e.svc.Get(ctx); apperr.CodeOf(err) != apperr.CodeNotFound {
		t.Fatalf("a refused token was stored: %v", err)
	}

	// Webhook mode needs an https public URL; polling doesn't.
	h := newEnv(t, "http://127.0.0.1:8080")
	_, err = h.svc.Put(ctx, admin, 0, Input{BotToken: h.tg.Token(), Enabled: true})
	fieldMsg(t, err, "mode", "public_url to be https")
	if _, err := h.svc.Put(ctx, admin, 0, Input{BotToken: h.tg.Token(), Enabled: true, Mode: ModePolling, MiniAppHost: "tg.localhost"}); err != nil {
		t.Fatal(err)
	}

	// Valid values are stored in canonical order (a set in Terraform).
	b := e.put(0, Input{BotToken: token, MiniAppHost: "tg.central.example.com", WebhookIPAllowlist: []string{"91.108.4.0/22", "149.154.160.0/20", "2001:db8::/32"}})
	if b.MiniAppHost != "tg.central.example.com" || strings.Join(b.WebhookIPAllowlist, ",") != "91.108.4.0/22,149.154.160.0/20,2001:db8::/32" {
		t.Fatalf("bot %+v", b)
	}
}

// SM-TG-014: If-Match versions, like the Harbor Pilot connection; every write is audited.
func TestVersions(t *testing.T) {
	t.Parallel()
	e := newEnv(t, publicURL)
	ctx := context.Background()
	token := e.tg.Token()
	if _, err := e.svc.Put(ctx, admin, 3, Input{BotToken: token}); apperr.CodeOf(err) != apperr.CodeNotFound {
		t.Fatalf("an update of a missing bot: %v", err)
	}
	e.put(0, Input{BotToken: token})
	if _, err := e.svc.Put(ctx, admin, 0, Input{BotToken: token}); apperr.CodeOf(err) != apperr.CodeConflict {
		t.Fatalf("a second create: %v", err)
	}
	if _, err := e.svc.Put(ctx, admin, 7, Input{}); apperr.CodeOf(err) != apperr.CodeConflict {
		t.Fatalf("a stale update: %v", err)
	}
	if err := e.svc.Delete(ctx, admin, 7); apperr.CodeOf(err) != apperr.CodeConflict {
		t.Fatalf("a stale delete: %v", err)
	}
	if err := e.svc.Delete(ctx, admin, 1); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Delete(ctx, admin, 1); apperr.CodeOf(err) != apperr.CodeNotFound {
		t.Fatalf("a second delete: %v", err)
	}
	if n := e.query(`SELECT count(*) FROM audit_events WHERE action IN ('telegram.bot.created', 'telegram.bot.deleted')`); n != "2" {
		t.Fatalf("audit rows %s", n)
	}
}

func (e *env) seedLinks(botID int64, n int) {
	e.t.Helper()
	ctx := context.Background()
	if err := e.s.W().TxNamed(ctx, "seed", func(tx *store.Tx) error {
		if _, err := tx.SQL.ExecContext(ctx, `INSERT OR IGNORE INTO users (id, display_name, enabled, provisioned_via, created_at, created_by, updated_at) VALUES ('usr_1', 'A', 1, 'admin', 0, 't', 0)`); err != nil {
			return err
		}
		for i := range n {
			tg := botID*100 + int64(i)
			if _, err := tx.SQL.ExecContext(ctx, `INSERT INTO telegram_links (id, user_id, bot_id, tg_user_id, chat_id, created_at, updated_at) VALUES (?, 'usr_1', ?, ?, ?, 0, 0)`,
				"tgl_"+itoa(tg), botID, tg, tg); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		e.t.Fatal(err)
	}
}

// SM-TG-001 (plan §3.1): a link belongs to one bot. A new token of the same bot keeps every link; a different bot (another ID from
// getMe) revokes them all, in the same write, and tells the link caches; deleting the bot keeps links.
func TestDifferentBotRevokesLinks(t *testing.T) {
	t.Parallel()
	e := newEnv(t, publicURL)
	ctx := context.Background()
	sub := e.bus.Subscribe(4, eventbus.TopicTelegramLink)
	defer sub.Close()
	first := e.tg.Bot().ID
	e.put(0, Input{BotToken: e.tg.Token()})
	e.seedLinks(first, 2)
	b := e.put(1, Input{BotToken: e.tg.Token()})
	if b.TokenVersion != 2 || len(b.Warnings) != 0 {
		t.Fatalf("same bot: %+v", b)
	}
	if n := e.query(`SELECT count(*) FROM telegram_links WHERE state = 'active'`); n != "2" {
		t.Fatalf("a new token of the same bot revoked links: %s active", n)
	}
	// The bot is removed: links stay for when it comes back.
	if err := e.svc.Delete(ctx, admin, 2); err != nil {
		t.Fatal(err)
	}
	if n := e.query(`SELECT count(*) FROM telegram_links WHERE state = 'active'`); n != "2" {
		t.Fatalf("deleting the bot revoked links: %s active", n)
	}
	select {
	case ev := <-sub.C:
		t.Fatalf("event without a revocation: %+v", ev)
	default:
	}

	other := tgtest.New(t, tgtest.Options{Limits: tgtest.Limits{Disabled: true}})
	e.svc.o.BaseURL = other.URL()
	b = e.put(0, Input{BotToken: other.Token()})
	if b.BotID != other.Bot().ID || b.BotID == first || len(b.Warnings) != 1 || !strings.Contains(b.Warnings[0], "2 Telegram links") {
		t.Fatalf("a different bot: %+v", b)
	}
	if n := e.query(`SELECT count(*) FROM telegram_links WHERE state = 'revoked' AND revoked_at IS NOT NULL AND revoked_by = 'telegram:bot_changed'`); n != "2" {
		t.Fatalf("revoked links: %s", n)
	}
	if d := e.query(`SELECT details_json FROM audit_events WHERE action = 'telegram.bot.created' ORDER BY seq DESC LIMIT 1`); !strings.Contains(d, `"links_revoked":2`) {
		t.Fatalf("audit %s", d)
	}
	select {
	case <-sub.C:
	case <-time.After(5 * time.Second):
		t.Fatal("no telegram.link event after the revocation")
	}
	noToken(t, e.tg.Token(), e.logs.String())
	noToken(t, other.Token(), e.logs.String())
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func private(id int64, text string) botapi.Update {
	return botapi.Update{Message: &botapi.Message{MessageID: 1, From: &botapi.User{ID: id, FirstName: "Stranger"}, Chat: botapi.Chat{ID: id, Type: botapi.ChatPrivate}, Text: text}}
}

// Plan §4.1 startup in webhook mode: getMe, commands, the menu button (the Mini App when a host is set), then
// setWebhook on central's public URL with the generated secret, the allowed updates and max_connections 10. The
// webhook route accepts Telegram's deliveries; health comes from getWebhookInfo and reports a delivery error newer
// than the setWebhook. Disabling removes the webhook and the route.
func TestWebhookModeStartupAndHealth(t *testing.T) {
	t.Parallel()
	e := newEnv(t, publicURL)
	ctx := context.Background()
	b := e.put(0, Input{BotToken: e.tg.Token(), Enabled: true, MiniAppHost: "tg.central.example.com"})
	if b.WebhookStatus != StateOK || b.Health.Username != "spaceman_test_bot" || b.Health.Error != "" {
		t.Fatalf("health %+v", b.Health)
	}
	set := e.tg.Calls("setWebhook")
	if len(set) != 1 {
		t.Fatalf("setWebhook calls %d", len(set))
	}
	p := set[0].Params
	hook := e.query(`SELECT hook_id FROM telegram_bot`)
	if p["url"] != publicURL+updates.WebhookPath+hook || p["max_connections"] != "10" || p["allowed_updates"] != `["message","callback_query","my_chat_member"]` ||
		p["drop_pending_updates"] != "true" {
		t.Fatalf("setWebhook %v", p)
	}
	if len(e.tg.Commands()) != len(Commands) {
		t.Fatalf("commands %v", e.tg.Commands())
	}
	if m := e.tg.MenuButton(); m == nil || m.Type != botapi.MenuButtonWebApp || m.WebApp.URL != "https://tg.central.example.com/" {
		t.Fatalf("menu button %+v", m)
	}
	if e.svc.MiniAppHost() != "tg.central.example.com" {
		t.Fatalf("mini app host %q", e.svc.MiniAppHost())
	}
	// A delivery reaches the dispatcher, which answers the unlinked user through the configured client.
	if code, err := e.tg.Deliver(ctx, private(4242, "hi")); err != nil || code != http.StatusOK {
		t.Fatalf("delivery: %d %v", code, err)
	}
	waitFor(t, "the unlinked reply", func() bool { return len(e.tg.Messages(4242)) == 1 })

	// Telegram reports a delivery error newer than the setWebhook: health shows it (after the 30 s reuse).
	e.clk.Advance(time.Minute)
	if code, err := e.tg.DeliverRaw(ctx, "wrong", []byte(`{"update_id":9}`)); err != nil || code != http.StatusUnauthorized {
		t.Fatalf("wrong secret: %d %v", code, err)
	}
	st := e.svc.Status(ctx)
	if st.WebhookStatus != StateError || !strings.Contains(st.Error, "401") || st.LastErrorAt == nil || st.CheckedAt == nil {
		t.Fatalf("health after a delivery error: %+v", st)
	}
	// Telegram unreachable: unknown, not ok.
	e.clk.Advance(time.Minute)
	e.tg.FailNext("getWebhookInfo", http.StatusInternalServerError, "Internal", 0)
	if st := e.svc.Status(ctx); st.WebhookStatus != StateUnknown {
		t.Fatalf("health without getWebhookInfo: %+v", st)
	}
	// Re-registering (an update) clears the old error; no new token keeps pending updates.
	b = e.put(1, Input{Enabled: true, WebhookIPAllowlist: []string{"149.154.160.0/20"}})
	if b.WebhookStatus != StateOK || e.tg.Calls("setWebhook")[1].Params["drop_pending_updates"] != "" {
		t.Fatalf("after an update: %+v %v", b.Health, e.tg.Calls("setWebhook")[1].Params)
	}
	if m := e.tg.MenuButton(); m.Type != botapi.MenuButtonCommands || e.svc.MiniAppHost() != "" {
		t.Fatalf("menu without a Mini App host: %+v %q", m, e.svc.MiniAppHost())
	}
	// The allowlist is live: the test client is not in Telegram's range.
	if code, _ := e.tg.Deliver(ctx, private(4243, "hi")); code != http.StatusForbidden {
		t.Fatalf("delivery from outside the allowlist: %d", code)
	}

	// Disabled: the webhook is removed at Telegram and the route answers 404.
	b = e.put(2, Input{Enabled: false})
	if b.WebhookStatus != StateOff || len(e.tg.Calls("deleteWebhook")) != 1 {
		t.Fatalf("disabled: %+v, deleteWebhook %d", b.Health, len(e.tg.Calls("deleteWebhook")))
	}
	if code, _ := e.tg.DeliverRaw(ctx, "x", []byte(`{}`)); code != http.StatusNotFound {
		t.Fatalf("webhook while disabled: %d", code)
	}
	noToken(t, e.tg.Token(), e.logs.String())
}

// Plan §4.1 polling mode: the webhook is deleted (Telegram refuses getUpdates while one is set), the route answers
// 404, and getUpdates feeds the same dispatcher. Polling starts with central (Start) and stops on delete.
func TestPollingModeAndDelete(t *testing.T) {
	t.Parallel()
	e := newEnv(t, "http://127.0.0.1:8080")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := e.put(0, Input{BotToken: e.tg.Token(), Enabled: true, Mode: ModePolling})
	// Not serving yet: polling waits for Start.
	if b.WebhookStatus != StateStarting || len(e.tg.Calls("deleteWebhook")) == 0 || len(e.tg.Calls("getUpdates")) != 0 {
		t.Fatalf("before Start: %+v", b.Health)
	}
	e.svc.Start(ctx)
	if st := e.svc.Status(ctx); st.WebhookStatus != StatePolling {
		t.Fatalf("after Start: %+v", st)
	}
	e.tg.PushUpdate(private(5151, "hi"))
	waitFor(t, "the unlinked reply", func() bool { return len(e.tg.Messages(5151)) == 1 })
	if code, _ := e.tg.DeliverRaw(ctx, "x", []byte(`{}`)); code != http.StatusNotFound {
		t.Fatalf("webhook route in polling mode: %d", code)
	}

	deletes := len(e.tg.Calls("deleteWebhook"))
	if err := e.svc.Delete(ctx, admin, 1); err != nil {
		t.Fatal(err)
	}
	st := e.svc.Status(ctx)
	if st.Configured || st.WebhookStatus != StateOff || len(e.tg.Calls("deleteWebhook")) != deletes+1 {
		t.Fatalf("after delete: %+v", st)
	}
	n := len(e.tg.Calls("getUpdates"))
	time.Sleep(50 * time.Millisecond)
	e.tg.PushUpdate(private(5152, "hi"))
	time.Sleep(200 * time.Millisecond)
	if len(e.tg.Calls("getUpdates")) > n+1 || len(e.tg.Messages(5152)) != 0 {
		t.Fatal("polling continued after the bot was deleted")
	}
}

// Startup reads the stored bot (a restart): the same configuration comes up again without dropping updates, and a
// failure shows in health without the token, then is retried.
func TestStartupFromStoredBot(t *testing.T) {
	t.Parallel()
	e := newEnv(t, publicURL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.put(0, Input{BotToken: e.tg.Token(), Enabled: true})
	// A fresh service over the same store is central after a restart.
	restarted := New(e.s, e.svc.audit, e.svc.kr, e.svc.o)
	e.tg.FailNext("getMe", http.StatusBadGateway, "Bad Gateway", 0)
	restarted.Start(ctx)
	st := restarted.Status(ctx)
	if st.WebhookStatus != StateError || !strings.Contains(st.Error, "getMe") {
		t.Fatalf("a failed start: %+v", st)
	}
	noToken(t, e.tg.Token(), st.Error, e.logs.String())
	if retry, _ := restarted.startOnce(ctx, 0); retry {
		t.Fatal("a working start asked for a retry")
	}
	if st := restarted.Status(ctx); st.WebhookStatus != StateOK {
		t.Fatalf("after the retry: %+v", st)
	}
	calls := e.tg.Calls("setWebhook")
	if p := calls[len(calls)-1].Params; p["drop_pending_updates"] != "" {
		t.Fatalf("a restart dropped pending updates: %v", p)
	}
	// A token Telegram revoked is not retried.
	e.tg.FailNext("getMe", http.StatusUnauthorized, "Unauthorized", 0)
	if retry, _ := restarted.startOnce(ctx, 0); retry {
		t.Fatal("a refused token is retried")
	}
}

// The public URL's host with a trailing dot (an absolute DNS name) is still the public host.
func TestMiniAppHostDiffersFromPublicHostWithTrailingDot(t *testing.T) {
	t.Parallel()
	for _, public := range []string{"https://central.example.com.", "https://CENTRAL.example.com.:8443/x"} {
		v := &apperr.Validation{}
		checkMiniAppHost(v, "central.example.com", public)
		fieldMsg(t, v.Err(), "miniapp_host", "must differ from the host of central's public URL")
	}
}

// A transient failure right after a write (Telegram 5xx or 429) is retried with the startup backoff: the bot ends in
// ok without another write.
func TestRetryAfterWrite(t *testing.T) {
	t.Parallel()
	e := newEnv(t, publicURL)
	e.svc.retryMin, e.svc.retryMax = 5*time.Millisecond, 20*time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.svc.Start(ctx) // no bot yet: nothing to retry, but the writes run on this context
	b := e.put(0, Input{BotToken: e.tg.Token(), Enabled: true})
	if b.WebhookStatus != StateOK {
		t.Fatalf("first put: %+v", b.Health)
	}

	for _, step := range []struct {
		method string
		code   int
	}{{"getMe", http.StatusBadGateway}, {"setMyCommands", http.StatusTooManyRequests}, {"setChatMenuButton", http.StatusBadGateway}, {"setWebhook", http.StatusBadGateway}} {
		e.tg.FailNext(step.method, step.code, "transient", 0)
		before := len(e.tg.Calls("setWebhook"))
		b = e.put(b.Version, Input{Enabled: true})
		if b.WebhookStatus != StateError || !strings.Contains(b.Health.Error, step.method) {
			t.Fatalf("%s: a failed apply shows %+v", step.method, b.Health)
		}
		waitFor(t, step.method+" retried to ok", func() bool { return e.svc.Status(ctx).WebhookStatus == StateOK })
		if len(e.tg.Calls("setWebhook")) <= before {
			t.Fatalf("%s: no setWebhook after the retry", step.method)
		}
	}

	// A refused token is not retried.
	e.tg.FailNext("getMe", http.StatusUnauthorized, "Unauthorized", 0)
	_ = e.put(b.Version, Input{Enabled: true})
	n := len(e.tg.Calls("getMe"))
	time.Sleep(100 * time.Millisecond)
	if got := len(e.tg.Calls("getMe")); got != n || e.svc.Status(ctx).WebhookStatus != StateError {
		t.Fatalf("a refused token was retried (%d -> %d getMe calls)", n, got)
	}
}

// A Telegram error that echoes the token, or only its secret part, reaches neither the health, the warnings, the
// Put error nor the logs (botapi redacts both).
func TestTokenEchoedByTelegramStaysOut(t *testing.T) {
	t.Parallel()
	e := newEnv(t, publicURL)
	token := e.tg.Token()
	secretPart := token[strings.IndexByte(token, ':')+1:]
	echoes := []string{"bad request near " + token, "bad request near " + secretPart, "x" + secretPart + "y"}

	for _, echo := range echoes {
		e.tg.FailNext("getMe", http.StatusBadGateway, echo, 0)
		_, err := e.svc.Put(context.Background(), admin, 0, Input{BotToken: token})
		if err == nil {
			t.Fatal("put succeeded")
		}
		noToken(t, token, err.Error(), e.logs.String())
	}

	b := e.put(0, Input{BotToken: token, Enabled: true})
	for _, echo := range echoes {
		for _, method := range []string{"getMe", "setMyCommands", "setChatMenuButton", "setWebhook"} {
			e.tg.FailNext(method, http.StatusBadGateway, echo, 0)
			b = e.put(b.Version, Input{Enabled: true})
			if b.WebhookStatus != StateError || !strings.Contains(b.Health.Error, method) {
				t.Fatalf("%s: %+v", method, b.Health)
			}
			noToken(t, token, b.Health.Error, strings.Join(b.Warnings, "\n"), e.svc.Status(context.Background()).Error, e.logs.String())
		}
	}
}
