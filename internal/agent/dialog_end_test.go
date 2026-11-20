package agent

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// openOccurrence is a flagged occurrence of a run, as dialogFlag would record it.
func openOccurrence(run, space string) proto.RunDialog {
	return proto.RunDialog{ID: ids.New(ids.Dialog), RunID: run, SpaceID: space, Generation: 1, Outcome: proto.DialogBlocked,
		Dialog: "unknown", Signal: "screen_prompt", DetectedAt: time.Now().UTC()}
}

// pendingOutcomes lists the run.dialog messages an agent without a central connection kept for the next welcome.
func pendingOutcomes(a *Agent) []string {
	a.dlg.mu.Lock()
	defer a.dlg.mu.Unlock()
	var out []string
	for _, d := range a.dlg.pending {
		out = append(out, d.Outcome+":"+d.ID)
	}
	return out
}

// A run's end tells central its flag is over on every path that ends a run, also the ones that never observe its exit
// (a failed launch, the runner reconcile at an agent restart): central would otherwise keep the occurrence open.
func TestDialogClearedOnEveryRunEndPath(t *testing.T) {
	for _, tc := range []struct {
		name string
		// inMemory: the flag is in the watch (a launch that fails in this process); otherwise it is only in the saved file
		// (an agent restart: the watch is gone).
		inMemory bool
		end      func(t *testing.T, a *Agent)
	}{
		{"launch fails", true, func(t *testing.T, a *Agent) {
			a.launch(context.Background(), proto.Command{CommandID: "cmd_" + adoptWaitRun, Kind: proto.CmdLaunch, SpaceID: ccSpace, RunID: adoptWaitRun, Generation: 1},
				proto.LaunchPayload{Harness: "no-such-harness", Kind: "worker", Role: "implementor", InstanceID: adoptWaitInst})
			if r := getRun(t, a, adoptWaitRun); r.State != proto.RunFailed {
				t.Fatalf("state %s, want failed", r.State)
			}
		}},
		{"reconcile finds the program gone", false, func(t *testing.T, a *Agent) {
			row := getRun(t, a, adoptWaitRun)
			row.State, row.TerminalID = proto.RunRunning, "term_gone"
			if err := a.j.updateRun(context.Background(), row); err != nil {
				t.Fatal(err)
			}
			if err := a.reconcileRunner(context.Background()); err != nil {
				t.Fatal(err)
			}
			if r := getRun(t, a, adoptWaitRun); r.State != proto.RunLost {
				t.Fatalf("state %s, want lost", r.State)
			}
		}},
		{"reconcile finds a launch that never started", false, func(t *testing.T, a *Agent) {
			if err := a.reconcileRunner(context.Background()); err != nil {
				t.Fatal(err)
			}
			if r := getRun(t, a, adoptWaitRun); r.State != proto.RunFailed {
				t.Fatalf("state %s, want failed", r.State)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := ccAgent(t, t.TempDir(), nil, nil)
			ccAgentSeedRun(t, a)
			d := openOccurrence(adoptWaitRun, ccSpace)
			if tc.inMemory {
				a.dlg.mu.Lock()
				a.dlg.watchLocked(adoptWaitRun).blocked = &d
				a.dlg.mu.Unlock()
			} else {
				if err := os.MkdirAll(a.runDir(adoptWaitRun), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := writeJSONFile(filepath.Join(a.runDir(adoptWaitRun), dialogOpenFile), d); err != nil {
					t.Fatal(err)
				}
			}
			tc.end(t, a)
			if got := pendingOutcomes(a); !slices.Equal(got, []string{proto.DialogCleared + ":" + d.ID}) {
				t.Fatalf("sent %v, want one cleared of the open occurrence", got)
			}
			if _, err := os.Stat(filepath.Join(a.runDir(adoptWaitRun), dialogOpenFile)); err == nil {
				t.Fatal("the open occurrence is still saved")
			}
			a.dlg.mu.Lock()
			defer a.dlg.mu.Unlock()
			if a.dlg.runs[adoptWaitRun] != nil {
				t.Fatal("the watch of an ended run was kept")
			}
		})
	}
}

// A clear from the observe path waits for a flag in progress (the run's dialog lock), so central never gets cleared
// before the blocked it ends and keeps the occurrence open.
func TestDialogClearWaitsForAFlagInProgress(t *testing.T) {
	e := newDialogEnv(t, nil)
	d := openOccurrence(e.run, e.space)
	e.a.dlg.mu.Lock()
	e.a.dlg.watchLocked(e.run).blocked = &d
	e.a.dlg.mu.Unlock()
	row := getRun(t, e.a, e.run)
	prev := row
	row.Status = "working"
	unlock := e.a.dialogLock(e.run) // the flag, mid-way
	var done atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		e.a.dialogObserved(context.Background(), prev, row)
		done.Store(true)
	}()
	time.Sleep(200 * time.Millisecond)
	if done.Load() || !e.a.dialogBlocked(e.run) {
		t.Fatal("the clear did not wait for the flag in progress")
	}
	unlock()
	wg.Wait()
	if e.a.dialogBlocked(e.run) {
		t.Fatal("the flag was not cleared")
	}
	waitFor(t, "cleared sent", func() bool { return slices.Equal(e.outcomes(), []string{"cleared:unknown"}) })
}

// A run that ends while its screen is read with a known dialog on it: nothing is typed into it, nothing is sent, and the
// watch the end dropped stays gone. The journal re-check after the read stops it; the Known branch's own nil guard is
// defence in depth that no test reaches.
func TestDialogKnownDialogRunEndsDuringRead(t *testing.T) {
	e := newDialogEnv(t, nil)
	var once sync.Once
	var read atomic.Bool
	e.f.OnReadScreen = func(string) {
		once.Do(func() {
			e.endRun()
			e.a.dialogForget(e.run) // what the end of a run does (runEndHooks)
			read.Store(true)
		})
	}
	e.show(fixture(t, "claude-2.1.328-code-mode.txt"))
	waitFor(t, "read", read.Load)
	quiet()
	if got := e.f.KeysPressed(e.term); len(got) != 0 {
		t.Fatalf("keys typed for a run that ended: %v", got)
	}
	if got := e.outcomes(); len(got) != 0 || e.watching() {
		t.Fatalf("outcomes %v, watching %v", got, e.watching())
	}
}

// A run that ends while a known dialog is being answered: the answer is logged (the keys were typed), and the write of
// "answered" does not recreate the watch the end dropped (the answered-write guard).
func TestDialogKnownDialogRunEndsDuringAnswer(t *testing.T) {
	e := newDialogEnv(t, nil)
	var once sync.Once
	var typed atomic.Bool
	e.f.OnKeys = func(string, []string) {
		once.Do(func() {
			e.endRun()
			e.a.dialogForget(e.run)
			typed.Store(true)
		})
	}
	e.show(fixture(t, "claude-2.1.328-code-mode.txt"))
	waitFor(t, "answered", typed.Load)
	waitFor(t, "answer logged", func() bool { return len(e.dialogs()) == 1 })
	quiet()
	if e.watching() {
		t.Fatal("the answer recreated the watch of a run that ended")
	}
	if got := e.outcomes(); !slices.Equal(got, []string{"answered:claude.code_mode_optin"}) {
		t.Fatalf("outcomes %v", got)
	}
}

// A run that ended while its screen was read is not flagged, even when its end hooks have not dropped the watch yet
// (they wait for the run's dialog lock, which the check holds).
func TestDialogEndedDuringReadIsNotFlaggedBeforeForget(t *testing.T) {
	e := newDialogEnv(t, nil)
	var once sync.Once
	var read atomic.Bool
	e.f.OnReadScreen = func(string) {
		once.Do(func() {
			e.endRun() // the watch stays: runEndHooks would wait for the lock
			read.Store(true)
		})
	}
	e.show(fixture(t, "claude-unknown.txt"))
	waitFor(t, "read", read.Load)
	quiet()
	if got := e.outcomes(); len(got) != 0 {
		t.Fatalf("an ended run was flagged: %v", got)
	}
}

// A blocked that could not be sent (the connection dropped) is still queued when the flag clears: central gets blocked
// then cleared, in that order, never the cleared first (which would leave the occurrence open).
func TestDialogClearedFollowsAQueuedBlockedAcrossAReconnect(t *testing.T) {
	e := newDialogEnv(t, nil)
	d := openOccurrence(e.run, e.space)
	e.a.dlg.mu.Lock()
	e.a.dlg.watchLocked(e.run).blocked = &d
	e.a.dlg.pending = []proto.RunDialog{d} // the blocked send failed while the connection was down
	e.a.dlg.mu.Unlock()
	row := getRun(t, e.a, e.run)
	row.Status = "working"
	e.a.dialogObserved(context.Background(), row, row) // the flag clears after the welcome, before the resend ran
	e.a.resendDialogs(context.Background())
	want := []string{"blocked:unknown", "cleared:unknown"}
	waitFor(t, "both sent", func() bool { return slices.Equal(e.outcomes(), want) })
	quiet()
	if got := e.outcomes(); !slices.Equal(got, want) {
		t.Fatalf("sent %v, want %v once, in order", got, want)
	}
}

// The websocket send of a dialog message happens outside the run's dialog lock: a send that is stuck does not keep
// the lock (and with it the monitor loop and the launch path) waiting.
func TestDialogSendDoesNotHoldTheRunLock(t *testing.T) {
	e := newDialogEnv(t, nil)
	d := openOccurrence(e.run, e.space)
	e.a.dlg.mu.Lock()
	e.a.dlg.watchLocked(e.run).blocked = &d
	e.a.dlg.mu.Unlock()
	row := getRun(t, e.a, e.run)
	row.Status = "working"
	e.a.dlg.sendMu.Lock() // a send in progress that does not return
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.a.dialogObserved(context.Background(), row, row)
	}()
	waitFor(t, "cleared queued", func() bool { return slices.Equal(pendingOutcomes(e.a), []string{proto.DialogCleared + ":" + d.ID}) })
	locked := make(chan struct{})
	go func() {
		defer close(locked)
		e.a.dialogLock(e.run)()
	}()
	select {
	case <-locked:
	case <-time.After(5 * time.Second):
		t.Fatal("the run's dialog lock is held while a send waits")
	}
	select {
	case <-done:
		t.Fatal("the clear returned before its message was sent")
	default:
	}
	e.a.dlg.sendMu.Unlock()
	<-done
	waitFor(t, "cleared sent", func() bool { return slices.Equal(e.outcomes(), []string{"cleared:unknown"}) })
}

