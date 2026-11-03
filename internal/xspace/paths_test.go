package xspace

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/agenttools"
	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
)

// Every tool through the registry, as control runs call them: the space always comes from the credential.
func TestToolsAsControlCallsThem(t *testing.T) {
	f := newFx(t)
	f.allow("spc_a", "spc_b", Inbound{Discover: true, Questions: true, Delegation: true, Disclosure: DiscloseDetailed}, all)
	reg := agenttools.New(agenttools.Deps{Extra: []func(*agenttools.Registry){Tools(f.svc)}})
	call := func(p *fleet.RunPrincipal, tool string, args map[string]any, out any) error {
		t.Helper()
		raw, _ := json.Marshal(args)
		res, err := reg.Dispatch(f.ctx, p, tool, raw)
		if err == nil && out != nil {
			must(t, json.Unmarshal(res, out))
		}
		return err
	}
	var ds []Destination
	must(t, call(pa, "spaceman_xspace_discover", nil, &ds))
	if len(ds) != 1 || !ds[0].Questions || !ds[0].Delegation || ds[0].Knowledge {
		t.Fatalf("discover %+v", ds)
	}
	var made struct {
		RequestID string `json:"request_id"`
		Created   bool   `json:"created"`
	}
	must(t, call(pa, "spaceman_xspace_request", map[string]any{"to": "bravo", "kind": "delegation", "title": "Rotate", "body": "Please.", "dedup_key": "k1"}, &made))
	if !made.Created {
		t.Fatal("not created")
	}
	code(t, call(pa, "spaceman_xspace_request", map[string]any{"to": "bravo", "kind": "delegation", "title": "x", "surprise": true}, nil), apperr.CodeInvalid)
	var l []Request
	must(t, call(pb, "spaceman_xspace_list", map[string]any{"direction": "incoming"}, &l))
	if len(l) != 1 || l[0].ID != made.RequestID || l[0].Side != Incoming {
		t.Fatalf("bravo's list %+v", l)
	}
	var r Request
	must(t, call(pb, "spaceman_xspace_accept", map[string]any{"request_id": made.RequestID, "task": map[string]any{"title": "Rotate the key for alpha", "priority": "high"}}, &r))
	if r.State != Accepted || r.LocalTaskID == "" || r.DecidedBy != ctlB {
		t.Fatalf("accepted %+v", r)
	}
	must(t, call(pb, "spaceman_xspace_reply", map[string]any{"request_id": made.RequestID, "outcome": "failed", "reply": "The key is managed elsewhere."}, &r))
	r = Request{}
	must(t, call(pa, "spaceman_xspace_get", map[string]any{"request_id": made.RequestID}, &r))
	if r.State != Failed || r.Reply != "The key is managed elsewhere." || r.LocalTaskID != "" {
		t.Fatalf("alpha's view %+v", r)
	}
	must(t, call(pa, "spaceman_xspace_request", map[string]any{"to": "bravo", "kind": "question", "title": "Q2"}, &made))
	must(t, call(pb, "spaceman_xspace_decline", map[string]any{"request_id": made.RequestID, "reason": "Out of scope"}, &r))
	must(t, call(pa, "spaceman_xspace_request", map[string]any{"to": "bravo", "kind": "question", "title": "Q3"}, &made))
	must(t, call(pa, "spaceman_xspace_cancel", map[string]any{"request_id": made.RequestID, "reason": "never mind"}, &r))
	if r.State != Cancelled {
		t.Fatalf("cancelled %+v", r)
	}
	must(t, call(pa, "spaceman_xspace_list", map[string]any{"all": true}, &l))
	if len(l) != 3 {
		t.Fatalf("alpha's requests %d", len(l))
	}
	must(t, call(pa, "spaceman_xspace_list", nil, &l))
	if len(l) != 0 {
		t.Fatalf("open requests %+v", l)
	}
	// Bravo's control cannot read alpha's view of a request that is not bravo's, nor act as alpha.
	code(t, call(pc, "spaceman_xspace_get", map[string]any{"request_id": made.RequestID}, nil), apperr.CodeNotFound)
	code(t, call(pb, "spaceman_xspace_cancel", map[string]any{"request_id": made.RequestID}, nil), apperr.CodeForbidden)
}

