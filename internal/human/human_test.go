package human

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/keyring/keyringtest"
	"github.com/aurimasniekis/go-spaceman/internal/outbox"
	"github.com/aurimasniekis/go-spaceman/internal/queue"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/storetest"
)

var (
	op    = audit.Actor{Type: audit.ActorUser, ID: "usr_op"}
	op2   = audit.Actor{Type: audit.ActorUser, ID: "usr_two"}
	agent = Requester{Kind: "agent", ID: "ins_1", Role: "worker", Run: "run_1", Session: "ses_1", Label: "worker"}
)

type fx struct {
	t   *testing.T
	ctx context.Context
	s   *store.Store
	clk *clock.Fake
	svc *Service
	q   *queue.Service
}

func newFx(t *testing.T) *fx {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC))
	s := storetest.Open(t, storetest.WithClock(clk))
	kr := keyringtest.Load(t, s)
	aw, ch := audit.NewWriter(kr), changefeed.New(nil)
	f := &fx{t: t, ctx: context.Background(), s: s, clk: clk, svc: New(s, aw, ch, clk), q: queue.New(s, aw, ch)}
	must(t, s.W().TxNamed(f.ctx, "seed", func(tx *store.Tx) error {
		for _, q := range []string{
			`INSERT INTO nodes (id, name, state, created_at, created_by, updated_at) VALUES ('nod_1', 'n1', 'active', 0, 't', 0)`,
			`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('spc_a', 'aaa', 'A', 'nod_1', 'active', 'a', 0, 't', 0)`,
			`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('spc_b', 'bbb', 'B', 'nod_1', 'active', 'b', 0, 't', 0)`,
			`INSERT INTO users (id, email, display_name, enabled, provisioned_via, mfa_required, version, created_at, created_by, updated_at) VALUES ('usr_op', 'op@example.test', 'Op', 1, 'admin', 0, 1, 0, 't', 0)`,
			// The requester of most tests: a live worker run.
			`INSERT INTO runs (id, space_id, node_id, generation, kind, role, harness, instance_id, desired, observed, status, launch_json, created_at, created_by, updated_at)
			 VALUES ('run_1', 'spc_a', 'nod_1', 1, 'worker', 'worker', 'claude-code', 'ins_1', 'running', 'running', 'idle', '{}', 0, 't', 0)`,
		} {
			if _, err := tx.SQL.ExecContext(f.ctx, q); err != nil {
				return err
			}
		}
		return nil
	}))
	return f
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func code(t *testing.T, err error, want apperr.Code) {
	t.Helper()
	if apperr.CodeOf(err) != want {
		t.Fatalf("got %v (%s), want %s", err, apperr.CodeOf(err), want)
	}
}

func (f *fx) create(in CreateInput) *Action {
	f.t.Helper()
	if in.Requester.Kind == "" {
		in.Requester = agent
	}
	a, _, err := f.svc.Create(f.ctx, op, "spc_a", in)
	must(f.t, err)
	return a
}

