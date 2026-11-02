// Package catalog holds the central configuration catalogs (SM-PROMPT-*): versioned prompts, harness profiles, role
// templates and space roles, and the resolution that turns them into a run's effective configuration and manifest.
package catalog

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Prompt kinds: one system for roles, dreams, KB and event instructions (SM-PROMPT-001).
var promptKinds = map[string]bool{"role": true, "dream": true, "kb": true, "event": true, "general": true}

// Prompt is a prompt with its versions.
type Prompt struct {
	ID          string           `json:"id"`
	Slug        string           `json:"slug"`
	Name        string           `json:"name"`
	Kind        string           `json:"kind" enum:"role,dream,kb,event,general"`
	Description string           `json:"description,omitempty"`
	ForkedFrom  string           `json:"forked_from,omitempty" doc:"The prompt version this prompt was forked from"`
	Latest      int64            `json:"latest" doc:"The newest published version number (0: none yet)"`
	Versions    []*PromptVersion `json:"versions,omitempty"`
	CreatedAt   time.Time        `json:"created_at"`
	Version     int64            `json:"version"`
}

// PromptVersion is a draft or an immutable published version.
type PromptVersion struct {
	ID          string     `json:"id"`
	PromptID    string     `json:"prompt_id"`
	Number      int64      `json:"number" doc:"0 for the draft"`
	State       string     `json:"state" enum:"draft,published"`
	Body        string     `json:"body" doc:"Markdown"`
	Extends     string     `json:"extends,omitempty" doc:"A published version of another prompt this one builds on; its content comes first"`
	Notes       string     `json:"notes,omitempty"`
	SHA256      string     `json:"sha256" doc:"Of the resolved content (with what it extends)"`
	CreatedAt   time.Time  `json:"created_at"`
	CreatedBy   string     `json:"created_by"`
	PublishedAt *time.Time `json:"published_at,omitempty"`
	PublishedBy string     `json:"published_by,omitempty"`
	Version     int64      `json:"version"`
}

// Service manages the catalogs.
type Service struct {
	s       *store.Store
	audit   *audit.Writer
	changes *changefeed.Recorder
	opts    Options
}

// Options configure the service.
type Options struct {
	// MaxPermissionMode is the policy ceiling for Claude Code's permission mode (SM-PROMPT-003). Empty: "auto".
	MaxPermissionMode string
}

// New returns the service.
func New(s *store.Store, aw *audit.Writer, ch *changefeed.Recorder, o Options) *Service {
	if o.MaxPermissionMode == "" {
		o.MaxPermissionMode = "auto"
	}
	return &Service{s: s, audit: aw, changes: ch, opts: o}
}

var (
	reSlug       = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	errNotFound  = apperr.New(apperr.CodeNotFound, "prompt not found")
	errVNotFound = apperr.New(apperr.CodeNotFound, "prompt version not found")
)

const (
	maxBody        = 200_000
	maxExtendDepth = 5
)

func ptime(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := time.UnixMicro(v.Int64).UTC()
	return &t
}

func hash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// content resolves a version's text: what it extends (recursively, bounded), then its own body.
func content(ctx context.Context, q *db.Queries, v db.PromptVersion, depth int) (string, error) {
	if v.Extends == "" {
		return v.Body, nil
	}
	if depth >= maxExtendDepth {
		return "", apperr.New(apperr.CodeInvalid, "prompts extend each other more than %d levels deep", maxExtendDepth)
	}
	parent, err := q.GetPromptVersion(ctx, v.Extends)
	if err != nil {
		return "", apperr.New(apperr.CodeInvalid, "extended version %s not found", v.Extends)
	}
	pc, err := content(ctx, q, parent, depth+1)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(pc, "\n") + "\n\n" + v.Body, nil
}

