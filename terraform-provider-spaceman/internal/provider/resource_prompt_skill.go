package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/aurimasniekis/go-spaceman/terraform-provider-spaceman/internal/client"
)

// Versioned catalog objects (prompts, skills, human-action types, event templates) publish immutable versions. The
// resource's content attributes describe the latest published version; a change publishes the next version. Reading
// compares against the latest published version only, so an unpublished draft is not drift, and an unchanged
// configuration never publishes again (no perpetual plan).

func fixedString(desc, what string) schema.StringAttribute {
	return schema.StringAttribute{Required: true, MarkdownDescription: desc, PlanModifiers: []planmodifier.String{fixedAfterCreate(what)}}
}

func fixedOptString(desc, what string) schema.StringAttribute {
	return schema.StringAttribute{Optional: true, Computed: true, Default: stringdefault.StaticString(""), MarkdownDescription: desc,
		PlanModifiers: []planmodifier.String{fixedAfterCreate(what)}}
}

func defaultEmpty(desc string) schema.StringAttribute {
	return schema.StringAttribute{Optional: true, Computed: true, Default: stringdefault.StaticString(""), MarkdownDescription: desc}
}

func draftReplacedWarning(diags *diagnostics, what string, number int64) {
	diags.AddWarning("Unpublished draft replaced",
		fmt.Sprintf("The %s had an unpublished draft (draft version %d, for example started in the web UI). Terraform replaced it with the "+
			"configured content and published it; the draft's earlier content is not kept.", what, number))
}

// ---- prompts ----

type promptResource struct{ base }

func newPromptResource() resource.Resource { return &promptResource{} }

type promptModel struct {
	ID              types.String `tfsdk:"id"`
	Slug            types.String `tfsdk:"slug"`
	Name            types.String `tfsdk:"name"`
	Kind            types.String `tfsdk:"kind"`
	Description     types.String `tfsdk:"description"`
	Body            types.String `tfsdk:"body"`
	Extends         types.String `tfsdk:"extends"`
	Notes           types.String `tfsdk:"notes"`
	LatestVersion   types.Int64  `tfsdk:"latest_version"`
	LatestVersionID types.String `tfsdk:"latest_version_id"`
	SHA256          types.String `tfsdk:"sha256"`
}

func (r *promptResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_prompt"
}

func (r *promptResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A catalog prompt (`/prompts`). `body`, `extends` and `notes` describe the latest published version; changing them " +
			"publishes the next immutable version (`PUT /prompts/{id}/draft`, `POST /prompts/{id}/publish`). Roles that follow the prompt use it " +
			"from their next launch. SpaceMan has no API to rename or delete a prompt: identifying fields are fixed and destroying the resource " +
			"only removes it from Terraform's state.",
		Attributes: map[string]schema.Attribute{
			"id":   idAttr("The prompt ID."),
			"slug": fixedString("Unique identifier. Fixed after creation.", "A prompt's slug"),
			"name": fixedString("Display name. Fixed after creation (the API has no rename).", "A prompt's name"),
			"kind": schema.StringAttribute{Required: true, MarkdownDescription: "`role`, `dream`, `kb`, `event` or `general`. Fixed after creation.",
				Validators:    []validatorString{stringvalidator.OneOf("role", "dream", "kb", "event", "general")},
				PlanModifiers: []planmodifier.String{fixedAfterCreate("A prompt's kind")}},
			"description":       fixedOptString("Description. Fixed after creation.", "A prompt's description"),
			"body":              schema.StringAttribute{Required: true, MarkdownDescription: "Markdown content of the published version."},
			"extends":           defaultEmpty("A published version (of another prompt) whose content comes first."),
			"notes":             defaultEmpty("Notes recorded with the published version."),
			"latest_version":    schema.Int64Attribute{Computed: true, MarkdownDescription: "Number of the latest published version."},
			"latest_version_id": schema.StringAttribute{Computed: true, MarkdownDescription: "ID of the latest published version (use it to pin)."},
			"sha256":            schema.StringAttribute{Computed: true, MarkdownDescription: "SHA-256 of the latest version's resolved content."},
		},
	}
}

