package provider

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/aurimasniekis/go-spaceman/terraform-provider-spaceman/internal/client"
)

type scheduleResource struct{ base }

func newScheduleResource() resource.Resource { return &scheduleResource{} }

type scheduleModel struct {
	ID              types.String `tfsdk:"id"`
	SpaceID         types.String `tfsdk:"space_id"`
	Name            types.String `tfsdk:"name"`
	Description     types.String `tfsdk:"description"`
	Enabled         types.Bool   `tfsdk:"enabled"`
	Kind            types.String `tfsdk:"kind"`
	Cron            types.String `tfsdk:"cron"`
	IntervalSeconds types.Int64  `tfsdk:"interval_seconds"`
	Anchor          types.String `tfsdk:"anchor"`
	Timezone        types.String `tfsdk:"timezone"`
	Missed          types.String `tfsdk:"missed"`
	CatchUpMax      types.Int64  `tfsdk:"catch_up_max"`
	SkipIfOpen      types.Bool   `tfsdk:"skip_if_open"`
	Target          types.String `tfsdk:"target"`
	Title           types.String `tfsdk:"title"`
	Body            types.String `tfsdk:"body"`
	Priority        types.String `tfsdk:"priority"`
	AssigneeRole    types.String `tfsdk:"assignee_role"`
}

func (r *scheduleResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_schedule"
}

func (r *scheduleResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	str := func(v ...string) []validator.String { return []validator.String{stringvalidator.OneOf(v...)} }
	resp.Schema = schema.Schema{
		MarkdownDescription: "A space schedule (`/spaces/{id}/schedules`, ADR-0036): at each cron or interval occurrence it creates an inbox item or a " +
			"ready task in the space. Occurrences, their outcomes and what they created are runtime state and are not managed. A changed timing " +
			"restarts the schedule from now; occurrences before the change are not treated as missed. Destroying the resource deletes the " +
			"schedule; what it already created stays.",
		Attributes: map[string]schema.Attribute{
			"id":          idAttr("The schedule ID."),
			"space_id":    requiredReplace("The space (not archived)."),
			"name":        schema.StringAttribute{Required: true, MarkdownDescription: "Unique name in the space (at most 80 characters)."},
			"description": optString("Description."),
			"enabled":     optBool("Whether occurrences run (default `true`)."),
			"kind":        schema.StringAttribute{Required: true, MarkdownDescription: "`cron` or `interval`.", Validators: str("cron", "interval")},
			"cron": schema.StringAttribute{Optional: true, Computed: true, Default: stringDefaultEmpty,
				MarkdownDescription: "`cron`: a five-field expression or `@hourly`, `@daily`, `@weekly`, `@monthly`, `@yearly`, in `timezone` " +
					"(`@every` and `TZ=` are refused). Empty for interval schedules."},
			"interval_seconds": schema.Int64Attribute{Optional: true, Computed: true, Default: int64default.StaticInt64(0),
				MarkdownDescription: "`interval`: seconds between occurrences (60 s to 366 days). 0 for cron schedules."},
			"anchor": schema.StringAttribute{Optional: true, Computed: true,
				MarkdownDescription: "`interval`: RFC 3339 time occurrences are counted from (anchor + n × interval). Left out, SpaceMan sets it to the " +
					"creation time and keeps it while `kind` and `interval_seconds` stay the same.",
				Validators:    []validator.String{rfc3339Validator{}},
				PlanModifiers: []planmodifier.String{anchorPlanModifier{}}},
			"timezone": optString("IANA time zone of the cron expression (default `UTC`)."),
			"missed": optString("Occurrences missed while central was stopped or the space archived: `skip` (default), `run_once` or `catch_up`.",
				stringvalidator.OneOf("skip", "run_once", "catch_up")),
			"catch_up_max":  optInt64("`catch_up`: how many missed occurrences run at most (1-100, default 3)."),
			"skip_if_open":  optBool("Skip an occurrence while what the previous one created is still open (default `true`)."),
			"target":        schema.StringAttribute{Required: true, MarkdownDescription: "`inbox` (an inbox item) or `task` (a ready task).", Validators: str("inbox", "task")},
			"title":         schema.StringAttribute{Required: true, MarkdownDescription: "Title of the item or task (at most 200 characters)."},
			"body":          optString("Markdown: the inbox item body or the task objective."),
			"priority":      optString("Inbox urgency or task priority: `low`, `normal` (default), `high` or `urgent`.", stringvalidator.OneOf(priorities...)),
			"assignee_role": optString("`task` only: the role the task is assigned to; set it to `\"\"` before changing `target` to `inbox`."),
		},
	}
}

// rfc3339Validator checks an RFC 3339 time.
type rfc3339Validator struct{}

func (rfc3339Validator) Description(context.Context) string { return "must be an RFC 3339 time" }
func (v rfc3339Validator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (rfc3339Validator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if _, err := time.Parse(time.RFC3339, req.ConfigValue.ValueString()); err != nil {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid time", fmt.Sprintf("%q is not an RFC 3339 time such as 2026-01-01T09:00:00Z.", req.ConfigValue.ValueString()))
	}
}

// anchorPlanModifier keeps an unconfigured anchor while SpaceMan keeps it (same kind and interval) and leaves it
// unknown otherwise (SpaceMan then sets it to the time of the update).
type anchorPlanModifier struct{}

func (anchorPlanModifier) Description(context.Context) string {
	return "Keeps the anchor while the kind and interval stay the same."
}
func (m anchorPlanModifier) MarkdownDescription(ctx context.Context) string {
	return m.Description(ctx)
}

func (anchorPlanModifier) PlanModifyString(ctx context.Context, req planmodifier.StringRequest, resp *planmodifier.StringResponse) {
	if req.State.Raw.IsNull() || req.Plan.Raw.IsNull() || !req.ConfigValue.IsNull() || req.StateValue.IsNull() {
		return
	}
	var pk, sk types.String
	var pi, si types.Int64
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("kind"), &pk)...)
	resp.Diagnostics.Append(req.State.GetAttribute(ctx, path.Root("kind"), &sk)...)
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("interval_seconds"), &pi)...)
	resp.Diagnostics.Append(req.State.GetAttribute(ctx, path.Root("interval_seconds"), &si)...)
	if pk.Equal(sk) && pi.Equal(si) {
		resp.PlanValue = req.StateValue
	}
}

