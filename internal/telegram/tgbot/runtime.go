package tgbot

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/keyring"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/updates"
)

// Live states of the bot (Status.WebhookStatus).
const (
	StateOff      = "off"      // no bot, or not enabled
	StateStarting = "starting" // being configured, or polling waits for central to start serving
	StatePolling  = "polling"  // getUpdates running
	StateOK       = "ok"       // webhook registered and Telegram reports no newer delivery error
	StateError    = "error"    // startup failed, or Telegram reports a delivery error since the webhook was set
	StateUnknown  = "unknown"  // webhook registered, but Telegram couldn't be asked about it
)

// Webhook registration (plan §3.5).
const (
	webhookMaxConnections = 10
	// statusTTL is how long one getWebhookInfo answer is reused.
	statusTTL = 30 * time.Second
	// statusTimeout bounds getWebhookInfo for a status read (a page load waits for it).
	statusTimeout = 5 * time.Second
	// Startup retries after a failure that isn't the token's fault (Telegram unreachable, 5xx).
	retryMin = 30 * time.Second
	retryMax = 10 * time.Minute
)

// Commands is the bot's command menu in private chats (plan §2.0).
var Commands = []botapi.BotCommand{
	{Command: "needs", Description: "What needs you"},
	{Command: "inbox", Description: "Inbox of the current space"},
	{Command: "new", Description: "Create an inbox item"},
	{Command: "queue", Description: "Queue of the current space"},
	{Command: "runs", Description: "Runs of the current space"},
	{Command: "spaces", Description: "Your spaces"},
	{Command: "space", Description: "Switch the current space"},
	{Command: "digest", Description: "Your digest now"},
	{Command: "settings", Description: "Your Telegram settings"},
	{Command: "help", Description: "What this bot can do"},
	{Command: "unlink", Description: "Unlink this Telegram account"},
}

// Status is the bot's live health, for the System page and get-telegram-bot. It carries no token, secret or webhook
// path.
type Status struct {
	Configured    bool   `json:"configured" doc:"A bot is configured"`
	Enabled       bool   `json:"enabled"`
	Mode          string `json:"mode,omitempty" enum:"webhook,polling,"`
	BotID         int64  `json:"bot_id,omitempty"`
	Username      string `json:"username,omitempty" doc:"The bot's username, without @"`
	WebhookStatus string `json:"webhook_status" enum:"off,starting,polling,ok,error,unknown" doc:"off: no bot or disabled; starting: being configured; polling: getUpdates runs; ok: the webhook is registered and Telegram reports no delivery error since; error: see error; unknown: Telegram couldn't be asked"`
	Error         string `json:"error,omitempty" doc:"Why the bot isn't working (startup, or Telegram's last delivery error)"`
	// From getWebhookInfo (webhook mode).
	PendingUpdateCount int        `json:"pending_update_count" doc:"Updates Telegram holds for the webhook"`
	LastErrorAt        *time.Time `json:"last_error_at,omitempty" doc:"Telegram's last failed webhook delivery"`
	LastErrorMessage   string     `json:"last_error_message,omitempty"`
	CheckedAt          *time.Time `json:"checked_at,omitempty" doc:"When Telegram was last asked (getWebhookInfo)"`
}

// runtime is the live bot. Fields above mu are owned by Service.mu; the rest are read by Status under mu.
type runtime struct {
	baseCtx    context.Context // central's serving context (set by Start)
	client     *botapi.Client
	pollCancel context.CancelFunc
	pollDone   chan struct{}

	refreshMu sync.Mutex // one getWebhookInfo at a time

	mu         sync.Mutex
	gen        uint64 // incremented by every reconfiguration
	configured bool
	enabled    bool
	mode       string
	botID      int64
	username   string
	state      string
	err        string
	webhookURL string
	setAt      time.Time
	info       *botapi.WebhookInfo
	infoErr    bool
	infoAt     time.Time
	infoClient *botapi.Client
}

// MiniAppHost is the Mini App host of the running bot, or "" when the bot is off or has none. It is read per request.
func (svc *Service) MiniAppHost() string { return *svc.miniAppHost.Load() }