func (svc *Service) versionOut(ctx context.Context, q *db.Queries, v db.PromptVersion) *PromptVersion {
	c, _ := content(ctx, q, v, 0)
	return &PromptVersion{ID: v.ID, PromptID: v.PromptID, Number: v.Number, State: v.State, Body: v.Body, Extends: v.Extends, Notes: v.Notes, SHA256: hash(c),
		CreatedAt: time.UnixMicro(v.CreatedAt).UTC(), CreatedBy: v.CreatedBy, PublishedAt: ptime(v.PublishedAt), PublishedBy: v.PublishedBy, Version: v.Version}
}

// CreatePrompt adds a prompt with a first draft.
func (svc *Service) CreatePrompt(ctx context.Context, actor audit.Actor, slug, name, kind, description, body string) (*Prompt, error) {
	v := &apperr.Validation{}
	if !reSlug.MatchString(slug) {
		v.Add("slug", "must be 1-63 lowercase letters, digits or dashes")
	}
	if strings.TrimSpace(name) == "" || utf8.RuneCountInString(name) > 120 {
		v.Add("name", "is required (at most 120 characters)")
	}
	if !promptKinds[kind] {
		v.Add("kind", "must be role, dream, kb, event or general")
	}
	if len(body) > maxBody || len(description) > 2000 {
		v.Add("body", "body at most 200000 bytes, description 2000")
	}
	if err := v.Err(); err != nil {
		return nil, err
	}
	id := ids.New(ids.Prompt)
	err := svc.s.W().TxNamed(ctx, "catalog.prompt_create", func(tx *store.Tx) error {
		if _, err := tx.Q.GetPromptBySlug(ctx, slug); err == nil {
			return apperr.New(apperr.CodeAlreadyExists, "a prompt %q exists", slug)
		}
		if err := tx.Q.InsertPrompt(ctx, db.InsertPromptParams{ID: id, Slug: slug, Name: name, Kind: kind, Description: description, CreatedAt: tx.NowMicros(),
			CreatedBy: actor.ID, UpdatedAt: tx.NowMicros()}); err != nil {
			return err
		}
		if err := tx.Q.InsertPromptVersion(ctx, db.InsertPromptVersionParams{ID: ids.New(ids.PromptVersion), PromptID: id, Body: body, CreatedAt: tx.NowMicros(),
			CreatedBy: actor.ID, UpdatedAt: tx.NowMicros()}); err != nil {
			return err
		}
		return svc.log(ctx, tx, actor, "prompt.created", "prompt", id, map[string]any{"slug": slug, "kind": kind})
	})
	if err != nil {
		return nil, err
	}
	return svc.GetPrompt(ctx, id)
}

func (svc *Service) log(ctx context.Context, tx *store.Tx, actor audit.Actor, action, typ, id string, details map[string]any) error {
	if err := svc.changes.Record(ctx, tx, "", typ, id, "updated", 0); err != nil {
		return err
	}
	_, err := svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: action, Target: audit.Target{Type: typ, ID: id}, Details: details})
	return err
}

