package events

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-pulse/pkg/pulse"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/events/template"
	"github.com/aurimasniekis/go-spaceman/internal/outbox"
	"github.com/aurimasniekis/go-spaceman/internal/store"
)

const (
	ownOrigin    = "spaceman-test"
	dialogType   = "spaceman.fleet.run.dialog_detected.v1"
	dialogDetail = `{"space_id":"spc_a","run_id":"run_1","dialog_id":"dlg_1","dialog":"trust_folder"}`
)

var dialogSubject = outbox.RunDialogDetectedName.Subject()

func dialogDef(subject string) *template.Definition {
	return &template.Definition{Subject: subject, Title: "Dialog {{data.dialog_id}} on {{data.run_id}}", Body: "{{data | json}}"}
}

// subscribeOwn creates a custom subscription on subject in spc_a and waits for its consumer.
func (f *fx) subscribeOwn(name, subject string, own bool, delivery string) *Subscription {
	f.t.Helper()
	s, err := f.svc.CreateSubscription(f.ctx, admin, "spc_a", SubscriptionInput{Name: name, ConnectionID: f.conn.ID, Custom: dialogDef(subject),
		IngestOwn: own, Delivery: delivery})
	must(f.t, err)
	if s.IngestOwn != own {
		f.t.Fatalf("subscription %s: ingest_own %v, want %v", name, s.IngestOwn, own)
	}
	eventually(f.t, "subscription running", func() bool { g, _ := f.svc.GetSubscription(f.ctx, "spc_a", s.ID); return g.State == "running" })
	return s
}

// publishOwn publishes an event on subject as this installation's outbox would (its spaceman-origin), with depth
// as spaceman-depth when not empty.
func (f *fx) publishOwn(subject, typ, id, depth string) {
	f.t.Helper()
	opts := []pulse.PublishOption{pulse.WithID(id), pulse.WithHeader(outbox.HeaderOrigin, ownOrigin)}
	if depth != "" {
		opts = append(opts, pulse.WithHeader(outbox.HeaderDepth, depth))
	}
	_, err := f.pub.Publish(f.ctx, subject, typ, json.RawMessage(dialogDetail), opts...)
	must(f.t, err)
}

// publishOwnData is publishOwn with an explicit payload.
func (f *fx) publishOwnData(id, data string) {
	f.t.Helper()
	_, err := f.pub.Publish(f.ctx, dialogSubject, dialogType, json.RawMessage(data), pulse.WithID(id), pulse.WithHeader(outbox.HeaderOrigin, ownOrigin))
	must(f.t, err)
}

func (f *fx) detailsOf(sub string) map[string]int {
	f.t.Helper()
	d, err := f.svc.Deliveries(f.ctx, "spc_a", sub, 100)
	must(f.t, err)
	out := map[string]int{}
	for _, x := range d {
		out[x.Outcome+": "+x.Detail]++
	}
	return out
}

// ADR-0104: an own run.dialog_detected becomes an item on a subscription that opts in (ingest_own) and is still
// filtered as an own event on one that does not; the item counts one ingestion (depth 1), which its own event carries.
func TestOwnDialogEventIngestedOnlyWithOptIn(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	f.svc.o.Origin = ownOrigin
	f.inbox.Outbox = outbox.New(f.s, outbox.Options{Enabled: true, Source: ownOrigin})
	opted := f.subscribeOwn("dialogs-own", dialogSubject, true, "")
	plain := f.subscribeOwn("dialogs", dialogSubject, false, "")

	f.publishOwn(dialogSubject, dialogType, "evt-dialog-1", "")
	eventually(t, "both handled", func() bool {
		return f.outcomes("spc_a", opted.ID)[OutcomeAccepted] == 1 && f.outcomes("spc_a", plain.ID)[OutcomeFiltered] == 1
	})
	if d := f.detailsOf(plain.ID); d[OutcomeFiltered+": "+OwnOriginDetail] != 1 {
		t.Fatalf("the subscription without ingest_own: %v", d)
	}
	items := f.items("spc_a")
	if len(items) != 1 || items[0].SourceID != opted.ID || items[0].Title != "Dialog dlg_1 on run_1" || items[0].AutomationDepth != 1 {
		t.Fatalf("items %+v", items)
	}
	var depth int64
	must(t, f.s.ReadDB().QueryRowContext(f.ctx, `SELECT depth FROM event_outbox WHERE causation_id = 'evt-dialog-1'`).Scan(&depth))
	if depth != 1 {
		t.Fatalf("the item's own event carries depth %d, want 1", depth)
	}

	// Turning the opt-in off takes effect for the next event, without a new consumer.
	cur, err := f.svc.GetSubscription(f.ctx, "spc_a", opted.ID)
	must(t, err)
	off := false
	_, err = f.svc.UpdateSubscription(f.ctx, admin, "spc_a", opted.ID, cur.Version, SubscriptionUpdate{Enabled: true, IngestOwn: &off})
	must(t, err)
	f.publishOwn(dialogSubject, dialogType, "evt-dialog-2", "")
	eventually(t, "filtered after opting out", func() bool { return f.outcomes("spc_a", opted.ID)[OutcomeFiltered] == 1 })
	if n := len(f.items("spc_a")); n != 1 {
		t.Fatalf("items after opting out: %d", n)
	}
}

