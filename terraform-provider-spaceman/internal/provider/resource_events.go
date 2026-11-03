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
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/aurimasniekis/go-spaceman/terraform-provider-spaceman/internal/client"
)

// ---- connections ----

type eventConnectionResource struct{ base }

func newEventConnectionResource() resource.Resource { return &eventConnectionResource{} }

type eventConnectionModel struct {
	ID             types.String `tfsdk:"id"`
	Name           types.String `tfsdk:"name"`
	URL            types.String `tfsdk:"url"`
	CAPEM          types.String `tfsdk:"ca_pem"`
	Stream         types.String `tfsdk:"stream"`
	Enabled        types.Bool   `tfsdk:"enabled"`
	CredsWO        types.String `tfsdk:"creds_wo"`
	CredsWOVersion types.Int64  `tfsdk:"creds_wo_version"`
	ClientID       types.String `tfsdk:"client_id"`
	HasCA          types.Bool   `tfsdk:"has_ca"`
}

func (r *eventConnectionResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_event_connection"
}

func (r *eventConnectionResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A Pulse connection (`/event-connections`). The `.creds` content is write-only: sealed by SpaceMan, never returned and never " +
			"stored by Terraform (requires Terraform 1.11 or later). Deleting is refused while a subscription uses the connection.",
		Attributes: map[string]schema.Attribute{
			"id":   idAttr("The connection ID."),
			"name": schema.StringAttribute{Required: true, MarkdownDescription: "Unique name: lowercase letters, digits and dashes (2-63)."},
			"url":  schema.StringAttribute{Required: true, MarkdownDescription: "`nats://host:port` or `tls://host:port`."},
			"ca_pem": schema.StringAttribute{Optional: true, Computed: true, Default: stringDefaultEmpty,
				MarkdownDescription: "A PEM CA certificate for `tls://` (not secret, but never returned: the API reports only `has_ca`)."},
			"stream": optString("The JetStream stream (default `EVENTS`)."),
			"enabled": schema.BoolAttribute{Optional: true, Computed: true, Default: booldefault.StaticBool(true),
				MarkdownDescription: "Whether subscriptions on the connection consume (default `true`)."},
			"creds_wo": schema.StringAttribute{Required: true, WriteOnly: true, Sensitive: true,
				MarkdownDescription: "The `.creds` file content issued by Pulse (write-only). Sent on create and whenever `creds_wo_version` changes."},
			"creds_wo_version": woVersion("creds_wo"),
			"client_id":        schema.StringAttribute{Computed: true, MarkdownDescription: "The Pulse client the credentials belong to (from the JWT)."},
			"has_ca":           schema.BoolAttribute{Computed: true, MarkdownDescription: "Whether a CA certificate is stored."},
		},
	}
}

func (r *eventConnectionResource) toModel(c *client.EventConnection, m *eventConnectionModel) {
	m.ID = types.StringValue(c.ID)
	m.Name = types.StringValue(c.Name)
	m.URL = types.StringValue(c.URL)
	m.Stream = types.StringValue(c.Stream)
	m.Enabled = types.BoolValue(c.Enabled)
	m.ClientID = types.StringValue(c.ClientID)
	m.HasCA = types.BoolValue(c.HasCA)
	// ca_pem is never returned: keep the configured value, but notice a CA removed (or added) elsewhere.
	if m.CAPEM.IsNull() || m.CAPEM.IsUnknown() || (strOrEmpty(m.CAPEM) != "") != c.HasCA {
		if c.HasCA {
			m.CAPEM = types.StringValue("(set outside Terraform)")
		} else {
			m.CAPEM = types.StringValue("")
		}
	}
	m.CredsWO = types.StringNull()
}

