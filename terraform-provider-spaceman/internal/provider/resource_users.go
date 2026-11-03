package provider

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/aurimasniekis/go-spaceman/terraform-provider-spaceman/internal/client"
)

// ---- users ----

type userResource struct{ base }

func newUserResource() resource.Resource { return &userResource{} }

type userModel struct {
	ID             types.String `tfsdk:"id"`
	Email          types.String `tfsdk:"email"`
	DisplayName    types.String `tfsdk:"display_name"`
	Enabled        types.Bool   `tfsdk:"enabled"`
	ProvisionedVia types.String `tfsdk:"provisioned_via"`
}

func (r *userResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_user"
}

func (r *userResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A SpaceMan user (`POST /users`, `POST /users/{id}/enable|disable`). Grants are `spaceman_user_grants`, linked " +
			"sign-in identities `spaceman_user_identity`. Terraform never sets passwords, issues reset tokens or link invitations, or enrolls " +
			"MFA: those are one-time secrets or the user's own steps. SpaceMan has no API to change a user's email or display name or to " +
			"delete a user: changes of those fields are refused at plan time, and destroying the resource disables the user (sessions end, " +
			"API tokens stop working) and removes it from Terraform's state. The last enabled global administrator cannot be disabled.",
		Attributes: map[string]schema.Attribute{
			"id": idAttr("The user ID (`usr_…`)."),
			"email": schema.StringAttribute{Optional: true, MarkdownDescription: "Email address, in lower case (unique). Fixed after creation.",
				Validators:    []validator.String{lowerCaseEmail{}},
				PlanModifiers: []planmodifier.String{fixedAfterCreate("A user's email")}},
			"display_name": schema.StringAttribute{Required: true, MarkdownDescription: "Display name (at most 128 bytes). Fixed after creation.",
				PlanModifiers: []planmodifier.String{fixedAfterCreate("A user's display name")}},
			"enabled":         optBool("Whether the user may sign in and use API tokens (default `true`)."),
			"provisioned_via": computedString("How the user was created: `admin`, `bootstrap` or `jit`."),
		},
	}
}

func (r *userResource) toModel(u *client.User, m *userModel) {
	m.ID = types.StringValue(u.ID)
	if u.Email == "" {
		m.Email = types.StringNull()
	} else {
		m.Email = types.StringValue(u.Email)
	}
	m.DisplayName = keepTrimmed(m.DisplayName, u.DisplayName)
	m.Enabled = types.BoolValue(u.Enabled)
	m.ProvisionedVia = types.StringValue(u.ProvisionedVia)
}

func (r *userResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var m userModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	u, err := r.c.CreateUser(ctx, client.CreateUserInput{Email: strOrEmpty(m.Email), DisplayName: m.DisplayName.ValueString()})
	if err != nil {
		addAPIError(&resp.Diagnostics, "Could not create the user", err)
		return
	}
	want := m.Enabled
	r.toModel(u, &m)
	setVersion(ctx, resp.Private, u.Version, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...) // saved first: a failed disable leaves a tainted user, not an orphan
	if !want.IsUnknown() && !want.IsNull() && want.ValueBool() != u.Enabled {
		if u, err = r.c.SetUserEnabled(ctx, u.ID, u.Version, want.ValueBool()); err != nil {
			addAPIError(&resp.Diagnostics, "Could not disable the new user", err)
			return
		}
		r.toModel(u, &m)
		setVersion(ctx, resp.Private, u.Version, &resp.Diagnostics)
		resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
	}
}

