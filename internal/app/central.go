// Package app is the composition root of the central `spaceman` service: it opens storage and the keyring, builds the
// services, and runs the API, web UI and management listeners with graceful shutdown.
package app

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.yaml.in/yaml/v3"

	"github.com/aurimasniekis/go-spaceman/internal/accounts"
	"github.com/aurimasniekis/go-spaceman/internal/agenttools"
	"github.com/aurimasniekis/go-spaceman/internal/api"
	"github.com/aurimasniekis/go-spaceman/internal/approvals"
	"github.com/aurimasniekis/go-spaceman/internal/artifacts"
	"github.com/aurimasniekis/go-spaceman/internal/attachments"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/backup"
	"github.com/aurimasniekis/go-spaceman/internal/catalog"
	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/config"
	"github.com/aurimasniekis/go-spaceman/internal/control"
	"github.com/aurimasniekis/go-spaceman/internal/dream"
	"github.com/aurimasniekis/go-spaceman/internal/eventbus"
	"github.com/aurimasniekis/go-spaceman/internal/events"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/gateway"
	"github.com/aurimasniekis/go-spaceman/internal/gitkeys"
	"github.com/aurimasniekis/go-spaceman/internal/harborpilot"
	"github.com/aurimasniekis/go-spaceman/internal/httpguard"
	"github.com/aurimasniekis/go-spaceman/internal/human"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/inbox"
	"github.com/aurimasniekis/go-spaceman/internal/kb"
	"github.com/aurimasniekis/go-spaceman/internal/keyring"
	"github.com/aurimasniekis/go-spaceman/internal/limits"
	"github.com/aurimasniekis/go-spaceman/internal/memory"
	"github.com/aurimasniekis/go-spaceman/internal/messages"
	"github.com/aurimasniekis/go-spaceman/internal/obs"
	"github.com/aurimasniekis/go-spaceman/internal/obs/health"
	"github.com/aurimasniekis/go-spaceman/internal/obs/metrics"
	"github.com/aurimasniekis/go-spaceman/internal/oidc"
	"github.com/aurimasniekis/go-spaceman/internal/outbox"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/queue"
	"github.com/aurimasniekis/go-spaceman/internal/repos"
	"github.com/aurimasniekis/go-spaceman/internal/retention"
	"github.com/aurimasniekis/go-spaceman/internal/schedule"
	"github.com/aurimasniekis/go-spaceman/internal/spaceexport"
	"github.com/aurimasniekis/go-spaceman/internal/spacemanmcp"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
	"github.com/aurimasniekis/go-spaceman/internal/usage"
	"github.com/aurimasniekis/go-spaceman/internal/xspace"
)

// Central holds the running central service.
type Central struct {
	// StartedAt is when this central opened; BootID identifies this start (the System page, SM-OBS-006).
	StartedAt time.Time
	BootID    string
	Cfg       *config.Config
	Log       *slog.Logger
	Clock     clock.Clock
	Store     *store.Store
	Keyring   *keyring.Keyring
	Audit     *audit.Writer
	Authz     *authz.Engine
	Auth      *auth.Service
	Bus       *eventbus.Bus
	Changes   *changefeed.Recorder
	Metrics   *metrics.Metrics
	Health    *health.Checker
	Fleet     *fleet.Service
	// Artifacts is the artifact store (SM-STORE-002, ADR-0048): attachment content, finished recordings and transcripts.
	Artifacts artifacts.Store
	// Attachments stores uploaded files (SM-ATT-*).
	Attachments *attachments.Service
	// Inbox holds each space's incoming items (SM-INBOX-*).
	Inbox *inbox.Service
	// Events manages Pulse connections, templates and subscriptions (SM-EVT-*).
	Events *events.Service
	// Queue is each space's execution queue (SM-QUEUE-*).
	Queue *queue.Service
	// Messages is durable communication between agents, control and operators (SM-COMM-*); Notifier delivers notices.
	Messages *messages.Service
	Notifier *messages.Notifier
	// Supervisor wakes control on actionable events and restarts it within bounds (SM-CTRL-002).
	Supervisor *control.Supervisor
	// GitKeys holds shared Git SSH keys (ADR-0058).
	GitKeys *gitkeys.Service
	// HarborPilot holds the Harbor Pilot connection and space settings (ADR-0060).
	HarborPilot *harborpilot.Service
	// HarborPilotRuns is the run-token lifecycle: routing, mint-or-reuse at dispatch, revocation (ADR-0060, HP-T3).
	HarborPilotRuns *harborpilot.Lifecycle
	// HarborPilotBudgets reads Harbor Pilot budget status: the early launch refusal and the Limits page (ADR-0060, HP-T5).
	HarborPilotBudgets *harborpilot.Budgets
	// Tools are the built-in agent tools served through the MCP bridge (SM-MCP-003).
	Tools *agenttools.Registry
	// Gateway serves external MCP servers' tools to granted runs (ADR-0023).
	Gateway *gateway.Service
	// mcpEndpoint serves api.MCPPath (ADR-0063); set by wireSpacemanMCP.
	mcpEndpoint *spacemanmcp.Endpoint
	// Approvals gates operations that policies make need a person's approval (ADR-0024).
	Approvals *approvals.Service
	// Memory preserves role memory synced from nodes (ADR-0025).
	Memory *memory.Service
	// KB runs knowledge-base support sessions (ADR-0026).
	KB *kb.Service
	// Dreams runs the dreamer (ADR-0027).
	Dreams *dream.Service
	// Usage keeps runs' usage and native transcripts (ADR-0028).
	Usage *usage.Service
	// Catalog holds prompts, harness profiles, role templates and space roles (SM-PROMPT-*).
	Catalog *catalog.Service
	// Accounts holds harness credentials (SM-ACCT-*).
	Accounts *accounts.Service
	// Human holds requests for people: questions, approvals, to-dos and messages (SM-HUM-*).
	Human *human.Service
	// OIDC is sign-in through OpenID Connect providers such as Porch (ADR-0029).
	OIDC *oidc.Service
	// Outbox publishes SpaceMan's own events to Pulse after commit (SM-OBS-004/005, ADR-0030).
	Outbox *outbox.Outbox
	// Backups makes, schedules, verifies and prunes backups (SM-STORE-003…005, ADR-0031).
	Backups *backup.Service
	// XSpace is cross-space collaboration: permissions per peer and requests between spaces (ADR-0038).
	XSpace *xspace.Service
	// Restored is the restore applied at this start, or nil (SM-STORE-006).
	Restored *backup.RestoreState
	// Limits is the limits model: installation defaults, space overrides, admission and budgets (ADR-0036).
	Limits *limits.Service
	// Schedules create inbox items and tasks at cron or interval occurrences (SM-LIMIT-003, ADR-0036).
	Schedules *schedule.Service
	// Repos is a space's configured repositories and the node's reports of them (ADR-0044).
	Repos *repos.Service
	// Retention deletes what is older than its retention period in one daily pass (ADR-0045).
	Retention *retention.Service
	// Exports writes space export archives (ADR-0045).
	Exports *spaceexport.Service

	// UI serves the embedded web UI (set by cmd/spaceman; a notice handler when the UI is not embedded).
	UI http.Handler
}