func (r *eventConnectionResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var m eventConnectionModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	var creds types.String
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("creds_wo"), &creds)...)
	if resp.Diagnostics.HasError() {
		return
	}
	c, err := r.c.CreateEventConnection(ctx, client.EventConnectionInput{Name: m.Name.ValueString(), URL: m.URL.ValueString(), Creds: creds.ValueString(),
		CAPEM: strOrEmpty(m.CAPEM), Stream: strOrEmpty(m.Stream), Enabled: m.Enabled.ValueBool()})
	if err != nil {
		addAPIError(&resp.Diagnostics, "Could not create the event connection", err)
		return
	}
	r.toModel(c, &m)
	setVersion(ctx, resp.Private, c.Version, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *eventConnectionResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var m eventConnectionModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	c, err := r.c.GetEventConnection(ctx, m.ID.ValueString())
	if client.IsNotFound(err) {
		resp.State.RemoveResource(ctx)
		return
	} else if err != nil {
		addAPIError(&resp.Diagnostics, "Could not read the event connection", err)
		return
	}
	r.toModel(c, &m)
	setVersion(ctx, resp.Private, c.Version, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *eventConnectionResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var m, prior eventConnectionModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &prior)...)
	if resp.Diagnostics.HasError() {
		return
	}
	in := client.EventConnectionInput{Name: m.Name.ValueString(), URL: m.URL.ValueString(), CAPEM: strOrEmpty(m.CAPEM), Stream: strOrEmpty(m.Stream),
		Enabled: m.Enabled.ValueBool()}
	if !m.CredsWOVersion.Equal(prior.CredsWOVersion) {
		var creds types.String
		resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("creds_wo"), &creds)...)
		in.Creds = creds.ValueString()
	}
	v := getVersion(ctx, req.Private, &resp.Diagnostics)
	if v == 0 {
		cur, err := r.c.GetEventConnection(ctx, prior.ID.ValueString())
		if err != nil {
			addAPIError(&resp.Diagnostics, "Could not read the event connection", err)
			return
		}
		v = cur.Version
	}
	c, err := r.c.UpdateEventConnection(ctx, prior.ID.ValueString(), v, in)
	if err != nil {
		addAPIError(&resp.Diagnostics, "Could not update the event connection", err)
		return
	}
	r.toModel(c, &m)
	setVersion(ctx, resp.Private, c.Version, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *eventConnectionResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var m eventConnectionModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.c.DeleteEventConnection(ctx, m.ID.ValueString()); err != nil {
		addAPIError(&resp.Diagnostics, "Could not delete the event connection", err)
	}
}

func (r *eventConnectionResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	importPassthrough(ctx, req, resp)
}

// ---- templates ----

type eventTemplateResource struct{ base }

func newEventTemplateResource() resource.Resource { return &eventTemplateResource{} }

type eventTemplateModel struct {
	ID              types.String `tfsdk:"id"`
	Slug            types.String `tfsdk:"slug"`
	Name            types.String `tfsdk:"name"`
	Description     types.String `tfsdk:"description"`
	Definition      types.String `tfsdk:"definition"`
	Notes           types.String `tfsdk:"notes"`
	LatestVersion   types.Int64  `tfsdk:"latest_version"`
	LatestVersionID types.String `tfsdk:"latest_version_id"`
}

// definitionValidator checks that a definition parses into the API's shape (the server validates its meaning).
type definitionValidator struct{}

func (definitionValidator) Description(context.Context) string {
	return "must be a template definition as JSON"
}
func (v definitionValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}
func (definitionValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if _, err := canonicalDefinition(req.ConfigValue.ValueString()); err != nil {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid template definition", err.Error())
	}
}

func (r *eventTemplateResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_event_template"
}

func (r *eventTemplateResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A custom event template (`/event-templates`). `definition` and `notes` describe the latest published version; changing them " +
			"publishes the next immutable version (`PUT /event-templates/{id}/draft`, `POST /event-template-versions/{id}/publish`). Subscriptions " +
			"that follow the latest version move to it when its subject stays the same; the others are reported. SpaceMan has no API to rename or " +
			"delete a template: identifying fields are fixed and destroying the resource only removes it from Terraform's state.",
		Attributes: map[string]schema.Attribute{
			"id":          idAttr("The template ID."),
			"slug":        fixedString("Unique identifier. Fixed after creation.", "An event template's slug"),
			"name":        fixedString("Display name. Fixed after creation.", "An event template's name"),
			"description": fixedOptString("Description. Fixed after creation.", "An event template's description"),
			"definition": schema.StringAttribute{Required: true, Validators: []validatorString{definitionValidator{}},
				MarkdownDescription: "The definition as JSON (use `jsonencode`): `subject`, `variables`, `filters`, `input_schema`, `title`, `body`, " +
					"`instructions`, `correlation`, `urgency`. Formatting and key order never cause a diff."},
			"notes":             defaultEmpty("Notes recorded with the published version."),
			"latest_version":    schema.Int64Attribute{Computed: true, MarkdownDescription: "Number of the latest published version."},
			"latest_version_id": schema.StringAttribute{Computed: true, MarkdownDescription: "ID of the latest published version (subscribe to it)."},
		},
	}
}

