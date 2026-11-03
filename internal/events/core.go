package events

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/aurimasniekis/go-pulse/pkg/pulse"
	"github.com/aurimasniekis/go-pulse/pkg/pulse/envelope"
	"github.com/nats-io/nats.go"

	"github.com/aurimasniekis/go-spaceman/internal/obs"
	"github.com/aurimasniekis/go-spaceman/internal/obs/metrics"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Core NATS subscriptions (delivery "core", ADR-0042) receive messages through the connection's plain NATS client
// (pulse.Client.Conn, docs/contracts/pulse.md: "for plain NATS Core use c.Conn() with subscribe grants") instead of a
// JetStream durable. They are non-durable: a message is handled once, as it arrives; nothing acknowledges it, so a
// transient failure, a slow-consumer drop or a disconnection loses it.

// subscribeCheck bounds the round trip that makes the server answer a core subscription (a refusal arrives before it).
const subscribeCheck = 5 * time.Second

// reCoreID is the message ID a core message may carry in Nats-Msg-Id to be deduplicated by it. Anything else (or no
// header) is deduplicated by content (coreEventID).
var reCoreID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

// coreEventID is the identity of a core message, used for deduplication like a Pulse event ID: its Nats-Msg-Id header
// when it carries a well-formed one, otherwise "sha256-" and the first 128 bits (hex) of a SHA-256 over the subject,
// the ce-type, ce-source and ce-time headers and the payload (each length-prefixed). So a source that re-sends the same
// message (same time stamp) is deduplicated, while two messages that differ in any of these fields are not.
func coreEventID(e pulse.Event, h nats.Header) string {
	if reCoreID.MatchString(e.ID) {
		return e.ID
	}
	sum := sha256.New()
	for _, part := range []string{e.Subject, h.Get(envelope.HeaderType), h.Get(envelope.HeaderSource), h.Get(envelope.HeaderTime), string(e.Data)} {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(part)))
		sum.Write(n[:])
		sum.Write([]byte(part))
	}
	return "sha256-" + hex.EncodeToString(sum.Sum(nil)[:16])
}

// startCore subscribes a core subscription on the connection's NATS client. A refusal by the server (no subscribe
// grant) is reported here when it arrives within the check round trip, and through natsError otherwise.
func (m *Manager) startCore(ctx context.Context, c *connRun, s db.EventSubscription, key string) error {
	nc := c.client.Conn()
	sctx, stop := context.WithCancel(ctx)
	run := &subRun{key: key, id: s.ID, subject: s.EffectiveSubject, stop: stop, nc: nc}
	subID, spaceID := s.ID, s.SpaceID
	before := nc.LastError()
	nsub, err := nc.Subscribe(s.EffectiveSubject, func(msg *nats.Msg) { m.handleCore(sctx, subID, spaceID, msg) })
	if err != nil {
		stop()
		return fmt.Errorf("core subscribe: %w", err)
	}
	run.nsub = nsub
	if err := nc.FlushTimeout(subscribeCheck); err != nil {
		run.halt()
		return fmt.Errorf("core subscribe: %w", err)
	}
	if last := nc.LastError(); last != nil && last != before && deniedSubscription(last, s.EffectiveSubject) { //nolint:errorlint // identity: a new error since subscribing
		run.halt()
		return deniedError(s.EffectiveSubject)
	}
	m.coreMu.Lock()
	m.core[nsub] = run
	m.coreMu.Unlock()
	m.subs[s.ID] = run
	return nil
}

func deniedSubscription(err error, subject string) bool {
	return errors.Is(err, nats.ErrPermissionViolation) && strings.Contains(strings.ToLower(err.Error()), "subscription") &&
		strings.Contains(err.Error(), `"`+subject+`"`)
}

func deniedError(subject string) error {
	return fmt.Errorf("permission denied: the Pulse client needs the grant subscribe:%s", subject)
}

