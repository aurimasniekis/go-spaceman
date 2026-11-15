package notify_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/notify"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/settings"
)

func zoneOf(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, ok := settings.LoadZone(name)
	if !ok {
		t.Fatalf("zone %q", name)
	}
	return loc
}

// setZones gives link i (1-based) the zone zones[i-1] ("" none).
func (e *env) setZones(t *testing.T, zones ...string) {
	t.Helper()
	ls := e.links.Snapshot()
	out := make([]notify.Link, len(ls))
	copy(out, ls)
	for i, z := range zones {
		if z != "" {
			out[i].Zone = zoneOf(t, z)
		}
	}
	e.links.Set(out)
}

func (e *env) newAction(id string, version int64) {
	e.rd.set(action(id, version, false))
	e.feed.add("spc_1", notify.TypeHumanAction, id, changefeed.Created, version)
	e.ring()
	e.endWindow()
}

func (e *env) update(r notify.Resource, version int64) {
	before := e.feed.reads.Load()
	e.rd.set(r)
	e.feed.add("spc_1", r.Type, r.ID, changefeed.Updated, version)
	e.ring()
	e.endWindow()
	e.settle(before)
}

func inboxItem(id, title string) notify.Resource {
	return notify.Resource{Key: notify.Key{Type: notify.TypeInboxItem, ID: id}, SpaceID: "spc_1", Version: 1, Kind: notify.KindInbox,
		Perm: authz.InboxRead, Label: "Inbox", Title: title, State: "new", Notify: true}
}

// A notification sent during a user's quiet hours has no sound; the same instant is daytime in another zone, and a
// user without a zone never has quiet hours.
func TestQuietHoursAcrossZones(t *testing.T) {
	e := newEnv(t, 3)
	e.setZones(t, "Europe/Vilnius", "America/New_York", "")
	e.clk.Set(time.Date(2026, 11, 14, 21, 30, 0, 0, time.UTC)) // 23:30 in Vilnius, 16:30 in New York
	stop := e.run(nil)
	defer stop()
	e.newAction("hum_1", 1)
	calls := e.waitCalls("sendMessage", 3)
	silent := map[string]string{}
	for _, c := range calls {
		silent[c.Params["chat_id"]] = c.Params["disable_notification"]
	}
	if silent["1001"] != "true" {
		t.Errorf("Vilnius (23:30 local) disable_notification = %q, want true", silent["1001"])
	}
	if silent["1002"] != "" || silent["1003"] != "" {
		t.Errorf("New York %q and no-zone %q must make sound", silent["1002"], silent["1003"])
	}
}

// Quiet hours are judged when the message is sent: a card queued at 22:59 that goes out at 23:00 is silent. Here the
// clock moves across the boundary while the card waits.
func TestQuietHoursAreJudgedAtSendTime(t *testing.T) {
	e := newEnv(t, 1)
	e.setZones(t, "Europe/Vilnius")
	e.clk.Set(time.Date(2026, 11, 14, 20, 59, 0, 0, time.UTC)) // 22:59 local
	stop := e.run(nil)
	defer stop()
	release := e.holdChat(1001)
	e.newAction("hum_1", 1)
	time.Sleep(100 * time.Millisecond)
	e.clk.Set(time.Date(2026, 11, 14, 21, 0, 0, 0, time.UTC)) // 23:00 local: quiet starts
	release()
	c := e.waitCalls("sendMessage", 1)
	if c[len(c)-1].Params["disable_notification"] != "true" {
		t.Error("a card sent after quiet hours began must be silent")
	}
}

