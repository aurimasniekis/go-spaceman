package human

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"slices"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/obs"
	"github.com/aurimasniekis/go-spaceman/internal/outbox"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Deadline is another job driven by the same timer (enforced approvals expire this way): Next returns its earliest
// due time (zero: none) and Due does what is due.
type Deadline struct {
	Name string
	Next func(ctx context.Context) (time.Time, error)
	Due  func(ctx context.Context) (int, error)
}

// idleWait bounds the wait when nothing is scheduled; any change kicks the loop earlier.
const idleWait = time.Hour

// Kick asks the deadline loop to look again (after a commit that may have changed the earliest deadline, a run that
// ended or turned idle). It never blocks.
func (svc *Service) Kick() {
	select {
	case svc.wake <- struct{}{}:
	default:
	}
}

// Run drives expiry, reminders, escalation and stranded notifications with one timer set to the earliest deadline,
// woken early by Kick (SM-HUM-011). Nothing polls per action or per space. The first round runs at once, so
// notifications stranded while central was down are routed on start.
func (svc *Service) Run(ctx context.Context, extra ...Deadline) {
	t := time.NewTimer(0)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-svc.wake:
			if !t.Stop() {
				select {
				case <-t.C:
				default:
				}
			}
		case <-t.C:
		}
		svc.Tick(ctx, extra...)
		t.Reset(svc.untilNext(ctx, extra))
	}
}

func (svc *Service) logger() *slog.Logger {
	if svc.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return svc.Log
}

// Tick does everything that is due now: expiry, reminders, escalation, routing of stranded notifications, and the
// extra deadlines.
func (svc *Service) Tick(ctx context.Context, extra ...Deadline) {
	log := svc.logger()
	jobs := []Deadline{{Name: "expire", Due: svc.Expire}, {Name: "remind", Due: svc.Remind}, {Name: "escalate", Due: svc.Escalate},
		{Name: "reroute", Due: svc.reroute}}
	for _, j := range append(jobs, extra...) {
		n, err := j.Due(ctx)
		if err != nil && ctx.Err() == nil {
			log.Warn("human deadline", slog.String("job", j.Name), obs.Err(err))
		} else if n > 0 {
			log.Info("human deadline", slog.String("job", j.Name), slog.Int("count", n))
		}
	}
}

// NextDeadline is the earliest expiry, reminder, escalation or notification hold; zero when nothing is scheduled.
func (svc *Service) NextDeadline(ctx context.Context) (time.Time, error) {
	q := svc.s.R()
	next := int64(0)
	for _, f := range []func(context.Context) (int64, error){q.NextHumanExpiry, q.NextHumanReminder, q.NextHumanEscalation, q.NextHumanHold} {
		t, err := f(ctx)
		if err != nil {
			return time.Time{}, err
		}
		if t > 0 && (next == 0 || t < next) {
			next = t
		}
	}
	if next == 0 {
		return time.Time{}, nil
	}
	return time.UnixMicro(next).UTC(), nil
}

func (svc *Service) untilNext(ctx context.Context, extra []Deadline) time.Duration {
	next, err := svc.NextDeadline(ctx)
	if err != nil {
		return time.Minute
	}
	for _, d := range extra {
		if d.Next == nil {
			continue
		}
		t, err := d.Next(ctx)
		if err != nil {
			return time.Minute
		}
		if !t.IsZero() && (next.IsZero() || t.Before(next)) {
			next = t
		}
	}
	if next.IsZero() {
		return idleWait
	}
	return min(max(next.Sub(svc.clock.Now()), 0), idleWait)
}

// Remind re-surfaces pending actions whose reminder is due: the reminder count and time change (shown in Needs you),
// the change feed and an outbound event say so, and the next reminder is scheduled per policy. The action's version
// and order do not change, so a person in the middle of responding is not interrupted.
func (svc *Service) Remind(ctx context.Context) (int, error) {
	rows, err := svc.s.R().DueHumanReminders(ctx, db.DueHumanRemindersParams{NextReminderAt: sql.NullInt64{Int64: svc.clock.Now().UnixMicro(), Valid: true}, Limit: 500})
	if err != nil {
		return 0, err
	}
	n := 0
	var errs []error
	for _, r := range rows {
		err := svc.s.W().TxNamed(ctx, "human.remind", func(tx *store.Tx) error {
			cur, err := tx.Q.GetHumanAction(ctx, r.ID)
			if err != nil || !pending(cur.Status) || !cur.NextReminderAt.Valid || cur.NextReminderAt.Int64 > tx.NowMicros() {
				return ignoreMissing(err)
			}
			p, err := policyFor(ctx, tx.Q, cur.SpaceID, cur.Type)
			if err != nil {
				return err
			}
			after := cur
			after.ReminderCount++
			after.RemindedAt = sql.NullInt64{Int64: tx.NowMicros(), Valid: true}
			next, _ := schedule(after, p.PolicyFields)
			if k, err := tx.Q.RemindHumanAction(ctx, db.RemindHumanActionParams{Now: after.RemindedAt, NextReminderAt: next, ID: cur.ID, ReminderCount: cur.ReminderCount}); err != nil || k == 0 {
				return err
			}
			if err := svc.changes.Record(ctx, tx, cur.SpaceID, "human_action", cur.ID, "updated", cur.Version); err != nil {
				return err
			}
			if err := svc.publish(ctx, tx, outbox.HumanActionReminded{HumanAction: svc.meta(cur, cur.Status, ""), ReminderCount: after.ReminderCount}, cur, ""); err != nil {
				return err
			}
			n++
			_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: audit.System, Action: "human_action.reminded", Target: audit.Target{Type: "human_action", ID: cur.ID},
				Details: map[string]any{"space_id": cur.SpaceID, "reminder": after.ReminderCount, "policy": p.Source}})
			return err
		})
		if err != nil {
			errs = append(errs, err) // one failing row never starves the others
		}
	}
	return n, errors.Join(errs...)
}

