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

	"github.com/aurimasniekis/go-spaceman/internal/artifacts"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/eventbus"
	"github.com/aurimasniekis/go-spaceman/internal/keyring"
	"github.com/aurimasniekis/go-spaceman/internal/obs"
	"github.com/aurimasniekis/go-spaceman/internal/obs/metrics"
	"github.com/aurimasniekis/go-spaceman/internal/outbox"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Options configure the service.
type Options struct {
	HeartbeatInterval time.Duration
	LeaseTTL          time.Duration
	DisconnectGrace   time.Duration
	EnrollmentTTL     time.Duration
	// CredentialRecheck is how often a live node connection re-checks its node credential and node state, the
	// backstop for credential expiry and changes made outside this process; disabling, revoking and re-enrolling a
	// node close its connection at once (SM-AUTH-007, ADR-0033). Default 30 s.
	CredentialRecheck time.Duration
	Clock             clock.Clock
	Log               *slog.Logger
	Metrics           *metrics.Metrics
	// RecordingsDir stores central copies of node recordings (empty: <none>, recordings are not accepted). With a
	// RecordingStore other than a local store rooted here, it is the spool of recordings still being shipped.
	RecordingsDir string
	// RecordingStore keeps finished recordings (ADR-0048). Nil: a local store rooted at RecordingsDir.
	RecordingStore artifacts.Store
	// ResolveAccount picks the account a launch uses inside the launch transaction (SM-ACCT-003); it returns the
	// account ID stored with the run. Nil: launches carry no account. avoid, when set, names accounts to pass over for
	// another usable one (usage-limit failover, ADR-0079 decision 8); when every account is to be avoided, the usual
	// choice stands.
	ResolveAccount func(ctx context.Context, tx *store.Tx, spaceID, harness, ref string, avoid func(accountID string) (bool, error)) (string, error)
	// UsageLimitSettings returns a space's usage-limit settings inside the launch transaction (failover and the probe
	// backoff cap, ADR-0079). Nil: failover off, the default cap.
	UsageLimitSettings func(ctx context.Context, q *db.Queries, spaceID string) UsageLimitSettings
	// OnHoldsCleared is called after an operator's "clear now" committed, with every hold it cleared: each space's
	// control is told once (ADR-0079 decision 7). Nil: nobody is told.
	OnHoldsCleared func(holds []ClearedHold)
	// ResolveLaunch fills a launch from the space's configured role and catalogs (kind, harness, model, settings, system
	// text) before validation, and returns what the run manifest records about it (SM-PROMPT-003/004). Nil: launches are
	// used as given.
	ResolveLaunch func(ctx context.Context, spaceID string, in *LaunchInput) (map[string]any, error)
	// AddStarterRoles gives a new space the starter roles when its creation asks for them (ADR-0051). Nil: the option
	// is refused.
	AddStarterRoles func(ctx context.Context, actor audit.Actor, spaceID string) ([]string, error)
	// OnSpacePurged is called after a space was purged, with its runs' IDs, to delete artifacts kept elsewhere
	// (transcripts). Nil: nothing more is deleted.
	OnSpacePurged func(ctx context.Context, runIDs []string)
	// OnRunLaunched is called after a launch committed (the run exists, pending): a response held for the run's agent
	// instance can be pinned to it at once.
	OnRunLaunched func(runID string)
	// OnRunEnded is called after a run was recorded as ended (exited, failed or lost).
	OnRunEnded func(runID string)
	// OnRunIdle is called after a run's program reported idle or done (turn finished), or a usage limit cleared while it
	// was idle: pending deliveries can go out.
	OnRunIdle func(runID string)
	// OnRunLimitTx is called inside the report transaction when a run's usage limit changes window: ended is the limit
	// that ended (nil: none) and opened the one that opened (nil: none); a newer window ends the old one and opens the
	// new one in one call. A repeat or a reclassified limit (the same since) is no change. r is the run as the report
	// left it. It is how control is told once per limit (ADR-0079 decision 11). Nil: nobody is told.
	OnRunLimitTx func(ctx context.Context, tx *store.Tx, r *Run, ended, opened *RunLimit) error
	// OnLimitWaitTx is called inside the transaction that parks a run, resumes it or releases its wait (ADR-0079
	// decisions 4–6), so control is told once per change. Nil: nobody is told.
	OnLimitWaitTx func(ctx context.Context, tx *store.Tx, ev LimitWaitEvent) error
	// RepointTaskTx points a parked run's task at the run that resumed it, inside the launch transaction; false when the
	// task is no longer the parked run's (the resume is then given up). Nil: tasks are not touched.
	RepointTaskTx func(ctx context.Context, tx *store.Tx, actor audit.Actor, taskID, fromRun, toRun string) (bool, error)
	// ReleaseTaskTx gives a parked run's task back when its wait is released: ready and unassigned, or, in review, only
	// unassigned. It returns the task's status after the change ("" when it changed nothing). Nil: tasks are not touched.
	ReleaseTaskTx func(ctx context.Context, tx *store.Tx, actor audit.Actor, taskID, runID, reason string) (string, error)
	// OnSpaceState is called after an operator paused, resumed, archived or restored a space (SetSpaceState), with
	// the new state: deferred work can go out on resume, and subscriptions that stop while paused stop or start.
	OnSpaceState func(spaceID, state string)
	// OnCommandAck is called inside the transaction that records a node's command acknowledgement (with the command's
	// kind and the node's reason).
	OnCommandAck func(ctx context.Context, tx *store.Tx, commandID, kind, result, reason string) error
	// Outbox records run lifecycle events for publication to Pulse (SM-OBS-004). Nil: nothing is published.
	Outbox *outbox.Outbox
	// RelocationsDir stores files of relocations in progress: exported state and memory bundles (ADR-0037). Empty:
	// relocations that need files are blocked with a clear error.
	RelocationsDir string
	// RelocationSyncWait bounds how long a source waits for central to acknowledge memory, recordings and transcripts
	// before it reports its inspection (default 60 s).
	RelocationSyncWait time.Duration
	// Admit decides inside the launch transaction whether a launch may start (limits and budgets, SM-LIMIT-001); an
	// error refuses it. Nil: every launch the space and lease allow starts.
	Admit func(ctx context.Context, tx *store.Tx, a Admission) error
	// Gateway routes the runs of the harnesses a space opted in through an LLM gateway (Harbor Pilot, ADR-0060). Nil:
	// every run uses the space's direct accounts.
	Gateway Gateway
	// OnGatewayRevocation is called after a commit that queued gateway token revocations (it wakes the revoker).
	OnGatewayRevocation func()
	// GatewayMintWait bounds how long a node's dispatch loop waits for a gateway run's token (default 5 s). A mint that
	// takes longer goes on in the background; the launch stays pending, the loop moves on to the node's other commands
	// (only the pass that starts a mint waits for it), and the node is kicked when the mint is done.
	GatewayMintWait time.Duration
	// WakeDeadlines wakes the deadline timer (the stale-run sweep's due time moves when a node connects or
	// disconnects). Nil: the timer finds the change on its next round.
	WakeDeadlines func()
	// StaleRunGrace overrides staleRunGrace (tests; 0 keeps the default).
	StaleRunGrace time.Duration
	// StaleLeaseMargin overrides staleLeaseMargin (tests; 0 keeps the default).
	StaleLeaseMargin time.Duration
}

