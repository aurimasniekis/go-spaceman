// Package metrics defines the SpaceMan Prometheus metric catalog (SM-OBS-002, site/operators/observability.md).
//
// Every metric lives on a dedicated registry (never the global default). Callers record events through small
// typed methods; label values are normalized against allowlists or bounded sets, so no call can create an
// unbounded number of series or put secrets, prompts, terminal output, space names or client addresses into labels.
// Space, node, run and task identifiers are never labels. A nil *Metrics is valid and every method on it is a no-op.
//
// Derived from github.com/aurimasniekis/go-hallpass internal/obs/metrics (Apache-2.0, same author).
package metrics

import (
	"errors"
	"maps"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	dto "github.com/prometheus/client_model/go"

	"github.com/aurimasniekis/go-spaceman/internal/buildinfo"
	"github.com/aurimasniekis/go-spaceman/internal/obs/fsstat"
	"github.com/aurimasniekis/go-spaceman/internal/obs/internal/httpwrap"
)

// Namespace prefixes every SpaceMan metric.
const Namespace = "spaceman"

// Histogram buckets (seconds).
var (
	// LatencyBuckets suit request/transaction latencies.
	LatencyBuckets = []float64{0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}
	// HTTPBuckets suit API requests.
	HTTPBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}
)

// Metrics holds every SpaceMan metric family.
type Metrics struct {
	reg *prometheus.Registry

	txNames, jobNames, oidcProviders, routes *bounded

	txDuration      *prometheus.HistogramVec
	txTotal         *prometheus.CounterVec
	txBusyRetries   prometheus.Counter
	writerWait      prometheus.Histogram
	writerQueue     prometheus.Gauge
	storeDBBytes    prometheus.Gauge
	storeWALBytes   prometheus.Gauge
	outboxPending   prometheus.Gauge
	outboxDead      prometheus.Gauge
	eventsPublished *prometheus.CounterVec
	eventsPending   prometheus.Gauge
	eventsDead      prometheus.Gauge
	eventsLag       prometheus.Gauge
	jobRuns         *prometheus.CounterVec
	backupRuns      *prometheus.CounterVec
	backupLastOK    prometheus.Gauge
	backupDuration  prometheus.Histogram
	httpRequests    *prometheus.CounterVec
	httpDuration    *prometheus.HistogramVec
	httpInFlight    prometheus.Gauge
	sseClients      prometheus.Gauge
	oidcLogins      *prometheus.CounterVec
	userLogins      *prometheus.CounterVec
	nodesConnected  prometheus.Gauge
	nodeConnects    *prometheus.CounterVec
	nodeCommands    *prometheus.CounterVec
	leaseFenced     prometheus.Counter
	runsActive      *prometheus.GaugeVec
	runsStarted     *prometheus.CounterVec
	runsEnded       *prometheus.CounterVec
	runsStale       *prometheus.CounterVec
	gwRevokeFailed  prometheus.Gauge
	ingest          ingestMetrics
	telegram        telegramMetrics
	fs              *fsCollector
	handlerTimeout  time.Duration
	handlerInFlight int
}

