package updates

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/obs/metrics"
	"github.com/aurimasniekis/go-spaceman/internal/obs/metrics/metricstest"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
)

// randomB64 returns n random bytes as unpadded base64url, generated at test time (no secret fixtures).
func randomB64(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func testConfig(t *testing.T) *WebhookConfig {
	return &WebhookConfig{HookID: randomB64(t, 16), Secret: randomB64(t, 32)}
}

// recorder is a Handler and Links pair: users in linked are linked; every handled update is recorded.
type recorder struct {
	mu      sync.Mutex
	linked  map[int64]Link
	handled []botapi.Update
	lookups int
	block   chan struct{} // when set, Handle waits on it
	started chan int64    // when set, Handle reports each update_id on entry
}

func (r *recorder) Lookup(id int64) (Link, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lookups++
	l, ok := r.linked[id]
	return l, ok
}

func (r *recorder) Handle(_ context.Context, u botapi.Update, _ Link) {
	if r.started != nil {
		r.started <- u.UpdateID
	}
	if r.block != nil {
		<-r.block
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.handled = append(r.handled, u)
}

func (r *recorder) ids() []int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []int64
	for _, u := range r.handled {
		out = append(out, u.UpdateID)
	}
	return out
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func privateMsg(id, user int64, text string) botapi.Update {
	return botapi.Update{UpdateID: id, Message: &botapi.Message{MessageID: int(id), From: &botapi.User{ID: user, FirstName: "U"},
		Chat: botapi.Chat{ID: user, Type: botapi.ChatPrivate}, Text: text}}
}

func body(t *testing.T, u botapi.Update) []byte {
	t.Helper()
	b, err := json.Marshal(u)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// readSpy fails the test if the handler reads the body.
type readSpy struct {
	t    *testing.T
	read bool
}

func (s *readSpy) Read([]byte) (int, error) {
	s.read = true
	return 0, io.EOF
}

func post(w http.Handler, path, contentType, secret string, b io.Reader, edit func(*http.Request)) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, b)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if secret != "" {
		req.Header.Set(SecretHeader, secret)
	}
	if edit != nil {
		edit(req)
	}
	rec := httptest.NewRecorder()
	w.ServeHTTP(rec, req)
	return rec
}

// SM-TG-004: every refusal of the webhook, in its order: no configuration or a wrong hook id 404, a wrong method 405, a
// wrong content type 415, a wrong or missing secret 401 before the body is touched, a source outside the allowlist
// 403, a body over 256 KiB 413 (declared or streamed), an invalid update 400. Response bodies never echo the request.
func TestWebhookRefusals(t *testing.T) {
	m := metrics.New()
	rec := &recorder{}
	s := New(Options{Intake: IntakeOptions{Metrics: m, Rate: -1}, Dispatcher: DispatcherOptions{Links: rec, Handler: rec}})
	s.Webhook.setRunning(true)
	cfg := testConfig(t)
	path := WebhookPath + cfg.HookID
	good := body(t, privateMsg(1, 5, "hello"))

	if r := post(s.Webhook, path, "application/json", cfg.Secret, bytes.NewReader(good), nil); r.Code != http.StatusNotFound {
		t.Fatalf("no configuration: %d", r.Code)
	}
	if err := s.Webhook.Set(cfg); err != nil {
		t.Fatal(err)
	}

	spy := &readSpy{t: t}
	cases := []struct {
		name   string
		path   string
		method string
		ctype  string
		secret string
		body   io.Reader
		edit   func(*http.Request)
		want   int
		result metrics.TelegramWebhookResult
	}{
		{"wrong hook id", WebhookPath + randomB64(t, 16), "", "application/json", cfg.Secret, bytes.NewReader(good), nil, 404, metrics.TelegramWebhookNotFound},
		{"hook id prefix", path[:len(path)-1], "", "application/json", cfg.Secret, bytes.NewReader(good), nil, 404, metrics.TelegramWebhookNotFound},
		{"other path", "/telegram/other", "", "application/json", cfg.Secret, bytes.NewReader(good), nil, 404, metrics.TelegramWebhookNotFound},
		{"GET", path, http.MethodGet, "application/json", cfg.Secret, nil, nil, 405, metrics.TelegramWebhookBadMethod},
		{"form body", path, "", "application/x-www-form-urlencoded", cfg.Secret, bytes.NewReader(good), nil, 415, metrics.TelegramWebhookBadType},
		{"no content type", path, "", "", cfg.Secret, bytes.NewReader(good), nil, 415, metrics.TelegramWebhookBadType},
		{"wrong secret, body unread", path, "", "application/json", randomB64(t, 32), spy, nil, 401, metrics.TelegramWebhookUnauthorized},
		{"secret prefix", path, "", "application/json", cfg.Secret[:len(cfg.Secret)-1], bytes.NewReader(good), nil, 401, metrics.TelegramWebhookUnauthorized},
		{"no secret", path, "", "application/json", "", bytes.NewReader(good), nil, 401, metrics.TelegramWebhookUnauthorized},
		{"declared too large", path, "", "application/json", cfg.Secret, bytes.NewReader(good),
			func(r *http.Request) { r.ContentLength = MaxBodyBytes + 1 }, 413, metrics.TelegramWebhookTooLarge},
		{"streamed too large", path, "", "application/json", cfg.Secret, io.MultiReader(bytes.NewReader([]byte(`{"update_id":1,"x":"`)),
			bytes.NewReader(bytes.Repeat([]byte("a"), MaxBodyBytes))), func(r *http.Request) { r.ContentLength = -1 }, 413, metrics.TelegramWebhookTooLarge},
		{"not JSON", path, "", "application/json", cfg.Secret, strings.NewReader("{"), nil, 400, metrics.TelegramWebhookBadRequest},
		{"no update_id", path, "", "application/json", cfg.Secret, strings.NewReader(`{"message":{}}`), nil, 400, metrics.TelegramWebhookBadRequest},
		{"trailing data", path, "", "application/json", cfg.Secret, strings.NewReader(`{"update_id":3} {"update_id":4}`), nil, 400, metrics.TelegramWebhookBadRequest},
		{"array", path, "", "application/json", cfg.Secret, strings.NewReader(`[{"update_id":3}]`), nil, 400, metrics.TelegramWebhookBadRequest},
	}
	for _, c := range cases {
		before := metricstest.Value(t, m, "spaceman_telegram_webhook_requests_total", "result", string(c.result))
		req := httptest.NewRequest(http.MethodPost, c.path, c.body)
		if c.method != "" {
			req.Method = c.method
		}
		if c.ctype != "" {
			req.Header.Set("Content-Type", c.ctype)
		}
		if c.secret != "" {
			req.Header.Set(SecretHeader, c.secret)
		}
		if c.edit != nil {
			c.edit(req)
		}
		r := httptest.NewRecorder()
		s.Webhook.ServeHTTP(r, req)
		if r.Code != c.want {
			t.Errorf("%s: %d, want %d", c.name, r.Code, c.want)
		}
		if got := metricstest.Value(t, m, "spaceman_telegram_webhook_requests_total", "result", string(c.result)); got != before+1 {
			t.Errorf("%s: metric %s %v -> %v", c.name, c.result, before, got)
		}
		if b := r.Body.String(); strings.Contains(b, cfg.Secret) || strings.Contains(b, cfg.HookID) || strings.Contains(b, "hello") {
			t.Errorf("%s: the response echoes the request: %q", c.name, b)
		}
	}
	if spy.read {
		t.Fatal("the body was read before the secret was checked")
	}
	if n := len(rec.ids()); n != 0 || len(s.Intake.seen) != 0 {
		t.Fatalf("a refused request reached the queue: handled %d, seen %d", n, len(s.Intake.seen))
	}

	// The allowlist: refused from outside, accepted from inside; a trusted proxy's X-Forwarded-For counts, another
	// peer's doesn't.
	ip := testConfig(t)
	ip.AllowedIPs = []netip.Prefix{netip.MustParsePrefix("149.154.160.0/20")}
	w := NewWebhook(s.Intake, WebhookOptions{TrustedProxies: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}, Metrics: m})
	w.setRunning(true)
	if err := w.Set(ip); err != nil {
		t.Fatal(err)
	}
	ipPath := WebhookPath + ip.HookID
	for i, c := range []struct {
		remote, xff string
		want        int
	}{
		{"203.0.113.9:443", "", 403},
		{"203.0.113.9:443", "149.154.167.1", 403}, // an untrusted peer can't claim an address
		{"149.154.167.1:443", "", 200},
		{"10.1.2.3:443", "149.154.167.1", 200},
		{"10.1.2.3:443", "149.154.167.1, 203.0.113.9", 403}, // the right-most untrusted entry wins
		{"[::ffff:149.154.167.1]:443", "", 200},
		{"10.1.2.3:443", "149.154.167.1|203.0.113.9", 403}, // every X-Forwarded-For line counts: the last one too
		{"10.1.2.3:443", "203.0.113.9|149.154.167.1, 10.4.4.4", 200},
	} {
		r := post(w, ipPath, "application/json", ip.Secret, bytes.NewReader(body(t, privateMsg(int64(100+i), 5, "x"))), func(r *http.Request) {
			r.RemoteAddr = c.remote
			if c.xff != "" {
				for line := range strings.SplitSeq(c.xff, "|") {
					r.Header.Add("X-Forwarded-For", line)
				}
			}
		})
		if r.Code != c.want {
			t.Errorf("remote %s xff %q: %d, want %d", c.remote, c.xff, r.Code, c.want)
		}
	}
}

