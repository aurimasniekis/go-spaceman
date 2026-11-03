// Package kb runs knowledge-base support (SM-KB-*, SM-CTRL-005, ADR-0026). A KB is a Git repository in a space with
// its own instructions, write policy and limits. Agents ask it questions through their tools; each requesting agent
// instance gets its own support session: a kb run with its own conversation, worktree and branch, so one busy
// session never holds up the others and a requester keeps its context. Requests are durable, correlated and answered
// asynchronously (nothing waits in an MCP call). Sessions are started within the KB's limits, put to sleep when idle,
// recovered when they fail and closed when old. KB sessions write only to their own branch; a proposal reaches the
// KB's base branch through review or serialized acceptance on the node, never by force.
//
// The KB repository, role memory and the support conversation are separate things. A KB session cannot change
// policy or start workers: its tools are answering and proposing.
package kb

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path"
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
	"github.com/aurimasniekis/go-spaceman/internal/messages"
	"github.com/aurimasniekis/go-spaceman/internal/outbox"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Request kinds (SM-KB-002).
var Kinds = []string{"context", "decision", "discovery", "proposal", "documentation", "consistency"}

// Write policies.
const (
	ReadOnly   = "read_only"
	Review     = "review"
	Serialized = "serialized"
)

var (
	reSlug   = regexp.MustCompile(`^[a-z][a-z0-9-]{1,30}$`)
	reRole   = regexp.MustCompile(`^(\*|[a-z][a-z0-9-]{0,47})$`)
	reBranch = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,99}$`)
	reCommit = regexp.MustCompile(`^[0-9a-f]{40,64}$`)
	reRev    = regexp.MustCompile(`^[0-9a-f]{7,64}$`)
	system   = audit.Actor{Type: audit.ActorSystem, ID: "kb"}

	errNotFound = apperr.New(apperr.CodeNotFound, "knowledge base not found")
)

// KB is a configured knowledge base.
type KB struct {
	ID                string    `json:"id"`
	SpaceID           string    `json:"space_id"`
	Slug              string    `json:"slug"`
	Name              string    `json:"name"`
	Description       string    `json:"description,omitempty"`
	Repo              string    `json:"repo" doc:"Repository folder inside the space (a configured space repository)"`
	BaseBranch        string    `json:"base_branch" doc:"Where accepted knowledge lives"`
	SpaceRole         string    `json:"space_role" doc:"The configured space role (kind kb) sessions run as"`
	EntryInstructions string    `json:"entry_instructions,omitempty"`
	Validator         []string  `json:"validator" doc:"Command run on a proposal before it is accepted, e.g. [python3, tools/kb.py, check]"`
	WritePolicy       string    `json:"write_policy" enum:"read_only,review,serialized"`
	AllowedRoles      []string  `json:"allowed_roles" doc:"Requester roles admitted to support sessions; * admits every role"`
	MaxSessions       int64     `json:"max_sessions" doc:"Sessions running at once; further requests queue"`
	IdleSleepSeconds  int64     `json:"idle_sleep_seconds"`
	RequestTimeout    int64     `json:"request_timeout_seconds"`
	SessionTTLSeconds int64     `json:"session_ttl_seconds"`
	Enabled           bool      `json:"enabled"`
	Stats             *Stats    `json:"stats,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
	Version           int64     `json:"version"`
}

// Stats are a KB's capacity and usage.
type Stats struct {
	ActiveSessions int64 `json:"active_sessions"`
	Sessions       int64 `json:"sessions"`
	OpenRequests   int64 `json:"open_requests"`
	Answered       int64 `json:"answered"`
	Failed         int64 `json:"failed"`
	Proposals      int64 `json:"pending_proposals"`
}

// Input creates or edits a KB.
type Input struct {
	Slug              string   `json:"slug,omitempty" maxLength:"31" doc:"Fixed after creation"`
	Name              string   `json:"name" maxLength:"120"`
	Description       string   `json:"description,omitempty" maxLength:"2000"`
	Repo              string   `json:"repo" maxLength:"256" doc:"The folder of a configured space repository, or another folder inside the space, which is then added to the space's repositories (role kb, clone policy manual)"`
	BaseBranch        string   `json:"base_branch,omitempty" maxLength:"100"`
	SpaceRole         string   `json:"space_role" maxLength:"48"`
	EntryInstructions string   `json:"entry_instructions,omitempty" maxLength:"20000"`
	Validator         []string `json:"validator,omitempty" maxItems:"20"`
	WritePolicy       string   `json:"write_policy,omitempty" enum:"read_only,review,serialized"`
	AllowedRoles      []string `json:"allowed_roles,omitempty" maxItems:"50"`
	MaxSessions       int64    `json:"max_sessions,omitempty" doc:"1-16; default 2"`
	IdleSleepSeconds  int64    `json:"idle_sleep_seconds,omitempty" doc:"60-86400; default 900"`
	RequestTimeout    int64    `json:"request_timeout_seconds,omitempty" doc:"60-86400; default 1800"`
	SessionTTLSeconds int64    `json:"session_ttl_seconds,omitempty" doc:"3600-2592000; default 604800"`
	Enabled           *bool    `json:"enabled,omitempty"`
}

