package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
)

// FleetRules are the authorization rules of the node, space and run operations.
var FleetRules = map[string]Rule{
	"list-nodes":         Global(authz.NodeRead),
	"get-node":           Global(authz.NodeRead),
	"create-node":        StepUp(authz.NodeEnroll),
	"reissue-enrollment": StepUp(authz.NodeEnroll),
	"set-node-state":     StepUp(authz.NodeWrite),

	"list-spaces":     {Session: true},
	"create-space":    Global(authz.SpaceCreate),
	"get-space":       InSpace(authz.SpaceRead),
	"update-space":    InSpace(authz.SpaceWrite),
	"set-space-state": InSpace(authz.SpaceWrite),
	"purge-space":     {Perm: authz.SpacePurge, Reauth: true, MFA: true},

	"list-runs":  InSpace(authz.RunRead),
	"get-run":    InSpace(authz.RunRead),
	"launch-run": InSpace(authz.RunControl),
	"stop-run":   InSpace(authz.RunControl),
	"send-input": InSpace(authz.RunControl),
}

type spaceOut struct {
	ETag string `header:"ETag"`
	Body *fleet.Space
}

func spaceResult(s *fleet.Space) *spaceOut { return &spaceOut{ETag: ETag(s.Version), Body: s} }

// SpacePath is the {space_id} parameter (exported: huma sets embedded parameter structs only when exported).
type SpacePath struct {
	SpaceID string `path:"space_id" pattern:"^spc_[0-9a-z]{26}$"`
}

// NodePath is the {node_id} parameter.
type NodePath struct {
	NodeID string `path:"node_id" pattern:"^nod_[0-9a-z]{26}$"`
}

// RunPath is the {run_id} parameter within a space.
type RunPath struct {
	SpaceID string `path:"space_id" pattern:"^spc_[0-9a-z]{26}$"`
	RunID   string `path:"run_id" pattern:"^run_[0-9a-z]{26}$"`
}