// Options adjust OpenCentral (tests).
type Options struct {
	Clock   clock.Clock
	Environ []string
	// StoreBusyTimeout overrides SQLite's busy timeout (fault tests of lock contention; 0 keeps the default).
	StoreBusyTimeout time.Duration
	// StoreReaders overrides the store's reader pool size (0 keeps the default, max(4, GOMAXPROCS)). Opening the store
	// opens and checks every reader connection, which on a many-core host under the race detector takes seconds.
	StoreReaders int
	// StaleRunGrace overrides the stale-run sweeper's grace period (tests; 0 keeps the default, ADR-0074).
	StaleRunGrace time.Duration
}

// OpenCentral opens storage (taking the data-directory lock), loads the KEK and keyring, and builds the core services.
func OpenCentral(ctx context.Context, cfg *config.Config, log *slog.Logger, o Options) (*Central, error) {
	if o.Clock == nil {
		o.Clock = clock.System{}
	}
	if o.Environ == nil {
		o.Environ = os.Environ()
	}
	m := metrics.New()
	st, err := store.Open(ctx, store.Options{DataDir: cfg.DataDir, Logger: obs.Component(log, "store"), Clock: o.Clock, Metrics: m,
		BusyTimeout: o.StoreBusyTimeout, Readers: o.StoreReaders, AllowNetworkFS: !cfg.IsProduction() && os.Getenv("SPACEMAN_ALLOW_NETWORK_FS") == "1"})
	if err != nil {
		return nil, err
	}
	c := &Central{Cfg: cfg, Log: log, Clock: o.Clock, Store: st, Metrics: m, Bus: eventbus.New(), StartedAt: o.Clock.Now().UTC(), BootID: ids.New(ids.Boot)}
	ob := cfg.Events.Outbound
	c.Outbox = outbox.New(st, outbox.Options{Enabled: ob.Enabled, Source: ob.Source, URL: ob.URL, Clock: o.Clock, Metrics: m, Log: log,
		Retention: ob.Retention.D()})
	if err := c.openKeyring(ctx, o.Environ); err != nil {
		_ = st.Close()
		return nil, err
	}
	c.Audit = audit.NewWriter(c.Keyring)
	if c.Authz, err = authz.New(ctx, st.R()); err != nil {
		_ = st.Close()
		return nil, err
	}
	c.Changes = changefeed.New(c.Bus)
	c.Limits = limits.New(st, c.Audit, c.Changes, cfg.LimitValues(), o.Clock, obs.Component(log, "limits"))
	// The first start after a restore pauses every space and renews execution authority before anything else runs.
	if c.Restored, err = backup.ApplyPendingRestore(ctx, st, c.Audit, c.Changes); err != nil {
		_ = st.Close()
		return nil, err
	}
	if rs := c.Restored; rs != nil {
		log.Warn("restored from a backup: every space is paused and execution authority was renewed; resume spaces explicitly",
			slog.String("backup.id", rs.BackupID), slog.Int("spaces_paused", len(rs.PausedSpaces)), slog.Int64("runs_unknown", rs.RunsUnknown),
			slog.Int64("subscriptions_reset", rs.SubscriptionsReset), slog.Int64("approvals_revoked", rs.ApprovalsRevoked))
	}
	c.Auth = auth.New(st, c.Keyring, c.Audit, c.Bus, c.Authz, auth.Options{
		PasswordMinBytes: cfg.Auth.PasswordMinLength, SessionIdle: cfg.Auth.SessionIdle.D(), SessionAbsolute: cfg.Auth.SessionAbsolute.D(),
		ReauthWindow: cfg.Auth.ReauthWindow.D(), RequireTOTP: cfg.Auth.RequireTOTP, Clock: o.Clock, Metrics: m,
	})
	c.OIDC = oidc.New(st, c.Keyring, c.Audit, c.Auth, oidc.Options{PublicURL: cfg.PublicURL, DevMode: !cfg.IsProduction(), Clock: o.Clock,
		Logger: log, Metrics: m})
	c.Catalog = catalog.New(st, c.Audit, c.Changes, catalog.Options{MaxPermissionMode: cfg.Harness.MaxPermissionMode})
	artifactDir := cfg.Artifacts.LocalDir
	if artifactDir == "" {
		artifactDir = st.Path("artifacts")
	}
	if c.Artifacts, err = openArtifacts(cfg, st, artifactDir); err != nil {
		_ = st.Close()
		return nil, err
	}
	// Queued gateway revocations are done on the deadline timer, woken after the commit that queued them.
	wakeDeadlines := func() {
		if c.Human != nil {
			c.Human.Kick()
		}
	}
	c.HarborPilot = harborpilot.New(st, c.Audit, c.Changes, c.Keyring, harborpilot.Options{Log: obs.Component(log, "harborpilot"), Wake: wakeDeadlines})
	c.HarborPilotBudgets = harborpilot.NewBudgets(c.HarborPilot, harborpilot.BudgetsOptions{})
	c.HarborPilotRuns = harborpilot.NewLifecycle(c.HarborPilot, harborpilot.LifecycleOptions{Metrics: m, Budget: c.HarborPilotBudgets})
	c.Fleet = fleet.New(st, c.Keyring, c.Audit, c.Bus, c.Changes, fleet.Options{HeartbeatInterval: cfg.Nodes.HeartbeatInterval.D(),
		LeaseTTL: cfg.Nodes.LeaseTTL.D(), DisconnectGrace: cfg.Nodes.DisconnectGrace.D(), EnrollmentTTL: cfg.Nodes.EnrollmentTTL.D(),
		CredentialRecheck: cfg.Auth.StreamRevalidate.D(), Clock: o.Clock, WakeDeadlines: wakeDeadlines, StaleRunGrace: o.StaleRunGrace,
		Log: log, Metrics: m, RecordingsDir: st.Path("recordings"), RecordingStore: c.Artifacts, RelocationsDir: st.Path("relocations"), ResolveAccount: accounts.Resolve,
		ResolveLaunch: func(ctx context.Context, spaceID string, in *fleet.LaunchInput) (map[string]any, error) {
			sp, err := st.R().GetSpace(ctx, spaceID)
			if err != nil {
				return nil, nil //nolint:nilerr // the launch reports a missing space itself
			}
			r, err := c.Catalog.Resolve(ctx, st.R(), sp, catalog.Launch{Role: in.Role, Kind: in.Kind, Harness: in.Harness, Model: in.Model,
				Settings: in.Settings, SystemText: in.SystemText})
			if err != nil {
				return nil, err
			}
			in.Kind, in.Harness, in.Model, in.Settings, in.SystemText = r.Kind, r.Harness, r.Model, r.Settings, r.SystemText
			in.ClaudeSettings, in.HarnessVersion = r.ClaudeSettings, r.HarnessVersion
			in.RecordingMaxBytes = c.Limits.Value(ctx, st.R(), spaceID, limits.RecordingQuota)
			if lv, err := c.Limits.For(ctx, st.R(), spaceID); err == nil {
				if cols, rows, ok := lv.TerminalSize(); ok {
					in.TerminalSize = &fleet.TerminalSize{Cols: cols, Rows: rows}
				}
			}
			in.Skills = nil
			for _, sk := range r.Skills {
				in.Skills = append(in.Skills, proto.Skill{Name: sk.Slug, Version: sk.Number, SHA256: sk.SHA256, Files: sk.Files})
			}
			if c.Gateway != nil {
				// What the role is granted at launch; every call is checked again against the grants of that moment.
				mcp, err := c.Gateway.ForManifest(ctx, spaceID, in.Role, in.Kind)
				if err != nil {
					return nil, err
				}
				r.Manifest["mcp_servers"] = mcp
			}
			return r.Manifest, nil
		},
		AddStarterRoles: func(ctx context.Context, actor audit.Actor, spaceID string) ([]string, error) {
			return c.Catalog.AddStarterRoles(ctx, actor, spaceID)
		},
		// The notifier is created below; these hooks only run once the server serves.
		OnSpacePurged: func(ctx context.Context, runIDs []string) {
			if c.Usage != nil {
				c.Usage.PurgeTranscripts(ctx, runIDs)
			}
		},
		OnRunLaunched: func(string) {
			if c.Human != nil {
				c.Human.Kick() // a response held for the run's instance goes to it (SM-HUM-009)
			}
		},
		OnRunEnded: func(runID string) {
			if c.Fleet != nil {
				c.Fleet.Recordings().RunEnded(runID) // archived to the artifact store (S3) once the run ended
			}
			if c.Gateway != nil {
				c.Gateway.EndRun(runID)
			}
			if c.KB != nil {
				c.KB.OnRunEnded(runID)
			}
			if c.Repos != nil {
				c.Repos.OnRunEnded(runID)
			}
			if c.Dreams != nil {
				c.Dreams.OnRunEnded(runID)
			}
			if c.Supervisor != nil {
				c.Supervisor.OnRunEnded(runID)
			}
			if c.Human != nil {
				c.Human.Kick() // route responses the run had not acknowledged (SM-HUM-009)
			}
		},
		OnSpaceState: func(spaceID, state string) {
			if c.Events != nil {
				c.Events.SpaceStateChanged() // subscriptions with while_paused stop stop or start (SM-EVT-014)
			}
			if c.Supervisor != nil {
				c.Supervisor.OnSpaceState(spaceID, state) // notices deferred while paused go out on resume
			}
		},
		OnRunIdle: func(runID string) {
			if c.Notifier != nil {
				c.Notifier.Kick()
			}
			if c.Supervisor != nil {
				c.Supervisor.OnRunIdle(runID) // control's idle stop (ADR-0056)
			}
			if c.Human != nil {
				c.Human.Kick() // a resumed instance may take a held response
			}
		},
		OnCommandAck: func(ctx context.Context, tx *store.Tx, commandID, kind, result, reason string) error {
			if kind == proto.CmdDreamDone {
				if c.Dreams == nil {
					return nil
				}
				return c.Dreams.OnCommitAck(ctx, tx, commandID, result, reason)
			}
			if c.Notifier == nil {
				return nil
			}
			return c.Notifier.OnCommandAck(ctx, tx, commandID, result)
		},
		Outbox: c.Outbox, Admit: c.Limits.Admit, Gateway: c.HarborPilotRuns, OnGatewayRevocation: wakeDeadlines})
	c.Accounts = accounts.New(st, c.Keyring, c.Audit, c.Changes)
	c.Fleet.Hub().Secrets = func(ctx context.Context, run *fleet.Run) (map[string]string, error) {
		if run.Launch.AccountRef == "" {
			return nil, nil
		}
		env, err := c.Accounts.SecretEnv(ctx, run.SpaceID, run.Harness, run.Launch.AccountRef)
		if errors.Is(err, accounts.ErrUnavailable) {
			return nil, fmt.Errorf("%w: account %s was deleted, disabled or unbound after the launch", fleet.ErrCredentialsUnavailable, run.Launch.AccountRef)
		}
		return env, err
	}
	c.Attachments = attachments.New(st, attachments.NewStore(c.Artifacts, filepath.Join(artifactDir, "tmp")), c.Audit, attachments.Options{MaxBytes: cfg.HTTP.MaxUploadBytes,
		QuotaBytes: cfg.Artifacts.SpaceQuotaBytes, Clock: o.Clock, QuotaFor: func(ctx context.Context, q *db.Queries, spaceID string) int64 {
			return c.Limits.Value(ctx, q, spaceID, limits.StorageQuota)
		}})
	c.Inbox = inbox.New(st, c.Audit, c.Changes, c.Attachments)
	c.Inbox.Outbox = c.Outbox
	c.Queue = queue.New(st, c.Audit, c.Changes)
	c.Queue.Outbox = c.Outbox
	c.Queue.DefaultMaxActive = int(cfg.Limits.MaxTopLevelTasks)
	c.Human = human.New(st, c.Audit, c.Changes, o.Clock)
	c.Human.Outbox = c.Outbox
	c.Human.PendingLimits = func(ctx context.Context, q *db.Queries, spaceID string) (int64, int64) {
		v, err := c.Limits.For(ctx, q, spaceID)
		if err != nil {
			return 0, 0 // the built-in defaults apply
		}
		return v.Get(limits.HumanPending), v.Get(limits.HumanPendingPerRun)
	}
	c.Human.Log = obs.Component(log, "human")
	// A response whose run is gone goes to a control auto-start can start (ADR-0056).
	c.Human.ControlAutostart = func(ctx context.Context, q *db.Queries, spaceID string) bool {
		v, err := c.Limits.For(ctx, q, spaceID)
		return err == nil && v.Get(limits.ControlAutostart) != 0
	}
	c.Messages = messages.New(st, c.Audit, c.Changes)
	c.Notifier = messages.NewNotifier(c.Messages, c.Fleet, log, o.Clock)
	c.Human.OnNotify = c.Notifier.Kick
	c.Supervisor = control.NewSupervisor(c.Fleet, c.Queue, c.Messages, c.Human, log, o.Clock)
	c.Queue.Workers = c.Fleet
	c.Queue.Notify = c.Supervisor.Notify
	c.Supervisor.Inbox = c.Inbox
	// Control lifecycle (ADR-0056): started for waiting work, stopped after its idle time.
	c.Supervisor.Limit = func(ctx context.Context, spaceID, key string) int64 { return c.Limits.Value(ctx, nil, spaceID, key) }
	c.Supervisor.ControlRole = c.Catalog.ControlRole
	c.Notifier.ControlWanted = c.Supervisor.ControlWanted
	c.Inbox.OnCreated = c.Supervisor.OnInboxItem
	c.Human.Inbox = c.Inbox // routed responses become inbox items when the human policy says so (SM-INBOX-001)
	c.Queue.OnCreated = c.Supervisor.OnTaskCreated
	c.Queue.OnStatus = func(actor audit.Actor, t *queue.Task, from string) {
		c.Supervisor.OnTaskStatus(actor, t, from)
		c.Human.Kick() // a task that starts or stops running moves the earliest task deadline (ADR-0036)
	}
	c.Gateway = gateway.New(st, c.Keyring, c.Audit, c.Changes, gateway.Options{Log: obs.Component(log, "gateway")})
	c.Approvals = approvals.New(st, c.Audit, c.Changes, c.Human, obs.Component(log, "approvals"))
	c.Approvals.StillAuthorized = c.approverStillAuthorized
	c.Approvals.Outbox = c.Outbox
	gitBin, gitErr := memory.NewGit()
	if gitErr != nil {
		log.Warn("role memory cannot be stored centrally until git is installed", obs.Err(gitErr))
		gitBin = nil
	}
	c.Memory = memory.New(st, c.Audit, c.Changes, st.Path("memory"), gitBin, obs.Component(log, "memory"))
	c.Memory.Outbox = c.Outbox
	c.Fleet.Hub().Memory = c.Memory
	c.Fleet.MemoryExport = c.Memory
	c.Fleet.Grants = c.Auth
	c.KB = kb.New(st, c.Audit, c.Changes, c.Fleet, c.Messages, o.Clock, obs.Component(log, "kb"))
	c.KB.Outbox = c.Outbox
	c.KB.SpaceLimits = func(ctx context.Context, q *db.Queries, spaceID string) (int64, int64) {
		v, err := c.Limits.For(ctx, q, spaceID)
		if err != nil {
			return cfg.Limits.MaxKBSessions, cfg.Limits.SupportDepth
		}
		return v.Get(limits.KBSessions), v.Get(limits.SupportDepth)
	}
	c.Fleet.Hub().KBResult = c.KB.OnResult
	c.GitKeys = gitkeys.New(st, c.Audit, c.Changes, c.Keyring)
	c.Repos = repos.New(st, c.Audit, c.Changes, c.Fleet, repos.Options{AllowLocalURLs: !cfg.IsProduction(), Keyring: c.Keyring, GitKeys: c.GitKeys, Clock: o.Clock,
		Log: obs.Component(log, "repos")})
	c.Fleet.Hub().RepoKey = c.Repos.Key // repository SSH keys go only onto the wire message of a clone or fetch (ADR-0057)
	c.KB.ResolveRepo = c.Repos.ForKB
	c.Fleet.Hub().RepoStatus = c.Repos.OnStatus
	c.Fleet.Hub().OnConnected = c.Repos.OnNodeConnected
	c.XSpace = xspace.New(st, c.Audit, c.Changes, c.Inbox, c.Queue, c.KB, o.Clock, obs.Component(log, "xspace"))
	c.XSpace.Outbox = c.Outbox
	c.XSpace.Notified = c.Notifier.Kick
	c.XSpace.Kick = c.Human.Kick
	c.KB.OnCrossSpace = c.XSpace.OnKBDone
	c.Dreams = dream.New(st, c.Audit, c.Changes, c.Fleet, o.Clock, obs.Component(log, "dreamer"))
	c.Usage = usage.New(st, c.Audit, c.Changes, st.Path("transcripts"), c.Artifacts, obs.Component(log, "usage"))
	target, err := backup.TargetFromConfig(cfg)
	if err != nil {
		_ = st.Close()
		return nil, err
	}
	bc := cfg.Backups
	c.Backups = backup.NewService(st, c.Keyring.Wrapper(), c.Audit, c.Changes, backup.ServiceOptions{Target: target,
		Sources: backupSources(st.Dir(), artifactDir, gitBin, c.Artifacts), Config: func() []byte { b, _ := yaml.Marshal(cfg); return b },
		Schedule: bc.Enabled, Interval: bc.Interval.D(), KeepLast: bc.KeepLast, MaxAge: bc.MaxAge.D(), Clock: o.Clock, Log: obs.Component(log, "backup"),
		PrepareMemory: c.prepareBackupMemory})
	c.Fleet.Hub().Usage = usageWithBudgets{Service: c.Usage, limits: c.Limits}
	c.Schedules = schedule.New(st, c.Audit, c.Changes, c.Inbox, c.Queue, o.Clock, obs.Component(log, "schedule"))
	c.Schedules.Kick = c.Human.Kick
	c.Limits.OnChange = c.Human.Kick
	c.Limits.AuditDays = cfg.Retention.AuditDays
	c.Retention = retention.New(st, c.Audit, c.Changes, c.Limits, retention.Options{AuditDays: cfg.Retention.AuditDays, Clock: o.Clock,
		Log: obs.Component(log, "retention"), DeleteRecording: c.Fleet.DeleteRecording, DeleteTranscript: c.Usage.DeleteTranscript,
		AfterPass: func(ctx context.Context) {
			if _, _, err := c.Attachments.GC(ctx); err != nil && ctx.Err() == nil {
				log.Warn("attachment garbage collection after retention", obs.Err(err))
			}
		}})
	c.Exports = spaceexport.New(st, c.Audit, c.Attachments.OpenContent, func(ctx context.Context, spaceID string) int64 {
		return c.Limits.Value(ctx, nil, spaceID, limits.ExportMax)
	}, o.Clock)
	c.Limits.Stop = func(ctx context.Context, actor audit.Actor, runID, reason string) error {
		_, err := c.Fleet.StopRun(ctx, actor, runID, "interrupt", reason)
		return err
	}
	c.Limits.Notify = func(ctx context.Context, spaceID, subject, body, key string, toControl bool) error {
		from := messages.From{Kind: "system", ID: "limits", Label: "Limits"}
		_, _, err := c.Messages.Send(ctx, audit.Actor{Type: audit.ActorSystem, ID: "limits"}, spaceID, from,
			messages.SendInput{To: "operators", Kind: "notification", Subject: subject, Body: body, DedupKey: key})
		if err != nil || !toControl {
			return err
		}
		_, _, err = c.Messages.Send(ctx, audit.Actor{Type: audit.ActorSystem, ID: "limits"}, spaceID, from,
			messages.SendInput{To: "control", Kind: "notification", Subject: subject, Body: body, DedupKey: key + ":control"})
		return err
	}
	c.Gateway.Gate = c.Approvals
	c.Tools = agenttools.New(agenttools.Deps{Inbox: c.Inbox, Queue: c.Queue, Attachments: c.Attachments, Human: c.Human,
		Extra: []func(*agenttools.Registry){messages.Tools(c.Messages), control.Tools(c.Fleet, c.Queue), memory.Tools(c.Memory), kb.Tools(c.KB), repos.Tools(c.Repos), dream.Tools(c.Dreams), xspace.Tools(c.XSpace)}, Upstream: c.Gateway, Gate: c.Approvals})
	c.Fleet.Hub().Tools = c.Limits.ToolCalls(c.Tools.Dispatch)
	evOpts := events.Options{Clock: o.Clock, Metrics: m, Log: obs.Component(log, "events"), RateFor: func(ctx context.Context, spaceID string) int64 {
		return c.Limits.Value(ctx, nil, spaceID, limits.IngestPerMinute)
	}, DepthFor: func(ctx context.Context, spaceID string) int64 {
		return c.Limits.Value(ctx, nil, spaceID, limits.EventDepth) // bounded automation across installations (SM-EVT-016)
	}}
	if ob.Enabled {
		evOpts.Origin = ob.Source // never re-ingest our own events (SM-EVT-016)
	}
	c.Events = events.New(st, c.Keyring, c.Audit, c.Changes, c.Inbox, c.Attachments, evOpts)
	if err := c.Catalog.SeedBuiltins(ctx); err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("seed skills: %w", err)
	}
	if err := c.Catalog.SeedStarterRoles(ctx); err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("seed starter roles: %w", err)
	}
	if err := c.Events.SeedBuiltins(ctx); err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("seed event templates: %w", err)
	}
	c.Health = health.New(health.Options{ExposeDetails: !cfg.IsProduction()})
	c.Health.MustRegister(
		health.StoreHealth("store", st.Health()),
		health.StoreWritable("store.writable", st.W()),
		health.DiskSpace("disk.data", cfg.DataDir, dataReserveBytes, dataReservePercent),
	)
	_ = m.Register(c.Health.Collector())
	m.ObserveFilesystem(metrics.VolumeData, cfg.DataDir)
	// SpaceMan MCP servers (ADR-0063): SpaceMan's own API as gateway tools, run in process with each server's internal
	// token. Built last, from the complete API.
	if err := c.wireSpacemanMCP(); err != nil { //nolint:contextcheck // builds handlers; each request brings its own context
		_ = st.Close()
		return nil, fmt.Errorf("spaceman mcp: %w", err)
	}
	return c, nil
}

