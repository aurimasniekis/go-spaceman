package app_test

import (
	"context"
	"encoding/json"
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
}
