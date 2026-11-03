package provider

import (
	"context"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/aurimasniekis/go-spaceman/terraform-provider-spaceman/internal/client"
)

// humanPolicyTypes are the action types a policy can name (internal/human.PolicyTypes).
var humanPolicyTypes = []string{"all", "question", "approval", "todo", "message", "custom"}

// builtinAllPolicy is SpaceMan's built-in policy for a type without its own defaults (internal/human.Builtin): the
// starting point of an `all` policy's fields that the configuration leaves out.
var builtinAllPolicy = client.HumanPolicyFields{MaxReminders: 1, RequesterEnded: "reroute", HoldMinutes: 60, WakeIdle: true,
	RatePerRunHour: 30, RatePerSpaceHour: 300}

type humanPolicyResource struct{ base }

func newHumanPolicyResource() resource.Resource { return &humanPolicyResource{} }

type humanPolicyModel struct {
	ID                   types.String `tfsdk:"id"`
	SpaceID              types.String `tfsdk:"space_id"`
	ActionType           types.String `tfsdk:"action_type"`
	RemindAfterMinutes   types.Int64  `tfsdk:"remind_after_minutes"`
	RemindEveryMinutes   types.Int64  `tfsdk:"remind_every_minutes"`
	MaxReminders         types.Int64  `tfsdk:"max_reminders"`
	EscalateAfterMinutes types.Int64  `tfsdk:"escalate_after_minutes"`
	EscalatePriority     types.String `tfsdk:"escalate_priority"`
	EscalateAssignee     types.String `tfsdk:"escalate_assignee"`
	RequesterEnded       types.String `tfsdk:"requester_ended"`
	HoldMinutes          types.Int64  `tfsdk:"hold_minutes"`
	SuccessorRole        types.String `tfsdk:"successor_role"`
	WakeIdle             types.Bool   `tfsdk:"wake_idle"`
	RatePerRunHour       types.Int64  `tfsdk:"rate_per_run_hour"`
	RatePerSpaceHour     types.Int64  `tfsdk:"rate_per_space_hour"`
}

func (r *humanPolicyResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_human_action_policy"
}

func (r *humanPolicyResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A human-action policy (SM-HUI-008, ADR-0032): reminders, escalation, rate limits and response routing for one action " +
			"type, as an installation default (`PUT/DELETE /human-policies/{type}`) or a space override (`/spaces/{id}/human-policies/{type}`). " +
			"The most specific policy applies as a whole (space and type, space and `all`, default and type, default and `all`, built-in). " +
			"Fields the configuration leaves out start from the policy in force for that type when the resource is created and are not owned " +
			"afterwards. Creating a policy that already exists fails (import it). Destroying the resource deletes the policy; the next less " +
			"specific one applies. Pending actions get their reminder and escalation times recomputed on every change. Human-action requests " +
			"and responses are runtime state and are never managed.",
		Attributes: map[string]schema.Attribute{
			"id": idAttr("The policy ID (`hpl_…`)."),
			"space_id": schema.StringAttribute{Optional: true, Computed: true, Default: stringdefault.StaticString(""),
				MarkdownDescription: "The space (not archived); empty (default) is the installation default.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()}},
			"action_type": schema.StringAttribute{Required: true, Validators: []validator.String{stringvalidator.OneOf(humanPolicyTypes...)},
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
				MarkdownDescription: "`question`, `approval`, `todo`, `message`, `custom` (every custom type) or `all` (every type without a more " +
					"specific policy)."},
			"remind_after_minutes":   optInt64("Remind people this long after the action was asked (0-43200; 0: no reminders)."),
			"remind_every_minutes":   optInt64("Repeat the reminder this often (0-43200; 0: once)."),
			"max_reminders":          optInt64("Reminders at most (1-20)."),
			"escalate_after_minutes": optInt64("Escalate this long after the action was asked (0-43200; 0: never). Needs `escalate_priority` or `escalate_assignee`."),
			"escalate_priority": optString("Raise the priority to this on escalation: `high`, `urgent` or `\"\"` (never lowers it).",
				stringvalidator.OneOf("", "high", "urgent")),
			"escalate_assignee": optString("Assign the action to this user (`usr_…`) when it escalates; `\"\"` for nobody."),
			"requester_ended": optString("When the requester's run has ended: `reroute` at once, or `next_launch` (keep it for the next run of the same agent instance).",
				stringvalidator.OneOf("reroute", "next_launch")),
			"hold_minutes":        optInt64("`next_launch`: how long to wait for that run before rerouting (1-10080)."),
			"successor_role":      optString("Reroute to a live run of this role before control; `\"\"`: control."),
			"wake_idle":           optBool("Wake an idle requester with a one-line terminal notice."),
			"rate_per_run_hour":   optInt64("New actions of this type one agent run may ask per hour (1-1000)."),
			"rate_per_space_hour": optInt64("New actions of this type agents of one space may ask per hour (1-10000)."),
		},
	}
}

