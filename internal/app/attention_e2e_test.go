package app_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/api"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
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
	// Rows of the new categories in both spaces: a parked run, an account limit, a failed switch and an incoming request.
	seedAttentionRows(t, h, en.Node.ID, a.ID, b.ID)
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
	if got := attentionKinds(at.Items, a.ID); got != "account_limit account_switch_failed run_parked task_blocked xspace_request" {
		t.Fatalf("root items of alpha: %q (%+v)", got, at.Items)
	}
	if got := attentionKinds(at.Items, b.ID); got != "account_limit account_switch_failed run_parked xspace_request" {
		t.Fatalf("root items of beta: %q", got)
	}
	if at.ItemCounts["task_blocked"] != 1 || at.ItemCounts["account_limit"] != 2 || at.ItemCounts["xspace_request"] != 2 || *at.Spaces[0].RunsParked != 1 {
		t.Fatalf("counts %+v", at.ItemCounts)
	}
	for _, it := range at.Items {
		if it.Target.SpaceID != it.SpaceID || it.Title == "" || it.Since.IsZero() || it.ID == "" || it.Target.Type == "" {
			t.Fatalf("incomplete item %+v", it)
		}
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
	for _, it := range at.Items {
		if it.SpaceID != a.ID || it.Target.SpaceID != a.ID {
			t.Fatalf("viewer sees another space's item %+v", it)
		}
	}
	if got := attentionKinds(at.Items, a.ID); got != "account_limit account_switch_failed run_parked task_blocked xspace_request" || at.ItemCounts["account_limit"] != 1 || at.ItemCounts["xspace_request"] != 1 {
		t.Fatalf("viewer items %q %+v", got, at.ItemCounts)
	}
	if len(at.Nodes) != 0 || !at.Spaces[0].NodeDisconnected { // node:read is global; the space still says its node is down
		t.Fatalf("viewer nodes %+v", at)
	}
	at = api.Attention{}
	viewer.do("GET", "/attention?space="+b.ID, nil).json(t, &at)
	if len(at.Spaces) != 0 || len(at.Items) != 0 {
		t.Fatalf("unreadable space leaked %+v %+v", at.Spaces, at.Items)
	}
	expect(t, h.client().do("GET", "/attention", nil), 401)
}

