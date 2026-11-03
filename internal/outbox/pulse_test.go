package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aurimasniekis/go-pulse/pkg/pulse"
	"github.com/aurimasniekis/go-pulse/pkg/pulse/envelope"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/aurimasniekis/go-spaceman/internal/events/pulsetest"
	"github.com/aurimasniekis/go-spaceman/internal/store"
)

// flaky fails its first publish (as a lost acknowledgement would) and then delegates to the real client.
type flaky struct {
	Publisher
	fails int
}

func (f *flaky) Publish(ctx context.Context, subject, typ string, data any, opts ...pulse.PublishOption) (*pulse.PubAck, error) {
	if f.fails > 0 {
		f.fails--
		return nil, errors.New("nats: timeout")
	}
	return f.Publisher.Publish(ctx, subject, typ, data, opts...)
}

// Against the in-process Pulse-shaped NATS server (real nats-server JetStream, real go-pulse v0.1.3 SDK; simulated, not
// a pulsed deployment): events arrive on their subject with Nats-Msg-Id = the outbox ID on every attempt, ce-type,
// ce-source, ce-time = the enqueue time, and the SpaceMan correlation, causation and origin headers; a re-publish of
// the same ID inside the window is deduplicated by JetStream.
func TestPublishToPulse(t *testing.T) {
	srv := pulsetest.Start(t)
	creds := filepath.Join(t.TempDir(), "spaceman.creds")
	if err := os.WriteFile(creds, srv.Creds("spaceman"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, true)
	e.o.o.Source = "spaceman-test"
	dial := PulseDialer(PulseConfig{URL: srv.URL, CredsFile: creds, Source: "spaceman-test"}, nil)
	pub, closeFn, err := dial(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer closeFn()
	var id string
	if err := e.s.W().Tx(e.ctx, func(tx *store.Tx) error {
		id, err = e.o.Enqueue(e.ctx, tx, Event{Payload: HumanActionResolved{HumanAction{SpaceID: "spc_a", ActionID: "hum_1", Type: "question",
			Status: "resolved", Outcome: "answered", Priority: "normal", RequesterKind: "agent"}}, Correlation: "tsk_1", Causation: "req_42"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	created := e.clk.Now()
	fp := &flaky{Publisher: pub, fails: 1}
	if _, failed, err := e.o.Once(e.ctx, fp); err != nil || failed != 1 {
		t.Fatalf("first attempt %d %v", failed, err)
	}
	e.clk.Advance(5 * time.Second)
	if n, failed, err := e.o.Once(e.ctx, fp); err != nil || n != 1 || failed != 0 {
		t.Fatalf("retry %d %d %v", n, failed, err)
	}

	nc := srv.Conn("reader")
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	cons, err := js.OrderedConsumer(e.ctx, "EVENTS", jetstream.OrderedConsumerConfig{FilterSubjects: []string{"evt.spaceman.>"}})
	if err != nil {
		t.Fatal(err)
	}
	m, err := cons.Next(jetstream.FetchMaxWait(5 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	h := m.Headers()
	if m.Subject() != "evt.spaceman.human.action.resolved" || h.Get("Nats-Msg-Id") != id || h.Get("ce-type") != "spaceman.human.action_resolved.v1" ||
		h.Get("ce-source") != "spaceman-test" || h.Get(HeaderCorrelation) != "tsk_1" || h.Get(HeaderCausation) != "req_42" ||
		h.Get(HeaderOrigin) != "spaceman-test" || h.Get("content-type") != "application/json" || h.Get(HeaderDepth) != "" {
		t.Fatalf("message %s %v", m.Subject(), h)
	}
	if ts, err := time.Parse(time.RFC3339Nano, h.Get("ce-time")); err != nil || !ts.Equal(created) {
		t.Fatalf("ce-time %q, want %s", h.Get("ce-time"), created)
	}
	var body map[string]any
	if err := json.Unmarshal(m.Data(), &body); err != nil || body["action_id"] != "hum_1" || body["outcome"] != "answered" {
		t.Fatalf("body %s %v", m.Data(), err)
	}
	// A crash between publishing and marking publishes again with the same ID: JetStream drops the duplicate.
	ack, err := pub.Publish(e.ctx, m.Subject(), h.Get("ce-type"), json.RawMessage(m.Data()), pulse.WithID(id))
	if err != nil || !ack.Duplicate {
		t.Fatalf("duplicate not detected: %+v %v", ack, err)
	}
	// The SDK decodes the extension headers as Extra, which the inbound side checks for its own origin.
	if got := envelope.Decode(m.Subject(), h, m.Data()); got.Extra.Get(HeaderOrigin) != "spaceman-test" || got.ID != id {
		t.Fatalf("decoded %+v", got)
	}

	// An event caused by an ingested one carries its automation depth (ADR-0049, SM-EVT-016).
	if err := e.s.W().Tx(e.ctx, func(tx *store.Tx) error {
		_, err := e.o.Enqueue(e.ctx, tx, Event{Payload: InboxItemCreated{SpaceID: "spc_a", ItemID: "inb_1", SourceKind: "event"}, Depth: 3})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if n, _, err := e.o.Once(e.ctx, pub); err != nil || n != 1 {
		t.Fatalf("publish the deep event %d %v", n, err)
	}
	m, err = cons.Next(jetstream.FetchMaxWait(5 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if got := envelope.Decode(m.Subject(), m.Headers(), m.Data()); got.Extra.Get(HeaderDepth) != "3" || m.Subject() != "evt.spaceman.inbox.item.created" {
		t.Fatalf("depth header %v", m.Headers())
	}

	if _, _, err := PulseDialer(PulseConfig{URL: srv.URL, CredsFile: filepath.Join(t.TempDir(), "missing.creds")}, nil)(e.ctx); err == nil {
		t.Fatal("dialed without creds")
	}
	if _, _, err := PulseDialer(PulseConfig{URL: srv.URL, CredsFile: creds, CAFile: filepath.Join(t.TempDir(), "missing.pem")}, nil)(e.ctx); err == nil {
		t.Fatal("dialed with a missing CA file")
	}
}
