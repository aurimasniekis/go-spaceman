package events

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aurimasniekis/go-pulse/pkg/pulse"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/outbox"
	"github.com/aurimasniekis/go-spaceman/internal/store"
)

// otherSpaceDialog is a run dialog event of spc_b with every field a real one carries.
const otherSpaceDialog = `{"space_id":"spc_b","run_id":"run_9","node_id":"nod_secret","kind":"worker","role":"payments-dev","harness":"claude",` +
	`"instance_id":"ins_9","lease_generation":4,"dialog_id":"dlg_9","dialog":"unknown","signal":"screen_prompt","harness_version":"9.9.9",` +
	`"detected_at":"2026-11-21T10:00:00Z"}`

// publishOwnOn publishes an own event (this installation's origin) on subject with data and depth.
func (f *fx) publishOwnOn(subject, id, data, depth string) {
	f.t.Helper()
	opts := []pulse.PublishOption{pulse.WithID(id), pulse.WithHeader(outbox.HeaderOrigin, ownOrigin)}
	if depth != "" {
		opts = append(opts, pulse.WithHeader(outbox.HeaderDepth, depth))
	}
	_, err := f.pub.Publish(f.ctx, subject, dialogType, json.RawMessage(data), opts...)
	must(f.t, err)
}

// subscribeFleet creates a fleet-scoped ingest_own subscription on subject in spc_a and waits for its consumer.
func (f *fx) subscribeFleet(name, subject, delivery string) *Subscription {
	f.t.Helper()
	s, err := f.svc.CreateSubscription(f.ctx, admin, "spc_a", SubscriptionInput{Name: name, ConnectionID: f.conn.ID, Custom: dialogDef(subject),
		IngestOwn: true, IngestOwnScope: ScopeFleet, Delivery: delivery})
	must(f.t, err)
	if s.IngestOwnScope != ScopeFleet || !s.IngestOwn {
		f.t.Fatalf("subscription %s: %+v", name, s)
	}
	eventually(f.t, "subscription running", func() bool { g, _ := f.svc.GetSubscription(f.ctx, "spc_a", s.ID); return g.State == "running" })
	return s
}

// ADR-0106: a space holding the operator's fleet-scope grant, with a fleet-scoped subscription, ingests another
// space's dialog_detected and dialog_cleared (durable and core alike), reduced to the identifying fields; a
// space-scoped subscription of the same space still filters them, and depth 0 still holds.
func TestFleetScopeAdmitsOtherSpaceDialog(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	f.svc.o.Origin = ownOrigin
	g, err := f.svc.GrantFleetScope(f.ctx, admin, "spc_a", "operations triage")
	must(t, err)
	if g.SpaceID != "spc_a" || g.Reason != "operations triage" || g.GrantedBy != admin.ID {
		t.Fatalf("grant %+v", g)
	}
	cleared := outbox.RunDialogClearedName.Subject()
	detected := f.subscribeFleet("fleet-detected", dialogSubject, "")
	clearedSub := f.subscribeFleet("fleet-cleared", cleared, "")
	core := f.subscribeFleet("fleet-core", dialogSubject, DeliveryCore)
	plain := f.subscribeOwn("own-space", dialogSubject, true, "")
	if l, _ := f.svc.ListFleetScopeGrants(f.ctx); len(l) != 1 || l[0].FleetSubscriptions != 3 {
		t.Fatalf("grants %+v", l)
	}

	f.publishOwnOn(dialogSubject, "evt-b-detected", otherSpaceDialog, "")
	f.publishOwnOn(cleared, "evt-b-cleared", strings.Replace(otherSpaceDialog, `}`, `,"cleared_at":"2026-11-21T10:05:00Z"}`, 1), "")
	eventually(t, "admitted", func() bool {
		return f.outcomes("spc_a", detected.ID)[OutcomeAccepted] == 1 && f.outcomes("spc_a", clearedSub.ID)[OutcomeAccepted] == 1 &&
			f.outcomes("spc_a", core.ID)[OutcomeAccepted] == 1 && f.detailsOf(plain.ID)[OutcomeFiltered+": "+OwnOtherSpaceDetail] == 1
	})
	items := f.items("spc_a")
	if len(items) != 3 {
		t.Fatalf("items %d, want 3", len(items))
	}
	for _, it := range items {
		if it.Title != "Dialog dlg_9 on run_9" || it.AutomationDepth != 1 {
			t.Errorf("item %+v", it)
		}
		var data map[string]any
		if err := json.Unmarshal([]byte(it.Body), &data); err != nil {
			t.Fatalf("item body %q: %v", it.Body, err)
		}
		for _, k := range []string{"node_id", "kind", "role", "harness", "instance_id", "lease_generation", "harness_version"} {
			if _, ok := data[k]; ok {
				t.Errorf("fleet item carries %s: %s", k, it.Body)
			}
		}
		for _, k := range []string{"space_id", "run_id", "dialog_id", "dialog", "signal", "detected_at"} {
			if _, ok := data[k]; !ok {
				t.Errorf("fleet item lacks %s: %s", k, it.Body)
			}
		}
		if strings.Contains(it.Body, "nod_secret") || strings.Contains(it.Body, "payments-dev") {
			t.Errorf("fleet item leaks: %s", it.Body)
		}
	}

	// Depth 0 only, with fleet scope too: an other-space dialog event an ingestion led to is filtered.
	f.publishOwnOn(dialogSubject, "evt-b-caused", otherSpaceDialog, "1")
	eventually(t, "caused filtered", func() bool { return f.detailsOf(detected.ID)[OutcomeFiltered+": "+OwnCausedDetail] == 1 })
	// An event without a readable space is still filtered.
	f.publishOwnOn(dialogSubject, "evt-none", `{"run_id":"run_9","dialog_id":"dlg_8"}`, "")
	eventually(t, "no space filtered", func() bool { return f.detailsOf(detected.ID)[OutcomeFiltered+": "+OwnOtherSpaceDetail] == 1 })
	if n := len(f.items("spc_a")); n != 3 {
		t.Fatalf("items %d after filtered events", n)
	}
}

