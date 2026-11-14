// Package tgbot keeps the Telegram bot's connection (ADR-0083, plan §3.6): the singleton telegram_bot row behind the
// get/put/delete-telegram-bot operations, and the live bot central runs from it (startup, mode switches, health).
//
// The bot token is write-only, like the Harbor Pilot parent token: it is sealed with the keyring's credential purpose,
// bound to its row, and never returned, logged or audited. Reads show only has_token, token_version, the bot's ID and
// username. The webhook's secret_token is generated here and rotates with every token set.
//
// A link belongs to one bot: rotating the same bot's token keeps every link, configuring a different bot (another ID
// from getMe) revokes all links in the same transaction, because their private chats belong to the old bot.
package tgbot

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/eventbus"
	"github.com/aurimasniekis/go-spaceman/internal/keyring"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/updates"
)

// Modes (plan §4.1).
const (
	ModeWebhook = "webhook"
	ModePolling = "polling"
)

// MaxAllowlist bounds webhook_ip_allowlist (Telegram publishes two ranges).
const MaxAllowlist = 32

// Associated data of the sealed values: a sealed value opens only for the column it was written for.
var (
	tokenAAD  = keyring.FieldAAD("telegram_bot", "token_enc", "1")
	secretAAD = keyring.FieldAAD("telegram_bot", "webhook_secret_enc", "1")
)

// Bot is the bot's connection as the API shows it: never the token or the webhook secret.
type Bot struct {
	Enabled            bool      `json:"enabled" doc:"Central runs the bot (webhook or polling); false: Telegram is off and the webhook is removed"`
	Mode               string    `json:"mode" enum:"webhook,polling" doc:"How updates arrive: webhook (public HTTPS) or polling (getUpdates, outbound only)"`
	MiniAppHost        string    `json:"miniapp_host" doc:"The Mini App's own host (e.g. tg.central.example.com); empty: the Mini App is off"`
	WebhookIPAllowlist []string  `json:"webhook_ip_allowlist" doc:"CIDRs the webhook accepts requests from (after trusted proxies); empty: no source check"`
	HasToken           bool      `json:"has_token" doc:"A bot token is stored (it is never returned)"`
	TokenVersion       int64     `json:"token_version" doc:"Incremented each time a token is written"`
	BotID              int64     `json:"bot_id" doc:"The bot's Telegram user ID, from getMe"`
	Username           string    `json:"username" doc:"The bot's username (without @), from getMe"`
	WebhookStatus      string    `json:"webhook_status" enum:"off,starting,polling,ok,error,unknown" doc:"health.webhook_status"`
	Health             Status    `json:"health"`
	Warnings           []string  `json:"warnings"`
	CreatedAt          time.Time `json:"created_at"`
	CreatedBy          string    `json:"created_by"`
	UpdatedAt          time.Time `json:"updated_at"`
	UpdatedBy          string    `json:"updated_by"`
	Version            int64     `json:"version"`
}

// Input creates or replaces the bot's connection. Every field but the token is replaced as given; the token is
// write-only and an empty one keeps the stored token.
type Input struct {
	BotToken           string   `json:"bot_token,omitempty" maxLength:"256" writeOnly:"true" doc:"The token BotFather gave (123456:ABC…). Required on create; empty keeps the stored one. Checked with getMe before it is stored. Never returned"`
	Enabled            bool     `json:"enabled,omitempty" doc:"Run the bot (default false)"`
	Mode               string   `json:"mode,omitempty" enum:"webhook,polling," doc:"webhook (default; needs central's public_url to be https) or polling"`
	MiniAppHost        string   `json:"miniapp_host,omitempty" maxLength:"253" doc:"The Mini App's host: a lower-case DNS name, not an IP address, without scheme, port or path, and not the host of central's public_url. Empty: Mini App off"`
	WebhookIPAllowlist []string `json:"webhook_ip_allowlist,omitempty" maxItems:"32" doc:"CIDRs (e.g. 149.154.160.0/20) the webhook accepts requests from, in canonical form; empty: no check. Only safe when the client address behind the proxy is trustworthy"`
}

