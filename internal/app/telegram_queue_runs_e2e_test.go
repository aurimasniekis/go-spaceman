package app_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/messages"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
	tgnotify "github.com/aurimasniekis/go-spaceman/internal/telegram/notify"
	tgsettings "github.com/aurimasniekis/go-spaceman/internal/telegram/settings"
)

// qrChat drives the chat of Telegram user 42 through the real dispatcher.
type qrChat struct {
	t *testing.T
	e *miniAppEnv
	n int64
}

func newQRChat(t *testing.T) (*qrChat, string) {
	t.Helper()
	e := newMiniAppEnv(t)
	_ = enrolledSession(t, e.h, "mini@example.com") // operators must enroll MFA; until then the link is Limited too
	e.startBot(t)
	// A test taps faster than a person; the default burst (10) would answer "slow down".
	expect(t, e.root.do("PUT", "/telegram/settings", map[string]any{"inbound_limits": map[string]any{"actions_per_minute": 600, "actions_burst": 100}}), 200)
	ctx := t.Context()
	e.h.c.TelegramLinks.Start(ctx) // the link was inserted after central started
	nctx, stopNotify := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = e.h.c.TelegramNotify.Run(nctx); close(done) }()
	t.Cleanup(func() {
		st := time.Now()
		stopNotify()
		<-done
		t.Logf("notifier stopped in %v", time.Since(st))
	})
	on := true
	if _, err := e.h.c.TelegramSettings.PutSpace(ctx, audit.System, e.space, 0, tgsettings.SpaceInput{Enabled: &on}); err != nil {
		t.Fatal(err)
	}
	if err := e.h.c.TelegramSettings.Load(ctx); err != nil {
		t.Fatal(err)
	}
	return &qrChat{t: t, e: e}, e.space
}

// push hands an update to the bot the way Telegram does: the fake Bot API serves it to the running poller, which gives
// it to the dispatcher (dispatching it directly as well would handle it twice).
func (c *qrChat) push(m *botapi.Message, q *botapi.CallbackQuery) {
	c.n++
	c.e.tg.PushUpdate(botapi.Update{UpdateID: c.n + 5000, Message: m, CallbackQuery: q})
}

// The dispatcher handles updates on its own workers: say waits for the chat's answer and tap for the tap's, so one step
// is done before the next (a role change, say) is made.
func (c *qrChat) say(text string, replyTo *botapi.Message) {
	before := len(c.e.tg.Messages(42))
	c.push(&botapi.Message{MessageID: int(c.n + 1000), From: &botapi.User{ID: 42}, Chat: botapi.Chat{ID: 42, Type: botapi.ChatPrivate}, Text: text, ReplyToMessage: replyTo}, nil)
	eventuallyFor(c.t, "an answer to "+text, 10*time.Second, func() bool { return len(c.e.tg.Messages(42)) > before })
}

func (c *qrChat) tap(data string, on botapi.Message) {
	before := len(c.e.tg.CallbackAnswers())
	c.push(nil, &botapi.CallbackQuery{ID: "q" + strings.Repeat("x", int(c.n+1)), From: botapi.User{ID: 42}, Message: &on, Data: data})
	eventuallyFor(c.t, "an answer to the tap "+data, 10*time.Second, func() bool { return len(c.e.tg.CallbackAnswers()) > before })
}

func (c *qrChat) last() botapi.Message {
	ms := c.e.tg.Messages(42)
	if len(ms) == 0 {
		c.t.Fatal("no message")
	}
	return ms[len(ms)-1]
}

func (c *qrChat) answer() string {
	a := c.e.tg.CallbackAnswers()
	if len(a) == 0 {
		return ""
	}
	return a[len(a)-1].Text
}

