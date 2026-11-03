package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/aurimasniekis/go-spaceman/terraform-provider-spaceman/internal/client"
)

// ---- approval policies ----

type approvalPolicyResource struct{ base }

func newApprovalPolicyResource() resource.Resource { return &approvalPolicyResource{} }

type approvalPolicyModel struct {
	ID                types.String `tfsdk:"id"`
	SpaceID           types.String `tfsdk:"space_id"`
	Operation         types.String `tfsdk:"operation"`
	Uses              types.Int64  `tfsdk:"uses"`
	ValidSeconds      types.Int64  `tfsdk:"valid_seconds"`
	RequestTTLSeconds types.Int64  `tfsdk:"request_ttl_seconds"`
	Note              types.String `tfsdk:"note"`
}

func replaceInt64(desc string) schema.Int64Attribute {
	return schema.Int64Attribute{Optional: true, Computed: true, MarkdownDescription: desc,
		PlanModifiers: []planmodifier.Int64{int64planmodifier.RequiresReplaceIfConfigured(), int64planmodifier.UseStateForUnknown()}}
}

func (r *approvalPolicyResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_approval_policy"
}

func (r *approvalPolicyResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Makes an operation need a person's approval (`POST/DELETE /approval-policies`). Policies cannot be edited: any change " +
			"removes the policy and adds a new one. Approvals it already produced stay usable until they expire or are revoked.",
		Attributes: map[string]schema.Attribute{
			"id": idAttr("The policy ID."),
			"space_id": schema.StringAttribute{Optional: true, Computed: true, Default: stringdefault.StaticString(""),
				MarkdownDescription: "The space; empty (default) applies to every space.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()}},
			"operation":           requiredReplace("`spaceman.<tool>`, `mcp.<server>.<tool>`, or a prefix ending in `.*`."),
			"uses":                replaceInt64("Executions one approval allows (1-100, default 1)."),
			"valid_seconds":       replaceInt64("How long an approval can be used after the decision (60-604800, default 3600)."),
			"request_ttl_seconds": replaceInt64("How long a request waits for a decision (300-2592000, default 86400)."),
			"note": schema.StringAttribute{Optional: true, Computed: true, Default: stringdefault.StaticString(""), MarkdownDescription: "Note shown to approvers.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()}},
		},
	}
}

func (r *approvalPolicyResource) toModel(p *client.ApprovalPolicy, m *approvalPolicyModel) {
	m.ID = types.StringValue(p.ID)
	m.SpaceID = types.StringValue(p.SpaceID)
	m.Operation = types.StringValue(p.Operation)
	m.Uses = types.Int64Value(p.Uses)
	m.ValidSeconds = types.Int64Value(p.ValidSeconds)
	m.RequestTTLSeconds = types.Int64Value(p.RequestTTLSeconds)
	m.Note = types.StringValue(p.Note)
}

func (r *approvalPolicyResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var m approvalPolicyModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	p, err := r.c.CreateApprovalPolicy(ctx, client.ApprovalPolicyInput{SpaceID: strOrEmpty(m.SpaceID), Operation: m.Operation.ValueString(),
		Uses: int64OrZero(m.Uses), ValidSeconds: int64OrZero(m.ValidSeconds), RequestTTLSeconds: int64OrZero(m.RequestTTLSeconds), Note: strOrEmpty(m.Note)})
	if err != nil {
		addAPIError(&resp.Diagnostics, "Could not create the approval policy", err)
		return
	}
	r.toModel(p, &m)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *approvalPolicyResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var m approvalPolicyModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	p, err := r.c.GetApprovalPolicy(ctx, m.ID.ValueString())
	if client.IsNotFound(err) {
		resp.State.RemoveResource(ctx)
		return
	} else if err != nil {
		addAPIError(&resp.Diagnostics, "Could not read the approval policy", err)
		return
	}
	r.toModel(p, &m)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *approvalPolicyResource) Update(_ context.Context, _ resource.UpdateRequest, resp *resource.UpdateResponse) {
	resp.Diagnostics.AddError("Approval policies cannot be updated", "Every attribute of a policy forces a replacement; this is a provider bug.")
}

func (r *approvalPolicyResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var m approvalPolicyModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.c.DeleteApprovalPolicy(ctx, m.ID.ValueString()); err != nil {
		addAPIError(&resp.Diagnostics, "Could not remove the approval policy", err)
	}
}

func (r *approvalPolicyResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	importPassthrough(ctx, req, resp)
}

// ---- knowledge bases ----

type knowledgeBaseResource struct{ base }

func newKnowledgeBaseResource() resource.Resource { return &knowledgeBaseResource{} }

type knowledgeBaseModel struct {
	ID                types.String `tfsdk:"id"`
	SpaceID           types.String `tfsdk:"space_id"`
	Slug              types.String `tfsdk:"slug"`
	Name              types.String `tfsdk:"name"`
	Description       types.String `tfsdk:"description"`
	Repo              types.String `tfsdk:"repo"`
	BaseBranch        types.String `tfsdk:"base_branch"`
	SpaceRole         types.String `tfsdk:"space_role"`
	EntryInstructions types.String `tfsdk:"entry_instructions"`
	Validator         types.List   `tfsdk:"validator"`
	WritePolicy       types.String `tfsdk:"write_policy"`
	AllowedRoles      types.List   `tfsdk:"allowed_roles"`
	MaxSessions       types.Int64  `tfsdk:"max_sessions"`
	IdleSleepSeconds  types.Int64  `tfsdk:"idle_sleep_seconds"`
	RequestTimeout    types.Int64  `tfsdk:"request_timeout_seconds"`
	SessionTTLSeconds types.Int64  `tfsdk:"session_ttl_seconds"`
	Enabled           types.Bool   `tfsdk:"enabled"`
}

func (r *knowledgeBaseResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_knowledge_base"
}

func (r *knowledgeBaseResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A knowledge base configuration of a space (`/spaces/{id}/kbs`). Sessions, requests and proposals are runtime state and are " +
			"not managed. SpaceMan has no API to delete a knowledge base: destroying the resource disables it and removes it from Terraform's state.",
		Attributes: map[string]schema.Attribute{
			"id":       idAttr("The knowledge base ID."),
			"space_id": requiredReplace("The space."),
			"slug": schema.StringAttribute{Required: true, MarkdownDescription: "Identifier in the space (2-31 characters). Fixed after creation.",
				PlanModifiers: []planmodifier.String{fixedAfterCreate("A knowledge base's slug")}},
			"name":        schema.StringAttribute{Required: true, MarkdownDescription: "Display name."},
			"description": optString("Description."),
			"repo": schema.StringAttribute{Required: true, MarkdownDescription: "Repository folder inside the space (relative), for example " +
				"`spaceman_space_repository.kb.path`. A folder no space repository uses yet is added to the space's repositories (role `kb`, " +
				"clone policy `manual`); declare it as a `spaceman_space_repository` to manage it."},
			"base_branch":        optString("Where accepted knowledge lives (default `main`)."),
			"space_role":         schema.StringAttribute{Required: true, MarkdownDescription: "A space role whose template is of kind `kb`; sessions run as it."},
			"entry_instructions": optString("Instructions every session starts with."),
			"validator":          optList("Command run on a proposal before it is accepted, e.g. `[\"python3\", \"tools/kb.py\", \"check\"]`."),
			"write_policy": optString("`read_only`, `review` (default) or `serialized`.",
				stringvalidator.OneOf("read_only", "review", "serialized")),
			"allowed_roles":           optList("Requester roles admitted to sessions; `*` (default) admits every role."),
			"max_sessions":            optInt64("Sessions running at once (1-16, default 2)."),
			"idle_sleep_seconds":      optInt64("Idle time before a session sleeps (60-86400, default 900)."),
			"request_timeout_seconds": optInt64("Request timeout (60-86400, default 1800)."),
			"session_ttl_seconds":     optInt64("Session lifetime (3600-2592000, default 604800)."),
			"enabled":                 optBool("Whether the knowledge base accepts requests (default `true`)."),
		},
	}
}

