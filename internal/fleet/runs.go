package fleet

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/keyring"
	"github.com/aurimasniekis/go-spaceman/internal/obs/metrics"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Run is one execution attempt of an agent instance (SM-NODE-005: desired and observed state are separate).
type Run struct {
	ID         string     `json:"id"`
	SpaceID    string     `json:"space_id"`
	NodeID     string     `json:"node_id"`
	Generation int64      `json:"generation" doc:"Lease generation the run belongs to"`
	Kind       string     `json:"kind" enum:"control,worker,kb,dreamer,diagnostic"`
	Role       string     `json:"role"`
	Harness    string     `json:"harness" enum:"claude-code,codex"`
	InstanceID string     `json:"instance_id"`
	SessionID  string     `json:"session_id,omitempty"`
	Desired    string     `json:"desired" enum:"running,stopped"`
	Observed   string     `json:"observed" enum:"pending,starting,running,stopping,exited,failed,lost,unknown"`
	TerminalID string     `json:"terminal_id,omitempty"`
	PID        int64      `json:"pid,omitempty"`
	ExitCode   *int64     `json:"exit_code,omitempty"`
	EndReason  string     `json:"end_reason,omitempty"`
	Error      string     `json:"error,omitempty"`
	Status     string     `json:"status,omitempty" doc:"Program-reported status (OSC 7501): idle, working, done, blocked, error"`
	StatusKind string     `json:"status_kind,omitempty"`
	Turn       int64      `json:"turn"`
	Adopted    bool       `json:"adopted"`
	Launch     LaunchSpec `json:"launch"`
	Version    int64      `json:"version"`
	CreatedAt  time.Time  `json:"created_at"`
	CreatedBy  string     `json:"created_by" doc:"Who launched it: user:<id>, agent:<run id>, root or system:<component>"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	EndedAt    *time.Time `json:"ended_at,omitempty"`
	ObservedAt *time.Time `json:"observed_at,omitempty"`
}

// LaunchSpec is the non-secret launch description stored with the run.
type LaunchSpec struct {
	Prompt     string            `json:"prompt,omitempty"`
	SystemText string            `json:"system,omitempty"`
	Model      string            `json:"model,omitempty"`
	Settings   map[string]string `json:"settings,omitempty"`
	Workspace  string            `json:"workspace,omitempty"`
	Worktree   *proto.Worktree   `json:"worktree,omitempty"`
	AccountRef string            `json:"account_ref,omitempty"`
}

func runFromRow(r db.Run) *Run {
	out := &Run{ID: r.ID, SpaceID: r.SpaceID, NodeID: r.NodeID, Generation: r.Generation, Kind: r.Kind, Role: r.Role, Harness: r.Harness,
		InstanceID: r.InstanceID, SessionID: r.SessionID, Desired: r.Desired, Observed: r.Observed, TerminalID: r.TerminalID, PID: r.Pid,
		EndReason: r.EndReason, Error: r.Error, Status: r.Status, StatusKind: r.StatusKind, Turn: r.Turn, Adopted: r.Adopted == 1, Version: r.Version,
		CreatedAt: time.UnixMicro(r.CreatedAt).UTC(), CreatedBy: r.CreatedBy, StartedAt: microsPtr(r.StartedAt), EndedAt: microsPtr(r.EndedAt), ObservedAt: microsPtr(r.ObservedAt)}
	if r.ExitCode.Valid {
		v := r.ExitCode.Int64
		out.ExitCode = &v
	}
	_ = json.Unmarshal([]byte(r.LaunchJson), &out.Launch)
	return out
}

var (
	errRunNotFound = apperr.New(apperr.CodeNotFound, "run not found")
	reRole         = regexp.MustCompile(`^[a-z][a-z0-9-]{0,47}$`)
	reBranch       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,199}$`)
)