// SpacemanCatalog returns the SpaceMan MCP tool catalog (nil before wireSpacemanMCP).
func (c *Central) SpacemanCatalog() *spacemanmcp.Catalog {
	if c.mcpEndpoint == nil {
		return nil
	}
	return c.mcpEndpoint.Cat
}

// wireSpacemanMCP gives the gateway its SpaceMan MCP backend: the tool catalog generated from this build's API and the
// in-process handler that runs the tools.
func (c *Central) wireSpacemanMCP() error {
	d := c.APIDeps()
	cat, err := spacemanmcp.Build(api.Register(http.NewServeMux(), d).OpenAPI(), d.RuleFor)
	if err != nil {
		return err
	}
	c.Gateway.Tokens = c.Auth
	h := d.InProcessHandler()
	c.Gateway.Spaceman = &spacemanmcp.Backend{Cat: cat, Handler: h, Tokens: c.Auth}
	// The external MCP endpoint (ADR-0063 §External endpoint): the same catalog and executor for operators' own MCP
	// clients, behind the API's guard (api.MCPPath).
	c.mcpEndpoint = &spacemanmcp.Endpoint{Cat: cat, Handler: h, Tokens: c.Auth, Approvals: c.Approvals, Authz: c.Authz,
		Log: obs.Component(c.Log, "mcp_endpoint"),
		Record: func(ctx context.Context, ev audit.Event) error {
			return c.Store.W().TxNamed(ctx, "mcp_endpoint.call", func(tx *store.Tx) error {
				_, err := c.Audit.Append(ctx, tx, ev)
				return err
			})
		}}
	return nil
}