// New builds the registry with Go runtime and process collectors, build info, and every SpaceMan family.
func New() *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{
		reg:             reg,
		txNames:         newBounded(reDotted, MaxTxNames, 64, "unnamed"),
		jobNames:        newBounded(reDotted, MaxJobNames, 64, "unnamed"),
		oidcProviders:   newBounded(reSlug, MaxOIDCProvider, 48, "unknown"),
		routes:          newBounded(reRoute, MaxRoutes, 128, "unmatched"),
		fs:              newFSCollector(),
		handlerTimeout:  10 * time.Second,
		handlerInFlight: 4,
	}
	f := factory{reg: reg}

	bi := buildinfo.Get()
	f.gaugeVec("build_info", "Build information; always 1.", "version", "revision", "goversion").
		WithLabelValues(bi.Version, bi.Commit, bi.GoVersion).Set(1)

	m.txDuration = f.histogramVec("store_tx_duration_seconds", "Write transaction duration including retries, by transaction name.", LatencyBuckets, "name")
	m.txTotal = f.counterVec("store_tx_total", "Write transactions by name and final result.", "name", "result")
	m.txBusyRetries = f.counter("store_tx_busy_retries_total", "Write transaction attempts retried after SQLITE_BUSY.")
	m.writerWait = f.histogram("store_writer_wait_seconds", "Time spent waiting to acquire the single writer connection.", LatencyBuckets)
	m.writerQueue = f.gauge("store_writer_queue_depth", "Write transactions currently waiting for the single writer connection.")
	m.storeDBBytes = f.gauge("store_db_bytes", "SQLite main database file size.")
	m.storeWALBytes = f.gauge("store_wal_bytes", "SQLite WAL file size.")

	m.outboxPending = f.gauge("outbox_pending", "Outbox messages queued or awaiting retry.")
	m.outboxDead = f.gauge("outbox_dead", "Outbox messages dead-lettered after exhausting retries.")
	m.eventsPublished = f.counterVec("events_publish_total", "Pulse event publish attempts by outcome.", "outcome")
	m.eventsPending = f.gauge("events_pending", "Pulse events waiting to be published.")
	m.eventsDead = f.gauge("events_dead", "Pulse events dead-lettered after exhausting retries.")
	m.eventsLag = f.gauge("events_lag_seconds", "Age of the oldest Pulse event waiting to be published; 0 when none.")
	m.jobRuns = f.counterVec("job_runs_total", "Background job runs by job name and result.", "job", "result")
	m.backupRuns = f.counterVec("backup_runs_total", "Backup runs by result.", "result")
	m.backupLastOK = f.gauge("backup_last_success_timestamp_seconds", "Start time of the last successful backup (Unix seconds); 0 if none.")
	m.backupDuration = f.histogram("backup_duration_seconds", "Backup run duration.", []float64{1, 5, 15, 60, 300, 900, 3600, 10800})

	m.httpRequests = f.counterVec("http_requests_total", "HTTP requests by route template, method and status class.", "route", "method", "code")
	m.httpDuration = f.histogramVec("http_request_duration_seconds", "HTTP request duration by route template and method.", HTTPBuckets, "route", "method")
	m.httpInFlight = f.gauge("http_requests_in_flight", "HTTP requests currently being served.")
	m.sseClients = f.gauge("sse_clients", "Connected live-event (SSE) streams.")

	m.oidcLogins = f.counterVec("oidc_logins_total", "OIDC login completions by provider slug and outcome.", "provider", "outcome")
	m.userLogins = f.counterVec("user_logins_total", "User login attempts by method and outcome.", "method", "outcome")

	m.nodesConnected = f.gauge("nodes_connected", "Node agents currently connected.")
	m.nodeConnects = f.counterVec("node_connects_total", "Node agent connection attempts by result.", "result")
	m.nodeCommands = f.counterVec("node_commands_total", "Commands sent to nodes by command type and acknowledgement result.", "type", "result")
	m.leaseFenced = f.counter("execution_lease_fenced_total", "Node reports or commands rejected because their lease generation was stale.")
	m.runsActive = f.gaugeVec("runs_active", "Runs currently observed live, by harness.", "harness")
	m.runsStarted = f.counterVec("runs_started_total", "Run launches by harness and result.", "harness", "result")
	m.runsEnded = f.counterVec("runs_ended_total", "Runs ended by harness and reason.", "harness", "reason")
	m.runsStale = f.counterVec("runs_reconciled_stale_total", "Runs central's stale-run sweeper moved to lost, by end reason (ADR-0074).", "reason")
	m.gwRevokeFailed = f.gauge("gateway_revocations_failed", "LLM gateway (Harbor Pilot) credential revocations that failed: each credential stays valid until it expires unless a person revokes it, or its parent, in the gateway.")
	m.ingest = newIngestMetrics(f)
	m.telegram = newTelegramMetrics(f)

	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.fs,
	)
	return m
}

func (m *Metrics) Registry() *prometheus.Registry {
	if m == nil {
		return nil
	}
	return m.reg
}

