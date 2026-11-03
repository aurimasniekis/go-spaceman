package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework-validators/datasourcevalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/aurimasniekis/go-spaceman/terraform-provider-spaceman/internal/client"
)

type dsBase struct {
	c *client.Client
}

func (b *dsBase) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	b.c = configureClient(req.ProviderData, &resp.Diagnostics)
}

// ---- nodes ----

var nodeAttrTypes = map[string]attr.Type{
	"id": types.StringType, "name": types.StringType, "description": types.StringType, "state": types.StringType,
	"labels": types.MapType{ElemType: types.StringType}, "connected": types.BoolType, "capacity": types.Int64Type, "agent_version": types.StringType,
}

func nodeAttrs(lookup bool) map[string]schema.Attribute {
	a := map[string]schema.Attribute{
		"id":            schema.StringAttribute{Computed: true, MarkdownDescription: "The node ID (`nod_…`)."},
		"name":          schema.StringAttribute{Computed: true, MarkdownDescription: "Unique name."},
		"description":   schema.StringAttribute{Computed: true, MarkdownDescription: "Description."},
		"state":         schema.StringAttribute{Computed: true, MarkdownDescription: "`pending`, `active`, `disabled` or `revoked`."},
		"labels":        schema.MapAttribute{Computed: true, ElementType: types.StringType, MarkdownDescription: "Labels."},
		"connected":     schema.BoolAttribute{Computed: true, MarkdownDescription: "Whether the node's agent is connected now."},
		"capacity":      schema.Int64Attribute{Computed: true, MarkdownDescription: "Runs the node accepts at once."},
		"agent_version": schema.StringAttribute{Computed: true, MarkdownDescription: "The agent's version, once it connected."},
	}
	if lookup {
		a["id"] = schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The node ID (`nod_…`). Give `id` or `name`."}
		a["name"] = schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The node's unique name. Give `id` or `name`."}
	}
	return a
}

func nodeValue(n *client.Node) map[string]attr.Value {
	return map[string]attr.Value{"id": types.StringValue(n.ID), "name": types.StringValue(n.Name), "description": types.StringValue(n.Description),
		"state": types.StringValue(n.State), "labels": mapValue(n.Labels), "connected": types.BoolValue(n.Connected), "capacity": types.Int64Value(n.Capacity),
		"agent_version": types.StringValue(n.AgentVersion)}
}

type nodeDataSource struct{ dsBase }

func newNodeDataSource() datasource.DataSource { return &nodeDataSource{} }

type nodeModel struct {
	ID           types.String `tfsdk:"id"`
	Name         types.String `tfsdk:"name"`
	Description  types.String `tfsdk:"description"`
	State        types.String `tfsdk:"state"`
	Labels       types.Map    `tfsdk:"labels"`
	Connected    types.Bool   `tfsdk:"connected"`
	Capacity     types.Int64  `tfsdk:"capacity"`
	AgentVersion types.String `tfsdk:"agent_version"`
}

func (d *nodeDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_node"
}

func (d *nodeDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{MarkdownDescription: "A registered node, by ID or name (`GET /nodes`). Nodes are enrolled with a one-time token, " +
		"which is never handled by Terraform.", Attributes: nodeAttrs(true)}
}

func (d *nodeDataSource) ConfigValidators(context.Context) []datasource.ConfigValidator {
	return []datasource.ConfigValidator{datasourcevalidator.ExactlyOneOf(path.MatchRoot("id"), path.MatchRoot("name"))}
}

func (d *nodeDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var m nodeModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	var n *client.Node
	var err error
	if id := strOrEmpty(m.ID); id != "" {
		n, err = d.c.GetNode(ctx, id)
	} else {
		var l []*client.Node
		if l, err = d.c.ListNodes(ctx); err == nil {
			for _, x := range l {
				if x.Name == m.Name.ValueString() {
					n = x
				}
			}
			if n == nil {
				resp.Diagnostics.AddError("Node not found", fmt.Sprintf("no node is named %q", m.Name.ValueString()))
				return
			}
		}
	}
	if err != nil {
		addAPIError(&resp.Diagnostics, "Could not read the node", err)
		return
	}
	v := nodeValue(n)
	m = nodeModel{ID: v["id"].(types.String), Name: v["name"].(types.String), Description: v["description"].(types.String), State: v["state"].(types.String),
		Labels: v["labels"].(types.Map), Connected: v["connected"].(types.Bool), Capacity: v["capacity"].(types.Int64), AgentVersion: v["agent_version"].(types.String)}
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

type nodesDataSource struct{ dsBase }

func newNodesDataSource() datasource.DataSource { return &nodesDataSource{} }

func (d *nodesDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_nodes"
}

func (d *nodesDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{MarkdownDescription: "Every registered node (`GET /nodes`).", Attributes: map[string]schema.Attribute{
		"nodes": schema.ListNestedAttribute{Computed: true, MarkdownDescription: "The nodes.",
			NestedObject: schema.NestedAttributeObject{Attributes: nodeAttrs(false)}},
	}}
}

func (d *nodesDataSource) Read(ctx context.Context, _ datasource.ReadRequest, resp *datasource.ReadResponse) {
	l, err := d.c.ListNodes(ctx)
	if err != nil {
		addAPIError(&resp.Diagnostics, "Could not list nodes", err)
		return
	}
	t := types.ObjectType{AttrTypes: nodeAttrTypes}
	elems := make([]attr.Value, 0, len(l))
	for _, n := range l {
		elems = append(elems, types.ObjectValueMust(nodeAttrTypes, nodeValue(n)))
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("nodes"), types.ListValueMust(t, elems))...)
}

