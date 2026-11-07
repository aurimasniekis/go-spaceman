package app_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/api"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/events"
	"github.com/aurimasniekis/go-spaceman/internal/events/pulsetest"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
)

// SM-EVT-003/005/015/017/018 over HTTP: connections never echo credentials, the catalog has the verified GitHub seeds,
// a sample event previews into an inbox item, a subscription shows the grant it needs, and replay needs replay:execute.
func TestEventsOverHTTP(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	nats := pulsetest.Start(t)
	rootSecret, _, _ := h.c.Auth.IssueRootToken(ctx)
	root := h.client()
	root.bearer = rootSecret
	actor := audit.Actor{Type: audit.ActorUser, ID: "usr_test"}
	en, _ := h.c.Fleet.CreateNode(ctx, actor, fleet.CreateNodeInput{Name: "node-a"})
	sp, err := h.c.Fleet.CreateSpace(ctx, actor, fleet.CreateSpaceInput{Slug: "alpha", Name: "Alpha", NodeID: en.Node.ID})
	if err != nil {
		t.Fatal(err)
	}

	creds := string(nats.Creds("spaceman"))
	r := root.do("POST", "/event-connections", map[string]any{"name": "pulse", "url": nats.URL, "creds": creds, "enabled": true})
	expect(t, r, 201)
	if strings.Contains(string(r.body), "SEED") || strings.Contains(string(r.body), "NATS USER JWT") {
		t.Fatal("connection response contains credentials")
	}
	var conn events.Connection
	r.json(t, &conn)

	var cat struct{ Items []*events.Template }
	root.do("GET", "/event-templates", nil).json(t, &cat)
	var issue *events.Template
	for _, x := range cat.Items {
		if x.Slug == "github-issue" {
			issue = x
		}
	}
	if issue == nil || !issue.Builtin || issue.Versions[0].Status != "published" {
		t.Fatalf("catalog %+v", cat.Items)
	}

	sample, err := os.ReadFile("../events/template/testdata/pulse-github-issues-opened.json")
	if err != nil {
		t.Fatal(err)
	}
	var pv api.PreviewOut
	r = root.do("POST", "/event-templates/preview", map[string]any{"definition": issue.Versions[0].Definition,
		"vars":  map[string]string{"account_key": "example-org", "repository": "example-org/tool"},
		"event": map[string]any{"subject": "evt.github.example-org.webhook.issues.opened", "type": "github.webhook.issues.opened.v1", "data": json.RawMessage(sample)}})
	expect(t, r, 200)
	r.json(t, &pv)
	if pv.Outcome != "accepted" || !pv.SubjectMatch || pv.Title != "example-org/tool#42 opened: Bug <script>" {
		t.Fatalf("preview %+v", pv)
	}

	r = root.do("POST", "/spaces/"+sp.ID+"/subscriptions", map[string]any{"name": "tool issues", "connection_id": conn.ID, "template_version_id": issue.Versions[0].ID,
		"vars": map[string]string{"account_key": "example-org", "repository": "example-org/tool"}})
	expect(t, r, 201)
	var sub events.Subscription
	r.json(t, &sub)
	if sub.Grant != "durable:evt.github.example-org.webhook.issues.*:"+sub.Durable || sub.Consumer != "spaceman_"+sub.Durable || r.header.Get("ETag") != `"1"` {
		t.Fatalf("subscription %+v", sub)
	}
	if p := root.do("POST", "/spaces/"+sp.ID+"/subscriptions", map[string]any{"name": "wide", "connection_id": conn.ID, "template_version_id": issue.Versions[0].ID,
		"vars": map[string]string{"account_key": "*", "repository": "example-org/tool"}}).problem(t); p.Code != "validation_failed" {
		t.Fatalf("wildcard variable accepted: %+v", p)
	}

	// A viewer reads subscriptions but cannot change or replay them, nor add connections. (Operators hold replay:execute;
	// the separate permission keeps replay grantable on its own.)
	expect(t, root.do("POST", "/users", map[string]any{"email": "vic@example.com", "display_name": "Vic", "password": "correct horse battery staple",
		"grants": []map[string]string{{"role": "viewer", "domain": sp.ID}}}), 201)
	viewer := h.client()
	viewer.bearer = loginAndIssueToken(t, h, "vic@example.com", []map[string]string{{"role": "viewer", "domain": sp.ID}})
	expect(t, viewer.do("GET", "/spaces/"+sp.ID+"/subscriptions", nil), 200)
	expect(t, viewer.do("PUT", "/spaces/"+sp.ID+"/subscriptions/"+sub.ID, map[string]any{"enabled": true, "start": map[string]any{"policy": "all"}}, "If-Match", `"1"`), 403)
	expect(t, viewer.do("POST", "/event-connections", map[string]any{"name": "x", "url": nats.URL, "creds": creds, "enabled": true}), 403)

	// SM-EVT-015: the explicit replay operation needs replay:execute, If-Match and a reason; it is listed in the history.
	replay := map[string]any{"policy": "all", "reason": "rebuild the inbox after a template fix"}
	expect(t, viewer.do("POST", "/spaces/"+sp.ID+"/subscriptions/"+sub.ID+"/replay", replay, "If-Match", `"1"`), 403)
	expect(t, root.do("POST", "/spaces/"+sp.ID+"/subscriptions/"+sub.ID+"/replay", replay), 428)
	expect(t, root.do("POST", "/spaces/"+sp.ID+"/subscriptions/"+sub.ID+"/replay", map[string]any{"policy": "all"}, "If-Match", `"1"`), 422)
	expect(t, root.do("POST", "/spaces/"+sp.ID+"/subscriptions/"+sub.ID+"/replay", map[string]any{"policy": "new", "reason": "x"}, "If-Match", `"1"`), 422)
	r = root.do("POST", "/spaces/"+sp.ID+"/subscriptions/"+sub.ID+"/replay", replay, "If-Match", `"1"`)
	expect(t, r, 202)
	var rp events.Replay
	r.json(t, &rp)
	if rp.Policy != "all" || rp.Generation != 2 || rp.Reason != replay["reason"] {
		t.Fatalf("replay %+v", rp)
	}
	expect(t, root.do("POST", "/spaces/"+sp.ID+"/subscriptions/"+sub.ID+"/replay", replay, "If-Match", `"1"`), 409)
	var hist struct{ Items []events.Replay }
	viewer.do("GET", "/spaces/"+sp.ID+"/subscriptions/"+sub.ID+"/replays", nil).json(t, &hist)
	if len(hist.Items) != 1 || hist.Items[0].ID != rp.ID {
		t.Fatalf("history %+v", hist)
	}
}

