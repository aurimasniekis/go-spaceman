package human

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/store"
)

func joinTitles(l []*Action) string {
	var s []string
	for _, a := range l {
		s = append(s, a.Title)
	}
	return strings.Join(s, ",")
}

// SM-HUM-006, SM-HUI-001: search over title and body, filters by status, assignee, priority, requester and due date,
// and display sorts that never rewrite the durable rank (SM-HUI-004).
func TestSearchFiltersAndSorts(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	due := f.clk.Now().Add(time.Hour)
	a := f.create(CreateInput{Type: TypeTodo, Title: "Rotate the API key", Body: "The **staging** key expires Friday.", Priority: "high", DueAt: &due})
	f.clk.Advance(time.Minute)
	b := f.create(CreateInput{Type: TypeQuestion, Title: "Which region?", Body: "eu-west or us-east for staging", Priority: "urgent"})
	f.clk.Advance(time.Minute)
	c := f.create(CreateInput{Type: TypeTodo, Title: "Renew certificate", Requester: Requester{Kind: "operator", ID: "usr_op", Label: "Op Erator"}})
	_, err := f.svc.Assign(f.ctx, op, "spc_a", b.ID, 0, "usr_op")
	must(t, err)

	list := func(fl ListFilter) string {
		t.Helper()
		l, err := f.svc.List(f.ctx, "spc_a", fl)
		must(t, err)
		return joinTitles(l)
	}
	if got := list(ListFilter{Query: "STAGING"}); got != "Rotate the API key,Which region?" {
		t.Fatalf("search title+body = %q", got)
	}
	if got := list(ListFilter{Query: "staging key"}); got != "Rotate the API key" {
		t.Fatalf("every word must match = %q", got)
	}
	if got := list(ListFilter{Assignee: "usr_op"}); got != "Which region?" {
		t.Fatalf("assignee = %q", got)
	}
	if got := list(ListFilter{Assignee: Unassigned}); got != "Rotate the API key,Renew certificate" {
		t.Fatalf("unassigned = %q", got)
	}
	if got := list(ListFilter{Status: InProgress}); got != "Which region?" {
		t.Fatalf("status = %q", got)
	}
	if got := list(ListFilter{Priority: "high"}); got != "Rotate the API key" {
		t.Fatalf("priority = %q", got)
	}
	if got := list(ListFilter{Requester: "operator"}); got != "Renew certificate" {
		t.Fatalf("requester kind = %q", got)
	}
	if got := list(ListFilter{Requester: "erator"}); got != "Renew certificate" {
		t.Fatalf("requester label = %q", got)
	}
	if got := list(ListFilter{Requester: "run_1"}); got != "Rotate the API key,Which region?" {
		t.Fatalf("requester run = %q", got)
	}
	if got := list(ListFilter{Due: DueSet}); got != "Rotate the API key" {
		t.Fatalf("due = %q", got)
	}
	if got := list(ListFilter{Due: DueOverdue}); got != "" {
		t.Fatalf("not overdue yet = %q", got)
	}
	f.clk.Advance(2 * time.Hour)
	if got := list(ListFilter{Due: DueOverdue}); got != "Rotate the API key" {
		t.Fatalf("overdue = %q", got)
	}

	if got := list(ListFilter{Sort: SortPriority}); got != "Which region?,Rotate the API key,Renew certificate" {
		t.Fatalf("by priority = %q", got)
	}
	if got := list(ListFilter{Sort: SortNewest}); got != "Renew certificate,Which region?,Rotate the API key" {
		t.Fatalf("newest = %q", got)
	}
	if got := list(ListFilter{Sort: SortDue}); !strings.HasPrefix(got, "Rotate the API key,") {
		t.Fatalf("due first = %q", got)
	}
	// Sorting is display-only: the durable order is unchanged.
	if got := list(ListFilter{}); got != "Rotate the API key,Which region?,Renew certificate" {
		t.Fatalf("rank = %q", got)
	}
	g, err := f.svc.ListPending(f.ctx, nil, ListFilter{Query: "certificate"})
	must(t, err)
	if joinTitles(g) != "Renew certificate" {
		t.Fatalf("global search = %q", joinTitles(g))
	}
	_ = a
	_ = c
	for _, bad := range []ListFilter{{Status: "done"}, {Priority: "p1"}, {Due: "soon"}, {Sort: "random"}, {Query: strings.Repeat("x", 201)}} {
		_, err := f.svc.List(f.ctx, "spc_a", bad)
		code(t, err, apperr.CodeInvalid)
	}
}

