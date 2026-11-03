package provider

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/aurimasniekis/go-spaceman/terraform-provider-spaceman/internal/client"
)

func i64(n int64) types.Int64 { return types.Int64Value(n) }

func nulls() map[string]types.Int64 {
	m := map[string]types.Int64{}
	for _, l := range spaceLimitAttrs {
		m[l.key] = types.Int64Null()
	}
	return m
}

// Limits: Terraform sets what it configures, clears what it stops configuring and keeps overrides it never owned.
func TestMergeLimits(t *testing.T) {
	current := map[string]int64{"max_concurrent_runs": 9, "support_depth": 2, "max_kb_sessions": 7}
	prior, plan := nulls(), nulls()
	prior["max_concurrent_runs"], prior["support_depth"] = i64(4), i64(2)
	plan["max_concurrent_runs"], plan["budget_warn_percent"] = i64(6), i64(50)
	got := mergeLimits(current, prior, plan)
	want := map[string]int64{"max_concurrent_runs": 6, "budget_warn_percent": 50, "max_kb_sessions": 7}
	if len(got) != len(want) {
		t.Fatalf("merged = %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("merged = %v, want %v", got, want)
		}
	}
	if current["support_depth"] != 2 {
		t.Fatal("the current overrides must not be modified")
	}
	// Destroy: nothing planned, the owned overrides go.
	if got := mergeLimits(current, prior, nulls()); len(got) != 1 || got["max_kb_sessions"] != 7 {
		t.Fatalf("destroy = %v", got)
	}
}

func TestMergeOnLower(t *testing.T) {
	for _, tc := range []struct {
		current     string
		prior, plan types.String
		want        string
	}{
		{"stop", types.StringNull(), types.StringNull(), "stop"},                 // not owned: kept
		{"drain", types.StringNull(), types.StringValue("stop"), "stop"},         // owned now
		{"stop", types.StringValue("stop"), types.StringNull(), "drain"},         // no longer owned: reset
		{"drain", types.StringValue("drain"), types.StringValue("stop"), "stop"}, // changed
	} {
		if got := mergeOnLower(tc.current, tc.prior, tc.plan); got != tc.want {
			t.Errorf("mergeOnLower(%s, %v, %v) = %s, want %s", tc.current, tc.prior, tc.plan, got, tc.want)
		}
	}
}

// Refresh: owned limits follow SpaceMan (drift), unowned ones stay null; an import owns every stored override.
func TestOwnedFrom(t *testing.T) {
	six, seven := int64(6), int64(7)
	l := &client.SpaceLimits{OnLower: "stop", Version: 3, Limits: []client.Limit{
		{Key: "max_concurrent_runs", Override: &six}, {Key: "max_kb_sessions", Override: &seven}, {Key: "support_depth"}}}
	prior := limitsState{spaceID: "spc_1", values: nulls(), onLower: types.StringNull()}
	prior.values["max_concurrent_runs"], prior.values["support_depth"] = i64(4), i64(2)
	got := ownedFrom(l, prior, false)
	if got.values["max_concurrent_runs"].ValueInt64() != 6 || !got.values["support_depth"].IsNull() || !got.values["max_kb_sessions"].IsNull() {
		t.Fatalf("refresh = %v", got.values)
	}
	if !got.onLower.IsNull() {
		t.Fatal("an unowned on_lower must stay null")
	}
	imp := ownedFrom(l, limitsState{spaceID: "spc_1", values: nulls(), onLower: types.StringNull()}, true)
	if imp.values["max_kb_sessions"].ValueInt64() != 7 || imp.values["max_concurrent_runs"].ValueInt64() != 6 || imp.onLower.ValueString() != "stop" {
		t.Fatalf("import = %v %v", imp.values, imp.onLower)
	}
	l.Version = 0
	if imp := ownedFrom(l, limitsState{values: nulls(), onLower: types.StringNull()}, true); !imp.onLower.IsNull() {
		t.Fatal("nothing stored: an import does not take on_lower")
	}
}

// Every overridable limit of SpaceMan is an attribute; the queue-policy limit is not.
func TestLimitAttributes(t *testing.T) {
	seen := map[string]bool{}
	for _, l := range spaceLimitAttrs {
		if seen[l.key] || l.desc == "" {
			t.Errorf("limit %s duplicated or undocumented", l.key)
		}
		seen[l.key] = true
	}
	if len(seen) != 19 || seen["max_top_level_tasks"] {
		t.Fatalf("limits = %v", seen)
	}
}

