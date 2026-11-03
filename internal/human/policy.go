package human

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"slices"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// What happens to a response when the requester's run has ended (SM-HUM-009).
const (
	// EndedReroute routes the response at once: to a live run of the successor role, else to control, else to the
	// operators.
	EndedReroute = "reroute"
	// EndedNextLaunch keeps it for the next run of the requester's agent instance (a resumed session: control restarts,
	// sleeping KB sessions, an operator relaunching the instance) and reroutes it when none starts within the hold.
	EndedNextLaunch = "next_launch"
)

// Whether a final response also becomes an inbox item of the action's space (SM-INBOX-001, ADR-0049).
const (
	// ResponseInboxOff: responses reach agents as notifications only (the default).
	ResponseInboxOff = "off"
	// ResponseInboxRouted: a response routed to control or the operators (its requester's run had ended) also becomes
	// an inbox item, so it is triaged like other inputs instead of only being acknowledged.
	ResponseInboxRouted = "routed"
	// ResponseInboxAlways: every response of the type becomes an inbox item, whoever asked (operators and SpaceMan
	// itself included), so control learns of answers to questions it did not ask.
	ResponseInboxAlways = "always"
)

// PolicyTypes are the action types a policy can name: 'all' (every type), the built-in types and 'custom' (every custom type).
var PolicyTypes = []string{"all", TypeQuestion, TypeApproval, TypeTodo, TypeMessage, TypeCustom}

// Bounds of the policy fields.
const (
	MaxPolicyMinutes   = 30 * 24 * 60 // reminders and escalation within 30 days
	MaxHoldMinutes     = 7 * 24 * 60
	MaxRemindersBound  = 20
	MaxRatePerRunHour  = 1000
	MaxRatePerSpaceHr  = 10000
	rateWindow         = time.Hour
	maxReroutes        = 5
	policyReapplyLimit = 5000
)

// PolicyFields is what a policy decides (SM-HUM-009/011).
type PolicyFields struct {
	RemindAfterMinutes   int64  `json:"remind_after_minutes" minimum:"0" maximum:"43200" doc:"Remind people this long after the action was asked; 0: no reminders"`
	RemindEveryMinutes   int64  `json:"remind_every_minutes" minimum:"0" maximum:"43200" doc:"Repeat the reminder this often; 0: once"`
	MaxReminders         int64  `json:"max_reminders" minimum:"1" maximum:"20"`
	EscalateAfterMinutes int64  `json:"escalate_after_minutes" minimum:"0" maximum:"43200" doc:"Escalate this long after the action was asked; 0: never"`
	EscalatePriority     string `json:"escalate_priority" enum:",high,urgent" doc:"Raise the priority to this (never lowers it); the order of the queue is unchanged"`
	EscalateAssignee     string `json:"escalate_assignee" maxLength:"64" doc:"Assign the action to this user when it escalates"`
	RequesterEnded       string `json:"requester_ended" enum:"reroute,next_launch" doc:"When the requester's run has ended: reroute at once, or keep it for the next run of the same agent instance"`
	HoldMinutes          int64  `json:"hold_minutes" minimum:"1" maximum:"10080" doc:"next_launch: how long to wait for that run before rerouting"`
	SuccessorRole        string `json:"successor_role" maxLength:"48" doc:"Reroute to a live run of this role before control; empty: control"`
	WakeIdle             bool   `json:"wake_idle" doc:"Wake an idle requester with a one-line terminal notice (otherwise it reads the response the next time it checks its messages)"`
	RatePerRunHour       int64  `json:"rate_per_run_hour" minimum:"1" maximum:"1000" doc:"New actions of this type one agent run may ask per hour"`
	RatePerSpaceHour     int64  `json:"rate_per_space_hour" minimum:"1" maximum:"10000" doc:"New actions of this type agents of one space may ask per hour"`
	ResponseInbox        string `json:"response_inbox,omitempty" enum:"off,routed,always" doc:"Whether a response also becomes an inbox item of the space (source human_response; metadata only, the response stays on the action): off (default), routed (when it is routed to control or the operators because the requester's run ended) or always"`
}

