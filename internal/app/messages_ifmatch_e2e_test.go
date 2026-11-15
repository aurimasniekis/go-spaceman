package app_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/messages"
)

// SM-COMM-* over HTTP: If-Match on ack and resolve, as TestQueueOverHTTP does for move. The API refuses to send to
// operators, so the message is sent by a run through the service; the rest goes over HTTP.
func TestMessageIfMatchOverHTTP(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
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
	base := "/spaces/" + sp.ID + "/messages/"

	m, _, err := h.c.Messages.Send(ctx, audit.System, sp.ID, messages.From{Kind: "run", ID: "run_e2e", Label: "agent"},
		messages.SendInput{To: "operators", Kind: "request", Subject: "Need a key", Body: "Which key?"})
	if err != nil {
		t.Fatal(err)
	}
	var cur messages.Message
	root.do("GET", base+m.ID, nil).json(t, &cur)
	if cur.Version < 1 || cur.ToKind != "operators" {
		t.Fatalf("message %+v", cur)
	}
	v := cur.Version
	ifMatch := func(n int64) string { return fmt.Sprintf(`"%d"`, n) }
	unchanged := func(step string, want messages.Message) {
		t.Helper()
		var got messages.Message
		root.do("GET", base+m.ID, nil).json(t, &got)
		if got.State != want.State || got.Version != want.Version {
			t.Fatalf("%s changed the message: %+v (want %+v)", step, got, want)
		}
	}

	// Ack: a stale version is a 409 precondition_failed and changes nothing; a malformed one is a 422.
	expect(t, root.do("POST", base+m.ID+"/ack", nil, "If-Match", ifMatch(v+5)), 409)
	unchanged("a stale ack", cur)
	expect(t, root.do("POST", base+m.ID+"/ack", nil, "If-Match", "abc"), 422)
	unchanged("a malformed ack", cur)
	// The current version is honoured and bumps the message.
	var acked messages.Message
	expect(t, root.do("POST", base+m.ID+"/ack", nil, "If-Match", ifMatch(v)), 200)
	root.do("GET", base+m.ID, nil).json(t, &acked)
	if acked.State != "acknowledged" || acked.Version != v+1 {
		t.Fatalf("ack did not take the current version: %+v", acked)
	}

	// Resolve with the version the ack just made stale is a 409; the current one resolves.
	resolve := base + m.ID + "/resolve"
	expect(t, root.do("POST", resolve, map[string]any{"resolution": "done"}, "If-Match", ifMatch(v)), 409)
	unchanged("a stale resolve", acked)
	expect(t, root.do("POST", resolve, map[string]any{"resolution": "done"}, "If-Match", ifMatch(v+1)), 200)
	var resolved messages.Message
	root.do("GET", base+m.ID, nil).json(t, &resolved)
	if resolved.State != "resolved" || resolved.Resolution != "done" {
		t.Fatalf("resolve did not take the current version: %+v", resolved)
	}
}
