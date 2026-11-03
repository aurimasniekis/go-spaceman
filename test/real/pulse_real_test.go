//go:build real

package real

// TestRealPulseGitHub (SM-VER-002) runs SpaceMan's event paths against a real Pulse deployment instead of the embedded
// Pulse-shaped nats-server of the simulated suites:
//
//   - pulsed v0.1.3 and the pulse CLI built from the sibling checkout (SPACEMAN_PULSE_SRC) into a temp dir, `pulsed
//     init` (JWT operator mode, internal CA), the system nats-server (2.15.x, as in Pulse's compose files) on the
//     nats.conf pulsed wrote, then `pulsed serve`. Clients and grants are created through pulsed's admin API with an
//     automation token from `pulsed admin token`, exactly as an operator would.
//   - go-pulse-github v0.1.0 built from SPACEMAN_PULSE_GITHUB_SRC and run as the real `pulse-github run` service. Its
//     own fixtures (testdata/webhooks/issues_opened.json, push.json) are POSTed as signed webhooks; the service
//     normalizes them and publishes through its outbox with the real go-pulse SDK. Webhooks need GitHub App
//     installation lookups, so a local HTTPS stand-in for the GitHub API answers GET /app/installations/555 with the
//     response pulse-github's own end-to-end test uses (internal/app/app_test.go). Nothing reaches github.com.
//   - The real spaceman central built from this tree, with an inbound connection, subscriptions in two spaces and
//     outbound publication (events.outbound) to the same pulsed.
//
// Opt-in: SPACEMAN_REAL_PULSE=1 (make test-real-pulse). Every credential (master key, admin token, .creds, webhook
// secret, App key) is generated at run time into the test's temp dir and never logged. Only processes and the temp
// dir this test created are cleaned up.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/aurimasniekis/go-pulse/pkg/pulse"
)

// pulseStack is a running pulsed + nats-server pair started by the test.
type pulseStack struct {
	t        *testing.T
	dir      string
	bin      string
	envv     []string
	natsURL  string
	adminURL string
	pat      string
	caPEM    string
	procs    []*exec.Cmd
}

// pulseEnv is the process environment for sibling binaries: the developer's PULSE_*, HC_*, SPACEMAN_* and harness
// variables never reach them.
func pulseEnv(extra ...string) []string {
	var out []string
	for _, kv := range cleanEnv() {
		if !strings.HasPrefix(kv, "PULSE_") && !strings.HasPrefix(kv, "NATS_") {
			out = append(out, kv)
		}
	}
	return append(out, extra...)
}

func pulseBuild(t *testing.T, src, pkg, out string) {
	t.Helper()
	cmd := exec.Command("go", "build", "-o", out, pkg)
	cmd.Dir = src
	cmd.Env = append(pulseEnv(), "GOFLAGS=-mod=readonly", "GOWORK=off")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build %s in %s: %v\n%s", pkg, src, err, b)
	}
}

func pulseGitDescribe(dir string) string {
	out, err := exec.Command("git", "-C", dir, "describe", "--tags", "--always", "--dirty").Output()
	if err != nil {
		return "?"
	}
	rev, _ := exec.Command("git", "-C", dir, "rev-parse", "--short=12", "HEAD").Output()
	return strings.TrimSpace(string(out)) + " (" + strings.TrimSpace(string(rev)) + ")"
}

func pulseStartProc(t *testing.T, s *pulseStack, logName string, env []string, name string, args ...string) *exec.Cmd {
	t.Helper()
	f, err := os.Create(filepath.Join(s.dir, logName))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(name, args...)
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = f, f
	must(t, cmd.Start())
	s.procs = append(s.procs, cmd)
	return cmd
}

func pulseStop(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan struct{})
	go func() { _, _ = cmd.Process.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		_ = cmd.Process.Kill()
		<-done
	}
}

func pulseWait(t *testing.T, d time.Duration, url string) {
	t.Helper()
	eventually(t, d, url+" ready", func() bool {
		res, err := http.Get(url)
		if err != nil {
			return false
		}
		res.Body.Close()
		return res.StatusCode == 200
	})
}

