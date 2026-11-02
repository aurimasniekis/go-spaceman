// Package queue is each space's execution queue (SM-QUEUE-*): one explicit order, where priority and policy only decide
// where a new task is inserted and operator moves are authoritative. SpaceMan enforces the mechanics (order,
// dependencies, blockers, resource exclusions, the active limit, states) and records why tasks were skipped; control
// decides what to do. Nothing here accepts a task automatically.
package queue

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/outbox"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Statuses (SM-QUEUE-008).
const (
	Backlog   = "backlog"
	Ready     = "ready"
	Running   = "running"
	Blocked   = "blocked"
	Review    = "review"
	Done      = "done"
	Failed    = "failed"
	Cancelled = "cancelled"
)

const gap = 1024

var priorities = map[string]int{"low": 0, "normal": 1, "high": 2, "urgent": 3}

// transitions lists the allowed status changes. Running → ready is a preemption, never a failure; nothing moves to done
// by itself (worker success and human answers never accept a task, SM-QUEUE-010).
var transitions = map[string][]string{
	Backlog:   {Ready, Cancelled},
	Ready:     {Running, Backlog, Blocked, Cancelled},
	Running:   {Review, Done, Failed, Blocked, Ready, Cancelled},
	Blocked:   {Ready, Running, Cancelled, Failed},
	Review:    {Done, Running, Ready, Failed, Cancelled},
	Done:      {Ready},
	Failed:    {Ready, Cancelled},
	Cancelled: {Backlog, Ready},
}

func terminal(s string) bool { return s == Done || s == Failed || s == Cancelled }

// Task is one unit of accepted work (SM-QUEUE-003).
type Task struct {
	ID           string     `json:"id"`
	SpaceID      string     `json:"space_id"`
	ParentID     string     `json:"parent_id,omitempty"`
	Title        string     `json:"title"`
	Objective    string     `json:"objective" doc:"Markdown"`
	Acceptance   string     `json:"acceptance" doc:"Acceptance criteria (Markdown)"`
	Kind         string     `json:"kind" enum:"ordinary,incident"`
	Priority     string     `json:"priority" enum:"low,normal,high,urgent"`
	Position     int        `json:"position" doc:"1-based place in the effective order"`
	Depth        int        `json:"depth" doc:"0 for top-level tasks"`
	Manual       bool       `json:"manual" doc:"Placed by an operator: duplicate triggers never move it"`
	Status       string     `json:"status" enum:"backlog,ready,running,blocked,review,done,failed,cancelled"`
	StatusReason string     `json:"status_reason,omitempty"`
	Exclusions   []string   `json:"exclusions" doc:"Resource keys this task holds exclusively while running"`
	AssigneeRole string     `json:"assignee_role,omitempty"`
	AssigneeRun  string     `json:"assignee_run,omitempty"`
	Workspace    string     `json:"workspace,omitempty"`
	OriginKind   string     `json:"origin_kind,omitempty"`
	OriginID     string     `json:"origin_id,omitempty"`
	DedupKey     string     `json:"dedup_key,omitempty"`
	Result       string     `json:"result,omitempty"`
	Preemptions  int64      `json:"preemptions"`
	DependsOn    []string   `json:"depends_on"`
	Blockers     []Blocker  `json:"blockers"`
	Waiting      []string   `json:"waiting,omitempty" doc:"Why a ready task is not eligible now"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
	StartedAt    *time.Time `json:"started_at,omitempty"`
	FinishedAt   *time.Time `json:"finished_at,omitempty"`
	Version      int64      `json:"version"`
}

// Blocker is an explicit reason a task cannot proceed (SM-QUEUE-006).
type Blocker struct {
	ID         string     `json:"id"`
	Kind       string     `json:"kind" enum:"human_action,external"`
	RefID      string     `json:"ref_id,omitempty"`
	Note       string     `json:"note,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	ResolvedAt *time.Time `json:"resolved_at,omitempty"`
}

