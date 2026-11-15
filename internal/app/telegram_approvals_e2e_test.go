package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/api"
	"github.com/aurimasniekis/go-spaceman/internal/approvals"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/human"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
	tgsettings "github.com/aurimasniekis/go-spaceman/internal/telegram/settings"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/tgtest"
)

// approvalChat drives the bot's chat through the fake Bot API as Telegram user 42, linked to the Mini App env's user
// (an operator, the link capped to operator), in a space visible to Telegram.
type approvalChat struct {
	t       *testing.T
	s       *telegramSetup
	tg      *tgtest.Server
	n       int64
	version int64 // of the space's Telegram settings
}

// newApprovalChat gives the dispatcher a client of the fake Bot API without starting a poller, so each update is
// handled exactly once, by Dispatch.
func newApprovalChat(t *testing.T) *approvalChat {
	t.Helper()
	s := newTelegramSetup(t)
	tg := tgtest.New(t, tgtest.Options{Limits: tgtest.Limits{Disabled: true}})
	bot, err := botapi.New(botapi.Options{BaseURL: tg.URL(), Token: tg.Token()})
	if err != nil {
		t.Fatal(err)
	}
	s.h.c.TelegramUpdates.Dispatcher.SetBot(bot)
	s.h.c.TelegramLinks.Start(t.Context())
	// These tests tap more than the default inbound burst allows.
	perMinute, burst := int64(600), int64(100)
	if _, err := s.h.c.TelegramSettings.Put(context.Background(), audit.System, 0, tgsettings.Input{
		InboundLimits: &tgsettings.InboundLimitsInput{ActionsPerMinute: &perMinute, ActionsBurst: &burst}}); err != nil {
		t.Fatal(err)
	}
	c := &approvalChat{t: t, s: s, tg: tg, n: 1000}
	on := true
	c.setSpace(tgsettings.SpaceInput{Enabled: &on})
	return c
}

// setSpace stores the space's Telegram settings and reloads the cache the chat renders from. The respond rule reads the
// store itself.
func (c *approvalChat) setSpace(in tgsettings.SpaceInput) {
	c.t.Helper()
	ctx := context.Background()
	sp, err := c.s.h.c.TelegramSettings.PutSpace(ctx, audit.System, c.s.space, c.version, in)
	if err != nil {
		c.t.Fatal(err)
	}
	c.version = sp.Version
	if err := c.s.h.c.TelegramSettings.Load(ctx); err != nil {
		c.t.Fatal(err)
	}
}

func (c *approvalChat) optIn(on bool) {
	c.t.Helper()
	yes, mode := true, tgsettings.ApprovalsWebOnly
	if on {
		mode = tgsettings.ApprovalsConfirm
	}
	c.setSpace(tgsettings.SpaceInput{Enabled: &yes, EnforcedApprovals: &mode})
}

func (c *approvalChat) push(m *botapi.Message, q *botapi.CallbackQuery) {
	c.n++
	u := botapi.Update{UpdateID: c.n, Message: m, CallbackQuery: q}
	if q != nil {
		c.tg.PushUpdate(u) // the fake accepts answers only for taps it issued
	}
	c.s.h.c.TelegramUpdates.Dispatcher.Dispatch(context.Background(), u)
}

func (c *approvalChat) last() botapi.Message {
	ms := c.tg.Messages(42)
	return ms[len(ms)-1]
}

// message returns the current state of a message the bot sent (edits applied).
func (c *approvalChat) message(id int) botapi.Message {
	c.t.Helper()
	for _, m := range c.tg.Messages(42) {
		if m.MessageID == id {
			return m
		}
	}
	c.t.Fatalf("message %d not found", id)
	return botapi.Message{}
}

func (c *approvalChat) say(text string, replyTo int) {
	m := &botapi.Message{MessageID: int(c.n + 1), From: &botapi.User{ID: 42}, Chat: botapi.Chat{ID: 42, Type: botapi.ChatPrivate}, Text: text}
	if replyTo != 0 {
		m.ReplyToMessage = &botapi.Message{MessageID: replyTo, Chat: botapi.Chat{ID: 42}}
	}
	c.push(m, nil)
}

