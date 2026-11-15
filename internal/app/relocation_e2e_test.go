package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/agent"
	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	hns "github.com/aurimasniekis/go-spaceman/internal/harness"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/runner/runnertest"
)

// node is one node agent with an in-memory runner (simulated evidence: the runner is a fake, git and the protocol are real).
type node struct {
	t      *testing.T
	e      *fleetEnv
	id     string
	dir    string
	cred   string
	run    *runnertest.Fake
	bridge string
	cancel context.CancelFunc
	j      *agent.Journal
	done   chan struct{}
}

func (e *fleetEnv) addNode(name string) *node {
	e.t.Helper()
	ctx := context.Background()
	en, err := e.c.Fleet.CreateNode(ctx, admin, fleet.CreateNodeInput{Name: name})
	if err != nil {
		e.t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{"token": en.Token})
	res, err := http.Post(e.srv.URL+fleet.EnrollPath, "application/json", bytes.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	var er fleet.EnrollResult
	_ = json.Unmarshal(b, &er)
	sock, err := os.MkdirTemp("/tmp", "smr")
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { _ = os.RemoveAll(sock) })
	n := &node{t: e.t, e: e, id: en.Node.ID, dir: filepath.Join(e.t.TempDir(), name), cred: er.Credential, run: runnertest.New(),
		bridge: filepath.Join(sock, "b.sock")}
	e.t.Cleanup(n.stop)
	n.start()
	eventually(e.t, name+" connected", func() bool { x, _ := e.c.Fleet.GetNode(ctx, n.id); return x != nil && x.Connected })
	return n
}

func (n *node) start() {
	n.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	j, err := agent.OpenJournal(ctx, filepath.Join(n.dir, "journal"), discard(), nil)
	if err != nil {
		n.t.Fatal(err)
	}
	a := agent.New(j, agent.Options{DataDir: n.dir, CentralURL: n.e.srv.URL, Credential: n.cred, Runner: n.run,
		RunnerInfo: proto.RunnerInfo{Kind: "fake", CoreID: "fakecore"}, HarnessBinaries: map[string]string{hns.KindClaude: "/usr/bin/true", hns.KindCodex: "/usr/bin/true"},
		AgentBinary: "/usr/bin/true", BridgeSocket: n.bridge, ReconnectMax: 200 * time.Millisecond, Capacity: 4, Log: testLog()})
	n.cancel, n.j, n.done = cancel, j, make(chan struct{})
	go func() { _ = a.Run(ctx); close(n.done) }()
}

func (n *node) stop() {
	if n.cancel != nil {
		n.cancel()
		<-n.done
		_ = n.j.Close()
		n.cancel = nil
	}
}

func eventuallyFor(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			timedOut(t, what, d)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.test", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.test")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// relocSpace is a space on node a with a pushed repository "app" (origin: a bare repository both nodes can reach) and
// role memory acknowledged by central.
type relocSpace struct {
	sp     *fleet.Space
	origin string
	repo   string
	head   string
	memory string
}