func (r *promptResource) toModel(p *client.Prompt, m *promptModel) {
	m.ID = types.StringValue(p.ID)
	m.Slug = types.StringValue(p.Slug)
	m.Name = types.StringValue(p.Name)
	m.Kind = types.StringValue(p.Kind)
	m.Description = types.StringValue(p.Description)
	if lp := p.LatestPublished(); lp != nil {
		m.Body = types.StringValue(lp.Body)
		m.Extends = types.StringValue(lp.Extends)
		m.Notes = types.StringValue(lp.Notes)
		m.LatestVersion = types.Int64Value(lp.Number)
		m.LatestVersionID = types.StringValue(lp.ID)
		m.SHA256 = types.StringValue(lp.SHA256)
	} else {
		// Nothing published yet: an empty body plans a publish of the configured content.
		m.Body, m.Extends, m.Notes = types.StringValue(""), types.StringValue(""), types.StringValue("")
		m.LatestVersion, m.LatestVersionID, m.SHA256 = types.Int64Value(0), types.StringValue(""), types.StringValue("")
	}
}

// publish saves the configured content as the prompt's draft (replacing any draft) and publishes it.
func (r *promptResource) publish(ctx context.Context, id string, m *promptModel, diags *diagnostics) *client.Prompt {
	p, err := r.c.GetPrompt(ctx, id)
	if err != nil {
		addAPIError(diags, "Could not read the prompt", err)
		return nil
	}
	in := client.PromptDraftInput{Body: m.Body.ValueString(), Extends: strOrEmpty(m.Extends), Notes: strOrEmpty(m.Notes)}
	var dv int64
	if d := p.Draft(); d != nil {
		if d.Body != in.Body || d.Extends != in.Extends || d.Notes != in.Notes {
			draftReplacedWarning(diags, "prompt", d.Version)
		}
		dv = d.Version
	}
	d, err := r.c.SavePromptDraft(ctx, id, dv, in)
	if err != nil {
		addAPIError(diags, "Could not save the prompt's draft", err)
		return nil
	}
	if _, err := r.c.PublishPrompt(ctx, id, d.Version); err != nil {
		addAPIError(diags, "Could not publish the prompt", err)
		return nil
	}
	if p, err = r.c.GetPrompt(ctx, id); err != nil {
		addAPIError(diags, "Could not read the prompt", err)
		return nil
	}
	return p
}

func (r *promptResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var m promptModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	p, err := r.c.CreatePrompt(ctx, client.CreatePromptInput{Slug: m.Slug.ValueString(), Name: m.Name.ValueString(), Kind: m.Kind.ValueString(),
		Description: strOrEmpty(m.Description), Body: m.Body.ValueString()})
	if err != nil {
		addAPIError(&resp.Diagnostics, "Could not create the prompt", err)
		return
	}
	// Record the prompt before publishing: if publishing fails, Terraform keeps it (tainted) instead of orphaning it.
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), p.ID)...)
	if p = r.publish(ctx, p.ID, &m, &resp.Diagnostics); p == nil {
		return
	}
	r.toModel(p, &m)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *promptResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var m promptModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	p, err := r.c.GetPrompt(ctx, m.ID.ValueString())
	if client.IsNotFound(err) {
		resp.State.RemoveResource(ctx)
		return
	} else if err != nil {
		addAPIError(&resp.Diagnostics, "Could not read the prompt", err)
		return
	}
	r.toModel(p, &m)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *promptResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var m promptModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	p := r.publish(ctx, m.ID.ValueString(), &m, &resp.Diagnostics)
	if p == nil {
		return
	}
	r.toModel(p, &m)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *promptResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var m promptModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	noDeleteWarning(&resp.Diagnostics, "prompt", m.ID.ValueString())
}

func (r *promptResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	importPassthrough(ctx, req, resp)
}

// ---- skills ----

type skillResource struct{ base }

func newSkillResource() resource.Resource { return &skillResource{} }

type skillModel struct {
	ID              types.String `tfsdk:"id"`
	Slug            types.String `tfsdk:"slug"`
	Name            types.String `tfsdk:"name"`
	Description     types.String `tfsdk:"description"`
	Files           types.Map    `tfsdk:"files"`
	Notes           types.String `tfsdk:"notes"`
	LatestVersion   types.Int64  `tfsdk:"latest_version"`
	LatestVersionID types.String `tfsdk:"latest_version_id"`
	SHA256          types.String `tfsdk:"sha256"`
}

func (r *skillResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_skill"
}

