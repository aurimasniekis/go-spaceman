//go:build fault

package fault

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/aurimasniekis/go-spaceman/internal/agent"
	"github.com/aurimasniekis/go-spaceman/internal/app/apptest"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	hns "github.com/aurimasniekis/go-spaceman/internal/harness"
	"github.com/aurimasniekis/go-spaceman/internal/recording"
	"github.com/aurimasniekis/go-spaceman/internal/runner"
)

var eventually = apptest.Eventually

// centralRecording is the concatenated output and the records of central's copy of a run's recording.
func centralRecording(e *apptest.Env, runID string) (string, []recording.Record) {
	res, err := e.C.Fleet.Recordings().Read(context.Background(), runID, 0)
	if err != nil {
		return "", nil
	}
	var out strings.Builder
	for _, r := range res.Records {
		out.Write(r.Data)
	}
	return out.String(), res.Records
}

func increasing(t *testing.T, recs []recording.Record) {
	t.Helper()
	for i := 1; i < len(recs); i++ {
		if recs[i].Seq <= recs[i-1].Seq {
			t.Fatalf("central's recording is not strictly increasing at %d: %d after %d", i, recs[i].Seq, recs[i-1].Seq)
		}
	}
}

// diskFile is a recording segment on a disk that fills up when full is set: the write that crosses it is partial and
// fails with ENOSPC, as do all later writes.
type diskFile struct {
	f    *os.File
	full *atomic.Bool
}

func (d diskFile) Write(p []byte) (int, error) {
	if d.full.Load() {
		n, _ := d.f.Write(p[:len(p)/2])
		return n, syscall.ENOSPC
	}
	return d.f.Write(p)
}
func (d diskFile) Sync() error  { return d.f.Sync() }
func (d diskFile) Close() error { return d.f.Close() }

// Fault: the node's disk fills up while a run is being recorded (SM-REC-004). The run keeps running and its later
// exit is observed; central's copy keeps what was recorded before, ends with a "disk" trunc record and the close
// record; the node's own recording stays readable up to its last complete frame.
func TestFaultRecordingDiskFull(t *testing.T) {
	e := apptest.New(t)
	var full atomic.Bool
	e.AgentOptions = func(o *agent.Options) {
		o.RecordingCreate = func(path string) (recording.File, error) {
			if full.Load() {
				return nil, syscall.ENOSPC
			}
			f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // test path
			if err != nil {
				return nil, err
			}
			return diskFile{f: f, full: &full}, nil
		}
	}
	e.Enroll()
	e.StartAgent()
	e.Connected()
	sp := e.Space("diskfull", fleet.CreateSpaceInput{})
	r := e.Launch(sp.ID, "implementor", hns.KindClaude)
	term := e.Terminal(r.ID)
	e.Runner.Output(term, "before the disk filled\r\n")
	eventually(t, "output recorded centrally", func() bool {
		out, _ := centralRecording(e, r.ID)
		return strings.Contains(out, "before the disk filled")
	})
	starts := e.Runner.StartCount()

	full.Store(true)
	for i := range 20 {
		e.Runner.Output(term, "while the disk is full "+string(rune('a'+i))+"\r\n")
		time.Sleep(20 * time.Millisecond)
	}
	eventually(t, "disk truncation marker at central", func() bool {
		_, recs := centralRecording(e, r.ID)
		for _, rec := range recs {
			if rec.Kind == recording.KindTrunc && rec.Why == "disk" {
				return true
			}
		}
		return false
	})
	// The run is unaffected: still running, the same single program, nothing restarted.
	time.Sleep(time.Second)
	if x := e.Run(r.ID); x.Observed != "running" {
		t.Fatalf("run %s after the disk filled, want running", x.Observed)
	}
	if n := len(e.Runner.LiveByLabel("spaceman.run", r.ID)); n != 1 || e.Runner.StartCount() != starts {
		t.Fatalf("live %d starts %d→%d", n, starts, e.Runner.StartCount())
	}
	e.Runner.Exit(term, 3, "exited")
	eventually(t, "exit observed", func() bool { x := e.Run(r.ID); return x.Observed == "exited" })
	eventually(t, "close record at central", func() bool {
		_, recs := centralRecording(e, r.ID)
		return len(recs) > 0 && recs[len(recs)-1].Kind == recording.KindClose
	})
	_, recs := centralRecording(e, r.ID)
	increasing(t, recs)
	if recs[0].Kind != recording.KindOpen {
		t.Fatalf("first record %+v", recs[0])
	}
	local, err := recording.Read(filepath.Join(e.AgentDir, "runs", r.ID, "recording"), 0)
	if err != nil || len(local.Records) < 2 {
		t.Fatalf("local recording after disk full: %d records, %v", len(local.Records), err)
	}
}

