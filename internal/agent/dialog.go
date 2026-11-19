package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/aurimasniekis/go-spaceman/internal/dialog"
	"github.com/aurimasniekis/go-spaceman/internal/harness"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/obs"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/runner"
)

// Terminal dialog watch (ADR-0102). A program can stop at an interactive dialog before its first turn or between turns
// (a new Claude Code opt-in, a directory-trust question). Its own status report then says nothing useful (Claude Code
// reports no status at all before its main screen), so the run would look idle or starting forever.
//
// The watch is driven by the run's output, never by a poll: the recorder calls dialogOutput for every chunk, which
// (re)arms a per-run quiet timer. When the output has been quiet for dialogQuiet and the program does not report
// working, the screen is read once (runner.Screen) and classified (internal/dialog):
//   - a known dialog with a safe answer is answered with named keys (terminal.keys: never free text), once; if it is
//     still there at the next quiet read, it is flagged instead (answer_failed);
//   - an unknown dialog, one on the never-answer list, or a known one whose answer is not safe for this run is flagged:
//     the run reports status blocked with kind dialog (blocked_on_dialog) and central is told once per occurrence,
//     with a masked excerpt, so control and the operators can answer it from the takeover;
//   - a screen without a dialog clears a flag.
//
// A program that itself reports blocked with a kind (OSC 7501 permission, question, auth) is never read or answered: it
// is already shown as blocked, and permission prompts belong to the approval path. Nothing is read or typed while an
// operator has the terminal taken over. The screen text never makes a run look idle or done.

// dialogQuiet is how long a run's output must be quiet before its screen is read. A variable so tests can shorten it.
var dialogQuiet = 10 * time.Second

// dialogPendingMax bounds the run.dialog messages kept while central is unreachable (oldest dropped first; an open
// blocked occurrence is sent again from the watch itself on every welcome).
const dialogPendingMax = 64

// dialogContextFile holds what a run's dialog answers may depend on, written at launch (it survives an agent restart).
const dialogContextFile = "dialog.json"

// dialogLaunch is the launch facts saved in dialogContextFile.
type dialogLaunch struct {
	Harness  string   `json:"harness"`
	Version  string   `json:"version,omitempty"`
	Dirs     []string `json:"dirs,omitempty"`
	Home     string   `json:"home,omitempty"`
	CodeMode bool     `json:"code_mode,omitempty"`
}

// dialogWatch is one run's watch state.
type dialogWatch struct {
	timer *time.Timer
	// answered is the registry dialog SpaceMan answered and has not seen go away yet: seen again, it is flagged.
	answered string
	// blocked is the open flagged occurrence (nil: none).
	blocked *proto.RunDialog
	// secrets are values masked in excerpts (the run's secret env and token), known only to the process that launched it.
	secrets []string
}

// dialogs is the agent's watch state; the zero value is ready to use.
type dialogs struct {
	mu      sync.Mutex
	runs    map[string]*dialogWatch
	pending []proto.RunDialog
}

// watchLocked returns a run's watch, creating it (d.mu held).
func (d *dialogs) watchLocked(runID string) *dialogWatch {
	if d.runs == nil {
		d.runs = map[string]*dialogWatch{}
	}
	w := d.runs[runID]
	if w == nil {
		w = &dialogWatch{}
		d.runs[runID] = w
	}
	return w
}

// saveDialogLaunch records the launch facts a run's dialog answers depend on, and the secret values its excerpts mask.
func (a *Agent) saveDialogLaunch(runID, kind, version string, dirs []string, built harness.Launch, secrets []string) error {
	l := dialogLaunch{Harness: kind, Version: version, Home: built.Env["HOME"]}
	if l.Home == "" {
		l.Home, _ = os.UserHomeDir()
	}
	for _, d := range dirs {
		if d == "" {
			continue
		}
		l.Dirs = append(l.Dirs, filepath.Clean(d))
		if real, err := filepath.EvalSymlinks(d); err == nil && real != filepath.Clean(d) {
			l.Dirs = append(l.Dirs, real)
		}
	}
	if kind == harness.KindClaude {
		for _, own := range built.OwnJSON {
			if env, ok := own["env"].(map[string]any); ok {
				if _, set := env[dialog.CodeModeEnv]; set {
					l.CodeMode = true
				}
			}
		}
	}
	var keep []string
	for _, s := range secrets {
		if len(s) >= 8 {
			keep = append(keep, s)
		}
	}
	a.dlg.mu.Lock()
	a.dlg.watchLocked(runID).secrets = keep
	a.dlg.mu.Unlock()
	return writeJSONFile(filepath.Join(a.runDir(runID), dialogContextFile), l)
}

