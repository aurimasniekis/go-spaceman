package agent

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/harness"
	"github.com/aurimasniekis/go-spaceman/internal/harness/dist"
	"github.com/aurimasniekis/go-spaceman/internal/harness/dist/disttest"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// ccLaunchPin launches a Claude Code run whose profile pins pin (protocol minor 11).
func ccLaunchPin(a *Agent, runID, pin string) {
	a.launch(context.Background(), proto.Command{CommandID: "cmd_" + runID, Kind: proto.CmdLaunch, SpaceID: ccSpace, RunID: runID, Generation: 1},
		proto.LaunchPayload{Harness: harness.KindClaude, Kind: "worker", Role: "implementor", InstanceID: "ins_cc", Prompt: "hello", HarnessVersion: pin})
}

// ADR-0077: a profile pin overrides the node's channel. A pinned version that isn't cached is downloaded and runs
// exactly (no fallback to the cached channel target), the run reports it, and GC keeps it as pinned afterwards.
func TestManagedClaudePinDownloadsExactVersion(t *testing.T) {
	srv := disttest.New(t)
	for _, v := range []string{"2.1.1", "2.1.2", "2.1.3"} {
		srv.Add(v, disttest.StubBinary(v))
	}
	srv.SetPointer(dist.ChannelLatest, "2.1.3")
	dir := t.TempDir()
	m := srv.Manager(t, filepath.Join(dir, "cc"), func(o *dist.Options) { o.Keep = 1 })
	if _, err := m.Ensure(context.Background(), "2.1.3"); err != nil { // the channel's target is cached
		t.Fatal(err)
	}
	a, f := ccAgent(t, dir, m, nil)

	ccLaunchPin(a, "run_pin", "2.1.1")
	r := getRun(t, a, "run_pin")
	want := filepath.Join(m.Dir(), "2.1.1", "claude")
	if r.State != proto.RunRunning || r.Bin != (harnessRun{Version: "2.1.1", Path: want, Requested: "2.1.1"}) {
		t.Fatalf("run %s (%s), harness %+v", r.State, r.Error, r.Bin)
	}
	if _, argv := runSpec(t, f, "run_pin"); argv[0] != want {
		t.Fatalf("argv[0] %q, want %q", argv[0], want)
	}
	// Every report from now on carries what ran (central stores it on the run row).
	if rep := r.report(); rep.HarnessVersion != "2.1.1" || rep.HarnessRequested != "2.1.1" || rep.HarnessFallback != "" {
		t.Fatalf("report %+v", rep)
	}
	// An unpinned launch still follows the channel.
	ccLaunch(a, "run_chan")
	if r := getRun(t, a, "run_chan"); r.Bin.Version != "2.1.3" || r.Bin.Requested != dist.ChannelLatest {
		t.Fatalf("channel run %+v", r.Bin)
	}
	// GC (keep 1) keeps the pinned version even with no live run on it: the node saw a profile pin it.
	for _, id := range []string{"run_pin", "run_chan"} {
		row := getRun(t, a, id)
		row.State = proto.RunExited
		if err := a.j.updateRun(context.Background(), row); err != nil {
			t.Fatal(err)
		}
	}
	if used, _ := a.claudeVersionsInUse(context.Background()); len(used) != 0 {
		t.Fatalf("in use %v", used)
	}
	if p := a.claudePinned(context.Background()); !slices.Equal(p, []string{"2.1.1"}) {
		t.Fatalf("pinned %v", p)
	}
	a.claudeGC(context.Background())
	if got := versions(m); !slices.Contains(got, "2.1.1") {
		t.Fatalf("GC removed the pinned version: %v", got)
	}
	// A new process (no in-memory pins) still knows the pin from the journal's recent runs.
	a.mu.Lock()
	a.claudePins = nil
	a.mu.Unlock()
	if p := a.claudePinned(context.Background()); !slices.Equal(p, []string{"2.1.1"}) {
		t.Fatalf("pinned from the journal %v", p)
	}
}

// A pin never falls back: a pinned version the release host doesn't have fails the launch even though another version
// is cached, and so does one whose download doesn't finish within the download timeout.
func TestManagedClaudePinNeverFallsBack(t *testing.T) {
	srv := disttest.New(t)
	srv.Add("2.1.2", disttest.StubBinary("2.1.2"))
	srv.Add("2.1.4", disttest.StubBinary("2.1.4"))
	srv.SetPointer(dist.ChannelLatest, "2.1.2")
	dir := t.TempDir()
	m := srv.Manager(t, filepath.Join(dir, "cc"), nil)
	if _, err := m.Ensure(context.Background(), "2.1.2"); err != nil {
		t.Fatal(err)
	}
	a, f := ccAgent(t, dir, m, func(o *Options) { o.Claude.DownloadTimeout = 300 * time.Millisecond })

	ccLaunchPin(a, "run_missing", "2.1.9")
	if r := getRun(t, a, "run_missing"); r.State != proto.RunFailed || !strings.Contains(r.Error, "resolve the Claude Code version") || r.Bin.Version != "" {
		t.Fatalf("missing pin: %s %q %+v", r.State, r.Error, r.Bin)
	}
	notStarted(t, f, "run_missing")

	srv.Hold("2.1.4")
	defer srv.Release()
	ccLaunchPin(a, "run_slow", "2.1.4")
	if r := getRun(t, a, "run_slow"); r.State != proto.RunFailed || !strings.Contains(r.Error, "download timeout") || r.Bin.Version != "" {
		t.Fatalf("slow pin: %s %q %+v", r.State, r.Error, r.Bin)
	}
	notStarted(t, f, "run_slow")
}

