package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	rt "github.com/aurimasniekis/go-spaceman/internal/runner/runnertest"
	"github.com/aurimasniekis/go-spaceman/internal/store"
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

// collectAtLeast waits (up to a generous cap, not a fixed sleep) for n of runID's reports, then returns them with any
// that follow until none has come for quiet, so a late extra report still fails the exact-count checks. A loaded
// -race run can take far longer than a quiet window to produce the first one.
func (fc *wsCentral) collectAtLeast(t *testing.T, runID string, n int, quiet time.Duration) []proto.RunReport {
	t.Helper()
	var out []proto.RunReport
	deadline := time.After(15 * time.Second)
	for len(out) < n {
		select {
		case env := <-fc.msgs:
			noTranscriptText(t, env)
			var r proto.RunReport
			if env.Type == proto.TypeRunReport && env.Decode(&r) == nil && r.RunID == runID {
				out = append(out, r)
			}
		case <-deadline:
			return out
		}
	}
	return append(out, fc.collect(t, runID, quiet)...)
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
	ccLaunchEarlier(t, a, runID)
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
	fc.collectAtLeast(t, runID, 2, 300*time.Millisecond) // the launch's reports
	// A turn with no limit: a report per status, none with a limit.
	f.SetStatus(row.TerminalID, "working")
	f.SetStatus(row.TerminalID, "idle")
	if rs := fc.collectAtLeast(t, runID, 2, 300*time.Millisecond); len(rs) != 2 || limitChanges(nil, rs) != 0 {
		t.Fatalf("turn without a limit: %d reports, %d limit changes", len(rs), limitChanges(nil, rs))
	}

	// Set: the status reports go out as they are, and the scan sends exactly one more report, with the limit.
	reset := at.Add(5 * time.Hour).Truncate(time.Second)
	if err := f.HitLimit(row.TerminalID, transcript, proto.LimitFiveHour, reset, at); err != nil {
		t.Fatal(err)
	}
	rs := fc.collectAtLeast(t, runID, 3, 300*time.Millisecond)
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
	rs = fc.collectAtLeast(t, runID, 2, 300*time.Millisecond)
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
	reconciled := make(chan struct{}, 4)
	a2.snapshotReconciled = func() {
		select {
		case reconciled <- struct{}{}:
		default:
		}
	}
	startAgent(t, a2, fc, clk)
	wantOpen(t, "hello after restart", fc.helloRun(t, runID), proto.LimitFiveHour, reset, at)
	// The monitor's snapshot reconcile of an adopted run may or may not send one report after the hello (it races
	// the hello), but never more, and any it sends repeats the limit still open and the run still running.
	// Wait for that reconcile to finish first, so nothing of it can land in the clear step below.
	select {
	case <-reconciled:
	case <-time.After(5 * time.Second):
		t.Fatal("the monitor's snapshot reconcile did not finish")
	}
	rs = fc.collect(t, runID, 300*time.Millisecond)
	if len(rs) > 1 {
		t.Fatalf("adopted run: %d reports after the hello, want at most 1", len(rs))
	}
	for _, r := range rs {
		wantOpen(t, "adopted reconcile", r, proto.LimitFiveHour, reset, at)
		if r.State != "running" {
			t.Fatalf("adopted reconcile: state %q, want running", r.State)
		}
	}
	if j := getRun(t, a2, runID).Limit; j.Offset != offset {
		t.Fatalf("offset after restart %d, want %d", j.Offset, offset)
	}

	// Clear: auto-continue at the reset; one cleared report with the limit's since.
	if err := f.LimitReset(row.TerminalID, transcript, reset.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	rs = fc.collectAtLeast(t, runID, 3, 300*time.Millisecond)
	if len(rs) != 3 || limitChanges(rs[0].Limit, rs) != 1 {
		t.Fatalf("reset: %d reports, %d limit changes", len(rs), limitChanges(rs[0].Limit, rs))
	}
	wantOpen(t, "before the clear", rs[0], proto.LimitFiveHour, reset, at)
	wantCleared(t, "clear", rs[2], at)

	// The run ends: its final report keeps the cleared limit, and the end scan finds nothing new to send.
	f.Exit(row.TerminalID, 0, "exited")
	rs = fc.collectAtLeast(t, runID, 1, 300*time.Millisecond)
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
		ccLaunchEarlier(t, a, "run_wrap")
		row := getRun(t, a, "run_wrap")
		transcript := filepath.Join(dir, "spaces", "cc", "instances", "ins_cc", "claude", "projects", "-work", row.SessionID+".jsonl")
		if err := os.MkdirAll(filepath.Dir(transcript), 0o700); err != nil {
			t.Fatal(err)
		}
		fc.collectAtLeast(t, "run_wrap", 2, 300*time.Millisecond)
		at := clk.Now().Add(-time.Minute)
		f.SetStatus(row.TerminalID, "working")
		if err := rt.AppendTranscript(transcript, rt.TypedRecord(at), rt.WrapUpRecord(at), rt.AssistantRecord("grace", at), rt.NoticeRecord(at)); err != nil {
			t.Fatal(err)
		}
		f.SetStatus(row.TerminalID, "idle")
		want := 3 // working, idle, and the limit
		if minor < proto.MinorUsageLimit {
			want = 2
		}
		rs := fc.collectAtLeast(t, "run_wrap", want, 300*time.Millisecond)
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

// A run report that comes between the hello and the welcome (the restarted agent's adopt/reconcile) must not go out
// before the minor is negotiated: it would be stripped of its limit, and central would see the limit unset. It is
// held and re-sent, with the limit, after the welcome.
func TestUsageLimitReportBeforeWelcomeIsHeld(t *testing.T) {
	dir := t.TempDir()
	a, _ := ccAgent(t, dir, nil, nil)
	at := time.Now().UTC().Truncate(time.Second).Add(-time.Minute)
	reset := at.Add(5 * time.Hour)
	hold := make(chan struct{})
	fc := newHeldWSCentral(t, proto.MinorUsageLimit, hold)
	startAgent(t, a, fc, clock.NewFake(time.Now().UTC().Truncate(time.Second)))
	fc.next(t, proto.TypeHello) // sent: the connection is set, the welcome is held
	ccLaunchEarlier(t, a, "run_held")
	fc.collectAtLeast(t, "run_held", 2, 100*time.Millisecond)
	if err := a.j.setRunLimit(context.Background(), "run_held", runLimit{Kind: proto.LimitFiveHour, ResetsAt: &reset, Since: &at, Offset: 1}); err != nil {
		t.Fatal(err)
	}
	a.report(context.Background(), getRun(t, a, "run_held"))
	if rs := fc.collect(t, "run_held", 200*time.Millisecond); len(rs) != 0 {
		t.Fatalf("%d reports before the welcome: %+v", len(rs), rs[0].Limit)
	}
	close(hold)
	rs := fc.collectAtLeast(t, "run_held", 1, 500*time.Millisecond)
	if len(rs) == 0 {
		t.Fatal("no report after the welcome")
	}
	for _, r := range rs {
		wantOpen(t, "after welcome", r, proto.LimitFiveHour, reset, at)
	}
}

// A limit the transcript scan finds between the hello and the welcome (the hello is already built) is held and sent
// after the welcome, not dropped.
func TestUsageLimitScannedBeforeWelcomeIsSentAfter(t *testing.T) {
	old := limitSettle
	limitSettle = 10 * time.Millisecond
	t.Cleanup(func() { limitSettle = old })
	dir := t.TempDir()
	a, _ := ccAgent(t, dir, nil, nil)
	hold := make(chan struct{})
	fc := newHeldWSCentral(t, proto.MinorUsageLimit, hold)
	clk := clock.NewFake(time.Now().UTC().Truncate(time.Second))
	startAgent(t, a, fc, clk)
	fc.next(t, proto.TypeHello)
	ccLaunchEarlier(t, a, "run_scan")
	// The launch's own report is held too, and its re-send would read the limit from the journal: forget it, so only
	// the scan can send the limit.
	a.mu.Lock()
	if _, ok := a.heldReports["run_scan"]; !ok {
		a.mu.Unlock()
		t.Fatal("the launch report was not held")
	}
	delete(a.heldReports, "run_scan")
	a.mu.Unlock()
	row := getRun(t, a, "run_scan")
	transcript := filepath.Join(dir, "spaces", "cc", "instances", "ins_cc", "claude", "projects", "-work", row.SessionID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(transcript), 0o700); err != nil {
		t.Fatal(err)
	}
	at := clk.Now().Add(-time.Minute)
	reset := at.Add(5 * time.Hour).Truncate(time.Second)
	if err := rt.AppendTranscript(transcript, rt.TypedRecord(at.Add(-time.Minute)), rt.AssistantRecord("m1", at.Add(-time.Minute))); err != nil {
		t.Fatal(err)
	}
	// The limit record alone, with no status change: a status report would hold the run too.
	if err := rt.AppendTranscript(transcript, rt.TypedRecord(at), rt.LimitRecord(proto.LimitFiveHour, reset, at), rt.NoticeRecord(at)); err != nil {
		t.Fatal(err)
	}
	a.limitScan(context.Background(), "run_scan", false)
	if j := getRun(t, a, "run_scan").Limit; j.Kind != proto.LimitFiveHour {
		t.Fatalf("limit not journaled: %+v", j)
	}
	if rs := fc.collect(t, "run_scan", 200*time.Millisecond); len(rs) != 0 {
		t.Fatalf("%d reports before the welcome", len(rs))
	}
	close(hold)
	rs := fc.collectAtLeast(t, "run_scan", 1, 500*time.Millisecond)
	if len(rs) == 0 {
		t.Fatal("no report after the welcome")
	}
	wantOpen(t, "after welcome", rs[len(rs)-1], proto.LimitFiveHour, reset, at)
}

// A run end reported between the hello snapshot and the connection being set is not in the hello and has no
// connection to go out on: it is held and sent after the welcome, or central keeps the run live until the next reconnect.
func TestRunEndBetweenHelloSnapshotAndConnIsSentAfterWelcome(t *testing.T) {
	dir := t.TempDir()
	a, _ := ccAgent(t, dir, nil, nil)
	fc := newWSCentral(t, proto.MinorUsageLimit)
	var once sync.Once
	a.helloStage = func() {
		once.Do(func() {
			ccLaunchEarlier(t, a, "run_gap")
			row := getRun(t, a, "run_gap")
			row.State = proto.RunExited
			if err := a.j.updateRun(context.Background(), row); err != nil {
				t.Error(err)
			}
			a.report(context.Background(), getRun(t, a, "run_gap"))
		})
	}
	startAgent(t, a, fc, clock.NewFake(time.Now().UTC().Truncate(time.Second)))
	fc.next(t, proto.TypeHello)
	rs := fc.collectAtLeast(t, "run_gap", 1, 500*time.Millisecond)
	if len(rs) == 0 || rs[len(rs)-1].State != proto.RunExited {
		t.Fatalf("run end not reported after the welcome: %d reports", len(rs))
	}
}

// A limit the scan finds in the same window (before the connection is set) is held too, not dropped.
func TestUsageLimitScannedBeforeConnIsHeld(t *testing.T) {
	a, _ := ccAgent(t, t.TempDir(), nil, nil)
	a.mu.Lock()
	a.preConn = true
	a.mu.Unlock()
	if !a.holdingReports() {
		t.Fatal("the pre-connection window does not hold reports")
	}
}

// A limit the scan finds after the hello snapshot and before the connection is set, on a first connect (the minor is not
// negotiated yet), is held and sent after the welcome, not dropped.
func TestUsageLimitScannedInPreConnWindowIsSentAfterWelcome(t *testing.T) {
	old := limitSettle
	limitSettle = 10 * time.Millisecond
	t.Cleanup(func() { limitSettle = old })
	dir := t.TempDir()
	a, _ := ccAgent(t, dir, nil, nil)
	fc := newWSCentral(t, proto.MinorUsageLimit)
	clk := clock.NewFake(time.Now().UTC().Truncate(time.Second))
	at := clk.Now().Add(-time.Minute)
	reset := at.Add(5 * time.Hour).Truncate(time.Second)
	var once sync.Once
	a.helloStage = func() {
		once.Do(func() {
			if a.protocolAtLeast(proto.MinorUsageLimit) {
				t.Error("the minor is already negotiated: not the first-connect case")
			}
			ccLaunchEarlier(t, a, "run_pre")
			// The launch's own held report would read the limit from the journal: forget it, so only the scan can send it.
			a.mu.Lock()
			delete(a.heldReports, "run_pre")
			a.mu.Unlock()
			row := getRun(t, a, "run_pre")
			transcript := filepath.Join(dir, "spaces", "cc", "instances", "ins_cc", "claude", "projects", "-work", row.SessionID+".jsonl")
			if err := os.MkdirAll(filepath.Dir(transcript), 0o700); err != nil {
				t.Error(err)
				return
			}
			if err := rt.AppendTranscript(transcript, rt.TypedRecord(at.Add(-time.Minute)), rt.AssistantRecord("m1", at.Add(-time.Minute))); err != nil {
				t.Error(err)
			}
			if err := rt.AppendTranscript(transcript, rt.TypedRecord(at), rt.LimitRecord(proto.LimitFiveHour, reset, at), rt.NoticeRecord(at)); err != nil {
				t.Error(err)
			}
			a.limitScan(context.Background(), "run_pre", false)
			if j := getRun(t, a, "run_pre").Limit; j.Kind != proto.LimitFiveHour {
				t.Errorf("limit not journaled: %+v", j)
			}
		})
	}
	startAgent(t, a, fc, clk)
	fc.next(t, proto.TypeHello)
	rs := fc.collectAtLeast(t, "run_pre", 1, 500*time.Millisecond)
	if len(rs) == 0 {
		t.Fatal("no report after the welcome")
	}
	wantOpen(t, "after welcome", rs[len(rs)-1], proto.LimitFiveHour, reset, at)
}

// A failed hello snapshot must not leave the pre-connection hold set: a later report would be held forever.
func TestPreConnHoldClearedWhenHelloRunsFails(t *testing.T) {
	a, _ := ccAgent(t, t.TempDir(), nil, nil)
	fc := newWSCentral(t, proto.MinorUsageLimit)
	a.o.Clock, a.o.CentralURL, a.o.Credential = clock.NewFake(time.Now().UTC()), fc.srv.URL, "spaceman_nd_test"
	a.o.BridgeSocket = filepath.Join(t.TempDir(), "b.sock")
	if err := a.j.Close(); err != nil {
		t.Fatal(err)
	}
	if err := a.session(context.Background()); err == nil {
		t.Fatal("session succeeded with a closed journal: helloRuns did not fail")
	}
	if a.holdingReports() {
		t.Fatal("a failed hello snapshot left the pre-connection hold set")
	}
}

// A run that continues at the reset and keeps working (a long turn that never goes idle) has its limit cleared while
// the turn runs, not when it ends: the idle scan alone left it at "usage limit" for as long as the turn lasted.
func TestUsageLimitClearedWhileRunStaysWorking(t *testing.T) {
	oldSettle, oldPoll := limitSettle, limitPoll
	limitSettle, limitPoll = 10*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { limitSettle, limitPoll = oldSettle, oldPoll })
	dir := t.TempDir()
	a, f := ccAgent(t, dir, nil, nil)
	fc := newWSCentral(t, proto.MinorUsageLimit)
	clk := clock.NewFake(time.Now().UTC().Truncate(time.Second))
	startAgent(t, a, fc, clk)
	fc.next(t, proto.TypeHello)

	const runID = "run_limit_working"
	ccLaunchEarlier(t, a, runID)
	row := getRun(t, a, runID)
	transcript := filepath.Join(dir, "spaces", "cc", "instances", "ins_cc", "claude", "projects", "-work", row.SessionID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(transcript), 0o700); err != nil {
		t.Fatal(err)
	}
	at := clk.Now().Add(-time.Minute)
	if err := rt.AppendTranscript(transcript, rt.TypedRecord(at.Add(-time.Minute)), rt.AssistantRecord("m1", at.Add(-time.Minute))); err != nil {
		t.Fatal(err)
	}
	fc.collectAtLeast(t, runID, 2, 300*time.Millisecond) // the launch's reports
	reset := at.Add(5 * time.Hour).Truncate(time.Second)
	if err := f.HitLimit(row.TerminalID, transcript, proto.LimitFiveHour, reset, at); err != nil {
		t.Fatal(err)
	}
	rs := fc.collectAtLeast(t, runID, 1, 300*time.Millisecond)
	if len(rs) == 0 {
		t.Fatal("no report after the limit was hit")
	}
	wantOpen(t, "set", rs[len(rs)-1], proto.LimitFiveHour, reset, at)

	// The reset passes: the run starts its auto-continuation turn and keeps working; the status never goes idle.
	f.SetStatus(row.TerminalID, "working")
	// The scan the working status kicked finds nothing yet; the continuation is written after it, so only the
	// polling scan can see it while the turn is still running.
	time.Sleep(150 * time.Millisecond)
	if err := rt.AppendTranscript(transcript, rt.NoticeRecord(reset), rt.AutoContinueRecord(reset), rt.AssistantRecord("msg_continue", reset.Add(time.Second))); err != nil {
		t.Fatal(err)
	}
	rs = fc.collectAtLeast(t, runID, 1, 500*time.Millisecond)
	if len(rs) == 0 {
		t.Fatal("no report after the reset")
	}
	wantCleared(t, "cleared while working", rs[len(rs)-1], at)
	if got := getRun(t, a, runID); got.Status != "working" || got.Limit.Kind != "" {
		t.Fatalf("run %+v", got)
	}
}

// A node agent that restarts and adopts a run that is already working with an open limit starts the polling scan
// itself: observe sees no status or turn change on an adopt, so nothing else would.
func TestUsageLimitClearedForRunAdoptedWorking(t *testing.T) {
	oldSettle, oldPoll := limitSettle, limitPoll
	limitSettle, limitPoll = 10*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { limitSettle, limitPoll = oldSettle, oldPoll })
	dir := t.TempDir()
	a, f := ccAgent(t, dir, nil, nil)
	fc := newWSCentral(t, proto.MinorUsageLimit)
	clk := clock.NewFake(time.Now().UTC().Truncate(time.Second))
	stop := startAgent(t, a, fc, clk)
	fc.next(t, proto.TypeHello)

	const runID = "run_limit_adopt_working"
	ccLaunchEarlier(t, a, runID)
	row := getRun(t, a, runID)
	transcript := filepath.Join(dir, "spaces", "cc", "instances", "ins_cc", "claude", "projects", "-work", row.SessionID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(transcript), 0o700); err != nil {
		t.Fatal(err)
	}
	at := clk.Now().Add(-time.Minute)
	if err := rt.AppendTranscript(transcript, rt.TypedRecord(at.Add(-time.Minute)), rt.AssistantRecord("m1", at.Add(-time.Minute))); err != nil {
		t.Fatal(err)
	}
	fc.collectAtLeast(t, runID, 2, 300*time.Millisecond) // the launch's reports
	reset := at.Add(5 * time.Hour).Truncate(time.Second)
	if err := f.HitLimit(row.TerminalID, transcript, proto.LimitFiveHour, reset, at); err != nil {
		t.Fatal(err)
	}
	rs := fc.collectAtLeast(t, runID, 1, 300*time.Millisecond)
	if len(rs) == 0 {
		t.Fatal("no report after the limit was hit")
	}
	wantOpen(t, "set", rs[len(rs)-1], proto.LimitFiveHour, reset, at)
	// The run starts working again; the agent stops before the continuation's records are written.
	f.SetStatus(row.TerminalID, "working")
	fc.collectAtLeast(t, runID, 1, 300*time.Millisecond)
	stop()
	if got := getRun(t, a, runID); got.Status != "working" || got.Limit.Kind == "" {
		t.Fatalf("run before restart %+v", got)
	}
	if err := rt.AppendTranscript(transcript, rt.NoticeRecord(reset), rt.AutoContinueRecord(reset), rt.AssistantRecord("msg_continue", reset.Add(time.Second))); err != nil {
		t.Fatal(err)
	}

	a2 := New(a.j, a.o)
	a2.mem.base = context.Background()
	t.Cleanup(func() { a2.drain(10 * time.Second) })
	t.Cleanup(a2.stopRecorders)
	a2.leases[ccSpace] = a.leases[ccSpace]
	startAgent(t, a2, fc, clk)
	hello := fc.helloRun(t, runID)
	// The scan may run before or after the hello snapshot: the journal and the next report both show the clear.
	deadline := time.Now().Add(3 * time.Second)
	for getRun(t, a2, runID).Limit.Kind != "" && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	got := getRun(t, a2, runID)
	if got.Status != "working" || got.Limit.Kind != "" || got.Limit.Since == nil || !got.Limit.Since.Equal(at) {
		t.Fatalf("run %+v (hello limit %+v)", got, hello.Limit)
	}
}

