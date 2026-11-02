package queue

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// mutate runs fn on a fresh snapshot inside a write transaction, checking the task's version when version > 0.
func (svc *Service) mutate(ctx context.Context, name, spaceID, id string, version int64, fn func(tx *store.Tx, sn *snapshot, t *Task) error) (*Task, error) {
	err := svc.s.W().TxNamed(ctx, name, func(tx *store.Tx) error {
		sn, err := load(ctx, tx.Q, spaceID)
		if err != nil {
			return err
		}
		t := sn.byID[id]
		if t == nil {
			return errNotFound
		}
		if version > 0 && t.Version != version {
			return apperr.New(apperr.CodePrecondition, "the task changed since version %d (now %d)", version, t.Version)
		}
		return fn(tx, sn, t)
	})
	if err != nil {
		return nil, err
	}
	return svc.Get(ctx, spaceID, id)
}

// MoveInput places a task: before or after a sibling, or at the top or bottom of its siblings.
type MoveInput struct {
	BeforeID string
	AfterID  string
	To       string // "top" or "bottom"
}

// Move places a task by an operator. The placement is authoritative: it marks the task manual, so policy and duplicate
// triggers never move it again, and reordering never stops work (SM-QUEUE-001/002/009).
func (svc *Service) Move(ctx context.Context, actor audit.Actor, spaceID, id string, in MoveInput) (*Task, error) {
	return svc.mutate(ctx, "queue.move", spaceID, id, 0, func(tx *store.Tx, sn *snapshot, t *Task) error {
		var others []db.Task
		for _, s := range sn.siblings(t.ParentID) {
			if s.ID != t.ID {
				others = append(others, s)
			}
		}
		idx := -1
		switch {
		case in.To == "top":
			idx = 0
		case in.To == "bottom":
			idx = len(others)
		case in.BeforeID != "" || in.AfterID != "":
			ref := in.BeforeID
			if ref == "" {
				ref = in.AfterID
			}
			for i, s := range others {
				if s.ID == ref {
					idx = i
					if in.AfterID != "" {
						idx = i + 1
					}
				}
			}
			if idx < 0 {
				return apperr.New(apperr.CodeInvalid, "%q is not a sibling of this task", ref)
			}
		default:
			return apperr.New(apperr.CodeInvalid, "give before_id, after_id or to (top or bottom)")
		}
		rank, err := rankAt(ctx, tx, others, idx)
		if err != nil {
			return err
		}
		if err := tx.Q.SetTaskRank(ctx, db.SetTaskRankParams{Rank: rank, ManualPosition: 1, Bump: 1, Now: tx.NowMicros(), ID: t.ID}); err != nil {
			return err
		}
		if err := svc.changes.Record(ctx, tx, spaceID, "task", t.ID, "updated", t.Version+1); err != nil {
			return err
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "task.moved", Target: audit.Target{Type: "task", ID: t.ID},
			Details: map[string]any{"space_id": spaceID, "from": t.Position, "to": idx + 1, "status": t.Status}})
		return err
	})
}

// Escalate raises a task to urgent. It moves the task to the top only when the space's policy allows escalation to
// reorder (SM-QUEUE-002: escalation changes placement only through explicit policy).
func (svc *Service) Escalate(ctx context.Context, actor audit.Actor, spaceID, id, reason string) (*Task, error) {
	return svc.mutate(ctx, "queue.escalate", spaceID, id, 0, func(tx *store.Tx, sn *snapshot, t *Task) error {
		ex, _ := json.Marshal(t.Exclusions)
		if _, err := tx.Q.UpdateTaskFields(ctx, db.UpdateTaskFieldsParams{Title: t.Title, Objective: t.Objective, Acceptance: t.Acceptance, Priority: "urgent",
			ExclusionsJson: string(ex), AssigneeRole: t.AssigneeRole, AssigneeRun: t.AssigneeRun, Workspace: t.Workspace, Now: tx.NowMicros(), ID: t.ID, Version: t.Version}); err != nil {
			return err
		}
		moved := false
		if sn.policy.EscalationReorders {
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
			moved = true
		}
		if err := svc.changes.Record(ctx, tx, spaceID, "task", t.ID, "updated", t.Version+1); err != nil {
			return err
		}
		_, err := svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "task.escalated", Target: audit.Target{Type: "task", ID: t.ID},
			Details: map[string]any{"space_id": spaceID, "reason": reason, "moved_to_top": moved}})
		return err
	})
}

