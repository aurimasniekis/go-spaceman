package provider

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
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

// relocationPoll is how often an apply that relocates a space reads the relocation's progress.
var relocationPoll = 2 * time.Second

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
	RelocDirtyState      types.String `tfsdk:"relocation_dirty_state"`
	RelocStopRuns        types.Bool   `tfsdk:"relocation_stop_runs"`
	RelocRecovery        types.String `tfsdk:"relocation_recovery"`
	RelocRetainDays      types.Int64  `tfsdk:"relocation_retain_days"`
	RelocTimeout         types.String `tfsdk:"relocation_timeout"`
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
				MarkdownDescription: "The node the space is pinned to (`nod_…`). A change is an in-place, safe relocation through SpaceMan (`POST /spaces/{id}/relocation`, " +
					"SM-SPACE-005): the source is checked, unpushed or uncommitted state is refused unless `relocation_dirty_state = \"export\"`, the destination " +
					"is prepared, and the assignment moves in one step. The space stays paused afterwards. It never destroys and recreates the space."},
			"relocation_dirty_state": schema.StringAttribute{Optional: true, Computed: true, Default: stringdefault.StaticString("refuse"),
				MarkdownDescription: "On a `node_id` change: `refuse` (default) stops at unpushed, changed or untracked state on the source; `export` carries it " +
					"through central to the destination. Not sent to SpaceMan otherwise.",
				Validators: []validator.String{stringvalidator.OneOf("refuse", "export")}},
			"relocation_stop_runs": schema.BoolAttribute{Optional: true, Computed: true, Default: booldefault.StaticBool(false),
				MarkdownDescription: "On a `node_id` change: pause the space (interrupt) and stop its runs. Default `false`: the space must already be paused with no active runs."},
			"relocation_recovery": schema.StringAttribute{Optional: true, Computed: true, Default: stringdefault.StaticString("retain"),
				MarkdownDescription: "On a `node_id` change: `retain` (default) keeps the source folder on the source node, `delete` removes it.",
				Validators:          []validator.String{stringvalidator.OneOf("retain", "delete")}},
			"relocation_retain_days": schema.Int64Attribute{Optional: true, Computed: true, Default: int64default.StaticInt64(0),
				MarkdownDescription: "With `relocation_recovery = \"retain\"`: days to keep the source folder; `0` (default) keeps it until released.",
				Validators:          []validator.Int64{int64validator.Between(0, 3650)}},
			"relocation_timeout": schema.StringAttribute{Optional: true, Computed: true, Default: stringdefault.StaticString("30m"),
				MarkdownDescription: "How long an apply waits for a relocation to finish (Go duration, default `30m`). A relocation that is still running " +
					"continues in SpaceMan; the next apply waits for it again."},
			"disconnect_policy": optString("What the node does with runs after losing central beyond the grace: `interrupt` (default) or `kill`.",
				stringvalidator.OneOf("interrupt", "kill")),
			"purge_on_destroy": schema.BoolAttribute{Optional: true, Computed: true, Default: booldefault.StaticBool(false),
				MarkdownDescription: "On destroy, also purge the archived space's records (irreversible). Upstream repositories are never touched. Default `false`: destroy only archives."},
			"state":   computedString("`active`, `paused` or `archived` (read-only: pausing is an operation, not configuration; a relocated space is paused)."),
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
	// Relocation options are provider-side settings (sent only with a node_id change); imported or upgraded state gets
	// the defaults, so they never show as drift.
	if m.RelocDirtyState.IsNull() || m.RelocDirtyState.IsUnknown() {
		m.RelocDirtyState = types.StringValue("refuse")
	}
	if m.RelocStopRuns.IsNull() || m.RelocStopRuns.IsUnknown() {
		m.RelocStopRuns = types.BoolValue(false)
	}
	if m.RelocRecovery.IsNull() || m.RelocRecovery.IsUnknown() {
		m.RelocRecovery = types.StringValue("retain")
	}
	if m.RelocRetainDays.IsNull() || m.RelocRetainDays.IsUnknown() {
		m.RelocRetainDays = types.Int64Value(0)
	}
	if m.RelocTimeout.IsNull() || m.RelocTimeout.IsUnknown() {
		m.RelocTimeout = types.StringValue("30m")
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
	v := getVersion(ctx, req.Private, &resp.Diagnostics)
	if m.NodeID.ValueString() != prior.NodeID.ValueString() {
		if !r.relocate(ctx, prior.ID.ValueString(), m, &resp.Diagnostics) {
			return
		}
		v = 0 // the relocation changed the space's version
	}
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

// ModifyPlan marks the space's state unknown when node_id changes: a relocation leaves the space paused.
func (r *spaceResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.State.Raw.IsNull() || req.Plan.Raw.IsNull() {
		return
	}
	var plan, state spaceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() || plan.NodeID.IsUnknown() || plan.NodeID.ValueString() == state.NodeID.ValueString() {
		return
	}
	plan.State = types.StringUnknown()
	resp.Diagnostics.Append(resp.Plan.Set(ctx, &plan)...)
}

// relocate performs a node_id change as a SpaceMan relocation and waits for it (SM-API-005, ADR-0037). A relocation to
// the same node already in progress is resumed (a blocked one is retried with the configured policy) instead of starting
// a second one. A refusal leaves the relocation blocked in SpaceMan for an operator to inspect.
func (r *spaceResource) relocate(ctx context.Context, spaceID string, m spaceModel, d *diag.Diagnostics) bool {
	dest := m.NodeID.ValueString()
	timeout, err := time.ParseDuration(m.RelocTimeout.ValueString())
	if err != nil || timeout <= 0 {
		d.AddAttributeError(path.Root("relocation_timeout"), "Invalid relocation timeout", "relocation_timeout must be a positive Go duration such as 30m.")
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cur, err := r.c.GetRelocation(ctx, spaceID)
	switch {
	case err != nil && !client.IsNotFound(err):
		addAPIError(d, "Could not read the space's relocation", err)
		return false
	case err == nil && !cur.Finished() && cur.DestinationNodeID != dest:
		d.AddError("Another relocation is in progress", fmt.Sprintf("Relocation %s moves the space to %s; finish or cancel it in SpaceMan before "+
			"relocating it to %s.", cur.ID, cur.DestinationNodeID, dest))
		return false
	case err == nil && !cur.Finished() && cur.State == "blocked":
		if cur, err = r.c.RetryRelocation(ctx, spaceID, m.RelocDirtyState.ValueString()); err != nil {
			addAPIError(d, "Could not retry the space's relocation", err)
			return false
		}
	case err == nil && !cur.Finished():
		// Already moving to this node: wait for it.
	default:
		if cur, err = r.c.StartRelocation(ctx, spaceID, client.StartRelocationInput{DestinationNodeID: dest, DirtyState: m.RelocDirtyState.ValueString(),
			StopRuns: m.RelocStopRuns.ValueBool(), Recovery: m.RelocRecovery.ValueString(), RetainDays: m.RelocRetainDays.ValueInt64()}); err != nil {
			addAPIError(d, "SpaceMan refused to relocate the space", err)
			return false
		}
	}
	for {
		switch cur.State {
		case "done":
			return true
		case "blocked":
			var issues []string
			for _, i := range cur.Issues {
				s := i.Code
				if i.Path != "" {
					s += " (" + i.Path + ")"
				}
				issues = append(issues, s+": "+i.Detail)
			}
			detail := cur.Error
			if len(issues) > 0 {
				detail += "\n\nFindings:\n- " + strings.Join(issues, "\n- ")
			}
			d.AddError("The relocation is blocked", detail+"\n\nThe relocation "+cur.ID+" stays blocked in SpaceMan. Resolve it (push, sync), or set "+
				"relocation_dirty_state = \"export\" to carry the reported state, and apply again; or cancel it in SpaceMan.")
			return false
		case "failed", "cancelled":
			d.AddError("The relocation "+cur.State, cur.Error)
			return false
		}
		select {
		case <-ctx.Done():
			d.AddError("The relocation is still running", fmt.Sprintf("Relocation %s is %s after %s. It continues in SpaceMan; apply again to wait for it.",
				cur.ID, cur.State, timeout))
			return false
		case <-time.After(relocationPoll):
		}
		if cur, err = r.c.GetRelocation(ctx, spaceID); err != nil {
			if ctx.Err() != nil {
				continue
			}
			addAPIError(d, "Could not read the space's relocation", err)
			return false
		}
	}
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
