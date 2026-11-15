package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	rt "github.com/aurimasniekis/go-spaceman/internal/runner/runnertest"
)

// helloRun returns runID's report in the next hello.
func (fc *wsCentral) helloRun(t *testing.T, runID string) proto.RunReport {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case env := <-fc.msgs:
			noTranscriptText(t, env)
			if env.Type != proto.TypeHello {
				continue
			}
			var h proto.Hello
			if err := env.Decode(&h); err != nil {
				t.Fatal(err)
			}
			for _, r := range h.Runs {
				if r.RunID == runID {
					return r
				}
			}
			t.Fatalf("hello without %s", runID)
		case <-deadline:
			t.Fatal("timed out waiting for hello")
		}
	}
}

// collect returns runID's reports until none has come for d.
func (fc *wsCentral) collect(t *testing.T, runID string, d time.Duration) []proto.RunReport {
	t.Helper()
	var out []proto.RunReport
	for {
		select {
		case env := <-fc.msgs:
			noTranscriptText(t, env)
			var r proto.RunReport
			if env.Type == proto.TypeRunReport && env.Decode(&r) == nil && r.RunID == runID {
				out = append(out, r)
			}
		case <-time.After(d):
			return out
		}
	}
}

// limitChanges counts the reports whose limit differs from the report before (the first against before).
func limitChanges(before *proto.RunLimit, rs []proto.RunReport) int {
	n := 0
	for _, r := range rs {
		a, _ := json.Marshal(before)
		b, _ := json.Marshal(r.Limit)
		if string(a) != string(b) {
			n++
		}
		before = r.Limit
	}
	return n
}

func noTranscriptText(t *testing.T, env proto.Envelope) {
	t.Helper()
	b, _ := json.Marshal(env)
	if strings.Contains(string(b), rt.TranscriptMarker) {
		t.Fatalf("%s carries transcript text", env.Type)
	}
}

// startAgent runs a until the returned stop is called (or the test ends).
func startAgent(t *testing.T, a *Agent, fc *wsCentral, clk clock.Clock) (stop func()) {
	t.Helper()
	a.o.Clock, a.o.CentralURL, a.o.Credential = clk, fc.srv.URL, "spaceman_nd_test"
	a.o.BridgeSocket = filepath.Join(t.TempDir(), "b.sock")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = a.Run(ctx); close(done) }()
	stopped := false
	stop = func() {
		if !stopped {
			stopped = true
			cancel()
			<-done
		}
	}
	t.Cleanup(stop)
	return stop
}

func wantOpen(t *testing.T, what string, r proto.RunReport, kind string, reset, since time.Time) {
	t.Helper()
	l := r.Limit
	if l == nil || l.Cleared || l.Kind != kind || l.ResetsAt == nil || !l.ResetsAt.Equal(reset) || !l.Since.Equal(since) {
		t.Fatalf("%s: limit %+v, want %s until %v since %v", what, l, kind, reset, since)
	}
}

func wantCleared(t *testing.T, what string, r proto.RunReport, since time.Time) {
	t.Helper()
	if l := r.Limit; l == nil || !l.Cleared || l.Kind != "" || l.ResetsAt != nil || !l.Since.Equal(since) {
		t.Fatalf("%s: limit %+v, want cleared since %v", what, l, since)
	}
}

