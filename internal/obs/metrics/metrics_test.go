package metrics

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
)

// exercise records one event through every method so every vector family has at least one series.
func exercise(m *Metrics) {
	m.StoreTx("space.create", 3*time.Millisecond, TxOK)
	m.StoreTxBusyRetry()
	m.StoreWriterWait(time.Millisecond)
	m.StoreWriterQueue(1)
	m.SetStoreSizes(4096, 1024)
	m.SetOutboxBacklog(2, 1)
	m.EventPublish(EventPublished)
	m.SetEventBacklog(3, 0)
	m.JobRun("store.wal_checkpoint", nil)
	m.BackupRun("succeeded", time.Second)
	m.SetBackupLastSuccess(time.Now())
	m.HTTPRequest("GET /api/v1/spaces/{id}", http.MethodGet, 200, time.Millisecond)
	m.SSEClientConnected()
	m.SSEClientDisconnected()
	m.OIDCLogin("porch", LoginSuccess)
	m.OperatorLogin(LoginPassword, LoginFailure)
	m.NodeConnected(1)
	m.NodeConnect(NodeAccepted)
	m.NodeCommand(CmdLaunch, CmdAccepted)
	m.LeaseFenced()
	m.SetRunsActive(HarnessClaude, 2)
	m.RunStarted(HarnessCodex, RunAdopted)
	m.RunEnded(HarnessClaude, RunLeaseExpired)
}

func gather(t *testing.T, m *Metrics) map[string]*dto.MetricFamily {
	t.Helper()
	fams, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	out := map[string]*dto.MetricFamily{}
	for _, f := range fams {
		out[f.GetName()] = f
	}
	return out
}

