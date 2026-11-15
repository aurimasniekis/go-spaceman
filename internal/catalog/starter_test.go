package catalog

import (
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
)

// ADR-0051: the starter roles are added once and are ordinary catalog entries: a space gets them on request, ready to
// launch, and an operator can edit or delete them without SpaceMan adding them back.
func TestStarterRoles(t *testing.T) {
	f := newFx(t)
	must(t, f.svc.SeedBuiltins(f.ctx)) // the dreamer uses the built-in dream prompt
	must(t, f.svc.SeedStarterRoles(f.ctx))
	tpls, err := f.svc.ListTemplates(f.ctx)
	must(t, err)
	if len(tpls) != len(StarterRoles) {
		t.Fatalf("templates = %d, want %d", len(tpls), len(StarterRoles))
	}
	for _, r := range StarterRoles {
		tpl, err := f.s.R().GetRoleTemplateBySlug(f.ctx, r.Template)
		must(t, err)
		if tpl.Kind != r.Kind || tpl.PromptID == "" || tpl.ProfileID == "" {
			t.Fatalf("template %s: %+v", r.Template, tpl)
		}
	}
	profiles, err := f.svc.ListProfiles(f.ctx)
	must(t, err)
	if len(profiles) != 2 {
		t.Fatalf("profiles = %d, want the two starter profiles", len(profiles))
	}

	// A second start adds nothing, even after an operator deleted a starter template.
	planner, err := f.s.R().GetRoleTemplateBySlug(f.ctx, "starter-planner")
	must(t, err)
	must(t, f.svc.DeleteTemplate(f.ctx, op, planner.ID))
	must(t, f.svc.SeedStarterRoles(f.ctx))
	if tpls, _ := f.svc.ListTemplates(f.ctx); len(tpls) != len(StarterRoles)-1 {
		t.Fatalf("templates after a re-seed = %d: a deleted starter came back", len(tpls))
	}

	// A space gets the starter roles that still exist; an existing role of the same name is kept.
	ctrl, err := f.s.R().GetRoleTemplateBySlug(f.ctx, "starter-control")
	must(t, err)
	_, err = f.svc.SaveSpaceRole(f.ctx, op, "spc_a", SpaceRole{Name: "reviewer", TemplateID: ctrl.ID, Instructions: "mine"})
	must(t, err)
	added, err := f.svc.AddStarterRoles(f.ctx, op, "spc_a")
	must(t, err)
	if strings.Join(added, ",") != "control,implementor,kb,dreamer" {
		t.Fatalf("added %v", added)
	}
	if again, err := f.svc.AddStarterRoles(f.ctx, op, "spc_a"); err != nil || len(again) != 0 {
		t.Fatalf("second add: %v %v", again, err)
	}
	roles, err := f.svc.ListSpaceRoles(f.ctx, "spc_a")
	must(t, err)
	for _, r := range roles {
		if r.Name == "reviewer" && r.Instructions != "mine" {
			t.Fatal("an existing role was replaced")
		}
	}

	// A starter role launches with its kind, harness, prompt and memory conventions, without further configuration.
	res, err := f.svc.Resolve(f.ctx, f.s.R(), f.space(), Launch{Role: "implementor"})
	must(t, err)
	if res.Kind != "worker" || res.Harness != "claude-code" || res.Prompt == nil || res.Prompt.Slug != "starter-implementor" ||
		!strings.Contains(res.SystemText, "# Implementor") || !strings.Contains(res.SystemText, "## Memory conventions") {
		t.Fatalf("resolved implementor: kind %s harness %s prompt %+v", res.Kind, res.Harness, res.Prompt)
	}
	if res, err := f.svc.Resolve(f.ctx, f.s.R(), f.space(), Launch{Role: "dreamer"}); err != nil || res.Prompt == nil || res.Prompt.Slug != "dream-consolidation" {
		t.Fatalf("dreamer: %+v %v", res, err)
	}

	// Templates and profiles in use cannot be deleted; unused ones can.
	impl, err := f.s.R().GetRoleTemplateBySlug(f.ctx, "starter-implementor")
	must(t, err)
	code(t, f.svc.DeleteTemplate(f.ctx, op, impl.ID), apperr.CodePrecondition)
	var claudeID, codexID string
	for _, p := range profiles {
		switch p.Slug {
		case StarterProfileClaude:
			claudeID = p.ID
		case StarterProfileCodex:
			codexID = p.ID
		}
	}
	code(t, f.svc.DeleteProfile(f.ctx, op, claudeID), apperr.CodePrecondition)
	must(t, f.svc.DeleteProfile(f.ctx, op, codexID))
	code(t, f.svc.DeleteProfile(f.ctx, op, codexID), apperr.CodeNotFound)
	must(t, f.svc.DeleteSpaceRole(f.ctx, op, "spc_a", "implementor"))
	must(t, f.svc.DeleteTemplate(f.ctx, op, impl.ID))
	code(t, f.svc.DeleteTemplate(f.ctx, op, impl.ID), apperr.CodeNotFound)
}

