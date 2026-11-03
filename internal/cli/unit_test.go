package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// fake is a scripted API: handlers by "METHOD /path" (relative to /api/v1), recording every request.
type fake struct {
	t      *testing.T
	srv    *httptest.Server
	mu     sync.Mutex
	routes map[string]func(w http.ResponseWriter, r *http.Request, body map[string]any)
	reqs   []recorded
}

type recorded struct {
	method, path, query string
	header              http.Header
	body                map[string]any
}

func newFake(t *testing.T) *fake {
	f := &fake{t: t, routes: map[string]func(http.ResponseWriter, *http.Request, map[string]any){}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(b, &body)
		p := strings.TrimPrefix(r.URL.Path, APIPrefix)
		f.mu.Lock()
		f.reqs = append(f.reqs, recorded{method: r.Method, path: p, query: r.URL.RawQuery, header: r.Header.Clone(), body: body})
		h := f.routes[r.Method+" "+p]
		f.mu.Unlock()
		if h == nil {
			problem(w, 404, "not_found", "no route "+r.Method+" "+p)
			return
		}
		h(w, r, body)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fake) on(route string, h func(w http.ResponseWriter, r *http.Request, body map[string]any)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes[route] = h
}

// json answers a route with a fixed body (and an ETag when version is set).
func (f *fake) json(route string, status int, v any) {
	f.on(route, func(w http.ResponseWriter, _ *http.Request, _ map[string]any) { reply(w, status, v) })
}

func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	if m, ok := v.(map[string]any); ok {
		if ver, ok := m["version"]; ok {
			w.Header().Set("ETag", `"`+numberString(ver)+`"`)
		}
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func problem(w http.ResponseWriter, status int, code, detail string, fields ...FieldError) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("X-Request-Id", "req_test")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"type": "https://example/errors#" + code, "title": http.StatusText(status), "status": status,
		"code": code, "detail": detail, "errors": fields})
}

func (f *fake) last(method, path string) recorded {
	f.t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.reqs) - 1; i >= 0; i-- {
		if f.reqs[i].method == method && f.reqs[i].path == path {
			return f.reqs[i]
		}
	}
	f.t.Fatalf("no %s %s among %d requests", method, path, len(f.reqs))
	return recorded{}
}

func (f *fake) count(method, path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.reqs {
		if r.method == method && r.path == path {
			n++
		}
	}
	return n
}

func (f *fake) env() env { return env{server: f.srv.URL, token: "spaceman_at_test"} }

const spaceID = "spc_0123456789abcdefghijklmnop"
const nodeID = "nod_0123456789abcdefghijklmnop"

func walk(c *cobra.Command, fn func(*cobra.Command)) {
	fn(c)
	for _, ch := range c.Commands() {
		walk(ch, fn)
	}
}

// Every remote command calls only operations of the build's API document (the one `spaceman openapi` prints and
// make api commits), offers --idempotency-key exactly where the operation accepts an Idempotency-Key, and offers
// --if-match only where the operation takes If-Match.
func TestCommandsMatchTheAPIDocument(t *testing.T) {
	live, err := specOnce()
	if err != nil {
		t.Fatal(err)
	}
	spec, err := ParseSpec(live)
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.IDs()) < 150 {
		t.Fatalf("only %d operations in the document", len(spec.IDs()))
	}
	root := newRootFor(&App{SpecSource: specOnce})
	used := map[string]bool{}
	walk(root, func(c *cobra.Command) {
		ops := splitComma(c.Annotations[annOps])
		for _, id := range ops {
			op, err := spec.Op(id)
			if err != nil {
				t.Errorf("%s: %v", c.CommandPath(), err)
				continue
			}
			used[id] = true
			if c.Flags().Lookup("idempotency-key") != nil && op.Method == http.MethodPost && !op.Headers["Idempotency-Key"] {
				t.Errorf("%s offers --idempotency-key but %s takes none", c.CommandPath(), id)
			}
		}
		if len(ops) == 0 {
			return
		}
		main, _ := spec.Op(ops[len(ops)-1])
		if len(ops) >= 2 {
			main, _ = spec.Op(ops[1])
		}
		if main != nil && main.Method == http.MethodPost && main.Headers["Idempotency-Key"] && c.Flags().Lookup("idempotency-key") == nil {
			t.Errorf("%s calls %s, which accepts Idempotency-Key, without --idempotency-key", c.CommandPath(), main.ID)
		}
		if c.Flags().Lookup("if-match") != nil && main != nil && !main.Headers["If-Match"] {
			t.Errorf("%s offers --if-match but %s takes no If-Match", c.CommandPath(), main.ID)
		}
	})
	if len(used) < 120 {
		t.Fatalf("only %d operations are reachable from the CLI", len(used))
	}
}

// Secrets are never flag values: no flag carries a token, password, account secret, credentials or headers itself.
func TestNoSecretFlagValues(t *testing.T) {
	root := newRootFor(&App{SpecSource: specOnce})
	banned := map[string]bool{"token": true, "secret": true, "password": true, "creds": true, "headers": true, "client-secret": true, "api-key": true}
	walk(root, func(c *cobra.Command) {
		check := func(f *pflag.Flag) {
			if banned[f.Name] && f.Value.Type() != "bool" {
				t.Errorf("%s has a --%s flag value", c.CommandPath(), f.Name)
			}
		}
		c.LocalFlags().VisitAll(check)
		c.PersistentFlags().VisitAll(check)
	})
}

