package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/catalog"
	"github.com/aurimasniekis/go-spaceman/internal/store"
)

// CatalogRules are the authorization rules of the prompt, profile, role template and space role operations.
var CatalogRules = map[string]Rule{
	"list-prompts":           Global(authz.PromptRead),
	"get-prompt":             Global(authz.PromptRead),
	"create-prompt":          Global(authz.PromptWrite),
	"save-prompt-draft":      Global(authz.PromptWrite),
	"publish-prompt":         Global(authz.PromptWrite),
	"discard-prompt-draft":   Global(authz.PromptWrite),
	"rollback-prompt":        Global(authz.PromptWrite),
	"fork-prompt":            Global(authz.PromptWrite),
	"diff-prompt-versions":   Global(authz.PromptRead),
	"list-harness-profiles":  Global(authz.PromptRead),
	"get-harness-profile":    Global(authz.PromptRead),
	"save-harness-profile":   Global(authz.PromptWrite),
	"delete-harness-profile": Global(authz.PromptWrite),
	"list-role-templates":    Global(authz.PromptRead),
	"get-role-template":      Global(authz.PromptRead),
	"save-role-template":     Global(authz.PromptWrite),
	"delete-role-template":   Global(authz.PromptWrite),
	"list-starter-roles":     Global(authz.PromptRead),
	"list-role-options":      InSpace(authz.SpaceConfigRead),
	"list-space-roles":       InSpace(authz.SpaceConfigRead),
	"get-space-role":         InSpace(authz.SpaceConfigRead),
	"save-space-role":        InSpace(authz.SpaceConfigWrite),
	"delete-space-role":      InSpace(authz.SpaceConfigWrite),
	"add-starter-roles":      InSpace(authz.SpaceConfigWrite),
	"preview-space-role":     InSpace(authz.SpaceConfigRead),
	"list-skills":            Global(authz.SkillRead),
	"get-skill":              Global(authz.SkillRead),
	"create-skill":           Global(authz.SkillWrite),
	"save-skill-draft":       Global(authz.SkillWrite),
	"publish-skill":          Global(authz.SkillWrite),
}

// ProfileInput creates or edits a harness profile (slug and harness are fixed after creation).
type ProfileInput struct {
	Slug        string            `json:"slug,omitempty" maxLength:"63"`
	Name        string            `json:"name" maxLength:"120"`
	Harness     string            `json:"harness,omitempty" enum:"claude-code,codex"`
	Model       string            `json:"model,omitempty" maxLength:"128"`
	Settings    map[string]string `json:"settings,omitempty"`
	Description string            `json:"description,omitempty" maxLength:"2000"`
}

// TemplateInput creates or edits a role template (slug and kind are fixed after creation).
type TemplateInput struct {
	Slug              string             `json:"slug,omitempty" maxLength:"63"`
	Name              string             `json:"name" maxLength:"120"`
	Kind              string             `json:"kind,omitempty" enum:"control,worker,kb,dreamer,diagnostic"`
	Description       string             `json:"description,omitempty" maxLength:"2000"`
	ProfileID         string             `json:"profile_id,omitempty" maxLength:"64"`
	PromptID          string             `json:"prompt_id,omitempty" maxLength:"64"`
	PromptPin         string             `json:"prompt_pin,omitempty" maxLength:"64" doc:"A published version ID; empty follows the latest"`
	MemoryConventions string             `json:"memory_conventions,omitempty" maxLength:"20000"`
	Settings          map[string]string  `json:"settings,omitempty"`
	Skills            []catalog.SkillRef `json:"skills,omitempty" maxItems:"20"`
}

// SpaceRoleInput configures a role in a space; empty fields use the template's values.
type SpaceRoleInput struct {
	TemplateID   string             `json:"template_id" maxLength:"64"`
	ProfileID    string             `json:"profile_id,omitempty" maxLength:"64"`
	PromptID     string             `json:"prompt_id,omitempty" maxLength:"64"`
	PromptPin    string             `json:"prompt_pin,omitempty" maxLength:"64"`
	Model        string             `json:"model,omitempty" maxLength:"128"`
	Settings     map[string]string  `json:"settings,omitempty"`
	Instructions string             `json:"instructions,omitempty" maxLength:"50000"`
	Skills       []catalog.SkillRef `json:"skills,omitempty" maxItems:"20" doc:"Added to the template's skills; a pin here wins"`
}

