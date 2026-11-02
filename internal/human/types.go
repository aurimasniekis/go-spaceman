package human

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/schemautil"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// TypeCustom is the action type of catalog-defined types (SM-HTYPE-*).
const TypeCustom = "custom"

// HumanType is a catalog type with its versions.
type HumanType struct {
	ID          string         `json:"id"`
	Slug        string         `json:"slug"`
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Latest      int64          `json:"latest"`
	Versions    []*TypeVersion `json:"versions,omitempty"`
	CreatedAt   time.Time      `json:"created_at"`
}

// TypeVersion is a draft or an immutable published version of a type.
type TypeVersion struct {
	ID                   string          `json:"id"`
	TypeID               string          `json:"type_id"`
	Number               int64           `json:"number" doc:"0 for the draft"`
	State                string          `json:"state" enum:"draft,published"`
	RequestSchema        json.RawMessage `json:"request_schema" doc:"JSON Schema 2020-12 of the request (root: object)"`
	ResponseSchema       json.RawMessage `json:"response_schema" doc:"JSON Schema 2020-12 of the response data (root: object)"`
	UI                   json.RawMessage `json:"ui" doc:"Safe form metadata: field order, labels, help and widgets; never code"`
	Outcomes             []string        `json:"outcomes" doc:"How a person can finish it, e.g. [\"done\", \"declined\"]"`
	DefaultPriority      string          `json:"default_priority" enum:"low,normal,high,urgent"`
	DefaultExpiryMinutes int64           `json:"default_expiry_minutes"`
	Notes                string          `json:"notes,omitempty"`
	PublishedAt          *time.Time      `json:"published_at,omitempty"`
	PublishedBy          string          `json:"published_by,omitempty"`
	Version              int64           `json:"version"`
}

// TypeDraft is the editable content of a type version.
type TypeDraft struct {
	RequestSchema        json.RawMessage
	ResponseSchema       json.RawMessage
	UI                   json.RawMessage
	Outcomes             []string
	DefaultPriority      string
	DefaultExpiryMinutes int64
	Notes                string
}

// TypeSpec is what an action needs from its type version to be shown and answered.
type TypeSpec struct {
	RequestSchema  json.RawMessage `json:"request_schema"`
	ResponseSchema json.RawMessage `json:"response_schema"`
	UI             json.RawMessage `json:"ui"`
	Outcomes       []string        `json:"outcomes"`
}

// SpaceType is a type enabled in a space with the version it uses now (SM-HTYPE-002).
type SpaceType struct {
	TypeID               string          `json:"type_id"`
	Slug                 string          `json:"slug"`
	Name                 string          `json:"name"`
	Label                string          `json:"label" doc:"The space's name for it (a variant); defaults to the type name"`
	Pinned               bool            `json:"pinned"`
	VersionID            string          `json:"version_id"`
	Number               int64           `json:"number"`
	RequestSchema        json.RawMessage `json:"request_schema"`
	ResponseSchema       json.RawMessage `json:"response_schema"`
	UI                   json.RawMessage `json:"ui"`
	Outcomes             []string        `json:"outcomes"`
	DefaultPriority      string          `json:"default_priority"`
	DefaultExpiryMinutes int64           `json:"default_expiry_minutes"`
}

var (
	reTypeSlug = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	reOutcome  = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	reField    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)
	widgets    = []string{"text", "textarea", "markdown", "number", "checkbox", "select", "radio", "date", "datetime", "url", "email"}
)

// uiMeta is the only form metadata accepted: plain text and an allowlisted widget per field. Unknown keys are refused,
// so metadata can never carry scripts, components or styles (SM-HTYPE-003).
type uiMeta struct {
	Order  []string               `json:"order,omitempty"`
	Fields map[string]uiFieldMeta `json:"fields,omitempty"`
}

type uiFieldMeta struct {
	Label       string `json:"label,omitempty"`
	Help        string `json:"help,omitempty"`
	Widget      string `json:"widget,omitempty"`
	Placeholder string `json:"placeholder,omitempty"`
}

