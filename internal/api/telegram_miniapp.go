package api

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/miniapp"
)

// TelegramMiniAppRules are the rules of the Mini App's session exchange (ADR-0083 §3.4.2): public, served only on the
// Mini App host (telegram.Ops classifies it as the exchange; on any other host it is not found).
var TelegramMiniAppRules = map[string]Rule{
	"create-telegram-session": public(),
	// The Mini App's own identity: /auth/me is not served on the Mini App host (the whole /auth/ prefix is a 404 there).
	"get-telegram-me": {Session: true, TelegramSelf: true},
	// The caller needs terminal:watch in the space for it to be true; the handler checks that itself, per space.
	"get-telegram-space-features": {Session: true, TelegramSelf: true},
}

// TelegramExchanger exchanges a Mini App's initData for a session (miniapp.Service).
type TelegramExchanger interface {
	Exchange(ctx context.Context, initData, clientIP string) (*miniapp.Session, error)
}

// telegramSessionBodyLimit is the exchange's body limit (plan §3.4.2: 8 KiB).
const telegramSessionBodyLimit = 8 << 10

type telegramSessionIn struct {
	Body struct {
		InitData string `json:"init_data" minLength:"1" maxLength:"8000" doc:"The raw Telegram.WebApp.initData string, as the Mini App received it"`
	}
}

// TelegramSessionOut is a new Mini App session.
type TelegramSessionOut struct {
	Token              string    `json:"token" doc:"The session token, sent as \"Authorization: Telegram <token>\" on the Mini App host only; never in a URL or a cookie"`
	ExpiresAt          time.Time `json:"expires_at" doc:"When the session ends whatever its use"`
	IdleTimeoutSeconds int64     `json:"idle_timeout_seconds" doc:"The session ends after this long without a request"`
}

// TelegramSpaceFeatures is what a Telegram link may use in a space beyond the allowlisted operations.
type TelegramSpaceFeatures struct {
	TerminalWatch bool `json:"terminal_watch" doc:"The watch-only terminal is on for this space and the caller may watch it"`
}

// RegisterTelegramMiniApp registers the Mini App's session exchange and its identity read. A nil exchanger serves
// nothing (404).
func RegisterTelegramMiniApp(ex TelegramExchanger) func(api huma.API, d *Deps) {
	return func(api huma.API, d *Deps) {
		huma.Register(api, huma.Operation{OperationID: "get-telegram-me", Method: http.MethodGet, Path: "/telegram/me",
			Summary: "The Mini App session's principal",
			Description: "Mini App host only (404 elsewhere). The same document as get-me, for the Telegram link's user, so the Mini App " +
				"can tell what the person may do. It carries no CSRF token and the session never needs one.",
			Tags: []string{"telegram"}},
			func(ctx context.Context, _ *struct{}) (*struct{ Body Me }, error) {
				p := PrincipalFrom(ctx)
				if ex == nil || telegramChannelFrom(ctx) != channelMiniApp || !p.ViaTelegram() {
					return nil, d.fail(ctx, "telegram.me", apperr.New(apperr.CodeNotFound, "not found"))
				}
				return &struct{ Body Me }{Body: d.me(p)}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "get-telegram-space-features", Method: http.MethodGet, Path: "/telegram/spaces/{space_id}/features",
			Summary: "What the Mini App may show in a space",
			Description: "Mini App host only (404 elsewhere). terminal_watch is true only when the space's admin turned the watch-only " +
				"terminal on for Telegram and the caller may watch terminals in the space; otherwise it is false, so the Mini App " +
				"hides the terminal. The stream route enforces the same two conditions itself.",
			Tags: []string{"telegram"}},
			func(ctx context.Context, in *struct {
				SpaceID string `path:"space_id" doc:"The space"`
			}) (*struct{ Body TelegramSpaceFeatures }, error) {
				p := PrincipalFrom(ctx)
				if ex == nil || telegramChannelFrom(ctx) != channelMiniApp || !p.ViaTelegram() {
					return nil, d.fail(ctx, "telegram.features", apperr.New(apperr.CodeNotFound, "not found"))
				}
				on := authz.ValidDomain(in.SpaceID) && in.SpaceID != authz.Global && d.Authz != nil &&
					d.Authz.AllowedAll(p.Subjects(), in.SpaceID, authz.TerminalWatch) &&
					d.telegramFeature(ctx, in.SpaceID, TelegramFeatureTerminalWatch)
				return &struct{ Body TelegramSpaceFeatures }{Body: TelegramSpaceFeatures{TerminalWatch: on}}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "create-telegram-session", Method: http.MethodPost, Path: "/telegram/session",
			Summary: "Exchange Telegram Mini App initData for a session",
			Description: "Served only on the Mini App host (miniapp_host; 404 elsewhere). The initData must carry a valid signature of the " +
				"configured bot, be at most init_data_max_age_seconds old and not used before (each initData opens one session); its " +
				"Telegram user needs an active link. Refused initData is 401 whatever the reason. Rate limited per address and per " +
				"Telegram user. The session lives in memory only and ends on idle, absolute expiry, unlink, user disable, a bot change " +
				"or a restart; the Mini App then asks to be reopened.",
			Tags: []string{"telegram"}, Security: []map[string][]string{}, MaxBodyBytes: telegramSessionBodyLimit, DefaultStatus: http.StatusCreated},
			func(ctx context.Context, in *telegramSessionIn) (*struct{ Body TelegramSessionOut }, error) {
				if ex == nil || telegramChannelFrom(ctx) != channelMiniApp {
					// Mini App host only: the main host doesn't have this operation.
					return nil, d.fail(ctx, "telegram.session", apperr.New(apperr.CodeNotFound, "not found"))
				}
				s, err := ex.Exchange(ctx, in.Body.InitData, ClientIP(ctx))
				if err != nil {
					return nil, d.fail(ctx, "telegram.session", err)
				}
				return &struct{ Body TelegramSessionOut }{Body: TelegramSessionOut{Token: s.Token, ExpiresAt: s.ExpiresAt,
					IdleTimeoutSeconds: int64(s.Idle / time.Second)}}, nil
			})
	}
}