// Options configure the service.
type Options struct {
	// BaseURL is central's telegram.api_base_url (config file only, never the API: ADR-0083 decision 11).
	BaseURL string
	// PublicURL is central's public_url: the webhook is registered under it, and the Mini App host must differ from it.
	PublicURL string
	// HTTPClient sends Bot API requests (nil: a default client; every call is bounded by CallTimeout).
	HTTPClient *http.Client
	// CallTimeout bounds each Bot API call (default 15 s).
	CallTimeout time.Duration
	// Updates is the inbound side the live bot configures (webhook, dispatcher's client, poller). Nil: nothing runs.
	Updates *updates.Service
	// PollerOptions configure polling mode (tests shorten the wait).
	PollerOptions updates.PollerOptions
	Bus           *eventbus.Bus
	Log           *slog.Logger
}

// Service keeps the bot row and runs the live bot.
type Service struct {
	s     *store.Store
	audit *audit.Writer
	kr    *keyring.Keyring
	o     Options

	// mu serializes Put, Delete and the live reconfiguration, so the live bot follows the database's order of writes.
	mu  sync.Mutex
	run runtime

	// miniAppHost is the Mini App host of a running bot ("" when off): central's host routing reads it per request.
	miniAppHost atomic.Pointer[string]
}

// New returns the service. Call Start once central serves, to bring the configured bot up.
func New(s *store.Store, aw *audit.Writer, kr *keyring.Keyring, o Options) *Service {
	if o.CallTimeout <= 0 {
		o.CallTimeout = 15 * time.Second
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.Bus == nil {
		o.Bus = eventbus.New()
	}
	o.PublicURL = strings.TrimRight(o.PublicURL, "/")
	svc := &Service{s: s, audit: aw, kr: kr, o: o}
	empty := ""
	svc.miniAppHost.Store(&empty)
	svc.run.state = StateOff
	return svc
}

var errNoBot = apperr.New(apperr.CodeNotFound, "no Telegram bot is configured")

func actorRef(a audit.Actor) string {
	if a.Type == audit.ActorRoot {
		return "root"
	}
	return a.Type + ":" + a.ID
}

func b2i(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// Get returns the bot's connection with its live health (not found when none is configured).
func (svc *Service) Get(ctx context.Context) (*Bot, error) {
	r, err := svc.s.R().GetTelegramBot(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNoBot
	} else if err != nil {
		return nil, err
	}
	return svc.out(ctx, r), nil
}

func (svc *Service) out(ctx context.Context, r db.TelegramBot) *Bot {
	b := &Bot{Enabled: r.Enabled == 1, Mode: r.Mode, MiniAppHost: r.MiniappHost, WebhookIPAllowlist: []string{}, HasToken: len(r.TokenEnc) > 0,
		TokenVersion: r.TokenVersion, BotID: r.BotID, Username: r.Username, Warnings: []string{},
		CreatedAt: time.UnixMicro(r.CreatedAt).UTC(), CreatedBy: r.CreatedBy, UpdatedAt: time.UnixMicro(r.UpdatedAt).UTC(), UpdatedBy: r.UpdatedBy, Version: r.Version}
	_ = json.Unmarshal([]byte(r.WebhookIpAllowlistJson), &b.WebhookIPAllowlist)
	b.Health = svc.Status(ctx)
	b.WebhookStatus = b.Health.WebhookStatus
	if b.Enabled && b.Mode == ModeWebhook && len(b.WebhookIPAllowlist) == 0 {
		b.Warnings = append(b.Warnings, "No webhook source allowlist: anyone who learns the webhook path still needs the secret token, but requests "+
			"aren't limited to Telegram's ranges (149.154.160.0/20, 91.108.4.0/22). Set one only when the client address behind the proxy is trustworthy.")
	}
	if b.Health.Error != "" {
		b.Warnings = append(b.Warnings, b.Health.Error)
	}
	return b
}

// reHostLabel is one DNS label in lower case.
var reHostLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// checkMiniAppHost validates miniapp_host (plan §3.4.1): a bare lower-case DNS name with at least two labels, no
// scheme, port, path or trailing dot, not an IP address, and not the host of central's public URL (the Mini App needs
// its own origin, so no cookie of the main host reaches it).
func checkMiniAppHost(v *apperr.Validation, host, publicURL string) {
	if host == "" {
		return
	}
	if _, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		v.Add("miniapp_host", "must be a DNS name, not an IP address")
		return
	}
	if strings.ContainsAny(host, ":/?#@ ") {
		v.Add("miniapp_host", "must be a bare host name, without scheme, port or path (e.g. tg.central.example.com)")
		return
	}
	if host != strings.ToLower(host) {
		v.Add("miniapp_host", "must be lower case")
		return
	}
	labels := strings.Split(host, ".")
	if len(host) > 253 || len(labels) < 2 {
		v.Add("miniapp_host", "must be a fully qualified DNS name of at most 253 characters (e.g. tg.central.example.com)")
		return
	}
	for _, l := range labels {
		if !reHostLabel.MatchString(l) {
			v.Add("miniapp_host", "is not a valid DNS name: each label is 1-63 of a-z, 0-9 and '-', not starting or ending with '-'")
			return
		}
	}
	if u, err := url.Parse(publicURL); err == nil && strings.EqualFold(u.Hostname(), host) {
		v.Add("miniapp_host", "must differ from the host of central's public URL: the Mini App needs its own origin")
	}
}

// parseAllowlist validates webhook_ip_allowlist: canonical CIDRs without duplicates. It returns them sorted (the
// Terraform attribute is a set, so order carries no meaning).
func parseAllowlist(v *apperr.Validation, in []string) ([]netip.Prefix, []string) {
	if len(in) > MaxAllowlist {
		v.Add("webhook_ip_allowlist", "has more than %d entries", MaxAllowlist)
		return nil, nil
	}
	prefixes := make([]netip.Prefix, 0, len(in))
	seen := map[netip.Prefix]bool{}
	for _, s := range in {
		p, err := netip.ParsePrefix(s)
		switch {
		case err != nil:
			v.Add("webhook_ip_allowlist", "%q is not a CIDR (e.g. 149.154.160.0/20)", s)
			return nil, nil
		case p.Masked() != p || p.String() != s:
			v.Add("webhook_ip_allowlist", "%q is not in canonical form: use %q", s, p.Masked().String())
			return nil, nil
		case seen[p]:
			v.Add("webhook_ip_allowlist", "%q is listed twice", s)
			return nil, nil
		}
		seen[p] = true
		prefixes = append(prefixes, p)
	}
	slices.SortFunc(prefixes, func(a, b netip.Prefix) int {
		if c := a.Addr().Compare(b.Addr()); c != 0 {
			return c
		}
		return a.Bits() - b.Bits()
	})
	out := make([]string, len(prefixes))
	for i, p := range prefixes {
		out[i] = p.String()
	}
	return prefixes, out
}

// randomToken is n random bytes as unpadded base64url (the webhook secret's and the hook id's alphabet).
func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func (svc *Service) client(token string) (*botapi.Client, error) {
	return botapi.New(botapi.Options{BaseURL: svc.o.BaseURL, Token: token, HTTPClient: svc.o.HTTPClient, Logger: svc.o.Log})
}

func (svc *Service) callCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, svc.o.CallTimeout)
}

