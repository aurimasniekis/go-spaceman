package agent

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/harness"
	"github.com/aurimasniekis/go-spaceman/internal/obs"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/runner"
)

// LeaseFile is the content of a space's lease file, read by the lease guard.
type LeaseFile struct {
	SpaceID    string    `json:"space_id"`
	Generation int64     `json:"generation"`
	ValidUntil time.Time `json:"valid_until"`
}

// guardSlack is how long after the daemon's own enforcement point the guard ends a space's runs (the daemon stops
// them politely first; the guard is the hard bound when the daemon is gone).
const guardSlack = 10 * time.Second

func (a *Agent) grant(ctx context.Context, g proto.LeaseGrant) {
	l := leaseRow{SpaceID: g.SpaceID, Generation: g.Generation, ExpiresAt: g.ExpiresAt, Workdir: g.Workdir, DisconnectPolicy: g.DisconnectPolicy, Grace: g.Grace}
	if l.DisconnectPolicy == "" {
		l.DisconnectPolicy = "interrupt"
	}
	a.mu.Lock()
	prev, had := a.leases[g.SpaceID]
	a.leases[g.SpaceID] = l
	a.mu.Unlock()
	if err := a.j.putLease(ctx, l); err != nil {
		a.log.Warn("journal lease", obs.Err(err))
	}
	if err := a.j.putWorkdir(ctx, g.SpaceID, g.Workdir); err != nil {
		a.log.Warn("journal space folder", obs.Err(err))
	}
	if err := writeJSONFile(a.leaseFile(g.SpaceID), LeaseFile{SpaceID: g.SpaceID, Generation: g.Generation,
		ValidUntil: g.ExpiresAt.Add(g.Grace + guardSlack)}); err != nil {
		a.log.Warn("lease file", obs.Err(err))
	}
	if err := os.MkdirAll(filepath.Join(a.spaceDir(g.Workdir), "work"), 0o700); err != nil {
		a.log.Warn("space folder", obs.Err(err))
	}
	if had && prev.Generation != g.Generation {
		// A new assignment epoch: runs of the old generation are fenced and stopped (SM-NODE-006).
		a.stopSpaceRuns(ctx, g.SpaceID, func(r runRow) bool { return r.Generation != g.Generation }, "kill", "fenced: lease generation changed")
	}
}

func (a *Agent) revoke(ctx context.Context, r proto.LeaseRevoke) {
	a.mu.Lock()
	l, ok := a.leases[r.SpaceID]
	if ok && l.Generation == r.Generation {
		delete(a.leases, r.SpaceID)
	}
	a.mu.Unlock()
	if !ok || l.Generation != r.Generation {
		return
	}
	_ = a.j.deleteLease(ctx, r.SpaceID)
	_ = writeJSONFile(a.leaseFile(r.SpaceID), LeaseFile{SpaceID: r.SpaceID, Generation: r.Generation, ValidUntil: a.now()})
	mode := r.Mode
	if mode != "kill" {
		mode = "interrupt"
	}
	a.stopSpaceRuns(ctx, r.SpaceID, func(runRow) bool { return true }, mode, "lease revoked: "+r.Reason)
}

