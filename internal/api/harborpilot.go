package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/harborpilot"
)

// HarborPilotRules are the authorization rules of the Harbor Pilot gateway (ADR-0060 decision 2). The connection holds
// a parent token that mints run tokens for every opted-in space, so it is managed with account:write, like harness
// accounts, and its writes need a step-up like event connections; a space's opt-in is space configuration, except
// its project (who pays), which needs account:write too (checked in the handler: it depends on the body).
var HarborPilotRules = map[string]Rule{
	"get-harbor-pilot-connection":    Global(authz.AccountRead),
	"get-harbor-pilot-gateway":       Global(authz.AccountRead),
	"put-harbor-pilot-connection":    StepUp(authz.AccountWrite),
	"delete-harbor-pilot-connection": StepUp(authz.AccountWrite),
	"get-space-harbor-pilot":         InSpace(authz.SpaceConfigRead),
	"put-space-harbor-pilot":         InSpace(authz.SpaceConfigWrite),
	"delete-space-harbor-pilot":      InSpace(authz.SpaceConfigWrite),
	"get-space-harbor-pilot-status":  InSpace(authz.SpaceConfigRead),
}

type harborPilotConnectionOut struct {
	ETag string `header:"ETag"`
	Body *harborpilot.Connection
}

type harborPilotGatewayOut struct {
	Body *harborpilot.GatewayInfo
}

type spaceHarborPilotOut struct {
	ETag string `header:"ETag"`
	Body *harborpilot.SpaceSettings
}

type spaceHarborPilotStatusOut struct {
	Body *harborpilot.SpaceStatus
}

