package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/objectplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/aurimasniekis/go-spaceman/terraform-provider-spaceman/internal/client"
)

// ---- OIDC providers ----

type oidcProviderResource struct{ base }

func newOIDCProviderResource() resource.Resource { return &oidcProviderResource{} }

var jitType = types.ObjectType{AttrTypes: map[string]attr.Type{
	"email_domains": types.ListType{ElemType: types.StringType}, "tenants": types.ListType{ElemType: types.StringType},
	"hosted_domains": types.ListType{ElemType: types.StringType}, "groups": types.ListType{ElemType: types.StringType}}}

type jitModel struct {
	EmailDomains  types.List `tfsdk:"email_domains"`
	Tenants       types.List `tfsdk:"tenants"`
	HostedDomains types.List `tfsdk:"hosted_domains"`
	Groups        types.List `tfsdk:"groups"`
}

type oidcProviderModel struct {
	ID                        types.String `tfsdk:"id"`
	Slug                      types.String `tfsdk:"slug"`
	DisplayName               types.String `tfsdk:"display_name"`
	Issuer                    types.String `tfsdk:"issuer"`
	ClientID                  types.String `tfsdk:"client_id"`
	ClientSecretWO            types.String `tfsdk:"client_secret_wo"`
	ClientSecretWOVersion     types.Int64  `tfsdk:"client_secret_wo_version"`
	Scopes                    types.List   `tfsdk:"scopes"`
	GroupsClaim               types.String `tfsdk:"groups_claim"`
	Provisioning              types.String `tfsdk:"provisioning"`
	JIT                       types.Object `tfsdk:"jit"`
	AllowPrivateNetwork       types.Bool   `tfsdk:"allow_private_network"`
	AllowCrossOriginEndpoints types.Bool   `tfsdk:"allow_cross_origin_endpoints"`
	CABundlePEM               types.String `tfsdk:"ca_bundle_pem"`
	StepUpFallback            types.String `tfsdk:"stepup_fallback"`
	ACRValues                 types.List   `tfsdk:"acr_values"`
	Enabled                   types.Bool   `tfsdk:"enabled"`
	HasClientSecret           types.Bool   `tfsdk:"has_client_secret"`
}

func (r *oidcProviderResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_oidc_provider"
}

func (r *oidcProviderResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	strList := func(d string) schema.ListAttribute {
		return schema.ListAttribute{Optional: true, Computed: true, ElementType: types.StringType, MarkdownDescription: d}
	}
	resp.Schema = schema.Schema{
		MarkdownDescription: "An OpenID Connect sign-in provider (`/oidc/providers`). The client secret is write-only: sealed by SpaceMan, never returned " +
			"and never stored by Terraform (requires Terraform 1.11 or later). Discovery runs at the first sign-in, not when the provider is saved. " +
			"Deleting it removes its group mappings and linked identities.",
		Attributes: map[string]schema.Attribute{
			"id":           idAttr("The provider ID."),
			"slug":         schema.StringAttribute{Required: true, MarkdownDescription: "Identifier used in sign-in URLs."},
			"display_name": schema.StringAttribute{Required: true, MarkdownDescription: "Name on the sign-in page."},
			"issuer":       schema.StringAttribute{Required: true, MarkdownDescription: "The issuer URL. Changing it clears the test-login evidence."},
			"client_id":    schema.StringAttribute{Required: true, MarkdownDescription: "The OAuth client ID. Changing it clears the test-login evidence."},
			"client_secret_wo": schema.StringAttribute{Optional: true, WriteOnly: true, Sensitive: true,
				MarkdownDescription: "The client secret (write-only). Sent on create and whenever `client_secret_wo_version` changes; empty or left " +
					"out then removes the stored secret (public clients)."},
			"client_secret_wo_version": woVersion("client_secret_wo"),
			"scopes":                   optList("Requested scopes (default `openid profile email`)."),
			"groups_claim":             optString("Claim holding the user's groups; a dotted path walks nested objects (Porch: `porch.groups`)."),
			"provisioning": optString("`preprovisioned` (default: only linked users sign in) or `jit` (create users on first sign-in).",
				stringvalidator.OneOf("preprovisioned", "jit")),
			"jit": schema.SingleNestedAttribute{Optional: true, Computed: true, MarkdownDescription: "Restrictions of just-in-time provisioning.",
				PlanModifiers: []planmodifier.Object{objectplanmodifier.UseStateForUnknown()},
				Attributes: map[string]schema.Attribute{
					"email_domains":  strList("Require a verified email in one of these domains."),
					"tenants":        strList("Allowed `tid` claim values (Microsoft Entra ID)."),
					"hosted_domains": strList("Allowed `hd` claim values (Google Workspace)."),
					"groups":         strList("Require at least one of these values in the groups claim."),
				}},
			"allow_private_network":        optBool("Allow the issuer's endpoints on private addresses (default `false`)."),
			"allow_cross_origin_endpoints": optBool("Allow discovery to name endpoints on other origins than the issuer (default `false`)."),
			"ca_bundle_pem":                optString("PEM certificates trusted for the issuer's endpoints."),
			"stepup_fallback": optString("Step-up for SSO sessions when the IdP cannot re-authenticate: `none` (default) or `local_totp`.",
				stringvalidator.OneOf("none", "local_totp")),
			"acr_values":        optList("ACR values requested for step-up."),
			"enabled":           optBool("Whether the provider is offered at sign-in (default `false`)."),
			"has_client_secret": schema.BoolAttribute{Computed: true, MarkdownDescription: "Whether a client secret is stored."},
		},
	}
}

