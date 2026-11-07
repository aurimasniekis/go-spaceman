package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Profile is a harness profile: harness, model and settings defaults (SM-PROMPT-002).
type Profile struct {
	ID       string            `json:"id"`
	Slug     string            `json:"slug"`
	Name     string            `json:"name"`
	Harness  string            `json:"harness" enum:"claude-code,codex"`
	Model    string            `json:"model,omitempty"`
	Settings map[string]string `json:"settings"`
	// ClaudeSettings are set in each run's Claude Code settings.json (claude-code profiles only, ADR-0059).
	ClaudeSettings proto.ClaudeSettings `json:"claude_settings" doc:"Claude Code settings.json keys for the profile's runs (claude-code profiles only); {} for none"`
	Description    string               `json:"description,omitempty"`
	Revision       int64                `json:"revision"`
	UpdatedAt      time.Time            `json:"updated_at"`
	Version        int64                `json:"version"`
}

// RoleTemplate is reusable role configuration: kind, behavior prompt, profile and memory conventions.
type RoleTemplate struct {
	ID                string            `json:"id"`
	Slug              string            `json:"slug"`
	Name              string            `json:"name"`
	Kind              string            `json:"kind" enum:"control,worker,kb,dreamer,diagnostic"`
	Description       string            `json:"description,omitempty"`
	ProfileID         string            `json:"profile_id,omitempty"`
	PromptID          string            `json:"prompt_id,omitempty"`
	PromptPin         string            `json:"prompt_pin,omitempty" doc:"A published version ID; empty follows the latest version"`
	MemoryConventions string            `json:"memory_conventions,omitempty"`
	Settings          map[string]string `json:"settings"`
	Skills            []SkillRef        `json:"skills"`
	Revision          int64             `json:"revision"`
	UpdatedAt         time.Time         `json:"updated_at"`
	Version           int64             `json:"version"`
}

// SpaceRole configures a role template inside one space; its non-empty fields override the template's.
type SpaceRole struct {
	SpaceID      string            `json:"space_id"`
	Name         string            `json:"name"`
	TemplateID   string            `json:"template_id"`
	ProfileID    string            `json:"profile_id,omitempty"`
	PromptID     string            `json:"prompt_id,omitempty"`
	PromptPin    string            `json:"prompt_pin,omitempty"`
	Model        string            `json:"model,omitempty"`
	Settings     map[string]string `json:"settings"`
	Instructions string            `json:"instructions,omitempty" doc:"Space-specific text appended after the role's prompt"`
	Skills       []SkillRef        `json:"skills" doc:"Added to the template's skills; a pin here wins"`
	Revision     int64             `json:"revision"`
	UpdatedAt    time.Time         `json:"updated_at"`
	Version      int64             `json:"version"`
}

var (
	reRoleName   = regexp.MustCompile(`^[a-z][a-z0-9-]{0,47}$`)
	reSettingKey = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)
	runKinds     = map[string]bool{"control": true, "worker": true, "kb": true, "dreamer": true, "diagnostic": true}
)

