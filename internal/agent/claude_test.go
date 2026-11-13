package agent

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/harness"
	"github.com/aurimasniekis/go-spaceman/internal/harness/dist"
	"github.com/aurimasniekis/go-spaceman/internal/harness/dist/disttest"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/runner"
	"github.com/aurimasniekis/go-spaceman/internal/runner/runnertest"
)

const ccSpace = "spc_cc"

// ccAgent is an agent on the in-memory runner with a lease for spc_cc (generation 1). Claude Code is managed when m
// is not nil.
func ccAgent(t *testing.T, dir string, m *dist.Manager, edit func(*Options)) (*Agent, *runnertest.Fake) {
	t.Helper()
	j, err := OpenJournal(context.Background(), filepath.Join(dir, "journal"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = j.Close() })
	f := runnertest.New()
	o := Options{DataDir: dir, Runner: f, AgentBinary: "/opt/spaceman/spaceman-agent", BridgeSocket: "/run/sm/bridge.sock",
		HarnessBinaries: map[string]string{harness.KindClaude: "claude", harness.KindCodex: "codex"},
		Claude:          ClaudeOptions{Dist: m, Channel: dist.ChannelLatest, DownloadTimeout: 30 * time.Second}}
	if edit != nil {
		edit(&o)
	}
	a := New(j, o)
	a.mem.base = context.Background()
	t.Cleanup(a.stopRecorders)
	// Review N1: the agent's tracked background work (a run-ended memory flush, an idle checkpoint) writes under dir
	// and into the journal. Registered after the caller's TempDir and the journal, this runs first: it waits for that
	// work and refuses more, so neither is removed or closed under it.
	t.Cleanup(func() { a.drain(10 * time.Second) })
	a.leases[ccSpace] = leaseRow{SpaceID: ccSpace, Generation: 1, ExpiresAt: time.Now().Add(time.Hour), Workdir: "cc"}
	return a, f
}

func ccLaunch(a *Agent, runID string) {
	a.launch(context.Background(), proto.Command{CommandID: "cmd_" + runID, Kind: proto.CmdLaunch, SpaceID: ccSpace, RunID: runID, Generation: 1},
		proto.LaunchPayload{Harness: harness.KindClaude, Kind: "worker", Role: "implementor", InstanceID: "ins_cc", Prompt: "hello"})
}

// runSpec is the runner spec of runID's program (the launch shim's argv; the harness argv follows "--").
func runSpec(t *testing.T, f *runnertest.Fake, runID string) (runner.Spec, []string) {
	t.Helper()
	for _, s := range slices.Backward(f.Specs) {
		if s.Name == RunName(runID) {
			i := slices.Index(s.Argv, "--")
			if i < 0 {
				t.Fatalf("argv %v has no harness part", s.Argv)
			}
			return s, s.Argv[i+1:]
		}
	}
	t.Fatalf("%s was not started", runID)
	return runner.Spec{}, nil
}