// pulseStart runs `pulsed init`, nats-server and `pulsed serve` on random loopback ports.
func pulseStart(t *testing.T, dir, src string) *pulseStack {
	t.Helper()
	s := &pulseStack{t: t, dir: dir, bin: filepath.Join(dir, "bin")}
	pulseBuild(t, src, "./cmd/pulsed", filepath.Join(s.bin, "pulsed"))
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	must(t, os.WriteFile(filepath.Join(dir, "pulse-master.key"), []byte(hex.EncodeToString(key)+"\n"), 0o600))
	natsPort, monPort, wsPort, adminPort, metricsPort, publicPort := freePort(t), freePort(t), freePort(t), freePort(t), freePort(t), freePort(t)
	s.natsURL = fmt.Sprintf("tls://127.0.0.1:%d", natsPort)
	s.adminURL = fmt.Sprintf("http://127.0.0.1:%d", adminPort)
	s.envv = pulseEnv(
		"PULSE_PROFILE=dev", "PULSE_LOG_LEVEL=info",
		"PULSE_DATA_DIR="+filepath.Join(dir, "pulsed"), "PULSE_NATS_DIR="+filepath.Join(dir, "nats"),
		"PULSE_MASTER_KEY_FILE="+filepath.Join(dir, "pulse-master.key"),
		fmt.Sprintf("PULSE_NATS_CLIENT_LISTEN=127.0.0.1:%d", natsPort), fmt.Sprintf("PULSE_NATS_MONITOR_LISTEN=127.0.0.1:%d", monPort),
		fmt.Sprintf("PULSE_NATS_WEBSOCKET_LISTEN=127.0.0.1:%d", wsPort),
		"PULSE_NATS_URL="+s.natsURL, "PULSE_NATS_TLS_NAME=localhost", "PULSE_RAW_HOST=localhost",
		fmt.Sprintf("PULSE_ADMIN_ADDR=127.0.0.1:%d", adminPort), "PULSE_ADMIN_URL="+s.adminURL,
		fmt.Sprintf("PULSE_METRICS_ADDR=127.0.0.1:%d", metricsPort), fmt.Sprintf("PULSE_PUBLIC_ADDR=127.0.0.1:%d", publicPort),
		"PULSE_EVENTS_MAX_BYTES=256MiB",
	)
	t.Cleanup(func() {
		for i := len(s.procs) - 1; i >= 0; i-- { // pulsed before nats-server
			pulseStop(s.procs[i])
		}
	})
	initCmd := exec.Command(filepath.Join(s.bin, "pulsed"), "init")
	initCmd.Env = s.envv
	if b, err := initCmd.CombinedOutput(); err != nil {
		t.Fatalf("pulsed init: %v\n%s", err, b)
	}
	natsCmd := pulseStartProc(t, s, "nats.log", pulseEnv(), "nats-server", "-c", filepath.Join(dir, "nats", "nats.conf"))
	_ = natsCmd
	pulseWait(t, 20*time.Second, fmt.Sprintf("http://127.0.0.1:%d/healthz?js-enabled-only=true", monPort))
	pulseStartProc(t, s, "pulsed.log", s.envv, filepath.Join(s.bin, "pulsed"), "serve")
	pulseWait(t, 30*time.Second, fmt.Sprintf("http://127.0.0.1:%d/readyz", metricsPort))

	// An automation token for the admin API (printed once on stdout; kept in memory only).
	tok := exec.Command(filepath.Join(s.bin, "pulsed"), "admin", "token", "--email", "spaceman-real@example.com", "--create",
		"--role", "administrator", "--name", "spaceman-real", "--days", "1")
	tok.Env = s.envv
	out, err := tok.Output()
	if err != nil {
		t.Fatalf("pulsed admin token: %v", err)
	}
	s.pat = strings.TrimSpace(string(out))
	var info struct {
		Version string `json:"version"`
		Connect struct {
			CACert string `json:"ca_cert"`
		} `json:"connect"`
	}
	if st := s.admin("GET", "/system/info", nil, &info); st != 200 || info.Connect.CACert == "" {
		t.Fatalf("system info: %d", st)
	}
	s.caPEM = info.Connect.CACert
	must(t, os.WriteFile(filepath.Join(dir, "pulse-ca.pem"), []byte(s.caPEM), 0o600))
	return s
}

func (s *pulseStack) admin(method, path string, body, out any) int {
	s.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, s.adminURL+"/api/admin/v1"+path, rd)
	req.Header.Set("Authorization", "Bearer "+s.pat)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if out != nil {
		_ = json.Unmarshal(b, out)
	}
	if res.StatusCode >= 300 {
		s.t.Logf("pulsed %s %s: %d %s", method, path, res.StatusCode, b)
	}
	return res.StatusCode
}

type pulseGrant struct {
	Action  string `json:"action"`
	Subject string `json:"subject,omitempty"`
	Stream  string `json:"stream,omitempty"`
	Name    string `json:"name,omitempty"`
}

type pulseClientRec struct {
	ID      string       `json:"id"`
	Grants  []pulseGrant `json:"grants"`
	Version int64        `json:"version"`
}

// client creates a Pulse client with grants and writes its .creds (shown once by pulsed) to the temp dir.
func (s *pulseStack) client(name string, grants ...pulseGrant) (id, credsPath string) {
	s.t.Helper()
	var issued struct {
		Client pulseClientRec `json:"client"`
		Creds  string         `json:"creds"`
	}
	if st := s.admin("POST", "/clients", map[string]any{"name": name, "grants": grants, "expires_in_days": 1}, &issued); st != 200 && st != 201 {
		s.t.Fatalf("create pulse client %s: %d", name, st)
	}
	if issued.Client.ID == "" || !strings.Contains(issued.Creds, "NATS USER JWT") {
		s.t.Fatalf("pulse client %s: no credentials issued", name)
	}
	credsPath = filepath.Join(s.dir, name+".creds")
	must(s.t, os.WriteFile(credsPath, []byte(issued.Creds), 0o600))
	return issued.Client.ID, credsPath
}

// addGrants appends grants to a client (PUT replaces the list; live sessions reconnect with the new permissions).
func (s *pulseStack) addGrants(id string, grants ...pulseGrant) {
	s.t.Helper()
	var c pulseClientRec
	if st := s.admin("GET", "/clients/"+id, nil, &c); st != 200 {
		s.t.Fatalf("get pulse client: %d", st)
	}
	if st := s.admin("PUT", "/clients/"+id+"/grants", map[string]any{"grants": append(c.Grants, grants...), "version": c.Version}, nil); st != 200 {
		s.t.Fatalf("update pulse grants: %d", st)
	}
}

