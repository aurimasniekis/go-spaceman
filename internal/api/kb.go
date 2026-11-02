package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/kb"
)

// KBRules are the authorization rules of the knowledge-base operations. A KB's validator runs on the space's node with
// the node user's rights, as agent programs do, so configuring it needs kb:write in the space.
var KBRules = map[string]Rule{
	"list-kbs":           InSpace(authz.KBRead),
	"get-kb":             InSpace(authz.KBRead),
	"create-kb":          InSpace(authz.KBWrite),
	"update-kb":          InSpace(authz.KBWrite),
	"accept-kb-proposal": InSpace(authz.KBWrite),
	"reject-kb-proposal": InSpace(authz.KBWrite),
	"cancel-kb-request":  InSpace(authz.KBWrite),
}

// KBPath identifies a KB.
type KBPath struct {
	SpacePath
	KBID string `path:"kb_id" maxLength:"64"`
}

type kbOut struct {
	ETag string `header:"ETag"`
	Body *kb.KB
}

// RegisterKB registers the knowledge-base operations.
func RegisterKB(svc *kb.Service) func(api huma.API, d *Deps) {
	return func(api huma.API, d *Deps) {
		tags := []string{"kb"}
		huma.Register(api, huma.Operation{OperationID: "list-kbs", Method: http.MethodGet, Path: "/spaces/{space_id}/kbs", Summary: "A space's knowledge bases with capacity and usage", Tags: tags},
			func(ctx context.Context, in *SpacePath) (*struct {
				Body struct {
					Items []*kb.KB `json:"items"`
				}
			}, error) {
				l, err := svc.List(ctx, in.SpaceID)
				if err != nil {
					return nil, d.fail(ctx, "kb.list", err)
				}
				out := &struct {
					Body struct {
						Items []*kb.KB `json:"items"`
					}
				}{}
				out.Body.Items = l
				return out, nil
			})
		huma.Register(api, huma.Operation{OperationID: "get-kb", Method: http.MethodGet, Path: "/spaces/{space_id}/kbs/{kb_id}",
			Summary: "A knowledge base with its sessions, requests and proposals", Tags: tags},
			func(ctx context.Context, in *KBPath) (*struct{ Body *kb.Detail }, error) {
				dt, err := svc.Detail(ctx, in.SpaceID, in.KBID)
				if err != nil {
					return nil, d.fail(ctx, "kb.get", err)
				}
				return &struct{ Body *kb.Detail }{Body: dt}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "create-kb", Method: http.MethodPost, Path: "/spaces/{space_id}/kbs", Summary: "Add a knowledge base",
			Tags: tags, DefaultStatus: http.StatusCreated},
			func(ctx context.Context, in *struct {
				SpacePath
				Body kb.Input
			}) (*kbOut, error) {
				k, err := svc.Create(ctx, Actor(ctx), in.SpaceID, in.Body)
				if err != nil {
					return nil, d.fail(ctx, "kb.create", err)
				}
				return &kbOut{ETag: ETag(k.Version), Body: k}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "update-kb", Method: http.MethodPut, Path: "/spaces/{space_id}/kbs/{kb_id}", Summary: "Edit a knowledge base", Tags: tags},
			func(ctx context.Context, in *struct {
				KBPath
				IfMatch
				Body kb.Input
			}) (*kbOut, error) {
				v, err := in.Version()
				if err != nil {
					return nil, d.fail(ctx, "kb.update", err)
				}
				k, err := svc.Update(ctx, Actor(ctx), in.SpaceID, in.KBID, v, in.Body)
				if err != nil {
					return nil, d.fail(ctx, "kb.update", err)
				}
				return &kbOut{ETag: ETag(k.Version), Body: k}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "accept-kb-proposal", Method: http.MethodPost, Path: "/spaces/{space_id}/kb-proposals/{proposal_id}/accept",
			Summary: "Accept a proposal", Description: "The node merges it into the KB's base branch by fast-forward or a clean merge after the KB's check passes; conflicts are reported, never forced.",
			Tags: tags},
			func(ctx context.Context, in *struct {
				SpacePath
				ProposalID string `path:"proposal_id" maxLength:"64"`
			}) (*struct{ Body *kb.Proposal }, error) {
				p, err := svc.Accept(ctx, Actor(ctx), in.SpaceID, in.ProposalID)
				if err != nil {
					return nil, d.fail(ctx, "kb.accept", err)
				}
				return &struct{ Body *kb.Proposal }{Body: p}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "reject-kb-proposal", Method: http.MethodPost, Path: "/spaces/{space_id}/kb-proposals/{proposal_id}/reject",
			Summary: "Reject a proposal", Tags: tags},
			func(ctx context.Context, in *struct {
				SpacePath
				ProposalID string `path:"proposal_id" maxLength:"64"`
				Body       struct {
					Reason string `json:"reason,omitempty" maxLength:"2000"`
				}
			}) (*struct{ Body *kb.Proposal }, error) {
				p, err := svc.Reject(ctx, Actor(ctx), in.SpaceID, in.ProposalID, in.Body.Reason)
				if err != nil {
					return nil, d.fail(ctx, "kb.reject", err)
				}
				return &struct{ Body *kb.Proposal }{Body: p}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "cancel-kb-request", Method: http.MethodPost, Path: "/spaces/{space_id}/kb-requests/{request_id}/cancel",
			Summary: "Cancel a pending request", Tags: tags, DefaultStatus: http.StatusNoContent},
			func(ctx context.Context, in *struct {
				SpacePath
				RequestID string `path:"request_id" maxLength:"64"`
			}) (*struct{}, error) {
				if err := svc.Cancel(ctx, Actor(ctx), nil, in.SpaceID, in.RequestID); err != nil {
					return nil, d.fail(ctx, "kb.cancel", err)
				}
				return &struct{}{}, nil
			})
	}
}