// SM-HUM-003/005: each type has its own outcomes, lifecycle and outcome are separate, responses are final.
func TestTypeSpecificResponses(t *testing.T) {
	f := newFx(t)
	q := f.create(CreateInput{Type: TypeQuestion, Title: "Which DB?", Request: json.RawMessage(`{"options":["sqlite","postgres"]}`)})
	if q.Status != Open || q.Outcome != "" {
		t.Fatalf("new: %+v", q)
	}
	_, err := f.svc.Respond(f.ctx, op, "spc_a", q.ID, q.Version, RespondInput{Outcome: "approved"})
	code(t, err, apperr.CodeInvalid)
	_, err = f.svc.Respond(f.ctx, op, "spc_a", q.ID, q.Version, RespondInput{Outcome: "answered", Response: Response{Answer: "mysql"}})
	code(t, err, apperr.CodeInvalid)
	got, err := f.svc.Respond(f.ctx, op, "spc_a", q.ID, q.Version, RespondInput{Outcome: "answered", Response: Response{Answer: "sqlite"}})
	must(t, err)
	if got.Status != Resolved || got.Outcome != "answered" || got.RespondedBy != "usr_op" || len(got.Notifications) != 1 || got.Notifications[0].TargetRun != "run_1" {
		t.Fatalf("answered: %+v", got)
	}
	_, err = f.svc.Respond(f.ctx, op, "spc_a", q.ID, got.Version, RespondInput{Outcome: "answered", Response: Response{Answer: "postgres"}})
	code(t, err, apperr.CodeConflict)

	ap := f.create(CreateInput{Type: TypeApproval, Title: "Deploy?", Request: json.RawMessage(`{"action":"deploy to prod"}`)})
	_, err = f.svc.Respond(f.ctx, op, "spc_a", ap.ID, ap.Version, RespondInput{Outcome: "denied"})
	code(t, err, apperr.CodeInvalid) // a denial says why
	d, err := f.svc.Respond(f.ctx, op, "spc_a", ap.ID, ap.Version, RespondInput{Outcome: "denied", Response: Response{Note: "freeze"}})
	must(t, err)
	if d.Outcome != "denied" || d.Status != Resolved {
		t.Fatalf("denied: %+v", d)
	}

	td := f.create(CreateInput{Type: TypeTodo, Title: "Rotate the key", Requester: Requester{Kind: "operator", ID: "usr_op"}})
	c, err := f.svc.Respond(f.ctx, op, "spc_a", td.ID, 0, RespondInput{Outcome: "completed"})
	must(t, err)
	if c.Outcome != "completed" || len(c.Notifications) != 0 {
		t.Fatalf("todo: %+v", c) // operator requesters get no agent notification
	}
	m := f.create(CreateInput{Type: TypeMessage, Title: "FYI"})
	a, err := f.svc.Respond(f.ctx, op, "spc_a", m.ID, 0, RespondInput{Outcome: "acknowledged"})
	must(t, err)
	if a.Outcome != "acknowledged" {
		t.Fatalf("message: %+v", a)
	}
}

func TestCreateValidation(t *testing.T) {
	f := newFx(t)
	for name, in := range map[string]CreateInput{
		"no title":        {Type: TypeTodo, Requester: agent},
		"bad type":        {Type: "poll", Title: "x", Requester: agent},
		"approval action": {Type: TypeApproval, Title: "x", Requester: agent},
		"unknown field":   {Type: TypeTodo, Title: "x", Request: json.RawMessage(`{"nope":1}`), Requester: agent},
		"blocks no task":  {Type: TypeTodo, Title: "x", BlocksTask: true, Requester: agent},
		"info blocks":     {Type: TypeMessage, Title: "x", TaskID: "tsk_x", BlocksTask: true, Requester: agent},
		"bad priority":    {Type: TypeTodo, Title: "x", Priority: "meh", Requester: agent},
		"bad requester":   {Type: TypeTodo, Title: "x"},
		"expiry in past":  {Type: TypeTodo, Title: "x", ExpiresAt: new(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)), Requester: agent},
	} {
		_, _, err := f.svc.Create(f.ctx, op, "spc_a", in)
		code(t, err, apperr.CodeInvalid)
		_ = name
	}
	_, _, err := f.svc.Create(f.ctx, op, "spc_a", CreateInput{Type: TypeTodo, Title: "x", TaskID: "tsk_missing", Requester: agent})
	code(t, err, apperr.CodeInvalid)
	_, _, err = f.svc.Create(f.ctx, op, "spc_missing", CreateInput{Type: TypeTodo, Title: "x", Requester: agent})
	code(t, err, apperr.CodeNotFound)
	_, err = f.svc.Get(f.ctx, "spc_b", f.create(CreateInput{Type: TypeTodo, Title: "x"}).ID)
	code(t, err, apperr.CodeNotFound) // actions are scoped to their space
}

// SM-HUM-008: a repeated request with the same dedup key returns the original.
func TestDedup(t *testing.T) {
	f := newFx(t)
	a, created, err := f.svc.Create(f.ctx, op, "spc_a", CreateInput{Type: TypeTodo, Title: "one", DedupKey: "k", Requester: agent})
	must(t, err)
	b, again, err := f.svc.Create(f.ctx, op, "spc_a", CreateInput{Type: TypeTodo, Title: "two", DedupKey: "k", Requester: agent})
	must(t, err)
	if !created || again || a.ID != b.ID || b.Title != "one" {
		t.Fatalf("dedup: %v %v %+v", created, again, b)
	}
}

