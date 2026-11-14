package app_test

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/aurimasniekis/go-spaceman/internal/api"
	"github.com/aurimasniekis/go-spaceman/internal/app"
	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/telegram"
)

// End-to-end tests of the Telegram security core (ADR-0083 §3.2–§3.4, TG-4a) on a real central: the classification is
// exhaustive, every personal or step-up operation refuses a Telegram principal, the main host never lets the Telegram
// scheme combine with a cookie, and the Mini App's Telegram mode authorizes as the link intersected with the user.

func specOps(t *testing.T, d *api.Deps) []*huma.Operation {
	t.Helper()
	var ops []*huma.Operation
	for _, item := range api.Register(http.NewServeMux(), d).OpenAPI().Paths {
		for _, op := range []*huma.Operation{item.Get, item.Post, item.Put, item.Patch, item.Delete} {
			if op != nil {
				ops = append(ops, op)
			}
		}
	}
	slices.SortFunc(ops, func(a, b *huma.Operation) int { return strings.Compare(a.OperationID, b.OperationID) })
	return ops
}

// Every huma operation, every raw route (Deps.Raw) and the change feed has a reviewed Telegram class (§3.3): a new one
// fails here until someone classifies it. The table names only real operations, uses valid classes, never lets
// Telegram reach an operation its rule auto-excludes, and keeps the exchange class for public operations.
func TestTelegramClassificationIsExhaustive(t *testing.T) {
	t.Parallel()
	d := app.SpecDeps()
	ops := specOps(t, d)
	ids := map[string]api.Rule{}
	for _, op := range ops {
		r, ok := d.RuleFor(op.OperationID)
		if !ok {
			t.Errorf("%s has no rule", op.OperationID)
			continue
		}
		ids[op.OperationID] = r
		c, ok := telegram.Classify(op.OperationID, r)
		if !ok {
			t.Errorf("operation %s has no Telegram class: add it to telegram.Ops (internal/telegram/ops.go); anything but excluded needs a security review (ADR-0083 §3.3)", op.OperationID)
			continue
		}
		if !slices.Contains(api.TelegramClasses, c) {
			t.Errorf("%s: class %q", op.OperationID, c)
		}
	}
	for id, c := range telegram.Ops {
		r, ok := ids[id]
		if !ok {
			t.Errorf("telegram.Ops names %s, which the API lacks", id)
			continue
		}
		if !slices.Contains(api.TelegramClasses, c) {
			t.Errorf("telegram.Ops %s: class %q", id, c)
		}
		if c == api.TelegramExchange && !r.Public {
			t.Errorf("%s: the exchange class is for a public operation only", id)
		}
		if why, auto := telegram.AutoExcluded(r); auto && c != api.TelegramExcluded && c != api.TelegramExchange {
			t.Errorf("%s is %s but its rule excludes it (%s)", id, c, why)
		}
		if r.Reauth || r.MFA {
			if c != api.TelegramExcluded {
				t.Errorf("%s needs step-up and must be excluded", id)
			}
		}
	}
	// Raw routes: every route central serves (SpecDeps lists the same ones) has a class; the plan's table holds.
	want := map[string]api.TelegramClass{
		"GET /api/v1/spaces/{space_id}/attachments/{attachment_id}/content": api.TelegramMiniApp,
		"GET /api/v1/spaces/{space_id}/runs/{run_id}/terminal":              api.TelegramMiniApp,
		"GET /api/v1/spaces/{space_id}/runs/{run_id}/recording/download":    api.TelegramExcluded,
		"GET /api/v1/spaces/{space_id}/runs/{run_id}/transcript":            api.TelegramExcluded,
		"GET /api/v1/spaces/{space_id}/export":                              api.TelegramExcluded,
	}
	if len(d.Raw) == 0 {
		t.Fatal("SpecDeps lists no raw routes")
	}
	for _, rt := range d.Raw {
		if !slices.Contains(api.TelegramClasses, rt.Telegram) || rt.Telegram == api.TelegramExchange {
			t.Errorf("raw route %s has no valid Telegram class (%q): set RawRoute.Telegram (ADR-0083 §3.3)", rt.Pattern, rt.Telegram)
		}
		if w, ok := want[rt.Pattern]; ok && w != rt.Telegram {
			t.Errorf("raw route %s: class %s, want %s", rt.Pattern, rt.Telegram, w)
		}
		if strings.HasSuffix(rt.Pattern, "/terminal") && rt.TelegramFeature != api.TelegramFeatureTerminalWatch {
			t.Errorf("the terminal stream must need terminal_watch, has %q", rt.TelegramFeature)
		}
	}
	for p := range want {
		if !slices.ContainsFunc(d.Raw, func(rt api.RawRoute) bool { return rt.Pattern == p }) {
			t.Errorf("raw route %s is gone: update this test and the plan's table", p)
		}
	}
	if !slices.Contains(api.TelegramClasses, telegram.Watch) || telegram.Watch == api.TelegramExchange {
		t.Errorf("the change feed has no valid Telegram class: %q", telegram.Watch)
	}
}