func TestClientRules(t *testing.T) {
	for _, tc := range []struct {
		server   string
		insecure bool
		ok       bool
	}{
		{"http://127.0.0.1:8080", false, true},
		{"http://localhost:8080/api/v1", false, true},
		{"http://[::1]:8080", false, true},
		{"https://spaceman.example.com", false, true},
		{"http://spaceman.example.com", false, false},
		{"http://spaceman.example.com", true, true},
		{"ftp://spaceman.example.com", false, false},
		{"https://user:pw@spaceman.example.com", false, false},
		{"https://spaceman.example.com/?x=1", false, false},
		{"spaceman.example.com", false, false},
		{"", false, false},
	} {
		_, err := NewClient(tc.server, "tok", ClientOptions{AllowInsecureHTTP: tc.insecure})
		if (err == nil) != tc.ok {
			t.Errorf("%q insecure=%v: %v", tc.server, tc.insecure, err)
		}
	}
	cl, _ := NewClient("https://x.example.com/prefix/api/v1/", "tok", ClientOptions{})
	if cl.BaseURL() != "https://x.example.com/prefix/api/v1" {
		t.Fatalf("base %s", cl.BaseURL())
	}
	if _, err := NewClient("https://x.example.com", "", ClientOptions{}); ExitCode(err) != ExitAuth {
		t.Fatalf("no token: %v", err)
	}
	if _, err := NewClient("https://x.example.com", "a\nb", ClientOptions{}); ExitCode(err) != ExitAuth {
		t.Fatalf("token with newline: %v", err)
	}
	// The plain-http rule applies to the global flag too, and --allow-insecure-http lifts it.
	e := env{server: "http://spaceman.example.com", token: "tok"}
	if r := e.code(t, ExitUsage, "space", "list"); !strings.Contains(r.err, "plain http") {
		t.Fatalf("plain http: %s", r.err)
	}
}

// Redirects are never followed, so the bearer token never leaves the configured origin.
func TestRedirectsAreNotFollowed(t *testing.T) {
	var leaked bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked = leaked || r.Header.Get("Authorization") != ""
		reply(w, 200, map[string]any{"items": []any{}})
	}))
	defer other.Close()
	f := newFake(t)
	f.on("GET /spaces", func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		http.Redirect(w, r, other.URL+"/api/v1/spaces", http.StatusFound)
	})
	r := f.env().code(t, ExitAPI, "space", "list")
	if leaked || !strings.Contains(r.err, "redirect") {
		t.Fatalf("leaked=%v err=%s", leaked, r.err)
	}
}

func TestRetriesAndProblems(t *testing.T) {
	f := newFake(t)
	n := 0
	f.on("GET /nodes", func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
		n++
		if n < 3 {
			problem(w, 503, "unavailable", "starting")
			return
		}
		reply(w, 200, map[string]any{"items": []any{map[string]any{"id": nodeID, "name": "n1", "state": "active", "connected": true}}})
	})
	e := f.env()
	if out := e.ok(t, "node", "list"); !strings.Contains(out, "n1") || n != 3 {
		t.Fatalf("retry: n=%d %s", n, out)
	}
	// A create without an idempotency key is never retried; with one (spaces) it is.
	f.on("POST /approval-policies", func(w http.ResponseWriter, _ *http.Request, _ map[string]any) { problem(w, 503, "unavailable", "busy") })
	e.code(t, ExitAPI, "approval-policy", "create", "spaceman.x")
	if c := f.count("POST", "/approval-policies"); c != 1 {
		t.Fatalf("non-idempotent create retried %d times", c)
	}
	f.json("GET /nodes/"+nodeID, 200, map[string]any{"id": nodeID, "name": "n1"})
	k := 0
	f.on("POST /spaces", func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		k++
		if k == 1 {
			problem(w, 502, "bad_gateway", "proxy")
			return
		}
		reply(w, 201, map[string]any{"id": spaceID, "slug": body["slug"], "version": 1})
	})
	e.ok(t, "space", "create", "pay", "--node", nodeID)
	reqs := []recorded{}
	for _, r := range f.reqs {
		if r.method == "POST" && r.path == "/spaces" {
			reqs = append(reqs, r)
		}
	}
	if len(reqs) != 2 || reqs[0].header.Get("Idempotency-Key") == "" || reqs[0].header.Get("Idempotency-Key") != reqs[1].header.Get("Idempotency-Key") {
		t.Fatalf("idempotent retry %v", reqs)
	}
	if reqs[1].body["name"] != "pay" || reqs[1].body["node_id"] != nodeID {
		t.Fatalf("create body %v", reqs[1].body)
	}

	// Problems map to exit codes and are printed with their fields, operation and request ID.
	for status, want := range map[int]int{401: ExitAuth, 403: ExitAuth, 404: ExitNotFound, 409: ExitConflict, 412: ExitConflict, 428: ExitConflict,
		422: ExitAPI, 500: ExitAPI} {
		st := status
		f.on("GET /accounts", func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
			problem(w, st, "some_code", "it failed", FieldError{Field: "body.name", Message: "is required"}, FieldError{Message: "general"})
		})
		r := e.code(t, want, "account", "list")
		for _, s := range []string{"it failed", "some_code", "body.name: is required", "general", "list-accounts (GET /accounts)", "req_test"} {
			if !strings.Contains(r.err, s) {
				t.Errorf("%d: %q missing in %s", st, s, r.err)
			}
		}
	}
	// A non-problem error body still gives the status.
	f.on("GET /skills", func(w http.ResponseWriter, _ *http.Request, _ map[string]any) { w.WriteHeader(http.StatusTeapot) })
	if r := e.code(t, ExitAPI, "skill", "list"); !strings.Contains(r.err, "418") {
		t.Fatalf("bare status %s", r.err)
	}
	// Transport failures exit 2.
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	if r := (env{server: dead.URL, token: "t"}).code(t, ExitAPI, "space", "list"); !strings.Contains(r.err, "GET /spaces") {
		t.Fatalf("transport %s", r.err)
	}
}

