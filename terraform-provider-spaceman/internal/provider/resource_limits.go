package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/aurimasniekis/go-spaceman/terraform-provider-spaceman/internal/client"
)

// limitAttr is one overridable limit of the limits model (ADR-0036): its key in SpaceMan is its attribute name.
type limitAttr struct {
	key  string
	desc string
}

// spaceLimitAttrs are the limits a space can override through PUT /spaces/{id}/limits. The top-level task limit is
// not among them: it lives in the queue policy (spaceman_space_queue_policy). Bounds are SpaceMan's
// (config.LimitRanges); SpaceMan checks them.
var spaceLimitAttrs = []limitAttr{
	{"max_concurrent_runs", "Worker and diagnostic runs active in the space at once (1-256)."},
	{"max_kb_sessions", "Active KB support sessions in the space, across its knowledge bases (1-64; each KB also has its own `max_sessions`)."},
	{"support_depth", "How deep support may nest (1-3; 1: a KB session cannot ask a knowledge base)."},
	{"max_human_pending", "Open human actions in the space (1-5000)."},
	{"max_human_pending_per_run", "Open human actions one agent run asked (1-500)."},
	{"max_tool_calls_per_run", "SpaceMan and MCP gateway tool calls of one run (0-1000000; 0: no limit)."},
	{"ingest_per_minute", "Events one subscription ingests per minute; the rest waits in the stream (1-60000)."},
	{"task_deadline_minutes", "How long a top-level task may run before control and the operators are told (0-43200; 0: none; never stops a task)."},
	{"storage_quota_bytes", "Attachment bytes stored for the space (1 MiB-1 TiB)."},
	{"budget_tokens_24h", "Reported tokens of runs started in the last 24 hours (0: no budget). Usage-based and reported late: never a hard limit."},
	{"budget_cost_cents_24h", "Harness cost estimates of runs started in the last 24 hours, in US cents (0: no budget). Estimates, reported late."},
	{"budget_tokens_per_run", "Reported tokens of one run (0: no budget; warning only, never stops a run)."},
	{"budget_warn_percent", "Share of a budget at which operators are warned (1-100)."},
	{"recording_quota_bytes", "Compressed size of one run's terminal recording on the node (1 MiB-64 GiB); new runs get the new size."},
	{"export_max_bytes", "Size of one space export archive (1 MiB-64 GiB); a larger export is refused."},
	{"retention_recordings_days", "Days central keeps an ended run's terminal recording (0-36500; 0: kept). Changing it needs `retention:write` (space admins); a shorter period deletes older recordings in the next daily retention pass."},
	{"retention_transcripts_days", "Days central keeps a run's stored native transcript (0-36500; 0: kept). Needs `retention:write`."},
	{"retention_attachments_days", "Days an attachment is kept once every item using it is finished (0-36500; 0: kept); never deleted while an open item uses it. Needs `retention:write`."},
	{"retention_human_actions_days", "Days a resolved, cancelled or expired human action is kept (0-36500; 0: kept); kept while open work refers to it. Needs `retention:write`."},
}

// ---- space limits ----

type spaceLimitsResource struct{ base }

func newSpaceLimitsResource() resource.Resource { return &spaceLimitsResource{} }

// importKey marks, in private state, a resource that was just imported: its first read takes ownership of every
// override stored in SpaceMan (there is no configuration to tell which ones are Terraform's).
const importKey = "imported"

func (r *spaceLimitsResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_space_limits"
}

