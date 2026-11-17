package app_test

import (
	"context"
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/fleet"
)

// ADR-0051 over HTTP: a fresh installation lists the starter roles, a space created with starter_roles gets them
// ready to launch, a space without the option gets none until it asks, and templates and profiles in use refuse
// deletion while unused ones go.
func TestStarterRolesOverHTTP(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	secret, _, _ := h.c.Auth.IssueRootToken(ctx)
	root := h.client()
	root.bearer = secret
	en, err := h.c.Fleet.CreateNode(ctx, admin, fleet.CreateNodeInput{Name: "node-a"})
	if err != nil {
		t.Fatal(err)
	}

	var starters struct {
		Items []struct {
			Name, Kind, Template string
			TemplateID           string `json:"template_id"`
		}
	}
	r := root.do("GET", "/starter-roles", nil)
	expect(t, r, 200)
	r.json(t, &starters)
	if len(starters.Items) != 6 {
		t.Fatalf("starter roles %+v", starters.Items)
	}
	for _, s := range starters.Items {
		if s.TemplateID == "" {
			t.Fatalf("starter %s has no template on a fresh installation", s.Name)
		}
	}

	type roles struct {
		Items []struct{ Name string }
	}
	var with struct{ ID string }
	r = root.do("POST", "/spaces", map[string]any{"slug": "with-roles", "name": "With roles", "node_id": en.Node.ID, "starter_roles": true})
	expect(t, r, 201)
	r.json(t, &with)
	var got roles
	r = root.do("GET", "/spaces/"+with.ID+"/roles", nil)
	expect(t, r, 200)
	r.json(t, &got)
	if len(got.Items) != 6 {
		t.Fatalf("roles of a space created with starter_roles: %+v", got.Items)
	}
	var preview struct {
		Kind, Harness string
		SystemText    string `json:"system_text"`
	}
	r = root.do("GET", "/spaces/"+with.ID+"/roles/reviewer/preview", nil)
	expect(t, r, 200)
	r.json(t, &preview)
	if preview.Kind != "worker" || preview.Harness != "claude-code" || !strings.Contains(preview.SystemText, "# Reviewer") {
		t.Fatalf("reviewer preview %+v", preview)
	}

	var without struct{ ID string }
	r = root.do("POST", "/spaces", map[string]any{"slug": "no-roles", "name": "No roles", "node_id": en.Node.ID})
	expect(t, r, 201)
	r.json(t, &without)
	r = root.do("GET", "/spaces/"+without.ID+"/roles", nil)
	expect(t, r, 200)
	got = roles{}
	r.json(t, &got)
	if len(got.Items) != 0 {
		t.Fatalf("a space created without the option has roles: %+v", got.Items)
	}
	var added struct{ Added []string }
	r = root.do("POST", "/spaces/"+without.ID+"/starter-roles", nil)
	expect(t, r, 200)
	r.json(t, &added)
	if len(added.Added) != 6 {
		t.Fatalf("added %v", added.Added)
	}

	// In use: refused, naming the users. Unused: deleted.
	if p := root.do("DELETE", "/role-templates/starter-reviewer", nil).problem(t); p.Status != 409 || !strings.Contains(p.Detail, "reviewer") {
		t.Fatalf("deleting a template in use: %+v", p)
	}
	expect(t, root.do("DELETE", "/harness-profiles/starter-codex", nil), 204)
	if p := root.do("DELETE", "/harness-profiles/starter-claude-code", nil).problem(t); p.Status != 409 {
		t.Fatalf("deleting a profile in use: %+v", p)
	}
	for _, sp := range []string{with.ID, without.ID} {
		expect(t, root.do("DELETE", "/spaces/"+sp+"/roles/reviewer", nil), 204)
	}
	expect(t, root.do("DELETE", "/role-templates/starter-reviewer", nil), 204)
	r = root.do("GET", "/starter-roles", nil)
	expect(t, r, 200)
	starters.Items = nil
	r.json(t, &starters)
	for _, s := range starters.Items {
		if s.Name == "reviewer" && s.TemplateID != "" {
			t.Fatal("a deleted starter template is still listed as present")
		}
	}
}

// SM-PROMPT-011 over HTTP: save-space-role, get-space-role and list-space-roles carry the space role's own description, and
// the preview shows the effective one with its origin; empty falls back to the template's.
func TestSpaceRoleDescriptionOverHTTP(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	secret, _, _ := h.c.Auth.IssueRootToken(ctx)
	root := h.client()
	root.bearer = secret
	en, err := h.c.Fleet.CreateNode(ctx, admin, fleet.CreateNodeInput{Name: "node-a"})
	if err != nil {
		t.Fatal(err)
	}
	var sp struct{ ID string }
	r := root.do("POST", "/spaces", map[string]any{"slug": "desc", "name": "Desc", "node_id": en.Node.ID, "starter_roles": true})
	expect(t, r, 201)
	r.json(t, &sp)
	var tpl struct {
		ID          string
		Description string
	}
	r = root.do("GET", "/role-templates/starter-reviewer", nil)
	expect(t, r, 200)
	r.json(t, &tpl)

	preview := func() (string, string) {
		var p struct {
			Effective map[string]struct{ Value, Origin string }
		}
		r := root.do("GET", "/spaces/"+sp.ID+"/roles/reviewer/preview", nil)
		expect(t, r, 200)
		r.json(t, &p)
		return p.Effective["description"].Value, p.Effective["description"].Origin
	}
	if v, o := preview(); v != tpl.Description || o != "template" {
		t.Fatalf("starter role preview description %q (%s), want the template's %q", v, o, tpl.Description)
	}

	expect(t, root.do("PUT", "/spaces/"+sp.ID+"/roles/reviewer", map[string]any{"template_id": tpl.ID, "description": "Checks the backend PRs."}), 200)
	var got struct{ Description string }
	r = root.do("GET", "/spaces/"+sp.ID+"/roles/reviewer", nil)
	expect(t, r, 200)
	r.json(t, &got)
	if got.Description != "Checks the backend PRs." {
		t.Fatalf("get description %q", got.Description)
	}
	var list struct {
		Items []struct{ Name, Description string }
	}
	r = root.do("GET", "/spaces/"+sp.ID+"/roles", nil)
	expect(t, r, 200)
	r.json(t, &list)
	for _, it := range list.Items {
		if want := map[bool]string{true: "Checks the backend PRs."}[it.Name == "reviewer"]; it.Description != want {
			t.Errorf("list: role %s description %q, want %q", it.Name, it.Description, want)
		}
	}
	if v, o := preview(); v != "Checks the backend PRs." || o != "space_role" {
		t.Fatalf("preview description %q (%s)", v, o)
	}

	expect(t, root.do("PUT", "/spaces/"+sp.ID+"/roles/reviewer", map[string]any{"template_id": tpl.ID}), 200)
	if v, o := preview(); v != tpl.Description || o != "template" {
		t.Fatalf("after clearing: preview description %q (%s)", v, o)
	}
	if p := root.do("PUT", "/spaces/"+sp.ID+"/roles/reviewer", map[string]any{"template_id": tpl.ID, "description": strings.Repeat("x", 2001)}).problem(t); p.Status != 422 {
		t.Fatalf("a 2001-character description: %+v", p)
	}
}