// UpdateInput edits a task's description and assignment.
type UpdateInput struct {
	Title, Objective, Acceptance, Priority string
	Exclusions                             []string
	AssigneeRole, AssigneeRun, Workspace   string
}

// Update edits a task (optimistic on version).
func (svc *Service) Update(ctx context.Context, actor audit.Actor, spaceID, id string, version int64, in UpdateInput) (*Task, error) {
	c := CreateInput{Title: in.Title, Objective: in.Objective, Acceptance: in.Acceptance, Priority: in.Priority, Exclusions: in.Exclusions}
	if err := c.validate(); err != nil {
		return nil, err
	}
	if in.Exclusions == nil {
		in.Exclusions = []string{}
	}
	return svc.mutate(ctx, "queue.update", spaceID, id, version, func(tx *store.Tx, _ *snapshot, t *Task) error {
		ex, _ := json.Marshal(in.Exclusions)
		if _, err := tx.Q.UpdateTaskFields(ctx, db.UpdateTaskFieldsParams{Title: c.Title, Objective: in.Objective, Acceptance: in.Acceptance, Priority: c.Priority,
			ExclusionsJson: string(ex), AssigneeRole: in.AssigneeRole, AssigneeRun: in.AssigneeRun, Workspace: in.Workspace, Now: tx.NowMicros(), ID: t.ID,
			Version: version}); err != nil {
			return err
		}
		if err := svc.changes.Record(ctx, tx, spaceID, "task", t.ID, "updated", version+1); err != nil {
			return err
		}
		_, err := svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "task.updated", Target: audit.Target{Type: "task", ID: t.ID},
			Details: map[string]any{"space_id": spaceID, "priority": c.Priority}})
		return err
	})
}

// StatusInput changes a task's state.
type StatusInput struct {
	Status string
	Reason string
	Result string
}

// SetStatus moves a task between states (SM-QUEUE-008). Starting a task requires it to be eligible; running → ready is
// recorded as a preemption, not a failure.
func (svc *Service) SetStatus(ctx context.Context, actor audit.Actor, spaceID, id string, version int64, in StatusInput) (*Task, error) {
	if _, ok := transitions[in.Status]; !ok {
		return nil, apperr.New(apperr.CodeInvalid, "unknown status %q", in.Status)
	}
	if len(in.Reason) > 2000 || len(in.Result) > 100_000 {
		return nil, apperr.New(apperr.CodeInvalid, "reason or result too long")
	}
	return svc.mutate(ctx, "queue.status", spaceID, id, version, func(tx *store.Tx, sn *snapshot, t *Task) error {
		from := t.Status
		if !slices.Contains(transitions[from], in.Status) {
			return apperr.New(apperr.CodePrecondition, "a %s task cannot become %s", from, in.Status)
		}
		if in.Status == Running {
			if why := sn.why(t); len(why) > 0 {
				return apperr.New(apperr.CodePrecondition, "the task cannot start: %s", why[0])
			}
		}
		preempted := int64(0)
		if from == Running && in.Status == Ready {
			preempted = 1
		}
		result := t.Result
		if in.Result != "" {
			result = in.Result
		}
		if _, err := tx.Q.SetTaskStatus(ctx, db.SetTaskStatusParams{Status: in.Status, StatusReason: in.Reason, Result: result, Preempted: preempted,
			Now: tx.NowMicros(), MarkFinished: b2i(terminal(in.Status)), ID: t.ID, Version: t.Version}); err != nil {
			return err
		}
		if err := svc.changes.Record(ctx, tx, spaceID, "task", t.ID, "updated", t.Version+1); err != nil {
			return err
		}
		_, err := svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "task.status", Target: audit.Target{Type: "task", ID: t.ID},
			Details: map[string]any{"space_id": spaceID, "from": from, "to": in.Status, "reason": in.Reason, "preempted": preempted == 1}})
		return err
	})
}

