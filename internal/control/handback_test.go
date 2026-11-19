package control

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/limits"
	"github.com/aurimasniekis/go-spaceman/internal/queue"
)

// hbEnv is a watchdog env with only the hand-back check on (10 minutes) and one worker run whose task is running.
type hbEnv struct {
	*wdEnv
	run       *fleet.Run
	task      *queue.Task
	wait      workerWait
	refuse    error
	reminders []string
	details   []map[string]any
	told      []string
	subjects  []string
	dedups    []string
}

func newHBEnv(t *testing.T) *hbEnv {
	e := &hbEnv{wdEnv: newWDEnv(t)}
	e.lim[limits.ControlWatchdog] = 0
	e.lim[limits.WorkerIdleWatchdog] = 10
	e.run = &fleet.Run{ID: "run_w", Kind: "worker", Role: "implementor", Desired: "running", Observed: "running", Status: "working", StartedAt: tp(wdNow)}
	e.task = &queue.Task{ID: "tsk_1", Title: "Do the thing", Status: queue.Running, AssigneeRun: "run_w", Version: 3}
	e.wire()
	return e
}

// wire points the watchdog's seams at the env (again after restart).
func (e *hbEnv) wire() {
	e.w.spaces = func(context.Context) ([]string, error) { return []string{"spc_a"}, nil }
	e.w.workers = func(context.Context, string) ([]*fleet.Run, map[string]*queue.Task, error) {
		r, t := *e.run, *e.task
		return []*fleet.Run{&r}, map[string]*queue.Task{t.AssigneeRun: &t}, nil
	}
	e.w.waits = func(context.Context, string, string) (workerWait, error) { return e.wait, nil }
	e.w.remind = func(_ context.Context, r *fleet.Run, text string, details map[string]any) error {
		if e.refuse != nil {
			return e.refuse
		}
		if r.ID != "run_w" {
			e.t.Fatalf("reminded %s", r.ID)
		}
		e.reminders = append(e.reminders, text)
		e.details = append(e.details, details)
		return nil
	}
	e.w.tell = func(_ context.Context, _, dedup, subject, body string) {
		e.dedups = append(e.dedups, dedup)
		e.subjects = append(e.subjects, subject)
		e.told = append(e.told, body)
	}
}

// restart is a restart of central: a new watchdog with nothing in memory.
func (e *hbEnv) restart() {
	e.w = NewWatchdog(e.w.sv)
	e.w.started = e.clk.Now()
	e.wire()
}

// endTurn finishes the worker's turn: its program goes idle and its turn counter advances.
func (e *hbEnv) endTurn() {
	e.run.Status = "idle"
	e.run.Turn++
}

func (e *hbEnv) want(reminders, told int) {
	e.t.Helper()
	if len(e.reminders) != reminders || len(e.told) != told {
		e.t.Fatalf("got %d reminders and %d notices to control (%v), want %d and %d", len(e.reminders), len(e.told), e.subjects, reminders, told)
	}
}

// An idle worker whose task is still running gets exactly one reminder N minutes after its turn ended, and when the
// reminder's own turn leaves the task running too, control gets exactly one notice N minutes after the reminder.
func TestHandbackOneReminderThenOneNotice(t *testing.T) {
	e := newHBEnv(t)
	e.pass(0)
	e.endTurn()
	e.pass(time.Minute) // the stretch begins
	e.pass(9 * time.Minute)
	e.want(0, 0)
	e.pass(time.Minute)
	e.want(1, 0)
	if !strings.Contains(e.reminders[0], "task tsk_1 is still running") || !strings.Contains(e.reminders[0], "spaceman_task_set_status") ||
		e.details[0]["task_id"] != "tsk_1" {
		t.Fatalf("reminder %q %v", e.reminders[0], e.details[0])
	}
	// The reminder's own turn: the worker works, then goes idle without handing the task back.
	e.run.Status = "working"
	e.pass(time.Minute)
	e.endTurn()
	e.pass(time.Minute)
	e.pass(7 * time.Minute)
	e.want(1, 0)
	e.pass(time.Minute)
	e.want(1, 1)
	if !strings.HasPrefix(e.dedups[0], "worker-idle:run_w:") || !strings.Contains(e.told[0], "run_w") || !strings.Contains(e.told[0], "tsk_1") ||
		!strings.Contains(e.subjects[0], "did not hand back") {
		t.Fatalf("notice %q %q %q", e.dedups[0], e.subjects[0], e.told[0])
	}
	for range 5 {
		e.pass(time.Hour)
	}
	e.want(1, 1)
}

