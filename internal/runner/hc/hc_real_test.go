//go:build real

package hc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/runner"
)

// Real herdr-core contract suite (SM-RUN-001/002, SM-VER-002): runs against the installed `hc` binary with an
// isolated core in a fresh directory. It never touches the operator's default core, and stops only the core it
// started. Run: go test -tags real ./internal/runner/hc/
func realCore(t *testing.T) (*Runner, CoreOptions) {
	t.Helper()
	bin, err := exec.LookPath("hc")
	if err != nil {
		t.Skip("hc not installed")
	}
	home, err := os.MkdirTemp("/tmp", "smhc-") // short path: socket limit (S-0)
	if err != nil {
		t.Fatal(err)
	}
	o := CoreOptions{Binary: bin, Session: "smtest" + filepath.Base(home)[6:], Socket: filepath.Join(home, "s.sock"), Home: home}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, err := EnsureCore(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = c.Close()
		sctx, scancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer scancel()
		if err := StopCore(sctx, o); err != nil {
			t.Logf("stop core: %v", err)
		}
		_ = os.RemoveAll(home)
	})
	if c.Hello().Core.ID == "" || !strings.HasPrefix(c.Hello().Version, "0.") {
		t.Fatalf("hello %+v", c.Hello())
	}
	return New(c), o
}