func TestExitCodesAndErrors(t *testing.T) {
	for err, want := range map[error]int{
		nil:                                  ExitOK,
		errors.New("x"):                      ExitUsage,
		&UsageError{Msg: "u"}:                ExitUsage,
		&AuthError{Msg: "a"}:                 ExitAuth,
		&NetError{Op: "GET /x", Err: io.EOF}: ExitAPI,
		&StateError{Msg: "s"}:                ExitAPI,
		&APIError{Status: 404}:               ExitNotFound,
	} {
		if got := ExitCode(err); got != want {
			t.Errorf("%v: %d want %d", err, got, want)
		}
	}
	ae := &APIError{Status: 409, Code: "conflict", Detail: "the space changed", Errors: []FieldError{{Field: "f", Message: "m"}, {Message: "g"}}}
	if ae.Error() != "409 conflict: the space changed; f: m; g" {
		t.Fatalf("error %q", ae.Error())
	}
	if (&APIError{Status: 500, Title: "Internal"}).Error() != "500: Internal" {
		t.Fatal("title fallback")
	}
	if !strings.Contains((&NetError{Op: "GET /x", Err: io.EOF}).Error(), "EOF") || !errors.Is(&NetError{Err: io.EOF}, io.EOF) {
		t.Fatal("net error")
	}
	var b bytes.Buffer
	PrintError(&b, &APIError{Status: 428, Method: "PUT", Path: "/spaces/x"})
	if !strings.Contains(b.String(), "precondition_required") || !strings.Contains(b.String(), "--if-match") || !strings.Contains(b.String(), "request: PUT /spaces/x") {
		t.Fatalf("print %s", b.String())
	}
	b.Reset()
	PrintError(&b, &APIError{Status: 409, Detail: "the space changed since version 3"})
	if !strings.Contains(b.String(), "read it again") {
		t.Fatalf("hint %s", b.String())
	}
	b.Reset()
	PrintError(&b, &APIError{Status: 401})
	if !strings.Contains(b.String(), "expired or revoked") {
		t.Fatalf("hint %s", b.String())
	}
	b.Reset()
	PrintError(&b, &StateError{Msg: "boom"})
	if b.String() != "spaceman: boom\n" {
		t.Fatalf("plain %q", b.String())
	}
}

func TestTokenSources(t *testing.T) {
	f := newFake(t)
	f.on("GET /auth/me", func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		reply(w, 200, map[string]any{"method": "token", "auth": r.Header.Get("Authorization")})
	})
	file := writeFile(t, "tok", "spaceman_at_fromfile\n", 0o600)
	e := env{server: f.srv.URL, token: "spaceman_at_fromenv"}
	if m := e.obj(t, "whoami", "--token-file", file); m["auth"] != "Bearer spaceman_at_fromfile" {
		t.Fatalf("token file wins: %v", m)
	}
	e.extra = map[string]string{EnvTokenFile: file}
	if m := e.obj(t, "whoami"); m["auth"] != "Bearer spaceman_at_fromfile" {
		t.Fatalf("token file env: %v", m)
	}
	e.extra = nil
	if m := e.obj(t, "whoami"); m["auth"] != "Bearer spaceman_at_fromenv" {
		t.Fatalf("env token: %v", m)
	}
	loose := writeFile(t, "loose", "spaceman_at_x\n", 0o644)
	if r := e.code(t, ExitAuth, "whoami", "--token-file", loose); !strings.Contains(r.err, "chmod 600") {
		t.Fatalf("loose file %s", r.err)
	}
	if r := e.code(t, ExitAuth, "whoami", "--token-file", filepath.Join(t.TempDir(), "missing")); !strings.Contains(r.err, "token file") {
		t.Fatalf("missing file %s", r.err)
	}
	stdin := env{server: f.srv.URL, stdin: "spaceman_at_stdin\n"}
	if m := stdin.obj(t, "whoami", "--token-file", "-"); m["auth"] != "Bearer spaceman_at_stdin" {
		t.Fatalf("stdin token: %v", m)
	}
	if r := e.code(t, ExitUsage, "whoami", "-o", "xml"); !strings.Contains(r.err, "table, json or yaml") {
		t.Fatalf("bad output %s", r.err)
	}
	e.extra = map[string]string{EnvOutput: "yaml"}
	if out := e.ok(t, "whoami"); !strings.Contains(out, "method: token") {
		t.Fatalf("env output %s", out)
	}
	ins := env{server: "http://" + strings.TrimPrefix(f.srv.URL, "http://"), token: "t", extra: map[string]string{EnvInsecure: "1"}}
	ins.ok(t, "whoami")
}

