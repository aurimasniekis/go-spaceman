package app

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/api"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/obs"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/miniapp"
	tgsettings "github.com/aurimasniekis/go-spaceman/internal/telegram/settings"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/tghost"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/updates"
)

// The Telegram Mini App's host and sessions (ADR-0083 §3.4.1–§3.4.2).

// newTelegramMiniApp builds the Mini App's session service. Its sessions name a link; the link's principal is built
// on every request with the current link_sso_grants, so an unlink, a cap or grants change, a disabled user or a policy
// change applies to the next request. Open streams re-check when link_sso_grants changes.
func (c *Central) newTelegramMiniApp() {
	c.TelegramMiniApp = miniapp.New(miniapp.Options{
		Bot: c.TelegramBot.MiniAppBot,
		Limits: func() miniapp.Limits {
			s := c.TelegramSettings.Current()
			return miniapp.Limits{InitDataMaxAge: time.Duration(s.InitDataMaxAgeSeconds) * time.Second,
				Idle: time.Duration(s.MiniAppSessionIdleSeconds) * time.Second, Absolute: time.Duration(s.MiniAppSessionAbsoluteSeconds) * time.Second}
		},
		ActiveLink: func(ctx context.Context, botID, tgUserID int64) (string, error) {
			id, err := c.Store.R().GetActiveTelegramLinkForUser(ctx, db.GetActiveTelegramLinkForUserParams{BotID: botID, TgUserID: tgUserID})
			if errors.Is(err, sql.ErrNoRows) {
				return "", miniapp.ErrNotLinked
			}
			return id, err
		},
		Principal: func(ctx context.Context, linkID string) (*auth.Principal, error) {
			return c.Auth.TelegramPrincipal(ctx, linkID, auth.TelegramOptions{SSOGrants: c.telegramSSOGrants()})
		},
		OnRefused: c.auditMiniAppRefused,
		Clock:     c.Clock,
		Log:       obs.Component(c.Log, "telegram"),
	})
	c.TelegramDownloads = miniapp.NewDownloads(c.Clock)
	// A space's Telegram visibility or terminal_watch turned off: open terminal streams end now, not at their next
	// interval (the feature gate itself reads the store, so the re-check refuses).
	c.TelegramSettings.OnSpaceOff(func(string) { c.Auth.TelegramPolicyChanged() })
	var last *bool
	c.TelegramSettings.OnChange(func(s tgsettings.Settings) {
		if last != nil && *last != s.LinkSSOGrants {
			// Which subject a link acts as changed: open Telegram streams re-check now, not at their next interval.
			c.Auth.TelegramPolicyChanged()
		}
		v := s.LinkSSOGrants
		last = &v
	})
}

// isMiniAppHost reports whether a request's Host (r.Host, never X-Forwarded-Host) is a Mini App host (ADR-0083
// §3.4.1), compared case-insensitively without the port and one trailing dot: central's telegram.miniapp_host, read
// from the configuration here so it holds even without a bot service, or the stored bot's miniapp_host, enabled or
// not (when it differs from telegram.miniapp_host it is ignored, but still never reaches the main handler). Such a
// request is served by the Mini App handler only.
func (c *Central) isMiniAppHost(hostport string) bool {
	h := tghost.Of(hostport)
	if h == "" {
		return false
	}
	if h == c.Cfg.Telegram.MiniAppHost {
		return true
	}
	return c.TelegramBot != nil && c.TelegramBot.RoutesToMiniApp(h)
}

// miniAppServing reports whether the running bot serves the Mini App on hostport: a bot runs, enabled, and its Mini
// App host is this one. Otherwise the Mini App host answers 404 for everything.
func (c *Central) miniAppServing(hostport string) bool {
	if c.TelegramBot == nil {
		return false
	}
	h := c.TelegramBot.MiniAppHost()
	return h != "" && tghost.Of(hostport) == h
}

