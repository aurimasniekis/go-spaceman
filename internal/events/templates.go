package events

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/events/template"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Template is a catalog entry with its versions (newest first).
type Template struct {
	ID          string             `json:"id"`
	Slug        string             `json:"slug"`
	Name        string             `json:"name"`
	Description string             `json:"description"`
	Builtin     bool               `json:"builtin" doc:"Seeded from a verified upstream contract"`
	Versions    []*TemplateVersion `json:"versions"`
	CreatedAt   time.Time          `json:"created_at"`
}

// TemplateVersion is one version of a template; published versions never change (SM-EVT-004).
type TemplateVersion struct {
	ID          string              `json:"id"`
	TemplateID  string              `json:"template_id"`
	Version     int64               `json:"version"`
	Status      string              `json:"status" enum:"draft,published"`
	Definition  template.Definition `json:"definition"`
	Notes       string              `json:"notes,omitempty"`
	CreatedAt   time.Time           `json:"created_at"`
	PublishedAt *time.Time          `json:"published_at,omitempty"`
}

var reSlug = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,62}$`)

type builtin struct {
	slug, name, description string
	def                     func() template.Definition
}

var builtins = []builtin{
	{"github-issue", "GitHub issues", "Issue events for one repository, from go-pulse-github webhooks (evt.github.<account>.webhook.issues.*).", template.GitHubIssue},
	{"github-pull-request", "GitHub pull requests", "Pull request events for one repository, from go-pulse-github webhooks (evt.github.<account>.webhook.pull_request.*).", template.GitHubPullRequest},
	{"github-pr-comment", "GitHub pull request comments", "New comments in pull request conversations (evt.github.<account>.webhook.issue_comment.created on pull requests).", template.GitHubPullRequestComment},
	{"github-pr-review", "GitHub pull request reviews", "Submitted pull request reviews: approved, changes requested or commented (evt.github.<account>.webhook.pull_request_review.submitted).", template.GitHubPullRequestReview},
	{"github-pr-review-comment", "GitHub pull request review comments", "New inline comments on pull request diffs (evt.github.<account>.webhook.pull_request_review_comment.created).", template.GitHubPullRequestReviewComment},
	{"github-issue-comment", "GitHub issue comments", "New comments on issues, not pull requests (evt.github.<account>.webhook.issue_comment.created).", template.GitHubIssueComment},
	{"github-workflow-run", "GitHub Actions workflow runs", "Completed workflow runs, by default only failed, timed out or cancelled ones (evt.github.<account>.webhook.workflow_run.completed).", template.GitHubWorkflowRun},
	{"github-check-suite", "GitHub check suites", "Completed check suites of any CI app, by default only failed, timed out or cancelled ones (evt.github.<account>.webhook.check_suite.completed).", template.GitHubCheckSuite},
}

// SeedBuiltins creates the built-in templates (published version 1) when they do not exist yet.
func (svc *Service) SeedBuiltins(ctx context.Context) error {
	for _, b := range builtins {
		if _, err := svc.s.R().GetEventTemplateBySlug(ctx, b.slug); err == nil {
			continue
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		def := b.def()
		if err := def.Validate(); err != nil {
			return err
		}
		id := ids.New(ids.EventTemplate)
		err := svc.s.W().TxNamed(ctx, "events.seed_template", func(tx *store.Tx) error {
			if err := tx.Q.InsertEventTemplate(ctx, db.InsertEventTemplateParams{ID: id, Slug: b.slug, Name: b.name, Description: b.description, Builtin: 1,
				CreatedAt: tx.NowMicros(), CreatedBy: "system", UpdatedAt: tx.NowMicros()}); err != nil {
				if isUnique(err) {
					return nil
				}
				return err
			}
			return tx.Q.InsertEventTemplateVersion(ctx, db.InsertEventTemplateVersionParams{ID: ids.New(ids.EventTemplVer), TemplateID: id, Version: 1,
				Status: "published", DefinitionJson: mustJSON(def), Notes: "Seeded from the go-pulse-github v0.1.0 contract.", CreatedAt: tx.NowMicros(),
				CreatedBy: "system", PublishedAt: sql.NullInt64{Int64: tx.NowMicros(), Valid: true}})
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func versionFromRow(r db.EventTemplateVersion) (*TemplateVersion, error) {
	v := &TemplateVersion{ID: r.ID, TemplateID: r.TemplateID, Version: r.Version, Status: r.Status, Notes: r.Notes, CreatedAt: fromMicros(r.CreatedAt)}
	if err := json.Unmarshal([]byte(r.DefinitionJson), &v.Definition); err != nil {
		return nil, err
	}
	if v.Definition.Variables == nil {
		v.Definition.Variables = []template.Variable{}
	}
	if v.Definition.Filters == nil {
		v.Definition.Filters = []template.Filter{}
	}
	if r.PublishedAt.Valid {
		t := fromMicros(r.PublishedAt.Int64)
		v.PublishedAt = &t
	}
	return v, nil
}

func (svc *Service) templateWithVersions(ctx context.Context, r db.EventTemplate) (*Template, error) {
	t := &Template{ID: r.ID, Slug: r.Slug, Name: r.Name, Description: r.Description, Builtin: r.Builtin == 1, CreatedAt: fromMicros(r.CreatedAt),
		Versions: []*TemplateVersion{}}
	rows, err := svc.s.R().ListEventTemplateVersions(ctx, r.ID)
	if err != nil {
		return nil, err
	}
	for _, vr := range rows {
		v, err := versionFromRow(vr)
		if err != nil {
			return nil, err
		}
		t.Versions = append(t.Versions, v)
	}
	return t, nil
}

// ListTemplates returns the catalog.
func (svc *Service) ListTemplates(ctx context.Context) ([]*Template, error) {
	rows, err := svc.s.R().ListEventTemplates(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*Template, 0, len(rows))
	for _, r := range rows {
		t, err := svc.templateWithVersions(ctx, r)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

// GetTemplate returns one template with its versions.
func (svc *Service) GetTemplate(ctx context.Context, id string) (*Template, error) {
	r, err := svc.s.R().GetEventTemplate(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound("template")
	} else if err != nil {
		return nil, err
	}
	return svc.templateWithVersions(ctx, r)
}

// GetVersion returns one template version.
func (svc *Service) GetVersion(ctx context.Context, id string) (*TemplateVersion, error) {
	r, err := svc.s.R().GetEventTemplateVersion(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound("template version")
	} else if err != nil {
		return nil, err
	}
	return versionFromRow(r)
}

// CreateTemplate adds a catalog entry with a first draft.
func (svc *Service) CreateTemplate(ctx context.Context, actor audit.Actor, slug, name, description string, def template.Definition) (*Template, error) {
	v := &apperr.Validation{}
	if !reSlug.MatchString(slug) {
		v.Add("slug", "lowercase letters, digits and dashes (2-63)")
	}
	if name == "" || len(name) > 120 {
		v.Add("name", "is required (at most 120 characters)")
	}
	if err := def.Validate(); err != nil {
		v.Add("definition", "%v", err)
	}
	if err := v.Err(); err != nil {
		return nil, err
	}
	id := ids.New(ids.EventTemplate)
	err := svc.s.W().TxNamed(ctx, "events.template_create", func(tx *store.Tx) error {
		if err := tx.Q.InsertEventTemplate(ctx, db.InsertEventTemplateParams{ID: id, Slug: slug, Name: name, Description: description,
			CreatedAt: tx.NowMicros(), CreatedBy: actor.ID, UpdatedAt: tx.NowMicros()}); err != nil {
			if isUnique(err) {
				return apperr.New(apperr.CodeAlreadyExists, "a template with slug %q exists", slug)
			}
			return err
		}
		if err := tx.Q.InsertEventTemplateVersion(ctx, db.InsertEventTemplateVersionParams{ID: ids.New(ids.EventTemplVer), TemplateID: id, Version: 1,
			Status: "draft", DefinitionJson: mustJSON(def), CreatedAt: tx.NowMicros(), CreatedBy: actor.ID}); err != nil {
			return err
		}
		_, err := svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "event_template.created", Target: audit.Target{Type: "event_template", ID: id},
			Details: map[string]any{"slug": slug}})
		return err
	})
	if err != nil {
		return nil, err
	}
	return svc.GetTemplate(ctx, id)
}

// SaveDraft replaces the template's draft, or starts a new draft after the latest version.
func (svc *Service) SaveDraft(ctx context.Context, actor audit.Actor, templateID string, def template.Definition, notes string) (*TemplateVersion, error) {
	if err := def.Validate(); err != nil {
		return nil, apperr.New(apperr.CodeInvalid, "definition: %v", err)
	}
	var vid string
	err := svc.s.W().TxNamed(ctx, "events.template_draft", func(tx *store.Tx) error {
		if _, err := tx.Q.GetEventTemplate(ctx, templateID); errors.Is(err, sql.ErrNoRows) {
			return notFound("template")
		}
		rows, err := tx.Q.ListEventTemplateVersions(ctx, templateID)
		if err != nil {
			return err
		}
		if len(rows) > 0 && rows[0].Status == "draft" {
			vid = rows[0].ID
			_, err = tx.Q.UpdateTemplateDraft(ctx, db.UpdateTemplateDraftParams{DefinitionJson: mustJSON(def), Notes: notes, ID: vid})
			return err
		}
		max, err := tx.Q.MaxTemplateVersion(ctx, templateID)
		if err != nil {
			return err
		}
		vid = ids.New(ids.EventTemplVer)
		return tx.Q.InsertEventTemplateVersion(ctx, db.InsertEventTemplateVersionParams{ID: vid, TemplateID: templateID, Version: max + 1, Status: "draft",
			DefinitionJson: mustJSON(def), Notes: notes, CreatedAt: tx.NowMicros(), CreatedBy: actor.ID})
	})
	if err != nil {
		return nil, err
	}
	return svc.GetVersion(ctx, vid)
}

// Publish freezes a draft. Subscriptions that follow the template's latest version move to it when its definition binds
// with their variables and keeps their subject; others stay on their version and report why (SM-EVT-006).
func (svc *Service) Publish(ctx context.Context, actor audit.Actor, versionID string) (*TemplateVersion, []string, error) {
	v, err := svc.GetVersion(ctx, versionID)
	if err != nil {
		return nil, nil, err
	}
	if err := v.Definition.Validate(); err != nil {
		return nil, nil, apperr.New(apperr.CodeInvalid, "definition: %v", err)
	}
	var held []string
	err = svc.s.W().TxNamed(ctx, "events.template_publish", func(tx *store.Tx) error {
		held = nil
		n, err := tx.Q.PublishTemplateVersion(ctx, db.PublishTemplateVersionParams{Now: sql.NullInt64{Int64: tx.NowMicros(), Valid: true}, ID: versionID})
		if err != nil {
			return err
		}
		if n == 0 {
			return apperr.New(apperr.CodePrecondition, "only drafts can be published")
		}
		followers, err := tx.Q.ListFollowingSubscriptions(ctx, sql.NullString{String: v.TemplateID, Valid: true})
		if err != nil {
			return err
		}
		for _, s := range followers {
			var vars map[string]string
			_ = json.Unmarshal([]byte(s.VarsJson), &vars)
			b, err := v.Definition.Bind(vars)
			if err != nil || b.Subject != s.EffectiveSubject {
				// A subject change needs a matching Pulse grant: never follow it silently.
				held = append(held, s.ID)
				continue
			}
			if _, err := tx.Q.UpdateEventSubscription(ctx, db.UpdateEventSubscriptionParams{Name: s.Name, TemplateVersionID: sql.NullString{String: versionID, Valid: true},
				Follow: s.Follow, CustomJson: s.CustomJson, VarsJson: s.VarsJson, EffectiveSubject: s.EffectiveSubject, Enabled: s.Enabled,
				CursorGeneration: s.CursorGeneration, StartPolicy: s.StartPolicy, StartTime: s.StartTime, StartSeq: s.StartSeq, Replay: s.Replay, WhilePaused: s.WhilePaused,
				Now: tx.NowMicros(), ID: s.ID, Version: s.Version}); err != nil {
				return err
			}
			if err := svc.changes.Record(ctx, tx, s.SpaceID, "event_subscription", s.ID, "updated", s.Version+1); err != nil {
				return err
			}
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "event_template.published", Target: audit.Target{Type: "event_template_version", ID: versionID},
			Details: map[string]any{"template_id": v.TemplateID, "version": v.Version, "held_subscriptions": held}})
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	svc.notify()
	out, err := svc.GetVersion(ctx, versionID)
	return out, held, err
}