func setupRelocSpace(t *testing.T, e *fleetEnv, a *node, slug string) relocSpace {
	t.Helper()
	ctx := context.Background()
	sp, err := e.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: slug, Name: slug, NodeID: a.id})
	if err != nil {
		t.Fatal(err)
	}
	bindStubAccounts(t, e.c, sp.ID)
	eventually(t, "lease granted", func() bool { x, _ := e.c.Fleet.GetSpace(ctx, sp.ID); return x.Lease != nil && x.Lease.Valid })
	rs := relocSpace{sp: sp, origin: filepath.Join(t.TempDir(), "app.git")}
	git(t, "/", "init", "-q", "--bare", "-b", "main", rs.origin)
	rs.repo = filepath.Join(a.dir, "spaces", sp.Workdir, "app")
	git(t, "/", "clone", "-q", rs.origin, rs.repo)
	if err := os.WriteFile(filepath.Join(rs.repo, "README.md"), []byte("app\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, rs.repo, "checkout", "-q", "-b", "main")
	git(t, rs.repo, "add", ".")
	git(t, rs.repo, "commit", "-q", "-m", "init")
	git(t, rs.repo, "push", "-q", "-u", "origin", "main")
	rs.head = git(t, rs.repo, "rev-parse", "HEAD")
	// A run writes role memory; it is synced to central when the run ends.
	run, err := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Kind: "worker", Role: "implementor-backend", Harness: hns.KindClaude})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "run running", func() bool { return e.run1(run.ID).Observed == "running" })
	mem := filepath.Join(a.dir, "memory", sp.ID, "implementor-backend", "instances", e.run1(run.ID).InstanceID)
	if err := os.WriteFile(filepath.Join(mem, "decisions.md"), []byte("- relocate cleanly\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.c.Fleet.StopRun(ctx, admin, run.ID, "kill", "done"); err != nil {
		t.Fatal(err)
	}
	eventuallyFor(t, "memory acknowledged by central", 20*time.Second, func() bool {
		c, err := e.c.Memory.Get(ctx, sp.ID, "implementor-backend")
		files, _ := e.c.Memory.Files(ctx, sp.ID, "implementor-backend")
		return err == nil && slices.Contains(files, "decisions.md") && c.Node != nil && !c.Node.Pending
	})
	c, _ := e.c.Memory.Get(ctx, sp.ID, "implementor-backend")
	rs.memory = c.Head
	return rs
}

