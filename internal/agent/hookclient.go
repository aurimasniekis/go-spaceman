package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/harness"
)

// Bridge tools of Claude Code's own processes (the StopFailure hook and the status-line wrapper). They are served by the
// node agent itself on the bridge socket, authenticated like every bridge call with the run token, and never forwarded
// to central.
const (
	ToolHookStopFailure = "hook.stop_failure"
	ToolStatusLine      = "hook.statusline"
)

const (
	// hookSendTimeout bounds one send to the agent socket: dial, write and the agent's answer.
	hookSendTimeout = 500 * time.Millisecond
	// StatusLineWrapTimeout bounds the profile's own status-line command.
	StatusLineWrapTimeout = 2 * time.Second
	// wrapOutputMax bounds the output taken from the wrapped command.
	wrapOutputMax = harness.MaxStatusInput
)

// sendHook sends one typed payload to the node agent's socket. Every failure is returned and meant to be ignored by the
// callers: Claude Code must never be held up or broken by a missing or broken agent. The run token comes from the
// environment (SPACEMAN_RUN_TOKEN), never from the command line.
func sendHook(ctx context.Context, socket, run, tool string, payload any) error {
	args, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, hookSendTimeout)
	defer cancel()
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		return err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(hookSendTimeout))
	if err := json.NewEncoder(c).Encode(bridgeRequest{Run: run, Token: os.Getenv("SPACEMAN_RUN_TOKEN"), Tool: tool, Args: args}); err != nil {
		return err
	}
	var res bridgeResponse
	if err := json.NewDecoder(c).Decode(&res); err != nil {
		return err
	}
	if !res.OK {
		return errHookRefused
	}
	return nil
}

type hookError string

func (e hookError) Error() string { return string(e) }

const errHookRefused = hookError("the node agent refused the hook data")

// StopFailureHook is the body of `spaceman-agent hook stop-failure`: it reads the hook input (bounded), keeps only the
// allowlisted fields and sends them to the agent. It never returns an error and writes nothing; the hook's output and
// exit code are ignored by Claude Code anyway.
func StopFailureHook(ctx context.Context, in io.Reader, socket, run string) {
	if in == nil || socket == "" || run == "" {
		return
	}
	raw, _ := io.ReadAll(io.LimitReader(in, harness.MaxStatusInput))
	f, ok := harness.SanitizeStopFailure(raw)
	if !ok {
		return
	}
	_ = sendHook(ctx, socket, run, ToolHookStopFailure, f)
}

// StatusLine is the body of `spaceman-agent statusline`. It reads Claude Code's status-line JSON (at most 64 KiB), sends
// the allowlisted subset to the agent (in parallel, at most 500 ms, failures ignored), and prints the profile's own
// status line when wrap is set: the command runs under `sh -c` with the same input, for at most 2 s, and its output is
// printed as it is (at most 64 KiB). Without a wrapped command, or when it fails or times out, a short SpaceMan line is
// printed instead. It never fails: a missing agent socket changes nothing about what is printed.
func StatusLine(ctx context.Context, in io.Reader, out io.Writer, socket, run, wrap string) {
	var raw []byte
	if in != nil {
		raw, _ = io.ReadAll(io.LimitReader(in, harness.MaxStatusInput))
	}
	snap, parsed := harness.SanitizeStatusLine(raw)
	var wg sync.WaitGroup
	if parsed && socket != "" && run != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = sendHook(ctx, socket, run, ToolStatusLine, snap)
		}()
	}
	text, ok := "", false
	if wrap != "" {
		text, ok = runWrapped(ctx, wrap, raw)
	}
	wg.Wait()
	if !ok {
		text = defaultStatusLine(snap)
	}
	_, _ = io.WriteString(out, text)
}

// runWrapped runs the profile's status-line command and returns what it printed. ok is false when it failed, timed out
// or printed nothing.
func runWrapped(ctx context.Context, command string, stdin []byte) (string, bool) {
	ctx, cancel := context.WithTimeout(ctx, StatusLineWrapTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command) //nolint:gosec // the profile's own status-line command, run as Claude Code would
	cmd.Stdin = bytes.NewReader(stdin)
	var buf cappedBuffer
	buf.max = wrapOutputMax
	cmd.Stdout = &buf
	cmd.WaitDelay = 200 * time.Millisecond
	// Its own process group, so a timeout kills the whole group: a hung status script must not leave orphans at every
	// refresh. Background children of a command that exits cleanly are not killed, as when Claude Code runs it directly.
	// Pdeathsig ends the command if this wrapper is killed first (it is tied to the thread that started the process,
	// hence the locked thread around Start).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	runtime.LockOSThread()
	err := cmd.Start()
	runtime.UnlockOSThread()
	if err != nil {
		return "", false
	}
	// A clean exit whose background child still holds the output open (ErrWaitDelay) keeps what was printed.
	if err := cmd.Wait(); (err != nil && !errors.Is(err, exec.ErrWaitDelay)) || ctx.Err() != nil {
		return "", false
	}
	return buf.b.String(), buf.b.Len() > 0
}

// cappedBuffer keeps the first max bytes written and drops the rest.
type cappedBuffer struct {
	b   bytes.Buffer
	max int
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if room := c.max - c.b.Len(); room > 0 {
		c.b.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

// defaultStatusLine is the line printed when the profile has no status line of its own: the model, the context use and
// the subscription windows, from allowlisted fields only.
func defaultStatusLine(s harness.StatusSnapshot) string {
	line := "SpaceMan"
	if s.ModelID != "" {
		line += " · " + s.ModelID
	}
	if s.ContextUsedPct != nil {
		line += " · ctx " + strconv.Itoa(int(*s.ContextUsedPct)) + "%"
	}
	if s.FiveHour != nil {
		line += " · 5h " + strconv.Itoa(int(s.FiveHour.UsedPct)) + "%"
	}
	if s.SevenDay != nil {
		line += " · 7d " + strconv.Itoa(int(s.SevenDay.UsedPct)) + "%"
	}
	return line
}
