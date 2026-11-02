package human

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/queue"
	"github.com/aurimasniekis/go-spaceman/internal/schemautil"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Outcomes per type (SM-HUM-003). Each type has distinct actions so a person never "approves" a question.
var outcomes = map[string][]string{
	TypeQuestion: {"answered"},
	TypeApproval: {"approved", "denied"},
	TypeTodo:     {"completed"},
	TypeMessage:  {"acknowledged"},
}

// RespondInput is a person's response.
type RespondInput struct {
	Outcome string
	Response
}

func pending(status string) bool { return status == Open || status == InProgress }

func typeName(r db.HumanAction) string {
	if r.Type == TypeCustom {
		return r.TypeSlug
	}
	return r.Type
}

// load reads an action of a space inside a transaction and checks the expected version.
func load(ctx context.Context, tx *store.Tx, spaceID, id string, version int64) (db.HumanAction, error) {
	r, err := tx.Q.GetHumanAction(ctx, id)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && r.SpaceID != spaceID) {
		return r, errNotFound
	} else if err != nil {
		return r, err
	}
	if !pending(r.Status) {
		return r, apperr.New(apperr.CodeConflict, "the action is already %s; responses are final (create a follow-up to correct it)", r.Status)
	}
	if version != 0 && r.Version != version {
		return r, apperr.New(apperr.CodePrecondition, "the action changed since version %d (now %d)", version, r.Version)
	}
	return r, nil
}

// Respond finalizes an action with a type-specific outcome. Finalization is atomic: of two concurrent responses exactly
// one wins and the other gets a conflict (SM-HUM-007). In the same transaction the related task's blockers are
// resolved (the task keeps its state; control decides) and a notification for the requester is stored (SM-HUM-009).
func (svc *Service) Respond(ctx context.Context, actor audit.Actor, spaceID, id string, version int64, in RespondInput) (*Action, error) {
	err := svc.s.W().TxNamed(ctx, "human.respond", func(tx *store.Tx) error {
		r, err := load(ctx, tx, spaceID, id, version)
		if err != nil {
			return err
		}
		allowed := outcomes[r.Type]
		if r.Type == TypeCustom {
			// The response is checked against the version the action was created with, whatever the type became since.
			tv, err := tx.Q.GetHumanTypeVersion(ctx, r.TypeVersionID)
			if err != nil {
				return apperr.New(apperr.CodePrecondition, "the action's type version is missing")
			}
			_ = json.Unmarshal([]byte(tv.OutcomesJson), &allowed)
			data := in.Data
			if len(data) == 0 || string(data) == "null" {
				data = json.RawMessage(`{}`)
			}
			sch, err := schemautil.CompileObject(json.RawMessage(tv.ResponseSchema))
			if err != nil {
				return err
			}
			if err := schemautil.Validate(sch, data); err != nil {
				return invalid("data", "the response does not match %s v%d: %v", r.TypeSlug, r.TypeVersion, err)
			}
			in.Data = data
		} else if len(in.Data) > 0 && string(in.Data) != "null" {
			return invalid("data", "only custom types take response data")
		}
		if !slices.Contains(allowed, in.Outcome) {
			return invalid("outcome", "a %s takes %s", typeName(r), strings.Join(allowed, " or "))
		}
		if len(in.Answer) > 50_000 || len(in.Note) > 20_000 {
			return invalid("answer", "the response is too long")
		}
		if r.Type == TypeQuestion {
			var q QuestionRequest
			_ = json.Unmarshal([]byte(r.RequestJson), &q)
			if strings.TrimSpace(in.Answer) == "" {
				return invalid("answer", "is required")
			}
			if len(q.Options) > 0 && !q.AllowText && !slices.Contains(q.Options, in.Answer) {
				return invalid("answer", "must be one of the offered options")
			}
		}
		if r.Type == TypeApproval && in.Outcome == "denied" && strings.TrimSpace(in.Note) == "" {
			return invalid("note", "say why the request is denied")
		}
		resp, _ := json.Marshal(in.Response)
		return svc.finalize(ctx, tx, actor, r, Resolved, in.Outcome, string(resp), "", "resolved")
	})
	if err != nil {
		return nil, err
	}
	svc.notified()
	return svc.Get(ctx, spaceID, id)
}

