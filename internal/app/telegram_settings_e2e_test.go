package app_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/api"
	"github.com/aurimasniekis/go-spaceman/internal/app"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/settings"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/updates"
)

// SM-TG-014 (TG-3c): every Telegram setting of plan §5.2 round-trips through its operation with If-Match; out-of-range
// values and unknown fields are refused; reads and writes need system:read/system:write (global) or
// space_config:read/space_config:write in the space; every write is audited; and a change reaches the inbound limits
// at once, without a restart.
func TestTelegramSettingsOverHTTP(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	rootSecret, _, _ := h.c.Auth.IssueRootToken(ctx)
	root := h.client()
	root.bearer = rootSecret
	actor := audit.Actor{Type: audit.ActorUser, ID: "usr_test"}
	en, _ := h.c.Fleet.CreateNode(ctx, actor, fleet.CreateNodeInput{Name: "node-a"})
	a, err := h.c.Fleet.CreateSpace(ctx, actor, fleet.CreateSpaceInput{Slug: "alpha", Name: "Alpha", NodeID: en.Node.ID})
	if err != nil {
		t.Fatal(err)
	}
	b, err := h.c.Fleet.CreateSpace(ctx, actor, fleet.CreateSpaceInput{Slug: "bravo", Name: "Bravo", NodeID: en.Node.ID})
	if err != nil {
		t.Fatal(err)
	}
	user := func(email string, grants ...auth.GrantInput) *client {
		c := h.client()
		c.bearer = userToken(t, h.c.Auth, email, grants)
		return c
	}
	global := user("gina@example.com", auth.GrantInput{Role: authz.RoleAdmin, Domain: "*"})
	spaceAdmin := user("sam@example.com", auth.GrantInput{Role: authz.RoleAdmin, Domain: a.ID})
	viewer := user("vic@example.com", auth.GrantInput{Role: authz.RoleViewer, Domain: a.ID})
	other := user("olga@example.com", auth.GrantInput{Role: authz.RoleOperator, Domain: b.ID})

	// Global settings: system:read / system:write only.
	for _, c := range []*client{spaceAdmin, viewer, other} {
		expect(t, c.do("GET", "/telegram/settings", nil), 403)
		expect(t, c.do("PUT", "/telegram/settings", map[string]any{}), 403)
		expect(t, c.do("DELETE", "/telegram/settings", nil, "If-Match", `"1"`), 403)
	}
	r := global.do("GET", "/telegram/settings", nil)
	expect(t, r, 200)
	var out settings.SettingsOut
	r.json(t, &out)
	if out.Stored || out.Version != 0 || out.Settings != settings.Defaults() || r.header.Get("ETag") != `"0"` {
		t.Fatalf("defaults %+v %q", out, r.header.Get("ETag"))
	}

	// Every setting at once, all different from the defaults.
	full := map[string]any{
		"link_default_max_role": "viewer", "link_allow_admin_cap": true, "link_sso_grants": true, "link_reconfirm_days": 30,
		"links_per_user": 3, "link_code_ttl_seconds": 300, "init_data_max_age_seconds": 120, "miniapp_session_idle_seconds": 600,
		"miniapp_session_absolute_seconds": 7200, "topics_mode_default": true,
		"send_limits": map[string]any{"global_per_second": 10, "chat_per_second": 0.5, "chat_burst": 2, "group_per_minute": 10,
			"queue_total": 1000, "queue_per_chat": 20},
		"inbound_limits": map[string]any{"actions_per_minute": 12, "actions_burst": 4, "inbox_items_per_hour": 6, "downloads_per_hour": 5,
			"updates_per_second": 7},
		"notification_defaults": map[string]any{"new_actions": false, "min_priority": "high", "assigned_to_me": false, "escalations": false,
			"reminders": "digest", "clarifications": false, "operator_messages": false, "inbox_items": "push", "tasks_in_review": "off",
			"failed_runs": "push", "digest": false, "digest_time": "07:15", "quiet_hours": false, "quiet_hours_start": "22:00",
			"quiet_hours_end": "06:30"},
	}
	r = global.do("PUT", "/telegram/settings", full)
	expect(t, r, 200)
	r.json(t, &out)
	got, _ := json.Marshal(out.Settings)
	var gotMap map[string]any
	_ = json.Unmarshal(got, &gotMap)
	wantJSON, _ := json.Marshal(full)
	var wantMap map[string]any
	_ = json.Unmarshal(wantJSON, &wantMap)
	if g, w := canon(t, gotMap), canon(t, wantMap); g != w || !out.Stored || out.Version != 1 || r.header.Get("ETag") != `"1"` {
		t.Fatalf("round trip:\n got %s\nwant %s (%+v)", g, w, out)
	}
	// The change reached the inbound limits at once (a restart isn't needed).
	if l := h.c.TelegramLimiter.Limits(); l != (updates.InboundLimits{ActionsPerMinute: 12, ActionsBurst: 4, InboxItemsPerHour: 6, DownloadsPerHour: 5}) {
		t.Fatalf("limiter %+v", l)
	}
	if h.c.TelegramSettings.Current() != out.Settings {
		t.Fatal("the cache didn't follow")
	}

	expect(t, root.do("PUT", "/telegram/settings", full), 409)                    // stored: replace with If-Match
	expect(t, root.do("PUT", "/telegram/settings", full, "If-Match", `"7"`), 409) // stale
	// Out of range, cross-field and unknown fields: 422, nothing changes.
	for _, bad := range []map[string]any{
		{"links_per_user": 4}, {"link_reconfirm_days": 6}, {"link_code_ttl_seconds": 1801}, {"init_data_max_age_seconds": 59},
		{"miniapp_session_idle_seconds": 3601}, {"miniapp_session_absolute_seconds": 899}, {"link_default_max_role": "admin"},
		{"miniapp_session_idle_seconds": 1800, "miniapp_session_absolute_seconds": 900},
		{"send_limits": map[string]any{"global_per_second": 31}}, {"send_limits": map[string]any{"chat_per_second": 0}},
		{"send_limits": map[string]any{"chat_burst": 4}}, {"send_limits": map[string]any{"group_per_minute": 21}},
		{"send_limits": map[string]any{"queue_total": 10, "queue_per_chat": 11}},
		{"inbound_limits": map[string]any{"updates_per_second": 101}}, {"inbound_limits": map[string]any{"actions_per_minute": 0}},
		{"notification_defaults": map[string]any{"digest_time": "25:00"}}, {"notification_defaults": map[string]any{"reminders": "email"}},
		{"notification_defaults": map[string]any{"quiet_hours_start": "08:00"}}, // equals the default end
		{"api_base_url": "https://evil.example"}, {"bot_token": "123:abc"},
	} {
		if r := global.do("PUT", "/telegram/settings", bad, "If-Match", `"1"`); r.status != 422 {
			t.Errorf("%v: %d %s", bad, r.status, r.body)
		}
	}
	// A partial replacement: what isn't sent goes back to its default.
	r = global.do("PUT", "/telegram/settings", map[string]any{"inbound_limits": map[string]any{"actions_burst": 20}}, "If-Match", `"1"`)
	expect(t, r, 200)
	r.json(t, &out)
	want := settings.Defaults()
	want.InboundLimits.ActionsBurst = 20
	if out.Settings != want || out.Version != 2 {
		t.Fatalf("partial %+v", out)
	}
	if l := h.c.TelegramLimiter.Limits(); l.ActionsBurst != 20 || l.ActionsPerMinute != 30 {
		t.Fatalf("limiter after the partial replacement %+v", l)
	}
	expect(t, global.do("DELETE", "/telegram/settings", nil), 428)
	expect(t, global.do("DELETE", "/telegram/settings", nil, "If-Match", `"1"`), 409)
	expect(t, global.do("DELETE", "/telegram/settings", nil, "If-Match", `"2"`), 204)
	r = root.do("GET", "/telegram/settings", nil)
	r.json(t, &out)
	if out.Stored || out.Settings != settings.Defaults() || h.c.TelegramLimiter.Limits() != updates.DefaultInboundLimits() {
		t.Fatalf("after reset %+v", out)
	}

	// A space's settings: space configuration, its viewers read, its admins write; another space can't see it.
	base := "/spaces/" + a.ID + "/telegram"
	r = viewer.do("GET", base, nil)
	expect(t, r, 200)
	var sp settings.Space
	r.json(t, &sp)
	if sp.Stored || sp.Enabled || sp.Version != 0 || sp.ContentLevel != "full" || sp.EnforcedApprovals != "web_only" {
		t.Fatalf("space defaults %+v", sp)
	}
	expect(t, other.do("GET", base, nil), 404)
	expect(t, viewer.do("PUT", base, map[string]any{"enabled": true}), 403)
	expect(t, other.do("PUT", base, map[string]any{"enabled": true}), 404)
	spaceAll := map[string]any{"enabled": true, "content_level": "summary", "enforced_approvals": "confirm", "topics": true, "terminal_watch": true}
	r = spaceAdmin.do("PUT", base, spaceAll)
	expect(t, r, 200)
	r.json(t, &sp)
	if !sp.Stored || !sp.Enabled || sp.ContentLevel != "summary" || sp.EnforcedApprovals != "confirm" || !sp.Topics || !sp.TerminalWatch ||
		sp.Version != 1 || r.header.Get("ETag") != `"1"` {
		t.Fatalf("space set %+v", sp)
	}
	if !h.c.TelegramSettings.Space(a.ID).TerminalWatch {
		t.Fatal("the space cache didn't follow")
	}
	expect(t, spaceAdmin.do("PUT", base, spaceAll), 409)
	for _, bad := range []map[string]any{{"content_level": "all"}, {"enforced_approvals": "chat"}, {"group_chat_id": -100}} {
		if r := spaceAdmin.do("PUT", base, bad, "If-Match", `"1"`); r.status != 422 {
			t.Errorf("%v: %d %s", bad, r.status, r.body)
		}
	}
	r = spaceAdmin.do("PUT", base, map[string]any{"enabled": true}, "If-Match", `"1"`)
	expect(t, r, 200)
	r.json(t, &sp)
	if sp.ContentLevel != "full" || sp.TerminalWatch || sp.Version != 2 {
		t.Fatalf("space partial %+v", sp)
	}
	expect(t, root.do("PUT", "/spaces/"+b.ID+"/telegram", map[string]any{}), 200)
	// The list shows only spaces whose configuration the caller can read.
	var list struct {
		Items []settings.Space `json:"items"`
	}
	r = viewer.do("GET", "/telegram/spaces", nil)
	expect(t, r, 200)
	r.json(t, &list)
	if len(list.Items) != 1 || list.Items[0].SpaceID != a.ID {
		t.Fatalf("viewer's list %+v", list.Items)
	}
	r = root.do("GET", "/telegram/spaces", nil)
	r.json(t, &list)
	if len(list.Items) != 2 {
		t.Fatalf("root's list %+v", list.Items)
	}
	expect(t, viewer.do("DELETE", base, nil, "If-Match", `"2"`), 403)
	expect(t, spaceAdmin.do("DELETE", base, nil, "If-Match", `"1"`), 409)
	expect(t, spaceAdmin.do("DELETE", base, nil, "If-Match", `"2"`), 204)
	expect(t, spaceAdmin.do("DELETE", base, nil, "If-Match", `"2"`), 404)
	if h.c.TelegramSettings.Space(a.ID).Visible() {
		t.Fatal("a deleted space stayed visible")
	}

	// Every write is audited.
	var events struct {
		Items []struct {
			Action string `json:"action"`
		} `json:"items"`
	}
	r = root.do("GET", "/audit?limit=200", nil)
	expect(t, r, 200)
	r.json(t, &events)
	seen := map[string]int{}
	for _, e := range events.Items {
		seen[e.Action]++
	}
	for action, n := range map[string]int{"telegram.settings.created": 1, "telegram.settings.updated": 1, "telegram.settings.deleted": 1,
		"telegram.space.created": 2, "telegram.space.updated": 1, "telegram.space.deleted": 1} {
		if seen[action] != n {
			t.Errorf("%s audited %d times, want %d", action, seen[action], n)
		}
	}
}

