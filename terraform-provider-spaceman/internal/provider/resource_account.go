package provider

import (
	"context"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/aurimasniekis/go-spaceman/terraform-provider-spaceman/internal/client"
)

func optBool(desc string) schema.BoolAttribute {
	return schema.BoolAttribute{Optional: true, Computed: true, MarkdownDescription: desc,
		PlanModifiers: []planmodifier.Bool{boolplanmodifier.UseStateForUnknown()}}
}

func optInt64(desc string) schema.Int64Attribute {
	return schema.Int64Attribute{Optional: true, Computed: true, MarkdownDescription: desc,
		PlanModifiers: []planmodifier.Int64{int64planmodifier.UseStateForUnknown()}}
}

// woVersion is the companion of a write-only secret: Terraform never stores the secret, so a change of this number
// is what tells the provider to send (rotate) it.
func woVersion(secret string) schema.Int64Attribute {
	return schema.Int64Attribute{Optional: true,
		MarkdownDescription: "Change this number to send `" + secret + "` again (rotation). Terraform never stores the write-only value, so it cannot " +
			"see when the value itself changes."}
}

type accountResource struct{ base }

func newAccountResource() resource.Resource { return &accountResource{} }

type accountModel struct {
	ID              types.String `tfsdk:"id"`
	Name            types.String `tfsdk:"name"`
	Kind            types.String `tfsdk:"kind"`
	Description     types.String `tfsdk:"description"`
	Metadata        types.Map    `tfsdk:"metadata"`
	Enabled         types.Bool   `tfsdk:"enabled"`
	SecretWO        types.String `tfsdk:"secret_wo"`
	SecretWOVersion types.Int64  `tfsdk:"secret_wo_version"`
	Harness         types.String `tfsdk:"harness"`
	SecretVersion   types.Int64  `tfsdk:"secret_version"`
	SecretRotatedAt types.String `tfsdk:"secret_rotated_at"`
}

func (r *accountResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_account"
}

func (r *accountResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A harness account (`/accounts`): a sealed credential that runs of a harness use. The secret is write-only: it is sent to " +
			"SpaceMan, sealed there and never returned, and Terraform never stores it (requires Terraform 1.11 or later).",
		Attributes: map[string]schema.Attribute{
			"id":   idAttr("The account ID."),
			"name": schema.StringAttribute{Required: true, MarkdownDescription: "Unique name (at most 63 characters)."},
			"kind": schema.StringAttribute{Required: true, MarkdownDescription: "`claude_oauth_token`, `anthropic_api_key` or `openai_api_key`. Changing it replaces the account (its bindings go with it).",
				Validators:    []validatorString{stringvalidator.OneOf("claude_oauth_token", "anthropic_api_key", "openai_api_key")},
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()}},
			"description": optString("Description."),
			"metadata":    optMap("Free-form labels."),
			"enabled":     optBool("Whether runs may use the account (default `true`)."),
			"secret_wo": schema.StringAttribute{Required: true, WriteOnly: true, Sensitive: true,
				MarkdownDescription: "The credential (write-only). Sent on create and whenever `secret_wo_version` changes."},
			"secret_wo_version": woVersion("secret_wo"),
			"harness":           computedString("The harness the kind is for (`claude-code` or `codex`)."),
			"secret_version":    schema.Int64Attribute{Computed: true, MarkdownDescription: "SpaceMan's count of stored secrets (increments on every rotation)."},
			"secret_rotated_at": schema.StringAttribute{Computed: true, MarkdownDescription: "When the secret was last set (RFC 3339)."},
		},
	}
}

func (r *accountResource) toModel(a *client.Account, m *accountModel) {
	m.ID = types.StringValue(a.ID)
	m.Name = types.StringValue(a.Name)
	m.Kind = types.StringValue(a.Kind)
	m.Description = types.StringValue(a.Description)
	m.Metadata = mapValue(a.Metadata)
	m.Enabled = types.BoolValue(a.Enabled)
	m.Harness = types.StringValue(a.Harness)
	m.SecretVersion = types.Int64Value(a.SecretVersion)
	m.SecretRotatedAt = types.StringNull()
	if a.SecretRotatedAt != nil {
		m.SecretRotatedAt = types.StringValue(a.SecretRotatedAt.UTC().Format(time.RFC3339))
	}
	m.SecretWO = types.StringNull() // write-only values are never stored
}

func (r *accountResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var m accountModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	var secret types.String
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("secret_wo"), &secret)...)
	if resp.Diagnostics.HasError() {
		return
	}
	in := client.AccountInput{Name: m.Name.ValueString(), Kind: m.Kind.ValueString(), Description: strOrEmpty(m.Description),
		Metadata: stringMap(ctx, m.Metadata, &resp.Diagnostics), Secret: secret.ValueString(), Enabled: boolPtr(m.Enabled)}
	a, err := r.c.CreateAccount(ctx, in)
	if err != nil {
		addAPIError(&resp.Diagnostics, "Could not create the account", err)
		return
	}
	r.toModel(a, &m)
	setVersion(ctx, resp.Private, a.Version, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *accountResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var m accountModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	a, err := r.c.GetAccount(ctx, m.ID.ValueString())
	if client.IsNotFound(err) {
		resp.State.RemoveResource(ctx)
		return
	} else if err != nil {
		addAPIError(&resp.Diagnostics, "Could not read the account", err)
		return
	}
	r.toModel(a, &m)
	setVersion(ctx, resp.Private, a.Version, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *accountResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var m, prior accountModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &prior)...)
	if resp.Diagnostics.HasError() {
		return
	}
	in := client.AccountInput{Name: m.Name.ValueString(), Description: strOrEmpty(m.Description),
		Metadata: stringMap(ctx, m.Metadata, &resp.Diagnostics), Enabled: boolPtr(m.Enabled)}
	if in.Metadata == nil {
		in.Metadata = map[string]string{}
	}
	if !m.SecretWOVersion.Equal(prior.SecretWOVersion) {
		var secret types.String
		resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("secret_wo"), &secret)...)
		in.Secret = secret.ValueString()
	}
	if resp.Diagnostics.HasError() {
		return
	}
	v := getVersion(ctx, req.Private, &resp.Diagnostics)
	if v == 0 {
		cur, err := r.c.GetAccount(ctx, prior.ID.ValueString())
		if err != nil {
			addAPIError(&resp.Diagnostics, "Could not read the account", err)
			return
		}
		v = cur.Version
	}
	a, err := r.c.UpdateAccount(ctx, prior.ID.ValueString(), v, in)
	if err != nil {
		addAPIError(&resp.Diagnostics, "Could not update the account", err)
		return
	}
	r.toModel(a, &m)
	setVersion(ctx, resp.Private, a.Version, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *accountResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var m accountModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.c.DeleteAccount(ctx, m.ID.ValueString()); err != nil {
		addAPIError(&resp.Diagnostics, "Could not delete the account", err)
	}
}

func (r *accountResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	importPassthrough(ctx, req, resp)
}
