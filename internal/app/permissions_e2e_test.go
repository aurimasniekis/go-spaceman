package app_test

import (
	"context"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/human"
)

// SM-HUI-008 over HTTP (ADR-0050): type permissions are managed by space admins only (If-Match, an existing one is
// refused on create) and narrow what people see and do: an operator does not see approvals restricted to admins
// (not in the space list, the global list or its counts; a direct read is not found; answering is not found), cannot
// ask with a type restricted to admins, and cannot answer a type whose responders are admins; agents of an unlisted
// role are refused when they ask. Admins keep everything.
func TestHumanTypePermissionsOverHTTP(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	rootSecret, _, _ := h.c.Auth.IssueRootToken(ctx)
	root := h.client()
	root.bearer = rootSecret
	actor := audit.Actor{Type: audit.ActorUser, ID: "usr_test"}
	en, _ := h.c.Fleet.CreateNode(ctx, actor, fleet.CreateNodeInput{Name: "node-a"})
	sp, err := h.c.Fleet.CreateSpace(ctx, actor, fleet.CreateSpaceInput{Slug: "alpha", Name: "Alpha", NodeID: en.Node.ID})
	if err != nil {
		t.Fatal(err)
	}
	base := "/spaces/" + sp.ID
	op := h.client()
	op.bearer = userToken(t, h.c.Auth, "oscar@example.com", []auth.GrantInput{{Role: authz.RoleOperator, Domain: sp.ID}})
	adm := h.client()
	adm.bearer = userToken(t, h.c.Auth, "ada@example.com", []auth.GrantInput{{Role: authz.RoleAdmin, Domain: sp.ID}})

	// Only admins change permissions; operators may read them.
	perm := map[string]any{"view_role": "admin", "respond_role": "admin"}
	expect(t, op.do("PUT", base+"/human-type-permissions/approval", perm), 403)
	r := adm.do("PUT", base+"/human-type-permissions/approval", perm)
	expect(t, r, 200)
	if r.header.Get("ETag") != `"1"` {
		t.Fatalf("etag %q", r.header.Get("ETag"))
	}
	expect(t, adm.do("PUT", base+"/human-type-permissions/approval", perm), 409) // exists: update with If-Match
	expect(t, adm.do("PUT", base+"/human-type-permissions/approval", perm, "If-Match", `"7"`), 409)
	expect(t, adm.do("PUT", base+"/human-type-permissions/todo", map[string]any{"create_role": "admin"}), 200)
	expect(t, adm.do("PUT", base+"/human-type-permissions/question", map[string]any{"respond_role": "admin", "agents": "roles",
		"agent_roles": []string{"reviewer"}}), 200)
	expect(t, adm.do("PUT", base+"/human-type-permissions/nonsense", perm), 404)
	var list struct {
		Items []human.TypePermission `json:"items"`
	}
	op.do("GET", base+"/human-type-permissions", nil).json(t, &list)
	if len(list.Items) != 3 {
		t.Fatalf("permissions %+v", list)
	}
	var got human.TypePermission
	op.do("GET", base+"/human-type-permissions/message", nil).json(t, &got)
	if got.Stored || got.ViewRole != "viewer" {
		t.Fatalf("defaults %+v", got)
	}

	// An approval and a question, asked by the root (an admin).
	var ap, q human.Action
	root.do("POST", base+"/human-actions", map[string]any{"type": "approval", "title": "Deploy?", "request": map[string]any{"action": "deploy"}}).json(t, &ap)
	root.do("POST", base+"/human-actions", map[string]any{"type": "question", "title": "Which DB?"}).json(t, &q)

	var mine struct {
		Items []human.Action `json:"items"`
	}
	op.do("GET", base+"/human-actions", nil).json(t, &mine)
	if len(mine.Items) != 1 || mine.Items[0].ID != q.ID {
		t.Fatalf("operator's space list %+v", mine)
	}
	var pending struct {
		Items  []human.Action   `json:"items"`
		Counts map[string]int64 `json:"counts"`
		Total  int64            `json:"total"`
	}
	op.do("GET", "/human-actions", nil).json(t, &pending)
	if len(pending.Items) != 1 || pending.Total != 1 || pending.Counts[sp.ID] != 1 {
		t.Fatalf("operator's global list %+v", pending)
	}
	adm.do("GET", "/human-actions", nil).json(t, &pending)
	if pending.Total != 2 {
		t.Fatalf("admin's global list %+v", pending)
	}
	expect(t, op.do("GET", base+"/human-actions/"+ap.ID, nil), 404)
	expect(t, op.do("POST", base+"/human-actions/"+ap.ID+"/respond", map[string]any{"outcome": "approved"}, "If-Match", `"1"`), 404)
	// The question is visible, but only admins answer it.
	expect(t, op.do("GET", base+"/human-actions/"+q.ID, nil), 200)
	expect(t, op.do("POST", base+"/human-actions/"+q.ID+"/respond", map[string]any{"outcome": "answered", "answer": "SQLite"}, "If-Match", `"1"`), 403)
	expect(t, op.do("POST", base+"/human-actions/"+q.ID+"/comments", map[string]any{"body": "hm"}), 403)
	expect(t, adm.do("POST", base+"/human-actions/"+q.ID+"/respond", map[string]any{"outcome": "answered", "answer": "SQLite"}, "If-Match", `"1"`), 200)
	// Only admins ask with todos.
	expect(t, op.do("POST", base+"/human-actions", map[string]any{"type": "todo", "title": "Rotate keys"}), 403)
	expect(t, adm.do("POST", base+"/human-actions", map[string]any{"type": "todo", "title": "Rotate keys"}), 201)
	// Agents of a role the question permission does not list are refused.
	_, _, err = h.c.Human.Create(ctx, audit.System, sp.ID, human.CreateInput{Type: human.TypeQuestion, Title: "Agent asks",
		Requester: human.Requester{Kind: "agent", ID: "ins_1", Role: "implementor", Label: "implementor"}})
	if apperr.CodeOf(err) != apperr.CodeForbidden {
		t.Fatalf("agent of an unlisted role: %v", err)
	}
	// Removing the permission restores the defaults.
	expect(t, op.do("DELETE", base+"/human-type-permissions/approval", nil), 403)
	expect(t, adm.do("DELETE", base+"/human-type-permissions/approval", nil), 204)
	expect(t, op.do("GET", base+"/human-actions/"+ap.ID, nil), 200)
}