// Session is one requester's support session.
type Session struct {
	ID             string    `json:"id"`
	RequesterKey   string    `json:"requester_instance"`
	RequesterRole  string    `json:"requester_role"`
	RequesterLabel string    `json:"requester_label,omitempty"`
	InstanceID     string    `json:"instance_id"`
	Branch         string    `json:"branch"`
	RunID          string    `json:"run_id,omitempty"`
	State          string    `json:"state" enum:"new,active,sleeping,failed,closed"`
	Failures       int64     `json:"failures"`
	LastError      string    `json:"last_error,omitempty"`
	Answered       int64     `json:"answered"`
	LastActiveAt   time.Time `json:"last_active_at"`
	CreatedAt      time.Time `json:"created_at"`
}

// Evidence points an answer at its sources.
type Evidence struct {
	Path     string `json:"path" maxLength:"500"`
	Revision string `json:"revision,omitempty" maxLength:"64" doc:"Commit the cited text is from"`
	Kind     string `json:"kind,omitempty" enum:"committed,proposal" doc:"Accepted knowledge, or a proposal not yet accepted"`
}

// Request is one question to a KB.
type Request struct {
	ID            string     `json:"id"`
	KBID          string     `json:"kb_id"`
	SessionID     string     `json:"session_id"`
	RequesterRun  string     `json:"requester_run"`
	RequesterRole string     `json:"requester_role"`
	Kind          string     `json:"kind"`
	Question      string     `json:"question"`
	TaskID        string     `json:"task_id,omitempty"`
	State         string     `json:"state" enum:"queued,dispatched,answered,failed,cancelled,timed_out"`
	Answer        string     `json:"answer,omitempty"`
	Basis         string     `json:"basis,omitempty" doc:"committed: from accepted knowledge; proposal: from proposed changes; none: the KB does not know"`
	Evidence      []Evidence `json:"evidence"`
	Attempts      int64      `json:"attempts"`
	Error         string     `json:"error,omitempty"`
	DeadlineAt    time.Time  `json:"deadline_at"`
	CreatedAt     time.Time  `json:"created_at"`
	AnsweredAt    *time.Time `json:"answered_at,omitempty"`
}