func settingsJSON(v *apperr.Validation, m map[string]string) string {
	if len(m) > 32 {
		v.Add("settings", "at most 32 settings")
	}
	for k, val := range m {
		if !reSettingKey.MatchString(k) || len(val) > 500 {
			v.Add("settings", "keys are lowercase identifiers; values at most 500 characters")
			break
		}
	}
	if m == nil {
		m = map[string]string{}
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func parseSettings(s string) map[string]string {
	m := map[string]string{}
	_ = json.Unmarshal([]byte(s), &m)
	return m
}

// parseClaudeSettings reads a stored claude_settings_json ('{}' when none); the result is never nil.
func parseClaudeSettings(s string) proto.ClaudeSettings {
	c := proto.ClaudeSettings{}
	_ = json.Unmarshal([]byte(s), &c)
	if c == nil {
		c = proto.ClaudeSettings{}
	}
	return c
}

// claudeSettingsJSON validates a profile's Claude Code settings and returns their stored form.
func claudeSettingsJSON(v *apperr.Validation, harness string, c proto.ClaudeSettings) string {
	if c == nil {
		c = proto.ClaudeSettings{}
	}
	if len(c) > 0 && harness != "claude-code" {
		v.Add("claude_settings", "only a claude-code profile has Claude Code settings")
	}
	for _, msg := range c.Validate() {
		field, text, _ := strings.Cut(msg, ": ")
		if field != "claude_settings" {
			field = "claude_settings." + field
		}
		v.Add(field, "%s", text)
	}
	b, _ := json.Marshal(c)
	return string(b)
}

func profileOut(r db.HarnessProfile) *Profile {
	return &Profile{ID: r.ID, Slug: r.Slug, Name: r.Name, Harness: r.Harness, Model: r.Model, Settings: parseSettings(r.SettingsJson),
		ClaudeSettings: parseClaudeSettings(r.ClaudeSettingsJson), Description: r.Description, Revision: r.Revision,
		UpdatedAt: time.UnixMicro(r.UpdatedAt).UTC(), Version: r.Version}
}

func templateOut(r db.RoleTemplate) *RoleTemplate {
	return &RoleTemplate{ID: r.ID, Slug: r.Slug, Name: r.Name, Kind: r.Kind, Description: r.Description, ProfileID: r.ProfileID, PromptID: r.PromptID,
		PromptPin: r.PromptPin, MemoryConventions: r.MemoryConventions, Settings: parseSettings(r.SettingsJson), Skills: parseSkillRefs(r.SkillsJson), Revision: r.Revision,
		UpdatedAt: time.UnixMicro(r.UpdatedAt).UTC(), Version: r.Version}
}

func spaceRoleOut(r db.SpaceRole) *SpaceRole {
	return &SpaceRole{SpaceID: r.SpaceID, Name: r.Name, TemplateID: r.TemplateID, ProfileID: r.ProfileID, PromptID: r.PromptID, PromptPin: r.PromptPin, Model: r.Model,
		Settings: parseSettings(r.SettingsJson), Instructions: r.Instructions, Skills: parseSkillRefs(r.SkillsJson), Revision: r.Revision, UpdatedAt: time.UnixMicro(r.UpdatedAt).UTC(), Version: r.Version}
}

// SaveProfile creates (id empty) or updates a profile; an update bumps its revision.
func (svc *Service) SaveProfile(ctx context.Context, actor audit.Actor, id string, version int64, in Profile) (*Profile, error) {
	v := &apperr.Validation{}
	if id == "" && !reSlug.MatchString(in.Slug) {
		v.Add("slug", "must be 1-63 lowercase letters, digits or dashes")
	}
	if strings.TrimSpace(in.Name) == "" || len(in.Name) > 120 {
		v.Add("name", "is required")
	}
	if id == "" && in.Harness != "claude-code" && in.Harness != "codex" {
		v.Add("harness", "must be claude-code or codex")
	}
	if len(in.Model) > 128 || len(in.Description) > 2000 {
		v.Add("model", "model at most 128 characters")
	}
	sj := settingsJSON(v, in.Settings)
	harness := in.Harness
	if id != "" {
		// The harness never changes on an update: check the settings against the stored one.
		if cur, err := svc.s.R().GetProfile(ctx, id); err == nil {
			harness = cur.Harness
		}
	}
	cj := claudeSettingsJSON(v, harness, in.ClaudeSettings)
	if err := v.Err(); err != nil {
		return nil, err
	}
	err := svc.s.W().TxNamed(ctx, "catalog.profile_save", func(tx *store.Tx) error {
		if id == "" {
			id = ids.New(ids.Profile)
			if err := tx.Q.InsertProfile(ctx, db.InsertProfileParams{ID: id, Slug: in.Slug, Name: in.Name, Harness: in.Harness, Model: in.Model, SettingsJson: sj,
				ClaudeSettingsJson: cj, Description: in.Description, CreatedAt: tx.NowMicros(), CreatedBy: actor.ID, UpdatedAt: tx.NowMicros()}); err != nil {
				if store.IsConstraint(err) {
					return apperr.New(apperr.CodeAlreadyExists, "a profile %q exists", in.Slug)
				}
				return err
			}
			return svc.log(ctx, tx, actor, "profile.created", "profile", id, map[string]any{"slug": in.Slug, "harness": in.Harness})
		}
		n, err := tx.Q.UpdateProfile(ctx, db.UpdateProfileParams{Name: in.Name, Model: in.Model, SettingsJson: sj, ClaudeSettingsJson: cj, Description: in.Description,
			UpdatedAt: tx.NowMicros(),
			ID:        id, Version: version})
		if err != nil {
			return err
		}
		if n == 0 {
			return apperr.New(apperr.CodePrecondition, "the profile changed or does not exist")
		}
		return svc.log(ctx, tx, actor, "profile.updated", "profile", id, nil)
	})
	if err != nil {
		return nil, err
	}
	r, err := svc.s.R().GetProfile(ctx, id)
	if err != nil {
		return nil, err
	}
	return profileOut(r), nil
}

// GetProfile returns one profile by ID.
func (svc *Service) GetProfile(ctx context.Context, id string) (*Profile, error) {
	r, err := svc.s.R().GetProfile(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, apperr.New(apperr.CodeNotFound, "harness profile not found")
	} else if err != nil {
		return nil, err
	}
	return profileOut(r), nil
}

// ListProfiles returns every profile.
func (svc *Service) ListProfiles(ctx context.Context) ([]*Profile, error) {
	rows, err := svc.s.R().ListProfiles(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*Profile, 0, len(rows))
	for _, r := range rows {
		out = append(out, profileOut(r))
	}
	return out, nil
}

func (svc *Service) checkRefs(ctx context.Context, q *db.Queries, v *apperr.Validation, profileID, promptID, pin string) {
	if profileID != "" {
		if _, err := q.GetProfile(ctx, profileID); err != nil {
			v.Add("profile_id", "is not a profile")
		}
	}
	if promptID != "" {
		if _, err := q.GetPrompt(ctx, promptID); err != nil {
			v.Add("prompt_id", "is not a prompt")
		}
	}
	if pin != "" {
		pv, err := q.GetPromptVersion(ctx, pin)
		if err != nil || pv.State != "published" || pv.PromptID != promptID {
			v.Add("prompt_pin", "must be a published version of the chosen prompt")
		}
	}
}

// SaveTemplate creates (id empty) or updates a role template; an update bumps its revision.
func (svc *Service) SaveTemplate(ctx context.Context, actor audit.Actor, id string, version int64, in RoleTemplate) (*RoleTemplate, error) {
	v := &apperr.Validation{}
	if id == "" && !reSlug.MatchString(in.Slug) {
		v.Add("slug", "must be 1-63 lowercase letters, digits or dashes")
	}
	if id == "" && !runKinds[in.Kind] {
		v.Add("kind", "must be control, worker, kb, dreamer or diagnostic")
	}
	if strings.TrimSpace(in.Name) == "" || len(in.Name) > 120 || len(in.Description) > 2000 || len(in.MemoryConventions) > 20_000 {
		v.Add("name", "is required; description at most 2000 and memory conventions 20000 characters")
	}
	sj := settingsJSON(v, in.Settings)
	kj := svc.skillRefsJSON(ctx, v, in.Skills)
	svc.checkRefs(ctx, svc.s.R(), v, in.ProfileID, in.PromptID, in.PromptPin)
	if err := v.Err(); err != nil {
		return nil, err
	}
	err := svc.s.W().TxNamed(ctx, "catalog.template_save", func(tx *store.Tx) error {
		if id == "" {
			id = ids.New(ids.RoleTemplate)
			if err := tx.Q.InsertRoleTemplate(ctx, db.InsertRoleTemplateParams{ID: id, Slug: in.Slug, Name: in.Name, Kind: in.Kind, Description: in.Description,
				ProfileID: in.ProfileID, PromptID: in.PromptID, PromptPin: in.PromptPin, MemoryConventions: in.MemoryConventions, SettingsJson: sj, SkillsJson: kj,
				CreatedAt: tx.NowMicros(), CreatedBy: actor.ID, UpdatedAt: tx.NowMicros()}); err != nil {
				if store.IsConstraint(err) {
					return apperr.New(apperr.CodeAlreadyExists, "a role template %q exists", in.Slug)
				}
				return err
			}
			return svc.log(ctx, tx, actor, "role_template.created", "role_template", id, map[string]any{"slug": in.Slug, "kind": in.Kind})
		}
		n, err := tx.Q.UpdateRoleTemplate(ctx, db.UpdateRoleTemplateParams{Name: in.Name, Description: in.Description, ProfileID: in.ProfileID, PromptID: in.PromptID,
			PromptPin: in.PromptPin, MemoryConventions: in.MemoryConventions, SettingsJson: sj, SkillsJson: kj, UpdatedAt: tx.NowMicros(), ID: id, Version: version})
		if err != nil {
			return err
		}
		if n == 0 {
			return apperr.New(apperr.CodePrecondition, "the role template changed or does not exist")
		}
		return svc.log(ctx, tx, actor, "role_template.updated", "role_template", id, nil)
	})
	if err != nil {
		return nil, err
	}
	r, err := svc.s.R().GetRoleTemplate(ctx, id)
	if err != nil {
		return nil, err
	}
	return templateOut(r), nil
}

// ListTemplates returns every role template.
func (svc *Service) ListTemplates(ctx context.Context) ([]*RoleTemplate, error) {
	rows, err := svc.s.R().ListRoleTemplates(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*RoleTemplate, 0, len(rows))
	for _, r := range rows {
		out = append(out, templateOut(r))
	}
	return out, nil
}

// SaveSpaceRole creates or replaces a space's role.
func (svc *Service) SaveSpaceRole(ctx context.Context, actor audit.Actor, spaceID string, in SpaceRole) (*SpaceRole, error) {
	v := &apperr.Validation{}
	if !reRoleName.MatchString(in.Name) {
		v.Add("name", "must be a lowercase role name, e.g. implementor-backend")
	}
	if _, err := svc.s.R().GetRoleTemplate(ctx, in.TemplateID); err != nil {
		v.Add("template_id", "is not a role template")
	}
	if len(in.Model) > 128 || len(in.Instructions) > 50_000 {
		v.Add("instructions", "at most 50000 characters")
	}
	sj := settingsJSON(v, in.Settings)
	kj := svc.skillRefsJSON(ctx, v, in.Skills)
	svc.checkRefs(ctx, svc.s.R(), v, in.ProfileID, in.PromptID, in.PromptPin)
	if in.PromptPin != "" && in.PromptID == "" {
		v.Add("prompt_pin", "needs prompt_id")
	}
	if err := v.Err(); err != nil {
		return nil, err
	}
	err := svc.s.W().TxNamed(ctx, "catalog.space_role_save", func(tx *store.Tx) error {
		if _, err := tx.Q.GetSpace(ctx, spaceID); err != nil {
			return apperr.New(apperr.CodeNotFound, "space not found")
		}
		if err := tx.Q.UpsertSpaceRole(ctx, db.UpsertSpaceRoleParams{SpaceID: spaceID, Name: in.Name, TemplateID: in.TemplateID, ProfileID: in.ProfileID,
			PromptID: in.PromptID, PromptPin: in.PromptPin, Model: in.Model, SettingsJson: sj, Instructions: in.Instructions, SkillsJson: kj, Now: tx.NowMicros(),
			By: actor.ID}); err != nil {
			return err
		}
		if err := svc.changes.Record(ctx, tx, spaceID, "space", spaceID, "updated", 0); err != nil {
			return err
		}
		_, err := svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "space.role_saved", Target: audit.Target{Type: "space", ID: spaceID},
			Details: map[string]any{"role": in.Name, "template_id": in.TemplateID}})
		return err
	})
	if err != nil {
		return nil, err
	}
	r, err := svc.s.R().GetSpaceRole(ctx, db.GetSpaceRoleParams{SpaceID: spaceID, Name: in.Name})
	if err != nil {
		return nil, err
	}
	return spaceRoleOut(r), nil
}