// Fault: the upload is interrupted (central unreachable) and then the agent is killed while recording, leaving a
// torn final zstd frame. The torn tail is detected, everything before it is kept, the restarted agent adopts the run
// and resumes the recording with a gap marker, catch-up ships what central missed, and central's copy is strictly
// ordered with no duplicates.
func TestFaultInterruptedRecordingAndUpload(t *testing.T) {
	e := apptest.New(t)
	e.Enroll()
	e.StartAgent()
	e.Connected()
	sp := e.Space("torn", fleet.CreateSpaceInput{})
	r := e.Launch(sp.ID, "implementor", hns.KindCodex)
	term := e.Terminal(r.ID)
	e.Runner.Output(term, "part A\r\n")
	eventually(t, "A at central", func() bool { out, _ := centralRecording(e, r.ID); return strings.Contains(out, "part A") })

	e.Unreachable()
	e.Runner.Output(term, "part B1 while central is away\r\n")
	time.Sleep(recording.FlushInterval + 300*time.Millisecond) // B1 gets its own complete frame
	e.KillAgent()
	dir := filepath.Join(e.AgentDir, "runs", r.ID, "recording")
	segs, err := recording.Segments(dir)
	if err != nil || len(segs) == 0 {
		t.Fatalf("segments %v %v", segs, err)
	}
	// The crash interrupted the write of the next frame: half of a complete zstd frame is on disk.
	last := segs[len(segs)-1]
	pre, err := recording.Read(dir, 0)
	if err != nil || len(pre.Records) == 0 {
		t.Fatalf("local recording before the tear: %v", err)
	}
	line, _ := json.Marshal(recording.Record{Seq: pre.Records[len(pre.Records)-1].Seq + 1, Kind: recording.KindOut, Data: []byte("part B2 torn")})
	enc, _ := zstd.NewWriter(nil)
	frame := enc.EncodeAll(append(line, '\n'), nil)
	f, err := os.OpenFile(last, os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // test path
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(frame[:len(frame)/2]); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	torn, err := recording.Read(dir, 0)
	if err != nil || !torn.Truncated {
		t.Fatalf("torn segment not reported: truncated=%v err=%v", torn.Truncated, err)
	}
	var localOut strings.Builder
	for _, rec := range torn.Records {
		localOut.Write(rec.Data)
	}
	if !strings.Contains(localOut.String(), "part B1") {
		t.Fatal("the complete frame before the torn one was lost")
	}

	e.Reachable()
	starts := e.Runner.StartCount()
	e.StartAgent()
	eventually(t, "run running after restart", func() bool { return e.Run(r.ID).Observed == "running" })
	e.Runner.Output(term, "part C after the restart\r\n")
	eventually(t, "B1 caught up and C shipped", func() bool {
		out, recs := centralRecording(e, r.ID)
		if os.Getenv("SM_FAULT_DEBUG") != "" {
			for _, x := range recs {
				t.Logf("central seq %d %s %q", x.Seq, x.Kind, x.Data)
			}
		}
		return strings.Contains(out, "part B1") && strings.Contains(out, "part C")
	})
	if e.Runner.StartCount() != starts || len(e.Runner.LiveByLabel("spaceman.run", r.ID)) != 1 {
		t.Fatal("restart started the program again")
	}
	e.Runner.Exit(term, 0, "exited")
	eventually(t, "close at central", func() bool {
		_, recs := centralRecording(e, r.ID)
		return len(recs) > 0 && recs[len(recs)-1].Kind == recording.KindClose
	})
	res, err := e.C.Fleet.Recordings().Read(context.Background(), r.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	increasing(t, res.Records)
	if res.Gaps == 0 {
		t.Fatal("the reattach after the crash is not marked as a gap")
	}
}

// Fault: the agent process dies mid-run (journal closed under it, connection dropped, no graceful shutdown) — once
// while runs are live and once between the runner starting a program and the journal recording it. The restarted
// agent adopts every program; nothing is launched twice.
func TestFaultAgentKilledMidRun(t *testing.T) {
	e := apptest.New(t)
	e.Enroll()
	e.StartAgent()
	e.Connected()
	sp := e.Space("killed", fleet.CreateSpaceInput{})
	r1 := e.Launch(sp.ID, "implementor", hns.KindClaude)
	r2 := e.Launch(sp.ID, "reviewer", hns.KindCodex)
	e.Runner.Output(e.Terminal(r1.ID), "busy\r\n")

	starts := e.Runner.StartCount()
	e.KillAgent()
	e.StartAgent()
	eventually(t, "both runs running after the kill", func() bool {
		return e.Run(r1.ID).Observed == "running" && e.Run(r2.ID).Observed == "running"
	})
	time.Sleep(time.Second) // reconcile, heartbeats, command re-delivery
	if got := e.Runner.StartCount(); got != starts {
		t.Fatalf("restart after a kill launched programs: %d → %d", starts, got)
	}
	for _, id := range []string{r1.ID, r2.ID} {
		if n := len(e.Runner.LiveByLabel("spaceman.run", id)); n != 1 {
			t.Fatalf("run %s has %d live programs", id, n)
		}
	}

	// Killed between "program started" and "start journaled".
	started := make(chan string, 1)
	e.Runner.AfterStart = func(p runner.Process) {
		if p.Labels["spaceman.run"] != "" {
			select {
			case started <- p.ID:
			default:
			}
		}
	}
	r3, err := e.C.Fleet.LaunchRun(context.Background(), apptest.Admin, sp.ID, fleet.LaunchInput{Kind: "worker", Role: "planner", Harness: hns.KindClaude, Prompt: "x"})
	if err != nil {
		t.Fatal(err)
	}
	var term string
	select {
	case term = <-started:
	case <-time.After(20 * time.Second):
		t.Fatal("third run never started")
	}
	e.KillAgent()
	e.Runner.AfterStart = nil
	starts = e.Runner.StartCount()
	e.StartAgent()
	eventually(t, "unjournaled launch adopted", func() bool { x := e.Run(r3.ID); return x.Observed == "running" && x.TerminalID == term })
	time.Sleep(time.Second)
	if got := e.Runner.StartCount(); got != starts || len(e.Runner.LiveByLabel("spaceman.run", r3.ID)) != 1 {
		t.Fatalf("adoption launched again: starts %d → %d", starts, got)
	}
}

// Fault: central stays unreachable beyond the lease and the disconnect grace. The node stops the space's runs on its
// own; when central returns nothing is relaunched, and the run's end is reported.
func TestFaultDisconnectBeyondLeaseAndGrace(t *testing.T) {
	e := apptest.New(t, apptest.ShortLease())
	e.Enroll()
	e.StartAgent()
	e.Connected()
	sp := e.Space("beyond", fleet.CreateSpaceInput{DisconnectPolicy: "kill"})
	r := e.Launch(sp.ID, "implementor", hns.KindClaude)
	starts := e.Runner.StartCount()
	e.Unreachable()
	lost := time.Now()
	eventually(t, "run stopped by the node without central", func() bool { return len(e.Runner.LiveByLabel("spaceman.run", r.ID)) == 0 })
	if waited := time.Since(lost); waited < time.Second {
		t.Fatalf("run stopped after %s, before the lease could expire", waited)
	}
	e.Reachable()
	e.Connected()
	eventually(t, "end reported to central", func() bool { o := e.Run(r.ID).Observed; return o == "exited" || o == "failed" || o == "lost" })
	time.Sleep(time.Second)
	if e.Runner.StartCount() != starts {
		t.Fatal("a stopped run was relaunched when central returned")
	}
}

// Fault: a network partition during a command. Central's launch reaches the node but the acknowledgement and the
// run report are lost; then the connection breaks. Central keeps the command undelivered and re-sends it after the
// partition heals (at least once); the node recognises the repeat, acknowledges it as a duplicate and launches
// nothing again.
func TestFaultPartitionDuringCommand(t *testing.T) {
	e := apptest.New(t, apptest.WithProxy())
	e.Enroll()
	e.StartAgent()
	e.Connected()
	sp := e.Space("partition", fleet.CreateSpaceInput{})
	started := make(chan struct{}, 1)
	e.Runner.AfterStart = func(p runner.Process) {
		if p.Labels["spaceman.run"] != "" {
			select {
			case started <- struct{}{}:
			default:
			}
		}
	}
	e.Proxy.DropUpstream(true) // what the node sends is lost from here on
	r, err := e.C.Fleet.LaunchRun(context.Background(), apptest.Admin, sp.ID, fleet.LaunchInput{Kind: "worker", Role: "implementor", Harness: hns.KindClaude, Prompt: "x"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(20 * time.Second):
		t.Fatal("the node never received the launch")
	}
	time.Sleep(200 * time.Millisecond) // the acknowledgement is sent into the black hole
	e.Proxy.Cut()
	e.Runner.AfterStart = nil
	ctx := context.Background()
	cmds, err := e.C.Store.R().ListCommandsByRun(ctx, sql.NullString{String: r.ID, Valid: true})
	if err != nil || len(cmds) != 1 {
		t.Fatalf("commands %v %v", cmds, err)
	}
	if cmds[0].State != "sent" {
		t.Fatalf("command state %q during the partition, want sent (the ack was lost)", cmds[0].State)
	}
	if e.Proxy.UpBytesDropped.Load() == 0 {
		t.Fatal("nothing was dropped: the partition did not cover the acknowledgement")
	}
	starts := e.Runner.StartCount()
	time.Sleep(500 * time.Millisecond)
	e.Proxy.Heal()
	eventually(t, "command re-delivered and acknowledged", func() bool {
		cs, _ := e.C.Store.R().ListCommandsByRun(ctx, sql.NullString{String: r.ID, Valid: true})
		return len(cs) == 1 && cs[0].State == "duplicate"
	})
	cs, _ := e.C.Store.R().ListCommandsByRun(ctx, sql.NullString{String: r.ID, Valid: true})
	if cs[0].Attempts < 2 {
		t.Fatalf("attempts %d, want at least 2", cs[0].Attempts)
	}
	eventually(t, "run reported running", func() bool { return e.Run(r.ID).Observed == "running" })
	if e.Runner.StartCount() != starts || len(e.Runner.LiveByLabel("spaceman.run", r.ID)) != 1 {
		t.Fatal("the re-delivered launch started the program again")
	}
}