// Proposal is a change a KB session offers for acceptance.
type Proposal struct {
	ID        string    `json:"id"`
	SessionID string    `json:"session_id"`
	RequestID string    `json:"request_id,omitempty"`
	Title     string    `json:"title"`
	Summary   string    `json:"summary,omitempty"`
	Branch    string    `json:"branch"`
	Commit    string    `json:"commit"`
	State     string    `json:"state" enum:"proposed,accepting,accepted,rejected,conflict,failed"`
	Merged    string    `json:"merged_commit,omitempty"`
	Detail    string    `json:"detail,omitempty"`
	DecidedBy string    `json:"decided_by,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Service runs KB support.
type Service struct {
	s       *store.Store
	audit   *audit.Writer
	changes *changefeed.Recorder
	fleet   *fleet.Service
	msgs    *messages.Service
	clock   clock.Clock
	log     *slog.Logger

	kick chan struct{}
	mu   sync.Mutex // serializes step

	// Outbox records accepted-proposal events for publication to Pulse (SM-OBS-004). Nil: nothing is published.
	Outbox *outbox.Outbox
	// SpaceLimits returns a space's effective KB limits (the limits model, ADR-0036): active support sessions across
	// its knowledge bases, and how deep support may nest. Nil: 8 sessions, depth 1.
	SpaceLimits func(ctx context.Context, q *db.Queries, spaceID string) (maxSessions, depth int64)
	// OnCrossSpace is told (after commit) that a KB request asked for another space finished (ADR-0038).
	OnCrossSpace func(ctx context.Context, kbRequestID string)
	// ResolveRepo resolves a KB's repository folder inside the create or update transaction (ADR-0044): a configured
	// space repository at that folder, or the folder registered as a new manual kb repository. It returns the folder.
	// Nil: the folder is used as given.
	ResolveRepo func(ctx context.Context, tx *store.Tx, actor audit.Actor, spaceID, repo, baseBranch string) (string, error)
	// base is the context of Run; hooks without a caller context use it.
	base context.Context
}

func (svc *Service) spaceLimits(ctx context.Context, q *db.Queries, spaceID string) (int64, int64) {
	if svc.SpaceLimits == nil {
		return 8, 1
	}
	return svc.SpaceLimits(ctx, q, spaceID)
}

// New returns the service.
func New(s *store.Store, aw *audit.Writer, ch *changefeed.Recorder, fl *fleet.Service, msgs *messages.Service, c clock.Clock, log *slog.Logger) *Service {
	if c == nil {
		c = clock.System{}
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Service{s: s, audit: aw, changes: ch, fleet: fl, msgs: msgs, clock: c, log: log, kick: make(chan struct{}, 1), base: context.Background()}
}

// Kick wakes the scheduler.
func (svc *Service) Kick() {
	select {
	case svc.kick <- struct{}{}:
	default:
	}
}

func b2i(b bool) int64 {
	if b {
		return 1
	}
	return 0
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

func micros(t sql.NullInt64) *time.Time {
	if !t.Valid {
		return nil
	}
	x := time.UnixMicro(t.Int64).UTC()
	return &x
}

func kbOut(r db.KbRepo) *KB {
	return &KB{ID: r.ID, SpaceID: r.SpaceID, Slug: r.Slug, Name: r.Name, Description: r.Description, Repo: r.Repo, BaseBranch: r.BaseBranch,
		SpaceRole: r.SpaceRole, EntryInstructions: r.EntryInstructions, Validator: parseList(r.ValidatorJson), WritePolicy: r.WritePolicy,
		AllowedRoles: parseList(r.AllowedRolesJson), MaxSessions: r.MaxSessions, IdleSleepSeconds: r.IdleSleepSeconds, RequestTimeout: r.RequestTimeoutSeconds,
		SessionTTLSeconds: r.SessionTtlSeconds, Enabled: r.Enabled == 1, CreatedAt: time.UnixMicro(r.CreatedAt).UTC(), UpdatedAt: time.UnixMicro(r.UpdatedAt).UTC(),
		Version: r.Version}
}

func (in *Input) normalize(ctx context.Context, q *db.Queries, spaceID string, create bool) error {
	v := &apperr.Validation{}
	in.Name = strings.TrimSpace(in.Name)
	if create && !reSlug.MatchString(in.Slug) {
		v.Add("slug", "must be 2-31 lowercase letters, digits or dashes, starting with a letter")
	}
	if in.Name == "" || len(in.Name) > 120 {
		v.Add("name", "is required")
	}
	clean := path.Clean(in.Repo)
	if in.Repo == "" || path.IsAbs(in.Repo) || clean != in.Repo || clean == "." || strings.HasPrefix(clean, "..") {
		v.Add("repo", "must be a relative folder inside the space")
	}
	if in.BaseBranch == "" {
		in.BaseBranch = "main"
	}
	if !reBranch.MatchString(in.BaseBranch) || strings.Contains(in.BaseBranch, "..") || strings.HasPrefix(in.BaseBranch, "kb/") {
		v.Add("base_branch", "is not a usable branch name (kb/ is reserved for sessions)")
	}
	if in.WritePolicy == "" {
		in.WritePolicy = Review
	}
	if !slices.Contains([]string{ReadOnly, Review, Serialized}, in.WritePolicy) {
		v.Add("write_policy", "must be read_only, review or serialized")
	}
	if len(in.Validator) > 0 && strings.TrimSpace(in.Validator[0]) == "" {
		v.Add("validator", "the first element is the program to run")
	}
	for _, a := range in.Validator {
		if len(a) > 1000 || strings.ContainsAny(a, "\x00\n") {
			v.Add("validator", "arguments are single lines of at most 1000 bytes")
		}
	}
	if len(in.AllowedRoles) == 0 {
		in.AllowedRoles = []string{"*"}
	}
	for _, r := range in.AllowedRoles {
		if !reRole.MatchString(r) {
			v.Add("allowed_roles", "%q is not a role name", r)
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
	def(&in.MaxSessions, 2, 1, 16, "max_sessions")
	def(&in.IdleSleepSeconds, 900, 60, 86400, "idle_sleep_seconds")
	def(&in.RequestTimeout, 1800, 60, 86400, "request_timeout_seconds")
	def(&in.SessionTTLSeconds, 7*86400, 3600, 30*86400, "session_ttl_seconds")
	// The sessions' role must be a configured space role whose template is of kind kb: KB sessions get the KB
	// tools and nothing that orchestrates workers.
	role, err := q.GetSpaceRole(ctx, db.GetSpaceRoleParams{SpaceID: spaceID, Name: in.SpaceRole})
	if err != nil {
		v.Add("space_role", "must be a role configured in this space")
	} else if tpl, err := q.GetRoleTemplate(ctx, role.TemplateID); err != nil || tpl.Kind != "kb" {
		v.Add("space_role", "must be a role whose template is of kind kb")
	}
	return v.Err()
}

// Create adds a KB.
func (svc *Service) Create(ctx context.Context, actor audit.Actor, spaceID string, in Input) (*KB, error) {
	if err := in.normalize(ctx, svc.s.R(), spaceID, true); err != nil {
		return nil, err
	}
	en := in.Enabled == nil || *in.Enabled
	id := ids.New(ids.KBRepo)
	err := svc.s.W().TxNamed(ctx, "kb.create", func(tx *store.Tx) error {
		if _, err := tx.Q.GetKBBySlug(ctx, db.GetKBBySlugParams{SpaceID: spaceID, Slug: in.Slug}); err == nil {
			return apperr.New(apperr.CodeAlreadyExists, "a knowledge base %q exists in this space", in.Slug)
		}
		if svc.ResolveRepo != nil {
			var err error
			if in.Repo, err = svc.ResolveRepo(ctx, tx, actor, spaceID, in.Repo, in.BaseBranch); err != nil {
				return err
			}
		}
		if err := tx.Q.InsertKB(ctx, db.InsertKBParams{ID: id, SpaceID: spaceID, Slug: in.Slug, Name: in.Name, Description: in.Description, Repo: in.Repo,
			BaseBranch: in.BaseBranch, SpaceRole: in.SpaceRole, EntryInstructions: in.EntryInstructions, ValidatorJson: listJSON(in.Validator),
			WritePolicy: in.WritePolicy, AllowedRolesJson: listJSON(in.AllowedRoles), MaxSessions: in.MaxSessions, IdleSleepSeconds: in.IdleSleepSeconds,
			RequestTimeoutSeconds: in.RequestTimeout, SessionTtlSeconds: in.SessionTTLSeconds, Enabled: b2i(en), CreatedAt: tx.NowMicros(),
			CreatedBy: actor.ID, UpdatedAt: tx.NowMicros()}); err != nil {
			return err
		}
		if err := svc.changes.Record(ctx, tx, spaceID, "kb", id, "created", 1); err != nil {
			return err
		}
		_, err := svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "kb.created", Target: audit.Target{Type: "kb", ID: id},
			Details: map[string]any{"space_id": spaceID, "slug": in.Slug, "repo": in.Repo, "write_policy": in.WritePolicy, "validator": in.Validator}})
		return err
	})
	if err != nil {
		return nil, err
	}
	return svc.Get(ctx, spaceID, id)
}

// Update edits a KB.
func (svc *Service) Update(ctx context.Context, actor audit.Actor, spaceID, id string, version int64, in Input) (*KB, error) {
	if err := in.normalize(ctx, svc.s.R(), spaceID, false); err != nil {
		return nil, err
	}
	err := svc.s.W().TxNamed(ctx, "kb.update", func(tx *store.Tx) error {
		cur, err := tx.Q.GetKB(ctx, id)
		if err != nil || cur.SpaceID != spaceID {
			return errNotFound
		}
		en := cur.Enabled == 1
		if in.Enabled != nil {
			en = *in.Enabled
		}
		if svc.ResolveRepo != nil {
			if in.Repo, err = svc.ResolveRepo(ctx, tx, actor, spaceID, in.Repo, in.BaseBranch); err != nil {
				return err
			}
		}
		n, err := tx.Q.UpdateKB(ctx, db.UpdateKBParams{Name: in.Name, Description: in.Description, Repo: in.Repo, BaseBranch: in.BaseBranch, SpaceRole: in.SpaceRole,
			EntryInstructions: in.EntryInstructions, ValidatorJson: listJSON(in.Validator), WritePolicy: in.WritePolicy, AllowedRolesJson: listJSON(in.AllowedRoles),
			MaxSessions: in.MaxSessions, IdleSleepSeconds: in.IdleSleepSeconds, RequestTimeoutSeconds: in.RequestTimeout, SessionTtlSeconds: in.SessionTTLSeconds,
			Enabled: b2i(en), Now: tx.NowMicros(), ID: id, Version: version})
		if err != nil {
			return err
		}
		if n == 0 {
			return apperr.New(apperr.CodePrecondition, "the knowledge base changed since version %d", version)
		}
		if err := svc.changes.Record(ctx, tx, spaceID, "kb", id, "updated", version+1); err != nil {
			return err
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "kb.updated", Target: audit.Target{Type: "kb", ID: id},
			Details: map[string]any{"space_id": spaceID, "repo": in.Repo, "write_policy": in.WritePolicy, "validator": in.Validator, "enabled": en}})
		return err
	})
	if err != nil {
		return nil, err
	}
	svc.Kick()
	return svc.Get(ctx, spaceID, id)
}

// Get returns a KB with its stats.
func (svc *Service) Get(ctx context.Context, spaceID, id string) (*KB, error) {
	r, err := svc.s.R().GetKB(ctx, id)
	if err != nil || r.SpaceID != spaceID {
		return nil, errNotFound
	}
	return svc.withStats(ctx, r)
}

func (svc *Service) withStats(ctx context.Context, r db.KbRepo) (*KB, error) {
	k := kbOut(r)
	act, err := svc.s.R().CountActiveKBSessions(ctx, r.ID)
	if err != nil {
		return nil, err
	}
	c, err := svc.s.R().CountKBRequests(ctx, r.ID)
	if err != nil {
		return nil, err
	}
	sess, _ := svc.s.R().ListKBSessions(ctx, r.ID)
	props, _ := svc.s.R().ListKBProposals(ctx, r.ID)
	pending := int64(0)
	for _, p := range props {
		if p.State == "proposed" || p.State == "accepting" {
			pending++
		}
	}
	k.Stats = &Stats{ActiveSessions: act, Sessions: int64(len(sess)), OpenRequests: toInt(c.Open), Answered: toInt(c.Answered), Failed: toInt(c.Failed), Proposals: pending}
	return k, nil
}

func toInt(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case float64:
		return int64(x)
	}
	return 0
}

// List returns a space's KBs.
func (svc *Service) List(ctx context.Context, spaceID string) ([]*KB, error) {
	rows, err := svc.s.R().ListKBs(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	out := make([]*KB, 0, len(rows))
	for _, r := range rows {
		k, err := svc.withStats(ctx, r)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, nil
}

func sessionOut(s db.KbSession) Session {
	return Session{ID: s.ID, RequesterKey: s.RequesterKey, RequesterRole: s.RequesterRole, RequesterLabel: s.RequesterLabel, InstanceID: s.InstanceID,
		Branch: s.Branch, RunID: s.RunID, State: s.State, Failures: s.Failures, LastError: s.LastError, Answered: s.Answered,
		LastActiveAt: time.UnixMicro(s.LastActiveAt).UTC(), CreatedAt: time.UnixMicro(s.CreatedAt).UTC()}
}

func requestOut(r db.KbRequest) *Request {
	ev := []Evidence{}
	_ = json.Unmarshal([]byte(r.EvidenceJson), &ev)
	return &Request{ID: r.ID, KBID: r.KbID, SessionID: r.SessionID, RequesterRun: r.RequesterRun, RequesterRole: r.RequesterRole, Kind: r.Kind,
		Question: r.Question, TaskID: r.TaskID, State: r.State, Answer: r.Answer, Basis: r.Basis, Evidence: ev, Attempts: r.Attempts, Error: r.Error,
		DeadlineAt: time.UnixMicro(r.DeadlineAt).UTC(), CreatedAt: time.UnixMicro(r.CreatedAt).UTC(), AnsweredAt: micros(r.AnsweredAt)}
}

func proposalOut(p db.KbProposal) Proposal {
	return Proposal{ID: p.ID, SessionID: p.SessionID, RequestID: p.RequestID, Title: p.Title, Summary: p.Summary, Branch: p.Branch, Commit: p.CommitID,
		State: p.State, Merged: p.MergedCommit, Detail: p.Detail, DecidedBy: p.DecidedBy, CreatedAt: time.UnixMicro(p.CreatedAt).UTC(),
		UpdatedAt: time.UnixMicro(p.UpdatedAt).UTC()}
}

// Detail is a KB with its sessions, recent requests and proposals (SM-KB-007).
type Detail struct {
	KB        *KB        `json:"kb"`
	Sessions  []Session  `json:"sessions"`
	Requests  []*Request `json:"requests"`
	Proposals []Proposal `json:"proposals"`
}

// Detail returns everything the UI shows about a KB.
func (svc *Service) Detail(ctx context.Context, spaceID, id string) (*Detail, error) {
	k, err := svc.Get(ctx, spaceID, id)
	if err != nil {
		return nil, err
	}
	d := &Detail{KB: k, Sessions: []Session{}, Requests: []*Request{}, Proposals: []Proposal{}}
	ss, err := svc.s.R().ListKBSessions(ctx, id)
	if err != nil {
		return nil, err
	}
	for _, s := range ss {
		d.Sessions = append(d.Sessions, sessionOut(s))
	}
	rs, err := svc.s.R().ListKBRequests(ctx, db.ListKBRequestsParams{KbID: id, Limit: 100})
	if err != nil {
		return nil, err
	}
	for _, r := range rs {
		d.Requests = append(d.Requests, requestOut(r))
	}
	ps, err := svc.s.R().ListKBProposals(ctx, id)
	if err != nil {
		return nil, err
	}
	for _, p := range ps {
		d.Proposals = append(d.Proposals, proposalOut(p))
	}
	return d, nil
}

// ---- Requests ----

// AskInput is a question to a KB.
type AskInput struct {
	KB       string `json:"kb" maxLength:"31" doc:"The KB's slug (spaceman_kb_list)"`
	Kind     string `json:"kind" enum:"context,decision,discovery,proposal,documentation,consistency"`
	Question string `json:"question" maxLength:"20000"`
	TaskID   string `json:"task_id,omitempty" maxLength:"64" doc:"The task this is for (correlation)"`
}

// Ask queues a question for the requester's own support session and returns at once (SM-KB-002/003). Admission is
// the KB's configuration (allowed roles), preauthorized by an operator: asking is not permission to launch runs.
func (svc *Service) Ask(ctx context.Context, p *fleet.RunPrincipal, in AskInput) (*Request, error) {
	v := &apperr.Validation{}
	if !slices.Contains(Kinds, in.Kind) {
		v.Add("kind", "must be one of %s", strings.Join(Kinds, ", "))
	}
	in.Question = strings.TrimSpace(in.Question)
	if in.Question == "" || len(in.Question) > 20000 {
		v.Add("question", "is required (at most 20000 bytes)")
	}
	if err := v.Err(); err != nil {
		return nil, err
	}
	k, err := svc.s.R().GetKBBySlug(ctx, db.GetKBBySlugParams{SpaceID: p.SpaceID, Slug: in.KB})
	if err != nil {
		return nil, errNotFound
	}
	if k.Enabled != 1 {
		return nil, apperr.New(apperr.CodePrecondition, "the knowledge base %s is disabled", k.Slug)
	}
	allowed := parseList(k.AllowedRolesJson)
	if !slices.Contains(allowed, "*") && !slices.Contains(allowed, p.Role) {
		return nil, apperr.New(apperr.CodeForbidden, "the %s role is not admitted to the %s knowledge base", p.Role, k.Slug)
	}
	if p.Kind == "kb" && p.Role == k.SpaceRole {
		return nil, apperr.New(apperr.CodeInvalid, "a KB session cannot ask its own knowledge base")
	}
	// Support never starts support without bound (SM-LIMIT-002): a request from a KB session is one level deeper than
	// the session itself, and the space's support depth caps it (default 1: KB sessions cannot ask knowledge bases).
	depth := int64(1)
	if p.Kind == "kb" {
		depth = 2
		if own, err := svc.s.R().GetKBSessionByRun(ctx, p.RunID); err == nil {
			depth = own.Depth + 1
		}
	}
	if _, maxDepth := svc.spaceLimits(ctx, svc.s.R(), p.SpaceID); depth > maxDepth {
		return nil, apperr.New(apperr.CodeForbidden, "support sessions cannot start further support sessions here (support depth %d, this request would be %d); "+
			"answer from your own knowledge base or say it does not know", maxDepth, depth)
	}
	reqID := ids.New("kbq")
	err = svc.s.W().TxNamed(ctx, "kb.ask", func(tx *store.Tx) error {
		now := tx.NowMicros()
		sess, err := tx.Q.GetKBSessionByRequester(ctx, db.GetKBSessionByRequesterParams{KbID: k.ID, RequesterKey: p.InstanceID})
		if errors.Is(err, sql.ErrNoRows) || (err == nil && sess.State == "closed") {
			if err == nil {
				// A closed session's requester asks again: a fresh session, same association.
				if _, err := tx.SQL.ExecContext(ctx, `DELETE FROM kb_sessions WHERE id = ?`, sess.ID); err != nil {
					return err
				}
			}
			sid := ids.New(ids.KBSession)
			sess = db.KbSession{ID: sid}
			if err := tx.Q.InsertKBSession(ctx, db.InsertKBSessionParams{ID: sid, KbID: k.ID, SpaceID: k.SpaceID, RequesterKey: p.InstanceID,
				RequesterRole: p.Role, RequesterLabel: p.Role + " (" + p.Kind + ")", InstanceID: ids.New(ids.Instance), Branch: "kb/" + k.Slug + "/" + sid,
				LastActiveAt: now, CreatedAt: now, UpdatedAt: now, Depth: depth}); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		if err := tx.Q.InsertKBRequest(ctx, db.InsertKBRequestParams{ID: reqID, KbID: k.ID, SpaceID: k.SpaceID, SessionID: sess.ID, RequesterRun: p.RunID,
			RequesterInstance: p.InstanceID, RequesterRole: p.Role, Kind: in.Kind, Question: in.Question, TaskID: in.TaskID,
			DeadlineAt: now + k.RequestTimeoutSeconds*1_000_000, CreatedAt: now, UpdatedAt: now}); err != nil {
			return err
		}
		if err := svc.changes.Record(ctx, tx, k.SpaceID, "kb", k.ID, "updated", 0); err != nil {
			return err
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: audit.Actor{Type: audit.ActorAgent, ID: p.RunID}, Action: "kb.asked",
			Target: audit.Target{Type: "kb", ID: k.ID}, Details: map[string]any{"space_id": k.SpaceID, "request": reqID, "kind": in.Kind, "task_id": in.TaskID}})
		return err
	})
	if err != nil {
		return nil, err
	}
	svc.Kick()
	r, err := svc.s.R().GetKBRequest(ctx, reqID)
	if err != nil {
		return nil, err
	}
	return requestOut(r), nil
}

// GetRequest returns a request to its requester (same agent instance) or to a KB session that answers it.
func (svc *Service) GetRequest(ctx context.Context, p *fleet.RunPrincipal, id string) (*Request, error) {
	r, err := svc.s.R().GetKBRequest(ctx, id)
	if err != nil || r.SpaceID != p.SpaceID {
		return nil, apperr.New(apperr.CodeNotFound, "request not found")
	}
	if r.RequesterInstance != p.InstanceID {
		sess, err := svc.s.R().GetKBSession(ctx, r.SessionID)
		if err != nil || sess.RunID != p.RunID {
			return nil, apperr.New(apperr.CodeForbidden, "only the requester and its KB session can read this request")
		}
	}
	return requestOut(r), nil
}

// Cancel withdraws a request (by its requester, or an operator when p is nil).
func (svc *Service) Cancel(ctx context.Context, actor audit.Actor, p *fleet.RunPrincipal, spaceID, id string) error {
	r, err := svc.s.R().GetKBRequest(ctx, id)
	if err != nil || r.SpaceID != spaceID {
		return apperr.New(apperr.CodeNotFound, "request not found")
	}
	if p != nil && r.RequesterInstance != p.InstanceID {
		return apperr.New(apperr.CodeForbidden, "only the requester can cancel this request")
	}
	if err := svc.end(ctx, actor, r, "cancelled", "cancelled"); err != nil {
		return err
	}
	if r.XspaceRequestID != "" && svc.OnCrossSpace != nil {
		svc.OnCrossSpace(ctx, r.ID) // an operator withdrew it in the destination: the source learns it failed
	}
	svc.Kick()
	return nil
}

// CrossSpaceAsk is a question asked on behalf of another space's cross-space request (ADR-0038). The cross-space
// permission is the admission (the KB's allowed roles are for its own space's agents); the requester identity is the
// source run, "space:<id>/run:<id>", so each source run gets its own support session.
type CrossSpaceAsk struct {
	XSpaceRequestID string
	RequesterKey    string
	RequesterLabel  string
	RequesterRun    string
	Kind, Question  string
}

// AskCrossSpaceTx queues a cross-space question for a KB of spaceID inside the caller's transaction and returns the
// KB request's ID. Its outcome is reported through OnCrossSpace, never as a message to a local requester.
func (svc *Service) AskCrossSpaceTx(ctx context.Context, tx *store.Tx, spaceID, slug string, in CrossSpaceAsk) (string, error) {
	if !slices.Contains(Kinds, in.Kind) {
		return "", apperr.New(apperr.CodeInvalid, "kb_kind must be one of %s", strings.Join(Kinds, ", "))
	}
	k, err := tx.Q.GetKBBySlug(ctx, db.GetKBBySlugParams{SpaceID: spaceID, Slug: slug})
	if err != nil {
		return "", errNotFound
	}
	if k.Enabled != 1 {
		return "", apperr.New(apperr.CodePrecondition, "the knowledge base %s is disabled", k.Slug)
	}
	now := tx.NowMicros()
	sess, err := tx.Q.GetKBSessionByRequester(ctx, db.GetKBSessionByRequesterParams{KbID: k.ID, RequesterKey: in.RequesterKey})
	if errors.Is(err, sql.ErrNoRows) || (err == nil && sess.State == "closed") {
		if err == nil {
			if _, err := tx.SQL.ExecContext(ctx, `DELETE FROM kb_sessions WHERE id = ?`, sess.ID); err != nil {
				return "", err
			}
		}
		sid := ids.New(ids.KBSession)
		sess = db.KbSession{ID: sid}
		if err := tx.Q.InsertKBSession(ctx, db.InsertKBSessionParams{ID: sid, KbID: k.ID, SpaceID: k.SpaceID, RequesterKey: in.RequesterKey,
			RequesterRole: "cross-space", RequesterLabel: in.RequesterLabel, InstanceID: ids.New(ids.Instance), Branch: "kb/" + k.Slug + "/" + sid,
			LastActiveAt: now, CreatedAt: now, UpdatedAt: now}); err != nil {
			return "", err
		}
	} else if err != nil {
		return "", err
	}
	reqID := ids.New("kbq")
	if err := tx.Q.InsertKBRequest(ctx, db.InsertKBRequestParams{ID: reqID, KbID: k.ID, SpaceID: k.SpaceID, SessionID: sess.ID, RequesterRun: in.RequesterRun,
		RequesterInstance: in.RequesterKey, RequesterRole: "cross-space", Kind: in.Kind, Question: in.Question,
		DeadlineAt: now + k.RequestTimeoutSeconds*1_000_000, CreatedAt: now, UpdatedAt: now}); err != nil {
		return "", err
	}
	if err := tx.Q.SetKBRequestXSpace(ctx, db.SetKBRequestXSpaceParams{XspaceRequestID: in.XSpaceRequestID, ID: reqID}); err != nil {
		return "", err
	}
	if err := svc.changes.Record(ctx, tx, k.SpaceID, "kb", k.ID, "updated", 0); err != nil {
		return "", err
	}
	if _, err := svc.audit.Append(ctx, tx, audit.Event{Actor: system, Action: "kb.asked", Target: audit.Target{Type: "kb", ID: k.ID},
		Details: map[string]any{"space_id": k.SpaceID, "request": reqID, "kind": in.Kind, "xspace_request": in.XSpaceRequestID}}); err != nil {
		return "", err
	}
	tx.AfterCommit(svc.Kick)
	return reqID, nil
}

// EndCrossSpaceTx withdraws a cross-space KB request inside the caller's transaction (the source cancelled it or its
// deadline passed). A request that already finished is left as it is.
func (svc *Service) EndCrossSpaceTx(ctx context.Context, tx *store.Tx, kbRequestID, state, reason string) error {
	r, err := tx.Q.GetKBRequest(ctx, kbRequestID)
	if err != nil {
		return ignoreNoRows(err)
	}
	n, err := tx.Q.EndKBRequest(ctx, db.EndKBRequestParams{State: state, Error: reason, Now: tx.NowMicros(), ID: r.ID})
	if err != nil || n == 0 {
		return err
	}
	tx.AfterCommit(svc.Kick)
	return svc.changes.Record(ctx, tx, r.SpaceID, "kb", r.KbID, "updated", 0)
}

func ignoreNoRows(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	return err
}

// CrossSpaceResult is a finished cross-space KB request as the cross-space service reads it.
type CrossSpaceResult struct {
	State, Answer, Basis, Error string
	Evidence                    []Evidence
}

// CrossSpaceOutcome reads a KB request's outcome (for OnCrossSpace).
func (svc *Service) CrossSpaceOutcome(ctx context.Context, kbRequestID string) (*CrossSpaceResult, error) {
	r, err := svc.s.R().GetKBRequest(ctx, kbRequestID)
	if err != nil {
		return nil, err
	}
	out := requestOut(r)
	return &CrossSpaceResult{State: out.State, Answer: out.Answer, Basis: out.Basis, Error: out.Error, Evidence: out.Evidence}, nil
}

func (svc *Service) end(ctx context.Context, actor audit.Actor, r db.KbRequest, state, msg string) error {
	return svc.s.W().TxNamed(ctx, "kb.end", func(tx *store.Tx) error {
		n, err := tx.Q.EndKBRequest(ctx, db.EndKBRequestParams{State: state, Error: msg, Now: tx.NowMicros(), ID: r.ID})
		if err != nil {
			return err
		}
		if n == 0 {
			return apperr.New(apperr.CodePrecondition, "the request is already %s", r.State)
		}
		if err := svc.changes.Record(ctx, tx, r.SpaceID, "kb", r.KbID, "updated", 0); err != nil {
			return err
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "kb.request_" + state, Target: audit.Target{Type: "kb", ID: r.KbID},
			Details: map[string]any{"space_id": r.SpaceID, "request": r.ID, "reason": msg}})
		return err
	})
}

// AnswerInput is a KB session's answer.
type AnswerInput struct {
	RequestID string     `json:"request_id" maxLength:"64"`
	Answer    string     `json:"answer" maxLength:"100000" doc:"Markdown"`
	Basis     string     `json:"basis" enum:"committed,proposal,none" doc:"committed: from accepted knowledge; proposal: from your proposed changes; none: the KB does not know"`
	Evidence  []Evidence `json:"evidence,omitempty" maxItems:"50" doc:"Files (and revisions) the answer is based on"`
}

// Answer records a session's answer and notifies the requester.
func (svc *Service) Answer(ctx context.Context, p *fleet.RunPrincipal, in AnswerInput) (*Request, error) {
	r, sess, err := svc.ownRequest(ctx, p, in.RequestID)
	if err != nil {
		return nil, err
	}
	v := &apperr.Validation{}
	if !slices.Contains([]string{"committed", "proposal", "none"}, in.Basis) {
		v.Add("basis", "must be committed, proposal or none")
	}
	if strings.TrimSpace(in.Answer) == "" {
		v.Add("answer", "is required")
	}
	for i := range in.Evidence {
		e := &in.Evidence[i]
		if e.Path == "" || len(e.Path) > 500 || strings.Contains(e.Path, "..") {
			v.Add("evidence", "paths are relative file paths in the KB")
		}
		if e.Revision != "" && !reRev.MatchString(e.Revision) {
			v.Add("evidence", "revisions are commit IDs")
		}
		if e.Kind == "" {
			e.Kind = "committed"
		}
		if e.Kind != "committed" && e.Kind != "proposal" {
			v.Add("evidence", "kind is committed or proposal")
		}
	}
	if err := v.Err(); err != nil {
		return nil, err
	}
	ev, _ := json.Marshal(nonNilEvidence(in.Evidence))
	state := "answered"
	err = svc.s.W().TxNamed(ctx, "kb.answer", func(tx *store.Tx) error {
		n, err := tx.Q.AnswerKBRequest(ctx, db.AnswerKBRequestParams{State: state, Answer: in.Answer, Basis: in.Basis, EvidenceJson: string(ev),
			Now: sql.NullInt64{Int64: tx.NowMicros(), Valid: true}, ID: r.ID})
		if err != nil {
			return err
		}
		if n == 0 {
			return apperr.New(apperr.CodePrecondition, "the request is %s", r.State)
		}
		if err := tx.Q.TouchKBSession(ctx, db.TouchKBSessionParams{LastActiveAt: tx.NowMicros(), Answered: 1, UpdatedAt: tx.NowMicros(), ID: sess.ID}); err != nil {
			return err
		}
		if err := svc.changes.Record(ctx, tx, r.SpaceID, "kb", r.KbID, "updated", 0); err != nil {
			return err
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: audit.Actor{Type: audit.ActorAgent, ID: p.RunID}, Action: "kb.answered",
			Target: audit.Target{Type: "kb", ID: r.KbID}, Details: map[string]any{"space_id": r.SpaceID, "request": r.ID, "basis": in.Basis, "evidence": len(in.Evidence)}})
		return err
	})
	if err != nil {
		return nil, err
	}
	svc.notifyRequester(ctx, r, "answered", firstLine(in.Answer))
	svc.Kick()
	out, err := svc.s.R().GetKBRequest(ctx, r.ID)
	if err != nil {
		return nil, err
	}
	return requestOut(out), nil
}

func nonNilEvidence(e []Evidence) []Evidence {
	if e == nil {
		return []Evidence{}
	}
	return e
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}

// ownRequest checks that a request is dispatched to the calling KB session's run.
func (svc *Service) ownRequest(ctx context.Context, p *fleet.RunPrincipal, id string) (db.KbRequest, db.KbSession, error) {
	sess, err := svc.s.R().GetKBSessionByRun(ctx, p.RunID)
	if err != nil {
		return db.KbRequest{}, db.KbSession{}, apperr.New(apperr.CodeForbidden, "this run is not a KB support session")
	}
	if id == "" {
		return db.KbRequest{}, sess, nil
	}
	r, err := svc.s.R().GetKBRequest(ctx, id)
	if err != nil || r.SessionID != sess.ID {
		return db.KbRequest{}, db.KbSession{}, apperr.New(apperr.CodeNotFound, "no such request for this session")
	}
	return r, sess, nil
}

// notifyRequester tells the requester (its run, or its role when the run ended) that a request finished.
func (svc *Service) notifyRequester(ctx context.Context, r db.KbRequest, what, excerpt string) {
	if r.XspaceRequestID != "" {
		// Asked for another space: the cross-space service reports it to the source, within its disclosure level.
		if svc.OnCrossSpace != nil {
			svc.OnCrossSpace(ctx, r.ID)
		}
		return
	}
	k, err := svc.s.R().GetKB(ctx, r.KbID)
	if err != nil {
		return
	}
	from := messages.From{Kind: "system", ID: "kb:" + k.Slug, Label: "KB " + k.Name}
	body := fmt.Sprintf("Your %s request %s to the %s knowledge base was %s.", r.Kind, r.ID, k.Slug, what)
	if excerpt != "" {
		body += "\n\n> " + excerpt
	}
	body += "\n\nRead it with spaceman_kb_get (request_id " + r.ID + ")."
	in := messages.SendInput{To: "run:" + r.RequesterRun, Kind: "notification", Subject: "KB " + k.Slug + ": request " + what, Body: body,
		CorrelationID: r.ID, DedupKey: "kb-done:" + r.ID}
	if run, err := svc.fleet.GetRun(ctx, r.RequesterRun); err != nil || run.Observed == "exited" || run.Observed == "failed" || run.Observed == "lost" {
		in.To = "role:" + r.RequesterRole
	}
	if _, _, err := svc.msgs.Send(ctx, system, r.SpaceID, from, in); err != nil {
		svc.log.Warn("notify KB requester", slog.String("request", r.ID), slog.Any("error", err))
	}
}