func TestOutputFormats(t *testing.T) {
	body := []byte(`{"items":[{"id":"a","name":"x|y","created_at":"2026-11-03T10:00:00.123Z","tags":["p","q"],"meta":{"k":"v"},"n":3,"ok":true,"long":"` +
		strings.Repeat("z", 150) + `","nested":[{"a":1}],"multi":"l1\nl2"}],"next_cursor":""}`)
	v := view{items: "items", cols: []col{c("ID", "id"), c("NAME", "name"), c("AT", "created_at"), c("TAGS", "tags"), c("META", "meta"), c("N", "n"),
		c("OK", "ok"), c("LONG", "long"), c("NESTED", "nested"), c("MULTI", "multi"), c("MISSING", "nope.deeper")}}
	var b bytes.Buffer
	if err := render(&b, FormatTable, body, v); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, s := range []string{"2026-11-03 10:00:00Z", "p,q", `{"k":"v"}`, "true", "…", `[{"a":1}]`, "l1 l2"} {
		if !strings.Contains(out, s) {
			t.Errorf("table lacks %q:\n%s", s, out)
		}
	}
	b.Reset()
	if err := render(&b, FormatYAML, body, v); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "- id: a") || !strings.Contains(b.String(), "multi: |-") {
		t.Fatalf("yaml:\n%s", b.String())
	}
	b.Reset()
	if err := render(&b, FormatJSON, body, v); err != nil || !strings.HasPrefix(b.String(), "{\n  \"items\"") {
		t.Fatalf("json %v %s", err, b.String())
	}
	b.Reset()
	obj := []byte(`{"version":3,"zeta":"z","id":"i","state":"s","empty":null,"$schema":"x"}`)
	var after bool
	if err := render(&b, FormatTable, obj, view{first: []string{"zeta"}, after: func(io.Writer, map[string]any) { after = true }}); err != nil {
		t.Fatal(err)
	}
	if !after || strings.Index(b.String(), "zeta") > strings.Index(b.String(), "id:") || strings.Contains(b.String(), "$schema") || strings.Contains(b.String(), "empty") {
		t.Fatalf("object:\n%s", b.String())
	}
	b.Reset()
	_ = render(&b, FormatTable, []byte(`[{"id":"x"}]`), view{items: "items", cols: []col{c("ID", "id")}})
	if !strings.Contains(b.String(), "x") {
		t.Fatalf("array %s", b.String())
	}
	b.Reset()
	_ = render(&b, FormatTable, []byte(`"plain"`), view{})
	if b.String() != "plain\n" {
		t.Fatalf("scalar %q", b.String())
	}
	for _, f := range []string{FormatTable, FormatJSON, FormatYAML} {
		if err := render(&b, f, []byte("{bad"), view{}); err == nil {
			t.Errorf("%s accepted bad JSON", f)
		}
		if err := render(&b, f, nil, view{}); err != nil {
			t.Errorf("%s empty: %v", f, err)
		}
	}
	if scalar(nil) != "" || scalar(json.Number("4")) != "4" || scalar(false) != "false" || scalar(map[string]any{}) != "{}" || scalar(1.5) != "1.5" {
		t.Fatal("scalar")
	}
	if numberString(float64(3)) != "3" || numberString(int64(4)) != "4" || numberString(5) != "5" || numberString("6") != "6" || numberString(nil) != "" {
		t.Fatal("numberString")
	}
}

func TestInputDocuments(t *testing.T) {
	for _, tc := range []struct {
		in  string
		ok  bool
		key string
	}{
		{`{"a": 1}`, true, "a"},
		{"a: 1\nb:\n  c: [1, 2]\n", true, "a"},
		{"1: x\n", true, "1"},
		{`{"a": `, false, ""},
		{"a: [", false, ""},
	} {
		v, err := parseDoc([]byte(tc.in))
		if (err == nil) != tc.ok {
			t.Errorf("%q: %v", tc.in, err)
			continue
		}
		if tc.ok {
			if _, ok := v.(map[string]any)[tc.key]; !ok {
				t.Errorf("%q: %#v", tc.in, v)
			}
		}
	}
	if m := normalizeYAML(map[any]any{1: []any{map[any]any{"k": "v"}}}).(map[string]any); m["1"].([]any)[0].(map[string]any)["k"] != "v" {
		t.Fatal("normalize")
	}
	m := map[string]any{}
	setPath(m, "a.b.c", 1)
	setPath(m, "a.d", 2)
	if m["a"].(map[string]any)["b"].(map[string]any)["c"] != 1 || m["a"].(map[string]any)["d"] != 2 {
		t.Fatalf("setPath %v", m)
	}
	if r := parseRepo("https://git.example.com/a/b.git@main=src/b"); r["url"] != "https://git.example.com/a/b.git" || r["branch"] != "main" || r["path"] != "src/b" {
		t.Fatalf("repo %v", r)
	}
	if r := parseRepo("git@github.com:a/b.git"); r["url"] != "git@github.com:a/b.git" || r["branch"] != nil {
		t.Fatalf("scp repo %v", r)
	}
}

