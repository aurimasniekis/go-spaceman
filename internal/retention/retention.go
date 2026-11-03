// Package retention deletes data whose retention period passed (SM-REC-003, SM-HUI-007, SM-ATT-006, SM-SPACE-001,
// SM-OBS-003; ADR-0045).
//
// Retention periods are limits: an installation default in `limits:` and an optional override per space
// (internal/limits). Audit retention is installation-wide (`retention.audit_days`). One daily pass, driven by central's
// shared deadline timer, applies them all: nothing polls per space. Every deletion is audited and recorded in the change
// feed, in the same transaction as the database change; files are removed before that transaction, never inside it.
//
// The pass never deletes what open work still uses: recordings of active runs, attachments of open inbox items (or of
// items whose task or human action is open), and finished human actions that an open task, an open follow-up, an open
// inbox item or an unacknowledged response still refers to. It does not run at all while a restore has not been
// acknowledged, so a restored installation keeps everything until an administrator has looked at it.
package retention

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"strconv"
	"sync"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/backup"
	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/limits"
	"github.com/aurimasniekis/go-spaceman/internal/obs"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Triggers and states of a pass.
const (
	TriggerScheduled = "scheduled"
	TriggerManual    = "manual"

	StateRunning     = "running"
	StateSucceeded   = "succeeded"
	StateFailed      = "failed"
	StateInterrupted = "interrupted"
)

// Kinds of deleted data (keys of Counts).
const (
	KindRecordings   = "recordings"
	KindTranscripts  = "transcripts"
	KindAttachments  = "attachments"
	KindHumanActions = "human_actions"
	KindAudit        = "audit_events"
)

// Actor is who retention deletions are attributed to.
var Actor = audit.Actor{Type: audit.ActorSystem, ID: "retention"}

// ErrRunning means a pass is already running.
var ErrRunning = apperr.New(apperr.CodeConflict, "a retention pass is already running; wait for it to finish")

// ErrRestorePending means the installation was restored and the restore was not acknowledged yet.
var ErrRestorePending = apperr.New(apperr.CodePrecondition,
	"central was restored from a backup and the restore is not acknowledged yet: retention deletes nothing until an administrator acknowledges it")

// Options configure the service.
type Options struct {
	// AuditDays is the installation's audit retention (0: keep forever).
	AuditDays int64
	// Interval is the time between scheduled passes (default 24 h).
	Interval time.Duration
	// FirstDelay is the wait after start before the first scheduled pass when none ran yet (default 10 minutes).
	FirstDelay time.Duration
	// Batch bounds the candidates of one kind read at once in one space (default 500); a pass loops until done.
	Batch int64
	// DeleteRecording deletes central's copy of a run's recording and records it (fleet.Service.DeleteRecording).
	DeleteRecording func(ctx context.Context, actor audit.Actor, spaceID, runID string, details map[string]any) error
	// DeleteTranscript deletes a run's stored transcript and records it (usage.Service.DeleteTranscript).
	DeleteTranscript func(ctx context.Context, actor audit.Actor, spaceID, runID string) error
	// AfterPass runs after a pass that deleted attachments (attachment content garbage collection), outside any
	// transaction.
	AfterPass func(ctx context.Context)
	Clock     clock.Clock
	Log       *slog.Logger
}

// Service runs retention passes.
type Service struct {
	s       *store.Store
	audit   *audit.Writer
	changes *changefeed.Recorder
	limits  *limits.Service
	o       Options

	mu        sync.Mutex
	busy      bool
	startedAt time.Time
	wg        sync.WaitGroup
	base      context.Context
}