// handleCore handles one core NATS message: the same loop guard, rate limit, filtering, deduplication, quarantine
// and inbox path as a durable delivery, with no acknowledgement and no stream sequence.
func (m *Manager) handleCore(ctx context.Context, subID, spaceID string, msg *nats.Msg) {
	e := envelope.DecodeMsg(msg)
	e.ID = coreEventID(e, msg.Header)
	if e.Time.IsZero() {
		e.Time = m.svc.o.Clock.Now().UTC() // no ce-time: when SpaceMan received it
	}
	var err error
	switch {
	case m.svc.ownEvent(e):
		err = m.svc.skipOwn(ctx, subID, fromPulse(e), 0)
	case m.throttle(ctx, subID, spaceID, true) != nil:
		m.svc.observe(DeliveryCore, OriginLive, "", errThrottled, -1)
		return // dropped: nothing could redeliver it
	default:
		// Always live: core NATS never redelivers, so a repeated ID is the source sending the message again.
		err = m.svc.Process(ctx, subID, fromPulse(e), 0, Origin{Kind: OriginLive})
	}
	var qe *QuarantinedError
	if err != nil && !errors.As(err, &qe) && ctx.Err() == nil {
		m.svc.setState(subID, "running", "a core NATS message was lost (non-durable, nothing redelivers it): "+err.Error())
		m.log.Warn("core NATS message lost", slog.String("subscription", subID), slog.String("event_id", e.ID), obs.Err(err))
	}
}

// natsError receives a connection's asynchronous errors: a slow consumer (messages dropped by the client) and a
// refused subscription. Both are reported on the core subscriptions concerned.
func (m *Manager) natsError(nc *nats.Conn, sub *nats.Subscription, err error) {
	m.coreMu.Lock()
	defer m.coreMu.Unlock()
	switch {
	case sub != nil && errors.Is(err, nats.ErrSlowConsumer):
		if r := m.core[sub]; r != nil {
			m.svc.setState(r.id, "running", "slow consumer: messages arrived faster than SpaceMan stored them and core NATS dropped some; they are lost")
		}
	case errors.Is(err, nats.ErrPermissionViolation):
		for _, r := range m.core {
			if r.nc == nc && deniedSubscription(err, r.subject) {
				r.failed.Store(true) // retried on the next round, not at once (the grant may take a while)
				m.svc.setState(r.id, "error", deniedError(r.subject).Error())
			}
		}
	}
}

// refreshMetrics sets the ingestion gauges: connections and subscriptions by state, pending messages of durable
// consumers (one consumer-info request each) and of core subscriptions (client buffers), core drops and open
// quarantine entries. Runs on the manager's goroutine after each round.
func (m *Manager) refreshMetrics(ctx context.Context, subs []db.EventSubscription) {
	mt := m.svc.o.Metrics
	if mt == nil {
		return
	}
	conns := map[metrics.ConnectionKind]map[metrics.IngestConnState]int{}
	for _, c := range m.conns {
		st := metrics.IngestConnConnected
		switch {
		case c.row.Enabled == 0:
			st = metrics.IngestConnDisabled
		case c.client == nil:
			st = metrics.IngestConnError
		case !c.client.Conn().IsConnected():
			st = metrics.IngestConnReconnecting
		}
		k := metrics.ConnectionKind(c.row.Kind)
		if conns[k] == nil {
			conns[k] = map[metrics.IngestConnState]int{}
		}
		conns[k][st]++
	}
	mt.SetIngestConnections(conns)
	states := map[metrics.IngestDelivery]map[metrics.IngestSubState]int{}
	for _, s := range subs {
		st, _ := m.svc.runtimeState(s.ID, s.Enabled == 1)
		d := metrics.IngestDelivery(s.Delivery)
		if states[d] == nil {
			states[d] = map[metrics.IngestSubState]int{}
		}
		states[d][metrics.IngestSubState(st)]++
	}
	mt.SetIngestSubscriptions(states)
	pending := map[metrics.IngestDelivery]int64{}
	var ackPending int64
	for _, r := range m.subs {
		if r.nsub != nil {
			if n, _, err := r.nsub.Pending(); err == nil {
				pending[metrics.IngestCore] += int64(n)
			}
			if d, err := r.nsub.Dropped(); err == nil && d > r.dropped {
				mt.IngestCoreDropped(d - r.dropped)
				r.dropped = d
			}
			continue
		}
		ictx, cancel := context.WithTimeout(ctx, 2*time.Second)
		cons, err := r.js.Consumer(ictx, r.stream, r.sub.Consumer())
		cancel()
		if err != nil {
			continue // unreachable or not granted: left out of the sum
		}
		if info := cons.CachedInfo(); info != nil {
			pending[metrics.IngestDurable] += int64(info.NumPending) //nolint:gosec // message counts fit int64
			ackPending += int64(info.NumAckPending)
		}
	}
	mt.SetIngestPending(pending, ackPending)
	if n, err := m.svc.s.R().CountAllOpenQuarantine(ctx); err == nil {
		mt.SetIngestQuarantine(n)
	}
}