func jitValue(j client.JITRules) types.Object {
	return types.ObjectValueMust(jitType.AttrTypes, map[string]attr.Value{"email_domains": listValue(j.EmailDomains), "tenants": listValue(j.Tenants),
		"hosted_domains": listValue(j.HostedDomains), "groups": listValue(j.Groups)})
}

func (r *oidcProviderResource) toModel(p *client.OIDCProvider, m *oidcProviderModel) {
	m.ID = types.StringValue(p.ID)
	m.Slug = types.StringValue(p.Slug)
	m.DisplayName = types.StringValue(p.DisplayName)
	m.Issuer = types.StringValue(p.Issuer)
	m.ClientID = types.StringValue(p.ClientID)
	m.Scopes = listValue(p.Scopes)
	m.GroupsClaim = types.StringValue(p.GroupsClaim)
	m.Provisioning = types.StringValue(p.Provisioning)
	m.JIT = jitValue(p.JIT)
	m.AllowPrivateNetwork = types.BoolValue(p.AllowPrivateNetwork)
	m.AllowCrossOriginEndpoints = types.BoolValue(p.AllowCrossOriginEndpoints)
	m.CABundlePEM = types.StringValue(p.CABundlePEM)
	m.StepUpFallback = types.StringValue(p.StepUpFallback)
	m.ACRValues = listValue(p.ACRValues)
	m.Enabled = types.BoolValue(p.Enabled)
	m.HasClientSecret = types.BoolValue(p.HasClientSecret)
	m.ClientSecretWO = types.StringNull()
}

func listPtr(ctx context.Context, v types.List, diags *diagnostics) *[]string {
	if v.IsNull() || v.IsUnknown() {
		return nil
	}
	l := stringList(ctx, v, diags)
	return &l
}

// input is the provider's fields that are known in the plan: the API leaves the others unchanged (PATCH).
func (r *oidcProviderResource) input(ctx context.Context, m *oidcProviderModel, diags *diagnostics) client.OIDCProviderInput {
	in := client.OIDCProviderInput{Slug: strPtr(m.Slug), DisplayName: strPtr(m.DisplayName), Issuer: strPtr(m.Issuer), ClientID: strPtr(m.ClientID),
		Scopes: listPtr(ctx, m.Scopes, diags), GroupsClaim: strPtr(m.GroupsClaim), Provisioning: strPtr(m.Provisioning),
		AllowPrivateNetwork: boolPtr(m.AllowPrivateNetwork), AllowCrossOriginEndpoints: boolPtr(m.AllowCrossOriginEndpoints), CABundlePEM: strPtr(m.CABundlePEM),
		StepUpFallback: strPtr(m.StepUpFallback), ACRValues: listPtr(ctx, m.ACRValues, diags), Enabled: boolPtr(m.Enabled)}
	if !m.JIT.IsNull() && !m.JIT.IsUnknown() {
		var j jitModel
		diags.Append(m.JIT.As(ctx, &j, objectAsOptions)...)
		in.JIT = &client.JITRules{EmailDomains: orEmpty(stringList(ctx, j.EmailDomains, diags)), Tenants: orEmpty(stringList(ctx, j.Tenants, diags)),
			HostedDomains: orEmpty(stringList(ctx, j.HostedDomains, diags)), Groups: orEmpty(stringList(ctx, j.Groups, diags))}
	}
	return in
}

func orEmpty(l []string) []string {
	if l == nil {
		return []string{}
	}
	return l
}

