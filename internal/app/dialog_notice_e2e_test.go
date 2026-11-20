package app_test

import (
	"context"
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/app"
	"github.com/aurimasniekis/go-spaceman/internal/dialog"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	hns "github.com/aurimasniekis/go-spaceman/internal/harness"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// ADR-0102, the notice and its texts as the in-app messages show them (notifyRunDialog called directly; the hub wiring
// is TestRunDialogOverTheNodeConnection): a run blocked on a dialog tells the operators (and control) once,
// in words that fit the signal. An unknown dialog, a withheld answer and a dialog without an excerpt (the node agent
// restarted) each get their own text, and a repeat of an occurrence sends nothing more.
func TestRunDialogNoticeTexts(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	secret, _, _ := h.c.Auth.IssueRootToken(ctx)
	root := h.client()
	root.bearer = secret
	en, err := h.c.Fleet.CreateNode(ctx, admin, fleet.CreateNodeInput{Name: "node-a"})
	if err != nil {
		t.Fatal(err)
	}
	var sp struct{ ID string }
	r := root.do("POST", "/spaces", map[string]any{"slug": "payments", "name": "Payments", "node_id": en.Node.ID})
	expect(t, r, 201)
	r.json(t, &sp)
	run := db.Run{ID: "run_dlg", SpaceID: sp.ID, NodeID: en.Node.ID, Generation: 1, Kind: "worker", Role: "implementor"}
	if err := h.c.Store.W().TxNamed(ctx, "test.dialog_run", func(tx *store.Tx) error {
		now := h.clk.Now().UnixMicro()
		return tx.Q.InsertRun(ctx, db.InsertRunParams{ID: run.ID, SpaceID: sp.ID, NodeID: en.Node.ID, Generation: 1, Kind: "worker",
			Role: "implementor", Harness: hns.KindClaude, InstanceID: "ins_dlg", SessionID: "ses_dlg", LaunchJson: "{}", ManifestJson: "{}",
			CreatedAt: now, CreatedBy: "usr_test", UpdatedAt: now})
	}); err != nil {
		t.Fatal(err)
	}

	unknown := fleet.RunDialog{ID: "dlg_unknown", Dialog: "unknown", Signal: dialog.SignalPrompt, Excerpt: "Pick a thing\n❯ 1. Yes"}
	withheld := fleet.RunDialog{ID: "dlg_withheld", Dialog: "claude.code_mode_optin", Signal: dialog.SignalWithheld, Excerpt: "Code Mode"}
	bare := fleet.RunDialog{ID: "dlg_bare", Dialog: "unknown", Signal: dialog.SignalStuck}
	for _, d := range []fleet.RunDialog{unknown, withheld, bare, unknown} {
		app.NotifyRunDialog(h.c, ctx, run, d)
	}

	var msgs struct {
		Items []struct {
			ToKind   string `json:"to_kind"`
			FromKind string `json:"from_kind"`
			Subject  string `json:"subject"`
			Body     string `json:"body"`
		} `json:"items"`
	}
	r = root.do("GET", "/spaces/"+sp.ID+"/messages", nil)
	expect(t, r, 200)
	r.json(t, &msgs)
	bodyFor := func(occurrence, to string) string {
		t.Helper()
		var found []string
		for _, m := range msgs.Items {
			if m.FromKind == "system" && m.ToKind == to && strings.Contains(m.Body, "occurrence `"+occurrence+"`") {
				if !strings.Contains(m.Subject, "run_dlg") || !strings.Contains(m.Body, "**blocked_on_dialog**") {
					t.Fatalf("notice %q / %q", m.Subject, m.Body)
				}
				found = append(found, m.Body)
			}
		}
		if len(found) != 1 {
			t.Fatalf("%d notices for %s to %s (want 1, a repeat sends nothing more)", len(found), occurrence, to)
		}
		return found[0]
	}
	for _, to := range []string{"operators", "control"} {
		b := bodyFor("dlg_unknown", to)
		if !strings.Contains(b, "SpaceMan does not know this dialog") || !strings.Contains(b, "❯ 1. Yes") || strings.Contains(b, "No screen text") {
			t.Fatalf("unknown to %s: %q", to, b)
		}
		b = bodyFor("dlg_withheld", to)
		if !strings.Contains(b, "already reports a status") || !strings.Contains(b, "Code Mode") || strings.Contains(b, "No screen text") {
			t.Fatalf("withheld to %s: %q", to, b)
		}
		b = bodyFor("dlg_bare", to)
		if !strings.Contains(b, "it is still there") || !strings.Contains(b, "No screen text is attached") || strings.Contains(b, "```text") {
			t.Fatalf("no excerpt to %s: %q", to, b)
		}
	}

	// Control itself is the one waiting: only the operators are told.
	ctl := run
	ctl.ID, ctl.Kind = "run_ctl", "control"
	app.NotifyRunDialog(h.c, ctx, ctl, fleet.RunDialog{ID: "dlg_ctl", Dialog: "unknown", Signal: dialog.SignalPrompt})
	r = root.do("GET", "/spaces/"+sp.ID+"/messages", nil)
	r.json(t, &msgs)
	toOps, toControl := 0, 0
	for _, m := range msgs.Items {
		if strings.Contains(m.Body, "occurrence `dlg_ctl`") {
			if m.ToKind == "operators" {
				toOps++
			} else if m.ToKind == "control" {
				toControl++
			}
		}
	}
	if toOps != 1 || toControl != 0 {
		t.Fatalf("control run's dialog: %d to operators, %d to control", toOps, toControl)
	}
}