// checkToken asks Telegram whom the token belongs to (getMe). The error never contains the token.
func (svc *Service) checkToken(ctx context.Context, token string) (botapi.User, error) {
	c, err := svc.client(token)
	if err != nil {
		return botapi.User{}, err
	}
	cctx, cancel := svc.callCtx(ctx)
	defer cancel()
	me, err := c.GetMe(cctx)
	switch {
	case botapi.IsUnauthorized(err):
		return me, fieldErr("bot_token", "Telegram refused the token (getMe: 401): check it in BotFather")
	case err != nil:
		return me, apperr.Wrap(apperr.CodeUnavailable, err, "could not check the bot token with Telegram")
	case !me.IsBot || me.ID == 0 || c.BotID() != itoa(me.ID):
		return me, fieldErr("bot_token", "getMe did not return the token's bot")
	}
	return me, nil
}

// Put creates (version 0) or replaces (the current version) the bot's connection. A new token is checked with getMe
// first; a different bot than before revokes every link. The live bot is then reconfigured; a failure there doesn't
// fail the write and shows in the health.
func (svc *Service) Put(ctx context.Context, actor audit.Actor, version int64, in Input) (*Bot, error) {
	if svc.kr == nil {
		return nil, apperr.New(apperr.CodeUnavailable, "the Telegram bot needs the keyring")
	}
	svc.mu.Lock()
	defer svc.mu.Unlock()
	cur, err := svc.s.R().GetTelegramBot(ctx)
	exists := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err := checkVersion(exists, cur.Version, version); err != nil {
		return nil, err
	}

	v := &apperr.Validation{}
	if in.Mode == "" {
		in.Mode = ModeWebhook
	}
	if in.Mode != ModeWebhook && in.Mode != ModePolling {
		v.Add("mode", "must be webhook or polling")
	}
	if in.Enabled && in.Mode == ModeWebhook && !strings.HasPrefix(svc.o.PublicURL, "https://") {
		v.Add("mode", "webhook mode needs central's public_url to be https (Telegram only calls HTTPS webhooks); use polling")
	}
	checkMiniAppHost(v, in.MiniAppHost, svc.o.PublicURL)
	_, allow := parseAllowlist(v, in.WebhookIPAllowlist)
	switch {
	case in.BotToken == "" && !exists:
		v.Add("bot_token", "is required")
	case in.BotToken != "" && !botapi.ValidToken(in.BotToken):
		v.Add("bot_token", "is not a bot token (123456:ABC…)")
	}
	if err := v.Err(); err != nil {
		return nil, err
	}
	allowJSON, _ := json.Marshal(allow)

	botID, username := cur.BotID, cur.Username
	tokenEnc, secretEnc, hookID := cur.TokenEnc, cur.WebhookSecretEnc, cur.HookID
	if in.BotToken != "" {
		me, err := svc.checkToken(ctx, in.BotToken)
		if err != nil {
			return nil, err
		}
		botID, username = me.ID, me.Username
		secret, err := randomToken(32)
		if err != nil {
			return nil, err
		}
		if tokenEnc, err = svc.kr.Seal(keyring.PurposeCredential, tokenAAD, []byte(in.BotToken)); err != nil {
			return nil, err
		}
		if secretEnc, err = svc.kr.Seal(keyring.PurposeCredential, secretAAD, []byte(secret)); err != nil {
			return nil, err
		}
	}
	if !exists {
		if hookID, err = randomToken(16); err != nil {
			return nil, err
		}
	}

	var revoked int64
	var out db.TelegramBot
	err = svc.s.W().TxNamed(ctx, "telegram.put_bot", func(tx *store.Tx) error {
		now := tx.NowMicros()
		row, err := tx.Q.GetTelegramBot(ctx)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if exists {
				return errNoBot
			}
		case err != nil:
			return err
		case !exists:
			return apperr.New(apperr.CodeConflict, "a Telegram bot was configured meanwhile; reload and retry")
		}
		action, tokenVersion := "created", int64(1)
		if exists {
			action, tokenVersion = "updated", row.TokenVersion
			if in.BotToken != "" {
				tokenVersion++
			}
			n, err := tx.Q.UpdateTelegramBot(ctx, db.UpdateTelegramBotParams{BotID: botID, Username: username, TokenEnc: tokenEnc, TokenVersion: tokenVersion,
				WebhookSecretEnc: secretEnc, Mode: in.Mode, Enabled: b2i(in.Enabled), MiniappHost: in.MiniAppHost, WebhookIpAllowlistJson: string(allowJSON),
				Now: now, UpdatedBy: actorRef(actor), Version: version})
			if err != nil {
				return err
			}
			if n == 0 {
				return apperr.New(apperr.CodeConflict, "the Telegram bot changed (version %d, you sent %d); reload and retry", row.Version, version)
			}
		} else if err := tx.Q.InsertTelegramBot(ctx, db.InsertTelegramBotParams{BotID: botID, Username: username, TokenEnc: tokenEnc, WebhookSecretEnc: secretEnc,
			HookID: hookID, Mode: in.Mode, Enabled: b2i(in.Enabled), MiniappHost: in.MiniAppHost, WebhookIpAllowlistJson: string(allowJSON),
			CreatedAt: now, CreatedBy: actorRef(actor), UpdatedAt: now, UpdatedBy: actorRef(actor)}); err != nil {
			if store.IsConstraint(err) {
				return apperr.New(apperr.CodeConflict, "a Telegram bot was configured meanwhile; reload and retry")
			}
			return err
		}
		// A link's private chat belongs to the bot it was made with: links of any other bot end here, in the same
		// transaction as the token that makes them meaningless (a deleted bot's links stay until then).
		if in.BotToken != "" {
			if revoked, err = tx.Q.RevokeTelegramLinksOfOtherBots(ctx, db.RevokeTelegramLinksOfOtherBotsParams{Now: now, RevokedBy: "telegram:bot_changed", BotID: botID}); err != nil {
				return err
			}
		}
		if revoked > 0 {
			tx.AfterCommit(func() { svc.o.Bus.Publish(eventbus.Event{Topic: eventbus.TopicTelegramLink, Key: "*"}) })
		}
		if out, err = tx.Q.GetTelegramBot(ctx); err != nil {
			return err
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "telegram.bot." + action, Target: audit.Target{Type: "telegram_bot", ID: itoa(botID)},
			Details: map[string]any{"enabled": in.Enabled, "mode": in.Mode, "miniapp_host": in.MiniAppHost, "webhook_ip_allowlist": allow,
				"token_changed": in.BotToken != "", "token_version": tokenVersion, "bot_id": botID, "username": username,
				"bot_changed": exists && in.BotToken != "" && row.BotID != botID, "links_revoked": revoked}})
		return err
	})
	if err != nil {
		return nil, err
	}
	// The live bot follows outside the transaction (network calls). A new token drops updates Telegram still holds
	// for the old configuration.
	svc.applyLocked(context.WithoutCancel(ctx), &out, in.BotToken != "")
	b := svc.out(ctx, out)
	if revoked > 0 {
		b.Warnings = append(b.Warnings, "A different bot was configured: "+itoa(revoked)+" Telegram links of the previous bot were revoked; their users link again with the new bot.")
	}
	return b, nil
}