func (c *Central) openKeyring(ctx context.Context, environ []string) error {
	kek, err := keyring.LoadKEK(keyring.Source{Kind: c.Cfg.KEK.Source, File: c.Cfg.KEK.File, Production: c.Cfg.IsProduction(),
		DataDir: c.Cfg.DataDir, Environ: environ})
	if err != nil {
		return err
	}
	if c.Keyring, err = keyring.Load(ctx, c.Store, kek); err != nil {
		return fmt.Errorf("keyring: %w", err)
	}
	return nil
}

// The data directory's free-space reserve: below it readiness degrades (and the attention summary shows a storage problem).
const (
	dataReserveBytes   = 256 << 20
	dataReservePercent = 2
)

// shutdownGrace is how long Close lets background gateway mints, node connections and the work those started (memory
// syncs, usage, transcripts, tool calls) finish before it cancels the mints and closes the store. The two waits run
// side by side under this one grace, so a stop takes at most shutdown.timeout (30 s by default) + 5 s + the store's
// 8 s = 43 s, inside the units' TimeoutStopSec=45s.
const shutdownGrace = 5 * time.Second

// Close closes node connections, then releases storage and the data-directory lock.
func (c *Central) Close() error {
	if c.Backups != nil {
		c.Backups.Wait()
	}
	if c.Retention != nil {
		c.Retention.Wait()
	}
	if c.Fleet != nil {
		hub := c.Fleet.Hub()
		var wg sync.WaitGroup
		wg.Go(func() { hub.StopMints(shutdownGrace) })
		hub.Shutdown("central shutting down", shutdownGrace)
		wg.Wait()
		c.Fleet.Recordings().Close()
	}
	if c.Gateway != nil {
		c.Gateway.Close()
	}
	return c.Store.Close()
}