func getRun(t *testing.T, a *Agent, runID string) runRow {
	t.Helper()
	r, err := a.j.getRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// ADR-0077: with manage on, a run launches the verified cached binary by absolute path (downloaded first when nothing
// is cached), with both updater variables, and the journal records the version, its path and what was requested.
func TestManagedClaudeLaunch(t *testing.T) {
	srv := disttest.New(t)
	srv.Add("2.1.900", disttest.StubBinary("2.1.900"))
	srv.SetPointer(dist.ChannelLatest, "2.1.900")
	dir := t.TempDir()
	m := srv.Manager(t, filepath.Join(dir, "harness", "claude-code"), nil)
	// A managed node needs no claude_binary: the run gets the cached binary's path.
	a, f := ccAgent(t, dir, m, func(o *Options) { delete(o.HarnessBinaries, harness.KindClaude) })

	ccLaunch(a, "run_cc1")
	spec, argv := runSpec(t, f, "run_cc1")
	want := filepath.Join(m.Dir(), "2.1.900", "claude")
	if argv[0] != want || !filepath.IsAbs(argv[0]) {
		t.Fatalf("harness argv[0] = %q, want %q", argv[0], want)
	}
	for _, k := range []string{"DISABLE_AUTOUPDATER", "DISABLE_UPDATES"} {
		if spec.Env[k] != "1" {
			t.Errorf("run env %s = %q, want 1", k, spec.Env[k])
		}
	}
	r := getRun(t, a, "run_cc1")
	if r.State != proto.RunRunning {
		t.Fatalf("run state %s (%s)", r.State, r.Error)
	}
	if r.Bin != (harnessRun{Version: "2.1.900", Path: want, Requested: dist.ChannelLatest}) {
		t.Fatalf("journal harness %+v", r.Bin)
	}
	if used, _ := a.claudeVersionsInUse(context.Background()); !slices.Equal(used, []string{"2.1.900"}) {
		t.Fatalf("versions in use %v", used)
	}
	// The cached binary is the stub that was published, and it runs.
	info := a.claudeInfo(context.Background())
	if !info.Available || info.Version != "2.1.900" || info.Path != want {
		t.Fatalf("capabilities %+v", info)
	}
	// A second launch uses the cache: no second download.
	ccLaunch(a, "run_cc2")
	if _, argv := runSpec(t, f, "run_cc2"); argv[0] != want || srv.BinaryHits.Load() != 1 {
		t.Fatalf("second launch argv[0] %q, %d downloads", argv[0], srv.BinaryHits.Load())
	}
}

// Review N1: a fallback (the channel's target not cached yet) is logged and recorded on the run's journal entry.
func TestManagedClaudeFallbackRecorded(t *testing.T) {
	srv := disttest.New(t)
	srv.Add("2.1.1", disttest.StubBinary("2.1.1"))
	srv.Add("2.1.2", disttest.StubBinary("2.1.2"))
	dir := t.TempDir()
	m := srv.Manager(t, filepath.Join(dir, "cc"), nil)
	if _, err := m.Ensure(context.Background(), "2.1.1"); err != nil {
		t.Fatal(err)
	}
	srv.SetPointer(dist.ChannelLatest, "2.1.2")
	srv.Hold() // 2.1.2 downloads in the background meanwhile
	a, f := ccAgent(t, dir, m, nil)
	ccLaunch(a, "run_fb")
	r := getRun(t, a, "run_fb")
	if r.Bin.Version != "2.1.1" || r.Bin.Fallback != dist.FallbackOlderCached || r.Bin.Requested != dist.ChannelLatest {
		t.Fatalf("journal harness %+v", r.Bin)
	}
	if _, argv := runSpec(t, f, "run_fb"); argv[0] != filepath.Join(m.Dir(), "2.1.1", "claude") {
		t.Fatalf("argv[0] %q", argv[0])
	}
}

// manage: false keeps the configured claude_binary (resolved on the core's PATH by the shim) and records no version;
// the updater stays off unless self_update is on.
func TestUnmanagedClaudeLaunch(t *testing.T) {
	for _, selfUpdate := range []bool{false, true} {
		dir := t.TempDir()
		a, f := ccAgent(t, dir, nil, func(o *Options) { o.Claude = ClaudeOptions{SelfUpdate: selfUpdate} })
		ccLaunch(a, "run_un")
		spec, argv := runSpec(t, f, "run_un")
		if argv[0] != "claude" {
			t.Fatalf("argv[0] = %q, want the configured claude_binary", argv[0])
		}
		for _, k := range []string{"DISABLE_AUTOUPDATER", "DISABLE_UPDATES"} {
			if v, set := spec.Env[k]; set == selfUpdate || (!selfUpdate && v != "1") {
				t.Errorf("self_update %v: %s = %q (set %v)", selfUpdate, k, v, set)
			}
		}
		if r := getRun(t, a, "run_un"); r.Bin != (harnessRun{}) || r.State != proto.RunRunning {
			t.Fatalf("run %s %+v", r.State, r.Bin)
		}
		if _, err := os.Stat(filepath.Join(dir, "harness")); !os.IsNotExist(err) {
			t.Fatalf("an unmanaged node created a cache: %v", err)
		}
	}
}

// With nothing cached the run waits in starting for the download, and runs once it is there.
func TestManagedClaudeWaitsForDownload(t *testing.T) {
	srv := disttest.New(t)
	srv.Add("2.1.7", disttest.StubBinary("2.1.7"))
	srv.SetPointer(dist.ChannelLatest, "2.1.7")
	srv.Hold()
	dir := t.TempDir()
	m := srv.Manager(t, filepath.Join(dir, "cc"), nil)
	a, f := ccAgent(t, dir, m, nil)
	done := make(chan struct{})
	go func() { defer close(done); ccLaunch(a, "run_wait") }()
	waitFor(t, "the download to start", func() bool { return srv.BinaryHits.Load() == 1 })
	if r := getRun(t, a, "run_wait"); r.State != proto.RunStarting || r.Bin.Version != "" {
		t.Fatalf("while downloading: state %s, harness %+v", r.State, r.Bin)
	}
	srv.Release()
	<-done
	if r := getRun(t, a, "run_wait"); r.State != proto.RunRunning || r.Bin.Version != "2.1.7" {
		t.Fatalf("after the download: state %s (%s), harness %+v", r.State, r.Error, r.Bin)
	}
	if _, argv := runSpec(t, f, "run_wait"); argv[0] != filepath.Join(m.Dir(), "2.1.7", "claude") {
		t.Fatalf("argv[0] %q", argv[0])
	}
}

// The wait is bounded by download_timeout; then the run fails with a clear error and nothing is started.
func TestManagedClaudeDownloadTimeout(t *testing.T) {
	srv := disttest.New(t)
	srv.Add("2.1.7", disttest.StubBinary("2.1.7"))
	srv.SetPointer(dist.ChannelLatest, "2.1.7")
	srv.Hold()
	dir := t.TempDir()
	m := srv.Manager(t, filepath.Join(dir, "cc"), nil)
	a, f := ccAgent(t, dir, m, func(o *Options) { o.Claude.DownloadTimeout = 300 * time.Millisecond })
	start := time.Now()
	ccLaunch(a, "run_slow")
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("the launch waited %s", d)
	}
	r := getRun(t, a, "run_slow")
	if r.State != proto.RunFailed || !strings.Contains(r.Error, "resolve the Claude Code version") ||
		!strings.Contains(r.Error, "download timeout") {
		t.Fatalf("state %s, error %q", r.State, r.Error)
	}
	for _, s := range f.Specs {
		if s.Name == RunName("run_slow") {
			t.Fatal("the run was started without a binary")
		}
	}
}