// Setting the task to review, before or after the reminder, ends the stretch: no reminder, or no notice. Any other
// change of the task (control edits it) starts the clock over.
func TestHandbackReviewStops(t *testing.T) {
	e := newHBEnv(t)
	e.endTurn()
	e.pass(0)
	e.pass(5 * time.Minute)
	e.task.Version = 4
	e.pass(time.Minute)
	e.pass(9 * time.Minute)
	e.want(0, 0)
	e.pass(time.Minute)
	e.want(1, 0)

	e = newHBEnv(t)
	e.endTurn()
	e.pass(0)
	e.pass(5 * time.Minute)
	e.task.Status, e.task.Version = queue.Review, 4
	for range 4 {
		e.pass(10 * time.Minute)
	}
	e.want(0, 0)

	e = newHBEnv(t)
	e.endTurn()
	e.pass(0)
	e.pass(10 * time.Minute)
	e.want(1, 0)
	e.run.Status = "working"
	e.pass(time.Minute)
	e.endTurn()
	e.task.Status, e.task.Version = queue.Review, 4
	for range 4 {
		e.pass(10 * time.Minute)
	}
	e.want(1, 0)
}

// A message from the worker ends the stretch: a worker that answers the reminder with a message to control is never
// reminded again, and when the answer stays open control gets one notice that the worker waits for it (not that it
// ignored the reminder). A new turn before the reminder ends the stretch too.
func TestHandbackMessageStops(t *testing.T) {
	e := newHBEnv(t)
	e.endTurn()
	e.pass(0)
	e.pass(10 * time.Minute)
	e.want(1, 0)
	e.run.Status = "working"
	e.pass(time.Minute)
	e.endTurn()
	e.wait.MsgAt = e.clk.Now() // its reply turn messaged control
	e.pass(time.Minute)
	e.pass(9 * time.Minute)
	e.want(1, 0)
	e.pass(time.Minute)
	e.want(1, 1)
	if !strings.Contains(e.subjects[0], "waiting for you") {
		t.Fatalf("notice %q", e.subjects[0])
	}
	for range 4 {
		e.pass(time.Hour)
	}
	e.want(1, 1)

	// A new turn before the reminder is due (control's message woke it, say) starts the clock over.
	e = newHBEnv(t)
	e.endTurn()
	e.pass(0)
	e.pass(8 * time.Minute)
	e.endTurn() // a whole turn between two passes
	e.pass(time.Minute)
	e.pass(8 * time.Minute)
	e.want(0, 0)
	e.pass(time.Minute)
	e.want(0, 0)
	e.pass(time.Minute)
	e.want(1, 0)
}

// A worker waiting for control's answer is not reminded: its message to control is still open, or its turn ended
// with that message. Control is told once instead.
func TestHandbackWaitingForControl(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(e *hbEnv)
	}{
		{"open message", func(e *hbEnv) { e.wait.MsgAt, e.wait.MsgOpen = ago(time.Hour), true }},
		{"last act", func(e *hbEnv) { e.wait.MsgAt = e.clk.Now() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newHBEnv(t)
			e.pass(0) // seen working
			e.clk.Advance(30 * time.Second)
			tc.set(e)
			e.endTurn()
			e.pass(30 * time.Second)
			e.pass(10 * time.Minute)
			e.want(0, 1)
			if !strings.Contains(e.subjects[0], "waiting for you") {
				t.Fatalf("notice %q", e.subjects[0])
			}
			e.pass(time.Hour)
			e.want(0, 1)
		})
	}
	// A message from early in the turn (the "started" line) that control acknowledged is no sign of waiting.
	e := newHBEnv(t)
	e.wait.MsgAt = e.clk.Now()
	e.pass(time.Minute)
	e.pass(30 * time.Minute)
	e.endTurn()
	e.pass(time.Minute)
	e.pass(10 * time.Minute)
	e.want(1, 0)
}