// ADR-0104: an own dialog event is admitted only for the subscription's own space: another space's, one without a
// space_id and an unreadable one are filtered even with ingest_own on, on durable and core subscriptions alike.
func TestIngestOwnScopedToSubscriptionSpace(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	f.svc.o.Origin = ownOrigin
	dur := f.subscribeOwn("dialogs-own", dialogSubject, true, "")
	core := f.subscribeOwn("dialogs-core-own", dialogSubject, true, DeliveryCore)
	src := f.nats.Conn("core-source")
	t.Cleanup(src.Close)
	hdr := func(id string) map[string]string {
		return map[string]string{outbox.HeaderOrigin: ownOrigin, "Nats-Msg-Id": id}
	}

	f.publishOwnData("evt-other", `{"space_id":"spc_b","run_id":"run_9","dialog_id":"dlg_9"}`)
	f.corePublish(src, dialogSubject, []byte(`{"space_id":"spc_b","run_id":"run_9","dialog_id":"dlg_9"}`), hdr("evt-other-core"))
	f.publishOwnData("evt-none", `{"run_id":"run_9","dialog_id":"dlg_9"}`)
	f.publishOwnData("evt-bad", `not json`)
	eventually(t, "others filtered", func() bool {
		// Every event reaches both subscriptions: a core subscription sees stream traffic too.
		return f.detailsOf(dur.ID)[OutcomeFiltered+": "+OwnOtherSpaceDetail] == 4 && f.detailsOf(core.ID)[OutcomeFiltered+": "+OwnOtherSpaceDetail] == 4
	})
	if n := len(f.items("spc_a")); n != 0 {
		t.Fatalf("items %d, want none", n)
	}

	f.publishOwn(dialogSubject, dialogType, "evt-same", "")
	eventually(t, "same space admitted", func() bool { return f.outcomes("spc_a", dur.ID)[OutcomeAccepted] == 1 })
}

// ADR-0104 on core NATS: the same opt-in applies to a core subscription.
func TestOwnDialogEventIngestedOnCore(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	f.svc.o.Origin = ownOrigin
	opted := f.subscribeOwn("dialogs-core-own", dialogSubject, true, DeliveryCore)
	plain := f.subscribeOwn("dialogs-core", dialogSubject, false, DeliveryCore)
	src := f.nats.Conn("core-source")
	t.Cleanup(src.Close)
	f.corePublish(src, dialogSubject, []byte(dialogDetail), map[string]string{outbox.HeaderOrigin: ownOrigin, "Nats-Msg-Id": "evt-core-1"})
	eventually(t, "both handled", func() bool {
		return f.outcomes("spc_a", opted.ID)[OutcomeAccepted] == 1 && f.outcomes("spc_a", plain.ID)[OutcomeFiltered] == 1
	})
	f.corePublish(src, dialogSubject, []byte(dialogDetail), map[string]string{outbox.HeaderOrigin: ownOrigin, outbox.HeaderDepth: "1", "Nats-Msg-Id": "evt-core-2"})
	eventually(t, "caused event filtered", func() bool { return f.detailsOf(opted.ID)[OutcomeFiltered+": "+OwnCausedDetail] == 1 })
	if n := len(f.items("spc_a")); n != 1 {
		t.Fatalf("items %d", n)
	}
}

