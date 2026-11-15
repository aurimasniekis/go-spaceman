// Package events connects spaces to Pulse (SM-EVT-*, ADR-0011): connections hold how to reach a Pulse deployment,
// templates (a versioned central catalog) say how to select and present events, and subscriptions bind a template
// version (or a custom definition) to a space with variable values and a cursor. The ingestion manager runs one durable
// consumer per subscription, so every subscribed space receives its own copy of an event (SM-EVT-012), and acknowledges
// an event only after its inbox item is committed or its disposition recorded (SM-EVT-011). A subscription can instead
// use a plain NATS Core subscription (delivery "core", ADR-0042) for sources that publish without JetStream: the same
// filtering, deduplication, quarantine and inbox path, but non-durable (no acknowledgement, redelivery or replay, and
// messages published while SpaceMan is not connected are lost).
package events

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/attachments"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/events/template"
	"github.com/aurimasniekis/go-spaceman/internal/inbox"
	"github.com/aurimasniekis/go-spaceman/internal/keyring"
	"github.com/aurimasniekis/go-spaceman/internal/obs/metrics"
	"github.com/aurimasniekis/go-spaceman/internal/store"
)

// Options configure the service.
type Options struct {
	Clock clock.Clock
	// DeliveryRetention bounds the delivery history.
	DeliveryRetention time.Duration
	// Origin is this installation's outbound source while it publishes its own events (SM-EVT-016): an inbound event
	// whose spaceman-origin header equals it is acknowledged and recorded as filtered, never turned into an item, so
	// SpaceMan does not re-ingest what it published. Empty: no check.
	Origin string
	// RateFor returns the ingestion rate (events per minute and subscription) of a space (the limits model,
	// ADR-0036). Nil or 0: no rate limit.
	RateFor func(ctx context.Context, spaceID string) int64
	// DepthFor returns a space's automation depth limit (the limit event_depth, ADR-0049): an event whose
	// spaceman-depth is at or above it is quarantined instead of becoming an item (SM-EVT-016). Nil or 0:
	// DefaultMaxDepth.
	DepthFor func(ctx context.Context, spaceID string) int64
	// Metrics receives the ingestion metrics (bounded labels only). Nil: none.
	Metrics *metrics.Metrics
	// Log receives the service's warnings. Nil: discarded.
	Log *slog.Logger
	// OnAccepted, when set, is called after a live or redelivered event became a new inbox item, with the slug of the
	// built-in template its subscription uses ("" for a custom one). The event is untrusted: a hook may only use it as
	// a hint (the gateway re-check reads Harbor Pilot again on quota.window.recovered, ADR-0079 decision 9).
	OnAccepted func(templateSlug string, e template.Event)
}

// Service manages connections, templates and subscriptions, and processes delivered events.
type Service struct {
	s       *store.Store
	kr      *keyring.Keyring
	audit   *audit.Writer
	changes *changefeed.Recorder
	inbox   *inbox.Service
	att     *attachments.Service
	o       Options
	// changed wakes the ingestion manager after configuration changes.
	changed chan struct{}
	stateMu sync.Mutex
	states  map[string]runState
}

// New returns the service.
func New(s *store.Store, kr *keyring.Keyring, aw *audit.Writer, ch *changefeed.Recorder, ib *inbox.Service, att *attachments.Service, o Options) *Service {
	if o.Clock == nil {
		o.Clock = clock.System{}
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.DeliveryRetention <= 0 {
		o.DeliveryRetention = 30 * 24 * time.Hour
	}
	return &Service{s: s, kr: kr, audit: aw, changes: ch, inbox: ib, att: att, o: o, changed: make(chan struct{}, 1), states: map[string]runState{}}
}

// Changed is signalled (coalesced) whenever connections or subscriptions change.
func (svc *Service) Changed() <-chan struct{} { return svc.changed }

// SpaceStateChanged wakes the ingestion manager after a space was paused or resumed, so subscriptions that stop while
// their space is paused (while_paused stop) stop or start again at once.
func (svc *Service) SpaceStateChanged() { svc.notify() }

func (svc *Service) notify() {
	select {
	case svc.changed <- struct{}{}:
	default:
	}
}

func notFound(what string) error { return apperr.New(apperr.CodeNotFound, "%s not found", what) }

func fromMicros(v int64) time.Time { return time.UnixMicro(v).UTC() }

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// TemplateRef identifies the definition a subscription uses.
type TemplateRef struct {
	TemplateID   string `json:"template_id,omitempty"`
	TemplateSlug string `json:"template_slug,omitempty"`
	VersionID    string `json:"version_id,omitempty"`
	Version      int64  `json:"version,omitempty"`
}

func jsonUnmarshal(s string, v any) error { return json.Unmarshal([]byte(s), v) }