// SM-HUM-007: of concurrent responses exactly one wins.
func TestConcurrentResponsesOneWins(t *testing.T) {
	f := newFx(t)
	a := f.create(CreateInput{Type: TypeApproval, Title: "Merge?", Request: json.RawMessage(`{"action":"merge"}`)})
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Go(func() {
			outcome := "approved"
			if i%2 == 1 {
				outcome = "denied"
			}
			_, errs[i] = f.svc.Respond(f.ctx, op, "spc_a", a.ID, a.Version, RespondInput{Outcome: outcome, Response: Response{Note: "n"}})
		})
	}
	wg.Wait()
	wins := 0
	for _, err := range errs {
		if err == nil {
			wins++
		} else if c := apperr.CodeOf(err); c != apperr.CodeConflict && c != apperr.CodePrecondition {
			t.Fatalf("unexpected: %v", err)
		}
	}
	if wins != 1 {
		t.Fatalf("wins = %d", wins)
	}
	got, _ := f.svc.Get(f.ctx, "spc_a", a.ID)
	if len(got.Notifications) != 1 {
		t.Fatalf("notifications: %+v", got.Notifications)
	}
}

// SM-HUM-010: a blocking action blocks only its task; a response unblocks it without changing the task's state;
// expiry leaves the task blocked.
func TestBlockingAffectsOnlyTheTask(t *testing.T) {
	f := newFx(t)
	t1, _, err := f.q.Create(f.ctx, op, "spc_a", queue.CreateInput{Title: "t1", Status: queue.Ready})
	must(t, err)
	t2, _, err := f.q.Create(f.ctx, op, "spc_a", queue.CreateInput{Title: "t2", Status: queue.Ready})
	must(t, err)
	a := f.create(CreateInput{Type: TypeQuestion, Title: "Which?", TaskID: t1.ID, BlocksTask: true})
	sel, err := f.q.Next(f.ctx, op, "spc_a", false)
	must(t, err)
	if sel.Selected == nil || sel.Selected.ID != t2.ID || len(sel.Skipped) != 1 || sel.Skipped[0].TaskID != t1.ID {
		t.Fatalf("blocked selection: %+v", sel)
	}
	_, err = f.svc.Respond(f.ctx, op, "spc_a", a.ID, a.Version, RespondInput{Outcome: "answered", Response: Response{Answer: "this"}})
	must(t, err)
	got, err := f.q.Get(f.ctx, "spc_a", t1.ID)
	must(t, err)
	if got.Status != queue.Ready || len(got.Waiting) != 0 {
		t.Fatalf("unblocked: %+v", got)
	}

	exp := f.clk.Now().Add(time.Hour)
	b := f.create(CreateInput{Type: TypeApproval, Title: "ok?", Request: json.RawMessage(`{"action":"x"}`), TaskID: t2.ID, BlocksTask: true, ExpiresAt: &exp})
	f.clk.Advance(2 * time.Hour)
	n, err := f.svc.Expire(f.ctx)
	must(t, err)
	if n != 1 {
		t.Fatalf("expired %d", n)
	}
	eb, _ := f.svc.Get(f.ctx, "spc_a", b.ID)
	if eb.Status != Expired || eb.Outcome != "" || len(eb.Notifications) != 1 || eb.Notifications[0].Kind != "expired" {
		t.Fatalf("expired: %+v", eb)
	}
	g2, _ := f.q.Get(f.ctx, "spc_a", t2.ID)
	if len(g2.Waiting) == 0 {
		t.Fatalf("an expired approval must leave the task blocked: %+v", g2)
	}
	if n, _ := f.svc.Expire(f.ctx); n != 0 {
		t.Fatalf("expire twice: %d", n)
	}
}