// Bodies from --file (JSON or YAML) with flags on top, inputs from standard input, and the input-validation errors.
func TestFormInputs(t *testing.T) {
	f := newFake(t)
	f.on("POST /spaces/"+spaceID+"/runs", func(w http.ResponseWriter, _ *http.Request, body map[string]any) {
		reply(w, 201, map[string]any{"id": "run_x", "role": body["role"]})
	})
	f.json("GET /spaces", 200, map[string]any{"items": []any{map[string]any{"id": spaceID, "slug": "pay"}, map[string]any{"id": "spc_dup1", "slug": "dup"},
		map[string]any{"id": "spc_dup2", "slug": "dup"}}})
	e := f.env()
	doc := writeFile(t, "launch.yaml", "role: worker\nkind: worker\nsettings:\n  effort: high\n", 0o644)
	e.ok(t, "run", "launch", "--space", "pay", "-f", doc, "--model", "opus", "--setting", "a=b", "--worktree-repo", "app", "--prompt", "go")
	got := f.last("POST", "/spaces/"+spaceID+"/runs").body
	if got["role"] != "worker" || got["model"] != "opus" || got["settings"].(map[string]any)["a"] != "b" || got["worktree"].(map[string]any)["repo"] != "app" ||
		got["prompt"] != "go" {
		t.Fatalf("launch body %v", got)
	}
	e.extra = map[string]string{EnvSpace: spaceID}
	in := e
	in.stdin = "Do the thing.\n"
	in.ok(t, "run", "launch", "--role", "worker", "--prompt-file", "-")
	if got := f.last("POST", "/spaces/"+spaceID+"/runs").body; got["prompt"] != "Do the thing.\n" {
		t.Fatalf("prompt from stdin %v", got)
	}
	e.extra = nil
	for _, args := range [][]string{
		{"run", "launch", "--space", "pay", "--prompt", "x", "--prompt-file", doc},
		{"run", "launch", "--space", "pay", "-f", "-", "--prompt-file", "-"},
		{"run", "launch", "--space", "pay", "-f", filepath.Join(t.TempDir(), "missing")},
		{"run", "launch", "--space", "pay", "--setting", "novalue"},
		{"run", "launch", "--space", "dup"},
		{"run", "launch"},
		{"run", "get", "--space", "pay"},
	} {
		e.code(t, ExitUsage, args...)
	}
	list := writeFile(t, "list.json", `[1, 2]`, 0o644)
	e.code(t, ExitUsage, "run", "launch", "--space", "pay", "-f", list)
	e.code(t, ExitNotFound, "run", "launch", "--space", "nope")
	empty := writeFile(t, "empty", "\n", 0o600)
	e.code(t, ExitUsage, "account", "create", "--name", "x", "--secret-file", empty)
	e.code(t, ExitUsage, "account", "create", "--name", "x", "--secret-file", empty, "--secret-stdin")
}

// Updates read the object first and send its version as If-Match with the merged body; --if-match overrides it.
func TestUpdateReadsVersion(t *testing.T) {
	f := newFake(t)
	space := map[string]any{"id": spaceID, "slug": "pay", "name": "Pay", "description": "d", "goals": "", "version": 7, "state": "active",
		"disconnect_policy": "interrupt", "lease": map[string]any{"generation": 1}}
	f.json("GET /spaces/"+spaceID, 200, space)
	f.on("PUT /spaces/"+spaceID, func(w http.ResponseWriter, _ *http.Request, body map[string]any) { reply(w, 200, body) })
	f.on("POST /spaces/"+spaceID+"/state", func(w http.ResponseWriter, _ *http.Request, body map[string]any) { reply(w, 200, body) })
	e := f.env()
	e.ok(t, "space", "update", spaceID, "--name", "Payments")
	req := f.last("PUT", "/spaces/"+spaceID)
	if req.header.Get("If-Match") != `"7"` || req.body["name"] != "Payments" || req.body["description"] != "d" || req.body["lease"] != nil ||
		req.body["standing_instructions"] != "" {
		t.Fatalf("update %v %v", req.header, req.body)
	}
	e.ok(t, "space", "update", spaceID, "--name", "P", "--if-match", `W/"5"`)
	if h := f.last("PUT", "/spaces/"+spaceID).header.Get("If-Match"); h != `"5"` {
		t.Fatalf("explicit If-Match %s", h)
	}
	e.code(t, ExitUsage, "space", "update", spaceID, "--if-match", "abc")
	e.ok(t, "space", "pause", spaceID, "--mode", "emergency")
	if b := f.last("POST", "/spaces/"+spaceID+"/state").body; b["state"] != "paused" || b["mode"] != "emergency" {
		t.Fatalf("pause %v", b)
	}
	// Human actions: respond and cancel carry the action's version.
	act := "hac_1"
	f.json("GET /spaces/"+spaceID+"/human-actions/"+act, 200, map[string]any{"id": act, "version": 4})
	f.on("POST /spaces/"+spaceID+"/human-actions/"+act+"/respond", func(w http.ResponseWriter, _ *http.Request, body map[string]any) { reply(w, 200, body) })
	data := writeFile(t, "data.yaml", "approved_by: ops\n", 0o644)
	e.ok(t, "human", "respond", "--space", spaceID, act, "--outcome", "approved", "--note", "ok", "--data-file", data)
	r := f.last("POST", "/spaces/"+spaceID+"/human-actions/"+act+"/respond")
	if r.header.Get("If-Match") != `"4"` || r.body["outcome"] != "approved" || r.body["data"].(map[string]any)["approved_by"] != "ops" {
		t.Fatalf("respond %v %v", r.header, r.body)
	}
	// A 412 from a stale version is a conflict.
	f.on("POST /spaces/"+spaceID+"/human-actions/"+act+"/cancel", func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
		problem(w, 412, "conflict", "the action changed")
	})
	e.code(t, ExitConflict, "human", "cancel", "--space", spaceID, act, "--reason", "dup")
	// Publishing a prompt sends the draft's version; without a draft the CLI refuses.
	f.json("GET /prompts/p1", 200, map[string]any{"id": "prm_1", "versions": []any{map[string]any{"state": "published", "version": 1},
		map[string]any{"state": "draft", "version": 3}}})
	f.on("POST /prompts/prm_1/publish", func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
		reply(w, 200, map[string]any{"state": "published"})
	})
	e.ok(t, "prompt", "publish", "p1")
	if h := f.last("POST", "/prompts/prm_1/publish").header.Get("If-Match"); h != `"3"` {
		t.Fatalf("publish If-Match %s", h)
	}
	f.json("GET /skills/s1", 200, map[string]any{"id": "skl_1", "draft": map[string]any{"version": 2}})
	f.on("POST /skills/skl_1/publish", func(w http.ResponseWriter, _ *http.Request, _ map[string]any) { reply(w, 200, map[string]any{}) })
	e.ok(t, "skill", "publish", "s1")
	if h := f.last("POST", "/skills/skl_1/publish").header.Get("If-Match"); h != `"2"` {
		t.Fatalf("skill If-Match %s", h)
	}
}