// finalize moves a pending action to a final status. Blockers are resolved for responses and cancellations; an expired
// action leaves its task blocked so a person decides (SM-HUM-010).
func (svc *Service) finalize(ctx context.Context, tx *store.Tx, actor audit.Actor, r db.HumanAction, status, outcome, resp, reason, notice string) error {
	if resp == "" {
		resp = "{}"
	}
	n, err := tx.Q.FinalizeHumanAction(ctx, db.FinalizeHumanActionParams{Status: status, Outcome: outcome, ResponseJson: resp, RespondedBy: actor.ID,
		Now: sql.NullInt64{Int64: tx.NowMicros(), Valid: true}, StatusReason: reason, ID: r.ID, Version: r.Version})
	if err != nil {
		return err
	}
	if n == 0 {
		return apperr.New(apperr.CodeConflict, "the action was finalized concurrently")
	}
	if r.BlocksTask == 1 && status != Expired {
		tasks, err := queue.ResolveBlockersByRef(ctx, tx, "human_action", r.ID, actor.ID)
		if err != nil {
			return err
		}
		for _, t := range tasks {
			if err := svc.changes.Record(ctx, tx, r.SpaceID, "task", t, "updated", 0); err != nil {
				return err
			}
		}
	}
	if r.RequesterKind == "agent" {
		if err := tx.Q.InsertHumanNotification(ctx, db.InsertHumanNotificationParams{ID: ids.New("hnt"), ActionID: r.ID, SpaceID: r.SpaceID,
			TargetRun: r.RequesterRun, TargetSession: r.RequesterSession, TargetRole: r.RequesterRole, Kind: notice, CreatedAt: tx.NowMicros()}); err != nil {
			return err
		}
	}
	if err := svc.changes.Record(ctx, tx, r.SpaceID, "human_action", r.ID, "updated", r.Version+1); err != nil {
		return err
	}
	_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "human_action." + status, Target: audit.Target{Type: "human_action", ID: r.ID},
		Details: map[string]any{"space_id": r.SpaceID, "type": r.Type, "outcome": outcome, "reason": reason, "task_id": r.TaskID}})
	return err
}

// Cancel withdraws a pending action (the requester no longer needs it, or an operator removes it).
func (svc *Service) Cancel(ctx context.Context, actor audit.Actor, spaceID, id string, version int64, reason string) (*Action, error) {
	if len(reason) > 2000 {
		return nil, invalid("reason", "at most 2000 characters")
	}
	err := svc.s.W().TxNamed(ctx, "human.cancel", func(tx *store.Tx) error {
		r, err := load(ctx, tx, spaceID, id, version)
		if err != nil {
			return err
		}
		return svc.finalize(ctx, tx, actor, r, Cancelled, "", "", reason, "cancelled")
	})
	if err != nil {
		return nil, err
	}
	svc.notified()
	return svc.Get(ctx, spaceID, id)
}

// Assign sets (or clears) who works on an action. Assigning moves it to in_progress; clearing returns it to open.
func (svc *Service) Assign(ctx context.Context, actor audit.Actor, spaceID, id string, version int64, assignee string) (*Action, error) {
	if len(assignee) > 200 {
		return nil, invalid("assignee", "is too long")
	}
	err := svc.s.W().TxNamed(ctx, "human.assign", func(tx *store.Tx) error {
		r, err := load(ctx, tx, spaceID, id, version)
		if err != nil {
			return err
		}
		if assignee != "" {
			if _, err := tx.Q.GetUser(ctx, assignee); err != nil {
				return invalid("assignee", "is not a user")
			}
		}
		status := Open
		if assignee != "" {
			status = InProgress
		}
		n, err := tx.Q.UpdateHumanAssignment(ctx, db.UpdateHumanAssignmentParams{Assignee: assignee, Status: status, Now: tx.NowMicros(), ID: id, Version: r.Version})
		if err != nil {
			return err
		}
		if n == 0 {
			return apperr.New(apperr.CodeConflict, "the action changed concurrently")
		}
		if err := svc.changes.Record(ctx, tx, spaceID, "human_action", id, "updated", r.Version+1); err != nil {
			return err
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "human_action.assigned", Target: audit.Target{Type: "human_action", ID: id},
			Details: map[string]any{"space_id": spaceID, "from": r.Assignee, "to": assignee}})
		return err
	})
	if err != nil {
		return nil, err
	}
	return svc.Get(ctx, spaceID, id)
}

// AddComment adds discussion; it never changes the status or counts as a response (SM-HUM-006). Comments are allowed
// on final actions too, so a person can explain a decision afterwards.
func (svc *Service) AddComment(ctx context.Context, actor audit.Actor, spaceID, id, body, label string) (*Action, error) {
	body = strings.TrimSpace(body)
	if body == "" || len(body) > 20_000 {
		return nil, invalid("body", "is required (at most 20000 bytes)")
	}
	err := svc.s.W().TxNamed(ctx, "human.comment", func(tx *store.Tx) error {
		r, err := tx.Q.GetHumanAction(ctx, id)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && r.SpaceID != spaceID) {
			return errNotFound
		} else if err != nil {
			return err
		}
		cid := ids.New(ids.Comment)
		if err := tx.Q.InsertHumanComment(ctx, db.InsertHumanCommentParams{ID: cid, ActionID: id, Body: body, Author: actor.ID, AuthorLabel: label, CreatedAt: tx.NowMicros()}); err != nil {
			return err
		}
		if err := svc.changes.Record(ctx, tx, spaceID, "human_action", id, "updated", r.Version); err != nil {
			return err
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "human_action.commented", Target: audit.Target{Type: "human_action", ID: id},
			Details: map[string]any{"space_id": spaceID, "comment_id": cid}})
		return err
	})
	if err != nil {
		return nil, err
	}
	return svc.Get(ctx, spaceID, id)
}

