package metrics

import "github.com/prometheus/client_golang/prometheus"

// Inbound Telegram updates (internal/telegram/updates, ADR-0083, SM-TG-004). Chat, user, link and update identifiers
// are never labels, and neither is anything from an update's content: every label below is a fixed enum.

// TelegramWebhookResult is how the webhook answered one request.
type TelegramWebhookResult string

// Webhook results.
const (
	TelegramWebhookAccepted     TelegramWebhookResult = "accepted"     // 200: queued
	TelegramWebhookDuplicate    TelegramWebhookResult = "duplicate"    // 200: update_id already seen, dropped
	TelegramWebhookQueueFull    TelegramWebhookResult = "queue_full"   // 503: Telegram retries later
	TelegramWebhookUnauthorized TelegramWebhookResult = "unauthorized" // 401: wrong or missing secret token
	TelegramWebhookNotFound     TelegramWebhookResult = "not_found"    // 404: no webhook configured, or a wrong hook id
	TelegramWebhookBadMethod    TelegramWebhookResult = "bad_method"   // 405
	TelegramWebhookBadType      TelegramWebhookResult = "bad_type"     // 415: not application/json
	TelegramWebhookForbiddenIP  TelegramWebhookResult = "forbidden_ip" // 403: outside the source-IP allowlist
	TelegramWebhookTooLarge     TelegramWebhookResult = "too_large"    // 413: body over the limit
	TelegramWebhookBadRequest   TelegramWebhookResult = "bad_request"  // 400: unreadable or invalid update
	TelegramWebhookUnavailable  TelegramWebhookResult = "unavailable"  // 503: the dispatcher isn't running
)

// TelegramUpdateOutcome is what the dispatcher did with one update.
type TelegramUpdateOutcome string

// Dispatcher outcomes.
const (
	TelegramUpdateLinked          TelegramUpdateOutcome = "linked"           // handed to the handler for a linked user
	TelegramUpdateUnlinkedReplied TelegramUpdateOutcome = "unlinked_replied" // an unlinked user got the linking instructions
	TelegramUpdateUnlinkedDropped TelegramUpdateOutcome = "unlinked_dropped" // an unlinked user, dropped without a reply
	TelegramUpdateLeftChat        TelegramUpdateOutcome = "left_chat"        // the bot left a group it was added to
	TelegramUpdateIgnored         TelegramUpdateOutcome = "ignored"          // a kind, chat type or sender the bot doesn't handle
	TelegramUpdateFailed          TelegramUpdateOutcome = "failed"           // the handler panicked or a reply failed
	TelegramUpdateThrottled       TelegramUpdateOutcome = "throttled"        // a linked user over the inbound action limit
)

var (
	enumTelegramWebhookResult = newEnum(TelegramWebhookAccepted, TelegramWebhookDuplicate, TelegramWebhookQueueFull,
		TelegramWebhookUnauthorized, TelegramWebhookNotFound, TelegramWebhookBadMethod, TelegramWebhookBadType,
		TelegramWebhookForbiddenIP, TelegramWebhookTooLarge, TelegramWebhookBadRequest, TelegramWebhookUnavailable)
	enumTelegramUpdateOutcome = newEnum(TelegramUpdateLinked, TelegramUpdateUnlinkedReplied, TelegramUpdateUnlinkedDropped,
		TelegramUpdateLeftChat, TelegramUpdateIgnored, TelegramUpdateFailed, TelegramUpdateThrottled)
)

type telegramUpdateMetrics struct {
	webhook  *prometheus.CounterVec
	updates  *prometheus.CounterVec
	queued   prometheus.Gauge
	pollErrs prometheus.Counter
}

func newTelegramUpdateMetrics(f factory) telegramUpdateMetrics {
	return telegramUpdateMetrics{
		webhook:  f.counterVec("telegram_webhook_requests_total", "Telegram webhook requests by result.", "result"),
		updates:  f.counterVec("telegram_updates_total", "Telegram updates the dispatcher handled, by outcome.", "outcome"),
		queued:   f.gauge("telegram_updates_queued", "Telegram updates accepted and waiting for a dispatcher worker."),
		pollErrs: f.counter("telegram_poll_errors_total", "Failed getUpdates long polls (polling mode)."),
	}
}

// TelegramWebhook counts one webhook request by its result.
func (m *Metrics) TelegramWebhook(r TelegramWebhookResult) {
	if m == nil {
		return
	}
	m.tgUpdates.webhook.WithLabelValues(enumTelegramWebhookResult.norm(string(r))).Inc()
}

// TelegramUpdate counts one dispatched update by its outcome.
func (m *Metrics) TelegramUpdate(o TelegramUpdateOutcome) {
	if m == nil {
		return
	}
	m.tgUpdates.updates.WithLabelValues(enumTelegramUpdateOutcome.norm(string(o))).Inc()
}

// TelegramQueued moves the queued-updates gauge by delta.
func (m *Metrics) TelegramQueued(delta int) {
	if m == nil {
		return
	}
	m.tgUpdates.queued.Add(float64(delta))
}

// TelegramPollError counts one failed getUpdates call.
func (m *Metrics) TelegramPollError() {
	if m == nil {
		return
	}
	m.tgUpdates.pollErrs.Inc()
}
