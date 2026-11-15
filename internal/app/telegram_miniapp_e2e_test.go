package app_test

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/api"
	"github.com/aurimasniekis/go-spaceman/internal/app"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/config"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	tgsettings "github.com/aurimasniekis/go-spaceman/internal/telegram/settings"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/tgtest"
)

// End-to-end tests of the Mini App's host and session exchange (ADR-0083 §3.4.1–§3.4.2, TG-9a) on a real central with
// a fake Bot API: central routes on r.Host only, each host serves only its own routes, and the exchange accepts only
// fresh, correctly signed, unused initData of a linked user.

const miniHost = "tg.example.com"

type miniAppEnv struct {
	h      *harness
	tg     *tgtest.Server
	root   *client
	space  string
	userID string
	linkID string
	port   string
	seq    atomic.Int64
}

func newMiniAppEnv(t *testing.T) *miniAppEnv {
	t.Helper()
	return newMiniAppEnvWith(t, discard(), nil)
}

// newMiniAppEnvWith is newMiniAppEnv with central logging to log and pre run on central before its handler is built.
func newMiniAppEnvWith(t *testing.T, log *slog.Logger, pre func(*app.Central)) *miniAppEnv {
	t.Helper()
	return newMiniAppEnvFull(t, log, nil, pre)
}

// newMiniAppEnvFull is newMiniAppEnvWith with mod applied to central's configuration as well.
func newMiniAppEnvFull(t *testing.T, log *slog.Logger, mod func(*config.Config), pre func(*app.Central)) *miniAppEnv {
	t.Helper()
	return newMiniAppEnvStart(t, log, mod, pre, true)
}

// newMiniAppEnvStart is newMiniAppEnvFull; with start false the bot service never starts (a bot service whose start is
// still pending, or never happens, while requests arrive).
func newMiniAppEnvStart(t *testing.T, log *slog.Logger, mod func(*config.Config), pre func(*app.Central), start bool) *miniAppEnv {
	t.Helper()
	tg := tgtest.New(t, tgtest.Options{Limits: tgtest.Limits{Disabled: true}})
	h := newHarnessFull(t, log, func(c *config.Config) {
		c.Telegram.APIBaseURL = tg.URL()
		// A bot's miniapp_host needs the key: tests set it unless mod says otherwise.
		c.Telegram.MiniAppHost = miniHost
		if mod != nil {
			mod(c)
		}
	}, pre)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go h.c.TelegramUpdates.Run(ctx)
	go h.c.TelegramSettings.Run(ctx)
	if start {
		h.c.TelegramBot.Start(ctx)
	}
	e := &miniAppEnv{h: h, tg: tg, space: seedSpace(t, h), linkID: "tgl_mini"}
	u, _ := url.Parse(h.srv.URL)
	e.port = u.Port()
	rootSecret, _, err := h.c.Auth.IssueRootToken(ctx)
	if err != nil {
		t.Fatal(err)
	}
	e.root = h.client()
	e.root.bearer = rootSecret
	r := e.root.do("POST", "/users", map[string]any{"email": "mini@example.com", "display_name": "Mini", "password": "correct horse battery staple",
		"grants": []map[string]string{{"role": "operator", "domain": e.space}}})
	expect(t, r, 201)
	var usr struct {
		ID string `json:"id"`
	}
	r.json(t, &usr)
	e.userID = usr.ID
	return e
}

// startBot configures the bot with the Mini App host and links Telegram user 42 to the user (capped to operator).
func (e *miniAppEnv) startBot(t *testing.T) {
	t.Helper()
	expect(t, e.root.do("PUT", "/telegram/bot", map[string]any{"bot_token": e.tg.Token(), "enabled": true, "mode": "polling",
		"miniapp_host": miniHost}), 200)
	if e.h.c.TelegramBot.MiniAppHost() != miniHost {
		t.Fatalf("Mini App host %q", e.h.c.TelegramBot.MiniAppHost())
	}
	e.startBotLinks(t)
}

