package app_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/api"
	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/eventbus"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
	tglink "github.com/aurimasniekis/go-spaceman/internal/telegram/link"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/updates"
)

// startCache runs the dispatcher's link cache the way Serve does, and returns the dispatcher.
func (e *linkEnv) startCache(t *testing.T) *updates.Dispatcher {
	t.Helper()
	e.h.c.TelegramLinks.Start(t.Context())
	return e.h.c.TelegramUpdates.Dispatcher
}

// link walks a whole link: code, /start, [Confirm] in the bot, the web confirmation. body is the confirmation's body.
func (e *linkEnv) link(t *testing.T, w *webUser, tg botapi.User, body any) tglink.Link {
	t.Helper()
	c := e.confirmedInTelegram(t, w, tg)
	r := w.confirm(c.ID, body)
	expect(t, r, 201)
	var l tglink.Link
	r.json(t, &l)
	return l
}

func (e *linkEnv) linkState(t *testing.T, id string) string {
	t.Helper()
	var st string
	if err := e.h.c.Store.ReadDB().QueryRowContext(context.Background(), `SELECT state FROM telegram_links WHERE id = ?`, id).Scan(&st); err != nil {
		t.Fatal(err)
	}
	return st
}

func (e *linkEnv) cached(d *updates.Dispatcher, tg int64) bool {
	_, ok := e.h.c.TelegramLinks.Lookup(tg)
	return ok
}

func send(from botapi.User, id int64, text string) botapi.Update {
	return botapi.Update{UpdateID: id, Message: &botapi.Message{MessageID: int(id), From: &from, Chat: botapi.Chat{ID: from.ID, Type: botapi.ChatPrivate}, Text: text}}
}

