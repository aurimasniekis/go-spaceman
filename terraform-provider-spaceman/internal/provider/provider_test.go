package provider

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/aurimasniekis/go-spaceman/terraform-provider-spaceman/internal/client"
)

// Every schema is valid for the framework (write-only rules, defaults on computed attributes, nested types).
func TestSchemasAreValid(t *testing.T) {
	ctx := context.Background()
	p := New("test")()
	names := map[string]bool{}
	for _, f := range p.Resources(ctx) {
		r := f()
		var md resource.MetadataResponse
		r.Metadata(ctx, resource.MetadataRequest{ProviderTypeName: "spaceman"}, &md)
		if names[md.TypeName] {
			t.Errorf("duplicate resource %s", md.TypeName)
		}
		names[md.TypeName] = true
		var sr resource.SchemaResponse
		r.Schema(ctx, resource.SchemaRequest{}, &sr)
		if sr.Diagnostics.HasError() {
			t.Errorf("%s: %v", md.TypeName, sr.Diagnostics)
		}
		if d := sr.Schema.ValidateImplementation(ctx); d.HasError() {
			t.Errorf("%s: %v", md.TypeName, d)
		}
		if _, ok := r.(resource.ResourceWithImportState); !ok {
			t.Errorf("%s has no import", md.TypeName)
		}
		if sr.Schema.MarkdownDescription == "" {
			t.Errorf("%s has no description", md.TypeName)
		}
		for name, a := range sr.Schema.Attributes {
			if a.GetMarkdownDescription() == "" {
				t.Errorf("%s.%s has no description", md.TypeName, name)
			}
			if strings.HasSuffix(name, "_wo") && !a.IsWriteOnly() {
				t.Errorf("%s.%s must be write-only", md.TypeName, name)
			}
			if a.IsWriteOnly() {
				if _, ok := sr.Schema.Attributes[name+"_version"]; !ok {
					t.Errorf("%s.%s has no %s_version companion", md.TypeName, name, name)
				}
			}
		}
	}
	if len(names) != 29 {
		t.Errorf("resources = %d", len(names))
	}
	for _, f := range p.DataSources(ctx) {
		d := f()
		var md datasource.MetadataResponse
		d.Metadata(ctx, datasource.MetadataRequest{ProviderTypeName: "spaceman"}, &md)
		var sr datasource.SchemaResponse
		d.Schema(ctx, datasource.SchemaRequest{}, &sr)
		if dd := sr.Schema.ValidateImplementation(ctx); dd.HasError() {
			t.Errorf("%s: %v", md.TypeName, dd)
		}
	}
}

// Runtime state is never a resource (SM-API-004, SM-HUI-008).
func TestNoRuntimeResources(t *testing.T) {
	ctx := context.Background()
	for _, f := range New("test")().Resources(ctx) {
		var md resource.MetadataResponse
		f().Metadata(ctx, resource.MetadataRequest{ProviderTypeName: "spaceman"}, &md)
		for _, bad := range []string{"run", "task", "inbox", "message", "human_action_request", "approval_request", "recording", "transcript"} {
			if strings.HasSuffix(md.TypeName, "_"+bad) || strings.Contains(md.TypeName, "_"+bad+"_") {
				t.Errorf("%s manages runtime state", md.TypeName)
			}
		}
	}
}

func TestCanonicalDefinition(t *testing.T) {
	a := `{"subject":"evt.x.>","title":"T","body":"B"}`
	b := `{
	  "body": "B", "title": "T", "subject": "evt.x.>", "variables": [], "filters": [],
	  "input_schema": null
	}`
	ca, err := canonicalDefinition(a)
	if err != nil {
		t.Fatal(err)
	}
	cb, err := canonicalDefinition(b)
	if err != nil {
		t.Fatal(err)
	}
	if ca != cb {
		t.Fatalf("equal definitions differ:\n%s\n%s", ca, cb)
	}
	c, _ := canonicalDefinition(`{"subject":"evt.x.>","title":"T","body":"B","input_schema":{"type":"object","properties":{"b":{},"a":{}}}}`)
	d, _ := canonicalDefinition(`{"input_schema":{"properties":{"a":{},"b":{}},"type":"object"},"subject":"evt.x.>","title":"T","body":"B"}`)
	if c != d {
		t.Fatal("input schema key order must not matter")
	}
	if _, err := canonicalDefinition(`{"subject":"x","titel":"typo"}`); err == nil {
		t.Fatal("unknown fields must be refused")
	}
	if _, err := canonicalDefinition(`not json`); err == nil {
		t.Fatal("invalid JSON must be refused")
	}
}