func strictDecode(raw json.RawMessage, v any) error {
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.DisallowUnknownFields()
	return d.Decode(v)
}

func (d *TypeDraft) normalize() error {
	v := &apperr.Validation{}
	if len(d.RequestSchema) == 0 {
		d.RequestSchema = json.RawMessage(`{"type":"object"}`)
	}
	if len(d.ResponseSchema) == 0 {
		d.ResponseSchema = json.RawMessage(`{"type":"object"}`)
	}
	if len(d.UI) == 0 || string(d.UI) == "null" {
		d.UI = json.RawMessage(`{}`)
	}
	if _, err := schemautil.CompileObject(d.RequestSchema); err != nil {
		v.Add("request_schema", "%v", err)
	}
	if _, err := schemautil.CompileObject(d.ResponseSchema); err != nil {
		v.Add("response_schema", "%v", err)
	}
	var ui uiMeta
	if len(d.UI) > 32<<10 || strictDecode(d.UI, &ui) != nil {
		v.Add("ui", "only order and fields (label, help, widget, placeholder) are allowed")
	}
	for name, f := range ui.Fields {
		if !reField.MatchString(name) || len(f.Label) > 200 || len(f.Help) > 2000 || len(f.Placeholder) > 200 {
			v.Add("ui", "field %q: names are identifiers; labels and help are short plain text", name)
		}
		if f.Widget != "" && !slices.Contains(widgets, f.Widget) {
			v.Add("ui", "field %q: widget must be one of %s", name, strings.Join(widgets, ", "))
		}
	}
	if len(d.Outcomes) == 0 {
		d.Outcomes = []string{"done"}
	}
	if len(d.Outcomes) > 10 {
		v.Add("outcomes", "at most 10 outcomes")
	}
	for _, o := range d.Outcomes {
		if !reOutcome.MatchString(o) {
			v.Add("outcomes", "%q: lowercase identifiers only", o)
		}
	}
	if d.DefaultPriority == "" {
		d.DefaultPriority = "normal"
	}
	if !slices.Contains(priorityNames, d.DefaultPriority) {
		v.Add("default_priority", "must be low, normal, high or urgent")
	}
	if d.DefaultExpiryMinutes < 0 || d.DefaultExpiryMinutes > 60*24*90 {
		v.Add("default_expiry_minutes", "between 0 (none) and 90 days")
	}
	if len(d.Notes) > 2000 {
		v.Add("notes", "at most 2000 characters")
	}
	return v.Err()
}

func typeVersionOut(r db.HumanTypeVersion) *TypeVersion {
	var outcomes []string
	_ = json.Unmarshal([]byte(r.OutcomesJson), &outcomes)
	return &TypeVersion{ID: r.ID, TypeID: r.TypeID, Number: r.Number, State: r.State, RequestSchema: json.RawMessage(r.RequestSchema),
		ResponseSchema: json.RawMessage(r.ResponseSchema), UI: json.RawMessage(r.UiJson), Outcomes: outcomes, DefaultPriority: r.DefaultPriority,
		DefaultExpiryMinutes: r.DefaultExpiryMinutes, Notes: r.Notes, PublishedAt: tn(r.PublishedAt), PublishedBy: r.PublishedBy, Version: r.Version}
}

func (svc *Service) logType(ctx context.Context, tx *store.Tx, actor audit.Actor, action, id string, details map[string]any) error {
	if err := svc.changes.Record(ctx, tx, "", "human_type", id, "updated", 0); err != nil {
		return err
	}
	_, err := svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: action, Target: audit.Target{Type: "human_type", ID: id}, Details: details})
	return err
}

