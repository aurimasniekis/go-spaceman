package fleet

import (
	"context"
	"log/slog"
	"sync"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// dialogEnv is ulEnv with one running worker and a record of OnRunDialog calls.
func newFleetDialogEnv(t *testing.T) (*staleEnv, *[]RunDialog, func(proto.RunDialog, int)) {
	t.Helper()
	e := ulEnv(t)
	e.runOn("run_d", "spc_a", "acc_1", "")
	var mu sync.Mutex
	var told []RunDialog
	e.svc.opts.OnRunDialog = func(_ context.Context, r db.Run, d RunDialog) {
		mu.Lock()
		told = append(told, d)
		mu.Unlock()
	}
	send := func(d proto.RunDialog, minor int) {
		t.Helper()
		b, err := proto.Encode(ids.New(ids.Message), proto.TypeRunDialog, e.t0, d)
		if err != nil {
			t.Fatal(err)
		}
		env, err := proto.Decode(b)
		if err != nil {
			t.Fatal(err)
		}
		s := &session{nodeID: "nod_1", seq: 1, minor: minor, log: slog.New(slog.DiscardHandler)}
		_ = e.svc.hub.handle(e.ctx, s, env)
	}
	return e, &told, send
}

func blockedDialog() proto.RunDialog {
	return proto.RunDialog{ID: ids.New(ids.Dialog), RunID: "run_d", SpaceID: "spc_a", Generation: 1, Outcome: proto.DialogBlocked,
		Dialog: "unknown", Signal: "screen_prompt", Excerpt: " Something new\n ❯ 1. Not now", HarnessVersion: "2.1.328"}
}

// ADR-0102: an unknown dialog writes exactly one run.dialog_detected outbox row and one notice per occurrence, however
// often the node repeats it; the excerpt is never in the published payload; cleared publishes run.dialog_cleared once.
func TestRunDialogDetectedOncePerOccurrence(t *testing.T) {
	e, told, send := newFleetDialogEnv(t)
	d := blockedDialog()
	d.DetectedAt = e.t0
	for range 3 {
		send(d, proto.MinorDialog)
	}
	if n := e.count(`SELECT count(*) FROM event_outbox WHERE subject = 'evt.spaceman.fleet.run.dialog_detected'`); n != 1 {
		t.Fatalf("%d dialog_detected rows, want 1", n)
	}
	if n := e.count(`SELECT count(*) FROM event_outbox WHERE subject = 'evt.spaceman.fleet.run.dialog_detected' AND type = 'spaceman.fleet.run_dialog_detected.v1'
		AND json_extract(payload_json, '$.dialog_id') = ? AND json_extract(payload_json, '$.dialog') = 'unknown' AND json_extract(payload_json, '$.signal') = 'screen_prompt'
		AND json_extract(payload_json, '$.run_id') = 'run_d' AND json_extract(payload_json, '$.role') = 'implementor' AND json_extract(payload_json, '$.harness_version') = '2.1.328'
		AND payload_json NOT LIKE '%Something new%' AND payload_json NOT LIKE '%excerpt%'`, d.ID); n != 1 {
		t.Fatal("the dialog_detected payload is not the identifiers-only one")
	}
	if len(*told) != 1 || (*told)[0].ID != d.ID || (*told)[0].Excerpt != d.Excerpt {
		t.Fatalf("told %+v", *told)
	}
	if n := e.count(`SELECT count(*) FROM audit_events WHERE action = 'run.dialog_detected'`); n != 1 {
		t.Fatalf("%d audit events", n)
	}
	// A second occurrence of the same dialog is a new one.
	again := blockedDialog()
	send(again, proto.MinorDialog)
	if n := e.count(`SELECT count(*) FROM event_outbox WHERE subject = 'evt.spaceman.fleet.run.dialog_detected'`); n != 2 || len(*told) != 2 {
		t.Fatalf("second occurrence: %d rows, told %d", n, len(*told))
	}
	// Cleared: once.
	c := d
	c.Outcome, c.Excerpt = proto.DialogCleared, ""
	send(c, proto.MinorDialog)
	send(c, proto.MinorDialog)
	if n := e.count(`SELECT count(*) FROM event_outbox WHERE subject = 'evt.spaceman.fleet.run.dialog_cleared' AND json_extract(payload_json, '$.dialog_id') = ?`, d.ID); n != 1 {
		t.Fatalf("%d dialog_cleared rows", n)
	}
	if n := e.count(`SELECT count(*) FROM run_dialogs WHERE id = ? AND cleared_at IS NOT NULL`, d.ID); n != 1 {
		t.Fatal("not cleared")
	}
	ds, err := e.svc.RunDialogs(e.ctx, "run_d", 10)
	if err != nil || len(ds) != 2 {
		t.Fatalf("dialogs %+v %v", ds, err)
	}
}

// SpaceMan's own answers are logged on the run (audit, run_dialogs) and never published or notified.
func TestRunDialogAnsweredIsLoggedOnly(t *testing.T) {
	e, told, send := newFleetDialogEnv(t)
	d := proto.RunDialog{ID: ids.New(ids.Dialog), RunID: "run_d", SpaceID: "spc_a", Generation: 1, Outcome: proto.DialogAnswered,
		Dialog: "claude.code_mode_optin", Signal: "screen_known", Answer: "no", DetectedAt: e.t0}
	send(d, proto.MinorDialog)
	send(d, proto.MinorDialog)
	if n := e.count(`SELECT count(*) FROM event_outbox WHERE subject LIKE 'evt.spaceman.fleet.run.dialog%'`); n != 0 {
		t.Fatalf("%d published", n)
	}
	if len(*told) != 0 {
		t.Fatalf("told %+v", *told)
	}
	if n := e.count(`SELECT count(*) FROM audit_events WHERE action = 'run.dialog_answered' AND json_extract(details_json, '$.answer') = 'no'
		AND json_extract(details_json, '$.dialog') = 'claude.code_mode_optin'`); n != 1 {
		t.Fatalf("%d audit events", n)
	}
	if n := e.count(`SELECT count(*) FROM run_dialogs WHERE outcome = 'answered'`); n != 1 {
		t.Fatalf("%d rows", n)
	}
}

// A run.dialog below minor 15, for another node's run, another generation, or malformed is not recorded.
func TestRunDialogRefused(t *testing.T) {
	e, told, send := newFleetDialogEnv(t)
	e.runOn("run_x", "spc_b", "acc_1", "")
	e.exec(`INSERT INTO nodes (id, name, state, created_at, created_by, updated_at) VALUES ('nod_2', 'n2', 'active', 0, 't', 0)`)
	e.exec(`UPDATE runs SET node_id = 'nod_2' WHERE id = 'run_x'`)
	old := blockedDialog()
	send(old, proto.MinorDialog-1)
	gen := blockedDialog()
	gen.Generation = 2
	send(gen, proto.MinorDialog)
	other := blockedDialog()
	other.RunID, other.SpaceID = "run_x", "spc_b"
	send(other, proto.MinorDialog)
	bad := blockedDialog()
	bad.Outcome = proto.DialogAnswered // an excerpt only on blocked
	send(bad, proto.MinorDialog)
	if n := e.count(`SELECT count(*) FROM run_dialogs`); n != 0 || len(*told) != 0 {
		t.Fatalf("%d rows, told %d", n, len(*told))
	}
}

func TestStatusLabel(t *testing.T) {
	for _, tc := range [][3]string{{"blocked", "dialog", "blocked_on_dialog"}, {"blocked", "permission", "blocked"}, {"idle", "", "idle"}} {
		if got := StatusLabel(tc[0], tc[1]); got != tc[2] {
			t.Errorf("%v: %q", tc, got)
		}
	}
}
