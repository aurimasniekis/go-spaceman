package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/aurimasniekis/go-spaceman/terraform-provider-spaceman/internal/client"
)

// spaceAccountsResource owns the complete set of account bindings of one space: the API replaces the whole set.
type spaceAccountsResource struct{ base }

func newSpaceAccountsResource() resource.Resource { return &spaceAccountsResource{} }

var bindingType = types.ObjectType{AttrTypes: map[string]attr.Type{"account_id": types.StringType, "default": types.BoolType}}

type bindingModel struct {
	AccountID types.String `tfsdk:"account_id"`
	Default   types.Bool   `tfsdk:"default"`
}

type spaceAccountsModel struct {
	ID       types.String `tfsdk:"id"`
	SpaceID  types.String `tfsdk:"space_id"`
	Bindings types.Set    `tfsdk:"binding"`
}

func (r *spaceAccountsResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_space_accounts"
}

func (r *spaceAccountsResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "The accounts a space's runs may use (`GET/PUT /spaces/{id}/accounts`). This resource owns the space's whole binding set: " +
			"bindings added in the UI show as drift. At most one default per harness. Destroying it removes every binding.",
		Attributes: map[string]schema.Attribute{
			"id":       idAttr("The space ID."),
			"space_id": requiredReplace("The space."),
			"binding": schema.SetNestedAttribute{Required: true, MarkdownDescription: "Bound accounts.",
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"account_id": schema.StringAttribute{Required: true, MarkdownDescription: "The account (`acc_…`)."},
					"default": schema.BoolAttribute{Optional: true, Computed: true, Default: booldefault.StaticBool(false),
						MarkdownDescription: "The account the space's runs of its harness use unless a launch names another."},
				}}},
		},
	}
}

func (r *spaceAccountsResource) bindings(ctx context.Context, v types.Set, d *diag.Diagnostics) []client.AccountBinding {
	var ms []bindingModel
	d.Append(v.ElementsAs(ctx, &ms, false)...)
	out := make([]client.AccountBinding, 0, len(ms))
	for _, m := range ms {
		out = append(out, client.AccountBinding{AccountID: m.AccountID.ValueString(), Default: m.Default.ValueBool()})
	}
	return out
}

func bindingsValue(bs []client.AccountBinding) types.Set {
	elems := make([]attr.Value, 0, len(bs))
	for _, b := range bs {
		elems = append(elems, types.ObjectValueMust(bindingType.AttrTypes, map[string]attr.Value{
			"account_id": types.StringValue(b.AccountID), "default": types.BoolValue(b.Default)}))
	}
	return types.SetValueMust(bindingType, elems)
}

func (r *spaceAccountsResource) apply(ctx context.Context, plan *spaceAccountsModel, d *diag.Diagnostics) bool {
	bs := r.bindings(ctx, plan.Bindings, d)
	if d.HasError() {
		return false
	}
	out, err := r.c.SetSpaceAccounts(ctx, plan.SpaceID.ValueString(), bs)
	if err != nil {
		addAPIError(d, "Could not set the space's accounts", err)
		return false
	}
	plan.ID = plan.SpaceID
	plan.Bindings = bindingsValue(out)
	return true
}

func (r *spaceAccountsResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var m spaceAccountsModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() || !r.apply(ctx, &m, &resp.Diagnostics) {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *spaceAccountsResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var m spaceAccountsModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	bs, err := r.c.SpaceAccounts(ctx, m.SpaceID.ValueString())
	if client.IsNotFound(err) {
		resp.State.RemoveResource(ctx)
		return
	} else if err != nil {
		addAPIError(&resp.Diagnostics, "Could not read the space's accounts", err)
		return
	}
	m.ID = m.SpaceID
	m.Bindings = bindingsValue(bs)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *spaceAccountsResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var m spaceAccountsModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() || !r.apply(ctx, &m, &resp.Diagnostics) {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *spaceAccountsResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var m spaceAccountsModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if _, err := r.c.SetSpaceAccounts(ctx, m.SpaceID.ValueString(), nil); err != nil && !client.IsNotFound(err) {
		addAPIError(&resp.Diagnostics, "Could not remove the space's accounts", err)
	}
}

func (r *spaceAccountsResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("space_id"), req.ID)...)
}