// APIDeps returns the API dependencies.
func (c *Central) APIDeps() *api.Deps {
	var trusted []netip.Prefix
	for _, p := range c.Cfg.HTTP.TrustedProxies {
		if pf, err := netip.ParsePrefix(p); err == nil {
			trusted = append(trusted, pf)
		}
	}
	d := &api.Deps{Log: obs.Component(c.Log, "api"), Store: c.Store, Auth: c.Auth, Authz: c.Authz, Audit: c.Audit, Bus: c.Bus,
		Metrics: c.Metrics, Clock: c.Clock, PublicURL: c.Cfg.PublicURL, SecureCookies: strings.HasPrefix(c.Cfg.PublicURL, "https://"),
		TrustedProxies: trusted, MaxBodyBytes: c.Cfg.HTTP.MaxBodyBytes, MaxUploadBytes: c.Cfg.HTTP.MaxUploadBytes,
		StreamRevalidate: c.Cfg.Auth.StreamRevalidate.D(), ExtraRules: rules(),
		Extra: []func(huma.API, *api.Deps){api.RegisterFleet(c.Fleet), api.RegisterRelocations(c.Fleet), api.RegisterRecordings(c.Fleet), api.RegisterAttachments(c.Attachments),
			api.RegisterInbox(c.Inbox), api.RegisterEvents(c.Events),
			api.RegisterQueue(c.Queue, c.Inbox), api.RegisterHuman(c.Human), api.RegisterAccounts(c.Accounts), api.RegisterMessages(c.Messages), api.RegisterCatalog(c.Catalog, c.Store), api.RegisterGateway(c.Gateway), api.RegisterApprovals(c.Approvals), api.RegisterMemory(c.Memory), api.RegisterKB(c.KB), api.RegisterDreams(c.Dreams), api.RegisterUsage(c.Usage), api.RegisterOIDC(c.OIDC), api.RegisterOutbox(c.Outbox), api.RegisterBackups(c.Backups), api.RegisterLimits(c.Limits, c.Schedules), api.RegisterXSpace(c.XSpace), api.RegisterRepositories(c.Repos), api.RegisterGitKeys(c.GitKeys), api.RegisterHarborPilot(c.HarborPilot, c.HarborPilotBudgets), api.RegisterRetention(c.Retention, c.Exports), api.RegisterAttention(c.attentionSources()), api.RegisterSystemStatus(c.systemSources())},
		Raw: append(api.RecordingStreams(c.Fleet), api.AttachmentContent(c.Attachments), api.TranscriptDownload(c.Usage), api.SpaceExportDownload(c.Exports))}
	d.Ready = func() bool { r, ok := c.Health.Last(); return ok && r.Status == health.StatusReady }
	if c.mcpEndpoint != nil {
		d.MCP = c.mcpEndpoint
	}
	return d
}