// SM-TG-001 (plan §3.1 "Revocation", TG-4b): a link ends from the web, from the bot (/unlink) and by an administrator,
// and each takes effect on the very next update: the link row is revoked, its grants are gone, the Telegram principal
// is refused, the dispatcher's cache (kept current over the in-process eventbus) forgets it so the user gets the
// linking instructions again, and the end is audited with how it happened. Another user's link isn't theirs to end
// (404), and ending an ended link changes nothing. None of it writes the change feed.
func TestTelegramUnlinkPaths(t *testing.T) {
	t.Parallel()
	e := newLinkEnv(t)
	d := e.startCache(t)
	ctx := context.Background()
	grants := []map[string]string{{"role": "operator", "domain": e.space}}
	changes := e.count(t, `SELECT count(*) FROM changes`)

	ana := e.user(t, "ana@example.com", "Ana", grants)
	ben := e.user(t, "ben@example.com", "Ben", grants)
	cyd := e.user(t, "cyd@example.com", "Cyd", grants)
	anaTG, benTG, cydTG := tgPerson(1001, "Ana"), tgPerson(1002, "Ben"), tgPerson(1003, "Cyd")
	la, lb, lc := e.link(t, ana, anaTG, nil), e.link(t, ben, benTG, nil), e.link(t, cyd, cydTG, nil)
	for _, tg := range []int64{1001, 1002, 1003} {
		eventually(t, "the new link reaches the cache", func() bool { return e.cached(d, tg) })
	}
	if l, ok := e.h.c.TelegramLinks.Lookup(1001); !ok || l.ID != la.ID || l.UserID != ana.id || l.ChatID != 1001 || l.ReconfirmDueAt.IsZero() {
		t.Fatalf("cached link %+v", l)
	}

	// The web: your own link ends; someone else's is not found; ending it twice changes nothing.
	expect(t, ben.ui.do("DELETE", "/me/telegram/links/"+la.ID, nil), 404)
	if e.linkState(t, la.ID) != "active" {
		t.Fatal("another user ended Ana's link")
	}
	expect(t, ana.ui.do("DELETE", "/me/telegram/links/"+la.ID, nil), 204)
	expect(t, ana.ui.do("DELETE", "/me/telegram/links/"+la.ID, nil), 204)
	expect(t, ana.ui.do("DELETE", "/me/telegram/links/tgl_nope", nil), 404)
	if n := e.count(t, `SELECT count(*) FROM audit_events WHERE action = 'telegram.link.revoked' AND target_id = ?`, la.ID); n != 1 {
		t.Fatalf("web unlink audit rows: %d", n)
	}
	eventually(t, "the web unlink reaches the cache", func() bool { return !e.cached(d, 1001) })
	if n := e.count(t, `SELECT count(*) FROM role_grants WHERE subject = ?`, auth.TelegramSubject(la.ID)); n != 0 {
		t.Fatalf("%d grants of the unlinked link remain", n)
	}
	if _, err := e.h.c.Auth.TelegramPrincipal(ctx, la.ID, auth.TelegramOptions{}); err == nil {
		t.Fatal("an unlinked link still builds a principal")
	}
	// The next update is the linking instructions' (the dispatcher forgot the link).
	d.Dispatch(ctx, send(anaTG, 1, "hello"))
	if m := e.tg.Messages(1001); len(m) == 0 || m[len(m)-1].Text != updates.UnlinkedText {
		t.Fatalf("after the web unlink: %+v", m)
	}

	// The bot: /unlink from the linked account ends it.
	d.Dispatch(ctx, send(benTG, 2, "/unlink"))
	if m := e.tg.Messages(1002); len(m) != 1 || !strings.HasPrefix(m[0].Text, "Unlinked.") {
		t.Fatalf("the bot's /unlink reply: %+v", m)
	}
	if e.linkState(t, lb.ID) != "revoked" {
		t.Fatal("/unlink left the link active")
	}
	eventually(t, "the bot unlink reaches the cache", func() bool { return !e.cached(d, 1002) })
	if n := e.count(t, `SELECT count(*) FROM audit_events WHERE action = 'telegram.link.revoked' AND target_id = ? AND json_extract(details_json, '$.path') = 'bot'`, lb.ID); n != 1 {
		t.Fatalf("bot unlink audit rows: %d", n)
	}
	if n := e.count(t, `SELECT count(*) FROM role_grants WHERE subject = ?`, auth.TelegramSubject(lb.ID)); n != 0 {
		t.Fatalf("%d grants of the unlinked link remain", n)
	}

	// An administrator: any link, with a step-up (the root token has one), and the listing shows live links only.
	r := e.root.do("GET", "/telegram/links", nil)
	expect(t, r, 200)
	var live []tglink.Link
	r.json(t, &live)
	if len(live) != 1 || live[0].ID != lc.ID {
		t.Fatalf("live links %+v", live)
	}
	expect(t, ana.ui.do("DELETE", "/telegram/links/"+lc.ID, nil), 403) // user:write is an administrator's
	expect(t, e.root.do("DELETE", "/telegram/links/"+lc.ID, nil), 204)
	expect(t, e.root.do("DELETE", "/telegram/links/tgl_nope", nil), 404)
	eventually(t, "the admin unlink reaches the cache", func() bool { return !e.cached(d, 1003) })
	if e.linkState(t, lc.ID) != "revoked" {
		t.Fatal("the admin unlink left the link active")
	}
	if n := e.count(t, `SELECT count(*) FROM audit_events WHERE action = 'telegram.link.revoked' AND target_id = ? AND json_extract(details_json, '$.path') = 'admin'`, lc.ID); n != 1 {
		t.Fatalf("admin unlink audit rows: %d", n)
	}
	// A revoked link frees the Telegram account and the user's slot: linking again works.
	again := e.link(t, ana, anaTG, nil)
	if again.ID == la.ID {
		t.Fatal("the new link reuses the revoked ID")
	}
	eventually(t, "the new link reaches the cache", func() bool { return e.cached(d, 1001) })

	if n := e.count(t, `SELECT count(*) FROM changes`); n != changes {
		t.Fatalf("linking and unlinking wrote %d change-feed rows", n-changes)
	}
}