func TestQueueInputAndChange(t *testing.T) {
	cur := &client.QueuePolicy{MaxActive: 1, IncidentInsertion: "top", IncidentPreempts: true}
	m := queuePolicyModel{MaxActive: i64(3), IncidentInsertion: types.StringUnknown(), IncidentPreempts: types.BoolUnknown(), EscalationReorders: types.BoolValue(true)}
	in := queueInput(&m, cur)
	if in.MaxActive != 3 || in.IncidentInsertion != "top" || !in.IncidentPreempts || !in.EscalationReorders {
		t.Fatalf("input = %+v", in)
	}
	prior := queuePolicyModel{MaxActive: i64(1), IncidentInsertion: types.StringValue("top"), IncidentPreempts: types.BoolValue(true), EscalationReorders: types.BoolValue(false)}
	if queueChanged(&prior, cur) {
		t.Fatal("unchanged policy reported as changed")
	}
	cur.MaxActive = 2
	if !queueChanged(&prior, cur) {
		t.Fatal("a change after the plan must be detected")
	}
}

func TestHumanPolicyFieldsAndBase(t *testing.T) {
	l := &client.HumanPolicies{Effective: []*client.HumanPolicy{
		{ActionType: "question", HumanPolicyFields: client.HumanPolicyFields{RemindAfterMinutes: 240, MaxReminders: 3, RequesterEnded: "reroute"}}}}
	if b := policyBase(l, "question", nil); b.RemindAfterMinutes != 240 || b.MaxReminders != 3 {
		t.Fatalf("question base = %+v", b)
	}
	if b := policyBase(l, "all", nil); b != builtinAllPolicy {
		t.Fatalf("all base = %+v", b)
	}
	defaults := &client.HumanPolicies{Items: []*client.HumanPolicy{{ActionType: "all", HumanPolicyFields: client.HumanPolicyFields{MaxReminders: 5}}}}
	if b := policyBase(l, "all", defaults); b.MaxReminders != 5 {
		t.Fatalf("all base from the installation default = %+v", b)
	}
	m := humanPolicyModel{RemindAfterMinutes: i64(10), RemindEveryMinutes: types.Int64Unknown(), MaxReminders: types.Int64Null(),
		EscalateAfterMinutes: types.Int64Unknown(), EscalatePriority: types.StringValue("urgent"), EscalateAssignee: types.StringUnknown(),
		RequesterEnded: types.StringUnknown(), HoldMinutes: types.Int64Unknown(), SuccessorRole: types.StringUnknown(), WakeIdle: types.BoolValue(false),
		RatePerRunHour: types.Int64Unknown(), RatePerSpaceHour: types.Int64Unknown()}
	f := m.fields(policyBase(l, "question", nil))
	if f.RemindAfterMinutes != 10 || f.MaxReminders != 3 || f.EscalatePriority != "urgent" || f.WakeIdle || f.RequesterEnded != "reroute" {
		t.Fatalf("fields = %+v", f)
	}
}

func importState(t *testing.T, r resource.Resource, id string) (tfsdk.State, bool) {
	t.Helper()
	ctx := context.Background()
	var sr resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &sr)
	st := tfsdk.State{Schema: sr.Schema, Raw: tftypes.NewValue(sr.Schema.Type().TerraformType(ctx), nil)}
	resp := &resource.ImportStateResponse{State: st}
	r.(resource.ResourceWithImportState).ImportState(ctx, resource.ImportStateRequest{ID: id}, resp)
	return resp.State, !resp.Diagnostics.HasError()
}

func TestImportIDs(t *testing.T) {
	ctx := context.Background()
	attr := func(st tfsdk.State, name string) string {
		var v types.String
		st.GetAttribute(ctx, path.Root(name), &v)
		return v.ValueString()
	}
	st, ok := importState(t, newHumanPolicyResource(), "todo")
	if !ok || attr(st, "space_id") != "" || attr(st, "action_type") != "todo" {
		t.Fatal("installation default import")
	}
	st, ok = importState(t, newHumanPolicyResource(), "spc_1/question")
	if !ok || attr(st, "space_id") != "spc_1" || attr(st, "action_type") != "question" {
		t.Fatal("space policy import")
	}
	for _, bad := range []string{"spc_1/nope", "/todo", "nope"} {
		if _, ok := importState(t, newHumanPolicyResource(), bad); ok {
			t.Errorf("%q must be refused", bad)
		}
	}
	st, ok = importState(t, newUserIdentityResource(), "usr_1/oidc_1/tenant/user")
	if !ok || attr(st, "subject") != "tenant/user" || attr(st, "provider_id") != "oidc_1" {
		t.Fatal("identity import keeps slashes in the subject")
	}
	if _, ok := importState(t, newUserIdentityResource(), "usr_1/oidc_1"); ok {
		t.Fatal("identity import needs three parts")
	}
	st, ok = importState(t, newXSpacePeerResource(), "spc_a/spc_b")
	if !ok || attr(st, "space_id") != "spc_a" || attr(st, "peer_id") != "spc_b" {
		t.Fatal("peer import")
	}
}

