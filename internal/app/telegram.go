package app

import (
	"context"
	"log/slog"
	"net/netip"

	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/obs"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/tgbot"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/updates"
)

// trustedProxies parses http.trusted_proxies (validated with the configuration).
func (c *Central) trustedProxies() []netip.Prefix {
	var trusted []netip.Prefix
	for _, p := range c.Cfg.HTTP.TrustedProxies {
		if pf, err := netip.ParsePrefix(p); err == nil {
			trusted = append(trusted, pf)
		}
	}
	return trusted
}

// newTelegramUpdates builds the Telegram inbound side (ADR-0083). Until a bot is configured it has no webhook
// configuration (every webhook request is 404), no links (every user is unlinked) and no client (nothing is sent).
func (c *Central) newTelegramUpdates() *updates.Service {
	log := obs.Component(c.Log, "telegram")
	return updates.New(updates.Options{
		Intake: updates.IntakeOptions{Metrics: c.Metrics, Log: log},
		Webhook: updates.WebhookOptions{TrustedProxies: c.trustedProxies(), Clock: c.Clock, Log: log,
			OnUnauthorized: c.auditTelegramUnauthorized},
		Dispatcher: updates.DispatcherOptions{Clock: c.Clock, Metrics: c.Metrics, Log: log},
	})
}

// newTelegramBot builds the bot's connection service (ADR-0083). It configures TelegramUpdates (webhook route,
// dispatcher client, poller) whenever the bot is written and at start (Serve calls Start).
func (c *Central) newTelegramBot() *tgbot.Service {
	return tgbot.New(c.Store, c.Audit, c.Keyring, tgbot.Options{BaseURL: c.Cfg.Telegram.APIBaseURL, PublicURL: c.Cfg.PublicURL,
		Updates: c.TelegramUpdates, Bus: c.Bus, Log: obs.Component(c.Log, "telegram")})
}

// auditTelegramUnauthorized records webhook requests refused for their secret token (at most once a minute, with the
// number refused since the last record). The presented token is never recorded.
func (c *Central) auditTelegramUnauthorized(ctx context.Context, clientIP string, refused int64) {
	// The record must not depend on the refused client staying connected.
	ctx = context.WithoutCancel(ctx)
	err := c.Store.W().TxNamed(ctx, "telegram.webhook_refused", func(tx *store.Tx) error {
		_, err := c.Audit.Append(ctx, tx, audit.Event{Actor: audit.System, Action: "telegram.webhook.refused",
			Target: audit.Target{Type: "telegram_webhook"}, Outcome: audit.OutcomeDenied, SourceIP: clientIP,
			Details: map[string]any{"reason": "secret_token", "refused": refused}})
		return err
	})
	if err != nil {
		c.Log.Warn("telegram: auditing refused webhook requests failed", slog.String("error", err.Error()))
	}
}