func labelNames(f *dto.MetricFamily) string {
	if len(f.GetMetric()) == 0 {
		return ""
	}
	var names []string
	for _, l := range f.GetMetric()[0].GetLabel() {
		names = append(names, l.GetName())
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}

// TestCatalogGolden pins the metric catalog (names, types and label names) documented in
// docs/operator/observability.md. Changing it is a documented, reviewed change.
func TestCatalogGolden(t *testing.T) {
	m := New()
	m.ObserveFilesystem(VolumeData, t.TempDir())
	exercise(m)
	want := map[string]string{
		"spaceman_build_info":                            "gauge:goversion,revision,version",
		"spaceman_store_tx_duration_seconds":             "histogram:name",
		"spaceman_store_tx_total":                        "counter:name,result",
		"spaceman_store_tx_busy_retries_total":           "counter:",
		"spaceman_store_writer_wait_seconds":             "histogram:",
		"spaceman_store_writer_queue_depth":              "gauge:",
		"spaceman_store_db_bytes":                        "gauge:",
		"spaceman_store_wal_bytes":                       "gauge:",
		"spaceman_outbox_pending":                        "gauge:",
		"spaceman_outbox_dead":                           "gauge:",
		"spaceman_events_publish_total":                  "counter:outcome",
		"spaceman_events_pending":                        "gauge:",
		"spaceman_events_dead":                           "gauge:",
		"spaceman_job_runs_total":                        "counter:job,result",
		"spaceman_backup_runs_total":                     "counter:result",
		"spaceman_backup_last_success_timestamp_seconds": "gauge:",
		"spaceman_backup_duration_seconds":               "histogram:",
		"spaceman_http_requests_total":                   "counter:code,method,route",
		"spaceman_http_request_duration_seconds":         "histogram:method,route",
		"spaceman_http_requests_in_flight":               "gauge:",
		"spaceman_sse_clients":                           "gauge:",
		"spaceman_oidc_logins_total":                     "counter:outcome,provider",
		"spaceman_operator_logins_total":                 "counter:method,outcome",
		"spaceman_nodes_connected":                       "gauge:",
		"spaceman_node_connects_total":                   "counter:result",
		"spaceman_node_commands_total":                   "counter:result,type",
		"spaceman_execution_lease_fenced_total":          "counter:",
		"spaceman_runs_active":                           "gauge:harness",
		"spaceman_runs_started_total":                    "counter:harness,result",
		"spaceman_runs_ended_total":                      "counter:harness,reason",
		"spaceman_storage_filesystem_size_bytes":         "gauge:volume",
		"spaceman_storage_filesystem_avail_bytes":        "gauge:volume",
	}
	fams := gather(t, m)
	got := map[string]string{}
	for name, f := range fams {
		if strings.HasPrefix(name, "spaceman_") {
			got[name] = strings.ToLower(f.GetType().String()) + ":" + labelNames(f)
		}
	}
	for name, w := range want {
		if got[name] != w {
			t.Errorf("%s: got %q, want %q", name, got[name], w)
		}
	}
	for name := range got {
		if _, ok := want[name]; !ok {
			t.Errorf("undocumented metric %s (add it to the golden list and docs/operator/observability.md)", name)
		}
	}
	for _, std := range []string{"go_goroutines", "go_memstats_alloc_bytes"} {
		if _, ok := fams[std]; !ok {
			t.Errorf("missing Go collector metric %s", std)
		}
	}
}

func TestDedicatedRegistry(t *testing.T) {
	a, b := New(), New() // would panic on duplicate registration if the global registry were used
	if a.Registry() == b.Registry() {
		t.Fatal("registries must be independent")
	}
}

func seriesValues(t *testing.T, m *Metrics, family, label string) []string {
	t.Helper()
	f := gather(t, m)[family]
	if f == nil {
		return nil
	}
	var vals []string
	for _, mt := range f.GetMetric() {
		for _, l := range mt.GetLabel() {
			if l.GetName() == label {
				vals = append(vals, l.GetValue())
			}
		}
	}
	sort.Strings(vals)
	return slices.Compact(vals)
}

func TestEnumLabelsNormalized(t *testing.T) {
	m := New()
	const planted = "pl4nted-s3cret reason; rm -rf /"
	m.NodeConnect(NodeConnectResult(planted))
	m.NodeConnect(NodeRevoked)
	m.NodeCommand(CommandType(planted), CommandResult(planted))
	m.SetRunsActive(Harness(planted), 1)
	m.RunStarted(Harness(planted), RunStartResult(planted))
	m.RunEnded(Harness(planted), RunEndReason(planted))
	m.StoreTx("x", 0, TxResult(planted))
	m.OIDCLogin(planted, LoginOutcome(planted))
	m.OperatorLogin(LoginMethod(planted), LoginOutcome(planted))
	m.HTTPRequest("GET /a/"+planted, "BREW", 999, 0)

	if got := seriesValues(t, m, "spaceman_node_connects_total", "result"); !slices.Equal(got, []string{"other", "revoked"}) {
		t.Errorf("node connect results = %v", got)
	}
	if got := seriesValues(t, m, "spaceman_http_requests_total", "method"); !slices.Equal(got, []string{"other"}) {
		t.Errorf("methods = %v", got)
	}
	if got := seriesValues(t, m, "spaceman_http_requests_total", "code"); !slices.Equal(got, []string{"other"}) {
		t.Errorf("codes = %v", got)
	}
	body := scrape(t, m)
	if strings.Contains(body, "pl4nted") || strings.Contains(body, "rm -rf") {
		t.Fatalf("free-form value reached the exposition:\n%s", grepLines(body, "pl4nted"))
	}
}

func TestBoundedLabels(t *testing.T) {
	m := New()
	for i := range 3 * MaxTxNames {
		m.StoreTx(fmt.Sprintf("pkg.tx_%d", i), time.Millisecond, TxOK)
	}
	m.StoreTx("", 0, TxOK)
	m.StoreTx("Bad Name!", 0, TxOK)
	m.StoreTx(strings.Repeat("a", 65), 0, TxOK)
	names := seriesValues(t, m, "spaceman_store_tx_total", "name")
	if len(names) != MaxTxNames+2 { // cap + "other" + "unnamed"
		t.Errorf("tx names = %d, want %d", len(names), MaxTxNames+2)
	}
	for _, want := range []string{"other", "unnamed", "pkg.tx_0"} {
		if !slices.Contains(names, want) {
			t.Errorf("missing %q", want)
		}
	}

	for i := range 2 * MaxOIDCProvider {
		m.OIDCLogin(fmt.Sprintf("idp-%d", i), LoginSuccess)
	}
	m.OIDCLogin("", LoginError)
	m.OIDCLogin("UPPER", LoginError)
	if got := seriesValues(t, m, "spaceman_oidc_logins_total", "provider"); len(got) != MaxOIDCProvider+2 {
		t.Errorf("oidc providers = %d (%v)", len(got), got)
	}

	for i := range 2 * MaxJobNames {
		m.JobRun(fmt.Sprintf("job_%d", i), errors.New("x"))
	}
	if got := seriesValues(t, m, "spaceman_job_runs_total", "job"); len(got) != MaxJobNames+1 {
		t.Errorf("job names = %d", len(got))
	}
	if got := seriesValues(t, m, "spaceman_job_runs_total", "result"); !slices.Equal(got, []string{"error"}) {
		t.Errorf("job results = %v", got)
	}

	for i := range 2 * MaxRoutes {
		m.HTTPRequest(fmt.Sprintf("GET /r/%d", i), http.MethodGet, 200, 0)
	}
	m.HTTPRequest("", http.MethodGet, 404, 0)
	routes := seriesValues(t, m, "spaceman_http_requests_total", "route")
	if len(routes) != MaxRoutes+2 || !slices.Contains(routes, "unmatched") || !slices.Contains(routes, "other") {
		t.Errorf("routes = %d", len(routes))
	}
}

func TestNilSafe(t *testing.T) {
	var m *Metrics
	exercise(m)
	m.ObserveFilesystem(VolumeData, "/")
	_ = m.TxObserver(nil)
	if m.Registry() != nil {
		t.Error("nil registry expected")
	}
	if err := m.Register(nil); err != nil {
		t.Error(err)
	}
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	if h := m.HTTPMiddleware(next); fmt.Sprintf("%p", h) != fmt.Sprintf("%p", next) {
		t.Error("nil middleware must return next")
	}
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("nil handler code = %d", rec.Code)
	}
}

