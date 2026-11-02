package api

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/auth"
)

type tokenList struct {
	Body struct {
		Items []*auth.APIToken `json:"items"`
	}
}

func listOf(ts []*auth.APIToken) *tokenList {
	out := &tokenList{}
	out.Body.Items = append([]*auth.APIToken{}, ts...)
	return out
}

type TokenPath struct {
	ID string `path:"token_id" pattern:"^atk_[0-9a-z]{26}$"`
}

// CreatedToken returns the secret exactly once.
type CreatedToken struct {
	Token  *auth.APIToken `json:"token"`
	Secret string         `json:"secret" doc:"The bearer token; shown once and never retrievable again"`
}

// CreatedRootToken returns the new root token exactly once.
type CreatedRootToken struct {
	RootToken *auth.RootToken `json:"root_token"`
	Secret    string          `json:"secret" doc:"The root token; shown once. Store it offline."`
}

func registerTokens(api huma.API, d *Deps) {
	tags := []string{"tokens"}
	huma.Register(api, huma.Operation{OperationID: "list-my-tokens", Method: http.MethodGet, Path: "/tokens", Summary: "Your API tokens", Tags: tags},
		func(ctx context.Context, _ *struct{}) (*tokenList, error) {
			ts, err := d.Auth.ListUserAPITokens(ctx, PrincipalFrom(ctx).User.ID)
			if err != nil {
				return nil, d.fail(ctx, "tokens.list_mine", err)
			}
			return listOf(ts), nil
		})

	huma.Register(api, huma.Operation{OperationID: "create-api-token", Method: http.MethodPost, Path: "/tokens", Summary: "Create an API token",
		Description: "The token acts for you with the given grants, each of which must be within your own grants. Requests are " +
			"authorized as both the token and you, so the token loses anything you lose.",
		Tags: tags, DefaultStatus: http.StatusCreated},
		func(ctx context.Context, in *struct {
			IdempotencyKey
			Body struct {
				Name          string            `json:"name" minLength:"1" maxLength:"100"`
				Grants        []auth.GrantInput `json:"grants" minItems:"1" maxItems:"64"`
				ExpiresInDays int               `json:"expires_in_days,omitempty" minimum:"0" maximum:"366"`
				NeverExpires  bool              `json:"never_expires,omitempty"`
			}
		}) (*struct{ Body CreatedToken }, error) {
			res, _, err := Idempotent(ctx, d, "create-api-token", in.IdempotencyKey, in.Body, true, func() (*CreatedToken, error) {
				t, secret, err := d.Auth.CreateAPIToken(ctx, PrincipalFrom(ctx), in.Body.Name, in.Body.Grants,
					time.Duration(in.Body.ExpiresInDays)*24*time.Hour, in.Body.NeverExpires)
				if err != nil {
					return nil, err
				}
				return &CreatedToken{Token: t, Secret: secret}, nil
			})
			if err != nil {
				return nil, d.fail(ctx, "tokens.create", err)
			}
			return &struct{ Body CreatedToken }{Body: *res}, nil
		})

	huma.Register(api, huma.Operation{OperationID: "revoke-my-token", Method: http.MethodDelete, Path: "/tokens/{token_id}", Summary: "Revoke one of your API tokens",
		Tags: tags},
		func(ctx context.Context, in *TokenPath) (*struct{ Body *auth.APIToken }, error) {
			t, err := d.Auth.GetAPIToken(ctx, in.ID)
			if err != nil || t.UserID != PrincipalFrom(ctx).User.ID {
				return nil, d.fail(ctx, "tokens.revoke_mine", apperr.New(apperr.CodeNotFound, "API token not found"))
			}
			if t, err = d.Auth.RevokeAPIToken(ctx, Actor(ctx), in.ID); err != nil {
				return nil, d.fail(ctx, "tokens.revoke_mine", err)
			}
			return &struct{ Body *auth.APIToken }{Body: t}, nil
		})

	huma.Register(api, huma.Operation{OperationID: "list-all-tokens", Method: http.MethodGet, Path: "/admin/tokens", Summary: "Every API token", Tags: tags},
		func(ctx context.Context, _ *struct{}) (*tokenList, error) {
			ts, err := d.Auth.ListAPITokens(ctx)
			if err != nil {
				return nil, d.fail(ctx, "tokens.list_all", err)
			}
			return listOf(ts), nil
		})

	huma.Register(api, huma.Operation{OperationID: "revoke-any-token", Method: http.MethodDelete, Path: "/admin/tokens/{token_id}", Summary: "Revoke any API token",
		Tags: tags},
		func(ctx context.Context, in *TokenPath) (*struct{ Body *auth.APIToken }, error) {
			t, err := d.Auth.RevokeAPIToken(ctx, Actor(ctx), in.ID)
			if err != nil {
				return nil, d.fail(ctx, "tokens.revoke_any", err)
			}
			return &struct{ Body *auth.APIToken }{Body: t}, nil
		})

	huma.Register(api, huma.Operation{OperationID: "get-root-token", Method: http.MethodGet, Path: "/admin/root-token", Summary: "Active root token metadata",
		Tags: tags},
		func(ctx context.Context, _ *struct{}) (*struct{ Body *auth.RootToken }, error) {
			t, err := d.Auth.GetRootToken(ctx)
			if err != nil {
				return nil, d.fail(ctx, "root_token.get", err)
			}
			return &struct{ Body *auth.RootToken }{Body: t}, nil
		})

	huma.Register(api, huma.Operation{OperationID: "rotate-root-token", Method: http.MethodPost, Path: "/admin/root-token/rotate", Summary: "Rotate the root token",
		Tags: tags},
		func(ctx context.Context, _ *struct{}) (*struct{ Body CreatedRootToken }, error) {
			secret, rt, err := d.Auth.RotateRootToken(ctx, Actor(ctx))
			if err != nil {
				return nil, d.fail(ctx, "root_token.rotate", err)
			}
			return &struct{ Body CreatedRootToken }{Body: CreatedRootToken{RootToken: rt, Secret: secret}}, nil
		})

	huma.Register(api, huma.Operation{OperationID: "revoke-root-token", Method: http.MethodDelete, Path: "/admin/root-token", Summary: "Revoke the root token",
		Tags: tags, DefaultStatus: http.StatusNoContent},
		func(ctx context.Context, _ *struct{}) (*struct{}, error) {
			if err := d.Auth.RevokeRootToken(ctx, Actor(ctx)); err != nil {
				return nil, d.fail(ctx, "root_token.revoke", err)
			}
			return &struct{}{}, nil
		})
}
