package app_test

import (
	"context"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/api"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/human"
	"github.com/aurimasniekis/go-spaceman/internal/messages"
	"github.com/aurimasniekis/go-spaceman/internal/queue"
	"github.com/aurimasniekis/go-spaceman/internal/store"
)

// SM-UI-003 over HTTP: one attention summary of human actions, blocked tasks and storage, filtered per category by
// the caller's permissions (a viewer of one space sees only that space and no installation-wide sections).
func TestAttentionOverHTTP(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	rootSecret, _, _ := h.c.Auth.IssueRootToken(ctx)
	root := h.client()
	root.bearer = rootSecret
	en, _ := h.c.Fleet.CreateNode(ctx, admin, fleet.CreateNodeInput{Name: "node-a"})
	a, err := h.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "alpha", Name: "Alpha", NodeID: en.Node.ID})
	if err != nil {
		t.Fatal(err)
	}
	b, err := h.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "beta", Name: "Beta", NodeID: en.Node.ID})
	if err != nil {
		t.Fatal(err)
	}
	expect(t, root.do("POST", "/spaces/"+a.ID+"/human-actions", map[string]any{"type": "todo", "title": "Rotate the key"}), 201)
	expect(t, root.do("POST", "/spaces/"+b.ID+"/human-actions", map[string]any{"type": "todo", "title": "Elsewhere"}), 201)
	var task queue.Task
	root.do("POST", "/spaces/"+a.ID+"/tasks", map[string]any{"title": "Ship", "status": "ready"}).json(t, &task)
	expect(t, root.do("POST", "/spaces/"+a.ID+"/tasks/"+task.ID+"/status", map[string]any{"status": "blocked", "reason": "waiting"}, "If-Match", `"1"`), 200)

	// An enrolled (active) node that is not connected needs a person.
	if err := h.c.Store.W().TxNamed(ctx, "test.activate", func(tx *store.Tx) error {
		_, err := tx.SQL.ExecContext(ctx, "UPDATE nodes SET state = 'active' WHERE id = ?", en.Node.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var at api.Attention
	root.do("GET", "/attention", nil).json(t, &at)
	if len(at.Spaces) != 2 || at.Spaces[0].SpaceID != a.ID || at.Spaces[0].Total != 3 || !at.Spaces[0].NodeDisconnected || *at.Spaces[0].HumanPending != 1 || *at.Spaces[0].TasksBlocked != 1 {
		t.Fatalf("root spaces %+v", at.Spaces)
	}
	if at.Storage == nil || at.Storage.StoreState != "ok" || at.Storage.DiskTotal == 0 || at.Backups == nil || at.Backups.State != "none" || at.Outbox == nil {
		t.Fatalf("root installation sections %+v %+v %+v", at.Storage, at.Backups, at.Outbox)
	}
	if len(at.Nodes) != 1 || at.Nodes[0].ID != en.Node.ID || len(at.Nodes[0].Spaces) != 2 {
		t.Fatalf("nodes %+v", at.Nodes)
	}
	at = api.Attention{}
	root.do("GET", "/attention?space="+b.ID, nil).json(t, &at)
	if len(at.Spaces) != 1 || at.Spaces[0].SpaceID != b.ID || at.Storage != nil || at.Nodes != nil {
		t.Fatalf("one space %+v", at)
	}

	expect(t, root.do("POST", "/users", map[string]any{"email": "vic@example.com", "display_name": "Vic", "password": "correct horse battery staple",
		"grants": []map[string]string{{"role": "viewer", "domain": a.ID}}}), 201)
	viewer := h.client()
	viewer.bearer = loginAndIssueToken(t, h, "vic@example.com", []map[string]string{{"role": "viewer", "domain": a.ID}})
	at = api.Attention{}
	viewer.do("GET", "/attention", nil).json(t, &at)
	if len(at.Spaces) != 1 || at.Spaces[0].SpaceID != a.ID || at.Storage != nil || at.Backups != nil || at.Outbox != nil {
		t.Fatalf("viewer %+v", at)
	}
	if len(at.Nodes) != 0 || !at.Spaces[0].NodeDisconnected { // node:read is global; the space still says its node is down
		t.Fatalf("viewer nodes %+v", at)
	}
	at = api.Attention{}
	viewer.do("GET", "/attention?space="+b.ID, nil).json(t, &at)
	if len(at.Spaces) != 0 {
		t.Fatalf("unreadable space leaked %+v", at.Spaces)
	}
	expect(t, h.client().do("GET", "/attention", nil), 401)
}

// SM-HUM-006, SM-HUI-001, SM-ATT-001 over HTTP: search and filters on both listings, clarification requests,
// attachments on comments, responses, tasks and messages (only the space's own), and per-user notification
// preferences (SM-HUI-006) with optimistic concurrency.
func TestHumanSearchClarifyAttachmentsAndPrefsOverHTTP(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	rootSecret, _, _ := h.c.Auth.IssueRootToken(ctx)
	root := h.client()
	root.bearer = rootSecret
	en, _ := h.c.Fleet.CreateNode(ctx, admin, fleet.CreateNodeInput{Name: "node-a"})
	a, _ := h.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "alpha", Name: "Alpha", NodeID: en.Node.ID})
	b, _ := h.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "beta", Name: "Beta", NodeID: en.Node.ID})
	base := "/spaces/" + a.ID

	var q, todo human.Action
	root.do("POST", base+"/human-actions", map[string]any{"type": "question", "title": "Which region?", "body": "staging needs one", "priority": "urgent",
		"request": map[string]any{"allow_text": true}}).json(t, &q)
	root.do("POST", base+"/human-actions", map[string]any{"type": "todo", "title": "Renew certificate", "priority": "low"}).json(t, &todo)
	var l api.HumanActionList
	root.do("GET", base+"/human-actions?q=STAGING", nil).json(t, &l)
	if len(l.Items) != 1 || l.Items[0].ID != q.ID {
		t.Fatalf("search %+v", l.Items)
	}
	root.do("GET", base+"/human-actions?priority=low&assignee=unassigned&requester=operator", nil).json(t, &l)
	if len(l.Items) != 1 || l.Items[0].ID != todo.ID {
		t.Fatalf("filters %+v", l.Items)
	}
	root.do("GET", base+"/human-actions?sort=priority", nil).json(t, &l)
	if len(l.Items) != 2 || l.Items[0].ID != q.ID {
		t.Fatalf("sort %+v", l.Items)
	}
	var pend api.PendingHumanActions
	root.do("GET", "/human-actions?q=certificate&space="+a.ID, nil).json(t, &pend)
	if len(pend.Items) != 1 || pend.Items[0].ID != todo.ID || pend.Total != 2 {
		t.Fatalf("global search %+v", pend)
	}
	expect(t, root.do("GET", base+"/human-actions?sort=random", nil), 422)

	var clar human.Action
	r := root.do("POST", base+"/human-actions/"+q.ID+"/clarify", map[string]any{"body": "For which service?"})
	expect(t, r, 201)
	r.json(t, &clar)
	if clar.Status != "open" || len(clar.Comments) != 1 || clar.Comments[0].Kind != "clarification" {
		t.Fatalf("clarify %+v", clar)
	}

	var att, foreign api.AttachmentOut
	root.upload(base+"/attachments", "log.txt", []byte("line 1\n")).json(t, &att)
	root.upload("/spaces/"+b.ID+"/attachments", "other.txt", []byte("other\n")).json(t, &foreign)
	expect(t, root.do("POST", base+"/human-actions/"+q.ID+"/comments", map[string]any{"body": "see", "attachment_ids": []string{foreign.ID}}), 422)
	var cm human.Action
	root.do("POST", base+"/human-actions/"+q.ID+"/comments", map[string]any{"body": "see the log", "attachment_ids": []string{att.ID}}).json(t, &cm)
	if len(cm.Comments) != 2 || len(cm.Comments[1].Attachments) != 1 || cm.Comments[1].Attachments[0].ID != att.ID {
		t.Fatalf("comment attachments %+v", cm.Comments)
	}
	var resp human.Action
	root.do("POST", base+"/human-actions/"+q.ID+"/respond", map[string]any{"outcome": "answered", "answer": "eu-west", "attachment_ids": []string{att.ID}},
		"If-Match", `"1"`).json(t, &resp)
	if resp.Status != "resolved" || len(resp.ResponseAttachments) != 1 {
		t.Fatalf("response attachments %+v", resp)
	}
	var task queue.Task
	expect(t, root.do("POST", base+"/tasks", map[string]any{"title": "x", "attachment_ids": []string{foreign.ID}}), 422)
	root.do("POST", base+"/tasks", map[string]any{"title": "Investigate", "objective": "See spaceman://attachment/" + att.ID, "attachment_ids": []string{att.ID}}).json(t, &task)
	if len(task.Attachments) != 1 || task.Attachments[0].ID != att.ID {
		t.Fatalf("task attachments %+v", task.Attachments)
	}
	var m messages.Message
	expect(t, root.do("POST", base+"/messages", map[string]any{"to": "control", "subject": "s", "attachment_ids": []string{foreign.ID}}), 422)
	root.do("POST", base+"/messages", map[string]any{"to": "control", "subject": "Log", "body": "attached", "attachment_ids": []string{att.ID}}).json(t, &m)
	if len(m.Attachments) != 1 {
		t.Fatalf("message attachments %+v", m)
	}

	// Preferences belong to a user: the root token has none.
	expect(t, root.do("GET", "/auth/me/notification-preferences", nil), 403)
	expect(t, root.do("POST", "/users", map[string]any{"email": "ola@example.com", "display_name": "Ola", "password": "correct horse battery staple",
		"grants": []map[string]string{{"role": "operator", "domain": a.ID}}}), 201)
	op := h.client()
	op.bearer = loginAndIssueToken(t, h, "ola@example.com", []map[string]string{{"role": "operator", "domain": a.ID}})
	var p api.NotificationPrefsOut
	op.do("GET", "/auth/me/notification-preferences", nil).json(t, &p)
	if p.Version != 0 || !p.Enabled || !p.AssignedToMe || p.Desktop || p.MinPriority != "low" {
		t.Fatalf("defaults %+v", p)
	}
	set := map[string]any{"enabled": true, "desktop": true, "new_actions": false, "assigned_to_me": true, "escalations": true, "reminders": false,
		"clarifications": true, "min_priority": "high", "muted_spaces": []string{b.ID}}
	expect(t, op.do("PUT", "/auth/me/notification-preferences", set), 428)
	r = op.do("PUT", "/auth/me/notification-preferences", set, "If-Match", `"0"`)
	expect(t, r, 200)
	r.json(t, &p)
	if p.Version != 1 || !p.Desktop || p.NewActions || p.MinPriority != "high" || len(p.MutedSpaces) != 1 || r.header.Get("ETag") != `"1"` {
		t.Fatalf("saved %+v", p)
	}
	expect(t, op.do("PUT", "/auth/me/notification-preferences", set, "If-Match", `"0"`), 409)
	set["min_priority"] = "p0"
	expect(t, op.do("PUT", "/auth/me/notification-preferences", set, "If-Match", `"1"`), 422)
}
