package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/dialog"
	"github.com/aurimasniekis/go-spaceman/internal/harness"
	"github.com/aurimasniekis/go-spaceman/internal/harness/dist"
	"github.com/aurimasniekis/go-spaceman/internal/harness/dist/disttest"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/runner"
	rt "github.com/aurimasniekis/go-spaceman/internal/runner/runnertest"
)

// dialogEnv is a node with one managed Claude Code 2.1.328 run, a central that welcomes at minor 15, and a log of what
// the node sent it.
type dialogEnv struct {
	t     *testing.T
	a     *Agent
	f     *rt.Fake
	run   string
	space string
	term  string
	mu    sync.Mutex
	got   []proto.Envelope
}

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "dialog", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func newDialogEnv(t *testing.T, edit func(*proto.LaunchPayload)) *dialogEnv {
	t.Helper()
	old := dialogQuiet
	dialogQuiet = 50 * time.Millisecond
	t.Cleanup(func() { dialogQuiet = old })
	srv := disttest.New(t)
	srv.Add("2.1.328", disttest.StubBinary("2.1.328"))
	srv.SetPointer(dist.ChannelLatest, "2.1.328")
	dir := t.TempDir()
	m := srv.Manager(t, filepath.Join(dir, "harness", "claude-code"), nil)
	a, f := ccAgent(t, dir, m, nil)
	fc := newWSCentral(t, proto.MinorDialog)
	startAgent(t, a, fc, clock.System{})
	fc.next(t, proto.TypeHello)
	// Valid IDs, so commands for the run pass validation.
	e := &dialogEnv{t: t, a: a, f: f, run: ids.New(ids.Run), space: ids.New(ids.Space)}
	a.mu.Lock()
	a.leases[e.space] = leaseRow{SpaceID: e.space, Generation: 1, ExpiresAt: time.Now().Add(time.Hour), Workdir: "cc"}
	a.mu.Unlock()
	go func() {
		for env := range fc.msgs {
			e.mu.Lock()
			e.got = append(e.got, env)
			e.mu.Unlock()
		}
	}()
	waitFor(t, "connected", func() bool { return a.protocolAtLeast(proto.MinorDialog) })
	p := proto.LaunchPayload{Harness: "claude-code", Kind: "worker", Role: "implementor", InstanceID: "ins_cc", Prompt: "hello"}
	if edit != nil {
		edit(&p)
	}
	a.launch(context.Background(), proto.Command{CommandID: ids.New(ids.Command), Kind: proto.CmdLaunch, SpaceID: e.space, RunID: e.run, Generation: 1}, p)
	r := getRun(t, a, e.run)
	if r.State != proto.RunRunning || r.Bin.Version != "2.1.328" {
		t.Fatalf("run %s (%s) version %q", r.State, r.Error, r.Bin.Version)
	}
	e.term = r.TerminalID
	waitFor(t, "recorder attached", func() bool { return f.Observed(e.term) > 0 })
	// Claude Code reports no status before its main screen (a startup dialog is drawn before it).
	f.SetStatusReport(e.term, runner.Status{})
	return e
}

// show puts a screen on the run's terminal and draws something, as a program does.
func (e *dialogEnv) show(screen string) {
	e.f.SetScreen(e.term, screen)
	e.f.Output(e.term, "\x1b[H")
}

func (e *dialogEnv) dialogs() []proto.RunDialog {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []proto.RunDialog
	for _, env := range e.got {
		if env.Type == proto.TypeRunDialog {
			var d proto.RunDialog
			if err := env.Decode(&d); err != nil {
				e.t.Fatal(err)
			}
			if !d.Valid() {
				e.t.Fatalf("invalid run.dialog %+v", d)
			}
			out = append(out, d)
		}
	}
	return out
}

func (e *dialogEnv) lastReport() proto.RunReport {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, env := range slices.Backward(e.got) {
		if env.Type == proto.TypeRunReport {
			var r proto.RunReport
			_ = env.Decode(&r)
			if r.RunID == e.run {
				return r
			}
		}
	}
	return proto.RunReport{}
}