// attentionSources are the live inputs of the attention summary (SM-UI-003): spaces and node connections from the
// fleet, readiness and storage health, the data directory's free space and each space's attachment quota.
func (c *Central) attentionSources() *api.AttentionSources {
	return &api.AttentionSources{
		Spaces: func(ctx context.Context) ([]api.AttentionSpaceRef, error) {
			l, err := c.Fleet.ListSpaces(ctx, nil)
			if err != nil {
				return nil, err
			}
			out := make([]api.AttentionSpaceRef, 0, len(l))
			for _, s := range l {
				out = append(out, api.AttentionSpaceRef{ID: s.ID, Slug: s.Slug, Name: s.Name, State: s.State, NodeID: s.NodeID})
			}
			return out, nil
		},
		Nodes: func(ctx context.Context) ([]api.AttentionNode, error) {
			l, err := c.Fleet.ListNodes(ctx)
			if err != nil {
				return nil, err
			}
			out := make([]api.AttentionNode, 0, len(l))
			for _, n := range l {
				out = append(out, api.AttentionNode{ID: n.ID, Name: n.Name, State: n.State, Connected: n.Connected, LastDisconnectAt: n.LastDisconnectAt})
			}
			return out, nil
		},
		Readiness: c.Health.Evaluate,
		StoreHealth: func() (string, string) {
			h := c.Store.Health().Status()
			return string(h.State), h.Reason
		},
		DataDir: c.Cfg.DataDir, MinFreeBytes: dataReserveBytes, MinFreePct: dataReservePercent,
		StorageQuota: func(ctx context.Context, spaceID string) int64 {
			return c.Limits.Value(ctx, nil, spaceID, limits.StorageQuota)
		},
	}
}

// systemSources are the inputs of the System page (SM-OBS-006): what central learned at start, node connections, storage
// health and live streams. No key material or key paths.
func (c *Central) systemSources() *api.SystemSources {
	return &api.SystemSources{
		StartedAt: c.StartedAt, BootID: c.BootID, PublicURL: c.Cfg.PublicURL, DataDir: c.Cfg.DataDir,
		Schema: func(ctx context.Context) (int64, int64, error) {
			return store.SchemaVersions(ctx, c.Store.ReadDB(), store.EmbeddedMigrations())
		},
		Nodes: func(ctx context.Context) (active, connected int, err error) {
			l, err := c.Fleet.ListNodes(ctx)
			if err != nil {
				return 0, 0, err
			}
			for _, n := range l {
				if n.State == "active" {
					active++
					if n.Connected {
						connected++
					}
				}
			}
			return active, connected, nil
		},
		Storage:     c.attentionSources(),
		StoreSizes:  c.Store.Sizes,
		LiveStreams: c.Metrics.SSEClients,
	}
}