// Register adds collectors to the registry.
func (m *Metrics) Register(cs ...prometheus.Collector) error {
	if m == nil {
		return nil
	}
	var errs []error
	for _, c := range cs {
		if err := m.reg.Register(c); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Handler serves the registry in the Prometheus exposition format. Scrapes are limited in concurrency and time.
func (m *Metrics) Handler() http.Handler {
	if m == nil {
		return http.NotFoundHandler()
	}
	h := promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{
		Registry:            m.reg,
		ErrorHandling:       promhttp.ContinueOnError,
		MaxRequestsInFlight: m.handlerInFlight,
		Timeout:             m.handlerTimeout,
	})
	return promhttp.InstrumentMetricHandler(m.reg, h)
}

// --- Store ---

// StoreTx observes a finished write transaction. name is the TxNamed name (dotted lowercase, bounded).
func (m *Metrics) StoreTx(name string, d time.Duration, r TxResult) {
	if m == nil {
		return
	}
	n := m.txNames.norm(name)
	m.txDuration.WithLabelValues(n).Observe(d.Seconds())
	m.txTotal.WithLabelValues(n, enumTxResult.norm(string(r))).Inc()
}

// StoreTxBusyRetry counts a write attempt retried after SQLITE_BUSY.
func (m *Metrics) StoreTxBusyRetry() {
	if m == nil {
		return
	}
	m.txBusyRetries.Inc()
}

// StoreWriterWait observes the time a caller waited for the single writer connection (Hallpass HP-DB-003).
func (m *Metrics) StoreWriterWait(d time.Duration) {
	if m == nil {
		return
	}
	m.writerWait.Observe(d.Seconds())
}

// StoreWriterQueue adjusts the number of write transactions waiting for the writer connection (Hallpass HP-DB-003).
func (m *Metrics) StoreWriterQueue(delta int) {
	if m == nil {
		return
	}
	m.writerQueue.Add(float64(delta))
}

// SetStoreSizes sets the database and WAL size gauges (Hallpass HP-DB-011), e.g. from store.Sizes after checkpoints.
func (m *Metrics) SetStoreSizes(dbBytes, walBytes int64) {
	if m == nil {
		return
	}
	m.storeDBBytes.Set(float64(dbBytes))
	m.storeWALBytes.Set(float64(walBytes))
}

// --- Outbox, jobs, backups ---

// SetOutboxBacklog sets the pending and dead-letter gauges (periodic refresh from the store).
func (m *Metrics) SetOutboxBacklog(pending, dead int) {
	if m == nil {
		return
	}
	m.outboxPending.Set(float64(pending))
	m.outboxDead.Set(float64(dead))
}

// EventPublish counts a Pulse event publish attempt.
func (m *Metrics) EventPublish(o EventOutcome) {
	if m == nil {
		return
	}
	m.eventsPublished.WithLabelValues(enumEvent.norm(string(o))).Inc()
}

// SetEventBacklog sets the pending and dead-letter gauges of the Pulse event outbox.
func (m *Metrics) SetEventBacklog(pending, dead int64) {
	if m == nil {
		return
	}
	m.eventsPending.Set(float64(pending))
	m.eventsDead.Set(float64(dead))
}

// SetEventLag sets the age of the oldest unpublished Pulse event (publication lag).
func (m *Metrics) SetEventLag(d time.Duration) {
	if m == nil {
		return
	}
	m.eventsLag.Set(d.Seconds())
}

// JobRun counts a background job run (job is the code-defined job name, bounded).
func (m *Metrics) JobRun(job string, err error) {
	if m == nil {
		return
	}
	r := "ok"
	if err != nil {
		r = "error"
	}
	m.jobRuns.WithLabelValues(m.jobNames.norm(job), enumJobResult.norm(r)).Inc()
}

// BackupRun counts a finished remote backup run (result: succeeded, failed).
func (m *Metrics) BackupRun(result string, d time.Duration) {
	if m == nil {
		return
	}
	if result != "succeeded" {
		result = "failed"
	}
	m.backupRuns.WithLabelValues(result).Inc()
	m.backupDuration.Observe(d.Seconds())
}

// SetBackupLastSuccess records the start time of the last successful remote backup.
func (m *Metrics) SetBackupLastSuccess(t time.Time) {
	if m == nil {
		return
	}
	if t.IsZero() {
		m.backupLastOK.Set(0)
		return
	}
	m.backupLastOK.Set(float64(t.Unix()))
}

// --- HTTP ---

// HTTPMiddleware records request count, duration and in-flight requests. The route label is the matched
// http.ServeMux pattern (path part only), read after the inner handler ran; unmatched requests are
// "unmatched". Raw paths, query strings and client addresses are never used as labels.
func (m *Metrics) HTTPMiddleware(next http.Handler) http.Handler {
	if m == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		m.httpInFlight.Inc()
		rec := httpwrap.New(w)
		defer func() {
			m.httpInFlight.Dec()
			m.HTTPRequest(r.Pattern, r.Method, rec.Status(), time.Since(start))
		}()
		next.ServeHTTP(rec, r)
	})
}

