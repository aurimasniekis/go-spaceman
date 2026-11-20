package app

import (
	"context"
	"fmt"
	"strings"

	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/dialog"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/messages"
	"github.com/aurimasniekis/go-spaceman/internal/obs"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// dialogSignals explains a dialog signal in a notice.
var dialogSignals = map[string]string{
	dialog.SignalPrompt:   "SpaceMan does not know this dialog",
	dialog.SignalNever:    "it is a permission, approval or login dialog, which SpaceMan never answers",
	dialog.SignalDeclined: "SpaceMan knows this dialog, but its answer is not safe for this run",
	dialog.SignalStuck:    "SpaceMan answered it, and it is still there",
	dialog.SignalWithheld: "the program already reports a status, so it is past its startup dialogs and SpaceMan does not answer what looks like one on its screen",
}

// noExcerptNote says why a flagged dialog arrives without its screen: the node agent restarted since the run started,
// so it no longer knows the run's secret values and sends no excerpt rather than one that might show them.
const noExcerptNote = "No screen text is attached: the node agent restarted after this run started, and it no longer knows the run's " +
	"secret values to mask, so it sends none. Open the run's terminal to see the dialog."

// notifyRunDialog tells the operators and control that a run waits at a dialog SpaceMan does not answer (ADR-0102), once
// per occurrence (the dedup key is the occurrence). The excerpt is terminal output with secrets masked by the node: it
// goes into these in-app messages only, never into a published event.
func (c *Central) notifyRunDialog(ctx context.Context, r db.Run, d fleet.RunDialog) {
	subject, text := dialogNotice(r, d)
	from := messages.From{Kind: "system", ID: "dialogs", Label: "Dialogs"}
	actor := audit.Actor{Type: audit.ActorSystem, ID: "dialogs"}
	for _, to := range []string{"operators", "control"} {
		if to == "control" && r.Kind == "control" {
			continue // control itself is the one waiting
		}
		if _, _, err := c.Messages.Send(ctx, actor, r.SpaceID, from, messages.SendInput{To: to, Kind: "notification", Subject: subject,
			Body: text, DedupKey: "run-dialog:" + d.ID + ":" + to}); err != nil {
			c.Log.Warn("tell about a run waiting at a dialog", obs.Err(err))
		}
	}
}

// dialogNotice is the subject and body of the notice about a flagged dialog.
func dialogNotice(r db.Run, d fleet.RunDialog) (subject, body string) {
	why := dialogSignals[d.Signal]
	if why == "" {
		why = "SpaceMan does not answer it"
	}
	subject = fmt.Sprintf("Run %s (%s) waits at a dialog", r.ID, r.Role)
	var b strings.Builder
	fmt.Fprintf(&b, "The %s run `%s` of role `%s` is waiting at an interactive dialog on its terminal (`%s`, signal `%s`): %s. "+
		"It shows as **blocked_on_dialog** until the dialog is gone.\n\n", r.Kind, r.ID, r.Role, d.Dialog, d.Signal, why)
	b.WriteString("Answer it from the run's terminal (take it over in the UI), or stop the run.\n\n")
	if d.Excerpt != "" {
		fmt.Fprintf(&b, "Screen (secrets masked; occurrence `%s`):\n\n```text\n%s\n```\n", d.ID, strings.ReplaceAll(d.Excerpt, "```", "'''"))
	} else {
		fmt.Fprintf(&b, "%s (occurrence `%s`)\n", noExcerptNote, d.ID)
	}
	return subject, b.String()
}
