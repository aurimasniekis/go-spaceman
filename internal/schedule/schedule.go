// Package schedule runs space schedules (SM-LIMIT-003, ADR-0036): inbox items or tasks created at cron or interval
// occurrences in an IANA time zone. Every occurrence is recorded once and its key is the deduplication key of what it
// creates, so a restart never creates an occurrence twice. Occurrences missed while central was stopped (or the space
// archived) follow the schedule's missed-run policy: skip them, run once, or catch up a bounded number.
//
// One timer for the earliest next occurrence of every schedule drives them (central's deadline timer); nothing polls
// per schedule or per space, and no agent is woken to wait for a schedule: the items it creates are ordinary inbox
// items and tasks.
package schedule

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	_ "time/tzdata" // IANA zones are embedded: schedules never depend on the host's zone database

	"github.com/robfig/cron/v3"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/inbox"
	"github.com/aurimasniekis/go-spaceman/internal/queue"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Kinds, missed-run policies and targets.
const (
	KindCron     = "cron"
	KindInterval = "interval"

	MissedSkip    = "skip"     // missed occurrences are recorded as missed; the next one runs on time
	MissedRunOnce = "run_once" // the latest missed occurrence runs once, late; the others are recorded as missed
	MissedCatchUp = "catch_up" // up to catch_up_max of the latest missed occurrences run, oldest first

	TargetInbox = "inbox"
	TargetTask  = "task"
)

// Bounds.
const (
	MinInterval   = 60
	MaxInterval   = 366 * 24 * 60 * 60
	MaxCatchUp    = 100
	MaxTitle      = 200
	MaxBody       = 20_000
	lateAfter     = time.Minute // an occurrence handled later than this is late (central was stopped, say)
	maxEnumerated = 10_000      // occurrences counted per missed gap; beyond it the gap is summarized
)

var (
	parser   = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)
	system   = audit.Actor{Type: audit.ActorSystem, ID: "schedule"}
	notFound = apperr.New(apperr.CodeNotFound, "schedule not found")
)

// Schedule is one space schedule.
type Schedule struct {
	ID              string      `json:"id"`
	SpaceID         string      `json:"space_id"`
	Name            string      `json:"name"`
	Description     string      `json:"description,omitempty"`
	Enabled         bool        `json:"enabled"`
	Kind            string      `json:"kind" enum:"cron,interval"`
	Cron            string      `json:"cron,omitempty" doc:"Five-field cron expression or @hourly/@daily/@weekly/@monthly/@yearly, in the time zone"`
	IntervalSeconds int64       `json:"interval_seconds,omitempty"`
	Anchor          time.Time   `json:"anchor" doc:"Interval occurrences are anchor + n × interval"`
	Timezone        string      `json:"timezone" doc:"IANA time zone of the cron expression"`
	Missed          string      `json:"missed" enum:"skip,run_once,catch_up" doc:"What happens to occurrences missed while central was stopped or the space archived"`
	CatchUpMax      int64       `json:"catch_up_max" doc:"How many missed occurrences catch_up runs at most"`
	SkipIfOpen      bool        `json:"skip_if_open" doc:"Skip an occurrence while what the previous one created is still open"`
	Target          string      `json:"target" enum:"inbox,task"`
	Title           string      `json:"title"`
	Body            string      `json:"body,omitempty" doc:"Markdown: the inbox item body or the task objective"`
	Priority        string      `json:"priority" enum:"low,normal,high,urgent" doc:"Inbox urgency or task priority"`
	AssigneeRole    string      `json:"assignee_role,omitempty" doc:"Tasks only"`
	NextAt          *time.Time  `json:"next_at,omitempty"`
	Upcoming        []time.Time `json:"upcoming" doc:"The next occurrences"`
	LastAt          *time.Time  `json:"last_at,omitempty" doc:"The last occurrence handled"`
	LastOutcome     string      `json:"last_outcome,omitempty"`
	LastTargetID    string      `json:"last_target_id,omitempty"`
	CreatedAt       time.Time   `json:"created_at"`
	CreatedBy       string      `json:"created_by"`
	UpdatedAt       time.Time   `json:"updated_at"`
	Version         int64       `json:"version"`
}

