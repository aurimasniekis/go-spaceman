package provider

import (
	"context"
	"fmt"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/datasourcevalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/aurimasniekis/go-spaceman/terraform-provider-spaceman/internal/client"
)

// ---- effective limits of a space ----

type spaceLimitsDataSource struct{ dsBase }

func newSpaceLimitsDataSource() datasource.DataSource { return &spaceLimitsDataSource{} }

type spaceLimitsDSModel struct {
	SpaceID   types.String `tfsdk:"space_id"`
	Values    types.Map    `tfsdk:"values"`
	Overrides types.Map    `tfsdk:"overrides"`
	OnLower   types.String `tfsdk:"on_lower"`
	Limits    types.List   `tfsdk:"limits"`
}

func (d *spaceLimitsDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_space_limits"
}

func (d *spaceLimitsDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	c := func(desc string) schema.StringAttribute {
		return schema.StringAttribute{Computed: true, MarkdownDescription: desc}
	}
	resp.Schema = schema.Schema{
		MarkdownDescription: "A space's effective limits (`GET /spaces/{id}/limits`): every limit's value in force, the installation default, the " +
			"space's own override and where the value comes from (`installation`, `space` or `queue_policy`). Budgets are usage-based and " +
			"reported late, never a hard currency limit.",
		Attributes: map[string]schema.Attribute{
			"space_id": schema.StringAttribute{Required: true, MarkdownDescription: "The space."},
			"values":   schema.MapAttribute{Computed: true, ElementType: types.Int64Type, MarkdownDescription: "The effective value of every limit, by key."},
			"overrides": schema.MapAttribute{Computed: true, ElementType: types.Int64Type,
				MarkdownDescription: "The space's own overrides, by key (set in the UI, the API or `spaceman_space_limits`)."},
			"on_lower": c("What lowering the concurrent-runs limit does to running runs: `drain` or `stop`."),
			"limits": schema.ListNestedAttribute{Computed: true, MarkdownDescription: "Every limit, in SpaceMan's display order.",
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"key":         c("The limit key (the attribute of `spaceman_space_limits`)."),
					"label":       c("Display name."),
					"unit":        c("`count`, `bytes`, `per_minute`, `minutes`, `tokens`, `usd_cents`, `percent` or `depth`."),
					"value":       schema.Int64Attribute{Computed: true, MarkdownDescription: "The value in force."},
					"default":     schema.Int64Attribute{Computed: true, MarkdownDescription: "The installation default."},
					"override":    schema.Int64Attribute{Computed: true, MarkdownDescription: "The space's own value; null when it sets none."},
					"source":      c("Where the value comes from: `installation`, `space` or `queue_policy`."),
					"overridable": schema.BoolAttribute{Computed: true, MarkdownDescription: "Whether `spaceman_space_limits` can override it."},
				}}},
		},
	}
}

func (d *spaceLimitsDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var m spaceLimitsDSModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	l, err := d.c.GetSpaceLimits(ctx, m.SpaceID.ValueString())
	if err != nil {
		addAPIError(&resp.Diagnostics, "Could not read the space's limits", err)
		return
	}
	m.Limits, m.Values, m.Overrides = limitsValue(l)
	m.OnLower = types.StringValue(l.OnLower)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

// ---- a user ----

type userDataSource struct{ dsBase }

func newUserDataSource() datasource.DataSource { return &userDataSource{} }

type userDSModel struct {
	ID             types.String `tfsdk:"id"`
	Email          types.String `tfsdk:"email"`
	DisplayName    types.String `tfsdk:"display_name"`
	Enabled        types.Bool   `tfsdk:"enabled"`
	ProvisionedVia types.String `tfsdk:"provisioned_via"`
	Grants         types.List   `tfsdk:"grants"`
}

var userGrantObjType = types.ObjectType{AttrTypes: map[string]attr.Type{"role": types.StringType, "domain": types.StringType, "source": types.StringType}}

func (d *userDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_user"
}

func (d *userDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	c := func(desc string) schema.StringAttribute {
		return schema.StringAttribute{Computed: true, MarkdownDescription: desc}
	}
	resp.Schema = schema.Schema{
		MarkdownDescription: "A user, by ID or email (`GET /users`), for example one created by OIDC just-in-time provisioning whose grants " +
			"`spaceman_user_grants` manages, or an escalation assignee of `spaceman_human_action_policy`. Nothing secret is read.",
		Attributes: map[string]schema.Attribute{
			"id":              schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The user ID (`usr_…`). Give `id` or `email`."},
			"email":           schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The user's email (case-insensitive). Give `id` or `email`."},
			"display_name":    c("Display name."),
			"enabled":         schema.BoolAttribute{Computed: true, MarkdownDescription: "Whether the user may sign in."},
			"provisioned_via": c("`admin`, `bootstrap` or `jit`."),
			"grants": schema.ListNestedAttribute{Computed: true, MarkdownDescription: "Every grant: manual ones and those from OIDC group mappings.",
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"role":   c("`viewer`, `operator` or `admin`."),
					"domain": c("A space ID, or `*`."),
					"source": c("`manual` or `sso` (applies to SSO sessions only)."),
				}}},
		},
	}
}

func (d *userDataSource) ConfigValidators(context.Context) []datasource.ConfigValidator {
	return []datasource.ConfigValidator{datasourcevalidator.ExactlyOneOf(path.MatchRoot("id"), path.MatchRoot("email"))}
}

func (d *userDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var m userDSModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	var u *client.User
	var err error
	if id := strOrEmpty(m.ID); id != "" {
		u, err = d.c.GetUser(ctx, id)
	} else {
		var l []*client.User
		if l, err = d.c.ListUsers(ctx); err == nil {
			for _, x := range l {
				if strings.EqualFold(x.Email, strings.TrimSpace(m.Email.ValueString())) {
					u = x
				}
			}
			if u == nil {
				resp.Diagnostics.AddError("User not found", fmt.Sprintf("no user has the email %q", m.Email.ValueString()))
				return
			}
		}
	}
	if err != nil {
		addAPIError(&resp.Diagnostics, "Could not read the user", err)
		return
	}
	grants := make([]attr.Value, 0, len(u.Grants)+len(u.SSOGrants))
	for _, g := range append(append([]client.Grant(nil), u.Grants...), u.SSOGrants...) {
		src := g.Source
		if src == "" {
			src = "manual"
		}
		grants = append(grants, types.ObjectValueMust(userGrantObjType.AttrTypes, map[string]attr.Value{
			"role": types.StringValue(g.Role), "domain": types.StringValue(g.Domain), "source": types.StringValue(src)}))
	}
	m = userDSModel{ID: types.StringValue(u.ID), Email: types.StringValue(u.Email), DisplayName: types.StringValue(u.DisplayName),
		Enabled: types.BoolValue(u.Enabled), ProvisionedVia: types.StringValue(u.ProvisionedVia), Grants: types.ListValueMust(userGrantObjType, grants)}
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}
