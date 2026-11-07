package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aurimasniekis/go-pulse/pkg/pulse"
	"github.com/nats-io/nats.go"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/events/template"
	"github.com/aurimasniekis/go-spaceman/internal/obs/metrics/metricstest"
	"github.com/aurimasniekis/go-spaceman/internal/outbox"
)

func (f *fx) coreSubscribe(space, name string) *Subscription {
	f.t.Helper()
	s, err := f.svc.CreateSubscription(f.ctx, admin, space, SubscriptionInput{Name: name, ConnectionID: f.conn.ID, TemplateVersionID: f.issue.Versions[0].ID,
		Vars: map[string]string{"account_key": "example-org", "repository": "example-org/tool"}, Delivery: DeliveryCore})
	must(f.t, err)
	eventually(f.t, "core subscription running", func() bool { g, _ := f.svc.GetSubscription(f.ctx, space, s.ID); return g.State == "running" })
	return s
}

// corePublish publishes a plain core NATS message (no JetStream acknowledgement), with optional headers.
func (f *fx) corePublish(nc *nats.Conn, subject string, data []byte, headers map[string]string) {
	f.t.Helper()
	msg := nats.NewMsg(subject)
	msg.Data = data
	for k, v := range headers {
		msg.Header.Set(k, v)
	}
	must(f.t, nc.PublishMsg(msg))
	must(f.t, nc.Flush())
}

// SM-EVT-011, ADR-0042: a core NATS subscription uses the same filtering, deduplication, quarantine and inbox path
// as a durable one; it is labelled non-durable (subscribe grant, no durable or consumer), deduplicates by
// Nats-Msg-Id or, without one, by content, and every delivery is live (core NATS never redelivers).
func TestCoreSubscriptionIngests(t *testing.T) {
	f := newFx(t)
	s := f.coreSubscribe("spc_a", "issues-core")
	if s.Delivery != DeliveryCore || s.Durable != "" || s.Consumer != "" || s.Grant != "subscribe:evt.github.example-org.webhook.issues.*" || s.Pending {
		t.Fatalf("core subscription %+v", s)
	}
	src := f.nats.Conn("plain-source")
	t.Cleanup(src.Close)
	hdr := map[string]string{"Nats-Msg-Id": "core-msg-1", "ce-type": issueType, "ce-source": "plain-source"}
	f.corePublish(src, issueSubject, issuePayload(t, ""), hdr)
	eventually(t, "item", func() bool { return len(f.items("spc_a")) == 1 })
	it, err := f.inbox.Get(f.ctx, "spc_a", f.items("spc_a")[0].ID)
	must(t, err)
	if it.Trusted["event_id"] != "core-msg-1" || it.Trusted["subscription_id"] != s.ID || it.Title != "example-org/tool#42 opened: Bug <script>" {
		t.Fatalf("item %+v", it.Trusted)
	}
	// The same message ID again: a duplicate (origin live: the source re-sent it), no second item.
	f.corePublish(src, issueSubject, issuePayload(t, ""), hdr)
	eventually(t, "duplicate", func() bool { return f.outcomes("spc_a", s.ID)[OutcomeDuplicate] == 1 })
	// No message ID: deduplicated by content; a different payload is a new event.
	f.corePublish(src, issueSubject, issuePayload(t, ""), nil)
	f.corePublish(src, issueSubject, issuePayload(t, ""), nil)
	eventually(t, "content-hash duplicate", func() bool { return f.outcomes("spc_a", s.ID)[OutcomeDuplicate] == 2 })
	if n := len(f.items("spc_a")); n != 2 {
		t.Fatalf("items = %d, want 2", n)
	}
	// Another repository: filtered.
	f.corePublish(src, issueSubject, issuePayload(t, "example-org/other"), nil)
	eventually(t, "filtered", func() bool { return f.outcomes("spc_a", s.ID)[OutcomeFiltered] == 1 })
	hashed := 0
	for _, d := range f.deliveries("spc_a", s.ID) {
		if d.Origin != OriginLive || d.StreamSeq != 0 {
			t.Fatalf("core delivery %+v", d)
		}
		if strings.HasPrefix(d.EventID, "sha256-") && len(d.EventID) == len("sha256-")+32 {
			hashed++
		}
	}
	if hashed != 3 {
		t.Fatalf("content-hashed deliveries = %d, want 3", hashed)
	}
	// Ingestion metrics (bounded labels only). They are recorded after the delivery row, so wait for them.
	for _, c := range []struct {
		result string
		want   float64
	}{{"created", 2}, {"duplicate", 2}, {"filtered", 1}} {
		eventually(t, "core "+c.result+" metric", func() bool {
			return metricstest.Value(t, f.mt, "spaceman_ingest_deliveries_total", "delivery", "core", "origin", "live", "result", c.result) == c.want
		})
	}
	eventually(t, "core latency samples", func() bool {
		return metricstest.Value(t, f.mt, "spaceman_ingest_processing_duration_seconds", "delivery", "core") == 5
	})
	eventually(t, "gauges", func() bool {
		return metricstest.Value(t, f.mt, "spaceman_ingest_subscriptions", "delivery", "core", "state", "running") == 1 &&
			metricstest.Value(t, f.mt, "spaceman_ingest_connections", "connection_kind", "pulse", "state", "connected") == 1
	})
}