func (e *fleetEnv) relocation(spaceID string) *fleet.Relocation {
	r, err := e.c.Fleet.GetRelocation(context.Background(), spaceID)
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

func (e *fleetEnv) waitRelocation(spaceID string, states ...string) *fleet.Relocation {
	e.t.Helper()
	var r *fleet.Relocation
	eventuallyFor(e.t, "relocation "+strings.Join(states, "/"), 45*time.Second, func() bool {
		r = e.relocation(spaceID)
		return slices.Contains(states, r.State)
	})
	return r
}

func (e *fleetEnv) pause(spaceID string) {
	e.t.Helper()
	s, _ := e.c.Fleet.GetSpace(context.Background(), spaceID)
	if _, err := e.c.Fleet.SetSpaceState(context.Background(), admin, spaceID, s.Version, "paused", fleet.PauseHold); err != nil {
		e.t.Fatal(err)
	}
}

func (e *fleetEnv) auditCount(action, outcome string) int {
	var n int
	_ = e.c.Store.ReadDB().QueryRowContext(context.Background(), `SELECT count(*) FROM audit_events WHERE action = ? AND outcome = ?`, action, outcome).Scan(&n)
	return n
}

// SM-INV-003, SM-SPACE-005/006/008, SM-NODE-006: a precheck changes nothing; a clean relocation of a stopped space
// blocks launches, syncs memory, checks the source, prepares the destination (repository cloned at the source's
// commit, memory restored at central's head), moves the assignment and the lease (new generation) in one step, leaves
// the space paused, retains the source folder, and the next run on the destination starts a fresh native session.
func TestRelocationClean(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	e := newFleetEnv(t)
	ctx := context.Background()
	a, b := e.addNode("node-a"), e.addNode("node-b")
	rs := setupRelocSpace(t, e, a, "clean-move")
	sp := rs.sp
	before, _ := e.c.Fleet.GetSpace(ctx, sp.ID)
	gen := before.Lease.Generation

	// A precheck inspects the source and reports; nothing changes.
	if _, err := e.c.Fleet.StartRelocation(ctx, admin, sp.ID, fleet.StartRelocationInput{DestinationNodeID: b.id, Precheck: true}); err != nil {
		t.Fatal(err)
	}
	pc := e.waitRelocation(sp.ID, fleet.RelocChecked, fleet.RelocFailed)
	if pc.State != fleet.RelocChecked || pc.Report == nil || len(pc.Report.Repositories) != 1 || len(pc.Issues) != 0 || !pc.Report.Flushed {
		t.Fatalf("precheck %+v issues %+v", pc, pc.Issues)
	}
	if pc.Report.Repositories[0].Head != rs.head || pc.Report.Repositories[0].Remotes[0].URL != rs.origin {
		t.Fatalf("precheck repository %+v", pc.Report.Repositories[0])
	}
	if s, _ := e.c.Fleet.GetSpace(ctx, sp.ID); s.NodeID != a.id || s.DesiredNodeID != "" || s.State != "active" {
		t.Fatalf("precheck changed the space: %+v", s)
	}

	// An active space is refused unless stop_runs is set (only stopped spaces move).
	if _, err := e.c.Fleet.StartRelocation(ctx, admin, sp.ID, fleet.StartRelocationInput{DestinationNodeID: b.id}); apperr.CodeOf(err) != apperr.CodePrecondition {
		t.Fatalf("active space relocated: %v", err)
	}
	if e.auditCount("relocation.requested", "denied") == 0 {
		t.Fatal("refused request not audited as denied")
	}
	e.pause(sp.ID)
	if _, err := e.c.Fleet.StartRelocation(ctx, admin, sp.ID, fleet.StartRelocationInput{DestinationNodeID: b.id}); err != nil {
		t.Fatal(err)
	}
	// SM-SPACE-006: the destination is desired while the effective assignment stays on the source.
	if s, _ := e.c.Fleet.GetSpace(ctx, sp.ID); s.NodeID != a.id || s.DesiredNodeID != b.id {
		t.Fatalf("during relocation: node %s desired %s", s.NodeID, s.DesiredNodeID)
	}
	if _, err := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Kind: "worker", Role: "planner", Harness: hns.KindClaude}); err == nil {
		t.Fatal("launch accepted while relocating")
	}
	if _, err := e.c.Fleet.SetSpaceState(ctx, admin, sp.ID, before.Version+1, "active", ""); err == nil {
		t.Fatal("resume accepted while relocating")
	}
	r := e.waitRelocation(sp.ID, fleet.RelocDone, fleet.RelocBlocked, fleet.RelocFailed)
	if r.State != fleet.RelocDone {
		t.Fatalf("relocation %s: %s %+v", r.State, r.Error, r.Issues)
	}
	after, _ := e.c.Fleet.GetSpace(ctx, sp.ID)
	if after.NodeID != b.id || after.DesiredNodeID != "" || after.State != "paused" {
		t.Fatalf("after relocation: %+v", after)
	}
	if r.GenerationAfter <= gen || r.Continuity.NativeSessions != "not_carried" {
		t.Fatalf("generation %d → %d, continuity %+v", gen, r.GenerationAfter, r.Continuity)
	}
	eventually(t, "destination holds the lease", func() bool {
		s, _ := e.c.Fleet.GetSpace(ctx, sp.ID)
		return s.Lease != nil && s.Lease.NodeID == b.id && s.Lease.Valid && s.Lease.Generation == r.GenerationAfter
	})
	// The destination has the repository at the source's commit and the role memory at central's head.
	dest := filepath.Join(b.dir, "spaces", sp.Workdir)
	if got := git(t, filepath.Join(dest, "app"), "rev-parse", "HEAD"); got != rs.head {
		t.Fatalf("destination head %s, source %s", got, rs.head)
	}
	if got := git(t, filepath.Join(b.dir, "memory", sp.ID, "implementor-backend", "shared.git"), "rev-parse", "refs/heads/main"); got != rs.memory {
		t.Fatalf("destination memory %s, central %s", got, rs.memory)
	}
	// The source folder is retained, not deleted, and nothing remains in place.
	eventually(t, "source folder retained", func() bool { return e.relocation(sp.ID).SourceState == "retained" })
	retained := filepath.Join(a.dir, "retained", sp.Workdir+"-"+r.ID[len(r.ID)-8:])
	if _, err := os.Stat(filepath.Join(retained, "space", "app", "README.md")); err != nil {
		t.Fatalf("retained copy: %v", err)
	}
	if _, err := os.Stat(filepath.Join(a.dir, "spaces", sp.Workdir)); !os.IsNotExist(err) {
		t.Fatal("the source folder is still in place")
	}
	// So is the Claude Code auto memory the earlier run's instance kept outside the space folder (ADR-0068).
	srcRuns, _ := e.c.Fleet.ListRuns(ctx, sp.ID, 10)
	if _, err := os.Stat(filepath.Join(retained, "automemory", srcRuns[len(srcRuns)-1].InstanceID, ".migrated")); err != nil {
		t.Fatalf("retained auto memory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(a.dir, "automemory", sp.ID)); !os.IsNotExist(err) {
		t.Fatal("the source's auto memory is still in place")
	}
	if e.auditCount("space.relocated", "success") != 1 {
		t.Fatal("relocation not audited")
	}

	// Resumed, the space runs on the destination; a resume of an old session starts fresh (SM-SPACE-008), and the
	// role memory is there.
	s, _ := e.c.Fleet.GetSpace(ctx, sp.ID)
	if _, err := e.c.Fleet.SetSpaceState(ctx, admin, sp.ID, s.Version, "active", ""); err != nil {
		t.Fatal(err)
	}
	runs, _ := e.c.Fleet.ListRuns(ctx, sp.ID, 10)
	old := runs[len(runs)-1]
	if old.SessionID == "" {
		t.Fatal("the earlier run has no session")
	}
	run, err := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Kind: "worker", Role: "implementor-backend", Harness: hns.KindClaude,
		InstanceID: old.InstanceID, SessionID: old.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "run on the destination", func() bool { return e.run1(run.ID).Observed == "running" })
	got := e.run1(run.ID)
	if got.NodeID != b.id || got.Generation != r.GenerationAfter || len(b.run.LiveByLabel("spaceman.run", run.ID)) != 1 || got.SessionID == old.SessionID {
		t.Fatalf("run %+v", got)
	}
	if !strings.Contains(string(got.Manifest), "fresh session, project continuity only") {
		t.Fatalf("manifest does not label the fresh session: %s", got.Manifest)
	}
	mem := filepath.Join(b.dir, "memory", sp.ID, "implementor-backend", "instances", old.InstanceID, "decisions.md")
	if bs, err := os.ReadFile(mem); err != nil || string(bs) != "- relocate cleanly\n" {
		t.Fatalf("memory on the destination: %q %v", bs, err)
	}
}