// Policy is a stored policy, or an effective one with its source.
type Policy struct {
	PolicyFields
	ID         string     `json:"id,omitempty"`
	SpaceID    string     `json:"space_id,omitempty" doc:"Empty: the installation default"`
	ActionType string     `json:"action_type" enum:"all,question,approval,todo,message,custom"`
	Source     string     `json:"source,omitempty" doc:"Effective policies: where they come from (space, default or built-in, with the type it names)"`
	UpdatedAt  *time.Time `json:"updated_at,omitempty"`
	UpdatedBy  string     `json:"updated_by,omitempty"`
	Version    int64      `json:"version,omitempty"`
}

var reRoleName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,47}$`)

// Builtin is the policy that applies when no stored policy names a type.
func Builtin(actionType string) PolicyFields {
	p := PolicyFields{MaxReminders: 1, RequesterEnded: EndedReroute, HoldMinutes: 60, WakeIdle: true, RatePerRunHour: 30, RatePerSpaceHour: 300,
		ResponseInbox: ResponseInboxOff}
	switch actionType {
	case TypeQuestion:
		p.RemindAfterMinutes, p.RemindEveryMinutes, p.MaxReminders = 4*60, 24*60, 3
		p.EscalateAfterMinutes, p.EscalatePriority = 24*60, "high"
	case TypeApproval:
		p.RemindAfterMinutes, p.RemindEveryMinutes, p.MaxReminders = 60, 8*60, 3
		p.EscalateAfterMinutes, p.EscalatePriority = 8*60, "high"
	case TypeTodo, TypeCustom:
		p.RemindAfterMinutes, p.MaxReminders = 24*60, 1
	}
	return p
}

func policyKey(typ string) string {
	if slices.Contains(PolicyTypes, typ) && typ != "all" {
		return typ
	}
	return TypeCustom
}

// validatePolicy checks policy fields. It is a function, not a method: PolicyFields is embedded in Policy, and a
// promoted method stops huma from adding the $schema link to the response type.
func validatePolicy(p PolicyFields) error {
	v := &apperr.Validation{}
	in := func(field string, n, lo, hi int64) {
		if n < lo || n > hi {
			v.Add(field, "must be between %d and %d", lo, hi)
		}
	}
	in("remind_after_minutes", p.RemindAfterMinutes, 0, MaxPolicyMinutes)
	in("remind_every_minutes", p.RemindEveryMinutes, 0, MaxPolicyMinutes)
	in("max_reminders", p.MaxReminders, 1, MaxRemindersBound)
	in("escalate_after_minutes", p.EscalateAfterMinutes, 0, MaxPolicyMinutes)
	in("hold_minutes", p.HoldMinutes, 1, MaxHoldMinutes)
	in("rate_per_run_hour", p.RatePerRunHour, 1, MaxRatePerRunHour)
	in("rate_per_space_hour", p.RatePerSpaceHour, 1, MaxRatePerSpaceHr)
	if p.EscalatePriority != "" && p.EscalatePriority != "high" && p.EscalatePriority != "urgent" {
		v.Add("escalate_priority", "must be empty, high or urgent")
	}
	if p.EscalateAfterMinutes > 0 && p.EscalatePriority == "" && p.EscalateAssignee == "" {
		v.Add("escalate_after_minutes", "escalation raises the priority, assigns someone, or both: set escalate_priority or escalate_assignee")
	}
	if p.RequesterEnded != EndedReroute && p.RequesterEnded != EndedNextLaunch {
		v.Add("requester_ended", "must be reroute or next_launch")
	}
	if p.ResponseInbox != ResponseInboxOff && p.ResponseInbox != ResponseInboxRouted && p.ResponseInbox != ResponseInboxAlways {
		v.Add("response_inbox", "must be off, routed or always")
	}
	if p.SuccessorRole != "" && !reRoleName.MatchString(p.SuccessorRole) {
		v.Add("successor_role", "must be a role name (lowercase letters, digits and dashes)")
	}
	return v.Err()
}

func policyFromRow(r db.HumanPolicy) Policy {
	t := time.UnixMicro(r.UpdatedAt).UTC()
	return Policy{ID: r.ID, SpaceID: r.SpaceID.String, ActionType: r.ActionType, UpdatedAt: &t, UpdatedBy: r.UpdatedBy, Version: r.Version,
		PolicyFields: PolicyFields{RemindAfterMinutes: r.RemindAfterMinutes, RemindEveryMinutes: r.RemindEveryMinutes, MaxReminders: r.MaxReminders,
			EscalateAfterMinutes: r.EscalateAfterMinutes, EscalatePriority: r.EscalatePriority, EscalateAssignee: r.EscalateAssignee,
			RequesterEnded: r.RequesterEnded, HoldMinutes: r.HoldMinutes, SuccessorRole: r.SuccessorRole, WakeIdle: r.WakeIdle == 1,
			RatePerRunHour: r.RatePerRunHour, RatePerSpaceHour: r.RatePerSpaceHour, ResponseInbox: r.ResponseInbox}}
}

// effective picks the most specific policy for a type in a space: space and type, space and 'all', default and type,
// default and 'all', then the built-in policy of the type. The chosen policy applies as a whole.
func effective(rows []db.HumanPolicy, spaceID, typ string) Policy {
	key := policyKey(typ)
	rank := func(r db.HumanPolicy) int {
		n := 0
		if r.SpaceID.Valid {
			if r.SpaceID.String != spaceID {
				return -1
			}
			n += 2
		}
		if r.ActionType == key {
			n++
		} else if r.ActionType != "all" {
			return -1
		}
		return n
	}
	best, at := -1, -1
	for i, r := range rows {
		if n := rank(r); n > best {
			best, at = n, i
		}
	}
	if at < 0 {
		return Policy{SpaceID: spaceID, ActionType: key, PolicyFields: Builtin(key), Source: "built-in"}
	}
	p := policyFromRow(rows[at])
	scope := "default"
	if p.SpaceID != "" {
		scope = "space"
	}
	p.Source = scope + ":" + p.ActionType
	p.ActionType = key
	return p
}

type querier interface {
	ListHumanPoliciesFor(ctx context.Context, arg db.ListHumanPoliciesForParams) ([]db.HumanPolicy, error)
}

func policyFor(ctx context.Context, q querier, spaceID, typ string) (Policy, error) {
	rows, err := q.ListHumanPoliciesFor(ctx, db.ListHumanPoliciesForParams{SpaceID: sql.NullString{String: spaceID, Valid: true}, ActionType: policyKey(typ)})
	if err != nil {
		return Policy{}, err
	}
	return effective(rows, spaceID, typ), nil
}

// EffectivePolicies returns the policy that applies to each type in a space (spaceID empty: the installation defaults).
func (svc *Service) EffectivePolicies(ctx context.Context, spaceID string) ([]Policy, error) {
	out := []Policy{}
	for _, t := range PolicyTypes[1:] {
		p, err := policyFor(ctx, svc.s.R(), spaceID, t)
		if err != nil {
			return nil, err
		}
		if spaceID == "" {
			p.SpaceID = ""
		}
		out = append(out, p)
	}
	return out, nil
}

// Policies returns the stored policies of a space, or the installation defaults when spaceID is empty.
func (svc *Service) Policies(ctx context.Context, spaceID string) ([]Policy, error) {
	var rows []db.HumanPolicy
	var err error
	if spaceID == "" {
		rows, err = svc.s.R().ListDefaultHumanPolicies(ctx)
	} else {
		if _, err := svc.s.R().GetSpace(ctx, spaceID); err != nil {
			return nil, apperr.New(apperr.CodeNotFound, "space not found")
		}
		rows, err = svc.s.R().ListSpaceHumanPolicies(ctx, sql.NullString{String: spaceID, Valid: true})
	}
	if err != nil {
		return nil, err
	}
	out := make([]Policy, 0, len(rows))
	for _, r := range rows {
		out = append(out, policyFromRow(r))
	}
	return out, nil
}

// SetPolicy creates or replaces the policy of a type in a space (spaceID empty: the installation default). version
// 0 creates or overwrites; otherwise it must match. Pending actions in scope get their reminder and escalation times
// recomputed, so a shorter delay applies at once.
func (svc *Service) SetPolicy(ctx context.Context, actor audit.Actor, spaceID, actionType string, version int64, in PolicyFields) (*Policy, error) {
	if !slices.Contains(PolicyTypes, actionType) {
		return nil, invalid("action_type", "must be all, question, approval, todo, message or custom")
	}
	if in.ResponseInbox == "" {
		in.ResponseInbox = ResponseInboxOff // older clients do not send it
	}
	if err := validatePolicy(in); err != nil {
		return nil, err
	}
	sp := sql.NullString{String: spaceID, Valid: spaceID != ""}
	var id string
	err := svc.s.W().TxNamed(ctx, "human.policy_set", func(tx *store.Tx) error {
		if spaceID != "" {
			s, err := tx.Q.GetSpace(ctx, spaceID)
			if err != nil {
				return apperr.New(apperr.CodeNotFound, "space not found")
			}
			if s.State == "archived" {
				return apperr.New(apperr.CodePrecondition, "the space is archived")
			}
		}
		if in.EscalateAssignee != "" {
			if _, err := tx.Q.GetUser(ctx, in.EscalateAssignee); err != nil {
				return invalid("escalate_assignee", "is not a user")
			}
		}
		cur, err := getPolicy(ctx, tx.Q, spaceID, actionType)
		action := "human_policy.updated"
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if version != 0 {
				return apperr.New(apperr.CodePrecondition, "there is no policy at version %d", version)
			}
			id, action = ids.New("hpl"), "human_policy.created"
			if err := tx.Q.InsertHumanPolicy(ctx, db.InsertHumanPolicyParams{ID: id, SpaceID: sp, ActionType: actionType, RemindAfterMinutes: in.RemindAfterMinutes,
				RemindEveryMinutes: in.RemindEveryMinutes, MaxReminders: in.MaxReminders, EscalateAfterMinutes: in.EscalateAfterMinutes,
				EscalatePriority: in.EscalatePriority, EscalateAssignee: in.EscalateAssignee, RequesterEnded: in.RequesterEnded, HoldMinutes: in.HoldMinutes,
				SuccessorRole: in.SuccessorRole, WakeIdle: b2i(in.WakeIdle), RatePerRunHour: in.RatePerRunHour, RatePerSpaceHour: in.RatePerSpaceHour,
				CreatedAt: tx.NowMicros(), CreatedBy: actor.ID, UpdatedAt: tx.NowMicros(), UpdatedBy: actor.ID, ResponseInbox: in.ResponseInbox}); err != nil {
				return err
			}
		case err != nil:
			return err
		default:
			id = cur.ID
			if version != 0 && cur.Version != version {
				return apperr.New(apperr.CodePrecondition, "the policy changed since version %d (now %d)", version, cur.Version)
			}
			n, err := tx.Q.UpdateHumanPolicy(ctx, db.UpdateHumanPolicyParams{RemindAfterMinutes: in.RemindAfterMinutes, RemindEveryMinutes: in.RemindEveryMinutes,
				MaxReminders: in.MaxReminders, EscalateAfterMinutes: in.EscalateAfterMinutes, EscalatePriority: in.EscalatePriority,
				EscalateAssignee: in.EscalateAssignee, RequesterEnded: in.RequesterEnded, HoldMinutes: in.HoldMinutes, SuccessorRole: in.SuccessorRole,
				WakeIdle: b2i(in.WakeIdle), RatePerRunHour: in.RatePerRunHour, RatePerSpaceHour: in.RatePerSpaceHour, ResponseInbox: in.ResponseInbox, UpdatedAt: tx.NowMicros(),
				UpdatedBy: actor.ID, ID: cur.ID, Version: cur.Version})
			if err != nil {
				return err
			}
			if n == 0 {
				return apperr.New(apperr.CodeConflict, "the policy changed concurrently")
			}
		}
		if err := svc.reschedule(ctx, tx, spaceID); err != nil {
			return err
		}
		if err := svc.changes.Record(ctx, tx, spaceID, "human_policy", id, "updated", 0); err != nil {
			return err
		}
		tx.AfterCommit(svc.Kick)
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: action, Target: audit.Target{Type: "human_policy", ID: id},
			Details: map[string]any{"space_id": spaceID, "action_type": actionType, "remind_after_minutes": in.RemindAfterMinutes,
				"escalate_after_minutes": in.EscalateAfterMinutes, "escalate_priority": in.EscalatePriority, "escalate_assignee": in.EscalateAssignee,
				"requester_ended": in.RequesterEnded, "successor_role": in.SuccessorRole, "wake_idle": in.WakeIdle,
				"rate_per_run_hour": in.RatePerRunHour, "rate_per_space_hour": in.RatePerSpaceHour, "response_inbox": in.ResponseInbox}})
		return err
	})
	if err != nil {
		return nil, err
	}
	r, err := getPolicy(ctx, svc.s.R(), spaceID, actionType)
	if err != nil {
		return nil, err
	}
	p := policyFromRow(r)
	return &p, nil
}

// DeletePolicy removes the policy of a type in a space (or the installation default); the next less specific one applies.
func (svc *Service) DeletePolicy(ctx context.Context, actor audit.Actor, spaceID, actionType string) error {
	err := svc.s.W().TxNamed(ctx, "human.policy_delete", func(tx *store.Tx) error {
		cur, err := getPolicy(ctx, tx.Q, spaceID, actionType)
		if errors.Is(err, sql.ErrNoRows) {
			return apperr.New(apperr.CodeNotFound, "no policy for %s here", actionType)
		} else if err != nil {
			return err
		}
		if _, err := tx.Q.DeleteHumanPolicy(ctx, cur.ID); err != nil {
			return err
		}
		if err := svc.reschedule(ctx, tx, spaceID); err != nil {
			return err
		}
		if err := svc.changes.Record(ctx, tx, spaceID, "human_policy", cur.ID, "deleted", 0); err != nil {
			return err
		}
		tx.AfterCommit(svc.Kick)
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "human_policy.deleted", Target: audit.Target{Type: "human_policy", ID: cur.ID},
			Details: map[string]any{"space_id": spaceID, "action_type": actionType}})
		return err
	})
	return err
}

type policyGetter interface {
	GetDefaultHumanPolicy(ctx context.Context, actionType string) (db.HumanPolicy, error)
	GetSpaceHumanPolicy(ctx context.Context, arg db.GetSpaceHumanPolicyParams) (db.HumanPolicy, error)
}

func getPolicy(ctx context.Context, q policyGetter, spaceID, actionType string) (db.HumanPolicy, error) {
	if spaceID == "" {
		return q.GetDefaultHumanPolicy(ctx, actionType)
	}
	return q.GetSpaceHumanPolicy(ctx, db.GetSpaceHumanPolicyParams{SpaceID: sql.NullString{String: spaceID, Valid: true}, ActionType: actionType})
}

// schedule computes an action's next reminder and escalation from its history and a policy. It is deterministic, so
// recomputing it after a policy change (or at start) is safe.
func schedule(r db.HumanAction, p PolicyFields) (remind, escalate sql.NullInt64) {
	minutes := func(base, m int64) sql.NullInt64 {
		return sql.NullInt64{Int64: base + m*int64(time.Minute/time.Microsecond), Valid: true}
	}
	switch {
	case p.RemindAfterMinutes == 0 || r.ReminderCount >= p.MaxReminders:
	case r.ReminderCount == 0:
		remind = minutes(r.CreatedAt, p.RemindAfterMinutes)
	case p.RemindEveryMinutes > 0 && r.RemindedAt.Valid:
		remind = minutes(r.RemindedAt.Int64, p.RemindEveryMinutes)
	}
	if !r.EscalatedAt.Valid && p.EscalateAfterMinutes > 0 && (p.EscalatePriority != "" || p.EscalateAssignee != "") {
		escalate = minutes(r.CreatedAt, p.EscalateAfterMinutes)
	}
	return remind, escalate
}

// reschedule recomputes the reminder and escalation times of pending actions in scope (a space, or every space for
// a default policy).
func (svc *Service) reschedule(ctx context.Context, tx *store.Tx, spaceID string) error {
	rows, err := tx.Q.ListPendingHumanActions(ctx, policyReapplyLimit)
	if err != nil {
		return err
	}
	cache := map[string]Policy{}
	for _, r := range rows {
		if spaceID != "" && r.SpaceID != spaceID {
			continue
		}
		k := r.SpaceID + "|" + policyKey(r.Type)
		p, ok := cache[k]
		if !ok {
			if p, err = policyFor(ctx, tx.Q, r.SpaceID, r.Type); err != nil {
				return err
			}
			cache[k] = p
		}
		rem, esc := schedule(r, p.PolicyFields)
		if rem == r.NextReminderAt && esc == r.EscalateAt {
			continue
		}
		if err := tx.Q.SetHumanSchedule(ctx, db.SetHumanScheduleParams{NextReminderAt: rem, EscalateAt: esc, ID: r.ID}); err != nil {
			return err
		}
		if err := svc.changes.Record(ctx, tx, r.SpaceID, "human_action", r.ID, "updated", r.Version); err != nil {
			return err
		}
	}
	return nil
}