// Listings by direction and state, the global listings and the global view's chain.
func TestListings(t *testing.T) {
	f := newFx(t)
	f.allow("spc_a", "spc_b", Inbound{Questions: true, Delegation: true}, all)
	f.allow("spc_b", "spc_c", Inbound{Questions: true}, all)
	root, err := f.ask(pa, RequestInput{To: "bravo", Kind: Delegation, Title: "Root"})
	must(t, err)
	hop, err := f.ask(pb, RequestInput{To: "charlie", Kind: Question, Title: "Hop"})
	must(t, err)
	done, err := f.ask(pa, RequestInput{To: "bravo", Kind: Question, Title: "Done", CausedBy: ""})
	must(t, err)
	ok(t)(f.svc.Reply(f.ctx, op, "spc_b", done.ID, "", "yes"))
	for _, tc := range []struct {
		dir  string
		open bool
		want int
	}{{"", false, 3}, {Outgoing, false, 1}, {Incoming, false, 2}, {Incoming, true, 1}, {"", true, 2}} {
		l, err := f.svc.List(f.ctx, "spc_b", tc.dir, tc.open, 0)
		must(t, err)
		if len(l) != tc.want {
			t.Fatalf("bravo %q open=%v: %d", tc.dir, tc.open, len(l))
		}
	}
	if l, _ := f.svc.List(f.ctx, "spc_b", "", false, 1); len(l) != 1 {
		t.Fatalf("limit: %d", len(l))
	}
	_, err = f.svc.List(f.ctx, "spc_b", "sideways", false, 0)
	code(t, err, apperr.CodeInvalid)
	every, err := f.svc.ListAll(f.ctx, false, 0)
	must(t, err)
	if len(every) != 3 || every[0].Side != "" {
		t.Fatalf("all %+v", every)
	}
	if open, _ := f.svc.ListAll(f.ctx, true, 10); len(open) != 2 {
		t.Fatalf("open %d", len(open))
	}
	g, err := f.svc.GetAny(f.ctx, hop.ID)
	must(t, err)
	if strings.Join(g.Chain, ",") != "spc_a,spc_b,spc_c" || g.CausedBy != root.ID || g.InboxItemID == "" {
		t.Fatalf("global view %+v", g)
	}
	if out, _ := f.svc.Get(f.ctx, "spc_b", hop.ID); out.Chain != nil {
		t.Fatalf("a space sees the chain: %v", out.Chain)
	}
	_, err = f.svc.GetAny(f.ctx, "xrq_nope")
	code(t, err, apperr.CodeNotFound)
	peers, err := f.svc.ListPeers(f.ctx, "spc_b")
	must(t, err)
	if len(peers) != 2 {
		t.Fatalf("bravo's peers %+v", peers)
	}
	everyPeer, err := f.svc.ListAllPeers(f.ctx)
	must(t, err)
	if len(everyPeer) != 4 {
		t.Fatalf("all peers %d", len(everyPeer))
	}
}