func (r *userResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var m userModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	u, err := r.c.GetUser(ctx, m.ID.ValueString())
	if client.IsNotFound(err) {
		resp.State.RemoveResource(ctx)
		return
	} else if err != nil {
		addAPIError(&resp.Diagnostics, "Could not read the user", err)
		return
	}
	r.toModel(u, &m)
	setVersion(ctx, resp.Private, u.Version, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *userResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var m, prior userModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &prior)...)
	if resp.Diagnostics.HasError() {
		return
	}
	u, err := r.c.GetUser(ctx, prior.ID.ValueString())
	if err != nil {
		addAPIError(&resp.Diagnostics, "Could not read the user", err)
		return
	}
	if v := getVersion(ctx, req.Private, &resp.Diagnostics); v != 0 && v != u.Version {
		addAPIError(&resp.Diagnostics, "Could not update the user", &client.APIError{Status: 409, Code: "conflict",
			Detail: fmt.Sprintf("the user changed (version %d, Terraform read %d)", u.Version, v), Operation: "GET /users/{user_id}"})
		return
	}
	if !m.Enabled.IsUnknown() && !m.Enabled.IsNull() && m.Enabled.ValueBool() != u.Enabled {
		if u, err = r.c.SetUserEnabled(ctx, u.ID, u.Version, m.Enabled.ValueBool()); err != nil {
			addAPIError(&resp.Diagnostics, "Could not change whether the user is enabled", err)
			return
		}
	}
	r.toModel(u, &m)
	setVersion(ctx, resp.Private, u.Version, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *userResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var m userModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	u, err := r.c.GetUser(ctx, m.ID.ValueString())
	if client.IsNotFound(err) {
		return
	} else if err != nil {
		addAPIError(&resp.Diagnostics, "Could not read the user", err)
		return
	}
	if u.Enabled {
		if _, err := r.c.SetUserEnabled(ctx, u.ID, u.Version, false); err != nil {
			addAPIError(&resp.Diagnostics, "Could not disable the user", err)
			return
		}
	}
	resp.Diagnostics.AddWarning("User disabled, not deleted",
		"SpaceMan has no API to delete a user. Terraform disabled "+u.ID+" (sessions ended, API tokens stopped working) and removed it from its "+
			"state; the account and its audit history stay in SpaceMan. Re-creating a user with the same email fails until it is imported.")
}

func (r *userResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	importPassthrough(ctx, req, resp)
}

// lowerCaseEmail refuses an address SpaceMan would store differently (it lower-cases and trims addresses), so the
// configured value always equals the stored one.
type lowerCaseEmail struct{}

func (lowerCaseEmail) Description(context.Context) string {
	return "must be a lower-case email address"
}
func (v lowerCaseEmail) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (lowerCaseEmail) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	v := req.ConfigValue.ValueString()
	if v != strings.ToLower(strings.TrimSpace(v)) || !strings.Contains(v, "@") {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid email address",
			fmt.Sprintf("%q must be an email address in lower case without surrounding spaces (SpaceMan stores addresses that way), e.g. %q.",
				v, strings.ToLower(strings.TrimSpace(v))))
	}
}

// ---- grants ----

type userGrantsResource struct{ base }

func newUserGrantsResource() resource.Resource { return &userGrantsResource{} }

var grantType = types.ObjectType{AttrTypes: map[string]attr.Type{"role": types.StringType, "domain": types.StringType}}

type grantModel struct {
	Role   types.String `tfsdk:"role"`
	Domain types.String `tfsdk:"domain"`
}

type userGrantsModel struct {
	ID     types.String `tfsdk:"id"`
	UserID types.String `tfsdk:"user_id"`
	Grants types.Set    `tfsdk:"grant"`
}

func (r *userGrantsResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_user_grants"
}

func (r *userGrantsResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A user's manual role grants (`PUT /users/{id}/grants`). This resource owns the user's whole manual grant set: a grant " +
			"added or removed in the UI shows as drift. Grants that come from OIDC group mappings (`spaceman_oidc_group_mapping`, SSO sessions " +
			"only) are not part of it and are never touched. SpaceMan ends the user's sessions when their grants change, keeps at least one " +
			"enabled global administrator, and makes local accounts with operator or admin grants enroll TOTP. Before writing, the provider " +
			"re-reads the grants and refuses when they differ from the last refresh. Destroying the resource removes every manual grant.",
		Attributes: map[string]schema.Attribute{
			"id":      idAttr("The user ID."),
			"user_id": requiredReplace("The user (`usr_…`)."),
			"grant": schema.SetNestedAttribute{Required: true, MarkdownDescription: "Role grants; an empty set removes every manual grant.",
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"role": schema.StringAttribute{Required: true, MarkdownDescription: "`viewer`, `operator` or `admin`.",
						Validators: []validator.String{stringvalidator.OneOf("viewer", "operator", "admin")}},
					"domain": schema.StringAttribute{Required: true, MarkdownDescription: "A space ID, or `*` for every space and global administration."},
				}}},
		},
	}
}

