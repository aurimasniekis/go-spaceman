//go:build demo

package demo

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Evidence kinds of a step (docs/DEMO.md): what the step's checks rest on.
const (
	// Real: real processes only (central, node agents, herdr-core, Git, NATS with the real go-pulse SDK, a browser);
	// no harness behaviour is involved.
	Real = "real"
	// Simulated: what is shown depends on the stub harness standing in for Claude Code or Codex (it makes the tool
	// calls an agent would make). Everything around it is real.
	Simulated = "simulated"
	// Mixed: a real Claude Code run (SPACEMAN_DEMO_REAL=1) and stub programs together.
	Mixed = "mixed"
)

// record is one line of the evidence file.
type record struct {
	Step     int               `json:"step"`
	Title    string            `json:"title"`
	Result   string            `json:"result" enum:"pass,fail"`
	Evidence string            `json:"evidence"`
	Why      string            `json:"why,omitempty"`
	Checked  []string          `json:"checked"`
	IDs      map[string]string `json:"ids,omitempty"`
	Notes    []string          `json:"notes,omitempty"`
	Failure  string            `json:"failure,omitempty"`
	Started  time.Time         `json:"started_at"`
	Seconds  float64           `json:"seconds"`
	Mode     string            `json:"mode"`
}

// evidence appends records to the evidence file (JSON lines) and keeps them for the summary.
type evidence struct {
	mu      sync.Mutex
	path    string
	mode    string
	records []record
}

