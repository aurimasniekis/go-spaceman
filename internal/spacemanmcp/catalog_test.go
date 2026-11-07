package spacemanmcp_test

import (
	"encoding/json"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/jsonschema-go/jsonschema"

	"github.com/aurimasniekis/go-spaceman/internal/api"
	"github.com/aurimasniekis/go-spaceman/internal/app"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/schemautil"
	"github.com/aurimasniekis/go-spaceman/internal/spacemanmcp"
)

var (
	specOnce sync.Once
	spec     *huma.OpenAPI
	specDeps *api.Deps
)

// buildSpec returns the build's full API document and rules (every feature's operations).
func buildSpec(t *testing.T) (*huma.OpenAPI, *api.Deps) {
	t.Helper()
	specOnce.Do(func() {
		specDeps = app.SpecDeps()
		spec = api.Register(http.NewServeMux(), specDeps).OpenAPI()
	})
	return spec, specDeps
}

func catalog(t *testing.T) *spacemanmcp.Catalog {
	t.Helper()
	s, d := buildSpec(t)
	c, err := spacemanmcp.Build(s, d.RuleFor)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func operationIDs(s *huma.OpenAPI) []string {
	var ids []string
	for _, item := range s.Paths {
		for _, op := range []*huma.Operation{item.Get, item.Post, item.Put, item.Patch, item.Delete} {
			if op != nil {
				ids = append(ids, op.OperationID)
			}
		}
	}
	slices.Sort(ids)
	return ids
}

// Every API operation has a reviewed class (ADR-0063): it is excluded by its rule, or listed in Table with a class and
// a reason. A new operation fails here until someone classifies it. Table names only real operations.
func TestEveryOperationIsClassified(t *testing.T) {
	t.Parallel()
	s, d := buildSpec(t)
	ids := operationIDs(s)
	for _, id := range ids {
		r, ok := d.RuleFor(id)
		if !ok {
			t.Errorf("%s has no rule", id)
			continue
		}
		e, ok := spacemanmcp.Classify(id, r)
		if !ok {
			t.Errorf("operation %s is not classified: add it to spacemanmcp.Table with a class and a reason (and review ADR-0063)", id)
			continue
		}
		if !slices.Contains(spacemanmcp.Classes, e.Class) || e.Reason == "" {
			t.Errorf("%s: class %q, reason %q", id, e.Class, e.Reason)
		}
	}
	for id, e := range spacemanmcp.Table {
		if !slices.Contains(ids, id) {
			t.Errorf("Table names %s, which the API document lacks", id)
		}
		if !slices.Contains(spacemanmcp.Classes, e.Class) || e.Reason == "" {
			t.Errorf("Table %s: class %q, reason %q", id, e.Class, e.Reason)
		}
	}
	c := catalog(t)
	if n := len(c.Tools()) + len(c.Excluded()); n != len(ids) {
		t.Errorf("%d tools + %d excluded, want %d operations", len(c.Tools()), len(c.Excluded()), len(ids))
	}
}

// The hard exclusions hold whatever a server's settings (ADR-0063 §Decision 6): no tool acts as a person (approvals,
// human-action answers, takeover and input), manages users, tokens or sign-in, or returns a one-time secret.
func TestHardExclusions(t *testing.T) {
	t.Parallel()
	c := catalog(t)
	for _, id := range []string{
		// Need a person at the keyboard (rule flags).
		"login", "get-me", "create-api-token", "revoke-my-token", "create-user", "set-user-grants", "issue-reset",
		"revoke-any-token", "rotate-root-token", "acknowledge-break-glass", "start-takeover", "create-oidc-provider",
		// One-time secrets.
		"create-node", "reissue-enrollment",
		// Explicit.
		"list-approvals", "get-approval", "revoke-approval", "create-approval-policy", "delete-approval-policy",
		"respond-human-action", "assign-human-action", "cancel-human-action", "clarify-human-action", "comment-human-action",
		"move-human-action", "route-human-notification", "takeover-input", "send-input", "end-takeover", "renew-takeover",
		"list-users", "get-user", "list-all-tokens", "get-root-token", "list-oidc-providers", "get-oidc-test-login",
		"acknowledge-restore", "upload-attachment",
		// Personal session operations (a filtered read is not one).
		"logout", "list-my-tokens", "get-notification-preferences",
		// Harbor Pilot connection writes need a step-up (dangerous if they ever stop needing one).
		"put-harbor-pilot-connection", "delete-harbor-pilot-connection",
	} {
		if _, ok := c.Excluded()[id]; !ok {
			t.Errorf("%s is offered; it must be excluded", id)
		}
	}
	for name, want := range map[string]spacemanmcp.Class{
		"get_space": spacemanmcp.Read, "launch_run": spacemanmcp.Write, "create_task": spacemanmcp.Write,
		"put_space_harbor_pilot": spacemanmcp.Dangerous, "delete_space_harbor_pilot": spacemanmcp.Write,
		"get_harbor_pilot_connection": spacemanmcp.Read, "save_harness_profile": spacemanmcp.Dangerous,
		"create_account": spacemanmcp.Dangerous, "create_mcp_server": spacemanmcp.Dangerous,
		"delete_schedule": spacemanmcp.Dangerous, "get_recording": spacemanmcp.SensitiveRead,
	} {
		tl, ok := c.Tool(name)
		if !ok || tl.Class != want {
			t.Errorf("%s: %+v, want class %s", name, tl, want)
		}
	}
	// The Session-rule reads filtered by the principal (api.FilteredRead) are read tools; they are the only Session
	// rules that are.
	filtered := []string{"list-spaces", "list-event-connections", "get-event-connection", "list-event-templates",
		"get-event-template", "preview-event-template", "get-attention", "list-account-kinds", "list-limit-definitions",
		"list-pending-human-actions", "get-restore-state"}
	for _, id := range filtered {
		if tl, ok := c.Tool(strings.ReplaceAll(id, "-", "_")); !ok || tl.Class != spacemanmcp.Read {
			t.Errorf("%s: %+v, want a read tool", id, tl)
		}
	}
	s, d := buildSpec(t)
	for _, id := range operationIDs(s) {
		if r, _ := d.RuleFor(id); r.Session && r.Filtered != slices.Contains(filtered, id) {
			t.Errorf("%s: Filtered %v; review it (a filtered read must return only what the principal's grants reach)", id, r.Filtered)
		}
	}
	if e := spacemanmcp.Table["put-harbor-pilot-connection"]; e.Class != spacemanmcp.Dangerous {
		t.Errorf("put-harbor-pilot-connection falls back to %s, want dangerous", e.Class)
	}
}

// Every step-up operation (re-authentication or a second factor, not on the caller's own account) has an explicit
// Table entry, so that if its rule ever stops needing a step-up it takes a reviewed class instead of whatever a hurried
// fix picks. The administrator operations (users, sign-in settings and providers, tokens, node enrollment) fall back
// to Excluded; destructive ones (purges, relocations, retention) to Dangerous.
func TestStepUpOperationsFallBackToExcluded(t *testing.T) {
	t.Parallel()
	s, d := buildSpec(t)
	n := 0
	for _, id := range operationIDs(s) {
		r, _ := d.RuleFor(id)
		if !(r.Reauth || r.MFA) || r.Session || r.UserOnly {
			continue
		}
		n++
		if e, ok := spacemanmcp.Table[id]; !ok || e.Class != spacemanmcp.Excluded && e.Class != spacemanmcp.Dangerous {
			t.Errorf("step-up operation %s falls back to %q: add an explicit Excluded (or Dangerous) entry", id, e.Class)
		}
	}
	for _, id := range []string{"update-user", "disable-user", "enable-user", "issue-link-invite", "link-user-identity",
		"unlink-user-identity", "set-sign-in-settings", "create-oidc-provider", "update-oidc-provider", "delete-oidc-provider",
		"add-oidc-mapping", "delete-oidc-mapping", "revoke-root-token", "create-node", "reissue-enrollment", "create-user",
		"set-user-grants", "issue-reset", "revoke-any-token", "rotate-root-token", "acknowledge-break-glass", "start-takeover"} {
		if e := spacemanmcp.Table[id]; e.Class != spacemanmcp.Excluded {
			t.Errorf("%s falls back to %q, want excluded", id, e.Class)
		}
	}
	if n < 20 {
		t.Errorf("only %d step-up operations", n)
	}
}

var reGatewayTool = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Tool names are unique and fit the gateway's 64-character limit with the longest slug.
func TestToolNames(t *testing.T) {
	t.Parallel()
	c := catalog(t)
	seen := map[string]bool{}
	slug := strings.Repeat("s", spacemanmcp.MaxSlug)
	for _, tl := range c.Tools() {
		full := tl.FullName(slug)
		if len(full) > spacemanmcp.MaxToolName || !reGatewayTool.MatchString(tl.Name) {
			t.Errorf("tool %s: %q (%d characters)", tl.OperationID, full, len(full))
		}
		if seen[tl.Name] {
			t.Errorf("duplicate tool name %s", tl.Name)
		}
		seen[tl.Name] = true
		if tl.Name != strings.ReplaceAll(tl.OperationID, "-", "_") {
			t.Errorf("tool %s named %s", tl.OperationID, tl.Name)
		}
	}
	if len(c.Tools()) < 150 {
		t.Errorf("only %d tools", len(c.Tools()))
	}
}

// Every input schema is a self-contained JSON Schema object (no references) that both the MCP SDK and SpaceMan's
// schema compiler accept, with the path parameters required.
func TestSchemasAreValidObjects(t *testing.T) {
	t.Parallel()
	c := catalog(t)
	for _, tl := range c.Tools() {
		if strings.Contains(string(tl.InputSchema), `"$ref"`) {
			t.Errorf("%s: schema has a reference", tl.Name)
		}
		var s jsonschema.Schema
		if err := json.Unmarshal(tl.InputSchema, &s); err != nil {
			t.Errorf("%s: %v", tl.Name, err)
			continue
		}
		if s.Type != "object" {
			t.Errorf("%s: schema type %q", tl.Name, s.Type)
		}
		if _, err := s.Resolve(nil); err != nil {
			t.Errorf("%s: MCP SDK: %v", tl.Name, err)
		}
		if _, err := schemautil.CompileObject(tl.InputSchema); err != nil {
			t.Errorf("%s: %v", tl.Name, err)
		}
		for _, p := range tl.PathParams {
			if !slices.Contains(s.Required, p) {
				t.Errorf("%s: path parameter %s not required", tl.Name, p)
			}
		}
		if _, ok := s.Properties["$schema"]; ok {
			t.Errorf("%s: top-level $schema", tl.Name)
		}
		if tl.Body {
			if b := s.Properties[spacemanmcp.ArgBody]; b == nil || b.Properties["$schema"] != nil {
				t.Errorf("%s: body schema %+v", tl.Name, b)
			}
		}
		if !strings.HasPrefix(tl.Description, "[SpaceMan · "+string(tl.Class)+"] ") || len(tl.Description) > 1024 {
			t.Errorf("%s: description %q", tl.Name, tl.Description)
		}
	}
	tl, _ := c.Tool("update_space")
	if !tl.IfMatch || !strings.Contains(string(tl.InputSchema), `"if_match"`) || tl.Idempotent {
		t.Errorf("update_space: %+v", tl)
	}
	tl, _ = c.Tool("create_space")
	if !tl.Idempotent || strings.Contains(string(tl.InputSchema), "Idempotency") {
		t.Errorf("create_space: %+v %s", tl, tl.InputSchema)
	}
}

func names(ts []*spacemanmcp.Tool) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.Name
	}
	return out
}

