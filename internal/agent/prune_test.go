package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/harness"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/recording"
	"github.com/aurimasniekis/go-spaceman/internal/store"
)

func pruneAgent(t *testing.T, dir string) *Agent {
	t.Helper()
	j, err := OpenJournal(context.Background(), filepath.Join(dir, "journal"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = j.Close() })
	a := New(j, Options{DataDir: dir, Clock: clock.System{}, Log: slog.New(slog.DiscardHandler)})
	return a
}

// seedRun journals a run in state, last updated age ago.
func seedRun(t *testing.T, a *Agent, id, state string, age time.Duration) {
	t.Helper()
	ctx := context.Background()
	if err := a.j.insertRun(ctx, runRow{RunID: id, SpaceID: "spc_1", Generation: 1, Kind: "worker", Role: "r", Harness: harness.KindClaude, InstanceID: "ins_1", TerminalName: "t_" + id}); err != nil {
		t.Fatal(err)
	}
	err := a.j.s.W().TxNamed(ctx, "test.seed", func(tx *store.Tx) error {
		_, err := tx.SQL.ExecContext(ctx, `UPDATE runs SET state = ?, terminal_id = ?, updated_at = ? WHERE run_id = ?`, state, "term_"+id, a.now().Add(-age).UnixMicro(), id)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func runIDs(t *testing.T, a *Agent) []string {
	t.Helper()
	rs, err := a.j.runs(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, r := range rs {
		ids = append(ids, r.RunID)
	}
	return ids
}

// ack runs the connect exchange for the ended runs sent so far: central lists activeAtCentral as active.
func ack(a *Agent, activeAtCentral ...string) {
	var rec proto.Reconcile
	for _, id := range activeAtCentral {
		rec.Runs = append(rec.Runs, proto.DesiredRun{RunID: id})
	}
	a.finalAck(context.Background(), rec)
}

const day = 24 * time.Hour

// The hello is bounded by live and recent runs, not by history: 2500 ended runs add nothing to it.
func TestHelloBoundedByLiveAndRecentRuns(t *testing.T) {
	a := pruneAgent(t, t.TempDir())
	ctx := context.Background()
	for i := range 2500 {
		seedRun(t, a, fmt.Sprintf("run_old_%04d", i), proto.RunExited, 3*day+time.Duration(i)*time.Minute)
	}
	seedRun(t, a, "run_live", proto.RunRunning, 5*day)
	seedRun(t, a, "run_adopt", proto.RunStarting, 40*day)
	seedRun(t, a, "run_recent", proto.RunFailed, time.Hour)

	hello, follow, err := a.helloRuns(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ids := func(rs []runRow) []string {
		var out []string
		for _, r := range rs {
			out = append(out, r.RunID)
		}
		return out
	}
	if got := ids(hello); !slices.Equal(got, []string{"run_adopt", "run_live", "run_recent"}) {
		t.Fatalf("hello runs %v", got)
	}
	if len(follow) != 2500 {
		t.Fatalf("follow-up %d runs, want the 2500 older ended ones", len(follow))
	}
	h := proto.Hello{Runs: []proto.RunReport{}}
	for _, r := range hello {
		h.Runs = append(h.Runs, r.report(a.now()))
	}
	b, _ := json.Marshal(h)
	if len(b) > 4096 {
		t.Fatalf("hello is %d bytes with 3 runs", len(b))
	}

	// The follow-up goes out after the welcome; the next connection's hello does not repeat it, and central's reconcile
	// acknowledges what central no longer lists as active.
	a.noteSent(hello)
	a.noteSent(follow)
	hello, follow, _ = a.helloRuns(ctx)
	if len(follow) != 0 || !slices.Equal(ids(hello), []string{"run_adopt", "run_live", "run_recent"}) {
		t.Fatalf("second hello %v, follow-up %d", ids(hello), len(follow))
	}
	ack(a, "run_old_0007") // central still lists this one as active: its report did not apply
	hello, follow, _ = a.helloRuns(ctx)
	if got := ids(hello); !slices.Equal(got, []string{"run_adopt", "run_live"}) {
		t.Fatalf("hello after the acknowledgement %v", got)
	}
	if got := ids(follow); !slices.Equal(got, []string{"run_old_0007"}) {
		t.Fatalf("follow-up after the acknowledgement %v", got)
	}
}

// An unacknowledged ended run is sent again by every hello (while recent), and a run that changed after it was sent is
// not covered by the earlier acknowledgement.
func TestHelloKeepsUnacknowledgedRuns(t *testing.T) {
	a := pruneAgent(t, t.TempDir())
	ctx := context.Background()
	seedRun(t, a, "run_a", proto.RunExited, time.Hour)
	hello, _, _ := a.helloRuns(ctx)
	a.noteSent(hello)
	ack(a, "run_a") // central still has it active
	if hello, _, _ = a.helloRuns(ctx); len(hello) != 1 {
		t.Fatalf("unacknowledged run dropped from the hello: %d", len(hello))
	}
	a.noteSent(hello) // central listed it: it was reported again, and the next connection's hello sends it too
	ack(a)
	if hello, _, _ = a.helloRuns(ctx); len(hello) != 0 {
		t.Fatalf("acknowledged run still in the hello: %d", len(hello))
	}
	// Its final state changes (for example a transcript summary updates the row): sent and acknowledged again.
	row, _ := a.j.getRun(ctx, "run_a")
	row.Reason = "changed"
	if err := a.j.updateRun(ctx, row); err != nil {
		t.Fatal(err)
	}
	if hello, _, _ = a.helloRuns(ctx); len(hello) != 1 {
		t.Fatalf("changed run not sent again: %d", len(hello))
	}
}

// Each guard of the deletion keeps its run.
func TestPruneGuards(t *testing.T) {
	ctx := context.Background()
	a := pruneAgent(t, t.TempDir())
	old := 45 * day
	seed := func(id, state string, age time.Duration, sent bool) {
		seedRun(t, a, id, state, age)
		if sent {
			r, _ := a.j.getRun(ctx, id)
			a.noteSent([]runRow{r})
		}
	}
	seed("run_prune", proto.RunExited, old, true)          // the one that goes
	seed("run_prune_lost", proto.RunLost, old, true)       // lost and failed count as ended too
	seed("run_live", proto.RunRunning, old, true)          // not ended: live or adoptable (adoption after a restart)
	seed("run_starting", proto.RunStarting, old, true)     // a run adoption may still resolve
	seed("run_recent", proto.RunExited, 3*day, true)       // inside the retention
	seed("run_unacked", proto.RunExited, old, false)       // central was never shown its final state
	seed("run_central_active", proto.RunExited, old, true) // central still lists it as active
	seed("run_transcript", proto.RunExited, old, true)     // transcript upload unfinished
	seed("run_dream", proto.RunExited, old, true)          // a dream's evidence positions reference it
	seed("run_recording", proto.RunExited, old, true)      // recording segments central has not acknowledged
	seed("run_held", proto.RunExited, old, true)           // records kept back from a shipment
	seed("run_recorder", proto.RunExited, old, true)       // recorder still open
	err := a.j.s.W().TxNamed(ctx, "test.guards", func(tx *store.Tx) error {
		if _, err := tx.SQL.ExecContext(ctx, `INSERT INTO transcript_uploads (run_id, path, size, sha256, done, updated_at) VALUES ('run_transcript', 'p', 1, 'x', 0, 1)`); err != nil {
			return err
		}
		_, err := tx.SQL.ExecContext(ctx, `INSERT INTO dream_pending (run_id, config_id, path, offset) VALUES ('run_dream', 'c', 'p', 1)`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	w, err := recording.Open(recording.Options{Dir: a.RecordingDir("run_recording")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Append(recording.Record{Seq: 1, Kind: recording.KindOut, Data: []byte("hi")}); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	a.held = map[string]*heldRecords{"run_held": {}}
	a.recorders = map[string]*recorder{"run_recorder": {}}

	ack(a, "run_central_active")
	want := []string{"run_central_active", "run_dream", "run_held", "run_live", "run_recent", "run_recorder", "run_recording", "run_starting",
		"run_transcript", "run_unacked"}
	if got := runIDs(t, a); !slices.Equal(got, want) {
		t.Fatalf("journal after prune %v\nwant %v", got, want)
	}

	// Once the transcript is uploaded and the dream committed, the run goes with its finished upload row.
	err = a.j.s.W().TxNamed(ctx, "test.release", func(tx *store.Tx) error {
		if _, err := tx.SQL.ExecContext(ctx, `UPDATE transcript_uploads SET done = 1`); err != nil {
			return err
		}
		_, err := tx.SQL.ExecContext(ctx, `DELETE FROM dream_pending`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := a.pruneRuns(ctx); n != 2 {
		t.Fatalf("pruned %d, want the run with the finished transcript and the run of the committed dream", n)
	}
	var rows int
	_ = a.j.s.ReadDB().QueryRow(`SELECT COUNT(*) FROM transcript_uploads`).Scan(&rows)
	if rows != 0 {
		t.Fatalf("%d transcript upload rows left", rows)
	}
}

// A run that changed since central saw it is not deleted on the strength of the older acknowledgement.
func TestPruneSkipsRunChangedSinceAck(t *testing.T) {
	ctx := context.Background()
	a := pruneAgent(t, t.TempDir())
	seedRun(t, a, "run_x", proto.RunExited, 45*day)
	r, _ := a.j.getRun(ctx, "run_x")
	a.noteSent([]runRow{r})
	a.finalAck(ctx, proto.Reconcile{})
	if _, err := a.j.deleteEndedRun(ctx, runRow{RunID: "run_x", UpdatedAt: r.UpdatedAt - 1}); err != nil {
		t.Fatal(err)
	}
	if got := runIDs(t, a); len(got) != 0 {
		// finalAck already pruned it: seed again and change it behind the pass's back.
		t.Fatalf("expected the acknowledged run to go, have %v", got)
	}
	seedRun(t, a, "run_y", proto.RunExited, 45*day)
	r, _ = a.j.getRun(ctx, "run_y")
	if ok, err := a.j.deleteEndedRun(ctx, runRow{RunID: "run_y", UpdatedAt: r.UpdatedAt - 1}); ok || err != nil {
		t.Fatalf("deleted a run whose row changed: %v %v", ok, err)
	}
	if ok, err := a.j.deleteEndedRun(ctx, r); !ok || err != nil {
		t.Fatalf("did not delete the unchanged run: %v %v", ok, err)
	}
}

// Retention never goes below the pin window: a configured shorter one is raised to it, so the journal still knows the pins
// the cache's GC protects; GC's in-use set reads live runs, which are never pruned.
func TestPruneKeepsGCPins(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	a := pruneAgent(t, dir)
	a.o.JournalRetention = time.Hour
	if a.retention() != claudePinWindow {
		t.Fatalf("retention %v", a.retention())
	}
	for id, v := range map[string]string{"run_pin": "2.1.1", "run_live": "2.1.2", "run_gone": "2.1.3"} {
		age := 20 * day
		state := proto.RunExited
		switch id {
		case "run_live":
			state, age = proto.RunRunning, 90*day
		case "run_gone":
			age = 60 * day // past the pin window: nothing pinned by it any more
		}
		seedRun(t, a, id, state, age)
		if err := a.j.setRunHarness(ctx, id, harnessRun{Version: v, Path: "/c/" + v, Requested: v}); err != nil {
			t.Fatal(err)
		}
		// setRunHarness stamps updated_at: put the age back.
		_ = a.j.s.W().TxNamed(ctx, "test.age", func(tx *store.Tx) error {
			_, err := tx.SQL.ExecContext(ctx, `UPDATE runs SET updated_at = ? WHERE run_id = ?`, a.now().Add(-age).UnixMicro(), id)
			return err
		})
		r, _ := a.j.getRun(ctx, id)
		a.noteSent([]runRow{r})
	}
	ack(a)
	if got := runIDs(t, a); !slices.Equal(got, []string{"run_live", "run_pin"}) {
		t.Fatalf("journal after prune %v", got)
	}
	if used, _ := a.j.harnessVersionsInUse(ctx, harness.KindClaude); !slices.Equal(used, []string{"2.1.2"}) {
		t.Fatalf("in use %v", used)
	}
	if p := a.claudePinned(ctx, nil); !slices.Equal(p, []string{"2.1.1"}) {
		t.Fatalf("pinned %v", p)
	}
}

// Adoption after a restart: the live runs of the old process are all still in the journal and in the hello of the new
// one, whatever was pruned; what was ended and never acknowledged by the old process is not deleted by the new one
// before its own connection acknowledged it.
func TestPruneAfterRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	j, err := OpenJournal(ctx, filepath.Join(dir, "journal"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	a := New(j, Options{DataDir: dir, Clock: clock.System{}, Log: slog.New(slog.DiscardHandler)})
	seedRun(t, a, "run_live", proto.RunRunning, 50*day)
	seedRun(t, a, "run_old", proto.RunExited, 50*day)
	r, _ := a.j.getRun(ctx, "run_old")
	a.noteSent([]runRow{r})
	ack(a) // pruned: acknowledged in this process
	seedRun(t, a, "run_old2", proto.RunExited, 50*day)
	_ = j.Close()

	a = pruneAgent(t, dir)
	if n := a.pruneRuns(ctx); n != 0 {
		t.Fatalf("a fresh process pruned %d runs without an acknowledgement", n)
	}
	hello, follow, err := a.helloRuns(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(hello) != 1 || hello[0].RunID != "run_live" || len(follow) != 1 || follow[0].RunID != "run_old2" {
		t.Fatalf("hello %v follow-up %v", hello, follow)
	}
	live, _ := a.j.runs(ctx, true)
	if len(live) != 1 || live[0].RunID != "run_live" {
		t.Fatalf("adoptable runs %v", live)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatal(err)
	}
}

// Segments on disk while central's acknowledgement is unknown (after a restart) keep the run even when reading them yields
// no records: the recording may still be shipped (or relocated), so nothing is deleted on a guess.
func TestPruneKeepsRecordingWithUnknownAckAndNoRecords(t *testing.T) {
	ctx := context.Background()
	a := pruneAgent(t, t.TempDir())
	seedRun(t, a, "run_seg", proto.RunExited, 45*day)
	dir := a.RecordingDir("run_seg")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, recording.SegmentName(1)), nil, 0o600); err != nil { // a segment holding no record
		t.Fatal(err)
	}
	if !hasSegments(dir) {
		t.Fatal("setup: no segment")
	}
	if res, err := recording.Read(dir, 0); err != nil || len(res.Records) != 0 {
		t.Fatalf("setup: %d records, %v", len(res.Records), err)
	}
	r, _ := a.j.getRun(ctx, "run_seg")
	a.noteSent([]runRow{r})
	ack(a)
	if got := runIDs(t, a); !slices.Equal(got, []string{"run_seg"}) {
		t.Fatalf("run with segments and an unknown acknowledgement was pruned: %v", got)
	}
	// Once central acknowledged the recording (known), nothing is left to ship and the run goes.
	a.mu.Lock()
	a.acked = map[string]uint64{"run_seg": 0}
	a.mu.Unlock()
	if n := a.pruneRuns(ctx); n != 1 {
		t.Fatalf("pruned %d with the acknowledgement known", n)
	}
}

// The follow-up goes out on the connection it was started for, and skips a run whose row changed since the snapshot.
func TestSendFollowUpSkipsChangedRowAndUsesItsConn(t *testing.T) {
	ctx := context.Background()
	a := pruneAgent(t, t.TempDir())
	conn, reports := wsSink(t, a)
	a.mu.Lock()
	a.conn = nil // the agent's current connection is not the one the follow-up belongs to
	a.mu.Unlock()
	seedRun(t, a, "run_a", proto.RunExited, 3*day)
	seedRun(t, a, "run_b", proto.RunExited, 3*day)
	seedRun(t, a, "run_c", proto.RunExited, 3*day)
	_, follow, _ := a.helloRuns(ctx)
	row, _ := a.j.getRun(ctx, "run_b")
	row.Reason = "changed"
	if err := a.j.updateRun(ctx, row); err != nil {
		t.Fatal(err)
	}
	a.sendFollowUp(ctx, conn, follow)
	expectReport(t, reports, "run_a")
	expectReport(t, reports, "run_c")
	select {
	case id := <-reports:
		t.Fatalf("unexpected report for %s", id)
	case <-time.After(200 * time.Millisecond):
	}
	a.mu.Lock()
	_, sentB := a.finalSent["run_b"]
	a.mu.Unlock()
	if sentB {
		t.Fatal("the skipped run was marked sent")
	}
}
