package catalog

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Starter roles (ADR-0051) are example configuration SpaceMan adds once, on the first start of an installation, so a
// new space can launch useful agents without configuring prompts, profiles and templates first: two harness profiles,
// a prompt per role and a role template per role. They are ordinary catalog entries: operators edit or delete them,
// and a deleted one is not added again. A space gets space roles from them only when asked (AddStarterRoles).

//go:embed builtin/starter/*.md
var starterFS embed.FS

// StarterSeededSetting records that the starter roles were added (they are never added twice).
const StarterSeededSetting = "catalog.starter_roles_seeded"

// Starter profiles: one per harness, with the harness's own defaults.
const (
	StarterProfileClaude = "starter-claude-code"
	StarterProfileCodex  = "starter-codex"
)

// StarterRole is one starter role: the space role name and the template it comes from.
type StarterRole struct {
	// Name is the space role's name (control, planner, …).
	Name string `json:"name"`
	// Template is the role template's slug (starter-<name>).
	Template string `json:"template"`
	Kind     string `json:"kind" enum:"control,worker,kb,dreamer"`
	Label    string `json:"label"`
	// Description says what the role is for.
	Description string `json:"description"`
	prompt      string // the prompt file in builtin/starter, or the built-in dream prompt
	memory      string
}

// StarterRoles is the starter set, in the order spaces list them.
var StarterRoles = []StarterRole{
	{Name: "control", Template: "starter-control", Kind: "control", Label: "Control",
		Description: "Coordinates the space: triages the inbox, plans tasks, launches workers and reviews their results.", prompt: "control.md",
		memory: "Record decisions and their reasons, the state of ongoing work and what each worker role is good at."},
	{Name: "planner", Template: "starter-planner", Kind: "worker", Label: "Planner",
		Description: "Turns a goal into small, checkable tasks with acceptance criteria; does not change code.", prompt: "planner.md",
		memory: "Record the project's structure, its constraints and the planning conventions that worked."},
	{Name: "implementor", Template: "starter-implementor", Kind: "worker", Label: "Implementor",
		Description: "Makes the change a task describes in its own worktree, with tests, ready for review.", prompt: "implementor.md",
		memory: "Record the code base's conventions, how to build and test it, and pitfalls you hit."},
	{Name: "reviewer", Template: "starter-reviewer", Kind: "worker", Label: "Reviewer",
		Description: "Reviews a change for correctness, tests, security and maintainability before it is accepted.", prompt: "reviewer.md",
		memory: "Record recurring problems and the project's review standards."},
	{Name: "kb", Template: "starter-kb", Kind: "kb", Label: "Knowledge base support",
		Description: "Answers other agents' questions from the space's knowledge base, citing its files.", prompt: "kb.md"},
	{Name: "dreamer", Template: "starter-dreamer", Kind: "dreamer", Label: "Dreamer",
		Description: "Consolidates role memory from evidence, using SpaceMan's built-in dream prompt.", prompt: "dream-consolidation"},
}

// SeedStarterRoles adds the starter profiles, prompts and role templates on the first start. An entry whose slug
// already exists is left as it is. Once the set is complete it is recorded, so deleting an entry later is final.
func (svc *Service) SeedStarterRoles(ctx context.Context) error {
	if _, err := svc.s.R().GetSetting(ctx, StarterSeededSetting); err == nil {
		return nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	profiles := map[string]string{}
	for _, p := range []Profile{
		{Slug: StarterProfileClaude, Name: "Claude Code (starter)", Harness: "claude-code", Description: "Claude Code with its own defaults. A starter profile: edit or delete it."},
		{Slug: StarterProfileCodex, Name: "Codex (starter)", Harness: "codex", Description: "Codex with its own defaults. A starter profile: edit or delete it."},
	} {
		id, err := svc.profileBySlug(ctx, p.Slug)
		if err != nil {
			return err
		}
		if id == "" {
			created, err := svc.SaveProfile(ctx, audit.System, "", 0, p)
			if err != nil {
				return err
			}
			id = created.ID
		}
		profiles[p.Slug] = id
	}
	for _, r := range StarterRoles {
		if _, err := svc.s.R().GetRoleTemplateBySlug(ctx, r.Template); err == nil {
			continue
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		promptID, err := svc.starterPrompt(ctx, r)
		if err != nil {
			return err
		}
		if _, err := svc.SaveTemplate(ctx, audit.System, "", 0, RoleTemplate{Slug: r.Template, Name: r.Label + " (starter)", Kind: r.Kind,
			Description: r.Description + " A starter role: edit or delete it.", ProfileID: profiles[StarterProfileClaude], PromptID: promptID,
			MemoryConventions: r.memory, Settings: map[string]string{}}); err != nil {
			return err
		}
	}
	return svc.s.W().TxNamed(ctx, "catalog.starter_seeded", func(tx *store.Tx) error {
		return tx.Q.UpsertSetting(ctx, db.UpsertSettingParams{Key: StarterSeededSetting, Value: time.UnixMicro(tx.NowMicros()).UTC().Format(time.RFC3339),
			UpdatedAt: tx.NowMicros(), UpdatedBy: sql.NullString{String: audit.System.ID, Valid: true}})
	})
}

func (svc *Service) profileBySlug(ctx context.Context, slug string) (string, error) {
	ps, err := svc.s.R().ListProfiles(ctx)
	if err != nil {
		return "", err
	}
	for _, p := range ps {
		if p.Slug == slug {
			return p.ID, nil
		}
	}
	return "", nil
}

// starterPrompt returns the role's prompt, creating and publishing it when it does not exist yet.
func (svc *Service) starterPrompt(ctx context.Context, r StarterRole) (string, error) {
	if r.prompt == "dream-consolidation" {
		p, err := svc.s.R().GetPromptBySlug(ctx, "dream-consolidation")
		if err != nil {
			return "", err
		}
		return p.ID, nil
	}
	if p, err := svc.s.R().GetPromptBySlug(ctx, r.Template); err == nil {
		return p.ID, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	body, err := starterFS.ReadFile("builtin/starter/" + r.prompt)
	if err != nil {
		return "", err
	}
	kind := "role"
	if r.Kind == "kb" {
		kind = "kb"
	}
	p, err := svc.CreatePrompt(ctx, audit.System, r.Template, r.Label+" (starter)", kind, "The behavior of the starter "+r.Name+" role.", string(body))
	if err != nil {
		return "", err
	}
	for _, v := range p.Versions {
		if v.State == "draft" {
			if _, err := svc.Publish(ctx, audit.System, p.ID, v.Version); err != nil {
				return "", err
			}
		}
	}
	return p.ID, nil
}

// AddStarterRoles gives a space a role for every starter role whose template still exists and whose name the space
// does not use yet. It returns the names it added.
func (svc *Service) AddStarterRoles(ctx context.Context, actor audit.Actor, spaceID string) ([]string, error) {
	have, err := svc.ListSpaceRoles(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	used := map[string]bool{}
	for _, r := range have {
		used[r.Name] = true
	}
	added := []string{}
	for _, r := range StarterRoles {
		if used[r.Name] {
			continue
		}
		tpl, err := svc.s.R().GetRoleTemplateBySlug(ctx, r.Template)
		if errors.Is(err, sql.ErrNoRows) {
			continue // deleted by an operator
		} else if err != nil {
			return nil, err
		}
		if _, err := svc.SaveSpaceRole(ctx, actor, spaceID, SpaceRole{Name: r.Name, TemplateID: tpl.ID, Settings: map[string]string{}}); err != nil {
			return nil, err
		}
		added = append(added, r.Name)
	}
	return added, nil
}
