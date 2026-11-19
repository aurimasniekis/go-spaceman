package proto

import (
	"regexp"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/ids"
)

// Terminal dialogs (protocol minor 15, ADR-0102). The node recognises interactive dialogs on a run's terminal screen
// (internal/dialog): it answers the known ones with named keys and flags the others. A flagged run reports status
// blocked with status kind StatusKindDialog on its run reports (any minor: an older central stores the kind like any
// other), which the API, the UI and spaceman_runs_list name blocked_on_dialog. Each occurrence is also sent as one
// run.dialog message with the node's occurrence ID, so central records it once however often the node repeats it:
//   - answered: SpaceMan answered a registry dialog (ID, signal, the fixed answer word). Central keeps it on the run's
//     event log (audit) only.
//   - blocked: the run waits at a dialog SpaceMan does not answer. Central records it, publishes
//     run.dialog_detected and notifies control and the operators with the excerpt.
//   - cleared: a blocked dialog is gone (answered by an operator, or the program moved on).
//
// The node repeats a blocked occurrence that is still open, and any message not yet sent, after every welcome.

// MinorDialog is the first minor version that carries run.dialog.
const MinorDialog = 15

// StatusKindDialog is the status kind of a run whose program waits at an interactive dialog SpaceMan does not answer
// (status blocked). It is not an OSC 7501 kind.
const StatusKindDialog = "dialog"

// StatusBlockedOnDialog is the run status the API, the UI and the agent tools show for status blocked with kind
// StatusKindDialog.
const StatusBlockedOnDialog = "blocked_on_dialog"

// TypeRunDialog is the node's report of a dialog on a run's terminal (node to central, minor ≥ 15).
const TypeRunDialog = "run.dialog"

// Outcomes of a run.dialog.
const (
	DialogAnswered = "answered"
	DialogBlocked  = "blocked"
	DialogCleared  = "cleared"
)

// DialogExcerptMax bounds a dialog's screen excerpt (bytes); DialogExcerptLines its lines.
const (
	DialogExcerptMax   = 2048
	DialogExcerptLines = 20
)

// RunDialog is one dialog occurrence on a run's terminal.
type RunDialog struct {
	// ID is the node's occurrence ID ("dlg_" and 26 characters): the same occurrence keeps it on every message.
	ID         string `json:"id"`
	RunID      string `json:"run_id"`
	SpaceID    string `json:"space_id"`
	Generation int64  `json:"generation"`
	Outcome    string `json:"outcome"`
	// Dialog is the registry entry's ID, or "unknown".
	Dialog string `json:"dialog"`
	// Signal says what decided (internal/dialog Signal constants).
	Signal string `json:"signal"`
	// Answer is the registry's fixed answer word for answered ("yes", "no", "default").
	Answer string `json:"answer,omitempty"`
	// Excerpt is the dialog's lines with secrets masked, for blocked only (at most DialogExcerptLines lines and
	// DialogExcerptMax bytes). It is terminal output: central shows it in the run's notice and never publishes it.
	Excerpt        string    `json:"excerpt,omitempty"`
	HarnessVersion string    `json:"harness_version,omitempty"`
	DetectedAt     time.Time `json:"detected_at"`
}

var (
	reDialogWord       = regexp.MustCompile(`^[a-z][a-z0-9_.]{0,63}$`)
)

// Valid reports whether the message is well formed: an occurrence ID, a known outcome and fixed words, and an excerpt
// within bounds only on a blocked occurrence.
func (d RunDialog) Valid() bool {
	switch d.Outcome {
	case DialogAnswered, DialogBlocked, DialogCleared:
	default:
		return false
	}
	if !ids.Valid(ids.Dialog, d.ID) || d.RunID == "" || d.SpaceID == "" || !reDialogWord.MatchString(d.Dialog) ||
		!reDialogWord.MatchString(d.Signal) || d.Answer != "" && !reDialogWord.MatchString(d.Answer) {
		return false
	}
	if d.Excerpt != "" && (d.Outcome != DialogBlocked || len(d.Excerpt) > DialogExcerptMax) {
		return false
	}
	return len(d.HarnessVersion) <= 64
}
