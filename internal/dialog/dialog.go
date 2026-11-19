// Package dialog recognises interactive dialogs on a run's terminal screen (ADR-0102). It is pure: it reads a screen as
// herdr-core's terminal.read returns it and says whether the program waits at a dialog, which one, and, for a dialog in
// the registry whose answer is safe, the named keys that answer it. The node agent decides when to look and sends the
// keys; nothing here reads a terminal or types.
//
// Order of the checks, and why:
//  1. Find the dialog: a selection cursor in front of an option, with a confirm footer or a numbered option list. The
//     plain prompt of a waiting program ("❯ " with or without typed text) is not a dialog.
//  2. The never-answer list, over the dialog's own lines: permission and tool approval, login and credentials. Such a
//     dialog is flagged, never answered, whatever the registry says.
//  3. The registry: an entry for the harness and version whose stable text is all on the dialog's lines. Its answer
//     may still decline (a directory SpaceMan did not provision), and then the dialog is flagged.
//  4. Anything else that looks like a dialog is unknown: flagged, never guessed.
package dialog

import (
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/aurimasniekis/go-spaceman/internal/harness/dist"
)

// Signals: what made the agent decide (recorded with every event, ADR-0102).
const (
	// SignalKnown: the screen matched a registry entry's stable text.
	SignalKnown = "screen_known"
	// SignalPrompt: the screen showed a selection prompt the registry does not know (heuristic).
	SignalPrompt = "screen_prompt"
	// SignalNever: the screen showed a dialog on the never-answer list.
	SignalNever = "never_list"
	// SignalDeclined: a registry entry matched, but its answer was not safe for this run (an unprovisioned directory).
	SignalDeclined = "answer_declined"
	// SignalStuck: SpaceMan answered the dialog and it was still there afterwards.
	SignalStuck = "answer_failed"
)

// UnknownID is the dialog ID of a dialog that is not in the registry (or that SpaceMan does not answer).
const UnknownID = "unknown"

// Kind is the outcome of a classification.
type Kind int

// Kinds.
const (
	// None: the screen shows no dialog.
	None Kind = iota
	// Known: a registry dialog with a safe answer; Result.Keys answers it.
	Known
	// Unknown: a dialog SpaceMan must not answer; the run is flagged blocked_on_dialog.
	Unknown
)

// Context is what the answer of a registry dialog may depend on, taken from the run's launch.
type Context struct {
	// Harness is the run's harness kind ("claude-code", "codex").
	Harness string
	// Version is the harness version the run launched ("" when unknown: entries with a minimum version then do not match).
	Version string
	// Dirs are the directories SpaceMan provisioned for the run (working directory, memory folders): the only ones a
	// directory-trust dialog may be accepted for. Compared after filepath.Clean.
	Dirs []string
	// Home expands a leading "~/" in a path a dialog shows ("" leaves it unexpanded, so it matches nothing).
	Home string
	// CodeMode says the run's profile turned Claude Code's Code Mode on (its settings env sets the variable).
	CodeMode bool
}

// Result is a classification.
type Result struct {
	Kind Kind
	// ID is the registry entry's ID for Known, and the entry's ID or UnknownID for Unknown.
	ID string
	// Signal is one of the Signal constants ("" for None).
	Signal string
	// Keys are herdr-core terminal.keys names that answer a Known dialog.
	Keys []string
	// Answer is the option chosen for a Known dialog, as a fixed word of the entry (never screen text).
	Answer string
	// Lines are the dialog's own lines (trimmed of trailing space), for an excerpt.
	Lines []string
}

// Answer is a registry entry's decision for one screen.
type Answer struct {
	// Choice is the fixed word recorded as the answer ("yes", "no", "default").
	Choice string
	// Keys answer the dialog.
	Keys []string
}

// Entry is one registry dialog.
type Entry struct {
	// ID is stable and recorded with every event ("claude.code_mode_optin").
	ID string
	// Harness is the harness kind the dialog belongs to.
	Harness string
	// MinVersion is the oldest version the dialog was seen or read in ("" for any); a newer version with the same
	// stable text is taken to be the same dialog. MaxVersion ("" for none) closes the range when a version changes it.
	MinVersion, MaxVersion string
	// Text are lines that must all appear on the dialog's lines (substring match, after collapsing spaces).
	Text []string
	// Answer decides the keys for this screen; ok false declines (the dialog is then flagged).
	Answer func(d Dialog, c Context) (a Answer, ok bool)
}