// command handles one command. It is journaled before any side effect; a repeat is acknowledged as duplicate and
// never re-executed (SM-NODE-004). Commands for a generation the node does not hold are rejected (fencing).
func (a *Agent) command(ctx context.Context, c proto.Command) {
	if err := c.Validate(); err != nil {
		a.ack(ctx, c, proto.AckRejected, err.Error())
		return
	}
	fresh, err := a.j.commandSeen(ctx, c)
	if err != nil {
		a.log.Warn("journal command", obs.Err(err))
		return // no ack: central re-sends after reconnect
	}
	if proto.IsRelocation(c.Kind) {
		// Relocation steps are not execution: no lease is needed (ADR-0037). A repeat re-sends the stored outcome.
		if !fresh {
			a.ack(ctx, c, proto.AckDuplicate, "")
		}
		a.handleRelocation(ctx, c, fresh)
		return
	}
	if !fresh && c.Kind == proto.CmdReposSync {
		a.ack(ctx, c, proto.AckDuplicate, "")
		a.resendReposSync(ctx, c)
		return
	}
	if !fresh {
		a.ack(ctx, c, proto.AckDuplicate, "")
		if r, err := a.j.getRun(ctx, c.RunID); err == nil {
			a.report(ctx, r)
		}
		return
	}
	if c.Kind != proto.CmdStop {
		if _, ok := a.lease(c.SpaceID, c.Generation); !ok {
			a.j.commandDone(ctx, c.CommandID, "rejected", "no valid lease for this space and generation")
			a.ack(ctx, c, proto.AckRejected, "no valid lease for this space and generation")
			return
		}
	}
	switch c.Kind {
	case proto.CmdLaunch:
		var p proto.LaunchPayload
		if err := json.Unmarshal(c.Payload, &p); err != nil {
			a.ack(ctx, c, proto.AckRejected, "bad launch payload")
			return
		}
		a.ack(ctx, c, proto.AckAccepted, "")
		go a.launch(context.WithoutCancel(ctx), c, p)
	case proto.CmdStop:
		var p proto.StopPayload
		_ = json.Unmarshal(c.Payload, &p)
		a.ack(ctx, c, proto.AckAccepted, "")
		go a.stop(context.WithoutCancel(ctx), c.RunID, p.Mode, time.Duration(p.Grace)*time.Second, p.Reason)
	case proto.CmdReposSync:
		a.handleReposSync(ctx, c)
		return // the sync journals its own outcome (its report) when it finishes
	case proto.CmdKBAccept:
		a.handleKBAccept(ctx, c)
	case proto.CmdDreamDone:
		a.handleDreamCommit(ctx, c)
	case proto.CmdInterrupt:
		a.ack(ctx, c, proto.AckAccepted, "")
		if r, err := a.j.getRun(ctx, c.RunID); err == nil && r.TerminalID != "" {
			_, _ = a.o.Runner.Interrupt(ctx, r.TerminalID)
		}
	case proto.CmdInput:
		var p proto.InputPayload
		_ = json.Unmarshal(c.Payload, &p)
		r, err := a.j.getRun(ctx, c.RunID)
		if err != nil || r.TerminalID == "" || ended(r.State) {
			a.ack(ctx, c, proto.AckRejected, "run is not running")
			return
		}
		// Automated input is paused while an operator has taken over the terminal (SM-UI-004).
		if a.takenOver(c.RunID) {
			a.ack(ctx, c, proto.AckRejected, "an operator has taken over the terminal; input is paused")
			return
		}
		if err := a.o.Runner.Send(ctx, r.TerminalID, p.Text, !p.Force); err != nil {
			reason := "send failed"
			if errors.Is(err, runner.ErrBusy) {
				reason = "the program is busy; input not delivered (safe input)"
			}
			a.ack(ctx, c, proto.AckRejected, reason)
			return
		}
		a.ack(ctx, c, proto.AckAccepted, "")
	default:
		a.ack(ctx, c, proto.AckRejected, "unsupported command")
	}
	a.j.commandDone(ctx, c.CommandID, "done", "")
}

