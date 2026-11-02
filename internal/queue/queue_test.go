package queue

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/keyring/keyringtest"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/storetest"
)

var op = audit.Actor{Type: audit.ActorUser, ID: "usr_op"}

type fx struct {
	t   *testing.T
	ctx context.Context
	s   *store.Store
	svc *Service
}

func newFx(t *testing.T) *fx {
	t.Helper()
	s := storetest.Open(t, storetest.WithClock(clock.NewFake(time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC))))
	kr := keyringtest.Load(t, s)
	f := &fx{t: t, ctx: context.Background(), s: s, svc: New(s, audit.NewWriter(kr), changefeed.New(nil))}
	must(t, s.W().TxNamed(f.ctx, "seed", func(tx *store.Tx) error {
		for _, q := range []string{
			`INSERT INTO nodes (id, name, state, created_at, created_by, updated_at) VALUES ('nod_1', 'n1', 'active', 0, 't', 0)`,
			`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('spc_a', 'aaa', 'A', 'nod_1', 'active', 'a', 0, 't', 0)`,
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

func (f *fx) add(in CreateInput) *Task {
	f.t.Helper()
	if in.Status == "" {
		in.Status = Ready
	}
	t, _, err := f.svc.Create(f.ctx, op, "spc_a", in)
	must(f.t, err)
	return t
}

func (f *fx) order() string {
	f.t.Helper()
	l, _, err := f.svc.List(f.ctx, "spc_a")
	must(f.t, err)
	var out []string
	for _, t := range l {
		out = append(out, strings.Repeat(">", t.Depth)+t.Title)
	}
	return strings.Join(out, " ")
}

func (f *fx) get(id string) *Task {
	t, err := f.svc.Get(f.ctx, "spc_a", id)
	must(f.t, err)
	return t
}

// SM-QUEUE-001/002: priority decides insertion, operator moves are authoritative, incidents go to the top by policy, a
// duplicate trigger never re-promotes a demoted incident, and escalation reorders only when the policy allows it.
func TestOrderingAndIncidents(t *testing.T) {
	f := newFx(t)
	f.add(CreateInput{Title: "a"})
	f.add(CreateInput{Title: "b"})
	f.add(CreateInput{Title: "low", Priority: "low"})
	f.add(CreateInput{Title: "hot", Priority: "high"})
	if got := f.order(); got != "hot a b low" {
		t.Fatalf("priority insertion: %s", got)
	}
	inc, created, err := f.svc.Create(f.ctx, op, "spc_a", CreateInput{Title: "outage", Kind: "incident", Status: Ready, DedupKey: "alert:db"})
	if err != nil || !created || f.order() != "outage hot a b low" {
		t.Fatalf("incident insertion: %v %s", err, f.order())
	}
	// The operator drags the incident down: it stays there.
	_, err = f.svc.Move(f.ctx, op, "spc_a", inc.ID, MoveInput{To: "bottom"})
	must(t, err)
	if got := f.order(); got != "hot a b low outage" {
		t.Fatalf("move: %s", got)
	}
	again, created, err := f.svc.Create(f.ctx, op, "spc_a", CreateInput{Title: "outage again", Kind: "incident", Status: Ready, DedupKey: "alert:db"})
	if err != nil || created || again.ID != inc.ID || f.order() != "hot a b low outage" || !again.Manual {
		t.Fatalf("duplicate trigger re-promoted the incident: %v created=%v %s", err, created, f.order())
	}
	// Escalation without the reorder policy only raises priority.
	esc, err := f.svc.Escalate(f.ctx, op, "spc_a", inc.ID, "customer impact")
	if err != nil || esc.Priority != "urgent" || f.order() != "hot a b low outage" {
		t.Fatalf("escalation moved without policy: %v %s", err, f.order())
	}
	_, err = f.svc.SetPolicy(f.ctx, op, "spc_a", Policy{MaxActive: 1, IncidentInsertion: "top", EscalationReorders: true})
	must(t, err)
	_, err = f.svc.Escalate(f.ctx, op, "spc_a", inc.ID, "customer impact")
	must(t, err)
	if got := f.order(); got != "outage hot a b low" {
		t.Fatalf("escalation with policy: %s", got)
	}
	// Before/after placement and many moves (rank rebalancing).
	l, _, _ := f.svc.List(f.ctx, "spc_a")
	for i := 0; i < 40; i++ {
		_, err := f.svc.Move(f.ctx, op, "spc_a", l[4].ID, MoveInput{AfterID: l[0].ID})
		must(t, err)
		_, err = f.svc.Move(f.ctx, op, "spc_a", l[4].ID, MoveInput{BeforeID: l[1].ID})
		must(t, err)
	}
	if got := f.order(); got != "outage low hot a b" {
		t.Fatalf("after repeated moves: %s", got)
	}
}

// SM-QUEUE-004/005/006/007: cycles are rejected; blocked, dependent and excluded tasks are skipped with reasons while
// the next eligible task is selected; the default allows one running top-level task.
func TestEligibility(t *testing.T) {
	f := newFx(t)
	a := f.add(CreateInput{Title: "deploy", Exclusions: []string{"env:staging"}})
	b := f.add(CreateInput{Title: "migrate", DependsOn: []string{a.ID}})
	c := f.add(CreateInput{Title: "docs"})
	if _, err := f.svc.SetDependencies(f.ctx, op, "spc_a", a.ID, []string{b.ID}); apperr.CodeOf(err) != apperr.CodeInvalid || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("cycle accepted: %v", err)
	}
	if _, err := f.svc.SetDependencies(f.ctx, op, "spc_a", a.ID, []string{a.ID}); err == nil {
		t.Fatal("self dependency accepted")
	}
	_, err := f.svc.AddBlocker(f.ctx, op, "spc_a", a.ID, "human_action", "hum_1", "")
	must(t, err)
	sel, err := f.svc.Next(f.ctx, op, "spc_a", true)
	must(t, err)
	if sel.Selected == nil || sel.Selected.ID != c.ID || len(sel.Skipped) != 2 ||
		!strings.Contains(sel.Skipped[0].Reason, "human action hum_1") || !strings.Contains(sel.Skipped[1].Reason, "waiting for \"deploy\"") {
		t.Fatalf("selection %+v", sel)
	}
	if d, _ := f.svc.Decisions(f.ctx, "spc_a", 5); len(d) != 1 || d[0].SelectedID != c.ID || len(d[0].Skipped) != 2 {
		t.Fatalf("decision %+v", d)
	}
	// A blocked task cannot start; once the human action is answered it can.
	if _, err := f.svc.SetStatus(f.ctx, op, "spc_a", a.ID, f.get(a.ID).Version, StatusInput{Status: Running}); apperr.CodeOf(err) != apperr.CodePrecondition {
		t.Fatalf("blocked task started: %v", err)
	}
	must(t, f.s.W().TxNamed(f.ctx, "answer", func(tx *store.Tx) error {
		ids, err := ResolveBlockersByRef(f.ctx, tx, "human_action", "hum_1", "usr_op")
		if err == nil && (len(ids) != 1 || ids[0] != a.ID) {
			t.Errorf("resolved %v", ids)
		}
		return err
	}))
	_, err = f.svc.SetStatus(f.ctx, op, "spc_a", a.ID, f.get(a.ID).Version, StatusInput{Status: Running})
	must(t, err)
	// One running top-level task by default: the next one waits for capacity.
	sel, _ = f.svc.Next(f.ctx, op, "spc_a", false)
	if sel.Selected != nil || !strings.Contains(sel.Skipped[len(sel.Skipped)-1].Reason, "limit 1") {
		t.Fatalf("active limit: %+v", sel)
	}
	// With two allowed, exclusion still keeps a staging task away from the running deploy.
	_, err = f.svc.SetPolicy(f.ctx, op, "spc_a", Policy{MaxActive: 2, IncidentInsertion: "top"})
	must(t, err)
	stg := f.add(CreateInput{Title: "smoke staging", Exclusions: []string{"env:staging"}})
	sel, _ = f.svc.Next(f.ctx, op, "spc_a", false)
	if sel.Selected == nil || sel.Selected.ID != c.ID {
		t.Fatalf("selection with two slots: %+v", sel)
	}
	if w := f.get(stg.ID).Waiting; len(w) == 0 || !strings.Contains(w[0], "env:staging") {
		t.Fatalf("exclusion not reported: %v", w)
	}
	// Subtasks run under a running parent.
	sub := f.add(CreateInput{Title: "worker step", ParentID: a.ID})
	if f.get(sub.ID).Depth != 1 || len(f.get(sub.ID).Waiting) != 0 {
		t.Fatalf("subtask %+v", f.get(sub.ID))
	}
}

// SM-QUEUE-008/009/010: preemption returns work to ready (counted, not failed); reordering does not change state;
// nothing is accepted automatically; illegal transitions are refused.
func TestStatesAndPreemption(t *testing.T) {
	f := newFx(t)
	_, err := f.svc.SetPolicy(f.ctx, op, "spc_a", Policy{MaxActive: 1, IncidentInsertion: "top", IncidentPreempts: true})
	must(t, err)
	work := f.add(CreateInput{Title: "feature"})
	_, err = f.svc.SetStatus(f.ctx, op, "spc_a", work.ID, work.Version, StatusInput{Status: Running})
	must(t, err)
	_, err = f.svc.Move(f.ctx, op, "spc_a", work.ID, MoveInput{To: "bottom"})
	must(t, err)
	if f.get(work.ID).Status != Running {
		t.Fatal("reordering changed a running task")
	}
	f.add(CreateInput{Title: "incident", Kind: "incident"})
	w := f.get(work.ID)
	if w.Status != Ready || w.Preemptions != 1 || !strings.Contains(w.StatusReason, "preempted") {
		t.Fatalf("preemption: %+v", w)
	}
	if _, err := f.svc.SetStatus(f.ctx, op, "spc_a", work.ID, w.Version, StatusInput{Status: Done}); apperr.CodeOf(err) != apperr.CodePrecondition {
		t.Fatalf("ready → done accepted: %v", err)
	}
	if _, err := f.svc.SetStatus(f.ctx, op, "spc_a", work.ID, w.Version-1, StatusInput{Status: Cancelled}); apperr.CodeOf(err) != apperr.CodePrecondition {
		t.Fatalf("stale version accepted: %v", err)
	}
	c, err := f.svc.SetStatus(f.ctx, op, "spc_a", work.ID, w.Version, StatusInput{Status: Cancelled, Reason: "superseded"})
	if err != nil || c.FinishedAt == nil {
		t.Fatalf("cancel: %+v %v", c, err)
	}
	up, err := f.svc.Update(f.ctx, op, "spc_a", work.ID, c.Version, UpdateInput{Title: "feature v2", Priority: "high", Exclusions: []string{"repo:api"}})
	if err != nil || up.Title != "feature v2" || up.Exclusions[0] != "repo:api" {
		t.Fatalf("update: %+v %v", up, err)
	}
	if _, err := f.svc.Update(f.ctx, op, "spc_a", work.ID, up.Version, UpdateInput{Title: "x", Exclusions: []string{"has space"}}); apperr.CodeOf(err) != apperr.CodeInvalid {
		t.Fatalf("bad exclusion: %v", err)
	}
}
