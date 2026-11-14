package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/settings"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/tgtest"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/updates"
)

const chatID = 101

func sid(c byte) string { return "spc_" + strings.Repeat(string(c), 26) }
func iid(c byte) string { return "inb_" + strings.Repeat(string(c), 26) }

var (
	spA = sid('a') // visible, readable
	spB = sid('b') // readable, but Telegram is off for it
	spC = sid('c') // readable, enabled, content level off
	spD = sid('d') // enabled, but the user can't read it (the API omits it)
	spS = sid('s') // visible with content level summary
)

// fakeAPI answers the operations the commands call, like the in-process API handler would for one user.
type fakeAPI struct {
	mu    sync.Mutex
	reqs  []string
	hdrs  map[string]string // last If-Match per path
	posts []map[string]any
	items map[string]*inboxItem
	// forbidden item IDs answer 403 on every call.
	conflict bool
}

func (f *fakeAPI) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.reqs...)
}

func (f *fakeAPI) touched(id string) bool {
	for _, r := range f.seen() {
		if strings.Contains(r, id) {
			return true
		}
	}
	return false
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, r.Method+" "+r.URL.RequestURI())
	path := strings.TrimPrefix(r.URL.Path, "/api/v1")
	out := func(code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	seg := strings.Split(strings.Trim(path, "/"), "/")
	switch {
	case path == "/spaces":
		out(200, map[string]any{"items": []spaceRef{
			{ID: spA, Slug: "alpha", Name: "Alpha", State: "active"}, {ID: spB, Slug: "beta", Name: "Beta", State: "active"},
			{ID: spC, Slug: "gamma", Name: "Gamma", State: "active"}, {ID: spS, Slug: "sum", Name: "Summary", State: "active"}}})
	case path == "/attention":
		out(200, map[string]any{"spaces": []map[string]any{{"space_id": spA, "human_pending": 2, "tasks_failed": 1}}})
	case len(seg) == 2 && seg[0] == "spaces":
		if seg[1] == spD {
			out(403, map[string]any{"status": 403, "detail": "forbidden"})
			return
		}
		out(200, spaceRef{ID: seg[1], Slug: "s", Name: "Space " + seg[1][4:6], State: "active"})
	case len(seg) == 3 && seg[2] == "runs":
		out(200, map[string]any{"items": []map[string]any{{"kind": "worker", "observed": "running"}, {"kind": "control", "observed": "running"}}})
	case len(seg) == 3 && seg[2] == "tasks":
		out(200, map[string]any{"tasks": []map[string]any{{"title": "Build it", "status": "running"}, {"title": "Then test", "status": "ready"}, {"title": "Stuck", "status": "blocked"}}})
	case len(seg) == 3 && seg[2] == "inbox" && r.Method == http.MethodGet:
		var all []*inboxItem
		for _, it := range f.items {
			if it.SpaceID == seg[1] && it.Status == r.URL.Query().Get("status") {
				all = append(all, it)
			}
		}
		sortItems(all)
		lim := 50
		if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil {
			lim = v
		}
		if len(all) > lim {
			all = all[:lim]
		}
		items := make([]inboxItem, len(all))
		for i, it := range all {
			items[i] = *it
		}
		out(200, inboxPage{Items: items, Counts: map[string]int64{"new": 3, "in_progress": 1}})
	case len(seg) == 4 && seg[2] == "inbox" && r.Method == http.MethodGet:
		it := f.items[seg[3]]
		if it == nil || it.SpaceID != seg[1] {
			out(404, map[string]any{"status": 404})
			return
		}
		out(200, it)
	case len(seg) == 5 && seg[4] == "status" && r.Method == http.MethodPost:
		it := f.items[seg[3]]
		if it == nil || it.SpaceID != seg[1] {
			out(404, map[string]any{"status": 404})
			return
		}
		f.hdrs[path] = r.Header.Get("If-Match")
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.posts = append(f.posts, body)
		if f.conflict || r.Header.Get("If-Match") != fmt.Sprintf(`"%d"`, it.Version) {
			out(412, map[string]any{"status": 412, "detail": "the item changed"})
			return
		}
		it.Status, _ = body["status"].(string)
		it.Reason, _ = body["reason"].(string)
		it.Version++
		out(200, it)
	default:
		out(404, map[string]any{"status": 404})
	}
}