// PromptPath identifies a prompt (ID or slug).
type PromptPath struct {
	PromptID string `path:"prompt_id" maxLength:"64"`
}

// RoleOptions lists what a space role can refer to (names only; no prompt bodies).
type RoleOptions struct {
	Templates []RoleOption `json:"templates"`
	Profiles  []RoleOption `json:"profiles"`
	Prompts   []RoleOption `json:"prompts"`
	Skills    []RoleOption `json:"skills"`
}

// RoleOption is one choice.
type RoleOption struct {
	ID       string           `json:"id"`
	Slug     string           `json:"slug"`
	Name     string           `json:"name"`
	Kind     string           `json:"kind,omitempty"`
	Versions []PromptVersionN `json:"versions,omitempty"`
}

// PromptVersionN is a published prompt version by number.
type PromptVersionN struct {
	ID     string `json:"id"`
	Number int64  `json:"number"`
}

// Preview is a space role's effective configuration with origins (no launch).
type Preview struct {
	Kind       string                   `json:"kind"`
	Harness    string                   `json:"harness"`
	Model      string                   `json:"model,omitempty"`
	Settings   map[string]string        `json:"settings"`
	SystemText string                   `json:"system_text"`
	Effective  map[string]catalog.Field `json:"effective"`
	Prompt     *catalog.PromptRef       `json:"prompt,omitempty"`
	Skills     []catalog.ResolvedSkill  `json:"skills"`
}

