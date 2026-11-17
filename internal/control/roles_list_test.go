package control

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/agenttools"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/catalog"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// SM-CTRL-007 (ADR-0095): spaceman_roles_list is for control runs only, lists its own space's roles by name with the
// effective description, harness and model, and never returns prompt text, settings, skills or account references.
func TestRolesList(t *testing.T) {
	e := newPeopleEnv(t)
	cat := catalog.New(e.s, e.aw, e.ch, catalog.Options{})
	op := audit.Actor{Type: audit.ActorUser, ID: "usr_op"}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	prof, err := cat.SaveProfile(e.ctx, op, "", 0, catalog.Profile{Slug: "claude-sonnet", Name: "Claude Sonnet", Harness: "claude-code", Model: "sonnet"})
	must(err)
	tpl, err := cat.SaveTemplate(e.ctx, op, "", 0, catalog.RoleTemplate{Slug: "implementor", Name: "Implementor", Kind: "worker", ProfileID: prof.ID,
		Description: "Template text.", MemoryConventions: "SECRET-MEMORY-CONVENTIONS", Settings: map[string]string{"tpl_setting": "SECRET-TPL-SETTING"}})
	must(err)
	save := func(space string, r catalog.SpaceRole) {
		t.Helper()
		_, err := cat.SaveSpaceRole(e.ctx, op, space, r)
		must(err)
	}
	save("spc_a", catalog.SpaceRole{Name: "impl-b", TemplateID: tpl.ID})
	save("spc_a", catalog.SpaceRole{Name: "impl-a", TemplateID: tpl.ID, Description: "Hard backend work.", Model: "opus", Instructions: "SECRET-INSTRUCTIONS",
		Settings: map[string]string{"role_setting": "SECRET-ROLE-SETTING"}})
	// A template with no profile: the role lists with an empty harness (a launch defaults it to the caller's).
	bare, err := cat.SaveTemplate(e.ctx, op, "", 0, catalog.RoleTemplate{Slug: "bare", Name: "Bare", Kind: "worker", Description: "No profile."})
	must(err)
	save("spc_a", catalog.SpaceRole{Name: "impl-c", TemplateID: bare.ID, Model: "haiku"})
	// A role whose profile is gone: listed with an error, not dropped.
	gone, err := cat.SaveProfile(e.ctx, op, "", 0, catalog.Profile{Slug: "gone", Name: "Gone", Harness: "claude-code"})
	must(err)
	save("spc_a", catalog.SpaceRole{Name: "impl-d", TemplateID: tpl.ID, ProfileID: gone.ID})
	if err := cat.DeleteProfile(e.ctx, op, gone.ID); err != nil {
		// Deleting an in-use profile is refused, so remove the row beneath the catalog.
		if err := e.s.W().Exec(e.ctx, "DELETE FROM harness_profiles WHERE id = ?", gone.ID); err != nil {
			t.Fatalf("cannot break the role: %v", err)
		}
	}
	save("spc_c", catalog.SpaceRole{Name: "other-space-role", TemplateID: tpl.ID, Description: "Never visible in space A."})

	reg := agenttools.New(agenttools.Deps{Extra: []func(*agenttools.Registry){Tools(nil, nil, cat)}})
	call := func(kind, space string) (json.RawMessage, error) {
		return reg.Dispatch(context.Background(), &fleet.RunPrincipal{RunID: "run_x", Kind: kind, SpaceID: space, Role: "x"}, "spaceman_roles_list", nil)
	}

	raw, err := call(agenttools.KindControl, "spc_a")
	must(err)
	var out struct {
		Roles []map[string]string `json:"roles"`
	}
	must(json.Unmarshal(raw, &out))
	want := []map[string]string{
		{"name": "impl-a", "kind": "worker", "description": "Hard backend work.", "description_origin": "space_role", "template": "implementor", "harness": "claude-code", "model": "opus"},
		{"name": "impl-b", "kind": "worker", "description": "Template text.", "description_origin": "template", "template": "implementor", "harness": "claude-code", "model": "sonnet"},
		{"name": "impl-c", "kind": "worker", "description": "No profile.", "description_origin": "template", "template": "bare", "model": "haiku"},
		{"name": "impl-d", "kind": "worker", "template": "implementor", "error": "role \"impl-d\" refers to a missing harness profile"},
	}
	if len(out.Roles) != len(want) {
		t.Fatalf("roles %v, want %v", out.Roles, want)
	}
	for i, w := range want {
		for k, v := range w {
			if out.Roles[i][k] != v {
				t.Errorf("role %d %s = %q, want %q", i, k, out.Roles[i][k], v)
			}
		}
		if len(out.Roles[i]) != len(w) {
			t.Errorf("role %d has unexpected fields: %v", i, out.Roles[i])
		}
	}
	if strings.Contains(string(raw), "other-space-role") || strings.Contains(string(raw), "Never visible") {
		t.Errorf("another space's role is listed: %s", raw)
	}
	for _, secret := range []string{"SECRET-", "prompt", "settings", "skills", "account"} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("the output contains %q: %s", secret, raw)
		}
	}

	// Space C sees only its own role.
	raw, err = call(agenttools.KindControl, "spc_c")
	must(err)
	if !strings.Contains(string(raw), "other-space-role") || strings.Contains(string(raw), "impl-a") {
		t.Errorf("space C's list: %s", raw)
	}

	raw, err = reg.Dispatch(context.Background(), &fleet.RunPrincipal{Kind: agenttools.KindControl}, proto.ToolCatalog, nil)
	must(err)
	if !strings.Contains(string(raw), "spaceman_roles_list") {
		t.Error("control's tool catalog lacks spaceman_roles_list")
	}
	// Only control runs may call it, and it is not in any other kind's catalog.
	for _, kind := range []string{agenttools.KindWorker, agenttools.KindKB, agenttools.KindDreamer, agenttools.KindDiagnostic} {
		if _, err := call(kind, "spc_a"); err == nil {
			t.Errorf("%s run was allowed to list roles (err %v)", kind, err)
		}
		raw, err := reg.Dispatch(context.Background(), &fleet.RunPrincipal{Kind: kind}, proto.ToolCatalog, nil)
		must(err)
		if strings.Contains(string(raw), "spaceman_roles_list") {
			t.Errorf("%s's tool catalog lists spaceman_roles_list", kind)
		}
	}
}
