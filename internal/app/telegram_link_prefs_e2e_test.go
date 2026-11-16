package app_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/api"
	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/eventbus"
	tglink "github.com/aurimasniekis/go-spaceman/internal/telegram/link"
)

const prefsPath = api.Prefix + "/telegram/link/prefs"

// prefsEnv is a Mini App session of link tgl_mini (Telegram user 42, user mini@), plus a second link of the same user
// (tgl_mini2, Telegram user 43) and another user's link (tgl_other, Telegram user 44), both with stored preferences.
type prefsEnv struct {
	*miniAppEnv
	tok     string
	otherID string
	web     *client // mini@'s enrolled web session on the main host
}

func newPrefsEnv(t *testing.T) *prefsEnv {
	t.Helper()
	e := newMiniAppEnv(t)
	e.startBot(t)
	web := enrolledSession(t, e.h, "mini@example.com")
	r := e.root.do("POST", "/users", map[string]any{"email": "other@example.com", "display_name": "Other", "password": "correct horse battery staple",
		"grants": []map[string]string{{"role": "admin", "domain": e.space}}})
	expect(t, r, 201)
	var u struct {
		ID string `json:"id"`
	}
	r.json(t, &u)
	botID, _, _ := strings.Cut(e.tg.Token(), ":")
	e.exec(t, `INSERT INTO telegram_links (id, user_id, bot_id, tg_user_id, chat_id, state, created_at, updated_at, prefs_json) VALUES ('tgl_mini2', ?, ?, 43, 43, 'active', 0, 0, '{"digest":false}')`,
		e.userID, botID)
	e.exec(t, `INSERT INTO telegram_links (id, user_id, bot_id, tg_user_id, chat_id, state, created_at, updated_at, prefs_json) VALUES ('tgl_other', ?, ?, 44, 44, 'active', 0, 0, '{"digest":false}')`,
		u.ID, botID)
	return &prefsEnv{miniAppEnv: e, tok: e.exchange(t), otherID: u.ID, web: web}
}

func (e *prefsEnv) get(t *testing.T, path string) resp {
	t.Helper()
	return e.mini(t, "GET", path, "", "Authorization", "Telegram "+e.tok)
}

func (e *prefsEnv) put(t *testing.T, path string, version string, body any) resp {
	t.Helper()
	b, _ := json.Marshal(body)
	return e.mini(t, "PUT", path, string(b), "Authorization", "Telegram "+e.tok, "If-Match", version)
}

// row returns a link's prefs_json, max_role, spaces_json and version.
func (e *prefsEnv) row(t *testing.T, id string) (prefs, maxRole, spaces string, version int64) {
	t.Helper()
	if err := e.h.c.Store.ReadDB().QueryRowContext(context.Background(), `SELECT prefs_json, max_role, spaces_json, version FROM telegram_links WHERE id = ?`, id).
		Scan(&prefs, &maxRole, &spaces, &version); err != nil {
		t.Fatal(err)
	}
	return prefs, maxRole, spaces, version
}

// untouched checks that the other two links kept their preferences and version.
func (e *prefsEnv) untouched(t *testing.T, stage string) {
	t.Helper()
	for _, id := range []string{"tgl_mini2", "tgl_other"} {
		if prefs, _, _, v := e.row(t, id); prefs != `{"digest":false}` || v != 1 {
			t.Fatalf("%s: %s changed: %s v%d", stage, id, prefs, v)
		}
	}
}

