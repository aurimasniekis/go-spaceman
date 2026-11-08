package spacemanmcp

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/aurimasniekis/go-spaceman/internal/api"
	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/approvals"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/buildinfo"
	"github.com/aurimasniekis/go-spaceman/internal/gateway"
	"github.com/aurimasniekis/go-spaceman/internal/human"
	"github.com/aurimasniekis/go-spaceman/internal/store"
)

// EndpointApprovalRole is the requester role recorded on approvals the external MCP endpoint asks for.
const EndpointApprovalRole = "mcp_endpoint"

// maxEndpointArgs bounds one call's arguments, as the gateway does for runs.
const maxEndpointArgs = 256 << 10

// EndpointTokens reads a bearer token's grants (auth.Service).
type EndpointTokens interface {
	GetAPIToken(ctx context.Context, id string) (*auth.APIToken, error)
}

// Authorizer checks a principal's permission in a domain (authz.Engine).
type Authorizer interface {
	AllowedAll(subjects []string, domain string, perm authz.Permission) bool
}

// Approver asks a person to approve one call (approvals.Service).
type Approver interface {
	RequireFor(ctx context.Context, r approvals.Requester, op, resource string, args json.RawMessage) (func(error), error)
}

// Endpoint is the external MCP endpoint (api.MCPPath, ADR-0063 §External endpoint, SM-MCP-009): the SpaceMan MCP's
// catalog and executor served over streamable HTTP to an operator's own MCP client, which authenticates with the
// operator's API token as a bearer. The API's guard (api.Deps.mcpEndpoint) has already refused browsers, sessions, the
// root token and internal tokens, and marked the principal MCPEndpoint.
//
//   - Tools: every catalog tool of the read, write and dangerous classes that the token's grants can reach, minus the
//     dangerous tools that act on no space (an approval belongs to a space). The token's grants are the selection: the
//     endpoint has no settings of its own. Sensitive reads (recordings, transcripts) are not offered.
//   - Approvals: every dangerous call, and a launch_run asking for a diagnostic run, needs a person's approval
//     (approvals.RequireFor) bound to the token and the exact arguments; the token's own user can never decide it.
//     Read and write calls run at once, as they would with the same token on the API.
//   - Audit: each operation audits itself with the actor "via api_token:<id>" and the channel "mcp_endpoint"; each
//     tool call is audited as mcp_endpoint.tool_called, without arguments or results.
type Endpoint struct {
	Cat *Catalog
	// Handler is api.Deps.InProcessHandler().
	Handler   http.Handler
	Tokens    EndpointTokens
	Approvals Approver
	// Authz checks, before an approval is asked for, that the token could make the call in that space: nobody is
	// asked about a call the API would refuse anyway.
	Authz Authorizer
	Store *store.Store
	Audit *audit.Writer
	Log   *slog.Logger
	// MaxResultBytes caps a result (DefaultMaxResultBytes when zero).
	MaxResultBytes int
}

var endpointOptions = &mcp.StreamableHTTPOptions{
	// No session state: every request carries its own bearer, and the server is built for it.
	Stateless:    true,
	JSONResponse: true,
	// Central is usually reached through a reverse proxy on the same host, so a localhost peer with a public Host is
	// normal. DNS rebinding needs a browser, which the API's guard refuses (Origin and Sec-Fetch-*), and a bearer
	// token, which a browser page does not have.
	DisableLocalhostProtection: true,
}

// ServeHTTP serves one MCP request for the principal the API's guard passed on.
func (e *Endpoint) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := api.PrincipalFrom(r.Context())
	if p == nil || !p.MCPEndpoint || p.User == nil || p.TokenID == "" || p.Internal() {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	tok, err := e.Tokens.GetAPIToken(r.Context(), p.TokenID)
	if err != nil {
		e.log().Error("MCP endpoint token", slog.Any("error", err))
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	srv := e.server(p, tok.Grants)
	mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, endpointOptions).ServeHTTP(w, r)
}

func (e *Endpoint) log() *slog.Logger {
	if e.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return e.Log
}

// EndpointTools returns the tools the endpoint offers a token with these grants, sorted by name.
func (e *Endpoint) EndpointTools(grants []authz.Grant) []*Tool {
	tools, err := e.Cat.Select(Selection{Preset: PresetAdmin}, grants)
	if err != nil {
		return nil
	}
	return slices.DeleteFunc(tools, func(t *Tool) bool {
		return t.Class == Dangerous && !slices.Contains(t.PathParams, "space_id")
	})
}

const endpointInstructions = "SpaceMan's administrative API as tools, acting with your API token's grants. Results are JSON " +
	"{status, etag, body}. Calls marked as needing approval ask another person first: the call returns the approval " +
	"request, and once it is approved you call the tool again with exactly the same arguments."

func (e *Endpoint) server(p *auth.Principal, grants []authz.Grant) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "spaceman", Version: buildinfo.Get().Version}, &mcp.ServerOptions{Instructions: endpointInstructions})
	for _, tl := range e.EndpointTools(grants) {
		desc := tl.Description
		var destructive *bool
		if tl.ApprovalRequired() {
			desc += " Every call needs another person's approval."
			destructive = new(bool)
			*destructive = true
		}
		s.AddTool(&mcp.Tool{Name: tl.Name, Description: desc, InputSchema: tl.InputSchema,
			Annotations: &mcp.ToolAnnotations{ReadOnlyHint: tl.ReadOnly(), DestructiveHint: destructive}},
			func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				var args json.RawMessage
				if req.Params != nil {
					args = req.Params.Arguments
				}
				return e.call(ctx, p, grants, tl, args), nil
			})
	}
	return s
}

