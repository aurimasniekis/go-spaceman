// Package fleet is the central side of node management and execution (SM-NODE-*, SM-SPACE-001…004, ADR-0006):
// node enrollment and credentials, pinned spaces, execution leases with generation fencing, runs (desired vs
// observed state) and the outbound-connection hub that delivers commands and ingests node reports.
//
// Central is the only authority: every run is created here, pinned to the space's node and the space's current lease
// generation; nodes execute and report. Disconnected execution is "unknown", never assumed failed (SM-NODE-005).
//
// The lease generation is the fencing epoch of a space's assignment. It increases only when the assignment authority
// changes (relocation, restore, explicit revocation), not on a plain expiry: a node that reconnects after a short
// outage gets its same-generation lease renewed and its runs stay valid, while runs of an older epoch are fenced.
package fleet

import (
	"context"
	"log/slog"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/eventbus"
	"github.com/aurimasniekis/go-spaceman/internal/keyring"
	"github.com/aurimasniekis/go-spaceman/internal/obs/metrics"
	"github.com/aurimasniekis/go-spaceman/internal/outbox"
	"github.com/aurimasniekis/go-spaceman/internal/store"
)

// Options configure the service.
type Options struct {
	HeartbeatInterval time.Duration
	LeaseTTL          time.Duration
	DisconnectGrace   time.Duration
	EnrollmentTTL     time.Duration
	Clock             clock.Clock
	Log               *slog.Logger
	Metrics           *metrics.Metrics
	// RecordingsDir stores central copies of node recordings (empty: <none>, recordings are not accepted).
	RecordingsDir string
	// ResolveAccount picks the account a launch uses inside the launch transaction (SM-ACCT-003); it returns the
	// account ID stored with the run. Nil: launches carry no account.
	ResolveAccount func(ctx context.Context, tx *store.Tx, spaceID, harness, ref string) (string, error)
	// ResolveLaunch fills a launch from the space's configured role and catalogs (kind, harness, model, settings, system
	// text) before validation, and returns what the run manifest records about it (SM-PROMPT-003/004). Nil: launches are
	// used as given.
	ResolveLaunch func(ctx context.Context, spaceID string, in *LaunchInput) (map[string]any, error)
	// OnRunEnded is called after a run was recorded as ended (exited, failed or lost).
	OnRunEnded func(runID string)
	// OnRunIdle is called after a run's program reported idle or done (turn finished): pending deliveries can go out.
	OnRunIdle func(runID string)
	// OnCommandAck is called inside the transaction that records a node's command acknowledgement (with the command's
	// kind and the node's reason).
	OnCommandAck func(ctx context.Context, tx *store.Tx, commandID, kind, result, reason string) error
	// Outbox records run lifecycle events for publication to Pulse (SM-OBS-004). Nil: nothing is published.
	Outbox *outbox.Outbox
}

// Service manages nodes, spaces, leases and runs.
type Service struct {
	s       *store.Store
	kr      *keyring.Keyring
	audit   *audit.Writer
	bus     *eventbus.Bus
	changes *changefeed.Recorder
	opts    Options
	hub     *Hub
	rec     *Recordings
}

// New builds the service and its hub.
func New(s *store.Store, kr *keyring.Keyring, aw *audit.Writer, bus *eventbus.Bus, ch *changefeed.Recorder, o Options) *Service {
	if o.HeartbeatInterval <= 0 {
		o.HeartbeatInterval = 10 * time.Second
	}
	if o.LeaseTTL <= 0 {
		o.LeaseTTL = 45 * time.Second
	}
	if o.DisconnectGrace < 0 {
		o.DisconnectGrace = 0
	}
	if o.EnrollmentTTL <= 0 {
		o.EnrollmentTTL = 24 * time.Hour
	}
	if o.Clock == nil {
		o.Clock = clock.System{}
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	svc := &Service{s: s, kr: kr, audit: aw, bus: bus, changes: ch, opts: o}
	svc.hub = newHub(svc)
	svc.rec = newRecordings(o.RecordingsDir, o.Clock.Now)
	return svc
}

// Recordings returns central's recording store.
func (svc *Service) Recordings() *Recordings { return svc.rec }

// Hub returns the node connection hub.
func (svc *Service) Hub() *Hub { return svc.hub }

func (svc *Service) now() time.Time { return svc.opts.Clock.Now().UTC() }

func (svc *Service) record(ctx context.Context, tx *store.Tx, spaceID, typ, id, action string, version int64) error {
	if svc.changes == nil {
		return nil
	}
	return svc.changes.Record(ctx, tx, spaceID, typ, id, action, version)
}