// A limited run that is idle is not polled: the polling scan belongs to working runs, and an idle run's next turn
// start (a status change) kicks its own scan.
func TestUsageLimitIdleRunIsNotPolled(t *testing.T) {
	oldSettle, oldPoll := limitSettle, limitPoll
	limitSettle, limitPoll = 10*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { limitSettle, limitPoll = oldSettle, oldPoll })
	dir := t.TempDir()
	a, f := ccAgent(t, dir, nil, nil)
	fc := newWSCentral(t, proto.MinorUsageLimit)
	clk := clock.NewFake(time.Now().UTC().Truncate(time.Second))
	startAgent(t, a, fc, clk)
	fc.next(t, proto.TypeHello)

	const runID = "run_limit_idle"
	ccLaunchEarlier(t, a, runID)
	row := getRun(t, a, runID)
	transcript := filepath.Join(dir, "spaces", "cc", "instances", "ins_cc", "claude", "projects", "-work", row.SessionID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(transcript), 0o700); err != nil {
		t.Fatal(err)
	}
	at := clk.Now().Add(-time.Minute)
	if err := rt.AppendTranscript(transcript, rt.TypedRecord(at.Add(-time.Minute)), rt.AssistantRecord("m1", at.Add(-time.Minute))); err != nil {
		t.Fatal(err)
	}
	fc.collectAtLeast(t, runID, 2, 300*time.Millisecond) // the launch's reports
	f.SetStatus(row.TerminalID, "working")
	f.SetStatus(row.TerminalID, "idle")
	fc.collectAtLeast(t, runID, 2, 300*time.Millisecond)
	reset := at.Add(5 * time.Hour).Truncate(time.Second)
	if err := f.HitLimit(row.TerminalID, transcript, proto.LimitFiveHour, reset, at); err != nil {
		t.Fatal(err)
	}
	rs := fc.collectAtLeast(t, runID, 1, 300*time.Millisecond)
	if len(rs) == 0 {
		t.Fatal("no report after the limit was hit")
	}
	wantOpen(t, "set", rs[len(rs)-1], proto.LimitFiveHour, reset, at)
	if got := getRun(t, a, runID); got.Status != "idle" || got.Limit.Kind == "" {
		t.Fatalf("run %+v", got)
	}
	// The continuation's records appear while the run stays idle: no scan is armed, so the limit stays open.
	if err := rt.AppendTranscript(transcript, rt.NoticeRecord(reset), rt.AutoContinueRecord(reset), rt.AssistantRecord("msg_continue", reset.Add(time.Second))); err != nil {
		t.Fatal(err)
	}
	if rs := fc.collect(t, runID, 500*time.Millisecond); len(rs) != 0 {
		t.Fatalf("an idle run was scanned: %d reports, last %+v", len(rs), rs[len(rs)-1].Limit)
	}
	if got := getRun(t, a, runID); got.Limit.Kind == "" {
		t.Fatalf("limit cleared without a scan: %+v", got.Limit)
	}
}

