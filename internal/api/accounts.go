package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/aurimasniekis/go-spaceman/internal/accounts"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
)

// AccountRules are the authorization rules of the account operations. Binding an account to a space hands its
// credential to that space's runs, so it needs account:write, not only space configuration rights.
var AccountRules = map[string]Rule{
	"list-account-kinds":  FilteredRead(),
	"list-accounts":       Global(authz.AccountRead),
	"get-account":         Global(authz.AccountRead),
	"create-account":      Global(authz.AccountWrite),
	"update-account":      Global(authz.AccountWrite),
	"delete-account":      Global(authz.AccountWrite),
	"list-space-accounts": InSpace(authz.SpaceConfigRead),
	"set-space-accounts":  Global(authz.AccountWrite),
}

// AccountPath identifies an account.
type AccountPath struct {
	AccountID string `path:"account_id" maxLength:"64"`
}

// AccountBody creates or edits an account. The secret is write-only.
type AccountBody struct {
	Name        string            `json:"name" maxLength:"63"`
	Kind        string            `json:"kind,omitempty" enum:"claude_oauth_token,anthropic_api_key,openai_api_key,codex_chatgpt_login" doc:"Required on create; never changes"`
	Description string            `json:"description,omitempty" maxLength:"2000"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	Secret      string            `json:"secret,omitempty" maxLength:"65536" writeOnly:"true" doc:"The credential: a token or API key (one line, at most 8192 bytes), or for codex_chatgpt_login the auth.json a ChatGPT login wrote (at most 64 KiB). Required on create; on an edit, a value rotates it and empty keeps it. Never returned."`
	Enabled     *bool             `json:"enabled,omitempty" doc:"Default true"`
}

func (b AccountBody) input() accounts.Input {
	en := b.Enabled == nil || *b.Enabled
	return accounts.Input{Name: b.Name, Kind: b.Kind, Description: b.Description, Metadata: b.Metadata, Secret: b.Secret, Enabled: en}
}

// BindingInput allows a space's runs to use an account.
type BindingInput struct {
	AccountID string `json:"account_id" maxLength:"64"`
	Default   bool   `json:"default,omitempty" doc:"The account the space's runs of its harness use unless a launch names another"`
}

// AccountList is every account.
type AccountList struct {
	Items []*accounts.Account `json:"items"`
}

// SpaceAccounts is the accounts a space's runs may use.
type SpaceAccounts struct {
	Items []accounts.Binding `json:"items"`
}

type accountOut struct {
	ETag string `header:"ETag"`
	Body *accounts.Account
}

// RegisterAccounts registers the account operations.
func RegisterAccounts(svc *accounts.Service) func(api huma.API, d *Deps) {
	return func(api huma.API, d *Deps) {
		tags := []string{"accounts"}
		huma.Register(api, huma.Operation{OperationID: "list-account-kinds", Method: http.MethodGet, Path: "/account-kinds",
			Summary: "Supported credential kinds and the harness each one is for", Tags: tags},
			func(context.Context, *struct{}) (*struct {
				Body struct {
					Items []accounts.Kind `json:"items"`
				}
			}, error) {
				out := &struct {
					Body struct {
						Items []accounts.Kind `json:"items"`
					}
				}{}
				out.Body.Items = accounts.Kinds
				return out, nil
			})
		huma.Register(api, huma.Operation{OperationID: "list-accounts", Method: http.MethodGet, Path: "/accounts", Summary: "Harness accounts (never their secrets)", Tags: tags},
			func(ctx context.Context, _ *struct{}) (*struct{ Body AccountList }, error) {
				l, err := svc.List(ctx)
				if err != nil {
					return nil, d.fail(ctx, "accounts.list", err)
				}
				return &struct{ Body AccountList }{Body: AccountList{Items: l}}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "get-account", Method: http.MethodGet, Path: "/accounts/{account_id}", Summary: "An account (never its secret)", Tags: tags},
			func(ctx context.Context, in *AccountPath) (*accountOut, error) {
				a, err := svc.Get(ctx, in.AccountID)
				if err != nil {
					return nil, d.fail(ctx, "accounts.get", err)
				}
				return &accountOut{ETag: ETag(a.Version), Body: a}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "create-account", Method: http.MethodPost, Path: "/accounts", Summary: "Add a harness account",
			Description: "The secret is sealed and can never be read back.", Tags: tags, DefaultStatus: http.StatusCreated},
			func(ctx context.Context, in *struct {
				IdempotencyKey
				Body AccountBody
			}) (*accountOut, error) {
				// The idempotency fingerprint never includes the secret itself.
				fp := map[string]any{"name": in.Body.Name, "kind": in.Body.Kind}
				a, _, err := Idempotent(ctx, d, "create-account", in.IdempotencyKey, fp, false, func() (*accounts.Account, error) {
					return svc.Create(ctx, Actor(ctx), in.Body.input())
				})
				if err != nil {
					return nil, d.fail(ctx, "accounts.create", err)
				}
				return &accountOut{ETag: ETag(a.Version), Body: a}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "update-account", Method: http.MethodPut, Path: "/accounts/{account_id}", Summary: "Edit or rotate an account",
			Description: "A secret rotates the credential for later launches; running harnesses keep the one they started with.", Tags: tags},
			func(ctx context.Context, in *struct {
				AccountPath
				IfMatch
				Body AccountBody
			}) (*accountOut, error) {
				v, err := in.Version()
				if err != nil {
					return nil, d.fail(ctx, "accounts.update", err)
				}
				a, err := svc.Update(ctx, Actor(ctx), in.AccountID, v, in.Body.input())
				if err != nil {
					return nil, d.fail(ctx, "accounts.update", err)
				}
				return &accountOut{ETag: ETag(a.Version), Body: a}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "delete-account", Method: http.MethodDelete, Path: "/accounts/{account_id}", Summary: "Delete an account and its bindings",
			Tags: tags, DefaultStatus: http.StatusNoContent},
			func(ctx context.Context, in *AccountPath) (*struct{}, error) {
				if err := svc.Delete(ctx, Actor(ctx), in.AccountID); err != nil {
					return nil, d.fail(ctx, "accounts.delete", err)
				}
				return &struct{}{}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "list-space-accounts", Method: http.MethodGet, Path: "/spaces/{space_id}/accounts",
			Summary: "Accounts the space's runs may use", Tags: tags},
			func(ctx context.Context, in *SpacePath) (*struct{ Body SpaceAccounts }, error) {
				l, err := svc.Bindings(ctx, in.SpaceID)
				if err != nil {
					return nil, d.fail(ctx, "accounts.bindings", err)
				}
				return &struct{ Body SpaceAccounts }{Body: SpaceAccounts{Items: l}}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "set-space-accounts", Method: http.MethodPut, Path: "/spaces/{space_id}/accounts",
			Summary: "Set the accounts the space's runs may use", Description: "At most one default per harness. Launches use the named account, else the " +
				"default, else the only bound account; otherwise they are refused.", Tags: tags},
			func(ctx context.Context, in *struct {
				SpacePath
				Body struct {
					Items []BindingInput `json:"items" maxItems:"50"`
				}
			}) (*struct{ Body SpaceAccounts }, error) {
				bs := make([]accounts.Binding, 0, len(in.Body.Items))
				for _, b := range in.Body.Items {
					bs = append(bs, accounts.Binding{AccountID: b.AccountID, Default: b.Default})
				}
				l, err := svc.SetBindings(ctx, Actor(ctx), in.SpaceID, bs)
				if err != nil {
					return nil, d.fail(ctx, "accounts.bind", err)
				}
				return &struct{ Body SpaceAccounts }{Body: SpaceAccounts{Items: l}}, nil
			})
	}
}