// Relocation: the unavailable-source flow needs an explicit acknowledgement, --wait polls until a final state, and a
// blocked or failed relocation exits 2 after printing its issues.
func TestRelocation(t *testing.T) {
	f := newFake(t)
	f.json("GET /nodes", 200, map[string]any{"items": []any{map[string]any{"id": nodeID, "name": "n2"}}})
	base := "/spaces/" + spaceID + "/relocation"
	states := []string{"requested", "checking", "preparing_destination", "done"}
	polls := 0
	f.on("POST "+base, func(w http.ResponseWriter, _ *http.Request, body map[string]any) {
		polls = 0
		reply(w, 202, map[string]any{"id": "rel_1", "state": "requested", "mode": "clean"})
	})
	f.on("GET "+base, func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
		st := states[min(polls, len(states)-1)]
		polls++
		reply(w, 200, map[string]any{"id": "rel_1", "state": st, "issues": []any{}})
	})
	e := f.env()
	r := e.run(t, "space", "relocate", "start", spaceID, "--to", "n2", "--stop-runs", "--wait")
	if r.code != 0 || !strings.Contains(r.err, "preparing_destination") || !strings.Contains(r.out, "done") {
		t.Fatalf("wait: %d %s %s", r.code, r.out, r.err)
	}
	b := f.last("POST", base).body
	if b["destination_node_id"] != nodeID || b["stop_runs"] != true || f.last("POST", base).header.Get("Idempotency-Key") == "" {
		t.Fatalf("start body %v", b)
	}
	e.code(t, ExitUsage, "space", "relocate", "start", spaceID)
	e.code(t, ExitUsage, "space", "relocate", "start", spaceID, "--to", "n2", "--source-unavailable", "--acknowledge-loss")
	e.code(t, ExitUsage, "space", "relocate", "start", spaceID, "--to", "n2", "--repo", "https://x/y.git")
	e.code(t, ExitNotFound, "space", "relocate", "start", spaceID, "--to", "n9")
	e.ok(t, "space", "relocate", "start", spaceID, "--to", nodeID, "--source-unavailable", "--acknowledge-loss",
		"--acknowledgement", "node died", "--repo", "https://git.example.com/pay.git@main")
	b = f.last("POST", base).body
	repos := b["repositories"].([]any)
	if b["source_unavailable"] != true || b["acknowledgement"] != "node died" || repos[0].(map[string]any)["branch"] != "main" {
		t.Fatalf("unavailable body %v", b)
	}
	e.ok(t, "space", "relocate", "precheck", spaceID, "--to", "n2")
	if b := f.last("POST", base).body; b["precheck"] != true {
		t.Fatalf("precheck %v", b)
	}
	// Blocked: issues are printed, exit 2.
	states = []string{"blocked"}
	f.on("POST "+base+"/retry", func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
		reply(w, 200, map[string]any{"id": "rel_1", "state": "checking"})
	})
	f.on("GET "+base, func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
		reply(w, 200, map[string]any{"id": "rel_1", "state": "blocked", "blocked_step": "check",
			"issues": []any{map[string]any{"code": "unpushed", "path": "repo", "exportable": true, "detail": "2 commits"}},
			"loss":   map[string]any{"statement": "local work may be lost"}})
	})
	r = e.code(t, ExitAPI, "space", "relocate", "retry", spaceID, "--dirty-state", "export", "--wait")
	if !strings.Contains(r.out, "unpushed") || !strings.Contains(r.out, "local work may be lost") || !strings.Contains(r.err, "relocate retry") {
		t.Fatalf("blocked: %s %s", r.out, r.err)
	}
	f.on("GET "+base, func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
		reply(w, 200, map[string]any{"id": "rel_1", "state": "failed", "error": "destination refused"})
	})
	if r := e.code(t, ExitAPI, "space", "relocate", "retry", spaceID, "--wait", "-o", "json"); !strings.Contains(r.err, "destination refused") {
		t.Fatalf("failed: %s", r.err)
	}
	f.on("GET "+base, func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
		reply(w, 200, map[string]any{"id": "rel_1", "state": "checking"})
	})
	if r := e.code(t, ExitAPI, "space", "relocate", "retry", spaceID, "--wait", "--wait-timeout", "30ms"); !strings.Contains(r.err, "still checking") {
		t.Fatalf("timeout: %s", r.err)
	}
	f.on("POST "+base+"/cancel", func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
		reply(w, 200, map[string]any{"state": "cancelled"})
	})
	f.on("POST "+base+"/release", func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
		reply(w, 200, map[string]any{"source_state": "deleted"})
	})
	e.ok(t, "space", "relocate", "cancel", spaceID)
	e.code(t, ExitUsage, "space", "relocate", "release", spaceID)
	e.ok(t, "space", "relocate", "release", spaceID, "--yes")
}