// ListSpaceRoles returns a space's roles.
func (svc *Service) ListSpaceRoles(ctx context.Context, spaceID string) ([]*SpaceRole, error) {
	rows, err := svc.s.R().ListSpaceRoles(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	out := make([]*SpaceRole, 0, len(rows))
	for _, r := range rows {
		out = append(out, spaceRoleOut(r))
	}
	return out, nil
}

// DeleteSpaceRole removes a space role (runs already launched keep their manifest).
func (svc *Service) DeleteSpaceRole(ctx context.Context, actor audit.Actor, spaceID, name string) error {
	return svc.s.W().TxNamed(ctx, "catalog.space_role_delete", func(tx *store.Tx) error {
		n, err := tx.Q.DeleteSpaceRole(ctx, db.DeleteSpaceRoleParams{SpaceID: spaceID, Name: name})
		if err != nil {
			return err
		}
		if n == 0 {
			return apperr.New(apperr.CodeNotFound, "space role not found")
		}
		if err := svc.changes.Record(ctx, tx, spaceID, "space", spaceID, "updated", 0); err != nil {
			return err
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "space.role_deleted", Target: audit.Target{Type: "space", ID: spaceID},
			Details: map[string]any{"role": name}})
		return err
	})
}

// DeleteTemplate deletes a role template no space role uses (ADR-0051). Runs already launched keep their manifest.
func (svc *Service) DeleteTemplate(ctx context.Context, actor audit.Actor, id string) error {
	return svc.s.W().TxNamed(ctx, "catalog.template_delete", func(tx *store.Tx) error {
		t, err := tx.Q.GetRoleTemplate(ctx, id)
		if errors.Is(err, sql.ErrNoRows) {
			return apperr.New(apperr.CodeNotFound, "role template not found")
		} else if err != nil {
			return err
		}
		users, err := tx.Q.SpaceRolesUsingTemplate(ctx, id)
		if err != nil {
			return err
		}
		if len(users) > 0 {
			names := make([]string, 0, len(users))
			for _, u := range users {
				names = append(names, u.SpaceID+"/"+u.Name)
			}
			return apperr.New(apperr.CodePrecondition, "space roles use this template (%s); delete them or move them to another template first", strings.Join(names, ", "))
		}
		if _, err := tx.Q.DeleteRoleTemplate(ctx, id); err != nil {
			return err
		}
		return svc.log(ctx, tx, actor, "role_template.deleted", "role_template", id, map[string]any{"slug": t.Slug})
	})
}