func (e *evidence) add(r record) {
	e.mu.Lock()
	defer e.mu.Unlock()
	r.Mode = e.mode
	e.records = append(e.records, r)
	b, _ := json.Marshal(r)
	f, err := os.OpenFile(e.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	_, _ = f.Write(append(b, '\n'))
	_ = f.Close()
}

// summary is the table printed at the end of a run.
func (e *evidence) summary() string {
	var b strings.Builder
	pass := 0
	fmt.Fprintf(&b, "\nSpaceMan demonstration (%s mode), evidence: %s\n\n", e.mode, e.path)
	fmt.Fprintf(&b, "%-4s %-6s %-10s %-7s %s\n", "STEP", "RESULT", "EVIDENCE", "SECONDS", "TITLE")
	rs := append([]record(nil), e.records...)
	sort.SliceStable(rs, func(i, j int) bool { return rs[i].Step < rs[j].Step })
	for _, r := range rs {
		if r.Result == "pass" {
			pass++
		}
		fmt.Fprintf(&b, "%-4d %-6s %-10s %7.1f %s\n", r.Step, r.Result, r.Evidence, r.Seconds, r.Title)
		if r.Failure != "" {
			fmt.Fprintf(&b, "     ! %s\n", tail(r.Failure, 400))
		}
	}
	fmt.Fprintf(&b, "\n%d of %d steps passed\n", pass, len(rs))
	return b.String()
}

// step is the context of one demonstration step: its test, the stack and what it has checked.
type step struct {
	t   *testing.T
	s   *stack
	rec *record
}

// check records something the step verified.
func (st *step) check(format string, a ...any) {
	st.rec.Checked = append(st.rec.Checked, fmt.Sprintf(format, a...))
}

// note records an observation that is not a pass/fail check.
func (st *step) note(format string, a ...any) {
	st.rec.Notes = append(st.rec.Notes, fmt.Sprintf(format, a...))
}

func (st *step) id(k, v string) {
	if st.rec.IDs == nil {
		st.rec.IDs = map[string]string{}
	}
	st.rec.IDs[k] = v
}

func (st *step) fatalf(format string, a ...any) {
	st.t.Helper()
	msg := fmt.Sprintf(format, a...)
	st.rec.Failure = msg
	st.t.Fatal(msg)
}

// req sends one API request with the root token.
func (st *step) req(method, path string, body any, hdr ...string) (int, []byte) {
	st.t.Helper()
	code, b, _, err := st.s.api(method, path, body, hdr...)
	if err != nil {
		st.fatalf("%s %s: %v", method, path, err)
	}
	return code, b
}

// call sends a request and fails the step unless it answers want; it returns the decoded body.
func (st *step) call(method, path string, body any, want int, hdr ...string) obj {
	st.t.Helper()
	code, b := st.req(method, path, body, hdr...)
	if code != want {
		st.fatalf("%s %s: %d, want %d: %s", method, path, code, want, tail(string(b), 600))
	}
	var o obj
	if len(b) > 0 {
		if err := json.Unmarshal(b, &o); err != nil {
			st.fatalf("%s %s: decoding: %v", method, path, err)
		}
	}
	return o
}

func (st *step) get(path string) obj {
	st.t.Helper()
	return st.call("GET", path, nil, 200)
}

// items is the "items" array of a list response.
func (st *step) items(path string) []obj {
	st.t.Helper()
	return objs(st.get(path), "items")
}

func (st *step) eventually(d time.Duration, what string, cond func() bool) {
	st.t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			st.fatalf("timed out after %s waiting for %s", d, what)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// cli runs the spaceman command line against central (JSON output) and decodes its answer.
func (st *step) cli(args ...string) any {
	st.t.Helper()
	full := append([]string{"--server", st.s.url, "--token-file", st.s.tokenFile, "-o", "json"}, args...)
	cmd := exec.CommandContext(context.Background(), st.s.bin+"/spaceman", full...) //nolint:gosec // the demo's own build
	cmd.Env = cleanEnv()
	out, err := cmd.Output()
	if err != nil {
		var stderr string
		if ee, ok := err.(*exec.ExitError); ok { //nolint:errorlint // only the stderr text is wanted
			stderr = string(ee.Stderr)
		}
		st.fatalf("spaceman %s: %v %s", strings.Join(args, " "), err, tail(stderr, 600))
	}
	st.check("CLI: spaceman %s", strings.Join(args, " "))
	var v any
	if len(strings.TrimSpace(string(out))) > 0 {
		if err := json.Unmarshal(out, &v); err != nil {
			st.fatalf("spaceman %s: output %q: %v", strings.Join(args, " "), tail(string(out), 300), err)
		}
	}
	return v
}

// ifMatch is the If-Match header for an object's version.
func ifMatch(o obj) []string {
	return []string{"If-Match", `"` + strconv.FormatInt(num(o, "version"), 10) + `"`}
}

// obj is a decoded JSON object.
type obj = map[string]any

// at walks a dotted path ("a.b.0.c") through objects and arrays.
func at(v any, path string) any {
	if path == "" {
		return v
	}
	for _, k := range strings.Split(path, ".") {
		switch x := v.(type) {
		case map[string]any:
			v = x[k]
		case []any:
			i, err := strconv.Atoi(k)
			if err != nil || i < 0 || i >= len(x) {
				return nil
			}
			v = x[i]
		default:
			return nil
		}
	}
	return v
}

func str(v any, path string) string {
	switch x := at(v, path).(type) {
	case string:
		return x
	case nil:
		return ""
	default:
		return fmt.Sprint(x)
	}
}

func num(v any, path string) int64 {
	f, _ := at(v, path).(float64)
	return int64(f)
}

func boolean(v any, path string) bool { b, _ := at(v, path).(bool); return b }

func objs(v any, path string) []obj {
	a, _ := at(v, path).([]any)
	out := make([]obj, 0, len(a))
	for _, x := range a {
		if o, ok := x.(map[string]any); ok {
			out = append(out, o)
		}
	}
	return out
}

func arrLen(v any, path string) int { a, _ := at(v, path).([]any); return len(a) }

func find(list []obj, key, val string) obj {
	for _, o := range list {
		if str(o, key) == val {
			return o
		}
	}
	return nil
}

func jsonArg(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
