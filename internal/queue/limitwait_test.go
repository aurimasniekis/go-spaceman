package queue

import (
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/store"
)

// wait seeds a parked worker run and its waiting limit wait on task.
func (f *fx) wait(id, run, task string) {
	f.t.Helper()
	must(f.t, f.s.W().TxNamed(f.ctx, "seed", func(tx *store.Tx) error {
		if _, err := tx.SQL.ExecContext(f.ctx, `INSERT INTO runs (id, space_id, node_id, generation, kind, role, harness, instance_id, desired, observed,
			launch_json, created_at, created_by, updated_at) VALUES (?, 'spc_a', 'nod_1', 1, 'worker', 'w', 'claude-code', ?, 'stopped', 'exited', '{}', 0, 't', 0)`,
			run, "ins_"+run); err != nil {
			return err
		}
		_, err := tx.SQL.ExecContext(f.ctx, `INSERT INTO limit_waits (id, space_id, run_id, task_id, run_kind, role, node_id, instance_id, harness, launch_json,
			resume_at, created_at, updated_at) VALUES (?, 'spc_a', ?, ?, 'worker', 'w', 'nod_1', ?, 'claude-code', '{}', 1, 0, 0)`, id, run, task, "ins_"+run)
		return err
	}))
}

func (f *fx) waitState(id string) (state, reason string) {
	f.t.Helper()
	must(f.t, f.s.ReadDB().QueryRowContext(f.ctx, `SELECT state, last_error FROM limit_waits WHERE id = ?`, id).Scan(&state, &reason))
	return state, reason
}

// assigned creates a running task assigned to run.
func (f *fx) assigned(title, run string) *Task {
	f.t.Helper()
	t := f.add(CreateInput{Title: title})
	t, err := f.svc.SetStatus(f.ctx, op, "spc_a", t.ID, t.Version, StatusInput{Status: Running})
	must(f.t, err)
	t, err = f.svc.Update(f.ctx, op, "spc_a", t.ID, t.Version, UpdateInput{Title: t.Title, Priority: t.Priority, AssigneeRun: run})
	must(f.t, err)
	return t
}

// The UL-2 review follow-up (ADR-0079): a run parked on a task stops waiting for it when control finishes or cancels the
// task, sends it back to the queue or gives it to another run; blocking it or moving it to review keeps the wait.
func TestTaskChangesSettleLimitWaits(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	f.svc.DefaultMaxActive = 10
	a := f.assigned("A", "run_a")
	f.wait("lwt_a", "run_a", a.ID)
	a, err := f.svc.SetStatus(f.ctx, op, "spc_a", a.ID, a.Version, StatusInput{Status: Blocked})
	must(t, err)
	if st, _ := f.waitState("lwt_a"); st != "waiting" {
		t.Fatalf("blocked task: wait %s", st)
	}
	if _, err := f.svc.SetStatus(f.ctx, op, "spc_a", a.ID, a.Version, StatusInput{Status: Cancelled}); err != nil {
		t.Fatal(err)
	}
	if st, why := f.waitState("lwt_a"); st != "cancelled" || why != WaitTaskMoved {
		t.Fatalf("cancelled task: wait %s %s", st, why)
	}

	b := f.assigned("B", "run_b")
	f.wait("lwt_b", "run_b", b.ID)
	if _, err := f.svc.Update(f.ctx, op, "spc_a", b.ID, b.Version, UpdateInput{Title: b.Title, Priority: b.Priority, AssigneeRun: "run_other"}); err != nil {
		t.Fatal(err)
	}
	if st, _ := f.waitState("lwt_b"); st != "cancelled" {
		t.Fatalf("reassigned task: wait %s", st)
	}

	c := f.assigned("C", "run_c")
	f.wait("lwt_c", "run_c", c.ID)
	if _, err := f.svc.SetStatus(f.ctx, op, "spc_a", c.ID, c.Version, StatusInput{Status: Ready}); err != nil {
		t.Fatal(err)
	}
	if st, _ := f.waitState("lwt_c"); st != "cancelled" {
		t.Fatalf("task back in the queue: wait %s", st)
	}
	var audits int
	must(t, f.s.ReadDB().QueryRowContext(f.ctx, `SELECT count(*) FROM audit_events WHERE action = 'limit_wait.cancelled'`).Scan(&audits))
	if audits != 3 {
		t.Fatalf("%d cancel audits", audits)
	}
}

// Decisions 5 and 6: a resume re-points the parked run's task to the new run; a release gives it back to the queue,
// ready and unassigned. Neither touches a task that is no longer the parked run's.
func TestRepointAndReleaseRunTask(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	f.svc.DefaultMaxActive = 10
	a := f.assigned("A", "run_old")
	tx := func(fn func(tx *store.Tx) error) { must(t, f.s.W().TxNamed(f.ctx, "t", fn)) }
	var ok bool
	tx(func(tx *store.Tx) (err error) {
		ok, err = f.svc.RepointRunTaskTx(f.ctx, tx, op, a.ID, "run_x", "run_new")
		return
	})
	if ok || f.get(a.ID).AssigneeRun != "run_old" {
		t.Fatal("re-pointed a task another run holds")
	}
	tx(func(tx *store.Tx) (err error) {
		ok, err = f.svc.RepointRunTaskTx(f.ctx, tx, op, a.ID, "run_old", "run_new")
		return
	})
	if got := f.get(a.ID); !ok || got.AssigneeRun != "run_new" || got.Status != Running {
		t.Fatalf("re-point: %v %+v", ok, got)
	}
	var st string
	release := func(task, run string) string {
		t.Helper()
		tx(func(tx *store.Tx) (err error) {
			st, err = f.svc.ReleaseRunTaskTx(f.ctx, tx, op, task, run, "too long")
			return
		})
		return st
	}
	if st := release(a.ID, "run_old"); st != "" || f.get(a.ID).Status != Running {
		t.Fatal("released a task another run holds")
	}
	if st := release(a.ID, "run_new"); st != Ready {
		t.Fatalf("release: %q", st)
	}
	if got := f.get(a.ID); got.Status != Ready || got.AssigneeRun != "" || got.StatusReason != "too long" {
		t.Fatalf("release: %+v", got)
	}
	// A finished task is left alone.
	b := f.assigned("B", "run_b")
	b, err := f.svc.SetStatus(f.ctx, op, "spc_a", b.ID, b.Version, StatusInput{Status: Done})
	must(t, err)
	if st := release(b.ID, "run_b"); st != "" || f.get(b.ID).Status != Done {
		t.Fatal("released a finished task")
	}
	// A task in review stays in review (its work waits for acceptance); only its assignee is cleared (review S3).
	c := f.assigned("C", "run_c")
	_, err = f.svc.SetStatus(f.ctx, op, "spc_a", c.ID, c.Version, StatusInput{Status: Review, Result: "done, see the PR"})
	must(t, err)
	if st := release(c.ID, "run_c"); st != Review {
		t.Fatalf("release of a review task: %q", st)
	}
	if got := f.get(c.ID); got.Status != Review || got.AssigneeRun != "" || got.Result != "done, see the PR" {
		t.Fatalf("review task after release: %+v", got)
	}
}
