package app_test

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/api"
	"github.com/aurimasniekis/go-spaceman/internal/app"
	"github.com/aurimasniekis/go-spaceman/internal/config"
	webui "github.com/aurimasniekis/go-spaceman/web"
)

// ADR-0083 §3.4.1, SM-TG-008: the Mini App host fails closed. A request whose Host is the configured
// telegram.miniapp_host (any case, any port, one trailing dot) is served only by the Mini App handler and never by the
// main router, whatever the bot's state: with no bot, a disabled bot or a bot whose stored miniapp_host is another,
// every path is a 404, including sign-in, openapi.json, the web UI and an admin operation with a valid Bearer token.

// newFailClosedEnv is a Mini App environment with telegram.miniapp_host set to miniHost and both web UIs embedded (so a
// main-host answer is told from a Mini App one and from a 404), with central's logs in the returned buffer.
func newFailClosedEnv(t *testing.T) (*miniAppEnv, *syncBuf) {
	t.Helper()
	logs := &syncBuf{}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	e := newMiniAppEnvFull(t, log, func(c *config.Config) { c.Telegram.MiniAppHost = miniHost }, func(c *app.Central) {
		c.UI = webui.Handler(dlFiles)
		c.MiniAppUI = webui.MiniAppHandler(dlFiles)
	})
	return e, logs
}

// failClosedCase is one request tried on a host that must answer 404.
type failClosedCase struct {
	method, path, body string
	hdr                []string
}

// failClosedCases are main-router routes (each answers on the main host) and the Mini App's own routes.
func failClosedCases(e *miniAppEnv, cookie string) []failClosedCase {
	bearer := []string{"Authorization", "Bearer " + e.root.bearer}
	login := `{"email":"mini@example.com","password":"correct horse battery staple"}`
	cs := []failClosedCase{
		{"GET", "/", "", nil},
		{"GET", "/index.html", "", nil},
		{"GET", "/tg.html", "", nil},
		{"GET", "/assets/app-abc123.js", "", nil},
		{"GET", "/spaces/x", "", nil},
		{"GET", "/agent/connect", "", nil},
		{"POST", "/telegram/webhook/abc", "{}", nil},
		{"POST", api.Prefix + "/auth/login", login, nil},
		{"GET", api.Prefix + "/auth/me", "", bearer},
		{"GET", api.Prefix + "/openapi.json", "", nil},
		{"GET", api.Prefix + "/openapi.yaml", "", nil},
		{"GET", api.Prefix + "/system/info", "", bearer},
		{"GET", api.Prefix + "/users", "", bearer},
		{"GET", api.Prefix + "/spaces", "", bearer},
		{"GET", api.Prefix + "/telegram/bot", "", bearer},
		{"POST", api.Prefix + "/mcp", "{}", bearer},
		{"POST", api.Prefix + "/telegram/session", exchangeBody(e.initData(e.tg.Token(), 42, 0)), []string{"Origin", e.origin()}},
		{"GET", api.TelegramDownloadPath + "abc", "", nil},
	}
	if cookie != "" {
		cs = append(cs, failClosedCase{"GET", api.Prefix + "/spaces", "", []string{"Cookie", cookie}},
			failClosedCase{"GET", api.Prefix + "/auth/me", "", []string{"Cookie", cookie}})
	}
	return cs
}

// hostVariants are spellings of miniHost a request may carry; each is the Mini App host.
func hostVariants(port string) []string {
	return []string{miniHost, miniHost + ":" + port, "TG.Example.COM", "tg.EXAMPLE.com:8443", miniHost + ":443", miniHost + ".", "TG.example.com.:80"}
}

// assertAll404 sends every case to every host and fails on anything but the Mini App's plain 404.
func assertAll404(t *testing.T, e *miniAppEnv, stage string, hosts []string, cases []failClosedCase) {
	t.Helper()
	for _, host := range hosts {
		for _, c := range cases {
			r := e.send(t, host, c.method, c.path, c.body, c.hdr...)
			if r.status != 404 || string(r.body) != "not found\n" || len(r.header.Values("Set-Cookie")) > 0 {
				t.Errorf("%s: Host %q %s %s: %d %q, want the Mini App host's 404", stage, host, c.method, c.path, r.status, r.body)
			}
		}
	}
}