// LaunchInput starts a run.
type LaunchInput struct {
	Kind       string            `json:"kind" enum:"control,worker,kb,dreamer,diagnostic"`
	Role       string            `json:"role" doc:"Agent kind/role, e.g. control, implementor-backend (not the harness)"`
	Harness    string            `json:"harness" enum:"claude-code,codex"`
	InstanceID string            `json:"instance_id,omitempty" doc:"Reuse an instance's harness config and memory checkout; empty creates one"`
	SessionID  string            `json:"session_id,omitempty" doc:"Resume this conversation when the harness supports it"`
	Prompt     string            `json:"prompt,omitempty" maxLength:"100000"`
	SystemText string            `json:"system,omitempty" maxLength:"100000"`
	Model      string            `json:"model,omitempty" maxLength:"128"`
	Settings   map[string]string `json:"settings,omitempty"`
	Workspace  string            `json:"workspace,omitempty" maxLength:"256"`
	Worktree   *proto.Worktree   `json:"worktree,omitempty" doc:"Run in a git worktree of a repository inside the space folder"`
	AccountRef string            `json:"account_ref,omitempty"`
}

// LaunchRun creates a run pinned to the space's node and current lease generation and queues the launch command.
// New work is refused while the space is paused, archived or relocating, or when the node holds no valid lease
// (it is disconnected): disconnected nodes start no new work (SM-NODE-007), and placement never changes (SM-INV-003).
func (svc *Service) LaunchRun(ctx context.Context, actor audit.Actor, spaceID string, in LaunchInput) (*Run, error) {
	v := &apperr.Validation{}
	switch in.Kind {
	case "control", "worker", "kb", "dreamer", "diagnostic":
	default:
		v.Add("kind", "must be control, worker, kb, dreamer or diagnostic")
	}
	if !reRole.MatchString(in.Role) {
		v.Add("role", "must be a lowercase role name")
	}
	if in.Harness != "claude-code" && in.Harness != "codex" {
		v.Add("harness", "must be claude-code or codex")
	}
	if in.InstanceID != "" && !ids.Valid(ids.Instance, in.InstanceID) {
		v.Add("instance_id", "must be an instance ID")
	}
	if strings.Contains(in.Workspace, "..") || strings.HasPrefix(in.Workspace, "/") {
		v.Add("workspace", "must be a relative path inside the space")
	}
	if w := in.Worktree; w != nil {
		if w.Repo == "" || strings.Contains(w.Repo, "..") || strings.HasPrefix(w.Repo, "/") {
			v.Add("worktree.repo", "must be a relative path inside the space")
		}
		if !reBranch.MatchString(w.Branch) || strings.Contains(w.Branch, "..") || strings.HasSuffix(w.Branch, ".lock") {
			v.Add("worktree.branch", "must be a plain branch name (letters, digits, / . _ -)")
		}
		if w.Base != "" && (!reBranch.MatchString(w.Base) || strings.Contains(w.Base, "..")) {
			v.Add("worktree.base", "must be a branch, tag or commit name")
		}
		if in.Workspace != "" {
			v.Add("workspace", "a worktree launch runs in its worktree; leave workspace empty")
		}
	}
	if err := v.Err(); err != nil {
		return nil, err
	}
	if in.InstanceID == "" {
		in.InstanceID = ids.New(ids.Instance)
	}
	runID := ids.New(ids.Run)
	var nodeID string
	err := svc.s.W().TxNamed(ctx, "fleet.launch_run", func(tx *store.Tx) error {
		sp, err := tx.Q.GetSpace(ctx, spaceID)
		if errors.Is(err, sql.ErrNoRows) {
			return errSpaceNotFound
		} else if err != nil {
			return err
		}
		switch {
		case sp.State == "archived":
			return apperr.New(apperr.CodePrecondition, "the space is archived")
		case sp.State == "paused":
			return apperr.New(apperr.CodePrecondition, "the space is paused (%s); resume it to start work", sp.PauseMode)
		case sp.DesiredNodeID.Valid:
			return apperr.New(apperr.CodePrecondition, "the space is relocating; launches are blocked until it finishes")
		}
		lease, err := tx.Q.GetLease(ctx, spaceID)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && (lease.State != "active" || !tx.Now.Before(time.UnixMicro(lease.ExpiresAt)) || lease.NodeID != sp.NodeID)) {
			return apperr.New(apperr.CodeUnavailable, "the space's node is not connected (no valid execution lease); no new work starts until it reconnects")
		} else if err != nil {
			return err
		}
		nodeID = sp.NodeID
		if svc.opts.ResolveAccount != nil {
			if in.AccountRef, err = svc.opts.ResolveAccount(ctx, tx, spaceID, in.Harness, in.AccountRef); err != nil {
				return err
			}
		}
		spec := LaunchSpec{Prompt: in.Prompt, SystemText: in.SystemText, Model: in.Model, Settings: in.Settings, Workspace: in.Workspace, Worktree: in.Worktree,
			AccountRef: in.AccountRef}
		specJSON, _ := json.Marshal(spec)
		manifest, _ := json.Marshal(map[string]any{"harness": in.Harness, "role": in.Role, "kind": in.Kind, "lease_generation": lease.Generation,
			"account_ref": in.AccountRef, "model": in.Model})
		if err := tx.Q.InsertRun(ctx, db.InsertRunParams{ID: runID, SpaceID: spaceID, NodeID: sp.NodeID, Generation: lease.Generation, Kind: in.Kind,
			Role: in.Role, Harness: in.Harness, InstanceID: in.InstanceID, SessionID: in.SessionID, LaunchJson: string(specJSON), ManifestJson: string(manifest),
			CreatedAt: tx.NowMicros(), CreatedBy: actorRef(actor), UpdatedAt: tx.NowMicros()}); err != nil {
			if errors.Is(err, store.ErrConstraint) && in.Kind == "control" {
				return apperr.New(apperr.CodeConflict, "the space already has an authoritative control run")
			}
			return err
		}
		payload := proto.LaunchPayload{Harness: in.Harness, Role: in.Role, Kind: in.Kind, InstanceID: in.InstanceID, SessionID: in.SessionID,
			Resume: in.SessionID != "", Prompt: in.Prompt, SystemText: in.SystemText, Model: in.Model, Settings: in.Settings, Workspace: in.Workspace,
			Worktree: in.Worktree, AccountRef: in.AccountRef}
		if err := svc.queueCommand(ctx, tx, sp.NodeID, spaceID, runID, lease.Generation, proto.CmdLaunch, "launch:"+runID, payload); err != nil {
			return err
		}
		if err := svc.record(ctx, tx, spaceID, "run", runID, "created", 1); err != nil {
			return err
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "run.launched", Target: audit.Target{Type: "run", ID: runID},
			Details: map[string]any{"space_id": spaceID, "kind": in.Kind, "role": in.Role, "harness": in.Harness, "generation": lease.Generation}})
		return err
	})
	if err != nil {
		return nil, err
	}
	svc.hub.Kick(nodeID)
	return svc.GetRun(ctx, runID)
}

