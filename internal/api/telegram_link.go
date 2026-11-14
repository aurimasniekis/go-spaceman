package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/aurimasniekis/go-spaceman/internal/telegram/link"
)

// TelegramLinkRules are the rules of linking a Telegram account (ADR-0083 decision 2, plan §3.1). Creating a code and
// confirming the link need a recent re-authentication (an OIDC step-up for SSO users); the service additionally
// accepts only a person's own browser session, never an API token (tokens skip re-authentication), and confirms only
// from the session that created the code (or the session a re-authentication rotated it into). None of them is reachable from Telegram: they are Reauth or personal rules,
// which the Telegram refusal class covers, and the classification excludes them.
var TelegramLinkRules = map[string]Rule{
	"create-telegram-link-code": {Session: true, UserOnly: true, Reauth: true},
	"get-telegram-link-code":    {Session: true, UserOnly: true},
	"cancel-telegram-link-code": {Session: true, UserOnly: true},
	"confirm-telegram-link":     {Session: true, UserOnly: true, Reauth: true},
}

// TelegramLinkCodePath names a link code by its ID.
type TelegramLinkCodePath struct {
	ID string `path:"code_id" maxLength:"43" doc:"The code's ID (not the code)"`
}

type telegramLinkConfirmIn struct {
	MaxRole string `json:"max_role,omitempty" enum:"viewer,operator,admin," doc:"The link's cap; empty takes the policy default. viewer and operator are allowed up to the default; admin (same as your grants) only when the policy allows the admin cap"`
}

// RegisterTelegramLink registers the linking operations of the Account page.
func RegisterTelegramLink(svc *link.Service) func(api huma.API, d *Deps) {
	return func(api huma.API, d *Deps) {
		tags := []string{"telegram"}
		huma.Register(api, huma.Operation{OperationID: "create-telegram-link-code", Method: http.MethodPost, Path: "/me/telegram/link-codes",
			Summary: "Start linking your Telegram account",
			Description: "Returns a single-use code and a t.me deep link that sends /start <code> to the bot. The code is returned only here " +
				"and expires after link_code_ttl_seconds; creating one cancels your unfinished codes. The bot then asks the Telegram " +
				"account to confirm, naming this server and your account; after that, confirm-telegram-link from this same browser session finishes the link. " +
				"Needs a browser session with a recent re-authentication; API tokens can't link.",
			Tags: tags, DefaultStatus: http.StatusCreated},
			func(ctx context.Context, _ *struct{}) (*struct{ Body *link.Code }, error) {
				c, err := svc.CreateCode(ctx, PrincipalFrom(ctx))
				if err != nil {
					return nil, d.fail(ctx, "telegram.link_code.create", err)
				}
				return &struct{ Body *link.Code }{Body: c}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "get-telegram-link-code", Method: http.MethodGet, Path: "/me/telegram/link-codes/{code_id}",
			Summary: "One of your Telegram link codes",
			Description: "The page polls this while it is open. Once the Telegram account confirmed in the bot, the state is " +
				"telegram_confirmed and the answer names that account.",
			Tags: tags},
			func(ctx context.Context, in *TelegramLinkCodePath) (*struct{ Body *link.Code }, error) {
				c, err := svc.GetCode(ctx, PrincipalFrom(ctx), in.ID)
				if err != nil {
					return nil, d.fail(ctx, "telegram.link_code.get", err)
				}
				return &struct{ Body *link.Code }{Body: c}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "cancel-telegram-link-code", Method: http.MethodPost, Path: "/me/telegram/link-codes/{code_id}/cancel",
			Summary: "Cancel one of your Telegram link codes", Description: "Cancelling a used or cancelled code changes nothing.", Tags: tags},
			func(ctx context.Context, in *TelegramLinkCodePath) (*struct{ Body *link.Code }, error) {
				c, err := svc.CancelCode(ctx, PrincipalFrom(ctx), in.ID)
				if err != nil {
					return nil, d.fail(ctx, "telegram.link_code.cancel", err)
				}
				return &struct{ Body *link.Code }{Body: c}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "confirm-telegram-link", Method: http.MethodPost, Path: "/me/telegram/link-codes/{code_id}/confirm",
			Summary: "Link the Telegram account that confirmed your code",
			Description: "Only after the Telegram account confirmed in the bot, before the code expires, from the browser session " +
				"that created the code and with a recent re-authentication. The link's grants are your grants capped at max_role; every " +
				"request through it is also checked against your current grants.",
			Tags: tags, DefaultStatus: http.StatusCreated},
			func(ctx context.Context, in *struct {
				TelegramLinkCodePath
				Body *telegramLinkConfirmIn `required:"false"`
			}) (*struct{ Body *link.Link }, error) {
				var maxRole string
				if in.Body != nil {
					maxRole = in.Body.MaxRole
				}
				l, err := svc.Confirm(ctx, PrincipalFrom(ctx), in.ID, maxRole)
				if err != nil {
					return nil, d.fail(ctx, "telegram.link.confirm", err)
				}
				return &struct{ Body *link.Link }{Body: l}, nil
			})
	}
}
