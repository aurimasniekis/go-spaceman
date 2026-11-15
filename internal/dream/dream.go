// Package dream runs the dreamer (SM-DREAM-*, ADR-0027): a normal agent run of kind dreamer on the space's node that
// consolidates role memory from evidence (new transcript segments, the memories themselves, KB answers) under time
// and evidence budgets. The node prepares the dream's workspace; the dreamer reports through its tools; only then
// does central ask the node to checkpoint the dream's memory changes and, after that, advance its evidence positions.
// A dream that changes nothing is a success; one that ends without a report, or runs out of time, advances nothing.
package dream

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

var (
	reSlug = regexp.MustCompile(`^[a-z][a-z0-9-]{1,30}$`)
	reRole = regexp.MustCompile(`^(\*|[a-z][a-z0-9-]{0,47})$`)
	system = audit.Actor{Type: audit.ActorSystem, ID: "dreamer"}

	errNotFound = apperr.New(apperr.CodeNotFound, "dream configuration not found")
)

// Config is a dream configuration.
type Config struct {
	ID              string     `json:"id"`
	SpaceID         string     `json:"space_id"`
	Slug            string     `json:"slug"`
	Name            string     `json:"name"`
	SpaceRole       string     `json:"space_role" doc:"A configured space role whose template is of kind dreamer"`
	MemoryRoles     []string   `json:"memory_roles" doc:"Role memories the dream consolidates; * for every role"`
	TranscriptRoles []string   `json:"transcript_roles" doc:"Roles whose transcripts it may read; * for every role"`
	IncludeOwn      bool       `json:"include_own" doc:"Also read the dreamer's own transcripts (off by default)"`
	Instructions    string     `json:"instructions,omitempty"`
	IntervalHours   int64      `json:"interval_hours" doc:"0: manual only"`
	MaxMinutes      int64      `json:"max_minutes"`
	MaxTokens       int64      `json:"max_tokens" doc:"Given to the dreamer as its budget; not enforced by SpaceMan"`
	MaxFiles        int64      `json:"max_files" doc:"Transcript files offered per dream"`
	MaxBytes        int64      `json:"max_bytes" doc:"Transcript bytes offered per dream"`
	Enabled         bool       `json:"enabled"`
	NextDueAt       *time.Time `json:"next_due_at,omitempty"`
	LastRun         *Run       `json:"last_run,omitempty"`
	Version         int64      `json:"version"`
}

// Input creates or edits a configuration.
type Input struct {
	Slug            string   `json:"slug,omitempty" maxLength:"31"`
	Name            string   `json:"name" maxLength:"120"`
	SpaceRole       string   `json:"space_role" maxLength:"48"`
	MemoryRoles     []string `json:"memory_roles,omitempty" maxItems:"50"`
	TranscriptRoles []string `json:"transcript_roles,omitempty" maxItems:"50"`
	IncludeOwn      bool     `json:"include_own,omitempty"`
	Instructions    string   `json:"instructions,omitempty" maxLength:"20000"`
	IntervalHours   int64    `json:"interval_hours,omitempty" doc:"0-720; 0 runs only manually"`
	MaxMinutes      int64    `json:"max_minutes,omitempty" doc:"5-240; default 30"`
	MaxTokens       int64    `json:"max_tokens,omitempty"`
	MaxFiles        int64    `json:"max_files,omitempty" doc:"1-200; default 20"`
	MaxBytes        int64    `json:"max_bytes,omitempty" doc:"64 KiB-64 MiB; default 2 MiB"`
	Enabled         *bool    `json:"enabled,omitempty"`
}

// Finding is something the dream wants people or control to know.
type Finding struct {
	Kind     string `json:"kind" enum:"contradiction,instruction_conflict,stale,insight,question" doc:"What sort of finding"`
	Summary  string `json:"summary" maxLength:"2000"`
	Evidence string `json:"evidence,omitempty" maxLength:"2000" doc:"Where it comes from (files, transcript lines)"`
}