// Commands whose bodies are built in code: grants, token create with a secret file, limits, task moves, MCP headers,
// cross-space peers and the audit verification.
func TestBuiltBodies(t *testing.T) {
	f := newFake(t)
	f.json("GET /spaces", 200, map[string]any{"items": []any{map[string]any{"id": spaceID, "slug": "pay"}}})
	f.on("POST /tokens", func(w http.ResponseWriter, _ *http.Request, body map[string]any) {
		reply(w, 201, map[string]any{"secret": "spaceman_at_newsecret", "token": map[string]any{"id": "atk_1", "grants": body["grants"]}})
	})
	e := f.env()
	out := e.ok(t, "token", "create", "ci", "--grant", "viewer,operator:pay", "--expires-in-days", "30")
	if !strings.Contains(out, "spaceman_at_newsecret") {
		t.Fatalf("token secret not shown: %s", out)
	}
	gs := f.last("POST", "/tokens").body["grants"].([]any)
	if gs[0].(map[string]any)["domain"] != "*" || gs[1].(map[string]any)["domain"] != spaceID || f.last("POST", "/tokens").body["name"] != "ci" {
		t.Fatalf("grants %v", gs)
	}
	sf := filepath.Join(t.TempDir(), "tok")
	r := e.run(t, "-o", "json", "token", "create", "ci", "--grant", "admin", "--never-expires", "--secret-file", sf)
	if r.code != 0 || strings.Contains(r.out, "newsecret") {
		t.Fatalf("secret printed with --secret-file: %s", r.out)
	}
	if b, _ := os.ReadFile(sf); string(b) != "spaceman_at_newsecret\n" {
		t.Fatalf("secret file %q", b)
	}
	e.code(t, ExitUsage, "token", "create", "ci", "--grant", "root")
	file := writeFile(t, "grants.json", `{"grants":[{"role":"viewer","domain":"*"}]}`, 0o644)
	e.ok(t, "token", "create", "ci", "-f", file)
	f.json("GET /tokens", 200, map[string]any{"items": []any{}})
	f.json("GET /admin/tokens", 200, map[string]any{"items": []any{map[string]any{"id": "atk_1", "grants": []any{map[string]any{"role": "admin", "domain": "*"}}}}})
	if r := e.run(t, "token", "list"); !strings.Contains(r.err, "No API tokens") {
		t.Fatalf("empty list %s", r.err)
	}
	if out := e.ok(t, "token", "list", "--all-users"); !strings.Contains(out, "admin:*") {
		t.Fatalf("all tokens %s", out)
	}
	f.json("DELETE /admin/tokens/atk_1", 200, map[string]any{"id": "atk_1"})
	e.ok(t, "token", "revoke", "atk_1", "--any")

	// Task moves.
	f.on("POST /spaces/"+spaceID+"/tasks/tsk_1/move", func(w http.ResponseWriter, _ *http.Request, body map[string]any) { reply(w, 200, body) })
	e.ok(t, "task", "move", "--space", "pay", "tsk_1", "--top")
	if b := f.last("POST", "/spaces/"+spaceID+"/tasks/tsk_1/move").body; b["to"] != "top" {
		t.Fatalf("move %v", b)
	}
	e.code(t, ExitUsage, "task", "move", "--space", "pay", "tsk_1", "--top", "--bottom")
	e.code(t, ExitUsage, "task", "move", "--space", "pay", "tsk_1", "--top", "--before", "tsk_2")

	// MCP headers come from a file and set set_headers.
	f.on("POST /mcp-servers", func(w http.ResponseWriter, _ *http.Request, body map[string]any) { reply(w, 201, body) })
	hdr := writeFile(t, "h.yaml", "Authorization: Bearer x\n", 0o600)
	e.ok(t, "mcp", "server", "create", "docs", "--name", "Docs", "--url", "https://m.example.com", "--headers-file", hdr, "--tool", "a,b")
	b := f.last("POST", "/mcp-servers").body
	if b["set_headers"] != true || b["headers"].(map[string]any)["Authorization"] != "Bearer x" || len(b["tools"].([]any)) != 2 {
		t.Fatalf("mcp %v", b)
	}

	// The audit verification exits 2 when the chain is broken.
	f.json("POST /audit/verify", 200, map[string]any{"ok": false, "broken_at": 12, "problem": "hash mismatch"})
	if r := e.code(t, ExitAPI, "audit", "verify"); !strings.Contains(r.err, "broken at sequence 12") {
		t.Fatalf("verify %s", r.err)
	}

	// Limits with no overrides yet: no If-Match.
	f.json("GET /spaces/"+spaceID+"/limits", 200, map[string]any{"version": 0, "limits": []any{map[string]any{"key": "max_concurrent_runs", "source": "installation"}}})
	f.on("PUT /spaces/"+spaceID+"/limits", func(w http.ResponseWriter, _ *http.Request, body map[string]any) { reply(w, 200, body) })
	e.ok(t, "limits", "set", "--space", "pay", "--set", "max_concurrent_runs=4", "--on-lower", "stop")
	r2 := f.last("PUT", "/spaces/"+spaceID+"/limits")
	if r2.header.Get("If-Match") != "" || r2.body["on_lower"] != "stop" {
		t.Fatalf("limits %v %v", r2.header, r2.body)
	}

	// Event connection create defaults to enabled; replay needs a reason.
	f.on("POST /event-connections", func(w http.ResponseWriter, _ *http.Request, body map[string]any) { reply(w, 201, body) })
	creds := writeFile(t, "c.creds", "-----BEGIN NATS USER JWT-----\n", 0o600)
	e.ok(t, "event", "connection", "create", "--name", "p", "--url", "nats://x", "--creds-file", creds)
	if b := f.last("POST", "/event-connections").body; b["enabled"] != true || !strings.HasPrefix(b["creds"].(string), "-----BEGIN") {
		t.Fatalf("connection %v", b)
	}
	f.json("GET /spaces/"+spaceID+"/subscriptions/sub_1", 200, map[string]any{"id": "sub_1", "version": 2})
	e.code(t, ExitUsage, "event", "subscription", "replay", "--space", "pay", "sub_1", "--policy", "all")
	f.on("POST /spaces/"+spaceID+"/subscriptions/sub_1/replay", func(w http.ResponseWriter, _ *http.Request, body map[string]any) { reply(w, 200, body) })
	e.ok(t, "event", "subscription", "replay", "--space", "pay", "sub_1", "--policy", "seq", "--seq", "10", "--reason", "bug")
	if h := f.last("POST", "/spaces/"+spaceID+"/subscriptions/sub_1/replay").header.Get("If-Match"); h != `"2"` {
		t.Fatalf("replay If-Match %s", h)
	}
	f.json("GET /event-templates", 200, map[string]any{"items": []any{map[string]any{"id": "etp_1", "versions": []any{1, 2}}}})
	if out := e.ok(t, "event", "template", "list"); !strings.Contains(out, "2") {
		t.Fatalf("versions %s", out)
	}
	f.json("GET /audit", 200, map[string]any{"items": []any{map[string]any{"seq": 1, "at": 1762160400000000, "action": "x"}}})
	if out := e.ok(t, "audit", "list"); !strings.Contains(out, "2025-11-03") {
		t.Fatalf("audit at %s", out)
	}
}

