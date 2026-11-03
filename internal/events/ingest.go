package events

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/aurimasniekis/go-pulse/pkg/pulse"
	"github.com/nats-io/nats.go/jetstream"
	"golang.org/x/time/rate"

	"github.com/aurimasniekis/go-spaceman/internal/attachments"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/events/template"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/inbox"
	"github.com/aurimasniekis/go-spaceman/internal/obs"
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
	return template.Event{ID: e.ID, Subject: e.Subject, Type: e.Type, Source: e.Source, ContentType: e.ContentType, Time: e.Time, Data: e.Data}
}

// Process handles one delivered event for a subscription and says how to acknowledge it (SM-EVT-011/013):
//   - nil: the inbox item is committed, or the event was filtered or is a duplicate: ack;
//   - pulse.Terminal: the event is invalid and recorded in quarantine: terminate, never redeliver;
//   - any other error: transient (storage): nak, the SDK backs off and redelivers.
//
// The origin says why the event is handled; every delivery records it with its attempt number, so a replay is never
// confused with a retry (SM-EVT-015).
func (svc *Service) Process(ctx context.Context, subID string, e template.Event, seq uint64, o Origin) error {
	r, err := svc.s.R().GetEventSubscription(ctx, subID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // deleted while consuming
	} else if err != nil {
		return err
	}
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
		return svc.deliver(ctx, r, e, seq, o, OutcomeFiltered, "", res.SkipReason)
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
		CorrelationID: res.Correlation, DedupKey: dedup, Subscription: r.ID, Urgency: res.Urgency, Trusted: trusted, CausationID: e.ID}
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
	outcome := OutcomeAccepted
	switch {
	case !created:
		outcome = OutcomeDuplicate
	case o.Kind == OriginReplay:
		outcome = OutcomeReplayed
	}
	return svc.deliver(ctx, r, e, seq, o, outcome, it.ID, "")
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
	return svc.deliver(ctx, r, e, seq, Origin{Kind: OriginLive, attempt: 1}, OutcomeFiltered, "", OwnOriginDetail)
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
	err := svc.s.W().TxNamed(ctx, "events.quarantine", func(tx *store.Tx) error {
		if _, err := tx.Q.InsertQuarantine(ctx, db.InsertQuarantineParams{ID: ids.New(ids.Quarantine), SubscriptionID: r.ID, EventID: e.ID, Subject: e.Subject,
			Type: e.Type, Source: e.Source, EventTime: e.Time.UnixMicro(), ContentType: e.ContentType, Payload: append([]byte{}, payload...),
			Error: truncate(cause.Error(), 2000), CreatedAt: tx.NowMicros()}); err != nil {
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

// Manager runs the durable consumers of every enabled subscription (one goroutine set for the process; SDK consumers
// are push-driven, nothing polls). Configuration changes wake it; failed connections are retried periodically.
type Manager struct {
	svc   *Service
	log   *slog.Logger
	retry time.Duration
	conns map[string]*connRun
	subs  map[string]*subRun
	limMu sync.Mutex
	lims  map[string]*limiter
}

type limiter struct {
	perMinute int64
	l         *rate.Limiter
}

// throttleWait bounds how long one event waits for the subscription's ingestion rate before it is left in the
// stream (negative acknowledgement, redelivered later): well below the stream's acknowledgement timeout.
const throttleWait = 10 * time.Second

// throttle holds an event until the subscription's ingestion rate allows it (limits model, ADR-0036). Nothing is
// dropped: an event that cannot be admitted within throttleWait stays in the durable stream.
func (m *Manager) throttle(ctx context.Context, subID, spaceID string) error {
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
		m.svc.setState(subID, "running", fmt.Sprintf("throttled: over the ingestion rate of %d events per minute; events wait in the stream", n))
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

type subRun struct {
	key  string
	sub  *pulse.Subscription
	stop context.CancelFunc
}

// NewManager returns the ingestion manager.
func NewManager(svc *Service, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Manager{svc: svc, log: obs.Component(log, "events"), retry: 30 * time.Second, conns: map[string]*connRun{}, subs: map[string]*subRun{},
		lims: map[string]*limiter{}}
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
	for id, s := range m.subs {
		s.stop()
		s.sub.Stop()
		delete(m.subs, id)
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
	want := map[string]bool{}
	for _, s := range subs {
		c := m.conns[s.ConnectionID]
		if s.Enabled == 0 || c == nil || c.row.Enabled == 0 {
			continue
		}
		key := fmt.Sprintf("%d|%s|%d|%d", c.version, s.EffectiveSubject, s.CursorGeneration, s.Version)
		if c.client == nil {
			m.svc.setState(s.ID, "error", "connection: "+errText(c.err))
			continue
		}
		want[s.ID] = true
		if cur := m.subs[s.ID]; cur != nil && cur.key == key {
			continue
		}
		m.stopSub(s.ID)
		if err := m.start(ctx, c, s, key); err != nil {
			m.svc.setState(s.ID, "error", err.Error())
			m.log.Warn("event subscription failed", slog.String("subscription", s.ID), obs.Err(err))
			continue
		}
		m.svc.setState(s.ID, "running", "")
	}
	for id := range m.subs {
		if !want[id] {
			m.stopSub(id)
			m.svc.setState(id, "stopped", "")
		}
	}
}

func errText(err error) string {
	if err == nil {
		return "not connected"
	}
	return err.Error()
}

func (m *Manager) stopSub(id string) {
	if s := m.subs[id]; s != nil {
		s.stop()
		s.sub.Stop()
		delete(m.subs, id)
	}
}

func (m *Manager) connect(c db.EventConnection) (*pulse.Client, error) {
	creds, err := m.svc.credentials(c)
	if err != nil {
		return nil, err
	}
	opts := []pulse.Option{pulse.WithCreds(creds), pulse.WithStream(c.Stream), pulse.WithName("spaceman")}
	if c.CaPem != "" {
		opts = append(opts, pulse.WithCA([]byte(c.CaPem)))
	}
	return pulse.Connect(c.Url, opts...)
}

// start runs a subscription's durable consumer. A pending cursor change deletes the consumer first, so the SDK
// creates it again at the chosen position (ADR-0011: an existing durable is bound without updating its config).
func (m *Manager) start(ctx context.Context, c *connRun, s db.EventSubscription, key string) error {
	opts := []pulse.SubOption{pulse.Durable(s.Durable), pulse.FromStream(c.row.Stream)}
	var boundary time.Time
	if s.AppliedGeneration < s.CursorGeneration {
		dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := c.client.JetStream().DeleteConsumer(dctx, c.row.Stream, c.client.ClientID()+"_"+s.Durable)
		cancel()
		if err != nil && !errors.Is(err, jetstream.ErrConsumerNotFound) {
			return fmt.Errorf("reset consumer: %w", err)
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
		if err := m.throttle(hctx, subID, spaceID); err != nil {
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
		return err
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
			return err
		}
	}
	m.subs[s.ID] = &subRun{key: key, sub: sub, stop: stop}
	return nil
}
