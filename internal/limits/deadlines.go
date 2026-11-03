package limits

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

func fromMicros(us int64) time.Time { return time.UnixMicro(us).UTC() }

// deadlineOf returns when a running task passes its space's deadline (zero: no deadline).
func (svc *Service) deadlineOf(ctx context.Context, cache map[string]int64, t db.ListRunningTopLevelTasksRow) time.Time {
	m, ok := cache[t.SpaceID]
	if !ok {
		m = svc.Value(ctx, nil, t.SpaceID, TaskDeadline)
		cache[t.SpaceID] = m
	}
	if m <= 0 || !t.StartedAt.Valid {
		return time.Time{}
	}
	return fromMicros(t.StartedAt.Int64).Add(time.Duration(m) * time.Minute)
}

// NextDeadline is the earliest time a running top-level task passes its deadline (zero: none). It is one of the jobs
// of central's single deadline timer.
func (svc *Service) NextDeadline(ctx context.Context) (time.Time, error) {
	rows, err := svc.s.R().ListRunningTopLevelTasks(ctx)
	if err != nil {
		return time.Time{}, err
	}
	cache := map[string]int64{}
	var next time.Time
	for _, t := range rows {
		if d := svc.deadlineOf(ctx, cache, t); !d.IsZero() && (next.IsZero() || d.Before(next)) {
			next = d
		}
	}
	return next, nil
}

// Deadlines reports running top-level tasks past their deadline to control and the operators, once per start of the
// task. A passed deadline never stops, fails or moves the task (SM-LIMIT-002: limits never kill work).
func (svc *Service) Deadlines(ctx context.Context) (int, error) {
	rows, err := svc.s.R().ListRunningTopLevelTasks(ctx)
	if err != nil {
		return 0, err
	}
	now := svc.clock.Now()
	cache := map[string]int64{}
	n := 0
	for _, t := range rows {
		d := svc.deadlineOf(ctx, cache, t)
		if d.IsZero() || now.Before(d) {
			continue
		}
		created := false
		err := svc.s.W().TxNamed(ctx, "limits.task_deadline", func(tx *store.Tx) error {
			k, err := tx.Q.InsertTaskDeadlineNotice(ctx, db.InsertTaskDeadlineNoticeParams{TaskID: t.ID, StartedAt: t.StartedAt.Int64, SpaceID: t.SpaceID, At: tx.NowMicros()})
			if err != nil || k == 0 {
				return err
			}
			created = true
			_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: audit.Actor{Type: audit.ActorSystem, ID: "limits"}, Action: "task.deadline_passed",
				Target: audit.Target{Type: "task", ID: t.ID}, Details: map[string]any{"space_id": t.SpaceID, "deadline_minutes": cache[t.SpaceID]}})
			return err
		})
		if err != nil {
			return n, err
		}
		if !created {
			continue
		}
		n++
		if svc.Notify != nil {
			body := fmt.Sprintf("Task %s (%q) has been running for more than %d minutes, the space's task deadline. SpaceMan does not stop it; decide whether it "+
				"should continue, be split or be cancelled.", t.ID, t.Title, cache[t.SpaceID])
			if err := svc.Notify(ctx, t.SpaceID, "Task deadline passed", body, "deadline:"+t.ID+":"+fmt.Sprint(t.StartedAt.Int64), true); err != nil {
				svc.log.Warn("notify a passed task deadline", slog.String("task.id", t.ID), slog.Any("error", err))
			}
		}
	}
	return n, nil
}
