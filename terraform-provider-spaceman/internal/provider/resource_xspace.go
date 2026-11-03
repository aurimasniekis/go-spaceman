package provider

import (
	"context"
	"sort"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/objectdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/setdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/aurimasniekis/go-spaceman/terraform-provider-spaceman/internal/client"
)

// SpaceMan's defaults of the cross-space settings and peer limits (internal/xspace).
const (
	xspaceDefaultMaxDepth    = 3
	xspaceDefaultMaxOutgoing = 60
	xspaceDefaultMaxPerHour  = 20
	xspaceDefaultMaxPending  = 5
)

// ---- settings ----

type xspaceSettingsResource struct{ base }

func newXSpaceSettingsResource() resource.Resource { return &xspaceSettingsResource{} }

type xspaceSettingsModel struct {
	ID                 types.String `tfsdk:"id"`
	SpaceID            types.String `tfsdk:"space_id"`
	Profile            types.String `tfsdk:"profile"`
	MaxDepth           types.Int64  `tfsdk:"max_depth"`
	MaxOutgoingPerHour types.Int64  `tfsdk:"max_outgoing_per_hour"`
}

func (r *xspaceSettingsResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_space_xspace_settings"
}

func (r *xspaceSettingsResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A space's cross-space settings (`GET/PUT /spaces/{id}/xspace/settings`, ADR-0038): the profile peers allowed to " +
			"discover the space see, the deepest request chain it accepts and how many requests its control may send per hour. What a space " +
			"accepts from and may send to each peer is `spaceman_xspace_peer`. Every space has settings (SpaceMan's defaults until saved), so " +
			"destroying the resource resets them to the defaults.",
		Attributes: map[string]schema.Attribute{
			"id":                    idAttr("The space ID."),
			"space_id":              requiredReplace("The space."),
			"profile":               optString("What peers allowed to discover this space see about it, written for agents (at most 2000 characters)."),
			"max_depth":             optInt64("Requests deeper than this in a chain are refused here (1-8, default 3)."),
			"max_outgoing_per_hour": optInt64("Requests this space's control may send per hour, to every peer together (1-1000, default 60)."),
		},
	}
}

func (r *xspaceSettingsResource) toModel(s *client.XSpaceSettings, m *xspaceSettingsModel) {
	m.ID = m.SpaceID
	m.Profile = keepTrimmed(m.Profile, s.Profile)
	m.MaxDepth = types.Int64Value(s.MaxDepth)
	m.MaxOutgoingPerHour = types.Int64Value(s.MaxOutgoingPerHour)
}

// save writes the planned settings; fields the plan does not know keep the current values.
func (r *xspaceSettingsResource) save(ctx context.Context, m *xspaceSettingsModel, version int64, d *diagnostics) (*client.XSpaceSettings, bool) {
	cur, err := r.c.GetXSpaceSettings(ctx, m.SpaceID.ValueString())
	if err != nil {
		addAPIError(d, "Could not read the cross-space settings", err)
		return nil, false
	}
	if version == 0 {
		version = cur.Version
	}
	in := client.XSpaceSettingsInput{Profile: cur.Profile, MaxDepth: cur.MaxDepth, MaxOutgoingPerHour: cur.MaxOutgoingPerHour}
	if !m.Profile.IsUnknown() && !m.Profile.IsNull() {
		in.Profile = m.Profile.ValueString()
	}
	if !m.MaxDepth.IsUnknown() && !m.MaxDepth.IsNull() {
		in.MaxDepth = m.MaxDepth.ValueInt64()
	}
	if !m.MaxOutgoingPerHour.IsUnknown() && !m.MaxOutgoingPerHour.IsNull() {
		in.MaxOutgoingPerHour = m.MaxOutgoingPerHour.ValueInt64()
	}
	s, err := r.c.SetXSpaceSettings(ctx, m.SpaceID.ValueString(), version, in)
	if err != nil {
		addAPIError(d, "Could not save the cross-space settings", err)
		return nil, false
	}
	return s, true
}