func (svc *Service) queueCommand(ctx context.Context, tx *store.Tx, nodeID, spaceID, runID string, gen int64, kind, key string, payload any) error {
	_, err := svc.queueCommandID(ctx, tx, nodeID, spaceID, runID, gen, kind, key, payload)
	return err
}

func (svc *Service) queueCommandID(ctx context.Context, tx *store.Tx, nodeID, spaceID, runID string, gen int64, kind, key string, payload any) (string, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	id := ids.New(ids.Command)
	return id, tx.Q.InsertNodeCommand(ctx, db.InsertNodeCommandParams{ID: id, NodeID: nodeID, SpaceID: spaceID,
		RunID: sql.NullString{String: runID, Valid: runID != ""}, Generation: gen, Kind: kind, IdempotencyKey: key, PayloadJson: string(b),
		CreatedAt: tx.NowMicros()})
}

// QueueNotice queues safe (idle-gated) input for a running run inside the caller's transaction and returns the
// command ID; the node refuses it while the program is busy (SM-COMM-003). Call KickRun after the commit.
func (svc *Service) QueueNotice(ctx context.Context, tx *store.Tx, runID, text string) (string, error) {
	r, err := tx.Q.GetRun(ctx, runID)
	if err != nil {
		return "", err
	}
	if r.Desired != "running" || ended(r.Observed) {
		return "", apperr.New(apperr.CodePrecondition, "the run is not running")
	}
	return svc.queueCommandID(ctx, tx, r.NodeID, r.SpaceID, r.ID, r.Generation, proto.CmdInput, "notice:"+ids.New(ids.Message), proto.InputPayload{Text: text})
}