func TestRealLaunchAdoptObserveClose(t *testing.T) {
	r, _ := realCore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	labels := map[string]string{"spaceman.run": "run_test0001", "spaceman.space": "spc_test"}
	p, err := r.Start(ctx, runner.Spec{Name: "r-test0001", Argv: []string{"/bin/sh", "-c", "sleep 0.8; echo hello-spaceman; sleep 0.3; exit 7"},
		Labels: labels, Cwd: "/tmp"})
	if err != nil {
		t.Fatal(err)
	}
	if p.State != runner.StateLive || p.PID == 0 || p.Parent != "" {
		t.Fatalf("started %+v", p)
	}
	// Repeated launch delivery: the name is taken, and label lookup adopts the same process (SM-NODE-004).
	if _, err := r.Start(ctx, runner.Spec{Name: "r-test0001", Argv: []string{"/bin/true"}, Labels: labels}); !errors.Is(err, runner.ErrNameTaken) {
		t.Fatalf("duplicate launch: %v", err)
	}
	found, err := r.List(ctx, map[string]string{"spaceman.run": "run_test0001"}, false)
	if err != nil || len(found) != 1 || found[0].Identity() != p.Identity() {
		t.Fatalf("adopt by labels: %+v %v", found, err)
	}
	st, ap, err := r.Attach(ctx, p.ID, true, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if ap.Cols != 200 || ap.Rows != 120 {
		t.Fatalf("observer must not resize: %dx%d", ap.Cols, ap.Rows)
	}
	var out strings.Builder
	var ci *runner.CloseInfo
	for ci == nil {
		data, closed, err := st.Next(ctx)
		if err != nil {
			t.Fatalf("stream: %v (got %q)", err, out.String())
		}
		ci = closed
		out.Write(data)
	}
	if !strings.Contains(out.String(), "hello-spaceman") || ci.ExitCode == nil || *ci.ExitCode != 7 || ci.Reason != "exited" {
		t.Fatalf("output %q close %+v", out.String(), ci)
	}
	done, err := r.Get(ctx, p.ID)
	if err != nil || done.State != runner.StateFinished || done.ExitCode == nil || *done.ExitCode != 7 {
		t.Fatalf("finished %+v %v", done, err)
	}
	if err := r.Remove(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Get(ctx, p.ID); !errors.Is(err, runner.ErrNotFound) {
		t.Fatalf("removed terminal still found: %v", err)
	}
}

// ADR-0017: run terminals parented to a guard end when the guard's program ends.
func TestRealLeaseGuardParent(t *testing.T) {
	r, _ := realCore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	guard, err := r.Start(ctx, runner.Spec{Name: "g-test", Argv: []string{"/bin/sh", "-c", "sleep 1"}})
	if err != nil {
		t.Fatal(err)
	}
	child, err := r.Start(ctx, runner.Spec{Name: "r-child", Argv: []string{"/bin/sh", "-c", "sleep 60"}, Parent: guard.ID})
	if err != nil {
		t.Fatal(err)
	}
	if child.Parent != guard.ID {
		t.Fatalf("parent not set: %+v", child)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		c, err := r.Get(ctx, child.ID)
		if err != nil {
			t.Fatal(err)
		}
		if c.State == runner.StateFinished {
			if c.Reason != "killed" {
				t.Fatalf("child ended with reason %q", c.Reason)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child outlived its guard")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestRealEvents(t *testing.T) {
	r, _ := realCore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	evs, err := r.Subscribe(ctx, map[string]string{"spaceman.test": "events"})
	if err != nil {
		t.Fatal(err)
	}
	if first := <-evs; first.Kind != "snapshot" {
		t.Fatalf("first event %+v", first)
	}
	p, err := r.Start(ctx, runner.Spec{Name: "r-events", Argv: []string{"/bin/sh", "-c", "sleep 0.3; exit 4"}, Labels: map[string]string{"spaceman.test": "events"}})
	if err != nil {
		t.Fatal(err)
	}
	var created, finished bool
	for !created || !finished {
		select {
		case ev, ok := <-evs:
			if !ok {
				t.Fatal("event stream ended")
			}
			if ev.TerminalID != p.ID {
				continue
			}
			switch ev.Kind {
			case "created":
				created = ev.Process != nil && ev.Process.Name == "r-events"
			case "finished":
				finished = strings.Contains(string(ev.Data), `"exit_code":4`)
			}
		case <-ctx.Done():
			t.Fatalf("timeout: created=%v finished=%v", created, finished)
		}
	}
}

// SM-UI-004, S-5: an interactive attach on its own connection types into the program (bytes, not key names), keeps
// the terminal's size when it attaches at the current size, and closing it ends the dedicated connection; a later
// terminal.send is still delivered (hc holds it until ~3 s after the last typed key, its documented quiet rule).
func TestRealTakeoverTypesAndDetaches(t *testing.T) {
	r, _ := realCore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	p, err := r.Start(ctx, runner.Spec{Name: "r-takeover", Argv: []string{"/bin/cat"}, Cwd: "/tmp"})
	if err != nil {
		t.Fatal(err)
	}
	st, ap, err := r.Takeover(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ap.Cols != 200 || ap.Rows != 120 {
		t.Fatalf("takeover changed the size: %dx%d", ap.Cols, ap.Rows)
	}
	// Characters terminal.keys has no name for ($ ~ " \) travel as plain bytes.
	if err := st.Write([]byte("typed $HOME ~ \"q\" \\x\r")); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	for !strings.Contains(out.String(), `typed $HOME ~ "q" \x`) {
		data, closed, err := st.Next(ctx)
		if err != nil || closed != nil {
			t.Fatalf("stream: %v %v (got %q)", err, closed, out.String())
		}
		out.Write(data)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if got, err := r.Get(ctx, p.ID); err != nil || got.Cols != 200 || got.Rows != 120 {
		t.Fatalf("after takeover: %+v %v", got, err)
	}
	// After the release, automated input goes in again (after the quiet period that follows the last typed key).
	obs, _, err := r.Attach(ctx, p.ID, true, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer obs.Close()
	start := time.Now()
	if err := r.Send(ctx, p.ID, "after-release", false); err != nil {
		t.Fatal(err)
	}
	var seen strings.Builder
	for strings.Count(seen.String(), "after-release") < 2 { // the echo and cat's copy
		data, closed, err := obs.Next(ctx)
		if err != nil || closed != nil {
			t.Fatalf("observe: %v %v (got %q)", err, closed, seen.String())
		}
		seen.Write(data)
	}
	t.Logf("send after release delivered in %s", time.Since(start).Round(time.Millisecond))
	if _, err := r.Kill(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
}

// SM-COMM-003: the idle-gated send types into a program that reports idle or done (a finished turn, how Claude Code
// waits for input) and refuses one that reports working. herdr-core's own gate takes one exact status and refuses done.
func TestRealSendIdleOrDone(t *testing.T) {
	r, _ := realCore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	script := `printf '\033]7501;state=%s\007' "$0"; while read -r line; do echo "got:$line"; done`
	for state, want := range map[string]bool{"idle": true, "done": true, "working": false} {
		p, err := r.Start(ctx, runner.Spec{Name: "r-send-" + state, Argv: []string{"/bin/sh", "-c", script, state}, Cwd: "/tmp"})
		if err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(10 * time.Second)
		for {
			got, err := r.Get(ctx, p.ID)
			if err == nil && got.Status.State == state {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: status never reported (%+v, %v)", state, got, err)
			}
			time.Sleep(100 * time.Millisecond)
		}
		err = r.Send(ctx, p.ID, "hello-"+state, true)
		if want && err != nil {
			t.Fatalf("%s: send refused: %v", state, err)
		}
		if !want && !errors.Is(err, runner.ErrBusy) {
			t.Fatalf("%s: send = %v, want busy", state, err)
		}
		_, _ = r.Kill(ctx, p.ID)
	}
}

// Terminal size (hc 0.1.16): programs start at the core's `headless` size (CoreOptions.TerminalSize; herdr-core's own
// default is 200×120, and `hc run` takes no size). An interactive attach resizes a program only while it is attached:
// once it detaches the program is back at the headless size, and an observer (the recorder) never resizes. A running
// core reads a changed size for the programs it starts next, without a restart.
func TestRealTerminalSize(t *testing.T) {
	r, o := realCore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	start := func(name string) *runner.Process {
		t.Helper()
		p, err := r.Start(ctx, runner.Spec{Name: name, Argv: []string{"/bin/sh", "-c", "while :; do stty size; sleep 0.3; done"}, Cwd: "/tmp"})
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(300 * time.Millisecond)
		got, err := r.Get(ctx, p.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if p := start("r-default"); p.Cols != 200 || p.Rows != 120 {
		t.Fatalf("herdr-core's default size is %dx%d, want 200x120", p.Cols, p.Rows)
	}
	// An agent restart against the same running core, with a size configured (EnsureCore connects to the running core
	// and rewrites its config).
	o.TerminalSize = "160x48"
	again, err := EnsureCore(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	_ = again.Close()
	p := start("r-sized")
	if p.Cols != 160 || p.Rows != 48 {
		t.Fatalf("a program started after the size changed is %dx%d, want 160x48", p.Cols, p.Rows)
	}
	// A short interactive attach at another size on its own connection: resized while attached, back afterwards.
	c2, err := Dial(ctx, o.Socket)
	if err != nil {
		t.Fatal(err)
	}
	s, _, err := New(c2).Attach(ctx, p.ID, false, 120, 30)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if g, _ := r.Get(ctx, p.ID); g == nil || g.Cols != 120 || g.Rows != 30 {
		t.Fatalf("while attached: %+v", g)
	}
	_ = s.Close()
	_ = c2.Close()
	time.Sleep(300 * time.Millisecond)
	obs, _, err := r.Attach(ctx, p.ID, true, 200, 120)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	_ = obs.Close()
	if g, _ := r.Get(ctx, p.ID); g == nil || g.Cols != 160 || g.Rows != 48 {
		t.Fatalf("after the attach and an observer: %+v, want the headless 160x48", g)
	}
}

// ADR-0075, S-7 (hc 0.1.17): the node holds a run's size through runner.Takeover (an interactive attach at the current
// size on its own connection) and Resize. The program's size follows each resize (`stty size` and the terminal
// record), events.subscribe reports every change as `size` {cols, rows}, the newest interactive attach sets the size
// (a second one's resize wins; once it closes the first one's size is back), and closing the hold returns the program
// to the core's headless size. Nothing is typed.
func TestRealTerminalSizeHold(t *testing.T) {
	r, _ := realCore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	evs, err := r.Subscribe(ctx, map[string]string{"spaceman.test": "size"})
	if err != nil {
		t.Fatal(err)
	}
	if first := <-evs; first.Kind != "snapshot" {
		t.Fatalf("first event %+v", first)
	}
	p, err := r.Start(ctx, runner.Spec{Name: "r-hold", Argv: []string{"/bin/sh", "-c", "while :; do stty size; sleep 0.2; done"}, Cwd: "/tmp",
		Labels: map[string]string{"spaceman.test": "size"}})
	if err != nil {
		t.Fatal(err)
	}
	sizeEvent := func(cols, rows int) {
		t.Helper()
		want := fmt.Sprintf(`{"cols":%d,"rows":%d}`, cols, rows)
		for {
			select {
			case ev := <-evs:
				if ev.Kind == "size" && ev.TerminalID == p.ID && strings.Contains(string(ev.Data), want) {
					return
				}
			case <-ctx.Done():
				t.Fatalf("no size event %s", want)
			}
		}
	}
	is := func(cols, rows int) {
		t.Helper()
		sizeEvent(cols, rows)
		if g, err := r.Get(ctx, p.ID); err != nil || g.Cols != cols || g.Rows != rows {
			t.Fatalf("terminal is %+v, want %dx%d (%v)", g, cols, rows, err)
		}
	}
	hold, _, err := r.Takeover(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := hold.Resize(120, 40); err != nil {
		t.Fatal(err)
	}
	is(120, 40)
	if err := hold.Resize(90, 25); err != nil {
		t.Fatal(err)
	}
	is(90, 25)
	second, _, err := r.Takeover(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Resize(70, 20); err != nil {
		t.Fatal(err)
	}
	is(70, 20)
	_ = second.Close()
	is(90, 25)
	// The program itself sees the size (its stty output, read through an observer).
	obs, _, err := r.Attach(ctx, p.ID, true, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var seen strings.Builder
	for !strings.Contains(seen.String(), "25 90") {
		d, closed, err := obs.Next(ctx)
		if err != nil || closed != nil {
			t.Fatalf("observer: %v %+v (saw %q)", err, closed, seen.String())
		}
		seen.Write(d)
	}
	_ = obs.Close()
	_ = hold.Close()
	is(200, 120)
}