// ---- spaces ----

var spaceAttrTypes = map[string]attr.Type{
	"id": types.StringType, "slug": types.StringType, "name": types.StringType, "description": types.StringType, "node_id": types.StringType,
	"state": types.StringType, "disconnect_policy": types.StringType, "workdir": types.StringType,
}

func spaceAttrs(lookup bool) map[string]schema.Attribute {
	a := map[string]schema.Attribute{
		"id":                schema.StringAttribute{Computed: true, MarkdownDescription: "The space ID (`spc_…`)."},
		"slug":              schema.StringAttribute{Computed: true, MarkdownDescription: "Unique identifier."},
		"name":              schema.StringAttribute{Computed: true, MarkdownDescription: "Display name."},
		"description":       schema.StringAttribute{Computed: true, MarkdownDescription: "Description."},
		"node_id":           schema.StringAttribute{Computed: true, MarkdownDescription: "The pinned node."},
		"state":             schema.StringAttribute{Computed: true, MarkdownDescription: "`active`, `paused` or `archived`."},
		"disconnect_policy": schema.StringAttribute{Computed: true, MarkdownDescription: "`interrupt` or `kill`."},
		"workdir":           schema.StringAttribute{Computed: true, MarkdownDescription: "The managed folder name on the node."},
	}
	if lookup {
		a["id"] = schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The space ID. Give `id` or `slug`."}
		a["slug"] = schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "The space's slug. Give `id` or `slug`."}
	}
	return a
}

func spaceValue(s *client.Space) map[string]attr.Value {
	return map[string]attr.Value{"id": types.StringValue(s.ID), "slug": types.StringValue(s.Slug), "name": types.StringValue(s.Name),
		"description": types.StringValue(s.Description), "node_id": types.StringValue(s.NodeID), "state": types.StringValue(s.State),
		"disconnect_policy": types.StringValue(s.DisconnectPolicy), "workdir": types.StringValue(s.Workdir)}
}

type spaceDataSource struct{ dsBase }

func newSpaceDataSource() datasource.DataSource { return &spaceDataSource{} }

type spaceDSModel struct {
	ID               types.String `tfsdk:"id"`
	Slug             types.String `tfsdk:"slug"`
	Name             types.String `tfsdk:"name"`
	Description      types.String `tfsdk:"description"`
	NodeID           types.String `tfsdk:"node_id"`
	State            types.String `tfsdk:"state"`
	DisconnectPolicy types.String `tfsdk:"disconnect_policy"`
	Workdir          types.String `tfsdk:"workdir"`
}

func (d *spaceDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_space"
}

func (d *spaceDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{MarkdownDescription: "A space the token can see, by ID or slug (`GET /spaces`).", Attributes: spaceAttrs(true)}
}

func (d *spaceDataSource) ConfigValidators(context.Context) []datasource.ConfigValidator {
	return []datasource.ConfigValidator{datasourcevalidator.ExactlyOneOf(path.MatchRoot("id"), path.MatchRoot("slug"))}
}