// RegisterFleet registers node, space and run operations.
func RegisterFleet(f *fleet.Service) func(api huma.API, d *Deps) {
	return func(api huma.API, d *Deps) {
		tags := []string{"nodes"}
		huma.Register(api, huma.Operation{OperationID: "list-nodes", Method: http.MethodGet, Path: "/nodes", Summary: "List nodes", Tags: tags},
			func(ctx context.Context, _ *struct{}) (*struct {
				Body struct {
					Items []*fleet.Node `json:"items"`
				}
			}, error) {
				ns, err := f.ListNodes(ctx)
				if err != nil {
					return nil, d.fail(ctx, "nodes.list", err)
				}
				out := &struct {
					Body struct {
						Items []*fleet.Node `json:"items"`
					}
				}{}
				out.Body.Items = append([]*fleet.Node{}, ns...)
				return out, nil
			})
		huma.Register(api, huma.Operation{OperationID: "get-node", Method: http.MethodGet, Path: "/nodes/{node_id}", Summary: "Get a node", Tags: tags},
			func(ctx context.Context, in *NodePath) (*struct{ Body *fleet.Node }, error) {
				n, err := f.GetNode(ctx, in.NodeID)
				if err != nil {
					return nil, d.fail(ctx, "nodes.get", err)
				}
				return &struct{ Body *fleet.Node }{Body: n}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "create-node", Method: http.MethodPost, Path: "/nodes", Summary: "Register a node and issue its enrollment token",
			Tags: tags, DefaultStatus: http.StatusCreated},
			func(ctx context.Context, in *struct {
				IdempotencyKey
				Body fleet.CreateNodeInput
			}) (*struct{ Body *fleet.NodeEnrollment }, error) {
				e, _, err := Idempotent(ctx, d, "create-node", in.IdempotencyKey, in.Body, true, func() (*fleet.NodeEnrollment, error) {
					return f.CreateNode(ctx, Actor(ctx), in.Body)
				})
				if err != nil {
					return nil, d.fail(ctx, "nodes.create", err)
				}
				return &struct{ Body *fleet.NodeEnrollment }{Body: e}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "reissue-enrollment", Method: http.MethodPost, Path: "/nodes/{node_id}/enrollment",
			Summary: "Issue a new enrollment token (re-enrollment replaces the node's credentials)", Tags: tags},
			func(ctx context.Context, in *NodePath) (*struct{ Body *fleet.NodeEnrollment }, error) {
				e, err := f.ReissueEnrollment(ctx, Actor(ctx), in.NodeID)
				if err != nil {
					return nil, d.fail(ctx, "nodes.reissue", err)
				}
				return &struct{ Body *fleet.NodeEnrollment }{Body: e}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "set-node-state", Method: http.MethodPost, Path: "/nodes/{node_id}/state",
			Summary: "Enable, disable or revoke a node (disable and revoke close its connection)", Tags: tags},
			func(ctx context.Context, in *struct {
				NodePath
				Body struct {
					State string `json:"state" enum:"active,disabled,revoked"`
				}
			}) (*struct{ Body *fleet.Node }, error) {
				n, err := f.SetNodeState(ctx, Actor(ctx), in.NodeID, in.Body.State)
				if err != nil {
					return nil, d.fail(ctx, "nodes.set_state", err)
				}
				return &struct{ Body *fleet.Node }{Body: n}, nil
			})

		tags = []string{"spaces"}
		huma.Register(api, huma.Operation{OperationID: "list-spaces", Method: http.MethodGet, Path: "/spaces", Summary: "Spaces you can see", Tags: tags},
			func(ctx context.Context, _ *struct{}) (*struct {
				Body struct {
					Items []*fleet.Space `json:"items"`
				}
			}, error) {
				p := PrincipalFrom(ctx)
				ss, err := f.ListSpaces(ctx, func(id string) bool { return d.Authz.AllowedAll(p.Subjects(), id, authz.SpaceRead) })
				if err != nil {
					return nil, d.fail(ctx, "spaces.list", err)
				}
				out := &struct {
					Body struct {
						Items []*fleet.Space `json:"items"`
					}
				}{}
				out.Body.Items = ss
				return out, nil
			})
		huma.Register(api, huma.Operation{OperationID: "create-space", Method: http.MethodPost, Path: "/spaces", Summary: "Create a space pinned to a node",
			Tags: tags, DefaultStatus: http.StatusCreated},
			func(ctx context.Context, in *struct {
				IdempotencyKey
				Body fleet.CreateSpaceInput
			}) (*spaceOut, error) {
				s, _, err := Idempotent(ctx, d, "create-space", in.IdempotencyKey, in.Body, false, func() (*fleet.Space, error) {
					return f.CreateSpace(ctx, Actor(ctx), in.Body)
				})
				if err != nil {
					return nil, d.fail(ctx, "spaces.create", err)
				}
				return spaceResult(s), nil
			})
		huma.Register(api, huma.Operation{OperationID: "get-space", Method: http.MethodGet, Path: "/spaces/{space_id}", Summary: "Get a space", Tags: tags},
			func(ctx context.Context, in *SpacePath) (*spaceOut, error) {
				s, err := f.GetSpace(ctx, in.SpaceID)
				if err != nil {
					return nil, d.fail(ctx, "spaces.get", err)
				}
				return spaceResult(s), nil
			})
		huma.Register(api, huma.Operation{OperationID: "update-space", Method: http.MethodPut, Path: "/spaces/{space_id}", Summary: "Edit a space's profile",
			Tags: tags},
			func(ctx context.Context, in *struct {
				SpacePath
				IfMatch
				Body fleet.UpdateSpaceInput
			}) (*spaceOut, error) {
				v, err := in.Version()
				if err != nil {
					return nil, d.fail(ctx, "spaces.update", err)
				}
				s, err := f.UpdateSpace(ctx, Actor(ctx), in.SpaceID, v, in.Body)
				if err != nil {
					return nil, d.fail(ctx, "spaces.update", err)
				}
				return spaceResult(s), nil
			})
		huma.Register(api, huma.Operation{OperationID: "set-space-state", Method: http.MethodPost, Path: "/spaces/{space_id}/state",
			Summary:     "Pause (hold, interrupt, emergency), resume, archive or restore a space",
			Description: "hold admits no new work; interrupt also stops active runs politely; emergency kills them. Archiving needs no active runs.",
			Tags:        tags},
			func(ctx context.Context, in *struct {
				SpacePath
				IfMatch
				Body struct {
					State string `json:"state" enum:"active,paused,archived"`
					Mode  string `json:"mode,omitempty" enum:"hold,interrupt,emergency"`
				}
			}) (*spaceOut, error) {
				v, err := in.Version()
				if err != nil {
					return nil, d.fail(ctx, "spaces.state", err)
				}
				s, err := f.SetSpaceState(ctx, Actor(ctx), in.SpaceID, v, in.Body.State, in.Body.Mode)
				if err != nil {
					return nil, d.fail(ctx, "spaces.state", err)
				}
				return spaceResult(s), nil
			})
		huma.Register(api, huma.Operation{OperationID: "purge-space", Method: http.MethodDelete, Path: "/spaces/{space_id}",
			Summary: "Permanently delete an archived space's records (never upstream repositories)", Tags: tags, DefaultStatus: http.StatusNoContent},
			func(ctx context.Context, in *SpacePath) (*struct{}, error) {
				if err := f.PurgeSpace(ctx, Actor(ctx), in.SpaceID); err != nil {
					return nil, d.fail(ctx, "spaces.purge", err)
				}
				return &struct{}{}, nil
			})

		tags = []string{"runs"}
		huma.Register(api, huma.Operation{OperationID: "list-runs", Method: http.MethodGet, Path: "/spaces/{space_id}/runs", Summary: "A space's runs, newest first",
			Tags: tags},
			func(ctx context.Context, in *struct {
				SpacePath
				Limit int `query:"limit" minimum:"1" maximum:"500" default:"100"`
			}) (*struct {
				Body struct {
					Items []*fleet.Run `json:"items"`
				}
			}, error) {
				if in.Limit == 0 {
					in.Limit = 100
				}
				rs, err := f.ListRuns(ctx, in.SpaceID, in.Limit)
				if err != nil {
					return nil, d.fail(ctx, "runs.list", err)
				}
				out := &struct {
					Body struct {
						Items []*fleet.Run `json:"items"`
					}
				}{}
				out.Body.Items = rs
				return out, nil
			})
		huma.Register(api, huma.Operation{OperationID: "get-run", Method: http.MethodGet, Path: "/spaces/{space_id}/runs/{run_id}", Summary: "Get a run", Tags: tags},
			func(ctx context.Context, in *RunPath) (*struct{ Body *fleet.Run }, error) {
				r, err := d.spaceRun(ctx, f, in.SpaceID, in.RunID)
				if err != nil {
					return nil, err
				}
				return &struct{ Body *fleet.Run }{Body: r}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "launch-run", Method: http.MethodPost, Path: "/spaces/{space_id}/runs", Summary: "Launch a run on the space's node",
			Tags: tags, DefaultStatus: http.StatusCreated},
			func(ctx context.Context, in *struct {
				SpacePath
				IdempotencyKey
				Body fleet.LaunchInput
			}) (*struct{ Body *fleet.Run }, error) {
				r, _, err := Idempotent(ctx, d, "launch-run:"+in.SpaceID, in.IdempotencyKey, in.Body, false, func() (*fleet.Run, error) {
					return f.LaunchRun(ctx, Actor(ctx), in.SpaceID, in.Body)
				})
				if err != nil {
					return nil, d.fail(ctx, "runs.launch", err)
				}
				return &struct{ Body *fleet.Run }{Body: r}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "stop-run", Method: http.MethodPost, Path: "/spaces/{space_id}/runs/{run_id}/stop", Summary: "Stop a run",
			Tags: tags},
			func(ctx context.Context, in *struct {
				RunPath
				Body struct {
					Mode   string `json:"mode,omitempty" enum:"interrupt,kill"`
					Reason string `json:"reason,omitempty" maxLength:"500"`
				}
			}) (*struct{ Body *fleet.Run }, error) {
				if _, err := d.spaceRun(ctx, f, in.SpaceID, in.RunID); err != nil {
					return nil, err
				}
				r, err := f.StopRun(ctx, Actor(ctx), in.RunID, in.Body.Mode, in.Body.Reason)
				if err != nil {
					return nil, d.fail(ctx, "runs.stop", err)
				}
				return &struct{ Body *fleet.Run }{Body: r}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "send-input", Method: http.MethodPost, Path: "/spaces/{space_id}/runs/{run_id}/input",
			Summary: "Send safe input to a run (delivered only while the program reports idle, unless forced)", Tags: tags, DefaultStatus: http.StatusAccepted},
			func(ctx context.Context, in *struct {
				RunPath
				Body struct {
					Text  string `json:"text" minLength:"1" maxLength:"65536"`
					Force bool   `json:"force,omitempty"`
				}
			}) (*struct{}, error) {
				if _, err := d.spaceRun(ctx, f, in.SpaceID, in.RunID); err != nil {
					return nil, err
				}
				if err := f.SendInput(ctx, Actor(ctx), in.RunID, in.Body.Text, in.Body.Force); err != nil {
					return nil, d.fail(ctx, "runs.input", err)
				}
				return &struct{}{}, nil
			})
	}
}

// spaceRun returns a run only when it belongs to the space in the path (authorization is per space).
func (d *Deps) spaceRun(ctx context.Context, f *fleet.Service, spaceID, runID string) (*fleet.Run, error) {
	r, err := f.GetRun(ctx, runID)
	if err != nil || r.SpaceID != spaceID {
		return nil, newProblem(http.StatusNotFound, "not_found", "run not found", nil)
	}
	return r, nil
}
