package api

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/auth"
)

type userOut struct {
	ETag string `header:"ETag"`
	Body *auth.User
}

func userResult(u *auth.User) *userOut { return &userOut{ETag: ETag(u.Version), Body: u} }

type UserPath struct {
	ID string `path:"user_id" pattern:"^usr_[0-9a-z]{26}$"`
}

func registerUsers(api huma.API, d *Deps) {
	tags := []string{"users"}
	huma.Register(api, huma.Operation{OperationID: "list-users", Method: http.MethodGet, Path: "/users", Summary: "List users", Tags: tags},
		func(ctx context.Context, _ *struct{}) (*struct {
			Body struct {
				Items []*auth.User `json:"items"`
			}
		}, error) {
			us, err := d.Auth.List(ctx)
			if err != nil {
				return nil, d.fail(ctx, "users.list", err)
			}
			out := &struct {
				Body struct {
					Items []*auth.User `json:"items"`
				}
			}{}
			out.Body.Items = append([]*auth.User{}, us...)
			return out, nil
		})

	huma.Register(api, huma.Operation{OperationID: "get-user", Method: http.MethodGet, Path: "/users/{user_id}", Summary: "Get a user", Tags: tags},
		func(ctx context.Context, in *UserPath) (*userOut, error) {
			u, err := d.Auth.Get(ctx, in.ID)
			if err != nil {
				return nil, d.fail(ctx, "users.get", err)
			}
			return userResult(u), nil
		})

	huma.Register(api, huma.Operation{OperationID: "create-user", Method: http.MethodPost, Path: "/users", Summary: "Create a user",
		Description: "Global administrators (and the root token, for the first administrator) create users with explicit grants. " +
			"Without a password, issue a reset token afterwards and hand it over out of band.",
		Tags: tags, DefaultStatus: http.StatusCreated},
		func(ctx context.Context, in *struct {
			IdempotencyKey
			Body auth.CreateInput
		}) (*struct {
			Replayed bool   `header:"Idempotent-Replayed"`
			ETag     string `header:"ETag"`
			Body     *auth.User
		}, error) {
			u, replayed, err := Idempotent(ctx, d, "create-user", in.IdempotencyKey, in.Body, false, func() (*auth.User, error) {
				return d.Auth.Create(ctx, Actor(ctx), in.Body)
			})
			if err != nil {
				return nil, d.fail(ctx, "users.create", err)
			}
			return &struct {
				Replayed bool   `header:"Idempotent-Replayed"`
				ETag     string `header:"ETag"`
				Body     *auth.User
			}{Replayed: replayed, ETag: ETag(u.Version), Body: u}, nil
		})

	huma.Register(api, huma.Operation{OperationID: "set-user-grants", Method: http.MethodPut, Path: "/users/{user_id}/grants", Summary: "Replace a user's manual grants",
		Tags: tags},
		func(ctx context.Context, in *struct {
			UserPath
			IfMatch
			Body struct {
				Grants []auth.GrantInput `json:"grants"`
			}
		}) (*userOut, error) {
			if err := d.checkUserVersion(ctx, in.ID, in.IfMatch); err != nil {
				return nil, err
			}
			u, err := d.Auth.SetGrants(ctx, Actor(ctx), in.ID, in.Body.Grants)
			if err != nil {
				return nil, d.fail(ctx, "users.set_grants", err)
			}
			return userResult(u), nil
		})

	huma.Register(api, huma.Operation{OperationID: "update-user", Method: http.MethodPut, Path: "/users/{user_id}/profile",
		Summary: "Change a user's email, display name and MFA requirement",
		Description: "Replaces all three. Email is never an SSO key, so a change never links, unlinks or merges an identity; an address in use is " +
			"refused, a user with a password keeps one, and changing it ends that user's sessions. Clearing mfa_required never lifts MFA that " +
			"policy forces (require_totp, operator or admin grants on a password account).", Tags: tags},
		func(ctx context.Context, in *struct {
			UserPath
			IfMatch
			Body auth.ProfileInput
		}) (*userOut, error) {
			v, err := in.Version()
			if err != nil {
				return nil, d.fail(ctx, "users.update", err)
			}
			u, err := d.Auth.UpdateProfile(ctx, Actor(ctx), in.ID, v, in.Body)
			if err != nil {
				return nil, d.fail(ctx, "users.update", err)
			}
			return userResult(u), nil
		})

	for _, enable := range []bool{true, false} {
		op, path, summary := "enable-user", "/users/{user_id}/enable", "Enable a user"
		if !enable {
			op, path, summary = "disable-user", "/users/{user_id}/disable", "Disable a user (revokes sessions; tokens stop working)"
		}
		huma.Register(api, huma.Operation{OperationID: op, Method: http.MethodPost, Path: path, Summary: summary, Tags: tags},
			func(ctx context.Context, in *struct {
				UserPath
				IfMatch
			}) (*userOut, error) {
				if err := d.checkUserVersion(ctx, in.ID, in.IfMatch); err != nil {
					return nil, err
				}
				u, err := d.Auth.SetEnabled(ctx, Actor(ctx), in.ID, enable)
				if err != nil {
					return nil, d.fail(ctx, "users."+op, err)
				}
				return userResult(u), nil
			})
	}

	huma.Register(api, huma.Operation{OperationID: "issue-reset", Method: http.MethodPost, Path: "/users/{user_id}/reset-token", Summary: "Issue a one-time password reset token",
		Tags: tags},
		func(ctx context.Context, in *UserPath) (*struct {
			Body struct {
				Token     string    `json:"token" doc:"Shown once; hand it to the user out of band"`
				ExpiresAt time.Time `json:"expires_at"`
			}
		}, error) {
			tok, exp, err := d.Auth.IssueResetToken(ctx, Actor(ctx), in.ID)
			if err != nil {
				return nil, d.fail(ctx, "users.issue_reset", err)
			}
			out := &struct {
				Body struct {
					Token     string    `json:"token" doc:"Shown once; hand it to the user out of band"`
					ExpiresAt time.Time `json:"expires_at"`
				}
			}{}
			out.Body.Token, out.Body.ExpiresAt = tok, exp
			return out, nil
		})
}

// checkUserVersion enforces If-Match against the user's current version.
func (d *Deps) checkUserVersion(ctx context.Context, id string, m IfMatch) error {
	want, err := m.Version()
	if err != nil {
		return d.fail(ctx, "users.version", err)
	}
	u, err := d.Auth.Get(ctx, id)
	if err != nil {
		return d.fail(ctx, "users.version", err)
	}
	if u.Version != want {
		return d.fail(ctx, "users.version", apperr.New(apperr.CodeConflict, "the user changed (version %d, you sent %d); reload and retry", u.Version, want))
	}
	return nil
}
