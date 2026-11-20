package app

import (
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/dialog"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Every signal a node sends has its own words in the notice, and a flagged dialog without an excerpt (the agent
// restarted and no longer knows the run's secrets) says so instead of showing an empty screen.
func TestDialogNotice(t *testing.T) {
	r := db.Run{ID: "run_x", Role: "implementor", Kind: "worker"}
	for _, sig := range []string{dialog.SignalPrompt, dialog.SignalNever, dialog.SignalDeclined, dialog.SignalStuck, dialog.SignalWithheld} {
		if dialogSignals[sig] == "" {
			t.Errorf("no notice text for signal %q", sig)
		}
	}
	t.Run("withheld", func(t *testing.T) {
		_, body := dialogNotice(r, fleet.RunDialog{ID: "dlg_1", Dialog: "claude.code_mode_optin", Signal: dialog.SignalWithheld, Excerpt: "a screen"})
		if !strings.Contains(body, "already reports a status") || strings.Contains(body, "does not answer it.") {
			t.Fatalf("body %q", body)
		}
		if !strings.Contains(body, "a screen") || strings.Contains(body, "No screen text") {
			t.Fatalf("excerpt missing or wrongly explained: %q", body)
		}
	})
	t.Run("no excerpt", func(t *testing.T) {
		_, body := dialogNotice(r, fleet.RunDialog{ID: "dlg_2", Dialog: "unknown", Signal: dialog.SignalWithheld})
		if !strings.Contains(body, "No screen text is attached") || !strings.Contains(body, "restarted") || strings.Contains(body, "```text") {
			t.Fatalf("body %q", body)
		}
	})
}