func (r *eventTemplateResource) toModel(t *client.EventTemplate, m *eventTemplateModel) {
	m.ID = types.StringValue(t.ID)
	m.Slug = types.StringValue(t.Slug)
	m.Name = types.StringValue(t.Name)
	m.Description = types.StringValue(t.Description)
	if lp := t.LatestPublished(); lp != nil {
		m.Definition = definitionValue(m.Definition, lp.Definition)
		m.Notes = types.StringValue(lp.Notes)
		m.LatestVersion = types.Int64Value(lp.Version)
		m.LatestVersionID = types.StringValue(lp.ID)
	} else {
		m.Definition = types.StringValue("{}")
		m.Notes = types.StringValue("")
		m.LatestVersion, m.LatestVersionID = types.Int64Value(0), types.StringValue("")
	}
}

func (r *eventTemplateResource) publishVersion(ctx context.Context, versionID string, diags *diagnostics) bool {
	res, err := r.c.PublishEventTemplateVersion(ctx, versionID)
	if err != nil {
		addAPIError(diags, "Could not publish the event template", err)
		return false
	}
	if len(res.HeldSubscriptions) > 0 {
		diags.AddWarning("Some subscriptions did not move to the new version",
			fmt.Sprintf("Subscriptions following the template's latest version stay on their version when the new definition changes their "+
				"subject or no longer binds their variables (a subject change needs a new Pulse grant): %s.", strings.Join(res.HeldSubscriptions, ", ")))
	}
	return true
}

func (r *eventTemplateResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var m eventTemplateModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	def, err := parseDefinition(m.Definition.ValueString())
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("definition"), "Invalid template definition", err.Error())
		return
	}
	t, err := r.c.CreateEventTemplate(ctx, client.CreateEventTemplateInput{Slug: m.Slug.ValueString(), Name: m.Name.ValueString(),
		Description: strOrEmpty(m.Description), Definition: *def})
	if err != nil {
		addAPIError(&resp.Diagnostics, "Could not create the event template", err)
		return
	}
	id := t.ID
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), id)...)
	d := t.Draft()
	if d == nil {
		resp.Diagnostics.AddError("Could not publish the event template", "the created template has no draft")
		return
	}
	if notes := strOrEmpty(m.Notes); notes != "" {
		if d, err = r.c.SaveEventTemplateDraft(ctx, id, client.EventTemplateDraftInput{Definition: *def, Notes: notes}); err != nil {
			addAPIError(&resp.Diagnostics, "Could not save the event template's draft", err)
			return
		}
	}
	if !r.publishVersion(ctx, d.ID, &resp.Diagnostics) {
		return
	}
	if t, err = r.c.GetEventTemplate(ctx, id); err != nil {
		addAPIError(&resp.Diagnostics, "Could not read the event template", err)
		return
	}
	r.toModel(t, &m)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *eventTemplateResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var m eventTemplateModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	t, err := r.c.GetEventTemplate(ctx, m.ID.ValueString())
	if client.IsNotFound(err) {
		resp.State.RemoveResource(ctx)
		return
	} else if err != nil {
		addAPIError(&resp.Diagnostics, "Could not read the event template", err)
		return
	}
	r.toModel(t, &m)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *eventTemplateResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var m eventTemplateModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	def, err := parseDefinition(m.Definition.ValueString())
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("definition"), "Invalid template definition", err.Error())
		return
	}
	id := m.ID.ValueString()
	t, err := r.c.GetEventTemplate(ctx, id)
	if err != nil {
		addAPIError(&resp.Diagnostics, "Could not read the event template", err)
		return
	}
	if d := t.Draft(); d != nil {
		draftReplacedWarning(&resp.Diagnostics, "event template", d.Version)
	}
	d, err := r.c.SaveEventTemplateDraft(ctx, id, client.EventTemplateDraftInput{Definition: *def, Notes: strOrEmpty(m.Notes)})
	if err != nil {
		addAPIError(&resp.Diagnostics, "Could not save the event template's draft", err)
		return
	}
	if !r.publishVersion(ctx, d.ID, &resp.Diagnostics) {
		return
	}
	if t, err = r.c.GetEventTemplate(ctx, id); err != nil {
		addAPIError(&resp.Diagnostics, "Could not read the event template", err)
		return
	}
	r.toModel(t, &m)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *eventTemplateResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var m eventTemplateModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	noDeleteWarning(&resp.Diagnostics, "event template", m.ID.ValueString())
}