// SM-API-004 (ADR-0050): a global administrator updates a user's email, display name and MFA requirement over HTTP:
// If-Match is required and checked, the change is audited, a space admin is refused, and the effective MFA
// requirement stays forced by policy when the setting is cleared.
func TestUserProfileOverHTTP(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	rootSecret, _, _ := h.c.Auth.IssueRootToken(ctx)
	root := h.client()
	root.bearer = rootSecret
	u, err := h.c.Auth.Create(ctx, audit.System, auth.CreateInput{Email: "dev@example.com", DisplayName: "Dev", Grants: []auth.GrantInput{}})
	if err != nil {
		t.Fatal(err)
	}
	en, _ := h.c.Fleet.CreateNode(ctx, audit.System, fleet.CreateNodeInput{Name: "node-a"})
	sp, _ := h.c.Fleet.CreateSpace(ctx, audit.System, fleet.CreateSpaceInput{Slug: "alpha", Name: "Alpha", NodeID: en.Node.ID})
	spaceAdmin := h.client()
	spaceAdmin.bearer = userToken(t, h.c.Auth, "ada@example.com", []auth.GrantInput{{Role: authz.RoleAdmin, Domain: sp.ID}})
	path := "/users/" + u.ID + "/profile"
	body := map[string]any{"email": "Dev2@Example.com", "display_name": "Developer", "mfa_required": true}
	expect(t, root.do("PUT", path, body), 428)
	expect(t, root.do("PUT", path, body, "If-Match", `"9"`), 409)
	expect(t, spaceAdmin.do("PUT", path, body, "If-Match", `"1"`), 403)
	r := root.do("PUT", path, body, "If-Match", `"1"`)
	expect(t, r, 200)
	var got auth.User
	r.json(t, &got)
	if got.Email != "dev2@example.com" || got.DisplayName != "Developer" || !got.MFARequiredSet || !got.MFARequired || r.header.Get("ETag") != `"2"` {
		t.Fatalf("updated %+v %s", got, r.header.Get("ETag"))
	}
	expect(t, root.do("PUT", "/users/"+u.ID+"/profile", map[string]any{"email": "", "display_name": "Developer", "mfa_required": false}, "If-Match", `"2"`), 200)
	var page struct {
		Items []struct {
			Action  string         `json:"action"`
			Details map[string]any `json:"details"`
		} `json:"items"`
	}
	root.do("GET", "/audit?action=user.profile_updated&target_id="+u.ID, nil).json(t, &page)
	if len(page.Items) != 2 || page.Items[1].Details["email"] != "dev2@example.com" {
		t.Fatalf("audit %+v", page)
	}
}