func (r *oidcProviderResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var m oidcProviderModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	var secret types.String
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("client_secret_wo"), &secret)...)
	in := r.input(ctx, &m, &resp.Diagnostics)
	in.ClientSecret = strPtr(secret)
	if resp.Diagnostics.HasError() {
		return
	}
	p, err := r.c.CreateOIDCProvider(ctx, in)
	if err != nil {
		addAPIError(&resp.Diagnostics, "Could not create the OIDC provider", err)
		return
	}
	r.toModel(p, &m)
	setVersion(ctx, resp.Private, p.Version, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *oidcProviderResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var m oidcProviderModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	p, err := r.c.GetOIDCProvider(ctx, m.ID.ValueString())
	if client.IsNotFound(err) {
		resp.State.RemoveResource(ctx)
		return
	} else if err != nil {
		addAPIError(&resp.Diagnostics, "Could not read the OIDC provider", err)
		return
	}
	r.toModel(p, &m)
	setVersion(ctx, resp.Private, p.Version, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *oidcProviderResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var m, prior oidcProviderModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &prior)...)
	in := r.input(ctx, &m, &resp.Diagnostics)
	if !m.ClientSecretWOVersion.Equal(prior.ClientSecretWOVersion) {
		var secret types.String
		resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("client_secret_wo"), &secret)...)
		s := strOrEmpty(secret) // empty removes the stored secret
		in.ClientSecret = &s
	}
	if resp.Diagnostics.HasError() {
		return
	}
	v := getVersion(ctx, req.Private, &resp.Diagnostics)
	if v == 0 {
		cur, err := r.c.GetOIDCProvider(ctx, prior.ID.ValueString())
		if err != nil {
			addAPIError(&resp.Diagnostics, "Could not read the OIDC provider", err)
			return
		}
		v = cur.Version
	}
	p, err := r.c.UpdateOIDCProvider(ctx, prior.ID.ValueString(), v, in)
	if err != nil {
		addAPIError(&resp.Diagnostics, "Could not update the OIDC provider", err)
		return
	}
	r.toModel(p, &m)
	setVersion(ctx, resp.Private, p.Version, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *oidcProviderResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var m oidcProviderModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.c.DeleteOIDCProvider(ctx, m.ID.ValueString()); err != nil {
		addAPIError(&resp.Diagnostics, "Could not delete the OIDC provider", err)
	}
}

func (r *oidcProviderResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	importPassthrough(ctx, req, resp)
}

// ---- group mappings ----

type oidcMappingResource struct{ base }

func newOIDCGroupMappingResource() resource.Resource { return &oidcMappingResource{} }

type oidcMappingModel struct {
	ID         types.String `tfsdk:"id"`
	ProviderID types.String `tfsdk:"provider_id"`
	GroupValue types.String `tfsdk:"group_value"`
	Role       types.String `tfsdk:"role"`
	Domain     types.String `tfsdk:"domain"`
}

func (r *oidcMappingResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_oidc_group_mapping"
}

func (r *oidcMappingResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	role := requiredReplace("`viewer`, `operator` or `admin`.")
	role.Validators = []validatorString{stringvalidator.OneOf("viewer", "operator", "admin")}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Maps a groups-claim value of an OIDC provider to a role in a domain (`POST/DELETE /oidc/providers/{id}/mappings`). Applied at " +
			"every SSO sign-in; SSO grants apply only to SSO sessions. Mappings cannot be edited: any change replaces the mapping.",
		Attributes: map[string]schema.Attribute{
			"id":          idAttr("The mapping ID."),
			"provider_id": requiredReplace("The OIDC provider."),
			"group_value": requiredReplace("The groups-claim value."),
			"role":        role,
			"domain":      requiredReplace("A space ID, or `*` for every space and global administration."),
		},
	}
}

func (r *oidcMappingResource) toModel(mp *client.OIDCMapping, m *oidcMappingModel) {
	m.ID = types.StringValue(mp.ID)
	m.ProviderID = types.StringValue(mp.ProviderID)
	m.GroupValue = types.StringValue(mp.GroupValue)
	m.Role = types.StringValue(mp.Role)
	m.Domain = types.StringValue(mp.Domain)
}

func (r *oidcMappingResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var m oidcMappingModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	mp, err := r.c.AddOIDCMapping(ctx, m.ProviderID.ValueString(), client.OIDCMappingInput{GroupValue: m.GroupValue.ValueString(), Role: m.Role.ValueString(),
		Domain: m.Domain.ValueString()})
	if err != nil {
		addAPIError(&resp.Diagnostics, "Could not add the group mapping", err)
		return
	}
	r.toModel(mp, &m)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *oidcMappingResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var m oidcMappingModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	mp, err := r.c.GetOIDCMapping(ctx, m.ProviderID.ValueString(), m.ID.ValueString())
	if client.IsNotFound(err) {
		resp.State.RemoveResource(ctx)
		return
	} else if err != nil {
		addAPIError(&resp.Diagnostics, "Could not read the group mapping", err)
		return
	}
	r.toModel(mp, &m)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *oidcMappingResource) Update(_ context.Context, _ resource.UpdateRequest, resp *resource.UpdateResponse) {
	resp.Diagnostics.AddError("Group mappings cannot be updated", "Every attribute of a mapping forces a replacement; this is a provider bug.")
}

func (r *oidcMappingResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var m oidcMappingModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.c.DeleteOIDCMapping(ctx, m.ProviderID.ValueString(), m.ID.ValueString()); err != nil {
		addAPIError(&resp.Diagnostics, "Could not remove the group mapping", err)
	}
}

func (r *oidcMappingResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	p := splitImportID(req.ID, 2, "<provider_id>/<mapping_id>", &resp.Diagnostics)
	if p == nil {
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("provider_id"), p[0])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), p[1])...)
}