func (r *eventTemplateResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	importPassthrough(ctx, req, resp)
}

// ---- subscriptions ----

type subscriptionResource struct{ base }

func newEventSubscriptionResource() resource.Resource { return &subscriptionResource{} }

type startModel struct {
	Policy types.String `tfsdk:"policy"`
	Time   types.String `tfsdk:"time"`
	Seq    types.Int64  `tfsdk:"seq"`
}

type subscriptionModel struct {
	ID                types.String `tfsdk:"id"`
	SpaceID           types.String `tfsdk:"space_id"`
	Name              types.String `tfsdk:"name"`
	ConnectionID      types.String `tfsdk:"connection_id"`
	TemplateVersionID types.String `tfsdk:"template_version_id"`
	Follow            types.String `tfsdk:"follow"`
	Custom            types.String `tfsdk:"custom"`
	Vars              types.Map    `tfsdk:"vars"`
	Enabled           types.Bool   `tfsdk:"enabled"`
	Start             types.Object `tfsdk:"start"`
	TemplateID        types.String `tfsdk:"template_id"`
	Subject           types.String `tfsdk:"subject"`
	Durable           types.String `tfsdk:"durable"`
	Consumer          types.String `tfsdk:"consumer"`
	Grant             types.String `tfsdk:"grant"`
}

func (r *subscriptionResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_event_subscription"
}

func (r *subscriptionResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "An event subscription of a space (`/spaces/{id}/subscriptions`): a template version (or a custom definition) bound to a " +
			"Pulse connection. With `follow = \"latest\"` SpaceMan moves the subscription to newly published template versions; that move is not " +
			"drift. Deliveries, quarantine and the consumer's position are runtime state and are not managed.",
		Attributes: map[string]schema.Attribute{
			"id":            idAttr("The subscription ID."),
			"space_id":      requiredReplace("The space."),
			"name":          schema.StringAttribute{Required: true, MarkdownDescription: "Name, unique in the space (at most 80 characters)."},
			"connection_id": requiredReplace("The Pulse connection. Changing it replaces the subscription."),
			"template_version_id": schema.StringAttribute{Optional: true, Computed: true,
				MarkdownDescription: "A published template version. With `follow = \"latest\"` this is the starting version; SpaceMan moves it forward.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"follow": schema.StringAttribute{Optional: true, Computed: true, Default: stringDefault("pinned"),
				Validators:          []validatorString{stringvalidator.OneOf("pinned", "latest")},
				MarkdownDescription: "`pinned` (default) or `latest` (move to newly published versions that keep the subject)."},
			"custom": schema.StringAttribute{Optional: true, Validators: []validatorString{definitionValidator{}},
				MarkdownDescription: "A custom definition as JSON instead of a template version (cannot follow)."},
			"vars": schema.MapAttribute{Optional: true, Computed: true, ElementType: types.StringType,
				MarkdownDescription: "Variable values. Only the configured variables are compared: defaults SpaceMan fills in are not drift.",
				PlanModifiers:       []planmodifier.Map{mapUseStateForUnknown()}},
			"enabled": schema.BoolAttribute{Optional: true, Computed: true, Default: booldefault.StaticBool(true),
				MarkdownDescription: "Whether the subscription consumes (default `true`)."},
			"start": schema.SingleNestedAttribute{Optional: true,
				MarkdownDescription: "Where the consumer starts: applied on create, and on an update whenever this value changes (recreating the " +
					"consumer; required when the subject changes). Anything but `new` replays history. Left out: `new` (no history).",
				Attributes: map[string]schema.Attribute{
					"policy": schema.StringAttribute{Required: true, Validators: []validatorString{stringvalidator.OneOf("new", "all", "time", "seq")},
						MarkdownDescription: "`new`, `all`, `time` or `seq`."},
					"time": schema.StringAttribute{Optional: true, MarkdownDescription: "RFC 3339 time for `time`."},
					"seq":  schema.Int64Attribute{Optional: true, MarkdownDescription: "Stream sequence for `seq`."},
				}},
			"template_id": computedString("The template of `template_version_id`."),
			"subject":     schema.StringAttribute{Computed: true, MarkdownDescription: "The effective Pulse filter."},
			"durable":     computedString("The durable name the Pulse operator must grant."),
			"consumer":    schema.StringAttribute{Computed: true, MarkdownDescription: "The JetStream consumer (`<client_id>_<durable>`)."},
			"grant":       schema.StringAttribute{Computed: true, MarkdownDescription: "The Pulse grant the subscription needs."},
		},
	}
}

