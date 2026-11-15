package tgbot

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/keyring"
	"github.com/aurimasniekis/go-spaceman/internal/obs"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/commands"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/miniapp"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/sender"
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

// Commands is the bot's command menu in private chats (plan §2.0): the commands package's table.
var Commands = commands.Menu()

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
	// The Mini App host (ADR-0083 §3.4.1).
	MiniAppHost         string `json:"miniapp_host,omitempty" doc:"The host central routes to the Mini App handler only (it never reaches the rest of central; 404 for everything while no bot serves it): telegram.miniapp_host from central's configuration when set, otherwise the bot's miniapp_host"`
	MiniAppHostSource   string `json:"miniapp_host_source,omitempty" enum:"config,bot," doc:"Where miniapp_host comes from: config (telegram.miniapp_host) or bot (the bot's miniapp_host)"`
	MiniAppHostIgnored  string `json:"miniapp_host_ignored,omitempty" doc:"The bot's miniapp_host when it differs from telegram.miniapp_host: the configuration wins, and this host answers 404 for everything"`
	MiniAppHostNeedsKey bool   `json:"miniapp_host_needs_key,omitempty" doc:"The bot stores a miniapp_host but central's configuration does not set telegram.miniapp_host: set it (or clear the bot's miniapp_host)"`
}

// runtime is the live bot. Fields above mu are owned by Service.mu; the rest are read by Status under mu.
type runtime struct {
	baseCtx    context.Context    // central's serving context (set by Start)
	retryStop  context.CancelFunc // ends the running startup retry loop (owned by Service.mu)
	retryDone  chan struct{}      // closed when that loop has returned (owned by Service.mu)
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
// It is telegram.miniapp_host when that is set, whatever the bot's miniapp_host says.
func (svc *Service) MiniAppHost() string { return *svc.miniAppHost.Load() }

// StoredMiniAppHost is the stored bot's miniapp_host, enabled or not, or "" when there is no bot or it has none.
// Central routes it to the Mini App handler whatever the bot's state, like telegram.miniapp_host; the handler serves
// only MiniAppHost. Before Start has loaded the bot it is "".
func (svc *Service) StoredMiniAppHost() string { return *svc.storedHost.Load() }

// RoutesToMiniApp reports whether host (already normalized, tghost.Of) is a host central routes to the Mini App handler
// because of the bot: the stored miniapp_host, or one it has been since central started (a retired host keeps answering
// 404 until a restart, so a delete or a change never lets it reach the main router).
func (svc *Service) RoutesToMiniApp(host string) bool {
	if host == "" {
		return false
	}
	if host == *svc.storedHost.Load() {
		return true
	}
	svc.retiredMu.Lock()
	defer svc.retiredMu.Unlock()
	_, ok := svc.retired[host]
	return ok
}

// setStored records the stored bot's miniapp_host (row nil: no bot) and retires the host it replaces. It returns the
// previous one.
func (svc *Service) setStored(row *db.TelegramBot) string {
	stored := ""
	if row != nil {
		stored = row.MiniappHost
	}
	// The retired set takes the old host before the new one is visible, so no request sees neither.
	if old := *svc.storedHost.Load(); old != "" && old != stored {
		svc.retiredMu.Lock()
		if svc.retired == nil {
			svc.retired = map[string]struct{}{}
		}
		svc.retired[old] = struct{}{}
		svc.retiredMu.Unlock()
	}
	return *svc.storedHost.Swap(&stored)
}

// hostNeedsKey is true when a bot stores a miniapp_host while telegram.miniapp_host is not set.
func (svc *Service) hostNeedsKey(stored string) bool { return stored != "" && svc.o.MiniAppHost == "" }

// SetLoadStoredHostHook installs a hook LoadStoredHost calls before it reads the bot row; the hook may block. It exists
// so a test can pin that central loads the host before its listener accepts a connection. Production code never calls it.
func (svc *Service) SetLoadStoredHostHook(f func(context.Context)) { svc.onLoadStoredHost = f }

// LoadStoredHost reads the stored bot row and makes its miniapp_host known to the router. Central calls it before it
// listens, so the stored host reaches the Mini App handler (and its 404 gate) from the first request, whatever the
// bot's state or how long the bot takes to start. A failed read is an error: central must not serve without knowing.
func (svc *Service) LoadStoredHost(ctx context.Context) error {
	if svc.onLoadStoredHost != nil {
		svc.onLoadStoredHost(ctx)
	}
	svc.mu.Lock()
	defer svc.mu.Unlock()
	r, err := svc.s.R().GetTelegramBot(ctx)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		svc.setStored(nil)
		return nil
	case err != nil:
		return fmt.Errorf("read the Telegram bot's stored miniapp_host: %w", err)
	}
	svc.setStored(&r)
	if svc.hostNeedsKey(r.MiniappHost) {
		svc.o.Log.Warn("telegram: the bot stores a miniapp_host but telegram.miniapp_host is not set; set it in central's configuration",
			slog.String("bot_miniapp_host", r.MiniappHost))
	}
	return nil
}