func canon(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Plan §5.2: every Telegram settings write is a step-up operation. A global administrator's browser session that
// signed in longer ago than the re-authentication window is refused, for the global and the per-space writes alike,
// until it re-authenticates with its password and second factor. Reads need no step-up.
func TestTelegramSettingsNeedStepUp(t *testing.T) {
	t.Parallel()
	for op, r := range api.TelegramSettingsRules {
		write := strings.HasPrefix(op, "put-") || strings.HasPrefix(op, "delete-")
		if write != (r.Reauth && r.MFA) {
			t.Errorf("%s rule %+v: writes, and only writes, need a step-up", op, r)
		}
	}
	h := newHarness(t)
	ctx := context.Background()
	rootSecret, _, _ := h.c.Auth.IssueRootToken(ctx)
	root := h.client()
	root.bearer = rootSecret
	en, _ := h.c.Fleet.CreateNode(ctx, audit.Actor{Type: audit.ActorUser, ID: "usr_test"}, fleet.CreateNodeInput{Name: "node-a"})
	sp, err := h.c.Fleet.CreateSpace(ctx, audit.Actor{Type: audit.ActorUser, ID: "usr_test"}, fleet.CreateSpaceInput{Slug: "alpha", Name: "Alpha", NodeID: en.Node.ID})
	if err != nil {
		t.Fatal(err)
	}
	const pw = "correct horse battery staple"
	expect(t, root.do("POST", "/users", map[string]any{"email": "gina@example.com", "display_name": "Gina", "password": pw,
		"grants": []map[string]string{{"role": "admin", "domain": "*"}}}), 201)
	ui := h.client()
	r := ui.do("POST", "/auth/login", map[string]string{"email": "gina@example.com", "password": pw})
	expect(t, r, 200)
	var me api.Me
	r.json(t, &me)
	ui.csrf = me.CSRFToken
	r = ui.do("POST", "/auth/mfa/enroll", nil)
	expect(t, r, 200)
	var enr api.TOTPEnrollment
	r.json(t, &enr)
	secret, err := auth.DecodeTOTPSecret(enr.Secret)
	if err != nil {
		t.Fatal(err)
	}
	expect(t, ui.do("POST", "/auth/mfa/enroll/confirm", map[string]string{"code": auth.TOTPCode(secret, auth.TOTPStep(h.clk.Now()))}), 200)
	h.clk.Advance(30 * time.Second)
	r = ui.do("POST", "/auth/mfa/verify", map[string]string{"code": auth.TOTPCode(secret, auth.TOTPStep(h.clk.Now()))})
	expect(t, r, 200)
	r.json(t, &me)
	ui.csrf = me.CSRFToken
	spacePath := "/spaces/" + sp.ID + "/telegram"
	expect(t, ui.do("PUT", "/telegram/settings", map[string]any{}), 200) // fresh sign-in with MFA
	expect(t, ui.do("PUT", spacePath, map[string]any{}), 200)

	h.clk.Advance(30 * time.Minute)
	expect(t, ui.do("GET", "/telegram/settings", nil), 200)
	expect(t, ui.do("GET", spacePath, nil), 200)
	expect(t, ui.do("GET", "/telegram/spaces", nil), 200)
	for _, c := range []struct{ method, path string }{{"PUT", "/telegram/settings"}, {"DELETE", "/telegram/settings"}, {"PUT", spacePath}, {"DELETE", spacePath}} {
		var body any
		if c.method == "PUT" {
			body = map[string]any{}
		}
		if p := ui.do(c.method, c.path, body, "If-Match", `"1"`).problem(t); p.Code != "reauth_required" && p.Code != "mfa_required" {
			t.Fatalf("%s %s without a step-up: %+v", c.method, c.path, p)
		}
	}
	r = ui.do("POST", "/auth/reauth", map[string]string{"password": pw, "code": auth.TOTPCode(secret, auth.TOTPStep(h.clk.Now()))})
	expect(t, r, 200)
	r.json(t, &me)
	ui.csrf = me.CSRFToken
	expect(t, ui.do("PUT", "/telegram/settings", map[string]any{"topics_mode_default": true}, "If-Match", `"1"`), 200)
	expect(t, ui.do("DELETE", spacePath, nil, "If-Match", `"1"`), 204)
}

// Plan §5.2 / ADR-0083: the Bot API base URL is a deployment setting in central's configuration file only. No API
// schema has a field for it (one would let a system:write holder send the write-only token to a server of their
// choice), and the Telegram settings' input schemas refuse unknown fields.
func TestTelegramSettingsHaveNoBaseURL(t *testing.T) {
	t.Parallel()
	doc, err := json.Marshal(api.Register(http.NewServeMux(), app.SpecDeps()).OpenAPI())
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Paths      map[string]map[string]json.RawMessage `json:"paths"`
		Components struct {
			Schemas map[string]struct {
				AdditionalProperties *bool                      `json:"additionalProperties"`
				Properties           map[string]json.RawMessage `json:"properties"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(doc, &spec); err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{"api_base_url", "base_url\":{\"description\":\"The Bot API"} {
		if strings.Contains(string(doc), needle) {
			t.Fatalf("the API schema mentions %q", needle)
		}
	}
	for _, p := range []string{"/telegram/settings", "/spaces/{space_id}/telegram", "/telegram/spaces"} {
		if spec.Paths[p] == nil {
			t.Fatalf("%s is not in the API", p)
		}
	}
	for name, s := range spec.Components.Schemas {
		if !strings.HasPrefix(name, "Settings") {
			continue
		}
		for prop := range s.Properties {
			if strings.Contains(prop, "url") || strings.Contains(prop, "token") {
				t.Errorf("%s.%s: the Telegram settings carry no URL or token", name, prop)
			}
		}
		if strings.HasSuffix(name, "Input") && (s.AdditionalProperties == nil || *s.AdditionalProperties) {
			t.Errorf("%s accepts unknown fields", name)
		}
	}
}

// The user's own time zone (operator answer Q9): an IANA name or empty, validated; older clients that don't send it
// still save their preferences; the digest and quiet hours have no fallback zone.
func TestNotificationPrefsTimezone(t *testing.T) {
	h := newHarness(t)
	u := h.client()
	u.bearer = userToken(t, h.c.Auth, "tara@example.com", []auth.GrantInput{{Role: authz.RoleViewer, Domain: "*"}})
	r := u.do("GET", "/auth/me/notification-preferences", nil)
	expect(t, r, 200)
	var p api.NotificationPrefsOut
	r.json(t, &p)
	if p.Timezone != "" {
		t.Fatalf("default zone %q, want unset", p.Timezone)
	}
	prefs := map[string]any{"enabled": true, "desktop": false, "new_actions": true, "assigned_to_me": true, "escalations": true, "reminders": false,
		"clarifications": true, "min_priority": "low", "muted_spaces": []string{}}
	expect(t, u.do("PUT", "/auth/me/notification-preferences", prefs, "If-Match", `"0"`), 200) // without timezone: an older client
	for _, bad := range []string{"Mars/Olympus", "Local", "../../etc/passwd", strings.Repeat("A", 65)} {
		prefs["timezone"] = bad
		if r := u.do("PUT", "/auth/me/notification-preferences", prefs, "If-Match", `"1"`); r.status != 422 {
			t.Errorf("%q: %d %s", bad, r.status, r.body)
		}
	}
	prefs["timezone"] = "Europe/Vilnius"
	r = u.do("PUT", "/auth/me/notification-preferences", prefs, "If-Match", `"1"`)
	expect(t, r, 200)
	r.json(t, &p)
	if p.Timezone != "Europe/Vilnius" || p.Version != 2 {
		t.Fatalf("saved %+v", p)
	}
	prefs["timezone"] = ""
	r = u.do("PUT", "/auth/me/notification-preferences", prefs, "If-Match", `"2"`)
	expect(t, r, 200)
	r.json(t, &p)
	if p.Timezone != "" {
		t.Fatalf("cleared %+v", p)
	}
}