// ADR-0106: a space cannot give itself fleet scope. Without the operator's grant, create and update refuse
// ingest_own_scope fleet (forbidden); another space's grant does not count; fleet needs ingest_own; an unknown scope
// is invalid. A grant is refused for a missing space.
func TestFleetScopeRefusedWithoutGrant(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	in := SubscriptionInput{Name: "fleet", ConnectionID: f.conn.ID, Custom: dialogDef(dialogSubject), IngestOwn: true, IngestOwnScope: ScopeFleet}
	_, err := f.svc.CreateSubscription(f.ctx, admin, "spc_a", in)
	if apperr.CodeOf(err) != apperr.CodeForbidden || !strings.Contains(err.Error(), "holds no fleet-scope grant") {
		t.Fatalf("create without a grant: %v", err)
	}
	// Another space's grant is not this space's.
	_, err = f.svc.GrantFleetScope(f.ctx, admin, "spc_b", "")
	must(t, err)
	if _, err = f.svc.CreateSubscription(f.ctx, admin, "spc_a", in); apperr.CodeOf(err) != apperr.CodeForbidden {
		t.Fatalf("create with another space's grant: %v", err)
	}
	if _, err := f.svc.GrantFleetScope(f.ctx, admin, "spc_missing", ""); apperr.CodeOf(err) != apperr.CodeNotFound {
		t.Fatalf("grant for a missing space: %v", err)
	}
	if _, err := f.svc.GetFleetScopeGrant(f.ctx, "spc_a"); apperr.CodeOf(err) != apperr.CodeNotFound {
		t.Fatalf("get without a grant: %v", err)
	}
	if err := f.svc.RevokeFleetScope(f.ctx, admin, "spc_a"); apperr.CodeOf(err) != apperr.CodeNotFound {
		t.Fatalf("revoke without a grant: %v", err)
	}

	bad := in
	bad.IngestOwn = false
	must(t, grantFleet(f, "spc_a"))
	if _, err := f.svc.CreateSubscription(f.ctx, admin, "spc_a", bad); apperr.CodeOf(err) != apperr.CodeInvalid || !strings.Contains(err.Error(), "needs ingest_own") {
		t.Fatalf("fleet without ingest_own: %v", err)
	}
	bad = in
	bad.IngestOwnScope = "everything"
	if _, err := f.svc.CreateSubscription(f.ctx, admin, "spc_a", bad); apperr.CodeOf(err) != apperr.CodeInvalid {
		t.Fatalf("unknown scope: %v", err)
	}
	must(t, f.svc.RevokeFleetScope(f.ctx, admin, "spc_a"))

	// Update: a space-scoped subscription cannot move to fleet without the grant.
	s, err := f.svc.CreateSubscription(f.ctx, admin, "spc_a", SubscriptionInput{Name: "own", ConnectionID: f.conn.ID, Custom: dialogDef(dialogSubject), IngestOwn: true})
	must(t, err)
	if s.IngestOwnScope != ScopeSpace {
		t.Fatalf("default scope %q", s.IngestOwnScope)
	}
	_, err = f.svc.UpdateSubscription(f.ctx, admin, "spc_a", s.ID, s.Version, SubscriptionUpdate{Enabled: true, IngestOwnScope: ScopeFleet})
	if apperr.CodeOf(err) != apperr.CodeForbidden {
		t.Fatalf("update to fleet without a grant: %v", err)
	}
	if g, _ := f.svc.GetSubscription(f.ctx, "spc_a", s.ID); g.IngestOwnScope != ScopeSpace || g.Version != s.Version {
		t.Fatalf("a refused update changed the subscription: %+v", g)
	}
	if subs, _ := f.svc.ListSubscriptions(f.ctx, "spc_a"); len(subs) != 1 {
		t.Fatalf("refused creates left %d subscriptions", len(subs))
	}
}

