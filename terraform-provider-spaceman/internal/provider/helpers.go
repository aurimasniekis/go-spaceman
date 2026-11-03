package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/defaults"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/mapplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"

	"github.com/aurimasniekis/go-spaceman/terraform-provider-spaceman/internal/client"
)

type validatorString = validator.String

type diagnostics = diag.Diagnostics

// ---- configuration ----

// configureClient extracts the provider's client from ProviderData (nil before the provider is configured).
func configureClient(data any, diags *diag.Diagnostics) *client.Client {
	if data == nil {
		return nil
	}
	c, ok := data.(*client.Client)
	if !ok {
		diags.AddError("Unexpected provider data", fmt.Sprintf("expected *client.Client, got %T", data))
		return nil
	}
	return c
}

// ---- diagnostics ----

// addAPIError reports a failed SpaceMan call. Problem details (code, detail, field errors) are included; request and
// response bodies never are.
func addAPIError(diags *diag.Diagnostics, summary string, err error) {
	detail := err.Error()
	var ae *client.APIError
	if errors.As(err, &ae) {
		switch {
		case ae.Status == 409 && (ae.Code == "conflict" || ae.Code == "precondition_failed" || ae.Code == "precondition"):
			detail += "\n\nThe object changed in SpaceMan after Terraform last read it (for example through the web UI), or a " +
				"precondition of the operation does not hold. Run `terraform plan` again to see the current state."
		case ae.Status == 401:
			detail += "\n\nCheck the provider's token (SPACEMAN_TOKEN): it must be a valid API token or the root token."
		case ae.Status == 403:
			detail += "\n\nThe token's principal lacks the permission this operation needs."
		}
	}
	diags.AddError(summary, detail)
}

// ---- private state: the server version used for If-Match ----

const versionKey = "version"

type privateReader interface {
	GetKey(ctx context.Context, key string) ([]byte, diag.Diagnostics)
}

type privateWriter interface {
	SetKey(ctx context.Context, key string, value []byte) diag.Diagnostics
}

