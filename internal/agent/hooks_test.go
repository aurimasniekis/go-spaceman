package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/harness"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	rt "github.com/aurimasniekis/go-spaceman/internal/runner/runnertest"
)

const hookSample = `{"session_id":"0b5e3c1e-1111-4222-8333-444455556666","version":"2.1.325","model":{"id":"claude-opus-4-7"},` +
	`"context_window":{"used_percentage":12.5},"rate_limits":{"five_hour":{"used_percentage":100,"resets_at":1790000000}},` +
	`"cwd":"/secret/cwd","last_assistant_message":"SECRET-MESSAGE"}`

func statusLine(t *testing.T, in, socket, wrap string) (string, time.Duration) {
	t.Helper()
	var out bytes.Buffer
	start := time.Now()
	StatusLine(context.Background(), strings.NewReader(in), &out, socket, "run_x", wrap)
	return out.String(), time.Since(start)
}

// The wrapped command gets the same input and what it prints is what Claude Code shows, byte for byte.
func TestStatusLinePrintsWrappedCommandOutput(t *testing.T) {
	dir := t.TempDir()
	got := filepath.Join(dir, "stdin")
	out, _ := statusLine(t, hookSample, filepath.Join(dir, "none.sock"), `cat > '`+got+`'; printf 'my line \033[1mbold\033[0m'`)
	if out != "my line \x1b[1mbold\x1b[0m" {
		t.Fatalf("output %q", out)
	}
	if b, _ := os.ReadFile(got); string(b) != hookSample {
		t.Fatalf("the wrapped command got %q", b)
	}
}

// A wrapped command past 2 s is stopped and the SpaceMan line is printed; one that fails, or prints nothing, too.
func TestStatusLineWrappedCommandTimeoutAndFailure(t *testing.T) {
	// The sleeps outlive the wrapper on purpose; they are killed at the end so none is left behind.
	pids := filepath.Join(t.TempDir(), "pids")
	t.Cleanup(func() { killRecorded(pids) })
	out, took := statusLine(t, hookSample, "", `sleep 10 & echo $! >> '`+pids+`'; wait; echo late`)
	if took > StatusLineWrapTimeout+time.Second {
		t.Fatalf("took %v", took)
	}
	if strings.Contains(out, "late") || !strings.HasPrefix(out, "SpaceMan · claude-opus-4-7 · ctx 12% · 5h 100%") {
		t.Fatalf("timeout output %q", out)
	}
	// A command that keeps a background process holding its output open does not hold the line up either.
	if out, took := statusLine(t, hookSample, "", `(sleep 10 >/dev/null 2>&1 </dev/null & echo $! >> '`+pids+`'; exec sleep 0.01 ; true) ; echo ok; (sleep 5 & echo $! >> '`+pids+`')`); took > StatusLineWrapTimeout+time.Second || out != "ok\n" {
		t.Fatalf("background holder: %q after %v", out, took)
	}
	// A timed-out command's children die with it.
	pidFile := filepath.Join(t.TempDir(), "pid")
	statusLine(t, hookSample, "", `sleep 30 & echo $! > '`+pidFile+`'; wait`)
	b, _ := os.ReadFile(pidFile)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	waitFor(t, "orphan killed", func() bool { return syscall.Kill(pid, 0) != nil })
	for _, wrap := range []string{"exit 3", "true"} {
		if out, _ := statusLine(t, hookSample, "", wrap); !strings.HasPrefix(out, "SpaceMan") {
			t.Errorf("%s: output %q", wrap, out)
		}
	}
}

// killRecorded kills the processes whose IDs are listed one per line in the file.
func killRecorded(file string) {
	b, _ := os.ReadFile(file)
	for _, f := range strings.Fields(string(b)) {
		if pid, err := strconv.Atoi(f); err == nil && pid > 1 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
}

// Killing the wrapper with SIGKILL takes the directly wrapped command with it (Pdeathsig), though it has its own group.
func TestStatusLineWrapperKilledKillsWrappedCommand(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the agent binary's hook")
	}
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	t.Cleanup(func() { killRecorded(pidFile) })
	// The test binary is the wrapper: re-run it as a helper that calls StatusLine.
	cmd := exec.Command(os.Args[0], "-test.run=TestStatusLineWrapperHelper")
	cmd.Env = append(os.Environ(), "SM_STATUS_HELPER=1", "SM_STATUS_PIDFILE="+pidFile)
	cmd.Stdout = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	var pid int
	waitFor(t, "wrapped command started", func() bool {
		b, _ := os.ReadFile(pidFile)
		pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		return pid > 1
	})
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	waitFor(t, "wrapped command killed with the wrapper", func() bool { return syscall.Kill(pid, 0) != nil })
}

