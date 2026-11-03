package queue

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/outbox"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// The incident policy (SM-QUEUE-009, ADR-0050): which sources may raise incidents (trusted triggers), what happens to
// a preempted task's workers, extra capacity for incidents and how preempted work resumes when the incident ends.
// Everything happens inside the transaction that caused it (an incident created or confirmed, an incident's state
// change, an explicit resume): nothing polls, and worker commands are queued and delivered after the commit.

// Workers controls the worker runs of preempted tasks. The fleet implements it; every method runs inside the
// queue's write transaction, queues commands only and reports whether it acted.
type Workers interface {
	WorkerActive(ctx context.Context, tx *store.Tx, spaceID, runID string) (bool, error)
	PauseWorkerTx(ctx context.Context, tx *store.Tx, actor audit.Actor, spaceID, runID, key, reason string) (bool, error)
	StopWorkerTx(ctx context.Context, tx *store.Tx, actor audit.Actor, spaceID, runID, reason string) (bool, error)
	NoticeWorkerTx(ctx context.Context, tx *store.Tx, spaceID, runID, text string) (bool, error)
	KickRun(ctx context.Context, runID string)
}

// Preemption is one record of a preemption: the task's own row (run_id empty, action preempted) and one row per worker
// run with what the policy did to it, closed with an outcome when the task resumes.
type Preemption struct {
	ID         string     `json:"id"`
	IncidentID string     `json:"incident_id"`
	TaskID     string     `json:"task_id"`
	RunID      string     `json:"run_id,omitempty"`
	Action     string     `json:"action" enum:"preempted,finish,pause,interrupt"`
	CreatedAt  time.Time  `json:"created_at"`
	ResumedAt  *time.Time `json:"resumed_at,omitempty"`
	ResumedBy  string     `json:"resumed_by,omitempty"`
	Outcome    string     `json:"outcome,omitempty"`
}

func preemptionFromRow(r db.TaskPreemption) Preemption {
	return Preemption{ID: r.ID, IncidentID: r.IncidentID, TaskID: r.TaskID, RunID: r.RunID, Action: r.Action, CreatedAt: time.UnixMicro(r.CreatedAt).UTC(),
		ResumedAt: t2(r.ResumedAt), ResumedBy: r.ResumedBy, Outcome: r.Outcome}
}

var (
	reTemplateTrigger = regexp.MustCompile(`^template:[a-z0-9][a-z0-9_.-]{0,79}$`)
	triggerWords      = []string{TriggerOperator, TriggerControl}
)

// validTrigger reports whether a policy may list s as a trusted trigger. Workers, schedules, other spaces, dreamers
// and the other inbox sources can never be trusted to raise incidents.
func validTrigger(s string) bool {
	switch {
	case slices.Contains(triggerWords, s):
		return true
	case strings.HasPrefix(s, "subscription:"):
		return ids.Valid(ids.Subscription, strings.TrimPrefix(s, "subscription:"))
	default:
		return reTemplateTrigger.MatchString(s)
	}
}

// trusts returns the first of the request's triggers the policy trusts, or (the first trigger, false).
func (p Policy) trusts(triggers []string) (string, bool) {
	for _, t := range triggers {
		if t != "" && slices.Contains(p.IncidentTriggers, t) {
			return t, true
		}
	}
	if len(triggers) == 0 || triggers[0] == "" {
		return "unknown", false
	}
	return triggers[0], false
}

// InboxTriggers names the sources behind an inbox item, for an incident created from it by control: an event item's
// subscription and template, an operator's item the operator, anything else its source kind (never trusted).
func InboxTriggers(sourceKind, sourceID, subscriptionID string, trusted map[string]any) []string {
	switch sourceKind {
	case "event":
		out := []string{}
		if subscriptionID != "" {
			out = append(out, "subscription:"+subscriptionID)
		}
		for _, k := range []string{"template_id", "template_slug"} {
			if v, ok := trusted[k].(string); ok && v != "" {
				out = append(out, "template:"+v)
			}
		}
		if len(out) == 0 {
			out = append(out, "event")
		}
		return out
	case "operator":
		return []string{TriggerOperator}
	default:
		return []string{sourceKind}
	}
}

