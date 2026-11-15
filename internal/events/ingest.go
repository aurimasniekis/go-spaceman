package events

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aurimasniekis/go-pulse/pkg/pulse"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"golang.org/x/time/rate"

	"github.com/aurimasniekis/go-spaceman/internal/attachments"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/events/template"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/inbox"
	"github.com/aurimasniekis/go-spaceman/internal/obs"
	"github.com/aurimasniekis/go-spaceman/internal/obs/metrics"
	"github.com/aurimasniekis/go-spaceman/internal/outbox"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Delivery outcomes.
const (
	OutcomeAccepted    = "accepted"
	OutcomeDuplicate   = "duplicate"
	OutcomeFiltered    = "filtered"
	OutcomeQuarantined = "quarantined"
	OutcomeReplayed    = "replayed"
)

// MaxQuarantinePayload bounds a quarantined payload (Pulse messages are at most 1 MiB).
const MaxQuarantinePayload = 1 << 20

var ingestActor = audit.Actor{Type: audit.ActorSystem, ID: "event-ingest"}

func fromPulse(e pulse.Event) template.Event {
	return template.Event{ID: e.ID, Subject: e.Subject, Type: e.Type, Source: e.Source, ContentType: e.ContentType, Time: e.Time, Data: e.Data,
		Depth: e.Extra.Get(outbox.HeaderDepth)}
}

// DefaultMaxDepth is the automation depth limit when none is configured (the limit event_depth's default).
const DefaultMaxDepth = 8

// maxHeaderDepth bounds a spaceman-depth value that is accepted as a number at all.
const maxHeaderDepth = 1_000_000

