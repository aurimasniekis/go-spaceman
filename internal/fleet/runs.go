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
	"github.com/aurimasniekis/go-spaceman/internal/buildinfo"
	"github.com/aurimasniekis/go-spaceman/internal/harness"
	"github.com/aurimasniekis/go-spaceman/internal/harness/dist"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/keyring"
	"github.com/aurimasniekis/go-spaceman/internal/obs/metrics"
	"github.com/aurimasniekis/go-spaceman/internal/outbox"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Run is one execution attempt of an agent instance (SM-NODE-005: desired and observed state are separate).
type Run struct {
	ID         string `json:"id"`
	SpaceID    string `json:"space_id"`
	NodeID     string `json:"node_id"`
	Generation int64  `json:"generation" doc:"Lease generation the run belongs to"`
	Kind       string `json:"kind" enum:"control,worker,kb,dreamer,diagnostic"`
	Role       string `json:"role"`
	Harness    string `json:"harness" enum:"claude-code,codex"`
	InstanceID string `json:"instance_id"`
	SessionID  string `json:"session_id,omitempty"`
	Desired    string `json:"desired" enum:"running,stopped"`
	Observed   string `json:"observed" enum:"pending,starting,running,stopping,exited,failed,lost,unknown"`
	TerminalID string `json:"terminal_id,omitempty"`
	PID        int64  `json:"pid,omitempty"`
	ExitCode   *int64 `json:"exit_code,omitempty"`
	EndReason  string `json:"end_reason,omitempty" doc:"Why the run ended: the node's reason (exited, stopped, killed, lease_expired, …), usage_limit when it ended while at a usage limit, or central's stale_not_on_node, stale_lease_lost or relocated_source_unavailable"`
	Error      string `json:"error,omitempty"`
	Status     string `json:"status,omitempty" doc:"Program-reported status (OSC 7501): idle, working, done, blocked, error"`
	StatusKind string `json:"status_kind,omitempty"`
	Turn       int64  `json:"turn"`
	Adopted    bool   `json:"adopted"`
	// Limit is the run's open usage limit (ADR-0079): a live run with one is waiting for it to reset.
	Limit *RunLimit `json:"limit,omitempty" doc:"The run's open usage limit, as its node reported it (node protocol 1.12 or later); a live run with one is waiting for the limit to reset"`
	// HarnessVersion, HarnessRequested and HarnessFallback are what the node reported launching (minor ≥ 11, ADR-0077).
	HarnessVersion   string     `json:"harness_version,omitempty" doc:"The harness version the run launched, as its node reported it (node protocol 1.11 or later)"`
	HarnessRequested string     `json:"harness_requested,omitempty" doc:"What the run asked for: the profile's pin, else the node's channel (latest, stable or X.Y.Z)"`
	HarnessFallback  string     `json:"harness_fallback,omitempty" doc:"Why another version than the request's target ran (a node-managed channel whose target was not cached yet or unavailable); absent when the target ran"`
	Launch           LaunchSpec `json:"launch"`
	Version          int64      `json:"version"`
	CreatedAt        time.Time  `json:"created_at"`
	CreatedBy        string     `json:"created_by" doc:"Who launched it: user:<id>, agent:<run id>, root or system:<component>"`
	// Manifest is the non-secret resolved configuration the run started with (SM-PROMPT-004): prompt versions and
	// content, profile, template and space-role revisions, effective values with their origins, account reference,
	// dependency and policy versions.
	Manifest   json.RawMessage `json:"manifest,omitempty" doc:"Omitted from run lists unless include=manifest"`
	StartedAt  *time.Time      `json:"started_at,omitempty"`
	EndedAt    *time.Time      `json:"ended_at,omitempty"`
	ObservedAt *time.Time      `json:"observed_at,omitempty" doc:"When the node last reported a change to the run (a report that changes nothing, such as a reconnect re-reporting it, doesn't move it)"`
	// Takeover is the terminal takeover in force (SM-UI-004); set by GetRun.
	Takeover *Takeover `json:"takeover,omitempty" doc:"The terminal takeover in force: an operator types into the terminal and automated input is paused"`
	// TerminalSize is the size the run's terminal is held at (ADR-0075); nil is the node's default size.
	TerminalSize *TerminalSize `json:"terminal_size,omitempty" doc:"The size the run's terminal is held at; absent: the node's default size"`
	// TerminalSizing is set by GetRun: whether the run's node can hold a terminal size now.
	TerminalSizing bool `json:"terminal_sizing,omitempty" doc:"Whether the run's node is connected at node protocol 1.10 or later, so its terminal can be resized (set on a single run)"`
}