func (r *spaceLimitsResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	attrs := map[string]schema.Attribute{
		"id":       idAttr("The space ID."),
		"space_id": requiredReplace("The space."),
		"on_lower": schema.StringAttribute{Optional: true, Validators: []validator.String{stringvalidator.OneOf("drain", "stop")},
			MarkdownDescription: "What lowering `max_concurrent_runs` does to running runs: `drain` (SpaceMan's default) lets them finish, `stop` stops " +
				"the newest runs beyond the new limit. Left out, the value in SpaceMan is kept; removing it from the configuration resets it to `drain`."},
	}
	for _, l := range spaceLimitAttrs {
		attrs[l.key] = schema.Int64Attribute{Optional: true, Validators: []validator.Int64{int64validator.AtLeast(0)},
			MarkdownDescription: l.desc + " Unset: not managed (the installation default or an override set elsewhere applies)."}
	}
	resp.Schema = schema.Schema{
		MarkdownDescription: "A space's limit overrides (`GET/PUT /spaces/{id}/limits`, SM-LIMIT-001). Terraform owns only the limits the configuration " +
			"sets: an override set in the UI for another limit is kept and is not drift, and removing an attribute clears that override (the " +
			"installation default applies again). Destroying the resource clears the overrides it owns. Updates use `If-Match` with the version " +
			"read at refresh. Lowering a limit never stops running work, except `max_concurrent_runs` with `on_lower = \"stop\"`. Use one resource " +
			"per space. The effective values are in the `spaceman_space_limits` data source; the top-level task limit is in " +
			"`spaceman_space_queue_policy`.",
		Attributes: attrs,
	}
}

// limitsState is the resource's state: the owned overrides (null when not owned) and on_lower.
type limitsState struct {
	spaceID string
	values  map[string]types.Int64
	onLower types.String
}

func readLimitsState(ctx context.Context, get func(context.Context, path.Path, any) diag.Diagnostics, d *diag.Diagnostics) limitsState {
	s := limitsState{values: map[string]types.Int64{}}
	var id types.String
	d.Append(get(ctx, path.Root("space_id"), &id)...)
	s.spaceID = id.ValueString()
	d.Append(get(ctx, path.Root("on_lower"), &s.onLower)...)
	for _, l := range spaceLimitAttrs {
		var v types.Int64
		d.Append(get(ctx, path.Root(l.key), &v)...)
		s.values[l.key] = v
	}
	return s
}

func writeLimitsState(ctx context.Context, st *tfsdk.State, s limitsState, d *diag.Diagnostics) {
	d.Append(st.SetAttribute(ctx, path.Root("id"), s.spaceID)...)
	d.Append(st.SetAttribute(ctx, path.Root("space_id"), s.spaceID)...)
	d.Append(st.SetAttribute(ctx, path.Root("on_lower"), s.onLower)...)
	for _, l := range spaceLimitAttrs {
		d.Append(st.SetAttribute(ctx, path.Root(l.key), s.values[l.key])...)
	}
}

// mergeLimits computes the overrides to store: the current ones, with the planned values of owned limits set and the
// limits Terraform owned before (prior) but no longer configures removed. Overrides it never owned stay.
func mergeLimits(current map[string]int64, prior, plan map[string]types.Int64) map[string]int64 {
	out := map[string]int64{}
	for k, v := range current {
		out[k] = v
	}
	for _, l := range spaceLimitAttrs {
		pv, was := plan[l.key], prior[l.key]
		switch {
		case !pv.IsNull() && !pv.IsUnknown():
			out[l.key] = pv.ValueInt64()
		case !was.IsNull() && !was.IsUnknown():
			delete(out, l.key)
		}
	}
	return out
}

// mergeOnLower is the on_lower to store: the planned one, drain when Terraform stops managing it, else the current one.
func mergeOnLower(current string, prior, plan types.String) string {
	switch {
	case !plan.IsNull() && !plan.IsUnknown():
		return plan.ValueString()
	case !prior.IsNull() && !prior.IsUnknown():
		return "drain"
	}
	return current
}

// ownedFrom refreshes the owned values from SpaceMan: an owned limit takes its stored override (null when it was
// cleared elsewhere, which is drift); with all set, every stored override becomes owned (import).
func ownedFrom(l *client.SpaceLimits, prior limitsState, all bool) limitsState {
	over := l.Overrides()
	out := limitsState{spaceID: prior.spaceID, values: map[string]types.Int64{}, onLower: types.StringNull()}
	for _, a := range spaceLimitAttrs {
		v, set := over[a.key]
		owned := all || !prior.values[a.key].IsNull()
		switch {
		case owned && set:
			out.values[a.key] = types.Int64Value(v)
		default:
			out.values[a.key] = types.Int64Null()
		}
	}
	if !prior.onLower.IsNull() || (all && l.Version > 0) {
		out.onLower = types.StringValue(l.OnLower)
	}
	return out
}