// effectiveHost is the Mini App host for a bot whose stored miniapp_host is stored: the configured one wins.
func (svc *Service) effectiveHost(stored string) string {
	if svc.o.MiniAppHost != "" {
		return svc.o.MiniAppHost
	}
	return stored
}

// hostMismatch is the stored miniapp_host when it differs from telegram.miniapp_host (both set), otherwise "".
func (svc *Service) hostMismatch(stored string) string {
	if svc.o.MiniAppHost != "" && stored != "" && stored != svc.o.MiniAppHost {
		return stored
	}
	return ""
}

// Username is the bot's username without "@" from the last getMe ("" while unknown). It makes no call.
func (svc *Service) Username() string {
	rt := &svc.run
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.username
}

// MiniAppBot is the running bot's ID with the key its Mini App initData is signed with (miniapp.SecretKey of its
// token; the token itself stays here), or false while no bot runs or it has no Mini App host.
func (svc *Service) MiniAppBot() (miniapp.Bot, bool) {
	b := svc.miniAppBot.Load()
	if b == nil || *svc.miniAppHost.Load() == "" {
		return miniapp.Bot{}, false
	}
	return *b, true
}

// Start brings the configured bot up (getMe, commands, menu button, then setWebhook or polling) and keeps ctx for the
// poller. It returns once the first attempt is done; an attempt that fails for a reason other than the token is
// retried in the background with a backoff until it works, a write reconfigures the bot, or ctx ends.
func (svc *Service) Start(ctx context.Context) {
	svc.mu.Lock()
	svc.run.baseCtx = ctx
	svc.mu.Unlock()
	retry, gen := svc.startOnce(ctx, 0)
	if retry {
		svc.mu.Lock()
		svc.startRetryLocked(ctx, gen)
		svc.mu.Unlock()
	}
}

// startRetryLocked ends the running retry loop, a superseded one that would otherwise sleep up to retryMax before it
// notices, and starts one for gen when gen is not 0. Must hold svc.mu.
func (svc *Service) startRetryLocked(base context.Context, gen uint64) {
	if gen != 0 {
		svc.run.mu.Lock()
		cur := svc.run.gen
		svc.run.mu.Unlock()
		if gen != cur {
			return // a write reconfigured the bot since the attempt gen and owns the retry decision
		}
	}
	if svc.run.retryStop != nil {
		svc.run.retryStop()
		svc.run.retryStop = nil
	}
	if gen == 0 {
		return
	}
	ctx, stop := context.WithCancel(base)
	done := make(chan struct{})
	svc.run.retryStop, svc.run.retryDone = stop, done
	go func() {
		defer close(done)
		svc.retryLoop(ctx, gen)
	}()
}

// retryLoop repeats the startup with a backoff (retryMin doubling to retryMax) until it works, the bot is
// reconfigured since the attempt gen (a write applied the newest state, and starts its own loop if that failed), or
// ctx ends.
func (svc *Service) retryLoop(ctx context.Context, gen uint64) {
	wait := svc.retryMin
	for retry := true; retry; {
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		retry, gen = svc.startOnce(ctx, gen)
		wait = min(wait*2, svc.retryMax)
	}
}

// retryAfterWrite starts the backoff loop for the generation a write just applied, when that failed in a way worth
// retrying. Must hold svc.mu (the loop's first attempt waits for it).
func (svc *Service) retryAfterWrite(retry bool) {
	base := svc.run.baseCtx
	if base == nil {
		return
	}
	if !retry || base.Err() != nil {
		svc.startRetryLocked(base, 0) // the write applied the newest state: a loop for an older one is superseded
		return
	}
	svc.run.mu.Lock()
	gen := svc.run.gen
	svc.run.mu.Unlock()
	svc.startRetryLocked(base, gen)
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
	svc.stopSender()
	empty := ""
	svc.miniAppHost.Store(&empty)
	svc.miniAppBot.Store(nil)
}