func TestDefinitionValueKeepsEquivalentPrior(t *testing.T) {
	prior := types.StringValue(`{"title":"T","body":"B","subject":"evt.x.>"}`)
	server := &client.TemplateDefinition{Subject: "evt.x.>", Title: "T", Body: "B"}
	if got := definitionValue(prior, server); !got.Equal(prior) {
		t.Fatalf("equivalent server definition replaced the configured text: %s", got)
	}
	server.Title = "Changed in the UI"
	got := definitionValue(prior, server)
	if got.Equal(prior) || !strings.Contains(got.ValueString(), "Changed in the UI") {
		t.Fatalf("a changed definition must be drift: %s", got)
	}
	if !definitionValue(prior, nil).IsNull() {
		t.Fatal("no definition is null")
	}
}

func TestJSONValue(t *testing.T) {
	prior := types.StringValue(`{ "type": "object", "required": ["a"] }`)
	if got := jsonValue(prior, json.RawMessage(`{"required":["a"],"type":"object"}`)); !got.Equal(prior) {
		t.Fatalf("formatting caused a diff: %s", got)
	}
	if got := jsonValue(prior, json.RawMessage(`{"required":["b"],"type":"object"}`)); got.Equal(prior) {
		t.Fatal("a changed schema must be drift")
	}
	if got := jsonValue(types.StringUnknown(), json.RawMessage(`{ "b":1, "a":2 }`)); got.ValueString() != `{"a":2,"b":1}` {
		t.Fatalf("canonical form = %s", got)
	}
	if got := jsonValue(types.StringNull(), nil); got.ValueString() != "" {
		t.Fatalf("missing JSON = %q", got.ValueString())
	}
	if !jsonEqual(`[1, 2]`, `[1,2]`) || jsonEqual(`[1,2]`, `[2,1]`) || !jsonEqual("", " ") {
		t.Fatal("jsonEqual")
	}
}

func TestConversions(t *testing.T) {
	ctx := context.Background()
	var d diag.Diagnostics
	m := mapValue(map[string]string{"a": "1"})
	if got := stringMap(ctx, m, &d); got["a"] != "1" || len(got) != 1 {
		t.Fatalf("map = %v", got)
	}
	if stringMap(ctx, types.MapNull(types.StringType), &d) != nil || stringList(ctx, types.ListUnknown(types.StringType), &d) != nil {
		t.Fatal("null and unknown convert to nil")
	}
	if got := stringList(ctx, listValue([]string{"x", "y"}), &d); strings.Join(got, ",") != "x,y" {
		t.Fatalf("list = %v", got)
	}
	if got := stringSet(ctx, setValue([]string{"b", "a"}), &d); len(got) != 2 {
		t.Fatalf("set = %v", got)
	}
	if mapValue(nil).IsNull() || listValue(nil).IsNull() || len(listValue(nil).Elements()) != 0 {
		t.Fatal("nil converts to empty, not null")
	}
	refs := skillRefs(ctx, skillRefsValue([]client.SkillRef{{SkillID: "skl_1"}, {SkillID: "skl_2", Pin: "skv_1"}}), &d)
	if len(refs) != 2 || refs[1].Pin != "skv_1" || refs[0].Pin != "" {
		t.Fatalf("skill refs = %+v", refs)
	}
	if boolPtr(types.BoolNull()) != nil || *boolPtr(types.BoolValue(false)) != false || strPtr(types.StringUnknown()) != nil {
		t.Fatal("pointers")
	}
	if d.HasError() {
		t.Fatal(d)
	}
}

