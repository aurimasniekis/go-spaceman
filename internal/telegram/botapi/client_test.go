package botapi_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/tgtest"
)

const userChat = int64(424242)

func newPair(t *testing.T, o tgtest.Options) (*tgtest.Server, *botapi.Client) {
	t.Helper()
	fake := tgtest.New(t, o)
	c, err := botapi.New(botapi.Options{BaseURL: fake.URL(), Token: fake.Token()})
	if err != nil {
		t.Fatal(err)
	}
	return fake, c
}

func noLimits() tgtest.Options { return tgtest.Options{Limits: tgtest.Limits{Disabled: true}} }

func apiErr(t *testing.T, err error, code int, contains string) {
	t.Helper()
	var e *botapi.Error
	if !errors.As(err, &e) {
		t.Fatalf("err = %v (%T), want *botapi.Error %d", err, err, code)
	}
	if e.Code != code || !strings.Contains(e.Description, contains) {
		t.Fatalf("err = %v, want %d containing %q", err, code, contains)
	}
}

func TestNewValidates(t *testing.T) {
	const tok = "123456:ABC-DEF1234ghIkl-zyx57W2v1u123ew11"
	for _, base := range []string{"", "https://api.telegram.org", "https://tg.example.com/proxy/", "http://127.0.0.1:9", "http://[::1]:9", "http://localhost:9"} {
		if _, err := botapi.New(botapi.Options{BaseURL: base, Token: tok}); err != nil {
			t.Errorf("base %q: %v", base, err)
		}
	}
	for _, base := range []string{"http://api.example.com", "https://u:p@api.example.com", "https://api.example.com?x", "https://api.example.com#f", "api.telegram.org", "ftp://x.example.com"} {
		if _, err := botapi.New(botapi.Options{BaseURL: base, Token: tok}); err == nil {
			t.Errorf("base %q accepted", base)
		}
	}
	for _, bad := range []string{"", "123456", ":abc", "abc:def", "123:abc/def", "123:abc?x=1", "123:abc def", "123:abc#x"} {
		_, err := botapi.New(botapi.Options{Token: bad})
		if err == nil {
			t.Errorf("token %q accepted", bad)
			continue
		}
		if bad != "" && strings.Contains(err.Error(), bad) {
			t.Errorf("error echoes the token: %v", err)
		}
	}
}