// SM-TG-001 (plan §4.2, TG-4b): the dispatcher's cache follows the eventbus topic telegram.link, not the change feed.
// A key re-reads one link, "*" reloads all of them (the bot's token changed to another bot), and the interval reload
// repairs an event the bus dropped. A link revoked straight in the database drops out on the next event.
func TestTelegramLinkCacheFollowsTheBus(t *testing.T) {
	t.Parallel()
	e := newLinkEnv(t)
	d := e.startCache(t)
	u := e.user(t, "cal@example.com", "Cal", []map[string]string{{"role": "operator", "domain": e.space}})
	tg := tgPerson(2001, "Cal")
	l := e.link(t, u, tg, nil)
	eventually(t, "the new link reaches the cache", func() bool { return e.cached(d, 2001) })

	sql := &telegramSetup{h: e.h}
	sql.exec(t, `UPDATE telegram_links SET state = 'revoked', revoked_at = 1, revoked_by = 'test' WHERE id = ?`, l.ID)
	if !e.cached(d, 2001) {
		t.Fatal("the cache changed without an event")
	}
	e.h.c.Bus.Publish(eventbus.Event{Topic: eventbus.TopicTelegramLink, Key: "*"})
	eventually(t, "the wildcard reload drops the revoked link", func() bool { return !e.cached(d, 2001) })

	// A suspended or unknown key never enters the cache.
	sql.exec(t, `UPDATE telegram_links SET state = 'suspended', revoked_at = NULL WHERE id = ?`, l.ID)
	e.h.c.Bus.Publish(eventbus.Event{Topic: eventbus.TopicTelegramLink, Key: l.ID})
	e.h.c.Bus.Publish(eventbus.Event{Topic: eventbus.TopicTelegramLink, Key: "tgl_unknown"})
	time.Sleep(50 * time.Millisecond)
	if e.cached(d, 2001) {
		t.Fatal("a suspended link is cached")
	}
	sql.exec(t, `UPDATE telegram_links SET state = 'active' WHERE id = ?`, l.ID)
	e.h.c.Bus.Publish(eventbus.Event{Topic: eventbus.TopicTelegramLink, Key: l.ID})
	eventually(t, "the single-key reload restores the link", func() bool { return e.cached(d, 2001) })

	// Configuring a different bot revokes every link and publishes "*": the cache empties at once.
	sql.exec(t, `UPDATE telegram_links SET bot_id = 12345 WHERE id = ?`, l.ID)
	sql.exec(t, `UPDATE telegram_links SET state = 'revoked', revoked_at = 1, revoked_by = 'telegram:bot_changed' WHERE bot_id <> 999`)
	e.h.c.Bus.Publish(eventbus.Event{Topic: eventbus.TopicTelegramLink, Key: "*"})
	eventually(t, "a bot change empties the cache", func() bool { return !e.cached(d, 2001) })
}