// topLimit is the number of running top-level tasks t may start beside: incidents also get the incident capacity.
func (sn *snapshot) topLimit(t *Task) int {
	if t.Kind == KindIncident {
		return sn.policy.MaxActive + sn.policy.IncidentCapacity
	}
	return sn.policy.MaxActive
}

// effects collects what to do after a commit: kick nodes of runs that got commands and tell control.
type effects struct {
	kicks   []string
	notices []notice
}

type notice struct{ spaceID, dedup, subject, body string }

func (fx *effects) note(spaceID, dedup, subject, body string) {
	fx.notices = append(fx.notices, notice{spaceID, dedup, subject, body})
}

func (svc *Service) after(ctx context.Context, _ audit.Actor, fx *effects) {
	if fx == nil {
		return
	}
	if svc.Workers != nil {
		for _, r := range fx.kicks {
			svc.Workers.KickRun(ctx, r)
		}
	}
	if svc.Notify != nil {
		for _, n := range fx.notices {
			svc.Notify(n.spaceID, n.dedup, n.subject, n.body)
		}
	}
}

// auditRefused records a refused incident request in its own transaction (the refused one rolled back).
func (svc *Service) auditRefused(ctx context.Context, actor audit.Actor, spaceID, taskID, trigger string) {
	_ = svc.s.W().TxNamed(ctx, "queue.incident_refused", func(tx *store.Tx) error {
		_, err := svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "task.incident_refused", Outcome: audit.OutcomeDenied,
			Target: audit.Target{Type: "space", ID: spaceID}, Details: map[string]any{"trigger": trigger, "task_id": taskID}})
		return err
	})
}

// workerRuns returns the assigned runs of t and of its subtasks (each once).
func (sn *snapshot) workerRuns(t *Task) []string {
	var out []string
	var walk func(id string)
	walk = func(id string) {
		task := sn.byID[id]
		if task == nil {
			return
		}
		if task.AssigneeRun != "" && !slices.Contains(out, task.AssigneeRun) && !terminal(task.Status) {
			out = append(out, task.AssigneeRun)
		}
		for _, c := range sn.order {
			if c.ParentID == id {
				walk(c.ID)
			}
		}
	}
	walk(t.ID)
	return out
}