// An open human action the worker asked, a usage limit, a takeover, a preempted task (back to ready) or a worker
// without a task means no reminder; once the human action is answered, the clock starts then.
func TestHandbackGuards(t *testing.T) {
	e := newHBEnv(t)
	e.endTurn()
	e.wait.HumanOpen = 1
	for range 6 {
		e.pass(10 * time.Minute)
	}
	e.want(0, 0)
	e.wait.HumanOpen = 0
	e.pass(time.Minute)
	e.pass(9 * time.Minute)
	e.want(0, 0)
	e.pass(time.Minute)
	e.want(1, 0)

	for name, set := range map[string]func(e *hbEnv){
		"limit":     func(e *hbEnv) { e.run.Limit = &fleet.RunLimit{} },
		"takeover":  func(e *hbEnv) { e.wait.TakenOver = true },
		"preempted": func(e *hbEnv) { e.task.Status = queue.Ready },
		"blocked":   func(e *hbEnv) { e.task.Status = queue.Blocked },
		"no task":   func(e *hbEnv) { e.task.AssigneeRun, e.task.ID = "", "" },
		"ended":     func(e *hbEnv) { e.run.Observed = "exited" },
		"control":   func(e *hbEnv) { e.run.Kind = "control" },
	} {
		t.Run(name, func(t *testing.T) {
			e := newHBEnv(t)
			e.endTurn()
			set(e)
			for range 6 {
				e.pass(10 * time.Minute)
			}
			e.want(0, 0)
		})
	}
}

// With the check off nothing happens; turning it on starts the clock then, so a worker idle for hours is not
// reminded at once.
func TestHandbackOff(t *testing.T) {
	e := newHBEnv(t)
	e.lim[limits.WorkerIdleWatchdog] = 0
	e.endTurn()
	for range 6 {
		e.pass(time.Hour)
	}
	e.want(0, 0)
	e.lim[limits.WorkerIdleWatchdog] = 10
	e.pass(time.Minute)
	e.pass(time.Minute)
	e.want(0, 0)
	e.pass(9 * time.Minute)
	e.want(1, 0)
	// Turned off after the reminder: the stretch is forgotten, so no notice to control.
	e.lim[limits.WorkerIdleWatchdog] = 0
	e.pass(time.Hour)
	e.lim[limits.WorkerIdleWatchdog] = 10
	e.pass(time.Minute)
	e.want(1, 0)
}

// After a restart of central nothing is known: a worker idle for hours is reminded N minutes after the first pass sees
// it, never at once, and a stretch whose reminder went out before the restart gets no immediate notice.
func TestHandbackRestart(t *testing.T) {
	e := newHBEnv(t)
	e.endTurn()
	e.clk.Advance(5 * time.Hour)
	e.restart()
	e.pass(0)
	e.pass(9 * time.Minute)
	e.want(0, 0)
	e.pass(time.Minute)
	e.want(1, 0)
	e.pass(9 * time.Minute)
	e.restart()
	e.pass(time.Minute)
	e.pass(time.Minute)
	e.want(1, 0)
	e.pass(9 * time.Minute)
	e.want(2, 0) // a new stretch: one reminder, N minutes after the restart
}

// A refused reminder (the worker turned busy or was taken over meanwhile) is tried again at the next pass.
func TestHandbackRefusedReminderRetries(t *testing.T) {
	e := newHBEnv(t)
	e.endTurn()
	e.pass(0)
	e.refuse = errors.New("taken over")
	e.pass(10 * time.Minute)
	e.want(0, 0)
	e.refuse = nil
	e.pass(time.Minute)
	e.want(1, 0)
}
