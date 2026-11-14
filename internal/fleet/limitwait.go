package fleet

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/obs"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Parking and resuming (ADR-0079 decisions 4–6, UL-7). A run that outlives the keep-alive at its usage limit is stopped
// and parked; one that ends or is lost while at its limit is parked too. Parking writes a durable limit wait in the
// transaction that decides it. The wait's deadline job resumes it at the reset (or at the hold's probe) as a new run of
// the same instance under the space's current lease, with --resume of its session, and points its task at the new
// run. A worker's wait that is not resumed by its release time gives its task back to the queue; control's never is.
//
// The job is one deadline on central's shared timer. A round reads only what is due, writes only for those rows, and
// resumes at most limitResumeBatch waits; when more are due, the next round waits limitResumeSpacing, so a reset that
// frees many runs at once doesn't start them all at once. A wait that cannot resume yet (node away, space paused or
// full, account still held) moves its resume time on with a fixed reason, so it never makes the timer spin; the node's
// hello and the space's resume bring such waits forward at once.

// Wait states.
const (
	WaitWaiting   = "waiting"
	WaitResuming  = "resuming"
	WaitResumed   = "resumed"
	WaitReleased  = "released"
	WaitCancelled = "cancelled"
)

// Fixed reasons a wait records in last_error (never raw error text).
const (
	WaitNodeOffline     = "node_offline"
	WaitSpacePaused     = "space_paused"
	WaitSpaceRelocating = "space_relocating"
	WaitSpaceFull       = "space_full"
	WaitAccountHeld     = "account_held"
	WaitRunEnding       = "run_not_ended"
	WaitLaunchFailed    = "launch_failed"
	WaitInterrupted     = "interrupted"
	WaitSuperseded      = "superseded"
	WaitTaskMoved       = "task_moved"
	WaitSpaceArchived   = "space_archived"
	WaitMaxWait         = "max_wait"
	WaitReleasedByOp    = "operator"
)

const (
	// DefaultLimitMaxWait is how long a parked worker waits before its task is released (Q2: 7 days).
	DefaultLimitMaxWait = 7 * 24 * time.Hour
	// limitResumeSlack is how long after a known reset a parked run resumes.
	limitResumeSlack = 2 * time.Minute
	// limitGatewayKeepAlive is how long a gateway run whose reset is unknown is kept alive before it is parked.
	limitGatewayKeepAlive = 30 * time.Minute
	// limitResumeBatch bounds the resumes per round; limitResumeSpacing is the pause before the next round when more are due.
	limitResumeBatch   = 5
	limitResumeSpacing = 10 * time.Second
	// limitWaitBatch bounds the releases, parks and recoveries per round.
	limitWaitBatch = 50
	// Retry delays of a wait that couldn't resume yet.
	limitRetryNode   = 15 * time.Minute
	limitRetrySoon   = 2 * time.Minute
	limitRetryPaused = time.Hour
	limitRetryFailed = 10 * time.Minute
	// limitResumingStale is how long a wait may stay resuming before it is taken for stuck (a resume takes seconds).
	limitResumingStale = 10 * time.Minute
)

// LimitWaitActor resumes, releases and parks runs at their usage limits.
var LimitWaitActor = audit.Actor{Type: audit.ActorSystem, ID: "usage-limit"}

// LimitWaitEvent is a change of a limit wait that control may need to hear about (Options.OnLimitWaitTx).
type LimitWaitEvent struct {
	Wait LimitWait
	// Change is parked, resumed or released.
	Change string
	// Repark is a park of a run that a wait had resumed: the same wait goes on (its release time and attempts carry
	// forward), so control isn't told again.
	Repark bool
	// NewRunID is the run a resume started; Fresh says it started without the session (the space moved node since).
	NewRunID string
	Fresh    bool
	// FirstResume is the first resume of the wait's chain (attempts was 0), the one control hears about.
	FirstResume bool
	// TaskStatus is, on a release, the task's status after it was given back (ready, or review: only unassigned); ""
	// when the task was not the run's any more and was left alone.
	TaskStatus string
}

// Limit-wait changes.
const (
	LimitWaitParked   = "parked"
	LimitWaitResumed  = "resumed"
	LimitWaitReleased = "released"
	// LimitWaitResumeFailed is a wait written for a resumed run whose --resume failed before its first turn: it starts
	// again in a fresh session.
	LimitWaitResumeFailed = "resume_failed"
)

// limitWaitJob is the job's in-memory state.
type limitWaitJob struct {
	mu sync.Mutex
	// started is when this process started: a wait resuming since before then was left by a stopped central.
	started time.Time
	// recovered is set after the first round recovered such waits; recoverAt is when stuck resumes are looked for again.
	recovered bool
	recoverAt time.Time
	// spacedUntil holds the next resume round back after one that hit limitResumeBatch.
	spacedUntil time.Time
}

// errResumeAborted ends a resume's launch transaction without a run: the wait's task moved meanwhile.
var errResumeAborted = errors.New("the wait's task moved")

// parkKind reports whether runs of a kind are parked and resumed. KB support and dreamer runs are started on demand
// by their own schedulers, so they are neither stopped at the keep-alive nor brought back.
func parkKind(kind string) bool { return kind == "control" || kind == "worker" || kind == "diagnostic" }

func gatewayLimit(kind string) bool {
	return kind == proto.LimitGatewayQuota || kind == proto.LimitGatewayBudget
}

func (svc *Service) ulSettings(ctx context.Context, q *db.Queries, spaceID string) UsageLimitSettings {
	ul := UsageLimitSettings{KeepAliveMax: DefaultLimitKeepAliveMax, MaxWait: DefaultLimitMaxWait, NotifyAfter: DefaultLimitNotifyAfter}
	if svc.opts.UsageLimitSettings != nil {
		ul = svc.opts.UsageLimitSettings(ctx, q, spaceID)
	}
	if ul.KeepAliveMax <= 0 {
		ul.KeepAliveMax = DefaultLimitKeepAliveMax
	}
	if ul.MaxWait <= 0 {
		ul.MaxWait = DefaultLimitMaxWait
	}
	return ul
}