// dialogContext is the classification context of a run (an unreadable file gives one that matches no registry entry
// whose answer depends on it: no provisioned directories, Code Mode off).
func (a *Agent) dialogContext(row runRow) dialog.Context {
	var l dialogLaunch
	if b, err := os.ReadFile(filepath.Join(a.runDir(row.RunID), dialogContextFile)); err == nil { //nolint:gosec // a fixed name in the run's own directory
		_ = json.Unmarshal(b, &l)
	}
	c := dialog.Context{Harness: row.Harness, Version: l.Version, Dirs: l.Dirs, Home: l.Home, CodeMode: l.CodeMode}
	if c.Version == "" {
		c.Version = row.Bin.Version
	}
	return c
}

// dialogOutput notes output of a run: the screen is read once the output has been quiet for dialogQuiet. Called for every
// output chunk the recorder reads, so it only re-arms a timer.
func (a *Agent) dialogOutput(ctx context.Context, runID string) {
	if _, ok := a.o.Runner.(runner.Screen); !ok {
		return
	}
	a.dlg.mu.Lock()
	defer a.dlg.mu.Unlock()
	w := a.dlg.watchLocked(runID)
	if w.timer != nil {
		w.timer.Reset(dialogQuiet)
		return
	}
	w.timer = time.AfterFunc(dialogQuiet, func() {
		if ctx.Err() == nil {
			a.track(func() { a.dialogCheck(ctx, runID) })
		}
	})
}

// dialogRecheck reads the screen again after dialogQuiet even without new output (after an answer).
func (a *Agent) dialogRecheck(runID string) {
	a.dlg.mu.Lock()
	defer a.dlg.mu.Unlock()
	if w := a.dlg.runs[runID]; w != nil && w.timer != nil {
		w.timer.Reset(dialogQuiet)
	}
}

// dialogForget drops a run's watch when it ended.
func (a *Agent) dialogForget(runID string) {
	a.dlg.mu.Lock()
	defer a.dlg.mu.Unlock()
	if w := a.dlg.runs[runID]; w != nil {
		if w.timer != nil {
			w.timer.Stop()
		}
		delete(a.dlg.runs, runID)
	}
}

// dialogBlocked reports whether a run is flagged as waiting at a dialog.
func (a *Agent) dialogBlocked(runID string) bool {
	a.dlg.mu.Lock()
	defer a.dlg.mu.Unlock()
	w := a.dlg.runs[runID]
	return w != nil && w.blocked != nil
}

