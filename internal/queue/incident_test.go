package queue

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/store"
)

// fakeWorkers records what the queue asked the fleet to do with worker runs.
type fakeWorkers struct {
	live  map[string]bool
	calls []string
}

func (w *fakeWorkers) WorkerActive(_ context.Context, _ *store.Tx, _, run string) (bool, error) {
	return w.live[run], nil
}
func (w *fakeWorkers) PauseWorkerTx(_ context.Context, _ *store.Tx, _ audit.Actor, _, run, _, _ string) (bool, error) {
	w.calls = append(w.calls, "pause:"+run)
	return true, nil
}
func (w *fakeWorkers) StopWorkerTx(_ context.Context, _ *store.Tx, _ audit.Actor, _, run, _ string) (bool, error) {
	w.calls = append(w.calls, "stop:"+run)
	w.live[run] = false
	return true, nil
}
func (w *fakeWorkers) NoticeWorkerTx(_ context.Context, _ *store.Tx, _, run, text string) (bool, error) {
	kind := "notice"
	if strings.Contains(text, "Continue where") {
		kind = "continue"
	}
	w.calls = append(w.calls, kind+":"+run)
	return true, nil
}
func (w *fakeWorkers) KickRun(context.Context, string) {}

func (w *fakeWorkers) take() string {
	s := strings.Join(w.calls, " ")
	w.calls = nil
	return s
}

func (f *fx) auditCount(action string) int {
	f.t.Helper()
	var n int
	must(f.t, f.s.ReadDB().QueryRowContext(f.ctx, `SELECT count(*) FROM audit_events WHERE action = ?`, action).Scan(&n))
	return n
}

// running starts a ready top-level task assigned to run (and a running subtask assigned to sub, when given).
func (f *fx) running(title, run, sub string) *Task {
	f.t.Helper()
	t := f.add(CreateInput{Title: title})
	t, err := f.svc.SetStatus(f.ctx, op, "spc_a", t.ID, t.Version, StatusInput{Status: Running})
	must(f.t, err)
	t, err = f.svc.Update(f.ctx, op, "spc_a", t.ID, t.Version, UpdateInput{Title: t.Title, Priority: t.Priority, AssigneeRun: run})
	must(f.t, err)
	if sub != "" {
		s := f.add(CreateInput{Title: title + "-sub", ParentID: t.ID})
		s, err = f.svc.SetStatus(f.ctx, op, "spc_a", s.ID, s.Version, StatusInput{Status: Running})
		must(f.t, err)
		_, err = f.svc.Update(f.ctx, op, "spc_a", s.ID, s.Version, UpdateInput{Title: s.Title, Priority: s.Priority, AssigneeRun: sub})
		must(f.t, err)
	}
	return f.get(t.ID)
}

