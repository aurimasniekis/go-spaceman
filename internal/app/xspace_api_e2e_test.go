package app_test

import (
	"context"
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/xspace"
)

// ADR-0038 over HTTP: each space manages its own side (settings, peers with If-Match, delete); operators withdraw
// outgoing and decline incoming requests; each space sees the request from its side only; global administrators list
// everything; a viewer reads but changes nothing.
func TestCrossSpaceOverHTTP(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	rootSecret, _, _ := h.c.Auth.IssueRootToken(ctx)
	root := h.client()
	root.bearer = rootSecret
	en, _ := h.c.Fleet.CreateNode(ctx, admin, fleet.CreateNodeInput{Name: "node-a"})
	mk := func(slug string) *fleet.Space {
		sp, err := h.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: slug, Name: strings.ToUpper(slug[:1]) + slug[1:], NodeID: en.Node.ID})
		if err != nil {
			t.Fatal(err)
		}
		return sp
	}
	a, b := mk("alpha"), mk("bravo")

	// Settings: created without If-Match, then versioned.
	var st xspace.Settings
	root.do("GET", "/spaces/"+a.ID+"/xspace/settings", nil).json(t, &st)
	if st.Version != 0 || st.MaxDepth != xspace.DefaultMaxDepth {
		t.Fatalf("default settings %+v", st)
	}
	root.do("PUT", "/spaces/"+a.ID+"/xspace/settings", map[string]any{"profile": "Alpha team", "max_depth": 2}).json(t, &st)
	if st.Version != 1 || st.Profile != "Alpha team" {
		t.Fatalf("saved settings %+v", st)
	}
	expect(t, root.do("PUT", "/spaces/"+a.ID+"/xspace/settings", map[string]any{"max_depth": 9}, "If-Match", `"1"`), 422)
	expect(t, root.do("PUT", "/spaces/"+a.ID+"/xspace/settings", map[string]any{"profile": "x"}, "If-Match", `"7"`), 409)

	// Peers: bravo accepts questions from alpha with summaries; alpha may send questions.
	none := map[string]any{"discover": false, "questions": false, "delegation": false, "knowledge": false, "kbs": []string{}, "disclosure": "none",
		"max_per_hour": 20, "max_pending": 5}
	in := map[string]any{}
	for k, v := range none {
		in[k] = v
	}
	in["questions"], in["discover"], in["disclosure"] = true, true, "summary"
	var p xspace.Peer
	r := root.do("PUT", "/spaces/"+b.ID+"/xspace/peers/"+a.ID, map[string]any{"inbound": in, "outbound": map[string]bool{"questions": false, "delegation": false, "knowledge": false}})
	expect(t, r, 200)
	r.json(t, &p)
	if p.Version != 1 || !p.Inbound.Questions || p.Peer.Slug != "alpha" || r.header.Get("ETag") != `"1"` {
		t.Fatalf("peer %+v %v", p, r.header)
	}
	expect(t, root.do("PUT", "/spaces/"+b.ID+"/xspace/peers/"+a.ID, map[string]any{"inbound": in, "outbound": map[string]bool{"questions": false, "delegation": false, "knowledge": false}}), 409)
	expect(t, root.do("PUT", "/spaces/"+a.ID+"/xspace/peers/"+b.ID, map[string]any{"inbound": none, "outbound": map[string]bool{"questions": true, "delegation": false, "knowledge": false}}), 200)
	expect(t, root.do("PUT", "/spaces/"+a.ID+"/xspace/peers/"+a.ID, map[string]any{"inbound": none, "outbound": map[string]bool{"questions": true, "delegation": false, "knowledge": false}}), 422)
	var peers struct{ Items []xspace.Peer }
	root.do("GET", "/spaces/"+b.ID+"/xspace/peers", nil).json(t, &peers)
	if len(peers.Items) != 1 || peers.Items[0].Peer.ID != a.ID {
		t.Fatalf("bravo's peers %+v", peers)
	}
	root.do("GET", "/xspace/peers", nil).json(t, &peers)
	if len(peers.Items) != 2 {
		t.Fatalf("all peers %+v", peers)
	}

	// Requests come from control (the service, as its tool calls it); operators act on them over HTTP.
	ctl := &fleet.RunPrincipal{RunID: "run_01aaaaaaaaaaaaaaaaaaaaaaaa", SpaceID: a.ID, Kind: "control", Role: "control"}
	ask := func(title string) *xspace.Request {
		t.Helper()
		req, _, err := h.c.XSpace.Create(ctx, ctl, xspace.RequestInput{To: "bravo", Kind: xspace.Question, Title: title, Independent: true})
		if err != nil {
			t.Fatal(err)
		}
		return req
	}
	q1, q2 := ask("Which retry budget?"), ask("Where is the runbook?")
	var got xspace.Request
	root.do("GET", "/spaces/"+b.ID+"/xspace/requests/"+q1.ID, nil).json(t, fresh(&got))
	if got.Side != xspace.Incoming || got.InboxItemID == "" {
		t.Fatalf("incoming %+v", got)
	}
	root.do("GET", "/spaces/"+a.ID+"/xspace/requests/"+q1.ID, nil).json(t, fresh(&got))
	if got.Side != xspace.Outgoing || got.InboxItemID != "" {
		t.Fatalf("outgoing %+v", got)
	}
	var list struct{ Items []xspace.Request }
	root.do("GET", "/spaces/"+b.ID+"/xspace/requests?direction=outgoing", nil).json(t, freshList(&list))
	if len(list.Items) != 0 {
		t.Fatalf("bravo sent nothing: %+v", list)
	}
	root.do("GET", "/spaces/"+b.ID+"/xspace/requests?direction=incoming&open=true", nil).json(t, freshList(&list))
	if len(list.Items) != 2 {
		t.Fatalf("bravo's incoming %+v", list)
	}
	expect(t, root.do("GET", "/spaces/"+b.ID+"/xspace/requests?direction=sideways", nil), 422)

	// An operator of bravo declines; with summaries, alpha sees the reason. An operator of alpha withdraws the other.
	expect(t, root.do("POST", "/spaces/"+a.ID+"/xspace/requests/"+q1.ID+"/decline", map[string]string{"reason": "x"}), 403)
	root.do("POST", "/spaces/"+b.ID+"/xspace/requests/"+q1.ID+"/decline", map[string]string{"reason": "Ask the platform team"}).json(t, fresh(&got))
	if got.State != xspace.Declined {
		t.Fatalf("declined %+v", got)
	}
	root.do("GET", "/spaces/"+a.ID+"/xspace/requests/"+q1.ID, nil).json(t, fresh(&got))
	if got.Reason != "Ask the platform team" || got.Disclosure != xspace.DiscloseSummary {
		t.Fatalf("decline reason at summary %+v", got)
	}
	expect(t, root.do("POST", "/spaces/"+b.ID+"/xspace/requests/"+q2.ID+"/cancel", map[string]string{}), 403)
	root.do("POST", "/spaces/"+a.ID+"/xspace/requests/"+q2.ID+"/cancel", map[string]string{"reason": "Found it"}).json(t, fresh(&got))
	if got.State != xspace.Cancelled {
		t.Fatalf("cancelled %+v", got)
	}
	expect(t, root.do("POST", "/spaces/"+a.ID+"/xspace/requests/"+q2.ID+"/cancel", map[string]string{}), 409)
	root.do("GET", "/xspace/requests?open=false", nil).json(t, freshList(&list))
	if len(list.Items) != 2 || len(list.Items[0].Chain) != 2 {
		t.Fatalf("global listing %+v", list)
	}

	// A viewer of bravo reads but cannot change bravo's side.
	expect(t, root.do("POST", "/users", map[string]any{"email": "vic@example.com", "display_name": "Vic", "password": "correct horse battery staple",
		"grants": []map[string]string{{"role": "viewer", "domain": b.ID}}}), 201)
	viewer := h.client()
	viewer.bearer = loginAndIssueToken(t, h, "vic@example.com", []map[string]string{{"role": "viewer", "domain": b.ID}})
	expect(t, viewer.do("GET", "/spaces/"+b.ID+"/xspace/requests", nil), 200)
	expect(t, viewer.do("DELETE", "/spaces/"+b.ID+"/xspace/peers/"+a.ID, nil), 403)
	expect(t, viewer.do("GET", "/xspace/requests", nil), 403)
	expect(t, viewer.do("GET", "/spaces/"+a.ID+"/xspace/requests", nil), 404) // a space it cannot see

	// Deleting the peer row ends what alpha may do in bravo.
	expect(t, root.do("DELETE", "/spaces/"+b.ID+"/xspace/peers/"+a.ID, nil), 204)
	expect(t, root.do("DELETE", "/spaces/"+b.ID+"/xspace/peers/"+a.ID, nil), 404)
	if _, _, err := h.c.XSpace.Create(ctx, ctl, xspace.RequestInput{To: "bravo", Kind: xspace.Question, Title: "Again?"}); err == nil {
		t.Fatal("asked after the peer row was deleted")
	}
}

func fresh(r *xspace.Request) *xspace.Request { *r = xspace.Request{}; return r }

func freshList(l *struct{ Items []xspace.Request }) *struct{ Items []xspace.Request } {
	l.Items = nil
	return l
}