// DeleteProfile deletes a harness profile no role template or space role uses (ADR-0051).
func (svc *Service) DeleteProfile(ctx context.Context, actor audit.Actor, id string) error {
	return svc.s.W().TxNamed(ctx, "catalog.profile_delete", func(tx *store.Tx) error {
		p, err := tx.Q.GetProfile(ctx, id)
		if errors.Is(err, sql.ErrNoRows) {
			return apperr.New(apperr.CodeNotFound, "harness profile not found")
		} else if err != nil {
			return err
		}
		tpls, err := tx.Q.TemplatesUsingProfile(ctx, id)
		if err != nil {
			return err
		}
		roles, err := tx.Q.SpaceRolesUsingProfile(ctx, id)
		if err != nil {
			return err
		}
		if len(tpls) > 0 || len(roles) > 0 {
			names := append([]string{}, tpls...)
			for _, r := range roles {
				names = append(names, r.SpaceID+"/"+r.Name)
			}
			return apperr.New(apperr.CodePrecondition, "role templates or space roles use this profile (%s); change them first", strings.Join(names, ", "))
		}
		if _, err := tx.Q.DeleteProfile(ctx, id); err != nil {
			return err
		}
		return svc.log(ctx, tx, actor, "harness_profile.deleted", "harness_profile", id, map[string]any{"slug": p.Slug})
	})
}

// ControlRole returns the space role control auto-start launches when the space never had a control run (ADR-0056):
// the role named control when its template is of kind control, otherwise the first such role by name; "" when the
// space has none.
func (svc *Service) ControlRole(ctx context.Context, spaceID string) (string, error) {
	roles, err := svc.ListSpaceRoles(ctx, spaceID)
	if err != nil {
		return "", err
	}
	tpls, err := svc.ListTemplates(ctx)
	if err != nil {
		return "", err
	}
	kind := map[string]string{}
	for _, t := range tpls {
		kind[t.ID] = t.Kind
	}
	found := ""
	for _, r := range roles {
		if kind[r.TemplateID] != "control" {
			continue
		}
		if r.Name == "control" {
			return r.Name, nil
		}
		if found == "" || r.Name < found {
			found = r.Name
		}
	}
	return found, nil
}