func sortItems(a []*inboxItem) {
	for i := range a {
		for j := i + 1; j < len(a); j++ {
			if a[j].ID < a[i].ID {
				a[i], a[j] = a[j], a[i]
			}
		}
	}
}

type env struct {
	t   *testing.T
	srv *tgtest.Server
	api *fakeAPI
	h   *Handler
	l   updates.Link
	// principalErr makes the principal build fail (a revoked link).
	principalErr error
	upd          int64
}

func newEnv(t *testing.T) *env {
	t.Helper()
	srv := tgtest.New(t, tgtest.Options{Limits: tgtest.Limits{Disabled: true}})
	bc, err := botapi.New(botapi.Options{BaseURL: srv.URL(), Token: srv.Token()})
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, srv: srv, l: updates.Link{ID: "tgl_1", UserID: "usr_1", ChatID: chatID}}
	e.api = &fakeAPI{hdrs: map[string]string{}, items: map[string]*inboxItem{}}
	e.addItem(spA, iid('1'), "First item", "new", "# hello\nbody text")
	e.addItem(spS, iid('2'), "Summary item", "new", "SECRET BODY")
	e.addItem(spB, iid('3'), "Hidden item", "new", "nope")
	e.h = New(Options{
		API: e.api,
		Principal: func(context.Context, string) (*auth.Principal, error) {
			if e.principalErr != nil {
				return nil, e.principalErr
			}
			return &auth.Principal{User: &auth.User{ID: "usr_1"}, TelegramLinkID: "tgl_1"}, nil
		},
		Space: func(id string) settings.Space {
			s := settings.DefaultSpace(id)
			switch id {
			case spA, spD:
				s.Enabled = true
			case spS:
				s.Enabled, s.ContentLevel = true, settings.ContentSummary
			case spC:
				s.Enabled, s.ContentLevel = true, settings.ContentOff
			}
			return s
		},
		Bot:      func() Bot { return bc },
		PageSize: 2,
		Clock:    clock.NewFake(time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)),
	})
	return e
}

func (e *env) addItem(space, id, title, status, body string) *inboxItem {
	it := &inboxItem{ID: id, SpaceID: space, Title: title, Status: status, Body: body, Urgency: "high", SourceKind: "operator", SourceLabel: "Telegram", Version: 3,
		ReceivedAt: time.Date(2026, 11, 13, 8, 0, 0, 0, time.UTC)}
	e.api.items[id] = it
	return it
}

func (e *env) say(text string) {
	e.upd++
	e.h.Handle(context.Background(), botapi.Update{UpdateID: e.upd, Message: &botapi.Message{MessageID: int(e.upd), From: &botapi.User{ID: chatID}, Chat: botapi.Chat{ID: chatID, Type: botapi.ChatPrivate}, Text: text}}, e.l)
}

// tap taps a button on the latest message that has one with this callback data prefix.
func (e *env) tapData(data string, on botapi.Message) {
	e.upd++
	u := botapi.Update{UpdateID: e.upd, CallbackQuery: &botapi.CallbackQuery{ID: fmt.Sprintf("cb%d", e.upd), From: botapi.User{ID: chatID}, Message: &on, Data: data}}
	e.srv.PushUpdate(u) // the fake only accepts answers to taps it delivered
	e.h.Handle(context.Background(), u, e.l)
}

func (e *env) last() botapi.Message {
	e.t.Helper()
	ms := e.srv.Messages(chatID)
	if len(ms) == 0 {
		e.t.Fatal("no message sent")
	}
	return ms[len(ms)-1]
}