// SM-TG-001 (plan §3.1, Q8, TG-4b): a link is valid for link_reconfirm_days and then does nothing until the user
// re-confirms it from the web with a fresh login. Past its time the Telegram principal is refused, the dispatcher
// doesn't hand its updates to anyone and tells the user to re-confirm, and nothing but the web re-confirmation
// revives it: that moves the due time a full interval ahead, is audited and reaches the cache. Another user can't
// re-confirm it, and re-confirming needs a recent re-authentication.
func TestTelegramLinkMustBeReconfirmed(t *testing.T) {
	t.Parallel()
	e := newLinkEnv(t)
	d := e.startCache(t)
	ctx := context.Background()
	expect(t, e.root.do("PUT", "/telegram/settings", map[string]any{"link_reconfirm_days": 7}), 200)
	if err := e.h.c.TelegramSettings.Load(ctx); err != nil {
		t.Fatal(err)
	}
	grants := []map[string]string{{"role": "operator", "domain": e.space}}
	u := e.user(t, "dee@example.com", "Dee", grants)
	other := e.user(t, "eli@example.com", "Eli", grants)
	tg := tgPerson(3001, "Dee")
	l := e.link(t, u, tg, nil)
	start := e.h.clk.Now()
	if l.ReconfirmDueAt == nil || !l.ReconfirmDueAt.Equal(start.Add(7*24*time.Hour).Truncate(time.Microsecond)) && l.ReconfirmDueAt.Sub(start) != 7*24*time.Hour {
		t.Fatalf("reconfirm due %v from %v", l.ReconfirmDueAt, start)
	}
	eventually(t, "the link reaches the cache", func() bool { return e.cached(d, 3001) })
	if _, err := e.h.c.Auth.TelegramPrincipal(ctx, l.ID, auth.TelegramOptions{}); err != nil {
		t.Fatalf("a fresh link: %v", err)
	}

	e.h.clk.Advance(6 * 24 * time.Hour)
	if _, err := e.h.c.Auth.TelegramPrincipal(ctx, l.ID, auth.TelegramOptions{}); err != nil {
		t.Fatalf("a link before its due time: %v", err)
	}
	e.h.clk.Advance(25 * time.Hour) // 7 days and an hour
	if _, err := e.h.c.Auth.TelegramPrincipal(ctx, l.ID, auth.TelegramOptions{}); err == nil {
		t.Fatal("an overdue link still builds a principal")
	}
	d.Dispatch(ctx, send(tg, 1, "/needs"))
	if m := e.tg.Messages(3001); len(m) != 1 || m[0].Text != updates.ReconfirmText {
		t.Fatalf("overdue reply %+v", m)
	}
	r := e.root.do("GET", "/telegram/links", nil)
	expect(t, r, 200)
	var listed []tglink.Link
	r.json(t, &listed)
	if len(listed) != 1 || !listed[0].NeedsReconfirm {
		t.Fatalf("an overdue link isn't flagged: %+v", listed)
	}

	// Re-confirming: a new sign-in (the old session is long gone), a user who owns the link, and a fresh re-authentication.
	ui := e.login(t, "dee@example.com", u.secret)
	fresh := &webUser{id: u.id, ui: ui, secret: u.secret}
	oth := e.login(t, "eli@example.com", other.secret)
	if p := (&webUser{ui: oth}).ui.do("POST", "/me/telegram/links/"+l.ID+"/reconfirm", nil).problem(t); p.Status != 404 {
		t.Fatalf("another user's re-confirmation: %+v", p)
	}
	e.h.clk.Advance(6 * time.Minute) // past the re-authentication window
	if p := fresh.ui.do("POST", "/me/telegram/links/"+l.ID+"/reconfirm", nil).problem(t); p.Status != 401 && p.Status != 403 {
		t.Fatalf("a re-confirmation without a recent re-authentication: %+v", p)
	}
	fresh.reauth(t, e.h)
	r = fresh.ui.do("POST", "/me/telegram/links/"+l.ID+"/reconfirm", nil)
	expect(t, r, 200)
	var re tglink.Link
	r.json(t, &re)
	if re.NeedsReconfirm || re.ReconfirmDueAt == nil || re.ReconfirmDueAt.Sub(e.h.clk.Now()) != 7*24*time.Hour {
		t.Fatalf("re-confirmed link %+v at %v", re, e.h.clk.Now())
	}
	if n := e.count(t, `SELECT count(*) FROM audit_events WHERE action = 'telegram.link.reconfirmed' AND target_id = ?`, l.ID); n != 1 {
		t.Fatalf("re-confirmation audit rows: %d", n)
	}
	eventually(t, "the re-confirmation reaches the cache", func() bool {
		c, ok := e.h.c.TelegramLinks.Lookup(3001)
		return ok && c.ReconfirmDueAt.After(e.h.clk.Now())
	})
	if _, err := e.h.c.Auth.TelegramPrincipal(ctx, l.ID, auth.TelegramOptions{}); err != nil {
		t.Fatalf("a re-confirmed link: %v", err)
	}
	d.Dispatch(ctx, send(tg, 3, "/unlink"))
	if e.linkState(t, l.ID) != "revoked" {
		t.Fatal("a re-confirmed link couldn't be unlinked from the bot")
	}
}