// A reminder or an escalation of a pending action whose card exists is a message of its own (an edit makes no
// sound); one that was announced is not announced again; the preferences decide.
func TestReminderAndEscalationAnnounce(t *testing.T) {
	e := newEnv(t, 2)
	off := settings.DeliveryOff
	noEsc := false
	ls := e.links.Snapshot()
	out := append([]notify.Link(nil), ls...)
	out[1].Prefs.Reminders = &off
	out[1].Prefs.Escalations = &noEsc
	e.links.Set(out)
	stop := e.run(nil)
	defer stop()
	e.newAction("hum_1", 1)
	e.waitCalls("sendMessage", 2)

	a := action("hum_1", 1, false)
	a.RemindedAt = e.clk.Now().Add(time.Hour).UnixMicro()
	e.update(a, 1) // a reminder does not change the version
	e.waitCalls("sendMessage", 3)
	time.Sleep(100 * time.Millisecond)
	calls := e.fake.Calls("sendMessage")
	if len(calls) != 3 || !strings.HasPrefix(calls[2].Params["text"], "Reminder: Approval") || calls[2].Params["chat_id"] != "1001" {
		t.Fatalf("calls after a reminder = %+v, want one Reminder for the link that wants it", calls)
	}
	if strings.Contains(calls[2].Params["text"], "Please approve") {
		t.Error("a reminder carries the title, never the body")
	}

	e.update(a, 1) // the same reminder again: announced once
	time.Sleep(100 * time.Millisecond)
	if n := len(e.fake.Calls("sendMessage")); n != 3 {
		t.Fatalf("%d sends, want the reminder only once", n)
	}

	esc := action("hum_1", 2, false)
	esc.RemindedAt, esc.EscalatedAt, esc.Priority = a.RemindedAt, e.clk.Now().Add(2*time.Hour).UnixMicro(), "urgent"
	e.update(esc, 2)
	e.waitCalls("sendMessage", 4)
	time.Sleep(100 * time.Millisecond)
	calls = e.fake.Calls("sendMessage")
	if len(calls) != 4 || !strings.HasPrefix(calls[3].Params["text"], "Escalated: Approval") || calls[3].Params["chat_id"] != "1001" {
		t.Fatalf("calls after an escalation = %+v, want one Escalated for the link that wants it", calls)
	}
	if len(e.fake.Calls("editMessageText")) < 1 {
		t.Error("the escalation also edits the card (priority/version changed)")
	}
}

// An action assigned to the user is notified whatever its priority, when "assigned to me" is on; other users still
// need "new actions" and the minimum priority. Reassigning an action whose card exists announces it.
func TestAssignedToMe(t *testing.T) {
	e := newEnv(t, 2)
	e.pol.set.NotificationDefaults.NewActions = false
	stop := e.run(nil)
	defer stop()
	a := action("hum_1", 1, false)
	a.Priority, a.Assignee = "low", "usr_1"
	e.rd.set(a)
	e.feed.add("spc_1", notify.TypeHumanAction, "hum_1", changefeed.Created, 1)
	e.ring()
	e.endWindow()
	e.waitCalls("sendMessage", 1)
	time.Sleep(100 * time.Millisecond)
	if c := e.fake.Calls("sendMessage"); len(c) != 1 || c[0].Params["chat_id"] != "1001" {
		t.Fatalf("sends = %+v, want one, to the assignee", c)
	}
	// The assignee turns it off: the next assignment is silent for them.
	off := false
	ls := append([]notify.Link(nil), e.links.Snapshot()...)
	ls[0].Prefs.AssignedToMe = &off
	e.links.Set(ls)
	b := action("hum_2", 1, false)
	b.Priority, b.Assignee = "low", "usr_1"
	e.rd.set(b)
	e.feed.add("spc_1", notify.TypeHumanAction, "hum_2", changefeed.Created, 1)
	e.ring()
	e.endWindow()
	time.Sleep(150 * time.Millisecond)
	if n := len(e.fake.Calls("sendMessage")); n != 1 {
		t.Errorf("%d sends, want none for an assignment the user turned off", n)
	}
}

