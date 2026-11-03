package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/aurimasniekis/go-spaceman/terraform-provider-spaceman/internal/client"
)

// base gives a resource its configured client.
type base struct {
	c *client.Client
}

func (b *base) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	b.c = configureClient(req.ProviderData, &resp.Diagnostics)
}

// Common schema pieces.
func idAttr(desc string) schema.StringAttribute {
	return schema.StringAttribute{Computed: true, MarkdownDescription: desc,
		PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}}
}

// optString is an optional attribute Terraform owns only when it is set: left out, the server's value (edited in the
// UI, or the default) is kept and never shows as drift (SM-API-005).
func optString(desc string, more ...validator.String) schema.StringAttribute {
	return schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: desc, Validators: more,
		PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}}
}

func computedString(desc string) schema.StringAttribute {
	return schema.StringAttribute{Computed: true, MarkdownDescription: desc,
		PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}}
}

// relocationRefusal is the plan-time refusal of a node_id change: relocation (SM-SPACE-005…008, work package M4-05)
// is not implemented, and a pinned space is never moved by destroying and recreating it (SM-INV-003).
var relocationRefusal = refuseChange("Space relocation is not supported yet",
	"Changing node_id asks SpaceMan to relocate the space to another node. Safe relocation (stop, checkpoint, transfer "+
		"and re-pin, SM-SPACE-005…008) is not implemented in this SpaceMan version, and this provider never moves a "+
		"space by destroying and recreating it. Keep the current node_id; relocation will arrive with a later SpaceMan "+
		"release and this provider will then plan it as an in-place change.")

type spaceResource struct{ base }

func newSpaceResource() resource.Resource { return &spaceResource{} }

type spaceModel struct {
	ID                   types.String `tfsdk:"id"`
	Slug                 types.String `tfsdk:"slug"`
	Name                 types.String `tfsdk:"name"`
	Description          types.String `tfsdk:"description"`
	Goals                types.String `tfsdk:"goals"`
	StandingInstructions types.String `tfsdk:"standing_instructions"`
	NodeID               types.String `tfsdk:"node_id"`
	DisconnectPolicy     types.String `tfsdk:"disconnect_policy"`
	PurgeOnDestroy       types.Bool   `tfsdk:"purge_on_destroy"`
	State                types.String `tfsdk:"state"`
	Workdir              types.String `tfsdk:"workdir"`
}

func (r *spaceResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_space"
}

func (r *spaceResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A space pinned to a node (`POST/GET/PUT /spaces`). Destroying it archives the space (`POST /spaces/{id}/state`); " +
			"it is purged only with `purge_on_destroy`. Pause state is runtime state and is not managed.",
		Attributes: map[string]schema.Attribute{
			"id": idAttr("The space ID (`spc_…`)."),
			"slug": schema.StringAttribute{Required: true, MarkdownDescription: "Lowercase identifier, unique (3-48 characters). Fixed after creation.",
				PlanModifiers: []planmodifier.String{fixedAfterCreate("A space's slug")}},
			"name":                  schema.StringAttribute{Required: true, MarkdownDescription: "Display name."},
			"description":           optString("Description."),
			"goals":                 optString("Goals every run of the space sees."),
			"standing_instructions": optString("Standing instructions every run of the space sees."),
			"node_id": schema.StringAttribute{Required: true,
				MarkdownDescription: "The node the space is pinned to (`nod_…`). A change is refused at plan time until relocation is implemented; it never destroys and recreates the space.",
				PlanModifiers:       []planmodifier.String{relocationRefusal}},
			"disconnect_policy": optString("What the node does with runs after losing central beyond the grace: `interrupt` (default) or `kill`.",
				stringvalidator.OneOf("interrupt", "kill")),
			"purge_on_destroy": schema.BoolAttribute{Optional: true, Computed: true, Default: booldefault.StaticBool(false),
				MarkdownDescription: "On destroy, also purge the archived space's records (irreversible). Upstream repositories are never touched. Default `false`: destroy only archives."},
			"state":   computedString("`active`, `paused` or `archived` (read-only: pausing is an operation, not configuration)."),
			"workdir": computedString("The managed folder name on the node."),
		},
	}
}