// Invalid input and wrong-state calls are refused without changing anything.
func TestErrorPaths(t *testing.T) {
	f := newFx(t)
	f.allow("spc_a", "spc_b", Inbound{Questions: true, Delegation: true}, all)
	f.allow("spc_a", "spc_c", Inbound{Knowledge: true}, all)
	for _, in := range []RequestInput{
		{To: "bravo", Kind: "gossip", Title: "x"},
		{To: "bravo", Kind: Question},
		{To: "bravo", Kind: Question, Title: strings.Repeat("t", MaxTitle+1)},
		{To: "bravo", Kind: Question, Title: "x", Body: strings.Repeat("b", MaxBody+1)},
		{To: "bravo", Kind: Question, Title: "x", KB: "payments"},
		{To: "charlie", Kind: Knowledge, Title: "x", Body: "?", KB: "Not A Slug", KBKind: "decision"},
		{To: "charlie", Kind: Knowledge, Title: "x", Body: "?", KB: "payments", KBKind: "gossip"},
		{To: "charlie", Kind: Knowledge, Title: "x", KB: "payments", KBKind: "decision"},
		{To: "bravo", Kind: Question, Title: "x", DeadlineMinutes: 8 * 24 * 60},
		{To: "bravo", Kind: Question, Title: "x", CausedBy: "nope"},
		{To: "bravo", Kind: Question, Title: "x", CausedBy: "xrq_01aaaaaaaaaaaaaaaaaaaaaaaa", Independent: true},
		{To: "bravo", Kind: Question, Title: "x", DedupKey: strings.Repeat("k", 257)},
	} {
		_, err := f.ask(pa, in)
		code(t, err, apperr.CodeInvalid)
	}
	// A KB that does not exist, or is disabled, in the destination.
	_, err := f.ask(pa, RequestInput{To: "charlie", Kind: Knowledge, Title: "x", Body: "?", KB: "nothing", KBKind: "decision"})
	code(t, err, apperr.CodeNotFound)
	f.exec(`UPDATE kb_repos SET enabled = 0 WHERE slug = 'payments'`)
	_, err = f.ask(pa, RequestInput{To: "charlie", Kind: Knowledge, Title: "x", Body: "?", KB: "payments", KBKind: "decision"})
	code(t, err, apperr.CodePrecondition)

	r, err := f.ask(pa, RequestInput{To: "bravo", Kind: Question, Title: "Open"})
	must(t, err)
	// Wrong side, unknown request, invalid reply.
	for _, err := range []error{
		second(f.svc.Accept(f.ctx, op, "spc_c", r.ID, nil)),
		second(f.svc.Decline(f.ctx, op, "spc_c", "xrq_nope", "")),
		second(f.svc.Cancel(f.ctx, op, "spc_c", r.ID, "")),
	} {
		code(t, err, apperr.CodeNotFound)
	}
	code(t, second(f.svc.Decline(f.ctx, op, "spc_a", r.ID, "")), apperr.CodeForbidden)
	code(t, second(f.svc.Reply(f.ctx, op, "spc_b", r.ID, "maybe", "x")), apperr.CodeInvalid)
	code(t, second(f.svc.Reply(f.ctx, op, "spc_b", r.ID, "", strings.Repeat("x", 100_001))), apperr.CodeInvalid)
	ok(t)(f.svc.Accept(f.ctx, op, "spc_b", r.ID, nil))
	code(t, second(f.svc.Accept(f.ctx, op, "spc_b", r.ID, nil)), apperr.CodePrecondition)
	code(t, second(f.svc.Decline(f.ctx, op, "spc_b", r.ID, "")), apperr.CodePrecondition) // accepted: reply failed instead
	ok(t)(f.svc.Reply(f.ctx, op, "spc_b", r.ID, Completed, "done"))
	code(t, second(f.svc.Cancel(f.ctx, op, "spc_a", r.ID, "")), apperr.CodePrecondition)
	// A closed request cannot cause another.
	f.allow("spc_b", "spc_c", Inbound{Questions: true}, all)
	_, err = f.ask(pb, RequestInput{To: "charlie", Kind: Question, Title: "x", CausedBy: r.ID})
	code(t, err, apperr.CodePrecondition)
	// An archived destination takes nothing.
	f.exec(`UPDATE spaces SET state = 'archived' WHERE id = 'spc_b'`)
	_, err = f.ask(pa, RequestInput{To: "bravo", Kind: Question, Title: "x"})
	code(t, err, apperr.CodePrecondition)
	if ds, _ := f.svc.Discover(f.ctx, "spc_a"); len(ds) != 0 {
		t.Fatalf("archived space discovered: %+v", ds)
	}
	// Settings and peers of spaces that do not exist.
	_, err = f.svc.GetSettings(f.ctx, "spc_zz")
	code(t, err, apperr.CodeNotFound)
	_, err = f.svc.SaveSettings(f.ctx, op, "spc_zz", 0, SettingsInput{})
	code(t, err, apperr.CodeNotFound)
	_, err = f.svc.SaveSettings(f.ctx, op, "spc_a", 0, SettingsInput{MaxOutgoingPerHour: 5000})
	code(t, err, apperr.CodeInvalid)
	_, err = f.svc.SaveSettings(f.ctx, op, "spc_a", 0, SettingsInput{Profile: strings.Repeat("p", 2001)})
	code(t, err, apperr.CodeInvalid)
	_, err = f.svc.SavePeer(f.ctx, op, "spc_zz", "spc_a", 0, PeerInput{})
	code(t, err, apperr.CodeNotFound)
	for _, in := range []Inbound{{MaxPerHour: 5000}, {MaxPending: 500}, {KBs: []string{"Bad Slug"}}, {KBs: make([]string, 51)}} {
		_, err = f.svc.SavePeer(f.ctx, op, "spc_a", "spc_c", 0, PeerInput{Inbound: in})
		code(t, err, apperr.CodeInvalid)
	}
	_, err = f.svc.GetPeer(f.ctx, "spc_a", "spc_zz")
	code(t, err, apperr.CodeNotFound)
}

func second(_ *Request, err error) error { return err }