// Move places a pending action before another pending action of the same space (or last when before is empty).
func (svc *Service) Move(ctx context.Context, actor audit.Actor, spaceID, id, before string) (*Action, error) {
	err := svc.s.W().TxNamed(ctx, "human.move", func(tx *store.Tx) error {
		if _, err := load(ctx, tx, spaceID, id, 0); err != nil {
			return err
		}
		rows, err := tx.Q.ListHumanActions(ctx, db.ListHumanActionsParams{SpaceID: spaceID, Limit: MaxPendingPerSpace + 1})
		if err != nil {
			return err
		}
		order := make([]string, 0, len(rows))
		found := before == ""
		for _, r := range rows {
			if pending(r.Status) && r.ID != id {
				order = append(order, r.ID)
				found = found || r.ID == before
			}
		}
		if !found {
			return invalid("before", "is not a pending action of this space")
		}
		at := len(order)
		if before != "" {
			at = slices.Index(order, before)
		}
		order = slices.Insert(order, at, id)
		for i, o := range order {
			if err := tx.Q.SetHumanRank(ctx, db.SetHumanRankParams{Rank: int64(i+1) * gap, UpdatedAt: tx.NowMicros(), ID: o}); err != nil {
				return err
			}
		}
		if err := svc.changes.Record(ctx, tx, spaceID, "human_action", id, "updated", 0); err != nil {
			return err
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "human_action.moved", Target: audit.Target{Type: "human_action", ID: id},
			Details: map[string]any{"space_id": spaceID, "before": before}})
		return err
	})
	if err != nil {
		return nil, err
	}
	return svc.Get(ctx, spaceID, id)
}

// ListFilter narrows a listing.
type ListFilter struct {
	Pending bool
	Type    string
	Limit   int
}

func (f ListFilter) keep(r db.HumanAction) bool {
	return (!f.Pending || pending(r.Status)) && (f.Type == "" || r.Type == f.Type)
}

func (f ListFilter) limit() int {
	if f.Limit <= 0 || f.Limit > 500 {
		return 200
	}
	return f.Limit
}

// List returns a space's actions: pending first in rank order, then the most recent final ones.
func (svc *Service) List(ctx context.Context, spaceID string, f ListFilter) ([]*Action, error) {
	rows, err := svc.s.R().ListHumanActions(ctx, db.ListHumanActionsParams{SpaceID: spaceID, Limit: int64(f.limit()) + MaxPendingPerSpace})
	if err != nil {
		return nil, err
	}
	out := []*Action{}
	for _, r := range rows {
		if f.keep(r) && len(out) < f.limit() {
			out = append(out, fromRow(r))
		}
	}
	return out, nil
}

// ListPending returns pending actions across the given spaces (the caller's authorized spaces), oldest first, for the
// global view (SM-HUM-001). allowed nil means every space.
func (svc *Service) ListPending(ctx context.Context, allowed func(spaceID string) bool, f ListFilter) ([]*Action, error) {
	rows, err := svc.s.R().ListPendingHumanActions(ctx, 5000)
	if err != nil {
		return nil, err
	}
	out := []*Action{}
	for _, r := range rows {
		if (allowed == nil || allowed(r.SpaceID)) && f.keep(r) && len(out) < f.limit() {
			out = append(out, fromRow(r))
		}
	}
	return out, nil
}

// PendingCounts returns the number of pending actions per space.
func (svc *Service) PendingCounts(ctx context.Context) (map[string]int64, error) {
	rows, err := svc.s.R().CountPendingHuman(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]int64{}
	for _, r := range rows {
		out[r.SpaceID] = r.N
	}
	return out, nil
}

// Expire finalizes pending actions whose expiry passed. Their tasks stay blocked and the requester is notified.
func (svc *Service) Expire(ctx context.Context) (int, error) {
	rows, err := svc.s.R().ExpiredHumanActions(ctx, sql.NullInt64{Int64: svc.clock.Now().UnixMicro(), Valid: true})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range rows {
		err := svc.s.W().TxNamed(ctx, "human.expire", func(tx *store.Tx) error {
			cur, err := load(ctx, tx, r.SpaceID, r.ID, r.Version)
			if err != nil {
				return err
			}
			return svc.finalize(ctx, tx, audit.System, cur, Expired, "", "", "expired at "+tn(r.ExpiresAt).Format(time.RFC3339), "expired")
		})
		var ae *apperr.Error
		if errors.As(err, &ae) && (ae.Code == apperr.CodeConflict || ae.Code == apperr.CodePrecondition) {
			continue // answered or changed meanwhile
		} else if err != nil {
			return n, err
		}
		n++
	}
	if n > 0 {
		svc.notified()
	}
	return n, nil
}

func invalid(field, format string, args ...any) error {
	v := &apperr.Validation{}
	v.Add(field, format, args...)
	return v.Err()
}
