package catalog

import (
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
)

const reviewMD = "---\nname: review\ndescription: Use when reviewing pull requests.\n---\n\n# Review\n\nCheck tests first.\n"

// SM-MCP-004: central, versioned skills; the Hallpass skill is seeded; templates and space roles assign skills, which
// resolve into launches (versions and hashes in the manifest).
func TestSkills(t *testing.T) {
	f := newFx(t)
	for name, files := range map[string]map[string]string{
		"no SKILL.md":     {"README.md": "x"},
		"no frontmatter":  {"SKILL.md": "# Review"},
		"name mismatch":   {"SKILL.md": strings.Replace(reviewMD, "name: review", "name: other", 1)},
		"escaping path":   {"SKILL.md": reviewMD, "../x": "y"},
		"absolute path":   {"SKILL.md": reviewMD, "/etc/x": "y"},
		"binary content":  {"SKILL.md": reviewMD, "bin": "\x00\x01"},
		"hidden traverse": {"SKILL.md": reviewMD, "a/../../x": "y"},
	} {
		_, err := f.svc.CreateSkill(f.ctx, op, "review", "Review", "", files)
		if apperr.CodeOf(err) != apperr.CodeInvalid {
			t.Fatalf("%s: %v", name, err)
		}
	}
	sk, err := f.svc.CreateSkill(f.ctx, op, "review", "Review", "", map[string]string{"SKILL.md": reviewMD, "references/checklist.md": "- tests"})
	must(t, err)
	sk, err = f.svc.PublishSkill(f.ctx, op, sk.ID, sk.Versions[0].Version)
	must(t, err)
	v1 := sk.Versions[0]
	_, err = f.svc.SaveSkillDraft(f.ctx, op, sk.ID, 0, map[string]string{"SKILL.md": reviewMD + "\nAlso check docs.\n"}, "docs")
	must(t, err)
	sk, _ = f.svc.GetSkill(f.ctx, sk.ID)
	var draft *SkillVersion
	for _, v := range sk.Versions {
		if v.State == "draft" {
			draft = v
		}
	}
	sk, err = f.svc.PublishSkill(f.ctx, op, sk.ID, draft.Version)
	must(t, err)
	if sk.Latest != 2 {
		t.Fatalf("latest %d", sk.Latest)
	}
	must(t, f.svc.SeedBuiltins(f.ctx))
	must(t, f.svc.SeedBuiltins(f.ctx)) // idempotent
	hp, err := f.svc.GetSkill(f.ctx, "hallpass")
	must(t, err)
	if hp.Source != "builtin" || hp.Latest != 1 || !strings.Contains(hp.Versions[0].Files["SKILL.md"], "HallPass") || hp.Versions[0].Files["references/commands.md"] == "" {
		t.Fatalf("hallpass seed %+v", hp)
	}
	l, _ := f.svc.ListSkills(f.ctx)
	if len(l) != 2 {
		t.Fatalf("skills %d", len(l))
	}
	// A template gets hallpass (following); a space role adds review pinned to v1.
	tpl, err := f.svc.SaveTemplate(f.ctx, op, "", 0, RoleTemplate{Slug: "ops", Name: "Ops", Kind: "worker", Skills: []SkillRef{{SkillID: hp.ID}}})
	must(t, err)
	_, err = f.svc.SaveTemplate(f.ctx, op, "", 0, RoleTemplate{Slug: "bad", Name: "Bad", Kind: "worker", Skills: []SkillRef{{SkillID: "skl_missing"}}})
	code(t, err, apperr.CodeInvalid)
	_, err = f.svc.SaveSpaceRole(f.ctx, op, "spc_a", SpaceRole{Name: "ops", TemplateID: tpl.ID, Skills: []SkillRef{{SkillID: sk.ID, Pin: hp.Versions[0].ID}}})
	code(t, err, apperr.CodeInvalid) // a pin must be a version of that skill
	_, err = f.svc.SaveSpaceRole(f.ctx, op, "spc_a", SpaceRole{Name: "ops", TemplateID: tpl.ID, Skills: []SkillRef{{SkillID: sk.ID, Pin: v1.ID}}})
	must(t, err)
	r, err := f.svc.Resolve(f.ctx, f.s.R(), f.space(), Launch{Role: "ops", Harness: "claude-code"})
	must(t, err)
	if len(r.Skills) != 2 || r.Skills[0].Slug != "hallpass" || r.Skills[0].Origin != OriginTemplate || r.Skills[1].Slug != "review" || r.Skills[1].Number != 1 ||
		!r.Skills[1].Pinned || r.Skills[1].Files["references/checklist.md"] != "- tests" {
		t.Fatalf("skills %+v", r.Skills)
	}
	if r.Manifest["skills"] == nil {
		t.Fatal("skills missing from the manifest")
	}
}