func (r *knowledgeBaseResource) toModel(k *client.KB, m *knowledgeBaseModel) {
	m.ID = types.StringValue(k.ID)
	m.SpaceID = types.StringValue(k.SpaceID)
	m.Slug = types.StringValue(k.Slug)
	m.Name = types.StringValue(k.Name)
	m.Description = types.StringValue(k.Description)
	m.Repo = types.StringValue(k.Repo)
	m.BaseBranch = types.StringValue(k.BaseBranch)
	m.SpaceRole = types.StringValue(k.SpaceRole)
	m.EntryInstructions = types.StringValue(k.EntryInstructions)
	m.Validator = listValue(k.Validator)
	m.WritePolicy = types.StringValue(k.WritePolicy)
	m.AllowedRoles = listValue(k.AllowedRoles)
	m.MaxSessions = types.Int64Value(k.MaxSessions)
	m.IdleSleepSeconds = types.Int64Value(k.IdleSleepSeconds)
	m.RequestTimeout = types.Int64Value(k.RequestTimeout)
	m.SessionTTLSeconds = types.Int64Value(k.SessionTTLSeconds)
	m.Enabled = types.BoolValue(k.Enabled)
}

func (r *knowledgeBaseResource) input(ctx context.Context, m *knowledgeBaseModel, diags *diagnostics) client.KBInput {
	return client.KBInput{Slug: m.Slug.ValueString(), Name: m.Name.ValueString(), Description: strOrEmpty(m.Description), Repo: m.Repo.ValueString(),
		BaseBranch: strOrEmpty(m.BaseBranch), SpaceRole: m.SpaceRole.ValueString(), EntryInstructions: strOrEmpty(m.EntryInstructions),
		Validator: stringList(ctx, m.Validator, diags), WritePolicy: strOrEmpty(m.WritePolicy), AllowedRoles: stringList(ctx, m.AllowedRoles, diags),
		MaxSessions: int64OrZero(m.MaxSessions), IdleSleepSeconds: int64OrZero(m.IdleSleepSeconds), RequestTimeout: int64OrZero(m.RequestTimeout),
		SessionTTLSeconds: int64OrZero(m.SessionTTLSeconds), Enabled: boolPtr(m.Enabled)}
}