// grantFleet gives spaceID the fleet-scope grant.
func grantFleet(f *fx, spaceID string) error {
	_, err := f.svc.GrantFleetScope(f.ctx, admin, spaceID, "test")
	return err
}

// ADR-0106: fleet scope widens nothing but the space. Even a fleet-scoped subscription of a granted space forced past
// the API check onto a wildcard over every SpaceMan event admits only the two dialog subjects: another space's (and
// its own) inbox, task and run lifecycle events stay filtered as own events. The API refuses fleet scope with any
// other subject (ingest_own itself is refused there).
func TestFleetScopeOtherSubjectsRefused(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	f.svc.o.Origin = ownOrigin
	must(t, grantFleet(f, "spc_a"))
	for _, subject := range []string{outbox.InboxItemCreatedName.Subject(), outbox.RunEndedName.Subject(), "evt.spaceman.fleet.run.*"} {
		_, err := f.svc.CreateSubscription(f.ctx, admin, "spc_a", SubscriptionInput{Name: "x", ConnectionID: f.conn.ID, Custom: dialogDef(subject),
			IngestOwn: true, IngestOwnScope: ScopeFleet})
		if apperr.CodeOf(err) != apperr.CodeInvalid || !strings.Contains(err.Error(), "ingest_own is not allowed for subject") {
			t.Errorf("%s: %v", subject, err)
		}
	}

	wide := f.subscribeOwn("everything", "evt.spaceman.>", false, "")
	must(t, f.s.W().TxNamed(f.ctx, "force", func(tx *store.Tx) error {
		_, err := tx.SQL.ExecContext(f.ctx, `UPDATE event_subscriptions SET ingest_own = 1, ingest_own_scope = 'fleet' WHERE id = ?`, wide.ID)
		return err
	}))
	others := []string{outbox.InboxItemCreatedName.Subject(), outbox.TaskCreatedName.Subject(), outbox.RunEndedName.Subject(), "evt.spaceman.fleet.run.dialog_answered"}
	for i, subject := range others {
		f.publishOwnOn(subject, "evt-other-"+string(rune('a'+i)), otherSpaceDialog, "")
	}
	f.publishOwnOn(dialogSubject, "evt-dialog", otherSpaceDialog, "")
	eventually(t, "handled", func() bool {
		return f.outcomes("spc_a", wide.ID)[OutcomeAccepted] == 1 && f.detailsOf(wide.ID)[OutcomeFiltered+": "+OwnOriginDetail] == len(others)
	})
	if n := len(f.items("spc_a")); n != 1 {
		t.Fatalf("items %d, want only the dialog's", n)
	}
}