// mainSession signs a user in on the main host and returns the session cookie as a Cookie header value.
func mainSession(t *testing.T, e *miniAppEnv) string {
	t.Helper()
	ui := enrolledSession(t, e.h, "mini@example.com")
	pub, _ := url.Parse(e.h.srv.URL)
	for _, ck := range ui.hc.Jar.Cookies(pub) {
		if ck.Name == "sm_session" {
			return ck.Name + "=" + ck.Value
		}
	}
	t.Fatal("no session cookie")
	return ""
}

func TestMiniAppHostFailsClosedWithoutBot(t *testing.T) {
	t.Parallel()
	e, _ := newFailClosedEnv(t)
	cookie := mainSession(t, e)
	cases := failClosedCases(e, cookie)

	// The same requests reach the main router on the main host: the token, cookie and sign-in are valid there, so a
	// 404 on the Mini App host is the host's doing.
	expect(t, e.send(t, "", "GET", api.Prefix+"/users", "", "Authorization", "Bearer "+e.root.bearer), 200)
	expect(t, e.send(t, "", "GET", api.Prefix+"/spaces", "", "Cookie", cookie), 200)
	expect(t, e.send(t, "", "GET", api.Prefix+"/openapi.json", ""), 200)
	if r := e.send(t, "", "GET", "/", ""); r.status != 200 || !strings.Contains(string(r.body), "MainUI") {
		t.Fatalf("main UI: %d %q", r.status, r.body)
	}

	// No bot at all.
	if e.h.c.TelegramBot.StoredMiniAppHost() != "" || e.h.c.TelegramBot.MiniAppHost() != "" {
		t.Fatal("a Mini App host before any bot")
	}
	assertAll404(t, e, "no bot", hostVariants(e.port), cases)
	// X-Forwarded-Host never moves a Mini App host request to the main router.
	pub, _ := url.Parse(e.h.srv.URL)
	assertAll404(t, e, "no bot, X-Forwarded-Host", []string{miniHost}, []failClosedCase{
		{"GET", "/", "", []string{"X-Forwarded-Host", pub.Host}},
		{"GET", api.Prefix + "/users", "", []string{"X-Forwarded-Host", pub.Host, "Authorization", "Bearer " + e.root.bearer}},
	})

	// A bot stored but disabled, with and without the same miniapp_host.
	expect(t, e.root.do("PUT", "/telegram/bot", map[string]any{"bot_token": e.tg.Token(), "enabled": false, "mode": "polling"}), 200)
	assertAll404(t, e, "disabled bot", hostVariants(e.port), cases)
	expect(t, e.root.do("PUT", "/telegram/bot", map[string]any{"enabled": false, "mode": "polling", "miniapp_host": miniHost}, "If-Match", `"1"`), 200)
	assertAll404(t, e, "disabled bot with the host", hostVariants(e.port), cases)

	// Deleted again: still 404.
	expect(t, e.root.do("DELETE", "/telegram/bot", nil, "If-Match", `"2"`), 204)
	assertAll404(t, e, "deleted bot", hostVariants(e.port), cases)

	// Other hosts are untouched: the main host still answers.
	if r := e.send(t, "x."+miniHost, "GET", "/", ""); r.status != 200 || !strings.Contains(string(r.body), "MainUI") {
		t.Errorf("another host: %d %q", r.status, r.body)
	}
}