// TestStatusLineWrapperHelper is the wrapper process of the test above; it does nothing in a normal run.
func TestStatusLineWrapperHelper(t *testing.T) {
	if os.Getenv("SM_STATUS_HELPER") != "1" {
		t.Skip("helper process")
	}
	pidFile := os.Getenv("SM_STATUS_PIDFILE")
	command := `echo $$ > '` + pidFile + `'; exec sleep 37`
	if g := os.Getenv("SM_STATUS_GRANDCHILD"); g != "" {
		command = `echo $$ > '` + pidFile + `'; sleep 38 & echo $! > '` + g + `'; wait; true`
	}
	StatusLine(context.Background(), strings.NewReader(hookSample), io.Discard, "", "run_x", command)
}

// SIGHUP or SIGTERM on the wrapper kills the whole wrapped group, a grandchild that is not exec'd included.
func TestStatusLineWrapperSignalKillsGroup(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the wrapper as a helper process")
	}
	for _, sig := range []syscall.Signal{syscall.SIGHUP, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			dir := t.TempDir()
			pidFile, gFile := filepath.Join(dir, "pid"), filepath.Join(dir, "gpid")
			t.Cleanup(func() { killRecorded(pidFile); killRecorded(gFile) })
			cmd := exec.Command(os.Args[0], "-test.run=TestStatusLineWrapperHelper")
			cmd.Env = append(os.Environ(), "SM_STATUS_HELPER=1", "SM_STATUS_PIDFILE="+pidFile, "SM_STATUS_GRANDCHILD="+gFile)
			cmd.Stdout = io.Discard
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
			read := func(f string) int {
				b, _ := os.ReadFile(f)
				n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
				return n
			}
			var pid, gpid int
			waitFor(t, "shell and grandchild started", func() bool { pid, gpid = read(pidFile), read(gFile); return pid > 1 && gpid > 1 })
			if err := cmd.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			_ = cmd.Wait()
			waitFor(t, "grandchild killed", func() bool { return syscall.Kill(gpid, 0) != nil })
			waitFor(t, "shell killed", func() bool { return syscall.Kill(pid, 0) != nil })
		})
	}
}

// What the wrapped command prints is bounded to 64 KiB.
func TestStatusLineWrappedOutputBound(t *testing.T) {
	out, _ := statusLine(t, hookSample, "", `head -c 300000 /dev/zero | tr '\0' x`)
	if len(out) != wrapOutputMax {
		t.Fatalf("output %d bytes", len(out))
	}
}

// Input over 64 KiB is read only that far; the line is still printed and nothing is sent.
func TestStatusLineInputBound(t *testing.T) {
	big := `{"x":"` + strings.Repeat("a", 3*harness.MaxStatusInput) + `"}`
	out, _ := statusLine(t, big, "", `wc -c`)
	if n := strings.TrimSpace(out); n != "65536" {
		t.Fatalf("the wrapped command read %q bytes", n)
	}
}