func b2i(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// SetDependencies replaces a task's dependencies, rejecting cycles (SM-QUEUE-004).
func (svc *Service) SetDependencies(ctx context.Context, actor audit.Actor, spaceID, id string, dependsOn []string) (*Task, error) {
	if len(dependsOn) > 50 {
		return nil, apperr.New(apperr.CodeInvalid, "at most 50 dependencies")
	}
	return svc.mutate(ctx, "queue.dependencies", spaceID, id, 0, func(tx *store.Tx, sn *snapshot, t *Task) error {
		next := map[string][]string{}
		for k, v := range sn.deps {
			next[k] = v
		}
		next[id] = nil
		for _, d := range dependsOn {
			if sn.byID[d] == nil {
				return apperr.New(apperr.CodeInvalid, "dependency %q is not a task of this space", d)
			}
			if d == id {
				return apperr.New(apperr.CodeInvalid, "a task cannot depend on itself")
			}
			if !slices.Contains(next[id], d) {
				next[id] = append(next[id], d)
			}
		}
		if cycle := findCycle(next, id); cycle != nil {
			titles := make([]string, len(cycle))
			for i, c := range cycle {
				titles[i] = fmt.Sprintf("%q", sn.byID[c].Title)
			}
			return apperr.New(apperr.CodeInvalid, "dependency cycle: %v", titles)
		}
		if err := tx.Q.DeleteTaskDependencies(ctx, id); err != nil {
			return err
		}
		for _, d := range next[id] {
			if err := tx.Q.InsertTaskDependency(ctx, db.InsertTaskDependencyParams{TaskID: id, DependsOn: d}); err != nil {
				return err
			}
		}
		if err := svc.changes.Record(ctx, tx, spaceID, "task", id, "updated", t.Version); err != nil {
			return err
		}
		_, err := svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "task.dependencies", Target: audit.Target{Type: "task", ID: id},
			Details: map[string]any{"space_id": spaceID, "depends_on": next[id]}})
		return err
	})
}

// findCycle returns a dependency cycle through start, or nil.
func findCycle(deps map[string][]string, start string) []string {
	var path []string
	seen := map[string]bool{}
	var visit func(n string) bool
	visit = func(n string) bool {
		path = append(path, n)
		for _, d := range deps[n] {
			if d == start {
				return true
			}
			if !seen[d] {
				seen[d] = true
				if visit(d) {
					return true
				}
			}
		}
		path = path[:len(path)-1]
		return false
	}
	if visit(start) {
		return path
	}
	return nil
}

// AddBlocker records an explicit blocker; the task is skipped (with the reason) while it is open.
func (svc *Service) AddBlocker(ctx context.Context, actor audit.Actor, spaceID, id, kind, refID, note string) (*Task, error) {
	if kind != "human_action" && kind != "external" {
		return nil, apperr.New(apperr.CodeInvalid, "kind must be human_action or external")
	}
	if kind == "human_action" && refID == "" {
		return nil, apperr.New(apperr.CodeInvalid, "a human_action blocker needs ref_id")
	}
	return svc.mutate(ctx, "queue.blocker", spaceID, id, 0, func(tx *store.Tx, _ *snapshot, t *Task) error {
		if err := tx.Q.InsertTaskBlocker(ctx, db.InsertTaskBlockerParams{ID: ids.New("blk"), TaskID: id, Kind: kind, RefID: refID, Note: note,
			CreatedAt: tx.NowMicros(), CreatedBy: actor.ID}); err != nil {
			return err
		}
		if err := svc.changes.Record(ctx, tx, spaceID, "task", id, "updated", t.Version); err != nil {
			return err
		}
		_, err := svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "task.blocked", Target: audit.Target{Type: "task", ID: id},
			Details: map[string]any{"space_id": spaceID, "kind": kind, "ref_id": refID}})
		return err
	})
}

// ResolveBlocker closes a blocker.
func (svc *Service) ResolveBlocker(ctx context.Context, actor audit.Actor, spaceID, id, blockerID string) (*Task, error) {
	return svc.mutate(ctx, "queue.unblock", spaceID, id, 0, func(tx *store.Tx, _ *snapshot, t *Task) error {
		n, err := tx.Q.ResolveTaskBlocker(ctx, db.ResolveTaskBlockerParams{Now: sql.NullInt64{Int64: tx.NowMicros(), Valid: true}, By: actor.ID, ID: blockerID, TaskID: id})
		if err != nil {
			return err
		}
		if n == 0 {
			return apperr.New(apperr.CodeNotFound, "open blocker not found")
		}
		if err := svc.changes.Record(ctx, tx, spaceID, "task", id, "updated", t.Version); err != nil {
			return err
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "task.unblocked", Target: audit.Target{Type: "task", ID: id},
			Details: map[string]any{"space_id": spaceID, "blocker_id": blockerID}})
		return err
	})
}

