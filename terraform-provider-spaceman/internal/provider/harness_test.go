package provider

// The acceptance-test harness (TF_ACC=1): it builds `spaceman` and `devpulse` from the parent module, starts a throwaway
// central on a random loopback port with its own data directory and KEK, bootstraps it, and registers two nodes. It
// stops only the processes it started and removes only the directory it created (SM_TF_KEEP=1 keeps the directory).
// The root token is generated at run time and never printed.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"

	"github.com/aurimasniekis/go-spaceman/terraform-provider-spaceman/internal/client"
)

// accEnv describes the running dev server.
type accEnv struct {
	dir        string
	endpoint   string
	token      string
	nodeA      *client.Node
	nodeB      *client.Node
	creds      string // Pulse client credentials (client "spaceman")
	creds2     string // other Pulse client credentials (client "pulse-github")
	pulseURL   string
	procs      []*exec.Cmd
	client     *client.Client
	centralLog string
}

var acc *accEnv

var testAccProtoV6ProviderFactories = map[string]func() (tfprotov6.ProviderServer, error){
	"spaceman": providerserver.NewProtocol6WithError(New("test")()),
}

func TestMain(m *testing.M) {
	if os.Getenv("TF_ACC") == "" {
		os.Exit(m.Run())
	}
	env, err := startDevServer()
	if err != nil {
		fmt.Fprintln(os.Stderr, "acceptance harness:", err)
		if env != nil {
			env.stop()
		}
		os.Exit(1)
	}
	acc = env
	code := m.Run()
	if code != 0 {
		fmt.Fprintln(os.Stderr, "acceptance harness: central log kept at", env.centralLog)
		_ = os.Setenv("SM_TF_KEEP", "1")
	}
	env.stop()
	os.Exit(code)
}

func testAccPreCheck(t *testing.T) {
	t.Helper()
	if os.Getenv("TF_ACC") == "" {
		t.Skip("acceptance test: set TF_ACC=1 (starts a local SpaceMan dev server)")
	}
	if acc == nil {
		t.Fatal("the acceptance harness did not start (TF_ACC must be set before the test binary starts)")
	}
}

// repoRoot finds the parent SpaceMan module.
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		b, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err == nil && bytes.HasPrefix(b, []byte("module github.com/aurimasniekis/go-spaceman\n")) {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("the SpaceMan module (github.com/aurimasniekis/go-spaceman) was not found above the provider")
		}
		dir = parent
	}
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// cleanEnv is the process environment without any SpaceMan or herdr-core configuration of the caller.
func cleanEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		k := kv[:strings.IndexByte(kv+"=", '=')]
		if strings.HasPrefix(k, "SPACEMAN_") || strings.HasPrefix(k, "HC_") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func waitFor(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return cond()
}

var reRootToken = regexp.MustCompile(`spaceman_rt_[A-Za-z0-9_-]+`)

