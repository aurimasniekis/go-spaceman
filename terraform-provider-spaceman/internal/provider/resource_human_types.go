package provider

import (
	"context"
	"encoding/json"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/aurimasniekis/go-spaceman/terraform-provider-spaceman/internal/client"
)

var priorities = []string{"low", "normal", "high", "urgent"}

// jsonValue returns the value to store for JSON the server returned: the prior value when semantically equal (key
// order and whitespace never cause a diff), else the server's canonical form.
func jsonValue(prior types.String, server json.RawMessage) types.String {
	s := string(server)
	if len(server) == 0 || s == "null" {
		s = ""
	}
	if !prior.IsNull() && !prior.IsUnknown() && jsonEqual(prior.ValueString(), s) {
		return prior
	}
	if c, err := canonicalJSON(s); err == nil {
		return types.StringValue(c)
	}
	return types.StringValue(s)
}

func rawJSON(v types.String) json.RawMessage {
	s := strOrEmpty(v)
	if s == "" {
		return nil
	}
	return json.RawMessage(s)
}

// ---- human-action types ----

type humanTypeResource struct{ base }

func newHumanActionTypeResource() resource.Resource { return &humanTypeResource{} }

type humanTypeModel struct {
	ID                   types.String `tfsdk:"id"`
	Slug                 types.String `tfsdk:"slug"`
	Name                 types.String `tfsdk:"name"`
	Description          types.String `tfsdk:"description"`
	RequestSchema        types.String `tfsdk:"request_schema"`
	ResponseSchema       types.String `tfsdk:"response_schema"`
	UI                   types.String `tfsdk:"ui"`
	Outcomes             types.List   `tfsdk:"outcomes"`
	DefaultPriority      types.String `tfsdk:"default_priority"`
	DefaultExpiryMinutes types.Int64  `tfsdk:"default_expiry_minutes"`
	Notes                types.String `tfsdk:"notes"`
	LatestVersion        types.Int64  `tfsdk:"latest_version"`
	LatestVersionID      types.String `tfsdk:"latest_version_id"`
}

func (r *humanTypeResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_human_action_type"
}

func (r *humanTypeResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A custom human-action type (`/human-types`) with versioned request and response schemas. The content attributes describe " +
			"the latest published version; changing them publishes the next version (outstanding actions keep theirs). Requests and responses " +
			"are runtime state and are never managed. SpaceMan has no API to rename or delete a type: identifying fields are fixed and destroying " +
			"the resource only removes it from Terraform's state.",
		Attributes: map[string]schema.Attribute{
			"id":              idAttr("The type ID."),
			"slug":            fixedString("Unique identifier. Fixed after creation.", "A human-action type's slug"),
			"name":            fixedString("Display name. Fixed after creation.", "A human-action type's name"),
			"description":     fixedOptString("Description. Fixed after creation.", "A human-action type's description"),
			"request_schema":  optString("JSON Schema 2020-12 of the request (root `{\"type\": \"object\"}`), as JSON (use `jsonencode`)."),
			"response_schema": optString("JSON Schema 2020-12 of the response data, as JSON."),
			"ui":              optString("Safe form metadata (`{order, fields: {field: {label, help, widget, placeholder}}}`), as JSON."),
			"outcomes":        optList("How a person can finish it, e.g. `[\"done\", \"declined\"]`."),
			"default_priority": optString("`low`, `normal`, `high` or `urgent`.",
				stringvalidator.OneOf(priorities...)),
			"default_expiry_minutes": optInt64("Default expiry in minutes (0: none)."),
			"notes":                  defaultEmpty("Notes recorded with the published version."),
			"latest_version":         schema.Int64Attribute{Computed: true, MarkdownDescription: "Number of the latest published version."},
			"latest_version_id":      schema.StringAttribute{Computed: true, MarkdownDescription: "ID of the latest published version (use it to pin)."},
		},
	}
}

func (r *humanTypeResource) toModel(t *client.HumanType, m *humanTypeModel) {
	m.ID = types.StringValue(t.ID)
	m.Slug = types.StringValue(t.Slug)
	m.Name = types.StringValue(t.Name)
	m.Description = types.StringValue(t.Description)
	lp := t.LatestPublished()
	if lp == nil {
		lp = &client.HumanTypeVersion{}
	}
	m.RequestSchema = jsonValue(m.RequestSchema, lp.RequestSchema)
	m.ResponseSchema = jsonValue(m.ResponseSchema, lp.ResponseSchema)
	m.UI = jsonValue(m.UI, lp.UI)
	m.Outcomes = listValue(lp.Outcomes)
	m.DefaultPriority = types.StringValue(lp.DefaultPriority)
	m.DefaultExpiryMinutes = types.Int64Value(lp.DefaultExpiryMinutes)
	m.Notes = types.StringValue(lp.Notes)
	m.LatestVersion = types.Int64Value(lp.Number)
	m.LatestVersionID = types.StringValue(lp.ID)
}

