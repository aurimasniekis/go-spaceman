package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/gateway"
)

// GatewayRules are the authorization rules of the MCP gateway operations. Granting a server to a space hands its
// credentials' reach to that space's runs, so grants need mcp:write (global), not only space configuration rights.
var GatewayRules = map[string]Rule{
	"list-mcp-servers":      Global(authz.MCPRead),
	"get-mcp-server":        Global(authz.MCPRead),
	"create-mcp-server":     Global(authz.MCPWrite),
	"update-mcp-server":     Global(authz.MCPWrite),
	"delete-mcp-server":     Global(authz.MCPWrite),
	"check-mcp-server":      Global(authz.MCPWrite),
	"list-mcp-grants":       Global(authz.MCPRead),
	"add-mcp-grant":         Global(authz.MCPWrite),
	"revoke-mcp-grant":      Global(authz.MCPWrite),
	"list-space-mcp-grants": InSpace(authz.MCPRead),
	// SpaceMan MCP servers (ADR-0063) hand an administrator's authority to agents: creating, editing and re-issuing one
	// needs a step-up, like creating an API token.
	"create-spaceman-mcp":  StepUp(authz.MCPWrite),
	"update-spaceman-mcp":  StepUp(authz.MCPWrite),
	"reissue-spaceman-mcp": StepUp(authz.MCPWrite),
}

// MCPServerPath identifies an MCP server.
type MCPServerPath struct {
	ServerID string `path:"server_id" maxLength:"64"`
}

// MCPServerBody creates or edits an MCP server. Header values are write-only.
type MCPServerBody struct {
	Slug           string            `json:"slug,omitempty" maxLength:"31" doc:"Namespace of the server's tools (<slug>__<tool>); required on create, fixed after"`
	Name           string            `json:"name" maxLength:"120"`
	Description    string            `json:"description,omitempty" maxLength:"2000"`
	URL            string            `json:"url" maxLength:"2048" doc:"Streamable HTTP endpoint; https unless private networks are allowed"`
	SetHeaders     bool              `json:"set_headers,omitempty" doc:"On an edit, replace the stored headers with headers (empty clears them)"`
	Headers        map[string]string `json:"headers,omitempty" writeOnly:"true" doc:"Request headers such as Authorization; sealed and never returned"`
	AllowPrivate   bool              `json:"allow_private,omitempty" doc:"Allow loopback and private addresses and plain http. Link-local addresses stay refused."`
	Tools          []string          `json:"tools,omitempty" maxItems:"200" doc:"Upstream tools offered; empty offers all"`
	CallTimeoutMS  int64             `json:"call_timeout_ms,omitempty"`
	MaxConcurrency int64             `json:"max_concurrency,omitempty"`
	MaxResultBytes int64             `json:"max_result_bytes,omitempty"`
	Enabled        *bool             `json:"enabled,omitempty" doc:"Default true"`
}

func (b MCPServerBody) input() gateway.Input {
	return gateway.Input{Slug: b.Slug, Name: b.Name, Description: b.Description, URL: b.URL, SetHeaders: b.SetHeaders, Headers: b.Headers,
		AllowPrivate: b.AllowPrivate, Tools: b.Tools, CallTimeoutMS: b.CallTimeoutMS, MaxConcurrency: b.MaxConcurrency, MaxResultBytes: b.MaxResultBytes,
		Enabled: b.Enabled == nil || *b.Enabled}
}

// MCPGrantBody grants a server to a space role.
type MCPGrantBody struct {
	SpaceID string   `json:"space_id" maxLength:"64"`
	Role    string   `json:"role,omitempty" maxLength:"48" doc:"A space role name; * or empty for every role"`
	Tools   []string `json:"tools,omitempty" maxItems:"200" doc:"Subset of the server's tools; empty grants all"`
}

type mcpServerOut struct {
	ETag string `header:"ETag"`
	Body *gateway.Server
}