// GetPrompt returns a prompt with all its versions (draft first, then newest published first).
func (svc *Service) GetPrompt(ctx context.Context, id string) (*Prompt, error) {
	q := svc.s.R()
	p, err := q.GetPrompt(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		if p2, err2 := q.GetPromptBySlug(ctx, id); err2 == nil {
			p, err = p2, nil
		}
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound
	} else if err != nil {
		return nil, err
	}
	out := &Prompt{ID: p.ID, Slug: p.Slug, Name: p.Name, Kind: p.Kind, Description: p.Description, ForkedFrom: p.ForkedFrom, CreatedAt: time.UnixMicro(p.CreatedAt).UTC(),
		Version: p.Version, Versions: []*PromptVersion{}}
	vs, err := q.ListPromptVersions(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	for _, v := range vs {
		out.Versions = append(out.Versions, svc.versionOut(ctx, q, v))
		if v.State == "published" && v.Number > out.Latest {
			out.Latest = v.Number
		}
	}
	return out, nil
}

// ListPrompts returns every prompt without version bodies.
func (svc *Service) ListPrompts(ctx context.Context) ([]*Prompt, error) {
	rows, err := svc.s.R().ListPrompts(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*Prompt, 0, len(rows))
	for _, r := range rows {
		p := &Prompt{ID: r.ID, Slug: r.Slug, Name: r.Name, Kind: r.Kind, Description: r.Description, ForkedFrom: r.ForkedFrom, CreatedAt: time.UnixMicro(r.CreatedAt).UTC(),
			Version: r.Version}
		if l, err := svc.s.R().LatestPromptVersion(ctx, r.ID); err == nil {
			p.Latest = l.Number
		}
		out = append(out, p)
	}
	return out, nil
}

// SaveDraft creates or updates the prompt's draft. version is the draft's version (0 to create one).
func (svc *Service) SaveDraft(ctx context.Context, actor audit.Actor, promptID string, version int64, body, extends, notes string) (*PromptVersion, error) {
	if len(body) > maxBody || len(notes) > 2000 {
		return nil, apperr.New(apperr.CodeInvalid, "body at most 200000 bytes, notes 2000")
	}
	var id string
	err := svc.s.W().TxNamed(ctx, "catalog.prompt_draft", func(tx *store.Tx) error {
		p, err := tx.Q.GetPrompt(ctx, promptID)
		if err != nil {
			return errNotFound
		}
		if extends != "" {
			ev, err := tx.Q.GetPromptVersion(ctx, extends)
			if err != nil || ev.State != "published" {
				return apperr.New(apperr.CodeInvalid, "extends must be a published prompt version")
			}
			if ev.PromptID == p.ID {
				return apperr.New(apperr.CodeInvalid, "a prompt cannot extend itself")
			}
			if _, err := content(ctx, tx.Q, db.PromptVersion{Extends: extends}, 0); err != nil {
				return err
			}
		}
		d, err := tx.Q.GetPromptDraft(ctx, p.ID)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if version != 0 {
				return apperr.New(apperr.CodePrecondition, "the draft was published or discarded meanwhile")
			}
			id = ids.New(ids.PromptVersion)
			if err := tx.Q.InsertPromptVersion(ctx, db.InsertPromptVersionParams{ID: id, PromptID: p.ID, Body: body, Extends: extends, Notes: notes,
				CreatedAt: tx.NowMicros(), CreatedBy: actor.ID, UpdatedAt: tx.NowMicros()}); err != nil {
				return err
			}
		case err != nil:
			return err
		default:
			if version != d.Version {
				return apperr.New(apperr.CodePrecondition, "the draft changed since version %d (now %d)", version, d.Version)
			}
			id = d.ID
			if _, err := tx.Q.UpdatePromptDraft(ctx, db.UpdatePromptDraftParams{Body: body, Extends: extends, Notes: notes, UpdatedAt: tx.NowMicros(), ID: d.ID,
				Version: d.Version}); err != nil {
				return err
			}
		}
		return svc.log(ctx, tx, actor, "prompt.draft_saved", "prompt", p.ID, map[string]any{"version_id": id})
	})
	if err != nil {
		return nil, err
	}
	v, err := svc.s.R().GetPromptVersion(ctx, id)
	if err != nil {
		return nil, err
	}
	return svc.versionOut(ctx, svc.s.R(), v), nil
}