// parkAt is when a live run at its limit is parked (ADR-0079 decision 4): at once when its known reset is further away
// than the keep-alive, after 30 minutes for a gateway limit with no reset, otherwise when it has waited the keep-alive.
func parkAt(r db.Run, keepAliveMax time.Duration) time.Time {
	since := time.UnixMicro(r.UpdatedAt)
	if r.LimitSince.Valid {
		since = time.UnixMicro(r.LimitSince.Int64)
	}
	switch {
	case r.LimitResetsAt.Valid && time.UnixMicro(r.LimitResetsAt.Int64).Sub(since) > keepAliveMax:
		return since
	case !r.LimitResetsAt.Valid && gatewayLimit(r.LimitKind):
		return since.Add(limitGatewayKeepAlive)
	}
	return since.Add(keepAliveMax)
}

// waitResumeAt is when a new wait resumes: after the known reset; else at the hold's probe; else with backoff from 1 h,
// doubling with each attempt, capped at the keep-alive. A gateway limit whose hold is no longer open resumes now: the
// gateway's hold is opened with the limit, so it was cleared (Harbor Pilot recovered, UL-8). Never before now.
func waitResumeAt(now time.Time, lim limitState, hold *db.UsageLimit, attempts int64, keepAliveMax time.Duration) time.Time {
	var at time.Time
	switch {
	case lim.resets.Valid:
		at = time.UnixMicro(lim.resets.Int64).Add(limitResumeSlack)
	case hold == nil && gatewayLimit(lim.kind):
		at = now
	case hold != nil && hold.ResetsAt.Valid:
		at = time.UnixMicro(hold.ResetsAt.Int64).Add(limitResumeSlack)
	case hold != nil && hold.CheckAt.Valid:
		at = time.UnixMicro(hold.CheckAt.Int64)
	default:
		d := limitProbeFirst
		for i := int64(0); i < attempts && d < keepAliveMax; i++ {
			d *= 2
		}
		at = now.Add(min(d, max(keepAliveMax, limitProbeFirst)))
	}
	if at.Before(now) {
		return now
	}
	return at
}

