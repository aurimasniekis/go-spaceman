package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Telegram outbound sender (internal/telegram/sender, ADR-0083, plan §4.4). Chat, link, user and message identifiers
// are never labels: every label below is a fixed enum.

// TelegramClass is a sender priority class.
type TelegramClass string

// Priority classes, most urgent first.
const (
	TelegramInteractive  TelegramClass = "interactive"  // callback answers and replies to the user's own action
	TelegramNeedsYou     TelegramClass = "needs_you"    // new needs-you cards and escalations
	TelegramNotification TelegramClass = "notification" // other notifications, and overflow notices
	TelegramEdit         TelegramClass = "edit"         // card edits (final state, refresh)
	TelegramDigest       TelegramClass = "digest"       // digests
)

// TelegramSendResult is how a queued message ended after it was handed to the Bot API.
type TelegramSendResult string

// Send results.
const (
	TelegramSent    TelegramSendResult = "sent"    // Telegram accepted it
	TelegramFailed  TelegramSendResult = "failed"  // an error that retrying won't fix, or the retries ran out
	TelegramBlocked TelegramSendResult = "blocked" // 403: the user blocked the bot, or the bot can't write to the chat
)

// TelegramDropReason is why a queued message was never sent.
type TelegramDropReason string

// Drop reasons.
const (
	TelegramDropQueueFull TelegramDropReason = "queue_full" // a queue bound was reached and this was the lowest class
	TelegramDropCoalesced TelegramDropReason = "coalesced"  // a newer message with the same key replaced it
	TelegramDropBlocked   TelegramDropReason = "blocked"    // its chat blocked the bot
	TelegramDropShutdown  TelegramDropReason = "shutdown"   // the sender stopped
)

// TelegramLimitScope is what a 429 answer was attributed to.
type TelegramLimitScope string

// Rate-limit scopes.
const (
	TelegramScopeChat   TelegramLimitScope = "chat"
	TelegramScopeGlobal TelegramLimitScope = "global"
)

var (
	telegramClasses = []TelegramClass{TelegramInteractive, TelegramNeedsYou, TelegramNotification, TelegramEdit, TelegramDigest}

	enumTelegramClass  = newEnum(telegramClasses...)
	enumTelegramResult = newEnum(TelegramSent, TelegramFailed, TelegramBlocked)
	enumTelegramDrop   = newEnum(TelegramDropQueueFull, TelegramDropCoalesced, TelegramDropBlocked, TelegramDropShutdown)
	enumTelegramScope  = newEnum(TelegramScopeChat, TelegramScopeGlobal)
)

// TelegramWaitBuckets suit queue waits: an interactive answer within a second, a group backlog at 20/min in minutes.
var TelegramWaitBuckets = []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 180, 600}

type telegramMetrics struct {
	sends       *prometheus.CounterVec
	dropped     *prometheus.CounterVec
	rateLimited *prometheus.CounterVec
	overflow    prometheus.Counter
	depth       *prometheus.GaugeVec
	wait        *prometheus.HistogramVec
}

func newTelegramMetrics(f factory) telegramMetrics {
	return telegramMetrics{
		sends: f.counterVec("telegram_sends_total", "Telegram messages the sender finished, by priority class and result.",
			"class", "result"),
		dropped: f.counterVec("telegram_dropped_total", "Telegram messages the sender dropped without sending, by priority class and reason.",
			"class", "reason"),
		rateLimited: f.counterVec("telegram_rate_limited_total", "429 answers from the Bot API, by the scope the sender paused (chat or global).",
			"scope"),
		overflow: f.counter("telegram_overflow_notices_total", "Overflow notices queued: a chat's dropped backlog collapsed into one \"N updates\" message."),
		depth:    f.gaugeVec("telegram_queue_depth", "Telegram messages waiting in the sender's queue, by priority class.", "class"),
		wait: f.histogramVec("telegram_queue_wait_seconds", "Time from enqueue to the first send attempt, by priority class.",
			TelegramWaitBuckets, "class"),
	}
}

// TelegramSend records a message the sender finished.
func (m *Metrics) TelegramSend(c TelegramClass, r TelegramSendResult) {
	if m == nil {
		return
	}
	m.telegram.sends.WithLabelValues(enumTelegramClass.norm(string(c)), enumTelegramResult.norm(string(r))).Inc()
}

// TelegramDropped records n messages dropped without sending.
func (m *Metrics) TelegramDropped(c TelegramClass, r TelegramDropReason, n int) {
	if m == nil || n <= 0 {
		return
	}
	m.telegram.dropped.WithLabelValues(enumTelegramClass.norm(string(c)), enumTelegramDrop.norm(string(r))).Add(float64(n))
}

// TelegramRateLimited records one 429 answer.
func (m *Metrics) TelegramRateLimited(s TelegramLimitScope) {
	if m == nil {
		return
	}
	m.telegram.rateLimited.WithLabelValues(enumTelegramScope.norm(string(s))).Inc()
}

// TelegramOverflowNotice records an overflow notice queued for a chat.
func (m *Metrics) TelegramOverflowNotice() {
	if m == nil {
		return
	}
	m.telegram.overflow.Inc()
}

// SetTelegramQueueDepth sets the queue depth gauges; a class missing from depth is 0.
func (m *Metrics) SetTelegramQueueDepth(depth map[TelegramClass]int) {
	if m == nil {
		return
	}
	for _, c := range telegramClasses {
		m.telegram.depth.WithLabelValues(string(c)).Set(float64(depth[c]))
	}
}

// TelegramQueueWait observes how long a message waited before its first send attempt.
func (m *Metrics) TelegramQueueWait(c TelegramClass, d time.Duration) {
	if m == nil {
		return
	}
	m.telegram.wait.WithLabelValues(enumTelegramClass.norm(string(c))).Observe(d.Seconds())
}