func buttons(m botapi.Message) (texts, data []string) {
	if m.ReplyMarkup == nil {
		return
	}
	for _, r := range m.ReplyMarkup.InlineKeyboard {
		for _, b := range r {
			texts = append(texts, b.Text)
			data = append(data, b.CallbackData)
		}
	}
	return
}

func find(m botapi.Message, label string) string {
	texts, data := buttons(m)
	for i, t := range texts {
		if strings.Contains(t, label) {
			return data[i]
		}
	}
	return ""
}

func (e *env) lastAnswer() string {
	a := e.srv.CallbackAnswers()
	if len(a) == 0 {
		return ""
	}
	return a[len(a)-1].Text
}

// SM-TG-007 (TG-8a-1): /spaces lists one button per space the user can read and Telegram can see. A space with
// Telegram off, with content level off, or that the API omits for the user never appears; badges come from the
// attention summary.
func TestSpacesListsOnlyVisible(t *testing.T) {
	e := newEnv(t)
	e.say("/spaces")
	texts, data := buttons(e.last())
	if len(texts) != 2 || data[0] != "sp:"+spA || data[1] != "sp:"+spS {
		t.Fatalf("buttons %v %v", texts, data)
	}
	if !strings.Contains(texts[0], "Alpha") || !strings.Contains(texts[0], "🙋2") || !strings.Contains(texts[0], "❌1") {
		t.Fatalf("badges: %q", texts[0])
	}
	for _, s := range []string{spB, spC, spD} {
		for _, d := range data {
			if strings.Contains(d, s) {
				t.Fatalf("%s listed", s)
			}
		}
	}
	// The command works with a bot suffix and in capitals.
	e.say("/SPACES@mybot")
	if n := len(e.srv.Messages(chatID)); n != 2 {
		t.Fatalf("messages %d", n)
	}
}

func TestSpacesNoneVisible(t *testing.T) {
	e := newEnv(t)
	e.h.o.Space = func(id string) settings.Space { return settings.DefaultSpace(id) }
	e.say("/spaces")
	if got := e.last().Text; got != TextNoSpaces {
		t.Fatalf("got %q", got)
	}
}

// Tapping a space switches to it and shows the overview; a space invisible to Telegram, unreadable by the user, or made
// up gets the same answer and no API call at all for the hidden one.
func TestSpaceSwitchAndOverview(t *testing.T) {
	e := newEnv(t)
	e.say("/spaces")
	list := e.last()
	e.tapData("sp:"+spA, list)
	card := e.srv.Messages(chatID)[0] // the list message was edited into the overview
	for _, want := range []string{"State: active", "Control run: running", "Running tasks: 1 (Build it)", "Next ready: Then test", "Blocked: 1", "Needs you: 2", "Inbox: 3 new"} {
		if !strings.Contains(card.Text, want) {
			t.Errorf("overview lacks %q:\n%s", want, card.Text)
		}
	}
	for _, s := range []string{spB, spC} {
		e.tapData("sp:"+s, list)
		if e.lastAnswer() != TextUnavailable {
			t.Errorf("%s: answer %q", s, e.lastAnswer())
		}
		if e.api.touched(s) {
			t.Errorf("the API was called for the hidden space %s: %v", s, e.api.seen())
		}
	}
	e.tapData("sp:"+spD, list) // enabled, but the API refuses the user
	if e.lastAnswer() != TextUnavailable {
		t.Errorf("unreadable space: %q", e.lastAnswer())
	}
	e.tapData("sp:spc_zzzz", list)
	if e.lastAnswer() != TextUnavailable {
		t.Errorf("made-up space: %q", e.lastAnswer())
	}
}