// Input creates or replaces a schedule.
type Input struct {
	Name            string     `json:"name" minLength:"1" maxLength:"80"`
	Description     string     `json:"description,omitempty" maxLength:"2000"`
	Enabled         *bool      `json:"enabled,omitempty" doc:"Default true"`
	Kind            string     `json:"kind" enum:"cron,interval"`
	Cron            string     `json:"cron,omitempty" maxLength:"100"`
	IntervalSeconds int64      `json:"interval_seconds,omitempty" doc:"60 s to 366 days"`
	Anchor          *time.Time `json:"anchor,omitempty" doc:"Interval start (default: now)"`
	Timezone        string     `json:"timezone,omitempty" maxLength:"64" doc:"IANA time zone (default UTC)"`
	Missed          string     `json:"missed,omitempty" enum:"skip,run_once,catch_up" doc:"Default skip"`
	CatchUpMax      int64      `json:"catch_up_max,omitempty" doc:"1-100; default 3"`
	SkipIfOpen      *bool      `json:"skip_if_open,omitempty" doc:"Default true"`
	Target          string     `json:"target" enum:"inbox,task"`
	Title           string     `json:"title" minLength:"1" maxLength:"200"`
	Body            string     `json:"body,omitempty" maxLength:"20000"`
	Priority        string     `json:"priority,omitempty" enum:"low,normal,high,urgent"`
	AssigneeRole    string     `json:"assignee_role,omitempty" maxLength:"48"`
}

// Occurrence is one handled occurrence.
type Occurrence struct {
	At         time.Time `json:"at" doc:"The scheduled time"`
	Outcome    string    `json:"outcome" enum:"created,duplicate,skipped,missed,failed"`
	Late       bool      `json:"late" doc:"Handled after its time (central was stopped, say)"`
	TargetKind string    `json:"target_kind,omitempty" enum:",inbox_item,task"`
	TargetID   string    `json:"target_id,omitempty"`
	Detail     string    `json:"detail,omitempty"`
	HandledAt  time.Time `json:"handled_at"`
}

// Service manages schedules.
type Service struct {
	s       *store.Store
	audit   *audit.Writer
	changes *changefeed.Recorder
	inbox   *inbox.Service
	queue   *queue.Service
	clock   clock.Clock
	log     *slog.Logger
	// Kick wakes the timer after a commit that changed a schedule (nil: none).
	Kick func()
}

