package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/approvals"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/catalog"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/gateway"
	"github.com/aurimasniekis/go-spaceman/internal/human"
)

// A SpaceMan MCP server (ADR-0063, AM-5) end to end through the gateway: created with its internal token in one step,
// granted to a space, offered only to control and worker runs, gated by approvals (dangerous tools always, diagnostic
// launches as dangerous, writes when the server gates them), refused on its own management, re-issued and deleted with
// its token revoked, and refused with a reason once its token is revoked.
func TestSpacemanMCPGateway(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	sys := audit.Actor{Type: audit.ActorSystem, ID: "test"}
	en, err := h.c.Fleet.CreateNode(ctx, audit.Actor{Type: audit.ActorUser, ID: "usr_test"}, fleet.CreateNodeInput{Name: "node-a"})
	if err != nil {
		t.Fatal(err)
	}
	sp, err := h.c.Fleet.CreateSpace(ctx, sys, fleet.CreateSpaceInput{Slug: "infra", Name: "Infra", NodeID: en.Node.ID})
	if err != nil {
		t.Fatal(err)
	}
	ada, err := h.c.Auth.Create(ctx, sys, auth.CreateInput{Email: "ada@example.com", DisplayName: "Ada",
		Grants: []auth.GrantInput{{Role: authz.RoleAdmin, Domain: authz.Global}}})
	if err != nil {
		t.Fatal(err)
	}
	adaP := &auth.Principal{User: ada, Method: auth.MethodPassword}
	adaA := audit.Actor{Type: audit.ActorUser, ID: ada.ID}
	gw := h.c.Gateway

	// Defaults: preset admin, no write approvals, admin on every space, never expiring.
	srv, err := gw.CreateSpaceman(ctx, adaP, adaA, gateway.SpacemanInput{Slug: "sm", Name: "SpaceMan"})
	if err != nil {
		t.Fatal(err)
	}
	info := srv.Spaceman
	if srv.Kind != gateway.KindSpaceman || srv.URL != "" || info == nil || info.Preset != "admin" || info.Approval != gateway.ApprovalNone ||
		!info.NeverExpires || info.OwnerUserID != ada.ID || info.TokenState != "active" || len(info.Grants) != 1 || info.Grants[0].Domain != authz.Global {
		t.Fatalf("created %+v / %+v", srv, info)
	}
	tok, err := h.c.Auth.GetAPIToken(ctx, info.TokenID)
	if err != nil || tok.Purpose != auth.TokenPurposeSpacemanMCP || tok.UserID != ada.ID {
		t.Fatalf("token %+v %v", tok, err)
	}
	if _, err := gw.AddGrant(ctx, adaA, srv.ID, sp.ID, "*", nil); err != nil {
		t.Fatal(err)
	}
	// The generic edit refuses a spaceman server; the slug "spaceman" stays reserved.
	if _, err := gw.Update(ctx, adaA, srv.ID, srv.Version, gateway.Input{Name: "x", URL: "https://x.example.com", Enabled: true}); !errors.Is(err, apperr.ErrInvalid) {
		t.Fatalf("update-mcp-server on a spaceman server: %v", err)
	}
	if _, err := gw.CreateSpaceman(ctx, adaP, adaA, gateway.SpacemanInput{Slug: "spaceman", Name: "S"}); !errors.Is(err, apperr.ErrInvalid) {
		t.Fatalf("slug spaceman: %v", err)
	}
	// Read and operate presets need grants; tools must exist and never be excluded.
	for _, in := range []gateway.SpacemanInput{
		{Slug: "ro", Name: "R", SpacemanSettings: gateway.SpacemanSettings{Preset: "read"}},
		{Slug: "bad", Name: "B", SpacemanSettings: gateway.SpacemanSettings{Tools: []string{"respond_human_action"}}},
		{Slug: "bad2", Name: "B", SpacemanSettings: gateway.SpacemanSettings{Approval: "always"}},
	} {
		if _, err := gw.CreateSpaceman(ctx, adaP, adaA, in); !errors.Is(err, apperr.ErrInvalid) {
			t.Errorf("%s: %v", in.Slug, err)
		}
	}

	run := func(kind string) *fleet.RunPrincipal {
		return &fleet.RunPrincipal{RunID: "run_" + kind, SpaceID: sp.ID, Kind: kind, Role: kind, InstanceID: "ins_" + kind}
	}
	names := func(p *fleet.RunPrincipal) []string {
		var out []string
		for _, tl := range gw.Catalog(ctx, p) {
			if strings.HasPrefix(tl.Name, "sm__") {
				out = append(out, tl.Name)
			}
		}
		return out
	}
	for _, kind := range []string{"control", "worker"} {
		n := names(run(kind))
		if !slices.Contains(n, "sm__list_spaces") || !slices.Contains(n, "sm__create_task") || !slices.Contains(n, "sm__delete_schedule") ||
			slices.Contains(n, "sm__respond_human_action") || slices.Contains(n, "sm__create_spaceman_mcp") || len(n) < 150 {
			t.Errorf("%s run: %d tools", kind, len(n))
		}
	}
	for _, kind := range []string{"kb", "dreamer", "diagnostic"} {
		if n := names(run(kind)); len(n) != 0 {
			t.Errorf("%s run sees %d SpaceMan MCP tools", kind, len(n))
		}
		if _, err := gw.Call(ctx, run(kind), "sm__list_spaces", json.RawMessage(`{}`)); !errors.Is(err, apperr.ErrForbidden) {
			t.Errorf("%s run called a SpaceMan MCP tool: %v", kind, err)
		}
	}
	m, err := gw.ForManifest(ctx, sp.ID, "control", "control")
	if err != nil || len(m) != 1 || m[0].Kind != gateway.KindSpaceman || m[0].Slug != "sm" {
		t.Fatalf("manifest %+v %v", m, err)
	}
	if m, _ := gw.ForManifest(ctx, sp.ID, "kb", "kb"); len(m) != 0 {
		t.Fatalf("kb manifest %+v", m)
	}

	ctrl := run("control")
	result := func(raw json.RawMessage) (*mcp.CallToolResult, map[string]any) {
		t.Helper()
		var r mcp.CallToolResult
		if err := json.Unmarshal(raw, &r); err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		_ = json.Unmarshal([]byte(r.Content[0].(*mcp.TextContent).Text), &body)
		return &r, body
	}
	raw, err := gw.Call(ctx, ctrl, "sm__list_spaces", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if r, body := result(raw); r.IsError || body["status"] != float64(200) || !strings.Contains(string(raw), sp.ID) {
		t.Fatalf("list_spaces: %s", raw)
	}
	// Writes run without an approval by default.
	raw, err = gw.Call(ctx, ctrl, "sm__create_task", json.RawMessage(`{"space_id":"`+sp.ID+`","body":{"title":"Rotate keys"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if r, body := result(raw); r.IsError || body["status"] != float64(201) {
		t.Fatalf("create_task: %s", raw)
	}

	// Dangerous tools always need an approval, with no policy at all; so does a diagnostic launch.
	approvalOps := func() []string {
		l, err := h.c.Approvals.List(ctx, sp.ID, 50)
		if err != nil {
			t.Fatal(err)
		}
		var ops []string
		for _, a := range l {
			ops = append(ops, a.Operation)
		}
		return ops
	}
	if _, err := gw.Call(ctx, ctrl, "sm__delete_schedule", json.RawMessage(`{"space_id":"`+sp.ID+`","schedule_id":"sch_x"}`)); !errors.Is(err, approvals.ErrRequired) {
		t.Fatalf("dangerous tool without an approval: %v", err)
	}
	if _, err := gw.Call(ctx, ctrl, "sm__launch_run", json.RawMessage(`{"space_id":"`+sp.ID+`","body":{"kind":"diagnostic","role":"control"}}`)); !errors.Is(err, approvals.ErrRequired) {
		t.Fatalf("diagnostic launch without an approval: %v", err)
	}
	if ops := approvalOps(); !slices.Contains(ops, "mcp.sm.dangerous.delete_schedule") || !slices.Contains(ops, "mcp.sm.dangerous.launch_run") || len(ops) != 2 {
		t.Fatalf("approvals: %v", ops)
	}
	// Once a person approves, the same call runs once.
	l, _ := h.c.Approvals.List(ctx, sp.ID, 50)
	var del *approvals.Approval
	for _, a := range l {
		if a.Operation == "mcp.sm.dangerous.delete_schedule" {
			del = a
		}
	}
	act, err := h.c.Human.Get(ctx, sp.ID, del.HumanActionID)
	if err != nil {
		t.Fatal(err)
	}
	// An agent acting through a SpaceMan MCP server never answers a human action or decides an approval.
	internal := audit.Actor{Type: audit.ActorUser, ID: ada.ID, Via: auth.ViaSpacemanMCP + info.TokenID, Run: ctrl.RunID}
	if _, err := h.c.Human.Respond(ctx, internal, sp.ID, act.ID, act.Version, human.RespondInput{Outcome: "approved"}); !errors.Is(err, apperr.ErrForbidden) {
		t.Fatalf("internal actor answered an approval: %v", err)
	}
	if _, err := h.c.Human.Respond(ctx, adaA, sp.ID, act.ID, act.Version, human.RespondInput{Outcome: "approved"}); err != nil {
		t.Fatal(err)
	}
	raw, err = gw.Call(ctx, ctrl, "sm__delete_schedule", json.RawMessage(`{"space_id":"`+sp.ID+`","schedule_id":"sch_x"}`))
	if err != nil {
		t.Fatalf("approved dangerous call: %v", err)
	}
	if r, body := result(raw); !r.IsError || body["status"] != float64(404) {
		t.Fatalf("approved delete of a missing schedule: %s", raw)
	}

	// A diagnostic launch whose kind comes from the space role (no kind argument) is classed write by the gateway, but
	// the API checks the resolved kind and refuses it without an approved dangerous call. With kind "diagnostic" it is
	// gated, and once approved it passes the kind check (it then fails only because no node is connected).
	tpl, err := h.c.Catalog.SaveTemplate(ctx, adaA, "", 0, catalog.RoleTemplate{Slug: "doctor", Name: "Doctor", Kind: "diagnostic"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.c.Catalog.SaveSpaceRole(ctx, adaA, sp.ID, catalog.SpaceRole{Name: "diag", TemplateID: tpl.ID}); err != nil {
		t.Fatal(err)
	}
	roleOnly := json.RawMessage(`{"space_id":"` + sp.ID + `","body":{"role":"diag","harness":"claude-code"}}`)
	raw, err = gw.Call(ctx, ctrl, "sm__launch_run", roleOnly)
	if err != nil {
		t.Fatalf("role-only diagnostic launch: %v", err)
	}
	if r, body := result(raw); !r.IsError || body["status"] != float64(403) || !strings.Contains(string(raw), "approval") {
		t.Fatalf("role-only diagnostic launch ran without an approval: %s", raw)
	}
	explicit := json.RawMessage(`{"space_id":"` + sp.ID + `","body":{"kind":"diagnostic","role":"diag","harness":"claude-code"}}`)
	if _, err := gw.Call(ctx, ctrl, "sm__launch_run", explicit); !errors.Is(err, approvals.ErrRequired) {
		t.Fatalf("explicit diagnostic launch without an approval: %v", err)
	}
	l, _ = h.c.Approvals.List(ctx, sp.ID, 50)
	for _, a := range l {
		if a.Operation != "mcp.sm.dangerous.launch_run" || a.State != approvals.Pending {
			continue
		}
		if act, err = h.c.Human.Get(ctx, sp.ID, a.HumanActionID); err != nil {
			t.Fatal(err)
		}
		if _, err := h.c.Human.Respond(ctx, adaA, sp.ID, act.ID, act.Version, human.RespondInput{Outcome: "approved"}); err != nil {
			t.Fatal(err)
		}
	}
	raw, err = gw.Call(ctx, ctrl, "sm__launch_run", explicit)
	if err != nil {
		t.Fatalf("approved diagnostic launch: %v", err)
	}
	if _, body := result(raw); body["status"] == float64(403) {
		t.Fatalf("approved diagnostic launch refused by the kind check: %s", raw)
	}
	// Saving a space role is dangerous: it can point a role at a diagnostic template or widen its runs.
	if _, err := gw.Call(ctx, ctrl, "sm__save_space_role", json.RawMessage(`{"space_id":"`+sp.ID+`","name":"diag2","body":{"template_id":"`+tpl.ID+`"}}`)); !errors.Is(err, approvals.ErrRequired) {
		t.Fatalf("save_space_role without an approval: %v", err)
	}

	// approval: writes gates write tools too.
	srv, err = gw.UpdateSpaceman(ctx, adaA, srv.ID, srv.Version, gateway.SpacemanUpdate{Name: "SpaceMan", Enabled: true,
		SpacemanSettings: gateway.SpacemanSettings{Approval: gateway.ApprovalWrites}})
	if err != nil || srv.Spaceman.Approval != gateway.ApprovalWrites || srv.Spaceman.Preset != "admin" || srv.Spaceman.TokenID != info.TokenID {
		t.Fatalf("update %+v %v", srv, err)
	}
	if _, err := gw.Call(ctx, ctrl, "sm__create_task", json.RawMessage(`{"space_id":"`+sp.ID+`","body":{"title":"Again"}}`)); !errors.Is(err, approvals.ErrRequired) {
		t.Fatalf("write with approval writes: %v", err)
	}
	if ops := approvalOps(); !slices.Contains(ops, "mcp.sm.write.create_task") {
		t.Fatalf("approvals: %v", ops)
	}
	if _, err := gw.Call(ctx, ctrl, "sm__list_spaces", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("read with approval writes: %v", err)
	}

	// Self-protection: an agent acting through the server never grants, revokes, checks, edits or deletes a spaceman
	// server, nor creates or re-issues one.
	if _, err := gw.AddGrant(ctx, internal, srv.ID, sp.ID, "worker", nil); !errors.Is(err, apperr.ErrForbidden) {
		t.Errorf("internal grant: %v", err)
	}
	if err := gw.Delete(ctx, internal, srv.ID); !errors.Is(err, apperr.ErrForbidden) {
		t.Errorf("internal delete: %v", err)
	}
	if _, err := gw.Check(ctx, internal, srv.ID); !errors.Is(err, apperr.ErrForbidden) {
		t.Errorf("internal check: %v", err)
	}
	if _, err := gw.UpdateSpaceman(ctx, internal, srv.ID, srv.Version, gateway.SpacemanUpdate{Name: "x", Enabled: true}); !errors.Is(err, apperr.ErrForbidden) {
		t.Errorf("internal update: %v", err)
	}
	ip, err := h.c.Auth.InternalPrincipal(ctx, info.TokenID, ctrl.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gw.ReissueSpaceman(ctx, ip, internal, srv.ID, srv.Version, gateway.SpacemanToken{}); !errors.Is(err, apperr.ErrForbidden) {
		t.Errorf("internal reissue: %v", err)
	}
	if _, err := gw.CreateSpaceman(ctx, ip, internal, gateway.SpacemanInput{Slug: "again", Name: "A"}); !errors.Is(err, apperr.ErrForbidden) {
		t.Errorf("internal create: %v", err)
	}
	if d, err := gw.Check(ctx, adaA, srv.ID); err != nil || !d.OK || len(d.Tools) < 150 {
		t.Errorf("check: %+v %v", d, err)
	}

	// Re-issue: a new token with the new grants, the old one revoked in the same step.
	srv, err = gw.ReissueSpaceman(ctx, adaP, adaA, srv.ID, srv.Version, gateway.SpacemanToken{
		Grants: []auth.GrantInput{{Role: authz.RoleViewer, Domain: sp.ID}}, ExpiresInDays: 30})
	if err != nil {
		t.Fatal(err)
	}
	if srv.Spaceman.TokenID == info.TokenID || srv.Spaceman.NeverExpires || srv.Spaceman.ExpiresAt == nil || len(srv.Spaceman.Grants) != 1 {
		t.Fatalf("reissued %+v", srv.Spaceman)
	}
	if old, _ := h.c.Auth.GetAPIToken(ctx, info.TokenID); old.RevokedAt == nil {
		t.Fatal("re-issue kept the old token")
	}
	n := names(ctrl)
	if !slices.Contains(n, "sm__list_spaces") || !slices.Contains(n, "sm__get_space") || slices.Contains(n, "sm__create_task") || slices.Contains(n, "sm__list_nodes") {
		t.Fatalf("viewer token's catalog: %v", n)
	}
	// A missing space_id is the token's one space.
	raw, err = gw.Call(ctx, ctrl, "sm__get_space", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := result(raw); r.IsError {
		t.Fatalf("get_space with the default space: %s", raw)
	}

	// Revoking the token (on the Tokens page) empties the catalog, and calls are refused with the reason.
	if _, err := h.c.Auth.RevokeAPIToken(ctx, adaA, srv.Spaceman.TokenID); err != nil {
		t.Fatal(err)
	}
	if n := names(ctrl); len(n) != 0 {
		t.Fatalf("revoked token's catalog: %v", n)
	}
	_, err = gw.Call(ctx, ctrl, "sm__list_spaces", json.RawMessage(`{}`))
	if !errors.Is(err, apperr.ErrForbidden) || !strings.Contains(err.Error(), "revoked") || !strings.Contains(err.Error(), "re-issue") {
		t.Fatalf("call with a revoked token: %v", err)
	}
	if s, _ := gw.Get(ctx, srv.ID); s.Spaceman.TokenState != "revoked" {
		t.Fatalf("token state %q", s.Spaceman.TokenState)
	}
	if d, err := gw.Check(ctx, adaA, srv.ID); err != nil || d.OK || d.Error == "" {
		t.Fatalf("check with a revoked token: %+v %v", d, err)
	}

	// Delete revokes the current token in the same step.
	srv, err = gw.ReissueSpaceman(ctx, adaP, adaA, srv.ID, srv.Version, gateway.SpacemanToken{})
	if err != nil {
		t.Fatal(err)
	}
	if err := gw.Delete(ctx, adaA, srv.ID); err != nil {
		t.Fatal(err)
	}
	if tk, _ := h.c.Auth.GetAPIToken(ctx, srv.Spaceman.TokenID); tk.RevokedAt == nil {
		t.Fatal("delete kept the token")
	}
}
