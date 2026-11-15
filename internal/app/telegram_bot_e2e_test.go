package app_test

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/api"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/config"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/tgbot"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/tgtest"
)

// SM-TG-014, plan §5.2 TG-3b: the bot ops over HTTP against the fake Bot API. Only system:read reads the bot and only
// system:write changes it (a space's own admin does neither); the token is never in an answer; writes are If-Match
// checked; miniapp_host may not be the public URL's host; the running bot's health is on the System page; deleting
// turns Telegram off.
func TestTelegramBotOpsOverHTTP(t *testing.T) {
	t.Parallel()
	tg := tgtest.New(t, tgtest.Options{Limits: tgtest.Limits{Disabled: true}})
	h := newHarnessWith(t, func(c *config.Config) { c.Telegram.APIBaseURL = tg.URL() })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.c.TelegramUpdates.Run(ctx)
	h.c.TelegramBot.Start(ctx)

	rootSecret, _, err := h.c.Auth.IssueRootToken(ctx)
	if err != nil {
		t.Fatal(err)
	}
	root := h.client()
	root.bearer = rootSecret
	actor := audit.Actor{Type: audit.ActorUser, ID: "usr_test"}
	en, _ := h.c.Fleet.CreateNode(ctx, actor, fleet.CreateNodeInput{Name: "node-a"})
	sp, err := h.c.Fleet.CreateSpace(ctx, actor, fleet.CreateSpaceInput{Slug: "alpha", Name: "Alpha", NodeID: en.Node.ID})
	if err != nil {
		t.Fatal(err)
	}
	spaceAdmin := h.client()
	spaceAdmin.bearer = userToken(t, h.c.Auth, "sam@example.com", []auth.GrantInput{{Role: authz.RoleAdmin, Domain: sp.ID}})
	global := h.client()
	global.bearer = userToken(t, h.c.Auth, "gina@example.com", []auth.GrantInput{{Role: authz.RoleAdmin, Domain: "*"}})

	in := map[string]any{"bot_token": tg.Token(), "enabled": true, "mode": "polling"}
	expect(t, spaceAdmin.do("GET", "/telegram/bot", nil), 403)
	expect(t, spaceAdmin.do("PUT", "/telegram/bot", in), 403)
	expect(t, root.do("GET", "/telegram/bot", nil), 404)

	r := global.do("PUT", "/telegram/bot", in)
	expect(t, r, 200)
	var b tgbot.Bot
	r.json(t, &b)
	if r.header.Get("ETag") != `"1"` || !b.HasToken || b.BotID != tg.Bot().ID || b.WebhookStatus != tgbot.StatePolling {
		t.Fatalf("put: %s %s", r.header.Get("ETag"), r.body)
	}
	secretPart := tg.Token()[strings.IndexByte(tg.Token(), ':')+1:]
	get := root.do("GET", "/telegram/bot", nil)
	expect(t, get, 200)
	var sys api.SystemStatus
	sr := root.do("GET", "/system", nil)
	expect(t, sr, 200)
	sr.json(t, &sys)
	if !sys.Telegram.Configured || sys.Telegram.Username != "spaceman_test_bot" || sys.Telegram.WebhookStatus != tgbot.StatePolling {
		t.Fatalf("system page telegram %+v", sys.Telegram)
	}
	for _, body := range [][]byte{r.body, get.body, sr.body} {
		if strings.Contains(string(body), secretPart) {
			t.Fatalf("the token is in an answer: %s", body)
		}
	}

	pub, _ := url.Parse(h.srv.URL)
	bad := map[string]any{"enabled": true, "mode": "polling", "miniapp_host": pub.Hostname()}
	expect(t, global.do("PUT", "/telegram/bot", bad, "If-Match", `"1"`), 422)           // the public host here is an IP address
	expect(t, global.do("PUT", "/telegram/bot", map[string]any{"enabled": false}), 409) // no If-Match on an existing bot
	expect(t, global.do("PUT", "/telegram/bot", map[string]any{"enabled": false}, "If-Match", `"7"`), 409)
	expect(t, spaceAdmin.do("DELETE", "/telegram/bot", nil, "If-Match", `"1"`), 403)
	expect(t, global.do("DELETE", "/telegram/bot", nil, "If-Match", `"1"`), 204)
	expect(t, root.do("GET", "/telegram/bot", nil), 404)
	sr = root.do("GET", "/system", nil)
	sr.json(t, &sys)
	if sys.Telegram.Configured || sys.Telegram.WebhookStatus != tgbot.StateOff {
		t.Fatalf("system page after delete %+v", sys.Telegram)
	}
}

// ADR-0083, plan §5.2: replacing or deleting the Telegram bot is a step-up operation (StepUp(SystemWrite)): a global
// administrator's browser session that signed in more than the reauthentication window ago is refused until it
// re-authenticates with its password and second factor; reading needs no step-up.
func TestTelegramBotNeedsStepUp(t *testing.T) {
	t.Parallel()
	for _, op := range []string{"put-telegram-bot", "delete-telegram-bot"} {
		if r := api.TelegramBotRules[op]; !r.Reauth || !r.MFA || r.Perm != authz.SystemWrite {
			t.Fatalf("%s rule %+v, want StepUp(system:write)", op, r)
		}
	}
	if r := api.TelegramBotRules["get-telegram-bot"]; r.Reauth || r.MFA || r.Perm != authz.SystemRead {
		t.Fatalf("get-telegram-bot rule %+v, want Global(system:read)", r)
	}
	tg := tgtest.New(t, tgtest.Options{Limits: tgtest.Limits{Disabled: true}})
	h := newHarnessWith(t, func(c *config.Config) { c.Telegram.APIBaseURL = tg.URL() })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.c.TelegramUpdates.Run(ctx)
	h.c.TelegramBot.Start(ctx)
	rootSecret, _, _ := h.c.Auth.IssueRootToken(ctx)
	root := h.client()
	root.bearer = rootSecret
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

	in := map[string]any{"bot_token": tg.Token(), "enabled": true, "mode": "polling"}
	expect(t, ui.do("PUT", "/telegram/bot", in), 200) // fresh sign-in with MFA: step-up satisfied

	// Later, the session is no longer recent: reading works, replacing and deleting need a step-up.
	h.clk.Advance(30 * time.Minute)
	expect(t, ui.do("GET", "/telegram/bot", nil), 200)
	for _, c := range []struct {
		method string
		body   any
	}{{"PUT", map[string]any{"enabled": false}}, {"DELETE", nil}} {
		if p := ui.do(c.method, "/telegram/bot", c.body, "If-Match", `"1"`).problem(t); p.Code != "reauth_required" && p.Code != "mfa_required" {
			t.Fatalf("%s without a step-up: %+v", c.method, p)
		}
	}
	r = ui.do("POST", "/auth/reauth", map[string]string{"password": pw, "code": auth.TOTPCode(secret, auth.TOTPStep(h.clk.Now()))})
	expect(t, r, 200)
	r.json(t, &me)
	ui.csrf = me.CSRFToken
	expect(t, ui.do("PUT", "/telegram/bot", map[string]any{"enabled": false}, "If-Match", `"1"`), 200)
	expect(t, ui.do("DELETE", "/telegram/bot", nil, "If-Match", `"2"`), 204)
}
