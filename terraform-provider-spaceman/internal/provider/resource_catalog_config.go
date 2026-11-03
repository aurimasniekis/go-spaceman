package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/aurimasniekis/go-spaceman/terraform-provider-spaceman/internal/client"
)

// ---- harness profiles ----

type harnessProfileResource struct{ base }

func newHarnessProfileResource() resource.Resource { return &harnessProfileResource{} }

type harnessProfileModel struct {
	ID          types.String `tfsdk:"id"`
	Slug        types.String `tfsdk:"slug"`
	Name        types.String `tfsdk:"name"`
	Harness     types.String `tfsdk:"harness"`
	Model       types.String `tfsdk:"model"`
	Settings    types.Map    `tfsdk:"settings"`
	Description types.String `tfsdk:"description"`
	Revision    types.Int64  `tfsdk:"revision"`
}

func (r *harnessProfileResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_harness_profile"
}

func (r *harnessProfileResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A harness profile (`GET /harness-profiles`, `PUT /harness-profiles/{id}`): harness, model and settings a role template " +
			"starts from. An update bumps the revision and applies to later launches. SpaceMan has no API to delete a profile: destroying " +
			"the resource only removes it from Terraform's state.",
		Attributes: map[string]schema.Attribute{
			"id": idAttr("The profile ID."),
			"slug": schema.StringAttribute{Required: true, MarkdownDescription: "Unique identifier. Fixed after creation.",
				PlanModifiers: []planmodifier.String{fixedAfterCreate("A harness profile's slug")}},
			"name": schema.StringAttribute{Required: true, MarkdownDescription: "Display name."},
			"harness": schema.StringAttribute{Required: true, MarkdownDescription: "`claude-code` or `codex`. Fixed after creation.",
				Validators:    []validatorString{stringvalidator.OneOf("claude-code", "codex")},
				PlanModifiers: []planmodifier.String{fixedAfterCreate("A harness profile's harness")}},
			"model":       optString("Model."),
			"settings":    optMap("Harness settings (string values)."),
			"description": optString("Description."),
			"revision":    schema.Int64Attribute{Computed: true, MarkdownDescription: "Increments on every change."},
		},
	}
}

func (r *harnessProfileResource) toModel(p *client.Profile, m *harnessProfileModel) {
	m.ID = types.StringValue(p.ID)
	m.Slug = types.StringValue(p.Slug)
	m.Name = types.StringValue(p.Name)
	m.Harness = types.StringValue(p.Harness)
	m.Model = types.StringValue(p.Model)
	m.Settings = mapValue(p.Settings)
	m.Description = types.StringValue(p.Description)
	m.Revision = types.Int64Value(p.Revision)
}

func (r *harnessProfileResource) save(ctx context.Context, id string, version int64, m *harnessProfileModel, diags *diagnostics) *client.Profile {
	in := client.ProfileInput{Slug: m.Slug.ValueString(), Name: m.Name.ValueString(), Harness: m.Harness.ValueString(), Model: strOrEmpty(m.Model),
		Settings: stringMap(ctx, m.Settings, diags), Description: strOrEmpty(m.Description)}
	if diags.HasError() {
		return nil
	}
	p, err := r.c.SaveProfile(ctx, id, version, in)
	if err != nil {
		addAPIError(diags, "Could not save the harness profile", err)
		return nil
	}
	return p
}

