package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Inbound event ingestion (internal/events, SM-EVT-011…016). Subscription, space, connection and event identifiers
// and subjects are never labels: every label below is a fixed enum.

// IngestDelivery is how a subscription receives events.
type IngestDelivery string

// Delivery modes.
const (
	IngestDurable IngestDelivery = "durable" // a JetStream durable consumer
	IngestCore    IngestDelivery = "core"    // a plain NATS Core subscription (non-durable)
)

// IngestOrigin is why an event was handled (the delivery history's origin).
type IngestOrigin string

// Delivery origins.
const (
	IngestLive       IngestOrigin = "live"
	IngestRedelivery IngestOrigin = "redelivery"
	IngestReplay     IngestOrigin = "replay"
	IngestReprocess  IngestOrigin = "reprocess"
)

// IngestResult is what came of one handled event.
type IngestResult string

// Ingestion results.
const (
	IngestCreated     IngestResult = "created"      // an inbox item was created
	IngestDuplicate   IngestResult = "duplicate"    // the event had already produced an item
	IngestFiltered    IngestResult = "filtered"     // a filter did not match, or it was this installation's own event
	IngestQuarantined IngestResult = "quarantined"  // invalid: recorded in quarantine
	IngestError       IngestResult = "error"        // storage or other transient failure (durable: redelivered; core: lost)
	IngestRateLimited IngestResult = "rate_limited" // over the space's ingestion rate (durable: left in the stream; core: dropped)
)

// IngestSubState is a subscription's runtime state.
type IngestSubState string

// Subscription states.
const (
	IngestSubRunning  IngestSubState = "running"
	IngestSubStopped  IngestSubState = "stopped"
	IngestSubError    IngestSubState = "error"
	IngestSubDisabled IngestSubState = "disabled"
	IngestSubPaused   IngestSubState = "paused" // stopped while its space is paused (while_paused stop)
)

// IngestConnState is an event connection's state.
type IngestConnState string

// Connection states.
const (
	IngestConnConnected    IngestConnState = "connected"
	IngestConnReconnecting IngestConnState = "reconnecting" // the client lost the server and is reconnecting
	IngestConnError        IngestConnState = "error"        // the connection could not be established
	IngestConnDisabled     IngestConnState = "disabled"
)

// ConnectionKind is an event connection's kind.
type ConnectionKind string

// Connection kinds.
const ConnectionPulse ConnectionKind = "pulse"

var (
	ingestDeliveries = []IngestDelivery{IngestDurable, IngestCore}
	ingestSubStates  = []IngestSubState{IngestSubRunning, IngestSubStopped, IngestSubError, IngestSubDisabled, IngestSubPaused}
	ingestConnStates = []IngestConnState{IngestConnConnected, IngestConnReconnecting, IngestConnError, IngestConnDisabled}
	connectionKinds  = []ConnectionKind{ConnectionPulse}

	enumIngestDelivery = newEnum(ingestDeliveries...)
	enumIngestOrigin   = newEnum(IngestLive, IngestRedelivery, IngestReplay, IngestReprocess)
	enumIngestResult   = newEnum(IngestCreated, IngestDuplicate, IngestFiltered, IngestQuarantined, IngestError, IngestRateLimited)
)

type ingestMetrics struct {
	deliveries  *prometheus.CounterVec
	duration    *prometheus.HistogramVec
	pending     *prometheus.GaugeVec
	ackPending  prometheus.Gauge
	coreDropped prometheus.Counter
	subs        *prometheus.GaugeVec
	conns       *prometheus.GaugeVec
	quarantine  prometheus.Gauge
	replays     prometheus.Counter
}

func newIngestMetrics(f factory) ingestMetrics {
	return ingestMetrics{
		deliveries: f.counterVec("ingest_deliveries_total", "Inbound events handled by subscriptions, by delivery mode, origin and result.",
			"delivery", "origin", "result"),
		duration: f.histogramVec("ingest_processing_duration_seconds", "Time to handle one inbound event (validate, render, store), by delivery mode.",
			LatencyBuckets, "delivery"),
		pending: f.gaugeVec("ingest_consumer_pending", "Inbound events waiting for SpaceMan: durable, not yet delivered by the JetStream consumers; "+
			"core, buffered in the client. Summed over subscriptions.", "delivery"),
		ackPending:  f.gauge("ingest_consumer_ack_pending", "Inbound events delivered by the JetStream consumers and not yet acknowledged, summed over subscriptions."),
		coreDropped: f.counter("ingest_core_dropped_total", "Core NATS messages the client dropped because a subscription fell behind (slow consumer); they are lost."),
		subs:        f.gaugeVec("ingest_subscriptions", "Event subscriptions by delivery mode and runtime state.", "delivery", "state"),
		conns:       f.gaugeVec("ingest_connections", "Event connections by kind and state.", "connection_kind", "state"),
		quarantine:  f.gauge("ingest_quarantine_open", "Open quarantine entries over every subscription."),
		replays:     f.counter("ingest_replays_total", "Explicit replays recorded (replay operations and cursor changes that deliver history again)."),
	}
}

// IngestProcessed records one handled inbound event and how long handling took (took < 0: not timed, e.g. an event
// refused by the rate limit before it was handled).
func (m *Metrics) IngestProcessed(d IngestDelivery, o IngestOrigin, r IngestResult, took time.Duration) {
	if m == nil {
		return
	}
	dl := enumIngestDelivery.norm(string(d))
	m.ingest.deliveries.WithLabelValues(dl, enumIngestOrigin.norm(string(o)), enumIngestResult.norm(string(r))).Inc()
	if took >= 0 {
		m.ingest.duration.WithLabelValues(dl).Observe(took.Seconds())
	}
}

// SetIngestPending sets the waiting-event gauges: pending per delivery mode, and the durable consumers' unacknowledged deliveries.
func (m *Metrics) SetIngestPending(pending map[IngestDelivery]int64, ackPending int64) {
	if m == nil {
		return
	}
	for _, d := range ingestDeliveries {
		m.ingest.pending.WithLabelValues(string(d)).Set(float64(pending[d]))
	}
	m.ingest.ackPending.Set(float64(ackPending))
}

// IngestCoreDropped adds core NATS messages dropped by the client.
func (m *Metrics) IngestCoreDropped(n int) {
	if m == nil || n <= 0 {
		return
	}
	m.ingest.coreDropped.Add(float64(n))
}

// SetIngestSubscriptions sets the subscription gauge for every delivery mode and state (absent combinations are 0).
func (m *Metrics) SetIngestSubscriptions(counts map[IngestDelivery]map[IngestSubState]int) {
	if m == nil {
		return
	}
	for _, d := range ingestDeliveries {
		for _, s := range ingestSubStates {
			m.ingest.subs.WithLabelValues(string(d), string(s)).Set(float64(counts[d][s]))
		}
	}
}

// SetIngestConnections sets the connection gauge for every kind and state (absent combinations are 0).
func (m *Metrics) SetIngestConnections(counts map[ConnectionKind]map[IngestConnState]int) {
	if m == nil {
		return
	}
	for _, k := range connectionKinds {
		for _, s := range ingestConnStates {
			m.ingest.conns.WithLabelValues(string(k), string(s)).Set(float64(counts[k][s]))
		}
	}
}

// SetIngestQuarantine sets the number of open quarantine entries.
func (m *Metrics) SetIngestQuarantine(n int64) {
	if m == nil {
		return
	}
	m.ingest.quarantine.Set(float64(n))
}

// IngestReplay counts one recorded replay.
func (m *Metrics) IngestReplay() {
	if m == nil {
		return
	}
	m.ingest.replays.Inc()
}