// SM-QUEUE-009: only trusted triggers raise incidents; anything else is proposed (an ordinary task that neither jumps
// the queue nor preempts) or refused, as the policy says, and the refusal is audited. Operators confirm proposals.
func TestIncidentTriggers(t *testing.T) {
	f := newFx(t)
	var notes []string
	f.svc.Notify = func(_, _, subject, _ string) { notes = append(notes, subject) }
	_, err := f.svc.SetPolicy(f.ctx, op, "spc_a", Policy{MaxActive: 1, IncidentInsertion: "top", IncidentPreempts: true})
	must(t, err)
	if _, p, _ := f.svc.List(f.ctx, "spc_a"); !slices.Equal(p.IncidentTriggers, []string{"control", "operator"}) || p.IncidentUntrusted != "propose" ||
		p.IncidentWorkers != "finish" || p.IncidentResume != "manual" {
		t.Fatalf("defaults %+v", p)
	}
	work := f.running("work", "", "")
	// control and operators are trusted by default
	ctrl := f.add(CreateInput{Title: "ctrl-incident", Kind: "incident", Triggers: []string{TriggerControl}})
	if ctrl.Kind != KindIncident || ctrl.IncidentTrigger != TriggerControl || ctrl.Position != 1 || f.get(work.ID).Status != Ready {
		t.Fatalf("control incident %+v work %s", ctrl, f.get(work.ID).Status)
	}
	// an event subscription nobody listed is only proposed: ordinary, inserted by priority, nothing preempted
	_, err = f.svc.SetStatus(f.ctx, op, "spc_a", ctrl.ID, ctrl.Version, StatusInput{Status: Cancelled})
	must(t, err)
	w := f.get(work.ID)
	_, err = f.svc.SetStatus(f.ctx, op, "spc_a", w.ID, w.Version, StatusInput{Status: Running})
	must(t, err)
	sub := "subscription:sub_0000000000000000000000000x"
	prop := f.add(CreateInput{Title: "event-incident", Kind: "incident", Triggers: []string{sub, "template:github-issues"}})
	if prop.Kind != KindOrdinary || prop.IncidentProposedBy != sub || prop.IncidentTrigger != "" || f.get(work.ID).Status != Running {
		t.Fatalf("proposal %+v", prop)
	}
	if f.auditCount("task.incident_proposed") != 1 || !slices.Contains(notes, "Incident proposed: event-incident") {
		t.Fatalf("proposal not audited or control not told: %v", notes)
	}
	// without any trigger the request is untrusted too
	if anon := f.add(CreateInput{Title: "anon", Kind: "incident"}); anon.Kind != KindOrdinary || anon.IncidentProposedBy != "unknown" {
		t.Fatalf("anonymous incident %+v", anon)
	}
	// trusting the template (by slug) lets it raise incidents
	_, err = f.svc.SetPolicy(f.ctx, op, "spc_a", Policy{MaxActive: 1, IncidentInsertion: "top", IncidentTriggers: []string{"operator", "template:github-issues"},
		IncidentUntrusted: "refuse"})
	must(t, err)
	tpl := f.add(CreateInput{Title: "tpl-incident", Kind: "incident", Triggers: []string{sub, "template:github-issues"}})
	if tpl.Kind != KindIncident || tpl.IncidentTrigger != "template:github-issues" {
		t.Fatalf("trusted template %+v", tpl)
	}
	// control is no longer trusted, and the policy refuses: the refusal is audited although nothing was created
	if _, _, err := f.svc.Create(f.ctx, op, "spc_a", CreateInput{Title: "refused", Kind: "incident", Triggers: []string{TriggerControl}}); apperr.CodeOf(err) != apperr.CodeForbidden {
		t.Fatalf("untrusted control incident: %v", err)
	}
	if f.auditCount("task.incident_refused") != 1 {
		t.Fatal("refusal not audited")
	}
	// ordinary tasks are never affected by the trigger list
	f.add(CreateInput{Title: "plain", Triggers: []string{TriggerControl}})
	// a bad trigger is refused; workers and schedules can never be trusted
	for _, bad := range []string{"worker", "schedule:sch_x", "subscription:nope", "template:"} {
		if _, err := f.svc.SetPolicy(f.ctx, op, "spc_a", Policy{MaxActive: 1, IncidentInsertion: "top", IncidentTriggers: []string{bad}}); apperr.CodeOf(err) != apperr.CodeInvalid {
			t.Fatalf("trigger %q accepted: %v", bad, err)
		}
	}
}

// SM-QUEUE-009: an operator confirms or dismisses a proposal; confirming applies insertion and preemption then, and
// is refused when operators are not trusted.
func TestIncidentProposalDecision(t *testing.T) {
	f := newFx(t)
	_, err := f.svc.SetPolicy(f.ctx, op, "spc_a", Policy{MaxActive: 1, IncidentInsertion: "top", IncidentPreempts: true})
	must(t, err)
	work := f.running("work", "", "")
	f.add(CreateInput{Title: "other"})
	p1 := f.add(CreateInput{Title: "p1", Kind: "incident", Triggers: []string{"event"}})
	p2 := f.add(CreateInput{Title: "p2", Kind: "incident", Triggers: []string{"event"}})
	if _, err := f.svc.DecideIncident(f.ctx, op, "spc_a", work.ID, work.Version, true); apperr.CodeOf(err) != apperr.CodePrecondition {
		t.Fatalf("decision on a non-proposal: %v", err)
	}
	if _, err := f.svc.DecideIncident(f.ctx, op, "spc_a", p1.ID, p1.Version+5, true); apperr.CodeOf(err) != apperr.CodePrecondition {
		t.Fatalf("stale version: %v", err)
	}
	c, err := f.svc.DecideIncident(f.ctx, op, "spc_a", p1.ID, p1.Version, true)
	must(t, err)
	if c.Kind != KindIncident || c.IncidentTrigger != TriggerOperator || c.IncidentProposedBy != "" || c.Position != 1 || f.get(work.ID).PreemptedBy != c.ID {
		t.Fatalf("confirmed %+v; work %+v", c, f.get(work.ID))
	}
	d, err := f.svc.DecideIncident(f.ctx, op, "spc_a", p2.ID, p2.Version, false)
	must(t, err)
	if d.Kind != KindOrdinary || d.IncidentProposedBy != "" {
		t.Fatalf("dismissed %+v", d)
	}
	_, err = f.svc.SetPolicy(f.ctx, op, "spc_a", Policy{MaxActive: 1, IncidentInsertion: "top", IncidentTriggers: []string{"control"}})
	must(t, err)
	p3 := f.add(CreateInput{Title: "p3", Kind: "incident", Triggers: []string{TriggerOperator}})
	if _, err := f.svc.DecideIncident(f.ctx, op, "spc_a", p3.ID, p3.Version, true); apperr.CodeOf(err) != apperr.CodeForbidden {
		t.Fatalf("confirmation by an untrusted operator: %v", err)
	}
	if f.auditCount("task.incident_decided") != 2 || f.auditCount("task.incident_refused") != 1 {
		t.Fatal("decisions not audited")
	}
}