func (r *knowledgeBaseResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var m knowledgeBaseModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	in := r.input(ctx, &m, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	k, err := r.c.CreateKB(ctx, m.SpaceID.ValueString(), in)
	if err != nil {
		addAPIError(&resp.Diagnostics, "Could not create the knowledge base", err)
		return
	}
	r.toModel(k, &m)
	setVersion(ctx, resp.Private, k.Version, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *knowledgeBaseResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var m knowledgeBaseModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	k, err := r.c.GetKB(ctx, m.SpaceID.ValueString(), m.ID.ValueString())
	if client.IsNotFound(err) {
		resp.State.RemoveResource(ctx)
		return
	} else if err != nil {
		addAPIError(&resp.Diagnostics, "Could not read the knowledge base", err)
		return
	}
	r.toModel(k, &m)
	setVersion(ctx, resp.Private, k.Version, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *knowledgeBaseResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var m, prior knowledgeBaseModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &prior)...)
	in := r.input(ctx, &m, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	k, err := r.update(ctx, prior.SpaceID.ValueString(), prior.ID.ValueString(), getVersion(ctx, req.Private, &resp.Diagnostics), in)
	if err != nil {
		addAPIError(&resp.Diagnostics, "Could not update the knowledge base", err)
		return
	}
	r.toModel(k, &m)
	setVersion(ctx, resp.Private, k.Version, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *knowledgeBaseResource) update(ctx context.Context, spaceID, id string, v int64, in client.KBInput) (*client.KB, error) {
	if v == 0 {
		cur, err := r.c.GetKB(ctx, spaceID, id)
		if err != nil {
			return nil, err
		}
		v = cur.Version
	}
	return r.c.UpdateKB(ctx, spaceID, id, v, in)
}

func (r *knowledgeBaseResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var m knowledgeBaseModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	k, err := r.c.GetKB(ctx, m.SpaceID.ValueString(), m.ID.ValueString())
	if client.IsNotFound(err) {
		return
	} else if err != nil {
		addAPIError(&resp.Diagnostics, "Could not read the knowledge base", err)
		return
	}
	if k.Enabled {
		off := false
		in := client.KBInput{Name: k.Name, Description: k.Description, Repo: k.Repo, BaseBranch: k.BaseBranch, SpaceRole: k.SpaceRole,
			EntryInstructions: k.EntryInstructions, Validator: k.Validator, WritePolicy: k.WritePolicy, AllowedRoles: k.AllowedRoles,
			MaxSessions: k.MaxSessions, IdleSleepSeconds: k.IdleSleepSeconds, RequestTimeout: k.RequestTimeout, SessionTTLSeconds: k.SessionTTLSeconds,
			Enabled: &off}
		if _, err := r.c.UpdateKB(ctx, k.SpaceID, k.ID, k.Version, in); err != nil {
			addAPIError(&resp.Diagnostics, "Could not disable the knowledge base", err)
			return
		}
	}
	resp.Diagnostics.AddWarning("Knowledge base disabled, not deleted",
		"SpaceMan has no API to delete a knowledge base. Terraform disabled "+k.ID+" and removed it from its state; its configuration stays in SpaceMan.")
}

func (r *knowledgeBaseResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	p := splitImportID(req.ID, 2, "<space_id>/<kb_id>", &resp.Diagnostics)
	if p == nil {
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("space_id"), p[0])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), p[1])...)
}