// preemptTx returns the running ordinary top-level tasks to ready for the incident and handles their workers as the
// policy says. It returns the preempted task IDs.
func (svc *Service) preemptTx(ctx context.Context, tx *store.Tx, actor audit.Actor, sn *snapshot, incidentID, incidentTitle string, fx *effects) ([]string, error) {
	var preempted, summary []string
	spaceID := ""
	for _, t := range sn.order {
		if t.ParentID != "" || t.Kind != KindOrdinary || t.Status != Running {
			continue
		}
		reason := "preempted by incident " + incidentTitle
		if _, err := tx.Q.SetTaskStatus(ctx, db.SetTaskStatusParams{Status: Ready, StatusReason: reason, Result: t.Result,
			Preempted: 1, Now: tx.NowMicros(), MarkFinished: int64(0), ID: t.ID, Version: t.Version}); err != nil {
			return nil, err
		}
		if err := tx.Q.SetTaskPreemptedBy(ctx, db.SetTaskPreemptedByParams{PreemptedBy: incidentID, ID: t.ID}); err != nil {
			return nil, err
		}
		if err := tx.Q.InsertTaskPreemption(ctx, db.InsertTaskPreemptionParams{ID: ids.New(ids.Preemption), SpaceID: t.SpaceID, IncidentID: incidentID,
			TaskID: t.ID, Action: "preempted", CreatedAt: tx.NowMicros()}); err != nil {
			return nil, err
		}
		runs := []map[string]any{}
		var handled []string
		for _, run := range sn.workerRuns(t) {
			action, err := svc.handleWorker(ctx, tx, actor, sn.policy.IncidentWorkers, t, run, incidentID, incidentTitle)
			if err != nil {
				return nil, err
			}
			if action == "" {
				continue // not a live worker any more
			}
			if err := tx.Q.InsertTaskPreemption(ctx, db.InsertTaskPreemptionParams{ID: ids.New(ids.Preemption), SpaceID: t.SpaceID, IncidentID: incidentID,
				TaskID: t.ID, RunID: run, Action: action, CreatedAt: tx.NowMicros()}); err != nil {
				return nil, err
			}
			fx.kicks = append(fx.kicks, run)
			runs = append(runs, map[string]any{"run_id": run, "action": action})
			handled = append(handled, run+" ("+action+")")
		}
		t.Status, t.PreemptedBy, t.Version, spaceID = Ready, incidentID, t.Version+1, t.SpaceID
		preempted = append(preempted, t.ID)
		if err := svc.changes.Record(ctx, tx, t.SpaceID, "task", t.ID, "updated", t.Version); err != nil {
			return nil, err
		}
		if _, err := svc.Outbox.Enqueue(ctx, tx, outbox.Event{Payload: outbox.TaskStatusChanged{Task: eventTask(t), From: Running, To: Ready,
			Preempted: true}, Correlation: t.ID, Causation: incidentID}); err != nil {
			return nil, err
		}
		if _, err := svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "task.preempted", Target: audit.Target{Type: "task", ID: t.ID},
			Details: map[string]any{"space_id": t.SpaceID, "incident_id": incidentID, "workers": sn.policy.IncidentWorkers, "runs": runs}}); err != nil {
			return nil, err
		}
		line := fmt.Sprintf("- %s (%s)", t.ID, t.Title)
		if len(handled) > 0 {
			line += ": workers " + strings.Join(handled, ", ")
		}
		summary = append(summary, line)
	}
	if len(preempted) > 0 {
		fx.note(spaceID, "incident-preempted:"+incidentID, "Incident preempted work: "+incidentTitle,
			fmt.Sprintf("Incident %s returned these running tasks to ready (preemption, not failure); worker handling: %s. Resumption: %s.\n%s",
				incidentID, sn.policy.IncidentWorkers, sn.policy.IncidentResume, strings.Join(summary, "\n")))
	}
	return preempted, nil
}

// handleWorker applies the worker policy to one run and returns what was done ("" when the run is not live).
func (svc *Service) handleWorker(ctx context.Context, tx *store.Tx, actor audit.Actor, policy string, t *Task, run, incidentID, incidentTitle string) (string, error) {
	if svc.Workers == nil {
		return "", nil
	}
	live, err := svc.Workers.WorkerActive(ctx, tx, t.SpaceID, run)
	if err != nil || !live {
		return "", err
	}
	reason := "incident " + incidentID + " preempted task " + t.ID
	switch policy {
	case WorkersPause:
		ok, err := svc.Workers.PauseWorkerTx(ctx, tx, actor, t.SpaceID, run, incidentID, reason)
		if err != nil {
			return "", err
		}
		if ok {
			return WorkersPause, nil
		}
	case WorkersInterrupt:
		ok, err := svc.Workers.StopWorkerTx(ctx, tx, actor, t.SpaceID, run, reason)
		if err != nil {
			return "", err
		}
		if ok {
			return WorkersInterrupt, nil
		}
	}
	// finish (or a pause the run could not take, e.g. an operator holds its terminal): the worker keeps running and is
	// told, when idle, to wrap up.
	if _, err := svc.Workers.NoticeWorkerTx(ctx, tx, t.SpaceID, run, fmt.Sprintf("SpaceMan: incident %q preempted task %s. Finish the step you are on, "+
		"report with spaceman_task_set_status (review or blocked), and start nothing new on it until control resumes it.", incidentTitle, t.ID)); err != nil {
		return "", err
	}
	return WorkersFinish, nil
}