// A report that stalls on its write is sent outside the run's dialog lock: the lock stays free while it waits.
func TestDialogReportSendDoesNotHoldTheRunLock(t *testing.T) {
	e := newDialogEnv(t, nil)
	d := openOccurrence(e.run, e.space)
	e.a.dlg.mu.Lock()
	e.a.dlg.watchLocked(e.run).blocked = &d
	e.a.dlg.mu.Unlock()
	row := getRun(t, e.a, e.run)
	row.Status = "working"
	e.a.limitSend.Lock() // the report's write in progress, which does not return
	released := false
	release := func() {
		if !released {
			released = true
			e.a.limitSend.Unlock()
		}
	}
	defer release()
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.a.dialogObserved(context.Background(), row, row)
	}()
	waitFor(t, "cleared queued", func() bool { return slices.Equal(pendingOutcomes(e.a), []string{proto.DialogCleared + ":" + d.ID}) })
	locked := make(chan struct{})
	go func() {
		defer close(locked)
		e.a.dialogLock(e.run)()
	}()
	select {
	case <-locked:
	case <-time.After(5 * time.Second):
		t.Fatal("the run's dialog lock is held while a report waits")
	}
	select {
	case <-done:
		t.Fatal("the clear returned before its report was sent")
	default:
	}
	release()
	<-done
}