func (r *humanTypeResource) draft(ctx context.Context, m *humanTypeModel, diags *diagnostics) client.HumanTypeDraftInput {
	return client.HumanTypeDraftInput{RequestSchema: rawJSON(m.RequestSchema), ResponseSchema: rawJSON(m.ResponseSchema), UI: rawJSON(m.UI),
		Outcomes: stringList(ctx, m.Outcomes, diags), DefaultPriority: strOrEmpty(m.DefaultPriority), DefaultExpiryMinutes: int64OrZero(m.DefaultExpiryMinutes),
		Notes: strOrEmpty(m.Notes)}
}

func (r *humanTypeResource) publish(ctx context.Context, id string, in client.HumanTypeDraftInput, diags *diagnostics) *client.HumanType {
	t, err := r.c.GetHumanType(ctx, id)
	if err != nil {
		addAPIError(diags, "Could not read the human-action type", err)
		return nil
	}
	var dv int64
	if d := t.Draft(); d != nil {
		dv = d.Version
		if t.Latest > 0 { // the create's own first draft is not someone else's
			draftReplacedWarning(diags, "human-action type", d.Version)
		}
	}
	if t, err = r.c.SaveHumanTypeDraft(ctx, id, dv, in); err != nil {
		addAPIError(diags, "Could not save the human-action type's draft", err)
		return nil
	}
	d := t.Draft()
	if d == nil {
		diags.AddError("Could not publish the human-action type", "the saved draft was not found")
		return nil
	}
	if t, err = r.c.PublishHumanType(ctx, id, d.Version); err != nil {
		addAPIError(diags, "Could not publish the human-action type", err)
		return nil
	}
	return t
}

func (r *humanTypeResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var m humanTypeModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	in := r.draft(ctx, &m, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	t, err := r.c.CreateHumanType(ctx, client.CreateHumanTypeInput{Slug: m.Slug.ValueString(), Name: m.Name.ValueString(),
		Description: strOrEmpty(m.Description), HumanTypeDraftInput: in})
	if err != nil {
		addAPIError(&resp.Diagnostics, "Could not create the human-action type", err)
		return
	}
	d := t.Draft()
	if d == nil {
		resp.Diagnostics.AddError("Could not publish the human-action type", "the created type has no draft")
		return
	}
	id := t.ID
	if t, err = r.c.PublishHumanType(ctx, id, d.Version); err != nil {
		// Record the type (tainted) instead of orphaning it.
		resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), id)...)
		addAPIError(&resp.Diagnostics, "Could not publish the human-action type", err)
		return
	}
	r.toModel(t, &m)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *humanTypeResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var m humanTypeModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	t, err := r.c.GetHumanType(ctx, m.ID.ValueString())
	if client.IsNotFound(err) {
		resp.State.RemoveResource(ctx)
		return
	} else if err != nil {
		addAPIError(&resp.Diagnostics, "Could not read the human-action type", err)
		return
	}
	r.toModel(t, &m)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *humanTypeResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var m humanTypeModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	in := r.draft(ctx, &m, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	t := r.publish(ctx, m.ID.ValueString(), in, &resp.Diagnostics)
	if t == nil {
		return
	}
	r.toModel(t, &m)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *humanTypeResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var m humanTypeModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	noDeleteWarning(&resp.Diagnostics, "human-action type", m.ID.ValueString())
}

func (r *humanTypeResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	importPassthrough(ctx, req, resp)
}

// ---- enabling a type in a space ----

type spaceHumanTypeResource struct{ base }

func newSpaceHumanActionTypeResource() resource.Resource { return &spaceHumanTypeResource{} }

type spaceHumanTypeModel struct {
	ID                   types.String `tfsdk:"id"`
	SpaceID              types.String `tfsdk:"space_id"`
	TypeID               types.String `tfsdk:"type_id"`
	VersionPin           types.String `tfsdk:"version_pin"`
	Label                types.String `tfsdk:"label"`
	DefaultPriority      types.String `tfsdk:"default_priority"`
	DefaultExpiryMinutes types.Int64  `tfsdk:"default_expiry_minutes"`
	VersionID            types.String `tfsdk:"version_id"`
	VersionNumber        types.Int64  `tfsdk:"version_number"`
}

func (r *spaceHumanTypeResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_space_human_action_type"
}