func (d *spaceDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var m spaceDSModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	var s *client.Space
	var err error
	if id := strOrEmpty(m.ID); id != "" {
		s, err = d.c.GetSpace(ctx, id)
	} else {
		var l []*client.Space
		if l, err = d.c.ListSpaces(ctx); err == nil {
			for _, x := range l {
				if x.Slug == m.Slug.ValueString() {
					s = x
				}
			}
			if s == nil {
				resp.Diagnostics.AddError("Space not found", fmt.Sprintf("no visible space has slug %q", m.Slug.ValueString()))
				return
			}
		}
	}
	if err != nil {
		addAPIError(&resp.Diagnostics, "Could not read the space", err)
		return
	}
	m = spaceDSModel{ID: types.StringValue(s.ID), Slug: types.StringValue(s.Slug), Name: types.StringValue(s.Name), Description: types.StringValue(s.Description),
		NodeID: types.StringValue(s.NodeID), State: types.StringValue(s.State), DisconnectPolicy: types.StringValue(s.DisconnectPolicy),
		Workdir: types.StringValue(s.Workdir)}
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

type spacesDataSource struct{ dsBase }

func newSpacesDataSource() datasource.DataSource { return &spacesDataSource{} }

func (d *spacesDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_spaces"
}

func (d *spacesDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{MarkdownDescription: "Every space the token can see (`GET /spaces`).", Attributes: map[string]schema.Attribute{
		"spaces": schema.ListNestedAttribute{Computed: true, MarkdownDescription: "The spaces.",
			NestedObject: schema.NestedAttributeObject{Attributes: spaceAttrs(false)}},
	}}
}

func (d *spacesDataSource) Read(ctx context.Context, _ datasource.ReadRequest, resp *datasource.ReadResponse) {
	l, err := d.c.ListSpaces(ctx)
	if err != nil {
		addAPIError(&resp.Diagnostics, "Could not list spaces", err)
		return
	}
	t := types.ObjectType{AttrTypes: spaceAttrTypes}
	elems := make([]attr.Value, 0, len(l))
	for _, s := range l {
		elems = append(elems, types.ObjectValueMust(spaceAttrTypes, spaceValue(s)))
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("spaces"), types.ListValueMust(t, elems))...)
}

// ---- event templates (including the built-in GitHub presets) ----

type eventTemplateDataSource struct{ dsBase }

func newEventTemplateDataSource() datasource.DataSource { return &eventTemplateDataSource{} }

type eventTemplateDSModel struct {
	ID              types.String `tfsdk:"id"`
	Slug            types.String `tfsdk:"slug"`
	Name            types.String `tfsdk:"name"`
	Builtin         types.Bool   `tfsdk:"builtin"`
	LatestVersion   types.Int64  `tfsdk:"latest_version"`
	LatestVersionID types.String `tfsdk:"latest_version_id"`
	Subject         types.String `tfsdk:"subject"`
}

func (d *eventTemplateDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_event_template"
}

func (d *eventTemplateDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{MarkdownDescription: "An event template of the catalog by slug, including the built-in presets (`GET /event-templates`): " +
		"use `latest_version_id` in a `spaceman_event_subscription`.", Attributes: map[string]schema.Attribute{
		"slug":              schema.StringAttribute{Required: true, MarkdownDescription: "The template's slug."},
		"id":                schema.StringAttribute{Computed: true, MarkdownDescription: "The template ID."},
		"name":              schema.StringAttribute{Computed: true, MarkdownDescription: "Display name."},
		"builtin":           schema.BoolAttribute{Computed: true, MarkdownDescription: "Seeded from a verified upstream contract."},
		"latest_version":    schema.Int64Attribute{Computed: true, MarkdownDescription: "Number of the latest published version."},
		"latest_version_id": schema.StringAttribute{Computed: true, MarkdownDescription: "ID of the latest published version."},
		"subject":           schema.StringAttribute{Computed: true, MarkdownDescription: "The latest version's subject filter (may contain `{{vars.x}}`)."},
	}}
}

func (d *eventTemplateDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var m eventTemplateDSModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	l, err := d.c.ListEventTemplates(ctx)
	if err != nil {
		addAPIError(&resp.Diagnostics, "Could not list event templates", err)
		return
	}
	var t *client.EventTemplate
	for _, x := range l {
		if x.Slug == m.Slug.ValueString() {
			t = x
		}
	}
	if t == nil {
		resp.Diagnostics.AddError("Event template not found", fmt.Sprintf("no event template has slug %q", m.Slug.ValueString()))
		return
	}
	if len(t.Versions) == 0 { // the list may omit versions
		if t, err = d.c.GetEventTemplate(ctx, t.ID); err != nil {
			addAPIError(&resp.Diagnostics, "Could not read the event template", err)
			return
		}
	}
	m.ID, m.Name, m.Builtin = types.StringValue(t.ID), types.StringValue(t.Name), types.BoolValue(t.Builtin)
	m.LatestVersion, m.LatestVersionID, m.Subject = types.Int64Value(0), types.StringValue(""), types.StringValue("")
	if lp := t.LatestPublished(); lp != nil {
		m.LatestVersion, m.LatestVersionID = types.Int64Value(lp.Version), types.StringValue(lp.ID)
		if lp.Definition != nil {
			m.Subject = types.StringValue(lp.Definition.Subject)
		}
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}