func (r *humanPolicyResource) toModel(p *client.HumanPolicy, m *humanPolicyModel) {
	m.ID = types.StringValue(p.ID)
	m.SpaceID = types.StringValue(p.SpaceID)
	m.ActionType = types.StringValue(p.ActionType)
	f := p.HumanPolicyFields
	m.RemindAfterMinutes = types.Int64Value(f.RemindAfterMinutes)
	m.RemindEveryMinutes = types.Int64Value(f.RemindEveryMinutes)
	m.MaxReminders = types.Int64Value(f.MaxReminders)
	m.EscalateAfterMinutes = types.Int64Value(f.EscalateAfterMinutes)
	m.EscalatePriority = types.StringValue(f.EscalatePriority)
	m.EscalateAssignee = types.StringValue(f.EscalateAssignee)
	m.RequesterEnded = types.StringValue(f.RequesterEnded)
	m.HoldMinutes = types.Int64Value(f.HoldMinutes)
	m.SuccessorRole = types.StringValue(f.SuccessorRole)
	m.WakeIdle = types.BoolValue(f.WakeIdle)
	m.RatePerRunHour = types.Int64Value(f.RatePerRunHour)
	m.RatePerSpaceHour = types.Int64Value(f.RatePerSpaceHour)
}

// fields is the planned policy: every field the plan knows, the others from base.
func (m *humanPolicyModel) fields(base client.HumanPolicyFields) client.HumanPolicyFields {
	f := base
	i64 := func(v types.Int64, dst *int64) {
		if !v.IsNull() && !v.IsUnknown() {
			*dst = v.ValueInt64()
		}
	}
	str := func(v types.String, dst *string) {
		if !v.IsNull() && !v.IsUnknown() {
			*dst = v.ValueString()
		}
	}
	i64(m.RemindAfterMinutes, &f.RemindAfterMinutes)
	i64(m.RemindEveryMinutes, &f.RemindEveryMinutes)
	i64(m.MaxReminders, &f.MaxReminders)
	i64(m.EscalateAfterMinutes, &f.EscalateAfterMinutes)
	str(m.EscalatePriority, &f.EscalatePriority)
	str(m.EscalateAssignee, &f.EscalateAssignee)
	str(m.RequesterEnded, &f.RequesterEnded)
	i64(m.HoldMinutes, &f.HoldMinutes)
	str(m.SuccessorRole, &f.SuccessorRole)
	if !m.WakeIdle.IsNull() && !m.WakeIdle.IsUnknown() {
		f.WakeIdle = m.WakeIdle.ValueBool()
	}
	i64(m.RatePerRunHour, &f.RatePerRunHour)
	i64(m.RatePerSpaceHour, &f.RatePerSpaceHour)
	return f
}

// policyBase is the policy in force for the type in the scope now: the starting point of fields left out on create.
// For `all`, a space starts from the installation's `all` default when there is one, else from the built-in policy.
func policyBase(l *client.HumanPolicies, actionType string, defaults *client.HumanPolicies) client.HumanPolicyFields {
	if actionType == "all" {
		if defaults != nil {
			for _, p := range defaults.Items {
				if p.ActionType == "all" {
					return p.HumanPolicyFields
				}
			}
		}
		return builtinAllPolicy
	}
	for _, p := range l.Effective {
		if p.ActionType == actionType {
			return p.HumanPolicyFields
		}
	}
	return builtinAllPolicy
}