// SM-HUM-006: comments are discussion, never responses; cancellation is a final status without an outcome.
func TestCommentsAssignCancel(t *testing.T) {
	f := newFx(t)
	a := f.create(CreateInput{Type: TypeTodo, Title: "Check logs"})
	c, err := f.svc.AddComment(f.ctx, op, "spc_a", a.ID, "looking", "Op")
	must(t, err)
	if c.Status != Open || len(c.Comments) != 1 || c.Comments[0].Body != "looking" {
		t.Fatalf("comment: %+v", c)
	}
	_, err = f.svc.AddComment(f.ctx, op, "spc_a", a.ID, "  ", "")
	code(t, err, apperr.CodeInvalid)
	_, err = f.svc.Assign(f.ctx, op, "spc_a", a.ID, c.Version, "usr_ghost")
	code(t, err, apperr.CodeInvalid)
	as, err := f.svc.Assign(f.ctx, op, "spc_a", a.ID, c.Version, "usr_op")
	must(t, err)
	if as.Status != InProgress || as.Assignee != "usr_op" {
		t.Fatalf("assign: %+v", as)
	}
	_, err = f.svc.Assign(f.ctx, op, "spc_a", a.ID, c.Version, "")
	code(t, err, apperr.CodePrecondition)
	un, err := f.svc.Assign(f.ctx, op, "spc_a", a.ID, as.Version, "")
	must(t, err)
	if un.Status != Open {
		t.Fatalf("unassign: %+v", un)
	}
	cx, err := f.svc.Cancel(f.ctx, op2, "spc_a", a.ID, 0, "no longer needed")
	must(t, err)
	if cx.Status != Cancelled || cx.Outcome != "" || cx.StatusReason != "no longer needed" {
		t.Fatalf("cancel: %+v", cx)
	}
	_, err = f.svc.Cancel(f.ctx, op, "spc_a", a.ID, 0, "")
	code(t, err, apperr.CodeConflict)
	// comments on a final action are allowed; a correction is a follow-up that keeps the original
	_, err = f.svc.AddComment(f.ctx, op, "spc_a", a.ID, "closed because …", "")
	must(t, err)
	fu := f.create(CreateInput{Type: TypeTodo, Title: "Check logs again", FollowUpOf: a.ID})
	if fu.FollowUpOf != a.ID {
		t.Fatalf("follow-up: %+v", fu)
	}
	_, _, err = f.svc.Create(f.ctx, op, "spc_b", CreateInput{Type: TypeTodo, Title: "x", FollowUpOf: a.ID, Requester: agent})
	code(t, err, apperr.CodeInvalid)
}

func TestOrderingListsAndCounts(t *testing.T) {
	f := newFx(t)
	a := f.create(CreateInput{Type: TypeTodo, Title: "a"})
	b := f.create(CreateInput{Type: TypeQuestion, Title: "b"})
	c := f.create(CreateInput{Type: TypeTodo, Title: "c"})
	_, _, err := f.svc.Create(f.ctx, op, "spc_b", CreateInput{Type: TypeTodo, Title: "other", Requester: agent})
	must(t, err)
	titles := func(l []*Action) (s string) {
		for _, x := range l {
			s += x.Title
		}
		return
	}
	_, err = f.svc.Move(f.ctx, op, "spc_a", c.ID, a.ID)
	must(t, err)
	l, err := f.svc.List(f.ctx, "spc_a", ListFilter{Pending: true})
	must(t, err)
	if titles(l) != "cab" {
		t.Fatalf("order = %s", titles(l))
	}
	_, err = f.svc.Move(f.ctx, op, "spc_a", c.ID, "")
	must(t, err)
	_, err = f.svc.Move(f.ctx, op, "spc_a", c.ID, "hum_nope")
	code(t, err, apperr.CodeInvalid)
	l, _ = f.svc.List(f.ctx, "spc_a", ListFilter{})
	if titles(l) != "abc" {
		t.Fatalf("order = %s", titles(l))
	}
	l, _ = f.svc.List(f.ctx, "spc_a", ListFilter{Type: TypeQuestion})
	if titles(l) != "b" {
		t.Fatalf("filter = %s", titles(l))
	}
	_, err = f.svc.Respond(f.ctx, op, "spc_a", b.ID, 0, RespondInput{Outcome: "answered", Response: Response{Answer: "y"}})
	must(t, err)
	l, _ = f.svc.List(f.ctx, "spc_a", ListFilter{})
	if titles(l) != "acb" { // final actions after pending ones
		t.Fatalf("with final = %s", titles(l))
	}
	g, err := f.svc.ListPending(f.ctx, func(id string) bool { return id == "spc_b" }, ListFilter{})
	must(t, err)
	if titles(g) != "other" {
		t.Fatalf("global = %s", titles(g))
	}
	g, _ = f.svc.ListPending(f.ctx, nil, ListFilter{})
	if len(g) != 3 {
		t.Fatalf("global all = %d", len(g))
	}
	counts, err := f.svc.PendingCounts(f.ctx)
	must(t, err)
	if counts["spc_a"] != 2 || counts["spc_b"] != 1 {
		t.Fatalf("counts = %v", counts)
	}
}

