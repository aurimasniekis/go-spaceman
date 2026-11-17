package app_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/catalog"
)

// SM-MCP-010 over HTTP: PATCH /skills/{id} changes name and description in place under If-Match, creates no skill version,
// and is refused for roles without the global skill write permission (ADR-0095).
func TestUpdateSkillOverHTTP(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	rootSecret, _, _ := h.c.Auth.IssueRootToken(context.Background())
	root := h.client()
	root.bearer = rootSecret

	const skillMD = "---\nname: review\ndescription: Use when reviewing pull requests.\n---\n\n# Review\n"
	r := root.do("POST", "/skills", map[string]any{"slug": "review", "name": "Review", "description": "d0", "files": map[string]string{"SKILL.md": skillMD}})
	expect(t, r, 201)
	var sk catalog.Skill
	r.json(t, &sk)
	ifm := func(v int64) []string { return []string{"If-Match", fmt.Sprintf(`"%d"`, v)} }

	// Missing If-Match.
	if pr := root.do("PATCH", "/skills/review", map[string]any{"name": "X"}).problem(t); pr.Status != 428 {
		t.Fatalf("missing If-Match: %+v", pr)
	}
	// Stale If-Match.
	if pr := root.do("PATCH", "/skills/review", map[string]any{"name": "X"}, "If-Match", `"99"`).problem(t); pr.Status != 409 || pr.Code != "precondition_failed" {
		t.Fatalf("stale If-Match: %+v", pr)
	}
	// Validation: 422.
	for _, body := range []map[string]any{{"name": ""}, {"name": strings.Repeat("x", 121)}, {"description": strings.Repeat("d", 2001)}, {"name": "  "}} {
		if pr := root.do("PATCH", "/skills/review", body, ifm(sk.Version)[0], ifm(sk.Version)[1]).problem(t); pr.Status != 422 {
			t.Fatalf("validation %v: %+v", body, pr)
		}
	}

	// A change by slug; only name and description move, no version is created.
	r = root.do("PATCH", "/skills/review", map[string]any{"name": "Review 2", "description": "d1"}, ifm(sk.Version)[0], ifm(sk.Version)[1])
	expect(t, r, 200)
	var got catalog.Skill
	r.json(t, &got)
	if got.Name != "Review 2" || got.Description != "d1" || got.Slug != "review" || got.Version != sk.Version+1 || len(got.Versions) != len(sk.Versions) {
		t.Fatalf("updated %+v", got)
	}
	// A change by ID, the description only; a stale If-Match after it is refused.
	r = root.do("PATCH", "/skills/"+sk.ID, map[string]any{"description": "d2"}, ifm(got.Version)[0], ifm(got.Version)[1])
	expect(t, r, 200)
	r.json(t, &got)
	if got.Name != "Review 2" || got.Description != "d2" || got.Version != sk.Version+2 || len(got.Versions) != len(sk.Versions) {
		t.Fatalf("partial %+v", got)
	}
	// A name only: the description keeps its value (a missing field is not an empty one).
	r = root.do("PATCH", "/skills/review", map[string]any{"name": "Review 3"}, ifm(got.Version)[0], ifm(got.Version)[1])
	expect(t, r, 200)
	got = catalog.Skill{} // decode into a fresh value: a stale description must not survive the decode
	r.json(t, &got)
	if got.Name != "Review 3" || got.Description != "d2" || got.Version != sk.Version+3 {
		t.Fatalf("name only %+v", got)
	}
	if pr := root.do("PATCH", "/skills/review", map[string]any{"name": "Stale"}, ifm(sk.Version + 1)[0], ifm(sk.Version + 1)[1]).problem(t); pr.Status != 409 {
		t.Fatalf("stale after update: %+v", pr)
	}
	expect(t, root.do("PATCH", "/skills/nope", map[string]any{"name": "x"}, "If-Match", `"1"`), 404)

	// Authorization: a viewer and an operator are refused; an admin holds the permission.
	for _, role := range []string{"viewer", "operator"} {
		email := role + "@example.com"
		expect(t, root.do("POST", "/users", map[string]any{"email": email, "display_name": role,
			"password": "correct horse battery staple", "grants": []map[string]string{{"role": role, "domain": "*"}}}), 201)
		c := h.client()
		c.bearer = loginAndIssueToken(t, h, email, []map[string]string{{"role": role, "domain": "*"}})
		expect(t, c.do("PATCH", "/skills/review", map[string]any{"name": "nope"}, ifm(got.Version)[0], ifm(got.Version)[1]), 403)
	}
	expect(t, root.do("POST", "/users", map[string]any{"email": "adm@example.com", "display_name": "Adm",
		"password": "correct horse battery staple", "grants": []map[string]string{{"role": "admin", "domain": "*"}}}), 201)
	adm := h.client()
	adm.bearer = loginAndIssueToken(t, h, "adm@example.com", []map[string]string{{"role": "admin", "domain": "*"}})
	expect(t, adm.do("PATCH", "/skills/review", map[string]any{"name": "By admin"}, ifm(got.Version)[0], ifm(got.Version)[1]), 200)
}