// telegramSetup seeds a space, a user with operator in it, and an active link capped to operator there.
type telegramSetup struct {
	h       *harness
	space   string
	userID  string
	linkID  string
	root    *client
	ui      *client // the user's web session, MFA enrolled (operators must enroll)
	minisrv *httptest.Server
	deps    *api.Deps
}

func newTelegramSetup(t *testing.T) *telegramSetup {
	t.Helper()
	h := newHarness(t)
	s := &telegramSetup{h: h, space: seedSpace(t, h), linkID: "tgl_e2e"}
	rootSecret, _, _ := h.c.Auth.IssueRootToken(context.Background())
	s.root = h.client()
	s.root.bearer = rootSecret
	r := s.root.do("POST", "/users", map[string]any{"email": "tg@example.com", "display_name": "Tg", "password": "correct horse battery staple",
		"grants": []map[string]string{{"role": "operator", "domain": s.space}}})
	expect(t, r, 201)
	var u struct {
		ID string `json:"id"`
	}
	r.json(t, &u)
	s.userID = u.ID
	s.ui = enrolledSession(t, h, "tg@example.com")
	s.exec(t, `INSERT INTO telegram_links (id, user_id, bot_id, tg_user_id, chat_id, state, created_at, updated_at) VALUES (?, ?, 1, 42, 42, 'active', 0, 0)`,
		s.linkID, s.userID)
	s.setLinkRole(t, "operator")
	// The Mini App host: the real API in Telegram mode, with sessions "good" → this link.
	s.deps = h.c.APIDeps()
	s.deps.Telegram.Sessions = linkSessions{c: h.c, tokens: map[string]string{"good": s.linkID}}
	s.deps.Telegram.SpaceFeature = func(context.Context, string, string) bool { return false }
	s.minisrv = httptest.NewUnstartedServer(s.deps.TelegramHandler())
	s.minisrv.Start()
	t.Cleanup(s.minisrv.Close)
	s.deps.Telegram.MiniAppOrigin = func() string { return s.minisrv.URL }
	return s
}