func qrFind(m botapi.Message, label string) string {
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

// withButton is the latest message with a button of this label (a tap is answered before the message it opens is sent,
// so it waits for it).
func (c *qrChat) withButton(label string) botapi.Message { return c.withButtonSince(label, 0) }

// withButtonSince is withButton among the messages after the first n: a confirmation asked for again must not be
// mistaken for the (already used) one from the earlier ask, which would be found at once while the new one is still
// being sent, and answered "expired or already used".
func (c *qrChat) withButtonSince(label string, n int) botapi.Message {
	var found botapi.Message
	eventuallyFor(c.t, "a message with a "+label+" button", 10*time.Second, func() bool {
		ms := c.e.tg.Messages(42)
		for i := len(ms) - 1; i >= n && i >= 0; i-- {
			if qrFind(ms[i], label) != "" {
				found = ms[i]
				return true
			}
		}
		return false
	})
	return found
}

func (c *qrChat) setRole(role string) {
	c.e.exec(c.t, `DELETE FROM role_grants WHERE subject = ?`, auth.TelegramSubject(c.e.linkID))
	c.e.exec(c.t, `INSERT INTO role_grants (subject, role, domain, source, created_at, created_by) VALUES (?, ?, ?, 'telegram', 0, 't')`,
		auth.TelegramSubject(c.e.linkID), role, c.e.space)
	if err := c.e.h.c.Authz.Reload(context.Background()); err != nil {
		c.t.Fatal(err)
	}
}

// SM-TG-007 (TG-8b, plan §2.3, §2.5): /queue and /runs through the real dispatcher and API as the link's Telegram
// principal. A viewer link can't move the queue or stop a run (the API refuses, as it does over HTTP); the same taps
// from an operator link change the queue's rank and stop the run, and a stop asks for a confirmation first.
func TestTelegramQueueAndRunsRespectTheLinksRole(t *testing.T) {
	t.Parallel()
	c, space := newQRChat(t)
	e := c.e
	ctx := context.Background()
	var tasks []string
	for _, title := range []string{"First task", "Second task"} {
		r := e.root.do("POST", "/spaces/"+space+"/tasks", map[string]any{"title": title, "status": "ready"})
		expect(t, r, 201)
		var tk struct {
			ID string `json:"id"`
		}
		r.json(t, &tk)
		tasks = append(tasks, tk.ID)
	}
	run := ids.New(ids.Run)
	e.exec(t, `INSERT INTO runs (id, space_id, node_id, generation, kind, role, harness, instance_id, desired, observed, launch_json, created_at, created_by, updated_at, observed_at)
		VALUES (?, ?, 'nod_oidc', 1, 'worker', 'dev', 'claude-code', 'ins_x', 'running', 'running', '{}', 0, 't', 0, 0)`, run, space)
	position := func(id string) int {
		r := e.root.do("GET", "/spaces/"+space+"/tasks/"+id, nil)
		expect(t, r, 200)
		var tk struct {
			Position int `json:"position"`
		}
		r.json(t, &tk)
		return tk.Position
	}
	runDesired := func() string {
		r, err := e.h.c.Fleet.GetRun(ctx, run)
		if err != nil {
			t.Fatal(err)
		}
		return r.Desired
	}
	if position(tasks[0]) >= position(tasks[1]) {
		t.Fatalf("seed order: %d %d", position(tasks[0]), position(tasks[1]))
	}

	// A viewer link reads, but can't change anything.
	c.setRole("viewer")
	c.say("/queue sso-space", nil)
	list := c.last()
	if !strings.Contains(list.Text, "Queue of") || qrFind(list, "Second task") == "" {
		t.Fatalf("/queue: %q %+v", list.Text, list.ReplyMarkup)
	}
	c.tap(qrFind(list, "Second task"), list)
	card := c.withButton("Top")
	c.tap(qrFind(card, "Top"), card)
	if c.answer() != "That isn't available. Use /spaces to see your spaces." || position(tasks[1]) <= position(tasks[0]) {
		t.Fatalf("viewer move: answer %q, positions %d %d", c.answer(), position(tasks[0]), position(tasks[1]))
	}
	c.say("/runs sso-space", nil)
	rl := c.last()
	if !strings.Contains(rl.Text, "1 live") {
		t.Fatalf("/runs: %q", rl.Text)
	}
	c.tap(qrFind(rl, "dev"), rl)
	rc := c.withButton("Stop")
	c.tap(qrFind(rc, "Stop"), rc)
	conf := c.withButton("Stop run")
	c.tap(qrFind(conf, "Stop run"), conf)
	if c.answer() != "That isn't available. Use /spaces to see your spaces." || runDesired() != "running" {
		t.Fatalf("viewer stop: answer %q, desired %q", c.answer(), runDesired())
	}

	// An operator link can: the move changes the rank, and the stop needs its confirmation.
	c.setRole("operator")
	c.tap(qrFind(card, "Top"), card)
	if c.answer() != "Moved" || position(tasks[1]) >= position(tasks[0]) {
		t.Fatalf("operator move: answer %q, positions %d %d", c.answer(), position(tasks[0]), position(tasks[1]))
	}
	sent := len(c.e.tg.Messages(42))
	c.tap(qrFind(rc, "Stop"), rc)
	if runDesired() != "running" {
		t.Fatal("asking to stop stopped the run")
	}
	conf = c.withButtonSince("Stop run", sent)
	c.tap(qrFind(conf, "Stop run"), conf)
	if runDesired() != "stopped" {
		t.Fatalf("operator stop: answer %q, desired %q", c.answer(), runDesired())
	}

	// A stale card: the task moved on since the card was shown.
	if r := e.root.do("POST", "/spaces/"+space+"/tasks/"+tasks[0]+"/status", map[string]any{"status": "backlog"}, "If-Match", `"1"`); r.status != 200 {
		t.Fatalf("status: %d %s", r.status, r.body)
	}
	c.tap(qrFind(card, "Block"), card)
	if c.answer() == "Done" {
		t.Fatal("a stale card changed the task")
	}
}

// SM-TG-007 (TG-8b, Q13): talk to control. After /control, text becomes a message to the control run (as the user) once
// the draft is confirmed; control's reply, a message to the operators, comes back to the chat, and a Telegram reply to
// that card is a reply to the agent.
func TestTelegramTalkToControlAndReplyComeBack(t *testing.T) {
	t.Parallel()
	c, space := newQRChat(t)
	e := c.e
	ctx := context.Background()
	c.say("/control sso-space", nil)
	if !strings.Contains(c.last().Text, "Control mode") {
		t.Fatalf("/control: %q", c.last().Text)
	}
	c.say("what is the status?", nil)
	draft := c.last()
	c.tap(qrFind(draft, "Send to control"), draft)
	r := e.root.do("GET", "/spaces/"+space+"/messages", nil)
	expect(t, r, 200)
	var msgs struct {
		Items []struct {
			ID       string `json:"id"`
			ToKind   string `json:"to_kind"`
			FromKind string `json:"from_kind"`
			Subject  string `json:"subject"`
			Body     string `json:"body"`
			Kind     string `json:"kind"`
		} `json:"items"`
	}
	r.json(t, &msgs)
	if len(msgs.Items) != 1 || msgs.Items[0].ToKind != "control" || msgs.Items[0].FromKind != "operator" || msgs.Items[0].Body != "what is the status?" || msgs.Items[0].Kind != "request" {
		t.Fatalf("messages: %s", r.body)
	}
	orig := msgs.Items[0].ID

	// Control (a run) replies: a message to the operators that answers the request.
	run := ids.New(ids.Run)
	e.exec(t, `INSERT INTO runs (id, space_id, node_id, generation, kind, role, harness, instance_id, desired, observed, launch_json, created_at, created_by, updated_at, observed_at)
		VALUES (?, ?, 'nod_oidc', 1, 'control', 'control', 'claude-code', 'ins_c', 'running', 'running', '{}', 0, 't', 0, 0)`, run, space)
	if _, _, err := e.h.c.Messages.Send(ctx, audit.System, space, messages.From{Kind: "run", ID: run, Label: "control"},
		messages.SendInput{To: "operators", Kind: "reply", Subject: "Re: what is the status?", Body: "all green", ReplyTo: orig}); err != nil {
		t.Fatal(err)
	}
	var card botapi.Message
	eventuallyFor(t, "the reply card in the chat", 10*time.Second, func() bool {
		for _, m := range e.tg.Messages(42) {
			if strings.Contains(m.Text, "all green") {
				card = m
				return true
			}
		}
		return false
	})
	if !strings.Contains(card.Text, "Re: what is the status?") {
		t.Fatalf("card: %q", card.Text)
	}

	// A Telegram reply to the card is a reply to the agent (the card's mapping tells which message it shows).
	eventuallyFor(t, "the card's mapping", 10*time.Second, func() bool {
		_, err := tgnotify.StoreMappings{S: e.h.c.Store}.Card(ctx, 42, card.MessageID)
		return err == nil
	})
	c.say("thanks, carry on", &card)
	if c.last().Text != "Reply sent." {
		t.Fatalf("reply: %q", c.last().Text)
	}
	r = e.root.do("GET", "/spaces/"+space+"/messages", nil)
	expect(t, r, 200)
	var after struct {
		Items []struct {
			ToKind string `json:"to_kind"`
			ToID   string `json:"to_id"`
			Kind   string `json:"kind"`
			Body   string `json:"body"`
		} `json:"items"`
	}
	r.json(t, &after)
	found := false
	for _, m := range after.Items {
		if m.ToKind == "run" && m.ToID == run && m.Kind == "reply" && m.Body == "thanks, carry on" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no reply to the run: %s", r.body)
	}
}