// ---- dreams ----

type dreamResource struct{ base }

func newDreamResource() resource.Resource { return &dreamResource{} }

type dreamModel struct {
	ID              types.String `tfsdk:"id"`
	SpaceID         types.String `tfsdk:"space_id"`
	Slug            types.String `tfsdk:"slug"`
	Name            types.String `tfsdk:"name"`
	SpaceRole       types.String `tfsdk:"space_role"`
	MemoryRoles     types.List   `tfsdk:"memory_roles"`
	TranscriptRoles types.List   `tfsdk:"transcript_roles"`
	IncludeOwn      types.Bool   `tfsdk:"include_own"`
	Instructions    types.String `tfsdk:"instructions"`
	IntervalHours   types.Int64  `tfsdk:"interval_hours"`
	MaxMinutes      types.Int64  `tfsdk:"max_minutes"`
	MaxTokens       types.Int64  `tfsdk:"max_tokens"`
	MaxFiles        types.Int64  `tfsdk:"max_files"`
	MaxBytes        types.Int64  `tfsdk:"max_bytes"`
	Enabled         types.Bool   `tfsdk:"enabled"`
}

func (r *dreamResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_dream"
}

func (r *dreamResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A dream configuration of a space (`/spaces/{id}/dreams`): which role memories a dreamer consolidates, from which transcripts, " +
			"how often and within which bounds. Dream runs are runtime state and are not managed. SpaceMan has no API to delete a configuration: " +
			"destroying the resource disables it and removes it from Terraform's state.",
		Attributes: map[string]schema.Attribute{
			"id":       idAttr("The dream configuration ID."),
			"space_id": requiredReplace("The space."),
			"slug": schema.StringAttribute{Required: true, MarkdownDescription: "Identifier in the space (2-31 characters). Fixed after creation.",
				PlanModifiers: []planmodifier.String{fixedAfterCreate("A dream configuration's slug")}},
			"name":             schema.StringAttribute{Required: true, MarkdownDescription: "Display name."},
			"space_role":       schema.StringAttribute{Required: true, MarkdownDescription: "A space role whose template is of kind `dreamer`."},
			"memory_roles":     optList("Role memories the dream consolidates; `*` (default) for every role."),
			"transcript_roles": optList("Roles whose transcripts it may read; `*` (default) for every role."),
			"include_own":      optBool("Also read the dreamer's own transcripts (default `false`)."),
			"instructions":     optString("Extra instructions for the dreamer."),
			"interval_hours":   optInt64("Hours between scheduled dreams (0-720; 0, the default, runs only manually)."),
			"max_minutes":      optInt64("Time limit of one dream (5-240, default 30)."),
			"max_tokens":       optInt64("Token budget given to the dreamer (not enforced by SpaceMan)."),
			"max_files":        optInt64("Transcript files offered per dream (1-200, default 20)."),
			"max_bytes":        optInt64("Transcript bytes offered per dream (64 KiB-64 MiB, default 2 MiB)."),
			"enabled":          optBool("Whether the configuration is scheduled (default `true`)."),
		},
	}
}

func (r *dreamResource) toModel(d *client.Dream, m *dreamModel) {
	m.ID = types.StringValue(d.ID)
	m.SpaceID = types.StringValue(d.SpaceID)
	m.Slug = types.StringValue(d.Slug)
	m.Name = types.StringValue(d.Name)
	m.SpaceRole = types.StringValue(d.SpaceRole)
	m.MemoryRoles = listValue(d.MemoryRoles)
	m.TranscriptRoles = listValue(d.TranscriptRoles)
	m.IncludeOwn = types.BoolValue(d.IncludeOwn)
	m.Instructions = types.StringValue(d.Instructions)
	m.IntervalHours = types.Int64Value(d.IntervalHours)
	m.MaxMinutes = types.Int64Value(d.MaxMinutes)
	m.MaxTokens = types.Int64Value(d.MaxTokens)
	m.MaxFiles = types.Int64Value(d.MaxFiles)
	m.MaxBytes = types.Int64Value(d.MaxBytes)
	m.Enabled = types.BoolValue(d.Enabled)
}