// resumeTx resumes a preempted task: when it is ready, eligible and still has live workers it runs again and paused
// workers are told to continue; when its workers are gone it stays ready for control to launch a new worker. When
// running is already set (an explicit start), only the workers are resumed. It closes the task's open preemption rows
// and returns the outcome.
func (svc *Service) resumeTx(ctx context.Context, tx *store.Tx, actor audit.Actor, sn *snapshot, t *Task, fx *effects) (string, error) {
	rows, err := tx.Q.ListOpenTaskPreemptions(ctx, t.ID)
	if err != nil {
		return "", err
	}
	if len(rows) == 0 && t.PreemptedBy == "" {
		return "", apperr.New(apperr.CodePrecondition, "task %s is not preempted", t.ID)
	}
	live := []string{}
	for _, r := range rows {
		if r.RunID == "" || svc.Workers == nil {
			continue
		}
		ok, err := svc.Workers.WorkerActive(ctx, tx, t.SpaceID, r.RunID)
		if err != nil {
			return "", err
		}
		if ok && !slices.Contains(live, r.RunID) {
			live = append(live, r.RunID)
		}
	}
	incident := t.PreemptedBy
	if incident == "" && len(rows) > 0 {
		incident = rows[0].IncidentID
	}
	outcome := ""
	switch {
	case t.Status == Running:
		outcome = "resumed"
	case t.Status != Ready:
		outcome = "superseded: the task is " + t.Status
	case len(live) == 0:
		outcome = "relaunch: no live worker; control launches a new one"
	default:
		if why := sn.why(t); len(why) > 0 {
			return "waiting: " + why[0], nil // stays preempted; a later resume or the next incident's end retries
		}
		if _, err := tx.Q.SetTaskStatus(ctx, db.SetTaskStatusParams{Status: Running, StatusReason: "resumed after incident " + incident, Result: t.Result,
			Now: tx.NowMicros(), MarkFinished: 0, ID: t.ID, Version: t.Version}); err != nil {
			return "", err
		}
		if _, err := svc.Outbox.Enqueue(ctx, tx, outbox.Event{Payload: outbox.TaskStatusChanged{Task: eventTask(t), From: Ready, To: Running},
			Correlation: t.ID, Causation: incident}); err != nil {
			return "", err
		}
		t.Status, t.Version = Running, t.Version+1
		outcome = "resumed"
	}
	if t.Status == Running {
		for _, r := range rows {
			if r.Action != WorkersPause || !slices.Contains(live, r.RunID) {
				continue
			}
			if _, err := svc.Workers.NoticeWorkerTx(ctx, tx, t.SpaceID, r.RunID, fmt.Sprintf("SpaceMan: the incident is over; task %s resumes. "+
				"Continue where you stopped.", t.ID)); err != nil {
				return "", err
			}
			fx.kicks = append(fx.kicks, r.RunID)
		}
	}
	for _, r := range rows {
		if err := tx.Q.CloseTaskPreemption(ctx, db.CloseTaskPreemptionParams{Now: sql.NullInt64{Int64: tx.NowMicros(), Valid: true}, By: actor.ID,
			Outcome: outcome, ID: r.ID}); err != nil {
			return "", err
		}
	}
	if err := tx.Q.SetTaskPreemptedBy(ctx, db.SetTaskPreemptedByParams{PreemptedBy: "", ID: t.ID}); err != nil {
		return "", err
	}
	t.PreemptedBy = ""
	if err := svc.changes.Record(ctx, tx, t.SpaceID, "task", t.ID, "updated", t.Version); err != nil {
		return "", err
	}
	_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "task.resumed", Target: audit.Target{Type: "task", ID: t.ID},
		Details: map[string]any{"space_id": t.SpaceID, "incident_id": incident, "outcome": outcome, "live_workers": live}})
	return outcome, err
}