// getVersion returns the stored server version (0 when none is stored).
func getVersion(ctx context.Context, p privateReader, diags *diag.Diagnostics) int64 {
	if p == nil {
		return 0
	}
	b, d := p.GetKey(ctx, versionKey)
	diags.Append(d...)
	if len(b) == 0 {
		return 0
	}
	n, err := strconv.ParseInt(string(b), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// setVersion stores the server version as a JSON number (private state must be JSON).
func setVersion(ctx context.Context, p privateWriter, v int64, diags *diag.Diagnostics) {
	if p == nil || v <= 0 {
		return
	}
	diags.Append(p.SetKey(ctx, versionKey, []byte(strconv.FormatInt(v, 10)))...)
}

// ---- value conversions ----

func strOrEmpty(v types.String) string {
	if v.IsNull() || v.IsUnknown() {
		return ""
	}
	return v.ValueString()
}

func int64OrZero(v types.Int64) int64 {
	if v.IsNull() || v.IsUnknown() {
		return 0
	}
	return v.ValueInt64()
}

// boolPtr returns nil for a null or unknown value.
func boolPtr(v types.Bool) *bool {
	if v.IsNull() || v.IsUnknown() {
		return nil
	}
	b := v.ValueBool()
	return &b
}

func strPtr(v types.String) *string {
	if v.IsNull() || v.IsUnknown() {
		return nil
	}
	s := v.ValueString()
	return &s
}

// stringMap converts a map of strings (nil when null or unknown).
func stringMap(ctx context.Context, v types.Map, diags *diag.Diagnostics) map[string]string {
	if v.IsNull() || v.IsUnknown() {
		return nil
	}
	out := map[string]string{}
	diags.Append(v.ElementsAs(ctx, &out, false)...)
	return out
}

// stringList converts a list of strings (nil when null or unknown).
func stringList(ctx context.Context, v types.List, diags *diag.Diagnostics) []string {
	if v.IsNull() || v.IsUnknown() {
		return nil
	}
	out := []string{}
	diags.Append(v.ElementsAs(ctx, &out, false)...)
	return out
}

// mapValue returns a map value (an empty map for nil).
func mapValue(m map[string]string) types.Map {
	elems := map[string]attr.Value{}
	for k, v := range m {
		elems[k] = types.StringValue(v)
	}
	return types.MapValueMust(types.StringType, elems)
}

// listValue returns a list value (an empty list for nil).
func listValue(l []string) types.List {
	elems := make([]attr.Value, 0, len(l))
	for _, v := range l {
		elems = append(elems, types.StringValue(v))
	}
	return types.ListValueMust(types.StringType, elems)
}

// stringSet converts a set of strings (nil when null or unknown).
func stringSet(ctx context.Context, v types.Set, diags *diag.Diagnostics) []string {
	if v.IsNull() || v.IsUnknown() {
		return nil
	}
	out := []string{}
	diags.Append(v.ElementsAs(ctx, &out, false)...)
	return out
}

// setValue returns a set value (an empty set for nil).
func setValue(l []string) types.Set {
	elems := make([]attr.Value, 0, len(l))
	for _, v := range l {
		elems = append(elems, types.StringValue(v))
	}
	return types.SetValueMust(types.StringType, elems)
}

// ---- skill references (role templates and space roles) ----

var skillRefType = types.ObjectType{AttrTypes: map[string]attr.Type{"skill_id": types.StringType, "pin": types.StringType}}

type skillRefModel struct {
	SkillID types.String `tfsdk:"skill_id"`
	Pin     types.String `tfsdk:"pin"`
}

func skillRefs(ctx context.Context, v types.List, diags *diag.Diagnostics) []client.SkillRef {
	if v.IsNull() || v.IsUnknown() {
		return nil
	}
	var ms []skillRefModel
	diags.Append(v.ElementsAs(ctx, &ms, false)...)
	out := make([]client.SkillRef, 0, len(ms))
	for _, m := range ms {
		out = append(out, client.SkillRef{SkillID: m.SkillID.ValueString(), Pin: strOrEmpty(m.Pin)})
	}
	return out
}

func skillRefsValue(refs []client.SkillRef) types.List {
	elems := make([]attr.Value, 0, len(refs))
	for _, r := range refs {
		elems = append(elems, types.ObjectValueMust(skillRefType.AttrTypes, map[string]attr.Value{
			"skill_id": types.StringValue(r.SkillID), "pin": types.StringValue(r.Pin)}))
	}
	return types.ListValueMust(skillRefType, elems)
}

// ---- JSON attributes ----

// canonicalJSON re-encodes JSON with sorted keys and no insignificant whitespace ("" for empty input).
func canonicalJSON(s string) (string, error) {
	if strings.TrimSpace(s) == "" {
		return "", nil
	}
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return "", err
	}
	b, err := json.Marshal(v) // encoding/json sorts map keys
	return string(b), err
}

// jsonEqual reports whether two JSON documents are semantically equal (invalid JSON compares as text).
func jsonEqual(a, b string) bool {
	ca, err1 := canonicalJSON(a)
	cb, err2 := canonicalJSON(b)
	if err1 != nil || err2 != nil {
		return a == b
	}
	return ca == cb
}

// canonicalDefinition normalizes an event template definition: parsed into the API's shape, empty lists for missing
// ones, and the input schema canonicalized. Two definitions with the same canonical form are the same definition.
func canonicalDefinition(s string) (string, error) {
	var d client.TemplateDefinition
	dec := json.NewDecoder(strings.NewReader(s))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return "", fmt.Errorf("not a template definition: %w", err)
	}
	return marshalDefinition(&d)
}

func marshalDefinition(d *client.TemplateDefinition) (string, error) {
	c := *d
	if c.Variables == nil {
		c.Variables = []client.TemplateVariable{}
	}
	if c.Filters == nil {
		c.Filters = []client.TemplateFilter{}
	}
	if len(c.InputSchema) > 0 {
		cs, err := canonicalJSON(string(c.InputSchema))
		if err != nil {
			return "", fmt.Errorf("input_schema: %w", err)
		}
		if cs == "null" {
			c.InputSchema = nil
		} else {
			c.InputSchema = json.RawMessage(cs)
		}
	}
	b, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	return canonicalJSON(string(b))
}

