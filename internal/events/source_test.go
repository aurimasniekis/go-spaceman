package events

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/attachments"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/events/pulsetest"
	"github.com/aurimasniekis/go-spaceman/internal/events/template"
	"github.com/aurimasniekis/go-spaceman/internal/inbox"
	"github.com/aurimasniekis/go-spaceman/internal/keyring/keyringtest"
	"github.com/aurimasniekis/go-spaceman/internal/obs/metrics"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
	"github.com/aurimasniekis/go-spaceman/internal/store/storetest"
)

// fakeSource is an inbound event source that receives nothing by itself: the test delivers events through it.
type fakeSource struct {
	m        *Manager
	delivery string
	fail     error
	mu       sync.Mutex
	starts   int
	stops    int
	running  map[string]db.EventSubscription
}

func (f *fakeSource) Delivery() string { return f.delivery }

func (f *fakeSource) Start(ctx context.Context, _ *connRun, s db.EventSubscription, key string) (*subRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts++
	if f.fail != nil {
		return nil, f.fail
	}
	f.running[s.ID] = s
	_, stop := context.WithCancel(ctx)
	return &subRun{key: key, id: s.ID, subject: s.EffectiveSubject, stop: func() {
		stop()
		f.mu.Lock()
		f.stops++
		delete(f.running, s.ID)
		f.mu.Unlock()
	}}, nil
}

// deliver hands an event to the manager's processing path, as a durable source does with every message.
func (f *fakeSource) deliver(ctx context.Context, subID string, e template.Event, seq uint64) error {
	f.mu.Lock()
	_, ok := f.running[subID]
	f.mu.Unlock()
	if !ok {
		return errors.New("not running")
	}
	return f.m.svc.Process(ctx, subID, e, seq, Origin{})
}

// SM-ARCH-003: the ingestion manager drives inbound sources only through the Source interface. With fake sources in
// place of Pulse's durable and core NATS sources, it starts a subscription's source once, keeps it while nothing
// changes, stops it when the subscription is disabled, reports a source that fails or a delivery mode without a
// source, and an event the fake delivers takes the normal path into the inbox (deduplicated on redelivery).
func TestManagerUsesSourceInterface(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := storetest.Open(t, storetest.WithClock(clock.System{}))
	kr := keyringtest.Load(t, s)
	aw := audit.NewWriter(kr)
	ch := changefeed.New(nil)
	att := attachments.New(s, attachments.NewLocal(t.TempDir()), aw, attachments.Options{})
	ib := inbox.New(s, aw, ch, att)
	svc := New(s, kr, aw, ch, ib, att, Options{Metrics: metrics.New()})
	must(t, s.W().TxNamed(ctx, "seed", func(tx *store.Tx) error {
		for _, q := range []string{
			`INSERT INTO nodes (id, name, state, created_at, created_by, updated_at) VALUES ('nod_1', 'n1', 'active', 0, 't', 0)`,
			`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('spc_a', 'aaa', 'A', 'nod_1', 'active', 'a', 0, 't', 0)`,
		} {
			if _, err := tx.SQL.ExecContext(ctx, q); err != nil {
				return err
			}
		}
		return nil
	}))
	must(t, svc.SeedBuiltins(ctx))
	// The connection itself is real (an in-process NATS server shaped like Pulse): the sources are what is faked.
	nats := pulsetest.Start(t)
	conn, err := svc.CreateConnection(ctx, admin, ConnectionInput{Name: "pulse-fake", URL: nats.URL, Creds: string(nats.Creds("spaceman")), Enabled: true})
	must(t, err)
	var issue *Template
	ts, _ := svc.ListTemplates(ctx)
	for _, x := range ts {
		if x.Slug == "github-issue" {
			issue = x
		}
	}
	m := NewManager(svc, nil)
	t.Cleanup(m.stopAll)
	durable := &fakeSource{m: m, delivery: DeliveryDurable, running: map[string]db.EventSubscription{}}
	core := &fakeSource{m: m, delivery: DeliveryCore, running: map[string]db.EventSubscription{}, fail: errors.New("fake core source refused")}
	m.sources = map[string]Source{durable.Delivery(): durable, core.Delivery(): core}

	vars := map[string]string{"account_key": "example-org", "repository": "example-org/tool"}
	sub, err := svc.CreateSubscription(ctx, admin, "spc_a", SubscriptionInput{Name: "issues", ConnectionID: conn.ID, TemplateVersionID: issue.Versions[0].ID, Vars: vars})
	must(t, err)
	coreSub, err := svc.CreateSubscription(ctx, admin, "spc_a", SubscriptionInput{Name: "core", ConnectionID: conn.ID, TemplateVersionID: issue.Versions[0].ID,
		Vars: vars, Delivery: DeliveryCore})
	must(t, err)
	m.reconcile(ctx)
	m.reconcile(ctx) // nothing changed: the running source is kept
	if durable.starts != 1 || len(durable.running) != 1 {
		t.Fatalf("durable source started %d times, running %d", durable.starts, len(durable.running))
	}
	if g, _ := svc.GetSubscription(ctx, "spc_a", sub.ID); g.State != "running" {
		t.Fatalf("durable subscription %s %q", g.State, g.LastError)
	}
	if g, _ := svc.GetSubscription(ctx, "spc_a", coreSub.ID); g.State != "error" || g.LastError != "fake core source refused" {
		t.Fatalf("failing source: %s %q", g.State, g.LastError)
	}

	e := template.Event{ID: "evt-fake-1", Subject: issueSubject, Type: issueType, Source: "pulse-github", ContentType: "application/json",
		Time: time.Now().UTC(), Data: issuePayload(t, "")}
	must(t, durable.deliver(ctx, sub.ID, e, 7))
	must(t, durable.deliver(ctx, sub.ID, e, 7)) // a redelivery is a duplicate
	p, err := ib.List(ctx, "spc_a", "", "", 10)
	must(t, err)
	if len(p.Items) != 1 || p.Items[0].SourceID != sub.ID {
		t.Fatalf("inbox %+v", p.Items)
	}
	d, err := svc.Deliveries(ctx, "spc_a", sub.ID, 10)
	must(t, err)
	if len(d) != 2 || d[0].StreamSeq != 7 {
		t.Fatalf("deliveries %+v", d)
	}

	// A delivery mode without a source is reported, not ignored.
	delete(m.sources, DeliveryCore)
	m.reconcile(ctx)
	if g, _ := svc.GetSubscription(ctx, "spc_a", coreSub.ID); g.State != "error" || g.LastError != `no inbound event source for delivery "core"` {
		t.Fatalf("missing source: %s %q", g.State, g.LastError)
	}
	// Disabling the subscription stops its source.
	g, _ := svc.GetSubscription(ctx, "spc_a", sub.ID)
	_, err = svc.UpdateSubscription(ctx, admin, "spc_a", sub.ID, g.Version, SubscriptionUpdate{Name: g.Name, Enabled: false, TemplateVersionID: issue.Versions[0].ID, Vars: vars})
	must(t, err)
	m.reconcile(ctx)
	if durable.stops != 1 || len(durable.running) != 0 {
		t.Fatalf("durable source stops %d, running %d", durable.stops, len(durable.running))
	}
	if err := durable.deliver(ctx, sub.ID, e, 8); err == nil {
		t.Fatal("a stopped source delivered")
	}
}
