package app_test

import (
	"context"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/api"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/inbox"
)

// SM-INBOX-001/002/003, SM-ATT-001 over HTTP: an operator item with an attachment, idempotent creation, the inbox page
// with counts, optimistic status changes, and read-only access for viewers.
func TestInboxOverHTTP(t *testing.T) {
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
	base := "/spaces/" + sp.ID + "/inbox"

	r := root.upload("/spaces/"+sp.ID+"/attachments", "screenshot.png", png1x1)
	expect(t, r, 201)
	var att api.AttachmentOut
	r.json(t, &att)
	body := map[string]any{"body": "Checkout fails on Safari\n\n![screenshot](" + att.URI + ")", "urgency": "high", "attachment_ids": []string{att.ID}}
	r = root.do("POST", base, body, "Idempotency-Key", "inbox-create-1")
	expect(t, r, 201)
	var it inbox.Item
	r.json(t, &it)
	if it.Title != "Checkout fails on Safari" || it.SourceKind != "operator" || len(it.Attachments) != 1 || r.header.Get("ETag") != `"1"` {
		t.Fatalf("created %+v (etag %q)", it, r.header.Get("ETag"))
	}
	var again inbox.Item
	root.do("POST", base, body, "Idempotency-Key", "inbox-create-1").json(t, &again)
	if again.ID != it.ID {
		t.Fatal("idempotent create made a second item")
	}
	if p := root.do("POST", base, map[string]any{"content_type": "application/json", "body": "{bad"}).problem(t); p.Code != "validation_failed" {
		t.Fatalf("bad JSON body: %+v", p)
	}

	var page api.InboxPage
	r = root.do("GET", base, nil)
	expect(t, r, 200)
	r.json(t, &page)
	if len(page.Items) != 1 || page.Counts["new"] != 1 {
		t.Fatalf("page %+v", page)
	}

	expect(t, root.do("POST", base+"/"+it.ID+"/status", map[string]any{"status": "done"}), 428)
	if p := root.do("POST", base+"/"+it.ID+"/status", map[string]any{"status": "done"}, "If-Match", `"7"`).problem(t); p.Code != "precondition_failed" {
		t.Fatalf("stale If-Match: %+v", p)
	}
	r = root.do("POST", base+"/"+it.ID+"/status", map[string]any{"status": "rejected", "reason": "duplicate of the Safari ticket"}, "If-Match", `"1"`)
	expect(t, r, 200)
	var rej inbox.Item
	r.json(t, &rej)
	if rej.Status != "rejected" || rej.Version != 2 {
		t.Fatalf("rejected %+v", rej)
	}

	expect(t, root.do("POST", "/users", map[string]any{"email": "vic@example.com", "display_name": "Vic", "password": "correct horse battery staple",
		"grants": []map[string]string{{"role": "viewer", "domain": sp.ID}}}), 201)
	viewer := h.client()
	viewer.bearer = loginAndIssueToken(t, h, "vic@example.com", []map[string]string{{"role": "viewer", "domain": sp.ID}})
	expect(t, viewer.do("GET", base+"/"+it.ID, nil), 200)
	expect(t, viewer.do("POST", base, map[string]any{"body": "hi"}), 403)
}
