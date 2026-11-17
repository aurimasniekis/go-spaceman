package app_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/catalog"
)

// SM-PROMPT-010 over HTTP: PATCH /prompts/{id} changes name and description in place under If-Match, creates no version, and is
// refused for roles without the global prompt write permission (ADR-0095).
func TestUpdatePromptOverHTTP(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	rootSecret, _, _ := h.c.Auth.IssueRootToken(context.Background())
	root := h.client()
	root.bearer = rootSecret

	r := root.do("POST", "/prompts", map[string]any{"slug": "ctl", "name": "Control", "kind": "role", "description": "d0", "body": "hello"})
	expect(t, r, 201)
	var p catalog.Prompt
	r.json(t, &p)
	ifm := func(v int64) []string { return []string{"If-Match", fmt.Sprintf(`"%d"`, v)} }

	// Missing If-Match.
	if pr := root.do("PATCH", "/prompts/ctl", map[string]any{"name": "X"}).problem(t); pr.Status != 428 {
		t.Fatalf("missing If-Match: %+v", pr)
	}
	// Stale If-Match.
	if pr := root.do("PATCH", "/prompts/ctl", map[string]any{"name": "X"}, "If-Match", `"99"`).problem(t); pr.Status != 409 || pr.Code != "precondition_failed" {
		t.Fatalf("stale If-Match: %+v", pr)
	}
	// Validation: 422.
	for _, body := range []map[string]any{{"name": ""}, {"name": strings.Repeat("x", 121)}, {"description": strings.Repeat("d", 2001)}, {"name": "  "}} {
		if pr := root.do("PATCH", "/prompts/ctl", body, ifm(p.Version)[0], ifm(p.Version)[1]).problem(t); pr.Status != 422 {
			t.Fatalf("validation %v: %+v", body, pr)
		}
	}

	// A change by slug, then by ID; only name and description move.
	r = root.do("PATCH", "/prompts/ctl", map[string]any{"name": "Control 2", "description": "d1"}, "If-Match", ifm(p.Version)[1])
	expect(t, r, 200)
	var got catalog.Prompt
	r.json(t, &got)
	if got.Name != "Control 2" || got.Description != "d1" || got.Slug != "ctl" || got.Kind != "role" || got.Version != p.Version+1 || len(got.Versions) != len(p.Versions) {
		t.Fatalf("updated %+v", got)
	}
	r = root.do("PATCH", "/prompts/"+p.ID, map[string]any{"description": "d2"}, "If-Match", ifm(got.Version)[1])
	expect(t, r, 200)
	r.json(t, &got)
	if got.Name != "Control 2" || got.Description != "d2" {
		t.Fatalf("partial %+v", got)
	}
	expect(t, root.do("PATCH", "/prompts/nope", map[string]any{"name": "x"}, "If-Match", `"1"`), 404)

	// Authorization: a viewer and an operator are refused; an admin holds the permission.
	for _, role := range []string{"viewer", "operator"} {
		email := role + "@example.com"
		expect(t, root.do("POST", "/users", map[string]any{"email": email, "display_name": role,
			"password": "correct horse battery staple", "grants": []map[string]string{{"role": role, "domain": "*"}}}), 201)
		c := h.client()
		c.bearer = loginAndIssueToken(t, h, email, []map[string]string{{"role": role, "domain": "*"}})
		expect(t, c.do("PATCH", "/prompts/ctl", map[string]any{"name": "nope"}, "If-Match", ifm(got.Version)[1]), 403)
	}
	expect(t, root.do("POST", "/users", map[string]any{"email": "adm@example.com", "display_name": "Adm",
		"password": "correct horse battery staple", "grants": []map[string]string{{"role": "admin", "domain": "*"}}}), 201)
	adm := h.client()
	adm.bearer = loginAndIssueToken(t, h, "adm@example.com", []map[string]string{{"role": "admin", "domain": "*"}})
	expect(t, adm.do("PATCH", "/prompts/ctl", map[string]any{"name": "By admin"}, "If-Match", ifm(got.Version)[1]), 200)
}