// parseDefinition decodes a definition from configuration.
func parseDefinition(s string) (*client.TemplateDefinition, error) {
	cs, err := canonicalDefinition(s)
	if err != nil {
		return nil, err
	}
	var d client.TemplateDefinition
	if err := json.Unmarshal([]byte(cs), &d); err != nil {
		return nil, err
	}
	return &d, nil
}

// definitionValue returns the value to store for a definition the server returned: the prior value when it is the same
// definition (so formatting choices in the configuration never cause a diff), else the server's canonical JSON.
func definitionValue(prior types.String, server *client.TemplateDefinition) types.String {
	if server == nil {
		return types.StringNull()
	}
	sv, err := marshalDefinition(server)
	if err != nil {
		return prior
	}
	if !prior.IsNull() && !prior.IsUnknown() {
		if pv, err := canonicalDefinition(prior.ValueString()); err == nil && pv == sv {
			return prior
		}
	}
	return types.StringValue(sv)
}

// ---- plan modifiers ----

// immutableString refuses, at plan time, any change of an attribute that SpaceMan cannot change in place and that must
// not be replaced (a replacement would destroy data or leave an orphan the API cannot delete).
type immutableString struct {
	summary string
	detail  string
}

// refuseChange returns the modifier with the diagnostic shown when the value changes.
func refuseChange(summary, detail string) planmodifier.String {
	return immutableString{summary: summary, detail: detail}
}

func (m immutableString) Description(context.Context) string {
	return "Refuses changes after creation: " + m.summary
}

func (m immutableString) MarkdownDescription(ctx context.Context) string { return m.Description(ctx) }

func (m immutableString) PlanModifyString(_ context.Context, req planmodifier.StringRequest, resp *planmodifier.StringResponse) {
	if req.State.Raw.IsNull() || req.Plan.Raw.IsNull() { // create or destroy
		return
	}
	if req.StateValue.IsNull() || req.PlanValue.IsUnknown() {
		return
	}
	if !req.PlanValue.Equal(req.StateValue) {
		resp.Diagnostics.AddAttributeError(req.Path, m.summary,
			fmt.Sprintf("%s\n\nCurrent value: %q, configured value: %q.", m.detail, req.StateValue.ValueString(), req.PlanValue.ValueString()))
	}
}

// fixedAfterCreate is the common refusal for identifying fields the API cannot change.
func fixedAfterCreate(what string) planmodifier.String {
	return refuseChange(what+" cannot be changed",
		"SpaceMan fixes this value when the object is created and has no API to change it. Terraform does not replace the "+
			"object instead, because replacing it would lose its history (or leave the old object behind where the API "+
			"cannot delete it). Restore the previous value, or create a new resource and remove this one from the "+
			"configuration deliberately.")
}

// ---- schema shorthands ----

var stringDefaultEmpty = stringdefault.StaticString("")

func stringDefault(v string) defaults.String { return stringdefault.StaticString(v) }

func mapUseStateForUnknown() planmodifier.Map { return mapplanmodifier.UseStateForUnknown() }

var objectAsOptions = basetypes.ObjectAsOptions{}

// ---- resource helpers ----

// importPassthrough is the import of resources whose import ID is their ID.
func importPassthrough(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

// splitImportID splits "a/b" import IDs.
func splitImportID(id string, parts int, format string, diags *diag.Diagnostics) []string {
	s := strings.Split(id, "/")
	if len(s) != parts {
		diags.AddError("Invalid import ID", fmt.Sprintf("expected %s, got %q", format, id))
		return nil
	}
	for _, p := range s {
		if p == "" {
			diags.AddError("Invalid import ID", fmt.Sprintf("expected %s, got %q", format, id))
			return nil
		}
	}
	return s
}

// noDeleteWarning is the warning for catalog objects the API cannot delete: Terraform forgets them, SpaceMan keeps them.
func noDeleteWarning(diags *diag.Diagnostics, what, id string) {
	diags.AddWarning(what+" kept in SpaceMan",
		fmt.Sprintf("SpaceMan has no API to delete a %s (published versions are immutable history). Terraform removed %s from "+
			"its state only; it stays in SpaceMan's catalog.", what, id))
}