// SM-TG-001 (plan §2.5, TG-4b): the web confirmation saves the browser's time zone as the user's zone only when they
// have none. It never overwrites a chosen zone or touches the other notification preferences, and a zone that isn't
// an IANA zone is ignored without failing the link. The link response says whether it saved.
func TestTelegramLinkConfirmPrefillsTheTimezone(t *testing.T) {
	t.Parallel()
	e := newLinkEnv(t)
	grants := []map[string]string{{"role": "operator", "domain": e.space}}
	zone := func(w *webUser) (string, bool) {
		r := w.ui.do("GET", "/auth/me/notification-preferences", nil)
		expect(t, r, 200)
		var p api.NotificationPrefsOut
		r.json(t, &p)
		return p.Timezone, p.Enabled
	}

	// No preferences row: saved, and the other preferences read as defaults.
	a := e.user(t, "fay@example.com", "Fay", grants)
	l := e.link(t, a, tgPerson(4001, "Fay"), map[string]string{"timezone": "Europe/Vilnius"})
	if !l.TimezoneSaved {
		t.Fatalf("the zone wasn't saved: %+v", l)
	}
	if z, on := zone(a); z != "Europe/Vilnius" || !on {
		t.Fatalf("zone %q enabled %v", z, on)
	}

	// A chosen zone stays, whatever the browser says; other preferences are kept.
	b := e.user(t, "gus@example.com", "Gus", grants)
	r := b.ui.do("PUT", "/auth/me/notification-preferences", map[string]any{"enabled": false, "desktop": false, "new_actions": true, "assigned_to_me": true,
		"escalations": true, "reminders": false, "clarifications": true, "min_priority": "high", "muted_spaces": []string{}, "timezone": "Asia/Tokyo"}, "If-Match", `"0"`)
	expect(t, r, 200)
	l = e.link(t, b, tgPerson(4002, "Gus"), map[string]string{"timezone": "Europe/Vilnius"})
	if l.TimezoneSaved {
		t.Fatal("a chosen zone was overwritten")
	}
	if z, on := zone(b); z != "Asia/Tokyo" || on {
		t.Fatalf("zone %q enabled %v after the confirmation", z, on)
	}

	// A row without a zone gets one and keeps its other fields.
	c := e.user(t, "hal@example.com", "Hal", grants)
	expect(t, c.ui.do("PUT", "/auth/me/notification-preferences", map[string]any{"enabled": false, "desktop": false, "new_actions": true, "assigned_to_me": true,
		"escalations": true, "reminders": false, "clarifications": true, "min_priority": "high", "muted_spaces": []string{}}, "If-Match", `"0"`), 200)
	l = e.link(t, c, tgPerson(4003, "Hal"), map[string]string{"timezone": "America/New_York"})
	if z, on := zone(c); !l.TimezoneSaved || z != "America/New_York" || on {
		t.Fatalf("zone %q enabled %v saved %v", z, on, l.TimezoneSaved)
	}

	// Invalid and empty zones are ignored; the link still confirms.
	for i, tz := range []string{"Mars/Olympus", "../../etc/passwd", "", "Local"} {
		w := e.user(t, "ivo"+string(rune('a'+i))+"@example.com", "Ivo", grants)
		l = e.link(t, w, tgPerson(4010+int64(i), "Ivo"), map[string]string{"timezone": tz})
		if z, _ := zone(w); l.TimezoneSaved || z != "" {
			t.Fatalf("zone %q saved as %q (%v)", tz, z, l.TimezoneSaved)
		}
	}
}