// Publish makes the draft the next immutable version. Followers of the prompt get it on their next launch.
func (svc *Service) Publish(ctx context.Context, actor audit.Actor, promptID string, version int64) (*PromptVersion, error) {
	var id string
	err := svc.s.W().TxNamed(ctx, "catalog.prompt_publish", func(tx *store.Tx) error {
		d, err := tx.Q.GetPromptDraft(ctx, promptID)
		if err != nil {
			return apperr.New(apperr.CodePrecondition, "the prompt has no draft to publish")
		}
		if d.Version != version {
			return apperr.New(apperr.CodePrecondition, "the draft changed since version %d (now %d)", version, d.Version)
		}
		next := int64(1)
		if l, err := tx.Q.LatestPromptVersion(ctx, promptID); err == nil {
			next = l.Number + 1
		}
		if _, err := tx.Q.PublishPromptVersion(ctx, db.PublishPromptVersionParams{Number: next, Now: sql.NullInt64{Int64: tx.NowMicros(), Valid: true}, By: actor.ID,
			ID: d.ID, Version: d.Version}); err != nil {
			return err
		}
		id = d.ID
		return svc.log(ctx, tx, actor, "prompt.published", "prompt", promptID, map[string]any{"version_id": d.ID, "number": next})
	})
	if err != nil {
		return nil, err
	}
	v, _ := svc.s.R().GetPromptVersion(ctx, id)
	return svc.versionOut(ctx, svc.s.R(), v), nil
}

// DiscardDraft removes the draft; published versions are never removed.
func (svc *Service) DiscardDraft(ctx context.Context, actor audit.Actor, promptID string) error {
	return svc.s.W().TxNamed(ctx, "catalog.prompt_discard", func(tx *store.Tx) error {
		d, err := tx.Q.GetPromptDraft(ctx, promptID)
		if err != nil {
			return apperr.New(apperr.CodeNotFound, "the prompt has no draft")
		}
		if _, err := tx.Q.DeletePromptDraft(ctx, d.ID); err != nil {
			return err
		}
		return svc.log(ctx, tx, actor, "prompt.draft_discarded", "prompt", promptID, nil)
	})
}

// Rollback publishes an earlier version's content as a new version: history only ever grows (SM-PROMPT-001).
func (svc *Service) Rollback(ctx context.Context, actor audit.Actor, promptID, versionID string) (*PromptVersion, error) {
	old, err := svc.s.R().GetPromptVersion(ctx, versionID)
	if err != nil || old.PromptID != promptID || old.State != "published" {
		return nil, errVNotFound
	}
	if _, err := svc.s.R().GetPromptDraft(ctx, promptID); err == nil {
		return nil, apperr.New(apperr.CodePrecondition, "discard or publish the current draft first")
	}
	d, err := svc.SaveDraft(ctx, actor, promptID, 0, old.Body, old.Extends, "Rollback to version "+itoa(old.Number))
	if err != nil {
		return nil, err
	}
	return svc.Publish(ctx, actor, promptID, d.Version)
}

// Fork copies a published version into a new prompt that records where it came from. Forks never merge back
// automatically.
func (svc *Service) Fork(ctx context.Context, actor audit.Actor, versionID, slug, name string) (*Prompt, error) {
	v, err := svc.s.R().GetPromptVersion(ctx, versionID)
	if err != nil || v.State != "published" {
		return nil, errVNotFound
	}
	src, err := svc.s.R().GetPrompt(ctx, v.PromptID)
	if err != nil {
		return nil, errNotFound
	}
	p, err := svc.CreatePrompt(ctx, actor, slug, name, src.Kind, "Forked from "+src.Slug+" v"+itoa(v.Number), v.Body)
	if err != nil {
		return nil, err
	}
	err = svc.s.W().TxNamed(ctx, "catalog.prompt_fork", func(tx *store.Tx) error {
		if _, err := tx.SQL.ExecContext(ctx, `UPDATE prompts SET forked_from = ? WHERE id = ?`, versionID, p.ID); err != nil {
			return err
		}
		if v.Extends != "" {
			if _, err := tx.SQL.ExecContext(ctx, `UPDATE prompt_versions SET extends = ? WHERE prompt_id = ? AND state = 'draft'`, v.Extends, p.ID); err != nil {
				return err
			}
		}
		return svc.log(ctx, tx, actor, "prompt.forked", "prompt", p.ID, map[string]any{"from_version": versionID})
	})
	if err != nil {
		return nil, err
	}
	return svc.GetPrompt(ctx, p.ID)
}

func itoa(n int64) string { return formatInt(n) }