// launch starts (or adopts) a run's process. The intent is journaled first; if a terminal with the run's name already
// exists (a repeated delivery, or a restart between start and journal update) it is adopted (SM-NODE-004/009).
func (a *Agent) launch(ctx context.Context, c proto.Command, p proto.LaunchPayload) {
	log := a.log.With(slog.String(obs.FieldRunID, c.RunID), slog.String(obs.FieldSpaceID, c.SpaceID))
	l, _ := a.lease(c.SpaceID, c.Generation)
	row := runRow{RunID: c.RunID, SpaceID: c.SpaceID, Generation: c.Generation, Kind: p.Kind, Role: p.Role, Harness: p.Harness, InstanceID: p.InstanceID,
		TerminalName: RunName(c.RunID), Desired: "running", State: proto.RunPending, SessionID: p.SessionID}
	if err := a.j.insertRun(ctx, row); err != nil {
		log.Error("journal run intent", obs.Err(err))
		return
	}
	row, _ = a.j.getRun(ctx, c.RunID)
	if err := a.saveRecordingQuota(c.RunID, p.RecordingMaxBytes); err != nil {
		log.Warn("store the recording bound; the default applies", obs.Err(err))
	}
	labels := map[string]string{"spaceman.managed": "1", "spaceman.run": c.RunID, "spaceman.space": c.SpaceID}
	if found, err := a.o.Runner.List(ctx, map[string]string{"spaceman.run": c.RunID}, true); err == nil && len(found) > 0 {
		a.observe(ctx, row, found[0], true)
		return
	}
	fail := func(msg string, err error) {
		row.State, row.Error = proto.RunFailed, msg
		if err != nil {
			row.Error = msg + ": " + err.Error()
		}
		_ = a.j.updateRun(ctx, row)
		a.report(ctx, row)
		log.Warn("launch failed", slog.String("reason", msg), obs.Err(err))
	}
	h, err := harness.Get(p.Harness)
	if err != nil {
		fail("unknown harness", err)
		return
	}
	bin := a.o.HarnessBinaries[p.Harness]
	if bin == "" {
		fail("harness not installed on this node", nil)
		return
	}
	// The space's guard lock is held until the run is started under the guard, so retiring an idle space's guard
	// (retireGuard) never ends it under a starting run.
	gl := a.guardLock(c.SpaceID)
	gl.Lock()
	defer gl.Unlock()
	guard, err := a.ensureGuard(ctx, c.SpaceID)
	if err != nil {
		fail("lease guard unavailable", err)
		return
	}
	space := a.spaceDir(l.Workdir)
	work := filepath.Join(space, "work")
	if p.Workspace != "" {
		work = filepath.Join(space, filepath.Clean("/"+p.Workspace))
	}
	if p.Worktree != nil && p.Repository != nil && p.Repository.Path == p.Worktree.Repo {
		// A configured repository is cloned at its first use when its policy allows it (ADR-0044).
		if err := a.ensureRepo(ctx, c.SpaceID, space, *p.Repository); err != nil {
			fail("clone the repository", err)
			return
		}
	}
	if p.Worktree != nil {
		dir, err := prepareWorktree(ctx, space, *p.Worktree)
		if err != nil {
			fail("prepare worktree", err)
			return
		}
		work = dir
	}
	if p.Dream != nil {
		dir, err := a.prepareDream(ctx, c.SpaceID, space, c.RunID, p.InstanceID, *p.Dream)
		if err != nil {
			fail("prepare the dream", err)
			return
		}
		work = dir
	}
	rd := a.runDir(c.RunID)
	for _, d := range []string{rd, work, filepath.Join(space, "instances", p.InstanceID)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			fail("prepare folders", err)
			return
		}
	}
	// Saved attachments of this instance's earlier runs that ended without cleaning up (ADR-0073).
	a.pruneAttachments(ctx, l.Workdir, p.InstanceID, c.RunID)
	// Harnesses key per-directory state by the real path (macOS: /tmp is /private/tmp).
	if real, err := filepath.EvalSymlinks(work); err == nil {
		work = real
	}
	spaceReal := space
	if real, err := filepath.EvalSymlinks(space); err == nil {
		spaceReal = real
	}
	// Role memory (ADR-0025): the instance's checkout, refreshed from the shared history at this safe boundary. A node
	// without git runs without memory rather than not at all.
	memDir, err := a.mem.prepare(ctx, c.SpaceID, p.Role, p.InstanceID)
	if err != nil {
		log.Warn("role memory unavailable for this run", obs.Err(err))
		memDir = ""
	} else if real, err := filepath.EvalSymlinks(memDir); err == nil {
		memDir = real
	}
	if memDir != "" && !p.Resume {
		p.SystemText = strings.TrimSpace(p.SystemText + "\n\n" + memoryNote(memDir, p.Role))
	}
	// Claude Code's auto memory is kept outside the space folder, where no other run's file tools reach (ADR-0068).
	var autoMem string
	if p.Harness == harness.KindClaude {
		if _, set := p.ClaudeSettings["autoMemoryDirectory"]; set {
			log.Warn("the profile's autoMemoryDirectory is ignored: SpaceMan keeps each instance's auto memory in its own folder")
		}
		autoMem, err = PrepareAutoMemory(filepath.Join(space, "instances", p.InstanceID, "claude"), a.autoMemoryDir(c.SpaceID, p.InstanceID), work)
		if err != nil {
			fail("prepare auto memory", err)
			return
		}
	}
	built, err := h.Build(harness.Context{AgentBinary: a.o.AgentBinary, BridgeSocket: a.o.BridgeSocket, ConfigDir: filepath.Join(space, "instances", p.InstanceID),
		SpaceDir: spaceReal, SpaceInstances: spaceInstances(space), WorkDir: work, RunID: c.RunID, SpaceID: c.SpaceID, RunDir: rd, Binary: bin, Payload: p, MemoryDir: memDir,
		AutoMemoryDir: autoMem})
	if err != nil {
		fail("build launch", err)
		return
	}
	for path, content := range built.Files {
		if err := os.WriteFile(path, content, 0o600); err != nil {
			fail("write harness files", err)
			return
		}
	}
	for path, patch := range built.MergeJSON {
		if err := MergeJSONFile(path, patch); err != nil {
			fail("write harness state", err)
			return
		}
	}
	for path, keys := range built.OwnJSON {
		if err := OwnJSONKeysFile(log, path, keys, built.StripJSON[path]...); err != nil {
			fail("write harness settings", err)
			return
		}
	}
	if built.SkillsRoot != "" {
		if err := syncSkills(built.SkillsRoot, p.Skills); err != nil {
			fail("write skills", err)
			return
		}
	} else if len(p.Skills) > 0 {
		log.Warn("this harness's skill loading is not verified; skills were not materialized", slog.Int("skills", len(p.Skills)))
	}
	if err := a.linkLogin(built.Login, built.LoginLinks); err != nil {
		fail("write login", err)
		return
	}
	if err := removeCredentialFiles(built.RemoveFiles); err != nil { // after the login links are gone
		fail("remove stored credentials", err)
		return
	}
	if err := removeCredentialDirs(built.RemoveDirs); err != nil {
		fail("remove stored credentials", err)
		return
	}
	if err := writeSecretFiles(built.SecretFiles); err != nil { // after the login links are gone
		fail("write login", err)
		return
	}
	a.rememberRunToken(c.RunID, p.RunToken)
	if err := writeSecrets(rd, built.SecretEnv, p.RunToken); err != nil {
		fail("write secret file", err)
		return
	}
	argv := a.wrap(rd, built)
	row.State, row.SessionID = proto.RunStarting, built.SessionID
	_ = a.j.updateRun(ctx, row)
	a.report(ctx, row)
	proc, err := a.o.Runner.Start(ctx, runner.Spec{Name: row.TerminalName, Argv: argv, Cwd: work, Env: built.Env, Labels: labels, Parent: guard})
	if errors.Is(err, runner.ErrNameTaken) {
		if found, lerr := a.o.Runner.List(ctx, map[string]string{"spaceman.run": c.RunID}, true); lerr == nil && len(found) > 0 {
			a.observe(ctx, row, found[0], true)
			return
		}
	}
	if err != nil {
		_ = os.Remove(filepath.Join(rd, SecretsFile))
		fail("start", err)
		return
	}
	a.observe(ctx, row, *proc, false)
}

