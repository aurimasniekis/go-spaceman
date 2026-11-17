package catalog

import (
	"strings"
	"testing"
)

// SM-PROMPT-011: a space role's own description. Empty falls back to the template's, and the preview names the origin.
func TestSpaceRoleDescription(t *testing.T) {
	f := newFx(t)
	prof, err := f.svc.SaveProfile(f.ctx, op, "", 0, Profile{Slug: "claude-sonnet", Name: "Claude Sonnet", Harness: "claude-code", Model: "sonnet"})
	must(t, err)
	tpl, err := f.svc.SaveTemplate(f.ctx, op, "", 0, RoleTemplate{Slug: "implementor", Name: "Implementor", Kind: "worker", ProfileID: prof.ID, Description: "Template text."})
	must(t, err)

	saved, err := f.svc.SaveSpaceRole(f.ctx, op, "spc_a", SpaceRole{Name: "impl-a", TemplateID: tpl.ID, Description: "Hard backend work."})
	must(t, err)
	if saved.Description != "Hard backend work." {
		t.Fatalf("save returned %q", saved.Description)
	}
	_, err = f.svc.SaveSpaceRole(f.ctx, op, "spc_a", SpaceRole{Name: "impl-b", TemplateID: tpl.ID})
	must(t, err)

	roles, err := f.svc.ListSpaceRoles(f.ctx, "spc_a")
	must(t, err)
	got := map[string]string{}
	for _, r := range roles {
		got[r.Name] = r.Description
	}
	if got["impl-a"] != "Hard backend work." || got["impl-b"] != "" {
		t.Fatalf("list descriptions %v", got)
	}

	for role, want := range map[string]Field{
		"impl-a": {Value: "Hard backend work.", Origin: OriginSpaceRole},
		"impl-b": {Value: "Template text.", Origin: OriginTemplate},
	} {
		r, err := f.svc.Resolve(f.ctx, f.s.R(), f.space(), Launch{Role: role})
		must(t, err)
		if r.Effective["description"] != want {
			t.Errorf("%s: effective description %+v, want %+v", role, r.Effective["description"], want)
		}
		if _, ok := r.Manifest["description"]; ok {
			t.Errorf("%s: the description is in the manifest", role)
		}
	}

	// Saving again with an empty description clears it, so the template's applies again.
	_, err = f.svc.SaveSpaceRole(f.ctx, op, "spc_a", SpaceRole{Name: "impl-a", TemplateID: tpl.ID})
	must(t, err)
	r, err := f.svc.Resolve(f.ctx, f.s.R(), f.space(), Launch{Role: "impl-a"})
	must(t, err)
	if r.Effective["description"] != (Field{Value: "Template text.", Origin: OriginTemplate}) {
		t.Errorf("after clearing: %+v", r.Effective["description"])
	}

	_, err = f.svc.SaveSpaceRole(f.ctx, op, "spc_a", SpaceRole{Name: "impl-a", TemplateID: tpl.ID, Description: strings.Repeat("x", 2001)})
	if err == nil {
		t.Fatal("a 2001-character description was accepted")
	}
	_, err = f.svc.SaveSpaceRole(f.ctx, op, "spc_a", SpaceRole{Name: "impl-a", TemplateID: tpl.ID, Description: strings.Repeat("x", 2000)})
	must(t, err)
}