// HTTPRequest records one request. pattern is an http.ServeMux pattern or route template, never a raw path.
func (m *Metrics) HTTPRequest(pattern, method string, status int, d time.Duration) {
	if m == nil {
		return
	}
	route := httpwrap.Route(pattern)
	if route == "" && pattern != "" {
		route = Other
	} else {
		route = m.routes.norm(route)
	}
	meth := httpwrap.Method(method)
	m.httpRequests.WithLabelValues(route, meth, httpwrap.CodeClass(status)).Inc()
	m.httpDuration.WithLabelValues(route, meth).Observe(d.Seconds())
}

// SSEClientConnected increments the SSE stream gauge.
func (m *Metrics) SSEClientConnected() {
	if m == nil {
		return
	}
	m.sseClients.Inc()
}

// SSEClientDisconnected decrements the SSE stream gauge.
func (m *Metrics) SSEClientDisconnected() {
	if m == nil {
		return
	}
	m.sseClients.Dec()
}

// SSEClients returns the number of connected live-event (SSE) streams: watch and recording streams (the System page).
func (m *Metrics) SSEClients() int64 {
	if m == nil {
		return 0
	}
	var d dto.Metric
	if m.sseClients.Write(&d) != nil || d.Gauge == nil {
		return 0
	}
	return int64(d.Gauge.GetValue())
}

// --- User authentication ---

// OIDCLogin counts an OIDC login completion. provider is the configured provider slug (bounded).
func (m *Metrics) OIDCLogin(provider string, o LoginOutcome) {
	if m == nil {
		return
	}
	m.oidcLogins.WithLabelValues(m.oidcProviders.norm(provider), enumLoginOut.norm(string(o))).Inc()
}

// UserLogin counts a user login attempt.
func (m *Metrics) UserLogin(method LoginMethod, o LoginOutcome) {
	if m == nil {
		return
	}
	m.userLogins.WithLabelValues(enumLoginMeth.norm(string(method)), enumLoginOut.norm(string(o))).Inc()
}

// --- Nodes and runs ---

// NodeConnected adjusts the connected-nodes gauge.
func (m *Metrics) NodeConnected(delta int) {
	if m == nil {
		return
	}
	m.nodesConnected.Add(float64(delta))
}

// NodeConnect counts a node connection attempt.
func (m *Metrics) NodeConnect(r NodeConnectResult) {
	if m == nil {
		return
	}
	m.nodeConnects.WithLabelValues(enumNodeConnect.norm(string(r))).Inc()
}

// NodeCommand counts a command acknowledgement from a node.
func (m *Metrics) NodeCommand(t CommandType, r CommandResult) {
	if m == nil {
		return
	}
	m.nodeCommands.WithLabelValues(enumCommandType.norm(string(t)), enumCommandResult.norm(string(r))).Inc()
}

// LeaseFenced counts a report or command rejected for a stale lease generation.
func (m *Metrics) LeaseFenced() {
	if m == nil {
		return
	}
	m.leaseFenced.Inc()
}

// SetRunsActive sets the live-run gauge for a harness.
func (m *Metrics) SetRunsActive(h Harness, n int) {
	if m == nil {
		return
	}
	m.runsActive.WithLabelValues(enumHarness.norm(string(h))).Set(float64(n))
}

// RunStarted counts a launch outcome.
func (m *Metrics) RunStarted(h Harness, r RunStartResult) {
	if m == nil {
		return
	}
	m.runsStarted.WithLabelValues(enumHarness.norm(string(h)), enumRunStart.norm(string(r))).Inc()
}