func (a *Agent) wrap(runDir string, l harness.Launch) []string {
	if a.o.Wrap != nil {
		return a.o.Wrap(runDir, l)
	}
	return append([]string{a.o.AgentBinary, "launch", "--secrets-file", filepath.Join(runDir, SecretsFile), "--"}, l.Argv...)
}

// observe updates a run from the runner's view of its process.
func (a *Agent) observe(ctx context.Context, row runRow, p runner.Process, adopted bool) {
	row.TerminalID, row.CoreID, row.Identity, row.PID = p.ID, p.CoreID, p.Identity(), p.PID
	row.Adopted = row.Adopted || adopted
	row.Status, row.StatusKind, row.Turn = p.Status.State, p.Status.Kind, p.Turn
	switch p.State {
	case runner.StateLive:
		if row.Desired == "stopped" {
			row.State = proto.RunStopping
		} else {
			row.State = proto.RunRunning
		}
	case runner.StateFinished:
		row.ExitCode, row.Reason = p.ExitCode, p.Reason
		row.State = proto.RunExited
		if p.Reason == "holder_lost" || p.Reason == "server_stop" || p.Reason == "host_shutdown" {
			row.State = proto.RunLost
		}
	}
	prev, _ := a.j.getRun(ctx, row.RunID)
	_ = a.j.updateRun(ctx, row)
	a.report(ctx, row)
	if p.State == runner.StateLive {
		a.startRecorder(ctx, row)
	}
	// Role memory checkpoints at idle points (debounced) and at once when the run ends (SM-MEM-004/005).
	switch {
	case ended(row.State) && !ended(prev.State):
		bctx := context.WithoutCancel(ctx)
		a.goTrack(func() {
			a.mem.flush(bctx, row, "run ended")
			a.usageFinal(bctx, row)
		})
		a.goTrack(func() { a.retireGuard(bctx, row.SpaceID) })
		a.goTrack(func() { a.removeRunAttachments(bctx, row) })
	case row.State == proto.RunRunning && (row.Status == "idle" || row.Status == "done") && (prev.Status != row.Status || prev.Turn != row.Turn):
		a.mem.kick(row)
		a.usageKick(context.WithoutCancel(ctx), row)
	}
}