// Presets are deterministic: the same selection always gives the same sorted tools. read ⊂ operate ⊂ admin; admin
// ("infra") adds the dangerous tools, each flagged approval-required, with global grants; read and operate offer
// dangerous tools only by name, and no preset offers sensitive reads; custom offers only names.
func TestPresetsAreDeterministic(t *testing.T) {
	t.Parallel()
	c := catalog(t)
	global := []authz.Grant{{Role: authz.RoleAdmin, Domain: authz.Global}}
	sel := func(s spacemanmcp.Selection) []string {
		t.Helper()
		ts, err := c.Select(s, global)
		if err != nil {
			t.Fatal(err)
		}
		return names(ts)
	}
	for _, p := range spacemanmcp.Presets {
		a, b := sel(spacemanmcp.Selection{Preset: p}), sel(spacemanmcp.Selection{Preset: p})
		if !slices.Equal(a, b) || !slices.IsSorted(a) {
			t.Errorf("preset %s not deterministic", p)
		}
		for _, n := range a {
			tl, _ := c.Tool(n)
			if tl.Class == spacemanmcp.SensitiveRead || tl.Class == spacemanmcp.Dangerous && p != spacemanmcp.PresetAdmin {
				t.Errorf("preset %s offers %s (%s) without naming it", p, n, tl.Class)
			}
			if tl.ApprovalRequired() != (tl.Class == spacemanmcp.Dangerous) {
				t.Errorf("%s (%s): approval required %v", n, tl.Class, tl.ApprovalRequired())
			}
		}
	}
	read, operate, admin := sel(spacemanmcp.Selection{Preset: spacemanmcp.PresetRead}), sel(spacemanmcp.Selection{Preset: spacemanmcp.PresetOperate}),
		sel(spacemanmcp.Selection{})
	for _, n := range read {
		if !slices.Contains(operate, n) {
			t.Errorf("read tool %s not in operate", n)
		}
	}
	for _, n := range operate {
		if !slices.Contains(admin, n) {
			t.Errorf("operate tool %s not in admin", n)
		}
	}
	// The admin ("infra") preset offers dangerous tools, each one approval-gated; never sensitive reads.
	for _, n := range []string{"save_harness_profile", "delete_schedule", "create_account"} {
		tl, _ := c.Tool(n)
		if !slices.Contains(admin, n) || slices.Contains(operate, n) || !tl.ApprovalRequired() {
			t.Errorf("%s: in admin %v, in operate %v, approval required %v", n, slices.Contains(admin, n), slices.Contains(operate, n), tl.ApprovalRequired())
		}
	}
	if slices.Contains(admin, "get_recording") {
		t.Error("the admin preset offers get_recording without naming it")
	}
	if len(admin) <= len(operate) || len(operate) <= len(read) || !slices.Contains(read, "get_space") || slices.Contains(read, "create_task") {
		t.Errorf("read %d, operate %d, admin %d", len(read), len(operate), len(admin))
	}
	if spacemanmcp.DefaultPreset != spacemanmcp.PresetAdmin || spacemanmcp.PresetAdmin.Grants("")[0] != global[0] {
		t.Error("the default preset is not admin@*")
	}
	custom := sel(spacemanmcp.Selection{Preset: spacemanmcp.PresetCustom, Tools: []string{"save_harness_profile", "get_space"}})
	if !slices.Equal(custom, []string{"get_space", "save_harness_profile"}) {
		t.Errorf("custom: %v", custom)
	}
	withDanger := sel(spacemanmcp.Selection{Preset: spacemanmcp.PresetOperate, Tools: []string{"delete_schedule"}})
	if !slices.Contains(withDanger, "delete_schedule") || len(withDanger) != len(operate)+1 {
		t.Errorf("operate + delete_schedule: %d tools", len(withDanger))
	}
	for _, bad := range []spacemanmcp.Selection{{Preset: "everything"}, {Tools: []string{"respond_human_action"}}, {Tools: []string{"no_such_tool"}}} {
		if _, err := c.Select(bad, global); err == nil {
			t.Errorf("selection %+v accepted", bad)
		}
	}
}

