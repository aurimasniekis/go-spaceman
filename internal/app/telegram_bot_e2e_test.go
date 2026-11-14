package app_test

import (
	"context"
	"net/url"
	"strings"
	"testing"

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
