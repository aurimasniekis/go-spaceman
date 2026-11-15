package events

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/events/template"
)

func (f *fx) deliveries(space, sub string) []*Delivery {
	d, err := f.svc.Deliveries(f.ctx, space, sub, 100)
	must(f.t, err)
	return d
}

func (f *fx) countOrigin(space, sub, origin, outcome string) int {
	n := 0
	for _, d := range f.deliveries(space, sub) {
		if d.Origin == origin && (outcome == "" || d.Outcome == outcome) {
			n++
		}
	}
	return n
}

// SM-EVT-015: a replay is an explicit operation with a reason, recorded in the replay history and the audit log; its
// deliveries carry origin replay and the replay's ID, while the stream's own redeliveries are recorded as redelivery
// with their attempt number. Inbox deduplication holds during a replay unless fresh items are asked for.
func TestReplayIsExplicitAndDistinctFromRetry(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	f.publish(issueSubject, issueType, issuePayload(t, ""), "") // before the subscription: history
	s := f.subscribe("spc_a", "issues", Cursor{})
	f.publish(issueSubject, issueType, issuePayload(t, "example-org/tool"), "live-1")
	// The item is committed before its delivery is recorded: wait for both.
	eventually(t, "live item", func() bool { return len(f.items("spc_a")) == 1 && len(f.deliveries("spc_a", s.ID)) == 1 })
	d := f.deliveries("spc_a", s.ID)
	if len(d) != 1 || d[0].Origin != OriginLive || d[0].Attempt != 1 || d[0].ReplayID != "" {
		t.Fatalf("live delivery %+v", d)
	}
	live, err := f.inbox.Get(f.ctx, "spc_a", f.items("spc_a")[0].ID)
	must(t, err)
	if live.Trusted["delivery_origin"] != OriginLive {
		t.Fatalf("live item trusted %v", live.Trusted)
	}
	// The stream delivering the same event again (nak, ack timeout, restart) is a retry: redelivery, attempt 2.
	must(t, f.svc.Process(f.ctx, s.ID, template.Event{ID: d[0].EventID, Subject: issueSubject, Type: issueType, Data: issuePayload(t, "")}, 9, Origin{}))
	if d := f.deliveries("spc_a", s.ID); f.countOrigin("spc_a", s.ID, OriginRedelivery, OutcomeDuplicate) != 1 || len(d) == 0 || d[0].Attempt != 2 {
		t.Fatalf("redelivery %+v", d)
	}

	cur, _ := f.svc.GetSubscription(f.ctx, "spc_a", s.ID)
	// A replay needs a reason, a position and the current version.
	for name, in := range map[string]ReplayInput{"no reason": {Policy: StartAll}, "start now": {Policy: StartNew, Reason: "x"},
		"time missing": {Policy: StartTime, Reason: "x"}} {
		if _, err := f.svc.Replay(f.ctx, admin, "spc_a", s.ID, cur.Version, in); apperr.CodeOf(err) != apperr.CodeInvalid {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if _, err := f.svc.Replay(f.ctx, admin, "spc_a", s.ID, cur.Version+5, ReplayInput{Policy: StartAll, Reason: "x"}); apperr.CodeOf(err) != apperr.CodePrecondition {
		t.Fatalf("stale version: %v", err)
	}
	if _, err := f.svc.Replay(f.ctx, admin, "spc_b", s.ID, cur.Version, ReplayInput{Policy: StartAll, Reason: "x"}); apperr.CodeOf(err) != apperr.CodeNotFound {
		t.Fatalf("other space: %v", err)
	}
	rp, err := f.svc.Replay(f.ctx, admin, "spc_a", s.ID, cur.Version, ReplayInput{Policy: StartAll, Reason: "template fixed; rebuild the inbox"})
	must(t, err)
	if rp.Generation != cur.Cursor.Generation+1 || rp.Reason != "template fixed; rebuild the inbox" || rp.RequestedBy != "user:usr_admin" {
		t.Fatalf("replay %+v", rp)
	}
	// History is replayed: the old event becomes an item marked as replayed; the live one is a duplicate of a replay.
	eventually(t, "replayed deliveries", func() bool { return f.countOrigin("spc_a", s.ID, OriginReplay, "") == 2 })
	if f.countOrigin("spc_a", s.ID, OriginReplay, OutcomeReplayed) != 1 || f.countOrigin("spc_a", s.ID, OriginReplay, OutcomeDuplicate) != 1 || len(f.items("spc_a")) != 2 {
		t.Fatalf("replay outcomes %+v items=%d", f.deliveries("spc_a", s.ID), len(f.items("spc_a")))
	}
	for _, x := range f.deliveries("spc_a", s.ID) {
		if x.Origin == OriginReplay && x.ReplayID != rp.ID {
			t.Fatalf("replay delivery without its replay: %+v", x)
		}
	}
	var replayed *string
	for _, it := range f.items("spc_a") {
		if it.ID != live.ID {
			id := it.ID
			replayed = &id
		}
	}
	it, err := f.inbox.Get(f.ctx, "spc_a", *replayed)
	must(t, err)
	if it.Trusted["delivery_origin"] != OriginReplay || it.Trusted["replay_id"] != rp.ID {
		t.Fatalf("replayed item trusted %v", it.Trusted)
	}
	// The replay is applied with a persisted boundary, and audited.
	eventually(t, "replay applied", func() bool {
		g, _ := f.svc.GetSubscription(f.ctx, "spc_a", s.ID)
		return g.LastReplay != nil && g.LastReplay.Applied
	})
	var n int
	must(t, f.s.ReadDB().QueryRowContext(f.ctx, `SELECT count(*) FROM audit_events WHERE action = 'event_subscription.replayed' AND target_id = ?`, s.ID).Scan(&n))
	if n != 1 {
		t.Fatalf("replay audited %d times", n)
	}

	// Fresh items: each replayed event produces one new item, once per replay.
	cur, _ = f.svc.GetSubscription(f.ctx, "spc_a", s.ID)
	fresh, err := f.svc.Replay(f.ctx, admin, "spc_a", s.ID, cur.Version, ReplayInput{Policy: StartAll, Reason: "re-run triage", FreshItems: true})
	must(t, err)
	eventually(t, "fresh items", func() bool { return len(f.items("spc_a")) == 4 })
	time.Sleep(300 * time.Millisecond)
	if len(f.items("spc_a")) != 4 {
		t.Fatalf("fresh replay made %d items", len(f.items("spc_a")))
	}
	// A cursor change through an update is a replay too, in the same history.
	cur, _ = f.svc.GetSubscription(f.ctx, "spc_a", s.ID)
	_, err = f.svc.UpdateSubscription(f.ctx, admin, "spc_a", s.ID, cur.Version, SubscriptionUpdate{Enabled: true, Start: &Cursor{Policy: StartAll}})
	must(t, err)
	l, err := f.svc.Replays(f.ctx, "spc_a", s.ID, 10)
	must(t, err)
	if len(l) != 3 || l[0].Reason != "cursor changed with the subscription" || !l[1].Superseded || l[1].ID != fresh.ID || !l[1].FreshItems {
		t.Fatalf("history %+v", l)
	}
}

// SM-LIMIT-001: a subscription's ingestion rate holds events back without dropping them; an event that cannot be
// admitted in time is left in the stream for redelivery.
func TestIngestionRateLimit(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	m := NewManager(f.svc, nil)
	f.svc.o.RateFor = func(context.Context, string) int64 { return 6 } // one per 10 s, burst 1
	if err := m.throttle(f.ctx, "sub_x", "spc_a", false); err != nil {
		t.Fatalf("first event: %v", err)
	}
	short, cancel := context.WithTimeout(f.ctx, 100*time.Millisecond)
	defer cancel()
	if err := m.throttle(short, "sub_x", "spc_a", false); !errors.Is(err, errThrottled) {
		t.Fatalf("second event within the burst: %v", err)
	}
	// Another subscription has its own budget, and a raised rate applies at once.
	if err := m.throttle(f.ctx, "sub_y", "spc_a", false); err != nil {
		t.Fatalf("other subscription: %v", err)
	}
	f.svc.o.RateFor = func(context.Context, string) int64 { return 60_000 }
	if err := m.throttle(f.ctx, "sub_x", "spc_a", false); err != nil {
		t.Fatalf("raised rate: %v", err)
	}
	f.svc.o.RateFor = nil
	if err := m.throttle(f.ctx, "sub_x", "spc_a", false); err != nil {
		t.Fatalf("no rate: %v", err)
	}
	// End to end: a generous rate admits every event.
	f.svc.o.RateFor = func(context.Context, string) int64 { return 600 }
	s := f.subscribe("spc_a", "issues", Cursor{})
	for i := range 3 {
		f.publish(issueSubject, issueType, issuePayload(t, ""), "rate-"+string(rune('a'+i)))
	}
	eventually(t, "all admitted", func() bool { return f.outcomes("spc_a", s.ID)[OutcomeAccepted] == 3 })
}