func (r *humanPolicyResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var m humanPolicyModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	spaceID, typ := strOrEmpty(m.SpaceID), m.ActionType.ValueString()
	l, err := r.c.ListHumanPolicies(ctx, spaceID)
	if err != nil {
		addAPIError(&resp.Diagnostics, "Could not read the human-action policies", err)
		return
	}
	for _, p := range l.Items {
		if p.ActionType == typ {
			id := typ
			if spaceID != "" {
				id = spaceID + "/" + typ
			}
			resp.Diagnostics.AddError("The policy already exists",
				"SpaceMan already has a "+typ+" policy here ("+p.ID+"). Terraform does not overwrite it: import it with "+
					"`terraform import <address> "+id+"`.")
			return
		}
	}
	var defaults *client.HumanPolicies
	if typ == "all" && spaceID != "" {
		if defaults, err = r.c.ListHumanPolicies(ctx, ""); err != nil {
			addAPIError(&resp.Diagnostics, "Could not read the installation's human-action policies", err)
			return
		}
	}
	p, err := r.c.SetHumanPolicy(ctx, spaceID, typ, 0, m.fields(policyBase(l, typ, defaults)))
	if err != nil {
		addAPIError(&resp.Diagnostics, "Could not create the human-action policy", err)
		return
	}
	r.toModel(p, &m)
	setVersion(ctx, resp.Private, p.Version, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *humanPolicyResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var m humanPolicyModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	p, err := r.c.GetHumanPolicy(ctx, strOrEmpty(m.SpaceID), m.ActionType.ValueString())
	if client.IsNotFound(err) {
		resp.State.RemoveResource(ctx)
		return
	} else if err != nil {
		addAPIError(&resp.Diagnostics, "Could not read the human-action policy", err)
		return
	}
	r.toModel(p, &m)
	setVersion(ctx, resp.Private, p.Version, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *humanPolicyResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var m, prior humanPolicyModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &prior)...)
	if resp.Diagnostics.HasError() {
		return
	}
	spaceID, typ := strOrEmpty(prior.SpaceID), prior.ActionType.ValueString()
	v := getVersion(ctx, req.Private, &resp.Diagnostics)
	if v == 0 {
		cur, err := r.c.GetHumanPolicy(ctx, spaceID, typ)
		if err != nil {
			addAPIError(&resp.Diagnostics, "Could not read the human-action policy", err)
			return
		}
		v = cur.Version
	}
	p, err := r.c.SetHumanPolicy(ctx, spaceID, typ, v, m.fields(prior.fields(client.HumanPolicyFields{})))
	if err != nil {
		addAPIError(&resp.Diagnostics, "Could not update the human-action policy", err)
		return
	}
	r.toModel(p, &m)
	setVersion(ctx, resp.Private, p.Version, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *humanPolicyResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var m humanPolicyModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.c.DeleteHumanPolicy(ctx, strOrEmpty(m.SpaceID), m.ActionType.ValueString()); err != nil {
		addAPIError(&resp.Diagnostics, "Could not delete the human-action policy", err)
	}
}

// ImportState takes `<action_type>` (an installation default) or `<space_id>/<action_type>`.
func (r *humanPolicyResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	spaceID, typ := "", req.ID
	if i := strings.LastIndex(req.ID, "/"); i >= 0 {
		spaceID, typ = req.ID[:i], req.ID[i+1:]
		if spaceID == "" {
			resp.Diagnostics.AddError("Invalid import ID", "expected <action_type> or <space_id>/<action_type>, got "+req.ID)
			return
		}
	}
	valid := false
	for _, t := range humanPolicyTypes {
		valid = valid || t == typ
	}
	if !valid {
		resp.Diagnostics.AddError("Invalid import ID", "expected <action_type> or <space_id>/<action_type> with a type of "+
			strings.Join(humanPolicyTypes, ", ")+", got "+req.ID)
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("space_id"), spaceID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("action_type"), typ)...)
}
