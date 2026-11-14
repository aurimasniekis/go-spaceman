package app_test

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
	tgsettings "github.com/aurimasniekis/go-spaceman/internal/telegram/settings"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/tgtest"
)

// SM-TG-007 (TG-8a-1, plan §2.1, §2.2): the chat commands run through the real dispatcher, link cache and in-process
// API as the link's Telegram principal. /spaces lists only the spaces the user can read AND Telegram may see; the inbox
// is read and its status changed with the web's rules; a space the user may read but Telegram is off for, and a space
// Telegram is on for but the user may not read, are neither listed nor reachable with a forged button.
func TestTelegramChatSpacesAndInbox(t *testing.T) {
	t.Parallel()
	s := newTelegramSetup(t)
	ctx := context.Background()
	tg := tgtest.New(t, tgtest.Options{Limits: tgtest.Limits{Disabled: true}})
	bot, err := botapi.New(botapi.Options{BaseURL: tg.URL(), Token: tg.Token()})
	if err != nil {
		t.Fatal(err)
	}
	d := s.h.c.TelegramUpdates.Dispatcher
	d.SetBot(bot)
	s.h.c.TelegramLinks.Start(t.Context())

	// Space off: readable by the user and the link, Telegram off. Space hidden: Telegram on, no grant for the user.
	off, hidden := ids.New(ids.Space), ids.New(ids.Space)
	for i, id := range []string{off, hidden} {
		s.exec(t, `INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES (?, ?, ?, 'nod_oidc', 'active', 'w', 0, 't', 0)`,
			id, []string{"off-space", "hidden-space"}[i], []string{"Off space", "Hidden space"}[i])
	}
	for _, sub := range []string{auth.UserSubject(s.userID), auth.TelegramSubject(s.linkID)} {
		s.exec(t, `INSERT INTO role_grants (subject, role, domain, source, created_at, created_by) VALUES (?, 'operator', ?, 'manual', 0, 't')`, sub, off)
	}
	if err := s.h.c.Authz.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	on := true
	for _, id := range []string{s.space, hidden} {
		if _, err := s.h.c.TelegramSettings.PutSpace(ctx, audit.System, id, 0, tgsettings.SpaceInput{Enabled: &on}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.h.c.TelegramSettings.Load(ctx); err != nil {
		t.Fatal(err)
	}
	r := s.root.do("POST", "/spaces/"+s.space+"/inbox", map[string]any{"title": "Call the plumber", "body": "the **tap** drips", "urgency": "high"})
	expect(t, r, 201)
	var item struct {
		ID      string `json:"id"`
		Version int64  `json:"version"`
	}
	r.json(t, &item)

	n := int64(0)
	push := func(m *botapi.Message, q *botapi.CallbackQuery) {
		n++
		u := botapi.Update{UpdateID: n, Message: m, CallbackQuery: q}
		if q != nil {
			tg.PushUpdate(u)
		}
		d.Dispatch(ctx, u)
	}
	from := botapi.User{ID: 42}
	say := func(text string) {
		push(&botapi.Message{MessageID: int(n + 1), From: &from, Chat: botapi.Chat{ID: 42, Type: botapi.ChatPrivate}, Text: text}, nil)
	}
	tap := func(data string, on botapi.Message) {
		push(nil, &botapi.CallbackQuery{ID: "q" + strings.Repeat("x", int(n+1)), From: from, Message: &on, Data: data})
	}
	last := func() botapi.Message {
		ms := tg.Messages(42)
		return ms[len(ms)-1]
	}
	dataOf := func(m botapi.Message) (out []string) {
		if m.ReplyMarkup != nil {
			for _, row := range m.ReplyMarkup.InlineKeyboard {
				for _, b := range row {
					out = append(out, b.CallbackData)
				}
			}
		}
		return
	}

	say("/spaces")
	list := last()
	if got := dataOf(list); len(got) != 1 || got[0] != "sp:"+s.space {
		t.Fatalf("listed %v, want only %s (off %s, hidden %s)", got, s.space, off, hidden)
	}
	for _, id := range []string{off, hidden} {
		tap("sp:"+id, list)
		a := tg.CallbackAnswers()
		if a[len(a)-1].Text != "That isn't available. Use /spaces to see your spaces." {
			t.Fatalf("%s answered %q", id, a[len(a)-1].Text)
		}
	}
	say("/space off-space")
	if !strings.HasPrefix(last().Text, "That isn't available") {
		t.Fatalf("off space: %q", last().Text)
	}

	say("/inbox")
	ibx := last()
	if !strings.Contains(ibx.Text, "Inbox of") || !strings.Contains(ibx.Text, "1 new") {
		t.Fatalf("inbox: %q", ibx.Text)
	}
	var read string
	for _, dd := range dataOf(ibx) {
		if strings.HasPrefix(dd, "ir:") {
			read = dd
		}
	}
	if read == "" {
		t.Fatalf("no item button in %v", dataOf(ibx))
	}
	tap(read, ibx)
	ms := tg.Messages(42)
	card, body := ms[len(ms)-2], ms[len(ms)-1]
	if !strings.Contains(card.Text, "Call the plumber") || !strings.Contains(body.Text, "the **tap** drips") || len(body.Entities) != 0 {
		t.Fatalf("card %q body %q", card.Text, body.Text)
	}
	var done string
	for _, b := range card.ReplyMarkup.InlineKeyboard {
		for _, bb := range b {
			if bb.Text == "Done" {
				done = bb.CallbackData
			}
		}
	}
	tap(done, card)
	r = s.root.do("GET", "/spaces/"+s.space+"/inbox/"+item.ID, nil)
	expect(t, r, 200)
	var got struct {
		Status string `json:"status"`
	}
	r.json(t, &got)
	if got.Status != "done" {
		t.Fatalf("status %q after Done", got.Status)
	}
	// A viewer-only link can't change the status: the web's rule (inbox:write) applies.
	s.setLinkRole(t, "viewer")
	edited := last()
	tap(statusDataFor(s.space, item.ID, "p", item.Version+1), edited)
	a := tg.CallbackAnswers()
	if a[len(a)-1].Text == "" || a[len(a)-1].Text == "Done" {
		t.Fatalf("a viewer's status change answered %q", a[len(a)-1].Text)
	}
	r = s.root.do("GET", "/spaces/"+s.space+"/inbox/"+item.ID, nil)
	r.json(t, &got)
	if got.Status != "done" {
		t.Fatalf("a viewer link changed the status to %q", got.Status)
	}
}

func statusDataFor(spaceID, itemID, action string, v int64) string {
	return "is:" + strings.TrimPrefix(spaceID, "spc_") + ":" + strings.TrimPrefix(itemID, "inb_") + ":" + action + ":" + strconv.FormatInt(v, 10)
}