// miniAppOrigin is the Mini App's origin: public_url's scheme and port on the Mini App host ("" when there is none).
// Mini App mutations must come from it (api.TelegramPolicy.MiniAppOrigin).
func (c *Central) miniAppOrigin() string {
	if c.TelegramBot == nil {
		return ""
	}
	h := c.TelegramBot.MiniAppHost()
	u, err := url.Parse(c.Cfg.PublicURL)
	if h == "" || err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return ""
	}
	if p := u.Port(); p != "" {
		h = net.JoinHostPort(h, p)
	}
	return u.Scheme + "://" + h
}

// miniAppHandler serves the Mini App host. It is an allowlist (plan §3.4.1): the API in Telegram mode (only the
// Telegram scheme authenticates; anonymous requests reach only the session exchange), the signed downloads, tg.html
// with its assets, and nothing else. Sign-in and everything under /api/v1/auth/, the external MCP endpoint, the node
// protocol, the Telegram webhook and the main web UI are never served here: they are a plain 404, without a redirect to
// the main host.
//
// It never hands a request to the main handler. While no bot serves the Mini App (none stored, disabled, failed to
// start, or its Mini App host is another), everything is a 404.
func (c *Central) miniAppHandler(d *api.Deps) http.Handler {
	mux := c.miniAppMux(d)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !c.miniAppServing(r.Host) {
			miniAppNotFound(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// miniAppMux is the Mini App host's allowlist of routes.
func (c *Central) miniAppMux(d *api.Deps) http.Handler {
	mux := http.NewServeMux()
	mux.Handle(api.Prefix+"/", d.TelegramHandler())
	mux.Handle(api.Prefix+"/auth/", http.HandlerFunc(miniAppNotFound))
	mux.Handle(api.MCPPath, http.HandlerFunc(miniAppNotFound))
	mux.Handle(api.MCPPath+"/", http.HandlerFunc(miniAppNotFound))
	mux.Handle("/agent/", http.HandlerFunc(miniAppNotFound))
	mux.Handle(updates.WebhookPath, http.HandlerFunc(miniAppNotFound))
	// The one route that carries a CORS header, and the only one without a session: its ID is the capability.
	mux.Handle("GET "+api.TelegramDownloadPath+"{id}", d.TelegramDownloadHandler(c.TelegramDownloads, c.TelegramMiniApp, c.Attachments))
	// Everything else is the shell and its assets; a build without them is a 404.
	shell := http.HandlerFunc(miniAppNotFound)
	if c.MiniAppUI != nil {
		mux.Handle("/", c.MiniAppUI)
	} else {
		mux.Handle("/", shell)
	}
	return mux
}

func miniAppNotFound(w http.ResponseWriter, _ *http.Request) {
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte("not found\n"))
}

// auditMiniAppRefused records refused Mini App session exchanges (forged, malformed, expired or replayed initData), at
// most once a minute per address, with the number refused since the last record. The initData is never recorded. The
// overflow row (source address miniapp.OverflowKey) carries details.overflow, so it is told from a per-address row.
func (c *Central) auditMiniAppRefused(ctx context.Context, clientIP string, refused int64) {
	ctx = context.WithoutCancel(ctx)
	details := map[string]any{"reason": "init_data", "refused": refused}
	if clientIP == miniapp.OverflowKey {
		details["overflow"] = true
	}
	err := c.Store.W().TxNamed(ctx, "telegram.miniapp_refused", func(tx *store.Tx) error {
		_, err := c.Audit.Append(ctx, tx, audit.Event{Actor: audit.System, Action: "telegram.miniapp.session.refused",
			Target: audit.Target{Type: "telegram_miniapp"}, Outcome: audit.OutcomeDenied, SourceIP: clientIP,
			Details: details})
		return err
	})
	if err != nil {
		c.Log.Warn("telegram: auditing refused Mini App exchanges failed", slog.String("error", err.Error()))
	}
}
