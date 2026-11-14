package app_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/aurimasniekis/go-spaceman/internal/api"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/config"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
	tglink "github.com/aurimasniekis/go-spaceman/internal/telegram/link"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/tgtest"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/updates"
)

// linkEnv is central with a configured bot row (not started: the tests call the dispatcher and the link service
// directly), the fake Bot API as the dispatcher's client, and a space.
type linkEnv struct {
	h     *harness
	tg    *tgtest.Server
	root  *client
	space string
}

const linkPassword = "correct horse battery staple"

func newLinkEnv(t *testing.T) *linkEnv {
	t.Helper()
	tg := tgtest.New(t, tgtest.Options{Limits: tgtest.Limits{Disabled: true}})
	h := newHarnessWith(t, func(c *config.Config) { c.Telegram.APIBaseURL = tg.URL() })
	e := &linkEnv{h: h, tg: tg, space: seedSpace(t, h)}
	rootSecret, _, err := h.c.Auth.IssueRootToken(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	e.root = h.client()
	e.root.bearer = rootSecret
	ts := &telegramSetup{h: h}
	ts.exec(t, `INSERT INTO telegram_bot (singleton, bot_id, username, token_enc, webhook_secret_enc, hook_id, mode, enabled, created_at, created_by, updated_at, updated_by)
		VALUES (1, 999, 'spaceman_test_bot', x'00', x'00', 'hhhhhhhhhhhhhhhhhhhhhh', 'polling', 1, 0, 't', 0, 't')`)
	bot, err := botapi.New(botapi.Options{BaseURL: tg.URL(), Token: tg.Token()})
	if err != nil {
		t.Fatal(err)
	}
	h.c.TelegramUpdates.Dispatcher.SetBot(bot)
	return e
}

// webUser is a person with an MFA-enrolled browser session.
type webUser struct {
	id     string
	ui     *client
	secret []byte
}

func (e *linkEnv) user(t *testing.T, email, name string, grants []map[string]string) *webUser {
	t.Helper()
	r := e.root.do("POST", "/users", map[string]any{"email": email, "display_name": name, "password": linkPassword, "grants": grants})
	expect(t, r, 201)
	var u struct {
		ID string `json:"id"`
	}
	r.json(t, &u)
	w := &webUser{id: u.ID}
	w.ui, w.secret = e.session(t, email)
	return w
}

// session signs in, enrolls TOTP and verifies it (or only verifies when already enrolled).
func (e *linkEnv) session(t *testing.T, email string) (*client, []byte) {
	t.Helper()
	h := e.h
	ui := h.client()
	r := ui.do("POST", "/auth/login", map[string]string{"email": email, "password": linkPassword})
	expect(t, r, 200)
	var me api.Me
	r.json(t, &me)
	ui.csrf = me.CSRFToken
	r = ui.do("POST", "/auth/mfa/enroll", nil)
	expect(t, r, 200)
	var enr api.TOTPEnrollment
	r.json(t, &enr)
	secret, err := auth.DecodeTOTPSecret(enr.Secret)
	if err != nil {
		t.Fatal(err)
	}
	expect(t, ui.do("POST", "/auth/mfa/enroll/confirm", map[string]string{"code": auth.TOTPCode(secret, auth.TOTPStep(h.clk.Now()))}), 200)
	h.clk.Advance(30 * time.Second)
	r = ui.do("POST", "/auth/mfa/verify", map[string]string{"code": auth.TOTPCode(secret, auth.TOTPStep(h.clk.Now()))})
	expect(t, r, 200)
	r.json(t, &me)
	ui.csrf = me.CSRFToken
	return ui, secret
}

func (w *webUser) reauth(t *testing.T, h *harness) {
	t.Helper()
	h.clk.Advance(30 * time.Second) // a fresh TOTP step
	r := w.ui.do("POST", "/auth/reauth", map[string]string{"password": linkPassword, "code": auth.TOTPCode(w.secret, auth.TOTPStep(h.clk.Now()))})
	expect(t, r, 200)
	var me api.Me
	r.json(t, &me)
	w.ui.csrf = me.CSRFToken
}

func (w *webUser) createCode(t *testing.T) tglink.Code {
	t.Helper()
	r := w.ui.do("POST", "/me/telegram/link-codes", nil)
	expect(t, r, 201)
	var c tglink.Code
	r.json(t, &c)
	if len(c.Code) != 43 || c.ID == "" || c.ID == c.Code || c.State != tglink.StateNew || !strings.HasSuffix(c.DeepLink, "?start="+c.Code) {
		t.Fatalf("created code %+v", c)
	}
	return c
}

func (w *webUser) getCode(t *testing.T, id string) tglink.Code {
	t.Helper()
	r := w.ui.do("GET", "/me/telegram/link-codes/"+id, nil)
	expect(t, r, 200)
	var c tglink.Code
	r.json(t, &c)
	return c
}

func (w *webUser) confirm(id string, body any) resp {
	return w.ui.do("POST", "/me/telegram/link-codes/"+id+"/confirm", body)
}

func tgPerson(id int64, first string) botapi.User {
	return botapi.User{ID: id, FirstName: first, Username: strings.ToLower(first)}
}

// start sends /start <code> as a Telegram user, through the link service (the dispatcher's linking step).
func (e *linkEnv) start(from botapi.User, code string) updates.LinkReply {
	u := botapi.Update{Message: &botapi.Message{MessageID: 1, From: &from, Chat: botapi.Chat{ID: from.ID, Type: botapi.ChatPrivate}, Text: "/start " + code}}
	return e.h.c.TelegramLink.HandleUnlinked(context.Background(), u, from)
}

// tap taps a link button with the given callback data.
func (e *linkEnv) tap(from botapi.User, data string) updates.LinkReply {
	u := botapi.Update{CallbackQuery: &botapi.CallbackQuery{ID: "cb", From: from, Data: data,
		Message: &botapi.Message{MessageID: 7, Chat: botapi.Chat{ID: from.ID, Type: botapi.ChatPrivate}}}}
	return e.h.c.TelegramLink.HandleUnlinked(context.Background(), u, from)
}

func buttons(t *testing.T, r updates.LinkReply) (confirm, refuse string) {
	t.Helper()
	if r.Markup == nil || len(r.Markup.InlineKeyboard) != 1 || len(r.Markup.InlineKeyboard[0]) != 2 {
		t.Fatalf("no confirmation buttons: %+v", r)
	}
	row := r.Markup.InlineKeyboard[0]
	if row[0].Text != "Confirm" || row[1].Text != "This wasn't me" || len(row[0].CallbackData) > 64 {
		t.Fatalf("buttons %+v", row)
	}
	return row[0].CallbackData, row[1].CallbackData
}

func (e *linkEnv) count(t *testing.T, q string, args ...any) int {
	t.Helper()
	var n int
	if err := e.h.c.Store.ReadDB().QueryRowContext(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

// SM-TG-001 (ADR-0083 decision 2, plan §3.1 "the reverse attack"): an attacker creates a code in their own account
// and sends the deep link to a victim, hoping to bind the victim's Telegram account to the attacker's SpaceMan
// account. The victim's /start shows a prompt that names central's host and the attacker's account (display name and
// masked e-mail) before anything binds. "This wasn't me" cancels the code and is audited; the attacker's web
// confirmation is then refused, and so is a later [Confirm] tap. Without the victim's Telegram confirmation, the
// attacker's web confirmation is refused too. No link and no link grant exists at the end.
func TestTelegramLinkAttackerInitiatedIsRefused(t *testing.T) {
	t.Parallel()
	e := newLinkEnv(t)
	mallory := e.user(t, "mallory@example.com", "Mallory Attacker", []map[string]string{{"role": "operator", "domain": e.space}})
	victim := tgPerson(777, "Victim")

	// The victim taps the attacker's link and is told whose account it is.
	code := mallory.createCode(t)
	prompt := e.start(victim, code.Code)
	if prompt.Bounded || !strings.Contains(prompt.Text, "Mallory Attacker") || !strings.Contains(prompt.Text, "m***@example.com") ||
		!strings.Contains(prompt.Text, mustURL(t, e.h.srv.URL).Host) || strings.Contains(prompt.Text, "mallory@") {
		t.Fatalf("the prompt doesn't name the host and the attacker's account: %q", prompt.Text)
	}
	if len(prompt.Entities) != 2 || prompt.Entities[0].Type != "bold" || prompt.Entities[1].Type != "bold" {
		t.Fatalf("the host and the account name aren't bold: %+v", prompt.Entities)
	}
	if strings.Contains(prompt.Text, code.Code) {
		t.Fatal("the prompt repeats the code")
	}
	ok, no := buttons(t, prompt)
	// Before the victim confirms in Telegram, the attacker can't finish the link from the web.
	if p := mallory.confirm(code.ID, nil).problem(t); p.Code != "precondition_failed" {
		t.Fatalf("attacker's confirmation before the Telegram confirmation: %+v", p)
	}
	if st := mallory.getCode(t, code.ID); st.State != tglink.StateRedeemed || st.Telegram != nil {
		t.Fatalf("the page shows the Telegram account before it confirmed: %+v", st)
	}
	// Someone else can't refuse or confirm in the victim's place.
	if r := e.tap(tgPerson(778, "Other"), no); r.Text != "" || r.CallbackText == "" {
		t.Fatalf("another Telegram account refused the victim's code: %+v", r)
	}
	if r := e.tap(tgPerson(778, "Other"), ok); r.Text != "" {
		t.Fatalf("another Telegram account confirmed the victim's code: %+v", r)
	}
	// The victim refuses.
	if r := e.tap(victim, no); !r.ClearKeyboard || !strings.Contains(r.Text, "nothing was linked") {
		t.Fatalf("refusal reply %+v", r)
	}
	if n := e.count(t, `SELECT count(*) FROM audit_events WHERE action = 'telegram.link_code.refused' AND outcome = 'denied' AND target_id = ?`, code.ID); n != 1 {
		t.Fatalf("refusal audit rows: %d", n)
	}
	if st := mallory.getCode(t, code.ID); st.State != tglink.StateCancelled {
		t.Fatalf("code after the refusal: %+v", st)
	}
	if p := mallory.confirm(code.ID, nil).problem(t); p.Status != 409 {
		t.Fatalf("attacker's confirmation after the refusal: %+v", p)
	}
	if r := e.tap(victim, ok); r.Text != "" || r.CallbackText == "" {
		t.Fatalf("[Confirm] after the refusal: %+v", r)
	}
	if r := e.start(victim, code.Code); !r.Bounded {
		t.Fatalf("the cancelled code was redeemed again: %+v", r)
	}

	// A second attempt the victim simply ignores: still no link.
	code2 := mallory.createCode(t)
	buttons(t, e.start(victim, code2.Code))
	if p := mallory.confirm(code2.ID, nil).problem(t); p.Code != "precondition_failed" {
		t.Fatalf("attacker's confirmation without the victim's Telegram confirmation: %+v", p)
	}
	if n := e.count(t, `SELECT count(*) FROM telegram_links`); n != 0 {
		t.Fatalf("%d links exist", n)
	}
	if n := e.count(t, `SELECT count(*) FROM role_grants WHERE subject LIKE 'telegram:%'`); n != 0 {
		t.Fatalf("%d link grants exist", n)
	}
}

// SM-TG-001 (plan §3.1 "a leaked code"): an attacker who learns a victim's code can redeem it and confirm in their
// own Telegram account, but can't finish the link: another user's session can't read, cancel or confirm the code
// (404), and the victim's own other session can't confirm it (only the creating session can). The victim's page names
// the attacker's Telegram account, so the victim cancels. The code is single-use: the victim's own /start after the
// attacker's is refused.
func TestTelegramLinkLeakedCodeIsRefused(t *testing.T) {
	t.Parallel()
	e := newLinkEnv(t)
	victim := e.user(t, "vera@example.com", "Vera", []map[string]string{{"role": "operator", "domain": e.space}})
	other := e.user(t, "otto@example.com", "Otto", []map[string]string{{"role": "operator", "domain": e.space}})
	attackerTG := tgPerson(666, "Eve")

	code := victim.createCode(t)
	ok, _ := buttons(t, e.start(attackerTG, code.Code))
	if r := e.tap(attackerTG, ok); !strings.Contains(r.Text, "finish in the browser") || !r.ClearKeyboard {
		t.Fatalf("Telegram confirmation reply %+v", r)
	}
	// Another user can't see or confirm it.
	expect(t, other.ui.do("GET", "/me/telegram/link-codes/"+code.ID, nil), 404)
	expect(t, other.confirm(code.ID, nil), 404)
	expect(t, other.ui.do("POST", "/me/telegram/link-codes/"+code.ID+"/cancel", nil), 404)
	// The victim's other session can't confirm it either.
	second := e.login(t, "vera@example.com", victim.secret)
	if p := second.do("POST", "/me/telegram/link-codes/"+code.ID+"/confirm", nil).problem(t); p.Status != 403 {
		t.Fatalf("confirmation from another session: %+v", p)
	}
	// The page names the attacker's Telegram account; the victim cancels.
	st := victim.getCode(t, code.ID)
	if st.State != tglink.StateTelegramConfirmed || st.Telegram == nil || st.Telegram.ID != 666 || st.Telegram.Name != "Eve" || st.Telegram.Username != "eve" {
		t.Fatalf("page state %+v", st)
	}
	r := victim.ui.do("POST", "/me/telegram/link-codes/"+code.ID+"/cancel", nil)
	expect(t, r, 200)
	if p := victim.confirm(code.ID, nil).problem(t); p.Status != 409 {
		t.Fatalf("confirmation after cancelling: %+v", p)
	}
	if n := e.count(t, `SELECT count(*) FROM telegram_links`); n != 0 {
		t.Fatalf("%d links exist", n)
	}
}

// SM-TG-001: codes are single-use and expire. A code redeems once (a replayed /start, by the same or another
// account, is refused); [Confirm] counts once; the web confirmation succeeds once (a replay is 409). A code not
// redeemed within its lifetime is refused by /start and shown expired; one redeemed but not confirmed in time can't be
// confirmed in Telegram or the web. A new code cancels the unfinished one. Malformed codes are refused without a
// prompt. Only the code's hash is stored.
func TestTelegramLinkCodesAreSingleUseAndExpire(t *testing.T) {
	t.Parallel()
	e := newLinkEnv(t)
	// The shortest lifetime (120 s), so expiry falls inside the 5-minute re-authentication window.
	expect(t, e.root.do("PUT", "/telegram/settings", map[string]any{"link_code_ttl_seconds": 120}), 200)
	if err := e.h.c.TelegramSettings.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	u := e.user(t, "una@example.com", "Una", []map[string]string{{"role": "operator", "domain": e.space}})
	me := tgPerson(4242, "Una")

	code := u.createCode(t)
	if n := e.count(t, `SELECT count(*) FROM telegram_link_codes WHERE instr(code_hash, ?) > 0`, code.Code); n != 0 {
		t.Fatal("the code itself is stored")
	}
	ok, _ := buttons(t, e.start(me, code.Code))
	// The redeeming account's own replay is refused and changes nothing (another account's would cancel the code:
	// TestTelegramLinkReusedCodeWarnsItsOwner).
	if r := e.start(me, code.Code); !r.Bounded || r.Markup != nil {
		t.Fatalf("replayed /start: %+v", r)
	}
	if r := e.tap(me, ok); r.Text == "" {
		t.Fatalf("[Confirm]: %+v", r)
	}
	if r := e.tap(me, ok); r.Text != "" || !r.ClearKeyboard {
		t.Fatalf("replayed [Confirm]: %+v", r)
	}
	expect(t, u.confirm(code.ID, nil), 201)
	if p := u.confirm(code.ID, nil).problem(t); p.Status != 409 {
		t.Fatalf("replayed web confirmation: %+v", p)
	}
	if n := e.count(t, `SELECT count(*) FROM telegram_links WHERE user_id = ?`, u.id); n != 1 {
		t.Fatalf("%d links", n)
	}
	if r := e.start(me, code.Code); !r.Bounded {
		t.Fatalf("/start with a confirmed code: %+v", r)
	}

	// Expiry before /start.
	u2 := e.user(t, "ulf@example.com", "Ulf", []map[string]string{{"role": "operator", "domain": e.space}})
	late := tgPerson(4343, "Ulf")
	c2 := u2.createCode(t)
	e.h.clk.Advance(121 * time.Second)
	if r := e.start(late, c2.Code); !r.Bounded || r.Markup != nil {
		t.Fatalf("/start with an expired code: %+v", r)
	}
	if st := u2.getCode(t, c2.ID); st.State != tglink.StateExpired {
		t.Fatalf("expired code shows %+v", st)
	}
	// Expiry after /start, before the confirmations.
	u2.reauth(t, e.h)
	c3 := u2.createCode(t)
	ok3, _ := buttons(t, e.start(late, c3.Code))
	e.h.clk.Advance(121 * time.Second)
	if r := e.tap(late, ok3); r.Text != "" {
		t.Fatalf("[Confirm] after expiry: %+v", r)
	}
	u2.reauth(t, e.h)
	if p := u2.confirm(c3.ID, nil).problem(t); p.Code != "precondition_failed" {
		t.Fatalf("web confirmation after expiry: %+v", p)
	}
	// Expiry after the Telegram confirmation.
	c4 := u2.createCode(t)
	ok4, _ := buttons(t, e.start(late, c4.Code))
	e.tap(late, ok4)
	e.h.clk.Advance(121 * time.Second)
	u2.reauth(t, e.h)
	if p := u2.confirm(c4.ID, nil).problem(t); p.Code != "precondition_failed" {
		t.Fatalf("web confirmation after expiry: %+v", p)
	}
	// A new code cancels the unfinished one.
	c5 := u2.createCode(t)
	c6 := u2.createCode(t)
	if st := u2.getCode(t, c5.ID); st.State != tglink.StateCancelled {
		t.Fatalf("replaced code shows %+v", st)
	}
	buttons(t, e.start(late, c6.Code))
	if r := e.start(tgPerson(9, "X"), c5.Code); !r.Bounded {
		t.Fatalf("/start with a replaced code: %+v", r)
	}
	// Malformed codes never prompt.
	for _, bad := range []string{"x", strings.Repeat("A", 42), strings.Repeat("!", 43), strings.Repeat("A", 44), c6.ID} {
		if r := e.start(tgPerson(10, "Y"), bad); !r.Bounded || r.Markup != nil {
			t.Fatalf("/start %q: %+v", bad, r)
		}
	}
	if r := e.tap(tgPerson(10, "Y"), "tglink:c:garbage"); r.Text != "" {
		t.Fatalf("garbage callback: %+v", r)
	}
}

// SM-TG-001 (ADR-0083 decision 3): a confirmed link's grants are the user's grants capped by the link policy, written
// as grants of the subject telegram:<link>: the default cap is operator (an admin's space becomes operator, a global
// viewer stays viewer), viewer may always be chosen, admin only when link_allow_admin_cap is on, and operator not when
// the policy's default is viewer. The Telegram principal acts with the cap intersected with the user's grants, and a
// Telegram account with a live link can't redeem another code.
func TestTelegramLinkGrantsAreCappedByThePolicy(t *testing.T) {
	t.Parallel()
	e := newLinkEnv(t)
	type grantRow struct{ role, domain string }
	grantsOf := func(linkID string) map[grantRow]bool {
		t.Helper()
		rows, err := e.h.c.Store.ReadDB().QueryContext(context.Background(), `SELECT role, domain FROM role_grants WHERE subject = ? AND source = 'telegram'`, auth.TelegramSubject(linkID))
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[grantRow]bool{}
		for rows.Next() {
			var g grantRow
			if err := rows.Scan(&g.role, &g.domain); err != nil {
				t.Fatal(err)
			}
			out[g] = true
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	link := func(w *webUser, tg botapi.User, body any) resp {
		t.Helper()
		w.reauth(t, e.h)
		c := w.createCode(t)
		ok, _ := buttons(t, e.start(tg, c.Code))
		e.tap(tg, ok)
		return w.confirm(c.ID, body)
	}

	ada := e.user(t, "ada@example.com", "Ada", []map[string]string{{"role": "admin", "domain": e.space}, {"role": "viewer", "domain": "*"}})
	adaTG := tgPerson(100, "Ada")
	if p := link(ada, adaTG, map[string]string{"max_role": "admin"}).problem(t); p.Status != 403 {
		t.Fatalf("admin cap without the policy: %+v", p)
	}
	r := link(ada, adaTG, nil)
	expect(t, r, 201)
	var l tglink.Link
	r.json(t, &l)
	if l.MaxRole != "operator" || l.State != "active" || l.Telegram.ID != 100 || l.UserID != ada.id || l.ReconfirmDueAt == nil || l.ViaSSO {
		t.Fatalf("link %+v", l)
	}
	if !strings.HasPrefix(l.ID, "tgl_") {
		t.Fatalf("link id %q", l.ID)
	}
	want := map[grantRow]bool{{"operator", e.space}: true, {"viewer", "*"}: true}
	if got := grantsOf(l.ID); len(got) != len(want) || !got[grantRow{"operator", e.space}] || !got[grantRow{"viewer", "*"}] {
		t.Fatalf("link grants %v, want %v", got, want)
	}
	if due := l.ReconfirmDueAt.Sub(l.ConfirmedAt); due != 90*24*time.Hour {
		t.Fatalf("reconfirm due after %s", due)
	}
	p, err := e.h.c.Auth.TelegramPrincipal(context.Background(), l.ID, auth.TelegramOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !e.h.c.Authz.AllowedAll(p.Subjects(), e.space, authz.InboxWrite) || e.h.c.Authz.AllowedAll(p.Subjects(), e.space, authz.TerminalTakeover) {
		t.Fatal("the link principal isn't capped at operator in the space")
	}
	// One live link per Telegram account: it can't redeem another code (which stays unused).
	bob := e.user(t, "bob@example.com", "Bob", []map[string]string{{"role": "operator", "domain": e.space}})
	bc := bob.createCode(t)
	if r := e.start(adaTG, bc.Code); !r.Bounded || !strings.Contains(r.Text, "already linked") {
		t.Fatalf("/start from a linked account: %+v", r)
	}
	if st := bob.getCode(t, bc.ID); st.State != tglink.StateNew {
		t.Fatalf("the code was used by a linked account: %+v", st)
	}
	// One link per user by default.
	if p := link(ada, tgPerson(101, "Ada2"), nil).problem(t); p.Status != 409 {
		t.Fatalf("a second link over links_per_user: %+v", p)
	}

	// The policy: default viewer, admin cap allowed.
	expect(t, e.root.do("PUT", "/telegram/settings", map[string]any{"link_default_max_role": "viewer", "link_allow_admin_cap": true, "links_per_user": 3}), 200)
	if err := e.h.c.TelegramSettings.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p := link(ada, tgPerson(102, "Ada3"), map[string]string{"max_role": "operator"}).problem(t); p.Status != 403 {
		t.Fatalf("operator over a viewer default: %+v", p)
	}
	r = link(ada, tgPerson(103, "Ada4"), nil)
	expect(t, r, 201)
	r.json(t, &l)
	if got := grantsOf(l.ID); l.MaxRole != "viewer" || len(got) != 2 || !got[grantRow{"viewer", e.space}] || !got[grantRow{"viewer", "*"}] {
		t.Fatalf("viewer default: %s %v", l.MaxRole, got)
	}
	r = link(ada, tgPerson(104, "Ada5"), map[string]string{"max_role": "admin"})
	expect(t, r, 201)
	r.json(t, &l)
	if got := grantsOf(l.ID); l.MaxRole != "admin" || len(got) != 2 || !got[grantRow{"admin", e.space}] || !got[grantRow{"viewer", "*"}] {
		t.Fatalf("admin cap: %s %v", l.MaxRole, got)
	}
	expect(t, ada.confirm("x", map[string]string{"max_role": "superuser"}), 422)
}

// SM-TG-001: linking needs a person's own browser session. An API token can't create or confirm a code (tokens skip
// re-authentication, which the protocol relies on), a session that signed in longer ago than the re-authentication
// window must re-authenticate first, and a code isn't issued while no bot is running.
func TestTelegramLinkNeedsAFreshBrowserSession(t *testing.T) {
	t.Parallel()
	e := newLinkEnv(t)
	for _, op := range []string{"create-telegram-link-code", "confirm-telegram-link"} {
		if r := api.TelegramLinkRules[op]; !r.Reauth || !r.UserOnly || !r.Session || r.TelegramSelf {
			t.Fatalf("%s rule %+v", op, r)
		}
	}
	tok := e.h.client()
	tok.bearer = userToken(t, e.h.c.Auth, "tina@example.com", []auth.GrantInput{{Role: authz.RoleOperator, Domain: e.space}})
	if p := tok.do("POST", "/me/telegram/link-codes", nil).problem(t); p.Status != 403 {
		t.Fatalf("an API token created a code: %+v", p)
	}
	u := e.user(t, "uma@example.com", "Uma", []map[string]string{{"role": "operator", "domain": e.space}})
	c := u.createCode(t)
	if p := tok.do("POST", "/me/telegram/link-codes/"+c.ID+"/confirm", nil).problem(t); p.Status != 403 && p.Status != 404 {
		t.Fatalf("an API token reached a confirmation: %+v", p)
	}
	e.h.clk.Advance(6 * time.Minute)
	if p := u.ui.do("POST", "/me/telegram/link-codes", nil).problem(t); p.Code != "reauth_required" {
		t.Fatalf("an old session created a code: %+v", p)
	}
	if p := u.confirm(c.ID, nil).problem(t); p.Code != "reauth_required" {
		t.Fatalf("an old session confirmed: %+v", p)
	}
	u.reauth(t, e.h)
	(&telegramSetup{h: e.h}).exec(t, `UPDATE telegram_bot SET enabled = 0`)
	if p := u.ui.do("POST", "/me/telegram/link-codes", nil).problem(t); p.Code != "precondition_failed" {
		t.Fatalf("a code without a running bot: %+v", p)
	}
}

// SM-TG-001 through the dispatcher and the fake Bot API: /start <code> from an unlinked user gets the confirmation
// prompt with its two buttons (not the linking instructions); [Confirm] answers the tap, removes the keyboard and
// replies; a refusal (a made-up code) counts against the unlinked-user reply bounds.
func TestTelegramLinkThroughTheDispatcher(t *testing.T) {
	t.Parallel()
	e := newLinkEnv(t)
	u := e.user(t, "ivy@example.com", "Ivy", []map[string]string{{"role": "operator", "domain": e.space}})
	c := u.createCode(t)
	ctx := context.Background()
	d := e.h.c.TelegramUpdates.Dispatcher
	from := tgPerson(5150, "Ivy")
	d.Dispatch(ctx, botapi.Update{UpdateID: 1, Message: &botapi.Message{MessageID: 1, From: &from, Chat: botapi.Chat{ID: from.ID, Type: botapi.ChatPrivate},
		Text: "/start " + c.Code}})
	msgs := e.tg.Messages(from.ID)
	if len(msgs) != 1 || !strings.HasPrefix(msgs[0].Text, "Link this Telegram account to SpaceMan at ") {
		t.Fatalf("messages %+v", msgs)
	}
	tap := botapi.Update{UpdateID: 2, CallbackQuery: &botapi.CallbackQuery{ID: "cb1", From: from, Message: &msgs[0], Data: "tglink:c:" + c.ID}}
	e.tg.PushUpdate(tap) // the fake accepts answers only for taps it issued
	d.Dispatch(ctx, tap)
	if a := e.tg.CallbackAnswers(); len(a) != 1 || a[0].CallbackQueryID != "cb1" || a[0].Text != "Confirmed" {
		t.Fatalf("callback answers %+v", a)
	}
	if n := len(e.tg.Calls("editMessageReplyMarkup")); n != 1 {
		t.Fatalf("keyboard removals: %d", n)
	}
	if msgs = e.tg.Messages(from.ID); len(msgs) != 2 || !strings.Contains(msgs[1].Text, "finish in the browser") {
		t.Fatalf("messages after [Confirm] %+v", msgs)
	}
	if st := u.getCode(t, c.ID); st.State != tglink.StateTelegramConfirmed {
		t.Fatalf("state %+v", st)
	}
	// Made-up codes: one bounded refusal, then silence within the hour.
	stranger := tgPerson(6160, "Stranger")
	for i := range 3 {
		e.h.clk.Advance(2 * time.Second)
		d.Dispatch(ctx, botapi.Update{UpdateID: int64(10 + i), Message: &botapi.Message{MessageID: 1, From: &stranger,
			Chat: botapi.Chat{ID: stranger.ID, Type: botapi.ChatPrivate}, Text: "/start " + strings.Repeat("A", 43)}})
	}
	if msgs := e.tg.Messages(stranger.ID); len(msgs) != 1 {
		t.Fatalf("a stranger guessing codes got %d replies", len(msgs))
	}
}

// extraSpace adds another space on seedSpace's node.
func (e *linkEnv) extraSpace(t *testing.T, slug string) string {
	t.Helper()
	id := ids.New(ids.Space)
	(&telegramSetup{h: e.h}).exec(t, `INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at)
		VALUES (?, ?, ?, 'nod_oidc', 'active', ?, 0, 't', 0)`, id, slug, slug, slug)
	return id
}

// login signs an enrolled user in again: another browser session of the same user.
func (e *linkEnv) login(t *testing.T, email string, secret []byte) *client {
	t.Helper()
	ui := e.h.client()
	r := ui.do("POST", "/auth/login", map[string]string{"email": email, "password": linkPassword})
	expect(t, r, 200)
	var me api.Me
	r.json(t, &me)
	ui.csrf = me.CSRFToken
	e.h.clk.Advance(30 * time.Second)
	r = ui.do("POST", "/auth/mfa/verify", map[string]string{"code": auth.TOTPCode(secret, auth.TOTPStep(e.h.clk.Now()))})
	expect(t, r, 200)
	r.json(t, &me)
	ui.csrf = me.CSRFToken
	return ui
}

// confirmedInTelegram creates a code in w's session and has tg redeem and confirm it in the bot.
func (e *linkEnv) confirmedInTelegram(t *testing.T, w *webUser, tg botapi.User) tglink.Code {
	t.Helper()
	c := w.createCode(t)
	ok, _ := buttons(t, e.start(tg, c.Code))
	if r := e.tap(tg, ok); r.Text == "" {
		t.Fatalf("[Confirm]: %+v", r)
	}
	return c
}

// SM-TG-001 (plan §3.1 step 3): only the browser session that created a code confirms it. Another session of the same
// user is refused (403); a re-authentication rotates the session and carries the code to the new one, so the user can
// still finish; after a logout the old session can't authenticate (401) and a new sign-in can't confirm (403). Codes
// stored before migration 0052 have no creating session and no Telegram confirmation, so neither path confirms them.
func TestTelegramLinkConfirmIsBoundToTheCreatingSession(t *testing.T) {
	t.Parallel()
	e := newLinkEnv(t)
	ann := e.user(t, "ann@example.com", "Ann", []map[string]string{{"role": "operator", "domain": e.space}})
	c := e.confirmedInTelegram(t, ann, tgPerson(301, "Ann"))
	other := e.login(t, "ann@example.com", ann.secret)
	if p := other.do("POST", "/me/telegram/link-codes/"+c.ID+"/confirm", nil).problem(t); p.Status != 403 {
		t.Fatalf("confirmation from another session of the same user: %+v", p)
	}
	ann.reauth(t, e.h) // rotates the session
	expect(t, ann.confirm(c.ID, nil), 201)

	ben := e.user(t, "ben@example.com", "Ben", []map[string]string{{"role": "operator", "domain": e.space}})
	c2 := e.confirmedInTelegram(t, ben, tgPerson(302, "Ben"))
	expect(t, ben.ui.do("POST", "/auth/logout", nil), 200)
	if p := ben.confirm(c2.ID, nil).problem(t); p.Status != 401 {
		t.Fatalf("confirmation from a logged-out session: %+v", p)
	}
	again := e.login(t, "ben@example.com", ben.secret)
	if p := again.do("POST", "/me/telegram/link-codes/"+c2.ID+"/confirm", nil).problem(t); p.Status != 403 {
		t.Fatalf("confirmation from a new sign-in: %+v", p)
	}
	if n := e.count(t, `SELECT count(*) FROM telegram_links WHERE user_id = ?`, ben.id); n != 0 {
		t.Fatalf("%d links for Ben", n)
	}

	// Rows as migration 0052 leaves older codes: no creating session, with and without a Telegram confirmation.
	cy := e.user(t, "cy@example.com", "Cy", []map[string]string{{"role": "operator", "domain": e.space}})
	ts := &telegramSetup{h: e.h}
	for i, tgConfirmed := range []any{nil, e.h.clk.Now().UnixMicro()} {
		hash := sha256.Sum256([]byte(tgRandom(t, 32)))
		ts.exec(t, `INSERT INTO telegram_link_codes (code_hash, user_id, created_session_method, created_at, expires_at, state, redeemed_tg_user_id, redeemed_at)
			VALUES (?, ?, 'password', ?, ?, 'redeemed', ?, ?)`, hash[:], cy.id, e.h.clk.Now().UnixMicro(), e.h.clk.Now().Add(time.Hour).UnixMicro(), 400+i, e.h.clk.Now().UnixMicro())
		if tgConfirmed != nil {
			ts.exec(t, `UPDATE telegram_link_codes SET tg_confirmed_at = ? WHERE code_hash = ?`, tgConfirmed, hash[:])
		}
		if p := cy.confirm(base64.RawURLEncoding.EncodeToString(hash[:]), nil).problem(t); p.Status != 403 {
			t.Fatalf("a pre-0052 code (Telegram-confirmed %v) was confirmable: %+v", tgConfirmed != nil, p)
		}
	}
	if n := e.count(t, `SELECT count(*) FROM telegram_links WHERE user_id = ?`, cy.id); n != 0 {
		t.Fatalf("%d links for Cy", n)
	}
}

// SM-TG-001 (ADR-0083 decision 3): the cap is computed from the user's grants when the link is confirmed, not when
// the code was created: a role lost in between is not in the link (and a grant change through set-user-grants ends
// the creating session, so its code can't be confirmed at all). SSO-mapped grants count only for a link
// confirmed from an SSO session with link_sso_grants on: a password session's link gets the local grants only.
func TestTelegramLinkCapUsesTheGrantsAtConfirmation(t *testing.T) {
	t.Parallel()
	e := newLinkEnv(t)
	other := e.extraSpace(t, "beta")
	expect(t, e.root.do("PUT", "/telegram/settings", map[string]any{"link_sso_grants": true}), 200)
	if err := e.h.c.TelegramSettings.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	// SetGrants keeps at least one enabled global administrator.
	expect(t, e.root.do("POST", "/users", map[string]any{"email": "admin@example.com", "display_name": "Admin", "password": linkPassword,
		"grants": []map[string]string{{"role": "admin", "domain": "*"}}}), 201)
	dee := e.user(t, "dee@example.com", "Dee", []map[string]string{{"role": "operator", "domain": e.space}, {"role": "operator", "domain": other}})
	ts := &telegramSetup{h: e.h}
	ssoSpace := e.extraSpace(t, "gamma")
	ts.exec(t, `INSERT INTO role_grants (subject, role, domain, source, created_at, created_by) VALUES (?, 'operator', ?, 'sso', 0, 't')`,
		auth.SSOSubject(dee.id), ssoSpace)

	c := e.confirmedInTelegram(t, dee, tgPerson(501, "Dee"))
	// Between the code and the confirmation, Dee loses the second space and becomes viewer in the first. This writes
	// the grants directly, as a change that leaves the session alone would: an administrator's set-user-grants ends
	// the user's sessions, which leaves the code unconfirmable (below).
	ts.exec(t, `DELETE FROM role_grants WHERE subject = ? AND source = 'manual'`, auth.UserSubject(dee.id))
	ts.exec(t, `INSERT INTO role_grants (subject, role, domain, source, created_at, created_by) VALUES (?, 'viewer', ?, 'manual', 0, 't')`,
		auth.UserSubject(dee.id), e.space)
	if err := e.h.c.Authz.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	r := dee.confirm(c.ID, nil)
	expect(t, r, 201)
	var l tglink.Link
	r.json(t, &l)
	if l.ViaSSO || l.SSOUntil != nil {
		t.Fatalf("a password session's link carries SSO grants: %+v", l)
	}
	if len(l.Grants) != 1 || l.Grants[0].Role != authz.RoleViewer || l.Grants[0].Domain != e.space {
		t.Fatalf("link grants %+v, want viewer in %s only", l.Grants, e.space)
	}
	if n := e.count(t, `SELECT count(*) FROM role_grants WHERE subject = ? AND domain IN (?, ?)`, auth.TelegramSubject(l.ID), other, ssoSpace); n != 0 {
		t.Fatalf("the link holds %d grants in spaces the user lost or holds only through SSO", n)
	}

	// set-user-grants ends the user's sessions: the code dies with the session that created it.
	eli := e.user(t, "eli@example.com", "Eli", []map[string]string{{"role": "operator", "domain": e.space}})
	c2 := e.confirmedInTelegram(t, eli, tgPerson(502, "Eli"))
	if _, err := e.h.c.Auth.SetGrants(context.Background(), audit.System, eli.id, []auth.GrantInput{{Role: authz.RoleViewer, Domain: e.space}}); err != nil {
		t.Fatal(err)
	}
	if p := eli.confirm(c2.ID, nil).problem(t); p.Status != 401 {
		t.Fatalf("confirmation after a grant change: %+v", p)
	}
	if p := e.login(t, "eli@example.com", eli.secret).do("POST", "/me/telegram/link-codes/"+c2.ID+"/confirm", nil).problem(t); p.Status != 403 {
		t.Fatalf("confirmation from a new sign-in after a grant change: %+v", p)
	}
}

// SM-TG-001 (security review of PR #86): the display name is user-controlled, so it must not be able to look like the
// masked e-mail. The e-mail has its own labelled line outside the name (and the bold entities), an empty e-mail shows
// "(no e-mail)", and the name can't add a line of its own.
func TestTelegramLinkPromptNameCannotImitateTheEmail(t *testing.T) {
	t.Parallel()
	e := newLinkEnv(t)
	vera := e.user(t, "vera@real.example", "Vera (v***@victim.example)", []map[string]string{{"role": "operator", "domain": e.space}})
	prompt := e.start(tgPerson(801, "Target"), vera.createCode(t).Code)
	buttons(t, prompt)
	lines := strings.Split(prompt.Text, "\n")
	if len(lines) < 2 || lines[1] != "E-mail: v***@real.example" || strings.Count(prompt.Text, "E-mail:") != 1 {
		t.Fatalf("the e-mail is not on its own labelled line: %q", prompt.Text)
	}
	if !strings.HasSuffix(lines[0], " as Vera (v***@victim.example)?") || strings.Contains(lines[0], "real.example") {
		t.Fatalf("the name line %q", lines[0])
	}
	// The e-mail is outside both bold entities (host, name).
	emailAt := len(utf16.Encode([]rune(strings.Join(lines[:1], "\n") + "\n")))
	for _, en := range prompt.Entities {
		if en.Offset+en.Length > emailAt-1 {
			t.Fatalf("a bold entity %+v reaches the e-mail line", en)
		}
	}

	// A name with line breaks can't start a line that looks like the e-mail line.
	forged := e.user(t, "mal@real.example", "Mal\nE-mail: v***@victim.example", []map[string]string{{"role": "operator", "domain": e.space}})
	p2 := e.start(tgPerson(802, "Target2"), forged.createCode(t).Code)
	if l2 := strings.Split(p2.Text, "\n"); len(l2) < 2 || l2[1] != "E-mail: m***@real.example" || strings.HasPrefix(l2[0], "E-mail:") || strings.Count(p2.Text, "\nE-mail:") != 1 {
		t.Fatalf("a name with a newline forged the e-mail line: %q", p2.Text)
	}

	// No e-mail on record.
	nomail := e.user(t, "nomail@real.example", "Nomi", []map[string]string{{"role": "operator", "domain": e.space}})
	(&telegramSetup{h: e.h}).exec(t, `UPDATE users SET email = NULL WHERE id = ?`, nomail.id)
	p3 := e.start(tgPerson(803, "Target3"), nomail.createCode(t).Code)
	if !strings.Contains(p3.Text, "\nE-mail: (no e-mail)\n") || strings.Contains(p3.Text, "(***") {
		t.Fatalf("the prompt without an e-mail: %q", p3.Text)
	}
}

// SM-TG-001: a disabled user's code is never redeemed (the disabled-user guard in /start): the reply is the vague
// refusal and the code stays unredeemed, not bound to the Telegram account.
func TestTelegramLinkDisabledUsersCodeIsNotRedeemed(t *testing.T) {
	t.Parallel()
	e := newLinkEnv(t)
	dan := e.user(t, "dan@example.com", "Dan", []map[string]string{{"role": "operator", "domain": e.space}})
	code := dan.createCode(t)
	(&telegramSetup{h: e.h}).exec(t, `UPDATE users SET enabled = 0 WHERE id = ?`, dan.id)
	r := e.start(tgPerson(811, "Dan"), code.Code)
	if !r.Bounded || r.Markup != nil || strings.Contains(r.Text, "Dan") {
		t.Fatalf("a disabled user's code was redeemed: %+v", r)
	}
	if n := e.count(t, `SELECT count(*) FROM telegram_link_codes WHERE user_id = ? AND state = 'new' AND redeemed_tg_user_id IS NULL`, dan.id); n != 1 {
		t.Fatalf("the code was bound to the Telegram account (%d unredeemed codes)", n)
	}
	if n := e.count(t, `SELECT count(*) FROM audit_events WHERE action = 'telegram.link_code.redeemed'`); n != 0 {
		t.Fatalf("%d redemption audit rows", n)
	}
}

// SM-TG-001: two codes of different users redeemed and confirmed in Telegram by the same account (possible while it has
// no link) can't both link it. The first web confirmation links; the second is a 409 from the explicit check, not a 500
// from the unique index.
func TestTelegramLinkSecondConfirmForALinkedTelegramAccountIsConflict(t *testing.T) {
	t.Parallel()
	e := newLinkEnv(t)
	ann := e.user(t, "ann2@example.com", "Ann", []map[string]string{{"role": "operator", "domain": e.space}})
	bea := e.user(t, "bea@example.com", "Bea", []map[string]string{{"role": "operator", "domain": e.space}})
	tg := tgPerson(821, "Shared")
	c1 := e.confirmedInTelegram(t, ann, tg)
	c2 := e.confirmedInTelegram(t, bea, tg)
	expect(t, ann.confirm(c1.ID, nil), 201)
	if p := bea.confirm(c2.ID, nil).problem(t); p.Status != 409 || p.Code != "conflict" {
		t.Fatalf("the second confirmation for a linked Telegram account: %+v", p)
	}
	if n := e.count(t, `SELECT count(*) FROM telegram_links WHERE tg_user_id = ?`, 821); n != 1 {
		t.Fatalf("%d links for the Telegram account", n)
	}
}