// LaunchSpec is the non-secret launch description stored with the run.
type LaunchSpec struct {
	Prompt     string            `json:"prompt,omitempty" doc:"Omitted from run lists unless include=prompt"`
	SystemText string            `json:"system,omitempty" doc:"Omitted from run lists unless include=prompt"`
	Model      string            `json:"model,omitempty"`
	Settings   map[string]string `json:"settings,omitempty"`
	Workspace  string            `json:"workspace,omitempty"`
	Worktree   *proto.Worktree   `json:"worktree,omitempty"`
	AccountRef string            `json:"account_ref,omitempty"`
	// Gateway is set when the run's model traffic goes through an LLM gateway instead of an account (ADR-0060).
	Gateway *GatewaySpec `json:"gateway,omitempty"`
}

func runFromRow(r db.Run) *Run {
	out := &Run{ID: r.ID, SpaceID: r.SpaceID, NodeID: r.NodeID, Generation: r.Generation, Kind: r.Kind, Role: r.Role, Harness: r.Harness,
		InstanceID: r.InstanceID, SessionID: r.SessionID, Desired: r.Desired, Observed: r.Observed, TerminalID: r.TerminalID, PID: r.Pid,
		EndReason: r.EndReason, Error: r.Error, Status: r.Status, StatusKind: r.StatusKind, Turn: r.Turn, Adopted: r.Adopted == 1, Version: r.Version,
		HarnessVersion: r.HarnessVersion, HarnessRequested: r.HarnessRequested, HarnessFallback: r.HarnessFallback,
		CreatedAt: time.UnixMicro(r.CreatedAt).UTC(), CreatedBy: r.CreatedBy, Manifest: json.RawMessage(r.ManifestJson), StartedAt: microsPtr(r.StartedAt), EndedAt: microsPtr(r.EndedAt), ObservedAt: microsPtr(r.ObservedAt)}
	if r.ExitCode.Valid {
		v := r.ExitCode.Int64
		out.ExitCode = &v
	}
	_ = json.Unmarshal([]byte(r.LaunchJson), &out.Launch)
	out.TerminalSize = terminalSizeOf(r.TerminalCols, r.TerminalRows)
	out.Limit = runLimitOf(r)
	return out
}