// SM-TG-004: refused secrets are audited at most once a minute, with the count refused since the last record.
func TestWebhookUnauthorizedAuditedOncePerMinute(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC))
	type call struct {
		ip      string
		refused int64
	}
	var calls []call
	in := NewIntake(IntakeOptions{})
	w := NewWebhook(in, WebhookOptions{Clock: clk, OnUnauthorized: func(_ context.Context, ip string, n int64) { calls = append(calls, call{ip, n}) }})
	cfg := testConfig(t)
	if err := w.Set(cfg); err != nil {
		t.Fatal(err)
	}
	bad := func() {
		r := post(w, WebhookPath+cfg.HookID, "application/json", "wrong", strings.NewReader("{}"), func(r *http.Request) { r.RemoteAddr = "198.51.100.7:1234" })
		if r.Code != http.StatusUnauthorized {
			t.Fatalf("status %d", r.Code)
		}
	}
	bad()
	bad()
	clk.Advance(30 * time.Second)
	bad()
	if len(calls) != 1 || calls[0] != (call{"198.51.100.7", 1}) {
		t.Fatalf("calls within the first minute: %+v", calls)
	}
	clk.Advance(31 * time.Second)
	bad()
	if len(calls) != 2 || calls[1].refused != 3 {
		t.Fatalf("calls after a minute: %+v", calls)
	}
}

