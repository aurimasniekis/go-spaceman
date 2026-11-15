package app_test

import (
	"context"
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
	tgsettings "github.com/aurimasniekis/go-spaceman/internal/telegram/settings"
)

// SM-TG-006 (TG-7-2): an enforced approval in a space without the opt-in, which is decided only in the browser, gets a
// Mini App button built from the configured Mini App host (a link to the action's page).
func TestTelegramNeedsYouAppButton(t *testing.T) {
	t.Parallel()
	e := newMiniAppEnv(t)
	_ = enrolledSession(t, e.h, "mini@example.com") // operators must enroll MFA; until then the link is Limited too
	e.startBot(t)
	ctx := context.Background()
	e.h.c.TelegramLinks.Start(t.Context()) // the link was inserted after central started
	on := true
	if _, err := e.h.c.TelegramSettings.PutSpace(ctx, audit.System, e.space, 0, tgsettings.SpaceInput{Enabled: &on}); err != nil {
		t.Fatal(err)
	}
	if err := e.h.c.TelegramSettings.Load(ctx); err != nil {
		t.Fatal(err)
	}
	r := e.root.do("POST", "/spaces/"+e.space+"/human-actions", map[string]any{"type": "approval", "title": "Deploy?",
		"request": map[string]any{"action": "deploy"}})
	expect(t, r, 201)
	var act struct {
		ID string `json:"id"`
	}
	r.json(t, &act)
	// Enforced (ADR-0024): an approvals row stands behind the action.
	e.exec(t, `INSERT INTO approvals (id, space_id, policy_id, operation, resource, args_digest, args_preview, requester_run, requester_role,
		human_action_id, state, uses_allowed, uses_left, valid_seconds, request_expires_at, created_at, updated_at)
		VALUES (?, ?, 'pol', 'mcp.x.y', '', 'd', '', 'run_x', 'control', ?, 'pending', 1, 1, 60, 9000000000000000, 0, 0)`, "apr_"+act.ID, e.space, act.ID)

	d := e.h.c.TelegramUpdates.Dispatcher
	from := botapi.User{ID: 42}
	n := int64(1000)
	push := func(m *botapi.Message, q *botapi.CallbackQuery) {
		n++
		u := botapi.Update{UpdateID: n, Message: m, CallbackQuery: q}
		if q != nil {
			e.tg.PushUpdate(u)
		}
		d.Dispatch(ctx, u)
	}
	last := func() botapi.Message {
		ms := e.tg.Messages(42)
		return ms[len(ms)-1]
	}
	open := func() botapi.Message {
		push(&botapi.Message{MessageID: int(n + 1), From: &from, Chat: botapi.Chat{ID: 42, Type: botapi.ChatPrivate}, Text: "/needsyou sso-space"}, nil)
		list := last()
		if list.ReplyMarkup == nil || len(list.ReplyMarkup.InlineKeyboard) != 1 {
			t.Fatalf("/needsyou: %q %+v", list.Text, list.ReplyMarkup)
		}
		push(nil, &botapi.CallbackQuery{ID: "q" + strings.Repeat("x", int(n+1)), From: from, Message: &list, Data: list.ReplyMarkup.InlineKeyboard[0][0].CallbackData})
		return last()
	}
	f := open()
	if !strings.Contains(f.Text, "decided in the browser") || f.ReplyMarkup == nil || f.ReplyMarkup.InlineKeyboard[0][0].WebApp == nil {
		t.Fatalf("approval card: %q %+v", f.Text, f.ReplyMarkup)
	}
	want := e.origin() + "/spaces/" + e.space + "/human?action=" + act.ID
	if got := f.ReplyMarkup.InlineKeyboard[0][0].WebApp.URL; got != want {
		t.Fatalf("Mini App URL %q, want %q", got, want)
	}

	// Without a Mini App host: the text stays, no button.
	expect(t, e.root.do("PUT", "/telegram/bot", map[string]any{"enabled": true, "mode": "polling", "miniapp_host": ""}, "If-Match", `"1"`), 200)
	if e.h.c.TelegramBot.MiniAppHost() != "" {
		t.Fatal("Mini App host still set")
	}
	f = open()
	hasApp := false // the common row (claim, comment, ask) stays; only the Mini App button goes
	if f.ReplyMarkup != nil {
		for _, row := range f.ReplyMarkup.InlineKeyboard {
			for _, b := range row {
				hasApp = hasApp || b.WebApp != nil
			}
		}
	}
	if !strings.Contains(f.Text, "decided in the browser") || hasApp {
		t.Fatalf("no host: %q %+v", f.Text, f.ReplyMarkup)
	}
}