func TestSpecParsing(t *testing.T) {
	if _, err := ParseSpec([]byte("{")); err == nil {
		t.Fatal("bad JSON accepted")
	}
	if _, err := ParseSpec([]byte(`{"paths":{"/x":{"get":"nope"}}}`)); err == nil {
		t.Fatal("bad operation accepted")
	}
	doc := `{"paths":{"/x/{id}":{"parameters":[],"put":{"operationId":"put-x","parameters":[{"in":"header","name":"If-Match"},{"in":"query","name":"q"}],
	"requestBody":{"content":{"application/json":{"schema":{"$ref":"#/components/schemas/X"}}}}}}},
	"components":{"schemas":{"X":{"type":"object","required":["a","b","c","d","e","f"],"properties":{"$schema":{"type":"string"},
	"a":{"type":"string"},"b":{"type":["boolean","null"]},"c":{"type":"integer"},"d":{"type":"array"},"e":{"properties":{}},"f":{"$ref":"#/components/schemas/Y"}}},
	"Y":{"type":"number"}}}}`
	s, err := ParseSpec([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	op, err := s.Op("put-x")
	if err != nil || !op.Headers["If-Match"] || !op.Query["q"] || op.Body["b"] != "boolean" || op.Body["e"] != "object" {
		t.Fatalf("op %+v %v", op, err)
	}
	p := op.project(map[string]any{"a": "keep", "zz": "drop", "c": nil})
	if p["a"] != "keep" || p["zz"] != nil || p["b"] != false || p["c"] != 0 || p["e"] == nil || p["f"] != 0 {
		t.Fatalf("project %v", p)
	}
	if _, err := op.Expand(map[string]string{}); err == nil {
		t.Fatal("missing parameter accepted")
	}
	if path, _ := op.Expand(map[string]string{"id": "a/b"}); path != "/x/a%2Fb" {
		t.Fatalf("escape %s", path)
	}
	if _, err := s.Op("nope"); err == nil {
		t.Fatal("unknown op")
	}
	a := &App{SpecSource: func() ([]byte, error) { return nil, errors.New("no doc") }}
	if _, err := a.loadSpec(); err == nil {
		t.Fatal("spec error swallowed")
	}
	if _, err := (&App{}).loadSpec(); err == nil {
		t.Fatal("no source")
	}
}