func (r *harnessProfileResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var m harnessProfileModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	p := r.save(ctx, "", 0, &m, &resp.Diagnostics)
	if p == nil {
		return
	}
	r.toModel(p, &m)
	setVersion(ctx, resp.Private, p.Version, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *harnessProfileResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var m harnessProfileModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	p, err := r.c.GetProfile(ctx, m.ID.ValueString())
	if client.IsNotFound(err) {
		resp.State.RemoveResource(ctx)
		return
	} else if err != nil {
		addAPIError(&resp.Diagnostics, "Could not read the harness profile", err)
		return
	}
	r.toModel(p, &m)
	setVersion(ctx, resp.Private, p.Version, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *harnessProfileResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var m, prior harnessProfileModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &prior)...)
	if resp.Diagnostics.HasError() {
		return
	}
	v := getVersion(ctx, req.Private, &resp.Diagnostics)
	if v == 0 {
		cur, err := r.c.GetProfile(ctx, prior.ID.ValueString())
		if err != nil {
			addAPIError(&resp.Diagnostics, "Could not read the harness profile", err)
			return
		}
		v = cur.Version
	}
	p := r.save(ctx, prior.ID.ValueString(), v, &m, &resp.Diagnostics)
	if p == nil {
		return
	}
	r.toModel(p, &m)
	setVersion(ctx, resp.Private, p.Version, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *harnessProfileResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var m harnessProfileModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	noDeleteWarning(&resp.Diagnostics, "harness profile", m.ID.ValueString())
}

func (r *harnessProfileResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	importPassthrough(ctx, req, resp)
}

// ---- role templates ----

type roleTemplateResource struct{ base }

func newRoleTemplateResource() resource.Resource { return &roleTemplateResource{} }

type roleTemplateModel struct {
	ID                types.String `tfsdk:"id"`
	Slug              types.String `tfsdk:"slug"`
	Name              types.String `tfsdk:"name"`
	Kind              types.String `tfsdk:"kind"`
	Description       types.String `tfsdk:"description"`
	ProfileID         types.String `tfsdk:"profile_id"`
	PromptID          types.String `tfsdk:"prompt_id"`
	PromptPin         types.String `tfsdk:"prompt_pin"`
	MemoryConventions types.String `tfsdk:"memory_conventions"`
	Settings          types.Map    `tfsdk:"settings"`
	Skills            types.List   `tfsdk:"skills"`
	Revision          types.Int64  `tfsdk:"revision"`
}

func (r *roleTemplateResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_role_template"
}

func (r *roleTemplateResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A role template (`GET /role-templates`, `PUT /role-templates/{id}`): the kind of run, its profile, prompt binding and skills. " +
			"Spaces configure roles from templates. SpaceMan has no API to delete a template: destroying the resource only removes it from Terraform's state.",
		Attributes: map[string]schema.Attribute{
			"id": idAttr("The template ID."),
			"slug": schema.StringAttribute{Required: true, MarkdownDescription: "Unique identifier. Fixed after creation.",
				PlanModifiers: []planmodifier.String{fixedAfterCreate("A role template's slug")}},
			"name": schema.StringAttribute{Required: true, MarkdownDescription: "Display name."},
			"kind": schema.StringAttribute{Required: true, MarkdownDescription: "`control`, `worker`, `kb`, `dreamer` or `diagnostic`. Fixed after creation.",
				Validators:    []validatorString{stringvalidator.OneOf("control", "worker", "kb", "dreamer", "diagnostic")},
				PlanModifiers: []planmodifier.String{fixedAfterCreate("A role template's kind")}},
			"description":        optString("Description."),
			"profile_id":         optString("The harness profile."),
			"prompt_id":          optString("The prompt (prompt binding)."),
			"prompt_pin":         optString("A published version of `prompt_id`; empty follows the latest version."),
			"memory_conventions": optString("How the role keeps its memory."),
			"settings":           optMap("Harness settings overriding the profile's."),
			"skills":             skillsAttr("Skills the role gets."),
			"revision":           schema.Int64Attribute{Computed: true, MarkdownDescription: "Increments on every change."},
		},
	}
}

func (r *roleTemplateResource) toModel(t *client.RoleTemplate, m *roleTemplateModel) {
	m.ID = types.StringValue(t.ID)
	m.Slug = types.StringValue(t.Slug)
	m.Name = types.StringValue(t.Name)
	m.Kind = types.StringValue(t.Kind)
	m.Description = types.StringValue(t.Description)
	m.ProfileID = types.StringValue(t.ProfileID)
	m.PromptID = types.StringValue(t.PromptID)
	m.PromptPin = types.StringValue(t.PromptPin)
	m.MemoryConventions = types.StringValue(t.MemoryConventions)
	m.Settings = mapValue(t.Settings)
	m.Skills = skillRefsValue(t.Skills)
	m.Revision = types.Int64Value(t.Revision)
}

func (r *roleTemplateResource) save(ctx context.Context, id string, version int64, m *roleTemplateModel, diags *diagnostics) *client.RoleTemplate {
	in := client.RoleTemplateInput{Slug: m.Slug.ValueString(), Name: m.Name.ValueString(), Kind: m.Kind.ValueString(), Description: strOrEmpty(m.Description),
		ProfileID: strOrEmpty(m.ProfileID), PromptID: strOrEmpty(m.PromptID), PromptPin: strOrEmpty(m.PromptPin),
		MemoryConventions: strOrEmpty(m.MemoryConventions), Settings: stringMap(ctx, m.Settings, diags), Skills: skillRefs(ctx, m.Skills, diags)}
	if diags.HasError() {
		return nil
	}
	t, err := r.c.SaveRoleTemplate(ctx, id, version, in)
	if err != nil {
		addAPIError(diags, "Could not save the role template", err)
		return nil
	}
	return t
}

func (r *roleTemplateResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var m roleTemplateModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	t := r.save(ctx, "", 0, &m, &resp.Diagnostics)
	if t == nil {
		return
	}
	r.toModel(t, &m)
	setVersion(ctx, resp.Private, t.Version, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *roleTemplateResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var m roleTemplateModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	t, err := r.c.GetRoleTemplate(ctx, m.ID.ValueString())
	if client.IsNotFound(err) {
		resp.State.RemoveResource(ctx)
		return
	} else if err != nil {
		addAPIError(&resp.Diagnostics, "Could not read the role template", err)
		return
	}
	r.toModel(t, &m)
	setVersion(ctx, resp.Private, t.Version, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *roleTemplateResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var m, prior roleTemplateModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &prior)...)
	if resp.Diagnostics.HasError() {
		return
	}
	v := getVersion(ctx, req.Private, &resp.Diagnostics)
	if v == 0 {
		cur, err := r.c.GetRoleTemplate(ctx, prior.ID.ValueString())
		if err != nil {
			addAPIError(&resp.Diagnostics, "Could not read the role template", err)
			return
		}
		v = cur.Version
	}
	t := r.save(ctx, prior.ID.ValueString(), v, &m, &resp.Diagnostics)
	if t == nil {
		return
	}
	r.toModel(t, &m)
	setVersion(ctx, resp.Private, t.Version, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *roleTemplateResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var m roleTemplateModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	noDeleteWarning(&resp.Diagnostics, "role template", m.ID.ValueString())
}

func (r *roleTemplateResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	importPassthrough(ctx, req, resp)
}