func validate(v validator.String, s string) bool {
	resp := &validator.StringResponse{}
	v.ValidateString(context.Background(), validator.StringRequest{Path: path.Root("x"), ConfigValue: types.StringValue(s)}, resp)
	return !resp.Diagnostics.HasError()
}

func TestValidators(t *testing.T) {
	if !validate(lowerCaseEmail{}, "ops@example.com") || validate(lowerCaseEmail{}, "Ops@example.com") || validate(lowerCaseEmail{}, " ops@example.com") ||
		validate(lowerCaseEmail{}, "ops") {
		t.Fatal("email validator")
	}
	if !validate(rfc3339Validator{}, "2026-01-01T09:00:00+02:00") || validate(rfc3339Validator{}, "tomorrow") {
		t.Fatal("RFC 3339 validator")
	}
}

func TestKeepTrimmedAndInstant(t *testing.T) {
	if got := keepTrimmed(types.StringValue(" Daily "), "Daily"); got.ValueString() != " Daily " {
		t.Fatalf("trimmed = %q", got.ValueString())
	}
	if got := keepTrimmed(types.StringValue("Daily"), "Weekly"); got.ValueString() != "Weekly" {
		t.Fatal("a change must be drift")
	}
	at := time.Date(2026, 1, 1, 7, 0, 0, 0, time.UTC)
	if !sameInstant("2026-01-01T09:00:00+02:00", at) || sameInstant("2026-01-01T09:00:00Z", at) || sameInstant("bad", at) {
		t.Fatal("sameInstant")
	}
}

func TestSameGrants(t *testing.T) {
	a := []client.GrantInput{{Role: "viewer", Domain: "*"}, {Role: "admin", Domain: "spc_1"}}
	b := []client.GrantInput{{Role: "admin", Domain: "spc_1"}, {Role: "viewer", Domain: "*"}}
	if !sameGrants(a, b) || sameGrants(a, b[:1]) || !sameGrants(nil, []client.GrantInput{}) {
		t.Fatal("sameGrants")
	}
}

// The anchor of an unconfigured interval schedule is kept while kind and interval stay, unknown otherwise.
func TestAnchorPlanModifier(t *testing.T) {
	ctx := context.Background()
	var sr resource.SchemaResponse
	newScheduleResource().Schema(ctx, resource.SchemaRequest{}, &sr)
	typ := sr.Schema.Type().TerraformType(ctx).(tftypes.Object)
	obj := func(kind string, interval int64, anchor any) tftypes.Value {
		vals := map[string]tftypes.Value{}
		for name, at := range typ.AttributeTypes {
			vals[name] = tftypes.NewValue(at, nil)
		}
		vals["kind"] = tftypes.NewValue(tftypes.String, kind)
		vals["interval_seconds"] = tftypes.NewValue(tftypes.Number, interval)
		vals["anchor"] = tftypes.NewValue(tftypes.String, anchor)
		return tftypes.NewValue(typ, vals)
	}
	run := func(planKind string, planInterval int64) types.String {
		state := obj("interval", 3600, "2026-01-01T00:00:00Z")
		plan := obj(planKind, planInterval, tftypes.UnknownValue)
		req := planmodifier.StringRequest{Path: path.Root("anchor"), ConfigValue: types.StringNull(), StateValue: types.StringValue("2026-01-01T00:00:00Z"),
			PlanValue: types.StringUnknown(), State: tfsdk.State{Schema: sr.Schema, Raw: state}, Plan: tfsdk.Plan{Schema: sr.Schema, Raw: plan}}
		resp := &planmodifier.StringResponse{PlanValue: req.PlanValue}
		anchorPlanModifier{}.PlanModifyString(ctx, req, resp)
		if resp.Diagnostics.HasError() {
			t.Fatal(resp.Diagnostics)
		}
		return resp.PlanValue
	}
	if got := run("interval", 3600); got.ValueString() != "2026-01-01T00:00:00Z" {
		t.Fatalf("unchanged timing: %v", got)
	}
	if got := run("interval", 7200); !got.IsUnknown() {
		t.Fatalf("changed interval: %v", got)
	}
	if got := run("cron", 0); !got.IsUnknown() {
		t.Fatalf("changed kind: %v", got)
	}
}

// Phase-2 descriptions name what is never managed.
func TestPhase2NeverManagesSecrets(t *testing.T) {
	ctx := context.Background()
	for _, f := range []func() resource.Resource{newUserResource, newUserIdentityResource} {
		var sr resource.SchemaResponse
		f().Schema(ctx, resource.SchemaRequest{}, &sr)
		for name := range sr.Schema.Attributes {
			if strings.Contains(name, "password") || strings.Contains(name, "token") || strings.Contains(name, "invite") {
				t.Errorf("%s manages a one-time secret", name)
			}
		}
	}
}
