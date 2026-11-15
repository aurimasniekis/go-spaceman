package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/api"
	"github.com/aurimasniekis/go-spaceman/internal/app"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/miniapp"
	webui "github.com/aurimasniekis/go-spaceman/web"
)

// Plan §3.4.1 and §3.4.3, TG-9a: tg.html with its own headers on the Mini App host only, and signed single-purpose
// downloads on a real central: 60 s, 3 uses, bound to one attachment and one link, re-authorized on every use, never
// cached, never with a referrer, and the ID in no log line.

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

var dlFiles = fstest.MapFS{
	"index.html":           {Data: []byte("<!doctype html><title>MainUI</title>")},
	"tg.html":              {Data: []byte("<!doctype html><title>MiniApp</title>")},
	"assets/app-abc123.js": {Data: []byte("console.log(1)")},
}

const dlBody = "the attachment bytes"

type dlEnv struct {
	*miniAppEnv
	logs *syncBuf
	att  string // an attachment of the space
	tok  string
}

func newDLEnv(t *testing.T) *dlEnv {
	t.Helper()
	logs := &syncBuf{}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	e := newMiniAppEnvWith(t, log, func(c *app.Central) {
		c.UI = webui.Handler(dlFiles)
		c.MiniAppUI = webui.MiniAppHandler(dlFiles)
	})
	e.startBot(t)
	_ = enrolledSession(t, e.h, "mini@example.com")
	a, err := e.h.c.Attachments.Upload(context.Background(), audit.System, e.space, "report.txt", strings.NewReader(dlBody))
	if err != nil {
		t.Fatal(err)
	}
	return &dlEnv{miniAppEnv: e, logs: logs, att: a.ID, tok: e.exchange(t)}
}

// mint asks for a link to an attachment; it returns the response and the link's ID ("" when refused).
func (e *dlEnv) mint(t *testing.T, space, att string) (resp, string) {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"space_id": space, "attachment_id": att})
	r := e.mini(t, "POST", api.Prefix+"/telegram/download-links", string(b), "Authorization", "Telegram "+e.tok)
	if r.status != 201 {
		return r, ""
	}
	var out api.TelegramDownloadOut
	r.json(t, &out)
	want := e.origin() + "/tg-dl/"
	if !strings.HasPrefix(out.URL, want) || out.MaxUses != 3 || !out.ExpiresAt.Equal(e.h.clk.Now().Add(60*time.Second)) {
		t.Fatalf("minted %+v, want a link under %s valid 60 s for 3 uses", out, want)
	}
	return r, strings.TrimPrefix(out.URL, want)
}

func (e *dlEnv) get(t *testing.T, id string, hdr ...string) resp {
	t.Helper()
	return e.send(t, miniHost+":"+e.port, "GET", "/tg-dl/"+id, "", hdr...)
}

func requireDLHeaders(t *testing.T, r resp) {
	t.Helper()
	for k, v := range map[string]string{"Cache-Control": "no-store", "Referrer-Policy": "no-referrer", "X-Content-Type-Options": "nosniff",
		"Access-Control-Allow-Origin": "https://web.telegram.org"} {
		if got := r.header.Get(k); got != v {
			t.Errorf("%s = %q, want %q (status %d)", k, got, v, r.status)
		}
	}
	if c := r.header.Get("Access-Control-Allow-Credentials"); c != "" {
		t.Errorf("Access-Control-Allow-Credentials = %q", c)
	}
	if len(r.header.Values("Set-Cookie")) > 0 {
		t.Error("a download set a cookie")
	}
}