// A queued report is built from the run's row as it is when the send turns come, not as it was when the drain began:
// a newer state sent meanwhile is not followed by the older one.
func TestDialogDrainReportReadsRowUnderTheSendLock(t *testing.T) {
	e := newDialogEnv(t, nil)
	old := getRun(t, e.a, e.run)
	base := len(e.reportsFor())
	e.a.limitSend.Lock() // another report is being sent
	released := false
	release := func() {
		if !released {
			released = true
			e.a.limitSend.Unlock()
		}
	}
	defer release()
	e.a.dialogQueueReport(old)
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.a.dialogDrain(context.Background())
	}()
	time.Sleep(200 * time.Millisecond) // the drain has taken its row (before the fix) and waits for the lock
	cur := getRun(t, e.a, e.run)
	cur.Status = "fresh"
	if err := e.a.j.updateRun(context.Background(), cur); err != nil {
		t.Fatal(err)
	}
	release()
	<-done
	// The monitor reports on its own too, so look for the fresh state anywhere, and for the journal's original state
	// (what the drain would send had it kept the row it took) nowhere.
	// A report with either state ends the wait, so a revert fails at once instead of after the timeout.
	waitFor(t, "the drain's report sent", func() bool { return sentEither(e.reportsFor()[base:], old.Status, "fresh") })
	for _, r := range e.reportsFor()[base:] {
		if r.Status == old.Status {
			t.Fatalf("the journal's original state %q was sent after the row changed: %+v", old.Status, r)
		}
	}
	waitFor(t, "fresh report sent", func() bool {
		return slices.ContainsFunc(e.reportsFor(), func(r proto.RunReport) bool { return r.Status == "fresh" })
	})
}