func (r *spaceLimitsResource) apply(ctx context.Context, prior, plan limitsState, version int64, d *diag.Diagnostics) (*client.SpaceLimits, bool) {
	cur, err := r.c.GetSpaceLimits(ctx, plan.spaceID)
	if err != nil {
		addAPIError(d, "Could not read the space's limits", err)
		return nil, false
	}
	if version == 0 {
		version = cur.Version
	}
	in := client.SpaceLimitsInput{Values: mergeLimits(cur.Overrides(), prior.values, plan.values), OnLower: mergeOnLower(cur.OnLower, prior.onLower, plan.onLower)}
	out, err := r.c.SetSpaceLimits(ctx, plan.spaceID, version, in)
	if err != nil {
		addAPIError(d, "Could not set the space's limits", err)
		return nil, false
	}
	return out, true
}

func (r *spaceLimitsResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	plan := readLimitsState(ctx, req.Plan.GetAttribute, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	empty := limitsState{values: map[string]types.Int64{}, onLower: types.StringNull()}
	out, ok := r.apply(ctx, empty, plan, 0, &resp.Diagnostics)
	if !ok {
		return
	}
	setVersion(ctx, resp.Private, out.Version, &resp.Diagnostics)
	writeLimitsState(ctx, &resp.State, ownedFrom(out, plan, false), &resp.Diagnostics)
}

func (r *spaceLimitsResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	prior := readLimitsState(ctx, req.State.GetAttribute, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	var imported []byte
	if req.Private != nil {
		var d diag.Diagnostics
		imported, d = req.Private.GetKey(ctx, importKey)
		resp.Diagnostics.Append(d...)
	}
	l, err := r.c.GetSpaceLimits(ctx, prior.spaceID)
	if client.IsNotFound(err) {
		resp.State.RemoveResource(ctx)
		return
	} else if err != nil {
		addAPIError(&resp.Diagnostics, "Could not read the space's limits", err)
		return
	}
	if len(imported) > 0 && resp.Private != nil {
		resp.Diagnostics.Append(resp.Private.SetKey(ctx, importKey, nil)...)
	}
	setVersion(ctx, resp.Private, l.Version, &resp.Diagnostics)
	writeLimitsState(ctx, &resp.State, ownedFrom(l, prior, len(imported) > 0), &resp.Diagnostics)
}

func (r *spaceLimitsResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	plan := readLimitsState(ctx, req.Plan.GetAttribute, &resp.Diagnostics)
	prior := readLimitsState(ctx, req.State.GetAttribute, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	out, ok := r.apply(ctx, prior, plan, getVersion(ctx, req.Private, &resp.Diagnostics), &resp.Diagnostics)
	if !ok {
		return
	}
	setVersion(ctx, resp.Private, out.Version, &resp.Diagnostics)
	writeLimitsState(ctx, &resp.State, ownedFrom(out, plan, false), &resp.Diagnostics)
}

func (r *spaceLimitsResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	prior := readLimitsState(ctx, req.State.GetAttribute, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	cur, err := r.c.GetSpaceLimits(ctx, prior.spaceID)
	if client.IsNotFound(err) {
		return
	} else if err != nil {
		addAPIError(&resp.Diagnostics, "Could not read the space's limits", err)
		return
	}
	if cur.Version == 0 {
		return // nothing stored
	}
	none := limitsState{values: map[string]types.Int64{}, onLower: types.StringNull()}
	in := client.SpaceLimitsInput{Values: mergeLimits(cur.Overrides(), prior.values, none.values), OnLower: mergeOnLower(cur.OnLower, prior.onLower, none.onLower)}
	if _, err := r.c.SetSpaceLimits(ctx, prior.spaceID, cur.Version, in); err != nil && !client.IsNotFound(err) {
		addAPIError(&resp.Diagnostics, "Could not clear the space's limit overrides", err)
	}
}

func (r *spaceLimitsResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("space_id"), req.ID)...)
	if resp.Private != nil {
		resp.Diagnostics.Append(resp.Private.SetKey(ctx, importKey, []byte("true"))...)
	}
}

// ---- queue policy ----

type queuePolicyResource struct{ base }

func newQueuePolicyResource() resource.Resource { return &queuePolicyResource{} }

type queuePolicyModel struct {
	ID                 types.String `tfsdk:"id"`
	SpaceID            types.String `tfsdk:"space_id"`
	MaxActive          types.Int64  `tfsdk:"max_top_level_tasks"`
	IncidentInsertion  types.String `tfsdk:"incident_insertion"`
	IncidentPreempts   types.Bool   `tfsdk:"incident_preempts"`
	EscalationReorders types.Bool   `tfsdk:"escalation_reorders"`
}

func (r *queuePolicyResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_space_queue_policy"
}

func (r *queuePolicyResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A space's queue policy (`PUT /spaces/{id}/queue/policy`), which holds the top-level task limit of the limits model. " +
			"SpaceMan has no `If-Match` for it: before writing, the provider re-reads the policy and refuses when it differs from the last refresh. " +
			"It is read from the queue listing (`GET /spaces/{id}/tasks`). SpaceMan has no API to remove a stored policy: destroying the resource " +
			"keeps the current policy and removes it from Terraform's state.",
		Attributes: map[string]schema.Attribute{
			"id":                  idAttr("The space ID."),
			"space_id":            requiredReplace("The space."),
			"max_top_level_tasks": optInt64("Top-level tasks that may run at once (1-20; default: the installation's `limits.max_top_level_tasks`)."),
			"incident_insertion": optString("Where new incident tasks go: `top` (default) or `priority`.",
				stringvalidator.OneOf("top", "priority")),
			"incident_preempts":   optBool("A new incident returns running ordinary top-level tasks to ready (default `false`)."),
			"escalation_reorders": optBool("Escalation may move a task to the top; otherwise it only raises priority (default `false`)."),
		},
	}
}

func (r *queuePolicyResource) toModel(p *client.QueuePolicy, m *queuePolicyModel) {
	m.ID = m.SpaceID
	m.MaxActive = types.Int64Value(p.MaxActive)
	m.IncidentInsertion = types.StringValue(p.IncidentInsertion)
	m.IncidentPreempts = types.BoolValue(p.IncidentPreempts)
	m.EscalationReorders = types.BoolValue(p.EscalationReorders)
}

// input is the planned policy, with fields the plan does not know taken from the current one.
func queueInput(m *queuePolicyModel, cur *client.QueuePolicy) client.QueuePolicy {
	in := *cur
	if !m.MaxActive.IsUnknown() && !m.MaxActive.IsNull() {
		in.MaxActive = m.MaxActive.ValueInt64()
	}
	if !m.IncidentInsertion.IsUnknown() && !m.IncidentInsertion.IsNull() {
		in.IncidentInsertion = m.IncidentInsertion.ValueString()
	}
	if !m.IncidentPreempts.IsUnknown() && !m.IncidentPreempts.IsNull() {
		in.IncidentPreempts = m.IncidentPreempts.ValueBool()
	}
	if !m.EscalationReorders.IsUnknown() && !m.EscalationReorders.IsNull() {
		in.EscalationReorders = m.EscalationReorders.ValueBool()
	}
	return in
}

func (r *queuePolicyResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var m queuePolicyModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	cur, err := r.c.GetQueuePolicy(ctx, m.SpaceID.ValueString())
	if err != nil {
		addAPIError(&resp.Diagnostics, "Could not read the queue policy", err)
		return
	}
	p, err := r.c.SetQueuePolicy(ctx, m.SpaceID.ValueString(), queueInput(&m, cur))
	if err != nil {
		addAPIError(&resp.Diagnostics, "Could not set the queue policy", err)
		return
	}
	r.toModel(p, &m)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *queuePolicyResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var m queuePolicyModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	p, err := r.c.GetQueuePolicy(ctx, m.SpaceID.ValueString())
	if client.IsNotFound(err) {
		resp.State.RemoveResource(ctx)
		return
	} else if err != nil {
		addAPIError(&resp.Diagnostics, "Could not read the queue policy", err)
		return
	}
	r.toModel(p, &m)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

// queueChanged reports whether the stored policy differs from what Terraform read at refresh.
func queueChanged(prior *queuePolicyModel, cur *client.QueuePolicy) bool {
	return prior.MaxActive.ValueInt64() != cur.MaxActive || prior.IncidentInsertion.ValueString() != cur.IncidentInsertion ||
		prior.IncidentPreempts.ValueBool() != cur.IncidentPreempts || prior.EscalationReorders.ValueBool() != cur.EscalationReorders
}

func (r *queuePolicyResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var m, prior queuePolicyModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &prior)...)
	if resp.Diagnostics.HasError() {
		return
	}
	cur, err := r.c.GetQueuePolicy(ctx, prior.SpaceID.ValueString())
	if err != nil {
		addAPIError(&resp.Diagnostics, "Could not read the queue policy", err)
		return
	}
	if queueChanged(&prior, cur) {
		resp.Diagnostics.AddError("The queue policy changed after the plan",
			"The space's queue policy in SpaceMan differs from what Terraform read when it planned (for example it was edited in the web UI). "+
				"Run `terraform plan` again to see the current state.")
		return
	}
	p, err := r.c.SetQueuePolicy(ctx, prior.SpaceID.ValueString(), queueInput(&m, cur))
	if err != nil {
		addAPIError(&resp.Diagnostics, "Could not set the queue policy", err)
		return
	}
	r.toModel(p, &m)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *queuePolicyResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var id types.String
	resp.Diagnostics.Append(req.State.GetAttribute(ctx, path.Root("space_id"), &id)...)
	resp.Diagnostics.AddWarning("Queue policy kept in SpaceMan",
		"SpaceMan has no API to remove a space's queue policy. Terraform removed the policy of "+id.ValueString()+" from its state only; "+
			"the current values stay in force.")
}