// sameInstant reports whether two RFC 3339 strings name the same time.
func sameInstant(a string, b time.Time) bool {
	t, err := time.Parse(time.RFC3339, a)
	return err == nil && t.Equal(b)
}

// keepTrimmed keeps the prior (configured) text when SpaceMan stored it with surrounding whitespace removed.
func keepTrimmed(prior types.String, server string) types.String {
	if !prior.IsNull() && !prior.IsUnknown() && strings.TrimSpace(prior.ValueString()) == server {
		return prior
	}
	return types.StringValue(server)
}

func (r *scheduleResource) toModel(s *client.Schedule, m *scheduleModel) {
	m.ID = types.StringValue(s.ID)
	m.SpaceID = types.StringValue(s.SpaceID)
	m.Name = keepTrimmed(m.Name, s.Name)
	m.Description = types.StringValue(s.Description)
	m.Enabled = types.BoolValue(s.Enabled)
	m.Kind = types.StringValue(s.Kind)
	m.Cron = keepTrimmed(m.Cron, s.Cron)
	m.IntervalSeconds = types.Int64Value(s.IntervalSeconds)
	if m.Anchor.IsNull() || m.Anchor.IsUnknown() || !sameInstant(m.Anchor.ValueString(), s.Anchor) {
		m.Anchor = types.StringValue(s.Anchor.UTC().Format(time.RFC3339Nano))
	}
	m.Timezone = types.StringValue(s.Timezone)
	m.Missed = types.StringValue(s.Missed)
	m.CatchUpMax = types.Int64Value(s.CatchUpMax)
	m.SkipIfOpen = types.BoolValue(s.SkipIfOpen)
	m.Target = types.StringValue(s.Target)
	m.Title = keepTrimmed(m.Title, s.Title)
	m.Body = types.StringValue(s.Body)
	m.Priority = types.StringValue(s.Priority)
	m.AssigneeRole = types.StringValue(s.AssigneeRole)
}

// input is the planned schedule. anchor is sent only when it is known (configured, or kept from the state).
func (r *scheduleResource) input(m *scheduleModel) client.ScheduleInput {
	in := client.ScheduleInput{Name: m.Name.ValueString(), Description: strOrEmpty(m.Description), Enabled: boolPtr(m.Enabled), Kind: m.Kind.ValueString(),
		Cron: strOrEmpty(m.Cron), IntervalSeconds: int64OrZero(m.IntervalSeconds), Timezone: strOrEmpty(m.Timezone), Missed: strOrEmpty(m.Missed),
		CatchUpMax: int64OrZero(m.CatchUpMax), SkipIfOpen: boolPtr(m.SkipIfOpen), Target: m.Target.ValueString(), Title: m.Title.ValueString(),
		Body: strOrEmpty(m.Body), Priority: strOrEmpty(m.Priority), AssigneeRole: strOrEmpty(m.AssigneeRole)}
	if a := strOrEmpty(m.Anchor); a != "" {
		if t, err := time.Parse(time.RFC3339, a); err == nil {
			in.Anchor = &t
		}
	}
	return in
}

func (r *scheduleResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var m scheduleModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	s, err := r.c.CreateSchedule(ctx, m.SpaceID.ValueString(), r.input(&m))
	if err != nil {
		addAPIError(&resp.Diagnostics, "Could not create the schedule", err)
		return
	}
	r.toModel(s, &m)
	setVersion(ctx, resp.Private, s.Version, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *scheduleResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var m scheduleModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	s, err := r.c.GetSchedule(ctx, m.SpaceID.ValueString(), m.ID.ValueString())
	if client.IsNotFound(err) {
		resp.State.RemoveResource(ctx)
		return
	} else if err != nil {
		addAPIError(&resp.Diagnostics, "Could not read the schedule", err)
		return
	}
	r.toModel(s, &m)
	setVersion(ctx, resp.Private, s.Version, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *scheduleResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var m, prior scheduleModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &prior)...)
	if resp.Diagnostics.HasError() {
		return
	}
	v := getVersion(ctx, req.Private, &resp.Diagnostics)
	if v == 0 {
		cur, err := r.c.GetSchedule(ctx, prior.SpaceID.ValueString(), prior.ID.ValueString())
		if err != nil {
			addAPIError(&resp.Diagnostics, "Could not read the schedule", err)
			return
		}
		v = cur.Version
	}
	s, err := r.c.UpdateSchedule(ctx, prior.SpaceID.ValueString(), prior.ID.ValueString(), v, r.input(&m))
	if err != nil {
		addAPIError(&resp.Diagnostics, "Could not update the schedule", err)
		return
	}
	r.toModel(s, &m)
	setVersion(ctx, resp.Private, s.Version, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *scheduleResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var m scheduleModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.c.DeleteSchedule(ctx, m.SpaceID.ValueString(), m.ID.ValueString()); err != nil {
		addAPIError(&resp.Diagnostics, "Could not delete the schedule", err)
	}
}

func (r *scheduleResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	p := splitImportID(req.ID, 2, "<space_id>/<schedule_id>", &resp.Diagnostics)
	if p == nil {
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("space_id"), p[0])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), p[1])...)
}
