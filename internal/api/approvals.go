package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/aurimasniekis/go-spaceman/internal/approvals"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
)

// ApprovalRules are the authorization rules of the enforced-approval operations. Policies decide which operations
// need a person, so changing them is global administration; deciding happens on the human action (approval:decide).
var ApprovalRules = map[string]Rule{
	"list-approval-policies": Global(authz.ApprovalPolicyRead),
	"create-approval-policy": Global(authz.ApprovalPolicyWrite),
	"delete-approval-policy": Global(authz.ApprovalPolicyWrite),
	"list-approvals":         InSpace(authz.HumanRead),
	"get-approval":           InSpace(authz.HumanRead),
	"get-action-approval":    InSpace(authz.HumanRead),
	"revoke-approval":        InSpace(authz.ApprovalDecide),
}

// RegisterApprovals registers the enforced-approval operations.
func RegisterApprovals(svc *approvals.Service) func(api huma.API, d *Deps) {
	return func(api huma.API, d *Deps) {
		tags := []string{"approvals"}
		huma.Register(api, huma.Operation{OperationID: "list-approval-policies", Method: http.MethodGet, Path: "/approval-policies",
			Summary: "Operations that need a person's approval", Tags: tags},
			func(ctx context.Context, _ *struct{}) (*struct {
				Body struct {
					Items []*approvals.Policy `json:"items"`
				}
			}, error) {
				l, err := svc.Policies(ctx)
				if err != nil {
					return nil, d.fail(ctx, "approvals.policies", err)
				}
				out := &struct {
					Body struct {
						Items []*approvals.Policy `json:"items"`
					}
				}{}
				out.Body.Items = l
				return out, nil
			})
		huma.Register(api, huma.Operation{OperationID: "create-approval-policy", Method: http.MethodPost, Path: "/approval-policies",
			Summary: "Make an operation need approval", Description: "Operations are spaceman.<tool> (built-in agent tools) or mcp.<server>.<tool> (gateway tools); a prefix ending in .* covers several.",
			Tags: tags, DefaultStatus: http.StatusCreated},
			func(ctx context.Context, in *struct {
				Body approvals.PolicyInput
			}) (*struct{ Body *approvals.Policy }, error) {
				p, err := svc.CreatePolicy(ctx, Actor(ctx), in.Body)
				if err != nil {
					return nil, d.fail(ctx, "approvals.policy_create", err)
				}
				return &struct{ Body *approvals.Policy }{Body: p}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "delete-approval-policy", Method: http.MethodDelete, Path: "/approval-policies/{policy_id}",
			Summary: "Remove a policy", Description: "Approvals it already produced stay usable until they expire or are revoked.", Tags: tags, DefaultStatus: http.StatusNoContent},
			func(ctx context.Context, in *struct {
				PolicyID string `path:"policy_id" maxLength:"64"`
			}) (*struct{}, error) {
				if err := svc.DeletePolicy(ctx, Actor(ctx), in.PolicyID); err != nil {
					return nil, d.fail(ctx, "approvals.policy_delete", err)
				}
				return &struct{}{}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "list-approvals", Method: http.MethodGet, Path: "/spaces/{space_id}/approvals",
			Summary: "A space's enforced approvals with their executions", Tags: tags},
			func(ctx context.Context, in *SpacePath) (*struct {
				Body struct {
					Items []*approvals.Approval `json:"items"`
				}
			}, error) {
				l, err := svc.List(ctx, in.SpaceID, 100)
				if err != nil {
					return nil, d.fail(ctx, "approvals.list", err)
				}
				out := &struct {
					Body struct {
						Items []*approvals.Approval `json:"items"`
					}
				}{}
				out.Body.Items = l
				return out, nil
			})
		huma.Register(api, huma.Operation{OperationID: "get-approval", Method: http.MethodGet, Path: "/spaces/{space_id}/approvals/{approval_id}",
			Summary: "An enforced approval", Tags: tags},
			func(ctx context.Context, in *struct {
				SpacePath
				ApprovalID string `path:"approval_id" maxLength:"64"`
			}) (*struct{ Body *approvals.Approval }, error) {
				a, err := svc.Get(ctx, in.SpaceID, in.ApprovalID)
				if err != nil {
					return nil, d.fail(ctx, "approvals.get", err)
				}
				return &struct{ Body *approvals.Approval }{Body: a}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "get-action-approval", Method: http.MethodGet, Path: "/spaces/{space_id}/human-actions/{action_id}/approval",
			Summary: "The enforced approval behind a human action", Description: "Not found means the approval is advisory: a recorded decision that gates nothing.", Tags: tags},
			func(ctx context.Context, in *struct {
				SpacePath
				ActionID string `path:"action_id" maxLength:"64"`
			}) (*struct{ Body *approvals.Approval }, error) {
				a, err := svc.ForAction(ctx, in.SpaceID, in.ActionID)
				if err != nil {
					return nil, d.fail(ctx, "approvals.for_action", err)
				}
				return &struct{ Body *approvals.Approval }{Body: a}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "revoke-approval", Method: http.MethodPost, Path: "/spaces/{space_id}/approvals/{approval_id}/revoke",
			Summary: "Revoke an approval before (further) execution", Tags: tags},
			func(ctx context.Context, in *struct {
				SpacePath
				ApprovalID string `path:"approval_id" maxLength:"64"`
				Body       struct {
					Reason string `json:"reason,omitempty" maxLength:"500"`
				}
			}) (*struct{ Body *approvals.Approval }, error) {
				a, err := svc.Revoke(ctx, Actor(ctx), in.SpaceID, in.ApprovalID, in.Body.Reason)
				if err != nil {
					return nil, d.fail(ctx, "approvals.revoke", err)
				}
				return &struct{ Body *approvals.Approval }{Body: a}, nil
			})
	}
}