func TestSpaceCommand(t *testing.T) {
	e := newEnv(t)
	e.say("/space beta") // off for Telegram
	if e.last().Text != TextUnavailable || e.api.touched(spB) {
		t.Fatalf("invisible space: %q %v", e.last().Text, e.api.seen())
	}
	e.say("/space gamma") // content level off
	if e.last().Text != TextUnavailable {
		t.Fatal(e.last().Text)
	}
	e.say("/space nothing")
	if e.last().Text != TextUnavailable {
		t.Fatal(e.last().Text)
	}
	e.say("/space ALPHA")
	if !strings.Contains(e.last().Text, "State: active") {
		t.Fatal(e.last().Text)
	}
	if e.h.currentSpace("tgl_1") != spA {
		t.Fatalf("current %q", e.h.currentSpace("tgl_1"))
	}
	e.say("/space") // no argument: the chosen space
	if !strings.Contains(e.last().Text, "State: active") {
		t.Fatal(e.last().Text)
	}
}

// With two visible spaces and none chosen, /space and /inbox ask for one instead of guessing.
func TestNoGuessingBetweenSpaces(t *testing.T) {
	e := newEnv(t)
	e.say("/inbox")
	if e.last().Text != TextPickSpace {
		t.Fatalf("got %q", e.last().Text)
	}
	e.say("/space")
	if e.last().Text != TextPickSpace {
		t.Fatalf("got %q", e.last().Text)
	}
}

func TestInboxList(t *testing.T) {
	e := newEnv(t)
	for i := byte('4'); i <= '8'; i++ {
		e.addItem(spA, iid(i), "Item "+string(i), "new", "b")
	}
	e.addItem(spA, iid('9'), "Deferred one", "deferred", "b")
	e.say("/inbox alpha")
	m := e.last()
	if !strings.Contains(m.Text, "Inbox of Alpha") || !strings.Contains(m.Text, "· new") {
		t.Fatalf("text %q", m.Text)
	}
	texts, data := buttons(m)
	items := 0
	for _, d := range data {
		if strings.HasPrefix(d, "ir:") {
			items++
		}
		if len(d) > 64 {
			t.Errorf("callback data too long: %q", d)
		}
	}
	if items != 2 {
		t.Fatalf("page size: %d items in %v", items, texts)
	}
	next := find(m, "Next")
	if next == "" || find(m, "Prev") != "" {
		t.Fatalf("navigation %v", texts)
	}
	e.tapData(next, m)
	m2 := e.srv.Messages(chatID)[0] // edited in place
	if find(m2, "Prev") == "" || find(m2, "Next") == "" {
		t.Fatalf("page 2 navigation %v", m2.ReplyMarkup)
	}
	// Switch the filter.
	e.tapData(find(m2, "Deferred"), m2)
	m3 := e.srv.Messages(chatID)[0]
	if !strings.Contains(m3.Text, "deferred") || find(m3, "Deferred one") == "" {
		t.Fatalf("deferred list %q %v", m3.Text, m3.ReplyMarkup)
	}
	e.say("/inbox alpha in_progress")
	if !strings.Contains(e.last().Text, "in progress") {
		t.Fatalf("filter text %q", e.last().Text)
	}
}