// Run is one dream.
type Run struct {
	ID        string     `json:"id"`
	RunID     string     `json:"run_id,omitempty"`
	Trigger   string     `json:"trigger" enum:"manual,schedule"`
	State     string     `json:"state" enum:"running,reported,succeeded,failed,timed_out"`
	Summary   string     `json:"summary,omitempty"`
	Findings  []Finding  `json:"findings"`
	NoChange  bool       `json:"no_change"`
	Detail    string     `json:"detail,omitempty"`
	StartedAt time.Time  `json:"started_at"`
	Deadline  time.Time  `json:"deadline_at"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
}

// Service runs dreams.
type Service struct {
	s       *store.Store
	audit   *audit.Writer
	changes *changefeed.Recorder
	fleet   *fleet.Service
	clock   clock.Clock
	log     *slog.Logger

	kick chan struct{}
	mu   sync.Mutex
	base context.Context
	// stops are runs of finished dreams to stop (outside the transaction that finished them).
	stopMu sync.Mutex
	stops  []string
}

// New returns the service.
func New(s *store.Store, aw *audit.Writer, ch *changefeed.Recorder, fl *fleet.Service, c clock.Clock, log *slog.Logger) *Service {
	if c == nil {
		c = clock.System{}
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Service{s: s, audit: aw, changes: ch, fleet: fl, clock: c, log: log, kick: make(chan struct{}, 1), base: context.Background()}
}

// Kick wakes the scheduler.
func (svc *Service) Kick() {
	select {
	case svc.kick <- struct{}{}:
	default:
	}
}

func listJSON(l []string) string {
	if l == nil {
		l = []string{}
	}
	b, _ := json.Marshal(l)
	return string(b)
}

func parseList(s string) []string {
	out := []string{}
	_ = json.Unmarshal([]byte(s), &out)
	return out
}

func b2i(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func micros(t sql.NullInt64) *time.Time {
	if !t.Valid {
		return nil
	}
	x := time.UnixMicro(t.Int64).UTC()
	return &x
}

func configOut(r db.DreamConfig) *Config {
	return &Config{ID: r.ID, SpaceID: r.SpaceID, Slug: r.Slug, Name: r.Name, SpaceRole: r.SpaceRole, MemoryRoles: parseList(r.MemoryRolesJson),
		TranscriptRoles: parseList(r.TranscriptRolesJson), IncludeOwn: r.IncludeOwn == 1, Instructions: r.Instructions, IntervalHours: r.IntervalHours,
		MaxMinutes: r.MaxMinutes, MaxTokens: r.MaxTokens, MaxFiles: r.MaxFiles, MaxBytes: r.MaxBytes, Enabled: r.Enabled == 1, NextDueAt: micros(r.NextDueAt),
		Version: r.Version}
}

func runOut(r db.DreamRun) *Run {
	f := []Finding{}
	_ = json.Unmarshal([]byte(r.FindingsJson), &f)
	return &Run{ID: r.ID, RunID: r.RunID, Trigger: r.TriggerKind, State: r.State, Summary: r.Summary, Findings: f, NoChange: r.NoChange == 1, Detail: r.Detail,
		StartedAt: time.UnixMicro(r.StartedAt).UTC(), Deadline: time.UnixMicro(r.DeadlineAt).UTC(), EndedAt: micros(r.EndedAt)}
}

func (in *Input) normalize(ctx context.Context, q *db.Queries, spaceID string, create bool) error {
	v := &apperr.Validation{}
	in.Name = strings.TrimSpace(in.Name)
	if create && !reSlug.MatchString(in.Slug) {
		v.Add("slug", "must be 2-31 lowercase letters, digits or dashes")
	}
	if in.Name == "" {
		v.Add("name", "is required")
	}
	if len(in.MemoryRoles) == 0 {
		in.MemoryRoles = []string{"*"}
	}
	if len(in.TranscriptRoles) == 0 {
		in.TranscriptRoles = []string{"*"}
	}
	for _, r := range append(append([]string{}, in.MemoryRoles...), in.TranscriptRoles...) {
		if !reRole.MatchString(r) {
			v.Add("roles", "%q is not a role name", r)
		}
	}
	def := func(p *int64, d, lo, hi int64, field string) {
		if *p == 0 {
			*p = d
		}
		if *p < lo || *p > hi {
			v.Add(field, "must be between %d and %d", lo, hi)
		}
	}
	if in.IntervalHours < 0 || in.IntervalHours > 720 {
		v.Add("interval_hours", "must be between 0 and 720")
	}
	def(&in.MaxMinutes, 30, 5, 240, "max_minutes")
	def(&in.MaxFiles, 20, 1, 200, "max_files")
	def(&in.MaxBytes, 2<<20, 64<<10, 64<<20, "max_bytes")
	if in.MaxTokens < 0 {
		v.Add("max_tokens", "must not be negative")
	}
	role, err := q.GetSpaceRole(ctx, db.GetSpaceRoleParams{SpaceID: spaceID, Name: in.SpaceRole})
	if err != nil {
		v.Add("space_role", "must be a role configured in this space")
	} else if tpl, err := q.GetRoleTemplate(ctx, role.TemplateID); err != nil || tpl.Kind != "dreamer" {
		v.Add("space_role", "must be a role whose template is of kind dreamer")
	}
	return v.Err()
}

func (svc *Service) nextDue(hours int64, now time.Time) sql.NullInt64 {
	if hours <= 0 {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: now.Add(time.Duration(hours) * time.Hour).UnixMicro(), Valid: true}
}

// Create adds a configuration.
func (svc *Service) Create(ctx context.Context, actor audit.Actor, spaceID string, in Input) (*Config, error) {
	if err := in.normalize(ctx, svc.s.R(), spaceID, true); err != nil {
		return nil, err
	}
	en := in.Enabled == nil || *in.Enabled
	id := ids.New(ids.Dream)
	err := svc.s.W().TxNamed(ctx, "dream.create", func(tx *store.Tx) error {
		if err := tx.Q.InsertDreamConfig(ctx, db.InsertDreamConfigParams{ID: id, SpaceID: spaceID, Slug: in.Slug, Name: in.Name, SpaceRole: in.SpaceRole,
			InstanceID: ids.New(ids.Instance), MemoryRolesJson: listJSON(in.MemoryRoles), TranscriptRolesJson: listJSON(in.TranscriptRoles),
			IncludeOwn: b2i(in.IncludeOwn), Instructions: in.Instructions, IntervalHours: in.IntervalHours, MaxMinutes: in.MaxMinutes, MaxTokens: in.MaxTokens,
			MaxFiles: in.MaxFiles, MaxBytes: in.MaxBytes, Enabled: b2i(en), NextDueAt: svc.nextDue(in.IntervalHours, tx.Now), CreatedAt: tx.NowMicros(),
			CreatedBy: actor.ID, UpdatedAt: tx.NowMicros()}); err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				return apperr.New(apperr.CodeAlreadyExists, "a dream %q exists in this space", in.Slug)
			}
			return err
		}
		if err := svc.changes.Record(ctx, tx, spaceID, "dream", id, "created", 1); err != nil {
			return err
		}
		_, err := svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "dream.created", Target: audit.Target{Type: "dream", ID: id},
			Details: map[string]any{"space_id": spaceID, "slug": in.Slug, "memory_roles": in.MemoryRoles, "transcript_roles": in.TranscriptRoles}})
		return err
	})
	if err != nil {
		return nil, err
	}
	svc.Kick()
	return svc.Get(ctx, spaceID, id)
}

// Update edits a configuration.
func (svc *Service) Update(ctx context.Context, actor audit.Actor, spaceID, id string, version int64, in Input) (*Config, error) {
	if err := in.normalize(ctx, svc.s.R(), spaceID, false); err != nil {
		return nil, err
	}
	err := svc.s.W().TxNamed(ctx, "dream.update", func(tx *store.Tx) error {
		cur, err := tx.Q.GetDreamConfig(ctx, id)
		if err != nil || cur.SpaceID != spaceID {
			return errNotFound
		}
		en := cur.Enabled == 1
		if in.Enabled != nil {
			en = *in.Enabled
		}
		next := cur.NextDueAt
		if in.IntervalHours != cur.IntervalHours || !next.Valid {
			next = svc.nextDue(in.IntervalHours, tx.Now)
		}
		n, err := tx.Q.UpdateDreamConfig(ctx, db.UpdateDreamConfigParams{Name: in.Name, SpaceRole: in.SpaceRole, MemoryRolesJson: listJSON(in.MemoryRoles),
			TranscriptRolesJson: listJSON(in.TranscriptRoles), IncludeOwn: b2i(in.IncludeOwn), Instructions: in.Instructions, IntervalHours: in.IntervalHours,
			MaxMinutes: in.MaxMinutes, MaxTokens: in.MaxTokens, MaxFiles: in.MaxFiles, MaxBytes: in.MaxBytes, Enabled: b2i(en), NextDueAt: next,
			Now: tx.NowMicros(), ID: id, Version: version})
		if err != nil {
			return err
		}
		if n == 0 {
			return apperr.New(apperr.CodePrecondition, "the dream configuration changed since version %d", version)
		}
		if err := svc.changes.Record(ctx, tx, spaceID, "dream", id, "updated", version+1); err != nil {
			return err
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "dream.updated", Target: audit.Target{Type: "dream", ID: id},
			Details: map[string]any{"space_id": spaceID, "enabled": en, "interval_hours": in.IntervalHours}})
		return err
	})
	if err != nil {
		return nil, err
	}
	svc.Kick()
	return svc.Get(ctx, spaceID, id)
}

// Get returns a configuration with its last run.
func (svc *Service) Get(ctx context.Context, spaceID, id string) (*Config, error) {
	r, err := svc.s.R().GetDreamConfig(ctx, id)
	if err != nil || r.SpaceID != spaceID {
		return nil, errNotFound
	}
	c := configOut(r)
	if runs, err := svc.s.R().ListDreamRuns(ctx, db.ListDreamRunsParams{ConfigID: id, Limit: 1}); err == nil && len(runs) == 1 {
		c.LastRun = runOut(runs[0])
	}
	return c, nil
}

// List returns a space's configurations.
func (svc *Service) List(ctx context.Context, spaceID string) ([]*Config, error) {
	rows, err := svc.s.R().ListDreamConfigs(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	out := make([]*Config, 0, len(rows))
	for _, r := range rows {
		c, err := svc.Get(ctx, spaceID, r.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// Runs lists a configuration's recent dreams.
func (svc *Service) Runs(ctx context.Context, spaceID, id string) ([]*Run, error) {
	if _, err := svc.Get(ctx, spaceID, id); err != nil {
		return nil, err
	}
	rows, err := svc.s.R().ListDreamRuns(ctx, db.ListDreamRunsParams{ConfigID: id, Limit: 50})
	if err != nil {
		return nil, err
	}
	out := make([]*Run, 0, len(rows))
	for _, r := range rows {
		out = append(out, runOut(r))
	}
	return out, nil
}

// Start begins a dream now (manual), unless one of this configuration is still going.
func (svc *Service) Start(ctx context.Context, actor audit.Actor, spaceID, id string) (*Run, error) {
	c, err := svc.s.R().GetDreamConfig(ctx, id)
	if err != nil || c.SpaceID != spaceID {
		return nil, errNotFound
	}
	if c.Enabled != 1 {
		return nil, apperr.New(apperr.CodePrecondition, "the dream is disabled")
	}
	return svc.start(ctx, actor, c, "manual")
}

// prompt is the dream's task: sources, budgets and how to report. The method lives in the dreamer role's prompt.
func prompt(c db.DreamConfig) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Dream %s (%s). Your workspace holds DREAM.md: read it first. It lists the role memories you may consolidate (folders under memory/) "+
		"and the new transcript segments you may read (under evidence/).\n\n", c.Name, c.Slug)
	fmt.Fprintf(&b, "Budget: finish within %d minutes", c.MaxMinutes)
	if c.MaxTokens > 0 {
		fmt.Fprintf(&b, " and about %d tokens", c.MaxTokens)
	}
	b.WriteString(". Read evidence narrowly (search, do not read whole files).\n\n")
	b.WriteString("When you are done, call spaceman_dream_report once: a summary, what you changed in memory, and findings (contradictions, instruction ")
	b.WriteString("conflicts, stale facts, questions). Send findings that need action with spaceman_message_send (control), spaceman_human_ask (a person) ")
	b.WriteString("or spaceman_kb_ask (a knowledge base). A dream that changes nothing is a good dream: report no_change.\n")
	if strings.TrimSpace(c.Instructions) != "" {
		b.WriteString("\n## Instructions for this dream\n\n" + c.Instructions + "\n")
	}
	return b.String()
}

func (svc *Service) start(ctx context.Context, actor audit.Actor, c db.DreamConfig, trigger string) (*Run, error) {
	if n, err := svc.s.R().CountActiveDreamRuns(ctx, c.ID); err != nil {
		return nil, err
	} else if n > 0 {
		return nil, apperr.New(apperr.CodeConflict, "a dream of this configuration is still running")
	}
	exclude := []string{}
	if c.IncludeOwn != 1 {
		exclude = append(exclude, c.SpaceRole)
	}
	spec := &proto.DreamSpec{ConfigID: c.ID, MemoryRoles: parseList(c.MemoryRolesJson), TranscriptRoles: parseList(c.TranscriptRolesJson), ExcludeRoles: exclude,
		MaxFiles: int(c.MaxFiles), MaxBytes: c.MaxBytes}
	// The space role decides the kind: anything but dreamer is refused before a run exists (the check after launch
	// below stays for a launcher without the hook).
	notDreamer := apperr.New(apperr.CodePrecondition, "the space role %s does not run as kind dreamer", c.SpaceRole)
	run, err := svc.fleet.LaunchRun(ctx, actor, c.SpaceID, fleet.LaunchInput{Role: c.SpaceRole, InstanceID: c.InstanceID, Prompt: prompt(c), Dream: spec,
		AuthorizeKind: func(kind string) error {
			if kind != "dreamer" {
				return notDreamer
			}
			return nil
		}})
	if err != nil {
		return nil, err
	}
	if run.Kind != "dreamer" {
		_, _ = svc.fleet.StopRun(ctx, system, run.ID, "kill", "the dream's space role is not of kind dreamer")
		return nil, notDreamer
	}
	id := ids.New("drr")
	err = svc.s.W().TxNamed(ctx, "dream.start", func(tx *store.Tx) error {
		now := tx.NowMicros()
		if err := tx.Q.InsertDreamRun(ctx, db.InsertDreamRunParams{ID: id, ConfigID: c.ID, SpaceID: c.SpaceID, RunID: run.ID, TriggerKind: trigger, StartedAt: now,
			DeadlineAt: now + c.MaxMinutes*60_000_000, UpdatedAt: now}); err != nil {
			return err
		}
		if err := tx.Q.SetDreamNextDue(ctx, db.SetDreamNextDueParams{NextDueAt: svc.nextDue(c.IntervalHours, tx.Now), UpdatedAt: now, ID: c.ID}); err != nil {
			return err
		}
		if err := svc.changes.Record(ctx, tx, c.SpaceID, "dream", c.ID, "updated", 0); err != nil {
			return err
		}
		_, err := svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "dream.started", Target: audit.Target{Type: "dream", ID: c.ID},
			Details: map[string]any{"space_id": c.SpaceID, "dream_run": id, "run": run.ID, "trigger": trigger}})
		return err
	})
	if err != nil {
		return nil, err
	}
	svc.Kick()
	r, err := svc.s.R().GetDreamRun(ctx, id)
	if err != nil {
		return nil, err
	}
	return runOut(r), nil
}

// ReportInput is the dreamer's report.
type ReportInput struct {
	Summary  string    `json:"summary" maxLength:"20000" doc:"What the dream looked at and concluded (Markdown)"`
	NoChange bool      `json:"no_change" doc:"Nothing needed changing (a successful dream)"`
	Changes  []string  `json:"changes,omitempty" maxItems:"100" doc:"Memory files changed, one line each"`
	Findings []Finding `json:"findings,omitempty" maxItems:"50"`
}

// Report records the dreamer's report and asks the node to checkpoint its memory and then advance its evidence.
func (svc *Service) Report(ctx context.Context, p *fleet.RunPrincipal, in ReportInput) (map[string]any, error) {
	dr, err := svc.s.R().GetDreamRunByRun(ctx, p.RunID)
	if err != nil {
		return nil, apperr.New(apperr.CodeForbidden, "this run is not a dream")
	}
	if dr.State != "running" {
		return nil, apperr.New(apperr.CodePrecondition, "the dream is already %s", dr.State)
	}
	v := &apperr.Validation{}
	if strings.TrimSpace(in.Summary) == "" {
		v.Add("summary", "is required")
	}
	kinds := []string{"contradiction", "instruction_conflict", "stale", "insight", "question"}
	for _, f := range in.Findings {
		if !slices.Contains(kinds, f.Kind) || strings.TrimSpace(f.Summary) == "" {
			v.Add("findings", "each finding has a kind (%s) and a summary", strings.Join(kinds, ", "))
			break
		}
	}
	if err := v.Err(); err != nil {
		return nil, err
	}
	summary := in.Summary
	if len(in.Changes) > 0 {
		summary += "\n\nChanged:\n- " + strings.Join(in.Changes, "\n- ")
	}
	f, _ := json.Marshal(nonNil(in.Findings))
	err = svc.s.W().TxNamed(ctx, "dream.report", func(tx *store.Tx) error {
		cmd, err := svc.fleet.QueueSpaceCommandForRun(ctx, tx, dr.SpaceID, p.RunID, proto.CmdDreamDone, "dream-commit:"+dr.ID, struct{}{})
		if err != nil {
			return err
		}
		n, err := tx.Q.ReportDreamRun(ctx, db.ReportDreamRunParams{Summary: summary, FindingsJson: string(f), NoChange: b2i(in.NoChange), CommitCommandID: cmd,
			UpdatedAt: tx.NowMicros(), ID: dr.ID})
		if err != nil {
			return err
		}
		if n == 0 {
			return apperr.New(apperr.CodeConflict, "the dream changed concurrently")
		}
		if err := svc.changes.Record(ctx, tx, dr.SpaceID, "dream", dr.ConfigID, "updated", 0); err != nil {
			return err
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: audit.Actor{Type: audit.ActorAgent, ID: p.RunID}, Action: "dream.reported",
			Target: audit.Target{Type: "dream", ID: dr.ConfigID}, Details: map[string]any{"space_id": dr.SpaceID, "dream_run": dr.ID, "no_change": in.NoChange,
				"findings": len(in.Findings)}})
		return err
	})
	if err != nil {
		return nil, err
	}
	svc.fleet.KickSpace(ctx, dr.SpaceID)
	return map[string]any{"recorded": true, "note": "Your memory changes are being checkpointed; you can stop now."}, nil
}

func nonNil(f []Finding) []Finding {
	if f == nil {
		return []Finding{}
	}
	return f
}

// OnCommitAck finishes a dream when the node checkpointed its memory and advanced its evidence (inside the ack's
// transaction).
func (svc *Service) OnCommitAck(ctx context.Context, tx *store.Tx, commandID, result, reason string) error {
	dr, err := tx.Q.GetDreamRunByCommand(ctx, commandID)
	if err != nil {
		return nil //nolint:nilerr // not a dream command
	}
	state, detail := "succeeded", ""
	if result != proto.AckAccepted && result != proto.AckDuplicate {
		state, detail = "failed", "the node could not checkpoint the dream: "+reason
	}
	if _, err := tx.Q.EndDreamRun(ctx, db.EndDreamRunParams{State: state, Detail: detail, Now: sql.NullInt64{Int64: tx.NowMicros(), Valid: true}, ID: dr.ID}); err != nil {
		return err
	}
	if err := svc.changes.Record(ctx, tx, dr.SpaceID, "dream", dr.ConfigID, "updated", 0); err != nil {
		return err
	}
	_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: system, Action: "dream." + state, Target: audit.Target{Type: "dream", ID: dr.ConfigID},
		Details: map[string]any{"space_id": dr.SpaceID, "dream_run": dr.ID, "detail": detail}})
	if dr.RunID != "" {
		svc.stopMu.Lock()
		svc.stops = append(svc.stops, dr.RunID)
		svc.stopMu.Unlock()
	}
	svc.Kick()
	return err
}

// OnRunEnded fails a dream whose run ended before it reported (nothing is advanced).
func (svc *Service) OnRunEnded(runID string) {
	svc.do(func(ctx context.Context) {
		dr, err := svc.s.R().GetDreamRunByRun(ctx, runID)
		if err != nil || dr.State != "running" {
			return
		}
		svc.end(ctx, dr, "failed", "the dreamer ended without reporting; its evidence was not advanced")
	})
}

func (svc *Service) do(f func(ctx context.Context)) { f(svc.base) }

func (svc *Service) end(ctx context.Context, dr db.DreamRun, state, detail string) {
	_ = svc.s.W().TxNamed(ctx, "dream.end", func(tx *store.Tx) error {
		n, err := tx.Q.EndDreamRun(ctx, db.EndDreamRunParams{State: state, Detail: detail, Now: sql.NullInt64{Int64: tx.NowMicros(), Valid: true}, ID: dr.ID})
		if err != nil || n == 0 {
			return err
		}
		if err := svc.changes.Record(ctx, tx, dr.SpaceID, "dream", dr.ConfigID, "updated", 0); err != nil {
			return err
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: system, Action: "dream." + state, Target: audit.Target{Type: "dream", ID: dr.ConfigID},
			Details: map[string]any{"space_id": dr.SpaceID, "dream_run": dr.ID, "detail": detail}})
		return err
	})
}

// Run schedules dreams and enforces their time budgets until ctx ends (one goroutine, one timer).
func (svc *Service) Run(ctx context.Context) {
	svc.base = context.WithoutCancel(ctx)
	for {
		next := svc.Step(ctx)
		t := time.NewTimer(next)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-svc.kick:
		case <-t.C:
		}
		t.Stop()
	}
}

// Step starts due dreams, stops dreams past their deadline and stops finished dreams' runs; it returns when to look
// again.
func (svc *Service) Step(ctx context.Context) time.Duration {
	svc.mu.Lock()
	defer svc.mu.Unlock()
	now := svc.clock.Now()
	next := time.Hour
	active, _ := svc.s.R().ListActiveDreamRuns(ctx)
	for _, dr := range active {
		deadline := time.UnixMicro(dr.DeadlineAt)
		if now.Before(deadline) {
			if d := deadline.Sub(now); d < next {
				next = d
			}
			continue
		}
		svc.end(ctx, dr, "timed_out", "the dream ran past its time budget; its evidence was not advanced")
		if dr.RunID != "" {
			_, _ = svc.fleet.StopRun(ctx, system, dr.RunID, "kill", "dream time budget")
		}
	}
	due, _ := svc.s.R().ListDueDreamConfigs(ctx)
	for _, c := range due {
		at := time.UnixMicro(c.NextDueAt.Int64)
		if now.Before(at) {
			if d := at.Sub(now); d < next {
				next = d
			}
			continue
		}
		if _, err := svc.start(ctx, system, c, "schedule"); err != nil {
			svc.log.Warn("scheduled dream", slog.String("dream", c.Slug), slog.Any("error", err))
			_ = svc.s.W().TxNamed(ctx, "dream.reschedule", func(tx *store.Tx) error {
				return tx.Q.SetDreamNextDue(ctx, db.SetDreamNextDueParams{NextDueAt: sql.NullInt64{Int64: tx.Now.Add(15 * time.Minute).UnixMicro(), Valid: true},
					UpdatedAt: tx.NowMicros(), ID: c.ID})
			})
		}
	}
	// A dream that finished stops its run (the dreamer was told it can stop).
	svc.stopMu.Lock()
	stops := svc.stops
	svc.stops = nil
	svc.stopMu.Unlock()
	for _, id := range stops {
		_, _ = svc.fleet.StopRun(ctx, system, id, "kill", "dream finished")
	}
	if next < time.Second {
		next = time.Second
	}
	return next
}
