package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/aurimasniekis/go-spaceman/internal/authz"
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
	// Your own links: list, unlink (from the web), re-confirm every link_reconfirm_days (needs a fresh login).
	"list-my-telegram-links":  {Session: true, UserOnly: true},
	"delete-my-telegram-link": {Session: true, UserOnly: true},
	"reconfirm-telegram-link": {Session: true, UserOnly: true, Reauth: true},
	// An administrator's view and unlink of anyone's link.
	"list-telegram-links":  Global(authz.UserRead),
	"delete-telegram-link": StepUp(authz.UserWrite),
}

// TelegramLinkPath names a link by its ID.
type TelegramLinkPath struct {
	ID string `path:"link_id" maxLength:"64" doc:"The link's ID"`
}

// TelegramLinkCodePath names a link code by its ID.
type TelegramLinkCodePath struct {
	ID string `path:"code_id" maxLength:"43" doc:"The code's ID (not the code)"`
}

type telegramLinkConfirmIn struct {
	MaxRole  string `json:"max_role,omitempty" enum:"viewer,operator,admin," doc:"The link's cap; empty takes the policy default. viewer and operator are allowed up to the default; admin (same as your grants) only when the policy allows the admin cap"`
	Timezone string `json:"timezone,omitempty" maxLength:"64" doc:"The browser's IANA time zone, such as Europe/Vilnius. It is saved as your time zone only if you have none (it never overwrites one you chose); an invalid zone is ignored"`
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
				var maxRole, tz string
				if in.Body != nil {
					maxRole, tz = in.Body.MaxRole, in.Body.Timezone
				}
				l, err := svc.Confirm(ctx, PrincipalFrom(ctx), in.ID, maxRole, tz)
				if err != nil {
					return nil, d.fail(ctx, "telegram.link.confirm", err)
				}
				return &struct{ Body *link.Link }{Body: l}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "list-my-telegram-links", Method: http.MethodGet, Path: "/me/telegram/links",
			Summary: "Your linked Telegram accounts", Tags: tags},
			func(ctx context.Context, _ *struct{}) (*struct{ Body []*link.Link }, error) {
				ls, err := svc.ListMine(ctx, PrincipalFrom(ctx))
				if err != nil {
					return nil, d.fail(ctx, "telegram.link.list", err)
				}
				return &struct{ Body []*link.Link }{Body: ls}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "delete-my-telegram-link", Method: http.MethodDelete, Path: "/me/telegram/links/{link_id}",
			Summary: "Unlink your Telegram account",
			Description: "Ends the link at once: the account's next message is refused and its grants are removed. " +
				"Unlinking an already unlinked link changes nothing. To link again, start from the Account page.",
			Tags: tags, DefaultStatus: http.StatusNoContent},
			func(ctx context.Context, in *TelegramLinkPath) (*struct{}, error) {
				if err := svc.UnlinkMine(ctx, PrincipalFrom(ctx), in.ID); err != nil {
					return nil, d.fail(ctx, "telegram.link.unlink", err)
				}
				return nil, nil
			})
		huma.Register(api, huma.Operation{OperationID: "reconfirm-telegram-link", Method: http.MethodPost, Path: "/me/telegram/links/{link_id}/reconfirm",
			Summary: "Re-confirm your Telegram link",
			Description: "A link must be re-confirmed from the web every link_reconfirm_days (default 90) or it does nothing. " +
				"Needs a browser session with a recent re-authentication.",
			Tags: tags},
			func(ctx context.Context, in *TelegramLinkPath) (*struct{ Body *link.Link }, error) {
				l, err := svc.Reconfirm(ctx, PrincipalFrom(ctx), in.ID)
				if err != nil {
					return nil, d.fail(ctx, "telegram.link.reconfirm", err)
				}
				return &struct{ Body *link.Link }{Body: l}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "list-telegram-links", Method: http.MethodGet, Path: "/telegram/links",
			Summary: "Every linked Telegram account", Description: "Needs user:read. Revoked links are not listed.", Tags: tags},
			func(ctx context.Context, _ *struct{}) (*struct{ Body []*link.Link }, error) {
				ls, err := svc.List(ctx)
				if err != nil {
					return nil, d.fail(ctx, "telegram.link.list_all", err)
				}
				return &struct{ Body []*link.Link }{Body: ls}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "delete-telegram-link", Method: http.MethodDelete, Path: "/telegram/links/{link_id}",
			Summary: "Unlink anyone's Telegram account",
			Description: "An administrator ends a link at once (a lost phone, a departed colleague). Needs user:write and a recent " +
				"re-authentication with a second factor.",
			Tags: tags, DefaultStatus: http.StatusNoContent},
			func(ctx context.Context, in *TelegramLinkPath) (*struct{}, error) {
				if err := svc.UnlinkAny(ctx, Actor(ctx), in.ID); err != nil {
					return nil, d.fail(ctx, "telegram.link.admin_unlink", err)
				}
				return nil, nil
			})
	}
}