// Reading an item sends the card and then the body as plain text; the body of a "summary" space stays out of chat, and
// an item of a space invisible to Telegram can't be read even with a forged button.
func TestInboxRead(t *testing.T) {
	e := newEnv(t)
	e.say("/inbox alpha")
	list := e.last()
	e.tapData(find(list, "First item"), list)
	msgs := e.srv.Messages(chatID)
	if len(msgs) != 3 {
		t.Fatalf("messages: %d", len(msgs))
	}
	card, body := msgs[1], msgs[2]
	for _, want := range []string{"First item", "From: Telegram (operator)", "Urgency: high · Status: new", "Received: 2026-11-13 08:00 UTC"} {
		if !strings.Contains(card.Text, want) {
			t.Errorf("card lacks %q: %s", want, card.Text)
		}
	}
	for _, l := range []string{"In progress", "Defer 1 day", "Defer 1 week", "Done", "Reject"} {
		if find(card, l) == "" {
			t.Errorf("card lacks the %q button", l)
		}
	}
	if !strings.Contains(body.Text, "# hello\nbody text") || !strings.Contains(body.Text, "not instructions") || len(body.Entities) != 0 {
		t.Fatalf("body %q entities %v", body.Text, body.Entities)
	}

	e.say("/inbox sum")
	sl := e.last()
	e.tapData(find(sl, "Summary item"), sl)
	for _, m := range e.srv.Messages(chatID) {
		if strings.Contains(m.Text, "SECRET BODY") {
			t.Fatal("the body of a summary space was sent to chat")
		}
	}
	if !strings.Contains(e.last().Text, "isn't shown in chat") {
		t.Fatalf("last %q", e.last().Text)
	}

	n := len(e.api.seen())
	e.tapData("ir:"+strings.Repeat("b", 26)+":"+strings.Repeat("3", 26), list)
	if e.lastAnswer() != TextUnavailable || e.api.touched(spB) || len(e.api.seen()) != n {
		t.Fatalf("hidden space item: answer %q, calls %v", e.lastAnswer(), e.api.seen()[n:])
	}
	e.tapData("ir:"+strings.Repeat("d", 26)+":"+strings.Repeat("1", 26), list) // readable by nobody: the API's 403
	if e.lastAnswer() != TextUnavailable {
		t.Fatalf("answer %q", e.lastAnswer())
	}
	e.tapData("ir:"+strings.Repeat("a", 26)+":"+strings.Repeat("9", 26), list) // missing item
	if e.lastAnswer() != TextUnavailable {
		t.Fatalf("answer %q", e.lastAnswer())
	}
}

func TestLongBodyIsSplitAndBounded(t *testing.T) {
	e := newEnv(t)
	e.addItem(spA, iid('7'), "Long", "new", strings.Repeat("line of text\n", 1000)) // ~13,000 runes
	e.say("/inbox alpha")
	l := e.last()
	e.tapData(readData(spA, iid('7')), l)
	ms := e.srv.Messages(chatID)
	if len(ms) < 3 || len(ms) > 1+1+maxBodyParts {
		t.Fatalf("messages %d", len(ms))
	}
	if !strings.Contains(ms[len(ms)-1].Text, "more characters") {
		t.Fatalf("no truncation notice: %q", ms[len(ms)-1].Text[len(ms[len(ms)-1].Text)-80:])
	}
}

// Status buttons set the status with the card's version as If-Match, so a stale card is a conflict and nothing is
// overwritten. Defer carries a defer_until; Reject asks for a reason first.
func TestInboxStatus(t *testing.T) {
	e := newEnv(t)
	e.say("/inbox alpha")
	list := e.last()
	e.tapData(find(list, "First item"), list)
	card := e.srv.Messages(chatID)[1]

	e.tapData(find(card, "In progress"), card)
	if it := e.api.items[iid('1')]; it.Status != "in_progress" || it.Version != 4 {
		t.Fatalf("item %+v", it)
	}
	if got := e.api.hdrs["/spaces/"+spA+"/inbox/"+iid('1')+"/status"]; got != `"3"` {
		t.Fatalf("If-Match %q", got)
	}
	// The card now shows the new status and no longer offers it.
	edited := e.srv.Messages(chatID)[1]
	if !strings.Contains(edited.Text, "Status: in progress") || find(edited, "In progress") != "" {
		t.Fatalf("edited card %q %v", edited.Text, edited.ReplyMarkup)
	}

	// The old card (version 3) is stale now.
	e.tapData(find(card, "Done"), card)
	if e.lastAnswer() != TextStale || e.api.items[iid('1')].Status != "in_progress" {
		t.Fatalf("stale tap: %q, status %q", e.lastAnswer(), e.api.items[iid('1')].Status)
	}

	// Defer 1 day sets defer_until a day from the clock.
	e.tapData(find(edited, "Defer 1 day"), edited)
	last := e.api.posts[len(e.api.posts)-1]
	if last["status"] != "deferred" || last["defer_until"] != "2026-11-15T09:00:00Z" {
		t.Fatalf("defer body %v", last)
	}
}