// notStarted fails the test when runID's program was started.
func notStarted(t *testing.T, f *runnertest.Fake, runID string) {
	t.Helper()
	for _, s := range f.Specs {
		if s.Name == RunName(runID) {
			t.Fatalf("%s was started", runID)
		}
	}
}

// Review S1 (PR #42): a stop while the launch waits for the download is not undone when the download completes: the
// run stays stopped before start, and its program never starts.
func TestManagedClaudeStopDuringDownload(t *testing.T) {
	srv := disttest.New(t)
	srv.Add("2.1.7", disttest.StubBinary("2.1.7"))
	srv.SetPointer(dist.ChannelLatest, "2.1.7")
	srv.Hold()
	dir := t.TempDir()
	m := srv.Manager(t, filepath.Join(dir, "cc"), nil)
	a, f := ccAgent(t, dir, m, nil)
	done := make(chan struct{})
	go func() { defer close(done); ccLaunch(a, "run_stop") }()
	waitFor(t, "the download to start", func() bool { return srv.BinaryHits.Load() == 1 })
	a.stop(context.Background(), "run_stop", "interrupt", time.Second, "test")
	if r := getRun(t, a, "run_stop"); r.State != proto.RunExited || r.Desired != "stopped" {
		t.Fatalf("after the stop: state %s, desired %s", r.State, r.Desired)
	}
	srv.Release()
	<-done
	r := getRun(t, a, "run_stop")
	if r.State != proto.RunExited || r.Desired != "stopped" || r.Reason != "stopped before start" || r.TerminalID != "" {
		t.Fatalf("after the download: state %s (%s / %s), desired %s, terminal %q", r.State, r.Reason, r.Error, r.Desired, r.TerminalID)
	}
	notStarted(t, f, "run_stop")
	if len(m.Held()) != 0 {
		t.Fatalf("held %v", m.Held())
	}
}