// RegisterGateway registers the MCP gateway operations.
func RegisterGateway(svc *gateway.Service) func(api huma.API, d *Deps) {
	return func(api huma.API, d *Deps) {
		tags := []string{"mcp"}
		huma.Register(api, huma.Operation{OperationID: "list-mcp-servers", Method: http.MethodGet, Path: "/mcp-servers", Summary: "External MCP servers (never their header values)", Tags: tags},
			func(ctx context.Context, _ *struct{}) (*struct {
				Body struct {
					Items []*gateway.Server `json:"items"`
				}
			}, error) {
				l, err := svc.List(ctx)
				if err != nil {
					return nil, d.fail(ctx, "mcp.list", err)
				}
				out := &struct {
					Body struct {
						Items []*gateway.Server `json:"items"`
					}
				}{}
				out.Body.Items = l
				return out, nil
			})
		huma.Register(api, huma.Operation{OperationID: "get-mcp-server", Method: http.MethodGet, Path: "/mcp-servers/{server_id}", Summary: "An MCP server", Tags: tags},
			func(ctx context.Context, in *MCPServerPath) (*mcpServerOut, error) {
				s, err := svc.Get(ctx, in.ServerID)
				if err != nil {
					return nil, d.fail(ctx, "mcp.get", err)
				}
				return &mcpServerOut{ETag: ETag(s.Version), Body: s}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "create-mcp-server", Method: http.MethodPost, Path: "/mcp-servers", Summary: "Register an MCP server",
			Description: "Header values are sealed and can never be read back. Agents see the server's tools only through a grant.", Tags: tags, DefaultStatus: http.StatusCreated},
			func(ctx context.Context, in *struct {
				IdempotencyKey
				Body MCPServerBody
			}) (*mcpServerOut, error) {
				fp := map[string]any{"slug": in.Body.Slug, "url": in.Body.URL}
				s, _, err := Idempotent(ctx, d, "create-mcp-server", in.IdempotencyKey, fp, false, func() (*gateway.Server, error) {
					return svc.Create(ctx, Actor(ctx), in.Body.input())
				})
				if err != nil {
					return nil, d.fail(ctx, "mcp.create", err)
				}
				return &mcpServerOut{ETag: ETag(s.Version), Body: s}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "update-mcp-server", Method: http.MethodPut, Path: "/mcp-servers/{server_id}", Summary: "Edit an MCP server",
			Description: "Open upstream sessions are closed; runs reconnect on their next call.", Tags: tags},
			func(ctx context.Context, in *struct {
				MCPServerPath
				IfMatch
				Body MCPServerBody
			}) (*mcpServerOut, error) {
				v, err := in.Version()
				if err != nil {
					return nil, d.fail(ctx, "mcp.update", err)
				}
				s, err := svc.Update(ctx, Actor(ctx), in.ServerID, v, in.Body.input())
				if err != nil {
					return nil, d.fail(ctx, "mcp.update", err)
				}
				return &mcpServerOut{ETag: ETag(s.Version), Body: s}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "delete-mcp-server", Method: http.MethodDelete, Path: "/mcp-servers/{server_id}", Summary: "Remove an MCP server and its grants",
			Tags: tags, DefaultStatus: http.StatusNoContent},
			func(ctx context.Context, in *MCPServerPath) (*struct{}, error) {
				if err := svc.Delete(ctx, Actor(ctx), in.ServerID); err != nil {
					return nil, d.fail(ctx, "mcp.delete", err)
				}
				return &struct{}{}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "check-mcp-server", Method: http.MethodPost, Path: "/mcp-servers/{server_id}/check",
			Summary: "Connect to the server and list its tools", Description: "Uses a throwaway session under the same egress policy as runs.", Tags: tags},
			func(ctx context.Context, in *MCPServerPath) (*struct{ Body *gateway.Discovered }, error) {
				r, err := svc.Check(ctx, Actor(ctx), in.ServerID)
				if err != nil {
					return nil, d.fail(ctx, "mcp.check", err)
				}
				return &struct{ Body *gateway.Discovered }{Body: r}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "list-mcp-grants", Method: http.MethodGet, Path: "/mcp-servers/{server_id}/grants", Summary: "Spaces and roles granted the server", Tags: tags},
			func(ctx context.Context, in *MCPServerPath) (*struct {
				Body struct {
					Items []*gateway.Grant `json:"items"`
				}
			}, error) {
				l, err := svc.Grants(ctx, in.ServerID)
				if err != nil {
					return nil, d.fail(ctx, "mcp.grants", err)
				}
				out := &struct {
					Body struct {
						Items []*gateway.Grant `json:"items"`
					}
				}{}
				out.Body.Items = l
				return out, nil
			})
		huma.Register(api, huma.Operation{OperationID: "add-mcp-grant", Method: http.MethodPost, Path: "/mcp-servers/{server_id}/grants", Summary: "Grant the server to a space role",
			Tags: tags, DefaultStatus: http.StatusCreated},
			func(ctx context.Context, in *struct {
				MCPServerPath
				Body MCPGrantBody
			}) (*struct{ Body *gateway.Grant }, error) {
				g, err := svc.AddGrant(ctx, Actor(ctx), in.ServerID, in.Body.SpaceID, in.Body.Role, in.Body.Tools)
				if err != nil {
					return nil, d.fail(ctx, "mcp.grant", err)
				}
				return &struct{ Body *gateway.Grant }{Body: g}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "revoke-mcp-grant", Method: http.MethodDelete, Path: "/mcp-servers/{server_id}/grants/{grant_id}",
			Summary: "Revoke a grant", Description: "Calls in flight finish; the next call of an affected run is refused.", Tags: tags, DefaultStatus: http.StatusNoContent},
			func(ctx context.Context, in *struct {
				MCPServerPath
				GrantID string `path:"grant_id" maxLength:"64"`
			}) (*struct{}, error) {
				if err := svc.RevokeGrant(ctx, Actor(ctx), in.ServerID, in.GrantID); err != nil {
					return nil, d.fail(ctx, "mcp.revoke", err)
				}
				return &struct{}{}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "create-spaceman-mcp", Method: http.MethodPost, Path: "/mcp-servers/spaceman",
			Summary: "Add a SpaceMan MCP server", Description: "SpaceMan's own API as MCP tools for a space's control and worker runs (ADR-0063). " +
				"The server acts through a new internal API token owned by you, with grants covered by yours (default: admin on every space) " +
				"and no secret. Dangerous tools always need a person's approval. Grant the server to a space role for its runs to see the tools.",
			Tags: tags, DefaultStatus: http.StatusCreated},
			func(ctx context.Context, in *struct {
				IdempotencyKey
				Body gateway.SpacemanInput
			}) (*mcpServerOut, error) {
				fp := map[string]any{"slug": in.Body.Slug, "kind": gateway.KindSpaceman}
				s, _, err := Idempotent(ctx, d, "create-spaceman-mcp", in.IdempotencyKey, fp, false, func() (*gateway.Server, error) {
					return svc.CreateSpaceman(ctx, PrincipalFrom(ctx), Actor(ctx), in.Body)
				})
				if err != nil {
					return nil, d.fail(ctx, "mcp.create_spaceman", err)
				}
				return &mcpServerOut{ETag: ETag(s.Version), Body: s}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "update-spaceman-mcp", Method: http.MethodPut, Path: "/mcp-servers/{server_id}/spaceman",
			Summary: "Edit a SpaceMan MCP server", Description: "Changes the tools, the approval mode and the limits. The internal token is unchanged: " +
				"re-issue the server to change its grants, expiry or owner.", Tags: tags},
			func(ctx context.Context, in *struct {
				MCPServerPath
				IfMatch
				Body gateway.SpacemanUpdate
			}) (*mcpServerOut, error) {
				v, err := in.Version()
				if err != nil {
					return nil, d.fail(ctx, "mcp.update_spaceman", err)
				}
				s, err := svc.UpdateSpaceman(ctx, Actor(ctx), in.ServerID, v, in.Body)
				if err != nil {
					return nil, d.fail(ctx, "mcp.update_spaceman", err)
				}
				return &mcpServerOut{ETag: ETag(s.Version), Body: s}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "reissue-spaceman-mcp", Method: http.MethodPost, Path: "/mcp-servers/{server_id}/reissue",
			Summary: "Re-issue a SpaceMan MCP server's token", Description: "Revokes the server's internal token and mints a new one owned by you, " +
				"in one step: for new grants, a new expiry or a new owner.", Tags: tags},
			func(ctx context.Context, in *struct {
				MCPServerPath
				IfMatch
				Body gateway.SpacemanToken
			}) (*mcpServerOut, error) {
				v, err := in.Version()
				if err != nil {
					return nil, d.fail(ctx, "mcp.reissue_spaceman", err)
				}
				s, err := svc.ReissueSpaceman(ctx, PrincipalFrom(ctx), Actor(ctx), in.ServerID, v, in.Body)
				if err != nil {
					return nil, d.fail(ctx, "mcp.reissue_spaceman", err)
				}
				return &mcpServerOut{ETag: ETag(s.Version), Body: s}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "list-space-mcp-grants", Method: http.MethodGet, Path: "/spaces/{space_id}/mcp-servers",
			Summary: "MCP servers granted in a space", Tags: tags},
			func(ctx context.Context, in *SpacePath) (*struct {
				Body struct {
					Items []*gateway.SpaceServer `json:"items"`
				}
			}, error) {
				l, err := svc.SpaceGrants(ctx, in.SpaceID)
				if err != nil {
					return nil, d.fail(ctx, "mcp.space", err)
				}
				out := &struct {
					Body struct {
						Items []*gateway.SpaceServer `json:"items"`
					}
				}{}
				out.Body.Items = l
				return out, nil
			})
	}
}