// applyLocked makes the live bot match row (nil: no bot). drop drops the updates Telegram holds (a new token). It
// returns whether a failure is worth retrying (not the token's fault). Must hold svc.mu; never inside a transaction.
//
// The poller runs on central's serving context (Start), not ctx: it outlives the write that configured it.
func (svc *Service) applyLocked(ctx context.Context, row *db.TelegramBot, drop bool) (retry bool) { //nolint:contextcheck // see above
	var gen uint64
	stored := ""
	if row != nil {
		stored = row.MiniappHost
	}
	// Routing follows the stored row before anything else: a disabled or failing bot's host stays the Mini App's (404).
	prevStored := svc.setStored(row)
	svc.setState(func(rt *runtime) { rt.gen++; gen = rt.gen })
	if m := svc.hostMismatch(stored); m != "" && (m != prevStored || gen == 1) {
		svc.o.Log.Warn("telegram: the bot's miniapp_host differs from telegram.miniapp_host; the configuration wins",
			slog.String("miniapp_host", svc.o.MiniAppHost), slog.String("bot_miniapp_host", m))
	}
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
	svc.startSender(c) //nolint:contextcheck // the sender runs on central's serving context, like the poller
	host := svc.effectiveHost(row.MiniappHost)
	svc.miniAppHost.Store(&host)
	if c.BotID() == strconv.FormatInt(row.BotID, 10) {
		// Only the configured bot's own token signs initData its links accept.
		svc.miniAppBot.Store(&miniapp.Bot{ID: row.BotID, Secret: miniapp.SecretKey(string(token))})
	} else {
		svc.miniAppBot.Store(nil)
	}
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
	stored := svc.StoredMiniAppHost()
	rt := &svc.run
	rt.mu.Lock()
	defer rt.mu.Unlock()
	st := Status{Configured: rt.configured, Enabled: rt.enabled, Mode: rt.mode, BotID: rt.botID, Username: rt.username,
		WebhookStatus: rt.state, Error: rt.err, MiniAppHost: svc.effectiveHost(stored), MiniAppHostIgnored: svc.hostMismatch(stored), MiniAppHostNeedsKey: svc.hostNeedsKey(stored)}
	switch {
	case svc.o.MiniAppHost != "":
		st.MiniAppHostSource = "config"
	case st.MiniAppHost != "":
		st.MiniAppHostSource = "bot"
	}
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

// Sender is the live bot's outbound queue, or nil while the bot is off. It is replaced whenever the bot is
// reconfigured (a new token, another mode), so callers read it per use and don't keep it.
func (svc *Service) Sender() *sender.Sender {
	svc.sndMu.Lock()
	defer svc.sndMu.Unlock()
	return svc.snd
}

// SetLimits keeps the send limits for the live sender and for every sender made later. It is what
// settings.Service.BindSender is given, once: the settings' listeners can't be removed, so the listener must outlive
// the sender it was first made for. A refused limit set (outside the sender's bounds) changes nothing.
func (svc *Service) SetLimits(l sender.Limits) error {
	if err := l.Validate(); err != nil {
		return err
	}
	svc.sndMu.Lock()
	defer svc.sndMu.Unlock()
	svc.limits = l
	if svc.snd != nil {
		return svc.snd.SetLimits(l)
	}
	return nil
}

// startSender replaces the outbound sender with one for c, running on central's serving context (nothing sends before
// Start gives it one; a sender made earlier is replaced then). Must hold svc.mu.
func (svc *Service) startSender(c *botapi.Client) {
	svc.stopSender()
	svc.sndMu.Lock()
	defer svc.sndMu.Unlock()
	snd, err := sender.New(sender.Options{Client: c, Limits: svc.limits, Metrics: svc.o.Metrics,
		Logger: obs.Component(svc.o.Log, "telegram.sender")})
	if err != nil {
		svc.o.Log.Warn("telegram: starting the outbound sender failed", slog.String("error", err.Error()))
		return
	}
	svc.snd = snd
	base := svc.run.baseCtx
	if base == nil {
		return
	}
	ctx, cancel := context.WithCancel(base)
	done := make(chan struct{})
	svc.sndStop, svc.sndDone = cancel, done
	go func() {
		defer close(done)
		_ = snd.Run(ctx)
	}()
}

// stopSender stops the outbound sender and waits for it: what it still queued is dropped. Must hold svc.mu.
func (svc *Service) stopSender() {
	svc.sndMu.Lock()
	stop, done := svc.sndStop, svc.sndDone
	svc.snd, svc.sndStop, svc.sndDone = nil, nil, nil
	svc.sndMu.Unlock()
	if stop != nil {
		stop()
		<-done
	}
}