// Review S1: a stop that arrives after the version was resolved, while the launch is being prepared, also wins.
func TestManagedClaudeStopBeforeStart(t *testing.T) {
	srv := disttest.New(t)
	srv.Add("2.1.7", disttest.StubBinary("2.1.7"))
	srv.SetPointer(dist.ChannelLatest, "2.1.7")
	dir := t.TempDir()
	m := srv.Manager(t, filepath.Join(dir, "cc"), nil)
	a, f := ccAgent(t, dir, m, nil)
	a.claudeStage = func(stage string, _ dist.Resolution) {
		if stage == "released" {
			a.stop(context.Background(), "run_late", "kill", 0, "test")
		}
	}
	ccLaunch(a, "run_late")
	if r := getRun(t, a, "run_late"); r.State != proto.RunExited || r.Desired != "stopped" || r.TerminalID != "" {
		t.Fatalf("state %s (%s / %s), desired %s, terminal %q", r.State, r.Reason, r.Error, r.Desired, r.TerminalID)
	}
	notStarted(t, f, "run_late")
	if _, err := os.Stat(filepath.Join(a.runDir("run_late"), SecretsFile)); !os.IsNotExist(err) {
		t.Fatalf("the abandoned launch left its secrets file: %v", err)
	}
}

// Review S1: the space's lease is revoked (fenced) while the launch waits for the download: the run does not start.
func TestManagedClaudeLeaseLostDuringDownload(t *testing.T) {
	srv := disttest.New(t)
	srv.Add("2.1.7", disttest.StubBinary("2.1.7"))
	srv.SetPointer(dist.ChannelLatest, "2.1.7")
	srv.Hold()
	dir := t.TempDir()
	m := srv.Manager(t, filepath.Join(dir, "cc"), nil)
	a, f := ccAgent(t, dir, m, nil)
	done := make(chan struct{})
	go func() { defer close(done); ccLaunch(a, "run_fenced") }()
	waitFor(t, "the download to start", func() bool { return srv.BinaryHits.Load() == 1 })
	a.mu.Lock()
	l := a.leases[ccSpace]
	l.Generation = 2 // central fenced generation 1
	a.leases[ccSpace] = l
	a.mu.Unlock()
	srv.Release()
	<-done
	r := getRun(t, a, "run_fenced")
	if r.State != proto.RunFailed || !strings.Contains(r.Error, "no valid lease") {
		t.Fatalf("state %s, error %q", r.State, r.Error)
	}
	notStarted(t, f, "run_fenced")
}

