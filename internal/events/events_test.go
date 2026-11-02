package events

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-pulse/pkg/pulse"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/attachments"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/events/pulsetest"
	"github.com/aurimasniekis/go-spaceman/internal/events/template"
	"github.com/aurimasniekis/go-spaceman/internal/inbox"
	"github.com/aurimasniekis/go-spaceman/internal/keyring/keyringtest"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/storetest"
)

var admin = audit.Actor{Type: audit.ActorUser, ID: "usr_admin"}

type fx struct {
	t     *testing.T
	ctx   context.Context
	s     *store.Store
	svc   *Service
	inbox *inbox.Service
	nats  *pulsetest.Server
	pub   *pulse.Client
	conn  *Connection
	issue *Template
}

func newFx(t *testing.T) *fx {
	t.Helper()
	s := storetest.Open(t, storetest.WithClock(clock.System{}))
	kr := keyringtest.Load(t, s)
	aw := audit.NewWriter(kr)
	ch := changefeed.New(nil)
	att := attachments.New(s, attachments.NewLocal(t.TempDir()), aw, attachments.Options{})
	ib := inbox.New(s, aw, ch, att)
	f := &fx{t: t, ctx: context.Background(), s: s, inbox: ib, svc: New(s, kr, aw, ch, ib, att, Options{}), nats: pulsetest.Start(t)}
	must(t, s.W().TxNamed(f.ctx, "seed", func(tx *store.Tx) error {
		for _, q := range []string{
			`INSERT INTO nodes (id, name, state, created_at, created_by, updated_at) VALUES ('nod_1', 'n1', 'active', 0, 't', 0)`,
			`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('spc_a', 'aaa', 'A', 'nod_1', 'active', 'a', 0, 't', 0)`,
			`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('spc_b', 'bbb', 'B', 'nod_1', 'paused', 'b', 0, 't', 0)`,
		} {
			if _, err := tx.SQL.ExecContext(f.ctx, q); err != nil {
				return err
			}
		}
		return nil
	}))
	must(t, f.svc.SeedBuiltins(f.ctx))
	must(t, f.svc.SeedBuiltins(f.ctx)) // idempotent
	var err error
	f.conn, err = f.svc.CreateConnection(f.ctx, admin, ConnectionInput{Name: "pulse-test", URL: f.nats.URL, Creds: string(f.nats.Creds("spaceman")), Enabled: true})
	must(t, err)
	f.pub, err = pulse.Connect(f.nats.URL, pulse.WithCreds(f.nats.Creds("pulse-github")), pulse.WithSource("pulse-github"))
	must(t, err)
	t.Cleanup(f.pub.Close)
	ts, _ := f.svc.ListTemplates(f.ctx)
	for _, x := range ts {
		if x.Slug == "github-issue" {
			f.issue = x
		}
	}
	ctx, cancel := context.WithCancel(f.ctx)
	m := NewManager(f.svc, nil)
	m.retry = 200 * time.Millisecond
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return f
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out: %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func issuePayload(t *testing.T, repo string) json.RawMessage {
	b, err := os.ReadFile("template/testdata/pulse-github-issues-opened.json")
	must(t, err)
	if repo != "" {
		b = []byte(strings.ReplaceAll(string(b), `"example-org/tool"`, `"`+repo+`"`))
	}
	return b
}

func (f *fx) publish(subject, typ string, data json.RawMessage, id string) {
	f.t.Helper()
	opts := []pulse.PublishOption{}
	if id != "" {
		opts = append(opts, pulse.WithID(id))
	}
	_, err := f.pub.Publish(f.ctx, subject, typ, data, opts...)
	must(f.t, err)
}

func (f *fx) subscribe(space, name string, start Cursor) *Subscription {
	f.t.Helper()
	s, err := f.svc.CreateSubscription(f.ctx, admin, space, SubscriptionInput{Name: name, ConnectionID: f.conn.ID, TemplateVersionID: f.issue.Versions[0].ID,
		Vars: map[string]string{"account_key": "example-org", "repository": "example-org/tool"}, Start: start})
	must(f.t, err)
	eventually(f.t, "subscription running", func() bool { g, _ := f.svc.GetSubscription(f.ctx, space, s.ID); return g.State == "running" })
	return s
}

func (f *fx) items(space string) []*inbox.Item {
	p, err := f.inbox.List(f.ctx, space, "", "", 100)
	must(f.t, err)
	return p.Items
}

func (f *fx) outcomes(space, sub string) map[string]int {
	d, err := f.svc.Deliveries(f.ctx, space, sub, 100)
	must(f.t, err)
	out := map[string]int{}
	for _, x := range d {
		out[x.Outcome]++
	}
	return out
}

const issueSubject = "evt.github.example-org.webhook.issues.opened"
const issueType = "github.webhook.issues.opened.v1"

// SM-EVT-011/012/014, SM-EVT-008, SM-INBOX-002: a go-pulse-github event becomes one inbox item in each subscribed space
// (paused spaces keep ingesting), with provenance and the raw payload retained; acknowledgement follows the commit.
func TestIngestFanOutAndProvenance(t *testing.T) {
	f := newFx(t)
	a := f.subscribe("spc_a", "issues", Cursor{})
	b := f.subscribe("spc_b", "issues", Cursor{})
	if a.Durable == b.Durable || a.Consumer != "spaceman_"+a.Durable || a.Grant != "durable:evt.github.example-org.webhook.issues.*:"+a.Durable {
		t.Fatalf("durables %+v %+v", a, b)
	}
	f.publish(issueSubject, issueType, issuePayload(t, ""), "")
	eventually(t, "items in both spaces", func() bool { return len(f.items("spc_a")) == 1 && len(f.items("spc_b")) == 1 })
	it, err := f.inbox.Get(f.ctx, "spc_a", f.items("spc_a")[0].ID)
	must(t, err)
	if it.SourceKind != "event" || it.SourceID != a.ID || it.Title != "example-org/tool#42 opened: Bug <script>" || it.CorrelationID != "github:example-org/tool#42" ||
		it.Trusted["template_slug"] != "github-issue" || it.Trusted["subject"] != issueSubject || it.Trusted["instructions"] == "" ||
		len(it.Attachments) != 1 || it.Attachments[0].MIME != "text/plain; charset=utf-8" && it.Attachments[0].MIME != "application/json" {
		t.Fatalf("item %+v attachments %+v", it, it.Attachments)
	}
	if f.outcomes("spc_a", a.ID)[OutcomeAccepted] != 1 {
		t.Fatalf("deliveries %v", f.outcomes("spc_a", a.ID))
	}
	// Another repository of the same account: acknowledged and recorded as filtered, no item.
	f.publish(issueSubject, issueType, issuePayload(t, "example-org/other"), "")
	eventually(t, "filtered", func() bool { return f.outcomes("spc_a", a.ID)[OutcomeFiltered] == 1 })
	if len(f.items("spc_a")) != 1 {
		t.Fatal("filtered event created an item")
	}
	// A redelivered event (same Nats-Msg-Id after the JetStream dedup window) is a duplicate, not a second item.
	e := template.Event{ID: it.Trusted["event_id"].(string), Subject: issueSubject, Type: issueType, Data: issuePayload(t, "")}
	must(t, f.svc.Process(f.ctx, a.ID, e, 1, false))
	if len(f.items("spc_a")) != 1 || f.outcomes("spc_a", a.ID)[OutcomeDuplicate] != 1 {
		t.Fatalf("redelivery: %d items, %v", len(f.items("spc_a")), f.outcomes("spc_a", a.ID))
	}
}

// SM-EVT-013: an invalid event is quarantined once, never redelivered, and can be reprocessed after the definition is fixed.
func TestQuarantineAndReprocess(t *testing.T) {
	f := newFx(t)
	def := template.Definition{Subject: "evt.ops.alert.fired", Title: "{{data.name}}", Body: "{{data | json}}",
		InputSchema: json.RawMessage(`{"type":"object","required":["name","level"]}`)}
	s, err := f.svc.CreateSubscription(f.ctx, admin, "spc_a", SubscriptionInput{Name: "alerts", ConnectionID: f.conn.ID, Custom: &def})
	must(t, err)
	eventually(t, "running", func() bool { g, _ := f.svc.GetSubscription(f.ctx, "spc_a", s.ID); return g.State == "running" })
	f.publish("evt.ops.alert.fired", "ops.alert.v1", json.RawMessage(`{"name":"db-lag"}`), "")
	eventually(t, "quarantined", func() bool { q, _ := f.svc.Quarantine(f.ctx, "spc_a", s.ID, "open"); return len(q) == 1 })
	time.Sleep(1500 * time.Millisecond) // a nak would redeliver within a second
	if g, _ := f.svc.GetSubscription(f.ctx, "spc_a", s.ID); g.LastError != "" || g.State != "running" {
		t.Fatalf("a quarantined event is not a subscription error: %q %s", g.LastError, g.State)
	}
	if f.outcomes("spc_a", s.ID)[OutcomeQuarantined] != 1 || len(f.items("spc_a")) != 0 {
		t.Fatalf("poison event redelivered: %v", f.outcomes("spc_a", s.ID))
	}
	q, _ := f.svc.Quarantine(f.ctx, "spc_a", s.ID, "open")
	// Reprocessing with the unchanged definition fails again and stays open.
	if r, err := f.svc.ResolveQuarantine(f.ctx, admin, "spc_a", s.ID, q[0].ID, true); err != nil || r.Status != "open" {
		t.Fatalf("reprocess unchanged: %+v %v", r, err)
	}
	def.InputSchema = json.RawMessage(`{"type":"object","required":["name"]}`)
	cur, _ := f.svc.GetSubscription(f.ctx, "spc_a", s.ID)
	_, err = f.svc.UpdateSubscription(f.ctx, admin, "spc_a", s.ID, cur.Version, SubscriptionUpdate{Enabled: true, Custom: &def})
	must(t, err)
	r, err := f.svc.ResolveQuarantine(f.ctx, admin, "spc_a", s.ID, q[0].ID, true)
	if err != nil || r.Status != "reprocessed" || len(f.items("spc_a")) != 1 {
		t.Fatalf("reprocess fixed: %+v %v items=%d", r, err, len(f.items("spc_a")))
	}
	if _, err := f.svc.ResolveQuarantine(f.ctx, admin, "spc_a", s.ID, q[0].ID, false); apperr.CodeOf(err) != apperr.CodePrecondition {
		t.Fatalf("resolving twice: %v", err)
	}
}

// SM-EVT-007/015: edits never replay silently; "start now" skips history; an explicit replay redelivers it, labelled,
// and duplicates do not become new items.
func TestCursorSemantics(t *testing.T) {
	f := newFx(t)
	f.publish(issueSubject, issueType, issuePayload(t, ""), "")
	s := f.subscribe("spc_a", "issues", Cursor{}) // default: new events only
	time.Sleep(500 * time.Millisecond)
	if len(f.items("spc_a")) != 0 {
		t.Fatal("history delivered without asking for it")
	}
	cur, _ := f.svc.GetSubscription(f.ctx, "spc_a", s.ID)
	// Changing the subject without a cursor is refused.
	if _, err := f.svc.UpdateSubscription(f.ctx, admin, "spc_a", s.ID, cur.Version, SubscriptionUpdate{Enabled: true,
		Vars: map[string]string{"account_key": "other-org", "repository": "example-org/tool"}}); apperr.CodeOf(err) != apperr.CodeInvalid {
		t.Fatalf("subject change without cursor: %v", err)
	}
	// Explicit replay of everything the stream holds.
	up, err := f.svc.UpdateSubscription(f.ctx, admin, "spc_a", s.ID, cur.Version, SubscriptionUpdate{Enabled: true, Start: &Cursor{Policy: StartAll}})
	must(t, err)
	if !up.Cursor.Replay || up.Cursor.Generation != cur.Cursor.Generation+1 {
		t.Fatalf("cursor %+v", up.Cursor)
	}
	eventually(t, "replayed item", func() bool { return len(f.items("spc_a")) == 1 })
	eventually(t, "labelled replayed", func() bool { return f.outcomes("spc_a", s.ID)[OutcomeReplayed] == 1 })
	// Replaying again redelivers the same event: a duplicate, no new item.
	cur, _ = f.svc.GetSubscription(f.ctx, "spc_a", s.ID)
	_, err = f.svc.UpdateSubscription(f.ctx, admin, "spc_a", s.ID, cur.Version, SubscriptionUpdate{Enabled: true, Start: &Cursor{Policy: StartAll}})
	must(t, err)
	eventually(t, "duplicate recorded", func() bool { return f.outcomes("spc_a", s.ID)[OutcomeDuplicate] == 1 })
	if len(f.items("spc_a")) != 1 {
		t.Fatal("replay duplicated an item")
	}
	// Start now: the old event is not delivered again.
	cur, _ = f.svc.GetSubscription(f.ctx, "spc_a", s.ID)
	_, err = f.svc.UpdateSubscription(f.ctx, admin, "spc_a", s.ID, cur.Version, SubscriptionUpdate{Enabled: true, Start: &Cursor{Policy: StartNew}})
	must(t, err)
	eventually(t, "cursor applied", func() bool { g, _ := f.svc.GetSubscription(f.ctx, "spc_a", s.ID); return !g.Pending })
	time.Sleep(500 * time.Millisecond)
	if n := f.outcomes("spc_a", s.ID)[OutcomeDuplicate]; n != 1 {
		t.Fatalf("start-now redelivered history (%d duplicates)", n)
	}
}

// SM-EVT-004/006: published versions are immutable; following subscriptions move to a new version only when it keeps
// their subject; credentials are write-only and validated.
func TestTemplatesAndConnections(t *testing.T) {
	f := newFx(t)
	if _, err := f.svc.CreateConnection(f.ctx, admin, ConnectionInput{Name: "bad", URL: f.nats.URL, Creds: "not creds", Enabled: true}); apperr.CodeOf(err) != apperr.CodeInvalid {
		t.Fatalf("bad creds: %v", err)
	}
	if _, err := f.svc.CreateConnection(f.ctx, admin, ConnectionInput{Name: "web", URL: "https://x", Creds: string(f.nats.Creds("x")), Enabled: true}); apperr.CodeOf(err) != apperr.CodeInvalid {
		t.Fatalf("bad url: %v", err)
	}
	if b, _ := json.Marshal(f.conn); strings.Contains(string(b), "SEED") || strings.Contains(string(b), "JWT") || f.conn.ClientID != "spaceman" {
		t.Fatalf("connection leaks credentials or lacks client id: %s", b)
	}
	def := template.Definition{Subject: "evt.ops.deploy.{{vars.env}}", Variables: []template.Variable{{Name: "env", Pattern: "[a-z]+", Required: true}}, Title: "Deploy {{data.version}}"}
	tm, err := f.svc.CreateTemplate(f.ctx, admin, "deploys", "Deploys", "", def)
	must(t, err)
	v1, held, err := f.svc.Publish(f.ctx, admin, tm.Versions[0].ID)
	if err != nil || v1.Status != "published" || len(held) != 0 {
		t.Fatalf("publish v1: %+v %v", v1, err)
	}
	if _, _, err := f.svc.Publish(f.ctx, admin, v1.ID); apperr.CodeOf(err) != apperr.CodePrecondition {
		t.Fatalf("republish: %v", err)
	}
	s, err := f.svc.CreateSubscription(f.ctx, admin, "spc_a", SubscriptionInput{Name: "deploys", ConnectionID: f.conn.ID, TemplateVersionID: v1.ID, Follow: "latest",
		Vars: map[string]string{"env": "prod"}})
	must(t, err)
	def.Title = "Deployed {{data.version}}"
	v2, err := f.svc.SaveDraft(f.ctx, admin, tm.ID, def, "better title")
	must(t, err)
	if _, err := f.svc.CreateSubscription(f.ctx, admin, "spc_a", SubscriptionInput{Name: "draft", ConnectionID: f.conn.ID, TemplateVersionID: v2.ID,
		Vars: map[string]string{"env": "prod"}}); apperr.CodeOf(err) != apperr.CodeInvalid {
		t.Fatalf("subscribed to a draft: %v", err)
	}
	if _, held, err = f.svc.Publish(f.ctx, admin, v2.ID); err != nil || len(held) != 0 {
		t.Fatalf("publish v2: %v %v", held, err)
	}
	g, _ := f.svc.GetSubscription(f.ctx, "spc_a", s.ID)
	if g.Template.VersionID != v2.ID {
		t.Fatalf("follower did not move to v2: %+v", g.Template)
	}
	def.Subject = "evt.ops.release.{{vars.env}}"
	v3, err := f.svc.SaveDraft(f.ctx, admin, tm.ID, def, "new subject")
	must(t, err)
	if _, held, err = f.svc.Publish(f.ctx, admin, v3.ID); err != nil || len(held) != 1 || held[0] != s.ID {
		t.Fatalf("publish v3 should hold the follower: %v %v", held, err)
	}
	g, _ = f.svc.GetSubscription(f.ctx, "spc_a", s.ID)
	if g.Template.VersionID != v2.ID || g.Subject != "evt.ops.deploy.prod" {
		t.Fatalf("follower moved to a new subject: %+v", g)
	}
	if err := f.svc.DeleteConnection(f.ctx, admin, f.conn.ID); apperr.CodeOf(err) != apperr.CodePrecondition {
		t.Fatalf("deleted a connection in use: %v", err)
	}
}