func startDevServer() (*accEnv, error) {
	root, err := repoRoot()
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "smtf-")
	if err != nil {
		return nil, err
	}
	env := &accEnv{dir: dir, centralLog: filepath.Join(dir, "central.log")}
	bin := filepath.Join(dir, "bin")
	build := exec.Command("go", "build", "-o", bin+string(os.PathSeparator), "./cmd/spaceman", "./internal/events/pulsetest/devpulse")
	build.Dir = root
	build.Env = cleanEnv()
	if out, err := build.CombinedOutput(); err != nil {
		return env, fmt.Errorf("go build: %w\n%s", err, out)
	}
	// devpulse: a Pulse-shaped NATS server that writes client credentials (used by the connection tests).
	pport, err := freePort()
	if err != nil {
		return env, err
	}
	pdir := filepath.Join(dir, "pulse")
	if err := os.MkdirAll(pdir, 0o700); err != nil {
		return env, err
	}
	if err := env.start(filepath.Join(dir, "devpulse.log"), filepath.Join(bin, "devpulse"), "serve", "-dir", pdir, "-port", fmt.Sprint(pport)); err != nil {
		return env, err
	}
	ok := waitFor(30*time.Second, func() bool {
		for _, f := range []string{"url", "spaceman.creds", "publisher.creds"} {
			if st, err := os.Stat(filepath.Join(pdir, f)); err != nil || st.Size() == 0 {
				return false
			}
		}
		return true
	})
	if !ok {
		return env, errors.New("devpulse did not write its credentials")
	}
	read := func(f string) string { b, _ := os.ReadFile(filepath.Join(pdir, f)); return string(b) }
	env.creds, env.creds2, env.pulseURL = read("spaceman.creds"), read("publisher.creds"), strings.TrimSpace(read("url"))

	port, err := freePort()
	if err != nil {
		return env, err
	}
	mport, err := freePort()
	if err != nil {
		return env, err
	}
	env.endpoint = fmt.Sprintf("http://127.0.0.1:%d", port)
	cfg := filepath.Join(dir, "central.yaml")
	yaml := fmt.Sprintf(`mode: development
data_dir: %s
public_url: %s
kek: {file: %s}
http: {listen: "127.0.0.1:%d", management_listen: "127.0.0.1:%d"}
log: {level: info, format: text}
`, filepath.Join(dir, "central"), env.endpoint, filepath.Join(dir, "kek"), port, mport)
	if err := os.WriteFile(cfg, []byte(yaml), 0o600); err != nil {
		return env, err
	}
	sm := filepath.Join(bin, "spaceman")
	if out, err := runCmd(sm, "keys", "init-kek", "--out", filepath.Join(dir, "kek")); err != nil {
		return env, fmt.Errorf("init-kek: %w\n%s", err, out)
	}
	out, err := runCmd(sm, "-c", cfg, "admin", "bootstrap")
	if err != nil {
		return env, fmt.Errorf("bootstrap failed: %w", err) // the output holds the root token: never print it
	}
	env.token = reRootToken.FindString(string(out))
	if env.token == "" {
		return env, errors.New("bootstrap printed no root token")
	}
	if err := env.start(env.centralLog, sm, "-c", cfg, "serve"); err != nil {
		return env, err
	}
	if !waitFor(60*time.Second, func() bool {
		r, err := http.Get(env.endpoint + "/api/v1/system/info") //nolint:noctx // readiness probe of the local test server
		if err != nil {
			return false
		}
		_ = r.Body.Close()
		return r.StatusCode == http.StatusOK
	}) {
		return env, fmt.Errorf("central did not become ready (log: %s)", env.centralLog)
	}
	// The provider under test reads these (in-process).
	_ = os.Setenv(EnvEndpoint, env.endpoint)
	_ = os.Setenv(EnvToken, env.token)
	if env.client, err = client.New(env.endpoint, env.token, client.Options{}); err != nil {
		return env, err
	}
	ctx := context.Background()
	for i, n := range []**client.Node{&env.nodeA, &env.nodeB} {
		e, err := env.client.CreateNode(ctx, fmt.Sprintf("tf-node-%c", 'a'+i))
		if err != nil {
			return env, fmt.Errorf("create node: %w", err)
		}
		*n = e.Node
	}
	return env, nil
}

func runCmd(name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	cmd.Env = cleanEnv()
	return cmd.CombinedOutput()
}

// start runs a long-lived process with its output in logPath.
func (e *accEnv) start(logPath, name string, args ...string) error {
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	cmd := exec.Command(name, args...)
	cmd.Env = cleanEnv()
	cmd.Stdout, cmd.Stderr = f, f
	if err := cmd.Start(); err != nil {
		_ = f.Close()
		return err
	}
	e.procs = append(e.procs, cmd)
	return nil
}

// stop terminates the processes this harness started and removes its own directory.
func (e *accEnv) stop() {
	for i := len(e.procs) - 1; i >= 0; i-- {
		p := e.procs[i]
		_ = p.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _ = p.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			_ = p.Process.Kill()
			<-done
		}
	}
	if os.Getenv("SM_TF_KEEP") == "1" {
		fmt.Fprintln(os.Stderr, "acceptance harness: kept", e.dir)
		return
	}
	if e.dir != "" && strings.HasPrefix(filepath.Base(e.dir), "smtf-") {
		_ = os.RemoveAll(e.dir)
	}
}