func checkVersion(exists bool, cur, sent int64) error {
	switch {
	case !exists && sent != 0:
		return errNoBot
	case exists && sent == 0:
		return apperr.New(apperr.CodeConflict, "a Telegram bot is configured: replace it with If-Match \"%d\"", cur)
	case exists && sent != cur:
		return apperr.New(apperr.CodeConflict, "the Telegram bot changed (version %d, you sent %d); reload and retry", cur, sent)
	}
	return nil
}

// Delete removes the bot's connection (version-checked): the webhook is removed and Telegram is off. Links stay and
// resume if the same bot is configured again.
func (svc *Service) Delete(ctx context.Context, actor audit.Actor, version int64) error {
	svc.mu.Lock()
	defer svc.mu.Unlock()
	err := svc.s.W().TxNamed(ctx, "telegram.delete_bot", func(tx *store.Tx) error {
		cur, err := tx.Q.GetTelegramBot(ctx)
		if errors.Is(err, sql.ErrNoRows) {
			return errNoBot
		} else if err != nil {
			return err
		}
		if cur.Version != version {
			return apperr.New(apperr.CodeConflict, "the Telegram bot changed (version %d, you sent %d); reload and retry", cur.Version, version)
		}
		n, err := tx.Q.DeleteTelegramBot(ctx, version)
		if err != nil {
			return err
		}
		if n == 0 {
			return apperr.New(apperr.CodeConflict, "the Telegram bot changed; reload and retry")
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "telegram.bot.deleted", Target: audit.Target{Type: "telegram_bot", ID: itoa(cur.BotID)},
			Details: map[string]any{"bot_id": cur.BotID, "username": cur.Username, "mode": cur.Mode, "enabled": cur.Enabled == 1}})
		return err
	})
	if err != nil {
		return err
	}
	svc.applyLocked(context.WithoutCancel(ctx), nil, false)
	return nil
}

func fieldErr(field, msg string) error {
	v := &apperr.Validation{}
	v.Add(field, "%s", msg)
	return v.Err()
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}
