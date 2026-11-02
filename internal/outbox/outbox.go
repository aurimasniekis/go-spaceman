// Package outbox publishes SpaceMan's own events to Pulse through a transactional outbox (SM-OBS-004/005, ADR-0015,
// ADR-0030).
//
// Derived from github.com/aurimasniekis/go-hallpass internal/events (event_outbox recorder and relay) and the wake
// pattern of internal/outbox (Apache-2.0, same author), with Porch's event naming.
//
// A service that changes state calls Enqueue with its write transaction; the event row commits or rolls back with the
// change, and nothing touches the network inside the transaction. After the commit, a kick wakes the relay, which
// publishes due rows with the go-pulse SDK, using the row ID as the CloudEvents ID and Nats-Msg-Id, so a re-publish
// after a crash or a lost acknowledgement is deduplicated by JetStream (2-minute window) and by consumers beyond it.
// Delivery is at least once. A failed publish backs off exponentially (with jitter, capped) and the row is
// dead-lettered after MaxAttempts attempts or MaxAge. The relay sleeps on the kick channel and one timer set to the next
// retry: it never polls.
//
// Payloads are the versioned catalog in catalog.go: identifiers, states and outcomes only. Correlation and causation
// travel in SpaceMan extension headers (Pulse defines neither); spaceman-origin lets the inbound side drop SpaceMan's own
// events instead of re-ingesting them (SM-EVT-016).
package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sync"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/obs"
	"github.com/aurimasniekis/go-spaceman/internal/obs/metrics"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// SpaceMan extension headers (documented in docs/contracts/pulse.md; not part of the Pulse contract).
const (
	// HeaderCorrelation groups events that belong to one piece of work (a task, run, human action, approval…).
	HeaderCorrelation = "spaceman-correlation-id"
	// HeaderCausation is the ID of what caused the event: an inbound Pulse event, a node command, an API request or
	// the SpaceMan record that triggered it.
	HeaderCausation = "spaceman-causation-id"
	// HeaderOrigin is the publishing SpaceMan's source name; the inbound side drops events carrying its own origin.
	HeaderOrigin = "spaceman-origin"
)

// Defaults of the retry and retention policy (ADR-0015, ADR-0030).
const (
	DefaultRetention   = 7 * 24 * time.Hour
	DefaultMaxAttempts = 50
	DefaultMaxAge      = 24 * time.Hour
	DefaultBaseBackoff = time.Second
	DefaultMaxBackoff  = 5 * time.Minute
	DefaultTimeout     = 10 * time.Second
	DefaultSource      = "spaceman"
	batch              = 100
)

// Options configure an Outbox.
type Options struct {
	// Enabled turns enqueueing on. While it is off Enqueue records nothing (events of that time are never published).
	Enabled bool
	// Source is ce-source and spaceman-origin (default "spaceman").
	Source string
	// URL is shown in the status (never credentials; config validation refuses userinfo).
	URL         string
	Clock       clock.Clock
	Metrics     *metrics.Metrics
	Log         *slog.Logger
	Retention   time.Duration
	MaxAttempts int
	MaxAge      time.Duration
	BaseBackoff time.Duration
	MaxBackoff  time.Duration
	// Timeout bounds one publish (waiting for the JetStream acknowledgement).
	Timeout time.Duration
	// ReconnectInterval is how long Run waits before dialing Pulse again (default 30 s).
	ReconnectInterval time.Duration
}

// Outbox records events in write transactions and relays them to Pulse.
type Outbox struct {
	s    *store.Store
	o    Options
	log  *slog.Logger
	kick chan struct{}

	mu        sync.Mutex
	connected bool
	lastErr   string
	lastErrAt time.Time
	published int64 // since start
	failed    int64 // failed attempts since start
}

// New returns an outbox. A nil *Outbox is valid: it records nothing.
func New(s *store.Store, o Options) *Outbox {
	if o.Clock == nil {
		o.Clock = clock.System{}
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.Source == "" {
		o.Source = DefaultSource
	}
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&o.Retention, DefaultRetention)
	def(&o.MaxAge, DefaultMaxAge)
	def(&o.BaseBackoff, DefaultBaseBackoff)
	def(&o.MaxBackoff, DefaultMaxBackoff)
	def(&o.Timeout, DefaultTimeout)
	def(&o.ReconnectInterval, 30*time.Second)
	if o.MaxAttempts <= 0 {
		o.MaxAttempts = DefaultMaxAttempts
	}
	return &Outbox{s: s, o: o, log: obs.Component(o.Log, "outbox"), kick: make(chan struct{}, 1)}
}

// Enabled reports whether events are recorded.
func (o *Outbox) Enabled() bool { return o != nil && o.o.Enabled }

// Source is the configured ce-source (and spaceman-origin).
func (o *Outbox) Source() string {
	if o == nil {
		return ""
	}
	return o.o.Source
}

// Event is one event to record.
type Event struct {
	// Payload is a catalog payload (catalog.go); its type decides the subject and ce-type.
	Payload Payload
	// Correlation and Causation are optional IDs (see HeaderCorrelation and HeaderCausation). Values that are not
	// plain identifiers are dropped. Without a causation, the API request or node command of ctx is used.
	Correlation string
	Causation   string
}

// reHeaderID admits identifier-like header values: no spaces, controls or header-breaking characters.
var reHeaderID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/#@+=-]{0,127}$`)

// CleanID returns s if it is a plain identifier fit for a header, else "".
func CleanID(s string) string {
	if reHeaderID.MatchString(s) {
		return s
	}
	return ""
}

// ErrInvalid is returned for an event that is not in the catalog or does not form a valid Pulse subject.
var ErrInvalid = errors.New("outbox: invalid event")

// Enqueue records ev inside tx and returns its stable ID; the relay is kicked after the commit. With publication
// disabled it records nothing and returns "". Enqueue does no I/O besides the insert.
func (o *Outbox) Enqueue(ctx context.Context, tx *store.Tx, ev Event) (string, error) {
	if !o.Enabled() {
		return "", nil
	}
	if ev.Payload == nil {
		return "", fmt.Errorf("%w: no payload", ErrInvalid)
	}
	n := ev.Payload.eventName()
	if err := n.validate(); err != nil {
		return "", err
	}
	b, err := json.Marshal(ev.Payload)
	if err != nil {
		return "", err
	}
	causation := CleanID(ev.Causation)
	if causation == "" {
		causation = CleanID(obs.ID(ctx, obs.FieldHTTPRequestID))
	}
	if causation == "" {
		causation = CleanID(obs.ID(ctx, obs.FieldCommandID))
	}
	id := ids.New(ids.Event)
	now := tx.NowMicros()
	if err := tx.Q.InsertEventOutbox(ctx, db.InsertEventOutboxParams{ID: id, Subject: n.Subject(), Type: n.Type(), SpaceID: ev.Payload.spaceID(),
		CorrelationID: CleanID(ev.Correlation), CausationID: causation, PayloadJson: string(b), CreatedAt: now, NextAttemptAt: now}); err != nil {
		return "", err
	}
	tx.AfterCommit(o.Kick)
	return id, nil
}

// Kick wakes the relay (non-blocking, coalesced).
func (o *Outbox) Kick() {
	if o == nil {
		return
	}
	select {
	case o.kick <- struct{}{}:
	default:
	}
}
