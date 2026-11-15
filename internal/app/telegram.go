package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/netip"

	"net/http"
	"net/url"

	"github.com/aurimasniekis/go-spaceman/internal/api"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/obs"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/chat"
	tglink "github.com/aurimasniekis/go-spaceman/internal/telegram/link"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/notify"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/sender"
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
	c.TelegramConfirmations = api.NewApprovalConfirmations(c.Clock.Now)
	if err := c.TelegramSettings.Load(ctx); err != nil {
		log.Warn("telegram: loading the settings failed; using the defaults until they load", slog.String("error", err.Error()))
	}
	c.TelegramLink = c.newTelegramLink()
	c.TelegramLimiter = updates.NewLimiter(updates.InboundLimits{}, c.Clock)
	c.TelegramLinks = tglink.NewCache(c.Store, c.Bus, log, 0)
	c.TelegramLinks.OnRemove = func(id string) {
		c.TelegramLimiter.Forget(id)
		c.TelegramTopics.Forget(id) // nil-safe: topics are built with the notifier, later
	}
	c.TelegramUpdates = updates.New(updates.Options{
		Intake: updates.IntakeOptions{Metrics: c.Metrics, Log: log},
		Webhook: updates.WebhookOptions{TrustedProxies: c.trustedProxies(), Clock: c.Clock, Log: log,
			OnUnauthorized: c.auditTelegramUnauthorized},
		Dispatcher: updates.DispatcherOptions{Clock: c.Clock, Metrics: c.Metrics, Log: log, Limiter: c.TelegramLimiter, Links: c.TelegramLinks, Linker: c.TelegramLink, Unlinker: c.TelegramLink,
			Groups: c.TelegramSettings},
	})
	// Binding a group needs a bot that is a member of it: getChat only finds chats the bot is in (TG-13-2).
	c.TelegramSettings.SetGroupVerifier(func(ctx context.Context, chatID int64) (botapi.Chat, error) {
		b, ok := c.TelegramUpdates.Dispatcher.Bot().(interface {
			GetChat(context.Context, int64) (botapi.Chat, error)
		})
		if !ok || b == nil {
			return botapi.Chat{}, errors.New("the Telegram bot is not running")
		}
		return b.GetChat(ctx, chatID)
	})
	// A group that lost its binding (unbound, or its space disabled, hidden or removed) is left, so it isn't a member the
	// operator forgot about. Leaving runs apart from the reload that noticed.
	c.TelegramSettings.OnGroupLost(func(chatID int64) {
		go c.TelegramUpdates.Dispatcher.LeaveGroup(context.WithoutCancel(ctx), chatID)
	})
	// link_sso_grants (ADR-0083 Q8) is read per use, so a change applies to the next principal without a restart.
	c.TelegramSSOGrants = func() bool { return c.TelegramSettings.Current().LinkSSOGrants }
	c.TelegramSettings.OnChange(func(s tgsettings.Settings) {
		c.TelegramUpdates.Intake.SetRate(float64(s.InboundLimits.UpdatesPerSecond))
		c.TelegramLimiter.SetLimits(s.InboundLimits.Updates())
	})
}

// newTelegramBot builds the bot's connection service (ADR-0083). It configures TelegramUpdates (webhook route,
// dispatcher client, poller) whenever the bot is written and at start (Serve calls Start).
func (c *Central) newTelegramBot() *tgbot.Service {
	return tgbot.New(c.Store, c.Audit, c.Keyring, tgbot.Options{BaseURL: c.Cfg.Telegram.APIBaseURL, PublicURL: c.Cfg.PublicURL,
		WebhookBaseURL: c.Cfg.Telegram.WebhookBaseURL, MiniAppHost: c.Cfg.Telegram.MiniAppHost, Updates: c.TelegramUpdates, Metrics: c.Metrics, Bus: c.Bus, Log: obs.Component(c.Log, "telegram")})
}