// SM-EVT-011, ADR-0042 end to end: a core NATS subscription created over HTTP is labelled non-durable (no durable or
// consumer, a subscribe grant), receives a plain NATS message published without JetStream into one inbox item, drops
// the duplicate, refuses replay and start-now, and is counted on the management listener's /metrics.
func TestCoreSubscriptionEndToEnd(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	nats := pulsetest.Start(t)
	done := make(chan struct{})
	go func() { events.NewManager(h.c.Events, nil).Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	rootSecret, _, _ := h.c.Auth.IssueRootToken(ctx)
	root := h.client()
	root.bearer = rootSecret
	actor := audit.Actor{Type: audit.ActorUser, ID: "usr_test"}
	en, _ := h.c.Fleet.CreateNode(ctx, actor, fleet.CreateNodeInput{Name: "node-a"})
	sp, err := h.c.Fleet.CreateSpace(ctx, actor, fleet.CreateSpaceInput{Slug: "alpha", Name: "Alpha", NodeID: en.Node.ID})
	if err != nil {
		t.Fatal(err)
	}
	var conn events.Connection
	r := root.do("POST", "/event-connections", map[string]any{"name": "pulse", "url": nats.URL, "creds": string(nats.Creds("spaceman")), "enabled": true})
	expect(t, r, 201)
	r.json(t, &conn)
	custom := map[string]any{"subject": "evt.ops.alert.fired", "title": "Alert {{data.name}}", "body": "{{data | json}}", "variables": []any{}, "filters": []any{}}
	r = root.do("POST", "/spaces/"+sp.ID+"/subscriptions", map[string]any{"name": "alerts", "connection_id": conn.ID, "custom": custom, "delivery": "core"})
	expect(t, r, 201)
	var sub events.Subscription
	r.json(t, &sub)
	if sub.Delivery != "core" || sub.Durable != "" || sub.Consumer != "" || sub.Grant != "subscribe:evt.ops.alert.fired" {
		t.Fatalf("core subscription %+v", sub)
	}
	if p := root.do("POST", "/spaces/"+sp.ID+"/subscriptions", map[string]any{"name": "history", "connection_id": conn.ID, "custom": custom,
		"delivery": "core", "start": map[string]any{"policy": "all", "replay": false, "generation": 0}}).problem(t); p.Code != "validation_failed" {
		t.Fatalf("core subscription with history: %+v", p)
	}
	eventually(t, "running", func() bool {
		var g events.Subscription
		root.do("GET", "/spaces/"+sp.ID+"/subscriptions/"+sub.ID, nil).json(t, &g)
		return g.State == "running"
	})

	// A source publishing with plain NATS (no JetStream acknowledgement).
	src := nats.Conn("plain-source")
	t.Cleanup(src.Close)
	publish := func() {
		if err := src.Publish("evt.ops.alert.fired", []byte(`{"name":"db-lag"}`)); err != nil {
			t.Fatal(err)
		}
		if err := src.Flush(); err != nil {
			t.Fatal(err)
		}
	}
	deliveries := func() map[string]int {
		var l struct{ Items []events.Delivery }
		root.do("GET", "/spaces/"+sp.ID+"/subscriptions/"+sub.ID+"/deliveries", nil).json(t, &l)
		out := map[string]int{}
		for _, d := range l.Items {
			out[d.Outcome]++
		}
		return out
	}
	publish()
	eventually(t, "accepted", func() bool { return deliveries()[events.OutcomeAccepted] == 1 })
	publish() // the same message again, without an ID: deduplicated by content
	eventually(t, "duplicate", func() bool { return deliveries()[events.OutcomeDuplicate] == 1 })
	var items struct {
		Items []struct {
			Title string `json:"title"`
		} `json:"items"`
	}
	root.do("GET", "/spaces/"+sp.ID+"/inbox", nil).json(t, &items)
	if len(items.Items) != 1 || items.Items[0].Title != "Alert db-lag" {
		t.Fatalf("inbox %+v", items.Items)
	}

	// No cursor: replay and start-now are refused with a clear problem.
	p := root.do("POST", "/spaces/"+sp.ID+"/subscriptions/"+sub.ID+"/replay", map[string]any{"policy": "all", "reason": "rebuild"}, "If-Match", `"1"`).problem(t)
	if p.Code != "precondition_failed" || !strings.Contains(p.Detail, "non-durable") {
		t.Fatalf("core replay: %+v", p)
	}
	p = root.do("PUT", "/spaces/"+sp.ID+"/subscriptions/"+sub.ID, map[string]any{"enabled": true, "start": map[string]any{"policy": "new", "replay": false, "generation": 0}}, "If-Match", `"1"`).problem(t)
	if p.Code != "precondition_failed" || !strings.Contains(p.Detail, "start now") {
		t.Fatalf("core start now: %+v", p)
	}

	// The management listener's /metrics: bounded labels, never the subscription or the subject.
	mgmt := httptest.NewServer(h.c.ManagementHandler())
	t.Cleanup(mgmt.Close)
	scrape := func() string {
		res, err := http.Get(mgmt.URL + "/metrics")
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return string(b)
	}
	body := scrape()
	for _, want := range []string{
		`spaceman_ingest_deliveries_total{delivery="core",origin="live",result="created"} 1`,
		`spaceman_ingest_deliveries_total{delivery="core",origin="live",result="duplicate"} 1`,
		`spaceman_ingest_processing_duration_seconds_count{delivery="core"} 2`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics lacks %s", want)
		}
	}
	if strings.Contains(body, sub.ID) || strings.Contains(body, "evt.ops") {
		t.Fatal("/metrics names a subscription or a subject")
	}
	// An edit wakes the manager, whose round refreshes the gauges.
	expect(t, root.do("PUT", "/spaces/"+sp.ID+"/subscriptions/"+sub.ID, map[string]any{"enabled": true, "name": "alerts (core)"}, "If-Match", `"1"`), 200)
	eventually(t, "subscription gauge", func() bool {
		return strings.Contains(scrape(), `spaceman_ingest_subscriptions{delivery="core",state="running"} 1`)
	})
}