// incidentEndedTx runs when an incident task reaches a final state: with automatic resumption every preempted task of
// the space is resumed in queue order where it can be; otherwise control is told which tasks wait.
func (svc *Service) incidentEndedTx(ctx context.Context, tx *store.Tx, actor audit.Actor, sn *snapshot, inc *Task, fx *effects) error {
	var waiting []*Task
	for _, t := range sn.order {
		if t.PreemptedBy != "" && t.Status == Ready {
			waiting = append(waiting, t)
		}
	}
	if len(waiting) == 0 {
		return nil
	}
	var lines []string
	if sn.policy.IncidentResume == ResumeAuto {
		for _, t := range waiting {
			outcome, err := svc.resumeTx(ctx, tx, actor, sn, t, fx)
			if err != nil {
				return err
			}
			lines = append(lines, fmt.Sprintf("- %s (%s): %s", t.ID, t.Title, outcome))
		}
		fx.note(inc.SpaceID, "incident-ended:"+inc.ID, "Incident ended: "+inc.Title,
			fmt.Sprintf("Incident %s is %s. Preempted tasks were resumed automatically where possible:\n%s", inc.ID, inc.Status, strings.Join(lines, "\n")))
		return nil
	}
	for _, t := range waiting {
		lines = append(lines, fmt.Sprintf("- %s (%s), preempted by %s", t.ID, t.Title, t.PreemptedBy))
	}
	fx.note(inc.SpaceID, "incident-ended:"+inc.ID, "Incident ended: "+inc.Title,
		fmt.Sprintf("Incident %s is %s. These preempted tasks wait to be resumed (spaceman_task_resume, or Resume in the queue):\n%s", inc.ID, inc.Status,
			strings.Join(lines, "\n")))
	return nil
}

// Resume resumes a preempted task explicitly (an operator or control). It returns the task and the outcome.
func (svc *Service) Resume(ctx context.Context, actor audit.Actor, spaceID, id string) (*Task, string, error) {
	outcome := ""
	var fx *effects
	t, err := svc.mutate(ctx, "queue.resume", spaceID, id, 0, func(tx *store.Tx, sn *snapshot, t *Task) error {
		fx = &effects{}
		var err error
		outcome, err = svc.resumeTx(ctx, tx, actor, sn, t, fx)
		return err
	})
	if err != nil {
		return nil, "", err
	}
	svc.after(ctx, actor, fx)
	if svc.OnStatus != nil && t.Status == Running {
		svc.OnStatus(actor, t, Ready)
	}
	return t, outcome, nil
}

// DecideIncident confirms or dismisses an incident proposal (operators only: the API is the only caller). Confirming
// makes the task an incident raised by the operator, with the policy's insertion and preemption applied now.
func (svc *Service) DecideIncident(ctx context.Context, actor audit.Actor, spaceID, id string, version int64, confirm bool) (*Task, error) {
	var fx *effects
	refused := ""
	t, err := svc.mutate(ctx, "queue.incident_decision", spaceID, id, version, func(tx *store.Tx, sn *snapshot, t *Task) error {
		fx, refused = &effects{}, ""
		if t.IncidentProposedBy == "" {
			return apperr.New(apperr.CodePrecondition, "task %s is not an incident proposal", t.ID)
		}
		details := map[string]any{"space_id": spaceID, "proposed_by": t.IncidentProposedBy, "confirmed": confirm}
		if !confirm {
			if err := tx.Q.SetTaskIncident(ctx, db.SetTaskIncidentParams{Kind: KindOrdinary, Bump: 1, Now: tx.NowMicros(), ID: t.ID}); err != nil {
				return err
			}
		} else {
			if _, ok := sn.policy.trusts([]string{TriggerOperator}); !ok {
				refused = TriggerOperator
				return apperr.New(apperr.CodeForbidden, "the queue policy does not trust operators to raise incidents")
			}
			if terminal(t.Status) {
				return apperr.New(apperr.CodePrecondition, "task %s is %s", t.ID, t.Status)
			}
			if err := tx.Q.SetTaskIncident(ctx, db.SetTaskIncidentParams{Kind: KindIncident, IncidentTrigger: TriggerOperator, Bump: 1, Now: tx.NowMicros(),
				ID: t.ID}); err != nil {
				return err
			}
			if sn.policy.IncidentInsertion == "top" && !t.Manual {
				var others []db.Task
				for _, s := range sn.siblings(t.ParentID) {
					if s.ID != t.ID {
						others = append(others, s)
					}
				}
				rank, err := rankAt(ctx, tx, others, 0)
				if err != nil {
					return err
				}
				if err := tx.Q.SetTaskRank(ctx, db.SetTaskRankParams{Rank: rank, ManualPosition: 0, Bump: 0, Now: tx.NowMicros(), ID: t.ID}); err != nil {
					return err
				}
			}
			t.Kind = KindIncident
			if t.ParentID == "" && sn.policy.IncidentPreempts {
				preempted, err := svc.preemptTx(ctx, tx, actor, sn, t.ID, t.Title, fx)
				if err != nil {
					return err
				}
				details["preempted"] = preempted
			}
		}
		if err := svc.changes.Record(ctx, tx, spaceID, "task", t.ID, "updated", t.Version+1); err != nil {
			return err
		}
		_, err := svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "task.incident_decided", Target: audit.Target{Type: "task", ID: t.ID},
			Details: details})
		return err
	})
	if err != nil {
		if refused != "" {
			svc.auditRefused(ctx, actor, spaceID, id, refused)
		}
		return nil, err
	}
	svc.after(ctx, actor, fx)
	return t, nil
}

