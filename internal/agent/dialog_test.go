package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/dialog"
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