// Policy is a space's queue policy (SM-QUEUE-007/009).
type Policy struct {
	MaxActive          int    `json:"max_active" minimum:"1" maximum:"20" doc:"Top-level tasks that may run at once (default 1)"`
	IncidentInsertion  string `json:"incident_insertion" enum:"top,priority" doc:"Where new incident tasks go"`
	IncidentPreempts   bool   `json:"incident_preempts" doc:"A new incident returns running ordinary top-level tasks to ready (preemption, not failure)"`
	EscalationReorders bool   `json:"escalation_reorders" doc:"Escalation may move a task to the top; otherwise it only raises priority"`
	Version            int64  `json:"version"`
}

// Skip is a ready task that was not selected, and why (SM-QUEUE-005).
type Skip struct {
	TaskID string `json:"task_id"`
	Title  string `json:"title"`
	Reason string `json:"reason"`
}

// Selection is the result of choosing the next task.
type Selection struct {
	Selected *Task  `json:"selected,omitempty"`
	Skipped  []Skip `json:"skipped"`
}

// Service manages queues.
type Service struct {
	s       *store.Store
	audit   *audit.Writer
	changes *changefeed.Recorder
	// OnStatus is called after a commit that changed a task's state, e.g. to wake control.
	OnStatus func(actor audit.Actor, t *Task, from string)
	// Outbox records task events for publication to Pulse (SM-OBS-004). Nil: nothing is published.
	Outbox *outbox.Outbox
}

// New returns the service.
func New(s *store.Store, aw *audit.Writer, ch *changefeed.Recorder) *Service {
	return &Service{s: s, audit: aw, changes: ch}
}

var errNotFound = apperr.New(apperr.CodeNotFound, "task not found")

func t2(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := time.UnixMicro(v.Int64).UTC()
	return &t
}

func fromRow(r db.Task) *Task {
	t := &Task{ID: r.ID, SpaceID: r.SpaceID, ParentID: r.ParentID.String, Title: r.Title, Objective: r.Objective, Acceptance: r.Acceptance, Kind: r.Kind,
		Priority: r.Priority, Manual: r.ManualPosition == 1, Status: r.Status, StatusReason: r.StatusReason, AssigneeRole: r.AssigneeRole,
		AssigneeRun: r.AssigneeRun, Workspace: r.Workspace, OriginKind: r.OriginKind, OriginID: r.OriginID, DedupKey: r.DedupKey, Result: r.Result,
		Preemptions: r.Preemptions, CreatedAt: time.UnixMicro(r.CreatedAt).UTC(), UpdatedAt: time.UnixMicro(r.UpdatedAt).UTC(),
		StartedAt: t2(r.StartedAt), FinishedAt: t2(r.FinishedAt), Version: r.Version, DependsOn: []string{}, Blockers: []Blocker{}, Exclusions: []string{}}
	_ = json.Unmarshal([]byte(r.ExclusionsJson), &t.Exclusions)
	return t
}

// snapshot is a space's whole queue, loaded once per operation (queues are small: tens to hundreds of tasks).
type snapshot struct {
	rows   []db.Task
	byID   map[string]*Task
	order  []*Task // effective order: depth-first by rank
	deps   map[string][]string
	policy Policy
}

func defaultPolicy() Policy { return Policy{MaxActive: 1, IncidentInsertion: "top"} }