func TestMessagesAndKeyboards(t *testing.T) {
	fake, c := newPair(t, noLimits())
	ctx := context.Background()

	me, err := c.GetMe(ctx)
	if err != nil || !me.IsBot || me.Username != fake.Bot().Username || me.ID == 0 || c.BotID() == "" {
		t.Fatalf("getMe = %+v, %v", me, err)
	}

	kb := &botapi.InlineKeyboardMarkup{InlineKeyboard: [][]botapi.InlineKeyboardButton{{
		{Text: "Approve", CallbackData: "a:1", Style: botapi.StyleSuccess},
		{Text: "Open", WebApp: &botapi.WebAppInfo{URL: "https://tg.example.com/"}},
	}}}
	text := "Hello 👋 world"
	m, err := c.SendMessage(ctx, botapi.SendMessageParams{
		ChatID: userChat, Text: text, ProtectContent: true, ReplyMarkup: kb,
		// "world" starts at UTF-16 offset 9: the emoji is two units.
		Entities: []botapi.MessageEntity{{Type: "bold", Offset: 9, Length: 5}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if m.MessageID == 0 || m.Chat.ID != userChat || m.Chat.Type != botapi.ChatPrivate || m.Text != text || m.ReplyMarkup == nil {
		t.Fatalf("sent %+v", m)
	}
	call := fake.Calls("sendMessage")[0]
	if call.Params["protect_content"] != "true" || !strings.Contains(call.Params["reply_markup"], `"callback_data":"a:1"`) {
		t.Fatalf("recorded %+v", call.Params)
	}

	r, err := c.SendMessage(ctx, botapi.SendMessageParams{ChatID: userChat, Text: "reply", ReplyParameters: &botapi.ReplyParameters{MessageID: m.MessageID},
		ReplyMarkup: &botapi.ForceReply{ForceReply: true, InputFieldPlaceholder: "Your answer"}})
	if err != nil || r.ReplyToMessage == nil || r.ReplyToMessage.MessageID != m.MessageID {
		t.Fatalf("reply = %+v, %v", r, err)
	}

	e, err := c.EditMessageText(ctx, botapi.EditMessageTextParams{ChatID: userChat, MessageID: m.MessageID, Text: "Answered"})
	if err != nil || e.Text != "Answered" || e.EditDate == 0 || e.ReplyMarkup != nil {
		t.Fatalf("edit = %+v, %v", e, err)
	}
	_, err = c.EditMessageText(ctx, botapi.EditMessageTextParams{ChatID: userChat, MessageID: m.MessageID, Text: "Answered"})
	apiErr(t, err, 400, "message is not modified")
	if _, err := c.EditMessageReplyMarkup(ctx, botapi.EditMessageReplyMarkupParams{ChatID: userChat, MessageID: m.MessageID, ReplyMarkup: kb}); err != nil {
		t.Fatal(err)
	}
	_, err = c.EditMessageText(ctx, botapi.EditMessageTextParams{ChatID: userChat, MessageID: 999, Text: "x"})
	apiErr(t, err, 400, "message to edit not found")

	if err := c.DeleteMessage(ctx, userChat, m.MessageID); err != nil {
		t.Fatal(err)
	}
	apiErr(t, c.DeleteMessage(ctx, userChat, m.MessageID), 400, "message to delete not found")
	if got := fake.Messages(userChat); len(got) != 1 || got[0].MessageID != r.MessageID {
		t.Fatalf("messages left %+v", got)
	}

	if err := c.SetMyCommands(ctx, botapi.SetMyCommandsParams{Commands: []botapi.BotCommand{{Command: "needs", Description: "Needs you"}},
		Scope: &botapi.BotCommandScope{Type: "all_private_chats"}}); err != nil {
		t.Fatal(err)
	}
	if got := fake.Commands(); len(got) != 1 || got[0].Command != "needs" {
		t.Fatalf("commands %+v", got)
	}
	if err := c.SetChatMenuButton(ctx, botapi.SetChatMenuButtonParams{MenuButton: &botapi.MenuButton{Type: botapi.MenuButtonWebApp, Text: "Open",
		WebApp: &botapi.WebAppInfo{URL: "https://tg.example.com/"}}}); err != nil {
		t.Fatal(err)
	}
	if mb := fake.MenuButton(); mb == nil || mb.WebApp.URL != "https://tg.example.com/" {
		t.Fatalf("menu %+v", mb)
	}

	fake.Block(userChat)
	_, err = c.SendMessage(ctx, botapi.SendMessageParams{ChatID: userChat, Text: "hi"})
	if !botapi.IsForbidden(err) {
		t.Fatalf("blocked send: %v", err)
	}
}

func TestDocumentedLimits(t *testing.T) {
	fake, c := newPair(t, noLimits())
	ctx := context.Background()
	group := int64(-100123)

	send := func(text string) error {
		_, err := c.SendMessage(ctx, botapi.SendMessageParams{ChatID: userChat, Text: text})
		return err
	}
	if err := send(strings.Repeat("a", 4096)); err != nil {
		t.Errorf("4096 characters: %v", err)
	}
	apiErr(t, send(strings.Repeat("a", 4097)), 400, "message is too long")
	if err := send(strings.Repeat("😀", 2048)); err != nil {
		t.Errorf("2048 emoji (4096 UTF-16 units): %v", err)
	}
	apiErr(t, send(strings.Repeat("😀", 2049)), 400, "message is too long")
	apiErr(t, send(""), 400, "message text is empty")

	_, err := c.SendMessage(ctx, botapi.SendMessageParams{ChatID: userChat, Text: "abc", Entities: []botapi.MessageEntity{{Type: "bold", Offset: 2, Length: 2}}})
	apiErr(t, err, 400, "can't parse entities")
	_, err = c.SendMessage(ctx, botapi.SendMessageParams{ChatID: userChat, Text: "abcdef", Entities: []botapi.MessageEntity{
		{Type: "bold", Offset: 0, Length: 4}, {Type: "italic", Offset: 2, Length: 4}}})
	apiErr(t, err, 400, "overlap")
	_, err = c.SendMessage(ctx, botapi.SendMessageParams{ChatID: userChat, Text: "abcdef", Entities: []botapi.MessageEntity{
		{Type: "code", Offset: 0, Length: 6}, {Type: "bold", Offset: 1, Length: 2}}})
	apiErr(t, err, 400, "can't contain")

	button := func(chat int64, b botapi.InlineKeyboardButton) error {
		_, err := c.SendMessage(ctx, botapi.SendMessageParams{ChatID: chat, Text: "x",
			ReplyMarkup: &botapi.InlineKeyboardMarkup{InlineKeyboard: [][]botapi.InlineKeyboardButton{{b}}}})
		return err
	}
	if err := button(userChat, botapi.InlineKeyboardButton{Text: "ok", CallbackData: strings.Repeat("x", 64)}); err != nil {
		t.Errorf("64-byte callback_data: %v", err)
	}
	apiErr(t, button(userChat, botapi.InlineKeyboardButton{Text: "ok", CallbackData: strings.Repeat("é", 33)}), 400, "BUTTON_DATA_INVALID")
	apiErr(t, button(group, botapi.InlineKeyboardButton{Text: "app", WebApp: &botapi.WebAppInfo{URL: "https://tg.example.com/"}}), 400, "BUTTON_TYPE_INVALID")
	if err := button(group, botapi.InlineKeyboardButton{Text: "link", URL: "https://t.me/bot/app?startapp=x"}); err != nil {
		t.Errorf("url button in a group: %v", err)
	}
	apiErr(t, button(userChat, botapi.InlineKeyboardButton{Text: "none"}), 400, "exactly one action")

	_, err = c.SendDocument(ctx, botapi.SendDocumentParams{ChatID: userChat, Caption: strings.Repeat("c", 1025),
		Document: &botapi.InputFile{Name: "a.txt", Reader: strings.NewReader("hi")}})
	apiErr(t, err, 400, "caption is too long")

	doc := func(n int) []botapi.InputMedia {
		var out []botapi.InputMedia
		for range n {
			out = append(out, botapi.InputMedia{Type: botapi.MediaDocument, Media: &botapi.InputFile{Name: "d.txt", Reader: strings.NewReader("d")}})
		}
		return out
	}
	for _, n := range []int{1, 11} {
		_, err = c.SendMediaGroup(ctx, botapi.SendMediaGroupParams{ChatID: userChat, Media: doc(n)})
		apiErr(t, err, 400, "media group must have 2-10 items")
	}
	mixed := append(doc(1), botapi.InputMedia{Type: botapi.MediaPhoto, Media: &botapi.InputFile{Name: "p.png", Reader: strings.NewReader("p")}})
	_, err = c.SendMediaGroup(ctx, botapi.SendMediaGroupParams{ChatID: userChat, Media: mixed})
	apiErr(t, err, 400, "only with documents")

	big := fake.AddFile("big.bin", nil, botapi.MaxDownloadBytes+1)
	_, err = c.GetFile(ctx, big)
	apiErr(t, err, 400, "file is too big")
	_, err = c.GetFile(ctx, "nope")
	apiErr(t, err, 400, "invalid file_id")
}

func TestFiles(t *testing.T) {
	fake, c := newPair(t, noLimits())
	ctx := context.Background()

	id := fake.AddFile("report.pdf", []byte("%PDF-1.7 fake"), 0)
	f, err := c.GetFile(ctx, id)
	if err != nil || f.FilePath == "" || f.FileSize != 13 {
		t.Fatalf("getFile = %+v, %v", f, err)
	}
	var buf bytes.Buffer
	if n, err := c.DownloadFile(ctx, f.FilePath, &buf); err != nil || n != 13 || buf.String() != "%PDF-1.7 fake" {
		t.Fatalf("download = %d %q %v", n, buf.String(), err)
	}
	if _, err := c.DownloadFile(ctx, "documents/missing.pdf", &buf); err == nil {
		t.Fatal("missing file downloaded")
	}
	for _, p := range []string{"../etc/passwd", "a/../b", "/abs", "a?b", "a#b", "a%2fb", ""} {
		if _, err := c.DownloadFile(ctx, p, &buf); err == nil || !strings.Contains(err.Error(), "invalid file path") {
			t.Errorf("path %q: %v", p, err)
		}
	}

	m, err := c.SendDocument(ctx, botapi.SendDocumentParams{ChatID: userChat, Caption: "the report", ProtectContent: true,
		CaptionEntities: []botapi.MessageEntity{{Type: "italic", Offset: 4, Length: 6}},
		Document:        &botapi.InputFile{Name: "r.txt", Reader: strings.NewReader("contents")}})
	if err != nil || m.Document == nil || m.Caption != "the report" {
		t.Fatalf("sendDocument = %+v, %v", m, err)
	}
	if got, _ := fake.FileContent(m.Document.FileID); string(got) != "contents" {
		t.Fatalf("uploaded %q", got)
	}
	call := fake.Calls("sendDocument")[0]
	if call.Params["document"] != "attach://file0" || call.Params["protect_content"] != "true" || string(call.Files["file0"]) != "contents" {
		t.Fatalf("multipart call %+v", call)
	}

	p, err := c.SendPhoto(ctx, botapi.SendPhotoParams{ChatID: userChat, Photo: &botapi.InputFile{FileID: id}})
	if err != nil || len(p.Photo) == 0 || p.Photo[0].FileID != id {
		t.Fatalf("sendPhoto by file_id = %+v, %v", p, err)
	}

	ms, err := c.SendMediaGroup(ctx, botapi.SendMediaGroupParams{ChatID: userChat, Media: []botapi.InputMedia{
		{Type: botapi.MediaDocument, Media: &botapi.InputFile{Name: "a.txt", Reader: strings.NewReader("A")}, Caption: "first"},
		{Type: botapi.MediaDocument, Media: &botapi.InputFile{FileID: m.Document.FileID}},
		{Type: botapi.MediaDocument, Media: &botapi.InputFile{Name: "c.txt", Reader: strings.NewReader("C")}},
	}})
	if err != nil || len(ms) != 3 || ms[0].MediaGroupID == "" || ms[0].MediaGroupID != ms[2].MediaGroupID || ms[0].Caption != "first" {
		t.Fatalf("sendMediaGroup = %+v, %v", ms, err)
	}
	if got, _ := fake.FileContent(ms[2].Document.FileID); string(got) != "C" {
		t.Fatalf("third item content %q", got)
	}
}

func TestUpdatesAndWebhook(t *testing.T) {
	fake, c := newPair(t, noLimits())
	ctx := context.Background()

	id1 := fake.PushUpdate(botapi.Update{Message: &botapi.Message{MessageID: 1, Chat: botapi.Chat{ID: userChat, Type: botapi.ChatPrivate}, Text: "/start"}})
	fake.PushUpdate(botapi.Update{CallbackQuery: &botapi.CallbackQuery{ID: "cb1", From: botapi.User{ID: userChat}, Data: "a:1"}})
	us, err := c.GetUpdates(ctx, botapi.GetUpdatesParams{Timeout: 1})
	if err != nil || len(us) != 2 || us[0].UpdateID != id1 || us[0].Message.Text != "/start" || us[1].CallbackQuery.Data != "a:1" {
		t.Fatalf("getUpdates = %+v, %v", us, err)
	}
	if err := c.AnswerCallbackQuery(ctx, botapi.AnswerCallbackQueryParams{CallbackQueryID: "cb1", Text: "Done"}); err != nil {
		t.Fatal(err)
	}
	apiErr(t, c.AnswerCallbackQuery(ctx, botapi.AnswerCallbackQueryParams{CallbackQueryID: "cb1"}), 400, "query ID is invalid")
	if got := fake.CallbackAnswers(); len(got) != 1 || got[0].Text != "Done" {
		t.Fatalf("answers %+v", got)
	}

	// Confirming with offset = last+1 empties the queue; a long poll then wakes on the next update.
	done := make(chan []botapi.Update, 1)
	go func() {
		u, err := c.GetUpdates(ctx, botapi.GetUpdatesParams{Offset: us[1].UpdateID + 1, Timeout: 10})
		if err != nil {
			t.Error(err)
		}
		done <- u
	}()
	time.Sleep(50 * time.Millisecond)
	fake.PushUpdate(botapi.Update{Message: &botapi.Message{MessageID: 2, Chat: botapi.Chat{ID: userChat, Type: botapi.ChatPrivate}, Text: "late"}})
	select {
	case u := <-done:
		if len(u) != 1 || u[0].Message.Text != "late" {
			t.Fatalf("long poll = %+v", u)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("long poll did not wake")
	}

	err = c.SetWebhook(ctx, botapi.SetWebhookParams{URL: "http://hook.example.com/x", SecretToken: "s3cr3t-value"})
	apiErr(t, err, 400, "HTTPS URL")
	err = c.SetWebhook(ctx, botapi.SetWebhookParams{URL: "https://hook.example.com:8080/x"})
	apiErr(t, err, 400, "ports")
	err = c.SetWebhook(ctx, botapi.SetWebhookParams{URL: "https://hook.example.com/x", SecretToken: "bad secret!"})
	apiErr(t, err, 400, "secret token")
	if err := c.SetWebhook(ctx, botapi.SetWebhookParams{URL: "https://hook.example.com/telegram/webhook/abc", SecretToken: "s3cr3t_value-1",
		AllowedUpdates: []string{"message", "callback_query"}}); err != nil {
		t.Fatal(err)
	}
	info, err := c.GetWebhookInfo(ctx)
	if err != nil || info.URL != "https://hook.example.com/telegram/webhook/abc" || info.MaxConnections != 40 || len(info.AllowedUpdates) != 2 || info.PendingUpdateCount != 1 {
		t.Fatalf("webhook info = %+v, %v", info, err)
	}
	_, err = c.GetUpdates(ctx, botapi.GetUpdatesParams{})
	apiErr(t, err, 409, "webhook is active")
	if err := c.DeleteWebhook(ctx, true); err != nil {
		t.Fatal(err)
	}
	if us, err := c.GetUpdates(ctx, botapi.GetUpdatesParams{}); err != nil || len(us) != 0 {
		t.Fatalf("after deleteWebhook(drop) = %+v, %v", us, err)
	}
}

func TestRateLimits(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC))
	fake, c := newPair(t, tgtest.Options{Clock: clk})
	ctx := context.Background()
	send := func(chat int64) error {
		_, err := c.SendMessage(ctx, botapi.SendMessageParams{ChatID: chat, Text: "x"})
		return err
	}

	// Private chat: a burst of 3, then 1/s.
	for i := range 3 {
		if err := send(userChat); err != nil {
			t.Fatalf("burst send %d: %v", i, err)
		}
	}
	err := send(userChat)
	if ra, ok := botapi.RetryAfter(err); !ok || ra != time.Second {
		t.Fatalf("4th send: %v (retry_after %v)", err, ra)
	}
	apiErr(t, err, 429, "Too Many Requests: retry after 1")
	clk.Advance(time.Second)
	if err := send(userChat); err != nil {
		t.Fatalf("after retry_after: %v", err)
	}
	// Edits and deletes count as sends.
	err = c.DeleteMessage(ctx, userChat, 1)
	if _, ok := botapi.RetryAfter(err); !ok {
		t.Fatalf("delete right after a send: %v", err)
	}

	// Group: 20 per minute; the 21st waits 3 s for the next token.
	clk.Advance(time.Minute)
	group := int64(-100777)
	for i := range 20 {
		if err := send(group); err != nil {
			t.Fatalf("group send %d: %v", i, err)
		}
	}
	if ra, ok := botapi.RetryAfter(send(group)); !ok || ra != 3*time.Second {
		t.Fatalf("21st group send: retry_after %v %v", ra, ok)
	}

	// Global: 30 at once across chats, then 429 whatever the chat.
	clk.Advance(time.Minute)
	for i := range 30 {
		if err := send(int64(1000 + i)); err != nil {
			t.Fatalf("global send %d: %v", i, err)
		}
	}
	if _, ok := botapi.RetryAfter(send(5000)); !ok {
		t.Fatal("31st send in one second was accepted")
	}
	clk.Advance(100 * time.Millisecond)
	if err := send(5000); err != nil {
		t.Fatalf("after the global bucket refilled: %v", err)
	}

	// A sender at 1/s per chat and below 30/s overall never sees a 429.
	clk.Advance(time.Minute)
	before := len(fake.Calls())
	for tick := range 20 {
		for chat := range 25 {
			if err := send(int64(20000 + chat)); err != nil {
				t.Fatalf("paced send tick %d chat %d: %v", tick, chat, err)
			}
		}
		clk.Advance(time.Second)
	}
	for _, call := range fake.Calls()[before:] {
		if call.Status != http.StatusOK {
			t.Fatalf("paced sender got %d", call.Status)
		}
	}
}

func TestFailNextAndHelpers(t *testing.T) {
	fake, c := newPair(t, noLimits())
	ctx := context.Background()
	fake.FailNext("sendMessage", 429, "Too Many Requests: retry after 7", 7)
	_, err := c.SendMessage(ctx, botapi.SendMessageParams{ChatID: userChat, Text: "x"})
	if ra, ok := botapi.RetryAfter(err); !ok || ra != 7*time.Second {
		t.Fatalf("retry_after: %v", err)
	}
	fake.FailNext("getMe", 401, "Unauthorized", 0)
	if _, err := c.GetMe(ctx); !botapi.IsUnauthorized(err) {
		t.Fatalf("401: %v", err)
	}
	if err := c.Call(ctx, "noSuchMethod", nil, nil); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("unknown method: %v", err)
	}
	if err := c.Call(ctx, "../getMe", nil, nil); err == nil || !strings.Contains(err.Error(), "invalid method") {
		t.Fatalf("method name with a path: %v", err)
	}
	wrong, _ := botapi.New(botapi.Options{BaseURL: fake.URL(), Token: "1:wrong"})
	if _, err := wrong.GetMe(ctx); !botapi.IsUnauthorized(err) {
		t.Fatalf("wrong token: %v", err)
	}
}

func TestRedirectsNotFollowed(t *testing.T) {
	hit := make(chan string, 1)
	elsewhere := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { hit <- r.URL.Path }))
	defer elsewhere.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()
	c, err := botapi.New(botapi.Options{BaseURL: redirector.URL, Token: "123:abc"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetMe(context.Background()); err == nil {
		t.Fatal("redirect answer accepted")
	}
	select {
	case p := <-hit:
		t.Fatalf("redirect followed to %s", p)
	default:
	}
}
