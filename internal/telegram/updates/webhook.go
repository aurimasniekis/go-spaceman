package updates

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/netip"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/httpguard"
	"github.com/aurimasniekis/go-spaceman/internal/obs/metrics"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
)

// WebhookPath is the route prefix; the full path is WebhookPath + the hook id. Central mounts it on the main host's
// top-level mux, outside the API handler, and never on the Mini App host.
const WebhookPath = "/telegram/webhook/"

// SecretHeader carries setWebhook's secret_token on every webhook request (API#setwebhook).
const SecretHeader = "X-Telegram-Bot-Api-Secret-Token" //nolint:gosec // G101: a header name, not a credential

// MaxBodyBytes bounds one webhook request body (256 KiB). One update is far smaller: a message is at most 4096
// characters of text plus entities.
const MaxBodyBytes = 256 << 10

// readTimeout bounds reading one request's body, on the real clock (central's server sets no ReadTimeout).
const readTimeout = 10 * time.Second

// rejectAuditEvery is the most often OnUnauthorized is called.
const rejectAuditEvery = time.Minute

var (
	// reHookID is 16 random bytes as unpadded base64url.
	reHookID = regexp.MustCompile(`^[A-Za-z0-9_-]{22}$`)
	// reSecret is setWebhook's secret_token alphabet (API#setwebhook).
	reSecret = regexp.MustCompile(`^[A-Za-z0-9_-]{1,256}$`)
)

// WebhookConfig is the live webhook configuration (from the telegram_bot row).
type WebhookConfig struct {
	// HookID is the path's last segment.
	HookID string
	// Secret is the secret_token registered with setWebhook.
	Secret string
	// AllowedIPs, when not empty, is the only source ranges accepted (the client address after trusted proxies).
	AllowedIPs []netip.Prefix
}

type liveConfig struct {
	hookID     []byte
	secretHash [sha256.Size]byte
	allowed    []netip.Prefix
}

// WebhookOptions configure a Webhook.
type WebhookOptions struct {
	// TrustedProxies may set X-Forwarded-For; other peers' headers are ignored (central's http.trusted_proxies).
	TrustedProxies []netip.Prefix
	// OnUnauthorized records a request refused for its secret token (an audit row), at most once a minute. It gets the
	// client address (of the latest refusal) and how many were refused since the last call; it must not block for long.
	// Refusals still pending when a burst stops are recorded by Service.Run's timer once the minute has passed.
	OnUnauthorized func(ctx context.Context, clientIP string, refused int64)
	Clock          clock.Clock
	Metrics        *metrics.Metrics
	Log            *slog.Logger
}

// Webhook serves POST WebhookPath<hook id>. Until Set is given a configuration every request gets 404.
//
// The checks run in this order, and each refusal ends the request before anything later happens:
//  1. a configuration exists and the hook id matches (404);
//  2. POST (405) with Content-Type application/json (415);
//  3. the secret token matches, compared in constant time, before the body is read (401);
//  4. the client address is in the allowlist, if one is set (403);
//  5. the body is at most MaxBodyBytes (413) and is one valid update (400);
//  6. a duplicate update_id is acknowledged and dropped (200);
//  7. the update is queued (200), or its shard is full (503, so Telegram retries).
//
// Reading one request is bounded to readTimeout on the real clock. A refusal before the body is read (1-4, and a
// declared length over the limit) also closes the connection, so a client can't hold it open by never sending the body.
type Webhook struct {
	intake   *Intake
	trusted  []netip.Prefix
	onUnauth func(ctx context.Context, clientIP string, refused int64)
	clock    clock.Clock
	metrics  *metrics.Metrics
	log      *slog.Logger

	cfg     atomic.Pointer[liveConfig]
	running atomic.Bool

	rejMu      sync.Mutex
	rejLast    time.Time
	rejPending int64
	rejIP      string // the latest refused client, for a timer flush
}