// A missing, refusing or silent agent socket changes nothing about what is printed, and costs at most the send bound.
func TestStatusLineAgentSocketTrouble(t *testing.T) {
	dir := t.TempDir()
	want, _ := statusLine(t, hookSample, "", `printf hello`)
	if want != "hello" {
		t.Fatalf("baseline %q", want)
	}
	// Missing socket, a regular file instead of one, a directory.
	file := filepath.Join(dir, "file")
	_ = os.WriteFile(file, nil, 0o600)
	for _, sock := range []string{filepath.Join(dir, "missing.sock"), file, dir} {
		if out, took := statusLine(t, hookSample, sock, `printf hello`); out != "hello" || took > time.Second {
			t.Errorf("%s: %q after %v", sock, out, took)
		}
	}
	// A socket that accepts and never answers: the send gives up at its bound.
	sock := filepath.Join(dir, "silent.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close()
		}
	}()
	if out, took := statusLine(t, hookSample, sock, `printf hello`); out != "hello" || took > StatusLineWrapTimeout {
		t.Errorf("silent socket: %q after %v", out, took)
	}
	// The hook with no agent: returns at once, writes nothing.
	start := time.Now()
	StopFailureHook(context.Background(), strings.NewReader(`{"error":"rate_limit"}`), filepath.Join(dir, "missing.sock"), "run_x")
	StopFailureHook(context.Background(), strings.NewReader(`not json`), "", "")
	StopFailureHook(context.Background(), nil, sock, "run_x")
	if time.Since(start) > 2*hookSendTimeout {
		t.Fatalf("hook took %v", time.Since(start))
	}
}

// With no wrapped command the SpaceMan line is printed from allowlisted fields only.
func TestStatusLineNeverPrintsInputText(t *testing.T) {
	out, _ := statusLine(t, hookSample, "", "")
	if strings.Contains(out, "SECRET") || strings.Contains(out, "/secret") {
		t.Fatalf("input text reached the line: %q", out)
	}
	if out, _ := statusLine(t, "not json", "", ""); out != "SpaceMan" {
		t.Fatalf("line for bad input: %q", out)
	}
}

// startHookAgent starts an agent with a Claude Code run and returns its socket.
func startHookAgent(t *testing.T) (a *Agent, fc *wsCentral, runID, socket string, f *rt.Fake) {
	t.Helper()
	a, f = ccAgent(t, t.TempDir(), nil, nil)
	fc = newWSCentral(t, proto.MinorUsageLimit)
	startAgent(t, a, fc, clock.System{})
	fc.next(t, proto.TypeHello)
	runID = "run_hook"
	ccLaunch(a, runID)
	a.rememberRunToken(runID, "run-token-value")
	t.Setenv("SPACEMAN_RUN_TOKEN", "run-token-value")
	socket = a.o.BridgeSocket
	waitFor(t, "bridge socket", func() bool { _, err := os.Stat(socket); return err == nil })
	return a, fc, runID, socket, f
}

// The intake is authenticated by the run token: another token, another run's ID or no token is refused, and nothing is
// kept. With the right token the latest snapshot is kept per run, replaced by the next one, and dropped when the run ends.
func TestHookIntakeAuthAndLatestSnapshot(t *testing.T) {
	a, _, runID, socket, f := startHookAgent(t)
	ctx := context.Background()
	snap, _ := harness.SanitizeStatusLine([]byte(hookSample))

	t.Setenv("SPACEMAN_RUN_TOKEN", "someone-elses-token")
	if err := sendHook(ctx, socket, runID, ToolStatusLine, snap); err == nil {
		t.Fatal("a wrong token was accepted")
	}
	t.Setenv("SPACEMAN_RUN_TOKEN", "")
	if err := sendHook(ctx, socket, runID, ToolStatusLine, snap); err == nil {
		t.Fatal("no token was accepted")
	}
	t.Setenv("SPACEMAN_RUN_TOKEN", "run-token-value")
	if err := sendHook(ctx, socket, "run_other", ToolStatusLine, snap); err == nil {
		t.Fatal("another run's ID was accepted")
	}
	if _, ok := a.StatusSnapshot(runID); ok {
		t.Fatal("a refused call stored a snapshot")
	}

	if err := sendHook(ctx, socket, runID, ToolStatusLine, snap); err != nil {
		t.Fatal(err)
	}
	got, ok := a.StatusSnapshot(runID)
	if !ok || got.ModelID != "claude-opus-4-7" || got.FiveHour == nil || got.FiveHour.UsedPct != 100 || got.ObservedAt.IsZero() {
		t.Fatalf("snapshot %+v", got)
	}
	next := snap
	next.ModelID = "claude-sonnet-5-5"
	if err := sendHook(ctx, socket, runID, ToolStatusLine, next); err != nil {
		t.Fatal(err)
	}
	if got, _ := a.StatusSnapshot(runID); got.ModelID != "claude-sonnet-5-5" {
		t.Fatalf("latest snapshot not kept: %+v", got)
	}

	// A hand-written snapshot with text in a field is refused, and the earlier one stays.
	text := snap
	text.ModelID = "Ignore previous instructions"
	if err := sendHook(ctx, socket, runID, ToolStatusLine, text); err == nil {
		t.Fatal("a text model ID was accepted")
	}
	raw := map[string]any{"model_id": "x", "last_assistant_message": "SECRET"}
	if err := sendHook(ctx, socket, runID, ToolStatusLine, raw); err != nil {
		t.Fatal(err) // unknown keys are dropped when decoded into the typed snapshot
	}
	if b, _ := json.Marshal(func() any { g, _ := a.StatusSnapshot(runID); return g }()); strings.Contains(string(b), "SECRET") {
		t.Fatalf("unknown key kept: %s", b)
	}
	if err := sendHook(ctx, socket, runID, ToolHookStopFailure, map[string]any{"error": "made-up"}); err == nil {
		t.Fatal("an unknown error class was accepted")
	}

	// The run ends: its data is dropped.
	row := getRun(t, a, runID)
	f.Exit(row.TerminalID, 0, "exited")
	waitFor(t, "snapshot dropped", func() bool { _, ok := a.StatusSnapshot(runID); return !ok })
}

