package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/xspace"
)

// XSpaceRules are the authorization rules of the cross-space operations (ADR-0038). Each space manages only its own
// side of a pair: what it accepts from a peer and what its control may send to it; a request needs both sides.
var XSpaceRules = map[string]Rule{
	"get-xspace-settings":      InSpace(authz.XSpaceRead),
	"update-xspace-settings":   InSpace(authz.XSpaceWrite),
	"list-xspace-peers":        InSpace(authz.XSpaceRead),
	"get-xspace-peer":          InSpace(authz.XSpaceRead),
	"put-xspace-peer":          InSpace(authz.XSpaceWrite),
	"delete-xspace-peer":       InSpace(authz.XSpaceWrite),
	"list-xspace-requests":     InSpace(authz.XSpaceRead),
	"get-xspace-request":       InSpace(authz.XSpaceRead),
	"cancel-xspace-request":    InSpace(authz.XSpaceWrite),
	"decline-xspace-request":   InSpace(authz.XSpaceWrite),
	"list-all-xspace-requests": Global(authz.XSpaceRead),
	"list-all-xspace-peers":    Global(authz.XSpaceRead),
}

// XSpacePeerPath identifies a space's configuration for a peer.
type XSpacePeerPath struct {
	SpacePath
	PeerID string `path:"peer_id" maxLength:"64" doc:"The peer space's ID"`
}

// XSpaceRequestPath identifies a cross-space request of a space.
type XSpaceRequestPath struct {
	SpacePath
	RequestID string `path:"request_id" maxLength:"64"`
}

type xspaceSettingsOut struct {
	ETag string `header:"ETag"`
	Body *xspace.Settings
}

type xspacePeerOut struct {
	ETag string `header:"ETag"`
	Body *xspace.Peer
}

type xspaceRequestList struct {
	Body struct {
		Items []*xspace.Request `json:"items"`
	}
}

type xspacePeerList struct {
	Body struct {
		Items []*xspace.Peer `json:"items"`
	}
}