// CreateType adds a type with a first draft. Admins add types without code changes (SM-HTYPE-002).
func (svc *Service) CreateType(ctx context.Context, actor audit.Actor, slug, name, description string, d TypeDraft) (*HumanType, error) {
	v := &apperr.Validation{}
	if !reTypeSlug.MatchString(slug) {
		v.Add("slug", "must be 1-63 lowercase letters, digits or dashes")
	}
	if strings.TrimSpace(name) == "" || len(name) > 120 || len(description) > 2000 {
		v.Add("name", "is required (at most 120 characters)")
	}
	if err := v.Err(); err != nil {
		return nil, err
	}
	if err := d.normalize(); err != nil {
		return nil, err
	}
	id := ids.New(ids.HumanType)
	err := svc.s.W().TxNamed(ctx, "human.type_create", func(tx *store.Tx) error {
		if _, err := tx.Q.GetHumanTypeBySlug(ctx, slug); err == nil {
			return apperr.New(apperr.CodeAlreadyExists, "a type %q exists", slug)
		}
		if err := tx.Q.InsertHumanType(ctx, db.InsertHumanTypeParams{ID: id, Slug: slug, Name: name, Description: description, CreatedAt: tx.NowMicros(),
			CreatedBy: actor.ID, UpdatedAt: tx.NowMicros()}); err != nil {
			return err
		}
		if err := svc.insertTypeDraft(ctx, tx, actor, id, d); err != nil {
			return err
		}
		return svc.logType(ctx, tx, actor, "human_type.created", id, map[string]any{"slug": slug})
	})
	if err != nil {
		return nil, err
	}
	return svc.GetType(ctx, id)
}

func (svc *Service) insertTypeDraft(ctx context.Context, tx *store.Tx, actor audit.Actor, typeID string, d TypeDraft) error {
	outcomes, _ := json.Marshal(d.Outcomes)
	return tx.Q.InsertHumanTypeVersion(ctx, db.InsertHumanTypeVersionParams{ID: ids.New(ids.HumanTypeVer), TypeID: typeID, RequestSchema: string(d.RequestSchema),
		ResponseSchema: string(d.ResponseSchema), UiJson: string(d.UI), OutcomesJson: string(outcomes), DefaultPriority: d.DefaultPriority,
		DefaultExpiryMinutes: d.DefaultExpiryMinutes, Notes: d.Notes, CreatedAt: tx.NowMicros(), CreatedBy: actor.ID, UpdatedAt: tx.NowMicros()})
}

// SaveTypeDraft creates (version 0) or updates the type's draft.
func (svc *Service) SaveTypeDraft(ctx context.Context, actor audit.Actor, typeID string, version int64, d TypeDraft) (*HumanType, error) {
	if err := d.normalize(); err != nil {
		return nil, err
	}
	err := svc.s.W().TxNamed(ctx, "human.type_draft", func(tx *store.Tx) error {
		if _, err := tx.Q.GetHumanType(ctx, typeID); err != nil {
			return apperr.New(apperr.CodeNotFound, "type not found")
		}
		cur, err := tx.Q.GetHumanTypeDraft(ctx, typeID)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if version != 0 {
				return apperr.New(apperr.CodePrecondition, "the draft was published meanwhile")
			}
			if err := svc.insertTypeDraft(ctx, tx, actor, typeID, d); err != nil {
				return err
			}
		case err != nil:
			return err
		default:
			if cur.Version != version {
				return apperr.New(apperr.CodePrecondition, "the draft changed since version %d (now %d)", version, cur.Version)
			}
			outcomes, _ := json.Marshal(d.Outcomes)
			if _, err := tx.Q.UpdateHumanTypeDraft(ctx, db.UpdateHumanTypeDraftParams{RequestSchema: string(d.RequestSchema), ResponseSchema: string(d.ResponseSchema),
				UiJson: string(d.UI), OutcomesJson: string(outcomes), DefaultPriority: d.DefaultPriority, DefaultExpiryMinutes: d.DefaultExpiryMinutes, Notes: d.Notes,
				UpdatedAt: tx.NowMicros(), ID: cur.ID, Version: cur.Version}); err != nil {
				return err
			}
		}
		return svc.logType(ctx, tx, actor, "human_type.draft_saved", typeID, nil)
	})
	if err != nil {
		return nil, err
	}
	return svc.GetType(ctx, typeID)
}

