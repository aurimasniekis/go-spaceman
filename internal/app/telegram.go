package app

import (
	"context"
	"log/slog"
	"net/netip"

	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/obs"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	tgsettings "github.com/aurimasniekis/go-spaceman/internal/telegram/settings"
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

// newTelegram builds the Telegram settings and the inbound side (ADR-0083). Until a bot is configured the inbound side
// has no webhook configuration (every webhook request is 404), no links (every user is unlinked) and no client
// (nothing is sent).
//
// The settings are read once here, so the first update already sees them; a failure only logs and keeps the defaults
// until the settings' Run reloads. Every later change reaches the Intake's rate and the per-link limiter through
// OnChange. The outbound sender is bound once, in OpenCentral, through the bot service (tgbot.Service.SetLimits), which
// also applies the limits to every sender it makes later.
func (c *Central) newTelegram(ctx context.Context) {
	log := obs.Component(c.Log, "telegram")
	c.TelegramSettings = tgsettings.New(c.Store, c.Audit, c.Bus, log)
	if err := c.TelegramSettings.Load(ctx); err != nil {
		log.Warn("telegram: loading the settings failed; using the defaults until they load", slog.String("error", err.Error()))
	}
	c.TelegramLimiter = updates.NewLimiter(updates.InboundLimits{}, c.Clock)
	c.TelegramUpdates = updates.New(updates.Options{
		Intake: updates.IntakeOptions{Metrics: c.Metrics, Log: log},
		Webhook: updates.WebhookOptions{TrustedProxies: c.trustedProxies(), Clock: c.Clock, Log: log,
			OnUnauthorized: c.auditTelegramUnauthorized},
		Dispatcher: updates.DispatcherOptions{Clock: c.Clock, Metrics: c.Metrics, Log: log, Limiter: c.TelegramLimiter},
	})
	c.TelegramSettings.OnChange(func(s tgsettings.Settings) {
		c.TelegramUpdates.Intake.SetRate(float64(s.InboundLimits.UpdatesPerSecond))
		c.TelegramLimiter.SetLimits(s.InboundLimits.Updates())
	})
}

// newTelegramBot builds the bot's connection service (ADR-0083). It configures TelegramUpdates (webhook route,
// dispatcher client, poller) whenever the bot is written and at start (Serve calls Start).
func (c *Central) newTelegramBot() *tgbot.Service {
	return tgbot.New(c.Store, c.Audit, c.Keyring, tgbot.Options{BaseURL: c.Cfg.Telegram.APIBaseURL, PublicURL: c.Cfg.PublicURL,
		Updates: c.TelegramUpdates, Metrics: c.Metrics, Bus: c.Bus, Log: obs.Component(c.Log, "telegram")})
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
