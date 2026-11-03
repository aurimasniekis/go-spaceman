package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/listplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/mapplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/aurimasniekis/go-spaceman/terraform-provider-spaceman/internal/client"
)

func requiredReplace(desc string) schema.StringAttribute {
	return schema.StringAttribute{Required: true, MarkdownDescription: desc,
		PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()}}
}

func optMap(desc string) schema.MapAttribute {
	return schema.MapAttribute{Optional: true, Computed: true, ElementType: types.StringType, MarkdownDescription: desc,
		PlanModifiers: []planmodifier.Map{mapplanmodifier.UseStateForUnknown()}}
}

func optList(desc string) schema.ListAttribute {
	return schema.ListAttribute{Optional: true, Computed: true, ElementType: types.StringType, MarkdownDescription: desc,
		PlanModifiers: []planmodifier.List{listplanmodifier.UseStateForUnknown()}}
}

func skillsAttr(desc string) schema.ListNestedAttribute {
	return schema.ListNestedAttribute{Optional: true, Computed: true, MarkdownDescription: desc,
		PlanModifiers: []planmodifier.List{listplanmodifier.UseStateForUnknown()},
		NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
			"skill_id": schema.StringAttribute{Required: true, MarkdownDescription: "The skill ID (`skl_…`)."},
			"pin": schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "A published version ID; empty follows the latest version.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
		}}}
}

type spaceRoleResource struct{ base }

func newSpaceRoleResource() resource.Resource { return &spaceRoleResource{} }

type spaceRoleModel struct {
	ID           types.String `tfsdk:"id"`
	SpaceID      types.String `tfsdk:"space_id"`
	Name         types.String `tfsdk:"name"`
	TemplateID   types.String `tfsdk:"template_id"`
	ProfileID    types.String `tfsdk:"profile_id"`
	PromptID     types.String `tfsdk:"prompt_id"`
	PromptPin    types.String `tfsdk:"prompt_pin"`
	Model        types.String `tfsdk:"model"`
	Settings     types.Map    `tfsdk:"settings"`
	Instructions types.String `tfsdk:"instructions"`
	Skills       types.List   `tfsdk:"skills"`
	Revision     types.Int64  `tfsdk:"revision"`
}

func (r *spaceRoleResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_space_role"
}

func (r *spaceRoleResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A role configured in a space (`PUT/DELETE /spaces/{id}/roles/{name}`): a role template with space-specific overrides. " +
			"Non-empty fields override the template's; changes apply to later launches.",
		Attributes: map[string]schema.Attribute{
			"id":           idAttr("`<space_id>/<name>`."),
			"space_id":     requiredReplace("The space."),
			"name":         requiredReplace("Role name, lowercase (e.g. `implementor-backend`)."),
			"template_id":  schema.StringAttribute{Required: true, MarkdownDescription: "The role template (`rtp_…`)."},
			"profile_id":   optString("Harness profile overriding the template's."),
			"prompt_id":    optString("Prompt overriding the template's."),
			"prompt_pin":   optString("A published version of `prompt_id`; empty follows the latest version."),
			"model":        optString("Model overriding the profile's."),
			"settings":     optMap("Harness settings overriding the template's."),
			"instructions": optString("Space-specific text appended after the role's prompt."),
			"skills":       skillsAttr("Skills added to the template's; a pin here wins."),
			"revision":     schema.Int64Attribute{Computed: true, MarkdownDescription: "Increments on every change of the role."},
		},
	}
}

func (r *spaceRoleResource) toModel(sr *client.SpaceRole, m *spaceRoleModel) {
	m.ID = types.StringValue(sr.SpaceID + "/" + sr.Name)
	m.SpaceID = types.StringValue(sr.SpaceID)
	m.Name = types.StringValue(sr.Name)
	m.TemplateID = types.StringValue(sr.TemplateID)
	m.ProfileID = types.StringValue(sr.ProfileID)
	m.PromptID = types.StringValue(sr.PromptID)
	m.PromptPin = types.StringValue(sr.PromptPin)
	m.Model = types.StringValue(sr.Model)
	m.Settings = mapValue(sr.Settings)
	m.Instructions = types.StringValue(sr.Instructions)
	m.Skills = skillRefsValue(sr.Skills)
	m.Revision = types.Int64Value(sr.Revision)
}

func (r *spaceRoleResource) input(ctx context.Context, m *spaceRoleModel, d *diag.Diagnostics) client.SpaceRoleInput {
	return client.SpaceRoleInput{TemplateID: m.TemplateID.ValueString(), ProfileID: strOrEmpty(m.ProfileID), PromptID: strOrEmpty(m.PromptID),
		PromptPin: strOrEmpty(m.PromptPin), Model: strOrEmpty(m.Model), Settings: stringMap(ctx, m.Settings, d), Instructions: strOrEmpty(m.Instructions),
		Skills: skillRefs(ctx, m.Skills, d)}
}

func (r *spaceRoleResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var m spaceRoleModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// The API's save is an upsert: refuse to take over a role someone else configured (import it instead).
	if _, err := r.c.GetSpaceRole(ctx, m.SpaceID.ValueString(), m.Name.ValueString()); err == nil {
		resp.Diagnostics.AddError("The space role already exists",
			"Role "+m.Name.ValueString()+" is already configured in space "+m.SpaceID.ValueString()+". Import it with "+
				"`terraform import spaceman_space_role.<name> "+m.SpaceID.ValueString()+"/"+m.Name.ValueString()+"` to manage it.")
		return
	} else if !client.IsNotFound(err) {
		addAPIError(&resp.Diagnostics, "Could not read the space's roles", err)
		return
	}
	in := r.input(ctx, &m, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	sr, err := r.c.SaveSpaceRole(ctx, m.SpaceID.ValueString(), m.Name.ValueString(), in)
	if err != nil {
		addAPIError(&resp.Diagnostics, "Could not configure the space role", err)
		return
	}
	r.toModel(sr, &m)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *spaceRoleResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var m spaceRoleModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	sr, err := r.c.GetSpaceRole(ctx, m.SpaceID.ValueString(), m.Name.ValueString())
	if client.IsNotFound(err) {
		resp.State.RemoveResource(ctx)
		return
	} else if err != nil {
		addAPIError(&resp.Diagnostics, "Could not read the space role", err)
		return
	}
	r.toModel(sr, &m)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *spaceRoleResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var m spaceRoleModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	in := r.input(ctx, &m, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	sr, err := r.c.SaveSpaceRole(ctx, m.SpaceID.ValueString(), m.Name.ValueString(), in)
	if err != nil {
		addAPIError(&resp.Diagnostics, "Could not update the space role", err)
		return
	}
	r.toModel(sr, &m)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *spaceRoleResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var m spaceRoleModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.c.DeleteSpaceRole(ctx, m.SpaceID.ValueString(), m.Name.ValueString()); err != nil {
		addAPIError(&resp.Diagnostics, "Could not remove the space role", err)
	}
}

func (r *spaceRoleResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	p := splitImportID(req.ID, 2, "<space_id>/<role_name>", &resp.Diagnostics)
	if p == nil {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &spaceRoleModel{ID: types.StringValue(req.ID), SpaceID: types.StringValue(p[0]), Name: types.StringValue(p[1]),
		TemplateID: types.StringNull(), ProfileID: types.StringNull(), PromptID: types.StringNull(), PromptPin: types.StringNull(), Model: types.StringNull(),
		Settings: types.MapNull(types.StringType), Instructions: types.StringNull(), Skills: types.ListNull(skillRefType), Revision: types.Int64Null()})...)
}
