package app_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-pulse/pkg/pulse"

	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/config"
	"github.com/aurimasniekis/go-spaceman/internal/events"
	"github.com/aurimasniekis/go-spaceman/internal/events/pulsetest"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/inbox"
	"github.com/aurimasniekis/go-spaceman/internal/outbox"
)

// installation is one central of a multi-installation test: its HTTP root client, a space and a Pulse connection.
type installation struct {
	h     *harness
	root  *client
	space *fleet.Space
	conn  events.Connection
}

func newInstallation(t *testing.T, nats *pulsetest.Server, source string, publish bool, mod func(*config.Config)) *installation {
	t.Helper()
	creds := filepath.Join(t.TempDir(), source+".creds")
	if err := os.WriteFile(creds, nats.Creds(source), 0o600); err != nil {
		t.Fatal(err)
	}
	h := newHarnessWith(t, func(c *config.Config) {
		if publish {
			c.Events.Outbound = config.OutboundConfig{Enabled: true, URL: nats.URL, CredsFile: creds, Source: source, Retention: config.Duration(time.Hour)}
		}
		if mod != nil {
			mod(c)
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{}, 2)
	go func() { events.NewManager(h.c.Events, nil).Run(ctx); done <- struct{}{} }()
	go func() {
		if publish {
			h.c.Outbox.Run(ctx, outbox.PulseDialer(outbox.PulseConfig{URL: nats.URL, CredsFile: creds, Source: source}, nil))
		}
		done <- struct{}{}
	}()
	t.Cleanup(func() { cancel(); <-done; <-done })
	rootSecret, _, _ := h.c.Auth.IssueRootToken(ctx)
	in := &installation{h: h, root: h.client()}
	in.root.bearer = rootSecret
	actor := audit.Actor{Type: audit.ActorUser, ID: "usr_test"}
	en, _ := h.c.Fleet.CreateNode(ctx, actor, fleet.CreateNodeInput{Name: "node-" + source})
	sp, err := h.c.Fleet.CreateSpace(ctx, actor, fleet.CreateSpaceInput{Slug: "space-" + source, Name: "Space " + source, NodeID: en.Node.ID})
	if err != nil {
		t.Fatal(err)
	}
	in.space = sp
	r := in.root.do("POST", "/event-connections", map[string]any{"name": "pulse", "url": nats.URL, "creds": string(nats.Creds(source + "-in")), "enabled": true})
	expect(t, r, 201)
	r.json(t, &in.conn)
	return in
}

// subscribe adds a custom-definition subscription and waits until it runs.
func (in *installation) subscribe(t *testing.T, name, subject string, extra map[string]any) events.Subscription {
	t.Helper()
	body := map[string]any{"name": name, "connection_id": in.conn.ID,
		"custom": map[string]any{"subject": subject, "title": name, "body": "{{data | json}}", "variables": []any{}, "filters": []any{}}}
	for k, v := range extra {
		body[k] = v
	}
	r := in.root.do("POST", "/spaces/"+in.space.ID+"/subscriptions", body)
	expect(t, r, 201)
	var s events.Subscription
	r.json(t, &s)
	in.waitState(t, s.ID, "running")
	return s
}

func (in *installation) waitState(t *testing.T, subID, state string) {
	t.Helper()
	eventually(t, "subscription "+state, func() bool {
		var g events.Subscription
		in.root.do("GET", "/spaces/"+in.space.ID+"/subscriptions/"+subID, nil).json(t, &g)
		return g.State == state
	})
}

func (in *installation) items(t *testing.T) []*inbox.Item {
	t.Helper()
	var l struct{ Items []*inbox.Item }
	in.root.do("GET", "/spaces/"+in.space.ID+"/inbox", nil).json(t, &l)
	return l.Items
}

func (in *installation) quarantine(t *testing.T, subID string) []events.QuarantineEntry {
	t.Helper()
	var l struct{ Items []events.QuarantineEntry }
	in.root.do("GET", "/spaces/"+in.space.ID+"/subscriptions/"+subID+"/quarantine", nil).json(t, &l)
	return l.Items
}

// controlNotices are the notifications for control that name an inbox item.
func (in *installation) controlNotices(t *testing.T, itemID string) int {
	t.Helper()
	l, err := in.h.c.Messages.List(context.Background(), in.space.ID, 500)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, m := range l {
		if m.ToKind == "control" && strings.Contains(m.Body, itemID) {
			n++
		}
	}
	return n
}

func (in *installation) setState(t *testing.T, state, mode string) {
	t.Helper()
	sp, err := in.h.c.Fleet.GetSpace(context.Background(), in.space.ID)
	if err != nil {
		t.Fatal(err)
	}
	body := map[string]any{"state": state}
	if mode != "" {
		body["mode"] = mode
	}
	expect(t, in.root.do("POST", "/spaces/"+in.space.ID+"/state", body, "If-Match", fmt.Sprintf(`"%d"`, sp.Version)), 200)
}

func itemBySubscription(items []*inbox.Item, subID string) *inbox.Item {
	for _, it := range items {
		if it.Subscription == subID {
			return it
		}
	}
	return nil
}

// SM-EVT-014 end to end (ADR-0049): while a space is paused with Hold, a defer subscription keeps ingesting but control
// hears of the item only when the space resumes; a wake subscription tells control at once; a stop subscription stops
// (its durable events wait in the stream) and ingests them after the resume. Delivery on resume uses the notifier's
// ordinary path (a control message), triggered by the resume itself: nothing polls.
func TestPausedSpaceEventPolicies(t *testing.T) {
	nats := pulsetest.Start(t)
	a := newInstallation(t, nats, "spaceman", false, nil)
	pub, err := pulse.Connect(nats.URL, pulse.WithCreds(nats.Creds("monitor")), pulse.WithSource("monitor"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pub.Close)
	ctx := context.Background()
	deferred := a.subscribe(t, "deferred", "evt.ops.defer.fired", nil)
	woken := a.subscribe(t, "woken", "evt.ops.wake.fired", map[string]any{"while_paused": "wake"})
	stopped := a.subscribe(t, "stopped", "evt.ops.stop.fired", map[string]any{"while_paused": "stop"})
	if deferred.WhilePaused != events.PausedDefer || woken.WhilePaused != events.PausedWake || stopped.WhilePaused != events.PausedStop {
		t.Fatalf("policies %q %q %q", deferred.WhilePaused, woken.WhilePaused, stopped.WhilePaused)
	}
	if p := a.root.do("POST", "/spaces/"+a.space.ID+"/subscriptions", map[string]any{"name": "bad", "connection_id": a.conn.ID, "while_paused": "sleep",
		"custom": map[string]any{"subject": "evt.ops.bad", "title": "x", "variables": []any{}, "filters": []any{}}}).problem(t); p.Code != "validation_failed" {
		t.Fatalf("unknown policy accepted: %+v", p)
	}

	a.setState(t, "paused", "hold")
	a.waitState(t, stopped.ID, "paused")
	for _, s := range []string{"evt.ops.defer.fired", "evt.ops.wake.fired", "evt.ops.stop.fired"} {
		if _, err := pub.Publish(ctx, s, "ops.alert.v1", json.RawMessage(`{"name":"db-lag"}`)); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, "paused space ingests", func() bool {
		items := a.items(t)
		return itemBySubscription(items, deferred.ID) != nil && itemBySubscription(items, woken.ID) != nil
	})
	items := a.items(t)
	d, w := itemBySubscription(items, deferred.ID), itemBySubscription(items, woken.ID)
	if !d.NoticeDeferred || w.NoticeDeferred {
		t.Fatalf("deferred %v woken %v", d.NoticeDeferred, w.NoticeDeferred)
	}
	eventually(t, "wake tells control at once", func() bool { return a.controlNotices(t, w.ID) == 1 })
	time.Sleep(300 * time.Millisecond)
	if a.controlNotices(t, d.ID) != 0 || itemBySubscription(a.items(t), stopped.ID) != nil {
		t.Fatalf("while paused: %d notices of the deferred item, stopped item %v", a.controlNotices(t, d.ID), itemBySubscription(a.items(t), stopped.ID))
	}

	a.setState(t, "active", "")
	eventually(t, "deferred notice sent on resume", func() bool { return a.controlNotices(t, d.ID) == 1 })
	var got inbox.Item
	eventually(t, "deferred flag cleared", func() bool {
		a.root.do("GET", "/spaces/"+a.space.ID+"/inbox/"+d.ID, nil).json(t, &got)
		return !got.NoticeDeferred
	})
	if got.Version != d.Version {
		t.Fatalf("a notice is not an edit: version %d → %d", d.Version, got.Version)
	}
	a.waitState(t, stopped.ID, "running")
	eventually(t, "the stopped subscription catches up", func() bool { return itemBySubscription(a.items(t), stopped.ID) != nil })
	if s := itemBySubscription(a.items(t), stopped.ID); s.NoticeDeferred {
		t.Fatalf("ingested after the resume, yet deferred: %+v", s)
	}
	if a.controlNotices(t, w.ID) != 1 {
		t.Fatal("the woken item was announced again")
	}

	// The policy is editable; omitting it leaves it unchanged.
	r := a.root.do("PUT", "/spaces/"+a.space.ID+"/subscriptions/"+deferred.ID, map[string]any{"enabled": true, "while_paused": "stop"}, "If-Match", `"1"`)
	expect(t, r, 200)
	var up events.Subscription
	r.json(t, &up)
	r = a.root.do("PUT", "/spaces/"+a.space.ID+"/subscriptions/"+deferred.ID, map[string]any{"enabled": true}, "If-Match", fmt.Sprintf(`"%d"`, up.Version))
	expect(t, r, 200)
	r.json(t, &up)
	if up.WhilePaused != events.PausedStop {
		t.Fatalf("policy after edits %q", up.WhilePaused)
	}
}

// SM-EVT-016 end to end (ADR-0049): two installations subscribed to each other's inbox events form a loop (A
// publishes, B ingests and publishes, A ingests, ...). Each ingestion adds one to spaceman-depth, so the loop ends
// at the configured event_depth: the event that reaches it is quarantined with the reason, never turned into an item.
// Each installation still drops its own events by spaceman-origin.
func TestEventLoopBetweenInstallationsIsBounded(t *testing.T) {
	nats := pulsetest.Start(t)
	depth3 := func(c *config.Config) { c.Limits.EventDepth = 3 }
	a := newInstallation(t, nats, "spaceman-a", true, depth3)
	b := newInstallation(t, nats, "spaceman-b", true, depth3)
	ctx := context.Background()
	seed := a.subscribe(t, "alerts", "evt.ops.alert.fired", nil)
	aPeer := a.subscribe(t, "peer items", "evt.spaceman.inbox.item.created", nil)
	bPeer := b.subscribe(t, "peer items", "evt.spaceman.inbox.item.created", nil)

	pub, err := pulse.Connect(nats.URL, pulse.WithCreds(nats.Creds("monitor")), pulse.WithSource("monitor"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pub.Close)
	if _, err := pub.Publish(ctx, "evt.ops.alert.fired", "ops.alert.v1", json.RawMessage(`{"name":"db-lag"}`)); err != nil {
		t.Fatal(err)
	}
	// A: the alert (depth 1) and B's item event (depth 3). B: A's first item event (depth 2). B quarantines A's second.
	eventually(t, "the loop ends in quarantine", func() bool { return len(b.quarantine(t, bPeer.ID)) == 1 })
	time.Sleep(500 * time.Millisecond) // nothing else follows
	depths := func(items []*inbox.Item) []int64 {
		var out []int64
		for _, it := range items {
			out = append(out, it.AutomationDepth)
		}
		slices.Sort(out)
		return out
	}
	if got := depths(a.items(t)); len(got) != 2 || got[0] != 1 || got[1] != 3 {
		t.Fatalf("A's items have depths %v, want [1 3]", got)
	}
	if got := depths(b.items(t)); len(got) != 1 || got[0] != 2 {
		t.Fatalf("B's items have depths %v, want [2]", got)
	}
	if it := itemBySubscription(a.items(t), seed.ID); it == nil || it.AutomationDepth != 1 {
		t.Fatalf("the external event starts a chain at 1: %+v", it)
	}
	q := b.quarantine(t, bPeer.ID)[0]
	if q.Depth != 3 || !strings.Contains(q.Error, "automation depth") || !strings.Contains(q.Error, "event_depth limit of 3") {
		t.Fatalf("quarantine %+v", q)
	}
	// Each side dropped its own item events by origin (the guard of ADR-0030 still applies).
	for _, in := range []struct {
		i   *installation
		sub string
	}{{a, aPeer.ID}, {b, bPeer.ID}} {
		var l struct{ Items []events.Delivery }
		in.i.root.do("GET", "/spaces/"+in.i.space.ID+"/subscriptions/"+in.sub+"/deliveries", nil).json(t, &l)
		own := 0
		for _, d := range l.Items {
			if d.Outcome == events.OutcomeFiltered && d.Detail == events.OwnOriginDetail {
				own++
			}
		}
		if own == 0 {
			t.Fatalf("no own-origin filtering on %s: %+v", in.sub, l.Items)
		}
	}
	// Reprocessing is an operator's decision: the item is created, one level deeper, and the chain goes on from there.
	r := b.root.do("POST", "/spaces/"+b.space.ID+"/subscriptions/"+bPeer.ID+"/quarantine/"+q.ID, map[string]any{"action": "reprocess"})
	expect(t, r, 200)
	eventually(t, "reprocessed item", func() bool { return len(b.items(t)) == 2 })
	if got := depths(b.items(t)); got[1] != 4 {
		t.Fatalf("B's items after the reprocess have depths %v, want [2 4]", got)
	}
	eventually(t, "A quarantines the next hop", func() bool { return len(a.quarantine(t, aPeer.ID)) == 1 })
}