func TestReassignedAnnounces(t *testing.T) {
	e := newEnv(t, 2)
	stop := e.run(nil)
	defer stop()
	e.newAction("hum_1", 1) // normal priority: both links get the card
	e.waitCalls("sendMessage", 2)
	a := action("hum_1", 2, false)
	a.Assignee = "usr_2"
	e.update(a, 2)
	e.waitCalls("sendMessage", 3)
	time.Sleep(100 * time.Millisecond)
	c := e.fake.Calls("sendMessage")
	if len(c) != 3 || c[2].Params["chat_id"] != "1002" || !strings.HasPrefix(c[2].Params["text"], "Assigned to you") {
		t.Fatalf("sends = %+v, want one Assigned to you for the new assignee", c)
	}
}

// Expiry finalizes the action: the card is edited to its final state and nothing else is announced.
func TestExpiryEditsTheCard(t *testing.T) {
	e := newEnv(t, 1)
	stop := e.run(nil)
	defer stop()
	e.newAction("hum_1", 1)
	e.waitCalls("sendMessage", 1)
	r := action("hum_1", 2, true)
	r.State = "expired"
	e.update(r, 2)
	e.waitCalls("editMessageText", 1)
	m := e.fake.Messages(1001)
	if len(m) != 1 || !strings.Contains(m[0].Text, "expired") || len(e.fake.Calls("sendMessage")) != 1 {
		t.Fatalf("messages = %+v", m)
	}
}

// The digest: inbox items and tasks (digest by default) are not pushed; each user's digest is due at DigestTime in
// their own zone; Due only enqueues; an empty digest sends nothing.
func TestDigestInEachUsersZone(t *testing.T) {
	e := newEnv(t, 3)
	e.setZones(t, "Europe/Vilnius", "America/New_York", "")
	e.clk.Set(time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)) // 14:00 Vilnius, 07:00 New York
	stop := e.run(nil)
	defer stop()
	ctx := context.Background()

	// First sight of the links: the next digest is the earliest of each zone's next 09:00 (Vilnius tomorrow 07:00Z,
	// New York today 14:00Z); the zoneless user has none.
	next, _ := e.n.NextDigest(ctx)
	if want := time.Date(2026, 11, 14, 14, 0, 0, 0, time.UTC); !next.Equal(want) {
		t.Fatalf("next digest %v, want %v (09:00 in New York)", next, want)
	}

	before := e.feed.reads.Load()
	e.rd.set(inboxItem("inb_1", "Review the report"))
	e.feed.add("spc_1", notify.TypeInboxItem, "inb_1", changefeed.Created, 1)
	e.ring()
	e.endWindow()
	e.settle(before)
	time.Sleep(50 * time.Millisecond)
	if n := len(e.fake.Calls("sendMessage")); n != 0 {
		t.Fatalf("%d sends: an inbox item is digest-delivered by default", n)
	}

	// Nothing is due yet; a Due before the time makes no message.
	if n, err := e.n.DigestDue(ctx); n != 0 || err != nil {
		t.Fatalf("Due before any digest time = %d, %v", n, err)
	}
	e.clk.Set(time.Date(2026, 11, 14, 14, 0, 0, 0, time.UTC)) // 09:00 in New York
	// New York's user gets the digest; Vilnius and the zoneless user do not.
	n, err := e.n.DigestDue(ctx)
	if n != 1 || err != nil {
		t.Fatalf("Due at 09:00 New York = %d, %v, want 1 digest and no error", n, err)
	}
	c := e.waitCalls("sendMessage", 1)
	if c[0].Params["chat_id"] != "1002" || !strings.Contains(c[0].Params["text"], "Review the report") || !strings.HasPrefix(c[0].Params["text"], "Daily digest") {
		t.Fatalf("digest = %+v", c[0])
	}
	if c[0].Params["disable_notification"] != "" {
		t.Error("09:00 is outside quiet hours")
	}
	// The buffer was taken: the next day's digest is empty and sends nothing; the next instant is the day after.
	next, _ = e.n.NextDigest(ctx)
	if want := time.Date(2026, 11, 15, 7, 0, 0, 0, time.UTC); !next.Equal(want) { // Vilnius's 09:00, before New York's next
		t.Errorf("next after the digest = %v, want %v", next, want)
	}
	e.clk.Set(time.Date(2026, 11, 15, 14, 0, 0, 0, time.UTC))
	// Vilnius's own 09:00 (07:00Z) has passed too, and its buffer is full; New York's is empty and sends nothing.
	if n, _ := e.n.DigestDue(ctx); n != 1 {
		t.Errorf("digests enqueued = %d, want Vilnius's only", n)
	}
	e.waitCalls("sendMessage", 2)
	time.Sleep(100 * time.Millisecond)
	for _, c := range e.fake.Calls("sendMessage")[1:] {
		if c.Params["chat_id"] != "1001" {
			t.Errorf("unexpected message to %s: an empty digest sends nothing", c.Params["chat_id"])
		}
	}
}