// ackReason is the reason of the node's ack of a command ("" until it arrives).
func (e *dialogEnv) ackReason(cmd string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, env := range e.got {
		var a proto.Ack
		if env.Type == proto.TypeAck && env.Decode(&a) == nil && a.CommandID == cmd {
			return a.Result + ": " + a.Reason
		}
	}
	return ""
}

func (e *dialogEnv) outcomes() []string {
	var out []string
	for _, d := range e.dialogs() {
		out = append(out, d.Outcome+":"+d.Dialog)
	}
	return out
}

// quiet waits several quiet periods, so every read the watch would make has been made.
func quiet() { time.Sleep(6 * dialogQuiet) }

// The recorded startup dialogs are answered with their registry keys once, logged to central as answered (never as
// blocked), and the run is never flagged.
func TestDialogKnownStartupDialogsAnswered(t *testing.T) {
	for _, tc := range []struct {
		name, screen string
		edit         func(*proto.LaunchPayload)
		id, answer   string
		keys         []string
	}{
		{name: "code mode, not opted in", screen: "claude-2.1.328-code-mode.txt", id: "claude.code_mode_optin", answer: "no", keys: []string{"enter"}},
		{name: "code mode, opted in", screen: "claude-2.1.328-code-mode.txt", id: "claude.code_mode_optin", answer: "yes", keys: []string{"down", "enter"},
			edit: func(p *proto.LaunchPayload) {
				p.ClaudeSettings = proto.ClaudeSettings{"env": map[string]any{dialog.CodeModeEnv: "1"}}
			}},
		{name: "theme picker", screen: "claude-2.1.328-theme.txt", id: "claude.theme_picker", answer: "default", keys: []string{"enter"}},
		{name: "add-dir trust for the run's directory", screen: "add-dir", id: "claude.add_dir_trust", answer: "yes", keys: []string{"down", "enter"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newDialogEnv(t, tc.edit)
			screen := ""
			if tc.screen == "add-dir" {
				work := e.a.dialogContext(getRun(t, e.a, e.run)).Dirs[0]
				screen = strings.Replace(fixture(t, "claude-2.1.328-add-dir.txt"), "/srv/spaceman/memory/spc_example/worker/instances/ins_example", work, 1)
			} else {
				screen = fixture(t, tc.screen)
			}
			e.f.OnKeys = func(id string, _ []string) {
				e.f.SetScreen(id, fixture(t, "claude-2.1.328-main-idle.txt"))
				e.f.SetStatusReport(id, runner.Status{State: "idle", App: "claude-code"})
			}
			e.show(screen)
			waitFor(t, "answered", func() bool { return len(e.dialogs()) > 0 })
			quiet()
			if got := e.f.KeysPressed(e.term); len(got) != 1 || !slices.Equal(got[0], tc.keys) {
				t.Fatalf("keys %v, want once %v", got, tc.keys)
			}
			ds := e.dialogs()
			if len(ds) != 1 || ds[0].Outcome != proto.DialogAnswered || ds[0].Dialog != tc.id || ds[0].Answer != tc.answer ||
				ds[0].Signal != dialog.SignalKnown || ds[0].Excerpt != "" || ds[0].HarnessVersion != "2.1.328" {
				t.Fatalf("run.dialog %+v", ds)
			}
			if r := e.lastReport(); r.StatusKind == proto.StatusKindDialog {
				t.Fatalf("an answered dialog flagged the run: %+v", r)
			}
		})
	}
}