// sentEither is whether a report carries one of the statuses (the monitor's own reports carry others, or none).
func sentEither(rs []proto.RunReport, statuses ...string) bool {
	return slices.ContainsFunc(rs, func(r proto.RunReport) bool { return slices.Contains(statuses, r.Status) })
}

// Two observes whose reports wait on the send lock: the report goes out from the journal as it is when the lock is
// taken, not from the row the observe built before it waited, so an older state never follows a newer one.
func TestObserveReportReadsRowUnderTheSendLock(t *testing.T) {
	e := newDialogEnv(t, nil)
	ctx := context.Background()
	got, err := e.f.Get(ctx, e.term)
	if err != nil {
		t.Fatal(err)
	}
	p := *got // not a status the fake holds, so the run's monitor never observes it
	p.Status.State = "idle"
	orig := p.Status.State
	if orig == "" || getRun(t, e.a, e.run).Status == orig {
		t.Fatalf("status %q must be set and differ from the journal's", orig)
	}
	base := len(e.reportsFor())
	e.a.limitSend.Lock() // another report is being sent
	released := false
	release := func() {
		if !released {
			released = true
			e.a.limitSend.Unlock()
		}
	}
	defer release()
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.a.observe(ctx, getRun(t, e.a, e.run), p, false)
	}()
	// The observe's own row write (its older state) must land before the concurrent update below, or the observe would
	// overwrite that update with the older state it had built.
	waitFor(t, "the observe's row written", func() bool { return getRun(t, e.a, e.run).Status == orig })
	cur := getRun(t, e.a, e.run)
	cur.Status = "fresh" // a concurrent observe wrote a newer state
	if err := e.a.j.updateRun(ctx, cur); err != nil {
		t.Fatal(err)
	}
	release()
	<-done
	waitFor(t, "the observe's report sent", func() bool { return sentEither(e.reportsFor()[base:], orig, "fresh", "working") })
	for _, r := range e.reportsFor()[base:] {
		if r.Status == orig {
			t.Fatalf("the observe's own older state %q was sent: %+v", orig, r)
		}
	}
}

// A blocked whose second-phase send in resendDialogs fails (it was open, not queued) is kept in the queue, so the
// cleared that follows it still goes out after it on the next welcome.
func TestDialogResendFailureKeepsBlockedAheadOfCleared(t *testing.T) {
	e := newDialogEnv(t, nil)
	d := openOccurrence(e.run, e.space)
	e.a.dlg.mu.Lock()
	e.a.dlg.watchLocked(e.run).blocked = &d
	e.a.dlg.mu.Unlock()
	e.a.mu.Lock()
	conn := e.a.conn
	e.a.conn = nil // the connection dropped right after the welcome: the resend's send fails
	e.a.mu.Unlock()
	e.a.resendDialogs(context.Background())
	if got, want := pendingOutcomes(e.a), []string{proto.DialogBlocked + ":" + d.ID}; !slices.Equal(got, want) {
		t.Fatalf("queue %v after the failed resend, want %v", got, want)
	}
	row := getRun(t, e.a, e.run)
	row.Status = "working"
	e.a.dialogObserved(context.Background(), row, row) // the flag clears before the next welcome
	e.a.mu.Lock()
	e.a.conn = conn
	e.a.mu.Unlock()
	e.a.resendDialogs(context.Background())
	want := []string{"blocked:unknown", "cleared:unknown"}
	waitFor(t, "both sent", func() bool { return slices.Equal(e.outcomes(), want) })
	quiet()
	if got := e.outcomes(); !slices.Equal(got, want) {
		t.Fatalf("sent %v, want %v once, in order", got, want)
	}
}