// ADR-0106: revoking the grant fails closed with the next event. The fleet-scoped subscription keeps its setting but
// filters other spaces' dialog events (its own space's are still admitted, as with scope space); updating it is
// refused while it keeps fleet, and works once it goes back to space. A new grant restores it without a new consumer.
func TestFleetScopeRevocationFailsClosed(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	f.svc.o.Origin = ownOrigin
	must(t, grantFleet(f, "spc_a"))
	sub := f.subscribeFleet("fleet", dialogSubject, "")
	core := f.subscribeFleet("fleet-core", dialogSubject, DeliveryCore)
	f.publishOwnOn(dialogSubject, "evt-b-1", otherSpaceDialog, "")
	eventually(t, "admitted while granted", func() bool {
		return f.outcomes("spc_a", sub.ID)[OutcomeAccepted] == 1 && f.outcomes("spc_a", core.ID)[OutcomeAccepted] == 1
	})

	must(t, f.svc.RevokeFleetScope(f.ctx, admin, "spc_a"))
	f.publishOwnOn(dialogSubject, "evt-b-2", strings.Replace(otherSpaceDialog, "dlg_9", "dlg_10", 1), "")
	eventually(t, "filtered after revocation", func() bool {
		return f.detailsOf(sub.ID)[OutcomeFiltered+": "+OwnFleetUngrantedDetail] == 1 && f.detailsOf(core.ID)[OutcomeFiltered+": "+OwnFleetUngrantedDetail] == 1
	})
	f.publishOwn(dialogSubject, dialogType, "evt-a-1", "") // spc_a's own dialog
	eventually(t, "own space still admitted", func() bool { return f.outcomes("spc_a", sub.ID)[OutcomeAccepted] == 2 })
	g, err := f.svc.GetSubscription(f.ctx, "spc_a", sub.ID)
	must(t, err)
	if g.IngestOwnScope != ScopeFleet {
		t.Fatalf("revocation changed the subscription: %+v", g)
	}

	_, err = f.svc.UpdateSubscription(f.ctx, admin, "spc_a", sub.ID, g.Version, SubscriptionUpdate{Enabled: true, Name: "renamed"})
	if apperr.CodeOf(err) != apperr.CodeForbidden {
		t.Fatalf("update keeping fleet after revocation: %v", err)
	}
	back, err := f.svc.UpdateSubscription(f.ctx, admin, "spc_a", sub.ID, g.Version, SubscriptionUpdate{Enabled: true, IngestOwnScope: ScopeSpace})
	if err != nil || back.IngestOwnScope != ScopeSpace {
		t.Fatalf("back to space: %+v %v", back, err)
	}
	// Turning ingest_own off returns a fleet subscription to space.
	off := false
	cg, err := f.svc.GetSubscription(f.ctx, "spc_a", core.ID)
	must(t, err)
	if u, err := f.svc.UpdateSubscription(f.ctx, admin, "spc_a", core.ID, cg.Version, SubscriptionUpdate{Enabled: true, IngestOwn: &off}); err != nil ||
		u.IngestOwnScope != ScopeSpace || u.IngestOwn {
		t.Fatalf("ingest_own off: %+v %v", u, err)
	}

	// Granted again: the subscription moves back to fleet and admits the next other-space event.
	must(t, grantFleet(f, "spc_a"))
	_, err = f.svc.UpdateSubscription(f.ctx, admin, "spc_a", sub.ID, back.Version, SubscriptionUpdate{Enabled: true, IngestOwnScope: ScopeFleet})
	must(t, err)
	f.publishOwnOn(dialogSubject, "evt-b-3", strings.Replace(otherSpaceDialog, "dlg_9", "dlg_11", 1), "")
	eventually(t, "admitted after a new grant", func() bool { return f.outcomes("spc_a", sub.ID)[OutcomeAccepted] == 3 })
}

// ADR-0106: a template follower keeps ingest_own_scope fleet when a new version moves it.
func TestFleetScopeSurvivesTemplateFollow(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	must(t, grantFleet(f, "spc_a"))
	def := *dialogDef(dialogSubject)
	tm, err := f.svc.CreateTemplate(f.ctx, admin, "run-dialogs", "Run dialogs", "", def)
	must(t, err)
	v1, _, err := f.svc.Publish(f.ctx, admin, tm.Versions[0].ID)
	must(t, err)
	s, err := f.svc.CreateSubscription(f.ctx, admin, "spc_a", SubscriptionInput{Name: "dialogs", ConnectionID: f.conn.ID, TemplateVersionID: v1.ID,
		Follow: "latest", IngestOwn: true, IngestOwnScope: ScopeFleet})
	must(t, err)
	def.Title = "Run {{data.run_id}} in {{data.space_id}} is waiting"
	v2, err := f.svc.SaveDraft(f.ctx, admin, tm.ID, def, "better title")
	must(t, err)
	if _, held, err := f.svc.Publish(f.ctx, admin, v2.ID); err != nil || len(held) != 0 {
		t.Fatalf("publish v2: %v %v", held, err)
	}
	g, err := f.svc.GetSubscription(f.ctx, "spc_a", s.ID)
	must(t, err)
	if g.Template.VersionID != v2.ID || g.IngestOwnScope != ScopeFleet {
		t.Fatalf("follower after the move: %+v", g)
	}
}

func TestFleetData(t *testing.T) {
	t.Parallel()
	out, err := fleetData(json.RawMessage(otherSpaceDialog))
	must(t, err)
	want := `{"detected_at":"2026-11-21T10:00:00Z","dialog":"unknown","dialog_id":"dlg_9","run_id":"run_9","signal":"screen_prompt","space_id":"spc_b"}`
	if string(out) != want {
		t.Fatalf("got %s\nwant %s", out, want)
	}
}

// A repeated grant is audited as a replacement and a first grant is not: the audit flag is read back from the trail.
func TestGrantFleetScopeAuditsReplaced(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	must(t, grantFleet(f, "spc_a"))
	must(t, grantFleet(f, "spc_a"))
	rows, err := f.s.ReadDB().QueryContext(f.ctx, `SELECT details_json FROM audit_events WHERE action = 'event_fleet_scope.granted' AND target_id = 'spc_a' ORDER BY seq`)
	must(t, err)
	defer rows.Close()
	var got []bool
	for rows.Next() {
		var raw string
		must(t, rows.Scan(&raw))
		var d map[string]any
		must(t, json.Unmarshal([]byte(raw), &d))
		got = append(got, d["replaced"] == true)
	}
	must(t, rows.Err())
	if len(got) != 2 || got[0] || !got[1] {
		t.Fatalf("replaced flags %v, want [false true]", got)
	}
}