// parkTx writes the limit wait of a run that ends, or is stopped, at its usage limit, in the transaction that decides
// it; why is recorded in the audit. It writes nothing for a run of a kind that isn't parked, a run without a limit, or
// one that already has a wait. A run that a wait had resumed carries that wait's release time and attempts forward, so
// the maximum wait and the backoff don't start again; control isn't told about such a re-park. A wait another run of
// the same instance left waiting is superseded.
func (svc *Service) parkTx(ctx context.Context, tx *store.Tx, r db.Run, lim limitState, why string) (*db.LimitWait, error) {
	if !parkKind(r.Kind) || lim.kind == "" {
		return nil, nil
	}
	if _, err := tx.Q.GetLimitWaitByRun(ctx, r.ID); err == nil {
		return nil, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if old, err := tx.Q.GetActiveLimitWait(ctx, db.GetActiveLimitWaitParams{SpaceID: r.SpaceID, InstanceID: r.InstanceID}); err == nil {
		if err := svc.settleWaitTx(ctx, tx, old, WaitCancelled, WaitSuperseded, LimitWaitActor); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	ul := svc.ulSettings(ctx, tx.Q, r.SpaceID)
	now := tx.Now
	w := db.LimitWait{ID: ids.New(ids.LimitWait), SpaceID: r.SpaceID, RunID: r.ID, RunKind: r.Kind, Role: r.Role, NodeID: r.NodeID,
		InstanceID: r.InstanceID, Harness: r.Harness, LaunchJson: r.LaunchJson, State: WaitWaiting, Version: 1, CreatedAt: now.UnixMicro(), UpdatedAt: now.UnixMicro()}
	prev, err := tx.Q.GetLimitWaitResumedInto(ctx, db.GetLimitWaitResumedIntoParams{SpaceID: r.SpaceID, NewRunID: r.ID})
	repark := err == nil
	switch {
	case repark:
		// The task isn't carried: the lookup below finds it only while it is still this run's.
		w.Attempts, w.ReleaseAt = prev.Attempts, prev.ReleaseAt
		// The resumed run was launched with the resume note: a later fresh session needs the original prompt.
		var spec, first LaunchSpec
		if json.Unmarshal([]byte(r.LaunchJson), &spec) == nil && json.Unmarshal([]byte(prev.LaunchJson), &first) == nil {
			spec.Prompt = first.Prompt
			if b, err := json.Marshal(spec); err == nil {
				w.LaunchJson = string(b)
			}
		}
	case !errors.Is(err, sql.ErrNoRows):
		return nil, err
	}
	if r.Kind != "control" {
		if !w.ReleaseAt.Valid {
			w.ReleaseAt = sql.NullInt64{Int64: now.Add(ul.MaxWait).UnixMicro(), Valid: true}
		}
		if w.TaskID, err = activeTaskOf(ctx, tx, r.ID); err != nil {
			return nil, err
		}
	}
	if r.Turn > 0 {
		w.SessionID = r.SessionID // a session that never finished a turn has nothing to resume
	}
	var hold *db.UsageLimit
	if scope, account, gw, ok := limitScope(r, lim.kind); ok {
		if u, err := tx.Q.GetOpenUsageLimit(ctx, db.GetOpenUsageLimitParams{SpaceID: r.SpaceID, Scope: scope, AccountID: account, GatewayRef: gw, Kind: lim.kind}); err == nil {
			hold, w.UsageLimitID = &u, u.ID
		} else if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
	}
	w.ResumeAt = waitResumeAt(now, lim, hold, w.Attempts, ul.KeepAliveMax).UnixMicro()
	if err := tx.Q.InsertLimitWait(ctx, db.InsertLimitWaitParams{ID: w.ID, SpaceID: w.SpaceID, RunID: w.RunID, TaskID: w.TaskID, RunKind: w.RunKind,
		Role: w.Role, NodeID: w.NodeID, InstanceID: w.InstanceID, Harness: w.Harness, SessionID: w.SessionID, LaunchJson: w.LaunchJson,
		UsageLimitID: w.UsageLimitID, ResumeAt: w.ResumeAt, ReleaseAt: w.ReleaseAt, Attempts: w.Attempts, CreatedAt: w.CreatedAt, UpdatedAt: w.UpdatedAt}); err != nil {
		return nil, err
	}
	if err := svc.record(ctx, tx, w.SpaceID, "limit_wait", w.ID, "created", 1); err != nil {
		return nil, err
	}
	if _, err := svc.audit.Append(ctx, tx, audit.Event{Actor: LimitWaitActor, Action: "limit_wait.parked", Target: audit.Target{Type: "limit_wait", ID: w.ID},
		Details: map[string]any{"space_id": w.SpaceID, "run_id": r.ID, "kind": lim.kind, "why": why, "task_id": w.TaskID, "repark": repark,
			"resume_at": time.UnixMicro(w.ResumeAt).UTC()}}); err != nil {
		return nil, err
	}
	if svc.opts.OnLimitWaitTx != nil {
		if err := svc.opts.OnLimitWaitTx(ctx, tx, LimitWaitEvent{Wait: limitWaitFromRow(w), Change: LimitWaitParked, Repark: repark}); err != nil {
			return nil, err
		}
	}
	tx.AfterCommit(svc.wakeDeadlines)
	return &w, nil
}

// activeTaskOf returns the task assigned to a run that is still being worked on (running, blocked or in review), or "".
func activeTaskOf(ctx context.Context, tx *store.Tx, runID string) (string, error) {
	id, err := tx.Q.GetTaskIDForRun(ctx, runID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	} else if err != nil {
		return "", err
	}
	t, err := tx.Q.GetTask(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	} else if err != nil {
		return "", err
	}
	if t.AssigneeRun == runID && (t.Status == "running" || t.Status == "blocked" || t.Status == "review") {
		return t.ID, nil
	}
	return "", nil
}

// resumeFailedTx handles a resumed run that ended (failed, or exited) before its first turn: its --resume didn't work
// (a session file gone or unreadable). It writes a new wait for the run that starts it again at once in a fresh
// session with its original prompt, and control is told once. Only a resume that used a session gets this: a fresh
// start that fails is an ordinary failure, so a wait gets at most one fresh retry.
func (svc *Service) resumeFailedTx(ctx context.Context, tx *store.Tx, r db.Run) error {
	prev, err := tx.Q.GetLimitWaitResumedInto(ctx, db.GetLimitWaitResumedIntoParams{SpaceID: r.SpaceID, NewRunID: r.ID})
	if errors.Is(err, sql.ErrNoRows) || err == nil && prev.SessionID == "" {
		return nil
	} else if err != nil {
		return err
	}
	if _, err := tx.Q.GetActiveLimitWait(ctx, db.GetActiveLimitWaitParams{SpaceID: r.SpaceID, InstanceID: r.InstanceID}); err == nil {
		return nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	now := tx.Now.UnixMicro()
	w := db.LimitWait{ID: ids.New(ids.LimitWait), SpaceID: r.SpaceID, RunID: r.ID, RunKind: r.Kind, Role: r.Role, NodeID: r.NodeID,
		InstanceID: r.InstanceID, Harness: r.Harness, LaunchJson: prev.LaunchJson, UsageLimitID: prev.UsageLimitID, ResumeAt: now,
		ReleaseAt: prev.ReleaseAt, State: WaitWaiting, Attempts: prev.Attempts, Version: 1, CreatedAt: now, UpdatedAt: now}
	if r.Kind != "control" {
		if w.TaskID, err = activeTaskOf(ctx, tx, r.ID); err != nil {
			return err
		}
	}
	if err := tx.Q.InsertLimitWait(ctx, db.InsertLimitWaitParams{ID: w.ID, SpaceID: w.SpaceID, RunID: w.RunID, TaskID: w.TaskID, RunKind: w.RunKind,
		Role: w.Role, NodeID: w.NodeID, InstanceID: w.InstanceID, Harness: w.Harness, LaunchJson: w.LaunchJson, UsageLimitID: w.UsageLimitID,
		ResumeAt: w.ResumeAt, ReleaseAt: w.ReleaseAt, Attempts: w.Attempts, CreatedAt: w.CreatedAt, UpdatedAt: w.UpdatedAt}); err != nil {
		return err
	}
	if err := svc.record(ctx, tx, w.SpaceID, "limit_wait", w.ID, "created", 1); err != nil {
		return err
	}
	if _, err := svc.audit.Append(ctx, tx, audit.Event{Actor: LimitWaitActor, Action: "limit_wait.parked", Target: audit.Target{Type: "limit_wait", ID: w.ID},
		Details: map[string]any{"space_id": w.SpaceID, "run_id": r.ID, "why": "resume_failed", "task_id": w.TaskID, "previous_wait": prev.ID}}); err != nil {
		return err
	}
	if svc.opts.OnLimitWaitTx != nil {
		if err := svc.opts.OnLimitWaitTx(ctx, tx, LimitWaitEvent{Wait: limitWaitFromRow(w), Change: LimitWaitResumeFailed}); err != nil {
			return err
		}
	}
	tx.AfterCommit(svc.wakeDeadlines)
	return nil
}

// wakeHoldWaitsTx makes the waits a cleared hold may have kept waiting due now, in the clear's transaction: those parked
// under it, and those put off because an account was held whose launch would use the hold's account (or gateway). The
// job resumes them on its next round, bounded as usual; nothing is launched here (review S1).
func (svc *Service) wakeHoldWaitsTx(ctx context.Context, tx *store.Tx, u db.UsageLimit) error {
	rows, err := tx.Q.ListHoldWaitsToWake(ctx, db.ListHoldWaitsToWakeParams{Now: tx.NowMicros(), UsageLimitID: u.ID, Lim: limitWaitBatch})
	if err != nil || len(rows) == 0 {
		return err
	}
	woke := false
	for _, w := range rows {
		if w.UsageLimitID != u.ID {
			var spec LaunchSpec
			if json.Unmarshal([]byte(w.LaunchJson), &spec) != nil {
				continue
			}
			gw := spec.Gateway != nil && spec.Gateway.Kind != ""
			if u.Scope == LimitScopeAccount && (gw || spec.AccountRef != u.AccountID) || u.Scope == LimitScopeGateway && !gw {
				continue
			}
		}
		if err := svc.deferWaitTx(ctx, tx, w, tx.Now, w.LastError); err != nil {
			return err
		}
		woke = true
	}
	if woke {
		tx.AfterCommit(svc.wakeDeadlines)
	}
	return nil
}

// settleWaitTx ends a waiting (or resuming) wait as released or cancelled, with a fixed reason, audited.
func (svc *Service) settleWaitTx(ctx context.Context, tx *store.Tx, w db.LimitWait, state, reason string, actor audit.Actor) error {
	n, err := tx.Q.UpdateLimitWait(ctx, db.UpdateLimitWaitParams{State: state, ResumeAt: w.ResumeAt, NewRunID: w.NewRunID, Attempts: w.Attempts,
		LastError: reason, Now: tx.NowMicros(), ID: w.ID, Version: w.Version})
	if err != nil || n == 0 {
		return err
	}
	if err := svc.record(ctx, tx, w.SpaceID, "limit_wait", w.ID, "updated", w.Version+1); err != nil {
		return err
	}
	action := "limit_wait.cancelled"
	if state == WaitReleased {
		action = "limit_wait.released"
	}
	_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: action, Target: audit.Target{Type: "limit_wait", ID: w.ID},
		Details: map[string]any{"space_id": w.SpaceID, "run_id": w.RunID, "task_id": w.TaskID, "reason": reason}})
	return err
}

// deferWaitTx puts a wait off until at with a fixed reason. Every caller changes something (a due wait moves to a later
// time, a woken one to now, a resuming one back to waiting), so a wait that isn't due is never written.
func (svc *Service) deferWaitTx(ctx context.Context, tx *store.Tx, w db.LimitWait, at time.Time, reason string) error {
	n, err := tx.Q.UpdateLimitWait(ctx, db.UpdateLimitWaitParams{State: WaitWaiting, ResumeAt: at.UnixMicro(), NewRunID: w.NewRunID, Attempts: w.Attempts,
		LastError: reason, Now: tx.NowMicros(), ID: w.ID, Version: w.Version})
	if err != nil || n == 0 {
		return err
	}
	return svc.record(ctx, tx, w.SpaceID, "limit_wait", w.ID, "updated", w.Version+1)
}

// NextLimitWait is when the limit-wait job is next due: a wait to resume (no earlier than the stampede spacing allows),
// a wait to release, a limited run to park, or the recovery of stuck resumes.
func (svc *Service) NextLimitWait(ctx context.Context) (time.Time, error) {
	q := svc.s.R()
	now := svc.opts.Clock.Now()
	svc.lw.mu.Lock()
	recovered, spaced := svc.lw.recovered, svc.lw.spacedUntil
	svc.lw.mu.Unlock()
	var next time.Time
	if !recovered {
		next = now // the first round recovers what a stopped central left resuming
	}
	earlier := func(at time.Time) {
		if !at.IsZero() && (next.IsZero() || at.Before(next)) {
			next = at
		}
	}
	due, err := q.NextLimitWaitDue(ctx)
	if err != nil {
		return time.Time{}, err
	}
	if due > 0 {
		at := time.UnixMicro(due)
		if at.Before(spaced) {
			at = spaced
		}
		earlier(at)
	}
	rel, err := q.NextLimitWaitRelease(ctx)
	if err != nil {
		return time.Time{}, err
	}
	if rel > 0 {
		earlier(time.UnixMicro(rel))
	}
	runs, err := q.ListLimitedLiveRuns(ctx, maxLimitRows)
	if err != nil {
		return time.Time{}, err
	}
	settings := map[string]UsageLimitSettings{}
	for _, r := range runs {
		ul, ok := settings[r.SpaceID]
		if !ok {
			ul = svc.ulSettings(ctx, q, r.SpaceID)
			settings[r.SpaceID] = ul
		}
		earlier(parkAt(r, ul.KeepAliveMax))
	}
	if next.IsZero() {
		return time.Time{}, nil
	}
	if next.Before(now) {
		return now, nil
	}
	return next, nil
}

// LimitWaitsDue is the limit-wait deadline job: it recovers resumes a stopped central left behind, releases waits
// whose maximum wait passed, parks limited runs that outlived the keep-alive, and resumes due waits, control first,
// then the oldest (Q6), at most limitResumeBatch per round. It returns how many waits and runs it acted on.
func (svc *Service) LimitWaitsDue(ctx context.Context) (int, error) {
	now := svc.opts.Clock.Now()
	done := 0
	var errs []error
	svc.lw.mu.Lock()
	recoverDue := !svc.lw.recovered || !svc.lw.recoverAt.After(now)
	svc.lw.mu.Unlock()
	if recoverDue {
		n, err := svc.recoverStuckWaits(ctx, now)
		done += n
		errs = append(errs, err)
	}
	n, err := svc.releaseDueWaits(ctx, now)
	done += n
	errs = append(errs, err)
	n, err = svc.parkDueRuns(ctx, now)
	done += n
	errs = append(errs, err)
	n, err = svc.resumeDueWaits(ctx, now)
	done += n
	errs = append(errs, err)
	return done, errors.Join(errs...)
}

// recoverStuckWaits returns waits left resuming to waiting, due now. A resume commits its run and the wait's resumed
// state in one transaction, so a wait still resuming started no run: a central that stopped between the claim and the
// launch left it (rows from before this process started), or a resume whose cleanup failed (rows resuming for longer
// than limitResumingStale).
func (svc *Service) recoverStuckWaits(ctx context.Context, now time.Time) (int, error) {
	svc.lw.mu.Lock()
	cutoff := svc.lw.started
	if c := now.Add(-limitResumingStale); c.After(cutoff) {
		cutoff = c
	}
	svc.lw.recovered, svc.lw.recoverAt = true, now.Add(limitResumingStale)
	svc.lw.mu.Unlock()
	rows, err := svc.s.R().ListStuckLimitWaits(ctx, db.ListStuckLimitWaitsParams{UpdatedAt: cutoff.UnixMicro(), Limit: limitWaitBatch})
	if err != nil || len(rows) == 0 {
		return 0, err
	}
	n := 0
	err = svc.s.W().TxNamed(ctx, "fleet.limit_wait_recover", func(tx *store.Tx) error {
		n = 0
		for _, w := range rows {
			cur, err := tx.Q.GetLimitWait(ctx, w.ID)
			if err != nil {
				return err
			}
			if cur.State != WaitResuming || cur.Version != w.Version {
				continue
			}
			if err := svc.deferWaitTx(ctx, tx, cur, tx.Now, WaitInterrupted); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	if n > 0 {
		svc.opts.Log.Info("limit waits recovered from an interrupted resume", slog.Int("count", n))
	}
	return n, err
}

// releaseDueWaits gives up on workers whose maximum wait passed: their tasks go back to the queue.
func (svc *Service) releaseDueWaits(ctx context.Context, now time.Time) (int, error) {
	rows, err := svc.s.R().ListLimitWaitsToRelease(ctx, sql.NullInt64{Int64: now.UnixMicro(), Valid: true})
	if err != nil {
		return 0, err
	}
	n := 0
	var errs []error
	for i, w := range rows {
		if i == limitWaitBatch {
			break
		}
		ok, err := svc.release(ctx, LimitWaitActor, w.ID, WaitMaxWait, false)
		if err != nil {
			errs = append(errs, err)
		} else if ok {
			n++
		}
	}
	return n, errors.Join(errs...)
}

// release ends a waiting wait as released: its task (if it is still the parked run's) goes back to the queue, ready
// and unassigned, and control is told once. manual is an operator's release: any wait still waiting, control's too.
func (svc *Service) release(ctx context.Context, actor audit.Actor, waitID, reason string, manual bool) (bool, error) {
	released := false
	err := svc.s.W().TxNamed(ctx, "fleet.limit_wait_release", func(tx *store.Tx) error {
		released = false
		w, err := tx.Q.GetLimitWait(ctx, waitID)
		if err != nil {
			return err
		}
		if w.State != WaitWaiting {
			if manual {
				return apperr.New(apperr.CodePrecondition, "the wait is %s; only a waiting wait can be released", w.State)
			}
			return nil
		}
		if !manual && (!w.ReleaseAt.Valid || w.ReleaseAt.Int64 > tx.NowMicros()) {
			return nil
		}
		if err := svc.settleWaitTx(ctx, tx, w, WaitReleased, reason, actor); err != nil {
			return err
		}
		taskStatus := ""
		if w.TaskID != "" && svc.opts.ReleaseTaskTx != nil {
			if taskStatus, err = svc.opts.ReleaseTaskTx(ctx, tx, actor, w.TaskID, w.RunID, "its run waited for a usage limit to reset for too long"); err != nil {
				return err
			}
		}
		w.State, w.LastError, w.Version = WaitReleased, reason, w.Version+1
		if svc.opts.OnLimitWaitTx != nil {
			if err := svc.opts.OnLimitWaitTx(ctx, tx, LimitWaitEvent{Wait: limitWaitFromRow(w), Change: LimitWaitReleased, TaskStatus: taskStatus}); err != nil {
				return err
			}
		}
		released = true
		return nil
	})
	return released, err
}

// parkDueRuns stops the limited runs that outlived the keep-alive and parks them (ADR-0079 decision 4).
func (svc *Service) parkDueRuns(ctx context.Context, now time.Time) (int, error) {
	q := svc.s.R()
	runs, err := q.ListLimitedLiveRuns(ctx, maxLimitRows)
	if err != nil {
		return 0, err
	}
	n := 0
	var errs []error
	settings := map[string]UsageLimitSettings{}
	for _, r := range runs {
		if n == limitWaitBatch {
			break
		}
		ul, ok := settings[r.SpaceID]
		if !ok {
			ul = svc.ulSettings(ctx, q, r.SpaceID)
			settings[r.SpaceID] = ul
		}
		if parkAt(r, ul.KeepAliveMax).After(now) {
			continue
		}
		parked := false
		err := svc.s.W().TxNamed(ctx, "fleet.limit_park", func(tx *store.Tx) error {
			parked = false
			cur, err := tx.Q.GetRun(ctx, r.ID)
			if err != nil {
				return err
			}
			// Only the run as it was read: a report since (a clear, an end) decides instead.
			if cur.Version != r.Version || cur.Desired != "running" || !Live(cur.Observed) || cur.LimitKind == "" {
				return nil
			}
			w, err := svc.parkTx(ctx, tx, cur, storedLimit(cur), "keep_alive_max")
			if err != nil || w == nil {
				return err
			}
			if err := svc.stopRunTx(ctx, tx, cur, "interrupt", "SpaceMan parks the run at its usage limit; it resumes at the reset"); err != nil {
				return err
			}
			parked = true
			return nil
		})
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if parked {
			n++
			svc.hub.Kick(r.NodeID)
		}
	}
	return n, errors.Join(errs...)
}

// resumeDueWaits resumes due waits in order, control first, then the oldest, at most limitResumeBatch per round.
func (svc *Service) resumeDueWaits(ctx context.Context, now time.Time) (int, error) {
	rows, err := svc.s.R().ListDueLimitWaits(ctx, db.ListDueLimitWaitsParams{ResumeAt: now.UnixMicro(), Limit: limitResumeBatch + 1})
	if err != nil {
		return 0, err
	}
	more := len(rows) > limitResumeBatch
	if more {
		rows = rows[:limitResumeBatch]
	}
	svc.lw.mu.Lock()
	svc.lw.spacedUntil = time.Time{}
	if more {
		svc.lw.spacedUntil = now.Add(limitResumeSpacing)
	}
	svc.lw.mu.Unlock()
	n := 0
	var errs []error
	for _, w := range rows {
		// A refusal moves the wait on with its reason; an error here is a failure to record anything, which the deadline
		// loop retries with backoff rather than at once.
		r, err := svc.resume(ctx, LimitWaitActor, w.ID, false)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if r != nil {
			n++
		}
	}
	return n, errors.Join(errs...)
}

// wakeWaits brings forward the waits of a space that were put off for reason (the cause is gone), so the job looks at
// them now. It reads first and writes only when such a wait exists.
func (svc *Service) wakeWaits(ctx context.Context, spaceIDs []string, reason string) {
	woke := false
	for _, id := range spaceIDs {
		rows, err := svc.s.R().ListDeferredLimitWaits(ctx, db.ListDeferredLimitWaitsParams{SpaceID: id, LastError: reason, ResumeAt: svc.now().UnixMicro(),
			Limit: limitWaitBatch})
		if err != nil || len(rows) == 0 {
			continue
		}
		err = svc.s.W().TxNamed(ctx, "fleet.limit_wait_wake", func(tx *store.Tx) error {
			for _, w := range rows {
				cur, err := tx.Q.GetLimitWait(ctx, w.ID)
				if err != nil {
					return err
				}
				if cur.State != WaitWaiting || cur.LastError != reason || cur.ResumeAt <= tx.NowMicros() {
					continue
				}
				if err := svc.deferWaitTx(ctx, tx, cur, tx.Now, reason); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			svc.opts.Log.Warn("wake limit waits", obs.Err(err))
			continue
		}
		woke = true
	}
	if woke {
		svc.wakeDeadlines()
	}
}

// wakeWaitsOnNode is called after a node's hello granted its leases: waits put off because the node was away resume now.
func (svc *Service) wakeWaitsOnNode(ctx context.Context, nodeID string) {
	sps, err := svc.s.R().ListSpacesByNode(ctx, nodeID)
	if err != nil {
		return
	}
	ids := make([]string, 0, len(sps))
	for _, sp := range sps {
		ids = append(ids, sp.ID)
	}
	svc.wakeWaits(ctx, ids, WaitNodeOffline)
}

// resumeOutcome classifies a refused resume: retry later with a reason, or give the wait up.
func resumeOutcome(err error, now time.Time) (at time.Time, reason string, cancel bool) {
	if h, ok := AsHold(err); ok {
		switch {
		case h.Until.IsZero():
			return now.Add(limitRetryPaused), WaitAccountHeld, false
		case h.Known:
			return h.Until.Add(limitResumeSlack), WaitAccountHeld, false
		}
		return h.Until, WaitAccountHeld, false
	}
	if errors.Is(err, errResumeAborted) {
		return now, WaitTaskMoved, true
	}
	switch apperr.CodeOf(err) {
	case apperr.CodeRateLimited:
		return now.Add(limitRetrySoon), WaitSpaceFull, false
	case apperr.CodeUnavailable:
		return now.Add(limitRetryNode), WaitNodeOffline, false
	case apperr.CodeConflict:
		return now, WaitSuperseded, true
	}
	return now.Add(limitRetryFailed), WaitLaunchFailed, false
}

// resumeCheckTx decides, in the claim transaction, whether a wait can resume now: "" when it can, otherwise a fixed
// reason and when to look again, or cancel when it never will.
func (svc *Service) resumeCheckTx(ctx context.Context, tx *store.Tx, w db.LimitWait) (reason string, at time.Time, cancel bool, err error) {
	now := tx.Now
	old, err := tx.Q.GetRun(ctx, w.RunID)
	if err != nil {
		return "", now, false, err
	}
	// The parked run is stopped first; its instance may not have two runs.
	if !ended(old.Observed) {
		return WaitRunEnding, now.Add(limitRetrySoon), false, nil
	}
	sp, err := tx.Q.GetSpace(ctx, w.SpaceID)
	if err != nil {
		return "", now, false, err
	}
	switch {
	case sp.State == "archived":
		return WaitSpaceArchived, now, true, nil
	case sp.State == "paused":
		return WaitSpacePaused, now.Add(limitRetryPaused), false, nil
	case sp.DesiredNodeID.Valid:
		return WaitSpaceRelocating, now.Add(limitRetrySoon), false, nil
	}
	if w.TaskID != "" {
		t, err := tx.Q.GetTask(ctx, w.TaskID)
		if errors.Is(err, sql.ErrNoRows) || err == nil && (t.AssigneeRun != w.RunID || t.Status != "running" && t.Status != "blocked" && t.Status != "review") {
			return WaitTaskMoved, now, true, nil
		} else if err != nil {
			return "", now, false, err
		}
	}
	// Someone started the instance again meanwhile: that run carries on, not this wait.
	if id, err := tx.Q.LiveRunOfInstance(ctx, db.LiveRunOfInstanceParams{SpaceID: w.SpaceID, InstanceID: w.InstanceID}); err == nil && id != "" {
		return WaitSuperseded, now, true, nil
	} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", now, false, err
	}
	lease, err := tx.Q.GetLease(ctx, w.SpaceID)
	if errors.Is(err, sql.ErrNoRows) || err == nil && (lease.State != "active" || !now.Before(time.UnixMicro(lease.ExpiresAt)) || lease.NodeID != sp.NodeID) {
		return WaitNodeOffline, now.Add(limitRetryNode), false, nil
	} else if err != nil {
		return "", now, false, err
	}
	return "", now, false, nil
}

// resume resumes one wait as a new run of its instance: with --resume of its session on the same node, or a fresh
// session when the space has moved node since (control is told). The new run, the wait's resumed state and the task's
// new assignee commit together in the launch transaction. manual is an operator's "resume now": it doesn't wait for the
// resume time, and a refusal is returned and leaves the wait as it was (with the reason). It returns the new run, or nil
// when the wait didn't resume.
func (svc *Service) resume(ctx context.Context, actor audit.Actor, waitID string, manual bool) (*Run, error) {
	var w db.LimitWait
	var sp db.Space
	claimed := false
	err := svc.s.W().TxNamed(ctx, "fleet.limit_wait_claim", func(tx *store.Tx) error {
		claimed = false
		var err error
		if w, err = tx.Q.GetLimitWait(ctx, waitID); err != nil {
			return err
		}
		if w.State != WaitWaiting {
			if manual {
				return apperr.New(apperr.CodePrecondition, "the wait is %s; only a waiting wait can be resumed", w.State)
			}
			return nil
		}
		if !manual && w.ResumeAt > tx.NowMicros() {
			return nil
		}
		reason, at, cancel, err := svc.resumeCheckTx(ctx, tx, w)
		switch {
		case err != nil:
			return err
		case cancel:
			return svc.settleWaitTx(ctx, tx, w, WaitCancelled, reason, actor)
		case reason != "" && manual:
			return apperr.New(apperr.CodePrecondition, "the run can't resume now: %s", waitReasonText(reason))
		case reason != "":
			return svc.deferWaitTx(ctx, tx, w, at, reason)
		}
		if sp, err = tx.Q.GetSpace(ctx, w.SpaceID); err != nil {
			return err
		}
		// The parked run has ended: it no longer holds the space's single control slot (or anything else) as wanted.
		if old, err := tx.Q.GetRun(ctx, w.RunID); err != nil {
			return err
		} else if old.Desired != "stopped" {
			if _, err := tx.Q.SetRunDesired(ctx, db.SetRunDesiredParams{Desired: "stopped", UpdatedAt: tx.NowMicros(), ID: old.ID}); err != nil {
				return err
			}
			if err := svc.record(ctx, tx, old.SpaceID, "run", old.ID, "updated", old.Version+1); err != nil {
				return err
			}
		}
		n, err := tx.Q.UpdateLimitWait(ctx, db.UpdateLimitWaitParams{State: WaitResuming, ResumeAt: w.ResumeAt, NewRunID: "", Attempts: w.Attempts,
			LastError: w.LastError, Now: tx.NowMicros(), ID: w.ID, Version: w.Version})
		if err != nil || n == 0 {
			return err
		}
		w.State, w.Version, w.UpdatedAt = WaitResuming, w.Version+1, tx.NowMicros()
		claimed = true
		return svc.record(ctx, tx, w.SpaceID, "limit_wait", w.ID, "updated", w.Version)
	})
	if err != nil || !claimed {
		return nil, err
	}
	fresh := sp.NodeID != w.NodeID
	in, err := waitLaunch(w, fresh)
	if err == nil {
		in.inTx = func(ctx context.Context, tx *store.Tx, runID string) error {
			return svc.resumedTx(ctx, tx, actor, w, runID, fresh)
		}
		in.manifest = map[string]any{"usage_limit_resume": map[string]any{"limit_wait_id": w.ID, "parked_run_id": w.RunID, "session_resumed": in.SessionID != "",
			"relocated": fresh}}
		var r *Run
		if r, err = svc.LaunchRun(ctx, actor, w.SpaceID, in); err == nil {
			svc.opts.Log.Info("parked run resumed", slog.String("limit_wait.id", w.ID), slog.String(obs.FieldRunID, r.ID), slog.Bool("fresh", fresh))
			return r, nil
		}
	}
	// Refused: back to waiting with the reason, or given up.
	at, reason, cancel := resumeOutcome(err, svc.opts.Clock.Now())
	if manual && !cancel {
		at = time.UnixMicro(w.ResumeAt)
	}
	if rerr := svc.s.W().TxNamed(ctx, "fleet.limit_wait_retry", func(tx *store.Tx) error {
		cur, err := tx.Q.GetLimitWait(ctx, w.ID)
		if err != nil || cur.State != WaitResuming {
			return err
		}
		if cancel {
			return svc.settleWaitTx(ctx, tx, cur, WaitCancelled, reason, actor)
		}
		return svc.deferWaitTx(ctx, tx, cur, at, reason)
	}); rerr != nil {
		return nil, errors.Join(err, rerr)
	}
	if manual {
		return nil, err
	}
	// The refusal is recorded on the wait (its reason and next time): it is not the job's failure.
	svc.opts.Log.Info("limit wait not resumed", slog.String("limit_wait.id", w.ID), slog.String("reason", reason), obs.Err(err))
	return nil, nil
}

// waitReasonText words a wait's fixed reason for a refusal.
func waitReasonText(reason string) string {
	switch reason {
	case WaitRunEnding:
		return "the parked run has not ended yet"
	case WaitSpacePaused:
		return "the space is paused"
	case WaitSpaceRelocating:
		return "the space is relocating"
	case WaitNodeOffline:
		return "the space's node is not connected"
	}
	return reason
}

// resumePrompt is what a resumed run is told. A resumed session continues; a fresh one (the space moved node) gets its
// original prompt again first.
const (
	resumePrompt = "SpaceMan paused you at your usage limit; it has reset. Continue the task you were working on; check spaceman_messages first."
	freshPrompt  = "SpaceMan paused your previous session at a usage limit, and the limit has reset. That session could not be resumed (the " +
		"space moved to another node, or resuming it failed), so this is a fresh session. Continue the task from the state of the workspace; " +
		"check spaceman_messages first."
)

// waitLaunch is the launch that resumes a wait: the parked run's instance, role, model, settings, account and
// workspace, its session unless fresh.
func waitLaunch(w db.LimitWait, fresh bool) (LaunchInput, error) {
	var spec LaunchSpec
	if err := json.Unmarshal([]byte(w.LaunchJson), &spec); err != nil {
		return LaunchInput{}, fmt.Errorf("limit wait %s: launch: %w", w.ID, err)
	}
	in := LaunchInput{Kind: w.RunKind, Role: w.Role, Harness: w.Harness, InstanceID: w.InstanceID, Model: spec.Model, Settings: spec.Settings,
		Workspace: spec.Workspace, Worktree: spec.Worktree, AccountRef: spec.AccountRef, Prompt: resumePrompt}
	switch {
	case fresh || w.SessionID == "":
		// Relocated, a resume that failed, or a run that never finished a turn: it starts over with its prompt.
		in.Prompt = spec.Prompt + "\n\n" + freshPrompt
	default:
		in.SessionID = w.SessionID
	}
	return in, nil
}

// resumedTx runs inside the launch transaction of a resume: the wait becomes resumed with its new run, the task points
// at the new run, and control is told. A task that moved meanwhile aborts the launch.
func (svc *Service) resumedTx(ctx context.Context, tx *store.Tx, actor audit.Actor, w db.LimitWait, runID string, fresh bool) error {
	cur, err := tx.Q.GetLimitWait(ctx, w.ID)
	if err != nil {
		return err
	}
	if cur.State != WaitResuming || cur.Version != w.Version {
		return apperr.New(apperr.CodeConflict, "the wait changed while it was resuming")
	}
	// The instance must not have another live run: one started between the claim and this launch carries on instead.
	if n, err := tx.Q.CountOtherLiveRunsOfInstance(ctx, db.CountOtherLiveRunsOfInstanceParams{SpaceID: w.SpaceID, InstanceID: w.InstanceID, ID: runID}); err != nil {
		return err
	} else if n > 0 {
		return apperr.New(apperr.CodeConflict, "the instance has another live run")
	}
	if w.TaskID != "" && svc.opts.RepointTaskTx != nil {
		ok, err := svc.opts.RepointTaskTx(ctx, tx, actor, w.TaskID, w.RunID, runID)
		if err != nil {
			return err
		}
		if !ok {
			return errResumeAborted
		}
	}
	if _, err := tx.Q.UpdateLimitWait(ctx, db.UpdateLimitWaitParams{State: WaitResumed, ResumeAt: cur.ResumeAt, NewRunID: runID, Attempts: cur.Attempts + 1,
		LastError: "", Now: tx.NowMicros(), ID: cur.ID, Version: cur.Version}); err != nil {
		return err
	}
	if err := svc.record(ctx, tx, cur.SpaceID, "limit_wait", cur.ID, "updated", cur.Version+1); err != nil {
		return err
	}
	if _, err := svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "limit_wait.resumed", Target: audit.Target{Type: "limit_wait", ID: cur.ID},
		Details: map[string]any{"space_id": cur.SpaceID, "run_id": cur.RunID, "new_run_id": runID, "task_id": cur.TaskID, "fresh_session": fresh || cur.SessionID == "",
			"relocated": fresh}}); err != nil {
		return err
	}
	if svc.opts.OnLimitWaitTx != nil {
		first := cur.Attempts == 0
		cur.State, cur.NewRunID, cur.Attempts, cur.LastError, cur.Version = WaitResumed, runID, cur.Attempts+1, "", cur.Version+1
		return svc.opts.OnLimitWaitTx(ctx, tx, LimitWaitEvent{Wait: limitWaitFromRow(cur), Change: LimitWaitResumed, NewRunID: runID, Fresh: fresh,
			FirstResume: first})
	}
	return nil
}

// ResumeLimitWait resumes a parked run now (an operator's "resume now"), whatever its resume time: through the same
// checks and launch as the job, so a held account, a full space or an absent node refuses it with the reason, and the
// wait keeps waiting.
func (svc *Service) ResumeLimitWait(ctx context.Context, actor audit.Actor, spaceID, waitID string) (*LimitWait, error) {
	if err := svc.waitInSpace(ctx, spaceID, waitID); err != nil {
		return nil, err
	}
	if _, err := svc.resume(ctx, actor, waitID, true); err != nil {
		return nil, err
	}
	return svc.getWait(ctx, waitID)
}

// ReleaseLimitWait gives up on a parked run now (an operator's release): its task, if it is still the run's, goes back
// to the queue, ready and unassigned, and control is told. Control's wait can be released this way too: nothing then
// brings control back but its usual start.
func (svc *Service) ReleaseLimitWait(ctx context.Context, actor audit.Actor, spaceID, waitID string) (*LimitWait, error) {
	if err := svc.waitInSpace(ctx, spaceID, waitID); err != nil {
		return nil, err
	}
	if _, err := svc.release(ctx, actor, waitID, WaitReleasedByOp, true); err != nil {
		return nil, err
	}
	return svc.getWait(ctx, waitID)
}

var errWaitNotFound = apperr.New(apperr.CodeNotFound, "limit wait not found in this space")

func (svc *Service) waitInSpace(ctx context.Context, spaceID, waitID string) error {
	w, err := svc.s.R().GetLimitWait(ctx, waitID)
	if errors.Is(err, sql.ErrNoRows) || err == nil && w.SpaceID != spaceID {
		return errWaitNotFound
	}
	return err
}

func (svc *Service) getWait(ctx context.Context, waitID string) (*LimitWait, error) {
	w, err := svc.s.R().GetLimitWait(ctx, waitID)
	if err != nil {
		return nil, err
	}
	out := limitWaitFromRow(w)
	return &out, nil
}

// LimitWaitForRun returns the wait a run was parked in, or nil.
func (svc *Service) LimitWaitForRun(ctx context.Context, runID string) (*LimitWait, error) {
	w, err := svc.s.R().GetLimitWaitByRun(ctx, runID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	out := limitWaitFromRow(w)
	return &out, nil
}

// ControlParked reports whether the space's control is parked at a usage limit: its wait brings it back, so auto-start
// leaves it alone.
func (svc *Service) ControlParked(ctx context.Context, spaceID string) (bool, error) {
	_, err := svc.s.R().GetActiveControlLimitWait(ctx, spaceID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}