func TestInboxReject(t *testing.T) {
	e := newEnv(t)
	e.say("/inbox alpha")
	list := e.last()
	e.tapData(find(list, "First item"), list)
	card := e.srv.Messages(chatID)[1]
	e.tapData(find(card, "Reject"), card)
	prompt := e.last()
	if !strings.Contains(prompt.Text, "Why reject") {
		t.Fatalf("prompt %q", prompt.Text)
	}
	if e.api.items[iid('1')].Status != "new" {
		t.Fatal("rejected without a reason")
	}
	// A reply to something else does nothing.
	e.upd++
	e.h.Handle(context.Background(), botapi.Update{UpdateID: e.upd, Message: &botapi.Message{MessageID: 900, From: &botapi.User{ID: chatID}, Chat: botapi.Chat{ID: chatID, Type: botapi.ChatPrivate}, Text: "because", ReplyToMessage: &botapi.Message{MessageID: 777, Chat: botapi.Chat{ID: chatID}}}}, e.l)
	if e.api.items[iid('1')].Status != "new" {
		t.Fatal("a stray reply rejected the item")
	}
	reply := botapi.Message{MessageID: 901, From: &botapi.User{ID: chatID}, Chat: botapi.Chat{ID: chatID, Type: botapi.ChatPrivate}, Text: "  not relevant  ", ReplyToMessage: &prompt}
	e.h.Handle(context.Background(), botapi.Update{UpdateID: 99, Message: &reply}, e.l)
	it := e.api.items[iid('1')]
	if it.Status != "rejected" || it.Reason != "not relevant" {
		t.Fatalf("item %+v", it)
	}
	// The prompt is single-use.
	e.api.items[iid('1')].Status = "new"
	e.h.Handle(context.Background(), botapi.Update{UpdateID: 100, Message: &reply}, e.l)
	if e.api.items[iid('1')].Status != "new" {
		t.Fatal("the prompt was used twice")
	}
}

// A link that no longer acts (revoked, user disabled, wrong user) runs nothing: no API call, a short notice.
func TestNoPrincipalNoCalls(t *testing.T) {
	e := newEnv(t)
	e.principalErr = errors.New("unauthorized")
	for _, c := range []string{"/spaces", "/space alpha", "/inbox alpha"} {
		e.say(c)
		if e.last().Text != TextNoAccess {
			t.Fatalf("%s: %q", c, e.last().Text)
		}
	}
	e.tapData("sp:"+spA, e.last())
	e.tapData(statusData(spA, iid('1'), "x", 3), e.last())
	if e.lastAnswer() != TextNoAccess {
		t.Fatalf("answer %q", e.lastAnswer())
	}
	if n := len(e.api.seen()); n != 0 {
		t.Fatalf("API calls: %v", e.api.seen())
	}
	e.principalErr = nil
	e.h.o.Principal = func(context.Context, string) (*auth.Principal, error) {
		return &auth.Principal{User: &auth.User{ID: "someone-else"}, TelegramLinkID: "tgl_1"}, nil
	}
	e.say("/spaces")
	if e.last().Text != TextNoAccess || len(e.api.seen()) != 0 {
		t.Fatal("a principal of another user acted")
	}
}

// A tap on a message from another chat, or from a button the bot never sent, does nothing.
func TestForeignAndUnknownTaps(t *testing.T) {
	e := newEnv(t)
	e.tapData("zz:1", botapi.Message{MessageID: 1, Chat: botapi.Chat{ID: chatID}})
	e.tapData("sp:"+spA, botapi.Message{MessageID: 1, Chat: botapi.Chat{ID: 555}})
	e.tapData("il:garbage", botapi.Message{MessageID: 1, Chat: botapi.Chat{ID: chatID}})
	e.tapData("is:"+strings.Repeat("a", 26)+":"+strings.Repeat("1", 26)+":x:0", botapi.Message{MessageID: 1, Chat: botapi.Chat{ID: chatID}})
	if n := len(e.api.seen()); n > 2 {
		t.Fatalf("calls %v", e.api.seen())
	}
	for _, r := range e.api.seen() {
		if strings.Contains(r, "POST") {
			t.Fatalf("a write happened: %s", r)
		}
	}
}