func TestMiniAppHostFailsClosedWithBot(t *testing.T) {
	t.Parallel()
	e, logs := newFailClosedEnv(t)
	cookie := mainSession(t, e)
	// The bot stores no miniapp_host: the configured one is served.
	expect(t, e.root.do("PUT", "/telegram/bot", map[string]any{"bot_token": e.tg.Token(), "enabled": true, "mode": "polling"}), 200)
	if e.h.c.TelegramBot.MiniAppHost() != miniHost {
		t.Fatalf("serving %q", e.h.c.TelegramBot.MiniAppHost())
	}
	e.startBotLinks(t)
	tok := e.exchange(t)

	// Only the Mini App's routes answer, on every spelling of the host; main-router credentials are refused.
	for _, host := range hostVariants(e.port) {
		if r := e.send(t, host, "GET", "/", ""); r.status != 200 || !strings.Contains(string(r.body), "MiniApp") {
			t.Errorf("Host %q: shell %d %q", host, r.status, r.body)
		}
		if r := e.send(t, host, "GET", api.Prefix+"/spaces", "", "Authorization", "Telegram "+tok); r.status != 200 {
			t.Errorf("Host %q: Mini App session %d %s", host, r.status, r.body)
		}
		for _, c := range []struct {
			method, path, body string
			hdr                []string
			want               int
		}{
			{"GET", api.Prefix + "/users", "", []string{"Authorization", "Bearer " + e.root.bearer}, 401},
			{"POST", api.Prefix + "/auth/login", `{"email":"mini@example.com","password":"correct horse battery staple"}`, nil, 404},
			{"GET", api.Prefix + "/auth/me", "", []string{"Cookie", cookie}, 404},
			{"GET", api.Prefix + "/openapi.json", "", nil, 404},
			{"GET", "/agent/connect", "", nil, 404},
			{"POST", "/telegram/webhook/abc", "{}", nil, 404},
			{"POST", api.Prefix + "/mcp", "{}", []string{"Authorization", "Bearer " + e.root.bearer}, 404},
		} {
			r := e.send(t, host, c.method, c.path, c.body, c.hdr...)
			if r.status != c.want || strings.Contains(string(r.body), "MainUI") {
				t.Errorf("Host %q %s %s: %d, want %d: %s", host, c.method, c.path, r.status, c.want, r.body)
			}
		}
	}

	// A cookie, alone or with a Bearer token, is refused too (once: refusals count toward the sign-in rate limit).
	for _, hdr := range [][]string{{"Cookie", cookie}, {"Authorization", "Bearer " + e.root.bearer, "Cookie", cookie}} {
		if r := e.mini(t, "GET", api.Prefix+"/spaces", "", hdr...); r.status != 401 {
			t.Errorf("Mini App host with %q: %d %s", hdr, r.status, r.body)
		}
	}

	// A different stored miniapp_host: the configuration wins and is still served; the stored host is never the main
	// router either (it is a Mini App host that serves nothing); the mismatch is logged and in the system status.
	const other = "tg.other.example.com"
	expect(t, e.root.do("PUT", "/telegram/bot", map[string]any{"enabled": true, "mode": "polling", "miniapp_host": other}, "If-Match", `"1"`), 200)
	if r := e.send(t, miniHost, "GET", api.Prefix+"/spaces", "", "Authorization", "Telegram "+tok); r.status != 200 {
		t.Errorf("configured host with a mismatch: %d %s", r.status, r.body)
	}
	assertAll404(t, e, "the ignored stored host", []string{other, "TG.Other.Example.com:8443"}, failClosedCases(e, cookie))
	if !strings.Contains(logs.String(), "differs from telegram.miniapp_host") {
		t.Error("the mismatch is not logged")
	}
	var sys struct {
		Telegram struct {
			MiniAppHost        string `json:"miniapp_host"`
			MiniAppHostSource  string `json:"miniapp_host_source"`
			MiniAppHostIgnored string `json:"miniapp_host_ignored"`
		} `json:"telegram"`
	}
	r := e.root.do("GET", "/system", nil)
	expect(t, r, 200)
	r.json(t, &sys)
	if sys.Telegram.MiniAppHost != miniHost || sys.Telegram.MiniAppHostSource != "config" || sys.Telegram.MiniAppHostIgnored != other {
		t.Errorf("system status %+v", sys.Telegram)
	}

	// The stored host changes back to the configured one: the one it replaced is retired, not released.
	expect(t, e.root.do("PUT", "/telegram/bot", map[string]any{"enabled": true, "mode": "polling", "miniapp_host": miniHost}, "If-Match", `"2"`), 200)
	assertAll404(t, e, "the replaced stored host", []string{other}, failClosedCases(e, cookie))
	expect(t, e.root.do("PUT", "/telegram/bot", map[string]any{"enabled": true, "mode": "polling", "miniapp_host": other}, "If-Match", `"3"`), 200)
	expect(t, e.root.do("PUT", "/telegram/bot", map[string]any{"enabled": true, "mode": "polling", "miniapp_host": miniHost}, "If-Match", `"4"`), 200)

	// Disabled: everything on the host is a 404 again, the shell included.
	expect(t, e.root.do("PUT", "/telegram/bot", map[string]any{"enabled": false, "mode": "polling", "miniapp_host": miniHost}, "If-Match", `"5"`), 200)
	assertAll404(t, e, "disabled", hostVariants(e.port), failClosedCases(e, cookie))
	assertAll404(t, e, "disabled, Mini App session", []string{miniHost}, []failClosedCase{{"GET", api.Prefix + "/spaces", "", []string{"Authorization", "Telegram " + tok}}})
}