// SM-SPACE-005, SM-REPO-006: unpushed, changed and untracked state is refused (never lost silently), the space stays
// on its source; the operator's explicit export carries it to the destination.
func TestRelocationDirtyRefusedThenExported(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	e := newFleetEnv(t)
	ctx := context.Background()
	a, b := e.addNode("node-a"), e.addNode("node-b")
	rs := setupRelocSpace(t, e, a, "dirty-move")
	sp := rs.sp
	git(t, rs.repo, "checkout", "-q", "-b", "local-work")
	if err := os.WriteFile(filepath.Join(rs.repo, "feature.go"), []byte("package app\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, rs.repo, "add", ".")
	git(t, rs.repo, "commit", "-q", "-m", "not pushed")
	unpushed := git(t, rs.repo, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(rs.repo, "scratch.txt"), []byte("untracked\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.pause(sp.ID)
	if _, err := e.c.Fleet.StartRelocation(ctx, admin, sp.ID, fleet.StartRelocationInput{DestinationNodeID: b.id}); err != nil {
		t.Fatal(err)
	}
	r := e.waitRelocation(sp.ID, fleet.RelocBlocked, fleet.RelocDone, fleet.RelocFailed)
	if r.State != fleet.RelocBlocked || r.BlockedStep != "check" || !strings.HasPrefix(r.Error, "refused") {
		t.Fatalf("dirty relocation %s/%s: %s", r.State, r.BlockedStep, r.Error)
	}
	codes := map[string]bool{}
	for _, i := range r.Issues {
		codes[i.Code] = i.Exportable
	}
	if !codes["unpushed"] || !codes["untracked"] {
		t.Fatalf("issues %+v", r.Issues)
	}
	if s, _ := e.c.Fleet.GetSpace(ctx, sp.ID); s.NodeID != a.id {
		t.Fatal("a refused relocation moved the space")
	}
	if e.auditCount("relocation.refused", "denied") != 1 {
		t.Fatal("refusal not audited")
	}
	if _, err := os.Stat(filepath.Join(b.dir, "spaces", sp.Workdir, "app")); !os.IsNotExist(err) {
		t.Fatal("the destination was prepared despite the refusal")
	}
	// Explicit export.
	if _, err := e.c.Fleet.RetryRelocation(ctx, admin, sp.ID, fleet.RetryRelocationInput{DirtyState: "export"}); err != nil {
		t.Fatal(err)
	}
	r = e.waitRelocation(sp.ID, fleet.RelocDone, fleet.RelocBlocked, fleet.RelocFailed)
	if r.State != fleet.RelocDone || r.Export == nil || len(r.Export.Repos) != 1 {
		t.Fatalf("exported relocation %s: %s %+v", r.State, r.Error, r.Export)
	}
	dest := filepath.Join(b.dir, "spaces", sp.Workdir, "app")
	if got := git(t, dest, "rev-parse", "HEAD"); got != unpushed {
		t.Fatalf("destination head %s, unpushed %s", got, unpushed)
	}
	if bs, err := os.ReadFile(filepath.Join(dest, "scratch.txt")); err != nil || string(bs) != "untracked\n" {
		t.Fatalf("untracked file: %q %v", bs, err)
	}
	if st := git(t, dest, "status", "--porcelain"); st != "?? scratch.txt" {
		t.Fatalf("destination status %q", st)
	}
	if git(t, dest, "remote", "get-url", "origin") != rs.origin {
		t.Fatal("remote not carried")
	}
}

// SM-SPACE-007, SM-NODE-006: with the source offline, a relocation needs an acknowledgement of possible loss (audited),
// uses central's copies only, marks the source's runs lost, and fences the source when it comes back: its run is
// stopped, it gets no lease, and its folder is retained.
func TestRelocationUnavailableSource(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	e := newFleetEnv(t)
	ctx := context.Background()
	a, b := e.addNode("node-a"), e.addNode("node-b")
	rs := setupRelocSpace(t, e, a, "lost-node")
	sp := rs.sp
	run, err := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Kind: "worker", Role: "planner", Harness: hns.KindClaude})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "run running", func() bool { return e.run1(run.ID).Observed == "running" })
	gen := e.run1(run.ID).Generation
	a.stop() // the node goes away with its run still running
	eventually(t, "run unknown", func() bool { return e.run1(run.ID).Observed == "unknown" })

	// The clean flow needs the source.
	if _, err := e.c.Fleet.StartRelocation(ctx, admin, sp.ID, fleet.StartRelocationInput{DestinationNodeID: b.id, StopRuns: true}); apperr.CodeOf(err) != apperr.CodePrecondition {
		t.Fatalf("clean relocation from an offline source: %v", err)
	}
	// No acknowledgement, no relocation.
	in := fleet.StartRelocationInput{DestinationNodeID: b.id, StopRuns: true, SourceUnavailable: true,
		Repositories: []fleet.RepoSpec{{Path: "app", URL: rs.origin, Branch: "main"}}}
	if _, err := e.c.Fleet.StartRelocation(ctx, admin, sp.ID, in); apperr.CodeOf(err) != apperr.CodeInvalid {
		t.Fatalf("unacknowledged: %v", err)
	}
	in.AcknowledgeLoss, in.Acknowledgement = true, "node-a died; anything not on central is gone"
	r, err := e.c.Fleet.StartRelocation(ctx, admin, sp.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	if r.Loss == nil || !slices.Contains(r.Loss.Runs, run.ID) || len(r.Loss.Memory) != 1 || len(r.Loss.Unknown) == 0 {
		t.Fatalf("loss report %+v", r.Loss)
	}
	r = e.waitRelocation(sp.ID, fleet.RelocDone, fleet.RelocBlocked, fleet.RelocFailed)
	if r.State != fleet.RelocDone || r.Acknowledgement == "" {
		t.Fatalf("relocation %s: %s", r.State, r.Error)
	}
	var details string
	var sealed []byte
	if err := e.c.Store.ReadDB().QueryRowContext(ctx, `SELECT details_json, details_enc FROM audit_events WHERE action = 'relocation.loss_acknowledged'`).
		Scan(&details, &sealed); err != nil || !strings.Contains(details, `"acknowledged_loss":true`) || len(sealed) == 0 {
		t.Fatalf("acknowledgement audit %s %v", details, err)
	}
	if got := e.run1(run.ID); got.Observed != "lost" || got.Desired != "stopped" {
		t.Fatalf("source run %+v", got)
	}
	dest := filepath.Join(b.dir, "spaces", sp.Workdir)
	if got := git(t, filepath.Join(dest, "app"), "rev-parse", "HEAD"); got != rs.head {
		t.Fatalf("destination clone at %s", got)
	}
	if got := git(t, filepath.Join(b.dir, "memory", sp.ID, "implementor-backend", "shared.git"), "rev-parse", "refs/heads/main"); got != rs.memory {
		t.Fatal("memory not restored from central")
	}
	after, _ := e.c.Fleet.GetSpace(ctx, sp.ID)
	if after.NodeID != b.id || after.State != "paused" {
		t.Fatalf("space %+v", after)
	}

	// The source returns: its old run (of a fenced generation) is stopped, it holds no lease, and its folder is retained.
	if len(a.run.LiveByLabel("spaceman.run", run.ID)) != 1 {
		t.Fatal("test setup: the run should still be live on the stopped node")
	}
	a.start()
	eventually(t, "source reconnected", func() bool { n, _ := e.c.Fleet.GetNode(ctx, a.id); return n.Connected })
	eventuallyFor(t, "fenced run stopped on the returning source", 30*time.Second, func() bool {
		return len(a.run.LiveByLabel("spaceman.run", run.ID)) == 0
	})
	if got := e.run1(run.ID); got.Observed != "lost" || got.Generation != gen {
		t.Fatalf("the fenced report changed the run: %+v", got)
	}
	if s, _ := e.c.Fleet.GetSpace(ctx, sp.ID); s.Lease == nil || s.Lease.NodeID != b.id {
		t.Fatalf("lease %+v", s.Lease)
	}
	eventuallyFor(t, "source folder retained", 20*time.Second, func() bool { return e.relocation(sp.ID).SourceState == "retained" })
	// Releasing the retained folder deletes it on the source.
	if _, err := e.c.Fleet.ReleaseRelocationSource(ctx, admin, sp.ID); err != nil {
		t.Fatal(err)
	}
	eventually(t, "retained folder deleted", func() bool { return e.relocation(sp.ID).SourceState == "deleted" })
	if _, err := os.Stat(filepath.Join(a.dir, "retained", sp.Workdir+"-"+r.ID[len(r.ID)-8:])); !os.IsNotExist(err) {
		t.Fatal("retained folder still present")
	}
}

// A central restart in the middle of a relocation resumes it from the database (the destination was offline).
func TestRelocationResumesAfterCentralRestart(t *testing.T) {
	// Not parallel: central restarts must finish within the node lease, which parallel tests can starve.
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	e := newFleetEnv(t)
	ctx := context.Background()
	a, b := e.addNode("node-a"), e.addNode("node-b")
	rs := setupRelocSpace(t, e, a, "restart-move")
	sp := rs.sp
	b.stop()
	e.pause(sp.ID)
	if _, err := e.c.Fleet.StartRelocation(ctx, admin, sp.ID, fleet.StartRelocationInput{DestinationNodeID: b.id}); err != nil {
		t.Fatal(err)
	}
	r := e.waitRelocation(sp.ID, fleet.RelocPreparing, fleet.RelocBlocked, fleet.RelocFailed)
	if r.State != fleet.RelocPreparing {
		t.Fatalf("relocation %s: %s", r.State, r.Error)
	}
	eventually(t, "prepare queued", func() bool {
		x, _ := e.c.Store.R().GetRelocation(ctx, r.ID)
		return x.StepCommandID != ""
	})
	e.restartCentral()
	b.start()
	r = e.waitRelocation(sp.ID, fleet.RelocDone, fleet.RelocBlocked, fleet.RelocFailed)
	if r.State != fleet.RelocDone {
		t.Fatalf("after restart: %s %s", r.State, r.Error)
	}
	if s, _ := e.c.Fleet.GetSpace(ctx, sp.ID); s.NodeID != b.id {
		t.Fatal("not relocated")
	}
	if got := git(t, filepath.Join(b.dir, "spaces", sp.Workdir, "app"), "rev-parse", "HEAD"); got != rs.head {
		t.Fatal("destination not prepared")
	}
}

// SM-INV-003: nothing moves a space without an explicit request. A source that is gone for many lease periods keeps
// its spaces, no relocation is created, and launches wait for it.
func TestRelocationNeverAutomatic(t *testing.T) {
	// Not parallel: short leases (shortLease) expire if parallel tests starve the node.
	e := newFleetEnv(t, shortLease)
	ctx := context.Background()
	a, _ := e.addNode("node-a"), e.addNode("node-b")
	sp, err := e.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "stays-put", Name: "Stays", NodeID: a.id})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "lease", func() bool { x, _ := e.c.Fleet.GetSpace(ctx, sp.ID); return x.Lease != nil && x.Lease.Valid })
	a.stop()
	time.Sleep(4 * time.Second) // several lease TTLs and disconnect graces
	s, _ := e.c.Fleet.GetSpace(ctx, sp.ID)
	if s.NodeID != a.id || s.DesiredNodeID != "" {
		t.Fatalf("placement changed on its own: %+v", s)
	}
	if rs, _ := e.c.Fleet.ListRelocations(ctx, sp.ID, 10); len(rs) != 0 {
		t.Fatalf("relocations created automatically: %+v", rs)
	}
	var n int
	if err := e.c.Store.ReadDB().QueryRowContext(ctx, `SELECT count(*) FROM relocations`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("relocations %d %v", n, err)
	}
	if _, err := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Kind: "worker", Role: "planner", Harness: hns.KindClaude}); apperr.CodeOf(err) != apperr.CodeUnavailable {
		t.Fatalf("launch without the pinned node: %v", err)
	}
}

