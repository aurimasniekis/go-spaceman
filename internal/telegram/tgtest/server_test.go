package tgtest

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
)

func post(t *testing.T, s *Server, path, contentType, body string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, s.URL()+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var env map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("%s: not JSON: %v", path, err)
	}
	return resp.StatusCode, env
}

func TestRoutingAndParameterEncodings(t *testing.T) {
	s := New(t, Options{Limits: Limits{Disabled: true}})
	bot := "/bot" + s.Token()

	if code, env := post(t, s, "/bot1:wrong/getMe", "", ""); code != 401 || env["ok"] != false || env["description"] != "Unauthorized" {
		t.Fatalf("wrong token: %d %v", code, env)
	}
	if code, _ := post(t, s, bot+"/frobnicate", "", ""); code != 404 {
		t.Fatalf("unknown method: %d", code)
	}
	if code, _ := post(t, s, "/elsewhere", "", ""); code != 404 {
		t.Fatalf("other path: %d", code)
	}
	if code, env := post(t, s, bot+"/getMe", "", ""); code != 200 || env["ok"] != true {
		t.Fatalf("getMe: %d %v", code, env)
	}
	// API#making-requests: query string, URL-encoded form, JSON and multipart are all accepted.
	if code, env := post(t, s, bot+"/sendMessage?chat_id=5&text=q", "", ""); code != 200 {
		t.Fatalf("query string: %d %v", code, env)
	}
	form := url.Values{"chat_id": {"5"}, "text": {"form"}}.Encode()
	if code, env := post(t, s, bot+"/sendMessage", "application/x-www-form-urlencoded", form); code != 200 {
		t.Fatalf("form: %d %v", code, env)
	}
	if code, env := post(t, s, bot+"/sendMessage", "application/json", `{"chat_id":5,"text":"json"}`); code != 200 {
		t.Fatalf("json: %d %v", code, env)
	}
	if code, _ := post(t, s, bot+"/sendMessage", "application/json", `{"chat_id":`); code != 400 {
		t.Fatalf("bad json: %d", code)
	}
	if code, _ := post(t, s, bot+"/sendMessage", "text/plain", "x"); code != 400 {
		t.Fatalf("text/plain: %d", code)
	}
	if got := s.Messages(5); len(got) != 3 || got[0].Text != "q" || got[2].Text != "json" {
		t.Fatalf("messages %+v", got)
	}
	calls := s.Calls()
	if len(calls) != 8 || calls[0].Method != "getMe" || calls[0].Status != 401 || calls[len(calls)-1].Status != 400 {
		t.Fatalf("calls %+v", calls)
	}
	if len(s.Calls("sendMessage")) != 5 {
		t.Fatalf("sendMessage calls %d", len(s.Calls("sendMessage")))
	}

	// Files are served under /file/bot<token>/ only.
	id := s.AddFile("a.txt", []byte("hello"), 0)
	_, env := post(t, s, bot+"/getFile", "application/json", `{"file_id":"`+id+`"}`)
	path := env["result"].(map[string]any)["file_path"].(string)
	for _, c := range []struct {
		p    string
		code int
	}{{"/file/bot" + s.Token() + "/" + path, 200}, {"/file/bot1:wrong/" + path, 401}, {"/file/bot" + s.Token() + "/documents/nope", 404}} {
		resp, err := http.Get(s.URL() + c.p) //nolint:noctx // test helper
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != c.code || (c.code == 200 && string(b) != "hello") {
			t.Errorf("GET %s = %d %q", c.p, resp.StatusCode, b)
		}
	}
}