// An unknown dialog flags the run blocked_on_dialog (status blocked, kind dialog) and tells central once, with a masked
// excerpt; more output while it stays changes nothing; safe input is refused; when it is gone the flag clears.
func TestDialogUnknownFlagsOnce(t *testing.T) {
	e := newDialogEnv(t, nil)
	screen := strings.Replace(fixture(t, "claude-unknown.txt"), "A feature this version asks about before it starts.",
		"see sk-ant-api03-abcdefghijklmnop and s3cr3t-value-0123", 1)
	e.a.dlg.mu.Lock()
	e.a.dlg.watchLocked(e.run).secrets = append(e.a.dlg.watchLocked(e.run).secrets, "s3cr3t-value-0123")
	e.a.dlg.mu.Unlock()
	e.show(screen)
	waitFor(t, "flagged", func() bool { return len(e.dialogs()) > 0 })
	waitFor(t, "blocked report", func() bool {
		r := e.lastReport()
		return r.Status == "blocked" && r.StatusKind == proto.StatusKindDialog
	})
	for range 3 {
		e.f.Output(e.term, "\x1b[H")
		quiet()
	}
	if got := e.f.KeysPressed(e.term); len(got) != 0 {
		t.Fatalf("an unknown dialog was answered: %v", got)
	}
	ds := e.dialogs()
	if len(ds) != 1 || ds[0].Outcome != proto.DialogBlocked || ds[0].Dialog != dialog.UnknownID || ds[0].Signal != dialog.SignalPrompt {
		t.Fatalf("run.dialog %+v", ds)
	}
	if x := ds[0].Excerpt; !strings.Contains(x, "Something new (preview)") || strings.Contains(x, "sk-ant") || strings.Contains(x, "s3cr3t") ||
		!strings.Contains(x, "[redacted]") || strings.Contains(x, "Claude Code v9.9.9") {
		t.Fatalf("excerpt %q", x)
	}
	// Safe input would answer the dialog: refused. Forced input (an operator's) is not.
	cmdIn := ids.New(ids.Command)
	e.a.command(context.Background(), proto.Command{CommandID: cmdIn, IdempotencyKey: "in-1", Kind: proto.CmdInput, SpaceID: e.space, RunID: e.run, Generation: 1,
		Payload: mustJSONT(t, proto.InputPayload{Text: "you have 1 message"})})
	if in := e.f.Inputs(e.term); len(in) != 0 {
		t.Fatalf("safe input reached a dialog: %v", in)
	}
	waitFor(t, "input refused", func() bool { return strings.Contains(e.ackReason(cmdIn), "waits at a dialog") })
	// The dialog goes (an operator answered it): cleared, and the run reports its own status again.
	e.f.SetStatusReport(e.term, runner.Status{State: "idle", App: "claude-code"})
	e.show(fixture(t, "claude-2.1.328-main-idle.txt"))
	waitFor(t, "cleared", func() bool { return len(e.dialogs()) == 2 })
	waitFor(t, "unflagged report", func() bool { r := e.lastReport(); return r.Status == "idle" && r.StatusKind == "" })
	if ds := e.dialogs(); ds[1].Outcome != proto.DialogCleared || ds[1].ID != ds[0].ID || ds[1].Excerpt != "" {
		t.Fatalf("cleared %+v", ds[1])
	}
}

// Permission and tool-approval dialogs are never answered: one the program reports itself (OSC blocked, permission) is
// not even read, and one it does not report is flagged.
func TestDialogNeverAnswersPermission(t *testing.T) {
	e := newDialogEnv(t, nil)
	e.f.SetStatusReport(e.term, runner.Status{State: "blocked", Kind: "permission", App: "claude-code"})
	e.show(fixture(t, "claude-permission.txt"))
	quiet()
	if got, ds := e.f.KeysPressed(e.term), e.dialogs(); len(got) != 0 || len(ds) != 0 {
		t.Fatalf("a reported permission prompt: keys %v, dialogs %+v", got, ds)
	}
	e.f.SetStatusReport(e.term, runner.Status{})
	e.f.Output(e.term, "\x1b[H")
	waitFor(t, "flagged", func() bool { return len(e.dialogs()) > 0 })
	quiet()
	if got, ds := e.f.KeysPressed(e.term), e.dialogs(); len(got) != 0 || len(ds) != 1 || ds[0].Signal != dialog.SignalNever {
		t.Fatalf("an unreported permission prompt: keys %v, dialogs %+v", got, ds)
	}
}

// A known dialog that is still there after its answer is not answered again: it is flagged (answer_failed).
func TestDialogAnswerThatDoesNotTakeIsFlagged(t *testing.T) {
	e := newDialogEnv(t, nil)
	e.show(fixture(t, "claude-2.1.328-theme.txt"))
	waitFor(t, "flagged", func() bool { return len(e.dialogs()) == 2 })
	quiet()
	if got := e.f.KeysPressed(e.term); len(got) != 1 {
		t.Fatalf("keys %v, want one answer", got)
	}
	if got := e.outcomes(); !slices.Equal(got, []string{"answered:claude.theme_picker", "blocked:claude.theme_picker"}) {
		t.Fatalf("outcomes %v", got)
	}
	if ds := e.dialogs(); ds[1].Signal != dialog.SignalStuck {
		t.Fatalf("signal %q", ds[1].Signal)
	}
}