func TestMiniAppShellHeadersAndHostSeparation(t *testing.T) {
	t.Parallel()
	e := newDLEnv(t)
	for _, p := range []string{"/", "/tg.html", "/inbox/inb_1"} {
		r := e.mini(t, "GET", p, "")
		if r.status != 200 || !strings.Contains(string(r.body), "MiniApp") || r.header.Get("Content-Security-Policy") != webui.MiniAppCSP ||
			r.header.Get("X-Frame-Options") != "" || r.header.Get("Cache-Control") != "no-cache" ||
			!strings.Contains(r.header.Get("Content-Security-Policy"), "frame-ancestors https://web.telegram.org;") {
			t.Errorf("Mini App host GET %s: %d %v %q", p, r.status, r.header, r.body)
		}
		if r.header.Get("Access-Control-Allow-Origin") != "" {
			t.Errorf("GET %s: CORS header on the shell", p)
		}
	}
	if r := e.mini(t, "GET", "/assets/app-abc123.js", ""); r.status != 200 || !strings.Contains(r.header.Get("Cache-Control"), "immutable") {
		t.Errorf("asset: %d %v", r.status, r.header)
	}
	if r := e.mini(t, "GET", "/assets/gone.js", ""); r.status != 404 {
		t.Errorf("missing asset: %d", r.status)
	}
	// Still never served here.
	for _, c := range []struct{ method, path string }{{"GET", "/agent/connect"}, {"POST", "/telegram/webhook/abc"}, {"GET", api.Prefix + "/auth/me"},
		{"POST", api.Prefix + "/mcp"}, {"POST", "/tg-dl/" + strings.Repeat("A", 43)}} {
		if r := e.mini(t, c.method, c.path, ""); r.status != 404 && r.status != 405 || strings.Contains(string(r.body), "MiniApp") {
			t.Errorf("Mini App host %s %s: %d %q", c.method, c.path, r.status, r.body)
		}
	}
	// The main host: tg.html is never the shell there, its headers are the UI's, and /tg-dl/ and the mint operation
	// are not served.
	for _, p := range []string{"/tg.html", "/", "/tg-dl/" + strings.Repeat("A", 43)} {
		r := e.send(t, "", "GET", p, "")
		if r.status != 200 || strings.Contains(string(r.body), "MiniApp") || r.header.Get("Content-Security-Policy") != webui.CSP ||
			r.header.Get("X-Frame-Options") != "DENY" || r.header.Get("Access-Control-Allow-Origin") != "" {
			t.Errorf("main host GET %s: %d %v %q", p, r.status, r.header, r.body)
		}
	}
	b, _ := json.Marshal(map[string]string{"space_id": e.space, "attachment_id": e.att})
	if r := e.send(t, "", "POST", api.Prefix+"/telegram/download-links", string(b), "Origin", e.h.srv.URL, "Authorization", "Telegram "+e.tok); r.status != 401 {
		t.Errorf("mint on the main host: %d %s", r.status, r.body)
	}
	if r := e.send(t, "", "POST", api.Prefix+"/telegram/download-links", string(b), "Origin", e.h.srv.URL, "Authorization", "Bearer "+e.root.bearer); r.status != 404 {
		t.Errorf("mint with the root token on the main host: %d %s", r.status, r.body)
	}
}

func TestMiniAppDownload(t *testing.T) {
	t.Parallel()
	e := newDLEnv(t)
	_, id := e.mint(t, e.space, e.att)
	if len(id) != 43 {
		t.Fatalf("ID %q", id)
	}
	r := e.get(t, id)
	if r.status != 200 || string(r.body) != dlBody {
		t.Fatalf("download: %d %q", r.status, r.body)
	}
	requireDLHeaders(t, r)
	if cd := r.header.Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment;") || !strings.Contains(cd, "report.txt") {
		t.Errorf("Content-Disposition = %q", cd)
	}
	if ct := r.header.Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("Content-Type = %q", ct)
	}
	if csp := r.header.Get("Content-Security-Policy"); csp != "default-src 'none'; sandbox" {
		t.Errorf("download Content-Security-Policy = %q", csp)
	}
	// Range retries of a download manager count as uses: 3 in all, then the link is gone.
	if r := e.get(t, id, "Range", "bytes=4-"); r.status != 206 || string(r.body) != dlBody[4:] {
		t.Errorf("range: %d %q", r.status, r.body)
	}
	if r := e.get(t, id); r.status != 200 {
		t.Errorf("third use: %d", r.status)
	}
	r = e.get(t, id)
	if r.status != 404 || strings.Contains(string(r.body), dlBody) {
		t.Errorf("fourth use: %d %q", r.status, r.body)
	}
	requireDLHeaders(t, r) // a refusal is no more cacheable than a download

	// A CORS preflight gets no CORS answer; other methods are refused.
	_, id = e.mint(t, e.space, e.att)
	if r := e.send(t, miniHost+":"+e.port, "OPTIONS", "/tg-dl/"+id, "", "Origin", "https://web.telegram.org", "Access-Control-Request-Method", "GET"); r.header.Get("Access-Control-Allow-Origin") != "" || r.status == 200 {
		t.Errorf("preflight: %d %v", r.status, r.header)
	}
	if r := e.send(t, miniHost+":"+e.port, "POST", "/tg-dl/"+id, ""); r.status == 200 || strings.Contains(string(r.body), dlBody) {
		t.Errorf("POST: %d", r.status)
	}
	if r := e.get(t, id); r.status != 200 {
		t.Errorf("the refused methods spent the link: %d", r.status)
	}
}

