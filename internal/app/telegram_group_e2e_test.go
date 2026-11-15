package app_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/chat"
	tgsettings "github.com/aurimasniekis/go-spaceman/internal/telegram/settings"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/tgtest"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/updates"
)

const e2eGroup = -100500

// SM-TG-002 (TG-13-1, group mode): through the real dispatcher, link cache, settings cache, authorization and
// in-process API, every tap in a bound group is authorized as the tapping person's OWN link. The binder's link is an
// operator; a member whose link is capped at viewer can open the action but can't answer it, a member without a grant
// on the space is refused, an unlinked member is refused, and only the operator's own tap answers it. The space's
// content level is full, yet nothing above summary reaches the group.
func TestTelegramGroupPerTapperAuthorization(t *testing.T) {
	t.Parallel()
	s := newTelegramSetup(t) // the binder: tg user 42, link tgl_e2e, operator
	ctx := context.Background()
	tg := tgtest.New(t, tgtest.Options{Limits: tgtest.Limits{Disabled: true}})
	bot, err := botapi.New(botapi.Options{BaseURL: tg.URL(), Token: tg.Token()})
	if err != nil {
		t.Fatal(err)
	}
	d := s.h.c.TelegramUpdates.Dispatcher
	d.SetBot(bot)

	// Members: 43 (an operator user whose link is capped at viewer), 44 (a user without any grant on the space).
	for _, m := range []struct {
		tg        int64
		email     string
		userGrant bool
		linkRole  string
	}{{43, "viewer@example.com", true, "viewer"}, {44, "nogrant@example.com", false, ""}} {
		body := map[string]any{"email": m.email, "display_name": m.email, "password": "correct horse battery staple", "grants": []map[string]string{}}
		if m.userGrant {
			body["grants"] = []map[string]string{{"role": "operator", "domain": s.space}}
		}
		r := s.root.do("POST", "/users", body)
		expect(t, r, 201)
		var u struct {
			ID string `json:"id"`
		}
		r.json(t, &u)
		_ = enrolledSession(t, s.h, m.email)
		link := fmt.Sprintf("tgl_m%d", m.tg)
		s.exec(t, `INSERT INTO telegram_links (id, user_id, bot_id, tg_user_id, chat_id, state, created_at, updated_at) VALUES (?, ?, 1, ?, ?, 'active', 0, 0)`,
			link, u.ID, m.tg, m.tg)
		if m.linkRole != "" {
			s.exec(t, `INSERT INTO role_grants (subject, role, domain, source, created_at, created_by) VALUES (?, ?, ?, 'telegram', 0, 't')`,
				auth.TelegramSubject(link), m.linkRole, s.space)
		}
	}
	if err := s.h.c.Authz.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	s.h.c.TelegramLinks.Start(t.Context())

	// The group binding (TG-13-2 will set it through put-space-telegram): content level full on purpose.
	on, full := true, tgsettings.ContentFull
	if _, err := s.h.c.TelegramSettings.PutSpace(ctx, audit.System, s.space, 0, tgsettings.SpaceInput{Enabled: &on, ContentLevel: &full}); err != nil {
		t.Fatal(err)
	}
	s.exec(t, `UPDATE space_telegram_settings SET group_chat_id = ? WHERE space_id = ?`, e2eGroup, s.space)
	if err := s.h.c.TelegramSettings.Load(ctx); err != nil {
		t.Fatal(err)
	}
	r := s.root.do("POST", "/spaces/"+s.space+"/human-actions", map[string]any{"type": "question", "title": "Which colour?",
		"body": "PRIVATE-BODY-TEXT", "request": map[string]any{"options": []string{"red", "blue"}}})
	expect(t, r, 201)
	var act struct {
		ID string `json:"id"`
	}
	r.json(t, &act)
	status := func() string {
		r := s.root.do("GET", "/spaces/"+s.space+"/human-actions/"+act.ID, nil)
		var a struct {
			Status string `json:"status"`
		}
		r.json(t, &a)
		return a.Status
	}

	n := int64(0)
	group := botapi.Chat{ID: e2eGroup, Type: botapi.ChatSupergroup, Title: "Team"}
	tap := func(from int64, data string, on botapi.Message) string {
		n++
		u := botapi.Update{UpdateID: n, CallbackQuery: &botapi.CallbackQuery{ID: fmt.Sprintf("q%d", n), From: botapi.User{ID: from}, Message: &on, Data: data}}
		tg.PushUpdate(u)
		d.Dispatch(ctx, u)
		a := tg.CallbackAnswers()
		return a[len(a)-1].Text
	}
	say := func(from int64, text string) {
		n++
		d.Dispatch(ctx, botapi.Update{UpdateID: n, Message: &botapi.Message{MessageID: int(n), From: &botapi.User{ID: from}, Chat: group, Text: text}})
	}
	last := func() botapi.Message {
		ms := tg.Messages(e2eGroup)
		if len(ms) == 0 {
			t.Fatal("nothing in the group")
		}
		return ms[len(ms)-1]
	}
	find := func(m botapi.Message, label string) string {
		if m.ReplyMarkup != nil {
			for _, row := range m.ReplyMarkup.InlineKeyboard {
				for _, b := range row {
					if strings.Contains(b.Text, label) {
						return b.CallbackData
					}
				}
			}
		}
		return ""
	}
	open := "ho:" + strings.TrimPrefix(s.space, "spc_") + ":" + strings.TrimPrefix(act.ID, "hum_")
	card := botapi.Message{MessageID: 5000, Chat: group}

	// An unlinked member, and a linked member without a grant on the space, are refused; nothing is sent.
	if got := tap(45, open, card); got != updates.GroupUnlinkedText {
		t.Fatalf("unlinked tap answered %q", got)
	}
	if got := tap(44, open, card); got != chat.TextGroupNoAccess {
		t.Fatalf("no-grant tap answered %q", got)
	}
	say(44, "/needsyou")
	if n := len(tg.Messages(e2eGroup)); n != 0 {
		t.Fatalf("refused members made the bot post %d messages", n)
	}

	// The viewer-capped member can read the action in the group, but answering is an operator's action.
	tap(43, open, card)
	vf := last()
	if find(vf, "blue") == "" {
		t.Fatalf("viewer's form: %q", vf.Text)
	}
	if got := tap(43, find(vf, "blue"), vf); got == "" || got == "Done" || status() != "open" {
		t.Fatalf("a viewer link answered (%q), status %s", got, status())
	}

	// The binder's own tap on the viewer's form is not the viewer's: forms are bound to the link that opened them.
	if got := tap(42, find(vf, "blue"), vf); status() != "open" {
		t.Fatalf("another member completed the viewer's form (%q)", got)
	}

	// The operator opens and answers it in the group with their own link.
	tap(42, open, card)
	of := last()
	tap(42, find(of, "blue"), of)
	if status() == "open" {
		t.Fatal("the operator's own tap didn't answer")
	}

	// Nothing above summary reached the group, although the space is full.
	say(42, "/inbox")
	for _, m := range tg.Messages(e2eGroup) {
		if strings.Contains(m.Text, "PRIVATE-BODY-TEXT") {
			t.Fatalf("a body reached the group: %q", m.Text)
		}
	}
}
