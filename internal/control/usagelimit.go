package control

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/agenttools"
	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/human"
	"github.com/aurimasniekis/go-spaceman/internal/obs"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Dedup keys of the usage-limit notices: one "limited" and one "cleared" per run and limit window (its since).
func limitedKey(runID string, l *fleet.RunLimit) string {
	return fmt.Sprintf("usage-limit:%s:%d", runID, l.Since.UnixMicro())
}

func limitClearedKey(runID string, l *fleet.RunLimit) string {
	return fmt.Sprintf("usage-limit-cleared:%s:%d", runID, l.Since.UnixMicro())
}

// OnRunLimitTx tells control, inside the report transaction, when a run's usage limit opens and when it ends (fleet
// OnRunLimitTx, ADR-0079 decision 11): exactly one message per run and window each way, deduplicated on the run and the
// limit's since, and written only on that change, never per report. A "cleared" message goes out only for a window
// control was told about. Control's own limit is not announced to it: it couldn't act on it, and its notices wait for
// the clear anyway.
func (sv *Supervisor) OnRunLimitTx(ctx context.Context, tx *store.Tx, r *fleet.Run, ended, opened *fleet.RunLimit) error {
	if r.Kind == agenttools.KindControl {
		return nil
	}
	if ended != nil {
		told, err := sv.msgs.ControlNotifiedTx(ctx, tx, r.SpaceID, limitedKey(r.ID, ended))
		if err != nil {
			return err
		}
		if told {
			body := fmt.Sprintf("Run %s (%s, %s) is past its %s usage limit and can take notices again.%s", r.ID, r.Kind, r.Role, ended.Kind, limitTask(ctx, tx, r.ID))
			if _, err := sv.msgs.NotifyControlTx(ctx, tx, r.SpaceID, limitClearedKey(r.ID, ended), fmt.Sprintf("%s run usage limit cleared", r.Role), body); err != nil {
				return err
			}
		}
	}
	// Only a run kept alive at its limit is announced; one that ended with it is reported by its run-ended notice. A run
	// with an armed account switch isn't kept alive: control hears about it once, when it continues on the new account.
	if opened == nil || !fleet.Live(r.Observed) {
		return nil
	}
	if _, err := tx.Q.GetPendingAccountSwitchByRun(ctx, r.ID); err == nil {
		return nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	reset := "an unknown time (Claude Code didn't say)"
	if opened.ResetsAt != nil {
		reset = opened.ResetsAt.UTC().Format(time.RFC3339)
	}
	body := fmt.Sprintf("Run %s (%s, %s) reached its %s usage limit; it resets at %s.%s SpaceMan keeps the run alive and its harness continues "+
		"by itself after the reset. Until then it doesn't count against the space's run limit and gets no notices (messages to it wait). "+
		"Don't stop, replace or relaunch it: you get one more message when the limit clears.", r.ID, r.Kind, r.Role, opened.Kind, reset, limitTask(ctx, tx, r.ID))
	_, err := sv.msgs.NotifyControlTx(ctx, tx, r.SpaceID, limitedKey(r.ID, opened), fmt.Sprintf("%s run at its usage limit", r.Role), body)
	return err
}

// limitTask names the task assigned to the run, if any (" Its task …").
func limitTask(ctx context.Context, tx *store.Tx, runID string) string {
	id, err := tx.Q.GetTaskIDForRun(ctx, runID)
	if err != nil {
		return ""
	}
	t, err := tx.Q.GetTask(ctx, id)
	if err != nil {
		return ""
	}
	return fmt.Sprintf(" Its task %s (%s) is %s.", t.ID, t.Title, t.Status)
}

// OnLimitWaitTx tells control, inside the transaction of the change, when a run is parked at its usage limit, when it
// is resumed and when its wait is released (fleet OnLimitWaitTx, ADR-0079 decisions 4–6 and 11): one message per wait
// and change, deduplicated on the wait. A re-park of a resumed run (an unknown reset probed again) and the later
// resumes of the same chain are not announced again. Control's own wait isn't announced to it: a resumed control is
// told by its prompt, a fresh session's prompt included.
func (sv *Supervisor) OnLimitWaitTx(ctx context.Context, tx *store.Tx, ev fleet.LimitWaitEvent) error {
	w := ev.Wait
	if ev.Change == fleet.LimitWaitParked && ev.PoolExhausted != nil {
		if err := sv.tellPoolExhaustedTx(ctx, tx, w, ev.PoolExhausted); err != nil {
			return err
		}
	}
	if w.RunKind == agenttools.KindControl {
		return nil
	}
	task := ""
	if w.TaskID != "" {
		if t, err := tx.Q.GetTask(ctx, w.TaskID); err == nil {
			task = fmt.Sprintf(" Its task is %s (%s).", t.ID, t.Title)
		}
	}
	var key, subject, body string
	switch ev.Change {
	case fleet.LimitWaitParked:
		// An account switch's park is told with its resume (one message per switch, ADR-0090).
		if ev.Repark || ev.Switch != nil {
			return nil
		}
		if p := ev.PoolExhausted; p != nil {
			key, subject = "limit-wait:"+w.ID, fmt.Sprintf("%s run parked: every account of its pool is at its usage limit", w.Role)
			body = fmt.Sprintf("Run %s (%s, %s) is parked at its usage limit, and so is every account of its pool (%s): none could take it over. "+
				"Its process ended and its session is kept. SpaceMan resumes it as a new run of the same instance %s, choosing the account "+
				"then from the pool.%s Don't relaunch or reassign it; you get one message when it resumes. Its wait is %s (spaceman limits waits).",
				w.RunID, w.RunKind, w.Role, poolNames(ctx, tx, p.Accounts), resumeWhen(p, w.ResumeAt), task, w.ID)
			break
		}
		release := ""
		if w.ReleaseAt != nil {
			release = fmt.Sprintf(" If it hasn't resumed by %s, SpaceMan gives up on it and its task goes back to the queue, ready and unassigned.",
				w.ReleaseAt.UTC().Format(time.RFC3339))
		}
		key, subject = "limit-wait:"+w.ID, fmt.Sprintf("%s run parked at its usage limit", w.Role)
		body = fmt.Sprintf("Run %s (%s, %s) is parked at its usage limit: its process ended and its session is kept. SpaceMan resumes it as a new run "+
			"of the same instance at %s (or when the account's hold lets it through).%s%s Don't relaunch or reassign it; you get one message when it "+
			"resumes. Its wait is %s (spaceman limits waits).", w.RunID, w.RunKind, w.Role, w.ResumeAt.UTC().Format(time.RFC3339), task, release, w.ID)
	case fleet.LimitWaitResumed:
		if sw := ev.Switch; sw != nil {
			key, subject = "account-switch:"+sw.ID, fmt.Sprintf("%s run moved to another account at its usage limit", w.Role)
			reset := "an unknown time"
			if sw.LimitResetsAt != nil {
				reset = sw.LimitResetsAt.UTC().Format(time.RFC3339)
			}
			how := "with its session"
			if ev.Fresh {
				how = "in a fresh session (its session could not be carried over), with its original prompt"
			}
			body = fmt.Sprintf("Run %s (%s, %s) reached its %s usage limit on account %s (it resets at %s); SpaceMan continued it as run %s on "+
				"account %s, on the same instance, %s.%s Don't relaunch or reassign it. The switch is %s.",
				w.RunID, w.RunKind, w.Role, sw.LimitKind, accountName(ctx, tx, sw.FromAccountID), reset, ev.NewRunID,
				accountName(ctx, tx, sw.ToAccountID), how, taskNow(task, ev.NewRunID), sw.ID)
			break
		}
		if !ev.FirstResume && !ev.Fresh {
			return nil
		}
		key, subject = "limit-wait-resumed:"+w.ID, fmt.Sprintf("%s run resumed after its usage limit", w.Role)
		body = fmt.Sprintf("Run %s (%s, %s), parked at its usage limit, was resumed as run %s on the same instance with its session.%s",
			w.RunID, w.RunKind, w.Role, ev.NewRunID, taskNow(task, ev.NewRunID))
		if ev.Fresh {
			body = fmt.Sprintf("Run %s (%s, %s), parked at its usage limit, was resumed as run %s in a fresh session: the space moved to another node "+
				"since, so its conversation could not be carried over. It was given its original prompt again and works from the workspace's state.%s",
				w.RunID, w.RunKind, w.Role, ev.NewRunID, taskNow(task, ev.NewRunID))
		}
	case fleet.LimitWaitReleased:
		key, subject = "limit-wait-released:"+w.ID, fmt.Sprintf("%s run given up at its usage limit", w.Role)
		body = fmt.Sprintf("Run %s (%s, %s) waited at its usage limit since %s and was not resumed; SpaceMan gave up on it (%s).%s",
			w.RunID, w.RunKind, w.Role, w.CreatedAt.UTC().Format(time.RFC3339), w.LastError, task)
		switch ev.TaskStatus {
		case "ready":
			body += " The task is back in the queue, ready and unassigned: launch a worker for it when an account is free."
		case "review":
			body += " The task stays in review, waiting for acceptance; only its assignee was cleared."
		}
	case fleet.LimitWaitResumeFailed:
		key, subject = "limit-wait-resume-failed:"+w.ID, fmt.Sprintf("%s run's resume failed; restarted fresh", w.Role)
		body = fmt.Sprintf("Run %s (%s, %s), resumed after its usage limit, ended before its first turn: resuming its session failed. SpaceMan "+
			"starts it again now in a fresh session with its original prompt, on the same instance (only once; a fresh start that fails is "+
			"reported as usual).%s", w.RunID, w.RunKind, w.Role, task)
	default:
		return nil
	}
	_, err := sv.msgs.NotifyControlTx(ctx, tx, w.SpaceID, key, subject, body)
	return err
}

// accountName names an account for control (its name, never a secret); its ID when it is gone.
func accountName(ctx context.Context, tx *store.Tx, id string) string {
	if a, err := tx.Q.GetAccount(ctx, id); err == nil {
		return fmt.Sprintf("%q", a.Name)
	}
	return id
}

func taskNow(task, runID string) string {
	if task == "" {
		return ""
	}
	return task + " It now points at run " + runID + "."
}

// poolNames names a pool's accounts for a message (never a secret).
func poolNames(ctx context.Context, tx *store.Tx, ids []string) string {
	names := make([]string, 0, len(ids))
	for _, id := range ids {
		names = append(names, accountName(ctx, tx, id))
	}
	return strings.Join(names, ", ")
}

// resumeWhen words when a pool-exhausted wait resumes: the pool's earliest reset, or the probe time when none is known.
func resumeWhen(p *fleet.PoolExhausted, at time.Time) string {
	if p.ResetsAt != nil {
		return fmt.Sprintf("at %s, the earliest reset in the pool", at.UTC().Format(time.RFC3339))
	}
	return fmt.Sprintf("at %s (no reset in the pool is known, so it tries again then)", at.UTC().Format(time.RFC3339))
}

// poolWindow is the width the pool's earliest reset is rounded down to in the dedup key. A hold's reset only ever moves
// later (a report of a later reset replaces it), and a status line can restate the same window a few seconds apart, so
// the exact second would raise a notice per restatement.
const poolWindow = 10 * time.Minute

// PoolExhaustedDedup is the dedup key of the people notice of a pool: one human action per space, harness, role and limit
// window (the pool's earliest reset, rounded down to poolWindow; with none known, the day). The role is part of it
// because two roles' pools on one harness can be exhausted at the same reset and each notice names its own pool.
func PoolExhaustedDedup(spaceID string, p *fleet.PoolExhausted, now time.Time) string {
	window := "day-" + now.UTC().Format("20060102")
	if p.ResetsAt != nil {
		window = fmt.Sprintf("%d", p.ResetsAt.Truncate(poolWindow).Unix())
	}
	return fmt.Sprintf("account-pool-exhausted:%s:%s:%s:%s", spaceID, p.Harness, p.Role, window)
}

// tellPoolExhaustedTx records the people notice of a pool whose accounts are all at their usage limit, in the park's
// transaction, and raises it once that commits (a human action is written in a transaction of its own, never inside
// this one). It is one informational action per space, role and window, deduplicated on its reserved key, so a second
// park in the same window only finds it. People, and so Telegram, hear about the pool only here: a switch or a single
// limited account never raises one. The row stays until the action exists: a crash after the commit, a space at its
// open-action cap (RateLimited) or an archived one (Precondition) leaves it for TellPoolExhausted, which the deadline
// loop runs again later, so the notice is late in those cases, never lost.
func (sv *Supervisor) tellPoolExhaustedTx(ctx context.Context, tx *store.Tx, w fleet.LimitWait, p *fleet.PoolExhausted) error {
	space := w.SpaceID
	if sp, err := tx.Q.GetSpace(ctx, w.SpaceID); err == nil {
		space = sp.Name
	}
	names := poolNames(ctx, tx, p.Accounts)
	now := tx.Now
	reset := "no known time"
	if p.ResetsAt != nil {
		reset = fmt.Sprintf("%s (in about %s)", p.ResetsAt.UTC().Format(time.RFC3339), approxDuration(p.ResetsAt.Sub(now)))
	}
	title := fmt.Sprintf("Every %s account of the %s pool is at its usage limit", p.Harness, p.Role)
	body := fmt.Sprintf("In %s, every account of the pool of role %s (%s) has reached its usage limit, so a run that hits a limit cannot move to "+
		"another account. The earliest reset in the pool is at %s. Runs parked on it resume then, on the account that is free by that time.\n\nNothing "+
		"is needed if the work can wait. To keep it going sooner, bind or enable another account for the pool. You get one such notice per space, "+
		"role and limit window.", space, p.Role, names, reset)
	key := PoolExhaustedDedup(w.SpaceID, p, now)
	if err := tx.Q.InsertPoolExhaustedNotice(ctx, db.InsertPoolExhaustedNoticeParams{SpaceID: w.SpaceID, DedupKey: key, WaitID: w.ID, Title: title,
		Body: body, CreatedAt: tx.NowMicros()}); err != nil {
		return err
	}
	tx.AfterCommit(func() {
		if _, err := sv.TellPoolExhausted(context.WithoutCancel(ctx)); err != nil {
			sv.log.Info("pool exhausted people notice left for the next sweep", slog.String("limit_wait.id", w.ID), obs.Err(err))
		}
	})
	return nil
}

// poolNoticePage bounds the notices one sweep reads.
const poolNoticePage = 200

// TellPoolExhausted raises the recorded pool-exhausted notices that have no human action yet and deletes each row once
// its action exists (created now, or found by its dedup key). A space at its open-action cap or archived since is
// skipped and tried again on a later round, as TellLongHolds does: the cap frees up when a person answers an action.
// It returns the number of actions created.
func (sv *Supervisor) TellPoolExhausted(ctx context.Context) (int, error) {
	rows, err := sv.fl.PoolExhaustedNotices(ctx, poolNoticePage)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range rows {
		_, created, err := sv.human.Create(ctx, audit.System, r.SpaceID, human.CreateInput{Type: human.TypeMessage, Priority: "normal", Title: r.Title,
			Body: r.Body, Request: []byte(`{"requires_ack":false}`), Requester: human.Requester{Kind: "system", Label: "Usage limits"},
			CorrelationID: r.WaitID, DedupKey: r.DedupKey, Internal: true})
		switch apperr.CodeOf(err) {
		case "":
		case apperr.CodeRateLimited, apperr.CodePrecondition:
			sv.log.Info("pool exhausted people notice postponed", slog.String("limit_wait.id", r.WaitID), obs.Err(err))
			continue
		default:
			return n, err
		}
		if err := sv.fl.DeletePoolExhaustedNotice(ctx, r.SpaceID, r.DedupKey); err != nil {
			return n, err
		}
		if created {
			n++
		}
	}
	return n, nil
}