// PublishType publishes the draft as the next immutable version. Outstanding actions keep the version they were
// created with (SM-HTYPE-005).
func (svc *Service) PublishType(ctx context.Context, actor audit.Actor, typeID string, version int64) (*HumanType, error) {
	err := svc.s.W().TxNamed(ctx, "human.type_publish", func(tx *store.Tx) error {
		d, err := tx.Q.GetHumanTypeDraft(ctx, typeID)
		if err != nil {
			return apperr.New(apperr.CodePrecondition, "the type has no draft to publish")
		}
		if d.Version != version {
			return apperr.New(apperr.CodePrecondition, "the draft changed since version %d (now %d)", version, d.Version)
		}
		next := int64(1)
		if l, err := tx.Q.LatestHumanTypeVersion(ctx, typeID); err == nil {
			next = l.Number + 1
		}
		if _, err := tx.Q.PublishHumanTypeVersion(ctx, db.PublishHumanTypeVersionParams{Number: next, Now: sql.NullInt64{Int64: tx.NowMicros(), Valid: true},
			By: actor.ID, ID: d.ID, Version: d.Version}); err != nil {
			return err
		}
		return svc.logType(ctx, tx, actor, "human_type.published", typeID, map[string]any{"number": next})
	})
	if err != nil {
		return nil, err
	}
	return svc.GetType(ctx, typeID)
}

// GetType returns a type (by ID or slug) with its versions.
func (svc *Service) GetType(ctx context.Context, id string) (*HumanType, error) {
	q := svc.s.R()
	t, err := q.GetHumanType(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		t, err = q.GetHumanTypeBySlug(ctx, id)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil, apperr.New(apperr.CodeNotFound, "type not found")
	} else if err != nil {
		return nil, err
	}
	out := &HumanType{ID: t.ID, Slug: t.Slug, Name: t.Name, Description: t.Description, CreatedAt: time.UnixMicro(t.CreatedAt).UTC(), Versions: []*TypeVersion{}}
	vs, err := q.ListHumanTypeVersions(ctx, t.ID)
	if err != nil {
		return nil, err
	}
	for _, v := range vs {
		out.Versions = append(out.Versions, typeVersionOut(v))
		if v.State == "published" && v.Number > out.Latest {
			out.Latest = v.Number
		}
	}
	return out, nil
}

// ListTypes returns every type with its versions.
func (svc *Service) ListTypes(ctx context.Context) ([]*HumanType, error) {
	rows, err := svc.s.R().ListHumanTypes(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*HumanType, 0, len(rows))
	for _, r := range rows {
		t, err := svc.GetType(ctx, r.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

// EnableType lets a space use a type: following the latest version or pinned to one, optionally as a variant with
// its own label and defaults.
func (svc *Service) EnableType(ctx context.Context, actor audit.Actor, spaceID, typeID, pin, label, priority string, expiryMinutes int64) error {
	v := &apperr.Validation{}
	if len(label) > 120 {
		v.Add("label", "at most 120 characters")
	}
	if priority != "" && !slices.Contains(priorityNames, priority) {
		v.Add("default_priority", "must be low, normal, high or urgent")
	}
	if expiryMinutes < 0 || expiryMinutes > 60*24*90 {
		v.Add("default_expiry_minutes", "between 0 and 90 days")
	}
	if err := v.Err(); err != nil {
		return err
	}
	return svc.s.W().TxNamed(ctx, "human.type_enable", func(tx *store.Tx) error {
		if _, err := tx.Q.GetSpace(ctx, spaceID); err != nil {
			return apperr.New(apperr.CodeNotFound, "space not found")
		}
		if _, err := tx.Q.GetHumanType(ctx, typeID); err != nil {
			return apperr.New(apperr.CodeInvalid, "type %q not found", typeID)
		}
		if pin != "" {
			pv, err := tx.Q.GetHumanTypeVersion(ctx, pin)
			if err != nil || pv.TypeID != typeID || pv.State != "published" {
				return apperr.New(apperr.CodeInvalid, "pin must be a published version of the type")
			}
		} else if _, err := tx.Q.LatestHumanTypeVersion(ctx, typeID); err != nil {
			return apperr.New(apperr.CodePrecondition, "publish the type before enabling it")
		}
		if err := tx.Q.UpsertSpaceHumanType(ctx, db.UpsertSpaceHumanTypeParams{SpaceID: spaceID, TypeID: typeID, VersionPin: pin, Label: label,
			DefaultPriority: priority, DefaultExpiryMinutes: expiryMinutes, CreatedAt: tx.NowMicros(), CreatedBy: actor.ID}); err != nil {
			return err
		}
		if err := svc.changes.Record(ctx, tx, spaceID, "space", spaceID, "updated", 0); err != nil {
			return err
		}
		_, err := svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "space.human_type_enabled", Target: audit.Target{Type: "space", ID: spaceID},
			Details: map[string]any{"type_id": typeID, "pin": pin}})
		return err
	})
}

// DisableType stops new actions of a type in a space; existing ones are unaffected.
func (svc *Service) DisableType(ctx context.Context, actor audit.Actor, spaceID, typeID string) error {
	return svc.s.W().TxNamed(ctx, "human.type_disable", func(tx *store.Tx) error {
		n, err := tx.Q.DeleteSpaceHumanType(ctx, db.DeleteSpaceHumanTypeParams{SpaceID: spaceID, TypeID: typeID})
		if err != nil {
			return err
		}
		if n == 0 {
			return apperr.New(apperr.CodeNotFound, "the type is not enabled in this space")
		}
		if err := svc.changes.Record(ctx, tx, spaceID, "space", spaceID, "updated", 0); err != nil {
			return err
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "space.human_type_disabled", Target: audit.Target{Type: "space", ID: spaceID},
			Details: map[string]any{"type_id": typeID}})
		return err
	})
}