func TestSplitImportID(t *testing.T) {
	var d diag.Diagnostics
	if p := splitImportID("spc_1/worker", 2, "<a>/<b>", &d); len(p) != 2 || p[1] != "worker" || d.HasError() {
		t.Fatalf("parts = %v %v", p, d)
	}
	for _, bad := range []string{"spc_1", "spc_1/", "/x", "a/b/c"} {
		var d diag.Diagnostics
		if splitImportID(bad, 2, "<a>/<b>", &d) != nil || !d.HasError() {
			t.Errorf("%q must be refused", bad)
		}
	}
}

type memPrivate map[string][]byte

func (m memPrivate) GetKey(_ context.Context, k string) ([]byte, diag.Diagnostics) { return m[k], nil }
func (m memPrivate) SetKey(_ context.Context, k string, v []byte) diag.Diagnostics {
	if !json.Valid(v) {
		var d diag.Diagnostics
		d.AddError("invalid", "private state must be JSON")
		return d
	}
	m[k] = v
	return nil
}

func TestVersionInPrivateState(t *testing.T) {
	ctx := context.Background()
	var d diag.Diagnostics
	p := memPrivate{}
	if getVersion(ctx, p, &d) != 0 {
		t.Fatal("no stored version is 0")
	}
	setVersion(ctx, p, 42, &d)
	if got := getVersion(ctx, p, &d); got != 42 || d.HasError() {
		t.Fatalf("version = %d, %v", got, d)
	}
	setVersion(ctx, p, 0, &d) // ignored
	if getVersion(ctx, p, &d) != 42 {
		t.Fatal("a zero version must not overwrite")
	}
}

func planString(t *testing.T, m planmodifier.String, state, plan types.String) diag.Diagnostics {
	t.Helper()
	ctx := context.Background()
	objType := tftypes.Object{AttributeTypes: map[string]tftypes.Type{"node_id": tftypes.String}}
	raw := func(v types.String) tftypes.Value {
		if v.IsNull() {
			return tftypes.NewValue(objType, nil)
		}
		return tftypes.NewValue(objType, map[string]tftypes.Value{"node_id": tftypes.NewValue(tftypes.String, v.ValueString())})
	}
	req := planmodifier.StringRequest{Path: path.Root("node_id"), StateValue: state, PlanValue: plan, ConfigValue: plan,
		State: tfsdk.State{Raw: raw(state)}, Plan: tfsdk.Plan{Raw: raw(plan)}}
	resp := &planmodifier.StringResponse{PlanValue: plan}
	m.PlanModifyString(ctx, req, resp)
	return resp.Diagnostics
}

// An identifying field the API cannot change is refused at plan time, never replaced (ADR-0035).
func TestFixedFieldRefusedAtPlanTime(t *testing.T) {
	fixed := fixedAfterCreate("A space's slug")
	if d := planString(t, fixed, types.StringNull(), types.StringValue("a")); d.HasError() {
		t.Fatalf("create: %v", d)
	}
	if d := planString(t, fixed, types.StringValue("a"), types.StringValue("a")); d.HasError() {
		t.Fatalf("unchanged: %v", d)
	}
	if d := planString(t, fixed, types.StringValue("a"), types.StringUnknown()); d.HasError() {
		t.Fatalf("unknown: %v", d)
	}
	d := planString(t, fixed, types.StringValue("a"), types.StringValue("b"))
	if !d.HasError() || !strings.Contains(d[0].Summary(), "cannot be changed") {
		t.Fatalf("change: %v", d)
	}
}

func TestAPIErrorHints(t *testing.T) {
	var d diag.Diagnostics
	addAPIError(&d, "Could not update", &client.APIError{Status: 409, Code: "precondition_failed", Detail: "changed", Operation: "PUT /spaces/x"})
	if !strings.Contains(d[0].Detail(), "changed in SpaceMan after Terraform last read it") {
		t.Fatalf("detail = %s", d[0].Detail())
	}
	d = nil
	addAPIError(&d, "Could not read", &client.APIError{Status: 401, Code: "unauthorized", Operation: "GET /spaces"})
	if !strings.Contains(d[0].Detail(), "SPACEMAN_TOKEN") {
		t.Fatalf("detail = %s", d[0].Detail())
	}
}