// Dialog is a recognised dialog on a screen: its lines, the option lines and where the cursor is.
type Dialog struct {
	// Lines are the dialog's lines, from its first line (below the last rule above the options) to the screen's last
	// non-empty line.
	Lines []string
	// Options are the option lines' texts (cursor and numbering removed), top to bottom.
	Options []string
	// Cursor is the index in Options of the option the cursor is on.
	Cursor int
	// Weak is set when the dialog has neither a confirm footer nor a numbered option list, only a cursor in a block of
	// at least two options (Claude Code's theme picker): it counts only when a registry entry matches it, and is never
	// flagged as unknown on its own (a prompt with several lines of typed text looks the same).
	Weak bool
}

var (
	// reCursor is an option line with the selection cursor: Claude Code draws "❯", Codex "›". Text must follow, and
	// the text must not be the cursor of a plain prompt.
	reCursor = regexp.MustCompile(`^\s*[❯›]\s+(\S.*)$`)
	// reNumbered is a numbered option ("1. Yes", "❯ 2. No").
	reNumbered = regexp.MustCompile(`^\s*(?:[❯›]\s+)?(\d{1,2})\.\s+\S`)
	// reFooter is a confirm footer of a selection dialog.
	reFooter = regexp.MustCompile(`(?i)(enter to (confirm|select|continue|submit)|esc to (cancel|exit|go back|start without|skip)|\(y/n\)|\[y/n\]|↑/↓ to (select|navigate))`)
	// reRule is a horizontal rule line (the top of a Claude Code dialog box).
	reRule = regexp.MustCompile(`^\s*[─━╌—-]{20,}\s*$`)
	// reCheck is the "current value" mark Claude Code draws in front of an option ("❯ ✔ Auto").
	reCheck   = regexp.MustCompile(`^[✔✓]\s+`)
	reNumber  = regexp.MustCompile(`^\d{1,2}\.\s+`)
	reSpaces  = regexp.MustCompile(`\s+`)
	reNeverRe = regexp.MustCompile(`(?i)(do you want to (proceed|make this edit|create|allow|run|fetch|use)|don't ask again|permission|approv|allow (once|always)|\bdeny\b|bash command|tool use|login|log in|sign in|api key|password|token|credential|oauth|authenticat|trust this (mcp|server|hook))`)
)

// Never reports whether a dialog's lines are on the never-answer list: permission and tool-approval prompts, login,
// credentials and anything that grants a program more than SpaceMan provisioned. Such a dialog is never answered.
func Never(lines []string) bool {
	return reNeverRe.MatchString(strings.Join(lines, "\n"))
}