// newTelegramLink builds the linking protocol (ADR-0083 decision 2); newTelegram hands the dispatcher its bot-side
// steps. The
// "Linked" message goes through the live bot's outbound sender; without a running bot it is skipped (the web page
// already shows the link).
func (c *Central) newTelegramLink() *tglink.Service {
	log := obs.Component(c.Log, "telegram")
	svc := tglink.New(tglink.Options{Store: c.Store, Auth: c.Auth, Audit: c.Audit, Settings: c.TelegramSettings.Current,
		TopicSpaces: c.topicSpaces, TopicsAvailable: func() bool { return c.TelegramBot.Sender() != nil }, TopicsRefused: func(chatID int64) bool { return c.TelegramTopics.Refused(chatID) },
		CanRead:   func(subj []string, id string) bool { return c.Authz.AllowedAll(subj, id, authz.SpaceRead) },
		PublicURL: c.Cfg.PublicURL, Bus: c.Bus, Clock: c.Clock, Log: log, Notify: func(chatID int64, text string) {
			snd := c.TelegramBot.Sender()
			if snd == nil {
				return
			}
			err := snd.Enqueue(sender.Message{ChatID: chatID, Priority: sender.Interactive, Send: func(ctx context.Context, cl *botapi.Client) error {
				_, err := cl.SendMessage(ctx, botapi.SendMessageParams{ChatID: chatID, Text: text, LinkPreviewOptions: &botapi.LinkPreviewOptions{IsDisabled: true}})
				return err
			}})
			if err != nil {
				log.Debug("telegram: the linked notice was not queued", slog.String("error", err.Error()))
			}
		}})
	return svc
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

// wireTelegramChat gives the dispatcher the chat surfaces (/spaces, /space, /inbox). They call the API through its
// in-process handler as the link's Telegram principal, built for every update, so each read and write passes the same
// authorization as over HTTP. A space is shown only when its Telegram settings make it visible.
func (c *Central) wireTelegramChat(apiH http.Handler) {
	if c.TelegramUpdates == nil || c.TelegramSettings == nil {
		return
	}
	d := c.TelegramUpdates.Dispatcher
	d.SetHandler(chat.New(chat.Options{
		API: apiH,
		Principal: func(ctx context.Context, linkID string) (*auth.Principal, error) {
			return c.Auth.TelegramPrincipal(ctx, linkID, auth.TelegramOptions{SSOGrants: c.telegramSSOGrants()})
		},
		Space: c.TelegramSettings.Space,
		// A message written in a space's topic acts on that space (the topics the bot made, never user input).
		SpaceOfThread: func(ctx context.Context, linkID string, thread int) (string, bool) {
			return c.TelegramTopics.SpaceOf(ctx, linkID, thread)
		},
		// A notifier card's button names no action: it comes from the card mapping, only for the link the card went to.
		Resolve: func(ctx context.Context, chatID int64, messageID int) (string, string, string, bool) {
			m, err := notify.StoreMappings{S: c.Store}.Card(ctx, chatID, messageID)
			if err != nil || m.Key.Type != notify.TypeHumanAction {
				return "", "", "", false
			}
			return m.SpaceID, m.Key.ID, m.LinkID, true
		},
		// A notifier card of a message to the operators: a Telegram reply to it is a reply to that message (TG-8b).
		ResolveMessage: func(ctx context.Context, chatID int64, messageID int) (string, string, string, bool) {
			m, err := notify.StoreMappings{S: c.Store}.Card(ctx, chatID, messageID)
			if err != nil || m.Key.Type != notify.TypeMessage {
				return "", "", "", false
			}
			return m.SpaceID, m.Key.ID, m.LinkID, true
		},
		// Approvals and forms too complex for the chat open in the Mini App; without its host there is no button.
		AppURL: func(spaceID, actionID string) string {
			o := c.miniAppOrigin()
			if o == "" {
				return ""
			}
			// The shell's route for one action (the web's /spaces/$spaceId/human?action=).
			return o + "/spaces/" + url.PathEscape(spaceID) + "/human?action=" + url.QueryEscape(actionID)
		},
		// A page of the Mini App (the /settings command); without its host there is none.
		AppPage: func(path string) string {
			if o := c.miniAppOrigin(); o != "" {
				return o + path
			}
			return ""
		},
		// Telegram refuses web_app buttons outside private chats: a group's forms link to the Mini App with its direct
		// link, t.me/<bot>?startapp=human-<space>-<action> (the web's deep link, web/src/tg/deeplink.ts). It needs the
		// bot's Main Mini App (BotFather) and the Mini App host, like the button.
		AppLink: func(spaceID, actionID string) string {
			if c.miniAppOrigin() == "" || c.TelegramBot == nil || c.TelegramBot.Username() == "" {
				return ""
			}
			return "https://t.me/" + c.TelegramBot.Username() + "?startapp=" + url.QueryEscape("human-"+spaceID+"-"+actionID)
		},
		// The first step of deciding an enforced approval in chat arms the respond rule's confirmation (ADR-0083 decision 14).
		ArmApproval: c.TelegramConfirmations.Arm,
		Allow:       c.TelegramLimiter.Allow,
		Content: func(ctx context.Context, spaceID, attachmentID string) (io.ReadSeekCloser, error) {
			_, rc, err := c.Attachments.Open(ctx, spaceID, attachmentID)
			return rc, err
		},
		BotUsername: c.TelegramBot.Username,
		Bot: func() chat.Bot {
			b, _ := d.Bot().(chat.Bot)
			return b
		},
		Clock: c.Clock,
		Log:   obs.Component(c.Log, "telegram"),
	}))
}

// topicSpaces lists the spaces whose Telegram settings turn topics mode on and that Telegram may see, sorted.
func (c *Central) topicSpaces() []string {
	out := []string{}
	for _, id := range c.TelegramSettings.VisibleSpaces() {
		if c.TelegramSettings.Space(id).Topics {
			out = append(out, id)
		}
	}
	return out
}