// ResolveBlockersByRef closes every open blocker on a reference (a human action that was answered), inside the
// caller's transaction. The tasks stay in their state: control decides what happens next (SM-QUEUE-010).
func ResolveBlockersByRef(ctx context.Context, tx *store.Tx, kind, refID, by string) ([]string, error) {
	return tx.Q.ResolveBlockersByRef(ctx, db.ResolveBlockersByRefParams{Now: sql.NullInt64{Int64: tx.NowMicros(), Valid: true}, By: by, Kind: kind, RefID: refID})
}

// Next selects the highest-ordered eligible ready task and lists why higher tasks were skipped (SM-QUEUE-005). With
// record set, the decision is stored for inspection.
func (svc *Service) Next(ctx context.Context, actor audit.Actor, spaceID string, record bool) (*Selection, error) {
	sn, err := load(ctx, svc.s.R(), spaceID)
	if err != nil {
		return nil, err
	}
	sel := &Selection{Skipped: []Skip{}}
	for _, t := range sn.order {
		if t.Status != Ready {
			continue
		}
		if len(t.Waiting) > 0 {
			sel.Skipped = append(sel.Skipped, Skip{TaskID: t.ID, Title: t.Title, Reason: t.Waiting[0]})
			continue
		}
		sel.Selected = t
		break
	}
	if record {
		sk, _ := json.Marshal(sel.Skipped)
		selected := ""
		if sel.Selected != nil {
			selected = sel.Selected.ID
		}
		if err := svc.s.W().TxNamed(ctx, "queue.decision", func(tx *store.Tx) error {
			return tx.Q.InsertQueueDecision(ctx, db.InsertQueueDecisionParams{SpaceID: spaceID, SelectedID: selected, SkippedJson: string(sk), At: tx.NowMicros(), By: actor.ID})
		}); err != nil {
			return nil, err
		}
	}
	return sel, nil
}

// Decision is a recorded selection.
type Decision struct {
	SelectedID string    `json:"selected_id,omitempty"`
	Skipped    []Skip    `json:"skipped"`
	At         time.Time `json:"at"`
	By         string    `json:"by"`
}

// Decisions returns recent recorded selections.
func (svc *Service) Decisions(ctx context.Context, spaceID string, limit int) ([]Decision, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := svc.s.R().ListQueueDecisions(ctx, db.ListQueueDecisionsParams{SpaceID: spaceID, Limit: int64(limit)})
	if err != nil {
		return nil, err
	}
	out := make([]Decision, 0, len(rows))
	for _, r := range rows {
		d := Decision{SelectedID: r.SelectedID, At: time.UnixMicro(r.At).UTC(), By: r.By, Skipped: []Skip{}}
		_ = json.Unmarshal([]byte(r.SkippedJson), &d.Skipped)
		out = append(out, d)
	}
	return out, nil
}

// SetPolicy replaces a space's queue policy.
func (svc *Service) SetPolicy(ctx context.Context, actor audit.Actor, spaceID string, p Policy) (Policy, error) {
	if p.MaxActive < 1 || p.MaxActive > 20 {
		return Policy{}, apperr.New(apperr.CodeInvalid, "max_active must be 1-20")
	}
	if p.IncidentInsertion != "top" && p.IncidentInsertion != "priority" {
		return Policy{}, apperr.New(apperr.CodeInvalid, "incident_insertion must be top or priority")
	}
	err := svc.s.W().TxNamed(ctx, "queue.policy", func(tx *store.Tx) error {
		if _, err := tx.Q.GetSpace(ctx, spaceID); errors.Is(err, sql.ErrNoRows) {
			return apperr.New(apperr.CodeNotFound, "space not found")
		}
		if err := tx.Q.UpsertQueuePolicy(ctx, db.UpsertQueuePolicyParams{SpaceID: spaceID, MaxActive: int64(p.MaxActive), IncidentInsertion: p.IncidentInsertion,
			IncidentPreempts: b2i(p.IncidentPreempts), EscalationReorders: b2i(p.EscalationReorders), UpdatedAt: tx.NowMicros(), UpdatedBy: actor.ID}); err != nil {
			return err
		}
		if err := svc.changes.Record(ctx, tx, spaceID, "space", spaceID, "updated", 0); err != nil {
			return err
		}
		_, err := svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "queue.policy_updated", Target: audit.Target{Type: "space", ID: spaceID},
			Details: map[string]any{"max_active": p.MaxActive, "incident_insertion": p.IncidentInsertion, "incident_preempts": p.IncidentPreempts,
				"escalation_reorders": p.EscalationReorders}})
		return err
	})
	if err != nil {
		return Policy{}, err
	}
	_, pol, err := svc.List(ctx, spaceID)
	return pol, err
}