// SM-HUM-006/009: a clarification request keeps the action pending, is not a response, and reaches the agent
// requester as a durable clarification notification; the requester replies, other runs cannot.
func TestClarificationRequest(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	q := f.create(CreateInput{Type: TypeQuestion, Title: "Which DB?", Request: json.RawMessage(`{"allow_text":true}`)})
	a, err := f.svc.RequestClarification(f.ctx, op, "spc_a", q.ID, CommentInput{Body: "For which service?", Label: "Op"})
	must(t, err)
	if a.Status != Open || a.Outcome != "" || len(a.Comments) != 1 || a.Comments[0].Kind != CommentClarification {
		t.Fatalf("clarify: %+v", a)
	}
	if len(a.Notifications) != 1 || a.Notifications[0].Kind != NoticeClarification || a.Notifications[0].TargetRun != "run_1" {
		t.Fatalf("notification: %+v", a.Notifications)
	}
	// Another run of the space is not related and cannot reply.
	_, err = f.svc.ReplyAsRequester(f.ctx, op, "spc_a", q.ID, Viewer{RunID: "run_9", InstanceID: "ins_9"}, "x", "w")
	code(t, err, apperr.CodeForbidden)
	a, err = f.svc.ReplyAsRequester(f.ctx, op, "spc_a", q.ID, Viewer{RunID: "run_1", InstanceID: "ins_1"}, "The billing service.", "worker")
	must(t, err)
	if a.Status != Open || len(a.Comments) != 2 || a.Comments[1].Kind != CommentReply {
		t.Fatalf("reply: %+v", a.Comments)
	}
	// An operator requester gets the question as a comment only.
	o := f.create(CreateInput{Type: TypeTodo, Title: "op", Requester: Requester{Kind: "operator", ID: "usr_op"}})
	a, err = f.svc.RequestClarification(f.ctx, op, "spc_a", o.ID, CommentInput{Body: "Which host?"})
	must(t, err)
	if len(a.Notifications) != 0 || a.Comments[0].Kind != CommentClarification {
		t.Fatalf("operator requester: %+v", a)
	}
	// Final actions cannot be clarified; empty questions are refused.
	r, err := f.svc.Respond(f.ctx, op, "spc_a", q.ID, 0, RespondInput{Outcome: "answered", Response: Response{Answer: "sqlite"}})
	must(t, err)
	_, err = f.svc.RequestClarification(f.ctx, op, "spc_a", r.ID, CommentInput{Body: "late"})
	code(t, err, apperr.CodeConflict)
	_, err = f.svc.RequestClarification(f.ctx, op, "spc_a", o.ID, CommentInput{Body: " "})
	code(t, err, apperr.CodeInvalid)
}

// SM-ATT-001: requests, responses and comments reference attachments of their own space only.
func TestActionAttachments(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	must(t, f.s.W().TxNamed(f.ctx, "seed", func(tx *store.Tx) error {
		for _, q := range []string{
			`INSERT INTO attachments (id, space_id, sha256, size, mime, name, uploaded_by, created_at) VALUES ('att_a1', 'spc_a', 'x', 3, 'text/plain', 'a.txt', 'usr_op', 0)`,
			`INSERT INTO attachments (id, space_id, sha256, size, mime, name, uploaded_by, created_at) VALUES ('att_a2', 'spc_a', 'y', 3, 'image/png', 'b.png', 'usr_op', 0)`,
			`INSERT INTO attachments (id, space_id, sha256, size, mime, name, uploaded_by, created_at) VALUES ('att_b1', 'spc_b', 'z', 3, 'text/plain', 'c.txt', 'usr_op', 0)`,
		} {
			if _, err := tx.SQL.ExecContext(f.ctx, q); err != nil {
				return err
			}
		}
		return nil
	}))
	_, _, err := f.svc.Create(f.ctx, op, "spc_a", CreateInput{Type: TypeTodo, Title: "x", Requester: agent, AttachmentIDs: []string{"att_b1"}})
	code(t, err, apperr.CodeInvalid)
	q := f.create(CreateInput{Type: TypeQuestion, Title: "See log", Request: json.RawMessage(`{"allow_text":true}`), AttachmentIDs: []string{"att_a1"}})
	if len(q.Attachments) != 1 || q.Attachments[0].ID != "att_a1" {
		t.Fatalf("request attachments: %+v", q.Attachments)
	}
	_, err = f.svc.Comment(f.ctx, op, "spc_a", q.ID, CommentInput{Body: "c", AttachmentIDs: []string{"att_b1"}})
	code(t, err, apperr.CodeInvalid)
	a, err := f.svc.Comment(f.ctx, op, "spc_a", q.ID, CommentInput{Body: "screenshot", AttachmentIDs: []string{"att_a2"}})
	must(t, err)
	if len(a.Comments) != 1 || len(a.Comments[0].Attachments) != 1 || a.Comments[0].Attachments[0].Name != "b.png" {
		t.Fatalf("comment attachments: %+v", a.Comments)
	}
	_, err = f.svc.Respond(f.ctx, op, "spc_a", q.ID, 0, RespondInput{Outcome: "answered", Response: Response{Answer: "ok"}, AttachmentIDs: []string{"att_b1"}})
	code(t, err, apperr.CodeInvalid)
	r, err := f.svc.Respond(f.ctx, op, "spc_a", q.ID, 0, RespondInput{Outcome: "answered", Response: Response{Answer: "ok"}, AttachmentIDs: []string{"att_a1", "att_a2"}})
	must(t, err)
	if len(r.ResponseAttachments) != 2 || len(r.Attachments) != 1 {
		t.Fatalf("response attachments: %+v / %+v", r.ResponseAttachments, r.Attachments)
	}
}
