package backup

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/keyring"
	"github.com/aurimasniekis/go-spaceman/internal/obs"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Triggers of a backup run.
const (
	TriggerManual    = "manual"
	TriggerScheduled = "scheduled"
	TriggerCLI       = "cli"
)

// Run states.
const (
	StateRunning     = "running"
	StateSucceeded   = "succeeded"
	StateFailed      = "failed"
	StateInterrupted = "interrupted"
	StateDeleted     = "deleted"
)

const (
	retryAfterFailure = 15 * time.Minute
	maxErrorLen       = 1000
	listLimit         = 200
)

// ErrRunning means a backup or verification is already running.
var ErrRunning = apperr.New(apperr.CodeConflict, "a backup or verification is already running; wait for it to finish")

// ServiceOptions configure the in-server backup service.
type ServiceOptions struct {
	Target  Target
	Sources Sources
	// Config returns the effective configuration to include (YAML), or nil.
	Config func() []byte
	// Schedule enables scheduled backups every Interval; retention keeps KeepLast and deletes those older than MaxAge
	// (never the newest).
	Schedule bool
	Interval time.Duration
	KeepLast int
	MaxAge   time.Duration
	// FirstDelay is the wait before the first scheduled backup when none succeeded yet (default 5 minutes).
	FirstDelay time.Duration
	Clock      clock.Clock
	Log        *slog.Logger
}

// Service makes, lists, verifies and prunes backups while central runs, and schedules them with one timer.
type Service struct {
	st      *store.Store
	kek     keyring.KeyWrapper
	audit   *audit.Writer
	changes *changefeed.Recorder
	o       ServiceOptions

	mu        sync.Mutex
	busy      bool
	wake      chan struct{}
	wg        sync.WaitGroup
	base      context.Context
	startedAt time.Time
}