// MarkEndedRunStopped sets an ended run's desired state to stopped, so it no longer holds the space's single control
// slot (SM-INV-006) and is not treated as wanted.
func (svc *Service) MarkEndedRunStopped(ctx context.Context, runID string) error {
	return svc.s.W().TxNamed(ctx, "fleet.run_settle", func(tx *store.Tx) error {
		r, err := tx.Q.GetRun(ctx, runID)
		if err != nil {
			return err
		}
		if !ended(r.Observed) || r.Desired == "stopped" {
			return nil
		}
		if _, err := tx.Q.SetRunDesired(ctx, db.SetRunDesiredParams{Desired: "stopped", UpdatedAt: tx.NowMicros(), ID: runID}); err != nil {
			return err
		}
		return svc.record(ctx, tx, r.SpaceID, "run", r.ID, "updated", r.Version+1)
	})
}

// CountRunsCreatedBy counts a space's runs of a kind created by an actor since a time (bounded recovery).
func (svc *Service) CountRunsCreatedBy(ctx context.Context, spaceID, kind string, actor audit.Actor, since time.Time) (int64, error) {
	return svc.s.R().CountRunsCreatedBy(ctx, db.CountRunsCreatedByParams{SpaceID: spaceID, Kind: kind, CreatedBy: actorRef(actor), CreatedAt: since.UnixMicro()})
}

// KickRun wakes the dispatch loop of the run's node.
func (svc *Service) KickRun(ctx context.Context, runID string) {
	if r, err := svc.s.R().GetRun(ctx, runID); err == nil {
		svc.hub.Kick(r.NodeID)
	}
}

// failUndispatched ends a run whose launch could not be sent (its account became unusable after the launch was
// accepted): the launch command is cancelled and the run is failed with the reason, so it does not wait forever.
func (svc *Service) failUndispatched(ctx context.Context, runID, reason string) error {
	err := svc.s.W().TxNamed(ctx, "fleet.run_undispatched", func(tx *store.Tx) error {
		r, err := tx.Q.GetRun(ctx, runID)
		if err != nil {
			return err
		}
		if _, err := tx.Q.CancelRunCommands(ctx, db.CancelRunCommandsParams{Reason: reason, RunID: sql.NullString{String: runID, Valid: true}}); err != nil {
			return err
		}
		if _, err := tx.Q.UpdateRunObserved(ctx, db.UpdateRunObservedParams{Observed: proto.RunFailed, EndReason: "account_unavailable", Error: reason,
			Now: sql.NullInt64{Int64: tx.NowMicros(), Valid: true}, MarkEnded: 1, ID: r.ID, Generation: r.Generation}); err != nil {
			return err
		}
		if _, err := tx.Q.RevokeRunCredentials(ctx, db.RevokeRunCredentialsParams{RevokedAt: sql.NullInt64{Int64: tx.NowMicros(), Valid: true}, RunID: r.ID}); err != nil {
			return err
		}
		if err := svc.record(ctx, tx, r.SpaceID, "run", r.ID, "updated", r.Version+1); err != nil {
			return err
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: audit.System, Action: "run.launch_failed", Target: audit.Target{Type: "run", ID: r.ID},
			Details: map[string]any{"space_id": r.SpaceID, "reason": reason}})
		return err
	})
	if err == nil && svc.opts.OnRunEnded != nil {
		svc.opts.OnRunEnded(runID)
	}
	return err
}