// Start brings the configured bot up (getMe, commands, menu button, then setWebhook or polling) and keeps ctx for the
// poller. It returns once the first attempt is done; an attempt that fails for a reason other than the token is
// retried in the background with a backoff until it works, a write reconfigures the bot, or ctx ends.
func (svc *Service) Start(ctx context.Context) {
	svc.mu.Lock()
	svc.run.baseCtx = ctx
	svc.mu.Unlock()
	retry, gen := svc.startOnce(ctx, 0)
	if !retry {
		return
	}
	go func() {
		wait := retryMin
		for retry {
			t := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
			}
			retry, gen = svc.startOnce(ctx, gen)
			wait = min(wait*2, retryMax)
		}
	}()
}

// startOnce applies the stored configuration and reports whether it should be retried. With after set, it does
// nothing when anything reconfigured the bot since that attempt (a write already applied the newest state).
func (svc *Service) startOnce(ctx context.Context, after uint64) (retry bool, gen uint64) {
	svc.mu.Lock()
	defer svc.mu.Unlock()
	svc.run.mu.Lock()
	cur := svc.run.gen
	svc.run.mu.Unlock()
	if after != 0 && cur != after {
		return false, cur
	}
	retry = svc.startLocked(ctx)
	svc.run.mu.Lock()
	defer svc.run.mu.Unlock()
	return retry, svc.run.gen
}

func (svc *Service) startLocked(ctx context.Context) (retry bool) {
	r, err := svc.s.R().GetTelegramBot(ctx)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		svc.applyLocked(ctx, nil, false)
		return false
	case err != nil:
		svc.setState(func(rt *runtime) { rt.state, rt.err = StateError, "the bot's configuration could not be read" })
		svc.o.Log.Warn("telegram: reading the bot configuration failed", slog.String("error", err.Error()))
		return ctx.Err() == nil
	}
	return svc.applyLocked(ctx, &r, false) && ctx.Err() == nil
}

func (svc *Service) setState(f func(rt *runtime)) {
	svc.run.mu.Lock()
	defer svc.run.mu.Unlock()
	f(&svc.run)
}

func (svc *Service) stopPollerLocked() {
	if svc.run.pollCancel != nil {
		svc.run.pollCancel()
		<-svc.run.pollDone
		svc.run.pollCancel, svc.run.pollDone = nil, nil
	}
}

// offLocked stops everything that runs: no webhook route, no client, no poller.
func (svc *Service) offLocked() {
	svc.stopPollerLocked()
	if u := svc.o.Updates; u != nil {
		_ = u.Webhook.Set(nil)
		u.Dispatcher.SetBot(nil)
	}
	svc.run.client = nil
	empty := ""
	svc.miniAppHost.Store(&empty)
}

