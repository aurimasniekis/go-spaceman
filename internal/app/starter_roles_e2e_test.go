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