func (r *subscriptionResource) cursor(ctx context.Context, v types.Object, diags *diagnostics) *client.Cursor {
	if v.IsNull() || v.IsUnknown() {
		return nil
	}
	var s startModel
	diags.Append(v.As(ctx, &s, objectAsOptions)...)
	c := &client.Cursor{Policy: s.Policy.ValueString(), Seq: int64OrZero(s.Seq)}
	if t := strOrEmpty(s.Time); t != "" {
		tm, err := time.Parse(time.RFC3339, t)
		if err != nil {
			diags.AddAttributeError(path.Root("start").AtName("time"), "Invalid start time", err.Error())
			return nil
		}
		c.Time = &tm
	}
	return c
}

// toModel records the server's view. With follow = latest, a version moved forward by SpaceMan within the same
// template keeps the configured starting version; only vars the configuration names are compared.
func (r *subscriptionResource) toModel(s *client.Subscription, m *subscriptionModel) {
	m.ID = types.StringValue(s.ID)
	m.SpaceID = types.StringValue(s.SpaceID)
	m.Name = types.StringValue(s.Name)
	m.ConnectionID = types.StringValue(s.ConnectionID)
	m.Follow = types.StringValue(s.Follow)
	m.Enabled = types.BoolValue(s.Enabled)
	m.Subject = types.StringValue(s.Subject)
	m.Durable = types.StringValue(s.Durable)
	m.Consumer = types.StringValue(s.Consumer)
	m.Grant = types.StringValue(s.Grant)
	tmplID, verID := "", ""
	if s.Template != nil {
		tmplID, verID = s.Template.TemplateID, s.Template.VersionID
	}
	followed := s.Follow == "latest" && !m.TemplateVersionID.IsNull() && !m.TemplateVersionID.IsUnknown() && strOrEmpty(m.TemplateID) == tmplID && tmplID != ""
	if !followed {
		m.TemplateVersionID = types.StringValue(verID)
	}
	m.TemplateID = types.StringValue(tmplID)
	if s.Custom != nil {
		m.Custom = definitionValue(m.Custom, s.Custom)
	} else {
		m.Custom = types.StringNull()
	}
	vars := s.Vars
	if !m.Vars.IsNull() && !m.Vars.IsUnknown() {
		known := map[string]string{}
		for k := range m.Vars.Elements() {
			if v, ok := s.Vars[k]; ok {
				known[k] = v
			}
		}
		vars = known
	}
	m.Vars = mapValue(vars)
}