// SM-UI-003 over HTTP: a kind with more rows than the list shows lists the newest 20 and still counts all of them.
func TestAttentionItemsCappedAt20WithFullCount(t *testing.T) {
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
	const blocked = 25
	for i := range blocked {
		var task queue.Task
		root.do("POST", "/spaces/"+a.ID+"/tasks", map[string]any{"title": "Blocked " + string(rune('A'+i)), "status": "ready"}).json(t, &task)
		expect(t, root.do("POST", "/spaces/"+a.ID+"/tasks/"+task.ID+"/status", map[string]any{"status": "blocked", "reason": "waiting"}, "If-Match", `"1"`), 200)
	}
	var at api.Attention
	root.do("GET", "/attention", nil).json(t, &at)
	listed := 0
	for _, it := range at.Items {
		if it.Kind == "task_blocked" {
			listed++
		}
	}
	if listed != 20 {
		t.Fatalf("listed %d task_blocked items, want 20", listed)
	}
	if at.ItemCounts["task_blocked"] != blocked || len(at.Spaces) != 1 || *at.Spaces[0].TasksBlocked != blocked {
		t.Fatalf("counts %+v spaces %+v", at.ItemCounts, at.Spaces)
	}
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

// Each new attention category is withheld on its own permission: a principal that reads a space but not its runs sees
// no parked runs, account limits or failed switches, and one without xspace:read sees no incoming requests. No
// built-in role lacks either permission, so the test removes them from the in-memory policy.
func TestAttentionNewCategoriesNeedTheirOwnPermission(t *testing.T) {
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
	var task queue.Task
	root.do("POST", "/spaces/"+a.ID+"/tasks", map[string]any{"title": "Ship", "status": "ready"}).json(t, &task)
	expect(t, root.do("POST", "/spaces/"+a.ID+"/tasks/"+task.ID+"/status", map[string]any{"status": "blocked", "reason": "waiting"}, "If-Match", `"1"`), 200)
	seedAttentionRows(t, h, en.Node.ID, a.ID, b.ID)
	expect(t, root.do("POST", "/users", map[string]any{"email": "vic@example.com", "display_name": "Vic", "password": "correct horse battery staple",
		"grants": []map[string]string{{"role": "viewer", "domain": a.ID}}}), 201)
	viewer := h.client()
	viewer.bearer = loginAndIssueToken(t, h, "vic@example.com", []map[string]string{{"role": "viewer", "domain": a.ID}})

	fetch := func(withhold ...authz.Permission) api.Attention {
		t.Helper()
		if err := h.c.Authz.Reload(ctx); err != nil {
			t.Fatal(err)
		}
		if err := h.c.Authz.WithholdForTest(withhold...); err != nil {
			t.Fatal(err)
		}
		var at api.Attention
		viewer.do("GET", "/attention", nil).json(t, &at)
		if len(at.Spaces) != 1 {
			t.Fatalf("a reader of one space sees %d spaces: %+v", len(at.Spaces), at.Spaces)
		}
		return at
	}
	const all = "account_limit account_switch_failed run_parked task_blocked xspace_request"
	if at := fetch(); attentionKinds(at.Items, a.ID) != all || at.Spaces[0].Total != 1 {
		t.Fatalf("full reader %q %+v", attentionKinds(at.Items, a.ID), at.Spaces)
	}
	at := fetch(authz.RunRead)
	if got := attentionKinds(at.Items, a.ID); got != "task_blocked xspace_request" {
		t.Fatalf("without run:read: %q", got)
	}
	s := at.Spaces[0]
	if s.RunsParked != nil || s.AccountLimits != nil || s.AccountSwitchFails != nil || s.XSpaceIncoming == nil {
		t.Fatalf("without run:read, counts %+v", s)
	}
	if at.ItemCounts["run_parked"] != 0 || at.ItemCounts["account_limit"] != 0 || at.ItemCounts["account_switch_failed"] != 0 || at.ItemCounts["xspace_request"] != 1 {
		t.Fatalf("without run:read, item counts %+v", at.ItemCounts)
	}
	at = fetch(authz.XSpaceRead)
	if got := attentionKinds(at.Items, a.ID); got != "account_limit account_switch_failed run_parked task_blocked" {
		t.Fatalf("without xspace:read: %q", got)
	}
	s = at.Spaces[0]
	if s.XSpaceIncoming != nil || s.RunsParked == nil || s.AccountLimits == nil || s.AccountSwitchFails == nil || at.ItemCounts["xspace_request"] != 0 || at.ItemCounts["run_parked"] != 1 {
		t.Fatalf("without xspace:read, counts %+v %+v", s, at.ItemCounts)
	}
	at = fetch(authz.RunRead, authz.XSpaceRead)
	if got := attentionKinds(at.Items, a.ID); got != "task_blocked" || at.Spaces[0].Total != 1 {
		t.Fatalf("without both: %q %+v", got, at.Spaces)
	}
}

// attentionKinds lists the sorted distinct item kinds of one space.
func attentionKinds(items []api.AttentionItem, space string) string {
	seen := map[string]bool{}
	for _, it := range items {
		if it.SpaceID == space {
			seen[it.Kind] = true
		}
	}
	kinds := make([]string, 0, len(seen))
	for k := range seen {
		kinds = append(kinds, k)
	}
	slices.Sort(kinds)
	return strings.Join(kinds, " ")
}

// seedAttentionRows writes one row of each new category into both spaces.
func seedAttentionRows(t *testing.T, h *harness, node, a, b string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UnixMicro()
	err := h.c.Store.W().TxNamed(ctx, "test.attention_seed", func(tx *store.Tx) error {
		for i, sp := range []string{a, b} {
			n := string(rune('a' + i))
			stmts := []struct {
				q    string
				args []any
			}{
				{`INSERT INTO runs (id, space_id, node_id, generation, kind, role, harness, instance_id, desired, observed, launch_json, created_at, created_by, updated_at)
				  VALUES (?, ?, ?, 1, 'worker', 'implementor', 'claude-code', ?, 'stopped', 'exited', '{}', ?, 'test', ?)`, []any{"run_" + n, sp, node, "ins_" + n, now, now}},
				{`INSERT INTO limit_waits (id, space_id, run_id, run_kind, role, node_id, instance_id, harness, launch_json, resume_at, created_at, updated_at)
				  VALUES (?, ?, ?, 'worker', 'implementor', ?, ?, 'claude-code', '{}', ?, ?, ?)`, []any{"lw_" + n, sp, "run_" + n, node, "ins_" + n, now + 1e9, now, now}},
				{`INSERT INTO usage_limits (id, space_id, scope, account_id, kind, source, seen_at, created_at, updated_at)
				  VALUES (?, ?, 'account', 'acc_1', 'five_hour', 'transcript', ?, ?, ?)`, []any{"ul_" + n, sp, now, now, now}},
				{`INSERT INTO account_switches (id, space_id, instance_id, role, run_kind, harness, from_run_id, from_account_id, cause, limit_kind, state, reason, created_at, updated_at)
				  VALUES (?, ?, ?, 'implementor', 'worker', 'claude-code', ?, 'acc_1', 'usage_limit', 'five_hour', 'failed', 'ended_early', ?, ?)`, []any{"as_" + n, sp, "ins_" + n, "run_" + n, now, now}},
				{`INSERT INTO xspace_requests (id, source_space_id, dest_space_id, kind, title, body, requester_run, requester_role, correlation_id, chain_json, depth, state, deadline_at, created_at, updated_at)
				  VALUES (?, ?, ?, 'question', 'Is it up?', 'body', 'run_x', 'control', ?, '[]', 1, 'pending', ?, ?, ?)`, []any{"xr_" + n, a, sp, "xr_" + n, now + 1e9, now, now}},
			}
			for _, s := range stmts {
				if _, err := tx.SQL.ExecContext(ctx, s.q, s.args...); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A space whose only rows are in the categories outside Total (parked runs, account limits and switches, incoming
// cross-space requests) is still listed in spaces[], so a client can name it next to its items.
func TestAttentionListsSpaceWithOnlyWatchRows(t *testing.T) {
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
	seedAttentionRows(t, h, en.Node.ID, a.ID, b.ID)
	var at api.Attention
	root.do("GET", "/attention", nil).json(t, &at)
	found := map[string]int64{}
	for _, s := range at.Spaces {
		found[s.SpaceID] = s.Total
	}
	if len(at.Spaces) != 2 || found[a.ID] != 0 || found[b.ID] != 0 {
		t.Fatalf("spaces %+v", at.Spaces)
	}
}