// Nothing is read or typed while an operator has the terminal taken over.
func TestDialogNotAnsweredDuringTakeover(t *testing.T) {
	e := newDialogEnv(t, nil)
	e.a.mu.Lock()
	e.a.takeovers[e.run] = &takeover{}
	e.a.mu.Unlock()
	t.Cleanup(func() { // registered last, so it runs before the agent stops (a real takeover has a stream to close)
		e.a.mu.Lock()
		delete(e.a.takeovers, e.run)
		e.a.mu.Unlock()
	})
	e.show(fixture(t, "claude-2.1.328-code-mode.txt"))
	quiet()
	if got, ds := e.f.KeysPressed(e.term), e.dialogs(); len(got) != 0 || len(ds) != 0 {
		t.Fatalf("during a takeover: keys %v, dialogs %+v", got, ds)
	}
}

func mustJSONT(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// An agent restart that finds the same dialog still open keeps its occurrence (central counts it once); one gone since
// the restart is cleared.
func TestDialogOccurrenceSurvivesAgentRestart(t *testing.T) {
	e := newDialogEnv(t, nil)
	e.show(fixture(t, "claude-unknown.txt"))
	waitFor(t, "flagged", func() bool { return len(e.dialogs()) == 1 })
	first := e.dialogs()[0]
	forget := func() { // what a new agent process knows of the watch: nothing
		e.a.dlg.mu.Lock()
		for _, w := range e.a.dlg.runs {
			if w.timer != nil {
				w.timer.Stop()
			}
		}
		e.a.dlg.runs = nil
		e.a.dlg.mu.Unlock()
	}
	forget()
	e.f.Output(e.term, "\x1b[H")
	waitFor(t, "flagged again", func() bool { return len(e.dialogs()) == 2 })
	if again := e.dialogs()[1]; again.ID != first.ID || again.Outcome != proto.DialogBlocked {
		t.Fatalf("after a restart: %+v, want the same occurrence %s", again, first.ID)
	}
	forget()
	e.show(fixture(t, "claude-2.1.328-main-idle.txt"))
	waitFor(t, "cleared", func() bool { return len(e.dialogs()) == 3 })
	if c := e.dialogs()[2]; c.ID != first.ID || c.Outcome != proto.DialogCleared {
		t.Fatalf("cleared %+v", c)
	}
	quiet()
	if n := len(e.dialogs()); n != 3 {
		t.Fatalf("%d messages, want nothing more", n)
	}
}

// The excerpt masks the run's secret values and token-shaped strings, keeps the dialog's last lines and stays within
// proto's bounds on a rune boundary.
func TestDialogExcerptRedaction(t *testing.T) {
	lines := []string{
		" Paste your key: sk-ant-api03-AbCdEfGhIjKlMnOp",
		" gh token ghp_0123456789abcdefABCDEF0123",
		" slack xoxb-1234-5678-abcdefgh",
		" jwt " + strings.Join([]string{"ey" + "JhbGciOiJIUzI1NiJ9", "ey" + "JzdWIiOiIxMjM0NTY3ODkwIn0", "c2lnbmF0dXJl" + "LXZhbHVl"}, "."),
		" long " + strings.Repeat("A1b2", 12),
		" the run's own value my-run-secret-value here",
		" ❯ 1. Yes",
	}
	x := excerpt(lines, []string{"my-run-secret-value"})
	for _, leak := range []string{"sk-ant", "ghp_", "xoxb-", "JhbGci", "A1b2A1b2", "my-run-secret-value"} {
		if strings.Contains(x, leak) {
			t.Fatalf("excerpt leaks %q:\n%s", leak, x)
		}
	}
	if !strings.Contains(x, "❯ 1. Yes") || strings.Count(x, "[redacted]") != 6 {
		t.Fatalf("excerpt:\n%s", x)
	}
	var many []string
	for i := range 50 {
		many = append(many, fmt.Sprintf(" line %02d %s", i, strings.Repeat("é", 60)))
	}
	x = excerpt(many, nil)
	if len(x) > proto.DialogExcerptMax || !utf8.ValidString(x) || strings.Contains(x, "line 29") || !strings.HasPrefix(x, " line 30") {
		t.Fatalf("bounds: %d bytes, valid %v, starts %q", len(x), utf8.ValidString(x), x[:12])
	}
}

// screenWithCursorOn moves a fixture's selection cursor to the option starting with option.
func screenWithCursorOn(t *testing.T, name, from, to string) string {
	t.Helper()
	s := fixture(t, name)
	s = strings.Replace(s, "❯ "+from, "  "+from, 1)
	s = strings.Replace(s, "  "+to, "❯ "+to, 1)
	if !strings.Contains(s, "❯ "+to) {
		t.Fatalf("no option %q in %s", to, name)
	}
	return s
}

// Dialog text in the transcript, above the program's idle prompt, gets no keys and no flag (it is not a dialog), and a
// known dialog the screen does show while the program reports a status is flagged, not answered: startup dialogs come
// before any status.
func TestDialogNotAnsweredAtTheIdlePrompt(t *testing.T) {
	codeModeYes := screenWithCursorOn(t, "claude-2.1.328-code-mode.txt", "No, start without it", "Yes, turn on Code Mode")
	t.Run("transcript above the idle prompt", func(t *testing.T) {
		e := newDialogEnv(t, nil)
		e.f.SetStatusReport(e.term, runner.Status{State: "idle", App: "claude-code"})
		idle := fixture(t, "claude-2.1.328-main-idle.txt")
		at := strings.Index(idle, "\n─")
		e.show(idle[:at+1] + "⏺ The dialog read:\n\n" + codeModeYes + "\n" + idle[at+1:])
		quiet()
		if got, ds := e.f.KeysPressed(e.term), e.dialogs(); len(got) != 0 || len(ds) != 0 {
			t.Fatalf("keys %v, dialogs %+v", got, ds)
		}
		if r := e.lastReport(); r.StatusKind == proto.StatusKindDialog {
			t.Fatalf("flagged: %+v", r)
		}
	})
	t.Run("known dialog while the program reports a status", func(t *testing.T) {
		e := newDialogEnv(t, nil)
		e.f.SetStatusReport(e.term, runner.Status{State: "idle", App: "claude-code"})
		e.show(fixture(t, "claude-2.1.328-code-mode.txt"))
		waitFor(t, "flagged", func() bool { return len(e.dialogs()) > 0 })
		quiet()
		if got := e.f.KeysPressed(e.term); len(got) != 0 {
			t.Fatalf("answered at a reported status: %v", got)
		}
		if ds := e.dialogs(); len(ds) != 1 || ds[0].Outcome != proto.DialogBlocked || ds[0].Dialog != "claude.code_mode_optin" ||
			ds[0].Signal != dialog.SignalWithheld {
			t.Fatalf("run.dialog %+v", ds)
		}
	})
}

// A flagged run's screen is read again even when it stays quiet: a flag whose dialog went away without output ends,
// and the re-reads back off up to dialogRecheckMax.
func TestDialogFlagRecheckedOnAQuietScreen(t *testing.T) {
	old := dialogRecheckMax
	dialogRecheckMax = 200 * time.Millisecond
	t.Cleanup(func() { dialogRecheckMax = old })
	e := newDialogEnv(t, nil)
	e.show(fixture(t, "claude-unknown.txt"))
	waitFor(t, "flagged", func() bool { return len(e.dialogs()) == 1 })
	waitFor(t, "backoff at its bound", func() bool {
		e.a.dlg.mu.Lock()
		defer e.a.dlg.mu.Unlock()
		w := e.a.dlg.runs[e.run]
		return w != nil && w.backoff == dialogRecheckMax
	})
	e.f.SetScreen(e.term, fixture(t, "claude-2.1.328-main-idle.txt")) // no output: only a re-read sees it
	waitFor(t, "cleared", func() bool { return len(e.dialogs()) == 2 })
	if got := e.outcomes(); !slices.Equal(got, []string{"blocked:unknown", "cleared:unknown"}) {
		t.Fatalf("outcomes %v", got)
	}
	e.a.dlg.mu.Lock()
	b := e.a.dlg.runs[e.run].backoff
	e.a.dlg.mu.Unlock()
	if b != 0 {
		t.Fatalf("backoff %v after the flag cleared", b)
	}
}

// After an agent restart the run's secret values are unknown (they are never written down for the watch): a dialog
// flagged then is sent without an excerpt, rather than with values the token patterns miss.
func TestDialogNoExcerptAfterRestart(t *testing.T) {
	token := fmt.Sprintf("runtok-%016x", time.Now().UnixNano()) // short and prefix-less: no token pattern masks it
	e := newDialogEnv(t, func(p *proto.LaunchPayload) { p.RunToken = token })
	shows := strings.Replace(fixture(t, "claude-unknown.txt"), "A feature this version asks about before it starts.", "token "+token, 1)
	e.show(shows)
	waitFor(t, "flagged", func() bool { return len(e.dialogs()) == 1 })
	if x := e.dialogs()[0].Excerpt; x == "" || strings.Contains(x, token) || !strings.Contains(x, "[redacted]") {
		t.Fatalf("before the restart: excerpt %q", x)
	}
	e.show(fixture(t, "claude-2.1.328-main-idle.txt"))
	waitFor(t, "cleared", func() bool { return len(e.dialogs()) == 2 })
	e.a.dlg.mu.Lock() // what a new agent process knows of the watch: nothing
	for _, w := range e.a.dlg.runs {
		if w.timer != nil {
			w.timer.Stop()
		}
	}
	e.a.dlg.runs = nil
	e.a.dlg.mu.Unlock()
	e.show(shows)
	waitFor(t, "flagged again", func() bool { return len(e.dialogs()) == 3 })
	if d := e.dialogs()[2]; d.Outcome != proto.DialogBlocked || d.Excerpt != "" {
		t.Fatalf("after the restart: %+v", d)
	}
}

// takeoverEnv sets an operator's takeover on the run (removed before the agent stops).
func (e *dialogEnv) takeover() {
	e.a.mu.Lock()
	e.a.takeovers[e.run] = &takeover{}
	e.a.mu.Unlock()
}

// Each takeover check holds on its own: an unknown dialog is not even flagged during a takeover (the check before the
// read), and a takeover that starts while the screen is read stops the answer (the check before the keys).
func TestDialogTakeoverChecks(t *testing.T) {
	t.Run("before the read", func(t *testing.T) {
		e := newDialogEnv(t, nil)
		e.takeover()
		t.Cleanup(func() { e.a.mu.Lock(); delete(e.a.takeovers, e.run); e.a.mu.Unlock() })
		e.show(fixture(t, "claude-unknown.txt"))
		quiet()
		if ds := e.dialogs(); len(ds) != 0 {
			t.Fatalf("flagged during a takeover: %+v", ds)
		}
	})
	t.Run("during the read", func(t *testing.T) {
		e := newDialogEnv(t, nil)
		e.f.OnReadScreen = func(string) { e.takeover() }
		t.Cleanup(func() { e.a.mu.Lock(); delete(e.a.takeovers, e.run); e.a.mu.Unlock() })
		e.show(fixture(t, "claude-2.1.328-code-mode.txt"))
		quiet()
		if got, ds := e.f.KeysPressed(e.term), e.dialogs(); len(got) != 0 || len(ds) != 0 {
			t.Fatalf("answered after a takeover began: keys %v, dialogs %+v", got, ds)
		}
	})
}

// A provisioned directory that is a symlink is accepted at its resolved path too (the program shows the real path).
func TestDialogAddDirResolvedSymlink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	link := filepath.Join(dir, "link")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	a := &Agent{o: Options{DataDir: dir}}
	run := ids.New(ids.Run)
	if err := os.MkdirAll(a.runDir(run), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := a.saveDialogLaunch(run, "claude-code", "2.1.328", []string{link}, harness.Launch{}, nil); err != nil {
		t.Fatal(err)
	}
	c := a.dialogContext(runRow{RunID: run, Harness: "claude-code"})
	resolved, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	s := strings.Replace(fixture(t, "claude-2.1.328-add-dir.txt"), "/srv/spaceman/memory/spc_example/worker/instances/ins_example", resolved, 1)
	if r := dialog.Classify(s, c); r.Kind != dialog.Known || r.Answer != "yes" {
		t.Fatalf("resolved path %s with dirs %v: %+v", resolved, c.Dirs, r)
	}
}

// endRun marks the run as exited in the journal, as an observation of its exit does.
func (e *dialogEnv) endRun() {
	e.t.Helper()
	row := getRun(e.t, e.a, e.run)
	row.State = proto.RunExited
	if err := e.a.j.updateRun(context.Background(), row); err != nil {
		e.t.Fatal(err)
	}
}

// watching reports whether the agent keeps a watch for the run.
func (e *dialogEnv) watching() bool {
	e.a.dlg.mu.Lock()
	defer e.a.dlg.mu.Unlock()
	return e.a.dlg.runs[e.run] != nil
}

// A run that ends keeps no watch and no timer: not when it ends while its screen is read (the flag must not recreate
// the watch that the end dropped, and the backoff must not re-arm it), and not when the watch survives until a check
// finds the run no longer running.
func TestDialogEndedRunKeepsNoWatch(t *testing.T) {
	old := dialogRecheckMax
	dialogRecheckMax = 100 * time.Millisecond
	t.Cleanup(func() { dialogRecheckMax = old })
	t.Run("ends during the read", func(t *testing.T) {
		e := newDialogEnv(t, nil)
		var once sync.Once
		var read atomic.Bool
		e.f.OnReadScreen = func(string) {
			once.Do(func() {
				e.endRun()
				e.a.dialogForget(e.run) // what the end of a run does (runEndHooks)
				read.Store(true)
			})
		}
		e.show(fixture(t, "claude-unknown.txt"))
		waitFor(t, "read", read.Load)
		quiet()
		if e.watching() {
			t.Fatal("a watch was recreated for the run that ended")
		}
		if ds := e.dialogs(); len(ds) != 0 {
			t.Fatalf("an ended run was flagged: %+v", ds)
		}
	})
	t.Run("found ended by a check", func(t *testing.T) {
		e := newDialogEnv(t, nil)
		e.endRun() // no hook dropped the watch
		e.a.dlg.mu.Lock()
		e.a.dialogArmLocked(context.Background(), e.run, e.a.dlg.watchLocked(e.run), time.Hour)
		e.a.dlg.mu.Unlock()
		e.a.dialogCheck(context.Background(), e.run)
		if e.watching() {
			t.Fatal("the watch of an ended run was kept")
		}
	})
}

// The two reasons a known dialog is not answered again are separate: SpaceMan answered it and it is still there (checked
// by TestDialogAnswerThatDoesNotTakeIsFlagged), and it is already flagged, here as withheld while the program reported a
// status. When that status goes away and the same dialog is read again, it is not answered: the flag stands.
func TestDialogFlaggedKnownDialogNotAnsweredLater(t *testing.T) {
	e := newDialogEnv(t, nil)
	e.f.SetStatusReport(e.term, runner.Status{State: "idle", App: "claude-code"})
	e.show(fixture(t, "claude-2.1.328-code-mode.txt"))
	waitFor(t, "flagged", func() bool { return len(e.dialogs()) == 1 })
	// Only the in-memory flag is left (the saved occurrence would also make it "again").
	if err := os.Remove(filepath.Join(e.a.runDir(e.run), dialogOpenFile)); err != nil {
		t.Fatal(err)
	}
	e.f.SetStatusReport(e.term, runner.Status{})
	quiet()
	e.show(fixture(t, "claude-2.1.328-code-mode.txt"))
	quiet()
	if got := e.f.KeysPressed(e.term); len(got) != 0 {
		t.Fatalf("a flagged dialog was answered: %v", got)
	}
	if got := e.outcomes(); !slices.Equal(got, []string{"blocked:claude.code_mode_optin"}) {
		t.Fatalf("outcomes %v", got)
	}
}