// SM-HUM-011: per-run pending limits bound a misbehaving agent.
func TestRequesterLimit(t *testing.T) {
	f := newFx(t)
	for range MaxPendingPerRequester {
		f.create(CreateInput{Type: TypeTodo, Title: "x"})
	}
	_, _, err := f.svc.Create(f.ctx, op, "spc_a", CreateInput{Type: TypeTodo, Title: "x", Requester: agent})
	code(t, err, apperr.CodeRateLimited)
	_, _, err = f.svc.Create(f.ctx, op, "spc_a", CreateInput{Type: TypeTodo, Title: "x", Requester: Requester{Kind: "operator", ID: "usr_op"}})
	must(t, err)
}

// SM-OBS-004, SM-HUI-007: the lifecycle of a human action (created, resolved, cancelled, expired) is published with safe
// metadata only. The title, body, request, response, note and reasons never reach the outbox.
func TestLifecycleEventsCarrySafeMetadataOnly(t *testing.T) {
	f := newFx(t)
	f.svc.Outbox = outbox.New(f.s, outbox.Options{Enabled: true, Clock: f.clk})
	secret := []string{"TITLE-SECRET", "BODY-SECRET", "REQUEST-SECRET", "ANSWER-SECRET", "NOTE-SECRET", "REASON-SECRET"}
	q := f.create(CreateInput{Type: TypeQuestion, Title: "Q TITLE-SECRET", Body: "BODY-SECRET", CorrelationID: "corr-1",
		Request: json.RawMessage(`{"options":["REQUEST-SECRET"],"allow_text":true}`)})
	_, err := f.svc.Respond(f.ctx, op, "spc_a", q.ID, q.Version, RespondInput{Outcome: "answered", Response: Response{Answer: "ANSWER-SECRET", Note: "NOTE-SECRET"}})
	must(t, err)
	c := f.create(CreateInput{Type: TypeApproval, Title: "TITLE-SECRET", Request: json.RawMessage(`{"action":"REQUEST-SECRET"}`)})
	_, err = f.svc.Cancel(f.ctx, op, "spc_a", c.ID, c.Version, "REASON-SECRET")
	must(t, err)
	exp := f.clk.Now().Add(time.Hour)
	f.create(CreateInput{Type: TypeTodo, Title: "TITLE-SECRET", Body: "BODY-SECRET", ExpiresAt: &exp})
	f.clk.Advance(2 * time.Hour)
	if n, err := f.svc.Expire(f.ctx); err != nil || n != 1 {
		t.Fatalf("expired %d %v", n, err)
	}
	rows, err := f.s.ReadDB().QueryContext(f.ctx, `SELECT type, correlation_id, causation_id, payload_json FROM event_outbox ORDER BY id`)
	must(t, err)
	defer rows.Close()
	var types []string
	for rows.Next() {
		var typ, corr, cause, payload string
		must(t, rows.Scan(&typ, &corr, &cause, &payload))
		types = append(types, typ)
		for _, s := range secret {
			if strings.Contains(payload+corr+cause, s) {
				t.Errorf("%s leaks %s: %s", typ, s, payload)
			}
		}
		var m map[string]any
		must(t, json.Unmarshal([]byte(payload), &m))
		for _, k := range []string{"title", "body", "request", "response", "note", "reason", "status_reason", "comments"} {
			if _, ok := m[k]; ok {
				t.Errorf("%s has %q", typ, k)
			}
		}
		if typ == "spaceman.human.action_resolved.v1" && (m["outcome"] != "answered" || corr != "corr-1") {
			t.Errorf("resolved %v corr %q", m, corr)
		}
	}
	must(t, rows.Err())
	want := []string{"spaceman.human.action_created.v1", "spaceman.human.action_resolved.v1", "spaceman.human.action_created.v1",
		"spaceman.human.action_cancelled.v1", "spaceman.human.action_created.v1", "spaceman.human.action_expired.v1"}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Fatalf("events %v", types)
	}
}