// SM-TG-001 (PR #86 review, leaked code): a /start from a different Telegram account on a code another account already
// redeemed means someone besides the owner has seen the code. The code is cancelled at once, the owner's confirm page
// shows that it leaked (not just "cancelled"), the second sender is told the code was used, the account that redeemed
// it is told its request was cancelled, and the attempt is audited. Confirming the code from the web then fails. The
// account that redeemed it sending it again is not a leak. A code already confirmed, expired or cancelled, or made up,
// gets the vague refusal and no flag.
func TestTelegramLinkReusedCodeWarnsItsOwner(t *testing.T) {
	t.Parallel()
	e := newLinkEnv(t)
	// A second service over the same store, with a Notify that records what the bot would send.
	var notices []string
	svc := tglink.New(tglink.Options{Store: e.h.c.Store, Auth: e.h.c.Auth, Audit: e.h.c.Audit, Settings: e.h.c.TelegramSettings.Current,
		PublicURL: e.h.srv.URL, Clock: e.h.c.Clock, Notify: func(chat int64, text string) { notices = append(notices, text) }})
	start := func(from botapi.User, code string) updates.LinkReply {
		u := send(from, 1, "/start "+code)
		return svc.HandleUnlinked(context.Background(), u, from)
	}
	vera := e.user(t, "vera2@example.com", "Vera", []map[string]string{{"role": "operator", "domain": e.space}})
	eve, victimTG := tgPerson(666, "Eve"), tgPerson(42, "Vera")

	code := vera.createCode(t)
	ok, _ := buttons(t, start(eve, code.Code))
	e.tap(eve, ok) // the attacker redeemed and confirmed in Telegram; the owner's page now names them
	if st := vera.getCode(t, code.ID); st.State != tglink.StateTelegramConfirmed || st.ReuseAttempted {
		t.Fatalf("before the reuse: %+v", st)
	}
	if r := start(eve, code.Code); !r.Bounded || r.Text == "" || vera.getCode(t, code.ID).State != tglink.StateTelegramConfirmed {
		t.Fatalf("the redeeming account's own replay changed something: %+v", r)
	}

	// The owner's own /start arrives after the attacker's: the code leaked.
	r := start(victimTG, code.Code)
	if !r.Bounded || !strings.Contains(r.Text, "so it is cancelled") || r.Markup != nil {
		t.Fatalf("second sender's reply %+v", r)
	}
	st := vera.getCode(t, code.ID)
	if st.State != tglink.StateCancelled || !st.ReuseAttempted || st.Telegram != nil {
		t.Fatalf("the owner's page after the reuse: %+v", st)
	}
	if p := vera.confirm(code.ID, nil).problem(t); p.Status != 409 {
		t.Fatalf("web confirmation of a leaked code: %+v", p)
	}
	if r := e.tap(eve, ok); r.Text != "" {
		t.Fatalf("[Confirm] on a leaked code: %+v", r)
	}
	if n := e.count(t, `SELECT count(*) FROM audit_events WHERE action = 'telegram.link_code.reused' AND outcome = 'denied' AND target_id = ?`, code.ID); n != 1 {
		t.Fatalf("reuse audit rows: %d", n)
	}
	if len(notices) != 1 || !strings.Contains(notices[0], "another Telegram account") {
		t.Fatalf("the redeeming account's notice: %q", notices)
	}
	if n := e.count(t, `SELECT count(*) FROM telegram_links`); n != 0 {
		t.Fatalf("%d links exist", n)
	}

	// A code that was already confirmed (linked): audited, not flagged, nothing else changes.
	vera.reauth(t, e.h)
	c2 := vera.createCode(t)
	ok2, _ := buttons(t, start(victimTG, c2.Code))
	e.tap(victimTG, ok2)
	expect(t, vera.confirm(c2.ID, nil), 201)
	if r := start(eve, c2.Code); !r.Bounded || strings.Contains(r.Text, "cancelled") || !strings.Contains(r.Text, "can't be used again") {
		t.Fatalf("reply to a confirmed code %+v", r)
	}
	if st := vera.getCode(t, c2.ID); st.State != tglink.StateConfirmed || st.ReuseAttempted {
		t.Fatalf("a confirmed code after a reuse: %+v", st)
	}
	// Made-up and cancelled codes: the vague refusal, no audit row.
	before := e.count(t, `SELECT count(*) FROM audit_events WHERE action = 'telegram.link_code.reused'`)
	if r := start(eve, strings.Repeat("B", 43)); !r.Bounded || strings.Contains(r.Text, "so it is cancelled") {
		t.Fatalf("a made-up code: %+v", r)
	}
	if r := start(eve, code.Code); strings.Contains(r.Text, "so it is cancelled") {
		t.Fatalf("a cancelled code: %+v", r)
	}
	if n := e.count(t, `SELECT count(*) FROM audit_events WHERE action = 'telegram.link_code.reused'`); n != before {
		t.Fatalf("%d extra reuse rows", n-before)
	}
}