// SM-TG-004: refusals pending when a burst of wrong secrets stops are still recorded, once the minute has passed,
// by the flush Service.Run runs on a timer; with nothing pending the flush records nothing.
func TestWebhookUnauthorizedFlushedAfterBurst(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC))
	var mu sync.Mutex
	var calls []int64
	var ips []string
	w := NewWebhook(NewIntake(IntakeOptions{}), WebhookOptions{Clock: clk, OnUnauthorized: func(_ context.Context, ip string, n int64) {
		mu.Lock()
		defer mu.Unlock()
		calls, ips = append(calls, n), append(ips, ip)
	}})
	got := func() string {
		mu.Lock()
		defer mu.Unlock()
		return fmt.Sprint(calls, ips)
	}
	cfg := testConfig(t)
	if err := w.Set(cfg); err != nil {
		t.Fatal(err)
	}
	bad := func(remote string) {
		post(w, WebhookPath+cfg.HookID, "application/json", "wrong", strings.NewReader("{}"), func(r *http.Request) { r.RemoteAddr = remote })
	}
	bad("198.51.100.7:1")
	bad("198.51.100.8:1")
	bad("198.51.100.9:1")
	w.flushUnauthorized(context.Background()) // within the minute: nothing
	if g := got(); g != "[1] [198.51.100.7]" {
		t.Fatalf("after the burst: %s", g)
	}
	clk.Advance(rejectAuditEvery)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.auditLoop(ctx, time.Millisecond); close(done) }()
	waitFor(t, "the flush", func() bool { return got() == "[1 2] [198.51.100.7 198.51.100.9]" })
	clk.Advance(rejectAuditEvery)
	time.Sleep(20 * time.Millisecond)
	cancel()
	<-done
	if g := got(); g != "[1 2] [198.51.100.7 198.51.100.9]" {
		t.Fatalf("a flush with nothing pending recorded something: %s", g)
	}
}

// rawRequest sends head (a request without its body) on a fresh connection to srv and returns everything the server
// sends back before closing the connection. It fails the test when the response or the close takes over 3 seconds.
func rawRequest(t *testing.T, srv *httptest.Server, head string) string {
	t.Helper()
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := io.WriteString(conn, head); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	b, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("no prompt response and close (got %q): %v", b, err)
	}
	return string(b)
}