// RegisterHarborPilot registers the Harbor Pilot connection, space settings and status operations.
func RegisterHarborPilot(svc *harborpilot.Service, budgets *harborpilot.Budgets) func(api huma.API, d *Deps) {
	return func(api huma.API, d *Deps) {
		tags := []string{"harbor-pilot"}
		huma.Register(api, huma.Operation{OperationID: "get-harbor-pilot-connection", Method: http.MethodGet, Path: "/harbor-pilot/connection",
			Summary: "The Harbor Pilot connection (never its tokens)", Tags: tags},
			func(ctx context.Context, _ *struct{}) (*harborPilotConnectionOut, error) {
				c, err := svc.GetConnection(ctx)
				if err != nil {
					return nil, d.fail(ctx, "harborpilot.get_connection", err)
				}
				return &harborPilotConnectionOut{ETag: ETag(c.Version), Body: c}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "get-harbor-pilot-gateway", Method: http.MethodGet, Path: "/harbor-pilot/gateway",
			Summary: "The connected Harbor Pilot's version",
			Description: "Read with the connection's service-principal token from Harbor Pilot's GET /api/v1/system (0.1.4 or later; source system). " +
				"A gateway without it is older than 0.1.4 (source legacy): the connection's check_budget probe showed it is 0.1.3 or later. Without a " +
				"service-principal token, or when Harbor Pilot can't be read, source is unknown with the reason. project_filter says whether budget " +
				"status is read filtered by project. Reused for 10 minutes.",
			Tags: tags},
			func(ctx context.Context, _ *struct{}) (*harborPilotGatewayOut, error) {
				gi, err := budgets.Gateway(ctx)
				if err != nil {
					return nil, d.fail(ctx, "harborpilot.gateway", err)
				}
				return &harborPilotGatewayOut{Body: gi}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "put-harbor-pilot-connection", Method: http.MethodPut, Path: "/harbor-pilot/connection",
			Summary: "Connect Harbor Pilot, or replace the connection",
			Description: "Without If-Match this creates the connection; with it, it replaces it. Every write is validated against Harbor Pilot first: " +
				"the parent token mints (and at once revokes) a probe credential, which shows it holds tokens:mint, which client binding it belongs to, " +
				"when it expires and that the gateway is Harbor Pilot 0.1.3 or later; a service-principal token must read budget status and projects. " +
				"Tokens are sealed and never returned (ADR-0060). Stored tokens are only sent to the address they were entered for: a change of " +
				"base_url or run_base_url, or newly allowing insecure HTTP, needs parent_token (and service_token or remove_service_token when one is " +
				"stored) in the same request, or it is refused with 422 before any call. Needs account:write and a step-up.",
			Tags: tags},
			func(ctx context.Context, in *struct {
				IfMatch
				Body harborpilot.ConnectionInput
			}) (*harborPilotConnectionOut, error) {
				v, err := optionalVersion(in.IfMatch)
				if err != nil {
					return nil, d.fail(ctx, "harborpilot.put_connection", err)
				}
				c, err := svc.PutConnection(ctx, Actor(ctx), v, in.Body)
				if err != nil {
					return nil, d.fail(ctx, "harborpilot.put_connection", err)
				}
				return &harborPilotConnectionOut{ETag: ETag(c.Version), Body: c}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "delete-harbor-pilot-connection", Method: http.MethodDelete, Path: "/harbor-pilot/connection",
			Summary: "Remove the Harbor Pilot connection", Description: "Refused while any space routes runs through Harbor Pilot, or unexpired tokens (run or validation) are live or waiting to be revoked. Needs account:write and a step-up.",
			Tags: tags, DefaultStatus: http.StatusNoContent},
			func(ctx context.Context, in *IfMatch) (*struct{}, error) {
				v, err := in.Version()
				if err != nil {
					return nil, d.fail(ctx, "harborpilot.delete_connection", err)
				}
				if err := svc.DeleteConnection(ctx, Actor(ctx), v); err != nil {
					return nil, d.fail(ctx, "harborpilot.delete_connection", err)
				}
				return &struct{}{}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "get-space-harbor-pilot", Method: http.MethodGet, Path: "/spaces/{space_id}/harbor-pilot",
			Summary: "A space's Harbor Pilot settings", Tags: tags},
			func(ctx context.Context, in *SpacePath) (*spaceHarborPilotOut, error) {
				s, err := svc.GetSpace(ctx, in.SpaceID)
				if err != nil {
					return nil, d.fail(ctx, "harborpilot.get_space", err)
				}
				return &spaceHarborPilotOut{ETag: ETag(s.Version), Body: s}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "put-space-harbor-pilot", Method: http.MethodPut, Path: "/spaces/{space_id}/harbor-pilot",
			Summary: "Set a space's Harbor Pilot settings",
			Description: "Without If-Match this creates the settings; with it, it replaces them. Enabling needs the connection, and the project is checked " +
				"against Harbor Pilot: it must be the connection's binding project or one of its allowed projects. For the listed harnesses the gateway " +
				"replaces accounts; there is no fallback to a direct account (ADR-0060). The rest needs space_config:write, but setting or changing " +
				"project_id (who pays) needs account:write; an empty project_id on an update keeps the stored project.",
			Tags: tags},
			func(ctx context.Context, in *struct {
				SpacePath
				IfMatch
				Body harborpilot.SpaceInput
			}) (*spaceHarborPilotOut, error) {
				v, err := optionalVersion(in.IfMatch)
				if err != nil {
					return nil, d.fail(ctx, "harborpilot.put_space", err)
				}
				p := PrincipalFrom(ctx)
				canSetProject := d.Authz != nil && p != nil && d.Authz.AllowedAll(p.Subjects(), authz.Global, authz.AccountWrite)
				s, err := svc.PutSpace(ctx, Actor(ctx), in.SpaceID, v, in.Body, canSetProject)
				if err != nil {
					return nil, d.fail(ctx, "harborpilot.put_space", err)
				}
				return &spaceHarborPilotOut{ETag: ETag(s.Version), Body: s}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "get-space-harbor-pilot-status", Method: http.MethodGet, Path: "/spaces/{space_id}/harbor-pilot/status",
			Summary: "A space's Harbor Pilot budget status and project usage",
			Description: "The Harbor Pilot budgets covering the space (its project, the project's workspace, the connection's client binding and " +
				"global budgets; account budgets are left out), with their state, spend, limit and reset time, and the project's usage over all " +
				"Harbor Pilot clients. Budgets enforced with block are hard limits: a used-up one refuses launches, and Harbor Pilot refuses the " +
				"requests. Read with the connection's service-principal token and reused for 30 s; state unavailable says why it can't be read.",
			Tags: tags},
			func(ctx context.Context, in *SpacePath) (*spaceHarborPilotStatusOut, error) {
				st, err := budgets.SpaceStatus(ctx, in.SpaceID)
				if err != nil {
					return nil, d.fail(ctx, "harborpilot.space_status", err)
				}
				return &spaceHarborPilotStatusOut{Body: st}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "delete-space-harbor-pilot", Method: http.MethodDelete, Path: "/spaces/{space_id}/harbor-pilot",
			Summary: "Remove a space's Harbor Pilot settings (it no longer routes runs through Harbor Pilot)", Tags: tags, DefaultStatus: http.StatusNoContent},
			func(ctx context.Context, in *struct {
				SpacePath
				IfMatch
			}) (*struct{}, error) {
				v, err := in.Version()
				if err != nil {
					return nil, d.fail(ctx, "harborpilot.delete_space", err)
				}
				if err := svc.DeleteSpace(ctx, Actor(ctx), in.SpaceID, v); err != nil {
					return nil, d.fail(ctx, "harborpilot.delete_space", err)
				}
				return &struct{}{}, nil
			})
	}
}