// SM-EVT-013 on core: an invalid message is quarantined (nothing could redeliver it anyway) and can be reprocessed.
func TestCoreSubscriptionQuarantine(t *testing.T) {
	f := newFx(t)
	def := template.Definition{Subject: "evt.ops.alert.fired", Title: "{{data.name}}", InputSchema: json.RawMessage(`{"type":"object","required":["name","level"]}`)}
	s, err := f.svc.CreateSubscription(f.ctx, admin, "spc_a", SubscriptionInput{Name: "alerts", ConnectionID: f.conn.ID, Custom: &def, Delivery: DeliveryCore})
	must(t, err)
	eventually(t, "running", func() bool { g, _ := f.svc.GetSubscription(f.ctx, "spc_a", s.ID); return g.State == "running" })
	src := f.nats.Conn("plain-source")
	t.Cleanup(src.Close)
	f.corePublish(src, "evt.ops.alert.fired", []byte(`{"name":"db-lag"}`), nil)
	eventually(t, "quarantined", func() bool { q, _ := f.svc.Quarantine(f.ctx, "spc_a", s.ID, "open"); return len(q) == 1 })
	if g, _ := f.svc.GetSubscription(f.ctx, "spc_a", s.ID); g.LastError != "" || g.Quarantined != 1 {
		t.Fatalf("a quarantined message is not a subscription error: %+v", g)
	}
	eventually(t, "quarantined metric", func() bool {
		return metricstest.Value(t, f.mt, "spaceman_ingest_deliveries_total", "delivery", "core", "result", "quarantined") == 1
	})
	eventually(t, "quarantine gauge", func() bool { return metricstest.Value(t, f.mt, "spaceman_ingest_quarantine_open") == 1 })
	def.InputSchema = json.RawMessage(`{"type":"object","required":["name"]}`)
	cur, _ := f.svc.GetSubscription(f.ctx, "spc_a", s.ID)
	_, err = f.svc.UpdateSubscription(f.ctx, admin, "spc_a", s.ID, cur.Version, SubscriptionUpdate{Enabled: true, Custom: &def})
	must(t, err)
	q, _ := f.svc.Quarantine(f.ctx, "spc_a", s.ID, "open")
	if r, err := f.svc.ResolveQuarantine(f.ctx, admin, "spc_a", s.ID, q[0].ID, true); err != nil || r.Status != "reprocessed" || len(f.items("spc_a")) != 1 {
		t.Fatalf("reprocess: %+v %v", r, err)
	}
	if v := metricstest.Value(t, f.mt, "spaceman_ingest_deliveries_total", "delivery", "core", "origin", "reprocess", "result", "created"); v != 1 {
		t.Fatalf("reprocess metric = %v", v)
	}
	eventually(t, "quarantine gauge drained", func() bool { return metricstest.Value(t, f.mt, "spaceman_ingest_quarantine_open") == 0 })
}