// SpaceTypes returns the types a space has enabled, each with the version it uses now (SM-HTYPE-004 discovery).
func (svc *Service) SpaceTypes(ctx context.Context, spaceID string) ([]SpaceType, error) {
	return spaceTypes(ctx, svc.s.R(), spaceID)
}

func spaceTypes(ctx context.Context, q *db.Queries, spaceID string) ([]SpaceType, error) {
	rows, err := q.ListSpaceHumanTypes(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	out := []SpaceType{}
	for _, r := range rows {
		t, err := q.GetHumanType(ctx, r.TypeID)
		if err != nil {
			continue
		}
		var v db.HumanTypeVersion
		if r.VersionPin != "" {
			v, err = q.GetHumanTypeVersion(ctx, r.VersionPin)
		} else {
			v, err = q.LatestHumanTypeVersion(ctx, r.TypeID)
		}
		if err != nil {
			continue
		}
		tv := typeVersionOut(v)
		st := SpaceType{TypeID: t.ID, Slug: t.Slug, Name: t.Name, Label: r.Label, Pinned: r.VersionPin != "", VersionID: v.ID, Number: v.Number,
			RequestSchema: tv.RequestSchema, ResponseSchema: tv.ResponseSchema, UI: tv.UI, Outcomes: tv.Outcomes, DefaultPriority: tv.DefaultPriority,
			DefaultExpiryMinutes: tv.DefaultExpiryMinutes}
		if st.Label == "" {
			st.Label = t.Name
		}
		if r.DefaultPriority != "" {
			st.DefaultPriority = r.DefaultPriority
		}
		if r.DefaultExpiryMinutes > 0 {
			st.DefaultExpiryMinutes = r.DefaultExpiryMinutes
		}
		out = append(out, st)
	}
	return out, nil
}

// spaceTypeBySlug finds a type enabled in a space, with the version it uses now.
func spaceTypeBySlug(ctx context.Context, q *db.Queries, spaceID, slug string) (*SpaceType, error) {
	ts, err := spaceTypes(ctx, q, spaceID)
	if err != nil {
		return nil, err
	}
	for i := range ts {
		if ts[i].Slug == slug {
			return &ts[i], nil
		}
	}
	return nil, apperr.New(apperr.CodeInvalid, "type %q is not enabled in this space", slug)
}
