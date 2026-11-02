package pulsetest

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/aurimasniekis/go-pulse/pkg/pulse"
)

// The real go-pulse SDK publishes and consumes through a durable subscription on this server.
func TestDurableRoundTrip(t *testing.T) {
	s := Start(t)
	ctx := context.Background()
	pub, err := pulse.Connect(s.URL, pulse.WithCreds(s.Creds("publisher")), pulse.WithSource("pulse-github"))
	if err != nil {
		t.Fatal(err)
	}
	defer pub.Close()
	sub, err := pulse.Connect(s.URL, pulse.WithCreds(s.Creds("spaceman")))
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	if sub.ClientID() != "spaceman" {
		t.Fatalf("client id %q", sub.ClientID())
	}
	got := make(chan pulse.Event, 4)
	s1, err := sub.Subscribe(ctx, "evt.github.>", func(_ context.Context, e pulse.Event) error { got <- e; return nil }, pulse.Durable("sm-test"))
	if err != nil {
		t.Fatal(err)
	}
	defer s1.Stop()
	if s1.Consumer() != "spaceman_sm-test" {
		t.Fatalf("consumer %q", s1.Consumer())
	}
	ack, err := pub.Publish(ctx, "evt.github.example-org.webhook.issues.opened", "github.webhook.issues.opened.v1", json.RawMessage(`{"schema_version":1}`))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case e := <-got:
		if e.ID != ack.ID || e.Type != "github.webhook.issues.opened.v1" || e.Source != "pulse-github" || string(e.Data) != `{"schema_version":1}` {
			t.Fatalf("event %+v", e)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no event")
	}
}