// Due never fails because of Telegram: a blocked chat is the sender's problem.
func TestDigestDueNeverReturnsTelegramErrors(t *testing.T) {
	e := newEnv(t, 1)
	e.setZones(t, "UTC")
	e.clk.Set(time.Date(2026, 11, 14, 8, 0, 0, 0, time.UTC))
	stop := e.run(nil)
	defer stop()
	ctx := context.Background()
	_, _ = e.n.NextDigest(ctx)
	before := e.feed.reads.Load()
	e.rd.set(inboxItem("inb_1", "x"))
	e.feed.add("spc_1", notify.TypeInboxItem, "inb_1", changefeed.Created, 1)
	e.ring()
	e.endWindow()
	e.settle(before)
	e.fake.Block(1001)
	e.clk.Set(time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC))
	if n, err := e.n.DigestDue(ctx); err != nil || n != 1 {
		t.Fatalf("Due = %d, %v", n, err)
	}
}

// A digest respects visibility as it is when it is sent: a space hidden or muted meanwhile drops its items, and a
// resource that finished is not listed.
func TestDigestRespectsVisibility(t *testing.T) {
	e := newEnv(t, 1)
	e.setZones(t, "UTC")
	e.clk.Set(time.Date(2026, 11, 14, 8, 0, 0, 0, time.UTC))
	stop := e.run(nil)
	defer stop()
	ctx := context.Background()
	_, _ = e.n.NextDigest(ctx)
	add := func(id, title string) {
		before := e.feed.reads.Load()
		e.rd.set(inboxItem(id, title))
		e.feed.add("spc_1", notify.TypeInboxItem, id, changefeed.Created, 1)
		e.ring()
		e.endWindow()
		e.settle(before)
	}
	add("inb_1", "Keep me")
	add("inb_2", "Done soon")
	done := inboxItem("inb_2", "Done soon")
	done.State, done.Notify, done.Final = "done", false, true
	e.update(done, 2)
	e.clk.Set(time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC))
	if n, _ := e.n.DigestDue(ctx); n != 1 {
		t.Fatalf("digest not enqueued")
	}
	c := e.waitCalls("sendMessage", 1)
	if !strings.Contains(c[0].Params["text"], "Keep me") || strings.Contains(c[0].Params["text"], "Done soon") {
		t.Errorf("digest text %q: want the open item only", c[0].Params["text"])
	}

	// Hidden between buffering and sending: the digest is enqueued but nothing is sent for the space.
	add("inb_3", "Secret")
	e.pol.hide("spc_1")
	e.clk.Set(time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC))
	_, _ = e.n.DigestDue(ctx)
	time.Sleep(200 * time.Millisecond)
	if n := len(e.fake.Calls("sendMessage")); n != 1 {
		t.Errorf("%d sends: a hidden space's items must not be sent", n)
	}
}