func grantsValue(gs []client.GrantInput) types.Set {
	elems := make([]attr.Value, 0, len(gs))
	for _, g := range gs {
		elems = append(elems, types.ObjectValueMust(grantType.AttrTypes, map[string]attr.Value{
			"role": types.StringValue(g.Role), "domain": types.StringValue(g.Domain)}))
	}
	return types.SetValueMust(grantType, elems)
}

func (r *userGrantsResource) grants(ctx context.Context, v types.Set, d *diagnostics) []client.GrantInput {
	var ms []grantModel
	d.Append(v.ElementsAs(ctx, &ms, false)...)
	out := make([]client.GrantInput, 0, len(ms))
	for _, m := range ms {
		out = append(out, client.GrantInput{Role: m.Role.ValueString(), Domain: m.Domain.ValueString()})
	}
	return out
}

// sameGrants compares two grant sets.
func sameGrants(a, b []client.GrantInput) bool {
	key := func(l []client.GrantInput) []string {
		out := make([]string, 0, len(l))
		for _, g := range l {
			out = append(out, g.Role+"@"+g.Domain)
		}
		sort.Strings(out)
		return out
	}
	return strings.Join(key(a), ",") == strings.Join(key(b), ",")
}

func (r *userGrantsResource) set(ctx context.Context, userID string, prior *[]client.GrantInput, want []client.GrantInput, d *diagnostics) ([]client.GrantInput, bool) {
	u, err := r.c.GetUser(ctx, userID)
	if err != nil {
		addAPIError(d, "Could not read the user", err)
		return nil, false
	}
	if prior != nil && !sameGrants(*prior, u.ManualGrants()) {
		d.AddError("The user's grants changed after the plan",
			"The user's manual grants in SpaceMan differ from what Terraform read when it planned (for example they were edited in the web UI). "+
				"Run `terraform plan` again to see the current state.")
		return nil, false
	}
	out, err := r.c.SetUserGrants(ctx, userID, u.Version, want)
	if err != nil {
		addAPIError(d, "Could not set the user's grants", err)
		return nil, false
	}
	return out.ManualGrants(), true
}

func (r *userGrantsResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var m userGrantsModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	want := r.grants(ctx, m.Grants, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	got, ok := r.set(ctx, m.UserID.ValueString(), nil, want, &resp.Diagnostics)
	if !ok {
		return
	}
	m.ID, m.Grants = m.UserID, grantsValue(got)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *userGrantsResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var m userGrantsModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	u, err := r.c.GetUser(ctx, m.UserID.ValueString())
	if client.IsNotFound(err) {
		resp.State.RemoveResource(ctx)
		return
	} else if err != nil {
		addAPIError(&resp.Diagnostics, "Could not read the user", err)
		return
	}
	m.ID, m.Grants = m.UserID, grantsValue(u.ManualGrants())
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *userGrantsResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var m, prior userGrantsModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &prior)...)
	want := r.grants(ctx, m.Grants, &resp.Diagnostics)
	before := r.grants(ctx, prior.Grants, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	got, ok := r.set(ctx, prior.UserID.ValueString(), &before, want, &resp.Diagnostics)
	if !ok {
		return
	}
	m.ID, m.Grants = m.UserID, grantsValue(got)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *userGrantsResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var m userGrantsModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	u, err := r.c.GetUser(ctx, m.UserID.ValueString())
	if client.IsNotFound(err) {
		return
	} else if err != nil {
		addAPIError(&resp.Diagnostics, "Could not read the user", err)
		return
	}
	if len(u.ManualGrants()) == 0 {
		return
	}
	if _, err := r.c.SetUserGrants(ctx, u.ID, u.Version, nil); err != nil && !client.IsNotFound(err) {
		addAPIError(&resp.Diagnostics, "Could not remove the user's grants", err)
	}
}