// StopRun sets a run's desired state to stopped and queues a stop command (interrupt politely, then kill after the
// grace; or kill at once).
func (svc *Service) StopRun(ctx context.Context, actor audit.Actor, runID, mode, reason string) (*Run, error) {
	if mode == "" {
		mode = "interrupt"
	}
	if mode != "interrupt" && mode != "kill" {
		return nil, apperr.New(apperr.CodeInvalid, "mode must be interrupt or kill")
	}
	var nodeID string
	err := svc.s.W().TxNamed(ctx, "fleet.stop_run", func(tx *store.Tx) error {
		r, err := tx.Q.GetRun(ctx, runID)
		if errors.Is(err, sql.ErrNoRows) {
			return errRunNotFound
		} else if err != nil {
			return err
		}
		nodeID = r.NodeID
		if err := svc.stopRunTx(ctx, tx, r, mode, reason); err != nil {
			return err
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "run.stop_requested", Target: audit.Target{Type: "run", ID: runID},
			Details: map[string]any{"mode": mode, "reason": reason}})
		return err
	})
	if err != nil {
		return nil, err
	}
	svc.hub.Kick(nodeID)
	return svc.GetRun(ctx, runID)
}

func (svc *Service) stopRunTx(ctx context.Context, tx *store.Tx, r db.Run, mode, reason string) error {
	if _, err := tx.Q.SetRunDesired(ctx, db.SetRunDesiredParams{Desired: "stopped", UpdatedAt: tx.NowMicros(), ID: r.ID}); err != nil {
		return err
	}
	// A launch not yet delivered is cancelled instead of being started and stopped.
	if _, err := tx.Q.CancelRunCommands(ctx, db.CancelRunCommandsParams{Reason: "stop requested before delivery", RunID: sql.NullString{String: r.ID, Valid: true}}); err != nil {
		return err
	}
	if err := svc.queueCommand(ctx, tx, r.NodeID, r.SpaceID, r.ID, r.Generation, proto.CmdStop, "stop:"+r.ID+":"+mode,
		proto.StopPayload{Mode: mode, Grace: 15, Reason: reason}); err != nil {
		return err
	}
	if _, err := tx.Q.RevokeRunCredentials(ctx, db.RevokeRunCredentialsParams{RevokedAt: sql.NullInt64{Int64: tx.NowMicros(), Valid: true}, RunID: r.ID}); err != nil {
		return err
	}
	return svc.record(ctx, tx, r.SpaceID, "run", r.ID, "updated", r.Version+1)
}

// SendInput queues safe input for a run (delivered only when the program reports idle, unless forced).
func (svc *Service) SendInput(ctx context.Context, actor audit.Actor, runID, text string, force bool) error {
	if text == "" || len(text) > 64<<10 {
		return apperr.New(apperr.CodeInvalid, "text must be 1-65536 bytes")
	}
	var nodeID string
	err := svc.s.W().TxNamed(ctx, "fleet.send_input", func(tx *store.Tx) error {
		r, err := tx.Q.GetRun(ctx, runID)
		if errors.Is(err, sql.ErrNoRows) {
			return errRunNotFound
		} else if err != nil {
			return err
		}
		if r.Desired != "running" {
			return apperr.New(apperr.CodePrecondition, "the run is stopping or stopped")
		}
		nodeID = r.NodeID
		if err := svc.queueCommand(ctx, tx, r.NodeID, r.SpaceID, r.ID, r.Generation, proto.CmdInput, "input:"+ids.New(ids.Message),
			proto.InputPayload{Text: text, Force: force}); err != nil {
			return err
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "run.input", Target: audit.Target{Type: "run", ID: runID},
			Details: map[string]any{"bytes": len(text), "force": force}})
		return err
	})
	if err == nil {
		svc.hub.Kick(nodeID)
	}
	return err
}

// GetRun returns a run.
func (svc *Service) GetRun(ctx context.Context, id string) (*Run, error) {
	r, err := svc.s.R().GetRun(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errRunNotFound
	} else if err != nil {
		return nil, err
	}
	return runFromRow(r), nil
}