// New returns the service.
func New(s *store.Store, aw *audit.Writer, ch *changefeed.Recorder, lim *limits.Service, o Options) *Service {
	if o.Clock == nil {
		o.Clock = clock.System{}
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.Interval <= 0 {
		o.Interval = 24 * time.Hour
	}
	if o.FirstDelay <= 0 {
		o.FirstDelay = 10 * time.Minute
	}
	if o.Batch <= 0 {
		o.Batch = 500
	}
	return &Service{s: s, audit: aw, changes: ch, limits: lim, o: o, startedAt: o.Clock.Now(), base: context.Background()}
}

// Counts are the deletions of a pass by kind.
type Counts map[string]int64

// Pass is one retention pass.
type Pass struct {
	ID          string     `json:"id"`
	Trigger     string     `json:"trigger" enum:"scheduled,manual"`
	RequestedBy string     `json:"requested_by,omitempty"`
	State       string     `json:"state" enum:"running,succeeded,failed,interrupted"`
	StartedAt   time.Time  `json:"started_at"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
	Deleted     Counts     `json:"deleted" doc:"Deletions by kind: recordings, transcripts, attachments, human_actions, audit_events"`
	Error       string     `json:"error,omitempty"`
}

func passFromRow(r db.RetentionPass) Pass {
	p := Pass{ID: r.ID, Trigger: r.Trigger, RequestedBy: r.RequestedBy, State: r.State, StartedAt: time.UnixMicro(r.StartedAt).UTC(), Deleted: Counts{},
		Error: r.Error}
	if r.FinishedAt.Valid {
		t := time.UnixMicro(r.FinishedAt.Int64).UTC()
		p.FinishedAt = &t
	}
	_ = json.Unmarshal([]byte(r.DeletedJson), &p.Deleted)
	return p
}

// Default is one retention period's installation default.
type Default struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Days  int64  `json:"days" doc:"0: kept until deleted or the space is purged"`
}

// Status is the retention overview.
type Status struct {
	Interval  string    `json:"interval"`
	AuditDays int64     `json:"audit_days" doc:"Installation-wide audit retention (0: kept forever)"`
	Defaults  []Default `json:"defaults" doc:"Installation defaults of the per-space retention periods (spaces override them in their limits)"`
	// NextAt is when the next scheduled pass is due; absent while a restore is not acknowledged.
	NextAt *time.Time `json:"next_at,omitempty"`
	// Blocked explains why no pass runs (a restore that is not acknowledged).
	Blocked     string        `json:"blocked,omitempty"`
	Busy        bool          `json:"busy"`
	Passes      []Pass        `json:"passes" doc:"Recent passes, newest first"`
	AuditPruned *audit.Pruned `json:"audit_pruned,omitempty" doc:"The audit log's pruned prefix, when retention deleted audit events"`
}

// Status returns the retention overview.
func (svc *Service) Status(ctx context.Context) (*Status, error) {
	out := &Status{Interval: svc.o.Interval.String(), AuditDays: svc.o.AuditDays, Defaults: []Default{}, Passes: []Pass{}}
	for _, d := range limits.Defs {
		for _, k := range limits.RetentionKeys {
			if d.Key == k {
				out.Defaults = append(out.Defaults, Default{Key: k, Label: d.Label, Days: svc.limits.Default(k)})
			}
		}
	}
	svc.mu.Lock()
	out.Busy = svc.busy
	svc.mu.Unlock()
	blocked, err := svc.restorePending(ctx)
	if err != nil {
		return nil, err
	}
	if blocked {
		out.Blocked = ErrRestorePending.Error()
	} else if next, err := svc.nextAt(ctx); err == nil {
		out.NextAt = &next
	}
	rows, err := svc.s.R().ListRetentionPasses(ctx, 20)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out.Passes = append(out.Passes, passFromRow(r))
	}
	if out.AuditPruned, err = audit.PrunedPrefix(ctx, svc.s.R()); err != nil {
		return nil, err
	}
	return out, nil
}

func (svc *Service) restorePending(ctx context.Context) (bool, error) {
	rs, err := backup.LoadRestoreState(ctx, svc.s.R())
	if err != nil {
		return false, err
	}
	return rs != nil && rs.AcknowledgedAt == nil, nil
}

// nextAt is an interval after the last pass started, or FirstDelay after start when none ran.
func (svc *Service) nextAt(ctx context.Context) (time.Time, error) {
	last, err := svc.s.R().LastRetentionPass(ctx)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		svc.mu.Lock()
		defer svc.mu.Unlock()
		return svc.startedAt.Add(svc.o.FirstDelay), nil
	case err != nil:
		return time.Time{}, err
	}
	return time.UnixMicro(last.StartedAt).UTC().Add(svc.o.Interval), nil
}

// Next is when the next scheduled pass is due (zero while a restore is not acknowledged or a pass runs). It is one of
// the jobs of central's single deadline timer.
func (svc *Service) Next(ctx context.Context) (time.Time, error) {
	svc.mu.Lock()
	busy := svc.busy
	svc.mu.Unlock()
	if busy {
		return time.Time{}, nil
	}
	if blocked, err := svc.restorePending(ctx); err != nil || blocked {
		return time.Time{}, err
	}
	return svc.nextAt(ctx)
}

// Due starts the scheduled pass in the background when it is due. It returns 1 when it started one.
func (svc *Service) Due(ctx context.Context) (int, error) {
	next, err := svc.Next(ctx)
	if err != nil || next.IsZero() || svc.o.Clock.Now().Before(next) {
		return 0, err
	}
	p, err := svc.begin(ctx, audit.System, TriggerScheduled)
	if errors.Is(err, ErrRunning) || errors.Is(err, ErrRestorePending) {
		return 0, nil
	} else if err != nil {
		return 0, err
	}
	svc.wg.Add(1)
	go func() { //nolint:contextcheck // the pass outlives the timer round; it runs under the service context (ends at shutdown)
		defer svc.wg.Done()
		_, _ = svc.finish(svc.baseCtx(), p)
	}()
	return 1, nil
}

// Bind sets the context background passes run under (it ends at shutdown).
func (svc *Service) Bind(ctx context.Context) {
	svc.mu.Lock()
	svc.base, svc.startedAt = ctx, svc.o.Clock.Now()
	svc.mu.Unlock()
}

func (svc *Service) baseCtx() context.Context {
	svc.mu.Lock()
	defer svc.mu.Unlock()
	return svc.base
}

// Wait blocks until background passes started so far have finished (tests and shutdown).
func (svc *Service) Wait() { svc.wg.Wait() }

// Recover marks passes that a restart interrupted.
func (svc *Service) Recover(ctx context.Context) error {
	return svc.s.W().TxNamed(ctx, "retention.recover", func(tx *store.Tx) error {
		_, err := tx.Q.InterruptRetentionPasses(ctx, sql.NullInt64{Int64: tx.NowMicros(), Valid: true})
		return err
	})
}

// Run runs a pass now and returns it when it has finished (an administrator's manual pass).
func (svc *Service) Run(ctx context.Context, actor audit.Actor) (*Pass, error) {
	p, err := svc.begin(ctx, actor, TriggerManual)
	if err != nil {
		return nil, err
	}
	return svc.finish(ctx, p)
}

// begin claims the service and records a running pass.
func (svc *Service) begin(ctx context.Context, actor audit.Actor, trigger string) (*Pass, error) {
	if blocked, err := svc.restorePending(ctx); err != nil {
		return nil, err
	} else if blocked {
		return nil, ErrRestorePending
	}
	svc.mu.Lock()
	if svc.busy {
		svc.mu.Unlock()
		return nil, ErrRunning
	}
	svc.busy = true
	svc.mu.Unlock()
	p := &Pass{ID: ids.New(ids.Retention), Trigger: trigger, RequestedBy: actor.ID, State: StateRunning, Deleted: Counts{}}
	err := svc.s.W().TxNamed(ctx, "retention.start", func(tx *store.Tx) error {
		p.StartedAt = tx.Now
		if err := tx.Q.InsertRetentionPass(ctx, db.InsertRetentionPassParams{ID: p.ID, Trigger: trigger, RequestedBy: actor.ID, StartedAt: tx.NowMicros()}); err != nil {
			return err
		}
		if err := svc.changes.Record(ctx, tx, "", "retention_pass", p.ID, changefeed.Created, 0); err != nil {
			return err
		}
		_, err := svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "retention.started", Target: audit.Target{Type: "retention_pass", ID: p.ID},
			Details: map[string]any{"trigger": trigger}})
		return err
	})
	if err != nil {
		svc.release()
		return nil, err
	}
	return p, nil
}

func (svc *Service) release() {
	svc.mu.Lock()
	svc.busy = false
	svc.mu.Unlock()
}

// finish does the pass's work and records its outcome.
func (svc *Service) finish(ctx context.Context, p *Pass) (*Pass, error) {
	defer svc.release()
	log := svc.o.Log.With(slog.String("retention.pass", p.ID))
	counts, werr := svc.work(ctx)
	p.Deleted, p.State = counts, StateSucceeded
	if werr != nil {
		p.State, p.Error = StateFailed, truncate(werr.Error(), 1000)
		log.Warn("retention pass failed", obs.Err(werr))
	} else {
		log.Info("retention pass finished", slog.Any("deleted", map[string]int64(counts)))
	}
	if counts[KindAttachments] > 0 && svc.o.AfterPass != nil {
		svc.o.AfterPass(ctx)
	}
	js, _ := json.Marshal(counts)
	err := svc.s.W().TxNamed(context.WithoutCancel(ctx), "retention.finish", func(tx *store.Tx) error {
		t := tx.Now
		p.FinishedAt = &t
		if err := tx.Q.FinishRetentionPass(ctx, db.FinishRetentionPassParams{State: p.State, FinishedAt: sql.NullInt64{Int64: tx.NowMicros(), Valid: true},
			DeletedJson: string(js), Error: p.Error, ID: p.ID}); err != nil {
			return err
		}
		if err := svc.changes.Record(ctx, tx, "", "retention_pass", p.ID, changefeed.Updated, 0); err != nil {
			return err
		}
		outcome := audit.OutcomeSuccess
		if p.State != StateSucceeded {
			outcome = audit.OutcomeFailure
		}
		d := map[string]any{"trigger": p.Trigger}
		for k, v := range counts {
			d[k] = v
		}
		_, err := svc.audit.Append(ctx, tx, audit.Event{Actor: audit.System, Action: "retention.finished", Target: audit.Target{Type: "retention_pass", ID: p.ID},
			Outcome: outcome, Details: d})
		return err
	})
	if err != nil {
		return p, err
	}
	return p, werr
}

// work applies every space's retention and the audit retention. It continues past a failing space and reports the
// first error.
func (svc *Service) work(ctx context.Context) (Counts, error) {
	counts := Counts{}
	spaces, err := svc.s.R().ListRetentionSpaces(ctx)
	if err != nil {
		return counts, err
	}
	var errs []error
	for _, sp := range spaces {
		if ctx.Err() != nil {
			return counts, ctx.Err()
		}
		c, err := svc.space(ctx, sp.ID)
		for k, v := range c {
			counts[k] += v
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	if svc.o.AuditDays > 0 {
		n, err := svc.pruneAudit(ctx)
		counts[KindAudit] += n
		if err != nil {
			errs = append(errs, err)
		}
	}
	return counts, errors.Join(errs...)
}

// space applies one space's retention periods.
func (svc *Service) space(ctx context.Context, spaceID string) (Counts, error) {
	counts := Counts{}
	v, err := svc.limits.For(ctx, svc.s.R(), spaceID)
	if err != nil {
		return counts, err
	}
	now := svc.o.Clock.Now()
	cutoff := func(key string) (int64, int64, bool) {
		days := v.Get(key)
		if days <= 0 {
			return 0, 0, false
		}
		return now.Add(-time.Duration(days) * 24 * time.Hour).UnixMicro(), days, true
	}
	details := func(days int64, extra map[string]any) map[string]any {
		d := map[string]any{"reason": "retention", "retention_days": days}
		maps.Copy(d, extra)
		return d
	}
	var errs []error
	if c, days, ok := cutoff(limits.RetainRecordings); ok && svc.o.DeleteRecording != nil {
		n, err := svc.loop(ctx, func() ([]string, error) {
			return svc.s.R().RetainableRecordings(ctx, db.RetainableRecordingsParams{SpaceID: spaceID, Cutoff: sql.NullInt64{Int64: c, Valid: true}, Lim: svc.o.Batch})
		}, func(id string) error { return svc.o.DeleteRecording(ctx, Actor, spaceID, id, details(days, nil)) })
		counts[KindRecordings] += n
		errs = append(errs, err)
	}
	if c, _, ok := cutoff(limits.RetainTranscripts); ok && svc.o.DeleteTranscript != nil {
		n, err := svc.loop(ctx, func() ([]string, error) {
			return svc.s.R().RetainableTranscripts(ctx, db.RetainableTranscriptsParams{SpaceID: spaceID, Cutoff: c, Lim: svc.o.Batch})
		}, func(id string) error { return svc.o.DeleteTranscript(ctx, Actor, spaceID, id) })
		counts[KindTranscripts] += n
		errs = append(errs, err)
	}
	if c, days, ok := cutoff(limits.RetainHumanActions); ok {
		n, err := svc.humanActions(ctx, spaceID, c, days)
		counts[KindHumanActions] += n
		errs = append(errs, err)
	}
	if c, days, ok := cutoff(limits.RetainAttachments); ok {
		n, err := svc.attachments(ctx, spaceID, c, days)
		counts[KindAttachments] += n
		errs = append(errs, err)
	}
	return counts, errors.Join(errs...)
}

// loop deletes the candidates list returns, batch after batch, until a batch deletes nothing new. A candidate that
// fails to delete is skipped for the rest of the pass.
func (svc *Service) loop(ctx context.Context, list func() ([]string, error), del func(id string) error) (int64, error) {
	var n int64
	failed := map[string]bool{}
	var errs []error
	for ctx.Err() == nil {
		ids, err := list()
		if err != nil {
			return n, err
		}
		progress := false
		for _, id := range ids {
			if failed[id] {
				continue
			}
			if err := del(id); err != nil {
				if apperr.CodeOf(err) != apperr.CodeNotFound {
					errs = append(errs, err)
				}
				failed[id] = true
				continue
			}
			n++
			progress = true
		}
		if !progress || int64(len(ids)) < svc.o.Batch {
			break
		}
	}
	return n, errors.Join(errs...)
}

func (svc *Service) humanActions(ctx context.Context, spaceID string, cutoff, days int64) (int64, error) {
	versions := map[string]int64{}
	return svc.loop(ctx, func() ([]string, error) {
		rows, err := svc.s.R().RetainableHumanActions(ctx, db.RetainableHumanActionsParams{SpaceID: spaceID, Cutoff: cutoff, Lim: svc.o.Batch})
		out := make([]string, 0, len(rows))
		for _, r := range rows {
			versions[r.ID] = r.Version
			out = append(out, r.ID)
		}
		return out, err
	}, func(id string) error {
		return svc.s.W().TxNamed(ctx, "retention.human_action", func(tx *store.Tx) error {
			cur, err := tx.Q.GetHumanAction(ctx, id)
			if err != nil {
				return err
			}
			// Re-checked inside the transaction: still finished, unchanged since it was listed, and referred to by nothing open.
			still, err := tx.Q.RetainableHumanActions(ctx, db.RetainableHumanActionsParams{SpaceID: spaceID, OnlyID: sql.NullString{String: id, Valid: true},
				Cutoff: cutoff, Lim: 1})
			if err != nil {
				return err
			}
			if len(still) == 0 {
				return apperr.New(apperr.CodeNotFound, "in use again")
			}
			n, err := tx.Q.DeleteFinishedHumanAction(ctx, db.DeleteFinishedHumanActionParams{ID: id, Version: versions[id]})
			if err != nil {
				return err
			}
			if n == 0 {
				return apperr.New(apperr.CodeNotFound, "changed meanwhile")
			}
			if err := svc.changes.Record(ctx, tx, spaceID, "human_action", id, changefeed.Deleted, cur.Version); err != nil {
				return err
			}
			_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: Actor, Action: "human_action.deleted", Target: audit.Target{Type: "human_action", ID: id},
				Details: map[string]any{"space_id": spaceID, "reason": "retention", "retention_days": days, "status": cur.Status, "type": cur.Type}})
			return err
		})
	})
}

func (svc *Service) attachments(ctx context.Context, spaceID string, cutoff, days int64) (int64, error) {
	sizes := map[string]int64{}
	return svc.loop(ctx, func() ([]string, error) {
		rows, err := svc.s.R().RetainableAttachments(ctx, db.RetainableAttachmentsParams{SpaceID: spaceID, Cutoff: cutoff, Lim: svc.o.Batch})
		out := make([]string, 0, len(rows))
		for _, r := range rows {
			sizes[r.ID] = r.Size
			out = append(out, r.ID)
		}
		return out, err
	}, func(id string) error {
		return svc.s.W().TxNamed(ctx, "retention.attachment", func(tx *store.Tx) error {
			// Re-checked inside the transaction: an item may have been reopened since the attachment was listed.
			still, err := tx.Q.RetainableAttachments(ctx, db.RetainableAttachmentsParams{SpaceID: spaceID, OnlyID: sql.NullString{String: id, Valid: true},
				Cutoff: cutoff, Lim: 1})
			if err != nil {
				return err
			}
			if len(still) == 0 {
				return apperr.New(apperr.CodeNotFound, "in use again")
			}
			if n, err := tx.Q.DeleteAttachment(ctx, id); err != nil || n == 0 {
				return err
			}
			if err := svc.changes.Record(ctx, tx, spaceID, "attachment", id, changefeed.Deleted, 0); err != nil {
				return err
			}
			_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: Actor, Action: "attachment.deleted", Target: audit.Target{Type: "attachment", ID: id},
				Details: map[string]any{"space_id": spaceID, "reason": "retention", "retention_days": days, "size": sizes[id]}})
			return err
		})
	})
}

// pruneAudit deletes audit events older than the installation's audit retention, in batches.
func (svc *Service) pruneAudit(ctx context.Context) (int64, error) {
	cutoff := svc.o.Clock.Now().Add(-time.Duration(svc.o.AuditDays) * 24 * time.Hour).UnixMicro()
	var total int64
	for ctx.Err() == nil {
		var n, through int64
		err := svc.s.W().TxNamed(ctx, "retention.audit", func(tx *store.Tx) error {
			var err error
			if n, through, err = svc.audit.Prune(ctx, tx, cutoff, Actor); err != nil || n == 0 {
				return err
			}
			return svc.changes.Record(ctx, tx, "", "audit", strconv.FormatInt(through, 10), changefeed.Deleted, 0)
		})
		if err != nil {
			return total, err
		}
		total += n
		if n < audit.PruneBatch {
			break
		}
	}
	return total, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
