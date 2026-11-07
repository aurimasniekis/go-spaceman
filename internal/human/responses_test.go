package human

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/attachments"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/inbox"
)

func (f *fx) withInbox() *inbox.Service {
	f.t.Helper()
	att := attachments.New(f.s, attachments.NewLocal(f.t.TempDir()), nil, attachments.Options{})
	ib := inbox.New(f.s, audit.NewWriter(nil), changefeed.New(nil), att)
	f.svc.Inbox = ib
	return ib
}

func (f *fx) responseItems(ib *inbox.Service) []*inbox.Item {
	f.t.Helper()
	p, err := ib.List(f.ctx, "spc_a", "", "", 100)
	must(f.t, err)
	var out []*inbox.Item
	for _, it := range p.Items {
		if it.SourceKind == inbox.SourceHumanResponse {
			out = append(out, it)
		}
	}
	return out
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	must(t, err)
	return string(b)
}

func routedPolicy(mode string) PolicyFields {
	p := Builtin(TypeQuestion)
	p.ResponseInbox = mode
	return p
}

// SM-INBOX-001, ADR-0049: with response_inbox routed, a response routed to control or the operators also becomes an
// inbox item (source human_response) that names the action and its routing but never carries the response; responses
// for live requesters stay notifications; off (the default) creates none; always makes every response an item, for
// operators' questions too; a second routing never adds a second item.
func TestRoutedResponsesBecomeInboxItems(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	ib := f.withInbox()

	// Default (off): routed, but no item.
	a := f.create(question("Which port?"))
	f.end("run_1")
	if n := f.answer(a).Notifications[0]; n.TargetKind != TargetOperators {
		t.Fatalf("routing %+v", n)
	}
	if len(f.responseItems(ib)) != 0 {
		t.Fatal("off created an item")
	}
	if p, _ := f.svc.EffectivePolicies(f.ctx, "spc_a"); p[0].ResponseInbox != ResponseInboxOff {
		t.Fatalf("built-in response_inbox %q", p[0].ResponseInbox)
	}

	f.policy("spc_a", TypeQuestion, routedPolicy(ResponseInboxRouted))
	// A live requester: notification only.
	f.run("run_2", "worker", "worker", "ins_2", "running", "running")
	live := f.create(CreateInput{Type: TypeQuestion, Title: "live", Requester: Requester{Kind: "agent", ID: "ins_2", Role: "worker", Run: "run_2"}})
	f.answer(live)
	if len(f.responseItems(ib)) != 0 {
		t.Fatal("a response for a live requester became an item")
	}
	// An ended requester with a control run: routed to control, and an item.
	f.run("run_ctl", "control", "control", "ins_c", "running", "running")
	b := f.create(CreateInput{Type: TypeQuestion, Title: "Which region?", Priority: "high", Requester: Requester{Kind: "agent", ID: "ins_3", Role: "worker", Run: "run_gone"}})
	f.answer(b)
	items := f.responseItems(ib)
	if len(items) != 1 {
		t.Fatalf("items %+v", items)
	}
	it := items[0]
	if it.SourceID != b.ID || it.Title != "Response: Which region?" || it.Urgency != "high" || it.Trusted["routed_to"] != "control" ||
		it.Trusted["route_reason"] != ReasonRequesterEnded || it.Trusted["outcome"] != "answered" || it.Trusted["responded_by"] != op.ID ||
		!strings.Contains(it.Body, "spaceman_human_get") || it.CreatedBy != audit.System.ID {
		t.Fatalf("item %+v", it)
	}
	if strings.Contains(it.Body, responseMarker) || strings.Contains(it.Title, responseMarker) || strings.Contains(mustJSON(t, it.Trusted), responseMarker) {
		t.Fatal("the item carries the response")
	}
	// A person sending the undeliverable response of the first question to control makes its item now; routing the
	// second one again never adds a second item for it.
	if _, err := f.svc.RouteNotification(f.ctx, op, "spc_a", a.ID, f.get(a.ID).Notifications[0].ID, TargetControl); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.RouteNotification(f.ctx, op, "spc_a", b.ID, f.get(b.ID).Notifications[0].ID, TargetControl); err != nil {
		t.Fatal(err)
	}
	items = f.responseItems(ib)
	if len(items) != 2 || items[0].SourceID != a.ID && items[1].SourceID != a.ID {
		t.Fatalf("after routing again: %+v", items)
	}
	for _, it := range items {
		if it.SourceID == a.ID && it.Trusted["route_reason"] != ReasonOperator {
			t.Fatalf("operator routing %+v", it.Trusted)
		}
	}

	// always: an operator's own question becomes an item when answered; a cancellation does not.
	f.policy("spc_a", TypeQuestion, routedPolicy(ResponseInboxAlways))
	ask := f.create(CreateInput{Type: TypeQuestion, Title: "Ops question", Requester: Requester{Kind: "operator", ID: "usr_op"}})
	f.answer(ask)
	drop := f.create(CreateInput{Type: TypeQuestion, Title: "Dropped", Requester: Requester{Kind: "operator", ID: "usr_op"}})
	if _, err := f.svc.Cancel(f.ctx, op, "spc_a", drop.ID, 0, "no longer needed"); err != nil {
		t.Fatal(err)
	}
	items = f.responseItems(ib)
	var asked *inbox.Item
	for _, it := range items {
		if it.SourceID == ask.ID {
			asked = it
		}
		if it.SourceID == drop.ID {
			t.Fatal("a cancellation became an item")
		}
	}
	if len(items) != 3 || asked == nil || asked.Trusted["routed_to"] != nil || asked.Trusted["requester_kind"] != "operator" {
		t.Fatalf("always: %+v", items)
	}

	// The value is validated; older clients that omit it get off.
	_, err := f.svc.SetPolicy(f.ctx, op, "spc_a", TypeTodo, 0, routedPolicy("sometimes"))
	if apperr.CodeOf(err) != apperr.CodeInvalid {
		t.Fatalf("invalid response_inbox: %v", err)
	}
	p, err := f.svc.SetPolicy(f.ctx, op, "spc_a", TypeTodo, 0, routedPolicy(""))
	if err != nil || p.ResponseInbox != ResponseInboxOff {
		t.Fatalf("omitted response_inbox: %+v %v", p, err)
	}
}