// An item that waited more than 48 hours for the digest is left out of it.
func TestDigestLeavesOutOldItems(t *testing.T) {
	e := newEnv(t, 1)
	e.setZones(t, "UTC")
	e.clk.Set(time.Date(2026, 11, 14, 8, 0, 0, 0, time.UTC))
	stop := e.run(nil)
	defer stop()
	ctx := context.Background()
	_, _ = e.n.NextDigest(ctx)
	add := func(id, title string) {
		before := e.feed.reads.Load()
		e.rd.set(inboxItem(id, title))
		e.feed.add("spc_1", notify.TypeInboxItem, id, changefeed.Created, 1)
		e.ring()
		e.endWindow()
		e.settle(before)
	}
	add("inb_1", "Stale item")
	e.clk.Set(time.Date(2026, 11, 14, 8, 0, 0, 0, time.UTC).Add(47 * time.Hour))
	add("inb_2", "Fresh item")
	e.clk.Set(time.Date(2026, 11, 14, 8, 0, 0, 0, time.UTC).Add(49 * time.Hour)) // 49 h after the first, 2 h after the second
	if n, _ := e.n.DigestDue(ctx); n != 1 {
		t.Fatalf("digest not enqueued")
	}
	c := e.waitCalls("sendMessage", 1)
	if strings.Contains(c[0].Params["text"], "Stale item") || !strings.Contains(c[0].Params["text"], "Fresh item") {
		t.Errorf("digest text %q: want the fresh item only", c[0].Params["text"])
	}
}

// The final update of an action edits the card and announces nothing, even when it carries a newer reminder or
// escalation time.
func TestFinalActionAnnouncesNothing(t *testing.T) {
	for _, field := range []string{"reminded", "escalated"} {
		t.Run(field, func(t *testing.T) {
			e := newEnv(t, 1)
			stop := e.run(nil)
			defer stop()
			e.newAction("hum_1", 1)
			e.waitCalls("sendMessage", 1)
			time.Sleep(100 * time.Millisecond)
			r := action("hum_1", 2, true)
			if field == "reminded" {
				r.RemindedAt = e.clk.Now().Add(time.Hour).UnixMicro()
			} else {
				r.EscalatedAt = e.clk.Now().Add(time.Hour).UnixMicro()
			}
			e.update(r, 2)
			e.waitCalls("editMessageText", 1)
			time.Sleep(100 * time.Millisecond)
			if n := len(e.fake.Calls("sendMessage")); n != 1 {
				t.Errorf("%d sends, want the card only: a final action announces nothing", n)
			}
		})
	}
}

// A reminder, an escalation or an assignment alert that waited in the sender is dropped when the access ended
// meanwhile (the alert's send-time check, like a card's).
func TestAlertSendTimeRecheck(t *testing.T) {
	alerts := map[string]func(e *env, a *notify.Resource){
		"reminder":   func(e *env, a *notify.Resource) { a.RemindedAt = e.clk.Now().Add(time.Hour).UnixMicro() },
		"escalation": func(e *env, a *notify.Resource) { a.EscalatedAt = e.clk.Now().Add(time.Hour).UnixMicro() },
		"assignment": func(e *env, a *notify.Resource) { a.Assignee = "usr_1" },
	}
	revokes := map[string]func(e *env){
		"grants revoked": func(e *env) {
			e.az.mu.Lock()
			delete(e.az.space["telegram:tgl_1"], "spc_1")
			e.az.mu.Unlock()
		},
		"space hidden": func(e *env) { e.pol.hide("spc_1") },
	}
	for an, change := range alerts {
		for rn, revoke := range revokes {
			t.Run(an+"/"+rn, func(t *testing.T) {
				e := newEnv(t, 1)
				stop := e.run(nil)
				defer stop()
				e.newAction("hum_1", 1)
				e.waitCalls("sendMessage", 1)
				time.Sleep(100 * time.Millisecond)
				release := e.holdChat(1001)
				a := action("hum_1", 1, false)
				change(e, &a)
				e.update(a, 1) // the alert is queued behind the held slot
				revoke(e)
				release()
				time.Sleep(300 * time.Millisecond)
				if n := len(e.fake.Calls("sendMessage")); n != 1 {
					t.Errorf("%d sends, want the card only: the alert must not go out after the access ended", n)
				}
			})
		}
	}
}