// The catalog hides what the token's grants can never reach: a space viewer sees only that space's read tools, no
// writes and no global operations (nodes, accounts, catalogs).
func TestGrantFilter(t *testing.T) {
	t.Parallel()
	c := catalog(t)
	const space = "spc_0000000000000000000000000x"
	viewer, err := c.Select(spacemanmcp.Selection{Preset: spacemanmcp.PresetOperate, Tools: []string{"delete_schedule"}},
		spacemanmcp.PresetRead.Grants(space))
	if err != nil {
		t.Fatal(err)
	}
	vn := names(viewer)
	for _, tl := range viewer {
		if tl.Class != spacemanmcp.Read || !tl.Rule.Space {
			t.Errorf("viewer@space sees %s (%s, space rule %v)", tl.Name, tl.Class, tl.Rule.Space)
		}
	}
	if !slices.Contains(vn, "get_space") || !slices.Contains(vn, "list_tasks") || slices.Contains(vn, "list_nodes") || len(vn) < 30 {
		t.Errorf("viewer@space: %d tools %v", len(vn), vn)
	}
	op, _ := c.Select(spacemanmcp.Selection{Preset: spacemanmcp.PresetOperate}, spacemanmcp.PresetOperate.Grants(space))
	on := names(op)
	if !slices.Contains(on, "create_task") || !slices.Contains(on, "launch_run") || slices.Contains(on, "create_space") || slices.Contains(on, "list_accounts") {
		t.Errorf("operator@space: %v", on)
	}
	all, _ := c.Select(spacemanmcp.Selection{Preset: spacemanmcp.PresetOperate}, spacemanmcp.PresetAdmin.Grants(""))
	if len(all) <= len(op) || !slices.Contains(names(all), "create_space") {
		t.Errorf("admin@*: %d tools", len(all))
	}
	if none, _ := c.Select(spacemanmcp.Selection{}, nil); len(none) != 0 {
		t.Errorf("no grants: %d tools", len(none))
	}
}
