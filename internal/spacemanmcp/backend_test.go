package spacemanmcp_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/aurimasniekis/go-spaceman/internal/api"
	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/gateway"
	"github.com/aurimasniekis/go-spaceman/internal/spacemanmcp"
)

// fakeTokens resolves one internal token whose state a test changes.
type fakeTokens struct {
	tok      auth.APIToken
	disabled bool
	expired  bool
	missing  bool
}

func (f *fakeTokens) InternalPrincipal(_ context.Context, id, runID string) (*auth.Principal, error) {
	if f.missing || id != f.tok.ID || f.tok.RevokedAt != nil || f.disabled || f.expired {
		return nil, auth.ErrUnauthorized
	}
	return &auth.Principal{User: &auth.User{ID: f.tok.UserID}, TokenID: id, TokenPurpose: auth.TokenPurposeSpacemanMCP, RunID: runID,
		Method: auth.MethodInternal}, nil
}

func (f *fakeTokens) GetAPIToken(_ context.Context, id string) (*auth.APIToken, error) {
	if f.missing || id != f.tok.ID {
		return nil, apperr.New(apperr.CodeNotFound, "API token not found")
	}
	t := f.tok
	return &t, nil
}

func (f *fakeTokens) Get(_ context.Context, id string) (*auth.User, error) {
	return &auth.User{ID: id, Enabled: !f.disabled}, nil
}

