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

// SM-MCP-010 (ADR-0095): a skill's name and description change in place under its version (If-Match). No skill version is
// created, the files stay as they are, and the audit row names the fields and never their text.
func TestUpdateSkillMeta(t *testing.T) {
	f := newFx(t)
	sk, err := f.svc.CreateSkill(f.ctx, op, "review", "Review", "old text", map[string]string{"SKILL.md": reviewMD})
	must(t, err)
	sk, err = f.svc.PublishSkill(f.ctx, op, sk.ID, sk.Versions[0].Version)
	must(t, err)
	_, err = f.svc.SaveSkillDraft(f.ctx, op, sk.ID, 0, map[string]string{"SKILL.md": reviewMD + "\nMore.\n"}, "draft")
	must(t, err)
	sk, _ = f.svc.GetSkill(f.ctx, sk.ID)
	if sk.Version != 1 {
		t.Fatalf("a new skill is version %d, want 1", sk.Version)
	}
	countVersions := func() (n int, maxVer int64) {
		must(t, f.s.ReadDB().QueryRowContext(f.ctx, `SELECT count(*), coalesce(max(version),0) FROM skill_versions WHERE skill_id = ?`, sk.ID).Scan(&n, &maxVer))
		return
	}
	nBefore, vBefore := countVersions()
	filesBefore := sk.Versions[0].Files["SKILL.md"]

	name, desc := "Review v2", "Reviews pull requests."
	got, err := f.svc.UpdateSkillMeta(f.ctx, op, "review", sk.Version, &name, &desc) // by slug
	must(t, err)
	if got.Name != name || got.Description != desc || got.Slug != "review" || got.Version != sk.Version+1 || got.Latest != 1 {
		t.Fatalf("updated %+v", got)
	}
	if n, v := countVersions(); n != nBefore || v != vBefore {
		t.Fatalf("skill_versions changed: %d/%d -> %d/%d", nBefore, vBefore, n, v)
	}
	if got.Versions[0].Files["SKILL.md"] != filesBefore || len(got.Versions) != len(sk.Versions) {
		t.Fatalf("files or versions changed: %+v", got.Versions)
	}

	// A field left out keeps its value; by ID.
	d2 := "Only the description."
	got, err = f.svc.UpdateSkillMeta(f.ctx, op, sk.ID, got.Version, nil, &d2)
	must(t, err)
	if got.Name != name || got.Description != d2 || got.Version != sk.Version+2 {
		t.Fatalf("partial %+v", got)
	}

	// Clearing the description is allowed.
	empty := ""
	got, err = f.svc.UpdateSkillMeta(f.ctx, op, sk.ID, got.Version, nil, &empty)
	must(t, err)
	if got.Description != "" {
		t.Fatalf("cleared description %q", got.Description)
	}

	// A stale version is refused and changes nothing, also when the values match (nothing to write).
	other := "Stale"
	_, err = f.svc.UpdateSkillMeta(f.ctx, op, sk.ID, sk.Version, &other, nil)
	code(t, err, apperr.CodePrecondition)
	_, err = f.svc.UpdateSkillMeta(f.ctx, op, sk.ID, sk.Version, &name, nil)
	code(t, err, apperr.CodePrecondition)
	if cur, _ := f.svc.GetSkill(f.ctx, sk.ID); cur.Name != name {
		t.Fatalf("stale update applied: %+v", cur)
	}
	// Unknown skill.
	_, err = f.svc.UpdateSkillMeta(f.ctx, op, "nope", 1, &other, nil)
	code(t, err, apperr.CodeNotFound)

	// Validation as for create (runes, not bytes): 120 multi-byte characters are fine, 121 are not.
	empty2, long, longDesc := "  ", strings.Repeat("x", 121), strings.Repeat("d", 2001)
	for _, bad := range [][2]*string{{&empty2, nil}, {&long, nil}, {nil, &longDesc}} {
		_, err = f.svc.UpdateSkillMeta(f.ctx, op, sk.ID, got.Version, bad[0], bad[1])
		code(t, err, apperr.CodeInvalid)
	}
	ok120 := strings.Repeat("ž", 120)
	if got, err = f.svc.UpdateSkillMeta(f.ctx, op, sk.ID, got.Version, &ok120, nil); err != nil || got.Name != ok120 {
		t.Fatalf("120 runes refused: %v", err)
	}

	// Audit: field names only, never their text.
	var details string
	must(t, f.s.ReadDB().QueryRowContext(f.ctx, `SELECT details_json FROM audit_events WHERE action = 'skill.updated' ORDER BY seq ASC LIMIT 1`).Scan(&details))
	if !strings.Contains(details, `"name"`) || !strings.Contains(details, `"description"`) || strings.Contains(details, "Review v2") || strings.Contains(details, "Reviews pull") {
		t.Fatalf("audit details %s", details)
	}
	var n int
	must(t, f.s.ReadDB().QueryRowContext(f.ctx, `SELECT count(*) FROM audit_events WHERE action = 'skill.updated'`).Scan(&n))
	if n != 4 {
		t.Fatalf("%d skill.updated rows, want 4", n)
	}
}
