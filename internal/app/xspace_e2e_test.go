package app_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/catalog"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	hns "github.com/aurimasniekis/go-spaceman/internal/harness"
	"github.com/aurimasniekis/go-spaceman/internal/kb"
	"github.com/aurimasniekis/go-spaceman/internal/messages"
	"github.com/aurimasniekis/go-spaceman/internal/queue"
	"github.com/aurimasniekis/go-spaceman/internal/xspace"
)

// SM-XSPACE-001…003, ADR-0038 end to end with a real agent, three spaces and their control runs on the real bridge:
// a worker cannot reach another space; alpha's control discovers bravo and asks it to do work; bravo's control is
// woken by its inbox, accepts and creates its own task, and replies; alpha's control is notified and reads the reply
// as bravo's disclosure level allows; the request survives a central restart; a knowledge request to charlie is
// answered by a support session of charlie's KB whose requester is alpha's control run.
func TestCrossSpaceRequests(t *testing.T) {
	// Not parallel: central restarts must finish within the node lease, which parallel tests can starve.
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	e := newFleetEnv(t, longLease)
	e.enroll()
	e.startAgent()
	ctx := context.Background()
	nodes, _ := e.c.Fleet.ListNodes(ctx)
	eventually(t, "node connected", func() bool { n, _ := e.c.Fleet.GetNode(ctx, nodes[0].ID); return n.Connected })
	spaces := map[string]*fleet.Space{}
	ctl := map[string]*fleet.Run{}
	for _, slug := range []string{"alpha", "bravo", "charlie"} {
		sp, err := e.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: slug, Name: strings.ToUpper(slug[:1]) + slug[1:], NodeID: nodes[0].ID})
		if err != nil {
			t.Fatal(err)
		}
		bindStubAccounts(t, e.c, sp.ID)
		eventually(t, slug+" lease", func() bool { s, _ := e.c.Fleet.GetSpace(ctx, sp.ID); return s.Lease != nil && s.Lease.Valid })
		r, err := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Kind: "control", Role: "control", Harness: hns.KindClaude})
		if err != nil {
			t.Fatal(err)
		}
		spaces[slug], ctl[slug] = sp, r
	}
	a, b, c := spaces["alpha"], spaces["bravo"], spaces["charlie"]
	wrk, err := e.c.Fleet.LaunchRun(ctx, admin, a.ID, fleet.LaunchInput{Kind: "worker", Role: "implementor", Harness: hns.KindClaude})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []*fleet.Run{ctl["alpha"], ctl["bravo"], ctl["charlie"], wrk} {
		eventually(t, "run running", func() bool { return e.run1(r.ID).Observed == "running" })
	}
	term := func(id string) string { return e.run1(id).TerminalID }
	tok := map[string]string{}
	for slug, r := range ctl {
		tok[slug] = e.runToken(r.ID)
	}
	call := func(slug, tool string, args any) toolResult {
		t.Helper()
		return e.bridgeCall(ctl[slug].ID, tok[slug], tool, args)
	}
	nctx, stop := context.WithCancel(ctx)
	t.Cleanup(stop)
	go e.c.Notifier.Run(nctx)

	// Alpha may ask bravo questions and delegate work; bravo discloses summaries. Charlie lets alpha ask its KB.
	mustPeer := func(space, peer string, in xspace.PeerInput) {
		t.Helper()
		if _, err := e.c.XSpace.SavePeer(ctx, admin, space, peer, 0, in); err != nil {
			t.Fatal(err)
		}
	}
	mustPeer(b.ID, a.ID, xspace.PeerInput{Inbound: xspace.Inbound{Discover: true, Questions: true, Delegation: true, Disclosure: xspace.DiscloseSummary}})
	mustPeer(a.ID, b.ID, xspace.PeerInput{Outbound: xspace.Outbound{Questions: true, Delegation: true}})
	mustPeer(c.ID, a.ID, xspace.PeerInput{Inbound: xspace.Inbound{Knowledge: true, KBs: []string{"payments"}, Disclosure: xspace.DiscloseDetailed}})
	mustPeer(a.ID, c.ID, xspace.PeerInput{Outbound: xspace.Outbound{Knowledge: true}})

	// A worker has no cross-space tools at all.
	if r := e.bridgeCall(wrk.ID, e.runToken(wrk.ID), "spaceman_xspace_request", map[string]any{"to": "bravo", "kind": "question", "title": "x"}); r.OK || r.Code != "forbidden" {
		t.Fatalf("worker request: %+v", r)
	}
	res := call("alpha", "spaceman_xspace_discover", nil)
	var ds []xspace.Destination
	_ = json.Unmarshal(res.Result, &ds)
	if !res.OK || len(ds) != 1 || ds[0].Space.ID != b.ID || !ds[0].Delegation || ds[0].Disclosure != xspace.DiscloseSummary {
		t.Fatalf("discover: %+v", res)
	}
	res = call("alpha", "spaceman_xspace_request", map[string]any{"to": "bravo", "kind": "delegation", "title": "Rotate the shared key",
		"body": "Our integration with bravo uses the shared key. Please rotate it and tell us when done.", "deadline_minutes": 120})
	var created struct {
		RequestID string `json:"request_id"`
		State     string `json:"state"`
	}
	_ = json.Unmarshal(res.Result, &created)
	if !res.OK || created.State != xspace.Pending {
		t.Fatalf("request: %+v", res)
	}

	// Bravo's control is woken (idle-gated notice) by the new inbox item; the notice carries no content.
	e.run.SetStatus(term(ctl["bravo"].ID), "idle")
	eventually(t, "bravo control noticed", func() bool { return len(e.run.Inputs(term(ctl["bravo"].ID))) >= 1 })
	if in := e.run.Inputs(term(ctl["bravo"].ID))[0]; strings.Contains(in, "shared key") {
		t.Fatalf("notice carries content: %q", in)
	}

	// Central restarts: the request, its inbox item and the notification are durable.
	stop()
	e.restartCentral()
	eventually(t, "reconnected", func() bool { n, _ := e.c.Fleet.GetNode(ctx, nodes[0].ID); return n.Connected })
	nctx, stop = context.WithCancel(ctx)
	t.Cleanup(stop)
	go e.c.Notifier.Run(nctx)

	res = call("bravo", "spaceman_inbox_list", map[string]any{"status": "new"})
	if !res.OK || !strings.Contains(string(res.Result), "Delegated work from space Alpha") {
		t.Fatalf("bravo inbox: %+v", res)
	}
	res = call("bravo", "spaceman_xspace_accept", map[string]any{"request_id": created.RequestID, "task": map[string]any{"objective": "Rotate the key", "ready": true}})
	var acc xspace.Request
	_ = json.Unmarshal(res.Result, &acc)
	if !res.OK || acc.State != xspace.Accepted || acc.LocalTaskID == "" {
		t.Fatalf("accept: %+v", res)
	}
	task, err := e.c.Queue.Get(ctx, b.ID, acc.LocalTaskID)
	if err != nil || task.Status != queue.Ready || task.OriginID != created.RequestID {
		t.Fatalf("bravo's own task %+v %v", task, err)
	}
	// Alpha cannot decide bravo's side, nor see bravo's task.
	if r := call("alpha", "spaceman_xspace_reply", map[string]any{"request_id": created.RequestID, "reply": "done"}); r.OK || r.Code != "forbidden" {
		t.Fatalf("source replied: %+v", r)
	}
	if r := call("alpha", "spaceman_task_get", map[string]any{"task_id": acc.LocalTaskID}); r.OK {
		t.Fatalf("alpha read bravo's task: %+v", r)
	}
	res = call("bravo", "spaceman_xspace_reply", map[string]any{"request_id": created.RequestID, "reply": "Rotated; the new key is in your secret store as INTEGRATION_KEY. " + strings.Repeat("Details. ", 400)})
	if !res.OK {
		t.Fatalf("reply: %+v", res)
	}

	// Alpha's control reads the notification (provenance: from bravo, by request ID) and the reply, cut to a summary.
	var inb messages.Inbox
	res = call("alpha", "spaceman_messages", nil)
	_ = json.Unmarshal(res.Result, &inb)
	found := false
	for _, m := range inb.Messages {
		if m.CorrelationID == created.RequestID && strings.Contains(m.FromLabel, "Bravo") && strings.Contains(m.Body, "answered") {
			found = true
		}
	}
	if !found {
		t.Fatalf("alpha's notifications: %+v", inb.Messages)
	}
	var got xspace.Request
	res = call("alpha", "spaceman_xspace_get", map[string]any{"request_id": created.RequestID})
	_ = json.Unmarshal(res.Result, &got)
	if got.State != xspace.Completed || !got.ReplyTruncated || len([]rune(got.Reply)) != xspace.SummaryChars || got.LocalTaskID != "" || got.DecidedBy != "" {
		t.Fatalf("alpha's view: %+v", got)
	}

	// Knowledge: charlie's KB answers through a support session whose requester is alpha's control run.
	repo := filepath.Join(e.agentDir, "spaces", c.Workdir, "repos", "kb")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "init", "-q", "-b", "trunk")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# KB\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "init")
	gitIn(t, repo, "branch", "main")
	prof, err := e.c.Catalog.SaveProfile(ctx, admin, "", 0, catalog.Profile{Slug: "kb-claude", Name: "KB", Harness: hns.KindClaude})
	if err != nil {
		t.Fatal(err)
	}
	tpl, err := e.c.Catalog.SaveTemplate(ctx, admin, "", 0, catalog.RoleTemplate{Slug: "kb-keeper", Name: "KB keeper", Kind: "kb", ProfileID: prof.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.c.Catalog.SaveSpaceRole(ctx, admin, c.ID, catalog.SpaceRole{Name: "kb-support", TemplateID: tpl.ID}); err != nil {
		t.Fatal(err)
	}
	k, err := e.c.KB.Create(ctx, admin, c.ID, kb.Input{Slug: "payments", Name: "Payments KB", Repo: "repos/kb", BaseBranch: "main", SpaceRole: "kb-support",
		WritePolicy: kb.ReadOnly, AllowedRoles: []string{"nobody"}})
	if err != nil {
		t.Fatal(err)
	}
	if r := call("alpha", "spaceman_kb_ask", map[string]any{"kb": "payments", "kind": "decision", "question": "x"}); r.OK {
		t.Fatalf("alpha reached charlie's KB directly: %+v", r)
	}
	res = call("alpha", "spaceman_xspace_request", map[string]any{"to": c.ID, "kind": "knowledge", "kb": "payments", "kb_kind": "decision",
		"title": "Settlement window", "body": "Which settlement window did you decide on?"})
	var kq struct {
		RequestID string `json:"request_id"`
		State     string `json:"state"`
	}
	_ = json.Unmarshal(res.Result, &kq)
	if !res.OK || kq.State != xspace.Accepted {
		t.Fatalf("knowledge request: %+v", res)
	}
	e.c.KB.Step(ctx)
	d, err := e.c.KB.Detail(ctx, c.ID, k.ID)
	if err != nil || len(d.Sessions) != 1 || len(d.Requests) != 1 {
		t.Fatalf("KB detail %+v %v", d, err)
	}
	sess := d.Sessions[0]
	if sess.RequesterKey != "space:"+a.ID+"/run:"+ctl["alpha"].ID || sess.RunID == "" {
		t.Fatalf("support session %+v", sess)
	}
	eventually(t, "KB session running", func() bool { return e.run1(sess.RunID).Observed == "running" })
	if kr := e.run1(sess.RunID); kr.Kind != "kb" || kr.SpaceID != c.ID {
		t.Fatalf("KB run %+v", kr)
	}
	eventually(t, "KB request dispatched", func() bool {
		d, _ := e.c.KB.Detail(ctx, c.ID, k.ID)
		return d.Requests[0].State == "dispatched"
	})
	res = e.bridgeCall(sess.RunID, e.runToken(sess.RunID), "spaceman_kb_answer", map[string]any{"request_id": d.Requests[0].ID, "answer": "T+2 (ADR-0007).",
		"basis": "committed", "evidence": []map[string]string{{"path": "decisions/ADR-0007.md"}}})
	if !res.OK {
		t.Fatalf("KB answer: %+v", res)
	}
	res = call("alpha", "spaceman_xspace_get", map[string]any{"request_id": kq.RequestID})
	got = xspace.Request{}
	_ = json.Unmarshal(res.Result, &got)
	if got.State != xspace.Completed || got.Reply != "T+2 (ADR-0007)." || len(got.ReplyEvidence) != 1 || got.KBRequestID != "" {
		t.Fatalf("knowledge answer for alpha: %+v", res)
	}
	// Charlie's control never had to act, and bravo never sees alpha's knowledge request.
	if r := call("bravo", "spaceman_xspace_get", map[string]any{"request_id": kq.RequestID}); r.OK {
		t.Fatalf("bravo read charlie's request: %+v", r)
	}
}