// ListRuns returns a space's runs, newest first.
func (svc *Service) ListRuns(ctx context.Context, spaceID string, limit int) ([]*Run, error) {
	rows, err := svc.s.R().ListRunsBySpace(ctx, db.ListRunsBySpaceParams{SpaceID: spaceID, Limit: int64(limit)})
	if err != nil {
		return nil, err
	}
	out := make([]*Run, 0, len(rows))
	for _, r := range rows {
		out = append(out, runFromRow(r))
	}
	return out, nil
}

// applyReport records a node's observation of a run. Reports for another node or an older lease generation are
// fenced (SM-NODE-006); a run central wants stopped but the node reports running gets a new stop command.
func (svc *Service) applyReport(ctx context.Context, nodeID string, rep proto.RunReport) (fenced bool, err error) {
	becameIdle, justEnded := false, false
	switch rep.State {
	case proto.RunPending, proto.RunStarting, proto.RunRunning, proto.RunStopping, proto.RunExited, proto.RunFailed, proto.RunLost:
	default:
		return false, apperr.New(apperr.CodeInvalid, "unknown run state %q", rep.State)
	}
	err = svc.s.W().TxNamed(ctx, "fleet.run_report", func(tx *store.Tx) error {
		r, err := tx.Q.GetRun(ctx, rep.RunID)
		if errors.Is(err, sql.ErrNoRows) {
			fenced = true
			return nil
		} else if err != nil {
			return err
		}
		if r.NodeID != nodeID || r.SpaceID != rep.SpaceID || r.Generation != rep.Generation {
			fenced = true
			return nil
		}
		var exit sql.NullInt64
		if rep.ExitCode != nil {
			exit = sql.NullInt64{Int64: int64(*rep.ExitCode), Valid: true}
		}
		if _, err := tx.Q.UpdateRunObserved(ctx, db.UpdateRunObservedParams{Observed: rep.State, TerminalID: rep.TerminalID, CoreID: rep.CoreID,
			Identity: rep.Identity, Pid: int64(rep.PID), ExitCode: exit, EndReason: rep.Reason, Error: truncate(rep.Error, 2000), Status: rep.Status,
			StatusKind: rep.StatusKind, Turn: int64(min(rep.Turn, math.MaxInt64)), //nolint:gosec // saturated above
			Adopted: b2i(rep.Adopted), SessionID: rep.SessionID, Now: sql.NullInt64{Int64: tx.NowMicros(), Valid: true},
			MarkStarted: b2i(rep.State == proto.RunRunning || rep.State == proto.RunExited || rep.State == proto.RunFailed), MarkEnded: b2i(ended(rep.State)),
			ID: r.ID, Generation: r.Generation}); err != nil {
			return err
		}
		if r.Observed != rep.State {
			svc.opts.Metrics.RunStarted(metrics.Harness(r.Harness), startResult(rep))
			if ended(rep.State) {
				svc.opts.Metrics.RunEnded(metrics.Harness(r.Harness), metrics.RunEndReason(endReason(rep)))
				if _, err := tx.Q.RevokeRunCredentials(ctx, db.RevokeRunCredentialsParams{RevokedAt: sql.NullInt64{Int64: tx.NowMicros(), Valid: true}, RunID: r.ID}); err != nil {
					return err
				}
			}
		}
		if r.Desired == "stopped" && !ended(rep.State) {
			if err := svc.queueCommand(ctx, tx, r.NodeID, r.SpaceID, r.ID, r.Generation, proto.CmdStop, "stop:"+r.ID+":reconcile",
				proto.StopPayload{Mode: "interrupt", Grace: 15, Reason: "central wants this run stopped"}); err != nil {
				return err
			}
		}
		becameIdle = !ended(rep.State) && (rep.Status == "idle" || rep.Status == "done") && (r.Status != rep.Status || r.Observed != rep.State)
		justEnded = ended(rep.State) && !ended(r.Observed)
		return svc.record(ctx, tx, r.SpaceID, "run", r.ID, "updated", r.Version+1)
	})
	if err == nil && becameIdle && svc.opts.OnRunIdle != nil {
		svc.opts.OnRunIdle(rep.RunID)
	}
	if err == nil && justEnded && svc.opts.OnRunEnded != nil {
		svc.opts.OnRunEnded(rep.RunID)
	}
	if fenced {
		svc.opts.Metrics.LeaseFenced()
	}
	return fenced, err
}