// ADR-0056: control auto-start launches the space role named control when its template is of kind control, otherwise
// the first such role by name, and nothing when the space has none.
func TestControlRole(t *testing.T) {
	f := newFx(t)
	must(t, f.svc.SeedBuiltins(f.ctx))
	must(t, f.svc.SeedStarterRoles(f.ctx))
	if name, err := f.svc.ControlRole(f.ctx, "spc_a"); err != nil || name != "" {
		t.Fatalf("no roles: %q %v", name, err)
	}
	ctrl, err := f.s.R().GetRoleTemplateBySlug(f.ctx, "starter-control")
	must(t, err)
	impl, err := f.s.R().GetRoleTemplateBySlug(f.ctx, "starter-implementor")
	must(t, err)
	for _, r := range []SpaceRole{{Name: "worker", TemplateID: impl.ID}, {Name: "lead", TemplateID: ctrl.ID}, {Name: "chief", TemplateID: ctrl.ID}} {
		_, err := f.svc.SaveSpaceRole(f.ctx, op, "spc_a", r)
		must(t, err)
	}
	if name, err := f.svc.ControlRole(f.ctx, "spc_a"); err != nil || name != "chief" {
		t.Fatalf("first control role by name: %q %v", name, err)
	}
	// A role named control that is not of kind control does not count; one that is wins.
	_, err = f.svc.SaveSpaceRole(f.ctx, op, "spc_a", SpaceRole{Name: "control", TemplateID: impl.ID})
	must(t, err)
	if name, _ := f.svc.ControlRole(f.ctx, "spc_a"); name != "chief" {
		t.Fatalf("a worker role named control was chosen: %q", name)
	}
	must(t, f.svc.DeleteSpaceRole(f.ctx, op, "spc_a", "control"))
	_, err = f.svc.SaveSpaceRole(f.ctx, op, "spc_a", SpaceRole{Name: "control", TemplateID: ctrl.ID})
	must(t, err)
	if name, _ := f.svc.ControlRole(f.ctx, "spc_a"); name != "control" {
		t.Fatalf("control role: %q", name)
	}
}

// ADR-0066: SpaceMan's own orientation (every run kind) and every starter prompt tell the agent to ask people through
// spaceman_human_ask, never through an interactive terminal prompt.
func TestPromptsRouteQuestionsToHumanActions(t *testing.T) {
	for _, kind := range []string{"control", "worker", "kb", "dreamer", "diagnostic"} {
		o := builtinOrientation(kind, "", "Payments")
		if !strings.Contains(o, "Never ask through an interactive terminal prompt") || !strings.Contains(o, "use spaceman_human_ask") ||
			!strings.Contains(o, "use spaceman_attachment_save") { // ADR-0073: large attachments and images
			t.Fatalf("%s orientation: %s", kind, o)
		}
	}
	for _, r := range StarterRoles {
		if !strings.HasSuffix(r.prompt, ".md") {
			continue // the dreamer uses the built-in dream prompt, after the orientation
		}
		b, err := starterFS.ReadFile("builtin/starter/" + r.prompt)
		must(t, err)
		if !strings.Contains(string(b), "Never ask through an interactive terminal prompt") || !strings.Contains(string(b), "`spaceman_human_ask`") {
			t.Errorf("starter prompt %s does not route questions to spaceman_human_ask", r.prompt)
		}
	}
}