// NewWebhook returns a webhook feeding intake.
func NewWebhook(intake *Intake, o WebhookOptions) *Webhook {
	if o.Clock == nil {
		o.Clock = clock.System{}
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	return &Webhook{intake: intake, trusted: o.TrustedProxies, onUnauth: o.OnUnauthorized, clock: o.Clock, metrics: o.Metrics, log: o.Log}
}

// ErrInvalidConfig means a WebhookConfig has a malformed hook id or secret.
var ErrInvalidConfig = errors.New("telegram webhook: the hook id must be 22 base64url characters and the secret 1-256 of [A-Za-z0-9_-]")

// Set replaces the configuration; nil turns the webhook off (every request 404, as in polling mode).
func (w *Webhook) Set(c *WebhookConfig) error {
	if c == nil {
		w.cfg.Store(nil)
		return nil
	}
	if !reHookID.MatchString(c.HookID) || !reSecret.MatchString(c.Secret) {
		return ErrInvalidConfig
	}
	w.cfg.Store(&liveConfig{hookID: []byte(c.HookID), secretHash: sha256.Sum256([]byte(c.Secret)), allowed: append([]netip.Prefix(nil), c.AllowedIPs...)})
	return nil
}

// setRunning tells the webhook whether workers are taking updates; while they aren't it answers 503.
func (w *Webhook) setRunning(v bool) { w.running.Store(v) }

func (w *Webhook) refuse(rw http.ResponseWriter, code int, r metrics.TelegramWebhookResult) {
	w.metrics.TelegramWebhook(r)
	// A fixed body: nothing from the request is ever reflected.
	http.Error(rw, http.StatusText(code), code)
}

// refuseUnread refuses a request whose body was never read and closes the connection. Without this net/http would
// try to drain up to 256 KiB of the unread body to reuse the connection, before the response (to keep the connection)
// or after it (to find the body's end), so an unauthenticated client that declares a body and never sends it would
// hold a goroutine. Connection: close skips the first drain; a read deadline of now ends the second at once.
func (w *Webhook) refuseUnread(rw http.ResponseWriter, code int, r metrics.TelegramWebhookResult) {
	rw.Header().Set("Connection", "close")
	w.refuse(rw, code, r)
	_ = http.NewResponseController(rw).SetReadDeadline(time.Now())
}

// ServeHTTP implements http.Handler.
func (w *Webhook) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	// Real time, not w.clock: the deadline is for the network connection. It bounds the whole request, refusals too.
	_ = http.NewResponseController(rw).SetReadDeadline(time.Now().Add(readTimeout))
	cfg := w.cfg.Load()
	hook, ok := strings.CutPrefix(r.URL.Path, WebhookPath)
	// The hook id is not a secret (the secret token is), but comparing it in constant time costs nothing.
	if cfg == nil || !ok || subtle.ConstantTimeCompare([]byte(hook), cfg.hookID) != 1 {
		w.refuseUnread(rw, http.StatusNotFound, metrics.TelegramWebhookNotFound)
		return
	}
	if r.Method != http.MethodPost {
		rw.Header().Set("Allow", http.MethodPost)
		w.refuseUnread(rw, http.StatusMethodNotAllowed, metrics.TelegramWebhookBadMethod)
		return
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		w.refuseUnread(rw, http.StatusUnsupportedMediaType, metrics.TelegramWebhookBadType)
		return
	}
	// Hashing first makes the comparison independent of the presented token's length as well as its content.
	got := sha256.Sum256([]byte(r.Header.Get(SecretHeader)))
	if subtle.ConstantTimeCompare(got[:], cfg.secretHash[:]) != 1 {
		w.unauthorized(r)
		w.refuseUnread(rw, http.StatusUnauthorized, metrics.TelegramWebhookUnauthorized)
		return
	}
	if len(cfg.allowed) > 0 && !w.allowedSource(r, cfg.allowed) {
		w.refuseUnread(rw, http.StatusForbidden, metrics.TelegramWebhookForbiddenIP)
		return
	}
	if r.ContentLength > MaxBodyBytes {
		w.refuseUnread(rw, http.StatusRequestEntityTooLarge, metrics.TelegramWebhookTooLarge)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(rw, r.Body, MaxBodyBytes))
	if err != nil {
		if mbe := (*http.MaxBytesError)(nil); errors.As(err, &mbe) {
			w.refuse(rw, http.StatusRequestEntityTooLarge, metrics.TelegramWebhookTooLarge)
			return
		}
		w.refuse(rw, http.StatusBadRequest, metrics.TelegramWebhookBadRequest)
		return
	}
	u, ok := decodeUpdate(body)
	if !ok {
		w.refuse(rw, http.StatusBadRequest, metrics.TelegramWebhookBadRequest)
		return
	}
	if !w.running.Load() {
		w.refuse(rw, http.StatusServiceUnavailable, metrics.TelegramWebhookUnavailable)
		return
	}
	switch w.intake.Offer(u) {
	case Duplicate:
		w.metrics.TelegramWebhook(metrics.TelegramWebhookDuplicate)
	case Full:
		w.refuse(rw, http.StatusServiceUnavailable, metrics.TelegramWebhookQueueFull)
		return
	default:
		w.metrics.TelegramWebhook(metrics.TelegramWebhookAccepted)
	}
	// An empty 200: a method in the response body would run with an unknown result (API#making-requests-when-getting-updates).
	rw.WriteHeader(http.StatusOK)
}

// decodeUpdate parses exactly one JSON object with a positive update_id.
func decodeUpdate(body []byte) (botapi.Update, bool) {
	var u botapi.Update
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&u); err != nil || u.UpdateID <= 0 {
		return botapi.Update{}, false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return botapi.Update{}, false
	}
	return u, true
}

// unauthorized counts a refused secret and calls OnUnauthorized at most once per rejectAuditEvery.
func (w *Webhook) unauthorized(r *http.Request) {
	if w.onUnauth == nil {
		return
	}
	ip := httpguard.ClientIP(r, w.trusted).String()
	w.rejMu.Lock()
	w.rejPending++
	w.rejIP = ip
	w.rejMu.Unlock()
	w.flushUnauthorized(r.Context())
}

// flushUnauthorized calls OnUnauthorized with the refusals counted since the last call, if there are any and
// rejectAuditEvery has passed since it. Service.Run calls it on a timer, so a burst that stops is still recorded.
func (w *Webhook) flushUnauthorized(ctx context.Context) {
	if w.onUnauth == nil {
		return
	}
	now := w.clock.Now()
	w.rejMu.Lock()
	if w.rejPending == 0 || (!w.rejLast.IsZero() && now.Sub(w.rejLast) < rejectAuditEvery) {
		w.rejMu.Unlock()
		return
	}
	n, ip := w.rejPending, w.rejIP
	w.rejLast, w.rejPending = now, 0
	w.rejMu.Unlock()
	w.onUnauth(ctx, ip, n)
}

func (w *Webhook) allowedSource(r *http.Request, allowed []netip.Prefix) bool {
	ip := httpguard.ClientIP(r, w.trusted)
	if !ip.IsValid() {
		return false
	}
	for _, p := range allowed {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}