func (r *dreamResource) input(ctx context.Context, m *dreamModel, diags *diagnostics) client.DreamInput {
	return client.DreamInput{Slug: m.Slug.ValueString(), Name: m.Name.ValueString(), SpaceRole: m.SpaceRole.ValueString(),
		MemoryRoles: stringList(ctx, m.MemoryRoles, diags), TranscriptRoles: stringList(ctx, m.TranscriptRoles, diags), IncludeOwn: m.IncludeOwn.ValueBool(),
		Instructions: strOrEmpty(m.Instructions), IntervalHours: int64OrZero(m.IntervalHours), MaxMinutes: int64OrZero(m.MaxMinutes),
		MaxTokens: int64OrZero(m.MaxTokens), MaxFiles: int64OrZero(m.MaxFiles), MaxBytes: int64OrZero(m.MaxBytes), Enabled: boolPtr(m.Enabled)}
}

func dreamInputOf(d *client.Dream) client.DreamInput {
	en := d.Enabled
	return client.DreamInput{Name: d.Name, SpaceRole: d.SpaceRole, MemoryRoles: d.MemoryRoles, TranscriptRoles: d.TranscriptRoles, IncludeOwn: d.IncludeOwn,
		Instructions: d.Instructions, IntervalHours: d.IntervalHours, MaxMinutes: d.MaxMinutes, MaxTokens: d.MaxTokens, MaxFiles: d.MaxFiles,
		MaxBytes: d.MaxBytes, Enabled: &en}
}

func (r *dreamResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var m dreamModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	in := r.input(ctx, &m, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	d, err := r.c.CreateDream(ctx, m.SpaceID.ValueString(), in)
	if err != nil {
		addAPIError(&resp.Diagnostics, "Could not create the dream configuration", err)
		return
	}
	r.toModel(d, &m)
	setVersion(ctx, resp.Private, d.Version, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *dreamResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var m dreamModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	d, err := r.c.GetDream(ctx, m.SpaceID.ValueString(), m.ID.ValueString())
	if client.IsNotFound(err) {
		resp.State.RemoveResource(ctx)
		return
	} else if err != nil {
		addAPIError(&resp.Diagnostics, "Could not read the dream configuration", err)
		return
	}
	r.toModel(d, &m)
	setVersion(ctx, resp.Private, d.Version, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *dreamResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var m, prior dreamModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &prior)...)
	in := r.input(ctx, &m, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	v := getVersion(ctx, req.Private, &resp.Diagnostics)
	if v == 0 {
		cur, err := r.c.GetDream(ctx, prior.SpaceID.ValueString(), prior.ID.ValueString())
		if err != nil {
			addAPIError(&resp.Diagnostics, "Could not read the dream configuration", err)
			return
		}
		v = cur.Version
	}
	d, err := r.c.UpdateDream(ctx, prior.SpaceID.ValueString(), prior.ID.ValueString(), v, in)
	if err != nil {
		addAPIError(&resp.Diagnostics, "Could not update the dream configuration", err)
		return
	}
	r.toModel(d, &m)
	setVersion(ctx, resp.Private, d.Version, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *dreamResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var m dreamModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	d, err := r.c.GetDream(ctx, m.SpaceID.ValueString(), m.ID.ValueString())
	if client.IsNotFound(err) {
		return
	} else if err != nil {
		addAPIError(&resp.Diagnostics, "Could not read the dream configuration", err)
		return
	}
	if d.Enabled {
		in := dreamInputOf(d)
		off := false
		in.Enabled = &off
		if _, err := r.c.UpdateDream(ctx, d.SpaceID, d.ID, d.Version, in); err != nil {
			addAPIError(&resp.Diagnostics, "Could not disable the dream configuration", err)
			return
		}
	}
	resp.Diagnostics.AddWarning("Dream configuration disabled, not deleted",
		"SpaceMan has no API to delete a dream configuration. Terraform disabled "+d.ID+" and removed it from its state; it stays in SpaceMan.")
}

func (r *dreamResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	p := splitImportID(req.ID, 2, "<space_id>/<dream_id>", &resp.Diagnostics)
	if p == nil {
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("space_id"), p[0])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), p[1])...)
}