// Find returns the dialog on a screen, if there is one: an option line with the selection cursor, and either a
// confirm footer below it or at least two numbered options (or, Weak, a block of at least two options). A program's
// plain prompt ("❯ " with no text after it) is not a dialog.
func Find(screen string) (Dialog, bool) {
	lines := strings.Split(strings.ReplaceAll(screen, "\r", ""), "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " \t")
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	cur := -1
	for i := len(lines) - 1; i >= 0; i-- {
		if reCursor.MatchString(lines[i]) {
			cur = i
			break
		}
	}
	if cur < 0 {
		return Dialog{}, false
	}
	// The option block: the contiguous indented lines around the cursor line (no blank line between them).
	col := textColumn(lines[cur])
	first, last := cur, cur
	for first > 0 && isOption(lines[first-1], col) {
		first--
	}
	for last+1 < len(lines) && isOption(lines[last+1], col) {
		last++
	}
	numbered := 0
	for _, l := range lines[first : last+1] {
		if reNumbered.MatchString(l) {
			numbered++
		}
	}
	footer := false
	for _, l := range lines[last+1:] {
		if reFooter.MatchString(l) {
			footer = true
		}
	}
	weak := !footer && numbered < 2
	if weak && last == first {
		return Dialog{}, false
	}
	top := 0
	for i := first - 1; i >= 0; i-- {
		if reRule.MatchString(lines[i]) {
			top = i + 1
			break
		}
	}
	d := Dialog{Lines: slices.Clone(lines[top:]), Weak: weak}
	for i, l := range lines[first : last+1] {
		t := strings.TrimSpace(l)
		if m := reCursor.FindStringSubmatch(l); m != nil {
			t = m[1]
			d.Cursor = i
		}
		t = reCheck.ReplaceAllString(t, "")
		t = reNumber.ReplaceAllString(t, "")
		d.Options = append(d.Options, strings.TrimSpace(t))
	}
	return d, true
}

// isOption reports whether a line can belong to the option block of a cursor line whose option text starts at column
// col: not blank, not a rule or footer, and indented at least as far as the cursor's option (a dialog's title and body
// sit further left).
func isOption(l string, col int) bool {
	if strings.TrimSpace(l) == "" || reRule.MatchString(l) || reFooter.MatchString(l) {
		return false
	}
	if reCursor.MatchString(l) {
		return true
	}
	return utf8.RuneCountInString(l)-utf8.RuneCountInString(strings.TrimLeft(l, " ")) >= col
}

// textColumn is the column (in runes) where the option text of a cursor line starts.
func textColumn(l string) int {
	m := reCursor.FindStringSubmatchIndex(l)
	if m == nil {
		return 0
	}
	return utf8.RuneCountInString(l[:m[2]])
}

// has reports whether text (spaces collapsed) appears on the dialog's lines.
func (d Dialog) has(text string) bool {
	return strings.Contains(squash(strings.Join(d.Lines, " ")), squash(text))
}

func squash(s string) string { return strings.TrimSpace(reSpaces.ReplaceAllString(s, " ")) }

// Select returns the keys that move the cursor to the option starting with prefix and confirm it, or false when no
// option starts with it.
func (d Dialog) Select(prefix string) ([]string, bool) {
	for i, o := range d.Options {
		if strings.HasPrefix(o, prefix) {
			var keys []string
			for n := i - d.Cursor; n > 0; n-- {
				keys = append(keys, "down")
			}
			for n := d.Cursor - i; n > 0; n-- {
				keys = append(keys, "up")
			}
			return append(keys, "enter"), true
		}
	}
	return nil, false
}

// Between returns the text of the dialog's lines strictly between the line containing after and the line containing
// before, each line trimmed and joined without separators (a long path wraps over several lines).
func (d Dialog) Between(after, before string) (string, bool) {
	start := -1
	for i, l := range d.Lines {
		if start < 0 && strings.Contains(l, after) {
			start = i
			continue
		}
		if start >= 0 && strings.Contains(l, before) {
			var b strings.Builder
			for _, x := range d.Lines[start+1 : i] {
				b.WriteString(strings.TrimSpace(x))
			}
			return b.String(), true
		}
	}
	return "", false
}

// Classify reads a screen.
func Classify(screen string, c Context) Result {
	return classify(Registry, screen, c)
}

func classify(reg []Entry, screen string, c Context) Result {
	d, ok := Find(screen)
	if !ok {
		return Result{Kind: None}
	}
	unknown := func(id, signal string) Result {
		return Result{Kind: Unknown, ID: id, Signal: signal, Lines: d.Lines}
	}
	if Never(d.Lines) {
		if d.Weak {
			return Result{Kind: None}
		}
		return unknown(UnknownID, SignalNever)
	}
	for _, e := range reg {
		if !e.matches(d, c) {
			continue
		}
		a, ok := e.Answer(d, c)
		if !ok || len(a.Keys) == 0 {
			return unknown(e.ID, SignalDeclined)
		}
		return Result{Kind: Known, ID: e.ID, Signal: SignalKnown, Keys: a.Keys, Answer: a.Choice, Lines: d.Lines}
	}
	if d.Weak {
		return Result{Kind: None}
	}
	return unknown(UnknownID, SignalPrompt)
}

func (e Entry) matches(d Dialog, c Context) bool {
	if e.Harness != c.Harness || len(e.Text) == 0 {
		return false
	}
	if e.MinVersion != "" || e.MaxVersion != "" {
		if !dist.ValidVersion(c.Version) {
			return false
		}
		if e.MinVersion != "" && dist.CompareVersions(c.Version, e.MinVersion) < 0 {
			return false
		}
		if e.MaxVersion != "" && dist.CompareVersions(c.Version, e.MaxVersion) > 0 {
			return false
		}
	}
	for _, t := range e.Text {
		if !d.has(t) {
			return false
		}
	}
	return true
}

// provisioned reports whether path (as a dialog shows it) is one of the run's provisioned directories.
func (c Context) provisioned(path string) bool {
	path = strings.TrimSpace(path)
	if rest, ok := strings.CutPrefix(path, "~/"); ok {
		if c.Home == "" {
			return false
		}
		path = filepath.Join(c.Home, rest)
	}
	if !filepath.IsAbs(path) {
		return false
	}
	path = filepath.Clean(path)
	for _, d := range c.Dirs {
		if d != "" && filepath.Clean(d) == path {
			return true
		}
	}
	return false
}
