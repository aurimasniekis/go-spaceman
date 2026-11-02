package approvals

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/human"
	"github.com/aurimasniekis/go-spaceman/internal/keyring/keyringtest"
	"github.com/aurimasniekis/go-spaceman/internal/outbox"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/storetest"
)

var (
	admin    = audit.Actor{Type: audit.ActorUser, ID: "usr_admin"}
	approver = audit.Actor{Type: audit.ActorUser, ID: "usr_ops"}
)

type fx struct {
	t      *testing.T
	ctx    context.Context
	s      *store.Store
	clk    *clock.Fake
	human  *human.Service
	svc    *Service
	authOK atomic.Bool
}

func newFx(t *testing.T) *fx {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC))
	s := storetest.Open(t, storetest.WithClock(clk))
	kr := keyringtest.Load(t, s)
	aw, ch := audit.NewWriter(kr), changefeed.New(nil)
	h := human.New(s, aw, ch, clk)
	f := &fx{t: t, ctx: context.Background(), s: s, clk: clk, human: h}
	f.svc = New(s, aw, ch, h, nil)
	f.authOK.Store(true)
	f.svc.StillAuthorized = func(_ context.Context, by audit.Actor, spaceID string) bool {
		return f.authOK.Load() && by.ID == approver.ID && spaceID == "spc_a"
	}
	must(t, s.W().TxNamed(f.ctx, "seed", func(tx *store.Tx) error {
		for _, q := range []string{
			`INSERT INTO nodes (id, name, state, created_at, created_by, updated_at) VALUES ('nod_1', 'n1', 'active', 0, 't', 0)`,
			`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('spc_a', 'aaa', 'A', 'nod_1', 'active', 'a', 0, 't', 0)`,
			`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('spc_b', 'bbb', 'B', 'nod_1', 'active', 'b', 0, 't', 0)`,
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

var run1 = &fleet.RunPrincipal{RunID: "run_1", SpaceID: "spc_a", Kind: "control", Role: "control"}

func (f *fx) policy(op string, uses int64) *Policy {
	f.t.Helper()
	p, err := f.svc.CreatePolicy(f.ctx, admin, PolicyInput{SpaceID: "spc_a", Operation: op, Uses: uses})
	must(f.t, err)
	return p
}

// gateRequired calls the gate expecting "approval required" and returns the approval it points at.
func (f *fx) gateRequired(p *fleet.RunPrincipal, op string, args string) *Approval {
	f.t.Helper()
	finish, err := f.svc.Gate(f.ctx, p, op, "spaceman", json.RawMessage(args))
	if finish != nil || !errors.Is(err, ErrRequired) || apperr.CodeOf(err) != apperr.CodePrecondition {
		f.t.Fatalf("expected approval required, got finish=%v err=%v", finish != nil, err)
	}
	msg := err.Error()
	l, err := f.svc.List(f.ctx, p.SpaceID, 0)
	must(f.t, err)
	for _, a := range l {
		if strings.Contains(msg, a.ID) && strings.Contains(msg, a.HumanActionID) && a.State == Pending && a.RequesterRun == p.RunID {
			return a
		}
	}
	f.t.Fatalf("no pending approval named in %q", msg)
	return nil
}

func (f *fx) decide(a *Approval, outcome, note string) {
	f.t.Helper()
	act, err := f.human.Get(f.ctx, a.SpaceID, a.HumanActionID)
	must(f.t, err)
	_, err = f.human.Respond(f.ctx, approver, a.SpaceID, act.ID, act.Version, human.RespondInput{Outcome: outcome, Response: human.Response{Note: note}})
	must(f.t, err)
}

func TestPolicies(t *testing.T) {
	f := newFx(t)
	for _, in := range []PolicyInput{
		{Operation: "rm -rf"},
		{Operation: "spaceman.human_ask"},
		{Operation: "spaceman.messages"},
		{Operation: "mcp.Bad.x"},
		{Operation: "spaceman.task_create", Uses: 500},
		{Operation: "spaceman.task_create", ValidSeconds: 5},
		{Operation: "spaceman.task_create", RequestTTLSeconds: 10},
	} {
		_, err := f.svc.CreatePolicy(f.ctx, admin, in)
		code(t, err, apperr.CodeInvalid)
	}
	_, err := f.svc.CreatePolicy(f.ctx, admin, PolicyInput{SpaceID: "spc_missing", Operation: "spaceman.task_create"})
	code(t, err, apperr.CodeNotFound)
	g, err := f.svc.CreatePolicy(f.ctx, admin, PolicyInput{Operation: "mcp.*"})
	must(t, err)
	if g.Uses != 1 || g.ValidSeconds != 3600 || g.RequestTTLSeconds != 86400 {
		t.Fatalf("defaults: %+v", g)
	}
	_, err = f.svc.CreatePolicy(f.ctx, admin, PolicyInput{Operation: "mcp.*"})
	code(t, err, apperr.CodeAlreadyExists)
	exact, err := f.svc.CreatePolicy(f.ctx, admin, PolicyInput{Operation: "mcp.tickets.ticket_comment", Uses: 3})
	must(t, err)
	space, err := f.svc.CreatePolicy(f.ctx, admin, PolicyInput{SpaceID: "spc_a", Operation: "mcp.tickets.*", Uses: 2})
	must(t, err)

	// A space policy wins over a global one; within a scope an exact operation wins over a prefix.
	for _, c := range []struct{ space, op, want string }{
		{"spc_a", "mcp.tickets.ticket_comment", space.ID},
		{"spc_b", "mcp.tickets.ticket_comment", exact.ID},
		{"spc_b", "mcp.other.x", g.ID},
		{"spc_a", "spaceman.task_create", ""},
		{"spc_a", "spaceman.human_ask", ""},
	} {
		p, err := f.svc.policyFor(f.ctx, c.space, c.op)
		must(t, err)
		got := ""
		if p != nil {
			got = p.ID
		}
		if got != c.want {
			t.Errorf("%s in %s: got %q, want %q", c.op, c.space, got, c.want)
		}
	}
	l, err := f.svc.Policies(f.ctx)
	must(t, err)
	if len(l) != 3 {
		t.Fatalf("policies: %d", len(l))
	}
	must(t, f.svc.DeletePolicy(f.ctx, admin, g.ID))
	code(t, f.svc.DeletePolicy(f.ctx, admin, g.ID), apperr.CodeNotFound)
	// No policy: nothing is gated.
	finish, err := f.svc.Gate(f.ctx, run1, "mcp.other.x", "r", nil)
	if finish != nil || err != nil {
		t.Fatalf("ungated: %v %v", finish != nil, err)
	}
}

func TestDigest(t *testing.T) {
	d1, _, err := Digest("op", "r", json.RawMessage(`{"a":1,"b":{"y":2,"x":[1,"2"]}}`))
	must(t, err)
	d2, _, _ := Digest("op", "r", json.RawMessage(` {"b":{"x":[1,"2"],"y":2},"a":1} `))
	d3, _, _ := Digest("op", "r", json.RawMessage(`{"a":1.0,"b":{"y":2,"x":[1,"2"]}}`))
	d4, _, _ := Digest("op", "other", json.RawMessage(`{"a":1,"b":{"y":2,"x":[1,"2"]}}`))
	d5, _, _ := Digest("op", "r", nil)
	d6, _, _ := Digest("op", "r", json.RawMessage(`{}`))
	if d1 != d2 || d1 == d3 || d1 == d4 || d5 != d6 {
		t.Fatalf("digests: %s %s %s %s %s %s", d1, d2, d3, d4, d5, d6)
	}
	if _, _, err := Digest("op", "r", json.RawMessage(`{bad`)); apperr.CodeOf(err) != apperr.CodeInvalid {
		t.Fatalf("bad JSON: %v", err)
	}
	p := preview(`{"api_key":"k","nested":{"Password":"p","ok":"visible"},"list":[{"token":"t"}]}`)
	if strings.Contains(p, `"k"`) || strings.Contains(p, `"p"`) || strings.Contains(p, `"t"`) || !strings.Contains(p, "visible") {
		t.Fatalf("preview: %s", p)
	}
}

func TestApproveExecuteOnce(t *testing.T) {
	f := newFx(t)
	f.policy("spaceman.task_create", 1)
	args := `{"title":"Deploy","token":"s3cret"}`
	a := f.gateRequired(run1, "spaceman.task_create", args)
	act, err := f.human.Get(f.ctx, "spc_a", a.HumanActionID)
	must(t, err)
	if act.Type != human.TypeApproval || !strings.Contains(act.Body, a.ArgsDigest) || strings.Contains(act.Body, "s3cret") || act.ExpiresAt == nil {
		t.Fatalf("human action: %+v", act)
	}
	if strings.Contains(a.ArgsPreview, "s3cret") {
		t.Fatal("secret in preview")
	}
	// Asking again with the same arguments points at the same request; changed arguments need a new one.
	again := f.gateRequired(run1, "spaceman.task_create", `{"token":"s3cret","title":"Deploy"}`)
	if again.ID != a.ID {
		t.Fatal("a duplicate request was created")
	}
	other := f.gateRequired(run1, "spaceman.task_create", `{"title":"Deploy now","token":"s3cret"}`)
	if other.ID == a.ID {
		t.Fatal("changed arguments reused the approval")
	}
	// The advisory/enforced distinction: only policy-created actions have an approval.
	if _, err := f.svc.ForAction(f.ctx, "spc_a", a.HumanActionID); err != nil {
		t.Fatal(err)
	}
	adv, _, err := f.human.Create(f.ctx, admin, "spc_a", human.CreateInput{Type: human.TypeApproval, Title: "Advisory", Request: json.RawMessage(`{"action":"x"}`),
		Requester: human.Requester{Kind: "operator", ID: admin.ID}})
	must(t, err)
	_, err = f.svc.ForAction(f.ctx, "spc_a", adv.ID)
	code(t, err, apperr.CodeNotFound)

	f.decide(a, "approved", "")
	got, err := f.svc.Get(f.ctx, "spc_a", a.ID)
	must(t, err)
	if got.State != Approved || got.ValidUntil == nil || got.DecidedBy != approver.ID {
		t.Fatalf("decided: %+v", got)
	}
	// Another run with the same arguments cannot use it.
	run2 := &fleet.RunPrincipal{RunID: "run_2", SpaceID: "spc_a", Kind: "control", Role: "control"}
	f.gateRequired(run2, "spaceman.task_create", args)

	finish, err := f.svc.Gate(f.ctx, run1, "spaceman.task_create", "spaceman", json.RawMessage(args))
	if err != nil || finish == nil {
		t.Fatalf("approved call refused: %v", err)
	}
	finish(nil)
	got, _ = f.svc.Get(f.ctx, "spc_a", a.ID)
	if got.UsesLeft != 0 || len(got.Executions) != 1 || got.Executions[0].State != Succeeded || got.State != Approved {
		t.Fatalf("after execution: %+v", got)
	}
	// Used up: the next identical call needs a new approval (no replay).
	n := f.gateRequired(run1, "spaceman.task_create", args)
	if n.ID == a.ID {
		t.Fatal("a used approval was offered again")
	}
}

func TestDecisionsAndRechecks(t *testing.T) {
	f := newFx(t)
	f.policy("spaceman.worker_launch", 2)

	// Denied.
	a := f.gateRequired(run1, "spaceman.worker_launch", `{"task_id":"t1"}`)
	f.decide(a, "denied", "not now")
	if g, _ := f.svc.Get(f.ctx, "spc_a", a.ID); g.State != Denied {
		t.Fatalf("denied: %+v", g)
	}

	// Two uses, a failed execution, and an approver who loses the right to decide.
	b := f.gateRequired(run1, "spaceman.worker_launch", `{"task_id":"t2"}`)
	f.decide(b, "approved", "")
	finish, err := f.svc.Gate(f.ctx, run1, "spaceman.worker_launch", "spaceman", json.RawMessage(`{"task_id":"t2"}`))
	must(t, err)
	finish(apperr.New(apperr.CodeConflict, "busy"))
	g, _ := f.svc.Get(f.ctx, "spc_a", b.ID)
	if g.UsesLeft != 1 || g.Executions[0].State != Failed || g.Executions[0].Error != string(apperr.CodeConflict) {
		t.Fatalf("failed execution: %+v", g)
	}
	f.authOK.Store(false)
	f.gateRequired(run1, "spaceman.worker_launch", `{"task_id":"t2"}`)
	if g, _ := f.svc.Get(f.ctx, "spc_a", b.ID); g.State != Revoked || !strings.Contains(g.StateReason, "can no longer decide") {
		t.Fatalf("approver recheck: %+v", g)
	}
	f.authOK.Store(true)

	// Revoked by an operator; revoking again is refused.
	c := f.gateRequired(run1, "spaceman.worker_launch", `{"task_id":"t3"}`)
	f.decide(c, "approved", "")
	_, err = f.svc.Revoke(f.ctx, admin, "spc_a", c.ID, "changed my mind")
	must(t, err)
	_, err = f.svc.Revoke(f.ctx, admin, "spc_a", c.ID, "")
	code(t, err, apperr.CodePrecondition)
	_, err = f.svc.Revoke(f.ctx, admin, "spc_b", c.ID, "")
	code(t, err, apperr.CodeNotFound)
	f.gateRequired(run1, "spaceman.worker_launch", `{"task_id":"t3"}`)

	// Withdrawn request.
	d := f.gateRequired(run1, "spaceman.worker_launch", `{"task_id":"t4"}`)
	act, _ := f.human.Get(f.ctx, "spc_a", d.HumanActionID)
	_, err = f.human.Cancel(f.ctx, admin, "spc_a", act.ID, act.Version, "not needed")
	must(t, err)
	if g, _ := f.svc.Get(f.ctx, "spc_a", d.ID); g.State != Revoked {
		t.Fatalf("withdrawn: %+v", g)
	}

	// An approval that is not used in time expires; so does a request nobody decides.
	e := f.gateRequired(run1, "spaceman.worker_launch", `{"task_id":"t5"}`)
	f.decide(e, "approved", "")
	pend := f.gateRequired(run1, "spaceman.worker_launch", `{"task_id":"t6"}`)
	f.clk.Advance(2 * time.Hour)
	f.gateRequired(run1, "spaceman.worker_launch", `{"task_id":"t5"}`)
	n, err := f.svc.Expire(f.ctx)
	must(t, err)
	if g, _ := f.svc.Get(f.ctx, "spc_a", e.ID); n != 1 || g.State != Expired {
		t.Fatalf("expired approval: %d %+v", n, g)
	}
	f.clk.Advance(25 * time.Hour)
	_, err = f.human.Expire(f.ctx)
	must(t, err)
	if g, _ := f.svc.Get(f.ctx, "spc_a", pend.ID); g.State != Expired {
		t.Fatalf("expired request: %+v", g)
	}
}

// SM-APPR-003: two calls racing for the last use cannot both run.
func TestConcurrentUseRunsOnce(t *testing.T) {
	f := newFx(t)
	f.policy("spaceman.task_create", 1)
	a := f.gateRequired(run1, "spaceman.task_create", `{"title":"x"}`)
	f.decide(a, "approved", "")
	var ran atomic.Int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			finish, err := f.svc.Gate(f.ctx, run1, "spaceman.task_create", "spaceman", json.RawMessage(`{"title":"x"}`))
			if err == nil && finish != nil {
				ran.Add(1)
				finish(nil)
			}
		}()
	}
	wg.Wait()
	if ran.Load() != 1 {
		t.Fatalf("ran %d times", ran.Load())
	}
}

// A restart during an execution marks it interrupted; it is never retried.
func TestRecoverInterrupted(t *testing.T) {
	f := newFx(t)
	f.policy("spaceman.task_create", 2)
	a := f.gateRequired(run1, "spaceman.task_create", `{"title":"x"}`)
	f.decide(a, "approved", "")
	finish, err := f.svc.Gate(f.ctx, run1, "spaceman.task_create", "spaceman", json.RawMessage(`{"title":"x"}`))
	must(t, err)
	_ = finish // central "crashes" before the call finishes
	n, err := f.svc.Recover(f.ctx)
	must(t, err)
	g, _ := f.svc.Get(f.ctx, "spc_a", a.ID)
	if n != 1 || g.Executions[0].State != Interrupted || g.UsesLeft != 1 {
		t.Fatalf("recover: %d %+v", n, g)
	}
	finish(nil) // a late finish does not rewrite the interrupted record
	g, _ = f.svc.Get(f.ctx, "spc_a", a.ID)
	if g.Executions[0].State != Interrupted {
		t.Fatalf("late finish: %+v", g.Executions[0])
	}
	l, err := f.svc.List(f.ctx, "spc_a", 10)
	must(t, err)
	if len(l) != 1 {
		t.Fatalf("list: %d", len(l))
	}
}

// SM-OBS-004: the decision and each execution of an enforced approval are recorded for publication with the approval
// as correlation, and never its arguments, preview or resource.
func TestApprovalEventsRecorded(t *testing.T) {
	f := newFx(t)
	ob := outbox.New(f.s, outbox.Options{Enabled: true, Clock: f.clk})
	f.svc.Outbox, f.human.Outbox = ob, ob
	f.policy("spaceman.task_create", 1)
	args := `{"title":"ARG-SECRET","token":"s3cret"}`
	a := f.gateRequired(run1, "spaceman.task_create", args)
	f.decide(a, "approved", "")
	finish, err := f.svc.Gate(f.ctx, run1, "spaceman.task_create", "spaceman", json.RawMessage(args))
	if err != nil || finish == nil {
		t.Fatalf("approved call refused: %v", err)
	}
	finish(nil)
	rows, err := f.s.ReadDB().QueryContext(f.ctx, `SELECT type, correlation_id, causation_id, payload_json FROM event_outbox WHERE type LIKE 'spaceman.approvals.%' ORDER BY id`)
	must(t, err)
	defer rows.Close()
	var got []string
	for rows.Next() {
		var typ, corr, cause, payload string
		must(t, rows.Scan(&typ, &corr, &cause, &payload))
		if corr != a.ID || strings.Contains(payload, "SECRET") || strings.Contains(payload, "s3cret") || strings.Contains(payload, a.ArgsDigest) {
			t.Fatalf("%s %s %s", typ, corr, payload)
		}
		got = append(got, typ+"|"+cause)
		if typ == "spaceman.approvals.approval_decided.v1" && !strings.Contains(payload, `"state":"approved"`) {
			t.Fatalf("decided %s", payload)
		}
		if typ == "spaceman.approvals.approval_executed.v1" && !strings.Contains(payload, `"result":"succeeded"`) {
			t.Fatalf("executed %s", payload)
		}
	}
	must(t, rows.Err())
	if strings.Join(got, ",") != "spaceman.approvals.approval_decided.v1|"+a.HumanActionID+",spaceman.approvals.approval_executed.v1|run_1" {
		t.Fatalf("events %v", got)
	}
}
