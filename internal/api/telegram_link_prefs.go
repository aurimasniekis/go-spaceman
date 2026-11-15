package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/link"
)

// TelegramLinkPrefsRules are the rules of a Telegram link's own preferences (ADR-0083 amendment TG-10-4): Telegram
// self-service session rules, so no other principal kind (the SpaceMan MCP, say) reaches them, and the handler serves
// only a Telegram principal on the Mini App host. The link is the caller's own, taken from its session: no operation
// takes a link ID.
var TelegramLinkPrefsRules = map[string]Rule{
	"get-my-telegram-link-prefs": {Session: true, TelegramSelf: true},
	"set-my-telegram-link-prefs": {Session: true, TelegramSelf: true},
}

type telegramLinkPrefsOut struct {
	ETag string `header:"ETag"`
	Body *link.Prefs
}

// RegisterTelegramLinkPrefs registers the operations of a link's own preferences. A nil service serves nothing (404).
func RegisterTelegramLinkPrefs(svc *link.Service) func(api huma.API, d *Deps) {
	return func(api huma.API, d *Deps) {
		tags := []string{"telegram"}
		// mine refuses anything but a Telegram principal on the Mini App host with a plain 404, like get-telegram-me.
		mine := func(ctx context.Context, op string) error {
			if svc == nil || telegramChannelFrom(ctx) != channelMiniApp || !PrincipalFrom(ctx).ViaTelegram() {
				return d.fail(ctx, op, apperr.New(apperr.CodeNotFound, "not found"))
			}
			return nil
		}
		huma.Register(api, huma.Operation{OperationID: "get-my-telegram-link-prefs", Method: http.MethodGet, Path: "/telegram/link/prefs",
			Summary: "Your Telegram link's notification preferences",
			Description: "Mini App host only (404 elsewhere). The preferences of the Telegram link the session belongs to: its overrides, " +
				"the defaults they apply to, the effective result and your time zone. There is no link ID: it is always the caller's own link.",
			Tags: tags},
			func(ctx context.Context, _ *struct{}) (*telegramLinkPrefsOut, error) {
				if err := mine(ctx, "telegram.link.prefs.get"); err != nil {
					return nil, err
				}
				p, err := svc.GetPrefs(ctx, PrincipalFrom(ctx))
				if err != nil {
					return nil, d.fail(ctx, "telegram.link.prefs.get", err)
				}
				return &telegramLinkPrefsOut{ETag: ETag(p.Version), Body: p}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "set-my-telegram-link-prefs", Method: http.MethodPut, Path: "/telegram/link/prefs",
			Summary: "Save your Telegram link's notification preferences",
			Description: "Mini App host only (404 elsewhere). Replaces the overrides of the Telegram link the session belongs to: a field " +
				"left out follows the defaults again. timezone, when sent, sets your user's time zone (empty clears it). Send If-Match " +
				"with the version you read. It changes nothing else: not another link, the link's role cap, spaces or grants, nor any " +
				"space's or the global Telegram settings.",
			Tags: tags, MaxBodyBytes: 16 << 10},
			func(ctx context.Context, in *struct {
				IfMatch
				Body link.PrefsInput
			}) (*telegramLinkPrefsOut, error) {
				if err := mine(ctx, "telegram.link.prefs.set"); err != nil {
					return nil, err
				}
				v, err := in.Version()
				if err != nil {
					return nil, d.fail(ctx, "telegram.link.prefs.set", err)
				}
				p, err := svc.SetPrefs(ctx, PrincipalFrom(ctx), Actor(ctx), v, in.Body)
				if err != nil {
					return nil, d.fail(ctx, "telegram.link.prefs.set", err)
				}
				return &telegramLinkPrefsOut{ETag: ETag(p.Version), Body: p}, nil
			})
	}
}