func ended(s string) bool { return s == proto.RunExited || s == proto.RunFailed || s == proto.RunLost }

func startResult(rep proto.RunReport) metrics.RunStartResult {
	switch {
	case rep.State == proto.RunFailed && rep.Error != "":
		return metrics.RunFailed
	case rep.Adopted:
		return metrics.RunAdopted
	}
	return metrics.RunLaunched
}

func endReason(rep proto.RunReport) string {
	switch rep.Reason {
	case "exited", "killed", "lease_expired", "stopped":
		return rep.Reason
	case "holder_lost", "server_stop", "host_shutdown":
		return "lost"
	}
	if rep.State == proto.RunLost {
		return "lost"
	}
	return "exited"
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func b2i(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// RunPrincipal is the identity behind a run credential (agents; never a Casbin subject, ADR-0012).
type RunPrincipal struct {
	RunID      string
	SpaceID    string
	Kind       string
	Role       string
	InstanceID string
	Generation int64
}

// runToken returns the run's credential, creating it on first use. A run keeps one token for its lifetime: the launch
// may be re-sent until the node acknowledges it, and every copy must carry the token the harness already holds. The
// token is stored as a MAC (for authentication) and sealed (for re-delivery); revocation ends it (run ended).
func (svc *Service) runToken(ctx context.Context, runID string) (string, error) {
	if enc, err := svc.s.R().GetActiveRunToken(ctx, runID); err == nil {
		b, err := svc.kr.Open(keyring.PurposeCredential, runTokenAAD(runID), enc)
		if err != nil {
			return "", err
		}
		return string(b), nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	tok, err := randToken(RunTokenPrefix)
	if err != nil {
		return "", err
	}
	m, err := svc.mac(tok)
	if err != nil {
		return "", err
	}
	sealed, err := svc.kr.Seal(keyring.PurposeCredential, runTokenAAD(runID), []byte(tok))
	if err != nil {
		return "", err
	}
	err = svc.s.W().TxNamed(ctx, "fleet.issue_run_token", func(tx *store.Tx) error {
		// Another dispatch may have created it meanwhile: reuse that one.
		if enc, err := tx.Q.GetActiveRunToken(ctx, runID); err == nil {
			b, err := svc.kr.Open(keyring.PurposeCredential, runTokenAAD(runID), enc)
			tok = string(b)
			return err
		}
		return tx.Q.InsertRunCredential(ctx, db.InsertRunCredentialParams{Mac: m, RunID: runID, CreatedAt: tx.NowMicros(), TokenEnc: sealed})
	})
	return tok, err
}

func runTokenAAD(runID string) []byte { return []byte("run_token:" + runID) }

// AuthenticateRun resolves a run credential while the run is active and its credential unrevoked.
func (svc *Service) AuthenticateRun(ctx context.Context, tok string) (*RunPrincipal, error) {
	if !strings.HasPrefix(tok, RunTokenPrefix) || len(tok) != len(RunTokenPrefix)+43 {
		return nil, errBadCredential
	}
	macs, err := svc.macs(tok)
	if err != nil {
		return nil, err
	}
	for _, m := range macs {
		c, err := svc.s.R().GetRunCredential(ctx, m)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		} else if err != nil {
			return nil, err
		}
		if c.RevokedAt.Valid || c.Desired != "running" || ended(c.Observed) {
			return nil, errBadCredential
		}
		return &RunPrincipal{RunID: c.RunID, SpaceID: c.SpaceID, Kind: c.Kind, Role: c.Role, InstanceID: c.InstanceID, Generation: c.Generation}, nil
	}
	return nil, errBadCredential
}