// New returns the service.
func New(s *store.Store, aw *audit.Writer, ch *changefeed.Recorder, ib *inbox.Service, q *queue.Service, c clock.Clock, log *slog.Logger) *Service {
	if c == nil {
		c = clock.System{}
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Service{s: s, audit: aw, changes: ch, inbox: ib, queue: q, clock: c, log: log}
}

func (svc *Service) kick() {
	if svc.Kick != nil {
		svc.Kick()
	}
}

// timing is what computes occurrences.
type timing struct {
	kind     string
	cron     cron.Schedule
	interval time.Duration
	anchor   time.Time
	loc      *time.Location
}

// next returns the first occurrence strictly after t.
func (tm timing) next(t time.Time) time.Time {
	if tm.kind == KindInterval {
		if t.Before(tm.anchor) {
			return tm.anchor.UTC()
		}
		k := int64(t.Sub(tm.anchor)/tm.interval) + 1
		return tm.anchor.Add(time.Duration(k) * tm.interval).UTC() //nolint:durationcheck // k is a count, not a duration
	}
	return tm.cron.Next(t.In(tm.loc)).UTC()
}

func parseTiming(v *apperr.Validation, kind, spec string, intervalSeconds int64, anchor time.Time, tz string) timing {
	tm := timing{kind: kind, anchor: anchor}
	loc, err := time.LoadLocation(tz)
	if err != nil || tz == "" || tz == "Local" {
		v.Add("timezone", "must be an IANA time zone such as UTC or Europe/Vilnius")
		loc = time.UTC
	}
	tm.loc = loc
	switch kind {
	case KindCron:
		spec = strings.TrimSpace(spec)
		switch {
		case spec == "":
			v.Add("cron", "is required for a cron schedule")
		case strings.Contains(spec, "TZ="):
			v.Add("cron", "set the time zone in timezone, not in the expression")
		case strings.HasPrefix(spec, "@every"):
			v.Add("cron", "use an interval schedule for @every")
		default:
			s, err := parser.Parse(spec)
			if err != nil {
				v.Add("cron", "%v", err)
				break
			}
			tm.cron = s
			if n := s.Next(time.Now().In(loc)); n.IsZero() {
				v.Add("cron", "never fires")
			}
		}
		if intervalSeconds != 0 {
			v.Add("interval_seconds", "is for interval schedules")
		}
	case KindInterval:
		if intervalSeconds < MinInterval || intervalSeconds > MaxInterval {
			v.Add("interval_seconds", "must be within %d..%d", MinInterval, MaxInterval)
		}
		if spec != "" {
			v.Add("cron", "is for cron schedules")
		}
		tm.interval = time.Duration(intervalSeconds) * time.Second
	default:
		v.Add("kind", "must be cron or interval")
	}
	return tm
}

func timingOf(r *db.SpaceSchedule) (timing, error) {
	v := &apperr.Validation{}
	tm := parseTiming(v, r.Kind, r.Cron, r.IntervalSeconds, time.UnixMicro(r.AnchorAt).UTC(), r.Timezone)
	return tm, v.Err()
}

func (in *Input) normalize(now time.Time) (timing, error) {
	in.Name, in.Title = strings.TrimSpace(in.Name), strings.TrimSpace(in.Title)
	if in.Timezone == "" {
		in.Timezone = "UTC"
	}
	if in.Missed == "" {
		in.Missed = MissedSkip
	}
	if in.CatchUpMax == 0 {
		in.CatchUpMax = 3
	}
	if in.Priority == "" {
		in.Priority = "normal"
	}
	anchor := now.Truncate(time.Second)
	if in.Anchor != nil {
		anchor = in.Anchor.UTC()
	}
	in.Anchor = &anchor
	v := &apperr.Validation{}
	if in.Name == "" || len(in.Name) > 80 {
		v.Add("name", "is required (at most 80 characters)")
	}
	if in.Title == "" || len(in.Title) > MaxTitle {
		v.Add("title", "is required (at most %d characters)", MaxTitle)
	}
	if len(in.Body) > MaxBody || len(in.Description) > 2000 {
		v.Add("body", "body and description are limited to %d and 2000 bytes", MaxBody)
	}
	switch in.Missed {
	case MissedSkip, MissedRunOnce, MissedCatchUp:
	default:
		v.Add("missed", "must be skip, run_once or catch_up")
	}
	if in.CatchUpMax < 1 || in.CatchUpMax > MaxCatchUp {
		v.Add("catch_up_max", "must be within 1..%d", MaxCatchUp)
	}
	switch in.Target {
	case TargetInbox:
		if in.AssigneeRole != "" {
			v.Add("assignee_role", "is for task schedules")
		}
	case TargetTask:
	default:
		v.Add("target", "must be inbox or task")
	}
	switch in.Priority {
	case "low", "normal", "high", "urgent":
	default:
		v.Add("priority", "must be low, normal, high or urgent")
	}
	tm := parseTiming(v, in.Kind, in.Cron, in.IntervalSeconds, anchor, in.Timezone)
	return tm, v.Err()
}

// Preview validates timing fields and returns the next occurrences after now.
func (svc *Service) Preview(in Input, n int) ([]time.Time, error) {
	if in.Name == "" {
		in.Name = "preview"
	}
	if in.Title == "" {
		in.Title = "preview"
	}
	if in.Target == "" {
		in.Target = TargetInbox
	}
	tm, err := in.normalize(svc.clock.Now())
	if err != nil {
		return nil, err
	}
	return upcoming(tm, svc.clock.Now(), n), nil
}

func upcoming(tm timing, from time.Time, n int) []time.Time {
	out := []time.Time{}
	t := from
	for range n {
		t = tm.next(t)
		if t.IsZero() {
			break
		}
		out = append(out, t)
	}
	return out
}

func b2i(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func boolOr(p *bool, d bool) bool {
	if p == nil {
		return d
	}
	return *p
}

func tp(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := time.UnixMicro(v.Int64).UTC()
	return &t
}

func (svc *Service) fromRow(r db.SpaceSchedule) *Schedule {
	s := &Schedule{ID: r.ID, SpaceID: r.SpaceID, Name: r.Name, Description: r.Description, Enabled: r.Enabled == 1, Kind: r.Kind, Cron: r.Cron,
		IntervalSeconds: r.IntervalSeconds, Anchor: time.UnixMicro(r.AnchorAt).UTC(), Timezone: r.Timezone, Missed: r.Missed, CatchUpMax: r.CatchUpMax,
		SkipIfOpen: r.SkipIfOpen == 1, Target: r.Target, Title: r.Title, Body: r.Body, Priority: r.Priority, AssigneeRole: r.AssigneeRole,
		NextAt: tp(r.NextAt), LastAt: tp(r.LastAt), LastOutcome: r.LastOutcome, LastTargetID: r.LastTargetID, CreatedAt: time.UnixMicro(r.CreatedAt).UTC(),
		CreatedBy: r.CreatedBy, UpdatedAt: time.UnixMicro(r.UpdatedAt).UTC(), Version: r.Version, Upcoming: []time.Time{}}
	if tm, err := timingOf(&r); err == nil && s.Enabled && s.NextAt != nil {
		s.Upcoming = append([]time.Time{*s.NextAt}, upcoming(tm, *s.NextAt, 2)...)
	}
	return s
}

func actorRef(a audit.Actor) string {
	if a.ID == "" {
		return a.Type
	}
	return a.Type + ":" + a.ID
}

func nullTime(t time.Time) sql.NullInt64 {
	if t.IsZero() {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: t.UnixMicro(), Valid: true}
}

// Create adds a schedule; its first occurrence is the first one after now.
func (svc *Service) Create(ctx context.Context, actor audit.Actor, spaceID string, in Input) (*Schedule, error) {
	now := svc.clock.Now().UTC()
	tm, err := in.normalize(now)
	if err != nil {
		return nil, err
	}
	enabled := boolOr(in.Enabled, true)
	next := time.Time{}
	if enabled {
		next = tm.next(now)
	}
	id := ids.New(ids.Schedule)
	err = svc.s.W().TxNamed(ctx, "schedule.create", func(tx *store.Tx) error {
		sp, err := tx.Q.GetSpace(ctx, spaceID)
		if errors.Is(err, sql.ErrNoRows) {
			return apperr.New(apperr.CodeNotFound, "space not found")
		} else if err != nil {
			return err
		}
		if sp.State == "archived" {
			return apperr.New(apperr.CodePrecondition, "the space is archived")
		}
		if err := tx.Q.InsertSchedule(ctx, db.InsertScheduleParams{ID: id, SpaceID: spaceID, Name: in.Name, Description: in.Description, Enabled: b2i(enabled),
			Kind: in.Kind, Cron: strings.TrimSpace(in.Cron), IntervalSeconds: in.IntervalSeconds, AnchorAt: in.Anchor.UnixMicro(), Timezone: in.Timezone,
			Missed: in.Missed, CatchUpMax: in.CatchUpMax, SkipIfOpen: b2i(boolOr(in.SkipIfOpen, true)), Target: in.Target, Title: in.Title, Body: in.Body,
			Priority: in.Priority, AssigneeRole: in.AssigneeRole, NextAt: nullTime(next), CreatedAt: tx.NowMicros(), CreatedBy: actorRef(actor),
			UpdatedAt: tx.NowMicros()}); err != nil {
			if store.IsConstraint(err) {
				return apperr.New(apperr.CodeAlreadyExists, "this space already has a schedule named %q", in.Name)
			}
			return err
		}
		if err := svc.changes.Record(ctx, tx, spaceID, "schedule", id, "created", 1); err != nil {
			return err
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "schedule.created", Target: audit.Target{Type: "schedule", ID: id},
			Details: map[string]any{"space_id": spaceID, "kind": in.Kind, "timezone": in.Timezone, "missed": in.Missed, "target": in.Target, "enabled": enabled}})
		return err
	})
	if err != nil {
		return nil, err
	}
	svc.kick()
	return svc.Get(ctx, spaceID, id)
}

// Update replaces a schedule at version. A changed timing (or re-enabling) restarts it from now: occurrences before
// the change are not treated as missed.
func (svc *Service) Update(ctx context.Context, actor audit.Actor, spaceID, id string, version int64, in Input) (*Schedule, error) {
	now := svc.clock.Now().UTC()
	err := svc.s.W().TxNamed(ctx, "schedule.update", func(tx *store.Tx) error {
		cur, err := tx.Q.GetSchedule(ctx, id)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && cur.SpaceID != spaceID) {
			return notFound
		} else if err != nil {
			return err
		}
		if cur.Version != version {
			return apperr.New(apperr.CodePrecondition, "the schedule changed since version %d", version)
		}
		if in.Anchor == nil && in.Kind == cur.Kind && in.IntervalSeconds == cur.IntervalSeconds {
			a := time.UnixMicro(cur.AnchorAt).UTC()
			in.Anchor = &a
		}
		tm, err := in.normalize(now)
		if err != nil {
			return err
		}
		enabled := boolOr(in.Enabled, cur.Enabled == 1)
		next := cur.NextAt
		timingChanged := in.Kind != cur.Kind || strings.TrimSpace(in.Cron) != cur.Cron || in.IntervalSeconds != cur.IntervalSeconds ||
			in.Anchor.UnixMicro() != cur.AnchorAt || in.Timezone != cur.Timezone
		switch {
		case !enabled:
			next = sql.NullInt64{}
		case timingChanged || cur.Enabled == 0 || !cur.NextAt.Valid:
			next = nullTime(tm.next(now))
		}
		n, err := tx.Q.UpdateSchedule(ctx, db.UpdateScheduleParams{Name: in.Name, Description: in.Description, Enabled: b2i(enabled), Kind: in.Kind,
			Cron: strings.TrimSpace(in.Cron), IntervalSeconds: in.IntervalSeconds, AnchorAt: in.Anchor.UnixMicro(), Timezone: in.Timezone, Missed: in.Missed,
			CatchUpMax: in.CatchUpMax, SkipIfOpen: b2i(boolOr(in.SkipIfOpen, cur.SkipIfOpen == 1)), Target: in.Target, Title: in.Title, Body: in.Body,
			Priority: in.Priority, AssigneeRole: in.AssigneeRole, NextAt: next, Now: tx.NowMicros(), ID: id, Version: version})
		if err != nil {
			if store.IsConstraint(err) {
				return apperr.New(apperr.CodeAlreadyExists, "this space already has a schedule named %q", in.Name)
			}
			return err
		}
		if n == 0 {
			return apperr.New(apperr.CodePrecondition, "the schedule changed concurrently")
		}
		if err := svc.changes.Record(ctx, tx, spaceID, "schedule", id, "updated", version+1); err != nil {
			return err
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "schedule.updated", Target: audit.Target{Type: "schedule", ID: id},
			Details: map[string]any{"space_id": spaceID, "kind": in.Kind, "timezone": in.Timezone, "missed": in.Missed, "target": in.Target, "enabled": enabled,
				"timing_changed": timingChanged}})
		return err
	})
	if err != nil {
		return nil, err
	}
	svc.kick()
	return svc.Get(ctx, spaceID, id)
}