func TestMiniAppDownloadExpires(t *testing.T) {
	t.Parallel()
	e := newDLEnv(t)
	_, id := e.mint(t, e.space, e.att)
	e.h.clk.Advance(59 * time.Second)
	if r := e.get(t, id); r.status != 200 {
		t.Fatalf("at 59 s: %d", r.status)
	}
	e.h.clk.Advance(time.Second)
	if r := e.get(t, id); r.status != 404 {
		t.Fatalf("at 60 s: %d", r.status)
	}
	// The Mini App session lives on (idle is 15 min): only the link expired.
	if _, id := e.mint(t, e.space, e.att); e.get(t, id).status != 200 {
		t.Fatal("a fresh link does not work")
	}
}

func TestMiniAppDownloadBinding(t *testing.T) {
	t.Parallel()
	e := newDLEnv(t)
	other := "spc_other"
	e.exec(t, `INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at)
		VALUES (?, 'other-space', 'Other', 'nod_oidc', 'active', 'w', 0, 't', 0)`, other)
	foreign, err := e.h.c.Attachments.Upload(context.Background(), audit.System, other, "secret.txt", strings.NewReader("not yours"))
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string][2]string{
		"another space's attachment, in its space":       {other, foreign.ID},
		"another space's attachment named in this space": {e.space, foreign.ID},
		"this space's attachment named in another space": {other, e.att},
		"an attachment that does not exist":              {e.space, "att_missing"},
		"an unknown space":                               {"spc_nope", e.att},
	} {
		if r, id := e.mint(t, c[0], c[1]); r.status != 404 || id != "" {
			t.Errorf("%s: mint %d %s", name, r.status, r.body)
		}
	}
	// No session, a forged session and a Bearer token cannot mint.
	b, _ := json.Marshal(map[string]string{"space_id": e.space, "attachment_id": e.att})
	for _, hdr := range [][]string{nil, {"Authorization", "Telegram spaceman_tgs_" + strings.Repeat("A", 43)}, {"Authorization", "Bearer " + e.root.bearer}} {
		if r := e.mini(t, "POST", api.Prefix+"/telegram/download-links", string(b), hdr...); r.status != 401 {
			t.Errorf("mint with %v: %d", hdr, r.status)
		}
	}

	// A link is bound to the Telegram link it was minted for: its grants are checked on every use, so it stops working
	// when they narrow, and when the link is revoked, and a fresh one is not enough.
	_, id := e.mint(t, e.space, e.att)
	e.exec(t, `DELETE FROM role_grants WHERE subject = ?`, auth.TelegramSubject(e.linkID))
	if err := e.h.c.Authz.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r := e.get(t, id); r.status != 404 || strings.Contains(string(r.body), dlBody) {
		t.Errorf("after the grants were removed: %d %q", r.status, r.body)
	}
}

func TestMiniAppDownloadRevokedLink(t *testing.T) {
	t.Parallel()
	e := newDLEnv(t)
	_, id := e.mint(t, e.space, e.att)
	_, id2 := e.mint(t, e.space, e.att)
	if r := e.get(t, id2); r.status != 200 {
		t.Fatalf("before the revocation: %d", r.status)
	}
	e.exec(t, `UPDATE telegram_links SET state = 'revoked', revoked_at = 1 WHERE id = ?`, e.linkID)
	for _, i := range []string{id, id2} {
		if r := e.get(t, i); r.status != 404 || strings.Contains(string(r.body), dlBody) {
			t.Errorf("after the revocation: %d %q", r.status, r.body)
		}
	}
	// And the session can't mint any more.
	b, _ := json.Marshal(map[string]string{"space_id": e.space, "attachment_id": e.att})
	if r := e.mini(t, "POST", api.Prefix+"/telegram/download-links", string(b), "Authorization", "Telegram "+e.tok); r.status != 401 {
		t.Errorf("mint after the revocation: %d", r.status)
	}
}