func (r *userGrantsResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("user_id"), req.ID)...)
}

// ---- linked identities ----

type userIdentityResource struct{ base }

func newUserIdentityResource() resource.Resource { return &userIdentityResource{} }

type userIdentityModel struct {
	ID         types.String `tfsdk:"id"`
	UserID     types.String `tfsdk:"user_id"`
	ProviderID types.String `tfsdk:"provider_id"`
	Subject    types.String `tfsdk:"subject"`
	Issuer     types.String `tfsdk:"issuer"`
}

func (r *userIdentityResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_user_identity"
}

func (r *userIdentityResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "An expected OIDC sign-in identity linked to a user (`POST/DELETE /users/{id}/identities`), so a pre-provisioned user " +
			"signs in through that provider. Identities cannot be edited: any change replaces the link. Unlinking (destroy) drops the user's " +
			"SSO-mapped grants and ends their sessions. Single-use link invitations are one-time secrets and are not managed.",
		Attributes: map[string]schema.Attribute{
			"id":          idAttr("`<user_id>/<provider_id>/<subject>`."),
			"user_id":     requiredReplace("The user (`usr_…`)."),
			"provider_id": requiredReplace("The OIDC provider (`spaceman_oidc_provider`)."),
			"subject":     requiredReplace("The provider's stable `sub` claim (with Porch, the `u_…` user ID; never an email)."),
			"issuer":      computedString("The provider's issuer the identity is recorded under."),
		},
	}
}

func (r *userIdentityResource) toModel(i *client.Identity, m *userIdentityModel) {
	m.UserID = types.StringValue(i.UserID)
	m.ProviderID = types.StringValue(i.ProviderID)
	m.Subject = types.StringValue(i.Subject)
	m.Issuer = types.StringValue(i.Issuer)
	m.ID = types.StringValue(i.UserID + "/" + i.ProviderID + "/" + i.Subject)
}

func (r *userIdentityResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var m userIdentityModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.c.LinkUserIdentity(ctx, m.UserID.ValueString(), m.ProviderID.ValueString(), m.Subject.ValueString()); err != nil {
		addAPIError(&resp.Diagnostics, "Could not link the identity", err)
		return
	}
	i, err := r.c.GetUserIdentity(ctx, m.UserID.ValueString(), m.ProviderID.ValueString(), m.Subject.ValueString())
	if err != nil {
		addAPIError(&resp.Diagnostics, "Could not read the linked identity", err)
		return
	}
	r.toModel(i, &m)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *userIdentityResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var m userIdentityModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	i, err := r.c.GetUserIdentity(ctx, m.UserID.ValueString(), m.ProviderID.ValueString(), m.Subject.ValueString())
	if client.IsNotFound(err) {
		resp.State.RemoveResource(ctx)
		return
	} else if err != nil {
		addAPIError(&resp.Diagnostics, "Could not read the linked identity", err)
		return
	}
	r.toModel(i, &m)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *userIdentityResource) Update(_ context.Context, _ resource.UpdateRequest, resp *resource.UpdateResponse) {
	resp.Diagnostics.AddError("Linked identities cannot be updated", "Every attribute of a linked identity forces a replacement; this is a provider bug.")
}

func (r *userIdentityResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var m userIdentityModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.c.UnlinkUserIdentity(ctx, m.UserID.ValueString(), m.Issuer.ValueString(), m.Subject.ValueString()); err != nil {
		addAPIError(&resp.Diagnostics, "Could not unlink the identity", err)
	}
}

// ImportState takes `<user_id>/<provider_id>/<subject>`; the subject may itself contain slashes.
func (r *userIdentityResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	p := strings.SplitN(req.ID, "/", 3)
	if len(p) != 3 || p[0] == "" || p[1] == "" || p[2] == "" {
		resp.Diagnostics.AddError("Invalid import ID", fmt.Sprintf("expected <user_id>/<provider_id>/<subject>, got %q", req.ID))
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("user_id"), p[0])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("provider_id"), p[1])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("subject"), p[2])...)
}