// NewService returns the backup service.
func NewService(st *store.Store, kek keyring.KeyWrapper, aw *audit.Writer, ch *changefeed.Recorder, o ServiceOptions) *Service {
	if o.Clock == nil {
		o.Clock = clock.System{}
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.FirstDelay <= 0 {
		o.FirstDelay = 5 * time.Minute
	}
	if o.KeepLast <= 0 {
		o.KeepLast = 7
	}
	return &Service{st: st, kek: kek, audit: aw, changes: ch, o: o, wake: make(chan struct{}, 1), base: context.Background(), startedAt: o.Clock.Now()}
}

// Item is one backup as shown to administrators.
type Item struct {
	ID          string     `json:"id"`
	Target      string     `json:"target" enum:"local,s3"`
	Trigger     string     `json:"trigger" enum:"manual,scheduled,cli"`
	RequestedBy string     `json:"requested_by,omitempty"`
	State       string     `json:"state" enum:"running,succeeded,failed,interrupted,deleted"`
	StartedAt   time.Time  `json:"started_at"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
	CreatedAt   *time.Time `json:"created_at,omitempty" doc:"The snapshot time recorded in the manifest"`
	Files       int64      `json:"files"`
	Bytes       int64      `json:"bytes" doc:"Plaintext size"`
	StoredBytes int64      `json:"stored_bytes" doc:"Sealed size in the target"`
	Error       string     `json:"error,omitempty"`
	// Stored is true while the target holds a complete copy.
	Stored        bool       `json:"stored"`
	SchemaVersion int64      `json:"schema_version,omitempty"`
	KEKID         string     `json:"kek_id,omitempty"`
	Counts        *Counts    `json:"counts,omitempty"`
	VerifyState   string     `json:"verify_state" enum:",running,ok,failed"`
	VerifiedAt    *time.Time `json:"verified_at,omitempty"`
	VerifyError   string     `json:"verify_error,omitempty"`
}

// Status is the backup overview.
type Status struct {
	Target   string     `json:"target" enum:"local,s3"`
	Location string     `json:"location"`
	Schedule bool       `json:"schedule"`
	Interval string     `json:"interval"`
	KeepLast int        `json:"keep_last"`
	MaxAge   string     `json:"max_age,omitempty"`
	NextAt   *time.Time `json:"next_at,omitempty"`
	Busy     bool       `json:"busy"`
	Included []string   `json:"included"`
	Excluded []string   `json:"excluded"`
	Items    []Item     `json:"items"`
	// TargetError is set when the target could not be listed (the history is still shown).
	TargetError string `json:"target_error,omitempty"`
}

func micros(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := time.UnixMicro(v.Int64).UTC()
	return &t
}

func itemFromRow(r db.Backup) Item {
	return Item{ID: r.ID, Target: r.Target, Trigger: r.Trigger, RequestedBy: r.RequestedBy, State: r.State, StartedAt: time.UnixMicro(r.StartedAt).UTC(),
		FinishedAt: micros(r.FinishedAt), Files: r.Files, Bytes: r.Bytes, StoredBytes: r.StoredBytes, Error: r.Error, VerifyState: r.VerifyState,
		VerifiedAt: micros(r.VerifiedAt), VerifyError: r.VerifyError}
}

// Status lists the backups: the run history joined with what the target holds. Complete backups in the target
// without a run (made with the CLI) are listed too.
func (s *Service) Status(ctx context.Context) (*Status, error) {
	out := &Status{Target: s.o.Target.Kind(), Location: s.o.Target.Location(), Schedule: s.o.Schedule, Interval: s.o.Interval.String(),
		KeepLast: s.o.KeepLast, Included: Included, Excluded: Excluded, Items: []Item{}}
	if s.o.MaxAge > 0 {
		out.MaxAge = s.o.MaxAge.String()
	}
	s.mu.Lock()
	out.Busy = s.busy
	s.mu.Unlock()
	if s.o.Schedule {
		if next, err := s.nextAt(ctx); err == nil {
			out.NextAt = &next
		}
	}
	rows, err := s.st.R().ListBackups(ctx, listLimit)
	if err != nil {
		return nil, err
	}
	entries, lerr := s.o.Target.List(ctx)
	if lerr != nil {
		out.TargetError = lerr.Error()
	}
	byID := map[string]*Entry{}
	for i := range entries {
		byID[entries[i].ID] = &entries[i]
	}
	seen := map[string]bool{}
	for _, r := range rows {
		it := itemFromRow(r)
		if e := byID[r.ID]; e != nil && e.Complete {
			annotate(&it, e)
		}
		seen[r.ID] = true
		out.Items = append(out.Items, it)
	}
	for _, e := range entries {
		if seen[e.ID] || !e.Complete {
			continue
		}
		t := e.CreatedAt
		it := Item{ID: e.ID, Target: s.o.Target.Kind(), Trigger: TriggerCLI, State: StateSucceeded, StartedAt: t, FinishedAt: &t}
		annotate(&it, &e)
		out.Items = append(out.Items, it)
	}
	return out, nil
}

func annotate(it *Item, e *Entry) {
	it.Stored, it.StoredBytes = true, e.StoredBytes
	if m := e.Manifest; m != nil {
		t := m.CreatedAt
		it.CreatedAt, it.SchemaVersion, it.KEKID, it.Counts, it.Files, it.Bytes = &t, m.SchemaVersion, m.KEKID, &m.Counts, int64(len(m.Files)), m.Bytes
	}
}

// Start begins a backup in the background and returns its run. Only one backup or verification runs at a time.
func (s *Service) Start(ctx context.Context, actor audit.Actor, trigger string) (*Item, error) {
	if !s.claim() {
		return nil, ErrRunning
	}
	id := ids.New(ids.Backup)
	err := s.st.W().TxNamed(ctx, "backup.start", func(tx *store.Tx) error {
		if err := tx.Q.InsertBackup(ctx, db.InsertBackupParams{ID: id, Target: s.o.Target.Kind(), Trigger: trigger, RequestedBy: actor.ID,
			StartedAt: tx.NowMicros()}); err != nil {
			return err
		}
		if err := s.changes.Record(ctx, tx, "", "backup", id, changefeed.Created, 0); err != nil {
			return err
		}
		_, err := s.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "backup.started", Target: audit.Target{Type: "backup", ID: id},
			Details: map[string]any{"trigger": trigger, "target": s.o.Target.Kind()}})
		return err
	})
	if err != nil {
		s.release()
		return nil, err
	}
	s.wg.Add(1)
	go func() { //nolint:contextcheck // the backup outlives the request; it runs under the service context (ends at shutdown)
		defer s.wg.Done()
		defer s.release()
		s.create(s.baseCtx(), id, actor)
	}()
	r, err := s.st.R().GetBackup(ctx, id)
	if err != nil {
		return nil, err
	}
	it := itemFromRow(r)
	return &it, nil
}

// baseCtx is the context background work runs under: the one Run was given (it ends at shutdown), else Background.
func (s *Service) baseCtx() context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.base
}

func (s *Service) claim() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.busy {
		return false
	}
	s.busy = true
	return true
}

func (s *Service) release() {
	s.mu.Lock()
	s.busy = false
	s.mu.Unlock()
	s.kick()
}

func (s *Service) kick() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Wait blocks until background work started so far has finished (tests and shutdown).
func (s *Service) Wait() { s.wg.Wait() }

// create makes backup id and records the outcome. It runs outside any transaction.
func (s *Service) create(ctx context.Context, id string, actor audit.Actor) {
	log := s.o.Log.With(slog.String("backup.id", id))
	var m *Manifest
	err := func() error {
		staging := s.o.Target.StagingDir()
		if err := os.MkdirAll(staging, 0o700); err != nil {
			return err
		}
		src, err := OpenReadOnly(ctx, s.st.Dir())
		if err != nil {
			return err
		}
		defer src.Close()
		var cfg []byte
		if s.o.Config != nil {
			cfg = s.o.Config()
		}
		dest := filepath.Join(staging, id)
		res, err := Create(ctx, src, s.kek, dest, Options{ID: id, Sources: s.o.Sources, Config: cfg, Clock: s.o.Clock})
		if err != nil {
			return err
		}
		m = res.Manifest
		if s.o.Target.Kind() != "local" {
			defer func() { _ = os.RemoveAll(dest) }()
		}
		return s.o.Target.Store(ctx, dest)
	}()
	state, msg := StateSucceeded, ""
	if err != nil {
		state, msg = StateFailed, truncate(err.Error(), maxErrorLen)
		log.Warn("backup failed", obs.Err(err))
	} else {
		log.Info("backup created", slog.Int("files", len(m.Files)), slog.Int64("bytes", m.SealedBytes))
	}
	_ = s.st.W().TxNamed(ctx, "backup.finish", func(tx *store.Tx) error {
		p := db.FinishBackupParams{State: state, FinishedAt: sql.NullInt64{Int64: tx.NowMicros(), Valid: true}, Error: msg, ID: id}
		if m != nil {
			p.Files, p.Bytes, p.StoredBytes = int64(len(m.Files)), m.Bytes, m.SealedBytes
		}
		if _, err := tx.Q.FinishBackup(ctx, p); err != nil {
			return err
		}
		if err := s.changes.Record(ctx, tx, "", "backup", id, changefeed.Updated, 0); err != nil {
			return err
		}
		outcome := audit.OutcomeSuccess
		if state != StateSucceeded {
			outcome = audit.OutcomeFailure
		}
		_, err := s.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "backup.finished", Target: audit.Target{Type: "backup", ID: id}, Outcome: outcome,
			Details: map[string]any{"files": p.Files, "bytes": p.Bytes, "stored_bytes": p.StoredBytes}})
		return err
	})
	if err == nil {
		if n, perr := s.Prune(ctx); perr != nil {
			log.Warn("backup retention", obs.Err(perr))
		} else if n > 0 {
			log.Info("backup retention deleted old backups", slog.Int("count", n))
		}
	}
}

// StartVerify verifies backup id in the background with the KEK (decrypting every object and checking the database
// copy) and records the result.
func (s *Service) StartVerify(ctx context.Context, actor audit.Actor, id string) (*Item, error) {
	if !ValidID(id) {
		return nil, apperr.New(apperr.CodeNotFound, "backup not found")
	}
	entries, err := s.o.Target.List(ctx)
	if err != nil {
		return nil, err
	}
	var e *Entry
	for i := range entries {
		if entries[i].ID == id && entries[i].Complete {
			e = &entries[i]
		}
	}
	if e == nil {
		return nil, apperr.New(apperr.CodeNotFound, "backup %s is not in the target", id)
	}
	if !s.claim() {
		return nil, ErrRunning
	}
	err = s.st.W().TxNamed(ctx, "backup.verify_start", func(tx *store.Tx) error {
		var files, bytes int64
		if e.Manifest != nil {
			files, bytes = int64(len(e.Manifest.Files)), e.Manifest.Bytes
		}
		if err := tx.Q.UpsertExternalBackup(ctx, db.UpsertExternalBackupParams{ID: id, Target: s.o.Target.Kind(), StartedAt: e.CreatedAt.UnixMicro(),
			FinishedAt: sql.NullInt64{Int64: e.CreatedAt.UnixMicro(), Valid: true}, Files: files, Bytes: bytes, StoredBytes: e.StoredBytes}); err != nil {
			return err
		}
		if _, err := tx.Q.SetBackupVerify(ctx, db.SetBackupVerifyParams{VerifyState: "running", ID: id}); err != nil {
			return err
		}
		return s.changes.Record(ctx, tx, "", "backup", id, changefeed.Updated, 0)
	})
	if err != nil {
		s.release()
		return nil, err
	}
	s.wg.Add(1)
	go func() { //nolint:contextcheck // the verification outlives the request; it runs under the service context (ends at shutdown)
		defer s.wg.Done()
		defer s.release()
		s.verify(s.baseCtx(), id, actor)
	}()
	r, err := s.st.R().GetBackup(ctx, id)
	if err != nil {
		return nil, err
	}
	it := itemFromRow(r)
	return &it, nil
}

func (s *Service) verify(ctx context.Context, id string, actor audit.Actor) {
	tmpRoot := filepath.Join(s.st.Dir(), store.BackupsDir, ".verify")
	rep, err := func() (*Report, error) {
		if err := os.MkdirAll(tmpRoot, 0o700); err != nil {
			return nil, err
		}
		dir, cleanup, err := s.o.Target.Fetch(ctx, id, tmpRoot)
		if err != nil {
			return nil, err
		}
		defer cleanup()
		return Verify(ctx, dir, VerifyOptions{KEK: s.kek, TempDir: tmpRoot})
	}()
	state, msg := "ok", ""
	if err != nil {
		state, msg = "failed", truncate(err.Error(), maxErrorLen)
		s.o.Log.Warn("backup verification failed", slog.String("backup.id", id), obs.Err(err))
	}
	_ = s.st.W().TxNamed(ctx, "backup.verify_finish", func(tx *store.Tx) error {
		if _, err := tx.Q.SetBackupVerify(ctx, db.SetBackupVerifyParams{VerifyState: state, VerifiedAt: sql.NullInt64{Int64: tx.NowMicros(), Valid: true},
			VerifyError: msg, ID: id}); err != nil {
			return err
		}
		if err := s.changes.Record(ctx, tx, "", "backup", id, changefeed.Updated, 0); err != nil {
			return err
		}
		outcome := audit.OutcomeSuccess
		if state != "ok" {
			outcome = audit.OutcomeFailure
		}
		details := map[string]any{"result": state}
		if rep != nil {
			details["files"], details["audit_events"] = rep.Files, rep.AuditEvents
		}
		_, err := s.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "backup.verified", Target: audit.Target{Type: "backup", ID: id}, Outcome: outcome,
			Details: details})
		return err
	})
}

// Prune applies retention to the complete backups in the target: the newest KeepLast are kept; older ones are deleted,
// and with MaxAge so is every backup older than MaxAge except the newest. Incomplete leftovers older than a day are
// removed too.
func (s *Service) Prune(ctx context.Context) (int, error) {
	entries, err := s.o.Target.List(ctx)
	if err != nil {
		return 0, err
	}
	now := s.o.Clock.Now()
	n, complete := 0, 0
	var firstErr error
	for _, e := range entries {
		var drop bool
		if e.Complete {
			complete++
			drop = complete > s.o.KeepLast || (s.o.MaxAge > 0 && complete > 1 && now.Sub(e.CreatedAt) > s.o.MaxAge)
		} else {
			drop = now.Sub(e.CreatedAt) > 24*time.Hour
		}
		if !drop {
			continue
		}
		if err := s.o.Target.Delete(ctx, e.ID); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		n++
		_ = s.st.W().TxNamed(ctx, "backup.pruned", func(tx *store.Tx) error {
			if _, err := tx.Q.MarkBackupDeleted(ctx, db.MarkBackupDeletedParams{DeletedAt: sql.NullInt64{Int64: tx.NowMicros(), Valid: true}, ID: e.ID}); err != nil {
				return err
			}
			if err := s.changes.Record(ctx, tx, "", "backup", e.ID, changefeed.Deleted, 0); err != nil {
				return err
			}
			_, err := s.audit.Append(ctx, tx, audit.Event{Actor: audit.System, Action: "backup.deleted", Target: audit.Target{Type: "backup", ID: e.ID},
				Details: map[string]any{"reason": "retention", "complete": e.Complete}})
			return err
		})
	}
	return n, firstErr
}

// Recover marks runs and verifications that a restart interrupted.
func (s *Service) Recover(ctx context.Context) error {
	return s.st.W().TxNamed(ctx, "backup.recover", func(tx *store.Tx) error {
		if _, err := tx.Q.InterruptRunningBackups(ctx, sql.NullInt64{Int64: tx.NowMicros(), Valid: true}); err != nil {
			return err
		}
		_, err := tx.Q.InterruptRunningVerifications(ctx)
		return err
	})
}

// nextAt is when the next scheduled backup is due: an interval after the last successful one (FirstDelay after start
// when there is none), and no sooner than retryAfterFailure after a failed one.
func (s *Service) nextAt(ctx context.Context) (time.Time, error) {
	last, err := s.st.R().LastSucceededBackupAt(ctx)
	if err != nil {
		return time.Time{}, err
	}
	failed, err := s.st.R().LastFailedBackupAt(ctx)
	if err != nil {
		return time.Time{}, err
	}
	s.mu.Lock()
	next := s.startedAt.Add(s.o.FirstDelay)
	s.mu.Unlock()
	if last > 0 {
		next = time.UnixMicro(last).UTC().Add(s.o.Interval)
	}
	if failed > last {
		next = maxTime(next, time.UnixMicro(failed).UTC().Add(retryAfterFailure))
	}
	return next, nil
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// Run schedules backups with a single timer until ctx ends (no polling): the timer is set to the next due time, and
// reset after every backup and every manual start. Background work started through ctx stops with it.
func (s *Service) Run(ctx context.Context) {
	s.mu.Lock()
	s.base = ctx
	s.startedAt = s.o.Clock.Now()
	s.mu.Unlock()
	if !s.o.Schedule {
		return
	}
	t := time.NewTimer(time.Hour)
	defer t.Stop()
	failedAt := time.Time{}
	for {
		next, err := s.nextAt(ctx)
		if err != nil {
			next = s.o.Clock.Now().Add(retryAfterFailure)
		}
		if !failedAt.IsZero() {
			next = maxTime(next, failedAt.Add(retryAfterFailure))
		}
		wait := max(next.Sub(s.o.Clock.Now()), 0)
		t.Reset(wait)
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
			if !t.Stop() {
				select {
				case <-t.C:
				default:
				}
			}
			continue
		case <-t.C:
		}
		_, err = s.Start(ctx, audit.System, TriggerScheduled)
		switch {
		case errors.Is(err, ErrRunning):
			// Another backup or verification runs; it kicks the loop when it ends.
			select {
			case <-ctx.Done():
				return
			case <-s.wake:
			}
		case err != nil:
			s.o.Log.Warn("scheduled backup could not start", obs.Err(err))
			failedAt = s.o.Clock.Now()
		default:
			failedAt = time.Time{}
		}
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// AcknowledgeRestore records that an administrator has seen the restore notice.
func (s *Service) AcknowledgeRestore(ctx context.Context, actor audit.Actor) (*RestoreState, error) {
	return AcknowledgeRestore(ctx, s.st, s.audit, s.changes, actor)
}