// dialogCheck reads a run's screen after its output went quiet and acts on what it shows.
func (a *Agent) dialogCheck(ctx context.Context, runID string) {
	scr, ok := a.o.Runner.(runner.Screen)
	if !ok || ctx.Err() != nil {
		return
	}
	// One check at a time per run (the run's lock is not taken: a launch holds it while it observes).
	unlock := a.runLock("dialog/" + runID)
	defer unlock()
	log := a.log.With(slog.String(obs.FieldRunID, runID))
	row, err := a.j.getRun(ctx, runID)
	if err != nil || row.State != proto.RunRunning || row.TerminalID == "" || row.Desired == "stopped" {
		return
	}
	p, err := a.o.Runner.Get(ctx, row.TerminalID)
	if err != nil || p.State != runner.StateLive {
		return
	}
	switch {
	case p.Status.State == "blocked" && p.Status.Kind != "":
		// The program reports its own permission, question or auth prompt: shown as blocked already, never answered.
		return
	case p.Status.State == "working" && p.Status.Kind != runner.KindBackground:
		a.dialogClear(ctx, row)
		return
	}
	if a.takenOver(runID) {
		return // an operator is at the terminal: nothing is read or typed for them
	}
	text, err := scr.ReadScreen(ctx, row.TerminalID)
	if err != nil {
		log.Debug("dialog watch: read the screen", obs.Err(err))
		return
	}
	res := dialog.Classify(text, a.dialogContext(row))
	switch res.Kind {
	case dialog.None:
		a.dialogClear(ctx, row)
	case dialog.Known:
		a.dlg.mu.Lock()
		w := a.dlg.watchLocked(runID)
		again := w.answered == res.ID
		a.dlg.mu.Unlock()
		if again {
			// Answered and still there: SpaceMan does not try again, a person looks at it.
			res.Kind, res.Signal = dialog.Unknown, dialog.SignalStuck
			a.dialogFlag(ctx, row, res)
			return
		}
		a.dialogClear(ctx, row)
		if a.takenOver(runID) { // an operator took the terminal over while the screen was read
			return
		}
		if err := scr.Keys(ctx, row.TerminalID, res.Keys); err != nil {
			log.Warn("dialog watch: answer a known dialog", slog.String("dialog", res.ID), obs.Err(err))
			return
		}
		a.dlg.mu.Lock()
		a.dlg.watchLocked(runID).answered = res.ID
		a.dlg.mu.Unlock()
		log.Info("dialog watch: answered a known dialog", slog.String("dialog", res.ID), slog.String("answer", res.Answer))
		a.dialogSend(ctx, proto.RunDialog{ID: ids.New(ids.Dialog), RunID: runID, SpaceID: row.SpaceID, Generation: row.Generation,
			Outcome: proto.DialogAnswered, Dialog: res.ID, Signal: res.Signal, Answer: res.Answer, HarnessVersion: row.Bin.Version, DetectedAt: a.now().UTC()})
		a.dialogRecheck(runID) // look again even if the answer draws nothing
	case dialog.Unknown:
		a.dialogFlag(ctx, row, res)
	}
}

// dialogFlag marks a run as waiting at a dialog SpaceMan does not answer: one occurrence per dialog until it clears.
func (a *Agent) dialogFlag(ctx context.Context, row runRow, res dialog.Result) {
	a.dlg.mu.Lock()
	w := a.dlg.watchLocked(row.RunID)
	if w.blocked != nil && w.blocked.Dialog == res.ID {
		a.dlg.mu.Unlock()
		return // the same dialog, still open: told once
	}
	prev := w.blocked
	d := proto.RunDialog{ID: ids.New(ids.Dialog), RunID: row.RunID, SpaceID: row.SpaceID, Generation: row.Generation, Outcome: proto.DialogBlocked,
		Dialog: res.ID, Signal: res.Signal, Excerpt: excerpt(res.Lines, w.secrets), HarnessVersion: row.Bin.Version, DetectedAt: a.now().UTC()}
	w.blocked, w.answered = &d, ""
	a.dlg.mu.Unlock()
	if prev != nil {
		c := *prev
		c.Outcome, c.Excerpt, c.DetectedAt = proto.DialogCleared, "", a.now().UTC()
		a.dialogSend(ctx, c)
	}
	a.log.Warn("dialog watch: the run waits at a dialog SpaceMan does not answer", slog.String(obs.FieldRunID, row.RunID),
		slog.String("dialog", res.ID), slog.String("signal", res.Signal))
	a.report(ctx, row)
	a.dialogSend(ctx, d)
}

// dialogClear ends a run's flag (its dialog is gone, or the program works again).
func (a *Agent) dialogClear(ctx context.Context, row runRow) {
	a.dlg.mu.Lock()
	w := a.dlg.runs[row.RunID]
	if w == nil || w.blocked == nil && w.answered == "" {
		a.dlg.mu.Unlock()
		return
	}
	prev := w.blocked
	w.blocked, w.answered = nil, ""
	a.dlg.mu.Unlock()
	if prev == nil {
		return
	}
	if cur, err := a.j.getRun(ctx, row.RunID); err == nil {
		row = cur
	}
	a.report(ctx, row)
	c := *prev
	c.Outcome, c.Excerpt, c.DetectedAt = proto.DialogCleared, "", a.now().UTC()
	a.dialogSend(ctx, c)
}