// applyLocked makes the live bot match row (nil: no bot). drop drops the updates Telegram holds (a new token). It
// returns whether a failure is worth retrying (not the token's fault). Must hold svc.mu; never inside a transaction.
//
// The poller runs on central's serving context (Start), not ctx: it outlives the write that configured it.
func (svc *Service) applyLocked(ctx context.Context, row *db.TelegramBot, drop bool) (retry bool) { //nolint:contextcheck // see above
	var gen uint64
	svc.setState(func(rt *runtime) { rt.gen++; gen = rt.gen })
	prev := svc.run.client
	if row == nil || row.Enabled != 1 {
		svc.offLocked()
		if prev != nil {
			// Telegram must stop calling a webhook nobody serves any more.
			cctx, cancel := svc.callCtx(ctx)
			if err := prev.DeleteWebhook(cctx, false); err != nil {
				svc.o.Log.Warn("telegram: removing the webhook failed", slog.String("error", err.Error()))
			}
			cancel()
		}
		svc.setState(func(rt *runtime) {
			rt.configured, rt.enabled, rt.mode, rt.botID, rt.username = row != nil, false, "", 0, ""
			if row != nil {
				rt.mode, rt.botID, rt.username = row.Mode, row.BotID, row.Username
			}
			rt.state, rt.err, rt.webhookURL, rt.info, rt.infoErr, rt.infoAt, rt.setAt, rt.infoClient = StateOff, "", "", nil, false, time.Time{}, time.Time{}, nil
		})
		return false
	}
	fail := func(msg string, err error, retry bool) bool {
		if err != nil {
			svc.o.Log.Warn("telegram: starting the bot failed", slog.String("step", msg), slog.String("error", err.Error()))
			msg += ": " + err.Error()
		}
		svc.setState(func(rt *runtime) { rt.state, rt.err = StateError, msg })
		return retry
	}
	svc.setState(func(rt *runtime) {
		rt.configured, rt.enabled, rt.mode, rt.botID, rt.username = true, true, row.Mode, row.BotID, row.Username
		rt.state, rt.err, rt.webhookURL, rt.info, rt.infoErr, rt.infoAt, rt.setAt = StateStarting, "", "", nil, false, time.Time{}, time.Time{}
	})

	token, err := svc.kr.Open(keyring.PurposeCredential, tokenAAD, row.TokenEnc)
	if err != nil {
		svc.offLocked()
		return fail("the stored bot token can't be opened (keyring)", nil, false)
	}
	secret, err := svc.kr.Open(keyring.PurposeCredential, secretAAD, row.WebhookSecretEnc)
	if err != nil {
		svc.offLocked()
		return fail("the stored webhook secret can't be opened (keyring)", nil, false)
	}
	c, err := svc.client(string(token))
	if err != nil {
		svc.offLocked()
		return fail("the stored bot token is malformed", nil, false)
	}
	if prev != nil && prev.BotID() != c.BotID() {
		// Another bot's webhook would keep pointing here with a secret nobody accepts.
		cctx, cancel := svc.callCtx(ctx)
		_ = prev.DeleteWebhook(cctx, false)
		cancel()
	}
	svc.stopPollerLocked()
	svc.run.client = c
	host := row.MiniappHost
	svc.miniAppHost.Store(&host)
	svc.setState(func(rt *runtime) { rt.infoClient = c })

	var allowed []netip.Prefix
	var cidrs []string
	_ = json.Unmarshal([]byte(row.WebhookIpAllowlistJson), &cidrs)
	for _, s := range cidrs {
		if p, err := netip.ParsePrefix(s); err == nil {
			allowed = append(allowed, p)
		}
	}
	// Local first, so the first delivery after setWebhook already passes the checks.
	if u := svc.o.Updates; u != nil {
		u.Dispatcher.SetBot(c)
		if row.Mode == ModeWebhook {
			if err := u.Webhook.Set(&updates.WebhookConfig{HookID: row.HookID, Secret: string(secret), AllowedIPs: allowed}); err != nil {
				return fail("the webhook configuration is invalid", err, false)
			}
		} else {
			_ = u.Webhook.Set(nil)
		}
	}

	call := func(f func(context.Context) error) error {
		cctx, cancel := svc.callCtx(ctx)
		defer cancel()
		return f(cctx)
	}
	retryable := func(err error) bool { return !botapi.IsUnauthorized(err) }
	var me botapi.User
	if err := call(func(ctx context.Context) (err error) { me, err = c.GetMe(ctx); return err }); err != nil {
		return fail("getMe", err, retryable(err))
	}
	if me.ID != row.BotID {
		return fail("the token's bot ("+strconv.FormatInt(me.ID, 10)+") is not the configured bot ("+strconv.FormatInt(row.BotID, 10)+")", nil, false)
	}
	svc.setState(func(rt *runtime) { rt.username = me.Username })
	if err := call(func(ctx context.Context) error {
		return c.SetMyCommands(ctx, botapi.SetMyCommandsParams{Commands: Commands, Scope: &botapi.BotCommandScope{Type: "all_private_chats"}})
	}); err != nil {
		return fail("setMyCommands", err, retryable(err))
	}
	menu := &botapi.MenuButton{Type: botapi.MenuButtonCommands}
	if host != "" {
		menu = &botapi.MenuButton{Type: botapi.MenuButtonWebApp, Text: "SpaceMan", WebApp: &botapi.WebAppInfo{URL: "https://" + host + "/"}}
	}
	if err := call(func(ctx context.Context) error {
		return c.SetChatMenuButton(ctx, botapi.SetChatMenuButtonParams{MenuButton: menu})
	}); err != nil {
		return fail("setChatMenuButton", err, retryable(err))
	}

	if row.Mode == ModePolling {
		// Telegram refuses getUpdates while a webhook is set.
		if err := call(func(ctx context.Context) error { return c.DeleteWebhook(ctx, drop) }); err != nil {
			return fail("deleteWebhook", err, retryable(err))
		}
		base := svc.run.baseCtx
		if base == nil || svc.o.Updates == nil {
			svc.setState(func(rt *runtime) { rt.state = StateStarting })
			return false // Start begins polling
		}
		pctx, cancel := context.WithCancel(base)
		done := make(chan struct{})
		svc.run.pollCancel, svc.run.pollDone = cancel, done
		go func() {
			defer close(done)
			err := svc.o.Updates.Poll(pctx, c, svc.o.PollerOptions)
			if err != nil && pctx.Err() == nil {
				svc.setState(func(rt *runtime) {
					if rt.gen == gen {
						rt.state, rt.err = StateError, "polling stopped: "+err.Error()
					}
				})
			}
		}()
		svc.setState(func(rt *runtime) { rt.state = StatePolling })
		return false
	}

	url := svc.o.PublicURL + updates.WebhookPath + row.HookID
	if err := call(func(ctx context.Context) error {
		return c.SetWebhook(ctx, botapi.SetWebhookParams{URL: url, SecretToken: string(secret), MaxConnections: webhookMaxConnections,
			AllowedUpdates: updates.AllowedUpdates, DropPendingUpdates: drop})
	}); err != nil {
		return fail("setWebhook", err, retryable(err))
	}
	now := svc.s.Now()
	svc.setState(func(rt *runtime) { rt.state, rt.webhookURL, rt.setAt = StateOK, url, now })
	return false
}