// Delete removes a schedule (what it created stays).
func (svc *Service) Delete(ctx context.Context, actor audit.Actor, spaceID, id string) error {
	err := svc.s.W().TxNamed(ctx, "schedule.delete", func(tx *store.Tx) error {
		n, err := tx.Q.DeleteSchedule(ctx, db.DeleteScheduleParams{ID: id, SpaceID: spaceID})
		if err != nil {
			return err
		}
		if n == 0 {
			return notFound
		}
		if err := svc.changes.Record(ctx, tx, spaceID, "schedule", id, "deleted", 0); err != nil {
			return err
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "schedule.deleted", Target: audit.Target{Type: "schedule", ID: id},
			Details: map[string]any{"space_id": spaceID}})
		return err
	})
	if err == nil {
		svc.kick()
	}
	return err
}

// Get returns a schedule.
func (svc *Service) Get(ctx context.Context, spaceID, id string) (*Schedule, error) {
	r, err := svc.s.R().GetSchedule(ctx, id)
	if err != nil || r.SpaceID != spaceID {
		return nil, notFound
	}
	return svc.fromRow(r), nil
}

// List returns a space's schedules.
func (svc *Service) List(ctx context.Context, spaceID string) ([]*Schedule, error) {
	rows, err := svc.s.R().ListSchedules(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	out := make([]*Schedule, 0, len(rows))
	for _, r := range rows {
		out = append(out, svc.fromRow(r))
	}
	return out, nil
}

// Occurrences returns a schedule's most recent occurrences.
func (svc *Service) Occurrences(ctx context.Context, spaceID, id string, limit int) ([]Occurrence, error) {
	if _, err := svc.Get(ctx, spaceID, id); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := svc.s.R().ListScheduleOccurrences(ctx, db.ListScheduleOccurrencesParams{ScheduleID: id, Limit: int64(limit)})
	if err != nil {
		return nil, err
	}
	out := make([]Occurrence, 0, len(rows))
	for _, r := range rows {
		out = append(out, Occurrence{At: time.UnixMicro(r.OccurrenceAt).UTC(), Outcome: r.Outcome, Late: r.Late == 1, TargetKind: r.TargetKind, TargetID: r.TargetID,
			Detail: r.Detail, HandledAt: time.UnixMicro(r.At).UTC()})
	}
	return out, nil
}

// Next is the earliest next occurrence of any enabled schedule (zero: none): one of central's deadline-timer jobs.
func (svc *Service) Next(ctx context.Context) (time.Time, error) {
	n, err := svc.s.R().NextScheduleAt(ctx)
	if err != nil || n == 0 {
		return time.Time{}, err
	}
	return time.UnixMicro(n).UTC(), nil
}

// Due handles every occurrence that is due and returns how many items or tasks it created.
func (svc *Service) Due(ctx context.Context) (int, error) {
	now := svc.clock.Now().UTC()
	rows, err := svc.s.R().ListDueSchedules(ctx, nullTime(now))
	if err != nil {
		return 0, err
	}
	created := 0
	for _, r := range rows {
		n, err := svc.handle(ctx, &r, now)
		created += n
		if err != nil && ctx.Err() == nil {
			svc.log.Warn("schedule occurrence", slog.String("schedule.id", r.ID), slog.Any("error", err))
		}
	}
	return created, nil
}

// handle works through a due schedule: the occurrences from next_at up to now, under its missed-run policy.
func (svc *Service) handle(ctx context.Context, r *db.SpaceSchedule, now time.Time) (int, error) {
	tm, err := timingOf(r)
	if err != nil {
		return 0, err
	}
	first := time.UnixMicro(r.NextAt.Int64).UTC()
	occs := []time.Time{first}
	truncated := false
	for t := tm.next(first); !t.After(now); t = tm.next(t) {
		if len(occs) >= maxEnumerated {
			truncated = true
			break
		}
		occs = append(occs, t)
	}
	next := tm.next(now)
	// Occurrences handled more than lateAfter after their time were missed (central stopped, space archived).
	var onTime []time.Time
	missed := occs
	if last := occs[len(occs)-1]; now.Sub(last) <= lateAfter {
		onTime, missed = []time.Time{last}, occs[:len(occs)-1]
	}
	var run []time.Time
	switch r.Missed {
	case MissedRunOnce:
		if len(missed) > 0 && len(onTime) == 0 {
			run, missed = missed[len(missed)-1:], missed[:len(missed)-1]
		}
	case MissedCatchUp:
		k := min(int(r.CatchUpMax), len(missed))
		run, missed = missed[len(missed)-k:], missed[:len(missed)-k]
	}
	created := 0
	if len(missed) > 0 {
		detail := fmt.Sprintf("%d occurrence(s) missed between %s and %s (policy %s)", len(missed), missed[0].Format(time.RFC3339),
			missed[len(missed)-1].Format(time.RFC3339), r.Missed)
		if truncated {
			detail = fmt.Sprintf("more than %d occurrences missed since %s (policy %s)", maxEnumerated, missed[0].Format(time.RFC3339), r.Missed)
		}
		if err := svc.record(ctx, r, missed[len(missed)-1], "missed", true, "", "", detail); err != nil {
			return 0, err
		}
	}
	for _, at := range run {
		n, err := svc.fire(ctx, r, at, true)
		created += n
		if err != nil {
			return created, err
		}
	}
	for _, at := range onTime {
		n, err := svc.fire(ctx, r, at, false)
		created += n
		if err != nil {
			return created, err
		}
	}
	return created, svc.advance(ctx, r, next)
}

// fire handles one occurrence: skips it while the previous one's item or task is open (skip_if_open), otherwise
// creates the item or task under the occurrence's deduplication key.
func (svc *Service) fire(ctx context.Context, r *db.SpaceSchedule, at time.Time, late bool) (int, error) {
	key := fmt.Sprintf("schedule:%s:%d", r.ID, at.Unix())
	if r.SkipIfOpen == 1 && r.LastTargetID != "" && svc.stillOpen(ctx, r) {
		return 0, svc.record(ctx, r, at, "skipped", late, "", r.LastTargetID, "what the previous occurrence created is still open")
	}
	trusted := map[string]any{"schedule_id": r.ID, "schedule": r.Name, "occurrence": at.Format(time.RFC3339), "timezone": r.Timezone, "late": late}
	var kind, targetID string
	var created bool
	var err error
	switch r.Target {
	case TargetTask:
		kind = "task"
		var t *queue.Task
		t, created, err = svc.queue.Create(ctx, system, r.SpaceID, queue.CreateInput{Title: r.Title, Objective: r.Body, Priority: r.Priority, Status: "ready",
			AssigneeRole: r.AssigneeRole, OriginKind: "schedule", OriginID: r.ID, DedupKey: key})
		if err == nil {
			targetID = t.ID
		}
	default:
		kind = "inbox_item"
		var it *inbox.Item
		it, created, err = svc.inbox.Create(ctx, system, r.SpaceID, inbox.CreateInput{SourceKind: "schedule", SourceID: r.ID, SourceLabel: r.Name, Title: r.Title,
			ContentType: inbox.Markdown, Body: r.Body, CorrelationID: r.ID, DedupKey: key, Urgency: r.Priority, Trusted: trusted})
		if err == nil {
			targetID = it.ID
		}
	}
	if err != nil {
		// Recorded and not retried: a schedule never piles up retries; the next occurrence runs on time.
		return 0, svc.record(ctx, r, at, "failed", late, kind, "", truncate(err.Error(), 500))
	}
	outcome, n := "created", 1
	if !created {
		outcome, n = "duplicate", 0
	}
	return n, svc.record(ctx, r, at, outcome, late, kind, targetID, "")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func (svc *Service) stillOpen(ctx context.Context, r *db.SpaceSchedule) bool {
	switch r.Target {
	case TargetTask:
		t, err := svc.queue.Get(ctx, r.SpaceID, r.LastTargetID)
		return err == nil && t.Status != "done" && t.Status != "failed" && t.Status != "cancelled"
	default:
		it, err := svc.inbox.Get(ctx, r.SpaceID, r.LastTargetID)
		return err == nil && it.Status != inbox.StatusDone && it.Status != inbox.StatusRejected
	}
}

// record stores an occurrence (once: a repeated occurrence after a crash changes nothing) and the schedule's last outcome.
func (svc *Service) record(ctx context.Context, r *db.SpaceSchedule, at time.Time, outcome string, late bool, kind, targetID, detail string) error {
	if targetID != "" {
		r.LastTargetID = targetID
	}
	return svc.s.W().TxNamed(ctx, "schedule.occurrence", func(tx *store.Tx) error {
		n, err := tx.Q.InsertScheduleOccurrence(ctx, db.InsertScheduleOccurrenceParams{ScheduleID: r.ID, OccurrenceAt: at.UnixMicro(), SpaceID: r.SpaceID,
			Outcome: outcome, Late: b2i(late), TargetKind: kind, TargetID: targetID, Detail: detail, At: tx.NowMicros()})
		if err != nil || n == 0 {
			return err
		}
		if err := tx.Q.SetScheduleLast(ctx, db.SetScheduleLastParams{LastAt: nullTime(at), LastOutcome: outcome, LastTargetID: r.LastTargetID, ID: r.ID}); err != nil {
			return err
		}
		return svc.changes.Record(ctx, tx, r.SpaceID, "schedule", r.ID, "updated", 0)
	})
}

// advance moves the schedule to its next occurrence (unless it was edited meanwhile).
func (svc *Service) advance(ctx context.Context, r *db.SpaceSchedule, next time.Time) error {
	return svc.s.W().TxNamed(ctx, "schedule.advance", func(tx *store.Tx) error {
		_, err := tx.Q.AdvanceSchedule(ctx, db.AdvanceScheduleParams{NextAt: nullTime(next), ID: r.ID, ExpectedNext: r.NextAt})
		return err
	})
}