// ADR-0042: core subscriptions have no cursor. Replays and cursor changes (start now included) are refused with a
// clear error, history cannot be requested at creation, and a subject change resubscribes without asking for a cursor.
func TestCoreSubscriptionRefusesCursors(t *testing.T) {
	f := newFx(t)
	vars := map[string]string{"account_key": "example-org", "repository": "example-org/tool"}
	if _, err := f.svc.CreateSubscription(f.ctx, admin, "spc_a", SubscriptionInput{Name: "x", ConnectionID: f.conn.ID, TemplateVersionID: f.issue.Versions[0].ID,
		Vars: vars, Delivery: DeliveryCore, Start: Cursor{Policy: StartAll}}); apperr.CodeOf(err) != apperr.CodeInvalid {
		t.Fatalf("core with history: %v", err)
	}
	if _, err := f.svc.CreateSubscription(f.ctx, admin, "spc_a", SubscriptionInput{Name: "x", ConnectionID: f.conn.ID, TemplateVersionID: f.issue.Versions[0].ID,
		Vars: vars, Delivery: "queue"}); apperr.CodeOf(err) != apperr.CodeInvalid {
		t.Fatalf("unknown delivery: %v", err)
	}
	s := f.coreSubscribe("spc_a", "issues-core")
	_, err := f.svc.Replay(f.ctx, admin, "spc_a", s.ID, s.Version, ReplayInput{Policy: StartAll, Reason: "rebuild"})
	if apperr.CodeOf(err) != apperr.CodePrecondition || !strings.Contains(err.Error(), "non-durable") {
		t.Fatalf("core replay: %v", err)
	}
	_, err = f.svc.UpdateSubscription(f.ctx, admin, "spc_a", s.ID, s.Version, SubscriptionUpdate{Enabled: true, Start: &Cursor{Policy: StartNew}})
	if apperr.CodeOf(err) != apperr.CodePrecondition || !strings.Contains(err.Error(), "start now") {
		t.Fatalf("core start now: %v", err)
	}
	if l, _ := f.svc.Replays(f.ctx, "spc_a", s.ID, 10); len(l) != 0 {
		t.Fatalf("refused replay recorded: %+v", l)
	}
	if v := metricstest.Value(t, f.mt, "spaceman_ingest_replays_total"); v != 0 {
		t.Fatalf("replays metric = %v", v)
	}
	// Another account: the subject changes, no cursor is needed, and the subscription follows it.
	up, err := f.svc.UpdateSubscription(f.ctx, admin, "spc_a", s.ID, s.Version, SubscriptionUpdate{Enabled: true,
		Vars: map[string]string{"account_key": "other-org", "repository": "other-org/tool"}})
	must(t, err)
	if up.Subject != "evt.github.other-org.webhook.issues.*" || up.Delivery != DeliveryCore || up.Cursor.Generation != s.Cursor.Generation {
		t.Fatalf("updated %+v", up)
	}
	src := f.nats.Conn("plain-source")
	t.Cleanup(src.Close)
	eventually(t, "resubscribed", func() bool {
		f.corePublish(src, "evt.github.other-org.webhook.issues.opened", issuePayload(t, "other-org/tool"), nil)
		return len(f.items("spc_a")) == 1
	})
}

// A core subscription the server refuses (no subscribe grant) is reported as an error naming the grant, and retried.
func TestCoreSubscriptionDenied(t *testing.T) {
	f := newFx(t)
	conn, err := f.svc.CreateConnection(f.ctx, admin, ConnectionInput{Name: "pulse-narrow", URL: f.nats.URL,
		Creds: string(f.nats.SubscribeOnlyCreds("narrow", "evt.other.>", "_INBOX_narrow.>")), Enabled: true})
	must(t, err)
	def := template.Definition{Subject: "evt.ops.alert.fired", Title: "{{data.name}}"}
	s, err := f.svc.CreateSubscription(f.ctx, admin, "spc_a", SubscriptionInput{Name: "alerts", ConnectionID: conn.ID, Custom: &def, Delivery: DeliveryCore})
	must(t, err)
	eventually(t, "denied", func() bool {
		g, _ := f.svc.GetSubscription(f.ctx, "spc_a", s.ID)
		return g.State == "error" && strings.Contains(g.LastError, "subscribe:evt.ops.alert.fired")
	})
	eventually(t, "error gauge", func() bool {
		return metricstest.Value(t, f.mt, "spaceman_ingest_subscriptions", "delivery", "core", "state", "error") == 1
	})
}

// The asynchronous error callback marks a slow core subscription and a refusal that arrives late.
func TestCoreAsyncErrors(t *testing.T) {
	f := newFx(t)
	s := f.coreSubscribe("spc_a", "issues-core")
	var run *subRun
	f.m.coreMu.Lock()
	for _, r := range f.m.core {
		if r.id == s.ID {
			run = r
		}
	}
	f.m.coreMu.Unlock()
	if run == nil {
		t.Fatal("core run not registered")
	}
	f.m.natsError(run.nc, run.nsub, nats.ErrSlowConsumer)
	if g, _ := f.svc.GetSubscription(f.ctx, "spc_a", s.ID); !strings.Contains(g.LastError, "slow consumer") {
		t.Fatalf("slow consumer: %q", g.LastError)
	}
	f.m.natsError(run.nc, nil, fmt.Errorf("%w: Permissions Violation for Subscription to %q", nats.ErrPermissionViolation, "evt.unrelated"))
	if run.failed.Load() {
		t.Fatal("an unrelated refusal marked the subscription")
	}
	f.m.natsError(run.nc, nil, fmt.Errorf("%w: Permissions Violation for Subscription to %q", nats.ErrPermissionViolation, s.Subject))
	if !run.failed.Load() {
		t.Fatal("refusal not recorded")
	}
	eventually(t, "retried", func() bool {
		f.m.coreMu.Lock()
		defer f.m.coreMu.Unlock()
		_, still := f.m.core[run.nsub]
		return !still
	})
}