// ADR-0104: ingest_own is accepted only for an exact subject on the allow-list. Create and update refuse it for
// anything else (inbox, task, wildcards, a template's subject), including an update that moves an opted-in subscription
// to another subject.
func TestIngestOwnRefusedOffTheAllowList(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	for _, subject := range []string{
		outbox.InboxItemCreatedName.Subject(), outbox.TaskCreatedName.Subject(), outbox.TaskStatusChangedName.Subject(),
		outbox.HumanResolvedName.Subject(), outbox.RunEndedName.Subject(), "evt.spaceman.fleet.run.*", "evt.spaceman.>", "evt.spaceman.fleet.run.dialog_answered",
	} {
		_, err := f.svc.CreateSubscription(f.ctx, admin, "spc_a", SubscriptionInput{Name: "x", ConnectionID: f.conn.ID, Custom: dialogDef(subject), IngestOwn: true})
		if apperr.CodeOf(err) != apperr.CodeInvalid || !strings.Contains(err.Error(), "ingest_own is not allowed for subject "+subject) {
			t.Errorf("%s: %v", subject, err)
		}
	}
	_, err := f.svc.CreateSubscription(f.ctx, admin, "spc_a", SubscriptionInput{Name: "issues", ConnectionID: f.conn.ID, TemplateVersionID: f.issue.Versions[0].ID,
		Vars: map[string]string{"account_key": "example-org", "repository": "example-org/tool"}, IngestOwn: true})
	if apperr.CodeOf(err) != apperr.CodeInvalid {
		t.Errorf("template subscription with ingest_own: %v", err)
	}
	if subs, _ := f.svc.ListSubscriptions(f.ctx, "spc_a"); len(subs) != 0 {
		t.Fatalf("refused creates left %d subscriptions", len(subs))
	}

	// Both allow-listed subjects are accepted.
	for _, subject := range OwnIngestSubjects() {
		s, err := f.svc.CreateSubscription(f.ctx, admin, "spc_a", SubscriptionInput{Name: subject, ConnectionID: f.conn.ID, Custom: dialogDef(subject), IngestOwn: true})
		if err != nil || !s.IngestOwn {
			t.Fatalf("%s: %+v %v", subject, s, err)
		}
	}

	// Update: an opted-in subscription cannot move to another subject while it keeps the flag...
	s, err := f.svc.CreateSubscription(f.ctx, admin, "spc_a", SubscriptionInput{Name: "own", ConnectionID: f.conn.ID, Custom: dialogDef(dialogSubject), IngestOwn: true})
	must(t, err)
	inbox := outbox.InboxItemCreatedName.Subject()
	_, err = f.svc.UpdateSubscription(f.ctx, admin, "spc_a", s.ID, s.Version, SubscriptionUpdate{Enabled: true, Custom: dialogDef(inbox), Start: &Cursor{Policy: StartNew}})
	if apperr.CodeOf(err) != apperr.CodeInvalid || !strings.Contains(err.Error(), "ingest_own is not allowed") {
		t.Fatalf("subject change keeping ingest_own: %v", err)
	}
	// ...and can when it gives the flag up in the same update.
	off, on := false, true
	moved, err := f.svc.UpdateSubscription(f.ctx, admin, "spc_a", s.ID, s.Version, SubscriptionUpdate{Enabled: true, Custom: dialogDef(inbox), Start: &Cursor{Policy: StartNew},
		IngestOwn: &off})
	if err != nil || moved.IngestOwn || moved.Subject != inbox {
		t.Fatalf("subject change without ingest_own: %+v %v", moved, err)
	}
	// A subscription off the allow-list cannot opt in.
	_, err = f.svc.UpdateSubscription(f.ctx, admin, "spc_a", s.ID, moved.Version, SubscriptionUpdate{Enabled: true, IngestOwn: &on})
	if apperr.CodeOf(err) != apperr.CodeInvalid {
		t.Fatalf("opt-in on the inbox subject: %v", err)
	}
	if g, _ := f.svc.GetSubscription(f.ctx, "spc_a", s.ID); g.IngestOwn || g.Version != moved.Version {
		t.Fatalf("a refused update changed the subscription: %+v", g)
	}
}