func TestEntityChecks(t *testing.T) {
	e := func(typ string, off, n int) botapi.MessageEntity {
		return botapi.MessageEntity{Type: typ, Offset: off, Length: n}
	}
	ok := [][]botapi.MessageEntity{
		nil,
		{e("bold", 0, 6), e("italic", 1, 2)},     // nested
		{e("bold", 0, 3), e("italic", 3, 3)},     // adjacent
		{e("blockquote", 0, 6), e("bold", 0, 6)}, // same span
		{e("bold", 0, 2), e("bold", 0, 2)},       // duplicates nest
		{{Type: "text_link", Offset: 0, Length: 1, URL: "https://example.com"}},
	}
	for i, ents := range ok {
		if err := checkEntities("abcdef", ents); err != nil {
			t.Errorf("ok[%d]: %s", i, err.desc)
		}
	}
	bad := [][]botapi.MessageEntity{
		{e("bold", 0, 7)},
		{e("bold", -1, 2)},
		{e("bold", 1, 0)},
		{e("bold", 0, 4), e("italic", 2, 4)},
		{e("pre", 0, 6), e("bold", 1, 1)},
		{e("blockquote", 0, 6), e("expandable_blockquote", 1, 2)},
		{e("text_link", 0, 1)},
	}
	for i, ents := range bad {
		if err := checkEntities("abcdef", ents); err == nil {
			t.Errorf("bad[%d] accepted", i)
		}
	}
	// UTF-16: "😀a" is three units.
	if err := checkEntities("😀a", []botapi.MessageEntity{e("bold", 2, 1)}); err != nil {
		t.Errorf("UTF-16 offset: %s", err.desc)
	}
	if err := checkEntities("😀a", []botapi.MessageEntity{e("bold", 2, 2)}); err == nil {
		t.Error("entity past the UTF-16 end accepted")
	}
}

func TestWebhookDelivery(t *testing.T) {
	var mu sync.Mutex
	var got []string
	var bodies []botapi.Update
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, r.Header.Get(SecretHeader))
		var u botapi.Update
		_ = json.NewDecoder(r.Body).Decode(&u)
		bodies = append(bodies, u)
		if r.Header.Get(SecretHeader) != "right_secret" {
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer receiver.Close()
	s := New(t, Options{WebhookTarget: receiver.URL})
	ctx := context.Background()

	if _, err := s.Deliver(ctx, botapi.Update{}); err != nil {
		t.Fatalf("deliver without a registered webhook but with a target: %v", err)
	}
	code, env := post(t, s, "/bot"+s.Token()+"/setWebhook", "application/json",
		`{"url":"https://central.example.com/telegram/webhook/x","secret_token":"right_secret"}`)
	if code != 200 || s.WebhookSecret() != "right_secret" {
		t.Fatalf("setWebhook: %d %v", code, env)
	}
	status, err := s.Deliver(ctx, botapi.Update{Message: &botapi.Message{Text: "hi"}})
	if err != nil || status != 200 {
		t.Fatalf("deliver: %d %v", status, err)
	}
	if status, _ := s.DeliverRaw(ctx, "wrong", []byte(`{"update_id":99}`)); status != 401 {
		t.Fatalf("wrong secret: %d", status)
	}
	if status, _ := s.DeliverRaw(ctx, "", []byte(`{"update_id":100}`)); status != 401 {
		t.Fatalf("no secret: %d", status)
	}
	mu.Lock()
	if len(got) != 4 || got[1] != "right_secret" || got[2] != "wrong" || got[3] != "" || bodies[1].Message.Text != "hi" || bodies[1].UpdateID <= bodies[0].UpdateID {
		t.Fatalf("receiver saw %q %+v", got, bodies)
	}
	mu.Unlock()
	_, env = post(t, s, "/bot"+s.Token()+"/getWebhookInfo", "", "")
	info := env["result"].(map[string]any)
	if info["last_error_message"] != "Wrong response from the webhook: 401 Unauthorized" || info["last_error_date"] == nil {
		t.Fatalf("webhook info %v", info)
	}
}

func TestDeliverNeedsTarget(t *testing.T) {
	s := New(t, Options{})
	if _, err := s.Deliver(context.Background(), botapi.Update{}); err == nil {
		t.Fatal("delivered with no webhook")
	}
}

func TestTokenShape(t *testing.T) {
	a, b := New(t, Options{}), New(t, Options{})
	if !botapi.ValidToken(a.Token()) || a.Token() == b.Token() {
		t.Fatal("tokens must have the documented shape and differ per server")
	}
}