// SM-TG-004: a refusal before the body is read answers at once and closes the connection, even when the client
// declared a body (chunked or with a Content-Length) and never sends it. Otherwise net/http would wait, with no
// deadline (central's server sets no ReadTimeout), to drain the body first, and every such connection would hold a
// goroutine without ever being told 401.
func TestWebhookRefusalBeforeBodyClosesConnection(t *testing.T) {
	s := New(Options{Intake: IntakeOptions{Rate: -1}})
	s.Webhook.setRunning(true)
	cfg := testConfig(t)
	if err := s.Webhook.Set(cfg); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(s.Webhook)
	srv.Config.ErrorLog = slog.NewLogLogger(slog.DiscardHandler, slog.LevelError)
	srv.Start()
	defer srv.Close()
	path := WebhookPath + cfg.HookID
	for _, c := range []struct {
		name, path, ctype, secret, framing string
		want                               string
	}{
		{"wrong secret, chunked", path, "application/json", randomB64(t, 32), "Transfer-Encoding: chunked", "401"},
		{"wrong secret, Content-Length", path, "application/json", randomB64(t, 32), "Content-Length: 1000", "401"},
		{"wrong hook id, chunked", WebhookPath + randomB64(t, 16), "application/json", cfg.Secret, "Transfer-Encoding: chunked", "404"},
		{"wrong content type", path, "text/plain", cfg.Secret, "Content-Length: 1000", "415"},
		{"declared too large", path, "application/json", cfg.Secret, fmt.Sprintf("Content-Length: %d", MaxBodyBytes+1), "413"},
	} {
		head := "POST " + c.path + " HTTP/1.1\r\nHost: example.com\r\nContent-Type: " + c.ctype + "\r\n" + SecretHeader + ": " + c.secret +
			"\r\n" + c.framing + "\r\n\r\n"
		start := time.Now()
		resp := rawRequest(t, srv, head)
		if !strings.HasPrefix(resp, "HTTP/1.1 "+c.want+" ") {
			t.Errorf("%s: response %q, want %s", c.name, resp, c.want)
		}
		if !strings.Contains(strings.ToLower(resp), "\r\nconnection: close\r\n") {
			t.Errorf("%s: no Connection: close in %q", c.name, resp)
		}
		if d := time.Since(start); d > time.Second {
			t.Errorf("%s: took %v", c.name, d)
		}
	}
}

// SM-TG-004: the queue bound is per shard (QueueSize / Workers): with 4 slots over 2 workers, each chat's shard holds
// 2 updates and the third gets 503, while the other shard still has room.
func TestWebhookQueueBoundPerShard(t *testing.T) {
	s := New(Options{Intake: IntakeOptions{QueueSize: 4, Workers: 2, Rate: -1}})
	s.Webhook.setRunning(true) // no workers run: nothing leaves the queue
	cfg := testConfig(t)
	if err := s.Webhook.Set(cfg); err != nil {
		t.Fatal(err)
	}
	deliver := func(id, chat int64) int {
		return post(s.Webhook, WebhookPath+cfg.HookID, "application/json", cfg.Secret, bytes.NewReader(body(t, privateMsg(id, chat, "x"))), nil).Code
	}
	id := int64(0)
	for _, chat := range []int64{2, 3} { // shards 0 and 1
		for i := range 3 {
			id++
			want := http.StatusOK
			if i == 2 {
				want = http.StatusServiceUnavailable
			}
			if c := deliver(id, chat); c != want {
				t.Fatalf("chat %d, update %d: %d, want %d", chat, i+1, c, want)
			}
		}
	}
}

// Set accepts only a well-formed hook id and secret; nil turns the webhook off.
func TestWebhookSetValidates(t *testing.T) {
	w := NewWebhook(NewIntake(IntakeOptions{}), WebhookOptions{})
	for _, c := range []WebhookConfig{
		{HookID: "short", Secret: "s"},
		{HookID: strings.Repeat("a", 22), Secret: ""},
		{HookID: strings.Repeat("a", 22), Secret: "has space"},
		{HookID: strings.Repeat("a", 22), Secret: strings.Repeat("s", 257)},
		{HookID: strings.Repeat("/", 22), Secret: "s"},
	} {
		if err := w.Set(&c); err == nil {
			t.Errorf("accepted %+v", c)
		}
	}
	cfg := testConfig(t)
	if err := w.Set(cfg); err != nil {
		t.Fatal(err)
	}
	w.setRunning(true)
	if r := post(w, WebhookPath+cfg.HookID, "application/json", cfg.Secret, bytes.NewReader(body(t, privateMsg(1, 5, "x"))), nil); r.Code != 200 {
		t.Fatalf("configured: %d", r.Code)
	}
	_ = w.Set(nil)
	if r := post(w, WebhookPath+cfg.HookID, "application/json", cfg.Secret, bytes.NewReader(body(t, privateMsg(2, 5, "x"))), nil); r.Code != 404 {
		t.Fatalf("after Set(nil): %d", r.Code)
	}
}