func (r *spaceHumanTypeResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Enables a human-action type in a space (`PUT/DELETE /spaces/{id}/human-types/{type}`), following its latest version or " +
			"pinned to one, optionally as a variant with its own label and defaults. Destroying it stops new actions of the type in the space; " +
			"outstanding actions are unaffected.",
		Attributes: map[string]schema.Attribute{
			"id":          idAttr("`<space_id>/<type_id>`."),
			"space_id":    requiredReplace("The space."),
			"type_id":     requiredReplace("The type (`hty_…` ID)."),
			"version_pin": defaultEmpty("A published version ID to pin; empty follows the latest version."),
			"label": schema.StringAttribute{Optional: true,
				MarkdownDescription: "The space's name for the type (a variant). Left out, the type's name is used."},
			"default_priority": schema.StringAttribute{Optional: true, Validators: []validatorString{stringvalidator.OneOf(priorities...)},
				MarkdownDescription: "Overrides the version's default priority in this space."},
			"default_expiry_minutes": schema.Int64Attribute{Optional: true,
				MarkdownDescription: "Overrides the version's default expiry in this space (minutes)."},
			"version_id":     schema.StringAttribute{Computed: true, MarkdownDescription: "The version the space uses now."},
			"version_number": schema.Int64Attribute{Computed: true, MarkdownDescription: "Its number."},
		},
	}
}

func (r *spaceHumanTypeResource) apply(ctx context.Context, m *spaceHumanTypeModel, diags *diagnostics) bool {
	err := r.c.EnableSpaceHumanType(ctx, m.SpaceID.ValueString(), m.TypeID.ValueString(), client.EnableHumanTypeInput{VersionPin: strOrEmpty(m.VersionPin),
		Label: strOrEmpty(m.Label), DefaultPriority: strOrEmpty(m.DefaultPriority), DefaultExpiryMinutes: int64OrZero(m.DefaultExpiryMinutes)})
	if err != nil {
		addAPIError(diags, "Could not enable the human-action type in the space", err)
		return false
	}
	st, err := r.c.GetSpaceHumanType(ctx, m.SpaceID.ValueString(), m.TypeID.ValueString())
	if err != nil {
		addAPIError(diags, "Could not read the space's human-action types", err)
		return false
	}
	r.toModel(st, m)
	return true
}

// toModel records what the space uses. label and the defaults are overrides: the server reports effective values
// (falling back to the type's), so they are read back only when the configuration sets them.
func (r *spaceHumanTypeResource) toModel(st *client.SpaceHumanType, m *spaceHumanTypeModel) {
	m.ID = types.StringValue(m.SpaceID.ValueString() + "/" + st.TypeID)
	m.TypeID = types.StringValue(st.TypeID)
	if st.Pinned {
		m.VersionPin = types.StringValue(st.VersionID)
	} else {
		m.VersionPin = types.StringValue("")
	}
	if !m.Label.IsNull() {
		m.Label = types.StringValue(st.Label)
	}
	if !m.DefaultPriority.IsNull() {
		m.DefaultPriority = types.StringValue(st.DefaultPriority)
	}
	if !m.DefaultExpiryMinutes.IsNull() {
		m.DefaultExpiryMinutes = types.Int64Value(st.DefaultExpiryMinutes)
	}
	m.VersionID = types.StringValue(st.VersionID)
	m.VersionNumber = types.Int64Value(st.Number)
}

func (r *spaceHumanTypeResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var m spaceHumanTypeModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if _, err := r.c.GetSpaceHumanType(ctx, m.SpaceID.ValueString(), m.TypeID.ValueString()); err == nil {
		resp.Diagnostics.AddError("The type is already enabled in the space",
			"Import it with `terraform import spaceman_space_human_action_type.<name> "+m.SpaceID.ValueString()+"/"+m.TypeID.ValueString()+"` to manage it.")
		return
	} else if !client.IsNotFound(err) {
		addAPIError(&resp.Diagnostics, "Could not read the space's human-action types", err)
		return
	}
	if !r.apply(ctx, &m, &resp.Diagnostics) {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *spaceHumanTypeResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var m spaceHumanTypeModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	st, err := r.c.GetSpaceHumanType(ctx, m.SpaceID.ValueString(), m.TypeID.ValueString())
	if client.IsNotFound(err) {
		resp.State.RemoveResource(ctx)
		return
	} else if err != nil {
		addAPIError(&resp.Diagnostics, "Could not read the space's human-action types", err)
		return
	}
	r.toModel(st, &m)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *spaceHumanTypeResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var m spaceHumanTypeModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() || !r.apply(ctx, &m, &resp.Diagnostics) {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *spaceHumanTypeResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var m spaceHumanTypeModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.c.DisableSpaceHumanType(ctx, m.SpaceID.ValueString(), m.TypeID.ValueString()); err != nil {
		addAPIError(&resp.Diagnostics, "Could not disable the human-action type in the space", err)
	}
}

func (r *spaceHumanTypeResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	p := splitImportID(req.ID, 2, "<space_id>/<type_id>", &resp.Diagnostics)
	if p == nil {
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("space_id"), p[0])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("type_id"), p[1])...)
}