var (
	errRunNotFound = apperr.New(apperr.CodeNotFound, "run not found")
	reRole         = regexp.MustCompile(`^[a-z][a-z0-9-]{0,47}$`)
	reBranch       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,199}$`)
)

// LaunchInput starts a run.
type LaunchInput struct {
	Kind       string            `json:"kind,omitempty" enum:"control,worker,kb,dreamer,diagnostic" doc:"Required unless role names a configured space role"`
	Role       string            `json:"role" doc:"A configured space role (its template, profile and prompt apply), or a free role name"`
	Harness    string            `json:"harness,omitempty" enum:"claude-code,codex" doc:"Required unless the role's profile sets it"`
	InstanceID string            `json:"instance_id,omitempty" doc:"Reuse an instance's harness config and memory checkout; empty creates one"`
	SessionID  string            `json:"session_id,omitempty" doc:"Resume this conversation when the harness supports it"`
	Prompt     string            `json:"prompt,omitempty" maxLength:"100000"`
	SystemText string            `json:"system,omitempty" maxLength:"100000"`
	Model      string            `json:"model,omitempty" maxLength:"128"`
	Settings   map[string]string `json:"settings,omitempty"`
	Workspace  string            `json:"workspace,omitempty" maxLength:"256"`
	Worktree   *proto.Worktree   `json:"worktree,omitempty" doc:"Run in a git worktree of a repository inside the space folder"`
	// Skills are filled by resolution from the space role (not part of the API).
	Skills []proto.Skill `json:"-"`
	// ClaudeSettings are the role profile's Claude Code settings, filled by resolution (not part of the API, ADR-0059).
	ClaudeSettings proto.ClaudeSettings `json:"-"`
	// HarnessVersion is the role profile's harness version pin, filled by resolution (not part of the API, ADR-0077).
	HarnessVersion string `json:"-"`
	// Dream prepares a dream workspace (set by the dream scheduler, not part of the API).
	Dream      *proto.DreamSpec `json:"-"`
	AccountRef string           `json:"account_ref,omitempty"`
	// RecordingMaxBytes is the space's recording quota, filled by launch resolution (not part of the API).
	RecordingMaxBytes int64 `json:"-"`
	// TerminalSize is the size the run's terminal is held at from the start: the space's terminal_cols and
	// terminal_rows limits, filled by launch resolution (not part of the API, ADR-0075). nil: the node's default.
	TerminalSize *TerminalSize `json:"-"`
	// AuthorizeKind checks the caller may launch the resolved kind (the API: diagnostics need run:diagnose,
	// SM-CTRL-004). It runs after the space role decided the kind and before anything is written.
	AuthorizeKind func(kind string) error `json:"-"`
}

// LaunchRun creates a run pinned to the space's node and current lease generation and queues the launch command.
// New work is refused while the space is paused, archived or relocating, or when the node holds no valid lease
// (it is disconnected): disconnected nodes start no new work (SM-NODE-007), and placement never changes (SM-INV-003).
func (svc *Service) LaunchRun(ctx context.Context, actor audit.Actor, spaceID string, in LaunchInput) (*Run, error) {
	var resolved map[string]any
	if svc.opts.ResolveLaunch != nil {
		var err error
		if resolved, err = svc.opts.ResolveLaunch(ctx, spaceID, &in); err != nil {
			return nil, err
		}
	}
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
	if in.AuthorizeKind != nil {
		if err := in.AuthorizeKind(in.Kind); err != nil {
			return nil, err
		}
	}
	// The gateway's network checks (connection, budget) run before the transaction (ADR-0060 decision 4).
	if svc.opts.Gateway != nil {
		if err := svc.opts.Gateway.PreLaunch(ctx, spaceID, in.Harness); err != nil {
			return nil, err
		}
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
		if svc.opts.Admit != nil {
			if err := svc.opts.Admit(ctx, tx, Admission{SpaceID: spaceID, NodeID: sp.NodeID, Kind: in.Kind, Role: in.Role, Actor: actor}); err != nil {
				return err
			}
		}
		if in.SessionID != "" {
			// Native sessions are node-local (SM-SPACE-008): a session that only ever ran on another node (before a
			// relocation) cannot be resumed here, so the run starts a fresh session with the project's continuity.
			c, err := tx.Q.CountRunsWithSession(ctx, db.CountRunsWithSessionParams{NodeID: sp.NodeID, SpaceID: spaceID, SessionID: in.SessionID})
			if err != nil {
				return err
			}
			if c.Total > 0 && c.OnNode == 0 {
				if resolved == nil {
					resolved = map[string]any{}
				}
				resolved["session"] = map[string]any{"requested": in.SessionID, "resumed": false,
					"reason": "the native session is on another node (the space was relocated); fresh session, project continuity only"}
				in.SessionID = ""
			}
		}
		ul := UsageLimitSettings{KeepAliveMax: DefaultLimitKeepAliveMax}
		if svc.opts.UsageLimitSettings != nil {
			ul = svc.opts.UsageLimitSettings(ctx, tx.Q, spaceID)
		}
		// A harness the space routes through an LLM gateway uses no account, never as a fallback either (ADR-0060 Q1).
		var gw *GatewaySpec
		if svc.opts.Gateway != nil {
			if gw, err = svc.opts.Gateway.Route(ctx, tx, spaceID, in.Harness, in.AccountRef, in.Model); err != nil {
				return err
			}
		}
		// The harness session the launch starts. A fresh Claude Code gateway run gets its session ID from central, chosen
		// after the relocation check above, so the run's token can be bound to it (ADR-0060 decision 5); the run row
		// learns it from the node's report, as for any run, so a run that never started has none.
		session, resume := in.SessionID, in.SessionID != ""
		if gw != nil {
			if resolved == nil {
				resolved = map[string]any{}
			}
			resolved["gateway"] = map[string]any{"kind": gw.Kind, "project_id": gw.ProjectID}
			if session == "" && in.Harness == harness.KindClaude {
				session = harness.NewUUID()
			}
		} else if svc.opts.ResolveAccount != nil {
			// With failover on (a space setting, off by default: ADR-0079 decision 8, Q1), an account with an open hold
			// gives way to another bound account of the harness without one. A gateway run never gets here: it never
			// falls back to a direct account.
			var avoid func(string) (bool, error)
			if ul.Failover {
				avoid = func(id string) (bool, error) { return accountHeld(ctx, tx.Q, id, tx.NowMicros()) }
			}
			ref := in.AccountRef
			if in.AccountRef, err = svc.opts.ResolveAccount(ctx, tx, spaceID, in.Harness, ref, avoid); err != nil {
				return err
			}
			if avoid != nil {
				if base, err := svc.opts.ResolveAccount(ctx, tx, spaceID, in.Harness, ref, nil); err == nil && base != in.AccountRef {
					if resolved == nil {
						resolved = map[string]any{}
					}
					resolved["account_failover"] = map[string]any{"from": base, "to": in.AccountRef, "reason": "usage_limit"}
				}
			}
		}
		// A held account (or gateway reference) refuses the launch, writing nothing, unless this launch is the hold's
		// probe (ADR-0079 decision 7).
		probe, err := svc.holdCheckTx(ctx, tx, holdTarget(in.AccountRef, gw), ul.KeepAliveMax)
		if err != nil {
			return err
		}
		if probe {
			if resolved == nil {
				resolved = map[string]any{}
			}
			resolved["usage_limit_probe"] = true
		}
		// A worktree of a configured repository (named by its folder or its name) carries the repository, so the node can
		// clone it at its first use (ADR-0044). Folders that are not configured still work as they are.
		var source *proto.RepoSource
		if in.Worktree != nil {
			r, err := tx.Q.GetSpaceRepositoryByPath(ctx, db.GetSpaceRepositoryByPathParams{SpaceID: spaceID, Path: in.Worktree.Repo})
			if errors.Is(err, sql.ErrNoRows) {
				r, err = tx.Q.GetSpaceRepositoryByName(ctx, db.GetSpaceRepositoryByNameParams{SpaceID: spaceID, Name: in.Worktree.Repo})
			}
			switch {
			case err == nil:
				wt := *in.Worktree
				wt.Repo = r.Path
				in.Worktree = &wt
				source = &proto.RepoSource{ID: r.ID, Name: r.Name, Path: r.Path, URL: r.Url, DefaultBranch: r.DefaultBranch, ClonePolicy: r.ClonePolicy,
					KnownHosts: r.KnownHosts}
				if resolved == nil {
					resolved = map[string]any{}
				}
				resolved["repository"] = map[string]any{"id": r.ID, "name": r.Name, "path": r.Path, "url": r.Url, "default_branch": r.DefaultBranch}
			case !errors.Is(err, sql.ErrNoRows):
				return err
			}
		}
		spec := LaunchSpec{Prompt: in.Prompt, SystemText: in.SystemText, Model: in.Model, Settings: in.Settings, Workspace: in.Workspace, Worktree: in.Worktree,
			AccountRef: in.AccountRef, Gateway: gw}
		specJSON, _ := json.Marshal(spec)
		m := map[string]any{"harness": in.Harness, "role": in.Role, "kind": in.Kind, "lease_generation": lease.Generation,
			"account_ref": in.AccountRef, "model": in.Model, "settings": in.Settings, "dependencies": dependencyVersions(ctx, tx, sp.NodeID, in.Harness, in.HarnessVersion)}
		for k, v := range resolved {
			m[k] = v
		}
		manifest, _ := json.Marshal(m)
		var termCols, termRows sql.NullInt64
		// A node known to predate minor 10 cannot hold a size: storing one would show a size the run doesn't have.
		if minor, ok := svc.hub.minor(sp.NodeID); ok && minor < proto.MinorTerminalSize {
			in.TerminalSize = nil
		}
		if ts := in.TerminalSize; ts != nil && proto.CheckTerminalSize(ts.Cols, ts.Rows) == nil {
			termCols, termRows = sql.NullInt64{Int64: int64(ts.Cols), Valid: true}, sql.NullInt64{Int64: int64(ts.Rows), Valid: true}
			tx.AfterCommit(func() { svc.hub.pushTerminalSizes(context.WithoutCancel(ctx), sp.NodeID) })
		}
		if err := tx.Q.InsertRun(ctx, db.InsertRunParams{ID: runID, SpaceID: spaceID, NodeID: sp.NodeID, Generation: lease.Generation, Kind: in.Kind,
			Role: in.Role, Harness: in.Harness, InstanceID: in.InstanceID, SessionID: in.SessionID, LaunchJson: string(specJSON), ManifestJson: string(manifest),
			CreatedAt: tx.NowMicros(), CreatedBy: actorRef(actor), UpdatedAt: tx.NowMicros(), TerminalCols: termCols, TerminalRows: termRows}); err != nil {
			if store.IsConstraint(err) && in.Kind == "control" {
				return apperr.New(apperr.CodeConflict, "the space already has an authoritative control run")
			}
			return err
		}
		payload := proto.LaunchPayload{Harness: in.Harness, Role: in.Role, Kind: in.Kind, InstanceID: in.InstanceID, SessionID: session,
			Resume: resume, Prompt: in.Prompt, SystemText: in.SystemText, Model: in.Model, Settings: in.Settings, Workspace: in.Workspace,
			Worktree: in.Worktree, Repository: source, Skills: in.Skills, ClaudeSettings: in.ClaudeSettings, HarnessVersion: in.HarnessVersion, AccountRef: in.AccountRef, Dream: in.Dream,
			RecordingMaxBytes: in.RecordingMaxBytes}
		if err := svc.queueCommand(ctx, tx, sp.NodeID, spaceID, runID, lease.Generation, proto.CmdLaunch, "launch:"+runID, payload); err != nil {
			return err
		}
		if err := svc.record(ctx, tx, spaceID, "run", runID, "created", 1); err != nil {
			return err
		}
		if _, err := svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "run.launched", Target: audit.Target{Type: "run", ID: runID},
			Details: map[string]any{"space_id": spaceID, "kind": in.Kind, "role": in.Role, "harness": in.Harness, "generation": lease.Generation}}); err != nil {
			return err
		}
		_, err = svc.opts.Outbox.Enqueue(ctx, tx, outbox.Event{Payload: outbox.RunLaunched{Run: outbox.Run{SpaceID: spaceID, RunID: runID, NodeID: sp.NodeID,
			Kind: in.Kind, Role: in.Role, Harness: in.Harness, InstanceID: in.InstanceID, Generation: lease.Generation}}, Correlation: runID})
		return err
	})
	if err != nil {
		return nil, err
	}
	svc.hub.Kick(nodeID)
	if svc.opts.OnRunLaunched != nil {
		svc.opts.OnRunLaunched(runID)
	}
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

// ErrLimitWaiting refuses idle-gated input to a run waiting for its usage limit to reset (ADR-0079 decision 3): typing
// into it would start a turn that fails at once. The input waits for the clear; an operator's own input still goes.
var ErrLimitWaiting = apperr.New(apperr.CodePrecondition, "the run is waiting for its usage limit to reset; notices wait until it clears")

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
	if r.LimitKind != "" {
		return "", ErrLimitWaiting
	}
	// Notices are automated input: none while an operator has taken over the terminal (SM-UI-004).
	if err := refuseIfTakenOver(ctx, tx, r.ID); err != nil {
		return "", err
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

// LatestControlRun returns the space's newest control run, in any state (control auto-start resumes it, ADR-0056).
func (svc *Service) LatestControlRun(ctx context.Context, spaceID string) (*Run, error) {
	r, err := svc.s.R().GetLatestControlRun(ctx, spaceID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errRunNotFound
	} else if err != nil {
		return nil, err
	}
	return runFromRow(r), nil
}

// IdleControlRuns lists the running control runs whose program reports idle or done (control idle stop, ADR-0056).
func (svc *Service) IdleControlRuns(ctx context.Context) ([]string, error) {
	return svc.s.R().ListIdleControlRuns(ctx)
}

// KickRun wakes the dispatch loop of the run's node.
func (svc *Service) KickRun(ctx context.Context, runID string) {
	if r, err := svc.s.R().GetRun(ctx, runID); err == nil {
		svc.hub.Kick(r.NodeID)
	}
}

// dependencyVersions records what the run depends on, as the node last reported it (SM-PROMPT-004). For Claude Code on a
// node at minor 11 on that manages it (ADR-0077), the harness entry is {requested, node_default}: the profile's pin
// (else the node's channel) and the version the node last reported a new run would get. What the run actually launched
// is on the run row. Every other harness entry (older nodes, unmanaged Claude Code, other harnesses) is the plain
// version string.
func dependencyVersions(ctx context.Context, tx *store.Tx, nodeID, kind, pin string) map[string]any {
	out := map[string]any{"central": buildinfo.Get().Version, "node_protocol": proto.Major}
	n, err := tx.Q.GetNode(ctx, nodeID)
	if err != nil {
		return out
	}
	out["spaceman_agent"] = n.AgentVersion
	var caps proto.Capabilities
	if json.Unmarshal([]byte(n.CapabilitiesJson), &caps) == nil {
		out["runner"] = caps.Runner.Kind + " " + caps.Runner.Version
		for _, h := range caps.Harnesses {
			if h.Kind != kind {
				continue
			}
			if n.ProtocolMinor < proto.MinorHarnessVersion || kind != harness.KindClaude || !h.Managed {
				out[kind] = h.Version
				continue
			}
			requested := pin
			if requested == "" {
				requested = h.Channel
			}
			out[kind] = map[string]any{"requested": requested, "node_default": h.Version}
		}
	}
	return out
}

// failUndispatched ends a run whose launch could not be sent (its account became unusable after the launch was
// accepted): the launch command is cancelled and the run is failed with the reason, so it does not wait forever. Like a
// report that ends a run, it counts the end, publishes run_ended, calls OnRunEnded and advances the space's relocation,
// once: a run that has already ended only has its commands cancelled.
func (svc *Service) failUndispatched(ctx context.Context, runID, endReason, reason string) error {
	var justEnded bool
	var spaceID string
	err := svc.s.W().TxNamed(ctx, "fleet.run_undispatched", func(tx *store.Tx) error {
		r, err := tx.Q.GetRun(ctx, runID)
		if err != nil {
			return err
		}
		if _, err := tx.Q.CancelRunCommands(ctx, db.CancelRunCommandsParams{Reason: reason, RunID: sql.NullString{String: runID, Valid: true}}); err != nil {
			return err
		}
		spaceID, justEnded = r.SpaceID, !ended(r.Observed)
		if !justEnded {
			return nil
		}
		svc.opts.Metrics.RunEnded(metrics.Harness(r.Harness), metrics.RunLaunchFailed)
		// SessionID "" keeps the stored one (a nil would write NULL and fail the NOT NULL constraint).
		if _, err := tx.Q.UpdateRunObserved(ctx, db.UpdateRunObservedParams{Observed: proto.RunFailed, EndReason: endReason, Error: reason, SessionID: "",
			MarkStarted: 0, Now: sql.NullInt64{Int64: tx.NowMicros(), Valid: true}, MarkEnded: 1, ID: r.ID, Generation: r.Generation}); err != nil {
			return err
		}
		if _, err := tx.Q.RevokeRunCredentials(ctx, db.RevokeRunCredentialsParams{RevokedAt: sql.NullInt64{Int64: tx.NowMicros(), Valid: true}, RunID: r.ID}); err != nil {
			return err
		}
		if _, err := svc.queueGatewayRevocation(ctx, tx, r.ID, RevokeLaunchFail); err != nil {
			return err
		}
		if err := svc.record(ctx, tx, r.SpaceID, "run", r.ID, "updated", r.Version+1); err != nil {
			return err
		}
		if err := svc.runEvent(ctx, tx, r, outbox.RunEnded{Run: outboxRun(r), State: proto.RunFailed, EndReason: endReason}); err != nil {
			return err
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: audit.System, Action: "run.launch_failed", Target: audit.Target{Type: "run", ID: r.ID},
			Details: map[string]any{"space_id": r.SpaceID, "reason": reason}})
		return err
	})
	if err == nil && justEnded && svc.opts.OnRunEnded != nil {
		svc.opts.OnRunEnded(runID)
	}
	if err == nil && justEnded {
		svc.kickSpaceRelocation(ctx, spaceID)
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
	if _, err := svc.queueGatewayRevocation(ctx, tx, r.ID, RevokeRunStopped); err != nil {
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
		// While the terminal is taken over, only its holder types, through the takeover (SM-UI-004).
		if err := refuseIfTakenOver(ctx, tx, r.ID); err != nil {
			return err
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
	out := runFromRow(r)
	if out.Takeover, err = svc.ActiveTakeover(ctx, id); err != nil {
		return nil, err
	}
	out.TerminalSizing = svc.TerminalSizing(r.NodeID)
	return out, nil
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
	return svc.applyRunReport(ctx, nodeID, rep, nil)
}

// applyRunReport is applyReport; with sweep set, the report is central's own reconciliation of a stale run (ADR-0074):
// it applies only while the sweep's signal still holds (stillStale), it also cancels the run's unacknowledged commands
// and sets its desired state to stopped, and the run's end is audited and counted as a sweep. A sweep that no longer
// applies is skipped, not fenced.
func (svc *Service) applyRunReport(ctx context.Context, nodeID string, rep proto.RunReport, sweep *staleRun) (fenced bool, err error) {
	if svc.onReport != nil {
		svc.onReport(rep)
	}
	becameIdle, justEnded, skipped := false, false, false
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
		if sweep != nil {
			if ok, err := svc.stillStale(ctx, tx, r, sweep); err != nil || !ok {
				skipped = true // a report, a reconnect or another sweep got there first
				return err
			}
		}
		if r.NodeID != nodeID || r.SpaceID != rep.SpaceID || r.Generation != rep.Generation {
			fenced = true
			return nil
		}
		// After a relocation the space belongs to another node: whatever the old node still reports for it is fenced,
		// and a run it still has is stopped (SM-NODE-006, ADR-0037).
		if sp, err := tx.Q.GetSpace(ctx, r.SpaceID); err == nil && sp.NodeID != nodeID {
			fenced = true
			return nil
		}
		// An ended run stays ended. A report that it is still pending, starting, running or stopping arrived late (the
		// node sends reports concurrently, and both it and central treat exited, failed and lost as final), so it must not
		// reopen the run: that would end it a second time and publish a second run_ended event.
		if ended(r.Observed) && !ended(rep.State) {
			fenced = true
			return nil
		}
		// A run the stale-run sweeper ended stays exactly as it ended it (ADR-0074): a node that reconnects later and
		// reports a final state from its journal would otherwise end the run a second time or replace the sweep's reason.
		if (r.EndReason == EndStaleNotOnNode || r.EndReason == EndStaleLeaseLost) && ended(r.Observed) {
			fenced = true
			return nil
		}
		// The run's usage limit (minor ≥ 12, ADR-0079): a report without one keeps what is stored. A run that ends while
		// at a limit, without central having asked it to stop, ended because of it.
		curLimit := storedLimit(r)
		nextLimit, hasLimit := reportedLimit(curLimit, rep, tx.Now)
		limitChanged := hasLimit && nextLimit != curLimit
		if !hasLimit {
			nextLimit = curLimit
		}
		// An end recorded as usage_limit stays so when a later report clears the limit.
		nodeReason := rep.Reason
		// The override applies to the report that ends the run; an end already recorded keeps its reason (only an end
		// recorded as usage_limit is restored when a later report repeats the node's own reason).
		if sweep == nil && ended(rep.State) && (!ended(r.Observed) && nextLimit.kind != "" && r.Desired != "stopped" || ended(r.Observed) && r.EndReason == EndUsageLimit) {
			rep.Reason = EndUsageLimit
		}
		// A report that changes nothing (every hello re-reports the node's whole journal, ended runs included) writes
		// nothing: no version, no change-feed entry, so a reconnect doesn't make every open UI refetch its runs.
		if sweep == nil && !limitChanged && reportUnchanged(r, rep) {
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
		version := r.Version + 1
		// The harness version the run launched (minor ≥ 11, ADR-0077): a report without one keeps what was recorded.
		// What was requested and why another version ran are stored only when they are a valid request and a known
		// fallback reason; anything else is dropped (review S2).
		if proto.IsHarnessVersion(rep.HarnessVersion) {
			requested, fallback := reportHarness(rep)
			n, err := tx.Q.SetRunHarness(ctx, db.SetRunHarnessParams{HarnessVersion: rep.HarnessVersion,
				HarnessRequested: requested, HarnessFallback: fallback, Now: tx.NowMicros(), ID: r.ID, Generation: r.Generation})
			if err != nil {
				return err
			}
			version += n
		}
		if limitChanged {
			n, err := svc.applyLimitTx(ctx, tx, r, curLimit, nextLimit)
			if err != nil {
				return err
			}
			version += n
			if err := svc.limitWindowTx(ctx, tx, r.ID, curLimit, nextLimit); err != nil {
				return err
			}
		}
		if sweep != nil {
			// Nothing for the run may reach the node any more, and the node is no longer told to keep it (sendReconcile).
			if _, err := tx.Q.CancelAllRunCommands(ctx, db.CancelAllRunCommandsParams{Reason: "run ended by the stale-run sweeper", RunID: sql.NullString{String: r.ID, Valid: true}}); err != nil {
				return err
			}
			if r.Desired != "stopped" {
				if _, err := tx.Q.SetRunDesired(ctx, db.SetRunDesiredParams{Desired: "stopped", UpdatedAt: tx.NowMicros(), ID: r.ID}); err != nil {
					return err
				}
				version++
			}
		}
		if r.Observed != rep.State {
			svc.opts.Metrics.RunStarted(metrics.Harness(r.Harness), startResult(rep))
			if ended(rep.State) {
				svc.opts.Metrics.RunEnded(metrics.Harness(r.Harness), metrics.RunEndReason(endReason(rep)))
				if _, err := tx.Q.RevokeRunCredentials(ctx, db.RevokeRunCredentialsParams{RevokedAt: sql.NullInt64{Int64: tx.NowMicros(), Valid: true}, RunID: r.ID}); err != nil {
					return err
				}
				if _, err := svc.queueGatewayRevocation(ctx, tx, r.ID, RevokeRunEnded); err != nil {
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
		// A limit that clears while the program is idle counts as turning idle: the notices held for it go out and
		// control's idle stop is armed again (ADR-0079 decision 3).
		limitCleared := limitChanged && curLimit.kind != "" && nextLimit.kind == ""
		becameIdle = !ended(rep.State) && (rep.Status == "idle" || rep.Status == "done") && (r.Status != rep.Status || r.Observed != rep.State || limitCleared)
		justEnded = ended(rep.State) && !ended(r.Observed)
		if !r.StartedAt.Valid && (rep.State == proto.RunRunning || (justEnded && rep.State == proto.RunExited)) {
			if err := svc.runEvent(ctx, tx, r, outbox.RunStarted{Run: outboxRun(r), Adopted: rep.Adopted}); err != nil {
				return err
			}
		}
		if justEnded {
			if err := svc.endRunTakeoverTx(ctx, tx, r); err != nil {
				return err
			}
			ev := outbox.RunEnded{Run: outboxRun(r), State: rep.State, EndReason: outbox.CleanID(rep.Reason)}
			if nodeReason != rep.Reason {
				ev.NodeReason = outbox.CleanID(nodeReason)
			}
			if rep.ExitCode != nil {
				c := int64(*rep.ExitCode)
				ev.ExitCode = &c
			}
			if err := svc.runEvent(ctx, tx, r, ev); err != nil {
				return err
			}
			if sweep != nil {
				if err := svc.auditStaleRun(ctx, tx, r, sweep); err != nil {
					return err
				}
			}
		}
		return svc.record(ctx, tx, r.SpaceID, "run", r.ID, "updated", version)
	})
	if err == nil && becameIdle && svc.opts.OnRunIdle != nil {
		svc.opts.OnRunIdle(rep.RunID)
	}
	if err == nil && justEnded && svc.opts.OnRunEnded != nil {
		svc.opts.OnRunEnded(rep.RunID)
	}
	if err == nil && justEnded {
		svc.kickSpaceRelocation(ctx, rep.SpaceID)
	}
	if err == nil && justEnded && sweep != nil {
		svc.opts.Metrics.RunReconciledStale(metrics.StaleReason(sweep.Reason))
	}
	if fenced && sweep == nil {
		svc.opts.Metrics.LeaseFenced()
	}
	if skipped || (fenced && sweep != nil) {
		return true, err
	}
	return fenced, err
}

// reportHarness is what a report's harness request and fallback are stored as: a valid request and a known fallback
// reason, anything else dropped (review S2).
func reportHarness(rep proto.RunReport) (requested, fallback string) {
	requested, fallback = rep.HarnessRequested, rep.HarnessFallback
	if proto.CheckHarnessVersion(requested) != nil {
		requested = ""
	}
	if !dist.IsFallback(fallback) {
		fallback = ""
	}
	return requested, fallback
}

// reportUnchanged reports whether applying rep to r would change nothing applyRunReport stores (it would only move
// observed_at): the same observed state and fields, no start or end to mark, the same harness version, and no stop to
// queue again.
func reportUnchanged(r db.Run, rep proto.RunReport) bool {
	exitSame := (rep.ExitCode == nil && !r.ExitCode.Valid) || (rep.ExitCode != nil && r.ExitCode.Valid && int64(*rep.ExitCode) == r.ExitCode.Int64)
	markStarted := rep.State == proto.RunRunning || rep.State == proto.RunExited || rep.State == proto.RunFailed
	if r.Observed != rep.State || r.TerminalID != rep.TerminalID || r.CoreID != rep.CoreID || r.Identity != rep.Identity ||
		r.Pid != int64(rep.PID) || !exitSame || r.EndReason != rep.Reason || r.Error != truncate(rep.Error, 2000) || r.Status != rep.Status ||
		r.StatusKind != rep.StatusKind || r.Turn != int64(min(rep.Turn, math.MaxInt64)) || //nolint:gosec // saturated above
		r.Adopted != b2i(rep.Adopted) || (rep.SessionID != "" && rep.SessionID != r.SessionID) ||
		(markStarted && !r.StartedAt.Valid) || ended(rep.State) != r.EndedAt.Valid {
		return false
	}
	if proto.IsHarnessVersion(rep.HarnessVersion) {
		requested, fallback := reportHarness(rep)
		if r.HarnessVersion != rep.HarnessVersion || r.HarnessRequested != requested || r.HarnessFallback != fallback {
			return false
		}
	}
	return r.Desired != "stopped" || ended(rep.State)
}

func outboxRun(r db.Run) outbox.Run {
	return outbox.Run{SpaceID: r.SpaceID, RunID: r.ID, NodeID: r.NodeID, Kind: r.Kind, Role: r.Role, Harness: r.Harness, InstanceID: r.InstanceID,
		Generation: r.Generation}
}

// runEvent records a run lifecycle event in the run's transaction; the run is the correlation.
func (svc *Service) runEvent(ctx context.Context, tx *store.Tx, r db.Run, p outbox.Payload) error {
	_, err := svc.opts.Outbox.Enqueue(ctx, tx, outbox.Event{Payload: p, Correlation: r.ID})
	return err
}

func ended(s string) bool { return s == proto.RunExited || s == proto.RunFailed || s == proto.RunLost }

// RunUnknown is the observed state central records for the runs of a node that disconnected (SM-NODE-005); nodes
// never report it.
const RunUnknown = "unknown"

// Live reports whether a run in this observed state is live: not yet exited, failed or lost. It is the one definition
// of an active run that spaceman_runs_list shows and the space and node concurrency limits count (the SQL of
// ListLiveWorkRunsInSpace and ListLiveWorkRunsOnNode spells out the same states). desired is not part of it: a run
// that ended on its own keeps desired = "running".
func Live(observed string) bool {
	switch observed {
	case proto.RunPending, proto.RunStarting, proto.RunRunning, proto.RunStopping, RunUnknown:
		return true
	}
	return false
}

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
	case "exited", "killed", "lease_expired", "stopped", EndUsageLimit:
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

// QueueSpaceCommand queues a space-level command (no run) for the space's node at its current lease generation,
// inside the caller's transaction, and returns the command ID; call KickSpace after the commit. It fails while the
// node holds no valid lease: work in a space only happens on its current node.
func (svc *Service) QueueSpaceCommand(ctx context.Context, tx *store.Tx, spaceID, kind, key string, payload any) (string, error) {
	return svc.QueueSpaceCommandForRun(ctx, tx, spaceID, "", kind, key, payload)
}

// QueueSpaceCommandForRun is QueueSpaceCommand for a command about one run (dream.commit).
func (svc *Service) QueueSpaceCommandForRun(ctx context.Context, tx *store.Tx, spaceID, runID, kind, key string, payload any) (string, error) {
	sp, err := tx.Q.GetSpace(ctx, spaceID)
	if err != nil {
		return "", errSpaceNotFound
	}
	lease, err := tx.Q.GetLease(ctx, spaceID)
	if err != nil || lease.State != "active" || !tx.Now.Before(time.UnixMicro(lease.ExpiresAt)) || lease.NodeID != sp.NodeID {
		return "", apperr.New(apperr.CodeUnavailable, "the space's node is not connected (no valid execution lease)")
	}
	return svc.queueCommandID(ctx, tx, sp.NodeID, spaceID, runID, lease.Generation, kind, key, payload)
}

// KickSpace wakes the delivery of commands to the space's node.
func (svc *Service) KickSpace(ctx context.Context, spaceID string) {
	if sp, err := svc.s.R().GetSpace(ctx, spaceID); err == nil {
		svc.hub.Kick(sp.NodeID)
	}
}