var priorityRank = map[string]int{"low": 0, "normal": 1, "high": 2, "urgent": 3}

// Escalate applies the escalation policy to pending actions nobody resolved in time: the priority is raised (never
// lowered) and/or the action is assigned to the escalation assignee. It is marked escalated and audited; its place in
// the queue does not change (SM-INV-010 applies to people's ordering too).
func (svc *Service) Escalate(ctx context.Context) (int, error) {
	rows, err := svc.s.R().DueHumanEscalations(ctx, db.DueHumanEscalationsParams{EscalateAt: sql.NullInt64{Int64: svc.clock.Now().UnixMicro(), Valid: true}, Limit: 500})
	if err != nil {
		return 0, err
	}
	n := 0
	var errs []error
	for _, r := range rows {
		err := svc.s.W().TxNamed(ctx, "human.escalate", func(tx *store.Tx) error {
			cur, err := tx.Q.GetHumanAction(ctx, r.ID)
			if err != nil || !pending(cur.Status) || !cur.EscalateAt.Valid || cur.EscalateAt.Int64 > tx.NowMicros() {
				return ignoreMissing(err)
			}
			p, err := policyFor(ctx, tx.Q, cur.SpaceID, cur.Type)
			if err != nil {
				return err
			}
			prio, assignee, status := cur.Priority, cur.Assignee, cur.Status
			if p.EscalatePriority != "" && priorityRank[p.EscalatePriority] > priorityRank[prio] {
				prio = p.EscalatePriority
			}
			skipped := ""
			if p.EscalateAssignee != "" && p.EscalateAssignee != cur.Assignee {
				if _, err := tx.Q.GetUser(ctx, p.EscalateAssignee); err == nil {
					assignee, status = p.EscalateAssignee, InProgress
				} else {
					skipped = "the escalation assignee is not a user"
				}
			}
			k, err := tx.Q.EscalateHumanAction(ctx, db.EscalateHumanActionParams{Priority: prio, Assignee: assignee, Status: status,
				Now: sql.NullInt64{Int64: tx.NowMicros(), Valid: true}, EscalatedFrom: cur.Priority, ID: cur.ID, Version: cur.Version})
			if err != nil || k == 0 {
				return err
			}
			if err := svc.changes.Record(ctx, tx, cur.SpaceID, "human_action", cur.ID, "updated", cur.Version+1); err != nil {
				return err
			}
			after := cur
			after.Priority = prio
			if err := svc.publish(ctx, tx, outbox.HumanActionEscalated{HumanAction: svc.meta(after, status, ""), FromPriority: cur.Priority,
				Reassigned: assignee != cur.Assignee}, cur, ""); err != nil {
				return err
			}
			n++
			_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: audit.System, Action: "human_action.escalated", Target: audit.Target{Type: "human_action", ID: cur.ID},
				Details: map[string]any{"space_id": cur.SpaceID, "from_priority": cur.Priority, "to_priority": prio, "from_assignee": cur.Assignee,
					"to_assignee": assignee, "skipped": skipped, "policy": p.Source}})
			return err
		})
		if err != nil {
			errs = append(errs, err) // one failing row never starves the others
		}
	}
	return n, errors.Join(errs...)
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
		if errors.As(err, &ae) && slices.Contains([]apperr.Code{apperr.CodeConflict, apperr.CodePrecondition, apperr.CodeNotFound}, ae.Code) {
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

// Recover recomputes the reminder and escalation times of every pending action from the policies in force (actions
// created before reminders existed, or under a policy that changed while central was stopped). It is idempotent.
func (svc *Service) Recover(ctx context.Context) error {
	return svc.s.W().TxNamed(ctx, "human.recover", func(tx *store.Tx) error { return svc.reschedule(ctx, tx, "") })
}
