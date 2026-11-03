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
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.yaml.in/yaml/v3"

	"github.com/aurimasniekis/go-spaceman/internal/accounts"
	"github.com/aurimasniekis/go-spaceman/internal/agenttools"
	"github.com/aurimasniekis/go-spaceman/internal/api"
	"github.com/aurimasniekis/go-spaceman/internal/approvals"
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
	"github.com/aurimasniekis/go-spaceman/internal/human"
	"github.com/aurimasniekis/go-spaceman/internal/inbox"
	"github.com/aurimasniekis/go-spaceman/internal/kb"
	"github.com/aurimasniekis/go-spaceman/internal/keyring"
	"github.com/aurimasniekis/go-spaceman/internal/memory"
	"github.com/aurimasniekis/go-spaceman/internal/messages"
	"github.com/aurimasniekis/go-spaceman/internal/obs"
	"github.com/aurimasniekis/go-spaceman/internal/obs/health"
	"github.com/aurimasniekis/go-spaceman/internal/obs/metrics"
	"github.com/aurimasniekis/go-spaceman/internal/oidc"
	"github.com/aurimasniekis/go-spaceman/internal/outbox"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/queue"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/usage"
)

// Central holds the running central service.
type Central struct {
	Cfg     *config.Config
	Log     *slog.Logger
	Clock   clock.Clock
	Store   *store.Store
	Keyring *keyring.Keyring
	Audit   *audit.Writer
	Authz   *authz.Engine
	Auth    *auth.Service
	Bus     *eventbus.Bus
	Changes *changefeed.Recorder
	Metrics *metrics.Metrics
	Health  *health.Checker
	Fleet   *fleet.Service
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
	// Tools are the built-in agent tools served through the MCP bridge (SM-MCP-003).
	Tools *agenttools.Registry
	// Gateway serves external MCP servers' tools to granted runs (ADR-0023).
	Gateway *gateway.Service
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
	// Restored is the restore applied at this start, or nil (SM-STORE-006).
	Restored *backup.RestoreState

	// UI serves the embedded web UI (set by cmd/spaceman; a notice handler when the UI is not embedded).
	UI http.Handler
}