func (c *approvalChat) tapData(on botapi.Message, data string) {
	c.push(nil, &botapi.CallbackQuery{ID: fmt.Sprintf("q%d", c.n+1), From: botapi.User{ID: 42}, Message: &on, Data: data})
}

// tap taps the button whose label contains label; it fails when there is none.
func (c *approvalChat) tap(on botapi.Message, label string) {
	c.t.Helper()
	d := buttonData(on, label)
	if d == "" {
		c.t.Fatalf("no %q button on %q", label, on.Text)
	}
	c.tapData(on, d)
}

func buttonData(m botapi.Message, label string) string {
	if m.ReplyMarkup == nil {
		return ""
	}
	for _, row := range m.ReplyMarkup.InlineKeyboard {
		for _, b := range row {
			if strings.Contains(b.Text, label) {
				return b.CallbackData
			}
		}
	}
	return ""
}

// open lists the space's pending actions with /needsyou and taps the one with this ID: the form message.
func (c *approvalChat) open(actionID string) botapi.Message {
	c.t.Helper()
	c.say("/needsyou sso-space", 0)
	list := c.last()
	if list.ReplyMarkup != nil {
		for _, row := range list.ReplyMarkup.InlineKeyboard {
			if d := row[0].CallbackData; strings.HasSuffix(d, strings.TrimPrefix(actionID, "hum_")) {
				c.tapData(list, d)
				return c.last()
			}
		}
	}
	c.t.Fatalf("%s not in /needsyou: %q", actionID, list.Text)
	return botapi.Message{}
}

type actionState struct {
	Status      string          `json:"status"`
	Outcome     string          `json:"outcome"`
	RespondedBy string          `json:"responded_by"`
	Response    json.RawMessage `json:"response"`
}

func (c *approvalChat) action(id string) actionState {
	c.t.Helper()
	var a actionState
	r := c.s.root.do("GET", "/spaces/"+c.s.space+"/human-actions/"+id, nil)
	expect(c.t, r, 200)
	r.json(c.t, &a)
	return a
}

func (c *approvalChat) advisory(title string) string {
	c.t.Helper()
	r := c.s.root.do("POST", "/spaces/"+c.s.space+"/human-actions", map[string]any{"type": "approval", "title": title,
		"request": map[string]any{"action": title}})
	expect(c.t, r, 201)
	var a struct {
		ID string `json:"id"`
	}
	r.json(c.t, &a)
	return a.ID
}

// requester is the agent run asking for enforced approvals through the real approvals service.
func (c *approvalChat) requester() approvals.Requester {
	return approvals.Requester{SpaceID: c.s.space, Key: "run_tg", Role: "control", Actor: audit.Actor{Type: audit.ActorAgent, ID: "run_tg"},
		Human: human.Requester{Kind: "agent", ID: "run_tg", Role: "control", Run: "run_tg", Label: "control (agent)"}}
}

// require asks for (or uses) the enforced approval of merging pull request pr. It returns the finish function when an
// approval was used, else the human action that asks a person.
func (c *approvalChat) require(pr int) (func(error), string) {
	c.t.Helper()
	finish, err := c.s.h.c.Approvals.RequireFor(context.Background(), c.requester(), "mcp.github.merge", "repo/x", json.RawMessage(fmt.Sprintf(`{"pr":%d}`, pr)))
	if err == nil {
		return finish, ""
	}
	if !errors.Is(err, approvals.ErrRequired) {
		c.t.Fatalf("require: %v", err)
	}
	// The error names the human action that asks a person ("Request hum_… (approval apr_…)").
	id := regexp.MustCompile(`hum_[0-9a-z]{26}`).FindString(err.Error())
	if st, _ := c.approvalState(id); id == "" || st != approvals.Pending {
		c.t.Fatalf("approval for pr %d: %q %q (%v)", pr, id, st, err)
	}
	return nil, id
}

func (c *approvalChat) approvalState(actionID string) (state, via string) {
	_ = c.s.h.c.Store.ReadDB().QueryRowContext(context.Background(), `SELECT state, decided_via FROM approvals WHERE human_action_id = ?`, actionID).Scan(&state, &via)
	return state, via
}