// pulseDurableGrant turns SpaceMan's "durable:<subject>:<name>" hint (Subscription.Grant) into a Pulse grant.
func pulseDurableGrant(t *testing.T, hint string) pulseGrant {
	t.Helper()
	parts := strings.Split(hint, ":")
	if len(parts) != 3 || parts[0] != "durable" {
		t.Fatalf("grant hint %q", hint)
	}
	return pulseGrant{Action: "durable", Subject: parts[1], Name: parts[2]}
}

// pulseFakeGitHubAPI is the GitHub App API stand-in pulse-github needs to resolve webhook installations. The
// installation object is the one pulse-github's own end-to-end test serves (internal/app/app_test.go at v0.1.0).
func pulseFakeGitHubAPI(t *testing.T, dir string) (url, caFile string, calls func() map[string]int) {
	t.Helper()
	const installation = `{"id":555,"app_id":1,"account":{"id":7001,"login":"example-org","type":"Organization"},"repository_selection":"all","permissions":{"issues":"read"},"events":["issues"]}`
	var mu sync.Mutex
	seen := map[string]int{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.Method+" "+r.URL.Path]++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && r.URL.Path == "/app/installations/555":
			_, _ = io.WriteString(w, installation)
		case r.Method == "GET" && r.URL.Path == "/app/installations":
			_, _ = io.WriteString(w, "["+installation+"]")
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"Not Found"}`)
		}
	}))
	t.Cleanup(srv.Close)
	caFile = filepath.Join(dir, "github-api-ca.pem")
	must(t, os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600))
	return srv.URL, caFile, func() map[string]int {
		mu.Lock()
		defer mu.Unlock()
		out := map[string]int{}
		for k, v := range seen {
			out[k] = v
		}
		return out
	}
}

// pulseWebhook POSTs one GitHub webhook to pulse-github, signed like GitHub (X-Hub-Signature-256).
func pulseWebhook(t *testing.T, addr string, secret []byte, event, delivery string, body []byte) int {
	t.Helper()
	m := hmac.New(sha256.New, secret)
	m.Write(body)
	req, _ := http.NewRequest("POST", "http://"+addr+"/webhook", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", delivery)
	req.Header.Set("X-GitHub-Hook-ID", "7")
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(m.Sum(nil)))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	return res.StatusCode
}

func pulseUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// pulseObserver collects events from real pulsed with a plain go-pulse subscriber (ordered ephemeral consumer).
type pulseObserver struct {
	mu     sync.Mutex
	events []pulse.Event
}

func (o *pulseObserver) all() []pulse.Event {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]pulse.Event(nil), o.events...)
}

func pulseObserve(t *testing.T, c *pulse.Client, filter string, since time.Time) *pulseObserver {
	t.Helper()
	o := &pulseObserver{}
	sub, err := c.Subscribe(context.Background(), filter, func(_ context.Context, e pulse.Event) error {
		o.mu.Lock()
		o.events = append(o.events, e)
		o.mu.Unlock()
		return nil
	}, pulse.Since(since))
	if err != nil {
		t.Fatalf("subscribe %s: %v", filter, err)
	}
	t.Cleanup(sub.Stop)
	return o
}

type pulseSub struct {
	ID          string `json:"id"`
	Grant       string `json:"grant"`
	Consumer    string `json:"consumer"`
	Durable     string `json:"durable"`
	State       string `json:"state"`
	LastError   string `json:"last_error"`
	Version     int64  `json:"version"`
	Quarantined int64  `json:"quarantined"`
}

type pulseDelivery struct {
	EventID     string `json:"event_id"`
	Subject     string `json:"subject"`
	Type        string `json:"type"`
	Outcome     string `json:"outcome"`
	Origin      string `json:"origin"`
	InboxItemID string `json:"inbox_item_id"`
	Detail      string `json:"detail"`
}

type pulseItem struct {
	ID            string         `json:"id"`
	SourceKind    string         `json:"source_kind"`
	SourceID      string         `json:"source_id"`
	Title         string         `json:"title"`
	Subscription  string         `json:"subscription_id"`
	CorrelationID string         `json:"correlation_id"`
	DedupKey      string         `json:"dedup_key"`
	Trusted       map[string]any `json:"trusted"`
}

func pulseItems(e *env, spaceID string) []pulseItem {
	var l struct {
		Items []pulseItem `json:"items"`
	}
	e.api("GET", "/spaces/"+spaceID+"/inbox?limit=200", nil, &l)
	return l.Items
}

func pulseDeliveries(e *env, spaceID, subID string) []pulseDelivery {
	var l struct {
		Items []pulseDelivery `json:"items"`
	}
	e.api("GET", "/spaces/"+spaceID+"/subscriptions/"+subID+"/deliveries?limit=500", nil, &l)
	return l.Items
}

func pulseOutcomes(ds []pulseDelivery) map[string]int {
	out := map[string]int{}
	for _, d := range ds {
		out[d.Outcome+"/"+d.Origin]++
	}
	return out
}

// pulseReq is env.api with extra headers (If-Match), returning the status and raw body.
func pulseReq(e *env, method, path string, body any, headers ...string) (int, []byte) {
	e.t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(method, e.url+"/api/v1"+path, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+e.root)
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	rb, _ := io.ReadAll(res.Body)
	return res.StatusCode, rb
}

var pulseOutboxID = regexp.MustCompile(`^evt_[0-9a-z]{26}$`)

func TestRealPulseGitHub(t *testing.T) {
	if os.Getenv("SPACEMAN_REAL_PULSE") != "1" {
		t.Skip("set SPACEMAN_REAL_PULSE=1 (make test-real-pulse) to run against a real pulsed and go-pulse-github")
	}
	pulseSrc, ghSrc := os.Getenv("SPACEMAN_PULSE_SRC"), os.Getenv("SPACEMAN_PULSE_GITHUB_SRC")
	for name, p := range map[string]string{"SPACEMAN_PULSE_SRC (go-pulse checkout)": filepath.Join(pulseSrc, "cmd", "pulsed"),
		"SPACEMAN_PULSE_GITHUB_SRC (go-pulse-github checkout)": filepath.Join(ghSrc, "cmd", "pulse-github")} {
		if fi, err := os.Stat(p); err != nil || !fi.IsDir() || !filepath.IsAbs(p) {
			t.Skipf("%s not found at %q", name, p)
		}
	}
	natsBin, err := exec.LookPath("nats-server")
	if err != nil {
		t.Skip("nats-server not installed (Pulse runs it next to pulsed)")
	}
	natsVer, _ := exec.Command(natsBin, "--version").Output()

	dir, err := os.MkdirTemp("/tmp", "smpulse-")
	must(t, err)
	t.Cleanup(func() {
		if t.Failed() {
			for _, f := range []string{"nats.log", "pulsed.log", "pulse-github.log", "central.log"} {
				b, _ := os.ReadFile(filepath.Join(dir, f))
				t.Logf("---- %s ----\n%s", f, tail(string(b), 3000))
			}
		}
		if os.Getenv("SM_KEEP") == "" {
			_ = os.RemoveAll(dir)
		} else {
			t.Logf("kept %s", dir)
		}
	})
	started := time.Now().Add(-time.Second)

	// ---- Pulse: pulsed + nats-server, clients through the admin API.
	ps := pulseStart(t, dir, pulseSrc)
	t.Logf("pulse %s; %s; go-pulse-github %s", pulseGitDescribe(pulseSrc), strings.TrimSpace(string(natsVer)), pulseGitDescribe(ghSrc))
	smID, smCreds := ps.client("spaceman") // durable grants are added per subscription below, as SpaceMan's UI tells the operator
	_, pubCreds := ps.client("spaceman-publisher", pulseGrant{Action: "publish", Subject: "evt.spaceman.>"})
	_, ghCreds := ps.client("pulse-github", pulseGrant{Action: "publish", Subject: "evt.github.>"})
	_, obsCreds := ps.client("observer", pulseGrant{Action: "subscribe", Subject: "evt.github.>"}, pulseGrant{Action: "subscribe", Subject: "evt.spaceman.>"})

	obsClient, err := pulse.Connect(ps.natsURL, pulse.WithCredsFile(obsCreds), pulse.WithCAFile(filepath.Join(dir, "pulse-ca.pem")), pulse.WithName("spaceman-real-observer"))
	must(t, err)
	t.Cleanup(obsClient.Close)
	ghSeen := pulseObserve(t, obsClient, "evt.github.>", started)
	smSeen := pulseObserve(t, obsClient, "evt.spaceman.>", started)

	// ---- SpaceMan central with outbound publication to the same pulsed.
	e := &env{t: t, dir: dir, bin: filepath.Join(dir, "bin")}
	build(t, e.bin)
	port, mport := freePort(t), freePort(t)
	e.url = fmt.Sprintf("http://127.0.0.1:%d", port)
	central := filepath.Join(dir, "central.yaml")
	must(t, os.WriteFile(central, []byte(fmt.Sprintf(`mode: development