// The relocation API: authorization rules exist for every operation, and the read model is served.
func TestRelocationOverHTTP(t *testing.T) {
	t.Parallel()
	e := newFleetEnv(t)
	ctx := context.Background()
	a, b := e.addNode("node-a"), e.addNode("node-b")
	sp, err := e.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "http-move", Name: "HTTP", NodeID: a.id})
	if err != nil {
		t.Fatal(err)
	}
	root, _, err := e.c.Auth.IssueRootToken(ctx)
	if err != nil {
		t.Fatal(err)
	}
	do := func(method, path string, body any) (int, []byte) {
		var rd io.Reader
		if body != nil {
			bs, _ := json.Marshal(body)
			rd = bytes.NewReader(bs)
		}
		req, _ := http.NewRequest(method, e.srv.URL+"/api/v1/spaces/"+sp.ID+path, rd)
		req.Header.Set("Authorization", "Bearer "+root)
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		out, _ := io.ReadAll(res.Body)
		return res.StatusCode, out
	}
	if code, _ := do(http.MethodGet, "/relocation", nil); code != http.StatusNotFound {
		t.Fatalf("no relocation yet: %d", code)
	}
	if code, out := do(http.MethodPost, "/relocation", map[string]any{"destination_node_id": b.id, "precheck": true}); code != http.StatusAccepted {
		t.Fatalf("precheck: %d %s", code, out)
	}
	eventually(t, "precheck done", func() bool {
		_, out := do(http.MethodGet, "/relocation", nil)
		var r fleet.Relocation
		_ = json.Unmarshal(out, &r)
		return r.State == fleet.RelocChecked
	})
	code, out := do(http.MethodGet, "/relocations", nil)
	if code != http.StatusOK || !strings.Contains(string(out), `"continuity"`) {
		t.Fatalf("list: %d %s", code, out)
	}
	if code, _ := do(http.MethodPost, "/relocation/cancel", nil); code != http.StatusPreconditionFailed && code != http.StatusConflict && code != http.StatusUnprocessableEntity {
		t.Fatalf("cancel without a relocation in progress: %d", code)
	}
}