// SM-TG-001 (TG-4b, review PR #86): unlinking from the web re-checks the link's open streams (a Mini App session ends at
// once): a watch opened for the link's principal is signalled by DELETE /me/telegram/links/{id}.
func TestTelegramUnlinkSignalsTheLinksStreams(t *testing.T) {
	t.Parallel()
	e := newLinkEnv(t)
	u := e.user(t, "dee@example.com", "Dee", []map[string]string{{"role": "operator", "domain": e.space}})
	l := e.link(t, u, tgPerson(3001, "Dee"), nil)
	p, err := e.h.c.Auth.TelegramPrincipal(context.Background(), l.ID, auth.TelegramOptions{})
	if err != nil {
		t.Fatal(err)
	}
	w := e.h.c.Auth.Revocations().Watch(p)
	defer w.Close()
	select {
	case <-w.C:
		t.Fatal("the watch was signalled before anything changed")
	default:
	}
	expect(t, u.ui.do("DELETE", "/me/telegram/links/"+l.ID, nil), 204)
	select {
	case <-w.C:
	case <-time.After(5 * time.Second):
		t.Fatal("unlinking did not signal the link's open streams")
	}
}

// SM-TG-001 (review PR #86): listing and unlinking your own links need your own browser session, as re-confirming does:
// an API token of the very user who owns the link gets 403 and changes nothing, while the browser session still works.
func TestTelegramOwnLinksNeedABrowserSession(t *testing.T) {
	t.Parallel()
	e := newLinkEnv(t)
	grants := []map[string]string{{"role": "operator", "domain": e.space}}
	u := e.user(t, "fay@example.com", "Fay", grants)
	l := e.link(t, u, tgPerson(4001, "Fay"), nil)
	ctx := context.Background()
	user, err := e.h.c.Auth.Get(ctx, u.id)
	if err != nil {
		t.Fatal(err)
	}
	_, secret, err := e.h.c.Auth.CreateAPIToken(ctx, &auth.Principal{User: user}, "own", []auth.GrantInput{{Role: authz.RoleOperator, Domain: e.space}}, 24*time.Hour, false)
	if err != nil {
		t.Fatal(err)
	}
	tok := e.h.client()
	tok.bearer = secret
	if p := tok.do("GET", "/me/telegram/links", nil).problem(t); p.Status != 403 {
		t.Fatalf("an API token listed the links: %+v", p)
	}
	if p := tok.do("DELETE", "/me/telegram/links/"+l.ID, nil).problem(t); p.Status != 403 {
		t.Fatalf("an API token unlinked: %+v", p)
	}
	if e.linkState(t, l.ID) != "active" {
		t.Fatal("an API token ended the link")
	}
	expect(t, u.ui.do("GET", "/me/telegram/links", nil), 200)
	expect(t, u.ui.do("DELETE", "/me/telegram/links/"+l.ID, nil), 204)
}
