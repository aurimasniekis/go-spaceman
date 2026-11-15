package app_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
	tgsettings "github.com/aurimasniekis/go-spaceman/internal/telegram/settings"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/tgtest"
)

// SM-TG-007 (TG-8a-2, plan §2.2): capture through the real dispatcher, link cache, in-process API and attachment
// service. A text with a forward and a photo become one inbox item whose source is the principal (not the forward
// origin), the forward's origin is in the body, the photo is an attachment sniffed by the API, and reading the item
// sends the photo back with protect_content. A link that may only read can't create items or upload.
func TestTelegramChatCapture(t *testing.T) {
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
	on := true
	if _, err := s.h.c.TelegramSettings.PutSpace(ctx, audit.System, s.space, 0, tgsettings.SpaceInput{Enabled: &on}); err != nil {
		t.Fatal(err)
	}
	if err := s.h.c.TelegramSettings.Load(ctx); err != nil {
		t.Fatal(err)
	}

	n := int64(0)
	from := botapi.User{ID: 42}
	push := func(edit func(*botapi.Message)) {
		n++
		m := &botapi.Message{MessageID: int(n), From: &from, Chat: botapi.Chat{ID: 42, Type: botapi.ChatPrivate}}
		edit(m)
		d.Dispatch(ctx, botapi.Update{UpdateID: n, Message: m})
	}
	last := func() botapi.Message {
		ms := tg.Messages(42)
		return ms[len(ms)-1]
	}
	png := []byte("\x89PNG\r\n\x1a\n-pixels-")
	fid := tg.AddFile("p.jpg", png, int64(len(png)))
	origin, _ := json.Marshal(map[string]any{"type": "hidden_user", "sender_user_name": "Ghost"})
	push(func(m *botapi.Message) {
		m.Caption = "from a friend"
		m.ForwardOrigin = origin
		m.Photo = []botapi.PhotoSize{{FileID: fid, Width: 10, Height: 10, FileSize: int64(len(png))}}
	})
	if !strings.HasPrefix(last().Text, "Added to the inbox of") {
		t.Fatalf("ack %q", last().Text)
	}
	r := s.root.do("GET", "/spaces/"+s.space+"/inbox?status=new", nil)
	expect(t, r, 200)
	var page struct {
		Items []struct {
			ID          string `json:"id"`
			Body        string `json:"body"`
			SourceLabel string `json:"source_label"`
		} `json:"items"`
	}
	r.json(t, &page)
	if len(page.Items) != 1 {
		t.Fatalf("items %+v", page.Items)
	}
	it := page.Items[0]
	if it.Body != "> Forwarded from Ghost\n\nfrom a friend" || strings.Contains(it.SourceLabel, "Ghost") {
		t.Fatalf("body %q label %q", it.Body, it.SourceLabel)
	}
	r = s.root.do("GET", "/spaces/"+s.space+"/inbox/"+it.ID, nil)
	expect(t, r, 200)
	var full struct {
		Attachments []struct {
			ID   string `json:"id"`
			MIME string `json:"mime"`
			Size int64  `json:"size"`
		} `json:"attachments"`
	}
	r.json(t, &full)
	if len(full.Attachments) != 1 || full.Attachments[0].MIME != "image/png" || full.Attachments[0].Size != int64(len(png)) {
		t.Fatalf("attachments %+v", full.Attachments)
	}

	// Reading the item sends the file back, with protect_content, and only the file stored (sniffed as png: a photo).
	ack := last()
	var open string
	for _, row := range ack.ReplyMarkup.InlineKeyboard {
		for _, b := range row {
			if strings.HasPrefix(b.CallbackData, "ir:") {
				open = b.CallbackData
			}
		}
	}
	n++
	u := botapi.Update{UpdateID: n, CallbackQuery: &botapi.CallbackQuery{ID: "q1", From: from, Message: &ack, Data: open}}
	tg.PushUpdate(u)
	d.Dispatch(ctx, u)
	if c := tg.Calls("sendPhoto", "sendDocument"); len(c) != 1 {
		t.Fatalf("files sent: %d", len(c))
	}

	// A read-only link: the API refuses the upload and the create, and nothing new is stored.
	s.setLinkRole(t, "viewer")
	fid2 := tg.AddFile("n.txt", []byte("hello"), 5)
	push(func(m *botapi.Message) { m.Document = &botapi.Document{FileID: fid2, FileName: "n.txt"} })
	if got := last().Text; got == "" || strings.HasPrefix(got, "Added") {
		t.Fatalf("a viewer's capture answered %q", got)
	}
	push(func(m *botapi.Message) { m.Text = "viewer text" })
	if got := last().Text; strings.HasPrefix(got, "Added") {
		t.Fatalf("a viewer's capture answered %q", got)
	}
	r = s.root.do("GET", "/spaces/"+s.space+"/inbox?status=new", nil)
	r.json(t, &page)
	if len(page.Items) != 1 {
		t.Fatalf("a viewer link created an item: %+v", page.Items)
	}
}