func (r *skillResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A catalog skill (`/skills`). `files` and `notes` describe the latest published version; changing them publishes the " +
			"next immutable version. Skills give guidance, never permission. SpaceMan has no API to rename or delete a skill: identifying fields " +
			"are fixed and destroying the resource only removes it from Terraform's state.",
		Attributes: map[string]schema.Attribute{
			"id":          idAttr("The skill ID."),
			"slug":        fixedString("Unique identifier; `SKILL.md`'s frontmatter `name` must equal it. Fixed after creation.", "A skill's slug"),
			"name":        fixedString("Display name. Fixed after creation.", "A skill's name"),
			"description": fixedOptString("Description. Fixed after creation.", "A skill's description"),
			"files": schema.MapAttribute{Required: true, ElementType: types.StringType,
				MarkdownDescription: "Relative path → text content. `SKILL.md` is required and starts with `---` frontmatter naming the skill and describing it."},
			"notes":             defaultEmpty("Notes recorded with the published version."),
			"latest_version":    schema.Int64Attribute{Computed: true, MarkdownDescription: "Number of the latest published version."},
			"latest_version_id": schema.StringAttribute{Computed: true, MarkdownDescription: "ID of the latest published version (use it to pin)."},
			"sha256":            schema.StringAttribute{Computed: true, MarkdownDescription: "SHA-256 over the latest version's files."},
		},
	}
}

func (r *skillResource) toModel(s *client.Skill, m *skillModel) {
	m.ID = types.StringValue(s.ID)
	m.Slug = types.StringValue(s.Slug)
	m.Name = types.StringValue(s.Name)
	m.Description = types.StringValue(s.Description)
	if lp := s.LatestPublished(); lp != nil {
		m.Files = mapValue(lp.Files)
		m.Notes = types.StringValue(lp.Notes)
		m.LatestVersion = types.Int64Value(lp.Number)
		m.LatestVersionID = types.StringValue(lp.ID)
		m.SHA256 = types.StringValue(lp.SHA256)
	} else {
		m.Files, m.Notes = mapValue(nil), types.StringValue("")
		m.LatestVersion, m.LatestVersionID, m.SHA256 = types.Int64Value(0), types.StringValue(""), types.StringValue("")
	}
}

func (r *skillResource) publish(ctx context.Context, id string, m *skillModel, diags *diagnostics) *client.Skill {
	s, err := r.c.GetSkill(ctx, id)
	if err != nil {
		addAPIError(diags, "Could not read the skill", err)
		return nil
	}
	in := client.SkillDraftInput{Files: stringMap(ctx, m.Files, diags), Notes: strOrEmpty(m.Notes)}
	if diags.HasError() {
		return nil
	}
	var dv int64
	if d := s.Draft(); d != nil {
		if !equalMaps(d.Files, in.Files) || d.Notes != in.Notes {
			draftReplacedWarning(diags, "skill", d.Version)
		}
		dv = d.Version
	}
	if s, err = r.c.SaveSkillDraft(ctx, id, dv, in); err != nil {
		addAPIError(diags, "Could not save the skill's draft", err)
		return nil
	}
	d := s.Draft()
	if d == nil {
		diags.AddError("Could not publish the skill", "the saved draft was not found")
		return nil
	}
	if s, err = r.c.PublishSkill(ctx, id, d.Version); err != nil {
		addAPIError(diags, "Could not publish the skill", err)
		return nil
	}
	return s
}

func equalMaps(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

func (r *skillResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var m skillModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	files := stringMap(ctx, m.Files, &resp.Diagnostics)
	s, err := r.c.CreateSkill(ctx, client.CreateSkillInput{Slug: m.Slug.ValueString(), Name: m.Name.ValueString(), Description: strOrEmpty(m.Description),
		Files: files})
	if err != nil {
		addAPIError(&resp.Diagnostics, "Could not create the skill", err)
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), s.ID)...)
	if s = r.publish(ctx, s.ID, &m, &resp.Diagnostics); s == nil {
		return
	}
	r.toModel(s, &m)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *skillResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var m skillModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	s, err := r.c.GetSkill(ctx, m.ID.ValueString())
	if client.IsNotFound(err) {
		resp.State.RemoveResource(ctx)
		return
	} else if err != nil {
		addAPIError(&resp.Diagnostics, "Could not read the skill", err)
		return
	}
	r.toModel(s, &m)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *skillResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var m skillModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	s := r.publish(ctx, m.ID.ValueString(), &m, &resp.Diagnostics)
	if s == nil {
		return
	}
	r.toModel(s, &m)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *skillResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var m skillModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	noDeleteWarning(&resp.Diagnostics, "skill", m.ID.ValueString())
}

func (r *skillResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	importPassthrough(ctx, req, resp)
}