func (s *telegramSetup) exec(t *testing.T, q string, args ...any) {
	t.Helper()
	if err := s.h.c.Store.W().Tx(context.Background(), func(tx *store.Tx) error {
		_, err := tx.SQL.ExecContext(context.Background(), q, args...)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// setLinkRole replaces the link subject's grants (TG-4b will do this through insertGrants) and reloads the engine.
func (s *telegramSetup) setLinkRole(t *testing.T, role string) {
	t.Helper()
	s.exec(t, `DELETE FROM role_grants WHERE subject = ?`, auth.TelegramSubject(s.linkID))
	s.exec(t, `INSERT INTO role_grants (subject, role, domain, source, created_at, created_by) VALUES (?, ?, ?, 'telegram', 0, 't')`,
		auth.TelegramSubject(s.linkID), role, s.space)
	if err := s.h.c.Authz.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// mini sends a request to the Mini App host.
func (s *telegramSetup) mini(t *testing.T, method, path, body string, hdr ...string) resp {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, s.minisrv.URL+api.Prefix+path, rd)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Origin", s.minisrv.URL)
	for i := 0; i+1 < len(hdr); i += 2 {
		if hdr[i] == "Origin" {
			req.Header.Set(hdr[i], hdr[i+1])
			continue
		}
		req.Header.Add(hdr[i], hdr[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return resp{status: res.StatusCode, header: res.Header, body: b}
}

// linkSessions stands in for TG-9a's Mini App sessions: a token names a link, resolved on every request.
type linkSessions struct {
	c      *app.Central
	tokens map[string]string
}

func (l linkSessions) Authenticate(ctx context.Context, tok string) (*auth.Principal, error) {
	return l.Validate(ctx, tok)
}

func (l linkSessions) Validate(ctx context.Context, tok string) (*auth.Principal, error) {
	id, ok := l.tokens[tok]
	if !ok {
		return nil, auth.ErrUnauthorized
	}
	return l.c.Auth.TelegramPrincipal(ctx, id, auth.TelegramOptions{})
}

var pathParam = regexp.MustCompile(`\{[a-z_]+\}`)

// Every operation whose rule needs step-up, or is personal (Session or UserOnly) without being Telegram self-service,
// refuses a Telegram principal from the bot (§3.2 change 2), even with every operation classified as allowed and the
// link and user holding admin everywhere: the refusal itself is load-bearing, not the classification or the grants.
// It never succeeds: with the refusal removed, step-up still refuses (the principal's AuthAt is zero).
func TestTelegramRefusalIsExhaustive(t *testing.T) {
	t.Parallel()
	s := newTelegramSetup(t)
	s.exec(t, `INSERT INTO role_grants (subject, role, domain, source, created_at, created_by) VALUES (?, 'admin', '*', 'manual', 0, 't'), (?, 'admin', '*', 'telegram', 0, 't')`,
		auth.UserSubject(s.userID), auth.TelegramSubject(s.linkID))
	if err := s.h.c.Authz.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	d := s.h.c.APIDeps()
	all := map[string]api.TelegramClass{}
	ops := specOps(t, d)
	for _, op := range ops {
		all[op.OperationID] = api.TelegramBoth
	}
	d.Telegram = &api.TelegramPolicy{Ops: all, Watch: api.TelegramBoth}
	h := d.InProcessHandler()
	p, err := s.h.c.Auth.TelegramPrincipal(context.Background(), s.linkID, auth.TelegramOptions{})
	if err != nil {
		t.Fatal(err)
	}
	refused := 0
	for _, op := range ops {
		r, _ := d.RuleFor(op.OperationID)
		personal := r.Reauth || r.MFA || (!r.TelegramSelf && (r.Session && !r.Filtered || r.UserOnly))
		if r.Public || !personal {
			continue
		}
		refused++
		path := pathParam.ReplaceAllStringFunc(op.Path, func(m string) string {
			if m == "{space_id}" {
				return s.space
			}
			return "x"
		})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(op.Method, api.Prefix+path, strings.NewReader("{}")).WithContext(api.WithInProcessPrincipal(context.Background(), p)))
		if rec.Code < 400 {
			t.Errorf("SUCCEEDED: %s (%+v) answered %d for a Telegram principal", op.OperationID, r, rec.Code)
			continue
		}
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "not available from Telegram") {
			t.Errorf("%s (%+v): %d %s, want the Telegram refusal", op.OperationID, r, rec.Code, rec.Body.String())
		}
	}
	if refused < 50 {
		t.Fatalf("only %d personal or step-up operations: the loop is not seeing the rule table", refused)
	}
	// A filtered read stays open, so the refusal isn't just "everything fails".
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, api.Prefix+"/spaces", nil).WithContext(api.WithInProcessPrincipal(context.Background(), p)))
	if rec.Code != http.StatusOK {
		t.Fatalf("list-spaces: %d %s", rec.Code, rec.Body.String())
	}
}

// Host isolation, the authentication half (§3.4.1, B4). The main host answers any Telegram scheme with 401 even with
// a valid session cookie; the Mini App host drops the cookie (anonymous), refuses bearer API and root tokens, and
// authorizes a Telegram session as the link intersected with the user's current grants.
func TestTelegramHostIsolation(t *testing.T) {
	t.Parallel()
	s := newTelegramSetup(t)
	ui := s.ui
	expect(t, ui.do("GET", "/auth/me", nil), 200)
	for _, v := range []string{"Telegram good", "telegram good", "TELEGRAM x", "Telegram"} {
		expect(t, ui.do("GET", "/auth/me", nil, "Authorization", v), 401)
		expect(t, ui.do("POST", "/spaces/"+s.space+"/inbox", map[string]string{"title": "x"}, "Authorization", v), 401)
	}

	// The Mini App host: the session cookie is dropped, so the request is anonymous.
	u := s.h.srv.URL
	cookies := ui.hc.Jar.Cookies(mustURL(t, u))
	if len(cookies) == 0 {
		t.Fatal("no session cookie")
	}
	ck := cookies[0].Name + "=" + cookies[0].Value
	expect(t, s.mini(t, "GET", "/auth/me", "", "Cookie", ck), 401)
	expect(t, s.mini(t, "GET", "/spaces/"+s.space+"/inbox", "", "Cookie", ck), 401)
	r := ui.do("GET", "/spaces/"+s.space+"/inbox", nil)
	expect(t, r, 200)
	expect(t, s.mini(t, "GET", "/system/info", ""), 401)
	expect(t, s.mini(t, "GET", "/spaces/"+s.space+"/inbox", "", "Authorization", "Bearer "+s.root.bearer), 401)
	expect(t, s.mini(t, "GET", "/spaces/"+s.space+"/inbox", "", "Authorization", "Telegram nope"), 401)

	// A Telegram session reads and writes the inbox (class both), with no CSRF token, and the audit says so.
	r = s.mini(t, "GET", "/spaces/"+s.space+"/inbox", "", "Authorization", "Telegram good", "Cookie", ck)
	expect(t, r, 200)
	if r.header.Get("Set-Cookie") != "" {
		t.Fatalf("Set-Cookie on the Mini App host: %q", r.header.Get("Set-Cookie"))
	}
	expect(t, s.mini(t, "POST", "/spaces/"+s.space+"/inbox", `{"title":"from telegram","body":"b"}`, "Authorization", "Telegram good"), 201)
	audit := s.root.do("GET", "/audit?action=inbox.item_created", nil)
	expect(t, audit, 200)
	if !strings.Contains(string(audit.body), `"via":"telegram:`+s.linkID+`"`) || !strings.Contains(string(audit.body), `"channel":"telegram"`) {
		t.Fatalf("audit lacks the Telegram via and channel: %s", audit.body)
	}
	// From a foreign origin the same mutation is refused.
	expect(t, s.mini(t, "POST", "/spaces/"+s.space+"/inbox", `{"title":"x","body":"b"}`, "Authorization", "Telegram good", "Origin", "https://evil.example"), 403)
	// Excluded: a step-up op, an op outside the allowlist, an excluded raw route, the terminal without terminal_watch.
	expect(t, s.mini(t, "GET", "/auth/sessions", "", "Authorization", "Telegram good"), 403)
	expect(t, s.mini(t, "GET", "/spaces/"+s.space+"/memory", "", "Authorization", "Telegram good"), 403)
	expect(t, s.mini(t, "GET", "/spaces/"+s.space+"/export", "", "Authorization", "Telegram good"), 403)
	expect(t, s.mini(t, "GET", "/spaces/"+s.space+"/runs/run_x/transcript", "", "Authorization", "Telegram good"), 403)
	expect(t, s.mini(t, "GET", "/spaces/"+s.space+"/runs/run_x/recording/download", "", "Authorization", "Telegram good"), 403)
	expect(t, s.mini(t, "GET", "/spaces/"+s.space+"/runs/run_x/terminal", "", "Authorization", "Telegram good"), 403)
	expect(t, s.mini(t, "POST", "/mcp", `{}`, "Authorization", "Telegram good"), 404)

	// The link cap applies at request time: viewer can read, not write.
	s.setLinkRole(t, "viewer")
	expect(t, s.mini(t, "GET", "/spaces/"+s.space+"/inbox", "", "Authorization", "Telegram good"), 200)
	expect(t, s.mini(t, "POST", "/spaces/"+s.space+"/inbox", `{"title":"x","body":"b"}`, "Authorization", "Telegram good"), 403)
	// So does the user's: with the user's grant gone, the link's own grant counts for nothing.
	s.setLinkRole(t, "operator")
	s.exec(t, `DELETE FROM role_grants WHERE subject = ?`, auth.UserSubject(s.userID))
	if err := s.h.c.Authz.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	expect(t, s.mini(t, "GET", "/spaces/"+s.space+"/inbox", "", "Authorization", "Telegram good"), 404)
	// A revoked link is refused on the next request.
	s.exec(t, `UPDATE telegram_links SET state = 'revoked', revoked_at = 1 WHERE id = ?`, s.linkID)
	expect(t, s.mini(t, "GET", "/spaces", "", "Authorization", "Telegram good"), 401)
}

// The change feed on the Mini App host (§3.3, §3.4.1): a Telegram session streams it through the Telegram-mode
// wrapper, and the stream ends as soon as its link is revoked (the link's revocation signal and the session re-check).
func TestTelegramWatchStreamEndsOnRevoke(t *testing.T) {
	t.Parallel()
	s := newTelegramSetup(t)
	req, _ := http.NewRequest(http.MethodGet, s.minisrv.URL+api.Prefix+"/watch?live=1", nil)
	req.Header.Set("Authorization", "Telegram good")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "text/event-stream" || res.Header.Get("Set-Cookie") != "" {
		t.Fatalf("watch: %d %v", res.StatusCode, res.Header)
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
	if err := s.h.c.Store.W().Tx(context.Background(), func(tx *store.Tx) error {
		if _, err := tx.SQL.ExecContext(context.Background(), `UPDATE telegram_links SET state = 'revoked', revoked_at = 1 WHERE id = ?`, s.linkID); err != nil {
			return err
		}
		s.h.c.Auth.TelegramLinkChangedAfterCommit(tx, s.linkID)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	readUntil("event: end")
	if line := readUntil("data:"); !strings.Contains(line, api.EndUnauthenticated) {
		t.Fatalf("end reason: %s", line)
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// enrolledSession signs the user in, enrolls TOTP and verifies it: a full web session with the CSRF token set.
func enrolledSession(t *testing.T, h *harness, email string) *client {
	t.Helper()
	ui := h.client()
	r := ui.do("POST", "/auth/login", map[string]string{"email": email, "password": "correct horse battery staple"})
	expect(t, r, 200)
	var me api.Me
	r.json(t, &me)
	ui.csrf = me.CSRFToken
	r = ui.do("POST", "/auth/mfa/enroll", nil)
	var enr api.TOTPEnrollment
	r.json(t, &enr)
	secret, _ := auth.DecodeTOTPSecret(enr.Secret)
	expect(t, ui.do("POST", "/auth/mfa/enroll/confirm", map[string]string{"code": auth.TOTPCode(secret, auth.TOTPStep(h.clk.Now()))}), 200)
	h.clk.Advance(30 * time.Second)
	r = ui.do("POST", "/auth/mfa/verify", map[string]string{"code": auth.TOTPCode(secret, auth.TOTPStep(h.clk.Now()))})
	expect(t, r, 200)
	r.json(t, &me)
	if me.Limited {
		t.Fatal("session still limited after MFA")
	}
	ui.csrf = me.CSRFToken
	return ui
}