// actor is the audit actor of an endpoint call (as api.Actor returns for the principal).
func endpointActor(p *auth.Principal) audit.Actor {
	return audit.Actor{Type: audit.ActorUser, ID: p.User.ID, Via: "api_token:" + p.TokenID, Channel: audit.ChannelMCPEndpoint}
}

// call runs one tool call. Every outcome is a tool result the model can read; only the audit records it besides.
func (e *Endpoint) call(ctx context.Context, p *auth.Principal, grants []authz.Grant, tl *Tool, args json.RawMessage) *mcp.CallToolResult {
	start := time.Now()
	class := tl.Class
	if tl.OperationID == "launch-run" && diagnosticLaunch(args) {
		class = Dangerous
	}
	details := map[string]any{"tool": tl.Name, "operation": tl.OperationID, "class": string(class)}
	res, err := e.run(ctx, p, grants, tl, class, args, details)
	outcome, result := audit.OutcomeSuccess, "ok"
	switch {
	case errors.Is(err, approvals.ErrRequired):
		outcome, result = audit.OutcomeDenied, "approval_required"
	case err != nil:
		outcome, result = audit.OutcomeFailure, string(apperr.CodeOf(err))
	case res.IsError:
		outcome, result = audit.OutcomeFailure, "api_error"
	}
	details["result"], details["duration_ms"] = result, time.Since(start).Milliseconds()
	e.log().Info("MCP endpoint call", slog.String("token.id", p.TokenID), slog.String("mcp.tool", tl.Name), slog.String("result", result),
		slog.Duration("duration", time.Since(start)))
	if aerr := e.Store.W().TxNamed(context.WithoutCancel(ctx), "mcp_endpoint.call", func(tx *store.Tx) error {
		_, err := e.Audit.Append(ctx, tx, audit.Event{Actor: endpointActor(p), Action: "mcp_endpoint.tool_called",
			Target: audit.Target{Type: "api_token", ID: p.TokenID}, Outcome: outcome, Details: details})
		return err
	}); aerr != nil {
		e.log().Error("audit MCP endpoint call", slog.Any("error", aerr))
	}
	if err != nil {
		var ae *apperr.Error
		msg := "internal error"
		if errors.As(err, &ae) {
			msg = err.Error()
		} else {
			e.log().Error("MCP endpoint call failed", slog.String("mcp.tool", tl.Name), slog.Any("error", err))
		}
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: msg}}}
	}
	return res
}

func (e *Endpoint) run(ctx context.Context, p *auth.Principal, grants []authz.Grant, tl *Tool, class Class, args json.RawMessage,
	details map[string]any) (*mcp.CallToolResult, error) {
	if len(args) > maxEndpointArgs {
		return nil, apperr.New(apperr.CodeInvalid, "arguments larger than %d bytes", maxEndpointArgs)
	}
	def := defaultSpace(grants)
	var finish func(error)
	if class == Dangerous {
		space := spaceArg(args)
		if space == "" {
			space = def
		}
		if space == "" {
			return nil, apperr.New(apperr.CodeInvalid, "%s needs a person's approval in a space: pass space_id", tl.Name)
		}
		details["space_id"] = space
		if e.Authz == nil || !authz.ValidDomain(space) || space == authz.Global || !e.Authz.AllowedAll(p.Subjects(), space, tl.Rule.Perm) {
			return nil, apperr.New(apperr.CodeNotFound, "space not found, or this token cannot use %s there", tl.Name)
		}
		if e.Approvals == nil {
			return nil, apperr.New(apperr.CodePrecondition, "%s needs a person's approval, and approvals are not available", tl.Name)
		}
		label := p.User.DisplayName
		if label == "" {
			label = p.User.Email
		}
		var err error
		finish, err = e.Approvals.RequireFor(ctx, approvals.Requester{SpaceID: space, Key: approvals.TokenRequesterPrefix + p.TokenID,
			Role: EndpointApprovalRole, Actor: endpointActor(p), SelfUserID: p.User.ID,
			Human: human.Requester{Kind: "operator", ID: p.User.ID, Label: label + " (MCP endpoint)"}},
			"mcp.endpoint."+string(Dangerous)+"."+tl.Name, "api_token:"+p.TokenID, args)
		if err != nil {
			return nil, err
		}
		// The API refuses a diagnostic launch through the endpoint without this mark: the kind may come from the space
		// role, which only the API resolves (api.Deps.authorizeLaunchKind).
		ctx = gateway.WithApprovedCall(ctx, tl.OperationID)
	}
	ex := &Executor{Handler: e.Handler, MaxResultBytes: e.MaxResultBytes}
	res, err := ex.Execute(ctx, Call{Principal: p, Tool: tl, Args: args, DefaultSpaceID: def})
	if finish != nil {
		switch {
		case err != nil:
			finish(err)
		case res.IsError:
			finish(apperr.New(apperr.CodeInvalid, "the operation failed"))
		default:
			finish(nil)
		}
	}
	return res, err
}

// spaceArg returns a call's space_id argument as the executor reads it, or "".
func spaceArg(args json.RawMessage) string {
	m := map[string]json.RawMessage{}
	if json.Unmarshal(args, &m) != nil {
		return ""
	}
	v, _, err := scalar(m, "space_id")
	if err != nil {
		return ""
	}
	return v
}