// RegisterCatalog registers the catalog operations.
func RegisterCatalog(svc *catalog.Service, st *store.Store) func(api huma.API, d *Deps) {
	return func(api huma.API, d *Deps) {
		tags := []string{"prompts"}
		huma.Register(api, huma.Operation{OperationID: "list-prompts", Method: http.MethodGet, Path: "/prompts", Summary: "The prompt catalog", Tags: tags},
			func(ctx context.Context, _ *struct{}) (*struct {
				Body struct {
					Items []*catalog.Prompt `json:"items"`
				}
			}, error) {
				l, err := svc.ListPrompts(ctx)
				if err != nil {
					return nil, d.fail(ctx, "prompts.list", err)
				}
				out := &struct {
					Body struct {
						Items []*catalog.Prompt `json:"items"`
					}
				}{}
				out.Body.Items = l
				return out, nil
			})
		huma.Register(api, huma.Operation{OperationID: "get-prompt", Method: http.MethodGet, Path: "/prompts/{prompt_id}", Summary: "A prompt with its draft and versions", Tags: tags},
			func(ctx context.Context, in *PromptPath) (*struct{ Body *catalog.Prompt }, error) {
				p, err := svc.GetPrompt(ctx, in.PromptID)
				if err != nil {
					return nil, d.fail(ctx, "prompts.get", err)
				}
				return &struct{ Body *catalog.Prompt }{Body: p}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "create-prompt", Method: http.MethodPost, Path: "/prompts", Summary: "Add a prompt (with a first draft)",
			Tags: tags, DefaultStatus: http.StatusCreated},
			func(ctx context.Context, in *struct {
				Body struct {
					Slug        string `json:"slug" maxLength:"63"`
					Name        string `json:"name" maxLength:"120"`
					Kind        string `json:"kind" enum:"role,dream,kb,event,general"`
					Description string `json:"description,omitempty" maxLength:"2000"`
					Body        string `json:"body,omitempty" maxLength:"200000" doc:"Markdown"`
				}
			}) (*struct{ Body *catalog.Prompt }, error) {
				p, err := svc.CreatePrompt(ctx, Actor(ctx), in.Body.Slug, in.Body.Name, in.Body.Kind, in.Body.Description, in.Body.Body)
				if err != nil {
					return nil, d.fail(ctx, "prompts.create", err)
				}
				return &struct{ Body *catalog.Prompt }{Body: p}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "save-prompt-draft", Method: http.MethodPut, Path: "/prompts/{prompt_id}/draft",
			Summary: "Create or update the prompt's draft", Description: "Without If-Match, a new draft is started (refused if one exists); with it, the draft at that version is updated.", Tags: tags},
			func(ctx context.Context, in *struct {
				PromptPath
				IfMatch
				Body struct {
					Body    string `json:"body" maxLength:"200000"`
					Extends string `json:"extends,omitempty" maxLength:"64" doc:"A published version of another prompt to build on"`
					Notes   string `json:"notes,omitempty" maxLength:"2000"`
				}
			}) (*struct{ Body *catalog.PromptVersion }, error) {
				v := int64(0)
				if in.IfMatch.IfMatch != "" {
					var err error
					if v, err = in.Version(); err != nil {
						return nil, d.fail(ctx, "prompts.draft", err)
					}
				}
				pv, err := svc.SaveDraft(ctx, Actor(ctx), in.PromptID, v, in.Body.Body, in.Body.Extends, in.Body.Notes)
				if err != nil {
					return nil, d.fail(ctx, "prompts.draft", err)
				}
				return &struct{ Body *catalog.PromptVersion }{Body: pv}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "publish-prompt", Method: http.MethodPost, Path: "/prompts/{prompt_id}/publish",
			Summary: "Publish the draft as the next immutable version", Description: "Roles that follow the prompt use it from their next launch.", Tags: tags},
			func(ctx context.Context, in *struct {
				PromptPath
				IfMatch
			}) (*struct{ Body *catalog.PromptVersion }, error) {
				v, err := in.Version()
				if err != nil {
					return nil, d.fail(ctx, "prompts.publish", err)
				}
				pv, err := svc.Publish(ctx, Actor(ctx), in.PromptID, v)
				if err != nil {
					return nil, d.fail(ctx, "prompts.publish", err)
				}
				return &struct{ Body *catalog.PromptVersion }{Body: pv}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "discard-prompt-draft", Method: http.MethodDelete, Path: "/prompts/{prompt_id}/draft", Summary: "Discard the draft",
			Tags: tags, DefaultStatus: http.StatusNoContent},
			func(ctx context.Context, in *PromptPath) (*struct{}, error) {
				if err := svc.DiscardDraft(ctx, Actor(ctx), in.PromptID); err != nil {
					return nil, d.fail(ctx, "prompts.discard", err)
				}
				return &struct{}{}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "rollback-prompt", Method: http.MethodPost, Path: "/prompts/{prompt_id}/rollback",
			Summary: "Publish an earlier version's content as a new version", Tags: tags},
			func(ctx context.Context, in *struct {
				PromptPath
				Body struct {
					VersionID string `json:"version_id" maxLength:"64"`
				}
			}) (*struct{ Body *catalog.PromptVersion }, error) {
				pv, err := svc.Rollback(ctx, Actor(ctx), in.PromptID, in.Body.VersionID)
				if err != nil {
					return nil, d.fail(ctx, "prompts.rollback", err)
				}
				return &struct{ Body *catalog.PromptVersion }{Body: pv}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "fork-prompt", Method: http.MethodPost, Path: "/prompt-versions/{version_id}/fork",
			Summary: "Fork a published version into a new prompt", Description: "The new prompt records where it came from; forks never merge back automatically.",
			Tags: tags, DefaultStatus: http.StatusCreated},
			func(ctx context.Context, in *struct {
				VersionID string `path:"version_id" maxLength:"64"`
				Body      struct {
					Slug string `json:"slug" maxLength:"63"`
					Name string `json:"name" maxLength:"120"`
				}
			}) (*struct{ Body *catalog.Prompt }, error) {
				p, err := svc.Fork(ctx, Actor(ctx), in.VersionID, in.Body.Slug, in.Body.Name)
				if err != nil {
					return nil, d.fail(ctx, "prompts.fork", err)
				}
				return &struct{ Body *catalog.Prompt }{Body: p}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "diff-prompt-versions", Method: http.MethodGet, Path: "/prompt-diff", Summary: "Line diff between two prompt versions",
			Tags: tags},
			func(ctx context.Context, in *struct {
				From string `query:"from" maxLength:"64"`
				To   string `query:"to" maxLength:"64"`
			}) (*struct {
				Body struct {
					Lines []catalog.DiffLine `json:"lines"`
				}
			}, error) {
				l, err := svc.Diff(ctx, in.From, in.To)
				if err != nil {
					return nil, d.fail(ctx, "prompts.diff", err)
				}
				out := &struct {
					Body struct {
						Lines []catalog.DiffLine `json:"lines"`
					}
				}{}
				out.Body.Lines = l
				return out, nil
			})

		stags := []string{"skills"}
		huma.Register(api, huma.Operation{OperationID: "list-skills", Method: http.MethodGet, Path: "/skills", Summary: "The skill catalog", Tags: stags},
			func(ctx context.Context, _ *struct{}) (*struct {
				Body struct {
					Items []*catalog.Skill `json:"items"`
				}
			}, error) {
				l, err := svc.ListSkills(ctx)
				if err != nil {
					return nil, d.fail(ctx, "skills.list", err)
				}
				out := &struct {
					Body struct {
						Items []*catalog.Skill `json:"items"`
					}
				}{}
				out.Body.Items = l
				return out, nil
			})
		huma.Register(api, huma.Operation{OperationID: "get-skill", Method: http.MethodGet, Path: "/skills/{skill_id}", Summary: "A skill with its versions and files", Tags: stags},
			func(ctx context.Context, in *struct {
				SkillID string `path:"skill_id" maxLength:"64"`
			}) (*struct{ Body *catalog.Skill }, error) {
				sk, err := svc.GetSkill(ctx, in.SkillID)
				if err != nil {
					return nil, d.fail(ctx, "skills.get", err)
				}
				return &struct{ Body *catalog.Skill }{Body: sk}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "create-skill", Method: http.MethodPost, Path: "/skills", Summary: "Add a skill (with a first draft)",
			Description: "Files are text; SKILL.md needs frontmatter naming the skill. Skills give guidance, never permission, and nothing in them runs at install time.",
			Tags:        stags, DefaultStatus: http.StatusCreated},
			func(ctx context.Context, in *struct {
				Body struct {
					Slug        string            `json:"slug" maxLength:"63"`
					Name        string            `json:"name" maxLength:"120"`
					Description string            `json:"description,omitempty" maxLength:"2000"`
					Files       map[string]string `json:"files"`
				}
			}) (*struct{ Body *catalog.Skill }, error) {
				sk, err := svc.CreateSkill(ctx, Actor(ctx), in.Body.Slug, in.Body.Name, in.Body.Description, in.Body.Files)
				if err != nil {
					return nil, d.fail(ctx, "skills.create", err)
				}
				return &struct{ Body *catalog.Skill }{Body: sk}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "save-skill-draft", Method: http.MethodPut, Path: "/skills/{skill_id}/draft",
			Summary: "Create or update the skill's draft", Description: "Without If-Match a new draft is started; with it, the draft at that version is updated.", Tags: stags},
			func(ctx context.Context, in *struct {
				SkillID string `path:"skill_id" maxLength:"64"`
				IfMatch
				Body struct {
					Files map[string]string `json:"files"`
					Notes string            `json:"notes,omitempty" maxLength:"2000"`
				}
			}) (*struct{ Body *catalog.Skill }, error) {
				v := int64(0)
				if in.IfMatch.IfMatch != "" {
					var err error
					if v, err = in.Version(); err != nil {
						return nil, d.fail(ctx, "skills.draft", err)
					}
				}
				sk, err := svc.SaveSkillDraft(ctx, Actor(ctx), in.SkillID, v, in.Body.Files, in.Body.Notes)
				if err != nil {
					return nil, d.fail(ctx, "skills.draft", err)
				}
				return &struct{ Body *catalog.Skill }{Body: sk}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "publish-skill", Method: http.MethodPost, Path: "/skills/{skill_id}/publish",
			Summary: "Publish the draft as the next immutable version", Tags: stags},
			func(ctx context.Context, in *struct {
				SkillID string `path:"skill_id" maxLength:"64"`
				IfMatch
			}) (*struct{ Body *catalog.Skill }, error) {
				v, err := in.Version()
				if err != nil {
					return nil, d.fail(ctx, "skills.publish", err)
				}
				sk, err := svc.PublishSkill(ctx, Actor(ctx), in.SkillID, v)
				if err != nil {
					return nil, d.fail(ctx, "skills.publish", err)
				}
				return &struct{ Body *catalog.Skill }{Body: sk}, nil
			})

		tags = []string{"roles"}
		huma.Register(api, huma.Operation{OperationID: "list-harness-profiles", Method: http.MethodGet, Path: "/harness-profiles", Summary: "Harness profiles", Tags: tags},
			func(ctx context.Context, _ *struct{}) (*struct {
				Body struct {
					Items []*catalog.Profile `json:"items"`
				}
			}, error) {
				l, err := svc.ListProfiles(ctx)
				if err != nil {
					return nil, d.fail(ctx, "profiles.list", err)
				}
				out := &struct {
					Body struct {
						Items []*catalog.Profile `json:"items"`
					}
				}{}
				out.Body.Items = l
				return out, nil
			})
		huma.Register(api, huma.Operation{OperationID: "get-harness-profile", Method: http.MethodGet, Path: "/harness-profiles/{profile_id}",
			Summary: "A harness profile (by ID or slug)", Tags: tags},
			func(ctx context.Context, in *struct {
				ProfileID string `path:"profile_id" maxLength:"64"`
			}) (*struct{ Body *catalog.Profile }, error) {
				l, err := svc.ListProfiles(ctx)
				if err != nil {
					return nil, d.fail(ctx, "profiles.get", err)
				}
				for _, p := range l {
					if p.ID == in.ProfileID || p.Slug == in.ProfileID {
						return &struct{ Body *catalog.Profile }{Body: p}, nil
					}
				}
				return nil, d.fail(ctx, "profiles.get", apperr.New(apperr.CodeNotFound, "harness profile not found"))
			})
		huma.Register(api, huma.Operation{OperationID: "save-harness-profile", Method: http.MethodPut, Path: "/harness-profiles/{profile_id}",
			Summary: "Create (profile_id \"new\") or update a harness profile", Description: "An update bumps the revision; it applies to later launches.", Tags: tags},
			func(ctx context.Context, in *struct {
				ProfileID string `path:"profile_id" maxLength:"64"`
				IfMatch
				Body ProfileInput
			}) (*struct{ Body *catalog.Profile }, error) {
				id, v := in.ProfileID, int64(0)
				if id == "new" {
					id = ""
				} else {
					var err error
					if v, err = in.Version(); err != nil {
						return nil, d.fail(ctx, "profiles.save", err)
					}
				}
				b := in.Body
				p, err := svc.SaveProfile(ctx, Actor(ctx), id, v, catalog.Profile{Slug: b.Slug, Name: b.Name, Harness: b.Harness, Model: b.Model, Settings: b.Settings,
					Description: b.Description})
				if err != nil {
					return nil, d.fail(ctx, "profiles.save", err)
				}
				return &struct{ Body *catalog.Profile }{Body: p}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "delete-harness-profile", Method: http.MethodDelete, Path: "/harness-profiles/{profile_id}",
			Summary: "Delete a harness profile (by ID or slug) no role template or space role uses", Tags: tags, DefaultStatus: http.StatusNoContent},
			func(ctx context.Context, in *struct {
				ProfileID string `path:"profile_id" maxLength:"64"`
			}) (*struct{}, error) {
				id := in.ProfileID
				if ps, err := svc.ListProfiles(ctx); err == nil {
					for _, p := range ps {
						if p.Slug == id {
							id = p.ID
						}
					}
				}
				if err := svc.DeleteProfile(ctx, Actor(ctx), id); err != nil {
					return nil, d.fail(ctx, "profiles.delete", err)
				}
				return &struct{}{}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "list-role-templates", Method: http.MethodGet, Path: "/role-templates", Summary: "Role templates", Tags: tags},
			func(ctx context.Context, _ *struct{}) (*struct {
				Body struct {
					Items []*catalog.RoleTemplate `json:"items"`
				}
			}, error) {
				l, err := svc.ListTemplates(ctx)
				if err != nil {
					return nil, d.fail(ctx, "templates.list", err)
				}
				out := &struct {
					Body struct {
						Items []*catalog.RoleTemplate `json:"items"`
					}
				}{}
				out.Body.Items = l
				return out, nil
			})
		huma.Register(api, huma.Operation{OperationID: "get-role-template", Method: http.MethodGet, Path: "/role-templates/{template_id}",
			Summary: "A role template (by ID or slug)", Tags: tags},
			func(ctx context.Context, in *struct {
				TemplateID string `path:"template_id" maxLength:"64"`
			}) (*struct{ Body *catalog.RoleTemplate }, error) {
				l, err := svc.ListTemplates(ctx)
				if err != nil {
					return nil, d.fail(ctx, "templates.get", err)
				}
				for _, t := range l {
					if t.ID == in.TemplateID || t.Slug == in.TemplateID {
						return &struct{ Body *catalog.RoleTemplate }{Body: t}, nil
					}
				}
				return nil, d.fail(ctx, "templates.get", apperr.New(apperr.CodeNotFound, "role template not found"))
			})
		huma.Register(api, huma.Operation{OperationID: "save-role-template", Method: http.MethodPut, Path: "/role-templates/{template_id}",
			Summary: "Create (template_id \"new\") or update a role template", Tags: tags},
			func(ctx context.Context, in *struct {
				TemplateID string `path:"template_id" maxLength:"64"`
				IfMatch
				Body TemplateInput
			}) (*struct{ Body *catalog.RoleTemplate }, error) {
				id, v := in.TemplateID, int64(0)
				if id == "new" {
					id = ""
				} else {
					var err error
					if v, err = in.Version(); err != nil {
						return nil, d.fail(ctx, "templates.save", err)
					}
				}
				b := in.Body
				t, err := svc.SaveTemplate(ctx, Actor(ctx), id, v, catalog.RoleTemplate{Slug: b.Slug, Name: b.Name, Kind: b.Kind, Description: b.Description,
					ProfileID: b.ProfileID, PromptID: b.PromptID, PromptPin: b.PromptPin, MemoryConventions: b.MemoryConventions, Settings: b.Settings, Skills: b.Skills})
				if err != nil {
					return nil, d.fail(ctx, "templates.save", err)
				}
				return &struct{ Body *catalog.RoleTemplate }{Body: t}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "list-role-options", Method: http.MethodGet, Path: "/spaces/{space_id}/role-options",
			Summary: "Templates, profiles and prompts a space role can use (names only)", Tags: tags},
			func(ctx context.Context, in *SpacePath) (*struct{ Body RoleOptions }, error) {
				out := RoleOptions{Templates: []RoleOption{}, Profiles: []RoleOption{}, Prompts: []RoleOption{}, Skills: []RoleOption{}}
				sks, err := svc.ListSkills(ctx)
				if err != nil {
					return nil, d.fail(ctx, "roles.options", err)
				}
				for _, sk := range sks {
					o := RoleOption{ID: sk.ID, Slug: sk.Slug, Name: sk.Name, Kind: sk.Source}
					for _, v := range sk.Versions {
						if v.State == "published" {
							o.Versions = append(o.Versions, PromptVersionN{ID: v.ID, Number: v.Number})
						}
					}
					out.Skills = append(out.Skills, o)
				}
				ts, err := svc.ListTemplates(ctx)
				if err != nil {
					return nil, d.fail(ctx, "roles.options", err)
				}
				for _, t := range ts {
					out.Templates = append(out.Templates, RoleOption{ID: t.ID, Slug: t.Slug, Name: t.Name, Kind: t.Kind})
				}
				ps, err := svc.ListProfiles(ctx)
				if err != nil {
					return nil, d.fail(ctx, "roles.options", err)
				}
				for _, p := range ps {
					out.Profiles = append(out.Profiles, RoleOption{ID: p.ID, Slug: p.Slug, Name: p.Name, Kind: p.Harness})
				}
				prs, err := svc.ListPrompts(ctx)
				if err != nil {
					return nil, d.fail(ctx, "roles.options", err)
				}
				for _, p := range prs {
					full, err := svc.GetPrompt(ctx, p.ID)
					if err != nil {
						return nil, d.fail(ctx, "roles.options", err)
					}
					o := RoleOption{ID: p.ID, Slug: p.Slug, Name: p.Name, Kind: p.Kind}
					for _, v := range full.Versions {
						if v.State == "published" {
							o.Versions = append(o.Versions, PromptVersionN{ID: v.ID, Number: v.Number})
						}
					}
					out.Prompts = append(out.Prompts, o)
				}
				return &struct{ Body RoleOptions }{Body: out}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "list-space-roles", Method: http.MethodGet, Path: "/spaces/{space_id}/roles", Summary: "A space's roles", Tags: tags},
			func(ctx context.Context, in *SpacePath) (*struct {
				Body struct {
					Items []*catalog.SpaceRole `json:"items"`
				}
			}, error) {
				l, err := svc.ListSpaceRoles(ctx, in.SpaceID)
				if err != nil {
					return nil, d.fail(ctx, "roles.list", err)
				}
				out := &struct {
					Body struct {
						Items []*catalog.SpaceRole `json:"items"`
					}
				}{}
				out.Body.Items = l
				return out, nil
			})
		huma.Register(api, huma.Operation{OperationID: "get-space-role", Method: http.MethodGet, Path: "/spaces/{space_id}/roles/{name}",
			Summary: "One of a space's roles", Tags: tags},
			func(ctx context.Context, in *struct {
				SpacePath
				Name string `path:"name" maxLength:"48"`
			}) (*struct{ Body *catalog.SpaceRole }, error) {
				l, err := svc.ListSpaceRoles(ctx, in.SpaceID)
				if err != nil {
					return nil, d.fail(ctx, "roles.get", err)
				}
				for _, r := range l {
					if r.Name == in.Name {
						return &struct{ Body *catalog.SpaceRole }{Body: r}, nil
					}
				}
				return nil, d.fail(ctx, "roles.get", apperr.New(apperr.CodeNotFound, "role not found in this space"))
			})
		huma.Register(api, huma.Operation{OperationID: "save-space-role", Method: http.MethodPut, Path: "/spaces/{space_id}/roles/{name}",
			Summary: "Configure a role in the space", Description: "Non-empty fields override the role template's; changes apply to later launches.", Tags: tags},
			func(ctx context.Context, in *struct {
				SpacePath
				Name string `path:"name" maxLength:"48"`
				Body SpaceRoleInput
			}) (*struct{ Body *catalog.SpaceRole }, error) {
				b := in.Body
				r, err := svc.SaveSpaceRole(ctx, Actor(ctx), in.SpaceID, catalog.SpaceRole{Name: in.Name, TemplateID: b.TemplateID, ProfileID: b.ProfileID,
					PromptID: b.PromptID, PromptPin: b.PromptPin, Model: b.Model, Settings: b.Settings, Instructions: b.Instructions, Skills: b.Skills})
				if err != nil {
					return nil, d.fail(ctx, "roles.save", err)
				}
				return &struct{ Body *catalog.SpaceRole }{Body: r}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "delete-role-template", Method: http.MethodDelete, Path: "/role-templates/{template_id}",
			Summary: "Delete a role template (by ID or slug) no space role uses", Tags: tags, DefaultStatus: http.StatusNoContent},
			func(ctx context.Context, in *struct {
				TemplateID string `path:"template_id" maxLength:"64"`
			}) (*struct{}, error) {
				id := in.TemplateID
				if t, err := st.R().GetRoleTemplateBySlug(ctx, id); err == nil {
					id = t.ID
				}
				if err := svc.DeleteTemplate(ctx, Actor(ctx), id); err != nil {
					return nil, d.fail(ctx, "templates.delete", err)
				}
				return &struct{}{}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "list-starter-roles", Method: http.MethodGet, Path: "/starter-roles",
			Summary: "The starter roles and whether each one's template still exists", Tags: tags,
			Description: "SpaceMan adds the starter profiles, prompts and role templates once, on the first start; operators edit or delete them. " +
				"POST /spaces/{space_id}/starter-roles gives a space a role for each starter role whose template still exists."},
			func(ctx context.Context, _ *struct{}) (*struct{ Body StarterRolesOut }, error) {
				out := StarterRolesOut{Items: []StarterRoleOut{}}
				for _, r := range catalog.StarterRoles {
					o := StarterRoleOut{StarterRole: r}
					if t, err := st.R().GetRoleTemplateBySlug(ctx, r.Template); err == nil {
						o.TemplateID = t.ID
					}
					out.Items = append(out.Items, o)
				}
				return &struct{ Body StarterRolesOut }{Body: out}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "add-starter-roles", Method: http.MethodPost, Path: "/spaces/{space_id}/starter-roles",
			Summary: "Give the space the starter roles it does not have yet", Tags: tags,
			Description: "Adds a space role (control, planner, implementor, reviewer, kb, dreamer) for each starter role whose template still " +
				"exists and whose name the space does not use. Existing roles are never changed."},
			func(ctx context.Context, in *struct {
				SpacePath
			}) (*struct{ Body AddedRolesOut }, error) {
				added, err := svc.AddStarterRoles(ctx, Actor(ctx), in.SpaceID)
				if err != nil {
					return nil, d.fail(ctx, "roles.starter", err)
				}
				return &struct{ Body AddedRolesOut }{Body: AddedRolesOut{Added: added}}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "delete-space-role", Method: http.MethodDelete, Path: "/spaces/{space_id}/roles/{name}",
			Summary: "Remove a role from the space", Tags: tags, DefaultStatus: http.StatusNoContent},
			func(ctx context.Context, in *struct {
				SpacePath
				Name string `path:"name" maxLength:"48"`
			}) (*struct{}, error) {
				if err := svc.DeleteSpaceRole(ctx, Actor(ctx), in.SpaceID, in.Name); err != nil {
					return nil, d.fail(ctx, "roles.delete", err)
				}
				return &struct{}{}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "preview-space-role", Method: http.MethodGet, Path: "/spaces/{space_id}/roles/{name}/preview",
			Summary: "A role's effective configuration and where each value comes from", Tags: tags},
			func(ctx context.Context, in *struct {
				SpacePath
				Name string `path:"name" maxLength:"48"`
			}) (*struct{ Body Preview }, error) {
				sp, err := st.R().GetSpace(ctx, in.SpaceID)
				if err != nil {
					return nil, d.fail(ctx, "roles.preview", err)
				}
				r, err := svc.Resolve(ctx, st.R(), sp, catalog.Launch{Role: in.Name})
				if err != nil {
					return nil, d.fail(ctx, "roles.preview", err)
				}
				return &struct{ Body Preview }{Body: Preview{Kind: r.Kind, Harness: r.Harness, Model: r.Model, Settings: r.Settings, SystemText: r.SystemText,
					Effective: r.Effective, Prompt: r.Prompt, Skills: append([]catalog.ResolvedSkill{}, r.Skills...)}}, nil
			})
	}
}

// StarterRoleOut is a starter role and its template, when it still exists.
type StarterRoleOut struct {
	catalog.StarterRole
	TemplateID string `json:"template_id,omitempty" doc:"Empty when an operator deleted the starter template"`
}

// StarterRolesOut lists the starter roles.
type StarterRolesOut struct {
	Items []StarterRoleOut `json:"items"`
}

// AddedRolesOut names the space roles a request added.
type AddedRolesOut struct {
	Added []string `json:"added" doc:"The roles added; empty when the space had them all or their templates were deleted"`
}