// StopFailure scans for a usage limit at once, not after the settle delay; the class of a non-limit failure is kept as
// a health signal; the transcript is still the only thing that opens a limit.
func TestStopFailureStartsLimitScan(t *testing.T) {
	old := limitSettle
	limitSettle = time.Hour
	t.Cleanup(func() { limitSettle = old })
	a, fc, runID, socket, f := startHookAgent(t)
	row := getRun(t, a, runID)
	transcript := filepath.Join(a.o.DataDir, "spaces", "cc", "instances", "ins_cc", "claude", "projects", "-work", row.SessionID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(transcript), 0o700); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	if err := rt.AppendTranscript(transcript, rt.TypedRecord(at.Add(-time.Minute)), rt.AssistantRecord("m1", at.Add(-time.Minute))); err != nil {
		t.Fatal(err)
	}
	fc.collect(t, runID, 300*time.Millisecond)
	reset := at.Add(5 * time.Hour).Truncate(time.Second)
	if err := f.HitLimit(row.TerminalID, transcript, proto.LimitFiveHour, reset, at); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// The limit record is in the transcript and the settle delay is an hour: the hook's scan finds it. It is sent first, so
	// no earlier StopFailure's scan coalesces it away.
	StopFailureHook(ctx, strings.NewReader(`{"error":"rate_limit","last_assistant_message":"SECRET","session_id":"`+row.SessionID+`"}`), socket, runID)
	var found *proto.RunLimit
	deadline := time.After(5 * time.Second)
	for found == nil {
		select {
		case env := <-fc.msgs:
			var r proto.RunReport
			if env.Type == proto.TypeRunReport && env.Decode(&r) == nil && r.RunID == runID && r.Limit != nil && !r.Limit.Cleared {
				found = r.Limit
			}
		case <-deadline:
			t.Fatal("no limit report after StopFailure")
		}
	}
	if found.Kind != proto.LimitFiveHour {
		t.Fatalf("limit %+v", found)
	}
	if c := a.FailureClass(runID); c != "" {
		t.Fatalf("a rate_limit failure set the health class to %q", c)
	}
	if err := sendHook(ctx, socket, runID, ToolHookStopFailure, harness.StopFailure{Error: "authentication_failed"}); err != nil {
		t.Fatal(err)
	}
	if c := a.FailureClass(runID); c != "authentication_failed" {
		t.Fatalf("class %q", c)
	}
}

