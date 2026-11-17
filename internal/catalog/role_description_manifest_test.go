package catalog

import (
	"encoding/json"
	"strings"
	"testing"
)

// ADR-0095 §3: a role's description never reaches the run manifest.
func TestRoleDescriptionNotInManifest(t *testing.T) {
	f := newFx(t)
	prof, err := f.svc.SaveProfile(f.ctx, op, "", 0, Profile{Slug: "claude-sonnet", Name: "Claude Sonnet", Harness: "claude-code", Model: "sonnet"})
	must(t, err)
	tpl, err := f.svc.SaveTemplate(f.ctx, op, "", 0, RoleTemplate{Slug: "implementor", Name: "Implementor", Kind: "worker", ProfileID: prof.ID, Description: "Template secret-ish text."})
	must(t, err)
	_, err = f.svc.SaveSpaceRole(f.ctx, op, "spc_a", SpaceRole{Name: "impl-a", TemplateID: tpl.ID, Description: "Role own text."})
	must(t, err)
	_, err = f.svc.SaveSpaceRole(f.ctx, op, "spc_a", SpaceRole{Name: "impl-b", TemplateID: tpl.ID})
	must(t, err)
	for _, role := range []string{"impl-a", "impl-b"} {
		r, err := f.svc.Resolve(f.ctx, f.s.R(), f.space(), Launch{Role: role})
		must(t, err)
		mj, err := json.Marshal(r.Manifest)
		must(t, err)
		for _, text := range []string{"Role own text.", "Template secret-ish text."} {
			if strings.Contains(string(mj), text) {
				t.Errorf("%s: manifest contains %q: %s", role, text, mj)
			}
		}
	}
}