// ADR-0104, SM-EVT-016: no ingestion loop is possible with ingest_own. Even a subscription forced past the API check
// (ingest_own on a wildcard over every SpaceMan event, written straight to the database) ingests an own dialog event
// once: the item's own event (inbox.item.created, depth 1), relayed to Pulse with this installation's origin, is
// filtered, and so is an own dialog event an ingestion led to (depth above 0). event_depth still quarantines a
// foreign event that is too deep.
func TestIngestOwnCannotLoop(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	f.svc.o.Origin = ownOrigin
	f.svc.o.DepthFor = func(_ context.Context, _ string) int64 { return 2 }
	ob := outbox.New(f.s, outbox.Options{Enabled: true, Source: ownOrigin})
	f.inbox.Outbox = ob
	wide := f.subscribeOwn("everything", "evt.spaceman.>", false, "")
	must(t, f.s.W().TxNamed(f.ctx, "force", func(tx *store.Tx) error {
		_, err := tx.SQL.ExecContext(f.ctx, `UPDATE event_subscriptions SET ingest_own = 1 WHERE id = ?`, wide.ID)
		return err
	}))

	var err error
	f.publishOwn(dialogSubject, dialogType, "evt-dialog-1", "")
	eventually(t, "dialog ingested", func() bool { return f.outcomes("spc_a", wide.ID)[OutcomeAccepted] == 1 })

	// Relay the item's event exactly as the outbox would publish it (origin and depth headers).
	spaceman, err := pulse.Connect(f.nats.URL, pulse.WithCreds(f.nats.Creds("spaceman-relay")), pulse.WithSource(ownOrigin))
	must(t, err)
	t.Cleanup(spaceman.Close)
	for range 3 { // a loop would show as more items with every relay round
		if _, _, err := ob.Once(f.ctx, spaceman); err != nil {
			t.Fatal(err)
		}
		time.Sleep(300 * time.Millisecond)
	}
	eventually(t, "item event filtered", func() bool {
		return f.detailsOf(wide.ID)[OutcomeFiltered+": "+OwnOriginDetail] == 1
	})
	var relayed, pending int
	must(t, f.s.ReadDB().QueryRowContext(f.ctx, `SELECT count(*) FROM event_outbox WHERE published_at IS NOT NULL`).Scan(&relayed))
	must(t, f.s.ReadDB().QueryRowContext(f.ctx, `SELECT count(*) FROM event_outbox WHERE published_at IS NULL`).Scan(&pending))
	if relayed != 1 || pending != 0 || len(f.items("spc_a")) != 1 {
		t.Fatalf("relayed %d pending %d items %d", relayed, pending, len(f.items("spc_a")))
	}

	// An own dialog event that an ingestion led to is filtered, at any depth below or over event_depth.
	f.publishOwn(dialogSubject, dialogType, "evt-dialog-caused", "1")
	f.publishOwn(dialogSubject, dialogType, "evt-dialog-deep", "5")
	eventually(t, "caused events filtered", func() bool { return f.detailsOf(wide.ID)[OutcomeFiltered+": "+OwnCausedDetail] == 2 })

	// event_depth still applies on the subscription: another installation's event at the limit is quarantined.
	_, err = f.pub.Publish(f.ctx, dialogSubject, dialogType, json.RawMessage(dialogDetail), pulse.WithID("evt-foreign-deep"),
		pulse.WithHeader(outbox.HeaderOrigin, "spaceman-other"), pulse.WithHeader(outbox.HeaderDepth, "2"))
	must(t, err)
	eventually(t, "foreign deep quarantined", func() bool { return f.outcomes("spc_a", wide.ID)[OutcomeQuarantined] == 1 })
	if n := len(f.items("spc_a")); n != 1 {
		t.Fatalf("items %d, want only the first dialog's", n)
	}
}

// ADR-0104: a subscription that follows a template's latest version keeps ingest_own when a new version moves it.
func TestIngestOwnSurvivesTemplateFollow(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	def := *dialogDef(dialogSubject)
	tm, err := f.svc.CreateTemplate(f.ctx, admin, "run-dialogs", "Run dialogs", "", def)
	must(t, err)
	v1, _, err := f.svc.Publish(f.ctx, admin, tm.Versions[0].ID)
	must(t, err)
	s, err := f.svc.CreateSubscription(f.ctx, admin, "spc_a", SubscriptionInput{Name: "dialogs", ConnectionID: f.conn.ID, TemplateVersionID: v1.ID,
		Follow: "latest", IngestOwn: true})
	must(t, err)
	def.Title = "Run {{data.run_id}} is waiting at a dialog"
	v2, err := f.svc.SaveDraft(f.ctx, admin, tm.ID, def, "better title")
	must(t, err)
	if _, held, err := f.svc.Publish(f.ctx, admin, v2.ID); err != nil || len(held) != 0 {
		t.Fatalf("publish v2: %v %v", held, err)
	}
	g, err := f.svc.GetSubscription(f.ctx, "spc_a", s.ID)
	must(t, err)
	if g.Template.VersionID != v2.ID || !g.IngestOwn {
		t.Fatalf("follower after the move: version %s ingest_own %v", g.Template.VersionID, g.IngestOwn)
	}
}