// A limit record older than the run's launch (the transcript a run shares with an earlier run holds that run's limit) is
// read past: limitScan passes the launch time to the scan. Unlike the other limit tests, this run is not backdated, so
// dropping that cutoff would flag it.
func TestUsageLimitRecordBeforeLaunchIsNotFlagged(t *testing.T) {
	old := limitSettle
	limitSettle = 10 * time.Millisecond
	t.Cleanup(func() { limitSettle = old })
	dir := t.TempDir()
	a, _ := ccAgent(t, dir, nil, nil)
	fc := newWSCentral(t, proto.MinorUsageLimit)
	clk := clock.NewFake(time.Now().UTC().Truncate(time.Second))
	startAgent(t, a, fc, clk)
	fc.next(t, proto.TypeHello)

	const runID = "run_limit_prelaunch"
	ccLaunch(a, runID)
	row := getRun(t, a, runID)
	launched := time.UnixMicro(row.CreatedAt).UTC()
	transcript := filepath.Join(dir, "spaces", "cc", "instances", "ins_cc", "claude", "projects", "-work", row.SessionID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(transcript), 0o700); err != nil {
		t.Fatal(err)
	}
	before := launched.Add(-time.Minute).Truncate(time.Second)
	if err := rt.AppendTranscript(transcript, rt.TypedRecord(before), rt.LimitRecord(proto.LimitFiveHour, before.Add(5*time.Hour), before), rt.NoticeRecord(before)); err != nil {
		t.Fatal(err)
	}
	a.limitScan(context.Background(), runID, false)
	if got := getRun(t, a, runID).Limit; got.Kind != "" {
		t.Fatalf("a limit record from before the launch flagged the run: %+v", got)
	}
	if rs := fc.collect(t, runID, 300*time.Millisecond); limitChanges(nil, rs) != 0 {
		t.Fatalf("a limit record from before the launch was reported: %d reports", len(rs))
	}

	// Control: a limit record written after the launch is flagged by the same scan, so the check above is not vacuous.
	after := launched.Add(time.Minute).Truncate(time.Second)
	reset := after.Add(5 * time.Hour).Truncate(time.Second)
	if err := rt.AppendTranscript(transcript, rt.TypedRecord(after), rt.LimitRecord(proto.LimitFiveHour, reset, after), rt.NoticeRecord(after)); err != nil {
		t.Fatal(err)
	}
	a.limitScan(context.Background(), runID, false)
	rs := fc.collectAtLeast(t, runID, 1, 300*time.Millisecond)
	if len(rs) == 0 {
		t.Fatal("no report for a limit record after the launch")
	}
	wantOpen(t, "after the launch", rs[len(rs)-1], proto.LimitFiveHour, reset, after)
}

// ccLaunchEarlier launches a run and journals it as started an hour ago: these tests write limit records timestamped
// shortly before the test's own "now", which a run that started just now would rightly treat as older than itself.
// A failed backdate fails the test: otherwise the run would start now and the test would check the wrong thing.
func ccLaunchEarlier(t *testing.T, a *Agent, runID string) {
	t.Helper()
	ccLaunch(a, runID)
	if err := a.j.s.W().TxNamed(context.Background(), "test.backdate_run", func(tx *store.Tx) error {
		_, err := tx.SQL.ExecContext(context.Background(), `UPDATE runs SET created_at = created_at - ? WHERE run_id = ?`, time.Hour.Microseconds(), runID)
		return err
	}); err != nil {
		t.Errorf("backdating %s: %v", runID, err)
	}
}