// The backend (ADR-0063) offers a server's selection less what its token's grants can never reach, classes a
// diagnostic launch as dangerous, runs calls with the token's principal for the run (defaulting space_id to the token's
// one space) and refuses with a reason once the token can no longer be used.
func TestBackend(t *testing.T) {
	t.Parallel()
	const space = "spc_0000000000000000000000000x"
	var rec seen
	toks := &fakeTokens{tok: auth.APIToken{ID: "atk_mcp", UserID: "usr_owner", Grants: []authz.Grant{{Role: authz.RoleViewer, Domain: space}},
		Purpose: auth.TokenPurposeSpacemanMCP}}
	b := &spacemanmcp.Backend{Cat: catalog(t), Handler: echo(&rec, 200, `{"id":"x"}`), Tokens: toks}
	var _ gateway.SpacemanBackend = b
	ctx := context.Background()
	tgt := gateway.SpacemanTarget{ID: "mcp_1", Slug: "sm", Name: "SpaceMan", TokenID: "atk_mcp",
		Settings: gateway.SpacemanSettings{Preset: "operate", Tools: []string{"delete_schedule", "get_recording"}}, MaxResultBytes: 1 << 20}

	if err := b.Validate(gateway.SpacemanSettings{Preset: "custom", Tools: []string{"respond_human_action"}}); err == nil {
		t.Error("an excluded tool was accepted")
	}
	if err := b.Validate(gateway.SpacemanSettings{Preset: "everything"}); err == nil {
		t.Error("an unknown preset was accepted")
	}
	if err := b.Validate(gateway.SpacemanSettings{Preset: "operate", Tools: []string{"delete_schedule"}}); err != nil {
		t.Error(err)
	}

	// A viewer token on one space: the reads of that space and the filtered reads; no writes (operate is the selection,
	// the grants decide), no global reads; named dangerous and sensitive tools only if reachable.
	var names []string
	for _, tl := range b.Catalog(ctx, tgt) {
		names = append(names, tl.Name)
		if !strings.HasPrefix(tl.Name, "sm__") || !strings.HasPrefix(tl.Description, "[SpaceMan] ") {
			t.Errorf("tool %s: %q", tl.Name, tl.Description)
		}
	}
	if !slices.Contains(names, "sm__get_space") || !slices.Contains(names, "sm__list_spaces") || slices.Contains(names, "sm__create_task") ||
		slices.Contains(names, "sm__list_nodes") || slices.Contains(names, "sm__delete_schedule") {
		t.Errorf("viewer catalog: %v", names)
	}
	toks.tok.Grants = []authz.Grant{{Role: authz.RoleAdmin, Domain: space}}
	var dangerous *string
	for _, tl := range b.Catalog(ctx, tgt) {
		if tl.Name == "sm__delete_schedule" {
			dangerous = &tl.Description
		}
	}
	if dangerous == nil || !strings.Contains(*dangerous, "needs a person's approval") {
		t.Errorf("admin catalog lacks the named dangerous tool, or it does not say it needs an approval: %v", dangerous)
	}

	for _, tc := range []struct {
		tool, args, want string
		ok               bool
	}{
		{"get_space", `{}`, "read", true},
		{"create_task", `{}`, "write", true},
		{"launch_run", `{"body":{"kind":"worker"}}`, "write", true},
		{"launch_run", `{"body":{"kind":"diagnostic"}}`, "dangerous", true},
		{"launch_run", `{"body":{"kind":" Diagnostic "}}`, "dangerous", true},
		{"launch_run", `[]`, "write", true},
		{"delete_schedule", `{}`, "dangerous", true},
		{"get_recording", `{}`, "sensitive-read", true},
		{"delete_space", `{}`, "", false},         // dangerous, not named
		{"respond_human_action", `{}`, "", false}, // excluded
		{"no_such_tool", `{}`, "", false},
	} {
		if c, ok := b.Class(tgt, tc.tool, json.RawMessage(tc.args)); c != tc.want || ok != tc.ok {
			t.Errorf("class of %s %s: %q %v", tc.tool, tc.args, c, ok)
		}
	}

	// A call runs with the token's principal for the run; a missing space_id is the token's one space.
	raw, err := b.Call(ctx, tgt, "run_c", "get_space", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	var res mcp.CallToolResult
	if err := json.Unmarshal(raw, &res); err != nil || res.IsError || rec.uri != api.Prefix+"/spaces/"+space || rec.method != http.MethodGet {
		t.Fatalf("call: %s %v (%+v)", raw, err, rec)
	}
	if _, err := b.Call(ctx, tgt, "run_c", "delete_space", json.RawMessage(`{}`)); apperr.CodeOf(err) != apperr.CodeNotFound {
		t.Errorf("tool not offered: %v", err)
	}
	toks.tok.Grants = []authz.Grant{{Role: authz.RoleViewer, Domain: space}}
	if _, err := b.Call(ctx, tgt, "run_c", "list_nodes", json.RawMessage(`{}`)); apperr.CodeOf(err) != apperr.CodeForbidden {
		t.Errorf("tool the grants cannot reach: %v", err)
	}

	// A token that can no longer be used: empty catalog, and calls refused with the reason.
	now := time.Now()
	for reason, set := range map[string]func(){
		"was revoked":     func() { toks.tok.RevokedAt = &now },
		"is disabled":     func() { toks.disabled = true },
		"expired":         func() { toks.expired = true },
		"no longer exist": func() { toks.missing = true },
	} {
		*toks = fakeTokens{tok: toks.tok}
		toks.tok.RevokedAt = nil
		set()
		if c := b.Catalog(ctx, tgt); len(c) != 0 {
			t.Errorf("%s: catalog of %d tools", reason, len(c))
		}
		_, err := b.Call(ctx, tgt, "run_c", "get_space", json.RawMessage(`{}`))
		if !errors.Is(err, apperr.ErrForbidden) || !strings.Contains(err.Error(), reason) || !strings.Contains(err.Error(), "re-issue") {
			t.Errorf("%s: %v", reason, err)
		}
	}
}

// The tool picker lists every offerable tool with its class and tags, marked with what the preset, named tools and
// grants make of it: a viewer token reaches no writes, and named dangerous tools are selected but not in the preset.
func TestBackendTools(t *testing.T) {
	t.Parallel()
	const space = "spc_0000000000000000000000000x"
	b := &spacemanmcp.Backend{Cat: catalog(t)}
	if _, err := b.Tools(gateway.SpacemanSettings{Tools: []string{"respond_human_action"}}, nil); err == nil {
		t.Error("an excluded tool was accepted")
	}
	tools, err := b.Tools(gateway.SpacemanSettings{Preset: "read", Tools: []string{"delete_schedule"}}, []authz.Grant{{Role: authz.RoleViewer, Domain: space}})
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != len(catalog(t).Tools()) {
		t.Fatalf("%d tools, want every offerable one", len(tools))
	}
	by := map[string]gateway.SpacemanTool{}
	for _, tl := range tools {
		by[tl.Name] = tl
		if tl.Class == "excluded" || len(tl.Tags) == 0 || tl.Summary == "" {
			t.Errorf("tool %+v", tl)
		}
	}
	if g := by["get_space"]; !g.InPreset || !g.Selected || !g.Reachable || g.Class != "read" {
		t.Errorf("get_space: %+v", g)
	}
	if c := by["create_task"]; c.InPreset || c.Selected || c.Reachable || c.Class != "write" {
		t.Errorf("create_task: %+v", c)
	}
	if d := by["delete_schedule"]; d.InPreset || !d.Selected || d.Reachable || d.Class != "dangerous" {
		t.Errorf("delete_schedule: %+v", d)
	}
	admin, err := b.Tools(gateway.SpacemanSettings{}, []authz.Grant{{Role: authz.RoleAdmin, Domain: authz.Global}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range admin {
		if want := tl.Class != "sensitive-read"; tl.InPreset != want || !tl.Reachable {
			t.Errorf("admin preset: %+v", tl)
		}
	}
}