// dialogObserved runs on every observation of a run: a program that works again (or finished a turn) has left its
// dialog, so the flag ends without waiting for the next quiet read.
func (a *Agent) dialogObserved(ctx context.Context, prev, row runRow) {
	if !a.dialogBlocked(row.RunID) {
		return
	}
	if row.Status == "working" && row.StatusKind != runner.KindBackground || row.Turn != prev.Turn && prev.RunID != "" || ended(row.State) {
		a.dialogClear(ctx, row)
	}
}

// withDialog gives a report the dialog flag: status blocked, kind dialog (blocked_on_dialog).
func (a *Agent) withDialog(r runRow) runRow {
	if !ended(r.State) && a.dialogBlocked(r.RunID) {
		r.Status, r.StatusKind = "blocked", proto.StatusKindDialog
	}
	return r
}

// dialogSend sends a run.dialog, or keeps it for the next welcome.
func (a *Agent) dialogSend(ctx context.Context, d proto.RunDialog) {
	if a.protocolAtLeast(proto.MinorDialog) && a.send(ctx, proto.TypeRunDialog, d) == nil {
		return
	}
	a.dlg.mu.Lock()
	defer a.dlg.mu.Unlock()
	a.dlg.pending = append(a.dlg.pending, d)
	if n := len(a.dlg.pending) - dialogPendingMax; n > 0 {
		a.dlg.pending = slices.Delete(a.dlg.pending, 0, n)
	}
}

// resendDialogs sends, after a welcome, what could not be sent and every open blocked occurrence (central records each
// occurrence once, so a repeat changes nothing).
func (a *Agent) resendDialogs(ctx context.Context) {
	if !a.protocolAtLeast(proto.MinorDialog) {
		return
	}
	a.dlg.mu.Lock()
	out := a.dlg.pending
	a.dlg.pending = nil
	sent := map[string]bool{}
	for _, d := range out {
		if d.Outcome == proto.DialogBlocked {
			sent[d.ID] = true
		}
	}
	for _, w := range a.dlg.runs {
		if w.blocked != nil && !sent[w.blocked.ID] {
			out = append(out, *w.blocked)
		}
	}
	a.dlg.mu.Unlock()
	for i, d := range out {
		if err := a.send(ctx, proto.TypeRunDialog, d); err != nil {
			a.dlg.mu.Lock()
			a.dlg.pending = append(slices.Clone(out[i:]), a.dlg.pending...)
			a.dlg.mu.Unlock()
			return
		}
	}
}

// reToken matches token-shaped strings masked in excerpts besides the run's own secrets: provider key prefixes, JWTs and
// long runs of key characters.
var reToken = regexp.MustCompile(`(?i)\b(sk-[a-z0-9_-]{8,}|gh[opusr]_[a-z0-9]{16,}|github_pat_[a-z0-9_]{16,}|xox[abpr]-[a-z0-9-]{8,}|eyj[a-z0-9_-]{8,}\.[a-z0-9_-]{8,}\.[a-z0-9_-]{8,}|[a-z0-9+/_=-]{40,})`)

// excerpt is a dialog's last lines with secrets masked, within proto's bounds.
func excerpt(lines []string, secrets []string) string {
	if n := len(lines) - proto.DialogExcerptLines; n > 0 {
		lines = lines[n:]
	}
	s := strings.Join(lines, "\n")
	for _, v := range secrets {
		s = strings.ReplaceAll(s, v, "[redacted]")
	}
	s = reToken.ReplaceAllString(s, "[redacted]")
	if len(s) > proto.DialogExcerptMax {
		cut := proto.DialogExcerptMax
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut]
	}
	return s
}
