package app_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/aurimasniekis/go-spaceman/internal/api"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/config"
	"github.com/aurimasniekis/go-spaceman/internal/events/pulsetest"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	hns "github.com/aurimasniekis/go-spaceman/internal/harness"
	"github.com/aurimasniekis/go-spaceman/internal/outbox"
	"github.com/aurimasniekis/go-spaceman/internal/queue"
)

// SM-OBS-004/005 through the real central: a task created over HTTP is recorded in the outbox by the request's own
// transaction, the relay publishes it to the in-process Pulse-shaped server after commit with the task as correlation and
// the API request as causation, and the publication status is visible to global admins only.
func TestOutboundEventsOverHTTP(t *testing.T) {
	nats := pulsetest.Start(t)
	creds := filepath.Join(t.TempDir(), "spaceman.creds")
	if err := os.WriteFile(creds, nats.Creds("spaceman"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := newHarnessWith(t, func(c *config.Config) {
		c.Events.Outbound = config.OutboundConfig{Enabled: true, URL: nats.URL, CredsFile: creds, Source: "spaceman-e2e", Retention: config.Duration(time.Hour)}
	})
	ctx := context.Background()
	rootSecret, _, _ := h.c.Auth.IssueRootToken(ctx)
	root := h.client()
	root.bearer = rootSecret
	actor := audit.Actor{Type: audit.ActorUser, ID: "usr_test"}
	en, _ := h.c.Fleet.CreateNode(ctx, actor, fleet.CreateNodeInput{Name: "node-a"})
	sp, err := h.c.Fleet.CreateSpace(ctx, actor, fleet.CreateSpaceInput{Slug: "alpha", Name: "Alpha", NodeID: en.Node.ID})
	if err != nil {
		t.Fatal(err)
	}
	r := root.do("POST", "/spaces/"+sp.ID+"/tasks", map[string]any{"title": "Secret title", "objective": "Secret objective", "status": "ready"})
	expect(t, r, 201)
	var task queue.Task
	r.json(t, &task)
	reqID := r.header.Get(api.RequestIDHeader)

	var st outbox.Status
	root.do("GET", "/system/outbound-events", nil).json(t, &st)
	if !st.Enabled || st.Source != "spaceman-e2e" || st.Pending != 1 || st.Connected {
		t.Fatalf("before the relay runs: %+v", st)
	}

	rctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		h.c.Outbox.Run(rctx, outbox.PulseDialer(outbox.PulseConfig{URL: nats.URL, CredsFile: creds, Source: "spaceman-e2e"}, nil))
		close(done)
	}()
	t.Cleanup(func() { cancel(); <-done })
	deadline := time.Now().Add(10 * time.Second)
	for {
		root.do("GET", "/system/outbound-events", nil).json(t, &st)
		if st.Pending == 0 && st.Connected {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not published: %+v", st)
		}
		time.Sleep(20 * time.Millisecond)
	}

	nc := nats.Conn("reader")
	defer nc.Close()
	js, _ := jetstream.New(nc)
	cons, err := js.OrderedConsumer(ctx, "EVENTS", jetstream.OrderedConsumerConfig{FilterSubjects: []string{"evt.spaceman.queue.task.created"}})
	if err != nil {
		t.Fatal(err)
	}
	m, err := cons.Next(jetstream.FetchMaxWait(5 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	hd := m.Headers()
	if hd.Get("ce-type") != "spaceman.queue.task_created.v1" || hd.Get("ce-source") != "spaceman-e2e" || hd.Get(outbox.HeaderCorrelation) != task.ID ||
		hd.Get(outbox.HeaderCausation) != reqID || reqID == "" || hd.Get(outbox.HeaderOrigin) != "spaceman-e2e" {
		t.Fatalf("headers %v (request %s)", hd, reqID)
	}
	if b := string(m.Data()); !strings.Contains(b, task.ID) || strings.Contains(b, "Secret") {
		t.Fatalf("payload %s", b)
	}

	// Only global admins see the publication status.
	expect(t, root.do("POST", "/users", map[string]any{"email": "ada@example.com", "display_name": "Ada",
		"password": "correct horse battery staple", "grants": []map[string]string{{"role": "admin", "domain": "*"}}}), 201)
	viewer := h.client()
	viewer.bearer = loginAndIssueToken(t, h, "ada@example.com", []map[string]string{{"role": "operator", "domain": "*"}})
	expect(t, viewer.do("GET", "/system/outbound-events", nil), 403)
}

// SM-OBS-004: a run's lifecycle (launched, started once even across an agent restart, ended) is recorded in the
// transactions that change the run, with the run as correlation; nothing from the prompt reaches the payload.
func TestRunLifecycleEvents(t *testing.T) {
	e := newFleetEnv(t)
	e.cfg.Events.Outbound = config.OutboundConfig{Enabled: true, URL: "nats://127.0.0.1:4222", CredsFile: "/nonexistent/pulse.creds", Source: "spaceman",
		Retention: config.Duration(time.Hour)}
	e.restartCentral() // the relay is not started: rows stay pending for inspection
	ctx := context.Background()
	e.enroll()
	e.startAgent()
	nodes, _ := e.c.Fleet.ListNodes(ctx)
	sp, err := e.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "demo", Name: "Demo", NodeID: nodes[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	bindStubAccounts(t, e.c, sp.ID)
	eventually(t, "lease granted", func() bool { s, _ := e.c.Fleet.GetSpace(ctx, sp.ID); return s.Lease != nil && s.Lease.Valid })
	w, err := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Kind: "worker", Role: "dev", Harness: hns.KindCodex, Prompt: "PROMPT-SECRET"})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "worker running", func() bool { return e.run1(w.ID).Observed == "running" })
	e.stopAgent()
	eventually(t, "unknown after disconnect", func() bool { return e.run1(w.ID).Observed == "unknown" })
	e.startAgent()
	eventually(t, "running again", func() bool { return e.run1(w.ID).Observed == "running" })
	if _, err := e.c.Fleet.StopRun(ctx, admin, w.ID, "interrupt", "test"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "worker exited", func() bool { return e.run1(w.ID).Observed == "exited" })
	rows, err := e.c.Store.ReadDB().QueryContext(ctx, `SELECT type, correlation_id, payload_json FROM event_outbox WHERE type LIKE 'spaceman.fleet.%' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var typ, corr, payload string
		if err := rows.Scan(&typ, &corr, &payload); err != nil {
			t.Fatal(err)
		}
		if corr != w.ID || strings.Contains(payload, "PROMPT-SECRET") || !strings.Contains(payload, `"run_id":"`+w.ID+`"`) {
			t.Fatalf("%s %s %s", typ, corr, payload)
		}
		got = append(got, typ)
		if typ == "spaceman.fleet.run_ended.v1" && !strings.Contains(payload, `"state":"exited"`) {
			t.Fatalf("ended %s", payload)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "spaceman.fleet.run_launched.v1,spaceman.fleet.run_started.v1,spaceman.fleet.run_ended.v1" {
		t.Fatalf("run events %v", got)
	}
}