func (e *prefsEnv) count(t *testing.T, q string, args ...any) int {
	t.Helper()
	var n int
	if err := e.h.c.Store.ReadDB().QueryRowContext(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// SM-TG-009 (amendment TG-10-4): the Mini App reads and saves its own link's preferences; the save lands on that link
// only, sets the user's time zone without touching the web preferences, and is audited.
func TestMiniAppLinkPrefsReadAndSave(t *testing.T) {
	t.Parallel()
	e := newPrefsEnv(t)
	web := e.web
	expect(t, web.do("PUT", "/auth/me/notification-preferences", map[string]any{"enabled": true, "desktop": true, "new_actions": true,
		"assigned_to_me": true, "escalations": true, "reminders": false, "clarifications": true, "min_priority": "high", "muted_spaces": []string{}}, "If-Match", `"0"`), 200)

	r := e.get(t, prefsPath)
	expect(t, r, 200)
	var p tglink.Prefs
	r.json(t, &p)
	if p.LinkID != e.linkID || p.Version != 1 || r.header.Get("ETag") != `"1"` || !p.Effective.NewActions || p.Timezone != "" || p.Topics.Available || p.Topics.Mode != "auto" || p.Topics.Override != nil || p.Topics.Active || len(p.Topics.Spaces) != 0 || p.Topics.Refused {
		t.Fatalf("read %+v (ETag %s)", p, r.header.Get("ETag"))
	}
	r = e.put(t, prefsPath, `"1"`, map[string]any{"new_actions": false, "quiet_hours_start": "22:00", "muted_spaces": []string{e.space, e.space},
		"timezone": "Europe/Vilnius"})
	expect(t, r, 200)
	r.json(t, &p)
	if p.Version != 2 || p.Effective.NewActions || p.Effective.QuietHoursStart != "22:00" || len(p.Effective.MutedSpaces) != 1 || p.Timezone != "Europe/Vilnius" {
		t.Fatalf("saved %+v", p)
	}
	if prefs, _, _, _ := e.row(t, e.linkID); prefs != `{"new_actions":false,"quiet_hours_start":"22:00","muted_spaces":["`+e.space+`"]}` {
		t.Fatalf("stored %s", prefs)
	}
	e.untouched(t, "after a save")
	r = web.do("GET", "/auth/me/notification-preferences", nil)
	expect(t, r, 200)
	var np api.NotificationPrefsOut
	r.json(t, &np)
	if np.Timezone != "Europe/Vilnius" || !np.Desktop || np.MinPriority != "high" {
		t.Fatalf("web preferences %+v", np)
	}
	if n := e.count(t, `SELECT count(*) FROM audit_events WHERE action = 'telegram.link.prefs_saved' AND target_id = ?`, e.linkID); n != 1 {
		t.Fatalf("audit rows %d", n)
	}

	// The version guards the write; bad values are refused and change nothing.
	if p := e.put(t, prefsPath, `"1"`, map[string]any{}).problem(t); p.Code != "precondition_failed" {
		t.Fatalf("stale version: %+v", p)
	}
	if r := e.mini(t, "PUT", prefsPath, `{}`, "Authorization", "Telegram "+e.tok); r.status < 400 {
		t.Fatalf("without If-Match: %d", r.status)
	}
	for _, body := range []map[string]any{{"timezone": "Mars/Olympus"}, {"quiet_hours_end": "24:00"}, {"reminders": "loud"}, {"muted_spaces": []string{"*"}}} {
		expect(t, e.put(t, prefsPath, `"2"`, body), 422)
	}
	if _, _, _, v := e.row(t, e.linkID); v != 2 {
		t.Fatalf("a refused write changed the link: v%d", v)
	}
}

// The chat layout is saved through the threads field with If-Match, reads back as mode and override, and reaches the
// notifier at once (the save publishes telegram.link, which the notifier's link list reloads on).
func TestMiniAppLinkPrefsChatLayout(t *testing.T) {
	t.Parallel()
	e := newPrefsEnv(t)
	// Saving publishes telegram.link at once, which is what the notifier's link list reloads on.
	sub := e.h.c.Bus.Subscribe(8, eventbus.TopicTelegramLink)
	defer sub.Close()
	published := func(what string) {
		t.Helper()
		select {
		case ev := <-sub.C:
			if ev.Key != e.linkID {
				t.Fatalf("%s: event for %s", what, ev.Key)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: no telegram.link event", what)
		}
	}
	var p tglink.Prefs
	r := e.put(t, prefsPath, `"1"`, map[string]any{"threads": "on"})
	expect(t, r, 200)
	r.json(t, &p)
	if p.Version != 2 || p.Topics.Mode != "on" || p.Topics.Override == nil || *p.Topics.Override != "on" || p.Topics.Available || !p.Topics.Active {
		t.Fatalf("on: %+v", p.Topics)
	}
	published("on")
	// A threads save needs the version: none is 428, a stale one (v1, the link is at v2) 409 (precondition_failed), and neither changes the link.
	expect(t, e.put(t, prefsPath, "", map[string]any{"threads": "off"}), 428)
	expect(t, e.put(t, prefsPath, `"1"`, map[string]any{"threads": "off"}), 409)
	if prefs, _, _, v := e.row(t, e.linkID); prefs != `{"threads":"on"}` || v != 2 {
		t.Fatalf("a refused threads save changed the link: %s v%d", prefs, v)
	}
	r = e.put(t, prefsPath, `"2"`, map[string]any{"threads": "off"})
	expect(t, r, 200)
	r.json(t, &p)
	if p.Topics.Mode != "off" || p.Topics.Active {
		t.Fatalf("off: %+v", p.Topics)
	}
	published("off")
	expect(t, e.put(t, prefsPath, `"3"`, map[string]any{"threads": "sideways"}), 422)
	if prefs, _, _, v := e.row(t, e.linkID); prefs != `{"threads":"off"}` || v != 3 {
		t.Fatalf("stored %s v%d", prefs, v)
	}
	e.untouched(t, "after the chat layout")
}

// The link is the session's: no request names another link. A link ID in the body is refused, one in the query is
// ignored (the caller's own link is read and written), and the second link's session sees its own link.
func TestMiniAppLinkPrefsOnlyTheSessionsLink(t *testing.T) {
	t.Parallel()
	e := newPrefsEnv(t)
	for _, id := range []string{"tgl_mini2", "tgl_other"} {
		r := e.get(t, prefsPath+"?link_id="+id)
		expect(t, r, 200)
		var p tglink.Prefs
		r.json(t, &p)
		if p.LinkID != e.linkID {
			t.Fatalf("?link_id=%s read %s", id, p.LinkID)
		}
		expect(t, e.put(t, prefsPath, `"1"`, map[string]any{"link_id": id, "digest": true}), 422)
		expect(t, e.get(t, api.Prefix+"/telegram/link/prefs/"+id), 404)
	}
	e.untouched(t, "after named links")
	r := e.put(t, prefsPath+"?link_id=tgl_other", `"1"`, map[string]any{"digest": true})
	expect(t, r, 200)
	e.untouched(t, "after ?link_id")
	if prefs, _, _, _ := e.row(t, e.linkID); prefs != `{"digest":true}` {
		t.Fatalf("own link %s", prefs)
	}

	// The same user's second link: its own session reads its own preferences.
	r = e.mini(t, "POST", api.Prefix+"/telegram/session", exchangeBody(e.initData(e.tg.Token(), 43, 0)))
	expect(t, r, 201)
	var s api.TelegramSessionOut
	r.json(t, &s)
	r = e.mini(t, "GET", prefsPath, "", "Authorization", "Telegram "+s.Token)
	expect(t, r, 200)
	var p tglink.Prefs
	r.json(t, &p)
	if p.LinkID != "tgl_mini2" || p.Effective.Digest {
		t.Fatalf("second link read %+v", p)
	}
}

// No escalation: the body takes no cap, spaces, grants or user, and saving changes neither the link's cap, spaces
// and grants nor any Telegram settings; the global and space Telegram settings stay unreachable from the Mini App.
func TestMiniAppLinkPrefsCannotEscalate(t *testing.T) {
	t.Parallel()
	e := newPrefsEnv(t)
	grants := func() int {
		return e.count(t, `SELECT count(*) FROM role_grants WHERE subject = ?`, auth.TelegramSubject(e.linkID))
	}
	g0 := grants()
	_, role0, spaces0, _ := e.row(t, e.linkID)
	for _, body := range []map[string]any{
		{"max_role": "admin"}, {"spaces": []string{e.space}}, {"grants": []map[string]string{{"role": "admin", "domain": "*"}}},
		{"user_id": e.otherID}, {"state": "active"}, {"notification_defaults": map[string]any{"digest": false}},
		{"link_allow_admin_cap": true}, {"enabled": true}, {"topics": map[string]bool{"available": true}}, {"version": 9},
	} {
		if r := e.put(t, prefsPath, `"1"`, body); r.status != 422 {
			t.Fatalf("%v: %d %s", body, r.status, r.body)
		}
	}
	expect(t, e.put(t, prefsPath, `"1"`, map[string]any{"digest": false}), 200)
	if _, role, spaces, _ := e.row(t, e.linkID); role != role0 || spaces != spaces0 || grants() != g0 {
		t.Fatalf("cap %s→%s, spaces %s→%s, grants %d→%d", role0, role, spaces0, spaces, g0, grants())
	}
	if n := e.count(t, `SELECT count(*) FROM telegram_settings`) + e.count(t, `SELECT count(*) FROM space_telegram_settings`); n != 0 {
		t.Fatalf("Telegram settings written: %d rows", n)
	}
	for _, c := range []struct{ method, path string }{
		{"GET", api.Prefix + "/telegram/settings"}, {"PUT", api.Prefix + "/telegram/settings"},
		{"GET", api.Prefix + "/spaces/" + e.space + "/telegram"}, {"PUT", api.Prefix + "/spaces/" + e.space + "/telegram"},
		{"GET", api.Prefix + "/telegram/links"}, {"DELETE", api.Prefix + "/telegram/links/tgl_other"},
		{"GET", api.Prefix + "/me/telegram/links"}, {"PUT", api.Prefix + "/auth/me/notification-preferences"},
		{"PUT", api.Prefix + "/users/" + e.userID + "/grants"},
	} {
		if r := e.mini(t, c.method, c.path, `{}`, "Authorization", "Telegram "+e.tok, "If-Match", `"1"`); r.status != 403 && r.status != 404 {
			t.Fatalf("%s %s from the Mini App: %d", c.method, c.path, r.status)
		}
	}
	e.untouched(t, "after escalation attempts")
}

// Only a Telegram principal on the Mini App host has link preferences: a web session, an API token and the root token
// on the main host get 404, the bot's chat channel is refused by the classification, and a revoked link's session is
// no longer authenticated.
func TestMiniAppLinkPrefsRefusesOtherPrincipals(t *testing.T) {
	t.Parallel()
	e := newPrefsEnv(t)
	web := e.web
	tok := e.h.client()
	tok.bearer = userToken(t, e.h.c.Auth, "tokio@example.com", []auth.GrantInput{{Role: authz.RoleAdmin, Domain: authz.Global}})
	for name, c := range map[string]*client{"web session": web, "API token": tok, "root token": e.root} {
		expect(t, c.do("GET", "/telegram/link/prefs", nil), 404)
		if r := c.do("PUT", "/telegram/link/prefs", map[string]any{"digest": true}, "If-Match", `"1"`); r.status != 404 && r.status != 403 {
			t.Fatalf("%s PUT: %d", name, r.status)
		}
	}

	// The bot (in process, chat channel): the operations are Mini App only.
	ctx := context.Background()
	p, err := e.h.c.Auth.TelegramPrincipal(ctx, e.linkID, auth.TelegramOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{"GET", "PUT"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(m, prefsPath, strings.NewReader(`{"digest":true}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("If-Match", `"1"`)
		e.h.c.APIDeps().InProcessHandler().ServeHTTP(rec, req.WithContext(api.WithInProcessPrincipal(ctx, p)))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("chat %s: %d %s", m, rec.Code, rec.Body)
		}
	}
	e.untouched(t, "after other principals")
	if prefs, _, _, v := e.row(t, e.linkID); prefs != "{}" || v != 1 {
		t.Fatalf("own link changed: %s v%d", prefs, v)
	}

	// A revoked link: its session is gone, and nothing is written.
	e.exec(t, `UPDATE telegram_links SET state = 'revoked', revoked_at = 1 WHERE id = ?`, e.linkID)
	expect(t, e.get(t, prefsPath), 401)
	expect(t, e.put(t, prefsPath, `"1"`, map[string]any{"digest": true}), 401)
	if prefs, _, _, _ := e.row(t, e.linkID); prefs != "{}" {
		t.Fatalf("revoked link written: %s", prefs)
	}
}

// Amendment TG-10-4: queue-next is served to Telegram read-only (the same tasks list-tasks shows); recording the
// decision, a write behind a GET, is refused and records nothing.
func TestMiniAppQueueNextIsReadOnly(t *testing.T) {
	t.Parallel()
	e := newPrefsEnv(t)
	expect(t, e.root.do("POST", "/spaces/"+e.space+"/tasks", map[string]any{"title": "next one", "status": "ready"}), 201)
	r := e.get(t, api.Prefix+"/spaces/"+e.space+"/queue/next")
	expect(t, r, 200)
	var sel struct {
		Selected *struct {
			Title string `json:"title"`
		} `json:"selected"`
	}
	r.json(t, &sel)
	if sel.Selected == nil || sel.Selected.Title != "next one" {
		t.Fatalf("selection %s", r.body)
	}
	expect(t, e.get(t, api.Prefix+"/spaces/"+e.space+"/queue/next?record=true"), 403)
	if n := e.count(t, `SELECT count(*) FROM queue_decisions`); n != 0 {
		t.Fatalf("Telegram recorded %d decisions", n)
	}
	// The decisions stay off Telegram, and the root token still records.
	expect(t, e.get(t, api.Prefix+"/spaces/"+e.space+"/queue/decisions"), 403)
	expect(t, e.root.do("GET", "/spaces/"+e.space+"/queue/next?record=true", nil), 200)
}