func (r *spaceResource) toModel(s *client.Space, m *spaceModel) {
	m.ID = types.StringValue(s.ID)
	m.Slug = types.StringValue(s.Slug)
	m.Name = types.StringValue(s.Name)
	m.Description = types.StringValue(s.Description)
	m.Goals = types.StringValue(s.Goals)
	m.StandingInstructions = types.StringValue(s.StandingInstructions)
	m.NodeID = types.StringValue(s.NodeID)
	m.DisconnectPolicy = types.StringValue(s.DisconnectPolicy)
	m.State = types.StringValue(s.State)
	m.Workdir = types.StringValue(s.Workdir)
	if m.PurgeOnDestroy.IsNull() || m.PurgeOnDestroy.IsUnknown() {
		m.PurgeOnDestroy = types.BoolValue(false)
	}
}

func (r *spaceResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var m spaceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	s, err := r.c.CreateSpace(ctx, client.CreateSpaceInput{Slug: m.Slug.ValueString(), Name: m.Name.ValueString(), Description: strOrEmpty(m.Description),
		Goals: strOrEmpty(m.Goals), StandingInstructions: strOrEmpty(m.StandingInstructions), NodeID: m.NodeID.ValueString(),
		DisconnectPolicy: strOrEmpty(m.DisconnectPolicy)})
	if err != nil {
		addAPIError(&resp.Diagnostics, "Could not create the space", err)
		return
	}
	r.toModel(s, &m)
	setVersion(ctx, resp.Private, s.Version, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *spaceResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var m spaceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	s, err := r.c.GetSpace(ctx, m.ID.ValueString())
	if client.IsNotFound(err) {
		resp.State.RemoveResource(ctx)
		return
	} else if err != nil {
		addAPIError(&resp.Diagnostics, "Could not read the space", err)
		return
	}
	r.toModel(s, &m)
	setVersion(ctx, resp.Private, s.Version, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *spaceResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var m, prior spaceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &prior)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if m.NodeID.ValueString() != prior.NodeID.ValueString() {
		resp.Diagnostics.AddError("Space relocation is not supported yet", "node_id cannot change until SpaceMan implements relocation (M4-05).")
		return
	}
	v := getVersion(ctx, req.Private, &resp.Diagnostics)
	if v == 0 {
		cur, err := r.c.GetSpace(ctx, prior.ID.ValueString())
		if err != nil {
			addAPIError(&resp.Diagnostics, "Could not read the space", err)
			return
		}
		v = cur.Version
	}
	s, err := r.c.UpdateSpace(ctx, prior.ID.ValueString(), v, client.UpdateSpaceInput{Name: m.Name.ValueString(), Description: strOrEmpty(m.Description),
		Goals: strOrEmpty(m.Goals), StandingInstructions: strOrEmpty(m.StandingInstructions), DisconnectPolicy: strOrEmpty(m.DisconnectPolicy)})
	if err != nil {
		addAPIError(&resp.Diagnostics, "Could not update the space", err)
		return
	}
	r.toModel(s, &m)
	setVersion(ctx, resp.Private, s.Version, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *spaceResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var m spaceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	s, err := r.c.GetSpace(ctx, m.ID.ValueString())
	if client.IsNotFound(err) {
		return
	} else if err != nil {
		addAPIError(&resp.Diagnostics, "Could not read the space", err)
		return
	}
	if s.State != "archived" {
		if s, err = r.c.SetSpaceState(ctx, s.ID, s.Version, "archived", ""); err != nil {
			addAPIError(&resp.Diagnostics, "Could not archive the space", err)
			return
		}
	}
	if m.PurgeOnDestroy.ValueBool() {
		if err := r.c.PurgeSpace(ctx, s.ID); err != nil {
			addAPIError(&resp.Diagnostics, "Could not purge the archived space", err)
			return
		}
	}
}

func (r *spaceResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	importPassthrough(ctx, req, resp)
}