// Status is the bot's live health. In webhook mode it asks Telegram (getWebhookInfo, reused for 30 s): a webhook
// registered elsewhere, or a delivery error newer than central's setWebhook, is an error.
func (svc *Service) Status(ctx context.Context) Status {
	svc.refreshInfo(ctx)
	rt := &svc.run
	rt.mu.Lock()
	defer rt.mu.Unlock()
	st := Status{Configured: rt.configured, Enabled: rt.enabled, Mode: rt.mode, BotID: rt.botID, Username: rt.username,
		WebhookStatus: rt.state, Error: rt.err}
	if rt.state != StateOK {
		return st
	}
	if !rt.infoAt.IsZero() {
		at := rt.infoAt
		st.CheckedAt = &at
	}
	switch {
	case rt.infoErr || rt.info == nil:
		st.WebhookStatus = StateUnknown
	case rt.info.URL != rt.webhookURL:
		st.WebhookStatus, st.Error = StateError, "Telegram has another webhook URL for this bot than central registered (another program may use the token)"
	default:
		st.PendingUpdateCount = rt.info.PendingUpdateCount
		if rt.info.LastErrorDate > 0 {
			at := time.Unix(rt.info.LastErrorDate, 0).UTC()
			st.LastErrorAt, st.LastErrorMessage = &at, rt.info.LastErrorMessage
			// Telegram dates have whole seconds: an error in the second of the setWebhook counts (the safe side).
			if !at.Before(rt.setAt.Truncate(time.Second)) {
				st.WebhookStatus, st.Error = StateError, "Telegram can't deliver to the webhook: "+rt.info.LastErrorMessage
			}
		}
	}
	return st
}

// refreshInfo asks Telegram for the webhook's state when the last answer is older than statusTTL.
func (svc *Service) refreshInfo(ctx context.Context) {
	rt := &svc.run
	rt.refreshMu.Lock()
	defer rt.refreshMu.Unlock()
	rt.mu.Lock()
	c, fresh := rt.infoClient, rt.state == StateOK && !rt.infoAt.IsZero() && svc.s.Now().Sub(rt.infoAt) < statusTTL
	need := rt.state == StateOK && c != nil && !fresh
	rt.mu.Unlock()
	if !need {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, statusTimeout)
	info, err := c.GetWebhookInfo(cctx)
	cancel()
	if err != nil {
		svc.o.Log.Warn("telegram: getWebhookInfo failed", slog.String("error", err.Error()))
	}
	now := svc.s.Now()
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.infoClient != c {
		return // reconfigured meanwhile
	}
	rt.infoAt, rt.infoErr = now, err != nil
	if err == nil {
		rt.info = &info
	}
}
