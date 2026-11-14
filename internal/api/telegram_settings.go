package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/settings"
)

// TelegramSettingsRules are the authorization rules of the Telegram settings (ADR-0083, plan §5.2). Every write needs
// a step-up: the global settings with system:write, a space's with space_config:write in that space. They are what
// terraform-provider-spaceman manages (spaceman_telegram_settings, spaceman_space_telegram); API tokens skip the
// step-up as they do everywhere.
var TelegramSettingsRules = map[string]Rule{
	"get-telegram-settings":    Global(authz.SystemRead),
	"put-telegram-settings":    StepUp(authz.SystemWrite),
	"delete-telegram-settings": StepUp(authz.SystemWrite),
	"list-space-telegram":      FilteredRead(),
	"get-space-telegram":       InSpace(authz.SpaceConfigRead),
	"put-space-telegram":       {Perm: authz.SpaceConfigWrite, Space: true, Reauth: true, MFA: true},
	"delete-space-telegram":    {Perm: authz.SpaceConfigWrite, Space: true, Reauth: true, MFA: true},
}

type telegramSettingsOut struct {
	ETag string `header:"ETag"`
	Body *settings.SettingsOut
}

type spaceTelegramOut struct {
	ETag string `header:"ETag"`
	Body *settings.Space
}

type spaceTelegramList struct {
	Items []settings.Space `json:"items"`
}

// RegisterTelegramSettings registers the global and per-space Telegram settings operations.
func RegisterTelegramSettings(svc *settings.Service) func(api huma.API, d *Deps) {
	return func(api huma.API, d *Deps) {
		tags := []string{"telegram"}
		huma.Register(api, huma.Operation{OperationID: "get-telegram-settings", Method: http.MethodGet, Path: "/telegram/settings",
			Summary:     "The Telegram bot's policy and defaults",
			Description: "The effective values: built-in defaults for everything never set (version 0 and stored false while nothing is stored).",
			Tags:        tags},
			func(ctx context.Context, _ *struct{}) (*telegramSettingsOut, error) {
				s, err := svc.Get(ctx)
				if err != nil {
					return nil, d.fail(ctx, "telegram.settings.get", err)
				}
				return &telegramSettingsOut{ETag: ETag(s.Version), Body: s}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "put-telegram-settings", Method: http.MethodPut, Path: "/telegram/settings",
			Summary: "Set the Telegram bot's policy and defaults",
			Description: "Without If-Match this stores the settings for the first time (409 if some are stored); with it, it replaces them. " +
				"The body replaces everything stored: a field left out takes its built-in default, and only the fields sent are stored, so " +
				"a later release's new default reaches every field nobody set. Out-of-range values are refused with 422. Changes reach the " +
				"bot (send and inbound limits, link policy) without a restart. The Bot API base URL is not here: it is set only in central's " +
				"configuration file. Needs system:write and a step-up.",
			Tags: tags},
			func(ctx context.Context, in *struct {
				IfMatch
				Body settings.Input
			}) (*telegramSettingsOut, error) {
				v, err := optionalVersion(in.IfMatch)
				if err != nil {
					return nil, d.fail(ctx, "telegram.settings.put", err)
				}
				s, err := svc.Put(ctx, Actor(ctx), v, in.Body)
				if err != nil {
					return nil, d.fail(ctx, "telegram.settings.put", err)
				}
				return &telegramSettingsOut{ETag: ETag(s.Version), Body: s}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "delete-telegram-settings", Method: http.MethodDelete, Path: "/telegram/settings",
			Summary: "Reset the Telegram bot's policy and defaults", Description: "Every value goes back to its built-in default. Needs system:write and a step-up.",
			Tags: tags, DefaultStatus: http.StatusNoContent},
			func(ctx context.Context, in *IfMatch) (*struct{}, error) {
				v, err := in.Version()
				if err != nil {
					return nil, d.fail(ctx, "telegram.settings.delete", err)
				}
				if err := svc.Delete(ctx, Actor(ctx), v); err != nil {
					return nil, d.fail(ctx, "telegram.settings.delete", err)
				}
				return &struct{}{}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "list-space-telegram", Method: http.MethodGet, Path: "/telegram/spaces",
			Summary: "Spaces' Telegram settings", Description: "Every space with stored Telegram settings whose configuration you can read; a space not listed is off for Telegram.",
			Tags: tags},
			func(ctx context.Context, _ *struct{}) (*struct{ Body spaceTelegramList }, error) {
				all, err := svc.ListSpaces(ctx)
				if err != nil {
					return nil, d.fail(ctx, "telegram.space.list", err)
				}
				p := PrincipalFrom(ctx)
				out := spaceTelegramList{Items: make([]settings.Space, 0, len(all))}
				for _, s := range all {
					if d.Authz != nil && p != nil && d.Authz.AllowedAll(p.Subjects(), s.SpaceID, authz.SpaceConfigRead) {
						out.Items = append(out.Items, s)
					}
				}
				return &struct{ Body spaceTelegramList }{Body: out}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "get-space-telegram", Method: http.MethodGet, Path: "/spaces/{space_id}/telegram",
			Summary: "A space's Telegram settings", Description: "The defaults (enabled false, version 0) when the space has none: it is then invisible to Telegram.",
			Tags: tags},
			func(ctx context.Context, in *SpacePath) (*spaceTelegramOut, error) {
				s, err := svc.GetSpace(ctx, in.SpaceID)
				if err != nil {
					return nil, d.fail(ctx, "telegram.space.get", err)
				}
				return &spaceTelegramOut{ETag: ETag(s.Version), Body: s}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "put-space-telegram", Method: http.MethodPut, Path: "/spaces/{space_id}/telegram",
			Summary: "Set a space's Telegram settings",
			Description: "Without If-Match this creates the settings (409 if the space has some); with it, it replaces them. A field left out takes " +
				"its default. Needs space_config:write in the space and a step-up.",
			Tags: tags},
			func(ctx context.Context, in *struct {
				SpacePath
				IfMatch
				Body settings.SpaceInput
			}) (*spaceTelegramOut, error) {
				v, err := optionalVersion(in.IfMatch)
				if err != nil {
					return nil, d.fail(ctx, "telegram.space.put", err)
				}
				s, err := svc.PutSpace(ctx, Actor(ctx), in.SpaceID, v, in.Body)
				if err != nil {
					return nil, d.fail(ctx, "telegram.space.put", err)
				}
				return &spaceTelegramOut{ETag: ETag(s.Version), Body: s}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "delete-space-telegram", Method: http.MethodDelete, Path: "/spaces/{space_id}/telegram",
			Summary: "Remove a space's Telegram settings (Telegram is off for it again)", Description: "Needs space_config:write in the space and a step-up.",
			Tags: tags, DefaultStatus: http.StatusNoContent},
			func(ctx context.Context, in *struct {
				SpacePath
				IfMatch
			}) (*struct{}, error) {
				v, err := in.Version()
				if err != nil {
					return nil, d.fail(ctx, "telegram.space.delete", err)
				}
				if err := svc.DeleteSpace(ctx, Actor(ctx), in.SpaceID, v); err != nil {
					return nil, d.fail(ctx, "telegram.space.delete", err)
				}
				return &struct{}{}, nil
			})
	}
}