func scrape(t *testing.T, m *Metrics) string {
	t.Helper()
	srv := httptest.NewServer(m.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("scrape status %d", resp.StatusCode)
	}
	return string(b)
}

func grepLines(s, sub string) string {
	var out []string
	for l := range strings.SplitSeq(s, "\n") {
		if strings.Contains(l, sub) {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

func TestHandlerExposition(t *testing.T) {
	m := New()
	exercise(m)
	body := scrape(t, m)
	for _, want := range []string{
		`spaceman_build_info{goversion="`,
		`spaceman_node_commands_total{result="accepted",type="run.launch"} 1`,
		`spaceman_runs_active{harness="claude-code"} 2`,
		`spaceman_http_requests_total{code="2xx",method="GET",route="/api/v1/spaces/{id}"} 1`,
		`spaceman_outbox_dead 1`,
		`spaceman_execution_lease_fenced_total 1`,
		"go_goroutines",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("exposition missing %q", want)
		}
	}
	// promhttp self-instrumentation lands on the next scrape.
	if body := scrape(t, m); !strings.Contains(body, "promhttp_metric_handler_requests_total") {
		t.Error("missing promhttp handler metrics")
	}
}

func TestHTTPMiddlewareUsesRouteTemplate(t *testing.T) {
	m := New()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/items/{id}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = io.WriteString(w, "x")
		http.NewResponseController(w).Flush() //nolint:errcheck // exercises Flush passthrough
	})
	h := m.HTTPMiddleware(mux)
	for _, target := range []string{"/api/v1/items/act-t0ken-abc?token=s3cret", "/nope/act-t0ken-abc"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if !rec.Flushed && rec.Code == http.StatusTeapot {
			t.Error("flush not forwarded")
		}
	}
	body := scrape(t, m)
	if !strings.Contains(body, `spaceman_http_requests_total{code="4xx",method="GET",route="/api/v1/items/{id}"} 1`) {
		t.Errorf("templated route missing:\n%s", grepLines(body, "http_requests_total"))
	}
	if !strings.Contains(body, `route="unmatched"`) {
		t.Error("unmatched route missing")
	}
	for _, leak := range []string{"t0ken", "s3cret", "/nope"} {
		if strings.Contains(body, leak) {
			t.Errorf("raw path leaked into labels: %q", leak)
		}
	}
	if !strings.Contains(body, "spaceman_http_requests_in_flight 0") {
		t.Error("in-flight gauge not back to zero")
	}
}

func TestTxObserver(t *testing.T) {
	m := New()
	errBusy := errors.New("busy")
	obsFn := m.TxObserver(func(err error) TxResult {
		if errors.Is(err, errBusy) {
			return TxBusy
		}
		return TxError
	})
	obsFn("a.ok", time.Millisecond, nil)
	obsFn("a.busy", time.Millisecond, fmt.Errorf("wrap: %w", errBusy))
	obsFn("a.cancel", time.Millisecond, context.Canceled)
	obsFn("a.other", time.Millisecond, errors.New("boom"))
	m.TxObserver(nil)("a.nil", time.Millisecond, errors.New("boom"))
	body := scrape(t, m)
	for _, want := range []string{
		`spaceman_store_tx_total{name="a.ok",result="ok"} 1`,
		`spaceman_store_tx_total{name="a.busy",result="busy"} 1`,
		`spaceman_store_tx_total{name="a.cancel",result="canceled"} 1`,
		`spaceman_store_tx_total{name="a.other",result="error"} 1`,
		`spaceman_store_tx_total{name="a.nil",result="error"} 1`,
		`spaceman_store_tx_duration_seconds_count{name="a.ok"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
}

func TestGaugesAndFilesystem(t *testing.T) {
	m := New()
	dir := t.TempDir()
	m.ObserveFilesystem(VolumeArtifacts, dir)
	m.ObserveFilesystem(Volume("evil"), dir)
	m.ObserveFilesystem(VolumeBackups, dir+"/does-not-exist")
	m.ObserveFilesystem(VolumeData, "")
	m.NodeConnected(1)
	m.NodeConnected(1)
	m.NodeConnected(-1)
	m.SetRunsActive(HarnessCodex, 3)
	m.SetRunsActive(HarnessCodex, 1)
	body := scrape(t, m)
	for _, want := range []string{
		"spaceman_nodes_connected 1",
		`spaceman_runs_active{harness="codex"} 1`,
		`spaceman_storage_filesystem_size_bytes{volume="artifacts"}`,
		`spaceman_storage_filesystem_avail_bytes{volume="artifacts"}`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	for _, bad := range []string{`volume="evil"`, `volume="other"`, `volume="backups"`, `volume="data"`, dir} {
		if strings.Contains(body, bad) {
			t.Errorf("unexpected %q in exposition", bad)
		}
	}
}

func TestRegister(t *testing.T) {
	m := New()
	if err := m.Register(m.fs); err == nil {
		t.Error("duplicate registration must fail")
	}
}