// startBotLinks links Telegram user 42 of the configured bot to the user (capped to operator).
func (e *miniAppEnv) startBotLinks(t *testing.T) {
	t.Helper()
	botID, _, _ := strings.Cut(e.tg.Token(), ":")
	e.exec(t, `INSERT INTO telegram_links (id, user_id, bot_id, tg_user_id, chat_id, state, created_at, updated_at) VALUES (?, ?, ?, 42, 42, 'active', 0, 0)`,
		e.linkID, e.userID, botID)
	e.exec(t, `INSERT INTO role_grants (subject, role, domain, source, created_at, created_by) VALUES (?, 'operator', ?, 'telegram', 0, 't')`,
		auth.TelegramSubject(e.linkID), e.space)
	if err := e.h.c.Authz.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func (e *miniAppEnv) exec(t *testing.T, q string, args ...any) {
	t.Helper()
	if err := e.h.c.Store.W().Tx(context.Background(), func(tx *store.Tx) error {
		_, err := tx.SQL.ExecContext(context.Background(), q, args...)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// origin is the Mini App's origin as a browser sends it: the menu button opens https://<miniapp_host>/, whatever public_url says.
func (e *miniAppEnv) origin() string { return "https://" + miniHost }

// send sends a request to central's listener with the given Host header (headers as name, value pairs).
func (e *miniAppEnv) send(t *testing.T, host, method, path, body string, hdr ...string) resp {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, e.h.srv.URL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	if host != "" {
		req.Host = host
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Add(hdr[i], hdr[i+1])
	}
	res, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return resp{status: res.StatusCode, header: res.Header, body: b}
}

// mini sends a request to the Mini App host from the Mini App's origin.
func (e *miniAppEnv) mini(t *testing.T, method, path, body string, hdr ...string) resp {
	t.Helper()
	return e.send(t, miniHost+":"+e.port, method, path, body, append([]string{"Origin", e.origin()}, hdr...)...)
}

// initData signs initData for a Telegram user as Telegram does, auth_date age before central's clock.
func (e *miniAppEnv) initData(token string, tgUser int64, age time.Duration) string {
	f := map[string]string{
		"user":      `{"id":` + strconv.FormatInt(tgUser, 10) + `,"first_name":"Mini"}`,
		"auth_date": strconv.FormatInt(e.h.clk.Now().Add(-age).Unix(), 10),
		"query_id":  "q" + strconv.FormatInt(e.seq.Add(1), 10),
	}
	keys := make([]string, 0, len(f))
	for k := range f {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var lines []string
	for _, k := range keys {
		lines = append(lines, k+"="+f[k])
	}
	sk := hmac.New(sha256.New, []byte("WebAppData"))
	sk.Write([]byte(token))
	m := hmac.New(sha256.New, sk.Sum(nil))
	m.Write([]byte(strings.Join(lines, "\n")))
	v := url.Values{}
	for k, x := range f {
		v.Set(k, x)
	}
	v.Set("hash", hex.EncodeToString(m.Sum(nil)))
	return v.Encode()
}

func exchangeBody(initData string) string {
	b, _ := json.Marshal(map[string]string{"init_data": initData})
	return string(b)
}

// exchange opens a Mini App session and returns its token.
func (e *miniAppEnv) exchange(t *testing.T) string {
	t.Helper()
	r := e.mini(t, "POST", api.Prefix+"/telegram/session", exchangeBody(e.initData(e.tg.Token(), 42, 0)))
	expect(t, r, 201)
	var s api.TelegramSessionOut
	r.json(t, &s)
	if !strings.HasPrefix(s.Token, "spaceman_tgs_") || s.IdleTimeoutSeconds != 900 || !s.ExpiresAt.Equal(e.h.clk.Now().Add(time.Hour)) {
		t.Fatalf("session %+v", s)
	}
	return s.Token
}

// SM-TG-008, plan §3.4.1: host separation. Main-host routes are unreachable on the Mini App host and Mini App routes
// on the main host; only r.Host routes, X-Forwarded-Host never; without a Mini App host nothing is routed to it.
func TestMiniAppHostSeparation(t *testing.T) {
	t.Parallel()
	e := newMiniAppEnv(t)
	main := ""

	// No bot yet: the configured Mini App host is routed to the Mini App handler, which answers 404 (fail closed).
	if r := e.send(t, miniHost, "GET", "/", ""); r.status != 404 {
		t.Fatalf("no bot: GET / on %s is %d", miniHost, r.status)
	}
	if r := e.send(t, miniHost, "POST", api.Prefix+"/telegram/session", exchangeBody("x"), "Origin", e.h.srv.URL); r.status != 404 {
		t.Fatalf("exchange without a Mini App host: %d %s", r.status, r.body)
	}
	e.startBot(t)

	// A web session on the main host, to try its cookie on the Mini App host.
	ui := enrolledSession(t, e.h, "mini@example.com")
	pub, _ := url.Parse(e.h.srv.URL)
	var cookie string
	for _, ck := range ui.hc.Jar.Cookies(pub) {
		if ck.Name == "sm_session" {
			cookie = ck.Name + "=" + ck.Value
		}
	}
	if cookie == "" {
		t.Fatal("no session cookie")
	}
	expect(t, e.send(t, main, "GET", api.Prefix+"/spaces", "", "Cookie", cookie), 200)
	tok := e.exchange(t)

	// Main-host routes on the Mini App host: not found, or (the API's own operations) never authenticated but by a
	// Mini App session.
	for _, c := range []struct {
		method, path, body string
		hdr                []string
		want               int
	}{
		{"GET", "/", "", nil, 404},
		{"GET", "/index.html", "", nil, 404},
		{"GET", "/spaces/x", "", nil, 404},
		{"GET", "/favicon.ico", "", nil, 404},
		{"GET", "/agent/connect", "", nil, 404},
		{"POST", "/telegram/webhook/abc", "{}", nil, 404},
		{"POST", api.Prefix + "/auth/login", `{"email":"mini@example.com","password":"correct horse battery staple"}`, nil, 404},
		{"GET", api.Prefix + "/auth/me", "", []string{"Cookie", cookie}, 404},
		{"GET", api.Prefix + "/auth/oidc/x/start", "", nil, 404},
		{"POST", api.Prefix + "/mcp", "{}", []string{"Authorization", "Telegram " + tok}, 404},
		{"GET", api.Prefix + "/openapi.json", "", nil, 404},
		{"GET", api.Prefix + "/system/info", "", nil, 401},
		{"GET", api.Prefix + "/spaces", "", []string{"Cookie", cookie}, 401},
		{"GET", api.Prefix + "/spaces", "", []string{"Authorization", "Bearer " + e.root.bearer}, 401},
		{"GET", api.Prefix + "/spaces", "", nil, 401},
	} {
		r := e.mini(t, c.method, c.path, c.body, c.hdr...)
		if r.status != c.want {
			t.Errorf("Mini App host %s %s: %d, want %d: %s", c.method, c.path, r.status, c.want, r.body)
		}
		if len(r.header.Values("Set-Cookie")) > 0 {
			t.Errorf("Mini App host %s %s set a cookie", c.method, c.path)
		}
	}
	// The Mini App session works there, and the cookie adds nothing (it is dropped).
	expect(t, e.mini(t, "GET", api.Prefix+"/spaces", "", "Authorization", "Telegram "+tok, "Cookie", cookie), 200)

	// Mini App routes on the main host: the exchange is not found there, and the Telegram scheme is refused before the
	// cookie is looked at.
	if r := e.send(t, main, "POST", api.Prefix+"/telegram/session", exchangeBody(e.initData(e.tg.Token(), 42, 0)), "Origin", e.h.srv.URL); r.status != 404 {
		t.Errorf("exchange on the main host: %d %s", r.status, r.body)
	}
	if r := e.send(t, main, "GET", api.Prefix+"/spaces", "", "Authorization", "Telegram "+tok); r.status != 401 {
		t.Errorf("Telegram session on the main host: %d", r.status)
	}
	if r := e.send(t, main, "GET", api.Prefix+"/spaces", "", "Authorization", "Telegram "+tok, "Cookie", cookie); r.status != 401 {
		t.Errorf("Telegram session plus cookie on the main host: %d", r.status)
	}
	if r := e.send(t, main, "GET", "/", ""); r.status != 200 {
		t.Errorf("main host UI: %d", r.status)
	}

	// X-Forwarded-Host never routes: the main host stays the main host, the Mini App host the Mini App host.
	if r := e.send(t, main, "POST", api.Prefix+"/telegram/session", exchangeBody(e.initData(e.tg.Token(), 42, 0)),
		"X-Forwarded-Host", miniHost, "Origin", e.h.srv.URL); r.status != 404 {
		t.Errorf("X-Forwarded-Host reached the exchange: %d %s", r.status, r.body)
	}
	if r := e.send(t, main, "GET", "/", "", "X-Forwarded-Host", miniHost); r.status != 200 {
		t.Errorf("X-Forwarded-Host moved a main-host request: %d", r.status)
	}
	if r := e.send(t, main, "GET", api.Prefix+"/spaces", "", "X-Forwarded-Host", miniHost, "Authorization", "Telegram "+tok); r.status != 401 {
		t.Errorf("X-Forwarded-Host let a Telegram session in on the main host: %d", r.status)
	}
	if r := e.mini(t, "GET", "/", "", "X-Forwarded-Host", pub.Host); r.status != 404 {
		t.Errorf("X-Forwarded-Host moved a Mini App request to the main host: %d", r.status)
	}
	if r := e.mini(t, "GET", api.Prefix+"/spaces", "", "X-Forwarded-Host", pub.Host, "Cookie", cookie); r.status != 401 {
		t.Errorf("X-Forwarded-Host let a cookie in on the Mini App host: %d", r.status)
	}

	// Host matching: case, port and one trailing dot don't matter (a host that differs only by them never reaches the
	// main host's routes under the Mini App name); anything else is not the Mini App host.
	for host, mini := range map[string]bool{"TG.Example.COM": true, "tg.EXAMPLE.com:8443": true, miniHost + ":443": true, miniHost: true,
		miniHost + ".": true, "TG.example.com.:80": true, miniHost + "..": false, "x." + miniHost: false, "tg.example.co": false,
		"tg-example.com": false} {
		r := e.send(t, host, "GET", "/", "")
		if got := r.status == 404; got != mini {
			t.Errorf("Host %q: status %d, Mini App host %v", host, r.status, mini)
		}
		if mini {
			if r := e.send(t, host, "POST", api.Prefix+"/auth/login", `{"email":"mini@example.com","password":"correct horse battery staple"}`); r.status != 404 {
				t.Errorf("Host %q: sign-in reachable (%d)", host, r.status)
			}
			if r := e.send(t, host, "GET", api.Prefix+"/spaces", "", "Cookie", cookie); r.status != 401 {
				t.Errorf("Host %q: cookie accepted (%d)", host, r.status)
			}
		}
	}

	// Turning the bot off never gives the host to the main router: it answers 404 for everything.
	expect(t, e.root.do("PUT", "/telegram/bot", map[string]any{"enabled": false, "mode": "polling"}, "If-Match", `"1"`), 200)
	if r := e.send(t, miniHost, "GET", "/", ""); r.status != 404 || strings.Contains(string(r.body), "MainUI") {
		t.Errorf("Mini App off: GET / on %s is %d", miniHost, r.status)
	}
}

// SM-TG-008, plan §3.4.2: the exchange accepts fresh, correctly signed, unused initData of a linked user only. Forged,
// wrong-hash, old, future-dated and replayed initData are 401; refusals are audited in aggregate; a cross-site or
// oversized exchange is refused; a session ends when its link is revoked or its user disabled.
func TestMiniAppSessionExchange(t *testing.T) {
	t.Parallel()
	e := newMiniAppEnv(t)
	e.startBot(t)
	_ = enrolledSession(t, e.h, "mini@example.com") // operators must enroll MFA; until then the link is Limited too
	path := api.Prefix + "/telegram/session"
	raw := e.initData(e.tg.Token(), 42, 30*time.Second)
	r := e.mini(t, "POST", path, exchangeBody(raw))
	expect(t, r, 201)
	if len(r.header.Values("Set-Cookie")) > 0 {
		t.Fatal("the exchange set a cookie")
	}
	var s api.TelegramSessionOut
	r.json(t, &s)
	auth := []string{"Authorization", "Telegram " + s.Token}
	if r := e.mini(t, "GET", api.Prefix+"/spaces", "", auth...); r.status != 200 || !strings.Contains(string(r.body), e.space) {
		t.Fatalf("spaces through the session: %d %s", r.status, r.body)
	}

	other := strings.Replace(e.tg.Token(), e.tg.Token()[len(e.tg.Token())-4:], "abcd", 1)
	wrongHash := raw[:len(raw)-4] + "0000"
	if strings.HasSuffix(raw, "0000") {
		wrongHash = raw[:len(raw)-4] + "1111"
	}
	for name, d := range map[string]string{
		"replayed":         raw,
		"forged":           e.initData(other, 42, 0),
		"wrong hash":       wrongHash,
		"older than 5 min": e.initData(e.tg.Token(), 42, 5*time.Minute+time.Second),
		"future auth_date": e.initData(e.tg.Token(), 42, -time.Minute),
	} {
		if r := e.mini(t, "POST", path, exchangeBody(d)); r.status != 401 {
			t.Errorf("%s: %d %s", name, r.status, r.body)
		}
	}
	// Refusals are audited once for the window, with no initData.
	var n int
	var details string
	if err := e.h.c.Store.ReadDB().QueryRowContext(context.Background(),
		`SELECT count(*), coalesce(max(details_json), '') FROM audit_events WHERE action = 'telegram.miniapp.session.refused'`).Scan(&n, &details); err != nil {
		t.Fatal(err)
	}
	if n != 1 || strings.Contains(details, "auth_date") || strings.Contains(details, "hash") {
		t.Errorf("refusal audit rows %d: %s", n, details)
	}

	// Unlinked user: 403; cross-site: 403; oversized: 413.
	if r := e.mini(t, "POST", path, exchangeBody(e.initData(e.tg.Token(), 99, 0))); r.status != 403 {
		t.Errorf("unlinked user: %d %s", r.status, r.body)
	}
	if r := e.send(t, miniHost+":"+e.port, "POST", path, exchangeBody(e.initData(e.tg.Token(), 42, 0)), "Origin", "https://evil.example"); r.status != 403 {
		t.Errorf("cross-site exchange: %d %s", r.status, r.body)
	}
	if r := e.send(t, miniHost+":"+e.port, "POST", path, exchangeBody(e.initData(e.tg.Token(), 42, 0)), "Origin", e.h.srv.URL); r.status != 403 {
		t.Errorf("exchange from the main origin: %d %s", r.status, r.body)
	}
	// The old origin, built from public_url's scheme and port, is no longer the Mini App's origin.
	if r := e.send(t, miniHost+":"+e.port, "POST", path, exchangeBody(e.initData(e.tg.Token(), 42, 0)), "Origin", "http://"+miniHost+":"+e.port); r.status != 403 {
		t.Errorf("exchange from the old http origin: %d %s", r.status, r.body)
	}
	if r := e.mini(t, "POST", path, exchangeBody(strings.Repeat("a", 9000))); r.status != 413 && r.status != 422 {
		t.Errorf("oversized exchange: %d", r.status)
	}

	// The session ends when the link is revoked, and stays ended when it comes back.
	e.exec(t, `UPDATE telegram_links SET state = 'revoked', revoked_at = 1 WHERE id = ?`, e.linkID)
	if r := e.mini(t, "GET", api.Prefix+"/spaces", "", auth...); r.status != 401 {
		t.Fatalf("revoked link: %d", r.status)
	}
	e.exec(t, `UPDATE telegram_links SET state = 'active', revoked_at = NULL WHERE id = ?`, e.linkID)
	if r := e.mini(t, "GET", api.Prefix+"/spaces", "", auth...); r.status != 401 {
		t.Fatalf("session revived with the link: %d", r.status)
	}

	// A disabled user's session ends. (Another administrator first: the user is the only one.)
	expect(t, e.root.do("POST", "/users", map[string]any{"email": "admin2@example.com", "display_name": "Admin", "password": "correct horse battery staple",
		"grants": []map[string]string{{"role": "admin", "domain": "*"}}}), 201)
	tok := e.exchange(t)
	setEnabled := func(on bool) {
		t.Helper()
		g := e.root.do("GET", "/users/"+e.userID, nil)
		expect(t, g, 200)
		op := "/disable"
		if on {
			op = "/enable"
		}
		expect(t, e.root.do("POST", "/users/"+e.userID+op, nil, "If-Match", g.header.Get("ETag")), 200)
	}
	setEnabled(false)
	if r := e.mini(t, "GET", api.Prefix+"/spaces", "", "Authorization", "Telegram "+tok); r.status != 401 {
		t.Fatalf("disabled user: %d", r.status)
	}
	setEnabled(true)

	// Idle and absolute lifetimes follow central's clock.
	tok = e.exchange(t)
	e.h.clk.Advance(16 * time.Minute)
	if r := e.mini(t, "GET", api.Prefix+"/spaces", "", "Authorization", "Telegram "+tok); r.status != 401 {
		t.Fatalf("idle session: %d", r.status)
	}
}

// A link_sso_grants change makes open streams re-check at once (principals are built per request anyway).
func TestMiniAppPolicyChangeSignalsStreams(t *testing.T) {
	t.Parallel()
	e := newMiniAppEnv(t)
	e.startBot(t)
	w := e.h.c.Auth.Revocations().Watch(&auth.Principal{User: &auth.User{ID: e.userID}, Method: auth.MethodTelegram, TelegramLinkID: e.linkID})
	defer w.Close()
	select {
	case <-w.C:
	default:
	}
	expect(t, e.root.do("PUT", "/telegram/settings", map[string]any{"link_sso_grants": true}), 200)
	select {
	case <-w.C:
	case <-time.After(10 * time.Second):
		t.Fatal("no re-check after link_sso_grants changed")
	}
	// Another setting changes nothing for streams.
	expect(t, e.root.do("PUT", "/telegram/settings", map[string]any{"link_sso_grants": true, "links_per_user": 2}, "If-Match", `"1"`), 200)
	eventually(t, "the settings are applied", func() bool { return e.h.c.TelegramSettings.Current().LinksPerUser == 2 })
	select {
	case <-w.C:
		t.Fatal("re-check without a policy change")
	default:
	}
}

// A per-space Telegram toggle-off of visibility or terminal_watch makes open streams re-check at once, not at the
// next StreamRevalidate; turning something on, or changing another space setting, does not.
func TestMiniAppSpaceToggleOffSignalsStreams(t *testing.T) {
	t.Parallel()
	e := newMiniAppEnv(t)
	e.startBot(t)
	ctx := context.Background()
	on, off := true, false
	put := func(version int64, in tgsettings.SpaceInput) {
		t.Helper()
		if _, err := e.h.c.TelegramSettings.PutSpace(ctx, audit.System, e.space, version, in); err != nil {
			t.Fatal(err)
		}
	}
	w := e.h.c.Auth.Revocations().Watch(&auth.Principal{User: &auth.User{ID: e.userID}, Method: auth.MethodTelegram, TelegramLinkID: e.linkID})
	defer w.Close()
	select { // a fresh watch starts with a pending signal
	case <-w.C:
	default:
	}
	quiet := func(what string) {
		t.Helper()
		select {
		case <-w.C:
			t.Fatalf("re-check without a toggle-off: %s", what)
		default:
		}
	}
	signalled := func(what string) {
		t.Helper()
		select {
		case <-w.C:
		case <-time.After(10 * time.Second):
			t.Fatalf("no re-check after %s", what)
		}
	}
	put(0, tgsettings.SpaceInput{Enabled: &on, TerminalWatch: &on})
	quiet("turning on")
	put(1, tgsettings.SpaceInput{Enabled: &on, TerminalWatch: &on, Topics: &on})
	quiet("another setting")
	put(2, tgsettings.SpaceInput{Enabled: &on})
	signalled("terminal_watch went off")
	put(3, tgsettings.SpaceInput{Enabled: &on, TerminalWatch: &on})
	quiet("terminal_watch back on")
	put(4, tgsettings.SpaceInput{Enabled: &off, TerminalWatch: &on})
	signalled("the space stopped being visible")
}

// Integration (TG-9a with TG-4b): a Mini App session and its change-feed stream are rebuilt through
// auth.Service.TelegramPrincipal, the link service's path, so an unlink reaches them: the stream ends with an
// unauthenticated end event and the session answers 401 from then on.
func TestMiniAppSessionAndStreamEndOnUnlink(t *testing.T) {
	t.Parallel()
	// The periodic re-check is pushed out so only the event-driven path can end the stream within revocationBound.
	e := newMiniAppEnvWith(t, discard(), func(c *app.Central) { c.Cfg.Auth.StreamRevalidate = config.Duration(time.Minute) })
	e.startBot(t)
	_ = enrolledSession(t, e.h, "mini@example.com") // the link is Limited until its user has enrolled MFA
	tok := e.exchange(t)
	auth := []string{"Authorization", "Telegram " + tok}
	expect(t, e.mini(t, "GET", api.Prefix+"/spaces", "", auth...), 200)

	req, err := http.NewRequest(http.MethodGet, e.h.srv.URL+api.Prefix+"/watch?live=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = miniHost + ":" + e.port
	req.Header.Set("Origin", e.origin())
	req.Header.Set("Authorization", "Telegram "+tok)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	res, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("watch: %d", res.StatusCode)
	}
	br := bufio.NewReader(res.Body)
	readUntil := func(prefix string) string {
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				t.Fatalf("stream ended before %q: %v", prefix, err)
			}
			if strings.HasPrefix(line, prefix) {
				return line
			}
		}
	}
	readUntil("event: position")

	// The real unlink path (an administrator's), not a row edit.
	expect(t, e.root.do("DELETE", "/telegram/links/"+e.linkID, nil), 204)

	start := time.Now()
	readUntil("event: end")
	if line := readUntil("data:"); !strings.Contains(line, api.EndUnauthenticated) {
		t.Fatalf("end reason: %s", line)
	}
	if d := time.Since(start); d > revocationBound {
		t.Fatalf("stream ended after %s", d)
	}
	if r := e.mini(t, "GET", api.Prefix+"/spaces", "", auth...); r.status != 401 {
		t.Fatalf("session after unlink: %d", r.status)
	}
}

// get-telegram-me goes through the middleware's Mini App rule (Session and TelegramSelf): a real Mini App session gets
// the linked user's own Me on the Mini App host, the main host serves nothing there (404), and no session is a 401.
func TestMiniAppMeThroughMiddleware(t *testing.T) {
	t.Parallel()
	e := newMiniAppEnv(t)
	e.startBot(t)
	_ = enrolledSession(t, e.h, "mini@example.com") // the link is Limited until its user has enrolled MFA
	tok := e.exchange(t)
	hdr := []string{"Authorization", "Telegram " + tok}

	r := e.mini(t, "GET", api.Prefix+"/telegram/me", "", hdr...)
	expect(t, r, 200)
	var me api.Me
	r.json(t, &me)
	if me.User == nil || me.User.ID != e.userID || me.Method != auth.MethodTelegram || me.Limited || me.CSRFToken != "" {
		t.Fatalf("me %+v", me)
	}
	if !slices.Contains(me.Domains, e.space) {
		t.Fatalf("domains %v lack %s", me.Domains, e.space)
	}
	// The main host: a web principal reaches the handler and is not found; the Mini App's token is no credential there.
	expect(t, e.root.do("GET", "/telegram/me", nil), 404)
	if r := e.send(t, "", "GET", api.Prefix+"/telegram/me", "", hdr...); r.status != 401 {
		t.Fatalf("main host with a Mini App token: %d", r.status)
	}
	if r := e.mini(t, "GET", api.Prefix+"/telegram/me", ""); r.status != 401 {
		t.Fatalf("no session: %d", r.status)
	}
}

// A Limited principal is refused (resolve, and the middleware behind it): when MFA becomes required for the user (here: their factor is gone) while a
// Mini App session is open, its next request is a 401 and not served.
func TestMiniAppSessionRefusedWhenMFABecomesRequired(t *testing.T) {
	t.Parallel()
	e := newMiniAppEnv(t)
	e.startBot(t)
	_ = enrolledSession(t, e.h, "mini@example.com")
	tok := e.exchange(t)
	hdr := []string{"Authorization", "Telegram " + tok}
	expect(t, e.mini(t, "GET", api.Prefix+"/spaces", "", hdr...), 200)

	e.exec(t, `DELETE FROM mfa_totp`)
	for _, path := range []string{"/spaces", "/telegram/me"} {
		if r := e.mini(t, "GET", api.Prefix+path, "", hdr...); r.status != 401 {
			t.Fatalf("%s after MFA became required: %d %s", path, r.status, r.body)
		}
	}
}