func load(ctx context.Context, q *db.Queries, spaceID string) (*snapshot, error) {
	rows, err := q.ListTasks(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	sn := &snapshot{rows: rows, byID: map[string]*Task{}, deps: map[string][]string{}, policy: defaultPolicy()}
	children := map[string][]*Task{}
	for _, r := range rows {
		t := fromRow(r)
		sn.byID[t.ID] = t
		children[t.ParentID] = append(children[t.ParentID], t)
	}
	var walk func(parent string, depth int)
	walk = func(parent string, depth int) {
		for _, t := range children[parent] {
			t.Depth = depth
			sn.order = append(sn.order, t)
			t.Position = len(sn.order)
			walk(t.ID, depth+1)
		}
	}
	walk("", 0)
	ds, err := q.ListSpaceDependencies(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	for _, d := range ds {
		sn.deps[d.TaskID] = append(sn.deps[d.TaskID], d.DependsOn)
		if t := sn.byID[d.TaskID]; t != nil {
			t.DependsOn = append(t.DependsOn, d.DependsOn)
		}
	}
	bs, err := q.ListSpaceBlockers(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	for _, b := range bs {
		if t := sn.byID[b.TaskID]; t != nil && !b.ResolvedAt.Valid {
			t.Blockers = append(t.Blockers, Blocker{ID: b.ID, Kind: b.Kind, RefID: b.RefID, Note: b.Note, CreatedAt: time.UnixMicro(b.CreatedAt).UTC()})
		}
	}
	if p, err := q.GetQueuePolicy(ctx, spaceID); err == nil {
		sn.policy = Policy{MaxActive: int(p.MaxActive), IncidentInsertion: p.IncidentInsertion, IncidentPreempts: p.IncidentPreempts == 1,
			EscalationReorders: p.EscalationReorders == 1, Version: p.Version}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	sn.annotate()
	return sn, nil
}

func (sn *snapshot) siblings(parent string) []db.Task {
	var out []db.Task
	for _, r := range sn.rows {
		if r.ParentID.String == parent {
			out = append(out, r)
		}
	}
	return out
}

func (sn *snapshot) runningTop() int {
	n := 0
	for _, t := range sn.byID {
		if t.Status == Running && t.ParentID == "" {
			n++
		}
	}
	return n
}

// why returns the reasons a ready task cannot start now (empty: eligible).
func (sn *snapshot) why(t *Task) []string {
	var out []string
	for _, d := range sn.deps[t.ID] {
		if dt := sn.byID[d]; dt != nil && dt.Status != Done {
			out = append(out, fmt.Sprintf("waiting for %q (%s)", dt.Title, dt.Status))
		}
	}
	for _, b := range t.Blockers {
		switch {
		case b.Kind == "human_action" && b.RefID != "":
			out = append(out, "waiting for human action "+b.RefID)
		case b.Note != "":
			out = append(out, "blocked: "+b.Note)
		default:
			out = append(out, "blocked")
		}
	}
	if t.ParentID != "" {
		if p := sn.byID[t.ParentID]; p != nil && p.Status != Running {
			out = append(out, fmt.Sprintf("parent %q is %s", p.Title, p.Status))
		}
	} else if n := sn.runningTop(); n >= sn.policy.MaxActive {
		out = append(out, fmt.Sprintf("%d top-level task(s) already running (limit %d)", n, sn.policy.MaxActive))
	}
	for _, x := range t.Exclusions {
		for _, o := range sn.byID {
			if o.ID != t.ID && o.Status == Running && slices.Contains(o.Exclusions, x) {
				out = append(out, fmt.Sprintf("resource %s is held by %q", x, o.Title))
			}
		}
	}
	return out
}

func (sn *snapshot) annotate() {
	for _, t := range sn.order {
		if t.Status == Ready {
			t.Waiting = sn.why(t)
		}
	}
}

// insertRank picks the rank for a new task among its siblings: incidents go to the top when the policy says so;
// everything else goes after the last active sibling with the same or a higher priority (SM-QUEUE-001).
func (sn *snapshot) insertRank(ctx context.Context, tx *store.Tx, parent, kind, priority string) (int64, error) {
	sib := sn.siblings(parent)
	idx := len(sib)
	switch {
	case kind == "incident" && sn.policy.IncidentInsertion == "top":
		idx = 0
	default:
		for i, s := range sib {
			if !terminal(s.Status) && priorities[s.Priority] < priorities[priority] {
				idx = i
				break
			}
		}
	}
	return rankAt(ctx, tx, sib, idx)
}

// rankAt returns a rank that sorts at position idx among sibs, rebalancing them when there is no room.
func rankAt(ctx context.Context, tx *store.Tx, sibs []db.Task, idx int) (int64, error) {
	lo, hi := int64(0), int64(0)
	switch {
	case len(sibs) == 0:
		return gap, nil
	case idx == 0:
		return sibs[0].Rank - gap, nil
	case idx >= len(sibs):
		return sibs[len(sibs)-1].Rank + gap, nil
	default:
		lo, hi = sibs[idx-1].Rank, sibs[idx].Rank
	}
	if hi-lo > 1 {
		return lo + (hi-lo)/2, nil
	}
	for i, s := range sibs {
		r := int64(i+1) * gap * 2
		if i >= idx {
			r += gap * 2
		}
		if err := tx.Q.SetTaskRankOnly(ctx, db.SetTaskRankOnlyParams{Rank: r, ID: s.ID}); err != nil {
			return 0, err
		}
	}
	return int64(idx+1) * gap * 2, nil
}

// CreateInput is a new task.
type CreateInput struct {
	Title, Objective, Acceptance string
	Kind, Priority, ParentID     string
	Status                       string // backlog (default) or ready
	Exclusions                   []string
	AssigneeRole, Workspace      string
	OriginKind, OriginID         string
	// DedupKey makes repeated triggers idempotent: an existing task with the key is returned unchanged (never moved).
	DedupKey  string
	DependsOn []string
}

func (in *CreateInput) validate() error {
	v := &apperr.Validation{}
	in.Title = strings.TrimSpace(in.Title)
	if in.Title == "" || utf8.RuneCountInString(in.Title) > 200 {
		v.Add("title", "is required (at most 200 characters)")
	}
	if len(in.Objective) > 100_000 || len(in.Acceptance) > 50_000 {
		v.Add("objective", "objective and acceptance are limited to 100000 and 50000 bytes")
	}
	if in.Kind == "" {
		in.Kind = "ordinary"
	}
	if in.Kind != "ordinary" && in.Kind != "incident" {
		v.Add("kind", "must be ordinary or incident")
	}
	if in.Priority == "" {
		in.Priority = "normal"
	}
	if _, ok := priorities[in.Priority]; !ok {
		v.Add("priority", "must be low, normal, high or urgent")
	}
	if in.Status == "" {
		in.Status = Backlog
	}
	if in.Status != Backlog && in.Status != Ready {
		v.Add("status", "new tasks start in backlog or ready")
	}
	if err := checkExclusions(in.Exclusions); err != nil {
		v.Add("exclusions", "%v", err)
	}
	return v.Err()
}

func checkExclusions(xs []string) error {
	if len(xs) > 20 {
		return errors.New("at most 20 resource keys")
	}
	for _, x := range xs {
		if x == "" || len(x) > 100 || strings.ContainsAny(x, " \t\n") {
			return fmt.Errorf("%q is not a resource key (no spaces, at most 100 characters)", x)
		}
	}
	return nil
}

// Create adds a task. With a dedup key that already exists, the existing task is returned and created is false: a
// duplicate trigger never re-promotes a task an operator moved (SM-QUEUE-002).
func (svc *Service) Create(ctx context.Context, actor audit.Actor, spaceID string, in CreateInput) (*Task, bool, error) {
	if err := in.validate(); err != nil {
		return nil, false, err
	}
	if in.Exclusions == nil {
		in.Exclusions = []string{}
	}
	id := ids.New(ids.Task)
	created := false
	var preempted []string
	err := svc.s.W().TxNamed(ctx, "queue.create", func(tx *store.Tx) error {
		created, preempted = false, nil
		sp, err := tx.Q.GetSpace(ctx, spaceID)
		if errors.Is(err, sql.ErrNoRows) {
			return apperr.New(apperr.CodeNotFound, "space not found")
		} else if err != nil {
			return err
		}
		if sp.State == "archived" {
			return apperr.New(apperr.CodePrecondition, "the space is archived")
		}
		if in.DedupKey != "" {
			if ex, err := tx.Q.GetTaskByDedup(ctx, db.GetTaskByDedupParams{SpaceID: spaceID, DedupKey: in.DedupKey}); err == nil {
				id = ex.ID
				return nil
			} else if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		sn, err := load(ctx, tx.Q, spaceID)
		if err != nil {
			return err
		}
		if in.ParentID != "" && sn.byID[in.ParentID] == nil {
			return apperr.New(apperr.CodeInvalid, "parent %q is not a task of this space", in.ParentID)
		}
		for _, d := range in.DependsOn {
			if sn.byID[d] == nil {
				return apperr.New(apperr.CodeInvalid, "dependency %q is not a task of this space", d)
			}
		}
		rank, err := sn.insertRank(ctx, tx, in.ParentID, in.Kind, in.Priority)
		if err != nil {
			return err
		}
		ex, _ := json.Marshal(in.Exclusions)
		if err := tx.Q.InsertTask(ctx, db.InsertTaskParams{ID: id, SpaceID: spaceID, ParentID: sql.NullString{String: in.ParentID, Valid: in.ParentID != ""},
			Title: in.Title, Objective: in.Objective, Acceptance: in.Acceptance, Kind: in.Kind, Priority: in.Priority, Rank: rank, Status: in.Status,
			ExclusionsJson: string(ex), AssigneeRole: in.AssigneeRole, Workspace: in.Workspace, OriginKind: in.OriginKind, OriginID: in.OriginID,
			DedupKey: in.DedupKey, CreatedAt: tx.NowMicros(), CreatedBy: actor.ID, UpdatedAt: tx.NowMicros()}); err != nil {
			return err
		}
		for _, d := range in.DependsOn {
			if err := tx.Q.InsertTaskDependency(ctx, db.InsertTaskDependencyParams{TaskID: id, DependsOn: d}); err != nil {
				return err
			}
		}
		created = true
		if in.Kind == "incident" && in.ParentID == "" && sn.policy.IncidentPreempts {
			for _, t := range sn.order {
				if t.ParentID == "" && t.Kind == "ordinary" && t.Status == Running {
					if _, err := tx.Q.SetTaskStatus(ctx, db.SetTaskStatusParams{Status: Ready, StatusReason: "preempted by incident " + in.Title, Result: t.Result,
						Preempted: 1, Now: tx.NowMicros(), MarkFinished: int64(0), ID: t.ID, Version: t.Version}); err != nil {
						return err
					}
					preempted = append(preempted, t.ID)
					if err := svc.changes.Record(ctx, tx, spaceID, "task", t.ID, "updated", t.Version+1); err != nil {
						return err
					}
					if _, err := svc.Outbox.Enqueue(ctx, tx, outbox.Event{Payload: outbox.TaskStatusChanged{Task: eventTask(t), From: Running, To: Ready,
						Preempted: true}, Correlation: t.ID, Causation: id}); err != nil {
						return err
					}
				}
			}
		}
		if err := svc.changes.Record(ctx, tx, spaceID, "task", id, "created", 1); err != nil {
			return err
		}
		if _, err := svc.Outbox.Enqueue(ctx, tx, outbox.Event{Payload: outbox.TaskCreated{Task: outbox.Task{SpaceID: spaceID, TaskID: id,
			ParentID: in.ParentID, Kind: in.Kind, Priority: in.Priority, AssigneeRole: outbox.CleanID(in.AssigneeRole), OriginKind: outbox.CleanID(in.OriginKind),
			OriginID: outbox.CleanID(in.OriginID)}, Status: in.Status}, Correlation: id, Causation: in.OriginID}); err != nil {
			return err
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "task.created", Target: audit.Target{Type: "task", ID: id},
			Details: map[string]any{"space_id": spaceID, "kind": in.Kind, "priority": in.Priority, "origin": in.OriginKind + ":" + in.OriginID, "preempted": preempted}})
		return err
	})
	if err != nil {
		return nil, false, err
	}
	t, err := svc.Get(ctx, spaceID, id)
	return t, created, err
}

// eventTask is the published metadata of a task (never its title, objective, acceptance, reason or result).
func eventTask(t *Task) outbox.Task {
	return outbox.Task{SpaceID: t.SpaceID, TaskID: t.ID, ParentID: t.ParentID, Kind: t.Kind, Priority: t.Priority, AssigneeRole: outbox.CleanID(t.AssigneeRole),
		OriginKind: outbox.CleanID(t.OriginKind), OriginID: outbox.CleanID(t.OriginID)}
}

// Get returns a task of a space with its dependencies, open blockers and eligibility.
func (svc *Service) Get(ctx context.Context, spaceID, id string) (*Task, error) {
	sn, err := load(ctx, svc.s.R(), spaceID)
	if err != nil {
		return nil, err
	}
	t := sn.byID[id]
	if t == nil {
		return nil, errNotFound
	}
	return t, nil
}

// List returns a space's queue in its effective order, with eligibility notes on ready tasks, and the policy.
func (svc *Service) List(ctx context.Context, spaceID string) ([]*Task, Policy, error) {
	sn, err := load(ctx, svc.s.R(), spaceID)
	if err != nil {
		return nil, Policy{}, err
	}
	return append([]*Task{}, sn.order...), sn.policy, nil
}