// RegisterXSpace registers the cross-space operations.
func RegisterXSpace(svc *xspace.Service) func(api huma.API, d *Deps) {
	return func(api huma.API, d *Deps) {
		tags := []string{"cross-space"}
		huma.Register(api, huma.Operation{OperationID: "get-xspace-settings", Method: http.MethodGet, Path: "/spaces/{space_id}/xspace/settings",
			Summary: "A space's cross-space settings", Description: "Its public profile for peers, the deepest request chain it accepts and its outgoing limit.", Tags: tags},
			func(ctx context.Context, in *SpacePath) (*xspaceSettingsOut, error) {
				s, err := svc.GetSettings(ctx, in.SpaceID)
				if err != nil {
					return nil, d.fail(ctx, "xspace.settings", err)
				}
				return &xspaceSettingsOut{ETag: ETag(s.Version), Body: s}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "update-xspace-settings", Method: http.MethodPut, Path: "/spaces/{space_id}/xspace/settings",
			Summary: "Edit a space's cross-space settings", Description: "Without If-Match the settings must never have been saved.", Tags: tags},
			func(ctx context.Context, in *struct {
				SpacePath
				IfMatch
				Body xspace.SettingsInput
			}) (*xspaceSettingsOut, error) {
				v, err := optionalVersion(in.IfMatch)
				if err != nil {
					return nil, d.fail(ctx, "xspace.settings", err)
				}
				s, err := svc.SaveSettings(ctx, Actor(ctx), in.SpaceID, v, in.Body)
				if err != nil {
					return nil, d.fail(ctx, "xspace.settings", err)
				}
				return &xspaceSettingsOut{ETag: ETag(s.Version), Body: s}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "list-xspace-peers", Method: http.MethodGet, Path: "/spaces/{space_id}/xspace/peers",
			Summary: "What a space accepts from and may send to each peer space", Tags: tags},
			func(ctx context.Context, in *SpacePath) (*xspacePeerList, error) {
				l, err := svc.ListPeers(ctx, in.SpaceID)
				if err != nil {
					return nil, d.fail(ctx, "xspace.peers", err)
				}
				out := &xspacePeerList{}
				out.Body.Items = l
				return out, nil
			})
		huma.Register(api, huma.Operation{OperationID: "get-xspace-peer", Method: http.MethodGet, Path: "/spaces/{space_id}/xspace/peers/{peer_id}",
			Summary: "What a space accepts from and may send to one peer space", Tags: tags},
			func(ctx context.Context, in *XSpacePeerPath) (*xspacePeerOut, error) {
				l, err := svc.ListPeers(ctx, in.SpaceID)
				if err != nil {
					return nil, d.fail(ctx, "xspace.peer_get", err)
				}
				for _, p := range l {
					if p.Peer.ID == in.PeerID {
						return &xspacePeerOut{ETag: ETag(p.Version), Body: p}, nil
					}
				}
				return nil, d.fail(ctx, "xspace.peer_get", apperr.New(apperr.CodeNotFound, "no configuration for that peer space"))
			})
		huma.Register(api, huma.Operation{OperationID: "put-xspace-peer", Method: http.MethodPut, Path: "/spaces/{space_id}/xspace/peers/{peer_id}",
			Summary: "Set what a space accepts from and may send to a peer space",
			Description: "Without If-Match a new configuration is created. Every flag is separate and off unless set; a request needs the destination's " +
				"inbound flag and the source's outbound flag.", Tags: tags},
			func(ctx context.Context, in *struct {
				XSpacePeerPath
				IfMatch
				Body xspace.PeerInput
			}) (*xspacePeerOut, error) {
				v, err := optionalVersion(in.IfMatch)
				if err != nil {
					return nil, d.fail(ctx, "xspace.peer", err)
				}
				p, err := svc.SavePeer(ctx, Actor(ctx), in.SpaceID, in.PeerID, v, in.Body)
				if err != nil {
					return nil, d.fail(ctx, "xspace.peer", err)
				}
				return &xspacePeerOut{ETag: ETag(p.Version), Body: p}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "delete-xspace-peer", Method: http.MethodDelete, Path: "/spaces/{space_id}/xspace/peers/{peer_id}",
			Summary: "Remove a space's configuration for a peer space", Description: "Nothing is allowed with that peer any more in either direction from this side.",
			Tags: tags, DefaultStatus: http.StatusNoContent},
			func(ctx context.Context, in *XSpacePeerPath) (*struct{}, error) {
				if err := svc.DeletePeer(ctx, Actor(ctx), in.SpaceID, in.PeerID); err != nil {
					return nil, d.fail(ctx, "xspace.peer_delete", err)
				}
				return &struct{}{}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "list-xspace-requests", Method: http.MethodGet, Path: "/spaces/{space_id}/xspace/requests",
			Summary: "A space's outgoing and incoming cross-space requests", Tags: tags},
			func(ctx context.Context, in *struct {
				SpacePath
				Direction string `query:"direction" enum:"outgoing,incoming," doc:"Empty lists both"`
				Open      bool   `query:"open" doc:"Only pending and accepted requests"`
				Limit     int    `query:"limit" minimum:"0" maximum:"500"`
			}) (*xspaceRequestList, error) {
				l, err := svc.List(ctx, in.SpaceID, in.Direction, in.Open, in.Limit)
				if err != nil {
					return nil, d.fail(ctx, "xspace.requests", err)
				}
				out := &xspaceRequestList{}
				out.Body.Items = l
				return out, nil
			})
		huma.Register(api, huma.Operation{OperationID: "get-xspace-request", Method: http.MethodGet, Path: "/spaces/{space_id}/xspace/requests/{request_id}",
			Summary: "A cross-space request as this space sees it", Tags: tags},
			func(ctx context.Context, in *XSpaceRequestPath) (*struct{ Body *xspace.Request }, error) {
				r, err := svc.Get(ctx, in.SpaceID, in.RequestID)
				if err != nil {
					return nil, d.fail(ctx, "xspace.request", err)
				}
				return &struct{ Body *xspace.Request }{Body: r}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "cancel-xspace-request", Method: http.MethodPost, Path: "/spaces/{space_id}/xspace/requests/{request_id}/cancel",
			Summary: "Withdraw an open request this space sent", Description: "The destination's control is told; its local work stays its decision.", Tags: tags},
			func(ctx context.Context, in *struct {
				XSpaceRequestPath
				Body struct {
					Reason string `json:"reason,omitempty" maxLength:"500"`
				}
			}) (*struct{ Body *xspace.Request }, error) {
				r, err := svc.Cancel(ctx, Actor(ctx), in.SpaceID, in.RequestID, in.Body.Reason)
				if err != nil {
					return nil, d.fail(ctx, "xspace.cancel", err)
				}
				return &struct{ Body *xspace.Request }{Body: r}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "decline-xspace-request", Method: http.MethodPost, Path: "/spaces/{space_id}/xspace/requests/{request_id}/decline",
			Summary: "Decline a pending request another space sent this one", Description: "The reason reaches the requester only when this space's disclosure level for it allows text.",
			Tags: tags},
			func(ctx context.Context, in *struct {
				XSpaceRequestPath
				Body struct {
					Reason string `json:"reason,omitempty" maxLength:"500"`
				}
			}) (*struct{ Body *xspace.Request }, error) {
				r, err := svc.Decline(ctx, Actor(ctx), in.SpaceID, in.RequestID, in.Body.Reason)
				if err != nil {
					return nil, d.fail(ctx, "xspace.decline", err)
				}
				return &struct{ Body *xspace.Request }{Body: r}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "list-all-xspace-requests", Method: http.MethodGet, Path: "/xspace/requests",
			Summary: "Every cross-space request (global administration)", Tags: tags},
			func(ctx context.Context, in *struct {
				Open  bool `query:"open"`
				Limit int  `query:"limit" minimum:"0" maximum:"500"`
			}) (*xspaceRequestList, error) {
				l, err := svc.ListAll(ctx, in.Open, in.Limit)
				if err != nil {
					return nil, d.fail(ctx, "xspace.requests_all", err)
				}
				out := &xspaceRequestList{}
				out.Body.Items = l
				return out, nil
			})
		huma.Register(api, huma.Operation{OperationID: "list-all-xspace-peers", Method: http.MethodGet, Path: "/xspace/peers",
			Summary: "Every space's peer configuration (global administration)", Tags: tags},
			func(ctx context.Context, _ *struct{}) (*xspacePeerList, error) {
				l, err := svc.ListAllPeers(ctx)
				if err != nil {
					return nil, d.fail(ctx, "xspace.peers_all", err)
				}
				out := &xspacePeerList{}
				out.Body.Items = l
				return out, nil
			})
	}
}
