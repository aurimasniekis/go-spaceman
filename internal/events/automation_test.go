package events

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-pulse/pkg/pulse"

	"github.com/aurimasniekis/go-spaceman/internal/outbox"
	"github.com/aurimasniekis/go-spaceman/internal/store"
)

func TestParseDepth(t *testing.T) {
	for in, want := range map[string]int64{"": 0, "0": 0, "7": 7, "0012": 12, "1000000": 1_000_000} {
		if got, err := parseDepth(in); err != nil || got != want {
			t.Errorf("parseDepth(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"-1", "x", "1.5", " 3", "3 ", "+3", "1000001", "99999999999999999999", "0x10"} {
		if _, err := parseDepth(in); err == nil {
			t.Errorf("parseDepth(%q) accepted", in)
		}
	}
}

func (f *fx) publishDepth(depth, id string) {
	f.t.Helper()
	opts := []pulse.PublishOption{pulse.WithID(id)}
	if depth != "" {
		opts = append(opts, pulse.WithHeader(outbox.HeaderDepth, depth))
	}
	_, err := f.pub.Publish(f.ctx, issueSubject, issueType, issuePayload(f.t, ""), opts...)
	must(f.t, err)
}

// SM-EVT-016, ADR-0049: an inbound event's spaceman-depth plus one is its item's automation depth, published with the
// item's own event; an event at or over the space's limit, or with an unreadable depth, is quarantined with the
// reason; reprocessing it is an operator's decision and passes the depth on.
func TestAutomationDepthIsBounded(t *testing.T) {
	f := newFx(t)
	ob := outbox.New(f.s, outbox.Options{Enabled: true, Source: "spaceman-test"})
	f.inbox.Outbox = ob
	limit := int64(4)
	f.svc.o.DepthFor = func(context.Context, string) int64 { return limit }
	s := f.subscribe("spc_a", "issues", Cursor{})

	f.publishDepth("", "evt-plain")
	f.publishDepth("2", "evt-two")
	eventually(t, "items", func() bool { return len(f.items("spc_a")) == 2 })
	byEvent := map[string]int64{}
	for _, it := range f.items("spc_a") {
		byEvent[it.Trusted["event_id"].(string)] = it.AutomationDepth
	}
	if byEvent["evt-plain"] != 1 || byEvent["evt-two"] != 3 {
		t.Fatalf("depths %v", byEvent)
	}
	var published int64
	must(t, f.s.ReadDB().QueryRowContext(f.ctx, `SELECT depth FROM event_outbox WHERE causation_id = 'evt-two'`).Scan(&published))
	if published != 3 {
		t.Fatalf("the item event carries depth %d", published)
	}

	f.publishDepth("4", "evt-deep")
	f.publishDepth("lots", "evt-garbled")
	eventually(t, "quarantined", func() bool { q, _ := f.svc.Quarantine(f.ctx, "spc_a", s.ID, "open"); return len(q) == 2 })
	q, _ := f.svc.Quarantine(f.ctx, "spc_a", s.ID, "open")
	byID := map[string]*QuarantineEntry{}
	for _, e := range q {
		byID[e.EventID] = e
	}
	deep, garbled := byID["evt-deep"], byID["evt-garbled"]
	if deep == nil || deep.Depth != 4 || !strings.Contains(deep.Error, "automation depth") || !strings.Contains(deep.Error, "limit of 4") {
		t.Fatalf("deep %+v", deep)
	}
	if garbled == nil || !strings.Contains(garbled.Error, "spaceman-depth") {
		t.Fatalf("garbled %+v", garbled)
	}
	if len(f.items("spc_a")) != 2 {
		t.Fatal("an over-deep event became an item")
	}
	time.Sleep(time.Second) // quarantined events are terminated, never redelivered
	if n := f.outcomes("spc_a", s.ID)[OutcomeQuarantined]; n != 2 {
		t.Fatalf("quarantined deliveries %d", n)
	}

	// Reprocessing the deep one ingests it one level deeper; raising the limit is not needed.
	r, err := f.svc.ResolveQuarantine(f.ctx, admin, "spc_a", s.ID, deep.ID, true)
	if err != nil || r.Status != "reprocessed" {
		t.Fatalf("reprocess %+v %v", r, err)
	}
	found := false
	for _, it := range f.items("spc_a") {
		if it.Trusted["event_id"] == "evt-deep" {
			found = it.AutomationDepth == 5
		}
	}
	if !found {
		t.Fatal("the reprocessed item does not have depth 5")
	}
}

func (f *fx) setSpaceState(space, state string) {
	f.t.Helper()
	must(f.t, f.s.W().TxNamed(f.ctx, "state", func(tx *store.Tx) error {
		_, err := tx.SQL.ExecContext(f.ctx, `UPDATE spaces SET state = ? WHERE id = ?`, state, space)
		return err
	}))
	f.svc.SpaceStateChanged()
}

// SM-EVT-014, ADR-0049: a paused space's defer subscription marks its items' notices deferred (an active space's
// items are not), a wake subscription does not, and a stop subscription stops while the space is paused and catches up
// on the events that waited in the stream once it resumes.
func TestPausedSpacePolicies(t *testing.T) {
	f := newFx(t)
	// spc_b is paused in the fixture.
	deferSub := f.subscribe("spc_b", "deferred", Cursor{})
	wake, err := f.svc.CreateSubscription(f.ctx, admin, "spc_b", SubscriptionInput{Name: "woken", ConnectionID: f.conn.ID, TemplateVersionID: f.issue.Versions[0].ID,
		Vars: map[string]string{"account_key": "example-org", "repository": "example-org/tool"}, WhilePaused: PausedWake})
	must(t, err)
	stop, err := f.svc.CreateSubscription(f.ctx, admin, "spc_a", SubscriptionInput{Name: "stopped", ConnectionID: f.conn.ID, TemplateVersionID: f.issue.Versions[0].ID,
		Vars: map[string]string{"account_key": "example-org", "repository": "example-org/tool"}, WhilePaused: PausedStop})
	must(t, err)
	eventually(t, "running", func() bool {
		w, _ := f.svc.GetSubscription(f.ctx, "spc_b", wake.ID)
		s, _ := f.svc.GetSubscription(f.ctx, "spc_a", stop.ID)
		return w.State == "running" && s.State == "running"
	})
	if _, err := f.svc.CreateSubscription(f.ctx, admin, "spc_a", SubscriptionInput{Name: "bad", ConnectionID: f.conn.ID, TemplateVersionID: f.issue.Versions[0].ID,
		Vars: map[string]string{"account_key": "example-org", "repository": "example-org/tool"}, WhilePaused: "nap"}); err == nil {
		t.Fatal("unknown policy accepted")
	}

	f.setSpaceState("spc_a", "paused")
	eventually(t, "stopped while paused", func() bool { s, _ := f.svc.GetSubscription(f.ctx, "spc_a", stop.ID); return s.State == "paused" })
	f.publish(issueSubject, issueType, issuePayload(t, ""), "evt-paused")
	eventually(t, "items in the paused space", func() bool { return len(f.items("spc_b")) == 2 })
	for _, it := range f.items("spc_b") {
		if want := it.Subscription == deferSub.ID; it.NoticeDeferred != want {
			t.Fatalf("item of %s: notice deferred %v", it.Subscription, it.NoticeDeferred)
		}
	}
	time.Sleep(500 * time.Millisecond)
	if len(f.items("spc_a")) != 0 {
		t.Fatal("a stopped subscription ingested while paused")
	}
	f.setSpaceState("spc_a", "active")
	eventually(t, "caught up after the resume", func() bool { return len(f.items("spc_a")) == 1 })
	if it := f.items("spc_a")[0]; it.NoticeDeferred || it.Trusted["event_id"] != "evt-paused" {
		t.Fatalf("caught-up item %+v", it)
	}
	if s, _ := f.svc.GetSubscription(f.ctx, "spc_a", stop.ID); s.State != "running" || s.WhilePaused != PausedStop {
		t.Fatalf("after resume %+v", s)
	}
}