// noKeyEnv is a Mini App environment with telegram.miniapp_host unset, both web UIs embedded; start false leaves the
// bot service unstarted.
func noKeyEnv(t *testing.T, start bool) *miniAppEnv {
	t.Helper()
	return newMiniAppEnvStart(t, slog.New(slog.DiscardHandler), func(c *config.Config) { c.Telegram.MiniAppHost = "" }, func(c *app.Central) {
		c.UI = webui.Handler(dlFiles)
		c.MiniAppUI = webui.MiniAppHandler(dlFiles)
	}, start)
}

// seedLegacyHost stores a bot (without a host, the only kind a keyless central accepts) and writes miniapp_host into
// the row as an installation from before the key was required would have it. The service's memory is untouched: it
// is what a restarted central knows before it reads the row.
func seedLegacyHost(t *testing.T, e *miniAppEnv, enabled bool) {
	t.Helper()
	expect(t, e.root.do("PUT", "/telegram/bot", map[string]any{"bot_token": e.tg.Token(), "enabled": enabled, "mode": "polling"}), 200)
	e.exec(t, `UPDATE telegram_bot SET miniapp_host = ?`, miniHost)
}

// L-1: no telegram.miniapp_host and a stored but disabled bot whose miniapp_host is the probe host. The host reaches the
// Mini App handler only, loaded at start and again after the bot's own start; every path is a 404.
func TestMiniAppHostFailsClosedNoKeyDisabledBot(t *testing.T) {
	t.Parallel()
	e := noKeyEnv(t, true)
	cases := failClosedCases(e, "")
	seedLegacyHost(t, e, false)
	if err := e.h.c.TelegramBot.LoadStoredHost(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertAll404(t, e, "no key, disabled bot, loaded", hostVariants(e.port), cases)
	e.h.c.TelegramBot.Start(t.Context())
	assertAll404(t, e, "no key, disabled bot, started", hostVariants(e.port), cases)
	if e.h.c.TelegramBot.MiniAppHost() != "" {
		t.Fatal("a disabled bot serves the Mini App")
	}
	expect(t, e.send(t, "", "GET", api.Prefix+"/openapi.json", ""), 200) // the main host is untouched
}

// M-1: a stored host with no key reaches the Mini App handler from the first request, before the bot service has
// started (central reads the row before it listens). Without the read the same request is the main UI.
func TestMiniAppHostRoutedBeforeTheBotStarts(t *testing.T) {
	t.Parallel()
	e := noKeyEnv(t, false) // the bot service's start is still pending
	seedLegacyHost(t, e, true)
	if r := e.send(t, miniHost, "GET", "/", ""); r.status != 200 {
		t.Fatalf("before the stored host is known the probe host is the main host (%d): the test proves nothing", r.status)
	}
	if err := e.h.c.TelegramBot.LoadStoredHost(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertAll404(t, e, "stored host loaded, bot not started", hostVariants(e.port), failClosedCases(e, ""))
}

// M-1: with no key, a delete or an emptied host never hands the old host to the main router (until restart).
func TestMiniAppHostRetiredNoKey(t *testing.T) {
	t.Parallel()
	e := noKeyEnv(t, true)
	cases := failClosedCases(e, "")
	seedLegacyHost(t, e, true)
	if err := e.h.c.TelegramBot.LoadStoredHost(t.Context()); err != nil {
		t.Fatal(err)
	}
	// The host is refused on every write; emptying it is allowed and must not release the old host.
	if r := e.root.do("PUT", "/telegram/bot", map[string]any{"enabled": true, "mode": "polling", "miniapp_host": miniHost}, "If-Match", `"1"`); r.status != 422 ||
		!strings.Contains(string(r.body), "telegram.miniapp_host") {
		t.Fatalf("a host without the key: %d %s", r.status, r.body)
	}
	expect(t, e.root.do("PUT", "/telegram/bot", map[string]any{"enabled": true, "mode": "polling"}, "If-Match", `"1"`), 200)
	assertAll404(t, e, "host emptied", hostVariants(e.port), cases)
	expect(t, e.root.do("DELETE", "/telegram/bot", nil, "If-Match", `"2"`), 204)
	assertAll404(t, e, "bot deleted", hostVariants(e.port), cases)
}

// M-1: a bot row that can't be read refuses central's start, before anything listens.
func TestMiniAppServeRefusesWhenTheStoredHostCannotBeRead(t *testing.T) {
	t.Parallel()
	e := noKeyEnv(t, false)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := e.h.c.Serve(ctx)
	if err == nil || !strings.Contains(err.Error(), "miniapp_host") {
		t.Fatalf("Serve with an unreadable bot row: %v", err)
	}
}

// L-a: the stored host is loaded before the listener accepts a connection. The hook holds LoadStoredHost while a client
// hammers the real port with Host: <mini host>; Serve is not yet listening, so no request is answered (each is refused).
// Once the load is released, every answer from the first one on is the Mini App host's 404. A Serve that loads the host
// after the listener goroutine has started answers the hammering with the main UI while the load is held.
func TestMiniAppServeLoadsTheStoredHostBeforeListening(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	e := newMiniAppEnvStart(t, slog.New(slog.DiscardHandler), func(c *config.Config) {
		c.Telegram.MiniAppHost = ""
		c.HTTP.Listen = addr
	}, func(c *app.Central) {
		c.UI = webui.Handler(dlFiles)
		c.MiniAppUI = webui.MiniAppHandler(dlFiles)
	}, false)
	seedLegacyHost(t, e, false) // disabled: a started bot would legitimately serve the Mini App itself

	const heldAttempts = 150
	var attempts atomic.Int64
	release := make(chan struct{})
	var once sync.Once
	e.h.c.TelegramBot.SetLoadStoredHostHook(func(ctx context.Context) {
		// Hold until the client has tried enough times while the load is pending (or give up after a bound).
		deadline := time.After(10 * time.Second)
		for attempts.Load() < heldAttempts {
			select {
			case <-deadline:
				return
			case <-ctx.Done():
				return
			case <-time.After(time.Millisecond):
			}
		}
		once.Do(func() { close(release) })
	})

	ctx, cancel := context.WithCancel(t.Context())
	served := make(chan error, 1)
	go func() { served <- e.h.c.Serve(ctx) }()
	defer func() {
		cancel()
		select {
		case err := <-served:
			_ = err // the shutdown error is not what this test checks
		case <-time.After(20 * time.Second):
			t.Error("Serve did not stop")
		}
	}()

	hc := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	type answer struct {
		status int
		body   string
	}
	var answers []answer
	var firstRefused int
	deadline := time.Now().Add(15 * time.Second)
	for len(answers) < 20 && time.Now().Before(deadline) {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr+"/", nil)
		req.Host = miniHost
		resp, err := hc.Do(req)
		attempts.Add(1)
		if err != nil { // not listening yet
			if len(answers) == 0 {
				firstRefused++
			}
			time.Sleep(time.Millisecond)
			continue
		}
		b := make([]byte, 512)
		n, _ := resp.Body.Read(b)
		_ = resp.Body.Close()
		answers = append(answers, answer{resp.StatusCode, string(b[:n])})
	}
	if len(answers) == 0 {
		t.Fatalf("Serve never answered (%d refused)", firstRefused)
	}
	select {
	case <-release:
	default:
		t.Error("a request was answered before the held load was released: the listener is up before the stored host is loaded")
	}
	for i, a := range answers {
		if a.status != 404 || a.body != "not found\n" {
			t.Errorf("answer %d on Host %q: %d %q, want the Mini App host's 404", i, miniHost, a.status, a.body)
		}
	}
}