func TestMiniAppDownloadMalformedIDs(t *testing.T) {
	t.Parallel()
	e := newDLEnv(t)
	_, id := e.mint(t, e.space, e.att)
	for _, bad := range []string{"x", strings.Repeat("A", 43), strings.Repeat("A", 44), id + "A", id[:42], "..%2f..%2fapi", strings.Repeat("%00", 43)} {
		r := e.get(t, bad)
		if r.status != 404 && r.status != 400 {
			t.Errorf("GET /tg-dl/%s: %d", bad, r.status)
		}
		if strings.Contains(string(r.body), dlBody) {
			t.Errorf("GET /tg-dl/%s served the file", bad)
		}
	}
	if r := e.get(t, id); r.status != 200 {
		t.Errorf("malformed lookups spent the link: %d", r.status)
	}
}

// Plan §3.4.3: request logs record /tg-dl/<redacted>. The ID and the Mini App session token appear in no log line, on
// success or refusal.
func TestMiniAppDownloadLogsRedactTheID(t *testing.T) {
	t.Parallel()
	e := newDLEnv(t)
	_, ok := e.mint(t, e.space, e.att)
	_, spent := e.mint(t, e.space, e.att)
	for i := 0; i < miniapp.DownloadMaxUses+1; i++ {
		e.get(t, spent)
	}
	e.get(t, ok)
	e.get(t, strings.Repeat("B", 43))
	e.h.clk.Advance(time.Minute)
	e.get(t, ok)

	out := e.logs.String()
	for name, secret := range map[string]string{"download ID": ok, "spent download ID": spent, "session token": e.tok, "unknown ID": strings.Repeat("B", 43)} {
		if strings.Contains(out, secret) {
			t.Errorf("a log line contains the %s", name)
		}
	}
	if n := strings.Count(out, "path=/tg-dl/<redacted>"); n < 7 {
		t.Errorf("%d redacted download log lines, want one per request (7):\n%s", n, out)
	}
	if strings.Contains(out, "/tg-dl/") && strings.Contains(strings.ReplaceAll(out, "/tg-dl/<redacted>", ""), "/tg-dl/") {
		t.Errorf("a log line names a /tg-dl/ path other than the redacted one:\n%s", out)
	}
}

// A link is held to a handful of live downloads: the 17th mint replaces its oldest (a session cannot crowd out the
// store, or anyone else's grants).
func TestMiniAppDownloadPerLinkCap(t *testing.T) {
	t.Parallel()
	e := newDLEnv(t)
	var ids []string
	for i := 0; i < 17; i++ {
		_, id := e.mint(t, e.space, e.att)
		if id == "" {
			t.Fatalf("mint %d refused", i)
		}
		ids = append(ids, id)
	}
	if r := e.get(t, ids[0]); r.status != 404 {
		t.Errorf("the oldest link past the cap: %d", r.status)
	}
	if r := e.get(t, ids[1]); r.status != 200 {
		t.Errorf("the second link: %d", r.status)
	}
	if r := e.get(t, ids[16]); r.status != 200 {
		t.Errorf("the newest link: %d", r.status)
	}
}

// A link minted before the user became Limited (MFA required, no factor) is refused on redemption, as the Mini App
// session would be.
func TestMiniAppDownloadLimitedPrincipal(t *testing.T) {
	t.Parallel()
	e := newDLEnv(t)
	_, id := e.mint(t, e.space, e.att)
	e.exec(t, `DELETE FROM mfa_totp`)
	if r := e.get(t, id); r.status != 404 || strings.Contains(string(r.body), dlBody) {
		t.Errorf("after the user lost MFA: %d %q", r.status, r.body)
	}
}
