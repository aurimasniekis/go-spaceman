package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
)

// TakeoverRules are the authorization rules of terminal takeover (SM-UI-004, ADR-0046): every operation needs
// terminal:takeover in the run's space (admins by default; operators watch and message but do not take over). Taking
// over also needs a recent sign-in for browser sessions. Typing and renewing are the holder's alone (checked by the
// service against the stored holder, after this rule).
var TakeoverRules = map[string]Rule{
	"start-takeover": {Perm: authz.TerminalTakeover, Space: true, Reauth: true},
	"renew-takeover": InSpace(authz.TerminalTakeover),
	"takeover-input": InSpace(authz.TerminalTakeover),
	"end-takeover":   InSpace(authz.TerminalTakeover),
}

// TakeoverPath identifies a takeover of a run.
type TakeoverPath struct {
	RunPath
	TakeoverID string `path:"takeover_id" pattern:"^tko_[0-9a-z]{26}$"`
}

func registerTakeover(api huma.API, d *Deps, f *fleet.Service) {
	tags := []string{"runs"}
	huma.Register(api, huma.Operation{OperationID: "start-takeover", Method: http.MethodPost, Path: "/spaces/{space_id}/runs/{run_id}/takeover",
		Summary: "Take over a run's terminal",
		Description: "You type into the terminal (takeover-input) and every automated input to it is refused until the takeover ends: " +
			"Message the agent, and the notices that tell the agent about messages and human responses (they are delivered afterwards). " +
			"Everyone watching sees who holds it. It lasts 30 s unless renewed, and ends on release, when the run ends, or when the node cannot hold it. " +
			"Audited (start and end, never the keystrokes).", Tags: tags, DefaultStatus: http.StatusCreated},
		func(ctx context.Context, in *RunPath) (*struct{ Body *fleet.Takeover }, error) {
			if _, err := d.spaceRun(ctx, f, in.SpaceID, in.RunID); err != nil {
				return nil, err
			}
			t, err := f.StartTakeover(ctx, Actor(ctx), actorLabel(ctx), in.RunID)
			if err != nil {
				return nil, d.fail(ctx, "runs.takeover_start", err)
			}
			return &struct{ Body *fleet.Takeover }{Body: t}, nil
		})
	huma.Register(api, huma.Operation{OperationID: "renew-takeover", Method: http.MethodPost, Path: "/spaces/{space_id}/runs/{run_id}/takeover/{takeover_id}/renew",
		Summary: "Keep your takeover for another 30 s", Description: "Only the holder renews. A takeover that is not renewed ends.", Tags: tags},
		func(ctx context.Context, in *TakeoverPath) (*struct{ Body *fleet.Takeover }, error) {
			if _, err := d.spaceRun(ctx, f, in.SpaceID, in.RunID); err != nil {
				return nil, err
			}
			t, err := f.RenewTakeover(ctx, Actor(ctx), in.RunID, in.TakeoverID)
			if err != nil {
				return nil, d.fail(ctx, "runs.takeover_renew", err)
			}
			return &struct{ Body *fleet.Takeover }{Body: t}, nil
		})
	huma.Register(api, huma.Operation{OperationID: "takeover-input", Method: http.MethodPost, Path: "/spaces/{space_id}/runs/{run_id}/takeover/{takeover_id}/input",
		Summary: "Type into a taken-over terminal",
		Description: "Only the holder types. The text goes to the program as keystrokes, escape sequences included (\\r is Enter); at most 4096 bytes " +
			"per request and 50 requests or 64 KiB per second. It is never stored or logged.", Tags: tags, DefaultStatus: http.StatusAccepted},
		func(ctx context.Context, in *struct {
			TakeoverPath
			Body struct {
				Data string `json:"data" minLength:"1" maxLength:"4096" doc:"Keystrokes as the terminal produces them"`
			}
		}) (*struct{}, error) {
			if _, err := d.spaceRun(ctx, f, in.SpaceID, in.RunID); err != nil {
				return nil, err
			}
			if err := f.TakeoverInput(ctx, Actor(ctx), in.RunID, in.TakeoverID, []byte(in.Body.Data)); err != nil {
				return nil, d.fail(ctx, "runs.takeover_input", err)
			}
			return &struct{}{}, nil
		})
	huma.Register(api, huma.Operation{OperationID: "end-takeover", Method: http.MethodPost, Path: "/spaces/{space_id}/runs/{run_id}/takeover/{takeover_id}/end",
		Summary:     "End a takeover",
		Description: "The holder releases it; anyone else with terminal:takeover in the space can end it too (recorded as ended). Automated input resumes.",
		Tags:        tags},
		func(ctx context.Context, in *TakeoverPath) (*struct{ Body *fleet.Takeover }, error) {
			if _, err := d.spaceRun(ctx, f, in.SpaceID, in.RunID); err != nil {
				return nil, err
			}
			t, err := f.EndTakeover(ctx, Actor(ctx), in.RunID, in.TakeoverID)
			if err != nil {
				return nil, d.fail(ctx, "runs.takeover_end", err)
			}
			return &struct{ Body *fleet.Takeover }{Body: t}, nil
		})
}