// Handler is the public listener's handler: the API under /api/ and the web UI elsewhere.
func (c *Central) Handler(d *api.Deps) http.Handler {
	mux := http.NewServeMux()
	apiH := d.Handler()
	mux.Handle("/api/", apiH)
	mux.Handle("/agent/", c.Fleet.AgentHandler())
	ui := c.UI
	if ui == nil {
		ui = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = w.Write([]byte("The SpaceMan web UI is not embedded in this build (build with `make build-ui`). The API is available under /api/v1.\n"))
		})
	}
	mux.Handle("/", ui)
	// A client that stops reading must not pin a response in memory (a browser tab holding hundreds of requests it never
	// reads, with central keeping every response): writes must make progress.
	return httpguard.WriteProgress(mux, c.Cfg.HTTP.WriteIdleTimeout.D())
}

// ManagementHandler serves /healthz, /readyz and /metrics (SM-OBS-002). Bind it to a private address.
func (c *Central) ManagementHandler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /healthz", c.Health.LivenessHandler())
	mux.Handle("GET /readyz", c.Health.ReadinessHandler())
	mux.Handle("GET /metrics", c.Metrics.Handler())
	return mux
}

// Serve runs every listener until ctx ends, then shuts down gracefully within the configured timeout.
func (c *Central) Serve(ctx context.Context) error {
	d := c.APIDeps()
	srv := &http.Server{Addr: c.Cfg.HTTP.Listen, Handler: c.Handler(d), ReadHeaderTimeout: c.Cfg.HTTP.ReadHeaderTimeout.D(), //nolint:contextcheck // handlers take their context from each request
		ErrorLog: slog.NewLogLogger(obs.Component(c.Log, "http").Handler(), slog.LevelWarn), BaseContext: func(net.Listener) context.Context { return ctx }}
	if c.Cfg.HTTP.TLSCertFile != "" {
		srv.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	errc := make(chan error, 2)
	go func() {
		var err error
		if c.Cfg.HTTP.TLSCertFile != "" {
			err = srv.ListenAndServeTLS(c.Cfg.HTTP.TLSCertFile, c.Cfg.HTTP.TLSKeyFile)
		} else {
			err = srv.ListenAndServe()
		}
		errc <- fmt.Errorf("http %s: %w", c.Cfg.HTTP.Listen, err)
	}()
	var mgmt *http.Server
	if c.Cfg.HTTP.ManagementListen != "" {
		mgmt = &http.Server{Addr: c.Cfg.HTTP.ManagementListen, Handler: c.ManagementHandler(), ReadHeaderTimeout: 10 * time.Second}
		go func() { errc <- fmt.Errorf("management %s: %w", c.Cfg.HTTP.ManagementListen, mgmt.ListenAndServe()) }()
	}
	// Executions that were running when central stopped have an unknown outcome; they are marked, never retried.
	if n, err := c.Approvals.Recover(ctx); err != nil {
		return fmt.Errorf("recover approval executions: %w", err)
	} else if n > 0 {
		c.Log.Warn("approved executions interrupted by a restart", slog.Int("count", n))
	}
	if err := c.Backups.Recover(ctx); err != nil {
		return fmt.Errorf("recover backup runs: %w", err)
	}
	go c.Backups.Run(ctx)
	if err := c.Retention.Recover(ctx); err != nil {
		return fmt.Errorf("recover retention passes: %w", err)
	}
	c.Retention.Bind(ctx)
	go c.Health.Run(ctx, 15*time.Second)
	go c.housekeeping(ctx)
	go c.uploadSpooledArtifacts(ctx)
	if err := c.Human.Recover(ctx); err != nil {
		return fmt.Errorf("schedule human-action reminders: %w", err)
	}
	// One timer for the earliest human-action deadline (expiry, reminder, escalation, notification hold) and approval
	// expiry, woken by changes; its first round routes responses stranded while central was down (SM-HUM-009/011).
	// Schedules and task deadlines share the timer (SM-LIMIT-003): nothing polls per schedule, task or space.
	go c.Human.Run(ctx, human.Deadline{Name: "approvals", Next: c.Approvals.NextExpiry, Due: c.Approvals.Expire},
		human.Deadline{Name: "schedules", Next: c.Schedules.Next, Due: c.Schedules.Due},
		human.Deadline{Name: "task_deadlines", Next: c.Limits.NextDeadline, Due: c.Limits.Deadlines},
		human.Deadline{Name: "xspace", Next: c.XSpace.NextDeadline, Due: c.XSpace.Expire},
		// Harbor Pilot run tokens (ADR-0060): queued revocations, and runs a node left unknown past its lease.
		human.Deadline{Name: "gateway_revocations", Next: c.HarborPilotRuns.NextRevocation, Due: c.HarborPilotRuns.RevokeDue},
		human.Deadline{Name: "gateway_lease_sweep", Next: c.Fleet.NextGatewayLeaseSweep, Due: c.Fleet.SweepGatewayLeases},
		// Runs counted as live whose process is gone (a node agent restart, a lost lease) end as lost (ADR-0074).
		human.Deadline{Name: "stale_run_sweep", Next: c.Fleet.NextStaleRunSweep, Due: c.Fleet.SweepStaleRuns},
		// Ended gateway runs' usage, read back from Harbor Pilot (HP-T5).
		human.Deadline{Name: "gateway_usage", Next: c.HarborPilotRuns.NextUsageRead, Due: c.HarborPilotRuns.ReadDueUsage},
		// The daily retention pass (ADR-0045): one due time for the whole installation, never per space.
		human.Deadline{Name: "retention", Next: c.Retention.Next, Due: c.Retention.Due})
	go c.Notifier.Run(ctx)
	// Relocations in progress resume where they stopped; retained source folders are deleted when their retention ends.
	go c.Fleet.RunRelocations(ctx)
	// Terminal takeovers end when their holder stops renewing them (one timer; SM-UI-004).
	go c.Fleet.RunTakeovers(ctx)
	go c.KB.Run(ctx)
	c.Repos.Bind(ctx)
	go c.Dreams.Run(ctx)
	c.Supervisor.Bind(ctx)
	// Notices deferred while a space was paused that a resume did not get to send before central stopped (SM-EVT-014).
	go c.Supervisor.FlushDeferred(ctx)
	go c.Supervisor.ArmIdleTimers(ctx)
	go events.NewManager(c.Events, c.Log).Run(ctx)
	c.Outbox.RefreshMetrics(ctx)
	if ob := c.Cfg.Events.Outbound; ob.Enabled {
		// Publication is separate from inbound subscriptions (SM-OBS-005) and never runs inside a transaction.
		go c.Outbox.Run(ctx, outbox.PulseDialer(outbox.PulseConfig{URL: ob.URL, CredsFile: ob.CredsFile, CAFile: ob.CAFile, Source: ob.Source}, c.Log))
	}
	c.Health.MarkReady()
	c.Log.Info("spaceman serving", slog.String("listen", c.Cfg.HTTP.Listen), slog.String("management", c.Cfg.HTTP.ManagementListen),
		slog.String("public_url", c.Cfg.PublicURL), slog.String("boot_id", c.BootID))

	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-errc:
	}
	c.Health.MarkStopping()
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.Cfg.Shutdown.Timeout.D())
	defer cancel()
	_ = srv.Shutdown(sctx)
	if mgmt != nil {
		_ = mgmt.Shutdown(sctx)
	}
	if runErr != nil && !errors.Is(runErr, http.ErrServerClosed) {
		return runErr
	}
	return nil
}