func (r *xspaceSettingsResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var m xspaceSettingsModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	s, ok := r.save(ctx, &m, 0, &resp.Diagnostics)
	if !ok {
		return
	}
	r.toModel(s, &m)
	setVersion(ctx, resp.Private, s.Version, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *xspaceSettingsResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var m xspaceSettingsModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	s, err := r.c.GetXSpaceSettings(ctx, m.SpaceID.ValueString())
	if client.IsNotFound(err) {
		resp.State.RemoveResource(ctx)
		return
	} else if err != nil {
		addAPIError(&resp.Diagnostics, "Could not read the cross-space settings", err)
		return
	}
	r.toModel(s, &m)
	setVersion(ctx, resp.Private, s.Version, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *xspaceSettingsResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var m xspaceSettingsModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	s, ok := r.save(ctx, &m, getVersion(ctx, req.Private, &resp.Diagnostics), &resp.Diagnostics)
	if !ok {
		return
	}
	r.toModel(s, &m)
	setVersion(ctx, resp.Private, s.Version, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *xspaceSettingsResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var m xspaceSettingsModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	cur, err := r.c.GetXSpaceSettings(ctx, m.SpaceID.ValueString())
	if client.IsNotFound(err) {
		return
	} else if err != nil {
		addAPIError(&resp.Diagnostics, "Could not read the cross-space settings", err)
		return
	}
	if cur.Version == 0 {
		return
	}
	if _, err := r.c.SetXSpaceSettings(ctx, m.SpaceID.ValueString(), cur.Version,
		client.XSpaceSettingsInput{MaxDepth: xspaceDefaultMaxDepth, MaxOutgoingPerHour: xspaceDefaultMaxOutgoing}); err != nil && !client.IsNotFound(err) {
		addAPIError(&resp.Diagnostics, "Could not reset the cross-space settings", err)
	}
}

func (r *xspaceSettingsResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("space_id"), req.ID)...)
}

// ---- peers ----

type xspacePeerResource struct{ base }

func newXSpacePeerResource() resource.Resource { return &xspacePeerResource{} }

var inboundType = types.ObjectType{AttrTypes: map[string]attr.Type{
	"discover": types.BoolType, "questions": types.BoolType, "delegation": types.BoolType, "knowledge": types.BoolType,
	"knowledge_bases": types.SetType{ElemType: types.StringType}, "disclosure": types.StringType, "max_per_hour": types.Int64Type,
	"max_pending": types.Int64Type}}

var outboundType = types.ObjectType{AttrTypes: map[string]attr.Type{"questions": types.BoolType, "delegation": types.BoolType, "knowledge": types.BoolType}}

type inboundModel struct {
	Discover   types.Bool   `tfsdk:"discover"`
	Questions  types.Bool   `tfsdk:"questions"`
	Delegation types.Bool   `tfsdk:"delegation"`
	Knowledge  types.Bool   `tfsdk:"knowledge"`
	KBs        types.Set    `tfsdk:"knowledge_bases"`
	Disclosure types.String `tfsdk:"disclosure"`
	MaxPerHour types.Int64  `tfsdk:"max_per_hour"`
	MaxPending types.Int64  `tfsdk:"max_pending"`
}

type outboundModel struct {
	Questions  types.Bool `tfsdk:"questions"`
	Delegation types.Bool `tfsdk:"delegation"`
	Knowledge  types.Bool `tfsdk:"knowledge"`
}

type xspacePeerModel struct {
	ID       types.String `tfsdk:"id"`
	SpaceID  types.String `tfsdk:"space_id"`
	PeerID   types.String `tfsdk:"peer_id"`
	Inbound  types.Object `tfsdk:"inbound"`
	Outbound types.Object `tfsdk:"outbound"`
}

func inboundValue(in client.XSpaceInbound) types.Object {
	kbs := append([]string(nil), in.KBs...)
	sort.Strings(kbs)
	return types.ObjectValueMust(inboundType.AttrTypes, map[string]attr.Value{
		"discover": types.BoolValue(in.Discover), "questions": types.BoolValue(in.Questions), "delegation": types.BoolValue(in.Delegation),
		"knowledge": types.BoolValue(in.Knowledge), "knowledge_bases": setValue(kbs), "disclosure": types.StringValue(in.Disclosure),
		"max_per_hour": types.Int64Value(in.MaxPerHour), "max_pending": types.Int64Value(in.MaxPending)})
}

func outboundValue(out client.XSpaceOutbound) types.Object {
	return types.ObjectValueMust(outboundType.AttrTypes, map[string]attr.Value{
		"questions": types.BoolValue(out.Questions), "delegation": types.BoolValue(out.Delegation), "knowledge": types.BoolValue(out.Knowledge)})
}

func (r *xspacePeerResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_xspace_peer"
}

func (r *xspacePeerResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	flag := func(d string) schema.BoolAttribute {
		return schema.BoolAttribute{Optional: true, Computed: true, Default: booldefault.StaticBool(false), MarkdownDescription: d + " Default `false`."}
	}
	resp.Schema = schema.Schema{
		MarkdownDescription: "One space's side of a cross-space grant with a peer space (`PUT/DELETE /spaces/{id}/xspace/peers/{peer_id}`, ADR-0038): " +
			"what the space accepts from the peer (`inbound`) and what its control may send to it (`outbound`). A request from S to D needs D's " +
			"inbound flag **and** S's outbound flag, so a working pair needs two resources, one per side. Every flag is off unless set. Because " +
			"this is an authorization grant, Terraform owns the whole configuration: a flag turned on in the UI shows as drift. Destroying the " +
			"resource removes the configuration (nothing is allowed with that peer from this side); requests already open stay open.",
		Attributes: map[string]schema.Attribute{
			"id":       idAttr("`<space_id>/<peer_id>`."),
			"space_id": requiredReplace("The space whose side this is."),
			"peer_id":  requiredReplace("The peer space (not the same space)."),
			"inbound": schema.SingleNestedAttribute{Optional: true, Computed: true, MarkdownDescription: "What this space accepts from the peer.",
				Default: objectdefault.StaticValue(inboundValue(client.XSpaceInbound{Disclosure: "none", MaxPerHour: xspaceDefaultMaxPerHour,
					MaxPending: xspaceDefaultMaxPending})),
				Attributes: map[string]schema.Attribute{
					"discover":   flag("The peer's control may see this space and its profile (not needed to ask)."),
					"questions":  flag("The peer may ask this space's control questions."),
					"delegation": flag("The peer may ask this space's control to do work; this space decides and runs it."),
					"knowledge":  flag("The peer may ask this space's knowledge bases (through support sessions, never the repository)."),
					"knowledge_bases": schema.SetAttribute{Optional: true, Computed: true, ElementType: types.StringType,
						Default:             setdefault.StaticValue(types.SetValueMust(types.StringType, []attr.Value{})),
						MarkdownDescription: "KB slugs the peer may ask; empty (default) means every enabled knowledge base."},
					"disclosure": schema.StringAttribute{Optional: true, Computed: true, Default: stringdefault.StaticString("none"),
						Validators: []validator.String{stringvalidator.OneOf("none", "summary", "detailed")},
						MarkdownDescription: "What replies to the peer may contain: `none` (default, the outcome only), `summary` (text up to 2,000 " +
							"characters), `detailed` (text up to 20,000 characters and KB evidence)."},
					"max_per_hour": schema.Int64Attribute{Optional: true, Computed: true, Default: int64default.StaticInt64(xspaceDefaultMaxPerHour),
						MarkdownDescription: "Requests the peer may send here per hour (1-1000, default 20)."},
					"max_pending": schema.Int64Attribute{Optional: true, Computed: true, Default: int64default.StaticInt64(xspaceDefaultMaxPending),
						MarkdownDescription: "Requests from the peer open here at once (1-100, default 5)."},
				}},
			"outbound": schema.SingleNestedAttribute{Optional: true, Computed: true, MarkdownDescription: "What this space's control may send to the peer.",
				Default: objectdefault.StaticValue(outboundValue(client.XSpaceOutbound{})),
				Attributes: map[string]schema.Attribute{
					"questions":  flag("Control may ask the peer's control questions."),
					"delegation": flag("Control may ask the peer's control to do work."),
					"knowledge":  flag("Control may ask the peer's knowledge bases."),
				}},
		},
	}
}

func (r *xspacePeerResource) toModel(p *client.XSpacePeer, m *xspacePeerModel) {
	m.SpaceID = types.StringValue(p.SpaceID)
	m.PeerID = types.StringValue(p.Peer.ID)
	m.ID = types.StringValue(p.SpaceID + "/" + p.Peer.ID)
	m.Inbound = inboundValue(p.Inbound)
	m.Outbound = outboundValue(p.Outbound)
}

func (r *xspacePeerResource) input(ctx context.Context, m *xspacePeerModel, d *diagnostics) client.XSpacePeerInput {
	var ib inboundModel
	var ob outboundModel
	d.Append(m.Inbound.As(ctx, &ib, objectAsOptions)...)
	d.Append(m.Outbound.As(ctx, &ob, objectAsOptions)...)
	kbs := stringSet(ctx, ib.KBs, d)
	sort.Strings(kbs)
	return client.XSpacePeerInput{
		Inbound: client.XSpaceInbound{Discover: ib.Discover.ValueBool(), Questions: ib.Questions.ValueBool(), Delegation: ib.Delegation.ValueBool(),
			Knowledge: ib.Knowledge.ValueBool(), KBs: orEmpty(kbs), Disclosure: ib.Disclosure.ValueString(), MaxPerHour: ib.MaxPerHour.ValueInt64(),
			MaxPending: ib.MaxPending.ValueInt64()},
		Outbound: client.XSpaceOutbound{Questions: ob.Questions.ValueBool(), Delegation: ob.Delegation.ValueBool(), Knowledge: ob.Knowledge.ValueBool()},
	}
}

func (r *xspacePeerResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var m xspacePeerModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	in := r.input(ctx, &m, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	p, err := r.c.SetXSpacePeer(ctx, m.SpaceID.ValueString(), m.PeerID.ValueString(), 0, in)
	if err != nil {
		addAPIError(&resp.Diagnostics, "Could not create the peer configuration", err)
		if client.IsConflict(err) {
			resp.Diagnostics.AddError("The space already has a configuration for this peer",
				"Import it instead: terraform import <address> "+m.SpaceID.ValueString()+"/"+m.PeerID.ValueString())
		}
		return
	}
	r.toModel(p, &m)
	setVersion(ctx, resp.Private, p.Version, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *xspacePeerResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var m xspacePeerModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	p, err := r.c.GetXSpacePeer(ctx, m.SpaceID.ValueString(), m.PeerID.ValueString())
	if client.IsNotFound(err) {
		resp.State.RemoveResource(ctx)
		return
	} else if err != nil {
		addAPIError(&resp.Diagnostics, "Could not read the peer configuration", err)
		return
	}
	r.toModel(p, &m)
	setVersion(ctx, resp.Private, p.Version, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *xspacePeerResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var m xspacePeerModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	in := r.input(ctx, &m, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	v := getVersion(ctx, req.Private, &resp.Diagnostics)
	if v == 0 {
		cur, err := r.c.GetXSpacePeer(ctx, m.SpaceID.ValueString(), m.PeerID.ValueString())
		if err != nil {
			addAPIError(&resp.Diagnostics, "Could not read the peer configuration", err)
			return
		}
		v = cur.Version
	}
	p, err := r.c.SetXSpacePeer(ctx, m.SpaceID.ValueString(), m.PeerID.ValueString(), v, in)
	if err != nil {
		addAPIError(&resp.Diagnostics, "Could not update the peer configuration", err)
		return
	}
	r.toModel(p, &m)
	setVersion(ctx, resp.Private, p.Version, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *xspacePeerResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var m xspacePeerModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.c.DeleteXSpacePeer(ctx, m.SpaceID.ValueString(), m.PeerID.ValueString()); err != nil {
		addAPIError(&resp.Diagnostics, "Could not remove the peer configuration", err)
	}
}

func (r *xspacePeerResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	p := splitImportID(req.ID, 2, "<space_id>/<peer_id>", &resp.Diagnostics)
	if p == nil {
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("space_id"), p[0])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("peer_id"), p[1])...)
}