// A pin on a codex launch is refused: only Claude Code versions are managed.
func TestPinOnCodexRefused(t *testing.T) {
	a, f := ccAgent(t, t.TempDir(), nil, nil)
	a.launch(context.Background(), proto.Command{CommandID: "cmd_cx", Kind: proto.CmdLaunch, SpaceID: ccSpace, RunID: "run_cx", Generation: 1},
		proto.LaunchPayload{Harness: harness.KindCodex, Kind: "worker", Role: "implementor", InstanceID: "ins_cx", HarnessVersion: "1.2.3"})
	if r := getRun(t, a, "run_cx"); r.State != proto.RunFailed || !strings.Contains(r.Error, "claude-code only") {
		t.Fatalf("%s %q", r.State, r.Error)
	}
	notStarted(t, f, "run_cx")
}

// stubClaude writes an executable stub that reports version, as an operator-installed claude would.
func stubClaude(t *testing.T, version string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(p, disttest.StubBinary(version), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o755); err != nil { // umask 077 on some hosts
		t.Fatal(err)
	}
	return p
}

// ADR-0077 decision 6: on a node that doesn't manage Claude Code, a pin runs only when it is the installed version (by
// the absolute path that was checked); a different version or a channel pin fails the launch, which never runs another
// version.
func TestUnmanagedClaudePin(t *testing.T) {
	bin := stubClaude(t, "2.1.317")
	unmanaged := func(o *Options) { o.Claude = ClaudeOptions{}; o.HarnessBinaries[harness.KindClaude] = bin }

	a, f := ccAgent(t, t.TempDir(), nil, unmanaged)
	ccLaunchPin(a, "run_match", "2.1.317")
	r := getRun(t, a, "run_match")
	if r.State != proto.RunRunning || r.Bin != (harnessRun{Version: "2.1.317", Path: bin, Requested: "2.1.317"}) {
		t.Fatalf("matching pin: %s (%s) %+v", r.State, r.Error, r.Bin)
	}
	if _, argv := runSpec(t, f, "run_match"); argv[0] != bin {
		t.Fatalf("argv[0] %q", argv[0])
	}

	for pin, want := range map[string]string{
		"2.1.322":          "the profile pins 2.1.322, but the installed Claude Code is 2.1.317",
		dist.ChannelLatest: `can't resolve the profile's pin "latest"`,
		dist.ChannelStable: `can't resolve the profile's pin "stable"`,
	} {
		id := "run_" + strings.ReplaceAll(pin, ".", "_")
		ccLaunchPin(a, id, pin)
		r := getRun(t, a, id)
		if r.State != proto.RunFailed || !strings.Contains(r.Error, "this node does not manage Claude Code versions") || !strings.Contains(r.Error, want) {
			t.Fatalf("pin %s: %s %q", pin, r.State, r.Error)
		}
		notStarted(t, f, id)
	}

	// Claude Code missing altogether: the pin can't be checked, so the launch fails too.
	b, f2 := ccAgent(t, t.TempDir(), nil, func(o *Options) {
		o.Claude = ClaudeOptions{}
		o.HarnessBinaries[harness.KindClaude] = filepath.Join(t.TempDir(), "absent")
	})
	ccLaunchPin(b, "run_absent", "2.1.317")
	if r := getRun(t, b, "run_absent"); r.State != proto.RunFailed || !strings.Contains(r.Error, "does not manage Claude Code versions") {
		t.Fatalf("absent: %s %q", r.State, r.Error)
	}
	notStarted(t, f2, "run_absent")
}

// Minor 11: a managed node's capabilities describe the channel, its resolved targets and the cache.
func TestManagedClaudeInfo(t *testing.T) {
	srv := disttest.New(t)
	srv.Add("2.1.5", disttest.StubBinary("2.1.5"))
	srv.Add("2.1.6", disttest.StubBinary("2.1.6"))
	srv.SetPointer(dist.ChannelLatest, "2.1.6")
	dir := t.TempDir()
	m := srv.Manager(t, filepath.Join(dir, "cc"), nil)
	for _, v := range []string{"2.1.5", "2.1.6"} {
		if _, err := m.Ensure(context.Background(), v); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.Channel(context.Background(), dist.ChannelLatest); err != nil {
		t.Fatal(err)
	}
	a, _ := ccAgent(t, dir, m, nil)
	info := a.claudeInfo(context.Background())
	if !info.Managed || info.Channel != dist.ChannelLatest || info.Version != "2.1.6" || !slices.Equal(info.Cached, []string{"2.1.6", "2.1.5"}) {
		t.Fatalf("info %+v", info)
	}
	if res := info.Resolved[dist.ChannelLatest]; res.Version != "2.1.6" || res.ResolvedAt.IsZero() {
		t.Fatalf("resolved %+v", info.Resolved)
	}
	if want := int64(len(disttest.StubBinary("2.1.5")) + len(disttest.StubBinary("2.1.6"))); info.CacheBytes != want {
		t.Fatalf("cache bytes %d, want %d", info.CacheBytes, want)
	}
}