// A run adopted after an agent restart keeps its version protected from GC while it is live, and the startup
// re-check leaves a good version alone; once the run has ended its version may go.
func TestClaudeAdoptedRunKeepsVersion(t *testing.T) {
	srv := disttest.New(t)
	dir := t.TempDir()
	m := srv.Manager(t, filepath.Join(dir, "cc"), func(o *dist.Options) { o.Keep = 1 })
	for _, v := range []string{"2.1.1", "2.1.2", "2.1.3"} {
		srv.Add(v, disttest.StubBinary(v))
		if _, err := m.Ensure(context.Background(), v); err != nil {
			t.Fatal(err)
		}
	}
	srv.SetPointer(dist.ChannelLatest, "2.1.3")
	a, f := ccAgent(t, dir, m, nil)
	ctx := context.Background()
	// The run started before the restart: its program is still live in the runner and the journal holds its version.
	p, err := f.Start(ctx, runner.Spec{Name: RunName("run_old"), Argv: []string{"/bin/true"},
		Labels: map[string]string{"spaceman.managed": "1", "spaceman.run": "run_old"}})
	if err != nil {
		t.Fatal(err)
	}
	row := runRow{RunID: "run_old", SpaceID: ccSpace, Generation: 1, Kind: "worker", Role: "implementor", Harness: harness.KindClaude,
		InstanceID: "ins_cc", TerminalName: RunName("run_old")}
	if err := a.j.insertRun(ctx, row); err != nil {
		t.Fatal(err)
	}
	if err := a.j.setRunHarness(ctx, "run_old", harnessRun{Version: "2.1.1", Path: filepath.Join(m.Dir(), "2.1.1", "claude"), Requested: "latest"}); err != nil {
		t.Fatal(err)
	}
	if err := a.reconcileRunner(ctx); err != nil {
		t.Fatal(err)
	}
	if r := getRun(t, a, "run_old"); !r.Adopted || r.State != proto.RunRunning || r.Bin.Version != "2.1.1" {
		t.Fatalf("adopted run %+v", r)
	}
	cctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	a.startClaude(cctx, &wg)
	cancel()
	wg.Wait()
	if got := versions(m); !slices.Equal(got, []string{"2.1.3", "2.1.1"}) {
		t.Fatalf("cache after startup GC %v, want 2.1.3 (newest, the channel's target) and 2.1.1 (the adopted run's)", got)
	}
	// The run ends: its version is no longer in use.
	if _, err := f.Kill(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if err := a.reconcileRunner(ctx); err != nil {
		t.Fatal(err)
	}
	a.claudeGC(ctx)
	if got := versions(m); !slices.Equal(got, []string{"2.1.3"}) {
		t.Fatalf("cache after the run ended %v", got)
	}
}

// Review S4 at the agent: a GC at any point of a managed launch never removes the version it runs. The launch gets an
// older cached version as a fallback (the channel's target is downloading), which nothing but the launch hold and then
// the run's journal entry protects; GC runs right after the launch took its hold, once the journal records the version
// (the hold still taken), right after it released the hold, and once more after the run started. (dist's TestAcquireRacesGC and
// TestAcquireStressGC cover a GC between the decision and the hold.)
func TestClaudeGCRacesLaunch(t *testing.T) {
	srv := disttest.New(t)
	dir := t.TempDir()
	m := srv.Manager(t, filepath.Join(dir, "cc"), func(o *dist.Options) { o.Keep = 1 })
	for _, v := range []string{"2.1.1", "2.1.5"} {
		srv.Add(v, disttest.StubBinary(v))
		if _, err := m.Ensure(context.Background(), v); err != nil {
			t.Fatal(err)
		}
	}
	srv.Add("2.1.3", disttest.StubBinary("2.1.3"))
	srv.SetPointer(dist.ChannelLatest, "2.1.3")
	srv.Hold("2.1.3") // the target stays uncached: the launch falls back to 2.1.1, beyond Keep
	a, f := ccAgent(t, dir, m, nil)
	var stages []string
	a.claudeStage = func(stage string, res dist.Resolution) {
		// Review N2: the hold is released only once the journal records the version, so GC never sees the version
		// neither held nor in use.
		held := slices.Contains(m.Held(), res.Version)
		recorded := getRun(t, a, "run_race").Bin.Version == res.Version
		switch stage {
		case "acquired":
			if !held || recorded {
				t.Errorf("at %q: held %v, recorded %v; want held, not yet recorded", stage, held, recorded)
			}
		case "recorded":
			if !held || !recorded {
				t.Errorf("at %q: held %v, recorded %v; want both (the hold is released after the journal has it)", stage, held, recorded)
			}
		case "released":
			if held || !recorded {
				t.Errorf("at %q: held %v, recorded %v", stage, held, recorded)
			}
		}
		a.claudeGC(context.Background())
		if _, err := os.Stat(res.Path); err != nil {
			t.Errorf("GC at %q removed %s: %v", stage, res.Version, err)
		}
		stages = append(stages, stage)
	}
	ccLaunch(a, "run_race")
	if !slices.Equal(stages, []string{"acquired", "recorded", "released"}) {
		t.Fatalf("stages %v", stages)
	}
	r := getRun(t, a, "run_race")
	if r.State != proto.RunRunning || r.Bin.Version != "2.1.1" || r.Bin.Fallback != dist.FallbackOlderCached {
		t.Fatalf("state %s (%s), harness %+v", r.State, r.Error, r.Bin)
	}
	a.claudeGC(context.Background())
	if _, err := os.Stat(r.Bin.Path); err != nil {
		t.Fatalf("GC removed a live run's binary: %v", err)
	}
	// The run ends: now GC may take the fallback.
	_, _ = f.Kill(context.Background(), r.TerminalID)
	if err := a.reconcileRunner(context.Background()); err != nil {
		t.Fatal(err)
	}
	a.claudeGC(context.Background())
	if _, err := os.Stat(r.Bin.Path); !os.IsNotExist(err) {
		t.Fatalf("GC kept an unused fallback: %v", err)
	}
}

func versions(m *dist.Manager) []string {
	var out []string
	for _, inst := range m.Installed() {
		out = append(out, inst.Version)
	}
	return out
}

// A profile's updater settings are reported, not silently ignored.
func TestWarnClaudeUpdateSettings(t *testing.T) {
	var buf strings.Builder
	log := slog.New(slog.NewTextHandler(&buf, nil))
	warnClaudeUpdateSettings(log, proto.ClaudeSettings{"autoUpdatesChannel": "stable", "env": map[string]any{"DISABLE_UPDATES": "0"}})
	if !strings.Contains(buf.String(), "autoUpdatesChannel") || !strings.Contains(buf.String(), "env.DISABLE_UPDATES") {
		t.Fatalf("log %q", buf.String())
	}
	buf.Reset()
	warnClaudeUpdateSettings(log, proto.ClaudeSettings{"model": "opus"})
	if buf.Len() != 0 {
		t.Fatalf("warned about %q", buf.String())
	}
}

// Review N4: the launch warns about the profile's updater settings only when the node turns the updater off; with
// self_update on (unmanaged) they take effect, so there is nothing to warn about.
func TestUpdateSettingsWarnedOnlyWhenUpdaterOff(t *testing.T) {
	for _, selfUpdate := range []bool{false, true} {
		var buf syncBuffer
		dir := t.TempDir()
		a, _ := ccAgent(t, dir, nil, func(o *Options) {
			o.Claude = ClaudeOptions{SelfUpdate: selfUpdate}
			o.Log = slog.New(slog.NewTextHandler(&buf, nil))
		})
		a.launch(context.Background(), proto.Command{CommandID: "cmd_w", Kind: proto.CmdLaunch, SpaceID: ccSpace, RunID: "run_w", Generation: 1},
			proto.LaunchPayload{Harness: harness.KindClaude, Kind: "worker", Role: "implementor", InstanceID: "ins_cc", Prompt: "hello",
				ClaudeSettings: proto.ClaudeSettings{"autoUpdatesChannel": "stable"}})
		if warned := strings.Contains(buf.String(), "updater settings have no effect"); warned == selfUpdate {
			t.Errorf("self_update %v: warned %v", selfUpdate, warned)
		}
	}
}

// syncBuffer is a strings.Builder safe for the agent's concurrent logging.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