// approveInChat opens an enforced approval and decides it in two steps: Approve, then Confirm approve.
func (c *approvalChat) approveInChat(actionID string) {
	c.t.Helper()
	f := c.open(actionID)
	c.tap(f, "Approve")
	if c.action(actionID).Status != "open" {
		c.t.Fatal("Approve decided without Confirm")
	}
	c.tap(c.message(f.MessageID), "Confirm approve")
	if st, via := c.approvalState(actionID); st != approvals.Approved || via != audit.ViaTelegram+c.s.linkID {
		c.t.Fatalf("enforced approval: %q via %q", st, via)
	}
}

// SM-TG-003, SM-TG-006 (TG-7-2): approvals through the fake Bot API, the real chat, API, approvals service and store.
//   - Advisory: Approve → Confirm approve decides it as the linked user; Deny asks for a note, asks again for a blank
//     one, and denies with the note.
//   - Enforced without the space's opt-in: the card says it is decided in the browser and has no decision buttons;
//     the server refuses it from the bot even with a confirmation armed for exactly it.
//   - Enforced with the opt-in: Approve → Confirm approve decides it (via telegram:<link>), and the operation then runs
//     once with that approval.
func TestTelegramApprovalsInChat(t *testing.T) {
	t.Parallel()
	c := newApprovalChat(t)

	// Advisory approve.
	adv := c.advisory("Ship the release")
	f := c.open(adv)
	if !strings.Contains(f.Text, "Approve: Ship the release") {
		t.Fatalf("advisory card: %q", f.Text)
	}
	c.tap(f, "Approve")
	if c.action(adv).Status != "open" {
		t.Fatal("Approve decided without Confirm")
	}
	confirmStep := c.message(f.MessageID)
	if !strings.Contains(confirmStep.Text, "Approve: Ship the release?") {
		t.Fatalf("confirm step: %q", confirmStep.Text)
	}
	c.tap(confirmStep, "Confirm approve")
	if a := c.action(adv); a.Status != human.Resolved || a.Outcome != "approved" || a.RespondedBy != c.s.userID {
		t.Fatalf("advisory approve: %+v", a)
	}

	// Advisory deny: a blank note is asked for again; the note goes with the denial.
	adv2 := c.advisory("Drop the table")
	f = c.open(adv2)
	c.tap(f, "Deny")
	prompt := c.last()
	if !strings.Contains(prompt.Text, "Why are you denying this?") {
		t.Fatalf("deny prompt: %q", prompt.Text)
	}
	c.say("   ", prompt.MessageID)
	again := c.last()
	if !strings.Contains(again.Text, "needs a note") || c.action(adv2).Status != "open" {
		t.Fatalf("blank note: %q", again.Text)
	}
	c.say("not before the backup", again.MessageID)
	a := c.action(adv2)
	var resp human.Response
	_ = json.Unmarshal(a.Response, &resp)
	if a.Status != human.Resolved || a.Outcome != "denied" || resp.Note != "not before the backup" {
		t.Fatalf("advisory deny: %+v %s", a, a.Response)
	}

	// Enforced, no opt-in: decided in the browser; the chat offers nothing and the server refuses the bot.
	_, enf := c.require(1)
	f = c.open(enf)
	if !strings.Contains(f.Text, "decided in the browser") || !strings.Contains(f.Text, "Operation: mcp.github.merge") ||
		buttonData(f, "Approve") != "" || buttonData(f, "Deny") != "" || strings.Contains(f.Text, `"pr"`) {
		t.Fatalf("web-only card: %q", f.Text)
	}
	c.tapData(f, "ha:"+strings.TrimPrefix(c.s.space, "spc_")+":"+strings.TrimPrefix(enf, "hum_")) // a crafted Approve
	p, err := c.s.h.c.Auth.TelegramPrincipal(context.Background(), c.s.linkID, auth.TelegramOptions{})
	if err != nil {
		t.Fatal(err)
	}
	tok := c.s.h.c.TelegramConfirmations.Arm(c.s.linkID, c.s.space, enf, "approved", 1)
	req := httptest.NewRequest(http.MethodPost, api.Prefix+"/spaces/"+c.s.space+"/human-actions/"+enf+"/respond", strings.NewReader(`{"outcome":"approved"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("If-Match", `"1"`)
	rec := httptest.NewRecorder()
	c.s.h.c.APIDeps().InProcessHandler().ServeHTTP(rec, req.WithContext(api.WithApprovalConfirmation(api.WithInProcessPrincipal(context.Background(), p), tok)))
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "decided on the web") {
		t.Fatalf("bot without the opt-in: %d %s", rec.Code, rec.Body.String())
	}
	if st, _ := c.approvalState(enf); st != approvals.Pending || c.action(enf).Status != "open" {
		t.Fatalf("enforced approval decided without the opt-in: %q", st)
	}

	// With the opt-in: two steps in chat, then the operation runs once with the approval.
	c.optIn(true)
	f = c.open(enf)
	if buttonData(f, "Approve") == "" || strings.Contains(f.Text, `"pr"`) {
		t.Fatalf("opted-in card: %q", f.Text)
	}
	c.approveInChat(enf)
	finish, id := c.require(1)
	if finish == nil || id != "" {
		t.Fatal("the approved operation did not run")
	}
	finish(nil)
}

// SM-APPR-003 (TG-7-2): an enforced approval decided in chat is rechecked before the operation runs. Approved from the
// chat with the opt-in, it is used while the approver may still decide; it is revoked, and the operation asks again,
// once the link is suspended, the user's grant is removed, the user is disabled, or the link is revoked.
func TestTelegramEnforcedApprovalRecheck(t *testing.T) {
	t.Parallel()
	c := newApprovalChat(t)
	c.optIn(true)
	// Another global administrator, so the user can be disabled.
	expect(t, c.s.root.do("POST", "/users", map[string]any{"email": "admin2@example.com", "display_name": "Admin", "password": "correct horse battery staple",
		"grants": []map[string]string{{"role": "admin", "domain": "*"}}}), 201)
	ids := map[int]string{}
	for pr := 1; pr <= 5; pr++ {
		_, ids[pr] = c.require(pr)
		c.approveInChat(ids[pr])
	}
	ctx := context.Background()
	reload := func() {
		if err := c.s.h.c.Authz.Reload(ctx); err != nil {
			t.Fatal(err)
		}
	}
	revoked := func(what string, pr int) {
		t.Helper()
		if finish, _ := c.require(pr); finish != nil {
			finish(nil)
			t.Fatalf("%s: the approval was still used", what)
		}
		if st, _ := c.approvalState(ids[pr]); st != approvals.Revoked {
			t.Fatalf("%s: approval %q", what, st)
		}
	}
	// While nothing changed, the approval is used.
	finish, _ := c.require(1)
	if finish == nil {
		t.Fatal("approved from chat, not used")
	}
	finish(nil)

	c.s.exec(t, `UPDATE telegram_links SET state = 'suspended' WHERE id = ?`, c.s.linkID)
	revoked("suspended link", 2)
	c.s.exec(t, `UPDATE telegram_links SET state = 'active' WHERE id = ?`, c.s.linkID)

	c.s.exec(t, `DELETE FROM role_grants WHERE subject = ? AND domain = ?`, auth.UserSubject(c.s.userID), c.s.space)
	reload()
	revoked("user grant removed", 3)
	c.s.exec(t, `INSERT INTO role_grants (subject, role, domain, source, created_at, created_by) VALUES (?, 'operator', ?, 'manual', 0, 't')`, auth.UserSubject(c.s.userID), c.s.space)
	reload()

	if _, err := c.s.h.c.Auth.SetEnabled(ctx, audit.System, c.s.userID, false); err != nil {
		t.Fatal(err)
	}
	revoked("disabled user", 4)
	if _, err := c.s.h.c.Auth.SetEnabled(ctx, audit.System, c.s.userID, true); err != nil {
		t.Fatal(err)
	}

	c.s.exec(t, `UPDATE telegram_links SET state = 'revoked', revoked_at = 1 WHERE id = ?`, c.s.linkID)
	revoked("revoked link", 5)
}