data_dir: %[1]s/central
public_url: %[2]s
kek: {file: %[1]s/kek}
http: {listen: "127.0.0.1:%[3]d", management_listen: "127.0.0.1:%[4]d"}
events:
  outbound: {enabled: true, url: "%[5]s", creds_file: "%[6]s", ca_file: "%[1]s/pulse-ca.pem", source: spaceman-real}
log: {level: debug, format: text}
`, dir, e.url, port, mport, ps.natsURL, pubCreds)), 0o600))
	run(t, e.bin+"/spaceman", "keys", "init-kek", "--out", dir+"/kek")
	for _, f := range strings.Fields(run(t, e.bin+"/spaceman", "-c", central, "admin", "bootstrap")) {
		if strings.HasPrefix(f, "spaceman_rt_") {
			e.root = f
		}
	}
	e.central = start(t, filepath.Join(dir, "central.log"), e.bin+"/spaceman", "-c", central, "serve")
	t.Cleanup(func() { pulseStop(e.central) })
	waitHTTP(t, e.url+"/api/v1/system/info")

	var node struct {
		Node struct{ ID string } `json:"node"`
	}
	if st := e.api("POST", "/nodes", map[string]any{"name": "pulse-node"}, &node); st != 201 {
		t.Fatalf("create node: %d", st)
	}
	spaces := map[string]string{}
	for _, slug := range []string{"pulse-a", "pulse-b"} {
		var sp struct{ ID string }
		if st := e.api("POST", "/spaces", map[string]any{"slug": slug, "name": slug, "node_id": node.Node.ID}, &sp); st != 201 {
			t.Fatalf("create space %s: %d", slug, st)
		}
		spaces[slug] = sp.ID
	}
	smCredsPEM, _ := os.ReadFile(smCreds)
	var conn struct {
		ID       string `json:"id"`
		ClientID string `json:"client_id"`
	}
	if st := e.api("POST", "/event-connections", map[string]any{"name": "real-pulse", "url": ps.natsURL, "creds": string(smCredsPEM),
		"ca_pem": ps.caPEM, "enabled": true}, &conn); st != 201 {
		t.Fatalf("create connection: %d", st)
	}
	if conn.ClientID != smID {
		t.Fatalf("connection client_id %q, pulsed issued %q", conn.ClientID, smID)
	}
	var cat struct {
		Items []struct {
			Slug     string `json:"slug"`
			Versions []struct {
				ID string `json:"id"`
			} `json:"versions"`
		} `json:"items"`
	}
	e.api("GET", "/event-templates", nil, &cat)
	issueTpl := ""
	for _, x := range cat.Items {
		if x.Slug == "github-issue" && len(x.Versions) > 0 {
			issueTpl = x.Versions[0].ID
		}
	}
	if issueTpl == "" {
		t.Fatal("github-issue seed template missing")
	}
	vars := map[string]string{"account_key": "example-org", "repository": "example-org/tool"}
	subs := map[string]pulseSub{}
	createSub := func(key, space string, body map[string]any) {
		var s pulseSub
		st, rb := pulseReq(e, "POST", "/spaces/"+space+"/subscriptions", body)
		if st != 201 {
			t.Fatalf("create subscription %s: %d %s", key, st, rb)
		}
		must(t, json.Unmarshal(rb, &s))
		if s.Consumer != smID+"_"+s.Durable {
			t.Fatalf("subscription %s consumer %q", key, s.Consumer)
		}
		subs[key] = s
	}
	createSub("a-issues", spaces["pulse-a"], map[string]any{"name": "tool issues", "connection_id": conn.ID, "template_version_id": issueTpl, "vars": vars})
	createSub("b-issues", spaces["pulse-b"], map[string]any{"name": "tool issues", "connection_id": conn.ID, "template_version_id": issueTpl, "vars": vars})
	// A custom definition whose input schema rejects push events (they carry no issue): quarantine (SM-EVT-013).
	strict := map[string]any{"variables": []any{}, "filters": []any{}, "subject": "evt.github.example-org.webhook.push.*", "title": "{{data.repository.full_name}} push {{data.payload.after | truncate 12}}",
		"body": "push to {{data.payload.ref}}", "input_schema": json.RawMessage(`{"type":"object","required":["schema_version","payload"],"properties":{"payload":{"type":"object","required":["issue"]}}}`)}
	createSub("b-push", spaces["pulse-b"], map[string]any{"name": "pushes", "connection_id": conn.ID, "custom": strict})
	// SpaceMan's own outbound events, subscribed back: the loop guard must filter them (SM-EVT-014).
	createSub("a-own", spaces["pulse-a"], map[string]any{"name": "own events", "connection_id": conn.ID,
		"custom": map[string]any{"variables": []any{}, "filters": []any{}, "subject": "evt.spaceman.inbox.item.created", "title": "own {{data.item_id}}", "body": "x"}})

	// The operator grants exactly the durables SpaceMan asks for.
	var grants []pulseGrant
	for _, s := range subs {
		grants = append(grants, pulseDurableGrant(t, s.Grant))
	}
	ps.addGrants(smID, grants...)
	spaceOf := map[string]string{"a-issues": spaces["pulse-a"], "b-issues": spaces["pulse-b"], "b-push": spaces["pulse-b"], "a-own": spaces["pulse-a"]}
	getSub := func(key string) pulseSub {
		var s pulseSub
		e.api("GET", "/spaces/"+spaceOf[key]+"/subscriptions/"+subs[key].ID, nil, &s)
		return s
	}
	eventually(t, 90*time.Second, "all subscriptions running on real pulsed", func() bool {
		for k := range subs {
			if s := getSub(k); s.State != "running" || s.LastError != "" {
				return false
			}
		}
		return true
	})
	// The durable consumers exist on the real server under the names SpaceMan announced.
	var consumers []struct {
		Name string `json:"name"`
	}
	if st := ps.admin("GET", "/streams/EVENTS/consumers", nil, &consumers); st != 200 {
		t.Fatalf("pulsed consumers: %d", st)
	}
	names := map[string]bool{}
	for _, c := range consumers {
		names[c.Name] = true
	}
	for k, s := range subs {
		if !names[s.Consumer] {
			t.Errorf("consumer %s for %s not listed by pulsed: %v", s.Consumer, k, names)
		}
	}

	// ---- go-pulse-github: the real service publishes its own fixtures.
	ghURL, ghCA, ghCalls := pulseFakeGitHubAPI(t, dir)
	appKey, err := rsa.GenerateKey(rand.Reader, 2048)
	must(t, err)
	must(t, os.WriteFile(filepath.Join(dir, "github-app.pem"), pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(appKey)}), 0o600))
	hookSecret := make([]byte, 24)
	_, _ = rand.Read(hookSecret)
	hookSecret = []byte(hex.EncodeToString(hookSecret))
	must(t, os.WriteFile(filepath.Join(dir, "github-webhook.secret"), hookSecret, 0o600))
	hookAddr, admAddr := fmt.Sprintf("127.0.0.1:%d", freePort(t)), fmt.Sprintf("127.0.0.1:%d", freePort(t))
	ghCfg := filepath.Join(dir, "pulse-github.yaml")
	must(t, os.WriteFile(ghCfg, []byte(fmt.Sprintf(`version: 1