func (r *queuePolicyResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("space_id"), req.ID)...)
}

// ---- space limits data source helpers ----

var limitObjType = types.ObjectType{AttrTypes: map[string]attr.Type{
	"key": types.StringType, "label": types.StringType, "unit": types.StringType, "value": types.Int64Type, "default": types.Int64Type,
	"override": types.Int64Type, "source": types.StringType, "overridable": types.BoolType}}

func limitsValue(l *client.SpaceLimits) (types.List, types.Map, types.Map) {
	elems := make([]attr.Value, 0, len(l.Limits))
	values, overrides := map[string]attr.Value{}, map[string]attr.Value{}
	for _, x := range l.Limits {
		ov := types.Int64Null()
		if x.Override != nil {
			ov = types.Int64Value(*x.Override)
			overrides[x.Key] = types.Int64Value(*x.Override)
		}
		values[x.Key] = types.Int64Value(x.Value)
		elems = append(elems, types.ObjectValueMust(limitObjType.AttrTypes, map[string]attr.Value{
			"key": types.StringValue(x.Key), "label": types.StringValue(x.Label), "unit": types.StringValue(x.Unit), "value": types.Int64Value(x.Value),
			"default": types.Int64Value(x.Default), "override": ov, "source": types.StringValue(x.Source), "overridable": types.BoolValue(x.Overridable)}))
	}
	return types.ListValueMust(limitObjType, elems), types.MapValueMust(types.Int64Type, values), types.MapValueMust(types.Int64Type, overrides)
}