// housekeeping purges expired sessions, one-time tokens and idempotency records hourly (one goroutine for the whole
// process, SM-ARCH-004).
func (c *Central) housekeeping(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		if err := c.Auth.PurgeExpired(ctx); err != nil && ctx.Err() == nil {
			c.Log.Warn("purge expired auth records", obs.Err(err))
		}
		if _, err := c.OIDC.PurgeStates(ctx); err != nil && ctx.Err() == nil {
			c.Log.Warn("purge expired sign-in states", obs.Err(err))
		}
		if _, err := c.Events.PurgeDeliveries(ctx); err != nil && ctx.Err() == nil {
			c.Log.Warn("purge event deliveries", obs.Err(err))
		}
		if _, err := c.Outbox.Purge(ctx); err != nil && ctx.Err() == nil {
			c.Log.Warn("purge published events", obs.Err(err))
		}
		if rows, blobs, err := c.Attachments.GC(ctx); err != nil && ctx.Err() == nil {
			c.Log.Warn("attachment garbage collection", obs.Err(err))
		} else if rows+int64(blobs) > 0 {
			c.Log.Info("attachment garbage collection", slog.Int64("uploads", rows), slog.Int("contents", blobs))
		}
		_ = c.Store.W().TxNamed(ctx, "api.idempotency_purge", func(tx *store.Tx) error {
			_, err := tx.Q.PurgeIdempotency(ctx, tx.NowMicros())
			return err
		})
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// SpecDeps returns API dependencies for OpenAPI generation only: every operation group is registered, nothing runs.
func SpecDeps() *api.Deps {
	return &api.Deps{ExtraRules: rules(), Extra: []func(huma.API, *api.Deps){api.RegisterFleet(nil), api.RegisterRelocations(nil), api.RegisterRecordings(nil),
		api.RegisterAttachments(nil), api.RegisterInbox(nil), api.RegisterEvents(nil), api.RegisterQueue(nil, nil),
		api.RegisterHuman(nil), api.RegisterAccounts(nil), api.RegisterMessages(nil), api.RegisterCatalog(nil, nil), api.RegisterGateway(nil), api.RegisterApprovals(nil), api.RegisterMemory(nil), api.RegisterKB(nil), api.RegisterDreams(nil), api.RegisterUsage(nil), api.RegisterOIDC(nil), api.RegisterOutbox(nil), api.RegisterBackups(nil), api.RegisterLimits(nil, nil), api.RegisterXSpace(nil), api.RegisterRepositories(nil), api.RegisterGitKeys(nil), api.RegisterHarborPilot(nil, nil), api.RegisterRetention(nil, nil), api.RegisterAttention(nil), api.RegisterSystemStatus(nil)}}
}

// rules merges the authorization rules of every feature's operations.
func rules() map[string]api.Rule {
	out := map[string]api.Rule{}
	for _, m := range []map[string]api.Rule{api.FleetRules, api.RelocationRules, api.RecordingRules, api.AttachmentRules, api.InboxRules, api.EventRules, api.QueueRules, api.HumanRules, api.HumanTypePermissionRules, api.AccountRules, api.MessageRules, api.CatalogRules, api.GatewayRules, api.ApprovalRules, api.MemoryRules, api.KBRules, api.DreamRules, api.UsageRules, api.OIDCRules, api.OutboxRules, api.BackupRules, api.LimitRules, api.XSpaceRules, api.RepositoryRules, api.GitKeyRules, api.HarborPilotRules, api.RetentionRules, api.AttentionRules, api.SystemStatusRules} {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

// approverStillAuthorized rechecks, before an approved operation runs, that the person who approved it may still
// decide approvals in the space: the user is active, the API token they used (if any) is not revoked, the root token
// is current, and the policy still grants approval:decide (SM-APPR-003).
func (c *Central) approverStillAuthorized(ctx context.Context, by audit.Actor, spaceID string) bool {
	if c.Authz == nil {
		return false
	}
	var subjects []string
	switch by.Type {
	case audit.ActorRoot:
		rt, err := c.Store.R().GetActiveRootToken(ctx)
		if err != nil || rt.ID != by.ID {
			return false
		}
		subjects = []string{auth.RootSubject(by.ID)}
	case audit.ActorUser:
		if strings.HasPrefix(by.Via, auth.ViaSpacemanMCP) {
			// An agent acting through a SpaceMan MCP server is never an approver (ADR-0063).
			return false
		}
		u, err := c.Store.R().GetUser(ctx, by.ID)
		if err != nil || u.DisabledAt.Valid {
			return false
		}
		subjects = []string{auth.UserSubject(by.ID)}
		if strings.HasPrefix(by.Via, "sso:") {
			// Decided in an SSO session: its SSO-mapped grants count, as they did for the decision.
			subjects = []string{auth.SSOSubject(by.ID)}
		}
		if tok, ok := strings.CutPrefix(by.Via, "api_token:"); ok {
			t, err := c.Store.R().GetAPIToken(ctx, tok)
			if err != nil || t.RevokedAt.Valid || t.Purpose != "" {
				return false
			}
			subjects = append([]string{auth.TokenSubject(tok)}, subjects...)
		}
	default:
		return false
	}
	return c.Authz.AllowedAll(subjects, spaceID, authz.ApprovalDecide)
}

// usageWithBudgets stores usage, then checks the run's budgets: a warning is sent once when a budget passes its
// warning share or its limit (budgets never stop a run).
type usageWithBudgets struct {
	*usage.Service
	limits *limits.Service
}

func (u usageWithBudgets) Report(ctx context.Context, nodeID string, rep proto.UsageReport) {
	u.Service.Report(ctx, nodeID, rep)
	u.limits.OnUsage(ctx, rep.RunID)
}