// SM-TG-004: a duplicate update_id is acknowledged with 200 and handled once; while the dispatcher isn't running the
// webhook answers 503 and remembers nothing, so Telegram's retry is accepted later.
func TestWebhookDedupesAndNeedsRunningWorkers(t *testing.T) {
	rec := &recorder{linked: map[int64]Link{5: {ID: "tgl_1", UserID: "usr_1", ChatID: 5}}}
	s := New(Options{Intake: IntakeOptions{Rate: -1}, Dispatcher: DispatcherOptions{Links: rec, Handler: rec}})
	cfg := testConfig(t)
	if err := s.Webhook.Set(cfg); err != nil {
		t.Fatal(err)
	}
	path := WebhookPath + cfg.HookID
	u := body(t, privateMsg(7, 5, "x"))
	if r := post(s.Webhook, path, "application/json", cfg.Secret, bytes.NewReader(u), nil); r.Code != http.StatusServiceUnavailable {
		t.Fatalf("before Run: %d", r.Code)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	waitFor(t, "workers", s.Webhook.running.Load)
	for range 3 {
		if r := post(s.Webhook, path, "application/json", cfg.Secret, bytes.NewReader(u), nil); r.Code != http.StatusOK {
			t.Fatalf("delivery: %d", r.Code)
		}
	}
	waitFor(t, "the update", func() bool { return len(rec.ids()) == 1 })
	time.Sleep(20 * time.Millisecond)
	if ids := rec.ids(); len(ids) != 1 || ids[0] != 7 {
		t.Fatalf("handled %v, want [7] once", ids)
	}
	cancel()
	<-done
	if r := post(s.Webhook, path, "application/json", cfg.Secret, bytes.NewReader(body(t, privateMsg(8, 5, "x"))), nil); r.Code != http.StatusServiceUnavailable {
		t.Fatalf("after Run ended: %d", r.Code)
	}
}

// SM-TG-004: with the worker busy and the queue full the webhook answers 503 (Telegram retries), and the refused
// update isn't remembered, so the retry is accepted once there is room. One chat's updates are handled in order.
func TestWebhookQueueFullAnswers503(t *testing.T) {
	m := metrics.New()
	rec := &recorder{linked: map[int64]Link{5: {ID: "tgl_1"}}, block: make(chan struct{}), started: make(chan int64, 16)}
	s := New(Options{Intake: IntakeOptions{QueueSize: 2, Workers: 1, Rate: -1, Metrics: m}, Dispatcher: DispatcherOptions{Links: rec, Handler: rec}})
	cfg := testConfig(t)
	if err := s.Webhook.Set(cfg); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)
	waitFor(t, "workers", s.Webhook.running.Load)
	path := WebhookPath + cfg.HookID
	deliver := func(id int64) int {
		return post(s.Webhook, path, "application/json", cfg.Secret, bytes.NewReader(body(t, privateMsg(id, 5, "x"))), nil).Code
	}
	if c := deliver(1); c != 200 {
		t.Fatalf("first: %d", c)
	}
	<-rec.started // the worker holds update 1
	for id := int64(2); id <= 3; id++ {
		if c := deliver(id); c != 200 {
			t.Fatalf("update %d: %d", id, c)
		}
	}
	if c := deliver(4); c != http.StatusServiceUnavailable {
		t.Fatalf("over the bound: %d, want 503", c)
	}
	if v := metricstest.Value(t, m, "spaceman_telegram_updates_queued"); v != 2 {
		t.Fatalf("queued gauge %v", v)
	}
	if v := metricstest.Value(t, m, "spaceman_telegram_webhook_requests_total", "result", "queue_full"); v != 1 {
		t.Fatalf("queue_full metric %v", v)
	}
	close(rec.block)
	waitFor(t, "the queue to drain", func() bool { return len(rec.ids()) == 3 })
	if c := deliver(4); c != 200 {
		t.Fatalf("the retry: %d", c)
	}
	waitFor(t, "the retry", func() bool { return len(rec.ids()) == 4 })
	if got := fmt.Sprint(rec.ids()); got != "[1 2 3 4]" {
		t.Fatalf("order %s", got)
	}
}
