package events

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/aurimasniekis/go-pulse/pkg/pulse"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/aurimasniekis/go-spaceman/internal/attachments"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/events/template"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/inbox"
	"github.com/aurimasniekis/go-spaceman/internal/obs"
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
func (svc *Service) Process(ctx context.Context, subID string, e template.Event, seq uint64, replay bool) error {
	r, err := svc.s.R().GetEventSubscription(ctx, subID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // deleted while consuming
	} else if err != nil {
		return err
	}
	def, ref, err := svc.definitionOf(ctx, svc.s.R(), r)
	if err != nil {
		return err
	}
	vars := map[string]string{}
	_ = jsonUnmarshal(r.VarsJson, &vars)
	bound, err := def.Bind(vars)
	if err != nil {
		return svc.quarantine(ctx, r, e, seq, fmt.Errorf("the subscription no longer binds: %w", err))
	}
	res, err := bound.Apply(e)
	if errors.Is(err, template.ErrInvalid) {
		return svc.quarantine(ctx, r, e, seq, err)
	} else if err != nil {
		return err
	}
	if !res.Accepted {
		return svc.deliver(ctx, r, e, seq, OutcomeFiltered, "", res.SkipReason)
	}
	trusted := map[string]any{"event_id": e.ID, "subject": e.Subject, "type": e.Type, "source": e.Source, "time": e.Time.UTC().Format(time.RFC3339Nano),
		"stream_seq": seq, "subscription_id": r.ID, "subscription": r.Name, "connection_id": r.ConnectionID, "instructions": res.Instructions}
	if ref != nil {
		trusted["template_id"], trusted["template_slug"], trusted["template_version"] = ref.TemplateID, ref.TemplateSlug, ref.Version
	} else {
		trusted["template"] = "custom"
	}
	in := inbox.CreateInput{SourceKind: "event", SourceID: r.ID, SourceLabel: r.Name, Title: res.Title, ContentType: inbox.Markdown, Body: res.Body,
		CorrelationID: res.Correlation, DedupKey: r.ID + ":" + e.ID, Subscription: r.ID, Urgency: res.Urgency, Trusted: trusted}
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
	case replay:
		outcome = OutcomeReplayed
	}
	return svc.deliver(ctx, r, e, seq, outcome, it.ID, "")
}

func (svc *Service) deliver(ctx context.Context, r db.EventSubscription, e template.Event, seq uint64, outcome, itemID, detail string) error {
	return svc.s.W().TxNamed(ctx, "events.delivery", func(tx *store.Tx) error {
		if err := tx.Q.InsertEventDelivery(ctx, db.InsertEventDeliveryParams{SubscriptionID: r.ID, EventID: e.ID, Subject: e.Subject, Type: e.Type,
			StreamSeq: int64(seq), Outcome: outcome, InboxItemID: itemID, Detail: truncate(detail, 1000), At: tx.NowMicros()}); err != nil { //nolint:gosec // stream sequences fit int64
			return err
		}
		return tx.Q.TouchSubscription(ctx, db.TouchSubscriptionParams{LastEventAt: sql.NullInt64{Int64: tx.NowMicros(), Valid: true}, ID: r.ID})
	})
}

func (svc *Service) quarantine(ctx context.Context, r db.EventSubscription, e template.Event, seq uint64, cause error) error {
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
			StreamSeq: int64(seq), Outcome: OutcomeQuarantined, Detail: truncate(cause.Error(), 1000), At: tx.NowMicros()}); err != nil { //nolint:gosec // stream sequences fit int64
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
}

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
	return &Manager{svc: svc, log: obs.Component(log, "events"), retry: 30 * time.Second, conns: map[string]*connRun{}, subs: map[string]*subRun{}}
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
			boundary = time.Now()
		}
	}
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
		replay := !boundary.IsZero() && e.StreamTime.Before(boundary)
		return m.svc.Process(hctx, subID, fromPulse(e), e.Sequence, replay)
	}, opts...)
	if err != nil {
		stop()
		return err
	}
	if s.AppliedGeneration < s.CursorGeneration {
		if err := m.svc.s.W().TxNamed(ctx, "events.cursor_applied", func(tx *store.Tx) error {
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
