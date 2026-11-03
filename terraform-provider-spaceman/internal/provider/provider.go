// Package provider is the SpaceMan Terraform provider (SM-API-004/005): resources for SpaceMan's durable configuration
// (spaces, roles, catalogs, credentials references, events, human-action types and policies, approvals, knowledge
// bases, dreams, limits, schedules, cross-space grants, users and sign-in), never for runtime state (runs, tasks, inbox items, human-action requests and responses).
package provider

import (
	"context"
	"os"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/aurimasniekis/go-spaceman/terraform-provider-spaceman/internal/client"
)

// Environment variables read when the provider block leaves a value out.
const (
	EnvEndpoint = "SPACEMAN_ENDPOINT"
	EnvToken    = "SPACEMAN_TOKEN"
)

// New returns the provider factory for version.
func New(version string) func() provider.Provider {
	return func() provider.Provider { return &spacemanProvider{version: version} }
}

type spacemanProvider struct {
	version string
}

type providerModel struct {
	Endpoint          types.String `tfsdk:"endpoint"`
	Token             types.String `tfsdk:"token"`
	AllowInsecureHTTP types.Bool   `tfsdk:"allow_insecure_http"`
	RequestTimeout    types.Int64  `tfsdk:"request_timeout_seconds"`
}

func (p *spacemanProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "spaceman"
	resp.Version = p.version
}

func (p *spacemanProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages the durable configuration of a [SpaceMan](https://aurimasniekis.github.io/go-spaceman/) central " +
			"through its administrative API. Runtime state (runs, tasks, inbox items, human-action requests and responses) is never managed.",
		Attributes: map[string]schema.Attribute{
			"endpoint": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "The central's public URL, e.g. `https://spaceman.example.com` (the provider adds `/api/v1`). " +
					"Defaults to the `" + EnvEndpoint + "` environment variable.",
			},
			"token": schema.StringAttribute{
				Optional:  true,
				Sensitive: true,
				MarkdownDescription: "An API token (`spaceman_at_…`) or the bootstrap root token (`spaceman_rt_…`), sent as a bearer token. " +
					"Defaults to the `" + EnvToken + "` environment variable. Prefer an API token of a dedicated automation user.",
			},
			"allow_insecure_http": schema.BoolAttribute{
				Optional: true,
				MarkdownDescription: "Allow a plain `http://` endpoint on a host that is not a loopback address. The token then crosses the " +
					"network unencrypted; off by default.",
			},
			"request_timeout_seconds": schema.Int64Attribute{
				Optional:            true,
				MarkdownDescription: "Timeout of one API request in seconds (default 60).",
			},
		},
	}
}

func (p *spacemanProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var m providerModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if m.Endpoint.IsUnknown() || m.Token.IsUnknown() {
		resp.Diagnostics.AddError("Provider configuration is not known yet",
			"endpoint and token must be known when the provider is configured (they cannot depend on resources created in the same apply).")
		return
	}
	endpoint := m.Endpoint.ValueString()
	if endpoint == "" {
		endpoint = os.Getenv(EnvEndpoint)
	}
	token := m.Token.ValueString()
	if token == "" {
		token = os.Getenv(EnvToken)
	}
	if endpoint == "" {
		resp.Diagnostics.AddAttributeError(path.Root("endpoint"), "Missing SpaceMan endpoint",
			"Set endpoint in the provider block or the "+EnvEndpoint+" environment variable.")
	}
	if token == "" {
		resp.Diagnostics.AddAttributeError(path.Root("token"), "Missing SpaceMan token",
			"Set token in the provider block or the "+EnvToken+" environment variable.")
	}
	if resp.Diagnostics.HasError() {
		return
	}
	timeout := time.Duration(int64OrZero(m.RequestTimeout)) * time.Second
	c, err := client.New(endpoint, token, client.Options{AllowInsecureHTTP: m.AllowInsecureHTTP.ValueBool(), Timeout: timeout,
		UserAgent: "terraform-provider-spaceman/" + p.version})
	if err != nil {
		resp.Diagnostics.AddError("Invalid SpaceMan provider configuration", err.Error())
		return
	}
	resp.ResourceData = c
	resp.DataSourceData = c
}

func (p *spacemanProvider) Resources(context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		newSpaceResource,
		newSpaceRoleResource,
		newSpaceAccountsResource,
		newSpaceRepositoryResource,
		newAccountResource,
		newHarnessProfileResource,
		newRoleTemplateResource,
		newPromptResource,
		newSkillResource,
		newMCPServerResource,
		newMCPGrantResource,
		newEventConnectionResource,
		newEventTemplateResource,
		newEventSubscriptionResource,
		newHumanActionTypeResource,
		newSpaceHumanActionTypeResource,
		newApprovalPolicyResource,
		newKnowledgeBaseResource,
		newDreamResource,
		newOIDCProviderResource,
		newOIDCGroupMappingResource,
		newSpaceLimitsResource,
		newQueuePolicyResource,
		newScheduleResource,
		newXSpaceSettingsResource,
		newXSpacePeerResource,
		newHumanPolicyResource,
		newUserResource,
		newUserGrantsResource,
		newUserIdentityResource,
	}
}

func (p *spacemanProvider) DataSources(context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{
		newNodeDataSource,
		newNodesDataSource,
		newSpaceDataSource,
		newSpacesDataSource,
		newEventTemplateDataSource,
		newSpaceLimitsDataSource,
		newUserDataSource,
	}
}