log: {level: info, format: text}
storage: {path: %[1]s/pulse-github.db, min_free_bytes: 1MiB}
pulse: {url: "%[2]s", creds_file: %[3]s, ca_file: %[1]s/pulse-ca.pem, publish_timeout: 5s}
outbox: {poll_interval: 100ms, backoff_initial: 100ms, backoff_max: 1s}
webhook: {enabled: true, listen: "%[4]s", secrets: [{file: %[1]s/github-webhook.secret}]}
admin: {listen: "%[5]s"}
github:
  api_url: %[6]s
  ca_files: [%[7]s]
  app: {enabled: true, app_id: 1, private_key: {file: %[1]s/github-app.pem}}
accounts:
  - {login: example-org, type: organization, webhook: {enabled: true}}
`, dir, ps.natsURL, ghCreds, hookAddr, admAddr, ghURL, ghCA)), 0o600))
	pulseBuild(t, ghSrc, "./cmd/pulse-github", filepath.Join(ps.bin, "pulse-github"))
	ghProc := pulseStartProc(t, ps, "pulse-github.log", pulseEnv(), filepath.Join(ps.bin, "pulse-github"), "-c", ghCfg, "run")
	t.Cleanup(func() { pulseStop(ghProc) })
	pulseWait(t, 30*time.Second, "http://"+admAddr+"/readyz")

	issueBody, err := os.ReadFile(filepath.Join(ghSrc, "testdata", "webhooks", "issues_opened.json"))
	must(t, err)
	issueBody = bytes.TrimSpace(issueBody)
	pushBody, err := os.ReadFile(filepath.Join(ghSrc, "testdata", "webhooks", "push.json"))
	must(t, err)
	pushBody = bytes.TrimSpace(pushBody)

	issueDelivery := pulseUUID()
	if st := pulseWebhook(t, hookAddr, hookSecret, "issues", issueDelivery, issueBody); st != http.StatusAccepted {
		t.Fatalf("issues webhook: %d", st)
	}
	var ghIssue pulse.Event
	eventually(t, 30*time.Second, "pulse-github published the issue event to pulsed", func() bool {
		for _, ev := range ghSeen.all() {
			if strings.Contains(ev.Subject, ".issues.") {
				ghIssue = ev
				return true
			}
		}
		return false
	})
	// What the real publisher put on the wire, checked against docs/contracts/pulse-github.md.
	var payload map[string]any
	must(t, json.Unmarshal(ghIssue.Data, &payload))
	if ghIssue.Subject != "evt.github.example-org.webhook.issues.opened" || ghIssue.Type != "github.webhook.issues.opened.v1" ||
		ghIssue.Source != "pulse-github" || ghIssue.ContentType != "application/json" || ghIssue.Extra.Get("github-time-basis") == "" ||
		payload["publication_id"] != ghIssue.ID || payload["schema_version"] != float64(1) {
		t.Fatalf("pulse-github envelope: subject=%s type=%s source=%s ct=%s basis=%q pub=%v id=%s schema=%v", ghIssue.Subject, ghIssue.Type,
			ghIssue.Source, ghIssue.ContentType, ghIssue.Extra.Get("github-time-basis"), payload["publication_id"], ghIssue.ID, payload["schema_version"])
	}
	// pulse-github learns installation 555 at start-up (listing) or on the first webhook (lookup); either way through
	// the App API, never from the webhook body alone.
	if c := ghCalls(); c["GET /app/installations/555"]+c["GET /app/installations"] == 0 {
		t.Fatalf("installation never resolved through the App API stand-in: %v", c)
	}
	// SpaceMan's template sample (internal/events/template/testdata, derived from upstream code) against the real
	// payload: every field except the run-specific identifiers and observation time must agree.
	pulseCompareSample(t, payload)

	// ---- Inbound: one item in each space (fan-out), rendered by the seed template, provenance from the event.
	wantTitle := "example-org/tool#42 opened: Bug <script>"
	itemOf := map[string]pulseItem{}
	for _, k := range []string{"a-issues", "b-issues"} {
		k := k
		eventually(t, 30*time.Second, "inbox item in "+k, func() bool {
			for _, it := range pulseItems(e, spaceOf[k]) {
				if it.Subscription == subs[k].ID {
					itemOf[k] = it
					return true
				}
			}
			return false
		})
		it := itemOf[k]
		if it.Title != wantTitle || it.SourceKind != "event" || it.CorrelationID != "github:example-org/tool#42" {
			t.Fatalf("%s item: %+v", k, it)
		}
		ds := pulseDeliveries(e, spaceOf[k], subs[k].ID)
		if len(ds) != 1 || ds[0].EventID != ghIssue.ID || ds[0].Outcome != "accepted" || ds[0].Origin != "live" || ds[0].InboxItemID != it.ID {
			t.Fatalf("%s deliveries: %+v", k, ds)
		}
	}
	if itemOf["a-issues"].ID == itemOf["b-issues"].ID {
		t.Fatal("both spaces share one item")
	}

	// ---- Dedup (SM-EVT-012). 1) GitHub redelivers the same webhook: pulse-github accepts it and publishes nothing new.
	if st := pulseWebhook(t, hookAddr, hookSecret, "issues", issueDelivery, issueBody); st/100 != 2 {
		t.Fatalf("redelivered webhook: %d", st)
	}
	// 2) A publisher retry with the same Nats-Msg-Id (pulse-github's at-least-once outbox) inside JetStream's window.
	ghClient, err := pulse.Connect(ps.natsURL, pulse.WithCredsFile(ghCreds), pulse.WithCAFile(filepath.Join(dir, "pulse-ca.pem")), pulse.WithSource("pulse-github"))
	must(t, err)
	defer ghClient.Close()
	opts := []pulse.PublishOption{pulse.WithID(ghIssue.ID), pulse.WithTime(ghIssue.Time), pulse.WithContentType(ghIssue.ContentType)}
	for k := range ghIssue.Extra {
		opts = append(opts, pulse.WithHeader(k, ghIssue.Extra.Get(k)))
	}
	ack, err := ghClient.Publish(context.Background(), ghIssue.Subject, ghIssue.Type, json.RawMessage(ghIssue.Data), opts...)
	must(t, err)
	if !ack.Duplicate {
		t.Fatalf("same Nats-Msg-Id inside the window was stored again: %+v", ack)
	}
	// 3) Past JetStream's window only SpaceMan's own dedup holds: an explicit replay redelivers the stored event to
	// space A, which records a duplicate and creates nothing.
	cur := getSub("a-issues")
	st, rb := pulseReq(e, "POST", "/spaces/"+spaceOf["a-issues"]+"/subscriptions/"+subs["a-issues"].ID+"/replay",
		map[string]any{"policy": "all", "reason": "real pulse dedup check"}, "If-Match", fmt.Sprintf(`"%d"`, cur.Version))
	if st != 202 {
		t.Fatalf("replay: %d %s", st, rb)
	}
	eventually(t, 60*time.Second, "replayed delivery recorded as a duplicate", func() bool {
		return pulseOutcomes(pulseDeliveries(e, spaceOf["a-issues"], subs["a-issues"].ID))["duplicate/replay"] >= 1
	})
	time.Sleep(2 * time.Second)
	for _, k := range []string{"a-issues", "b-issues"} {
		n := 0
		for _, it := range pulseItems(e, spaceOf[k]) {
			if it.Subscription == subs[k].ID {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("%s has %d items after redelivery, retry and replay", k, n)
		}
	}
	if n := len(pulseSubjects(ghSeen, ".issues.")); n != 1 {
		t.Fatalf("pulsed stored %d issue events, want 1", n)
	}

	// ---- Quarantine (SM-EVT-013): a real push publication fails the custom input schema.
	if st := pulseWebhook(t, hookAddr, hookSecret, "push", pulseUUID(), pushBody); st != http.StatusAccepted {
		t.Fatalf("push webhook: %d", st)
	}
	var quar struct {
		Items []struct {
			ID      string `json:"id"`
			EventID string `json:"event_id"`
			Status  string `json:"status"`
		} `json:"items"`
	}
	bPush := spaceOf["b-push"]
	eventually(t, 30*time.Second, "push event quarantined", func() bool {
		e.api("GET", "/spaces/"+bPush+"/subscriptions/"+subs["b-push"].ID+"/quarantine?status=open", nil, &quar)
		return len(quar.Items) == 1
	})
	pushSubjects := pulseSubjects(ghSeen, ".push.")
	if len(pushSubjects) != 1 || pushSubjects[0] != "evt.github.example-org.webhook.push.none" {
		t.Fatalf("push subjects on pulsed: %v", pushSubjects)
	}
	time.Sleep(2 * time.Second) // a nak would come back within a second or two
	if s := getSub("b-push"); s.State != "running" || s.LastError != "" || s.Quarantined != 1 {
		t.Fatalf("quarantine is not a subscription error: %+v", s)
	}
	if o := pulseOutcomes(pulseDeliveries(e, bPush, subs["b-push"].ID)); o["quarantined/live"] != 1 || len(o) != 1 {
		t.Fatalf("push deliveries: %v", o)
	}
	// Fix the schema, reprocess: the quarantined event becomes an item.
	cur = getSub("b-push")
	fixed := map[string]any{}
	for k, v := range strict {
		fixed[k] = v
	}
	fixed["input_schema"] = json.RawMessage(`{"type":"object","required":["schema_version","payload"]}`)
	if st, rb := pulseReq(e, "PUT", "/spaces/"+bPush+"/subscriptions/"+subs["b-push"].ID, map[string]any{"enabled": true, "custom": fixed},
		"If-Match", fmt.Sprintf(`"%d"`, cur.Version)); st != 200 {
		t.Fatalf("fix schema: %d %s", st, rb)
	}
	if st, rb := pulseReq(e, "POST", "/spaces/"+bPush+"/subscriptions/"+subs["b-push"].ID+"/quarantine/"+quar.Items[0].ID, map[string]any{"action": "reprocess"}); st != 200 {
		t.Fatalf("reprocess: %d %s", st, rb)
	}
	eventually(t, 15*time.Second, "reprocessed push item", func() bool {
		for _, it := range pulseItems(e, bPush) {
			if it.Subscription == subs["b-push"].ID && strings.HasPrefix(it.Title, "example-org/tool push ") {
				return true
			}
		}
		return false
	})

	// ---- Outbound (SM-OBS-004): SpaceMan's inbox.item.created events on the real pulsed, read back with go-pulse.
	wantItems := map[string]string{itemOf["a-issues"].ID: spaces["pulse-a"], itemOf["b-issues"].ID: spaces["pulse-b"]}
	var outbound []pulse.Event
	eventually(t, 30*time.Second, "outbound inbox.item.created for both issue items", func() bool {
		outbound = outbound[:0]
		found := 0
		for _, ev := range smSeen.all() {
			if ev.Subject != "evt.spaceman.inbox.item.created" {
				continue
			}
			outbound = append(outbound, ev)
			var p map[string]any
			_ = json.Unmarshal(ev.Data, &p)
			if sp, ok := wantItems[fmt.Sprint(p["item_id"])]; ok && p["space_id"] == sp {
				found++
			}
		}
		return found == 2
	})
	ids := map[string]bool{}
	for _, ev := range outbound {
		var p map[string]any
		_ = json.Unmarshal(ev.Data, &p)
		if ev.Type != "spaceman.inbox.item_created.v1" || !pulseOutboxID.MatchString(ev.ID) || ev.Source != "spaceman-real" ||
			ev.ContentType != "application/json" || ev.Extra.Get("spaceman-origin") != "spaceman-real" || ids[ev.ID] {
			t.Fatalf("outbound envelope: subject=%s type=%s id=%s source=%s ct=%s origin=%q", ev.Subject, ev.Type, ev.ID, ev.Source, ev.ContentType, ev.Extra.Get("spaceman-origin"))
		}
		ids[ev.ID] = true
		if _, ok := wantItems[fmt.Sprint(p["item_id"])]; ok {
			if ev.Extra.Get("spaceman-causation-id") != ghIssue.ID || p["source_kind"] != "event" {
				t.Fatalf("outbound causation %q (want the pulse-github publication %s), payload %v", ev.Extra.Get("spaceman-causation-id"), ghIssue.ID, p)
			}
		}
	}

	// ---- Loop guard (SM-EVT-014): SpaceMan's own events come back on its subscription and are filtered, not items.
	eventually(t, 30*time.Second, "own outbound events filtered", func() bool {
		return pulseOutcomes(pulseDeliveries(e, spaceOf["a-own"], subs["a-own"].ID))["filtered/live"] >= len(outbound)
	})
	for _, it := range pulseItems(e, spaceOf["a-own"]) {
		if it.Subscription == subs["a-own"].ID {
			t.Fatalf("own event became an item: %+v", it)
		}
	}
	t.Logf("real pulsed: github subjects %v; %d spaceman events; subscriptions %d; consumers %d", pulseSubjects(ghSeen, "."), len(smSeen.all()), len(subs), len(consumers))
}

func pulseSubjects(o *pulseObserver, contains string) []string {
	var out []string
	for _, ev := range o.all() {
		if strings.Contains(ev.Subject, contains) {
			out = append(out, ev.Subject)
		}
	}
	return out
}

// pulseCompareSample reports where SpaceMan's derived sample payload differs from the real one.
func pulseCompareSample(t *testing.T, real map[string]any) {
	t.Helper()
	b, err := os.ReadFile("../../internal/events/template/testdata/pulse-github-issues-opened.json")
	must(t, err)
	var sample map[string]any
	must(t, json.Unmarshal(b, &sample))
	var diffs []string
	keys := map[string]bool{}
	for k := range sample {
		keys[k] = true
	}
	for k := range real {
		keys[k] = true
	}
	for k := range keys {
		switch k {
		case "observation_id", "publication_id", "observed_at": // run-specific (hashes and the observation time)
			continue
		}
		sv, sok := sample[k]
		rv, rok := real[k]
		if k == "github" || k == "source" { // delivery and hook IDs are per delivery
			sm, _ := sv.(map[string]any)
			rm, _ := rv.(map[string]any)
			sv, rv = pulseWithout(sm, "delivery_id", "hook_id"), pulseWithout(rm, "delivery_id", "hook_id")
		}
		if sok != rok || !reflect.DeepEqual(sv, rv) {
			diffs = append(diffs, k)
		}
	}
	sort.Strings(diffs)
	for _, k := range diffs {
		sb, _ := json.Marshal(sample[k])
		rb, _ := json.Marshal(real[k])
		t.Logf("finding: SpaceMan's derived sample differs from the real publication in %q:\n  sample %s\n  real   %s", k, tail(string(sb), 400), tail(string(rb), 400))
	}
	// The paths the seed templates read (internal/events/template/github.go: filters, title, body, correlation) must
	// be identical; other differences are reported above as findings, not failures.
	for _, p := range []string{"schema_version", "repository.full_name", "resource.number", "resource.html_url", "github.action",
		"actor.login", "payload.issue.title", "payload.issue.body"} {
		if sv, rv := pulseAt(sample, p), pulseAt(real, p); sv == nil || !reflect.DeepEqual(sv, rv) {
			t.Errorf("template path %s: sample %v, real %v", p, sv, rv)
		}
	}
}

func pulseAt(m map[string]any, path string) any {
	var cur any = m
	for _, k := range strings.Split(path, ".") {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[k]
	}
	return cur
}

func pulseWithout(m map[string]any, keys ...string) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		out[k] = v
	}
	for _, k := range keys {
		delete(out, k)
	}
	return out
}