func TestCleanAndParts(t *testing.T) {
	if got := clean("a\u202eb\n\n c\x00d", 10); got != "a b c d" {
		t.Fatalf("%q", got)
	}
	if got := clean(strings.Repeat("x", 20), 5); got != "xxxxx…" {
		t.Fatalf("%q", got)
	}
	ps, dropped := parts("one\r\ntwo\x07")
	if len(ps) != 1 || ps[0] != "one\ntwo" || dropped != 0 {
		t.Fatalf("%q %d", ps, dropped)
	}
	if c, a, ok := command("/inbox@bot new  alpha"); !ok || c != "inbox" || a != "new  alpha" {
		t.Fatalf("%q %q %v", c, a, ok)
	}
	if _, _, ok := command("hello /inbox"); ok {
		t.Fatal("not a command")
	}
}

var _ updates.Handler = (*Handler)(nil)

// A forged item ID in callback data (path pieces, a query) is refused before any API call.
func TestForgedItemIDs(t *testing.T) {
	e := newEnv(t)
	on := botapi.Message{MessageID: 1, Chat: botapi.Chat{ID: chatID}}
	a := strings.Repeat("a", 26)
	for _, bad := range []string{"x/../../../" + spB + "/inbox/" + iid('3'), strings.Repeat("1", 25) + "?", "A" + strings.Repeat("1", 25), strings.Repeat("1", 27)} {
		e.tapData("ir:"+a+":"+bad, on)
		e.tapData("is:"+a+":"+bad+":x:3", on)
	}
	for _, r := range e.api.seen() {
		if strings.Contains(r, "/inbox/") {
			t.Fatalf("a forged item ID reached the API: %s", r)
		}
	}
}

// The last page the API can serve is the last page offered: no request asks for more than its largest limit.
func TestPageCap(t *testing.T) {
	e := newEnv(t)
	e.h.o.PageSize = 8
	e.say("/inbox alpha")
	e.tapData(listData(spA, "n", 24), e.last())
	e.tapData(listData(spA, "n", 500), e.last())
	saw := false
	for _, r := range e.api.seen() {
		if i := strings.Index(r, "limit="); i >= 0 {
			var n int
			if _, err := fmt.Sscanf(r[i:], "limit=%d", &n); err != nil || n > maxLimit {
				t.Fatalf("request %s asks for too many", r)
			}
			saw = true
		}
	}
	if !saw {
		t.Fatal("no list request seen")
	}
}

// One link's reject prompts never evict another's, and an old prompt expires.
func TestPendingBounds(t *testing.T) {
	e := newEnv(t)
	clk := e.h.o.Clock.(*clock.Fake)
	e.h.addPending(pendingKey{1, 1}, rej{linkID: "tgl_other", spaceID: spA, itemID: iid('1'), version: 3})
	for i := 0; i < 50; i++ {
		e.h.addPending(pendingKey{2, i}, rej{linkID: "tgl_1", spaceID: spA, itemID: iid('1'), version: 3})
	}
	e.h.mu.Lock()
	n := len(e.h.pending)
	_, other := e.h.pending[pendingKey{1, 1}]
	e.h.mu.Unlock()
	if !other || n != maxPendingPerLink+1 {
		t.Fatalf("other kept %v, %d entries", other, n)
	}
	clk.Advance(pendingTTL + time.Minute)
	e.h.addPending(pendingKey{3, 1}, rej{linkID: "tgl_1"})
	e.h.mu.Lock()
	n = len(e.h.pending)
	e.h.mu.Unlock()
	if n != 1 {
		t.Fatalf("%d entries after expiry", n)
	}
}