func (r *subscriptionResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var m subscriptionModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	in := client.CreateSubscriptionInput{Name: m.Name.ValueString(), ConnectionID: m.ConnectionID.ValueString(), TemplateVersionID: strOrEmpty(m.TemplateVersionID),
		Follow: strOrEmpty(m.Follow), Vars: stringMap(ctx, m.Vars, &resp.Diagnostics), Start: r.cursor(ctx, m.Start, &resp.Diagnostics)}
	if c := strOrEmpty(m.Custom); c != "" {
		def, err := parseDefinition(c)
		if err != nil {
			resp.Diagnostics.AddAttributeError(path.Root("custom"), "Invalid custom definition", err.Error())
			return
		}
		in.Custom = def
	}
	if resp.Diagnostics.HasError() {
		return
	}
	s, err := r.c.CreateSubscription(ctx, m.SpaceID.ValueString(), in)
	if err != nil {
		addAPIError(&resp.Diagnostics, "Could not create the subscription", err)
		return
	}
	if !m.Enabled.ValueBool() { // created enabled; disabling is an update
		resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), s.ID)...)
		resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("space_id"), s.SpaceID)...)
		if s, err = r.c.UpdateSubscription(ctx, s.SpaceID, s.ID, s.Version, client.UpdateSubscriptionInput{Enabled: false}); err != nil {
			addAPIError(&resp.Diagnostics, "Could not disable the new subscription", err)
			return
		}
	}
	m.TemplateID = types.StringNull()
	r.toModel(s, &m)
	setVersion(ctx, resp.Private, s.Version, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *subscriptionResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var m subscriptionModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	s, err := r.c.GetSubscription(ctx, m.SpaceID.ValueString(), m.ID.ValueString())
	if client.IsNotFound(err) {
		resp.State.RemoveResource(ctx)
		return
	} else if err != nil {
		addAPIError(&resp.Diagnostics, "Could not read the subscription", err)
		return
	}
	r.toModel(s, &m)
	setVersion(ctx, resp.Private, s.Version, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *subscriptionResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var m, prior subscriptionModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &prior)...)
	if resp.Diagnostics.HasError() {
		return
	}
	in := client.UpdateSubscriptionInput{Name: m.Name.ValueString(), Enabled: m.Enabled.ValueBool(), Follow: strOrEmpty(m.Follow),
		Vars: stringMap(ctx, m.Vars, &resp.Diagnostics)}
	if c := strOrEmpty(m.Custom); c != "" {
		def, err := parseDefinition(c)
		if err != nil {
			resp.Diagnostics.AddAttributeError(path.Root("custom"), "Invalid custom definition", err.Error())
			return
		}
		in.Custom = def
	} else if !m.TemplateVersionID.Equal(prior.TemplateVersionID) {
		in.TemplateVersionID = strOrEmpty(m.TemplateVersionID)
	}
	if !m.Start.Equal(prior.Start) {
		in.Start = r.cursor(ctx, m.Start, &resp.Diagnostics)
	}
	if resp.Diagnostics.HasError() {
		return
	}
	v := getVersion(ctx, req.Private, &resp.Diagnostics)
	if v == 0 {
		cur, err := r.c.GetSubscription(ctx, prior.SpaceID.ValueString(), prior.ID.ValueString())
		if err != nil {
			addAPIError(&resp.Diagnostics, "Could not read the subscription", err)
			return
		}
		v = cur.Version
	}
	s, err := r.c.UpdateSubscription(ctx, prior.SpaceID.ValueString(), prior.ID.ValueString(), v, in)
	if client.IsConflict(err) {
		// Publishing a template in the same apply moves following subscriptions, which bumps their version. That move
		// is the only change tolerated here; any other change since the refresh is a real conflict.
		if cur, gerr := r.c.GetSubscription(ctx, prior.SpaceID.ValueString(), prior.ID.ValueString()); gerr == nil && onlyFollowed(&prior, cur) {
			s, err = r.c.UpdateSubscription(ctx, prior.SpaceID.ValueString(), prior.ID.ValueString(), cur.Version, in)
		}
	}
	if err != nil {
		addAPIError(&resp.Diagnostics, "Could not update the subscription", err)
		return
	}
	m.TemplateID = prior.TemplateID
	r.toModel(s, &m)
	setVersion(ctx, resp.Private, s.Version, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

// onlyFollowed reports whether cur differs from the refreshed state only by SpaceMan moving a following subscription
// to a newer version of the same template.
func onlyFollowed(prior *subscriptionModel, cur *client.Subscription) bool {
	if cur.Follow != "latest" || prior.Follow.ValueString() != "latest" || cur.Template == nil || cur.Template.TemplateID != prior.TemplateID.ValueString() {
		return false
	}
	if cur.Name != prior.Name.ValueString() || cur.Enabled != prior.Enabled.ValueBool() || cur.ConnectionID != prior.ConnectionID.ValueString() ||
		cur.Subject != prior.Subject.ValueString() || cur.Custom != nil {
		return false
	}
	for k, v := range prior.Vars.Elements() {
		if sv, ok := v.(types.String); !ok || cur.Vars[k] != sv.ValueString() {
			return false
		}
	}
	return true
}

func (r *subscriptionResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var m subscriptionModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.c.DeleteSubscription(ctx, m.SpaceID.ValueString(), m.ID.ValueString()); err != nil {
		addAPIError(&resp.Diagnostics, "Could not delete the subscription", err)
	}
}

func (r *subscriptionResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	p := splitImportID(req.ID, 2, "<space_id>/<subscription_id>", &resp.Diagnostics)
	if p == nil {
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("space_id"), p[0])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), p[1])...)
}