// The ingestion metrics classify every result, including rate-limited and failed events, without free-form labels.
func TestObserveClassifies(t *testing.T) {
	f := newFx(t)
	f.svc.observe(DeliveryDurable, OriginRedelivery, OutcomeDuplicate, nil, time.Millisecond)
	f.svc.observe(DeliveryDurable, OriginReplay, OutcomeReplayed, nil, time.Millisecond)
	f.svc.observe(DeliveryDurable, "", "", errThrottled, -1)
	f.svc.observe(DeliveryCore, OriginLive, OutcomeAccepted, errors.New("disk full"), time.Millisecond)
	f.svc.observe(DeliveryDurable, OriginLive, "", pulse.Terminal(&QuarantinedError{Cause: errors.New("bad")}), time.Millisecond)
	for _, c := range []struct {
		labels []string
		want   float64
	}{
		{[]string{"origin", "redelivery", "result", "duplicate"}, 1},
		{[]string{"origin", "replay", "result", "created"}, 1},
		{[]string{"origin", "live", "result", "rate_limited"}, 1},
		{[]string{"delivery", "core", "result", "error"}, 1},
		{[]string{"result", "quarantined"}, 1},
	} {
		if v := metricstest.Value(t, f.mt, "spaceman_ingest_deliveries_total", c.labels...); v != c.want {
			t.Errorf("%v = %v, want %v", c.labels, v, c.want)
		}
	}
	if v := metricstest.Value(t, f.mt, "spaceman_ingest_processing_duration_seconds", "delivery", "durable"); v != 3 {
		t.Errorf("durable latency samples = %v (rate-limited events are not timed)", v)
	}
}

// Durable consumers report their pending and unacknowledged events; replays are counted.
func TestDurableIngestMetrics(t *testing.T) {
	f := newFx(t)
	var rate atomic.Int64
	rate.Store(6) // one per 10 s, burst 1: the rest wait
	f.svc.o.RateFor = func(context.Context, string) int64 { return rate.Load() }
	s := f.subscribe("spc_a", "issues", Cursor{})
	for i := range 4 {
		f.publish(issueSubject, issueType, issuePayload(t, ""), "pend-"+string(rune('a'+i)))
	}
	eventually(t, "waiting events", func() bool {
		return metricstest.Value(t, f.mt, "spaceman_ingest_consumer_pending", "delivery", "durable")+
			metricstest.Value(t, f.mt, "spaceman_ingest_consumer_ack_pending") >= 2
	})
	rate.Store(0)
	cur, _ := f.svc.GetSubscription(f.ctx, "spc_a", s.ID)
	_, err := f.svc.Replay(f.ctx, admin, "spc_a", s.ID, cur.Version, ReplayInput{Policy: StartAll, Reason: "metrics"})
	must(t, err)
	if v := metricstest.Value(t, f.mt, "spaceman_ingest_replays_total"); v != 1 {
		t.Fatalf("replays = %v", v)
	}
	// The metric is recorded after the delivery, asynchronously to this test.
	eventually(t, "durable created metric", func() bool {
		return metricstest.Value(t, f.mt, "spaceman_ingest_deliveries_total", "delivery", "durable", "result", "created") >= 1
	})
}

// SM-EVT-016 on core: a message carrying this installation's spaceman-origin is recorded as filtered, not ingested.
func TestCoreOwnEventsAreNotReingested(t *testing.T) {
	f := newFx(t)
	f.svc.o.Origin = "spaceman-test" // before the subscription starts its handler
	s := f.coreSubscribe("spc_a", "issues-core")
	src := f.nats.Conn("plain-source")
	t.Cleanup(src.Close)
	f.corePublish(src, issueSubject, issuePayload(t, ""), map[string]string{outbox.HeaderOrigin: "spaceman-test"})
	eventually(t, "own message filtered", func() bool { return f.outcomes("spc_a", s.ID)[OutcomeFiltered] == 1 })
	if d := f.deliveries("spc_a", s.ID); len(d) == 0 || d[0].Detail != OwnOriginDetail || len(f.items("spc_a")) != 0 {
		t.Fatalf("delivery %+v items %d", d, len(f.items("spc_a")))
	}
}