// stop ends a run: interrupt, then kill if still live after grace; or kill at once.
func (a *Agent) stop(ctx context.Context, runID, mode string, grace time.Duration, reason string) {
	row, err := a.j.getRun(ctx, runID)
	if err != nil {
		return
	}
	row.Desired = "stopped"
	if ended(row.State) {
		_ = a.j.updateRun(ctx, row)
		a.report(ctx, row)
		return
	}
	if row.TerminalID == "" {
		row.State, row.Reason = proto.RunExited, "stopped before start"
		_ = a.j.updateRun(ctx, row)
		a.report(ctx, row)
		return
	}
	row.State = proto.RunStopping
	_ = a.j.updateRun(ctx, row)
	a.report(ctx, row)
	if mode != "kill" {
		_, _ = a.o.Runner.Interrupt(ctx, row.TerminalID)
		if grace <= 0 {
			grace = 15 * time.Second
		}
		deadline := time.Now().Add(grace)
		for time.Now().Before(deadline) {
			p, err := a.o.Runner.Get(ctx, row.TerminalID)
			if err != nil || p.State == runner.StateFinished {
				break
			}
			time.Sleep(250 * time.Millisecond)
		}
	}
	if p, err := a.o.Runner.Get(ctx, row.TerminalID); err == nil && p.State == runner.StateLive {
		if _, err := a.o.Runner.Kill(ctx, row.TerminalID); err != nil {
			a.log.Warn("kill", slog.String(obs.FieldRunID, runID), obs.Err(err))
		}
	}
	a.log.Info("run stopped", slog.String(obs.FieldRunID, runID), slog.String("reason", reason))
	// The finished event (monitor) records the final state; record it here too in case events are delayed.
	if p, err := a.o.Runner.Get(ctx, row.TerminalID); err == nil {
		row, _ = a.j.getRun(ctx, runID)
		a.observe(ctx, row, *p, false)
	}
}

func (a *Agent) stopSpaceRuns(ctx context.Context, spaceID string, match func(runRow) bool, mode, reason string) {
	runs, err := a.j.runs(ctx, true)
	if err != nil {
		return
	}
	for _, r := range runs {
		if r.SpaceID == spaceID && match(r) {
			go a.stop(context.WithoutCancel(ctx), r.RunID, mode, 15*time.Second, reason)
		}
	}
}