// SM-QUEUE-009: what happens to a preempted task's workers is the policy's choice and is recorded and audited:
// finish tells them to wrap up, pause interrupts their turn, interrupt stops them. Subtask workers count too.
func TestPreemptedWorkers(t *testing.T) {
	for _, c := range []struct{ policy, want string }{
		{"finish", "notice:run_a notice:run_s"},
		{"pause", "pause:run_a pause:run_s"},
		{"interrupt", "stop:run_a stop:run_s"},
	} {
		t.Run(c.policy, func(t *testing.T) {
			f := newFx(t)
			wk := &fakeWorkers{live: map[string]bool{"run_a": true, "run_s": true}}
			f.svc.Workers = wk
			_, err := f.svc.SetPolicy(f.ctx, op, "spc_a", Policy{MaxActive: 1, IncidentInsertion: "top", IncidentPreempts: true, IncidentWorkers: c.policy})
			must(t, err)
			work := f.running("work", "run_a", "run_s")
			inc := f.add(CreateInput{Title: "inc", Kind: "incident", Triggers: []string{TriggerOperator}})
			if got := wk.take(); got != c.want {
				t.Fatalf("worker calls %q, want %q", got, c.want)
			}
			w := f.get(work.ID)
			if w.Status != Ready || w.PreemptedBy != inc.ID || len(w.PreemptionLog) != 3 {
				t.Fatalf("preempted task %+v", w)
			}
			actions := []string{}
			for _, p := range w.PreemptionLog {
				actions = append(actions, p.Action+":"+p.RunID)
			}
			slices.Sort(actions)
			if want := []string{c.policy + ":run_a", c.policy + ":run_s", "preempted:"}; !slices.Equal(actions, want) {
				t.Fatalf("records %v, want %v", actions, want)
			}
			if f.auditCount("task.preempted") != 1 {
				t.Fatal("preemption not audited")
			}
		})
	}
}