// Reasons and KB failures reach the source only when the destination's level allows text; a deleted peer row
// discloses nothing.
func TestReasonsAndFailuresFollowDisclosure(t *testing.T) {
	f := newFx(t)
	f.allow("spc_a", "spc_b", Inbound{Questions: true, Disclosure: DiscloseSummary}, all)
	r, err := f.ask(pa, RequestInput{To: "bravo", Kind: Question, Title: "Why?"})
	must(t, err)
	ok(t)(f.svc.Decline(f.ctx, op, "spc_b", r.ID, "  "+strings.Repeat("r", MaxReason+50)))
	if out, _ := f.svc.Get(f.ctx, "spc_a", r.ID); len(out.Reason) != MaxReason {
		t.Fatalf("reason at summary: %d chars", len(out.Reason))
	}
	// A failed delegation's reason text is the reply, under the same rule; at none it is withheld.
	r2, err := f.ask(pa, RequestInput{To: "bravo", Kind: Question, Title: "Fail?"})
	must(t, err)
	f.setPeer("spc_b", "spc_a", Inbound{Questions: true, Disclosure: DiscloseNone}, nil)
	ok(t)(f.svc.Reply(f.ctx, op, "spc_b", r2.ID, Failed, "Blocked by our customer ACME"))
	if out, _ := f.svc.Get(f.ctx, "spc_a", r2.ID); out.Reply != "" || out.State != Failed || out.Disclosure != DiscloseNone {
		t.Fatalf("failure at none %+v", out)
	}
	// The destination removes its row while a request is open: the reply then discloses nothing.
	f.setPeer("spc_b", "spc_a", Inbound{Questions: true, Disclosure: DiscloseDetailed}, nil)
	r3, err := f.ask(pa, RequestInput{To: "bravo", Kind: Question, Title: "Later?"})
	must(t, err)
	must(t, f.svc.DeletePeer(f.ctx, op, "spc_b", "spc_a"))
	ok(t)(f.svc.Reply(f.ctx, op, "spc_b", r3.ID, Completed, "internal detail"))
	if out, _ := f.svc.Get(f.ctx, "spc_a", r3.ID); out.Reply != "" || out.Disclosure != DiscloseNone {
		t.Fatalf("reply after the row was deleted %+v", out)
	}
	_, err = f.ask(pa, RequestInput{To: "bravo", Kind: Question, Title: "Again?"})
	code(t, err, apperr.CodeNotFound)

	// A KB request an operator of the destination withdraws fails for the source; the reason says only that.
	f.allow("spc_a", "spc_c", Inbound{Knowledge: true, Disclosure: DiscloseSummary}, all)
	k, err := f.ask(pa, RequestInput{To: "charlie", Kind: Knowledge, Title: "W", Body: "Which window?", KB: "payments", KBKind: "decision"})
	must(t, err)
	in, _ := f.svc.Get(f.ctx, "spc_c", k.ID)
	must(t, f.kb.Cancel(f.ctx, op, nil, "spc_c", in.KBRequestID))
	out, _ := f.svc.Get(f.ctx, "spc_a", k.ID)
	if out.State != Failed || out.Reason != "the knowledge base did not answer (cancelled)" {
		t.Fatalf("KB withdrawn %+v", out)
	}
	if f.messageFor("spc_a", k.ID, Failed) == nil {
		t.Fatal("source not told of the failure")
	}
	// At none, that reason is withheld too.
	f.setPeer("spc_c", "spc_a", Inbound{Knowledge: true, Disclosure: DiscloseNone}, nil)
	k2, err := f.ask(pa, RequestInput{To: "charlie", Kind: Knowledge, Title: "W2", Body: "?", KB: "payments", KBKind: "context"})
	must(t, err)
	in2, _ := f.svc.Get(f.ctx, "spc_c", k2.ID)
	must(t, f.kb.Cancel(f.ctx, op, nil, "spc_c", in2.KBRequestID))
	if out, _ := f.svc.Get(f.ctx, "spc_a", k2.ID); out.State != Failed || out.Reason != "" {
		t.Fatalf("KB failure at none %+v", out)
	}
	// Expiry of a knowledge request withdraws its KB request.
	f.setPeer("spc_c", "spc_a", Inbound{Knowledge: true, Disclosure: DiscloseDetailed}, nil)
	k3, err := f.ask(pa, RequestInput{To: "charlie", Kind: Knowledge, Title: "W3", Body: "?", KB: "payments", KBKind: "context", DeadlineMinutes: 5})
	must(t, err)
	f.clk.Advance(6 * time.Minute)
	if n, err := f.svc.Expire(f.ctx); err != nil || n != 1 {
		t.Fatalf("expire %d %v", n, err)
	}
	in3, _ := f.svc.Get(f.ctx, "spc_c", k3.ID)
	if kq, _ := f.s.R().GetKBRequest(f.ctx, in3.KBRequestID); kq.State != "cancelled" {
		t.Fatalf("KB request after expiry %s", kq.State)
	}
	// An answer arriving after the request closed changes nothing.
	f.svc.OnKBDone(f.ctx, in3.KBRequestID)
	f.svc.OnKBDone(f.ctx, "kbq_unknown")
	if out, _ := f.svc.Get(f.ctx, "spc_a", k3.ID); out.State != Expired {
		t.Fatalf("after a late answer %+v", out)
	}
}
