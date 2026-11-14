package app

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/api"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/obs"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/miniapp"
	tgsettings "github.com/aurimasniekis/go-spaceman/internal/telegram/settings"
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

// hostOf is a Host header's host name: lower case, without the port and one trailing dot. "tg.example.com." is the
// same host to DNS, so it must get the Mini App host's allowlist, never the main host's routes under the Mini App name.
func hostOf(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		hostport = h
	}
	return strings.ToLower(strings.TrimSuffix(hostport, "."))
}

// isMiniAppHost reports whether a request's Host (r.Host, never X-Forwarded-Host) is the running bot's Mini App host.
// With no bot or no miniapp_host, nothing is.
func (c *Central) isMiniAppHost(hostport string) bool {
	if c.TelegramBot == nil {
		return false
	}
	h := c.TelegramBot.MiniAppHost()
	return h != "" && hostOf(hostport) == h
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
// Telegram scheme authenticates; anonymous requests reach only the session exchange), and nothing else. Sign-in and
// everything under /api/v1/auth/, the external MCP endpoint, the node protocol, the Telegram webhook and the main web
// UI are never served here: anything not listed is a plain 404, without a redirect to the main host.
func (c *Central) miniAppHandler(d *api.Deps) http.Handler {
	mux := http.NewServeMux()
	mux.Handle(api.Prefix+"/", d.TelegramHandler())
	mux.Handle(api.Prefix+"/auth/", http.HandlerFunc(miniAppNotFound))
	mux.Handle(api.MCPPath, http.HandlerFunc(miniAppNotFound))
	mux.Handle(api.MCPPath+"/", http.HandlerFunc(miniAppNotFound))
	mux.Handle("/", http.HandlerFunc(miniAppNotFound))
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
// most once a minute per address, with the number refused since the last record. The initData is never recorded.
func (c *Central) auditMiniAppRefused(ctx context.Context, clientIP string, refused int64) {
	ctx = context.WithoutCancel(ctx)
	err := c.Store.W().TxNamed(ctx, "telegram.miniapp_refused", func(tx *store.Tx) error {
		_, err := c.Audit.Append(ctx, tx, audit.Event{Actor: audit.System, Action: "telegram.miniapp.session.refused",
			Target: audit.Target{Type: "telegram_miniapp"}, Outcome: audit.OutcomeDenied, SourceIP: clientIP,
			Details: map[string]any{"reason": "init_data", "refused": refused}})
		return err
	})
	if err != nil {
		c.Log.Warn("telegram: auditing refused Mini App exchanges failed", slog.String("error", err.Error()))
	}
}