// parseDepth reads a spaceman-depth header: absent is 0 (an event from a source that is not SpaceMan); anything but a
// plain non-negative decimal number is invalid.
func parseDepth(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	if len(s) > 7 || strings.TrimLeft(s, "0123456789") != "" {
		return 0, fmt.Errorf("the spaceman-depth header %q is not a number", truncate(s, 32))
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n > maxHeaderDepth {
		return 0, fmt.Errorf("the spaceman-depth header %q is out of range", s)
	}
	return n, nil
}

func (svc *Service) depthLimit(ctx context.Context, spaceID string) int64 {
	if svc.o.DepthFor != nil {
		if n := svc.o.DepthFor(ctx, spaceID); n > 0 {
			return n
		}
	}
	return DefaultMaxDepth
}

// errTooDeep explains a quarantine for automation depth (SM-EVT-016).
func errTooDeep(depth, limit int64) error {
	return fmt.Errorf("automation depth: this event already passed through %d SpaceMan ingestions (spaceman-depth %d), at or over this space's "+
		"event_depth limit of %d; quarantined to stop an event loop between installations. Reprocess it to ingest it anyway", depth, depth, limit)
}

// Process handles one delivered event for a subscription and says how to acknowledge it (SM-EVT-011/013):
//   - nil: the inbox item is committed, or the event was filtered or is a duplicate: ack;
//   - pulse.Terminal: the event is invalid and recorded in quarantine: terminate, never redeliver;
//   - any other error: transient (storage): nak, the SDK backs off and redelivers.
//
// The origin says why the event is handled; every delivery records it with its attempt number, so a replay is never
// confused with a retry (SM-EVT-015). Core NATS subscriptions (DeliveryCore) use the same path; nothing acknowledges
// their messages, so a transient error loses the message.
func (svc *Service) Process(ctx context.Context, subID string, e template.Event, seq uint64, o Origin) (err error) {
	r, err := svc.s.R().GetEventSubscription(ctx, subID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // deleted while consuming
	} else if err != nil {
		return err
	}
	start, outcome := time.Now(), ""
	defer func() { svc.observe(r.Delivery, o.Kind, outcome, err, time.Since(start)) }()
	prior, err := svc.s.R().CountEventDeliveriesOf(ctx, db.CountEventDeliveriesOfParams{SubscriptionID: subID, EventID: e.ID})
	if err != nil {
		return err
	}
	o.attempt = prior + 1
	if o.Kind == "" {
		o.Kind = OriginLive
		if prior > 0 {
			o.Kind = OriginRedelivery
		}
	}
	def, ref, err := svc.definitionOf(ctx, svc.s.R(), r)
	if err != nil {
		return err
	}
	vars := map[string]string{}
	_ = jsonUnmarshal(r.VarsJson, &vars)
	bound, err := def.Bind(vars)
	if err != nil {
		return svc.quarantine(ctx, r, e, seq, o, fmt.Errorf("the subscription no longer binds: %w", err))
	}
	res, err := bound.Apply(e)
	if errors.Is(err, template.ErrInvalid) {
		return svc.quarantine(ctx, r, e, seq, o, err)
	} else if err != nil {
		return err
	}
	if !res.Accepted {
		outcome = OutcomeFiltered
		return svc.deliver(ctx, r, e, seq, o, OutcomeFiltered, "", res.SkipReason)
	}
	// Bounded automation (SM-EVT-016, ADR-0049): an event that already went through too many SpaceMan ingestions is
	// quarantined, not turned into an item. Reprocessing it is an operator's decision and is not checked again.
	depth, err := parseDepth(e.Depth)
	if err != nil {
		return svc.quarantine(ctx, r, e, seq, o, err)
	}
	if limit := svc.depthLimit(ctx, r.SpaceID); depth >= limit && o.Kind != OriginReprocess {
		return svc.quarantine(ctx, r, e, seq, o, errTooDeep(depth, limit))
	}
	trusted := map[string]any{"event_id": e.ID, "subject": e.Subject, "type": e.Type, "source": e.Source, "time": e.Time.UTC().Format(time.RFC3339Nano),
		"stream_seq": seq, "subscription_id": r.ID, "subscription": r.Name, "connection_id": r.ConnectionID, "instructions": res.Instructions}
	if ref != nil {
		trusted["template_id"], trusted["template_slug"], trusted["template_version"] = ref.TemplateID, ref.TemplateSlug, ref.Version
	} else {
		trusted["template"] = "custom"
	}
	trusted["delivery_origin"], trusted["attempt"] = o.Kind, o.attempt
	dedup := r.ID + ":" + e.ID
	if o.Kind == OriginReplay {
		trusted["replay_id"] = o.ReplayID
		if o.Fresh {
			dedup += ":" + o.ReplayID // a fresh-items replay makes one new item per event, once per replay
		}
	}
	in := inbox.CreateInput{SourceKind: "event", SourceID: r.ID, SourceLabel: r.Name, Title: res.Title, ContentType: inbox.Markdown, Body: res.Body,
		CorrelationID: res.Correlation, DedupKey: dedup, Subscription: r.ID, Urgency: res.Urgency, Trusted: trusted, CausationID: e.ID,
		// While the space is paused, control hears of the item when it resumes unless the subscription says wake
		// (SM-EVT-014). A stop subscription's stragglers (delivered before its consumer stopped) are deferred too.
		AutomationDepth: depth + 1, DeferNoticeWhilePaused: r.WhilePaused != PausedWake}
	// The raw payload is retained as an attachment of the item (SM-INBOX-002, SM-EVT-008).
	if len(e.Data) > 0 && svc.att != nil {
		name := "event-" + e.ID + ".json"
		if a, err := svc.att.Upload(ctx, ingestActor, r.SpaceID, name, bytes.NewReader(e.Data)); err == nil {
			in.AttachmentIDs, in.PayloadRef = []string{a.ID}, attachments.URIScheme+a.ID
			trusted["payload"] = in.PayloadRef
		} else {
			trusted["payload_error"] = err.Error()
		}
	}
	it, created, err := svc.inbox.Create(ctx, ingestActor, r.SpaceID, in)
	if err != nil {
		return err
	}
	outcome = OutcomeAccepted
	switch {
	case !created:
		outcome = OutcomeDuplicate
	case o.Kind == OriginReplay:
		outcome = OutcomeReplayed
	}
	if err := svc.deliver(ctx, r, e, seq, o, outcome, it.ID, ""); err != nil {
		return err
	}
	if outcome == OutcomeAccepted && svc.o.OnAccepted != nil {
		slug := ""
		if ref != nil {
			slug = ref.TemplateSlug
		}
		svc.o.OnAccepted(slug, e)
	}
	return nil
}

// OwnOriginDetail is the delivery detail of an event this installation published itself (loop guard, SM-EVT-016).
const OwnOriginDetail = "published by this SpaceMan (spaceman-origin); not re-ingested"

// ownEvent reports whether e carries this installation's spaceman-origin header.
func (svc *Service) ownEvent(e pulse.Event) bool {
	return svc.o.Origin != "" && e.Extra.Get(outbox.HeaderOrigin) == svc.o.Origin
}

// skipOwn acknowledges one of SpaceMan's own events, recorded as filtered.
func (svc *Service) skipOwn(ctx context.Context, subID string, e template.Event, seq uint64) error {
	r, err := svc.s.R().GetEventSubscription(ctx, subID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	start := time.Now()
	err = svc.deliver(ctx, r, e, seq, Origin{Kind: OriginLive, attempt: 1}, OutcomeFiltered, "", OwnOriginDetail)
	svc.observe(r.Delivery, OriginLive, OutcomeFiltered, err, time.Since(start))
	return err
}

// observe records one handled event in the ingestion metrics: the delivery mode, the origin and what came of it
// (an error wins over the outcome: the delivery was not recorded). took < 0 skips the latency histogram.
func (svc *Service) observe(delivery, origin, outcome string, err error, took time.Duration) {
	if svc.o.Metrics == nil {
		return
	}
	var qe *QuarantinedError
	res := metrics.IngestError
	switch {
	case errors.As(err, &qe):
		res = metrics.IngestQuarantined
	case errors.Is(err, errThrottled):
		res = metrics.IngestRateLimited
	case err != nil:
	case outcome == OutcomeAccepted || outcome == OutcomeReplayed:
		res = metrics.IngestCreated
	case outcome == OutcomeDuplicate:
		res = metrics.IngestDuplicate
	case outcome == OutcomeFiltered:
		res = metrics.IngestFiltered
	}
	if origin == "" {
		origin = OriginLive
	}
	svc.o.Metrics.IngestProcessed(metrics.IngestDelivery(delivery), metrics.IngestOrigin(origin), res, took)
}

func (svc *Service) deliver(ctx context.Context, r db.EventSubscription, e template.Event, seq uint64, o Origin, outcome, itemID, detail string) error {
	return svc.s.W().TxNamed(ctx, "events.delivery", func(tx *store.Tx) error {
		if err := tx.Q.InsertEventDelivery(ctx, db.InsertEventDeliveryParams{SubscriptionID: r.ID, EventID: e.ID, Subject: e.Subject, Type: e.Type,
			StreamSeq: int64(seq), Outcome: outcome, InboxItemID: itemID, Detail: truncate(detail, 1000), At: tx.NowMicros(), //nolint:gosec // stream sequences fit int64
			Origin: o.Kind, Attempt: o.attempt, ReplayID: o.ReplayID}); err != nil {
			return err
		}
		return tx.Q.TouchSubscription(ctx, db.TouchSubscriptionParams{LastEventAt: sql.NullInt64{Int64: tx.NowMicros(), Valid: true}, ID: r.ID})
	})
}

func (svc *Service) quarantine(ctx context.Context, r db.EventSubscription, e template.Event, seq uint64, o Origin, cause error) error {
	payload := e.Data
	if len(payload) > MaxQuarantinePayload {
		payload = payload[:MaxQuarantinePayload]
	}
	depth, _ := parseDepth(e.Depth) // an unreadable header is kept as 0: reprocessing then starts a new chain
	err := svc.s.W().TxNamed(ctx, "events.quarantine", func(tx *store.Tx) error {
		if _, err := tx.Q.InsertQuarantine(ctx, db.InsertQuarantineParams{ID: ids.New(ids.Quarantine), SubscriptionID: r.ID, EventID: e.ID, Subject: e.Subject,
			Type: e.Type, Source: e.Source, EventTime: e.Time.UnixMicro(), ContentType: e.ContentType, Payload: append([]byte{}, payload...),
			Error: truncate(cause.Error(), 2000), CreatedAt: tx.NowMicros(), Depth: depth}); err != nil {
			return err
		}
		if err := tx.Q.InsertEventDelivery(ctx, db.InsertEventDeliveryParams{SubscriptionID: r.ID, EventID: e.ID, Subject: e.Subject, Type: e.Type,
			StreamSeq: int64(seq), Outcome: OutcomeQuarantined, Detail: truncate(cause.Error(), 1000), At: tx.NowMicros(), //nolint:gosec // stream sequences fit int64
			Origin: o.Kind, Attempt: o.attempt, ReplayID: o.ReplayID}); err != nil {
			return err
		}
		return svc.changes.Record(ctx, tx, r.SpaceID, "event_subscription", r.ID, "updated", r.Version)
	})
	if err != nil {
		return err // not recorded: let the SDK redeliver rather than lose the event
	}
	return pulse.Terminal(&QuarantinedError{Cause: cause})
}

// QuarantinedError is returned (inside pulse.Terminal) for an event that was recorded in quarantine.
type QuarantinedError struct{ Cause error }

func (e *QuarantinedError) Error() string { return "quarantined: " + e.Cause.Error() }
func (e *QuarantinedError) Unwrap() error { return e.Cause }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// runState is a subscription's runtime state, as the manager last saw it.
type runState struct {
	state string
	err   string
}

func (svc *Service) setState(subID, state, errText string) {
	svc.stateMu.Lock()
	defer svc.stateMu.Unlock()
	svc.states[subID] = runState{state: state, err: errText}
}

func (svc *Service) runtimeState(subID string, enabled bool) (string, string) {
	if !enabled {
		return "disabled", ""
	}
	svc.stateMu.Lock()
	defer svc.stateMu.Unlock()
	st, ok := svc.states[subID]
	if !ok {
		return "stopped", ""
	}
	return st.state, st.err
}

// Manager runs the durable consumers and core NATS subscriptions of every enabled subscription (one goroutine set for
// the process; SDK consumers are push-driven, nothing polls). Configuration changes wake it; failed connections and
// subscriptions are retried periodically, and every round refreshes the ingestion gauges.
type Manager struct {
	svc   *Service
	log   *slog.Logger
	retry time.Duration
	conns map[string]*connRun
	subs  map[string]*subRun
	limMu sync.Mutex
	lims  map[string]*limiter
	// coreMu guards core, the running core subscriptions by their NATS subscription (read by NATS's error callback).
	coreMu sync.Mutex
	core   map[*nats.Subscription]*subRun
	// sources are the inbound event sources by delivery mode (SM-ARCH-003).
	sources map[string]Source
}

type limiter struct {
	perMinute int64
	l         *rate.Limiter
}

// throttleWait bounds how long one event waits for the subscription's ingestion rate before it is left in the
// stream (negative acknowledgement, redelivered later): well below the stream's acknowledgement timeout.
const throttleWait = 10 * time.Second

// throttle holds an event until the subscription's ingestion rate allows it (limits model, ADR-0036). For a durable
// subscription nothing is dropped: an event that cannot be admitted within throttleWait stays in the stream. A core
// NATS message that cannot be admitted is dropped (nothing could redeliver it).
func (m *Manager) throttle(ctx context.Context, subID, spaceID string, core bool) error {
	if m.svc.o.RateFor == nil {
		return nil
	}
	n := m.svc.o.RateFor(ctx, spaceID)
	if n <= 0 {
		return nil
	}
	m.limMu.Lock()
	lim := m.lims[subID]
	if lim == nil || lim.perMinute != n {
		lim = &limiter{perMinute: n, l: rate.NewLimiter(rate.Limit(float64(n)/60), int(max(1, n/6)))}
		m.lims[subID] = lim
	}
	m.limMu.Unlock()
	wctx, cancel := context.WithTimeout(ctx, throttleWait)
	defer cancel()
	if err := lim.l.Wait(wctx); err != nil {
		msg := fmt.Sprintf("throttled: over the ingestion rate of %d events per minute; events wait in the stream", n)
		if core {
			msg = fmt.Sprintf("throttled: over the ingestion rate of %d events per minute; core NATS messages over it are dropped", n)
		}
		m.svc.setState(subID, "running", msg)
		return errThrottled
	}
	return nil
}

var errThrottled = errors.New("ingestion rate limit: left in the stream")

type connRun struct {
	version int64
	client  *pulse.Client
	row     db.EventConnection
	err     error
}

// subRun is a running subscription: a durable consumer (sub) or a core NATS subscription (nsub).
type subRun struct {
	key     string
	id      string
	subject string
	stop    context.CancelFunc
	// Durable.
	sub    *pulse.Subscription
	js     jetstream.JetStream
	stream string
	// Core.
	nsub    *nats.Subscription
	nc      *nats.Conn
	dropped int
	// failed is set (by NATS's error callback) when the server refused the core subscription; the next round retries it.
	failed atomic.Bool
}

func (r *subRun) halt() {
	r.stop()
	if r.sub != nil {
		r.sub.Stop()
	}
	if r.nsub != nil {
		_ = r.nsub.Unsubscribe() // fails harmlessly when the connection was already closed
	}
}

// NewManager returns the ingestion manager.
func NewManager(svc *Service, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	m := &Manager{svc: svc, log: obs.Component(log, "events"), retry: 30 * time.Second, conns: map[string]*connRun{}, subs: map[string]*subRun{},
		lims: map[string]*limiter{}, core: map[*nats.Subscription]*subRun{}}
	m.sources = map[string]Source{DeliveryDurable: durableSource{m}, DeliveryCore: coreSource{m}}
	return m
}

// Run reconciles consumers until ctx ends.
func (m *Manager) Run(ctx context.Context) {
	t := time.NewTicker(m.retry)
	defer t.Stop()
	defer m.stopAll()
	for {
		m.reconcile(ctx)
		select {
		case <-ctx.Done():
			return
		case <-m.svc.changed:
		case <-t.C:
		}
	}
}

func (m *Manager) stopAll() {
	for id := range m.subs {
		m.stopSub(id)
	}
	for id, c := range m.conns {
		if c.client != nil {
			c.client.Close()
		}
		delete(m.conns, id)
	}
}

func (m *Manager) reconcile(ctx context.Context) {
	conns, err := m.svc.s.R().ListEventConnections(ctx)
	if err != nil {
		m.log.Warn("list connections", obs.Err(err))
		return
	}
	live := map[string]bool{}
	for _, c := range conns {
		live[c.ID] = true
		cur := m.conns[c.ID]
		if cur != nil && cur.version == c.Version && (cur.client != nil || c.Enabled == 0) {
			continue
		}
		if cur != nil && cur.client != nil {
			cur.client.Close()
		}
		run := &connRun{version: c.Version, row: c}
		if c.Enabled == 1 {
			run.client, run.err = m.connect(c)
			if run.err != nil {
				m.log.Warn("event connection failed", slog.String("connection", c.Name), obs.Err(run.err))
			}
		}
		m.conns[c.ID] = run
	}
	for id, c := range m.conns {
		if !live[id] {
			if c.client != nil {
				c.client.Close()
			}
			delete(m.conns, id)
		}
	}
	subs, err := m.svc.s.R().ListEventSubscriptions(ctx)
	if err != nil {
		m.log.Warn("list subscriptions", obs.Err(err))
		return
	}
	pausedIDs, err := m.svc.s.R().ListPausedSpaceIDs(ctx)
	if err != nil {
		m.log.Warn("list paused spaces", obs.Err(err))
		return
	}
	paused := map[string]bool{}
	for _, id := range pausedIDs {
		paused[id] = true
	}
	want := map[string]bool{}
	for _, s := range subs {
		c := m.conns[s.ConnectionID]
		if s.Enabled == 0 || c == nil || c.row.Enabled == 0 {
			continue
		}
		if s.WhilePaused == PausedStop && paused[s.SpaceID] {
			// Stopped until the space resumes (SM-EVT-014); a durable consumer keeps its position on the server.
			m.stopSub(s.ID)
			m.svc.setState(s.ID, "paused", "")
			continue
		}
		key := fmt.Sprintf("%d|%s|%d|%d", c.version, s.EffectiveSubject, s.CursorGeneration, s.Version)
		if c.client == nil {
			m.svc.setState(s.ID, "error", "connection: "+errText(c.err))
			continue
		}
		want[s.ID] = true
		if cur := m.subs[s.ID]; cur != nil && cur.key == key && !cur.failed.Load() {
			continue
		}
		m.stopSub(s.ID)
		src := m.sources[s.Delivery]
		if src == nil {
			m.svc.setState(s.ID, "error", fmt.Sprintf("no inbound event source for delivery %q", s.Delivery))
			continue
		}
		run, err := src.Start(ctx, c, s, key)
		if err != nil {
			m.svc.setState(s.ID, "error", err.Error())
			m.log.Warn("event subscription failed", slog.String("subscription", s.ID), obs.Err(err))
			continue
		}
		m.subs[s.ID] = run
		m.svc.setState(s.ID, "running", "")
	}
	for id := range m.subs {
		if !want[id] {
			m.stopSub(id)
			m.svc.setState(id, "stopped", "")
		}
	}
	m.refreshMetrics(ctx, subs)
}

func errText(err error) string {
	if err == nil {
		return "not connected"
	}
	return err.Error()
}

func (m *Manager) stopSub(id string) {
	if s := m.subs[id]; s != nil {
		s.halt()
		delete(m.subs, id)
		if s.nsub != nil {
			m.coreMu.Lock()
			delete(m.core, s.nsub)
			m.coreMu.Unlock()
		}
	}
}

func (m *Manager) connect(c db.EventConnection) (*pulse.Client, error) {
	creds, err := m.svc.credentials(c)
	if err != nil {
		return nil, err
	}
	opts := []pulse.Option{pulse.WithCreds(creds), pulse.WithStream(c.Stream), pulse.WithName("spaceman"),
		pulse.WithNATSOptions(nats.ErrorHandler(m.natsError))}
	if c.CaPem != "" {
		opts = append(opts, pulse.WithCA([]byte(c.CaPem)))
	}
	return pulse.Connect(c.Url, opts...)
}

// start runs a subscription's durable consumer. A pending cursor change deletes the consumer first, so the SDK
// creates it again at the chosen position (ADR-0011: an existing durable is bound without updating its config).
func (m *Manager) start(ctx context.Context, c *connRun, s db.EventSubscription, key string) (*subRun, error) {
	opts := []pulse.SubOption{pulse.Durable(s.Durable), pulse.FromStream(c.row.Stream)}
	var boundary time.Time
	if s.AppliedGeneration < s.CursorGeneration {
		dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := c.client.JetStream().DeleteConsumer(dctx, c.row.Stream, c.client.ClientID()+"_"+s.Durable)
		cancel()
		if err != nil && !errors.Is(err, jetstream.ErrConsumerNotFound) {
			return nil, fmt.Errorf("reset consumer: %w", err)
		}
		switch s.StartPolicy {
		case StartNew:
			opts = append(opts, pulse.OnlyNew())
		case StartTime:
			opts = append(opts, pulse.Since(fromMicros(s.StartTime.Int64)))
		case StartSeq:
			opts = append(opts, pulse.FromSeq(uint64(s.StartSeq.Int64))) //nolint:gosec // stored from a uint64
		}
		if s.Replay == 1 {
			boundary = m.svc.o.Clock.Now()
		}
	}
	// A replay keeps its boundary across restarts: events stored before it are replayed ones, whenever they arrive.
	var rp *db.SubscriptionReplay
	if r, err := m.svc.s.R().GetSubscriptionReplayByGeneration(ctx, db.GetSubscriptionReplayByGenerationParams{SubscriptionID: s.ID, Generation: s.CursorGeneration}); err == nil {
		rp = &r
		if r.BoundaryAt.Valid {
			boundary = fromMicros(r.BoundaryAt.Int64)
		} else if boundary.IsZero() {
			boundary = m.svc.o.Clock.Now()
		}
	}
	origin := Origin{}
	if rp != nil {
		origin = Origin{Kind: OriginReplay, ReplayID: rp.ID, Fresh: rp.FreshItems == 1}
	} else if !boundary.IsZero() {
		origin = Origin{Kind: OriginReplay}
	}
	spaceID := s.SpaceID
	sctx, stop := context.WithCancel(ctx)
	subID := s.ID
	opts = append(opts, pulse.OnError(func(_ pulse.Event, err error) {
		var qe *QuarantinedError
		if errors.As(err, &qe) {
			return // expected and recorded in quarantine, not a subscription error
		}
		m.svc.setState(subID, "running", err.Error())
	}))
	sub, err := c.client.Subscribe(sctx, s.EffectiveSubject, func(hctx context.Context, e pulse.Event) error {
		if m.svc.ownEvent(e) {
			return m.svc.skipOwn(hctx, subID, fromPulse(e), e.Sequence)
		}
		if err := m.throttle(hctx, subID, spaceID, false); err != nil {
			m.svc.observe(DeliveryDurable, OriginLive, "", err, -1)
			return err // left in the stream: redelivered after a backoff
		}
		o := Origin{}
		if !boundary.IsZero() && e.StreamTime.Before(boundary) {
			o = origin
		}
		return m.svc.Process(hctx, subID, fromPulse(e), e.Sequence, o)
	}, opts...)
	if err != nil {
		stop()
		return nil, err
	}
	if s.AppliedGeneration < s.CursorGeneration {
		if err := m.svc.s.W().TxNamed(ctx, "events.cursor_applied", func(tx *store.Tx) error {
			if rp != nil {
				if err := tx.Q.SetSubscriptionReplayBoundary(ctx, db.SetSubscriptionReplayBoundaryParams{BoundaryAt: sql.NullInt64{Int64: boundary.UnixMicro(), Valid: true},
					SubscriptionID: s.ID, Generation: s.CursorGeneration}); err != nil {
					return err
				}
			}
			return tx.Q.MarkSubscriptionApplied(ctx, db.MarkSubscriptionAppliedParams{AppliedGeneration: s.CursorGeneration, ID: s.ID})
		}); err != nil {
			sub.Stop()
			stop()
			return nil, err
		}
	}
	return &subRun{key: key, id: s.ID, subject: s.EffectiveSubject, sub: sub, stop: stop, js: c.client.JetStream(), stream: c.row.Stream}, nil
}