// A second StopFailure within the gap starts no second scan; one after the gap does. The time of the last scan start
// is what the coalescing keys on, so the test reads it with a fake clock.
func TestStopFailureCoalescesScans(t *testing.T) {
	a, _ := ccAgent(t, t.TempDir(), nil, nil)
	clk := clock.NewFake(time.Now())
	a.o.Clock = clk
	runID := "run_hook"
	ccLaunch(a, runID)
	req := bridgeRequest{Run: runID, Tool: ToolHookStopFailure, Args: json.RawMessage(`{"error":"rate_limit"}`)}
	scans := func() time.Time { a.mu.Lock(); defer a.mu.Unlock(); return a.stopScans[runID] }
	a.stopFailureIntake(context.Background(), req)
	first := scans()
	clk.Advance(stopScanGap - time.Millisecond)
	a.stopFailureIntake(context.Background(), req)
	if got := scans(); !got.Equal(first) {
		t.Fatalf("a second StopFailure inside the gap started a scan (%v != %v)", got, first)
	}
	clk.Advance(2 * time.Millisecond)
	a.stopFailureIntake(context.Background(), req)
	if got := scans(); !got.After(first) {
		t.Fatal("a StopFailure after the gap started no scan")
	}
}

// A StopFailure that races the run's end leaves nothing behind.
func TestStopFailureIntakeAfterRunEndKeepsNothing(t *testing.T) {
	a, _, runID, _, f := startHookAgent(t)
	row := getRun(t, a, runID)
	f.Exit(row.TerminalID, 0, "exited")
	waitFor(t, "run ended", func() bool { r, err := a.j.getRun(context.Background(), runID); return err == nil && ended(r.State) })
	resp := a.stopFailureIntake(context.Background(), bridgeRequest{Run: runID, Tool: ToolHookStopFailure, Args: json.RawMessage(`{"error":"authentication_failed"}`)})
	if !resp.OK {
		t.Fatalf("response %+v", resp)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.failureClasses[runID]; ok {
		t.Fatal("failure class kept for an ended run")
	}
	if _, ok := a.stopScans[runID]; ok {
		t.Fatal("scan time kept for an ended run")
	}
}

// A StopFailure inside the coalescing gap that races the run's end leaves nothing behind either.
func TestStopFailureCoalescedAfterRunEndKeepsNothing(t *testing.T) {
	a, _, runID, _, f := startHookAgent(t)
	req := bridgeRequest{Run: runID, Tool: ToolHookStopFailure, Args: json.RawMessage(`{"error":"authentication_failed"}`)}
	if resp := a.stopFailureIntake(context.Background(), req); !resp.OK {
		t.Fatalf("response %+v", resp)
	}
	row := getRun(t, a, runID)
	f.Exit(row.TerminalID, 0, "exited")
	waitFor(t, "run ended", func() bool { r, err := a.j.getRun(context.Background(), runID); return err == nil && ended(r.State) })
	if resp := a.stopFailureIntake(context.Background(), req); !resp.OK {
		t.Fatalf("response %+v", resp)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.failureClasses[runID]; ok {
		t.Fatal("failure class kept for an ended run")
	}
	if _, ok := a.stopScans[runID]; ok {
		t.Fatal("scan time kept for an ended run")
	}
}

// A bridge line over the cap gets an error answer and closes the connection; the agent does not buffer it.
func TestBridgeLineOverCapClosesConnection(t *testing.T) {
	_, _, _, socket, _ := startHookAgent(t)
	c, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	chunk := bytes.Repeat([]byte("a"), 64<<10)
	var werr error
	for sent := 0; sent <= bridgeLineMax+len(chunk) && werr == nil; sent += len(chunk) {
		_, werr = c.Write(chunk)
	}
	got, err := io.ReadAll(c)
	if err != nil && !errors.Is(err, syscall.ECONNRESET) {
		t.Fatalf("connection not closed by the agent: %v", err)
	}
	if err == nil && !strings.Contains(string(got), "request too large") {
		t.Fatalf("no overflow error, got %q", got)
	}
	// A normal line still works on a new connection.
	c2, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	_ = c2.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = c2.Write([]byte(`{"run":"x","tool":"ping"}` + "\n"))
	if line, err := bufio.NewReader(c2).ReadString('\n'); err != nil || !strings.Contains(line, "unauthenticated") {
		t.Fatalf("normal line: %q %v", line, err)
	}
}