// reconcileDesired applies central's desired state after (re)connection.
func (a *Agent) reconcileDesired(ctx context.Context, rec proto.Reconcile) {
	want := map[string]proto.DesiredRun{}
	for _, d := range rec.Runs {
		want[d.RunID] = d
	}
	runs, err := a.j.runs(ctx, true)
	if err != nil {
		return
	}
	for _, r := range runs {
		d, ok := want[r.RunID]
		switch {
		case !ok:
			// Unknown to central (or already ended there): report; central fences and stops it if needed.
			a.report(ctx, r)
		case d.Desired == "stopped" || d.Generation != r.Generation:
			go a.stop(context.WithoutCancel(ctx), r.RunID, "interrupt", 15*time.Second, "central reconcile")
		default:
			a.report(ctx, r)
		}
	}
}

// reconcileRunner re-reads every managed terminal and brings the journal up to date (startup, missed events).
func (a *Agent) reconcileRunner(ctx context.Context) error {
	procs, err := a.o.Runner.List(ctx, map[string]string{"spaceman.managed": "1"}, true)
	if err != nil {
		return err
	}
	byRun := map[string]runner.Process{}
	for _, p := range procs {
		if id := p.Labels["spaceman.run"]; id != "" {
			byRun[id] = p
		}
	}
	runs, err := a.j.runs(ctx, true)
	if err != nil {
		return err
	}
	for _, r := range runs {
		p, ok := byRun[r.RunID]
		switch {
		case ok:
			a.observe(ctx, r, p, r.TerminalID == "" || r.TerminalID != p.ID)
		case r.State == proto.RunPending:
			// Intent journaled but never started (crash before start): leave it for central's reconcile; a launch
			// re-delivery is a duplicate, so mark it failed rather than starting it silently.
			r.State, r.Error = proto.RunFailed, "agent restarted before the process started"
			_ = a.j.updateRun(ctx, r)
			a.removeRunAttachments(ctx, r)
		default:
			r.State, r.Reason = proto.RunLost, "process not found in the runner"
			_ = a.j.updateRun(ctx, r)
			a.removeRunAttachments(ctx, r)
		}
	}
	return nil
}

// monitor follows runner events to keep run states and statuses current (no polling).
func (a *Agent) monitor(ctx context.Context) {
	for ctx.Err() == nil {
		evs, err := a.o.Runner.Subscribe(ctx, map[string]string{"spaceman.managed": "1"})
		if err != nil {
			a.log.Warn("runner events", obs.Err(err))
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
			continue
		}
		for ev := range evs {
			switch ev.Kind {
			case "snapshot", "missed", "restarted", "lost":
				if err := a.reconcileRunner(ctx); err != nil {
					a.log.Warn("runner reconcile", obs.Err(err))
				}
			case "finished", "status", "turn":
				if ev.TerminalID == "" {
					continue
				}
				row, err := a.j.runByTerminal(ctx, ev.TerminalID)
				if err != nil {
					continue
				}
				if p, err := a.o.Runner.Get(ctx, ev.TerminalID); err == nil {
					a.observe(ctx, row, *p, false)
				}
			}
		}
	}
}

// enforce stops runs once central has been gone longer than the lease plus grace (SM-NODE-007). It wakes only at the
// next deadline (one timer per node, not per space).
func (a *Agent) enforce(ctx context.Context) {
	for {
		next := time.Hour
		a.mu.Lock()
		connected := a.connected
		var expired []leaseRow
		for _, l := range a.leases {
			until := l.ExpiresAt.Add(l.Grace)
			if !connected {
				if !a.now().Before(until) {
					expired = append(expired, l)
				} else if d := until.Sub(a.now()); d < next {
					next = d
				}
			}
		}
		for _, l := range expired {
			delete(a.leases, l.SpaceID)
		}
		a.mu.Unlock()
		for _, l := range expired {
			a.log.Warn("lease expired without central; stopping the space's runs", slog.String(obs.FieldSpaceID, l.SpaceID), slog.String("policy", l.DisconnectPolicy))
			a.stopSpaceRuns(ctx, l.SpaceID, func(runRow) bool { return true }, l.DisconnectPolicy, "central unreachable beyond lease and grace")
		}
		select {
		case <-ctx.Done():
			return
		case <-a.wake:
		case <-time.After(next):
		}
	}
}