// Options adjust OpenCentral (tests).
type Options struct {
	Clock   clock.Clock
	Environ []string
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
		AllowNetworkFS: !cfg.IsProduction() && os.Getenv("SPACEMAN_ALLOW_NETWORK_FS") == "1"})
	if err != nil {
		return nil, err
	}
	c := &Central{Cfg: cfg, Log: log, Clock: o.Clock, Store: st, Metrics: m, Bus: eventbus.New()}
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
	c.Fleet = fleet.New(st, c.Keyring, c.Audit, c.Bus, c.Changes, fleet.Options{HeartbeatInterval: cfg.Nodes.HeartbeatInterval.D(),
		LeaseTTL: cfg.Nodes.LeaseTTL.D(), DisconnectGrace: cfg.Nodes.DisconnectGrace.D(), EnrollmentTTL: cfg.Nodes.EnrollmentTTL.D(), Clock: o.Clock,
		Log: log, Metrics: m, RecordingsDir: st.Path("recordings"), ResolveAccount: accounts.Resolve,
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
			in.Skills = nil
			for _, sk := range r.Skills {
				in.Skills = append(in.Skills, proto.Skill{Name: sk.Slug, Version: sk.Number, SHA256: sk.SHA256, Files: sk.Files})
			}
			if c.Gateway != nil {
				// What the role is granted at launch; every call is checked again against the grants of that moment.
				mcp, err := c.Gateway.ForManifest(ctx, spaceID, in.Role)
				if err != nil {
					return nil, err
				}
				r.Manifest["mcp_servers"] = mcp
			}
			return r.Manifest, nil
		},
		// The notifier is created below; these hooks only run once the server serves.
		OnRunEnded: func(runID string) {
			if c.Gateway != nil {
				c.Gateway.EndRun(runID)
			}
			if c.KB != nil {
				c.KB.OnRunEnded(runID)
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
		OnRunIdle: func(string) {
			if c.Notifier != nil {
				c.Notifier.Kick()
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
		Outbox: c.Outbox})
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
	if cfg.Artifacts.Backend != "local" {
		_ = st.Close()
		return nil, fmt.Errorf("artifacts.backend %q is not implemented yet; use \"local\"", cfg.Artifacts.Backend)
	}
	artifactDir := cfg.Artifacts.LocalDir
	if artifactDir == "" {
		artifactDir = st.Path("artifacts")
	}
	c.Attachments = attachments.New(st, attachments.NewLocal(artifactDir), c.Audit, attachments.Options{MaxBytes: cfg.HTTP.MaxUploadBytes,
		QuotaBytes: cfg.Artifacts.SpaceQuotaBytes, Clock: o.Clock})
	c.Inbox = inbox.New(st, c.Audit, c.Changes, c.Attachments)
	c.Inbox.Outbox = c.Outbox
	c.Queue = queue.New(st, c.Audit, c.Changes)
	c.Queue.Outbox = c.Outbox
	c.Human = human.New(st, c.Audit, c.Changes, o.Clock)
	c.Human.Outbox = c.Outbox
	c.Human.Log = obs.Component(log, "human")
	c.Messages = messages.New(st, c.Audit, c.Changes)
	c.Notifier = messages.NewNotifier(c.Messages, c.Fleet, log, o.Clock)
	c.Human.OnNotify = c.Notifier.Kick
	c.Supervisor = control.NewSupervisor(c.Fleet, c.Queue, c.Messages, c.Human, log, o.Clock)
	c.Inbox.OnCreated = c.Supervisor.OnInboxItem
	c.Queue.OnStatus = c.Supervisor.OnTaskStatus
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
	c.KB = kb.New(st, c.Audit, c.Changes, c.Fleet, c.Messages, o.Clock, obs.Component(log, "kb"))
	c.KB.Outbox = c.Outbox
	c.Fleet.Hub().KBResult = c.KB.OnResult
	c.Dreams = dream.New(st, c.Audit, c.Changes, c.Fleet, o.Clock, obs.Component(log, "dreamer"))
	c.Usage = usage.New(st, c.Audit, c.Changes, st.Path("transcripts"), obs.Component(log, "usage"))
	target, err := backup.TargetFromConfig(cfg)
	if err != nil {
		_ = st.Close()
		return nil, err
	}
	bc := cfg.Backups
	c.Backups = backup.NewService(st, c.Keyring.Wrapper(), c.Audit, c.Changes, backup.ServiceOptions{Target: target,
		Sources: backup.DefaultSources(st.Dir(), artifactDir, gitBin), Config: func() []byte { b, _ := yaml.Marshal(cfg); return b },
		Schedule: bc.Enabled, Interval: bc.Interval.D(), KeepLast: bc.KeepLast, MaxAge: bc.MaxAge.D(), Clock: o.Clock, Log: obs.Component(log, "backup")})
	c.Fleet.Hub().Usage = c.Usage
	c.Gateway.Gate = c.Approvals
	c.Tools = agenttools.New(agenttools.Deps{Inbox: c.Inbox, Queue: c.Queue, Attachments: c.Attachments, Human: c.Human,
		Extra: []func(*agenttools.Registry){messages.Tools(c.Messages), control.Tools(c.Fleet, c.Queue), memory.Tools(c.Memory), kb.Tools(c.KB), dream.Tools(c.Dreams)}, Upstream: c.Gateway, Gate: c.Approvals})
	c.Fleet.Hub().Tools = c.Tools.Dispatch
	evOpts := events.Options{Clock: o.Clock}
	if ob.Enabled {
		evOpts.Origin = ob.Source // never re-ingest our own events (SM-EVT-016)
	}
	c.Events = events.New(st, c.Keyring, c.Audit, c.Changes, c.Inbox, c.Attachments, evOpts)
	if err := c.Catalog.SeedBuiltins(ctx); err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("seed skills: %w", err)
	}
	if err := c.Events.SeedBuiltins(ctx); err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("seed event templates: %w", err)
	}
	c.Health = health.New(health.Options{ExposeDetails: !cfg.IsProduction()})
	c.Health.MustRegister(
		health.StoreHealth("store", st.Health()),
		health.StoreWritable("store.writable", st.W()),
		health.DiskSpace("disk.data", cfg.DataDir, 256<<20, 2),
	)
	_ = m.Register(c.Health.Collector())
	m.ObserveFilesystem(metrics.VolumeData, cfg.DataDir)
	return c, nil
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

// Close closes node connections, then releases storage and the data-directory lock.
func (c *Central) Close() error {
	if c.Backups != nil {
		c.Backups.Wait()
	}
	if c.Fleet != nil {
		c.Fleet.Hub().CloseAll("central shutting down")
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
		Extra: []func(huma.API, *api.Deps){api.RegisterFleet(c.Fleet), api.RegisterRecordings(c.Fleet), api.RegisterAttachments(c.Attachments),
			api.RegisterInbox(c.Inbox), api.RegisterEvents(c.Events),
			api.RegisterQueue(c.Queue, c.Inbox), api.RegisterHuman(c.Human), api.RegisterAccounts(c.Accounts), api.RegisterMessages(c.Messages), api.RegisterCatalog(c.Catalog, c.Store), api.RegisterGateway(c.Gateway), api.RegisterApprovals(c.Approvals), api.RegisterMemory(c.Memory), api.RegisterKB(c.KB), api.RegisterDreams(c.Dreams), api.RegisterUsage(c.Usage), api.RegisterOIDC(c.OIDC), api.RegisterOutbox(c.Outbox), api.RegisterBackups(c.Backups)},
		Raw: append(api.RecordingStreams(c.Fleet), api.AttachmentContent(c.Attachments), api.TranscriptDownload(c.Usage))}
	d.Ready = func() bool { r, ok := c.Health.Last(); return ok && r.Status == health.StatusReady }
	return d
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
	return mux
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
	go c.Health.Run(ctx, 15*time.Second)
	go c.housekeeping(ctx)
	if err := c.Human.Recover(ctx); err != nil {
		return fmt.Errorf("schedule human-action reminders: %w", err)
	}
	// One timer for the earliest human-action deadline (expiry, reminder, escalation, notification hold) and approval
	// expiry, woken by changes; its first round routes responses stranded while central was down (SM-HUM-009/011).
	go c.Human.Run(ctx, human.Deadline{Name: "approvals", Next: c.Approvals.NextExpiry, Due: c.Approvals.Expire})
	go c.Notifier.Run(ctx)
	go c.KB.Run(ctx)
	go c.Dreams.Run(ctx)
	c.Supervisor.Bind(ctx)
	go events.NewManager(c.Events, c.Log).Run(ctx)
	c.Outbox.RefreshMetrics(ctx)
	if ob := c.Cfg.Events.Outbound; ob.Enabled {
		// Publication is separate from inbound subscriptions (SM-OBS-005) and never runs inside a transaction.
		go c.Outbox.Run(ctx, outbox.PulseDialer(outbox.PulseConfig{URL: ob.URL, CredsFile: ob.CredsFile, CAFile: ob.CAFile, Source: ob.Source}, c.Log))
	}
	c.Health.MarkReady()
	c.Log.Info("spaceman serving", slog.String("listen", c.Cfg.HTTP.Listen), slog.String("management", c.Cfg.HTTP.ManagementListen),
		slog.String("public_url", c.Cfg.PublicURL))

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
	return &api.Deps{ExtraRules: rules(), Extra: []func(huma.API, *api.Deps){api.RegisterFleet(nil), api.RegisterRecordings(nil),
		api.RegisterAttachments(nil), api.RegisterInbox(nil), api.RegisterEvents(nil), api.RegisterQueue(nil, nil),
		api.RegisterHuman(nil), api.RegisterAccounts(nil), api.RegisterMessages(nil), api.RegisterCatalog(nil, nil), api.RegisterGateway(nil), api.RegisterApprovals(nil), api.RegisterMemory(nil), api.RegisterKB(nil), api.RegisterDreams(nil), api.RegisterUsage(nil), api.RegisterOIDC(nil), api.RegisterOutbox(nil), api.RegisterBackups(nil)}}
}

// rules merges the authorization rules of every feature's operations.
func rules() map[string]api.Rule {
	out := map[string]api.Rule{}
	for _, m := range []map[string]api.Rule{api.FleetRules, api.RecordingRules, api.AttachmentRules, api.InboxRules, api.EventRules, api.QueueRules, api.HumanRules, api.AccountRules, api.MessageRules, api.CatalogRules, api.GatewayRules, api.ApprovalRules, api.MemoryRules, api.KBRules, api.DreamRules, api.UsageRules, api.OIDCRules, api.OutboxRules, api.BackupRules} {
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
			if err != nil || t.RevokedAt.Valid {
				return false
			}
			subjects = append([]string{auth.TokenSubject(tok)}, subjects...)
		}
	default:
		return false
	}
	return c.Authz.AllowedAll(subjects, spaceID, authz.ApprovalDecide)
}