// Preemptions returns a task's recent preemption records, newest first.
func (svc *Service) Preemptions(ctx context.Context, taskID string) ([]Preemption, error) {
	rows, err := svc.s.R().ListTaskPreemptions(ctx, taskID)
	if err != nil {
		return nil, err
	}
	out := make([]Preemption, 0, len(rows))
	for _, r := range rows {
		out = append(out, preemptionFromRow(r))
	}
	return out, nil
}

// normalizePolicy fills defaults for omitted incident fields and validates the policy.
func normalizePolicy(p *Policy) error {
	v := &apperr.Validation{}
	if p.MaxActive < 1 || p.MaxActive > 20 {
		v.Add("max_active", "must be 1-20")
	}
	if p.IncidentInsertion != "top" && p.IncidentInsertion != "priority" {
		v.Add("incident_insertion", "must be top or priority")
	}
	if p.IncidentTriggers == nil {
		p.IncidentTriggers = DefaultTriggers()
	}
	clean := []string{}
	for _, t := range p.IncidentTriggers {
		t = strings.TrimSpace(t)
		if !validTrigger(t) {
			v.Add("incident_triggers", "%q is not a trigger (operator, control, subscription:<id> or template:<id or slug>)", t)
			continue
		}
		if !slices.Contains(clean, t) {
			clean = append(clean, t)
		}
	}
	if len(clean) > 50 {
		v.Add("incident_triggers", "at most 50 triggers")
	}
	slices.Sort(clean)
	p.IncidentTriggers = clean
	if p.IncidentUntrusted == "" {
		p.IncidentUntrusted = defaultUntrusted
	}
	if p.IncidentUntrusted != UntrustedPropose && p.IncidentUntrusted != UntrustedRefuse {
		v.Add("incident_untrusted", "must be propose or refuse")
	}
	if p.IncidentWorkers == "" {
		p.IncidentWorkers = defaultWorkers
	}
	if !slices.Contains([]string{WorkersFinish, WorkersPause, WorkersInterrupt}, p.IncidentWorkers) {
		v.Add("incident_workers", "must be finish, pause or interrupt")
	}
	if p.IncidentCapacity < 0 || p.IncidentCapacity > 20 {
		v.Add("incident_capacity", "must be 0-20")
	}
	if p.IncidentResume == "" {
		p.IncidentResume = defaultResumption
	}
	if p.IncidentResume != ResumeManual && p.IncidentResume != ResumeAuto {
		v.Add("incident_resume", "must be manual or auto")
	}
	return v.Err()
}
