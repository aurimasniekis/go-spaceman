//go:build real

package hc

import (
	"context"
	"errors"
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