// SM-RUN-004 (simulated: fake runner, fake Claude Code transcript and fake central): a run that hits its usage limit is
// reported limited once, its scan state survives an agent restart (the hello re-reports the same since and nothing
// is scanned again), and auto-continue at the reset is reported as cleared once. Nothing from the transcript reaches
// a message or the log.
func TestUsageLimitDetectedSetClearRestart(t *testing.T) {
	old := limitSettle
	limitSettle = 10 * time.Millisecond
	t.Cleanup(func() { limitSettle = old })
	dir := t.TempDir()
	logs := &syncBuffer{}
	a, f := ccAgent(t, dir, nil, func(o *Options) {
		o.Log = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	})
	fc := newWSCentral(t, proto.MinorUsageLimit)
	clk := clock.NewFake(time.Now().UTC().Truncate(time.Second))
	stop := startAgent(t, a, fc, clk)
	fc.next(t, proto.TypeHello)

	const runID = "run_limit"
	ccLaunch(a, runID)
	row := getRun(t, a, runID)
	if row.State != proto.RunRunning || row.SessionID == "" {
		t.Fatalf("run %+v", row)
	}
	transcript := filepath.Join(dir, "spaces", "cc", "instances", "ins_cc", "claude", "projects", "-work", row.SessionID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(transcript), 0o700); err != nil {
		t.Fatal(err)
	}
	at := clk.Now().Add(-time.Minute)
	if err := rt.AppendTranscript(transcript, rt.TypedRecord(at.Add(-time.Minute)), rt.AssistantRecord("m1", at.Add(-time.Minute))); err != nil {
		t.Fatal(err)
	}
	fc.collect(t, runID, 300*time.Millisecond) // the launch's reports
	// A turn with no limit: a report per status, none with a limit.
	f.SetStatus(row.TerminalID, "working")
	f.SetStatus(row.TerminalID, "idle")
	if rs := fc.collect(t, runID, 300*time.Millisecond); len(rs) != 2 || limitChanges(nil, rs) != 0 {
		t.Fatalf("turn without a limit: %d reports, %d limit changes", len(rs), limitChanges(nil, rs))
	}

	// Set: the status reports go out as they are, and the scan sends exactly one more report, with the limit.
	reset := at.Add(5 * time.Hour).Truncate(time.Second)
	if err := f.HitLimit(row.TerminalID, transcript, proto.LimitFiveHour, reset, at); err != nil {
		t.Fatal(err)
	}
	rs := fc.collect(t, runID, 300*time.Millisecond)
	if len(rs) != 3 || limitChanges(nil, rs) != 1 {
		t.Fatalf("limit hit: %d reports, %d limit changes", len(rs), limitChanges(nil, rs))
	}
	wantOpen(t, "set", rs[2], proto.LimitFiveHour, reset, at)
	if j := getRun(t, a, runID).Limit; j.Kind != proto.LimitFiveHour || j.Since == nil || !j.Since.Equal(at) || j.Offset == 0 {
		t.Fatalf("journal %+v", j)
	}
	// Another turn with nothing new in the transcript: the status reports carry the same limit, and no extra report.
	f.SetStatus(row.TerminalID, "working")
	f.SetStatus(row.TerminalID, "idle")
	rs = fc.collect(t, runID, 300*time.Millisecond)
	if len(rs) != 2 {
		t.Fatalf("unchanged: %d reports", len(rs))
	}
	for _, r := range rs {
		wantOpen(t, "unchanged", r, proto.LimitFiveHour, reset, at)
	}

	// Restart: the process stays up in the runner; the new agent adopts it, and its hello re-reports the same since
	// from the journal without scanning the transcript from the start.
	stop()
	offset := getRun(t, a, runID).Limit.Offset
	a2 := New(a.j, a.o)
	a2.mem.base = context.Background()
	t.Cleanup(func() { a2.drain(10 * time.Second) })
	t.Cleanup(a2.stopRecorders)
	a2.leases[ccSpace] = a.leases[ccSpace]
	startAgent(t, a2, fc, clk)
	wantOpen(t, "hello after restart", fc.helloRun(t, runID), proto.LimitFiveHour, reset, at)
	for _, r := range fc.collect(t, runID, 300*time.Millisecond) {
		wantOpen(t, "adopted", r, proto.LimitFiveHour, reset, at) // reconcile's reports
	}
	if j := getRun(t, a2, runID).Limit; j.Offset != offset {
		t.Fatalf("offset after restart %d, want %d", j.Offset, offset)
	}

	// Clear: auto-continue at the reset; one cleared report with the limit's since.
	if err := f.LimitReset(row.TerminalID, transcript, reset.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	rs = fc.collect(t, runID, 300*time.Millisecond)
	if len(rs) != 3 || limitChanges(rs[0].Limit, rs) != 1 {
		t.Fatalf("reset: %d reports, %d limit changes", len(rs), limitChanges(rs[0].Limit, rs))
	}
	wantOpen(t, "before the clear", rs[0], proto.LimitFiveHour, reset, at)
	wantCleared(t, "clear", rs[2], at)

	// The run ends: its final report keeps the cleared limit, and the end scan finds nothing new to send.
	f.Exit(row.TerminalID, 0, "exited")
	rs = fc.collect(t, runID, 300*time.Millisecond)
	if len(rs) == 0 || limitChanges(rs[0].Limit, rs) != 0 || rs[len(rs)-1].State != proto.RunExited {
		t.Fatalf("end: %+v", rs)
	}
	wantCleared(t, "ended", rs[len(rs)-1], at)
	if strings.Contains(logs.String(), rt.TranscriptMarker) {
		t.Fatal("transcript text in the log")
	}
	if !strings.Contains(logs.String(), "usage limit changed") {
		t.Fatalf("no limit change logged:\n%s", logs.String())
	}
}

// The wrap-up path (no kind, no reset) is reported as an unknown limit, and a minor-11 central never gets a limit.
func TestUsageLimitWrapUpAndOldCentral(t *testing.T) {
	old := limitSettle
	limitSettle = 10 * time.Millisecond
	t.Cleanup(func() { limitSettle = old })
	for _, minor := range []int{proto.MinorUsageLimit, proto.MinorUsageLimit - 1} {
		dir := t.TempDir()
		a, f := ccAgent(t, dir, nil, nil)
		fc := newWSCentral(t, minor)
		clk := clock.NewFake(time.Now().UTC().Truncate(time.Second))
		startAgent(t, a, fc, clk)
		fc.next(t, proto.TypeHello)
		ccLaunch(a, "run_wrap")
		row := getRun(t, a, "run_wrap")
		transcript := filepath.Join(dir, "spaces", "cc", "instances", "ins_cc", "claude", "projects", "-work", row.SessionID+".jsonl")
		if err := os.MkdirAll(filepath.Dir(transcript), 0o700); err != nil {
			t.Fatal(err)
		}
		fc.collect(t, "run_wrap", 300*time.Millisecond)
		at := clk.Now().Add(-time.Minute)
		f.SetStatus(row.TerminalID, "working")
		if err := rt.AppendTranscript(transcript, rt.TypedRecord(at), rt.WrapUpRecord(at), rt.AssistantRecord("grace", at), rt.NoticeRecord(at)); err != nil {
			t.Fatal(err)
		}
		f.SetStatus(row.TerminalID, "idle")
		rs := fc.collect(t, "run_wrap", 300*time.Millisecond)
		if j := getRun(t, a, "run_wrap").Limit; j.Kind != proto.LimitUnknown {
			t.Fatalf("minor %d: journal %+v", minor, j)
		}
		if minor < proto.MinorUsageLimit {
			if len(rs) != 2 || limitChanges(nil, rs) != 0 {
				t.Fatalf("minor %d: %d reports, %d limit changes", minor, len(rs), limitChanges(nil, rs))
			}
			continue
		}
		if len(rs) != 3 || limitChanges(nil, rs) != 1 {
			t.Fatalf("wrap-up: %d reports, %d limit changes", len(rs), limitChanges(nil, rs))
		}
		if l := rs[2].Limit; l.Kind != proto.LimitUnknown || l.ResetsAt != nil || l.Cleared || !l.Since.Equal(at) {
			t.Fatalf("wrap-up limit %+v", l)
		}
	}
}