// SM-QUEUE-009: when the incident ends, automatic resumption runs eligible preempted tasks again and tells paused
// workers to continue; a task whose workers were stopped stays ready for control to relaunch. Manual resumption only
// tells control, and an explicit resume (or start) does the rest. Nothing polls: it all happens in the transition.
func TestIncidentResumption(t *testing.T) {
	t.Run("auto with paused workers", func(t *testing.T) {
		f := newFx(t)
		wk := &fakeWorkers{live: map[string]bool{"run_a": true}}
		f.svc.Workers = wk
		_, err := f.svc.SetPolicy(f.ctx, op, "spc_a", Policy{MaxActive: 1, IncidentInsertion: "top", IncidentPreempts: true, IncidentWorkers: "pause",
			IncidentResume: "auto"})
		must(t, err)
		work := f.running("work", "run_a", "")
		inc := f.add(CreateInput{Title: "inc", Kind: "incident", Triggers: []string{TriggerOperator}})
		inc, err = f.svc.SetStatus(f.ctx, op, "spc_a", inc.ID, inc.Version, StatusInput{Status: Running})
		must(t, err)
		wk.take()
		_, err = f.svc.SetStatus(f.ctx, op, "spc_a", inc.ID, inc.Version, StatusInput{Status: Done})
		must(t, err)
		w := f.get(work.ID)
		if w.Status != Running || w.PreemptedBy != "" || wk.take() != "continue:run_a" {
			t.Fatalf("auto resumption: %+v", w)
		}
		for _, p := range w.PreemptionLog {
			if p.ResumedAt == nil || p.Outcome != "resumed" {
				t.Fatalf("record not closed: %+v", p)
			}
		}
	})
	t.Run("auto with stopped workers", func(t *testing.T) {
		f := newFx(t)
		wk := &fakeWorkers{live: map[string]bool{"run_a": true}}
		f.svc.Workers = wk
		_, err := f.svc.SetPolicy(f.ctx, op, "spc_a", Policy{MaxActive: 1, IncidentInsertion: "top", IncidentPreempts: true, IncidentWorkers: "interrupt",
			IncidentResume: "auto"})
		must(t, err)
		work := f.running("work", "run_a", "")
		inc := f.add(CreateInput{Title: "inc", Kind: "incident", Triggers: []string{TriggerOperator}})
		_, err = f.svc.SetStatus(f.ctx, op, "spc_a", inc.ID, inc.Version, StatusInput{Status: Cancelled})
		must(t, err)
		w := f.get(work.ID)
		if w.Status != Ready || w.PreemptedBy != "" || !strings.HasPrefix(w.PreemptionLog[0].Outcome, "relaunch") {
			t.Fatalf("stopped workers: %+v", w)
		}
	})
	t.Run("manual", func(t *testing.T) {
		f := newFx(t)
		var notes []string
		f.svc.Notify = func(_, _, subject, body string) { notes = append(notes, subject+": "+body) }
		wk := &fakeWorkers{live: map[string]bool{"run_a": true}}
		f.svc.Workers = wk
		_, err := f.svc.SetPolicy(f.ctx, op, "spc_a", Policy{MaxActive: 1, IncidentInsertion: "top", IncidentPreempts: true, IncidentWorkers: "pause"})
		must(t, err)
		work := f.running("work", "run_a", "")
		inc := f.add(CreateInput{Title: "inc", Kind: "incident", Triggers: []string{TriggerOperator}})
		inc, err = f.svc.SetStatus(f.ctx, op, "spc_a", inc.ID, inc.Version, StatusInput{Status: Running})
		must(t, err)
		// while the incident runs, an explicit resume waits for capacity
		if _, outcome, err := f.svc.Resume(f.ctx, op, "spc_a", work.ID); err != nil || !strings.HasPrefix(outcome, "waiting") {
			t.Fatalf("resume during the incident: %q %v", outcome, err)
		}
		_, err = f.svc.SetStatus(f.ctx, op, "spc_a", inc.ID, inc.Version, StatusInput{Status: Done})
		must(t, err)
		if f.get(work.ID).Status != Ready || !strings.Contains(strings.Join(notes, "\n"), "wait to be resumed") {
			t.Fatalf("manual resumption changed the task or control was not told: %v", notes)
		}
		wk.take()
		r, outcome, err := f.svc.Resume(f.ctx, op, "spc_a", work.ID)
		if err != nil || outcome != "resumed" || r.Status != Running || wk.take() != "continue:run_a" {
			t.Fatalf("resume: %+v %q %v", r, outcome, err)
		}
		if _, _, err := f.svc.Resume(f.ctx, op, "spc_a", work.ID); apperr.CodeOf(err) != apperr.CodePrecondition {
			t.Fatalf("resume of a task that is not preempted: %v", err)
		}
	})
	t.Run("finish worker reports review", func(t *testing.T) {
		f := newFx(t)
		f.svc.Workers = &fakeWorkers{live: map[string]bool{"run_a": true}}
		_, err := f.svc.SetPolicy(f.ctx, op, "spc_a", Policy{MaxActive: 1, IncidentInsertion: "top", IncidentPreempts: true})
		must(t, err)
		work := f.running("work", "run_a", "")
		f.add(CreateInput{Title: "inc", Kind: "incident", Triggers: []string{TriggerOperator}})
		w := f.get(work.ID)
		r, err := f.svc.SetStatus(f.ctx, op, "spc_a", w.ID, w.Version, StatusInput{Status: Review, Result: "done before the incident"})
		if err != nil || r.Status != Review || r.PreemptedBy != "" {
			t.Fatalf("a finishing worker's report: %+v %v", r, err)
		}
		// an ordinary ready task still cannot jump to review
		o := f.add(CreateInput{Title: "o"})
		if _, err := f.svc.SetStatus(f.ctx, op, "spc_a", o.ID, o.Version, StatusInput{Status: Review}); apperr.CodeOf(err) != apperr.CodePrecondition {
			t.Fatalf("ready → review accepted: %v", err)
		}
	})
}

// SM-QUEUE-009: the incident capacity lets incidents run beside the ordinary limit without preempting anything.
func TestIncidentCapacity(t *testing.T) {
	f := newFx(t)
	_, err := f.svc.SetPolicy(f.ctx, op, "spc_a", Policy{MaxActive: 1, IncidentInsertion: "top", IncidentCapacity: 1})
	must(t, err)
	work := f.running("work", "", "")
	inc := f.add(CreateInput{Title: "inc", Kind: "incident", Triggers: []string{TriggerOperator}})
	if f.get(work.ID).Status != Running {
		t.Fatal("an incident without preemption stopped work")
	}
	if _, err := f.svc.SetStatus(f.ctx, op, "spc_a", inc.ID, inc.Version, StatusInput{Status: Running}); err != nil {
		t.Fatalf("incident capacity: %v", err)
	}
	o := f.add(CreateInput{Title: "o"})
	if _, err := f.svc.SetStatus(f.ctx, op, "spc_a", o.ID, o.Version, StatusInput{Status: Running}); apperr.CodeOf(err) != apperr.CodePrecondition {
		t.Fatalf("an ordinary task used the incident slot: %v", err)
	}
}