// ensureGuard returns the space's live lease-guard terminal, starting it if needed (ADR-0017).
func (a *Agent) ensureGuard(ctx context.Context, spaceID string) (string, error) {
	name := GuardName(spaceID)
	found, err := a.o.Runner.List(ctx, map[string]string{"spaceman.guard": spaceID}, true)
	if err != nil {
		return "", err
	}
	for _, p := range found {
		if p.State == runner.StateLive {
			return p.ID, nil
		}
		_ = a.o.Runner.Remove(ctx, p.ID)
	}
	p, err := a.o.Runner.Start(ctx, runner.Spec{Name: name, Argv: []string{a.o.AgentBinary, "lease-guard", "--lease-file", a.leaseFile(spaceID)},
		Cwd: a.o.DataDir, Labels: map[string]string{"spaceman.guard": spaceID}})
	if errors.Is(err, runner.ErrNameTaken) {
		if found, lerr := a.o.Runner.List(ctx, map[string]string{"spaceman.guard": spaceID}, false); lerr == nil && len(found) > 0 {
			return found[0].ID, nil
		}
	}
	if err != nil {
		return "", err
	}
	return p.ID, nil
}

// guardLock returns the lock that serializes a space's lease guard.
func (a *Agent) guardLock(spaceID string) *sync.Mutex {
	a.mu.Lock()
	defer a.mu.Unlock()
	m := a.guardLocks[spaceID]
	if m == nil {
		m = &sync.Mutex{}
		a.guardLocks[spaceID] = m
	}
	return m
}

// retireGuard ends a space's lease guard once none of the space's runs is live on the node (ADR-0043, SM-ARCH-004): a
// space without runs keeps no process polling its lease file. The guard has nothing left to end, and the next launch
// starts a new one before its run.
func (a *Agent) retireGuard(ctx context.Context, spaceID string) {
	gl := a.guardLock(spaceID)
	gl.Lock()
	defer gl.Unlock()
	live, err := a.o.Runner.List(ctx, map[string]string{"spaceman.managed": "1", "spaceman.space": spaceID}, false)
	if err != nil || len(live) > 0 {
		return
	}
	guards, err := a.o.Runner.List(ctx, map[string]string{"spaceman.guard": spaceID}, true)
	if err != nil {
		return
	}
	for _, g := range guards {
		if g.State == runner.StateLive {
			if _, err := a.o.Runner.Kill(ctx, g.ID); err != nil {
				a.log.Warn("retire the lease guard", slog.String(obs.FieldSpaceID, spaceID), obs.Err(err))
				continue
			}
		}
		_ = a.o.Runner.Remove(ctx, g.ID)
	}
}

// SecretsFile is the per-run secret file read (and unlinked) by the launcher shim.
const SecretsFile = "secrets.json"

func writeSecrets(runDir string, env map[string]string, runToken string) error {
	all := map[string]string{}
	for k, v := range env {
		all[k] = v
	}
	if runToken != "" {
		all["SPACEMAN_RUN_TOKEN"] = runToken
	}
	if len(all) == 0 {
		return nil
	}
	return writeJSONFile(filepath.Join(runDir, SecretsFile), all)
}

func (a *Agent) startRecorder(ctx context.Context, row runRow) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.recorders[row.RunID]; ok {
		return
	}
	rctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	a.recorders[row.RunID] = cancel
	go func() {
		defer func() {
			a.mu.Lock()
			delete(a.recorders, row.RunID)
			a.mu.Unlock()
			cancel()
		}()
		a.record(rctx, row)
	}()
}

// spaceInstances lists the instance folders of a space (the names in <space>/instances), for the harness's rules that keep a
// run out of the other instances' folders (ADR-0068). A folder that can't be listed has none.
func spaceInstances(space string) []string {
	entries, err := os.ReadDir(filepath.Join(space, "instances"))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out
}