// SetGatewayRevocationsFailed sets the number of gateway credential revocations that failed (refreshed from the store).
func (m *Metrics) SetGatewayRevocationsFailed(n int64) {
	if m == nil {
		return
	}
	m.gwRevokeFailed.Set(float64(n))
}

// RunEnded counts a run end.
func (m *Metrics) RunEnded(h Harness, r RunEndReason) {
	if m == nil {
		return
	}
	m.runsEnded.WithLabelValues(enumHarness.norm(string(h)), enumRunEnd.norm(string(r))).Inc()
}

// RunReconciledStale counts a run the stale-run sweeper ended.
func (m *Metrics) RunReconciledStale(r StaleReason) {
	if m == nil {
		return
	}
	m.runsStale.WithLabelValues(enumStaleReason.norm(string(r))).Inc()
}

// --- Filesystems ---

// ObserveFilesystem exports size and available bytes of the filesystem holding path under the given volume
// label (Hallpass HP-OPS-006). The path itself is never exported. Values are read at scrape time.
func (m *Metrics) ObserveFilesystem(v Volume, path string) {
	if m == nil || path == "" {
		return
	}
	name := enumVolume.norm(string(v))
	if name == Other {
		return
	}
	m.fs.mu.Lock()
	m.fs.paths[name] = path
	m.fs.mu.Unlock()
}

// fsCollector reads filesystem capacity at scrape time.
type fsCollector struct {
	mu          sync.Mutex
	paths       map[string]string
	size, avail *prometheus.Desc
}

func newFSCollector() *fsCollector {
	return &fsCollector{
		paths: map[string]string{},
		size: prometheus.NewDesc(prometheus.BuildFQName(Namespace, "", "storage_filesystem_size_bytes"),
			"Size of the filesystem holding a SpaceMan volume.", []string{"volume"}, nil),
		avail: prometheus.NewDesc(prometheus.BuildFQName(Namespace, "", "storage_filesystem_avail_bytes"),
			"Space available to SpaceMan on the filesystem holding a volume.", []string{"volume"}, nil),
	}
}

func (c *fsCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.size
	ch <- c.avail
}

func (c *fsCollector) Collect(ch chan<- prometheus.Metric) {
	c.mu.Lock()
	paths := maps.Clone(c.paths)
	c.mu.Unlock()
	for v, p := range paths {
		u, err := fsstat.Stat(p)
		if err != nil {
			continue
		}
		ch <- prometheus.MustNewConstMetric(c.size, prometheus.GaugeValue, float64(u.Total), v)
		ch <- prometheus.MustNewConstMetric(c.avail, prometheus.GaugeValue, float64(u.Avail), v)
	}
}

// factory registers families under the SpaceMan namespace.
type factory struct{ reg prometheus.Registerer }

func (f factory) counter(name, help string) prometheus.Counter {
	c := prometheus.NewCounter(prometheus.CounterOpts{Namespace: Namespace, Name: name, Help: help})
	f.reg.MustRegister(c)
	return c
}

func (f factory) counterVec(name, help string, labels ...string) *prometheus.CounterVec {
	c := prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: Namespace, Name: name, Help: help}, labels)
	f.reg.MustRegister(c)
	return c
}

func (f factory) gauge(name, help string) prometheus.Gauge {
	g := prometheus.NewGauge(prometheus.GaugeOpts{Namespace: Namespace, Name: name, Help: help})
	f.reg.MustRegister(g)
	return g
}

func (f factory) gaugeVec(name, help string, labels ...string) *prometheus.GaugeVec {
	g := prometheus.NewGaugeVec(prometheus.GaugeOpts{Namespace: Namespace, Name: name, Help: help}, labels)
	f.reg.MustRegister(g)
	return g
}

func (f factory) histogram(name, help string, buckets []float64) prometheus.Histogram {
	h := prometheus.NewHistogram(prometheus.HistogramOpts{Namespace: Namespace, Name: name, Help: help, Buckets: buckets})
	f.reg.MustRegister(h)
	return h
}

func (f factory) histogramVec(name, help string, buckets []float64, labels ...string) *prometheus.HistogramVec {
	h := prometheus.NewHistogramVec(prometheus.HistogramOpts{Namespace: Namespace, Name: name, Help: help, Buckets: buckets}, labels)
	f.reg.MustRegister(h)
	return h
}