// Admission is a launch about to be admitted.
type Admission struct {
	SpaceID, NodeID, Kind, Role string
	Actor                       audit.Actor
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
	reloc   relocEngine
	tko     takeovers
	lw      limitWaitJob
	// MemoryExport writes central's role memory for a relocation's destination (nil: spaces with memory cannot relocate).
	MemoryExport MemoryExporter
	// Grants removes a purged space's role grants and reloads the authorization engine after commit (the auth
	// service). Nil only in fleet-only tests: purge then deletes the rows but cannot reload an engine it does not know.
	Grants GrantRemover
	// onReport, when set (tests only), sees every run report as it is applied, after the session's minor filtered it.
	onReport func(proto.RunReport)
}

// GrantRemover deletes every role grant of one domain inside a write transaction and refreshes authorization after
// it commits (auth.Service.RemoveDomainGrants).
type GrantRemover interface {
	RemoveDomainGrants(ctx context.Context, tx *store.Tx, domain string) error
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
	if o.GatewayMintWait <= 0 {
		o.GatewayMintWait = 5 * time.Second
	}
	if o.EnrollmentTTL <= 0 {
		o.EnrollmentTTL = 24 * time.Hour
	}
	if o.CredentialRecheck <= 0 {
		o.CredentialRecheck = 30 * time.Second
	}
	if o.StaleRunGrace <= 0 {
		o.StaleRunGrace = staleRunGrace
	}
	if o.StaleLeaseMargin <= 0 {
		o.StaleLeaseMargin = staleLeaseMargin
	}
	if o.Clock == nil {
		o.Clock = clock.System{}
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	svc := &Service{s: s, kr: kr, audit: aw, bus: bus, changes: ch, opts: o, reloc: relocEngine{retain: make(chan struct{}, 1)},
		tko: takeovers{meters: map[string]*takeoverMeter{}, wake: make(chan struct{}, 1)}}
	svc.lw.started = o.Clock.Now()
	svc.hub = newHub(svc)
	svc.rec = newRecordings(o.RecordingsDir, o.RecordingStore, obs.Component(o.Log, "recordings"), o.Clock.Now)
	return svc
}

// Recordings returns central's recording store.
func (svc *Service) Recordings() *Recordings { return svc.rec }

// Hub returns the node connection hub.
func (svc *Service) Hub() *Hub { return svc.hub }

func (svc *Service) now() time.Time { return svc.opts.Clock.Now().UTC() }

func (svc *Service) wakeDeadlines() {
	if svc.opts.WakeDeadlines != nil {
		svc.opts.WakeDeadlines()
	}
}

func (svc *Service) record(ctx context.Context, tx *store.Tx, spaceID, typ, id, action string, version int64) error {
	if svc.changes == nil {
		return nil
	}
	return svc.changes.Record(ctx, tx, spaceID, typ, id, action, version)
}
